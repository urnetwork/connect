package connect

import (
	"strings"
	"testing"
)

func TestRewardAccumulatorDrainLines(t *testing.T) {
	r := newRewardAccumulator()
	r.add(ClassBulk, "exitA", 1000, true)
	r.add(ClassBulk, "exitA", 3000, false)
	lines := r.drainLines()
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d: %v", len(lines), lines)
	}
	l := lines[0]
	for _, want := range []string{"[rel] event=reward", "class=bulk", "exit=exitA", "samples=2", "stallfree=0.5"} {
		if !strings.Contains(l, want) {
			t.Fatalf("line %q missing %q", l, want)
		}
	}
	if len(r.drainLines()) != 0 {
		t.Fatal("drain must reset the accumulator")
	}
}

func TestRewardAccumulatorPerClassDivergence(t *testing.T) {
	r := newRewardAccumulator()
	r.add(ClassBulk, "exitA", 1000, true)
	r.add(ClassLatency, "exitA", 2000, false)
	lines := r.drainLines()
	if len(lines) != 2 {
		t.Fatalf("want 2 lines (one per class), got %d: %v", len(lines), lines)
	}
	// Verify that we got separate lines for each class
	hasBulk := false
	hasLatency := false
	for _, line := range lines {
		if strings.Contains(line, "class=bulk") {
			hasBulk = true
		}
		if strings.Contains(line, "class=latency") {
			hasLatency = true
		}
	}
	if !hasBulk || !hasLatency {
		t.Fatalf("lines must contain separate entries for bulk and latency: %v", lines)
	}
}

func TestRewardAccumulatorEmptyDrain(t *testing.T) {
	r := newRewardAccumulator()
	lines := r.drainLines()
	if lines != nil && len(lines) != 0 {
		t.Fatalf("empty accumulator must return empty slice, not %v", lines)
	}
}
