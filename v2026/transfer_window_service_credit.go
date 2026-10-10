// Service delivery is published at ACK arrival, independently of paced sender
// workers. Queue locking protects per-item credit; service locking protects
// aggregate publication and cancellation of each sequence's ownership.
package connect

import "time"

// A validated initial H1 head supplies only its own receiver wait. The raw
// arrival remains the ownership, recovery and physical drain clock.
type windowServiceAckTiming struct {
	receivedAtNanos int64
	receiverDelay   time.Duration
	headSentAtNanos int64
}

// Every credited envelope retains the earliest physical offer in its group.
// A missing timestamp stays explicit and cannot establish a flight boundary.
type windowServiceAckCredit struct {
	bytes                  ByteCount
	firstSentAtNanos       int64
	lastSentAtNanos        int64
	maximumOfferGap        time.Duration
	offerRecorded          bool
	offerComplete          bool
	receiverTimingObserved bool
	receiverTiming         windowServiceAckTiming
	receiverHeadAtNanos    int64
	receiverTimingEligible bool
	receiverHeldPrefix     bool
}

// Merge only newly delivered envelopes, preserving incomplete offer evidence.
func (self *windowServiceAckCredit) add(other windowServiceAckCredit) {
	if other.bytes <= 0 {
		return
	}
	if self.bytes == 0 {
		self.receiverTimingEligible = other.receiverTimingEligible
		self.offerComplete = other.offerComplete
		self.maximumOfferGap = other.maximumOfferGap
	} else {
		self.receiverTimingEligible = self.receiverTimingEligible && other.receiverTimingEligible
		self.offerComplete = self.offerComplete && other.offerComplete
		// Overlapping summaries may fill an old gap, but never erase it.
		gap := max(int64(0), other.firstSentAtNanos-self.lastSentAtNanos,
			self.firstSentAtNanos-other.lastSentAtNanos)
		self.maximumOfferGap = max(self.maximumOfferGap, other.maximumOfferGap, time.Duration(gap))
	}
	if self.bytes == 0 || other.firstSentAtNanos < self.firstSentAtNanos {
		self.firstSentAtNanos = other.firstSentAtNanos
	}
	self.lastSentAtNanos = max(self.lastSentAtNanos, other.lastSentAtNanos)
	self.offerRecorded = self.offerRecorded || other.offerRecorded
	self.receiverTimingObserved = self.receiverTimingObserved || other.receiverTimingObserved
	self.receiverHeldPrefix = self.receiverHeldPrefix || other.receiverHeldPrefix
	self.bytes += other.bytes
}

// The item's resend queue lock must be held. A retry does not restore credit.
func (self *windowServiceAckCredit) addItemWithLock(item *sendItem) {
	if item == nil || item.serviceCreditObserved || item.pacingByteCount <= 0 {
		return
	}
	item.serviceCreditObserved = true
	self.add(windowServiceAckCredit{bytes: item.pacingByteCount, firstSentAtNanos: item.pacingSentAtNanos,
		lastSentAtNanos: item.pacingSentAtNanos, offerRecorded: true,
		offerComplete: item.pacingSentAtNanos > 0 && item.sendCount == 1 && item.rttH1 &&
			(item.rttState == sendItemRttWriteConfirmed || item.rttState == sendItemRttObserved),
		receiverTimingEligible: item.rttH1 && (item.rttState == sendItemRttWriteConfirmed || item.rttState == sendItemRttObserved)})
}

// Worker fallback covers items temporarily outside the retry queue when the
// coalescer received a covering head. It cannot repeat earlier SACK credit.
func (self *SendSequence) takePacingServiceCredit(item *sendItem) windowServiceAckCredit {
	credit := windowServiceAckCredit{}
	if self.windowPacer.service == nil || self.resendQueue == nil {
		return credit
	}
	self.resendQueue.stateLock.Lock()
	defer self.resendQueue.stateLock.Unlock()
	credit.addItemWithLock(item)
	return credit
}

// A cumulative head normally visits only its newly acknowledged prefix.
// Reliable sequence numbers are contiguous; a sparse synthetic/retired prefix
// uses the bounded live index instead of iterating an arbitrary numeric gap.
func (self *SendSequence) publishAckServiceCredit(messageId Id, selective bool, at time.Time) {
	self.publishAckServiceCreditWithTiming(messageId, selective, at, windowServiceAckTiming{})
}

// Only newly credited bytes from this exact head can use its receiver wait.
// A previously credited head cannot retime an earlier cumulative prefix.
func (self *SendSequence) publishAckServiceCreditWithTiming(messageId Id, selective bool, at time.Time, timing windowServiceAckTiming) {
	if self.windowPacer.service == nil || self.resendQueue == nil {
		return
	}
	credit := windowServiceAckCredit{}
	headCredited := false
	headByteCount := ByteCount(0)
	headSentAtNanos := int64(0)
	func() {
		self.resendQueue.stateLock.Lock()
		defer self.resendQueue.stateLock.Unlock()
		queue := self.resendQueue
		item := queue.messageIdItems[messageId]
		if item == nil {
			return
		}
		headCredited = !item.serviceCreditObserved && item.pacingByteCount > 0
		headByteCount = item.pacingByteCount
		headSentAtNanos = item.pacingSentAtNanos
		if selective {
			credit.addItemWithLock(item)
			return
		}
		head := item.sequenceNumber
		if self.serviceAckHeadSet && head <= self.serviceAckHeadNumber {
			credit.addItemWithLock(item)
			return
		}
		first := uint64(0)
		if self.serviceAckHeadSet {
			first = self.serviceAckHeadNumber + 1
		}
		if head-first >= uint64(len(queue.sequenceNumberItems)) {
			for number, pending := range queue.sequenceNumberItems {
				if first <= number && number <= head {
					credit.addItemWithLock(pending)
				}
			}
		} else {
			for number := first; ; number++ {
				credit.addItemWithLock(queue.sequenceNumberItems[number])
				if number == head {
					break
				}
			}
		}
		self.serviceAckHeadNumber, self.serviceAckHeadSet = head, true
	}()
	credit.receiverTimingObserved = timing.receivedAtNanos != 0 && timing.receivedAtNanos == at.UnixNano()
	// The validated head retains its own arrival boundary even when its
	// wait cannot retime every byte in the newly acknowledged prefix.
	if headCredited && timing.receivedAtNanos == at.UnixNano() && timing.receivedAtNanos > 0 &&
		timing.receiverDelay >= 0 && int64(timing.receiverDelay) <= timing.receivedAtNanos &&
		headSentAtNanos > 0 && headSentAtNanos <= timing.receivedAtNanos-int64(timing.receiverDelay) {
		credit.receiverHeadAtNanos = timing.receivedAtNanos - int64(timing.receiverDelay)
	}
	// A relayed head can arrive before an earlier sequence hole. Its own
	// wait cannot remove time spent receiving that newly credited prefix.
	if headCredited && credit.receiverTimingEligible && (timing.receiverDelay == 0 || credit.bytes == headByteCount) &&
		timing.receivedAtNanos != 0 && timing.receivedAtNanos == at.UnixNano() {
		credit.receiverTiming = timing
		credit.receiverTiming.headSentAtNanos = headSentAtNanos
	}
	// Refusing to retime a multi-item prefix must not discard evidence that
	// it was held behind a hole or receiver backpressure. Its raw release is
	// valid delivery, but cannot by itself prove faster physical service.
	credit.receiverHeldPrefix = headCredited && credit.bytes > headByteCount &&
		timing.receivedAtNanos != 0 && timing.receivedAtNanos == at.UnixNano() &&
		timing.receiverDelay > self.ackCompressionResidence()
	self.observePacingServiceCredit(credit, at)
}

// Cancellation and publication share one lock. Late callbacks cannot recreate
// credit after close has released this sequence's unacknowledged ownership.
func (self *SendSequence) observePacingServiceCredit(credit windowServiceAckCredit, at time.Time) {
	service := self.windowPacer.service
	if service == nil || credit.bytes <= 0 {
		return
	}
	service.stateLock.Lock()
	defer service.stateLock.Unlock()
	if self.windowPacer.serviceClosed {
		return
	}
	self.windowPacer.serviceAcked += credit.bytes
	if !service.qualityChangedAt.IsZero() && credit.firstSentAtNanos <= service.qualityChangedAt.UnixNano() {
		// Old ACKs still repay physical ownership. They cannot supply a
		// checkpoint or be paired with bytes from the new generation.
		service.accountAckWithLock(credit.bytes, at)
		service.completeFeedbackCycleWithLock()
		return
	}
	if credit.receiverHeldPrefix {
		service.receiverHeldPrefixAtNanos = max(service.receiverHeldPrefixAtNanos, at.UnixNano())
	}
	service.observeAggregateDeliveryWithLock(credit, at)
	service.observeAckWithLock(credit, at)
}
