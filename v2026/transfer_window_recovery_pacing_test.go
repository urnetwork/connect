// Missing original envelopes are recovery debt, not proof that a physical
// serializer still contains them. Retry ACK cadence is paced by the sender.
package connect

import (
	"testing"
	"time"
)

func TestWindowPacingRecoveryRequiresFreshBacklogEvidence(t *testing.T) {
	for _, receiverTiming := range []bool{false, true} {
		service, at := newWindowQualifiedServiceFixture(t, 1000000, 10*time.Millisecond, time.Millisecond)
		if !receiverTiming {
			service.receiverRoundTrips = nil
		}
		service.sent += 2 * 1024 * 1024
		observe := func(at time.Time) {
			if receiverTiming {
				service.observeReceiverRoundTrip(0, 30*time.Millisecond, 20*time.Millisecond, 10*time.Millisecond, at)
			} else {
				service.observeRoundTrip(30*time.Millisecond, 10*time.Millisecond, at)
			}
		}
		at = at.Add(20 * time.Millisecond)
		observe(at)
		if !service.backloggedAt(1000000, at) {
			t.Fatal("the original flight did not establish backlog")
		}
		sequence, retry := NewId(), NewId()
		at = at.Add(time.Second)
		service.beginWrite(sequence, retry, 1, at, true)
		service.finishWrite(sequence, retry, true)
		// Credit every newly delivered byte, but do not interpret the missing
		// original flight as a queue that demands another multiplicative cut.
		for i := range 8 {
			at = at.Add(10 * time.Millisecond)
			service.observe(1000, at)
			rate, _, latest := service.measured(time.Second, at)
			estimate := SendWindowEstimate{ServiceByteRate: max(rate, latest), ServiceEstablished: true,
				ServiceBacklogged: service.backloggedAt(max(rate, latest), at)}
			if pace := windowPacingRate(estimate, 1000000); pace < estimate.ServiceByteRate {
				t.Fatalf("receiver timing=%t turn=%d: paced retry feedback recursively reduced service=%d to pace=%d", receiverTiming, i, estimate.ServiceByteRate, pace)
			}
		}
		if rate, _, latest := service.measured(time.Second, at); max(rate, latest) != 100000 {
			t.Fatalf("receiver timing=%t: recovery stopped measuring the actual slower delivery: %d/%d", receiverTiming, rate, latest)
		}
		// A different unretried write can still establish real queueing and
		// lower the pace immediately; recovery does not disable adaptation.
		at = at.Add(time.Millisecond)
		observe(at)
		if !service.backloggedAt(1000000, at) {
			t.Fatalf("receiver timing=%t: fresh queue evidence could not restore drain pacing", receiverTiming)
		}
		service.beginWrite(sequence, NewId(), 2, at, true)
		if service.backloggedAt(1000000, at) {
			t.Fatalf("receiver timing=%t: timing at the recovery boundary was treated as newer evidence", receiverTiming)
		}
	}
}
