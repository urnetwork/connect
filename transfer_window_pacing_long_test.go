// Long virtual-time samples expose periodic pacing stalls independently of
// host scheduling and of a one-second measurement's phase within a drain cycle.
package connect

import (
	"fmt"
	"testing"
	"time"
)

// Preserve the exact high-residence matrix cell: eight flows must fill a
// 100 Mb/s service when the feedback delay includes 50 ms of compression.
func TestWindowPathServiceLongCompressedFeedback(t *testing.T) {
	assertMessagePoolOwnership(t)
	readings := measureWindowPathArms(t, time.Second, []string{"ceiling", "delivery"}, func(arm string) windowPathCell {
		return windowPathCell{Arm: arm, RoundTrip: 400 * time.Millisecond,
			Compression: 50 * time.Millisecond, Flows: 8, Payload: 1280,
			Budget: mib(48), Rate: 12500000, Drop: arm == "delivery", Warmup: 2335544320 * time.Nanosecond}
	})
	ceiling, candidate := readings[0], readings[1]
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
// The constant-window ceiling never paces, so no wake delay can reach it: one
// ceiling per payload runs at the largest delay and proves no delayed wake.
func TestWindowPathServiceAcrossRepeatedDrains(t *testing.T) {
	assertMessagePoolOwnership(t)
	delays := []time.Duration{0, time.Millisecond, 3 * time.Millisecond}
	for _, payload := range []int{16 * 1024, 64 * 1024} {
		t.Run(fmt.Sprintf("payload=%d", payload), func(t *testing.T) {
			t.Parallel()
			reference := windowPathCell{Arm: "ceiling", RoundTrip: 300 * time.Microsecond, Compression: 10 * time.Millisecond,
				Flows: 1, Payload: payload, Budget: mib(48), Rate: 125000000,
				Warmup: 2301500 * time.Microsecond, PacingWakeDelay: delays[len(delays)-1]}
			arms, trials := []string{"ceiling"}, map[string]windowPathCell{"ceiling": reference}
			for _, delay := range delays {
				arm, trial := fmt.Sprintf("delivery/wake=%s", delay), reference
				trial.Arm, trial.PacingWakeDelay, trial.Drop = "delivery", delay, true
				arms, trials[arm] = append(arms, arm), trial
			}
			readings := measureWindowPathArms(t, 12*time.Second, arms, func(arm string) windowPathCell { return trials[arm] })
			ceiling := readings[0]
			if ceiling.DelayedPacingWakes != 0 {
				t.Fatalf("payload=%d: a wake delay reached the constant-window ceiling %d times", payload, ceiling.DelayedPacingWakes)
			}
			for i, delay := range delays {
				candidate := readings[i+1]
				t.Logf("repeated drains payload=%d wake=%s reference=%.3f candidate=%.3f intervals=%v delayed-wakes=%d",
					payload, delay, ceiling.Mbps, candidate.Mbps, candidate.IntervalMbps, candidate.DelayedPacingWakes)
				if ceiling.Mbps < 900 || candidate.Mbps < .9*ceiling.Mbps ||
					candidate.MinFlowMbps == 0 || candidate.MeasurementRelayDrops != 0 ||
					candidate.MaxRelayQueued > 4096 || candidate.MaxRelayQueuedBytes > int64(mib(8)) ||
					(delay > 0) != (candidate.DelayedPacingWakes > 0) {
					t.Errorf("repeated drains payload=%d wake=%s: reference=%.3f candidate=%.3f minimum=%.3f drops=%d queue=%d/%d delayed-wakes=%d",
						payload, delay, ceiling.Mbps, candidate.Mbps, candidate.MinFlowMbps,
						candidate.MeasurementRelayDrops, candidate.MaxRelayQueued, candidate.MaxRelayQueuedBytes, candidate.DelayedPacingWakes)
				}
			}
		})
	}
}
