package connect

import (
	"context"
	"math"
	"testing"
	"testing/synctest"
	"time"
)

// Accepts the shared wire buffer and records exactly when pacing released it.
type windowPacingClockWriter struct {
	windowPacingPolicyWriter
	writtenAt time.Time
}

func (self *windowPacingClockWriter) WriteDetailedWithTransport(_ context.Context, bytes []byte, _ time.Duration) (bool, TransportType, error) {
	self.writtenAt = time.Now()
	MessagePoolReturn(bytes)
	return true, TransportTypeH1, nil
}

// Local send pacing and delayed ack application are outside network service.
// Exercise the real write, handoff and acknowledgement application boundaries.
func TestWindowPacingRoundTripExcludesLocalWriteWait(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultSendBufferSettings()
		settings.DeliverySizedWindowScale = 2
		service := &windowPacingService{}
		writer := &windowPacingClockWriter{windowPacingPolicyWriter: windowPacingPolicyWriter{policy: transferFlightPolicySnapshot{h1Only: true}}}
		sequence := newEstimatorFixture(t, func(s *SendBufferSettings) { *s = *settings })
		sequence.ctx, sequence.client, sequence.log = context.Background(), &Client{}, NewNoopLogger()
		sequence.contractMultiRouteWriter = writer
		sequence.ackWindow = newSequenceAckWindow()
		sequence.flightController = newSendFlightController(settings)
		sequence.windowPacer = windowBurstPacer{service: service, rate: 1000000, rateUpdated: time.Now()}
		defer sequence.windowPacer.close()
		start := time.Now()
		item := &sendItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: 1}, sendTime: start, sendCount: 1, expectsAck: true, transferFrameBytes: MessagePoolGet(20000)}
		sequence.sendItems = append(sequence.sendItems, item)
		sequence.resendQueue.Add(item)
		defer func() {
			for _, pending := range sequence.resendQueue.Clear() {
				pending.messagePoolReturn()
			}
		}()
		if _, err := sequence.writeMaybeWrappedBytes(item.transferFrameBytes, TransferPath{}, true, item, false, false); err != nil {
			t.Fatal(err)
		}
		if writer.writtenAt.Sub(start) != 20*time.Millisecond {
			t.Fatal("the write did not cross the intended pacing wait")
		}
		time.Sleep(100 * time.Millisecond)
		arrival := time.Now()
		tag := sequenceTag{set: true, sendTime: uint64(start.UnixMilli())}
		sequence.coalesceReceivedAck(sequence.ackWindow, receiveAckMessage{messageId: item.messageId, receivedAtNanos: arrival.UnixNano(), tag: tag})
		time.Sleep(500 * time.Millisecond)
		ack := sequence.ackWindow.Snapshot(true).headAck
		sequence.receiveAckAt(ack.messageId, false, ack.tag, false, ack.receivedAtNanos)
		if service.minRoundTrip != 100*time.Millisecond {
			t.Fatalf("local send or receive wait entered service RTT: %s", service.minRoundTrip)
		}
	})
}

// An acknowledgement cannot identify which retransmitted or changed-carrier
// copy arrived. A direct datagram cannot establish relay residence either.
func TestWindowPacingAmbiguousDeliveryCannotSetRelayRoundTrip(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, test := range []struct {
		name       string
		sendCount  int
		changed    bool
		unreliable bool
	}{
		{name: "resent", sendCount: 2},
		{name: "changed", sendCount: 1, changed: true},
		{name: "unreliable", sendCount: 1, unreliable: true},
	} {
		synctest.Test(t, func(t *testing.T) {
			sequence := newEstimatorFixture(t, nil)
			sequence.client, sequence.log = &Client{}, NewNoopLogger()
			sequence.flightController = newSendFlightController(sequence.sendBufferSettings)
			sequence.ackWindow = newSequenceAckWindow()
			start := time.Now()
			service := &windowPacingService{}
			service.observeRoundTrip(100*time.Millisecond, 0, start)
			sequence.windowPacer.service = service
			item := &sendItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: 1},
				transferFrameBytes: MessagePoolGet(1280), pacingByteCount: 1280, pacingSentAtNanos: start.UnixNano(),
				sendCount: test.sendCount, carrierChanged: test.changed, unreliableCarrierObserved: test.unreliable}
			sequence.sendItems = append(sequence.sendItems, item)
			sequence.resendQueue.Add(item)
			time.Sleep(10 * time.Millisecond)
			tag := sequenceTag{set: true, sendTime: uint64(start.UnixMilli())}
			sequence.coalesceReceivedAck(sequence.ackWindow, receiveAckMessage{messageId: item.messageId, receivedAtNanos: time.Now().UnixNano(), tag: tag})
			ack := sequence.ackWindow.Snapshot(true).headAck
			sequence.receiveAckAt(ack.messageId, false, ack.tag, false, ack.receivedAtNanos)
			sequence.resendQueue.Clear()
			if service.minRoundTrip != 100*time.Millisecond {
				t.Fatalf("%s delivery changed relay residence to %s", test.name, service.minRoundTrip)
			}
		})
	}
}

// Missing local sizing evidence cannot override a limit already supplied by
// the peer. The constant fallback is a ceiling, including before RTT samples.
func TestWindowMismatchUnbudgetedSenderRespectsKnownLimits(t *testing.T) {
	for _, configured := range []ByteCount{0, kib(32)} {
		sequence := newEstimatorFixture(t, func(settings *SendBufferSettings) {
			settings.DeliverySizedWindowScale = 2
			settings.ResendQueueBudget = nil
			settings.DeliverySizedWindowCeilingByteCount = configured
		})
		sequence.observeReceiveWindowAdvertisement(receiveAckMessage{
			receiveWindowSet: true, receiveWindowByteCount: uint32(kib(64)),
		})
		want := kib(64)
		if configured > 0 {
			want = min(want, configured)
		}
		for phase := range 2 {
			if phase != 0 {
				sampleRoundTrip(sequence, 100*time.Millisecond)
			}
			estimate := sequence.sendWindowEstimate(time.Now())
			if estimate.Window != want || estimate.Floor > want {
				t.Errorf("configured=%d phase=%d: window=%d floor=%d, want limit %d", configured, phase, estimate.Window, estimate.Floor, want)
			}
		}
	}
}

// An explicit zero hold permits only the queue's existing single-item
// progress allowance. Completing it permits the next capacity probe.
func TestWindowMismatchZeroHoldKeepsOneItemProgress(t *testing.T) {
	sequence := newEstimatorFixture(t, func(settings *SendBufferSettings) {
		settings.DeliverySizedWindowScale = 2
		settings.ResendQueueBudget = NewTransferMemoryBudget(mib(48))
	})
	defer sequence.resendQueue.Clear()
	sequence.observeReceiveWindowAdvertisement(receiveAckMessage{receiveWindowSet: true})
	limit := sequence.sendWindowEstimate(time.Now()).Window
	if !sequence.resendQueue.CanAdd(0, limit) {
		t.Fatal("zero peer hold prevented an in-order progress probe")
	}
	item := &sendItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: 1}}
	sequence.resendQueue.Add(item)
	if sequence.resendQueue.CanAdd(0, limit) {
		t.Fatal("zero peer hold allowed a second unacknowledged message")
	}
	sequence.resendQueue.RemoveByMessageId(item.messageId)
	if !sequence.resendQueue.CanAdd(0, limit) {
		t.Fatal("completed progress probe did not reopen admission")
	}
}

// Both the target rate and its residence product saturate before conversion;
// a large positive target cannot turn into a negative limit or disable pacing.
func TestWindowMismatchLargeTargetCannotWrapItsBounds(t *testing.T) {
	sequence := newEstimatorFixture(t, func(settings *SendBufferSettings) {
		settings.DeliverySizedWindowScale = 2
		settings.ResendQueueBudget = NewTransferMemoryBudget(mib(48))
		settings.TargetGoodputByteRate = math.MaxInt64
	})
	t.Cleanup(func() { sequence.resendQueue.Clear() })
	at := time.Unix(1700000000, 0)
	sequence.rttWindow.closeSendTime(uint64(at.Add(-time.Second).UnixMilli()), at)
	sequence.observeReceiveWindowAdvertisement(receiveAckMessage{
		receiveWindowSet: true, receiveWindowByteCount: uint32(mib(48)),
	})
	sequence.receiveWindowSetAtNanos.Store(at.UnixNano())
	// Earn a full memory-share candidate; permission alone no longer grows
	// the opening. A wrapped target product would now suppress this growth.
	for range 64 {
		at = at.Add(50 * time.Millisecond)
		sequence.observeDeliveredBytes(4*1024*1024, at)
	}
	estimate := sequence.sendWindowEstimate(at)
	if !estimate.Sized || estimate.Window != mib(48) || estimate.PacingByteRate <= 0 || estimate.PacingProbeByteRate != math.MaxInt64 {
		t.Fatalf("positive target overflowed its bounds: %+v", estimate)
	}
}

// Contract lead time consumes the window owner's byte/time evidence too.
// A valid long compression interval must not overflow this sibling product.
func TestWindowDeliveryContractLeadDoesNotOverflow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sequence := newEstimatorFixture(t, func(settings *SendBufferSettings) {
			settings.DeliverySizedWindowScale = 2
			settings.TargetGoodputByteRate = 0
			settings.ResendQueueBudget = NewTransferMemoryBudget(mib(64))
			settings.ContractAheadScale = 2
			settings.ContractAheadFloorByteCount = 0
		})
		sequence.observeReceiveWindowAdvertisement(receiveAckMessage{
			receiveWindowSet: true, receiveWindowByteCount: uint32(mib(32)),
			ackCompressTimeoutSet: true, ackCompressTimeoutMicros: 4000000000,
		})
		now := time.Now()
		span := 16004 * time.Second
		start := now.Add(-span)
		sequence.receiveWindowSetAtNanos.Store(start.UnixNano())
		sequence.observeDeliveredBytes(1, start)
		sequence.observeDeliveredBytes(16004*mib(1), now)
		sequence.rttWindow.closeSendTime(uint64(now.Add(-time.Second).UnixMilli()), now)
		// Exactly 1 MiB/s, one second of path RTT, and scale two.
		if got := sequence.announceAheadByteCount(); got != mib(2) {
			t.Fatalf("contract lead overflowed byte/time multiplication: %d, want %d", got, mib(2))
		}
	})
}

// The standalone history used by estimator fixtures has the same feedback
// horizon contract as managed shared services, including after an idle gap.
func TestWindowPacingLocalHistoryRetainsStableFeedbackSpan(t *testing.T) {
	sequence := newEstimatorFixture(t, func(settings *SendBufferSettings) {
		settings.DeliverySizedWindowScale = 2
	})
	sampleRoundTrip(sequence, 400*time.Millisecond)
	start := time.Now()
	for i := range 26 {
		bytes := ByteCount(200000)
		if i < 3 {
			bytes = 2500000
		}
		at := start.Add(time.Duration(i) * 20 * time.Millisecond)
		sequence.observeDeliveredBytes(bytes, at)
		if i == 15 {
			if rate, _, _ := sequence.deliveredServiceRate(time.Second, at); rate != 125000000 {
				t.Fatalf("standalone history forgot service before feedback: %d B/s", rate)
			}
		}
	}
	if rate, _, _ := sequence.deliveredServiceRate(10*time.Second, start.Add(700*time.Millisecond)); rate != 10000000 {
		t.Fatalf("standalone history retained expired fast service: %d B/s", rate)
	}
}

// Capacity changes bound admission independently of learned capacity. An
// increase needs fresh cumulative history; measured service stays independent.
func TestWindowMismatchCapacityIncreaseStartsFreshDeliveryEvidence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sequence := newEstimatorFixture(t, func(settings *SendBufferSettings) {
			settings.DeliverySizedWindowScale = 2
			settings.ResendQueueBudget = NewTransferMemoryBudget(mib(48))
			settings.TargetGoodputByteRate = 0
		})
		sampleRoundTrip(sequence, 100*time.Millisecond)
		advertise := func(window ByteCount) {
			sequence.observeReceiveWindowAdvertisement(receiveAckMessage{
				receiveWindowSet: true, receiveWindowByteCount: uint32(window),
				ackCompressTimeoutSet: true,
			})
		}
		advertise(kib(64))
		opening := sequence.sendWindowEstimate(time.Now())
		if opening.Window != kib(64) || opening.LearnedWindow != opening.Initial {
			t.Fatalf("small peer capacity changed the configured bootstrap: %+v", opening)
		}
		for range 40 {
			sequence.observeDeliveredBytes(kib(4), time.Now())
			time.Sleep(10 * time.Millisecond)
		}
		advertise(mib(2))
		step := sequence.receiveWindowSetAtNanos.Load()
		if estimate := sequence.sendWindowEstimate(time.Now()); estimate.Sized || !estimate.ServiceSized || estimate.Interval < 200*time.Millisecond || estimate.DeliveredByteCount == 0 || estimate.Window != opening.LearnedWindow || estimate.LearnedWindow != opening.LearnedWindow || estimate.Ceiling != mib(2) {
			t.Fatalf("old constrained delivery qualified after a capacity increase: %+v", estimate)
		}
		for range 40 {
			advertise(mib(2))
			sequence.observeDeliveredBytes(kib(4), time.Now())
			time.Sleep(10 * time.Millisecond)
		}
		if estimate := sequence.sendWindowEstimate(time.Now()); !estimate.Sized || estimate.CandidateWindow != estimate.Floor || estimate.Window != opening.LearnedWindow || estimate.LearnedWindow != opening.LearnedWindow || sequence.receiveWindowSetAtNanos.Load() != step {
			t.Fatalf("unchanged advertisements lost fresh delivery or reduced learned capacity: %+v", estimate)
		}
		advertise(kib(32))
		if estimate := sequence.sendWindowEstimate(time.Now()); estimate.Window != kib(32) || estimate.Ceiling != kib(32) || estimate.LearnedWindow != opening.LearnedWindow || sequence.receiveWindowSetAtNanos.Load() != step {
			t.Fatalf("capacity decrease failed to clamp admission while retaining learned capacity: %+v", estimate)
		}
	})
}

// Idle time at the start of an ACK interval cannot become delivered bytes.
// Compare a short full-rate train with a sustained train at that same rate.
func TestWindowPacingQueueEstimateDoesNotIncludeWindowLimitedIdle(t *testing.T) {
	start := time.Unix(1700000000, 0)
	service := &windowPacingService{}
	service.observeRoundTrip(400*time.Millisecond, 0, start)
	service.observe(1250, start)
	// The sender was window-limited for 300 ms, then serialized for 100 ms.
	for i := 300; i <= 400; i++ {
		service.observe(125000, start.Add(time.Duration(i)*time.Millisecond))
	}
	now := start.Add(400 * time.Millisecond)
	service.sent = service.total + 51000000
	service.observeRoundTrip(403*time.Millisecond, 0, now)
	if rate, _, _ := service.measured(time.Second, now); rate < 118750000 {
		t.Fatalf("window idle reduced a full-rate service train to %d B/s", rate)
	}
}
