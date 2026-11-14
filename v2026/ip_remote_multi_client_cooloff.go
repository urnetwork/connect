package connect

// ip_remote_multi_client_cooloff.go — the providers a window just found
// failing, left out of its discovery for a while.
//
// Discovery excludes the healthy exits already in the window and a service
// refresh's refusals. A candidate that failed its evaluation, and an exit a
// verdict removed, were eligible again on the next round, so in a pool whose
// few providers do not answer the window drew the same ones round after
// round, each costing a contract, an identity and an evaluation of up to the
// ping timeout before it failed again. The window now notes each provider it
// found failing and passes it to discovery as an exclusion until
// MultiClientSettings.EvaluationFailureCooloff has passed; non-positive
// disables the cool-off. Discovery runs ahead of the expansion, so a candidate
// listed before its provider failed is also refused when the expansion takes
// it (acceptDiscoveryDemandArgs, cooloffRefuses). Each candidate carries the
// listing of the round that drew it (cooloffListing): whether that round
// skipped the exclusions, and the last note it had seen.
//
// What is noted:
//   - a candidate whose evaluation failed for the provider: its channel could
//     not be created, or its evaluation ping went unanswered or errored, and
//     the window counted it a provider failure (ProviderStateEvaluationFailed);
//   - an exit the resize pass removed with a verdict against the provider (a
//     blackhole verdict or a send-stall conviction) that the recent-exit
//     memory did not keep as an outage of ours, and only while another exit
//     is receiving (verdictJudgeable): with no receiving sibling, in a one-exit
//     window or with the uplink stale past the gate's hold, the silence may be
//     ours.
//
// Never noted: a candidate the pass or the window canceled, a local contract
// failure, a platform setup failure, a transport loss, a close or a user
// action, the user's Fixed IP exit, and fixed destination sets, which their
// own retry backoff governs (FixedDestinationRetryMin).
//
// Every note leaves the provider out of every discovery request. The
// evaluation is a control-plane ping to the provider and its construction a
// contract and transport setup, neither of which involves the provider's
// egress families, so a failure while the window fills an IPv6 shortfall is
// as much a failure for the v4-capable fill.
//
// The starvation escape. A pool of one to three providers can be emptied by a
// cool-off, and a transient failure of ours (a ping lost to the uplink) is
// enough to cause it. So the exclusions are skipped for a discovery round
// when the window holds no provider at all (no live exit that takes new flows
// or is kept from them only because it is full, holdsWindowForCooloff), and
// for the round after one whose exclusions left discovery empty (a round that
// asked only for v6-capable providers does not count,
// cooloffRoundAsksThePool). A candidate such a round lists after its
// provider failed was listed knowingly, and the expansion evaluates it: the
// refusal is only for a candidate listed before its provider failed, or one a
// round returned despite excluding it. The entries stay, so the next round
// that applies the exclusions leaves the providers out again. The window's
// silent rebuild (the outcome watchdog's programmatic reconnect) clears the
// cool-off, as a reconnect starts with none; the watchdog acts only on a
// window that never formed, and the escape is what serves the rest. A network
// change clears it too, as it starts the expansion backoff over: failures on
// the old path say nothing about the new one.
//
// Bounds: one entry per provider, at most cooloffMaxCount, the oldest
// dropped first; an entry past the cool-off is dropped when discovery reads
// the exclusions. The memory has its own lock and never takes the window's.
//
// Visibility: one relEvent line per action ("cooloff", action
// note/refuse/skip/clear, and exclude with the count whenever it changes),
// naming providers by client id.

import (
	"strings"
	"sync"
	"time"
)

// The most providers one window keeps in its cool-off. The exclusions ride
// along on every discovery request, and the platform widens its candidate
// read by the exclusions it is sent.
const cooloffMaxCount = 64

// Why a provider was noted: the reason value of its log line.
const (
	cooloffReasonEvaluation = "evaluation"
	cooloffReasonVerdict    = "verdict"
)

type cooloffEntry struct {
	failTime time.Time
	// the note's place in the cool-off's sequence of notes
	generation uint64
}

// How a discovery round read the cool-off: the last note it had seen, and
// whether it skipped the exclusions. The zero value is a listing that applied
// them.
type cooloffListing struct {
	generation uint64
	skipped    bool
}

type destinationCooloff struct {
	stateLock sync.Mutex
	// when each provider last failed
	entries map[MultiHopId]cooloffEntry
	// the notes so far, never reset, so a listing can tell an entry noted
	// after it from one it saw
	generation uint64
	// the last round whose exclusions applied came back empty: the next round
	// runs without them
	skipNext bool
	// the excluded count last logged, so the line is written on a change
	loggedExcludedCount int
}

// Notes that destination failed at failTime, keeping at most maxCount
// providers, the newest.
func (self *destinationCooloff) note(destination MultiHopId, failTime time.Time, maxCount int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.entries == nil {
		self.entries = map[MultiHopId]cooloffEntry{}
	}
	self.generation += 1
	self.entries[destination] = cooloffEntry{failTime: failTime, generation: self.generation}
	for maxCount < len(self.entries) {
		var oldest MultiHopId
		var oldestTime time.Time
		for candidate, entry := range self.entries {
			if oldestTime.IsZero() || entry.failTime.Before(oldestTime) {
				oldest, oldestTime = candidate, entry.failTime
			}
		}
		delete(self.entries, oldest)
	}
}

// Adds each provider that failed within timeout of now to excluded, drops the
// expired, and returns how many it added.
func (self *destinationCooloff) addExclusions(excluded map[MultiHopId]bool, now time.Time, timeout time.Duration) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.addExclusionsWithLock(excluded, now, timeout)
}

func (self *destinationCooloff) addExclusionsWithLock(excluded map[MultiHopId]bool, now time.Time, timeout time.Duration) int {
	count := 0
	for destination, entry := range self.entries {
		switch {
		case timeout <= now.Sub(entry.failTime):
			delete(self.entries, destination)
		case !excluded[destination]:
			excluded[destination] = true
			count += 1
		}
	}
	return count
}

// Drops the entries past timeout at now.
func (self *destinationCooloff) expireWithLock(now time.Time, timeout time.Duration) {
	for destination, entry := range self.entries {
		if timeout <= now.Sub(entry.failTime) {
			delete(self.entries, destination)
		}
	}
}

// Whether a candidate for destination that listing drew is refused at now:
// its provider failed within timeout, and either after the listing read the
// cool-off, or the listing applied the exclusions, so the provider was left
// out of the request that still returned it. A candidate a skipping round
// listed after its provider failed is not refused.
func (self *destinationCooloff) refuses(destination MultiHopId, listing cooloffListing, now time.Time, timeout time.Duration) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	entry, ok := self.entries[destination]
	if !ok || timeout <= now.Sub(entry.failTime) {
		return false
	}
	return !listing.skipped || listing.generation < entry.generation
}

// Drops every entry and the pending skip, and returns how many entries it
// dropped.
func (self *destinationCooloff) clear() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	count := len(self.entries)
	self.entries = nil
	self.skipNext = false
	self.loggedExcludedCount = 0
	return count
}

// The window's cool-off; non-positive is off.
func (self *multiClientWindow) cooloffTimeout() time.Duration {
	if self.settings == nil {
		return 0
	}
	return self.settings.EvaluationFailureCooloff
}

// Notes a provider this window found failing, for reason. See the file
// comment for which failures call this.
func (self *multiClientWindow) noteFailedDestination(destination MultiHopId, reason string) {
	if self.cooloffTimeout() <= 0 || destination.Len() == 0 {
		return
	}
	if self.generator != nil {
		if _, fixed := self.generator.FixedDestinationSize(); fixed {
			return
		}
	}
	self.cooloff.note(destination, time.Now(), cooloffMaxCount)
	loggerOrDefault(self.log).Infof("%s\n", relEvent(
		"cooloff",
		"window", self.windowName(),
		"provider", destination.Tail(),
		"action", "note",
		"reason", reason,
		"for", self.cooloffTimeout(),
	))
}

// Whether the expansion refuses a candidate for destination that listing
// drew: one listed before its provider failed in this window, or returned
// despite the exclusions (destinationCooloff.refuses). Logs the refusal.
func (self *multiClientWindow) cooloffRefuses(destination MultiHopId, listing cooloffListing) bool {
	timeout := self.cooloffTimeout()
	if timeout <= 0 || !self.cooloff.refuses(destination, listing, time.Now(), timeout) {
		return false
	}
	loggerOrDefault(self.log).Infof("%s\n", relEvent(
		"cooloff",
		"window", self.windowName(),
		"provider", destination.Tail(),
		"action", "refuse",
	))
	return true
}

// Adds the providers still cooling off to a discovery round's exclusions,
// unless the starvation escape skips them: the window holds no provider
// (heldCount 0), or the last round with exclusions came back empty. Only a
// round that asks the pool (asksThePool, cooloffRoundAsksThePool) spends that
// skip: a probe or a family-filtered IPv6 demand read landing between an
// empty round and the next ordinary one applies the exclusions and leaves the
// skip for it. Returns how many it added, and the listing the round's
// candidates carry to the expansion's refusal (cooloffRefuses); the round
// reports its result to noteCooloffRound.
func (self *multiClientWindow) addCooloffExclusions(excluded map[MultiHopId]bool, heldCount int, asksThePool bool) (int, cooloffListing) {
	timeout := self.cooloffTimeout()
	if timeout <= 0 {
		return 0, cooloffListing{}
	}
	now := time.Now()
	skipReason := ""
	count, changed, pending, listing := func() (int, bool, int, cooloffListing) {
		self.cooloff.stateLock.Lock()
		defer self.cooloff.stateLock.Unlock()
		if heldCount == 0 {
			skipReason = "empty_window"
		} else if asksThePool && self.cooloff.skipNext {
			skipReason = "empty_round"
		}
		if asksThePool {
			self.cooloff.skipNext = false
		}
		listing := cooloffListing{generation: self.cooloff.generation, skipped: skipReason != ""}
		if skipReason != "" {
			// expire what is due, and keep the rest for the next round
			self.cooloff.expireWithLock(now, timeout)
			return 0, false, len(self.cooloff.entries), listing
		}
		count := self.cooloff.addExclusionsWithLock(excluded, now, timeout)
		changed := count != self.cooloff.loggedExcludedCount
		self.cooloff.loggedExcludedCount = count
		return count, changed, 0, listing
	}()
	if skipReason != "" {
		if 0 < pending {
			loggerOrDefault(self.log).Infof("%s\n", relEvent(
				"cooloff",
				"window", self.windowName(),
				"action", "skip",
				"reason", skipReason,
				"held", pending,
			))
		}
		return 0, listing
	}
	if changed {
		loggerOrDefault(self.log).Infof("%s\n", relEvent(
			"cooloff",
			"window", self.windowName(),
			"action", "exclude",
			"count", count,
		))
	}
	return count, listing
}

// Whether a discovery round's empty answer says the pool has nothing left
// outside the exclusions, so the starvation escape may skip them next round.
// Not for a probe, which asks only the v6-capable request, nor for a demand
// read for an IPv6 flow, which asks a family-aware generator for one
// v6-capable candidate: a pool of providers that carry only IPv4 answers
// either with nothing.
func (self *multiClientWindow) cooloffRoundAsksThePool(probeOnly bool, demandRead *discoveryDemandRead) bool {
	if probeOnly {
		return false
	}
	if demandRead != nil && demandRead.need.ipVersion == 6 {
		// listDiscoveryDemand filters by family only with these capabilities
		_, familyAware := self.generator.(multiClientGeneratorFamilyDiscoveryContext)
		_, capabilityAware := self.generator.(MultiClientGeneratorWithCapabilities)
		return !familyAware && !capabilityAware
	}
	return true
}

// Reports a discovery round's result: a round whose cool-off exclusions
// applied and that came back empty makes the next round run without them.
func (self *multiClientWindow) noteCooloffRound(excludedCount int, empty bool) {
	if excludedCount <= 0 || !empty {
		return
	}
	self.cooloff.stateLock.Lock()
	defer self.cooloff.stateLock.Unlock()
	self.cooloff.skipNext = true
}

// Clears the cool-off for the window's silent rebuild, which is the
// programmatic form of a reconnect (a reconnect starts with no cool-off), and
// for a network change, after which the failures say nothing of the path.
func (self *multiClientWindow) clearCooloff(reason string) {
	if count := self.cooloff.clear(); 0 < count {
		loggerOrDefault(self.log).Infof("%s\n", relEvent(
			"cooloff",
			"window", self.windowName(),
			"action", "clear",
			"reason", reason,
			"count", count,
		))
	}
}

// Whether the exit holds its window for the starvation escape: it is live,
// and it takes new flows or is kept from them only because it is full (the
// window's source ulimit, warnCapacity), which a hosted proxy's profile can
// set. A full window is not starving, and skipping the cool-off there would
// draw its failed providers again on every expansion. An exit warned for
// anything else is on its way out, as is one that is done but not yet reaped.
func (self *multiClientChannel) holdsWindowForCooloff() bool {
	if self.IsDone() {
		return false
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.quarantined || self.serviceRefreshReservation.Load() != nil {
		return false
	}
	return !self.warning || self.warnCause == warnCapacity
}

// Whether the window can judge a verdict against client as the provider's:
// another exit is receiving right now, so the uplink demonstrably delivers.
// With no receiving sibling (a one-exit window, or an uplink stale past the
// gate's hold, where the gate no longer vouches either way) the silence may be
// ours, and the cool-off must not hold it against the provider.
func (self *multiClientWindow) verdictJudgeable(client *multiClientChannel) bool {
	return self.receivingSiblingsFunc != nil && 0 < self.receivingSiblingsFunc(client)
}

// Whether a removal's error is a verdict against the provider: a blackhole
// verdict, or a send-stall conviction, which is admitted only while a sibling
// exit receives, or a receive gap eviction, which is admitted only while the
// tunnel receives (ip_remote_multi_client_exit_stall.go).
func providerVerdictErr(err error) bool {
	return blackholeVerdictErr(err) || err != nil && strings.HasPrefix(err.Error(), "send stalled") || receiveGapTimeoutErr(err)
}
