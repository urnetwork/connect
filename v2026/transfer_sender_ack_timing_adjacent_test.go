// Receiver timing separates exact raw residence from receiver-held time.
package connect

import (
	"testing"
	"testing/synctest"
	"time"
	"unsafe"
)

// A narrow timing owner contains no background goroutine or pooled payload.
func newReceiverTimingSendTestSequence() (*SendSequence, *sendItem) {
	at := time.Now()
	sequence := &SendSequence{
		client:      &Client{feedbackTimeBase: at},
		resendQueue: newResendQueue(nil, 0),
		rttWindow:   NewRttWindow(nil, 4, time.Second, 2, 2*time.Second, 300*time.Millisecond, 8*time.Second),
	}
	item := &sendItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: 1}, sendTime: at, sendCount: 1, expectsAck: true}
	sequence.resendQueue.Add(item)
	sequence.beginReceiverRttWrite(item, false)
	return sequence, item
}

// Delay changes neither raw mean, raw minimum nor either recovery timer.
// Actual receiver waits still occupy the effective window residence.
func TestRttReceiverTimingKeepsRawRecoveryAndPairedWindowResidence(t *testing.T) {
	base := time.Unix(1700000000, 0)
	timed := NewRttWindow(nil, 4, time.Second, 2, 2*time.Second, 300*time.Millisecond, 8*time.Second)
	plain := NewRttWindow(nil, 4, time.Second, 2, 2*time.Second, 300*time.Millisecond, 8*time.Second)
	for i, raw := range []time.Duration{30 * time.Millisecond, 40 * time.Millisecond} {
		at := base.Add(time.Duration(i) * time.Millisecond)
		timed.observeReceiverRoundTrip(raw, raw-5*time.Millisecond, 10000, at)
		plain.closeSendTime(uint64(at.Add(-raw).UnixMilli()), at)
		got, want := timed.estimate(at), plain.estimate(at)
		if got.Mean != want.Mean || got.Min != want.Min || timed.scaledRtt(at) != plain.scaledRtt(at) || timed.deviationRtt(at) != plain.deviationRtt(at) {
			t.Fatal("receiver delay changed raw RTT or recovery")
		}
	}
	adjusted, residence, set := timed.receiverWindowEstimate(base.Add(time.Millisecond))
	if !set || adjusted != 5*time.Millisecond || residence != 30*time.Millisecond {
		t.Fatalf("receiver queue vanished from residence: adjusted=%s residence=%s set=%t", adjusted, residence, set)
	}
	if got := unsafe.Sizeof(rttWindowItem{}); got != 40 {
		t.Fatalf("paired timing ring record size=%d want40", got)
	}
	if got := unsafe.Sizeof(sendItem{}); got != 584+retainedBudgetOwnerByteCount {
		t.Fatalf("timing duplicated the existing physical timestamp: sendItem=%d", got)
	}
	if got := unsafe.Sizeof(sequenceAck{}); got != 96 {
		t.Fatalf("timing grew per-ACK record: %d", got)
	}
}

// A later advertisement cannot reprice a historical timing tuple. Future
// reads/expiry do not retire records, and reads in the past ignore future ACKs.
func TestRttReceiverTimingPinsCompressionAndReadOnlySampleCutoffs(t *testing.T) {
	base := time.Unix(1700000000, 0)
	window := NewRttWindow(nil, 4, time.Second, 2, time.Second, time.Millisecond, 8*time.Second)
	window.observeReceiverRoundTrip(30*time.Millisecond, 25*time.Millisecond, 50000, base)
	adjusted, residence, set := window.receiverWindowEstimate(base)
	if !set || adjusted != 5*time.Millisecond || residence != 55*time.Millisecond {
		t.Fatal("pinned50ms compression not retained")
	}
	window.observeReceiverRoundTrip(40*time.Millisecond, 35*time.Millisecond, 0, base.Add(time.Millisecond))
	adjusted, residence, set = window.receiverWindowEstimate(base)
	if !set || adjusted != 5*time.Millisecond || residence != 55*time.Millisecond {
		t.Fatal("future zero-compression tuple repriced earlier evidence")
	}
	_, residence, set = window.receiverWindowEstimate(base.Add(time.Millisecond))
	if !set || residence != 40*time.Millisecond {
		t.Fatal("fresh tuple did not retain its own receiver residence")
	}
	beforeCount, beforeSequence := window.windowCount, window.nextSequence
	if _, _, set := window.receiverWindowEstimate(base.Add(2 * time.Second)); set {
		t.Fatal("expired sample remained available")
	}
	if _, _, set := window.receiverWindowEstimate(base.Add(-time.Nanosecond)); set {
		t.Fatal("future sample appeared in earlier snapshot")
	}
	if window.windowCount != beforeCount || window.nextSequence != beforeSequence {
		t.Fatal("read-only timing snapshot mutated retained history")
	}
}

// The existing sample capacity, not an expected-rate target or a new drain,
// replaces old short RTT after a real step beyond the old feedback ring.
func TestRttReceiverTimingRollingSamplesTrackLongRoundTripGrowth(t *testing.T) {
	base := time.Unix(1700000000, 0)
	window := NewRttWindow(nil, 4, time.Minute, 2, time.Second, time.Millisecond, 8*time.Second)
	window.observeReceiverRoundTrip(15*time.Millisecond, 10*time.Millisecond, 10000, base)
	for i := 1; i <= 4; i++ {
		window.observeReceiverRoundTrip(1210*time.Millisecond, 10*time.Millisecond, 10000, base.Add(time.Duration(i)*time.Second))
	}
	adjusted, residence, set := window.receiverWindowEstimate(base.Add(4 * time.Second))
	if !set || adjusted != 1200*time.Millisecond || residence != 1210*time.Millisecond {
		t.Fatalf("old short sample survived capacity replacement: adjusted=%s residence=%s set=%t", adjusted, residence, set)
	}
	count := window.windowCount
	window.observeReceiverRoundTrip(time.Millisecond, 0, 0, base.Add(2*time.Second))
	if got, _, _ := window.receiverWindowEstimate(base.Add(4 * time.Second)); got != 1200*time.Millisecond || window.windowCount != count {
		t.Fatal("late old ACK rewound measured RTT")
	}
	for i := 5; i <= 8; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		window.closeSendTime(uint64(at.Add(-time.Millisecond).UnixMilli()), at)
	}
	if _, _, set := window.receiverWindowEstimate(base.Add(8 * time.Second)); set {
		t.Fatal("legacy replacement retained unsupported old timing indefinitely")
	}
}

// Every invalid tuple leaves the live first-write record eligible for the
// later exact reply; identities cannot poison another message's measurement.
func TestSenderReceiverTimingRejectsInvalidTupleWithoutConsumingWrite(t *testing.T) {
	for _, kind := range []string{"missing", "message", "tag", "negative", "excess-delay", "contract"} {
		synctest.Test(t, func(t *testing.T) {
			sequence, item := newReceiverTimingSendTestSequence()
			sequence.finishReceiverRttWrite(item, transferWriteDisposition{transportType: TransportTypeH1, reliable: true}, nil)
			time.Sleep(10 * time.Millisecond)
			valid := receiveAckMessage{messageId: item.messageId, tag: sequenceTag{sendTime: uint64(item.sendTime.UnixMilli()), set: true}, receivedAtNanos: sequence.client.feedbackArrivalNanos(time.Now()), receiverAckDelaySet: true, receiverAckDelayMicros: 3000, ackCompressTimeoutSet: true, ackCompressTimeoutMicros: 10000}
			bad := valid
			switch kind {
			case "missing":
				bad.receiverAckDelaySet = false
			case "message":
				bad.messageId = NewId()
			case "tag":
				bad.tag.sendTime++
			case "negative":
				bad.receivedAtNanos = item.pacingSentAtNanos - 1
			case "excess-delay":
				bad.receiverAckDelayMicros = 10001
			case "contract":
				bad.contractMissing = true
			}
			sequence.observeReceiverAckRtt(bad)
			if got := sequence.rttWindow.Estimate(); got.SampleCount != 0 {
				t.Fatalf("%s tuple fabricated RTT: %+v", kind, got)
			}
			sequence.observeReceiverAckRtt(valid)
			got := sequence.rttWindow.Estimate()
			adjusted, _, set := sequence.rttWindow.receiverWindowEstimate(time.Now())
			if got.SampleCount != 1 || got.Mean != 10*time.Millisecond || !set || adjusted != 7*time.Millisecond {
				t.Fatalf("%s tuple consumed the later valid timing: %+v", kind, got)
			}
			sequence.observeReceiverAckRtt(valid)
			if got := sequence.rttWindow.Estimate(); got.SampleCount != 1 {
				t.Fatal("duplicate timed reply counted twice")
			}
		})
	}
}

// A retry or unreliable physical confirmation invalidates timing before any
// new ambiguous reply. Different message/lane state remains independent.
func TestSenderReceiverTimingInvalidatesRetryAndUnreliableCopies(t *testing.T) {
	for _, kind := range []string{"retry", "unreliable"} {
		synctest.Test(t, func(t *testing.T) {
			sequence, item := newReceiverTimingSendTestSequence()
			if kind == "retry" {
				sequence.finishReceiverRttWrite(item, transferWriteDisposition{transportType: TransportTypeH1, reliable: true}, nil)
				sequence.invalidateReceiverRttWrite(item)
				sequence.beginReceiverRttWrite(item, true)
				sequence.finishReceiverRttWrite(item, transferWriteDisposition{transportType: TransportTypeH3, reliable: true}, nil)
			} else {
				sequence.finishReceiverRttWrite(item, transferWriteDisposition{transportType: TransportTypeH3, unreliable: true}, nil)
			}
			time.Sleep(10 * time.Millisecond)
			sequence.observeReceiverAckRtt(receiveAckMessage{messageId: item.messageId, tag: sequenceTag{sendTime: uint64(item.sendTime.UnixMilli()), set: true}, receivedAtNanos: sequence.client.feedbackArrivalNanos(time.Now()), receiverAckDelaySet: true})
			if got := sequence.rttWindow.Estimate(); got.SampleCount != 0 {
				t.Fatalf("%s copy fabricated unambiguous RTT", kind)
			}
		})
	}
}

// A sibling's confirmed reply may arrive while a newer write is pending.
// Neither observation can consume or overwrite the other message's record.
func TestSenderReceiverTimingKeepsConfirmedAndPendingMessagesSeparate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sequence, first := newReceiverTimingSendTestSequence()
		sequence.finishReceiverRttWrite(first, transferWriteDisposition{transportType: TransportTypeH1, reliable: true}, nil)
		time.Sleep(time.Millisecond)
		second := &sendItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: 2}, sendTime: time.Now(), sendCount: 1, expectsAck: true}
		sequence.resendQueue.Add(second)
		sequence.beginReceiverRttWrite(second, false)
		time.Sleep(time.Millisecond)
		for _, item := range []*sendItem{second, first} {
			sequence.observeReceiverAckRtt(receiveAckMessage{messageId: item.messageId, tag: sequenceTag{sendTime: uint64(item.sendTime.UnixMilli()), set: true}, receivedAtNanos: sequence.client.feedbackArrivalNanos(time.Now()), receiverAckDelaySet: true})
		}
		if got := sequence.rttWindow.Estimate(); got.SampleCount != 1 || got.Mean != 2*time.Millisecond {
			t.Fatalf("pending newer write changed confirmed older sample: %+v", got)
		}
		sequence.finishReceiverRttWrite(second, transferWriteDisposition{transportType: TransportTypeH1, reliable: true}, nil)
		if got := sequence.rttWindow.Estimate(); got.SampleCount != 2 || got.Mean != 1500*time.Microsecond {
			t.Fatalf("older reply erased pending newer sample: %+v", got)
		}
	})
}

// Once exact timing owns one message, coalescing cannot resurrect its legacy
// tag path. The marker is never inherited by a different newer head.
func TestSenderReceiverTimingSuppressionSurvivesAckCoalescing(t *testing.T) {
	for _, kind := range []string{"selective", "head-from-selective", "duplicate-head", "different-head"} {
		window := newSequenceAckWindow()
		id := NewId()
		first := sequenceAck{messageId: id, sequenceNumber: 1, selective: kind == "selective" || kind == "head-from-selective", receiverTiming: true}
		window.Update(first)
		next := first
		next.receiverTiming = false
		if kind == "head-from-selective" {
			next.selective = false
		}
		if kind == "different-head" {
			next.messageId = NewId()
			next.sequenceNumber = 2
		}
		window.Update(next)
		snapshot := window.Snapshot(true)
		got := snapshot.headAck.receiverTiming
		if kind == "selective" {
			got = snapshot.selectiveAcks[id].receiverTiming
		}
		if got != (kind != "different-head") {
			t.Fatalf("%s coalescing changed exact-timing ownership", kind)
		}
	}
}
