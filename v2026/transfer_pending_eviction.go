// Receiver withdrawals retain one marker per existing retry owner, never an
// append-only list of peer-supplied numbers. Queue state protects the markers;
// only Run changes recovery policy. No callback or write runs under that lock.
package connect

import (
	"container/heap"
	"time"
)

// One owning Run consumes one cut before taking another. Alternating bits
// separate ingress after the cut without an overflowing local generation.
type pendingEvictionBatch struct {
	sequenceId Id
	phase      uint8
}

// Current phase is read and toggled only under resendQueue.stateLock.
func (self *SendSequence) pendingEvictionPhaseWithLock() uint8 {
	if self.pendingEvictionSecondPhase {
		return 2
	}
	return 1
}

// Publishes the carrier's acceptance before its withdrawal becomes extractable.
// Released ACK carriers still carry valid state for other retained owners.
func (self *SendSequence) publishEvictionFeedback(window *sequenceAckWindow, ack receiveAckMessage) {
	if self.observeEvictions(ack) {
		window.Wake()
	}
}

// Exact current membership, not the carrier's retained identity, authorizes a
// marker. The one detached identity is still a live retry owner. Unknown,
// retired and future numbers retain no state; duplicate marks do not allocate.
func (self *SendSequence) observeEvictions(ack receiveAckMessage) bool {
	if ack.evictions == nil || len(ack.evictions.sequenceNumbers) == 0 {
		return false
	}
	changed := func() bool {
		queue := self.resendQueue
		queue.stateLock.Lock()
		defer queue.stateLock.Unlock()
		phase := self.pendingEvictionPhaseWithLock()
		changed := false
		for _, number := range ack.evictions.sequenceNumbers {
			if item := queue.sequenceNumberItems[number]; item != nil {
				if item.pendingEvictionPhases&phase == 0 {
					item.pendingEvictionPhases |= phase
					self.pendingEvictionPhases |= phase
					changed = true
				}
			} else if self.detachedAck.active && self.detachedAck.sequenceNumber == number {
				if self.detachedAck.pendingEvictionPhases&phase == 0 {
					self.detachedAck.pendingEvictionPhases |= phase
					self.pendingEvictionPhases |= phase
					changed = true
				}
			}
		}
		return changed
	}()
	// The common private same-carrier seam is outside the leaf queue lock.
	if changed && self.sendBuffer != nil && self.sendBuffer.afterEvictionMetadataForTest != nil {
		self.sendBuffer.afterEvictionMetadataForTest(self.id())
	}
	return changed
}

// Constant work on ordinary feedback, with no retained-flight scan. A nonempty
// cut owns one phase until applyPendingEvictions finishes or teardown clears it.
func (self *SendSequence) takePendingEvictions() pendingEvictionBatch {
	queue := self.resendQueue
	queue.stateLock.Lock()
	defer queue.stateLock.Unlock()
	phase := self.pendingEvictionPhaseWithLock()
	if self.pendingEvictionPhases&phase == 0 {
		return pendingEvictionBatch{}
	}
	if self.pendingEvictionInFlight != 0 {
		panic("another pending eviction cut is still owned")
	}
	self.pendingEvictionPhases &^= phase
	self.pendingEvictionInFlight = phase
	self.pendingEvictionSecondPhase = !self.pendingEvictionSecondPhase
	return pendingEvictionBatch{sequenceId: self.sequenceId, phase: phase}
}

// A genuine cut examines N current queued owners once and performs at most M
// heap repairs: O(N + M log N). The map membership never changes during this
// walk. All payload/callback/write ownership stays with Run and its queue.
func (self *SendSequence) applyPendingEvictions(batch pendingEvictionBatch) {
	if batch.phase == 0 {
		return
	}
	queue := self.resendQueue
	queue.stateLock.Lock()
	defer queue.stateLock.Unlock()
	if batch.sequenceId != self.sequenceId ||
		(batch.phase != 1 && batch.phase != 2) ||
		self.pendingEvictionInFlight != batch.phase {
		return
	}
	now := time.Now()
	for _, item := range queue.messageIdItems {
		if item.pendingEvictionPhases&batch.phase == 0 {
			continue
		}
		item.pendingEvictionPhases &^= batch.phase
		self.resendEvictedItemWithLock(item, now)
	}
	if self.detachedAck.active && self.detachedAck.pendingEvictionPhases&batch.phase != 0 {
		// A direct boundary caller may capture while its rewrite is detached.
		// Defer that exact owner's mark into the next phase, not a raw number
		// which could later refer to a different owner.
		self.detachedAck.pendingEvictionPhases &^= batch.phase
		phase := self.pendingEvictionPhaseWithLock()
		self.detachedAck.pendingEvictionPhases |= phase
		self.pendingEvictionPhases |= phase
	}
	self.pendingEvictionInFlight = 0
}

// The existing direct recovery entry shares the same continuous-membership
// reschedule. No byte-count churn, temporary ACK hole or pointer handoff.
func (self *SendSequence) resendEvicted(numbers []uint64) {
	if len(numbers) == 0 {
		return
	}
	queue := self.resendQueue
	queue.stateLock.Lock()
	defer queue.stateLock.Unlock()
	now := time.Now()
	for _, number := range numbers {
		self.resendEvictedItemWithLock(queue.sequenceNumberItems[number], now)
	}
}

// Selective withdrawal changes only recovery eligibility/time. sendTime and
// ACK expiry do not change, so the separate lifetime index remains valid.
func (self *SendSequence) resendEvictedItemWithLock(item *sendItem, now time.Time) {
	if item == nil || !item.selectiveAcked {
		return
	}
	item.selectiveAcked = false
	item.resendTime = now
	item.recoveryKind = sendRecoveryEviction
	heap.Fix(self.resendQueue, item.HeapIndex())
	heap.Fix(self.resendQueue.maxHeap, item.MaxHeapIndex())
}

// Cancellation has already joined all admitted ACK publishers before this
// teardown call. The fixed markers hold no independent owner or backing array.
func (self *SendSequence) clearPendingEvictions() {
	queue := self.resendQueue
	queue.stateLock.Lock()
	defer queue.stateLock.Unlock()
	for _, item := range queue.messageIdItems {
		item.pendingEvictionPhases = 0
	}
	self.detachedAck.pendingEvictionPhases = 0
	self.pendingEvictionPhases = 0
	self.pendingEvictionInFlight = 0
	self.pendingEvictionSecondPhase = false
}
