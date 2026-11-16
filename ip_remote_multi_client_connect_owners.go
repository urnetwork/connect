// Source-ordered TCP openings own retractable scalar connect-health evidence.
// No node retains a flow, packet, source worker, context or channel graph.
package connect

import (
	"sync/atomic"
	"time"
	"unsafe"
)

// Includes transient candidate nodes; exhaustion preserves conservative legacy
// evidence rather than refusing a packet or losing an unanswered obligation.
const connectSynOwnerMaxCount = 64

const connectSynQuotaClosedBit uint64 = 1 << 63

type connectSynOwnerQuota struct {
	state  atomic.Uint64 // one atomic closed bit plus callback/node count
	budget *TransferMemoryBudget
	charge ByteCount
}

func (self *connectSynOwnerQuota) reserveOwner() bool {
	for {
		state := self.state.Load()
		if state&connectSynQuotaClosedBit != 0 || state == 0 || state > connectSynOwnerMaxCount {
			return false
		}
		if self.state.CompareAndSwap(state, state+1) {
			return true
		}
	}
}

// The state that loses the final reference owns the only quota-byte release.
func (self *connectSynOwnerQuota) releaseOwner() {
	if self.state.Add(^uint64(0)) == connectSynQuotaClosedBit && self.budget != nil {
		self.budget.Release(self.charge)
	}
}

func (self *connectSynOwnerQuota) close() {
	for {
		state := self.state.Load()
		if state&connectSynQuotaClosedBit != 0 {
			return
		}
		next := (state | connectSynQuotaClosedBit) - 1 // drop channel reference
		if self.state.CompareAndSwap(state, next) {
			if next == connectSynQuotaClosedBit && self.budget != nil {
				self.budget.Release(self.charge)
			}
			return
		}
	}
}

// Channel lock owns age/list fields. Flow lock owns source-order fields.
// Retirement and references cross those boundaries through atomics only.
type connectSynOwner struct {
	retired            atomic.Bool
	references         atomic.Int64
	quota              *connectSynOwnerQuota
	budget             *TransferMemoryBudget
	charge             ByteCount
	epoch              uint64
	sourceId           Id
	sourceOrder        uint64
	cohortOrder        uint64
	sourceKey          multiClientAffinitySourceKey
	flowNext           *connectSynOwner
	sequence           uint32
	started            time.Time
	linked             bool
	next               *connectSynOwner
	releaseNext        *connectSynOwner
	channelReleaseNext *connectSynOwner
}

// Called before taking any packet/flow/channel state lock. New pointed-to
// storage is charged independently of the existing retained group descriptor.
func (self *multiClientChannel) prepareConnectSynOwner(offer *connectSynOffer) {
	if offer == nil || self.client == nil || self.client.settings == nil ||
		self.client.settings.SendBufferSettings == nil || self.connectSynQuotaClosed.Load() {
		return
	}
	budget := self.client.settings.SendBufferSettings.ResendQueueBudget
	quota := self.connectSynQuota.Load()
	if quota == nil {
		charge := (ByteCount(unsafe.Sizeof(connectSynOwnerQuota{})) + 63) / 64 * 64
		if budget != nil && !budget.TryReserve(charge) {
			return
		}
		candidate := &connectSynOwnerQuota{budget: budget, charge: charge}
		candidate.state.Store(1) // channel ownership, detached only by close
		if hook := self.beforeConnectSynQuotaPublishForTest; hook != nil {
			hook()
		}
		if self.connectSynQuota.CompareAndSwap(nil, candidate) {
			quota = candidate
		} else {
			if budget != nil {
				budget.Release(charge)
			}
			quota = self.connectSynQuota.Load()
		}
	}
	if self.connectSynQuotaClosed.Load() {
		quota.close()
		return
	}
	if !quota.reserveOwner() {
		return
	}
	charge := (ByteCount(unsafe.Sizeof(connectSynOwner{}))+63)/64*64 + (ByteCount(unsafe.Sizeof(connectSynOffer{}))+63)/64*64
	if budget != nil && !budget.TryReserve(charge) {
		quota.releaseOwner()
		return
	}
	owner := &connectSynOwner{quota: quota, budget: budget, charge: charge, epoch: offer.epoch}
	owner.references.Store(1) // this offer through BOTH public and terminal/refusal
	offer.candidate = owner
}

func (self *connectSynOwner) retain() bool {
	if self == nil {
		return false
	}
	for {
		references := self.references.Load()
		if references <= 0 {
			return false
		}
		if self.references.CompareAndSwap(references, references+1) {
			return true
		}
	}
}

// Only outside flow/channel locks: budget notification can call other owners.
func (self *connectSynOwner) release() {
	if self == nil {
		return
	}
	left := self.references.Add(-1)
	if left < 0 {
		panic("connect SYN owner released without ownership")
	}
	if left == 0 {
		if self.budget != nil {
			self.budget.Release(self.charge)
		}
		self.quota.releaseOwner()
	}
}

// Both logical boundaries must finish, then the flow lock synchronizes
// pointer detachment with the original source binder. Done/finished fences
// prevent retained race metadata from publishing a new binding afterward.
func (self *parsedPacketGroup) releaseConnectSynReferences() {
	offer := self.connectSynOffer
	if offer == nil || !offer.publicReturned.Load() || !offer.terminalFinished.Load() {
		return
	}
	update := self.collapseAdmission.update
	if update != nil {
		update.stateLock.Lock()
	}
	var candidate, owner *connectSynOwner
	if offer.referencesReleased.CompareAndSwap(false, true) {
		candidate, owner = offer.candidate, offer.owner.Swap(nil)
		offer.candidate = nil
	}
	if update != nil {
		update.stateLock.Unlock()
	}
	if owner != nil && owner != candidate {
		owner.release()
	}
	candidate.release()
}

// Existing source-order guards run first for selected and candidate groups.
// Exact source keys index a separate stable flow list. No pointer into the
// resizable affinity-source slice is retained or published.
func (self *parsedPacketGroup) observeConnectSynSourceWithLock(cohortOrder uint64) {
	if self.affinitySourceRegistration == 0 || self.collapseOrder() == 0 || self.completionFlags.Load()&groupCompletionDone != 0 {
		return
	}
	update := self.collapseAdmission.update
	entry := update.affinitySources.entryWithLock(int(self.affinitySourceRegistration&0xff) - 1)
	if entry == nil || entry.epoch != self.affinitySourceRegistration>>8 {
		return
	}
	sourceId, order := self.collapseSourceIdWithLock(), self.collapseOrder()
	var owner *connectSynOwner
	for candidate := update.connectSynOwners; candidate != nil; candidate = candidate.flowNext {
		if candidate.sourceKey == entry.key {
			owner = candidate
			break
		}
	}
	if owner != nil && (sourceId.LessThan(owner.sourceId) || sourceId == owner.sourceId && order <= owner.sourceOrder) {
		return
	}
	for index := range self.packets {
		path := self.packets[index].ipPath
		if path.Rst {
			update.detachConnectSynOwnerWithLock(owner, true)
			owner = nil
		}
		if !path.Syn {
			continue
		}
		offer := self.connectSynOffer
		if offer == nil || offer.ownershipAmbiguous || offer.referencesReleased.Load() || offer.owner.Load() != nil {
			continue
		}
		if owner != nil && (owner.retired.Load() || owner.epoch != offer.epoch || owner.sequence != path.SequenceNumber || cohortOrder != 0 && owner.cohortOrder != cohortOrder) {
			update.detachConnectSynOwnerWithLock(owner, true)
			owner = nil
		}
		if owner == nil {
			candidate := offer.candidate
			if candidate == nil || !candidate.retain() {
				continue
			}
			owner = candidate
			owner.sourceId, owner.sourceOrder, owner.sequence = sourceId, order, path.SequenceNumber
			owner.cohortOrder, owner.sourceKey = cohortOrder, entry.key
			owner.flowNext, update.connectSynOwners = update.connectSynOwners, owner // exact flow/source reference
		} else if !owner.retain() {
			continue
		}
		// Candidate already owns the complete public+terminal offer lease; an
		// existing source owner needs one lease for this later whole offer.
		offer.owner.Store(owner)
	}
	if owner != nil {
		owner.sourceId, owner.sourceOrder = sourceId, order
	}
}

// Terminal application reset is exact; generic cancellation/Close only drops
// local ownership and preserves conservative provider evidence on the channel.
func (self *multiClientChannelUpdate) detachConnectSynOwnerWithLock(owner *connectSynOwner, retire bool) {
	if owner == nil {
		return
	}
	found := false
	for link := &self.connectSynOwners; *link != nil; link = &(*link).flowNext {
		if *link == owner {
			*link = owner.flowNext
			owner.flowNext = nil
			found = true
			break
		}
	}
	if !found {
		return
	}
	if retire {
		owner.retired.Store(true)
	}
	owner.releaseNext = self.connectSynReleased.Load()
	self.connectSynReleased.Store(owner)
}

func (self *multiClientChannelUpdate) detachConnectSynOwnersWithLock(retire bool) {
	for self.connectSynOwners != nil {
		self.detachConnectSynOwnerWithLock(self.connectSynOwners, retire)
	}
}

func (self *multiClientChannelUpdate) releaseConnectSynOwners() {
	if self.connectSynReleased.Load() == nil {
		return
	}
	self.stateLock.Lock()
	released := self.connectSynReleased.Swap(nil)
	self.stateLock.Unlock()
	for owners := released; owners != nil; {
		next := owners.releaseNext
		owners.releaseNext = nil
		owners.release()
		owners = next
	}
}

// Deferred releases leave the channel lock before notifying shared budgets.
func (self *multiClientChannel) releaseConnectSynOwners() {
	if self.connectSynReleased.Load() == nil {
		return
	}
	self.stateLock.Lock()
	released := self.connectSynReleased.Swap(nil)
	self.stateLock.Unlock()
	for owners := released; owners != nil; {
		next := owners.channelReleaseNext
		owners.channelReleaseNext = nil
		owners.release()
		owners = next
	}
}

func (self *multiClientChannel) unlinkConnectSynOwnerWithLock(link **connectSynOwner) {
	owner := *link
	*link = owner.next
	owner.linked = false
	owner.next = nil
	owner.channelReleaseNext = self.connectSynReleased.Load()
	self.connectSynReleased.Store(owner)
}

// Recompute before snapshots/verdicts. Bounded owned records preserve each
// surviving opening's age; old unowned confirmation keeps legacy behavior.
func (self *multiClientChannel) recomputeConnectSynWithLock() {
	first := self.connectSynConfirmedTime
	for link := &self.connectSynOwners; *link != nil; {
		owner := *link
		if owner.retired.Load() || owner.epoch != self.connectSynEpoch {
			self.unlinkConnectSynOwnerWithLock(link)
			continue
		}
		if !owner.started.IsZero() && (first.IsZero() || owner.started.Before(first)) {
			first = owner.started
		}
		link = &owner.next
	}
	for offer := self.connectSynPending; offer != nil; offer = offer.next {
		if owner := offer.owner.Load(); owner != nil && owner.retired.Load() {
			continue
		}
		if first.IsZero() || offer.started.Before(first) {
			first = offer.started
		}
	}
	self.packetStats.firstUnansweredSendSynTime = first
}

func (self *multiClientChannel) retainConnectSynOwnerWithLock(offer *connectSynOffer) bool {
	owner := offer.owner.Load()
	if owner == nil {
		return false
	}
	if self.connectSynClosed {
		owner.retired.Store(true)
	}
	if owner.retired.Load() || owner.epoch != self.connectSynEpoch {
		return true
	}
	if owner.started.IsZero() || offer.started.Before(owner.started) {
		owner.started = offer.started
	}
	if !owner.linked && owner.retain() {
		owner.next, self.connectSynOwners, owner.linked = self.connectSynOwners, owner, true
	}
	return true
}

func (self *multiClientChannel) clearConnectSynOwnersWithLock() {
	for self.connectSynOwners != nil {
		self.connectSynOwners.retired.Store(true)
		self.unlinkConnectSynOwnerWithLock(&self.connectSynOwners)
	}
}

func (self *multiClientChannel) closeConnectSynOwners() {
	self.connectSynQuotaClosed.Store(true)
	self.stateLock.Lock()
	self.connectSynClosed = true
	self.clearConnectSynOwnersWithLock()
	self.stateLock.Unlock()
	self.releaseConnectSynOwners()
	if quota := self.connectSynQuota.Load(); quota != nil {
		quota.close()
	}
}
