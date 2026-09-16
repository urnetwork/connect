// Established sparse senders must discover later capacity without mistaking
// every naturally empty flight for a requested measurement pause.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// Move the original capacity transition and its measurement forward by 36 s,
// so the slow opening train has drained before the change. Preserve the same
// post-change allowance, ninety-percent gate and sixty-four-message interval.
func TestWindowPathServiceSettledLargeMessageCapacityChanges(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, rates := range [][2]ByteCount{{125000, 1250000}, {1250000, 125000}} {
		for _, payload := range []int{16 * 1024, 64 * 1024} {
			var ceiling, candidate windowPathReading
			warmup := 40500*time.Millisecond + time.Duration(2*int64(mib(2))*int64(time.Second)/int64(min(rates[0], rates[1])))
			measurement := max(2*time.Second, time.Duration(64*int64(payload)*int64(time.Second)/int64(rates[1])))
			for _, arm := range []string{"ceiling", "delivery"} {
				synctest.Test(t, func(t *testing.T) {
					cell := windowPathCell{Arm: arm, RoundTrip: 100 * time.Millisecond, Compression: 10 * time.Millisecond,
						Flows: 8, RoundRobinOffer: true, Payload: payload, Budget: mib(48), Rate: rates[1], Warmup: warmup}
					if arm == "delivery" {
						cell.Rate, cell.RateAfter, cell.RateChangeAfter, cell.Drop = rates[0], rates[1], 40*time.Second, true
					}
					reading := measureWindowPathCell(t, cell, measurement)
					logWindowServiceReading(t, reading)
					if arm == "ceiling" {
						ceiling = reading
					} else {
						candidate = reading
					}
				})
			}
			if ceiling.Mbps < .9*float64(rates[1])*8/1e6 || candidate.Mbps < .9*ceiling.Mbps ||
				candidate.MinFlowMbps == 0 || candidate.MeasurementRelayDrops != 0 ||
				candidate.MaxRelayQueued > 4096 || candidate.MaxRelayQueuedBytes > int64(mib(8)) {
				t.Errorf("large-message rate=%d->%d payload=%d: reference=%.3f candidate=%.3f minimum=%.3f drops=%d queue=%d/%d",
					rates[0], rates[1], payload, ceiling.Mbps, candidate.Mbps, candidate.MinFlowMbps,
					candidate.MeasurementRelayDrops, candidate.MaxRelayQueued, candidate.MaxRelayQueuedBytes)
			}
		}
	}
}
