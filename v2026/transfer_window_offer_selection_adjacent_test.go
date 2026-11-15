// Selection must preserve a real covered decrease and the nearest offer-only
// fallback without borrowing rejection or timing from unselected older pairs.
package connect

import (
	"testing"
	"time"
)

// Coverage priority is evidence priority, not a maximum-rate rule. A lower
// covered pair can price independently excessive flight at its own current rate.
func TestWindowPacingOfferSelectionKeepsLowerCoveredDecrease(t *testing.T) {
	start := time.Unix(1700000000, 0)
	fixture := newWindowReceiverCreditFixture(t, nil, start, 50*time.Millisecond)
	sibling := newWindowReceiverCreditFixture(t, fixture.service, start, 50*time.Millisecond)
	sequence, service := fixture.sequence, fixture.service
	sequence.sendBufferSettings = DefaultSendBufferSettings()
	sequence.deliveredBytes = make([]deliveredBytesSample, deliveredBytesRingSize)
	sequence.contractMultiRouteWriter = &windowPacingPolicyWriter{policy: transferFlightPolicySnapshot{h1Only: true}}
	sequence.observeReceiveWindowAdvertisement(receiveAckMessage{receiveWindowSet: true, receiveWindowByteCount: 2 * 1024 * 1024})
	t.Cleanup(func() {
		sequence.windowPacer.close()
		sibling.sequence.windowPacer.close()
		sequence.resendQueue.Clear()
		sibling.sequence.resendQueue.Clear()
		if service.sent != 5000 || service.total != 5000 || service.pendingWrites != 0 ||
			service.pacingReservations != 0 || service.reservedByteCount != 0 {
			t.Error("closing the real pending flight lost byte ownership")
		}
	})
	var items [24]*sendItem
	items[0] = fixture.write(0, 1000, start.Add(175*time.Millisecond))
	for number := 1; number < 4; number++ {
		items[number] = fixture.write(uint64(number), 1000, start.Add(time.Duration(180+10*(number-1))*time.Millisecond))
	}
	first := sibling.write(0, 1000, start.Add(200*time.Millisecond))
	for number := 4; number < len(items); number++ {
		items[number] = fixture.write(uint64(number), 1000, start.Add(205*time.Millisecond))
	}
	firstAt, nearestAt, at := start.Add(400*time.Millisecond), start.Add(425*time.Millisecond), start.Add(475*time.Millisecond)
	sibling.sequence.coalesceReceivedAck(sibling.ackWindow, sibling.ack(first, firstAt, 0))
	sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(items[0], nearestAt, 50*time.Millisecond))
	before := sequence.sendWindowEstimate(nearestAt)
	if before.ServiceByteRate != 0 || before.ServiceEstablished || before.PacingByteRate <= 60000 ||
		!before.PacingDiscovery || service.heldPacingRate != before.PacingByteRate {
		t.Fatalf("ordinary admission did not establish the independent high grant: %+v", before)
	}
	sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(items[3], at, 50*time.Millisecond))
	timing := service.roundTripEvidence(at)
	if service.sent != 25000 || service.total != 5000 || service.pendingWrites != 1 ||
		service.feedbackPending || service.drained || service.receiverHeldPrefixAtNanos != 0 ||
		timing.minimum != 200*time.Millisecond || timing.latest != 225*time.Millisecond || items[23].serviceCreditObserved {
		t.Fatalf("the lower covered pair lost real flight or timing: sent=%d total=%d timing=%+v", service.sent, service.total, timing)
	}
	rate, support, latest, proof := service.measureCapacityEvidence(time.Second, at, false)
	if rate != 53333 || latest != rate || support != kib(4) || proof != at.UnixNano() || service.serviceHoldRate != 0 {
		t.Fatalf("the lower covered 4000/75ms pair lost priority: rate=%d latest=%d support=%d proof=%d", rate, latest, support, proof)
	}
	if !service.backloggedCapacityAt(rate, proof, at) || service.backloggedCapacityAt(rate, 0, at) {
		t.Fatal("twenty real pending frames did not distinguish current-rate from held-rate flight authority")
	}
	for _, estimate := range []SendWindowEstimate{sequence.sendWindowSnapshot(at), sequence.sendWindowEstimate(at)} {
		if estimate.ServiceByteRate != rate || !estimate.ServiceBacklogged || !estimate.ServiceEstablished ||
			estimate.Window > before.Ceiling || estimate.Ceiling != before.Ceiling {
			t.Fatalf("admission lost the current covered decrease or hard permission: %+v", estimate)
		}
	}
}

// Searching beyond an accepted fallback may encounter reversed receiver
// heads. Those unselected endpoints must not reject the earlier valid pair.
func TestWindowPacingOfferFallbackSurvivesOlderReversedHead(t *testing.T) {
	start := time.Unix(1700000000, 0)
	fixture := newWindowReceiverCreditFixture(t, nil, start, 100*time.Millisecond)
	older := newWindowReceiverCreditFixture(t, fixture.service, start, 100*time.Millisecond)
	newer := newWindowReceiverCreditFixture(t, fixture.service, start, 100*time.Millisecond)
	service := fixture.service
	t.Cleanup(func() {
		for _, owner := range []*windowReceiverCreditFixture{fixture, older, newer} {
			owner.sequence.windowPacer.close()
			owner.sequence.resendQueue.Clear()
		}
		if service.sent != 6000 || service.total != 6000 || service.pendingWrites != 0 ||
			service.pacingReservations != 0 || service.reservedByteCount != 0 {
			t.Error("reversed-head fallback retained real flight")
		}
	})
	left := fixture.write(0, 2000, start.Add(20*time.Millisecond))
	right := newer.write(0, 2000, start.Add(40*time.Millisecond))
	pending := fixture.write(1, 2000, start.Add(45*time.Millisecond))
	first := older.write(0, 2000, start.Add(140*time.Millisecond))
	older.sequence.coalesceReceivedAck(older.ackWindow, older.ack(first, start.Add(150*time.Millisecond), 0))
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(left, start.Add(190*time.Millisecond), 160*time.Millisecond))
	at := start.Add(290 * time.Millisecond)
	newer.sequence.coalesceReceivedAck(newer.ackWindow, newer.ack(right, at, 190*time.Millisecond))
	var samples [3]windowServiceSample
	count := 0
	for _, sample := range service.samples {
		if sample.bytes == 0 {
			continue
		}
		count++
		switch sample.lastAtNanos {
		case start.Add(150 * time.Millisecond).UnixNano():
			samples[0] = sample
		case start.Add(190 * time.Millisecond).UnixNano():
			samples[1] = sample
		case at.UnixNano():
			samples[2] = sample
		}
	}
	if count != 3 || samples[0].receiverLastAtNanos != start.Add(150*time.Millisecond).UnixNano() ||
		samples[1].receiverLastAtNanos != start.Add(30*time.Millisecond).UnixNano() ||
		samples[2].receiverLastAtNanos != start.Add(100*time.Millisecond).UnixNano() ||
		service.bucketInterval != 25*time.Millisecond || service.total != 6000 || service.sent != 8000 ||
		service.feedbackPending || service.drained || service.receiverHeldPrefixAtNanos != 0 || pending.serviceCreditObserved {
		t.Fatalf("the explicit receiver reversal or owned tail changed: samples=%+v", samples)
	}
	for _, sample := range samples {
		if sample.bytes != 2000 || sample.receiverBytes != sample.bytes || !sample.supply.complete || !sample.supply.headComplete {
			t.Fatalf("a single confirmed head lost its paired timing: %+v", sample)
		}
	}
	nearest := samples[2].supply
	nearest.maximumGap = 100 * time.Millisecond
	nearest.receiverMaximumGap = 70 * time.Millisecond
	if nearest.qualifiedSpan(int64(70*time.Millisecond), samples[1].receiverLastAtNanos, samples[1].supply.headLastMinAtNanos, service.bucketInterval, true) != 0 ||
		nearest.continuousOfferSpan(int64(70*time.Millisecond), samples[1].supply.headLastMinAtNanos, samples[1].supply.latestSentNanos, service.bucketInterval) != int64(70*time.Millisecond) {
		t.Fatal("the nearest pair no longer requires continuous offer evidence")
	}
	for _, retain := range []bool{false, true, false, true} {
		rate, support, latest, proof := service.measureCapacityEvidence(time.Second, at, retain)
		if rate != 28571 || latest != rate || support != kib(4) || proof != 0 {
			t.Fatalf("an older reversed head poisoned the accepted fallback: retain=%t rate=%d latest=%d support=%d proof=%d", retain, rate, latest, support, proof)
		}
		if retain && service.serviceHoldRate != rate {
			t.Fatal("controller retention lost the selected fallback")
		}
	}
}

// An older covered growth candidate can be refused by a real receiver-held
// prefix. Exploring it must not turn the accepted lower fallback into a hold.
func TestWindowPacingOfferFallbackSurvivesOlderRejectedGrowth(t *testing.T) {
	start := time.Unix(1700000000, 0)
	fixture := newWindowReceiverCreditFixture(t, nil, start, 50*time.Millisecond)
	warm := newWindowReceiverCreditFixture(t, fixture.service, start, 50*time.Millisecond)
	sibling := newWindowReceiverCreditFixture(t, fixture.service, start, 50*time.Millisecond)
	sequence, service := fixture.sequence, fixture.service
	sequence.sendBufferSettings = DefaultSendBufferSettings()
	sequence.deliveredBytes = make([]deliveredBytesSample, deliveredBytesRingSize)
	sequence.contractMultiRouteWriter = &windowPacingPolicyWriter{policy: transferFlightPolicySnapshot{h1Only: true}}
	sequence.observeReceiveWindowAdvertisement(receiveAckMessage{receiveWindowSet: true, receiveWindowByteCount: 2 * 1024 * 1024})
	t.Cleanup(func() {
		for _, owner := range []*windowReceiverCreditFixture{fixture, warm, sibling} {
			owner.sequence.windowPacer.close()
			owner.sequence.resendQueue.Clear()
		}
		if service.sent != 15000 || service.total != 15000 || service.pendingWrites != 0 ||
			service.pacingReservations != 0 || service.reservedByteCount != 0 {
			t.Error("rejected-growth fallback retained real flight")
		}
	})
	var opening [6]*sendItem
	for number := range opening {
		opening[number] = warm.write(uint64(number), 1000, start.Add(50*time.Millisecond))
	}
	var items [9]*sendItem
	for number := 0; number < 5; number++ {
		items[number] = fixture.write(uint64(number), 1000, start.Add(175*time.Millisecond))
	}
	for number := 5; number < 8; number++ {
		items[number] = fixture.write(uint64(number), 1000, start.Add(time.Duration(180+10*(number-5))*time.Millisecond))
	}
	first := sibling.write(0, 1000, start.Add(200*time.Millisecond))
	items[8] = fixture.write(8, 1000, start.Add(205*time.Millisecond))
	warm.sequence.coalesceReceivedAck(warm.ackWindow, warm.ack(opening[0], start.Add(250*time.Millisecond), 0))
	warm.sequence.coalesceReceivedAck(warm.ackWindow, warm.ack(opening[4], start.Add(300*time.Millisecond), 0))
	before := sequence.sendWindowEstimate(start.Add(300 * time.Millisecond))
	if before.ServiceByteRate != 80000 || service.serviceHoldRate != 80000 || service.heldPacingRate <= 106666 {
		t.Fatalf("real warm credit did not establish the old service: %+v", before)
	}
	warm.sequence.coalesceReceivedAck(warm.ackWindow, warm.ack(opening[5], start.Add(350*time.Millisecond), 100*time.Millisecond))
	sibling.sequence.coalesceReceivedAck(sibling.ackWindow, sibling.ack(first, start.Add(400*time.Millisecond), 0))
	sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(items[4], start.Add(425*time.Millisecond), 60*time.Millisecond))
	headAt := start.Add(475 * time.Millisecond)
	sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(items[7], headAt, 50*time.Millisecond))
	at := start.Add(700 * time.Millisecond)
	timing := service.roundTripEvidence(at)
	if service.receiverHeldPrefixAtNanos != start.Add(425*time.Millisecond).UnixNano() ||
		service.serviceHoldRate != 80000 || service.total != 15000 || service.sent != 16000 ||
		service.pendingWrites != 1 || service.feedbackPending || service.drained || items[8].serviceCreditObserved ||
		timing.minimum != 190*time.Millisecond || timing.latest != 225*time.Millisecond ||
		timing.residence+2*service.bucketInterval != 265*time.Millisecond {
		t.Fatalf("the real held-prefix rejection or expired warm peak changed: timing=%+v hold=%d prefix=%d",
			timing, service.serviceHoldRate, service.receiverHeldPrefixAtNanos)
	}
	var left, middle, right windowServiceSample
	for _, sample := range service.samples {
		switch sample.lastAtNanos {
		case start.Add(400 * time.Millisecond).UnixNano():
			left = sample
		case start.Add(425 * time.Millisecond).UnixNano():
			middle = sample
		case headAt.UnixNano():
			right = sample
		}
	}
	nearest := right.supply
	nearest.maximumGap = 50 * time.Millisecond
	covered := right
	covered.mergeSupply(middle)
	covered.supply.maximumGap = max(covered.supply.maximumGap, 25*time.Millisecond)
	if left.bytes != 1000 || middle.bytes != 5000 || right.bytes != 3000 ||
		nearest.qualifiedSpan(int64(50*time.Millisecond), 0, middle.supply.headLastMinAtNanos, service.bucketInterval, false) != 0 ||
		nearest.continuousOfferSpan(int64(50*time.Millisecond), middle.supply.headLastMinAtNanos, middle.supply.latestSentNanos, service.bucketInterval) != int64(60*time.Millisecond) ||
		covered.supply.qualifiedSpan(int64(75*time.Millisecond), 0, left.supply.headLastMinAtNanos, service.bucketInterval, false) != int64(75*time.Millisecond) {
		t.Fatal("the nearest lower bound or older covered growth no longer reaches the held prefix")
	}
	for _, retain := range []bool{false, true, false, true} {
		rate, support, latest, proof := service.measureCapacityEvidence(time.Second, at, retain)
		if rate != 50000 || latest != rate || support != kib(4) || proof != 0 {
			t.Fatalf("an older rejected growth candidate poisoned the fallback: retain=%t rate=%d latest=%d support=%d proof=%d", retain, rate, latest, support, proof)
		}
		if retain && service.serviceHoldRate != rate {
			t.Fatal("controller retention restored the unselected old hold")
		}
	}
}

// A covered within-bucket tail already contributes to the existing peak.
// Higher offer-only growth across buckets remains useful without decrease proof.
func TestWindowPacingOfferSelectionPreservesWithinBucketGrowth(t *testing.T) {
	start := time.Unix(1700000000, 0)
	fixture := newWindowReceiverCreditFixture(t, nil, start, 0)
	service := fixture.service
	t.Cleanup(func() {
		fixture.sequence.windowPacer.close()
		fixture.sequence.resendQueue.Clear()
		if service.sent != 32000 || service.total != 32000 || service.pendingWrites != 0 ||
			service.pacingReservations != 0 || service.reservedByteCount != 0 {
			t.Error("within-bucket growth retained real flight")
		}
	})
	var items [33]*sendItem
	items[0] = fixture.write(0, 1000, start)
	for number := 1; number <= 30; number++ {
		items[number] = fixture.write(uint64(number), 1000, start.Add(time.Duration(10+10*((number-1)/3))*time.Millisecond))
	}
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(items[0], start.Add(100*time.Millisecond), 0))
	items[31] = fixture.write(31, 1000, start.Add(110*time.Millisecond))
	items[32] = fixture.write(32, 1000, start.Add(115*time.Millisecond))
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(items[30], start.Add(200*time.Millisecond), 0))
	at := start.Add(205 * time.Millisecond)
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(items[31], at, 0))
	var newest windowServiceSample
	count := 0
	for _, sample := range service.samples {
		if sample.bytes > 0 {
			count++
			if sample.lastAtNanos == at.UnixNano() {
				newest = sample
			}
		}
	}
	if count != 2 || newest.firstAtNanos != start.Add(200*time.Millisecond).UnixNano() ||
		newest.bytes != 31000 || newest.firstBytes != 30000 || newest.receiverBytes != newest.bytes ||
		service.bucketInterval != 10*time.Millisecond || service.sent != 33000 || service.total != 32000 ||
		service.feedbackPending || service.drained || items[32].serviceCreditObserved {
		t.Fatalf("the covered within-bucket tail or real pending frame changed: %+v", newest)
	}
	if span := newest.supply.qualifiedSpan(int64(5*time.Millisecond), newest.receiverFirstAtNanos, newest.supply.headFirstAtNanos, service.bucketInterval, true); span != int64(5*time.Millisecond) {
		t.Fatal("the within-bucket 1000/5ms interval lost independent coverage")
	}
	across := newest.supply
	across.maximumGap = 100 * time.Millisecond
	across.receiverMaximumGap = 100 * time.Millisecond
	leftAt := start.Add(100 * time.Millisecond).UnixNano()
	if across.qualifiedSpan(int64(105*time.Millisecond), leftAt, leftAt, service.bucketInterval, true) != 0 ||
		across.continuousOfferSpan(int64(105*time.Millisecond), leftAt, start.UnixNano(), service.bucketInterval) != int64(110*time.Millisecond) ||
		newest.firstQueued || newest.queued {
		t.Fatal("the cross-bucket growth no longer has an independent within-bucket positive control")
	}
	rate, support, latest, proof := service.measureCapacityEvidence(time.Second, at, false)
	if rate != 281818 || latest != 200000 || support != kib(4) || proof != 0 || service.serviceHoldRate != 0 {
		t.Fatalf("within-bucket coverage suppressed useful offer-only growth: rate=%d latest=%d support=%d proof=%d", rate, latest, support, proof)
	}
	for _, retain := range []bool{true, false, true} {
		got, gotSupport, gotLatest, gotProof := service.measureCapacityEvidence(time.Second, at, retain)
		if got != rate || gotLatest != latest || gotSupport != support || gotProof != proof || service.serviceHoldRate != rate {
			t.Fatalf("retention changed within-bucket growth: retain=%t rate=%d latest=%d support=%d proof=%d", retain, got, gotLatest, gotSupport, gotProof)
		}
	}
}
