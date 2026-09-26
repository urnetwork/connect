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
}

// Successful deadline installation delegates to the same bounded pipe.
func (self *h1LivenessConn) SetReadDeadline(deadline time.Time) error {
	if self.failReadDeadline.Load() {
		self.readRejected.Store(true)
		return errors.New("synthetic read deadline rejected")
	}
	return self.Conn.SetReadDeadline(deadline)
}

// A rejected writer deadline must not permit the heartbeat write to proceed.
func (self *h1LivenessConn) SetWriteDeadline(deadline time.Time) error {
	if self.failWriteDeadline.Load() {
		self.writeRejected.Store(true)
		return errors.New("synthetic write deadline rejected")
	}
	return self.Conn.SetWriteDeadline(deadline)
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
type h1LivenessFixture struct {
	transport       *PlatformTransport
	routes          *RouteManager
	send            Route
	stats           *H1PlusStats
	connectionStats *H1ConnectionStats
	client          *h1LivenessConn
	peer            *FramedMessageConn
	peerDone        chan struct{}
	peerReadBlocked chan struct{}
	peerReadResume  chan struct{}
	heartbeats      atomic.Int64
	payloads        atomic.Int64
	registered      atomic.Int64
	withdrawn       atomic.Int64
	strategy        *ClientStrategy
	cancel          context.CancelFunc
}

// Call inside a synctest bubble after priming the package's lazy pool outside.
// No DNS, TLS, listening socket, host configuration or production identity is used.
func newH1LivenessFixture(t *testing.T, configure ...func(*PlatformTransportSettings)) *h1LivenessFixture {
	t.Helper()
	return newH1LivenessFixtureWithBackpressure(t, false, 0)
}

// A gate after one complete peer read applies actual pipe backpressure while
// keeping that peer's sole writer available for independent heartbeats.
func newH1LivenessFixtureWithBackpressure(t *testing.T, pausePeer bool, writeTimeout time.Duration) *h1LivenessFixture {
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
	ready := make(chan *FramedMessageConn, 1)
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
		if request.Header.Get("Upgrade") != H1FramerProtocol {
			peerError <- errors.New("synthetic peer did not receive H1+ upgrade")
			return
		}
		if _, err := fmt.Fprintf(peer, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: %s\r\n\r\n", H1FramerProtocol); err != nil {
			peerError <- err
			return
		}
		framed, err := NewFramedMessageConn(&httpUpgradeConn{Conn: peer, reader: reader}, H1FramerProtocol, 65535, nil)
		if err != nil {
			peerError <- err
			return
		}
		ready <- framed
		messageCount := 0
		for {
			_, message, err := framed.ReadMessage()
			if err != nil {
				return
			}
			if len(message) == 0 {
				fixture.heartbeats.Add(1)
			} else {
				fixture.payloads.Add(1)
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
	fixture.routes = NewRouteManagerWithLogger(ctx, "synthetic-h1-liveness", NewNoopLogger())
	fixture.transport = NewPlatformTransportWithTargetMode(ctx, fixture.strategy, fixture.routes,
		"ws://liveness.example/endpoint", &ClientAuth{ByJwt: "synthetic-test-token", InstanceId: NewId()},
		TransportModeH1, settings)
	t.Cleanup(func() {
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
		t.Fatal("synthetic H1+ upgrade did not complete without a timer")
	}
	fixture.assertConnected(t)
	if fixture.stats.Snapshot().Accepted != 1 {
		t.Fatal("synthetic owner did not select H1+")
	}
	return fixture
}

// Readiness is checked at both the live carrier and actual route publication.
func (self *h1LivenessFixture) assertConnected(t *testing.T) {
	t.Helper()
	if !self.transport.IsConnected() || !self.routes.HasActiveTransport() ||
		self.connectionStats.Snapshot() != (H1ConnectionStatsSnapshot{H1PlusConnectionCount: 1}) ||
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

// The old owner ignored the error and began another unbounded read. This
// forces rejection after a successful upgrade, at the actual read-loop edge.
func TestH1PlusLivenessReadDeadlineFailureIsTerminal(t *testing.T) {
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newH1LivenessFixture(t)
		fixture.client.failReadDeadline.Store(true)
		if err := fixture.peer.WriteMessage(websocket.BinaryMessage, nil); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if !fixture.client.readRejected.Load() {
			t.Fatal("fixture did not reach the rejected read deadline")
		}
		fixture.assertWithdrawn(t)
		if fixture.client.readsAfterRejection.Load() != 0 {
			t.Error("reader began socket I/O after rejecting its deadline")
		}
	})
}

// The old heartbeat writer ignored the deadline error and could block forever.
func TestH1PlusLivenessWriteDeadlineFailureIsTerminal(t *testing.T) {
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newH1LivenessFixture(t)
		fixture.client.failWriteDeadline.Store(true)
		time.Sleep(time.Second)
		synctest.Wait()
		if !fixture.client.writeRejected.Load() {
			t.Fatal("fixture did not reach the rejected write deadline")
		}
		fixture.assertWithdrawn(t)
		if fixture.client.writesAfterRejection.Load() != 0 {
			t.Error("writer began socket I/O after rejecting its deadline")
		}
	})
}

// Control echoes share the serialized writer and must obey the same deadline.
func TestH1PlusLivenessControlDeadlineFailureIsTerminal(t *testing.T) {
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newH1LivenessFixture(t)
		fixture.client.failWriteDeadline.Store(true)
		if err := fixture.peer.WriteMessage(websocket.BinaryMessage, make([]byte, 16)); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if !fixture.client.writeRejected.Load() {
			t.Fatal("control echo did not reach the rejected deadline")
		}
		fixture.assertWithdrawn(t)
		if fixture.client.writesAfterRejection.Load() != 0 {
			t.Error("control echo performed I/O after rejecting its deadline")
		}
	})
}

// Speed mode has a distinct heartbeat select branch; the same failure policy
// must hold there without changing the speed-control wire format.
func TestH1PlusLivenessSpeedHeartbeatDeadlineFailureIsTerminal(t *testing.T) {
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newH1LivenessFixture(t)
		if err := fixture.peer.WriteMessage(websocket.BinaryMessage, []byte{TransportControlSpeedStart, 0, 0, 0, 0}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if fixture.payloads.Load() != 1 {
			t.Fatal("speed-mode control was not echoed before the failure")
		}
		fixture.client.failWriteDeadline.Store(true)
		time.Sleep(time.Second)
		synctest.Wait()
		if !fixture.client.writeRejected.Load() {
			t.Fatal("speed heartbeat did not reach the rejected deadline")
		}
		fixture.assertWithdrawn(t)
		if fixture.client.writesAfterRejection.Load() != 0 {
			t.Error("speed heartbeat performed I/O after rejecting its deadline")
		}
	})
}

// The non-batching speed-mode sender owns its pooled payload even on the new
// deadline-error exit. The witness is released only after the owner returned.
func TestH1PlusLivenessSpeedPayloadDeadlineReturnsOwner(t *testing.T) {
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newH1LivenessFixture(t)
		if err := fixture.peer.WriteMessage(websocket.BinaryMessage, []byte{TransportControlSpeedStart, 0, 0, 0, 0}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if fixture.payloads.Load() != 1 {
			t.Fatal("speed-mode control was not echoed before the failure")
		}
		fixture.client.failWriteDeadline.Store(true)
		message := MessagePoolGet(64)
		witness := MessagePoolShareReadOnly(message)
		fixture.send <- message
		synctest.Wait()
		fixture.assertWithdrawn(t)
		if !MessagePoolReturn(witness) {
			t.Error("deadline rejection retained the speed payload owner")
		}
		if !fixture.client.writeRejected.Load() || fixture.client.writesAfterRejection.Load() != 0 {
			t.Error("speed payload bypassed the rejected deadline")
		}
	})
}

// Ordinary Framed ready batching already checks deadline errors. Preserve
// that healthy negative control and all pooled ownership when fixing pings.
func TestH1PlusLivenessReadyBatchDeadlineAlreadyFailsClosed(t *testing.T) {
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newH1LivenessFixture(t)
		fixture.client.failWriteDeadline.Store(true)
		message := MessagePoolGet(64)
		witness := MessagePoolShareReadOnly(message)
		fixture.send <- message
		synctest.Wait()
		fixture.assertWithdrawn(t)
		if !MessagePoolReturn(witness) {
			t.Error("ready batch retained its payload owner")
		}
		if !fixture.client.writeRejected.Load() || fixture.client.writesAfterRejection.Load() != 0 {
			t.Error("ready batch bypassed the rejected deadline")
		}
	})
}
