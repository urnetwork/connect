// Reliable unordered delivery is independent of memory policy and routing.
package connect

import "time"

// Requests delivery without waiting for missing sequence predecessors.
// ACKs, retries and downstream custody remain reliable. Old peers may ignore
// the request and deliver in order. This option does not exempt DATA bytes.
func DeliverImmediately() transferOptionsDeliverImmediately {
	return transferOptionsDeliverImmediately{}
}

type transferOptionsDeliverImmediately struct{}

// Coalescing never grants immediate delivery to an ordered sibling.
func (self sendAckRecord) deliversImmediately() bool {
	return self.deliverImmediately || self.group != nil && self.group.ack.deliverImmediately
}

func (self *sendAckSet) deliversImmediately() bool {
	return self.budgetTracked && !self.hasOrdered
}

// Reports whether a Pack first written at now may still ask for immediate
// delivery (SendBufferSettings.ImmediateDeliveryMaxGapAge). The oldest
// outstanding item without a selective ACK is the first ordinal the receiver
// may be missing, and its first write starts the gap. A selective ACK
// refreshes an item's sendTime, but such an item is no gap. Called by the
// send worker, the sole sendItems mutator.
func (self *SendSequence) immediateGapYoung(now time.Time) bool {
	for _, item := range self.sendItems {
		if item == nil || item.selectiveAcked {
			continue
		}
		maxGapAge := self.sendBufferSettings.ImmediateDeliveryMaxGapAge
		return 0 < maxGapAge && now.Sub(item.sendTime) <= maxGapAge
	}
	return true
}

// The configured count bounds serialized immediate custody independently of
// DATA windows. Its reliable receipts recycle the count even behind a DATA gap.
func (self *SendSequence) immediateSendAvailable(pack *SendPack) bool {
	return !pack.DeliverImmediately || self.immediateSendCount < max(1, self.immediateSendLimit)
}

// Count pressure is observed only with a real queued immediate owner. It never
// changes DATA admission or creates a byte reservation. The owner publishes it
// through the existing optional immutable diagnostic snapshot.
func (self *SendSequence) observeImmediateCapacityWait(scheduler *sendPackScheduler, now time.Time) {
	blocked := false
	if self.immediateSendCount >= max(1, self.immediateSendLimit) {
		for _, pack := range scheduler.order {
			if pack.DeliverImmediately {
				blocked = true
				break
			}
		}
	}
	if blocked && self.immediateWaitStart.IsZero() {
		self.immediateWaitCount++
		self.immediateWaitStart = now
	} else if !blocked && !self.immediateWaitStart.IsZero() {
		self.immediateWaitNanos += max(time.Duration(0), now.Sub(self.immediateWaitStart))
		self.immediateWaitStart = time.Time{}
	}
}
