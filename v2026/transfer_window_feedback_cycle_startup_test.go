// Partial feedback can prove a faster lower bound without certifying a full cycle.
package connect

import (
	"testing"
	"time"
)

// A startup control train cannot pin service after a large newly delivered
// prefix demonstrates a greater byte/time rate across the entire feedback gap.
func TestWindowPacingPartialCycleRaisesOnlyFromNewDelivery(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		start := time.Unix(1700000000, 0)
		service := &windowPacingService{sent: 10000000, minRoundTrip: 400 * time.Millisecond,
			latestRoundTrip: 400 * time.Millisecond, compression: 50 * time.Millisecond,
			lastRoundTrip: start, bucketInterval: 10 * time.Millisecond}
		service.observe(1, start)
		service.observe(500, start.Add(50*time.Millisecond))
		service.observe(500, start.Add(100*time.Millisecond))
		if rate, _, _ := service.measured(time.Second, start.Add(100*time.Millisecond)); rate != 10000 {
			t.Fatalf("read-only=%t: initial control rate=%d, want 10000", readOnly, rate)
		}
		service.observe(5000000, start.Add(500*time.Millisecond))
		for _, now := range []time.Duration{500 * time.Millisecond, 510 * time.Millisecond, time.Second} {
			rate, total, latest := service.measure(time.Second, start.Add(now), !readOnly)
			if max(rate, latest) != 12500000 || total != 5001001 || !service.feedbackPending {
				t.Fatalf("read-only=%t now=%s: new delivery rate=%d/%d total=%d pending=%t", readOnly, now, rate, latest, total, service.feedbackPending)
			}
		}
		if readOnly && service.serviceHoldRate != 10000 {
			t.Fatalf("statistics retained the partial cycle: hold=%d", service.serviceHoldRate)
		}
		service.measured(time.Second, start.Add(500*time.Millisecond))
		if service.serviceHoldRate != 12500000 {
			t.Fatalf("controller did not retain actual newly delivered lower bound: %d", service.serviceHoldRate)
		}
		service.observe(500, start.Add(550*time.Millisecond))
		if rate, _, _ := service.measured(time.Nanosecond, start.Add(550*time.Millisecond)); rate != 10000 || service.feedbackPending {
			t.Fatalf("completed genuinely slower cycle failed to replace provisional rate: rate=%d pending=%t", rate, service.feedbackPending)
		}
	}
}

// Same-time and tightly compressed replies add real bytes, but their rate
// denominator remains the full gap instead of the spacing of compressed ACKs.
func TestWindowPacingPartialCycleIncreaseKeepsCompressionGap(t *testing.T) {
	service, start := newPartialFeedbackCycleFixture(t)
	service.serviceHoldRate = 10000
	for _, sample := range []struct {
		at          time.Duration
		bytes, want ByteCount
	}{
		{at: 100 * time.Millisecond, bytes: 4000, want: 50000},
		{at: 100 * time.Millisecond, bytes: 4000, want: 100000},
		{at: 101 * time.Millisecond, bytes: 100, want: 100000},
	} {
		service.observe(sample.bytes, start.Add(sample.at))
		rate, _, latest := service.measured(time.Second, start.Add(sample.at))
		if max(rate, latest) != sample.want || !service.feedbackPending {
			t.Fatalf("partial at=%s bytes=%d: rate=%d/%d want=%d pending=%t", sample.at, sample.bytes, rate, latest, sample.want, service.feedbackPending)
		}
	}
}
