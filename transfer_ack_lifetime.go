// ACK lifetimes belong to the send worker, including while one of its writes
// waits in a shared pacer. The index contains only discardable retained items.
package connect

import (
	"container/heap"
	"context"
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

// Read already validated feedback without consuming the owner's ACK snapshot.
// A head finishes lifetime ownership; a SACK or valid contract recovery request
// can only renew it from that reply's actual arrival, once.
func (self *sequenceAckWindow) pendingLifetimeFeedback(item *sendItem) (bool, int64) {
	self.ackLock.Lock()
	defer self.ackLock.Unlock()
	if self.ackUpdateCount > 0 && self.hasHeadAck && item.sequenceNumber <= self.headAck.sequenceNumber {
		return true, 0
	}
	receivedAtNanos := int64(0)
	if ack, ok := self.selectiveAcks[item.messageId]; ok && ack.sequenceNumber == item.sequenceNumber {
		receivedAtNanos = ack.receivedAtNanos
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
		if self.ackWindow != nil {
			delivered, receivedAtNanos := self.ackWindow.pendingLifetimeFeedback(item)
			if delivered {
				self.ackLifetimes.remove(item)
				continue
			}
			if receivedAtNanos != 0 {
				renewedAt := time.Unix(0, receivedAtNanos)
				if self.client != nil && !self.client.feedbackTimeBase.IsZero() {
					base := self.client.feedbackTimeBase
					renewedAt = base.Add(time.Duration(receivedAtNanos - base.UnixNano()))
				}
				if renewedDeadline := renewedAt.Add(item.ackTimeout); renewedDeadline.After(deadline) {
					self.ackLifetimes.items[0].at = renewedDeadline
					heap.Fix(&self.ackLifetimes, 0)
					continue
				}
			}
		}
		return deadline, context.DeadlineExceeded
	}
	return time.Time{}, nil
}
