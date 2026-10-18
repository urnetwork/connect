// Propagation growth can outlive the old service ring before its RTT floor adapts.
package connect

import (
	"testing"
	"time"
)

// Eight lanes share the existing finite serializer and the same fixed memory
// budget. The delivery arm establishes settled circulation before its three
// steady intervals; every earlier nominal interval remains in its capacity debt.
// A late release cannot hide an empty measured interval behind aggregate rate.
func TestWindowPathServiceRoundTripGrowthBeyondOldRing(t *testing.T) {
	assertMessagePoolOwnership(t)
	ceiling := measureWindowPathTrial(t, windowLongRingGrowthCell("ceiling", 0), 3*time.Second)
	candidate := measureWindowPathTrial(t, windowSettledLongRingGrowthCell(t, 0, ceiling), 3*time.Second)
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

// Calibration is complete before candidate admission. Both phase tests use
// this exact policy value, so offset zero still shares the same trial cache.
func windowSettledLongRingGrowthCell(t *testing.T, offset time.Duration, ceiling windowPathReading) windowPathCell {
	t.Helper()
	reference, span, err := windowPathDebtReference(ceiling.IntervalBytes, ceiling.IntervalDurations)
	if err != nil {
		t.Fatal(err)
	}
	cell := windowLongRingGrowthCell("delivery", offset)
	cell.Settlement = &windowPathSettlementPolicy{Version: "paired-circulation-v1", ReferenceBytes: reference, ReferenceDuration: span,
		CapBytes: int64(cell.Budget), AckLifetime: DefaultSendBufferSettings().AckTimeout}
	return cell
}

// The ceiling holds the 1.2 s path from the start; the candidate grows to it
// after four settled seconds plus a dispatch-phase offset. The growth control
// and TestWindowPathDispatchQualityPhaseProbe share these exact cells.
func windowLongRingGrowthCell(arm string, offset time.Duration) windowPathCell {
	cell := windowPathCell{Arm: arm, RoundTrip: 1200 * time.Millisecond,
		Compression: 10 * time.Millisecond, Flows: 8, Lanes: 8,
		Payload: 1280, Budget: mib(48), Rate: 12500000, Warmup: 12 * time.Second, MeasurementGrid: true}
	if arm == "delivery" {
		cell.QualityChanged = true
		cell.RoundTrip, cell.RoundTripAfter, cell.RoundTripChangeAfter, cell.Drop = 300*time.Microsecond, 1200*time.Millisecond, 4*time.Second+offset, true
	}
	return cell
}
