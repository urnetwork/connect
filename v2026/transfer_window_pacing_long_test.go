// Long virtual-time samples expose periodic pacing stalls independently of
// host scheduling and of a one-second measurement's phase within a drain cycle.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// Preserve the exact high-residence matrix cell: eight flows must fill a
// 100 Mb/s service when the feedback delay includes 50 ms of compression.
func TestWindowPathServiceLongCompressedFeedback(t *testing.T) {
	assertMessagePoolOwnership(t)
	var ceiling, candidate windowPathReading
	for _, arm := range []string{"ceiling", "delivery"} {
		synctest.Test(t, func(t *testing.T) {
			reading := measureWindowPathCell(t, windowPathCell{Arm: arm, RoundTrip: 400 * time.Millisecond,
				Compression: 50 * time.Millisecond, Flows: 8, RoundRobinOffer: true, Payload: 1280,
				Budget: mib(48), Rate: 12500000, Drop: arm == "delivery", Warmup: 2335544320 * time.Nanosecond}, time.Second)
			logWindowServiceReading(t, reading)
			if arm == "ceiling" {
				ceiling = reading
			} else {
				candidate = reading
			}
		})
	}
	t.Logf("long compressed feedback reference=%.3f candidate=%.3f minimum=%.3f drops=%d queue=%d/%d",
		ceiling.Mbps, candidate.Mbps, candidate.MinFlowMbps, candidate.MeasurementRelayDrops,
		candidate.MaxRelayQueued, candidate.MaxRelayQueuedBytes)
	if ceiling.Mbps < 90 || candidate.Mbps < .9*ceiling.Mbps || candidate.MinFlowMbps == 0 ||
		candidate.MeasurementRelayDrops != 0 || candidate.MaxRelayQueued > 4096 || candidate.MaxRelayQueuedBytes > int64(mib(8)) {
		t.Errorf("long compressed feedback underfilled the service: reference=%+.3f candidate=%+.3f", ceiling.Mbps, candidate.Mbps)
	}
}

// Match the short-path host cell's warmup and retain twelve one-second
// readings across more than two drain intervals, including delayed dispatch.
func TestWindowPathServiceAcrossRepeatedDrains(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, payload := range []int{16 * 1024, 64 * 1024} {
		for _, delay := range []time.Duration{0, time.Millisecond, 3 * time.Millisecond} {
			var ceiling, candidate windowPathReading
			for _, arm := range []string{"ceiling", "delivery"} {
				synctest.Test(t, func(t *testing.T) {
					cell := windowPathCell{Arm: arm, RoundTrip: 300 * time.Microsecond, Compression: 10 * time.Millisecond,
						Flows: 1, RoundRobinOffer: true, Payload: payload, Budget: mib(48), Rate: 125000000,
						Warmup: 2301500 * time.Microsecond, PacingWakeDelay: delay, Drop: arm == "delivery"}
					reading := measureWindowPathCell(t, cell, 12*time.Second)
					logWindowServiceReading(t, reading)
					if arm == "ceiling" {
						ceiling = reading
					} else {
						candidate = reading
					}
				})
			}
			t.Logf("repeated drains payload=%d wake=%s reference=%.3f candidate=%.3f intervals=%v",
				payload, delay, ceiling.Mbps, candidate.Mbps, candidate.IntervalMbps)
			if ceiling.Mbps < 900 || candidate.Mbps < .9*ceiling.Mbps ||
				candidate.MinFlowMbps == 0 || candidate.MeasurementRelayDrops != 0 ||
				candidate.MaxRelayQueued > 4096 || candidate.MaxRelayQueuedBytes > int64(mib(8)) {
				t.Errorf("repeated drains payload=%d wake=%s: reference=%.3f candidate=%.3f minimum=%.3f drops=%d queue=%d/%d",
					payload, delay, ceiling.Mbps, candidate.Mbps, candidate.MinFlowMbps,
					candidate.MeasurementRelayDrops, candidate.MaxRelayQueued, candidate.MaxRelayQueuedBytes)
			}
		}
	}
}
