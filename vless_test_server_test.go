package connect

// An in-process VLESS server for the tests: raw tcp or tls security, the raw
// tcp, ws and httpupgrade transports, and the vision flow, which ends its
// downlink padding with either the end or the direct command. Each stream is
// relayed to the destination its request names. The server records what it
// saw so tests can assert on the wire behavior of the client.

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const testVlessUserId = "5783a3e7-e373-51cd-8642-c83782b807c5"

type testVlessServer struct {
	t            *testing.T
	listener     net.Listener
	userId       [16]byte
	security     string
	network      string
	path         string
	tlsConfig    *tls.Config
	visionDirect bool
	// pads no downlink block even for the vision flow, as a server without
	// vision would
	visionNoPadding bool

	stateLock      sync.Mutex
	destinations   []string
	flows          []string
	uplinkCommands []byte
	requestPaths   []string
	requestHosts   []string

	waitGroup sync.WaitGroup
}

func newTestVlessServer(t *testing.T, security string, network string) *testVlessServer {
	t.Helper()
	userId, err := vlessId(testVlessUserId)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &testVlessServer{
		t:        t,
		listener: listener,
		userId:   userId,
		security: security,
		network:  network,
		path:     "/vless-test",
	}
	if security == VlessSecurityTls {
		server.tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{newTestVlessCertificate(t, "vless.example")},
			NextProtos:   []string{"http/1.1"},
		}
	}
	server.waitGroup.Add(1)
	go func() {
		defer server.waitGroup.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			server.waitGroup.Add(1)
			go func() {
				defer server.waitGroup.Done()
				server.serve(conn)
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		server.waitGroup.Wait()
	})
	return server
}

func newTestVlessCertificate(t *testing.T, serverName string) tls.Certificate {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: serverName},
		DNSNames:     []string{serverName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: privateKey}
}

// The configuration a client uses to reach this server.
func (self *testVlessServer) config(flow string) *VlessConfig {
	port := self.listener.Addr().(*net.TCPAddr).Port
	config := &VlessConfig{
		Address:  "127.0.0.1",
		Port:     port,
		Id:       testVlessUserId,
		Flow:     flow,
		Network:  self.network,
		Security: self.security,
	}
	if self.security == VlessSecurityTls {
		config.ServerName = "vless.example"
		config.AllowInsecure = true
	}
	if self.network == VlessNetworkWs || self.network == VlessNetworkHttpUpgrade {
		config.Path = self.path
		config.Host = "cdn.example"
	}
	return config
}

func (self *testVlessServer) seen() (destinations []string, flows []string, uplinkCommands []byte) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string{}, self.destinations...), append([]string{}, self.flows...), append([]byte{}, self.uplinkCommands...)
}

func (self *testVlessServer) serve(socketConn net.Conn) {
	defer socketConn.Close()

	// every write goes through one coalescing conn, so the direct switch can
	// put the record that carries the command and the raw bytes after it in
	// one tcp write
	rawConn := &testCoalescingConn{Conn: socketConn}
	var conn net.Conn = rawConn
	if self.security == VlessSecurityTls {
		tlsConn := tls.Server(rawConn, self.tlsConfig)
		if err := tlsConn.Handshake(); err != nil {
			return
		}
		conn = tlsConn
	}

	var stream net.Conn
	switch self.network {
	case VlessNetworkTcp:
		stream = conn
	case VlessNetworkWs:
		reader := bufio.NewReader(conn)
		request, err := http.ReadRequest(reader)
		if err != nil {
			return
		}
		self.recordRequest(request)
		writer := &testHijackResponseWriter{
			conn:   conn,
			brw:    bufio.NewReadWriter(reader, bufio.NewWriter(conn)),
			header: http.Header{},
		}
		upgrader := websocket.Upgrader{}
		wsConn, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		stream = newVlessWebSocketConn(wsConn)
	case VlessNetworkHttpUpgrade:
		reader := bufio.NewReader(conn)
		request, err := http.ReadRequest(reader)
		if err != nil {
			return
		}
		self.recordRequest(request)
		if request.Header.Get("Upgrade") != "websocket" || request.Header.Get("Connection") != "Upgrade" {
			conn.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
			return
		}
		if _, err := conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")); err != nil {
			return
		}
		stream = &vlessBufferedConn{Conn: conn, reader: reader}
	default:
		return
	}

	reader := bufio.NewReader(stream)
	head := make([]byte, 1+16+1)
	if _, err := io.ReadFull(reader, head); err != nil {
		return
	}
	if head[0] != vlessVersion || [16]byte(head[1:17]) != self.userId {
		return
	}
	addons := make([]byte, int(head[17]))
	if _, err := io.ReadFull(reader, addons); err != nil {
		return
	}
	flow := ""
	if 2 <= len(addons) && addons[0] == 0x0a && int(addons[1]) == len(addons)-2 {
		flow = string(addons[2:])
	}
	commandPort := make([]byte, 4)
	if _, err := io.ReadFull(reader, commandPort); err != nil {
		return
	}
	if commandPort[0] != vlessCommandTcp {
		return
	}
	port := int(binary.BigEndian.Uint16(commandPort[1:3]))
	var host string
	switch commandPort[3] {
	case vlessAddressTypeIpv4:
		ip := make([]byte, 4)
		if _, err := io.ReadFull(reader, ip); err != nil {
			return
		}
		host = net.IP(ip).String()
	case vlessAddressTypeIpv6:
		ip := make([]byte, 16)
		if _, err := io.ReadFull(reader, ip); err != nil {
			return
		}
		host = net.IP(ip).String()
	case vlessAddressTypeDomain:
		length, err := reader.ReadByte()
		if err != nil {
			return
		}
		name := make([]byte, int(length))
		if _, err := io.ReadFull(reader, name); err != nil {
			return
		}
		host = string(name)
	default:
		return
	}
	destination := net.JoinHostPort(host, strconv.Itoa(port))
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.destinations = append(self.destinations, destination)
		self.flows = append(self.flows, flow)
	}()

	destinationConn, err := net.Dial("tcp", destination)
	if err != nil {
		return
	}
	defer destinationConn.Close()

	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		if flow == VlessFlowVision {
			self.visionUplink(reader, destinationConn)
		} else {
			io.Copy(destinationConn, reader)
		}
		if tcpConn, ok := destinationConn.(*net.TCPConn); ok {
			tcpConn.CloseWrite()
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		if flow == VlessFlowVision && !self.visionNoPadding {
			self.visionDownlink(destinationConn, stream, rawConn)
		} else {
			if _, err := stream.Write([]byte{vlessVersion, 0}); err != nil {
				return
			}
			io.Copy(stream, destinationConn)
		}
	}()
	<-done
	<-done
}

func (self *testVlessServer) recordRequest(request *http.Request) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.requestPaths = append(self.requestPaths, request.URL.RequestURI())
	self.requestHosts = append(self.requestHosts, request.Host)
}

// Reads the client's padded blocks, forwarding their content, then the plain
// rest.
func (self *testVlessServer) visionUplink(reader *bufio.Reader, destinationConn net.Conn) {
	userId := make([]byte, 16)
	if _, err := io.ReadFull(reader, userId); err != nil || [16]byte(userId) != self.userId {
		self.t.Errorf("vision uplink does not start with the user id")
		return
	}
	for {
		head := make([]byte, 5)
		if _, err := io.ReadFull(reader, head); err != nil {
			return
		}
		command := head[0]
		contentLength := int(head[1])<<8 | int(head[2])
		paddingLength := int(head[3])<<8 | int(head[4])
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.uplinkCommands = append(self.uplinkCommands, command)
		}()
		content := make([]byte, contentLength)
		if _, err := io.ReadFull(reader, content); err != nil {
			return
		}
		if _, err := destinationConn.Write(content); err != nil {
			return
		}
		if _, err := io.CopyN(io.Discard, reader, int64(paddingLength)); err != nil {
			return
		}
		if command == vlessVisionCommandEnd {
			break
		}
		if command != vlessVisionCommandContinue {
			self.t.Errorf("vision uplink command %d", command)
			return
		}
	}
	io.Copy(destinationConn, reader)
}

// Pads the destination's first reads, then ends with end or, when the server
// is set to, with direct at the first application data record, after which
// the destination's bytes go to the raw socket outside the outer tls. The
// direct block carries no content: the chunk follows it raw in the same tcp
// write, which is the read-ahead case a client must not lose bytes to.
func (self *testVlessServer) visionDownlink(destinationConn net.Conn, stream net.Conn, rawConn *testCoalescingConn) {
	buffer := make([]byte, vlessVisionMaxBlockContent)
	first := true
	padding := true
	downlink := stream
	count := 0
	for {
		n, err := destinationConn.Read(buffer)
		if 0 < n {
			chunk := buffer[:n]
			if padding {
				count += 1
				command := vlessVisionCommandContinue
				applicationData := isVlessVisionTlsApplicationData(chunk)
				if applicationData || 8 <= count {
					command = vlessVisionCommandEnd
					if self.visionDirect && applicationData {
						command = vlessVisionCommandDirect
					}
				}
				packet := []byte{}
				if first {
					first = false
					packet = append(packet, vlessVersion, 0)
					packet = append(packet, self.userId[:]...)
				}
				content := chunk
				if command == vlessVisionCommandDirect {
					content = nil
				}
				paddingLength := 64
				packet = append(packet, command, byte(len(content)>>8), byte(len(content)), byte(paddingLength>>8), byte(paddingLength))
				packet = append(packet, content...)
				packet = append(packet, make([]byte, paddingLength)...)
				switch command {
				case vlessVisionCommandDirect:
					padding = false
					downlink = rawConn
					rawConn.hold()
					if _, err := stream.Write(packet); err != nil {
						return
					}
					rawConn.Write(chunk)
					if err := rawConn.flush(); err != nil {
						return
					}
				default:
					if _, err := stream.Write(packet); err != nil {
						return
					}
					if command == vlessVisionCommandEnd {
						padding = false
					}
				}
			} else if _, err := downlink.Write(chunk); err != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// A conn whose writes can be held and sent as one write.
type testCoalescingConn struct {
	net.Conn

	stateLock sync.Mutex
	holding   bool
	held      []byte
}

func (self *testCoalescingConn) Write(b []byte) (int, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.holding {
		self.held = append(self.held, b...)
		return len(b), nil
	}
	return self.Conn.Write(b)
}

func (self *testCoalescingConn) hold() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.holding = true
}

// Sends what was held in one write. The lock is held across it so no other
// write can come between.
func (self *testCoalescingConn) flush() error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.holding = false
	held := self.held
	self.held = nil
	_, err := self.Conn.Write(held)
	return err
}

type testHijackResponseWriter struct {
	conn   net.Conn
	brw    *bufio.ReadWriter
	header http.Header
}

func (self *testHijackResponseWriter) Header() http.Header {
	return self.header
}

func (self *testHijackResponseWriter) Write(b []byte) (int, error) {
	return self.conn.Write(b)
}

func (self *testHijackResponseWriter) WriteHeader(statusCode int) {
	fmt.Fprintf(self.conn, "HTTP/1.1 %d %s\r\n\r\n", statusCode, http.StatusText(statusCode))
}

func (self *testHijackResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return self.conn, self.brw, nil
}
