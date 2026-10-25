// The committed-prefix hold acknowledges an out-of-order item only when it can
// keep it, which is a promise about every item still missing below it: each
// will be admitted when it arrives. One admission test decides both an arrival
// and that promise, and the shared budget part of the promise is reserved when
// it is made, so a budget other owners fill afterwards cannot break it.
//
// Without the reservation a hold whose shared budget is spent acknowledges
// items whose predecessors it then refuses. The gap can then close only when
// the exact next Pack arrives, one sender retransmission at a time, and a gap
// of tens of Packs outlives the gap timeout: every later flow on the sequence
// waits behind it, and the timeout discards everything the hold acknowledged.
// The pack worker owns all of this state.
package connect

import (
	"math"
)

// receiveHoldPromise is what the committed prefix owes its missing
// predecessors. boundary is the sequence number of the newest committed held
// item; every missing number below it is promised.
type receiveHoldPromise struct {
	set      bool
	boundary uint64
	// Retained bytes reserved in the hold's exact shared budget for the
	// missing predecessors. A promised arrival takes its charge from here;
	// the rest is released as the gap fills or the delivery point passes the
	// boundary, and all of it when the sequence exits.
	reservedByteCount ByteCount
}

// holdPromised reports whether an arrival at this number is a missing
// predecessor of an acknowledged held item.
func (self *ReceiveSequence) holdPromised(sequenceNumber uint64) bool {
	return self.holdPromise.set && self.nextSequenceNumber <= sequenceNumber &&
		sequenceNumber < self.holdPromise.boundary
}

// holdPromiseCredit is the shared-budget headroom a promised arrival may use
// beyond what the budget has available: on an exact budget the reservation
// made for it, and on a standalone legacy budget, which admits by overdraft,
// whatever it needs. The per-sequence window still applies to every arrival.
func (self *ReceiveSequence) holdPromiseCredit(promised bool) ByteCount {
	if !promised {
		return 0
	}
	if self.receiveQueue.lifetimeBudget {
		return self.holdPromise.reservedByteCount
	}
	if budget := self.receiveQueue.Budget(); budget != nil &&
		(budget.Parent() != nil || budget.hasChildren.Load()) {
		// an exact hierarchy never overdraws, and a hierarchical queue always
		// runs with exact (lifetime) admission
		return 0
	}
	return ByteCount(math.MaxInt64)
}

// holdPromiseFits is the arrival window test applied to the last of `missing`
// predecessors of a held item: heldByteCount protocol bytes are held through
// that item and the other predecessors have arrived at the largest held size.
func (self *ReceiveSequence) holdPromiseFits(heldByteCount ByteCount, missing int64, frameByteCount ByteCount) bool {
	if missing <= 0 {
		return true
	}
	return transferQueueWindowAdmits(
		heldByteCount+ByteCount(missing-1)*frameByteCount,
		frameByteCount,
		self.receiveBufferSettings.ReceiveQueueMaxByteCount,
	)
}

// reserveHoldPromise grows the reservation to targetByteCount. Only an exact
// (lifetime) budget refuses; there is nothing to reserve otherwise.
func (self *ReceiveSequence) reserveHoldPromise(targetByteCount ByteCount) bool {
	budget := self.receiveQueue.Budget()
	if !self.receiveQueue.lifetimeBudget || budget == nil ||
		targetByteCount <= self.holdPromise.reservedByteCount {
		return true
	}
	if !budget.TryReserve(targetByteCount - self.holdPromise.reservedByteCount) {
		return false
	}
	self.holdPromise.reservedByteCount = targetByteCount
	return true
}

// settleHoldPromise records the newest committed item and resizes the
// reservation to what its missing predecessors need now. Growth here is only
// for a larger held frame raising the estimate after the commitment; if the
// budget refuses it the shortfall is handled, and counted, at admission.
func (self *ReceiveSequence) settleHoldPromise(set bool, boundary uint64, requiredByteCount ByteCount) {
	if !set {
		boundary, requiredByteCount = 0, 0
	}
	self.holdPromise.set = set
	self.holdPromise.boundary = boundary
	if self.holdPromise.reservedByteCount < requiredByteCount {
		self.reserveHoldPromise(requiredByteCount)
		return
	}
	if excess := self.holdPromise.reservedByteCount - requiredByteCount; 0 < excess {
		self.holdPromise.reservedByteCount = requiredByteCount
		if budget := self.receiveQueue.Budget(); budget != nil {
			budget.Release(excess)
		}
	}
}

// releaseHoldPromise returns the whole reservation when the hold is torn down.
func (self *ReceiveSequence) releaseHoldPromise() {
	self.settleHoldPromise(false, 0, 0)
}

// heldMissingBelow counts the numbers missing between the delivery point and a
// held item with `heldBelow` held items before it. Sequence numbers are dense,
// one per Pack.
func heldMissingBelow(sequenceNumber uint64, nextSequenceNumber uint64, heldBelow int) int64 {
	return max(0, int64(sequenceNumber)-int64(nextSequenceNumber)-int64(heldBelow))
}

// withdrawHeldCommitments tells the sender, before the hold is discarded, that
// every acknowledged item in it did not survive. A selective acknowledgement
// leases the item at the sender for SelectiveAckTimeout, so without the notice
// a replacement sequence would reach each leased item only by probe or at the
// lease's expiry. The notices ride the final acknowledgements the ACK worker
// drains while stopping.
func (self *ReceiveSequence) withdrawHeldCommitments() {
	if !self.receiveBufferSettings.EvictionNotice {
		return
	}
	defer func() {
		clear(self.heldScratch)
		self.heldScratch = self.heldScratch[:0]
	}()
	self.heldScratch = self.receiveQueue.AscendingItems(self.heldScratch)
	for _, held := range self.heldScratch {
		if held.committed {
			self.client.receiveQueueExitWithdrawCount.Add(1)
			self.noteEviction(held.sequenceNumber)
		}
	}
}
