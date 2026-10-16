package connect

import (
	"testing"
	"time"
)

// One fixed-path ceiling serves every phase offset; the phases are independent
// bubbles. Offset zero is TestWindowPathServiceRoundTripGrowthBeyondOldRing's cell.
func TestWindowPathDispatchQualityPhaseProbe(t *testing.T) {
	assertMessagePoolOwnership(t)
	ceiling := measureWindowPathTrial(t, windowLongRingGrowthCell("ceiling", 0), 3*time.Second)
	for _, offset := range []time.Duration{0, 250 * time.Microsecond, 500 * time.Microsecond, time.Millisecond, 2500 * time.Microsecond, 5 * time.Millisecond, 7500 * time.Microsecond, 10 * time.Millisecond} {
		t.Run(offset.String(), func(t *testing.T) {
			windowPathParallel(t)
			candidate := measureWindowPathTrial(t, windowLongRingGrowthCell("delivery", offset), 3*time.Second)
			t.Logf("quality-phase=%s intervals=%v reference=%v drops=%d initial-drops=%d", offset, candidate.IntervalMbps, ceiling.IntervalMbps, candidate.MeasurementRelayDrops, candidate.RelayDrops)
			if len(candidate.IntervalMbps) != 3 || len(ceiling.IntervalMbps) != 3 || candidate.MeasurementRelayDrops != 0 {
				t.Fatal("quality-change fixture lost intervals or packets")
			}
			for i, reference := range ceiling.IntervalMbps {
				if reference < 90 || candidate.IntervalMbps[i] < .9*reference {
					t.Errorf("interval=%d got=%f reference=%f", i, candidate.IntervalMbps[i], reference)
				}
			}
		})
	}
}
