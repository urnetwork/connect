// Shared-flight cancellation must not multiply one caller's lifetime across
// other live callers. These controls do not identify production request mix.
package connect

import (
	"context"
	"testing"
)

// Starts a finite group only after the real resolver is held, and observes
// each caller entering the existing flight before releasing its owner.
func dohSharedFlightWaiterGroup(t *testing.T, cancelOwner bool) {
	t.Helper()
	self := newDohSharedOwnerFixture(t)
	ownerCtx, ownerCancel := context.WithCancel(self.ctx)
	defer ownerCancel()
	owner := self.query(ownerCtx)
	self.await(t, self.firstEntered)

	const waiterCount = 8
	waiters := make([]<-chan dohSharedOwnerResult, 0, waiterCount)
	for range waiterCount {
		ctx := &dohSharedOwnerWaitContext{Context: self.ctx, waiting: make(chan struct{})}
		waiters = append(waiters, self.query(ctx))
		self.await(t, ctx.waiting)
	}
	if cancelOwner {
		ownerCancel()
	} else {
		close(self.firstRelease)
	}
	ownerResult := self.receive(t, owner)
	if cancelOwner {
		if ownerResult.authoritative || len(ownerResult.addrs) != 0 {
			t.Fatal("canceled owner unexpectedly produced an answer")
		}
	} else if !ownerResult.authoritative || len(ownerResult.addrs) != 1 || ownerResult.addrs[0] != self.addr {
		t.Fatal("healthy owner did not publish the configured resolver answer")
	}

	// Join all finite callers before reporting a failure, so the test owns the
	// complete failure path as well as success. The fixture closes the cache.
	failed := 0
	for _, waiter := range waiters {
		value := self.receive(t, waiter)
		if !value.authoritative || len(value.addrs) != 1 || value.addrs[0] != self.addr {
			failed++
		}
	}
	if err := self.ctx.Err(); err != nil {
		t.Fatalf("waiter group lost its independent lifetime: %v", err)
	}
	if failed != 0 {
		t.Fatalf("one owner lifetime poisoned %d of %d independently live waiters", failed, waiterCount)
	}
	wantQueries := int64(1)
	if cancelOwner {
		wantQueries++
	}
	if actual := self.queries.Load(); actual != wantQueries {
		t.Fatalf("queries=%d, want %d coalesced resolver generations", actual, wantQueries)
	}
}

// One foreign cancellation cannot be a terminal failure for every live waiter.
func TestDohSharedFlightCanceledOwnerPreservesWaiterGroup(t *testing.T) {
	dohSharedFlightWaiterGroup(t, true)
}

// Healthy concurrency still coalesces the entire group into one wire query.
func TestDohSharedFlightHealthyWaiterGroupControl(t *testing.T) {
	dohSharedFlightWaiterGroup(t, false)
}
