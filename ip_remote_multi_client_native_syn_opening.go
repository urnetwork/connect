// A first native SYN refused by raw protocol capacity may retain one optional
// opening on its existing flow. Real byte permission funds both the packet and
// every watched source; the original false result still belongs to its caller.
package connect

import (
	"errors"
	"unsafe"
)

var errNativeSynOpeningSuperseded = errors.New("native SYN opening superseded before serialization")

// One outer ordinary offer advances this fence, never a parallel candidate or
// an internal retry. Established traffic does not take this extra flow lock.
// A newer offer retires only optional, never-materialized custody.
func (self *parsedPacketGroup) prepareNativeSynOpeningOffer(parent *RemoteUserNatMultiClient, update *multiClientChannelUpdate, native bool) {
	if parent == nil || parent.flowReaperDone == nil || self.ipPath.Protocol != IpProtocolTcp || update.receivedInbound.Load() {
		return
	}
	defer update.releaseSynRetryMemory()
	update.stateLock.Lock()
	defer update.stateLock.Unlock()
	if update.synOpeningSerial != ^uint64(0) {
		update.synOpeningSerial++
	}
	if owner := update.synRetry; owner != nil && owner.openingSerial != 0 {
		update.clearSynRetryWithLock()
	}
	request := self.synRetry
	selection := update.client.loadSelection()
	if !native || request == nil || request.serial != 0 || len(self.packets) != 1 ||
		!self.ipPath.Syn || self.ipPath.Ack || self.ipPath.Rst || self.ipPath.Fin ||
		self.ipPath.TcpPayloadByteCount != 0 || !protocolClassPureTcpControl(self.packets[0].packet) ||
		update.synOpeningSerial == ^uint64(0) || update.synRetrySerial == ^uint64(0) ||
		update.IsDone() || update.sourceReset.Load() || update.receivedInbound.Load() ||
		update.synGenerationSeen || update.synRetry != nil || selection != nil && !self.affinitySynReuse || update.race != nil ||
		parent == nil || parent.flowReaperDone == nil {
		return
	}
	request.update, request.openingSerial = update, update.synOpeningSerial
	request.openingSelection = selection
	request.openingEpoch, request.openingWidth = update.sequenceAdmissionEpoch, 1
}

// All tentative observations share the original request and its flow lock.
// Geometry and actual credit are checked before any packet or slot allocation.
func (self *parsedPacketGroup) noteNativeSynOpeningRefusal(source *SendSequence, gate *resendCapacityGate) {
	request := self.synRetry
	if request == nil || request.serial != 0 || request.openingSerial == 0 || source == nil || gate == nil {
		return
	}
	update := request.update
	update.stateLock.Lock()
	defer update.stateLock.Unlock()
	if request.openingOverflow || request.openingSerial != update.synOpeningSerial ||
		request.openingEpoch != update.sequenceAdmissionEpoch || update.IsDone() ||
		update.synRetry != nil || update.synGenerationSeen || update.client.loadSelection() != request.openingSelection ||
		update.sourceReset.Load() || update.receivedInbound.Load() || source.ctx.Err() != nil {
		return
	}
	owner := request.openingOwner
	if owner == nil {
		parent := update.sequenceParent
		capacity := max(request.openingWidth, synRetryReadyCapacity(parent, update))
		remoteCapacity := synRetryRemoteTailCapacity(parent, update)
		if remoteCapacity <= 0 {
			request.openingOverflow = true
			return
		}
		metadata := retainedMessageCapacity(ByteCount(unsafe.Sizeof(tcpSynRetry{})))
		metadata = addReceiveQueueByteCount(metadata, retainedMessageCapacity(ByteCount(len(request.pin.appId))))
		metadata = addReceiveQueueByteCount(metadata, retainedMessageCapacity(ByteCount(remoteCapacity)*ByteCount(unsafe.Sizeof(Id{}))))
		packet := self.packets[0].packet
		required := addReceiveQueueByteCount(metadata, retainedMessageCapacity(ByteCount(len(packet))))
		credit, reserved := source.reserveRetainedControlMemory(required)
		if !reserved {
			request.openingOverflow = true
			return
		}
		owner = &tcpSynRetry{
			update: update, credit: credit, references: 1,
			source: request.source, provideMode: request.provideMode, pin: request.pin,
			sequence: self.ipPath.SequenceNumber, openingSerial: request.openingSerial,
			openingSelection: request.openingSelection,
			readinessEpoch:   request.openingEpoch, readinessWidth: capacity,
		}
		// Even a partial reservation is returned only after the outer attempt
		// joins its candidates, outside the flow and raw-admission locks.
		request.openingOwner = owner
		owner.readiness = newSynRetryReadiness(parent, update, source, 0, owner.sequence, capacity)
		if owner.readiness == nil {
			request.openingOverflow = true
			return
		}
		owner.packet = MessagePoolCopy(packet)
		owner.refusedRemoteIds = make([]Id, 0, remoteCapacity)
		owner.readiness.attemptEpoch = request.openingEpoch
	}
	ready := owner.readiness
	if ready == nil || request.openingWidth > len(ready.slots) {
		request.openingOverflow = true
		return
	}
	for index := 0; index < ready.count; index++ {
		if ready.slots[index].source == source && ready.slots[index].gate == gate {
			return
		}
	}
	if ready.count == len(ready.slots) {
		request.openingOverflow = true
		return
	}
	slot := &ready.slots[ready.count]
	slot.source, slot.gate, slot.sourceOrder = source, gate, update.sequenceSourceOrder
	if flags := self.completionFlags.Load(); flags&groupCompletionCandidate != 0 {
		slot.raceOrder, slot.sourceOrder = flags>>16, flags>>16
	}
	ready.count++
}

// All candidate calls and their refusal cleanup have joined. Only an overall
// false result may publish optional custody; a successful sibling never does.
// An extra reference keeps the owner alive through ready-before-watch races.
func (self *RemoteUserNatMultiClient) finishNativeSynOpening(group *parsedPacketGroup, success bool) {
	request := group.synRetry
	if request == nil || request.serial != 0 || request.openingSerial == 0 {
		return
	}
	update := request.update
	self.stateLock.Lock()
	update.stateLock.Lock()
	owner := request.openingOwner
	request.openingOwner = nil
	publish := owner != nil && !success && !request.openingOverflow && owner.readiness != nil && owner.readiness.count != 0 &&
		self.ctx.Err() == nil && self.affinityUpdateWithLock(group.ipPath) == update && self.affinityTransitions[update] == nil &&
		request.openingSerial == update.synOpeningSerial && request.openingEpoch == update.sequenceAdmissionEpoch &&
		update.synRetrySerial != ^uint64(0) && update.synRetry == nil && !update.synGenerationSeen &&
		!update.IsDone() && !update.sourceReset.Load() && !update.receivedInbound.Load() &&
		!update.affinitySources.closed && !update.pendingCollapseCloseWithLock() && update.client.loadSelection() == request.openingSelection && update.race == nil
	if publish {
		update.synRetrySerial++
		owner.serial, owner.retryLineage = update.synRetrySerial, update.synRetrySerial
		owner.readiness.serial = owner.serial
		owner.references++
		update.synRetry = owner
	} else if owner != nil {
		update.releaseSynRetryReferenceWithLock(owner)
	}
	update.stateLock.Unlock()
	self.stateLock.Unlock()
	update.releaseSynRetryMemory()
	if publish {
		self.armSynRetryReadiness(owner)
		self.finishSynRetry(owner)
	}
}

// Only the new optional opening carries this stamp. Ordinary accepted owners
// keep their existing semantics; a stale retry cannot acquire a wire ordinal.
func (self *parsedPacketGroup) nativeSynOpeningCurrentWithLock() bool {
	request := self.synRetry
	if request == nil || request.serial == 0 || request.openingSerial == 0 {
		return true
	}
	// This source already owns the admitted claim. Its own pending SYN is
	// dequeued before checking source-reset claims. Every later ordinary
	// offer independently advances the stamp, including refused controls.
	return self.nativeSynOpeningStampCurrentWithLock() && request.update.synRetry.validGenerationWithLock(request.update)
}

// Before FIFO dequeue, the group's own admitted SYN is still a pending reset.
// Its exact cache/offer stamp can already reject supersession without confusing
// that own claim with a different control; full generation checks follow dequeue.
func (self *parsedPacketGroup) nativeSynOpeningStampCurrentWithLock() bool {
	request := self.synRetry
	if request == nil || request.serial == 0 || request.openingSerial == 0 {
		return true
	}
	update := request.update
	owner := update.synRetry
	return update.synOpeningSerial == request.openingSerial && owner != nil && owner.serial == request.serial &&
		(owner.openingSerial == 0 || owner.openingSelection == nil || owner.openingSelection == update.client.loadSelection()) &&
		!update.IsDone() && !update.sourceReset.Load() && !update.receivedInbound.Load()
}

// Materialized retries retain their existing unbound-only policy. A pending
// first SYN may instead resume its exact inherited selection, never a rebind.
func (self *tcpSynRetry) synRetrySelectionCurrentWithLock(update *multiClientChannelUpdate) bool {
	selection := update.client.loadSelection()
	if self.openingSerial != 0 && self.openingSelection != nil {
		return selection == self.openingSelection
	}
	return selection == nil
}

// Preparation can wait on contracts or routes after source dequeue. The final
// ordinal assignment repeats this check under the same lock as newer offers.
func (self *parsedPacketGroup) nativeSynOpeningCurrent() bool {
	request := self.synRetry
	if request == nil || request.serial == 0 || request.openingSerial == 0 {
		return true
	}
	request.update.stateLock.Lock()
	defer request.update.stateLock.Unlock()
	return self.nativeSynOpeningCurrentWithLock()
}

// A failed first opening with no live wake source has no standing remote
// retry obligation. Return its optional backing instead of waiting for TCP idle.
func (self *multiClientChannelUpdate) retireUnwatchedSynOpeningWithLock(owner *tcpSynRetry, ready *tcpSynRetryReadiness) {
	if owner != nil && owner == self.synRetry && ready != nil && owner.readiness == ready &&
		owner.serial == ready.serial && owner.sequence == ready.sequence && owner.openingSerial != 0 && !owner.queued &&
		(self.client.Load() == nil || owner.openingSelection != nil) && self.race == nil && !self.pendingCollapseCloseWithLock() {
		self.clearSynRetryWithLock()
	}
}
