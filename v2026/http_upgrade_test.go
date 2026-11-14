package connect

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func h1UpgradeTestRequest() *http.Request {
	r := httptest.NewRequest(http.MethodGet, "https://connect.example/connect", nil)
	r.Header.Set("Connection", "keep-alive, UpGrAdE")
	r.Header.Set("Upgrade", H1FramerProtocol)
	return r
}

func TestValidateFramedUpgradeRequest(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*http.Request)
		status int
	}{
		{name: "valid"},
		{name: "wrong_method", mutate: func(r *http.Request) { r.Method = http.MethodPost }, status: 400},
		{name: "http10", mutate: func(r *http.Request) { r.ProtoMinor = 0 }, status: 400},
		{name: "http2", mutate: func(r *http.Request) { r.ProtoMajor = 2; r.ProtoMinor = 0 }, status: 400},
		{name: "missing_connection", mutate: func(r *http.Request) { r.Header.Del("Connection") }, status: 400},
		{name: "substring_connection", mutate: func(r *http.Request) { r.Header.Set("Connection", "not-upgrade") }, status: 400},
		{name: "missing_upgrade", mutate: func(r *http.Request) { r.Header.Del("Upgrade") }, status: 400},
		{name: "case_changed_protocol", mutate: func(r *http.Request) { r.Header.Set("Upgrade", "URNETWORK-framer/1") }, status: 400},
		{name: "different_version", mutate: func(r *http.Request) { r.Header.Set("Upgrade", "urnetwork-framer/2") }, status: 400},
		{name: "xl_is_not_compact", mutate: func(r *http.Request) { r.Header.Set("Upgrade", H1FramerXlProtocol) }, status: 400},
		{name: "comma_protocols", mutate: func(r *http.Request) { r.Header.Set("Upgrade", H1FramerProtocol+", websocket") }, status: 400},
		{name: "duplicate_protocols", mutate: func(r *http.Request) { r.Header.Add("Upgrade", H1FramerProtocol) }, status: 400},
		{name: "body", mutate: func(r *http.Request) { r.ContentLength = 1 }, status: 400},
		{name: "unknown_body_length", mutate: func(r *http.Request) { r.ContentLength = -1 }, status: 400},
		{name: "transfer_encoding", mutate: func(r *http.Request) { r.TransferEncoding = []string{"chunked"} }, status: 400},
		{name: "raw_transfer_encoding", mutate: func(r *http.Request) { r.Header.Set("Transfer-Encoding", "identity") }, status: 400},
		{name: "duplicate_content_length", mutate: func(r *http.Request) { r.Header["Content-Length"] = []string{"0", "0"} }, status: 400},
		{name: "expect", mutate: func(r *http.Request) { r.Header.Set("Expect", "100-continue") }, status: 400},
		{name: "trailers", mutate: func(r *http.Request) { r.Trailer = http.Header{"X-Trailer": []string{"x"}} }, status: 400},
		{name: "oversized_headers", mutate: func(r *http.Request) { r.Header.Set("X-Large", strings.Repeat("x", h1UpgradeMaxHeaderBytes)) }, status: 431},
		{name: "oversized_target", mutate: func(r *http.Request) { r.RequestURI = "/" + strings.Repeat("x", h1UpgradeMaxHeaderBytes) }, status: 431},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := h1UpgradeTestRequest()
			if test.mutate != nil {
				test.mutate(r)
			}
			err := ValidateFramedUpgradeRequest(r, H1FramerProtocol)
			if test.status == 0 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var upgradeErr *HTTPUpgradeError
			if !errors.As(err, &upgradeErr) || upgradeErr.StatusCode != test.status {
				t.Fatalf("validation = %v, want status %d", err, test.status)
			}
		})
	}
	r := h1UpgradeTestRequest()
	r.Header.Set("Upgrade", H1FramerXlProtocol)
	if err := ValidateFramedUpgradeRequest(r, H1FramerXlProtocol); err != nil {
		t.Fatalf("valid XL upgrade: %v", err)
	}
	if err := ValidateFramedUpgradeRequest(r, "arbitrary/1"); err == nil {
		t.Fatal("accepted unsupported local protocol")
	}
}

func TestIsFramedUpgradeRecognizesMalformedOffersForRejection(t *testing.T) {
	for _, offer := range []string{H1FramerProtocol, "URNETWORK-FRAMER/1", "websocket, " + H1FramerProtocol} {
		r := h1UpgradeTestRequest()
		r.Header.Set("Upgrade", offer)
		if !IsFramedUpgrade(r, H1FramerProtocol) {
			t.Fatalf("offer %q escaped custom protocol validation", offer)
		}
	}
	for _, offer := range []string{"urnetwork-framer/10", "not-" + H1FramerProtocol, H1FramerXlProtocol, "websocket"} {
		r := h1UpgradeTestRequest()
		r.Header.Set("Upgrade", offer)
		if IsFramedUpgrade(r, H1FramerProtocol) {
			t.Fatalf("unrelated offer %q matched compact protocol", offer)
		}
	}
}

// This deterministic connection returns 101 and following payload from one
// read, exposing loss of the HTTP parser's prefetched application bytes.
type h1UpgradeScriptConn struct {
	input       *bytes.Reader
	output      bytes.Buffer
	closed      atomic.Bool
	deadline    time.Time
	writeErr    error
	writeLimit  int
	readCalls   int
	readBytes   int
	writeCalls  int
	maxReadSize int
}

func newH1UpgradeScriptConn(input []byte) *h1UpgradeScriptConn {
	return &h1UpgradeScriptConn{input: bytes.NewReader(input), writeLimit: -1}
}

func (c *h1UpgradeScriptConn) Read(p []byte) (int, error) {
	if c.closed.Load() {
		return 0, net.ErrClosed
	}
	c.readCalls++
	c.maxReadSize = max(c.maxReadSize, len(p))
	n, err := c.input.Read(p)
	c.readBytes += n
	return n, err
}
func (c *h1UpgradeScriptConn) Write(p []byte) (int, error) {
	if c.closed.Load() {
		return 0, net.ErrClosed
	}
	c.writeCalls++
	if 0 <= c.writeLimit && c.writeLimit < len(p) {
		p = p[:c.writeLimit]
	}
	n, _ := c.output.Write(p)
	return n, c.writeErr
}
func (c *h1UpgradeScriptConn) Close() error                       { c.closed.Store(true); return nil }
func (c *h1UpgradeScriptConn) LocalAddr() net.Addr                { return &net.TCPAddr{} }
func (c *h1UpgradeScriptConn) RemoteAddr() net.Addr               { return &net.TCPAddr{} }
func (c *h1UpgradeScriptConn) SetDeadline(v time.Time) error      { c.deadline = v; return nil }
func (c *h1UpgradeScriptConn) SetReadDeadline(v time.Time) error  { c.deadline = v; return nil }
func (c *h1UpgradeScriptConn) SetWriteDeadline(v time.Time) error { c.deadline = v; return nil }

func h1UpgradeScriptDialer(c net.Conn, calls *atomic.Int32) *websocket.Dialer {
	return &websocket.Dialer{NetDialContext: func(context.Context, string, string) (net.Conn, error) {
		if calls != nil {
			calls.Add(1)
		}
		return c, nil
	}}
}

func TestDialFramedUpgradePreservesPrefetchedBytes(t *testing.T) {
	for _, protocol := range []string{H1FramerProtocol, H1FramerXlProtocol} {
		t.Run(protocol, func(t *testing.T) {
			payload := bytes.Repeat([]byte{0, 0xff, 0x5a, 0x83}, 20*1024)
			header := "HTTP/1.1 101 Switching Protocols\r\nConnection: keep-alive, Upgrade\r\nUpgrade: " + protocol + "\r\n\r\n"
			raw := newH1UpgradeScriptConn(append([]byte(header), payload...))
			conn, err := DialFramedUpgrade(context.Background(), "ws://connect.example/connect?mode=test", http.Header{"Authorization": []string{"Bearer synthetic-test"}}, h1UpgradeScriptDialer(raw, nil), protocol)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if !raw.deadline.IsZero() {
				t.Fatal("handshake deadline retained after successful upgrade")
			}
			if raw.readBytes <= len(header) {
				t.Fatal("fixture did not prefetch post-101 bytes")
			}
			got, err := io.ReadAll(conn)
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("post-101 bytes lost/truncated: got %d, want %d, err %v", len(got), len(payload), err)
			}
			if upgraded := conn.(*httpUpgradeConn); upgraded.reader != nil {
				t.Fatal("handshake buffer retained after prefetched bytes consumed")
			}
			r, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(raw.output.Bytes())))
			if err != nil {
				t.Fatal(err)
			}
			if r.RequestURI != "/connect?mode=test" || r.Header.Get("Authorization") != "Bearer synthetic-test" || r.Header.Get("Upgrade") != protocol {
				t.Fatalf("wrong HTTP upgrade request: %s / %s", r.RequestURI, r.Header.Get("Upgrade"))
			}
			if r.Header.Get("Sec-Websocket-Key") != "" || r.Header.Get("Sec-Websocket-Version") != "" {
				t.Fatal("custom upgrade unexpectedly emitted WebSocket negotiation")
			}
		})
	}
}

func TestDialFramedUpgradeResponseValidation(t *testing.T) {
	valid := "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + H1FramerProtocol + "\r\n"
	tests := []struct {
		name, response string
		fallback       bool
	}{
		{"normal_http", "HTTP/1.1 200 OK\r\nContent-Length: 1000000000\r\n\r\n", true},
		{"not_found", "HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\n\r\n", true},
		{"upgrade_required", "HTTP/1.1 426 Upgrade Required\r\nContent-Length: 0\r\n\r\n", true},
		{"unauthorized", "HTTP/1.1 401 Unauthorized\r\nContent-Length: 0\r\n\r\n", false},
		{"forbidden", "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n", false},
		{"redirect", "HTTP/1.1 307 Temporary Redirect\r\nLocation: https://other.example/credential-target\r\nContent-Length: 0\r\n\r\n", false},
		{"missing_upgrade", "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\n\r\n", true},
		{"missing_connection", "HTTP/1.1 101 Switching Protocols\r\nUpgrade: " + H1FramerProtocol + "\r\n\r\n", true},
		{"wrong_upgrade", strings.Replace(valid, H1FramerProtocol, H1FramerXlProtocol, 1) + "\r\n", true},
		{"case_changed_upgrade", strings.Replace(valid, H1FramerProtocol, "URNETWORK-FRAMER/1", 1) + "\r\n", true},
		{"multiple_upgrade", strings.Replace(valid, H1FramerProtocol, H1FramerProtocol+", websocket", 1) + "\r\n", true},
		{"duplicate_upgrade", valid + "Upgrade: " + H1FramerProtocol + "\r\n\r\n", true},
		{"http10", strings.Replace(valid, "HTTP/1.1", "HTTP/1.0", 1) + "\r\n", true},
		{"response_body", valid + "Content-Length: 12\r\n\r\nbody-is-not-a-frame", true},
		{"chunked_response", valid + "Transfer-Encoding: chunked\r\n\r\n", true},
		{"oversized_response", valid + "X-Large: " + strings.Repeat("x", h1UpgradeMaxHeaderBytes) + "\r\n\r\n", true},
		{"invalid_response", "not HTTP\r\n\r\n", true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := newH1UpgradeScriptConn([]byte(test.response))
			conn, err := DialFramedUpgrade(context.Background(), "ws://connect.example/", nil, h1UpgradeScriptDialer(raw, nil), H1FramerProtocol)
			if conn != nil {
				conn.Close()
				t.Fatal("accepted malformed/rejected upgrade response")
			}
			if err == nil || HTTPUpgradeAllowsFallback(err) != test.fallback {
				t.Fatalf("error = %v, allows fallback = %v, want %v", err, HTTPUpgradeAllowsFallback(err), test.fallback)
			}
			if !raw.closed.Load() {
				t.Fatal("failed handshake socket was not closed")
			}
			if raw.readBytes > h1UpgradeMaxHeaderBytes {
				t.Fatalf("response header limit exceeded: %d bytes read", raw.readBytes)
			}
			if strings.Contains(err.Error(), "credential-target") || strings.Contains(err.Error(), "body-is-not-a-frame") {
				t.Fatalf("error exposes peer-controlled content: %v", err)
			}
		})
	}
}

func TestDialFramedUpgradeRejectsInvalidInputsBeforeDial(t *testing.T) {
	tests := []struct {
		name, address, protocol string
		header                  http.Header
	}{
		{name: "userinfo", address: "wss://secret:password@connect.example/", protocol: H1FramerProtocol},
		{name: "fragment", address: "wss://connect.example/#secret", protocol: H1FramerProtocol},
		{name: "scheme", address: "ftp://connect.example/", protocol: H1FramerProtocol},
		{name: "no_host", address: "wss:///connect", protocol: H1FramerProtocol},
		{name: "protocol", address: "ws://connect.example/", protocol: "unknown/1"},
		{name: "headers", address: "ws://connect.example/", protocol: H1FramerProtocol, header: http.Header{"X-Large": []string{strings.Repeat("x", h1UpgradeMaxHeaderBytes)}}},
		{name: "target", address: "ws://connect.example/" + strings.Repeat("x", h1UpgradeMaxHeaderBytes), protocol: H1FramerProtocol},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var dials atomic.Int32
			raw := newH1UpgradeScriptConn(nil)
			conn, err := DialFramedUpgrade(context.Background(), test.address, test.header, h1UpgradeScriptDialer(raw, &dials), test.protocol)
			if conn != nil || err == nil || dials.Load() != 0 {
				t.Fatalf("invalid input dialed: conn %v, err %v, dials %d", conn, err, dials.Load())
			}
			if strings.Contains(err.Error(), "password") || strings.Contains(err.Error(), "secret") {
				t.Fatalf("error exposes URL credentials: %v", err)
			}
		})
	}
}

func TestDialFramedUpgradeCancellationClosesBlockedHandshake(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	requestSeen := make(chan struct{})
	serverClosed := make(chan error, 1)
	go func() {
		_, err := http.ReadRequest(bufio.NewReader(server))
		close(requestSeen)
		if err == nil {
			var one [1]byte
			_, err = server.Read(one[:])
		}
		serverClosed <- err
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := DialFramedUpgrade(ctx, "ws://connect.example/", nil, h1UpgradeScriptDialer(client, nil), H1FramerProtocol)
		done <- err
	}()
	select {
	case <-requestSeen:
	case <-time.After(3 * time.Second):
		t.Fatal("handshake request not written")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || HTTPUpgradeAllowsFallback(err) {
			t.Fatalf("cancellation = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not unblock handshake")
	}
	select {
	case err := <-serverClosed:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("peer socket close = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled handshake left peer socket open")
	}
}

func TestAcceptFramedUpgradePreservesServerPrefetch(t *testing.T) {
	payload := []byte{0, 3, 0, 0, 'a', 'b', 'c'}
	result := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := AcceptFramedUpgrade(w, r, H1FramerProtocol, time.Second)
		if err != nil {
			result <- err
			return
		}
		defer conn.Close()
		got := make([]byte, len(payload))
		_, err = io.ReadFull(conn, got)
		if err == nil && !bytes.Equal(got, payload) {
			err = fmt.Errorf("lost server-prefetched bytes: %x", got)
		}
		result <- err
	}))
	defer server.Close()
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	request := "GET / HTTP/1.1\r\nHost: test\r\nConnection: Upgrade\r\nUpgrade: " + H1FramerProtocol + "\r\n\r\n"
	if _, err := conn.Write(append([]byte(request), payload...)); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil || response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("server handshake: %v, %v", response, err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server lost prefetched first frame")
	}
}

func resetH1UpgradeTestState(t *testing.T) {
	t.Helper()
	wasDisabled := h1PlusDisabled.Load()
	SetH1PlusDisabled(false)
	framedUpgradeCache.Lock()
	previous := framedUpgradeCache.until
	framedUpgradeCache.until = map[string]time.Time{}
	framedUpgradeCache.Unlock()
	t.Cleanup(func() {
		SetH1PlusDisabled(wasDisabled)
		framedUpgradeCache.Lock()
		framedUpgradeCache.until = previous
		framedUpgradeCache.Unlock()
	})
}

func TestFramedUpgradeCapabilityCache(t *testing.T) {
	resetH1UpgradeTestState(t)
	const address = "wss://connect.example:443/endpoint"
	if !FramedUpgradePermitted(address, H1FramerProtocol) {
		t.Fatal("new endpoint unexpectedly suppressed")
	}
	for _, err := range []error{
		context.Canceled, io.EOF, &tls.CertificateVerificationError{Err: errors.New("synthetic")},
		&HTTPUpgradeError{StatusCode: 401, Terminal: true},
		&HTTPUpgradeError{Reason: "response-io"},
		&HTTPUpgradeError{Reason: "rejected", StatusCode: 429},
		&HTTPUpgradeError{Reason: "rejected", StatusCode: 503},
	} {
		RecordFramedUpgradeFailure(address, H1FramerProtocol, err)
		if !FramedUpgradePermitted(address, H1FramerProtocol) {
			t.Fatalf("security/network/cancellation failure cached: %v", err)
		}
	}
	RecordFramedUpgradeFailure(address, H1FramerProtocol, &HTTPUpgradeError{StatusCode: 426, Reason: "rejected"})
	if FramedUpgradePermitted(address, H1FramerProtocol) || FramedUpgradePermitted("wss://CONNECT.EXAMPLE:443/another-path", H1FramerProtocol) {
		t.Fatal("old origin was reprobed")
	}
	if !FramedUpgradePermitted(address, H1FramerXlProtocol) || !FramedUpgradePermitted("wss://other.example:443/endpoint", H1FramerProtocol) {
		t.Fatal("capability miss escaped protocol/origin scope")
	}
	framedUpgradeCache.Lock()
	framedUpgradeCache.until[framedUpgradeCacheKey(address, H1FramerProtocol)] = time.Now().Add(-time.Second)
	framedUpgradeCache.Unlock()
	if !FramedUpgradePermitted(address, H1FramerProtocol) {
		t.Fatal("expired capability miss still suppresses probe")
	}
	for i := range 600 {
		RecordFramedUpgradeFailure(fmt.Sprintf("wss://endpoint-%d.example/", i), H1FramerProtocol, &HTTPUpgradeError{Reason: "rejected", StatusCode: 426})
	}
	framedUpgradeCache.Lock()
	count := len(framedUpgradeCache.until)
	framedUpgradeCache.Unlock()
	if count == 0 || count > 256 {
		t.Fatalf("unbounded capability cache: %d", count)
	}
}

func TestH1PlusProcessKillSwitch(t *testing.T) {
	resetH1UpgradeTestState(t)
	SetH1PlusDisabled(true)
	if H1PlusAvailable() || FramedUpgradePermitted("ws://connect.example/", H1FramerProtocol) {
		t.Fatal("process kill switch permits framed upgrade")
	}
	var calls atomic.Int32
	_, err := DialFramedUpgrade(context.Background(), "ws://connect.example/", nil, h1UpgradeScriptDialer(newH1UpgradeScriptConn(nil), &calls), H1FramerProtocol)
	if err == nil || calls.Load() != 0 {
		t.Fatalf("disabled attempt dialed: err %v, calls %d", err, calls.Load())
	}
}

// Track physical peers so fallback cannot be satisfied by reinterpreting the
// failed custom connection as WebSocket.
type h1UpgradeObservedRequests struct {
	sync.Mutex
	upgrades []string
	peers    []string
}

func (o *h1UpgradeObservedRequests) record(r *http.Request) {
	o.Lock()
	defer o.Unlock()
	o.upgrades = append(o.upgrades, r.Header.Get("Upgrade"))
	o.peers = append(o.peers, r.RemoteAddr)
}

func h1UpgradeTestEcho(t *testing.T, conn H1MessageConn) {
	t.Helper()
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetWriteDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{0x17, 0x81}, 600)
	if err := conn.WriteMessage(websocket.BinaryMessage, payload); err != nil {
		t.Fatal(err)
	}
	kind, got, err := conn.ReadMessage()
	if err != nil || kind != websocket.BinaryMessage || !bytes.Equal(got, payload) {
		t.Fatalf("carrier echo failed: kind=%d bytes=%d err=%v", kind, len(got), err)
	}
}

func h1UpgradeTestEchoServer(protocol string, observation *h1UpgradeObservedRequests, customStatus int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observation.record(r)
		// This is a synthetic prerequisite gate. Actual JWT/revocation and
		// signed proxy-id admission belong to endpoint integration tests.
		if r.Header.Get("Authorization") != "Bearer synthetic-test" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var conn H1MessageConn
		var err error
		if r.Header.Get("Upgrade") == protocol {
			if customStatus == 0 {
				raw, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					raw.Close()
				}
				return
			}
			if customStatus != 101 {
				http.Error(w, "unsupported", customStatus)
				return
			}
			var raw net.Conn
			raw, err = AcceptFramedUpgrade(w, r, protocol, time.Second)
			if err == nil {
				conn, err = NewFramedMessageConn(raw, protocol, 65535, nil)
			}
		} else {
			upgrader := websocket.Upgrader{}
			conn, err = upgrader.Upgrade(w, r, nil)
		}
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		_ = conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
		for {
			kind, message, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(kind, message); err != nil {
				return
			}
		}
	})
}

func TestDialH1MessagesFreshFallbackAndCachedUnsupportedOrigin(t *testing.T) {
	resetH1UpgradeTestState(t)
	for _, protocol := range []string{H1FramerProtocol, H1FramerXlProtocol} {
		t.Run(protocol, func(t *testing.T) {
			observation := &h1UpgradeObservedRequests{}
			server := httptest.NewServer(h1UpgradeTestEchoServer(protocol, observation, http.StatusUpgradeRequired))
			defer server.Close()
			address := "ws" + strings.TrimPrefix(server.URL, "http")
			header := http.Header{"Authorization": []string{"Bearer synthetic-test"}}
			dialer := &websocket.Dialer{HandshakeTimeout: 3 * time.Second}
			stats := &H1PlusStats{}
			for range 2 {
				conn, err := DialH1Messages(context.Background(), address, header, dialer, protocol, 65535, true, stats)
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := conn.(*websocket.Conn); !ok {
					conn.Close()
					t.Fatal("unsupported endpoint did not use standard WebSocket fallback")
				}
				h1UpgradeTestEcho(t, conn)
			}
			observation.Lock()
			defer observation.Unlock()
			if fmt.Sprint(observation.upgrades) != fmt.Sprint([]string{protocol, "websocket", "websocket"}) {
				t.Fatalf("unexpected attempt/cache sequence: %v", observation.upgrades)
			}
			if observation.peers[0] == observation.peers[1] {
				t.Fatal("fallback reused rejected custom TCP connection")
			}
			if got := stats.Snapshot(); got.Attempts != 1 || got.Fallbacks != 1 || got.Accepted != 0 || got.AuthFailures != 0 {
				t.Fatalf("fallback counters: %+v", got)
			}
		})
	}
}

func TestDialH1MessagesTransientFailureDoesNotSuppressLaterProbe(t *testing.T) {
	resetH1UpgradeTestState(t)
	for _, status := range []int{0, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			observation := &h1UpgradeObservedRequests{}
			server := httptest.NewServer(h1UpgradeTestEchoServer(H1FramerProtocol, observation, status))
			defer server.Close()
			address := "ws" + strings.TrimPrefix(server.URL, "http")
			for range 2 {
				conn, err := DialH1Messages(context.Background(), address, http.Header{"Authorization": []string{"Bearer synthetic-test"}}, &websocket.Dialer{HandshakeTimeout: 3 * time.Second}, H1FramerProtocol, 65535, true, nil)
				if err != nil {
					t.Fatal(err)
				}
				h1UpgradeTestEcho(t, conn)
				if !FramedUpgradePermitted(address, H1FramerProtocol) {
					t.Fatal("transient response/socket failure cached as unsupported capability")
				}
			}
			observation.Lock()
			defer observation.Unlock()
			if fmt.Sprint(observation.upgrades) != fmt.Sprint([]string{H1FramerProtocol, "websocket", H1FramerProtocol, "websocket"}) {
				t.Fatalf("wrong transient-failure retry sequence: %v", observation.upgrades)
			}
			if observation.peers[0] == observation.peers[1] || observation.peers[2] == observation.peers[3] {
				t.Fatal("transient fallback reused failed connection")
			}
		})
	}
}

func TestDialH1MessagesSelectionAndCallerKillSwitch(t *testing.T) {
	resetH1UpgradeTestState(t)
	for _, test := range []struct {
		name, protocol    string
		enabled, disabled bool
		framed            bool
	}{
		{"compact", H1FramerProtocol, true, false, true},
		{"xl", H1FramerXlProtocol, true, false, true},
		{"caller_disabled", H1FramerProtocol, false, false, false},
		{"process_disabled", H1FramerProtocol, true, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			SetH1PlusDisabled(test.disabled)
			defer SetH1PlusDisabled(false)
			observation := &h1UpgradeObservedRequests{}
			server := httptest.NewServer(h1UpgradeTestEchoServer(test.protocol, observation, 101))
			defer server.Close()
			stats := &H1PlusStats{}
			conn, err := DialH1Messages(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"), http.Header{"Authorization": []string{"Bearer synthetic-test"}}, &websocket.Dialer{HandshakeTimeout: 3 * time.Second}, test.protocol, 65535, test.enabled, stats)
			if err != nil {
				t.Fatal(err)
			}
			framed, ok := conn.(*FramedMessageConn)
			if ok != test.framed {
				conn.Close()
				t.Fatalf("selected framed=%v, want %v", ok, test.framed)
			}
			if ok && ((framed.xl != nil) != (test.protocol == H1FramerXlProtocol)) {
				conn.Close()
				t.Fatal("negotiated token selected incorrect parser")
			}
			h1UpgradeTestEcho(t, conn)
			observation.Lock()
			defer observation.Unlock()
			want := "websocket"
			if test.framed {
				want = test.protocol
			}
			if len(observation.upgrades) != 1 || observation.upgrades[0] != want {
				t.Fatalf("unexpected attempt sequence: %v", observation.upgrades)
			}
			if !test.framed && stats.Snapshot().Attempts != 0 {
				t.Fatal("disabled custom attempt counted/dialed")
			}
		})
	}
}

func TestDialH1MessagesAuthorizationFailureDoesNotFallbackOrCache(t *testing.T) {
	resetH1UpgradeTestState(t)
	observation := &h1UpgradeObservedRequests{}
	server := httptest.NewServer(h1UpgradeTestEchoServer(H1FramerProtocol, observation, 101))
	defer server.Close()
	address := "ws" + strings.TrimPrefix(server.URL, "http")
	stats := &H1PlusStats{}
	conn, err := DialH1Messages(context.Background(), address, http.Header{"Authorization": []string{"Bearer wrong"}}, &websocket.Dialer{HandshakeTimeout: 3 * time.Second}, H1FramerProtocol, 65535, true, stats)
	if conn != nil || err == nil || HTTPUpgradeAllowsFallback(err) {
		t.Fatalf("authorization rejection: conn=%v err=%v", conn, err)
	}
	observation.Lock()
	defer observation.Unlock()
	if len(observation.upgrades) != 1 || observation.upgrades[0] != H1FramerProtocol || !FramedUpgradePermitted(address, H1FramerProtocol) {
		t.Fatal("authorization failure triggered fallback or poisoned capability cache")
	}
	if got := stats.Snapshot(); got.AuthFailures != 1 || got.Fallbacks != 0 || got.Accepted != 0 {
		t.Fatalf("authorization counters: %+v", got)
	}
}

func TestDialH1MessagesDoesNotReplayAfterSuccessfulUpgrade(t *testing.T) {
	resetH1UpgradeTestState(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		conn, err := AcceptFramedUpgrade(w, r, H1FramerProtocol, time.Second)
		if err != nil {
			return
		}
		defer conn.Close()
		// A confirmed carrier receives a malformed/truncated application
		// frame. Its failure belongs to the stream, not capability fallback.
		_, _ = conn.Write([]byte{0, 5, 0, 0, 'x'})
	}))
	defer server.Close()
	address := "ws" + strings.TrimPrefix(server.URL, "http")
	stats := &H1PlusStats{}
	conn, err := DialH1Messages(context.Background(), address, nil, &websocket.Dialer{HandshakeTimeout: 3 * time.Second}, H1FramerProtocol, 65535, true, stats)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, message, err := conn.ReadMessage()
	if !errors.Is(err, io.ErrUnexpectedEOF) || message != nil {
		t.Fatalf("established stream failure = %v, body=%d", err, len(message))
	}
	if requests.Load() != 1 || stats.Snapshot().Fallbacks != 0 || !FramedUpgradePermitted(address, H1FramerProtocol) {
		t.Fatal("failed established stream triggered downgrade or poisoned capability")
	}
}

func TestDialH1MessagesTLSValidationAndHTTP1ALPN(t *testing.T) {
	resetH1UpgradeTestState(t)
	observation := &h1UpgradeObservedRequests{}
	server := httptest.NewUnstartedServer(h1UpgradeTestEchoServer(H1FramerProtocol, observation, 101))
	server.EnableHTTP2 = true
	server.TLS = &tls.Config{NextProtos: []string{"h2", "http/1.1"}}
	server.StartTLS()
	defer server.Close()
	address := "wss" + strings.TrimPrefix(server.URL, "https")
	header := http.Header{"Authorization": []string{"Bearer synthetic-test"}}
	var dials atomic.Int32
	dialer := &websocket.Dialer{HandshakeTimeout: 3 * time.Second, NetDialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}}
	conn, err := DialH1Messages(context.Background(), address, header, dialer, H1FramerProtocol, 65535, true, nil)
	if conn != nil || err == nil || HTTPUpgradeAllowsFallback(err) || dials.Load() != 1 {
		t.Fatalf("certificate failure downgraded: conn=%v err=%v dials=%d", conn, err, dials.Load())
	}
	if !FramedUpgradePermitted(address, H1FramerProtocol) {
		t.Fatal("certificate failure poisoned capability cache")
	}
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	dialer.TLSClientConfig = &tls.Config{RootCAs: roots, NextProtos: []string{"h2", "http/1.1"}}
	conn, err = DialH1Messages(context.Background(), address, header, dialer, H1FramerProtocol, 65535, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	upgraded := conn.UnderlyingConn().(*httpUpgradeConn)
	state := upgraded.Conn.(*tls.Conn).ConnectionState()
	if state.NegotiatedProtocol != "http/1.1" {
		conn.Close()
		t.Fatalf("H1 selected ALPN %q", state.NegotiatedProtocol)
	}
	if dialer.TLSClientConfig.NextProtos[0] != "h2" {
		conn.Close()
		t.Fatal("H1 dial modified shared TLS configuration")
	}
	h1UpgradeTestEcho(t, conn)
}
