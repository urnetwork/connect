// ACK lifetimes belong to the send worker, including while one of its writes
// waits in a shared pacer. The index contains only discardable retained items.
package connect

import (
	"container/heap"
	"errors"
	"time"
)

// Delivery ends an unissued recovery attempt without becoming a write failure.
var errWindowPacingAcknowledged = errors.New("paced recovery already acknowledged")

// One deadline per retained message; the send worker owns every mutation.
// Recovery temporarily removes messages from the resend queue, so this index
// keeps its own membership until delivery or sequence shutdown.
type sendAckLifetimes struct {
	items []sendAckLifetime
}

// A queued reply can renew expiry before the send loop consumes it. Keep that
// expiry separate from the original timestamp used to validate echoed RTT tags.
type sendAckLifetime struct {
	item *sendItem
	at   time.Time
}

// Insert or repair one deadline without scanning the outstanding flight.
func (self *sendAckLifetimes) update(item *sendItem) {
	if !item.expectsAck || item.acks.retainPastAckTimeout() {
		self.remove(item)
		return
	}
	if item.ackLifetimeIndex == 0 {
		heap.Push(self, item)
	} else {
		self.items[item.ackLifetimeIndex-1].at = item.sendTime.Add(item.ackTimeout)
		heap.Fix(self, item.ackLifetimeIndex-1)
	}
}

// Removal precedes returning the item to its pool; zero means unindexed.
func (self *sendAckLifetimes) remove(item *sendItem) {
	if item.ackLifetimeIndex != 0 {
		heap.Remove(self, item.ackLifetimeIndex-1)
	}
}

// Shutdown removes all borrowed item pointers before the resend queue drains.
func (self *sendAckLifetimes) clear() {
	for _, entry := range self.items {
		entry.item.ackLifetimeIndex = 0
	}
	clear(self.items)
	self.items = self.items[:0]
}

// Heap operations are confined to the send worker.
func (self *sendAckLifetimes) Len() int { return len(self.items) }

// Order by absolute ACK expiry, independently of recovery/backoff ordering.
func (self *sendAckLifetimes) Less(i, j int) bool {
	return self.items[i].at.Before(self.items[j].at)
}

// Keep intrusive indices aligned through every heap adjustment.
func (self *sendAckLifetimes) Swap(i, j int) {
	self.items[i], self.items[j] = self.items[j], self.items[i]
	self.items[i].item.ackLifetimeIndex, self.items[j].item.ackLifetimeIndex = i+1, j+1
}

// New items are indexed before any physical write can expose their identity.
func (self *sendAckLifetimes) Push(value any) {
	item := value.(*sendItem)
	self.items = append(self.items, sendAckLifetime{item: item, at: item.sendTime.Add(item.ackTimeout)})
	item.ackLifetimeIndex = len(self.items)
}

// Clear both the index and backing slot so pooled items cannot remain aliased.
func (self *sendAckLifetimes) Pop() any {
	last := len(self.items) - 1
	item := self.items[last].item
	self.items[last] = sendAckLifetime{}
	self.items = self.items[:last]
	item.ackLifetimeIndex = 0
	return item
}

// Read validated feedback without consuming the owner's acknowledgement snapshot.
// A pending head delivers its prefix and renews the uncovered tail from its
// original arrival. Applied heads stay out of this pending-feedback decision.
func (self *sequenceAckWindow) pendingLifetimeFeedback(item *sendItem) (bool, int64) {
	self.ackLock.Lock()
	defer self.ackLock.Unlock()
	receivedAtNanos := int64(0)
	if self.ackUpdateCount > 0 && self.hasHeadAck {
		if item.sequenceNumber <= self.headAck.sequenceNumber {
			return true, 0
		}
		// Duplicate and stale heads do not replace headAck or its arrival.
		// Pacing may hold Run past expiry while this progress remains pending.
		receivedAtNanos = self.headAck.receivedAtNanos
	}
	if ack, ok := self.selectiveAcks[item.messageId]; ok && ack.sequenceNumber == item.sequenceNumber {
		receivedAtNanos = max(receivedAtNanos, ack.receivedAtNanos)
	}
	if ack, ok := self.contractMissingAcks[item.messageId]; ok && ack.sequenceNumber == item.sequenceNumber &&
		item.contractId != nil && !item.hasContractFrame && ack.missingContractId == *item.contractId {
		// Independently coalesced feedback may describe the same item. The
		// newest valid receipt renews it; a contract request is not delivery.
		receivedAtNanos = max(receivedAtNanos, ack.receivedAtNanos)
	}
	return false, receivedAtNanos
}

// Contract requests do not prove delivery and cannot cancel a physical retry.
func (self *sequenceAckWindow) pendingDeliveryFor(sequenceNumber uint64, messageId Id) bool {
	self.ackLock.Lock()
	defer self.ackLock.Unlock()
	if self.ackUpdateCount > 0 && self.hasHeadAck && sequenceNumber <= self.headAck.sequenceNumber {
		return true
	}
	ack, ok := self.selectiveAcks[messageId]
	return ok && ack.sequenceNumber == sequenceNumber
}

// Both expiry decisions use the same typed pending evidence. A renewal is
// anchored to the actual reply arrival, never to the time this check runs.
// Zero means no valid renewal; contract requests still do not prove delivery.
func (self *SendSequence) pendingAckLifetime(item *sendItem) (bool, time.Time) {
	if self.ackWindow == nil {
		return false, time.Time{}
	}
	delivered, receivedAtNanos := self.ackWindow.pendingLifetimeFeedback(item)
	if delivered || receivedAtNanos == 0 {
		return delivered, time.Time{}
	}
	return false, self.ackArrivalTime(receivedAtNanos).Add(item.ackTimeout)
}

// Restore the client's elapsed-time clock. Zero is unavailable feedback, never
// a reason to renew from the later time when the send owner consumes a snapshot.
func (self *SendSequence) ackArrivalTime(receivedAtNanos int64) time.Time {
	if receivedAtNanos == 0 {
		return time.Time{}
	}
	if self.client != nil && !self.client.feedbackTimeBase.IsZero() {
		base := self.client.feedbackTimeBase
		return base.Add(time.Duration(receivedAtNanos - base.UnixNano()))
	}
	return time.Unix(0, receivedAtNanos)
}

// An advancing cumulative head proves the receiver is alive and the flight is
// draining in order, so a message still waiting behind it has not been lost to
// a dead path. Its lifetime runs from the last advance. A head that stops gets
// no renewal and the sequence expires as before. Zero means no head has advanced.
func (self *SendSequence) ackProgressDeadline(item *sendItem) time.Time {
	if self.lastHeadAckTime.IsZero() {
		return time.Time{}
	}
	return self.lastHeadAckTime.Add(item.ackTimeout)
}

// The time this item's lifetime ends, counting head progress. A resend capped
// at the send-time deadline alone would always be due once the item outlives it.
func (self *SendSequence) ackLifetimeDeadline(item *sendItem) time.Time {
	deadline := item.sendTime.Add(item.ackTimeout)
	if progressDeadline := self.ackProgressDeadline(item); deadline.Before(progressDeadline) {
		return progressDeadline
	}
	return deadline
}

// Future deadlines are constant-time reads. At expiry, reconcile only the due
// prefix against feedback already received while the owner was pacing. Actual
// delivery, callbacks and buffer release remain in the ordinary send loop.
func (self *SendSequence) nextAckLifetime(now time.Time) (time.Time, error) {
	for len(self.ackLifetimes.items) > 0 {
		entry := self.ackLifetimes.items[0]
		item, deadline := entry.item, entry.at
		if now.Before(deadline) {
			return deadline, nil
		}
		delivered, renewedDeadline := self.pendingAckLifetime(item)
		if delivered {
			self.ackLifetimes.remove(item)
			continue
		}
		if progressDeadline := self.ackProgressDeadline(item); renewedDeadline.Before(progressDeadline) {
			renewedDeadline = progressDeadline
		}
		if renewedDeadline.After(deadline) {
			self.ackLifetimes.items[0].at = renewedDeadline
			heap.Fix(&self.ackLifetimes, 0)
			continue
		}
		return deadline, self.ackLifetimeDisposition(item, deadline)
	}
	return time.Time{}, nil
}
