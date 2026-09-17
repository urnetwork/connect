// Measure ACK publication with live receiver timing, separately from estimator
// reads and from the credit-only fixtures that have no receiver history.
package connect

import (
	"testing"
	"time"
)

// One newly acknowledged item with a full receiver timing ring.
func BenchmarkWindowPacingReceiverAckPublication(b *testing.B) {
	benchmarkWindowPacingReceiverAckPublication(b, 512, 1)
}

// Keep the same timing work with sixteen times as much retained flight.
func BenchmarkWindowPacingReceiverAckPublicationLargeWindow(b *testing.B) {
	benchmarkWindowPacingReceiverAckPublication(b, 8192, 1)
}

// A compressed head publishes one timing tuple and thirty-two new envelopes.
func BenchmarkWindowPacingReceiverAckBatchPublication(b *testing.B) {
	benchmarkWindowPacingReceiverAckPublication(b, 8192, 32)
}

// Includes validation and attribution of the exact confirmed H1 head.
func BenchmarkWindowPacingReceiverHeadCoalescing(b *testing.B) {
	benchmarkWindowPacingReceiverHeadCoalescing(b, 512, 1)
}

// Retained flight size must not add a scan to single-head publication.
func BenchmarkWindowPacingReceiverHeadCoalescingLargeWindow(b *testing.B) {
	benchmarkWindowPacingReceiverHeadCoalescing(b, 8192, 1)
}

// One timed cumulative head covers thirty-two newly delivered envelopes.
func BenchmarkWindowPacingReceiverHeadBatchCoalescing(b *testing.B) {
	benchmarkWindowPacingReceiverHeadCoalescing(b, 8192, 32)
}

// Reuse confirmed fixture envelopes once per window to isolate ACK work from
// allocation and dispatch. Every iteration goes through the real coalescer;
// the older publication-only benchmark intentionally bypasses this validation.
func benchmarkWindowPacingReceiverHeadCoalescing(b *testing.B, count, batch int) {
	b.Helper()
	numbers := make([]uint64, count)
	for i := range numbers {
		numbers[i] = uint64(i)
	}
	sequence, items := testWindowServiceCreditSequence(numbers)
	sequence.sequenceId = NewId()
	sequence.rttWindow = NewRttWindow(nil, 4, time.Second, 2, 2*time.Second, 300*time.Millisecond, 8*time.Second)
	service := sequence.windowPacer.service
	cycleBytes := service.sent
	at := time.Unix(1700000001, 0)
	for _, item := range items {
		item.sendCount, item.expectsAck = 1, true
		item.rttState, item.rttH1 = sendItemRttWriteConfirmed, true
	}
	for i := range service.receiverRoundTrips.windowSize {
		at = at.Add(time.Microsecond)
		service.observeReceiverRoundTrip(uint64(i+1), 11*time.Millisecond, 10*time.Millisecond, 10*time.Millisecond, at)
	}
	window := newSequenceAckWindow()
	head := batch - 1
	observed := 0
	b.ReportAllocs()
	for b.Loop() {
		at = at.Add(time.Microsecond)
		item := items[head]
		item.sendTime = at.Add(-11 * time.Millisecond)
		item.pacingSentAtNanos = item.sendTime.UnixNano()
		sequence.coalesceReceivedAck(window, receiveAckMessage{
			messageId: item.messageId, tag: sequenceTag{sendTime: uint64(item.sendTime.UnixMilli()), set: true},
			receivedAtNanos: at.UnixNano(), receiverAckDelaySet: true, receiverAckDelayMicros: 1000,
			ackCompressTimeoutSet: true, ackCompressTimeoutMicros: 10000,
		})
		if item.rttState == sendItemRttObserved {
			observed++
		}
		head += batch
		if head >= count {
			for _, item := range items {
				item.serviceCreditObserved = false
				item.rttState = sendItemRttWriteConfirmed
			}
			sequence.serviceAckHeadSet = false
			sequence.windowPacer.serviceSent += cycleBytes
			service.sent += cycleBytes
			window.hasHeadAck, window.ackUpdateCount = false, 0
			head = batch - 1
		}
	}
	cycles := b.N / (count / batch)
	remaining := ByteCount((b.N % (count / batch)) * batch)
	expected := ByteCount(cycles)*cycleBytes + 1000*remaining + remaining*(remaining-1)/2
	if observed != b.N || service.total != expected || sequence.windowPacer.serviceAcked != expected || service.sent < expected {
		b.Fatalf("head publication lost timing or delivery: observed=%d/%d total=%d owned=%d sent=%d want=%d", observed, b.N, service.total, sequence.windowPacer.serviceAcked, service.sent, expected)
	}
	timing := service.roundTripEvidence(at)
	if timing.count != service.receiverRoundTrips.windowSize || timing.minimum != 10*time.Millisecond || timing.latestRaw != 11*time.Millisecond || timing.residence != 20*time.Millisecond {
		b.Fatalf("receiver history changed during coalescing: %+v", timing)
	}
	b.ReportMetric(float64(batch), "messages/op")
	b.ReportMetric(float64(timing.count), "rtt-samples/op")
}

// Every measured iteration publishes fresh timing before its logical credit,
// so the bounded history remains populated regardless of benchmark duration.
// Envelope reuse is amortized once per window, as in the credit-only fixture.
func benchmarkWindowPacingReceiverAckPublication(b *testing.B, count, batch int) {
	b.Helper()
	numbers := make([]uint64, count)
	for i := range numbers {
		numbers[i] = uint64(i)
	}
	sequence, items := testWindowServiceCreditSequence(numbers)
	service := sequence.windowPacer.service
	cycleBytes := service.sent
	at := time.Unix(1700000001, 0)
	burst := uint64(0)
	for range service.receiverRoundTrips.windowSize {
		at = at.Add(time.Microsecond)
		burst++
		service.observeReceiverRoundTrip(burst, 11*time.Millisecond, time.Millisecond, 10*time.Millisecond, at)
	}
	head := batch - 1
	b.ReportAllocs()
	for b.Loop() {
		at = at.Add(time.Microsecond)
		burst++
		service.observeReceiverRoundTrip(burst, 11*time.Millisecond, time.Millisecond, 10*time.Millisecond, at)
		sequence.publishAckServiceCredit(items[head].messageId, false, at)
		head += batch
		if head >= count {
			for _, item := range items {
				item.serviceCreditObserved = false
			}
			sequence.serviceAckHeadSet = false
			sequence.windowPacer.serviceSent += cycleBytes
			service.sent += cycleBytes
			head = batch - 1
		}
	}
	cycles := b.N / (count / batch)
	remaining := ByteCount((b.N % (count / batch)) * batch)
	expected := ByteCount(cycles)*cycleBytes + 1000*remaining + remaining*(remaining-1)/2
	if service.total != expected || sequence.windowPacer.serviceAcked != expected || service.sent < expected {
		b.Fatalf("ACK publication lost delivery: total=%d owned=%d sent=%d want=%d", service.total, sequence.windowPacer.serviceAcked, service.sent, expected)
	}
	timing := service.roundTripEvidence(at)
	if timing.count != service.receiverRoundTrips.windowSize || timing.minimum != time.Millisecond || timing.residence != 11*time.Millisecond {
		b.Fatalf("receiver history changed during publication: %+v", timing)
	}
	b.ReportMetric(float64(batch), "messages/op")
	b.ReportMetric(float64(timing.count), "rtt-samples/op")
}
