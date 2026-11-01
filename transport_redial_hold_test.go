package connect

import (
	"context"
	"crypto/tls"
	"sync/atomic"
	"testing"
	"time"
)

// The re-dial spread of a lost connection (ReconnectJitter) against the
// transport's holds: the owner's switch (SetEnabled) and the pinned family's
// hold, which the kick monitor also signals (updateHold). A hold is this
// device's own stop, not a fleet-wide loss:
//   - a hold that lands while a runner waits to dial must send it back to
//     admission, not let it dial while held, and a connection a dial produced
//     while held must not survive;
//   - a connection a hold closed, and a dial after a park in admission, are
//     no lost connection's re-dial, so they draw no spread;
//   - the spread is waited out before any shared pacing is taken, so a
//     fast-path slot is not held through it, a staircase lead is not stacked
//     on it, and a staircase step a hold abandons is given back.

// One pre-dial wait a runner asked for: its delay, and the strategy's
// fast-path slots and staircase lead at that moment.
type testingPacingWait struct {
	delay         time.Duration
	fastPathCount int
	staircaseLead time.Duration
	release       chan time.Time
}

// Records every pre-dial wait of the runners whose settings carry `after`,
// reading `strategy` at each. While holding, the recorder leaves each wait
// for the test to end; otherwise each ends at once. Safe for concurrent use.
type testingPacingWaits struct {
	strategy *ClientStrategy
	hold     atomic.Bool
	waits    chan *testingPacingWait
}

func newTestingPacingWaits(strategy *ClientStrategy, hold bool) *testingPacingWaits {
	waits := &testingPacingWaits{
		strategy: strategy,
		waits:    make(chan *testingPacingWait, 256),
	}
	waits.hold.Store(hold)
	return waits
}

func (self *testingPacingWaits) after(delay time.Duration) <-chan time.Time {
	wait := &testingPacingWait{
		delay:   delay,
		release: make(chan time.Time, 1),
	}
	func() {
		self.strategy.mutex.Lock()
		defer self.strategy.mutex.Unlock()
		wait.fastPathCount = self.strategy.reconnectFastPathCount
		wait.staircaseLead = time.Until(self.strategy.nextConnectTime)
	}()
	if !self.hold.Load() {
		wait.release <- time.Now()
	}
	self.waits <- wait
	return wait.release
}

// next is the next wait a runner asked for, or fails.
func (self *testingPacingWaits) next(t *testing.T) *testingPacingWait {
	t.Helper()
	select {
	case wait := <-self.waits:
		return wait
	case <-time.After(15 * time.Second):
		t.Fatal("the runner never waited to dial")
		return nil
	}
}

// none fails when a runner already asked for a wait.
func (self *testingPacingWaits) none(t *testing.T) {
	t.Helper()
	select {
	case wait := <-self.waits:
		t.Fatalf("unexpected pre-dial wait of %s", wait.delay)
	default:
	}
}

// A family-agnostic H1 transport over its own strategy, so a test can read
// and shape the strategy's pacing.
func newRedialTestTransport(
	t *testing.T,
	ctx context.Context,
	platformUrl string,
	strategy *ClientStrategy,
	settings *PlatformTransportSettings,
) *PlatformTransport {
	t.Helper()
	transport := NewPlatformTransportWithTargetMode(
		ctx,
		strategy,
		NewRouteManager(ctx, "redial"),
		platformUrl,
		&ClientAuth{
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

// Signals each park of a runner on the owner's or the family's hold.
func observeTestingHeldParks(settings *PlatformTransportSettings) chan struct{} {
	parked := make(chan struct{}, 64)
	settings.heldParkForTest = func() {
		select {
		case parked <- struct{}{}:
		default:
		}
	}
	return parked
}

func waitTestingConnected(t *testing.T, connections *MonitorValue[testingTransportConnectionCounts], connected int) {
	t.Helper()
	if counts, ok := waitTestingTransportConnections(connections, 15*time.Second, func(counts testingTransportConnectionCounts) bool {
		return connected <= counts.connected
	}); !ok {
		t.Fatalf("connections = %+v, want %d connected", counts, connected)
	}
}

// Waits for the runner to park on the hold. A connection that the transport
// registers first is a dial while held, and fails the test.
func waitTestingHeldParkWithoutDial(
	t *testing.T,
	parked chan struct{},
	connections *MonitorValue[testingTransportConnectionCounts],
	connected int,
) {
	t.Helper()
	timeout := time.After(15 * time.Second)
	for {
		counts, notify := connections.Get()
		if connected < counts.connected {
			t.Fatalf("the transport dialed and connected while held: connections = %+v", counts)
		}
		select {
		case <-parked:
			if counts := connections.Value(); connected < counts.connected {
				t.Fatalf("the transport dialed and connected while held: connections = %+v", counts)
			}
			return
		case <-notify:
		case <-timeout:
			t.Fatal("the runner never parked on the hold")
		}
	}
}

func testingDialFailureCount(backoff *pinnedDialBackoff) int {
	backoff.stateLock.Lock()
	defer backoff.stateLock.Unlock()
	return backoff.failures
}

// Finding 1: the owner holds the transport while its runner waits out a lost
// connection's spread. The kick monitor wakes the wait, and the runner used to
// take any wake that was not a kick for the end of the wait and dial, held,
// with a connection that the hold's watch never closed. It parks instead.
func TestHoldDuringRedialSpreadParksWithoutDialing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	platform := newTestingPlatformServer(t)
	strategy := NewClientStrategy(ctx, DefaultClientStrategySettings())
	defer strategy.Close()
	settings := testingPlatformTransportSettings()
	settings.ReconnectJitter = 10 * time.Second
	draws := newTestingJitterDraws(5 * time.Second)
	settings.reconnectJitterForTest = draws.draw
	waits := newTestingPacingWaits(strategy, true)
	settings.pacingAfterForTest = waits.after
	parked := observeTestingHeldParks(settings)
	connections := observeTestingTransportConnections(settings)
	transport := newRedialTestTransport(t, ctx, platform.url, strategy, settings)

	waitTestingConnected(t, connections, 1)
	// the cold dial clamps the staircase to now and waits for nothing
	waits.none(t)

	// the platform restarts, and the runner waits out its spread
	platform.closeConns()
	waits.next(t)
	requireDrawWindows(t, draws, 1, settings.ReconnectJitter)

	transport.SetEnabled(false)
	waitTestingHeldParkWithoutDial(t, parked, connections, 1)
	if transport.IsConnected() {
		t.Fatal("a held transport is connected")
	}
	AssertEqual(t, transport.State(), PlatformTransportStateDisabled)
}

// Finding 1, the connection side: a connection that a dial produced after the
// hold began is closed at once. The watch read the hold as an edge on the kick
// monitor, which had already fired, so such a connection outlived the hold.
func TestConnectionWatchClosesTheConnectionOfAHeldTransport(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := testingPlatformTransportSettings()
	settings.StartDisabled = true
	transport := NewPlatformTransportWithTargetMode(
		ctx,
		NewClientStrategyWithDefaults(ctx),
		NewRouteManager(ctx, "held-watch"),
		"wss://127.0.0.1:1",
		&ClientAuth{ByJwt: "testing", InstanceId: NewId(), AppVersion: "testing"},
		TransportModeH1,
		settings,
	)
	defer transport.Close()

	watchCtx, watchCancel := context.WithCancel(ctx)
	defer watchCancel()
	closed := make(chan string, 1)
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		// a kick channel taken after the hold fired: it never closes
		transport.runConnectionWatch(watchCtx, make(chan struct{}), func(reason string) {
			closed <- reason
		})
	}()
	select {
	case reason := <-closed:
		AssertEqual(t, reason, connectionCloseHeld)
	case <-time.After(5 * time.Second):
		t.Fatal("the watch did not close the connection of a held transport")
	}
	watchCancel()
	<-watchDone
}

// Finding 2: the owner's switch closes a live connection and opens again. The
// close was taken for a lost connection, so the dial after the re-enable drew
// the spread on top of the owner's own wait (a provider's standby: up to
// StandbyDelay plus ReconnectJitter). It is a fresh start.
func TestReenabledTransportRedialsWithoutTheSpread(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	platform := newTestingPlatformServer(t)
	strategy := NewClientStrategy(ctx, DefaultClientStrategySettings())
	defer strategy.Close()
	settings := testingPlatformTransportSettings()
	settings.ReconnectJitter = 10 * time.Second
	draws := newTestingJitterDraws(300 * time.Millisecond)
	settings.reconnectJitterForTest = draws.draw
	parked := observeTestingHeldParks(settings)
	connections := observeTestingTransportConnections(settings)
	transport := newRedialTestTransport(t, ctx, platform.url, strategy, settings)

	waitTestingConnected(t, connections, 1)
	transport.SetEnabled(false)
	waitTestingHeldParkWithoutDial(t, parked, connections, 1)
	if !waitForCondition(15*time.Second, func() bool { return !transport.IsConnected() }) {
		t.Fatal("the hold did not close the live connection")
	}

	transport.SetEnabled(true)
	waitTestingConnected(t, connections, 2)
	requireDrawWindows(t, draws, 0, settings.ReconnectJitter)
}

// Finding 2, after a failed dial: a hold that wakes the runner from its
// failure backoff sends it back to admission, and the dial after the park is
// no lost connection's re-dial. It used to be marked one, and drew the spread.
func TestHoldAfterFailedDialRedialsWithoutTheSpread(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	platform := newTestingPlatformServer(t)
	platform.rejecting.Store(true)
	strategySettings := DefaultClientStrategySettings()
	// a refused dial fails once the strategy's request deadline passes
	strategySettings.RequestTimeout = time.Second
	strategy := NewClientStrategy(ctx, strategySettings)
	defer strategy.Close()
	settings := testingPlatformTransportSettings()
	settings.StartDisabled = true
	settings.ReconnectTimeout = 5 * time.Second
	settings.ReconnectJitter = 10 * time.Second
	draws := newTestingJitterDraws(300 * time.Millisecond)
	settings.reconnectJitterForTest = draws.draw
	parked := observeTestingHeldParks(settings)
	connections := observeTestingTransportConnections(settings)
	transport := newRedialTestTransport(t, ctx, platform.url, strategy, settings)
	// two earlier failures: the next one backs off at least 5 s, so the hold
	// below lands in that wait
	transport.failBackoff.fail()
	transport.failBackoff.fail()

	transport.SetEnabled(true)
	if !waitForCondition(15*time.Second, func() bool {
		return 3 <= testingDialFailureCount(transport.failBackoff)
	}) {
		t.Fatal("the dial to a refusing platform never failed")
	}
	// the parks while the transport started disabled
	for drained := false; !drained; {
		select {
		case <-parked:
		default:
			drained = true
		}
	}
	transport.SetEnabled(false)
	waitTestingHeldParkWithoutDial(t, parked, connections, 0)

	platform.rejecting.Store(false)
	transport.SetEnabled(true)
	waitTestingConnected(t, connections, 1)
	requireDrawWindows(t, draws, 0, settings.ReconnectJitter)
}

// Finding 3: the spread is waited out before the strategy's fast-path slot is
// taken. The slot used to be held through the spread and the dial, so up to
// ReconnectFastPathLimit lost connections on one strategy held every slot for
// seconds and pushed the next to the staircase.
func TestRedialSpreadIsWaitedBeforeTakingAFastPathSlot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	platform := newTestingPlatformServer(t)
	strategySettings := DefaultClientStrategySettings()
	strategy := NewClientStrategy(ctx, strategySettings)
	defer strategy.Close()
	const spread = 3 * time.Second
	settings := testingPlatformTransportSettings()
	settings.ReconnectJitter = 10 * time.Second
	draws := newTestingJitterDraws(spread)
	settings.reconnectJitterForTest = draws.draw
	waits := newTestingPacingWaits(strategy, true)
	settings.pacingAfterForTest = waits.after
	connections := observeTestingTransportConnections(settings)
	newRedialTestTransport(t, ctx, platform.url, strategy, settings)

	waitTestingConnected(t, connections, 1)
	waits.none(t)

	platform.closeConns()
	spreadWait := waits.next(t)
	if spreadWait.fastPathCount != 0 {
		t.Fatalf("the runner held %d fast-path slots through its spread", spreadWait.fastPathCount)
	}
	AssertEqual(t, spreadWait.delay, spread)
	// the waits after the spread end at once
	waits.hold.Store(false)
	spreadWait.release <- time.Now()

	waitTestingConnected(t, connections, 2)
	// the fast path's own jitter may be a wait of its own, which holds the slot
	select {
	case fastPathWait := <-waits.waits:
		if fastPathWait.fastPathCount != 1 {
			t.Fatalf("fast-path wait with %d slots held, want 1", fastPathWait.fastPathCount)
		}
		if strategySettings.ReconnectFastPathMaxDelay <= fastPathWait.delay {
			t.Fatalf("fast-path wait %s, want under %s", fastPathWait.delay, strategySettings.ReconnectFastPathMaxDelay)
		}
	default:
	}
	// the slot is free again once the dial completed
	if !waitForCondition(5*time.Second, func() bool {
		strategy.mutex.Lock()
		defer strategy.mutex.Unlock()
		return strategy.reconnectFastPathCount == 0
	}) {
		t.Fatal("the fast-path slot was not released after the dial")
	}
}

// Finding 3, past the fast path's cap: a re-dial past the cap takes a
// staircase step, which nextConnectMaxLead bounds. The spread used to be added
// to that lead, so one wait ran up to the lead plus the spread (14 s here).
// Each wait now stays within the bound.
func TestOverCapRedialSpreadDoesNotStackOnTheStaircase(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	platform := newTestingPlatformServer(t)
	strategySettings := DefaultClientStrategySettings()
	// every re-dial is past the cap and takes a whole one-second step
	strategySettings.ReconnectFastPathLimit = 0
	strategySettings.MinNextConnectDelay = time.Second
	strategySettings.MaxNextConnectDelay = time.Second
	strategy := NewClientStrategy(ctx, strategySettings)
	defer strategy.Close()
	settings := testingPlatformTransportSettings()
	settings.ReconnectJitter = 10 * time.Second
	draws := newTestingJitterDraws(4 * time.Second)
	settings.reconnectJitterForTest = draws.draw
	waits := newTestingPacingWaits(strategy, false)
	settings.pacingAfterForTest = waits.after
	connections := observeTestingTransportConnections(settings)
	newRedialTestTransport(t, ctx, platform.url, strategy, settings)

	waitTestingConnected(t, connections, 1)
	// the staircase is as deep as it may get: other transports on the
	// strategy are queued to its lead bound
	for i := 0; i < 20; i += 1 {
		strategy.NextConnectTime()
	}
	for {
		select {
		case <-waits.waits:
			continue
		default:
		}
		break
	}

	platform.closeConns()
	waitTestingConnected(t, connections, 2)
	waitCount := 0
	for {
		select {
		case wait := <-waits.waits:
			waitCount += 1
			if nextConnectMaxLead+50*time.Millisecond < wait.delay {
				t.Fatalf("a re-dial waited %s at once, past the %s staircase bound", wait.delay, nextConnectMaxLead)
			}
			continue
		default:
		}
		break
	}
	if waitCount == 0 {
		t.Fatal("the re-dial waited for nothing")
	}
	requireDrawWindows(t, draws, 1, settings.ReconnectJitter)
}

// Finding 3, the leak: a staircase step taken past the fast path's cap is
// given back when a hold ends the wait before the dial. It was kept, so the
// shared staircase stayed a step deeper for every later dial.
func TestOverCapRedialGivesItsStepBackWhenHeld(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	platform := newTestingPlatformServer(t)
	strategySettings := DefaultClientStrategySettings()
	strategySettings.ReconnectFastPathLimit = 0
	strategySettings.MinNextConnectDelay = time.Second
	strategySettings.MaxNextConnectDelay = time.Second
	strategy := NewClientStrategy(ctx, strategySettings)
	defer strategy.Close()
	settings := testingPlatformTransportSettings()
	// the step alone: no spread, so the only wait is the staircase's
	settings.ReconnectJitter = 0
	waits := newTestingPacingWaits(strategy, true)
	settings.pacingAfterForTest = waits.after
	parked := observeTestingHeldParks(settings)
	connections := observeTestingTransportConnections(settings)
	transport := newRedialTestTransport(t, ctx, platform.url, strategy, settings)

	waitTestingConnected(t, connections, 1)
	waits.none(t)
	// other transports on the strategy are queued a few steps deep, so the
	// re-dial's step is a wait however long the test took to get here
	for i := 0; i < 4; i += 1 {
		strategy.NextConnectTime()
	}
	readNextConnectTime := func() time.Time {
		strategy.mutex.Lock()
		defer strategy.mutex.Unlock()
		return strategy.nextConnectTime
	}
	beforeStep := readNextConnectTime()

	platform.closeConns()
	stepWait := waits.next(t)
	if stepWait.fastPathCount != 0 {
		t.Fatalf("fast-path slots held = %d, want none past the cap", stepWait.fastPathCount)
	}
	afterStep := readNextConnectTime()
	if !afterStep.After(beforeStep) {
		t.Fatalf("the re-dial took no staircase step: %s, then %s", beforeStep, afterStep)
	}

	transport.SetEnabled(false)
	waitTestingHeldParkWithoutDial(t, parked, connections, 1)
	if given := readNextConnectTime(); !given.Equal(afterStep.Add(-time.Second)) {
		t.Fatalf("staircase at %s after the held wait, want the step %s given back to %s", given, time.Second, afterStep.Add(-time.Second))
	}
}

// Finding 3, the leak on close: a staircase step taken past the fast path's
// cap is given back when the transport closes during the wait. The over-cap
// path dropped its cancel, so a transport torn down while it waited left the
// shared staircase a step deeper for every later dial on the strategy.
func TestOverCapRedialGivesItsStepBackWhenClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	platform := newTestingPlatformServer(t)
	strategySettings := DefaultClientStrategySettings()
	strategySettings.ReconnectFastPathLimit = 0
	strategySettings.MinNextConnectDelay = time.Second
	strategySettings.MaxNextConnectDelay = time.Second
	strategy := NewClientStrategy(ctx, strategySettings)
	defer strategy.Close()
	settings := testingPlatformTransportSettings()
	settings.ReconnectJitter = 0
	waits := newTestingPacingWaits(strategy, true)
	settings.pacingAfterForTest = waits.after
	connections := observeTestingTransportConnections(settings)
	transport := newRedialTestTransport(t, ctx, platform.url, strategy, settings)

	waitTestingConnected(t, connections, 1)
	waits.none(t)
	for i := 0; i < 4; i += 1 {
		strategy.NextConnectTime()
	}
	readNextConnectTime := func() time.Time {
		strategy.mutex.Lock()
		defer strategy.mutex.Unlock()
		return strategy.nextConnectTime
	}

	platform.closeConns()
	waits.next(t)
	afterStep := readNextConnectTime()

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer closeCancel()
	if err := transport.CloseAndWait(closeCtx); err != nil {
		t.Fatalf("the transport did not close: %v", err)
	}
	if given := readNextConnectTime(); !given.Equal(afterStep.Add(-time.Second)) {
		t.Fatalf("staircase at %s after the closed wait, want the step %s given back to %s", given, time.Second, afterStep.Add(-time.Second))
	}
}

// A hold cancels a dial in flight (cancelOnKick shares the kick monitor), and
// that cancellation is the owner's or the family's stop, not a dial failure.
// It was counted as one: it grew the transport's failure backoff and fed the
// process-wide degraded-backend gate, which holds contract requests and
// window growth for every client in the process. A provider's standby is
// disabled this way whenever a pin connects while the standby dials.
func TestHoldCanceledDialIsNoDialFailure(t *testing.T) {
	resetBackendDegraded()
	defer resetBackendDegraded()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	platform, held := newTestingPlatformServerHoldingUpgrades(t, 4)
	strategy := NewClientStrategy(ctx, DefaultClientStrategySettings())
	defer strategy.Close()
	settings := testingPlatformTransportSettings()
	parked := observeTestingHeldParks(settings)
	connections := observeTestingTransportConnections(settings)
	transport := newRedialTestTransport(t, ctx, platform.url, strategy, settings)

	// the dial parks in the platform's upgrade until the test ends
	select {
	case <-held:
	case <-time.After(15 * time.Second):
		t.Fatal("the transport never dialed")
	}
	transport.SetEnabled(false)
	waitTestingHeldParkWithoutDial(t, parked, connections, 0)

	if failures := testingDialFailureCount(transport.failBackoff); failures != 0 {
		t.Fatalf("a hold-canceled dial counted %d dial failures", failures)
	}
	if n := consecutiveBackendFails.Load(); n != 0 {
		t.Fatalf("a hold-canceled dial counted %d backend failures", n)
	}
}

// Holds the transport and lifts the hold at once, the first time a
// connection's watch waits, right after the watch read no hold. The hold's
// edge on the kick monitor wakes the watch, and the watch then reads no hold:
// a re-enable within microseconds of the hold, with no scheduling to win.
type testingLiftedHold struct {
	lifted atomic.Bool
}

func (self *testingLiftedHold) beforeWatchWait(transport *PlatformTransport) {
	if self.lifted.CompareAndSwap(false, true) {
		transport.SetEnabled(false)
		transport.SetEnabled(true)
	}
}

// A hold that lifts before the connection's watch reads it. The watch, woken
// by the hold's edge on the kick monitor, reads no hold and closes the
// connection as for a network change. With no kick that close is the hold's,
// not a lost connection, and the re-dial after it draws no spread. It was
// taken for a lost connection and drew one.
func TestHoldLiftedBeforeTheWatchReadsItRedialsWithoutTheSpread(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	platform := newTestingPlatformServer(t)
	strategy := NewClientStrategy(ctx, DefaultClientStrategySettings())
	defer strategy.Close()
	settings := testingPlatformTransportSettings()
	settings.ReconnectJitter = 10 * time.Second
	draws := newTestingJitterDraws(300 * time.Millisecond)
	settings.reconnectJitterForTest = draws.draw
	hold := &testingLiftedHold{}
	settings.connectionWatchWaitForTest = hold.beforeWatchWait
	connections := observeTestingTransportConnections(settings)
	newRedialTestTransport(t, ctx, platform.url, strategy, settings)

	waitTestingConnected(t, connections, 2)
	if !hold.lifted.Load() {
		t.Fatal("the first connection's watch never waited")
	}
	requireDrawWindows(t, draws, 0, settings.ReconnectJitter)
}

// The same for the H3 runner, whose watch closes the QUIC connection. With a
// transport budget an H3 runner runs in the H3 group, which restarts it on any
// hold's edge, so the transport here has none: its explicit H3 runner runs on
// its own and meets the hold itself.
func TestH3HoldLiftedBeforeTheWatchReadsItRedialsWithoutTheSpread(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	platform := newTestingClientLimitH3Platform(t)
	settings := testingFamilyTransportSettings()
	settings.PlatformTransportBudget = nil
	settings.H3Port = platform.port
	settings.QuicConnectTimeout = 2 * time.Second
	settings.QuicHandshakeTimeout = 2 * time.Second
	settings.QuicTlsConfig = &tls.Config{
		InsecureSkipVerify: true, // test-only self-signed endpoint
		NextProtos:         []string{platform.nextProto},
	}
	settings.FramerSettings = platform.framerSets
	settings.ReconnectJitter = 10 * time.Second
	draws := newTestingJitterDraws(300 * time.Millisecond)
	settings.reconnectJitterForTest = draws.draw
	hold := &testingLiftedHold{}
	settings.connectionWatchWaitForTest = hold.beforeWatchWait
	connections := observeTestingTransportConnections(settings)
	strategySettings := DefaultClientStrategySettings()
	strategySettings.EnableNormal = true
	strategySettings.EnableResilient = false
	strategy := NewClientStrategy(ctx, strategySettings)
	defer strategy.Close()
	transport := NewPlatformTransportWithTargetMode(
		ctx,
		strategy,
		NewRouteManager(ctx, "lifted-hold-h3"),
		"https://"+testLoopbackHost(4),
		&ClientAuth{ByJwt: "testing", InstanceId: NewId(), AppVersion: "testing"},
		TransportModeH3,
		settings,
	)
	defer transport.Close()

	platform.nextConnection(t)
	platform.nextConnection(t)
	waitTestingConnected(t, connections, 2)
	if !hold.lifted.Load() {
		t.Fatal("the first connection's watch never waited")
	}
	requireDrawWindows(t, draws, 0, settings.ReconnectJitter)
}
