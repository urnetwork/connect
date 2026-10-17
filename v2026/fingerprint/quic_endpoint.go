package fingerprint

// quic_endpoint.go -- the udp half of the shared endpoint: a local QUIC/h3
// server that both real Chrome (Layer B) and connect's quic dialer (Layer A)
// hit, capturing the raw first datagram -- the client's Initial -- before
// quic-go processes it.
//
// it listens on an ephemeral udp port, certifies ServerName with a fresh
// private ca, offers the "h3" alpn, and accepts both QUIC versions so a client
// reaches it whichever version its policy offers first. the capture reads the
// first datagram off the packet conn, which is the unprotected-long-header
// Initial whatever client sent it.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"slices"
	"sync"
	"time"

	quic "github.com/quic-go/quic-go"
)

// QuicEndpointOptions configures a new QUIC endpoint.
type QuicEndpointOptions struct {
	// the host to bind, default 127.0.0.1. Layer B binds a host-reachable
	// address so a Docker container can reach it.
	BindHost string
}

// QuicEndpoint is a running shared local QUIC/h3 endpoint. Close it when done.
type QuicEndpoint struct {
	udpConn    *net.UDPConn
	transport  *quic.Transport
	listener   *quic.Listener
	caCertPool *x509.CertPool
	caPem      []byte
	cancel     context.CancelFunc

	stateLock        sync.Mutex
	capturedInitials [][]byte
	acceptedConns    []*quic.Conn
}

// NewQuicEndpoint starts a shared local QUIC/h3 endpoint.
func NewQuicEndpoint(opts QuicEndpointOptions) (*QuicEndpoint, error) {
	bindHost := opts.BindHost
	if bindHost == "" {
		bindHost = "127.0.0.1"
	}
	caCertPool, caPem, certificate, err := newEndpointCertificates(ServerName)
	if err != nil {
		return nil, err
	}
	udpAddr, err := net.ResolveUDPAddr("udp4", net.JoinHostPort(bindHost, "0"))
	if err != nil {
		return nil, err
	}
	udpConn, err := net.ListenUDP("udp4", udpAddr)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	endpoint := &QuicEndpoint{
		udpConn:    udpConn,
		caCertPool: caCertPool,
		caPem:      caPem,
		cancel:     cancel,
	}
	// a plain net.PacketConn wrapper (not the *net.UDPConn itself), so quic-go
	// reads through ReadFrom and the capture sees every datagram, rather than
	// taking the oob fast path that would bypass it.
	endpoint.transport = &quic.Transport{Conn: &quicCaptureConn{udpConn: udpConn, endpoint: endpoint}}
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{certificate},
		NextProtos:   []string{"h3"},
	}
	// accept both versions, so the client reaches the endpoint whichever its
	// policy offers first.
	quicConfig := &quic.Config{Versions: []quic.Version{quic.Version2, quic.Version1}}
	listener, err := endpoint.transport.Listen(tlsConfig, quicConfig)
	if err != nil {
		cancel()
		endpoint.transport.Close()
		udpConn.Close()
		return nil, err
	}
	endpoint.listener = listener
	go endpoint.acceptLoop(ctx)
	return endpoint, nil
}

// acceptLoop completes the handshakes clients bring, so a dial returns cleanly;
// the capture does not depend on it, as the read loop records the first
// datagram whether or not a connection is accepted.
func (self *QuicEndpoint) acceptLoop(ctx context.Context) {
	for {
		conn, err := self.listener.Accept(ctx)
		if err != nil {
			return
		}
		self.stateLock.Lock()
		self.acceptedConns = append(self.acceptedConns, conn)
		self.stateLock.Unlock()
	}
}

// Addr is the udp address to dial.
func (self *QuicEndpoint) Addr() net.Addr {
	return self.udpConn.LocalAddr()
}

// CaCertPool is the private ca a client must trust to verify the endpoint.
func (self *QuicEndpoint) CaCertPool() *x509.CertPool {
	return self.caCertPool
}

// CaPem is the private ca in pem form, to write to a file for a client (real
// Chrome) that trusts roots from disk.
func (self *QuicEndpoint) CaPem() []byte {
	return slices.Clone(self.caPem)
}

// CapturedInitials is the datagrams captured so far, in arrival order; the
// first is the client's Initial.
func (self *QuicEndpoint) CapturedInitials() [][]byte {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.capturedInitials)
}

// Close shuts the endpoint down.
func (self *QuicEndpoint) Close() {
	self.cancel()
	self.stateLock.Lock()
	conns := slices.Clone(self.acceptedConns)
	self.stateLock.Unlock()
	for _, conn := range conns {
		_ = conn.CloseWithError(quic.ApplicationErrorCode(0), "")
	}
	self.listener.Close()
	self.transport.Close()
	self.udpConn.Close()
}

// addInitial records one captured datagram.
func (self *QuicEndpoint) addInitial(datagram []byte) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.capturedInitials = append(self.capturedInitials, datagram)
}

// quicCaptureConn is a net.PacketConn over a udp conn that records the bytes of
// each datagram it reads. it implements the interface explicitly rather than
// embedding the *net.UDPConn, so it does NOT promote the oob methods that would
// make quic-go bypass ReadFrom.
type quicCaptureConn struct {
	udpConn  *net.UDPConn
	endpoint *QuicEndpoint
}

func (self *quicCaptureConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, addr, err := self.udpConn.ReadFrom(b)
	if 0 < n {
		datagram := make([]byte, n)
		copy(datagram, b[:n])
		self.endpoint.addInitial(datagram)
	}
	return n, addr, err
}

func (self *quicCaptureConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	return self.udpConn.WriteTo(b, addr)
}

func (self *quicCaptureConn) Close() error {
	return self.udpConn.Close()
}

func (self *quicCaptureConn) LocalAddr() net.Addr {
	return self.udpConn.LocalAddr()
}

func (self *quicCaptureConn) SetDeadline(t time.Time) error {
	return self.udpConn.SetDeadline(t)
}

func (self *quicCaptureConn) SetReadDeadline(t time.Time) error {
	return self.udpConn.SetReadDeadline(t)
}

func (self *quicCaptureConn) SetWriteDeadline(t time.Time) error {
	return self.udpConn.SetWriteDeadline(t)
}
