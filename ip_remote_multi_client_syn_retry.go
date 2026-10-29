// An explicitly failed unanswered SYN can retry from bounded source-owned
// bytes, without waiting for another native RTO. The existing parent reaper
// dispatches work; no packet, timer or goroutine is created by a failure callback.
package connect

import (
	"bytes"
	"time"
	"unsafe"

	"github.com/urnetwork/connect/protocol"
)

// Original metadata is immutable after publication. A retry carries only a
// numeric owner token and the existing flow, never the cached packet owner.
type tcpSynRetryRequest struct {
	source      TransferPath
	provideMode protocol.ProvideMode
	pin         flowPin
	update      *multiClientChannelUpdate
	serial      uint64
	// Nonzero only for a continuation of this exact unanswered wide race.
	raceOrder uint64
}

// The flow lock guards cache/queue references. Packet and credit remain live
// until both the cache and queued/in-flight dispatcher have released them.
// Queue links use the parent lock; release links use the flow lock.
type tcpSynRetry struct {
	update                               *multiClientChannelUpdate
	packet                               []byte
	credit                               *transferRetainedControlCredit
	readiness                            *tcpSynRetryReadiness
	readinessWidth                       int
	readinessEpoch, readinessSourceOrder uint64
	readinessOverflow                    bool
	source                               TransferPath
	provideMode                          protocol.ProvideMode
	pin                                  flowPin
	sequence                             uint32
	serial                               uint64
	references                           uint32
	queued                               bool
	// Queue-local token; the dispatcher snapshots it before releasing the lock.
	raceOrder uint64
	next      *tcpSynRetry
	released  *tcpSynRetry
}

func (self *parsedPacketGroup) isSynRetry() bool {
	return self.synRetry != nil && self.synRetry.serial != 0
}

func (self *parsedPacketGroup) prepareSynRetry(source TransferPath, mode protocol.ProvideMode, parent *RemoteUserNatMultiClient, update *multiClientChannelUpdate, path *IpPath) {
	if path == nil || path.Protocol != IpProtocolTcp || !path.Syn || path.Ack || self.synRetry != nil || parent.flowReaperDone == nil {
		return
	}
	if update.receivedInbound.Load() {
		update.stateLock.Lock()
		established := update.receivedInbound.Load() && update.synGenerationSeen && update.synGenerationNumber == path.SequenceNumber
		update.stateLock.Unlock()
		if established {
			return
		}
	}
	self.synRetry = &tcpSynRetryRequest{source: source, provideMode: mode, pin: self.pin}
}

func (self *tcpSynRetry) validWithLock(update *multiClientChannelUpdate) bool {
	return self != nil && self.update == update && update.synRetry == self &&
		!update.IsDone() && !update.sourceReset.Load() && !update.receivedInbound.Load() &&
		!update.affinitySources.closed && !update.pendingCollapseResetWithLock() &&
		!self.pendingConflictingControlWithLock(update) &&
		(!update.synGenerationSeen || update.synGenerationNumber == self.sequence)
}

// A queued sibling SYN from this same retained attempt does not introduce a
// different control generation. Every other pending SYN/RST remains a fence.
// This applies at both candidate preparation and source materialization.
func (self *tcpSynRetry) pendingConflictingControlWithLock(update *multiClientChannelUpdate) bool {
	if !update.pendingCollapseCloseWithLock() {
		return false
	}
	race := update.race
	if race == nil || race.single || update.client.Load() != nil {
		return true
	}
	for group := update.sequenceClaims; group != nil; group = group.collapseNext {
		flags := group.completionFlags.Load()
		if flags&(groupCompletionDequeued|groupCompletionAbandoned) != 0 || !group.awaitingCollapsePromotionWithLock() {
			continue
		}
		for _, packet := range group.packets {
			path := packet.ipPath
			if !path.Syn && !path.Rst {
				continue
			}
			const ownCandidate = groupCompletionAck | groupCompletionTracked | groupCompletionAdmitted | groupCompletionLinked | groupCompletionCandidate
			request := group.synRetry
			if flags&ownCandidate != ownCandidate || flags&(groupCompletionDisposed|groupCompletionDone) != 0 ||
				flags>>16 != race.collapseOrder || race.clientStates[group.completionClient] == nil ||
				len(group.packets) != 1 || path.Protocol != IpProtocolTcp || !path.Syn || path.Ack || path.Rst || path.Fin || path.SequenceNumber != self.sequence ||
				request == nil || request.serial != self.serial || request.serial == 0 || request.update != update ||
				request.raceOrder != 0 && request.raceOrder != race.collapseOrder ||
				group.collapseAdmission.update != update || group.collapseAdmission.epoch != update.sequenceAdmissionEpoch ||
				group.collapseSource == nil || group.collapseSource.ctx.Err() != nil {
				return true
			}
		}
	}
	return false
}

func (self *parsedPacketGroup) validSynRetryWithLock(update *multiClientChannelUpdate) bool {
	return !self.isSynRetry() || self.synRetry.update == update && update.synRetry != nil &&
		update.synRetry.serial == self.synRetry.serial && update.synRetry.validWithLock(update) &&
		(self.synRetry.raceOrder == 0 || unansweredSynRetryRaceWithLock(update, self.synRetry.raceOrder))
}

// Materialization has already published source control. These actual borrowed
// frames still own their bytes in v1 and raw encodings, unlike an old parsed
// descriptor's packet slice after the legacy public caller returned it.
func (self sendGroupDisposition) retainSynRetry(source *SendSequence, frames []*protocol.Frame) {
	group, ok := self.target.(*parsedPacketGroup)
	if !ok || group.synRetry == nil || group.collapseAdmission.update == nil ||
		self.end-self.start != len(frames) || group.completionFlags.Load()&groupCompletionAbandoned != 0 {
		return
	}
	update := group.collapseAdmission.update
	defer update.releaseSynRetryMemory()
	update.stateLock.Lock()
	defer update.stateLock.Unlock()
	if update.IsDone() || update.receivedInbound.Load() || update.affinitySources.closed ||
		group.completionFlags.Load()&groupCompletionAbandoned != 0 || !group.validSynRetryWithLock(update) {
		return
	}
	for _, frame := range frames {
		if frame == nil || frame.MessageType != protocol.MessageType_IpIpPacketToProvider {
			continue
		}
		packet, err := ipPacketToProviderBytes(frame)
		if err != nil {
			continue
		}
		path, err := ParseIpPath(packet)
		if err != nil || path.Protocol != IpProtocolTcp {
			continue
		}
		if path.Rst || path.Fin {
			update.clearSynRetryWithLock()
			continue
		}
		if !path.Syn || path.Ack {
			continue
		}
		if retained := update.synRetry; retained != nil && retained.sequence == path.SequenceNumber && bytes.Equal(retained.packet, packet) {
			continue
		}
		// A newly admitted different SYN cannot borrow the preceding packet,
		// even when this bounded extra reservation cannot be obtained.
		update.clearSynRetryWithLock()
		if update.synRetrySerial == ^uint64(0) {
			continue
		}
		metadata := ByteCount(unsafe.Sizeof(tcpSynRetry{})) + ByteCount(len(group.synRetry.pin.appId))
		metadata = (metadata + 63) / 64 * 64
		credit, reserved := source.reserveRetainedControlMemory(addReceiveQueueByteCount(retainedMessageCapacity(ByteCount(len(packet))), metadata))
		if !reserved {
			continue
		}
		copyPacket := MessagePoolCopy(packet)
		update.synRetrySerial++
		owner := &tcpSynRetry{
			update: update, packet: copyPacket, credit: credit,
			source: group.synRetry.source, provideMode: group.synRetry.provideMode,
			pin: group.synRetry.pin, sequence: path.SequenceNumber,
			serial: update.synRetrySerial, references: 1,
		}
		owner.readiness = newSynRetryReadiness(update.sequenceParent, update, source, owner.serial, owner.sequence, synRetryReadyCapacity(update.sequenceParent, update))
		update.synRetry = owner
	}
}

func (self *multiClientChannelUpdate) releaseSynRetryReferenceWithLock(owner *tcpSynRetry) {
	if owner.references == 0 {
		panic("SYN retry owner lost its reference")
	}
	owner.references--
	if owner.references == 0 {
		owner.released = self.synRetryReleased.Load()
		self.synRetryReleased.Store(owner)
	}
}

func (self *multiClientChannelUpdate) clearSynRetryWithLock() {
	if owner := self.synRetry; owner != nil {
		if ready := owner.readiness; ready != nil {
			ready.closed.Store(true)
			ready.parent.notifyFlowReaper()
		}
		self.synRetry = nil
		self.releaseSynRetryReferenceWithLock(owner)
	}
}

// Releasing credit can notify other senders, so it never runs under a flow
// or parent lock. The final owner drops every packet/graph reference first.
func (self *multiClientChannelUpdate) releaseSynRetryMemory() {
	if self.synRetryReleased.Load() == nil {
		return
	}
	self.stateLock.Lock()
	owners := self.synRetryReleased.Swap(nil)
	self.stateLock.Unlock()
	for owners != nil {
		owner := owners
		owners, owner.released = owner.released, nil
		packet, credit, ready := owner.packet, owner.credit, owner.readiness
		owner.packet, owner.credit, owner.update, owner.next, owner.readiness = nil, nil, nil, nil, nil
		if ready != nil {
			ready.closed.Store(true)
			ready.detach()
			ready.release()
		}
		MessagePoolReturn(packet)
		credit.Release()
	}
}

// A continuation cannot compete with response proof already owned by the race.
// The caller holds the flow lock; collapseOrder fences race replacement.
func unansweredSynRetryRaceWithLock(update *multiClientChannelUpdate, order uint64) bool {
	race := update.race
	if order == 0 || race == nil || race.collapseOrder != order || race.single ||
		race.packetCount != 0 || race.clientsWithPacketCount != 0 {
		return false
	}
	for _, state := range race.clientStates {
		if len(state.packets) != 0 {
			return false
		}
	}
	return true
}

// A withdrawn candidate's still-running admissions retain slots until return.
// Existing live/pending candidates and fresh proposals share the original width
// and the current configured/degraded bound. Contract hold reads are lock-free.
func (self *RemoteUserNatMultiClient) synRetryRaceSlotsWithLock(update *multiClientChannelUpdate, fresh []*multiClientChannel) int {
	race := update.race
	if race == nil || race.clientLimit <= 0 || !unansweredSynRetryRaceWithLock(update, race.collapseOrder) {
		return 0
	}
	retiredPending := race.pendingSendCount
	degraded := self.backendDegraded()
	now := time.Now()
	for client, state := range race.clientStates {
		retiredPending -= state.pendingSendCount
		degraded = degraded || contractRateLimitHeldChannel(client, now)
	}
	for _, client := range fresh {
		degraded = degraded || contractRateLimitHeldChannel(client, now)
	}
	limit := multiRaceClientCount(race.clientLimit, self.settings.MultiRaceClientCount, self.settings.MultiRaceDegradedClientCount, degraded)
	return max(0, limit-len(race.clientStates)-max(0, retiredPending))
}

// A normal empty-race retry keeps its old policy. A partial continuation must
// still name its exact unanswered race and a genuinely vacated admission slot.
func (self *RemoteUserNatMultiClient) synRetryDispatchEligibleWithLock(update *multiClientChannelUpdate, order uint64) bool {
	if order == 0 {
		return update.race == nil
	}
	return unansweredSynRetryRaceWithLock(update, order) && self.synRetryRaceSlotsWithLock(update, nil) > 0
}

// Filtering precedes the first-window return: live quality owners are not fresh
// candidates. A continuation can visit the next permitted healthy window only
// after exhausting untried offers in the preferred field. Call under flow lock.
func (self *RemoteUserNatMultiClient) freshSynRetryCandidatesWithLock(group *parsedPacketGroup, update *multiClientChannelUpdate, candidates []*multiClientChannel) []*multiClientChannel {
	if !group.isSynRetry() || group.synRetry.raceOrder == 0 {
		return candidates
	}
	if !group.validSynRetryWithLock(update) || update.client.Load() != nil {
		return nil
	}
	available := self.synRetryRaceSlotsWithLock(update, candidates)
	if available == 0 {
		return nil
	}
	fresh := make([]*multiClientChannel, 0, min(available, len(candidates)))
	for _, client := range candidates {
		if update.race.clientStates[client] != nil {
			continue
		}
		failed := false
		for _, prior := range update.dialFailedClients {
			if prior == client {
				failed = true
				break
			}
		}
		if failed {
			continue
		}
		fresh = append(fresh, client)
		if len(fresh) == available {
			break
		}
	}
	return fresh
}

// A first standing explicit failure earns one coalesced continuation. If an
// admission still owns the vacated slot, its actual return retries this enqueue.
func (self *RemoteUserNatMultiClient) enqueueSynRetry(update *multiClientChannelUpdate) {
	self.enqueueSynRetryForRace(update, 0)
}

// A nonzero completion token may consume only its race's existing request.
// It never grants a retry merely because an unrelated admission completed.
func (self *RemoteUserNatMultiClient) enqueueSynRetryForRace(update *multiClientChannelUpdate, completedOrder uint64) {
	if update == nil || self.flowReaperDone == nil {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.ctx.Err() != nil || self.affinityUpdateWithLock(update.ipPath) != update || self.affinityTransitions[update] != nil {
		return
	}
	update.stateLock.Lock()
	defer update.stateLock.Unlock()
	owner := update.synRetry
	if owner == nil || !owner.validWithLock(update) || update.client.Load() != nil {
		return
	}
	if completedOrder != 0 && (update.race == nil || update.race.collapseOrder != completedOrder || !update.race.synRetryPending) {
		return
	}
	order := uint64(0)
	if race := update.race; race != nil {
		if !unansweredSynRetryRaceWithLock(update, race.collapseOrder) {
			return
		}
		race.synRetryPending = true
		if self.synRetryRaceSlotsWithLock(update, nil) == 0 {
			return
		}
		order = race.collapseOrder
		race.synRetryPending = false
	}
	self.queueSynRetryWithLock(owner, order)
}

// Parent and flow locks protect one identity-specific queue publication.
func (self *RemoteUserNatMultiClient) queueSynRetryWithLock(owner *tcpSynRetry, order uint64) {
	owner.raceOrder = order
	if owner.queued {
		// Another standing failure may empty the race while its partial work
		// is still queued. Retarget only work not yet captured by a dispatcher.
		return
	}
	owner.references++
	owner.queued, owner.next = true, nil
	if self.synRetryTail == nil {
		self.synRetryHead = owner
	} else {
		self.synRetryTail.next = owner
	}
	self.synRetryTail = owner
	self.notifyFlowReaper()
}

func (self *RemoteUserNatMultiClient) takeSynRetry() *tcpSynRetry {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	owner := self.synRetryHead
	if owner != nil {
		self.synRetryHead, owner.next = owner.next, nil
		if self.synRetryHead == nil {
			self.synRetryTail = nil
		}
	}
	return owner
}

func (self *RemoteUserNatMultiClient) finishSynRetry(owner *tcpSynRetry) {
	update := owner.update
	update.stateLock.Lock()
	update.releaseSynRetryReferenceWithLock(owner)
	update.stateLock.Unlock()
	update.releaseSynRetryMemory()
}

// Work present at entry is a finite, byte-charged batch. Newly generated
// failures wake the next pass instead of extending this one indefinitely.
func (self *RemoteUserNatMultiClient) drainSynRetries() {
	self.stateLock.Lock()
	last := self.synRetryTail
	self.stateLock.Unlock()
	for last != nil && self.ctx.Err() == nil {
		owner := self.takeSynRetry()
		if owner == nil {
			return
		}
		func() {
			defer self.finishSynRetry(owner)
			update := owner.update
			update.stateLock.Lock()
			order := owner.raceOrder
			owner.queued = false
			eligible := owner.validWithLock(update) && update.client.Load() == nil && self.synRetryDispatchEligibleWithLock(update, order)
			update.stateLock.Unlock()
			if !eligible {
				return
			}
			if hook := self.beforeSynRetryDispatchForTest; hook != nil {
				hook(update)
			}
			self.dispatchSynRetry(owner, order)
		}()
		if owner == last {
			return
		}
	}
}

func (self *RemoteUserNatMultiClient) discardSynRetries() {
	for owner := self.takeSynRetry(); owner != nil; owner = self.takeSynRetry() {
		owner.update.stateLock.Lock()
		owner.queued = false
		owner.update.stateLock.Unlock()
		self.finishSynRetry(owner)
	}
}

func (self *RemoteUserNatMultiClient) dispatchSynRetry(owner *tcpSynRetry, order uint64) {
	update := owner.update
	if ready := owner.readiness; ready != nil {
		ready.detach()
	}
	update.stateLock.Lock()
	if !owner.validWithLock(update) || update.client.Load() != nil || !self.synRetryDispatchEligibleWithLock(update, order) || self.ctx.Err() != nil {
		update.stateLock.Unlock()
		return
	}
	owner.readinessWidth, owner.readinessOverflow = 1, false
	owner.readinessEpoch, owner.readinessSourceOrder = update.sequenceAdmissionEpoch, update.sequenceSourceOrder
	if ready := owner.readiness; ready != nil {
		ready.attemptEpoch, ready.attemptSourceOrder = owner.readinessEpoch, owner.readinessSourceOrder
	}
	packet := MessagePoolShareReadOnly(owner.packet)
	update.stateLock.Unlock()
	path, payload, err := ParseIpPathWithPayload(packet)
	if err != nil {
		MessagePoolReturn(packet)
		return
	}
	request := &tcpSynRetryRequest{source: owner.source, provideMode: owner.provideMode, pin: owner.pin, update: update, serial: owner.serial, raceOrder: order}
	group := &parsedPacketGroup{
		ipPath: path, pin: owner.pin, byteCount: ByteCount(len(packet)), synRetry: request,
		packets: []parsedPacket{{packet: packet, ipPath: path, payload: payload, pin: owner.pin}},
	}
	if !self.sendParsedPacketGroupAttempt(owner.source, owner.provideMode, group, 0, true, nil) {
		MessagePoolReturn(packet)
	}
	// All synchronous candidate calls and refusal cleanup have returned.
	// The dispatch reference keeps owner.readiness immutable through this arm.
	self.armSynRetryReadiness(owner)
}

// A retry belongs to this existing canonical flow. It must not create a new
// tuple generation, renew affinity, or borrow a newly established selection.
func (self *RemoteUserNatMultiClient) sendOwnedSynRetryPath(group *parsedPacketGroup, timeout time.Duration, callback func(*multiClientChannelUpdate, *multiClientChannel, time.Duration)) {
	update := group.synRetry.update
	self.stateLock.Lock()
	current := self.affinityUpdateWithLock(group.ipPath)
	valid := self.ctx.Err() == nil && current == update && self.affinityTransitions[update] == nil
	self.stateLock.Unlock()
	if !valid {
		return
	}
	callback(update, update.client.Load(), timeout)
	if client := update.client.Load(); client != nil {
		self.bindClientFlow(update, client)
	}
}
