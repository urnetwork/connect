package connect

import (
	"fmt"
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

// TestRewardAccumulatorCapDropsAreCountedAndReported drives the accumulator
// past rewardAccumulatorMaxKeys and proves the cap is no longer silent: the
// rejected keys are counted on r.dropped, and drainLines surfaces that count
// as its own [rel] event=reward_dropped line -- distinct from the per-key
// event=reward lines, since a drop is a fact about a key that never got an
// entry at all, not about any tracked key's stats. Before this fix, the
// 257th distinct (class, exitId) pair simply vanished: no counter, no log
// line, and (per the standing rule this project has already been bitten by
// twice) that is exactly the "silent cap" defect, not a fixed one.
func TestRewardAccumulatorCapDropsAreCountedAndReported(t *testing.T) {
	r := newRewardAccumulator()

	// fill exactly to the cap: every key here is new, none should be dropped.
	for i := 0; i < rewardAccumulatorMaxKeys; i++ {
		r.add(ClassBulk, fmt.Sprintf("exit-%d", i), 1000, true)
	}
	if got := len(r.m); got != rewardAccumulatorMaxKeys {
		t.Fatalf("test fixture bug: want the map filled to exactly the cap (%d), got %d", rewardAccumulatorMaxKeys, got)
	}
	if r.dropped != 0 {
		t.Fatalf("filling exactly to the cap must drop nothing, got dropped=%d", r.dropped)
	}

	// an already-tracked key must keep accumulating past the cap -- the cap
	// guards against NEW keys, not against keys already paying rent.
	r.add(ClassBulk, "exit-0", 500, false)
	if s := r.m[rewardKey{Class: ClassBulk, ExitId: "exit-0"}]; s == nil || s.samples != 2 {
		t.Fatalf("an already-tracked key must keep accumulating past the cap, got %+v", s)
	}
	if r.dropped != 0 {
		t.Fatalf("re-observing an already-tracked key must not count as a drop, got dropped=%d", r.dropped)
	}

	// 5 genuinely NEW keys past the cap: all 5 must be rejected and counted.
	const wantDropped = 5
	for i := 0; i < wantDropped; i++ {
		r.add(ClassBulk, fmt.Sprintf("overflow-%d", i), 2000, true)
	}
	if r.dropped != wantDropped {
		t.Fatalf("want dropped=%d after %d new keys past the cap, got %d", wantDropped, wantDropped, r.dropped)
	}
	if got := len(r.m); got != rewardAccumulatorMaxKeys {
		t.Fatalf("a dropped key must not be added: want the map to stay at the cap (%d), got %d", rewardAccumulatorMaxKeys, got)
	}

	lines := r.drainLines()
	dropLines := []string{}
	for _, line := range lines {
		if strings.Contains(line, "event=reward_dropped") {
			dropLines = append(dropLines, line)
		}
	}
	if len(dropLines) != 1 {
		t.Fatalf("want exactly 1 event=reward_dropped line, got %d in %v", len(dropLines), lines)
	}
	for _, want := range []string{"[rel] event=reward_dropped", fmt.Sprintf("dropped=%d", wantDropped), fmt.Sprintf("cap=%d", rewardAccumulatorMaxKeys)} {
		if !strings.Contains(dropLines[0], want) {
			t.Fatalf("reward_dropped line %q missing %q", dropLines[0], want)
		}
	}
	// the ordinary per-key reward lines must still be present alongside the
	// drop line -- reporting the drop must not swallow the real data.
	if got := len(lines); got != rewardAccumulatorMaxKeys+1 {
		t.Fatalf("want %d reward lines plus 1 reward_dropped line = %d, got %d", rewardAccumulatorMaxKeys, rewardAccumulatorMaxKeys+1, got)
	}

	// draining must reset the drop counter along with the stats map: a
	// second, drop-free interval must report no reward_dropped line at all.
	if r.dropped != 0 {
		t.Fatalf("drainLines must reset the drop counter, got dropped=%d", r.dropped)
	}
	r.add(ClassBulk, "quiet-interval-key", 1000, true)
	for _, line := range r.drainLines() {
		if strings.Contains(line, "event=reward_dropped") {
			t.Fatalf("a drop-free interval must not emit a reward_dropped line, got %q", line)
		}
	}
}
