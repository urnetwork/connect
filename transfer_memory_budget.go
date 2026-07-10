package connect

import (
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
	totalByteCount ByteCount
	usedByteCount  atomic.Int64
	// cumulative counters, so tests can assert reserve/release balance after
	// a build/load/teardown cycle (the message pool counts pattern)
	reservedByteCount atomic.Int64
	releasedByteCount atomic.Int64
}

func NewTransferMemoryBudget(totalByteCount ByteCount) *TransferMemoryBudget {
	return &TransferMemoryBudget{
		totalByteCount: totalByteCount,
	}
}

func (self *TransferMemoryBudget) TotalByteCount() ByteCount {
	return self.totalByteCount
}

// Available is the unreserved remainder of the budget
func (self *TransferMemoryBudget) Available() ByteCount {
	return max(0, self.totalByteCount-self.usedByteCount.Load())
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
}

// Counts returns the cumulative reserved/released byte counts.
// reserved-released equals the currently used bytes: it returns to zero when
// every borrowing queue is drained or cleared, so growth across a
// build/load/teardown cycle attributes a lost release.
func (self *TransferMemoryBudget) Counts() (reservedByteCount ByteCount, releasedByteCount ByteCount) {
	return self.reservedByteCount.Load(), self.releasedByteCount.Load()
}
