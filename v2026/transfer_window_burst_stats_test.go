// Burst epoch transitions and zero-order hold are explicit clock operations,
// independent of worker scheduling and of how often consumers read estimates.
package connect

import (
	"math"
	"testing"
	"time"
)

// The prior burst summary fills empty buckets after reset. New completed
// buckets progressively replace that hold; the current partial one does not.
func TestWindowBucketStatsResetCarriesBurstUntilNewEvidence(t *testing.T) {
	start := time.Unix(1700000000, 0)
	stats := newWindowBucketStats(10*time.Millisecond, 4)
	stats.add(999, start)
	at := start.Add(time.Second)
	stats.reset(100, at)
	if mean, count := stats.mean(at); mean != 100 || count != 4 {
		t.Fatalf("reset lost its hold: mean=%v count=%d", mean, count)
	}
	stats.add(200, at)
	if mean, _ := stats.mean(at.Add(9 * time.Millisecond)); mean != 100 {
		t.Fatal("a partial new bucket replaced the burst hold")
	}
	for i, want := range []float64{125, 150, 175, 200} {
		if mean, count := stats.mean(at.Add(time.Duration(i+1) * 10 * time.Millisecond)); mean != want || count != 4 {
			t.Fatalf("completed=%d mean=%v count=%d want=%v", i+1, mean, count, want)
		}
	}
}

// An epoch boundary can occur inside a clock bucket. Stale data on either
// side of that bucket boundary must not replace the new held value.
func TestWindowBucketStatsResetRejectsEarlierEpoch(t *testing.T) {
	at := time.Unix(1700000000, int64(5*time.Millisecond))
	stats := newWindowBucketStats(10*time.Millisecond, 4)
	stats.reset(0, at)
	for _, late := range []time.Time{at.Add(-time.Nanosecond), at.Add(-time.Millisecond), at.Add(-time.Hour)} {
		if stats.add(999, late) {
			t.Error("old epoch sample replaced the held zero")
		}
	}
	for _, invalid := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if stats.reset(invalid, at.Add(time.Second)) {
			t.Error("invalid reset destroyed the current epoch")
		}
	}
	if mean, count := stats.mean(at.Add(time.Hour)); mean != 0 || count != 4 {
		t.Fatal("an explicit zero did not survive an empty epoch")
	}
}

// Every new burst starts with the previous burst's actual mean, not the old
// ring's long history. It can replace a stale estimate without an idle flight.
func TestWindowPacingBurstStatsResetOnEachBurst(t *testing.T) {
	start := time.Unix(1700000000, 0)
	stats := &windowBurstStats{ring: newWindowBucketStats(10*time.Millisecond, 4)}
	stats.add(1, 100, start)
	stats.add(1, 300, start.Add(time.Millisecond))
	stats.add(2, 400, start.Add(10*time.Millisecond))
	if mean, _ := stats.ring.mean(start.Add(10 * time.Millisecond)); mean != 200 {
		t.Fatalf("new burst did not hold the prior burst's mean: %v", mean)
	}
	stats.add(3, 600, start.Add(20*time.Millisecond))
	if mean, _ := stats.ring.mean(start.Add(20 * time.Millisecond)); mean != 400 {
		t.Fatalf("reset carried old ring history instead of the completed burst: %v", mean)
	}
	if mean, _ := stats.ring.mean(start.Add(60 * time.Millisecond)); mean != 600 {
		t.Fatalf("completed new evidence did not replace the hold: %v", mean)
	}
}

// Older bursts can finish after newer feedback. Arrival time alone cannot
// authorize their samples to restart a current measurement epoch.
func TestWindowPacingBurstStatsRejectLateOlderBursts(t *testing.T) {
	start := time.Unix(1700000000, 0)
	stats := &windowBurstStats{ring: newWindowBucketStats(10*time.Millisecond, 4)}
	stats.add(1, 100, start)
	stats.add(2, 200, start.Add(10*time.Millisecond))
	if stats.add(1, 999, start.Add(20*time.Millisecond)) || stats.add(3, 999, start.Add(9*time.Millisecond)) {
		t.Fatal("an older burst or reordered arrival restarted the newer ring")
	}
	if mean, _ := stats.ring.mean(start.Add(50 * time.Millisecond)); mean != 200 {
		t.Fatalf("old feedback changed new evidence: %v", mean)
	}
}

// Different workers may apply samples from one retained burst out of order.
// They still contribute to its summary, without rewinding its newest arrival
// or allowing a preceding burst to contaminate the same time bucket.
func TestWindowPacingBurstStatsAcceptRetainedCurrentBurst(t *testing.T) {
	start := time.Unix(1700000000, 0)
	stats := &windowBurstStats{ring: newWindowBucketStats(10*time.Millisecond, 4)}
	stats.add(1, 100, start.Add(2*time.Millisecond))
	if !stats.add(1, 300, start.Add(time.Millisecond)) {
		t.Fatal("a retained observation from the current burst was discarded")
	}
	if stats.lastAt != start.Add(2*time.Millisecond) {
		t.Fatal("a late current-burst observation rewound the arrival clock")
	}
	stats.add(2, 400, start.Add(10*time.Millisecond))
	if mean, _ := stats.ring.mean(start.Add(10 * time.Millisecond)); mean != 200 {
		t.Fatalf("the previous burst's hold lost a valid reordered sample: %v", mean)
	}
	if stats.add(1, 999, start.Add(11*time.Millisecond)) {
		t.Fatal("an older burst reused the current burst's time bucket")
	}
}

// The first ACK applied for a burst need not be its earliest arrival. A known
// current burst may repair either retained bucket even after the ring resets.
func TestWindowPacingBurstStatsAcceptRetainedAfterReset(t *testing.T) {
	for _, test := range []struct {
		name string
		late time.Duration
		mean float64
	}{
		{name: "same-bucket", late: 11 * time.Millisecond, mean: 150},
		{name: "preceding-bucket", late: 9 * time.Millisecond, mean: 200},
	} {
		start := time.Unix(1700000000, 0)
		stats := &windowBurstStats{ring: newWindowBucketStats(10*time.Millisecond, 4)}
		stats.add(1, 100, start)
		stats.add(2, 400, start.Add(12*time.Millisecond))
		if !stats.add(2, 200, start.Add(test.late)) {
			t.Errorf("%s: a retained current-burst ACK before its first applied arrival was discarded", test.name)
			continue
		}
		if stats.lastAt != start.Add(12*time.Millisecond) || stats.burstMean != 300 || stats.count != 2 {
			t.Fatalf("%s: reordered ACK corrupted burst state: latest=%s mean=%v count=%d", test.name, stats.lastAt.Sub(start), stats.burstMean, stats.count)
		}
		if stats.add(1, 999, start.Add(test.late)) || stats.add(2, 999, start.Add(-time.Second)) {
			t.Fatalf("%s: an older burst or expired current-burst ACK changed retained evidence", test.name)
		}
		if mean, count := stats.ring.mean(start.Add(20 * time.Millisecond)); mean != test.mean || count != 4 {
			t.Fatalf("%s: reordered ACK did not repair its retained bucket: mean=%v count=%d want=%v", test.name, mean, count, test.mean)
		}
		stats.add(3, 600, start.Add(20*time.Millisecond))
		if mean, _ := stats.ring.mean(start.Add(20 * time.Millisecond)); mean != 300 {
			t.Fatalf("%s: the next burst lost the repaired previous-burst hold: mean=%v want=300", test.name, mean)
		}
		if stats.add(2, 999, start.Add(21*time.Millisecond)) {
			t.Fatalf("%s: a now older burst overwrote the new epoch", test.name)
		}
	}
}
