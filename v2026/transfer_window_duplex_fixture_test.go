// Duplex fixture orientation must preserve both physical serialization limits.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// Asymmetric finite windows expose an accidentally unlimited return path.
// Reversing the reported direction may reorder readings, never remove a link.
func TestWindowPerformanceDuplexBoundsBothDirections(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, upload := range []bool{false, true} {
		for _, flows := range []int{1, 8} {
			synctest.Test(t, func(t *testing.T) {
				reading := measureWindowPathCell(t, windowPathCell{
					Arm: "ceiling", Bidirectional: true, Upload: upload,
					RoundTrip: 300 * time.Microsecond, Compression: 10 * time.Millisecond,
					Flows: flows, Lanes: flows, RoundRobinOffer: true, Payload: 1280,
					Rate: 12500000, Budget: kib(512), SendWindow: kib(64), ReceiveWindow: kib(64),
					Warmup: time.Second,
				}, time.Second)
				logWindowServiceReading(t, reading)
				if len(reading.DirectionMbps) != 2 || reading.MinFlowMbps <= 0 || reading.MeasurementRelayDrops != 0 {
					t.Fatalf("upload=%t flows=%d: duplex fixture stalled or lost data: directions=%v minimum=%f drops=%d",
						upload, flows, reading.DirectionMbps, reading.MinFlowMbps, reading.MeasurementRelayDrops)
				}
				for direction, rate := range reading.DirectionMbps {
					if rate <= 0 || rate > 101 {
						t.Errorf("upload=%t flows=%d direction=%d: %.6f Mb/s exceeds the 100 Mb/s physical link or lacks progress",
							upload, flows, direction, rate)
					}
				}
			})
		}
	}
}

// Both serializers retain their scheduled rate change when directions reverse.
// Drop mode exercises the same link setup used by delivery-window comparisons.
func TestWindowPerformanceDuplexBoundsRateChanges(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, upload := range []bool{false, true} {
		for _, rate := range []ByteCount{6250000, 12500000} {
			synctest.Test(t, func(t *testing.T) {
				rateAfter := ByteCount(18750000) - rate
				reading := measureWindowPathCell(t, windowPathCell{
					Arm: "ceiling", Bidirectional: true, Upload: upload, Drop: true,
					RoundTrip: 300 * time.Microsecond, Compression: 10 * time.Millisecond,
					Flows: 8, Lanes: 8, RoundRobinOffer: true, Payload: 1280,
					Rate: rate, RateAfter: rateAfter, RateChangeAfter: 500 * time.Millisecond,
					Budget: kib(512), SendWindow: kib(64), ReceiveWindow: kib(64), Warmup: 2 * time.Second,
				}, time.Second)
				logWindowServiceReading(t, reading)
				if len(reading.DirectionMbps) != 2 || reading.MinFlowMbps <= 0 || reading.MeasurementRelayDrops != 0 {
					t.Fatalf("upload=%t rate=%d after=%d: duplex rate change stalled or lost data: directions=%v minimum=%f drops=%d",
						upload, rate, rateAfter, reading.DirectionMbps, reading.MinFlowMbps, reading.MeasurementRelayDrops)
				}
				ceiling := 1.01 * float64(rateAfter) * 8 / 1e6
				for direction, measuredRate := range reading.DirectionMbps {
					if measuredRate <= 0 || measuredRate > ceiling {
						t.Errorf("upload=%t rate=%d after=%d direction=%d: %.6f Mb/s exceeds the changed physical link or lacks progress",
							upload, rate, rateAfter, direction, measuredRate)
					}
				}
			})
		}
	}
}
