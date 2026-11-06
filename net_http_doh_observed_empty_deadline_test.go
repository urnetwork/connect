// Preserve a genuine encrypted empty response across the private fanout
// deadline without manufacturing authority from silence or canceled owners.
package connect

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// The original lookup stays live after a local fanout timer expires.
func TestDohObservedNoDataSurvivesPrivateDeadline(t *testing.T) {
	testDohObservedEmptyPrivateDeadline(t, "AAAA", false)
}

// NXDOMAIN has the same authoritative lifetime as an observed NODATA.
func TestDohObservedNxDomainSurvivesPrivateDeadline(t *testing.T) {
	testDohObservedEmptyPrivateDeadline(t, "A", true)
}

// Every later provider remains blocked; only actual decoded wire may provide
// the negative-cache authority, and the whole collecting interval is retained.
func testDohObservedEmptyPrivateDeadline(t *testing.T, recordType string, nxDomain bool) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		var requests atomic.Int32
		finalResults := make(chan struct{})
		releaseFinalResults := func() { releaseObservedEmptyDeadlineResults(finalResults) }
		cache := newObservedEmptyDeadlineTestCache(func(request *http.Request) (*http.Response, error) {
			if requests.Add(1) != 1 {
				<-request.Context().Done()
				<-finalResults
				return nil, request.Context().Err()
			}
			return observedEmptyDeadlineTestResponse(request, nil, nxDomain), nil
		})
		ctx, cancel := context.WithCancel(t.Context())
		var workers sync.WaitGroup
		defer joinObservedEmptyDeadlineTestCache(t, cache, cancel, &workers, releaseFinalResults)
		done := make(chan observedEmptyDeadlineTestResult, 1)
		started := time.Now()
		workers.Add(1)
		go func() {
			defer workers.Done()
			addrs, authoritative := cache.QueryResult(ctx, recordType, "api.space.example")
			done <- observedEmptyDeadlineTestResult{addrs: addrs, authoritative: authoritative}
		}()
		synctest.Wait()
		if requests.Load() != 5 || len(cache.remoteClient.httpSem) != 4 {
			t.Fatalf("collecting prefix requests=%d occupied=%d, want one observed empty and four held providers", requests.Load(), len(cache.remoteClient.httpSem))
		}
		time.Sleep(cache.settings.RequestTimeout - time.Nanosecond)
		select {
		case <-done:
			t.Fatal("observed empty ended collection before its original fanout deadline")
		default:
		}
		time.Sleep(time.Nanosecond)
		result := <-done
		if ctx.Err() != nil || time.Since(started) != cache.settings.RequestTimeout {
			t.Fatalf("lookup deadline changed: caller=%v elapsed=%v", ctx.Err(), time.Since(started))
		}
		if len(result.addrs) != 0 || !result.authoritative {
			t.Fatalf("DOH_OBSERVED_EMPTY_WAS_LOST_AT_PRIVATE_DEADLINE: addrs=%v authoritative=%v", result.addrs, result.authoritative)
		}
		releaseFinalResults()
		synctest.Wait()
		before := requests.Load()
		addrs, authoritative := cache.QueryResult(ctx, recordType, "api.space.example")
		if len(addrs) != 0 || !authoritative || requests.Load() != before {
			t.Fatalf("genuine empty was not cached: addrs=%v authoritative=%v requests=%d before=%d", addrs, authoritative, requests.Load(), before)
		}
	})
}

// No encrypted response is a temporary failure. It must remain a fresh
// attempt on the same live cache rather than becoming a fabricated miss.
func TestDohPrivateDeadlineWithoutResponseStaysRetryable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var requests atomic.Int32
		finalResults := make(chan struct{})
		releaseFinalResults := func() { releaseObservedEmptyDeadlineResults(finalResults) }
		cache := newObservedEmptyDeadlineTestCache(func(request *http.Request) (*http.Response, error) {
			finish := finalResults
			requests.Add(1)
			<-request.Context().Done()
			<-finish
			return nil, request.Context().Err()
		})
		ctx, cancel := context.WithCancel(t.Context())
		var workers sync.WaitGroup
		defer joinObservedEmptyDeadlineTestCache(t, cache, cancel, &workers, releaseFinalResults)
		for attempt := range 2 {
			if attempt != 0 {
				finalResults = make(chan struct{})
			}
			before := requests.Load()
			done := make(chan observedEmptyDeadlineTestResult, 1)
			workers.Add(1)
			go func() {
				defer workers.Done()
				addrs, authoritative := cache.QueryResult(ctx, "AAAA", "unanswered.space.example")
				done <- observedEmptyDeadlineTestResult{addrs: addrs, authoritative: authoritative}
			}()
			synctest.Wait()
			if requests.Load()-before != 4 {
				t.Fatalf("unanswered generation %d did not dispatch fresh work: requests=%d before=%d", attempt, requests.Load(), before)
			}
			time.Sleep(cache.settings.RequestTimeout)
			result := <-done
			if len(result.addrs) != 0 || result.authoritative || ctx.Err() != nil {
				t.Fatalf("DOH_SILENCE_MANUFACTURED_AUTHORITATIVE_EMPTY: attempt=%d addrs=%v authoritative=%v caller=%v", attempt, result.addrs, result.authoritative, ctx.Err())
			}
			releaseFinalResults()
			synctest.Wait()
		}
	})
}

// An earlier negative cannot override a usable response arriving one tick
// before the existing deadline. The positive record, not the empty, is cached.
func TestDohObservedEmptyKeepsCollectingUntilLatePositive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var requests atomic.Int32
		positive := make(chan struct{})
		answer := netip.MustParseAddr("192.0.2.91")
		cache := newObservedEmptyDeadlineTestCache(func(request *http.Request) (*http.Response, error) {
			switch requests.Add(1) {
			case 1:
				return observedEmptyDeadlineTestResponse(request, nil, true), nil
			case 2:
				select {
				case <-positive:
					return observedEmptyDeadlineTestResponse(request, []netip.Addr{answer}, false), nil
				case <-request.Context().Done():
					return nil, request.Context().Err()
				}
			default:
				<-request.Context().Done()
				return nil, request.Context().Err()
			}
		})
		ctx, cancel := context.WithCancel(t.Context())
		var workers sync.WaitGroup
		defer joinObservedEmptyDeadlineTestCache(t, cache, cancel, &workers)
		done := make(chan observedEmptyDeadlineTestResult, 1)
		workers.Add(1)
		go func() {
			defer workers.Done()
			addrs, authoritative := cache.QueryResult(ctx, "A", "late.space.example")
			done <- observedEmptyDeadlineTestResult{addrs: addrs, authoritative: authoritative}
		}()
		synctest.Wait()
		time.Sleep(cache.settings.RequestTimeout - time.Nanosecond)
		select {
		case result := <-done:
			t.Fatalf("DOH_EARLY_EMPTY_OVERRULED_LATE_POSITIVE: result=%+v", result)
		default:
		}
		close(positive)
		synctest.Wait()
		result := <-done
		if !result.authoritative || !slices.Equal(result.addrs, []netip.Addr{answer}) {
			t.Fatalf("late real answer lost: %+v", result)
		}
		before := requests.Load()
		addrs, authoritative := cache.QueryResult(ctx, "A", "late.space.example")
		if !authoritative || !slices.Equal(addrs, []netip.Addr{answer}) || requests.Load() != before {
			t.Fatalf("positive was not cached: addrs=%v authoritative=%v requests=%d before=%d", addrs, authoritative, requests.Load(), before)
		}
	})
}

// A caller ending before the collection deadline cannot cache a new empty.
func TestDohObservedEmptyCallerCancellationStaysUncached(t *testing.T) {
	testDohObservedEmptyCanceledOwner(t, false)
}

// Retiring the cache has the same fence and joins every held request.
func TestDohObservedEmptyCacheRetirementStaysUncached(t *testing.T) {
	testDohObservedEmptyCanceledOwner(t, true)
}

// The first real empty is decoded before either explicit cancellation fence.
func testDohObservedEmptyCanceledOwner(t *testing.T, retire bool) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		var requests atomic.Int32
		finalResults := make(chan struct{})
		releaseFinalResults := func() { releaseObservedEmptyDeadlineResults(finalResults) }
		cache := newObservedEmptyDeadlineTestCache(func(request *http.Request) (*http.Response, error) {
			if requests.Add(1) == 1 {
				return observedEmptyDeadlineTestResponse(request, nil, false), nil
			}
			<-request.Context().Done()
			<-finalResults
			return nil, request.Context().Err()
		})
		ctx, cancel := context.WithCancel(t.Context())
		var workers sync.WaitGroup
		defer joinObservedEmptyDeadlineTestCache(t, cache, cancel, &workers, releaseFinalResults)
		done := make(chan observedEmptyDeadlineTestResult, 1)
		workers.Add(1)
		go func() {
			defer workers.Done()
			addrs, authoritative := cache.QueryResult(ctx, "AAAA", "canceled.space.example")
			done <- observedEmptyDeadlineTestResult{addrs: addrs, authoritative: authoritative}
		}()
		synctest.Wait()
		if requests.Load() != 5 || len(cache.remoteClient.httpSem) != 4 {
			t.Fatalf("cancellation prefix lacked the observed empty and held siblings: requests=%d occupied=%d", requests.Load(), len(cache.remoteClient.httpSem))
		}
		if retire {
			workers.Add(1)
			go func() {
				defer workers.Done()
				cache.Close()
			}()
		} else {
			cancel()
		}
		result := <-done
		if len(result.addrs) != 0 || result.authoritative {
			t.Fatalf("DOH_CANCELED_FANOUT_ACQUIRED_EMPTY_AUTHORITY: result=%+v retire=%t", result, retire)
		}
		cache.stateLock.Lock()
		entry := cache.queryResultExpiration[NewDohKey("AAAA", "canceled.space.example")]
		cache.stateLock.Unlock()
		if entry != nil {
			t.Fatal("canceled partial collection was negative-cached")
		}
	})
}

// One caller result, transported only after the public cache call returns.
type observedEmptyDeadlineTestResult struct {
	addrs         []netip.Addr
	authoritative bool
}

// All request work uses the real cache, fanout, HTTP client and wire parser.
// The transport controls completion order without sockets or scheduler sleeps.
func newObservedEmptyDeadlineTestCache(roundTrip dohRoundTripperFunc) *DohCache {
	settings := DefaultDohSettings()
	settings.Log = NewNoopLogger()
	settings.RequestTimeout = time.Second
	settings.MaxConcurrentHttpRequests = 4
	settings.DohServerStagger = 0
	settings.DnsResolverSettings = &DnsResolverSettings{
		EnableRemoteDoh: true,
		RemoteDohUrlsIpv4: []string{
			"https://192.0.2.51/dns-query",
			"https://192.0.2.52/dns-query",
			"https://192.0.2.53/dns-query",
			"https://192.0.2.54/dns-query",
			"https://192.0.2.55/dns-query",
		},
	}
	cache := NewDohCache(settings)
	cache.remoteClient.httpClient.Transport = roundTrip
	return cache
}

// Generate real DNS response bytes from the actual encrypted request's question.
func observedEmptyDeadlineTestResponse(request *http.Request, records []netip.Addr, nxDomain bool) *http.Response {
	recorder := httptest.NewRecorder()
	writeDohWire(recorder, request, records, 60, nxDomain)
	response := recorder.Result()
	response.Request = request
	return response
}

// Cancel admission and transport work, retire the owner, and join its callers
// before asserting all shared request credits returned exactly once.
func joinObservedEmptyDeadlineTestCache(t *testing.T, cache *DohCache, cancel context.CancelFunc, workers *sync.WaitGroup, releaseResults ...func()) {
	t.Helper()
	cancel()
	for _, release := range releaseResults {
		release()
	}
	cache.Close()
	workers.Wait()
	synctest.Wait()
	if len(cache.remoteClient.httpSem) != 0 {
		t.Errorf("deadline controls retained %d HTTP credits", len(cache.remoteClient.httpSem))
	}
}

// Only the controlling test goroutine releases completed transport results.
// Withholding them pins the private-deadline branch, not the all-results branch.
func releaseObservedEmptyDeadlineResults(release chan struct{}) {
	select {
	case <-release:
	default:
		close(release)
	}
}
