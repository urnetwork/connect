// Owners that wait for transfer memory are served in arrival order: whatever
// frees capacity reserves it for the oldest waiters it fits before anyone
// woken can run, so a releaser coming straight back cannot take it again.
package connect

import "testing"

// Report whether a queued reservation has been granted, without waiting.
func reserveGranted(waiter *transferMemoryReserveWaiter) bool {
	select {
	case <-waiter.ready:
		return true
	default:
		return false
	}
}

// A release reserves for the oldest waiter before its releaser can reserve
// again, so an owner that comes straight back queues behind every waiter it
// left. Each later release serves the next waiter in arrival order.
func TestTransferMemoryBudgetReleaseReservesForOldestWaiterFirst(t *testing.T) {
	budget := NewTransferMemoryBudget(200)
	for range 2 {
		if reserved, waiter := budget.tryReserveOrQueue(100); !reserved || waiter != nil {
			t.Fatal("an empty budget did not reserve at once")
		}
	}
	first, waiter := budget.tryReserveOrQueue(100)
	if first || waiter == nil {
		t.Fatal("a full budget did not queue the first waiter")
	}
	firstWaiter := waiter
	_, secondWaiter := budget.tryReserveOrQueue(100)

	budget.Release(100)
	if !reserveGranted(firstWaiter) || reserveGranted(secondWaiter) {
		t.Fatal("a release did not reserve for the oldest waiter alone")
	}
	reserved, returningWaiter := budget.tryReserveOrQueue(100)
	if reserved {
		t.Fatal("the releaser coming straight back took the bytes ahead of the second waiter")
	}
	budget.Release(100)
	if !reserveGranted(secondWaiter) || reserveGranted(returningWaiter) {
		t.Fatal("the next release did not serve the second waiter ahead of the returning owner")
	}
	budget.Release(100)
	if !reserveGranted(returningWaiter) {
		t.Fatal("the returning owner was not served after the waiters ahead of it")
	}
	if used := budget.UsedByteCount(); used != 200 {
		t.Fatalf("budget holds %d bytes for two granted owners, want 200", used)
	}
	budget.Release(200)
	assertRetainedBudgetBalance(t, budget)
}

// A waiter that does not fit keeps its place while a later waiter that fits
// is served, and a request that fits the free capacity takes it at once:
// no queued waiter fits it, so the request passes nobody it could serve.
func TestTransferMemoryBudgetQueuedRequestThatDoesNotFitKeepsItsPlace(t *testing.T) {
	budget := NewTransferMemoryBudget(100)
	if !budget.TryReserve(100) {
		t.Fatal("holder could not fill the budget")
	}
	_, largeWaiter := budget.tryReserveOrQueue(80)
	_, smallWaiter := budget.tryReserveOrQueue(20)

	budget.Release(20)
	if reserveGranted(largeWaiter) || !reserveGranted(smallWaiter) {
		t.Fatal("a release that fits only the later waiter did not serve it alone")
	}
	budget.Release(50)
	if reserveGranted(largeWaiter) {
		t.Fatal("the large waiter was served without room for it")
	}
	if reserved, waiter := budget.tryReserveOrQueue(30); !reserved || waiter != nil {
		t.Fatal("a request that fits free bytes no waiter fits was queued")
	}
	budget.Release(30)
	if reserveGranted(largeWaiter) {
		t.Fatal("the large waiter was served without room for it")
	}
	budget.Release(50)
	if !reserveGranted(largeWaiter) {
		t.Fatal("the large waiter was not served once its bytes were free")
	}
	budget.Release(80)
	assertRetainedBudgetBalance(t, budget)
	if reserved, waiter := budget.tryReserveOrQueue(-1); reserved || waiter != nil {
		t.Fatal("a negative request was reserved or queued")
	}
}

// A waiter that stops waiting leaves the line, and bytes already reserved for
// it pass to the next waiter as their release would.
func TestTransferMemoryBudgetAbandonedReservePassesOnItsGrant(t *testing.T) {
	budget := NewTransferMemoryBudget(100)
	if !budget.TryReserve(100) {
		t.Fatal("holder could not fill the budget")
	}
	_, firstWaiter := budget.tryReserveOrQueue(100)
	_, canceledWaiter := budget.tryReserveOrQueue(100)
	_, lastWaiter := budget.tryReserveOrQueue(100)
	canceledWaiter.abandon()

	budget.Release(100)
	if !reserveGranted(firstWaiter) || reserveGranted(lastWaiter) {
		t.Fatal("a release did not serve the first waiter alone")
	}
	firstWaiter.abandon()
	if !reserveGranted(lastWaiter) || reserveGranted(canceledWaiter) {
		t.Fatal("an abandoned grant did not pass to the waiter behind one that left")
	}
	if used := budget.UsedByteCount(); used != 100 {
		t.Fatalf("budget holds %d bytes for one granted owner, want 100", used)
	}
	budget.Release(100)
	assertRetainedBudgetBalance(t, budget)
}

// Waiters of a whole tree share one line at the root. A release in one child,
// a resize, and a move out of a full child each serve the waiters they make
// room for, checked against every ceiling on the waiter's own chain.
func TestTransferMemoryBudgetTreeServesWaitersOnEveryCapacityIncrease(t *testing.T) {
	root := NewTransferMemoryBudget(100)
	left := NewTransferMemoryBudgetWithParent(100, root)
	right := NewTransferMemoryBudgetWithParent(100, root)
	if !left.TryReserve(100) {
		t.Fatal("left child could not fill the root")
	}
	_, rightWaiter := right.tryReserveOrQueue(50)
	left.Release(50)
	if !reserveGranted(rightWaiter) {
		t.Fatal("a release in one child did not serve a sibling's waiter")
	}

	_, rootWaiter := root.tryReserveOrQueue(50)
	root.SetTotalByteCount(150)
	if !reserveGranted(rootWaiter) {
		t.Fatal("growing the root did not serve its waiter")
	}

	root.SetTotalByteCount(300)
	left.SetTotalByteCount(50)
	_, leftWaiter := left.tryReserveOrQueue(50)
	if !left.tryMoveReservation(right, 50) {
		t.Fatal("left child could not move its claim to its sibling")
	}
	if !reserveGranted(leftWaiter) {
		t.Fatal("a move out of a full child did not serve that child's waiter")
	}
	left.Release(50)
	right.Release(100)
	root.Release(50)
	for _, budget := range []*TransferMemoryBudget{root, left, right} {
		if used := budget.UsedByteCount(); used != 0 {
			t.Fatalf("budget holds %d bytes after every owner released", used)
		}
	}
}
