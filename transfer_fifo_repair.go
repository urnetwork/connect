// An exact repaired-prefix acknowledgement proves passage of a captured
// retained cohort. Each later cumulative advance exposes at most one repair.
package connect

import "time"

// Only the send worker owns these scalar records and the two item flags. A
// pending promoted envelope cannot erase an already accepted cohort. Neither
// record retains a pooled item; all membership stays on existing paid owners.
type fifoRepairState struct {
	maxItems             int
	generation           uint64
	generationStartNanos int64
	pending              fifoRepairFence
	passed               fifoPassedHead
}

// The creation fence belongs to this exact tag, including repeated writes of
// that envelope. Later successful copies leave their captured cohort.
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

// The exact head marker is consumed by its next successful write or delivery.
// The route remains while other accepted owners await cumulative advancement.
type fifoPassedHead struct {
	messageId Id
	number    uint64
	route     Route
	set       bool
}

// Captured owners cannot move out of this prefix: the worker only appends new
// originals and retires older positions. This also bounds flag clearing after
// generation changes, including a queue containing arbitrarily many NoBudget
// originals. The settings object is immutable for the worker's lifetime.
func (self *SendSequence) fifoRepairItems() []*sendItem {
	if self.fifoRepair.maxItems == 0 {
		// Production snapshots this before publication. Inert worker fixtures
		// initialize lazily under the same exclusive worker ownership.
		self.fifoRepair.maxItems = max(2, self.sendBufferSettings.FifoRepairMaxItems)
	}
	return self.sendItems[:min(len(self.sendItems), self.fifoRepair.maxItems)]
}

// A route channel can be reused by another FIFO. Originals older than this
// conservative monotonic epoch floor cannot establish physical order in it.
func (self *SendSequence) resetFifoRepairGeneration(generation uint64) {
	for _, item := range self.fifoRepairItems() {
		if item != nil {
			item.fifoRepairPending, item.fifoRepairAccepted = false, false
		}
	}
	self.fifoRepair = fifoRepairState{generation: generation, maxItems: self.fifoRepair.maxItems}
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

// Capture only a current head's distinct promoted envelope. The immediate
// successor establishes the existing conservative route premise; other paid
// originals already written on that route join the same cohort. Initial-write
// time establishes the lower bound, while per-owner flags reject later copies.
func (self *SendSequence) prepareFifoRepair(item *sendItem, original, promoted sequenceTag, forceFullContract bool) {
	for _, retained := range self.fifoRepairItems() {
		if retained != nil {
			retained.fifoRepairPending = false
		}
	}
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
		if self.resendQueue.messageIdItems[next.messageId] != next ||
			self.resendQueue.sequenceNumberItems[next.sequenceNumber] != next || !next.rttH1 ||
			next.pacingSentAtNanos < self.fifoRepair.generationStartNanos || createdNanos <= next.pacingSentAtNanos {
			return false
		}
		for _, retained := range self.fifoRepairItems()[1:] {
			// Expansion covers DATA only. The original immediate-successor
			// proof retains its prior protocol compatibility boundary.
			if retained != nil && (retained == next || !retained.noBudget) && fifoRepairItemOnRoute(retained, item.carrierRoute) &&
				!retained.selectiveAcked && !retained.deliveryObserved && retained.recoveryKind == sendRecoveryNone &&
				retained.sequenceNumber > item.sequenceNumber && self.resendQueue.messageIdItems[retained.messageId] == retained &&
				self.resendQueue.sequenceNumberItems[retained.sequenceNumber] == retained && retained.rttH1 &&
				self.fifoRepair.generationStartNanos <= retained.pacingSentAtNanos && retained.pacingSentAtNanos < createdNanos {
				retained.fifoRepairPending = true
			}
		}
		return true
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
	item.fifoRepairPending, item.fifoRepairAccepted = false, false
	if state.passed.set && state.passed.messageId == item.messageId && state.passed.number == item.sequenceNumber {
		state.passed = fifoPassedHead{route: state.passed.route}
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

// Accept a cohort only from the exact explicitly acknowledged promoted copy.
// Later cumulative advances create no evidence: they expose at most one owner
// already marked by that proof. Proven absence also supersedes an untouched
// initial deadline, while all existing lifetime and physical service gates stay.
func (self *SendSequence) applyFifoRepair(proof fifoRepairFence, at time.Time) {
	if len(self.sendItems) == 0 {
		self.fifoRepair.passed = fifoPassedHead{}
		return
	}
	policy, eligible := self.fifoRepairPolicy()
	if !eligible {
		return
	}
	item := self.sendItems[0]
	eligibleHead := func() bool {
		self.resendQueue.stateLock.Lock()
		defer self.resendQueue.stateLock.Unlock()
		if proof.set && proof.successful && proof.generation == policy.generation &&
			item.messageId == proof.nextMessageId && item.sequenceNumber == proof.nextNumber {
			changedRoute := self.fifoRepair.passed.route != nil && self.fifoRepair.passed.route != proof.route
			for _, retained := range self.fifoRepairItems() {
				if retained == nil {
					continue
				}
				if changedRoute {
					retained.fifoRepairAccepted = false
				}
				if retained.fifoRepairPending && fifoRepairItemOnRoute(retained, proof.route) &&
					!retained.selectiveAcked && !retained.deliveryObserved && retained.recoveryKind == sendRecoveryNone &&
					self.resendQueue.messageIdItems[retained.messageId] == retained &&
					self.resendQueue.sequenceNumberItems[retained.sequenceNumber] == retained {
					retained.fifoRepairAccepted = true
				}
				retained.fifoRepairPending = false
			}
			self.fifoRepair.passed = fifoPassedHead{route: proof.route}
		}
		route := self.fifoRepair.passed.route
		return item.fifoRepairAccepted && fifoRepairItemOnRoute(item, route) &&
			!item.selectiveAcked && !item.deliveryObserved && item.recoveryKind == sendRecoveryNone &&
			self.resendQueue.messageIdItems[item.messageId] == item && self.resendQueue.sequenceNumberItems[item.sequenceNumber] == item
	}()
	if !eligibleHead {
		return
	}
	route := self.fifoRepair.passed.route
	self.fifoRepair.passed = fifoPassedHead{messageId: item.messageId, number: item.sequenceNumber, route: route, set: true}
	if at.Before(item.resendTime) && self.setResendTimeIfRetained(item, at) {
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
	item.fifoRepairPending, item.fifoRepairAccepted = false, false
	if state.pending.set && state.pending.prefixMessageId == item.messageId && state.pending.prefixNumber == item.sequenceNumber {
		state.pending = fifoRepairFence{}
	}
	if state.passed.set && state.passed.messageId == item.messageId && state.passed.number == item.sequenceNumber {
		state.passed = fifoPassedHead{route: state.passed.route}
	}
}
