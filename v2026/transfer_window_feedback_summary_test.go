// Completed sparse cycles preserve byte accounting across independent lane workers.
package connect

import (
	"testing"
	"time"
)

// Partitions at one timestamp, including a late interior timestamp, carry
// the same evidence whether a controller or a statistics reader ran in between.
func TestWindowPacingCompletedCycleRetainsEligibleLateBytes(t *testing.T) {
	for _, readMode := range []string{"none", "statistics", "controller"} {
		for _, parts := range [][]ByteCount{{1000, 9000}, {1000, 2000, 3000, 4000}} {
			service, start := newPartialFeedbackCycleFixture(t)
			service.observe(1000, start.Add(time.Second))
			service.observe(parts[0], start.Add(2*time.Second))
			for _, bytes := range parts[1:] {
				if readMode != "none" {
					service.measure(time.Second, start.Add(2*time.Second), readMode == "controller")
				}
				service.observe(bytes, start.Add(2*time.Second))
			}
			// An older worker's ACK still lies inside this retained interval,
			// despite its timestamp having aged out of the fixed bucket ring.
			service.observe(5000, start.Add(1200*time.Millisecond))
			rate, total, latest := service.measured(time.Second, start.Add(2*time.Second))
			if max(rate, latest) != 15000 || total != 216001 {
				t.Errorf("mode=%s parts=%v: completed summary lost eligible bytes: rate=%d/%d total=%d", readMode, parts, rate, latest, total)
			}
		}
	}
}

// A physically proved old flight remains unapplied while the fresh 1 s / 2 s
// pair establishes its own epoch. Later eligible bytes must still join that pair.
func TestWindowPacingRetiredFreshCycleRetainsEligibleLateBytes(t *testing.T) {
	for _, readMode := range []string{"none", "statistics", "controller"} {
		service, start := newPartialFeedbackCycleFixture(t)
		sequenceId, tail, resumed := NewId(), NewId(), NewId()
		service.sent = service.total + 10000
		service.beginWrite(sequenceId, tail, 1, start.Add(20*time.Millisecond), false)
		service.finishWrite(sequenceId, tail, true)
		service.acknowledgeWrite(sequenceId, tail, 1, false, 10*time.Millisecond, start.Add(100*time.Millisecond))
		service.observe(1000, start.Add(100*time.Millisecond))
		service.measured(time.Second, start.Add(100*time.Millisecond))
		service.sent += 16000
		service.beginWrite(sequenceId, resumed, 2, start.Add(101*time.Millisecond), false)
		service.finishWrite(sequenceId, resumed, true)
		service.observe(1000, start.Add(time.Second))
		service.observe(1000, start.Add(2*time.Second))
		if readMode != "none" {
			rate, _, latest := service.measure(time.Second, start.Add(2*time.Second), readMode == "controller")
			if max(rate, latest) != 1000 {
				t.Fatalf("mode=%s: first fresh pair=%d/%d, want 1000", readMode, rate, latest)
			}
		}
		service.observe(9000, start.Add(2*time.Second))
		service.observe(5000, start.Add(1200*time.Millisecond))
		rate, total, latest := service.measured(time.Second, start.Add(2*time.Second))
		if max(rate, latest) != 15000 || total != 217001 {
			t.Errorf("mode=%s: retired summary lost eligible fresh bytes: rate=%d/%d total=%d", readMode, rate, latest, total)
		}
		// Only old accounting changes; that excluded timestamp cannot reprice
		// the fresh epoch, even while its retained summary remains available.
		service.observe(9000, start.Add(100*time.Millisecond))
		rate, total, latest = service.measured(time.Second, start.Add(2*time.Second))
		if max(rate, latest) != 15000 || total != 226001 {
			t.Errorf("mode=%s: old bytes re-entered fresh summary: rate=%d/%d total=%d", readMode, rate, latest, total)
		}
	}
}

// Once a newer train supplies an accepted rate, a late fragment of the older
// summarized cycle remains accounting and cannot resurrect its obsolete fallback.
func TestWindowPacingSupersededCycleCannotReturnThroughLateBytes(t *testing.T) {
	service, start := newPartialFeedbackCycleFixture(t)
	service.observe(1000, start.Add(time.Second))
	service.observe(10000, start.Add(2*time.Second))
	service.measured(time.Second, start.Add(2*time.Second))
	service.observe(10, start.Add(2010*time.Millisecond))
	service.observe(10, start.Add(2020*time.Millisecond))
	if rate, _, latest := service.measured(time.Millisecond, start.Add(2020*time.Millisecond)); max(rate, latest) != 1000 {
		t.Fatalf("new sustained slower rate=%d/%d, want 1000", rate, latest)
	}
	service.observe(9000, start.Add(1200*time.Millisecond))
	if rate, _, latest := service.measured(time.Millisecond, start.Add(2020*time.Millisecond)); max(rate, latest) != 1000 {
		t.Fatalf("superseded cycle regained eligibility through an old fragment: %d/%d", rate, latest)
	}
}

// A neighboring arrival inside one compressed turn can extend the accepted
// summary without fabricating a rate from the short spacing of those replies.
func TestWindowPacingCompletedCycleExtendsThroughCompressedSuffix(t *testing.T) {
	service, start := newPartialFeedbackCycleFixture(t)
	service.observe(1000, start.Add(time.Second))
	service.observe(1000, start.Add(2*time.Second))
	service.measured(time.Second, start.Add(2*time.Second))
	service.observe(9000, start.Add(2001*time.Millisecond))
	rate, total, latest := service.measured(time.Second, start.Add(2001*time.Millisecond))
	if max(rate, latest) != 9990 || total != 211001 {
		t.Fatalf("compressed suffix lost its full 10000-byte/1001-ms span: %d/%d total=%d", rate, latest, total)
	}
}

// A later compressed reply extends the retained interval under its advertised
// timing. A subsequent advertisement cannot reinterpret those same bytes.
func TestWindowPacingRetainedSummaryPinsLaterCompression(t *testing.T) {
	service, start := newPartialFeedbackCycleFixture(t)
	service.observe(1000, start.Add(time.Second))
	service.observe(1000, start.Add(2*time.Second))
	service.measured(time.Second, start.Add(2*time.Second))
	service.observeRoundTrip(5*time.Millisecond, 100*time.Millisecond, start.Add(2*time.Second))
	service.observe(10000, start.Add(2025*time.Millisecond))
	rate, _, latest := service.measured(time.Second, start.Add(2025*time.Millisecond))
	if max(rate, latest) != 10731 {
		t.Fatalf("extended compressed interval rate=%d/%d, want 11000 bytes / 1025 ms", rate, latest)
	}
	service.observeRoundTrip(5*time.Millisecond, 0, start.Add(2025*time.Millisecond))
	for _, retain := range []bool{false, true} {
		next, _, nextLatest := service.measure(time.Second, start.Add(2025*time.Millisecond), retain)
		if max(next, nextLatest) != max(rate, latest) {
			t.Fatalf("retain=%t: smaller advertisement repriced identical retained bytes: %d/%d -> %d/%d", retain, rate, latest, next, nextLatest)
		}
	}
}
