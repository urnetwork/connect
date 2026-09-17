// Sender timing retains one exact first-write/ACK pair. Receiver residence
// corrects sizing samples without shortening the raw recovery clock.
package connect

import (
	"time"
)

// The queue lock protects timing publication and one-shot consumption. A
// retry or failed physical write makes its copied acknowledgement ambiguous.
type sendItemRttState uint8

const (
	sendItemRttUnavailable sendItemRttState = iota
	sendItemRttWritePending
	sendItemRttWriteConfirmed
	sendItemRttObserved
)

// One sender worker has at most one physical write in progress. Keep the
// earliest exact reply until the actual writer confirms carrier and success.
type pendingReceiverRtt struct {
	messageId         Id
	receivedAtNanos   int64
	delayMicros       uint32
	compressionMicros uint32
}

// Preserve the established local-nanosecond API while deriving elapsed time
// from the client's monotonic clock, rather than subtracting two wall clocks.
func (self *Client) feedbackArrivalNanos(at time.Time) int64 {
	if self == nil || self.feedbackTimeBase.IsZero() {
		return at.UnixNano()
	}
	return self.feedbackTimeBase.Add(at.Sub(self.feedbackTimeBase)).UnixNano()
}

// Starts an exact initial-write record after local pacing and wrapping. The
// existing pacing timestamp is shared with service accounting, not duplicated.
func (self *SendSequence) beginReceiverRttWrite(item *sendItem, resend bool) {
	if item == nil || !item.expectsAck || self.resendQueue == nil {
		return
	}
	self.resendQueue.stateLock.Lock()
	defer self.resendQueue.stateLock.Unlock()
	self.pendingReceiverRtt = pendingReceiverRtt{}
	if item.rttState == sendItemRttObserved {
		return
	}
	if resend || item.rttState != sendItemRttUnavailable || item.sendCount != 1 {
		item.rttState = sendItemRttUnavailable
		return
	}
	item.pacingSentAtNanos = self.client.feedbackArrivalNanos(time.Now())
	item.rttState = sendItemRttWritePending
}

// Invalidating before a retry's pacing wait prevents any late old copy from
// becoming a new exact sample. An already consumed observation stays consumed
// so a later reply without metadata cannot reopen its legacy tag sampler.
func (self *SendSequence) invalidateReceiverRttWrite(item *sendItem) {
	if item == nil || self.resendQueue == nil {
		return
	}
	self.resendQueue.stateLock.Lock()
	defer self.resendQueue.stateLock.Unlock()
	if item.rttState != sendItemRttObserved {
		item.rttState = sendItemRttUnavailable
	}
	if self.pendingReceiverRtt.messageId == item.messageId {
		self.pendingReceiverRtt = pendingReceiverRtt{}
	}
}

// Matching timing is applied outside the queue lock. A synchronous ACK may
// reach the coalescer before this callback, but cannot certify a failed write.
func (self *SendSequence) finishReceiverRttWrite(item *sendItem, disposition transferWriteDisposition, writeErr error) {
	if item == nil || !item.expectsAck || self.resendQueue == nil {
		return
	}
	var pending pendingReceiverRtt
	var sentAtNanos int64
	var burst uint64
	var paced bool
	func() {
		self.resendQueue.stateLock.Lock()
		defer self.resendQueue.stateLock.Unlock()
		if item.rttState != sendItemRttWritePending {
			return
		}
		if writeErr != nil || disposition.unreliable {
			item.rttState = sendItemRttUnavailable
			self.pendingReceiverRtt = pendingReceiverRtt{}
			return
		}
		item.rttState = sendItemRttWriteConfirmed
		item.rttH1 = disposition.transportType == TransportTypeH1
		if self.pendingReceiverRtt.messageId == item.messageId {
			pending = self.pendingReceiverRtt
			self.pendingReceiverRtt = pendingReceiverRtt{}
			sentAtNanos, burst, paced = item.pacingSentAtNanos, item.pacingBurst, item.pacingByteCount > 0 && item.rttH1
			item.rttState = sendItemRttObserved
		}
	}()
	if pending.receivedAtNanos != 0 {
		self.applyReceiverRtt(sentAtNanos, pending, burst, paced)
	}
}

// Observes the exact message and initial echoed tag while the item is still
// retained. Only a confirmed H1 head may pair its receiver wait with new credit.
func (self *SendSequence) observeReceiverAckRtt(ack receiveAckMessage) windowServiceAckTiming {
	if !ack.receiverAckDelaySet || !ack.tag.set || ack.contractMissing || self.resendQueue == nil {
		return windowServiceAckTiming{}
	}
	var observation pendingReceiverRtt
	var sentAtNanos int64
	var burst uint64
	var paced bool
	func() {
		self.resendQueue.stateLock.Lock()
		defer self.resendQueue.stateLock.Unlock()
		item := self.resendQueue.messageIdItems[ack.messageId]
		if item == nil || (item.rttState != sendItemRttWritePending && item.rttState != sendItemRttWriteConfirmed) ||
			ack.tag.sendTime != uint64(item.sendTime.UnixMilli()) || item.pacingSentAtNanos == 0 ||
			ack.receivedAtNanos < item.pacingSentAtNanos {
			return
		}
		raw := time.Duration(ack.receivedAtNanos - item.pacingSentAtNanos)
		if raw < time.Duration(ack.receiverAckDelayMicros)*time.Microsecond {
			return
		}
		compressionMicros := uint32(defaultAckCompressTimeout.Microseconds())
		if ack.ackCompressTimeoutSet {
			compressionMicros = ack.ackCompressTimeoutMicros
		}
		observation = pendingReceiverRtt{messageId: ack.messageId, receivedAtNanos: ack.receivedAtNanos, delayMicros: ack.receiverAckDelayMicros, compressionMicros: compressionMicros}
		if item.rttState == sendItemRttWritePending {
			if self.pendingReceiverRtt.receivedAtNanos == 0 || ack.receivedAtNanos < self.pendingReceiverRtt.receivedAtNanos {
				self.pendingReceiverRtt = observation
			}
			observation = pendingReceiverRtt{}
			return
		}
		sentAtNanos, burst, paced = item.pacingSentAtNanos, item.pacingBurst, item.pacingByteCount > 0 && item.rttH1
		item.rttState = sendItemRttObserved
	}()
	if observation.receivedAtNanos != 0 {
		self.applyReceiverRtt(sentAtNanos, observation, burst, paced)
		if paced {
			return windowServiceAckTiming{receivedAtNanos: observation.receivedAtNanos, receiverDelay: time.Duration(observation.delayMicros) * time.Microsecond}
		}
	}
	return windowServiceAckTiming{}
}

// Recovery keeps the complete physical residence. The optional adjusted value
// is a separate sample, and local ACK-worker delay never enters either value.
func (self *SendSequence) applyReceiverRtt(sentAtNanos int64, observation pendingReceiverRtt, burst uint64, paced bool) {
	at := time.Unix(0, observation.receivedAtNanos)
	raw := time.Duration(observation.receivedAtNanos - sentAtNanos)
	self.rttWindow.observeReceiverRoundTrip(raw, time.Duration(observation.delayMicros)*time.Microsecond, observation.compressionMicros, at)
	if service := self.windowPacer.service; service != nil && paced {
		service.observeReceiverRoundTripForWrite(self.sequenceId, observation.messageId, burst, raw, raw-time.Duration(observation.delayMicros)*time.Microsecond, time.Duration(observation.compressionMicros)*time.Microsecond, at)
	}
}

// The send worker must not count a sample already published by the coalescer.
func (self *SendSequence) receiverRttObserved(item *sendItem) bool {
	if item == nil || self.resendQueue == nil {
		return false
	}
	self.resendQueue.stateLock.Lock()
	defer self.resendQueue.stateLock.Unlock()
	return item.rttState == sendItemRttObserved
}
