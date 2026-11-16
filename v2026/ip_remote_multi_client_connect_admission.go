// Connect-health evidence follows the SYN offer through admission and local
// expiry. An unaccepted or provably unwritten input is not provider silence.
package connect

import (
	"sync/atomic"
	"time"
)

// One offer owns only scalars, never packet bytes or a flow generation. The
// channel links admitted, unresolved offers until terminal completion. Except
// for the source's atomic materialization bit, fields use channel stateLock.
type connectSynOffer struct {
	started            time.Time
	epoch              uint64
	previous           *connectSynOffer
	next               *connectSynOffer
	linked             bool
	finished           bool
	candidate          *connectSynOwner
	ownershipAmbiguous bool
	owner              atomic.Pointer[connectSynOwner]
	publicReturned     atomic.Bool
	terminalFinished   atomic.Bool
	referencesReleased atomic.Bool
	// Source range disposition precedes its chunk's terminal callback. A
	// successful SYN prefix must survive a different chunk's local expiry.
	materialized atomic.Bool
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

// Whole-Pack pre-serialization proof also retracts SYN silence evidence.
// This does not excuse its underlying provider error. Joined chunks must each
// prove unwritten; the caller separately protects any materialized SYN prefix.
func packetGroupConnectSynUnwritten(err error) bool {
	if err == errSendPackExpiredUnwritten {
		return true
	}
	switch err.(type) {
	case *sendGroupCapacityError, *sendPackUnwrittenError:
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !packetGroupConnectSynUnwritten(cause) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return packetGroupConnectSynUnwritten(wrapped.Unwrap())
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
	defer offer.publicReturned.Store(true)
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
	if offer.finished || offer.linked || offer.epoch != self.connectSynEpoch {
		return
	}
	if offer.started.IsZero() {
		offer.started = self.packetTransferNow()
	}
	// Earlier completed evidence can no longer be retracted. This offer
	// cannot move its clock, so only its existing callback needs the token.
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
	var releaseOffer bool
	defer func() {
		self.releaseConnectSynOwners()
		if releaseOffer {
			offer.terminalFinished.Store(true)
		}
	}()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if offer.finished {
		return
	}
	offer.finished, releaseOffer = true, true
	if offer.epoch != self.connectSynEpoch {
		return
	}
	if retain && offer.started.IsZero() {
		offer.started = self.packetTransferNow()
	}
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
	if retain && !self.retainConnectSynOwnerWithLock(offer) {
		self.confirmConnectSynWithLock(offer.started)
	}
	self.recomputeConnectSynWithLock()
}

// A completed SYN cannot later become a local refusal. Retain just its earliest
// time until a received SYN clears the episode, independent of stats rotation.
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
	self.clearConnectSynOwnersWithLock()
	for offer := self.connectSynPending; offer != nil; {
		next := offer.next
		offer.previous, offer.next, offer.linked = nil, nil, false
		offer = next
	}
	self.connectSynPending = nil
	self.connectSynConfirmedTime = time.Time{}
	self.packetStats.firstUnansweredSendSynTime = time.Time{}
}
