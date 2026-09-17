// A confirmed drain can retire every old byte bucket while its measured
// service survives. The first new queued timing still owns its provenance.
package connect

import (
	"testing"
	"time"
)

// Exact probe and tail identities separate physical proof from late byte
// accounting. The legacy probe refreshes residence before any new byte applies;
// subsequent feedback may carry either legacy or paired receiver timing.
func checkWindowQueueEpochAccounting(t *testing.T, late bool) {
	t.Helper()
	for _, metadata := range []bool{false, true} {
		fixture := newWindowDrainFeedbackFixture(t, false, 2131032)
		service := fixture.service
		service.measured(time.Second, fixture.partialAt)
		dispatchAt := fixture.start.Add(1210300 * time.Microsecond)
		fixture.complete(dispatchAt)
		if delay, _ := service.admitBurst(dispatchAt, 2673, false, &fixture.waiter); delay != 0 {
			t.Fatal("the complete physical drain did not admit its exact probe")
		}
		probeSequence, probeId := NewId(), NewId()
		fixture.dispatch(probeSequence, probeId, dispatchAt)
		sequence, tail := NewId(), NewId()
		service.sent += 50000
		service.beginWrite(sequence, tail, 1, dispatchAt.Add(time.Microsecond), false)
		service.finishWrite(sequence, tail, true)
		probeAt := dispatchAt.Add(1200 * time.Millisecond)
		service.acknowledgeWrite(probeSequence, probeId, 1, false, 0, probeAt)
		if metadata {
			service.observeReceiverRoundTripForWrite(probeSequence, probeId, 0, 1200*time.Millisecond, 1200*time.Millisecond, 0, probeAt)
		}
		if timing := service.roundTripEvidence(probeAt); timing.minimum != 1200*time.Millisecond {
			t.Fatalf("metadata=%t: the exact physical probe did not refresh residence: %+v", metadata, timing)
		}
		rate, before, latest := service.measured(time.Second, probeAt)
		if rate != 0 || latest != 12500000 {
			t.Fatalf("metadata=%t: the new epoch must retain service without a fresh byte pair: %d/%d", metadata, rate, latest)
		}
		if !late {
			service.observe(2673, probeAt)
		}
		queuedAt := probeAt.Add(20 * time.Millisecond)
		observeTiming := func(raw time.Duration, at time.Time) {
			if metadata {
				service.observeReceiverRoundTrip(0, raw, raw, 0, at)
			} else {
				service.observeRoundTrip(raw, 0, at)
			}
		}
		for i := range 25 {
			at := queuedAt.Add(time.Duration(i) * 10 * time.Microsecond)
			observeTiming(at.Sub(dispatchAt.Add(time.Microsecond)), at)
			if !late {
				service.observe(2000, at)
			}
		}
		service.acknowledgeWrite(sequence, tail, 1, false, 0, queuedAt.Add(240*time.Microsecond))
		clearAt := queuedAt.Add(time.Millisecond)
		for i := range DefaultSendBufferSettings().RttWindowSize {
			observeTiming(1200*time.Millisecond, clearAt.Add(time.Duration(i)*time.Nanosecond))
		}
		if late {
			service.observe(2673, probeAt)
			for i := range 25 {
				service.observe(2000, queuedAt.Add(time.Duration(i)*10*time.Microsecond))
			}
		}
		rate, total, latest := service.measured(time.Second, clearAt.Add(time.Millisecond))
		if max(rate, latest) != 12500000 || total != before+52673 || !service.drained || service.sent != total {
			t.Errorf("metadata=%t late=%t: first post-epoch queue lost provenance or delivery: rate=%d/%d total=%d want=%d drained=%t sent=%d", metadata, late, rate, latest, total, before+52673, service.drained, service.sent)
		}
	}
}

// Clear newer timing may evict all queued tuples before either the probe's
// bytes or the following queued batch reaches the accounting owner.
func TestWindowPacingFirstQueuedTimingAfterEpochKeepsProvenance(t *testing.T) {
	checkWindowQueueEpochAccounting(t, true)
}

// Prompt accounting of the first post-drain byte supplies the ordinary ring
// entry before queued timing arrives; the same delivery remains qualified.
func TestWindowPacingPostDrainFirstBytesKeepQueueProvenance(t *testing.T) {
	checkWindowQueueEpochAccounting(t, false)
}

// Timing-only entries in an empty service are not delivered-byte checkpoints.
// A later clean pair still measures capacity without phantom endpoint bytes.
func TestWindowPacingColdQueuedTimingDoesNotInventDelivery(t *testing.T) {
	service := newWindowPacingService(DefaultSendBufferSettings())
	start := time.Unix(1700000000, 0)
	service.observeReceiverRoundTrip(0, 300*time.Microsecond, 300*time.Microsecond, 0, start)
	queuedAt := start.Add(20 * time.Millisecond)
	service.observeReceiverRoundTrip(0, 20*time.Millisecond, 20*time.Millisecond, 0, queuedAt)
	if rate, total, latest := service.measured(time.Second, queuedAt); rate != 0 || total != 0 || latest != 0 {
		t.Fatalf("timing alone invented cold service: %d/%d bytes=%d", rate, latest, total)
	}
	service.sent += 1000
	service.observe(1000, queuedAt)
	if rate, total, latest := service.measured(time.Second, queuedAt); rate != 0 || total != 1000 || latest != 0 {
		t.Fatalf("one queued checkpoint invented cold service: %d/%d bytes=%d", rate, latest, total)
	}
	for i := range 2 {
		at := queuedAt.Add(11*time.Millisecond + time.Duration(i)*time.Millisecond)
		service.observeReceiverRoundTrip(0, 300*time.Microsecond, 300*time.Microsecond, 0, at)
		service.sent += 1000
		service.observe(1000, at)
	}
	if rate, total, latest := service.measured(time.Second, queuedAt.Add(12*time.Millisecond)); rate != 1000000 || total != 3000 || latest != 1000000 {
		t.Fatalf("fresh cold pair inherited timing-only bytes: %d/%d bytes=%d", rate, latest, total)
	}
}
