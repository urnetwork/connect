// Sparse compressed heads must not hide a continuously offered physical train.
// Every byte below belongs to a real retained original and a cumulative ACK.
package connect

import (
	"testing"
	"time"
)

// The first six heads establish the observed 106840-byte/s lower bound and
// end discovery through actual queue timing. Later offers are continuous,
// although their raw ACK checkpoints remain two sampler turns apart.
func windowContinuousOfferFixture(t *testing.T, gap time.Duration, unknown bool) (*windowReceiverCreditFixture, time.Time) {
	t.Helper()
	const frameBytes ByteCount = 2671
	const offerInterval = 22727273 * time.Nanosecond
	const residence = 421368 * time.Microsecond
	start := time.Unix(1700000000, 0)
	fixture := newWindowReceiverCreditFixture(t, nil, start, 50*time.Millisecond)
	sequence, service := fixture.sequence, fixture.service
	sequence.sendBufferSettings = DefaultSendBufferSettings()
	sequence.deliveredBytes = make([]deliveredBytesSample, deliveredBytesRingSize)
	sequence.contractMultiRouteWriter = &windowPacingPolicyWriter{policy: transferFlightPolicySnapshot{h1Only: true}}
	sequence.observeReceiveWindowAdvertisement(receiveAckMessage{receiveWindowSet: true, receiveWindowByteCount: 2 * 1024 * 1024})
	t.Cleanup(func() {
		sequence.windowPacer.close()
		sequence.resendQueue.Clear()
		if service.sent != service.total || service.pendingWrites != 0 || service.pacingReservations != 0 || service.reservedByteCount != 0 {
			t.Error("closing the exact physical owner retained flight or reservation ownership")
		}
	})
	var opening [12]*sendItem
	for number := range opening {
		opening[number] = fixture.write(uint64(number), frameBytes, start)
	}
	var offered [40]*sendItem
	count := 0
	offerThrough := func(at time.Time) {
		for count < len(offered) {
			sentAt := start.Add(300*time.Millisecond + time.Duration(count)*offerInterval)
			if count >= 11 {
				sentAt = sentAt.Add(gap)
			}
			if sentAt.After(at) {
				return
			}
			if unknown && count == 12 {
				offered[count] = fixture.offer(uint64(12+count), frameBytes, sentAt)
			} else {
				offered[count] = fixture.write(uint64(12+count), frameBytes, sentAt)
			}
			count++
		}
		t.Fatal("the bounded offer table was exhausted")
	}
	at := start
	for turn := 0; turn < 6; turn++ {
		at = start.Add(residence + time.Duration(turn)*50*time.Millisecond)
		offerThrough(at)
		wait := time.Duration(turn) * 7264 * time.Microsecond
		sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(opening[2*turn], at, wait))
		fixture.measured(at)
	}
	before := sequence.sendWindowEstimate(at)
	if before.ServiceByteRate != 106840 || before.PacingByteRate != 117524 || before.PacingDiscovery || before.ServiceBacklogged ||
		service.serviceHoldRate != 106840 || service.heldPacingRate != 117524 || service.queueObservedAt.IsZero() ||
		service.total != 11*frameBytes || service.drained || service.feedbackPending || opening[11].serviceCreditObserved {
		t.Fatalf("real compressed credit did not establish the retained operating point: %+v total=%d held=%d discoveryAt=%s", before, service.total, service.heldPacingRate, service.queueObservedAt)
	}
	for turn, head := range []int{0, 2, 4, 6, 8, 10, 13} {
		at = start.Add(residence + time.Duration(6+turn)*50*time.Millisecond)
		if head == 13 {
			at = at.Add(gap)
		}
		offerThrough(at)
		wait := at.Sub(offered[head].sendTime.Add(residence))
		if wait < 0 || fixture.compression < wait {
			t.Fatalf("head %d does not own a valid compressed receiver wait: %s", head, wait)
		}
		sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(offered[head], at, wait))
		if head != 13 {
			estimate := sequence.sendWindowEstimate(at)
			if estimate.ServiceByteRate != 106840 || estimate.PacingByteRate != 117524 || estimate.PacingDiscovery || estimate.ServiceBacklogged {
				t.Fatalf("head %d changed the preceding two-frame operating point: %+v", head, estimate)
			}
		}
	}
	var newest windowServiceSample
	for _, sample := range service.samples {
		if sample.lastAtNanos == at.UnixNano() {
			newest = sample
		}
	}
	timing := service.roundTripEvidence(at)
	if newest.bytes != 3*frameBytes || newest.receiverBytes != 0 || !newest.supply.recorded || newest.supply.complete == unknown ||
		!newest.supply.headComplete || newest.queued || newest.supply.lastQueue != 0 ||
		service.bucketInterval != 12500*time.Microsecond || timing.latest-timing.minimum > time.Microsecond ||
		service.total != 26*frameBytes || sequence.windowPacer.serviceAcked != service.total || service.sent != ByteCount(12+count)*frameBytes ||
		service.sent <= service.total || service.drained || service.feedbackPending || service.drainServiceEpoch ||
		!service.roundTripProbe.sentAt.IsZero() || service.serviceHoldRate != 106840 || service.heldPacingRate != 117524 ||
		offered[count-1].serviceCreditObserved || service.receiverHeldPrefixAtNanos != 0 {
		t.Fatalf("the selected head lost complete credit, healthy timing, old ownership or its pending tail: sample=%+v timing=%+v sent=%d total=%d held=%d", newest, timing, service.sent, service.total, service.heldPacingRate)
	}
	return fixture, at
}

// Three continuously offered frames span about 68 ms even though their raw
// heads are 50 ms apart. Statistics and admission must use that slower clock.
func TestWindowPacingContinuousOffersRecoverCompressedService(t *testing.T) {
	fixture, at := windowContinuousOfferFixture(t, 0, false)
	service := fixture.service
	snapshot := fixture.sequence.sendWindowSnapshot(at)
	if snapshot.ServiceByteRate <= 106840 || snapshot.ServiceByteRate > 117524 || snapshot.PacingByteRate <= 117524 ||
		snapshot.ServiceBacklogged || snapshot.PacingDiscovery || service.serviceHoldRate != 106840 || service.heldPacingRate != 117524 {
		t.Errorf("statistics hid continuous physical supply or changed the retained owner: %+v", snapshot)
	}
	after := fixture.sequence.sendWindowEstimate(at)
	if after.ServiceByteRate <= 106840 || after.ServiceByteRate > 117524 || after.PacingByteRate <= 117524 ||
		after.ServiceBacklogged || after.PacingDiscovery || service.serviceHoldRate != after.ServiceByteRate || service.heldPacingRate != after.PacingByteRate {
		t.Errorf("admission could not grow from the confirmed offer train: %+v", after)
	}
	// The real reservation prices the next write at the admitted pace. Its
	// cancellation releases only local ownership, not previously paid debt.
	waiter := windowPacingWaiter{}
	service.reserve(at, 2671, after.PacingByteRate, after.ServiceByteRate, 0, 0, false, &waiter)
	defer releaseOpeningCreditReservation(service, &waiter, 2671, false)
	if waiter.serialization != windowPacingSerializationTime(2671, after.PacingByteRate) ||
		waiter.serialization >= 22727273*time.Nanosecond || service.probeSent != 0 || service.pacingReservations != 1 || service.reservedByteCount != 2671 {
		t.Errorf("the next physical reservation did not inherit recovered service: serialization=%s rate=%d", waiter.serialization, after.PacingByteRate)
	}
}

// The gap is between the excluded left head and the first newly credited
// offer. A dense suffix must not erase that actual source idle.
func TestWindowPacingContinuousOffersRejectRealBoundaryGap(t *testing.T) {
	fixture, at := windowContinuousOfferFixture(t, 3*time.Millisecond, false)
	for _, estimate := range []SendWindowEstimate{fixture.sequence.sendWindowSnapshot(at), fixture.sequence.sendWindowEstimate(at)} {
		if estimate.ServiceByteRate != 106840 || estimate.PacingByteRate != 117524 || estimate.ServiceBacklogged || estimate.PacingDiscovery {
			t.Errorf("a late first offer borrowed its dense suffix's continuity: %+v", estimate)
		}
	}
}

// Confirmation after cumulative publication cannot restore the one-shot
// offer proof of an earlier credit, even when every timestamp is populated.
func TestWindowPacingContinuousOffersRejectUnconfirmedCredit(t *testing.T) {
	fixture, at := windowContinuousOfferFixture(t, 0, true)
	unknown := fixture.sequence.resendQueue.sequenceNumberItems[24]
	if unknown == nil || !unknown.serviceCreditObserved || unknown.rttState != sendItemRttWritePending {
		t.Fatal("the cumulative prefix did not spend an actually unconfirmed original")
	}
	for _, estimate := range []SendWindowEstimate{fixture.sequence.sendWindowSnapshot(at), fixture.sequence.sendWindowEstimate(at)} {
		if estimate.ServiceByteRate != 106840 || estimate.PacingByteRate != 117524 || estimate.ServiceBacklogged || estimate.PacingDiscovery {
			t.Errorf("unknown physical credit manufactured a growth interval: %+v", estimate)
		}
	}
	fixture.confirm(unknown)
	head := fixture.sequence.resendQueue.sequenceNumberItems[25]
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(head, at, at.Sub(head.sendTime.Add(421368*time.Microsecond))))
	if estimate := fixture.sequence.sendWindowEstimate(at); estimate.ServiceByteRate != 106840 || fixture.service.total != 26*2671 || fixture.service.heldPacingRate != 117524 {
		t.Fatalf("late confirmation or duplicate head restored spent offer proof: %+v", estimate)
	}
}
