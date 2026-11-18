// The host instrument must reject slow or lossy candidates even when every
// flow moves some bytes. Invalid controls limit attribution, never hide loss.
package connect

import "testing"

// Independent readings have a 1 Gb/s configured link and positive progress;
// their byte counts deliberately do not derive from the comparison's ratios.
func windowPerformanceComparisonReadings(ceiling, first, candidate, last float64) []windowPathReading {
	var readings []windowPathReading
	for _, arm := range []struct {
		name string
		rate float64
	}{
		{name: "ceiling", rate: ceiling},
		{name: "matched", rate: first},
		{name: "delivery", rate: candidate},
		{name: "matched", rate: last},
	} {
		readings = append(readings, windowPathReading{
			Cell:  windowPathCell{Arm: arm.name, Rate: 125000000, Flows: 8},
			Bytes: 1000000, Mbps: arm.rate, MinFlowMbps: 1,
		})
	}
	return readings
}

// The previous host gate accepted a candidate at one third of its controls
// because nonzero per-flow progress was its only performance assertion.
func TestWindowPerformanceRejectsSlowCandidate(t *testing.T) {
	comparison := compareWindowPathReadings(windowPerformanceComparisonReadings(940, 930, 300, 935))
	if len(comparison.FailureReasons) == 0 || len(comparison.CensoredReasons) != 0 {
		t.Fatalf("calibrated slow candidate was not a performance failure: %+v", comparison)
	}
}

// A TCP-buffer-limited control still establishes the attainable reference:
// an independently slower candidate is a failure even below link capacity.
func TestWindowPerformanceCappedFixtureCannotHideRegression(t *testing.T) {
	comparison := compareWindowPathReadings(windowPerformanceComparisonReadings(165, 150, 90, 145))
	if len(comparison.FailureReasons) == 0 || len(comparison.CensoredReasons) == 0 {
		t.Fatalf("instrument ceiling erased the candidate regression: %+v", comparison)
	}
}

// Preserve both the failed rate comparison and its uncertain host controls.
func TestWindowPerformanceDriftCannotEraseSlowReading(t *testing.T) {
	comparison := compareWindowPathReadings(windowPerformanceComparisonReadings(940, 910, 300, 810))
	if len(comparison.FailureReasons) == 0 || len(comparison.CensoredReasons) == 0 {
		t.Fatalf("drifting controls erased or validated the slow reading: %+v", comparison)
	}
}

// A malformed ceiling must not hide the candidate falling below both A/A
// arms. The unchanged-window comparison remains an independent guard.
func TestWindowPerformanceChecksMatchedControlsIndependently(t *testing.T) {
	comparison := compareWindowPathReadings(windowPerformanceComparisonReadings(800, 1000, 800, 950))
	if len(comparison.FailureReasons) == 0 || comparison.DeliveryOfCeiling != 1 {
		t.Fatalf("candidate passed by matching an underfilled ceiling: %+v", comparison)
	}
}

// Loss and starvation remain outcomes even when the aggregate rate is good.
func TestWindowPerformanceRejectsDeliveryLossAndStalls(t *testing.T) {
	for _, defect := range []string{"relay", "nat", "receiver", "sender", "stalled-flow", "no-bytes"} {
		readings := windowPerformanceComparisonReadings(940, 930, 935, 935)
		candidate := &readings[2]
		switch defect {
		case "relay":
			candidate.MeasurementRelayDrops = 1
		case "nat":
			candidate.NatRefused = 1
		case "receiver":
			candidate.Receiver.ReceiveQueueEvictionCount = 1
		case "sender":
			candidate.SenderReceive.ReceiveQueueEvictionCount = 1
		case "stalled-flow":
			candidate.MinFlowMbps = 0
		case "no-bytes":
			candidate.Bytes = 0
		}
		if comparison := compareWindowPathReadings(readings); len(comparison.FailureReasons) == 0 {
			t.Fatalf("%s passed despite a delivery failure: %+v", defect, comparison)
		}
	}
}

// The predeclared ten-percent margin includes its exact boundary. Control
// drift below ten percent must not create an instrumentation exclusion.
func TestWindowPerformanceAcceptsItsDeclaredMargin(t *testing.T) {
	for _, rate := range []float64{900, 950, 1000} {
		comparison := compareWindowPathReadings(windowPerformanceComparisonReadings(1000, 1000, rate, 950))
		if len(comparison.FailureReasons) != 0 || len(comparison.CensoredReasons) != 0 {
			t.Fatalf("valid rate %.1f rejected at the declared margin: %+v", rate, comparison)
		}
	}
}

// Intentionally incomplete diagnostic arms cannot masquerade as a complete
// paired validation, nor invent a zero-throughput candidate that never ran.
func TestWindowPerformanceMissingControlsStayInconclusive(t *testing.T) {
	complete := windowPerformanceComparisonReadings(940, 930, 935, 935)
	for _, readings := range [][]windowPathReading{
		nil, complete[:1], complete[1:], complete[:3],
		{complete[0], complete[1], complete[3]},
	} {
		comparison := compareWindowPathReadings(readings)
		if len(comparison.CensoredReasons) == 0 || len(comparison.FailureReasons) != 0 {
			t.Fatalf("missing control/candidate became valid or a fabricated regression: %+v", comparison)
		}
	}
}
