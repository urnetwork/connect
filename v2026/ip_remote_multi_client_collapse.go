package connect

// A value copied into the existing send completion closure, not a separately
// allocated recovery owner. It records only a flow and its admission epoch;
// it retains no IP bytes or frame. An unwritten expiry invalidates proof rather
// than pretending the byte range remains owned by a selected Transfer queue.
type tcpCollapseAdmission struct {
	update *multiClientChannelUpdate
	epoch  uint64
}

// One numeric receipt per in-flight group offering a new SYN generation. It
// lives in that group's existing optional observation scope, never in a Pack's
// asynchronous completion token. The native send's unconditional completion
// unlinks and clears it, including on refusal or panic. No packet is retained.
type tcpSynAdmission struct {
	next           *tcpSynAdmission
	update         *multiClientChannelUpdate
	responseClient *multiClientChannel
	sequence       uint32
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

func (group *parsedPacketGroup) prepareCollapseAdmission(update *multiClientChannelUpdate) {
	update.stateLock.Lock()
	admission := tcpCollapseAdmission{update: update, epoch: update.sequenceAdmissionEpoch}
	// Only an actually different generation needs pre-commit response proof.
	// Generation identity survives coverage revocation and provider rebinding.
	seen, sequence := update.synGenerationSeen, update.synGenerationNumber
	hasPriorState := seen || update.sequencePacketCount != 0
	newGeneration := false
	for i := range group.packets {
		path := group.packets[i].ipPath
		if path.Syn {
			newGeneration = newGeneration || hasPriorState && (!seen || sequence != path.SequenceNumber)
			seen, sequence, hasPriorState = true, path.SequenceNumber, true
		}
	}
	if newGeneration {
		if group.admissionObservations == nil {
			group.admissionObservations = &sendPackAdmissionObservations{}
		}
		observation := &group.admissionObservations.synAdmission
		*observation = tcpSynAdmission{next: update.synAdmissions, update: update, sequence: sequence}
		update.synAdmissions = observation
	}
	update.stateLock.Unlock()
	group.collapseAdmission = admission
	for i := range group.packets {
		group.packets[i].collapseAdmission = admission
	}
}

// Established non-SYN ingress keeps its lock-free fast path. First response
// and SYN-ACK observations serialize with admission commit so an old response
// cannot establish a newly accepted generation after the reset.
func (update *multiClientChannelUpdate) markReceivedInbound(client *multiClientChannel, control tcpControlObservation) bool {
	if !control.syn && update.receivedInbound.Load() {
		return false
	}
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
			// The provider acknowledges the SYN at ISN+1, including a
			// payload-bearing SYN. uint32 addition preserves wraparound.
			if control.ackSequenceNumber == admission.sequence+1 {
				admission.responseClient = client
			}
		}
	}
	if update.synGenerationAwaiting {
		if !control.syn || !control.ack || control.ackSequenceNumber != update.synGenerationNumber+1 {
			return false
		}
		update.synGenerationAwaiting = false
	}
	return update.receivedInbound.CompareAndSwap(false, true)
}

func (admission tcpCollapseAdmission) complete(err error) {
	if admission.update == nil || !packetTransferExpiredUnwritten(err) {
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
func (update *multiClientChannelUpdate) updateSequenceCoverageWithLock(packet *parsedPacket) bool {
	start := packet.ipPath.SequenceNumber
	end := tcpPacketNextSequenceNumber(packet.ipPath, packet.payload)
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
