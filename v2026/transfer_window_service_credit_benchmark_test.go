// Arrival publication visits newly acknowledged items rather than repeatedly
// scanning a large retained window. Setup and ownership reset are amortized.
package connect

import (
	"testing"
	"time"
)

// One newly acknowledged item in a normal populated send window.
func BenchmarkWindowPacingServiceAckArrival(b *testing.B) {
	benchmarkWindowPacingServiceAckArrival(b, 512, 1)
}

// The same one-item operation with sixteen times as much retained flight.
func BenchmarkWindowPacingServiceAckArrivalLargeWindow(b *testing.B) {
	benchmarkWindowPacingServiceAckArrival(b, 8192, 1)
}

// A compressed head credits each of its thirty-two new envelopes once.
func BenchmarkWindowPacingServiceAckBatchArrival(b *testing.B) {
	benchmarkWindowPacingServiceAckArrival(b, 8192, 32)
}

// Reset consumed bits only once per entire window, keeping fixture work per
// credited item constant. Every iteration advances one actual cumulative head.
func benchmarkWindowPacingServiceAckArrival(b *testing.B, count, batch int) {
	b.Helper()
	numbers := make([]uint64, count)
	for i := range numbers {
		numbers[i] = uint64(i)
	}
	sequence, items := testWindowServiceCreditSequence(numbers)
	service := sequence.windowPacer.service
	cycleBytes := service.sent
	at := time.Unix(1700000001, 0)
	head := batch - 1
	b.ReportAllocs()
	b.ReportMetric(float64(batch), "messages/op")
	for b.Loop() {
		sequence.publishAckServiceCredit(items[head].messageId, false, at)
		at = at.Add(time.Microsecond)
		head += batch
		if head >= count {
			for _, item := range items {
				item.serviceCreditObserved = false
			}
			sequence.serviceAckHeadSet = false
			service.sent += cycleBytes
			head = batch - 1
		}
	}
}
