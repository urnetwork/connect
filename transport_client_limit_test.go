package connect

// transport_client_limit_test.go -- the platform's client limit close and the
// client's hold (transport_client_limit.go), and the provide intent the
// transports declare (transport_provide_intent.go).
//
// The hold runs on a manual clock: time moves only when a test advances it,
// and a transport reports the exact point it parks on the hold through
// clientLimitHoldForTest. Once parked, a runner cannot dial until the hold
// changes, so "no redial while held" is read at a barrier, not after a sleep.

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	quic "github.com/quic-go/quic-go"

	"github.com/urnetwork/connect/protocol"
)

// testingClientLimitTimer is one expiry armed on the manual clock.
type testingClientLimitTimer struct {
	fireTime time.Time
	f        func()
	stopped  bool
	fired    bool
}

// testingClientLimitClock is a manual clock for a ClientLimitBackoff.
type testingClientLimitClock struct {
	stateLock sync.Mutex
	now       time.Time
	timers    []*testingClientLimitTimer
	// every armed timeout, in arm order
	armedTimeouts chan time.Duration
}

// newTestingClientLimitClock starts the clock at a fixed synthetic time.
func newTestingClientLimitClock() *testingClientLimitClock {
	return &testingClientLimitClock{
		now:           time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC),
		armedTimeouts: make(chan time.Duration, 64),
	}
}

// Now is the clock's current time.
func (self *testingClientLimitClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

// AfterFunc arms f to run when advance passes timeout, records the timeout,
// and returns the stop, as time.AfterFunc does.
func (self *testingClientLimitClock) AfterFunc(timeout time.Duration, f func()) func() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	timer := &testingClientLimitTimer{
		fireTime: self.now.Add(timeout),
		f:        f,
	}
	self.timers = append(self.timers, timer)
	// a test reads the first arms; never block the hold on the record
	select {
	case self.armedTimeouts <- timeout:
	default:
	}
	return func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if timer.stopped || timer.fired {
			return false
		}
		timer.stopped = true
		return true
	}
}

// advance moves the clock and runs every live timer that came due, outside
// the clock lock, in arm order.
func (self *testingClientLimitClock) advance(d time.Duration) {
	dueTimers := func() []*testingClientLimitTimer {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.now = self.now.Add(d)
		dueTimers := []*testingClientLimitTimer{}
		for _, timer := range self.timers {
			if !timer.stopped && !timer.fired && !timer.fireTime.After(self.now) {
				timer.fired = true
				dueTimers = append(dueTimers, timer)
			}
		}
		return dueTimers
	}()
	for _, timer := range dueTimers {
		timer.f()
	}
}

// timer returns the i-th armed timer.
func (self *testingClientLimitClock) timer(i int) *testingClientLimitTimer {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.timers[i]
}

// nextArmedTimeout is the timeout of the next arm, or fails.
func (self *testingClientLimitClock) nextArmedTimeout(t *testing.T) time.Duration {
	t.Helper()
	select {
	case timeout := <-self.armedTimeouts:
		return timeout
	case <-time.After(10 * time.Second):
		t.Fatal("no client limit expiry was armed")
		return 0
	}
}

// newTestingClientLimitBackoff is a hold on the manual clock with a fixed
// jitter.
func newTestingClientLimitBackoff(clock *testingClientLimitClock, jitter time.Duration) *ClientLimitBackoff {
	backoff := NewClientLimitBackoff()
	backoff.now = clock.Now
	backoff.afterFunc = clock.AfterFunc
	backoff.jitter = func(maxJitter time.Duration) time.Duration {
		return min(jitter, maxJitter)
	}
	return backoff
}

// noteTestingClientLimitExceeded starts or extends a hold for a close of a
// connection dialed under the current reset generation, which always holds.
func noteTestingClientLimitExceeded(t *testing.T, backoff *ClientLimitBackoff) time.Time {
	t.Helper()
	retryTime, held := backoff.noteExceeded(backoff.dialResetGeneration())
	if !held {
		t.Fatal("a close dialed under the current reset generation started no hold")
	}
	return retryTime
}

// clientLimitNotified reads, without waiting, whether a monitor channel fired.
func clientLimitNotified(notify chan struct{}) bool {
	select {
	case <-notify:
		return true
	default:
		return false
	}
}

// A client limit close holds for the timeout plus jitter, never less than
// ClientLimitBackoffTimeout. The hold ends only when its retry time has come,
// and every change notifies.
func TestClientLimitBackoffHoldsForAtLeastTheTimeout(t *testing.T) {
	if ClientLimitBackoffTimeout < 15*time.Minute {
		t.Fatalf("hold timeout = %s, want at least 15m", ClientLimitBackoffTimeout)
	}
	clock := newTestingClientLimitClock()
	start := clock.Now()
	backoff := newTestingClientLimitBackoff(clock, 2*time.Minute)

	status, notify := backoff.Get()
	if status.Exceeded || !status.RetryTime.IsZero() {
		t.Fatalf("initial status = %+v, want no hold", status)
	}

	retryTime := noteTestingClientLimitExceeded(t, backoff)
	if want := start.Add(ClientLimitBackoffTimeout + 2*time.Minute); !retryTime.Equal(want) {
		t.Fatalf("retry time = %s, want %s", retryTime, want)
	}
	if !clientLimitNotified(notify) {
		t.Fatal("starting the hold did not notify")
	}
	if status := backoff.Status(); !status.Exceeded || !status.RetryTime.Equal(retryTime) {
		t.Fatalf("status = %+v, want exceeded until %s", status, retryTime)
	}
	if timeout := clock.nextArmedTimeout(t); timeout != ClientLimitBackoffTimeout+2*time.Minute {
		t.Fatalf("armed expiry = %s, want %s", timeout, ClientLimitBackoffTimeout+2*time.Minute)
	}

	// a timer that fires before the retry time keeps the hold and re-arms for
	// the rest
	_, notify = backoff.Get()
	clock.advance(time.Minute)
	clock.timer(0).f()
	if !backoff.Status().Exceeded || clientLimitNotified(notify) {
		t.Fatalf("an early expiry ended or touched the hold: %+v", backoff.Status())
	}
	if timeout := clock.nextArmedTimeout(t); timeout != ClientLimitBackoffTimeout+time.Minute {
		t.Fatalf("re-armed expiry = %s, want %s", timeout, ClientLimitBackoffTimeout+time.Minute)
	}

	// a minute short of the retry time still holds
	clock.advance(ClientLimitBackoffTimeout)
	if !backoff.Status().Exceeded {
		t.Fatal("the hold ended before its retry time")
	}
	clock.advance(time.Minute)
	if status := backoff.Status(); status.Exceeded || !status.RetryTime.IsZero() {
		t.Fatalf("status after the retry time = %+v, want no hold", status)
	}
	if !clientLimitNotified(notify) {
		t.Fatal("the end of the hold did not notify")
	}
}

// A second close inside a hold never shortens it, and extends it when it would
// end later. A timer of an older arm can neither end nor re-arm the newer hold.
func TestClientLimitBackoffRepeatedCloseNeverShortensTheHold(t *testing.T) {
	clock := newTestingClientLimitClock()
	start := clock.Now()
	backoff := newTestingClientLimitBackoff(clock, ClientLimitBackoffJitter)
	first := noteTestingClientLimitExceeded(t, backoff)
	clock.nextArmedTimeout(t)

	// one minute later with no jitter would end four minutes earlier
	backoff.jitter = func(time.Duration) time.Duration { return 0 }
	clock.advance(time.Minute)
	if again := noteTestingClientLimitExceeded(t, backoff); !again.Equal(first) {
		t.Fatalf("a second close moved the retry time from %s to %s", first, again)
	}
	select {
	case timeout := <-clock.armedTimeouts:
		t.Fatalf("a close that ends earlier re-armed the expiry (%s)", timeout)
	default:
	}

	// fourteen minutes into the hold a close ends later and extends it
	clock.advance(13 * time.Minute)
	extended := noteTestingClientLimitExceeded(t, backoff)
	if want := start.Add(14*time.Minute + ClientLimitBackoffTimeout); !extended.Equal(want) {
		t.Fatalf("extended retry time = %s, want %s", extended, want)
	}
	clock.nextArmedTimeout(t)

	// the first arm's timer firing late, after the re-arm stopped it
	clock.advance(first.Sub(clock.Now()))
	clock.timer(0).f()
	if status := backoff.Status(); !status.Exceeded || !status.RetryTime.Equal(extended) {
		t.Fatalf("a stale expiry changed the hold to %+v", status)
	}
	select {
	case timeout := <-clock.armedTimeouts:
		t.Fatalf("a stale expiry re-armed (%s)", timeout)
	default:
	}

	clock.advance(extended.Sub(clock.Now()))
	if backoff.Status().Exceeded {
		t.Fatal("the extended hold did not end at its retry time")
	}

	// a close that ends exactly with the hold in force arms nothing more
	same := noteTestingClientLimitExceeded(t, backoff)
	clock.nextArmedTimeout(t)
	if again := noteTestingClientLimitExceeded(t, backoff); !again.Equal(same) {
		t.Fatalf("an equal close moved the retry time from %s to %s", same, again)
	}
	select {
	case timeout := <-clock.armedTimeouts:
		t.Fatalf("an equal close re-armed the expiry (%s)", timeout)
	default:
	}
}

// Reset ends a hold at once and notifies; the expiry it stopped can no longer
// touch a hold that starts later.
func TestClientLimitBackoffResetEndsTheHold(t *testing.T) {
	clock := newTestingClientLimitClock()
	backoff := newTestingClientLimitBackoff(clock, 0)
	noteTestingClientLimitExceeded(t, backoff)
	clock.nextArmedTimeout(t)

	_, notify := backoff.Get()
	backoff.Reset()
	if status := backoff.Status(); status.Exceeded || !status.RetryTime.IsZero() {
		t.Fatalf("status after reset = %+v, want no hold", status)
	}
	if !clientLimitNotified(notify) {
		t.Fatal("the reset did not notify")
	}

	// a new hold, then the reset hold's expiry firing late
	clock.advance(time.Minute)
	retryTime := noteTestingClientLimitExceeded(t, backoff)
	clock.nextArmedTimeout(t)
	clock.timer(0).f()
	if status := backoff.Status(); !status.Exceeded || !status.RetryTime.Equal(retryTime) {
		t.Fatalf("the reset hold's expiry changed the new hold to %+v", status)
	}

	// a reset with no hold in force changes nothing
	backoff.Reset()
	_, notify = backoff.Get()
	backoff.Reset()
	if clientLimitNotified(notify) {
		t.Fatal("a reset with no hold notified")
	}
}

// A reset supersedes every connection dialed before it: a close of one starts
// no hold and changes nothing, and a close dialed after it holds as usual.
func TestClientLimitBackoffResetSupersedesEarlierDials(t *testing.T) {
	clock := newTestingClientLimitClock()
	backoff := newTestingClientLimitBackoff(clock, 0)
	before := backoff.dialResetGeneration()
	backoff.Reset()

	_, notify := backoff.Get()
	if retryTime, held := backoff.noteExceeded(before); held || !retryTime.IsZero() {
		t.Fatalf("a close dialed before the reset held until %s", retryTime)
	}
	if backoff.Status().Exceeded || clientLimitNotified(notify) {
		t.Fatalf("a close dialed before the reset changed the hold to %+v", backoff.Status())
	}
	select {
	case timeout := <-clock.armedTimeouts:
		t.Fatalf("a close dialed before the reset armed an expiry (%s)", timeout)
	default:
	}

	after := backoff.dialResetGeneration()
	if after == before {
		t.Fatal("the reset did not advance the generation a dial carries")
	}
	if _, held := backoff.noteExceeded(after); !held || !backoff.Status().Exceeded {
		t.Fatalf("a close dialed after the reset did not hold: %+v", backoff.Status())
	}
}

// The production jitter stays inside its bound.
func TestClientLimitBackoffJitterBound(t *testing.T) {
	backoff := NewClientLimitBackoff()
	for range 1000 {
		jitter := backoff.jitter(ClientLimitBackoffJitter)
		if jitter < 0 || ClientLimitBackoffJitter <= jitter {
			t.Fatalf("jitter = %s, want in [0, %s)", jitter, ClientLimitBackoffJitter)
		}
	}
	if jitter := backoff.jitter(0); jitter != 0 {
		t.Fatalf("jitter without a bound = %s, want 0", jitter)
	}
}

// Only a 5-byte close control is a close, and only reason 1 is the client
// limit; other close codes and remote causes are ordinary closes.
func TestClientLimitCloseRecognition(t *testing.T) {
	for _, c := range []struct {
		message []byte
		reason  uint32
		ok      bool
	}{
		{message: []byte{TransportControlClose, 0, 0, 0, 1}, reason: TransportCloseReasonClientLimitExceeded, ok: true},
		{message: []byte{TransportControlClose, 0, 0, 0, 2}, reason: 2, ok: true},
		{message: []byte{TransportControlClose, 0, 0, 1, 0}, reason: 256, ok: true},
		{message: []byte{TransportControlSpeedStart, 0, 0, 0, 1}, ok: false},
		{message: []byte{TransportControlClose, 0, 0, 1}, ok: false},
		{message: []byte{TransportControlClose, 0, 0, 0, 1, 0}, ok: false},
		{message: nil, ok: false},
	} {
		reason, ok := transportCloseReason(c.message)
		if reason != c.reason || ok != c.ok {
			t.Errorf("close reason of %v = (%d, %t), want (%d, %t)", c.message, reason, ok, c.reason, c.ok)
		}
	}

	for _, c := range []struct {
		err  error
		want bool
	}{
		{err: &websocket.CloseError{Code: ClientLimitCloseCode, Text: ClientLimitCloseText}, want: true},
		{err: fmt.Errorf("read: %w", &websocket.CloseError{Code: ClientLimitCloseCode}), want: true},
		{err: &websocket.CloseError{Code: websocket.CloseNormalClosure}, want: false},
		{err: &quic.ApplicationError{Remote: true, ErrorCode: ClientLimitCloseCode, ErrorMessage: ClientLimitCloseText}, want: true},
		{err: fmt.Errorf("stream: %w", &quic.ApplicationError{Remote: true, ErrorCode: ClientLimitCloseCode}), want: true},
		// the client's own close with the code is not the platform's
		{err: &quic.ApplicationError{Remote: false, ErrorCode: ClientLimitCloseCode}, want: false},
		{err: &quic.ApplicationError{Remote: true, ErrorCode: 0}, want: false},
		{err: errors.New(ClientLimitCloseText), want: false},
		{err: nil, want: false},
	} {
		if got := isClientLimitCloseError(c.err); got != c.want {
			t.Errorf("client limit close of %v = %t, want %t", c.err, got, c.want)
		}
	}
}

// testingClientLimitConnection is one platform-side h1 connection.
type testingClientLimitConnection struct {
	provideIntent string
	framed        bool
	conn          H1MessageConn
	// closed when the platform's read of the connection ends
	done chan struct{}
}

// closeForClientLimit sends the platform's client limit close: the close
// control, the WebSocket close code on WebSocket only, then the socket close.
func (self testingClientLimitConnection) closeForClientLimit(reason uint32) {
	control := []byte{TransportControlClose, 0, 0, 0, 0}
	control[1] = byte(reason >> 24)
	control[2] = byte(reason >> 16)
	control[3] = byte(reason >> 8)
	control[4] = byte(reason)
	_ = self.conn.WriteMessage(websocket.BinaryMessage, control)
	if ws, ok := self.conn.(*websocket.Conn); ok && reason == TransportCloseReasonClientLimitExceeded {
		_ = ws.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(ClientLimitCloseCode, ClientLimitCloseText),
			time.Now().Add(time.Second),
		)
	}
	self.conn.Close()
}

// testingClientLimitPlatform is an h1 platform that accepts WebSocket and h1+
// upgrades and records each connection's provide intent header.
type testingClientLimitPlatform struct {
	url         string
	connections chan testingClientLimitConnection
	acceptCount atomic.Int64
	// closed when the test ends, so a handler never waits on a full
	// connections queue that a failed test stopped reading
	closed chan struct{}
}

// newTestingClientLimitPlatform serves the platform on the v4 loopback until
// the test ends.
func newTestingClientLimitPlatform(t *testing.T) *testingClientLimitPlatform {
	t.Helper()
	platform := &testingClientLimitPlatform{
		connections: make(chan testingClientLimitConnection, 64),
		closed:      make(chan struct{}),
	}
	var handlers sync.WaitGroup
	server := newTestingLoopbackHttpServer(t, 4, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlers.Add(1)
		defer handlers.Done()
		connection := testingClientLimitConnection{
			provideIntent: r.Header.Get(HeaderProvideIntent),
			done:          make(chan struct{}),
		}
		if r.Header.Get("Upgrade") == H1FramerProtocol {
			raw, err := AcceptFramedUpgrade(w, r, H1FramerProtocol, time.Second)
			if err != nil {
				return
			}
			framed, err := NewFramedMessageConn(raw, H1FramerProtocol, 65535, nil)
			if err != nil {
				raw.Close()
				return
			}
			connection.framed = true
			connection.conn = framed
		} else {
			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			ws, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			connection.conn = ws
		}
		platform.acceptCount.Add(1)
		defer close(connection.done)
		defer connection.conn.Close()
		select {
		case platform.connections <- connection:
		case <-platform.closed:
			return
		}
		for {
			if _, _, err := connection.conn.ReadMessage(); err != nil {
				return
			}
		}
	}), false)
	platform.url = "ws" + strings.TrimPrefix(server.URL, "http")
	t.Cleanup(func() {
		close(platform.closed)
		server.CloseClientConnections()
		server.Close()
		handlers.Wait()
	})
	return platform
}

// nextConnection waits for the next accepted connection, or fails.
func (self *testingClientLimitPlatform) nextConnection(t *testing.T) testingClientLimitConnection {
	t.Helper()
	select {
	case connection := <-self.connections:
		return connection
	case <-time.After(15 * time.Second):
		t.Fatal("no platform connection arrived")
		return testingClientLimitConnection{}
	}
}

// newTestingClientLimitTransport is an h1 transport on the hold, which reports
// each park on the hold to parked.
func newTestingClientLimitTransport(
	t *testing.T,
	ctx context.Context,
	platformUrl string,
	backoff *ClientLimitBackoff,
	framed bool,
	provideIntent bool,
) (*PlatformTransport, chan struct{}) {
	t.Helper()
	parked := make(chan struct{}, 64)
	settings := testingPlatformTransportSettings()
	settings.EnableH1Plus = framed
	settings.ReconnectTimeout = time.Millisecond
	settings.ClientLimitBackoff = backoff
	settings.clientLimitHoldForTest = func() {
		select {
		case parked <- struct{}{}:
		default:
		}
	}
	strategySettings := DefaultClientStrategySettings()
	strategySettings.EnableNormal = true
	strategySettings.EnableResilient = false
	strategy := NewClientStrategy(ctx, strategySettings)
	t.Cleanup(strategy.Close)
	transport := NewPlatformTransportWithTargetMode(
		ctx,
		strategy,
		NewRouteManager(ctx, "client-limit"),
		platformUrl,
		&ClientAuth{
			ByJwt:         "testing",
			InstanceId:    NewId(),
			AppVersion:    "testing",
			ProvideIntent: provideIntent,
		},
		TransportModeH1,
		settings,
	)
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := transport.CloseAndWait(closeCtx); err != nil {
			t.Errorf("transport did not join: %v", err)
		}
	})
	return transport, parked
}

// waitClientLimitParked waits for a runner to park on the hold.
func waitClientLimitParked(t *testing.T, parked chan struct{}, platformAcceptCount func() int64) {
	t.Helper()
	select {
	case <-parked:
	case <-time.After(10 * time.Second):
		t.Fatalf("the transport did not hold its dials after the client limit close (platform connections = %d)", platformAcceptCount())
	}
}

// The h1 client limit close -- the close control on WebSocket and on h1+ --
// holds the transport's dials for at least the timeout. No dial happens while
// it holds; when it ends the transport dials again, still declaring intent.
func TestPlatformTransportH1ClientLimitCloseHoldsDials(t *testing.T) {
	resetH1UpgradeTestState(t)
	for _, framed := range []bool{false, true} {
		func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			clock := newTestingClientLimitClock()
			backoff := newTestingClientLimitBackoff(clock, 0)
			platform := newTestingClientLimitPlatform(t)
			transport, parked := newTestingClientLimitTransport(t, ctx, platform.url, backoff, framed, true)

			first := platform.nextConnection(t)
			if first.framed != framed {
				t.Fatalf("h1+ = %t: the platform accepted h1+ = %t", framed, first.framed)
			}
			if first.provideIntent != ProvideIntentDeclared {
				t.Fatalf("h1+ = %t: %s = %q, want %q", framed, HeaderProvideIntent, first.provideIntent, ProvideIntentDeclared)
			}
			if !waitForCondition(10*time.Second, transport.IsConnected) {
				t.Fatalf("h1+ = %t: the transport never registered its routes", framed)
			}

			first.closeForClientLimit(TransportCloseReasonClientLimitExceeded)
			waitClientLimitParked(t, parked, platform.acceptCount.Load)

			// parked: nothing can dial until the hold changes
			if count := platform.acceptCount.Load(); count != 1 {
				t.Fatalf("h1+ = %t: the platform saw %d connections while the hold was in force, want 1", framed, count)
			}
			status := backoff.Status()
			if want := clock.Now().Add(ClientLimitBackoffTimeout); !status.Exceeded || !status.RetryTime.Equal(want) {
				t.Fatalf("h1+ = %t: hold = %+v, want exceeded until %s", framed, status, want)
			}
			if timeout := clock.nextArmedTimeout(t); timeout < 15*time.Minute {
				t.Fatalf("h1+ = %t: hold armed for %s, want at least 15m", framed, timeout)
			}
			if state := transport.State(); state != PlatformTransportStateClientLimit {
				t.Fatalf("h1+ = %t: state = %s, want %s", framed, state, PlatformTransportStateClientLimit)
			}
			select {
			case <-first.done:
			case <-time.After(10 * time.Second):
				t.Fatalf("h1+ = %t: the closed connection stayed open", framed)
			}

			// the end of the hold releases the parked runner
			clock.advance(ClientLimitBackoffTimeout)
			second := platform.nextConnection(t)
			if second.provideIntent != ProvideIntentDeclared {
				t.Fatalf("h1+ = %t: the redial declared %q, want %q", framed, second.provideIntent, ProvideIntentDeclared)
			}
			if !waitForCondition(10*time.Second, transport.IsConnected) {
				t.Fatalf("h1+ = %t: the redial never registered its routes", framed)
			}
			if state := transport.State(); state != PlatformTransportStateConnected {
				t.Fatalf("h1+ = %t: state after the hold = %s, want connected", framed, state)
			}
		}()
	}
}

// The WebSocket close code alone, without the close control, is the same
// signal.
func TestPlatformTransportH1ClientLimitCloseCodeHoldsDials(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := newTestingClientLimitClock()
	backoff := newTestingClientLimitBackoff(clock, 0)
	platform := newTestingClientLimitPlatform(t)
	_, parked := newTestingClientLimitTransport(t, ctx, platform.url, backoff, false, true)

	first := platform.nextConnection(t)
	ws, ok := first.conn.(*websocket.Conn)
	if !ok {
		t.Fatal("the platform did not accept a WebSocket")
	}
	if err := ws.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(ClientLimitCloseCode, ClientLimitCloseText),
		time.Now().Add(time.Second),
	); err != nil {
		t.Fatal(err)
	}
	waitClientLimitParked(t, parked, platform.acceptCount.Load)
	if count := platform.acceptCount.Load(); count != 1 || !backoff.Status().Exceeded {
		t.Fatalf("connections = %d, hold = %+v; want 1 and exceeded", count, backoff.Status())
	}
}

// A close control with an unknown reason and a plain close are ordinary
// closes: the transport redials at once and takes no hold.
func TestPlatformTransportH1OrdinaryClosesTakeNoHold(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := newTestingClientLimitClock()
	backoff := newTestingClientLimitBackoff(clock, 0)
	platform := newTestingClientLimitPlatform(t)
	_, parked := newTestingClientLimitTransport(t, ctx, platform.url, backoff, false, false)

	first := platform.nextConnection(t)
	first.closeForClientLimit(2)
	second := platform.nextConnection(t)
	second.conn.Close()
	platform.nextConnection(t)

	if backoff.Status().Exceeded {
		t.Fatalf("an ordinary close took a hold: %+v", backoff.Status())
	}
	select {
	case <-parked:
		t.Fatal("an ordinary close parked the transport on a hold")
	default:
	}
}

// A close of a connection whose provide intent the client has since changed
// judges a declaration the client no longer makes: the transport takes no hold
// and redials with its current declaration.
func TestPlatformTransportClientLimitCloseForSupersededIntentTakesNoHold(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := newTestingClientLimitClock()
	backoff := newTestingClientLimitBackoff(clock, 0)
	platform := newTestingClientLimitPlatform(t)
	transport, parked := newTestingClientLimitTransport(t, ctx, platform.url, backoff, false, false)

	first := platform.nextConnection(t)
	if first.provideIntent != "" {
		t.Fatalf("%s = %q, want none", HeaderProvideIntent, first.provideIntent)
	}
	transport.SetAuth(&ClientAuth{
		ByJwt:         "testing",
		InstanceId:    NewId(),
		AppVersion:    "testing",
		ProvideIntent: true,
	})
	first.closeForClientLimit(TransportCloseReasonClientLimitExceeded)

	second := platform.nextConnection(t)
	if second.provideIntent != ProvideIntentDeclared {
		t.Fatalf("the redial declared %q, want %q", second.provideIntent, ProvideIntentDeclared)
	}
	if backoff.Status().Exceeded {
		t.Fatalf("a close of a superseded declaration took a hold: %+v", backoff.Status())
	}
	select {
	case <-parked:
		t.Fatal("a close of a superseded declaration parked the transport")
	default:
	}
}

// A close of a connection dialed before the owner reset the hold judged a
// declaration the owner has since replaced: the transport takes no hold and
// redials.
func TestPlatformTransportClientLimitCloseDialedBeforeResetTakesNoHold(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := newTestingClientLimitClock()
	backoff := newTestingClientLimitBackoff(clock, 0)
	platform := newTestingClientLimitPlatform(t)
	_, parked := newTestingClientLimitTransport(t, ctx, platform.url, backoff, false, true)

	first := platform.nextConnection(t)
	backoff.Reset()
	first.closeForClientLimit(TransportCloseReasonClientLimitExceeded)
	platform.nextConnection(t)

	if backoff.Status().Exceeded {
		t.Fatalf("a close dialed before the reset took a hold: %+v", backoff.Status())
	}
	select {
	case <-parked:
		t.Fatal("a close dialed before the reset parked the transport")
	default:
	}
}

// Reset releases a transport parked on the hold before the hold's time.
func TestPlatformTransportClientLimitResetReleasesParkedTransport(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := newTestingClientLimitClock()
	backoff := newTestingClientLimitBackoff(clock, 0)
	platform := newTestingClientLimitPlatform(t)
	_, parked := newTestingClientLimitTransport(t, ctx, platform.url, backoff, false, true)

	first := platform.nextConnection(t)
	first.closeForClientLimit(TransportCloseReasonClientLimitExceeded)
	waitClientLimitParked(t, parked, platform.acceptCount.Load)
	if count := platform.acceptCount.Load(); count != 1 {
		t.Fatalf("the platform saw %d connections while the hold was in force, want 1", count)
	}
	backoff.Reset()
	platform.nextConnection(t)
}

// A transport declares provide intent exactly when its auth generation does:
// the h1 header, and the auth frame field on the first-frame h1 auth.
func TestPlatformTransportH1DeclaresProvideIntentOnlyWhenSet(t *testing.T) {
	for _, provideIntent := range []bool{false, true} {
		func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			platform := newTestingClientLimitPlatform(t)
			newTestingClientLimitTransport(t, ctx, platform.url, NewClientLimitBackoff(), false, provideIntent)
			connection := platform.nextConnection(t)
			want := ""
			if provideIntent {
				want = ProvideIntentDeclared
			}
			if connection.provideIntent != want {
				t.Fatalf("intent %t: %s = %q, want %q", provideIntent, HeaderProvideIntent, connection.provideIntent, want)
			}
		}()
	}

	// the first-frame auth carries the same declaration
	for _, provideIntent := range []bool{false, true} {
		func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			platform := newTestingFamilyPlatformServer(t, 4, true)
			settings := testingFamilyTransportSettings()
			settings.V2H1Auth = false
			declared := make(chan bool, 4)
			settings.AuthFrameObserver = func(authFrameBytes []byte) {
				if decoded, err := DecodeFrame(authFrameBytes); err == nil {
					if auth, ok := decoded.(*protocol.Auth); ok {
						declared <- auth.ProvideIntent
					}
				}
			}
			transport := NewPlatformTransportWithTargetMode(
				ctx,
				NewClientStrategyWithDefaults(ctx),
				NewRouteManager(ctx, "provide-intent-v1"),
				platform.url(),
				&ClientAuth{
					ByJwt:         "testing",
					InstanceId:    NewId(),
					AppVersion:    "testing",
					ProvideIntent: provideIntent,
				},
				TransportModeH1,
				settings,
			)
			defer transport.Close()
			select {
			case got := <-declared:
				if got != provideIntent {
					t.Fatalf("auth frame provide_intent = %t, want %t", got, provideIntent)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("no auth frame was built")
			}
		}()
	}
}

// The hold belongs to the client: a close of one transport closes the live
// connection of every transport sharing the hold and parks all of them.
func TestPlatformTransportClientLimitHoldClosesSharingTransports(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := newTestingClientLimitClock()
	backoff := newTestingClientLimitBackoff(clock, 0)
	platform := newTestingClientLimitPlatform(t)
	_, firstParked := newTestingClientLimitTransport(t, ctx, platform.url, backoff, false, true)
	first := platform.nextConnection(t)
	secondTransport, secondParked := newTestingClientLimitTransport(t, ctx, platform.url, backoff, false, true)
	second := platform.nextConnection(t)
	if !waitForCondition(10*time.Second, secondTransport.IsConnected) {
		t.Fatal("the second transport never registered its routes")
	}

	first.closeForClientLimit(TransportCloseReasonClientLimitExceeded)
	waitClientLimitParked(t, firstParked, platform.acceptCount.Load)
	select {
	case <-second.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the sharing transport kept its connection through the hold")
	}
	waitClientLimitParked(t, secondParked, platform.acceptCount.Load)
	if count := platform.acceptCount.Load(); count != 2 {
		t.Fatalf("the platform saw %d connections while the hold was in force, want 2", count)
	}

	clock.advance(ClientLimitBackoffTimeout)
	platform.nextConnection(t)
	platform.nextConnection(t)
}

// testingClientLimitH3Platform is a QUIC platform that reads each auth frame,
// records its provide intent, and either closes for the client limit before
// echoing the auth or echoes it and hands the connection to the test.
type testingClientLimitH3Platform struct {
	port        int
	nextProto   string
	framerSets  *FramerSettings
	intents     chan bool
	connections chan *quic.Conn
	acceptCount atomic.Int64
	// the platform closes the next connection this many times before echoing
	closeBeforeEcho atomic.Int64
}

// newTestingClientLimitH3Platform serves the QUIC platform on the v4 loopback
// with a self-signed certificate until the test ends.
func newTestingClientLimitH3Platform(t *testing.T) *testingClientLimitH3Platform {
	t.Helper()
	host := testLoopbackIp(4)
	certPem, keyPem, err := selfSign([]string{host}, host, 24*time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPem, keyPem)
	if err != nil {
		t.Fatal(err)
	}
	nextProto := "urnetwork-platform-client-limit-test"
	listener, err := quic.ListenAddrEarly(
		testLoopbackHostPort(4, 0),
		&tls.Config{
			Certificates: []tls.Certificate{cert},
			NextProtos:   []string{nextProto},
		},
		&quic.Config{MaxIdleTimeout: 30 * time.Second},
	)
	if err != nil {
		t.Fatal(err)
	}
	serverCtx, cancel := context.WithCancel(context.Background())
	platform := &testingClientLimitH3Platform{
		port:        listener.Addr().(*net.UDPAddr).Port,
		nextProto:   nextProto,
		framerSets:  DefaultFramerSettings(int(DefaultClientSettings().MinimumMessageLenLimit())),
		intents:     make(chan bool, 64),
		connections: make(chan *quic.Conn, 64),
	}
	var handlers sync.WaitGroup
	handlers.Add(1)
	go func() {
		defer handlers.Done()
		for {
			conn, err := listener.Accept(serverCtx)
			if err != nil {
				return
			}
			handlers.Add(1)
			go func() {
				defer handlers.Done()
				stream, err := conn.AcceptStream(serverCtx)
				if err != nil {
					return
				}
				framer := NewFramer(platform.framerSets)
				authBytes, err := framer.Read(stream)
				if err != nil {
					return
				}
				defer MessagePoolReturn(authBytes)
				if decoded, err := DecodeFrame(authBytes); err == nil {
					if auth, ok := decoded.(*protocol.Auth); ok {
						select {
						case platform.intents <- auth.ProvideIntent:
						case <-serverCtx.Done():
							return
						}
					}
				}
				platform.acceptCount.Add(1)
				if 0 < platform.closeBeforeEcho.Load() {
					platform.closeBeforeEcho.Add(-1)
					conn.CloseWithError(ClientLimitCloseCode, ClientLimitCloseText)
					return
				}
				if err := framer.Write(stream, authBytes); err != nil {
					return
				}
				select {
				case platform.connections <- conn:
				case <-serverCtx.Done():
					return
				}
				for {
					message, err := framer.Read(stream)
					if err != nil {
						return
					}
					MessagePoolReturn(message)
				}
			}()
		}
	}()
	t.Cleanup(func() {
		cancel()
		listener.Close()
		handlers.Wait()
	})
	return platform
}

// nextIntent waits for the provide intent of the next auth frame, or fails.
func (self *testingClientLimitH3Platform) nextIntent(t *testing.T) bool {
	t.Helper()
	select {
	case intent := <-self.intents:
		return intent
	case <-time.After(15 * time.Second):
		t.Fatal("no h3 auth frame arrived")
		return false
	}
}

// nextConnection waits for the next connection whose auth was echoed, or
// fails.
func (self *testingClientLimitH3Platform) nextConnection(t *testing.T) *quic.Conn {
	t.Helper()
	select {
	case conn := <-self.connections:
		return conn
	case <-time.After(15 * time.Second):
		t.Fatal("no h3 connection was echoed")
		return nil
	}
}

// The h3 client limit close -- the QUIC application close, after the auth or
// right after the handshake -- holds the transport's dials, and the auth frame
// declares the provide intent.
func TestPlatformTransportH3ClientLimitCloseHoldsDials(t *testing.T) {
	for _, closeBeforeEcho := range []bool{false, true} {
		func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			clock := newTestingClientLimitClock()
			backoff := newTestingClientLimitBackoff(clock, 0)
			platform := newTestingClientLimitH3Platform(t)
			if closeBeforeEcho {
				platform.closeBeforeEcho.Store(1)
			}

			parked := make(chan struct{}, 64)
			settings := testingFamilyTransportSettings()
			settings.ReconnectTimeout = time.Millisecond
			settings.H3Port = platform.port
			settings.QuicConnectTimeout = 2 * time.Second
			settings.QuicHandshakeTimeout = 2 * time.Second
			settings.QuicTlsConfig = &tls.Config{
				InsecureSkipVerify: true, // test-only self-signed endpoint
				NextProtos:         []string{platform.nextProto},
			}
			settings.FramerSettings = platform.framerSets
			settings.ClientLimitBackoff = backoff
			settings.clientLimitHoldForTest = func() {
				select {
				case parked <- struct{}{}:
				default:
				}
			}
			strategySettings := DefaultClientStrategySettings()
			strategySettings.EnableNormal = true
			strategySettings.EnableResilient = false
			strategy := NewClientStrategy(ctx, strategySettings)
			defer strategy.Close()
			transport := NewPlatformTransportWithTargetMode(
				ctx,
				strategy,
				NewRouteManager(ctx, "client-limit-h3"),
				"https://"+testLoopbackHost(4),
				&ClientAuth{
					ByJwt:         "testing",
					InstanceId:    NewId(),
					AppVersion:    "testing",
					ProvideIntent: true,
				},
				TransportModeH3,
				settings,
			)
			defer func() {
				closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer closeCancel()
				if err := transport.CloseAndWait(closeCtx); err != nil {
					t.Errorf("close before echo = %t: transport did not join: %v", closeBeforeEcho, err)
				}
			}()

			if !platform.nextIntent(t) {
				t.Fatalf("close before echo = %t: the h3 auth frame did not declare provide intent", closeBeforeEcho)
			}
			if !closeBeforeEcho {
				conn := platform.nextConnection(t)
				if !waitForCondition(10*time.Second, transport.IsConnected) {
					t.Fatal("the h3 connection never registered its routes")
				}
				conn.CloseWithError(ClientLimitCloseCode, ClientLimitCloseText)
			}
			waitClientLimitParked(t, parked, platform.acceptCount.Load)
			if count := platform.acceptCount.Load(); count != 1 {
				t.Fatalf("close before echo = %t: the platform saw %d connections while the hold was in force, want 1", closeBeforeEcho, count)
			}
			if status := backoff.Status(); !status.Exceeded {
				t.Fatalf("close before echo = %t: hold = %+v, want exceeded", closeBeforeEcho, status)
			}
			if state := transport.State(); state != PlatformTransportStateClientLimit {
				t.Fatalf("close before echo = %t: state = %s, want %s", closeBeforeEcho, state, PlatformTransportStateClientLimit)
			}

			clock.advance(ClientLimitBackoffTimeout)
			if !platform.nextIntent(t) {
				t.Fatalf("close before echo = %t: the redial did not declare provide intent", closeBeforeEcho)
			}
			platform.nextConnection(t)
		}()
	}
}

// A provider group is one client: without a hold in its settings it builds one
// and shares it with every transport, and the caller's settings stay as they
// were. A hold in the settings is used as given.
func TestFamilyPlatformTransportGroupSharesOneClientLimitHold(t *testing.T) {
	clientSettings := newTestingFamilyStrategySettings(t)
	for _, given := range []bool{false, true} {
		func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			settings := testingFamilyTransportSettings()
			var givenBackoff *ClientLimitBackoff
			if given {
				givenBackoff = NewClientLimitBackoff()
				settings.ClientLimitBackoff = givenBackoff
			}
			strategy := NewClientStrategyWithDefaults(ctx)
			defer strategy.Close()
			group := NewFamilyPlatformTransportGroup(
				ctx,
				clientSettings,
				strategy,
				NewRouteManager(ctx, "client-limit-group"),
				"ws://127.0.0.1:1",
				familyTransportMissingUrl(4),
				familyTransportMissingUrl(6),
				testingFamilyAuth(),
				TransportModeH1,
				settings,
				nil,
			)
			defer group.Close()

			shared := group.ClientLimitBackoff()
			if shared == nil {
				t.Fatalf("given %t: the group has no hold", given)
			}
			if given && shared != givenBackoff {
				t.Fatalf("given %t: the group replaced the hold in its settings", given)
			}
			if !given && settings.ClientLimitBackoff != nil {
				t.Fatalf("given %t: the group wrote its hold into the caller's settings", given)
			}
			transports := group.Transports()
			if len(transports) != 3 {
				t.Fatalf("given %t: transports = %d, want 3", given, len(transports))
			}
			for _, transport := range transports {
				if transport.ClientLimitBackoff() != shared {
					t.Fatalf("given %t: a transport of the group has a hold of its own", given)
				}
			}
		}()
	}
}
