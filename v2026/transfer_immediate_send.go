package connect

// Exact terminal receipts retire one opted-in original without acknowledging a
// predecessor. Compressed ordinal ranges preserve later cumulative anchors.
import (
	"slices"
	"time"
)

const immediateTerminalRangeLimit = 32

// A range contains only contiguous, actually secured immediate ordinals.
// It retains no payload, callback, pooled item or route pointer.
type immediateOrdinalRange struct {
	first uint64
	last  uint64
}

// One sorted union operation is bounded by the fixed range limit. Sparse
// saturation preserves every prior identity and refuses the new terminal claim.
func addImmediateOrdinalRange(ranges []immediateOrdinalRange, number uint64) ([]immediateOrdinalRange, bool) {
	index := 0
	for index < len(ranges) && ranges[index].last < number && number-ranges[index].last > 1 {
		index++
	}
	if index < len(ranges) && (ranges[index].first <= number || ranges[index].first-number <= 1) {
		ranges[index].first = min(ranges[index].first, number)
		ranges[index].last = max(ranges[index].last, number)
		end := index + 1
		for end < len(ranges) && (ranges[end].first <= ranges[index].last || ranges[end].first-ranges[index].last <= 1) {
			ranges[index].last = max(ranges[index].last, ranges[end].last)
			end++
		}
		if end > index+1 {
			ranges = slices.Delete(ranges, index+1, end)
		}
		return ranges, true
	}
	if len(ranges) >= immediateTerminalRangeLimit {
		return ranges, false
	}
	if ranges == nil {
		ranges = make([]immediateOrdinalRange, 0, immediateTerminalRangeLimit)
	}
	ranges = append(ranges, immediateOrdinalRange{})
	copy(ranges[index+1:], ranges[index:len(ranges)-1])
	ranges[index] = immediateOrdinalRange{first: number, last: number}
	return ranges, true
}

// Derived identity is useful only inside this exact sender generation and its
// actually secured ranges. The caller holds resendQueue.stateLock.
func (self *SendSequence) resolveImmediateAckSequenceNumberWithLock(messageId Id, number uint64, numberSet bool) (uint64, bool) {
	if !numberSet || self.client == nil || messageId != immediateMessageId(self.client.ClientId(), self.sequenceId, number) {
		return 0, false
	}
	for _, interval := range self.immediateTerminals {
		if number < interval.first {
			break
		}
		if number <= interval.last {
			return number, true
		}
	}
	return 0, false
}

// Only a cumulative ACK may recover a terminal original's removed boundary.
// A contract request or unrelated selective ACK cannot borrow these ranges.
func (self *SendSequence) resolveImmediateAckSequenceNumber(ack receiveAckMessage) (uint64, bool) {
	if ack.selective || ack.deliveredImmediately || ack.contractMissing || ack.sequenceId != self.sequenceId || self.resendQueue == nil {
		return 0, false
	}
	self.resendQueue.stateLock.Lock()
	defer self.resendQueue.stateLock.Unlock()
	return self.resolveImmediateAckSequenceNumberWithLock(ack.messageId, ack.immediateSequenceNumber, ack.immediateSequenceNumberSet)
}

// The send worker is the sole item/list mutator. Publishing the range and
// removing the original share the existing queue lock, so ACK ingress cannot
// observe an identity gap. False asks the caller to retain an ordinary SACK;
// malformed, stale and duplicate terminal receipts are consumed without effect.
func (self *SendSequence) receiveImmediateAck(ack sequenceAck) bool {
	if !ack.deliveredImmediately {
		return false
	}
	if !ack.selective || ack.contractMissing || !ack.immediateSequenceNumberSet || self.resendQueue == nil || self.client == nil {
		return true
	}
	queue := self.resendQueue
	queue.stateLock.Lock()
	item := queue.messageIdItems[ack.messageId]
	if item == nil || !item.expectsAck || !item.deliverImmediately ||
		item.sequenceNumber != ack.sequenceNumber || item.sequenceNumber != ack.immediateSequenceNumber ||
		item.messageId != immediateMessageId(self.client.ClientId(), self.sequenceId, item.sequenceNumber) {
		queue.stateLock.Unlock()
		return true
	}
	index := slices.Index(self.sendItems, item)
	if index < 0 {
		queue.stateLock.Unlock()
		panic("immediate receipt without retained send ownership")
	}
	ranges, accepted := addImmediateOrdinalRange(self.immediateTerminals, item.sequenceNumber)
	if !accepted {
		queue.stateLock.Unlock()
		return false
	}
	self.immediateTerminals = ranges
	if queue.remove(item) != item {
		queue.stateLock.Unlock()
		panic("immediate receipt lost exact resend ownership")
	}
	queue.stateLock.Unlock()
	self.sendItems = slices.Delete(self.sendItems, index, index+1)

	now := time.Now()
	deliveryTime := now
	if ack.receivedAtNanos != 0 {
		deliveryTime = time.Unix(0, ack.receivedAtNanos)
	}
	self.retainedSilenceStart = time.Time{}
	if !ack.receiverTiming {
		self.observeAckRtt(item, ack.tag)
	}
	bytes := item.MessageByteCount()
	firstSentAtNanos := item.pacingSentAtNanos
	serviced := ByteCount(0)
	credit := windowServiceAckCredit{}
	if !item.deliveryObserved {
		self.observeLiveSelectiveAck(item, ack.receivedAtNanos)
		serviced = bytes
		if self.windowPacer.service != nil {
			serviced = item.pacingByteCount
		}
		credit = self.takePacingServiceCredit(item)
		item.deliveryObserved = true
	}
	if !item.selectiveAcked {
		self.observeItemAck(item)
		self.releaseUnreliableFlight(item)
	}
	self.observeLaneAck(item, now)
	self.observeReliableLaneAck(item, now)
	promoteLanes := uint32(0)
	if item.sendCount > 1 && item.carrierRoute != nil && !item.carrierChanged {
		if slot := self.laneSlotFor(item.carrierRoute); slot >= 0 {
			promoteLanes = uint32(1) << uint(slot)
		}
	}
	number := item.sequenceNumber
	self.ackItem(item)
	// ackItem returns pooled storage. Only captured scalar values may follow.
	if self.sendBuffer != nil && self.sendBuffer.afterAckSendItemForTest != nil {
		self.sendBuffer.afterAckSendItemForTest(self.id(), number)
	}
	self.observeAckedBytesForWrite(bytes, serviced, credit, deliveryTime, firstSentAtNanos)
	if promoteLanes != 0 {
		self.promoteLaneHeads(promoteLanes, now)
	}
	return true
}

// Cumulative receipt has covered these ordinals, so older terminal anchors no
// longer need to survive. Admission and pruning use the same short queue lock.
func (self *SendSequence) retireImmediateTerminalsThrough(number uint64) {
	if self.resendQueue == nil {
		return
	}
	self.resendQueue.stateLock.Lock()
	defer self.resendQueue.stateLock.Unlock()
	removed := 0
	for removed < len(self.immediateTerminals) && self.immediateTerminals[removed].last <= number {
		removed++
	}
	self.immediateTerminals = slices.Delete(self.immediateTerminals, 0, removed)
	if len(self.immediateTerminals) != 0 && self.immediateTerminals[0].first <= number {
		// The remaining interval ends after number, so this cannot overflow.
		self.immediateTerminals[0].first = number + 1
	}
}

// A cumulative receipt may advance across only already-retired immediate
// originals. Record that actual boundary without counting their items twice.
func (self *SendSequence) observeImmediateCumulativeBoundary(number uint64, receivedAtNanos int64) {
	if diagnostics := self.liveDiagnostics; diagnostics != nil && diagnostics.cumulativeAckSequenceNumber < number {
		diagnostics.cumulativeAckCount++
		diagnostics.cumulativeAckSequenceNumber = number
		diagnostics.cumulativeAckAtNanos = max(diagnostics.cumulativeAckAtNanos, receivedAtNanos)
	}
}
