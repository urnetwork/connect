// Receiver timing benchmarks exercise populated history, including zero hold.
package connect

import (
	"testing"
	"time"
)

// Fill both the service buckets and the configured receiver timing history.
// The physical service remains 12.5 MB/s throughout every measured read.
func benchmarkWindowPacingReceiverService(b *testing.B, hold, retain bool) {
	settings := DefaultSendBufferSettings()
	service := newWindowPacingService(settings)
	at := time.Unix(1700000000, 0)
	service.observeReceiverRoundTrip(1, 110*time.Millisecond, 100*time.Millisecond, 10*time.Millisecond, at)
	for i := range max(2*deliveredBytesRingSize, settings.RttWindowSize) {
		at = at.Add(10 * time.Millisecond)
		service.sent += 125000
		service.observeReceiverRoundTrip(uint64(i+2), 310*time.Millisecond, 300*time.Millisecond, 10*time.Millisecond, at)
		service.observe(125000, at)
		if i == 2 {
			if rate, _, _ := service.measured(time.Second, at); rate != 12500000 {
				b.Fatalf("fixture did not establish 12.5 MB/s service: %d", rate)
			}
		}
	}
	if hold {
		at = at.Add(time.Nanosecond)
		service.observeReceiverRoundTrip(1, 110*time.Millisecond, 100*time.Millisecond, 10*time.Millisecond, at)
	}
	var rate, latest ByteCount
	b.ReportAllocs()
	for b.Loop() {
		rate, _, latest = service.measure(time.Second, at, retain)
	}
	if max(rate, latest) != 12500000 {
		b.Fatalf("unchanged physical service was repriced: %d/%d", rate, latest)
	}
	b.ReportMetric(float64(max(rate, latest)), "estimated-B/s")
}

// Receiver history is live while completed delivery buckets establish service.
func BenchmarkWindowPacingReceiverServiceContinuous(b *testing.B) {
	benchmarkWindowPacingReceiverService(b, false, true)
}

// A timing-only observation must not turn a held service read into new traffic.
func BenchmarkWindowPacingReceiverServiceHold(b *testing.B) {
	benchmarkWindowPacingReceiverService(b, true, true)
}

// Public diagnostics read the same populated history without retaining changes.
func BenchmarkWindowPacingReceiverServiceStatisticsHold(b *testing.B) {
	benchmarkWindowPacingReceiverService(b, true, false)
}

// Measure the full shared timing publication, after its bounded allocation.
func BenchmarkWindowPacingReceiverTimingObservation(b *testing.B) {
	settings := DefaultSendBufferSettings()
	service := newWindowPacingService(settings)
	at := time.Unix(1700000000, 0)
	for i := range settings.RttWindowSize {
		at = at.Add(time.Millisecond)
		service.observeReceiverRoundTrip(uint64(i+1), 11*time.Millisecond, time.Millisecond, 10*time.Millisecond, at)
	}
	burst := uint64(settings.RttWindowSize)
	b.ReportAllocs()
	for b.Loop() {
		at = at.Add(time.Millisecond)
		burst++
		service.observeReceiverRoundTrip(burst, 11*time.Millisecond, time.Millisecond, 10*time.Millisecond, at)
	}
	adjusted, residence, sampled := service.receiverWindowEstimate(at)
	if !sampled || adjusted != time.Millisecond || residence != 11*time.Millisecond {
		b.Fatalf("timing changed during publication: adjusted=%s residence=%s sampled=%t", adjusted, residence, sampled)
	}
}
