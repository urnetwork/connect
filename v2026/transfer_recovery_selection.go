// Recovery selection belongs to the send worker even while its current write
// waits for physical service. A selection owns no wire, waiter or flight bytes.
package connect

import (
	"errors"
	"fmt"
	"time"
)

// The selected item's ordinary ACK owner remains in the resend queue. Exact
// identity and scheduling stamps reject a freed, acknowledged or rewritten item
// before this record is used; no pooled item pointer survives that validation.
type sendRecoverySelection struct {
	messageId    Id
	number       uint64
	sendTime     time.Time
	due          time.Time
	sendCount    int
	at           time.Time
	kind         sendRecoveryKind
	holeCarrier  gapHoleCarrier
	reliableOnly bool
}

type sendRecoverySelectionOutcome uint8

const (
	sendRecoverySelectionReady sendRecoverySelectionOutcome = iota
	sendRecoverySelectionDeferred
	sendRecoverySelectionFeedback
)

// Only the sequence worker reads this record. Feedback may still be pending in
// the coalescer: discard its stale selection before the ordinary owner applies
// delivery or a missing-contract rewrite, without taking or crediting that ACK.
func (self *SendSequence) selectedRecoveryItem() *sendItem {
	selection := self.pendingRecovery
	if selection == nil {
		return nil
	}
	item := self.resendQueue.GetByMessageId(selection.messageId)
	if item == nil || item.sequenceNumber != selection.number ||
		item.sendTime != selection.sendTime || item.resendTime != selection.due ||
		item.sendCount != selection.sendCount || item.recoveryKind != sendRecoveryNone ||
		item.selectiveAcked ||
		self.ackWindow != nil && self.ackWindow.PendingDispositionFor(selection.number, selection.messageId) {
		self.pendingRecovery = nil
		return nil
	}
	return item
}

// The current original keeps its exact call stack, wire share and charged FIFO
// reservation. Select at most one older retry without recursively dispatching
// through that same waiter. The outer worker writes it before any fresh Pack.
func (self *windowPacingWriteStart) recoveryDeadline(now time.Time) (time.Time, error) {
	if self == nil || self.owner == nil || self.recovery {
		return time.Time{}, nil
	}
	owner := self.owner
	if owner.resendQueue == nil || owner.ackWindow == nil {
		return time.Time{}, nil
	}
	if owner.selectedRecoveryItem() != nil {
		return time.Time{}, nil
	}
	feedbackPending := false
	for {
		// The current unwritten original may lead the timer heap after an
		// older lane defers. Never recover that write or lose the next older
		// deadline behind it. This exceptional scan creates no second queue.
		var policy transferFlightPolicySnapshot
		item := func() *sendItem {
			if feedbackPending {
				// Feedback can cover the heap prefix without covering its tail.
				// Only this worker owns retained membership and item clocks;
				// inspect that existing owner without nesting queue/ACK locks.
				policy = owner.transferFlightPolicy()
				var oldest *sendItem
				for _, candidate := range owner.sendItems {
					if candidate != nil && candidate.messageId != self.messageId && !candidate.resendTime.IsZero() &&
						!owner.recoveryFeedbackPending(candidate, policy, owner.ackWindow) &&
						(oldest == nil || candidate.resendTime.Before(oldest.resendTime)) {
						oldest = candidate
					}
				}
				return oldest
			}
			queue := owner.resendQueue
			queue.stateLock.Lock()
			defer queue.stateLock.Unlock()
			if len(queue.orderedItems) == 0 {
				return nil
			}
			first := queue.orderedItems[0]
			if first.messageId != self.messageId && !first.resendTime.IsZero() {
				return first
			}
			var oldest *sendItem
			for _, candidate := range queue.orderedItems {
				if candidate.messageId != self.messageId && !candidate.resendTime.IsZero() &&
					(oldest == nil || queue.cmp(candidate, oldest) < 0) {
					oldest = candidate
				}
			}
			return oldest
		}()
		if item == nil {
			return time.Time{}, nil
		}
		if now.Before(item.resendTime) {
			return item.resendTime, nil
		}
		if err := owner.ctx.Err(); err != nil {
			return time.Time{}, err
		}
		if !feedbackPending {
			policy = owner.transferFlightPolicy()
		}
		item.ackTimeout = max(item.ackTimeout, owner.ackTimeoutForPolicy(item.unreliableRecoveryPolicy()))
		owner.ackLifetimes.update(item)
		if _, err := self.lifetime(now); err != nil {
			return time.Time{}, err
		}
		selection, outcome, err := owner.selectDueRecovery(item, now, policy, owner.ackWindow)
		if err != nil {
			// The ordinary owner returns on this same terminal preparation
			// failure. Do not turn it into an ignored initial route failure.
			owner.cancel()
			return time.Time{}, err
		}
		switch outcome {
		case sendRecoverySelectionReady:
			owner.pendingRecovery = &selection
			return time.Time{}, nil
		case sendRecoverySelectionFeedback:
			// Keep ACK application with Run, but do not let its covered prefix
			// hide an eligible deadline. The rescan omits every covered item.
			feedbackPending = true
		}
	}
}

// One eligibility predicate serves the exceptional retained scan and the due
// decision. The latter rechecks live ACK ingress before any item is changed.
func (self *SendSequence) recoveryFeedbackPending(
	item *sendItem,
	flightPolicy transferFlightPolicySnapshot,
	ackWindow *sequenceAckWindow,
) bool {
	unreliableTimeout := item.recoveryKind == sendRecoveryNone && item.unreliableFlightTracked
	h1ProgressTimeout := item.recoveryKind == sendRecoveryNone &&
		self.sendBufferSettings.DeferTimeoutResendWhileCumulativeProgress &&
		item.reliableCarrierObserved && !item.unreliableCarrierObserved &&
		!item.carrierChanged && flightPolicy.h1Only
	return ackWindow.PendingDispositionFor(item.sequenceNumber, item.messageId) ||
		(unreliableTimeout || h1ProgressTimeout) && ackWindow.PendingCumulativeProgress()
}

// Shared by the outer recovery loop and an initial physical wait. Every real
// lane/ACK/timeout decision is made here once; physical dispatch is separate so
// it cannot overwrite a younger write's existing FIFO entry or charge it twice.
func (self *SendSequence) selectDueRecovery(
	item *sendItem,
	sendTime time.Time,
	flightPolicy transferFlightPolicySnapshot,
	ackWindow *sequenceAckWindow,
) (sendRecoverySelection, sendRecoverySelectionOutcome, error) {
	retainPastAckTimeout := item.acks.retainPastAckTimeout()
	if self.sendBuffer != nil && self.sendBuffer.beforeDueResendForTest != nil {
		self.sendBuffer.beforeDueResendForTest(self.id(), item.sequenceNumber)
	}
	// An Ack may have reached the coalescer after this iteration took
	// its snapshot. Apply that receiver evidence before an already-due
	// recovery write; otherwise a busy sender can emit one spurious
	// retransmit for every snapshot/arrival race. The lock is paid only
	// on the due-recovery path, never for an ordinary initial write.
	unreliableTimeout := item.recoveryKind == sendRecoveryNone && item.unreliableFlightTracked
	// Stable H1 timeouts also defer while a cumulative prefix is
	// draining. Apply newly coalesced lower progress before consulting
	// lastCumulativeAckTime; explicit recovery keeps its own boundary.
	if self.recoveryFeedbackPending(item, flightPolicy, ackWindow) {
		self.client.ackPendingResendPreemptCount.Add(1)
		return sendRecoverySelection{}, sendRecoverySelectionFeedback, nil
	}
	if unreliableTimeout && !self.lastCumulativeAckTime.IsZero() {
		// A draining prefix is not silence: restart the ordinary
		// datagram timer on cumulative progress. Per-item age alone
		// retransmits and contracts an entire healthy delayed flight.
		// Selective gaps still recover immediately, and the existing
		// interval bounds a tail once cumulative progress stops.
		deadline := self.lastCumulativeAckTime.Add(self.resendIntervalForItem(item, item.sendCount))
		if sendTime.Before(deadline) {
			self.setResendTime(item, deadline)
			self.client.timeoutResendDeferCount.Add(1)
			return sendRecoverySelection{}, sendRecoverySelectionDeferred, nil
		}
	}
	laneVerdict := laneTimerNotApplicable
	if item.recoveryKind == sendRecoveryNone {
		laneVerdict = self.laneTimerVerdictFor(item)
	}
	// Confirmed raw H1 residence applies to an unproved ordinary
	// timeout, anchored to this item's actual first physical write.
	// Explicit recovery and a proved same-lane hole retain their
	// own due boundary. Drained probes share the same fixed bound.
	if service := self.windowPacer.service; service != nil &&
		item.recoveryKind == sendRecoveryNone && laneVerdict != laneTimerEndpointDrop && item.sendCount == 1 &&
		item.reliableCarrierObserved && !item.unreliableCarrierObserved &&
		!item.carrierChanged && flightPolicy.h1Only {
		deadline := service.probeRecoveryDeadline(self.sequenceId, item.messageId,
			self.sendBufferSettings.RttScale, self.sendBufferSettings.MaxResendInterval)
		if interval := self.sharedRawRecoveryInterval(item, sendTime); interval > 0 && item.pacingSentAtNanos != 0 {
			physicalDeadline := self.firstPhysicalRecoveryTime(item).Add(max(interval, self.resendIntervalForItem(item, 1)))
			if deadline.Before(physicalDeadline) {
				deadline = physicalDeadline
			}
		}
		if !retainPastAckTimeout && deadline.After(item.sendTime.Add(item.ackTimeout)) {
			deadline = item.sendTime.Add(item.ackTimeout)
		}
		if sendTime.Before(deadline) {
			self.setResendTime(item, deadline)
			return sendRecoverySelection{}, sendRecoverySelectionDeferred, nil
		}
	}
	self.preferH3AfterH1Timeout(item)
	self.detachResendItem(item.messageId)

	// A selective recovery is receiver-paced evidence rather than an
	// RTO. Consume its marker before the write and do not increase the
	// item's timeout backoff; a lost recovery returns to its prior
	// ordinary cadence. Any resend awaits fresh acknowledgement state.
	recoveryKind := item.recoveryKind
	// Attribute the hole before a successful retry can change the
	// item's carrier. Recovery on a direct lane does not make an
	// earlier relay-carried hole a direct-lane loss.
	holeCarrier := gapHoleCarrierOf(item)
	item.recoveryKind = sendRecoveryNone
	// §34.3: what this firing means is decided by this item's own
	// lane and by its position in it. Anything acknowledged above
	// it means write it; anything below it since it last looked
	// means the lane is draining toward it, so wait; neither means
	// write it if it is the lane's oldest unacknowledged item and
	// otherwise ride that head.
	if recoveryKind == sendRecoveryNone {
		if laneVerdict != laneTimerNotApplicable {
			// this firing has now looked: the next one asks what
			// moved on this lane since
			if highest, acked := self.laneHighestAcked(item.carrierRoute); acked {
				item.laneAckedAtLastFiring = highest
			}
		}
		self.observeReliableLaneFiring(
			item,
			self.resendIntervalForItem(item, item.sendCount),
			sendTime,
		)
	}
	if laneVerdict == laneTimerEndpointDrop {
		// the route delivered past this item, so it was dropped at
		// an endpoint: written with backoff, as today
		self.client.laneProvenTimeoutWriteCount.Add(1)
	} else if laneVerdict == laneTimerSilent {
		// §34.3 rule 3. Nothing on this item's lane has moved
		// since it last looked, so no acknowledgement is coming to
		// prove it and none will: the receiver's own drops can
		// remove every later same-lane item, which is how the
		// proof chain breaks. The lane's oldest unacknowledged
		// item is written on its own timer, with setHead where it
		// is the sequence head, which is also the only path that
		// re-establishes a receiver that silently lost the
		// sequence. Everything else on the lane rides that head,
		// so one write recovers a dropped batch a position at a
		// time rather than a window at a time.
		head := self.laneOldestOutstanding(item.carrierRoute)
		if head != nil && head != item {
			// Held until the head's next firing. When the head is
			// due in this pass and not yet written, that is the
			// interval its write is about to schedule; never a
			// time already past, which would spin this item
			// through the loop.
			holdUntil := head.resendTime
			if !sendTime.Before(holdUntil) {
				holdUntil = sendTime.Add(
					self.resendIntervalForItem(head, head.sendCount+1))
			}
			item.resendTime = holdUntil
			item.recoveryKind = sendRecoveryNone
			self.addResendItem(item)
			self.client.laneProbeRideCount.Add(1)
			return sendRecoverySelection{}, sendRecoverySelectionDeferred, nil
		}
		self.client.laneProbeWriteCount.Add(1)
	} else if laneVerdict == laneTimerDraining {
		// §34.3 rule 2. Something below this item was
		// acknowledged on its own lane since it last looked, so
		// on a FIFO lane the lane is draining toward it and it is
		// next: re-armed with backoff, no limit, no since-last
		// term and no estimate read. §32.4 re-armed here on the
		// absence of a later same-lane acknowledgement instead,
		// which is unbounded when the receiver's own drops remove
		// every item that could carry that proof; this re-arm
		// rests on an acknowledgement that arrived, so a lane
		// that stops answering leaves it at once. This is §13.5's
		// deferral and §27.3's ride collapsed into the one action
		// they were both answers to.
		// The count is advanced before the interval is read, so
		// the re-arms keep the rewrite's own timer: a timer that
		// fired at one interval is next due at three, then seven.
		item.timeoutDeferCount += 1
		item.timeoutDeferAckTime = self.lastCumulativeAckTime
		item.deferralOutstanding = true
		// No bound on this re-arm, and in particular no
		// liveness due time to clamp the sequence head to.
		// Re-establishing a receiver that silently lost the
		// sequence needs a setHead rewrite, and rule 3 is what
		// produces it: a receiver in that state acknowledges
		// nothing, so nothing on the lane ever moves, so the head
		// is never held here in the first place.
		item.resendTime = sendTime.Add(
			self.deferredResendInterval(item, self.rttWindow.ScaledRtt()))
		self.addResendItem(item)
		self.client.timeoutResendDeferCount.Add(1)
		return sendRecoverySelection{}, sendRecoverySelectionDeferred, nil
	} else if recoveryKind == sendRecoveryNone && !self.lastCumulativeAckTime.IsZero() {
		scaledRtt := self.rttWindow.ScaledRtt()
		if sendTime.Sub(self.lastCumulativeAckTime) < scaledRtt {
			// M4: a whole-window timeout while the cumulative ack is
			// still advancing is the spurious cascade, not a stalled lane.
			self.client.timeoutResendWithRecentCumulativeProgress.Add(1)
		}
		if self.shouldDeferTimeoutResend(item, scaledRtt) {
			// FLIGHTGATEFIX §13.5 (F12): the cumulative ack advanced
			// within one scaled RTT of this item's send, so the reliable
			// lane is alive and its queue is deeper than the estimate.
			// Wait one more round trip. Each further deferral needs the
			// cumulative ack to have advanced since the last one, so a
			// hole nothing can acknowledge is deferred once and then
			// retransmitted: deferring is right while the queue drains
			// and wrong once the Pack is gone (§16).
			deferInterval := self.deferredResendInterval(item, scaledRtt)
			item.timeoutDeferCount += 1
			item.timeoutDeferAckTime = self.lastCumulativeAckTime
			item.deferralOutstanding = true
			item.resendTime = sendTime.Add(deferInterval)
			self.addResendItem(item)
			self.client.timeoutResendDeferCount.Add(1)
			return sendRecoverySelection{}, sendRecoverySelectionDeferred, nil
		}
	}
	// the deferral, if any, is over: this timeout is being written
	item.deferralOutstanding = false
	reliableOnlyResend := false
	if recoveryKind == sendRecoveryNone && item.unreliableFlightTracked {
		reliableOnlyResend = self.observeUnreliableResendTimeout(item, flightPolicy)
	}
	item.selectiveAcked = false

	// resend
	var transferFrameBytes []byte
	if self.sendItems[0].sequenceNumber == item.sequenceNumber &&
		!item.head {
		// Set head after cumulative progress. A negotiated compact head stays
		// compact through every loss recovery; only an explicit receiver
		// request reconstructs its complete contract.
		var err error
		var hasContractFrame bool
		transferFrameBytes, hasContractFrame, err = self.setHead(item, false)
		if err != nil {
			self.log.Errorf("[s]%s->%s...%s s(%s) exit could not set head = %s\n", self.client.ClientTag(), self.contractIntermediaryIds(), self.destination, self.contractMultiRouteWriterAlias.StreamId, err)
			self.recordSendSequenceExit("head_rewrite", item, time.Time{}, err)
			return sendRecoverySelection{}, sendRecoverySelectionDeferred, err
		}
		self.replaceSendItemFrame(item, transferFrameBytes)
		item.head = true
		item.hasContractFrame = hasContractFrame
		item.promotedHead = true
	}

	// Selection keeps the exact item in its one ACK/retention owner while a
	// younger physical write finishes. Only dispatch spends new service.
	self.addResendItem(item)
	return sendRecoverySelection{
		messageId: item.messageId, number: item.sequenceNumber,
		sendTime: item.sendTime, due: item.resendTime, sendCount: item.sendCount,
		at: sendTime, kind: recoveryKind, holeCarrier: holeCarrier,
		reliableOnly: reliableOnlyResend,
	}, sendRecoverySelectionReady, nil
}

// The existing physical retry tail is shared by Run and the next initial entry
// after a synchronous contract announcement. Its caller has consumed selection;
// resend entry never selects or dispatches recursively through the active waiter.
func (self *SendSequence) writeSelectedRecovery(item *sendItem, selection sendRecoverySelection) error {
	// Selection can precede this entry by a younger write's entire wait.
	// Its policy timestamp is not time spent attempting this retry.
	recoveryStart := time.Now()
	retainPastAckTimeout := item.acks.retainPastAckTimeout()
	recoveryKind, holeCarrier := selection.kind, selection.holeCarrier
	reliableOnlyResend := selection.reliableOnly
	transferFrameBytes := item.transferFrameBytes

	// resend uses the same path the item was originally sent on
	resendPath := sendTransferPath(self.client.ClientId(), DestinationId(self.destination))
	resendBytes := transferFrameBytes
	resendForceUnwrapped := item.forceUnwrapped
	previousPacedWrite := self.windowPacer.waiter.sentAt
	var resendDisposition transferWriteDisposition
	var resendErr error
	// Selection retained this immutable envelope's ACK lookup and
	// byte ownership through the current FIFO write and this retry.
	c := func() error {
		var writeErr error
		resendDisposition, writeErr = self.writeMaybeWrappedBytes(
			resendBytes,
			resendPath,
			resendForceUnwrapped,
			item,
			true,
			reliableOnlyResend,
		)
		return writeErr
	}
	if self.log.V(2).Enabled() {
		resendErr = TraceWithReturn(
			fmt.Sprintf(
				"[s]resend %d multi route write %s->%s...%s s(%s)",
				item.sequenceNumber,
				self.client.ClientTag(),
				self.contractIntermediaryIds(),
				self.destination,
				self.contractMultiRouteWriterAlias.StreamId,
			),
			c,
		)
	} else {
		resendErr = c()
		if resendErr != nil {
			if self.log.V(1).Enabled() {
				self.log.Infof("[s]resend drop = %s", resendErr)
			}
		}
	}
	if errors.Is(resendErr, errWindowPacingAcknowledged) || errors.Is(resendErr, errSendAckLifetime) {
		return resendErr
	}
	self.detachResendItem(item.messageId)
	if resendErr == nil {
		if !item.transportWriteObserved {
			item.transportWriteObserved = true
			item.acks.observeTransportWrite(resendDisposition.transportType)
		}
		self.observeCarrierWrite(item, resendDisposition)
		self.resendWriteCount.Add(1)
		self.resendWriteByteCount.Add(uint64(len(transferFrameBytes)))
	}
	self.client.recordSendRecovery(recoveryKind, resendErr)
	if recoveryKind == sendRecoverySelectiveGap && 0 < item.timeoutDeferCount {
		// a recovery the deferred retransmit declined to write
		// and the scoreboard wrote instead (FLIGHTGATEFIX §23.3)
		self.client.selectiveGapWritesOfDeferredItems[holeCarrier].Add(1)
	}
	if recoveryKind == sendRecoverySelectiveGap &&
		self.scheduleGapRecoveryProbe(
			item,
			time.Now(),
			item.sendTime.Add(self.sendBufferSettings.AckTimeout),
		) {
		self.addResendItem(item)
		return nil
	}

	if recoveryKind == sendRecoveryNone {
		item.sendCount += 1
	}
	// back off the resend timeout multiplicatively with each resend
	// of the same item, up to `MaxResendInterval`. When acks are
	// delayed (not lost) by queueing, a flat timeout re-sends the
	// whole in-flight window every interval, and the duplicates
	// feed the congestion that delayed the acks in the first place.
	// §34.3: a lane head written under rule 3 is re-armed on its
	// own backed-off interval. eeca11f re-armed it on a fixed
	// cold cadence instead, justified as a constant that cannot
	// lag, and that justification is true but not sufficient: a
	// constant still fires while the lane is demonstrably
	// draining, which is the M4 failure in a new place. Rule 2
	// already answers that with a fact about position, and a fact
	// beats a constant, so the cadence goes.
	itemResendTimeout := self.resendIntervalForItem(item, item.sendCount)
	if resendErr == nil && resendDisposition.transportType == TransportTypeH1 &&
		self.windowPacer.waiter.sentAt.After(previousPacedWrite) &&
		self.windowPacer.waiter.sentAt.After(recoveryStart) {
		// A paced retry starts its next backoff at the physical write.
		// The reusable waiter is owned by this sequence worker.
		recoveryStart = self.windowPacer.waiter.sentAt
	}
	item.resendTime = recoveryStart.Add(itemResendTimeout)
	if !retainPastAckTimeout && item.resendTime.After(item.sendTime.Add(item.ackTimeout)) {
		item.resendTime = item.sendTime.Add(item.ackTimeout)
	}
	self.addResendItem(item)
	// A paced recovery write can take a complete service interval.
	// Apply ACKs received during it before deciding whether another
	// timeout is still needed or measuring the next pacing rate.
	return nil
}
