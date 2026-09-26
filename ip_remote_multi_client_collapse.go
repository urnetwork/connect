package connect

// A value copied into the existing send completion closure, not a separately
// allocated recovery owner. It records only a flow and its admission epoch;
// it retains no IP bytes or frame. An unwritten expiry invalidates proof rather
// than pretending the byte range remains owned by a selected Transfer queue.
type tcpCollapseAdmission struct {
	update *multiClientChannelUpdate
	epoch  uint64
}

func (group *parsedPacketGroup) prepareCollapseAdmission(update *multiClientChannelUpdate) {
	update.stateLock.Lock()
	admission := tcpCollapseAdmission{update: update, epoch: update.sequenceAdmissionEpoch}
	update.stateLock.Unlock()
	group.collapseAdmission = admission
	for i := range group.packets {
		group.packets[i].collapseAdmission = admission
	}
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
