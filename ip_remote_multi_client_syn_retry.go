// An explicitly failed unanswered SYN can retry from bounded source-owned
// bytes, without waiting for another native RTO. The existing parent reaper
// dispatches work; no packet, timer or goroutine is created by a failure callback.
package connect

import (
	"bytes"
	"sync/atomic"
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
	// An ordinary caller keeps serial zero; this attests a continuous native retry.
	nativeOwnerSerial uint64
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
	refusedRemoteIds                     []Id
	remoteRefusalOverflow                bool
	readinessWidth                       int
	readinessEpoch, readinessSourceOrder uint64
	readinessOverflow                    bool
	source                               TransferPath
	provideMode                          protocol.ProvideMode
	pin                                  flowPin
	sequence                             uint32
	serial                               uint64
	// Continuous metadata refreshes share this bounded provenance token.
	retryLineage uint64
	references   uint32
	queued       bool
	// Weak index links are detached before final credit release. Cache or
	// queued/in-flight references own the struct throughout membership.
	publicationPrevious, publicationNext *tcpSynRetry
	publicationIndexed                   bool
	publicationRetired                   atomic.Bool
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
	request := &tcpSynRetryRequest{source: source, provideMode: mode, pin: self.pin}
	// Compare only while the caller still owns these bytes. Queued descriptors
	// retain the scalar attestation, never authority from their borrowed packet.
	if len(self.packets) == 1 && !path.Rst && !path.Fin {
		update.stateLock.Lock()
		if owner := update.synRetry; owner != nil && owner.serial != 0 &&
			owner.sequence == path.SequenceNumber && owner.source == source &&
			owner.provideMode == mode && owner.pin == self.pin && owner.validWithLock(update) &&
			0 < len(owner.packet) && (bytes.Equal(owner.packet, self.packets[0].packet) || sameSynRetryMetadata(owner.packet, self.packets[0].packet)) {
			request.update = update
			request.nativeOwnerSerial = owner.serial
		}
		update.stateLock.Unlock()
	}
	self.synRetry = request
}

// Exact live cache generation, independent of temporary same-SYN claim readiness.
func (self *tcpSynRetry) validGenerationWithLock(update *multiClientChannelUpdate) bool {
	return self != nil && self.update == update && update.synRetry == self &&
		!update.IsDone() && !update.sourceReset.Load() && !update.receivedInbound.Load() &&
		!update.affinitySources.closed && !update.pendingCollapseResetWithLock() &&
		(!update.synGenerationSeen || update.synGenerationNumber == self.sequence)
}

// Reuse and dispatch retain every existing pending-control fence.
func (self *tcpSynRetry) validWithLock(update *multiClientChannelUpdate) bool {
	return self.validGenerationWithLock(update) && !self.pendingConflictingControlWithLock(update)
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
			sameOwner := request != nil && request.update == update &&
				(request.serial != 0 && request.serial == self.serial ||
					request.serial == 0 && request.nativeOwnerSerial != 0 && request.nativeOwnerSerial == self.serial)
			if flags&ownCandidate != ownCandidate || flags&(groupCompletionDisposed|groupCompletionDone) != 0 ||
				flags>>16 != race.collapseOrder || race.clientStates[group.completionClient] == nil ||
				len(group.packets) != 1 || path.Protocol != IpProtocolTcp || !path.Syn || path.Ack || path.Rst || path.Fin || path.SequenceNumber != self.sequence ||
				!sameOwner ||
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

// Only actually retained source controls advance this receipt. FIFO dequeue
// order, then in-group position, distinguish admitted cohorts without ordering
// TCP ISNs. A later refused offer cannot revoke an older materialized SYN.
func (self *parsedPacketGroup) noteSynRetryControlMaterializedWithLock(index int) {
	path := self.packets[index].ipPath
	if path.Protocol != IpProtocolTcp || !(path.Syn && !path.Ack || path.Rst || path.Fin) ||
		self.completionFlags.Load()&groupCompletionAbandoned != 0 {
		return
	}
	order := self.collapseOrder()
	if order == 0 {
		return
	}
	update := self.collapseAdmission.update
	// A still-live older Transfer lane may dequeue after a newer lane. The
	// existing selected/candidate source policy already ignores that control.
	sourceId := self.collapseSourceIdWithLock()
	if sourceId == (Id{}) || update.sequenceClient == self.completionClient && sourceId.LessThan(update.sequenceSourceId) {
		return
	}
	if race := update.race; race != nil {
		if state := race.clientStates[self.completionClient]; state != nil && sourceId.LessThan(state.collapseControl.sourceId) {
			return
		}
	}
	if update.synRetryMaterializedOrder < order || update.synRetryMaterializedOrder == order && update.synRetryMaterializedIndex < index {
		update.synRetryMaterializedOrder, update.synRetryMaterializedIndex = order, index
	}
}

// The caller holds the same flow lock used by successful disposition. The
// receipt survives optional-cache allocation failure and final credit release.
func (self *parsedPacketGroup) currentSynRetryControlMaterializationWithLock(index int) bool {
	update := self.collapseAdmission.update
	order := self.collapseOrder()
	return order != 0 && update.synRetryMaterializedOrder == order && update.synRetryMaterializedIndex == index
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
	materializedSyn := false
	defer func() {
		if materializedSyn {
			update.reassertServiceRefreshAfterMaterialization()
		}
	}()
	defer update.releaseSynRetryMemory()
	defer update.indexSynRetryPublication()
	update.stateLock.Lock()
	defer update.stateLock.Unlock()
	if update.IsDone() || update.receivedInbound.Load() || update.affinitySources.closed ||
		group.completionFlags.Load()&groupCompletionAbandoned != 0 || !group.validSynRetryWithLock(update) {
		return
	}
	for frameIndex, frame := range frames {
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
		if (path.Syn && !path.Ack || path.Rst || path.Fin) && !group.currentSynRetryControlMaterializationWithLock(self.start+frameIndex) {
			continue
		}
		if path.Rst || path.Fin {
			update.clearSynRetryWithLock()
			continue
		}
		if !path.Syn || path.Ack {
			continue
		}
		// An original prepared before cache publication can fence its own
		// paid refusal until this materialization, even when bytes are equal.
		materializedSyn = true
		if retained := update.synRetry; retained != nil && retained.sequence == path.SequenceNumber && bytes.Equal(retained.packet, packet) {
			continue
		}
		previous := update.synRetry
		continuous := previous != nil && previous.validGenerationWithLock(update) &&
			previous.sequence == path.SequenceNumber && previous.source == group.synRetry.source &&
			previous.provideMode == group.synRetry.provideMode && previous.pin == group.synRetry.pin &&
			sameSynRetryMetadata(previous.packet, packet)
		// A continuous retry may keep its older paid wire image if optional
		// replacement credit is unavailable. A different attempt cannot borrow it.
		if !continuous {
			update.clearSynRetryWithLock()
			update.serviceRefreshRequested = false
			update.serviceRefreshLineage.Store(0)
		}
		if update.synRetrySerial == ^uint64(0) {
			continue
		}
		remoteCapacity := synRetryRemoteTailCapacity(update.sequenceParent, update)
		if remoteCapacity <= 0 {
			continue
		}
		metadata := ByteCount(unsafe.Sizeof(tcpSynRetry{})) + ByteCount(len(group.synRetry.pin.appId))
		metadata = (metadata + 63) / 64 * 64
		metadata = addReceiveQueueByteCount(metadata, retainedMessageCapacity(ByteCount(remoteCapacity)*ByteCount(unsafe.Sizeof(Id{}))))
		noBudget := protocolClassPureTcpControl(packet)
		credit, reserved := source.reserveRetainedControlMemory(addReceiveQueueByteCount(retainedMessageCapacity(ByteCount(len(packet))), metadata), noBudget)
		if !reserved {
			continue
		}
		copyPacket := MessagePoolCopy(packet)
		update.synRetrySerial++
		owner := &tcpSynRetry{
			update: update, packet: copyPacket, credit: credit,
			source: group.synRetry.source, provideMode: group.synRetry.provideMode,
			pin: group.synRetry.pin, sequence: path.SequenceNumber,
			serial: update.synRetrySerial, retryLineage: update.synRetrySerial, references: 1,
			refusedRemoteIds: make([]Id, 0, remoteCapacity),
		}
		if continuous {
			owner.retryLineage = previous.retryLineage
			count := min(len(previous.refusedRemoteIds), cap(owner.refusedRemoteIds))
			owner.refusedRemoteIds = append(owner.refusedRemoteIds, previous.refusedRemoteIds[:count]...)
			owner.remoteRefusalOverflow = previous.remoteRefusalOverflow || count != len(previous.refusedRemoteIds)
			update.clearSynRetryWithLock()
		}
		owner.readiness = newSynRetryReadiness(update.sequenceParent, update, source, owner.serial, owner.sequence, synRetryReadyCapacity(update.sequenceParent, update), noBudget)
		update.synRetry = owner
	}
}

func (self *multiClientChannelUpdate) releaseSynRetryReferenceWithLock(owner *tcpSynRetry) {
	if owner.references == 0 {
		panic("SYN retry owner lost its reference")
	}
	owner.references--
	if owner.references == 0 {
		owner.publicationRetired.Store(true)
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
	parent := self.sequenceParentWithLock()
	owners := self.synRetryReleased.Swap(nil)
	self.stateLock.Unlock()
	for owners != nil {
		owner := owners
		owners, owner.released = owner.released, nil
		packet, credit, ready := owner.packet, owner.credit, owner.readiness
		if parent != nil {
			parent.removeSynPublicationOwner(owner)
		}
		owner.packet, owner.credit, owner.update, owner.next, owner.readiness = nil, nil, nil, nil, nil
		owner.refusedRemoteIds, owner.remoteRefusalOverflow = nil, false
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

// A native admission may occupy this queued continuation's slot before either
// dispatch check. Its eventual return must still see the same race's request.
func (self *tcpSynRetry) preserveRaceRequestWithLock(order uint64) {
	update := self.update
	if self.validWithLock(update) && update.client.Load() == nil && unansweredSynRetryRaceWithLock(update, order) {
		update.race.synRetryPending = true
	}
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
		if self.synRetryRaceSlotsWithLock(update, nil) == 0 {
			update.synRetry.preserveRaceRequestWithLock(group.synRetry.raceOrder)
		}
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
			if !eligible && self.ctx.Err() == nil {
				owner.preserveRaceRequestWithLock(order)
			}
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
		if self.ctx.Err() == nil {
			owner.preserveRaceRequestWithLock(order)
		}
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
