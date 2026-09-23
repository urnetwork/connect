// A retry retains short caller budgets and one logical observation per call.
package connect

import (
	"context"
	"net/netip"
	"testing"
	"testing/synctest"
	"time"
)

// A replacement cached while the canceled owner retires is immediately
// usable, even when this caller has less than one pacing interval left.
func TestDohSharedFlightFirstHandoffKeepsShortBudgetAndOneObservation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultDohSettings()
		settings.Log = NewNoopLogger()
		settings.RequestTimeout = time.Minute
		settings.DnsResolverSettings = &DnsResolverSettings{}
		cache := NewDohCache(settings)
		defer cache.Close()
		key := NewDohKey("AAAA", "short-owner.example")
		flight := &dohFlight{done: make(chan struct{}), ownerCanceled: true}
		cache.inflight[key] = flight
		ctx, cancel := context.WithTimeout(context.Background(), DefaultDialFallbackDelay/2)
		defer cancel()
		counter := &dohResolverCounters[dohRemoteDialPath(settings)][dohScopeAddress]
		counter.stateLock.Lock()
		before := counter.counts
		counter.stateLock.Unlock()
		started := time.Now()
		result := make(chan dohSharedOwnerResult, 1)
		go func() {
			addrs, authoritative := cache.QueryResult(ctx, "AAAA", "short-owner.example")
			result <- dohSharedOwnerResult{addrs: addrs, authoritative: authoritative}
		}()
		synctest.Wait()
		addr := netip.MustParseAddr("2001:db8::96")
		cache.stateLock.Lock()
		delete(cache.inflight, key)
		cache.queryResultExpiration[key] = &DohResult{
			Time:            time.Now(),
			AddrExpirations: map[netip.Addr]time.Time{addr: time.Now().Add(time.Minute)},
		}
		cache.stateLock.Unlock()
		close(flight.done)
		value := <-result
		if !value.authoritative || len(value.addrs) != 1 || value.addrs[0] != addr || !time.Now().Equal(started) {
			t.Fatal("first handoff spent a live caller's short budget instead of accepting the replacement answer")
		}
		counter.stateLock.Lock()
		after := counter.counts
		counter.stateLock.Unlock()
		for outcome := range after {
			want := uint64(0)
			if outcome == int(dohOutcomeAnswer) {
				want = 1
			}
			if after[outcome]-before[outcome] != want {
				t.Fatalf("outcome=%d delta=%d, want %d for one external caller", outcome, after[outcome]-before[outcome], want)
			}
		}
	})
}

// An earlier caller deadline wins over both the retry floor and cache budget.
func TestDohSharedFlightCallerDeadlineBoundsHandoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultDohSettings()
		settings.Log = NewNoopLogger()
		settings.RequestTimeout = time.Minute
		settings.DnsResolverSettings = &DnsResolverSettings{}
		cache := NewDohCache(settings)
		defer cache.Close()
		flight := &dohFlight{done: make(chan struct{}), ownerCanceled: true}
		close(flight.done)
		cache.inflight[NewDohKey("AAAA", "early-deadline.example")] = flight
		callerBudget := DefaultDialFallbackDelay / 2
		ctx, cancel := context.WithTimeout(context.Background(), callerBudget)
		defer cancel()
		started := time.Now()
		result := make(chan dohSharedOwnerResult, 1)
		go func() {
			addrs, authoritative := cache.QueryResult(ctx, "AAAA", "early-deadline.example")
			result <- dohSharedOwnerResult{addrs: addrs, authoritative: authoritative}
		}()
		value := <-result
		if value.authoritative || len(value.addrs) != 0 || time.Since(started) != callerBudget || ctx.Err() != context.DeadlineExceeded {
			t.Fatal("handoff extended the original caller deadline")
		}
	})
}
