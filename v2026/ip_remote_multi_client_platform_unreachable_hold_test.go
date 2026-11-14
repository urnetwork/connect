package connect

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

// The platform-unreachable hold (publishStallStatus): every app offers its
// "Can't reach URnetwork" prompt when a window's published diagnosis turns to
// platform-unreachable (sdk CantReachOnStall). On 2026-11-13 the prompt came
// up again and again on a working iOS tunnel: a connect server restart drops
// a window's transports for the seconds a reconnect takes, the transport
// detector in stallReason read platform-unreachable at once, and one platform
// timeout kept the derived diagnosis there for the failure horizon after the
// platform had answered again.

// holdTestWindow is outcomeTestWindow with one installed client whose
// carrier the test switches with the returned functions. Its route manager is
// the only part of the client the diagnosis reads.
func holdTestWindow(ctx context.Context, log *recordingLogger) (window *multiClientWindow, up func(), down func()) {
	window = outcomeTestWindow(ctx, log)
	routeManager := NewRouteManager(ctx, "synthetic-hold")
	transport := NewSendGatewayTransport()
	route := make(chan []byte)
	up = func() {
		routeManager.UpdateTransport(transport, []Route{route})
	}
	down = func() {
		routeManager.RemoveTransport(transport)
	}
	up()
	window.clients[NewId()] = &multiClientChannel{
		ctx:    ctx,
		client: &Client{routeManager: routeManager},
	}
	return
}

// turnsToPlatformUnreachable counts the published turns to
// platform-unreachable: each published change logs one window_stall line.
func turnsToPlatformUnreachable(log *recordingLogger) int {
	return len(log.linesWith("event=window_stall window=quality reason=" + WindowStallPlatformUnreachable))
}

// A connect restart: the carrier drops and a reconnect brings it back inside
// the hold, twice, with an evaluation failure published during each drop. The
// published diagnosis never turns, so no app offers the prompt. Before the
// hold the first failure during the first drop published platform-unreachable.
func TestWindowTransportDropsInsideHoldNeverTurnPlatformUnreachable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		log := newRecordingLogger()
		window, up, down := holdTestWindow(ctx, log)
		AssertEqual(t, window.settings.WindowPlatformUnreachableHold, 30*time.Second)

		window.publishStallStatus()
		AssertEqual(t, window.monitor.WindowExpandEvent().Reason, WindowStallEvaluating)

		// the first drop: the detector reads platform-unreachable at once
		down()
		window.recordEvaluationFailure(windowFailureProvider, nil)
		AssertEqual(t, window.stallReason(), WindowStallPlatformUnreachable)
		AssertEqual(t, window.monitor.WindowExpandEvent().Reason, WindowStallEvaluating)

		time.Sleep(10 * time.Second)
		synctest.Wait()
		up()
		time.Sleep(5 * time.Second)
		synctest.Wait()

		// the second drop starts its own hold: it does not inherit the first
		// drop's start, which is now past the hold
		down()
		window.recordEvaluationFailure(windowFailureProvider, nil)
		time.Sleep(20 * time.Second)
		synctest.Wait()
		AssertEqual(t, window.monitor.WindowExpandEvent().Reason, WindowStallProvidersUnresponsive)
		up()
		time.Sleep(40 * time.Second)
		synctest.Wait()

		if turns := turnsToPlatformUnreachable(log); turns != 0 {
			t.Fatalf("published turns to platform-unreachable = %d, want 0: %v", turns, log.linesWith("event=window_stall"))
		}
		AssertEqual(t, window.monitor.WindowExpandEvent().Reason == WindowStallPlatformUnreachable, false)
	})
}

// A carrier that stays down turns the published diagnosis once the hold has
// passed, with no further failure to publish it: the watch does. When the
// carrier comes back the published diagnosis leaves platform-unreachable.
func TestWindowSustainedTransportDownTurnsAfterHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		log := newRecordingLogger()
		window, up, down := holdTestWindow(ctx, log)

		down()
		window.publishStallStatus()
		time.Sleep(29 * time.Second)
		synctest.Wait()
		AssertEqual(t, turnsToPlatformUnreachable(log), 0)

		time.Sleep(2 * time.Second)
		synctest.Wait()
		AssertEqual(t, window.monitor.WindowExpandEvent().Reason, WindowStallPlatformUnreachable)
		AssertEqual(t, turnsToPlatformUnreachable(log), 1)

		up()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		AssertEqual(t, window.monitor.WindowExpandEvent().Reason, WindowStallEvaluating)
		AssertEqual(t, turnsToPlatformUnreachable(log), 1)

		// the episode is over and the watch has stopped
		window.outcomeLock.Lock()
		watching := window.platformUnreachableWatch
		window.outcomeLock.Unlock()
		AssertEqual(t, watching, false)
	})
}

// One platform timeout, then the platform answers a listing: the derived
// diagnosis leaves platform-unreachable at the answer. Before the fix the
// recorded timeout kept it there for the whole 60s failure horizon, which
// the hold alone would have published 30s in.
func TestWindowPlatformAnswerEndsPlatformDiagnosis(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		log := newRecordingLogger()
		window := outcomeTestWindow(ctx, log)

		window.recordEvaluationFailure(windowFailurePlatform, errors.New("synthetic find-providers timeout"))
		AssertEqual(t, window.stallReason(), WindowStallPlatformUnreachable)
		AssertEqual(t, window.monitor.WindowExpandEvent().Reason, WindowStallEvaluating)

		time.Sleep(5 * time.Second)
		synctest.Wait()
		window.windowListingRetryReset()
		AssertEqual(t, window.stallReason(), WindowStallEvaluating)

		time.Sleep(time.Minute)
		synctest.Wait()
		if turns := turnsToPlatformUnreachable(log); turns != 0 {
			t.Fatalf("published turns to platform-unreachable = %d, want 0: %v", turns, log.linesWith("event=window_stall"))
		}
	})
}

// A mint that succeeds and an installed client are answers too.
func TestWindowMintAndInstallEndPlatformDiagnosis(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := newRecordingLogger()
	window := outcomeTestWindow(ctx, log)

	window.recordEvaluationFailure(windowFailurePlatform, errors.New("synthetic client mint timeout"))
	AssertEqual(t, window.stallReason(), WindowStallPlatformUnreachable)
	window.windowRetryReset()
	AssertEqual(t, window.stallReason(), WindowStallEvaluating)

	window.recordEvaluationFailure(windowFailurePlatform, errors.New("synthetic client mint timeout"))
	AssertEqual(t, window.stallReason(), WindowStallPlatformUnreachable)
	window.noteClientAdded(nil)
	AssertEqual(t, window.stallReason(), WindowStallEvaluating)

	// the other classes are not the platform's to clear
	window.recordEvaluationFailure(windowFailureProvider, nil)
	window.windowListingRetryReset()
	AssertEqual(t, window.stallReason(), WindowStallProvidersUnresponsive)
}

// Platform failures that keep coming with no answer are the sustained case:
// the turn is published once the hold has passed.
func TestWindowRepeatedPlatformFailuresTurnAfterHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		log := newRecordingLogger()
		window := outcomeTestWindow(ctx, log)

		for range 6 {
			window.recordEvaluationFailure(windowFailurePlatform, errors.New("synthetic find-providers timeout"))
			AssertEqual(t, turnsToPlatformUnreachable(log), 0)
			time.Sleep(5 * time.Second)
			synctest.Wait()
		}
		time.Sleep(time.Second)
		synctest.Wait()
		AssertEqual(t, window.monitor.WindowExpandEvent().Reason, WindowStallPlatformUnreachable)
		AssertEqual(t, turnsToPlatformUnreachable(log), 1)
	})
}

// A non-positive hold publishes the turn at once, as before the hold.
func TestWindowPlatformUnreachableHoldDisabled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := newRecordingLogger()
	window := outcomeTestWindow(ctx, log)
	window.settings.WindowPlatformUnreachableHold = 0

	window.recordEvaluationFailure(windowFailurePlatform, errors.New("synthetic find-providers timeout"))
	AssertEqual(t, window.monitor.WindowExpandEvent().Reason, WindowStallPlatformUnreachable)
	AssertEqual(t, turnsToPlatformUnreachable(log), 1)
}
