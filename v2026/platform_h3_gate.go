package connect

import (
	"context"
	"strings"
	"sync"
	"time"
)

// platformH3Gate allows one H3-family socket generation per
// PlatformTransport. A waiting higher-priority family cancels a lower-priority
// connection, but not a dial that is hearing from its peer: an owner keeps the
// gate through its handshake and authentication until it reports itself
// connected or releases the gate, so a better family that keeps retrying
// cannot end every handshake of a lower one. A client refused on h1 and h3
// (429s, QUIC caps) cycled its modes and ended each h3dns handshake this way,
// and alt's front waited out its idle timeout for every one (2026-11-21).
//
// A dns carrier's lease carries a receive clock in its context, which its
// dial's packet conns report to (quic_dial_race.go). Once such a dial has heard
// nothing from alt for the mode's stall timeout, a better family waiting ends
// it, so a blackholed 53 or 4053 cannot hold the gate for its handshake bound
// (four times 30 s). A dial without a clock is bounded by its own timeouts,
// which bound the wait too. Equal families wait without oscillating.
type platformH3Gate struct {
	owner *PlatformTransport

	mutex        sync.Mutex
	activeMode   TransportMode
	activeCancel context.CancelFunc
	// the active owner has not reported itself connected
	activeDialing bool
	// the active dns carrier's receive clock and stall timeout; nil and zero
	// for a dial that cannot stall
	activeClock        *quicDialReceiveClock
	activeStallTimeout time.Duration
	generation         uint64
	waiters            map[TransportMode]int
	notify             chan struct{}

	// Tells a test that a waiter of the mode has seen the gate's state and
	// waits for its next change. Guarded by mutex; called without it.
	waitingForTest func(mode TransportMode)
}

func newPlatformH3Gate(owner *PlatformTransport) *platformH3Gate {
	return &platformH3Gate{
		owner:   owner,
		waiters: map[TransportMode]int{},
		notify:  make(chan struct{}),
	}
}

func (self *platformH3Gate) notifyChangedLocked() {
	close(self.notify)
	self.notify = make(chan struct{})
}

func (self *platformH3Gate) bestWaitingModeLocked() TransportMode {
	best := TransportModeNone
	for mode, count := range self.waiters {
		if count <= 0 {
			continue
		}
		if best == TransportModeNone || self.owner.isBetterMode(mode, best) ||
			(self.owner.modePreference(mode) == self.owner.modePreference(best) &&
				strings.Compare(string(mode), string(best)) < 0) {
			best = mode
		}
	}
	return best
}

func (self *platformH3Gate) removeWaiterLocked(mode TransportMode) {
	if count := self.waiters[mode]; count <= 1 {
		delete(self.waiters, mode)
	} else {
		self.waiters[mode] = count - 1
	}
}

// Ends the active owner for a waiting better family when it is connected,
// or when it is a dial that has stalled. Otherwise returns when its dial
// could stall, zero when it cannot.
func (self *platformH3Gate) preemptLocked(mode TransportMode, now time.Time) time.Time {
	if self.activeMode == TransportModeNone || !self.owner.isBetterMode(mode, self.activeMode) {
		return time.Time{}
	}
	if !self.activeDialing {
		self.activeCancel()
		return time.Time{}
	}
	if self.activeClock == nil || self.activeStallTimeout <= 0 {
		return time.Time{}
	}
	stallTime := self.activeClock.silentSince().Add(self.activeStallTimeout)
	if !now.Before(stallTime) {
		self.activeCancel()
		return time.Time{}
	}
	return stallTime
}

// Waits for the gate and returns the owner's context, which a waiting better
// family cancels once the owner is connected or its dial has stalled, with
// `connected`, which the owner calls once its dial has connected, and
// `release`. Both are idempotent. Not acquired when ctx ends first.
func (self *platformH3Gate) Acquire(
	ctx context.Context,
	mode TransportMode,
) (attemptCtx context.Context, connected func(), release func(), acquired bool) {
	func() {
		self.mutex.Lock()
		defer self.mutex.Unlock()
		self.waiters[mode] += 1
		self.notifyChangedLocked()
	}()

	for {
		self.mutex.Lock()
		if ctx.Err() != nil {
			self.removeWaiterLocked(mode)
			self.notifyChangedLocked()
			self.mutex.Unlock()
			return nil, nil, nil, false
		}
		stallTime := self.preemptLocked(mode, time.Now())
		if self.activeMode == TransportModeNone && self.bestWaitingModeLocked() == mode {
			self.removeWaiterLocked(mode)
			attemptCtx, attemptCancel := context.WithCancel(ctx)
			self.generation += 1
			generation := self.generation
			self.activeMode = mode
			self.activeCancel = attemptCancel
			self.activeDialing = true
			self.activeClock = nil
			self.activeStallTimeout = 0
			// a dns carrier's dial reports what it hears to the lease
			if schedule := self.owner.h3DialRaceSchedule(mode); schedule.whodis && 0 < schedule.stallTimeout {
				self.activeClock = newQuicDialReceiveClock(nil)
				self.activeStallTimeout = schedule.stallTimeout
				attemptCtx = withQuicDialReceiveClock(attemptCtx, self.activeClock)
			}
			self.notifyChangedLocked()
			self.mutex.Unlock()

			connected := func() {
				self.mutex.Lock()
				defer self.mutex.Unlock()
				if self.generation == generation && self.activeDialing {
					self.activeDialing = false
					// a better family waiting may now cancel this one
					self.notifyChangedLocked()
				}
			}
			var once sync.Once
			release := func() {
				once.Do(func() {
					attemptCancel()
					self.mutex.Lock()
					defer self.mutex.Unlock()
					if self.generation == generation {
						self.activeMode = TransportModeNone
						self.activeCancel = nil
						self.activeDialing = false
						self.activeClock = nil
						self.activeStallTimeout = 0
						self.notifyChangedLocked()
					}
				})
			}
			return attemptCtx, connected, release, true
		}
		notify := self.notify
		waitingForTest := self.waitingForTest
		self.mutex.Unlock()

		if waitingForTest != nil {
			waitingForTest(mode)
		}
		// the active dial's next stall, which no change of the gate announces
		var stall <-chan time.Time
		if !stallTime.IsZero() {
			stall = time.After(time.Until(stallTime))
		}
		select {
		case <-ctx.Done():
			func() {
				self.mutex.Lock()
				defer self.mutex.Unlock()
				self.removeWaiterLocked(mode)
				self.notifyChangedLocked()
			}()
			return nil, nil, nil, false
		case <-notify:
		case <-stall:
		}
	}
}
