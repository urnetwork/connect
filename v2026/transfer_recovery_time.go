// Ordinary reliable recovery consumes exact raw residence without changing
// the ownership or identity of per-sequence RTT samples.
package connect

import (
	"container/heap"
	"time"
)

// Receiver delay stays in recovery residence. Only the current H1 policy may
// use this shared H1 evidence; changed and unreliable carriers keep their own
// recovery contract. Expired or future metadata cannot extend a timer.
func (self *SendSequence) sharedRawRecoveryInterval(item *sendItem, now time.Time) time.Duration {
	if item == nil || self.windowPacer.service == nil || !item.reliableCarrierObserved ||
		!item.rttH1 || item.unreliableCarrierObserved || item.hybridReliableCarrierObserved ||
		item.carrierChanged || !self.transferFlightPolicy().h1Only {
		return 0
	}
	// The previous path's short RTO cannot certify a new first write as
	// lost before this generation has any RTT. Keep the configured cold
	// floor, anchored to that physical write, without extending its lifetime.
	if item.sendCount == 1 {
		if interval := self.windowPacer.service.qualityRecoveryInterval(item.pacingSentAtNanos,
			self.sendBufferSettings.MinResendInterval, self.sendBufferSettings.MaxResendInterval); interval > 0 {
			return interval
		}
	}
	timing := self.windowPacer.service.roundTripEvidence(now)
	if timing.count == 0 || timing.latestRaw <= 0 {
		return 0
	}
	maximum := self.sendBufferSettings.MaxResendInterval
	scaled := float64(timing.latestRaw) * max(1, float64(self.sendBufferSettings.RttScale))
	if scaled >= float64(maximum) {
		return maximum
	}
	return time.Duration(scaled)
}

// Changing a retained item's deadline preserves both heap orders and its
// continuous ACK lookup/budget ownership. The send worker owns the deadline;
// queue readers are excluded until all ordering indices agree again.
func (self *SendSequence) setResendTime(item *sendItem, at time.Time) {
	queue := self.resendQueue
	queue.stateLock.Lock()
	defer queue.stateLock.Unlock()
	item.resendTime = at
	if queue.messageIdItems[item.messageId] == item {
		heap.Fix(queue, item.HeapIndex())
		heap.Fix(queue.maxHeap, item.MaxHeapIndex())
	}
}

// The stored first-write timestamp is based on this client's monotonic elapsed
// time. Reconstruct that same clock for deadline comparisons; synthetic clients
// without a clock base keep the existing wall-time fallback.
func (self *SendSequence) firstPhysicalRecoveryTime(item *sendItem) time.Time {
	if self.client != nil && !self.client.feedbackTimeBase.IsZero() {
		base := self.client.feedbackTimeBase
		return base.Add(time.Duration(item.pacingSentAtNanos - base.UnixNano()))
	}
	return time.Unix(0, item.pacingSentAtNanos)
}
