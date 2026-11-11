// Reliable immediate delivery keeps bounded receipt evidence independently of
// pooled payload owners. Only the receive worker advances the ordered floor.
package connect

import (
	"crypto/sha256"
	"math"
	"sync"
	"time"

	"github.com/urnetwork/connect/protocol"
)

const receiveImmediateHistoryCapacity = 1024
const receiveImmediateRangeCapacity = 32

// A pending receipt has one exact ordinal. Secured adjacent ordinals under the
// same verified contract merge; deterministic IDs authenticate interior retries.
type receiveImmediateRange struct {
	first      uint64
	last       uint64
	token      uint64
	contractId Id
	expires    time.Time
	received   time.Time
	pending    bool
	secured    bool
	unwrapped  bool
}

// This buffer-owned history survives a worker's ordinary gap/idle exit. It is
// never evicted while replay authority remains live. Saturation refuses new
// early admission; it grants neither byte permission nor cumulative progress.
type receiveImmediateHistory struct {
	stateLock     sync.Mutex
	sequenceId    Id
	maxRanges     int
	ranges        [receiveImmediateRangeCapacity]*receiveImmediateRange
	nextToken     uint64
	pendingCount  int
	noExpiry      bool
	expires       time.Time
	cumulative    sequenceAck
	hasCumulative bool
	everSecured   bool
}

func (self *ReceiveSequence) immediateHeadKey() receiveSequenceHeadKey {
	return receiveSequenceHeadKey{Source: self.source, EncryptionRole: self.encryptionRole,
		EncryptionCompanion: self.encryptionCompanion, ForceStream: self.transferKey.ForceStream,
		CompanionContract: self.transferKey.CompanionContract, LogicalLane: self.transferKey.LogicalLane}
}

// Called only by the receive worker. The buffer mutex publishes history across
// worker replacement; history's own lock serializes receipt completions.
func (self *ReceiveSequence) ensureImmediateHistory() *receiveImmediateHistory {
	if self.immediateHistory != nil {
		return self.immediateHistory
	}
	buffer := self.client.receiveBuffer
	if buffer == nil {
		return nil
	}
	buffer.mutex.Lock()
	defer buffer.mutex.Unlock()
	key := self.immediateHeadKey()
	if history := buffer.immediateHistories[key]; history != nil {
		history.stateLock.Lock()
		if history.sequenceId != self.sequenceId {
			if self.sequenceId.LessThan(history.sequenceId) || history.pendingCount != 0 {
				history.stateLock.Unlock()
				return nil
			}
			history.sequenceId = self.sequenceId
			for _, span := range history.ranges {
				if span != nil {
					*span = receiveImmediateRange{}
				}
			}
			history.cumulative = sequenceAck{}
			history.hasCumulative = false
			if !history.everSecured {
				history.noExpiry = false
				history.expires = time.Time{}
			}
		}
		history.stateLock.Unlock()
		self.publishImmediateHistory(history)
		return history
	}
	if len(buffer.immediateHistories) >= receiveImmediateHistoryCapacity {
		now := time.Now()
		for headKey, history := range buffer.immediateHistories {
			history.stateLock.Lock()
			expired := history.pendingCount == 0 && (!history.everSecured ||
				!history.noExpiry && !history.expires.IsZero() && !now.Before(history.expires))
			history.stateLock.Unlock()
			if expired {
				delete(buffer.immediateHistories, headKey)
			}
		}
	}
	if len(buffer.immediateHistories) >= receiveImmediateHistoryCapacity {
		return nil
	}
	if buffer.immediateHistories == nil {
		buffer.immediateHistories = map[receiveSequenceHeadKey]*receiveImmediateHistory{}
	}
	history := &receiveImmediateHistory{sequenceId: self.sequenceId,
		maxRanges: min(receiveImmediateRangeCapacity, max(1, self.receiveBufferSettings.SequenceBufferSize))}
	buffer.immediateHistories[key] = history
	self.publishImmediateHistory(history)
	return history
}

// The same ACK lock orders the first snapshot against asynchronous older
// receipt completion. Callback owners read only the atomic published pointer.
func (self *ReceiveSequence) publishImmediateHistory(history *receiveImmediateHistory) {
	self.ackWindow.ackLock.Lock()
	self.immediateHistory = history
	self.immediateDeliveryHistory.Store(history)
	if self.ackWindow.hasHeadAck {
		history.noteCumulative(self.ackWindow.headAck)
	}
	self.ackWindow.ackLock.Unlock()
}

// No new proof is registered and no current-contract pointer is changed here.
// Only an already verified, unexpired exact reference can authorize early work.
func (self *ReceiveSequence) immediateContract(item *receiveItem) (*sequenceContract, bool) {
	if item.contractAhead {
		return nil, false
	}
	if item.contractId == nil && item.sequenceNumber == self.nextSequenceNumber && self.receiveContract != nil {
		item.contractId = &self.receiveContract.contractId
	}
	if item.contractId == nil {
		return nil, item.contractFrame == nil && self.client.ContractManager().ReceiveNoContract(self.source.SourceId)
	}
	contract := self.openReceiveContracts[*item.contractId]
	if contract == nil || contract.expired() {
		return nil, false
	}
	if item.contractFrame != nil && (item.contractFrame.MessageType != protocol.MessageType_TransferContract ||
		!contract.verifiedReceiveProofSet || sha256.Sum256(item.contractFrame.MessageBytes) != contract.verifiedReceiveProof) {
		return nil, false
	}
	return contract, true
}

func (self *receiveImmediateHistory) reserve(number uint64, contractId Id, expires time.Time,
	received time.Time, unwrapped bool) (*receiveImmediateRange, uint64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	var free *receiveImmediateRange
	freeIndex := -1
	for index := range self.maxRanges {
		span := self.ranges[index]
		if span == nil || !span.pending && !span.secured {
			if freeIndex < 0 {
				free, freeIndex = span, index
			}
			continue
		}
		if span.first <= number && number <= span.last {
			return nil, 0
		}
	}
	if freeIndex < 0 {
		return nil, 0
	}
	if free == nil {
		free = &receiveImmediateRange{}
		self.ranges[freeIndex] = free
	}
	self.nextToken++
	if self.nextToken == 0 {
		self.nextToken++
	}
	*free = receiveImmediateRange{first: number, last: number, token: self.nextToken,
		contractId: contractId, expires: expires, received: received, pending: true, unwrapped: unwrapped}
	self.pendingCount++
	if expires.IsZero() {
		self.noExpiry = true
	} else if self.expires.Before(expires) {
		self.expires = expires
	}
	return free, free.token
}

// Completion may run on a downstream owner. It records only actual secured
// custody, never a receive floor; a failed/unclaimed callback releases its slot.
func (self *receiveImmediateHistory) complete(span *receiveImmediateRange, token uint64, secured bool) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if span == nil || span.token != token || !span.pending {
		return false
	}
	span.pending = false
	self.pendingCount--
	if !secured {
		*span = receiveImmediateRange{}
		return false
	}
	span.secured = true
	self.everSecured = true
	for {
		merged := false
		for index := range self.maxRanges {
			other := self.ranges[index]
			if other == nil || other == span || !other.secured || other.contractId != span.contractId {
				continue
			}
			if other.last != math.MaxUint64 && other.last+1 == span.first {
				span.first = other.first
			} else if span.last != math.MaxUint64 && span.last+1 == other.first {
				span.last = other.last
			} else {
				continue
			}
			span.unwrapped = span.unwrapped || other.unwrapped
			if other.received.Before(span.received) {
				span.received = other.received
			}
			*other = receiveImmediateRange{}
			merged = true
		}
		if !merged {
			return true
		}
	}
}

func (self *receiveImmediateHistory) replay(number uint64, contractId Id) (sequenceAck, bool, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.hasCumulative && number <= self.cumulative.sequenceNumber {
		return self.cumulative, true, true
	}
	for index := range self.maxRanges {
		span := self.ranges[index]
		if span == nil || !span.pending && !span.secured || number < span.first || span.last < number {
			continue
		}
		if span.contractId != contractId || !span.expires.IsZero() && !time.Now().Before(span.expires) {
			return sequenceAck{}, true, false
		}
		return sequenceAck{}, true, span.secured
	}
	return sequenceAck{}, false, false
}

func (self *receiveImmediateHistory) next(floor uint64) (receiveImmediateRange, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	var next receiveImmediateRange
	found := false
	for index := range self.maxRanges {
		span := self.ranges[index]
		if span == nil || !span.pending && !span.secured || span.last < floor {
			continue
		}
		if !found || span.first < next.first {
			next, found = *span, true
		}
	}
	return next, found
}

// Only an actually secured cumulative receipt may prune replay ranges. The
// saved exact boundary also lets a recreated worker answer a past retry safely.
func (self *receiveImmediateHistory) noteCumulative(ack sequenceAck) {
	if self == nil || ack.selective || ack.contractMissing || ack.deliveredImmediately {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.hasCumulative && ack.sequenceNumber <= self.cumulative.sequenceNumber {
		return
	}
	self.cumulative, self.hasCumulative = ack, true
	self.everSecured = true
	for index := range self.maxRanges {
		span := self.ranges[index]
		if span == nil || !span.secured || span.first > ack.sequenceNumber {
			continue
		}
		if span.last <= ack.sequenceNumber {
			*span = receiveImmediateRange{}
		} else if ack.sequenceNumber != math.MaxUint64 {
			span.first = ack.sequenceNumber + 1
		}
	}
}

func (self *ReceiveSequence) sendImmediateDeliveryAck(ack sequenceAck) {
	ack.selective = true
	ack.deliveredImmediately = true
	ack.immediateSequenceNumber = ack.sequenceNumber
	ack.immediateSequenceNumberSet = true
	self.ackWindow.Update(ack)
}

// Called after ordinary identity decoding, and before reorder/Head mutation.
// A future unknown contract is refused, not registered or retained for early
// dispatch. Its reliable sender keeps the original until ordered authority exists.
func (self *ReceiveSequence) tryReceiveImmediate(item *receiveItem) (bool, bool) {
	if !item.deliverImmediately || !item.ack {
		return false, false
	}
	if item.sequenceNumber == math.MaxUint64 || item.messageId != immediateMessageId(self.source.SourceId, self.sequenceId, item.sequenceNumber) {
		return true, false
	}
	if self.session != nil && (item.unwrapped && self.session.RequireEncryption() || !self.session.admitInboundApplication()) {
		return true, false
	}
	if item.sequenceNumber < self.nextSequenceNumber {
		self.ackWindow.ackLock.Lock()
		ack, known := self.ackWindow.headAck, self.ackWindow.hasHeadAck
		self.ackWindow.ackLock.Unlock()
		if !known && self.immediateHistory != nil {
			self.immediateHistory.stateLock.Lock()
			ack, known = self.immediateHistory.cumulative, self.immediateHistory.hasCumulative
			self.immediateHistory.stateLock.Unlock()
		}
		if known {
			self.ackWindow.Update(ack)
		}
		return true, false
	}
	history := self.immediateHistory
	contractId := Id{}
	if item.contractId != nil {
		contractId = *item.contractId
	}
	if history != nil {
		ack, known, secured := history.replay(item.sequenceNumber, contractId)
		if known {
			if secured {
				if ack.messageId != (Id{}) {
					self.ackWindow.Update(ack)
				} else {
					self.sendImmediateDeliveryAck(sequenceAck{sequenceNumber: item.sequenceNumber,
						messageId: item.messageId, tag: item.receiveTag(), receivedAtNanos: item.receivedAtNanos,
						unwrapped: item.unwrapped, transportType: item.transportType})
				}
			}
			return true, false
		}
	}
	if item.head && item.sequenceNumber > self.nextSequenceNumber {
		return false, false
	}
	contract, valid := self.immediateContract(item)
	if !valid {
		if item.sequenceNumber == self.nextSequenceNumber {
			return false, false
		}
		return true, false
	}
	history = self.ensureImmediateHistory()
	if history == nil {
		return true, false
	}
	if item.contractId != nil {
		contractId = *item.contractId
	}
	var expires time.Time
	if contract != nil && contract.expirationTimeUnixMilli != nil {
		expires = time.UnixMilli(*contract.expirationTimeUnixMilli)
	}
	span, token := history.reserve(item.sequenceNumber, contractId, expires, item.receiveTime, item.unwrapped)
	if span == nil {
		return true, false
	}
	if self.immediateDeliveryQueue == nil {
		self.immediateDeliveryQueue = newReceiveDeliveryQueue(self,
			min(receiveImmediateRangeCapacity, max(1, self.receiveBufferSettings.SequenceBufferSize)),
			self.receiveBufferSettings.ReceiveQueueMaxByteCount)
		self.immediateDeliveryQueue.immediate = true
	}
	if self.deliveryQueue == nil {
		self.deliveryQueue = newReceiveDeliveryQueue(self, self.receiveBufferSettings.SequenceBufferSize, self.receiveBufferSettings.ReceiveQueueMaxByteCount)
	}
	q := self.immediateDeliveryQueue
	if !q.admit(item) {
		history.complete(span, token, false)
		return true, false
	}
	q.mutex.Lock()
	receipt := q.admitted[len(q.admitted)-1]
	receipt.immediateHistory, receipt.immediateRange, receipt.immediateToken = history, span, token
	item.immediateReceipt = receipt
	q.mutex.Unlock()
	if !self.updateContract(item) {
		q.discardAdmission(item)
		history.complete(span, token, false)
		return true, false
	}
	self.adoptReceiveMemoryOwner(item)
	self.flushDeliver()
	self.receiveHead(item)
	self.flushDeliver()
	return true, true
}

// The callback's roots remain owned until it seals and every actual claim
// completes. Completion publishes only an exact terminal receipt and returns
// roots; it never advances the ordinary sequence or claims neighboring DATA.
func (q *receiveDeliveryQueue) advanceImmediateLocked() []*receiveItem {
	var returns []*receiveItem
	remaining := q.items[:0]
	for _, r := range q.items {
		if !r.sealed || r.pending != 0 {
			remaining = append(remaining, r)
			continue
		}
		item := r.item
		if item != nil && r.immediateHistory != nil {
			r.secured = r.immediateHistory.complete(r.immediateRange, r.immediateToken, !r.failed)
			if r.secured {
				q.sequence.sendImmediateDeliveryAck(r.ack)
			}
		}
		q.removeChargeLocked(r)
		if item != nil {
			returns = append(returns, item)
			r.item = nil
		}
	}
	clear(q.items[len(remaining):])
	q.items = remaining
	return returns
}

// Consumes only already-secured control ranges at the true ordered floor.
// The ordinary receipt ledger publishes cumulative coverage after any older
// DATA claim is secured. There is no second application callback or debit.
func (self *ReceiveSequence) drainImmediateRanges() {
	if self.immediateHistory == nil {
		return
	}
	for {
		span, ok := self.immediateHistory.next(self.nextSequenceNumber)
		if !ok || !span.secured || span.first > self.nextSequenceNumber || span.last == math.MaxUint64 {
			return
		}
		// Register every older buffered DATA receipt before this range can
		// become a cumulative boundary. Worker floor alone is not delivery.
		self.flushDeliver()
		if self.keyHistoryDeliveryRefused || self.ctx.Err() != nil {
			return
		}
		if self.deliveryQueue == nil {
			self.deliveryQueue = newReceiveDeliveryQueue(self,
				self.receiveBufferSettings.SequenceBufferSize, self.receiveBufferSettings.ReceiveQueueMaxByteCount)
		}
		q := self.deliveryQueue
		q.mutex.Lock()
		r := &receiveDeliveryReceipt{queue: q, control: true, sealed: true, secured: true,
			firstSequenceNumber: self.nextSequenceNumber, ack: sequenceAck{
				sequenceNumber: span.last, messageId: immediateMessageId(self.source.SourceId, self.sequenceId, span.last),
				unwrapped: span.unwrapped, immediateSequenceNumber: span.last, immediateSequenceNumberSet: true}}
		q.items = append(q.items, r)
		self.nextSequenceNumber = span.last + 1
		returns := q.advanceLocked(r)
		q.mutex.Unlock()
		returnReceiveDeliveryItems(returns)
		self.publishReceiveMemoryFloor()
	}
}

func (self *ReceiveSequence) immediateWake() <-chan struct{} {
	if self.immediateDeliveryQueue != nil {
		return self.immediateDeliveryQueue.wake
	}
	return nil
}
