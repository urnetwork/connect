package connect

// transport_goroutine_joins_test.go — every goroutine the client strategy,
// the platform transport, its H3 dials and the family transport group start
// is joined by its owner's close: the strategy's context goroutine by Close, a
// dial's kick watch by its stop, the H3 mode group's runners by each pass, an
// H3 race's dials and losers by the race, an H3 dial attempt's handshake watch
// by the attempt's close, and the family group's transports and owned
// strategies by its Done, while the group's run loop and Done start none of
// their own.
//
// The goroutine checks read the process's goroutine profile and count only
// the goroutines labeled for the test, by a frame each goroutine has returned
// from before its owner's join can end. So a goroutine another test left
// running is not counted, and neither is one already past its join and
// unwinding. Each check first finds the goroutine it counts while it runs, so
// a frame that stops matching fails rather than passing unseen.

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"regexp"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	quic "github.com/quic-go/quic-go"
)

// The goroutine label key under which this file's owners run.
const goroutineJoinTestLabelKey = "goroutine-join-test"

// The prefix of this package's function names in a goroutine profile.
const goroutineJoinTestPackagePrefix = "github.com/urnetwork/connect."

// How long a step a test waits for may take before the test fails. A step
// takes moments; the bound is far past any scheduling delay under load.
const goroutineJoinTestWait = 30 * time.Second

// The frames of the goroutines the checks count. Each is the body of its
// goroutine, which has returned before the goroutine's done channel closes. A
// goroutine not yet scheduled shows only the wrapper that closes that channel,
// so a check that the goroutine exists counts the wrapper's prefix.
const (
	// the strategy's context goroutine, inside its recovery
	clientStrategyContextGoroutineFrame = "NewClientStrategy.func1.1"
	// the same goroutine, in its wrapper or its body
	clientStrategyContextGoroutineFramePrefix = "NewClientStrategy.func1"
	// a dial's kick watch, inside its recovery
	dialKickWatchFrame = "(*PlatformTransport).cancelOnKick.func1.1"
	// the same goroutine, in its wrapper or its body
	dialKickWatchFramePrefix = "(*PlatformTransport).cancelOnKick.func1"
	// an H3 dial attempt's handshake watch
	h3HandshakeWatchFrame = "(*PlatformTransport).dialH3.func3"
	// the runner of the H3 mode group, which startModeRunner runs
	h3ModeGroupRunnerFrame = "(*PlatformTransport).startH3ModeGroup.func1"
	// every goroutine of the H3 mode group: its runner, its passes' mode
	// runners, and anything else it starts
	h3ModeGroupFramePrefix = "(*PlatformTransport).startH3ModeGroup"
	// the family group's run loop, and anything it starts
	familyGroupRunFramePrefix = "(*FamilyPlatformTransportGroup).run"
	familyGroupRunFrame       = "(*FamilyPlatformTransportGroup).run"
)

// Runs start under a fresh goroutine label, so every goroutine it starts, and
// every one those start, carries the label. Returns the label's value.
func goroutineJoinTestLabeled(ctx context.Context, start func(ctx context.Context)) string {
	labelValue := NewId().String()
	pprof.Do(ctx, pprof.Labels(goroutineJoinTestLabelKey, labelValue), start)
	return labelValue
}

// The goroutines labeled labelValue in one goroutine profile, each as the
// function names of its frames, innermost first.
func goroutineJoinTestStacks(t *testing.T, labelValue string) [][]string {
	t.Helper()
	var profile bytes.Buffer
	if err := pprof.Lookup("goroutine").WriteTo(&profile, 1); err != nil {
		t.Fatalf("goroutine profile: %v", err)
	}
	labelText := fmt.Sprintf("%q:%q", goroutineJoinTestLabelKey, labelValue)
	recordCountPattern := regexp.MustCompile(`(?m)^(\d+) @`)
	stacks := [][]string{}
	for _, record := range strings.Split(profile.String(), "\n\n") {
		if !strings.Contains(record, labelText) {
			continue
		}
		match := recordCountPattern.FindStringSubmatch(record)
		if match == nil {
			t.Fatalf("goroutine profile record without a count: %q", record)
		}
		recordCount, err := strconv.Atoi(match[1])
		if err != nil {
			t.Fatalf("goroutine profile count %q: %v", match[1], err)
		}
		frames := []string{}
		for _, line := range strings.Split(record, "\n") {
			// "#\t0x1234\tpackage.function+0x56\t/path/file.go:78"
			fields := strings.Split(line, "\t")
			if len(fields) < 3 || fields[0] != "#" {
				continue
			}
			name, _, _ := strings.Cut(fields[2], "+0x")
			frames = append(frames, name)
		}
		for range recordCount {
			stacks = append(stacks, frames)
		}
	}
	return stacks
}

// How many goroutines labeled labelValue have a frame of this package for
// which match holds.
func goroutineJoinTestCount(t *testing.T, labelValue string, match func(name string) bool) int {
	t.Helper()
	count := 0
	for _, frames := range goroutineJoinTestStacks(t, labelValue) {
		for _, frame := range frames {
			name, ok := strings.CutPrefix(frame, goroutineJoinTestPackagePrefix)
			if ok && match(name) {
				count += 1
				break
			}
		}
	}
	return count
}

// How many goroutines labeled labelValue have this package's frame named name.
func goroutineJoinTestFrameCount(t *testing.T, labelValue string, name string) int {
	t.Helper()
	return goroutineJoinTestCount(t, labelValue, func(frameName string) bool {
		return frameName == name
	})
}

// How many goroutines labeled labelValue have a frame of this package whose
// name begins with prefix.
func goroutineJoinTestFramePrefixCount(t *testing.T, labelValue string, prefix string) int {
	t.Helper()
	return goroutineJoinTestCount(t, labelValue, func(frameName string) bool {
		return strings.HasPrefix(frameName, prefix)
	})
}

// Holds a joined goroutine right before it returns until its owner's close
// waits for it, and records what that close saw. The goroutine's exit hook
// calls exit and the close's join hook calls join. Only the first goroutine
// to exit is held; the test's end releases one whose join never came.
type goroutineJoinHold struct {
	testEnd       chan struct{}
	exitOnce      sync.Once
	exiting       chan struct{}
	joinOnce      sync.Once
	joining       chan struct{}
	joinCount     atomic.Int32
	exitingAtJoin atomic.Bool
}

func newGoroutineJoinHold(t *testing.T) *goroutineJoinHold {
	hold := &goroutineJoinHold{
		testEnd: make(chan struct{}),
		exiting: make(chan struct{}),
		joining: make(chan struct{}),
	}
	// registered before the test's own cleanups, so it runs after them: a
	// goroutine a failed join left held ends with the test
	t.Cleanup(func() { close(hold.testEnd) })
	return hold
}

func (self *goroutineJoinHold) exit() {
	first := false
	self.exitOnce.Do(func() {
		first = true
		close(self.exiting)
	})
	if !first {
		return
	}
	select {
	case <-self.joining:
	case <-self.testEnd:
	}
}

// Waits until the goroutine is held before it releases it, so the close waits
// for a goroutine that is still running.
func (self *goroutineJoinHold) join() {
	self.joinCount.Add(1)
	select {
	case <-self.exiting:
		self.exitingAtJoin.Store(true)
	case <-self.testEnd:
	case <-time.After(goroutineJoinTestWait):
	}
	self.joinOnce.Do(func() { close(self.joining) })
}

func (self *goroutineJoinHold) waitExiting(t *testing.T, what string) {
	t.Helper()
	select {
	case <-self.exiting:
	case <-time.After(goroutineJoinTestWait):
		t.Fatalf("%s never reached its return", what)
	}
}

// The close waited, exactly once, for the goroutine while it was held.
func (self *goroutineJoinHold) requireJoinedOnce(t *testing.T, what string) {
	t.Helper()
	if n := self.joinCount.Load(); n != 1 {
		t.Fatalf("the close waited for %s %d times, want 1", what, n)
	}
	if !self.exitingAtJoin.Load() {
		t.Fatalf("the close waited without %s having been held before its return", what)
	}
}

// Close returns only once the goroutine that closes the strategy when its
// context ends has returned, and no such goroutine is left once it does. The
// goroutine, woken by the close's cancel, is held right before it returns
// until the close waits for it. Before Close joined it, sdk's
// TestRpcFixturesJoinTheirDevicesWithTheTest found it still running after the
// fixtures closed (tip-check at c48dbfa6).
func TestClientStrategyCloseJoinsItsContextGoroutine(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hold := newGoroutineJoinHold(t)
	settings := DefaultClientStrategySettings()
	settings.contextCloseExitForTest = hold.exit
	settings.contextCloseJoinForTest = hold.join
	var strategy *ClientStrategy
	labelValue := goroutineJoinTestLabeled(ctx, func(ctx context.Context) {
		strategy = NewClientStrategy(ctx, settings)
	})
	if n := goroutineJoinTestFramePrefixCount(t, labelValue, clientStrategyContextGoroutineFramePrefix); n != 1 {
		t.Fatalf("%d context goroutines of the strategy before the close, want 1", n)
	}

	strategy.Close()
	if n := goroutineJoinTestFrameCount(t, labelValue, clientStrategyContextGoroutineFrame); n != 0 {
		t.Fatalf("%d context goroutines of the strategy left once Close returned, want 0", n)
	}
	hold.requireJoinedOnce(t, "the strategy's context goroutine")
	select {
	case <-strategy.contextCloseDone:
	default:
		t.Fatal("Close returned before the strategy's context goroutine did")
	}
}

// An owner that ends the strategy by its context and then closes it, as an sdk
// network space does, has the context's end close the strategy on the
// goroutine, and its Close waits for that goroutine. The goroutine is held
// right before it returns, the strategy already closed, until Close waits for
// it.
func TestClientStrategyCloseJoinsTheCloseItsContextEndRan(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hold := newGoroutineJoinHold(t)
	settings := DefaultClientStrategySettings()
	settings.contextCloseExitForTest = hold.exit
	settings.contextCloseJoinForTest = hold.join
	strategyCtx, strategyCancel := context.WithCancel(ctx)
	defer strategyCancel()
	var strategy *ClientStrategy
	labelValue := goroutineJoinTestLabeled(strategyCtx, func(ctx context.Context) {
		strategy = NewClientStrategy(ctx, settings)
	})

	strategyCancel()
	hold.waitExiting(t, "the strategy's context goroutine")
	closed := func() bool {
		strategy.mutex.Lock()
		defer strategy.mutex.Unlock()
		return strategy.closed
	}()
	if !closed {
		t.Fatal("the end of the strategy's context did not close it")
	}
	select {
	case <-strategy.contextCloseDone:
		t.Fatal("the strategy's context goroutine counted as returned while it was held")
	default:
	}

	strategy.Close()
	if n := goroutineJoinTestFrameCount(t, labelValue, clientStrategyContextGoroutineFrame); n != 0 {
		t.Fatalf("%d context goroutines of the strategy left once Close returned, want 0", n)
	}
	hold.requireJoinedOnce(t, "the strategy's context goroutine")
	select {
	case <-strategy.contextCloseDone:
	default:
		t.Fatal("Close returned before the strategy's context goroutine did")
	}
}

// A strategy its constructor did not build runs no context goroutine, so its
// Close, called again too, waits for none.
func TestClientStrategyNotBuiltByItsConstructorCloseWaitsForNothing(t *testing.T) {
	var joinCount atomic.Int32
	settings := DefaultClientStrategySettings()
	settings.contextCloseJoinForTest = func() { joinCount.Add(1) }
	strategy := &ClientStrategy{settings: settings, dialers: map[*clientDialer]bool{}}
	strategy.Close()
	strategy.Close()
	if n := joinCount.Load(); n != 0 {
		t.Fatalf("a strategy with no context goroutine waited for one %d times", n)
	}
}

// A held transport, which dials nothing, with its kick watch hooks.
func newDialKickWatchTestTransport(t *testing.T, ctx context.Context, hold *goroutineJoinHold) *PlatformTransport {
	t.Helper()
	settings := testingPlatformTransportSettings()
	settings.StartDisabled = true
	settings.dialKickWatchExitForTest = hold.exit
	settings.dialKickWatchJoinForTest = hold.join
	return testingPlatformTransport(t, ctx, "ws://127.0.0.1:1", settings)
}

// A dial's kick watch stop returns only once the watch has returned, without
// canceling the dial. The watch, woken by the stop, is held right before it
// returns until the stop waits for it. A second stop waits for nothing more.
func TestDialKickWatchStopJoinsTheWatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hold := newGoroutineJoinHold(t)
	transport := newDialKickWatchTestTransport(t, ctx, hold)
	dialCtx, cancelDial := context.WithCancel(ctx)
	defer cancelDial()
	var stop func()
	labelValue := goroutineJoinTestLabeled(ctx, func(context.Context) {
		stop = transport.cancelOnKick(dialCtx, transport.kickGeneration.Load(), cancelDial)
	})
	if n := goroutineJoinTestFramePrefixCount(t, labelValue, dialKickWatchFramePrefix); n != 1 {
		t.Fatalf("%d kick watches before the stop, want 1", n)
	}

	stop()
	if n := goroutineJoinTestFrameCount(t, labelValue, dialKickWatchFrame); n != 0 {
		t.Fatalf("%d kick watches left once the stop returned, want 0", n)
	}
	hold.requireJoinedOnce(t, "the kick watch")
	if dialCtx.Err() != nil {
		t.Fatal("the stop canceled the dial")
	}
	stop()
	if n := hold.joinCount.Load(); n != 1 {
		t.Fatalf("a second stop waited for the watch again (%d waits)", n)
	}
}

// A kick during the dial cancels it, and the stop that follows joins the watch,
// which is held right before it returns, having canceled the dial, until the
// stop waits for it.
func TestDialKickWatchKickCancelsTheDialAndStopJoins(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hold := newGoroutineJoinHold(t)
	transport := newDialKickWatchTestTransport(t, ctx, hold)
	dialCtx, cancelDial := context.WithCancel(ctx)
	defer cancelDial()
	var stop func()
	labelValue := goroutineJoinTestLabeled(ctx, func(context.Context) {
		stop = transport.cancelOnKick(dialCtx, transport.kickGeneration.Load(), cancelDial)
	})

	transport.Kick()
	select {
	case <-dialCtx.Done():
	case <-time.After(goroutineJoinTestWait):
		t.Fatal("the kick did not cancel the dial")
	}
	hold.waitExiting(t, "the kick watch")
	if n := goroutineJoinTestFrameCount(t, labelValue, dialKickWatchFrame); n != 1 {
		t.Fatalf("%d kick watches held after the kick, want 1", n)
	}

	stop()
	if n := goroutineJoinTestFrameCount(t, labelValue, dialKickWatchFrame); n != 0 {
		t.Fatalf("%d kick watches left once the stop returned, want 0", n)
	}
	hold.requireJoinedOnce(t, "the kick watch")
}

// A platform that accepts connections and never answers them, so an H1 dial
// to it stays in flight until its context ends. Returns its url and a channel
// of the connections it accepted.
func newGoroutineJoinSilentPlatform(t *testing.T) (string, <-chan net.Conn) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 64)
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			select {
			case accepted <- conn:
			default:
				conn.Close()
			}
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		<-acceptDone
		for {
			select {
			case conn := <-accepted:
				conn.Close()
			default:
				return
			}
		}
	})
	return fmt.Sprintf("ws://127.0.0.1:%d", listener.Addr().(*net.TCPAddr).Port), accepted
}

// CloseAndWait on a transport whose H1 dial is in flight returns only once
// the dial's kick watch has returned. The watch, woken by the close, is held
// right before it returns until the runner's stop waits for it. Before the
// stop joined it, CloseAndWait returned with the watch still held.
func TestPlatformTransportCloseAndWaitJoinsAnInFlightDialsKickWatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	platformUrl, accepted := newGoroutineJoinSilentPlatform(t)
	hold := newGoroutineJoinHold(t)
	settings := testingPlatformTransportSettings()
	settings.dialKickWatchExitForTest = hold.exit
	settings.dialKickWatchJoinForTest = hold.join
	var transport *PlatformTransport
	labelValue := goroutineJoinTestLabeled(ctx, func(ctx context.Context) {
		transport = testingPlatformTransport(t, ctx, platformUrl, settings)
	})
	select {
	case <-accepted:
	case <-time.After(goroutineJoinTestWait):
		t.Fatal("the transport never dialed")
	}
	if n := goroutineJoinTestFramePrefixCount(t, labelValue, dialKickWatchFramePrefix); n != 1 {
		t.Fatalf("%d kick watches while the dial is in flight, want 1", n)
	}

	closeCtx, closeCancel := context.WithTimeout(context.Background(), goroutineJoinTestWait)
	defer closeCancel()
	if err := transport.CloseAndWait(closeCtx); err != nil {
		t.Fatalf("CloseAndWait = %v", err)
	}
	if n := goroutineJoinTestFrameCount(t, labelValue, dialKickWatchFrame); n != 0 {
		t.Fatalf("%d kick watches left once CloseAndWait returned, want 0", n)
	}
	if n := hold.joinCount.Load(); n < 1 || !hold.exitingAtJoin.Load() {
		t.Fatalf("the runner's stop waited for the held kick watch %d times, want at least 1", n)
	}
}

// A dial that panics still joins its kick watch: the stop is deferred, so the
// runner waits for the watch before the panic's recovery closes the
// transport, and none is left once the transport is done. The dial panics in
// its auth frame observer, after the platform's upgrade.
func TestPlatformTransportDialPanicJoinsItsKickWatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	platform := newTestingFamilyPlatformServer(t, 4, false)
	hold := newGoroutineJoinHold(t)
	settings := testingPlatformTransportSettings()
	settings.dialKickWatchExitForTest = hold.exit
	settings.dialKickWatchJoinForTest = hold.join
	// the auth frame, which the observer sees, rides only v1 auth
	settings.V2H1Auth = false
	settings.AuthFrameObserver = func([]byte) {
		panic("goroutine join test: the dial panics")
	}
	var transport *PlatformTransport
	labelValue := goroutineJoinTestLabeled(ctx, func(ctx context.Context) {
		transport = testingPlatformTransport(t, ctx, fmt.Sprintf("ws://127.0.0.1:%d", platform.port), settings)
	})

	select {
	case <-transport.Done():
	case <-time.After(goroutineJoinTestWait):
		t.Fatal("the transport did not end after its dial panicked")
	}
	if n := goroutineJoinTestFrameCount(t, labelValue, dialKickWatchFrame); n != 0 {
		t.Fatalf("%d kick watches left once the transport was done, want 0", n)
	}
	hold.requireJoinedOnce(t, "the kick watch")
}

// Runs one H3 mode of the mode group in place of its carrier: it reports its
// start, runs until its group's context ends, reports its stop, and returns,
// after the test releases it when release is set.
type h3ModeGroupTestRunner struct {
	started chan TransportMode
	stopped chan TransportMode
	release chan struct{}
}

func newH3ModeGroupTestRunner() *h3ModeGroupTestRunner {
	return &h3ModeGroupTestRunner{
		started: make(chan TransportMode, 16),
		stopped: make(chan TransportMode, 16),
	}
}

func (self *h3ModeGroupTestRunner) run(ctx context.Context, mode TransportMode, _ time.Duration) {
	self.started <- mode
	<-ctx.Done()
	self.stopped <- mode
	if self.release != nil {
		<-self.release
	}
}

// The frame the checks count for a mode runner.
const h3ModeGroupTestRunnerFrame = "(*h3ModeGroupTestRunner).run"

func (self *h3ModeGroupTestRunner) wait(t *testing.T, modes chan TransportMode, count int, what string) {
	t.Helper()
	for range count {
		select {
		case <-modes:
		case <-time.After(goroutineJoinTestWait):
			t.Fatalf("an H3 mode runner never %s", what)
		}
	}
}

// An Auto transport whose H3 lease runs two H3 modes as one group, each in
// runner. configure adjusts the settings before the transport is built.
func newH3ModeGroupTestTransport(
	t *testing.T,
	ctx context.Context,
	runner *h3ModeGroupTestRunner,
	configure func(settings *PlatformTransportSettings),
) (*PlatformTransport, string) {
	t.Helper()
	settings := testingDefaultPlatformTransportSettings()
	// room for the lease and the DNS mode's nested translation budget
	settings.PlatformTransportBudget = NewPlatformTransportBudget(mib(1024), 1)
	settings.H3BudgetByteCount = 4
	settings.PlatformTransportBudgetPriority = PlatformTransportBudgetPriorityBackground
	settings.ModePreferences = map[TransportMode]int{TransportModeH3: 1, TransportModeH3Dns: 2}
	settings.runH3ModeForTest = runner.run
	configure(settings)
	var transport *PlatformTransport
	labelValue := goroutineJoinTestLabeled(ctx, func(ctx context.Context) {
		transport = NewPlatformTransportWithTargetMode(
			ctx,
			NewClientStrategyWithDefaults(ctx),
			NewRouteManager(ctx, "h3-mode-group-join"),
			"https://127.0.0.1",
			&ClientAuth{InstanceId: NewId()},
			TransportModeAuto,
			settings,
		)
	})
	t.Cleanup(transport.Close)
	return transport, labelValue
}

// While the H3 mode group runs, its only goroutines are its runner and one
// per mode: no helper waits for the modes beside it, as one did, unjoined,
// before. CloseAndWait returns once the runner and every mode runner have
// returned.
func TestH3ModeGroupRunsNoHelperGoroutineAndCloseJoinsItsRunners(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runner := newH3ModeGroupTestRunner()
	waiting := make(chan struct{}, 16)
	transport, labelValue := newH3ModeGroupTestTransport(t, ctx, runner, func(settings *PlatformTransportSettings) {
		settings.h3ModeGroupWaitForTest = func() {
			select {
			case waiting <- struct{}{}:
			default:
			}
		}
	})
	runner.wait(t, runner.started, 2, "started")
	select {
	case <-waiting:
	case <-time.After(goroutineJoinTestWait):
		t.Fatal("the H3 mode group never waited")
	}
	if n := goroutineJoinTestFramePrefixCount(t, labelValue, h3ModeGroupFramePrefix); n != 3 {
		t.Fatalf("%d goroutines in the H3 mode group while it runs, want its runner and one per mode, 3", n)
	}

	closeCtx, closeCancel := context.WithTimeout(context.Background(), goroutineJoinTestWait)
	defer closeCancel()
	if err := transport.CloseAndWait(closeCtx); err != nil {
		t.Fatalf("CloseAndWait = %v", err)
	}
	if n := goroutineJoinTestFrameCount(t, labelValue, h3ModeGroupTestRunnerFrame); n != 0 {
		t.Fatalf("%d H3 mode runners left once CloseAndWait returned, want 0", n)
	}
	if n := goroutineJoinTestFrameCount(t, labelValue, h3ModeGroupRunnerFrame); n != 0 {
		t.Fatalf("%d H3 mode group runners left once CloseAndWait returned, want 0", n)
	}
}

// A hold ends the H3 mode group's pass, which joins its mode runners before
// the group parks for the hold: once parked, none is left, and the runners
// start again when the hold lifts.
func TestH3ModeGroupHoldJoinsItsRunnersBeforeParking(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runner := newH3ModeGroupTestRunner()
	parked := make(chan struct{}, 16)
	transport, labelValue := newH3ModeGroupTestTransport(t, ctx, runner, func(settings *PlatformTransportSettings) {
		settings.heldParkForTest = func() {
			select {
			case parked <- struct{}{}:
			default:
			}
		}
	})
	runner.wait(t, runner.started, 2, "started")

	transport.SetEnabled(false)
	select {
	case <-parked:
	case <-time.After(goroutineJoinTestWait):
		t.Fatal("the H3 mode group never parked for the hold")
	}
	runner.wait(t, runner.stopped, 2, "stopped")
	if n := goroutineJoinTestFrameCount(t, labelValue, h3ModeGroupTestRunnerFrame); n != 0 {
		t.Fatalf("%d H3 mode runners left once the group parked for the hold, want 0", n)
	}
	if n := goroutineJoinTestFrameCount(t, labelValue, h3ModeGroupRunnerFrame); n != 1 {
		t.Fatalf("%d H3 mode group runners while the group is parked, want 1", n)
	}

	transport.SetEnabled(true)
	runner.wait(t, runner.started, 2, "started again")
	closeCtx, closeCancel := context.WithTimeout(context.Background(), goroutineJoinTestWait)
	defer closeCancel()
	if err := transport.CloseAndWait(closeCtx); err != nil {
		t.Fatalf("CloseAndWait = %v", err)
	}
	if n := goroutineJoinTestFrameCount(t, labelValue, h3ModeGroupTestRunnerFrame); n != 0 {
		t.Fatalf("%d H3 mode runners left once CloseAndWait returned, want 0", n)
	}
}

// A pass of the H3 mode group that panics once its runners started still
// joins them: its join is deferred, so the transport is not done while a
// runner still runs. The transport runs in a synctest bubble, and the runners,
// stopped by the pass's end, are held until everything else in the bubble is
// blocked, by which point the panic's recovery would have ended the transport
// had the pass not waited for them.
func TestH3ModeGroupPanicInAPassJoinsItsRunners(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		runner := newH3ModeGroupTestRunner()
		runner.release = make(chan struct{})
		// a failed check still lets the held runners end, so the bubble does
		releaseRunners := sync.OnceFunc(func() { close(runner.release) })
		defer releaseRunners()
		var passCount atomic.Int32
		transport, labelValue := newH3ModeGroupTestTransport(t, ctx, runner, func(settings *PlatformTransportSettings) {
			settings.h3ModeGroupWaitForTest = func() {
				if passCount.Add(1) == 1 {
					panic("goroutine join test: the H3 mode group's pass panics")
				}
			}
		})
		runner.wait(t, runner.stopped, 2, "stopped")
		synctest.Wait()
		select {
		case <-transport.Done():
			t.Fatal("the transport finished while its H3 mode runners still ran")
		default:
		}

		releaseRunners()
		<-transport.Done()
		if n := goroutineJoinTestFrameCount(t, labelValue, h3ModeGroupTestRunnerFrame); n != 0 {
			t.Fatalf("%d H3 mode runners left once the transport was done, want 0", n)
		}
	})
}

// Closes only when the test releases it, and reports that its close began.
type raceJoinTestPacketConn struct {
	net.PacketConn
	closeEntered chan struct{}
	closeRelease chan struct{}
}

func (self *raceJoinTestPacketConn) Close() error {
	close(self.closeEntered)
	<-self.closeRelease
	return nil
}

// Dials for the H3 race test. A port in heldPorts reports its start, dials
// until its context ends, reports that, waits for release when it is set, and
// returns heldAttempts' attempt for its port, or its context's error when
// there is none. Every other port wins at once.
type raceJoinTestDialer struct {
	heldPorts     map[int]bool
	heldAttempts  map[int]*h3DialAttempt
	started       chan int
	canceled      chan int
	release       chan struct{}
	returnedCount atomic.Int32
}

func (self *raceJoinTestDialer) dial(ctx context.Context, udpAddr *net.UDPAddr) (*h3DialAttempt, error) {
	defer self.returnedCount.Add(1)
	if !self.heldPorts[udpAddr.Port] {
		return &h3DialAttempt{udpAddr: udpAddr}, nil
	}
	self.started <- udpAddr.Port
	<-ctx.Done()
	self.canceled <- udpAddr.Port
	if self.release != nil {
		<-self.release
	}
	if attempt := self.heldAttempts[udpAddr.Port]; attempt != nil {
		return attempt, nil
	}
	return nil, ctx.Err()
}

// The frame the checks count for a dial.
const raceJoinTestDialFrame = "(*raceJoinTestDialer).dial"

// The H3 race returns its winner only once a loser that completes after the
// winner has been closed and its dial goroutine has returned: the loser's
// close is held, and the race is still running, blocked on it, once every
// goroutine of the test is. Before, a drain goroutine closed the loser after
// the race had returned.
func TestRaceH3DialJoinsALoserThatCompletesAfterTheWinner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		loserConn := &raceJoinTestPacketConn{
			closeEntered: make(chan struct{}),
			closeRelease: make(chan struct{}),
		}
		candidates := []*net.UDPAddr{
			{IP: net.ParseIP("192.0.2.1"), Port: 1},
			{IP: net.ParseIP("192.0.2.2"), Port: 2},
		}
		dialer := &raceJoinTestDialer{
			heldPorts:    map[int]bool{1: true},
			heldAttempts: map[int]*h3DialAttempt{1: {udpAddr: candidates[0], packetConn: loserConn}},
			started:      make(chan int, 2),
			canceled:     make(chan int, 2),
		}
		// a failed check still lets the held close end, so the bubble does
		releaseLoserClose := sync.OnceFunc(func() { close(loserConn.closeRelease) })
		defer releaseLoserClose()
		raceDone := make(chan struct{})
		var winner *h3DialAttempt
		var raceErr error
		labelValue := goroutineJoinTestLabeled(t.Context(), func(ctx context.Context) {
			go func() {
				defer close(raceDone)
				// the first candidate is still dialing when the stagger
				// launches the second, which wins
				winner, raceErr = raceH3Dial(ctx, candidates, dialer.dial)
			}()
		})

		<-loserConn.closeEntered
		synctest.Wait()
		select {
		case <-raceDone:
			t.Fatal("the race returned while its loser's close was still running")
		default:
		}

		releaseLoserClose()
		<-raceDone
		if raceErr != nil {
			t.Fatalf("race = %v", raceErr)
		}
		if winner.udpAddr.Port != 2 {
			t.Fatalf("winner = %v, want port 2", winner.udpAddr)
		}
		if n := dialer.returnedCount.Load(); n != 2 {
			t.Fatalf("%d dials returned once the race did, want 2", n)
		}
		if n := goroutineJoinTestFrameCount(t, labelValue, raceJoinTestDialFrame); n != 0 {
			t.Fatalf("%d dials left once the race returned, want 0", n)
		}
	})
}

// The H3 race its context ends returns only once every dial it started has
// returned: both dials, canceled and held, keep the race running once every
// goroutine of the test is blocked. Before, the race returned at once and a
// drain goroutine waited for them.
func TestRaceH3DialEndedByItsContextJoinsEveryDial(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		candidates := []*net.UDPAddr{
			{IP: net.ParseIP("2001:db8::1"), Port: 1},
			{IP: net.ParseIP("192.0.2.1"), Port: 2},
		}
		dialer := &raceJoinTestDialer{
			heldPorts: map[int]bool{1: true, 2: true},
			started:   make(chan int, 2),
			canceled:  make(chan int, 2),
			release:   make(chan struct{}),
		}
		// a failed check still lets the held dials end, so the bubble does
		releaseDials := sync.OnceFunc(func() { close(dialer.release) })
		defer releaseDials()
		raceDone := make(chan struct{})
		var raceErr error
		labelValue := goroutineJoinTestLabeled(ctx, func(ctx context.Context) {
			go func() {
				defer close(raceDone)
				_, raceErr = raceH3Dial(ctx, candidates, dialer.dial)
			}()
		})
		// both candidates are dialing once the stagger, on the bubble's
		// clock, has launched the second
		<-dialer.started
		<-dialer.started
		synctest.Wait()
		if n := goroutineJoinTestFrameCount(t, labelValue, raceJoinTestDialFrame); n != 2 {
			t.Fatalf("%d dials running before the race's context ended, want 2", n)
		}

		cancel()
		<-dialer.canceled
		<-dialer.canceled
		synctest.Wait()
		select {
		case <-raceDone:
			t.Fatal("the race returned while its dials still ran")
		default:
		}

		releaseDials()
		<-raceDone
		if !errors.Is(raceErr, context.Canceled) {
			t.Fatalf("race = %v, want its context's end", raceErr)
		}
		if n := dialer.returnedCount.Load(); n != 2 {
			t.Fatalf("%d dials returned once the race did, want 2", n)
		}
		if n := goroutineJoinTestFrameCount(t, labelValue, raceJoinTestDialFrame); n != 0 {
			t.Fatalf("%d dials left once the race returned, want 0", n)
		}
	})
}

// An H3 dial attempt's close returns only once the attempt's handshake watch
// has returned. The watch, having recorded the handshake against a live
// loopback QUIC listener, is held right before it returns until the close
// waits for it.
func TestH3DialAttemptCloseJoinsItsHandshakeWatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	certPem, keyPem, err := selfSign([]string{"127.0.0.1"}, "goroutine-join", time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPem, keyPem)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := quic.ListenAddrEarly(
		"127.0.0.1:0",
		&tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"goroutine-join"}},
		&quic.Config{MaxIdleTimeout: time.Minute},
	)
	if err != nil {
		t.Fatal(err)
	}
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, err := listener.Accept(ctx)
			if err != nil {
				return
			}
			go func() {
				<-ctx.Done()
				conn.CloseWithError(0, "")
			}()
		}
	}()
	defer func() {
		cancel()
		listener.Close()
		<-acceptDone
	}()

	hold := newGoroutineJoinHold(t)
	stats := &H3QuicPacketStats{}
	settings := testingDefaultPlatformTransportSettings()
	settings.H3QuicPacketStats = stats
	settings.h3HandshakeWatchExitForTest = hold.exit
	settings.h3HandshakeWatchJoinForTest = hold.join
	transport := newTestAltTransport(t, settings)
	// only this loopback fixture skips verification
	tlsConfig := &tls.Config{InsecureSkipVerify: true, ServerName: "127.0.0.1", NextProtos: []string{"goroutine-join"}}
	wrap := func(_ context.Context, packetConn net.PacketConn) (net.PacketConn, error) { return packetConn, nil }
	var attempt *h3DialAttempt
	var dialErr error
	labelValue := goroutineJoinTestLabeled(ctx, func(ctx context.Context) {
		attempt, dialErr = transport.dialH3(
			ctx,
			TransportModeH3,
			tlsConfig.ServerName,
			listener.Addr().(*net.UDPAddr),
			wrap,
			tlsConfig,
			newPlatformQuicConfig(settings, 1),
			1,
			false,
		)
	})
	if dialErr != nil {
		t.Fatal(dialErr)
	}
	hold.waitExiting(t, "the handshake watch")
	if n := goroutineJoinTestFrameCount(t, labelValue, h3HandshakeWatchFrame); n != 1 {
		t.Fatalf("%d handshake watches before the attempt's close, want 1", n)
	}
	if n := stats.Snapshot().HandshakeSuccessCount; n != 1 {
		t.Fatalf("the handshake watch recorded %d successful handshakes, want 1", n)
	}

	attempt.close()
	if n := goroutineJoinTestFrameCount(t, labelValue, h3HandshakeWatchFrame); n != 0 {
		t.Fatalf("%d handshake watches left once the attempt's close returned, want 0", n)
	}
	hold.requireJoinedOnce(t, "the handshake watch")
	select {
	case <-attempt.handshakeWatchDone:
	default:
		t.Fatal("the attempt's close returned before its handshake watch did")
	}
}

// A family group whose pins dial ports nothing listens on, so no transport
// connects and the standby waits out a delay far beyond the test. The group
// and everything it starts carry a fresh goroutine label, which it returns.
func newGoroutineJoinTestGroup(
	t *testing.T,
	ctx context.Context,
	clientSettings *ClientStrategySettings,
	pinnedClientSettings *ClientStrategySettings,
	groupSettings *FamilyPlatformTransportGroupSettings,
) (*FamilyPlatformTransportGroup, string) {
	t.Helper()
	v4Port := reserveTestPort(t, "tcp4", "127.0.0.1")
	v6Port := reserveTestPort(t, "tcp6", "::1")
	standbyPort := reserveTestPort(t, "tcp4", "127.0.0.1")
	strategy := NewClientStrategy(ctx, clientSettings)
	t.Cleanup(strategy.Close)
	var group *FamilyPlatformTransportGroup
	labelValue := goroutineJoinTestLabeled(ctx, func(ctx context.Context) {
		group = NewFamilyPlatformTransportGroup(
			ctx,
			pinnedClientSettings,
			strategy,
			NewRouteManager(ctx, "group"),
			fmt.Sprintf("ws://%s:%d", familyTransportTestHost, standbyPort),
			fmt.Sprintf("ws://%s:%d", familyTransportTestHost, v4Port),
			fmt.Sprintf("ws://%s:%d", familyTransportTestHost, v6Port),
			testingFamilyAuth(),
			TransportModeH1,
			testingFamilyTransportSettings(),
			groupSettings,
		)
	})
	t.Cleanup(group.Close)
	return group, labelValue
}

// The family group's run loop waits on its transports' notifications without
// starting a goroutine: while it waits, it is its own only goroutine, where it
// used to start one per transport on every wake and leave them unjoined.
// CloseAndWait returns once the loop, every transport and the owned
// strategies' context goroutines have returned.
func TestFamilyPlatformTransportGroupRunStartsNoGoroutineAndCloseJoinsIt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	waiting := make(chan struct{}, 64)
	clientSettings := newTestingFamilyStrategySettingsOwningHost(t)
	group, labelValue := newGoroutineJoinTestGroup(t, ctx, clientSettings, clientSettings, &FamilyPlatformTransportGroupSettings{
		StandbyDelay: time.Hour,
		runWaitForTest: func() {
			select {
			case waiting <- struct{}{}:
			default:
			}
		},
	})
	select {
	case <-waiting:
	case <-time.After(goroutineJoinTestWait):
		t.Fatal("the group's run loop never waited")
	}
	if n := goroutineJoinTestFramePrefixCount(t, labelValue, familyGroupRunFramePrefix); n != 1 {
		t.Fatalf("%d goroutines in the group's run loop while it waits, want the loop alone", n)
	}

	closeCtx, closeCancel := context.WithTimeout(context.Background(), goroutineJoinTestWait)
	defer closeCancel()
	if err := group.CloseAndWait(closeCtx); err != nil {
		t.Fatalf("CloseAndWait = %v", err)
	}
	if n := goroutineJoinTestFrameCount(t, labelValue, familyGroupRunFrame); n != 0 {
		t.Fatalf("%d group run loops left once CloseAndWait returned, want 0", n)
	}
	for _, transport := range group.Transports() {
		select {
		case <-transport.Done():
		default:
			t.Fatal("CloseAndWait returned before a transport of the group was done")
		}
	}
	for _, strategy := range []*ClientStrategy{group.ipv4Strategy, group.ipv6Strategy} {
		select {
		case <-strategy.contextCloseDone:
		default:
			t.Fatal("CloseAndWait returned before an owned strategy's context goroutine did")
		}
	}
}

// An owner that ends the family group by its context, without Close, has
// Done wait for the owned strategies' context goroutines too: with both held
// right before they return, every transport is done and the group is not.
func TestFamilyPlatformTransportGroupDoneWaitsForItsOwnedStrategies(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	held := make(chan struct{}, 2)
	clientSettings := newTestingFamilyStrategySettingsOwningHost(t)
	pinnedClientSettings := *clientSettings
	pinnedClientSettings.contextCloseExitForTest = func() {
		held <- struct{}{}
		<-release
	}
	groupCtx, groupCancel := context.WithCancel(ctx)
	defer groupCancel()
	group, _ := newGoroutineJoinTestGroup(t, groupCtx, clientSettings, &pinnedClientSettings, &FamilyPlatformTransportGroupSettings{
		StandbyDelay: time.Hour,
	})

	groupCancel()
	for range 2 {
		select {
		case <-held:
		case <-time.After(goroutineJoinTestWait):
			t.Fatal("an owned strategy's context goroutine never closed it")
		}
	}
	for _, transport := range group.Transports() {
		select {
		case <-transport.Done():
		case <-time.After(goroutineJoinTestWait):
			t.Fatal("a transport of the group did not end with its context")
		}
	}
	select {
	case <-group.Done():
		t.Fatal("the group was done while its owned strategies' context goroutines still ran")
	default:
	}

	releaseOnce.Do(func() { close(release) })
	select {
	case <-group.Done():
	case <-time.After(goroutineJoinTestWait):
		t.Fatal("the group was not done once its owned strategies' context goroutines returned")
	}
	for _, strategy := range []*ClientStrategy{group.ipv4Strategy, group.ipv6Strategy} {
		select {
		case <-strategy.contextCloseDone:
		default:
			t.Fatal("the group was done before an owned strategy's context goroutine returned")
		}
	}
}
