package connect

import (
	"cmp"
	"maps"
	"slices"
	"sync"
	"time"
)

type sequenceAckWindowSnapshot struct {
	ackNotify           <-chan struct{}
	headAck             sequenceAck
	ackUpdateCount      int
	selectiveAcks       map[Id]sequenceAck
	contractMissingAcks map[Id]sequenceAck
}

// sequenceAckWindow is the ACK compression state, independent of timers and
// route writers. Update coalesces cumulative progress and selective evidence.
// takeResponse emits at most one head followed by a bounded oldest-first SACK
// prefix strictly above it; overflow remains available for the next response.
// All methods are safe for concurrent use; there is one draining consumer.
type sequenceAckWindow struct {
	// There is exactly one draining consumer per sequence. A
	// capacity-one signal coalesces any number of updates while that consumer
	// is running and avoids allocating/closing a broadcast channel per packet.
	ackNotify              chan struct{}
	ackLock                sync.Mutex
	headAck                sequenceAck
	hasHeadAck             bool
	ackUpdateCount         int
	headQuietNotify        chan struct{}
	headDeliveredByteCount ByteCount
	headUpdatedAt          time.Time
	selectiveAcks          map[Id]sequenceAck
	// Recovery requests never acknowledge delivery and therefore remain
	// separate from both cumulative and selective acknowledgement windows.
	contractMissingAcks map[Id]sequenceAck
	// A gap is proved once per cumulative head, independently of snapshots.
	// Retain a bounded set of distinct evidence across compression intervals;
	// repeating the same proof must not disable compression under sustained loss.
	// Zero disables early wakes.
	gapNotify             chan struct{}
	gapWakeSelectiveCount int
	gapWakeSignaled       bool
	gapEvidence           []uint64
	// highest selectively acked sequence number; a head below it has
	// selective acks outstanding above it, whether or not they were already
	// written, so the next head advance is a hole filling
	gapSelectiveMax uint64
}

func newSequenceAckWindow() *sequenceAckWindow {
	return newSequenceAckWindowWithGapWake(0)
}

func newSequenceAckWindowWithGapWake(gapWakeSelectiveCount int) *sequenceAckWindow {
	return &sequenceAckWindow{
		ackNotify:             make(chan struct{}, 1),
		headQuietNotify:       make(chan struct{}, 1),
		ackUpdateCount:        0,
		selectiveAcks:         map[Id]sequenceAck{},
		contractMissingAcks:   map[Id]sequenceAck{},
		gapNotify:             make(chan struct{}, 1),
		gapWakeSelectiveCount: min(gapWakeSelectiveCount, ackResponseMaxCount),
	}
}

// Notify is the stable coalesced edge consumed by the one sequence worker.
// It is safe to fetch without a lock because the channel never changes.
func (self *sequenceAckWindow) Notify() <-chan struct{} {
	return self.ackNotify
}

// GapNotify is the early-wake edge for the consumer's compression wait. Like
// Notify it never changes, so it is safe to fetch without a lock.
func (self *sequenceAckWindow) GapNotify() <-chan struct{} {
	return self.gapNotify
}

// signalGapWakeWithLock fires once per cumulative head.
func (self *sequenceAckWindow) signalGapWakeWithLock() {
	if self.gapWakeSignaled {
		return
	}
	self.gapWakeSignaled = true
	select {
	case self.gapNotify <- struct{}{}:
	default:
	}
}

// Pending checks whether a worker can proceed without constructing a
// snapshot. The ACK-compression worker uses this before its wait and extracts
// only a bounded response when it actually drains the window.
func (self *sequenceAckWindow) Pending() bool {
	self.ackLock.Lock()
	defer self.ackLock.Unlock()
	return 0 < self.ackUpdateCount ||
		0 < len(self.selectiveAcks) ||
		0 < len(self.contractMissingAcks)
}

// PendingDispositionFor reports whether the not-yet-snapshotted window can
// retire or materially rewrite one exact due item. Unrelated ACK progress must
// not postpone its recovery: on a busy sequence, duplicate/newer selective
// ACKs can otherwise keep Pending true indefinitely while the actual hole is
// never retransmitted.
func (self *sequenceAckWindow) PendingDispositionFor(
	sequenceNumber uint64,
	messageId Id,
) bool {
	self.ackLock.Lock()
	defer self.ackLock.Unlock()
	if 0 < self.ackUpdateCount && self.hasHeadAck &&
		sequenceNumber <= self.headAck.sequenceNumber {
		return true
	}
	if ack, ok := self.selectiveAcks[messageId]; ok &&
		ack.sequenceNumber == sequenceNumber {
		return true
	}
	_, contractMissing := self.contractMissingAcks[messageId]
	return contractMissing
}

func (self *sequenceAckWindow) UpdateContractMissing(ack sequenceAck) {
	self.ackLock.Lock()
	defer self.ackLock.Unlock()
	if prior, ok := self.contractMissingAcks[ack.messageId]; ok {
		if prior.unwrapped {
			ack.unwrapped = true
		}
		if prior.compactContractRecoverySupported {
			ack.compactContractRecoverySupported = true
		}
		if ack.transportType == TransportTypeUnknown {
			ack.transportType = prior.transportType
		}
	}
	self.contractMissingAcks[ack.messageId] = ack
	select {
	case self.ackNotify <- struct{}{}:
	default:
	}
}

// Adds feedback without delivery credit, including retransmitted heads and SACKs.
func (self *sequenceAckWindow) Update(ack sequenceAck) {
	self.update(ack, 0)
}

// First delivery can earn bounded early-head credit on H1. The count is
// consumed here rather than retained in every ACK record or selective entry.
func (self *sequenceAckWindow) UpdateDelivered(ack sequenceAck, deliveredByteCount ByteCount) {
	if ack.transportType != TransportTypeH1 {
		deliveredByteCount = 0
	}
	self.update(ack, deliveredByteCount)
}

// Serializes feedback and first-delivery credit under the compressor's lock.
func (self *sequenceAckWindow) update(ack sequenceAck, deliveredByteCount ByteCount) {
	self.ackLock.Lock()
	defer self.ackLock.Unlock()

	if !self.hasHeadAck || self.headAck.sequenceNumber < ack.sequenceNumber {
		if ack.selective {
			if prior, ok := self.selectiveAcks[ack.messageId]; ok {
				// Exact receiver timing is handled before byte application.
				// A duplicate without metadata cannot re-enable legacy sampling.
				ack.receiverTiming = ack.receiverTiming || prior.receiverTiming
				if prior.unwrapped {
					// Coalesced selective Ack for the same message preserves any
					// prior plaintext bit so one late wrapped resend cannot upgrade
					// the Ack format past the sender's reach.
					ack.unwrapped = true
				}
				if prior.compactContractRecoverySupported {
					ack.compactContractRecoverySupported = true
				}
				if ack.transportType == TransportTypeUnknown {
					ack.transportType = prior.transportType
				}
			}
			self.selectiveAcks[ack.messageId] = ack
			if self.gapSelectiveMax < ack.sequenceNumber {
				self.gapSelectiveMax = ack.sequenceNumber
			}
			if 0 < self.gapWakeSelectiveCount && !self.gapWakeSignaled &&
				!slices.Contains(self.gapEvidence, ack.sequenceNumber) {
				self.gapEvidence = append(self.gapEvidence, ack.sequenceNumber)
				if self.gapWakeSelectiveCount <= len(self.gapEvidence) {
					self.signalGapWakeWithLock()
				}
			}
		} else {
			// a head advancing under outstanding selective acks is a hole
			// filling; the sender's flight is head-blocked on this ack
			self.gapWakeSignaled = false
			self.gapEvidence = self.gapEvidence[:0]
			if 0 < self.gapWakeSelectiveCount && self.gapSelectiveMax != 0 &&
				(!self.hasHeadAck || self.headAck.sequenceNumber < self.gapSelectiveMax) {
				self.signalGapWakeWithLock()
			}
			// cumulative head ack: or-in the prior head's plaintext bit
			// (and any absorbed selective acks below the new head) so a
			// single plaintext pack anywhere under the head keeps the
			// ack plaintext. Selective acks at or below the new head are
			// already dropped by the Snapshot pass.
			if self.hasHeadAck && self.headAck.unwrapped {
				ack.unwrapped = true
			}
			if self.hasHeadAck && self.headAck.compactContractRecoverySupported {
				ack.compactContractRecoverySupported = true
			}
			if self.hasHeadAck && ack.transportType == TransportTypeUnknown {
				ack.transportType = self.headAck.transportType
			}
			if !ack.unwrapped {
				for _, sel := range self.selectiveAcks {
					if sel.unwrapped && sel.sequenceNumber <= ack.sequenceNumber {
						ack.unwrapped = true
						break
					}
				}
			}
			if !ack.compactContractRecoverySupported {
				for _, selectiveAck := range self.selectiveAcks {
					if selectiveAck.compactContractRecoverySupported &&
						selectiveAck.sequenceNumber <= ack.sequenceNumber {
						ack.compactContractRecoverySupported = true
						break
					}
				}
			}
			self.ackUpdateCount += 1
			if 0 < deliveredByteCount || 0 < self.headDeliveredByteCount {
				self.headUpdatedAt = time.Now()
			}
			priorDeliveredByteCount := self.headDeliveredByteCount
			self.headDeliveredByteCount = min(ackBurstTailByteCount,
				self.headDeliveredByteCount+min(ackBurstTailByteCount, max(0, deliveredByteCount)))
			if priorDeliveredByteCount < ackBurstTailByteCount && self.headDeliveredByteCount >= ackBurstTailByteCount {
				select {
				case self.headQuietNotify <- struct{}{}:
				default:
				}
			}
			if prior, ok := self.selectiveAcks[ack.messageId]; ok {
				ack.receiverTiming = ack.receiverTiming || prior.receiverTiming
			}
			self.headAck = ack
			self.hasHeadAck = true
			// no need to clean up `selectiveAcks` here
			// selective acks with sequence number <= head are ignored in a final pass during update
		}
	} else {
		if self.headAck.messageId == ack.messageId && ack.receiverTiming {
			self.headAck.receiverTiming = true
		}
		// past the head
		// resend the head — fold this late ack's plaintext bit into the
		// head so the resend covers it. Snapshots copy the value, so the
		// internal value can be updated under ackLock without a published
		// pointer or copy-on-write allocation.
		if ack.unwrapped && self.hasHeadAck && !self.headAck.unwrapped {
			self.headAck.unwrapped = true
		}
		if ack.compactContractRecoverySupported && self.hasHeadAck &&
			!self.headAck.compactContractRecoverySupported {
			self.headAck.compactContractRecoverySupported = true
		}
		self.ackUpdateCount += 1
	}

	select {
	case self.ackNotify <- struct{}{}:
	default:
	}
}

// Snapshot is returned by value: it is consumed immediately by the caller and
// never retained, so a heap allocation per snapshot is pure waste. The caller
// always receives a copy of (or nil for) the selective acks, never the live
// map, so the live map can be cleared and reused on reset.
func (self *sequenceAckWindow) Snapshot(reset bool) sequenceAckWindowSnapshot {
	self.ackLock.Lock()
	defer self.ackLock.Unlock()

	// build the selective-ack copy lazily so the common in-order case (a
	// cumulative head ack with no selective acks) allocates no map.
	var selectiveAcksAfterHead map[Id]sequenceAck
	if 0 < self.ackUpdateCount {
		for messageId, ack := range self.selectiveAcks {
			if self.headAck.sequenceNumber < ack.sequenceNumber {
				if selectiveAcksAfterHead == nil {
					selectiveAcksAfterHead = map[Id]sequenceAck{}
				}
				selectiveAcksAfterHead[messageId] = ack
			}
		}
	} else if 0 < len(self.selectiveAcks) {
		selectiveAcksAfterHead = maps.Clone(self.selectiveAcks)
	}

	var contractMissingAcks map[Id]sequenceAck
	if 0 < len(self.contractMissingAcks) {
		contractMissingAcks = maps.Clone(self.contractMissingAcks)
	}

	snapshot := sequenceAckWindowSnapshot{
		ackNotify:           self.ackNotify,
		headAck:             self.headAck,
		ackUpdateCount:      self.ackUpdateCount,
		selectiveAcks:       selectiveAcksAfterHead,
		contractMissingAcks: contractMissingAcks,
	}

	if reset {
		// keep the head ack in place. clear() reuses the live map's storage
		// instead of allocating a fresh map; the caller holds only a copy.
		self.ackUpdateCount = 0
		self.headDeliveredByteCount = 0
		select {
		case <-self.headQuietNotify:
		default:
		}
		clear(self.selectiveAcks)
		clear(self.contractMissingAcks)
		// The signals correspond to state included in this snapshot. Drain
		// them while ackLock excludes Update so the next empty snapshot cannot
		// wake on a stale token. Gap evidence survives snapshots.
		select {
		case <-self.ackNotify:
		default:
		}
		select {
		case <-self.gapNotify:
		default:
		}
	}

	return snapshot
}

// takeResponse removes only the bounded response being sent. The caller owns
// scratch; overflow stays pending and new cumulative progress can absorb it.
// Selective entries are ordered even across responses, so a partial write
// cannot manufacture multiple holes at the sender.
func (self *sequenceAckWindow) takeResponse(scratch []sequenceAck, limit int) ([]sequenceAck, bool) {
	self.ackLock.Lock()
	defer self.ackLock.Unlock()
	limit = min(limit, ackResponseMaxCount)
	if limit <= 0 {
		return scratch[:0], self.ackUpdateCount != 0 || len(self.selectiveAcks) != 0 || len(self.contractMissingAcks) != 0
	}
	acks := scratch[:0]
	if self.ackUpdateCount != 0 {
		acks = append(acks, self.headAck)
		self.ackUpdateCount = 0
		self.headDeliveredByteCount = 0
		select {
		case <-self.headQuietNotify:
		default:
		}
	}
	headCount := len(acks)
	for id, ack := range self.selectiveAcks {
		if self.hasHeadAck && ack.sequenceNumber <= self.headAck.sequenceNumber {
			delete(self.selectiveAcks, id)
			continue
		}
		ack.messageId, ack.selective = id, true
		index, _ := slices.BinarySearchFunc(acks[headCount:], ack, func(a, b sequenceAck) int {
			return cmp.Compare(a.sequenceNumber, b.sequenceNumber)
		})
		index += headCount
		if index < limit {
			if len(acks) == limit {
				acks = acks[:limit-1]
			}
			acks = slices.Insert(acks, index, ack)
		}
	}
	for _, ack := range acks[headCount:] {
		delete(self.selectiveAcks, ack.messageId)
	}
	for id, ack := range self.contractMissingAcks {
		if len(acks) == limit {
			break
		}
		ack.messageId, ack.contractMissing = id, true
		acks = append(acks, ack)
		delete(self.contractMissingAcks, id)
	}
	select {
	case <-self.ackNotify:
	default:
	}
	select {
	case <-self.gapNotify:
	default:
	}
	if len(self.selectiveAcks) != 0 || len(self.contractMissingAcks) != 0 {
		self.ackNotify <- struct{}{}
	}
	return acks, len(self.selectiveAcks) != 0 || len(self.contractMissingAcks) != 0
}

// One early head's complete encoded Transfer frame, including legacy and
// encrypted wrapping, is at most ackResponseEntryMaxByteCount. Requiring one
// hundred times that many freshly delivered bytes bounds additional encoded
// head feedback at one percent of cumulative progress.
const ackBurstTailByteCount = 100 * ackResponseEntryMaxByteCount

// Removes only a sufficiently large cumulative burst's head once arrivals
// have gone quiet. The ordinary response deadline still owns above-head
// SACKs, missing-contract requests and eviction metadata.
func (self *sequenceAckWindow) takeQuietHead(now time.Time, quiet time.Duration) (sequenceAck, bool, time.Time) {
	self.ackLock.Lock()
	defer self.ackLock.Unlock()
	if self.ackUpdateCount == 0 || self.headDeliveredByteCount < ackBurstTailByteCount {
		return sequenceAck{}, false, time.Time{}
	}
	deadline := self.headUpdatedAt.Add(quiet)
	if now.Before(deadline) {
		return sequenceAck{}, false, deadline
	}
	ack := self.headAck
	self.ackUpdateCount = 0
	self.headDeliveredByteCount = 0
	select {
	case <-self.headQuietNotify:
	default:
	}
	select {
	case <-self.ackNotify:
	default:
	}
	for id, selective := range self.selectiveAcks {
		if selective.sequenceNumber <= ack.sequenceNumber {
			delete(self.selectiveAcks, id)
		}
	}
	return ack, true, time.Time{}
}

// Signals only the transition to one eligible cumulative byte quantum.
// Further arrivals move the checked quiet deadline without waking per Pack.
func (self *sequenceAckWindow) HeadQuietNotify() <-chan struct{} {
	return self.headQuietNotify
}
