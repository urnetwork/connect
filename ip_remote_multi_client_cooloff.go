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
// listed before its provider failed is also dropped when the expansion takes
// it (cooloffHolds).
//
// What is noted:
//   - a candidate whose evaluation failed for the provider: its channel could
//     not be created, or its evaluation ping went unanswered or errored, and
//     the window counted it a provider failure (ProviderStateEvaluationFailed);
//   - an exit the resize pass removed with a verdict against the provider (a
//     blackhole verdict or a send-stall conviction) that the recent-exit
//     memory did not keep as an outage of ours.
//
// Never noted: a candidate the pass or the window canceled, a local contract
// failure, a platform setup failure, a transport loss, a close or a user
// action, the user's Fixed IP exit, and fixed destination sets, which their
// own retry backoff governs (FixedDestinationRetryMin).
//
// Bounds: one entry per provider, at most cooloffMaxCount, the oldest
// dropped first; an entry past the cool-off is dropped when discovery reads
// the exclusions. The memory has its own lock and never takes the window's.

import (
	"strings"
	"sync"
	"time"
)

// The most providers one window keeps in its cool-off. The exclusions ride
// along on every discovery request, and the platform widens its candidate
// read by the exclusions it is sent.
const cooloffMaxCount = 64

type destinationCooloff struct {
	stateLock sync.Mutex
	// when each provider last failed
	failTimes map[MultiHopId]time.Time
}

// Notes that destination failed at failTime, keeping at most maxCount
// providers, the newest.
func (self *destinationCooloff) note(destination MultiHopId, failTime time.Time, maxCount int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.failTimes == nil {
		self.failTimes = map[MultiHopId]time.Time{}
	}
	self.failTimes[destination] = failTime
	for maxCount < len(self.failTimes) {
		var oldest MultiHopId
		var oldestTime time.Time
		for candidate, candidateTime := range self.failTimes {
			if oldestTime.IsZero() || candidateTime.Before(oldestTime) {
				oldest, oldestTime = candidate, candidateTime
			}
		}
		delete(self.failTimes, oldest)
	}
}

// Adds each provider that failed within timeout of now to excluded, and drops
// the rest.
func (self *destinationCooloff) addExclusions(excluded map[MultiHopId]bool, now time.Time, timeout time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for destination, failTime := range self.failTimes {
		if now.Sub(failTime) < timeout {
			excluded[destination] = true
		} else {
			delete(self.failTimes, destination)
		}
	}
}

// Whether destination failed within timeout of now.
func (self *destinationCooloff) holds(destination MultiHopId, now time.Time, timeout time.Duration) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	failTime, ok := self.failTimes[destination]
	return ok && now.Sub(failTime) < timeout
}

// The window's cool-off; non-positive is off.
func (self *multiClientWindow) cooloffTimeout() time.Duration {
	if self.settings == nil {
		return 0
	}
	return self.settings.EvaluationFailureCooloff
}

// Notes a provider this window found failing. See the file comment for which
// failures call this.
func (self *multiClientWindow) noteFailedDestination(destination MultiHopId) {
	if self.cooloffTimeout() <= 0 || destination.Len() == 0 {
		return
	}
	if self.generator != nil {
		if _, fixed := self.generator.FixedDestinationSize(); fixed {
			return
		}
	}
	self.cooloff.note(destination, time.Now(), cooloffMaxCount)
}

// Whether destination is cooling off now: a candidate listed before its
// provider failed in this window is dropped when the expansion takes it.
func (self *multiClientWindow) cooloffHolds(destination MultiHopId) bool {
	timeout := self.cooloffTimeout()
	if timeout <= 0 {
		return false
	}
	return self.cooloff.holds(destination, time.Now(), timeout)
}

// Adds the providers still cooling off to a discovery request's exclusions.
func (self *multiClientWindow) addCooloffExclusions(excluded map[MultiHopId]bool) {
	if timeout := self.cooloffTimeout(); 0 < timeout {
		self.cooloff.addExclusions(excluded, time.Now(), timeout)
	}
}

// Whether a removal's error is a verdict against the provider: a blackhole
// verdict, or a send-stall conviction, which is admitted only while a sibling
// exit receives.
func providerVerdictErr(err error) bool {
	return blackholeVerdictErr(err) || err != nil && strings.HasPrefix(err.Error(), "send stalled")
}
