package connect

import (
	"sync"
	"testing"
	"time"
)

// The degradation state is package-level, because the flood it guards against
// is across every transport and sequence at once. These tests therefore reset
// it rather than constructing an instance, and must not run in parallel.
func resetBackendDegraded() {
	consecutiveBackendFails.Store(0)
	lastBackendFailNano.Store(0)
}

func TestBackendDegraded_CleanStateIsNotDegraded(t *testing.T) {
	resetBackendDegraded()
	defer resetBackendDegraded()

	if isBackendDegraded() {
		t.Fatal("a provider that has never seen a failure must not be degraded")
	}
}

func TestBackendDegraded_BelowThresholdIsNotDegraded(t *testing.T) {
	resetBackendDegraded()
	defer resetBackendDegraded()

	// One or two stray timeouts are normal churn on a busy provider.
	for i := 0; i < backendDegradedFailThreshold-1; i++ {
		noteBackendFailure()
		if isBackendDegraded() {
			t.Fatalf("degraded after %d failures; threshold is %d", i+1, backendDegradedFailThreshold)
		}
	}
}

func TestBackendDegraded_ThresholdReachedIsDegraded(t *testing.T) {
	resetBackendDegraded()
	defer resetBackendDegraded()

	for i := 0; i < backendDegradedFailThreshold; i++ {
		noteBackendFailure()
	}
	if !isBackendDegraded() {
		t.Fatalf("not degraded after %d consecutive recent failures", backendDegradedFailThreshold)
	}
}

// The counter alone is not enough: a stale count left by an old blip on an
// otherwise idle provider must not read as a live outage.
func TestBackendDegraded_StaleFailuresAreNotDegraded(t *testing.T) {
	resetBackendDegraded()
	defer resetBackendDegraded()

	for i := 0; i < backendDegradedFailThreshold; i++ {
		noteBackendFailure()
	}
	if !isBackendDegraded() {
		t.Fatal("precondition: should be degraded before aging the failure")
	}

	// Age the last failure past the recency window.
	lastBackendFailNano.Store(time.Now().Add(-backendDegradedWindow - time.Second).UnixNano())

	if isBackendDegraded() {
		t.Fatalf("still degraded with the last failure older than %s", backendDegradedWindow)
	}
}

// Recovery must be immediate on the first good round-trip, not on a timer.
func TestBackendDegraded_SuccessClearsImmediately(t *testing.T) {
	resetBackendDegraded()
	defer resetBackendDegraded()

	for i := 0; i < backendDegradedFailThreshold+5; i++ {
		noteBackendFailure()
	}
	if !isBackendDegraded() {
		t.Fatal("precondition: should be degraded")
	}

	noteBackendSuccess()

	if isBackendDegraded() {
		t.Fatal("a successful round-trip must clear the degraded state immediately")
	}
	if got := consecutiveBackendFails.Load(); got != 0 {
		t.Fatalf("consecutive failures = %d after success, want 0", got)
	}
}

// This is the false-positive guard that matters most: an intermittently failing
// path that still succeeds sometimes is NOT an outage, however many failures it
// accumulates in total.
func TestBackendDegraded_InterleavedSuccessNeverAccumulates(t *testing.T) {
	resetBackendDegraded()
	defer resetBackendDegraded()

	for i := 0; i < 50; i++ {
		noteBackendFailure()
		noteBackendFailure()
		noteBackendSuccess()
		if isBackendDegraded() {
			t.Fatalf("degraded on iteration %d despite an interleaved success", i)
		}
	}
}

// Regression: a streak that aged out must not be resumed by a later failure.
// Before the fix, three stale failures plus one fresh one totalled four with a
// current timestamp, so the backend read as degraded on the strength of a
// single recent failure.
func TestBackendDegraded_StaleStreakIsNotResumedByANewFailure(t *testing.T) {
	resetBackendDegraded()
	defer resetBackendDegraded()

	for i := 0; i < backendDegradedFailThreshold; i++ {
		noteBackendFailure()
	}
	// Age the whole streak out of the window, as an idle provider that simply
	// stopped retrying would.
	lastBackendFailNano.Store(time.Now().Add(-backendDegradedWindow - time.Minute).UnixNano())
	if isBackendDegraded() {
		t.Fatal("precondition: an aged-out streak must not read as degraded")
	}

	// One new failure. This is the first failure of a fresh streak, not the
	// fourth of the old one.
	noteBackendFailure()

	if got := consecutiveBackendFails.Load(); got != 1 {
		t.Fatalf("consecutive failures = %d after a stale streak plus one failure, want 1", got)
	}
	if isBackendDegraded() {
		t.Fatal("degraded after a single recent failure; the stale streak was resumed instead of reset")
	}
}

// The reset must not fire on a gap shorter than the window, or a slow but
// genuine outage would never accumulate.
func TestBackendDegraded_StreakSurvivesGapInsideWindow(t *testing.T) {
	resetBackendDegraded()
	defer resetBackendDegraded()

	noteBackendFailure()
	noteBackendFailure()
	// A gap well inside the window: still the same outage.
	lastBackendFailNano.Store(time.Now().Add(-backendDegradedWindow / 2).UnixNano())

	noteBackendFailure()

	if got := consecutiveBackendFails.Load(); got != backendDegradedFailThreshold {
		t.Fatalf("consecutive failures = %d, want %d (streak reset inside the window)", got, backendDegradedFailThreshold)
	}
	if !isBackendDegraded() {
		t.Fatal("not degraded after 3 failures spanning a gap inside the window")
	}
}

func TestBackendDegraded_ConcurrentFailuresReachThreshold(t *testing.T) {
	resetBackendDegraded()
	defer resetBackendDegraded()

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			noteBackendFailure()
		}()
	}
	wg.Wait()

	if got := consecutiveBackendFails.Load(); got != 32 {
		t.Fatalf("consecutive failures = %d after 32 concurrent failures, want 32", got)
	}
	if !isBackendDegraded() {
		t.Fatal("not degraded after 32 concurrent failures")
	}
}
