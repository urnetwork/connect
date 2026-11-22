// Endpoint enrollment can complete before StreamOpen. The bounded stream
// buffer transfers that proof to the matching pending/active stream lifetime.
package connect

import (
	"context"
	"time"
)

// At most the same number of early proofs as existing pending destination
// aliases. An expired or finished introducing sequence frees its own slot.
const maxPendingStreamForwardingEnrollments = maxPendingWriterStreamAliases

type streamForwardingEnrollment struct {
	ctx         context.Context
	grant       streamForwardingGrant
	keys        streamForwardingKeys
	destination bool
}

// A completed key negotiation cannot publish through a retired stream grant.
func (self *StreamManager) enrollStreamForwarding(ctx context.Context, grant streamForwardingGrant, keys streamForwardingKeys, destination bool) bool {
	if self == nil || ctx.Err() != nil || grant.expirationTimeUnixMillis <= time.Now().UnixMilli() {
		return false
	}
	enrollment := streamForwardingEnrollment{ctx: ctx, grant: grant, keys: keys, destination: destination}
	accepted := false
	self.withStreamAuthorization(grant.path.StreamId, func() {
		accepted = self.streamBuffer.enrollStreamForwarding(enrollment)
	})
	return accepted
}

// The association is published against the exact indexed owner. When no
// StreamOpen has yet been applied, retain one bounded pending proof.
func (self *StreamBuffer) enrollStreamForwarding(enrollment streamForwardingEnrollment) bool {
	self.managementStateLock.Lock()
	defer self.managementStateLock.Unlock()
	if self.closed || self.ctx.Err() != nil || !enrollment.live() {
		return false
	}
	self.mutex.Lock()
	defer self.mutex.Unlock()
	streamId := enrollment.grant.path.StreamId
	sequence := self.streamSequencesByStreamId[streamId]
	var current *streamForwardingAssociation
	if sequence != nil && sequence.forwarding != nil {
		current = sequence.forwarding.association.Load()
		if current.live() && enrollment.grant.path == current.grant.path.Reverse() {
			return sequence.matchesForwardingEnrollment(enrollment) && current.bindReciprocal(enrollment)
		}
		if current != nil && current.readinessKeys.transcript == enrollment.keys.transcript {
			// An exact repeat retains the current owner and never restarts
			// its counters on a retiring physical stream.
			return sequence.bindStreamForwarding(enrollment)
		}
	}
	if enrollment.grant.reciprocal {
		// A provider can answer the initial platform Pack before the
		// original grant is installed at either endpoint. It must never
		// choose the physical readiness keys.
		if sequence != nil && sequence.ctx.Err() == nil &&
			(!sequence.matchesForwardingEnrollment(enrollment) || current.live()) {
			return false
		}
		return self.retainForwardingEnrollment(enrollment)
	}
	var previousOwner *StreamSequence
	if sequence != nil && sequence.ctx.Err() == nil {
		if !sequence.matchesForwardingEnrollment(enrollment) {
			return false
		}
		if current == nil {
			return self.bindForwardingPrimary(sequence, enrollment)
		}
		if current.live() {
			select {
			case <-current.nativeSequenceDone:
			default:
				return false
			}
		}
		// Only new verified work can retire an association whose native
		// owner has already ended. A new StreamOpen gets the fresh proof.
		previousOwner = sequence
	}
	if !self.retainForwardingEnrollment(enrollment) {
		return false
	}
	if previousOwner != nil {
		previousOwner.Cancel()
	}
	return true
}

// A classified adjacent-only stream keeps its ordinary direct setup. An
// early reply can be staged until an authenticated StreamOpen arrives.
func (self *StreamBuffer) canOfferReciprocal(streamId Id) bool {
	if self == nil {
		return false
	}
	self.mutex.Lock()
	defer self.mutex.Unlock()
	sequence := self.streamSequencesByStreamId[streamId]
	return sequence == nil || sequence.forwarding != nil
}

func (self *StreamBuffer) retainForwardingEnrollment(enrollment streamForwardingEnrollment) bool {
	streamId := enrollment.grant.path.StreamId
	pending := &self.pendingForwardingEnrollments
	if enrollment.grant.reciprocal {
		pending = &self.pendingReciprocalEnrollments
	}
	if previous, found := (*pending)[streamId]; found && previous.live() {
		return previous.grant == enrollment.grant &&
			previous.keys.transcript == enrollment.keys.transcript &&
			previous.destination == enrollment.destination
	}
	for _, enrollments := range []map[Id]streamForwardingEnrollment{self.pendingForwardingEnrollments, self.pendingReciprocalEnrollments} {
		for id, previous := range enrollments {
			if !previous.live() {
				delete(enrollments, id)
			}
		}
	}
	if maxPendingStreamForwardingEnrollments <= len(self.pendingForwardingEnrollments)+len(self.pendingReciprocalEnrollments) || !enrollment.live() {
		return false
	}
	if *pending == nil {
		*pending = map[Id]streamForwardingEnrollment{}
	}
	(*pending)[streamId] = enrollment
	return true
}

// The already verified reverse proof belongs only to the exact primary
// logical pair, regardless of which delivery ACK completed first.
func (self *StreamBuffer) bindForwardingPrimary(sequence *StreamSequence, enrollment streamForwardingEnrollment) bool {
	if !sequence.bindStreamForwarding(enrollment) {
		return false
	}
	if reverse, found := self.pendingReciprocalEnrollments[sequence.streamId]; found {
		delete(self.pendingReciprocalEnrollments, sequence.streamId)
		if sequence.matchesForwardingEnrollment(reverse) {
			sequence.forwarding.association.Load().bindReciprocal(reverse)
		}
	}
	return true
}

// This bound applies before ownership passes to the live StreamSequence.
func (self streamForwardingEnrollment) live() bool {
	return self.ctx.Err() == nil && self.grant.expirationTimeUnixMillis > time.Now().UnixMilli()
}

// Validates logical endpoint identity against the exact local classified
// sequence. The trusted StreamOpen supplies only the immediate peer, whose
// legacy authority stays separate from the enrolled distant endpoint.
func (self *StreamSequence) matchesForwardingEnrollment(enrollment streamForwardingEnrollment) bool {
	if self.forwarding == nil || self.ctx.Err() != nil || !enrollment.live() ||
		self.streamId != enrollment.grant.path.StreamId {
		return false
	}
	localId, remoteId := enrollment.grant.path.SourceId, enrollment.grant.path.DestinationId
	if enrollment.destination {
		localId, remoteId = remoteId, localId
	}
	peerId := self.sourceId
	if peerId == nil {
		peerId = self.destinationId
	}
	if peerId == nil || localId != self.streamManager.client.ClientId() || remoteId == *peerId {
		return false
	}
	// The platform orders StreamOpen edges by logical endpoint ID, so the
	// orientation is the same for the two directions of the signed grant.
	if (self.sourceId == nil) == (self.destinationId == nil) ||
		(self.sourceId != nil) != remoteId.LessThan(localId) {
		return false
	}
	return true
}

func (self *StreamSequence) bindStreamForwarding(enrollment streamForwardingEnrollment) bool {
	if enrollment.grant.reciprocal || !self.matchesForwardingEnrollment(enrollment) {
		return false
	}
	if current := self.forwarding.association.Load(); current != nil {
		return current.live() && current.grant == enrollment.grant &&
			current.readinessKeys.transcript == enrollment.keys.transcript && current.destination == enrollment.destination
	}
	association, valid := newStreamForwardingAssociation(self.ctx, enrollment.grant,
		enrollment.keys, enrollment.destination, self.forwarding.readiness)
	if !valid {
		return false
	}
	association.nativeSequenceDone = enrollment.ctx.Done()
	// The raw receive/send path must see its association before any writer can
	// be granted by authenticated readiness.
	self.forwarding.association.Store(association)
	if !association.installReadiness() {
		self.forwarding.association.CompareAndSwap(association, nil)
		return false
	}
	return true
}

// Await retirement of the logical owner. All physical reconnects share the
// same signed deadline and the existing stream teardown joins their workers.
func (self *StreamSequence) waitForRetirement() {
	if self.forwarding == nil {
		<-self.ctx.Done()
		return
	}
	readiness := self.forwarding.readiness
	select {
	case <-self.ctx.Done():
		return
	case <-readiness.installed:
	}
	deadline := time.UnixMilli(readiness.keys.expirationTimeUnixMillis)
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-self.ctx.Done():
	case <-timer.C:
		self.cancel()
	}
}
