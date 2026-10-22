// Shared resolver callers retain their own cancellation authority.
package connect

import (
	"context"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Observes entry into the follower's real wait, without scheduling sleeps.
type dohSharedOwnerWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

// The follower evaluates this channel after selecting its shared flight.
func (self *dohSharedOwnerWaitContext) Done() <-chan struct{} {
	self.once.Do(func() { close(self.waiting) })
	return self.Context.Done()
}

// Keeps one real wire-format resolution blocked until the test's state edge.
type dohSharedOwnerFixture struct {
	ctx          context.Context
	cache        *DohCache
	firstEntered chan struct{}
	firstRelease chan struct{}
	queries      atomic.Int64
	addr         netip.Addr
}

// Creates an explicitly configured local resolver, with no host fallback.
func newDohSharedOwnerFixture(t *testing.T) *dohSharedOwnerFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	self := &dohSharedOwnerFixture{
		ctx:          ctx,
		firstEntered: make(chan struct{}),
		firstRelease: make(chan struct{}),
		addr:         netip.MustParseAddr("2001:db8::95"),
	}
	self.cache = newDohProgressCache(t, func(writer http.ResponseWriter, request *http.Request, recordType dnsmessage.Type) {
		if recordType != dnsmessage.TypeAAAA {
			t.Error("shared-flight control queried an unexpected record type")
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		if self.queries.Add(1) == 1 {
			close(self.firstEntered)
			select {
			case <-self.firstRelease:
			case <-request.Context().Done():
				return
			}
		}
		writeDohWire(writer, request, []netip.Addr{self.addr}, 60, false)
	})
	return self
}

// Owns the addresses returned across a test goroutine boundary.
type dohSharedOwnerResult struct {
	addrs         []netip.Addr
	authoritative bool
}

// Starts exactly one caller against the same uncached record.
func (self *dohSharedOwnerFixture) query(ctx context.Context) <-chan dohSharedOwnerResult {
	result := make(chan dohSharedOwnerResult, 1)
	go func() {
		addrs, authoritative := self.cache.QueryResult(ctx, "AAAA", "shared-owner.example")
		result <- dohSharedOwnerResult{addrs: addrs, authoritative: authoritative}
	}()
	return result
}

// Waits only for a causal barrier; the deadline is a deadlock watchdog.
func (self *dohSharedOwnerFixture) await(t *testing.T, barrier <-chan struct{}) {
	t.Helper()
	select {
	case <-barrier:
	case <-self.ctx.Done():
		t.Fatal("shared-flight control did not reach its explicit barrier")
	}
}

// Joins a result before examining or retiring its owner.
func (self *dohSharedOwnerFixture) receive(t *testing.T, result <-chan dohSharedOwnerResult) dohSharedOwnerResult {
	t.Helper()
	select {
	case value := <-result:
		return value
	case <-self.ctx.Done():
		t.Fatal("shared-flight caller did not finish before its watchdog")
		return dohSharedOwnerResult{}
	}
}

// A canceled first caller must not terminate another caller's live lookup.
func TestDohSharedFlightLeaderCancellationPreservesLiveWaiter(t *testing.T) {
	self := newDohSharedOwnerFixture(t)
	leaderCtx, cancelLeader := context.WithCancel(self.ctx)
	defer cancelLeader()
	leader := self.query(leaderCtx)
	self.await(t, self.firstEntered)
	waiterCtx := &dohSharedOwnerWaitContext{Context: self.ctx, waiting: make(chan struct{})}
	waiter := self.query(waiterCtx)
	self.await(t, waiterCtx.waiting)
	cancelLeader()
	leaderResult := self.receive(t, leader)
	if leaderResult.authoritative || len(leaderResult.addrs) != 0 {
		t.Fatal("canceled leader unexpectedly published an answer")
	}
	waiterResult := self.receive(t, waiter)
	if self.ctx.Err() != nil || !waiterResult.authoritative || len(waiterResult.addrs) != 1 || waiterResult.addrs[0] != self.addr {
		t.Fatalf("live waiter inherited foreign cancellation: own_error=%v authoritative=%t answers=%d queries=%d", self.ctx.Err(), waiterResult.authoritative, len(waiterResult.addrs), self.queries.Load())
	}
	if self.queries.Load() != 2 {
		t.Fatalf("queries=%d, want the canceled owner followed by one bounded replacement", self.queries.Load())
	}
}

// A healthy shared resolution remains one query with two successful callers.
func TestDohSharedFlightHealthyWaiterControl(t *testing.T) {
	self := newDohSharedOwnerFixture(t)
	leader := self.query(self.ctx)
	self.await(t, self.firstEntered)
	waiterCtx := &dohSharedOwnerWaitContext{Context: self.ctx, waiting: make(chan struct{})}
	waiter := self.query(waiterCtx)
	self.await(t, waiterCtx.waiting)
	close(self.firstRelease)
	for _, result := range []<-chan dohSharedOwnerResult{leader, waiter} {
		value := self.receive(t, result)
		if !value.authoritative || len(value.addrs) != 1 || value.addrs[0] != self.addr {
			t.Fatal("healthy shared resolution lost an answer")
		}
	}
	if self.queries.Load() != 1 {
		t.Fatalf("healthy queries=%d, want one coalesced owner", self.queries.Load())
	}
}

// A canceled follower must return without canceling the healthy leader.
func TestDohSharedFlightWaiterCancellationControl(t *testing.T) {
	self := newDohSharedOwnerFixture(t)
	leader := self.query(self.ctx)
	self.await(t, self.firstEntered)
	waiterBase, cancelWaiter := context.WithCancel(self.ctx)
	defer cancelWaiter()
	waiterCtx := &dohSharedOwnerWaitContext{Context: waiterBase, waiting: make(chan struct{})}
	waiter := self.query(waiterCtx)
	self.await(t, waiterCtx.waiting)
	cancelWaiter()
	value := self.receive(t, waiter)
	if value.authoritative || len(value.addrs) != 0 {
		t.Fatal("canceled follower unexpectedly published an answer")
	}
	close(self.firstRelease)
	value = self.receive(t, leader)
	if !value.authoritative || len(value.addrs) != 1 || value.addrs[0] != self.addr {
		t.Fatal("follower cancellation changed the healthy leader")
	}
	if self.queries.Load() != 1 {
		t.Fatalf("queries=%d, want no replacement for a canceled follower", self.queries.Load())
	}
}
