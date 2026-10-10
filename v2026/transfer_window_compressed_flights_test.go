// Window-limited flights contain an idle gap even while their earliest reply
// has already released enough capacity to keep the next tail outstanding.
package connect

import (
	"testing"
	"time"
)

// A complete compression interval is not a serialization interval when it
// crosses two small-window flights. Force that ordering without a scheduler.
func TestWindowPacingCompressedFlightsKeepMeasuredSerialization(t *testing.T) {
	start := time.Unix(1700000000, 0)
	service := newWindowPacingService(DefaultSendBufferSettings())
	for i := 0; i <= 8; i++ {
		at := start.Add(time.Duration(i) * 50 * time.Millisecond)
		service.sent += 625000
		service.observeReceiverRoundTrip(0, 150*time.Millisecond, 100*time.Millisecond, 50*time.Millisecond, at)
		service.observe(625000, at)
	}
	at := start.Add(400 * time.Millisecond)
	prior, _, _ := service.measured(time.Second, at)
	if prior != 12500000 {
		t.Fatalf("opening train did not establish 12.5 MB/s: %d", prior)
	}
	sequence, oldTail, first, tail, next, nextTail := NewId(), NewId(), NewId(), NewId(), NewId(), NewId()
	service.beginWrite(sequence, oldTail, 1, at.Add(-time.Millisecond), false)
	service.finishWrite(sequence, oldTail, true)
	service.acknowledgeWrite(sequence, oldTail, 1, false, 50*time.Millisecond, at)
	if !service.drained {
		t.Fatal("old flight must be physically acknowledged before window shrink")
	}
	write := func(id Id, number uint64, bytes ByteCount, when time.Time) {
		service.sent += bytes
		service.beginWrite(sequence, id, number, when, false)
		service.finishWrite(sequence, id, true)
	}
	ack := func(id Id, number uint64, bytes ByteCount, elapsed time.Duration) {
		arrival := at.Add(elapsed)
		service.observeReceiverRoundTrip(0, 110*time.Millisecond, 100*time.Millisecond, 50*time.Millisecond, arrival)
		service.acknowledgeWrite(sequence, id, number, false, 50*time.Millisecond, arrival)
		service.observe(bytes, arrival)
	}
	write(first, 2, 2673, at)
	write(tail, 3, 64104, at)
	ack(first, 2, 2673, 100213840*time.Nanosecond)
	// The first reply reopens one message of capacity before the old tail.
	write(next, 4, 2671, at.Add(100213840*time.Nanosecond))
	ack(tail, 3, 64104, 110342160*time.Nanosecond)
	write(nextTail, 5, 64104, at.Add(110342160*time.Nanosecond))
	if outstanding := service.sent - service.total; outstanding != 66775 || service.drained {
		t.Fatalf("fixture lost the rolling small-window boundary: outstanding=%d drained=%t", outstanding, service.drained)
	}
	ack(next, 4, 2671, 200427520*time.Nanosecond)
	rate, _, latest := service.measured(time.Second, at.Add(200427520*time.Nanosecond))
	if got := max(rate, latest); got < prior*9/10 {
		t.Fatalf("window idle replaced measured serialization: %d -> %d", prior, got)
	}
}
