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
// it (acceptDiscoveryDemandArgs, cooloffHolds).
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
// when the window holds no provider at all, and for the round after one whose
// exclusions left discovery empty. The window's silent rebuild (the outcome
// watchdog's programmatic reconnect) clears the cool-off, as a reconnect
// starts with none.
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
}

type destinationCooloff struct {
	stateLock sync.Mutex
	// when each provider last failed
	entries map[MultiHopId]cooloffEntry
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
	self.entries[destination] = cooloffEntry{failTime: failTime}
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

// Whether destination failed within timeout of now.
func (self *destinationCooloff) holds(destination MultiHopId, now time.Time, timeout time.Duration) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	entry, ok := self.entries[destination]
	return ok && now.Sub(entry.failTime) < timeout
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

// Whether destination is cooling off now: a candidate
// listed before its provider failed in this window is refused when the
// expansion takes it. Logs the refusal.
func (self *multiClientWindow) cooloffHolds(destination MultiHopId) bool {
	timeout := self.cooloffTimeout()
	if timeout <= 0 || !self.cooloff.holds(destination, time.Now(), timeout) {
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
// unless the starvation escape skips them: the window
// holds no provider (heldCount 0), or the last round with exclusions came back
// empty. Returns how many it added; the round reports its result to
// noteCooloffRound.
func (self *multiClientWindow) addCooloffExclusions(excluded map[MultiHopId]bool, heldCount int) int {
	timeout := self.cooloffTimeout()
	if timeout <= 0 {
		return 0
	}
	now := time.Now()
	skipReason := ""
	count, changed, pending := func() (int, bool, int) {
		self.cooloff.stateLock.Lock()
		defer self.cooloff.stateLock.Unlock()
		if heldCount == 0 {
			skipReason = "empty_window"
		} else if self.cooloff.skipNext {
			skipReason = "empty_round"
		}
		self.cooloff.skipNext = false
		if skipReason != "" {
			// expire what is due, and keep the rest for the next round
			self.cooloff.expireWithLock(now, timeout)
			return 0, false, len(self.cooloff.entries)
		}
		count := self.cooloff.addExclusionsWithLock(excluded, now, timeout)
		changed := count != self.cooloff.loggedExcludedCount
		self.cooloff.loggedExcludedCount = count
		return count, changed, 0
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
		return 0
	}
	if changed {
		loggerOrDefault(self.log).Infof("%s\n", relEvent(
			"cooloff",
			"window", self.windowName(),
			"action", "exclude",
			"count", count,
		))
	}
	return count
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
// programmatic form of a reconnect: a reconnect starts with no cool-off.
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
// exit receives.
func providerVerdictErr(err error) bool {
	return blackholeVerdictErr(err) || err != nil && strings.HasPrefix(err.Error(), "send stalled")
}
