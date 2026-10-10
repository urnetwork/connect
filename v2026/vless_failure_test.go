package connect

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/tls"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// A tls 1.3 server that is not a reality server -- the borrowed site, or a
// server that lost the key -- never yields a stream: its certificate cannot
// prove the configured key, so the dial fails, and no VLESS request reaches it.
func TestVlessRealityRefusesAServerThatDoesNotProveTheKey(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{newTestVlessCertificate(t, "www.cover.example")},
		MinVersion:   tls.VersionTLS13,
	}
	received := make(chan int, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		tlsConn := tls.Server(conn, tlsConfig)
		if err := tlsConn.Handshake(); err != nil {
			received <- 0
			return
		}
		tlsConn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, _ := io.Copy(io.Discard, tlsConn)
		received <- int(n)
	}()

	serverKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	config := &VlessConfig{
		Address:     "127.0.0.1",
		Port:        listener.Addr().(*net.TCPAddr).Port,
		Id:          testVlessUserId,
		Network:     VlessNetworkTcp,
		Security:    VlessSecurityReality,
		ServerName:  "www.cover.example",
		Fingerprint: "chrome",
		PublicKey:   serverKey.PublicKey().Bytes(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := DialVless(ctx, DefaultConnectSettings(), config, "tcp", "api.example:443")
	if err == nil {
		conn.Close()
		t.Fatal("a server that does not prove the key must not yield a stream")
	}
	if n := <-received; n != 0 {
		t.Fatalf("the server received %d bytes of application data", n)
	}
}

// A server that accepts the tcp connection and never answers holds the dial
// only as long as the caller's deadline, for every security.
func TestVlessDialHonorsTheContextDeadline(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var connsLock sync.Mutex
	conns := []net.Conn{}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			// hold it open, silent
			func() {
				connsLock.Lock()
				defer connsLock.Unlock()
				conns = append(conns, conn)
			}()
		}
	}()
	defer func() {
		connsLock.Lock()
		defer connsLock.Unlock()
		for _, conn := range conns {
			conn.Close()
		}
	}()

	serverKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	configs := []*VlessConfig{
		{Address: "127.0.0.1", Port: port, Id: testVlessUserId, Network: VlessNetworkTcp, Security: VlessSecurityTls, ServerName: "vless.example"},
		{Address: "127.0.0.1", Port: port, Id: testVlessUserId, Network: VlessNetworkTcp, Security: VlessSecurityReality, ServerName: "www.cover.example", PublicKey: serverKey.PublicKey().Bytes()},
		{Address: "127.0.0.1", Port: port, Id: testVlessUserId, Network: VlessNetworkWs, Security: VlessSecurityNone, Path: "/ws"},
		{Address: "127.0.0.1", Port: port, Id: testVlessUserId, Network: VlessNetworkHttpUpgrade, Security: VlessSecurityNone, Path: "/up"},
	}
	for _, config := range configs {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		start := time.Now()
		conn, err := DialVless(ctx, DefaultConnectSettings(), config, "tcp", "api.example:443")
		elapsed := time.Since(start)
		cancel()
		if err == nil {
			conn.Close()
			t.Fatalf("%s/%s: a silent server must fail the dial", config.Network, config.Security)
		}
		if 5*time.Second < elapsed {
			t.Fatalf("%s/%s: the dial took %s, past the 500ms deadline", config.Network, config.Security, elapsed)
		}
	}
}

// Replacing the VLESS dialers while requests run and the configurations are
// read is safe (run with -race) and leaves exactly the last set in force.
func TestClientStrategySetVlessConfigsConcurrently(t *testing.T) {
	destination := newTestVlessDestination(t)
	server := newTestVlessServer(t, VlessSecurityNone, VlessNetworkTcp)
	config := server.config(VlessFlowNone)

	settings := DefaultClientStrategySettings()
	settings.EnableNormal = false
	settings.EnableResilient = false
	settings.ExposeServerIps = false
	settings.ExposeServerHostNames = false
	settings.ExtenderDirectory = nil
	settings.ConnectSettings.TlsConfig = destination.tlsConfig
	settings.VlessConfigs = []*VlessConfig{config}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	strategy := NewClientStrategy(ctx, settings)
	defer strategy.Close()

	var waitGroup sync.WaitGroup
	for i := 0; i < 4; i += 1 {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for j := 0; j < 10; j += 1 {
				strategy.SetVlessConfigs([]*VlessConfig{config})
				strategy.VlessConfigs()
			}
		}()
	}
	for i := 0; i < 4; i += 1 {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			requestCtx, requestCancel := context.WithTimeout(ctx, 30*time.Second)
			defer requestCancel()
			// a request may lose its dialer to a concurrent replacement; it
			// must not race or panic
			HttpGetWithStrategyRaw(requestCtx, strategy, destination.server.URL+"/hello", "")
		}()
	}
	waitGroup.Wait()
	if n := len(strategy.VlessConfigs()); n != 1 {
		t.Fatalf("VLESS dialers after the replacements = %d, expected the one set last", n)
	}
	requestCtx, requestCancel := context.WithTimeout(ctx, 30*time.Second)
	defer requestCancel()
	if body, err := HttpGetWithStrategyRaw(requestCtx, strategy, destination.server.URL+"/hello", ""); err != nil || string(body) != "hello through vless" {
		t.Fatalf("after the replacements: body = %q err = %v", body, err)
	}
}
