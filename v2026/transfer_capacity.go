package connect

import (
	"slices"
	"sync"
	"sync/atomic"
)

// An admission gate with a zero value that permits progress. Most Packs only
// read its atomics, and nothing is allocated until a producer waits. Store
// owns the wake, so callers cannot publish capacity silently.
//
// Senders that wait pass in arrival order, one turn each: an opening gives the
// turn to the first waiter only, and each waiter that passes gives it to the
// next. While anyone waits, a sender that could wait queues behind them even
// when the gate is open. Waking every waiter at once let the runtime choose who
// passed: with no idle processor the first to run passed again and again while
// the gate stayed open, and its siblings found it closed by the time they ran.
// A sender that cannot wait holds no place in line and takes an open gate.
//
// Safe for concurrent use. The send loop is the one publisher.
type resendCapacityGate struct {
	unavailable atomic.Bool
	// A waiter counts itself before it reads unavailable, so a Store that
	// opens the gate after that read sees the waiter and gives it the turn.
	waiterCount atomic.Int64
	stateLock   sync.Mutex
	// Arrival order. While the gate is open the first waiter holds the turn.
	waiters []*resendCapacityWaiter
}

// One blocked sender. Its ready channel closes when it is given the turn.
type resendCapacityWaiter struct {
	ready chan struct{}
	turn  bool
}

func (self *resendCapacityGate) Load() bool { return self.unavailable.Load() }

func (self *resendCapacityGate) Store(unavailable bool) {
	if self.unavailable.Swap(unavailable) && !unavailable && self.waiterCount.Load() != 0 {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.giveTurnWithLock()
	}
}

// Whether a sender may pass without queueing. One that can wait must also find
// nobody waiting ahead of it.
func (self *resendCapacityGate) open(canWait bool) bool {
	return !self.unavailable.Load() && (!canWait || self.waiterCount.Load() == 0)
}

// Returns nil when the sender may pass now, and otherwise its place at the end
// of the line. The caller must wait for the turn and call pass, or call
// abandon.
func (self *resendCapacityGate) queue() *resendCapacityWaiter {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.waiterCount.Add(1)
	if !self.unavailable.Load() && len(self.waiters) == 0 {
		self.waiterCount.Add(-1)
		return nil
	}
	waiter := &resendCapacityWaiter{ready: make(chan struct{})}
	self.waiters = append(self.waiters, waiter)
	return waiter
}

// Called once the waiter's ready channel closes; reports whether the waiter may
// go, giving the turn to the next waiter. A gate that closed again since the
// turn was given keeps the waiter first in line, with a fresh ready channel.
func (self *resendCapacityGate) pass(waiter *resendCapacityWaiter) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.unavailable.Load() {
		waiter.turn = false
		waiter.ready = make(chan struct{})
		return false
	}
	self.removeWithLock(waiter)
	self.giveTurnWithLock()
	return true
}

// Takes a waiter that stopped waiting out of line and passes on any turn it
// held.
func (self *resendCapacityGate) abandon(waiter *resendCapacityWaiter) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.removeWithLock(waiter)
	self.giveTurnWithLock()
}

func (self *resendCapacityGate) removeWithLock(waiter *resendCapacityWaiter) {
	if index := slices.Index(self.waiters, waiter); 0 <= index {
		self.waiters = slices.Delete(self.waiters, index, index+1)
		self.waiterCount.Add(-1)
	}
}

// Gives the first waiter the turn while the gate is open.
func (self *resendCapacityGate) giveTurnWithLock() {
	if len(self.waiters) == 0 || self.waiters[0].turn || self.unavailable.Load() {
		return
	}
	self.waiters[0].turn = true
	close(self.waiters[0].ready)
}
