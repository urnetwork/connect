// A consumer's promised current head may exceed the physical estimate made
// from smaller retained tails. A separate shared child pays that whole owner
// without spending the tails or their RX promise. It changes no ancestor cap.
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
	budget := self.receiveBufferSettings.receiveHeadRecoveryBudget()
	if budget == nil || sequence == nil || !scan.loanSafe || scan.compatibility || scan.nack || scan.contractAhead ||
		scan.hasEncryptedControl || scan.frameCount == 0 || scan.consumerFrameCount != scan.frameCount || scan.retainedByteCount <= 0 {
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
	if promise.closed || sequence.ctx.Err() != nil || !promise.set || scan.sequenceNumber != promise.ingressFloor ||
		promise.boundary <= scan.sequenceNumber || sequence.receiveIngressDuplicateWithLock(scan.messageId, scan.sequenceNumber) ||
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

// A child-funded item can only follow the consumer path that was scanned.
// ToProvider and mixed frames need RX's separate downstream-growth protocol.
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
