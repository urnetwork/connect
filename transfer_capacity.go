package connect

import (
	"sync"
	"sync/atomic"
)

// Waiting producers pass in arrival order. The zero value permits progress;
// zero-timeout producers keep their immediate open-gate offer. Intrusive links
// leave no retained slice backing after a waiter departs.
// Safe for concurrent use. The send loop is the capacity publisher.
type resendCapacityGate struct {
	unavailable atomic.Bool
	waiterCount atomic.Int64
	stateLock   sync.Mutex
	first, last *resendCapacityWaiter
	closed      atomic.Bool
	// Nil in production; the first-waiter test publishes while count is zero.
	beforeQueueAppendForTest func()
}

// Ordinary producers own their ready channel. A prepaid directed subscriber
// uses only its parent's existing nonblocking wake, never a worker/channel.
type resendCapacityWaiter struct {
	ready          chan struct{}
	wake           chan<- struct{}
	turn           bool
	queued         bool
	owner          atomic.Pointer[resendCapacityGate]
	epoch          uint64
	previous, next *resendCapacityWaiter
}

func (self *resendCapacityGate) Load() bool { return self.closed.Load() || self.unavailable.Load() }

func (self *resendCapacityGate) Store(unavailable bool) {
	if self.closed.Load() {
		return
	}
	if self.unavailable.Swap(unavailable) && !unavailable && self.waiterCount.Load() != 0 {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.giveTurnWithLock()
	}
}

// A producer that could wait respects older waiters. A zero-timeout producer
// holds no FIFO place and retains its existing open-gate offer.
func (self *resendCapacityGate) open(canWait bool) bool {
	return !self.closed.Load() && !self.unavailable.Load() && (!canWait || self.waiterCount.Load() == 0)
}

func (self *resendCapacityGate) queue() *resendCapacityWaiter {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed.Load() || !self.unavailable.Load() && self.first == nil {
		return nil
	}
	if hook := self.beforeQueueAppendForTest; hook != nil {
		hook()
	}
	waiter := &resendCapacityWaiter{ready: make(chan struct{})}
	waiter.owner.Store(self)
	self.appendWithLock(waiter)
	// Store may have opened before our count became visible.
	self.giveTurnWithLock()
	return waiter
}

// Installs one already-paid identity slot and rechecks capacity under the same
// lock. An ACK before registration therefore grants one turn rather than
// losing its edge. No application callback runs under this lock.
func (self *resendCapacityGate) watch(waiter *resendCapacityWaiter, wake chan<- struct{}) uint64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed.Load() || wake == nil || !waiter.owner.CompareAndSwap(nil, self) {
		return 0
	}
	if waiter.epoch == ^uint64(0) {
		waiter.owner.Store(nil)
		return 0
	}
	waiter.epoch++
	waiter.wake = wake
	self.appendWithLock(waiter)
	self.giveTurnWithLock()
	return waiter.epoch
}

func (self *resendCapacityGate) appendWithLock(waiter *resendCapacityWaiter) {
	if waiter.owner.Load() != self || waiter.queued {
		panic("capacity waiter has another owner")
	}
	waiter.previous, waiter.next, waiter.queued, waiter.turn = self.last, nil, true, false
	if self.last == nil {
		self.first = waiter
	} else {
		self.last.next = waiter
	}
	self.last = waiter
	self.waiterCount.Add(1)
}

// Directed operations require both exact gate and immutable installation token.
// An old notification/withdrawal cannot operate on a reused slot.
func (self *resendCapacityGate) watchGranted(waiter *resendCapacityWaiter, epoch uint64) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return waiter.owner.Load() == self && waiter.queued && waiter.wake != nil && waiter.epoch == epoch && waiter.turn && !self.closed.Load()
}

func (self *resendCapacityGate) passWatch(waiter *resendCapacityWaiter, epoch uint64) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if waiter.owner.Load() != self || waiter.wake == nil || waiter.epoch != epoch {
		return false
	}
	return self.passWithLock(waiter)
}

func (self *resendCapacityGate) abandonWatch(waiter *resendCapacityWaiter, epoch uint64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if waiter.owner.Load() != self || waiter.wake == nil || waiter.epoch != epoch {
		return
	}
	self.removeWithLock(waiter)
	self.giveTurnWithLock()
}

func (self *resendCapacityGate) pass(waiter *resendCapacityWaiter) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if waiter.owner.Load() != self || waiter.wake != nil {
		return false
	}
	return self.passWithLock(waiter)
}

func (self *resendCapacityGate) passWithLock(waiter *resendCapacityWaiter) bool {
	if self.closed.Load() || !waiter.queued || !waiter.turn {
		return false
	}
	if self.unavailable.Load() {
		waiter.turn = false
		if waiter.wake == nil {
			waiter.ready = make(chan struct{})
		}
		return false
	}
	self.removeWithLock(waiter)
	self.giveTurnWithLock()
	return true
}

func (self *resendCapacityGate) abandon(waiter *resendCapacityWaiter) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if waiter.owner.Load() != self || waiter.wake != nil {
		return
	}
	self.removeWithLock(waiter)
	self.giveTurnWithLock()
}

func (self *resendCapacityGate) removeWithLock(waiter *resendCapacityWaiter) {
	if waiter.owner.Load() != self || !waiter.queued {
		return
	}
	if waiter.previous == nil {
		self.first = waiter.next
	} else {
		waiter.previous.next = waiter.next
	}
	if waiter.next == nil {
		self.last = waiter.previous
	} else {
		waiter.next.previous = waiter.previous
	}
	waiter.previous, waiter.next, waiter.queued, waiter.turn = nil, nil, false, false
	self.waiterCount.Add(-1)
	waiter.ready, waiter.wake = nil, nil
	waiter.owner.Store(nil)
}

func (self *resendCapacityGate) giveTurnWithLock() {
	waiter := self.first
	if self.closed.Load() || waiter == nil || waiter.turn || self.unavailable.Load() {
		return
	}
	waiter.turn = true
	if waiter.wake == nil {
		close(waiter.ready)
	} else {
		select {
		case waiter.wake <- struct{}{}:
		default:
		}
	}
}

// Source teardown wakes directed subscribers for withdrawal, never permission.
// Ordinary blocked callers already select the source context's cancellation.
func (self *resendCapacityGate) close() {
	self.unavailable.Store(true)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.closed.Store(true)
	for waiter := self.first; waiter != nil; waiter = waiter.next {
		if waiter.wake == nil && !waiter.turn {
			waiter.turn = true
			close(waiter.ready)
		} else if waiter.wake != nil {
			select {
			case waiter.wake <- struct{}{}:
			default:
			}
		}
	}
}
