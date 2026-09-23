package connect

import (
	"context"
	"encoding/binary"
	"sync"
	"time"
	"unsafe"
)

const (
	p2pLegacySendSlabByteCount   = 8192
	p2pLegacySendHeaderByteCount = 10
	// Reserve the fixed entry table, queue state and one worker's initial stack
	// before construction. Packet roots have independent live reservations.
	p2pLegacySendQueueOwnerByteCount ByteCount = 8 * 1024
	// A lightly loaded carrier transfers existing roots without copying. Only
	// a sustained backlog needs compact slab storage.
	p2pLegacySendRawEntryCount = 4
)

type p2pLegacySendMemoryBudget interface {
	legacySendMemoryBudget() *TransferMemoryBudget
}

func (self *peerConn) legacySendMemoryBudget() *TransferMemoryBudget {
	return self.admissionBudget
}

// One producer (P2pSendTransport.run) owns admission. The worker serializes all
// physical SCTP writes. Every accepted root is charged until its final write,
// including the root borrowed by the currently blocked SCTP call.
type p2pLegacySendQueue struct {
	ctx          context.Context
	cancel       context.CancelFunc
	write        func([]byte, time.Time) error
	budget       *TransferMemoryBudget
	ownerCharge  ByteCount
	limit        ByteCount
	epoch        time.Time
	mutex        sync.Mutex
	entries      []p2pLegacySendEntry
	head, count  int
	retained     ByteCount
	peakRetained ByteCount
	closed       bool
	err          error
	ready        chan struct{}
	capacity     chan struct{}
	done         chan struct{}
	ownerRelease sync.Once
	// Endpoint probes bypass the bulk backlog at the physical writer, including
	// when the producer is blocked on capacity or an ordinary FIFO barrier.
	probeSender *P2pSendTransport
	probe       p2pLegacySendProbe
}

type p2pLegacySendProbe struct {
	bytes    []byte
	deadline time.Time
}

type p2pLegacySendEntry struct {
	bytes         []byte
	read, written int
	deadline      time.Time
	charge        ByteCount
	compact       bool
}

func newP2pLegacySendQueue(ctx context.Context, cancel context.CancelFunc, write func([]byte, time.Time) error, limit ByteCount, budget *TransferMemoryBudget) *p2pLegacySendQueue {
	return newP2pLegacySendQueueWithProbes(ctx, cancel, write, limit, budget, nil)
}

func newP2pLegacySendQueueWithProbes(ctx context.Context, cancel context.CancelFunc, write func([]byte, time.Time) error, limit ByteCount, budget *TransferMemoryBudget, probeSender *P2pSendTransport) *p2pLegacySendQueue {
	if limit < p2pLegacySendSlabByteCount+MessagePoolMetaByteCount {
		return nil
	}
	entryCount := int(limit/p2pLegacySendSlabByteCount) + p2pLegacySendRawEntryCount
	ownerCharge := p2pLegacySendQueueOwnerByteCount + ByteCount(entryCount)*ByteCount(unsafe.Sizeof(p2pLegacySendEntry{}))
	if budget != nil && !budget.TryReserve(ownerCharge) {
		return nil
	}
	q := &p2pLegacySendQueue{
		ctx: ctx, cancel: cancel, write: write, budget: budget, limit: limit, ownerCharge: ownerCharge,
		epoch:   time.Now(),
		entries: make([]p2pLegacySendEntry, entryCount),
		ready:   make(chan struct{}, 1), capacity: make(chan struct{}, 1), done: make(chan struct{}),
		probeSender: probeSender,
	}
	go q.run()
	return q
}

// The sole producer retains this root until the worker has finished borrowing
// it. This is the same one-message ownership as a synchronous physical write;
// no extra probe queue or packet budget is introduced.
func (q *p2pLegacySendQueue) enqueueProbe(wire []byte, deadline time.Time) error {
	defer MessagePoolReturn(wire)
	q.mutex.Lock()
	if q.closed || q.ctx.Err() != nil {
		q.mutex.Unlock()
		return context.Canceled
	}
	q.probe = p2pLegacySendProbe{bytes: wire, deadline: deadline}
	q.mutex.Unlock()
	notifyP2pLegacySendQueue(q.ready)
	// Cancellation must still join a physical write borrowing this root. The
	// worker clears it and wakes the existing capacity signal on every exit.
	for {
		q.mutex.Lock()
		pending, closed, err := q.probe.bytes != nil, q.closed, q.err
		q.mutex.Unlock()
		if !pending {
			if closed && err == nil {
				return context.Canceled
			}
			return err
		}
		<-q.capacity
	}
}

func (q *p2pLegacySendQueue) reserveWithLock(charge ByteCount) bool {
	if q.limit-q.retained < charge || (q.budget != nil && !q.budget.TryReserve(charge)) {
		return false
	}
	q.retained += charge
	q.peakRetained = max(q.peakRetained, q.retained)
	return true
}

func (q *p2pLegacySendQueue) releaseEntryWithLock(entry *p2pLegacySendEntry) {
	MessagePoolReturn(entry.bytes)
	q.retained -= entry.charge
	if q.budget != nil {
		q.budget.Release(entry.charge)
	}
	*entry = p2pLegacySendEntry{}
}

func notifyP2pLegacySendQueue(channel chan struct{}) {
	select {
	case channel <- struct{}{}:
	default:
	}
}

// enqueue consumes wire on every outcome. Control/large messages drain all
// earlier writes and complete synchronously. A full queue waits only for the
// next released entry, keeping its existing backlog moving. An empty queue
// uses synchronous fallback when no byte budget is available, so unrelated
// connections cannot stop an otherwise writable carrier.
func (q *p2pLegacySendQueue) enqueue(wire []byte, deadline time.Time, synchronous bool) error {
	if synchronous || len(wire)+p2pLegacySendHeaderByteCount > p2pLegacySendSlabByteCount {
		defer MessagePoolReturn(wire)
		if err := q.flush(); err != nil {
			return err
		}
		return q.write(wire, deadline)
	}
	for {
		q.mutex.Lock()
		if q.closed || q.ctx.Err() != nil {
			q.mutex.Unlock()
			MessagePoolReturn(wire)
			return context.Canceled
		}
		var tail *p2pLegacySendEntry
		if q.count > 0 {
			tail = &q.entries[(q.head+q.count-1)%len(q.entries)]
		}
		if tail != nil && tail.compact && len(tail.bytes)-tail.written >= len(wire)+p2pLegacySendHeaderByteCount {
			q.appendCompactWithLock(tail, wire, deadline)
			q.mutex.Unlock()
			MessagePoolReturn(wire)
			return nil
		}
		if q.count < len(q.entries) {
			compact := q.count >= p2pLegacySendRawEntryCount
			charge := ByteCount(cap(wire))
			if compact {
				charge = p2pLegacySendSlabByteCount + MessagePoolMetaByteCount
			}
			if q.reserveWithLock(charge) {
				entry := &q.entries[(q.head+q.count)%len(q.entries)]
				if compact {
					*entry = p2pLegacySendEntry{bytes: MessagePoolGet(p2pLegacySendSlabByteCount), charge: charge, compact: true}
					q.appendCompactWithLock(entry, wire, deadline)
					MessagePoolReturn(wire)
				} else {
					*entry = p2pLegacySendEntry{bytes: wire, charge: charge, deadline: deadline}
				}
				q.count++
				q.mutex.Unlock()
				notifyP2pLegacySendQueue(q.ready)
				return nil
			}
		}
		empty := q.count == 0
		q.mutex.Unlock()
		if empty {
			defer MessagePoolReturn(wire)
			return q.write(wire, deadline)
		}
		select {
		case <-q.ctx.Done():
			MessagePoolReturn(wire)
			return context.Canceled
		case <-q.capacity:
		}
	}
}

func (q *p2pLegacySendQueue) appendCompactWithLock(entry *p2pLegacySendEntry, wire []byte, deadline time.Time) {
	buffer := entry.bytes[entry.written:]
	binary.BigEndian.PutUint16(buffer, uint16(len(wire)))
	// Offset from a local monotonic-bearing Time retains deadline semantics
	// without storing a 24-byte Time per compact packet or using wall time.
	offset := int64(-1)
	if !deadline.IsZero() {
		offset = int64(deadline.Sub(q.epoch))
	}
	binary.BigEndian.PutUint64(buffer[2:], uint64(offset))
	copy(buffer[p2pLegacySendHeaderByteCount:], wire)
	entry.written += p2pLegacySendHeaderByteCount + len(wire)
}

func (q *p2pLegacySendQueue) flush() error {
	for {
		q.mutex.Lock()
		closed, err, count := q.closed, q.err, q.count
		q.mutex.Unlock()
		if closed || q.ctx.Err() != nil {
			if err != nil {
				return err
			}
			return context.Canceled
		}
		if count == 0 {
			return nil
		}
		select {
		case <-q.ctx.Done():
			return context.Canceled
		case <-q.capacity:
		}
	}
}

// The single producer calls this only after its final enqueue/direct write
// has returned. Worker cancellation alone cannot release metadata still held
// by a synchronous no-budget write on that producer.
func (q *p2pLegacySendQueue) stopAndWait() {
	q.cancel()
	<-q.done
	q.ownerRelease.Do(func() {
		if q.budget != nil {
			q.budget.Release(q.ownerCharge)
		}
	})
}

func (q *p2pLegacySendQueue) run() {
	defer close(q.done)
	defer func() {
		q.mutex.Lock()
		q.closed = true
		for index := range q.entries {
			if q.entries[index].bytes != nil {
				q.releaseEntryWithLock(&q.entries[index])
			}
		}
		q.count = 0
		q.probe = p2pLegacySendProbe{}
		q.mutex.Unlock()
		notifyP2pLegacySendQueue(q.capacity)
	}()
	probeBurst := 0
	for {
		if q.ctx.Err() != nil {
			return
		}
		q.mutex.Lock()
		if (q.count == 0 || probeBurst < 2) && q.probe.bytes != nil {
			probe := q.probe
			q.mutex.Unlock()
			err := q.write(probe.bytes, probe.deadline)
			q.mutex.Lock()
			q.probe = p2pLegacySendProbe{}
			if err != nil {
				q.err, q.closed = err, true
			}
			q.mutex.Unlock()
			notifyP2pLegacySendQueue(q.capacity)
			if err != nil {
				q.cancel()
				return
			}
			probeBurst++
			continue
		}
		if q.count == 0 {
			q.mutex.Unlock()
			select {
			case <-q.ctx.Done():
				return
			case <-q.ready:
			}
			continue
		}
		if probeBurst < 2 && q.probeSender != nil {
			if probe := q.probeSender.takePendingProbe(probeBurst); probe != nil {
				// A bulk entry stays counted while this control is written, so
				// flush cannot let the producer start a concurrent SCTP write.
				q.mutex.Unlock()
				err := q.write(probe, time.Time{})
				MessagePoolReturn(probe)
				if err != nil {
					q.mutex.Lock()
					q.err, q.closed = err, true
					q.mutex.Unlock()
					q.cancel()
					return
				}
				probeBurst++
				continue
			}
		}
		entry := &q.entries[q.head]
		wire, deadline := entry.bytes, entry.deadline
		if entry.compact {
			buffer := entry.bytes[entry.read:]
			length := int(binary.BigEndian.Uint16(buffer))
			offset := int64(binary.BigEndian.Uint64(buffer[2:]))
			if offset != -1 {
				deadline = q.epoch.Add(time.Duration(offset))
			}
			wire = buffer[p2pLegacySendHeaderByteCount : p2pLegacySendHeaderByteCount+length]
		}
		q.mutex.Unlock()
		err := q.write(wire, deadline)
		probeBurst = 0
		q.mutex.Lock()
		if err != nil {
			q.err = err
			q.closed = true
			q.mutex.Unlock()
			q.cancel()
			return
		}
		if entry.compact {
			entry.read += len(wire) + p2pLegacySendHeaderByteCount
		}
		if !entry.compact || entry.read == entry.written {
			q.releaseEntryWithLock(entry)
			q.head = (q.head + 1) % len(q.entries)
			q.count--
		}
		q.mutex.Unlock()
		notifyP2pLegacySendQueue(q.capacity)
	}
}
