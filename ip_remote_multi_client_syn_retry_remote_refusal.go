// An unanswered native SYN remembers remote refusals in its paid owner, so a
// new local channel to the same remote cannot erase that generation's evidence.
package connect

import "slices"

// One fixed backing covers the configured windows and one refresh candidate.
// Impossible geometry declines optional retention before any backing allocation.
func synRetryRemoteTailCapacity(parent *RemoteUserNatMultiClient, update *multiClientChannelUpdate) int {
	width := synRetryReadyCapacity(parent, update)
	windowCount := 1
	if parent != nil {
		windowCount = max(1, len(parent.windows))
	}
	maximum := int(^uint(0) >> 1)
	if width <= 0 || width > (maximum-1)/windowCount {
		return 0
	}
	capacity := width*windowCount + 1
	if capacity > maximum/len(Id{}) {
		return 0
	}
	return capacity
}

// The caller holds the flow lock, after this exact provider's failure acquired
// standing. A full backing declines automatic reuse; it never silently evicts.
func (self *multiClientChannelUpdate) noteSynRetryRemoteRefusalWithLock(client *multiClientChannel) {
	self.synRetry.noteRemoteRefusalWithLock(self, client)
}

// A standing signal records a fact for this exact live owner. A pending
// ordinary sibling may delay dispatch without erasing that received fact.
func (self *tcpSynRetry) noteRemoteRefusalWithLock(update *multiClientChannelUpdate, client *multiClientChannel) {
	if client == nil || client.args == nil || !self.validGenerationWithLock(update) || self.openingSerial != 0 ||
		self.serial == 0 || self.references == 0 || !update.dialFailedSynSeen || self.sequence != update.dialFailedSyn ||
		self.pendingGenerationChangeWithLock(update) {
		return
	}
	id := client.Destination().Tail()
	if slices.Contains(self.refusedRemoteIds, id) {
		return
	}
	if len(self.refusedRemoteIds) == cap(self.refusedRemoteIds) {
		self.remoteRefusalOverflow = true
		return
	}
	self.refusedRemoteIds = append(self.refusedRemoteIds, id)
}

// Only ordinary same-sequence SYN claims may coexist with history recording.
// Terminal or different-generation controls remain fences, without reading
// packet bytes whose native caller has already transferred them.
func (self *tcpSynRetry) pendingGenerationChangeWithLock(update *multiClientChannelUpdate) bool {
	client := update.client.Load()
	for group := update.sequenceClaims; group != nil; group = group.collapseNext {
		if group.completionFlags.Load()&(groupCompletionDequeued|groupCompletionAbandoned) != 0 ||
			client != nil && group.completionClient != client ||
			client == nil && !group.awaitingCollapsePromotionWithLock() {
			continue
		}
		for _, packet := range group.packets {
			path := packet.ipPath
			if path.Rst || path.Fin || path.Syn && (path.Ack || path.SequenceNumber != self.sequence) {
				return true
			}
		}
	}
	return false
}

// The flow lock protects the paid owner and its exact current source sequence.
func (self *multiClientChannelUpdate) synRetryRemoteRefusalOwnerWithLock() *tcpSynRetry {
	owner := self.synRetry
	if owner == nil || owner.openingSerial != 0 || !owner.validWithLock(self) ||
		!self.dialFailedSynSeen || owner.sequence != self.dialFailedSyn {
		return nil
	}
	return owner
}

// A publication needs a standing remote refusal before it may wake work.
func (self *multiClientChannelUpdate) synRetryHasRemoteRefusalWithLock() bool {
	owner := self.synRetryRemoteRefusalOwnerWithLock()
	return owner != nil && len(owner.refusedRemoteIds) != 0
}

// Overflow is unknown freshness and therefore refuses automatic continuation.
// A later ordinary native source offer retains its existing routing policy.
func (self *tcpSynRetry) remoteTailRefusedWithLock(client *multiClientChannel) bool {
	return self != nil && client != nil && client.args != nil &&
		(self.remoteRefusalOverflow || slices.Contains(self.refusedRemoteIds, client.Destination().Tail()))
}

// Publication and final admission use the same paid generation's remote proof.
func (self *multiClientChannelUpdate) synRetryRemoteTailRefusedWithLock(client *multiClientChannel) bool {
	return self.synRetryRemoteRefusalOwnerWithLock().remoteTailRefusedWithLock(client)
}

// The publication that woke this retry is only a wake. Check every candidate
// in the actual selection field, including new channels to a refused provider.
func (self *parsedPacketGroup) excludeRefusedSynRetryRemotes(update *multiClientChannelUpdate, candidates []*multiClientChannel) []*multiClientChannel {
	if len(candidates) == 0 {
		return candidates
	}
	update.stateLock.Lock()
	defer update.stateLock.Unlock()
	owner := update.synRetryRemoteRefusalOwnerWithLock()
	if owner == nil || self.synRetry.update != update || self.synRetry.serial != owner.serial {
		return candidates
	}
	return slices.DeleteFunc(candidates, owner.remoteTailRefusedWithLock)
}
