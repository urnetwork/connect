// A small queued tail cannot price a later refill silence, and a fresh
// serialization train cannot inherit the silence before a propagation step.
package connect

import (
	"testing"
	"time"
)

// Receiver timing can expose a modest queue inside a small compressed flight.
// Its later refill still leaves a much longer idle gap between physical trains.
func TestWindowPacingLimitedFlightModestQueueKeepsSerialization(t *testing.T) {
	service, start := newWindowQualifiedServiceFixture(t, 12500000, 50*time.Millisecond, 100*time.Millisecond)
	sequence, oldTail, first, tail, next, nextTail := NewId(), NewId(), NewId(), NewId(), NewId(), NewId()
	service.beginWrite(sequence, oldTail, 1, start, false)
	service.finishWrite(sequence, oldTail, true)
	service.acknowledgeWrite(sequence, oldTail, 1, false, 50*time.Millisecond, start)
	write := func(id Id, number uint64, bytes ByteCount, at time.Time) {
		service.sent += bytes
		service.beginWrite(sequence, id, number, at, false)
		service.finishWrite(sequence, id, true)
	}
	ack := func(id Id, number uint64, bytes ByteCount, elapsed, raw, adjusted time.Duration) {
		at := start.Add(elapsed)
		service.observeReceiverRoundTrip(0, raw, adjusted, 50*time.Millisecond, at)
		service.acknowledgeWrite(sequence, id, number, false, 50*time.Millisecond, at)
		service.observe(bytes, at)
	}
	write(first, 2, 2673, start)
	write(tail, 3, 64104, start)
	ack(first, 2, 2673, 100213840*time.Nanosecond, 100213840*time.Nanosecond, 100213840*time.Nanosecond)
	write(next, 4, 2671, start.Add(100213840*time.Nanosecond))
	// This ten-millisecond queue is real, but does not cover the following
	// ninety-millisecond gap while the small window waits for another reply.
	ack(tail, 3, 64104, 110342160*time.Nanosecond, 110342160*time.Nanosecond, 110317160*time.Nanosecond)
	write(nextTail, 5, 64104, start.Add(110342160*time.Nanosecond))
	ack(next, 4, 2671, 200427520*time.Nanosecond, 100213680*time.Nanosecond, 100213680*time.Nanosecond)
	if service.drained || service.sent-service.total != 64104 {
		t.Fatalf("the next small flight must remain physically outstanding: drained=%t outstanding=%d", service.drained, service.sent-service.total)
	}
	rate, _, latest := service.measured(time.Second, start.Add(200427520*time.Nanosecond))
	if max(rate, latest) != 12500000 {
		t.Fatalf("a modest queued tail priced the later refill silence: %d/%d", rate, latest)
	}
}

// A preoffered physical flight resumes after a propagation change. Three
// fresh checkpoints measure serialization while the older silence remains
// just inside the bounded ring; no empty-flight proof can refresh the floor.
func newWindowServicePropagationGapFixture(t *testing.T, bytes ByteCount) (*windowPacingService, time.Time) {
	t.Helper()
	service, start := newWindowQualifiedServiceFixture(t, 12563100, 10*time.Millisecond, 300*time.Microsecond)
	sequence, tail := NewId(), NewId()
	service.sent += 8000000
	service.beginWrite(sequence, tail, 1, start, false)
	service.finishWrite(sequence, tail, true)
	for i := range 3 {
		elapsed := 600*time.Millisecond + time.Duration(i)*10*time.Millisecond
		at := start.Add(elapsed)
		service.observeReceiverRoundTrip(0, elapsed, elapsed-10*time.Millisecond, 10*time.Millisecond, at)
		service.observe(bytes, at)
	}
	if service.drained || service.sent-service.total <= 0 {
		t.Fatal("the new feedback must still belong to a physically outstanding flight")
	}
	return service, start.Add(620 * time.Millisecond)
}

// The fresh train differs slightly from the older measured peak because ack
// checkpoints include whole messages. That phase uncertainty cannot admit a
// six-hundred-millisecond silence into an otherwise supported service rate.
func TestWindowPacingFreshTrainRejectsPriorPropagationGap(t *testing.T) {
	service, at := newWindowServicePropagationGapFixture(t, 125000)
	rate, _, latest := service.measured(time.Second, at)
	if max(rate, latest) != 12500000 {
		t.Fatalf("fresh serialization inherited an older propagation silence: %d/%d", rate, latest)
	}
}

// Even including the uncertain first checkpoint, this new train supplies
// only 12.3 MB/s over its measured interval, below the 12.5631 MB/s hold.
// The endpoint allowance must not block a genuine slowdown near that edge.
func TestWindowPacingFreshTrainBelowEndpointAllowanceLowersService(t *testing.T) {
	service, at := newWindowServicePropagationGapFixture(t, 82000)
	rate, _, latest := service.measured(time.Second, at)
	if got := max(rate, latest); got <= 0 || got > 8200000 {
		t.Fatalf("a fresh slower train could not lower service: %d/%d", rate, latest)
	}
}

// Repeated continuous feedback removes the single-checkpoint uncertainty.
// A modest sustained slowdown must settle at its actual serialization rate
// without any path signal, global drain, or old-rate amount of delivered data.
func TestWindowPacingFreshTrainSustainedSlowdownReplacesHold(t *testing.T) {
	service, at := newWindowServicePropagationGapFixture(t, 100000)
	for i := 0; i <= 8; i++ {
		if i > 0 {
			at = at.Add(10 * time.Millisecond)
			service.observeReceiverRoundTrip(0, 620*time.Millisecond+time.Duration(i)*10*time.Millisecond, 610*time.Millisecond+time.Duration(i)*10*time.Millisecond, 10*time.Millisecond, at)
			service.observe(100000, at)
		}
		rate, _, latest := service.measured(time.Second, at)
		if i >= 4 && max(rate, latest) != 10000000 {
			t.Fatalf("continuous slow turn=%d retained or understated service: %d/%d", i, rate, latest)
		}
	}
}
