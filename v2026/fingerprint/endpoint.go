package fingerprint

// endpoint.go -- the shared local tls endpoint both real Chrome and connect's
// dialer hit, so a golden and a live hello are captured the same way.
//
// it is a tls 1.3 + h2/http1.1 https server, certified for the documentation
// name endpoint.example by a private ca, that offers the default go group set
// -- which from go 1.24 includes the post-quantum X25519MLKEM768 -- so a client
// emits its post-quantum key share and completes in one round trip. it records
// each connection's raw first flight before its tls server reads it, which is
// the ClientHello whatever client sent it.
//
// it does not restrict the group list: restricting it to X25519MLKEM768 would
// be the server advertising, not offering, and a client that could not do the
// post-quantum exchange would be forced into a hello-retry. the default set
// both offers the post-quantum exchange to a client that wants it and completes
// with one that does not.
//
// the quic/h3 path the owner's note also asks for is future work gated on the
// quicv2 branch; see README.md. this endpoint is the tcp/tls half, which is
// what the merged client-hello work needs now.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"time"
)

// ServerName is the documentation sni both Chrome and connect dial the
// endpoint under (rfc 2606 reserves .example). the endpoint's leaf certifies
// it.
const ServerName = "endpoint.example"

// the tls record content type of a handshake record (rfc 8446 5.1), the only
// type a first flight holds.
const recordTypeHandshake byte = 0x16

// CapturedClientHello is one connection's ClientHello as it arrived: the
// handshake message (the record payloads joined) and the number of tls records
// it came in.
type CapturedClientHello struct {
	Message     []byte
	RecordCount int
}

// EndpointOptions configures a new endpoint.
type EndpointOptions struct {
	// the host to bind, default 127.0.0.1. Layer B binds a host-reachable
	// address instead so a Docker container can reach it.
	BindHost string
	// the http handler, default a handler that answers "ok" at /.
	Handler http.Handler
}

// Endpoint is a running shared local tls endpoint. Close it when done.
type Endpoint struct {
	server     *httptest.Server
	caCertPool *x509.CertPool
	caPem      []byte
	port       int

	stateLock sync.Mutex
	captures  []CapturedClientHello
}

// NewEndpoint starts a shared local tls endpoint. it listens on an ephemeral
// port, certifies ServerName with a fresh private ca, offers h2 and http/1.1,
// and captures every connection's first flight.
func NewEndpoint(opts EndpointOptions) (*Endpoint, error) {
	bindHost := opts.BindHost
	if bindHost == "" {
		bindHost = "127.0.0.1"
	}
	handler := opts.Handler
	if handler == nil {
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "ok")
		})
		handler = mux
	}

	caCertPool, caPem, certificate, err := newEndpointCertificates(ServerName)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(bindHost, "0"))
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	endpoint := &Endpoint{
		caCertPool: caCertPool,
		caPem:      caPem,
		port:       listener.Addr().(*net.TCPAddr).Port,
	}
	server := httptest.NewUnstartedServer(handler)
	server.Listener.Close()
	server.Listener = &captureListener{Listener: listener, endpoint: endpoint}
	server.EnableHTTP2 = true
	server.TLS = &tls.Config{
		Certificates: []tls.Certificate{certificate},
		NextProtos:   []string{"h2", "http/1.1"},
		// CurvePreferences deliberately unset: the go default (>= 1.24)
		// already offers X25519MLKEM768, and setting it would restrict rather
		// than offer. the post-quantum exchange test proves it is negotiated.
	}
	server.StartTLS()
	endpoint.server = server
	return endpoint, nil
}

// Addr is the host:port the endpoint listens on, for a dial.
func (self *Endpoint) Addr() string {
	return self.server.Listener.Addr().String()
}

// Port is the endpoint's tcp port.
func (self *Endpoint) Port() int {
	return self.port
}

// CaCertPool is the private ca a client must trust to verify the endpoint.
func (self *Endpoint) CaCertPool() *x509.CertPool {
	return self.caCertPool
}

// CaPem is the private ca in pem form, to write to a file for a client (real
// Chrome) that trusts roots from disk.
func (self *Endpoint) CaPem() []byte {
	return slices.Clone(self.caPem)
}

// CapturedClientHellos is the first flights captured so far, in arrival order.
// a hello is captured before the server answers it, so a completed dial's hello
// is always present.
func (self *Endpoint) CapturedClientHellos() []CapturedClientHello {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.captures)
}

// Close shuts the endpoint down.
func (self *Endpoint) Close() {
	self.server.Close()
}

// addCapture records one connection's first flight.
func (self *Endpoint) addCapture(capture CapturedClientHello) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.captures = append(self.captures, capture)
}

// captureListener hands each accepted connection to a capture of its first
// flight.
type captureListener struct {
	net.Listener
	endpoint *Endpoint
}

func (self *captureListener) Accept() (net.Conn, error) {
	conn, err := self.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &captureConn{Conn: conn, endpoint: self.endpoint}, nil
}

// captureConn keeps the bytes the tls server reads until they hold a complete
// ClientHello, then records it. Read is called only by the goroutine serving
// the connection.
type captureConn struct {
	net.Conn
	endpoint *Endpoint
	raw      []byte
	captured bool
}

func (self *captureConn) Read(b []byte) (int, error) {
	n, err := self.Conn.Read(b)
	if !self.captured && 0 < n {
		self.raw = append(self.raw, b[:n]...)
		if capture := parseFirstFlight(self.raw); capture != nil {
			self.captured = true
			self.raw = nil
			self.endpoint.addCapture(*capture)
		}
	}
	return n, err
}

// parseFirstFlight returns the ClientHello at the start of raw once raw holds
// all of it, its handshake records' payloads joined. nil while it is
// incomplete, or when raw does not start with handshake records.
func parseFirstFlight(raw []byte) *CapturedClientHello {
	var message []byte
	recordCount := 0
	for 5 <= len(raw) {
		if raw[0] != recordTypeHandshake {
			return nil
		}
		length := int(binary.BigEndian.Uint16(raw[3:5]))
		if len(raw) < 5+length {
			return nil
		}
		message = append(message, raw[5:5+length]...)
		raw = raw[5+length:]
		recordCount += 1
		if 4 <= len(message) {
			messageLength := 4 + (int(message[1])<<16 | int(message[2])<<8 | int(message[3]))
			if messageLength <= len(message) {
				return &CapturedClientHello{Message: message[:messageLength], RecordCount: recordCount}
			}
		}
	}
	return nil
}

// newEndpointCertificates makes a private ca and a leaf it certifies for
// serverName, returning the ca as a pool and as pem, and the leaf as a tls
// certificate.
func newEndpointCertificates(serverName string) (*x509.CertPool, []byte, tls.Certificate, error) {
	now := time.Now()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, tls.Certificate{}, err
	}
	rootTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "fingerprint endpoint root"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	rootDer, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, &rootKey.PublicKey, rootKey)
	if err != nil {
		return nil, nil, tls.Certificate{}, err
	}
	root, err := x509.ParseCertificate(rootDer)
	if err != nil {
		return nil, nil, tls.Certificate{}, err
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, tls.Certificate{}, err
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: serverName},
		DNSNames:     []string{serverName},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDer, err := x509.CreateCertificate(rand.Reader, leafTemplate, root, &leafKey.PublicKey, rootKey)
	if err != nil {
		return nil, nil, tls.Certificate{}, err
	}
	caCertPool := x509.NewCertPool()
	caCertPool.AddCert(root)
	caPem := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDer})
	return caCertPool, caPem, tls.Certificate{Certificate: [][]byte{leafDer}, PrivateKey: leafKey}, nil
}
