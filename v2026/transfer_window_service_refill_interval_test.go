// Rolling sibling refills keep physical work outstanding while exact receiver
// waits distinguish serializer time from the next round trip's idle interval.
package connect

import (
	"testing"
	"time"
)

// Two siblings replace each acknowledged envelope immediately. An independent
// FIFO serializer supplies all later arrivals, including a real slowdown.
func windowReceiverRefillService(t *testing.T, serialization time.Duration) []ByteCount {
	t.Helper()
	service, start := newWindowQualifiedServiceFixture(t, 12500000, 50*time.Millisecond, 100*time.Millisecond)
	var sequences [2]*SendSequence
	var windows [2]*sequenceAckWindow
	for i := range sequences {
		sequenceId := NewId()
		sequences[i] = &SendSequence{
			sequenceId: sequenceId, client: &Client{feedbackTimeBase: start},
			resendQueue: newResendQueue(nil, 0),
			rttWindow:   NewRttWindow(nil, 4, time.Second, 2, 2*time.Second, 300*time.Millisecond, 8*time.Second),
			windowPacer: windowBurstPacer{service: service, serviceSequenceId: sequenceId},
		}
		windows[i] = newSequenceAckWindow()
		t.Cleanup(sequences[i].windowPacer.close)
	}
	write := func(sibling int, number uint64, bytes ByteCount, at time.Time) *sendItem {
		sequence := sequences[sibling]
		item := &sendItem{
			transferItem: transferItem{messageId: NewId(), sequenceNumber: number},
			sendTime:     at, sendCount: 1, expectsAck: true,
			pacingByteCount: bytes, pacingSentAtNanos: at.UnixNano(), rttState: sendItemRttWritePending,
		}
		sequence.resendQueue.Add(item)
		sequence.windowPacer.serviceSent += bytes
		service.sent += bytes
		service.beginWrite(sequence.sequenceId, item.messageId, number, at, false)
		sequence.finishReceiverRttWrite(item, transferWriteDisposition{transportType: TransportTypeH1, reliable: true}, nil)
		service.finishWrite(sequence.sequenceId, item.messageId, true)
		return item
	}
	credited := ByteCount(0)
	acknowledge := func(sibling int, item *sendItem, at time.Time, wait time.Duration) {
		sequence := sequences[sibling]
		sequence.coalesceReceivedAck(windows[sibling], receiveAckMessage{
			messageId: item.messageId, tag: sequenceTag{sendTime: uint64(item.sendTime.UnixMilli()), set: true},
			receivedAtNanos: at.UnixNano(), receiverAckDelaySet: true, receiverAckDelayMicros: uint32(wait.Microseconds()),
			ackCompressTimeoutSet: true, ackCompressTimeoutMicros: 50000,
		})
		credited += item.pacingByteCount
		if service.drained || sequences[0].windowPacer.serviceAcked+sequences[1].windowPacer.serviceAcked != credited {
			t.Fatalf("rolling physical overlap or exact byte credit lost: drained=%t credited=%d/%d", service.drained,
				sequences[0].windowPacer.serviceAcked+sequences[1].windowPacer.serviceAcked, credited)
		}
	}
	first := write(0, 0, 50000, start.Add(196*time.Millisecond))
	second := write(1, 0, 12500, start.Add(200*time.Millisecond))
	// The opening train serializes contiguously in 4 ms then 1 ms. Its tail
	// waits 49 ms at the receiver, stretching the ACK interval to 50 ms.
	firstAt, secondAt := start.Add(300*time.Millisecond), start.Add(350*time.Millisecond)
	physicalFree := start.Add(201 * time.Millisecond)
	type arrival struct {
		sibling int
		item    *sendItem
		at      time.Time
	}
	var arrivals []arrival
	numbers := [2]uint64{1, 1}
	refill := func(sibling int, at time.Time) {
		item := write(sibling, numbers[sibling], 1250, at)
		numbers[sibling]++
		if physicalFree.Before(at) {
			physicalFree = at
		}
		physicalFree = physicalFree.Add(serialization)
		arrivals = append(arrivals, arrival{sibling: sibling, item: item, at: physicalFree.Add(100 * time.Millisecond)})
	}
	acknowledge(0, first, firstAt, 0)
	refill(0, firstAt)
	acknowledge(1, second, secondAt, 49*time.Millisecond)
	refill(1, secondAt)
	if rate, _, latest := service.measured(time.Second, secondAt); max(rate, latest) != 12500000 {
		t.Fatalf("opening exact receiver pair did not preserve serialization: %d/%d", rate, latest)
	}
	var rates []ByteCount
	for range 8 {
		point := arrivals[0]
		arrivals = arrivals[1:]
		acknowledge(point.sibling, point.item, point.at, 0)
		refill(point.sibling, point.at)
		rate, _, latest := service.measured(time.Second, point.at)
		rates = append(rates, max(rate, latest))
	}
	return rates
}

// With no receiver wait, alternating small refills are separated by path
// propagation. Aging out their earlier qualified pair cannot reprice that gap.
func TestWindowPacingReceiverWaitRefillPreservesSerialization(t *testing.T) {
	for i, rate := range windowReceiverRefillService(t, 100*time.Microsecond) {
		if rate != 12500000 {
			t.Fatalf("refill %d priced propagation after the qualified pair expired: service=%d want12500000", i, rate)
		}
	}
}

// A sustained slower serializer has physical queue residence covering each
// sparse arrival. Continuous overlapping refills must discover its real rate.
func TestWindowPacingReceiverWaitRefillDiscoversSlowSerialization(t *testing.T) {
	rates := windowReceiverRefillService(t, 125*time.Millisecond)
	for i, rate := range rates[4:] {
		if rate != 10000 {
			t.Fatalf("slow refill %d retained the old service: rate=%d want10000, history=%v", i+4, rate, rates)
		}
	}
}
