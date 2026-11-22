package connect

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// The api race's last-resort dialer, alt whodis (parallelEvalWithRouteHedge):
// out of the parallel blocks and the GET hedge until the other dialers fail
// on the network, then alone, then leading the blocks on that network.

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

// Fake dials by dialer description, recording each entry: its order, how
// many other dials were in flight, and the attempt context's deadline.
type lastResortTestEval struct {
	stateLock       sync.Mutex
	behaviors       map[string]lastResortTestBehavior
	entered         []string
	inflight        int
	inflightAtEntry map[string]int
	deadlines       map[string]time.Time
}

func newLastResortTestEval() *lastResortTestEval {
	return &lastResortTestEval{
		behaviors:       map[string]lastResortTestBehavior{},
		inflightAtEntry: map[string]int{},
		deadlines:       map[string]time.Time{},
	}
}

func (self *lastResortTestEval) set(behaviors map[string]lastResortTestBehavior) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.behaviors = behaviors
	self.entered = nil
}

func (self *lastResortTestEval) eval(ctx context.Context, dialer *clientDialer) *evalResult {
	behavior := func() lastResortTestBehavior {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.entered = append(self.entered, dialer.description)
		self.inflightAtEntry[dialer.description] = self.inflight
		self.inflight += 1
		if deadline, ok := ctx.Deadline(); ok {
			self.deadlines[dialer.description] = deadline
		}
		return self.behaviors[dialer.description]
	}()
	defer func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.inflight -= 1
	}()
	err := behavior(ctx)
	dialer.Update(ctx, err)
	return &evalResult{err: err}
}

func (self *lastResortTestEval) enteredNames() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.entered)
}

// A bare strategy of the named primary dialers, each of full weight, and one
// last-resort dialer of almost no weight, which a weighted shuffle all but
// never puts first.
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
		minimumWeight: 0.0001,
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

// A race whose other dialers work never dials the last resort: not in the
// parallel blocks, which here hold every dialer at once, and not in the GET
// hedge that a stale preferred dialer opens.
func TestParallelEvalKeepsTheLastResortOutOfTheRace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		strategy, dialers, cancel := newLastResortTestStrategy(8, "a", "b", "c")
		defer cancel()
		testEval := newLastResortTestEval()
		// a wins once b and c are out, so every dial the race starts at once
		// has started by then
		othersOut := make(chan struct{}, 2)
		testEval.set(map[string]lastResortTestBehavior{
			"a": func(ctx context.Context) error {
				for range 2 {
					<-othersOut
				}
				return nil
			},
			"b": func(ctx context.Context) error {
				othersOut <- struct{}{}
				return lastResortTestBlackhole(ctx)
			},
			"c": func(ctx context.Context) error {
				othersOut <- struct{}{}
				return lastResortTestBlackhole(ctx)
			},
			lastResortTestName: lastResortTestSucceed,
		})
		result := strategy.parallelEval(context.Background(), false, testEval.eval)
		if result == nil || result.dialer != dialers["a"] {
			t.Fatalf("winner=%v", result)
		}
		if slices.Contains(testEval.enteredNames(), lastResortTestName) {
			t.Fatalf("the parallel blocks dialed the last resort: %v", testEval.enteredNames())
		}

		// a delivered on this network, then went stale: the hedge opens the
		// others after the head start, and b wins
		testEval.set(map[string]lastResortTestBehavior{
			"a":                lastResortTestBlackhole,
			"b":                lastResortTestSucceed,
			"c":                lastResortTestBlackhole,
			lastResortTestName: lastResortTestSucceed,
		})
		result = strategy.parallelEvalWithRouteHedge(
			context.Background(), false, testEval.eval, preferredHttpAttemptContext, time.Second)
		if result == nil || result.dialer != dialers["b"] {
			t.Fatalf("hedged winner=%v", result)
		}
		entered := testEval.enteredNames()
		if len(entered) == 0 || entered[0] != "a" || slices.Contains(entered, lastResortTestName) {
			t.Fatalf("the hedge dialed %v", entered)
		}
		if strategy.primaryDialersFailedOn(strategy.networkIdSnapshot()) {
			t.Fatal("a network where the others won is marked")
		}
	})
}

// Once every other dialer of a round has failed, the last resort runs alone,
// with the request's whole remaining deadline, and wins; the network is
// marked.
func TestParallelEvalRunsTheLastResortAloneOnceTheOthersFail(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		strategy, dialers, cancel := newLastResortTestStrategy(8, "a", "b", "c")
		defer cancel()
		testEval := newLastResortTestEval()
		testEval.set(map[string]lastResortTestBehavior{
			"a":                lastResortTestFail,
			"b":                lastResortTestFail,
			"c":                lastResortTestFail,
			lastResortTestName: lastResortTestSucceed,
		})
		start := time.Now()
		result := strategy.parallelEval(context.Background(), false, testEval.eval)
		if result == nil || result.dialer != dialers[lastResortTestName] {
			t.Fatalf("winner=%v", result)
		}
		entered := testEval.enteredNames()
		if len(entered) != 4 || entered[3] != lastResortTestName {
			t.Fatalf("dials=%v, want the last resort after the others", entered)
		}
		testEval.stateLock.Lock()
		inflight := testEval.inflightAtEntry[lastResortTestName]
		deadline := testEval.deadlines[lastResortTestName]
		testEval.stateLock.Unlock()
		if inflight != 0 {
			t.Fatalf("the last resort ran beside %d other dials", inflight)
		}
		if !deadline.Equal(start.Add(strategy.settings.RequestTimeout)) {
			t.Fatalf("the last resort's deadline=%s, want the request's", deadline.Sub(start))
		}
		if !strategy.primaryDialersFailedOn(strategy.networkIdSnapshot()) {
			t.Fatal("the network the others failed on is not marked")
		}
	})
}

// On a network where the others failed, the last resort leads the first
// parallel block: with one dial a block, it is dialed first and wins while
// the others hold their dials to the deadline. A win by another dialer then
// clears the mark, and the last resort is out of the race again.
func TestParallelEvalLeadsWithTheLastResortWhereTheOthersFailed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		strategy, dialers, cancel := newLastResortTestStrategy(1, "a", "b", "c")
		defer cancel()
		strategy.notePrimaryDialersFailed(strategy.networkIdSnapshot())
		testEval := newLastResortTestEval()
		testEval.set(map[string]lastResortTestBehavior{
			"a":                lastResortTestBlackhole,
			"b":                lastResortTestBlackhole,
			"c":                lastResortTestBlackhole,
			lastResortTestName: lastResortTestSucceed,
		})
		result := strategy.parallelEval(context.Background(), false, testEval.eval)
		if result == nil || result.dialer != dialers[lastResortTestName] {
			t.Fatalf("winner=%v dials=%v", result, testEval.enteredNames())
		}
		if entered := testEval.enteredNames(); entered[0] != lastResortTestName {
			t.Fatalf("dials=%v, want the last resort first", entered)
		}
		if !strategy.primaryDialersFailedOn(strategy.networkIdSnapshot()) {
			t.Fatal("the last resort's win cleared the mark")
		}

		// the network recovers: with room for every dial, a wins
		strategy.settings.ParallelBlockSize = 8
		testEval.set(map[string]lastResortTestBehavior{
			"a":                lastResortTestSucceed,
			"b":                lastResortTestBlackhole,
			"c":                lastResortTestBlackhole,
			lastResortTestName: lastResortTestBlackhole,
		})
		result = strategy.parallelEval(context.Background(), false, testEval.eval)
		if result == nil || result.dialer != dialers["a"] {
			t.Fatalf("winner=%v", result)
		}
		if strategy.primaryDialersFailedOn(strategy.networkIdSnapshot()) {
			t.Fatal("another dialer's win did not clear the mark")
		}
		testEval.set(map[string]lastResortTestBehavior{
			"a":                lastResortTestSucceed,
			"b":                lastResortTestBlackhole,
			"c":                lastResortTestBlackhole,
			lastResortTestName: lastResortTestBlackhole,
		})
		result = strategy.parallelEval(context.Background(), false, testEval.eval)
		if result == nil || result.dialer != dialers["a"] || slices.Contains(testEval.enteredNames(), lastResortTestName) {
			t.Fatalf("after the mark cleared: winner=%v dials=%v", result, testEval.enteredNames())
		}
	})
}

// The request's own deadline passing with the others out and no winner marks
// the network, so the next request leads with the last resort; a caller's
// shorter deadline does not.
func TestParallelEvalMarksTheNetworkAtTheRequestDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		strategy, dialers, cancel := newLastResortTestStrategy(4, "a", "b", "c")
		defer cancel()
		testEval := newLastResortTestEval()
		testEval.set(map[string]lastResortTestBehavior{
			"a":                lastResortTestBlackhole,
			"b":                lastResortTestBlackhole,
			"c":                lastResortTestBlackhole,
			lastResortTestName: lastResortTestSucceed,
		})
		callerCtx, callerCancel := context.WithTimeout(context.Background(), strategy.settings.RequestTimeout/2)
		defer callerCancel()
		if result := strategy.parallelEval(callerCtx, false, testEval.eval); result != nil {
			t.Fatalf("a blackholed race won: %v", result)
		}
		if strategy.primaryDialersFailedOn(strategy.networkIdSnapshot()) {
			t.Fatal("the caller's own deadline marked the network")
		}

		if result := strategy.parallelEval(context.Background(), false, testEval.eval); result != nil {
			t.Fatalf("a blackholed race won: %v", result)
		}
		if slices.Contains(testEval.enteredNames(), lastResortTestName) {
			t.Fatal("the last resort raced on an unmarked network")
		}
		if !strategy.primaryDialersFailedOn(strategy.networkIdSnapshot()) {
			t.Fatal("the request deadline did not mark the network")
		}
		result := strategy.parallelEval(context.Background(), false, testEval.eval)
		if result == nil || result.dialer != dialers[lastResortTestName] {
			t.Fatalf("the marked network's winner=%v", result)
		}
	})
}

// A network change leaves the mark with the old network: the last resort is
// out of the race on the new one.
func TestParallelEvalForgetsTheMarkOnANetworkChange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		strategy, dialers, cancel := newLastResortTestStrategy(8, "a", "b")
		defer cancel()
		strategy.notePrimaryDialersFailed(strategy.networkIdSnapshot())
		strategy.networkChanged()
		if strategy.primaryDialersFailedOn(strategy.networkIdSnapshot()) {
			t.Fatal("the new network inherited the mark")
		}
		testEval := newLastResortTestEval()
		testEval.set(map[string]lastResortTestBehavior{
			"a":                lastResortTestSucceed,
			"b":                lastResortTestBlackhole,
			lastResortTestName: lastResortTestSucceed,
		})
		result := strategy.parallelEval(context.Background(), false, testEval.eval)
		if result == nil || result.dialer != dialers["a"] || slices.Contains(testEval.enteredNames(), lastResortTestName) {
			t.Fatalf("winner=%v dials=%v", result, testEval.enteredNames())
		}
	})
}
