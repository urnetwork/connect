package connect

import (
	"context"
	"slices"
	"sync"
)

// PlatformTransportBudget bounds the aggregate retained capacity of platform
// carriers and their logical carrier-graph slots. A slot can own multiple
// physical sockets, whose retained buffers are covered by byte claims. H1
// claims register before transport goroutines start and precede optional Auto
// H3 when both do not fit. Optional H3 leases are revocable: an explicit H3
// choice can reclaim one, and foreground client Auto H3 can reclaim a provider
// lease, so construction order cannot permanently pin outbound traffic to H1.
// A policy replacement with H1 on either side may also use one serialized
// temporary handoff per limit bounded to the H1 claim, preserving make-before-break
// without allowing two full H3 working sets to escape the aggregate limit.
type PlatformTransportBudget struct {
	mutex sync.Mutex
	// The hierarchy is immutable after construction. Every budget and every
	// reservation in it is guarded by root.mutex, so a claim either acquires
	// all its limits or none. Children share the root's change notification;
	// no forwarding goroutines or capacity held while waiting are needed.
	parent *PlatformTransportBudget
	root   *PlatformTransportBudget

	totalByteCount     ByteCount
	usedByteCount      ByteCount
	maxTransportCount  int
	usedTransportCount int
	pendingH1ByteCount ByteCount
	pendingH1SlotCount int
	notify             chan struct{}
	reservedByteCount  ByteCount
	releasedByteCount  ByteCount
	preemptedH3Count   uint64
	// activeHandoff is the only reservation allowed to exceed the ordinary
	// byte/carrier-slot ceiling while its paired carrier remains live. A handoff is
	// allowed only when one endpoint is H1, so the temporary overage is bounded
	// by the H1 claim: old H1 while moving to H3, or new H1 while moving from H3.
	// More than one replacement may wait, but admission serializes the overage.
	activeHandoff           *platformTransportBudgetReservation
	handoffAcquisitionCount uint64
	reservations            map[*platformTransportBudgetReservation]bool
	nextSequence            uint64
}

type platformTransportBudgetClass uint8

const (
	platformTransportBudgetH1 platformTransportBudgetClass = iota + 1
	platformTransportBudgetH3Auto
	platformTransportBudgetH3Explicit
	platformTransportBudgetExtender

	// Keep the old internal name for tests and callers that mean optional H3.
	platformTransportBudgetH3 = platformTransportBudgetH3Auto
)

const (
	// Foreground is the default for outbound client windows. A larger value is
	// lower priority and may yield an optional Auto-H3 lease to a foreground
	// claimant when both cannot fit.
	PlatformTransportBudgetPriorityForeground = 0
	PlatformTransportBudgetPriorityBackground = 1
)

type platformTransportBudgetReservation struct {
	budget    *PlatformTransportBudget
	parent    *platformTransportBudgetReservation
	owner     *platformTransportBudgetReservation
	class     platformTransportBudgetClass
	byteCount ByteCount
	usesSlot  bool
	priority  int
	sequence  uint64

	// Lifecycle and preemption state are guarded by budget.root.mutex. Keeping one
	// lock lets admission inspect and revoke another reservation without a
	// reservation-to-budget / budget-to-reservation lock inversion.
	pending          bool
	acquired         bool
	closed           bool
	preempt          chan struct{}
	preemptRequested bool
	// A policy replacement may pair with the claim it will replace when one side
	// is H1. The pair remains linked after admission until either transport
	// releases; that is the accounting proof that the temporary overage is
	// bounded by the H1 reservation rather than a second full H3 working set.
	handoffFrom *platformTransportBudgetReservation
	handoffTo   *platformTransportBudgetReservation
}

type PlatformTransportBudgetStats struct {
	TotalByteCount              ByteCount
	UsedByteCount               ByteCount
	MaxTransportCount           int
	UsedTransportCount          int
	PendingH1ByteCount          ByteCount
	PendingH1Count              int
	ReservedByteCount           ByteCount
	ReleasedByteCount           ByteCount
	PreemptedH3Count            uint64
	PendingHandoffCount         int
	ActiveHandoffCount          int
	ActiveHandoffByteCount      ByteCount
	ActiveHandoffTransportCount int
	ActiveHandoffID             uint64
	ActiveHandoffFromClass      string
	ActiveHandoffToClass        string
	// H1 endpoint bytes, or the smaller endpoint when both carriers are H1.
	ActiveHandoffH1ByteCount ByteCount
	HandoffAcquisitionCount  uint64
}

// PlatformTransportBudgetHandoffStats identifies the live replacement pair,
// independently of which limit required its temporary loan. Owner is
// "device", "other_device", or "process", relative to the sampled budget.
type PlatformTransportBudgetHandoffStats struct {
	ID             uint64
	FromClass      string
	ToClass        string
	H1ByteCount    ByteCount
	ByteCount      ByteCount
	TransportCount int
	Owner          string
}

type PlatformTransportBudgetHierarchyStats struct {
	Budget        PlatformTransportBudgetStats
	Root          PlatformTransportBudgetStats
	BudgetHandoff PlatformTransportBudgetHandoffStats
	RootHandoff   PlatformTransportBudgetHandoffStats
	// Each level may lend to one pair independently. When two distinct loans
	// overlap, retain the other level's pair as provenance too, without adding
	// its overlap to this level's permitted overage. An ownerless root pair is
	// never projected into a device that does not own it.
	BudgetAdditionalHandoff PlatformTransportBudgetHandoffStats
	RootAdditionalHandoff   PlatformTransportBudgetHandoffStats
}

// NewPlatformTransportBudget creates a byte budget with an optional aggregate
// PlatformTransport count cap. maxTransportCount <= 0 disables the count cap.
func NewPlatformTransportBudget(
	totalByteCount ByteCount,
	maxTransportCount int,
) *PlatformTransportBudget {
	return newPlatformTransportBudget(totalByteCount, maxTransportCount, nil)
}

// newPlatformTransportBudget adds a local limit to an existing hierarchy.
// Parent claims retain the child's class, priority, and carrier-slot charge so
// direct process claims compete with device-owned carriers on equal terms.
func newPlatformTransportBudget(
	totalByteCount ByteCount,
	maxTransportCount int,
	parent *PlatformTransportBudget,
) *PlatformTransportBudget {
	budget := &PlatformTransportBudget{
		totalByteCount:    max(0, totalByteCount),
		maxTransportCount: max(0, maxTransportCount),
		reservations:      map[*platformTransportBudgetReservation]bool{},
		parent:            parent,
	}
	budget.root = budget
	if parent != nil {
		budget.root = parent.root
	} else {
		budget.notify = make(chan struct{})
	}
	return budget
}

func (self *PlatformTransportBudget) Stats() PlatformTransportBudgetStats {
	if self == nil {
		return PlatformTransportBudgetStats{}
	}
	self.root.mutex.Lock()
	defer self.root.mutex.Unlock()
	return self.statsLocked()
}

// StatsWithRoot samples both limits and their pair provenance under one lock.
// A root-only loan owned by this device still reports the device's pair even
// when its local limit needed no loan; child-only loans likewise expose the
// same pair at the root without claiming the root borrowed capacity.
func (self *PlatformTransportBudget) StatsWithRoot() PlatformTransportBudgetHierarchyStats {
	if self == nil {
		return PlatformTransportBudgetHierarchyStats{}
	}
	self.root.mutex.Lock()
	defer self.root.mutex.Unlock()
	snapshot := PlatformTransportBudgetHierarchyStats{
		Budget: self.statsLocked(),
		Root:   self.root.statsLocked(),
	}
	rootPair, devicePair := self.root.activeHandoff, self.activeHandoff
	if rootPair == nil {
		rootPair = devicePair
	}
	if devicePair == nil && rootPair != nil && rootPair.ownedByLocked(self) {
		devicePair = rootPair
	}
	snapshot.BudgetHandoff = devicePair.handoffStatsLocked(self)
	snapshot.RootHandoff = rootPair.handoffStatsLocked(self)
	if rootPair != nil && devicePair != nil && rootPair.owner != devicePair.owner {
		snapshot.RootAdditionalHandoff = devicePair.handoffStatsLocked(self)
		if rootPair.ownedByLocked(self) {
			snapshot.BudgetAdditionalHandoff = rootPair.handoffStatsLocked(self)
		}
	}
	return snapshot
}

func (self *platformTransportBudgetReservation) ownedByLocked(budget *PlatformTransportBudget) bool {
	for owner := self.owner.budget; owner != nil; owner = owner.parent {
		if owner == budget {
			return true
		}
	}
	return false
}

func (self platformTransportBudgetClass) name() string {
	switch self {
	case platformTransportBudgetH1:
		return "h1"
	case platformTransportBudgetH3Auto:
		return "h3_auto"
	case platformTransportBudgetH3Explicit:
		return "h3_explicit"
	case platformTransportBudgetExtender:
		return "extender"
	default:
		return ""
	}
}

func (self *platformTransportBudgetReservation) handoffStatsLocked(
	budget *PlatformTransportBudget,
) PlatformTransportBudgetHandoffStats {
	if self == nil || self.handoffFrom == nil {
		return PlatformTransportBudgetHandoffStats{}
	}
	previous := self.handoffFrom
	rootClaim := self
	for rootClaim.parent != nil {
		rootClaim = rootClaim.parent
	}
	stats := PlatformTransportBudgetHandoffStats{
		ID:        rootClaim.sequence,
		FromClass: previous.class.name(),
		ToClass:   self.class.name(),
		ByteCount: min(previous.byteCount, self.byteCount),
		Owner:     "other_device",
	}
	if previous.class == platformTransportBudgetH1 {
		stats.H1ByteCount = previous.byteCount
	}
	if self.class == platformTransportBudgetH1 {
		stats.H1ByteCount = self.byteCount
		if previous.class == platformTransportBudgetH1 {
			stats.H1ByteCount = min(previous.byteCount, self.byteCount)
		}
	}
	if self.usesSlot && previous.usesSlot {
		stats.TransportCount = 1
	}
	if self.owner.budget == self.budget.root {
		stats.Owner = "process"
	} else if self.ownedByLocked(budget) {
		stats.Owner = "device"
	}
	return stats
}

func (self *PlatformTransportBudget) statsLocked() PlatformTransportBudgetStats {
	stats := PlatformTransportBudgetStats{
		TotalByteCount:          self.totalByteCount,
		UsedByteCount:           self.usedByteCount,
		MaxTransportCount:       self.maxTransportCount,
		UsedTransportCount:      self.usedTransportCount,
		PendingH1ByteCount:      self.pendingH1ByteCount,
		PendingH1Count:          self.pendingH1SlotCount,
		ReservedByteCount:       self.reservedByteCount,
		ReleasedByteCount:       self.releasedByteCount,
		PreemptedH3Count:        self.preemptedH3Count,
		HandoffAcquisitionCount: self.handoffAcquisitionCount,
	}
	for reservation := range self.reservations {
		if reservation.handoffFrom != nil && reservation.pending && !reservation.closed {
			stats.PendingHandoffCount += 1
		}
	}
	if active := self.activeHandoff; active != nil && active.handoffFrom != nil {
		pair := active.handoffStatsLocked(self)
		stats.ActiveHandoffCount = 1
		stats.ActiveHandoffByteCount = pair.ByteCount
		stats.ActiveHandoffTransportCount = pair.TransportCount
		stats.ActiveHandoffID = pair.ID
		stats.ActiveHandoffFromClass = pair.FromClass
		stats.ActiveHandoffToClass = pair.ToClass
		stats.ActiveHandoffH1ByteCount = pair.H1ByteCount
	}
	return stats
}

func (self *PlatformTransportBudget) notifyChangedLocked() {
	close(self.root.notify)
	self.root.notify = make(chan struct{})
}

// CapacityNotify closes after any claim changes within this hierarchy. Read
// it before testing admission to avoid missing a release by a sibling device
// or by a process-owned carrier between the failed attempt and the wait.
func (self *PlatformTransportBudget) CapacityNotify() <-chan struct{} {
	if self == nil {
		return nil
	}
	self.root.mutex.Lock()
	defer self.root.mutex.Unlock()
	return self.root.notify
}

func (self *PlatformTransportBudget) register(
	class platformTransportBudgetClass,
	byteCount ByteCount,
	usesSlot bool,
) *platformTransportBudgetReservation {
	return self.registerWithPriority(
		class,
		byteCount,
		usesSlot,
		PlatformTransportBudgetPriorityForeground,
	)
}

func (self *PlatformTransportBudget) registerWithPriority(
	class platformTransportBudgetClass,
	byteCount ByteCount,
	usesSlot bool,
	priority int,
) *platformTransportBudgetReservation {
	if self == nil {
		return nil
	}
	self.root.mutex.Lock()
	defer self.root.mutex.Unlock()
	reservation := self.registerLocked(class, byteCount, usesSlot, priority, nil)
	self.notifyChangedLocked()
	return reservation
}

func (self *PlatformTransportBudget) registerLocked(
	class platformTransportBudgetClass,
	byteCount ByteCount,
	usesSlot bool,
	priority int,
	owner *platformTransportBudgetReservation,
) *platformTransportBudgetReservation {
	self.nextSequence += 1
	reservation := &platformTransportBudgetReservation{
		budget:    self,
		class:     class,
		byteCount: max(0, byteCount),
		usesSlot:  usesSlot,
		priority:  priority,
		sequence:  self.nextSequence,
		pending:   true,
	}
	if owner == nil {
		owner = reservation
		owner.preempt = make(chan struct{})
	}
	reservation.owner = owner
	reservation.preempt = owner.preempt
	if self.parent != nil {
		reservation.parent = self.parent.registerLocked(class, byteCount, usesSlot, priority, owner)
	}
	self.reservations[reservation] = true
	if class == platformTransportBudgetH1 {
		self.pendingH1ByteCount += reservation.byteCount
		if usesSlot {
			self.pendingH1SlotCount += 1
		}
	}
	return reservation
}

func (self *platformTransportBudgetReservation) higherPriorityPendingH3Locked() *platformTransportBudgetReservation {
	var best *platformTransportBudgetReservation
	for candidate := range self.budget.reservations {
		if candidate == self || candidate.closed || candidate.acquired || !candidate.pending {
			continue
		}
		precedes := candidate.class == platformTransportBudgetH3Explicit &&
			self.class != platformTransportBudgetH3Explicit
		if candidate.class == platformTransportBudgetH3Auto &&
			self.class == platformTransportBudgetH3Auto &&
			candidate.priority < self.priority {
			precedes = true
		}
		if !precedes {
			continue
		}
		if best == nil || candidate.sequence < best.sequence {
			best = candidate
		}
	}
	return best
}

// pendingH1CapacityLocked returns only the pending H1 capacity that could fit
// alongside the already-counted transports. A slot-using H1 claim beyond the
// aggregate carrier-slot cap cannot acquire until one of those transports leaves, so
// reserving its bytes and slot against a slotless Auto-H3 claim creates a false
// dependency: H1 -> Auto policy migration fills the cap with the old and new
// H1 carriers, then H3 waits for a pending H1 that is itself unable to start.
//
// H1 still has precedence. Claims that can fit are reserved here, and any H1
// registered after Auto H3 acquires can revoke that optional H3 lease. When
// claim sizes differ, reserving the largest structurally admissible claims is
// conservative regardless of which Acquire goroutine wins the wake-up race.
func (self *platformTransportBudgetReservation) pendingH1CapacityLocked(
	baseTransportCount int,
) (byteCount ByteCount, transportCount int) {
	budget := self.budget
	if budget.maxTransportCount <= 0 {
		return budget.pendingH1ByteCount, budget.pendingH1SlotCount
	}

	availableSlots := max(0, budget.maxTransportCount-baseTransportCount)
	slotByteCounts := []ByteCount{}
	for candidate := range budget.reservations {
		if candidate.closed || candidate.acquired || !candidate.pending ||
			candidate.class != platformTransportBudgetH1 {
			continue
		}
		if !candidate.usesSlot {
			byteCount += candidate.byteCount
			continue
		}
		slotByteCounts = append(slotByteCounts, candidate.byteCount)
	}
	slices.SortFunc(slotByteCounts, func(a ByteCount, b ByteCount) int {
		if b < a {
			return -1
		}
		if a < b {
			return 1
		}
		return 0
	})
	transportCount = min(availableSlots, len(slotByteCounts))
	for _, pendingByteCount := range slotByteCounts[:transportCount] {
		byteCount += pendingByteCount
	}
	return
}

func (self *platformTransportBudgetReservation) requiredCapacityLocked() (
	byteCount ByteCount,
	transportCount int,
) {
	budget := self.budget
	byteCount = budget.usedByteCount + self.byteCount
	transportCount = budget.usedTransportCount
	if self.usesSlot {
		transportCount += 1
	}
	if self.class == platformTransportBudgetH3Auto || self.class == platformTransportBudgetExtender {
		// H1 claims are registered at construction, before any carrier goroutine
		// runs. Preserve the pending claims that can structurally fit alongside
		// this claim, so required H1 remains ahead of optional Auto H3 without
		// letting claims beyond the carrier-slot cap deadlock a slotless H3 migration.
		pendingH1ByteCount, pendingH1TransportCount :=
			self.pendingH1CapacityLocked(transportCount)
		byteCount += pendingH1ByteCount
		transportCount += pendingH1TransportCount
	}
	// Preserve one higher-priority H3 claimant. Reserving one, rather than the
	// sum of every window's identical claim, guarantees progress without
	// deadlocking the budget when only one H3 carrier can fit.
	if higher := self.higherPriorityPendingH3Locked(); higher != nil {
		byteCount += higher.byteCount
		if higher.usesSlot {
			transportCount += 1
		}
	}
	return
}

func (self *platformTransportBudgetReservation) capacityFitsLocked(
	byteCount ByteCount,
	transportCount int,
) bool {
	budget := self.budget
	if budget.totalByteCount < byteCount {
		return false
	}
	if 0 < budget.maxTransportCount && budget.maxTransportCount < transportCount {
		return false
	}
	return true
}

// handoffCapacityLocked returns the capacity required when this claim replaces
// its paired, already-acquired claim. One endpoint must be H1. Accounting
// continues to include the old carrier until it actually drains; only the
// admission decision discounts it, and activeHandoff ensures no second claim
// can do the same at the same time.
func (self *platformTransportBudgetReservation) handoffCapacityLocked() (
	byteCount ByteCount,
	transportCount int,
	ok bool,
) {
	previous := self.handoffFrom
	budget := self.budget
	if previous == nil || previous.budget != budget || previous.closed ||
		!previous.acquired ||
		self.closed || self.acquired || !self.pending ||
		(self.class != platformTransportBudgetH1 &&
			previous.class != platformTransportBudgetH1) ||
		(budget.activeHandoff != nil && budget.activeHandoff != self) {
		return 0, 0, false
	}

	byteCount, transportCount = self.requiredCapacityLocked()
	byteCount -= previous.byteCount
	if previous.usesSlot {
		transportCount -= 1
	}
	return byteCount, transportCount, true
}

func (self *platformTransportBudgetReservation) admissionLocked() (
	canAcquire bool,
	usesHandoff bool,
) {
	byteCount, transportCount := self.requiredCapacityLocked()
	if self.capacityFitsLocked(byteCount, transportCount) {
		return true, false
	}
	byteCount, transportCount, ok := self.handoffCapacityLocked()
	return ok && self.capacityFitsLocked(byteCount, transportCount), ok
}

func (self *platformTransportBudgetReservation) canAcquireLocked() bool {
	canAcquire, _ := self.admissionLocked()
	return canAcquire
}

func (self *platformTransportBudgetReservation) canAcquireHierarchyLocked() bool {
	for claim := self; claim != nil; claim = claim.parent {
		if !claim.canAcquireLocked() {
			return false
		}
	}
	return true
}

// AllowHandoffFrom pairs two replacement reservations when at least one is H1.
// Multiple pairs may wait, but each limit lends to at most one pair at a time. This
// method never releases the old reservation; the migration owner closes it
// only after the replacement has authenticated and published its routes.
func (self *platformTransportBudgetReservation) AllowHandoffFrom(
	previous *platformTransportBudgetReservation,
) bool {
	if self == nil || previous == nil || self.budget == nil ||
		self.budget != previous.budget {
		return false
	}
	budget := self.budget
	budget.root.mutex.Lock()
	defer budget.root.mutex.Unlock()

	// Pair every level before publishing any of them. Root admission must be
	// able to discount exactly the same previous carrier as device admission.
	for next, old := self, previous; next != nil; next, old = next.parent, old.parent {
		if !next.canHandoffFromLocked(old) {
			return false
		}
	}
	for next, old := self, previous; next != nil; next, old = next.parent, old.parent {
		next.handoffFrom = old
		old.handoffTo = next
	}
	budget.notifyChangedLocked()
	return true
}

func (self *platformTransportBudgetReservation) canHandoffFromLocked(
	previous *platformTransportBudgetReservation,
) bool {
	if self.closed || self.acquired || !self.pending ||
		previous.closed || !previous.acquired ||
		(self.class != platformTransportBudgetH1 &&
			previous.class != platformTransportBudgetH1) {
		return false
	}
	if self.handoffFrom != nil && self.handoffFrom != previous {
		return false
	}
	if previous.handoffTo != nil && previous.handoffTo != self {
		return false
	}
	return true
}

func (self *platformTransportBudgetReservation) clearHandoffLocked() {
	budget := self.budget
	if previous := self.handoffFrom; previous != nil {
		if previous.handoffTo == self {
			previous.handoffTo = nil
		}
		self.handoffFrom = nil
	}
	if budget.activeHandoff == self {
		budget.activeHandoff = nil
	}
}

func (self *platformTransportBudgetReservation) clearHandoffToLocked() {
	budget := self.budget
	if replacement := self.handoffTo; replacement != nil {
		if replacement.handoffFrom == self {
			replacement.handoffFrom = nil
		}
		if budget.activeHandoff == replacement {
			budget.activeHandoff = nil
		}
		self.handoffTo = nil
	}
}

func (self *platformTransportBudgetReservation) canPreemptLocked(
	victim *platformTransportBudgetReservation,
) bool {
	if victim == self || victim.closed || !victim.acquired ||
		victim.class != platformTransportBudgetH3Auto || victim.preemptRequested {
		return false
	}
	switch self.class {
	case platformTransportBudgetH1, platformTransportBudgetH3Explicit:
		return true
	case platformTransportBudgetH3Auto:
		return self.priority < victim.priority
	default:
		return false
	}
}

// requestPreemptionLocked revokes only as many lower-precedence optional H3
// leases as can satisfy this claim's current byte/slot deficit. The lease
// owner tears down its H3 sockets before yielding the accounting reservation.
func (self *platformTransportBudgetReservation) requestPreemptionLocked() {
	planned := map[*platformTransportBudgetReservation]bool{}
	for claim := self; claim != nil; claim = claim.parent {
		victims, sufficient := claim.preemptionPlanLocked(planned)
		// A required H1 path must be possible at every level before tearing
		// down any optional carrier. Otherwise a resolvable child byte limit
		// could trigger preemption despite an unresolvable root carrier-slot cap,
		// or root pressure could revoke a sibling for a locally blocked H1.
		if self.class == platformTransportBudgetH1 && !sufficient {
			return
		}
		for _, victim := range victims {
			planned[victim.owner] = true
		}
	}
	for victim := range planned {
		// Any level may revoke a lease, but its socket owner receives exactly
		// one signal. Mark every copy before closing the shared channel so a
		// second deficit cannot preempt the same carrier again.
		for claim := victim; claim != nil; claim = claim.parent {
			claim.preemptRequested = true
			claim.budget.preemptedH3Count += 1
		}
		close(victim.preempt)
	}
}

func (self *platformTransportBudgetReservation) preemptionPlanLocked(
	planned map[*platformTransportBudgetReservation]bool,
) ([]*platformTransportBudgetReservation, bool) {
	budget := self.budget
	requiredBytes, requiredTransports := self.requiredCapacityLocked()
	if handoffBytes, handoffTransports, ok := self.handoffCapacityLocked(); ok {
		// Reclaim only the optional leases still needed after the paired previous
		// reservation is discounted. The previous carrier stays acquired until
		// the replacement connects.
		requiredBytes = handoffBytes
		requiredTransports = handoffTransports
	}
	byteDeficit := max(ByteCount(0), requiredBytes-budget.totalByteCount)
	transportDeficit := 0
	if 0 < budget.maxTransportCount {
		transportDeficit = max(0, requiredTransports-budget.maxTransportCount)
	}
	if byteDeficit == 0 && transportDeficit == 0 {
		return nil, true
	}

	victims := []*platformTransportBudgetReservation{}
	for candidate := range budget.reservations {
		if candidate.acquired && (candidate.preemptRequested || planned[candidate.owner]) {
			// A lower level or another waiter already requested this carrier's
			// teardown. Its still-live bytes remain charged, but the same
			// deficit must not revoke a second carrier while it drains.
			byteDeficit -= candidate.byteCount
			if candidate.usesSlot {
				transportDeficit -= 1
			}
			continue
		}
		if self.canPreemptLocked(candidate) {
			victims = append(victims, candidate)
		}
	}
	slices.SortFunc(victims, func(a, b *platformTransportBudgetReservation) int {
		// Reclaim background Auto H3 before foreground Auto H3. For equal
		// priorities, reclaim the oldest lease first so the result is stable.
		if a.priority != b.priority {
			return b.priority - a.priority
		}
		if a.sequence < b.sequence {
			return -1
		}
		if b.sequence < a.sequence {
			return 1
		}
		return 0
	})
	selectedVictims := []*platformTransportBudgetReservation{}
	for _, victim := range victims {
		if byteDeficit <= 0 && transportDeficit <= 0 {
			break
		}
		// A slotless Auto-H3 lease cannot solve a slot-only deficit.
		if byteDeficit <= 0 && (transportDeficit <= 0 || !victim.usesSlot) {
			continue
		}
		selectedVictims = append(selectedVictims, victim)
		byteDeficit -= victim.byteCount
		if victim.usesSlot {
			transportDeficit -= 1
		}
	}
	// H1 requires a sufficient plan at every level. H3 policy replacements
	// retain their existing ability to drain an optional H3 carrier while
	// waiting for a nonpreemptible H1 carrier slot to close.
	return selectedVictims, byteDeficit <= 0 && transportDeficit <= 0
}

func (self *platformTransportBudgetReservation) Acquire(ctx context.Context) bool {
	if self == nil {
		return true
	}
	for {
		budget := self.budget
		budget.root.mutex.Lock()
		if ctx.Err() != nil {
			self.releaseHierarchyLocked()
			budget.root.mutex.Unlock()
			return false
		}
		if self.closed {
			budget.root.mutex.Unlock()
			return false
		}
		if self.acquired {
			budget.root.mutex.Unlock()
			return true
		}
		if self.canAcquireHierarchyLocked() {
			for claim := self; claim != nil; claim = claim.parent {
				_, usesHandoff := claim.admissionLocked()
				claim.acquireLocked(usesHandoff)
			}
			budget.notifyChangedLocked()
			budget.root.mutex.Unlock()
			return true
		}
		self.requestPreemptionLocked()
		notify := budget.root.notify
		budget.root.mutex.Unlock()

		select {
		case <-ctx.Done():
			self.Release()
			return false
		case <-notify:
		}
	}
}

// TryAcquire admits without waiting, preemption, or a handoff overdraft. An
// extender dial already has an inner carrier reservation, so waiting for more
// capacity here could wait on that same caller forever. A failed attempt is
// left registered until its owner releases it.
func (self *platformTransportBudgetReservation) TryAcquire() bool {
	if self == nil {
		return true
	}
	budget := self.budget
	budget.root.mutex.Lock()
	defer budget.root.mutex.Unlock()
	if self.closed {
		return false
	}
	if self.acquired {
		return true
	}
	for claim := self; claim != nil; claim = claim.parent {
		byteCount, transportCount := claim.requiredCapacityLocked()
		if !claim.capacityFitsLocked(byteCount, transportCount) {
			return false
		}
	}
	for claim := self; claim != nil; claim = claim.parent {
		claim.acquireLocked(false)
	}
	budget.notifyChangedLocked()
	return true
}

func (self *platformTransportBudgetReservation) acquireLocked(usesHandoff bool) {
	budget := self.budget
	if self.class == platformTransportBudgetH1 && self.pending {
		budget.pendingH1ByteCount -= self.byteCount
		if self.usesSlot {
			budget.pendingH1SlotCount -= 1
		}
	}
	self.pending = false
	self.acquired = true
	if usesHandoff {
		budget.activeHandoff = self
		budget.handoffAcquisitionCount += 1
	} else {
		self.clearHandoffLocked()
	}
	budget.usedByteCount += self.byteCount
	budget.reservedByteCount += self.byteCount
	if self.usesSlot {
		budget.usedTransportCount += 1
	}
}

// IsWaiting reports whether this live claim is currently prevented from
// acquiring by the aggregate budget. It is a point-in-time migration hint,
// not a platform capability signal; callers that need stable eligibility use
// PlatformTransportAutoEligibility instead.
func (self *platformTransportBudgetReservation) IsWaiting() bool {
	if self == nil {
		return false
	}
	budget := self.budget
	budget.root.mutex.Lock()
	defer budget.root.mutex.Unlock()
	if self.closed || self.acquired || !self.pending {
		return false
	}
	return !self.canAcquireHierarchyLocked()
}

// PreemptNotify closes when a higher-precedence claim needs this acquired,
// optional Auto-H3 lease. The owner must stop its H3 sockets, then call Yield.
func (self *platformTransportBudgetReservation) PreemptNotify() <-chan struct{} {
	if self == nil {
		return nil
	}
	budget := self.budget
	budget.root.mutex.Lock()
	defer budget.root.mutex.Unlock()
	return self.preempt
}

// Yield returns an acquired optional H3 lease to pending state without closing
// the reservation. It can reacquire automatically after higher-precedence
// demand leaves. The socket owner calls this only after its H3 runners stop.
func (self *platformTransportBudgetReservation) Yield() bool {
	if self == nil {
		return false
	}
	budget := self.budget
	budget.root.mutex.Lock()
	defer budget.root.mutex.Unlock()
	if self.closed || !self.acquired || self.class != platformTransportBudgetH3Auto {
		return false
	}
	// Yield can occur while this optional Auto-H3 reservation is either side
	// of a policy handoff. Returning its capacity must also return the loan;
	// otherwise activeHandoff could remain pinned to a no-longer-acquired pair.
	preempt := make(chan struct{})
	for claim := self; claim != nil; claim = claim.parent {
		claim.clearHandoffToLocked()
		claim.clearHandoffLocked()
		claim.budget.usedByteCount -= claim.byteCount
		claim.budget.releasedByteCount += claim.byteCount
		if claim.usesSlot {
			claim.budget.usedTransportCount -= 1
		}
		claim.acquired = false
		claim.pending = true
		claim.preemptRequested = false
		claim.preempt = preempt
	}
	budget.notifyChangedLocked()
	return true
}

// Release is idempotent and also unregisters a pending H1 priority claim.
func (self *platformTransportBudgetReservation) Release() {
	if self == nil {
		return
	}
	budget := self.budget
	budget.root.mutex.Lock()
	defer budget.root.mutex.Unlock()
	self.releaseHierarchyLocked()
}

func (self *platformTransportBudgetReservation) releaseHierarchyLocked() {
	if self.closed {
		return
	}
	for claim := self; claim != nil; claim = claim.parent {
		claim.releaseLocked()
	}
	self.budget.notifyChangedLocked()
}

func (self *platformTransportBudgetReservation) releaseLocked() {
	if self.closed {
		return
	}
	self.closed = true
	budget := self.budget
	// This reservation may be either the replacement or the old carrier.
	// Releasing either endpoint resolves the loan before waking another waiter.
	self.clearHandoffToLocked()
	self.clearHandoffLocked()
	if self.class == platformTransportBudgetH1 && self.pending {
		budget.pendingH1ByteCount -= self.byteCount
		if self.usesSlot {
			budget.pendingH1SlotCount -= 1
		}
	}
	if self.acquired {
		budget.usedByteCount -= self.byteCount
		budget.releasedByteCount += self.byteCount
		if self.usesSlot {
			budget.usedTransportCount -= 1
		}
	}
	self.pending = false
	self.acquired = false
	delete(budget.reservations, self)
}
