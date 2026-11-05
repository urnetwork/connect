// A discovery window's refill while its payer is out of balance (server
// CPUOPT4.md, Part B #1).
//
// While the payer's balance is short, the platform refuses every contract a
// window client asks for with InsufficientBalance. A below-target discovery
// window then builds candidate after candidate on every expansion step: a
// discovery listing, a minted identity, Provide, a platform transport dial
// and an evaluation ping, whose contract wait asks for a contract about eight
// times before the candidate fails and is retired. None of it can succeed
// until the balance changes, and a hosted device keeps refilling for as long
// as it is held (server/proxy ProxyDevice.Active). On Main on 2026-11-04 about
// 2,000 such refusals a second pinned both proxy hosts.
//
// With OutOfBalanceRefillProbe the window holds its refill instead:
//   - The first InsufficientBalance status any of its clients or candidates
//     reports holds it. Any granted contract a client reports releases it.
//     Other results say nothing about the balance and leave it as it stood.
//   - While held, an expansion step that would build candidates sends one
//     contract probe under the generator's own credential
//     (MultiClientGeneratorBalanceProbe) and builds nothing. The enumerator
//     lists and mints nothing.
//   - A probe that finds the balance restored releases the hold, and the same
//     pass expands at once.
//   - A probe the platform refuses for the balance or rate limits keeps the
//     hold. The step counts as an unproductive expansion, so the expansion
//     backoff advances exactly as it does for a failed expansion. A restored
//     balance is therefore found at the step that would have found it before.
//   - A probe that cannot answer (the destination is gone, the request
//     failed) releases the hold. The window falls back to the ordinary
//     expansion, which holds again on the next refusal, so a probe never
//     leaves a window without refill.
//
// Nothing else moves: the backoff's bounds, the expansion and evaluation
// deadlines, live clients' contract retries and every health and detection
// bound are unchanged, and nothing is disconnected. A window that never sees a
// refusal for its balance runs the ordinary path. Apps leave the setting off;
// fixed destinations, and windows whose generator has no probe, are
// unaffected.
//
// The state is per window and guarded by the window stateLock.
package connect

import (
	"context"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// An optional generator capability: ask the platform for one contract to the
// destination under the generator's own credential, to learn whether the
// payer's balance can carry a window contract again. A granted contract is
// closed unused. Safe for concurrent use.
type MultiClientGeneratorBalanceProbe interface {
	ProbeBalance(ctx context.Context, destinationId Id) BalanceProbeOutcome
}

// What one balance probe learned.
type BalanceProbeOutcome int

const (
	// the probe could not answer the balance question: the request failed,
	// or the platform refused it for something other than the balance
	BalanceProbeUnknown BalanceProbeOutcome = iota
	// the platform refused the contract for the payer's balance
	BalanceProbeShort
	// the platform asked the caller to slow down, which says nothing about
	// the balance
	BalanceProbeLimited
	// the platform granted the contract
	BalanceProbeRestored
	balanceProbeOutcomeCount
)

func (self BalanceProbeOutcome) String() string {
	switch self {
	case BalanceProbeShort:
		return "short"
	case BalanceProbeLimited:
		return "limited"
	case BalanceProbeRestored:
		return "restored"
	default:
		return "unknown"
	}
}

// Whether the outcome keeps a window held.
func (self BalanceProbeOutcome) holds() bool {
	return self == BalanceProbeShort || self == BalanceProbeLimited
}

// A window's balance hold.
type windowBalanceHold struct {
	held bool
	// the destination of the latest contract refused for the balance, which
	// the probe asks for
	destinationId Id
}

// The host's opt-in, on a discovery window whose generator can probe.
func (self *multiClientWindow) balanceHoldEnabled() bool {
	if self == nil || self.settings == nil || !self.settings.OutOfBalanceRefillProbe || self.generator == nil {
		return false
	}
	if _, fixed := self.generator.FixedDestinationSize(); fixed {
		return false
	}
	_, ok := self.generator.(MultiClientGeneratorBalanceProbe)
	return ok
}

// Holds the refill on a refusal for the payer's balance, and releases it on a
// granted contract. Any other result leaves the hold as it stood, as the sdk's
// device contract status does.
func (self *multiClientWindow) noteContractStatusForBalanceHold(status *ContractStatus) {
	if status == nil || !self.balanceHoldEnabled() {
		return
	}
	if status.Error == nil {
		self.releaseBalanceHold()
		return
	}
	if *status.Error != protocol.ContractError_InsufficientBalance {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.balanceHold.held = true
	self.balanceHold.destinationId = status.Key.Destination.DestinationId
}

// Ends the hold and wakes a waiting pass. The expansion backoff stands as the
// last step left it.
func (self *multiClientWindow) releaseBalanceHold() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !self.balanceHold.held {
		return
	}
	self.balanceHold = windowBalanceHold{}
	if self.resizeMonitor != nil {
		self.resizeMonitor.NotifyAll()
	}
}

// The destination a probe asks for, and whether the window is held.
func (self *multiClientWindow) balanceRefillHeld() (Id, bool) {
	if !self.balanceHoldEnabled() {
		return Id{}, false
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.balanceHold.destinationId, self.balanceHold.held
}

// Whether the enumerator must list and mint nothing for the window now.
func (self *multiClientWindow) balanceListingHeld() bool {
	_, held := self.balanceRefillHeld()
	return held
}

// One expansion step of a held window: one probe instead of candidates. True
// when the window stays held and the pass builds nothing; false when it was
// not held, or the probe released it and the pass expands now.
func (self *multiClientWindow) probeBalanceHold(windowClientStats *WindowClientStats) bool {
	destinationId, held := self.balanceRefillHeld()
	if !held {
		return false
	}
	windowClientStats.addExpandPass(self.windowType, windowExpandPassHeldInsufficientBalance)
	outcome := BalanceProbeUnknown
	if probe, ok := self.generator.(MultiClientGeneratorBalanceProbe); ok {
		outcome = probe.ProbeBalance(self.ctx, destinationId)
	}
	windowClientStats.addBalanceProbe(self.windowType, outcome)
	if self.log != nil && self.log.V(1).Enabled() {
		self.log.Infof("%s\n", relEvent(
			"balance_probe",
			"window", windowStatsWindowOf(self.windowType).String(),
			"outcome", outcome.String(),
		))
	}
	if outcome.holds() {
		// unproductive, as a failed expansion is, so the backoff advances the
		// same way
		self.noteDiscoveryExpandUnproductive(time.Now())
		return true
	}
	self.releaseBalanceHold()
	return false
}
