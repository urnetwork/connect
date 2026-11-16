// Exact physical retries and ACK publication expose short-gap supply authority.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

// The clock is owned by synctest. Initial service uses confirmed H1 offers and
// real cumulative ACK publication; a retained tail prevents a drained restart.
func newWindowRecoveryServiceFixture(t *testing.T) (*windowReceiverCreditFixture, *fifoRepairCopyWriter, time.Time) {
	t.Helper()
	start := time.Now()
	fixture := newWindowReceiverCreditFixture(t, nil, start, 10*time.Millisecond)
	sequence := fixture.sequence
	ctx, cancel := context.WithCancel(context.Background())
	sequence.ctx, sequence.cancel, sequence.log = ctx, cancel, NewNoopLogger()
	sequence.sendBufferSettings = DefaultSendBufferSettings()
	sequence.flightController = newSendFlightController(sequence.sendBufferSettings)
	sequence.deliveredBytes = make([]deliveredBytesSample, deliveredBytesRingSize)
	sequence.client.ctx, sequence.client.clientId, sequence.client.log = ctx, NewId(), sequence.log
	sequence.destination = NewId()
	sequence.ackWindow = fixture.ackWindow
	writer := &fifoRepairCopyWriter{route: make(Route, 1),
		windowPacingPolicyWriter: windowPacingPolicyWriter{policy: transferFlightPolicySnapshot{generation: 2, h1Only: true}}}
	sequence.contractMultiRouteWriter = writer
	sequence.contractMultiRouteWriterDestination = DestinationId(sequence.destination)
	var opening [9]*sendItem
	for number := range opening {
		opening[number] = fixture.write(uint64(number), 1000, start)
	}
	fixture.write(9, 3445, start)
	for number, item := range opening {
		at := start.Add(time.Millisecond + time.Duration(number)*10*time.Millisecond)
		time.Sleep(time.Until(at))
		sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(item, at, 0))
	}
	at := time.Now()
	before := sequence.sendWindowEstimate(at)
	if fixture.service.serviceHoldRate != 100000 || before.ServiceByteRate != 100000 || before.PacingByteRate <= 0 || fixture.service.drained {
		t.Fatalf("confirmed opening did not establish service: estimate=%+v hold=%d drained=%t", before, fixture.service.serviceHoldRate, fixture.service.drained)
	}
	t.Cleanup(func() {
		cancel()
		sequence.windowPacer.close()
		sequence.ackLifetimes.clear()
		for _, item := range sequence.resendQueue.Clear() {
			item.messagePoolReturn()
		}
		if fixture.service.sent != fixture.service.total || fixture.service.pendingWrites != 0 || fixture.service.reservedByteCount != 0 {
			t.Error("closing the actual owner retained physical credit")
		}
	})
	return fixture, writer, at
}

// The shared recovery entry consumes a real wire share, invalidates timing,
// and increments ordinary attempts. No supply or RTT flags are assigned here.
func writeWindowRecoveryServiceCopy(t *testing.T, fixture *windowReceiverCreditFixture, writer *fifoRepairCopyWriter, item *sendItem) {
	t.Helper()
	sequence := fixture.sequence
	item.ackTimeout = time.Minute
	item.transferFrameBytes = marshalSendPackTransferFrame(&sendPackFrame{
		path:      sendTransferPath(sequence.client.ClientId(), DestinationId(sequence.destination)),
		messageId: item.messageId, sequenceId: sequence.sequenceId, sequenceNumber: item.sequenceNumber,
	})
	sequence.ackLifetimes.update(item)
	before := sequence.resendWriteCount.Load()
	fixture.service.beginWrite(sequence.sequenceId, item.messageId, item.sequenceNumber, time.Now(), true)
	// The fixture owns the physical clock. Suppress only this wire call's
	// timer wait, then restore ordinary policy before ACKs or estimates.
	err := func() error {
		scale := sequence.sendBufferSettings.DeliverySizedWindowScale
		sequence.sendBufferSettings.DeliverySizedWindowScale = 0
		defer func() { sequence.sendBufferSettings.DeliverySizedWindowScale = scale }()
		return sequence.writeSelectedRecovery(item, sendRecoverySelection{kind: sendRecoveryNone})
	}()
	if err != nil {
		t.Fatal(err)
	}
	fixture.service.finishWrite(sequence.sequenceId, item.messageId, true)
	if len(writer.route) != 1 {
		t.Fatal("the actual H1 writer did not consume the retry")
	}
	wire := <-writer.route
	pack := decodeSendPackLifecycleWirePack(t, wire)
	MessagePoolReturn(wire)
	if string(pack.MessageId) != string(item.messageId.Bytes()) || sequence.resendWriteCount.Load() != before+1 || item.sendCount != 2 || item.rttState != sendItemRttUnavailable || !item.rttH1 {
		t.Fatal("successful ordinary retry did not preserve identity and invalidate initial timing")
	}
}

// The same 154-byte right credit follows two explicit physical boundaries.
// A retry must not gain serialization authority just because its replies are
// closer than two sampler intervals; all real delivery remains exact once.
func TestWindowServiceRecoveryShortRawRetryCannotReprice(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture, writer, _ := newWindowRecoveryServiceFixture(t)
		sequence, service := fixture.sequence, fixture.service
		oldRate, oldTotal := service.serviceHoldRate, service.total
		left := fixture.write(10, 2315, time.Now())
		right := fixture.write(11, 154, time.Now())
		fixture.write(12, 3445, time.Now())
		time.Sleep(22 * time.Second)
		// The existing generic recovery release stays unchanged. This probe
		// asks the independent estimator whether ambiguous bytes can reprice it.
		waiter := &windowPacingWaiter{}
		service.reserve(time.Now(), 154, 110000, oldRate, 0, 0, true, waiter)
		releaseOpeningCreditReservation(service, waiter, 154, true)
		if service.heldPacingRate != 0 {
			t.Fatal("generic recovery failed to release the old pacing hold")
		}
		writeWindowRecoveryServiceCopy(t, fixture, writer, left)
		time.Sleep(time.Millisecond)
		leftAt := time.Now()
		leftAck := fixture.ack(left, leftAt, 0)
		leftAck.selective = true
		sequence.coalesceReceivedAck(fixture.ackWindow, leftAck)
		time.Sleep(16298750*time.Nanosecond - time.Millisecond)
		writeWindowRecoveryServiceCopy(t, fixture, writer, right)
		time.Sleep(time.Millisecond)
		rightAt := time.Now()
		rightAck := fixture.ack(right, rightAt, 0)
		rightAck.selective = true
		sequence.coalesceReceivedAck(fixture.ackWindow, rightAck)
		sequence.coalesceReceivedAck(fixture.ackWindow, rightAck)
		if rightAt.Sub(leftAt) != 16298750*time.Nanosecond || service.total-oldTotal != 2469 || sequence.windowPacer.serviceAcked != service.total || service.drained || service.lastRoundTrip.After(service.lastRecoveryWriteAt) {
			t.Fatal("exact raw cadence, once-only credit, retained flight or ambiguous timing premise changed")
		}
		var newest windowServiceSample
		for _, sample := range service.samples {
			if sample.lastAtNanos == rightAt.UnixNano() {
				newest = sample
			}
		}
		if newest.bytes != 154 || !newest.supply.recorded || newest.supply.complete || !newest.supply.rawClock || newest.supply.headComplete || newest.receiverBytes != 0 {
			t.Fatalf("real retry credit did not carry the observed incomplete raw provenance: %+v", newest)
		}
		after := sequence.sendWindowEstimate(rightAt)
		if service.serviceHoldRate != oldRate || after.ServiceByteRate < oldRate || after.PacingByteRate < oldRate {
			t.Errorf("incomplete short recovery pair repriced established service: prior=%d hold=%d service=%d pace=%d exact raw pair=%d", oldRate, service.serviceHoldRate, after.ServiceByteRate, after.PacingByteRate, ByteCount(154*int64(time.Second)/16298750))
		}
	})
}

// A confirmed original at the same cadence is legitimate continuous evidence.
func TestWindowServiceRecoveryShortConfirmedPairRemainsQualified(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture, _, _ := newWindowRecoveryServiceFixture(t)
		sequence, service := fixture.sequence, fixture.service
		left := fixture.write(10, 2315, time.Now())
		right := fixture.write(11, 154, time.Now())
		fixture.write(12, 3445, time.Now())
		time.Sleep(22 * time.Second)
		leftAt := time.Now()
		for _, point := range []struct {
			item *sendItem
			at   time.Time
		}{
			{item: left, at: leftAt}, {item: right, at: leftAt.Add(16298750 * time.Nanosecond)},
		} {
			time.Sleep(time.Until(point.at))
			ack := fixture.ack(point.item, point.at, 0)
			ack.selective, ack.receiverAckDelaySet = true, false
			sequence.coalesceReceivedAck(fixture.ackWindow, ack)
		}
		if rate, _, latest := service.measured(time.Second, time.Now()); rate != 9448 || latest != 9448 || service.serviceHoldRate != 9448 {
			t.Fatalf("confirmed continuous original pair lost qualification: %d/%d hold=%d", rate, latest, service.serviceHoldRate)
		}
	})
}

// Existing real receiver-timed queue evidence must retain its decrease scope.
func TestWindowServiceRecoveryQueuedOriginalDecreaseRemainsQualified(t *testing.T) {
	TestWindowPacingHeldDecreaseRequiresIndependentQueue(t)
}

// Legacy observations without a physical-offer record keep their raw semantics.
func TestWindowServiceRecoveryLegacyContinuousPairRemainsQualified(t *testing.T) {
	TestWindowPacingLimitedFlightAcceptsContinuousSlowPair(t)
	TestWindowServiceSupplyLegacyPreofferedSlowPair(t)
}
