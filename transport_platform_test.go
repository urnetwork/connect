package connect

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

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

	connectCount atomic.Int64
	rejecting    atomic.Bool

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
			if _, _, err := ws.ReadMessage(); err != nil {
				ws.Close()
				return
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
