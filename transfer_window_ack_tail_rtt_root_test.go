// Retains the service model cell that exposed the ACK-tail RTT-growth regression.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// Exact retained RTT-growth cell; only the containing matrix is narrowed.
func TestWindowPathAckTailRoundTripGrowthControl(t *testing.T) {
	assertMessagePoolOwnership(t)
	var ceiling, candidate windowPathReading
	for _, arm := range []string{"ceiling", "delivery"} {
		synctest.Test(t, func(t *testing.T) {
			cell := windowPathCell{Arm: arm, RoundTrip: 100 * time.Millisecond, Compression: 10 * time.Millisecond,
				Flows: 8, RoundRobinOffer: true, Payload: 1280, Budget: mib(48), Rate: 12500000, Warmup: 8 * time.Second}
			if arm == "delivery" {
				cell.QualityChanged = true
				cell.RoundTrip, cell.RoundTripAfter, cell.RoundTripChangeAfter, cell.Drop = 300*time.Microsecond, 100*time.Millisecond, 4*time.Second, true
			}
			reading := measureWindowPathCell(t, cell, time.Second)
			logWindowServiceReading(t, reading)
			if arm == "ceiling" {
				ceiling = reading
			} else {
				candidate = reading
			}
		})
	}
	if ceiling.Mbps < 90 || candidate.Mbps < .9*ceiling.Mbps || candidate.MinFlowMbps == 0 || candidate.MeasurementRelayDrops != 0 {
		t.Errorf("RTT growth reference=%.6f candidate=%.6f minimum=%.6f drops=%d", ceiling.Mbps, candidate.Mbps, candidate.MinFlowMbps, candidate.MeasurementRelayDrops)
	}
}
