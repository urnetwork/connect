package connect

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// --- the failure classifier -------------------------------------------------

func TestClassifyWindowFailure(t *testing.T) {
	// nil keeps the call site's fallback
	AssertEqual(t, classifyWindowFailure(nil, windowFailurePlatform), windowFailurePlatform)
	AssertEqual(t, classifyWindowFailure(nil, windowFailureProvider), windowFailureProvider)

	// a timeout stays whatever the call site says it is
	AssertEqual(t,
		classifyWindowFailure(errors.New("generator call abandoned after 20s"), windowFailurePlatform),
		windowFailurePlatform)

	// the message-revealed classes win over the fallback
	AssertEqual(t,
		classifyWindowFailure(errors.New("api error: 429 Too Many Requests"), windowFailurePlatform),
		windowFailureRateLimit)
	AssertEqual(t,
		classifyWindowFailure(errors.New("rate limit exceeded"), windowFailureProvider),
		windowFailureRateLimit)
	AssertEqual(t,
		classifyWindowFailure(errors.New("401 unauthorized"), windowFailurePlatform),
		windowFailureAuth)
	AssertEqual(t,
		classifyWindowFailure(errors.New("auth error timeout"), windowFailureProvider),
		windowFailureAuth)
}

// --- the dominance derivation ----------------------------------------------

func TestDeriveStallReason(t *testing.T) {
	// no failures: plain evaluating
	AssertEqual(t, deriveStallReason([windowFailureClassCount]int{}), WindowStallEvaluating)

	// dominant class wins
	var counts [windowFailureClassCount]int
	counts[windowFailureProvider] = 5
	counts[windowFailurePlatform] = 1
	AssertEqual(t, deriveStallReason(counts), WindowStallProvidersUnresponsive)

	counts = [windowFailureClassCount]int{}
	counts[windowFailurePlatform] = 3
	counts[windowFailureProvider] = 2
	AssertEqual(t, deriveStallReason(counts), WindowStallPlatformUnreachable)

	// ties break to the sharper diagnosis
	counts = [windowFailureClassCount]int{}
	counts[windowFailureProvider] = 2
	counts[windowFailurePlatform] = 2
	AssertEqual(t, deriveStallReason(counts), WindowStallPlatformUnreachable)

	counts = [windowFailureClassCount]int{}
	counts[windowFailureRateLimit] = 1
	AssertEqual(t, deriveStallReason(counts), WindowStallRateLimited)

	counts = [windowFailureClassCount]int{}
	counts[windowFailureAuth] = 1
	AssertEqual(t, deriveStallReason(counts), WindowStallAuthFailing)
}

func TestWindowFailureRecorderHorizon(t *testing.T) {
	recorder := &windowFailureRecorder{}
	now := time.Now()

	recorder.record(windowFailureProvider, now.Add(-2*windowFailureHorizon))
	recorder.record(windowFailureProvider, now.Add(-time.Second))
	recorder.record(windowFailurePlatform, now)

	counts := recorder.counts(now)
	// the entry past the horizon is trimmed
	AssertEqual(t, counts[windowFailureProvider], 1)
	AssertEqual(t, counts[windowFailurePlatform], 1)

	// nil recorder (a bare fixture window) is safe on both paths
	var bare *windowFailureRecorder
	bare.record(windowFailureProvider, now)
	AssertEqual(t, bare.counts(now), [windowFailureClassCount]int{})
}

// --- the monitor stall status ----------------------------------------------

// SetStallStatus dispatches on change only, and AddWindowExpandEvent carries
// the diagnosis through unchanged — two writers, one struct, no clobbering.
func TestMonitorSetStallStatus(t *testing.T) {
	monitor := NewRemoteUserNatMultiClientMonitorWithDefaults()

	events := make(chan *WindowExpandEvent, 16)
	sub := monitor.AddMonitorEventCallback(func(windowExpandEvent *WindowExpandEvent, providerEvents map[Id]*ProviderEvent, reset bool) {
		events <- windowExpandEvent
	})
	defer sub()

	// the initial state reads evaluating
	AssertEqual(t, monitor.WindowExpandEvent().Reason, WindowStallEvaluating)
	AssertEqual(t, monitor.WindowExpandEvent().Failed, false)

	AssertEqual(t, monitor.SetStallStatus(WindowStallProvidersUnresponsive, false), true)
	select {
	case event := <-events:
		AssertEqual(t, event.Reason, WindowStallProvidersUnresponsive)
		AssertEqual(t, event.Failed, false)
	case <-time.After(5 * time.Second):
		t.Fatal("no dispatch for the reason change")
	}

	// unchanged: no dispatch, and the call says so
	AssertEqual(t, monitor.SetStallStatus(WindowStallProvidersUnresponsive, false), false)

	// the size half preserves the diagnosis
	monitor.AddWindowExpandEvent(false, 4)
	windowExpandEvent := monitor.WindowExpandEvent()
	AssertEqual(t, windowExpandEvent.TargetSize, 4)
	AssertEqual(t, windowExpandEvent.Reason, WindowStallProvidersUnresponsive)

	// ...and the diagnosis half preserves the size
	AssertEqual(t, monitor.SetStallStatus(WindowStallProvidersUnresponsive, true), true)
	windowExpandEvent = monitor.WindowExpandEvent()
	AssertEqual(t, windowExpandEvent.TargetSize, 4)
	AssertEqual(t, windowExpandEvent.Failed, true)
}

// the merged monitor: reason merges by sharpness, and Failed only when every
// window that is actually trying has failed
func TestMergedMonitorStallStatus(t *testing.T) {
	quality := NewRemoteUserNatMultiClientMonitorWithDefaults()
	speed := NewRemoteUserNatMultiClientMonitorWithDefaults()
	merged := NewMergedMultiClientMonitor([]MultiClientMonitor{quality, speed})

	// both idle: evaluating, not failed
	AssertEqual(t, merged.WindowExpandEvent().Reason, WindowStallEvaluating)
	AssertEqual(t, merged.WindowExpandEvent().Failed, false)

	// the sharper reason wins across windows
	quality.SetStallStatus(WindowStallProvidersUnresponsive, false)
	speed.SetStallStatus(WindowStallPlatformUnreachable, false)
	AssertEqual(t, merged.WindowExpandEvent().Reason, WindowStallPlatformUnreachable)

	// one window failed while the other is still trying (target > 0): not failed
	quality.AddWindowExpandEvent(false, 4)
	speed.AddWindowExpandEvent(false, 1)
	quality.SetStallStatus(WindowStallProvidersUnresponsive, true)
	AssertEqual(t, merged.WindowExpandEvent().Failed, false)

	// both trying windows failed: failed
	speed.SetStallStatus(WindowStallPlatformUnreachable, true)
	AssertEqual(t, merged.WindowExpandEvent().Failed, true)

	// a disabled window (target 0, not failed) must not veto
	disabled := NewRemoteUserNatMultiClientMonitorWithDefaults()
	merged = NewMergedMultiClientMonitor([]MultiClientMonitor{quality, speed, disabled})
	AssertEqual(t, merged.WindowExpandEvent().Failed, true)

	// min satisfied anywhere overrides failed
	speed.AddWindowExpandEvent(true, 1)
	AssertEqual(t, merged.WindowExpandEvent().Failed, false)
}

// --- the outcome state machine ---------------------------------------------

func TestWindowOutcomeAction(t *testing.T) {
	deadline := 45 * time.Second
	rebuildDeadline := 45 * time.Second

	// disabled, unarmed, added, or already failed: never acts
	AssertEqual(t, windowOutcomeAction(time.Hour, 0, rebuildDeadline, true, false, false, false), outcomeNone)
	AssertEqual(t, windowOutcomeAction(time.Hour, deadline, rebuildDeadline, false, false, false, false), outcomeNone)
	AssertEqual(t, windowOutcomeAction(time.Hour, deadline, rebuildDeadline, true, true, false, false), outcomeNone)
	AssertEqual(t, windowOutcomeAction(time.Hour, deadline, rebuildDeadline, true, false, true, true), outcomeNone)

	// before the deadline: nothing
	AssertEqual(t, windowOutcomeAction(deadline-time.Second, deadline, rebuildDeadline, true, false, false, false), outcomeNone)
	// at the deadline with the rebuild unspent: rebuild — exactly once
	AssertEqual(t, windowOutcomeAction(deadline, deadline, rebuildDeadline, true, false, false, false), outcomeRebuild)
	// rebuilt, second span not yet expired (elapsed measures from the rebuild's
	// arm reset): nothing
	AssertEqual(t, windowOutcomeAction(rebuildDeadline-time.Second, deadline, rebuildDeadline, true, false, true, false), outcomeNone)
	// rebuilt and the second span expired: fail
	AssertEqual(t, windowOutcomeAction(rebuildDeadline, deadline, rebuildDeadline, true, false, true, false), outcomeFail)
	// rebuilt with the failed latch disabled: never fails
	AssertEqual(t, windowOutcomeAction(time.Hour, deadline, 0, true, false, true, false), outcomeNone)
}

func TestOutcomeWatchPollTimeout(t *testing.T) {
	resizeTimeout := 15 * time.Second
	// disabled idles at the resize cadence
	AssertEqual(t, outcomeWatchPollTimeout(0, resizeTimeout), resizeTimeout)
	// the default deadline polls at the 1s cap
	AssertEqual(t, outcomeWatchPollTimeout(45*time.Second, resizeTimeout), time.Second)
	// a short (test) deadline polls at a fraction of itself, floored
	AssertEqual(t, outcomeWatchPollTimeout(800*time.Millisecond, resizeTimeout), 100*time.Millisecond)
	AssertEqual(t, outcomeWatchPollTimeout(4*time.Second, resizeTimeout), 500*time.Millisecond)
}

// the settings pair rides ReliabilitySettings like the other runtime knobs, so
// it appears in the session banner and can be toggled from the developer menu
func TestWindowOutcomeDeadlineSettings(t *testing.T) {
	settings := DefaultMultiClientSettings()
	AssertEqual(t, settings.WindowOutcomeDeadline, 45*time.Second)
	AssertEqual(t, settings.WindowOutcomeRebuildDeadline, 45*time.Second)

	reliabilitySettings := ReliabilitySettingsFrom(settings)
	AssertEqual(t, reliabilitySettings.WindowOutcomeDeadline, settings.WindowOutcomeDeadline)
	AssertEqual(t, reliabilitySettings.WindowOutcomeRebuildDeadline, settings.WindowOutcomeRebuildDeadline)

	// zero-value-off, like the rest of the runtime knobs
	AssertEqual(t, ReliabilitySettingsFrom(nil).WindowOutcomeDeadline, time.Duration(0))

	// the session banner renders both (the checkpoint test greps these names)
	pairs := strings.Join(relSettingsPairs(reliabilitySettings), " ")
	AssertEqual(t, strings.Contains(pairs, "windowoutcomedeadline=45000"), true)
	AssertEqual(t, strings.Contains(pairs, "windowoutcomerebuilddeadline=45000"), true)
}

// outcomeTestWindow is a window with just enough wiring to drive the outcome
// transitions directly: a monitor, a recorder, a log, and live notify monitors.
func outcomeTestWindow(ctx context.Context, log *recordingLogger) *multiClientWindow {
	window := &multiClientWindow{
		ctx:              ctx,
		log:              log,
		windowType:       WindowTypeQuality,
		settings:         DefaultMultiClientSettings(),
		monitor:          NewRemoteUserNatMultiClientMonitorWithDefaults(),
		clients:          map[Id]*multiClientChannel{},
		generatorMonitor: NewMonitor(),
		resizeMonitor:    NewMonitor(),
		failures:         &windowFailureRecorder{},
		createFailThrottle:    newLogThrottle(evaluationFailureLogInterval),
		pingFailThrottle:      newLogThrottle(evaluationFailureLogInterval),
		enumerateZeroThrottle: newLogThrottle(evaluationFailureLogInterval),
	}
	window.evalEpochCtx, window.evalEpochCancel = context.WithCancel(ctx)
	return window
}

// the rebuild: spends the one automatic rescue, cancels the evaluation epoch
// so in-flight candidates die fast, resets the deadline clock, and logs the
// [rel] line the checkpoint test greps
func TestWindowOutcomeRebuild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := newRecordingLogger()
	window := outcomeTestWindow(ctx, log)

	window.armOutcome()
	oldEpoch := window.evalEpochContext()

	window.recordEvaluationFailure(windowFailurePlatform, errors.New("generator call abandoned after 20s"))
	window.rebuildWindow(45 * time.Second)

	// the epoch was cancelled and replaced
	select {
	case <-oldEpoch.Done():
	default:
		t.Fatal("the rebuild did not cancel the evaluation epoch")
	}
	newEpoch := window.evalEpochContext()
	select {
	case <-newEpoch.Done():
		t.Fatal("the fresh epoch is already cancelled")
	default:
	}

	// the state machine spent its one rebuild and reset the clock
	window.outcomeLock.Lock()
	rebuilt := window.outcomeRebuilt
	armTime := window.outcomeArmTime
	window.outcomeLock.Unlock()
	AssertEqual(t, rebuilt, true)
	AssertEqual(t, time.Since(armTime) < 10*time.Second, true)

	lines := log.linesWith("event=window_rebuild")
	if len(lines) != 1 {
		t.Fatalf("expected one window_rebuild line, got %v", lines)
	}
	AssertEqual(t, strings.Contains(lines[0], "reason=platform-unreachable"), true)
	AssertEqual(t, strings.Contains(lines[0], "window=quality"), true)
}

// the failed latch: published to the monitor with the reason, logged once, and
// cleared by a provider landing
func TestWindowOutcomeFailAndRecover(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := newRecordingLogger()
	window := outcomeTestWindow(ctx, log)

	window.armOutcome()
	window.recordEvaluationFailure(windowFailureProvider, errors.New("ping timeout"))
	window.failOutcome(45 * time.Second)

	windowExpandEvent := window.monitor.WindowExpandEvent()
	AssertEqual(t, windowExpandEvent.Failed, true)
	AssertEqual(t, windowExpandEvent.Reason, WindowStallProvidersUnresponsive)

	lines := log.linesWith("event=window_failed")
	if len(lines) != 1 {
		t.Fatalf("expected one window_failed line, got %v", lines)
	}
	AssertEqual(t, strings.Contains(lines[0], "reason=providers-unresponsive"), true)

	// a provider landing clears the latch and disarms the watchdog
	window.noteClientAdded(nil)
	AssertEqual(t, window.monitor.WindowExpandEvent().Failed, false)
	window.outcomeLock.Lock()
	added := window.everAdded
	window.outcomeLock.Unlock()
	AssertEqual(t, added, true)
	AssertEqual(t, len(log.linesWith("event=window_recovered")), 1)

	// ...and the state machine never acts again
	AssertEqual(t,
		windowOutcomeAction(time.Hour, 45*time.Second, 45*time.Second, true, true, true, false),
		outcomeNone)
}

// the stall transition is logged once per change through publishStallStatus
func TestWindowStallTransitionLogsOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := newRecordingLogger()
	window := outcomeTestWindow(ctx, log)

	window.recordEvaluationFailure(windowFailureProvider, nil)
	window.recordEvaluationFailure(windowFailureProvider, nil)
	window.recordEvaluationFailure(windowFailureProvider, nil)

	lines := log.linesWith("event=window_stall")
	if len(lines) != 1 {
		t.Fatalf("expected exactly one window_stall transition line, got %v", lines)
	}
	AssertEqual(t, strings.Contains(lines[0], "reason=providers-unresponsive"), true)
}

// the unconditional evaluation-failure lines carry the "(N suppressed)" tail
func TestSuppressedSuffix(t *testing.T) {
	AssertEqual(t, suppressedSuffix(0), "")
	AssertEqual(t, suppressedSuffix(-1), "")
	AssertEqual(t, suppressedSuffix(3), " (3 suppressed)")
}
