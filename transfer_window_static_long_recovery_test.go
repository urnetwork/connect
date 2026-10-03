// Fixed propagation and capacity must remain discoverable without an app
// quality notification. These are closed-loop performance controls; they do
// not claim to isolate a particular intermediate estimator classification.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// A constrained opening is ordinary startup, not a programmed path change.
// The real sender, ACK worker, shared pacer and finite FIFO drive every byte.
func TestWindowPathServiceStaticLongRoundTripSmallOpening(t *testing.T) {
	checkWindowStaticLongRecovery(t, 400*time.Millisecond, 1)
}

// Shared logical lanes must discover the same fixed aggregate service even
// when one propagation interval is longer than the old short delivery ring.
func TestWindowPathServiceStaticLongRoundTripSharedOpening(t *testing.T) {
	checkWindowStaticLongRecovery(t, 1200*time.Millisecond, 3)
}

// Give gradual pacing discovery 64 complete feedback turns before judging
// capacity. Eight more turns average a short-window flight's boundary phase.
// These durations are declared before results, not widened after a failure.
func checkWindowStaticLongRecovery(t *testing.T, roundTrip time.Duration, lanes int) {
	t.Helper()
	assertMessagePoolOwnership(t)
	const physicalRate ByteCount = 4000000
	const opening ByteCount = 512 * 1024
	residence := roundTrip + 10*time.Millisecond
	var reference, candidate windowPathReading
	for _, arm := range []string{"ceiling", "delivery"} {
		synctest.Test(t, func(t *testing.T) {
			cell := windowPathCell{
				Arm: arm, RoundTrip: roundTrip, Compression: 10 * time.Millisecond,
				Flows: lanes, Lanes: lanes, RoundRobinOffer: true,
				Payload: 1280, Budget: mib(48), Rate: physicalRate,
				Warmup: 64 * residence,
			}
			if arm == "delivery" {
				cell.BootstrapWindow, cell.Drop = opening, true
			}
			reading := measureWindowPathCell(t, cell, 8*residence)
			logWindowServiceReading(t, reading)
			if arm == "ceiling" {
				reference = reading
			} else {
				candidate = reading
			}
		})
	}
	if reference.Cell.RoundTripAfter != 0 || candidate.Cell.RoundTripAfter != 0 ||
		reference.Cell.RateAfter != 0 || candidate.Cell.RateAfter != 0 ||
		candidate.Cell.BootstrapWindow != opening || reference.Cell.BootstrapWindow != 0 {
		t.Fatal("static recovery fixture changed the path or constrained its reference")
	}
	t.Logf("static recovery rtt=%s lanes=%d reference=%.6f candidate=%.6f minimum=%.6f window=%d service=%d pace=%d intervals=%v",
		roundTrip, lanes, reference.Mbps, candidate.Mbps, candidate.MinFlowMbps,
		candidate.Window.Window, candidate.Window.ServiceByteRate, candidate.Window.PacingByteRate, candidate.IntervalMbps)
	if reference.Mbps < .9*float64(physicalRate)*8/1e6 {
		t.Errorf("the unconstrained static-path reference did not establish capacity: %.6f Mb/s", reference.Mbps)
	}
	if candidate.Mbps < .9*reference.Mbps || candidate.MinFlowMbps <= 0 {
		t.Errorf("small-opening pacing did not recover fixed-path capacity: reference=%.6f candidate=%.6f minimum=%.6f Mb/s",
			reference.Mbps, candidate.Mbps, candidate.MinFlowMbps)
	}
	if candidate.Window.Window > candidate.Window.Ceiling || candidate.MeasurementRelayDrops != 0 ||
		candidate.Receiver.ReceiveQueueDropCount != 0 || candidate.Receiver.ReceiveQueueEvictionCount != 0 {
		t.Errorf("static recovery violated byte permission or discarded measured traffic: window=%+v relay_drops=%d receiver=%+v",
			candidate.Window, candidate.MeasurementRelayDrops, candidate.Receiver)
	}
	if len(reference.IntervalMbps) == 0 || len(candidate.IntervalMbps) != len(reference.IntervalMbps) {
		t.Errorf("static comparison lost its complete interval ledger: reference=%v candidate=%v", reference.IntervalMbps, candidate.IntervalMbps)
	}
}
