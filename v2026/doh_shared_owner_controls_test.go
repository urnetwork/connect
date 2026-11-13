// Retirement, stale answers, and bounded foreign-owner retry controls.
package connect

import (
	"context"
	"net/http"
	"net/netip"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// A useful stale answer wins over its owner's simultaneous cancellation.
func TestDohSharedFlightCanceledOwnerRetainsStaleAnswer(t *testing.T) {
	self := newDohSharedOwnerFixture(t)
	now := time.Now()
	self.cache.stateLock.Lock()
	self.cache.queryResultExpiration[NewDohKey("AAAA", "shared-owner.example")] = &DohResult{
		Time:            now.Add(-time.Minute),
		AddrExpirations: map[netip.Addr]time.Time{self.addr: now.Add(-time.Second)},
	}
	self.cache.stateLock.Unlock()
	leaderCtx, cancelLeader := context.WithCancel(self.ctx)
	defer cancelLeader()
	leader := self.query(leaderCtx)
	self.await(t, self.firstEntered)
	waiterCtx := &dohSharedOwnerWaitContext{Context: self.ctx, waiting: make(chan struct{})}
	waiter := self.query(waiterCtx)
	self.await(t, waiterCtx.waiting)
	cancelLeader()
	for _, result := range []<-chan dohSharedOwnerResult{leader, waiter} {
		value := self.receive(t, result)
		if !value.authoritative || len(value.addrs) != 1 || value.addrs[0] != self.addr {
			t.Fatal("canceled owner discarded a usable retained answer")
		}
	}
	if self.queries.Load() != 1 || self.cache.staleServeCount.Load() != 1 {
		t.Fatalf("queries=%d stale_serves=%d, want one shared stale completion without replacement", self.queries.Load(), self.cache.staleServeCount.Load())
	}
}

// A normal resolver failure must not acquire retry authority from cleanup.
func TestDohSharedFlightOrdinaryFailureDoesNotRetry(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var queries atomic.Int64
	cache := newDohProgressCache(t, func(writer http.ResponseWriter, request *http.Request, recordType dnsmessage.Type) {
		queries.Add(1)
		writer.WriteHeader(http.StatusServiceUnavailable)
	})
	addrs, authoritative := cache.QueryResult(ctx, "AAAA", "unavailable.example")
	if authoritative || len(addrs) != 0 || queries.Load() != 1 {
		t.Fatalf("ordinary failure authoritative=%t answers=%d queries=%d", authoritative, len(addrs), queries.Load())
	}
}

// An authoritative empty result remains final, rather than a retry trigger.
func TestDohSharedFlightAuthoritativeEmptyDoesNotRetry(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var queries atomic.Int64
	cache := newDohProgressCache(t, func(writer http.ResponseWriter, request *http.Request, recordType dnsmessage.Type) {
		queries.Add(1)
		writeDohWire(writer, request, nil, 60, false)
	})
	addrs, authoritative := cache.QueryResult(ctx, "AAAA", "empty.example")
	if !authoritative || len(addrs) != 0 || queries.Load() != 1 {
		t.Fatalf("empty result authoritative=%t answers=%d queries=%d", authoritative, len(addrs), queries.Load())
	}
}

// Retiring the cache cancels the real owner and waiter without replacement.
func TestDohSharedFlightRetirementJoinsOwnerAndWaiter(t *testing.T) {
	self := newDohSharedOwnerFixture(t)
	leader := self.query(self.ctx)
	self.await(t, self.firstEntered)
	waiterCtx := &dohSharedOwnerWaitContext{Context: self.ctx, waiting: make(chan struct{})}
	waiter := self.query(waiterCtx)
	self.await(t, waiterCtx.waiting)
	self.cache.Close()
	for _, result := range []<-chan dohSharedOwnerResult{leader, waiter} {
		value := self.receive(t, result)
		if value.authoritative || len(value.addrs) != 0 {
			t.Fatal("retired resolver unexpectedly published an answer")
		}
	}
	addrs, authoritative := self.cache.QueryResult(self.ctx, "AAAA", "shared-owner.example")
	if authoritative || len(addrs) != 0 || self.queries.Load() != 1 {
		t.Fatal("cache retirement admitted a replacement resolution")
	}
	self.cache.stateLock.Lock()
	retained := len(self.cache.inflight)
	self.cache.stateLock.Unlock()
	if retained != 0 {
		t.Fatalf("retirement retained %d resolver generations", retained)
	}
}

// Repeated foreign cancellation cannot spin or extend a context-free caller
// beyond its original configured lookup budget. A finished-flight tombstone
// models an adversarial canceled owner at every reacquire; no network is used.
func TestDohSharedFlightRepeatedCancellationIsPacedAndBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultDohSettings()
		settings.Log = NewNoopLogger()
		settings.RequestTimeout = 3 * DefaultDialFallbackDelay
		settings.DnsResolverSettings = &DnsResolverSettings{}
		cache := NewDohCache(settings)
		defer cache.Close()
		flight := &dohFlight{done: make(chan struct{}), ownerCanceled: true}
		close(flight.done)
		cache.inflight[NewDohKey("AAAA", "repeated-owner.example")] = flight
		started := time.Now()
		result := make(chan dohSharedOwnerResult, 1)
		go func() {
			addrs, authoritative := cache.QueryResult(context.Background(), "AAAA", "repeated-owner.example")
			result <- dohSharedOwnerResult{addrs: addrs, authoritative: authoritative}
		}()
		// This returns only once the retry is durably blocked, not spinning.
		synctest.Wait()
		if !time.Now().Equal(started) {
			t.Fatal("retry advanced virtual time before its pacing edge")
		}
		select {
		case <-result:
			t.Fatal("foreign cancellation was treated as this live caller's failure")
		default:
		}
		value := <-result
		if value.authoritative || len(value.addrs) != 0 || time.Since(started) != settings.RequestTimeout {
			t.Fatalf("retry completion authoritative=%t answers=%d elapsed=%s", value.authoritative, len(value.addrs), time.Since(started))
		}
	})
}

// A caller can still cancel while paced; the original budget is not a join delay.
func TestDohSharedFlightCancellationInterruptsPacedHandoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultDohSettings()
		settings.Log = NewNoopLogger()
		settings.RequestTimeout = time.Minute
		settings.DnsResolverSettings = &DnsResolverSettings{}
		cache := NewDohCache(settings)
		defer cache.Close()
		flight := &dohFlight{done: make(chan struct{}), ownerCanceled: true}
		close(flight.done)
		cache.inflight[NewDohKey("AAAA", "paced-cancel.example")] = flight
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		started := time.Now()
		result := make(chan dohSharedOwnerResult, 1)
		go func() {
			addrs, authoritative := cache.QueryResult(ctx, "AAAA", "paced-cancel.example")
			result <- dohSharedOwnerResult{addrs: addrs, authoritative: authoritative}
		}()
		synctest.Wait()
		cancel()
		value := <-result
		if value.authoritative || len(value.addrs) != 0 || !time.Now().Equal(started) {
			t.Fatal("caller cancellation waited for the pacing or lookup timer")
		}
	})
}
