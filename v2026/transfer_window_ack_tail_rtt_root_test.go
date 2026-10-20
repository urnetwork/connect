// Retains the service model cell that exposed the ACK-tail RTT-growth regression.
package connect

import (
	"testing"
	"time"
)

// Exact retained RTT-growth cell of TestWindowPathServiceRoundTripChanges. One
// constructor keeps both identical, so a process measures the cell once.
func TestWindowPathAckTailRoundTripGrowthControl(t *testing.T) {
	assertMessagePoolOwnership(t)
	readings := measureWindowPathArms(t, time.Second, []string{"ceiling", "delivery"}, func(arm string) windowPathCell {
		return windowServiceRoundTripChangeCell(arm, 12500000, [2]time.Duration{300 * time.Microsecond, 100 * time.Millisecond})
	})
	ceiling, candidate := readings[0], readings[1]
	if ceiling.Mbps < 90 || candidate.Mbps < .9*ceiling.Mbps || candidate.MinFlowMbps == 0 || candidate.MeasurementRelayDrops != 0 {
		t.Errorf("RTT growth reference=%.6f candidate=%.6f minimum=%.6f drops=%d", ceiling.Mbps, candidate.Mbps, candidate.MinFlowMbps, candidate.MeasurementRelayDrops)
	}
}
