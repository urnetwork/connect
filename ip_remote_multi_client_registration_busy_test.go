// A window with no usable exit retries a busy registration bound's refusal on a
// short bounded cadence, and an expansion pass that waited only on refused
// mints does not defer the next one. Every identity and destination here is
// synthetic.
package connect

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// The refusal the hosted mint admission answers when its bound is full, as
// server localclient builds it: a 429 that says its bound is busy.
type registrationBusyTestRefusal struct {
	*HttpStatusError
	busy bool
}

func (self *registrationBusyTestRefusal) ClientRegistrationBusy() bool {
	return self.busy
}

func (self *registrationBusyTestRefusal) Unwrap() error {
	return self.HttpStatusError
}

func registrationBusyRefusal() error {
	return &registrationBusyTestRefusal{
		HttpStatusError: &HttpStatusError{
			StatusCode: http.StatusTooManyRequests,
			Status:     "429 Too Many Requests",
			Body:       []byte("hosted client registration is at its concurrent bound"),
		},
		busy: true,
	}
}

// A busy bound's refusal is read through any wrapping, keeps its 429 status
// and text, and only a refusal that says so is one.
func TestClientRegistrationBusyIsReadFromTheRefusal(t *testing.T) {
	refusal := registrationBusyRefusal()
	if !isClientRegistrationBusy(refusal) || !isClientRegistrationBusy(fmt.Errorf("mint: %w", refusal)) {
		t.Fatal("a busy bound's refusal was not read as one")
	}
	var status *HttpStatusError
	if !errors.As(refusal, &status) || status.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("the refusal lost its status: %v", refusal)
	}
	if classifyWindowFailure(refusal, windowFailurePlatform) != windowFailureRateLimit {
		t.Fatalf("the refusal %q does not read as a rate limit", refusal)
	}
	notBusy := &registrationBusyTestRefusal{HttpStatusError: status, busy: false}
	if isClientRegistrationBusy(notBusy) || isClientRegistrationBusy(status) {
		t.Fatal("a refusal that does not say its bound is busy was read as one")
	}
}

// A hosted device's window shape (server proxy/proxy_device.go): quality 5 and
// speed 1, strict, one candidate evaluated per slot, ipv4/any selectors.
func hostedLikeWindowSettings(settings *MultiClientSettings) {
	settings.WindowSizes = map[WindowType]WindowSizeSettings{
		WindowTypeQuality: {WindowSizeMin: 5, WindowSizeMax: 5, WindowSizeHardMax: 5, WindowSizeReconnectScale: 1},
		WindowTypeSpeed:   {WindowSizeMin: 1, WindowSizeMax: 1, WindowSizeHardMax: 1, FixedWindowSize: 1, WindowSizeReconnectScale: 1},
	}
	settings.StandingReserve = false
	settings.StrictWindowSizeHardMax = true
	settings.StrictWindowHandover = true
	settings.LimitedWindowSizeFraction = 0
	settings.NormalWindowSelectors = ProviderSelectors{Ip: ProviderIpCapabilityIpv4, Protocol: ProviderProtocolCapabilityAny}
	settings.EvaluationPoolMultiple = 1
	settings.OutOfBalanceRefillProbe = true
	settings.ProviderProbe = true
}

// The test providers' generator, whose first `refuse` mints the busy bound
// refuses.
type busyRefusingGenerator struct {
	*reraceGenerator

	stateLock    sync.Mutex
	refuse       int
	refusedCount int
}

func (self *busyRefusingGenerator) NewClientArgsForDestination(destination MultiHopId) (*MultiClientGeneratorClientArgs, error) {
	refused := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.refusedCount < self.refuse {
			self.refusedCount += 1
			return true
		}
		return false
	}()
	if refused {
		return nil, registrationBusyRefusal()
	}
	return self.reraceGenerator.NewClientArgsForDestination(destination)
}

// The production multi client with a hosted window over one provider that
// answers at once: its first live exit waited on k refused mints. Before the
// short retry each refusal took the rate-limit backoff, 5 s doubling (2.5-5 s,
// then 5-10 s, then 10-20 s, and a pass that admitted nothing deferred the next
// for 5-10 s), and k = 1, 2, 3 cost 3.2-4.6, 9.4-12.5 and 24.3-30.9 s (server
// proxy/P90SPEED.md §3.3). Each refusal now costs one short retry of 250-500
// ms.
func TestWindowFirstExitAfterBusyRegistrationRefusals(t *testing.T) {
	// the process-wide message pool worker must not start inside the bubble
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	for _, refuse := range []int{1, 2, 3} {
		synctest.Test(t, func(t *testing.T) {
			var generator *busyRefusingGenerator
			start := time.Now()
			h := newReraceHarnessWithGenerator(t, []reraceExitSpec{{name: "a"}}, hostedLikeWindowSettings, func(g *reraceGenerator) MultiClientGenerator {
				generator = &busyRefusingGenerator{reraceGenerator: g, refuse: refuse}
				return generator
			})
			firstExit := h.start.Sub(start)
			generator.stateLock.Lock()
			refusedCount := generator.refusedCount
			generator.stateLock.Unlock()
			t.Logf("%d refused mints: first live exit after %s", refusedCount, firstExit)
			if refusedCount != refuse {
				t.Fatalf("the generator refused %d mints, want %d", refusedCount, refuse)
			}
			// each refusal at most one short retry, and the first exit's own
			// evaluation well inside the rest
			if limit := time.Duration(refuse+1) * 500 * time.Millisecond; limit < firstExit {
				t.Fatalf("after %d refused mints the first exit went live after %s, want at most %s", refuse, firstExit, limit)
			}
		})
	}
}

// A window's own clock with the jitter pinned to the bottom of its range.
func registrationBusyTestWindow(t *testing.T) *multiClientWindow {
	t.Helper()
	window := outcomeTestWindow(t.Context(), newRecordingLogger())
	window.retryJitterForTest = func(half time.Duration) time.Duration {
		return 0
	}
	return window
}

// Installs a live client in the window and returns its cancel, which ends it.
func addRegistrationBusyTestClient(t *testing.T, window *multiClientWindow) context.CancelFunc {
	t.Helper()
	clientCtx, clientCancel := context.WithCancel(t.Context())
	window.stateLock.Lock()
	window.clients[NewId()] = &multiClientChannel{ctx: clientCtx}
	window.stateLock.Unlock()
	return clientCancel
}

// Fake time from a failed mint to the window's next ask.
func registrationBusyRetryWait(t *testing.T, window *multiClientWindow, err error) time.Duration {
	t.Helper()
	class := window.recordEvaluationFailure(windowFailurePlatform, err)
	start := time.Now()
	if !window.waitMintRetry(t.Context(), class, err, nil) {
		t.Fatal("the retry wait ended with its context")
	}
	return time.Since(start)
}

// An empty window asks again after the short retry, the bottom of its jitter
// here; once it has an exit the same refusal takes the rate-limit backoff, and
// so does any other rate limit, such as the platform API's.
func TestWindowShortRetryIsOnlyForABusyBoundWhileEmpty(t *testing.T) {
	// the process-wide message pool worker must not start inside the bubble
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		window := registrationBusyTestWindow(t)
		retry := window.settings.WindowRegistrationBusyRetry
		backoff := window.settings.WindowRateLimitBackoffMin

		if wait := registrationBusyRetryWait(t, window, registrationBusyRefusal()); wait != retry/2 {
			t.Fatalf("an empty window asked again after %s, want the short retry %s", wait, retry/2)
		}
		// the platform's own rate limit, not a busy bound
		apiRateLimit := &HttpStatusError{StatusCode: http.StatusTooManyRequests, Status: "429 Too Many Requests"}
		if wait := registrationBusyRetryWait(t, window, apiRateLimit); wait != backoff/2 {
			t.Fatalf("an empty window asked the platform's rate limit again after %s, want the backoff %s", wait, backoff/2)
		}
		closeClient := addRegistrationBusyTestClient(t, window)
		defer closeClient()
		// the backoff doubled once for the platform's rate limit above
		if wait := registrationBusyRetryWait(t, window, registrationBusyRefusal()); wait != backoff {
			t.Fatalf("a window with an exit asked again after %s, want the backoff %s", wait, backoff)
		}
	})
}

// A window takes at most five short retries until it has a usable exit again,
// and at most five in any minute; past either it takes the backoff, which the
// short retries never advanced.
func TestWindowShortRetriesAreBoundedPerEmptySpellAndPerMinute(t *testing.T) {
	window := registrationBusyTestWindow(t)
	maxCount := window.settings.WindowRegistrationBusyRetryMaxCount
	period := window.settings.WindowRegistrationBusyRetryPeriod
	if maxCount != 5 || period != time.Minute {
		t.Fatalf("short retries %d per %s, want 5 per minute", maxCount, period)
	}
	start := time.Now()
	take := func(at time.Duration) bool {
		_, ok := window.takeRegistrationBusyRetry(start.Add(at))
		return ok
	}
	// the first empty spell: five short retries, one a second
	for i := range maxCount {
		if !take(time.Duration(i) * time.Second) {
			t.Fatalf("short retry %d refused", i+1)
		}
	}
	if take(50 * time.Second) {
		t.Fatal("a sixth short retry in one empty spell")
	}
	// an exit ends the spell
	closeClient := addRegistrationBusyTestClient(t, window)
	if take(51 * time.Second) {
		t.Fatal("a window with an exit took a short retry")
	}
	closeClient()
	// a new spell, but the minute still holds the first spell's five
	if take(52 * time.Second) {
		t.Fatal("a sixth short retry in one minute")
	}
	// each ages out a minute after it was taken
	if !take(time.Minute) {
		t.Fatal("no short retry once the first of the five aged out")
	}
	if take(time.Minute + 500*time.Millisecond) {
		t.Fatal("a sixth short retry in one minute, after one aged out")
	}
	if !take(time.Minute + time.Second) {
		t.Fatal("no short retry once the second of the five aged out")
	}
	// still empty much later: the spell's own bound holds after the minute
	// frees
	for i := range maxCount - 2 {
		if !take(10*time.Minute + time.Duration(i)*time.Second) {
			t.Fatalf("short retry %d of the second spell refused", i+3)
		}
	}
	if take(20 * time.Minute) {
		t.Fatal("a sixth short retry in the second spell")
	}
	if streak := window.rateLimitStreak.Load(); streak != 0 {
		t.Fatalf("short retries advanced the rate-limit streak to %d", streak)
	}
}

// A pass that built no candidate while the enumerator's last mint was refused
// for rate tried no provider; any other pass is evidence for the backoff.
func TestExpandPassThatOnlyWaitedOnRefusedMintsIsRecognized(t *testing.T) {
	window := outcomeTestWindow(t.Context(), newRecordingLogger())
	passCandidateCount := window.expandCandidateCount.Load()
	window.mintStarve.Store(mintStarveRefused)
	if !window.expandPassOnlyRefusedMints(passCandidateCount) {
		t.Fatal("a pass that waited only on refused mints counts against the providers")
	}
	window.mintStarve.Store(mintStarveOther)
	if window.expandPassOnlyRefusedMints(passCandidateCount) {
		t.Fatal("a pass that waited on a failed listing does not count")
	}
	window.mintStarve.Store(mintStarveNone)
	if window.expandPassOnlyRefusedMints(passCandidateCount) {
		t.Fatal("a pass with a candidate offered does not count")
	}
	window.mintStarve.Store(mintStarveRefused)
	window.expandCandidateCount.Add(1)
	if window.expandPassOnlyRefusedMints(passCandidateCount) {
		t.Fatal("a pass that tried a provider does not count")
	}
}

// Lists fresh destinations whose every mint the busy bound refuses. Each mint
// and listing is stamped with its fake time.
type alwaysBusyGenerator struct {
	testingEmptyMultiClientGenerator

	stateLock    sync.Mutex
	mintTimes    []time.Time
	listingTimes []time.Time
}

func (self *alwaysBusyGenerator) NextDestinations(count int, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
	self.stateLock.Lock()
	self.listingTimes = append(self.listingTimes, time.Now())
	self.stateLock.Unlock()
	destinations := map[MultiHopId]DestinationStats{}
	for range count {
		destinations[RequireMultiHopId(NewId())] = DestinationStats{}
	}
	return destinations, nil
}

func (self *alwaysBusyGenerator) NewClientArgs() (*MultiClientGeneratorClientArgs, error) {
	self.stateLock.Lock()
	self.mintTimes = append(self.mintTimes, time.Now())
	self.stateLock.Unlock()
	return nil, registrationBusyRefusal()
}

// The hot loop the short retry must never become: a window whose every mint a
// saturated bound refuses, for ten minutes of fake time, with the production
// enumerator and resize loops. It takes one spell of five short retries, then
// the rate-limit backoff from its start; its passes try no provider and never
// defer the next (before, each 15 s pass that admitted nothing started or
// doubled the expansion backoff); a pass waits out its whole acquisition, so
// passes stay one per 15 s; and it lists again only as each listing's mint
// retries expire.
func TestWindowWhoseMintsAreAllRefusedDoesNotHotLoop(t *testing.T) {
	// the process-wide message pool worker must not start inside the bubble
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		generator := &alwaysBusyGenerator{}
		log := newRecordingLogger()
		ctx, cancel := context.WithCancel(t.Context())
		window := outcomeTestWindow(ctx, log)
		window.cancel = cancel
		window.generator = generator
		window.windowSizeLimit = 1
		window.settings.EvaluationPoolMultiple = 1
		window.clientChannelArgs = make(chan *multiClientChannelArgs)
		window.contractStatusCallbacks = NewCallbackList[*contractStatusCallbackWorker]()
		window.budgetFailThrottle = newLogThrottle(window.settings.EvaluationFailureLogInterval)
		window.backendDegradedForTest = func() bool { return false }
		var passLock sync.Mutex
		passCount := 0
		window.afterResizePassForTest = func() {
			passLock.Lock()
			defer passLock.Unlock()
			passCount += 1
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
		time.Sleep(10 * time.Minute)
		cancel()
		loops.Wait()

		generator.stateLock.Lock()
		mintTimes := append([]time.Time{}, generator.mintTimes...)
		listingCount := len(generator.listingTimes)
		generator.stateLock.Unlock()
		passLock.Lock()
		passes := passCount
		passLock.Unlock()
		t.Logf("ten refused minutes: %d mints, %d listings, %d resize passes", len(mintTimes), listingCount, passes)

		maxCount := window.settings.WindowRegistrationBusyRetryMaxCount
		backoffFloor := window.settings.WindowRateLimitBackoffMin / 2
		if len(mintTimes) < maxCount+2 {
			t.Fatalf("%d mints in ten minutes, want the first, its %d short retries and the backoff's", len(mintTimes), maxCount)
		}
		// the first mint and its short retries
		if spell := mintTimes[maxCount].Sub(mintTimes[0]); time.Duration(maxCount)*window.settings.WindowRegistrationBusyRetry < spell {
			t.Fatalf("the first %d retries took %s, want short retries", maxCount, spell)
		}
		// then never faster than the backoff's floor
		for i := maxCount + 1; i < len(mintTimes); i += 1 {
			if gap := mintTimes[i].Sub(mintTimes[i-1]); gap < backoffFloor {
				t.Fatalf("mint %d came %s after the one before, faster than the backoff floor %s", i+1, gap, backoffFloor)
			}
		}
		// the backoff doubles 2.5-5 s to the 1-2 minute cap: at most 13 asks
		// in ten minutes, as before, besides the one spell
		if 1+maxCount+13 < len(mintTimes) {
			t.Fatalf("%d mints in ten minutes, want at most %d", len(mintTimes), 1+maxCount+13)
		}
		if lines := log.linesWith("event=discovery_expand_deferred"); 0 < len(lines) {
			t.Fatalf("passes that only waited on refused mints deferred the expansion: %v", lines[:min(3, len(lines))])
		}
		// one pass per acquisition, with room for the first and last; a hot
		// loop would make thousands
		if limit := int(10*time.Minute/window.settings.WindowExpandTimeout) + 5; limit < passes {
			t.Fatalf("%d resize passes in ten minutes, want at most %d", passes, limit)
		}
		// a listing's mint retries last WindowExpandArgsTimeout
		if limit := int(10*time.Minute/window.settings.WindowExpandArgsTimeout) + 2; limit < listingCount {
			t.Fatalf("%d listings in ten minutes, want at most %d", listingCount, limit)
		}
	})
}
