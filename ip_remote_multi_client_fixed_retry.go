// Re-dial pacing for the destinations of a fixed-destination window.
//
// A fixed window (every spec is an explicit client id: a hosted proxy pinned
// to one provider, a chosen network peer) has exactly one candidate per slot,
// so a failed attempt can only be retried against the same destination. Each
// attempt mints a derived platform identity, registers its provide keys,
// opens a contract and, when the evaluation fails, retires the identity. With
// nothing pacing the retry, a destination that cannot be reached re-dialed on
// every resize pass for as long as the window lived: immediately after a
// Reliability verdict (the destination's client is inactive and will never
// return) and as soon as a timed-out pass ended otherwise. On Main on
// 2026-10-12 that was about 95% of all derived-client registrations and
// retirements: hosted devices pinned to a retired provider looped every ~9s,
// devices pinned to an offline provider every ~60s.
//
// Every failed attempt of a fixed destination now defers its next attempt by a
// jittered exponential backoff, from FixedDestinationRetryMin doubling to
// FixedDestinationRetryMax. A Reliability verdict goes straight to the maximum.
// An admission clears the destination's state, so a working destination keeps
// its immediate re-dial after transport loss. The first attempt is never
// deferred. Only the re-dial is paced: resize keeps its health cadence, and a
// window with other healthy destinations keeps serving them.
//
// The state is per destination and bounded by the generator's fixed spec set.
// It is guarded by the window stateLock.
package connect

import (
	mathrand "math/rand"
	"time"
)

// One fixed destination's consecutive failed attempts and the earliest time
// it may be attempted again.
type fixedDestinationRetry struct {
	failureCount int
	notBefore    time.Time
}

// The unjittered delay after `failureCount` consecutive failures: min, doubling
// per failure, capped at max. Nonpositive bounds disable pacing.
func fixedDestinationRetryDelay(failureCount int, min time.Duration, max time.Duration) time.Duration {
	if failureCount <= 0 || min <= 0 {
		return 0
	}
	if max < min {
		max = min
	}
	delay := min
	// bounded shift: the count keeps climbing while a destination stays down
	for i := 1; i < failureCount && delay < max; i += 1 {
		delay *= 2
	}
	if max < delay {
		delay = max
	}
	return delay
}

// Uniform in [delay/2, delay). Jitter only downward, so the configured maximum
// is a real ceiling. Hundreds of hosted windows fail against the same
// dependency at the same instant; without jitter they would retry in lockstep.
func (self *multiClientWindow) retryJitter(delay time.Duration) time.Duration {
	half := delay / 2
	if half <= 0 {
		return delay
	}
	if self.retryJitterForTest != nil {
		return half + max(0, min(half-1, self.retryJitterForTest(half)))
	}
	return half + time.Duration(mathrand.Int63n(int64(half)))
}

// Records one failed attempt of a fixed destination and defers its next one.
// `refused` marks an authoritative Reliability verdict, which defers by the
// maximum at once.
func (self *multiClientWindow) noteFixedDestinationFailed(destination MultiHopId, refused bool, now time.Time) {
	// bare fixture windows carry no settings and no pacing
	if self.settings == nil {
		return
	}
	retryMin := self.settings.FixedDestinationRetryMin
	retryMax := self.settings.FixedDestinationRetryMax
	if retryMin <= 0 {
		return
	}
	if retryMax < retryMin {
		retryMax = retryMin
	}
	var failureCount int
	var delay time.Duration
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.fixedRetries == nil {
			self.fixedRetries = map[MultiHopId]*fixedDestinationRetry{}
		}
		retry := self.fixedRetries[destination]
		if retry == nil {
			retry = &fixedDestinationRetry{}
			self.fixedRetries[destination] = retry
		}
		retry.failureCount += 1
		delay = fixedDestinationRetryDelay(retry.failureCount, retryMin, retryMax)
		if refused {
			delay = retryMax
		}
		delay = self.retryJitter(delay)
		// a late plain failure never shortens a deferral already in force
		if notBefore := now.Add(delay); retry.notBefore.Before(notBefore) {
			retry.notBefore = notBefore
		}
		failureCount = retry.failureCount
	}()
	if self.log != nil {
		self.log.Infof("%s\n", relEvent(
			"fixed_redial_deferred",
			"exit", destination.Tail(),
			"failures", failureCount,
			"refused", refused,
			"delay", delay,
		))
	}
}

// An admitted fixed destination is healthy again; its next failure starts
// the backoff over.
func (self *multiClientWindow) noteFixedDestinationAdmitted(destination MultiHopId) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.fixedRetries, destination)
}

// Whether a fixed destination must not be attempted at `now`.
func (self *multiClientWindow) fixedDestinationDeferred(destination MultiHopId, now time.Time) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	retry := self.fixedRetries[destination]
	return retry != nil && now.Before(retry.notBefore)
}

// Whether every one of the `neededCount` missing fixed destinations is
// deferred at `now`, and if so, when the first deferral ends. A destination
// only carries retry state while it is missing (admission clears it), so the
// deferred count bounds the missing destinations that may not be attempted.
func (self *multiClientWindow) fixedRetryDeferral(neededCount int, now time.Time) (time.Time, bool) {
	if neededCount <= 0 {
		return time.Time{}, false
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	deferredCount := 0
	var notBefore time.Time
	for _, retry := range self.fixedRetries {
		if !now.Before(retry.notBefore) {
			continue
		}
		deferredCount += 1
		if notBefore.IsZero() || retry.notBefore.Before(notBefore) {
			notBefore = retry.notBefore
		}
	}
	if deferredCount < neededCount {
		return time.Time{}, false
	}
	return notBefore, true
}
