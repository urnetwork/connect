package connect

import (
	"time"
)

// Telling from the host clock that the host slept (DESIGNNOTES4.md §6).
//
// A reading of time.Now carries a monotonic reading beside the wall one, and
// Go subtracts two readings by their monotonic parts. The monotonic clock
// stops while the host sleeps (mach_absolute_time on darwin and ios,
// CLOCK_MONOTONIC on linux and android), and every timer stops with it. Across
// a sleep the wall clock moves past the monotonic one by the time slept
// (hostSlept), which is how code whose timers stood still learns that the host
// was away, and how a loop tells a resume (hostResumeWatch). Where the
// monotonic clock counts sleep too, the two do not part and nothing is seen;
// the timers there come due across a sleep on their own.

// The defaults of a loop that watches for a resume (hostResumeWatch). Between
// two checks of a host that is awake, the clocks part only by what keeps the
// wall clock true: slews and steps of a second or so, a leap second. Fifteen
// minutes is far past that, and past the naps that leave a host where it was,
// a screen lock or a lid closed between rooms. A check every minute is one
// clock read per minute awake and none asleep, and acts one to two minutes
// after the host wakes, by when its network is back.
const (
	defaultResumeCheckTimeout = 1 * time.Minute
	defaultResumeMinSleep     = 15 * time.Minute
)

// The time the host slept from `t` to `now`: how far the wall clock moved past
// the monotonic one. Across awake time the two part only by what keeps the
// wall clock true. A wall clock set forward reads as a sleep, and one set back
// hides as much sleep: nothing else tells them apart. Zero when either time
// has no monotonic reading, one loaded from the store or a fake clock's, since
// then both differences are the wall clock's.
func hostSlept(now time.Time, t time.Time) time.Duration {
	return now.Round(0).Sub(t.Round(0)) - now.Sub(t)
}

// Tells a resume from the readings of the host clock a loop takes at the
// wakeups of its wait.
//
// A check that finds the host slept at least minSleep since the one before
// starts a resume, and the first check once the host has been awake for
// awakeTimeout since then tells it, with the time slept. Waiting out that
// awake time lets the network come back before anything is dialed, and keeps
// a dark wake, where the host wakes for upkeep and sleeps again, from telling
// one: a further long sleep starts the wait over. A nap shorter than minSleep
// neither starts nor ends a resume. A wall clock set forward at least minSleep
// reads as a sleep, which costs what one resume costs; one set back tells
// nothing, and hides as much sleep from the check it falls in.
//
// Only the loop that reads holds one, so it takes no lock.
type hostResumeWatch struct {
	minSleep     time.Duration
	awakeTimeout time.Duration

	// the reading of the last check
	checkTime time.Time
	// the reading of the last check that found a sleep of at least minSleep,
	// zero when no resume has started
	sleepCheckTime time.Time
	// the time slept in those sleeps since the last resume
	sleep time.Duration
}

// A watch whose first check is measured from `now`. A minSleep or an
// awakeTimeout <= 0 tells no resume.
func newHostResumeWatch(
	minSleep time.Duration,
	awakeTimeout time.Duration,
	now time.Time,
) *hostResumeWatch {
	return &hostResumeWatch{
		minSleep:     minSleep,
		awakeTimeout: awakeTimeout,
		checkTime:    now,
	}
}

// Whether the watch can tell a resume at all, so whether the loop checks.
func (self *hostResumeWatch) Watching() bool {
	return 0 < self.minSleep && 0 < self.awakeTimeout
}

// Takes the reading `now` of one check. Returns the time slept and true at the
// check that tells a resume, and false at every other.
func (self *hostResumeWatch) Check(now time.Time) (time.Duration, bool) {
	slept := hostSlept(now, self.checkTime)
	self.checkTime = now
	if !self.Watching() {
		return 0, false
	}
	if self.minSleep <= slept {
		self.sleepCheckTime = now
		self.sleep += slept
		return 0, false
	}
	// awake time, by the monotonic clock: a wall clock set meanwhile does not
	// move it
	if self.sleepCheckTime.IsZero() || now.Sub(self.sleepCheckTime) < self.awakeTimeout {
		return 0, false
	}
	sleep := self.sleep
	self.sleepCheckTime = time.Time{}
	self.sleep = 0
	return sleep, true
}
