// Connect-health evidence follows the SYN offer through admission and local
// expiry. An unaccepted or provably unwritten input is not provider silence.
package connect

import (
	"errors"
	"sync/atomic"
	"time"
)

// One offer owns only scalars, never packet bytes or a flow generation. The
// channel links admitted, unresolved offers until terminal completion. Except
// for the source's atomic materialization bit, fields use channel stateLock.
type connectSynOffer struct {
	started  time.Time
	epoch    uint64
	previous *connectSynOffer
	next     *connectSynOffer
	linked   bool
	finished bool
	// Source range disposition precedes its chunk's terminal callback. A
	// successful SYN prefix must survive a different chunk's local expiry.
	materialized  atomic.Bool
	orderedAnswer atomic.Bool
	// Immutable when the group starts. The remaining scalars belong to its
	// exact flow's stateLock, separate from the channel's admission lock.
	orderedReturnEligible bool
	returnSource          Id
	returnReceiveToken    *byte
	returnEpoch           uint64
	returnNext            uint32
}

// Mixed exact local proof types remain local; any ambiguous leaf preserves
// debt. A materialized SYN is checked separately because joins omit nil chunks.
func packetGroupTransferUnwritten(err error) bool {
	if err == errSendPackExpiredUnwritten {
		return true
	}
	if _, ok := err.(*sendGroupCapacityError); ok {
		return true
	}
	// A superseded native opening has no wire ordinal when its source
	// certifies the original Pack was still unwritten.
	if unwritten, ok := err.(*sendPackUnwrittenError); ok && errors.Is(unwritten.cause, errNativeSynOpeningSuperseded) {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !packetGroupTransferUnwritten(cause) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return packetGroupTransferUnwritten(wrapped.Unwrap())
	}
	return false
}

// Created in the existing send-accounting lock, only for SYN-bearing input.
// An offer captures the receive epoch, not an admitted operation's clock.
func (self *multiClientChannel) newConnectSynOfferWithLock() *connectSynOffer {
	return &connectSynOffer{epoch: self.connectSynEpoch}
}

// A successful return admits unresolved evidence. A callback can already have
// completed it or a received SYN can already have acquitted the entire epoch.
func (self *multiClientChannel) resolveConnectSynAdmission(offer *connectSynOffer, admitted bool) {
	if offer == nil {
		return
	}
	if !admitted {
		self.finishConnectSynOffer(offer, false)
		return
	}
	self.observeConnectSynAdmission(offer)
}

// Source handoff or a successful public result proves accepted custody. The
// first observation owns the full clock; a delayed second observer cannot
// restart it, and time spent waiting for local admission is not provider debt.
func (self *multiClientChannel) observeConnectSynAdmission(offer *connectSynOffer) {
	if offer == nil {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if offer.finished || offer.linked || offer.orderedAnswer.Load() || offer.epoch != self.connectSynEpoch {
		return
	}
	if offer.started.IsZero() {
		offer.started = self.packetTransferNow()
	}
	// Earlier opaque completed evidence cannot be retired individually. This
	// offer cannot move its clock, so only its existing callback needs the token.
	if !self.connectSynConfirmedTime.IsZero() && !offer.started.Before(self.connectSynConfirmedTime) {
		return
	}
	offer.next = self.connectSynPending
	if offer.next != nil {
		offer.next.previous = offer
	}
	self.connectSynPending = offer
	offer.linked = true
	self.recordConnectSynTimeWithLock(offer.started)
}

// Definite local refusal/expiry removes only this offer. Success, materialized
// SYNs and ambiguous admitted failures retain evidence; the unchanged caller
// still applies the provider-error policy. Completion can precede Send's return.
func (self *multiClientChannel) finishConnectSynOffer(offer *connectSynOffer, retain bool) {
	if offer == nil {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if offer.finished {
		return
	}
	offer.finished = true
	if offer.epoch != self.connectSynEpoch {
		return
	}
	if retain && offer.started.IsZero() {
		// A terminal callback can prove admission before its public result.
		offer.started = self.packetTransferNow()
	}
	wasEarliest := self.packetStats.firstUnansweredSendSynTime.Equal(offer.started)
	if offer.linked {
		if offer.previous == nil {
			self.connectSynPending = offer.next
		} else {
			offer.previous.next = offer.next
		}
		if offer.next != nil {
			offer.next.previous = offer.previous
		}
		offer.previous, offer.next, offer.linked = nil, nil, false
	}
	if retain && !offer.orderedAnswer.Load() {
		if offer.orderedReturnEligible {
			for index, completed := range self.connectSynCompleted {
				if completed == nil || completed.orderedAnswer.Load() {
					self.connectSynCompleted[index] = offer
					self.recordConnectSynTimeWithLock(offer.started)
					return
				}
			}
		}
		self.confirmConnectSynWithLock(offer.started)
	} else if wasEarliest {
		self.recomputeConnectSynWithLock()
	}
}

// Completed opens without an individually tracked return retain their
// earliest clock until a received ordered SYN clears the episode. This also
// preserves stock evidence when the bounded exact-receipt slots are full.
func (self *multiClientChannel) confirmConnectSynWithLock(started time.Time) {
	if self.connectSynConfirmedTime.IsZero() || started.Before(self.connectSynConfirmedTime) {
		self.connectSynConfirmedTime = started
	}
	self.recordConnectSynTimeWithLock(started)
}

// Publishes the earliest evidence without changing a survivor's deadline.
func (self *multiClientChannel) recordConnectSynTimeWithLock(started time.Time) {
	if self.packetStats.firstUnansweredSendSynTime.IsZero() {
		self.wakeBlackholeWithLock()
	}
	if self.packetStats.firstUnansweredSendSynTime.IsZero() || started.Before(self.packetStats.firstUnansweredSendSynTime) {
		self.packetStats.firstUnansweredSendSynTime = started
	}
}

// A received SYN acquits all earlier offers, including publishers that are
// still inside Send. Their late callbacks cannot reopen this cleared episode.
func (self *multiClientChannel) clearConnectSynWithLock() {
	self.connectSynEpoch++
	for offer := self.connectSynPending; offer != nil; {
		next := offer.next
		offer.previous, offer.next, offer.linked = nil, nil, false
		offer = next
	}
	self.connectSynPending = nil
	self.connectSynConfirmedTime = time.Time{}
	clear(self.connectSynCompleted[:])
	self.packetStats.firstUnansweredSendSynTime = time.Time{}
}

// Recompute only when exact evidence was retracted or acquired an ordered
// answer. A different pending, completed or conservatively aggregated offer
// keeps its own original time.
func (self *multiClientChannel) recomputeConnectSynWithLock() {
	first := self.connectSynConfirmedTime
	for index, offer := range self.connectSynCompleted {
		if offer == nil {
			continue
		}
		if offer.orderedAnswer.Load() {
			self.connectSynCompleted[index] = nil
			continue
		}
		if first.IsZero() || offer.started.Before(first) {
			first = offer.started
		}
	}
	for offer := self.connectSynPending; offer != nil; offer = offer.next {
		if !offer.orderedAnswer.Load() && (first.IsZero() || offer.started.Before(first)) {
			first = offer.started
		}
	}
	self.packetStats.firstUnansweredSendSynTime = first
}

// A loan of the already-owned SYN token to its exact flow selection. The
// corresponding channel stores only the offer, never the flow or selection.
type tcpConnectSynReturn struct {
	offer     *connectSynOffer
	selection *multiClientAffinitySelection
}

func (self *multiClientChannel) isConnectSynReturnSource(source TransferPath) bool {
	return source.SourceId != (Id{}) && self != nil && self.args != nil && self.args.Destination.Len() != 0 &&
		source.SourceId == self.args.Destination.Tail()
}

// A SYN-ACK ahead of a Transfer gap may establish the application handshake,
// while its connect-health debt remains. Retire only this original opening
// once its own ordered payload proves both TCP directions through that edge.
func (update *multiClientChannelUpdate) observeConnectSynReturnWithLock(client *multiClientChannel, control tcpControlObservation, firstInbound bool) {
	proof := &update.connectSynReturn
	offer := proof.offer
	if offer.returnEpoch != update.sequenceAdmissionEpoch || offer.returnSource != update.sequenceSourceId ||
		proof.selection != update.client.loadSelection() || update.sourceReset.Load() || update.IsDone() {
		*proof = tcpConnectSynReturn{}
		return
	}
	if !control.selectedSource || !control.valid {
		return
	}
	if control.rst {
		*proof = tcpConnectSynReturn{}
		return
	}
	if !control.ack || control.fin || control.reliableReceiveToken == nil {
		return
	}
	wantAck := update.synGenerationNumber + 1
	if control.syn {
		if control.ackSequenceNumber != wantAck || control.payloadByteCount != 0 {
			return
		}
		if control.aheadOfGap && firstInbound {
			offer.returnReceiveToken = control.reliableReceiveToken
			offer.returnNext = control.sequenceNumber + 1
		} else if !control.aheadOfGap {
			*proof = tcpConnectSynReturn{}
		}
		return
	}
	if control.aheadOfGap || offer.returnReceiveToken != control.reliableReceiveToken ||
		control.payloadByteCount == 0 ||
		control.sequenceNumber != offer.returnNext ||
		(control.ackSequenceNumber != wantAck &&
			(!update.sequenceCovered || !tcpSequenceAtOrAfter(control.ackSequenceNumber, wantAck) ||
				!tcpSequenceAtOrAfter(wantAck, update.sequenceCoveredFrom) ||
				!tcpSequenceAtOrAfter(update.sequenceCoveredTo, control.ackSequenceNumber))) {
		return
	}
	offer.orderedAnswer.Store(true)
	// Do not take the channel lock under the flow lock. The same channel
	// observes this proof under its stateLock before judging a new verdict.
	client.connectSynAnswersPending.Store(true)
	*proof = tcpConnectSynReturn{}
}
