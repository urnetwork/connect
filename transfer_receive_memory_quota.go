// Encoded handoff permission is separate from an already-paid physical owner.
// Only branches outside that owner's ancestor chain acquire quota counters;
// their common physical ancestors stay charged exactly once.
package connect

import "math"

// Finds the first target ancestor already covered by the physical owner.
// An unrelated hierarchy keeps ordinary independent-budget admission.
func receiveMemoryQuotaStop(target, owner *TransferMemoryBudget) *TransferMemoryBudget {
	if target == nil || owner == nil || target.admissionRoot() != owner.admissionRoot() {
		return nil
	}
	for budget := target; budget != nil; budget = budget.parent {
		for ancestor := owner; ancestor != nil; ancestor = ancestor.parent {
			if budget == ancestor {
				return budget
			}
		}
	}
	return nil
}

// The caller keeps an exclusive physical owner covering at least byteCount
// until this permission is returned. The root admission lock serializes both.
func (self *TransferMemoryBudget) tryReserveQuotaWithLock(byteCount ByteCount, stop *TransferMemoryBudget) bool {
	if byteCount < 0 || stop == nil {
		return false
	}
	for budget := self; budget != stop; budget = budget.parent {
		if budget == nil {
			return false
		}
		total := budget.totalByteCount.Load()
		used := budget.usedByteCount.Load() + budget.quotaByteCount.Load()
		if total < byteCount || total-byteCount < used {
			return false
		}
	}
	for budget := self; budget != stop; budget = budget.parent {
		budget.quotaByteCount.Add(byteCount)
	}
	return true
}

// Uses the existing FIFO reservation queue and wakeup discipline. No second
// worker, capacity channel or per-packet scheduler is introduced.
func (self *TransferMemoryBudget) reserveReceiveQuota(byteCount ByteCount, stop *TransferMemoryBudget, wait bool) (bool, *transferMemoryReserveWaiter) {
	root := self.admissionRoot()
	root.admissionLock.Lock()
	defer root.admissionLock.Unlock()
	if self.tryReserveQuotaWithLock(byteCount, stop) {
		return true, nil
	}
	if !wait || byteCount < 0 || stop == nil {
		return false, nil
	}
	waiter := &transferMemoryReserveWaiter{budget: self, byteCount: byteCount,
		quotaStop: stop, ready: make(chan struct{})}
	root.reserveWaiters = append(root.reserveWaiters, waiter)
	return false, waiter
}

// Releasing quota cannot release its physical backing owner. Reserve for
// older fitting waiters before notifying callers that capacity changed.
func (self *TransferMemoryBudget) releaseReceiveQuota(byteCount ByteCount, stop *TransferMemoryBudget) {
	root := self.admissionRoot()
	root.admissionLock.Lock()
	for budget := self; budget != stop; budget = budget.parent {
		if budget == nil || budget.quotaByteCount.Add(-byteCount) < 0 {
			root.admissionLock.Unlock()
			panic("receive handoff quota released without ownership")
		}
	}
	root.grantReserveWaitersWithLock()
	root.admissionLock.Unlock()
	self.notifyCapacityChanged()
}

// A prepaid owner needs free permission only on the target-exclusive branch.
// Common ancestors are already paid and must not block a full-root head.
func (self *TransferMemoryBudget) receiveQuotaAvailable(stop *TransferMemoryBudget) ByteCount {
	root := self.admissionRoot()
	root.admissionLock.Lock()
	defer root.admissionLock.Unlock()
	available := ByteCount(math.MaxInt64)
	for budget := self; budget != stop; budget = budget.parent {
		if budget == nil {
			return 0
		}
		available = min(available, max(0, budget.totalByteCount.Load()-budget.usedByteCount.Load()-budget.quotaByteCount.Load()))
	}
	return available
}
