package connect

// ip_remote_multi_client_sticky.go — the user's Fixed IP (a "sticky" window).
//
// Every app maps its Fixed IP toggle to window min = max = 1, which the sdk
// turns into a fixed profile with FixedWindowSize 1. The user asked for one
// egress ip for the session, so a sticky window:
//   - holds no standing-reserve spare (standingReserveTarget): a spare is a
//     second selectable exit, and new flows could leave on its ip,
//   - never drains its exit on the lifetime clock (lifetimeDrainDue), which
//     also skips the drain's quic migration,
//   - dials the same provider again when its exit is lost to transport loss
//     rather than to a verdict (stickyRedial), so a provider that is still
//     online comes back with the same egress ip.
//
// The exit is then replaced only when it is lost: sustained unhealthy, a
// send-stall conviction, a blackhole verdict, or transport loss. Rotation in
// auto mode and in fixed profiles of any other size is unchanged.
//
// Threading: the predicate reads the window profile under the window
// stateLock. The pending re-dial has its own small lock and never takes the
// window lock, so it may be used from the resize pass, the enumerator and
// SetPerformanceProfile alike.

import (
	"errors"
	"sync"
	"time"
)

// stickyExitProfile reports whether a performance profile is the user's
// Fixed IP: a fixed window type sized to exactly one exit. It reads the
// profile, never settings.WindowSizes: auto mode's internal speed window is
// FixedWindowSize 1 too, and it keeps rotating as designed.
func stickyExitProfile(performanceProfile *PerformanceProfile) bool {
	_, windowSize, ok := performanceProfile.FixedWindow()
	return ok && windowSize.FixedWindowSize == 1
}

// stickyExit reports whether this window holds the user's Fixed IP. See
// stickyExitProfile.
func (self *multiClientWindow) stickyExit() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return stickyExitProfile(self.performanceProfile)
}

// lifetimeDrainDue reports whether a healthy client is past its removeTime
// and is drain-warned this pass. Draining is rotation policy, not a health
// verdict (see the drain branch in resize). A sticky window never drains on
// the clock: the user asked for a stable egress ip, so its exit is replaced
// only when it is lost. A zero removeTime (MaxClientLifetime 0, rotation
// disabled) never drains either.
func lifetimeDrainDue(stats *clientWindowStats, now time.Time, stickyExit bool) bool {
	return !stickyExit && !stats.removeTime.IsZero() && stats.removeTime.Before(now)
}

// stickyRedial is the exit a sticky window lost to transport loss: the
// window's own route to the provider stayed down for the whole migration
// grace (errTransportDownTimeout), which says nothing against the provider.
// The next discovery round asks the platform for that provider first (see
// enumerateStickyRedial). A verdict against the provider (blackhole, send
// stall, sustained unhealthy) is never remembered: those exits are replaced
// by discovery as before.
//
// One pending exit at most. The zero value is ready, so bare test windows
// need no setup.
type stickyRedial struct {
	stateLock   sync.Mutex
	destination MultiHopId
	stats       DestinationStats
	pending     bool
}

// Remember holds a lost exit for the next discovery round, replacing any
// exit already held.
func (self *stickyRedial) Remember(destination MultiHopId, stats DestinationStats) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.destination = destination
	self.stats = stats
	self.pending = true
}

// Take hands the held exit to one discovery round and clears it, so an exit
// that does not come back costs one platform round trip, not a retry loop.
func (self *stickyRedial) Take() (destination MultiHopId, stats DestinationStats, ok bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !self.pending {
		return
	}
	destination, stats, ok = self.destination, self.stats, true
	self.destination = MultiHopId{}
	self.stats = DestinationStats{}
	self.pending = false
	return
}

// Forget drops the held exit: the user changed the profile or asked for new
// exits, and either way the old exit is no longer what they want back.
func (self *stickyRedial) Forget() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.destination = MultiHopId{}
	self.stats = DestinationStats{}
	self.pending = false
}

// rememberStickyRedial records an exit the resize pass removes, when the
// window is sticky and the removal is transport loss rather than a verdict.
// Reports whether it was remembered.
func (self *multiClientWindow) rememberStickyRedial(client *multiClientChannel, err error, stickyExit bool) bool {
	if !stickyExit || !errors.Is(err, errTransportDownTimeout) || client.args == nil {
		return false
	}
	self.stickyRedial.Remember(client.args.Destination, client.args.DestinationStats)
	loggerOrDefault(self.log).Infof("%s\n", relEvent(
		"sticky_redial",
		"exit", client.ClientId(),
		"provider", client.args.Destination.Tail(),
		"action", "remember",
	))
	return true
}

// enumerateStickyRedial runs ahead of a discovery round: when the window
// holds a lost exit (stickyRedial) it asks the generator for that provider by
// client id, and ok reports a round of that one destination. The platform
// still applies its exclusions to a named provider, so an answer without it
// falls back to discovery in the same round. A platform that cannot be
// reached keeps the exit pending for the enumerator's retry, which repeats
// the round. The destination the window held is dialed with the stats it was
// discovered with: a named answer carries no location or address family,
// and the egress is the provider either way.
func (self *multiClientWindow) enumerateStickyRedial(
	excludeDestinations []MultiHopId,
	rankMode string,
) (redial enumeratedDestination, ok bool, err error) {
	destination, stats, pending := self.stickyRedial.Take()
	if !pending || !self.stickyExit() {
		return
	}
	if _, fixed := self.generator.FixedDestinationSize(); fixed {
		// a fixed destination set already names its providers
		return
	}
	clientIdGenerator, capable := self.generator.(MultiClientGeneratorWithClientId)
	if !capable {
		return
	}
	destinations, err := windowGeneratorCall(
		self.ctx,
		self.settings.WindowGeneratorTimeout,
		func() (map[MultiHopId]DestinationStats, error) {
			return clientIdGenerator.NextDestinationsForClientId(
				destination.Tail(),
				excludeDestinations,
				rankMode,
			)
		},
		nil,
	)
	if err != nil {
		self.stickyRedial.Remember(destination, stats)
		return enumeratedDestination{}, false, err
	}
	for candidate := range destinations {
		if candidate.Tail() == destination.Tail() {
			loggerOrDefault(self.log).Infof("%s\n", relEvent(
				"sticky_redial",
				"provider", destination.Tail(),
				"action", "dial",
			))
			return enumeratedDestination{
				destination:  destination,
				stats:        stats,
				stickyRedial: true,
			}, true, nil
		}
	}
	loggerOrDefault(self.log).Infof("%s\n", relEvent(
		"sticky_redial",
		"provider", destination.Tail(),
		"action", "unavailable",
	))
	return enumeratedDestination{}, false, nil
}
