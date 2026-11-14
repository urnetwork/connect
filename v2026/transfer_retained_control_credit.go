// Pure protocol cache state owns detached lifetime tokens. DATA backing keeps
// the existing retained permission and legacy queue admission semantics.
package connect

import (
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// One lazily created counter per legacy source, independent of its queue graph.
// The queue lock precedes this lock; release needs only this detached counter.
type transferQueueRetainedControl struct {
	stateLock         sync.Mutex
	retainedByteCount ByteCount
}

// A final owner may release after the source queue is cleared or collected.
// Copies are forbidden; all cache/worker references share this exact pointer.
type transferRetainedControlCredit struct {
	budget    *TransferMemoryBudget
	local     *transferQueueRetainedControl
	wake      chan struct{}
	byteCount ByteCount
	released  atomic.Bool
	noBudget  bool
}

// Use the existing conservative backing-class envelope for both allocations.
// A shared-budget owner prepays the same worst case without creating a ledger.
func transferRetainedControlCreditByteCount() ByteCount {
	owners := addReceiveQueueByteCount(
		retainedMessageCapacity(ByteCount(unsafe.Sizeof(transferRetainedControlCredit{}))),
		retainedMessageCapacity(ByteCount(unsafe.Sizeof(transferQueueRetainedControl{}))),
	)
	// The existing one-slot wake can outlive its source through this lease.
	// Its runtime header/empty slot fits the same smallest backing envelope.
	return addReceiveQueueByteCount(owners, retainedMessageCapacity(0))
}

// The caller classifies the complete original before taking custody. Pure
// protocol cache/readiness state has one bounded owner per existing flow and
// takes only a detached lifetime token. DATA, including a payload-bearing SYN,
// keeps its existing complete backing charge and ordinary queue permission.
func (self *SendSequence) reserveRetainedControlMemory(requiredBytes ByteCount, noBudget ...bool) (*transferRetainedControlCredit, bool) {
	if self == nil || self.ctx == nil || self.ctx.Err() != nil || self.resendQueue == nil || requiredBytes <= 0 {
		return nil, false
	}
	if len(noBudget) != 0 && noBudget[0] {
		self.resendQueue.stateLock.Lock()
		defer self.resendQueue.stateLock.Unlock()
		if self.resendQueue.retainedControlClosed || self.ctx.Err() != nil {
			return nil, false
		}
		return &transferRetainedControlCredit{wake: self.retainedAdmissionWake, noBudget: true}, true
	}
	overhead := transferRetainedControlCreditByteCount()
	total := addReceiveQueueByteCount(requiredBytes, overhead)
	if total <= requiredBytes || total-requiredBytes != overhead {
		return nil, false
	}
	maximum := ByteCount(0)
	if self.resendQueue.Budget() == nil {
		if self.sendBufferSettings == nil {
			return nil, false
		}
		maximum = self.sendWindowSnapshot(time.Now()).Window
	}
	return self.resendQueue.tryReserveRetainedControl(total, maximum, self.retainedAdmissionWake)
}

// The queue's local maximum stays caller-owned. Only controls use this exact
// reservation; ordinary legacy data keeps its existing one-item overdraw bound.
func (self *transferQueue[T]) tryReserveRetainedControl(byteCount, maxByteCount ByteCount, wake chan struct{}) (*transferRetainedControlCredit, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.retainedControlClosed || byteCount <= 0 {
		return nil, false
	}
	if self.budget != nil {
		if !self.budget.TryReserve(byteCount) {
			return nil, false
		}
		return &transferRetainedControlCredit{budget: self.budget, wake: wake, byteCount: byteCount}, true
	}
	local := self.retainedControl
	if local == nil {
		if !transferRetainedControlFits(self.byteCount, 0, byteCount, maxByteCount) {
			return nil, false
		}
		local = &transferQueueRetainedControl{}
		self.retainedControl = local
	}
	local.stateLock.Lock()
	defer local.stateLock.Unlock()
	if !transferRetainedControlFits(self.byteCount, local.retainedByteCount, byteCount, maxByteCount) {
		return nil, false
	}
	local.retainedByteCount += byteCount
	return &transferRetainedControlCredit{local: local, wake: wake, byteCount: byteCount}, true
}

// Subtraction keeps impossible/overflowed requests from acquiring permission.
func transferRetainedControlFits(queued, retained, arrival, maximum ByteCount) bool {
	if queued < 0 || retained < 0 || arrival <= 0 || maximum <= queued || maximum-queued < retained {
		return false
	}
	return arrival <= maximum-queued-retained
}

// The caller holds the queue lock. No ordinary byte-update hook is needed:
// reservations read live occupancy under that same lock, not a mirrored copy.
func (self *transferQueue[T]) windowByteCountWithLock() ByteCount {
	if self.retainedControl == nil {
		return self.byteCount
	}
	self.retainedControl.stateLock.Lock()
	retained := self.retainedControl.retainedByteCount
	self.retainedControl.stateLock.Unlock()
	return addReceiveQueueByteCount(self.byteCount, retained)
}

// Exactly one final return owns the counter update; repeated cleanup is inert.
func (self *transferRetainedControlCredit) Release() {
	if self == nil || !self.released.CompareAndSwap(false, true) {
		return
	}
	budget, local, wake := self.budget, self.local, self.wake
	self.budget, self.local, self.wake = nil, nil, nil
	if budget != nil {
		budget.Release(self.byteCount)
	} else if local != nil {
		local.stateLock.Lock()
		if local.retainedByteCount < self.byteCount {
			local.stateLock.Unlock()
			panic("negative retained control credit")
		}
		local.retainedByteCount -= self.byteCount
		local.stateLock.Unlock()
	}
	// Flat legacy budgets need this directed edge too: they do not subscribe
	// to shared retained-budget capacity notifications in the source loop.
	select {
	case wake <- struct{}{}:
	default:
	}
}
