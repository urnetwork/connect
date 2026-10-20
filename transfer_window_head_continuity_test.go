package connect

import (
	"testing"
	"time"
)

// Compressed raw arrivals do not make an entire continuous receiver train
// one idle gap. Each exact head must contribute its own continuity boundary.
func TestWindowMixedHeadContinuousTrainCoversExpandedSpan(t *testing.T) {
	base := time.Unix(1700000000, 0).UnixNano()
	var coverage windowServiceCoverage
	for i := int64(1); i <= 3; i++ {
		head := base + i*int64(10*time.Millisecond)
		raw := base + int64(100*time.Millisecond) + i*int64(time.Millisecond)
		coverage.merge(windowServiceCoverage{
			firstAtNanos: raw, lastAtNanos: raw, bytes: 1000,
			supply: windowServiceSupply{
				recorded: true, complete: true, minimumPath: time.Millisecond,
				earliestSentNanos: head - int64(time.Millisecond),
				latestSentNanos:   head - int64(time.Millisecond),
				headComplete:      true, headFirstAtNanos: head,
				headLastMinAtNanos: head, headMaximumAtNanos: head,
			},
		})
	}
	// No single selected tail spans the train, and its final offer is late.
	if coverage.supply.lastQueue != 0 || coverage.supply.latestSentNanos <= base {
		t.Fatal("fixture accidentally provided independent full-span coverage")
	}
	if got := coverage.supply.qualifiedSpan(int64(3*time.Millisecond), 0, base, 10*time.Millisecond, false); got != int64(30*time.Millisecond) {
		t.Fatalf("continuous exact heads lost expanded denominator: got %s, want 30ms", time.Duration(got))
	}
}

// A real head-clock hole remains disqualifying even when raw ACKs bunch up.
func TestWindowMixedHeadTrainKeepsInternalHole(t *testing.T) {
	base := time.Unix(1700000000, 0).UnixNano()
	var coverage windowServiceCoverage
	for i, offset := range []time.Duration{10 * time.Millisecond, 50 * time.Millisecond, 60 * time.Millisecond} {
		head := base + int64(offset)
		raw := base + int64(100*time.Millisecond) + int64(i)*int64(time.Millisecond)
		coverage.merge(windowServiceCoverage{
			firstAtNanos: raw, lastAtNanos: raw, bytes: 1000,
			supply: windowServiceSupply{
				recorded: true, complete: true, minimumPath: time.Millisecond,
				earliestSentNanos: head - int64(time.Millisecond), latestSentNanos: head - int64(time.Millisecond),
				headComplete: true, headFirstAtNanos: head, headLastMinAtNanos: head, headMaximumAtNanos: head,
			},
		})
	}
	if got := coverage.supply.qualifiedSpan(int64(3*time.Millisecond), 0, base, 10*time.Millisecond, false); got != 0 {
		t.Fatalf("internal head hole gained decrease authority: %s", time.Duration(got))
	}
}
