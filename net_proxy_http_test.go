package connect

// net_proxy_http_test.go — HTTP CONNECT proxying (net_proxy_http.go): an api
// request and a platform transport's websocket dial through a fake CONNECT
// proxy, the proxy's refusals as dial errors, its credentials, a cancel in
// the middle of the exchange, and that a ProxySettings with no Protocol still
// speaks SOCKS5. Every host is a .example name the fake proxy maps to a local
// server; nothing here resolves a name or leaves the loopback.

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/net/proxy"
)

// The synthetic names the proxy tests dial.
const (
	testHttpConnectApiName      = "api.proxytest.example"
	testHttpConnectPlatformName = "connect.proxytest.example"
)

// How long a proxy test waits on anything before calling itself broken. Every
// wait is on a channel or a dial's own completion; this bounds a hang.
const testHttpConnectTimeout = 30 * time.Second

// One CONNECT the fake proxy read.
type testHttpConnectRequest struct {
	method        string
	target        string
	authorization string
}

// A fake HTTP CONNECT proxy on the v4 loopback: it reads each connection's
// request, records it, answers with answer's status, and on a 2xx tunnels to
// backend, whatever the request named, after sending tail.
type testHttpConnectProxy struct {
	listener net.Listener
	backend  string
	answer   func(request *testHttpConnectRequest) int
	tail     []byte
	// closed once a request has been read, before it is answered, when set
	requestRead chan struct{}
	// the answer waits for this to close, when set
	hold chan struct{}

	stateLock sync.Mutex
	requests  []*testHttpConnectRequest
	conns     []net.Conn
	waitGroup sync.WaitGroup
}

// Listens until the test ends, tunneling to backend.
func newTestHttpConnectProxy(t *testing.T, backend string, answer func(request *testHttpConnectRequest) int) *testHttpConnectProxy {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	self := &testHttpConnectProxy{
		listener: listener,
		backend:  backend,
		answer:   answer,
	}
	self.waitGroup.Add(1)
	go func() {
		defer self.waitGroup.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			self.track(conn)
			self.waitGroup.Add(1)
			go func() {
				defer self.waitGroup.Done()
				self.serve(conn)
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		self.stateLock.Lock()
		conns := slices.Clone(self.conns)
		self.stateLock.Unlock()
		for _, conn := range conns {
			conn.Close()
		}
		self.waitGroup.Wait()
	})
	return self
}

func (self *testHttpConnectProxy) track(conn net.Conn) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.conns = append(self.conns, conn)
}

// The proxy's address, for ProxySettings.
func (self *testHttpConnectProxy) address() string {
	return self.listener.Addr().String()
}

// What it read, in arrival order.
func (self *testHttpConnectProxy) readRequests() []*testHttpConnectRequest {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.requests)
}

// One connection: the request, the answer, then the tunnel.
func (self *testHttpConnectProxy) serve(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	request, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	read := &testHttpConnectRequest{
		method:        request.Method,
		target:        request.RequestURI,
		authorization: request.Header.Get("Proxy-Authorization"),
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.requests = append(self.requests, read)
	}()
	if self.requestRead != nil {
		close(self.requestRead)
	}
	if self.hold != nil {
		<-self.hold
	}
	status := self.answer(read)
	if status/100 != 2 {
		fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Length: 0\r\n\r\n", status, http.StatusText(status))
		return
	}
	backendConn, err := net.Dial("tcp4", self.backend)
	if err != nil {
		fmt.Fprintf(conn, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	self.track(backendConn)
	defer backendConn.Close()
	// the answer and any tail in one write, so a tail lands in the dial's own read
	answer := append([]byte("HTTP/1.1 200 Connection established\r\n\r\n"), self.tail...)
	if _, err := conn.Write(answer); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(backendConn, reader)
		backendConn.Close()
		done <- struct{}{}
	}()
	go func() {
		io.Copy(conn, backendConn)
		conn.Close()
		done <- struct{}{}
	}()
	<-done
	<-done
}

// Every proxy answer the tests use.
func testHttpConnectAccept(request *testHttpConnectRequest) int {
	return http.StatusOK
}

// A https server certified for each of serverNames by a private root, which
// answers GET /hello with its host and upgrades /ws to a websocket that
// echoes one message.
type testHttpConnectServer struct {
	server *httptest.Server
	tls    *ClientStrategySettings
}

func newTestHttpConnectServer(t *testing.T, serverName string) *testHttpConnectServer {
	t.Helper()
	rootCertPool, certificate := newTestTlsHelloCertificates(t, serverName)
	upgrader := websocket.Upgrader{}
	mux := http.NewServeMux()
	mux.HandleFunc("/hello", func(writer http.ResponseWriter, request *http.Request) {
		fmt.Fprintf(writer, "hello %s", request.Host)
	})
	mux.HandleFunc("/ws", func(writer http.ResponseWriter, request *http.Request) {
		ws, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		messageType, message, err := ws.ReadMessage()
		if err != nil {
			return
		}
		ws.WriteMessage(messageType, message)
	})
	server := httptest.NewUnstartedServer(mux)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server.Listener.Close()
	server.Listener = listener
	server.EnableHTTP2 = true
	server.TLS = &tls.Config{
		Certificates: []tls.Certificate{certificate},
		NextProtos:   []string{"h2", "http/1.1"},
	}
	server.StartTLS()
	t.Cleanup(server.Close)

	tlsConfig, err := DefaultTlsConfig()
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig.RootCAs = rootCertPool
	settings := DefaultClientStrategySettings()
	settings.TlsConfig = tlsConfig
	return &testHttpConnectServer{server: server, tls: settings}
}

// The server's listening address, which the proxy tunnels to.
func (self *testHttpConnectServer) address() string {
	return self.server.Listener.Addr().String()
}

// The server's port, which every dial names beside a synthetic host.
func (self *testHttpConnectServer) port() int {
	return self.server.Listener.Addr().(*net.TCPAddr).Port
}

// Strategy settings trusting the server's root, every dial through proxy.
func (self *testHttpConnectServer) strategySettings(proxySettings *ProxySettings) *ClientStrategySettings {
	settings := *self.tls
	settings.ConnectSettings.ProxySettings = proxySettings
	return &settings
}

// An api request and a platform transport's websocket dial each reach their
// host through the proxy, which is asked for the host's name, never an
// address: the proxy resolves, and the tls above keeps the name.
func TestHttpConnectProxyCarriesApiAndPlatformDials(t *testing.T) {
	server := newTestHttpConnectServer(t, testHttpConnectApiName)
	platformServer := newTestHttpConnectServer(t, testHttpConnectPlatformName)
	apiProxy := newTestHttpConnectProxy(t, server.address(), testHttpConnectAccept)
	platformProxy := newTestHttpConnectProxy(t, platformServer.address(), testHttpConnectAccept)

	ctx, cancel := context.WithTimeout(context.Background(), testHttpConnectTimeout)
	defer cancel()

	apiStrategy := NewClientStrategy(ctx, server.strategySettings(&ProxySettings{
		Address:  apiProxy.address(),
		Protocol: ProxyProtocolHttpConnect,
	}))
	defer apiStrategy.Close()
	apiAuthority := net.JoinHostPort(testHttpConnectApiName, fmt.Sprint(server.port()))
	body, err := HttpGetWithStrategyRaw(ctx, apiStrategy, "https://"+apiAuthority+"/hello", "")
	if err != nil {
		t.Fatalf("the api request through the proxy: %v", err)
	}
	if string(body) != "hello "+apiAuthority {
		t.Errorf("the api answered %q", body)
	}
	requests := apiProxy.readRequests()
	if len(requests) == 0 {
		t.Fatal("the api request did not cross the proxy")
	}
	for _, request := range requests {
		if request.method != http.MethodConnect || request.target != apiAuthority {
			t.Errorf("the proxy read %s %s, not CONNECT %s", request.method, request.target, apiAuthority)
		}
	}

	platformStrategy := NewClientStrategy(ctx, platformServer.strategySettings(&ProxySettings{
		Address:  platformProxy.address(),
		Protocol: ProxyProtocolHttpConnect,
	}))
	defer platformStrategy.Close()
	platformAuthority := net.JoinHostPort(testHttpConnectPlatformName, fmt.Sprint(platformServer.port()))
	// the platform transport's own dial (transport.go), H1+ off so the plain
	// websocket answers it
	conn, _, err := platformStrategy.H1DialContextWithDialer(ctx, "wss://"+platformAuthority+"/ws", http.Header{}, 64*1024, false, nil)
	if err != nil {
		t.Fatalf("the platform dial through the proxy: %v", err)
	}
	defer conn.Close()
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("through")); err != nil {
		t.Fatal(err)
	}
	_, echoed, err := conn.ReadMessage()
	if err != nil || string(echoed) != "through" {
		t.Fatalf("the websocket echoed %q, %v", echoed, err)
	}
	requests = platformProxy.readRequests()
	if len(requests) == 0 {
		t.Fatal("the platform dial did not cross the proxy")
	}
	for _, request := range requests {
		if request.method != http.MethodConnect || request.target != platformAuthority {
			t.Errorf("the proxy read %s %s, not CONNECT %s", request.method, request.target, platformAuthority)
		}
	}
}

// Auth goes to the proxy as Basic credentials, and to nobody else.
func TestHttpConnectProxyCarriesBasicCredentials(t *testing.T) {
	server := newTestHttpConnectServer(t, testHttpConnectApiName)
	connectProxy := newTestHttpConnectProxy(t, server.address(), testHttpConnectAccept)
	settings := server.strategySettings(&ProxySettings{
		Address:  connectProxy.address(),
		Auth:     &proxy.Auth{User: "synthetic-user", Password: "synthetic-password"},
		Protocol: ProxyProtocolHttpConnect,
	})

	ctx, cancel := context.WithTimeout(context.Background(), testHttpConnectTimeout)
	defer cancel()
	conn, err := settings.ConnectSettings.DialContext(ctx, "tcp", net.JoinHostPort(testHttpConnectApiName, "443"))
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	requests := connectProxy.readRequests()
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("synthetic-user:synthetic-password"))
	if len(requests) != 1 || requests[0].authorization != want {
		t.Errorf("the proxy read %+v", requests)
	}
}

// A 407 and any other refusal are dial errors that name the status and no
// target, proxy or credential.
func TestHttpConnectProxyRefusalsAreDialErrors(t *testing.T) {
	for _, status := range []int{http.StatusProxyAuthRequired, http.StatusForbidden, http.StatusBadGateway} {
		server := newTestHttpConnectServer(t, testHttpConnectApiName)
		connectProxy := newTestHttpConnectProxy(t, server.address(), func(request *testHttpConnectRequest) int {
			return status
		})
		settings := server.strategySettings(&ProxySettings{
			Address:  connectProxy.address(),
			Auth:     &proxy.Auth{User: "synthetic-user", Password: "synthetic-password"},
			Protocol: ProxyProtocolHttpConnect,
		})
		ctx, cancel := context.WithTimeout(context.Background(), testHttpConnectTimeout)
		conn, err := settings.ConnectSettings.DialContext(ctx, "tcp", net.JoinHostPort(testHttpConnectApiName, "443"))
		cancel()
		if err == nil {
			conn.Close()
			t.Fatalf("a %d dialed", status)
		}
		var proxyErr *HttpConnectProxyError
		if !errors.As(err, &proxyErr) || proxyErr.StatusCode != status {
			t.Fatalf("a %d answered %v", status, err)
		}
		if proxyErr.AuthenticationRequired() != (status == http.StatusProxyAuthRequired) {
			t.Errorf("a %d reads as authentication required: %v", status, proxyErr.AuthenticationRequired())
		}
		for _, leaked := range []string{testHttpConnectApiName, connectProxy.address(), "synthetic-user", "synthetic-password"} {
			if strings.Contains(err.Error(), leaked) {
				t.Errorf("a %d's error %q names %q", status, err, leaked)
			}
		}
	}
}

// A datagram dial refuses rather than go through or around the proxy, and
// the proxy sees nothing.
func TestHttpConnectProxyRefusesDatagrams(t *testing.T) {
	server := newTestHttpConnectServer(t, testHttpConnectApiName)
	connectProxy := newTestHttpConnectProxy(t, server.address(), testHttpConnectAccept)
	settings := server.strategySettings(&ProxySettings{
		Address:  connectProxy.address(),
		Protocol: ProxyProtocolHttpConnect,
	})
	ctx, cancel := context.WithTimeout(context.Background(), testHttpConnectTimeout)
	defer cancel()
	if conn, err := settings.ConnectSettings.DialContext(ctx, "udp", net.JoinHostPort(testHttpConnectApiName, "443")); err == nil {
		conn.Close()
		t.Fatal("a udp dial went through the CONNECT proxy")
	}
	if requests := connectProxy.readRequests(); len(requests) != 0 {
		t.Errorf("the proxy read %d requests", len(requests))
	}
}

// A cancel while the proxy holds its answer ends the dial with the cancel,
// and leaves nothing open.
func TestHttpConnectProxyCancelEndsTheExchange(t *testing.T) {
	server := newTestHttpConnectServer(t, testHttpConnectApiName)
	connectProxy := newTestHttpConnectProxy(t, server.address(), testHttpConnectAccept)
	connectProxy.requestRead = make(chan struct{})
	connectProxy.hold = make(chan struct{})
	defer close(connectProxy.hold)
	settings := server.strategySettings(&ProxySettings{
		Address:  connectProxy.address(),
		Protocol: ProxyProtocolHttpConnect,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type dialResult struct {
		conn net.Conn
		err  error
	}
	results := make(chan dialResult, 1)
	go func() {
		conn, err := settings.ConnectSettings.DialContext(ctx, "tcp", net.JoinHostPort(testHttpConnectApiName, "443"))
		results <- dialResult{conn: conn, err: err}
	}()
	select {
	case <-connectProxy.requestRead:
	case <-time.After(testHttpConnectTimeout):
		t.Fatal("the proxy never read the CONNECT")
	}
	cancel()
	select {
	case result := <-results:
		if result.err == nil {
			result.conn.Close()
			t.Fatal("a canceled exchange dialed")
		}
		if !errors.Is(result.err, context.Canceled) {
			t.Errorf("the dial answered %v, not the cancel", result.err)
		}
	case <-time.After(testHttpConnectTimeout):
		t.Fatal("the cancel did not end the exchange")
	}
}

// Bytes the proxy sends in the same write as its 200 are the tunnel's first,
// not lost to the response read.
func TestHttpConnectProxyKeepsBytesPastTheResponse(t *testing.T) {
	server := newTestHttpConnectServer(t, testHttpConnectApiName)
	connectProxy := newTestHttpConnectProxy(t, server.address(), testHttpConnectAccept)
	connectProxy.tail = []byte("early")
	settings := server.strategySettings(&ProxySettings{
		Address:  connectProxy.address(),
		Protocol: ProxyProtocolHttpConnect,
	})
	ctx, cancel := context.WithTimeout(context.Background(), testHttpConnectTimeout)
	defer cancel()
	conn, err := settings.ConnectSettings.DialContext(ctx, "tcp", net.JoinHostPort(testHttpConnectApiName, "443"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(testHttpConnectTimeout))
	early := make([]byte, len("early"))
	if _, err := io.ReadFull(conn, early); err != nil || string(early) != "early" {
		t.Errorf("the tunnel began %q, %v", early, err)
	}
}

// Off by default: connect's settings carry no proxy, and a ProxySettings
// written without a Protocol speaks SOCKS5 as before, through the exported
// dial and the settings' own.
func TestProxySettingsWithoutProtocolStaySocks5(t *testing.T) {
	if settings := DefaultClientStrategySettings(); settings.ProxySettings != nil {
		t.Fatal("connect's default settings carry a proxy")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	firstBytes := make(chan byte, 2)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			first := make([]byte, 1)
			if _, err := io.ReadFull(conn, first); err == nil {
				firstBytes <- first[0]
			}
			conn.Close()
		}
	}()
	proxySettings := &ProxySettings{Network: "tcp", Address: listener.Addr().String()}
	ctx, cancel := context.WithTimeout(context.Background(), testHttpConnectTimeout)
	defer cancel()

	dialContext := proxySettings.NewDialContext(ctx, &net.Dialer{})
	if conn, err := dialContext(ctx, "tcp", net.JoinHostPort(testHttpConnectApiName, "443")); err == nil {
		conn.Close()
		t.Fatal("a socks5 listener that closes at once carried a dial")
	}
	connectSettings := DefaultConnectSettings()
	connectSettings.ProxySettings = proxySettings
	if conn, err := connectSettings.DialContext(ctx, "tcp", net.JoinHostPort(testHttpConnectApiName, "443")); err == nil {
		conn.Close()
		t.Fatal("a socks5 listener that closes at once carried a settings dial")
	}
	for range 2 {
		select {
		case first := <-firstBytes:
			// RFC 1928's greeting starts with its version, 5; a CONNECT starts with "C"
			if first != 0x05 {
				t.Errorf("the proxy's first byte is %#x, not SOCKS5's version", first)
			}
		case <-time.After(testHttpConnectTimeout):
			t.Fatal("the proxy saw no dial")
		}
	}
}

// A protocol connect does not know refuses, rather than going direct.
func TestUnknownProxyProtocolRefuses(t *testing.T) {
	proxySettings := &ProxySettings{Network: "tcp", Address: "127.0.0.1:1", Protocol: "carrier-pigeon"}
	dialContext := proxySettings.NewDialContext(context.Background(), &net.Dialer{})
	if conn, err := dialContext(context.Background(), "tcp", net.JoinHostPort(testHttpConnectApiName, "443")); err == nil {
		conn.Close()
		t.Fatal("an unknown protocol dialed")
	}
}
