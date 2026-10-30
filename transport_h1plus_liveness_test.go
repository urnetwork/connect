// Real H1+ owners run against one synthetic in-memory upgrade peer. Virtual
// time advances heartbeat/deadline boundaries without a second socket reader.
package connect

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gorilla/websocket"
)

// Failure injection preserves ordinary pipe I/O until the exact owner arms a
// rejected deadline. Counters distinguish attempted unbounded I/O from close.
type h1LivenessConn struct {
	net.Conn
	failReadDeadline     atomic.Bool
	failWriteDeadline    atomic.Bool
	failWrite            atomic.Bool
	readRejected         atomic.Bool
	writeRejected        atomic.Bool
	readsAfterRejection  atomic.Int64
	writesAfterRejection atomic.Int64
	writesInProgress     atomic.Int64
	writeTimedOut        atomic.Bool
	closeOnce            sync.Once
	closed               chan struct{}
	// deadlineStatsLock guards deadlineStats: the deadline installations that
	// reached this socket and the time its write deadline left as writes began
	deadlineStatsLock sync.Mutex
	deadlineStats     h1LivenessDeadlineStats
}

// The socket deadline activity since the last reset.
type h1LivenessDeadlineStats struct {
	readDeadlineCount  int
	writeDeadlineCount int
	// the write deadline the socket holds, zero for none
	writeDeadline time.Time
	// writes begun, and the least and most time their write deadline left
	writeCount               int
	leastWriteDeadlineLeft   time.Duration
	mostWriteDeadlineLeft    time.Duration
	writesWithoutAnyDeadline int
}

// Forgets the counts so far, such as the upgrade's, but not the deadline the
// socket holds.
func (self *h1LivenessConn) resetDeadlineStats() {
	self.deadlineStatsLock.Lock()
	defer self.deadlineStatsLock.Unlock()
	self.deadlineStats = h1LivenessDeadlineStats{writeDeadline: self.deadlineStats.writeDeadline}
}

// The counts since the last reset.
func (self *h1LivenessConn) deadlineStatsSnapshot() h1LivenessDeadlineStats {
	self.deadlineStatsLock.Lock()
	defer self.deadlineStatsLock.Unlock()
	return self.deadlineStats
}

// Records the write deadline in force as a write begins.
func (self *h1LivenessConn) noteWriteBegin() {
	self.deadlineStatsLock.Lock()
	defer self.deadlineStatsLock.Unlock()
	stats := &self.deadlineStats
	stats.writeCount += 1
	if stats.writeDeadline.IsZero() {
		stats.writesWithoutAnyDeadline += 1
		return
	}
	left := time.Until(stats.writeDeadline)
	if stats.writeCount-stats.writesWithoutAnyDeadline == 1 {
		// the first write with a deadline
		stats.leastWriteDeadlineLeft = left
		stats.mostWriteDeadlineLeft = left
	} else {
		stats.leastWriteDeadlineLeft = min(stats.leastWriteDeadlineLeft, left)
		stats.mostWriteDeadlineLeft = max(stats.mostWriteDeadlineLeft, left)
	}
}

// The upgrade installs both deadlines together; count only what it leaves.
func (self *h1LivenessConn) SetDeadline(deadline time.Time) error {
	if err := self.Conn.SetDeadline(deadline); err != nil {
		return err
	}
	self.deadlineStatsLock.Lock()
	defer self.deadlineStatsLock.Unlock()
	self.deadlineStats.writeDeadline = deadline
	return nil
}

// Successful deadline installation delegates to the same bounded pipe.
func (self *h1LivenessConn) SetReadDeadline(deadline time.Time) error {
	func() {
		self.deadlineStatsLock.Lock()
		defer self.deadlineStatsLock.Unlock()
		self.deadlineStats.readDeadlineCount += 1
	}()
	if self.failReadDeadline.Load() {
		self.readRejected.Store(true)
		return errors.New("synthetic read deadline rejected")
	}
	return self.Conn.SetReadDeadline(deadline)
}

// A rejected writer deadline must not permit the heartbeat write to proceed.
func (self *h1LivenessConn) SetWriteDeadline(deadline time.Time) error {
	func() {
		self.deadlineStatsLock.Lock()
		defer self.deadlineStatsLock.Unlock()
		self.deadlineStats.writeDeadlineCount += 1
	}()
	if self.failWriteDeadline.Load() {
		self.writeRejected.Store(true)
		return errors.New("synthetic write deadline rejected")
	}
	if err := self.Conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	self.deadlineStatsLock.Lock()
	defer self.deadlineStatsLock.Unlock()
	self.deadlineStats.writeDeadline = deadline
	return nil
}

// Counts only a new read begun after a deadline error, not an in-flight read.
func (self *h1LivenessConn) Read(message []byte) (int, error) {
	if self.readRejected.Load() {
		self.readsAfterRejection.Add(1)
	}
	return self.Conn.Read(message)
}

// The ordinary failure case tests the real writer's existing terminal path.
func (self *h1LivenessConn) Write(message []byte) (int, error) {
	if self.writeRejected.Load() {
		self.writesAfterRejection.Add(1)
	}
	if self.failWrite.Load() {
		return 0, errors.New("synthetic socket write failed")
	}
	self.noteWriteBegin()
	self.writesInProgress.Add(1)
	defer self.writesInProgress.Add(-1)
	n, err := self.Conn.Write(message)
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		self.writeTimedOut.Store(true)
	}
	return n, err
}

// Socket closure is an explicit witness independent of the route counters.
func (self *h1LivenessConn) Close() error {
	err := self.Conn.Close()
	self.closeOnce.Do(func() { close(self.closed) })
	return err
}

// Each fixture owns its strategy, transport budget, peer, and both pipe ends.
// stateLock guards payloadMessages, which the peer reader appends to.
type h1LivenessFixture struct {
	transport        *PlatformTransport
	routes           *RouteManager
	send             Route
	writer           MultiRouteWriter
	stats            *H1PlusStats
	connectionStats  *H1ConnectionStats
	client           *h1LivenessConn
	peer             H1MessageConn // H1+, or WebSocket with EnableH1Plus off
	webSocket        bool
	peerDone         chan struct{}
	peerReadBlocked  chan struct{}
	peerReadResume   chan struct{}
	heartbeats       atomic.Int64
	payloads         atomic.Int64
	payloadByteCount atomic.Int64
	registered       atomic.Int64
	withdrawn        atomic.Int64
	strategy         *ClientStrategy
	cancel           context.CancelFunc
	stateLock        sync.Mutex
	payloadMessages  [][]byte
	// when the peer read each heartbeat
	heartbeatTimes []time.Time
}

// The non-empty messages the peer has read, in the order it read them.
func (self *h1LivenessFixture) payloadMessagesSnapshot() [][]byte {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([][]byte(nil), self.payloadMessages...)
}

// When the peer read each heartbeat, in order.
func (self *h1LivenessFixture) heartbeatTimesSnapshot() []time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]time.Time(nil), self.heartbeatTimes...)
}

// Hands the hijacked pipe to the WebSocket upgrader with the request reader,
// which has buffered nothing past the request.
type h1LivenessHijackWriter struct {
	header http.Header
	conn   net.Conn
	reader *bufio.Reader
}

// The upgrader sets its response headers here before it hijacks.
func (self *h1LivenessHijackWriter) Header() http.Header {
	return self.header
}

// The upgrader writes its response to the hijacked connection, never here.
func (self *h1LivenessHijackWriter) Write(message []byte) (int, error) {
	return 0, errors.New("synthetic upgrade wrote outside the hijacked connection")
}

// Only a failed upgrade sets a status, and that fails the fixture anyway.
func (self *h1LivenessHijackWriter) WriteHeader(int) {}

// Returns the pipe and the request reader.
func (self *h1LivenessHijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return self.conn, bufio.NewReadWriter(self.reader, bufio.NewWriter(self.conn)), nil
}

// Takes the message on admission through the same selector as real clients.
// The observer's raw route is retained only for topology assertions.
func (self *h1LivenessFixture) sendMessage(t *testing.T, message []byte) {
	t.Helper()
	if accepted, err := self.writer.WriteDetailed(self.transport.ctx, message, 0); !accepted || err != nil {
		MessagePoolReturn(message)
		t.Fatalf("synthetic h1 selector admission: accepted=%t err=%v", accepted, err)
	}
}

// Call inside a synctest bubble after priming the package's lazy pool outside.
// No DNS, TLS, listening socket, host configuration or production identity is used.
func newH1LivenessFixture(t *testing.T, configure ...func(*PlatformTransportSettings)) *h1LivenessFixture {
	t.Helper()
	return newH1LivenessFixtureWithBackpressure(t, false, 0, configure...)
}

// A gate after one complete peer read applies actual pipe backpressure while
// keeping that peer's sole writer available for independent heartbeats.
func newH1LivenessFixtureWithBackpressure(t *testing.T, pausePeer bool, writeTimeout time.Duration, configure ...func(*PlatformTransportSettings)) *h1LivenessFixture {
	t.Helper()
	return newH1LivenessFixtureWithRoutes(t, pausePeer, writeTimeout, nil, configure...)
}

// A real Client may supply its owned route manager while the physical owner
// keeps its independent cancellation lifetime, as in provider migration.
func newH1LivenessFixtureWithRoutes(t *testing.T, pausePeer bool, writeTimeout time.Duration, routes *RouteManager, configure ...func(*PlatformTransportSettings)) *h1LivenessFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	client, peer := net.Pipe()
	fixture := &h1LivenessFixture{
		client:          &h1LivenessConn{Conn: client, closed: make(chan struct{})},
		stats:           &H1PlusStats{},
		connectionStats: &H1ConnectionStats{},
		peerDone:        make(chan struct{}),
		peerReadBlocked: make(chan struct{}),
		peerReadResume:  make(chan struct{}),
		cancel:          cancel,
	}
	ready := make(chan H1MessageConn, 1)
	peerError := make(chan error, 1)
	go func() {
		defer close(fixture.peerDone)
		defer peer.Close()
		reader := bufio.NewReader(peer)
		request, err := http.ReadRequest(reader)
		if err != nil {
			peerError <- err
			return
		}
		var peerConn H1MessageConn
		switch request.Header.Get("Upgrade") {
		case H1FramerProtocol:
			if _, err := fmt.Fprintf(peer, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: %s\r\n\r\n", H1FramerProtocol); err != nil {
				peerError <- err
				return
			}
			framed, err := NewFramedMessageConn(&httpUpgradeConn{Conn: peer, reader: reader}, H1FramerProtocol, 65535, nil)
			if err != nil {
				peerError <- err
				return
			}
			peerConn = framed
		case "websocket":
			upgrader := websocket.Upgrader{}
			ws, err := upgrader.Upgrade(&h1LivenessHijackWriter{header: http.Header{}, conn: peer, reader: reader}, request, nil)
			if err != nil {
				peerError <- err
				return
			}
			ws.SetReadLimit(65535)
			fixture.webSocket = true
			peerConn = ws
		default:
			peerError <- errors.New("synthetic peer did not receive an H1+ or WebSocket upgrade")
			return
		}
		ready <- peerConn
		messageCount := 0
		for {
			_, message, err := peerConn.ReadMessage()
			if err != nil {
				return
			}
			if len(message) == 0 {
				fixture.heartbeats.Add(1)
				fixture.stateLock.Lock()
				fixture.heartbeatTimes = append(fixture.heartbeatTimes, time.Now())
				fixture.stateLock.Unlock()
			} else {
				fixture.payloads.Add(1)
				fixture.payloadByteCount.Add(int64(len(message)))
				fixture.stateLock.Lock()
				fixture.payloadMessages = append(fixture.payloadMessages, append([]byte(nil), message...))
				fixture.stateLock.Unlock()
			}
			messageCount++
			if pausePeer && messageCount == 1 {
				close(fixture.peerReadBlocked)
				select {
				case <-ctx.Done():
					return
				case <-fixture.peerReadResume:
				}
			}
		}
	}()
	strategySettings := DefaultClientStrategySettings()
	strategySettings.Log = NewNoopLogger()
	strategySettings.EnableNormal = true
	strategySettings.EnableResilient = false
	strategySettings.ParallelBlockSize = 1
	strategySettings.ExpandExtenderProfileCount = 0
	strategySettings.ExtenderConfigs = nil
	strategySettings.DnsTlds = nil
	strategySettings.InternalDohDomains = nil
	strategySettings.MinNextConnectDelay = 0
	strategySettings.MaxNextConnectDelay = 0
	var dialed atomic.Bool
	strategySettings.DialContextSettings = &DialContextSettings{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if !dialed.Swap(true) {
				return fixture.client, nil
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	fixture.strategy = NewClientStrategy(ctx, strategySettings)
	settings := DefaultPlatformTransportSettings()
	settings.Log = NewNoopLogger()
	settings.EnableH1Plus = true
	settings.V2H1Auth = true
	settings.ReadTimeout = 3 * time.Second
	settings.PingTimeout = time.Second
	settings.WriteTimeout = 2 * time.Second
	if writeTimeout > 0 {
		settings.WriteTimeout = writeTimeout
	}
	settings.ReconnectTimeout = time.Hour
	settings.TransportBufferSize = 1
	settings.H1PlusStats = fixture.stats
	settings.H1ConnectionStats = fixture.connectionStats
	settings.SendRouteObserver = func(_ Transport, route Route, active bool) {
		if active {
			fixture.send = route
			fixture.registered.Add(1)
		} else {
			fixture.withdrawn.Add(1)
		}
	}
	for _, apply := range configure {
		apply(settings)
	}
	fixture.routes = routes
	if fixture.routes == nil {
		fixture.routes = NewRouteManagerWithLogger(ctx, "synthetic-h1-liveness", NewNoopLogger())
	}
	fixture.writer = fixture.routes.OpenMultiRouteWriter(DestinationId(NewId()))
	fixture.transport = NewPlatformTransportWithTargetMode(ctx, fixture.strategy, fixture.routes,
		"ws://liveness.example/endpoint", &ClientAuth{ByJwt: "synthetic-test-token", InstanceId: NewId()},
		TransportModeH1, settings)
	t.Cleanup(func() {
		fixture.routes.CloseMultiRouteWriter(fixture.writer)
		fixture.cancel()
		fixture.client.Close()
		peer.Close()
		fixture.strategy.Close()
		if err := fixture.transport.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
		<-fixture.peerDone
	})
	synctest.Wait()
	select {
	case fixture.peer = <-ready:
	case err := <-peerError:
		t.Fatalf("synthetic upgrade: %v", err)
	default:
		t.Fatal("synthetic H1 upgrade did not complete without a timer")
	}
	fixture.assertConnected(t)
	if fixture.webSocket != !settings.EnableH1Plus {
		t.Fatal("synthetic owner did not select the configured H1 carrier")
	}
	if settings.EnableH1Plus && fixture.stats.Snapshot().Accepted != 1 {
		t.Fatal("synthetic owner did not select H1+")
	}
	return fixture
}

// Readiness is checked at both the live carrier and actual route publication.
func (self *h1LivenessFixture) assertConnected(t *testing.T) {
	t.Helper()
	connectionStats := H1ConnectionStatsSnapshot{H1PlusConnectionCount: 1}
	if self.webSocket {
		connectionStats = H1ConnectionStatsSnapshot{WebSocketConnectionCount: 1}
	}
	if !self.transport.IsConnected() || !self.routes.HasActiveTransport() ||
		self.connectionStats.Snapshot() != connectionStats ||
		self.registered.Load() != 1 || self.withdrawn.Load() != 0 {
		t.Fatal("healthy H1+ carrier was not registered exactly once")
	}
}

// Terminal lifecycle must retire routes and close the socket, not just log.
func (self *h1LivenessFixture) assertWithdrawn(t *testing.T) {
	t.Helper()
	if self.transport.IsConnected() || self.routes.HasActiveTransport() ||
		self.connectionStats.Snapshot() != (H1ConnectionStatsSnapshot{}) || self.withdrawn.Load() != 1 {
		t.Error("failed H1+ carrier retained an active route or connection statistic")
	}
	select {
	case <-self.client.closed:
	default:
		t.Error("failed H1+ carrier retained its socket")
	}
}

// Empty binary frames refresh the reader and do not become application data.
func TestH1PlusLivenessIdleHeartbeatsStayConnected(t *testing.T) {
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newH1LivenessFixture(t)
		for range 5 {
			time.Sleep(time.Second)
			if err := fixture.peer.WriteMessage(websocket.BinaryMessage, nil); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
			fixture.assertConnected(t)
		}
		if fixture.heartbeats.Load() < 5 || fixture.payloads.Load() != 0 || fixture.stats.Snapshot().Bytes != 0 {
			t.Fatal("idle heartbeat was absent or counted as application payload")
		}
		if fixture.transport.ReceiveStats().H1 != (PlatformTransportReceiveModeStatsSnapshot{}) {
			t.Fatal("empty heartbeat entered the application receive queue")
		}
	})
}

// Successful outbound heartbeats do not certify inbound peer reachability.
func TestH1PlusLivenessInboundBlackholeExpiresReadDeadline(t *testing.T) {
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newH1LivenessFixture(t)
		time.Sleep(3*time.Second - time.Nanosecond)
		synctest.Wait()
		fixture.assertConnected(t)
		if fixture.heartbeats.Load() < 2 {
			t.Fatal("blackhole control did not keep outbound writes healthy")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		fixture.assertWithdrawn(t)
	})
}

// A missed tick is not fatal when a complete heartbeat arrives before expiry.
func TestH1PlusLivenessDelayedHeartbeatRefreshesDeadline(t *testing.T) {
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newH1LivenessFixture(t)
		time.Sleep(2 * time.Second)
		if err := fixture.peer.WriteMessage(websocket.BinaryMessage, nil); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		fixture.assertConnected(t)
	})
}

// The serialized heartbeat writer must retire its route on real I/O failure.
func TestH1PlusLivenessWriteFailureWithdrawsRoute(t *testing.T) {
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newH1LivenessFixture(t)
		fixture.client.failWrite.Store(true)
		time.Sleep(time.Second)
		synctest.Wait()
		fixture.assertWithdrawn(t)
	})
}

// EOF is an event: no heartbeat or reconnect timer is needed to withdraw.
func TestH1PlusLivenessPeerEofWithdrawsRoute(t *testing.T) {
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newH1LivenessFixture(t)
		started := time.Now()
		fixture.peer.Close()
		synctest.Wait()
		fixture.assertWithdrawn(t)
		if !time.Now().Equal(started) {
			t.Fatal("peer EOF required a timer before withdrawal")
		}
	})
}

// The actual owner joins every connection worker before Done closes.
func TestH1PlusLivenessCancellationJoinsAndWithdraws(t *testing.T) {
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newH1LivenessFixture(t)
		started := time.Now()
		if err := fixture.transport.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		fixture.assertWithdrawn(t)
		if !time.Now().Equal(started) {
			t.Fatal("local cancellation waited for the read deadline")
		}
	})
}

// A complete reliable frame waiting on local queue capacity is not a dead
// peer. Keep its bounded ownership and make cancellation, not a second reader,
// terminate the wait. A socket deadline applies when socket reading resumes.
func TestH1PlusLivenessReceiveBackpressureIsNotDeadPeer(t *testing.T) {
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newH1LivenessFixture(t)
		for range 2 {
			if err := fixture.peer.WriteMessage(websocket.BinaryMessage, make([]byte, 64)); err != nil {
				t.Fatal(err)
			}
		}
		synctest.Wait()
		if got := fixture.transport.ReceiveStats().H1; got.QueueBackpressureMessageCount != 1 || got.QueueDropMessageCount != 0 {
			t.Fatalf("reliable reader did not retain one bounded frame: %+v", got)
		}
		time.Sleep(4 * time.Second)
		synctest.Wait()
		fixture.assertConnected(t)
		if err := fixture.transport.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.assertWithdrawn(t)
	})
}

// The read watchdog's bound is the reader's only deadline installation. A
// rejected one still ends the read at exactly ReadTimeout: the batch conn
// closes the socket instead, and no socket read begins after the rejection.
func TestH1PlusLivenessReadDeadlineFailureIsTerminal(t *testing.T) {
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	for _, h1Plus := range []bool{true, false} {
		synctest.Test(t, func(t *testing.T) {
			fixture := newH1LivenessFixture(t, withH1Plus(h1Plus))
			fixture.client.failReadDeadline.Store(true)
			readTimeout := fixture.transport.settings.ReadTimeout
			if cutOff := h1WithdrawnAfter(fixture, time.Now(), readTimeout); cutOff != readTimeout {
				t.Fatalf("h1+=%t: the silent peer was cut off %s after its last frame, want exactly %s", h1Plus, cutOff, readTimeout)
			}
			fixture.assertWithdrawn(t)
			if !fixture.client.readRejected.Load() {
				t.Fatalf("h1+=%t: the read watchdog did not install its bound", h1Plus)
			}
			if fixture.client.readsAfterRejection.Load() != 0 {
				t.Errorf("h1+=%t: the reader began socket I/O after its bound was rejected", h1Plus)
			}
		})
	}
}

// The write watchdog's bound is the writer's only deadline installation. For
// the named kind of write, stalled on the paused peer, a rejected one still
// ends the write at exactly WriteTimeout after it began: the batch conn closes
// the socket instead, no socket write begins after the rejection, and the
// owner returns the write's payload.
func testH1StalledWriteRejectedBoundIsTerminal(t *testing.T, kindName string) {
	t.Helper()
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	var kind h1StalledWriteKind
	for _, k := range h1StalledWriteKinds() {
		if k.name == kindName {
			kind = k
		}
	}
	if kind.stall == nil {
		t.Fatalf("no stalled write kind %q", kindName)
	}
	for _, h1Plus := range []bool{true, false} {
		synctest.Test(t, func(t *testing.T) {
			fixture := newH1LivenessFixtureWithBackpressure(t, true, 0, withH1Plus(h1Plus), kind.configure)
			stallStart, witness := kind.stall(t, fixture)
			fixture.client.failWriteDeadline.Store(true)
			writeTimeout := fixture.transport.settings.WriteTimeout
			if cutOff := h1WithdrawnAfter(fixture, stallStart, writeTimeout); cutOff != writeTimeout {
				t.Fatalf("h1+=%t: the stalled %s ended %s after it began, want exactly %s", h1Plus, kind.name, cutOff, writeTimeout)
			}
			fixture.assertWithdrawn(t)
			if !fixture.client.writeRejected.Load() {
				t.Fatalf("h1+=%t: the write watchdog did not install its bound under the %s", h1Plus, kind.name)
			}
			if fixture.client.writesAfterRejection.Load() != 0 {
				t.Errorf("h1+=%t: a socket write began after the %s's bound was rejected", h1Plus, kind.name)
			}
			if witness != nil && !MessagePoolReturn(witness) {
				t.Errorf("h1+=%t: the rejected bound retained the %s's payload", h1Plus, kind.name)
			}
		})
	}
}

// The heartbeat writer must not block forever behind a rejected bound.
func TestH1PlusLivenessWriteDeadlineFailureIsTerminal(t *testing.T) {
	testH1StalledWriteRejectedBoundIsTerminal(t, "ping")
}

// Control echoes share the serialized writer and its bound.
func TestH1PlusLivenessControlDeadlineFailureIsTerminal(t *testing.T) {
	testH1StalledWriteRejectedBoundIsTerminal(t, "control echo")
}

// Speed mode has a distinct heartbeat select branch; the same failure policy
// must hold there without changing the speed-control wire format.
func TestH1PlusLivenessSpeedHeartbeatDeadlineFailureIsTerminal(t *testing.T) {
	testH1StalledWriteRejectedBoundIsTerminal(t, "speed test ping")
}

// The non-batching speed-mode sender owns its pooled payload on this exit too.
// The witness is released only after the owner returned.
func TestH1PlusLivenessSpeedPayloadDeadlineReturnsOwner(t *testing.T) {
	testH1StalledWriteRejectedBoundIsTerminal(t, "speed test payload")
}

// Ready batches, framed and WebSocket, fail closed the same way, and return
// every pooled payload.
func TestH1PlusLivenessReadyBatchDeadlineAlreadyFailsClosed(t *testing.T) {
	testH1StalledWriteRejectedBoundIsTerminal(t, "ready batch payload")
}
