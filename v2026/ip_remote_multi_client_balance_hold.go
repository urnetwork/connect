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
//   - A probe that finds the balance restored releases the hold. The same pass
//     expands at once, unless the device is idle (below).
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
// The restore spread (2026-11-05). One balance grant restores every held
// window of its payer, and the daily free grant (server
// RefreshFreeTransferBalances, valid from 00:00 UTC) restores those of every
// free payer that ran dry, each at its next probe: within one expansion
// backoff, at most DiscoveryExpandRetryMax apart. Each then expanded in the
// pass that found the balance, with a listing, mints, dials, pings and
// contract waits for every missing client. On Main from 00:05Z on 2026-11-05
// that took both proxy hosts from 16 to 31 cores in ten minutes, and the dial
// failures of the saturated hosts kept them pinned for hours. So, with
// BalanceRestoreSpread:
//   - A window restored, by its probe or by a granted contract, whose device
//     sent no user packet in the last BalanceRestoreSpreadIdle waits a fixed
//     offset in [0, BalanceRestoreSpread), a pure function of the window's
//     key (balanceRestoreOffset), before it expands, so the restores of many
//     windows spread evenly over the interval. Its enumerator lists and mints
//     nothing while it waits.
//   - A restored window whose device sent a user packet within that time
//     expands in the same pass, as before.
//   - The device's first user packet during the wait ends it: the send path
//     wakes the resize pass, which expands at once.
//   - A refusal for the balance during the wait holds the window again, and
//     the hold, not the wait, decides its next step.
//
// So a user who is sending sees exactly the old timing. A user who starts
// sending during the wait gets an expansion that starts on their first packet,
// where the old window had already expanded, if it finished, by the time they
// came back. Their wait is that one expansion, which is exactly what they wait
// today when they arrive while it runs.
//
// The state is per window and guarded by the window stateLock.
package connect

import (
	"context"
	"hash/fnv"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
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
		// a contract the balance carries again is a restore, and an idle
		// device's window waits its offset as after a restored probe
		if self.releaseBalanceHold() {
			self.deferBalanceRestore(time.Now())
		}
		return
	}
	if *status.Error != protocol.ContractError_InsufficientBalance {
		return
	}
	self.balanceRestoreWake.Store(false)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.balanceHold.held = true
	self.balanceHold.destinationId = status.Key.Destination.DestinationId
	// the balance is short again: the hold decides the next step, not a
	// restore wait
	self.balanceRestore = windowBalanceRestore{}
}

// Ends the hold and wakes a waiting pass. The expansion backoff stands as the
// last step left it. False when the window was not held.
func (self *multiClientWindow) releaseBalanceHold() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !self.balanceHold.held {
		return false
	}
	self.balanceHold = windowBalanceHold{}
	if self.resizeMonitor != nil {
		self.resizeMonitor.NotifyAll()
	}
	return true
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

// Whether the enumerator must list and mint nothing for the window now: it is
// held, or its restored refill waits.
func (self *multiClientWindow) balanceListingHeld() bool {
	if _, held := self.balanceRefillHeld(); held {
		return true
	}
	if !self.balanceHoldEnabled() {
		return false
	}
	_, waiting := self.balanceRestoreDeferral(time.Now())
	return waiting
}

// One expansion step of a held window: one probe instead of candidates. True
// when the pass builds nothing: the window stays held, or the probe found the
// balance restored and the idle device's window waits out its spread offset.
// False when it was not held, or the probe released it and the pass expands
// now.
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
	// a probe that could not answer says nothing about a grant, and falls
	// back to the ordinary expansion at once
	return outcome == BalanceProbeRestored && self.deferBalanceRestore(time.Now())
}

// A restored window's wait. Guarded by the window stateLock.
type windowBalanceRestore struct {
	// when the probe found the balance restored: a user packet sent at or
	// after it ends the wait
	since time.Time
	// when the window expands without one; zero when it does not wait
	notBefore time.Time
}

// The spread offset of the window with `key`: a pure function of the key,
// uniform over [0, spread) for random keys. Nonpositive spread is 0.
func balanceRestoreOffset(key Id, spread time.Duration) time.Duration {
	if spread <= 0 {
		return 0
	}
	hash := fnv.New64a()
	hash.Write(key.Bytes())
	return time.Duration(hash.Sum64() % uint64(spread))
}

// Whether the window's device sent a user packet at or after `since`. A window
// without a parent (a bare fixture) reads as sending, so its refill never
// waits.
func (self *multiClientWindow) userSentSince(since time.Time) bool {
	if self.userSentSinceFunc == nil {
		return true
	}
	return self.userSentSinceFunc(since)
}

// After the hold ended at `now` because the balance carries contracts again:
// true when the window waits out its spread offset before it expands, false
// when it expands now. A device that sent a user packet within
// BalanceRestoreSpreadIdle, and a window whose offset is zero, expand now.
func (self *multiClientWindow) deferBalanceRestore(now time.Time) bool {
	spread := self.settings.BalanceRestoreSpread
	if spread <= 0 || self.userSentSince(now.Add(-self.settings.BalanceRestoreSpreadIdle)) {
		return false
	}
	offset := balanceRestoreOffset(self.balanceRestoreKey, spread)
	if offset <= 0 {
		return false
	}
	restore := windowBalanceRestore{since: now, notBefore: now.Add(offset)}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.balanceRestore = restore
		// the pass that ends the hold re-reads the wait before it builds
		if self.resizeMonitor != nil {
			self.resizeMonitor.NotifyAll()
		}
	}()
	// armed after the state is set: from here every user send stores its stamp
	// and wakes the pass
	self.balanceRestoreWake.Store(true)
	// a packet stamped before the wake was armed still ends the wait
	if self.userSentSince(now) {
		self.endBalanceRestore(restore)
		return false
	}
	if self.log != nil && self.log.V(1).Enabled() {
		self.log.Infof("%s\n", relEvent(
			"balance_restore_spread",
			"window", windowStatsWindowOf(self.windowType).String(),
			"offset", offset,
		))
	}
	return true
}

// Whether a restored window still waits at `now`, and until when. The offset
// passing, or a user packet since the restore, ends the wait.
func (self *multiClientWindow) balanceRestoreDeferral(now time.Time) (time.Time, bool) {
	var restore windowBalanceRestore
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		restore = self.balanceRestore
	}()
	if restore.notBefore.IsZero() {
		return time.Time{}, false
	}
	// the parent's stamp is read with no window lock held
	if !now.Before(restore.notBefore) || self.userSentSince(restore.since) {
		self.endBalanceRestore(restore)
		return time.Time{}, false
	}
	return restore.notBefore, true
}

// Ends the restored window's wait `restore`, if it is still the window's, and
// wakes its resize pass and enumerator. A wait set since it was read stands.
func (self *multiClientWindow) endBalanceRestore(restore windowBalanceRestore) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.balanceRestore.notBefore.IsZero() || self.balanceRestore != restore {
		return
	}
	self.balanceRestore = windowBalanceRestore{}
	self.balanceRestoreWake.Store(false)
	if self.resizeMonitor != nil {
		self.resizeMonitor.NotifyAll()
	}
}

// The parent's send path, after it stamped a user packet: a waiting window's
// resize pass wakes, finds the packet, and expands. Lock-free while nothing
// waits.
func (self *multiClientWindow) wakeBalanceRestore() {
	if self.balanceRestoreWake.CompareAndSwap(true, false) && self.resizeMonitor != nil {
		self.resizeMonitor.NotifyAll()
	}
}
