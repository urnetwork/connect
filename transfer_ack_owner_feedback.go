package connect

// Run is the sole draining owner. Receiving/accepted feedback keeps its
// existing retained-identity, cumulative and selective delivery authority.
func (self *SendSequence) applyAcceptedAckSnapshot(snapshot sequenceAckWindowSnapshot) {
	if 0 < snapshot.ackUpdateCount {
		if arrived := self.ackArrivalTime(snapshot.headAck.receivedAtNanos); self.lastHeadAckTime.Before(arrived) {
			self.lastHeadAckTime = arrived
		}
		self.receiveAckFeedbackAt(
			snapshot.headAck.messageId,
			false,
			snapshot.headAck.tag,
			snapshot.headAck.compactContractRecoverySupported,
			snapshot.headAck.receivedAtNanos,
			snapshot.headAck.receiverTiming,
			snapshot.headAck,
		)
	}
	for messageId, ack := range snapshot.selectiveAcks {
		if ack.deliveredImmediately && self.receiveImmediateAck(ack) {
			continue
		}
		self.receiveAckFeedbackAt(
			messageId,
			true,
			ack.tag,
			ack.compactContractRecoverySupported,
			ack.receivedAtNanos,
			ack.receiverTiming,
		)
	}
}

// Cancellation has rejected and joined further ACK publishers before this
// call. A cumulative delivery already accepted by that boundary must complete
// its original successfully before the remaining owners receive close errors.
// Selective receipt never completes delivery. Contract requests are not delivery
// and are intentionally not used to prepare a new recovery during close.
func (self *SendSequence) finishAcceptedAcknowledgements(ackWindow *sequenceAckWindow) {
	if ackWindow == nil {
		return
	}
	// Only directly constructed legacy sequences still use the ACK channel.
	// The compatibility worker is joined; take each remaining accepted value
	// once through the same coalescer before the final owner snapshot.
	if self.acks != nil {
	legacyDrain:
		for {
			select {
			case ack, ok := <-self.acks:
				if !ok {
					break legacyDrain
				}
				self.coalesceReceivedAck(ackWindow, ack)
			default:
				break legacyDrain
			}
		}
	}
	// Run may return after capturing a withdrawal but before applying it.
	// Reuse that owned cut; retaking while it is in flight would panic.
	queue := self.resendQueue
	queue.stateLock.Lock()
	phase := self.pendingEvictionInFlight
	queue.stateLock.Unlock()
	evictions := pendingEvictionBatch{sequenceId: self.sequenceId, phase: phase}
	if phase == 0 {
		evictions = self.takePendingEvictions()
	}
	// Publishers have joined. Capture whether the other phase already exists
	// before consuming its causal ACK; processing the first cut can otherwise
	// create another marker for a detached owner during close.
	queue.stateLock.Lock()
	nextPhase := self.pendingEvictionPhaseWithLock()
	hasNext := self.pendingEvictionPhases&nextPhase != 0
	queue.stateLock.Unlock()
	self.applyAcceptedAckSnapshot(ackWindow.Snapshot(true))
	self.applyPendingEvictions(evictions)
	if hasNext {
		self.applyPendingEvictions(self.takePendingEvictions())
	}
}
