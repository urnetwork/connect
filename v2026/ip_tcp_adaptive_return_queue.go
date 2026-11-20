// Optional return batching uses exact incremental flow credit, never a fill timer.
package connect

import (
	"context"
	"sync"
)

// One producer and one callback consumer own a flow's read queue. Unbounded
// and unexpanded flows keep the original channel path. An adaptive queue holds at
// most its configured cold roots until an actual ready overflow obtains the extra
// configured hot claim. Its small bounded metadata stays in the existing24KiB row.
type tcpReturnReadQueue struct {
	ctx          context.Context
	ordinary     chan []byte
	maximumBatch int
	coldBatch    int
	batch        [][]byte

	stateLock    sync.Mutex
	packets      [][]byte
	maximumQueue int
	closed       bool
	callback     bool
	ready        chan struct{}
	capacity     chan struct{}
	workspace    *tcpReturnBatchWorkspace
	memory       natMemoryReservation
	// Nil preserves production behavior; tests park the actual budget result.
	afterFundingAttemptForTest func(bool)
}

// The constructor allocates descriptors, not packet roots or extra permissions.
func newTcpReturnReadQueue(ctx context.Context, settings *TcpBufferSettings, workspace *tcpReturnBatchWorkspace) *tcpReturnReadQueue {
	settings = boundedNatTcpMemorySettings(settings, settings.MemoryBudget)
	maximumBatch := max(1, settings.WriteBatchSize)
	maximumQueue := min(settings.SequenceBufferSize, maximumBatch)
	queue := &tcpReturnReadQueue{ctx: ctx, maximumBatch: maximumBatch, coldBatch: settings.MemoryColdWriteBatchSize, batch: make([][]byte, 0, maximumBatch)}
	if workspace == nil {
		queue.ordinary = make(chan []byte, maximumQueue)
		return queue
	}
	queue.maximumQueue = maximumQueue
	queue.packets = make([][]byte, 0, maximumQueue)
	queue.ready, queue.capacity = make(chan struct{}, 1), make(chan struct{}, 1)
	queue.workspace = workspace
	return queue
}

// A buffered edge only wakes an owner to recheck its actual locked predicate.
func notifyTcpReturnQueue(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Takes a packet on success; failure leaves the caller its original root.
// Failed extra-credit admission keeps the configured cold backpressure.
func (self *tcpReturnReadQueue) offer(packet []byte) bool {
	return self.offerWithContext(self.ctx, packet, true)
}

// Takes an already-built final reset on success even if cancellation raced
// its construction. The original background teardown drain remains its owner;
// this regenerable control never promotes a cold queue.
func (self *tcpReturnReadQueue) offerFinal(packet []byte) bool {
	return self.offerWithContext(context.Background(), packet, false)
}

// Only final reset publication may outlive flow cancellation. Ordinary data
// and FIN keep their existing cancellation and caller-owned failure contract.
func (self *tcpReturnReadQueue) offerWithContext(ctx context.Context, packet []byte, allowPromotion bool) bool {
	if self.ordinary != nil {
		select {
		case <-ctx.Done():
			return false
		case self.ordinary <- packet:
			return true
		}
	}
	for {
		self.stateLock.Lock()
		if self.closed || ctx.Err() != nil {
			self.stateLock.Unlock()
			return false
		}
		limit := min(self.coldBatch, self.maximumQueue)
		if self.memory.budget != nil {
			limit = self.maximumQueue
		}
		if len(self.packets) < limit {
			self.packets = append(self.packets, packet)
			notifyTcpReturnQueue(self.ready)
			self.stateLock.Unlock()
			return true
		}
		// Discard only a stale edge while holding the predicate lock. A new
		// consumer drain after unlock always publishes a fresh capacity edge.
		select {
		case <-self.capacity:
		default:
		}
		promote := allowPromotion && self.memory.budget == nil && limit < self.maximumQueue
		self.stateLock.Unlock()

		if promote {
			// No queue lock is held across the shared budget's admission lock.
			memory, admitted := reserveNatMemory(self.workspace.budget, self.workspace.incrementBytes)
			if self.afterFundingAttemptForTest != nil {
				self.afterFundingAttemptForTest(admitted)
			}
			if admitted {
				self.stateLock.Lock()
				if self.closed || ctx.Err() != nil {
					self.stateLock.Unlock()
					memory.release()
					return false
				}
				// The consumer may have drained while admission ran. Do not
				// publish speculative funding after that actual demand vanished.
				if self.memory.budget == nil && len(self.packets) >= self.coldBatch {
					self.memory = memory
					self.workspace.funded.Store(true)
					memory = natMemoryReservation{}
				}
				self.stateLock.Unlock()
				memory.release()
				continue
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-self.capacity:
		}
	}
}

// Takes all returned roots from the queue. Only the existing consumer calls
// this; finishBatch must follow the synchronous callback before another take.
// A positive maximum is used by the original one-packet teardown drain.
func (self *tcpReturnReadQueue) takeBatch(ctx context.Context, maximum int) ([][]byte, bool) {
	if self.ordinary != nil {
		select {
		case <-ctx.Done():
			return nil, false
		case packet, ok := <-self.ordinary:
			if !ok {
				return nil, false
			}
			self.batch = append(self.batch[:0], packet)
		}
		limit := self.maximumBatch
		if maximum > 0 {
			limit = min(limit, maximum)
		}
		for len(self.batch) < limit {
			select {
			case packet, ok := <-self.ordinary:
				if !ok {
					return self.batch, true
				}
				self.batch = append(self.batch, packet)
			default:
				return self.batch, true
			}
		}
		return self.batch, true
	}
	for {
		if ctx.Err() != nil {
			return nil, false
		}
		self.stateLock.Lock()
		if self.callback {
			self.stateLock.Unlock()
			panic("TCP read queue reused before callback return")
		}
		if len(self.packets) != 0 {
			limit := min(self.coldBatch, self.maximumBatch)
			if self.memory.budget != nil {
				limit = self.maximumBatch
			}
			if maximum > 0 {
				limit = min(limit, maximum)
			}
			count := min(len(self.packets), limit)
			self.batch = append(self.batch[:0], self.packets[:count]...)
			copy(self.packets, self.packets[count:])
			clear(self.packets[len(self.packets)-count:])
			self.packets = self.packets[:len(self.packets)-count]
			self.callback = true
			notifyTcpReturnQueue(self.capacity)
			self.stateLock.Unlock()
			return self.batch, true
		}
		closed := self.closed
		select {
		case <-self.ready:
		default:
		}
		self.stateLock.Unlock()
		if closed {
			return nil, false
		}
		select {
		case <-ctx.Done():
			return nil, false
		case <-self.ready:
		}
	}
}

// The callback has transferred or returned all packet shares before this
// boundary. Producer packetization stays within its configured prepaid count.
func (self *tcpReturnReadQueue) finishBatch() {
	clear(self.batch)
	self.batch = self.batch[:0]
	if self.ordinary != nil {
		return
	}
	self.stateLock.Lock()
	if !self.callback || self.workspace.inUse.Load() {
		self.stateLock.Unlock()
		panic("TCP read queue ended a live or absent callback")
	}
	self.callback = false
	var release natMemoryReservation
	if len(self.packets) <= self.coldBatch {
		release, self.memory = self.memory, natMemoryReservation{}
		self.workspace.funded.Store(false)
	}
	self.stateLock.Unlock()
	release.release()
}

// Only the socket producer closes publication; queued owners still drain.
func (self *tcpReturnReadQueue) close() {
	if self.ordinary != nil {
		close(self.ordinary)
		return
	}
	self.stateLock.Lock()
	self.closed = true
	notifyTcpReturnQueue(self.ready)
	notifyTcpReturnQueue(self.capacity)
	self.stateLock.Unlock()
}

// The sequence's existing worker join is the final fence for optional credit.
func (self *tcpReturnReadQueue) releaseAfterJoin() {
	if self == nil || self.ordinary != nil {
		return
	}
	self.stateLock.Lock()
	if !self.closed || len(self.packets) != 0 || self.callback || self.workspace.inUse.Load() {
		self.stateLock.Unlock()
		panic("TCP read queue still owns work after child join")
	}
	release := self.memory
	self.memory = natMemoryReservation{}
	self.workspace.funded.Store(false)
	self.stateLock.Unlock()
	release.release()
}
