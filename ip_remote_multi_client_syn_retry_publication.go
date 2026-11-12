// Provider publication uses the existing reaper and an intrusive index of
// paid SYN caches. It never walks or locks unrelated ordinary flow entries.
package connect

import (
	"slices"
	"time"
)

// The callback owns no packet, allocation, timer or new worker. A client
// already queued for publication keeps one link until its indexed pass ends.
func (self *RemoteUserNatMultiClient) enqueuePublishedSynRetries(client *multiClientChannel) {
	if self.flowReaperDone == nil || client == nil || !self.synPublicationEnabled() {
		return
	}
	self.stateLock.Lock()
	if self.ctx.Err() != nil || client.IsDone() || client.synPublicationQueued {
		self.stateLock.Unlock()
		return
	}
	client.synPublicationQueued, client.synPublicationNext = true, nil
	if self.synPublicationTail == nil {
		self.synPublicationHead = client
	} else {
		self.synPublicationTail.synPublicationNext = client
	}
	self.synPublicationTail = client
	self.stateLock.Unlock()
	self.notifyFlowReaper()
}

// Parent identity belongs to the flow lock, including repeated admissions that
// store the same pointer. This snapshot never holds the parent lock.
func (self *multiClientChannelUpdate) sequenceParentSnapshot() *RemoteUserNatMultiClient {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.sequenceParentWithLock()
}

// The observer is an intentional nil-by-default test-only critical-section
// probe; it owns no packet and must not perform production callback work.
func (self *multiClientChannelUpdate) sequenceParentWithLock() *RemoteUserNatMultiClient {
	parent := self.sequenceParent
	if parent != nil {
		if hook := parent.afterSequenceParentReadForTest; hook != nil {
			hook(self)
		}
	}
	return parent
}

// Called after materialization releases the flow lock. Index membership owns
// no packet reference, and final release fences attachment before detaching.
func (self *multiClientChannelUpdate) indexSynRetryPublication() {
	parent := self.sequenceParentSnapshot()
	if parent == nil || parent.ctx == nil || parent.flowReaperDone == nil || !parent.synPublicationEnabled() {
		return
	}
	// This source-owned capture may wait for its own flow, outside the parent.
	// Final reference release fences later attachment before unlinking it.
	if hook := parent.beforeSynPublicationIndexCaptureForTest; hook != nil {
		hook(self)
	}
	self.stateLock.Lock()
	currentParent, owner := self.sequenceParentWithLock(), self.synRetry
	opening := owner != nil && owner.openingSerial != 0
	self.stateLock.Unlock()
	// The before-capture observer may allow another source admission. Never
	// index a new parent's current owner through a stale parent snapshot.
	if currentParent != parent || owner == nil || opening {
		return
	}
	parent.stateLock.Lock()
	defer parent.stateLock.Unlock()
	if parent.ctx.Err() == nil && !owner.publicationRetired.Load() && !owner.publicationIndexed {
		owner.publicationPrevious, owner.publicationNext = parent.synPublicationOwnerTail, nil
		if parent.synPublicationOwnerTail == nil {
			parent.synPublicationOwnerHead = owner
		} else {
			parent.synPublicationOwnerTail.publicationNext = owner
		}
		parent.synPublicationOwnerTail = owner
		owner.publicationIndexed = true
		parent.synPublicationOwnerCount++
	}
}

// releaseSynRetryMemory calls this outside the flow lock and before clearing
// packet/graph fields or returning the exact credit that funded these links.
func (self *RemoteUserNatMultiClient) removeSynPublicationOwner(owner *tcpSynRetry) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !owner.publicationIndexed {
		return
	}
	if self.synPublicationCursor == owner {
		self.synPublicationCursor = owner.publicationNext
	}
	if owner.publicationPrevious == nil {
		self.synPublicationOwnerHead = owner.publicationNext
	} else {
		owner.publicationPrevious.publicationNext = owner.publicationNext
	}
	if owner.publicationNext == nil {
		self.synPublicationOwnerTail = owner.publicationPrevious
	} else {
		owner.publicationNext.publicationPrevious = owner.publicationPrevious
	}
	owner.publicationPrevious, owner.publicationNext, owner.publicationIndexed = nil, nil, false
	self.synPublicationOwnerCount--
}

// Window/channel reads happen outside the parent and flow locks. The ordinary
// dispatcher subsequently rechecks the actual permitted selection field.
func (self *RemoteUserNatMultiClient) synPublicationClientAvailable(client *multiClientChannel) bool {
	if client.args == nil || client.IsDone() || client.isWarning() || !self.reliabilitySettings().DialFailureRerace {
		return false
	}
	var stats clientWindowStats
	if err := client.windowStatsInto(false, &stats); err != nil {
		return false
	}
	for _, window := range self.windows {
		window.stateLock.Lock()
		found := window.clients[client.ClientId()] == client
		window.stateLock.Unlock()
		if found {
			return true
		}
	}
	return false
}

// Both work and collision retry timing belong to this immutable owner.
func (self *RemoteUserNatMultiClient) synPublicationEnabled() bool {
	return self.settings != nil && self.settings.SynRetryPublicationBatchSize > 0 && self.settings.SynRetryPublicationRetryDelay > 0
}

// Each event starts with a fixed paid-owner allowance. A busy owner spends
// its visit; healthy later owners still run before a deferred repeat pass.
func (self *RemoteUserNatMultiClient) drainSynPublications() {
	if hook := self.afterSynPublicationTurnForTest; hook != nil {
		defer hook()
	}
	if !self.synPublicationEnabled() {
		self.discardSynPublications()
		return
	}
	for range self.settings.SynRetryPublicationBatchSize {
		self.stateLock.Lock()
		if self.ctx.Err() != nil {
			self.stateLock.Unlock()
			return
		}
		if deferred := self.synPublicationDeferredHead; deferred != nil && !time.Now().Before(deferred.synPublicationRetryAt) {
			// Promotion is one examined work unit; due events join the live FIFO.
			self.synPublicationDeferredHead = deferred.synPublicationNext
			if self.synPublicationDeferredHead == nil {
				self.synPublicationDeferredTail = nil
			}
			deferred.synPublicationNext, deferred.synPublicationDeferred = nil, false
			deferred.synPublicationRetryAt = time.Time{}
			if self.synPublicationTail == nil {
				self.synPublicationHead = deferred
			} else {
				self.synPublicationTail.synPublicationNext = deferred
			}
			self.synPublicationTail = deferred
			self.stateLock.Unlock()
			continue
		}
		client := self.synPublicationHead
		if client == nil {
			self.stateLock.Unlock()
			return
		}
		if !self.synPublicationActive {
			self.synPublicationCursor = self.synPublicationOwnerHead
			self.synPublicationRemaining = self.synPublicationOwnerCount
			self.synPublicationActive, self.synPublicationContended = true, false
		}
		owner := self.synPublicationCursor
		if self.synPublicationRemaining == 0 || owner == nil {
			self.synPublicationHead = client.synPublicationNext
			if self.synPublicationHead == nil {
				self.synPublicationTail = nil
			}
			client.synPublicationNext = nil
			if self.synPublicationContended {
				client.synPublicationDeferred = true
				client.synPublicationRetryAt = time.Now().Add(self.settings.SynRetryPublicationRetryDelay)
				if self.synPublicationDeferredTail == nil {
					self.synPublicationDeferredHead = client
				} else {
					self.synPublicationDeferredTail.synPublicationNext = client
				}
				self.synPublicationDeferredTail = client
			} else {
				client.synPublicationQueued = false
			}
			self.synPublicationCursor, self.synPublicationActive, self.synPublicationContended = nil, false, false
			self.synPublicationRemaining = 0
			self.stateLock.Unlock()
			continue
		}
		self.synPublicationCursor = owner.publicationNext
		self.synPublicationRemaining--
		update := owner.update
		self.stateLock.Unlock()
		if update != nil && self.synPublicationClientAvailable(client) && !self.enqueuePublishedSynOwner(client, update, owner) {
			self.stateLock.Lock()
			if self.synPublicationHead == client && self.synPublicationActive {
				self.synPublicationContended = true
			}
			self.stateLock.Unlock()
		}
	}
	self.stateLock.Lock()
	pending := self.synPublicationHead != nil ||
		(self.synPublicationDeferredHead != nil && !time.Now().Before(self.synPublicationDeferredHead.synPublicationRetryAt))
	self.stateLock.Unlock()
	if pending {
		self.notifyFlowReaper()
	}
}

// One nonblocking leaf attempt is made beneath the parent. No preliminary
// wait can park this shared worker behind the same busy paid owner.
func (self *RemoteUserNatMultiClient) enqueuePublishedSynOwner(client *multiClientChannel, update *multiClientChannelUpdate, expected *tcpSynRetry) bool {
	if self.ctx.Err() != nil {
		return true
	}
	if hook := self.beforeSynPublicationOwnerLockForTest; hook != nil {
		hook(update)
	}
	if hook := self.beforeSynPublicationQueueForTest; hook != nil {
		hook(update)
	}
	self.stateLock.Lock()
	if !update.stateLock.TryLock() {
		self.stateLock.Unlock()
		return false
	}
	defer self.stateLock.Unlock()
	defer update.stateLock.Unlock()
	owner := update.synRetry
	if self.ctx.Err() != nil || client.IsDone() || owner != expected || owner == nil ||
		self.affinityUpdateWithLock(update.ipPath) != update || self.affinityTransitions[update] != nil ||
		!client.supportsIpVersion(update.ipPath.Version) || !owner.validWithLock(update) || update.client.Load() != nil ||
		!update.synRetryHasRemoteRefusalWithLock() || update.synRetryRemoteTailRefusedWithLock(client) ||
		slices.Contains(update.dialFailedClients, client) {
		return true
	}
	order := uint64(0)
	if race := update.race; race != nil {
		if race.clientStates[client] != nil || !unansweredSynRetryRaceWithLock(update, race.collapseOrder) {
			return true
		}
		race.synRetryPending = true
		if self.synRetryRaceSlotsWithLock(update, []*multiClientChannel{client}) == 0 {
			return true
		}
		order = race.collapseOrder
		race.synRetryPending = false
	}
	self.queueSynRetryWithLock(owner, order)
	return true
}

// The deferred FIFO is ordered by one immutable per-owner delay. Its head is
// the next retry; the caller takes the minimum with the existing flow deadline.
func (self *RemoteUserNatMultiClient) synPublicationNextDelay(now time.Time) (time.Duration, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !self.synPublicationEnabled() || self.synPublicationDeferredHead == nil {
		return 0, false
	}
	return max(0, self.synPublicationDeferredHead.synPublicationRetryAt.Sub(now)), true
}

// Cache-only window retirement and same-identity replacement do not run full
// flow removal. They still relinquish their publication queue graph promptly.
func (self *RemoteUserNatMultiClient) retireSynPublicationClientMetadata(client *multiClientChannel) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.retireSynPublicationClientWithLock(client)
	self.clearDestinationServiceFailuresWithLock(client)
}

// Removal is parent-lock guarded and never waits for a paid flow. If this is
// the active event, the next client starts its own complete bounded pass.
func (self *RemoteUserNatMultiClient) retireSynPublicationClientWithLock(client *multiClientChannel) {
	if client == nil || !client.synPublicationQueued {
		return
	}
	head, tail := &self.synPublicationHead, &self.synPublicationTail
	if client.synPublicationDeferred {
		head, tail = &self.synPublicationDeferredHead, &self.synPublicationDeferredTail
	}
	var previous *multiClientChannel
	for current := *head; current != nil; current = current.synPublicationNext {
		if current != client {
			previous = current
			continue
		}
		if previous == nil {
			*head = client.synPublicationNext
			if !client.synPublicationDeferred {
				self.synPublicationCursor, self.synPublicationActive, self.synPublicationContended = nil, false, false
				self.synPublicationRemaining = 0
			}
		} else {
			previous.synPublicationNext = client.synPublicationNext
		}
		if *tail == client {
			*tail = previous
		}
		client.synPublicationNext, client.synPublicationQueued, client.synPublicationDeferred = nil, false, false
		client.synPublicationRetryAt = time.Time{}
		return
	}
}

// Parent shutdown ends queued event custody; paid caches detach independently
// through their existing last-reference release after all dispatchers finish.
func (self *RemoteUserNatMultiClient) discardSynPublications() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, head := range [2]*multiClientChannel{self.synPublicationHead, self.synPublicationDeferredHead} {
		for client := head; client != nil; {
			next := client.synPublicationNext
			client.synPublicationNext, client.synPublicationQueued, client.synPublicationDeferred = nil, false, false
			client.synPublicationRetryAt = time.Time{}
			client = next
		}
	}
	self.synPublicationHead, self.synPublicationTail, self.synPublicationCursor = nil, nil, nil
	self.synPublicationDeferredHead, self.synPublicationDeferredTail = nil, nil
	self.synPublicationRemaining, self.synPublicationActive, self.synPublicationContended = 0, false, false
}
