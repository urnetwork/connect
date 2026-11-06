// Overlapping SDK role ceilings preserve exact root admission and existing FIFO grants.
package connect

import (
	"context"
	"testing"
)

// Literal SDK32/64 owner values are checked separately at the real SDK constructor.
type providerSendBorrowBudgetCase struct {
	name                                       string
	root, client, provider, nat, send, receive ByteCount
}

// No physical allocation or byte-window conclusion follows from these permissions.
func providerSendBorrowBudgetCases() []providerSendBorrowBudgetCase {
	return []providerSendBorrowBudgetCase{
		{name: "low32", root: 21810380, client: 15099494, provider: 3355443, nat: 3355443, send: 1438047, receive: 1917396},
		{name: "android64", root: 43620760, client: 30198988, provider: 6710886, nat: 6710886, send: 2876094, receive: 3834792},
	}
}

// Every tracked claim or queued/granted waiter has exactly one cleanup owner.
type providerSendBorrowBudgetFixture struct {
	t                                                      *testing.T
	root, client, provider, nat, send, receive, clientSend *TransferMemoryBudget
	held                                                   map[*TransferMemoryBudget]ByteCount
	waiters                                                map[*transferMemoryReserveWaiter]bool
}

// Fill the root with an expanded provider plus live client/NAT/RX siblings.
func newProviderSendBorrowBudgetFixture(t *testing.T, row providerSendBorrowBudgetCase) *providerSendBorrowBudgetFixture {
	t.Helper()
	f := &providerSendBorrowBudgetFixture{t: t, root: NewTransferMemoryBudget(row.root), held: map[*TransferMemoryBudget]ByteCount{}, waiters: map[*transferMemoryReserveWaiter]bool{}}
	f.client = NewTransferMemoryBudgetWithParent(row.client, f.root)
	f.provider = NewTransferMemoryBudgetWithParent(row.provider+row.send, f.root)
	f.nat = NewTransferMemoryBudgetWithParent(row.nat, f.root)
	f.send = NewTransferMemoryBudgetWithParent(2*row.send, f.provider)
	f.receive = NewTransferMemoryBudgetWithParent(row.receive, f.provider)
	f.clientSend = NewTransferMemoryBudgetWithParent(row.client*3/7, f.client)
	t.Cleanup(func() {
		for waiter := range f.waiters {
			waiter.abandon()
		}
		for budget, bytes := range f.held {
			if 0 < bytes {
				budget.Release(bytes)
			}
		}
		for _, budget := range []*TransferMemoryBudget{f.root, f.client, f.provider, f.nat, f.send, f.receive, f.clientSend} {
			stats := budget.Stats()
			if stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
				t.Errorf("borrow grant fixture retained claim: %+v", stats)
			}
		}
	})
	f.reserve(f.client, row.client-row.send)
	f.reserve(f.nat, row.nat)
	f.reserve(f.receive, row.receive)
	f.reserve(f.send, 2*row.send)
	if f.root.UsedByteCount() != row.root {
		t.Fatalf("%s did not fill actual shared root", row.name)
	}
	return f
}

// Record only an admitted claim, so a setup failure cannot over-release.
func (self *providerSendBorrowBudgetFixture) reserve(budget *TransferMemoryBudget, bytes ByteCount) {
	self.t.Helper()
	if !budget.TryReserve(bytes) {
		self.t.Fatalf("reserve %d failed: %+v", bytes, budget.Stats())
	}
	self.held[budget] += bytes
}

// A fixture's held map never includes grants still owned by waiters.
func (self *providerSendBorrowBudgetFixture) release(budget *TransferMemoryBudget, bytes ByteCount) {
	self.t.Helper()
	if self.held[budget] < bytes {
		self.t.Fatal("release exceeds held fixture claim")
	}
	self.held[budget] -= bytes
	budget.Release(bytes)
}

// Tests queue under deterministic full-root pressure, without sleeps or polling.
func (self *providerSendBorrowBudgetFixture) queue(budget *TransferMemoryBudget, bytes ByteCount) *transferMemoryReserveWaiter {
	self.t.Helper()
	reserved, waiter := budget.tryReserveOrQueue(bytes)
	if reserved {
		self.held[budget] += bytes
		self.t.Fatal("full-root request unexpectedly admitted")
	}
	if waiter == nil {
		self.t.Fatal("full-root request has no waiter")
	}
	self.waiters[waiter] = true
	return waiter
}

// Convert a granted reservation into the fixture's ordinary final-release owner.
func (self *providerSendBorrowBudgetFixture) takeGrant(waiter *transferMemoryReserveWaiter) {
	self.t.Helper()
	if !reserveGranted(waiter) || !self.waiters[waiter] {
		self.t.Fatal("unowned/ungranted reservation consumed")
	}
	delete(self.waiters, waiter)
	self.held[waiter.budget] += waiter.byteCount
}

// Both pending cancellation and canceled-after-grant use the same real primitive.
func (self *providerSendBorrowBudgetFixture) abandon(waiter *transferMemoryReserveWaiter) {
	self.t.Helper()
	if !self.waiters[waiter] {
		self.t.Fatal("waiter abandoned twice")
	}
	delete(self.waiters, waiter)
	waiter.abandon()
}

// Provider release gives the oldest eligible client writer its bytes before
// the provider can refill; no client ceiling shrinks and no live owner is revoked.
func TestProviderSendBorrowOldestClientGrantBeforeRefill(t *testing.T) {
	for _, row := range providerSendBorrowBudgetCases() {
		f := newProviderSendBorrowBudgetFixture(t, row)
		client := f.queue(f.clientSend, row.send)
		provider := f.queue(f.send, row.send)
		f.release(f.send, row.send)
		if !reserveGranted(client) || reserveGranted(provider) || f.send.TryReserve(row.send) || f.client.TotalByteCount() != row.client {
			t.Fatalf("%s provider refill overtook queued client", row.name)
		}
		f.takeGrant(client)
		if f.root.UsedByteCount() != row.root || f.client.UsedByteCount() != row.client {
			t.Fatalf("%s grant lost parent ownership", row.name)
		}
		f.release(f.clientSend, row.send)
		if !reserveGranted(provider) {
			t.Fatalf("%s client return did not serve waiting provider", row.name)
		}
		f.takeGrant(provider)
		if f.root.UsedByteCount() != row.root || f.send.UsedByteCount() != 2*row.send {
			t.Fatalf("%s returned loan lost ownership", row.name)
		}
	}
}

// Cancel before or after the grant; the shared client and provider parents stay
// live, and the canceled reservation cannot strand the next eligible writer.
func TestProviderSendBorrowCanceledClientGrantReturnsCapacity(t *testing.T) {
	for _, row := range providerSendBorrowBudgetCases() {
		for _, afterGrant := range []bool{false, true} {
			f := newProviderSendBorrowBudgetFixture(t, row)
			ctx, cancel := context.WithCancel(context.Background())
			client := f.queue(f.clientSend, row.send)
			provider := f.queue(f.send, row.send)
			if afterGrant {
				f.release(f.send, row.send)
				if !reserveGranted(client) || reserveGranted(provider) {
					t.Fatalf("%s missing exact first grant", row.name)
				}
			}
			cancel()
			if ctx.Err() != context.Canceled {
				t.Fatal("cancellation not materialized")
			}
			f.abandon(client)
			if !afterGrant {
				f.release(f.send, row.send)
			}
			if !reserveGranted(provider) || f.clientSend.UsedByteCount() != 0 {
				t.Fatalf("%s cancel afterGrant=%v lost or retained credit", row.name, afterGrant)
			}
			f.takeGrant(provider)
			if f.root.UsedByteCount() != row.root {
				t.Fatalf("%s cancellation changed unrelated ownership", row.name)
			}
		}
	}
}

// Controls with zero-wait admission refuse at a full root. Once capacity is
// returned they acquire exact backing, survive cancellation, and release once.
func TestProviderSendBorrowZeroWaitControlRefusalAndRecovery(t *testing.T) {
	for _, row := range providerSendBorrowBudgetCases() {
		f := newProviderSendBorrowBudgetFixture(t, row)
		queue := retainedControlTestSendQueue()
		queue.setBudget(f.clientSend, 0)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		source := &SendSequence{ctx: ctx, resendQueue: queue}
		// No item is enqueued here; use the production destructor even on failure.
		clearQueue := func() {
			itemCount, _ := queue.QueueSize()
			source.releaseRetainedSendItems(context.Canceled)
			if itemCount != 0 {
				t.Errorf("%s control-only queue retained %d unexpected send items", row.name, itemCount)
			}
		}
		t.Cleanup(clearQueue)
		required := ByteCount(257)
		total := required + transferRetainedControlCreditByteCount()
		before := f.root.Stats()
		if credit, ok := source.reserveRetainedControlMemory(required); ok || credit != nil || f.root.Stats() != before {
			if credit != nil {
				credit.Release()
			}
			t.Fatalf("%s zero-wait control bypassed full root", row.name)
		}
		f.release(f.send, total)
		credit, ok := source.reserveRetainedControlMemory(required)
		if credit != nil {
			t.Cleanup(credit.Release)
		}
		if !ok || credit == nil || f.clientSend.UsedByteCount() != total || f.root.UsedByteCount() != row.root {
			t.Fatalf("%s released bytes did not fund full control owner", row.name)
		}
		cancel()
		clearQueue()
		if _, ok := source.reserveRetainedControlMemory(1); ok || f.clientSend.UsedByteCount() != total {
			t.Fatalf("%s cancel evicted custody or reopened source", row.name)
		}
		credit.Release()
		credit.Release()
		healthyQueue := retainedControlTestSendQueue()
		healthyQueue.setBudget(f.clientSend, 0)
		healthy := &SendSequence{ctx: context.Background(), resendQueue: healthyQueue}
		t.Cleanup(func() {
			itemCount, _ := healthyQueue.QueueSize()
			healthy.releaseRetainedSendItems(context.Canceled)
			if itemCount != 0 {
				t.Errorf("%s healthy control-only queue retained %d unexpected send items", row.name, itemCount)
			}
		})
		siblingCredit, ok := healthy.reserveRetainedControlMemory(required)
		if siblingCredit != nil {
			t.Cleanup(siblingCredit.Release)
		}
		if !ok || siblingCredit == nil || f.root.UsedByteCount() != row.root {
			t.Fatalf("%s healthy sibling did not recover", row.name)
		}
		siblingCredit.Release()
	}
}
