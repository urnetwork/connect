package extender

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// The read and write timeouts of one relay direction, kept by one timer
// instead of a deadline installed before every operation (CPUOPT2 §9.2
// finding 1). A direction reads and writes once per 4 KiB chunk, and each
// deadline install is a timer change in the runtime poller. Here an operation
// only stamps its start and kind into one atomic word. The timer falls due
// when the operation it last saw would time out. When it fires it finds that
// operation still in progress, which has then run its whole timeout and ends
// the relay, or a later one, and re-arms for the time that one has left. So a
// busy direction arms the timer about once per timeout, and an operation still
// ends exactly its timeout after it starts, as when each chunk installed its
// own deadline.
//
// The relay ends through cancel, which closes both connections, rather than
// through a deadline in the past. A timeout always ended the relay, and a
// past deadline could land on the next operation of a live connection after
// the one it was meant for completed; on a tls connection a write deadline
// that fires is terminal.
//
// A non-positive timeout leaves its operations unbounded. One goroutine at a
// time begins operations; the timer runs on its own.
type relayWatchdog struct {
	readTimeout  time.Duration
	writeTimeout time.Duration
	// The longest a fire is put off: the shorter positive timeout. An
	// operation that starts after a fire then falls due no earlier than the
	// next fire, whichever kind it is.
	period time.Duration
	cancel context.CancelFunc
	// the monotonic origin of the stamps
	startTime time.Time
	// The operation in progress: its start in nanoseconds after startTime,
	// shifted left one bit, with the low bit set for a write. One word, so
	// the timer never pairs one operation's start with another's kind.
	operation atomic.Int64

	stateLock sync.Mutex
	// nil when neither timeout is positive
	timer   *time.Timer
	stopped bool
	// nil outside package tests
	armForTest func()
}

// Arms the timer for the first operation, a read that starts now unless the
// caller begins another at once. armForTest is nil outside package tests.
func newRelayWatchdog(
	readTimeout time.Duration,
	writeTimeout time.Duration,
	cancel context.CancelFunc,
	armForTest func(),
) *relayWatchdog {
	period := readTimeout
	if period <= 0 || (0 < writeTimeout && writeTimeout < period) {
		period = writeTimeout
	}
	watchdog := &relayWatchdog{
		readTimeout:  readTimeout,
		writeTimeout: writeTimeout,
		period:       period,
		cancel:       cancel,
		startTime:    time.Now(),
		armForTest:   armForTest,
	}
	if 0 < period {
		func() {
			watchdog.stateLock.Lock()
			defer watchdog.stateLock.Unlock()
			watchdog.timer = time.AfterFunc(period, watchdog.check)
		}()
		if watchdog.armForTest != nil {
			watchdog.armForTest()
		}
	}
	return watchdog
}

// Stamps a read that starts now.
func (self *relayWatchdog) beginRead() {
	self.operation.Store(int64(time.Since(self.startTime)) << 1)
}

// Stamps a write that starts now.
func (self *relayWatchdog) beginWrite() {
	self.operation.Store(int64(time.Since(self.startTime))<<1 | 1)
}

// Takes a fire of the timer. Ends the relay when the operation in progress
// has run its whole timeout, and otherwise re-arms for the time it has left,
// at most one period.
func (self *relayWatchdog) check() {
	expired, armed := func() (bool, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.stopped {
			return false, false
		}
		// the time before the stamp: an operation stamped in between started
		// after now, so it is never judged expired
		now := time.Since(self.startTime)
		operation := self.operation.Load()
		timeout := self.readTimeout
		if operation&1 != 0 {
			timeout = self.writeTimeout
		}
		wait := self.period
		if 0 < timeout {
			left := time.Duration(operation>>1) + timeout - now
			if left <= 0 {
				self.stopped = true
				return true, false
			}
			wait = min(wait, left)
		}
		self.timer.Reset(wait)
		return false, true
	}()
	if expired {
		self.cancel()
	}
	if armed && self.armForTest != nil {
		self.armForTest()
	}
}

// Stops the timer once the direction is done.
func (self *relayWatchdog) stop() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.stopped = true
	if self.timer != nil {
		self.timer.Stop()
	}
}
