// Group admission reuses its parsed descriptor; range storage starts only
// with reliable retention. The flow lock guards claims and immutable metadata
// observations. Terminal/return flags also cover callback-before-return.
package connect

import (
	"time"
	"unsafe"
)

const (
	groupCompletionAck uint64 = 1 << iota
	groupCompletionTracked
	groupCompletionAdmitted
	groupCompletionDequeued
	groupCompletionLinked
	groupCompletionDisposed
	groupCompletionDone
	groupCompletionTerminal
	groupCompletionReturned
	groupCompletionObserved
	groupCompletionCandidate
	groupCompletionAbandoned
	groupCompletionSourceControls
	groupCompletionSynOffer
	groupCompletionReceived
)

// Before dequeue, upper bits identify the exact candidate registration. The
// source replaces that stamp with FIFO order only after validating it.
func (self *parsedPacketGroup) collapseOrder() uint64 {
	flags := self.completionFlags.Load()
	if flags&groupCompletionDequeued == 0 {
		return 0
	}
	return flags >> 16
}

// Exhaustion fails open instead of wrapping into lifetime flags or making an
// old candidate registration equal to a new one. No packet owner is affected.
func (self *multiClientChannelUpdate) nextCollapseOrderWithLock() uint64 {
	if self.sequenceSourceOrder == 1<<48-1 {
		return 0
	}
	self.sequenceSourceOrder++
	return self.sequenceSourceOrder
}

// Only additional retained range state is allocated. The parsed descriptor
// already owns completion, source order, and provisional raw membership.
type tcpCollapseGroup struct {
	materialized uint64
	disposed     uint64
	stateWords   []uint64
	remaining    int
	published    int
	budget       *TransferMemoryBudget
	budgetBytes  ByteCount
	sourceId     Id
}

// Replaces the escaping callback and once without a prequeue allocation or
// reservation. Test seams keep their legacy whole-group admission contract.
func (self *multiClientChannel) prepareGroupCompletion(group *parsedPacketGroup, ack bool, raceOrder uint64) {
	// The same caller-owned descriptor can retry a refused offer. A refusal
	// never acquired source ownership, so no previous target remains live.
	group.completionFlags.Store(group.completionFlags.Load() & groupCompletionSynOffer)
	group.collapseOwner, group.collapseNext = nil, nil
	group.collapseSource, group.admissionTime = nil, time.Time{}
	group.completionClient = self
	if ack {
		group.completionFlags.Or(groupCompletionAck)
	}
	if ack && group.ipPath != nil && group.ipPath.Protocol == IpProtocolTcp &&
		len(group.packets) != 0 && group.collapseAdmission.update != nil && self.sendTransferForTest == nil {
		group.completionFlags.Or(groupCompletionTracked)
		update := group.collapseAdmission.update
		update.stateLock.Lock()
		if raceOrder != 0 {
			group.completionFlags.Or(groupCompletionCandidate | raceOrder<<16)
			group.validateCollapseCandidateWithLock()
		} else if update.client.Load() != self {
			group.completionFlags.Or(groupCompletionAbandoned)
		}
		update.stateLock.Unlock()
	}
}

// Refusal drops only its close barrier, never generation or coverage. A true
// return has already transferred that barrier to the existing admitted claim.
func (self *parsedPacketGroup) finishCollapseSynOffer() {
	if self.completionFlags.Load()&groupCompletionSynOffer == 0 {
		return
	}
	if self.completionFlags.And(^groupCompletionSynOffer)&groupCompletionSynOffer == 0 {
		return
	}
	update := self.collapseAdmission.update
	update.stateLock.Lock()
	update.sequenceSynOffers--
	update.stateLock.Unlock()
	update.releaseCollapseClaims()
}

// A delayed producer must not join a replacement race merely because it
// contains the same provider. The committed stamp admits only its exact winner.
func (self *parsedPacketGroup) validateCollapseCandidateWithLock() {
	flags := self.completionFlags.Load()
	if flags&groupCompletionCandidate == 0 || flags&groupCompletionDequeued != 0 {
		return
	}
	update := self.collapseAdmission.update
	stamp := flags >> 16
	if update.client.Load() == self.completionClient && update.sequenceCommittedRaceOrder == stamp {
		return
	}
	if race := update.race; race != nil && race.collapseOrder == stamp && race.clientStates[self.completionClient] != nil {
		return
	}
	self.completionFlags.Or(groupCompletionAbandoned)
}

// Only this still-live registered candidate can retain metadata after terminal
// completion. Race clear marks every linked loser before a later race can start.
func (self *parsedPacketGroup) awaitingCollapsePromotionWithLock() bool {
	update := self.collapseAdmission.update
	flags := self.completionFlags.Load()
	return flags&groupCompletionCandidate != 0 && flags&groupCompletionAbandoned == 0 &&
		self.completionClient != update.client.Load() && !update.IsDone() && !self.completionClient.IsDone() &&
		update.race != nil && update.race.clientStates[self.completionClient] != nil
}

// Measures only the new lazy allocation, including overflow capacity. The
// descriptor replaces existing escaped ownership and is measured separately.
func (self *parsedPacketGroup) sendGroupMemoryByteCount() ByteCount {
	if self.completionFlags.Load()&groupCompletionTracked == 0 || self.collapseOwner != nil {
		return 0
	}
	capacity := 0
	if len(self.packets) > 64 {
		words := 2 * ((len(self.packets) - 1) / 64)
		capacity = 2
		for capacity < words {
			capacity *= 2
		}
	}
	bytes := (ByteCount(unsafe.Sizeof(tcpCollapseGroup{}))+63)/64*64 + ByteCount(capacity)*8
	if self.completionFlags.Load()&groupCompletionCandidate != 0 {
		// Only a candidate can outlive both callback and offer. Reserve its
		// surviving graph with the first item; terminal trimming drops all
		// packet roots and the live source before this extended phase.
		bytes = addReceiveQueueByteCount(bytes, self.collapsePromotionByteCount())
	}
	return bytes
}

// Every separately allocated descriptor/member/path is rounded conservatively.
// Optional observation storage is bounded by its existing fixed capacity.
func (self *parsedPacketGroup) collapsePromotionByteCount() ByteCount {
	round := func(bytes ByteCount) ByteCount { return (bytes + 63) / 64 * 64 }
	bytes := round(ByteCount(unsafe.Sizeof(parsedPacketGroup{})))
	bytes = addReceiveQueueByteCount(bytes, round(ByteCount(cap(self.packets))*ByteCount(unsafe.Sizeof(parsedPacket{}))))
	for index := range self.packets {
		packet := &self.packets[index]
		path := packet.ipPath
		bytes = addReceiveQueueByteCount(bytes, round(ByteCount(unsafe.Sizeof(IpPath{}))))
		bytes = addReceiveQueueByteCount(bytes, round(ByteCount(cap(path.SourceIp)+cap(path.DestinationIp))))
		bytes = addReceiveQueueByteCount(bytes, round(ByteCount(len(path.ServerName)+len(packet.pin.appId))))
		if packet.transportAttribution != nil {
			bytes = addReceiveQueueByteCount(bytes, round(ByteCount(unsafe.Sizeof(transportPacketAttribution{}))))
		}
	}
	if self.transportAttribution != nil {
		bytes = addReceiveQueueByteCount(bytes, round(ByteCount(unsafe.Sizeof(transportPacketAttribution{}))))
	}
	if self.admissionObservations != nil {
		bytes = addReceiveQueueByteCount(bytes, round(ByteCount(unsafe.Sizeof(sendPackAdmissionObservations{}))))
		bytes = addReceiveQueueByteCount(bytes, round(sendPackAdmissionObservationCapacity*
			ByteCount(unsafe.Sizeof(pendingSendPackAdmissionObservation{})+unsafe.Sizeof(pendingNoAckAdmissionObservation{}))))
	}
	return bytes
}

// The serializer has already reserved this charge jointly with its item.
// Splitting it changes ownership, never the parent's total or availability.
func (self *parsedPacketGroup) retainGroupMemory(budget *TransferMemoryBudget, bytes ByteCount) {
	if self.completionFlags.Load()&groupCompletionTracked == 0 || self.collapseOwner != nil {
		return
	}
	owner := &tcpCollapseGroup{remaining: len(self.packets), budget: budget, budgetBytes: bytes}
	if len(self.packets) > 64 {
		words := 2 * ((len(self.packets) - 1) / 64)
		capacity := 2
		for capacity < words {
			capacity *= 2
		}
		owner.stateWords = make([]uint64, words, capacity)
	}
	update := self.collapseAdmission.update
	update.stateLock.Lock()
	owner.published = self.collapseStartWithLock()
	owner.sourceId = self.collapseSource.sequenceId
	self.collapseOwner = owner
	update.stateLock.Unlock()
}

// Promotion retains the source's numeric identity, never its worker graph.
func (self *parsedPacketGroup) collapseSourceIdWithLock() Id {
	if self.collapseSource != nil {
		return self.collapseSource.sequenceId
	}
	if self.collapseOwner != nil {
		return self.collapseOwner.sourceId
	}
	return Id{}
}

// A descriptor can span several generations. Its final accepted control
// starts both raw and durable proof, including when no range owner exists.
func (self *parsedPacketGroup) collapseStartWithLock() int {
	if self.completionFlags.Load()&groupCompletionDequeued == 0 {
		return 0
	}
	for index := range self.packets {
		if self.packets[index].collapseAdmission.epoch == self.collapseAdmission.epoch {
			return index
		}
	}
	return len(self.packets)
}

// Returns one original's exact state; only the flow lock reads these bits.
func (self *tcpCollapseGroup) stateWithLock(index int) (*uint64, *uint64, uint64) {
	if index < 64 {
		return &self.materialized, &self.disposed, uint64(1) << uint(index)
	}
	block := (index/64 - 1) * 2
	return &self.stateWords[block], &self.stateWords[block+1], uint64(1) << uint(index%64)
}

// Claims are descriptor links, not an additional raw queue or bitmap. Their
// list order is never used as evidence of a producer's enqueue order.
func (self *parsedPacketGroup) linkCollapseWithLock() {
	flags := self.completionFlags.Load()
	if flags&(groupCompletionLinked|groupCompletionDisposed|groupCompletionDone|groupCompletionAbandoned) != 0 {
		return
	}
	update := self.collapseAdmission.update
	if update.sequenceClaimsTail == nil {
		update.sequenceClaims = self
	} else {
		update.sequenceClaimsTail.collapseNext = self
	}
	update.sequenceClaimsTail = self
	self.completionFlags.Or(groupCompletionLinked)
}

// Unlinks this admission without subtracting overlapping durable coverage.
func (self *parsedPacketGroup) unlinkCollapseWithLock() {
	if self.completionFlags.Load()&groupCompletionLinked == 0 {
		return
	}
	update := self.collapseAdmission.update
	var previous *parsedPacketGroup
	for link := &update.sequenceClaims; *link != nil; link = &(*link).collapseNext {
		if *link == self {
			*link = self.collapseNext
			if update.sequenceClaimsTail == self {
				update.sequenceClaimsTail = previous
			}
			break
		}
		previous = *link
	}
	self.collapseNext = nil
	self.completionFlags.And(^groupCompletionLinked)
	if self.completionFlags.Load()&(groupCompletionTerminal|groupCompletionReturned) == groupCompletionTerminal|groupCompletionReturned {
		self.collapseNext = update.sequenceReleasedClaims.Load()
		update.sequenceReleasedClaims.Store(self)
	}
}

// Detached terminal descriptors need no new release owner. Drain their existing
// links outside the flow lock so budget notification never nests underneath it.
func (self *multiClientChannelUpdate) releaseCollapseClaims() {
	if self.sequenceReleasedClaims.Load() == nil && !self.sequenceCloseReady.Load() {
		return
	}
	self.stateLock.Lock()
	var parent *RemoteUserNatMultiClient
	if self.sequenceCloseReady.Load() && self.sequenceSynOffers == 0 && !self.pendingCollapseCloseWithLock() &&
		self.sequenceCloseReady.Swap(false) && self.cancel != nil && !self.IsDone() {
		// Serialize retirement with a later source SYN reset. Notification is
		// outside this lock; no callback or joining operation runs here.
		self.cancel()
		parent = self.sequenceParent
		self.clearRaceWithLock()
		self.abandonCollapseClaimsWithLock()
	}
	groups := self.sequenceReleasedClaims.Swap(nil)
	self.stateLock.Unlock()
	if parent != nil {
		parent.notifyFlowReaper()
	}
	for groups != nil {
		group := groups
		groups, group.collapseNext = group.collapseNext, nil
		group.releaseCollapseMemory()
	}
}

// Source replay, not producer return order, decides whether a pending reset
// closes or starts another cohort. Refused offers never enter this list.
func (self *multiClientChannelUpdate) pendingCollapseCloseWithLock() bool {
	client := self.client.Load()
	for group := self.sequenceClaims; group != nil; group = group.collapseNext {
		if group.completionFlags.Load()&(groupCompletionDequeued|groupCompletionAbandoned) != 0 ||
			client != nil && group.completionClient != client ||
			client == nil && !group.awaitingCollapsePromotionWithLock() {
			continue
		}
		for index := range group.packets {
			if path := group.packets[index].ipPath; path.Syn || path.Rst {
				return true
			}
		}
	}
	return false
}

// Race termination revokes promotion, not any retained source item. Its target
// still releases range charge after callback and public return have both ended.
func (self *multiClientChannelUpdate) abandonCollapseClaimsWithLock() {
	for group := self.sequenceClaims; group != nil; {
		next := group.collapseNext
		if self.IsDone() || group.completionClient != self.client.Load() {
			group.completionFlags.Or(groupCompletionAbandoned)
			group.unlinkCollapseWithLock()
		}
		group = next
	}
}

// Successful channel handoff and source dequeue may race. Either proves
// admission; exactly one records performance/hold time, before serialization.
func (self *parsedPacketGroup) sendGroupAdmitted(source *SendSequence) {
	if self.completionFlags.Load()&groupCompletionTracked == 0 {
		return
	}
	update := self.collapseAdmission.update
	defer update.releaseCollapseClaims()
	update.stateLock.Lock()
	defer update.stateLock.Unlock()
	self.admitCollapseWithLock(source, false)
}

// Admission accounting is independent of coverage and source control order.
func (self *parsedPacketGroup) admitCollapseWithLock(source *SendSequence, sourceOrdered bool) {
	if self.completionFlags.Load()&groupCompletionAdmitted != 0 {
		return
	}
	self.completionFlags.Or(groupCompletionAdmitted)
	self.collapseSource = source
	self.admissionTime = time.Now()
	self.validateCollapseCandidateWithLock()
	if self.completionFlags.Load()&groupCompletionAbandoned != 0 {
		return
	}
	self.linkCollapseWithLock()
	update := self.collapseAdmission.update
	if self.completionClient != update.client.Load() || update.IsDone() || self.completionClient.IsDone() {
		return
	}
	self.observeCollapseAdmissionWithLock(sourceOrdered)
}

// A selected winner can adopt an admission after it was made. Metrics have
// their own SYN generation because a queued SYN need not be dequeued yet.
func (self *parsedPacketGroup) observeCollapseAdmissionWithLock(sourceOrdered bool) {
	update := self.collapseAdmission.update
	if self.completionFlags.Or(groupCompletionObserved)&groupCompletionObserved != 0 {
		return
	}
	for index := range self.packets {
		path := self.packets[index].ipPath
		if path.Syn && (!update.sequenceMetricSynSeen || update.sequenceMetricSynNumber != path.SequenceNumber) {
			update.ackPerformance.reset()
			update.sequenceMetricSynSeen, update.sequenceMetricSynNumber = true, path.SequenceNumber
		}
		if path.Protocol == IpProtocolTcp {
			update.ackPerformance.observe(self.admissionTime, path.Ack, path.AckSequenceNumber)
		}
		if !sourceOrdered && self.collapseOrder() == 0 {
			update.sequenceAdmissionAck = path.AckSequenceNumber
			update.sequenceAdmissionWindow = path.TcpWindowSize
			update.sequenceAdmissionStateSeen = true
		}
	}
	if admittedAt := self.admissionTime; update.sequenceTime.Before(admittedAt) {
		update.sequenceTime = admittedAt
	}
}

// The sole source consumer supplies FIFO, including an unbuffered handoff.
// An offer's eventual public return cannot change this order.
func (self *parsedPacketGroup) sendGroupDequeued(source *SendSequence) {
	if self.completionFlags.Load()&groupCompletionTracked == 0 {
		return
	}
	update := self.collapseAdmission.update
	defer update.releaseCollapseClaims()
	update.stateLock.Lock()
	defer update.stateLock.Unlock()
	if self.completionFlags.Or(groupCompletionReceived)&groupCompletionReceived != 0 {
		return
	}
	// A canceled receive still proves enqueue if its producer is paused.
	// Record only successful admission; never replay generation/close state.
	if source.ctx.Err() != nil {
		self.collapseSource = source
		following, ack, window, seen := self.followingCollapseStateWithLock()
		self.admitCollapseWithLock(source, false)
		if following && self.completionFlags.Load()&groupCompletionAbandoned == 0 &&
			self.completionClient == update.client.Load() && !update.IsDone() && !self.completionClient.IsDone() {
			update.sequenceAdmissionAck, update.sequenceAdmissionWindow = ack, window
			update.sequenceAdmissionStateSeen = seen
		}
		return
	}
	self.admitCollapseWithLock(source, true)
	self.validateCollapseCandidateWithLock()
	order := update.nextCollapseOrderWithLock()
	if order == 0 {
		self.completionFlags.Or(groupCompletionAbandoned)
		self.unlinkCollapseWithLock()
	}
	for {
		flags := self.completionFlags.Load()
		if self.completionFlags.CompareAndSwap(flags, flags&0xffff|groupCompletionDequeued|order<<16) {
			break
		}
	}
	if self.completionFlags.Load()&groupCompletionAbandoned != 0 || update.IsDone() || self.completionClient.IsDone() {
		return
	}
	if self.completionClient != update.client.Load() {
		if self.awaitingCollapsePromotionWithLock() {
			self.observeCandidateControlWithLock()
		}
		return
	}
	self.observeCollapseAdmissionWithLock(true)
	self.activateCollapseWithLock()
}

// Accepted controls outlive an unfunded raw descriptor on the existing bounded
// race/provider owner. No packet, descriptor, source worker or range is retained.
type tcpCandidateControlState struct {
	sourceId       Id
	order          uint64
	synOrder       uint64
	synIndex       int
	rstOrder       uint64
	synNumber      uint32
	sequenceNumber uint32
	ackNumber      uint32
	window         uint16
	synSeen        bool
	ambiguousSyn   bool
	controlSeen    bool
	closed         bool
	finSeen        bool
	ackSeen        bool
	finSequence    uint32
	ackSequence    uint32
	admittedAt     time.Time
	performance    tcpAckPerformance
}

// Exact race validation precedes this call. Only the candidate's newest source
// may change its semantic tail; failed admission and cleanup receives never do.
func (self *parsedPacketGroup) observeCandidateControlWithLock() {
	update := self.collapseAdmission.update
	state := &update.race.clientStates[self.completionClient].collapseControl
	sourceId := self.collapseSourceIdWithLock()
	if sourceId.LessThan(state.sourceId) || self.collapseOrder() <= state.order {
		return
	}
	if state.order == 0 {
		state.synSeen, state.synNumber = update.synGenerationSeen, update.synGenerationNumber
		state.finSeen, state.finSequence = update.egressFinSeen, update.egressFinSequence
		state.ackSeen, state.ackSequence = update.egressAckSeen, update.egressAckSequence
		state.performance = update.ackPerformance
	}
	state.sourceId, state.order = sourceId, self.collapseOrder()
	if state.admittedAt.Before(self.admissionTime) {
		state.admittedAt = self.admissionTime
	}
	changed := false
	for index := range self.packets {
		path := self.packets[index].ipPath
		if path.Rst {
			state.rstOrder, state.closed, state.controlSeen = state.order, true, true
			changed = true
		}
		if path.Syn && (!state.synSeen || state.synNumber != path.SequenceNumber) {
			// Once histories may diverge, equal ISNs cannot re-establish
			// cross-candidate lineage. This sticky bit cannot wrap.
			state.ambiguousSyn = state.ambiguousSyn || state.synOrder != 0
			state.synSeen, state.synNumber, state.synOrder = true, path.SequenceNumber, state.order
			state.synIndex = index
			state.closed, state.controlSeen = false, true
			state.finSeen, state.ackSeen = false, false
			state.finSequence, state.ackSequence = 0, 0
			state.performance.reset()
			changed = true
		}
		state.sequenceNumber, state.ackNumber, state.window = path.SequenceNumber, path.AckSequenceNumber, path.TcpWindowSize
		state.performance.observe(self.admissionTime, path.Ack, path.AckSequenceNumber)
		if path.Ack {
			updateLatestTcpSequence(&state.ackSeen, &state.ackSequence, path.AckSequenceNumber)
		}
		if path.Fin {
			updateLatestTcpSequence(&state.finSeen, &state.finSequence, path.SequenceNumber+uint32(path.TcpPayloadByteCount)+1)
		}
	}
	if changed {
		update.observeCandidateCloseWithLock()
	}
}

// A reset closes its own known cohort. Another candidate's genuinely newer
// open cohort fails open; a late copy of its identical SYN cannot undo a reset.
func (self *multiClientChannelUpdate) observeCandidateCloseWithLock() {
	if self.race == nil {
		return
	}
	closed := false
	for _, candidate := range self.race.clientStates {
		state := &candidate.collapseControl
		if !state.controlSeen {
			continue
		}
		if state.closed {
			closed = true
			continue
		}
		matched := false
		for _, other := range self.race.clientStates {
			reset := &other.collapseControl
			if reset.controlSeen && reset.closed && !state.ambiguousSyn && !reset.ambiguousSyn && state.synSeen == reset.synSeen &&
				(!state.synSeen || state.synNumber == reset.synNumber) {
				matched = true
				break
			}
		}
		if !matched {
			self.sequenceCloseReady.Store(false)
			return
		}
	}
	self.sequenceCloseReady.Store(closed)
}

// Applies source controls and reconciles previously admitted ACK progress.
// Publishing/teardown never invokes this transition.
func (self *parsedPacketGroup) activateCollapseWithLock() {
	update := self.collapseAdmission.update
	if self.completionFlags.Load()&groupCompletionAbandoned != 0 {
		return
	}
	clientChanged := update.sequenceClient != self.completionClient
	sourceId := self.collapseSourceIdWithLock()
	if !clientChanged && sourceId.LessThan(update.sequenceSourceId) {
		return
	}
	forceSynIndex := -1
	if race := update.race; race != nil {
		if candidate := race.clientStates[self.completionClient]; candidate != nil &&
			candidate.collapseControl.synOrder == self.collapseOrder() {
			forceSynIndex = candidate.collapseControl.synIndex
		}
	}
	if self.collapseAdmission.epoch != update.sequenceAdmissionEpoch {
		reset := forceSynIndex >= 0
		for index := range self.packets {
			path := self.packets[index].ipPath
			reset = reset || path.Rst || path.Syn && (!update.synGenerationSeen || update.synGenerationNumber != path.SequenceNumber)
		}
		if !reset && (sourceId != update.sequenceResetSourceId || update.sequenceResetOrder == 0 ||
			self.collapseOrder() <= update.sequenceResetOrder) {
			return
		}
		self.collapseAdmission.epoch = update.sequenceAdmissionEpoch
	}
	if !clientChanged && self.collapseOrder() <= update.sequenceControlOrder {
		return
	}
	following, acceptedAck, acceptedWindow, acceptedStateSeen := self.followingCollapseStateWithLock()
	update.sequenceSourceId = sourceId
	update.sequenceControlOrder = self.collapseOrder()
	if clientChanged {
		update.sequenceCovered = false
		update.sequenceAckPositionSeen = false
		update.sequenceSynSeen = false
		update.sequenceClient = self.completionClient
	}
	for index := range self.packets {
		packet := &self.packets[index]
		path := packet.ipPath
		forceSyn := path.Syn && index == forceSynIndex
		if path.Rst || forceSyn || path.Syn && (!update.synGenerationSeen || update.synGenerationNumber != path.SequenceNumber) {
			admittedAt := update.sequenceTime
			if forceSyn && update.synGenerationSeen && update.synGenerationNumber == path.SequenceNumber {
				// The candidate source proved a new cohort even when an
				// intervening unwritten SYN vanished before winner replay.
				update.resetSynGenerationWithLock(path.SequenceNumber)
			}
			update.resetSequenceWithLock(packet)
			update.sequenceTime = admittedAt
			self.collapseAdmission.epoch = update.sequenceAdmissionEpoch
			update.sequenceResetSourceId, update.sequenceResetOrder = sourceId, self.collapseOrder()
			update.sequenceSynSeen = false
			if owner := self.collapseOwner; owner != nil {
				owner.published = index
			}
			if observations := self.admissionObservations; observations != nil && path.Syn {
				proof := &observations.synAdmission
				if proof.update == update && proof.sequence == path.SequenceNumber && proof.responseClient == self.completionClient {
					update.receivedInbound.Store(true)
					update.synGenerationAwaiting = false
				}
			}
		}
		if path.Syn {
			update.sequenceMetricSynSeen, update.sequenceMetricSynNumber = true, path.SequenceNumber
		}
		// Source order reconciles a producer observation delayed after its
		// enqueue. ACK progress is monotonic, so an existing sample is not
		// counted twice; a new SYN deliberately resets its own metric cohort.
		update.ackPerformance.observe(self.admissionTime, path.Ack, path.AckSequenceNumber)
		update.sequenceAdmissionAck = path.AckSequenceNumber
		update.sequenceAdmissionWindow = path.TcpWindowSize
		update.sequenceAdmissionStateSeen = true
		packet.collapseAdmission.epoch = update.sequenceAdmissionEpoch
		if update.observeTcpControlWithLock(tcpControlFromIpPath(path), false) {
			update.sequenceCloseReady.Store(true)
		}
	}
	if following {
		update.sequenceAdmissionAck, update.sequenceAdmissionWindow = acceptedAck, acceptedWindow
		update.sequenceAdmissionStateSeen = acceptedStateSeen
	}
	self.publishCollapseWithLock()
}

// Every other raw claim on this same source follows the current dequeue.
// Keep its accepted control available for teardown even if it is later disposed.
// Conflicting raw controls retain the producer observation until FIFO resolves it.
func (self *parsedPacketGroup) followingCollapseStateWithLock() (bool, uint32, uint16, bool) {
	update := self.collapseAdmission.update
	found, coherent := false, true
	var ack uint32
	var window uint16
	for group := update.sequenceClaims; group != nil; group = group.collapseNext {
		if group == self || group.completionClient != self.completionClient ||
			group.completionFlags.Load()&(groupCompletionReceived|groupCompletionAbandoned) != 0 ||
			group.collapseSourceIdWithLock() != self.collapseSourceIdWithLock() {
			continue
		}
		path := group.packets[len(group.packets)-1].ipPath
		if found && (ack != path.AckSequenceNumber || window != path.TcpWindowSize) {
			coherent = false
		}
		found, ack, window = true, path.AckSequenceNumber, path.TcpWindowSize
	}
	if found && coherent {
		return true, ack, window, true
	}
	return found, update.sequenceAdmissionAck, update.sequenceAdmissionWindow, update.sequenceAdmissionStateSeen
}

// Before Run, a competing accepted SYN has no source-owned generation yet.
// Old ingress close edges must not retire it against the prior FIN cohort.
func (self *multiClientChannelUpdate) pendingCollapseResetWithLock() bool {
	for group := self.sequenceClaims; group != nil; group = group.collapseNext {
		if group.completionClient != self.client.Load() || group.collapseOrder() != 0 {
			continue
		}
		for index := range group.packets {
			path := group.packets[index].ipPath
			if path.Rst || path.Syn && (!self.synGenerationSeen || self.synGenerationNumber != path.SequenceNumber) {
				return true
			}
		}
	}
	return false
}

// A processed source cursor is never proof. Only the materialized bits may
// enter durable coverage; controls and admission clocks were already recorded.
func (self *parsedPacketGroup) publishCollapseWithLock() {
	owner := self.collapseOwner
	update := self.collapseAdmission.update
	if owner == nil || self.completionFlags.Load()&groupCompletionDequeued == 0 ||
		self.completionFlags.Load()&groupCompletionAbandoned != 0 ||
		self.collapseAdmission.epoch != update.sequenceAdmissionEpoch || self.completionClient != update.client.Load() ||
		update.IsDone() || self.completionClient.IsDone() {
		return
	}
	for owner.published < len(self.packets) {
		index := owner.published
		materialized, disposed, bit := owner.stateWithLock(index)
		if (*materialized|*disposed)&bit == 0 {
			break
		}
		owner.published++
		if *materialized&bit != 0 {
			packet := &self.packets[index]
			if packet.ipPath.Syn {
				update.sequenceSynSeen, update.sequenceSynNumber = true, packet.ipPath.SequenceNumber
			}
			update.updateMaterializedSequenceWithLock(packet, packet.ipPath.TcpPayloadByteCount)
		}
	}
	if owner.remaining == 0 {
		self.unlinkCollapseWithLock()
	}
}

// Disposition precedes original pool return and follows successful retention.
// An unfunded first chunk disposes the raw group before its source waits.
func (self *parsedPacketGroup) sendGroupDisposition(start, end int, materialized bool) {
	if self.completionFlags.Load()&groupCompletionTracked == 0 {
		return
	}
	update := self.collapseAdmission.update
	defer update.releaseCollapseClaims()
	update.stateLock.Lock()
	defer update.stateLock.Unlock()
	owner := self.collapseOwner
	if owner == nil {
		if materialized {
			panic("materialized group without retained range ownership")
		}
		self.completionFlags.Or(groupCompletionDisposed)
		self.unlinkCollapseWithLock()
		return
	}
	for index := start; index < end; index++ {
		written, disposed, bit := owner.stateWithLock(index)
		if (*written|*disposed)&bit != 0 {
			continue
		}
		if materialized {
			*written |= bit
		} else {
			*disposed |= bit
		}
		owner.remaining--
	}
	self.publishCollapseWithLock()
	if owner.remaining == 0 && self.completionClient == update.client.Load() {
		self.unlinkCollapseWithLock()
	}
}

// The terminal callback preserves provider accounting and completion once.
// A range error cannot erase a successfully retained prefix.
func (self *parsedPacketGroup) sendAckResult(_ ByteCount, err error) {
	if self.completionFlags.Or(groupCompletionDone)&groupCompletionDone != 0 {
		return
	}
	defer func() {
		self.completionFlags.Or(groupCompletionTerminal)
		self.releaseCollapseMemory()
	}()
	if self.completionFlags.Load()&groupCompletionTracked != 0 {
		update := self.collapseAdmission.update
		update.stateLock.Lock()
		self.publishCollapseWithLock()
		if self.collapseOwner == nil || !self.awaitingCollapsePromotionWithLock() {
			self.unlinkCollapseWithLock()
		}
		update.stateLock.Unlock()
	} else {
		self.collapseAdmission.complete(err)
	}
	self.completionClient.observePacketGroupTransferCompletion(self, self.completionFlags.Load()&groupCompletionAck != 0, err)
}

// This defer is installed only after the stalled fast path. The caller still
// owns refused packet buffers, and no callback or source reads their contents.
func (self *parsedPacketGroup) finishGroupOffer() {
	self.completionFlags.Or(groupCompletionReturned)
	self.releaseCollapseMemory()
}

// The extra charge survives both the complete callback and the public offer.
// Budget release happens outside the flow lock and can occur exactly once.
func (self *parsedPacketGroup) releaseCollapseMemory() {
	flags := self.completionFlags.Load()
	if flags&(groupCompletionReturned|groupCompletionTerminal|groupCompletionTracked) !=
		groupCompletionReturned|groupCompletionTerminal|groupCompletionTracked {
		return
	}
	update := self.collapseAdmission.update
	update.stateLock.Lock()
	self.collapseSource = nil
	if self.completionFlags.Load()&groupCompletionLinked != 0 {
		// No owner may read these roots after both ownership boundaries.
		// Existing parsed scalar metadata is sufficient for later promotion.
		for index := range self.packets {
			self.packets[index].packet = nil
			self.packets[index].payload = nil
		}
		update.stateLock.Unlock()
		return
	}
	var budget *TransferMemoryBudget
	var bytes ByteCount
	if owner := self.collapseOwner; owner != nil {
		budget, bytes = owner.budget, owner.budgetBytes
		owner.budget, owner.budgetBytes = nil, 0
	}
	update.stateLock.Unlock()
	if budget != nil {
		budget.Release(bytes)
	}
}

// Only dequeued claims have source order. Winner promotion replays them in
// that order, without using the possibly inverted order of public returns.
func (self *multiClientChannelUpdate) commitCollapseClaimsWithLock(client *multiClientChannel) {
	pendingClose := self.sequenceCloseReady.Load()
	var state *tcpCandidateControlState
	if self.race != nil && self.race.clientStates[client] != nil {
		state = &self.race.clientStates[client].collapseControl
	}
	synPending, rstPending := state != nil && state.synOrder != 0, state != nil && state.closed && state.rstOrder != 0
	for group := self.sequenceClaims; group != nil; group = group.collapseNext {
		if group.completionClient == client {
			group.observeCollapseAdmissionWithLock(group.collapseOrder() != 0)
		}
	}
	order := uint64(0)
	for {
		var next *parsedPacketGroup
		for group := self.sequenceClaims; group != nil; group = group.collapseNext {
			if group.completionClient == client && group.collapseOrder() > order &&
				(next == nil || group.collapseOrder() < next.collapseOrder()) {
				next = group
			}
		}
		if next == nil {
			break
		}
		order = next.collapseOrder()
		if synPending && state.synOrder < order {
			self.resetCandidateControlWithLock(client, state, true)
			synPending = false
		}
		if rstPending && state.rstOrder < order {
			self.resetCandidateControlWithLock(client, state, false)
			rstPending = false
		}
		next.observeCollapseAdmissionWithLock(true)
		next.activateCollapseWithLock()
		// An existing descriptor already replays every member in source order,
		// including opposite SYN/RST orders sharing one group ordinal.
		if state != nil {
			synPending = synPending && state.synOrder != order
			rstPending = rstPending && state.rstOrder != order
		}
		if next.completionFlags.Load()&groupCompletionDone != 0 {
			next.unlinkCollapseWithLock()
		}
	}
	if synPending {
		self.resetCandidateControlWithLock(client, state, true)
	}
	if rstPending {
		self.resetCandidateControlWithLock(client, state, false)
	}
	if state == nil || state.order == 0 {
		return
	}
	// Semantic admission is not retained proof. Preserve later raw admissions
	// until their own source replay resolves any unknown producer ordering.
	rawFollowing := false
	for group := self.sequenceClaims; group != nil; group = group.collapseNext {
		if group.completionClient == client && group.collapseSourceIdWithLock() == state.sourceId &&
			group.completionFlags.Load()&(groupCompletionReceived|groupCompletionAbandoned) == 0 {
			rawFollowing = true
			break
		}
	}
	if self.sequenceClient != client {
		self.sequenceCovered, self.sequenceAckPositionSeen, self.sequenceSynSeen = false, false, false
	}
	self.sequenceClient, self.sequenceSourceId, self.sequenceControlOrder = client, state.sourceId, state.order
	self.sequenceMetricSynSeen, self.sequenceMetricSynNumber = state.synSeen, state.synNumber
	if !rawFollowing {
		self.sequenceAdmissionAck, self.sequenceAdmissionWindow, self.sequenceAdmissionStateSeen = state.ackNumber, state.window, true
		self.ackPerformance = state.performance
	}
	if self.sequenceTime.Before(state.admittedAt) {
		self.sequenceTime = state.admittedAt
	}
	self.egressFinSeen, self.egressFinSequence = state.finSeen, state.finSequence
	self.egressAckSeen, self.egressAckSequence = state.ackSeen, state.ackSequence
	self.sequenceCloseReady.Store(pendingClose || state.closed || self.observeTcpControlWithLock(tcpControlObservation{valid: true}, false))
}

// A source-dequeued but wholly unwritten reset has no retained descriptor.
// Reconcile its numeric semantic boundary without fabricating a range or SYN bit.
func (self *multiClientChannelUpdate) resetCandidateControlWithLock(client *multiClientChannel, state *tcpCandidateControlState, syn bool) {
	admittedAt := self.sequenceTime
	if admittedAt.Before(state.admittedAt) {
		admittedAt = state.admittedAt
	}
	path := IpPath{Protocol: IpProtocolTcp, SequenceNumber: state.sequenceNumber,
		AckSequenceNumber: state.ackNumber, TcpWindowSize: state.window, Syn: syn, Rst: !syn}
	order := state.rstOrder
	if syn {
		path.SequenceNumber, order = state.synNumber, state.synOrder
		self.resetSynGenerationWithLock(state.synNumber)
	}
	self.resetSequenceWithLock(&parsedPacket{ipPath: &path})
	self.sequenceTime = admittedAt
	self.sequenceSynSeen = false
	self.sequenceClient, self.sequenceSourceId, self.sequenceControlOrder = client, state.sourceId, order
	self.sequenceResetSourceId, self.sequenceResetOrder = state.sourceId, order
	self.sequenceCloseReady.Store(!syn)
}

// Unknown/conflicting controls fail open. A known exact pending original can
// suppress a duplicate, but never an intervening zero-window/ACK transition.
func (self *multiClientChannelUpdate) pendingSequenceCoversWithLock(packet *parsedPacket, client *multiClientChannel) bool {
	path := packet.ipPath
	covered := false
	for group := self.sequenceClaims; group != nil; group = group.collapseNext {
		if group.completionClient != client || group.collapseAdmission.epoch != self.sequenceAdmissionEpoch {
			continue
		}
		start := group.collapseStartWithLock()
		if group.collapseOrder() == 0 {
			for index := range group.packets {
				if prior := group.packets[index].ipPath; prior.Rst ||
					prior.Syn && (!path.Syn || prior.SequenceNumber != path.SequenceNumber) {
					return false
				}
			}
		}
		for index := start; index < len(group.packets); index++ {
			if owner := group.collapseOwner; owner != nil {
				_, disposed, bit := owner.stateWithLock(index)
				if *disposed&bit != 0 {
					continue
				}
			}
			original := &group.packets[index]
			prior := original.ipPath
			if path.Syn {
				if !prior.Syn || path.SequenceNumber != prior.SequenceNumber {
					continue
				}
				if len(packet.payload) == 0 {
					covered = true
					continue
				}
			}
			if prior.AckSequenceNumber != path.AckSequenceNumber || prior.TcpWindowSize != path.TcpWindowSize {
				if group.collapseOrder() == 0 {
					return false
				}
				continue
			}
			start := path.SequenceNumber
			end := tcpPacketNextSequenceNumber(path, packet.payload)
			originalEnd := tcpPacketNextSequenceNumberWithPayloadByteCount(prior, prior.TcpPayloadByteCount)
			if int32(end-start) >= 0 && int32(start-prior.SequenceNumber) >= 0 && int32(originalEnd-end) >= 0 {
				covered = true
			}
		}
	}
	return covered
}
