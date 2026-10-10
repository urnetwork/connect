// New queue timing cannot turn an older lower-bound delivery pair into a
// capacity limit. Real offered credit still proves independently excess flight.
package connect

import (
	"testing"
	"time"
)

// Five preoffered frames supply the old clean pair. Nineteen later offers
// supply a newer queued prefix which cannot cover that pair's left boundary.
func windowPacingUnqualifiedQueueFixture(t *testing.T, flightFrames int) (*windowReceiverCreditFixture, time.Time, ByteCount) {
	t.Helper()
	const frameBytes ByteCount = 2671
	service, at := newWindowQualifiedServiceFixture(t, 12500000, 50*time.Millisecond, 100*time.Millisecond)
	service.observeReceiverRoundTrip(0, 180*time.Millisecond, 130*time.Millisecond, 50*time.Millisecond, at.Add(time.Millisecond))
	at = at.Add(2 * time.Millisecond)
	service.observeReceiverRoundTrip(0, 150*time.Millisecond, 100*time.Millisecond, 50*time.Millisecond, at)
	fixture := newWindowReceiverCreditFixture(t, service, at, 50*time.Millisecond)
	sequence := fixture.sequence
	sequence.sendBufferSettings = DefaultSendBufferSettings()
	sequence.deliveredBytes = make([]deliveredBytesSample, deliveredBytesRingSize)
	sequence.contractMultiRouteWriter = &windowPacingPolicyWriter{policy: transferFlightPolicySnapshot{h1Only: true}}
	permission := max(kib(64), ByteCount(flightFrames)*frameBytes)
	sequence.sendBufferSettings.ResendQueueMaxByteCount = max(sequence.sendBufferSettings.ResendQueueMaxByteCount, permission)
	sequence.observeReceiveWindowAdvertisement(receiveAckMessage{receiveWindowSet: true, receiveWindowByteCount: uint32(permission)})
	t.Cleanup(func() {
		sequence.windowPacer.close()
		sequence.resendQueue.Clear()
		if service.sent != service.total || service.pendingWrites != 0 || service.reservedByteCount != 0 {
			t.Error("closing the real owner retained physical flight")
		}
	})
	before := sequence.sendWindowEstimate(at)
	if before.PacingByteRate != 13750000 || before.ServiceBacklogged || before.PacingDiscovery {
		t.Fatalf("ordinary admission did not establish the fast held pace: %+v", before)
	}
	priorTotal := service.total
	offerAt := at.Add(time.Second)
	var originals [24]*sendItem
	for number := range originals {
		sentAt := offerAt
		if number >= 5 {
			sentAt = sentAt.Add(55 * time.Millisecond)
		}
		originals[number] = fixture.write(uint64(number), frameBytes, sentAt)
	}
	leftAt := offerAt.Add(100 * time.Millisecond)
	sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(originals[0], leftAt, 0))
	guard := fixture.write(24, frameBytes, leftAt)
	oldAt := leftAt.Add(50 * time.Millisecond)
	sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(originals[4], oldAt, 49*time.Millisecond))
	old := sequence.sendWindowEstimate(oldAt)
	if old.ServiceByteRate != 213680 || old.PacingByteRate != before.PacingByteRate || old.ServiceBacklogged {
		t.Fatalf("the old clean raw50ms pair lost its lower-bound meaning: %+v", old)
	}
	at = offerAt.Add(164 * time.Millisecond)
	ack := fixture.ack(originals[23], at, 5*time.Millisecond)
	sequence.coalesceReceivedAck(fixture.ackWindow, ack)
	for number := 25; number < 24+flightFrames; number++ {
		fixture.write(uint64(number), frameBytes, at)
	}
	var left, middle, newest windowServiceSample
	for _, sample := range service.samples {
		switch sample.lastAtNanos {
		case leftAt.UnixNano():
			left = sample
		case oldAt.UnixNano():
			middle = sample
		case at.UnixNano():
			newest = sample
		}
	}
	coverage := newest
	coverage.mergeSupply(middle)
	coverage.supply.maximumGap = max(coverage.supply.maximumGap, oldAt.Sub(leftAt))
	if left.bytes != frameBytes || middle.bytes != 4*frameBytes || newest.bytes != 19*frameBytes ||
		!newest.queued || newest.receiverBytes != 0 || !newest.supply.complete || !newest.supply.headComplete ||
		newest.supply.latestSentNanos != offerAt.Add(55*time.Millisecond).UnixNano() || newest.supply.lastQueue != 0 ||
		coverage.supply.qualifiedSpan(int64(at.Sub(leftAt)), 0, left.supply.headLastMinAtNanos, service.bucketInterval, false) != 0 {
		t.Fatalf("the newer queued prefix did not fail exact late-offer coverage: left=%+v middle=%+v newest=%+v", left, middle, newest)
	}
	timing := service.roundTripEvidence(at)
	if timing.latest-timing.minimum != 4*time.Millisecond || service.feedbackPending || service.drained ||
		service.total-priorTotal != 24*frameBytes || service.sent-service.total != ByteCount(flightFrames)*frameBytes ||
		service.sent-service.total > permission || service.heldPacingRate != before.PacingByteRate || guard.serviceCreditObserved {
		t.Fatal("the queued prefix changed real credit, retained pace, hard permission or the pending guard")
	}
	rate, total, latest := service.measure(time.Second, at, false)
	if rate != 213680 || latest != rate || total != priorTotal+24*frameBytes {
		t.Fatalf("the rejected queued prefix replaced the old accepted pair: %d/%d total=%d", rate, latest, total)
	}
	sequence.coalesceReceivedAck(fixture.ackWindow, ack)
	if service.total != total || sequence.windowPacer.serviceAcked != 24*frameBytes || guard.serviceCreditObserved {
		t.Fatal("duplicate publication changed exact-once owner credit")
	}
	return fixture, at, before.PacingByteRate
}

// The observed small flight is below the independently granted pace's
// residence. A newer rejected interval cannot authorize a fiftyfold cut.
func TestWindowPacingNewerUnqualifiedQueueCannotRepriceHeldFlight(t *testing.T) {
	fixture, at, held := windowPacingUnqualifiedQueueFixture(t, 19)
	service := fixture.service
	snapshot := fixture.sequence.sendWindowSnapshot(at)
	if snapshot.ServiceByteRate != 213680 || snapshot.ServiceBacklogged || snapshot.PacingByteRate != held || service.heldPacingRate != held {
		t.Errorf("statistics mixed newer queue with old lower-bound delivery: %+v", snapshot)
	}
	after := fixture.sequence.sendWindowEstimate(at)
	if after.ServiceByteRate != 213680 || after.ServiceBacklogged || after.PacingByteRate != held || service.heldPacingRate != held {
		t.Errorf("admission used a rejected interval to revoke held pacing: %+v", after)
	}
}

// Independent excess flight is still actionable even when the newest pair
// was rejected. All bytes are confirmed H1 originals under a sufficient grant.
func TestWindowPacingNewerUnqualifiedQueueStillBoundsExcessFlight(t *testing.T) {
	fixture, at, held := windowPacingUnqualifiedQueueFixture(t, 1030)
	if float64(fixture.service.sent-fixture.service.total) <= float64(held)*(152*time.Millisecond).Seconds() {
		t.Fatal("real offered flight did not exceed the independent old-pace allowance")
	}
	after := fixture.sequence.sendWindowEstimate(at)
	if after.ServiceByteRate != 213680 || !after.ServiceBacklogged || after.PacingByteRate != 202996 || fixture.service.heldPacingRate != after.PacingByteRate {
		t.Fatalf("independently excessive physical flight lost decrease authority: %+v", after)
	}
}
