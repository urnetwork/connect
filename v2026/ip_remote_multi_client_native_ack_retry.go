// Refused advancing feedback or a window reopening keeps one exact packet on
// its existing flow. The parent reaper consumes real protocol-admission turns;
// it creates no timer, worker, provider selection or synthetic acknowledgement.
package connect

import (
	"errors"
	"unsafe"

	"github.com/urnetwork/connect/v2026/protocol"
)

var errNativeAckRetrySuperseded = errors.New("native TCP feedback superseded before serialization")

// A later native TCP offer on the same live owners can supersede a detached
// replay. A local FIN also needs independent peer-acknowledged DATA coverage.
var errSendPackNativeAckRetrySuperseded = &sendPackUnwrittenError{cause: errNativeAckRetrySuperseded}

// Reports an unwritten local pure ACK replay superseded by a later native TCP
// offer. The later packet's admission and delivery have their own outcomes.
func IsSendPackNativeAckRetrySuperseded(err error) bool {
	return err == errSendPackNativeAckRetrySuperseded
}

// The original synchronous attempt records only an actual raw-count refusal.
// A retry copies this immutable flow/source stamp, never the pending owner.
type tcpNativeAckRetryAttempt struct {
	update        *multiClientChannelUpdate
	selection     *multiClientAffinitySelection
	epoch         uint64
	serial        uint64
	source        *SendSequence
	gate          *resendCapacityGate
	refusalOrder  uint64
	sequence, ack uint32
	replay        bool
}

// A real reliable DATA terminal can show that the same source has already
// delivered a greater inner ACK. This scalar receipt owns no packet bytes.
type nativeAckPeerDataReceipt struct {
	sourceId      Id
	epoch, order  uint64
	sequence, ack uint32
}

// Only flows with an actual observed retained ACK allocate this proof.
type nativeAckReplayEvidence struct {
	lastHardInvalidation uint64
	localFinOrder        uint64
	peerData             nativeAckPeerDataReceipt
}

// The parent index owns this node until a finite reaper visit returns it. The
// flow pointer is an identity index, not a second packet owner. While dispatch
// runs, inFlight prevents replacement; a newer native offer invalidates its
// serial and the ordinary source disposes a still-unwritten retry.
type tcpNativeAckRetry struct {
	attempt tcpNativeAckRetryAttempt
	packet  []byte
	credit  *transferRetainedControlCredit
	// Descriptive physical envelope; the detached protocol token is not DATA permission.
	memoryByteCount ByteCount
	inputSource     TransferPath
	provideMode     protocol.ProvideMode
	pin             flowPin
	waiter          resendCapacityWaiter
	installation    uint64
	indexed         bool
	inFlight        bool
	previous        *tcpNativeAckRetry
	next            *tcpNativeAckRetry
}

// Called inside the existing native group's flow lock, after policy and before
// admission. Every later TCP offer passing the gate invalidates an older
// unsent reopening; pure ACKs always pass that gate, including a newer zero
// window or different SACK/ECN/options. Lifecycle controls also invalidate at
// noteConnectAttempt, before collapse. Covered redundant DATA has no new authority.
func (self *parsedPacketGroup) prepareNativeAckRetryWithLock(update *multiClientChannelUpdate) {
	self.nativeAckRetry = nil
	self.nativeAckRetrySerial = 0
	self.nativeAckRetrySelection = nil
	update.advanceNativeAckRetryWithLock(true)
	if len(self.packets) != 1 || !self.packets[0].pureTcpAck() {
		return
	}
	path := self.packets[0].ipPath
	if path.TcpWindowSize == 0 || !update.sequenceAckPositionSeen ||
		update.sequenceAckPosition != path.SequenceNumber {
		return
	}
	ack, window := update.ackSequenceNumber, update.tcpWindowSize
	if update.sequenceAdmissionStateSeen {
		ack, window = update.sequenceAdmissionAck, update.sequenceAdmissionWindow
	}
	if path.AckSequenceNumber == ack {
		if window != 0 {
			return
		}
	} else if int32(path.AckSequenceNumber-ack) <= 0 {
		return
	}
	// Successful ordinary ACKs do not reparse bytes or scan retained claims.
	selection := update.client.loadSelection()
	if selection == nil || selection.client == nil || selection.client.IsDone() ||
		update.nativeAckSerial == ^uint64(0) || !update.receivedInbound.Load() ||
		update.IsDone() || update.sourceReset.Load() {
		return
	}
	// Ordinary advancing ACKs allocate nothing. The exact raw refusal below
	// materializes an attempt from these existing group/flow admission scalars.
	self.nativeAckRetrySelection = selection
	self.nativeAckRetrySerial = update.nativeAckSerial
}

// Lifecycle controls, resets and teardown invalidate independently of native preparation.
func (self *multiClientChannelUpdate) invalidateNativeAckRetryWithLock() {
	self.advanceNativeAckRetryWithLock(false)
	if evidence := self.nativeAckReplayEvidence; evidence != nil {
		evidence.lastHardInvalidation = self.nativeAckSerial
		evidence.peerData = nativeAckPeerDataReceipt{}
	}
}

// Preserve which serials advanced solely on native TCP offers. Overflow permanently
// disables optional recovery. Waking the existing reaper takes no parent lock.
func (self *multiClientChannelUpdate) advanceNativeAckRetryWithLock(nativeOffer bool) {
	if self.nativeAckSerial != ^uint64(0) {
		self.nativeAckSerial++
	}
	if !nativeOffer {
		self.nativeAckLastUnrelatedSerial = self.nativeAckSerial
		if evidence := self.nativeAckReplayEvidence; evidence != nil {
			evidence.localFinOrder = 0
		}
	}
	if self.nativeAckRetry != nil && self.sequenceParent != nil {
		self.sequenceParent.notifyFlowReaper()
	}
}

// Ordinary admitted originals are never changed by this predicate. Only a
// newly offered local retry needs the complete generation/selection fence.
func (self *tcpNativeAckRetryAttempt) currentWithLock() bool {
	return self.update != nil && self.update.nativeAckSerial == self.serial && self.ownerCurrentWithLock()
}

// The owner's lifetime, flow generation and selection stay independent from
// the feedback serial, which a newer native offer may advance while all live.
func (self *tcpNativeAckRetryAttempt) ownerCurrentWithLock() bool {
	update := self.update
	return update != nil && !update.IsDone() && !update.sourceReset.Load() &&
		update.receivedInbound.Load() &&
		update.sequenceAdmissionEpoch == self.epoch &&
		update.client.loadSelection() == self.selection && self.selection != nil && self.selection.client != nil &&
		!self.selection.client.IsDone() && !update.pendingCollapseCloseWithLock() &&
		(self.source == nil || self.source.ctx.Err() == nil)
}

// Check the retry at chunk entry, before any application frame is serialized.
func (self *tcpNativeAckRetryAttempt) replayDisposition(source *SendSequence) error {
	if self.update == nil {
		return newUnwrittenSendPackError(errNativeAckRetrySuperseded)
	}
	self.update.stateLock.Lock()
	defer self.update.stateLock.Unlock()
	return self.replayDispositionWithLock(source)
}

// The final serialization fence calls this under the same lock as ordinal
// assignment. Native offers explain only the replay's local disposal.
func (self *tcpNativeAckRetryAttempt) replayDispositionWithLock(source *SendSequence) error {
	if self.source == source && self.ownerCurrentWithLock() {
		update := self.update
		if update.nativeAckSerial == self.serial {
			return nil
		}
		if self.replay && source != nil && self.serial != 0 && self.serial < update.nativeAckSerial &&
			update.nativeAckSerial != ^uint64(0) && (update.nativeAckLastUnrelatedSerial <= self.serial ||
			self.peerDataCoversLocalFinWithLock(source)) {
			return errSendPackNativeAckRetrySuperseded
		}
	}
	return newUnwrittenSendPackError(errNativeAckRetrySuperseded)
}

// The last unrelated change must be the ordered local FIN on this exact source.
// It supplies cause only. The independently completed DATA supplies coverage.
func (self *tcpNativeAckRetryAttempt) peerDataCoversLocalFinWithLock(source *SendSequence) bool {
	update := self.update
	evidence := update.nativeAckReplayEvidence
	if evidence == nil {
		return false
	}
	proof := evidence.peerData
	return evidence.lastHardInvalidation <= self.serial &&
		self.serial < update.nativeAckLastUnrelatedSerial && update.nativeAckLastUnrelatedSerial == update.nativeAckSerial &&
		update.egressFinSeen && !update.ingressFinSeen && evidence.localFinOrder != 0 &&
		evidence.localFinOrder == update.sequenceControlOrder && update.sequenceSourceId == source.sequenceId &&
		proof.sourceId != (Id{}) && proof.sourceId == source.sequenceId && proof.epoch == self.epoch &&
		self.refusalOrder < proof.order && proof.order < evidence.localFinOrder &&
		tcpSequenceAtOrAfter(proof.sequence, self.sequence) && int32(proof.ack-self.ack) > 0
}

func (self tcpControlObservation) ordinaryLocalFin() bool {
	return self.ack && self.fin && !self.syn && !self.rst && self.payloadByteCount == 0
}

func (self *tcpNativeAckRetryAttempt) current() bool {
	if self == nil || self.update == nil {
		return false
	}
	self.update.stateLock.Lock()
	defer self.update.stateLock.Unlock()
	return self.currentWithLock()
}

// Called only by the exact NoBudget, zero-timeout raw-admission refusal. No
// source callback or queued packet may read this original after false returns.
func (self *parsedPacketGroup) noteNativeAckCapacityRefusal(source *SendSequence, gate *resendCapacityGate) {
	attempt := self.nativeAckRetry
	if attempt == nil {
		if self.nativeAckRetrySerial == 0 || self.nativeAckRetrySelection == nil || self.collapseAdmission.update == nil {
			return
		}
		update := self.collapseAdmission.update
		update.stateLock.Lock()
		refusalOrder := update.sequenceControlOrder
		update.stateLock.Unlock()
		attempt = &tcpNativeAckRetryAttempt{update: update,
			selection: self.nativeAckRetrySelection, epoch: self.collapseAdmission.epoch,
			serial: self.nativeAckRetrySerial, refusalOrder: refusalOrder,
			sequence: self.ipPath.SequenceNumber, ack: self.ipPath.AckSequenceNumber}
		self.nativeAckRetry = attempt
	}
	if !attempt.replay || attempt.source == source {
		attempt.source, attempt.gate = source, gate
	}
}

// False remains a pre-Transfer result. The consuming native entry returns the
// original normally; this separate exact copy has no delivery callback and
// cannot rewrite the original refusal observation after public return.
func (self *RemoteUserNatMultiClient) retainNativeAckRetry(group *parsedPacketGroup, attempt *tcpNativeAckRetryAttempt, source TransferPath, mode protocol.ProvideMode) {
	if attempt == nil || attempt.replay || attempt.source == nil || attempt.gate == nil ||
		self.flowReaperDone == nil || len(group.packets) != 1 ||
		!protocolClassPureTcpControl(group.packets[0].packet) {
		return
	}
	update := attempt.update
	var previous []byte
	var previousCredit *transferRetainedControlCredit
	self.stateLock.Lock()
	update.stateLock.Lock()
	if self.ctx.Err() != nil || self.affinityUpdateWithLock(update.ipPath) != update ||
		self.affinityTransitions[update] != nil || !attempt.currentWithLock() || attempt.gate.closed.Load() {
		update.stateLock.Unlock()
		self.stateLock.Unlock()
		return
	}
	owner := update.nativeAckRetry
	if owner != nil && owner.inFlight {
		update.stateLock.Unlock()
		self.stateLock.Unlock()
		return
	}
	size := addReceiveQueueByteCount(retainedMessageCapacity(ByteCount(len(group.packets[0].packet))),
		retainedMessageCapacity(ByteCount(unsafe.Sizeof(tcpNativeAckRetry{}))))
	envelope := addReceiveQueueByteCount(size, transferRetainedControlCreditByteCount())
	credit, ok := attempt.source.reserveRetainedControlMemory(size, true)
	if !ok {
		update.stateLock.Unlock()
		self.stateLock.Unlock()
		return
	}
	if owner == nil {
		owner = &tcpNativeAckRetry{}
		update.nativeAckRetry = owner
	} else {
		self.removeNativeAckRetryWithLock(owner)
		owner.attempt.gate.abandonWatch(&owner.waiter, owner.installation)
		previous = owner.packet
		previousCredit = owner.credit
	}
	if observations := group.admissionObservations; observations != nil && observations.nativeReadyPureTcpAck &&
		update.nativeAckReplayEvidence == nil {
		update.nativeAckReplayEvidence = &nativeAckReplayEvidence{lastHardInvalidation: update.nativeAckLastUnrelatedSerial}
	}
	owner.credit, owner.memoryByteCount = credit, envelope
	owner.attempt = *attempt
	owner.packet = MessagePoolCopy(group.packets[0].packet)
	owner.inputSource, owner.provideMode, owner.pin = source, mode, group.pin
	owner.installation = owner.attempt.gate.watch(&owner.waiter, self.flowReaperWake)
	self.appendNativeAckRetryWithLock(owner)
	update.stateLock.Unlock()
	self.stateLock.Unlock()
	if previous != nil {
		MessagePoolReturn(previous)
	}
	previousCredit.Release()
	// A closed gate returns no installation; the visit still returns its owner.
	self.notifyFlowReaper()
}

func (self *RemoteUserNatMultiClient) appendNativeAckRetryWithLock(owner *tcpNativeAckRetry) {
	if owner.indexed {
		panic("native ACK retry already indexed")
	}
	owner.previous, owner.next, owner.indexed = self.nativeAckRetryTail, nil, true
	if owner.previous == nil {
		self.nativeAckRetryHead = owner
	} else {
		owner.previous.next = owner
	}
	self.nativeAckRetryTail = owner
	self.nativeAckRetryCount++
}

func (self *RemoteUserNatMultiClient) removeNativeAckRetryWithLock(owner *tcpNativeAckRetry) {
	if !owner.indexed {
		return
	}
	if owner.previous == nil {
		self.nativeAckRetryHead = owner.next
	} else {
		owner.previous.next = owner.next
	}
	if owner.next == nil {
		self.nativeAckRetryTail = owner.previous
	} else {
		owner.next.previous = owner.previous
	}
	owner.previous, owner.next, owner.indexed = nil, nil, false
	self.nativeAckRetryCount--
}

// One finite rotation per existing reaper wake. A consumed turn can rearm only
// after another actual raw-count refusal, never merely because capacity is open.
func (self *RemoteUserNatMultiClient) drainNativeAckRetries() {
	self.stateLock.Lock()
	count := self.nativeAckRetryCount
	self.stateLock.Unlock()
	for range count {
		self.stateLock.Lock()
		owner := self.nativeAckRetryHead
		if owner != nil {
			self.removeNativeAckRetryWithLock(owner)
			owner.attempt.update.stateLock.Lock()
			owner.inFlight = true
			owner.attempt.update.stateLock.Unlock()
		}
		self.stateLock.Unlock()
		if owner == nil {
			return
		}
		self.processNativeAckRetry(owner)
	}
}

func (self *RemoteUserNatMultiClient) processNativeAckRetry(owner *tcpNativeAckRetry) {
	update := owner.attempt.update
	self.stateLock.Lock()
	update.stateLock.Lock()
	valid := self.ctx.Err() == nil && update.nativeAckRetry == owner &&
		self.affinityUpdateWithLock(update.ipPath) == update && self.affinityTransitions[update] == nil &&
		owner.attempt.currentWithLock() && owner.installation != 0 && !owner.attempt.gate.closed.Load()
	granted := valid && owner.attempt.gate.watchGranted(&owner.waiter, owner.installation) &&
		owner.attempt.gate.passWatch(&owner.waiter, owner.installation)
	if valid && !granted {
		owner.inFlight = false
		self.appendNativeAckRetryWithLock(owner)
		update.stateLock.Unlock()
		self.stateLock.Unlock()
		return
	}
	owner.attempt.gate.abandonWatch(&owner.waiter, owner.installation)
	owner.installation = 0
	owner.inFlight = true
	update.stateLock.Unlock()
	self.stateLock.Unlock()

	retry := owner.attempt
	retry.replay, retry.gate = true, nil
	if granted {
		packet := MessagePoolShareReadOnly(owner.packet)
		path, payload, err := ParseIpPathWithPayload(packet)
		accepted := false
		if err == nil {
			group := &parsedPacketGroup{ipPath: path, pin: owner.pin, byteCount: ByteCount(len(packet)), nativeAckRetry: &retry,
				packets: []parsedPacket{{packet: packet, ipPath: path, payload: payload, pin: owner.pin}}}
			admission := ipPacketGroupAdmission{expectedUpdate: update, expectedEpoch: retry.epoch}
			if retry.current() && group.prepareCollapseAdmissionWithRefusal(update, &admission, self) {
				group.completionSelection = retry.selection
				group.prepareAdmissionObservations(retry.selection.client, true)
				accepted, err = retry.selection.client.SendGroupDetailed(group, 0)
				if group.admissionObservations != nil {
					group.admissionObservations.complete(accepted && err == nil)
				}
				if !accepted || err != nil {
					group.collapseAdmission.releaseTcpRecovery()
				} else {
					update.commitSequenceGroupForClient(group, retry.selection.client, retry.selection)
				}
			}
		}
		if !accepted || err != nil {
			MessagePoolReturn(packet)
		}
	}
	self.stateLock.Lock()
	update.stateLock.Lock()
	owner.inFlight = false
	if granted && retry.gate != nil && retry.source == owner.attempt.source && self.ctx.Err() == nil &&
		update.nativeAckRetry == owner && retry.currentWithLock() && !retry.gate.closed.Load() {
		owner.attempt.gate = retry.gate
		owner.installation = retry.gate.watch(&owner.waiter, self.flowReaperWake)
		closedDuringWatch := owner.installation == 0
		self.appendNativeAckRetryWithLock(owner)
		update.stateLock.Unlock()
		self.stateLock.Unlock()
		if closedDuringWatch {
			self.notifyFlowReaper()
		}
		return
	}
	if update.nativeAckRetry == owner {
		update.nativeAckRetry = nil
	}
	update.stateLock.Unlock()
	self.stateLock.Unlock()
	packet, credit := owner.packet, owner.credit
	owner.packet, owner.credit = nil, nil
	owner.memoryByteCount = 0
	MessagePoolReturn(packet)
	credit.Release()
}

// Parent cancellation invalidates every indexed request before this drain.
// The existing joined reaper is also the final owner of their packet copies.
func (self *RemoteUserNatMultiClient) discardNativeAckRetries() {
	for {
		self.stateLock.Lock()
		owner := self.nativeAckRetryHead
		if owner != nil {
			self.removeNativeAckRetryWithLock(owner)
			owner.attempt.update.stateLock.Lock()
			owner.inFlight = true
			owner.attempt.update.stateLock.Unlock()
		}
		self.stateLock.Unlock()
		if owner == nil {
			return
		}
		self.processNativeAckRetry(owner)
	}
}
