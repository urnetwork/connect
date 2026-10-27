package connect

// Transfer custody ends at the provider queue; bounded inner TCP reordering
// may subsequently refuse a future segment. A cumulative inner ACK proves
// that a retry spanning that exact frontier cannot be future there anymore.
// One scalar lease permits one recovery group per advancing ACK. It holds no
// packet, descriptor, timer, or provider reference beyond the flow's owner.
type tcpCollapseRecovery struct {
	client             *multiClientChannel
	serial             uint64
	ack                uint32
	seen               bool
	reserved           bool
	used               bool
	peerSynSequence    uint32
	peerSequence       uint32
	peerWindow         uint32
	peerSeen           bool
	windowScale        uint8
	windowScaleOffered bool
	windowScaleEnabled bool
}

// Never reuse an old asynchronous token. Exhaustion conservatively fails open.
func (self *tcpCollapseRecovery) advance() {
	if self.serial != ^uint64(0) {
		self.serial++
	}
	self.reserved, self.used = false, false
}

func (self *tcpCollapseRecovery) clear() {
	self.advance()
	self.client, self.seen = nil, false
	self.peerSeen, self.windowScaleOffered, self.windowScaleEnabled = false, false, false
	self.windowScale = 0
}

// Called while the ingress source and accepted SYN cohort are resolved under
// stateLock, before delivery can race another SYN admission. The outgoing
// ACK/window tuple and carrier/Transfer ACKs cannot create this authority.
func (self *multiClientChannelUpdate) observeTcpRecoveryAckWithLock(client *multiClientChannel, control tcpControlObservation) {
	if !control.valid || !control.ack || control.rst || client == nil || client != self.client.Load() ||
		!self.synGenerationSeen || self.synGenerationAwaiting {
		return
	}
	recovery := &self.sequenceRecovery
	// Ordinary nonadvancing ACKs cannot change recovery authority. Reject
	// them before the claim scan; SYN still establishes its peer anchor below.
	if !control.syn && recovery.client == client && recovery.seen && int32(control.ackSequenceNumber-recovery.ack) <= 0 {
		return
	}
	if self.pendingCollapseResetWithLock() || control.syn && control.ackSequenceNumber != self.synGenerationNumber+1 {
		return
	}
	if control.syn {
		if recovery.peerSeen && (recovery.client != client || recovery.peerSynSequence != control.sequenceNumber) {
			return
		}
		if !recovery.peerSeen {
			recovery.peerSynSequence = control.sequenceNumber
			recovery.peerSequence, recovery.peerSeen = control.sequenceNumber+1, true
			recovery.peerWindow = uint32(self.tcpWindowSize)
			if self.sequenceAdmissionStateSeen {
				recovery.peerWindow = uint32(self.sequenceAdmissionWindow)
			}
			recovery.windowScaleEnabled = recovery.windowScaleOffered && control.windowScaleEnabled
		}
	} else if !self.tcpRecoveryControlInWindowWithLock(control) {
		return
	}
	// Native range publication precedes every route write. An ACK beyond
	// this cohort's materialized high-water is not evidence of its delivery.
	// The exact SYN-ACK also covers the synchronous pre-commit legacy return.
	if !control.syn && int32(control.ackSequenceNumber-self.sequenceNumber) > 0 {
		return
	}
	if recovery.client == client && recovery.seen && int32(control.ackSequenceNumber-recovery.ack) <= 0 {
		return
	}
	recovery.advance()
	recovery.client, recovery.ack, recovery.seen = client, control.ackSequenceNumber, true
	// The SYN-ACK seeds a spent baseline. An interval spanning ISN+1 cannot
	// have been a future-only reorder refusal in this cohort; only a later
	// ordinary cumulative ACK advance supplies new recovery authority.
	recovery.used = control.syn
}

// Advance the coherent receive edge/window on each source-ordered egress
// control, including when no ingress packet separates two source ACKs. A
// reordered older cumulative ACK cannot restore its obsolete receive window.
func (self *multiClientChannelUpdate) observeTcpRecoveryEgressWithLock(control tcpControlObservation) {
	recovery := &self.sequenceRecovery
	if !control.syn && recovery.peerSeen && recovery.client == self.client.Load() &&
		int32(control.ackSequenceNumber-recovery.peerSequence) >= 0 {
		recovery.peerSequence = control.ackSequenceNumber
		recovery.peerWindow = uint32(control.windowSize)
		if recovery.windowScaleEnabled {
			recovery.peerWindow <<= recovery.windowScale
		}
	}
}

// Apply normal TCP sequence plausibility before treating an inner ACK as
// current-cohort evidence. The moving source cumulative ACK is the receive
// edge; a permanent SYN-relative bound would fail after half-space progress.
// Window scale applies only when both SYNs offered it, and never to SYN.
func (self *multiClientChannelUpdate) tcpRecoveryControlInWindowWithLock(control tcpControlObservation) bool {
	recovery := &self.sequenceRecovery
	if !recovery.peerSeen || recovery.client != self.client.Load() {
		return false
	}
	window := recovery.peerWindow
	length := control.payloadByteCount
	if control.fin {
		length++
	}
	start := int32(control.sequenceNumber - recovery.peerSequence)
	if window == 0 {
		return length == 0 && start == 0
	}
	if 0 <= start && uint32(start) < window {
		return true
	}
	last := int32(control.sequenceNumber + length - 1 - recovery.peerSequence)
	return length != 0 && 0 <= last && uint32(last) < window
}

func (self *parsedPacket) spansTcpRecoveryAck(ack uint32, payloadByteCount int) bool {
	path := self.ipPath
	if path.Syn || path.Rst {
		return false
	}
	start := path.SequenceNumber
	end := tcpPacketNextSequenceNumberWithPayloadByteCount(path, payloadByteCount)
	return int32(end-start) > 0 && int32(ack-start) >= 0 && int32(end-ack) > 0
}

func (self *multiClientChannelUpdate) canRecoverTcpWithLock(packet *parsedPacket, client *multiClientChannel) bool {
	recovery := &self.sequenceRecovery
	return recovery.seen && !recovery.reserved && !recovery.used && client != nil &&
		recovery.serial != ^uint64(0) &&
		client == self.client.Load() && client == recovery.client && !self.synGenerationAwaiting &&
		!self.pendingCollapseResetWithLock() && packet.spansTcpRecoveryAck(recovery.ack, len(packet.payload))
}

// Classifies immutable parsed metadata without reparsing borrowed packet bytes.
func (self *parsedPacket) pureTcpAck() bool {
	path := self.ipPath
	return path != nil && path.Protocol == IpProtocolTcp && path.Ack && !path.Syn && !path.Fin && !path.Rst &&
		path.TcpPayloadByteCount == 0 && len(self.payload) == 0
}

// Equivalent to IsTcpAckOnlyPacket for already-parsed group members. Repeated
// ACKs carry loss/control feedback; Transfer deduplicates its own retries.
// The whole group must qualify, preserving existing admission and budgets.
func (self *parsedPacketGroup) onlyPureTcpAcks() bool {
	if len(self.packets) == 0 {
		return false
	}
	for index := range self.packets {
		if !self.packets[index].pureTcpAck() {
			return false
		}
	}
	return true
}

// Recheck the offer atomically: overlapping concurrent retries share one
// lease, while independent sequence/control progress keeps its usual path.
// Reservation does not consume authority; refusal/disposal returns this token.
func (self *parsedPacketGroup) reserveTcpRecoveryWithLock(update *multiClientChannelUpdate, parent *RemoteUserNatMultiClient, admission *tcpCollapseAdmission) bool {
	if self.onlyPureTcpAcks() {
		return true
	}
	client := update.client.Load()
	for index := range self.packets {
		packet := &self.packets[index]
		if packet.ipPath.Rst || update.canUpdateSequenceForClientWithLock(packet, client, false) {
			return true
		}
	}
	if hold := parent.reliabilitySettings().TcpCollapseMaxHold; hold > 0 && update.releaseSequenceHoldWithLock(hold) {
		return true
	}
	for index := range self.packets {
		if update.canRecoverTcpWithLock(&self.packets[index], client) {
			update.sequenceRecovery.advance()
			if update.sequenceRecovery.serial == ^uint64(0) {
				// Exhausted flows fail open without ever issuing a Max token.
				return true
			}
			update.sequenceRecovery.reserved = true
			admission.recovery = update.sequenceRecovery.serial
			return true
		}
	}
	return false
}

func (self tcpCollapseAdmission) releaseTcpRecovery() {
	if self.update == nil || self.recovery == 0 {
		return
	}
	self.update.stateLock.Lock()
	defer self.update.stateLock.Unlock()
	self.releaseTcpRecoveryWithLock()
}

func (self tcpCollapseAdmission) releaseTcpRecoveryWithLock() {
	if self.update == nil || self.recovery == 0 {
		return
	}
	recovery := &self.update.sequenceRecovery
	if recovery.serial == self.recovery && !recovery.used {
		recovery.reserved = false
	}
}

// A group may contain several overlaps of the frontier. One disposed member
// cannot release authority while another such member is pending or retained.
func (self *parsedPacketGroup) releaseDisposedTcpRecoveryWithLock() {
	admission := self.collapseAdmission
	recovery := &admission.update.sequenceRecovery
	if admission.recovery == 0 || admission.recovery != recovery.serial || !recovery.reserved {
		return
	}
	for index := range self.packets {
		packet := &self.packets[index]
		if packet.spansTcpRecoveryAck(recovery.ack, packet.ipPath.TcpPayloadByteCount) {
			_, disposed, bit := self.collapseOwner.stateWithLock(index)
			if *disposed&bit == 0 {
				return
			}
		}
	}
	admission.releaseTcpRecoveryWithLock()
}

// An explicit unwritten terminal result revokes even a materialized lease;
// ordinary successful Transfer completion never rearms an unchanged frontier.
func (self tcpCollapseAdmission) revokeTcpRecoveryWithLock(err error) {
	if self.update == nil || self.recovery == 0 ||
		!packetTransferExpiredUnwritten(err) && !sendPackFailedUnwritten(err) {
		return
	}
	recovery := &self.update.sequenceRecovery
	if recovery.serial == self.recovery {
		recovery.reserved, recovery.used = false, false
	}
}

// Native publication calls this only for an actually retained member, using
// parsed length after its original root may have returned. An older original
// or a completion from before an ACK advance has no matching lease to spend.
func (self tcpCollapseAdmission) materializeTcpRecoveryWithLock(packet *parsedPacket, payloadByteCount int) {
	if self.update == nil || self.recovery == 0 {
		return
	}
	recovery := &self.update.sequenceRecovery
	if recovery.serial != ^uint64(0) && recovery.serial == self.recovery && recovery.reserved && recovery.client == self.update.client.Load() &&
		packet.spansTcpRecoveryAck(recovery.ack, payloadByteCount) {
		recovery.reserved, recovery.used = false, true
	}
}
