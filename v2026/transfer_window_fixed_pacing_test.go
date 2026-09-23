// A fixed byte window still needs the shared H1 service's pacing evidence.
package connect

import (
	"testing"
	"time"
)

// Match the default provider: delivery pacing is enabled without a shared
// memory budget, so window sizing returns before computing a local residence.
func newWindowFixedPacingFixture(t *testing.T) (*SendSequence, time.Time) {
	t.Helper()
	sequence := newEstimatorFixture(t, nil)
	t.Cleanup(func() { sequence.resendQueue.Clear() })
	if sequence.resendQueue.Budget() != nil || sequence.sendBufferSettings.TargetGoodputByteRate <= 0 {
		t.Fatal("default provider no longer uses unbudgeted delivery pacing")
	}
	sequence.contractMultiRouteWriter = &windowPacingPolicyWriter{policy: transferFlightPolicySnapshot{h1Only: true}}
	service, at := newWindowQualifiedServiceFixture(t, 1000000, 10*time.Millisecond, time.Millisecond)
	sequence.windowPacer.service = service
	return sequence, at
}

// Slow application traffic bounds capacity only from below. Shared physical
// timing permits discovery even though this provider cannot grow its window.
func TestWindowFixedPacingDiscoversWithPhysicalResidence(t *testing.T) {
	sequence, at := newWindowFixedPacingFixture(t)
	estimate := sequence.sendWindowEstimate(at)
	if estimate.Sized || estimate.Window != estimate.Initial || estimate.WindowRoundTrip != 0 ||
		!estimate.ServiceEstablished || estimate.ServiceByteRate != 1000000 || !estimate.PacingDiscovery || estimate.ServiceBacklogged {
		t.Fatalf("fixture lost fixed-window, unqueued service evidence: %+v", estimate)
	}
	residence := sequence.windowPacer.service.roundTripEvidence(at).residence
	minimum := min(estimate.PacingProbeByteRate, ByteCount(float64(estimate.Window)/residence.Seconds()))
	if estimate.PacingByteRate < minimum {
		t.Fatalf("fixed window priced application-limited traffic as path capacity: pace=%d want>=%d residence=%s", estimate.PacingByteRate, minimum, residence)
	}
}

// Physical pacing evidence grants no additional peer or configured bytes,
// and discovery still respects the independently configured rate target.
func TestWindowFixedPacingDiscoveryHonorsByteAndRateBounds(t *testing.T) {
	for _, bound := range []string{"peer", "configured", "target"} {
		sequence, at := newWindowFixedPacingFixture(t)
		const permission ByteCount = 64 * 1024
		switch bound {
		case "peer":
			sequence.observeReceiveWindowAdvertisement(receiveAckMessage{receiveWindowSet: true, receiveWindowByteCount: uint32(permission)})
		case "configured":
			sequence.sendBufferSettings.DeliverySizedWindowCeilingByteCount = permission
		case "target":
			sequence.sendBufferSettings.TargetGoodputByteRate = 1000000
		}
		estimate := sequence.sendWindowEstimate(at)
		if estimate.Sized || estimate.Window > estimate.Initial || estimate.Window != estimate.Ceiling ||
			bound != "target" && estimate.Window != permission {
			t.Fatalf("%s permission changed during discovery: %+v", bound, estimate)
		}
		residence := sequence.windowPacer.service.roundTripEvidence(at).residence
		want := min(estimate.PacingProbeByteRate, ByteCount(float64(estimate.Window)/residence.Seconds()))
		if estimate.PacingByteRate != want {
			t.Errorf("%s bound: pace=%d want=%d", bound, estimate.PacingByteRate, want)
		}
	}
}

// After congestion ends discovery, sparse unqueued feedback must preserve
// the admitted pace. A later real queue must still reduce that pace.
func TestWindowFixedPacingRetainsUnqueuedRateAndAdaptsToQueue(t *testing.T) {
	sequence, at := newWindowFixedPacingFixture(t)
	service := sequence.windowPacer.service
	at = at.Add(time.Millisecond)
	service.observeReceiverRoundTrip(0, 31*time.Millisecond, 21*time.Millisecond, 10*time.Millisecond, at)
	at = at.Add(time.Millisecond)
	service.observeReceiverRoundTrip(0, 11*time.Millisecond, time.Millisecond, 10*time.Millisecond, at)
	before := sequence.sendWindowEstimate(at)
	if before.PacingDiscovery || before.ServiceBacklogged || before.PacingByteRate != 1100000 {
		t.Fatalf("fixture did not admit a recovered service rate: %+v", before)
	}
	// Explicit checkpoints replace the earlier fast interval with sparse
	// traffic on the same fast path; there is no outstanding physical flight.
	for i := range 9 {
		now := at.Add(time.Second + time.Duration(i)*10*time.Millisecond)
		service.sent += 100
		service.observeReceiverRoundTrip(0, 11*time.Millisecond, time.Millisecond, 10*time.Millisecond, now)
		service.observe(100, now)
	}
	at = at.Add(time.Second + 80*time.Millisecond)
	after := sequence.sendWindowEstimate(at)
	if after.ServiceByteRate != 10000 || after.PacingDiscovery || after.ServiceBacklogged {
		t.Fatalf("fixture did not measure sparse unqueued feedback: %+v", after)
	}
	if after.PacingByteRate != before.PacingByteRate {
		t.Errorf("sparse feedback erased a fixed-window provider's admitted pace: before=%d after=%d", before.PacingByteRate, after.PacingByteRate)
	}
	service.sent += 1024 * 1024
	at = at.Add(time.Millisecond)
	service.observeReceiverRoundTrip(0, 31*time.Millisecond, 21*time.Millisecond, 10*time.Millisecond, at)
	queued := sequence.sendWindowEstimate(at)
	if !queued.ServiceBacklogged || queued.PacingByteRate >= before.PacingByteRate || queued.Window != before.Window {
		t.Fatalf("real queueing could not lower pacing within the fixed byte window: %+v", queued)
	}
}

// Reporting cannot publish an admitted pace, and a non-H1 route must not
// inherit a physical H1 residence solely because a sibling measured it.
func TestWindowFixedPacingScopesPhysicalEvidenceAndReadOnlyStats(t *testing.T) {
	sequence, at := newWindowFixedPacingFixture(t)
	service := sequence.windowPacer.service
	sequence.sendWindowSnapshot(at)
	if _, held := service.pacingHold(); held != 0 {
		t.Fatalf("statistics published a pacing hold: %d", held)
	}
	sequence.contractMultiRouteWriter = &windowPacingPolicyWriter{policy: transferFlightPolicySnapshot{}}
	estimate := sequence.sendWindowEstimate(at)
	if estimate.PacingDiscovery || estimate.PacingHeldByteRate != 0 || estimate.PacingByteRate != 1100000 {
		t.Fatalf("non-H1 route borrowed physical H1 discovery or retention: %+v", estimate)
	}
	if _, held := service.pacingHold(); held != 0 {
		t.Fatalf("non-H1 admission changed shared H1 retention: %d", held)
	}
}
