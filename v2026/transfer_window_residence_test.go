// Window regression tests use exact byte checkpoints and virtual time. The
// observations are independent of the estimator's diagnostic arithmetic.
package connect

import (
	"math"
	"testing"
	"testing/synctest"
	"time"
)

func TestAckCompressionAdvertisementPreservesPresence(t *testing.T) {
	sequence := newEstimatorFixture(t, nil)
	if sequence.ackCompressionResidence() != 10*time.Millisecond {
		t.Fatal("legacy fallback changed")
	}
	for _, micros := range []uint32{0, 10000, math.MaxUint32} {
		ack := sendAckFrame{messageId: NewId(), sequenceId: NewId(), ackCompressTimeoutSet: true, ackCompressTimeoutMicros: micros}
		assertAckCodecMatches(t, &ack)
		encoded := marshalSendAckTransferFrame(&ack)
		decoded := inboundDecodedTransferFrames.take()
		if !unmarshalOwnedTransferFrame(encoded, decoded, true) {
			t.Fatal("owned codec rejected compression")
		}
		value, err := receiveAckMessageFromProtocol(decoded.frame.Ack)
		inboundDecodedTransferFrames.put(decoded)
		MessagePoolReturn(encoded)
		if err != nil {
			t.Fatal(err)
		}
		sequence.observeReceiveWindowAdvertisement(value)
		sequence.observeReceiveWindowAdvertisement(receiveAckMessage{})
		if sequence.ackCompressionResidence() != time.Duration(micros)*time.Microsecond {
			t.Fatalf("absent advertisement erased %d microseconds", micros)
		}
	}
}

// A saturated sender needs room for bytes awaiting the receiver's compressed
// acknowledgement, even when its fastest tag contains no compression delay.
func TestWindowTargetIncludesAcknowledgementResidence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sequence := newEstimatorFixture(t, func(settings *SendBufferSettings) {
			settings.WindowSizing = WindowSizingFromDelivery
			settings.ApplyWindowSizing()
			settings.ResendQueueBudget = NewTransferMemoryBudget(mib(64))
		})
		sequence.observeReceiveWindowAdvertisement(receiveAckMessage{
			receiveWindowSet: true, receiveWindowByteCount: uint32(mib(32)),
		})
		sequence.rttWindow.closeSendTime(uint64(time.Now().Add(-time.Millisecond).UnixMilli()), time.Now())
		estimate := sequence.sendWindowEstimate(time.Now())
		// The legacy receiver's 10 ms compression plus a 1 ms round trip
		// needs at least 1.375 MB of goodput capacity at the 1 Gb/s target.
		if estimate.Window < 1_375_000 {
			t.Fatalf("target cannot carry one compressed acknowledgement cycle: %+v", estimate)
		}
		if estimate.RoundTrip != time.Millisecond {
			t.Fatalf("compression changed the observed path RTT: %+v", estimate)
		}
	})
}

// Feedback on a 1 ms path must not shrink a full 10 ms acknowledgement burst
// to its floor. Queue inflation is deliberately absent from the minimum RTT.
func TestWindowDeliveryIncludesAcknowledgementResidence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sequence := newEstimatorFixture(t, func(settings *SendBufferSettings) {
			settings.DeliverySizedWindowScale = 2
			settings.TargetGoodputByteRate = 0
			settings.ResendQueueBudget = NewTransferMemoryBudget(mib(64))
		})
		sequence.observeReceiveWindowAdvertisement(receiveAckMessage{
			receiveWindowSet: true, receiveWindowByteCount: uint32(mib(32)),
		})
		sequence.rttWindow.closeSendTime(uint64(time.Now().Add(-time.Millisecond).UnixMilli()), time.Now())
		for range 12 {
			sequence.observeDeliveredBytes(mib(1), time.Now())
			time.Sleep(10 * time.Millisecond)
		}
		estimate := sequence.sendWindowEstimate(time.Now())
		if !estimate.Sized || estimate.Window < 2*mib(1) {
			t.Fatalf("feedback collapses below an independently counted ACK burst: %+v", estimate)
		}
	})
}

// Coalescing may not attach bytes received at 19 ms to a 10 ms checkpoint;
// that would double the reported rate without any change in actual delivery.
func TestDeliveryRateUsesCheckpointTimes(t *testing.T) {
	sequence := newEstimatorFixture(t, func(settings *SendBufferSettings) {
		settings.DeliverySizedWindowScale = 2
	})
	start := time.Now()
	sequence.observeDeliveredBytes(1000, start)
	sequence.observeDeliveredBytes(1000, start.Add(10*time.Millisecond))
	sequence.observeDeliveredBytes(9000, start.Add(19*time.Millisecond))
	bytes, span, _, ok := sequence.deliveredRate(10 * time.Millisecond)
	if !ok || bytes != 1000 || span != 10*time.Millisecond {
		t.Fatalf("checkpoint includes future bytes: bytes=%d span=%s sampled=%t", bytes, span, ok)
	}
}

func TestDeliveryRateWaitsForWholeHorizon(t *testing.T) {
	sequence := newEstimatorFixture(t, func(settings *SendBufferSettings) {
		settings.DeliverySizedWindowScale = 2
	})
	start := time.Now()
	for i := range 4 {
		sequence.observeDeliveredBytes(1000, start.Add(time.Duration(i)*10*time.Millisecond))
	}
	if _, span, _, ok := sequence.deliveredRate(100 * time.Millisecond); ok {
		t.Fatalf("accepted %s of evidence for a 100 ms horizon", span)
	}
}

// Dense 10 ms acknowledgements must still retain two design-point round trips
// without allocating a history proportional to the number of packets.
func TestDeliveryRateHistoryCoversDesignRoundTrip(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sequence := newEstimatorFixture(t, func(settings *SendBufferSettings) {
			settings.DeliverySizedWindowScale = 2
		})
		sequence.rttWindow.closeSendTime(uint64(time.Now().Add(-400*time.Millisecond).UnixMilli()), time.Now())
		for range 160 {
			sequence.observeDeliveredBytes(1000, time.Now())
			time.Sleep(10 * time.Millisecond)
		}
		bytes, span, _, ok := sequence.deliveredRate(800 * time.Millisecond)
		if !ok || span < 800*time.Millisecond || bytes != ByteCount(span/(10*time.Millisecond))*1000 {
			t.Fatalf("lost the design-point history: bytes=%d span=%s sampled=%t", bytes, span, ok)
		}
	})
}

// An untrusted but wire-valid compression value can make the byte-time
// product exceed int64 even when its final window fits comfortably in memory.
func TestWindowDeliveryLargeResidenceDoesNotOverflow(t *testing.T) {
	sequence := newEstimatorFixture(t, func(settings *SendBufferSettings) {
		settings.DeliverySizedWindowScale = 2
		settings.TargetGoodputByteRate = 0
		settings.ResendQueueBudget = NewTransferMemoryBudget(mib(64))
	})
	sequence.observeReceiveWindowAdvertisement(receiveAckMessage{
		receiveWindowSet: true, receiveWindowByteCount: uint32(mib(32)),
		ackCompressTimeoutSet: true, ackCompressTimeoutMicros: math.MaxUint32,
	})
	now := time.Now()
	residence := time.Duration(math.MaxUint32)*time.Microsecond + time.Millisecond
	start := now.Add(-4 * residence)
	sequence.receiveWindowSetAtNanos.Store(start.UnixNano())
	sequence.observeDeliveredBytes(mib(16), start)
	sequence.observeDeliveredBytes(mib(16), now)
	sequence.rttWindow.closeSendTime(uint64(now.Add(-time.Millisecond).UnixMilli()), now)
	estimate := sequence.sendWindowEstimate(now)
	// 16 MiB delivered in four residences, scaled by two, is 8 MiB.
	if !estimate.Sized || estimate.Window < mib(8)-kib(1) || estimate.Window > mib(8)+kib(1) {
		t.Fatalf("large receiver delay wrapped delivery arithmetic: %+v", estimate)
	}
}
