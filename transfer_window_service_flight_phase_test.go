// Repeated small flights change the phase between immediate and compressed
// replies. That phase drift does not turn refill silence into serialization.
package connect

import (
	"testing"
	"time"
)

// Four rolling flights reproduce the actual worker's later failure: a refill
// gap eventually fits the advertised compression allowance despite near-zero
// measured receiver wait. An older tail always overlaps the next first write.
func TestWindowPacingLimitedFlightAckPhaseDoesNotCompleteService(t *testing.T) {
	service, start := newWindowQualifiedServiceFixture(t, 12500000, 50*time.Millisecond, 100*time.Millisecond)
	sequence := NewId()
	var messages [10]Id
	for i := range messages {
		messages[i] = NewId()
	}
	service.beginWrite(sequence, messages[1], 1, start, false)
	service.finishWrite(sequence, messages[1], true)
	service.acknowledgeWrite(sequence, messages[1], 1, false, 50*time.Millisecond, start)
	write := func(number uint64, bytes ByteCount, at time.Time) {
		service.sent += bytes
		service.beginWrite(sequence, messages[number], number, at, false)
		service.finishWrite(sequence, messages[number], true)
	}
	write(2, 2673, start)
	write(3, 64104, start)
	arrivals := []struct {
		elapsed, raw, adjusted time.Duration
		bytes                  ByteCount
	}{
		{elapsed: 100213840 * time.Nanosecond, raw: 100213840 * time.Nanosecond, adjusted: 100213840 * time.Nanosecond, bytes: 2673},
		{elapsed: 110342160 * time.Nanosecond, raw: 110342160 * time.Nanosecond, adjusted: 110317160 * time.Nanosecond, bytes: 64104},
		{elapsed: 200427520 * time.Nanosecond, raw: 100213680 * time.Nanosecond, adjusted: 100213680 * time.Nanosecond, bytes: 2671},
		{elapsed: 220470480 * time.Nanosecond, raw: 110128320 * time.Nanosecond, adjusted: 110103320 * time.Nanosecond, bytes: 64104},
		{elapsed: 300641200 * time.Nanosecond, raw: 100213680 * time.Nanosecond, adjusted: 100213680 * time.Nanosecond, bytes: 2671},
		{elapsed: 330598800 * time.Nanosecond, raw: 110128320 * time.Nanosecond, adjusted: 110103320 * time.Nanosecond, bytes: 64104},
		{elapsed: 400854880 * time.Nanosecond, raw: 100213680 * time.Nanosecond, adjusted: 100213680 * time.Nanosecond, bytes: 2671},
	}
	for i, arrival := range arrivals {
		at := start.Add(arrival.elapsed)
		number := uint64(i + 2)
		service.observeReceiverRoundTrip(0, arrival.raw, arrival.adjusted, 50*time.Millisecond, at)
		service.acknowledgeWrite(sequence, messages[number], number, false, 50*time.Millisecond, at)
		service.observe(arrival.bytes, at)
		if i != len(arrivals)-1 {
			bytes := ByteCount(2671)
			if i%2 == 1 {
				bytes = 64104
			}
			write(number+2, bytes, at)
		}
		if service.drained {
			t.Fatalf("arrival=%d: the refill lost its overlapping physical tail", i)
		}
		rate, _, latest := service.measured(time.Second, at)
		if max(rate, latest) != 12500000 {
			t.Fatalf("arrival=%d: drifting ack phase priced refill silence as service: %d/%d", i, rate, latest)
		}
	}
}
