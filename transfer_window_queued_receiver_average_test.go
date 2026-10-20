// A queued service must average complete receiver-clock credit rather than
// keep an earlier raw-clock peak when ACK residence shifts within a bucket.
package connect

import (
	"testing"
	"time"
)

func TestWindowQueuedServiceAverageUsesCompleteReceiverClock(t *testing.T) {
	start := time.Unix(1700000000, 0)
	fixture := newWindowReceiverCreditFixture(t, nil, start, 10*time.Millisecond)
	service := fixture.service
	guard := newWindowReceiverCreditFixture(t, service, start, fixture.compression)
	guard.write(0, kib(64), start)
	t.Cleanup(func() {
		fixture.sequence.windowPacer.close()
		guard.sequence.windowPacer.close()
		fixture.sequence.resendQueue.Clear()
		guard.sequence.resendQueue.Clear()
		if service.sent != service.total || service.pendingWrites != 0 || service.reservedByteCount != 0 {
			t.Error("queued receiver average retained physical ownership")
		}
	})
	first := fixture.write(0, 64000, start)
	second := fixture.write(1, 64000, start.Add(10*time.Millisecond))
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(first, start.Add(10*time.Millisecond), 0))
	warm := start.Add(20 * time.Millisecond)
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(second, warm, 0))
	if got := fixture.measured(warm); got != 6400000 {
		t.Fatalf("independent warm service=%d want=6400000", got)
	}
	sizes := [...]ByteCount{1000, 1000, 1000, 1000, 1000, 1000, 3000, 3000, 3000}
	var items [len(sizes)]*sendItem
	for i, size := range sizes {
		arrived := start.Add(time.Duration(60+10*i) * time.Millisecond)
		wait := time.Duration(8-i) * time.Millisecond
		items[i] = fixture.write(uint64(i+2), size, arrived.Add(-wait-30*time.Millisecond))
	}
	for i, item := range items {
		arrived := start.Add(time.Duration(60+10*i) * time.Millisecond)
		fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(item, arrived, time.Duration(8-i)*time.Millisecond))
	}
	at := start.Add(140 * time.Millisecond)
	var receiverBytes, credited ByteCount
	for _, sample := range service.samples {
		if sample.lastAtNanos >= start.Add(60*time.Millisecond).UnixNano() {
			receiverBytes += sample.receiverBytes
			credited += sample.bytes
		}
	}
	if receiverBytes != 15000 || receiverBytes != credited {
		t.Fatalf("exact original ACKs lost complete receiver timing: receiver=%d credited=%d", receiverBytes, credited)
	}
	// The newest four checkpoints own 10,000B over their exact 44ms receiver
	// interval. Shorter high-rate pairs cannot perpetuate the observed queue.
	const want ByteCount = 10000 * 1000 / 44
	rate, _, latest, proof := service.measureCapacityEvidence(time.Second, at, true)
	if rate != want || latest != want || proof != at.UnixNano() || service.serviceHoldRate != want {
		t.Fatalf("queued complete receiver interval was not averaged: rate=%d latest=%d proof=%d hold=%d want=%d", rate, latest, proof, service.serviceHoldRate, want)
	}
}
