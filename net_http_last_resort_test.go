package connect

import (
	"context"
	"errors"
	"net/http/httptrace"
	"net/netip"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// The api race's last-resort dialer, alt whodis (parallelEvalWithRouteHedge):
// started late beside the dialers still out when none has connected, at once
// when they have all failed, first on a network where it last won, and never
// ended for another dialer's win while it hears from alt.

const lastResortTestName = "last resort"

// What one fake dial does with its attempt context.
type lastResortTestBehavior func(ctx context.Context) error

func lastResortTestFail(context.Context) error {
	return errors.New("synthetic refusal")
}

func lastResortTestSucceed(context.Context) error {
	return nil
}

func lastResortTestBlackhole(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// Fails after the delay, or with the context.
func lastResortTestFailAfter(delay time.Duration) lastResortTestBehavior {
	return func(ctx context.Context) error {
		select {
		case <-time.After(delay):
			return errors.New("synthetic late refusal")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Connects at once, writing its request as an HTTP transport reports it, and
// answers after the delay.
func lastResortTestConnectAnswerAfter(delay time.Duration, err error) lastResortTestBehavior {
	return func(ctx context.Context) error {
		if trace := httptrace.ContextClientTrace(ctx); trace != nil && trace.WroteHeaders != nil {
			trace.WroteHeaders()
		}
		select {
		case <-time.After(delay):
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// A whodis dial that hears alt every half second when `hearing`, and
// completes after the handshake time, or ends with the context.
func lastResortTestWhodis(handshake time.Duration, hearing bool) lastResortTestBehavior {
	return func(ctx context.Context) error {
		clock := quicDialReceiveClockFromContext(ctx)
		heard := time.NewTicker(500 * time.Millisecond)
		defer heard.Stop()
		complete := time.After(handshake)
		for {
			select {
			case <-heard.C:
				if hearing && clock != nil {
					clock.received()
				}
			case <-complete:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// One fake dial's record: when it was entered and ended, with what, and its
// attempt context.
type lastResortTestDial struct {
	name      string
	startTime time.Time
	endTime   time.Time
	err       error
	ctx       context.Context
}

// Fake dials by dialer description, recording each.
type lastResortTestEval struct {
	stateLock sync.Mutex
	behaviors map[string]lastResortTestBehavior
	dials     []*lastResortTestDial
}

func newLastResortTestEval() *lastResortTestEval {
	return &lastResortTestEval{behaviors: map[string]lastResortTestBehavior{}}
}

func (self *lastResortTestEval) set(behaviors map[string]lastResortTestBehavior) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.behaviors = behaviors
	self.dials = nil
}

func (self *lastResortTestEval) eval(ctx context.Context, dialer *clientDialer) *evalResult {
	dial := &lastResortTestDial{name: dialer.description, startTime: time.Now(), ctx: ctx}
	behavior := func() lastResortTestBehavior {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.dials = append(self.dials, dial)
		return self.behaviors[dialer.description]
	}()
	err := behavior(ctx)
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		dial.endTime = time.Now()
		dial.err = err
	}()
	dialer.Update(ctx, err)
	return &evalResult{err: err}
}

// The dials of the name, in entry order.
func (self *lastResortTestEval) dialsOf(name string) []lastResortTestDial {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	dials := []lastResortTestDial{}
	for _, dial := range self.dials {
		if dial.name == name {
			dials = append(dials, *dial)
		}
	}
	return dials
}

func (self *lastResortTestEval) lastResortDialed() bool {
	return 0 < len(self.dialsOf(lastResortTestName))
}

// A bare strategy of the named primary dialers and one last-resort dialer.
func newLastResortTestStrategy(blockSize int, primaryNames ...string) (*ClientStrategy, map[string]*clientDialer, context.CancelFunc) {
	strategyCtx, strategyCancel := context.WithCancel(context.Background())
	settings := DefaultClientStrategySettings()
	settings.RequestTimeout = 10 * time.Second
	settings.ParallelBlockSize = blockSize
	settings.ExpandExtenderProfileCount = 0
	dialersByName := map[string]*clientDialer{}
	dialers := map[*clientDialer]bool{}
	for i, name := range primaryNames {
		dialer := &clientDialer{description: name, minimumWeight: 1, priority: i, settings: settings}
		dialersByName[name] = dialer
		dialers[dialer] = true
	}
	lastResort := &clientDialer{
		description:   lastResortTestName,
		minimumWeight: altDialerMinimumWeight,
		priority:      altWhodisDialerPriority,
		lastResort:    true,
		settings:      settings,
	}
	dialersByName[lastResortTestName] = lastResort
	dialers[lastResort] = true
	strategy := &ClientStrategy{
		ctx:               strategyCtx,
		log:               loggerOrDefault(nil),
		settings:          settings,
		dialers:           dialers,
		extenderIpSecrets: map[netip.Addr]string{},
	}
	return strategy, dialersByName, strategyCancel
}

// The alt whodis dialer is the last resort; alt h3 races with the others.
func TestAltWhodisDialerIsTheLastResort(t *testing.T) {
	settings := DefaultClientStrategySettings()
	settings.AltUrl = "https://alt.space.example"
	for _, dialer := range newAltDialers(&ClientStrategy{settings: settings}, settings) {
		if want := dialer.description == "alt whodis"; dialer.lastResort != want {
			t.Fatalf("%s: last resort=%t", dialer.description, dialer.lastResort)
		}
	}
}

// A race whose other dialers connect within the late start never dials the
// last resort: a win in the parallel blocks, a win the GET hedge opens after a
// stale preferred dialer, and a dialer that connected at once but answers
// after the late start.
func TestParallelEvalNeverDialsTheLastResortWhereTheOthersConnect(t *testing.T) {
	type scenario struct {
		name       string
		behaviors  func(lateStart time.Duration) map[string]lastResortTestBehavior
		hedge      bool
		wantWinner string
	}
	scenarios := []scenario{
		{
			name: "blocks",
			behaviors: func(lateStart time.Duration) map[string]lastResortTestBehavior {
				return map[string]lastResortTestBehavior{
					"a":                lastResortTestConnectAnswerAfter(lateStart/6, nil),
					"b":                lastResortTestBlackhole,
					"c":                lastResortTestBlackhole,
					lastResortTestName: lastResortTestSucceed,
				}
			},
			wantWinner: "a",
		},
		{
			// a delivered on this network, then went stale: the hedge opens
			// the others after its head start, and b wins
			name: "hedge",
			behaviors: func(lateStart time.Duration) map[string]lastResortTestBehavior {
				return map[string]lastResortTestBehavior{
					"a":                lastResortTestBlackhole,
					"b":                lastResortTestConnectAnswerAfter(lateStart/6, nil),
					"c":                lastResortTestBlackhole,
					lastResortTestName: lastResortTestSucceed,
				}
			},
			hedge:      true,
			wantWinner: "b",
		},
		{
			// c connects at once and answers after the late start, past it
			name: "connected",
			behaviors: func(lateStart time.Duration) map[string]lastResortTestBehavior {
				return map[string]lastResortTestBehavior{
					"a":                lastResortTestBlackhole,
					"b":                lastResortTestBlackhole,
					"c":                lastResortTestConnectAnswerAfter(2*lateStart, nil),
					lastResortTestName: lastResortTestSucceed,
				}
			},
			wantWinner: "c",
		},
	}
	for _, scenario := range scenarios {
		synctest.Test(t, func(t *testing.T) {
			strategy, dialers, cancel := newLastResortTestStrategy(8, "a", "b", "c")
			defer cancel()
			testEval := newLastResortTestEval()
			testEval.set(scenario.behaviors(strategy.settings.AltWhodisLateStart))
			var result *evalResult
			if scenario.hedge {
				dialers["a"].Update(context.Background(), nil)
				result = strategy.parallelEvalWithRouteHedge(
					context.Background(), false, testEval.eval, preferredHttpAttemptContext, time.Second)
			} else {
				result = strategy.parallelEval(context.Background(), false, testEval.eval)
			}
			if result == nil || result.dialer != dialers[scenario.wantWinner] || testEval.lastResortDialed() {
				t.Fatalf("%s: winner=%v last resort dialed=%t", scenario.name, result, testEval.lastResortDialed())
			}
			if strategy.lastResortWonOn(strategy.networkIdSnapshot()) {
				t.Fatalf("%s: a network where the others won is marked", scenario.name)
			}
		})
	}
}

// On a network where only whodis works, the first request connects at the
// late start plus the whodis handshake: whodis starts at the late start
// beside the dialers still out, with the request's deadline, and wins; the
// network is marked.
func TestParallelEvalStartsTheLastResortLateOnAWhodisOnlyNetwork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		strategy, dialers, cancel := newLastResortTestStrategy(4, "a", "b", "c")
		defer cancel()
		lateStart := strategy.settings.AltWhodisLateStart
		const handshake = 700 * time.Millisecond
		testEval := newLastResortTestEval()
		testEval.set(map[string]lastResortTestBehavior{
			"a":                lastResortTestBlackhole,
			"b":                lastResortTestBlackhole,
			"c":                lastResortTestBlackhole,
			lastResortTestName: lastResortTestWhodis(handshake, true),
		})
		start := time.Now()
		result := strategy.parallelEval(context.Background(), false, testEval.eval)
		if result == nil || result.dialer != dialers[lastResortTestName] {
			t.Fatalf("winner=%v", result)
		}
		if took := time.Since(start); took != lateStart+handshake {
			t.Fatalf("the whodis-only network connected in %s, want %s", took, lateStart+handshake)
		}
		whodis := testEval.dialsOf(lastResortTestName)
		if len(whodis) != 1 || whodis[0].startTime.Sub(start) != lateStart {
			t.Fatalf("whodis dials=%v, want one at the late start", whodis)
		}
		if deadline, _ := whodis[0].ctx.Deadline(); !deadline.Equal(start.Add(strategy.settings.RequestTimeout)) {
			t.Fatalf("whodis's deadline=%s, want the request's", deadline.Sub(start))
		}
		for _, name := range []string{"a", "b", "c"} {
			if dials := testEval.dialsOf(name); len(dials) != 1 || !dials[0].startTime.Equal(start) {
				t.Fatalf("%s dials=%v, want one from the start", name, dials)
			}
		}
		if !strategy.lastResortWonOn(strategy.networkIdSnapshot()) {
			t.Fatal("the network whodis won is not marked")
		}
	})
}

// A round whose other dialers have all failed starts the last resort at once,
// not at the late start; one that connected and then failed holds it only
// until every other dialer of the round is done.
func TestParallelEvalStartsTheLastResortWhenTheOthersAreDone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		strategy, dialers, cancel := newLastResortTestStrategy(4, "a", "b", "c")
		defer cancel()
		lateStart := strategy.settings.AltWhodisLateStart
		testEval := newLastResortTestEval()
		testEval.set(map[string]lastResortTestBehavior{
			"a":                lastResortTestFail,
			"b":                lastResortTestFail,
			"c":                lastResortTestFail,
			lastResortTestName: lastResortTestSucceed,
		})
		start := time.Now()
		result := strategy.parallelEval(context.Background(), false, testEval.eval)
		if result == nil || result.dialer != dialers[lastResortTestName] || !time.Now().Equal(start) {
			t.Fatalf("fast refusals: winner=%v after %s", result, time.Since(start))
		}

		strategy.networkChanged()
		testEval.set(map[string]lastResortTestBehavior{
			"a":                lastResortTestConnectAnswerAfter(lateStart+time.Second, errors.New("synthetic reset")),
			"b":                lastResortTestFailAfter(lateStart + 2*time.Second),
			"c":                lastResortTestFail,
			lastResortTestName: lastResortTestSucceed,
		})
		start = time.Now()
		result = strategy.parallelEval(context.Background(), false, testEval.eval)
		if result == nil || result.dialer != dialers[lastResortTestName] {
			t.Fatalf("connected then failed: winner=%v", result)
		}
		if whodis := testEval.dialsOf(lastResortTestName); len(whodis) != 1 || whodis[0].startTime.Sub(start) != lateStart+2*time.Second {
			t.Fatalf("whodis dials=%v, want one once b failed", whodis)
		}
	})
}

// A whodis started late is never ended for another dialer's win while it
// hears from alt: the race returns b's win, and whodis goes on to complete
// its handshake. One that has heard nothing for the stall timeout when the
// race ends is ended then; one silent for less goes on to the request's
// deadline.
func TestParallelEvalNeverEndsALateLastResortHearingAlt(t *testing.T) {
	// b answers this long after whodis starts
	bAnswersAfter := func(lateStart time.Duration, afterLateStart time.Duration) lastResortTestBehavior {
		return func(ctx context.Context) error {
			select {
			case <-time.After(lateStart + afterLateStart):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	synctest.Test(t, func(t *testing.T) {
		strategy, dialers, cancel := newLastResortTestStrategy(4, "a", "b", "c")
		defer cancel()
		lateStart := strategy.settings.AltWhodisLateStart
		testEval := newLastResortTestEval()
		testEval.set(map[string]lastResortTestBehavior{
			"a":                lastResortTestBlackhole,
			"b":                bAnswersAfter(lateStart, time.Second),
			"c":                lastResortTestBlackhole,
			lastResortTestName: lastResortTestWhodis(2*time.Second, true),
		})
		start := time.Now()
		result := strategy.parallelEval(context.Background(), false, testEval.eval)
		if result == nil || result.dialer != dialers["b"] || time.Since(start) != lateStart+time.Second {
			t.Fatalf("winner=%v after %s", result, time.Since(start))
		}
		synctest.Wait()
		whodis := testEval.dialsOf(lastResortTestName)
		if len(whodis) != 1 || whodis[0].ctx.Err() != nil {
			t.Fatal("the race ended a whodis hearing alt for another dialer's win")
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		whodis = testEval.dialsOf(lastResortTestName)
		if whodis[0].err != nil || whodis[0].endTime.Sub(start) != lateStart+2*time.Second {
			t.Fatalf("whodis ended %s after the start with %v, want its handshake complete", whodis[0].endTime.Sub(start), whodis[0].err)
		}
		if strategy.lastResortWonOn(strategy.networkIdSnapshot()) {
			t.Fatal("b's win marked the network")
		}
	})

	for _, silentFor := range []time.Duration{2500 * time.Millisecond, time.Second} {
		synctest.Test(t, func(t *testing.T) {
			strategy, dialers, cancel := newLastResortTestStrategy(4, "a", "b", "c")
			defer cancel()
			lateStart := strategy.settings.AltWhodisLateStart
			stallTimeout := strategy.settings.AltWhodisRaceStallTimeout
			testEval := newLastResortTestEval()
			testEval.set(map[string]lastResortTestBehavior{
				"a":                lastResortTestBlackhole,
				"b":                bAnswersAfter(lateStart, silentFor),
				"c":                lastResortTestBlackhole,
				lastResortTestName: lastResortTestWhodis(time.Hour, false),
			})
			start := time.Now()
			if result := strategy.parallelEval(context.Background(), false, testEval.eval); result == nil || result.dialer != dialers["b"] {
				t.Fatalf("winner=%v", result)
			}
			synctest.Wait()
			whodis := testEval.dialsOf(lastResortTestName)[0]
			stalled := stallTimeout <= silentFor
			if ended := whodis.ctx.Err() != nil; ended != stalled {
				t.Fatalf("silent %s at the race's end: ended=%t, want %t", silentFor, ended, stalled)
			}
			if stalled {
				return
			}
			// silent for less than the stall timeout: on to the request's deadline
			time.Sleep(time.Until(start.Add(strategy.settings.RequestTimeout)))
			synctest.Wait()
			whodis = testEval.dialsOf(lastResortTestName)[0]
			if !errors.Is(whodis.err, context.DeadlineExceeded) || whodis.endTime.Sub(start) != strategy.settings.RequestTimeout {
				t.Fatalf("the silent whodis ended %s after the start with %v, want the request's deadline", whodis.endTime.Sub(start), whodis.err)
			}
		})
	}
}

// On a network where whodis last won, it starts with the others and wins at
// its handshake time; another dialer's win clears that, after which a race
// whose others connect never dials it.
func TestParallelEvalStartsTheLastResortFirstWhereItWon(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		strategy, dialers, cancel := newLastResortTestStrategy(4, "a", "b", "c")
		defer cancel()
		const handshake = 700 * time.Millisecond
		testEval := newLastResortTestEval()
		testEval.set(map[string]lastResortTestBehavior{
			"a":                lastResortTestBlackhole,
			"b":                lastResortTestBlackhole,
			"c":                lastResortTestBlackhole,
			lastResortTestName: lastResortTestWhodis(handshake, true),
		})
		if result := strategy.parallelEval(context.Background(), false, testEval.eval); result == nil || result.dialer != dialers[lastResortTestName] {
			t.Fatalf("first winner=%v", result)
		}
		testEval.set(testEval.behaviors)
		start := time.Now()
		result := strategy.parallelEval(context.Background(), false, testEval.eval)
		if result == nil || result.dialer != dialers[lastResortTestName] || time.Since(start) != handshake {
			t.Fatalf("where whodis won: winner=%v after %s, want whodis at its handshake", result, time.Since(start))
		}

		// the network recovers: a wins, beside whodis, which goes on
		testEval.set(map[string]lastResortTestBehavior{
			"a":                lastResortTestSucceed,
			"b":                lastResortTestBlackhole,
			"c":                lastResortTestBlackhole,
			lastResortTestName: lastResortTestWhodis(handshake, true),
		})
		result = strategy.parallelEval(context.Background(), false, testEval.eval)
		if result == nil || result.dialer != dialers["a"] {
			t.Fatalf("recovered winner=%v", result)
		}
		if strategy.lastResortWonOn(strategy.networkIdSnapshot()) {
			t.Fatal("another dialer's win did not clear the mark")
		}
		synctest.Wait()
		time.Sleep(time.Second)
		testEval.set(testEval.behaviors)
		result = strategy.parallelEval(context.Background(), false, testEval.eval)
		if result == nil || result.dialer != dialers["a"] || testEval.lastResortDialed() {
			t.Fatalf("after the mark cleared: winner=%v last resort dialed=%t", result, testEval.lastResortDialed())
		}
	})
}

// A network change leaves the mark with the old network: the last resort
// starts late on the new one, so a race whose others connect never dials it.
func TestParallelEvalForgetsTheMarkOnANetworkChange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		strategy, dialers, cancel := newLastResortTestStrategy(8, "a", "b")
		defer cancel()
		strategy.noteRaceWinner(strategy.networkIdSnapshot(), dialers[lastResortTestName])
		strategy.networkChanged()
		if strategy.lastResortWonOn(strategy.networkIdSnapshot()) {
			t.Fatal("the new network inherited the mark")
		}
		testEval := newLastResortTestEval()
		testEval.set(map[string]lastResortTestBehavior{
			"a":                lastResortTestSucceed,
			"b":                lastResortTestBlackhole,
			lastResortTestName: lastResortTestSucceed,
		})
		result := strategy.parallelEval(context.Background(), false, testEval.eval)
		if result == nil || result.dialer != dialers["a"] || testEval.lastResortDialed() {
			t.Fatalf("winner=%v last resort dialed=%t", result, testEval.lastResortDialed())
		}
	})
}
