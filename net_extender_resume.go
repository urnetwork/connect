package connect

import (
	"time"
)

// Measuring again after the host resumes from a sleep (DESIGNNOTES4.md §6).
//
// Every timer of the network client runs on the monotonic clock, which stops
// while the host sleeps (mach_absolute_time on darwin, CLOCK_MONOTONIC on
// linux and android). A client that woke on the same path has no path change
// to wake it, so it would measure again only once the rest of the probe loop's
// refresh period had passed in awake time, up to six hours, while the samples
// the sleep aged out ranked nothing. The probe loop tells a resume from the
// host clock instead: a reading of time.Now carries the monotonic reading
// beside the wall one, and between two readings the wall clock moves past the
// monotonic one by the time the host slept (extenderSlept). Where the
// monotonic clock counts sleep too, the two do not part and no resume is told;
// there the loop's own timers come due across a sleep.

// Tells a resume from the readings of the host clock the probe loop takes at
// the wakeups of its wait (runProbes).
//
// A check that finds the host slept at least minSleep since the one before
// starts a resume, and the first check once the host has been awake for
// awakeTimeout since then tells it, with the time slept. Waiting out that
// awake time lets the network come back before anything is dialed, and keeps
// a dark wake, where the host wakes for upkeep and sleeps again, from telling
// one: a further long sleep starts the wait over. A nap shorter than minSleep
// neither starts nor ends a resume.
//
// Between two checks of a host that is awake, the clocks part only by what
// keeps the wall clock true: slews and steps of a second or so, a leap second.
// A minimum of minutes cannot be reached that way. A wall clock set forward at
// least that far reads as a sleep, since nothing else tells the two apart, and
// costs one probe pass; one set back tells nothing, and hides as much sleep
// from the check it falls in.
//
// Only the probe loop holds one, so it takes no lock.
type extenderResumeWatch struct {
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
func newExtenderResumeWatch(
	minSleep time.Duration,
	awakeTimeout time.Duration,
	now time.Time,
) *extenderResumeWatch {
	return &extenderResumeWatch{
		minSleep:     minSleep,
		awakeTimeout: awakeTimeout,
		checkTime:    now,
	}
}

// Whether the watch can tell a resume at all, so whether the loop checks.
func (self *extenderResumeWatch) Watching() bool {
	return 0 < self.minSleep && 0 < self.awakeTimeout
}

// Takes the reading `now` of one check. Returns the time slept and true at the
// check that tells a resume, and false at every other.
func (self *extenderResumeWatch) Check(now time.Time) (time.Duration, bool) {
	slept := extenderSlept(now, self.checkTime)
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

// The host resumed from a sleep of `sleep` (runProbes), which for measurement
// is a path change (networkChanged): the samples taken before the sleep go,
// since the host may have woken where it measured nothing, and the probe pass
// measures again after the first sample that completes since the sleep, so it
// never probes a path that has not worked since. A sample that has completed
// since wakes the pass now: the pass that followed it may have found its
// window still full of the samples from before. A stream opened since wakes it
// when its sample completes. Else the refresh loop is woken to take a sample,
// over a new stream in the feed role, since the open one predates the sleep
// and the connection under it may have died meanwhile. One resume asks for one
// sample at most, and one probe pass follows. The rest of a path change does
// not apply: the hint, its country and hello stay, and so do the holds.
func (self *ExtenderNetworkClient) hostResumed(now time.Time, sleep time.Duration) {
	self.log.Infof("[extender]resumed after %s asleep\n", sleep.Round(time.Second))
	minSleep := self.settings.ResumeMinSleep
	self.directory.ExpireSleptLatencies(minSleep)

	// whether `t` is since the host woke: it has not slept that long since
	sinceSleep := func(t time.Time) bool {
		return !t.IsZero() && extenderSlept(now, t) < minSleep
	}
	if sinceSleep(self.Status().LastSampleTime) {
		self.probeWake.NotifyAll()
		return
	}
	feedStream, feedStreamTime := func() (*ExtenderFeedStream, time.Time) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.feedStream, self.feedStreamTime
	}()
	if feedStream != nil {
		if sinceSleep(feedStreamTime) {
			return
		}
		feedStream.Close()
	}
	self.wakeMonitor.NotifyAll()
}
