package connect

import (
	"sync"
	"sync/atomic"
)

// A byte budget shared by transfer queues across sequences — typically all
// clients of one device (the control client plus every window client) — so
// the aggregate queue memory stays flat as the number of peers grows, while
// a single fast peer can still borrow a deep queue.
//
// Queues reserve the bytes they hold above their guaranteed floor
// (`ResendQueueMinByteCount`/`ReceiveQueueMinByteCount`) and release them as
// items leave (see `transferQueue`). Admission gates on `Available` before a
// queue grows above its floor, so `Reserve` may transiently overdraft the
// total by up to one message per sequence past an admission that saw
// headroom. The floor keeps every sequence progressing when the pool is
// empty, which makes cross-sequence deadlock impossible.
//
// All methods are safe for concurrent use.
type TransferMemoryBudget struct {
	totalByteCount atomic.Int64
	usedByteCount  atomic.Int64
	// cumulative counters, so tests can assert reserve/release balance after
	// a build/load/teardown cycle (the message pool counts pattern)
	reservedByteCount atomic.Int64
	releasedByteCount atomic.Int64

	notifyLock sync.Mutex
	notify     chan struct{}
}

func NewTransferMemoryBudget(totalByteCount ByteCount) *TransferMemoryBudget {
	budget := &TransferMemoryBudget{}
	budget.totalByteCount.Store(totalByteCount)
	return budget
}

func (self *TransferMemoryBudget) TotalByteCount() ByteCount {
	return self.totalByteCount.Load()
}

// SetTotalByteCount retunes the budget capacity live (e.g. reallocating the
// provider share to the client pair while providing is off). Shrinking does
// not evict reserved bytes; the pool admits nothing new above the new total
// until enough releases drain it.
func (self *TransferMemoryBudget) SetTotalByteCount(totalByteCount ByteCount) {
	self.totalByteCount.Store(totalByteCount)
	self.notifyCapacityChanged()
}

// Available is the unreserved remainder of the budget
func (self *TransferMemoryBudget) Available() ByteCount {
	return max(0, self.totalByteCount.Load()-self.usedByteCount.Load())
}

func (self *TransferMemoryBudget) UsedByteCount() ByteCount {
	return self.usedByteCount.Load()
}

// Reserve takes bytes from the budget. It always succeeds (see the overdraft
// note in the type doc); admission gates on `Available`.
func (self *TransferMemoryBudget) Reserve(byteCount ByteCount) {
	self.usedByteCount.Add(byteCount)
	self.reservedByteCount.Add(byteCount)
}

// TryReserve atomically reserves byteCount only when it fits within the
// current total. Transfer queues deliberately use Reserve's bounded overdraft
// semantics, but fixed-size lifetime owners (notably WebRTC peer connections)
// need an exact admission ceiling shared across multiple managers.
func (self *TransferMemoryBudget) TryReserve(byteCount ByteCount) bool {
	if byteCount < 0 {
		return false
	}
	for {
		total := self.totalByteCount.Load()
		used := self.usedByteCount.Load()
		if total < byteCount || total-byteCount < used {
			return false
		}
		if self.usedByteCount.CompareAndSwap(used, used+byteCount) {
			self.reservedByteCount.Add(byteCount)
			return true
		}
	}
}

// Release returns bytes to the budget
func (self *TransferMemoryBudget) Release(byteCount ByteCount) {
	used := self.usedByteCount.Add(-byteCount)
	self.releasedByteCount.Add(byteCount)
	if used < 0 {
		// accounting bug: more released than reserved.
		// log unconditionally so production sees it (tests see it as a
		// negative used count breaking the balance assertions)
		DefaultLogger().Errorf("[tmb]release below zero (%d)", used)
	}
	self.notifyCapacityChanged()
}

// CapacityNotify returns a channel closed the next time a release or resize
// may make admission possible. Capture it before TryReserve to avoid a lost
// wakeup.
func (self *TransferMemoryBudget) CapacityNotify() <-chan struct{} {
	self.notifyLock.Lock()
	defer self.notifyLock.Unlock()
	if self.notify == nil {
		self.notify = make(chan struct{})
	}
	return self.notify
}

func (self *TransferMemoryBudget) notifyCapacityChanged() {
	self.notifyLock.Lock()
	defer self.notifyLock.Unlock()
	if self.notify != nil {
		close(self.notify)
		// Allocate the next generation only when a waiter actually subscribes.
		// Transfer queue budgets release on the packet path but normally have
		// no capacity waiters; eagerly replacing this channel made every
		// release allocate.
		self.notify = nil
	}
}

// Counts returns the cumulative reserved/released byte counts.
// reserved-released equals the currently used bytes: it returns to zero when
// every borrowing queue is drained or cleared, so growth across a
// build/load/teardown cycle attributes a lost release.
func (self *TransferMemoryBudget) Counts() (reservedByteCount ByteCount, releasedByteCount ByteCount) {
	return self.reservedByteCount.Load(), self.releasedByteCount.Load()
}
