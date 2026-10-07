// Arrival credit can precede legacy timing publication in the send owner.
// The two clocks must not let old queue provenance evict newer delivered bytes.
package connect

import (
	"testing"
	"time"
)

// A delayed but otherwise valid timing observation still refreshes residence.
// Its retired delivery slot cannot erase either endpoint of a current pair.
func TestWindowPacingLateQueuedTimingCannotEvictRecentDelivery(t *testing.T) {
	for _, metadata := range []bool{false, true} {
		for _, olderOffset := range []time.Duration{10 * time.Millisecond, 20 * time.Millisecond} {
			start := time.Unix(1700000000, 0)
			service := newWindowPacingService(DefaultSendBufferSettings())
			observeTiming := func(raw time.Duration, at time.Time) {
				if metadata {
					service.observeReceiverRoundTrip(0, raw, raw, 0, at)
				} else {
					service.observeRoundTrip(raw, 0, at)
				}
			}
			for i := 0; i <= 8; i++ {
				at := start.Add(time.Duration(i) * 10 * time.Millisecond)
				service.sent += 10000
				observeTiming(300*time.Microsecond, at)
				service.observe(10000, at)
			}
			start = start.Add(80 * time.Millisecond)
			if rate, _, _ := service.measured(time.Second, start); rate != 1000000 {
				t.Fatalf("initial service=%d, want 1000000", rate)
			}
			// The ack worker can publish later first-delivery credit while
			// the paced owner has not yet consumed the older timing update.
			for _, elapsed := range []time.Duration{650 * time.Millisecond, 660 * time.Millisecond} {
				service.sent += 125000
				service.observe(125000, start.Add(elapsed))
			}
			now := start.Add(660 * time.Millisecond)
			if rate, _, _ := service.measure(time.Second, now, false); rate != 12500000 {
				t.Fatalf("current unread pair did not establish service: %d", rate)
			}
			observeTiming(20*time.Millisecond, start.Add(olderOffset))
			if timing := service.roundTripEvidence(now); timing.latest != 20*time.Millisecond {
				t.Fatalf("metadata=%t offset=%s: valid late timing was discarded: %+v", metadata, olderOffset, timing)
			}
			rate, total, latest := service.measured(time.Second, now)
			if rate != 12500000 || total != 340000 {
				t.Errorf("metadata=%t offset=%s: retired queued slot erased current delivery: rate=%d/%d total=%d", metadata, olderOffset, rate, latest, total)
			}
		}
	}
}

// The oldest retained slot still accepts queue provenance. Clean newer timing
// may replace its tuple before the byte worker publishes that queued batch.
func TestWindowPacingOldestRetainedQueuedTimingStillQualifiesDelivery(t *testing.T) {
	service, start := newWindowQualifiedServiceFixture(t, 12500000, 0, 610*time.Millisecond)
	queuedAt := start.Add(10 * time.Millisecond)
	now := queuedAt.Add(630 * time.Millisecond)
	service.sent++
	service.observe(1, now)
	service.observeReceiverRoundTrip(0, 800*time.Millisecond, 800*time.Millisecond, 0, queuedAt)
	for i := range DefaultSendBufferSettings().RttWindowSize {
		service.observeReceiverRoundTrip(0, 610*time.Millisecond, 610*time.Millisecond, 0, queuedAt.Add(time.Millisecond+time.Duration(i)*time.Nanosecond))
	}
	for i := range 25 {
		service.sent += 2000
		service.observe(2000, queuedAt.Add(time.Duration(i)*10*time.Microsecond))
	}
	rate, total, latest := service.measured(time.Second, now)
	if max(rate, latest) > 12500000 || total != 1175001 {
		t.Fatalf("oldest retained queued batch lost its arrival provenance: rate=%d/%d total=%d", rate, latest, total)
	}
}
