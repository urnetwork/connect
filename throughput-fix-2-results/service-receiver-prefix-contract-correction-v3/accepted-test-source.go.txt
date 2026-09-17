// Cold sdk discovery uses the receiver wait belonging to each credited head.
// Physical offers and ACK arrivals are explicit, without sockets or workers.
package connect

import (
	"testing"
	"time"
)

// One logical sender publishes exact first-write timing into a shared service.
// Keeping the sender separate permits another sibling to publish between calls.
type windowReceiverCreditFixture struct {
	sequence    *SendSequence
	service     *windowPacingService
	ackWindow   *sequenceAckWindow
	compression time.Duration
}

// Construction creates no background work; the caller owns every clock edge.
func newWindowReceiverCreditFixture(t *testing.T, service *windowPacingService, at time.Time, compression time.Duration) *windowReceiverCreditFixture {
	t.Helper()
	if service == nil {
		service = newWindowPacingService(DefaultSendBufferSettings())
	}
	sequenceId := NewId()
	sequence := &SendSequence{
		sequenceId: sequenceId, client: &Client{feedbackTimeBase: at},
		resendQueue: newResendQueue(nil, 0),
		rttWindow:   NewRttWindow(nil, 4, time.Second, 2, 2*time.Second, 300*time.Millisecond, 8*time.Second),
		windowPacer: windowBurstPacer{service: service, serviceSequenceId: sequenceId},
	}
	t.Cleanup(sequence.windowPacer.close)
	return &windowReceiverCreditFixture{sequence: sequence, service: service, ackWindow: newSequenceAckWindow(), compression: compression}
}

// Enrollment precedes carrier confirmation, including synchronous ACK cases.
func (self *windowReceiverCreditFixture) offer(number uint64, bytes ByteCount, at time.Time) *sendItem {
	item := &sendItem{
		transferItem: transferItem{messageId: NewId(), sequenceNumber: number},
		sendTime:     at, sendCount: 1, expectsAck: true,
		pacingByteCount: bytes, pacingSentAtNanos: at.UnixNano(), rttState: sendItemRttWritePending,
	}
	self.sequence.resendQueue.Add(item)
	self.sequence.windowPacer.serviceSent += bytes
	self.service.sent += bytes
	self.service.beginWrite(self.sequence.sequenceId, item.messageId, number, at, false)
	return item
}

// Both RTT and physical-flight ownership observe the same successful H1 write.
func (self *windowReceiverCreditFixture) confirm(item *sendItem) {
	self.sequence.finishReceiverRttWrite(item, transferWriteDisposition{transportType: TransportTypeH1, reliable: true}, nil)
	self.service.finishWrite(self.sequence.sequenceId, item.messageId, true)
}

// The common positive path confirms before exposing a reply.
func (self *windowReceiverCreditFixture) write(number uint64, bytes ByteCount, at time.Time) *sendItem {
	item := self.offer(number, bytes, at)
	self.confirm(item)
	return item
}

// Wire metadata retains the raw arrival, the exact physical tag and its wait.
func (self *windowReceiverCreditFixture) ack(item *sendItem, at time.Time, wait time.Duration) receiveAckMessage {
	return receiveAckMessage{
		messageId: item.messageId, tag: sequenceTag{sendTime: uint64(item.sendTime.UnixMilli()), set: true},
		receivedAtNanos: at.UnixNano(), receiverAckDelaySet: true, receiverAckDelayMicros: uint32(wait.Microseconds()),
		ackCompressTimeoutSet: true, ackCompressTimeoutMicros: uint32(self.compression.Microseconds()),
	}
}

// Measurements use the caller's clock and advance only the controller hold.
func (self *windowReceiverCreditFixture) measured(at time.Time) ByteCount {
	rate, _, latest := self.service.measured(time.Second, at)
	return max(rate, latest)
}

// The original sdk trace has 123 confirmed 2670-byte envelopes. A tail wait
// names only that head; these fields cannot prove that its entire newly
// credited prefix arrived before it. Exact serialization may remain unknown.
func TestWindowPacingSdkColdReceiverWaitKeepsPrefixBounded(t *testing.T) {
	for _, path := range []time.Duration{100 * time.Millisecond, 400 * time.Millisecond} {
		for _, phase := range []time.Duration{0, 8 * time.Millisecond} {
			start := time.Unix(1700000000, 0).Add(phase)
			fixture := newWindowReceiverCreditFixture(t, nil, start, 10*time.Millisecond)
			var opening [123]*sendItem
			for number := range opening {
				opening[number] = fixture.write(uint64(number), 2670, start)
			}
			firstAt := start.Add(path + 21360*time.Nanosecond)
			fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(opening[0], firstAt, 0))
			if rate := fixture.measured(firstAt); rate != 0 {
				t.Fatalf("path=%s phase=%s: a single cold reply granted service=%d", path, phase, rate)
			}
			// A refill precedes the old cumulative tail, so no complete-drain
			// shortcut can supply the measured serialization or reset history.
			refill := fixture.write(123, 2670, firstAt)
			lastAt := start.Add(path + 3627280*time.Nanosecond)
			fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(opening[122], lastAt, time.Millisecond))
			if fixture.service.drained || fixture.service.total != 328410 || fixture.service.latestRoundTrip != lastAt.Sub(start) {
				t.Fatalf("path=%s phase=%s: raw timing or physical credit changed: drained=%t bytes=%d raw=%s", path, phase, fixture.service.drained, fixture.service.total, fixture.service.latestRoundTrip)
			}
			if rate := fixture.measured(lastAt); rate > 125000000 {
				t.Errorf("path=%s phase=%s: ambiguous prefix exceeded physical125MB/s: service=%d", path, phase, rate)
			}
			// One later head cannot repair the missing destination-order proof
			// for the preceding prefix. Final pacing recovery is tested by the
			// unchanged static sdk and shared-pacer throughput fixtures.
			refillAt := firstAt.Add(path + 21360*time.Nanosecond)
			fixture.write(124, 2670, lastAt)
			fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(refill, refillAt, 0))
			if rate := fixture.measured(refillAt); rate > 125000000 {
				t.Errorf("path=%s phase=%s: refill exceeded physical125MB/s: %d", path, phase, rate)
			}
			if fixture.service.drained || fixture.service.total != 331080 || fixture.service.latestRoundTrip != refillAt.Sub(firstAt) {
				t.Fatalf("path=%s phase=%s: refill changed physical credit or raw timing: drained=%t bytes=%d raw=%s", path, phase, fixture.service.drained, fixture.service.total, fixture.service.latestRoundTrip)
			}
		}
	}
}

// With no paired wait, the same short train retains the legacy compression
// qualification. A larger configured bootstrap is not delivery evidence.
func TestWindowPacingSdkColdLegacyTrainKeepsCompressionBound(t *testing.T) {
	start := time.Unix(1700000000, 0)
	fixture := newWindowReceiverCreditFixture(t, nil, start, 10*time.Millisecond)
	first := fixture.write(0, 2670, start)
	last := fixture.write(1, 325740, start)
	for _, point := range []struct {
		item *sendItem
		at   time.Time
	}{
		{item: first, at: start.Add(100021360 * time.Nanosecond)},
		{item: last, at: start.Add(103627280 * time.Nanosecond)},
	} {
		ack := fixture.ack(point.item, point.at, 0)
		ack.receiverAckDelaySet = false
		fixture.sequence.coalesceReceivedAck(fixture.ackWindow, ack)
		fixture.service.observeRoundTrip(point.at.Sub(start), fixture.compression, point.at)
		if rate := fixture.measured(point.at); rate != 0 {
			t.Fatalf("unpaired short train acquired corrected service=%d", rate)
		}
	}
}

// A cold serializer is allowed to be slow. Equal receiver waits preserve
// its 100 ms physical interval rather than manufacturing a capacity floor.
func TestWindowPacingSdkColdEqualWaitsDiscoverSlowService(t *testing.T) {
	start := time.Unix(1700000000, 0)
	fixture := newWindowReceiverCreditFixture(t, nil, start, 10*time.Millisecond)
	first := fixture.write(0, 2670, start)
	last := fixture.write(1, 2670, start)
	firstAt := start.Add(401 * time.Millisecond)
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(first, firstAt, time.Millisecond))
	fixture.write(2, 2670, firstAt)
	lastAt := firstAt.Add(100 * time.Millisecond)
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(last, lastAt, time.Millisecond))
	if rate := fixture.measured(lastAt); rate != 26700 {
		t.Fatalf("equal receiver waits hid cold slow service=%d, want26700", rate)
	}
}

// A complete cold flight proves byte delivery and the raw physical drain.
// It does not prove that the original ingress of a cumulative head encloses
// every earlier envelope released with it.
func TestWindowPacingSdkColdDrainedPrefixKeepsRawClocks(t *testing.T) {
	start := time.Unix(1700000000, 0)
	fixture := newWindowReceiverCreditFixture(t, nil, start, 10*time.Millisecond)
	var opening [123]*sendItem
	for number := range opening {
		opening[number] = fixture.write(uint64(number), 2670, start)
	}
	firstAt := start.Add(400021360 * time.Nanosecond)
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(opening[0], firstAt, 0))
	lastAt := start.Add(403627280 * time.Nanosecond)
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(opening[122], lastAt, time.Millisecond))
	if !fixture.service.drained || fixture.service.total != 328410 || fixture.service.latestRoundTrip != lastAt.Sub(start) {
		t.Fatal("the complete cold opening changed physical drain, credit or raw timing")
	}
	if got := fixture.measured(lastAt); got > 125000000 {
		t.Fatalf("a drained ambiguous prefix exceeded physical125MB/s: %d", got)
	}
}

// Many bytes in one exact cumulative head are still only one checkpoint.
// Physical completion alone grants no serialization rate.
func TestWindowPacingSdkColdOneCumulativeReplyStaysUnmeasured(t *testing.T) {
	start := time.Unix(1700000000, 0)
	fixture := newWindowReceiverCreditFixture(t, nil, start, 10*time.Millisecond)
	var last *sendItem
	for number := range 123 {
		last = fixture.write(uint64(number), 2670, start)
	}
	at := start.Add(403627280 * time.Nanosecond)
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(last, at, time.Millisecond))
	if !fixture.service.drained || fixture.service.total != 328410 || fixture.measured(at) != 0 {
		t.Fatalf("one cumulative reply manufactured service: drained=%t bytes=%d service=%d", fixture.service.drained, fixture.service.total, fixture.measured(at))
	}
}

// The established actual-worker carrier fixture can start with RTT evidence
// but no service rate. A reader releasing a completed cold train is not a
// newly fast serializer, regardless of which sample bucket it lands inside.
func TestWindowPacingSdkColdBufferedReaderKeepsCapacityUnproved(t *testing.T) {
	for _, delay := range []time.Duration{5 * time.Millisecond, 20 * time.Millisecond} {
		evidence := runWindowCarrierBufferWorker(t, true, 0, 12500000, delay)
		if evidence.serviceRate > evidence.physicalRate*101/100 || evidence.resultBurst > evidence.initialBurst*101/100 {
			t.Errorf("cold queued reader delay=%s inflated service or burst: %+v", delay, evidence)
		}
	}
}

// Identical cold offering with the reader running exposes real serialization.
// Cold queue protection must retain that measured capacity.
func TestWindowPacingSdkColdUnqueuedReaderMeasuresCapacity(t *testing.T) {
	evidence := runWindowCarrierBufferWorker(t, false, 0, 12500000, 0)
	if evidence.serviceRate < evidence.physicalRate*99/100 || evidence.serviceRate > evidence.physicalRate*101/100 {
		t.Fatalf("cold unqueued physical train lost capacity: %+v", evidence)
	}
}

// A newly credited single physical head owns all the bytes whose receiver
// wait is removed. No cumulative prefix or inferred destination FIFO is used.
func TestWindowPacingSdkColdSingleHeadWaitMeasuresShortSerialization(t *testing.T) {
	for _, path := range []time.Duration{100 * time.Millisecond, 400 * time.Millisecond} {
		for _, phase := range []time.Duration{0, 8 * time.Millisecond} {
			start := time.Unix(1700000000, 0).Add(phase)
			fixture := newWindowReceiverCreditFixture(t, nil, start, 10*time.Millisecond)
			first := fixture.write(0, 2670, start)
			head := fixture.write(1, 2670, start)
			firstAt := start.Add(path + 21360*time.Nanosecond)
			fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(first, firstAt, 0))
			if got := fixture.measured(firstAt); got != 0 {
				t.Fatalf("one checkpoint granted service=%d", got)
			}
			at := firstAt.Add(21360*time.Nanosecond + time.Millisecond)
			fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(head, at, time.Millisecond))
			if fixture.service.total != 5340 || !fixture.service.drained || fixture.service.latestRoundTrip != at.Sub(start) {
				t.Fatal("single-head timing changed physical credit or raw drain clock")
			}
			if got := fixture.measured(at); got != 125000000 {
				t.Errorf("path=%s phase=%s: exact head's own wait hid short serialization=%d", path, phase, got)
			}
		}
	}
}
