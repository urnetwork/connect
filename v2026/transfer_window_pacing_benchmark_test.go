// Measure estimator CPU and allocations separately from virtual-time goodput.
package connect

import (
	"testing"
	"time"
)

// Populate every ring slot through the ordinary observation methods. A final
// RTT-only event exercises zero hold without new delivery or a clock syscall.
func benchmarkWindowPacingService(b *testing.B, hold, retain bool) {
	service := &windowPacingService{}
	at := time.Unix(1700000000, 0)
	service.observeRoundTrip(100*time.Millisecond, 10*time.Millisecond, at)
	for i := range 2 * deliveredBytesRingSize {
		at = at.Add(10 * time.Millisecond)
		service.sent += 125000
		service.observeRoundTrip(300*time.Millisecond, 10*time.Millisecond, at)
		service.observe(125000, at)
		if i == 2 {
			if rate, _, _ := service.measured(time.Second, at); rate != 12500000 {
				b.Fatalf("fixture did not establish 12.5 MB/s service: %d", rate)
			}
		}
	}
	if hold {
		at = at.Add(time.Nanosecond)
		service.observeRoundTrip(100*time.Millisecond, 10*time.Millisecond, at)
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

// Continuous queued delivery exercises the completed-sample calculation.
func BenchmarkWindowPacingServiceContinuous(b *testing.B) {
	benchmarkWindowPacingService(b, false, true)
}

// No new byte observation is available after the last unqueued RTT event.
func BenchmarkWindowPacingServiceHold(b *testing.B) {
	benchmarkWindowPacingService(b, true, true)
}

// Public polling has the same evidence without retaining controller state.
func BenchmarkWindowPacingServiceStatisticsHold(b *testing.B) {
	benchmarkWindowPacingService(b, true, false)
}
