package connect

import (
	mathrand "math/rand/v2"
	"time"
)

// all regular protocol ping messages should align to this schedule
// this ensures the radio wakes up for as little as possible

// FIXME when the server ping matches the client ping, we can increase this to 10s
// const WakeupEpoch = 1 * time.Second

func WakeupAfter(d time.Duration, epoch time.Duration) <-chan time.Time {
	now := time.Now()
	t := WakeupTime(now.Add(d), epoch)
	return time.After(t.Sub(now))
}

// resetWakeupTimer resets a reusable timer onto the regular wakeup schedule.
// It is for hot selects where another arm often wins; constructing WakeupAfter
// there would abandon one newly allocated timer per message.
func resetWakeupTimer(timer *time.Timer, d time.Duration, epoch time.Duration) {
	now := time.Now()
	timer.Reset(WakeupTime(now.Add(d), epoch).Sub(now))
}

func WakeupTime(t time.Time, epoch time.Duration) time.Time {
	r := (time.Duration(t.UnixNano()) * time.Nanosecond) % epoch
	if r == 0 {
		return t
	}
	return t.Add(epoch - r)
}

// WakeupTime on the schedule shifted by offset: the first instant at or after t
// that lies offset past a multiple of epoch. A zero offset is the wall-clock
// schedule that every owner shares.
func wakeupTimeWithOffset(t time.Time, epoch time.Duration, offset time.Duration) time.Time {
	r := (time.Duration(t.UnixNano()) - offset) % epoch
	if r < 0 {
		r += epoch
	}
	if r == 0 {
		return t
	}
	return t.Add(epoch - r)
}

// Draws one owner's offset on the wakeup schedule, uniform in [0, jitter).
// Non-positive jitter is the wall-clock schedule, offset zero.
func wakeupOffset(jitter time.Duration) time.Duration {
	if jitter <= 0 {
		return 0
	}
	return time.Duration(mathrand.Int64N(int64(jitter)))
}

// pingPacer paces one writer's keepalive pings from its last write. A ping is
// due at the first wakeup instant at least timeout after the last write: the
// instant a timer reset after every write would choose. Writes only note their
// time. When the timer fires before the due instant that later writes moved,
// it is re-armed for that instant, so a busy writer arms its timer about once
// per ping interval instead of once per write, and each arm of a heaped timer
// is a timer heap modify (server CPUOPT2.md §9).
//
// Instants compare by their monotonic readings, which stop with the clock while
// a phone is suspended, never by wall-clock nanoseconds. One goroutine owns a
// pacer.
type pingPacer struct {
	timer   *time.Timer
	timeout time.Duration
	// the wakeup schedule: multiples of epoch, shifted by offset
	epoch         time.Duration
	offset        time.Duration
	lastWriteTime time.Time
	// nil outside package tests
	armForTest func()
}

// Creates the pacer of a writer whose last write was at now, its timer armed
// for the first ping.
func newPingPacer(
	timeout time.Duration,
	epoch time.Duration,
	offset time.Duration,
	now time.Time,
	armForTest func(),
) *pingPacer {
	pacer := &pingPacer{
		timeout:       timeout,
		epoch:         epoch,
		offset:        offset,
		lastWriteTime: now,
		armForTest:    armForTest,
	}
	pacer.timer = time.NewTimer(pacer.due().Sub(now))
	if armForTest != nil {
		armForTest()
	}
	return pacer
}

// The instant the last write makes the next ping due.
func (self *pingPacer) due() time.Time {
	return wakeupTimeWithOffset(self.lastWriteTime.Add(self.timeout), self.epoch, self.offset)
}

// Re-arms the timer for the due instant.
func (self *pingPacer) arm(now time.Time) {
	self.timer.Reset(self.due().Sub(now))
	if self.armForTest != nil {
		self.armForTest()
	}
}

// Notes a write that ended at now. The timer is left as it is.
func (self *pingPacer) wrote(now time.Time) {
	self.lastWriteTime = now
}

// Takes a fire of the timer at now. True when the ping is due; otherwise the
// timer is re-armed for the instant the writes since moved it to.
func (self *pingPacer) fired(now time.Time) bool {
	if now.Before(self.due()) {
		self.arm(now)
		return false
	}
	return true
}

// Notes a ping that ended at now and arms the timer for the next.
func (self *pingPacer) pinged(now time.Time) {
	self.lastWriteTime = now
	self.arm(now)
}

// Stops the timer when the writer exits.
func (self *pingPacer) stop() {
	self.timer.Stop()
}
