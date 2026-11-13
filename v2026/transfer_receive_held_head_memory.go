// Contract recovery cannot depend on capacity occupied by its original. A
// SACK prepays bounded duplicate metadata; a tentative consumer original may
// instead fund a full contract proof in its bounded sibling.
package connect

import "github.com/urnetwork/connect/v2026/protocol"

// The root admission lock guards every field. Eligible originals keep the
// escrow until actual return, so proof verification and payload callbacks can
// both finish without a release/reacquire gap. One active copy also serializes
// a separately funded consumer-child repair; that copy cannot refund RX.
type receiveHeldHeadRepair struct {
	reservedByteCount ByteCount
	eligibleCount     int
	owner             *decodedPackOwner
}

// The construction policy is observable before traffic. No-contract peers
// need no proof root; nonpositive disables new custody commitments.
func (self *ReceiveBufferSettings) receiveHeldHeadRepairByteCount(noContract bool) ByteCount {
	limit := max(0, self.ReceiveHeadContractRepairByteCount)
	if noContract {
		return min(limit, decodedPackOwnerQueueByteCount)
	}
	return limit
}

// The worker can preflight a whole commitment batch before any ACK. A known
// proof must fit the configured copied-proof envelope, including its owner.
func (self *ReceiveSequence) receiveHeldHeadRepairLimit(item *receiveItem) (ByteCount, bool) {
	if item == nil || item.head || !item.ack ||
		self.receiveQueue == nil || !self.receiveQueue.lifetimeBudget || self.receiveQueue.budget == nil || self.client == nil {
		return 0, true
	}
	owner := item.decodedOwner
	budget := self.receiveQueue.budget
	noContract := self.client.ContractManager() != nil && self.client.ContractManager().ReceiveNoContract(self.source.SourceId)
	limit := self.receiveBufferSettings.receiveHeldHeadRepairByteCount(noContract)
	if owner == nil || item != &owner.receiveItem || item.memoryBudget != budget || limit <= 0 {
		return 0, false
	}
	minimum := decodedPackOwnerQueueByteCount
	if item.contractFrame != nil {
		minimum = addReceiveQueueByteCount(minimum, receiveMemoryScanCopyRoot(len(item.contractFrame.MessageBytes)))
	}
	if limit < minimum {
		return 0, false
	}
	return limit, true
}

// Acknowledged tails fund metadata-only head promotion before promising
// custody. Contract peers also fund one bounded copied proof; explicit
// no-contract peers need only the inline owner.
func (self *ReceiveSequence) reserveReceiveHeldHeadRepair(item *receiveItem) bool {
	limit, ok := self.receiveHeldHeadRepairLimit(item)
	if !ok || limit == 0 {
		return ok
	}
	owner := item.decodedOwner
	budget := self.receiveQueue.budget
	root := budget.admissionRoot()
	root.admissionLock.Lock()
	defer root.admissionLock.Unlock()
	pack := &owner.receivePack
	state := &self.heldHeadRepair
	if self.holdPromise.closed || self.ctx.Err() != nil || pack.ingressSequence != self || !pack.receiveMemoryOwned {
		return false
	}
	if pack.heldHeadRepairEligible {
		return true
	}
	if state.eligibleCount == 0 {
		if state.owner != nil || state.reservedByteCount != 0 || !budget.tryReserveWithLock(limit) {
			return false
		}
		state.reservedByteCount = limit
	}
	state.eligibleCount++
	pack.heldHeadRepairEligible = true
	return true
}

// Borrows carrier. The normal scanner selects source, session, generation and
// lane first. This path supplies physical custody only; the worker still
// verifies the proof and keeps the original's payload, tag and receipt.
func (self *ReceiveBuffer) takeReceiveHeldHeadRepairOwner(sequence *ReceiveSequence, scan receiveMemoryPackScan, carrier []byte) *decodedPackOwner {
	budget := self.receiveBufferSettings.ReceiveQueueBudget
	if sequence == nil || budget == nil || sequence.receiveQueue.budget != budget ||
		!scan.loanSafe || scan.compatibility || !scan.head || scan.nack || scan.hasEncryptedControl || scan.retainedByteCount <= 0 {
		return nil
	}
	// The duplicate needs no application roots or overflow frame descriptors.
	// Its borrowed carrier returns before handoff, so only the inline owner
	// and copied proof remain live after this receive-pump turn.
	byteCount := decodedPackOwnerQueueByteCount
	cursor := receiveMemoryWireCursor{bytes: carrier[scan.packOffset : scan.packOffset+scan.packByteCount]}
	for cursor.offset < len(cursor.bytes) {
		field, ok := cursor.next()
		if !ok {
			return nil
		}
		if field.number == 7 {
			_, _, _, roots, safe, valid := receiveMemoryScanFrameBound(field.bytes, 0)
			if !safe || !valid {
				return nil
			}
			byteCount = addReceiveQueueByteCount(byteCount, roots)
		}
	}
	root := budget.admissionRoot()
	root.admissionLock.Lock()
	defer root.admissionLock.Unlock()
	state := &sequence.heldHeadRepair
	if sequence.holdPromise.closed || sequence.ctx.Err() != nil || state.owner != nil ||
		scan.sequenceNumber < sequence.holdPromise.ingressFloor {
		return nil
	}
	var original *ReceivePack
	for pack := sequence.holdPromise.ingressHead; pack != nil; pack = pack.ingressNext {
		// These flags publish after decoding under this root lock. Return
		// clears frame/proof roots first, but leaves the Pack pointer and its
		// Nack/Head/ContractAhead scalars intact until root-locked unlink.
		if pack.receiveMemoryOwned && pack.decodedOwner != nil &&
			(pack.heldHeadRepairEligible || pack.receiveConsumerData) && pack.Pack != nil && pack.Pack.ContractAhead == scan.contractAhead {
			item := &pack.decodedOwner.receiveItem
			if item.messageId == scan.messageId && item.sequenceNumber == scan.sequenceNumber {
				// A queued duplicate may precede its committed original in
				// this intrusive list. Always preserve that original's paid
				// escrow path before considering fresh consumer-child bytes.
				if pack.heldHeadRepairEligible {
					original = pack
					break
				}
				if original == nil || !original.receiveConsumerData && pack.receiveConsumerData {
					original = pack
				}
			}
		}
	}
	if original == nil {
		return nil
	}
	if original.heldHeadRepairEligible {
		if state.eligibleCount == 0 || state.reservedByteCount < byteCount {
			return nil
		}
		state.reservedByteCount -= byteCount
	} else {
		// A replacement receiver can own an unacknowledged future original
		// after the sender has already received its earlier prefix ACKs. Its
		// genuine promoted head with a full contract proof may fund metadata
		// in the bounded sibling, preserving every committed repair escrow.
		budget = self.receiveBufferSettings.receiveHeadRecoveryBudget()
		if budget == nil || !sequence.receiveQueue.lifetimeBudget || !original.receiveConsumerData ||
			original.Pack.Nack || original.Pack.Head || scan.contractAhead || scan.frameCount == 0 ||
			scan.consumerFrameCount != scan.frameCount || !receiveConsumerRecoverySession(scan) ||
			!scan.contractFrameSet || scan.contractFrameType != int32(protocol.MessageType_TransferContract) ||
			budget.totalByteCount.Load() > sequence.receiveQueue.budget.totalByteCount.Load() ||
			!budget.tryReserveWithLock(byteCount) {
			return nil
		}
	}
	owner := inboundDecodedPackOwners.take()
	owner.receiveItem.transferItem = transferItem{messageId: scan.messageId, sequenceNumber: scan.sequenceNumber,
		memoryBudget: budget, queueByteCount: byteCount}
	pack := &owner.receivePack
	pack.decodedOwner, pack.receiveMemoryOwned = owner, true
	pack.MessageByteCount = scan.messageByteCount
	sequence.registerReceiveIngressWithLock(pack)
	state.owner = owner
	return owner
}

// Borrows body; preflight already validated every skipped payload field. Using
// exact held identity makes the original the sole delivery owner, including
// its tag and provider receipt. The new proof still follows ordinary MAC/path
// verification; a compact id still resolves only through the verified map.
func decodeReceiveHeldHeadRepair(body []byte, scan receiveMemoryPackScan, owner *decodedPackOwner) bool {
	owner.messageId, owner.sequenceId, owner.contractId = scan.messageId, scan.sequenceId, scan.contractId
	owner.pack = protocol.Pack{MessageId: owner.messageId[:], SequenceId: owner.sequenceId[:],
		SequenceNumber: scan.sequenceNumber, Head: scan.head, ForceStream: scan.forceStream,
		CompanionContract: scan.companionContract, LogicalLane: scan.logicalLane, ContractAhead: scan.contractAhead}
	if scan.contractIdSet {
		owner.pack.ContractId = owner.contractId[:]
	}
	cursor := receiveMemoryWireCursor{bytes: body}
	for cursor.offset < len(body) {
		field, ok := cursor.next()
		if !ok {
			return false
		}
		if field.number == 7 {
			if !decodeFrameInto(field.bytes, &owner.contractFrame) {
				return false
			}
			owner.pack.ContractFrame = &owner.contractFrame
		}
	}
	return true
}

// A tentative original can disappear after its repair was decoded but before
// the worker receives it. The proof-only copy can never become fresh payload.
func (self *ReceiveSequence) receiveHeldHeadRepairCopy(pack *ReceivePack) bool {
	if pack == nil || pack.decodedOwner == nil || self.receiveQueue == nil || self.receiveQueue.budget == nil {
		return false
	}
	root := self.receiveQueue.budget.admissionRoot()
	root.admissionLock.Lock()
	defer root.admissionLock.Unlock()
	return self.heldHeadRepair.owner == pack.decodedOwner
}

// Roots have returned before this call. Only this escrow's exact RX owner may
// replenish it; provider children, prefix loans and downstream credits remain
// independent. The final original releases unused capacity after its callback.
func (self *ReceiveSequence) returnReceiveHeldHeadRepairWithLock(owner *decodedPackOwner, budget *TransferMemoryBudget, byteCount ByteCount) ByteCount {
	state := &self.heldHeadRepair
	if state.owner == owner {
		state.owner = nil
		if budget != self.receiveQueue.budget {
			return byteCount
		}
		if state.eligibleCount != 0 && !self.holdPromise.closed && self.ctx.Err() == nil {
			state.reservedByteCount += byteCount
			return 0
		}
		return byteCount
	}
	if budget != self.receiveQueue.budget {
		return byteCount
	}
	if owner.receivePack.heldHeadRepairEligible {
		owner.receivePack.heldHeadRepairEligible = false
		state.eligibleCount--
		if state.eligibleCount == 0 {
			byteCount += state.reservedByteCount
			state.reservedByteCount = 0
		}
	}
	return byteCount
}

// Sequence cancellation withdraws unused capacity. Any already-decoded repair
// remains funded until its own queued/refused owner really returns.
func (self *ReceiveSequence) releaseReceiveHeldHeadRepairWithLock() ByteCount {
	reserved := self.heldHeadRepair.reservedByteCount
	self.heldHeadRepair.reservedByteCount = 0
	self.receiveQueue.budget.releaseWithLock(reserved)
	return reserved
}
