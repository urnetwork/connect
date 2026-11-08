// A current consumer head may exceed its prefix estimate, or have no promise
// while paid tentative tails occupy RX. A shared child pays the whole owner
// without spending those tails or their credit. Provider TCP heads also prepay
// downstream owners; a promised UDP head must not strand those TCP tails.
// Ancestor caps and ordinary proof, packet-policy and delivery checks remain.
package connect

import "github.com/urnetwork/connect/protocol"

// Budget identities remain immutable while live owners drain. Capacity is
// checked only at admission, so a later shrink cannot invalidate paid custody.
func (self *ReceiveBufferSettings) receiveHeadRecoveryBudget() *TransferMemoryBudget {
	rx, recovery := self.ReceiveQueueBudget, self.ReceiveHeadRecoveryBudget
	if rx == nil || recovery == nil || rx == recovery || rx.Parent() == nil || recovery.Parent() != rx.Parent() {
		return nil
	}
	return recovery
}

// This fallback is fresh physical allocation, never proof authority or a
// prefix/pressure loan. The ordinary worker still verifies contracts before
// delivery; invalid proof keeps its existing error and close behavior.
func (self *ReceiveBuffer) takeReceiveHeadRecoveryOwner(sequence *ReceiveSequence, scan receiveMemoryPackScan) *decodedPackOwner {
	return self.takeReceiveHeadRecoveryOwnerInSession(sequence, scan, false)
}

// The decoder supplies its actual decrypted context. A plaintext retry cannot
// use the new established-generation fallback to cross Required encryption.
func (self *ReceiveBuffer) takeReceiveHeadRecoveryOwnerInSession(sequence *ReceiveSequence, scan receiveMemoryPackScan, decrypted bool) *decodedPackOwner {
	budget := self.receiveBufferSettings.receiveHeadRecoveryBudget()
	if budget == nil || sequence == nil || !scan.loanSafe || scan.compatibility || scan.nack || scan.contractAhead ||
		scan.hasEncryptedControl || scan.retainedByteCount <= 0 {
		return nil
	}
	consumer := scan.frameCount != 0 && scan.consumerFrameCount == scan.frameCount
	provider := scan.frameCount != 0 && scan.providerFrameCount == scan.frameCount &&
		self.client != nil && self.client.reliableProviderIngress.Load()
	opening := receiveConsumerOpeningHeadScan(scan)
	if !consumer && !provider && !opening {
		return nil
	}
	rx := self.receiveBufferSettings.ReceiveQueueBudget
	if sequence.receiveQueue.budget != rx || !sequence.receiveQueue.lifetimeBudget {
		return nil
	}
	root := budget.admissionRoot()
	root.admissionLock.Lock()
	defer root.admissionLock.Unlock()
	promise := &sequence.holdPromise
	promised := promise.set && scan.sequenceNumber < promise.boundary
	// Published current-prefix owners already pay for the scheduler gap before
	// the receive worker advances its floor. Their immediate successors may
	// use the same bounded child without turning future tails into authority.
	consumerPrefix := consumer && !promise.closed && sequence.ctx.Err() == nil &&
		receiveConsumerRecoverySession(scan) && sequence.receiveConsumerRecoveryPrefixWithLock(scan.sequenceNumber, budget)
	// A verified ordered generation can be quiet while other sequences fill
	// RX. Its exact current original has the same bounded physical need as a
	// missing predecessor, even without a later owner in its own list. It
	// reserves independently; foreign RX promises and escrows stay untouched.
	establishedCurrent := consumer && promise.establishedContract &&
		scan.sequenceNumber == promise.ingressFloor && !scan.head &&
		!scan.contractIdSet && !scan.contractFrameSet && receiveConsumerRecoverySession(scan) &&
		(decrypted || sequence.session == nil || !sequence.session.RequireEncryption())
	allowed := provider && promised ||
		consumer && (promised || consumerPrefix || establishedCurrent || sequence.receiveConsumerHeadBacklogWithLock(scan)) ||
		opening && sequence.receiveConsumerOpeningBacklogWithLock()
	if promise.closed || sequence.ctx.Err() != nil ||
		scan.sequenceNumber != promise.ingressFloor && !consumerPrefix ||
		!allowed ||
		sequence.receiveIngressDuplicateWithLock(scan.messageId, scan.sequenceNumber) ||
		budget.totalByteCount.Load() > rx.totalByteCount.Load() || !budget.tryReserveWithLock(scan.retainedByteCount) {
		return nil
	}
	owner := inboundDecodedPackOwners.take()
	owner.receiveItem.transferItem = transferItem{messageId: scan.messageId, sequenceNumber: scan.sequenceNumber,
		memoryBudget: budget, queueByteCount: scan.retainedByteCount}
	pack := &owner.receivePack
	pack.decodedOwner, pack.receiveMemoryOwned = owner, true
	sequence.registerReceiveIngressWithLock(pack)
	return owner
}

// The root admission lock keeps paid ingress identities stable through return.
// Only a consumer's exact current original can recover without a promise, and
// only when this same sequence already owns later retriable data. The storage
// grants no contract or head authority; the ordinary worker still checks both.
// The copied publication flag avoids reading Pack during decode or return.
func (self *ReceiveSequence) receiveConsumerHeadBacklogWithLock(scan receiveMemoryPackScan) bool {
	if !scan.loanSafe || scan.compatibility || scan.nack || scan.contractAhead || scan.hasEncryptedControl ||
		scan.frameCount == 0 || scan.consumerFrameCount != scan.frameCount || scan.sequenceNumber != self.holdPromise.ingressFloor {
		return false
	}
	for pack := self.holdPromise.ingressHead; pack != nil; pack = pack.ingressNext {
		if pack.ingressSequence == self && pack.receiveMemoryOwned && pack.receivePublishedReliable &&
			pack.decodedOwner != nil && scan.sequenceNumber < pack.decodedOwner.receiveItem.sequenceNumber {
			return true
		}
	}
	return false
}

// The admission root lock protects every borrowed owner and copied flag.
// One word proves distinct consecutive positions, independent of list order or
// duplicates. Reject a distant wire index before scanning the existing list.
// This buys physical storage only: a failed predecessor still makes future
// children follow ordinary refusal/refund, never selective ACK or proof credit.
func (self *ReceiveSequence) receiveConsumerRecoveryPrefixWithLock(number uint64, budget *TransferMemoryBudget) bool {
	floor := self.holdPromise.ingressFloor
	if number <= floor || number-floor > 64 {
		return false
	}
	span := number - floor
	required := ^uint64(0) >> (64 - span)
	var present uint64
	visits := 0
	for pack := self.holdPromise.ingressHead; pack != nil; pack = pack.ingressNext {
		// This opportunistic path cannot turn a large retained flight into
		// unbounded root-lock work. An incomplete census keeps the old refusal.
		if visits == 256 {
			return false
		}
		visits++
		if pack.ingressSequence != self || !pack.receiveMemoryOwned || !pack.receivePublishedReliable ||
			!pack.receiveConsumerData || pack.decodedOwner == nil || pack.ingressLoanByteCount != 0 {
			continue
		}
		item := &pack.decodedOwner.receiveItem
		if item.memoryBudget != budget || item.sequenceNumber < floor || number <= item.sequenceNumber {
			continue
		}
		present |= uint64(1) << (item.sequenceNumber - floor)
		if present == required {
			return true
		}
	}
	return false
}

// An unknown wire role cannot select the default local session for a newly
// funded control or proof repair. Ordinary funded decoding keeps its existing
// compatibility behavior.
func receiveConsumerRecoverySession(scan receiveMemoryPackScan) bool {
	if !scan.sessionRoleSet {
		return true
	}
	_, ok := sequenceTlsRoleFromProtobuf(protocol.SequenceRole(scan.sessionRole))
	return ok
}

// Only the actual empty full opening at zero can derive its consumer role
// from later paid data. Neither a promise nor the absence of frames supplies
// that role. Contract verification remains entirely with the worker.
func receiveConsumerOpeningHeadScan(scan receiveMemoryPackScan) bool {
	return scan.loanSafe && !scan.compatibility && scan.head && !scan.nack && !scan.contractAhead &&
		!scan.hasEncryptedControl && scan.sequenceNumber == 0 && scan.frameCount == 0 &&
		scan.contractFrameSet && scan.contractFrameType == int32(protocol.MessageType_TransferContract) &&
		!scan.contractIdSet && receiveConsumerRecoverySession(scan)
}

// The copied role bit remains immutable until the paid owner unlinks under
// this root lock. Its decoded frames may already have returned to their pool.
func (self *ReceiveSequence) receiveConsumerOpeningBacklogWithLock() bool {
	for pack := self.holdPromise.ingressHead; pack != nil; pack = pack.ingressNext {
		if pack.ingressSequence == self && pack.receiveMemoryOwned && pack.receiveConsumerData &&
			pack.decodedOwner != nil && pack.decodedOwner.receiveItem.sequenceNumber > 0 {
			return true
		}
	}
	return false
}

// A provider gap can decode within its smaller promise but fail to prepay the
// downstream owner. Fund the whole actual decoded cost in the sibling child
// before publishing acceptance. Restoring the existing RX loan is an atomic
// accounting transfer: those bytes stay paid as the missing-prefix promise.
// No already-committed item moves. Borrowed pressure credit is restored to its
// original RX escrow rather than released or relabeled as child ownership.
func (self *ReceiveSequence) recoverProviderReceiveDeliveryCredit(item *receiveItem, additional ByteCount) bool {
	budget := self.receiveBufferSettings.receiveHeadRecoveryBudget()
	rx := self.receiveBufferSettings.ReceiveQueueBudget
	if budget == nil || item == nil || additional <= 0 || self.client == nil || !self.client.reliableProviderIngress.Load() ||
		self.receiveQueue == nil || !self.receiveQueue.lifetimeBudget || self.receiveQueue.budget != rx ||
		item.memoryBudget != rx || !item.ack || item.committed || item.deliveryPrepared || item.decodedOwner == nil ||
		!receiveProviderHeadRecoveryFrames(item.frames) {
		return false
	}
	owner := item.decodedOwner
	pack := &owner.receivePack
	if !pack.receiveMemoryOwned || pack.decodedOwner != owner || &owner.receiveItem != item || pack.ingressSequence != self {
		return false
	}
	if !receiveProviderHeadRecoveryPack(pack.Pack) {
		return false
	}
	full, ok := receiveMemoryScanAdd(item.QueueByteCount(), additional)
	if !ok {
		return false
	}
	root := budget.admissionRoot()
	root.admissionLock.Lock()
	released := false
	defer func() {
		root.admissionLock.Unlock()
		if released {
			rx.notifyCapacityChanged()
		}
	}()
	promise := &self.holdPromise
	state := &self.memoryPressure
	loan := pack.ingressLoanByteCount
	if promise.closed || self.ctx.Err() != nil || !promise.set || item.sequenceNumber != promise.ingressFloor ||
		promise.boundary <= item.sequenceNumber || budget.totalByteCount.Load() > rx.totalByteCount.Load() ||
		state.reclaimOwner == owner || loan < 0 || loan > item.QueueByteCount() ||
		loan > promise.ingressLoanByteCount || full > budget.totalByteCount.Load() {
		return false
	}
	pressureLoan := ByteCount(0)
	if state.owner == owner {
		if !state.activeSet || !state.ready || state.active.messageId != item.messageId || state.active.sequenceNumber != item.sequenceNumber ||
			state.borrowedByteCount < 0 || item.QueueByteCount()-loan < state.borrowedByteCount {
			return false
		}
		pressureLoan = state.borrowedByteCount
	}
	prefix, prefixOk := receiveMemoryScanAdd(promise.reservedByteCount, loan)
	escrow, escrowOk := receiveMemoryScanAdd(state.reservedByteCount, pressureLoan)
	if !prefixOk || !escrowOk {
		return false
	}
	registered := false
	for current := promise.ingressHead; current != nil; current = current.ingressNext {
		if current == pack {
			registered = true
			continue
		}
		if current.decodedOwner == nil {
			return false
		}
		other := &current.decodedOwner.receiveItem
		if other.messageId == item.messageId || other.sequenceNumber == item.sequenceNumber {
			return false
		}
	}
	ordinary := item.QueueByteCount() - loan - pressureLoan
	growth := full - ordinary
	if !registered || ordinary < 0 || rx.usedByteCount.Load() < ordinary {
		return false
	}
	// RX and recovery are siblings. Already-paid ordinary bytes change leaf;
	// their common ancestors admit only the net growth. Validate every target
	// counter first so neither a release/reacquire gap nor transient double
	// payment can reject a final footprint that fits the unchanged hard caps.
	for current := budget; current != nil; current = current.parent {
		charge := growth
		if current == budget {
			charge = full
		}
		total := current.totalByteCount.Load()
		used := current.usedByteCount.Load() + current.quotaByteCount.Load()
		if total < charge || total-charge < used {
			return false
		}
	}
	for current := budget; current != nil; current = current.parent {
		charge := growth
		if current == budget {
			charge = full
		}
		current.usedByteCount.Add(charge)
		current.reservedByteCount.Add(charge)
	}
	promise.reservedByteCount = prefix
	promise.ingressLoanByteCount -= loan
	pack.ingressLoanByteCount = 0
	if state.owner == owner {
		state.reservedByteCount = escrow
		state.owner, state.borrowedByteCount = nil, 0
	}
	if ordinary != 0 {
		rx.usedByteCount.Add(-ordinary)
		rx.releasedByteCount.Add(ordinary)
		root.grantReserveWaitersWithLock()
		released = true
	}
	item.memoryBudget, item.queueByteCount = budget, full
	return true
}

// Only complete, unfragmented raw TCP or UDP is eligible for provider head
// recovery. One borrowed parse supplies both validity and protocol. This buys
// bounded memory; the worker still applies ordinary proof and packet policy.
func receiveProviderHeadRecoveryProtocol(packet []byte) IpProtocol {
	if len(packet) == 0 || isIpFragmentPacket(packet) {
		return IpProtocolUnknown
	}
	var path IpPath
	_, err := parseIpPathWithPayloadBorrowed(packet, &path)
	if err == nil && (path.Protocol == IpProtocolTcp || path.Protocol == IpProtocolUdp) {
		return path.Protocol
	}
	return IpProtocolUnknown
}

// A reliable Pack may mix TCP and UDP. Every member still needs a paid decode
// owner; wrapped, control, malformed and foreign-direction frames cannot use
// this child. UDP delivery keeps its ordinary callback semantics.
func receiveProviderHeadRecoveryFrames(frames []*protocol.Frame) bool {
	if len(frames) == 0 {
		return false
	}
	for _, frame := range frames {
		if frame == nil || frame.MessageType != protocol.MessageType_IpIpPacketToProvider || !frame.Raw ||
			receiveProviderHeadRecoveryProtocol(frame.MessageBytes) == IpProtocolUnknown {
			return false
		}
	}
	return true
}

func receiveProviderHeadRecoveryPack(pack *protocol.Pack) bool {
	return pack != nil && !pack.Nack && !pack.ContractAhead && receiveProviderHeadRecoveryFrames(pack.Frames)
}

// Binding rechecks the decoded shape of an already paid opening control.
func receiveHeadRecoveryOpeningPack(pack *protocol.Pack) bool {
	return pack != nil && pack.Head && !pack.Nack && !pack.ContractAhead && pack.SequenceNumber == 0 &&
		len(pack.Frames) == 0 && pack.ContractId == nil && pack.ContractFrame != nil &&
		pack.ContractFrame.MessageType == protocol.MessageType_TransferContract
}

// The consumer predicate stays separate from provider classification and its
// prepaid TCP downstream growth. Child memory supplies no proof authority.
func receiveHeadRecoveryPack(pack *protocol.Pack) bool {
	if pack == nil || pack.Nack || pack.ContractAhead || len(pack.Frames) == 0 {
		return false
	}
	for _, frame := range pack.Frames {
		if frame == nil || frame.MessageType != protocol.MessageType_IpIpPacketFromProvider || len(frame.MessageBytes) == 0 {
			return false
		}
	}
	return true
}
