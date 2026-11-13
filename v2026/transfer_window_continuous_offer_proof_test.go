// Offer continuity supplies a stable lower bound without borrowing the
// decrease authority of a different, strictly covered service interval.
package connect

import (
	"testing"
	"time"
)

// A lower old pair remains strictly covered inside the same scan horizon.
// Selecting a faster offer-only interval must select its absent proof too.
func TestWindowPacingContinuousOfferRateOwnsItsProof(t *testing.T) {
	fixture, at := windowContinuousOfferFixture(t, 0, false)
	service := fixture.service
	var left, right windowServiceSample
	for _, sample := range service.samples {
		if sample.lastAtNanos == at.Add(-400*time.Millisecond).UnixNano() {
			left = sample
		}
		if sample.lastAtNanos == at.Add(-350*time.Millisecond).UnixNano() {
			right = sample
		}
	}
	covered := right.supply
	covered.maximumGap = time.Duration(right.lastAtNanos - left.lastAtNanos)
	if left.bytes != 2*2671 || right.bytes != 2*2671 ||
		covered.qualifiedSpan(int64(50*time.Millisecond), 0, left.supply.headLastMinAtNanos, service.bucketInterval, false) != int64(50*time.Millisecond) {
		t.Fatal("the scan no longer contains its lower strictly covered old pair")
	}
	rate, support, latest, proofAtNanos := service.measureCapacityEvidence(time.Second, at, false)
	if rate <= 106840 || rate > 117524 || latest != rate || support != kib(4) || proofAtNanos != 0 ||
		service.serviceHoldRate != 106840 || service.heldPacingRate != 117524 {
		t.Fatalf("offer-only service borrowed older decrease proof: rate=%d latest=%d support=%d proof=%d", rate, latest, support, proofAtNanos)
	}
	for _, retain := range []bool{true, false, true} {
		got, gotSupport, gotLatest, gotProof := service.measureCapacityEvidence(time.Second, at, retain)
		if got != rate || gotLatest != latest || gotSupport != support || gotProof != 0 || service.serviceHoldRate != rate || service.heldPacingRate != 117524 {
			t.Fatalf("retention changed the selected interval's type: retain=%t rate=%d latest=%d support=%d proof=%d", retain, got, gotLatest, gotSupport, gotProof)
		}
	}
}

// Strict preoffering still carries the current endpoint and can identify
// an independently excessive real flight with a queued paired round trip.
func TestWindowPacingCoveredCurrentPairKeepsDecreaseProof(t *testing.T) {
	start := time.Unix(1700000000, 0)
	fixture := newWindowReceiverCreditFixture(t, nil, start, 50*time.Millisecond)
	t.Cleanup(func() { fixture.sequence.resendQueue.Clear() })
	var offered [12]*sendItem
	for number := range offered {
		offered[number] = fixture.write(uint64(number), 2671, start)
	}
	leftAt := start.Add(100 * time.Millisecond)
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(offered[0], leftAt, 0))
	at := leftAt.Add(50 * time.Millisecond)
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(offered[2], at, 40*time.Millisecond))
	rate, support, latest, proofAtNanos := fixture.service.measureCapacityEvidence(time.Second, at, false)
	if rate != 106840 || latest != rate || support != kib(4) || proofAtNanos != at.UnixNano() ||
		fixture.service.total != 3*2671 || fixture.service.sent-fixture.service.total != 9*2671 ||
		!fixture.service.backloggedCapacityAt(rate, proofAtNanos, at) || offered[11].serviceCreditObserved {
		t.Fatalf("a covered current pair lost its exact decrease authority: rate=%d latest=%d support=%d proof=%d", rate, latest, support, proofAtNanos)
	}
}

// Sparse cumulative fallback can merge credits out of offer order. A later
// interior point must not erase the conservative gap already recorded.
func TestWindowPacingOfferMergeKeepsConservativeGap(t *testing.T) {
	start := time.Unix(1700000000, 0)
	fixture := newWindowReceiverCreditFixture(t, nil, start, 50*time.Millisecond)
	t.Cleanup(func() { fixture.sequence.resendQueue.Clear() })
	var offered [3]*sendItem
	for number := range offered {
		offered[number] = fixture.write(uint64(number), 2671, start.Add(time.Duration(number)*20*time.Millisecond))
	}
	credit := windowServiceAckCredit{}
	for _, number := range []int{0, 2, 1} {
		credit.add(fixture.sequence.takePacingServiceCredit(offered[number]))
	}
	if credit.bytes != 3*2671 || !credit.offerComplete || credit.firstSentAtNanos != start.UnixNano() ||
		credit.lastSentAtNanos != start.Add(40*time.Millisecond).UnixNano() || credit.maximumOfferGap != 40*time.Millisecond {
		t.Fatalf("an interior offer erased conservative interval coverage: %+v", credit)
	}
	credit.add(fixture.sequence.takePacingServiceCredit(offered[1]))
	if credit.bytes != 3*2671 || credit.maximumOfferGap != 40*time.Millisecond {
		t.Fatal("duplicate physical credit changed its numerator or gap proof")
	}
}
