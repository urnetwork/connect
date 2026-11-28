// Strict window capacity before construction, and the slot handover.
//
// A strict window (StrictWindowSizeHardMax: every low-memory profile, the
// hosted proxy devices, iOS and Android) never installs more clients than its
// hard max. A warned client keeps its slot until it is removed, and in a
// strict window nothing removed it: capacity collapse runs only above the hard
// max, so a drained client held its slot forever. Every resize pass then saw
// the hole its warning left in the size math and expanded: it listed, minted,
// registered, dialed and evaluated a candidate that the strict hard max
// refused only at admission, and retired it. Lifetime rotation never
// completed, and the window repeated this on the discovery backoff for as long
// as it lived.
//
// With StrictWindowHandover a strict discovery window plans with the gate's
// own inputs:
//   - capacity before construction: a pass builds only as many candidates as
//     admission would accept now (strictAdmissionCapacityWithLock), and the
//     enumerator lists and mints nothing while the capacity is zero, and no
//     more than a running pass asked for;
//   - collapse at admission: a pass at the hard max reserves one warned
//     client that the capacity collapse test would remove (flowless, or past
//     the collapse deadline), and admission swaps the evaluated candidate in
//     for it in one stateLock section, so the installed count stays at the
//     hard max and never falls below the minimum.
//
// strictWindowAdmissionAllowed stays the authoritative gate. Fixed
// destinations, whose candidates may be same-identity replacements, and every
// non-strict window plan as before. No timeout, lifetime, collapse deadline,
// evaluation, probe or backoff bound changes: a skipped pass attempts nothing,
// so it neither advances nor resets the discovery backoff.
package connect

import (
	"math"
	"slices"
	"sync/atomic"
	"time"
)

// A strict window's current expansion pass, as the enumerator and admission
// see it. Guarded by the window stateLock.
type strictWindowPass struct {
	owner *multiClientEvaluationOwner
	// how many more candidates the enumerator may hand the pass: its request
	// count, less each candidate handed over, so a candidate is never minted
	// ahead for a slot the pass is already filling
	remainingCandidates int
	// the warned client whose slot the pass's candidate takes, when the
	// window is at its hard max; nil otherwise
	handover *strictWindowHandover
}

// One strict window's reservation of a warned client's slot for the
// candidate its current expansion pass admits. Unchanged once the pass
// starts, except completed.
type strictWindowHandover struct {
	victim *multiClientChannel
	// the victim's age when the pass chose it, for the collapse deadline's
	// re-check at admission; the deadline only gets more past with time
	victimDuration time.Duration
	serviceRefresh *tcpSynRetry
	// the swap happened; resize reads it after the pass returns
	completed atomic.Bool
}

// An optional generator capability: the platform's override of the strict
// window handover, as the generator's latest discovery answer carried it
// (`FindProviders2Result.WindowStrictHandover`). It is the platform's off
// switch: false turns every strict window's handover off, while true and no
// override leave the host's StrictWindowHandover, so the platform can turn the
// handover off but never on past its host. The windows read it at each
// decision, so a discovery answer that carries it takes effect from the next
// one. A generator without the capability overrides nothing.
type MultiClientGeneratorWithStrictWindowHandoverOverride interface {
	// The override as of the latest discovery answer, unset when that answer
	// carried none.
	StrictWindowHandoverOverride() (enabled bool, set bool)
}

// Both parts are on: a strict window with the handover enabled, which the
// platform has not turned off.
func (self *multiClientWindow) strictWindowHandoverEnabled() bool {
	return self != nil && self.settings != nil &&
		self.settings.StrictWindowSizeHardMax && self.settings.StrictWindowHandover &&
		!self.strictWindowHandoverTurnedOff()
}

// Whether the platform's latest discovery answer turned the handover off.
func (self *multiClientWindow) strictWindowHandoverTurnedOff() bool {
	override, ok := self.generator.(MultiClientGeneratorWithStrictWindowHandoverOverride)
	if !ok {
		return false
	}
	enabled, set := override.StrictWindowHandoverOverride()
	return set && !enabled
}

// How many new identities admission would accept right now: the free slots
// below the hard max, plus the one slot a live handover reservation will
// swap. Unbounded when the window has no strict ceiling. These are the
// inputs of strictWindowAdmissionAllowed, read under the same lock: without a
// reservation the capacity is positive exactly when that gate admits a new
// identity. A same-identity replacement needs no capacity; the gate admits it
// at the ceiling.
//
// must be called with stateLock
func (self *multiClientWindow) strictAdmissionCapacityWithLock(windowSizeHardMax int) int {
	if self.settings == nil || !self.settings.StrictWindowSizeHardMax || windowSizeHardMax <= 0 {
		return math.MaxInt
	}
	capacity := windowSizeHardMax - len(self.clients)
	if pass := self.strictPass; pass != nil && pass.handover != nil &&
		self.clients[pass.handover.victim.ClientId()] == pass.handover.victim {
		capacity += 1
	}
	return capacity
}

// The capacity of strictAdmissionCapacityWithLock, under the lock.
func (self *multiClientWindow) strictAdmissionCapacity(windowSizeHardMax int) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.strictAdmissionCapacityWithLock(windowSizeHardMax)
}

// The window size resize and selection read, under the same lock.
//
// must be called with stateLock
func (self *multiClientWindow) windowSizeWithLock() WindowSizeSettings {
	if _, profileWindowSize, ok := self.performanceProfile.FixedWindow(); ok {
		return profileWindowSize
	}
	return self.defaultWindowSizeWithLock()
}

// Whether the enumerator may list or mint for this window now. A strict
// discovery window lists and mints only while admission has capacity, as the
// discovery demand path already declines at the hard max
// (discoveryDemandLiveWithLock), and no more than a running pass still wants.
// Between passes it may mint the one candidate the next pass takes: a listing
// that outlasts its pass's acquisition budget, on a slow platform or after an
// error backoff, is minted and parked for the next pass instead of
// discarded, so every pass does not repeat the same race.
func (self *multiClientWindow) strictDiscoveryAdmissible() bool {
	if !self.strictWindowHandoverEnabled() {
		return true
	}
	if _, fixed := self.generator.FixedDestinationSize(); fixed {
		return true
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	pass := self.strictPass
	if self.serviceRefreshOwner != nil && !self.serviceRefreshLiveWithLock() {
		return false
	}
	return (pass == nil || 0 < pass.remainingCandidates) &&
		0 < self.strictAdmissionCapacityWithLock(self.windowSizeWithLock().WindowSizeHardMax)
}

// Hands a minted candidate to the running pass, from a discovery demand read
// or from the ordinary listing, and charges the pass's offer budget when the
// pass took it: a demand candidate fills one of the slots the pass asked for,
// as an ordinary one does.
func (self *multiClientWindow) offerPassCandidate(
	demandRead *discoveryDemandRead,
	args *multiClientChannelArgs,
	expirationTime time.Time,
	serviceOwners ...*tcpSynRetry,
) bool {
	var offered bool
	if demandRead == nil {
		offered = self.offerClientArgs(args, expirationTime, serviceOwners...)
	} else {
		offered = self.offerDiscoveryDemandArgs(demandRead, args)
	}
	if offered {
		self.noteStrictPassCandidateOffered()
	}
	return offered
}

// The enumerator handed the current pass one candidate, which uses up one of
// the candidates the pass asked for.
func (self *multiClientWindow) noteStrictPassCandidateOffered() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if pass := self.strictPass; pass != nil && 0 < pass.remainingCandidates {
		pass.remainingCandidates -= 1
	}
}

// Which warned clients may hand over their slot: every cause whose intent is backfill, under the same collapse test,
// as non-strict collapse sheds every warned client (owner answer to D2
// question 2). Owner override (a), only lifetime drains: return cause ==
// windowWarningDrain. A capacity warning is the window's own ulimit policy,
// and a family-swap victim already gives its slot to a v6-capable candidate
// through its own path.
func strictHandoverCauseEligible(cause windowWarningCause) bool {
	switch cause {
	case windowWarningDrain, windowWarningSilent, windowWarningQuarantine, windowWarningStarved, windowWarningUnhealthy:
		return true
	default:
		return false
	}
}

// The flow half of the capacity collapse test for a handover: flowless, or past the collapse deadline of
// collapseDeadlineLifetimes x MaxClientLifetime, when its flows are cut
// exactly as non-strict collapse cuts them (owner answer to D2 question 1).
// Owner override (b), never cut a flow: return self.flowCount(client) == 0.
// Must be called with no window or channel lock held (see flowCount).
func (self *multiClientWindow) strictHandoverRemovalAllowed(client *multiClientChannel, clientDuration time.Duration) bool {
	return self.collapseRemovalAllowed(client, clientDuration)
}

// Orders clients as capacity collapse keeps them, for a sort whose tail is
// shed: dualstack exits ahead of single-family ones (IPV6.md
// B2), then by descending weight, then by descending duration.
func collapseKeepOrder(
	a *multiClientChannel,
	b *multiClientChannel,
	weights map[*multiClientChannel]float32,
	durations map[*multiClientChannel]time.Duration,
) int {
	if c := collapseFamilyOrder(a, b); c != 0 {
		return c
	}
	// descending weight
	aWeight := weights[a]
	bWeight := weights[b]
	if aWeight < bWeight {
		return 1
	} else if bWeight < aWeight {
		return -1
	}
	aDuration := durations[a]
	bDuration := durations[b]
	if aDuration < bDuration {
		return 1
	} else if bDuration < aDuration {
		return -1
	}
	return 0
}

// Picks the warned client whose slot a strict window at its hard max hands to
// the next candidate it admits, or nil. A
// client is eligible when it is out of new-flow selection for an eligible
// cause and the capacity collapse test passes for it: past the stats grace, a
// zero weight over the stats window, and flowless or past the collapse
// deadline. Candidates are ordered as the collapse orders them and
// taken from the tail, as the collapse sheds. Must be called with no window
// or channel lock held.
func (self *multiClientWindow) selectStrictHandoverVictim(
	warnedClients []*multiClientChannel,
	weights map[*multiClientChannel]float32,
	durations map[*multiClientChannel]time.Duration,
) *strictWindowHandover {
	eligibleClients := []*multiClientChannel{}
	for _, client := range warnedClients {
		if strictHandoverCauseEligible(client.windowWarningCause()) {
			eligibleClients = append(eligibleClients, client)
		}
	}
	slices.SortFunc(eligibleClients, func(a *multiClientChannel, b *multiClientChannel) int {
		return collapseKeepOrder(a, b, weights, durations)
	})
	for i := len(eligibleClients) - 1; 0 <= i; i -= 1 {
		client := eligibleClients[i]
		if self.settings.StatsWindowGraceperiod <= durations[client] && weights[client] <= 0 &&
			self.strictHandoverRemovalAllowed(client, durations[client]) {
			return &strictWindowHandover{
				victim:         client,
				victimDuration: durations[client],
			}
		}
	}
	return nil
}

// Starts a strict window's pass: the candidates it requests, and its handover
// reservation, if any.
func (self *multiClientWindow) beginStrictPass(
	owner *multiClientEvaluationOwner,
	requestCount int,
	handover *strictWindowHandover,
) *strictWindowPass {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	pass := &strictWindowPass{
		owner:               owner,
		remainingCandidates: requestCount,
		handover:            handover,
	}
	self.strictPass = pass
	return pass
}

// Ends the pass and its reservation; the pass's expand defer calls it.
func (self *multiClientWindow) endStrictPass(pass *strictWindowPass) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.strictPass == pass {
		self.strictPass = nil
	}
}

// The reservation a candidate of this pass may still swap into, or nil: the
// reservation is this pass's, unspent, and its victim is done or still passes
// the flow half of the collapse test. Read with no lock held, then installed
// against what is there under the lock, as replacementAllowed is: a victim
// takes no new flows, so the window in which one lands is tiny.
func (self *multiClientWindow) takeStrictHandover(owner *multiClientEvaluationOwner) *strictWindowHandover {
	// turned off since the pass reserved the slot: the candidate is declined
	// as at the hard max and the victim keeps its slot, and the reservation
	// ends with the pass
	if !self.strictWindowHandoverEnabled() {
		return nil
	}
	var handover *strictWindowHandover
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if pass := self.strictPass; pass != nil && pass.owner == owner &&
			pass.handover != nil && !pass.handover.completed.Load() {
			handover = pass.handover
		}
	}()
	if handover == nil {
		return nil
	}
	if handover.serviceRefresh != nil {
		if !self.serviceRefreshLive(handover.serviceRefresh) || self.flowCount(handover.victim) != 0 {
			return nil
		}
		return handover
	}
	if !handover.victim.IsDone() && !self.strictHandoverRemovalAllowed(handover.victim, handover.victimDuration) {
		return nil
	}
	return handover
}

// The removal by its error of a handover's victim that ended with one, or nil
// for a victim that is still live. Called with no lock held.
func (self *multiClientWindow) classifyHandoverVictimRemoval(
	victim *multiClientChannel,
	windowSize WindowSizeSettings,
) *windowErrorRemoval {
	stats, err := victim.WindowStats()
	if err == nil {
		return nil
	}
	return self.classifyErrorRemoval(victim, err, stats, windowSize, self.stickyExit())
}

// The swap's precondition under the install lock: the reservation is still
// installed and unspent, and its victim is still installed and still out of
// selection, or done.
//
// must be called with stateLock
func (self *multiClientWindow) strictHandoverSwappableWithLock(handover *strictWindowHandover) bool {
	victim := handover.victim
	pass := self.strictPass
	valid := pass != nil && pass.handover == handover && !handover.completed.Load() &&
		self.clients[victim.ClientId()] == victim && (victim.isWarning() || victim.IsDone())
	if !valid {
		return false
	}
	if handover.serviceRefresh != nil {
		return self.serviceRefreshOwner == handover.serviceRefresh && self.serviceRefreshLiveWithLock() &&
			victim.serviceRefreshReservation.CompareAndSwap(handover.serviceRefresh, &serviceRefreshRetiringOwner)
	}
	return true
}
