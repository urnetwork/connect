// A receiver's wait changes local ACK spacing without changing serialization.
// Exact confirmed physical heads publish their timing and byte credit together.
package connect

import (
	"testing"
	"time"
)

// Two contiguous physical envelopes traverse a fixed 100 ms path. Their
// cumulative replies take the normal coalescer path, with a refill keeping a
// tail outstanding while the second reply is delayed at the receiver.
func windowReceiverIntervalService(t *testing.T, secondBytes ByteCount, serialization, firstWait, secondWait time.Duration) ByteCount {
	t.Helper()
	service, start := newWindowQualifiedServiceFixture(t, 12500000, 50*time.Millisecond, 100*time.Millisecond)
	sequence := &SendSequence{
		sequenceId: NewId(), client: &Client{feedbackTimeBase: start},
		resendQueue: newResendQueue(nil, 0),
		rttWindow:   NewRttWindow(nil, 4, time.Second, 2, 2*time.Second, 300*time.Millisecond, 8*time.Second),
		windowPacer: windowBurstPacer{service: service},
	}
	t.Cleanup(sequence.windowPacer.close)
	write := func(number uint64, bytes ByteCount, at time.Time) *sendItem {
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
	firstArrival := start.Add(600 * time.Millisecond)
	firstSerialization := time.Duration(int64(50000) * int64(serialization) / int64(secondBytes))
	firstSent := firstArrival.Add(-100*time.Millisecond - firstWait - firstSerialization)
	first := write(0, 50000, firstSent)
	second := write(1, secondBytes, firstSent.Add(firstSerialization))
	secondArrival := firstArrival.Add(-firstWait + serialization + secondWait)
	window := newSequenceAckWindow()
	acknowledge := func(item *sendItem, at time.Time, wait time.Duration) {
		sequence.coalesceReceivedAck(window, receiveAckMessage{
			messageId: item.messageId, tag: sequenceTag{sendTime: uint64(item.sendTime.UnixMilli()), set: true},
			receivedAtNanos: at.UnixNano(), receiverAckDelaySet: true, receiverAckDelayMicros: uint32(wait.Microseconds()),
			ackCompressTimeoutSet: true, ackCompressTimeoutMicros: 50000,
		})
	}
	acknowledge(first, firstArrival, firstWait)
	write(2, 50000+secondBytes, firstArrival)
	acknowledge(second, secondArrival, secondWait)
	if service.drained || sequence.windowPacer.serviceAcked != 50000+secondBytes {
		t.Fatalf("physical overlap or exact byte credit lost: drained=%t credited=%d", service.drained, sequence.windowPacer.serviceAcked)
	}
	rate, _, latest := service.measured(time.Second, secondArrival)
	return max(rate, latest)
}

// A 49 ms receiver wait stretches a real 1 ms delivery interval to 50 ms.
// It cannot turn a supported 12.5 MB/s serializer into a 250 kB/s estimate.
func TestWindowPacingReceiverWaitPreservesPhysicalSerialization(t *testing.T) {
	if got := windowReceiverIntervalService(t, 12500, time.Millisecond, 0, 49*time.Millisecond); got != 12500000 {
		t.Fatalf("receiver wait replaced physical serialization: service=%d want12500000", got)
	}
}

// Equal waits leave the physical delivery interval unchanged. A genuinely
// slow serializer must replace the old hold without an application signal.
func TestWindowPacingEqualReceiverWaitsDiscoverSlowSerialization(t *testing.T) {
	if got := windowReceiverIntervalService(t, 12500, 50*time.Millisecond, 50*time.Millisecond, 50*time.Millisecond); got != 250000 {
		t.Fatalf("equal receiver waits hid real slow serialization: service=%d want250000", got)
	}
}

// Correcting receiver wait is not an old-rate floor. A nearby but slower
// physical interval must publish its own measured capacity.
func TestWindowPacingReceiverWaitStillDiscoversCorrectedSlowSerialization(t *testing.T) {
	if got := windowReceiverIntervalService(t, 12400, time.Millisecond, 0, 49*time.Millisecond); got != 12400000 {
		t.Fatalf("receiver wait hid a corrected slower interval: service=%d want12400000", got)
	}
}
