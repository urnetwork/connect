package connect

// transport_move.go — how one platform transport takes part in a move
// (resident_move.go, server/connect/MOVE.md §5.2).
//
// Each H1 and plain H3 dial attempt asks for a move dial attempt first. A
// move's replacement gets its move's targets of the transport's family: a
// strict (advisory) move on every attempt until the owner concludes the move,
// and a compulsory move on the first attempt of each carrier. Otherwise a
// preference an earlier move left is tried, and otherwise the edge the client's
// placement chose, with or without moves (platform_placement.go). The targets
// reach the dial layers the transport does not own through the dial context
// (`moveDialOverrideContextKey`), the same route the pinned family takes:
// `ConnectSettings.DialContext` dials the target literals in place of the
// hostname, so the TLS server name and the HTTP host, both decided above that
// layer from the platform url, are unchanged; and the H3 candidates are the
// target addresses on the platform port, with the server name unchanged.
//
// A move redirects only the platform host's edges. A carrier an alt url
// redirects -- plain H3, and the DNS carriers, on every space with an alt
// deployment (EXTENDER.md L1, L4) -- keeps dialing alt: neither a move's
// targets nor a preference apply to it. A strict move's replacement parks such
// a carrier, like a DNS carrier or H3 through an extender, so it arrives at the
// target over H1, and the carrier dials alt again once the move concludes. A
// compulsory move's replacement dials alt by name. Only H1, and plain H3 on a
// space with no alt url, can use a target.
//
// The outcome of a dial that used targets is reported back: an arrival (the
// connection registered and no relay carried it) to the move's dial, which also
// records the preference; a failure, with its typed reason and how long the
// dial took, to the move's dial, or for a preference, by dropping the
// preference so the next attempt resolves the name.
//
// Concurrency: the attempt is owned by one mode runner iteration; its typed
// error collector is safe for the strategy's racing dialers.

import (
	"context"
	"net/netip"
	"slices"
	"sync/atomic"
	"time"
)

// One dial attempt that stands move targets in for the platform host.
type platformMoveDialAttempt struct {
	override *moveDialOverride
	// the move whose replacement dials, nil for a preference or a placement
	dial *ResidentMoveDial
	// the targets are the client's placement (platform_placement.go)
	placement bool
	startTime time.Time
	// set by the dial layer that applied the override
	used          atomic.Bool
	attemptErrors residentMoveAttemptErrors
}

// The targets one dial attempt uses in place of resolving the platform host,
// nil to resolve it as usual: with moves enabled, a move's targets, then a
// move's preference; then, with or without moves, the client's placement
// (platform_placement.go). `targetAttempted` is whether this carrier already
// made its one attempt for a move that is not strict. Only the H1 and plain H3
// carriers ask.
func (self *PlatformTransport) moveDialAttempt(targetAttempted bool) *platformMoveDialAttempt {
	if !self.settings.EnableMove && self.settings.Placement == nil {
		return nil
	}
	host, err := connectHost(self.platformUrl)
	if err != nil || host == "" {
		return nil
	}
	now := time.Now()
	if self.settings.EnableMove {
		if targetIps, strict, ok := self.residentMoveDial.targetIpsFor(self.ipFamily); ok && (strict || !targetAttempted) {
			return &platformMoveDialAttempt{
				override: &moveDialOverride{
					host:      host,
					targetIps: targetIps,
					strict:    strict,
				},
				dial:      self.residentMoveDial,
				startTime: now,
			}
		}
		if targetIps := self.residentMoveTracker.preferredTargetIps(host, self.ipFamily, now); 0 < len(targetIps) {
			return &platformMoveDialAttempt{
				override: &moveDialOverride{
					host:      host,
					targetIps: targetIps,
				},
				startTime: now,
			}
		}
	}
	if targetIps := self.placementTargetIps(host, now); 0 < len(targetIps) {
		return &platformMoveDialAttempt{
			override: &moveDialOverride{
				host:      host,
				targetIps: targetIps,
			},
			placement: true,
			startTime: now,
		}
	}
	return nil
}

// The placed addresses of this transport's family for a dial of `host`: the
// client's kept placement, else the placement's answer less the addresses held
// after a failed dial, which the client then keeps for every family offered
// for `PlacementPreferDuration`. None without a placement or an address to
// dial, which resolves the name.
func (self *PlatformTransport) placementTargetIps(host string, now time.Time) []netip.Addr {
	placement := self.settings.Placement
	if placement == nil {
		return nil
	}
	if targetIps := self.residentMoveTracker.placedTargetIps(host, self.ipFamily, now); 0 < len(targetIps) {
		return targetIps
	}
	offeredIps := []netip.Addr{}
	for _, targetIp := range placement.PlatformPlacementTargets(host) {
		targetIp = targetIp.Unmap()
		if !targetIp.IsValid() || targetIp.IsUnspecified() {
			continue
		}
		if self.residentMoveTracker.TargetFailedWithin(targetIp, self.settings.PlacementFailHold, now) {
			continue
		}
		offeredIps = append(offeredIps, targetIp)
	}
	if len(offeredIps) == 0 {
		return nil
	}
	if 0 < self.settings.PlacementPreferDuration {
		self.residentMoveTracker.placeTargets(host, offeredIps, now.Add(self.settings.PlacementPreferDuration))
	}
	targetIps := []netip.Addr{}
	for _, targetIp := range offeredIps {
		if self.ipFamily == 0 || netipAddrFamily(targetIp) == self.ipFamily {
			targetIps = append(targetIps, targetIp)
		}
	}
	return orderDialAddrs(targetIps)
}

// The dial context of the attempt: the override for the dial layers, and for a
// move an observer of each typed attempt error, chained after any observer the
// context already carries (a pinned transport's).
func (self *platformMoveDialAttempt) withContext(ctx context.Context) context.Context {
	if self == nil {
		return ctx
	}
	ctx = withMoveDialOverride(ctx, self.override)
	ctx = context.WithValue(ctx, platformMoveDialAttemptContextKey{}, self)
	if self.dial == nil {
		return ctx
	}
	previous, _ := ctx.Value(dialAttemptObserverContextKey{}).(func(error))
	return withDialAttemptObserver(ctx, func(err error) {
		if previous != nil {
			previous(err)
		}
		self.attemptErrors.observe(err)
	})
}

// Carries the attempt so the layer that applies the override can mark it used.
type platformMoveDialAttemptContextKey struct{}

// Marks the attempt in `ctx`, if any, as having dialed its targets.
func noteMoveDialOverrideUsed(ctx context.Context) {
	if attempt, ok := ctx.Value(platformMoveDialAttemptContextKey{}).(*platformMoveDialAttempt); ok && attempt != nil {
		attempt.used.Store(true)
	}
}

// Whether the attempt dialed its targets: a layer applied the override.
func (self *platformMoveDialAttempt) dialedTargets() bool {
	return self != nil && self.used.Load()
}

// Reports a failed attempt that dialed its targets. A move's dial counts it (in
// `mode`) and a preference is dropped, so the next attempt resolves the name. A
// placement is dropped, its addresses held for `PlacementFailHold` and the
// placement told, so the next attempt asks again and resolves the name when
// every address offered is held. A dial canceled by the transport's own
// teardown or a network change is no failure of the target; the caller does
// not report it.
func (self *PlatformTransport) noteMoveDialFailed(
	attempt *platformMoveDialAttempt,
	mode TransportMode,
	err error,
) {
	if !attempt.dialedTargets() {
		return
	}
	now := time.Now()
	if attempt.placement {
		self.residentMoveTracker.dropPlacedTargets(attempt.override.host, attempt.override.targetIps)
		for _, targetIp := range attempt.override.targetIps {
			self.residentMoveTracker.NoteTargetFailed(targetIp, now)
		}
		if placement := self.settings.Placement; placement != nil {
			placement.PlatformPlacementFailed(attempt.override.host, slices.Clone(attempt.override.targetIps))
		}
		return
	}
	if attempt.dial == nil {
		self.residentMoveTracker.dropPreferredTargets(attempt.override.host, attempt.override.targetIps)
		return
	}
	attempt.dial.noteFailed(
		self.ipFamily,
		mode,
		attempt.override.targetIps,
		attempt.attemptErrors.failureReason(err),
		now.Sub(attempt.startTime),
		now,
	)
}

// Reports a registered connection whose attempt dialed its targets. A
// connection a relay carried did not reach the target and is no arrival.
func (self *PlatformTransport) noteMoveDialArrived(attempt *platformMoveDialAttempt, relayed bool) {
	if !attempt.dialedTargets() || relayed || attempt.dial == nil {
		return
	}
	attempt.dial.noteArrived(attempt.override.host, attempt.override.targetIps, time.Now())
}

// Parks a mode runner whose carrier cannot use a target -- a DNS carrier, an
// H3 carrier an alt url redirects, or one an extender relays -- while this
// transport is a strict move's replacement. Any connection a strict
// replacement makes must be an arrival at the target. False means the context
// ended.
func (self *PlatformTransport) waitMoveCarrierAdmission(ctx context.Context, mode TransportMode) bool {
	canUseTarget := func() bool {
		switch mode {
		case TransportModeH1:
			return true
		case TransportModeH3:
			if self.h3AltRedirected() {
				return false
			}
			return self.clientStrategy == nil || self.clientStrategy.H3ExtenderConfig() == nil
		default:
			return false
		}
	}
	for {
		held, notify := self.residentMoveDial.holdsCarrier(self.ipFamily)
		if !held || canUseTarget() {
			return true
		}
		if self.settings.moveCarrierHoldForTest != nil {
			self.settings.moveCarrierHoldForTest(mode)
		}
		select {
		case <-ctx.Done():
			return false
		case <-notify:
		}
	}
}

// Whether an alt url redirects this transport's plain H3 carrier to the alt
// host (EXTENDER.md L4). A move redirects only the platform host's edges, so
// such a carrier dials neither a move's target nor a preference.
func (self *PlatformTransport) h3AltRedirected() bool {
	altHost, _ := altUrlHostPort(self.settings.AltUrl)
	return altHost != ""
}

// Counts a live connection a relay carries for exactly its lifetime. The
// returned release is idempotent.
func (self *PlatformTransport) holdRelayedConnection(relayed bool) func() {
	if !relayed {
		return func() {}
	}
	self.relayedConnectionCount.Add(1)
	var released atomic.Bool
	return func() {
		if released.CompareAndSwap(false, true) {
			self.relayedConnectionCount.Add(-1)
		}
	}
}

// The elected carrier mode, TransportModeNone while nothing is connected.
func (self *PlatformTransport) ActiveMode() TransportMode {
	return self.mode.Value()
}

// Whether a live connection reaches the platform through a relay -- an extender
// or a VLESS server -- that resolves the platform name itself, which a move
// cannot redirect.
func (self *PlatformTransport) RelayCarried() bool {
	return 0 < self.relayedConnectionCount.Load()
}

// The declared move version: `ResidentMoveVersion` while moves are enabled,
// else zero.
func (self *PlatformTransport) residentMoveVersion() uint32 {
	if self.settings.EnableMove {
		return ResidentMoveVersion
	}
	return 0
}
