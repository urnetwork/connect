// A decoded Pack owns one physical RX claim until its last real buffer return.
// The existing receive worker publishes prefix credit under the same root lock
// used by ingress. Pending identities are intrusive in live owners, not history.
package connect

// Exact owned decoding is enabled by the established retained RX setting.
// Standalone legacy/floor callers keep their existing admission contract.
func (self *ReceiveBufferSettings) ownsDecodedReceiveMemory() bool {
	budget := self.ReceiveQueueBudget
	return budget != nil && (self.ReceiveQueueRetainedByteAccounting || budget.Parent() != nil)
}

// Root lock held. Only an actual missing predecessor can use this promise.
func (self *ReceiveSequence) receiveMemoryPrefixCreditWithLock(number uint64) ByteCount {
	promise := &self.holdPromise
	if promise.closed || !promise.set || number < promise.ingressFloor || promise.boundary <= number {
		return 0
	}
	return promise.reservedByteCount
}

// Only the current head may protect prefix credit. A later target must leave
// earlier originals free to verify a new proof using their existing promise.
func (self *ReceiveSequence) receiveMemoryPressurePrefixCreditWithLock(number uint64) ByteCount {
	if number != self.holdPromise.ingressFloor {
		return 0
	}
	return self.receiveMemoryPrefixCreditWithLock(number)
}

// Keep the exact head's still-missing portion in its original promise. This
// fences credit during synchronous reclaim without moving or double-counting
// it, including while another promised item is adopted.
func (self *ReceiveSequence) receiveMemoryPressureProtectedPrefixWithLock() ByteCount {
	state := &self.memoryPressure
	if !state.activeSet {
		return 0
	}
	paid := addReceiveQueueByteCount(state.reservedByteCount, state.borrowedByteCount)
	if state.owner != nil {
		paid = addReceiveQueueByteCount(paid, state.owner.receivePack.ingressLoanByteCount)
	}
	return min(self.receiveMemoryPressurePrefixCreditWithLock(state.active.sequenceNumber), max(0, state.active.queueByteCount-paid))
}

// Root lock held. Until reclaimed roots return, even the exact target cannot
// spend the protected portion. Other ingress may consume only its surplus.
func (self *ReceiveSequence) receiveMemoryIngressPrefixCreditWithLock(messageId Id, number uint64) ByteCount {
	credit := self.receiveMemoryPrefixCreditWithLock(number)
	state := &self.memoryPressure
	if state.activeSet && (!state.ready || state.active.messageId != messageId || state.active.sequenceNumber != number) {
		credit -= self.receiveMemoryPressureProtectedPrefixWithLock()
	}
	return max(0, credit)
}

// Caller holds the shared root admission lock. Each live pending entry already
// has a paid owner envelope; duplicate lookup cannot allocate a history map.
func (self *ReceiveSequence) receiveIngressDuplicateWithLock(messageId Id, number uint64) bool {
	for pack := self.holdPromise.ingressHead; pack != nil; pack = pack.ingressNext {
		item := &pack.decodedOwner.receiveItem
		if item.messageId == messageId || item.sequenceNumber == number {
			return true
		}
	}
	return false
}

// Adds an already-paid owner. The immutable sequence pointer binds source,
// stream, session and sequence generation; closure never reuses that binding.
func (self *ReceiveSequence) registerReceiveIngressWithLock(pack *ReceivePack) {
	if pack.ingressSequence != nil {
		if pack.ingressSequence != self {
			panic("receive claim changed sequence generation")
		}
		return
	}
	pack.ingressSequence = self
	pack.ingressNext = self.holdPromise.ingressHead
	self.holdPromise.ingressHead = pack
}

// Allocates the owner only after the complete decode envelope is paid. Credit
// cannot be borrowed twice by pending duplicates, or from a closed generation.
func (self *ReceiveBuffer) takeReceiveMemoryOwner(sequence *ReceiveSequence, messageId Id, number uint64, byteCount ByteCount) *decodedPackOwner {
	budget := self.receiveBufferSettings.ReceiveQueueBudget
	if budget == nil || byteCount <= 0 {
		return nil
	}
	root := budget.admissionRoot()
	root.admissionLock.Lock()
	loan, pressureLoan := ByteCount(0), ByteCount(0)
	if sequence != nil && sequence.receiveQueue.budget == budget {
		promise := &sequence.holdPromise
		state := &sequence.memoryPressure
		if !promise.closed && sequence.ctx.Err() == nil && promise.ingressFloor <= number &&
			!sequence.receiveIngressDuplicateWithLock(messageId, number) {
			if state.activeSet && state.ready && state.owner == nil &&
				state.active.messageId == messageId && state.active.sequenceNumber == number {
				pressureLoan = min(byteCount, state.reservedByteCount)
			}
			if promise.set && number < promise.boundary {
				loan = min(byteCount-pressureLoan, sequence.receiveMemoryIngressPrefixCreditWithLock(messageId, number))
			}
		}
	}
	if remainder := byteCount - loan - pressureLoan; remainder > 0 && !budget.tryReserveWithLock(remainder) {
		root.admissionLock.Unlock()
		return nil
	}
	owner := inboundDecodedPackOwners.take()
	owner.receiveItem.transferItem = transferItem{messageId: messageId, sequenceNumber: number,
		memoryBudget: budget, queueByteCount: byteCount}
	pack := &owner.receivePack
	pack.decodedOwner = owner
	pack.receiveMemoryOwned = true
	if sequence != nil && sequence.receiveQueue.budget == budget {
		sequence.registerReceiveIngressWithLock(pack)
		state := &sequence.memoryPressure
		if pressureLoan != 0 || loan != 0 && state.activeSet && state.ready && state.owner == nil &&
			state.active.messageId == messageId && state.active.sequenceNumber == number {
			state.reservedByteCount -= pressureLoan
			state.owner, state.borrowedByteCount = owner, pressureLoan
		}
		if loan != 0 {
			sequence.holdPromise.reservedByteCount -= loan
			sequence.holdPromise.ingressLoanByteCount += loan
			pack.ingressLoanByteCount = loan
		}
	}
	root.admissionLock.Unlock()
	return owner
}

// A newly created sequence binds its first owner before queue publication.
// Its claim was already acquired in the RX tree, so no bytes move here.
func (self *ReceiveSequence) bindReceiveMemoryOwner(pack *ReceivePack) bool {
	if !pack.receiveMemoryOwned {
		return true
	}
	budget := pack.decodedOwner.receiveItem.memoryBudget
	recoveryOwner := budget != self.receiveQueue.budget
	if recoveryOwner {
		if budget == nil || budget != self.receiveBufferSettings.receiveHeadRecoveryBudget() ||
			pack.ingressSequence != self || pack.ingressLoanByteCount != 0 {
			return false
		}
	}
	root := budget.admissionRoot()
	root.admissionLock.Lock()
	defer root.admissionLock.Unlock()
	if self.holdPromise.closed || self.ctx.Err() != nil {
		return false
	}
	if recoveryOwner {
		if !receiveHeadRecoveryPack(pack.Pack) && !receiveHeadRecoveryOpeningPack(pack.Pack) &&
			self.heldHeadRepair.owner != pack.decodedOwner &&
			(self.client == nil || !self.client.reliableProviderIngress.Load() || !receiveProviderHeadRecoveryPack(pack.Pack)) {
			return false
		}
		registered := false
		for current := self.holdPromise.ingressHead; current != nil; current = current.ingressNext {
			if current == pack {
				registered = true
				break
			}
		}
		if !registered || pack != &pack.decodedOwner.receivePack || self.memoryPressure.owner == pack.decodedOwner ||
			self.memoryPressure.reclaimOwner == pack.decodedOwner {
			return false
		}
	}
	if pack.Pack != nil && pack.Pack.Nack {
		// NoAck has a physical owner, but is outside reliable-prefix identity
		// and cannot block a separately promised original at sequence zero.
		return pack.ingressSequence == nil && pack.ingressLoanByteCount == 0
	}
	if pack.ingressSequence != nil && pack.ingressSequence != self {
		return false
	}
	self.registerReceiveIngressWithLock(pack)
	return true
}

// Ordering/held membership now owns the original. Its claim remains intact;
// only the pending loan bookkeeping ends. The entry stays until real return so
// an in-flight or buffered duplicate cannot borrow the same prefix credit.
func (self *ReceiveSequence) adoptReceiveMemoryOwner(item *receiveItem) {
	if item.decodedOwner == nil || !item.decodedOwner.receivePack.receiveMemoryOwned {
		return
	}
	pack := &item.decodedOwner.receivePack
	if pack.Pack != nil && pack.Pack.Nack {
		return
	}
	root := item.memoryBudget.admissionRoot()
	root.admissionLock.Lock()
	if pack.ingressSequence != self {
		root.admissionLock.Unlock()
		panic("receive owner adopted by another generation")
	}
	self.holdPromise.ingressLoanByteCount -= pack.ingressLoanByteCount
	pack.ingressLoanByteCount = 0
	self.holdPromise.ingressFloor = self.nextSequenceNumber
	released := ByteCount(0)
	state := &self.memoryPressure
	if state.activeSet && state.active.messageId == item.messageId && state.active.sequenceNumber == item.sequenceNumber {
		released = self.clearActiveReceiveMemoryPressureWithLock()
	}
	root.admissionLock.Unlock()
	if released != 0 {
		item.memoryBudget.notifyCapacityChanged()
	}
}

// A real head advancement can retire an older outstanding loan association.
// Its decoded owner stays funded, but can no longer restore into the remaining
// prefix or a later promise. Publication precedes any following commitment.
func (self *ReceiveSequence) publishReceiveMemoryFloor() {
	budget := self.receiveQueue.budget
	if budget == nil {
		return
	}
	root := budget.admissionRoot()
	root.admissionLock.Lock()
	promise := &self.holdPromise
	promise.ingressFloor = self.nextSequenceNumber
	if self.receiveContract != nil && self.receiveContract.verifiedReceiveProofSet {
		promise.establishedContract = true
	}
	released := ByteCount(0)
	state := &self.memoryPressure
	if state.activeSet && state.active.sequenceNumber < promise.ingressFloor {
		released = self.clearActiveReceiveMemoryPressureWithLock()
	}
	if state.pendingSet && state.pending.sequenceNumber < promise.ingressFloor {
		state.pending, state.pendingSet = receiveMemoryPressureRequest{}, false
	}
	for pack := promise.ingressHead; pack != nil; pack = pack.ingressNext {
		if pack.ingressLoanByteCount != 0 && pack.decodedOwner.receiveItem.sequenceNumber < promise.ingressFloor {
			promise.ingressLoanByteCount -= pack.ingressLoanByteCount
			pack.ingressLoanByteCount = 0
		}
	}
	root.admissionLock.Unlock()
	if released != 0 {
		budget.notifyCapacityChanged()
	}
}

// Downstream prepayment grows the same claim before any SACK or receipt.
// A still-pending promised original consumes its remaining prepaid capacity
// first; a refusal changes neither the claim nor the promise.
func (self *receiveItem) growReceiveMemory(byteCount ByteCount) bool {
	budget := self.memoryBudget
	if budget == nil || byteCount <= 0 {
		return true
	}
	root := budget.admissionRoot()
	root.admissionLock.Lock()
	loan, pressureLoan := ByteCount(0), ByteCount(0)
	var pack *ReceivePack
	if self.decodedOwner != nil && self.decodedOwner.receivePack.receiveMemoryOwned {
		pack = &self.decodedOwner.receivePack
		if sequence := pack.ingressSequence; sequence != nil {
			// Consumer recovery owns a fixed whole claim. A provider head may
			// buy its distinct downstream custody only from that same child;
			// neither role may spend RX prefix or pressure credit here.
			if budget != sequence.receiveQueue.budget {
				if budget == sequence.receiveBufferSettings.receiveHeadRecoveryBudget() &&
					sequence.client != nil && sequence.client.reliableProviderIngress.Load() &&
					self.ack && !self.committed && !self.deliveryPrepared &&
					!sequence.holdPromise.closed && sequence.ctx.Err() == nil && pack.ingressLoanByteCount == 0 &&
					budget.totalByteCount.Load() <= sequence.receiveQueue.budget.totalByteCount.Load() &&
					receiveProviderHeadRecoveryFrames(self.frames) && budget.tryReserveWithLock(byteCount) {
					self.queueByteCount = addReceiveQueueByteCount(self.queueByteCount, byteCount)
					root.admissionLock.Unlock()
					return true
				}
				root.admissionLock.Unlock()
				return false
			}
			promise := &sequence.holdPromise
			state := &sequence.memoryPressure
			pressureOwner := state.activeSet && state.owner == self.decodedOwner && !promise.closed &&
				promise.ingressFloor <= self.sequenceNumber
			if pressureOwner {
				pressureLoan = min(byteCount, state.reservedByteCount)
			}
			if (pack.ingressLoanByteCount != 0 || pressureOwner) && !promise.closed && promise.set &&
				promise.ingressFloor <= self.sequenceNumber && self.sequenceNumber < promise.boundary {
				loan = min(byteCount-pressureLoan, sequence.receiveMemoryIngressPrefixCreditWithLock(self.messageId, self.sequenceNumber))
			}
		}
	}
	if remainder := byteCount - loan - pressureLoan; remainder > 0 && !budget.tryReserveWithLock(remainder) {
		root.admissionLock.Unlock()
		return false
	}
	if pressureLoan != 0 {
		state := &pack.ingressSequence.memoryPressure
		state.reservedByteCount -= pressureLoan
		state.borrowedByteCount += pressureLoan
	}
	if loan != 0 {
		promise := &pack.ingressSequence.holdPromise
		promise.reservedByteCount -= loan
		promise.ingressLoanByteCount += loan
		pack.ingressLoanByteCount += loan
	}
	self.queueByteCount = addReceiveQueueByteCount(self.queueByteCount, byteCount)
	root.admissionLock.Unlock()
	return true
}

// Returning roots precedes this release. A refused decode may return its exact
// still-live loan to the promise without opening a release/reacquire gap.
// Retirement explicitly revokes that association before a new promise starts.
func (self *decodedPackOwner) releaseReceiveMemory() {
	item := &self.receiveItem
	budget, byteCount := item.memoryBudget, item.queueByteCount
	if budget == nil || !self.receivePack.receiveMemoryOwned {
		return
	}
	pack := &self.receivePack
	root := budget.admissionRoot()
	root.admissionLock.Lock()
	// Registered prefix readers share this lock until actual owner unlink.
	item.memoryBudget = nil
	var pressureWake *ReceiveSequence
	if sequence := pack.ingressSequence; sequence != nil {
		byteCount = sequence.returnReceiveHeldHeadRepairWithLock(self, budget, byteCount)
		promise := &sequence.holdPromise
		previous := &promise.ingressHead
		for *previous != nil && *previous != pack {
			previous = &(*previous).ingressNext
		}
		if *previous == nil {
			root.admissionLock.Unlock()
			panic("paid receive owner missing from its generation")
		}
		*previous = pack.ingressNext
		loan := pack.ingressLoanByteCount
		if budget != sequence.receiveQueue.budget && (loan != 0 ||
			sequence.memoryPressure.owner == self || sequence.memoryPressure.reclaimOwner == self) {
			root.admissionLock.Unlock()
			panic("consumer recovery owner borrowed RX credit")
		}
		promise.ingressLoanByteCount -= loan
		if loan != 0 && !promise.closed && promise.set &&
			promise.ingressFloor <= item.sequenceNumber && item.sequenceNumber < promise.boundary {
			promise.reservedByteCount += loan
			byteCount -= loan
		}
		if budget == sequence.receiveQueue.budget {
			byteCount -= sequence.returnReceiveMemoryPressureWithLock(self, byteCount)
		}
		if sequence.memoryPressure.pendingSet {
			pressureWake = sequence
		}
		pack.ingressSequence, pack.ingressNext, pack.ingressLoanByteCount = nil, nil, 0
	}
	budget.releaseWithLock(byteCount)
	root.grantReserveWaitersWithLock()
	root.admissionLock.Unlock()
	if byteCount != 0 {
		budget.notifyCapacityChanged()
	}
	if pressureWake != nil {
		select {
		case pressureWake.closeNotify <- struct{}{}:
		default:
		}
	}
}

// Error paths may discard future work, but cannot return a physical claim
// before their caller returns the still-live decoded roots.
func (self *receiveItem) releaseMemory() {
	if self.decodedOwner != nil && self.decodedOwner.receivePack.receiveMemoryOwned {
		return
	}
	self.transferItem.releaseMemory()
}
