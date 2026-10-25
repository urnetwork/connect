// A discovery window whose candidates keep failing expands on a jittered
// backoff, never on every resize pass, and an admission or a network change
// ends the backoff. Every identity and destination here is synthetic.
package connect

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// Lists fresh destinations whose providers never answer. Every mint is
// counted, and every setup fails the way an unreachable provider's evaluation
// does.
type unreachableDiscoveryGenerator struct {
	testingEmptyMultiClientGenerator

	stateLock   sync.Mutex
	mintCount   int
	retireCount int
}

// A discovery listing: `count` destinations the window has not seen.
func (self *unreachableDiscoveryGenerator) NextDestinations(count int, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
	destinations := map[MultiHopId]DestinationStats{}
	for range count {
		destinations[RequireMultiHopId(NewId())] = DestinationStats{}
	}
	return destinations, nil
}

// Each call stands for one derived-client registration.
func (self *unreachableDiscoveryGenerator) NewClientArgs() (*MultiClientGeneratorClientArgs, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.mintCount += 1
	return &MultiClientGeneratorClientArgs{ClientId: NewId()}, nil
}

// Each call stands for one derived-client retirement.
func (self *unreachableDiscoveryGenerator) RemoveClientArgs(args *MultiClientGeneratorClientArgs) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.retireCount += 1
}

// The provider never answers, so the candidate's setup fails.
func (self *unreachableDiscoveryGenerator) NewClient(ctx context.Context, args *MultiClientGeneratorClientArgs, clientSettings *ClientSettings) (*Client, error) {
	return nil, errors.New("synthetic provider unreachable")
}

func (self *unreachableDiscoveryGenerator) counts() (mintCount int, retireCount int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.mintCount, self.retireCount
}

// Runs the production enumerator and resize loops of one discovery window for
// `duration` of synctest time and returns the joined mint and retirement
// counts. The window holds one exit and evaluates one candidate per pass, so
// each pass mints exactly one identity. Must run inside a synctest bubble.
func runUnreachableDiscoveryWindow(t *testing.T, generator *unreachableDiscoveryGenerator, duration time.Duration) (mintCount int, retireCount int) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	window := outcomeTestWindow(ctx, newRecordingLogger())
	window.cancel = cancel
	window.generator = generator
	window.windowSizeLimit = 1
	window.settings.EvaluationPoolMultiple = 1
	window.clientChannelArgs = make(chan *multiClientChannelArgs)
	window.contractStatusCallbacks = NewCallbackList[*contractStatusCallbackWorker]()
	window.budgetFailThrottle = newLogThrottle(window.settings.EvaluationFailureLogInterval)
	window.backendDegradedForTest = func() bool { return false }

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

// The hosted-proxy churn of 2026-10-21: a discovery window whose candidates
// all failed started its next pass as soon as the last one ended, each pass
// minting and retiring fresh identities, for as long as the failures lasted
// (every 15s here). Consecutive passes that admit nothing must back off. Ten
// minutes allow at most 13 passes for any jitter (5-10s doubling to the 1-2
// minute cap), where the unpaced loop makes 40.
func TestDiscoveryWindowExpansionIsPacedAfterUnproductivePasses(t *testing.T) {
	// the process-wide message pool worker must not start inside the bubble
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		generator := &unreachableDiscoveryGenerator{}
		mintCount, retireCount := runUnreachableDiscoveryWindow(t, generator, 10*time.Minute)
		t.Logf("unreachable discovery window: %d mints, %d retirements in 10 minutes", mintCount, retireCount)
		if mintCount < 6 || 14 < mintCount {
			t.Fatalf("an unreachable discovery window minted %d identities in 10 minutes, want 6 to 14", mintCount)
		}
		if retireCount != mintCount {
			t.Fatalf("retired %d of %d minted identities", retireCount, mintCount)
		}
	})
}

// The window's own clock with the jitter pinned to the bottom of its range,
// so each deferral is exactly half the doubled delay.
func discoveryRetryTestWindow(t *testing.T) *multiClientWindow {
	t.Helper()
	window := outcomeTestWindow(t.Context(), newRecordingLogger())
	window.retryJitterForTest = func(half time.Duration) time.Duration {
		return 0
	}
	return window
}

// Each unproductive pass doubles the deferral from the minimum to the maximum.
// An admission clears it, so the next unproductive pass starts over.
func TestDiscoveryExpansionAdmissionClearsDeferral(t *testing.T) {
	window := discoveryRetryTestWindow(t)
	now := time.Now()
	retryMin := window.settings.DiscoveryExpandRetryMin
	retryMax := window.settings.DiscoveryExpandRetryMax

	if _, deferred := window.discoveryExpandDeferral(now); deferred {
		t.Fatal("a window that never failed is deferred")
	}
	for i, want := range []time.Duration{retryMin / 2, retryMin, 2 * retryMin, 4 * retryMin} {
		window.noteDiscoveryExpandUnproductive(now)
		notBefore, deferred := window.discoveryExpandDeferral(now)
		if !deferred || notBefore.Sub(now) != want {
			t.Fatalf("after %d unproductive passes: deferred=%v for %s, want %s", i+1, deferred, notBefore.Sub(now), want)
		}
	}
	for range 10 {
		window.noteDiscoveryExpandUnproductive(now)
	}
	if notBefore, _ := window.discoveryExpandDeferral(now); notBefore.Sub(now) != retryMax/2 {
		t.Fatalf("the deferral grew to %s, want the maximum's %s", notBefore.Sub(now), retryMax/2)
	}
	if _, deferred := window.discoveryExpandDeferral(now.Add(retryMax)); deferred {
		t.Fatal("the deferral did not end at its time")
	}

	window.noteDiscoveryExpandAdmitted()
	if _, deferred := window.discoveryExpandDeferral(now); deferred {
		t.Fatal("an admission left the window deferred")
	}
	window.noteDiscoveryExpandUnproductive(now)
	if notBefore, _ := window.discoveryExpandDeferral(now); notBefore.Sub(now) != retryMin/2 {
		t.Fatalf("after an admission the next deferral is %s, want the minimum's %s", notBefore.Sub(now), retryMin/2)
	}
}

// Failures on the previous network say nothing about the new one: the window's
// network change hook ends the deferral, and the next unproductive pass starts
// the backoff over.
func TestDiscoveryExpansionDeferralEndsOnNetworkChange(t *testing.T) {
	window := discoveryRetryTestWindow(t)
	now := time.Now()
	retryMin := window.settings.DiscoveryExpandRetryMin
	for range 4 {
		window.noteDiscoveryExpandUnproductive(now)
	}
	if _, deferred := window.discoveryExpandDeferral(now); !deferred {
		t.Fatal("four unproductive passes did not defer the window")
	}

	window.notifyNetworkChanged()
	if _, deferred := window.discoveryExpandDeferral(now); deferred {
		t.Fatal("a network change left the window deferred")
	}
	window.noteDiscoveryExpandUnproductive(now)
	if notBefore, _ := window.discoveryExpandDeferral(now); notBefore.Sub(now) != retryMin/2 {
		t.Fatalf("after a network change the next deferral is %s, want the minimum's %s", notBefore.Sub(now), retryMin/2)
	}
}

// A nonpositive minimum keeps the unpaced cadence.
func TestDiscoveryExpansionPacingDisabledByNonpositiveMinimum(t *testing.T) {
	window := discoveryRetryTestWindow(t)
	window.settings.DiscoveryExpandRetryMin = 0
	now := time.Now()
	window.noteDiscoveryExpandUnproductive(now)
	if _, deferred := window.discoveryExpandDeferral(now); deferred {
		t.Fatal("disabled pacing deferred the window")
	}
}

// The enumerator mints one candidate ahead of the pass that takes it. Retiring
// that candidate when it expired during a deferral made every deferred window
// mint a replacement once per WindowExpandArgsTimeout. The candidate is held
// until one resize interval after the deferral ends and taken by the next
// pass; a candidate nothing takes still expires.
func TestDiscoveryCandidateIsHeldAcrossDeferral(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		window := discoveryRetryTestWindow(t)
		window.clientChannelArgs = make(chan *multiClientChannelArgs)
		resizeTimeout := window.settings.WindowResizeTimeout
		start := time.Now()
		for range 4 {
			window.noteDiscoveryExpandUnproductive(start)
		}
		deferralEnd, deferred := window.discoveryExpandDeferral(start)
		if !deferred {
			t.Fatal("four unproductive passes did not defer the window")
		}

		offered := make(chan bool, 1)
		args := &multiClientChannelArgs{Destination: RequireMultiHopId(NewId())}
		go func() {
			// the candidate's own expiry falls inside the deferral
			offered <- window.offerClientArgs(args, start.Add(time.Second))
		}()
		time.Sleep(time.Until(deferralEnd.Add(resizeTimeout)) - time.Second)
		synctest.Wait()
		select {
		case result := <-offered:
			t.Fatalf("the candidate was released before the next pass could take it, offered=%v", result)
		default:
		}
		if received := <-window.clientChannelArgs; received != args {
			t.Fatal("the next pass did not take the held candidate")
		}
		if !<-offered {
			t.Fatal("a taken candidate reported no offer")
		}

		// nothing takes the next candidate: it expires once the hold ends
		window.noteDiscoveryExpandAdmitted()
		window.noteDiscoveryExpandUnproductive(time.Now())
		deferralEnd, _ = window.discoveryExpandDeferral(time.Now())
		go func() {
			offered <- window.offerClientArgs(args, time.Now().Add(time.Second))
		}()
		time.Sleep(time.Until(deferralEnd.Add(resizeTimeout)) + time.Second)
		synctest.Wait()
		select {
		case result := <-offered:
			if result {
				t.Fatal("an untaken candidate reported an offer")
			}
		default:
			t.Fatal("an untaken candidate was held past the end of its hold")
		}
	})
}

// A deferred discovery window's enumerator asks the platform for nothing until
// the deferral ends.
func TestDiscoveryEnumeratorListsNothingWhileDeferred(t *testing.T) {
	// the process-wide message pool worker must not start inside the bubble
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		generator := &listingCountDiscoveryGenerator{}
		window := outcomeTestWindow(ctx, newRecordingLogger())
		window.retryJitterForTest = func(half time.Duration) time.Duration {
			return 0
		}
		window.generator = generator
		window.clientChannelArgs = make(chan *multiClientChannelArgs)
		start := time.Now()
		for range 4 {
			window.noteDiscoveryExpandUnproductive(start)
		}
		deferralEnd, deferred := window.discoveryExpandDeferral(start)
		if !deferred {
			t.Fatal("four unproductive passes did not defer the window")
		}

		done := make(chan struct{})
		go func() {
			defer close(done)
			window.randomEnumerateClientArgs()
		}()
		time.Sleep(time.Until(deferralEnd) - time.Second)
		synctest.Wait()
		if listingCount := generator.listings(); listingCount != 0 {
			t.Fatalf("a deferred enumerator listed destinations %d times", listingCount)
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if listingCount := generator.listings(); listingCount != 1 {
			t.Fatalf("after the deferral the enumerator listed destinations %d times, want 1", listingCount)
		}
		cancel()
		<-done
	})
}

// A network change ends a deferred enumerator's wait at once, so a window on a
// new path lists and mints without waiting out a backoff grown on the old one.
func TestDiscoveryEnumeratorResumesAfterNetworkChange(t *testing.T) {
	// the process-wide message pool worker must not start inside the bubble
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		generator := &listingCountDiscoveryGenerator{}
		window := outcomeTestWindow(ctx, newRecordingLogger())
		window.generator = generator
		window.networkChangeMonitor = NewMonitor()
		window.clientChannelArgs = make(chan *multiClientChannelArgs)
		for range 4 {
			window.noteDiscoveryExpandUnproductive(time.Now())
		}
		if _, deferred := window.discoveryExpandDeferral(time.Now()); !deferred {
			t.Fatal("four unproductive passes did not defer the window")
		}

		done := make(chan struct{})
		go func() {
			defer close(done)
			window.randomEnumerateClientArgs()
		}()
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if listingCount := generator.listings(); listingCount != 0 {
			t.Fatalf("a deferred enumerator listed destinations %d times", listingCount)
		}
		window.notifyNetworkChanged()
		synctest.Wait()
		if listingCount := generator.listings(); listingCount != 1 {
			t.Fatalf("after a network change the enumerator listed destinations %d times, want 1", listingCount)
		}
		cancel()
		<-done
	})
}

// A window that has been failing to add a spare exit must not hold back the
// replacement of an exit it loses: the drop clears the deferral. The same or a
// larger exit count keeps it.
func TestDiscoveryExitLossClearsDeferral(t *testing.T) {
	window := discoveryRetryTestWindow(t)
	now := time.Now()
	window.noteDiscoveryClientCount(3)
	for range 3 {
		window.noteDiscoveryExpandUnproductive(now)
	}
	window.noteDiscoveryClientCount(3)
	if _, deferred := window.discoveryExpandDeferral(now); !deferred {
		t.Fatal("an unchanged exit count ended the deferral")
	}
	window.noteDiscoveryClientCount(4)
	if _, deferred := window.discoveryExpandDeferral(now); !deferred {
		t.Fatal("a larger exit count ended the deferral")
	}
	window.noteDiscoveryClientCount(3)
	if _, deferred := window.discoveryExpandDeferral(now); deferred {
		t.Fatal("losing an exit left the window deferred")
	}
	window.noteDiscoveryExpandUnproductive(now)
	if notBefore, _ := window.discoveryExpandDeferral(now); notBefore.Sub(now) != window.settings.DiscoveryExpandRetryMin/2 {
		t.Fatalf("after losing an exit the next deferral is %s, want the minimum's %s", notBefore.Sub(now), window.settings.DiscoveryExpandRetryMin/2)
	}
}

// Counts destination listings; the listing is empty, so nothing is minted.
type listingCountDiscoveryGenerator struct {
	testingEmptyMultiClientGenerator

	stateLock    sync.Mutex
	listingCount int
}

func (self *listingCountDiscoveryGenerator) NextDestinations(count int, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.listingCount += 1
	return map[MultiHopId]DestinationStats{}, nil
}

func (self *listingCountDiscoveryGenerator) listings() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.listingCount
}
