package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

func TestWindowPathDispatchQualityPhaseProbe(t *testing.T) {
	assertMessagePoolOwnership(t)
	var ceiling windowPathReading
	synctest.Test(t, func(t *testing.T) {
		ceiling = measureWindowPathCell(t, windowPathCell{Arm: "ceiling", RoundTrip: 1200 * time.Millisecond,
			Compression: 10 * time.Millisecond, Flows: 8, Lanes: 8, RoundRobinOffer: true,
			Payload: 1280, Budget: mib(48), Rate: 12500000, Warmup: 12 * time.Second}, 3*time.Second)
	})
	for _, offset := range []time.Duration{0, 250 * time.Microsecond, 500 * time.Microsecond, time.Millisecond, 2500 * time.Microsecond, 5 * time.Millisecond, 7500 * time.Microsecond, 10 * time.Millisecond} {
		t.Run(offset.String(), func(t *testing.T) {
			var candidate windowPathReading
			synctest.Test(t, func(t *testing.T) {
				candidate = measureWindowPathCell(t, windowPathCell{Arm: "delivery", RoundTrip: 300 * time.Microsecond,
					RoundTripAfter: 1200 * time.Millisecond, RoundTripChangeAfter: 4*time.Second + offset,
					QualityChanged: true, Drop: true, Compression: 10 * time.Millisecond, Flows: 8, Lanes: 8, RoundRobinOffer: true,
					Payload: 1280, Budget: mib(48), Rate: 12500000, Warmup: 12 * time.Second}, 3*time.Second)
				logWindowServiceReading(t, candidate)
			})
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
