// Timestamped baseline evidence retains its observation order and cannot
// silently convert raw fallback into receiver-adjusted timing.
package connect

import (
	"testing"
	"time"
)

// Timestamped statistics cannot borrow a baseline established in their
// future, even when that later observation already arrived on another worker.
func TestWindowPacingReceiverBaselineReadDoesNotUseFutureObservation(t *testing.T) {
	at := time.Unix(1700000000, 0)
	service := newWindowPacingService(DefaultSendBufferSettings())
	service.observeReceiverRoundTrip(1, 110*time.Millisecond, 100*time.Millisecond, 10*time.Millisecond, at)
	service.observeReceiverRoundTrip(2, 11*time.Millisecond, time.Millisecond, 10*time.Millisecond, at.Add(20*time.Millisecond))
	before := *service.receiverRoundTrips
	got := service.roundTripEvidence(at.Add(10 * time.Millisecond))
	if got.minimum != 100*time.Millisecond || got.residence != 110*time.Millisecond {
		t.Errorf("historical read consumed future baseline: network=%s residence=%s", got.minimum, got.residence)
	}
	after := *service.receiverRoundTrips
	if before.tail != after.tail || before.count != after.count || before.baseline != after.baseline {
		t.Fatal("statistics mutated timing history")
	}
}

// The exact probe tuple can arrive before its coalesced covering-head step.
// A later worker's lower RTT must survive that delayed confirmation.
func TestWindowPacingReceiverBaselineLateProbeKeepsNewerEvidence(t *testing.T) {
	at := time.Unix(1700000000, 0)
	service := newWindowPacingService(DefaultSendBufferSettings())
	service.observeReceiverRoundTrip(1, 10300*time.Microsecond, 300*time.Microsecond, 10*time.Millisecond, at)
	sequenceId, messageId := NewId(), NewId()
	service.drained = true
	service.beginWrite(sequenceId, messageId, 1, at.Add(time.Second), false)
	service.finishWrite(sequenceId, messageId, true)
	probeAt := at.Add(1110 * time.Millisecond)
	service.observeReceiverRoundTripForWrite(sequenceId, messageId, 2, 110*time.Millisecond, 100*time.Millisecond, 10*time.Millisecond, probeAt)
	newerAt := probeAt.Add(10 * time.Millisecond)
	service.observeReceiverRoundTrip(3, 20*time.Millisecond, 10*time.Millisecond, 10*time.Millisecond, newerAt)
	service.acknowledgeWrite(sequenceId, messageId, 1, false, 10*time.Millisecond, probeAt)
	got := service.roundTripEvidence(newerAt)
	if got.minimum != 10*time.Millisecond {
		t.Errorf("late probe erased newer lower adjusted RTT: %s", got.minimum)
	}
	if service.lastRoundTrip != newerAt {
		t.Fatal("late probe rewound ordinary observation clock")
	}
	observed, residence, set := service.receiverWindowEstimate(newerAt)
	if !set || observed != 10*time.Millisecond || residence != 20*time.Millisecond || service.receiverRoundTrips.count != 2 {
		t.Errorf("late probe discarded fresh tuples: observed=%s residence=%s count=%d", observed, residence, service.receiverRoundTrips.count)
	}
	baseline := service.receiverRoundTrips.baseline
	service.receiverRoundTrips.confirmBaseline(windowReceiverRoundTripSample{atNanos: probeAt.UnixNano(), raw: 310 * time.Millisecond, adjusted: 300 * time.Millisecond, compression: 10 * time.Millisecond})
	if service.receiverRoundTrips.baseline != baseline {
		t.Fatal("still older proof rewound newer baseline")
	}
}

// Ending metadata authority is explicit. Legacy recovery can use a raw path
// estimate without silently treating it as a receiver-adjusted measurement.
func TestWindowPacingReceiverBaselineExpiredMetadataKeepsRawFallbackExplicit(t *testing.T) {
	at := time.Unix(1700000000, 0)
	settings := DefaultSendBufferSettings()
	service := newWindowPacingService(settings)
	service.observeReceiverRoundTrip(1, 10300*time.Microsecond, 300*time.Microsecond, 10*time.Millisecond, at)
	baseline := service.receiverRoundTrips.baseline
	now := at.Add(settings.RttWindowTimeout + time.Second)
	service.observeRoundTrip(200*time.Millisecond, 10*time.Millisecond, now)
	if _, _, set := service.receiverWindowEstimate(now); set {
		t.Fatal("expired receiver metadata remained authoritative")
	}
	got := service.roundTripEvidence(now)
	if got.count != 0 || got.minimum != service.minRoundTrip || got.residence != service.minRoundTrip+service.compression {
		t.Fatal("raw fallback was mislabeled as receiver timing")
	}
	if service.receiverRoundTrips.baseline != baseline {
		t.Fatal("legacy observation modified the adjusted baseline")
	}
}
