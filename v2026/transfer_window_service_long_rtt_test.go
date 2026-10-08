// Propagation growth can outlive the old service ring before its RTT floor adapts.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// Eight lanes share the existing finite serializer and the same fixed memory
// budget. Both arms settle for the same time. A late release of retained data
// cannot hide preceding empty measurement intervals behind the aggregate rate.
func TestWindowPathServiceRoundTripGrowthBeyondOldRing(t *testing.T) {
	assertMessagePoolOwnership(t)
	var ceiling, candidate windowPathReading
	for _, arm := range []string{"ceiling", "delivery"} {
		synctest.Test(t, func(t *testing.T) {
			cell := windowPathCell{Arm: arm, RoundTrip: 1200 * time.Millisecond,
				Compression: 10 * time.Millisecond, Flows: 8, Lanes: 8, RoundRobinOffer: true,
				Payload: 1280, Budget: mib(48), Rate: 12500000, Warmup: 12 * time.Second}
			if arm == "delivery" {
				cell.QualityChanged = true
				cell.RoundTrip, cell.RoundTripAfter, cell.RoundTripChangeAfter, cell.Drop = 300*time.Microsecond, 1200*time.Millisecond, 4*time.Second, true
			}
			reading := measureWindowPathCell(t, cell, 3*time.Second)
			logWindowServiceReading(t, reading)
			if arm == "ceiling" {
				ceiling = reading
			} else {
				candidate = reading
			}
		})
	}
	t.Logf("long-ring RTT growth reference=%.6f candidate=%.6f minimum=%.6f drops=%d", ceiling.Mbps, candidate.Mbps, candidate.MinFlowMbps, candidate.MeasurementRelayDrops)
	if ceiling.Mbps < 90 || candidate.Mbps < .9*ceiling.Mbps || candidate.MinFlowMbps == 0 || candidate.MeasurementRelayDrops != 0 {
		t.Errorf("long-ring RTT growth underfilled service: reference=%.6f candidate=%.6f minimum=%.6f drops=%d", ceiling.Mbps, candidate.Mbps, candidate.MinFlowMbps, candidate.MeasurementRelayDrops)
	}
	if len(ceiling.IntervalMbps) != 3 || len(candidate.IntervalMbps) != len(ceiling.IntervalMbps) {
		t.Fatalf("long-ring RTT growth must retain all three measurement intervals: reference=%v candidate=%v", ceiling.IntervalMbps, candidate.IntervalMbps)
	}
	for i, reference := range ceiling.IntervalMbps {
		if reference < 90 || candidate.IntervalMbps[i] < .9*reference {
			t.Errorf("long-ring RTT growth did not sustain capacity in interval %d: reference=%.6f candidate=%.6f Mb/s", i, reference, candidate.IntervalMbps[i])
		}
	}
}
