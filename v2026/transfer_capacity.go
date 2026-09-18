package connect

import "sync/atomic"

// An admission gate with a zero value that permits progress. This leaf state
// uses the same lazy notification pattern as TransferMemoryBudget: most Packs
// only read the atomic, and no channel is allocated until a producer waits.
// Store owns the notification, so callers cannot publish capacity silently.
type resendCapacityGate struct {
	unavailable atomic.Bool
	notify      atomic.Pointer[transferMemoryBudgetNotify]
}

func (self *resendCapacityGate) Load() bool { return self.unavailable.Load() }

func (self *resendCapacityGate) Store(unavailable bool) {
	if self.unavailable.Swap(unavailable) && !unavailable {
		if notify := self.notify.Swap(nil); notify != nil {
			close(notify.channel)
		}
	}
}

// Subscribe before Load. A publication racing subscription either closes the
// returned channel or is visible in that subsequent Load; neither can be lost.
func (self *resendCapacityGate) Notify() <-chan struct{} {
	var candidate *transferMemoryBudgetNotify
	for {
		if notify := self.notify.Load(); notify != nil {
			return notify.channel
		}
		if candidate == nil {
			candidate = &transferMemoryBudgetNotify{channel: make(chan struct{})}
		}
		if self.notify.CompareAndSwap(nil, candidate) {
			return candidate.channel
		}
	}
}
