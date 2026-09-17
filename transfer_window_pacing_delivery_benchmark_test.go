// Isolate common-history publication, reads and cadence changes. Inline byte
// sizes distinguish the bounded storage cost from per-operation allocations.
package connect

import (
	"testing"
	"time"
	"unsafe"
)

// Populate receiver timing and raw delivery before benchmark accounting so
// every ring-size comparison measures the same established observation path.
func newWindowPacingDeliveryBenchmark(b *testing.B) (*windowPacingService, time.Time) {
	b.Helper()
	settings := DefaultSendBufferSettings()
	settings.RttWindowTimeout = 24 * time.Hour
	service := newWindowPacingService(settings)
	at := time.Unix(1700000000, 0)
	for i := range settings.RttWindowSize {
		service.observeReceiverRoundTrip(uint64(i+1), 11*time.Millisecond, 10*time.Millisecond, 10*time.Millisecond, at)
		at = at.Add(time.Microsecond)
	}
	service.stateLock.Lock()
	for range 128 {
		at = at.Add(10 * time.Millisecond)
		service.observeAggregateDeliveryWithLock(windowServiceAckCredit{
			bytes: 1000, firstSentAtNanos: at.Add(-10 * time.Millisecond).UnixNano(), receiverTimingEligible: true,
		}, at)
	}
	service.stateLock.Unlock()
	b.ReportAllocs()
	return service, at
}

// b.Loop resets metrics when measurement starts, so structural metrics are
// reported after the loop instead of during fixture construction.
func reportWindowPacingDeliveryBenchmarkMetrics(b *testing.B) {
	b.ReportMetric(float64(unsafe.Sizeof(windowServiceDeliveryRing{})), "ring-bytes")
	b.ReportMetric(float64(unsafe.Sizeof(windowPacingService{})), "service-bytes")
}

// One confirmed envelope contributes once through the locked ring publisher.
func BenchmarkWindowPacingSharedDeliveryPublication(b *testing.B) {
	service, at := newWindowPacingDeliveryBenchmark(b)
	for b.Loop() {
		at = at.Add(time.Microsecond)
		service.stateLock.Lock()
		service.observeAggregateDeliveryWithLock(windowServiceAckCredit{
			bytes: 1000, firstSentAtNanos: at.Add(-10 * time.Millisecond).UnixNano(), receiverTimingEligible: true,
		}, at)
		service.stateLock.Unlock()
	}
	reportWindowPacingDeliveryBenchmarkMetrics(b)
}

// Reads inspect the existing bounded history without creating or aging credit.
func BenchmarkWindowPacingSharedDeliveryEstimate(b *testing.B) {
	service, at := newWindowPacingDeliveryBenchmark(b)
	for b.Loop() {
		if rate := service.aggregateDelivery(at, 20*time.Millisecond, 0).byteRate(); rate <= 0 {
			b.Fatal("benchmark lost qualified common delivery")
		}
	}
	reportWindowPacingDeliveryBenchmarkMetrics(b)
}

// Changing residence copies only the dedicated fixed ring, retaining exact
// endpoints while wholly retired aggregates leave its shorter horizon.
func BenchmarkWindowPacingSharedDeliveryRebucket(b *testing.B) {
	service, at := newWindowPacingDeliveryBenchmark(b)
	interval := 10 * time.Millisecond
	for b.Loop() {
		interval = 30*time.Millisecond - interval
		at = at.Add(10 * time.Millisecond)
		service.stateLock.Lock()
		service.aggregate.resize(interval)
		service.aggregate.insert(windowServiceDeliveryTestSample(at, 1000))
		service.stateLock.Unlock()
	}
	reportWindowPacingDeliveryBenchmarkMetrics(b)
}
