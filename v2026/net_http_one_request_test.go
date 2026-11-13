package connect

// A one-request client owns its sockets for the length of its call. Holding
// a canceled HTTP/2 stream's reset write keeps net/http's stream live past
// the call's return, where an idle sweep cannot see that it ended; every
// endpoint is a loopback test server.

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Holds every write of the test's client sockets made while held, as a full
// send buffer holds a socket write. Release lets them proceed.
type oneRequestTestWrites struct {
	stateLock sync.Mutex
	gate      chan struct{}
}

// Holds every later write until release or its socket's close.
func (self *oneRequestTestWrites) hold() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.gate = make(chan struct{})
}

// Lets every held and later write proceed.
func (self *oneRequestTestWrites) release() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.gate != nil {
		close(self.gate)
		self.gate = nil
	}
}

// A client socket whose writes follow the hold. Closing it ends a held write,
// as closing a socket ends a blocked send.
type oneRequestTestConn struct {
	net.Conn
	writes    *oneRequestTestWrites
	closed    chan struct{}
	closeOnce sync.Once
}

// Waits out a hold before writing.
func (self *oneRequestTestConn) Write(buffer []byte) (int, error) {
	gate := func() chan struct{} {
		self.writes.stateLock.Lock()
		defer self.writes.stateLock.Unlock()
		return self.writes.gate
	}()
	if gate != nil {
		select {
		case <-gate:
		case <-self.closed:
			return 0, net.ErrClosed
		}
	}
	return self.Conn.Write(buffer)
}

// Ends a held write, then closes the socket.
func (self *oneRequestTestConn) Close() error {
	self.closeOnce.Do(func() { close(self.closed) })
	return self.Conn.Close()
}

// An HTTP/2 server whose handler reads the whole request, reports it, and
// then answers nothing until the request is canceled. The returned channel
// closes when the server sees its connection closed.
func newOneRequestTestServer(t *testing.T) (*httptest.Server, <-chan struct{}, <-chan struct{}) {
	t.Helper()
	entered := make(chan struct{})
	var enteredOnce sync.Once
	closed := make(chan struct{})
	var closedOnce sync.Once
	var http2 atomic.Bool
	server := newFamilyHttptestUnstartedServer(t, 4, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		io.Copy(io.Discard, request.Body)
		http2.Store(request.ProtoMajor == 2)
		enteredOnce.Do(func() { close(entered) })
		<-request.Context().Done()
	}))
	server.EnableHTTP2 = true
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closedOnce.Do(func() { close(closed) })
		}
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	t.Cleanup(func() {
		if !http2.Load() {
			t.Error("the fixture request did not use HTTP/2")
		}
	})
	return server, entered, closed
}

// Connect settings that trust the test server and dial through held writes.
func newOneRequestTestConnectSettings(server *httptest.Server, writes *oneRequestTestWrites) *ConnectSettings {
	settings := DefaultConnectSettings()
	settings.TlsConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	settings.DialContextSettings = &DialContextSettings{
		DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			return &oneRequestTestConn{Conn: conn, writes: writes, closed: make(chan struct{})}, nil
		},
	}
	return settings
}

// Cancels the call once its request reached the handler, with the canceled
// stream's reset held, and requires the call's socket closed after the
// reset is let through. The stream is forgotten only after that write.
func assertOneRequestSocketCloses(t *testing.T, entered <-chan struct{}, closed <-chan struct{}, writes *oneRequestTestWrites, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the request did not reach the handler")
	}
	writes.hold()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the canceled call succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the canceled call did not return")
	}
	writes.release()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the call's socket outlived it")
	}
}

// A canceled streaming POST closes its own socket even when HTTP/2 forgets
// the stream only after the call returned.
func TestHttpPostStreamClosesItsSocketWhenItsStreamOutlivesTheCall(t *testing.T) {
	server, entered, closed := newOneRequestTestServer(t)
	writes := &oneRequestTestWrites{}
	settings := newOneRequestTestConnectSettings(server, writes)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	defer writes.release()
	done := make(chan error, 1)
	go func() {
		_, err := httpPostStreamRaw(ctx, settings, server.URL+"/stream", strings.NewReader("input"), "")
		done <- err
	}()
	assertOneRequestSocketCloses(t, entered, closed, writes, cancel, done)
}

// The extender probes' shape: a TLS dialer that registers its socket below
// TLS, as the extender dial does, under the same one-request owner.
func TestOneRequestHttpClientClosesTlsDialedSocketWhenItsStreamOutlivesTheCall(t *testing.T) {
	server, entered, closed := newOneRequestTestServer(t)
	writes := &oneRequestTestWrites{}
	settings := DefaultClientStrategySettings()
	settings.ConnectSettings = *newOneRequestTestConnectSettings(server, writes)
	client := &http.Client{Transport: &http.Transport{
		DialTLSContext:    newNormalDialTlsContext(settings, clientHttpNextProtos),
		ForceAttemptHTTP2: true,
		HTTP2:             nativeHttp2Config(&settings.ConnectSettings),
	}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	defer writes.release()
	done := make(chan error, 1)
	go func() {
		release := ownOneRequestHttpClient(client)
		defer release()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/hello", nil)
		if err != nil {
			done <- err
			return
		}
		response, err := client.Do(request)
		if err == nil {
			response.Body.Close()
		}
		done <- err
	}()
	assertOneRequestSocketCloses(t, entered, closed, writes, cancel, done)
}
