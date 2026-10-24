// Retry ambiguity preserves raw credit and an exact cumulative head, but
// cannot supply the older offered interval that would mask a false decrease.
package connect

import (
	"testing"
	"time"
)

// The first prefix member takes the production pre-retry invalidation and
// successful recovery-write path. Its confirmed original tail stays exact.
func windowMixedHeadCommittedFixture(t *testing.T, preoffered bool) (*windowReceiverCreditFixture, time.Time, time.Time) {
	t.Helper()
	start := time.Unix(1700000000, 0)
	fixture := newWindowReceiverCreditFixture(t, nil, start, 200*time.Microsecond)
	service := fixture.service
	guard := newWindowReceiverCreditFixture(t, service, start, fixture.compression)
	guardItem := guard.write(0, 64000, start)
	t.Cleanup(func() {
		fixture.sequence.windowPacer.close()
		guard.sequence.windowPacer.close()
		// Ordinary fixture items own no pooled payload, callback or budget.
		fixture.sequence.resendQueue.Clear()
		guard.sequence.resendQueue.Clear()
		if service.sent != service.total || service.pendingWrites != 0 || service.reservedByteCount != 0 {
			t.Error("committed mixed-head fixture retained physical ownership")
		}
	})
	first := fixture.write(0, 102949, start.Add(200*time.Microsecond))
	second := fixture.write(1, 102949, start.Add(10200*time.Microsecond))
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(first, start.Add(10200*time.Microsecond), 0))
	warmAt := start.Add(20200 * time.Microsecond)
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(second, warmAt, 0))
	if got := fixture.measured(warmAt); got != 10294900 || service.minRoundTrip != 10*time.Millisecond || service.drained {
		t.Fatalf("earned warm service changed: rate=%d minimum=%s drained=%t", got, service.minRoundTrip, service.drained)
	}

	prefixOffer := start.Add(20800 * time.Microsecond)
	prefixFirst := fixture.write(2, 2102, prefixOffer)
	prefixHead := fixture.write(3, 2102, prefixOffer)
	var rightHead *sendItem
	if preoffered {
		rightHead = fixture.write(4, 2102, prefixOffer)
	}
	if prefixFirst.rttState != sendItemRttWriteConfirmed || !prefixFirst.rttH1 || prefixFirst.serviceCreditObserved {
		t.Fatal("prefix original was not independently confirmed before recovery")
	}
	beforeSent, beforeTotal := service.sent, service.total
	recoveryAt := start.Add(610200 * time.Microsecond)
	fixture.sequence.invalidateReceiverRttWrite(prefixFirst)
	service.beginWrite(fixture.sequence.sequenceId, prefixFirst.messageId, prefixFirst.sequenceNumber, recoveryAt, true)
	service.finishWrite(fixture.sequence.sequenceId, prefixFirst.messageId, true)
	if prefixFirst.rttState != sendItemRttUnavailable || prefixFirst.serviceCreditObserved ||
		prefixHead.rttState != sendItemRttWriteConfirmed || !prefixHead.rttH1 ||
		service.sent != beforeSent || service.total != beforeTotal || !service.lastRecoveryWriteAt.Equal(recoveryAt) {
		t.Fatal("real recovery did not preserve exact tail timing and once-only original bytes")
	}

	leftAt := start.Add(620200 * time.Microsecond)
	if !preoffered {
		rightHead = fixture.write(4, 2102, leftAt)
	}
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
	wantOffer, wantQueue := leftAt, time.Duration(0)
	if preoffered {
		wantOffer, wantQueue = prefixOffer, 599400*time.Microsecond
	}
	if left.bytes != 4204 || left.receiverBytes != 0 || !left.supply.recorded || left.supply.complete ||
		!left.supply.headComplete || left.supply.headLastMinAtNanos != start.Add(30800*time.Microsecond).UnixNano() ||
		right.bytes != 2102 || right.receiverBytes != 2102 || !right.supply.complete || !right.supply.headComplete ||
		right.supply.latestSentNanos != wantOffer.UnixNano() || right.supply.minimumPath != 10*time.Millisecond ||
		right.supply.lastQueue != wantQueue || right.lastAtNanos-left.lastAtNanos != int64(10*time.Millisecond) ||
		right.supply.headMaximumAtNanos-left.supply.headLastMinAtNanos != int64(599400*time.Microsecond) ||
		service.bucketInterval != 10*time.Millisecond || service.feedbackPending || service.drained ||
		service.serviceHoldRate != 10294900 || service.receiverHeldPrefixAtNanos != leftAt.UnixNano() {
		t.Fatalf("preoffered=%t: retry/head selection premise changed: left=%+v right=%+v pending=%t drained=%t hold=%d",
			preoffered, left, right, service.feedbackPending, service.drained, service.serviceHoldRate)
	}
	wantTotal := ByteCount(2*102949 + 3*2102)
	if service.total != wantTotal || service.sent-service.total != 64000 ||
		fixture.sequence.windowPacer.serviceAcked != wantTotal || guard.sequence.windowPacer.serviceAcked != 0 ||
		!prefixFirst.serviceCreditObserved || !prefixHead.serviceCreditObserved || !rightHead.serviceCreditObserved ||
		guardItem.serviceCreditObserved {
		t.Fatal("retry/head fixture lost exact raw bytes or the independent retained guard")
	}
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, leftAck)
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, rightAck)
	if service.total != wantTotal || fixture.sequence.windowPacer.serviceAcked != wantTotal ||
		service.sent-service.total != 64000 || guardItem.serviceCreditObserved {
		t.Fatal("duplicate cumulative ACKs granted recovery bytes twice")
	}
	return fixture, warmAt, at
}

// With the older offered fallback genuinely incomplete, the wrong mixed
// clock pair becomes both the current selection and the committed service.
func TestWindowServiceMixedHeadCommittedRetryRejectsDecrease(t *testing.T) {
	fixture, warmAt, at := windowMixedHeadCommittedFixture(t, false)
	service := fixture.service
	// This positive horizon is the controller's target-goodput service query
	// (estimateSendWindow), not a latest-only zero-horizon read.
	const horizon = time.Second
	rate, _, latest, proof := service.measureCapacityEvidence(horizon, at, false)
	if service.serviceHoldRate != 10294900 {
		t.Fatal("nonretaining selection changed the independently earned hold")
	}
	retainedRate, _, retainedLatest, retainedProof := service.measureCapacityEvidence(horizon, at, true)
	// The earlier real warm pair remains the latest eligible fallback. Its
	// own old proof must not be relabeled as the rejected right endpoint.
	if rate != 0 || latest != 10294900 || proof != warmAt.UnixNano() ||
		retainedRate != 0 || retainedLatest != 10294900 || retainedProof != warmAt.UnixNano() || service.serviceHoldRate != 10294900 {
		t.Fatalf("MIXED_HEAD_COMMITTED_FALSE_DECREASE: horizon=%s rate=%d latest=%d proof=%d retained_rate=%d retained_latest=%d retained_proof=%d hold=%d right=%d warm=%d",
			horizon, rate, latest, proof, retainedRate, retainedLatest, retainedProof, service.serviceHoldRate, at.UnixNano(), warmAt.UnixNano())
	}
	if service.total != 2*102949+3*2102 || service.sent-service.total != 64000 {
		t.Fatal("selection or retention changed raw credit/custody")
	}
}

// A retry elsewhere in the prefix cannot erase an independently preoffered
// selected original's full-span authority to lower the committed service.
func TestWindowServiceMixedHeadCommittedRetryPreofferKeepsDecrease(t *testing.T) {
	fixture, _, at := windowMixedHeadCommittedFixture(t, true)
	service := fixture.service
	const horizon = time.Second
	rate, _, latest, proof := service.measureCapacityEvidence(horizon, at, false)
	if rate != 3506 || latest != 3506 || proof != at.UnixNano() || service.serviceHoldRate != 10294900 {
		t.Fatalf("full-span independent preoffer lost selected authority: rate=%d latest=%d proof=%d hold=%d", rate, latest, proof, service.serviceHoldRate)
	}
	rate, _, latest, proof = service.measureCapacityEvidence(horizon, at, true)
	if rate != 3506 || latest != 3506 || proof != at.UnixNano() || service.serviceHoldRate != 3506 {
		t.Fatalf("full-span independent preoffer lost committed authority: rate=%d latest=%d proof=%d hold=%d", rate, latest, proof, service.serviceHoldRate)
	}
	if service.total != 2*102949+3*2102 || service.sent-service.total != 64000 {
		t.Fatal("legitimate decrease changed raw credit/custody")
	}
}
