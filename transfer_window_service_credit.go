// Service delivery is published at ACK arrival, independently of paced sender
// workers. Queue locking protects per-item credit; service locking protects
// aggregate publication and cancellation of each sequence's ownership.
package connect

import "time"

// Every credited envelope retains the earliest physical offer in its group.
// A missing timestamp stays explicit and cannot establish a flight boundary.
type windowServiceAckCredit struct {
	bytes            ByteCount
	firstSentAtNanos int64
}

// Merge only newly delivered envelopes, preserving incomplete offer evidence.
func (self *windowServiceAckCredit) add(other windowServiceAckCredit) {
	if other.bytes <= 0 {
		return
	}
	if self.bytes == 0 || other.firstSentAtNanos < self.firstSentAtNanos {
		self.firstSentAtNanos = other.firstSentAtNanos
	}
	self.bytes += other.bytes
}

// The item's resend queue lock must be held. A retry does not restore credit.
func (self *windowServiceAckCredit) addItemWithLock(item *sendItem) {
	if item == nil || item.serviceCreditObserved || item.pacingByteCount <= 0 {
		return
	}
	item.serviceCreditObserved = true
	self.add(windowServiceAckCredit{bytes: item.pacingByteCount, firstSentAtNanos: item.pacingSentAtNanos})
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
	if self.windowPacer.service == nil || self.resendQueue == nil {
		return
	}
	credit := windowServiceAckCredit{}
	func() {
		self.resendQueue.stateLock.Lock()
		defer self.resendQueue.stateLock.Unlock()
		queue := self.resendQueue
		item := queue.messageIdItems[messageId]
		if item == nil {
			return
		}
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
	service.observeWithLock(credit.bytes, at)
}
