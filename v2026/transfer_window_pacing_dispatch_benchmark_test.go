package connect

import (
	"testing"
	"time"
)

// Price the actual reservation/FIFO bookkeeping separately from virtual-time
// throughput. The existing reusable waiter is warmed before measurement; no
// packet payload, timer or receiver-ring scan is allocated per reservation.
func BenchmarkWindowPacingDispatchReservation(b *testing.B) {
	service := newWindowPacingService(DefaultSendBufferSettings())
	at := time.Unix(1700000000, 0)
	service.observeReceiverRoundTrip(1, 10300*time.Microsecond, 300*time.Microsecond, 10*time.Millisecond, at)
	waiter := &windowPacingWaiter{ready: make(chan struct{}, 1)}
	b.ReportAllocs()
	for b.Loop() {
		at = at.Add(10 * time.Microsecond)
		service.reserve(at, 1250, 125000000, 125000000, 0, 0, false, waiter)
		service.stateLock.Lock()
		service.removeWaiterWithLock(waiter)
		service.pacingReservations--
		service.sent -= 1250
		service.reservedByteCount -= 1250
		service.stateLock.Unlock()
	}
}
