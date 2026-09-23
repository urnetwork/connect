// Time and values are explicitly supplied; no wall-clock scheduling affects
// rolling means, holds, expiration or out-of-order observations.
package connect

import (
	"math"
	"testing"
	"time"
)

// Leading empty time is unknown. A retained late measurement can fill that
// time without inventing observations before the first known bucket.
func TestWindowBucketStatsLeadingUnknownAndLateHold(t *testing.T) {
	for _, interval := range []time.Duration{time.Millisecond, 10 * time.Millisecond, 50 * time.Millisecond} {
		stats := newWindowBucketStats(interval, 4)
		start := time.Unix(1700000000, 0)
		if mean, count := stats.mean(start); mean != 0 || count != 0 {
			t.Fatal("an empty statistic manufactured a zero observation")
		}
		stats.add(30, start.Add(2*interval))
		if mean, count := stats.mean(start.Add(4 * interval)); mean != 30 || count != 2 {
			t.Fatalf("interval=%s leading unknown buckets biased the mean: %v/%d", interval, mean, count)
		}
		stats.add(10, start.Add(interval))
		if mean, count := stats.mean(start.Add(4 * interval)); math.Abs(mean-70.0/3) > 1e-12 || count != 3 {
			t.Fatalf("interval=%s retained late evidence did not fill the leading hold: %v/%d", interval, mean, count)
		}
	}
}

// A partial new measurement cannot replace the last completed value, even
// with one retained bucket or after a jump much longer than the whole ring.
func TestWindowBucketStatsPartialAfterIdleKeepsCompletedHold(t *testing.T) {
	for _, lookback := range []int{1, 4, 32} {
		stats := newWindowBucketStats(10*time.Millisecond, lookback)
		start := time.Unix(1700000000, 0)
		stats.add(4, start)
		stats.add(8, start.Add(time.Hour))
		if mean, count := stats.mean(start.Add(time.Hour)); mean != 4 || count != lookback {
			t.Fatalf("lookback=%d a partial post-idle measurement replaced the hold: %v/%d", lookback, mean, count)
		}
		if stats.add(math.Inf(1), start.Add(2*time.Hour)) {
			t.Fatal("an invalid future value was accepted")
		}
		if mean, count := stats.mean(start.Add(time.Hour + 10*time.Millisecond)); math.Abs(mean-(4+4.0/float64(lookback))) > 1e-12 || count != lookback {
			t.Fatalf("lookback=%d invalid future evidence expired a valid bucket: %v/%d", lookback, mean, count)
		}
	}
}

// Equal time buckets have equal weight despite unequal observation counts.
func TestWindowBucketStatsWeightsCompletedBucketsEqually(t *testing.T) {
	stats := newWindowBucketStats(10*time.Millisecond, 4)
	start := time.Unix(1700000000, 0)
	for range 100 {
		stats.add(10, start)
	}
	stats.add(30, start.Add(10*time.Millisecond))
	stats.add(10000, start.Add(20*time.Millisecond))
	mean, count := stats.mean(start.Add(20 * time.Millisecond))
	if mean != 20 || count != 2 {
		t.Fatalf("partial bucket or observation density biased the mean: %v from %d buckets", mean, count)
	}
}

// Empty buckets hold the last measured bucket mean, including after the
// entire retained ring expires. An actual zero updates the hold to zero.
func TestWindowBucketStatsZeroOrderHoldDistinguishesMeasuredZero(t *testing.T) {
	stats := newWindowBucketStats(10*time.Millisecond, 4)
	start := time.Unix(1700000000, 0)
	stats.add(8, start)
	stats.add(16, start.Add(time.Millisecond))
	mean, count := stats.mean(start.Add(40 * time.Millisecond))
	if mean != 12 || count != 4 {
		t.Fatalf("idle buckets did not hold the measured bucket mean: %v/%d", mean, count)
	}
	mean, count = stats.mean(start.Add(time.Hour))
	if mean != 12 || count != 4 {
		t.Fatalf("long idle decayed the last measured value: %v/%d", mean, count)
	}
	stats.add(0, start.Add(time.Hour))
	mean, count = stats.mean(start.Add(time.Hour + 10*time.Millisecond))
	if mean != 9 || count != 4 {
		t.Fatalf("explicit zero was treated as missing evidence: %v/%d", mean, count)
	}
	mean, count = stats.mean(start.Add(time.Hour + 40*time.Millisecond))
	if mean != 0 || count != 4 {
		t.Fatalf("zero observation failed to replace the held value: %v/%d", mean, count)
	}
}

// A bucket changes from partial to completed exactly at its right boundary.
func TestWindowBucketStatsExactBoundaries(t *testing.T) {
	for _, start := range []time.Time{time.Unix(1700000000, 0), time.Unix(0, 0).Add(-20 * time.Millisecond)} {
		stats := newWindowBucketStats(10*time.Millisecond, 2)
		stats.add(20, start.Add(10*time.Millisecond-time.Nanosecond))
		if _, count := stats.mean(start.Add(10*time.Millisecond - time.Nanosecond)); count != 0 {
			t.Fatal("partial startup bucket was treated as complete")
		}
		stats.add(1000, start.Add(10*time.Millisecond))
		if mean, count := stats.mean(start.Add(10 * time.Millisecond)); mean != 20 || count != 1 {
			t.Fatalf("boundary observation contaminated its preceding bucket: %v/%d", mean, count)
		}
	}
}

// Late measurements repair their original bucket and the hold after it,
// without replacing a newer measurement or counting either twice.
func TestWindowBucketStatsAcceptsRetainedOutOfOrderMeasurements(t *testing.T) {
	stats := newWindowBucketStats(10*time.Millisecond, 4)
	start := time.Unix(1700000000, 0)
	stats.add(10, start)
	stats.add(30, start.Add(20*time.Millisecond))
	stats.add(20, start.Add(10*time.Millisecond))
	for range 10 {
		if mean, count := stats.mean(start.Add(40 * time.Millisecond)); mean != 22.5 || count != 4 {
			t.Fatalf("late update or repeated read changed chronological weighting: %v/%d", mean, count)
		}
	}
	stats.add(40, start.Add(20*time.Millisecond))
	if mean, count := stats.mean(start.Add(40 * time.Millisecond)); math.Abs(mean-25) > 1e-12 || count != 4 {
		t.Fatalf("late update did not repair its following hold: %v/%d", mean, count)
	}
}

// Ring reuse keeps the last expired mean as its anchor; older arrivals and
// invalid values cannot overwrite that anchor or produce false fresh data.
func TestWindowBucketStatsExpirationCannotOverwriteTheHold(t *testing.T) {
	stats := newWindowBucketStats(10*time.Millisecond, 3)
	start := time.Unix(1700000000, 0)
	for i := range 20 {
		stats.add(float64(i), start.Add(time.Duration(i)*10*time.Millisecond))
	}
	if mean, count := stats.mean(start.Add(time.Second)); mean != 19 || count != 3 {
		t.Fatalf("ring expiration lost the newest measured value: %v/%d", mean, count)
	}
	for _, value := range []float64{100, math.NaN(), math.Inf(1)} {
		if stats.add(value, start) {
			t.Fatal("expired or invalid observation was accepted")
		}
	}
	if mean, count := stats.mean(start.Add(2 * time.Second)); mean != 19 || count != 3 {
		t.Fatalf("old arrival changed the hold: %v/%d", mean, count)
	}
}
