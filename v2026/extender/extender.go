package extender

import (
	"context"
	"net"

	// "net/http"

	// "os"
	"fmt"
	"strings"
	"time"

	// "strconv"
	"slices"

	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/tls"

	// "crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"

	// "encoding/json"
	// "flag"
	"log"
	"math/big"
	"sync"

	// "crypto/md5"
	"encoding/binary"
	// "encoding/hex"
	// "syscall"

	// mathrand "math/rand"

	// "golang.org/x/crypto/cryptobyte"
	"golang.org/x/net/idna"

	// quic "github.com/quic-go/quic-go"
	"google.golang.org/protobuf/proto"

	// "src.agwa.name/tlshacks"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// server listens for a tls connect and replies with a self-signed cert
// server set up to forward to only subdomains of a root domain
// otherwise close connection

// if the header is not detected, proxy the request
// in this way the server looks like any CDN with misconfigured certs

// note anyone can host an extender server on their IP
// the IP can be manually entered into the app
// the default is to not require signatures, to allow all users
// signatures can be used to make the traffic private

// https://go.dev/src/crypto/tls/generate_cert.go

func DefaultExtenderSettings() *ExtenderSettings {
	return &ExtenderSettings{
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		ValidFrom:    180 * 24 * time.Hour,
		ValidFor:     180 * 24 * time.Hour,
	}
}

type ExtenderSettings struct {
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	ValidFrom    time.Duration
	ValidFor     time.Duration
	// Listen, when set, binds the outer TLS listener. Userspace integration
	// tests use it to place the production extender on a simulated TUN. Nil
	// retains net.Listen. The extender owns and closes returned listeners.
	Listen func(network string, address string) (net.Listener, error)
	// DialContext, when set, creates the forwarded inner connection. Userspace
	// integration tests use it for the extender-to-connect segment. Nil
	// retains forwardDialer. The extender owns and closes returned connections.
	DialContext connect.DialContextFunction
	// ErrorHandler, when set, receives connection-stage failures. Measurement
	// tests use it to make an otherwise client-visible timeout attributable.
	// It runs synchronously and must not block. Nil retains the silent
	// production behavior.
	ErrorHandler func(stage string, err error)
}

type ExtenderServer struct {
	ctx    context.Context
	cancel context.CancelFunc

	stateLock   sync.Mutex
	closing     bool
	listeners   map[*extenderOwnedListener]bool
	connections map[*extenderOwnedConnection]bool
	workers     sync.WaitGroup

	requireSignature bool
	allowedSecrets   []string
	// exact (x) or wildcard (*.x)
	// wildcard *.x does not match exact x
	allowedHosts  []string
	ports         map[int][]connect.ExtenderConnectMode
	forwardDialer *net.Dialer

	settings *ExtenderSettings
}

// An extenderOwnedListener identifies one listener in the shutdown set.
type extenderOwnedListener struct {
	listener net.Listener
}

// An extenderOwnedConnection identifies one connection in the shutdown set.
type extenderOwnedConnection struct {
	connection net.Conn
}

func NewExtenderServerWithDefaults(
	ctx context.Context,
	allowedSecrets []string,
	allowedHosts []string,
	ports map[int][]connect.ExtenderConnectMode,
	forwardDialer *net.Dialer,
) *ExtenderServer {
	return NewExtenderServer(
		ctx,
		allowedSecrets,
		allowedHosts,
		ports,
		forwardDialer,
		DefaultExtenderSettings(),
	)
}

func NewExtenderServer(
	ctx context.Context,
	allowedSecrets []string,
	allowedHosts []string,
	ports map[int][]connect.ExtenderConnectMode,
	forwardDialer *net.Dialer,
	settings *ExtenderSettings,
) *ExtenderServer {
	cancelCtx, cancel := context.WithCancel(ctx)

	return &ExtenderServer{
		ctx:            cancelCtx,
		cancel:         cancel,
		listeners:      map[*extenderOwnedListener]bool{},
		connections:    map[*extenderOwnedConnection]bool{},
		allowedSecrets: allowedSecrets,
		allowedHosts:   allowedHosts,
		ports:          ports,
		forwardDialer:  forwardDialer,
		settings:       settings,
	}

}

// Begins one owned server operation unless shutdown has already started.
func (self *ExtenderServer) beginWorker() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closing {
		return false
	}
	self.workers.Add(1)
	return true
}

// Completes one owned server operation.
func (self *ExtenderServer) endWorker() {
	self.workers.Done()
}

// Adds a listener to the resources interrupted by Close.
func (self *ExtenderServer) addListener(listener net.Listener) (*extenderOwnedListener, bool) {
	ownedListener := &extenderOwnedListener{listener: listener}
	self.stateLock.Lock()
	if self.closing {
		self.stateLock.Unlock()
		listener.Close()
		return nil, false
	}
	self.listeners[ownedListener] = true
	self.stateLock.Unlock()
	return ownedListener, true
}

// Removes a listener after its serving operation has released it.
func (self *ExtenderServer) removeListener(ownedListener *extenderOwnedListener) {
	self.stateLock.Lock()
	delete(self.listeners, ownedListener)
	self.stateLock.Unlock()
}

// Runs a connection handler whose socket is interrupted and joined at Close.
func (self *ExtenderServer) startConnection(connection net.Conn) {
	ownedConnection := &extenderOwnedConnection{connection: connection}
	self.stateLock.Lock()
	if self.closing {
		self.stateLock.Unlock()
		connection.Close()
		return
	}
	self.connections[ownedConnection] = true
	self.workers.Add(1)
	self.stateLock.Unlock()

	go func() {
		defer func() {
			self.stateLock.Lock()
			delete(self.connections, ownedConnection)
			self.stateLock.Unlock()
			self.workers.Done()
		}()
		self.HandleExtenderConnection(self.ctx, connection)
	}()
}

// ListenAndServe owns every accepted listener and connection until shutdown.
func (self *ExtenderServer) ListenAndServe() error {
	if !self.beginWorker() {
		return self.ctx.Err()
	}
	defer self.endWorker()
	defer self.Close()

	listeners := map[int]*extenderOwnedListener{}
	defer func() {
		for _, ownedListener := range listeners {
			ownedListener.listener.Close()
			self.removeListener(ownedListener)
		}
	}()
	// quicListeners := map[int]*quic.Listener{}

	for port, connectModes := range self.ports {
		if !slices.Contains(connectModes, connect.ExtenderConnectModeTcpTls) {
			continue
		}

		log.Printf("[extender] listen tcp %d", port)
		listen := net.Listen
		if self.settings.Listen != nil {
			listen = self.settings.Listen
		}
		listener, err := listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			// Ownership transfers for every non-nil callback result, even when
			// the callback rejects that result with an error.
			if listener != nil {
				listener.Close()
			}
			log.Printf("[extender] listen error: %s", err)
			return err
		}
		if listener == nil {
			return fmt.Errorf("extender listener factory returned nil")
		}
		ownedListener, ok := self.addListener(listener)
		if !ok {
			return self.ctx.Err()
		}
		listeners[port] = ownedListener
		if !self.beginWorker() {
			return self.ctx.Err()
		}
		go func() {
			defer self.endWorker()
			connect.HandleError(func() {
				defer self.Close()

				for {
					select {
					case <-self.ctx.Done():
						return
					default:
					}

					conn, err := listener.Accept()
					if err != nil {
						log.Printf("[extender] accept error: %s", err)
						return
					}
					self.startConnection(conn)
				}
			}, self.cancel)
		}()
	}

	/*
		for port, connectModes := range self.ports {
			if !slices.Contains(connectModes, connect.ExtenderConnectModeQuic) {
				continue
			}

			fmt.Printf("listen quic %d\n", port)
			// certPemBytes, keyPemBytes, err := selfSign(
			//     []string{"example.org"},
			//     guessOrganizationName("example.org"),
			// )
			// if err != nil {
			//     return err
			// }
			// // X509KeyPair
			// cert, err := tls.X509KeyPair(certPemBytes, keyPemBytes)
			// if err != nil {
			//     return err
			// }

			tlsConfig := &tls.Config{
				GetConfigForClient: func(clientHello *tls.ClientHelloInfo) (*tls.Config, error) {
					certPemBytes, keyPemBytes, err := selfSign(
						[]string{clientHello.ServerName},
						guessOrganizationName(clientHello.ServerName),
						self.settings.ValidFrom,
						self.settings.ValidFor,
					)
					if err != nil {
						return nil, err
					}
					// X509KeyPair
					cert, err := tls.X509KeyPair(certPemBytes, keyPemBytes)
					return &tls.Config{
						Certificates: []tls.Certificate{cert},
					}, err
				},
			}
			quicConfig := &quic.Config{}
			listener, err := quic.ListenAddr(fmt.Sprintf(":%d", port), tlsConfig, quicConfig)
			if err != nil {
				fmt.Printf("%s\n", err)
				return err
			}
			quicListeners[port] = listener
			go func() {
				defer self.cancel()

				for {
					select {
					case <-self.ctx.Done():
						return
					default:
					}

					conn, err := listener.Accept(self.ctx)
					if err != nil {
						fmt.Printf("%s\n", err)
						return
					}
					// fmt.Printf("Extender pre\n")
					go self.HandleQuicExtenderConnection(self.ctx, conn)
				}
			}()
		}
	*/

	// TODO
	/*
	   for _, port := range self.ports {
	       fmt.Printf("listen udp %d\n", port)
	       packetConn, err := net.ListenPacket("udp", fmt.Sprintf(":%d", port))
	       go func() {
	           defer self.cancel()

	           packetStreams := map[src]*packetStream{}

	           buffer := make([]byte, 4096)

	           for {
	               select {
	               case <- self.ctx.Done():
	                   return
	               default:
	               }



	               n, addr, err := packetConn.ReadFrom(buffer)
	               if err != nil {
	                   fmt.Printf("%s\n", err)
	                   return
	               }

	               address := addr.String()

	               packetSteam, ok := packetStreams[address]
	               if !ok {
	                   packetStream = newServerPacketStream(packetConn, addr, UdpMtu)
	                   // fixme clean up packet stream
	                   // go func() {
	                   //     select {
	                   //     case <- packetStream.Done():
	                   //     }

	                   // }()
	                   go HandleExtenderConnection(self.ctx, packetStream)
	               }

	               packetStream.AddPacket(buffer[0:n])
	           }

	       }()
	   }
	*/

	select {
	case <-self.ctx.Done():
	}
	// for _, listener := range quicListeners {
	// 	listener.Close()
	// }

	return nil
}

func (self *ExtenderServer) Close() {
	self.stateLock.Lock()
	if self.closing {
		self.stateLock.Unlock()
		return
	}
	self.closing = true
	self.cancel()
	listeners := make([]net.Listener, 0, len(self.listeners))
	for ownedListener := range self.listeners {
		listeners = append(listeners, ownedListener.listener)
	}
	connections := make([]net.Conn, 0, len(self.connections))
	for ownedConnection := range self.connections {
		connections = append(connections, ownedConnection.connection)
	}
	self.stateLock.Unlock()

	for _, listener := range listeners {
		listener.Close()
	}
	for _, connection := range connections {
		connection.Close()
	}
}

// CloseAndWait interrupts and joins every listener and connection worker.
func (self *ExtenderServer) CloseAndWait() {
	self.Close()
	self.workers.Wait()
}

func (self *ExtenderServer) IsAllowedSecret(header *protocol.ExtenderHeader) bool {
	for _, secret := range self.allowedSecrets {
		mac := hmac.New(sha256.New, []byte(secret))
		timestampBytes := make([]byte, 8)
		binary.BigEndian.PutUint64(timestampBytes[0:8], header.Timestamp)
		mac.Write(timestampBytes)
		mac.Write(header.Nonce)
		signature := mac.Sum(nil)
		if slices.Equal(signature, header.Signature) {
			return true
		}
	}
	return false
}

func (self *ExtenderServer) IsAllowedHost(host string) bool {
	_, err := idna.ToUnicode(host)
	if err != nil {
		// not a valid host
		return false
	}
	for _, allowedHost := range self.allowedHosts {
		if host == allowedHost {
			return true
		}
		if strings.HasPrefix(allowedHost, "*.") {
			if strings.HasSuffix(host, allowedHost[1:]) {
				return true
			}
		}
	}
	return false
}

// Connection errors are observable only when a caller installs the test seam.
func (self *ExtenderServer) reportError(stage string, err error) {
	if self.settings.ErrorHandler != nil {
		self.settings.ErrorHandler(stage, err)
	}
}

func (self *ExtenderServer) HandleExtenderConnection(ctx context.Context, conn net.Conn) {

	handleCtx, handleCancel := context.WithCancel(ctx)
	defer handleCancel()

	defer conn.Close()

	// fmt.Printf("Extender 1\n")

	// FIXME switch to normal proxy if there are no tls fragments

	/*
	   handshakeBytes, clientHello, err := func()([]byte, *tlshacks.ClientHelloInfo, error) {
	       handshakeBytes := make([]byte, 8192)
	       handshakeBytesCount := 0
	       for handshakeBytesCount < len(handshakeBytes) {
	           // wait a short time for fragmented packets
	           conn.SetReadDeadline(time.Now().Add(ReadTimeout))
	           n, err := conn.Read(handshakeBytes[handshakeBytesCount:])
	           if err != nil {
	               return nil, nil, err
	           }
	           handshakeBytesCount += n

	           fmt.Printf("Extender handshake 1: %s\n", string(handshakeBytes[0:handshakeBytesCount]))
	           clientHello := UnmarshalClientHello(handshakeBytes[0:handshakeBytesCount])
	           if clientHello != nil {
	               return handshakeBytes[0:handshakeBytesCount], clientHello, nil
	           }
	           fmt.Printf("Extender handshake deepen\n")
	       }
	       return nil, nil, fmt.Errorf("Did not read complete handshake after %d bytes.", len(handshakeBytes))
	   }()
	   if err != nil {
	       return
	   }
	*/
	/*
		    recordReader := newReaderRecordInitialBytes(conn)
		    handshakeReader := tlshacks.NewHandshakeReader(recordReader)
		    handshakeBytes, err := handshakeReader.ReadMessage()
		    if err != nil {
		        return
		    }

		    clientHello := UnmarshalClientHello(handshakeBytes)
		    if clientHello == nil {
		        return
		    }



		    fmt.Printf("Extender 2\n")

		    if clientHello.Info.ServerName == nil {
		        return
		    }


		    fmt.Printf("Extender 3: %s\n", *clientHello.Info.ServerName)

			// generate a cert for that server name

			// start a tls server connection using the cert and pass in the hello bytes
			// pass in future bytes to the connection

		    certPemBytes, keyPemBytes, err := selfSign(
		        []string{*clientHello.Info.ServerName},
		        guessOrganizationName(*clientHello.Info.ServerName),
		    )
		    if err != nil {
		        return
		    }
		    // X509KeyPair
		    cert, err := tls.X509KeyPair(certPemBytes, keyPemBytes)
		    if err != nil {
		        return
		    }

		    fmt.Printf("Extender 4 with initial bytes: %s\n", string(recordReader.InitialBytes()))
		    fmt.Printf("Cert: %s\n\n", string(certPemBytes))
		    fmt.Printf("Key: %s\n\n", string(keyPemBytes))



			// todo need a net.COnn implementation that allows inserting bytes back at the front


		    tlsConfig := &tls.Config{
		        Certificates: []tls.Certificate{cert},
		        ServerName: *clientHello.Info.ServerName,
		    }
		    // put the handshake bytes back in front
		    rewindConn := newConnWithInitialBytes(conn, recordReader.InitialBytes())
			clientConn := tls.Server(rewindConn, tlsConfig)
		    defer clientConn.Close()
	*/

	tlsConfig := &tls.Config{
		GetCertificate: func(clientHello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			certPemBytes, keyPemBytes, err := selfSign(
				[]string{clientHello.ServerName},
				guessOrganizationName(clientHello.ServerName),
				self.settings.ValidFrom,
				self.settings.ValidFor,
			)
			if err != nil {
				return nil, err
			}
			// X509KeyPair
			cert, err := tls.X509KeyPair(certPemBytes, keyPemBytes)
			return &cert, err
		},
	}
	clientConn := tls.Server(conn, tlsConfig)
	defer clientConn.Close()

	// fmt.Printf("Extender 5\n")

	err := clientConn.HandshakeContext(handleCtx)
	if err != nil {
		self.reportError("outer TLS handshake", err)
		return
	}

	// fmt.Printf("Extender 6\n")

	// read extender header
	headerBytes := make([]byte, 1024)

	// TODO is header parsing doesn't work, forward the traffic to the SNI site and write the header bytes

	clientConn.SetReadDeadline(time.Now().Add(self.settings.ReadTimeout))
	for i := 0; i < 4; {
		n, err := clientConn.Read(headerBytes[i:4])
		i += n
		if err != nil {
			self.reportError("header length", err)
			return
		}
	}
	headerByteCount := int(binary.BigEndian.Uint32(headerBytes[0:4]))
	if 1024 < headerByteCount {
		// bad data
		self.reportError("header length", fmt.Errorf("header has %d bytes", headerByteCount))
		return
	}
	// fmt.Printf("Extender 6: %d\n", headerByteCount)
	for i := 0; i < headerByteCount; {
		clientConn.SetReadDeadline(time.Now().Add(self.settings.ReadTimeout))
		n, err := clientConn.Read(headerBytes[i:headerByteCount])
		i += n
		if err != nil {
			self.reportError("header body", err)
			return
		}
	}
	// fmt.Printf("Extender 7\n")

	header := &protocol.ExtenderHeader{}
	err = proto.Unmarshal(headerBytes[0:headerByteCount], header)
	if err != nil {
		self.reportError("header decode", err)
		return
	}

	if !self.IsAllowedSecret(header) {
		// fmt.Printf("Extender secret failed: %s\n", header.Secret)
		self.reportError("header authorization", fmt.Errorf("secret signature is not allowed"))
		return
	}

	if !self.IsAllowedHost(header.DestinationHost) {
		// fmt.Printf("Extender destination failed: %s\n", header.DestinationHost)
		self.reportError("destination authorization", fmt.Errorf("host %q is not allowed", header.DestinationHost))
		return
	}

	dialContext := self.forwardDialer.DialContext
	if self.settings.DialContext != nil {
		dialContext = self.settings.DialContext
	}
	forwardConn, err := dialContext(handleCtx, "tcp", net.JoinHostPort(
		header.DestinationHost,
		fmt.Sprintf("%d", header.DestinationPort),
	))
	if err != nil {
		// Ownership transfers for every non-nil callback result, even when
		// the callback also returns an error.
		if forwardConn != nil {
			forwardConn.Close()
		}
		self.reportError("forward dial", err)
		return
	}
	if forwardConn == nil {
		self.reportError("forward dial", fmt.Errorf("forward dial returned nil connection"))
		return
	}
	defer forwardConn.Close()

	var relayWorkers sync.WaitGroup
	relayWorkers.Add(2)
	go connect.HandleError(func() {
		defer relayWorkers.Done()
		// read packet from clientConn, write to forwardConn
		defer handleCancel()

		buffer := make([]byte, 4096)

		for {
			select {
			case <-handleCtx.Done():
				return
			default:
			}

			clientConn.SetReadDeadline(time.Now().Add(self.settings.ReadTimeout))
			n, err := clientConn.Read(buffer)
			if n > 0 {
				forwardConn.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
				toWrite := buffer[0:n]
				for len(toWrite) > 0 {
					nw, werr := forwardConn.Write(toWrite)
					if nw > 0 {
						toWrite = toWrite[nw:]
					}
					if werr != nil {
						return
					}
				}
			}
			if err != nil {
				return
			}
		}
	}, handleCancel)

	go connect.HandleError(func() {
		defer relayWorkers.Done()
		// read packet from forwardConn, write to clientConn
		defer handleCancel()

		buffer := make([]byte, 4096)

		for {
			select {
			case <-handleCtx.Done():
				return
			default:
			}

			forwardConn.SetReadDeadline(time.Now().Add(self.settings.ReadTimeout))
			n, err := forwardConn.Read(buffer)
			if n > 0 {
				clientConn.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
				toWrite := buffer[0:n]
				for len(toWrite) > 0 {
					nw, werr := clientConn.Write(toWrite)
					if nw > 0 {
						toWrite = toWrite[nw:]
					}
					if werr != nil {
						return
					}
				}
			}
			if err != nil {
				return
			}
		}
	}, handleCancel)

	select {
	case <-handleCtx.Done():
	}
	clientConn.Close()
	forwardConn.Close()
	relayWorkers.Wait()
}

/*
func (self *ExtenderServer) HandleQuicExtenderConnection(ctx context.Context, conn quic.Connection) {

	fmt.Printf("quic conn\n")

	handleCtx, handleCancel := context.WithCancel(ctx)
	defer handleCancel()

	clientStream, err := conn.AcceptStream(ctx)
	if err != nil {
		return
	}
	defer clientStream.Close()

	fmt.Printf("quic stream\n")

	// read extender header
	headerBytes := make([]byte, 1024)

	fmt.Printf("q 1\n")

	clientStream.SetReadDeadline(time.Now().Add(self.settings.ReadTimeout))
	for i := 0; i < 4; {
		n, err := clientStream.Read(headerBytes[i:4])
		if err != nil {
			return
		}
		i += n
	}
	fmt.Printf("q 2\n")
	headerByteCount := int(binary.BigEndian.Uint32(headerBytes[0:4]))
	if 1024 < headerByteCount {
		// bad data
		return
	}
	fmt.Printf("q 3\n")
	// fmt.Printf("Extender 6: %d\n", headerByteCount)
	for i := 0; i < headerByteCount; {
		clientStream.SetReadDeadline(time.Now().Add(self.settings.ReadTimeout))
		n, err := clientStream.Read(headerBytes[i:headerByteCount])
		if err != nil {
			return
		}
		i += n
	}
	// fmt.Printf("Extender 7\n")
	fmt.Printf("q 4\n")

	header := &protocol.ExtenderHeader{}
	err = proto.Unmarshal(headerBytes[0:headerByteCount], header)
	if err != nil {
		return
	}

	fmt.Printf("q 5\n")

	if !self.IsAllowedSecret(header) {
		// fmt.Printf("Extender secret failed: %s\n", header.Secret)
		return
	}

	fmt.Printf("q 6\n")

	if !self.IsAllowedHost(header.DestinationHost) {
		// fmt.Printf("Extender destination failed: %s\n", header.DestinationHost)
		return
	}

	fmt.Printf("q 7: %s %d\n", header.DestinationHost, header.DestinationPort)

	var resolvedHost string
	if header.DestinationHost == "api.bringyour.com" {
		resolvedHost = "65.19.157.41"
	} else if header.DestinationHost == "connect.bringyour.com" {
		resolvedHost = "65.49.70.71"
	} else {
		resolvedHost = header.DestinationHost
	}

	forwardConn, err := self.forwardDialer.Dial("tcp", net.JoinHostPort(
		// header.DestinationHost,
		resolvedHost,
		fmt.Sprintf("%d", header.DestinationPort),
	))
	if err != nil {
		return
	}
	defer forwardConn.Close()

	fmt.Printf("q 8\n")

	go func() {
		// read packet from clientConn, write to forwardConn
		defer handleCancel()

		buffer := make([]byte, 4096)

		for {
			select {
			case <-handleCtx.Done():
				return
			default:
			}

			clientStream.SetReadDeadline(time.Now().Add(self.settings.ReadTimeout))
			n, err := clientStream.Read(buffer)
			if err != nil {
				return
			}
			forwardConn.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
			_, err = forwardConn.Write(buffer[0:n])
			if err != nil {
				fmt.Printf("q r end\n")
				return
			}
		}
	}()

	go func() {
		// read packet from forwardConn, write to clientConn
		defer handleCancel()

		buffer := make([]byte, 4096)

		for {
			select {
			case <-handleCtx.Done():
				return
			default:
			}

			forwardConn.SetReadDeadline(time.Now().Add(self.settings.ReadTimeout))
			n, err := forwardConn.Read(buffer)
			if err != nil {
				return
			}
			clientStream.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
			_, err = clientStream.Write(buffer[0:n])
			if err != nil {
				fmt.Printf("q w end\n")
				return
			}
		}
	}()

	select {
	case <-handleCtx.Done():
	}

	fmt.Printf("q end\n")
}
*/

func guessOrganizationName(host string) string {

	// FIXME bringyour api for organization name
	/* For the following hostname, tell me your best guess at the organization name. Only list the full organization name and nothing else. The hostname: yandex.ru
	 */

	// FIXME
	return host
}

/*
type readerRecordInitialBytes struct {
    conn net.Conn
    initialBytes []byte
}

func newReaderRecordInitialBytes(conn net.Conn) *readerRecordInitialBytes {
    return &readerRecordInitialBytes{
        conn: conn,
    }
}

func (self *readerRecordInitialBytes) InitialBytes() []byte {
    return slices.Clone(self.initialBytes)
}

func (self *readerRecordInitialBytes) Read(b []byte) (int, error) {
    n, err := self.conn.Read(b)
    if 0 < n {
    	// FIXME need to make a copy
        self.initialBytes = append(self.initialBytes, COPY(b[0:n])...)
    }
    return n, err
}




type connWithInitialBytes struct {
    conn net.Conn
    initialBytes []byte
}

func newConnWithInitialBytes(conn net.Conn, initialBytes []byte) *connWithInitialBytes {
    return &connWithInitialBytes{
        conn: conn,
        initialBytes: initialBytes,
    }
}

func (self *connWithInitialBytes) Read(b []byte) (int, error) {
    m := min(len(self.initialBytes), len(b))
    if 0 < m {
        copy(b[0:m], self.initialBytes[0:m])
        self.initialBytes = self.initialBytes[m:]
    }
    if len(b) <= m {
        return m, nil
    }
    n, err := self.conn.Read(b[m:])
    return m + n, err
}

func (self *connWithInitialBytes) Write(b []byte) (int, error) {
    return self.conn.Write(b)
}

func (self *connWithInitialBytes) Close() error {
    return self.conn.Close()
}

func (self *connWithInitialBytes) LocalAddr() net.Addr {
    return self.conn.LocalAddr()
}

func (self *connWithInitialBytes) RemoteAddr() net.Addr {
    return self.conn.RemoteAddr()
}

func (self *connWithInitialBytes) SetDeadline(t time.Time) error {
    return self.conn.SetDeadline(t)
}

func (self *connWithInitialBytes) SetReadDeadline(t time.Time) error {
    return self.conn.SetReadDeadline(t)
}

func (self *connWithInitialBytes) SetWriteDeadline(t time.Time) error {
    return self.conn.SetWriteDeadline(t)
}
*/

// https://github.com/AGWA/tlshacks/blob/main/client_hello.go
// https://pkg.go.dev/crypto/tls#ClientHelloInfo
// https://www.agwa.name/blog/post/parsing_tls_client_hello_with_cryptobyte

// client issues tls connect to for a spoof name and ip:port, and does not check the tls cert
// on top of that connection, sends a header (protocol/extender) that lists the upstream host
// and then makes a tls connection through that

// https://go.dev/src/crypto/tls/generate_cert.go

func selfSign(hosts []string, organization string, validFrom time.Duration, validFor time.Duration) (certPemBytes []byte, keyPemBytes []byte, returnErr error) {

	var priv any
	var err error

	priv, err = rsa.GenerateKey(rand.Reader, 2048)
	// priv, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		returnErr = err
		return
	}

	publicKey := func(priv any) any {
		switch k := priv.(type) {
		case *rsa.PrivateKey:
			return &k.PublicKey
		case *ecdsa.PrivateKey:
			return &k.PublicKey
		case ed25519.PrivateKey:
			return k.Public().(ed25519.PublicKey)
		default:
			return nil
		}
	}

	// ECDSA, ED25519 and RSA subject keys should have the DigitalSignature
	// KeyUsage bits set in the x509.Certificate template
	keyUsage := x509.KeyUsageDigitalSignature
	// Only RSA subject keys should have the KeyEncipherment KeyUsage bits set. In
	// the context of TLS this KeyUsage is particular to RSA key exchange and
	// authentication.
	if _, isRSA := priv.(*rsa.PrivateKey); isRSA {
		keyUsage |= x509.KeyUsageKeyEncipherment
	}

	notBefore := time.Now().Add(-validFrom)
	// ValidFrom is the tolerated clock-skew/history window before creation;
	// ValidFor is the future lifetime after creation. Adding both durations
	// to notBefore made the default 180d/180d certificate expire at the
	// instant it was generated.
	notAfter := time.Now().Add(validFor)

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		log.Fatalf("Failed to generate serial number: %v", err)
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{organization},
		},
		NotBefore: notBefore,
		NotAfter:  notAfter,

		KeyUsage:              keyUsage,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, h)
		}
	}

	// we hope the client is using tls1.3 which hides the self signed cert
	template.IsCA = true
	template.KeyUsage |= x509.KeyUsageCertSign

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, publicKey(priv), priv)
	if err != nil {
		returnErr = err
		return
	}
	certPemBytes = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})

	privBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		returnErr = err
		return
	}
	keyPemBytes = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privBytes})

	return
}
