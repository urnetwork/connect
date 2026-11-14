package connect

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// Every recorded outcome is visited exactly once under its own labels, values
// outside the vocabularies are dropped, and a nil instance is inert.
func TestWindowClientStatsOutcomes(t *testing.T) {
	stats := NewWindowClientStats()
	for state := range windowExpandPassBuildStateCount {
		for outcome := range windowCandidateOutcomeCount {
			for range int(state)*int(windowCandidateOutcomeCount) + int(outcome) + 1 {
				stats.addCandidate(WindowTypeQuality, state, outcome)
			}
		}
	}
	stats.addCandidate(WindowTypeSpeed, windowExpandPassBelowMin, windowCandidateFailedPing)
	stats.addCandidate(WindowTypeAuto, windowExpandPassBelowMin, windowCandidateCanceled)
	// a pass state that builds nothing, and values outside the vocabularies
	stats.addCandidate(WindowTypeQuality, windowExpandPassDeferred, windowCandidateAdmitted)
	stats.addCandidate(WindowTypeQuality, windowExpandPassBelowMin, windowCandidateOutcomeCount)
	stats.addCandidate(WindowTypeQuality, windowExpandPassState(-1), windowCandidateAdmitted)

	seen := map[string]uint64{}
	stats.VisitCandidates(func(window string, state string, outcome string, count uint64) {
		key := window + "/" + state + "/" + outcome
		if _, ok := seen[key]; ok {
			t.Errorf("candidate %s visited twice", key)
		}
		seen[key] = count
	})
	want := int(windowStatsWindowCount) * int(windowExpandPassBuildStateCount) * int(windowCandidateOutcomeCount)
	if len(seen) != want {
		t.Fatalf("visited %d candidate counters, want %d", len(seen), want)
	}
	for state := range windowExpandPassBuildStateCount {
		for outcome := range windowCandidateOutcomeCount {
			key := "quality/" + state.String() + "/" + outcome.String()
			if wantCount := uint64(int(state)*int(windowCandidateOutcomeCount) + int(outcome) + 1); seen[key] != wantCount {
				t.Errorf("%s = %d, want %d", key, seen[key], wantCount)
			}
		}
	}
	if seen["speed/below_min/failed_ping"] != 1 || seen["auto/below_min/canceled"] != 1 {
		t.Errorf("windows are not counted apart: speed=%d auto=%d", seen["speed/below_min/failed_ping"], seen["auto/below_min/canceled"])
	}
	for key := range seen {
		for _, unknown := range []string{"unknown", "none"} {
			if strings.Contains(key, unknown) {
				t.Errorf("candidate label %s carries %s", key, unknown)
			}
		}
	}

	stats.addExpandPass(WindowTypeSpeed, windowExpandPassStrictHole)
	stats.addExpandPass(WindowTypeSpeed, windowExpandPassStrictHole)
	stats.addExpandPass(WindowTypeQuality, windowExpandPassStateCount)
	passes := map[string]uint64{}
	stats.VisitExpandPasses(func(window string, state string, count uint64) {
		passes[window+"/"+state] = count
	})
	if len(passes) != int(windowStatsWindowCount)*int(windowExpandPassStateCount) || passes["speed/strict_hole"] != 2 {
		t.Errorf("expand passes = %v", passes)
	}

	stats.addWarning(WindowTypeQuality, windowWarningDrain)
	stats.addWarning(WindowTypeQuality, windowWarningNone)
	warnings := map[string]uint64{}
	stats.VisitWarnings(func(window string, cause string, count uint64) {
		warnings[window+"/"+cause] = count
	})
	if len(warnings) != int(windowStatsWindowCount)*int(windowWarningCauseCount-1) || warnings["quality/drain"] != 1 {
		t.Errorf("warnings = %v", warnings)
	}
	if _, ok := warnings["quality/none"]; ok {
		t.Error("a warning without a cause has a series")
	}

	stats.addRemoval(WindowTypeQuality, windowRemovalHandover)
	stats.addRemoval(WindowTypeQuality, windowRemovalCauseCount)
	removals := map[string]uint64{}
	stats.VisitRemovals(func(window string, cause string, count uint64) {
		removals[window+"/"+cause] = count
	})
	if len(removals) != int(windowStatsWindowCount)*int(windowRemovalCauseCount) || removals["quality/handover"] != 1 {
		t.Errorf("removals = %v", removals)
	}

	// a window moves between states and its retraction leaves nothing behind
	stats.moveWindowState(WindowTypeQuality, windowStateNone, windowStateSelectableFull)
	stats.moveWindowState(WindowTypeQuality, windowStateSelectableFull, windowStateStrictHole)
	stats.moveWindowState(WindowTypeSpeed, windowStateNone, windowStateNoSelectable)
	windows := map[string]int64{}
	stats.VisitWindows(func(window string, state string, count int64) {
		windows[window+"/"+state] = count
	})
	if len(windows) != int(windowStatsWindowCount)*int(windowStateCount-1) ||
		windows["quality/strict_hole"] != 1 || windows["quality/selectable_full"] != 0 || windows["speed/no_selectable"] != 1 {
		t.Errorf("windows = %v", windows)
	}
	stats.moveWindowState(WindowTypeQuality, windowStateStrictHole, windowStateNone)
	stats.moveWindowState(WindowTypeSpeed, windowStateNoSelectable, windowStateNone)
	stats.VisitWindows(func(window string, state string, count int64) {
		if count != 0 {
			t.Errorf("retracted windows left %s/%s = %d", window, state, count)
		}
	})

	var nilStats *WindowClientStats
	nilStats.addCandidate(WindowTypeQuality, windowExpandPassBelowMin, windowCandidateAdmitted)
	nilStats.addExpandPass(WindowTypeQuality, windowExpandPassBelowMin)
	nilStats.addWarning(WindowTypeQuality, windowWarningDrain)
	nilStats.addRemoval(WindowTypeQuality, windowRemovalVerdict)
	nilStats.moveWindowState(WindowTypeQuality, windowStateNone, windowStateSelectableFull)
	visited := 0
	nilStats.VisitCandidates(func(string, string, string, uint64) { visited += 1 })
	nilStats.VisitExpandPasses(func(string, string, uint64) { visited += 1 })
	nilStats.VisitWarnings(func(string, string, uint64) { visited += 1 })
	nilStats.VisitRemovals(func(string, string, uint64) { visited += 1 })
	nilStats.VisitWindows(func(string, string, int64) { visited += 1 })
	if visited != 0 {
		t.Errorf("a nil instance visited %d counters", visited)
	}
	if (&multiClientWindow{}).windowClientStats() != nil {
		t.Error("a window without settings counts")
	}
}

func TestWindowStateOfPartitionsActiveWindows(t *testing.T) {
	cases := []struct {
		name       string
		selectable int
		installed  int
		target     int
		min        int
		fixed      bool
		strict     bool
		hardMax    int
		want       windowState
	}{
		{name: "disabled", selectable: 0, installed: 0, target: 0, min: 0, want: windowStateNone},
		{name: "full", selectable: 5, installed: 5, target: 5, min: 5, strict: true, hardMax: 5, want: windowStateSelectableFull},
		{name: "strict drained slot", selectable: 4, installed: 5, target: 5, min: 5, strict: true, hardMax: 5, want: windowStateStrictHole},
		{name: "strict free slot", selectable: 4, installed: 4, target: 5, min: 5, strict: true, hardMax: 5, want: windowStateBelowMin},
		{name: "every exit drained", selectable: 0, installed: 5, target: 5, min: 5, strict: true, hardMax: 5, want: windowStateNoSelectable},
		{name: "non-strict drained slot", selectable: 4, installed: 5, target: 5, min: 5, hardMax: 5, want: windowStateBelowMin},
		{name: "spare missing", selectable: 6, installed: 6, target: 7, min: 6, hardMax: 7, want: windowStateBelowTarget},
		{name: "strict spare missing at ceiling", selectable: 6, installed: 7, target: 7, min: 6, strict: true, hardMax: 7, want: windowStateStrictHole},
		{name: "fixed warned sole peer", selectable: 0, installed: 1, target: 1, min: 1, fixed: true, want: windowStateSelectableFull},
		{name: "fixed peer missing", selectable: 0, installed: 0, target: 1, min: 1, fixed: true, want: windowStateNoSelectable},
		{name: "empty", selectable: 0, installed: 0, target: 6, min: 6, want: windowStateNoSelectable},
	}
	for _, c := range cases {
		if got := windowStateOf(c.selectable, c.installed, c.target, c.min, c.fixed, c.strict, c.hardMax); got != c.want {
			t.Errorf("%s: windowStateOf = %s, want %s", c.name, got, c.want)
		}
	}
}

func TestWindowRemovalCauseOf(t *testing.T) {
	cases := []struct {
		err  error
		want windowRemovalCause
	}{
		{err: errors.New("Done."), want: windowRemovalClosed},
		{err: nil, want: windowRemovalClosed},
		{err: errTransportDownTimeout, want: windowRemovalTransportDown},
		{err: fmt.Errorf("wrapped: %w", errTransportDownTimeout), want: windowRemovalTransportDown},
		{err: errors.New("Blackhole no-send-ack (send 3/0B)"), want: windowRemovalVerdict},
		{err: errors.New("send stalled 3s"), want: windowRemovalVerdict},
		{err: errContractReliability, want: windowRemovalVerdict},
		{err: errors.New("Send sequence closed."), want: windowRemovalVerdict},
	}
	for _, c := range cases {
		if got := windowRemovalCauseOf(c.err); got != c.want {
			t.Errorf("windowRemovalCauseOf(%v) = %s, want %s", c.err, got, c.want)
		}
	}
}

func TestWindowExpandPassStateOf(t *testing.T) {
	cases := []struct {
		kept, installed, min int
		strict               bool
		hardMax              int
		want                 windowExpandPassState
	}{
		{kept: 0, installed: 0, min: 5, strict: true, hardMax: 5, want: windowExpandPassBelowMin},
		{kept: 4, installed: 5, min: 5, strict: true, hardMax: 5, want: windowExpandPassStrictHole},
		{kept: 3, installed: 4, min: 5, strict: true, hardMax: 5, want: windowExpandPassBelowMin},
		{kept: 4, installed: 5, min: 5, strict: false, hardMax: 5, want: windowExpandPassBelowMin},
		{kept: 6, installed: 6, min: 6, strict: false, hardMax: 7, want: windowExpandPassBelowTarget},
		{kept: 0, installed: 1, min: 1, strict: true, hardMax: 0, want: windowExpandPassBelowMin},
	}
	for _, c := range cases {
		if got := windowExpandPassStateOf(c.kept, c.installed, c.min, c.strict, c.hardMax); got != c.want {
			t.Errorf("windowExpandPassStateOf(%d, %d, %d, %t, %d) = %s, want %s", c.kept, c.installed, c.min, c.strict, c.hardMax, got, c.want)
		}
	}
}

func TestWindowCandidateFailureOutcome(t *testing.T) {
	AssertEqual(t, windowCandidateFailureOutcome(true, true), windowCandidateCanceled)
	AssertEqual(t, windowCandidateFailureOutcome(true, false), windowCandidateCanceled)
	AssertEqual(t, windowCandidateFailureOutcome(false, true), windowCandidateFailedContract)
	AssertEqual(t, windowCandidateFailureOutcome(false, false), windowCandidateFailedPing)
}

// A warning is counted when a client enters it, once per transition, not once
// per resize pass; a quarantine counts as its own cause; a client warned for
// the size math only, which new flows may still choose, is not counted.
func TestWindowWarningTransitionsCountOncePerTransition(t *testing.T) {
	stats := NewWindowClientStats()
	transitions := windowWarningTransitions{}
	drained := &multiClientChannel{}
	benched := &multiClientChannel{}
	shielded := &multiClientChannel{}
	drained.setWarning(true, warnDraining)
	benched.stateLock.Lock()
	benched.quarantined = true
	benched.stateLock.Unlock()
	shielded.setWarning(false, warnUnhealthy)

	warningCount := func(cause windowWarningCause) uint64 {
		return stats.warnings[windowStatsWindowQuality][cause].Load()
	}
	for range 3 {
		transitions.observe(stats, WindowTypeQuality, []*multiClientChannel{drained, benched, shielded})
	}
	AssertEqual(t, warningCount(windowWarningDrain), uint64(1))
	AssertEqual(t, warningCount(windowWarningQuarantine), uint64(1))
	AssertEqual(t, warningCount(windowWarningUnhealthy), uint64(0))

	// a new cause is a new transition
	drained.setWarning(true, warnUnhealthy)
	transitions.observe(stats, WindowTypeQuality, []*multiClientChannel{drained, benched})
	AssertEqual(t, warningCount(windowWarningUnhealthy), uint64(1))

	// kept for a pass, then warned again
	drained.setWarning(false, warnNone)
	transitions.observe(stats, WindowTypeQuality, []*multiClientChannel{benched})
	drained.setWarning(true, warnUnhealthy)
	transitions.observe(stats, WindowTypeQuality, []*multiClientChannel{drained, benched})
	AssertEqual(t, warningCount(windowWarningUnhealthy), uint64(2))
	AssertEqual(t, warningCount(windowWarningQuarantine), uint64(1))

	// nil counters record nothing and keep no state
	off := windowWarningTransitions{}
	off.observe(nil, WindowTypeQuality, []*multiClientChannel{drained})
	if off.clientCauses != nil {
		t.Error("observing without counters kept a record")
	}
}

// Ages one window client past a lifetime as if its first event had happened
// `age` ago. The next resize pass drains it.
func ageWindowClientForTest(client *multiClientChannel, lifetime time.Duration, age time.Duration) {
	client.stateLock.Lock()
	defer client.stateLock.Unlock()
	client.firstEventTime = time.Now().Add(-age)
	client.effectiveLifetime = lifetime
}

// A strict 5/5/5 quality window over five in-process providers with the
// process counters attached; the generator also offers one spare provider
// once the window has formed.
func newWindowStatsTestHarness(t *testing.T, configure ...func(*MultiClientSettings)) (*reraceHarness, *discoveryDemandTestGenerator, *WindowClientStats) {
	t.Helper()
	stats := NewWindowClientStats()
	specs := []reraceExitSpec{{name: "a"}, {name: "b"}, {name: "c"}, {name: "d"}, {name: "e"}}
	configureAll := append([]func(*MultiClientSettings){func(settings *MultiClientSettings) {
		settings.WindowClientStats = stats
		settings.MaxFlowsPerExit = DefaultMultiClientSettings().MaxFlowsPerExit
		// the host's opt-in, as the proxy service sets it
		settings.StrictWindowHandover = true
	}}, configure...)
	h, generator := newDiscoveryDemandTestHarness(t, specs, configureAll...)
	synctest.Wait()
	h.addExit(reraceExitSpec{name: "spare"})
	return h, generator, stats
}

func windowStatsCandidates(stats *WindowClientStats, state windowExpandPassState, outcome windowCandidateOutcome) uint64 {
	return stats.candidates[windowStatsWindowQuality][state][outcome].Load()
}

func windowStatsCandidateTotal(stats *WindowClientStats, outcome windowCandidateOutcome) uint64 {
	total := uint64(0)
	for state := range windowExpandPassBuildStateCount {
		total += windowStatsCandidates(stats, state, outcome)
	}
	return total
}

func windowStatsWindows(stats *WindowClientStats, state windowState) int64 {
	return stats.windows[windowStatsWindowQuality][state].Load()
}

// The baseline loop the counters exist to size, which the handover's kill
// switch restores: a strict window at its hard max whose drained client keeps
// its slot builds a candidate every pass, the strict hard max declines it,
// and the window sits in a strict hole. Formation counts as admissions below
// the minimum, the drain as one warning however many passes see it, and
// closing the window retracts its state and counts every installed client as
// closed.
func TestWindowClientStatsCountTheStrictDrainLoop(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		h, _, stats := newWindowStatsTestHarness(t, func(settings *MultiClientSettings) {
			settings.StrictWindowHandover = false
		})
		window := h.multiClient.windows[WindowTypeQuality]
		if admitted := windowStatsCandidates(stats, windowExpandPassBelowMin, windowCandidateAdmitted); admitted != 5 {
			t.Fatalf("formation admitted %d candidates below the minimum, want 5", admitted)
		}
		if total := windowStatsCandidateTotal(stats, windowCandidateAdmitted); total != 5 {
			t.Fatalf("formation admitted %d candidates in all, want 5", total)
		}
		h.sleep(time.Second)
		AssertEqual(t, windowStatsWindows(stats, windowStateSelectableFull), int64(1))

		drained := h.liveChannels()[0]
		ageWindowClientForTest(drained, time.Minute, 2*time.Minute)
		h.sleep(20 * time.Second)
		if passes := stats.expandPasses[windowStatsWindowQuality][windowExpandPassStrictHole].Load(); passes < 1 {
			t.Fatalf("no expand pass counted in the strict hole")
		}
		if declined := windowStatsCandidates(stats, windowExpandPassStrictHole, windowCandidateDeclinedStrictCap); declined < 1 {
			t.Fatalf("the strict hole's candidate was not counted as declined by the strict cap")
		}
		AssertEqual(t, windowStatsWindows(stats, windowStateStrictHole), int64(1))
		AssertEqual(t, windowStatsWindows(stats, windowStateSelectableFull), int64(0))

		h.sleep(3 * time.Minute)
		if declined := windowStatsCandidates(stats, windowExpandPassStrictHole, windowCandidateDeclinedStrictCap); declined < 2 {
			t.Fatalf("the strict hole declined %d candidates in three minutes, want the loop to repeat", declined)
		}
		if deferred := stats.expandPasses[windowStatsWindowQuality][windowExpandPassDeferred].Load(); deferred < 1 {
			t.Fatalf("no pass was counted as deferred by the expansion backoff")
		}
		AssertEqual(t, stats.warnings[windowStatsWindowQuality][windowWarningDrain].Load(), uint64(1))
		func() {
			window.stateLock.Lock()
			defer window.stateLock.Unlock()
			if window.clients[drained.ClientId()] != drained || len(window.clients) != 5 {
				t.Fatalf("the drained client left its slot or the window grew: %d installed", len(window.clients))
			}
		}()

		if err := h.multiClient.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		AssertEqual(t, stats.removals[windowStatsWindowQuality][windowRemovalClosed].Load(), uint64(5))
		stats.VisitWindows(func(window string, state string, count int64) {
			if count != 0 {
				t.Errorf("a closed window is still counted in %s/%s = %d", window, state, count)
			}
		})
	})
}

// A removal is counted once, by its cause: a cancellation with no other error
// as closed, a verdict error, and the transport-down timeout.
func TestWindowClientStatsCountRemovalsByCause(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		h, _, stats := newWindowStatsTestHarness(t)
		h.addExit(reraceExitSpec{name: "spare-b"})
		h.addExit(reraceExitSpec{name: "spare-c"})
		removals := func(cause windowRemovalCause) uint64 {
			return stats.removals[windowStatsWindowQuality][cause].Load()
		}

		h.liveChannels()[0].Cancel()
		h.sleep(20 * time.Second)
		AssertEqual(t, removals(windowRemovalClosed), uint64(1))

		h.liveChannels()[0].addError(errors.New("Blackhole no-send-ack (test)"))
		h.sleep(20 * time.Second)
		AssertEqual(t, removals(windowRemovalVerdict), uint64(1))

		h.liveChannels()[0].addError(errTransportDownTimeout)
		h.sleep(20 * time.Second)
		AssertEqual(t, removals(windowRemovalTransportDown), uint64(1))
		AssertEqual(t, removals(windowRemovalClosed), uint64(1))
		AssertEqual(t, removals(windowRemovalVerdict), uint64(1))

		h.waitForExits(5)
		if err := h.multiClient.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		AssertEqual(t, removals(windowRemovalClosed), uint64(1+5))
		AssertEqual(t, removals(windowRemovalUnhealthy)+removals(windowRemovalCollapse)+removals(windowRemovalHandover)+removals(windowRemovalFamilySwap), uint64(0))
	})
}

// Starts the lifecycle fixture's single-candidate pass under a plan, so a
// candidate is counted with the plan's pass state.
func startExpandLifecyclePass(fixture *multiClientExpandLifecycleFixture, pass windowExpandPassPlan) <-chan int {
	expandDone := make(chan int, 1)
	go func() {
		expandDone <- fixture.window.expand(
			WindowSizeSettings{WindowSizeMin: 1, WindowSizeMax: 1, WindowSizeHardMax: 1},
			0,
			0,
			1,
			1,
			1,
			0,
			pass,
		)
	}()
	return expandDone
}

// The candidate the lifecycle fixture offers, read and offered again.
func expandLifecycleOfferedArgs(fixture *multiClientExpandLifecycleFixture) *multiClientChannelArgs {
	args := <-fixture.window.clientChannelArgs
	fixture.window.clientChannelArgs <- args
	return args
}

func newExpandLifecycleStatsFixture(t *testing.T) (*multiClientExpandLifecycleFixture, *WindowClientStats) {
	t.Helper()
	fixture := newMultiClientExpandLifecycleFixture(t)
	stats := NewWindowClientStats()
	fixture.window.settings.WindowClientStats = stats
	return fixture, stats
}

// Checks that exactly one candidate was counted, with the state and outcome
// given.
func assertOnlyCandidateOutcome(t *testing.T, stats *WindowClientStats, state windowExpandPassState, outcome windowCandidateOutcome) {
	t.Helper()
	stats.VisitCandidates(func(window string, visitedState string, visitedOutcome string, count uint64) {
		want := uint64(0)
		if window == "quality" && visitedState == state.String() && visitedOutcome == outcome.String() {
			want = 1
		}
		if count != want {
			t.Errorf("candidates %s/%s/%s = %d, want %d", window, visitedState, visitedOutcome, count, want)
		}
	})
}

func TestWindowClientStatsCountAdmittedCandidate(t *testing.T) {
	fixture, stats := newExpandLifecycleStatsFixture(t)
	expandDone := startExpandLifecyclePass(fixture, windowExpandPassPlan{state: windowExpandPassBelowTarget})
	fixture.wait(t, "held initial-ping result", fixture.pingResultEntered)
	fixture.releasePing()
	if got := fixture.result(t, expandDone); got != 1 {
		t.Fatalf("admissions=%d, want 1", got)
	}
	assertOnlyCandidateOutcome(t, stats, windowExpandPassBelowTarget, windowCandidateAdmitted)
}

func TestWindowClientStatsCountPingTimeoutAsFailedPing(t *testing.T) {
	fixture, stats := newExpandLifecycleStatsFixture(t)
	expirePing := make(chan struct{})
	fixture.window.expireExpandPingForTest = expirePing
	expandDone := startExpandLifecyclePass(fixture, windowExpandPassPlan{state: windowExpandPassBelowMin})
	fixture.wait(t, "held initial-ping result", fixture.pingResultEntered)
	fixture.send(t, "initial-ping timeout", expirePing)
	if got := fixture.result(t, expandDone); got != 0 {
		t.Fatalf("timed-out ping admissions=%d, want 0", got)
	}
	fixture.releasePing()
	fixture.wait(t, "delayed initial-ping callback", fixture.pingResultDone)
	fixture.wait(t, "timed-out client cleanup", fixture.clientRemoved)
	assertOnlyCandidateOutcome(t, stats, windowExpandPassBelowMin, windowCandidateFailedPing)
}

func TestWindowClientStatsCountLifecycleCancellationAsCanceled(t *testing.T) {
	fixture, stats := newExpandLifecycleStatsFixture(t)
	expandDone := startExpandLifecyclePass(fixture, windowExpandPassPlan{state: windowExpandPassBelowMin})
	fixture.wait(t, "held initial-ping result", fixture.pingResultEntered)
	fixture.cancelEvaluation()
	if got := fixture.result(t, expandDone); got != 0 {
		t.Fatalf("canceled pass admissions=%d, want 0", got)
	}
	fixture.releasePing()
	fixture.wait(t, "canceled initial-ping callback", fixture.pingResultDone)
	fixture.wait(t, "canceled client cleanup", fixture.clientRemoved)
	assertOnlyCandidateOutcome(t, stats, windowExpandPassBelowMin, windowCandidateCanceled)
}

func TestWindowClientStatsCountSetupFailure(t *testing.T) {
	fixture, stats := newExpandLifecycleStatsFixture(t)
	generator := fixture.window.generator.(*multiClientExpandLifecycleGenerator)
	generator.newClient = func(context.Context, *MultiClientGeneratorClientArgs, *ClientSettings) (*Client, error) {
		return nil, errors.New("synthetic setup refusal")
	}
	// the bare fixture logs no construction failure of its own
	fixture.window.createFailThrottle = newLogThrottle(DefaultMultiClientSettings().EvaluationFailureLogInterval)
	expandDone := startExpandLifecyclePass(fixture, windowExpandPassPlan{state: windowExpandPassBelowMin})
	if got := fixture.result(t, expandDone); got != 0 {
		t.Fatalf("refused setup admissions=%d, want 0", got)
	}
	assertOnlyCandidateOutcome(t, stats, windowExpandPassBelowMin, windowCandidateFailedSetup)
}

// The same identity's live client still carries flows, so the evaluated
// candidate is declined as a replacement.
func TestWindowClientStatsCountDeclinedReplacement(t *testing.T) {
	fixture, stats := newExpandLifecycleStatsFixture(t)
	offered := expandLifecycleOfferedArgs(fixture)
	liveCtx, cancelLive := context.WithCancel(context.Background())
	defer cancelLive()
	existing := &multiClientChannel{
		ctx: liveCtx,
		args: &multiClientChannelArgs{
			MultiClientGeneratorClientArgs: offered.MultiClientGeneratorClientArgs,
			Destination:                    offered.Destination,
		},
	}
	fixture.window.clients[offered.ClientId] = existing
	fixture.window.flowCountFunc = func(client *multiClientChannel) int {
		if client == existing {
			return 1
		}
		return 0
	}
	expandDone := startExpandLifecyclePass(fixture, windowExpandPassPlan{state: windowExpandPassStrictHole})
	fixture.wait(t, "held initial-ping result", fixture.pingResultEntered)
	fixture.releasePing()
	if got := fixture.result(t, expandDone); got != 0 {
		t.Fatalf("declined replacement admissions=%d, want 0", got)
	}
	fixture.wait(t, "declined client cleanup", fixture.clientRemoved)
	assertOnlyCandidateOutcome(t, stats, windowExpandPassStrictHole, windowCandidateDeclinedReplacement)
}

// A pass that needs one client evaluates two under the pool multiple and
// cancels the one it does not admit.
func TestWindowClientStatsCountSurplus(t *testing.T) {
	fixture, stats := newExpandLifecycleStatsFixture(t)
	fixture.window.settings.EvaluationPoolMultiple = 2
	offered := expandLifecycleOfferedArgs(fixture)
	secondArgs, err := fixture.window.generator.NewClientArgs()
	if err != nil {
		t.Fatal(err)
	}
	expandDone := startExpandLifecyclePass(fixture, windowExpandPassPlan{state: windowExpandPassBelowMin})
	fixture.wait(t, "held initial-ping result", fixture.pingResultEntered)
	fixture.window.clientChannelArgs <- &multiClientChannelArgs{
		MultiClientGeneratorClientArgs: *secondArgs,
		Destination:                    offered.Destination,
	}
	fixture.releasePing()
	if got := fixture.result(t, expandDone); got != 1 {
		t.Fatalf("pooled pass admissions=%d, want 1", got)
	}
	AssertEqual(t, windowStatsCandidates(stats, windowExpandPassBelowMin, windowCandidateAdmitted), uint64(1))
	AssertEqual(t, windowStatsCandidates(stats, windowExpandPassBelowMin, windowCandidateNotAdmittedSurplus), uint64(1))
	AssertEqual(t, windowStatsCandidateTotal(stats, windowCandidateDeclinedStrictCap), uint64(0))
}
