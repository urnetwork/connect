// A compressed permission-limited prefix measures delivery from below; it
// cannot price its own outstanding bytes as a queue and revoke a faster pace.
package connect

import (
	"testing"
	"time"
)

// Confirmed H1 offers and the real cumulative coalescer reproduce the exact
// mixed-clock pair: one immediate head, then five frames after 50 ms whose
// own head arrived only 1 ms later. No rate, flight or backlog flag is seeded.
func TestWindowPacingCompressedPrefixCannotRevokeHeldPace(t *testing.T) {
	const frameBytes ByteCount = 2671
	const fastRate ByteCount = 12500000
	const permission ByteCount = 64 * 1024
	service, at := newWindowQualifiedServiceFixture(t, fastRate, 50*time.Millisecond, 100*time.Millisecond)
	// Prior queueing ends discovery; a clean following tuple restores healthy
	// timing before ordinary admission grants the established service's pace.
	service.observeReceiverRoundTrip(0, 180*time.Millisecond, 130*time.Millisecond, 50*time.Millisecond, at.Add(time.Millisecond))
	at = at.Add(2 * time.Millisecond)
	service.observeReceiverRoundTrip(0, 150*time.Millisecond, 100*time.Millisecond, 50*time.Millisecond, at)
	fixture := newWindowReceiverCreditFixture(t, service, at, 50*time.Millisecond)
	sequence := fixture.sequence
	sequence.sendBufferSettings = DefaultSendBufferSettings()
	sequence.deliveredBytes = make([]deliveredBytesSample, deliveredBytesRingSize)
	sequence.contractMultiRouteWriter = &windowPacingPolicyWriter{policy: transferFlightPolicySnapshot{h1Only: true}}
	sequence.observeReceiveWindowAdvertisement(receiveAckMessage{receiveWindowSet: true, receiveWindowByteCount: uint32(permission)})
	t.Cleanup(func() {
		sequence.windowPacer.close()
		sequence.resendQueue.Clear()
		if service.sent != service.total || service.pendingWrites != 0 || service.reservedByteCount != 0 {
			t.Error("closing the exact owner retained unacknowledged flight")
		}
	})
	before := sequence.sendWindowEstimate(at)
	if before.ServiceByteRate != fastRate || before.PacingByteRate != 13750000 ||
		before.PacingDiscovery || before.ServiceBacklogged || before.Window != permission {
		t.Fatalf("healthy admission did not establish the earlier granted pace: %+v", before)
	}
	priorTotal := service.total
	offerAt := at.Add(time.Second)
	var originals [24]*sendItem
	for number := range originals {
		originals[number] = fixture.write(uint64(number), frameBytes, offerAt)
	}
	leftAt := offerAt.Add(100 * time.Millisecond)
	sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(originals[0], leftAt, 0))
	// Refill the one released frame, remaining below the same 64 KiB grant.
	refill := fixture.write(24, frameBytes, leftAt)
	at = leftAt.Add(50 * time.Millisecond)
	ack := fixture.ack(originals[5], at, 49*time.Millisecond)
	sequence.coalesceReceivedAck(fixture.ackWindow, ack)
	var left, right windowServiceSample
	for _, sample := range service.samples {
		if sample.bytes > 0 && sample.lastAtNanos == leftAt.UnixNano() {
			left = sample
		}
		if sample.bytes > 0 && sample.lastAtNanos == at.UnixNano() {
			right = sample
		}
	}
	if left.bytes != frameBytes || left.receiverBytes != frameBytes || right.bytes != 5*frameBytes ||
		right.receiverBytes != 0 || !right.supply.recorded || !right.supply.complete || !right.supply.headComplete ||
		right.supply.rawClock || right.supply.latestSentNanos != offerAt.UnixNano() ||
		right.supply.headMaximumAtNanos != leftAt.Add(time.Millisecond).UnixNano() ||
		right.supply.lastQueue != 0 || right.supply.minimumPath != 100*time.Millisecond {
		t.Fatalf("real cumulative prefix lost its exact credit or clock provenance: left=%+v right=%+v", left, right)
	}
	coverage := right.supply
	coverage.maximumGap = at.Sub(leftAt)
	if coverage.latestSentNanos != left.supply.headLastMinAtNanos-int64(coverage.minimumPath) ||
		coverage.qualifiedSpan(int64(50*time.Millisecond), 0, left.supply.headLastMinAtNanos, service.bucketInterval, false) != int64(50*time.Millisecond) {
		t.Fatal("complete preoffering did not qualify the unchanged raw 50 ms denominator")
	}
	timing := service.roundTripEvidence(at)
	if timing.latest != 101*time.Millisecond || timing.minimum != 100*time.Millisecond ||
		service.total-priorTotal != 6*frameBytes || sequence.windowPacer.serviceAcked != 6*frameBytes ||
		service.sent-service.total != 19*frameBytes || service.sent-service.total > permission ||
		service.drained || service.feedbackPending || service.receiverHeldPrefixAtNanos != 0 || refill.serviceCreditObserved {
		t.Fatal("the real pair changed permission, delivery, refill ownership or healthy receiver timing")
	}
	// The lower estimate remains valid. Only its authority to lower an
	// independently admitted pace is disputed; no numerator is inflated.
	rate, total, latest := service.measure(time.Second, at, false)
	if rate != 267100 || latest != rate || total != priorTotal+6*frameBytes || service.serviceHoldRate != fastRate {
		t.Fatalf("raw lower-bound pair or read-only retention changed: rate=%d latest=%d total=%d held=%d", rate, latest, total, service.serviceHoldRate)
	}
	snapshot := sequence.sendWindowSnapshot(at)
	if snapshot.ServiceByteRate != rate || snapshot.ServiceBacklogged || snapshot.PacingByteRate != before.PacingByteRate ||
		service.heldPacingRate != before.PacingByteRate || service.serviceHoldRate != fastRate {
		t.Errorf("statistics gave a compressed lower bound decrease authority: %+v", snapshot)
	}
	after := sequence.sendWindowEstimate(at)
	if after.ServiceByteRate != rate || after.ServiceBacklogged || after.PacingByteRate != before.PacingByteRate ||
		after.Window != permission || service.heldPacingRate != before.PacingByteRate || service.serviceHoldRate != rate {
		t.Errorf("admission priced ordinary limited flight as a standing queue: before=%+v after=%+v", before, after)
	}
	sequence.coalesceReceivedAck(fixture.ackWindow, ack)
	if service.total != total || sequence.windowPacer.serviceAcked != 6*frameBytes || refill.serviceCreditObserved {
		t.Fatal("duplicate cumulative publication changed exact-once ownership")
	}
}
