package connect

import (
	"context"
	"testing"
	"time"
	"unsafe"
)

// Includes creation and synchronous teardown, so owner allocations and
// reservations cannot be hidden by a warmed queue or unjoined goroutine.
func BenchmarkP2pLegacyProbeQueueOwner(b *testing.B) {
	budget := NewTransferMemoryBudget(kib(512))
	write := func([]byte, time.Time) error { return nil }
	b.ReportAllocs()
	for b.Loop() {
		ctx, cancel := context.WithCancel(context.Background())
		q := newP2pLegacySendQueue(ctx, cancel, write, kib(256), budget)
		if q == nil {
			b.Fatal("queue admission failed")
		}
		q.stopAndWait()
	}
	if budget.UsedByteCount() != 0 {
		b.Fatal("owner retained a budget reservation")
	}
	b.ReportMetric(float64(unsafe.Sizeof(p2pLegacySendQueue{})), "state-B")
}
