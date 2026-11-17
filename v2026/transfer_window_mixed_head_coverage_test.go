// Mixed receiver-head and raw ACK clocks must qualify the denominator they
// actually price. These fixtures use confirmed offers and the real coalescer.
package connect

import (
	"testing"
	"time"
)

// A real fast pair establishes the hold. A later two-original cumulative
// prefix has an old exact head, but cannot retime all of its credited bytes.
func windowMixedHeadCoverageFixture(t *testing.T, preoffered bool) (*windowReceiverCreditFixture, windowServiceSample, windowServiceSample, time.Time) {
	t.Helper()
	start := time.Unix(1700000000, 0)
	fixture := newWindowReceiverCreditFixture(t, nil, start, 200*time.Microsecond)
	service := fixture.service
	guard := newWindowReceiverCreditFixture(t, service, start, fixture.compression)
	guardItem := guard.write(0, 64000, start)
	t.Cleanup(func() {
		fixture.sequence.windowPacer.close()
		guard.sequence.windowPacer.close()
		// Fixture items have no pooled payloads, callbacks or borrowed budget.
		fixture.sequence.resendQueue.Clear()
		guard.sequence.resendQueue.Clear()
		if service.sent != service.total || service.pendingWrites != 0 || service.reservedByteCount != 0 {
			t.Error("mixed-clock fixture retained physical ownership after close")
		}
	})
	first := fixture.write(0, 102949, start.Add(200*time.Microsecond))
	second := fixture.write(1, 102949, start.Add(10200*time.Microsecond))
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(first, start.Add(10200*time.Microsecond), 0))
	warmAt := start.Add(20200 * time.Microsecond)
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(second, warmAt, 0))
	if got := fixture.measured(warmAt); got != 10294900 || service.minRoundTrip != 10*time.Millisecond || service.drained {
		t.Fatalf("confirmed warm pair did not establish independent service: rate=%d minimum=%s drained=%t", got, service.minRoundTrip, service.drained)
	}
	prefixOffer := start.Add(20800 * time.Microsecond)
	prefixFirst := fixture.write(2, 2102, prefixOffer)
	prefixHead := fixture.write(3, 2102, prefixOffer)
	leftAt := start.Add(620200 * time.Microsecond)
	rightOffer := leftAt
	if preoffered {
		rightOffer = prefixOffer
	}
	rightHead := fixture.write(4, 2102, rightOffer)
	leftAck := fixture.ack(prefixHead, leftAt, 589400*time.Microsecond)
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, leftAck)
	at := start.Add(630200 * time.Microsecond)
	rightAck := fixture.ack(rightHead, at, 0)
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, rightAck)
	var left, right windowServiceSample
	for _, sample := range service.samples {
		switch sample.lastAtNanos {
		case leftAt.UnixNano():
			left = sample
		case at.UnixNano():
			right = sample
		}
	}
	if left.bytes != 4204 || left.receiverBytes != 0 || !left.supply.complete || !left.supply.headComplete ||
		left.supply.headLastMinAtNanos != start.Add(30800*time.Microsecond).UnixNano() ||
		right.bytes != 2102 || right.receiverBytes != 2102 || !right.supply.complete || !right.supply.headComplete ||
		right.supply.headMaximumAtNanos != at.UnixNano() || right.supply.latestSentNanos != rightOffer.UnixNano() ||
		service.bucketInterval != 10*time.Millisecond || service.feedbackPending || service.drained ||
		service.serviceHoldRate != 10294900 || service.receiverHeldPrefixAtNanos != leftAt.UnixNano() {
		t.Fatalf("preoffered=%t: real mixed-clock endpoints not isolated: left=%+v right=%+v pending=%t drained=%t hold=%d", preoffered, left, right, service.feedbackPending, service.drained, service.serviceHoldRate)
	}
	wantQueue := time.Duration(0)
	if preoffered {
		wantQueue = 599400 * time.Microsecond
	}
	if right.supply.minimumPath != 10*time.Millisecond || right.supply.lastQueue != wantQueue ||
		service.total != 2*102949+3*2102 || service.sent-service.total != 64000 ||
		fixture.sequence.windowPacer.serviceAcked != service.total || guard.sequence.windowPacer.serviceAcked != 0 ||
		!prefixFirst.serviceCreditObserved || !prefixHead.serviceCreditObserved || !rightHead.serviceCreditObserved || guardItem.serviceCreditObserved {
		t.Fatalf("preoffered=%t: exact queue or once-only ownership changed: queue=%s minimum=%s total=%d outstanding=%d", preoffered, right.supply.lastQueue, right.supply.minimumPath, service.total, service.sent-service.total)
	}
	before := service.total
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, leftAck)
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, rightAck)
	if service.total != before || guardItem.serviceCreditObserved {
		t.Fatal("duplicate ACKs changed byte credit or the independent guard")
	}
	return fixture, left, right, at
}

// Ten milliseconds of raw continuity cannot cover a 599.4 ms head extent
// when the only selected original was physically offered after that extent.
func TestWindowServiceMixedHeadGapRejectsRawContinuity(t *testing.T) {
	fixture, left, right, at := windowMixedHeadCoverageFixture(t, false)
	supply := right.supply
	rawSpan := right.lastAtNanos - left.lastAtNanos
	supply.maximumGap = max(supply.maximumGap, time.Duration(rawSpan))
	headSpan := supply.headMaximumAtNanos - left.supply.headLastMinAtNanos
	if rawSpan != int64(10*time.Millisecond) || headSpan != int64(599400*time.Microsecond) ||
		supply.latestSentNanos <= left.supply.headLastMinAtNanos-int64(supply.minimumPath) || supply.lastQueue != 0 {
		t.Fatal("late physical supply did not isolate the raw/head coverage mismatch")
	}
	span := supply.qualifiedSpan(rawSpan, 0, left.supply.headLastMinAtNanos, fixture.service.bucketInterval, false)
	rate, _, latest, proof := fixture.service.measureCapacityEvidence(time.Second, at, false)
	if span != 0 || proof != 0 {
		t.Fatalf("MIXED_HEAD_RAW_CONTINUITY_GRANTED_DECREASE: raw=%d head=%d qualified=%d rate=%d latest=%d proof=%d", rawSpan, headSpan, span, rate, latest, proof)
	}
	// The older real prefix remains an offered lower bound:4204B/600ms.
	// Rejecting false authority does not prohibit its existing proof-zero hold.
	if rate != 7006 || latest != 7006 || fixture.service.serviceHoldRate != 10294900 {
		t.Fatalf("the independent older offered lower bound changed: rate=%d latest=%d hold=%d", rate, latest, fixture.service.serviceHoldRate)
	}
	rate, _, latest, proof = fixture.service.measureCapacityEvidence(time.Second, at, true)
	if rate != 7006 || latest != 7006 || proof != 0 || fixture.service.serviceHoldRate != 7006 {
		t.Fatalf("proof-zero retention was silently changed: rate=%d latest=%d proof=%d hold=%d", rate, latest, proof, fixture.service.serviceHoldRate)
	}
}

// A confirmed original already present before the exact left head minus
// propagation still covers the full denominator and may lower service.
func TestWindowServiceMixedHeadGapPreofferKeepsDecrease(t *testing.T) {
	fixture, left, right, at := windowMixedHeadCoverageFixture(t, true)
	supply := right.supply
	rawSpan := right.lastAtNanos - left.lastAtNanos
	supply.maximumGap = max(supply.maximumGap, time.Duration(rawSpan))
	if supply.latestSentNanos != left.supply.headLastMinAtNanos-int64(supply.minimumPath) ||
		supply.lastQueue != 599400*time.Microsecond ||
		supply.qualifiedSpan(rawSpan, 0, left.supply.headLastMinAtNanos, fixture.service.bucketInterval, false) != int64(599400*time.Microsecond) {
		t.Fatal("confirmed preoffering lost full-span coverage")
	}
	// A zero horizon selects the latest pair rather than the older warm peak.
	rate, _, latest, proof := fixture.service.measureCapacityEvidence(0, at, true)
	if rate != 0 || latest != 3506 || proof != at.UnixNano() || fixture.service.serviceHoldRate != 3506 {
		t.Fatalf("full-span preoffering lost lower authority: rate=%d latest=%d proof=%d hold=%d", rate, latest, proof, fixture.service.serviceHoldRate)
	}
}
