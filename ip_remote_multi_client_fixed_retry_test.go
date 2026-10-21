// A fixed destination that cannot be reached is re-dialed on a jittered
// backoff, never on every resize pass, and a dependency failure is never
// retried hot. Every identity and destination here is synthetic.
package connect

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// One fixed destination that never becomes reachable. Every mint is counted,
// and every setup fails the way an unreachable provider's evaluation does.
type unreachableFixedGenerator struct {
	testingEmptyMultiClientGenerator
	destination MultiHopId
	// Runs inside each failed setup, before the failure is returned.
	beforeSetupFailure func()
	// The window is canceled at this many mints, so a loop that never paces
	// itself still ends.
	mintLimit int
	cancel    context.CancelFunc

	stateLock   sync.Mutex
	mintCount   int
	retireCount int
}

// One explicit client id spec.
func (self *unreachableFixedGenerator) FixedDestinationSize() (int, bool) {
	return 1, true
}

// The fixed spec is its own destination, as in ApiMultiClientGenerator.
func (self *unreachableFixedGenerator) NextDestinations(count int, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
	if slices.Contains(excludeDestinations, self.destination) {
		return map[MultiHopId]DestinationStats{}, nil
	}
	return map[MultiHopId]DestinationStats{self.destination: {}}, nil
}

// Each call stands for one derived-client registration.
func (self *unreachableFixedGenerator) NewClientArgsForDestinationContext(ctx context.Context, destination MultiHopId) (*MultiClientGeneratorClientArgs, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.mintCount += 1
	if 0 < self.mintLimit && self.mintLimit <= self.mintCount {
		self.cancel()
	}
	return &MultiClientGeneratorClientArgs{ClientId: NewId()}, nil
}

// Each call stands for one derived-client retirement.
func (self *unreachableFixedGenerator) RemoveClientArgs(args *MultiClientGeneratorClientArgs) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.retireCount += 1
}

// The provider never answers, so the candidate's setup fails.
func (self *unreachableFixedGenerator) NewClient(ctx context.Context, args *MultiClientGeneratorClientArgs, clientSettings *ClientSettings) (*Client, error) {
	if self.beforeSetupFailure != nil {
		self.beforeSetupFailure()
	}
	return nil, errors.New("synthetic provider unreachable")
}

func (self *unreachableFixedGenerator) counts() (mintCount int, retireCount int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.mintCount, self.retireCount
}

// Runs the production enumerator and resize loops of one fixed window for
// `duration` of synctest time, or until the mint limit cancels it, and returns
// the joined mint and retirement counts. Must run inside a synctest bubble.
func runUnreachableFixedWindow(t *testing.T, generator *unreachableFixedGenerator, refuse bool, duration time.Duration) (mintCount int, retireCount int) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	window := outcomeTestWindow(ctx, newRecordingLogger())
	window.cancel = cancel
	window.generator = generator
	window.clientChannelArgs = make(chan *multiClientChannelArgs)
	window.contractStatusCallbacks = NewCallbackList[*contractStatusCallbackWorker]()
	window.budgetFailThrottle = newLogThrottle(DefaultMultiClientSettings().EvaluationFailureLogInterval)
	window.backendDegradedForTest = func() bool { return false }
	generator.cancel = cancel
	if refuse {
		// The platform answers the candidate's contract with Reliability:
		// the pinned provider's client is inactive. The status reaches the
		// window through the candidate's contract manager callback.
		generator.beforeSetupFailure = func() {
			reliability := protocol.ContractError_Reliability
			candidate := &multiClientChannel{
				args:         &multiClientChannelArgs{Destination: generator.destination},
				settings:     window.settings,
				eventBuckets: []*multiClientEventBucket{},
				packetStats:  &clientWindowStats{},
			}
			window.contractStatusFromClient(candidate, &ContractStatus{
				Key:   ContractKey{Destination: DestinationId(generator.destination.Tail())},
				Error: &reliability,
			})
		}
	}

	var loops sync.WaitGroup
	loops.Add(2)
	go func() {
		defer loops.Done()
		window.randomEnumerateClientArgs()
	}()
	go func() {
		defer loops.Done()
		window.resize()
	}()
	time.Sleep(duration)
	cancel()
	loops.Wait()
	return generator.counts()
}

// The root cause of the hosted-proxy registration churn: a fixed destination
// whose provider cannot be reached was re-dialed on every resize pass, each
// attempt minting and retiring a derived identity, for as long as the window
// lived (every 15s here; on Main, devices pinned to an offline provider looped
// about once a minute). Consecutive failures must back off. Ten minutes allow
// at most 8 attempts for any jitter (7.5s doubling to the 2.5-5 minute cap),
// where the unpaced loop makes 40.
func TestFixedDestinationRedialIsPacedAfterFailedEvaluation(t *testing.T) {
	// the process-wide message pool worker must not start inside the bubble
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		generator := &unreachableFixedGenerator{
			destination: RequireMultiHopId(NewId()),
			mintLimit:   1000,
		}
		mintCount, retireCount := runUnreachableFixedWindow(t, generator, false, 10*time.Minute)
		t.Logf("unreachable fixed destination: %d mints, %d retirements in 10 minutes", mintCount, retireCount)
		if mintCount < 4 || 10 < mintCount {
			t.Fatalf("an unreachable fixed destination was minted %d times in 10 minutes, want 4 to 10", mintCount)
		}
		if retireCount != mintCount {
			t.Fatalf("retired %d of %d minted identities", retireCount, mintCount)
		}
	})
}

// A Reliability verdict (the pinned provider's client is inactive and is never
// reactivated) woke resize to replace the candidate at once, and a fixed window
// replaced it with the same destination: one mint and retirement per round
// trip, about every 9s on Main and without any bound in synctest time. The
// verdict must defer the re-dial by the 2.5-5 minute maximum, which allows at
// most 4 attempts in 590s for any jitter.
func TestFixedDestinationReliabilityDefersRedial(t *testing.T) {
	// the process-wide message pool worker must not start inside the bubble
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		generator := &unreachableFixedGenerator{
			destination: RequireMultiHopId(NewId()),
			mintLimit:   50,
		}
		mintCount, retireCount := runUnreachableFixedWindow(t, generator, true, 590*time.Second)
		t.Logf("refused fixed destination: %d mints, %d retirements in 590s", mintCount, retireCount)
		if mintCount < 2 || 4 < mintCount {
			t.Fatalf("a refused fixed destination was minted %d times in 590s, want 2 to 4", mintCount)
		}
		if retireCount != mintCount {
			t.Fatalf("retired %d of %d minted identities", retireCount, mintCount)
		}
	})
}

// A mint that fails on a dependency (a pool acquisition deadline, a 503) was
// retried every second for the whole acquisition phase, by every window on
// the host at once. It must double from the 1s floor with jitter: a 15s
// acquisition phase allows at most 6 attempts, where the flat cadence makes
// 15.
func TestFixedCandidateMintDependencyFailureBacksOff(t *testing.T) {
	// the process-wide message pool worker must not start inside the bubble
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		attemptCount := 0
		destination := RequireMultiHopId(NewId())
		generator := &fixedDemandGenerator{
			mint: func(context.Context, MultiHopId) (*MultiClientGeneratorClientArgs, error) {
				attemptCount += 1
				return nil, fmt.Errorf("synthetic pool acquisition: %w", context.DeadlineExceeded)
			},
			remove: func(*MultiClientGeneratorClientArgs) { t.Error("a failed mint retired an identity") },
		}
		window := outcomeEnumeratorTestWindow(t.Context(), newRecordingLogger(), generator)
		window.settings.WindowEnumerateErrorTimeout = time.Second
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		args, err := window.fixedClientArgs(ctx, destination)
		if args != nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("failing mint result=%v error=%v", args != nil, err)
		}
		t.Logf("failing dependency: %d mint attempts in 15s", attemptCount)
		if attemptCount < 4 || 6 < attemptCount {
			t.Fatalf("a failing dependency was asked %d times in 15s, want 4 to 6", attemptCount)
		}
	})
}

// The unjittered schedule doubles per consecutive failure and stops at the
// maximum; nonpositive bounds disable pacing.
func TestFixedDestinationRetryDelayDoublesToTheMaximum(t *testing.T) {
	for _, c := range []struct {
		failureCount int
		min          time.Duration
		max          time.Duration
		want         time.Duration
	}{
		{failureCount: 0, min: 15 * time.Second, max: 5 * time.Minute, want: 0},
		{failureCount: 1, min: 15 * time.Second, max: 5 * time.Minute, want: 15 * time.Second},
		{failureCount: 2, min: 15 * time.Second, max: 5 * time.Minute, want: 30 * time.Second},
		{failureCount: 5, min: 15 * time.Second, max: 5 * time.Minute, want: 4 * time.Minute},
		{failureCount: 6, min: 15 * time.Second, max: 5 * time.Minute, want: 5 * time.Minute},
		{failureCount: 1000, min: 15 * time.Second, max: 5 * time.Minute, want: 5 * time.Minute},
		{failureCount: 3, min: 15 * time.Second, max: 0, want: 15 * time.Second},
		{failureCount: 3, min: 0, max: 5 * time.Minute, want: 0},
	} {
		if got := fixedDestinationRetryDelay(c.failureCount, c.min, c.max); got != c.want {
			t.Errorf("delay(%d, %s, %s) = %s, want %s", c.failureCount, c.min, c.max, got, c.want)
		}
	}
}

// A fixed-retry test window with deterministic jitter: the lower bound, or
// the upper bound less a nanosecond.
func fixedRetryTestWindow(t *testing.T, upper bool) *multiClientWindow {
	t.Helper()
	window := outcomeTestWindow(t.Context(), newRecordingLogger())
	window.retryJitterForTest = func(half time.Duration) time.Duration {
		if upper {
			return half - 1
		}
		return 0
	}
	return window
}

// Each consecutive failure defers the destination by the jittered schedule
// from the moment it failed, and admission clears it at once.
func TestFixedDestinationAdmissionClearsRedialDeferral(t *testing.T) {
	window := fixedRetryTestWindow(t, false)
	destination := RequireMultiHopId(NewId())
	now := time.Unix(1000, 0)
	if window.fixedDestinationDeferred(destination, now) {
		t.Fatal("a first attempt was deferred")
	}
	window.noteFixedDestinationFailed(destination, false, now)
	if !window.fixedDestinationDeferred(destination, now.Add(7500*time.Millisecond-1)) ||
		window.fixedDestinationDeferred(destination, now.Add(7500*time.Millisecond)) {
		t.Fatal("first failure must defer exactly the jittered 7.5s")
	}
	later := now.Add(10 * time.Second)
	window.noteFixedDestinationFailed(destination, false, later)
	if !window.fixedDestinationDeferred(destination, later.Add(15*time.Second-1)) ||
		window.fixedDestinationDeferred(destination, later.Add(15*time.Second)) {
		t.Fatal("second failure must double the deferral")
	}
	window.noteFixedDestinationAdmitted(destination)
	if window.fixedDestinationDeferred(destination, later) {
		t.Fatal("admission left the destination deferred")
	}
	window.noteFixedDestinationFailed(destination, false, later)
	if window.fixedDestinationDeferred(destination, later.Add(7500*time.Millisecond)) {
		t.Fatal("a failure after admission did not restart the schedule")
	}
}

// A refusal defers by the maximum at once, and a plain failure recorded
// after it (the pass that owned the refused candidate ending) cannot shorten
// that deferral.
func TestFixedDestinationLateFailureKeepsRefusalDeferral(t *testing.T) {
	window := fixedRetryTestWindow(t, true)
	destination := RequireMultiHopId(NewId())
	now := time.Unix(1000, 0)
	window.noteFixedDestinationFailed(destination, true, now)
	refusedUntil := now.Add(5*time.Minute - 1)
	if !window.fixedDestinationDeferred(destination, refusedUntil.Add(-1)) ||
		window.fixedDestinationDeferred(destination, refusedUntil) {
		t.Fatal("a refusal must defer by the jittered maximum")
	}
	window.noteFixedDestinationFailed(destination, false, now.Add(time.Second))
	if !window.fixedDestinationDeferred(destination, refusedUntil.Add(-1)) {
		t.Fatal("a later plain failure shortened the refusal deferral")
	}
}

// A window missing two fixed destinations still expands while only one of
// them is deferred; it waits only when every missing destination is.
func TestFixedRetryDeferralCoversOnlyMissingDestinations(t *testing.T) {
	window := fixedRetryTestWindow(t, false)
	now := time.Unix(1000, 0)
	first, second := RequireMultiHopId(NewId()), RequireMultiHopId(NewId())
	window.noteFixedDestinationFailed(first, false, now)
	if _, deferred := window.fixedRetryDeferral(2, now); deferred {
		t.Fatal("one deferred destination stopped the other's expansion")
	}
	if until, deferred := window.fixedRetryDeferral(1, now); !deferred || until != now.Add(7500*time.Millisecond) {
		t.Fatalf("one missing deferred destination: until=%s deferred=%t", until, deferred)
	}
	window.noteFixedDestinationFailed(second, true, now.Add(time.Second))
	if until, deferred := window.fixedRetryDeferral(2, now.Add(time.Second)); !deferred || until != now.Add(7500*time.Millisecond) {
		t.Fatalf("two deferred destinations must wake at the first: until=%s deferred=%t", until, deferred)
	}
	if _, deferred := window.fixedRetryDeferral(2, now.Add(7500*time.Millisecond)); deferred {
		t.Fatal("an expired deferral still blocked expansion")
	}
	if _, deferred := window.fixedRetryDeferral(0, now); deferred {
		t.Fatal("a full window was reported deferred")
	}
}

// A stale offer of a deferred destination neither mints nor consumes the
// demanded slot; the missing sibling is minted in the same pass.
func TestFixedCandidateDeferredOfferKeepsSiblingDemand(t *testing.T) {
	ctx := t.Context()
	deferredDestination, missingDestination := RequireMultiHopId(NewId()), RequireMultiHopId(NewId())
	calls, retired := 0, 0
	generator := &fixedDemandGenerator{
		mint: func(_ context.Context, got MultiHopId) (*MultiClientGeneratorClientArgs, error) {
			calls++
			if got != missingDestination {
				t.Error("a deferred destination was minted")
			}
			return &MultiClientGeneratorClientArgs{ClientId: NewId()}, nil
		},
		remove: func(*MultiClientGeneratorClientArgs) { retired++ },
	}
	window := outcomeEnumeratorTestWindow(ctx, newRecordingLogger(), generator)
	window.cancel = func() {}
	window.retryJitterForTest = func(time.Duration) time.Duration { return 0 }
	window.noteFixedDestinationFailed(deferredDestination, true, time.Now())
	window.clientChannelArgs = make(chan *multiClientChannelArgs, 2)
	for _, destination := range []MultiHopId{deferredDestination, missingDestination} {
		window.clientChannelArgs <- &multiClientChannelArgs{Destination: destination, FixedDestination: true, deferredClientArgs: true}
	}
	window.settings.EvaluationPoolMultiple = 1
	added := window.expand(WindowSizeSettings{WindowSizeMin: 2, WindowSizeMax: 2, WindowSizeHardMax: 2}, 0, 0, 2, 2, 1, 0)
	if added != 0 || calls != 1 || retired != 1 {
		t.Fatalf("deferred offer consumed demand: added=%d calls=%d retired=%d", added, calls, retired)
	}
	// the sibling's own failed setup now defers it too
	if !window.fixedDestinationDeferred(missingDestination, time.Now()) {
		t.Fatal("the attempted sibling's failure was not recorded")
	}
}

// Ends every re-dial deferral, as the passage of its backoff would, for
// fixtures that run on the wall clock.
func expireFixedDestinationRetries(window *multiClientWindow) {
	window.stateLock.Lock()
	defer window.stateLock.Unlock()
	for _, retry := range window.fixedRetries {
		retry.notBefore = time.Time{}
	}
}
