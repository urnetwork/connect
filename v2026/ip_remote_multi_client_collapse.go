package connect

// A value copied into the existing send completion closure, not a separately
// allocated recovery owner. It records only a flow and its admission epoch;
// it retains no IP bytes or frame. An explicit unwritten failure invalidates
// proof rather than pretending the byte range remains owned by Transfer.
type tcpCollapseAdmission struct {
	update *multiClientChannelUpdate
	epoch  uint64
	// A scalar lease for one real inner-TCP ACK frontier, not Transfer ACK.
	recovery uint64
}

// One numeric receipt per in-flight group offering a new SYN generation. It
// lives in that group's existing optional observation scope, never in a Pack's
// asynchronous completion token. The native send's unconditional completion
// unlinks and clears it, including on refusal or panic. No packet is retained.
type tcpSynAdmission struct {
	next            *tcpSynAdmission
	update          *multiClientChannelUpdate
	responseClient  *multiClientChannel
	responseControl tcpControlObservation
	sequence        uint32
	sequenceEnd     uint32
}

func (admission *tcpSynAdmission) clear() {
	update := admission.update
	if update == nil {
		return
	}
	update.stateLock.Lock()
	defer update.stateLock.Unlock()
	for link := &update.synAdmissions; *link != nil; link = &(*link).next {
		if *link == admission {
			*link = admission.next
			break
		}
	}
	// A Transfer completion can retain the observation scope. It must not
	// retain this flow, another live offer, or that offer's selected client.
	*admission = tcpSynAdmission{}
}

// Compatibility callers retain the same complete-group collapse contract.
func (self *parsedPacketGroup) prepareCollapseAdmission(update *multiClientChannelUpdate, parents ...*RemoteUserNatMultiClient) bool {
	return self.prepareCollapseAdmissionWithRefusal(update, nil, parents...)
}

// Carries the synchronous native refusal receipt through the final lease gate.
func (self *parsedPacketGroup) prepareCollapseAdmissionWithRefusal(update *multiClientChannelUpdate, batchAdmission *ipPacketGroupAdmission, parents ...*RemoteUserNatMultiClient) bool {
	update.stateLock.Lock()
	if len(parents) != 0 {
		update.sequenceParent = parents[0]
	}
	admission := tcpCollapseAdmission{update: update, epoch: update.sequenceAdmissionEpoch}
	if len(parents) != 0 && parents[0].settings.TcpCollapsePrevention &&
		!self.reserveTcpRecoveryWithLock(update, parents[0], &admission) {
		parents[0].recordCollapsedPacketGroup(self, batchAdmission)
		update.stateLock.Unlock()
		return false
	}
	// Only an actually different generation needs pre-commit response proof.
	// Generation identity survives coverage revocation and provider rebinding.
	seen, sequence := update.synGenerationSeen, update.synGenerationNumber
	hasPriorState := seen || update.sequencePacketCount != 0
	newGeneration := false
	hasSyn := false
	sequenceEnd := sequence + 1
	for i := range self.packets {
		path := self.packets[i].ipPath
		if path.Syn {
			hasSyn = true
			newGeneration = newGeneration || hasPriorState && (!seen || sequence != path.SequenceNumber)
			seen, sequence, hasPriorState = true, path.SequenceNumber, true
			sequenceEnd = tcpPacketNextSequenceNumberWithPayloadByteCount(path, path.TcpPayloadByteCount)
		}
	}
	if hasSyn && len(parents) != 0 {
		if update.IsDone() {
			admission.releaseTcpRecoveryWithLock()
			update.stateLock.Unlock()
			return false
		}
		// The existing send defer releases this scalar; no descriptor is
		// linked and no bytes are reserved before successful admission.
		if self.completionFlags.Or(groupCompletionSynOffer)&groupCompletionSynOffer == 0 {
			update.sequenceSynOffers++
		}
	}
	if newGeneration {
		if self.admissionObservations == nil {
			self.admissionObservations = &sendPackAdmissionObservations{}
		}
		observation := &self.admissionObservations.synAdmission
		*observation = tcpSynAdmission{next: update.synAdmissions, update: update, sequence: sequence, sequenceEnd: sequenceEnd}
		update.synAdmissions = observation
	}
	update.stateLock.Unlock()
	self.collapseAdmission = admission
	for i := range self.packets {
		self.packets[i].collapseAdmission = admission
	}
	return true
}

// Controls without SYN/ACK keep their established lock-free fast path. Inner
// ACK authority and first/SYN responses serialize with admission and reset.
func (update *multiClientChannelUpdate) markReceivedInbound(client *multiClientChannel, control tcpControlObservation) bool {
	established := update.receivedInbound.Load()
	if !control.syn && !control.ack && established {
		return false
	}
	if (!established || control.syn) && client != nil && client.beforeFirstAnswerMarkForTest != nil {
		client.beforeFirstAnswerMarkForTest(update)
	}
	defer update.releaseSynRetryMemory()
	update.stateLock.Lock()
	defer update.stateLock.Unlock()
	return update.markReceivedInboundWithLock(client, control)
}

// The caller holds stateLock and has resolved this packet's committed owner.
func (update *multiClientChannelUpdate) markReceivedInboundWithLock(client *multiClientChannel, control tcpControlObservation) bool {
	if client != update.client.Load() {
		return false
	}
	if control.syn && control.ack {
		for admission := update.synAdmissions; admission != nil; admission = admission.next {
			if tcpSynAckInRange(admission.sequence, admission.sequenceEnd, control.ackSequenceNumber) {
				admission.responseClient = client
				admission.responseControl = control
			}
		}
	}
	if update.synGenerationAwaiting {
		if !control.syn || !control.ack || !tcpSynAckInRange(update.synGenerationNumber, update.synGenerationEnd, control.ackSequenceNumber) {
			return false
		}
		update.synGenerationAwaiting = false
	}
	update.observeTcpRecoveryAckWithLock(client, control)
	update.clearSynRetryWithLock()
	return update.receivedInbound.CompareAndSwap(false, true)
}

func (admission tcpCollapseAdmission) complete(err error) {
	if admission.update == nil ||
		!packetTransferExpiredUnwritten(err) && !sendPackFailedUnwritten(err) {
		return
	}
	update := admission.update
	update.stateLock.Lock()
	defer update.stateLock.Unlock()
	// Several later admissions may have merged this packet into one interval.
	// Without a per-packet map we cannot subtract an arbitrary interior hole.
	// Forget the proof conservatively, including a same-ISN SYN claim. A stale
	// completion may permit one extra duplicate; it can never suppress recovery.
	update.sequenceAdmissionEpoch++
	update.sequenceCovered = false
	update.sequenceAckPositionSeen = false
	update.sequenceSynSeen = false
	admission.revokeTcpRecoveryWithLock(err)
}

// sequenceCoversWithLock proves the entire requested interval, never only its
// right edge. TCP windows are smaller than half the sequence space; an
// ambiguous interval fails open. Zero-length controls cannot fill data holes.
func (update *multiClientChannelUpdate) sequenceCoversWithLock(packet *parsedPacket) bool {
	start := packet.ipPath.SequenceNumber
	end := tcpPacketNextSequenceNumber(packet.ipPath, packet.payload)
	if start == end && update.sequenceAckPositionSeen && start == update.sequenceAckPosition {
		return true
	}
	return update.sequenceCovered && int32(end-start) >= 0 &&
		int32(start-update.sequenceCoveredFrom) >= 0 &&
		int32(update.sequenceCoveredTo-end) >= 0
}

// Keep a single contiguous accepted interval inline. Overlap and adjacency
// merge; a disjoint range replaces the proof instead of covering the gap.
// Pure ACKs retain their own point and do not erase existing data coverage.
func (update *multiClientChannelUpdate) updateSequenceCoverageWithLock(packet *parsedPacket, payloadByteCounts ...int) bool {
	start := packet.ipPath.SequenceNumber
	payloadByteCount := len(packet.payload)
	if len(payloadByteCounts) != 0 {
		payloadByteCount = payloadByteCounts[0]
	}
	end := tcpPacketNextSequenceNumberWithPayloadByteCount(packet.ipPath, payloadByteCount)
	if start == end {
		changed := !update.sequenceAckPositionSeen || update.sequenceAckPosition != start
		update.sequenceAckPosition, update.sequenceAckPositionSeen = start, true
		return changed
	}
	if int32(end-start) < 0 {
		update.sequenceCovered = false
		return true
	}
	if !update.sequenceCovered || int32(start-update.sequenceCoveredTo) > 0 ||
		int32(update.sequenceCoveredFrom-end) > 0 {
		update.sequenceCoveredFrom, update.sequenceCoveredTo = start, end
		update.sequenceCovered = true
		return true
	}
	changed := false
	if int32(update.sequenceCoveredFrom-start) > 0 {
		update.sequenceCoveredFrom = start
		changed = true
	}
	if int32(end-update.sequenceCoveredTo) > 0 {
		update.sequenceCoveredTo = end
		changed = true
	}
	if int32(update.sequenceCoveredTo-update.sequenceCoveredFrom) < 0 {
		// Long-lived streams eventually cross the half-space ambiguity. Keep
		// only the latest proven range rather than invent a wraparound hole.
		update.sequenceCoveredFrom, update.sequenceCoveredTo = start, end
	}
	return changed
}
