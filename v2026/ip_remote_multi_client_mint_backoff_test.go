// A discovery window whose mints the platform refuses for asking too often
// keeps backing off across its listings. Every identity and destination here
// is synthetic.
package connect

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// Lists fresh destinations on every call and refuses every mint the way the
// hosted proxy's mint admission does, recording when each mint was asked.
type refusedMintDiscoveryGenerator struct {
	testingEmptyMultiClientGenerator

	stateLock    sync.Mutex
	mintTimes    []time.Time
	listingCount int
}

func (self *refusedMintDiscoveryGenerator) NextDestinations(count int, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.listingCount += 1
	destinations := map[MultiHopId]DestinationStats{}
	for range count {
		destinations[RequireMultiHopId(NewId())] = DestinationStats{}
	}
	return destinations, nil
}

func (self *refusedMintDiscoveryGenerator) NewClientArgs() (*MultiClientGeneratorClientArgs, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.mintTimes = append(self.mintTimes, time.Now())
	return nil, &HttpStatusError{
		StatusCode: http.StatusTooManyRequests,
		Status:     "429 Too Many Requests",
		Body:       []byte("hosted client registration is at its concurrent bound"),
	}
}

func (self *refusedMintDiscoveryGenerator) snapshot() (mintTimes []time.Time, listingCount int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]time.Time(nil), self.mintTimes...), self.listingCount
}

// The hosted-proxy churn of 2026-10-22: mint admission refused about 464
// registrations a second, yet windows kept asking. Each refusal backed off,
// but the next listing the platform answered cleared the rate-limit streak
// along with the error streak, so every expansion round started the mint
// backoff over from WindowRateLimitBackoffMin. Only a mint that succeeds may
// end it: across listings, the wait after the k-th refusal in a row is at
// least that refusal's backoff. A wait that spans a listing also carries the
// expansion pacing, so it can only be longer.
func TestRefusedMintBackoffSurvivesSuccessfulListing(t *testing.T) {
	// the process-wide message pool worker must not start inside the bubble
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		generator := &refusedMintDiscoveryGenerator{}
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
		// each wait is exactly half its doubled delay
		window.retryJitterForTest = func(time.Duration) time.Duration { return 0 }

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
		time.Sleep(10 * time.Minute)
		cancel()
		loops.Wait()

		mintTimes, listingCount := generator.snapshot()
		t.Logf("%d refused mints over %d listings in 10 minutes", len(mintTimes), listingCount)
		if listingCount < 2 {
			t.Fatalf("the window listed %d times, the test needs a second listing", listingCount)
		}
		backoff := window.settings.WindowRateLimitBackoffMin
		for i := 1; i < len(mintTimes); i += 1 {
			// the jitter floor of the doubled delay after refusal i
			if gap := mintTimes[i].Sub(mintTimes[i-1]); gap < backoff/2 {
				t.Fatalf("refused mint %d came %s after refusal %d, under its %s backoff: a listing reset the rate-limit backoff", i+1, gap, i, backoff/2)
			}
			backoff = min(2*backoff, window.settings.WindowRateLimitBackoffMax)
		}
		// 2.5s doubling to the 60s floor of the 2m cap fills 10 minutes with
		// about 13 refusals; a backoff restarted per listing asks twice as often
		if 16 < len(mintTimes) {
			t.Fatalf("a refused window asked %d times in 10 minutes, want at most 16", len(mintTimes))
		}
	})
}
