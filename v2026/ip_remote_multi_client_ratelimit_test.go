package connect

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The storm this guards against, measured on a real client 2026-08-17: 3,726
// platform 429s over 39 minutes, climbing 327 -> 672 per minute against a
// 1000/min budget, because every expander retried on a flat 1s cadence and
// nothing consulted the rate-limit class that classifyWindowFailure had
// already computed.

func newRateLimitTestWindow() *multiClientWindow {
	// windowRetryDelay/windowRetryReset touch only settings and the streak;
	// recordEvaluationFailure additionally needs the recorder and the monitor
	// that publishStallStatus reports through.
	return &multiClientWindow{
		settings: DefaultMultiClientSettings(),
		monitor:  NewRemoteUserNatMultiClientMonitorWithDefaults(),
		clients:  map[Id]*multiClientChannel{},
		failures: &windowFailureRecorder{},
	}
}

// The same herd formed on dependency failures: when the hosted control
// database slowed down, every device's window re-minted every second and kept
// the connection pool exhausted. Every other class now doubles from the 1s
// floor to WindowErrorBackoffMax with downward jitter, on its own streak.
func TestWindowRetryDelayDependencyFailureBacksOff(t *testing.T) {
	for _, class := range []windowFailureClass{windowFailurePlatform, windowFailureProvider, windowFailureAuth} {
		w := newRateLimitTestWindow()
		w.retryJitterForTest = func(time.Duration) time.Duration { return 0 }
		floor := w.settings.WindowEnumerateErrorTimeout
		// 1s, 2s, 4s, 8s, then the 15s cap, at the jitter floor
		want := []time.Duration{floor / 2, floor, 2 * floor, 4 * floor, w.settings.WindowErrorBackoffMax / 2, w.settings.WindowErrorBackoffMax / 2}
		for i, delay := range want {
			if d := w.windowRetryDelay(class); d != delay {
				t.Fatalf("class %v failure %d: delay %s, want %s", class, i+1, d, delay)
			}
		}
		// the rate-limit streak is untouched by dependency failures
		if d := w.windowRetryDelay(windowFailureRateLimit); w.settings.WindowRateLimitBackoffMin <= d {
			t.Fatalf("class %v advanced the rate-limit streak: %s", class, d)
		}
		w.windowRetryReset()
		if d := w.windowRetryDelay(class); d != floor/2 {
			t.Fatalf("class %v after reset: %s, want %s", class, d, floor/2)
		}
	}
}

// A settings literal without WindowErrorBackoffMax keeps the flat cadence.
func TestWindowRetryDelayLegacySettingsKeepFlatCadence(t *testing.T) {
	w := newRateLimitTestWindow()
	w.settings.WindowErrorBackoffMax = 0
	for i := 0; i < 4; i++ {
		if d := w.windowRetryDelay(windowFailurePlatform); d != w.settings.WindowEnumerateErrorTimeout {
			t.Fatalf("legacy failure %d: delay %s, want the flat %s", i+1, d, w.settings.WindowEnumerateErrorTimeout)
		}
	}
}

func TestWindowRetryDelayRateLimitBacksOffAndIsBounded(t *testing.T) {
	w := newRateLimitTestWindow()
	min := w.settings.WindowRateLimitBackoffMin
	max := w.settings.WindowRateLimitBackoffMax

	first := w.windowRetryDelay(windowFailureRateLimit)
	if first < min/2 || first >= min {
		t.Fatalf("first backoff must be jittered within [min/2, min), got %s (min %s)", first, min)
	}
	// every delay stays under the ceiling no matter how long the outage runs
	for i := 0; i < 200; i++ {
		d := w.windowRetryDelay(windowFailureRateLimit)
		if d >= max {
			t.Fatalf("backoff exceeded the ceiling at iteration %d: %s >= %s", i, d, max)
		}
		if d <= 0 {
			t.Fatalf("non-positive delay at iteration %d: %s", i, d)
		}
	}
	// and it did grow rather than sitting at the floor
	if late := w.windowRetryDelay(windowFailureRateLimit); late <= min {
		t.Fatalf("backoff never grew: %s <= min %s", late, min)
	}
}

func TestWindowRetryResetReturnsToTheFloor(t *testing.T) {
	w := newRateLimitTestWindow()
	for i := 0; i < 8; i++ {
		w.windowRetryDelay(windowFailureRateLimit)
	}
	w.windowRetryReset()
	d := w.windowRetryDelay(windowFailureRateLimit)
	if d >= w.settings.WindowRateLimitBackoffMin {
		t.Fatalf("after reset the backoff must start from the floor again, got %s", d)
	}
}

// Jitter is not decoration: dozens of expanders fail in the same instant, and
// without it they would retry in lockstep however long the delay.
func TestWindowRetryDelayIsJittered(t *testing.T) {
	w := newRateLimitTestWindow()
	seen := map[time.Duration]bool{}
	for i := 0; i < 32; i++ {
		w.windowRetryReset()
		seen[w.windowRetryDelay(windowFailureRateLimit)] = true
	}
	if len(seen) < 8 {
		t.Fatalf("expected jittered delays, got only %d distinct values in 32 draws", len(seen))
	}
}

// The verdict must actually reach the caller — it used to be computed and
// discarded, which is the whole reason the storm was possible.
func TestRecordEvaluationFailureReturnsTheRateLimitClass(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := outcomeTestWindow(ctx, newRecordingLogger())
	class := w.recordEvaluationFailure(windowFailurePlatform, errors.New("429 Too Many Requests: <html>"))
	if class != windowFailureRateLimit {
		t.Fatalf("a 429 must classify as a rate limit, got %v", class)
	}
}
