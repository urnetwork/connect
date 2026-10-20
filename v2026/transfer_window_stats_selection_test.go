// Explicit iteration orders isolate diagnostic selection from Go map order.
package connect

import (
	"testing"
	"time"
)

// A smaller cumulatively qualified lane cannot replace a larger retained or
// service-qualified window. The selected estimate keeps all its own evidence.
func TestWindowDestinationStatsKeepsLargestEffectiveWindow(t *testing.T) {
	for _, serviceSized := range []bool{false, true} {
		large := SendWindowEstimate{
			Window: 8 * 1024 * 1024, LearnedWindow: 12 * 1024 * 1024,
			CandidateWindow: 4 * 1024 * 1024, ServiceSized: serviceSized,
			ServiceByteRate: 125000000, RoundTrip: 400 * time.Millisecond,
			Ceiling: 8 * 1024 * 1024, Reason: "large retained window",
		}
		small := SendWindowEstimate{
			Window: 1024 * 1024, LearnedWindow: 1024 * 1024,
			CandidateWindow: 1024 * 1024, Sized: true,
			ServiceByteRate: 1250000, RoundTrip: time.Millisecond,
			Ceiling: 2 * 1024 * 1024, Reason: "small delivery window",
		}
		for _, reverse := range []bool{false, true} {
			ordered := []SendWindowEstimate{large, small}
			if reverse {
				ordered[0], ordered[1] = ordered[1], ordered[0]
			}
			stats := SendDestinationStats{}
			for _, estimate := range ordered {
				stats.observeWindowEstimate(estimate)
			}
			if stats.SendWindow != large {
				t.Errorf("service=%t reverse=%t: smaller qualification replaced largest window or mixed its evidence: got=%+v want=%+v", serviceSized, reverse, stats.SendWindow, large)
			}
		}
	}
}

// The larger window wins with either qualification, even when a smaller lane
// has measured service or a larger uncommitted sizing candidate.
func TestWindowDestinationStatsKeepsLargestQualifiedWindow(t *testing.T) {
	large := SendWindowEstimate{Window: 8 * 1024 * 1024, Sized: true, Reason: "large cumulative"}
	small := SendWindowEstimate{Window: 1024 * 1024, CandidateWindow: 16 * 1024 * 1024, ServiceSized: true, Reason: "small service"}
	for _, ordered := range [][]SendWindowEstimate{{large, small}, {small, large}} {
		stats := SendDestinationStats{}
		for _, estimate := range ordered {
			stats.observeWindowEstimate(estimate)
		}
		if stats.SendWindow != large {
			t.Errorf("candidate or service qualification replaced a larger effective window: %+v", stats.SendWindow)
		}
	}
}

// Preserve cumulative evidence preference when the effective windows tie,
// including an explicit zero peer limit whose diagnostics still matter.
func TestWindowDestinationStatsEqualWindowsPreferCumulativeEvidence(t *testing.T) {
	for _, window := range []ByteCount{0, 1024 * 1024} {
		cumulative := SendWindowEstimate{Window: window, Sized: true, Reason: "cumulative"}
		service := SendWindowEstimate{Window: window, ServiceSized: true, Reason: "service"}
		for _, ordered := range [][]SendWindowEstimate{{cumulative, service}, {service, cumulative}} {
			stats := SendDestinationStats{}
			for _, estimate := range ordered {
				stats.observeWindowEstimate(estimate)
			}
			if stats.SendWindow != cumulative {
				t.Errorf("window=%d: equal-window evidence preference changed: %+v", window, stats.SendWindow)
			}
		}
	}
}

// Equal cumulative qualification keeps the first complete estimate. Service
// qualification does not introduce a new tie breaker in destination statistics.
func TestWindowDestinationStatsEqualEvidenceKeepsFirstEstimate(t *testing.T) {
	for _, sized := range []bool{false, true} {
		first := SendWindowEstimate{Window: 1024 * 1024, Sized: sized, Reason: "first"}
		second := SendWindowEstimate{Window: 1024 * 1024, Sized: sized, ServiceSized: true, Reason: "second"}
		for _, ordered := range [][]SendWindowEstimate{{first, second}, {second, first}} {
			stats := SendDestinationStats{}
			for _, estimate := range ordered {
				stats.observeWindowEstimate(estimate)
			}
			if stats.SendWindow != ordered[0] {
				t.Errorf("sized=%t: equivalent evidence replaced the first estimate: got=%+v want=%+v", sized, stats.SendWindow, ordered[0])
			}
		}
	}
}
