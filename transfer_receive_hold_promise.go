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
	// The same root admission lock protects free credit and in-flight loans.
	// Intrusive entries exist only while an actual decoded owner is live.
	ingressHead          *ReceivePack
	ingressLoanByteCount ByteCount
	ingressFloor         uint64
	closed               bool
	// Published only after the receive worker advances an ordered generation
	// under a verified contract. This buys physical current-item storage, not
	// contract validity, delivery or ACK authority; those remain worker checks.
	establishedContract bool
}

// holdPromised reports whether an arrival at this number is a missing
// predecessor of an acknowledged held item.
func (self *ReceiveSequence) holdPromised(sequenceNumber uint64) bool {
	budget := self.receiveQueue.budget
	if budget != nil {
		root := budget.admissionRoot()
		root.admissionLock.Lock()
		defer root.admissionLock.Unlock()
	}
	return self.holdPromise.set && self.nextSequenceNumber <= sequenceNumber && sequenceNumber < self.holdPromise.boundary
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
		if budget := self.receiveQueue.budget; budget != nil {
			root := budget.admissionRoot()
			root.admissionLock.Lock()
			defer root.admissionLock.Unlock()
		}
		return self.holdPromise.reservedByteCount
	}
	if budget := self.receiveQueue.Budget(); budget != nil && (budget.Parent() != nil || budget.hasChildren.Load()) {
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
	budget := self.receiveQueue.budget
	if !self.receiveQueue.lifetimeBudget || budget == nil {
		return true
	}
	root := budget.admissionRoot()
	root.admissionLock.Lock()
	defer root.admissionLock.Unlock()
	if self.holdPromise.closed {
		return false
	}
	targetFree := max(0, targetByteCount-self.holdPromise.ingressLoanByteCount)
	if targetFree <= self.holdPromise.reservedByteCount {
		return true
	}
	if !budget.tryReserveWithLock(targetFree - self.holdPromise.reservedByteCount) {
		return false
	}
	self.holdPromise.reservedByteCount = targetFree
	return true
}

// settleHoldPromise records the newest committed item and resizes the
// reservation to what its missing predecessors need now. Growth here is only
// for a larger held frame raising the estimate after the commitment; if the
// budget refuses it the shortfall is handled, and counted, at admission.
func (self *ReceiveSequence) settleHoldPromise(set bool, boundary uint64, requiredByteCount ByteCount) {
	budget := self.receiveQueue.budget
	if budget == nil || !self.receiveQueue.lifetimeBudget {
		self.holdPromise.set = set
		if !set {
			boundary = 0
		}
		self.holdPromise.boundary = boundary
		return
	}
	root := budget.admissionRoot()
	root.admissionLock.Lock()
	promise := &self.holdPromise
	if promise.closed {
		set = false
	}
	if !set {
		boundary, requiredByteCount = 0, 0
	}
	promise.set, promise.boundary, promise.ingressFloor = set, boundary, self.nextSequenceNumber
	// A returned old copy must never replenish a newer promise. Revoking only
	// its association leaves the copy's already-paid physical owner intact.
	for pack := promise.ingressHead; pack != nil; pack = pack.ingressNext {
		if pack.ingressLoanByteCount != 0 && (!set || pack.decodedOwner.receiveItem.sequenceNumber < promise.ingressFloor || boundary <= pack.decodedOwner.receiveItem.sequenceNumber) {
			promise.ingressLoanByteCount -= pack.ingressLoanByteCount
			pack.ingressLoanByteCount = 0
		}
	}
	targetFree := max(0, requiredByteCount-promise.ingressLoanByteCount)
	// Pressure may need the surplus a smaller promised arrival did not use.
	// Keep that portion in this promise until its exact target adopts or retires.
	targetFree = max(targetFree, self.receiveMemoryPressureProtectedPrefixWithLock())
	released := ByteCount(0)
	if promise.reservedByteCount < targetFree {
		if budget.tryReserveWithLock(targetFree - promise.reservedByteCount) {
			promise.reservedByteCount = targetFree
		}
	} else {
		released = promise.reservedByteCount - targetFree
		promise.reservedByteCount = targetFree
		budget.releaseWithLock(released)
		root.grantReserveWaitersWithLock()
	}
	root.admissionLock.Unlock()
	if released != 0 {
		budget.notifyCapacityChanged()
	}
}

// releaseHoldPromise returns the whole reservation when the hold is torn down.
func (self *ReceiveSequence) releaseHoldPromise() {
	if budget := self.receiveQueue.budget; budget != nil {
		root := budget.admissionRoot()
		root.admissionLock.Lock()
		self.holdPromise.closed = true
		released := self.clearReceiveMemoryPressureWithLock() + self.releaseReceiveHeldHeadRepairWithLock()
		root.admissionLock.Unlock()
		if released != 0 {
			budget.notifyCapacityChanged()
		}
	}
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
