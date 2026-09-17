// Physical offering and arrival provenance distinguish limited flights and
// buffered reader bursts from actual changes in serialization capacity.
package connect

import (
	"testing"
	"time"
)

// Establish service from repeated byte/time pairs, before any physical tail
// exists. Tests then supply the actual flight and arrival ordering separately.
func newWindowQualifiedServiceFixture(t *testing.T, rate ByteCount, compression, path time.Duration) (*windowPacingService, time.Time) {
	t.Helper()
	start := time.Unix(1700000000, 0)
	service := newWindowPacingService(DefaultSendBufferSettings())
	interval := max(10*time.Millisecond, compression)
	for i := 0; i <= 8; i++ {
		at := start.Add(time.Duration(i) * interval)
		bytes := ByteCount(float64(rate) * interval.Seconds())
		service.sent += bytes
		service.observeReceiverRoundTrip(0, path+compression, path, compression, at)
		service.observe(bytes, at)
	}
	at := start.Add(8 * interval)
	if measured, _, _ := service.measured(time.Second, at); measured != rate {
		t.Fatalf("initial service=%d, want %d", measured, rate)
	}
	return service, at
}

// A small preoffered flight still measures slow service when two arrivals
// span a real continuous interval. It need not deliver an old-rate window.
func TestWindowPacingLimitedFlightAcceptsContinuousSlowPair(t *testing.T) {
	service, start := newWindowQualifiedServiceFixture(t, 12500000, 50*time.Millisecond, 100*time.Millisecond)
	sequence, tail := NewId(), NewId()
	service.sent += 10000
	service.beginWrite(sequence, tail, 3, start, false)
	service.finishWrite(sequence, tail, true)
	for i := range 2 {
		at := start.Add(300*time.Millisecond + time.Duration(i)*50*time.Millisecond)
		service.observeReceiverRoundTrip(0, 150*time.Millisecond, 100*time.Millisecond, 50*time.Millisecond, at)
		service.observe(1000, at)
	}
	rate, _, latest := service.measured(time.Second, start.Add(350*time.Millisecond))
	if rate != 20000 || latest != 20000 {
		t.Fatalf("continuous small flight could not discover slow service: %d/%d", rate, latest)
	}
}

// A genuinely slow serializer adds queue residence even when its outstanding
// bytes fit below the earlier service allowance. Sparse delivery remains valid.
func TestWindowPacingLimitedFlightAcceptsQueuedSlowService(t *testing.T) {
	service, start := newWindowQualifiedServiceFixture(t, 12500000, 50*time.Millisecond, 100*time.Millisecond)
	sequence, tail := NewId(), NewId()
	service.sent += 10000
	service.beginWrite(sequence, tail, 3, start, false)
	service.finishWrite(sequence, tail, true)
	for i := range 2 {
		at := start.Add(300*time.Millisecond + time.Duration(i)*100*time.Millisecond)
		path := 100*time.Millisecond + time.Duration(i)*100*time.Millisecond
		service.observeReceiverRoundTrip(0, path+50*time.Millisecond, path, 50*time.Millisecond, at)
		service.observe(1000, at)
	}
	rate, _, latest := service.measured(time.Second, start.Add(400*time.Millisecond))
	if got := max(rate, latest); got != 10000 {
		t.Fatalf("queued small flight could not discover slow service: %d/%d", rate, latest)
	}
}

// Repeated small flights on a slower serializer must adapt without a path
// signal or global drain, even when every early reply immediately refills it.
func TestWindowPacingLimitedFlightRepeatedSlowRefillAdapts(t *testing.T) {
	for _, compression := range []time.Duration{0, 10 * time.Millisecond, 50 * time.Millisecond} {
		service, start := newWindowQualifiedServiceFixture(t, 12500000, compression, 100*time.Millisecond)
		sequence := NewId()
		type arrival struct {
			id       Id
			number   uint64
			sentAt   time.Time
			arriveAt time.Time
		}
		pending := make([]arrival, 0, 2)
		physicalFree := start
		number := uint64(0)
		write := func(at time.Time) {
			number++
			id := NewId()
			service.sent += 8000
			service.beginWrite(sequence, id, number, at, false)
			service.finishWrite(sequence, id, true)
			if physicalFree.Before(at) {
				physicalFree = at
			}
			// The independent serializer delivers 8000 wire bytes in 80 ms.
			physicalFree = physicalFree.Add(80 * time.Millisecond)
			pending = append(pending, arrival{id: id, number: number, sentAt: at,
				arriveAt: physicalFree.Add(100*time.Millisecond + compression)})
		}
		write(start)
		write(start)
		for i := range 8 {
			delivered := pending[0]
			pending = pending[1:]
			raw := delivered.arriveAt.Sub(delivered.sentAt)
			service.observeReceiverRoundTrip(0, raw, raw-compression, compression, delivered.arriveAt)
			service.acknowledgeWrite(sequence, delivered.id, delivered.number, false, compression, delivered.arriveAt)
			service.observe(8000, delivered.arriveAt)
			write(delivered.arriveAt)
			if service.drained {
				t.Fatalf("compression=%s step=%d: refill must overlap the older physical tail", compression, i)
			}
			rate, _, latest := service.measured(time.Second, delivered.arriveAt)
			if i >= 4 && (max(rate, latest) <= 0 || max(rate, latest) > 125000) {
				t.Fatalf("compression=%s step=%d: repeated slow delivery retained old capacity: %d/%d", compression, i, rate, latest)
			}
		}
	}
}

// Later workers may account for the newest reply first. That cannot merge
// the idle gaps between rolling small flights into a serialization interval.
func TestWindowPacingLimitedFlightReorderedAccountingKeepsService(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		service, start := newWindowQualifiedServiceFixture(t, 12500000, 50*time.Millisecond, 100*time.Millisecond)
		sequence, first, tail, next, nextTail := NewId(), NewId(), NewId(), NewId(), NewId()
		write := func(id Id, number uint64, bytes ByteCount, at time.Time) {
			service.sent += bytes
			service.beginWrite(sequence, id, number, at, false)
			service.finishWrite(sequence, id, true)
		}
		arrivals := []struct {
			id     Id
			number uint64
			bytes  ByteCount
			at     time.Time
		}{
			{id: first, number: 1, bytes: 2673, at: start.Add(100213840 * time.Nanosecond)},
			{id: tail, number: 2, bytes: 64104, at: start.Add(110342160 * time.Nanosecond)},
			{id: next, number: 3, bytes: 2671, at: start.Add(200427520 * time.Nanosecond)},
		}
		write(first, 1, 2673, start)
		write(tail, 2, 64104, start)
		for i, arrival := range arrivals {
			service.observeReceiverRoundTrip(0, 110*time.Millisecond, 100*time.Millisecond, 50*time.Millisecond, arrival.at)
			service.acknowledgeWrite(sequence, arrival.id, arrival.number, false, 50*time.Millisecond, arrival.at)
			if i == 0 {
				write(next, 3, 2671, arrival.at)
			} else if i == 1 {
				write(nextTail, 4, 64104, arrival.at)
			}
		}
		for i := range arrivals {
			index := i
			if reverse {
				index = len(arrivals) - 1 - i
			}
			service.observe(arrivals[index].bytes, arrivals[index].at)
		}
		for _, retain := range []bool{false, true} {
			rate, _, latest := service.measure(time.Second, arrivals[2].at, retain)
			if got := max(rate, latest); got != 12500000 {
				t.Errorf("reverse=%t retain=%t: a limited-flight gap replaced service: %d/%d", reverse, retain, rate, latest)
			}
		}
	}
}

// A physically completed batch is read over 240 microseconds after twenty
// milliseconds in a carrier queue. Its timestamp peak is not new capacity.
func newWindowBufferedPeakFixture(t *testing.T) (*windowPacingService, time.Time) {
	t.Helper()
	service, start := newWindowQualifiedServiceFixture(t, 12500000, 0, 300*time.Microsecond)
	sequence, tail := NewId(), NewId()
	service.sent += 50000
	service.beginWrite(sequence, tail, 1, start, false)
	service.finishWrite(sequence, tail, true)
	for i := range 25 {
		at := start.Add(20*time.Millisecond + time.Duration(i)*10*time.Microsecond)
		service.observeReceiverRoundTrip(0, 20*time.Millisecond, 20*time.Millisecond, 0, at)
		if i == 24 {
			service.acknowledgeWrite(sequence, tail, 1, false, 0, at)
		}
		service.observe(2000, at)
	}
	if !service.drained || service.sent != service.total {
		t.Fatal("the buffered batch must have physically drained and completed byte accounting")
	}
	return service, start.Add(20240 * time.Microsecond)
}

// Clearing queue residence cannot rehabilitate an older compressed peak.
// Statistics and controller reads must make the same evidence decision.
func TestWindowPacingCarrierPeakStaysUnqualifiedAfterQueueClears(t *testing.T) {
	service, at := newWindowBufferedPeakFixture(t)
	for _, retain := range []bool{false, true} {
		rate, _, latest := service.measure(time.Second, at, retain)
		if got := max(rate, latest); got != 12500000 {
			t.Errorf("retain=%t: buffered peak raised service: %d/%d", retain, rate, latest)
		}
	}
	at = at.Add(300 * time.Microsecond)
	service.sent++
	service.observeReceiverRoundTrip(0, 300*time.Microsecond, 300*time.Microsecond, 0, at)
	service.observe(1, at)
	rate, _, latest := service.measured(time.Second, at)
	if got := max(rate, latest); got != 12500000 {
		t.Fatalf("a clear newer round trip revived the old buffered peak: %d/%d", rate, latest)
	}
}

// Real fast delivery following the buffered batch supplies independent fresh
// evidence. Rejecting the old peak cannot cap that new train at the old hold.
func TestWindowPacingCarrierFreshFastPairSupersedesRejectedPeak(t *testing.T) {
	service, at := newWindowBufferedPeakFixture(t)
	for i := range 2 {
		arrival := at.Add(11*time.Millisecond + time.Duration(i)*100*time.Microsecond)
		service.sent += 2500
		service.observeReceiverRoundTrip(0, 300*time.Microsecond, 300*time.Microsecond, 0, arrival)
		service.observe(2500, arrival)
	}
	rate, _, latest := service.measured(time.Second, at.Add(11100*time.Microsecond))
	if got := max(rate, latest); got != 25000000 {
		t.Fatalf("fresh fast pair did not supersede the queued peak: %d/%d", rate, latest)
	}
}

// A mixed bucket conservatively keeps the earlier queued provenance. A later
// clean pair must finish that hold; the queue flag cannot become a rate ceiling.
func TestWindowPacingCarrierSameBucketHoldEndsAtFreshTrain(t *testing.T) {
	service, at := newWindowBufferedPeakFixture(t)
	for _, delay := range []time.Duration{300 * time.Microsecond, 11 * time.Millisecond} {
		for i := range 2 {
			arrival := at.Add(delay + time.Duration(i)*100*time.Microsecond)
			service.sent += 2500
			service.observeReceiverRoundTrip(0, 300*time.Microsecond, 300*time.Microsecond, 0, arrival)
			service.observe(2500, arrival)
		}
		rate, _, latest := service.measured(time.Second, at.Add(delay+100*time.Microsecond))
		want := ByteCount(12500000)
		if delay == 11*time.Millisecond {
			want = 25000000
		}
		if got := max(rate, latest); got != want {
			t.Fatalf("fresh delay=%s: queued provenance did not have a bounded hold: %d/%d want=%d", delay, rate, latest, want)
		}
	}
}

// A worker can account for queued bytes after newer clean timing has replaced
// the timing ring. The original arrival bucket must retain their provenance.
func TestWindowPacingCarrierLateAccountingRetainsQueueProvenance(t *testing.T) {
	service, start := newWindowQualifiedServiceFixture(t, 12500000, 0, 300*time.Microsecond)
	for i := range 25 {
		at := start.Add(20*time.Millisecond + time.Duration(i)*10*time.Microsecond)
		service.observeReceiverRoundTrip(0, 20*time.Millisecond, 20*time.Millisecond, 0, at)
	}
	clearAt := start.Add(21 * time.Millisecond)
	for i := range DefaultSendBufferSettings().RttWindowSize {
		service.observeReceiverRoundTrip(0, 300*time.Microsecond, 300*time.Microsecond, 0, clearAt.Add(time.Duration(i)*time.Nanosecond))
	}
	for i := 24; i >= 0; i-- {
		service.sent += 2000
		service.observe(2000, start.Add(20*time.Millisecond+time.Duration(i)*10*time.Microsecond))
	}
	rate, _, latest := service.measured(time.Second, clearAt.Add(time.Millisecond))
	if got := max(rate, latest); got != 12500000 {
		t.Fatalf("late accounting forgot the queue that compressed its arrival clock: %d/%d", rate, latest)
	}
}

// A queue can accompany a genuinely faster serializer. Its sustained actual
// byte/time interval must raise capacity even after the final tail drains.
func TestWindowPacingCarrierSustainedQueuedIncreaseRaisesService(t *testing.T) {
	service, start := newWindowQualifiedServiceFixture(t, 1000000, 0, 300*time.Microsecond)
	sequence, tail := NewId(), NewId()
	service.sent += 900000
	service.beginWrite(sequence, tail, 1, start, false)
	service.finishWrite(sequence, tail, true)
	for i := 0; i <= 8; i++ {
		at := start.Add(10*time.Millisecond + time.Duration(i)*10*time.Millisecond)
		service.observeReceiverRoundTrip(0, at.Sub(start), at.Sub(start), 0, at)
		if i == 8 {
			service.acknowledgeWrite(sequence, tail, 1, false, 0, at)
		}
		service.observe(100000, at)
	}
	rate, _, latest := service.measured(time.Second, start.Add(90*time.Millisecond))
	if got := max(rate, latest); got != 10000000 || !service.drained {
		t.Fatalf("sustained faster queued delivery was discarded: %d/%d drained=%t", rate, latest, service.drained)
	}
}
