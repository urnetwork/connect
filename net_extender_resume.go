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
// refresh period had passed in awake time, up to six hours, ranking until then
// by the samples it took before the sleep, or by none once they aged out. The
// probe loop tells a resume from the host clock instead: between two of its
// readings the wall clock moves past the monotonic one by the time the host
// slept (hostSlept, hostResumeWatch).

// The host resumed from a sleep of `sleep` (runProbes), which for measurement
// is a path change (networkChanged): the samples taken before the sleep are
// due a refresh (RefreshSleptLatencies), since the host may have woken where
// it measured nothing. They stay in use, ranking the candidates, until the
// probe pass measures them again, which follows the first sample that
// completes since the sleep, so it never probes a path that has not worked
// since. A sample that has completed since wakes the pass now: the pass that
// followed it may have found its window still full of the samples from before.
// A stream opened since wakes it when its sample completes. Else the refresh
// loop is woken to take a sample, over a new stream in the feed role, since
// the open one predates the sleep and the connection under it may have died
// meanwhile. One resume asks for one sample at most, and one probe pass
// follows. Nothing else changes: the holds, limits and failure counts stay,
// and so do the hint, its country and hello.
func (self *ExtenderNetworkClient) hostResumed(now time.Time, sleep time.Duration) {
	self.log.Infof("[extender]resumed after %s asleep\n", sleep.Round(time.Second))
	minSleep := self.settings.ResumeMinSleep
	self.directory.RefreshSleptLatencies(minSleep)

	// whether `t` is since the host woke: it has not slept that long since
	sinceSleep := func(t time.Time) bool {
		return !t.IsZero() && hostSlept(now, t) < minSleep
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
