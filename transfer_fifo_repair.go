// Exact repaired-prefix acknowledgements can disprove queue residence for
// one successor without changing recovery clocks or growing retained items.
package connect

import "time"

// Only the send worker owns these scalar records. A pending promoted envelope
// and an accepted successor are independent: a failed later promotion cannot
// erase an already accepted proof. Neither record retains a pooled item.
type fifoRepairState struct {
	generation           uint64
	generationStartNanos int64
	pending              fifoRepairFence
	passed               fifoPassedHead
}

// The creation fence belongs to this exact tag, including repeated writes of
// that envelope. A later successful write of the named successor cancels it.
type fifoRepairFence struct {
	prefixMessageId Id
	nextMessageId   Id
	prefixNumber    uint64
	nextNumber      uint64
	tagSendTime     uint64
	createdNanos    int64
	generation      uint64
	route           Route
	set             bool
	successful      bool
}

// An accepted proof lasts only until this exact head is delivered, written
// again, or loses its unchanged FIFO generation.
type fifoPassedHead struct {
	messageId Id
	number    uint64
	route     Route
	set       bool
}

// A route channel can be reused by another FIFO. Originals older than this
// conservative monotonic epoch floor cannot establish physical order in it.
func (self *SendSequence) resetFifoRepairGeneration(generation uint64) {
	self.fifoRepair = fifoRepairState{generation: generation}
	if self.client != nil && !self.client.feedbackTimeBase.IsZero() {
		self.fifoRepair.generationStartNanos = self.client.feedbackArrivalNanos(time.Now())
	}
}

// Reading policy also invalidates stale evidence when a generation changes
// during a physical write, before the outer worker next applies its snapshot.
func (self *SendSequence) fifoRepairPolicy() (transferFlightPolicySnapshot, bool) {
	policy := self.transferFlightPolicy()
	if self.fifoRepair.generation != policy.generation || self.fifoRepair.generationStartNanos == 0 {
		self.resetFifoRepairGeneration(policy.generation)
	}
	return policy, self.fifoRepair.generationStartNanos != 0 && policy.h1Only &&
		self.sendBufferSettings.DeliverySizedWindowScale > 0 && !self.sendBufferSettings.disableWindowPacingForTest
}

// Ambiguous, mixed and changed carriers cannot establish FIFO passage.
func fifoRepairItemOnRoute(item *sendItem, route Route) bool {
	return item != nil && route != nil && item.carrierRoute == route && item.expectsAck &&
		item.reliableCarrierObserved && !item.unreliableCarrierObserved &&
		!item.hybridReliableCarrierObserved && !item.carrierChanged
}

// Capture only the current head's new promoted envelope and its exact next
// retained item. Tag order distinguishes the original envelope; physical
// order uses the client's monotonic clock, never the tag's wall-clock value.
func (self *SendSequence) prepareFifoRepair(item *sendItem, original, promoted sequenceTag, forceFullContract bool) {
	self.fifoRepair.pending = fifoRepairFence{}
	if forceFullContract || item.head || !original.set || original.sendTime == 0 ||
		!promoted.set || promoted.sendTime <= original.sendTime || len(self.sendItems) < 2 || self.sendItems[0] != item {
		return
	}
	policy, eligible := self.fifoRepairPolicy()
	next := self.sendItems[1]
	if !eligible || !fifoRepairItemOnRoute(item, item.carrierRoute) ||
		!fifoRepairItemOnRoute(next, item.carrierRoute) || next.selectiveAcked || next.deliveryObserved ||
		next.recoveryKind != sendRecoveryNone || next.sequenceNumber <= item.sequenceNumber {
		return
	}
	createdNanos := self.client.feedbackArrivalNanos(time.Now())
	confirmed := func() bool {
		self.resendQueue.stateLock.Lock()
		defer self.resendQueue.stateLock.Unlock()
		// Earlier copies make RTT sampling ambiguous, but precede this
		// owner-created fence. Every later successful copy invalidates it.
		return self.resendQueue.messageIdItems[next.messageId] == next && next.rttH1 &&
			self.fifoRepair.generationStartNanos <= next.pacingSentAtNanos && next.pacingSentAtNanos < createdNanos
	}()
	if !confirmed {
		return
	}
	self.fifoRepair.pending = fifoRepairFence{
		prefixMessageId: item.messageId, prefixNumber: item.sequenceNumber,
		nextMessageId: next.messageId, nextNumber: next.sequenceNumber,
		tagSendTime: promoted.sendTime, createdNanos: createdNanos,
		generation: policy.generation, route: item.carrierRoute, set: true,
	}
}

// Called only after an actual successful physical write. Explicit recovery
// copies also pass here even when their ordinary send counter stays unchanged.
func (self *SendSequence) observeFifoRepairWrite(item *sendItem, disposition transferWriteDisposition) {
	state := &self.fifoRepair
	if state.passed.set && state.passed.messageId == item.messageId && state.passed.number == item.sequenceNumber {
		state.passed = fifoPassedHead{}
	}
	if state.pending.set && state.pending.nextMessageId == item.messageId && state.pending.nextNumber == item.sequenceNumber {
		state.pending = fifoRepairFence{}
	}
	if !state.pending.set || state.pending.prefixMessageId != item.messageId || state.pending.prefixNumber != item.sequenceNumber {
		return
	}
	policy, eligible := self.fifoRepairPolicy()
	if !eligible || !state.pending.set || state.pending.generation != policy.generation ||
		disposition.transportType != TransportTypeH1 || !disposition.reliable || disposition.unreliable ||
		!fifoRepairItemOnRoute(item, state.pending.route) || disposition.route != state.pending.route {
		state.pending = fifoRepairFence{}
		return
	}
	state.pending.successful = true
}

// Snapshot before cumulative retirement can return the named item to the
// process-wide pool. An implicit prefix item or legacy tag supplies no proof.
func (self *SendSequence) acknowledgedFifoRepair(item *sendItem, tag sequenceTag) fifoRepairFence {
	if !self.fifoRepair.pending.set || !tag.set || tag.sendTime == 0 {
		return fifoRepairFence{}
	}
	policy, eligible := self.fifoRepairPolicy()
	proof := self.fifoRepair.pending
	if !eligible || !proof.set || !proof.successful || proof.generation != policy.generation ||
		proof.prefixMessageId != item.messageId || proof.prefixNumber != item.sequenceNumber ||
		proof.tagSendTime != tag.sendTime || !fifoRepairItemOnRoute(item, proof.route) {
		return fifoRepairFence{}
	}
	return proof
}

// Accept only the captured successor after prefix retirement. Untouched RTOs
// keep their clock; only a future queue-residence deferral is already disproved.
func (self *SendSequence) applyFifoRepair(proof fifoRepairFence, at time.Time) {
	if !proof.set || len(self.sendItems) == 0 {
		return
	}
	policy, eligible := self.fifoRepairPolicy()
	item := self.sendItems[0]
	if !eligible || proof.generation != policy.generation || item.messageId != proof.nextMessageId ||
		item.sequenceNumber != proof.nextNumber || !fifoRepairItemOnRoute(item, proof.route) ||
		item.selectiveAcked || item.deliveryObserved || item.recoveryKind != sendRecoveryNone ||
		self.resendQueue.GetByMessageId(item.messageId) != item {
		return
	}
	self.fifoRepair.passed = fifoPassedHead{messageId: item.messageId, number: item.sequenceNumber, route: proof.route, set: true}
	if item.deferralOutstanding && at.Before(item.resendTime) && self.setResendTimeIfRetained(item, at) {
		item.deferralOutstanding = false
	}
}

// The item may be detached from the retry heap while ordinary policy decides
// its next action, but it must remain the exact logical retained head.
func (self *SendSequence) fifoRepairPassed(item *sendItem) bool {
	passed := self.fifoRepair.passed
	if !passed.set || item == nil || passed.messageId != item.messageId || passed.number != item.sequenceNumber {
		return false
	}
	_, eligible := self.fifoRepairPolicy()
	return eligible && self.fifoRepair.passed.set && len(self.sendItems) > 0 && self.sendItems[0] == item &&
		fifoRepairItemOnRoute(item, passed.route) && !item.selectiveAcked && !item.deliveryObserved && item.recoveryKind == sendRecoveryNone
}

// Forget identities before delivery or pooling. No pointer outlives its owner.
func (self *SendSequence) forgetFifoRepairItem(item *sendItem) {
	state := &self.fifoRepair
	if state.pending.set && ((state.pending.prefixMessageId == item.messageId && state.pending.prefixNumber == item.sequenceNumber) ||
		(state.pending.nextMessageId == item.messageId && state.pending.nextNumber == item.sequenceNumber)) {
		state.pending = fifoRepairFence{}
	}
	if state.passed.set && state.passed.messageId == item.messageId && state.passed.number == item.sequenceNumber {
		state.passed = fifoPassedHead{}
	}
}
