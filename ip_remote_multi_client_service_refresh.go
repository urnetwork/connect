// One exhausted TCP service may rotate an idle slot in a full discovery
// window. It borrows the already-paid SYN owner and ordinary expansion worker.
package connect

import (
	"slices"
	"time"
)

// One static terminal token cannot be confused with any paid source owner.
var serviceRefreshRetiringOwner tcpSynRetry

// A value observation holds no owner reference. Window ownership protects the
// captured update while locked; later reads use its independent context/stamp.
type serviceRefreshSnapshot struct {
	owner  *tcpSynRetry
	update *multiClientChannelUpdate
	victim *multiClientChannel
	stamp  uint64
}

// A pending original may have blocked the refusal callback's one request.
// Reconsider only after actual SYN materialization, publication indexing and
// memory release. The ordinary claim rechecks current paid lineage and every
// refusal, lifetime, capacity and reservation guard; no authority is retained.
func (self *multiClientChannelUpdate) reassertServiceRefreshAfterMaterialization() {
	if parent := self.sequenceParentSnapshot(); parent != nil {
		parent.requestServiceRefresh(self, self.ipPath)
	}
}

// No parent/window lock nesting: gather membership first, then qualify the
// exact source and claim one idle victim under the parent's routing lock.
func (self *RemoteUserNatMultiClient) requestServiceRefresh(update *multiClientChannelUpdate, path *IpPath) {
	if update == nil || path == nil || path.Protocol != IpProtocolTcp || self.ctx.Err() != nil {
		return
	}
	update.stateLock.Lock()
	possible := update.client.Load() == nil && !update.serviceRefreshRequested && !update.pinned &&
		update.synRetryHasRemoteRefusalWithLock() && !update.synRetry.remoteRefusalOverflow &&
		discoveryDemandAttemptLive(update, update.discoveryAttemptStamp.Load())
	update.stateLock.Unlock()
	if !possible {
		return
	}
	if _, fixed := self.generator.FixedDestinationSize(); fixed {
		return
	}
	if _, _, fixed := self.config.Load().performanceProfile.FixedWindow(); fixed {
		return
	}
	self.stateLock.Lock()
	appId, pinned := update.pinAppId, update.pinned
	self.stateLock.Unlock()
	if pinned {
		return
	}
	windowTypes := self.selectWindowTypes(&parsedPacket{ipPath: path}, appId)
	// the limited window's exits race tcp flows beside every selected
	// window's (server FP2-PROTOCOL.md §7.5), so they are offered too, and
	// its slots rotate last
	if self.windows[WindowTypeLimited] != nil {
		windowTypes = append(windowTypes, WindowTypeLimited)
	}
	members := make(map[WindowType][]*multiClientChannel, len(windowTypes))
	var offered []*multiClientChannel
	for _, windowType := range windowTypes {
		if window := self.windows[windowType]; window != nil {
			members[windowType] = window.unorderedClients()
			for _, client := range members[windowType] {
				if !client.IsDone() && !client.isWarning() && client.supportsIpPath(path) {
					offered = append(offered, client)
				}
			}
		}
	}
	if len(offered) == 0 {
		return
	}
	update.stateLock.Lock()
	qualified := update.client.Load() == nil && !update.serviceRefreshRequested && !update.pinned && update.synRetryHasRemoteRefusalWithLock() &&
		!update.synRetry.remoteRefusalOverflow && discoveryDemandAttemptLive(update, update.discoveryAttemptStamp.Load())
	update.stateLock.Unlock()
	if !qualified {
		return
	}
	for _, windowType := range windowTypes {
		window := self.windows[windowType]
		if window == nil || !window.serviceRefreshCapacity(members[windowType]) {
			continue
		}
		for _, victim := range members[windowType] {
			if victim.IsDone() || victim.isWarning() || !victim.supportsIpPath(path) {
				continue
			}
			owner, stamp := self.claimServiceRefreshOwner(update, path, victim, offered)
			if owner == nil {
				continue
			}
			if window.installServiceRefresh(owner, victim, stamp, members[windowType]) {
				return
			}
			victim.releaseServiceRefreshReservation(owner)
			update.stateLock.Lock()
			if update.currentSynRetryLineageWithLock(owner) != nil && update.discoveryAttemptStamp.Load() == stamp {
				update.serviceRefreshRequested = false
			}
			update.releaseSynRetryReferenceWithLock(owner)
			update.stateLock.Unlock()
			update.releaseSynRetryMemory()
		}
	}
}

// Only the exact paid reservation may reassert its bounded acquisition before
// ordinary exhausted-field observation. Metadata successors keep that lineage.
func (self *multiClientWindow) observeServiceRefreshAttempt(update *multiClientChannelUpdate, source *IpPath) (discoveryAttemptObservation, bool) {
	snapshot := self.serviceRefreshSnapshot()
	if snapshot.owner == nil || snapshot.update != update {
		return discoveryAttemptObservation{}, false
	}
	update.stateLock.Lock()
	defer update.stateLock.Unlock()
	if update.currentSynRetryLineageWithLock(snapshot.owner) == nil || !update.synRetryHasRemoteRefusalWithLock() || !update.synRetryRemoteTailRefusedWithLock(snapshot.victim) ||
		!discoveryDemandAttemptLive(update, snapshot.stamp) ||
		(source != nil && (source.Protocol != IpProtocolTcp || !source.Syn || source.Ack || source.SequenceNumber != update.dialFailedSyn)) {
		return discoveryAttemptObservation{}, false
	}
	return discoveryAttemptObservation{stamp: snapshot.stamp, serviceOwner: snapshot.owner}, true
}

// The fence precedes the scan. Fresh race registration/adoption observes it
// under each flow lock; earlier registrations make the victim busy instead.
func (self *RemoteUserNatMultiClient) claimServiceRefreshOwner(update *multiClientChannelUpdate, path *IpPath, victim *multiClientChannel, offered []*multiClientChannel) (*tcpSynRetry, uint64) {
	update.stateLock.Lock()
	owner := update.synRetry
	if update.serviceRefreshRequested || !update.synRetryHasRemoteRefusalWithLock() || owner.remoteRefusalOverflow {
		update.stateLock.Unlock()
		return nil, 0
	}
	owner.references++
	update.stateLock.Unlock()
	claimed, reservationAcquired := false, false
	defer func() {
		if !claimed {
			if reservationAcquired {
				victim.releaseServiceRefreshReservation(owner)
			}
			update.stateLock.Lock()
			update.releaseSynRetryReferenceWithLock(owner)
			update.stateLock.Unlock()
			update.releaseSynRetryMemory()
		}
	}()
	if !victim.serviceRefreshReservation.CompareAndSwap(nil, owner) {
		return nil, 0
	}
	reservationAcquired = true
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.ctx.Err() != nil || self.affinityUpdateWithLock(path) != update || self.affinityTransitions[update] != nil {
		return nil, 0
	}
	victim.stateLock.Lock()
	busy := len(self.clientUpdates[victim]) != 0 || victim.serviceRefreshRaceReferences.Load() != 0
	victim.stateLock.Unlock()
	if busy {
		return nil, 0
	}
	update.stateLock.Lock()
	defer update.stateLock.Unlock()
	stamp := update.discoveryAttemptStamp.Load()
	current := update.currentSynRetryLineageWithLock(owner)
	if update.client.Load() != nil || update.serviceRefreshRequested || !discoveryDemandAttemptLive(update, stamp) ||
		current == nil || !update.synRetryHasRemoteRefusalWithLock() || current.remoteRefusalOverflow || victim.IsDone() ||
		victim.serviceRefreshReservation.Load() != owner {
		return nil, 0
	}
	// The retirement authority is this idle victim's authenticated refusal.
	// Other candidates may still answer; they retain their race references.
	// Requiring every peer to refuse delays replacement of an unusable idle
	// slot behind unrelated silent peers and their native retry schedule.
	if !update.synRetryRemoteTailRefusedWithLock(victim) {
		return nil, 0
	}
	update.serviceRefreshRequested = true
	update.serviceRefreshLineage.Store(owner.retryLineage)
	claimed = true
	return owner, stamp
}

// Refresh cannot borrow a retry deferral, fixed peer, non-strict ceiling, or
// another refresh's slot. An active filled ordinary pass may finish its surplus.
func (self *multiClientWindow) serviceRefreshCapacity(members []*multiClientChannel) bool {
	if !self.strictWindowHandoverEnabled() || !self.cooperativeDiscoveryDemand() || self.backendDegraded() {
		return false
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	size := self.windowSizeWithLock()
	return self.serviceRefreshOwner == nil && size.WindowSizeHardMax > 0 &&
		len(self.clients) == size.WindowSizeHardMax && len(members) == len(self.clients) &&
		!time.Now().Before(self.discoveryRetry.notBefore)
}

// The window's one extra reference keeps the fixed paid history and packet
// alive through its bounded acquisition. No extra map, timer or worker is owned.
func (self *multiClientWindow) installServiceRefresh(owner *tcpSynRetry, victim *multiClientChannel, stamp uint64, members []*multiClientChannel) bool {
	self.stateLock.Lock()
	if self.serviceRefreshOwner != nil || self.ctx.Err() != nil || len(members) != len(self.clients) ||
		time.Now().Before(self.discoveryRetry.notBefore) || victim.serviceRefreshReservation.Load() != owner {
		self.stateLock.Unlock()
		return false
	}
	for _, client := range members {
		if self.clients[client.ClientId()] != client {
			self.stateLock.Unlock()
			return false
		}
	}
	self.serviceRefreshOwner, self.serviceRefreshVictim, self.serviceRefreshStamp = owner, victim, stamp
	self.serviceRefreshAttempted = false
	self.stateLock.Unlock()
	self.resizeMonitor.NotifyAll()
	return true
}

// Must be called with the window state lock; it reads only independent atomics.
func (self *multiClientWindow) serviceRefreshLiveWithLock() bool {
	owner, victim := self.serviceRefreshOwner, self.serviceRefreshVictim
	if owner == nil || victim == nil || victim.serviceRefreshReservation.Load() != owner {
		return false
	}
	update := owner.update
	return update != nil && self.ctx.Err() == nil && discoveryDemandAttemptLive(update, self.serviceRefreshStamp)
}

// The owning resize pass holds the window reference until this snapshot ends.
func (self *multiClientWindow) serviceRefreshSnapshot() serviceRefreshSnapshot {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !self.serviceRefreshLiveWithLock() {
		return serviceRefreshSnapshot{}
	}
	return serviceRefreshSnapshot{owner: self.serviceRefreshOwner, update: self.serviceRefreshOwner.update,
		victim: self.serviceRefreshVictim, stamp: self.serviceRefreshStamp}
}

// A prior empty snapshot cannot authorize clearing a new live reservation.
// Recheck validity under the same window lock that detaches the current owner.
func (self *multiClientWindow) pruneInvalidServiceRefresh() {
	self.finishServiceRefreshMatching(nil, true)
}

// An exact pass owner may finish itself; nil remains force cleanup for teardown.
func (self *multiClientWindow) finishServiceRefresh(expected *tcpSynRetry) {
	self.finishServiceRefreshMatching(expected, false)
}

// Detachment is atomic with its predicate. Returning the source reference and
// its paid memory remains outside the window lock for both cleanup paths.
func (self *multiClientWindow) finishServiceRefreshMatching(expected *tcpSynRetry, invalidOnly bool) {
	self.stateLock.Lock()
	owner, victim := self.serviceRefreshOwner, self.serviceRefreshVictim
	if owner == nil || expected != nil && owner != expected || invalidOnly && self.serviceRefreshLiveWithLock() {
		self.stateLock.Unlock()
		return
	}
	update, attempted, stamp := owner.update, self.serviceRefreshAttempted, self.serviceRefreshStamp
	self.serviceRefreshOwner, self.serviceRefreshVictim, self.serviceRefreshStamp = nil, nil, 0
	self.serviceRefreshAttempted = false
	self.stateLock.Unlock()
	victim.releaseServiceRefreshReservation(owner)
	update.stateLock.Lock()
	if !attempted && update.currentSynRetryLineageWithLock(owner) != nil && update.discoveryAttemptStamp.Load() == stamp {
		update.serviceRefreshRequested = false
	}
	update.releaseSynRetryReferenceWithLock(owner)
	update.stateLock.Unlock()
	update.releaseSynRetryMemory()
	self.resizeMonitor.NotifyAll()
	self.generatorMonitor.NotifyAll()
}

// A read uses the paid per-SYN tail ledger while its window owns that source.
// The returned map is ordinary bounded request scratch, not persistent history.
func (self *multiClientWindow) addServiceRefreshExclusions(excluded map[MultiHopId]bool) {
	snapshot := self.serviceRefreshSnapshot()
	if snapshot.owner == nil {
		return
	}
	snapshot.update.stateLock.Lock()
	defer snapshot.update.stateLock.Unlock()
	current := snapshot.update.currentSynRetryLineageWithLock(snapshot.owner)
	if current == nil || !discoveryDemandAttemptLive(snapshot.update, snapshot.stamp) {
		return
	}
	for _, id := range current.refusedRemoteIds {
		excluded[RequireMultiHopId(id)] = true
	}
}

// Source cancellation, reset, useful response, new SYN or rebind preemption
// withdraws this optional pass without changing an admitted channel's lifetime.
func (self *multiClientWindow) serviceRefreshLive(owner *tcpSynRetry) bool {
	if owner == nil {
		return true
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.serviceRefreshOwner == owner && self.serviceRefreshLiveWithLock()
}

// Caller exclusions are not authority to admit a newly minted local alias.
// The original paid generation still decides whether its remote tail refused.
func (self *multiClientWindow) serviceRefreshCandidateAllowed(owner *tcpSynRetry, client *multiClientChannel) bool {
	if owner == nil {
		return true
	}
	snapshot := self.serviceRefreshSnapshot()
	if snapshot.owner != owner {
		return false
	}
	snapshot.update.stateLock.Lock()
	defer snapshot.update.stateLock.Unlock()
	return snapshot.update.currentSynRetryLineageWithLock(owner) != nil && discoveryDemandAttemptLive(snapshot.update, snapshot.stamp) &&
		!snapshot.update.synRetryRemoteTailRefusedWithLock(client)
}

// Failed service attempts retain expansion backoff even when their candidate
// passed a ping. Only useful response proof can clear that retry authority.
func (self *RemoteUserNatMultiClient) serviceRefreshConnected(path *IpPath) {
	update := self.receiveUpdate(path)
	if update == nil {
		return
	}
	update.stateLock.Lock()
	useful := update.serviceRefreshRequested && update.receivedInbound.Load()
	lineage := update.serviceRefreshLineage.Load()
	if useful {
		update.serviceRefreshRequested = false
	}
	update.stateLock.Unlock()
	if useful {
		for _, window := range self.windows {
			window.clearDiscoveryExpandRetryForService(update, lineage)
			if window.resizeMonitor != nil && window.serviceRefreshAnswerObserved(update) {
				window.resizeMonitor.NotifyAll()
			}
		}
	}
}

// Only new race registrations are fenced; an earlier live registration makes
// reservation fail and retains its normal response and ownership semantics.
func serviceRefreshRaceCandidates(candidates []*multiClientChannel) []*multiClientChannel {
	return slices.DeleteFunc(candidates, func(client *multiClientChannel) bool { return client.serviceRefreshReservation.Load() != nil })
}

// Registration and idle reservation synchronize on this channel lock. The
// reference remains held through winner publication into parent bookkeeping.
func (self *multiClientChannel) tryServiceRefreshRaceReference() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.serviceRefreshReservation.Load() != nil || self.ctx != nil && self.IsDone() {
		return false
	}
	self.serviceRefreshRaceReferences.Add(1)
	return true
}

// Each real race state owns at most one reference. Bare fixture states do not.
func (self *multiClientChannelRaceClientState) releaseServiceRefreshRaceReference(client *multiClientChannel) {
	if self.serviceRefreshCounted {
		self.serviceRefreshCounted = false
		if client.serviceRefreshRaceReferences.Add(-1) < 0 {
			panic("negative service refresh candidate references")
		}
	}
}

// The caller holds the flow lock, or the parent lock during initial inheritance
// before any leaf owner can access this new update. The counted channel claim
// follows its selected flow without a decrement/increment gap.
func (self *multiClientChannelUpdate) transferServiceRefreshBindingWithLock(client *multiClientChannel) {
	previous := self.pendingServiceRefreshBinding
	self.pendingServiceRefreshBinding = client
	if previous != nil && previous.serviceRefreshRaceReferences.Add(-1) < 0 {
		panic("negative service refresh binding references")
	}
}

// The caller holds the flow lock. Cancellation makes any unindexed winner
// irrelevant; otherwise bindClientFlow invokes this after map publication.
func (self *multiClientChannelUpdate) releaseServiceRefreshBindingWithLock() {
	if client := self.pendingServiceRefreshBinding; client != nil {
		self.pendingServiceRefreshBinding = nil
		if client.serviceRefreshRaceReferences.Add(-1) < 0 {
			panic("negative service refresh binding references")
		}
	}
}

// Parent bookkeeping has published this actual current owner. It now protects
// the flow even when a rebind replaced its earlier pending client. A stale bind
// cannot release the unpublished current winner's claim.
func (self *multiClientChannelUpdate) finishServiceRefreshBinding(client *multiClientChannel) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.client.Load() == client {
		self.releaseServiceRefreshBindingWithLock()
	}
}

// Existing-flow recovery can take precedence over optional service rotation.
// This clears only rotation state; it never clears a health warning.
func (self *multiClientChannel) preemptServiceRefresh() bool {
	for {
		owner := self.serviceRefreshReservation.Load()
		if owner == nil {
			return true
		}
		if owner == &serviceRefreshRetiringOwner {
			return false
		}
		if self.releaseServiceRefreshReservation(owner) {
			return true
		}
	}
}

// Stale cleanup cannot clear another source's reclaimed reservation or reopen
// a slot whose swap already committed. No packet reference is returned here.
func (self *multiClientChannel) releaseServiceRefreshReservation(owner *tcpSynRetry) bool {
	if owner == nil || owner == &serviceRefreshRetiringOwner || !self.serviceRefreshReservation.CompareAndSwap(owner, nil) {
		return false
	}
	self.stateLock.Lock()
	if self.serviceRefreshReservation.Load() == nil && self.warning && self.warnCause == warnServiceRefresh {
		self.warning, self.warnCause = false, warnNone
	}
	self.stateLock.Unlock()
	return true
}

// A pass canceled by the same source's actual useful answer is no longer an
// unproductive service attempt. A new source generation cannot inherit this.
func (self *multiClientWindow) serviceRefreshAnswerObserved(update *multiClientChannelUpdate) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	owner := self.serviceRefreshOwner
	return owner != nil && owner.update != nil && (update == nil || owner.update == update) && owner.update.ctx != nil && owner.update.ctx.Err() == nil &&
		owner.retryLineage != 0 && owner.update.serviceRefreshLineage.Load() == owner.retryLineage &&
		!owner.update.sourceReset.Load() && owner.update.receivedInbound.Load()
}

// Only the refresh actually attempted by this resize pass may exempt its own
// canceled speculative work. A pending unrelated owner supplies no authority.
func (self *multiClientWindow) serviceRefreshPassAnswered(expected *tcpSynRetry) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.serviceRefreshPassAnsweredWithLock(expected)
}

// The caller owns the window lock through the backoff decision/publication.
func (self *multiClientWindow) serviceRefreshPassAnsweredWithLock(expected *tcpSynRetry) bool {
	owner := self.serviceRefreshOwner
	return expected != nil && owner == expected && self.serviceRefreshAttempted &&
		owner.update != nil && owner.update.ctx != nil && owner.update.ctx.Err() == nil &&
		owner.retryLineage != 0 && owner.update.serviceRefreshLineage.Load() == owner.retryLineage &&
		!owner.update.sourceReset.Load() && owner.update.receivedInbound.Load()
}
