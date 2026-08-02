package connect

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	quic "github.com/quic-go/quic-go"
)

// closeInterruptWriteConn models the state observed on iOS after a path had
// gone stale: the WebSocket's next TCP write entered the kernel and remained
// blocked until the connection itself was closed. Context cancellation alone
// cannot interrupt net.Conn.Write.
type closeInterruptWriteConn struct {
	net.Conn

	blockWrite atomic.Bool
	writeOnce  sync.Once
	closeOnce  sync.Once

	writeStarted chan struct{}
	closed       chan struct{}
}

func newCloseInterruptWriteConn(conn net.Conn) *closeInterruptWriteConn {
	return &closeInterruptWriteConn{
		Conn:         conn,
		writeStarted: make(chan struct{}),
		closed:       make(chan struct{}),
	}
}

func (self *closeInterruptWriteConn) Write(buffer []byte) (int, error) {
	if !self.blockWrite.Load() {
		return self.Conn.Write(buffer)
	}
	self.writeOnce.Do(func() {
		close(self.writeStarted)
	})
	<-self.closed
	return 0, net.ErrClosed
}

func (self *closeInterruptWriteConn) Close() error {
	var err error
	self.closeOnce.Do(func() {
		close(self.closed)
		err = self.Conn.Close()
	})
	return err
}

// The platform transport had no test coverage at all: nothing constructed a
// PlatformTransport, so runH1, the mode election loop and the inactive-drain
// watchdog were never exercised. That is how a Monitor shipped with zero
// notifiers — the mode election never ran once in the product's life, and the
// live transport armed a 30s self-kill timer it could never cancel, surviving
// only because server pings happened to arrive faster than the timeout.
//
// These tests drive the real runH1 against a real websocket server. The default
// settings use V2H1Auth, which authenticates with request headers and performs no
// in-band handshake, so an ordinary websocket upgrade is all the platform side
// needs to provide.

// testingPlatformServer is a websocket server standing in for the platform. It
// accepts connections and then stays silent: it never sends a frame. Silence is
// deliberate — an idle inbound connection is exactly the condition the
// inactive-drain watchdog reacts to, and in production it is inbound server pings
// that were accidentally keeping the watchdog at bay.
type testingPlatformServer struct {
	server *httptest.Server
	url    string

	connectCount  atomic.Int64
	emptyMessages atomic.Int64
	rejecting     atomic.Bool

	stateLock sync.Mutex
	conns     []*websocket.Conn
}

func newTestingPlatformServer(t *testing.T) *testingPlatformServer {
	t.Helper()
	platform := &testingPlatformServer{}
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	platform.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if platform.rejecting.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		platform.connectCount.Add(1)
		func() {
			platform.stateLock.Lock()
			defer platform.stateLock.Unlock()
			platform.conns = append(platform.conns, ws)
		}()
		// hold the connection open, discarding whatever the client sends
		// (including its keepalive pings, which the client writes directly and
		// never counts). send nothing back
		for {
			_, message, err := ws.ReadMessage()
			if err != nil {
				ws.Close()
				return
			}
			if len(message) == 0 {
				platform.emptyMessages.Add(1)
			}
		}
	}))
	platform.url = "ws" + strings.TrimPrefix(platform.server.URL, "http")
	t.Cleanup(func() {
		// close the live connections first so the handlers return; httptest
		// Close blocks until every outstanding request completes
		platform.closeConns()
		platform.server.Close()
	})
	return platform
}

func (self *testingPlatformServer) closeConns() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, ws := range self.conns {
		ws.Close()
	}
	self.conns = nil
}

// down stops the platform from accepting, and drops the live connections, so the
// transport disconnects and cannot reconnect.
func (self *testingPlatformServer) down() {
	self.rejecting.Store(true)
	self.closeConns()
}

func testingPlatformTransportSettings() *PlatformTransportSettings {
	settings := DefaultPlatformTransportSettings()
	// short enough to exercise the inactive-drain watchdog inside a test, long
	// enough that the election (which is immediate) always precedes it
	settings.InactiveDrainTimeout = 500 * time.Millisecond
	settings.ReconnectTimeout = 50 * time.Millisecond
	// keep the client's own keepalive out of the way; it is written directly to
	// the socket and is not counted as activity either way
	settings.PingTimeout = 30 * time.Second
	return settings
}

func testingPlatformTransport(
	t *testing.T,
	ctx context.Context,
	platformUrl string,
	settings *PlatformTransportSettings,
) *PlatformTransport {
	t.Helper()
	transport := NewPlatformTransportWithTargetMode(
		ctx,
		NewClientStrategyWithDefaults(ctx),
		NewRouteManager(ctx, "test"),
		platformUrl,
		&ClientAuth{
			// the platform side does not verify; ClientId() failing to parse is
			// tolerated by runH1 (it only names log lines)
			ByJwt:      "testing",
			InstanceId: NewId(),
			AppVersion: "testing",
		},
		TransportModeH1,
		settings,
	)
	t.Cleanup(transport.Close)
	return transport
}

func testingWaitForActiveMode(transport *PlatformTransport, want TransportMode, timeout time.Duration) bool {
	return waitForCondition(timeout, func() bool {
		mode, _ := transport.activeMode()
		return mode == want
	})
}

// TestPlatformTransportConnectsAndElects drives the real transport against a real
// websocket platform: it must connect and the election must make the connected
// mode active.
//
// This is the regression test for the mode monitor having no producer.
// setModeAvailable mutated availableModes and notified nobody, so the election
// loop — which subscribes before it reads — parked on its very first iteration
// and the active mode stayed TransportModeNone for the entire process lifetime.
func TestPlatformTransportConnectsAndElects(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	platform := newTestingPlatformServer(t)
	transport := testingPlatformTransport(t, ctx, platform.url, testingPlatformTransportSettings())

	if !waitForCondition(15*time.Second, func() bool {
		return 0 < platform.connectCount.Load()
	}) {
		t.Fatal("the transport never connected to the platform")
	}

	if !testingWaitForActiveMode(transport, TransportModeH1, 15*time.Second) {
		mode, _ := transport.activeMode()
		t.Fatalf("active mode = %q, want h1: the connected transport was never elected", mode)
	}
}

// TestPlatformTransportActiveModeIsNotDrained pins the watchdog's semantics: the
// transport that IS the active mode must never be drained.
//
// The watchdog tears down a transport that is NOT the active mode after
// InactiveDrainTimeout of no traffic. Because the elected mode was never
// published, the live transport read TransportModeNone forever, concluded it was
// inactive, and armed that kill timer against itself, with no way to cancel it.
//
// Note on real-world impact, so this test is not read as more than it is: under
// the default settings the mis-arming was REDUNDANT, not fatal. InactiveDrainTimeout
// and ReadTimeout are both 30s, and the drain's condition (no inbound AND no
// counted outbound) is a strict subset of the read deadline's (no inbound), which
// the reader resets before every read. So the read deadline always tore down an
// idle connection first, and h1's connection lifecycle was unchanged in practice.
// What the bug really broke is the watchdog's ability to tell active from
// inactive at all — so it could never shed a transport superseded by a better
// mode, which is its actual purpose and which matters the moment h3 is re-enabled.
//
// This test shortens InactiveDrainTimeout well below ReadTimeout precisely to
// isolate the watchdog from the read deadline, so it observes the drain decision
// on its own rather than whichever timer happens to fire first.
func TestPlatformTransportActiveModeIsNotDrained(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	platform := newTestingPlatformServer(t)
	settings := testingPlatformTransportSettings()
	transport := testingPlatformTransport(t, ctx, platform.url, settings)

	// wait for the connection, deliberately NOT for the election: the drain is a
	// property of the live socket, so this measures the production symptom (a
	// transport tearing its own connection down while idle) rather than depending
	// on how the elected mode is published
	if !waitForCondition(15*time.Second, func() bool {
		return 0 < platform.connectCount.Load()
	}) {
		t.Fatal("the transport never connected to the platform")
	}
	connectCount := platform.connectCount.Load()

	// idle well past the drain timeout. a transport that believes it is inactive
	// cancels its own connection here, and reconnects, over and over
	select {
	case <-time.After(5 * settings.InactiveDrainTimeout):
	}

	if reconnects := platform.connectCount.Load() - connectCount; 0 < reconnects {
		t.Fatalf(
			"the transport reconnected %d times while idle: it drained itself, believing it was not the active mode",
			reconnects,
		)
	}
	// it stayed up because it knows it is the active transport, so the watchdog
	// sits on its benign branch instead of arming the kill timer
	if mode, _ := transport.activeMode(); mode != TransportModeH1 {
		t.Fatalf("active mode = %q after idling, want h1", mode)
	}
}

// TestPlatformTransportSendsRepeatedIdleKeepalives verifies both the initial
// reusable writer timer and its reset after firing.
func TestPlatformTransportSendsRepeatedIdleKeepalives(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	platform := newTestingPlatformServer(t)
	settings := testingPlatformTransportSettings()
	settings.PingTimeout = 20 * time.Millisecond
	transport := testingPlatformTransport(t, ctx, platform.url, settings)

	if !testingWaitForActiveMode(transport, TransportModeH1, 15*time.Second) {
		t.Fatal("the transport was never elected")
	}
	if !waitForCondition(2*time.Second, func() bool {
		return 2 <= platform.emptyMessages.Load()
	}) {
		t.Fatalf("idle keepalive count = %d, want at least 2", platform.emptyMessages.Load())
	}
}

// TestPlatformTransportModeFallsBackOnDisconnect: when the last available mode
// drops, the active mode must return to TransportModeNone.
//
// The election only ever called setActiveMode from inside "some mode is
// available", and its fallback lived in an else on `0 < len(orderedModes)` —
// unreachable, because orderedModes is the key set of a constant map. So a
// disconnected transport left the active mode pinned to its stale value.
func TestPlatformTransportModeFallsBackOnDisconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	platform := newTestingPlatformServer(t)
	transport := testingPlatformTransport(t, ctx, platform.url, testingPlatformTransportSettings())

	if !testingWaitForActiveMode(transport, TransportModeH1, 15*time.Second) {
		t.Fatal("the transport was never elected")
	}

	// the platform goes away: the transport disconnects and cannot reconnect
	platform.down()

	if !testingWaitForActiveMode(transport, TransportModeNone, 15*time.Second) {
		mode, _ := transport.activeMode()
		t.Fatalf("active mode = %q after the transport disconnected, want none", mode)
	}
}

// TestPlatformTransportReconnects: after the platform drops a connection the
// transport reconnects and is elected again, so a transient disconnect does not
// strand the active mode.
// TestPlatformTransportNetworkChangeKick pins the network-change path: a
// NetworkChanged broadcast closes the live connection and the transport
// re-dials immediately (the host's path-update signal, not a server drop).
func TestPlatformTransportNetworkChangeKick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	platform := newTestingPlatformServer(t)
	transport := testingPlatformTransport(t, ctx, platform.url, testingPlatformTransportSettings())

	if !testingWaitForActiveMode(transport, TransportModeH1, 15*time.Second) {
		t.Fatal("the transport was never elected")
	}
	connectCount := platform.connectCount.Load()

	// the host reports a network path change
	NetworkChanged()

	if !waitForCondition(15*time.Second, func() bool {
		return connectCount < platform.connectCount.Load()
	}) {
		t.Fatal("the transport did not re-dial after a network change")
	}
	if !testingWaitForActiveMode(transport, TransportModeH1, 15*time.Second) {
		mode, _ := transport.activeMode()
		t.Fatalf("active mode = %q after network change, want h1", mode)
	}

	// closing the transport unsubscribes it: a later broadcast must not panic
	// or kick a dead transport
	transport.Close()
	NetworkChanged()
}

func TestPlatformTransportReconnects(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	platform := newTestingPlatformServer(t)
	transport := testingPlatformTransport(t, ctx, platform.url, testingPlatformTransportSettings())

	if !testingWaitForActiveMode(transport, TransportModeH1, 15*time.Second) {
		t.Fatal("the transport was never elected")
	}
	connectCount := platform.connectCount.Load()

	// drop the live connection, leaving the platform accepting
	platform.closeConns()

	if !waitForCondition(15*time.Second, func() bool {
		return connectCount < platform.connectCount.Load()
	}) {
		t.Fatal("the transport did not reconnect after the platform dropped it")
	}
	if !testingWaitForActiveMode(transport, TransportModeH1, 15*time.Second) {
		mode, _ := transport.activeMode()
		t.Fatalf("active mode = %q after reconnect, want h1", mode)
	}
}

// TestPlatformTransportCloseInterruptsBlockedH1Write is the regression for a
// teardown dependency inversion found in an on-device iOS trace. The logical
// client was removed at 11:42:07, but its old TCP write did not return until its
// deadline at 11:42:16. Teardown removed routes and then joined the writer while
// the deferred socket Close—which was the only operation able to wake that
// writer—could not run until after the join.
func TestPlatformTransportCloseInterruptsBlockedH1Write(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	platform := newTestingPlatformServer(t)
	wrappedConn := make(chan *closeInterruptWriteConn, 1)
	strategySettings := DefaultClientStrategySettings()
	strategySettings.EnableResilient = false
	strategySettings.ParallelBlockSize = 1
	strategySettings.MinNextConnectDelay = 0
	strategySettings.MaxNextConnectDelay = 0
	netDialer := &net.Dialer{}
	strategySettings.DialContextSettings = &DialContextSettings{
		DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			conn, err := netDialer.DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			blocking := newCloseInterruptWriteConn(conn)
			select {
			case wrappedConn <- blocking:
			default:
			}
			return blocking, nil
		},
	}
	strategy := NewClientStrategy(ctx, strategySettings)
	routeManager := NewRouteManager(ctx, "test")
	settings := testingPlatformTransportSettings()
	settings.WriteTimeout = 30 * time.Second
	transport := NewPlatformTransportWithTargetMode(
		ctx,
		strategy,
		routeManager,
		platform.url,
		&ClientAuth{
			ByJwt:      "testing",
			InstanceId: NewId(),
			AppVersion: "testing",
		},
		TransportModeH1,
		settings,
	)
	t.Cleanup(transport.Close)

	if !testingWaitForActiveMode(transport, TransportModeH1, 15*time.Second) {
		t.Fatal("the transport was never elected")
	}
	var blocking *closeInterruptWriteConn
	select {
	case blocking = <-wrappedConn:
	case <-time.After(time.Second):
		t.Fatal("the websocket did not expose its underlying connection")
	}
	// Ensure even the pre-fix path can be released after an assertion, rather
	// than leaving the test process parked until the deliberately long deadline.
	defer blocking.Close()

	blocking.blockWrite.Store(true)
	writer := routeManager.OpenMultiRouteWriter(DestinationId(NewId()))
	defer routeManager.CloseMultiRouteWriter(writer)
	message := MessagePoolGet(32)
	if err := writer.Write(ctx, message, time.Second); err != nil {
		MessagePoolReturn(message)
		t.Fatalf("route write failed: %v", err)
	}
	select {
	case <-blocking.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("the transport writer did not enter the blocked socket write")
	}

	transport.Close()
	select {
	case <-blocking.closed:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("transport Close did not close the socket before joining its blocked writer")
	}
	if !waitForCondition(time.Second, func() bool {
		return !transport.IsConnected()
	}) {
		t.Fatal("transport remained registered after Close interrupted the blocked writer")
	}
}

// TestPlatformTransportCloseInterruptsBlockedH3Write covers the adjacent QUIC
// teardown path. A peer that stops reading exhausts stream flow control and
// parks Framer.Write inside quic.Stream.Write. Canceling handleCtx cannot wake
// that write; the connection must be closed before the transport joins its
// writer goroutine.
func TestPlatformTransportCloseInterruptsBlockedH3Write(t *testing.T) {
	certPem, keyPem, err := selfSign(
		[]string{"127.0.0.1"},
		"127.0.0.1",
		24*time.Hour,
		24*time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPem, keyPem)
	if err != nil {
		t.Fatal(err)
	}
	const nextProto = "urnetwork-platform-test"
	listener, err := quic.ListenAddrEarly(
		"127.0.0.1:0",
		&tls.Config{
			Certificates: []tls.Certificate{cert},
			NextProtos:   []string{nextProto},
		},
		&quic.Config{
			MaxIdleTimeout:                 30 * time.Second,
			InitialStreamReceiveWindow:     32 * 1024,
			MaxStreamReceiveWindow:         32 * 1024,
			InitialConnectionReceiveWindow: 64 * 1024,
			MaxConnectionReceiveWindow:     64 * 1024,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
	})

	serverCtx, serverCancel := context.WithCancel(context.Background())
	defer serverCancel()
	serverConn := make(chan *quic.Conn, 1)
	serverErr := make(chan error, 1)
	framerSettings := DefaultFramerSettings(int(DefaultClientSettings().MinimumMessageLenLimit()))
	go func() {
		conn, acceptErr := listener.Accept(serverCtx)
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		serverConn <- conn
		stream, acceptErr := conn.AcceptStream(serverCtx)
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		framer := NewFramer(framerSettings)
		authBytes, readErr := framer.Read(stream)
		if readErr != nil {
			serverErr <- readErr
			return
		}
		writeErr := framer.Write(stream, authBytes)
		MessagePoolReturn(authBytes)
		if writeErr != nil {
			serverErr <- writeErr
			return
		}
		// Deliberately never read another byte. The client's next large writes
		// consume the fixed receive credit and then block.
		<-conn.Context().Done()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := testingPlatformTransportSettings()
	settings.H3Port = listener.Addr().(*net.UDPAddr).Port
	settings.WriteTimeout = 30 * time.Second
	settings.QuicTlsConfig = &tls.Config{
		InsecureSkipVerify: true, // test-only self-signed endpoint
		NextProtos:         []string{nextProto},
	}
	settings.FramerSettings = framerSettings
	routeManager := NewRouteManager(ctx, "test")
	transport := NewPlatformTransportWithTargetMode(
		ctx,
		NewClientStrategyWithDefaults(ctx),
		routeManager,
		"https://127.0.0.1",
		&ClientAuth{
			ByJwt:      "testing",
			InstanceId: NewId(),
			AppVersion: "testing",
		},
		TransportModeH3,
		settings,
	)
	t.Cleanup(transport.Close)

	var accepted *quic.Conn
	select {
	case accepted = <-serverConn:
	case acceptErr := <-serverErr:
		t.Fatal(acceptErr)
	case <-time.After(5 * time.Second):
		t.Fatal("the QUIC server did not accept the platform connection")
	}
	if !testingWaitForActiveMode(transport, TransportModeH3, 5*time.Second) {
		t.Fatal("the H3 transport was never elected")
	}

	writer := routeManager.OpenMultiRouteWriter(DestinationId(NewId()))
	defer routeManager.CloseMultiRouteWriter(writer)
	blocked := false
	for range 128 {
		message := MessagePoolGet(3 * 1024)
		if writeErr := writer.Write(ctx, message, 20*time.Millisecond); writeErr != nil {
			MessagePoolReturn(message)
			blocked = true
			break
		}
	}
	if !blocked {
		t.Fatal("the unread QUIC stream never exhausted its bounded send route")
	}

	transport.Close()
	select {
	case <-accepted.Context().Done():
	case <-time.After(500 * time.Millisecond):
		t.Fatal("transport Close did not close QUIC before joining its flow-control-blocked writer")
	}
}
