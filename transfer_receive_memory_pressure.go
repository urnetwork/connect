package connect

import (
	"crypto/sha256"

	"github.com/urnetwork/connect/protocol"
)

// A refused carrier contributes only copied scalar facts. Its bytes return to
// the caller immediately; the receive worker alone may replace tentative hold
// owners. One pending proposal and one funded retry are bounded per sequence.
type receiveMemoryPressureRequest struct {
	messageId        Id
	contractId       Id
	contractProof    [32]byte
	sequenceNumber   uint64
	queueByteCount   ByteCount
	messageByteCount ByteCount
	contractIdSet    bool
	contractFrameSet bool
	head             bool
	decrypted        bool
}

// The RX root lock guards this state. The worker validates and installs active;
// ingress can consume it only for its exact identity. A real owner keeps its
// borrowed credit associated until worker adoption or actual buffer return.
type receiveMemoryPressure struct {
	pending           receiveMemoryPressureRequest
	pendingSet        bool
	active            receiveMemoryPressureRequest
	activeSet         bool
	ready             bool
	reservedByteCount ByteCount
	owner             *decodedPackOwner
	borrowedByteCount ByteCount
	// Set only around a synchronous worker-owned tentative return. The release
	// path clears it before that owner's storage can reenter its pool.
	reclaimOwner *decodedPackOwner
}

func (self *ReceiveSequence) requestReceiveMemoryPressure(scan receiveMemoryPackScan, decrypted bool) {
	if !scan.loanSafe || scan.compatibility || scan.nack || scan.contractAhead ||
		scan.hasEncryptedControl || scan.queueCostInvalid() ||
		self.receiveBufferSettings.ReceiveHoldPolicy != ReceiveHoldCommittedPrefix {
		return
	}
	budget := self.receiveQueue.budget
	if budget == nil || !self.receiveQueue.lifetimeBudget {
		return
	}
	request := receiveMemoryPressureRequest{messageId: scan.messageId, contractId: scan.contractId,
		sequenceNumber: scan.sequenceNumber, queueByteCount: scan.retainedByteCount,
		messageByteCount: scan.messageByteCount, contractIdSet: scan.contractIdSet, head: scan.head,
		contractFrameSet: scan.contractFrameSet, contractProof: scan.contractProof, decrypted: decrypted}
	root := budget.admissionRoot()
	root.admissionLock.Lock()
	state := &self.memoryPressure
	if self.holdPromise.closed || self.ctx.Err() != nil || request.sequenceNumber < self.holdPromise.ingressFloor ||
		self.receiveIngressDuplicateWithLock(request.messageId, request.sequenceNumber) ||
		state.pendingSet && state.pending.sequenceNumber <= request.sequenceNumber ||
		state.activeSet && (state.active.sequenceNumber < request.sequenceNumber ||
			state.active.sequenceNumber == request.sequenceNumber && (state.active.messageId != request.messageId ||
				request.queueByteCount <= state.active.queueByteCount)) {
		root.admissionLock.Unlock()
		return
	}
	state.pending, state.pendingSet = request, true
	root.admissionLock.Unlock()
	// Reuse the existing worker-control edge. An empty close request does not
	// close a sequence, and the pressure handler never extends a gap deadline.
	select {
	case self.closeNotify <- struct{}{}:
	default:
	}
}

// The receive worker may discover additional downstream custody after a
// pressure-backed retry has paid for decoding. Preserve the actual retry's
// authority, not the old proposal's proof, because contract registration has
// not run yet. Its owner must return normally before the worker can reclaim
// any further tentative capacity for this larger exact request.
func (self *ReceiveSequence) requestReceiveMemoryGrowth(item *receiveItem, additional ByteCount) {
	if item == nil || additional <= 0 || !item.ack || item.contractAhead ||
		item.decodedOwner == nil || self.receiveQueue == nil || !self.receiveQueue.lifetimeBudget ||
		self.receiveBufferSettings.ReceiveHoldPolicy != ReceiveHoldCommittedPrefix {
		return
	}
	// Encrypted control has its own admission path. A mixed application retry
	// must not use downstream growth to widen ordinary pressure's authority.
	for _, frame := range item.frames {
		if frame != nil && frame.MessageType == protocol.MessageType_TransferEncryptedControl {
			return
		}
	}
	owner := item.decodedOwner
	pack := &owner.receivePack
	budget := self.receiveQueue.budget
	if budget == nil || item.memoryBudget != budget || item != &owner.receiveItem ||
		!pack.receiveMemoryOwned || pack.ingressSequence != self || pack.Pack == nil || pack.Pack.Nack {
		return
	}
	request := receiveMemoryPressureRequest{
		messageId: item.messageId, sequenceNumber: item.sequenceNumber,
		queueByteCount:   addReceiveQueueByteCount(item.QueueByteCount(), additional),
		messageByteCount: item.MessageByteCount(), head: item.head, decrypted: !item.unwrapped,
	}
	if request.queueByteCount <= item.QueueByteCount() {
		return
	}
	if item.contractId != nil {
		request.contractIdSet, request.contractId = true, *item.contractId
	}
	if item.contractFrame != nil {
		request.contractFrameSet = true
		request.contractProof = sha256.Sum256(item.contractFrame.MessageBytes)
	}
	if !self.receiveMemoryPressureAuthorized(request) {
		return
	}
	root := budget.admissionRoot()
	root.admissionLock.Lock()
	state := &self.memoryPressure
	if self.holdPromise.closed || self.ctx.Err() != nil ||
		request.sequenceNumber < self.holdPromise.ingressFloor {
		root.admissionLock.Unlock()
		return
	}
	if state.activeSet {
		if state.owner != owner || state.borrowedByteCount <= 0 && pack.ingressLoanByteCount <= 0 ||
			state.active.messageId != request.messageId || state.active.sequenceNumber != request.sequenceNumber ||
			request.queueByteCount <= state.active.queueByteCount ||
			state.pendingSet && (state.pending.sequenceNumber < request.sequenceNumber ||
				state.pending.sequenceNumber == request.sequenceNumber &&
					(state.pending.messageId != request.messageId || state.pending.queueByteCount >= request.queueByteCount)) {
			root.admissionLock.Unlock()
			return
		}
	} else {
		// A fresh decode can fit every time while its downstream claim never
		// fits. Associate its already-paid roots with this exact retry before
		// their normal return, so recovery does not require a decode refusal.
		// Never replace another proposal or borrow a reservation twice.
		if pack.ingressLoanByteCount < 0 || item.QueueByteCount() < pack.ingressLoanByteCount ||
			state.owner != nil || state.reservedByteCount != 0 || state.borrowedByteCount != 0 ||
			state.reclaimOwner != nil || state.pendingSet {
			root.admissionLock.Unlock()
			return
		}
		current := request
		current.queueByteCount = item.QueueByteCount()
		state.active, state.activeSet, state.ready = current, true, false
		// The existing prefix loan returns to its promise. Only the fresh
		// remainder may also return to this pressure reservation.
		state.owner, state.borrowedByteCount = owner, item.QueueByteCount()-pack.ingressLoanByteCount
	}
	state.pending, state.pendingSet = request, true
	root.admissionLock.Unlock()
	select {
	case self.closeNotify <- struct{}{}:
	default:
	}
}

func (self receiveMemoryPackScan) queueCostInvalid() bool {
	return self.retainedByteCount <= 0 || self.messageByteCount < 0
}

func (self *ReceiveSequence) receiveMemoryPressurePending() bool {
	budget := self.receiveQueue.budget
	if budget == nil {
		return false
	}
	root := budget.admissionRoot()
	root.admissionLock.Lock()
	pending := self.memoryPressure.pendingSet
	root.admissionLock.Unlock()
	return pending
}

// Only the receive worker reads contract/order/held state. A request cannot
// introduce a proof, successor, or future Head reset. A known contract has
// already passed MAC/path checks; the explicit no-contract policy is separate.
func (self *ReceiveSequence) receiveMemoryPressureAuthorized(request receiveMemoryPressureRequest) bool {
	if self.ctx.Err() != nil || !request.decrypted && self.session != nil && self.session.RequireEncryption() ||
		request.sequenceNumber < self.nextSequenceNumber ||
		request.head && request.sequenceNumber != self.nextSequenceNumber ||
		self.receiveQueue.GetByMessageId(request.messageId) != nil ||
		self.receiveQueue.GetBySequenceNumber(request.sequenceNumber) != nil {
		return false
	}
	contract := self.receiveContract
	if request.contractIdSet {
		contract = self.openReceiveContracts[request.contractId]
	}
	if request.contractFrameSet {
		// Compare only with previously authenticated proof bytes. New, corrupt,
		// or alternate encodings need ordinary funded verification first.
		return contract != nil && !contract.expired() && contract.verifiedReceiveProofSet &&
			contract.verifiedReceiveProof == request.contractProof
	}
	if request.contractIdSet {
		return contract != nil && !contract.expired()
	}
	if contract != nil && !contract.expired() {
		return true
	}
	return self.client.ContractManager().ReceiveNoContract(self.source.SourceId)
}

// Called only on the sequence's Run goroutine. Plan the complete replacement
// before removing anything: an oversized request cannot discard a partial hold
// and still fail funding. Committed/SACKed owners are never candidates.
func (self *ReceiveSequence) applyReceiveMemoryPressure() {
	budget := self.receiveQueue.budget
	if budget == nil || !self.receiveQueue.lifetimeBudget {
		return
	}
	root := budget.admissionRoot()
	root.admissionLock.Lock()
	state := &self.memoryPressure
	if !state.pendingSet || state.owner != nil || self.holdPromise.closed {
		root.admissionLock.Unlock()
		return
	}
	request := state.pending
	state.pending, state.pendingSet = receiveMemoryPressureRequest{}, false
	credit := addReceiveQueueByteCount(state.reservedByteCount, self.receiveMemoryPressurePrefixCreditWithLock(request.sequenceNumber))
	root.admissionLock.Unlock()
	if !self.receiveMemoryPressureAuthorized(request) {
		return
	}

	// The worker is the only heap mutator. This reusable slice only borrows
	// held pointers until this synchronous decision/return batch finishes.
	defer func() { clear(self.heldScratch); self.heldScratch = self.heldScratch[:0] }()
	self.heldScratch = self.receiveQueue.AscendingItems(self.heldScratch)
	_, heldBytes := self.receiveQueue.QueueSize()
	removeCount := 0
	reclaimByteCount := ByteCount(0)
	fits := func() bool {
		return request.queueByteCount <= credit && (request.sequenceNumber == self.nextSequenceNumber ||
			transferQueueWindowAdmits(heldBytes, request.messageByteCount, self.receiveBufferSettings.ReceiveQueueMaxByteCount))
	}
	for index := len(self.heldScratch) - 1; !fits() && index >= 0; index-- {
		item := self.heldScratch[index]
		if item.sequenceNumber <= request.sequenceNumber || item.committed || !item.ack ||
			item.memoryBudget != budget || item.decodedOwner == nil ||
			!item.decodedOwner.receivePack.receiveMemoryOwned {
			break
		}
		credit = addReceiveQueueByteCount(credit, item.QueueByteCount())
		reclaimByteCount = addReceiveQueueByteCount(reclaimByteCount, item.QueueByteCount())
		heldBytes -= item.MessageByteCount()
		removeCount++
	}
	if !fits() {
		return
	}

	root.admissionLock.Lock()
	// Ingress may have taken the previous exact credit while this worker
	// examined its heap. Keep the earlier proposal for the adoption boundary.
	if state.owner != nil {
		if !state.pendingSet || request.sequenceNumber < state.pending.sequenceNumber {
			state.pending, state.pendingSet = request, true
		}
		root.admissionLock.Unlock()
		return
	}
	if self.holdPromise.closed || self.ctx.Err() != nil ||
		self.receiveIngressDuplicateWithLock(request.messageId, request.sequenceNumber) {
		root.admissionLock.Unlock()
		return
	}
	// Another ingress may have borrowed unprotected prefix credit during the
	// heap scan. Revalidate before publishing the protection or removing a tail.
	currentCredit := addReceiveQueueByteCount(state.reservedByteCount, self.receiveMemoryPressurePrefixCreditWithLock(request.sequenceNumber))
	if addReceiveQueueByteCount(currentCredit, reclaimByteCount) < request.queueByteCount {
		if !state.pendingSet || request.sequenceNumber < state.pending.sequenceNumber {
			state.pending, state.pendingSet = request, true
		}
		root.admissionLock.Unlock()
		return
	}
	state.active, state.activeSet, state.ready = request, true, false
	root.admissionLock.Unlock()
	for index := len(self.heldScratch) - 1; removeCount > 0; index-- {
		item := self.heldScratch[index]
		self.receiveQueue.RemoveByMessageId(item.messageId)
		self.client.receiveQueueTentativeEvictionCount.Add(1)
		self.client.receiveQueueTentativeEvictionByteCount.Add(uint64(max(item.MessageByteCount(), 0)))
		root.admissionLock.Lock()
		state.reclaimOwner = item.decodedOwner
		root.admissionLock.Unlock()
		item.messagePoolReturn()
		removeCount--
	}
	root.admissionLock.Lock()
	if state.reclaimOwner != nil {
		root.admissionLock.Unlock()
		panic("tentative owner returned without pressure settlement")
	}
	state.ready = true
	root.admissionLock.Unlock()
}

// Root lock held. This leaves an in-flight owner's physical claim untouched;
// revocation only prevents its later return from restoring obsolete credit.
func (self *ReceiveSequence) clearReceiveMemoryPressureWithLock() ByteCount {
	state := &self.memoryPressure
	if state.reclaimOwner != nil {
		panic("pressure revoked during tentative return")
	}
	released := state.reservedByteCount
	*state = receiveMemoryPressure{}
	if released != 0 {
		budget := self.receiveQueue.budget
		budget.releaseWithLock(released)
		budget.admissionRoot().grantReserveWaitersWithLock()
	}
	return released
}

func (self *ReceiveSequence) clearActiveReceiveMemoryPressureWithLock() ByteCount {
	pending, set := self.memoryPressure.pending, self.memoryPressure.pendingSet
	released := self.clearReceiveMemoryPressureWithLock()
	self.memoryPressure.pending, self.memoryPressure.pendingSet = pending, set
	return released
}

// Root lock held, after the old owner's real roots have returned. Escrow is
// an existing paid reservation, never a release followed by a fresh acquire.
func (self *ReceiveSequence) returnReceiveMemoryPressureWithLock(owner *decodedPackOwner, byteCount ByteCount) ByteCount {
	state := &self.memoryPressure
	retained := ByteCount(0)
	if state.reclaimOwner == owner {
		state.reclaimOwner = nil
		if state.activeSet && !self.holdPromise.closed {
			prefixCredit := self.receiveMemoryPressurePrefixCreditWithLock(state.active.sequenceNumber)
			retained = min(byteCount, max(0, state.active.queueByteCount-state.reservedByteCount-prefixCredit))
			state.reservedByteCount += retained
		}
	}
	if state.owner == owner {
		if state.activeSet && !self.holdPromise.closed && self.holdPromise.ingressFloor <= state.active.sequenceNumber {
			restored := min(byteCount-retained, state.borrowedByteCount)
			state.reservedByteCount += restored
			retained += restored
		}
		state.owner, state.borrowedByteCount = nil, 0
		// A larger request can lack a reclaimable tail. The actual returned
		// decode remains usable when only its downstream deficit later frees.
		// This is not the reclaim branch: its batch alone publishes readiness.
		if state.activeSet && !self.holdPromise.closed && self.holdPromise.ingressFloor <= state.active.sequenceNumber &&
			state.active.queueByteCount <= addReceiveQueueByteCount(
				state.reservedByteCount, self.receiveMemoryPrefixCreditWithLock(state.active.sequenceNumber)) {
			state.ready = true
		}
	}
	return retained
}
