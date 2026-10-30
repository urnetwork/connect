// Retained SYN readiness owns identity metadata, never packet references.
// Existing parent reaper work consumes a granted FIFO turn; no timer or
// additional worker retries a capacity level.
package connect

import (
	"sync/atomic"
	"unsafe"
)

// Cache, index and active reaper references share this separately paid owner.
// Flow state guards slot/fence fields; the parent guards index links. A final
// reference releases credit outside both locks, independently of packet bytes.
type tcpSynRetryReadiness struct {
	parent             *RemoteUserNatMultiClient
	update             *multiClientChannelUpdate
	credit             *transferRetainedControlCredit
	references         atomic.Int32
	closed             atomic.Bool
	serial             uint64
	sequence           uint32
	epoch              uint64
	raceOrder          uint64
	sourceOrder        uint64
	attemptEpoch       uint64
	attemptSourceOrder uint64
	slots              []tcpSynRetryReadySlot
	count              int
	indexed            atomic.Bool
	previous, next     *tcpSynRetryReadiness
}

// Exact source/gate refusal and immutable installation token. The embedded
// intrusive waiter needs no allocation, notification channel or queue backing.
type tcpSynRetryReadySlot struct {
	source       *SendSequence
	gate         *resendCapacityGate
	waiter       resendCapacityWaiter
	installation uint64
	raceOrder    uint64
	sourceOrder  uint64
}

// Prepay the configured field and any already-created wider race. An admission
// that cannot pay this extra metadata keeps the original native-RTO fallback;
// the ordinary provider race is never narrowed to fit this quota.
func synRetryReadyCapacity(parent *RemoteUserNatMultiClient, update *multiClientChannelUpdate) int {
	capacity := 1
	if race := update.race; race != nil {
		capacity = max(capacity, race.clientLimit)
	}
	if parent != nil && parent.settings != nil {
		capacity = max(capacity, parent.settings.MultiRaceClientCount, parent.settings.MultiRaceDegradedClientCount, parent.settings.WindowSizeLimit)
		for _, settings := range parent.settings.WindowSizes {
			capacity = max(capacity, settings.WindowSizeMin, settings.WindowSizeMax, settings.WindowSizeHardMax, settings.FixedWindowSize)
		}
	}
	return capacity
}

func newSynRetryReadiness(parent *RemoteUserNatMultiClient, update *multiClientChannelUpdate, source *SendSequence, serial uint64, sequence uint32, capacity int) *tcpSynRetryReadiness {
	if parent == nil || parent.flowReaperDone == nil {
		return nil
	}
	slotBytes := int(unsafe.Sizeof(tcpSynRetryReadySlot{}))
	if capacity > int(^uint(0)>>1)/slotBytes {
		return nil
	}
	bytes := addReceiveQueueByteCount(retainedMessageCapacity(ByteCount(unsafe.Sizeof(tcpSynRetryReadiness{}))), retainedMessageCapacity(ByteCount(capacity*slotBytes)))
	credit, ok := source.reserveRetainedControlMemory(bytes)
	if !ok {
		return nil
	}
	ready := &tcpSynRetryReadiness{parent: parent, update: update, credit: credit, serial: serial, sequence: sequence, slots: make([]tcpSynRetryReadySlot, capacity)}
	ready.references.Store(1)
	return ready
}

func (ready *tcpSynRetryReadiness) release() {
	if ready == nil {
		return
	}
	references := ready.references.Add(-1)
	if references < 0 {
		panic("readiness owner lost its reference")
	}
	if references != 0 {
		return
	}
	if ready.indexed.Load() || ready.count != 0 {
		panic("readiness owner still indexed")
	}
	for index := range ready.slots {
		slot := &ready.slots[index]
		if slot.source != nil || slot.gate != nil || slot.waiter.owner.Load() != nil {
			panic("readiness owner still watches source")
		}
	}
	credit := ready.credit
	ready.credit, ready.parent, ready.update, ready.slots = nil, nil, nil, nil
	ready.previous, ready.next = nil, nil
	credit.Release()
}

// Selection publishes the actual attempt field without changing its race.
func (group *parsedPacketGroup) noteSynRetryReadinessWidth(width int) {
	if !group.isSynRetry() {
		return
	}
	update := group.synRetry.update
	update.stateLock.Lock()
	defer update.stateLock.Unlock()
	if owner := update.synRetry; owner != nil && group.validSynRetryWithLock(update) {
		owner.readinessWidth = max(owner.readinessWidth, width)
	}
}

// The exact source is known before its reliable admission. A wider legitimate
// field may acquire a full replacement metadata lease, without retaining the
// original funding source graph or silently remembering only its first gates.
func (group *parsedPacketGroup) prepareSynRetryReadinessCapacity(source *SendSequence) {
	if !group.isSynRetry() || source == nil {
		return
	}
	update := group.synRetry.update
	var replaced *tcpSynRetryReadiness
	update.stateLock.Lock()
	owner := update.synRetry
	if owner == nil || !group.validSynRetryWithLock(update) || owner.readinessOverflow {
		update.stateLock.Unlock()
		return
	}
	old := owner.readiness
	width := max(1, owner.readinessWidth)
	if old != nil && len(old.slots) >= width {
		update.stateLock.Unlock()
		return
	}
	parent := update.sequenceParent
	fresh := newSynRetryReadiness(parent, update, source, owner.serial, owner.sequence, width)
	if fresh == nil || old != nil && old.indexed.Load() {
		owner.readinessOverflow = true
		if parent != nil {
			parent.synRetryReadinessRefusals.Add(1)
		}
		update.stateLock.Unlock()
		if fresh != nil {
			fresh.release()
		}
		return
	}
	fresh.attemptEpoch, fresh.attemptSourceOrder = owner.readinessEpoch, owner.readinessSourceOrder
	if old != nil {
		for index := 0; index < old.count; index++ {
			from, to := &old.slots[index], &fresh.slots[index]
			to.source, to.gate, to.raceOrder, to.sourceOrder = from.source, from.gate, from.raceOrder, from.sourceOrder
		}
		fresh.count = old.count
		old.clearSlotsWithLock()
		old.closed.Store(true)
		replaced = old
	}
	owner.readiness = fresh
	update.stateLock.Unlock()
	if replaced != nil {
		replaced.release()
	}
}

// Synchronous false admission still owns the descriptor. Only a retained retry
// at this exact capacity boundary may record one source identity.
func (group *parsedPacketGroup) noteSynRetryCapacityRefusal(source *SendSequence, gate *resendCapacityGate) {
	if !group.isSynRetry() || source == nil || gate == nil {
		return
	}
	update := group.synRetry.update
	update.stateLock.Lock()
	defer update.stateLock.Unlock()
	owner := update.synRetry
	if owner == nil || !group.validSynRetryWithLock(update) || owner.readiness == nil || owner.readinessOverflow {
		return
	}
	ready := owner.readiness
	if ready.closed.Load() || ready.indexed.Load() || ready.attemptEpoch != update.sequenceAdmissionEpoch {
		return
	}
	for index := 0; index < ready.count; index++ {
		if ready.slots[index].source == source && ready.slots[index].gate == gate {
			return
		}
	}
	if ready.count == len(ready.slots) {
		owner.readinessOverflow = true
		ready.parent.synRetryReadinessRefusals.Add(1)
		return
	}
	slot := &ready.slots[ready.count]
	slot.source, slot.gate = source, gate
	slot.sourceOrder = ready.attemptSourceOrder
	if flags := group.completionFlags.Load(); flags&groupCompletionCandidate != 0 {
		slot.raceOrder = flags >> 16
		slot.sourceOrder = slot.raceOrder
	}
	ready.count++
}

// Parent and flow locks are held. Leaf gate methods only publish a nonblocking
// wake, never call back, so token publication and index insertion are atomic
// to the reaper even when watch grants immediately.
func (self *RemoteUserNatMultiClient) armSynRetryReadiness(owner *tcpSynRetry) {
	ready := owner.readiness
	if ready == nil {
		return
	}
	if hook := self.beforeSynRetryReadinessArmForTest; hook != nil {
		hook(owner.update)
	}
	self.stateLock.Lock()
	update := owner.update
	update.stateLock.Lock()
	defer self.stateLock.Unlock()
	defer update.stateLock.Unlock()
	if self.ctx.Err() != nil || ready.closed.Load() || self.affinityUpdateWithLock(update.ipPath) != update || self.affinityTransitions[update] != nil || !owner.validWithLock(update) || owner.queued || owner.readinessOverflow || update.client.Load() != nil || ready.attemptEpoch != update.sequenceAdmissionEpoch {
		ready.clearSlotsWithLock()
		return
	}
	order := uint64(0)
	if race := update.race; race != nil {
		order = race.collapseOrder
		if !unansweredSynRetryRaceWithLock(update, order) {
			ready.clearSlotsWithLock()
			return
		}
	}
	ready.epoch, ready.raceOrder, ready.sourceOrder = update.sequenceAdmissionEpoch, order, update.sequenceSourceOrder
	watched := false
	for index := 0; index < ready.count; index++ {
		slot := &ready.slots[index]
		valid := slot.source != nil && slot.source.ctx.Err() == nil && slot.gate != nil && !slot.gate.closed.Load()
		if order == 0 {
			valid = valid && slot.sourceOrder == ready.sourceOrder
		} else {
			valid = valid && slot.raceOrder == order
		}
		if valid {
			slot.installation = slot.gate.watch(&slot.waiter, self.flowReaperWake)
			if slot.installation != 0 && slot.gate == &slot.source.resendCapacityUnavailable {
				slot.source.notifyRetainedAdmissionDemand()
			}
			watched = watched || slot.installation != 0
		}
		if slot.installation == 0 {
			slot.source, slot.gate = nil, nil
		}
	}
	if watched {
		ready.references.Add(1)
		self.appendSynRetryReadinessWithLock(ready)
	}
}

func (ready *tcpSynRetryReadiness) clearSlotsWithLock() {
	for index := 0; index < ready.count; index++ {
		slot := &ready.slots[index]
		if slot.gate != nil && slot.installation != 0 {
			slot.gate.abandonWatch(&slot.waiter, slot.installation)
			if slot.gate == &slot.source.resendCapacityUnavailable {
				slot.source.notifyRetainedAdmissionDemand()
			}
		}
		slot.source, slot.gate, slot.installation = nil, nil, 0
		slot.raceOrder, slot.sourceOrder = 0, 0
	}
	ready.count = 0
}

func (self *RemoteUserNatMultiClient) appendSynRetryReadinessWithLock(ready *tcpSynRetryReadiness) {
	if ready.indexed.Load() {
		panic("readiness index already owns registration")
	}
	ready.previous, ready.next = self.synRetryReadyTail, nil
	ready.indexed.Store(true)
	if self.synRetryReadyTail == nil {
		self.synRetryReadyHead = ready
	} else {
		self.synRetryReadyTail.next = ready
	}
	self.synRetryReadyTail = ready
	self.synRetryReadyCount++
}

// The index reference transfers to the caller, or back into the same index.
func (self *RemoteUserNatMultiClient) removeSynRetryReadinessWithLock(ready *tcpSynRetryReadiness) bool {
	if !ready.indexed.Load() {
		return false
	}
	if ready.previous == nil {
		self.synRetryReadyHead = ready.next
	} else {
		ready.previous.next = ready.next
	}
	if ready.next == nil {
		self.synRetryReadyTail = ready.previous
	} else {
		ready.next.previous = ready.previous
	}
	ready.previous, ready.next = nil, nil
	ready.indexed.Store(false)
	self.synRetryReadyCount--
	return true
}

// Cancellation and a new legitimate attempt detach only this metadata. Credit
// releases and source wakeups happen after the parent/flow locks are released.
func (ready *tcpSynRetryReadiness) detach() {
	parent, update := ready.parent, ready.update
	parent.stateLock.Lock()
	update.stateLock.Lock()
	removed := parent.removeSynRetryReadinessWithLock(ready)
	ready.clearSlotsWithLock()
	update.stateLock.Unlock()
	parent.stateLock.Unlock()
	if removed {
		ready.release()
	}
}

func (self *RemoteUserNatMultiClient) synRetryReadinessValidWithLock(ready *tcpSynRetryReadiness) (*tcpSynRetry, bool) {
	update := ready.update
	owner := update.synRetry
	valid := self.ctx.Err() == nil && !ready.closed.Load() && self.affinityUpdateWithLock(update.ipPath) == update && self.affinityTransitions[update] == nil && owner != nil && owner.readiness == ready && owner.serial == ready.serial && owner.sequence == ready.sequence && owner.validWithLock(update) && !owner.queued && update.client.Load() == nil && update.sequenceAdmissionEpoch == ready.epoch && self.synRetryDispatchEligibleWithLock(update, ready.raceOrder)
	if ready.raceOrder == 0 {
		valid = valid && update.sequenceSourceOrder == ready.sourceOrder
	}
	return owner, valid
}

// A finite rotation inspects each indexed registration at most once per pass.
// Unchanged capacity requeues its identity without issuing another wake.
func (self *RemoteUserNatMultiClient) drainSynRetryReadiness() {
	self.stateLock.Lock()
	count := self.synRetryReadyCount
	self.stateLock.Unlock()
	for index := 0; index < count; index++ {
		self.stateLock.Lock()
		ready := self.synRetryReadyHead
		if ready != nil {
			self.removeSynRetryReadinessWithLock(ready)
		}
		self.stateLock.Unlock()
		if ready == nil {
			return
		}
		self.processSynRetryReadiness(ready)
	}
}

func (self *RemoteUserNatMultiClient) processSynRetryReadiness(ready *tcpSynRetryReadiness) {
	keep := false
	defer func() {
		if !keep {
			ready.release()
		}
	}()
	update := ready.update
	self.stateLock.Lock()
	update.stateLock.Lock()
	_, valid := self.synRetryReadinessValidWithLock(ready)
	var grantedSource *SendSequence
	waiting := false
	if valid {
		for index := 0; index < ready.count; index++ {
			slot := &ready.slots[index]
			if slot.gate == nil {
				continue
			}
			if slot.source.ctx.Err() != nil || slot.gate.closed.Load() {
				slot.gate.abandonWatch(&slot.waiter, slot.installation)
				if slot.gate == &slot.source.resendCapacityUnavailable {
					slot.source.notifyRetainedAdmissionDemand()
				}
				slot.source, slot.gate, slot.installation = nil, nil, 0
				continue
			}
			waiting = true
			if slot.gate.watchGranted(&slot.waiter, slot.installation) && slot.gate.passWatch(&slot.waiter, slot.installation) {
				grantedSource = slot.source
				break
			}
		}
	}
	if grantedSource != nil || !valid || !waiting {
		ready.clearSlotsWithLock()
	} else {
		self.appendSynRetryReadinessWithLock(ready)
		keep = true
	}
	update.stateLock.Unlock()
	self.stateLock.Unlock()
	if grantedSource == nil {
		return
	}
	if hook := self.beforeSynRetryReadinessForTest; hook != nil {
		hook(update)
	}
	self.stateLock.Lock()
	update.stateLock.Lock()
	owner, valid := self.synRetryReadinessValidWithLock(ready)
	if valid && grantedSource.ctx.Err() == nil {
		self.queueSynRetryWithLock(owner, ready.raceOrder)
	}
	update.stateLock.Unlock()
	self.stateLock.Unlock()
}

func (self *RemoteUserNatMultiClient) discardSynRetryReadiness() {
	for {
		self.stateLock.Lock()
		ready := self.synRetryReadyHead
		if ready != nil {
			self.removeSynRetryReadinessWithLock(ready)
		}
		self.stateLock.Unlock()
		if ready == nil {
			return
		}
		ready.update.stateLock.Lock()
		ready.clearSlotsWithLock()
		ready.update.stateLock.Unlock()
		ready.release()
	}
}
