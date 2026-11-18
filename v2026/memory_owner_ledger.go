package connect

import (
	"sync/atomic"
	"unsafe"
)

// TransferMemoryOwnerLedger is optional, fixed-size lifecycle accounting. Set
// ClientSettings.MemoryOwnerLedger before constructing clients. A ledger can be
// shared by a chosen group of clients without retaining those clients. The zero
// value is ready for use; a nil pointer disables all accounting.
//
// This counts buffer-admitted workers through their final cleanup, including
// workers no longer present in lookup maps. It does not scan owners, acquire
// their locks, inspect the heap, or add per-packet work. Known bytes cover sequence
// structs and channel element storage only: not payload backing, channel headers,
// allocator spans, maps, pacing/route state, callbacks, or other goroutines.
// Directly constructed sequences outside the client buffers are outside scope.
// A ledger must not be copied after first use.
type TransferMemoryOwnerLedger struct {
	writers  atomic.Int64
	revision atomic.Uint64
	send     transferMemoryOwnerCounters
	receive  transferMemoryOwnerCounters
	forward  transferMemoryOwnerCounters
}

type transferMemoryOwnerCounters struct {
	workers      atomic.Int64
	cleanup      atomic.Int64
	channelBytes atomic.Int64
	cleanupBytes atomic.Int64
	admitted     atomic.Uint64
	finished     atomic.Uint64
}

type TransferMemoryOwnerGroup struct {
	Workers                      int64  `json:"workers"`
	CleanupWorkers               int64  `json:"cleanup_workers"`
	KnownSequenceStructBytes     int64  `json:"known_sequence_struct_bytes"`
	KnownChannelSlotBytes        int64  `json:"known_channel_slot_bytes"`
	CleanupKnownChannelSlotBytes int64  `json:"cleanup_known_channel_slot_bytes"`
	AdmittedTotal                uint64 `json:"admitted_total"`
	FinishedTotal                uint64 `json:"finished_total"`
}

type TransferMemoryOwnerSnapshot struct {
	Enabled  bool                     `json:"enabled"`
	Complete bool                     `json:"complete"`
	Revision uint64                   `json:"revision"`
	Send     TransferMemoryOwnerGroup `json:"send"`
	Receive  TransferMemoryOwnerGroup `json:"receive"`
	Forward  TransferMemoryOwnerGroup `json:"forward"`
}

// Snapshot uses a fixed number of atomic loads, with no retries, allocation,
// owner traversal, or application mutex. Complete is false if lifecycle updates
// overlap the read; such a sample must not be treated as a coherent census.
// CleanupWorkers means Run returned and buffer-owned cleanup has begun, not
// merely that a context was canceled or that all lookup entries are absent.
func (l *TransferMemoryOwnerLedger) Snapshot() TransferMemoryOwnerSnapshot {
	return l.snapshot(nil)
}

// boundary is used only by deterministic interleaving controls. Production
// callers pass nil; the ledger does not retain the callback or traverse owners.
func (l *TransferMemoryOwnerLedger) snapshot(boundary func(final bool)) TransferMemoryOwnerSnapshot {
	if l == nil {
		return TransferMemoryOwnerSnapshot{}
	}
	beforeWriters := l.writers.Load()
	beforeRevision := l.revision.Load()
	if boundary != nil {
		boundary(false)
	}
	out := TransferMemoryOwnerSnapshot{
		Enabled: true,
		Send:    l.send.snapshot(int64(unsafe.Sizeof(SendSequence{}))),
		Receive: l.receive.snapshot(int64(unsafe.Sizeof(ReceiveSequence{}))),
		Forward: l.forward.snapshot(int64(unsafe.Sizeof(ForwardSequence{}))),
	}
	// Read the active-writer count before the closing revision. Reversing
	// these loads lets a writer finish between them: a partial group can then
	// carry the old revision and a zero writer count and appear coherent.
	afterWriters := l.writers.Load()
	if boundary != nil {
		boundary(true)
	}
	out.Revision = l.revision.Load()
	out.Complete = beforeWriters == 0 && afterWriters == 0 && beforeRevision == out.Revision
	return out
}

func (c *transferMemoryOwnerCounters) snapshot(structBytes int64) TransferMemoryOwnerGroup {
	workers := c.workers.Load()
	return TransferMemoryOwnerGroup{
		Workers: workers, CleanupWorkers: c.cleanup.Load(),
		KnownSequenceStructBytes: workers * structBytes,
		KnownChannelSlotBytes:    c.channelBytes.Load(), CleanupKnownChannelSlotBytes: c.cleanupBytes.Load(),
		AdmittedTotal: c.admitted.Load(), FinishedTotal: c.finished.Load(),
	}
}

type transferMemoryOwnerKind uint8

const (
	transferMemoryOwnerSend transferMemoryOwnerKind = iota
	transferMemoryOwnerReceive
	transferMemoryOwnerForward
)

func (l *TransferMemoryOwnerLedger) counters(kind transferMemoryOwnerKind) *transferMemoryOwnerCounters {
	switch kind {
	case transferMemoryOwnerSend:
		return &l.send
	case transferMemoryOwnerReceive:
		return &l.receive
	default:
		return &l.forward
	}
}

func (l *TransferMemoryOwnerLedger) admit(kind transferMemoryOwnerKind, bytes int64) {
	if l == nil {
		return
	}
	l.writers.Add(1)
	c := l.counters(kind)
	c.workers.Add(1)
	c.channelBytes.Add(bytes)
	c.admitted.Add(1)
	l.revision.Add(1)
	l.writers.Add(-1)
}

func (l *TransferMemoryOwnerLedger) beginCleanup(kind transferMemoryOwnerKind, bytes int64) {
	if l == nil {
		return
	}
	l.writers.Add(1)
	c := l.counters(kind)
	c.cleanup.Add(1)
	c.cleanupBytes.Add(bytes)
	l.revision.Add(1)
	l.writers.Add(-1)
}

func (l *TransferMemoryOwnerLedger) finish(kind transferMemoryOwnerKind, bytes int64, cleanup bool) {
	if l == nil {
		return
	}
	l.writers.Add(1)
	c := l.counters(kind)
	if cleanup {
		c.cleanup.Add(-1)
		c.cleanupBytes.Add(-bytes)
	}
	c.workers.Add(-1)
	c.channelBytes.Add(-bytes)
	c.finished.Add(1)
	l.revision.Add(1)
	l.writers.Add(-1)
}

func (s *SendSequence) memoryOwnerChannelBytes() int64 {
	return int64(cap(s.packs))*int64(unsafe.Sizeof((*SendPack)(nil))) +
		int64(cap(s.acks))*int64(unsafe.Sizeof(receiveAckMessage{}))
}

func (s *ReceiveSequence) memoryOwnerChannelBytes() int64 {
	return int64(cap(s.packs)) * int64(unsafe.Sizeof((*ReceivePack)(nil)))
}

func (s *ForwardSequence) memoryOwnerChannelBytes() int64 {
	return int64(cap(s.packs)) * int64(unsafe.Sizeof((*ForwardPack)(nil)))
}
