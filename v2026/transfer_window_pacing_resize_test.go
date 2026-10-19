// RTT-driven bucket changes retain timestamped service evidence without
// turning aggregation boundaries into new bytes or changing their epoch.
package connect

import (
	"testing"
	"time"
)

// Resizing can merge buckets or leave an aggregate wider than a new bucket.
// A late observation still belongs to that aggregate's real timestamp range.
func TestWindowPacingResizedSamplesKeepFirstBytesAndReordering(t *testing.T) {
	for _, shrinking := range []bool{false, true} {
		for _, atFirst := range []bool{false, true} {
			start := time.Unix(1700000000, 0)
			before, after := 620*time.Millisecond, 1240*time.Millisecond
			if shrinking {
				before, after = after, before
			}
			service := &windowPacingService{}
			service.observeRoundTrip(before, 0, start)
			for _, sample := range []struct {
				at    time.Duration
				bytes ByteCount
			}{
				{at: 0, bytes: 100},
				{at: 0, bytes: 200},
				{at: 5 * time.Millisecond, bytes: 1000},
				{at: 10 * time.Millisecond, bytes: 300},
				{at: 10 * time.Millisecond, bytes: 400},
				{at: 15 * time.Millisecond, bytes: 1000},
			} {
				service.observe(sample.bytes, start.Add(sample.at))
			}
			now := start.Add(16 * time.Millisecond)
			service.stateLock.Lock()
			service.observeRoundTripWithLock(after, 0, now, true)
			service.stateLock.Unlock()
			lateAt, expectedFirst, expectedRate := 5*time.Millisecond, ByteCount(300), ByteCount(193333)
			if atFirst {
				lateAt, expectedFirst, expectedRate = 0, 500, 180000
			}
			service.observe(200, start.Add(lateAt))
			if rate, total, _ := service.measured(time.Second, now); rate != expectedRate || total != 3200 {
				t.Errorf("shrinking=%t at-first=%t: resized service=%d total=%d want=%d/3200", shrinking, atFirst, rate, total, expectedRate)
			}
			count := 0
			for _, sample := range service.samples {
				if sample.bytes <= 0 {
					continue
				}
				count++
				if sample.firstAtNanos != start.UnixNano() || sample.lastAtNanos != start.Add(15*time.Millisecond).UnixNano() ||
					sample.firstBytes != expectedFirst || sample.bytes != 3200 {
					t.Errorf("shrinking=%t at-first=%t: aggregate timestamps or bytes changed: %+v", shrinking, atFirst, sample)
				}
			}
			if count != 1 {
				t.Errorf("shrinking=%t at-first=%t: overlapping aggregates=%d", shrinking, atFirst, count)
			}
		}
	}
}

// Shorter intervals expire old checkpoints by their actual last arrival.
// Late expired bytes still release ownership, without reentering service.
func TestWindowPacingResizedSamplesExpireRetainedTime(t *testing.T) {
	start := time.Unix(1700000000, 0)
	service := &windowPacingService{}
	service.observeRoundTrip(6200*time.Millisecond, 0, start)
	for _, offset := range []time.Duration{0, 500 * time.Millisecond, time.Second} {
		service.observe(1000, start.Add(offset))
	}
	now := start.Add(time.Second + time.Millisecond)
	service.observeRoundTrip(100*time.Millisecond, 0, now)
	service.observe(1000, start.Add(250*time.Millisecond))
	if rate, total, _ := service.measured(time.Second, now); rate != 2000 || total != 4000 {
		t.Fatalf("resize retention: service=%d total=%d want=2000/4000", rate, total)
	}
	retained := ByteCount(0)
	for _, sample := range service.samples {
		retained += sample.bytes
	}
	if retained != 2000 {
		t.Fatalf("resize kept expired checkpoints: %d bytes", retained)
	}
}

// An aggregate crossing a confirmed service epoch cannot be split exactly.
// Discard that ambiguous aggregate before merging, and retain the valid hold.
func TestWindowPacingResizedSamplesRespectServiceEpoch(t *testing.T) {
	for _, cutoff := range []time.Duration{10 * time.Millisecond, 12 * time.Millisecond} {
		start := time.Unix(1700000000, 0)
		service := &windowPacingService{}
		service.observeRoundTrip(620*time.Millisecond, 0, start)
		for _, offset := range []time.Duration{0, 5 * time.Millisecond, 10 * time.Millisecond, 15 * time.Millisecond} {
			service.observe(1000, start.Add(offset))
		}
		now := start.Add(16 * time.Millisecond)
		if rate, _, _ := service.measured(time.Second, now); rate != 200000 {
			t.Fatalf("cutoff=%s: initial service=%d", cutoff, rate)
		}
		service.stateLock.Lock()
		service.serviceEpochAt = start.Add(cutoff)
		service.observeRoundTripWithLock(1240*time.Millisecond, 0, now, true)
		service.stateLock.Unlock()
		service.observe(1000, start.Add(5*time.Millisecond))
		retained := ByteCount(0)
		for _, sample := range service.samples {
			retained += sample.bytes
			if sample.bytes > 0 && sample.firstAtNanos < service.serviceEpochAt.UnixNano() {
				t.Errorf("cutoff=%s: resize retained a mixed epoch: %+v", cutoff, sample)
			}
		}
		wantRetained, wantRate := ByteCount(2000), ByteCount(200000)
		if cutoff == 12*time.Millisecond {
			wantRetained, wantRate = 0, 0
		}
		if rate, total, latest := service.measured(time.Second, now); retained != wantRetained || rate != wantRate || total != 5000 || latest != 200000 {
			t.Errorf("cutoff=%s: retained=%d rate=%d total=%d latest=%d", cutoff, retained, rate, total, latest)
		}
		service.observe(1000, start.Add(20*time.Millisecond))
		service.observe(1000, start.Add(25*time.Millisecond))
		if rate, _, _ := service.measured(time.Second, start.Add(25*time.Millisecond)); rate != 200000 {
			t.Errorf("cutoff=%s: fresh evidence did not replace hold: %d", cutoff, rate)
		}
	}
}
