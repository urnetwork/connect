// Process counters of what windows do with their window clients, for a host
// that runs many devices in one process (the proxy service) to export.
//
// A hosted window mints, dials, registers and evaluates every candidate it
// builds, and retires every one it does not keep, so the share of candidates
// a window builds only to discard is the cost these counters size. They count
// every candidate an expansion builds by its outcome, every resize pass that
// wants to grow by the window state it found, every warning by cause, every
// removal by cause, and the windows by the state their last resize pass ended
// in.
//
// Labels are fixed vocabularies, never identities. One instance is shared by
// every window of the process; a nil instance counts nothing, so a device
// built without one (every app) does no counting work. Safe for concurrent
// use.
package connect

import (
	"errors"
	"sync/atomic"
)

// The window a counter belongs to.
type windowStatsWindow int

const (
	windowStatsWindowQuality windowStatsWindow = iota
	windowStatsWindowSpeed
	// any other window type
	windowStatsWindowAuto
	windowStatsWindowCount
)

func (self windowStatsWindow) String() string {
	switch self {
	case windowStatsWindowQuality:
		return "quality"
	case windowStatsWindowSpeed:
		return "speed"
	default:
		return "auto"
	}
}

func windowStatsWindowOf(windowType WindowType) windowStatsWindow {
	switch windowType {
	case WindowTypeQuality:
		return windowStatsWindowQuality
	case WindowTypeSpeed:
		return windowStatsWindowSpeed
	default:
		return windowStatsWindowAuto
	}
}

// The state a resize pass that wants to grow found its window in. The states
// that build candidates come first; a candidate is counted with the state of
// the pass that built it.
type windowExpandPassState int

const (
	// fewer kept clients than the window's minimum
	windowExpandPassBelowMin windowExpandPassState = iota
	// at least the minimum, fewer than the target
	windowExpandPassBelowTarget
	// fewer kept clients than the target while a strict window's installed
	// clients, warned ones included, fill its hard max
	windowExpandPassStrictHole
	// a strict window at its hard max with no warned client that may hand
	// over its slot, so the pass builds nothing (counted once the pre-check
	// ships)
	windowExpandPassSkippedStrictFull
	// the expansion backoff or a fixed destination's re-dial pacing held the
	// pass
	windowExpandPassDeferred
	// the payer's balance refused the window's contracts, so the pass sent a
	// balance probe instead of building candidates (see
	// ip_remote_multi_client_balance_hold.go)
	windowExpandPassHeldInsufficientBalance
	windowExpandPassStateCount
)

// The pass states that build candidates.
const windowExpandPassBuildStateCount = windowExpandPassStrictHole + 1

func (self windowExpandPassState) String() string {
	switch self {
	case windowExpandPassBelowMin:
		return "below_min"
	case windowExpandPassBelowTarget:
		return "below_target"
	case windowExpandPassStrictHole:
		return "strict_hole"
	case windowExpandPassSkippedStrictFull:
		return "skipped_strict_full"
	case windowExpandPassDeferred:
		return "deferred"
	case windowExpandPassHeldInsufficientBalance:
		return "held_insufficient_balance"
	default:
		return "unknown"
	}
}

// How one candidate an expansion built ended.
type windowCandidateOutcome int

const (
	// installed into a free slot, or over a same-identity client
	windowCandidateAdmitted windowCandidateOutcome = iota
	// installed in the slot a warned client handed over (counted once the
	// handover ships)
	windowCandidateAdmittedHandover
	// evaluated, then refused by the strict hard max
	windowCandidateDeclinedStrictCap
	// evaluated, then refused because the same identity's live client still
	// carries flows
	windowCandidateDeclinedReplacement
	// evaluated, but its pass needed fewer clients or had ended
	windowCandidateNotAdmittedSurplus
	// the client could not be constructed
	windowCandidateFailedSetup
	// the evaluation ping failed or went unanswered
	windowCandidateFailedPing
	// the local contract for the evaluation could not be acquired
	windowCandidateFailedContract
	// the window or its evaluation epoch ended first
	windowCandidateCanceled
	windowCandidateOutcomeCount
)

func (self windowCandidateOutcome) String() string {
	switch self {
	case windowCandidateAdmitted:
		return "admitted"
	case windowCandidateAdmittedHandover:
		return "admitted_handover"
	case windowCandidateDeclinedStrictCap:
		return "declined_strict_cap"
	case windowCandidateDeclinedReplacement:
		return "declined_replacement"
	case windowCandidateNotAdmittedSurplus:
		return "not_admitted_surplus"
	case windowCandidateFailedSetup:
		return "failed_setup"
	case windowCandidateFailedPing:
		return "failed_ping"
	case windowCandidateFailedContract:
		return "failed_contract"
	case windowCandidateCanceled:
		return "canceled"
	default:
		return "unknown"
	}
}

// Why a client is out of new-flow selection, as a resize pass classifies it.
type windowWarningCause int

const (
	windowWarningNone windowWarningCause = iota
	// past its lifetime
	windowWarningDrain
	// repeated all-silent probe passes
	windowWarningSilent
	// a stats or blackhole verdict demoted, deferred, or rank-warned
	windowWarningUnhealthy
	// its provider is failing dials
	windowWarningStarved
	// at the window's source-count ulimit
	windowWarningCapacity
	// benched by a soft verdict while carrying flows
	windowWarningQuarantine
	// giving up its slot to a v6-capable candidate
	windowWarningFamilySwap
	windowWarningCauseCount
)

func (self windowWarningCause) String() string {
	switch self {
	case windowWarningDrain:
		return "drain"
	case windowWarningSilent:
		return "silent"
	case windowWarningUnhealthy:
		return "unhealthy"
	case windowWarningStarved:
		return "starved"
	case windowWarningCapacity:
		return "capacity"
	case windowWarningQuarantine:
		return "quarantine"
	case windowWarningFamilySwap:
		return "family_swap"
	default:
		return "none"
	}
}

// Why a client left its window.
type windowRemovalCause int

const (
	// an error ended it: a blackhole verdict, a send stall, a contract
	// reliability result, or a failed ping
	windowRemovalVerdict windowRemovalCause = iota
	// sustained or rank-removed stats-unhealthy
	windowRemovalUnhealthy
	// its routes stayed down for the whole migration grace
	windowRemovalTransportDown
	// capacity collapse above the hard max
	windowRemovalCollapse
	// it handed its slot to an admitted candidate (counted once the handover
	// ships)
	windowRemovalHandover
	// a strict family swap removed it to make room for a v6-capable candidate
	windowRemovalFamilySwap
	// it was canceled without another error, or its window closed
	windowRemovalClosed
	windowRemovalCauseCount
)

func (self windowRemovalCause) String() string {
	switch self {
	case windowRemovalVerdict:
		return "verdict"
	case windowRemovalUnhealthy:
		return "unhealthy"
	case windowRemovalTransportDown:
		return "transport_down"
	case windowRemovalCollapse:
		return "collapse"
	case windowRemovalHandover:
		return "handover"
	case windowRemovalFamilySwap:
		return "family_swap"
	case windowRemovalClosed:
		return "closed"
	default:
		return "unknown"
	}
}

// The state a window's resize pass ended in. The states partition the active
// windows; a disabled window (target 0) is in none of them.
type windowState int

const (
	windowStateNone windowState = iota
	// at least the target of selectable clients
	windowStateSelectableFull
	// the minimum is met, fewer selectable clients than the target, and the
	// window can still grow
	windowStateBelowTarget
	// the minimum is met and there are fewer selectable clients than the
	// target, while a strict window's installed clients, warned ones
	// included, fill its hard max, so admission refuses another identity
	windowStateStrictHole
	// some selectable clients, short of the minimum
	windowStateBelowMin
	// no selectable client
	windowStateNoSelectable
	windowStateCount
)

func (self windowState) String() string {
	switch self {
	case windowStateSelectableFull:
		return "selectable_full"
	case windowStateBelowTarget:
		return "below_target"
	case windowStateStrictHole:
		return "strict_hole"
	case windowStateBelowMin:
		return "below_min"
	case windowStateNoSelectable:
		return "no_selectable"
	default:
		return "none"
	}
}

// Classifies the end of a resize pass from the window's
// selectable (live, unwarned) and installed client counts and the pass's
// sizes, with the same minimum the monitor's readiness reads. A fixed
// destination offers its warned peers when it has nothing else
// (orderedClients), so every installed peer of a fixed window is selectable.
func windowStateOf(
	selectableCount int,
	installedCount int,
	targetWindowSize int,
	windowSizeMin int,
	fixedDestination bool,
	strictWindowSizeHardMax bool,
	windowSizeHardMax int,
) windowState {
	if fixedDestination {
		selectableCount = max(selectableCount, installedCount)
	}
	switch {
	case targetWindowSize <= 0:
		return windowStateNone
	case selectableCount <= 0:
		return windowStateNoSelectable
	case !windowMinSatisfied(
		windowSizeMin,
		selectableCount,
		max(0, installedCount-selectableCount),
		fixedDestination,
		strictWindowSizeHardMax,
		windowSizeHardMax,
	):
		return windowStateBelowMin
	case targetWindowSize <= selectableCount:
		return windowStateSelectableFull
	case strictWindowSizeHardMax && 0 < windowSizeHardMax && windowSizeHardMax <= installedCount:
		return windowStateStrictHole
	default:
		return windowStateBelowTarget
	}
}

// Counts window client candidates, expansion passes,
// warnings and removals by fixed label values, and holds the live windows by
// the state their last resize pass ended in. The zero value is ready.
type WindowClientStats struct {
	candidates   [windowStatsWindowCount][windowExpandPassBuildStateCount][windowCandidateOutcomeCount]atomic.Uint64
	expandPasses [windowStatsWindowCount][windowExpandPassStateCount]atomic.Uint64
	warnings     [windowStatsWindowCount][windowWarningCauseCount]atomic.Uint64
	removals     [windowStatsWindowCount][windowRemovalCauseCount]atomic.Uint64
	windows      [windowStatsWindowCount][windowStateCount]atomic.Int64
	// balance probes held windows sent, by what each learned
	balanceProbes [windowStatsWindowCount][balanceProbeOutcomeCount]atomic.Uint64
}

// A process-wide instance to share by every window of the process.
func NewWindowClientStats() *WindowClientStats {
	return &WindowClientStats{}
}

// One candidate that a pass in the given state built ended with the outcome.
// Nil-safe.
func (self *WindowClientStats) addCandidate(
	windowType WindowType,
	state windowExpandPassState,
	outcome windowCandidateOutcome,
) {
	if self == nil ||
		state < 0 || windowExpandPassBuildStateCount <= state ||
		outcome < 0 || windowCandidateOutcomeCount <= outcome {
		return
	}
	self.candidates[windowStatsWindowOf(windowType)][state][outcome].Add(1)
}

// One balance probe a held window sent learned the outcome. Nil-safe.
func (self *WindowClientStats) addBalanceProbe(windowType WindowType, outcome BalanceProbeOutcome) {
	if self == nil || outcome < 0 || balanceProbeOutcomeCount <= outcome {
		return
	}
	self.balanceProbes[windowStatsWindowOf(windowType)][outcome].Add(1)
}

// One resize pass wanted to grow and found the window in the state. Nil-safe.
func (self *WindowClientStats) addExpandPass(windowType WindowType, state windowExpandPassState) {
	if self == nil || state < 0 || windowExpandPassStateCount <= state {
		return
	}
	self.expandPasses[windowStatsWindowOf(windowType)][state].Add(1)
}

// One client entered the warned state for the cause. Nil-safe.
func (self *WindowClientStats) addWarning(windowType WindowType, cause windowWarningCause) {
	if self == nil || cause <= windowWarningNone || windowWarningCauseCount <= cause {
		return
	}
	self.warnings[windowStatsWindowOf(windowType)][cause].Add(1)
}

// One client left its window for the cause. Nil-safe.
func (self *WindowClientStats) addRemoval(windowType WindowType, cause windowRemovalCause) {
	if self == nil || cause < 0 || windowRemovalCauseCount <= cause {
		return
	}
	self.removals[windowStatsWindowOf(windowType)][cause].Add(1)
}

// Moves one window from one state to another; windowStateNone on either side
// adds or retracts the window. Nil-safe.
func (self *WindowClientStats) moveWindowState(windowType WindowType, from windowState, to windowState) {
	if self == nil || from == to {
		return
	}
	window := windowStatsWindowOf(windowType)
	if windowStateNone < from && from < windowStateCount {
		self.windows[window][from].Add(-1)
	}
	if windowStateNone < to && to < windowStateCount {
		self.windows[window][to].Add(1)
	}
}

// Calls visit for every candidate counter, in a fixed order,
// with its window, pass state and outcome label values.
func (self *WindowClientStats) VisitCandidates(visit func(window string, state string, outcome string, count uint64)) {
	if self == nil {
		return
	}
	for window := range windowStatsWindowCount {
		for state := range windowExpandPassBuildStateCount {
			for outcome := range windowCandidateOutcomeCount {
				visit(window.String(), state.String(), outcome.String(), self.candidates[window][state][outcome].Load())
			}
		}
	}
}

// Calls visit for every expansion pass counter, in a fixed
// order, with its window and pass state label values.
func (self *WindowClientStats) VisitExpandPasses(visit func(window string, state string, count uint64)) {
	if self == nil {
		return
	}
	for window := range windowStatsWindowCount {
		for state := range windowExpandPassStateCount {
			visit(window.String(), state.String(), self.expandPasses[window][state].Load())
		}
	}
}

// Calls visit for every balance probe counter, in a fixed order, with its
// window and outcome label values.
func (self *WindowClientStats) VisitBalanceProbes(visit func(window string, outcome string, count uint64)) {
	if self == nil {
		return
	}
	for window := range windowStatsWindowCount {
		for outcome := range balanceProbeOutcomeCount {
			visit(window.String(), outcome.String(), self.balanceProbes[window][outcome].Load())
		}
	}
}

// Calls visit for every warning counter, in a fixed order, with
// its window and cause label values.
func (self *WindowClientStats) VisitWarnings(visit func(window string, cause string, count uint64)) {
	if self == nil {
		return
	}
	for window := range windowStatsWindowCount {
		for cause := windowWarningNone + 1; cause < windowWarningCauseCount; cause += 1 {
			visit(window.String(), cause.String(), self.warnings[window][cause].Load())
		}
	}
}

// Calls visit for every removal counter, in a fixed order, with
// its window and cause label values.
func (self *WindowClientStats) VisitRemovals(visit func(window string, cause string, count uint64)) {
	if self == nil {
		return
	}
	for window := range windowStatsWindowCount {
		for cause := range windowRemovalCauseCount {
			visit(window.String(), cause.String(), self.removals[window][cause].Load())
		}
	}
}

// Calls visit for every window state, in a fixed order, with its
// window and state label values and the number of live windows in it.
func (self *WindowClientStats) VisitWindows(visit func(window string, state string, count int64)) {
	if self == nil {
		return
	}
	for window := range windowStatsWindowCount {
		for state := windowStateNone + 1; state < windowStateCount; state += 1 {
			visit(window.String(), state.String(), self.windows[window][state].Load())
		}
	}
}

// The process's window client counters. Nil (bare fixtures, and every app)
// counts nothing.
func (self *multiClientWindow) windowClientStats() *WindowClientStats {
	if self == nil || self.settings == nil {
		return nil
	}
	return self.settings.WindowClientStats
}

// Classifies the error a removed client ended with. A
// bare "Done." is a cancellation with no other cause: the user removed the
// provider, the window shuffled, or the channel was closed.
func windowRemovalCauseOf(err error) windowRemovalCause {
	switch {
	case errors.Is(err, errTransportDownTimeout):
		return windowRemovalTransportDown
	case err == nil || err.Error() == "Done.":
		return windowRemovalClosed
	default:
		return windowRemovalVerdict
	}
}

// Classifies a resize pass that wants to grow from its
// kept (unwarned) and installed client counts.
func windowExpandPassStateOf(
	keptCount int,
	installedCount int,
	windowSizeMin int,
	strictWindowSizeHardMax bool,
	windowSizeHardMax int,
) windowExpandPassState {
	switch {
	case strictWindowSizeHardMax && 0 < windowSizeHardMax && windowSizeHardMax <= installedCount:
		return windowExpandPassStrictHole
	case keptCount < windowSizeMin:
		return windowExpandPassBelowMin
	default:
		return windowExpandPassBelowTarget
	}
}

// Classifies a candidate whose evaluation
// failed: canceled when the window or its evaluation epoch ended first, a
// contract failure when the local contract could not be acquired before any
// provider contact, and a ping failure otherwise.
func windowCandidateFailureOutcome(canceled bool, localContractUnavailable bool) windowCandidateOutcome {
	switch {
	case canceled:
		return windowCandidateCanceled
	case localContractUnavailable:
		return windowCandidateFailedContract
	default:
		return windowCandidateFailedPing
	}
}

// The resize pass's warning cause for this channel: its warning cause, or a
// quarantine, which benches it without the warning flag. None for a client
// that new flows may still choose.
func (self *multiClientChannel) windowWarningCause() windowWarningCause {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.warning {
		switch self.warnCause {
		case warnDraining:
			return windowWarningDrain
		case warnStarved:
			return windowWarningStarved
		case warnCapacity:
			return windowWarningCapacity
		case warnUnhealthy:
			return windowWarningUnhealthy
		case warnSilent:
			return windowWarningSilent
		case warnFamilySwap:
			return windowWarningFamilySwap
		}
	}
	if self.quarantined {
		return windowWarningQuarantine
	}
	return windowWarningNone
}

// Counts the warned clients of one resize pass whose cause changed since the
// last pass, so a warning is counted once per transition into it rather than
// once per pass. The resize loop owns the record; nothing else reads it.
type windowWarningTransitions struct {
	clientCauses map[*multiClientChannel]windowWarningCause
}

// Records this pass's warned clients and counts each new cause. A warned
// client that new flows may still choose (a rank-shielded unhealthy client is
// warned for the size math only) is not in the warned state.
func (self *windowWarningTransitions) observe(
	stats *WindowClientStats,
	windowType WindowType,
	warnedClients []*multiClientChannel,
) {
	if stats == nil {
		return
	}
	clientCauses := make(map[*multiClientChannel]windowWarningCause, len(warnedClients))
	for _, client := range warnedClients {
		cause := client.windowWarningCause()
		if cause == windowWarningNone {
			continue
		}
		if self.clientCauses[client] != cause {
			stats.addWarning(windowType, cause)
		}
		clientCauses[client] = cause
	}
	self.clientCauses = clientCauses
}

// The selectable (live, unwarned, and counted as the monitor's readiness
// counts them) and installed client counts at this moment.
func (self *multiClientWindow) windowStateCounts(fixedDestination bool) (selectableCount int, installedCount int) {
	selectableCount = self.hardMinimumClientCount(fixedDestination)
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		installedCount = len(self.clients)
	}()
	return
}
