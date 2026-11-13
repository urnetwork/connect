// A new offer-only pair must not hide an older covered rate or tighten the
// physical reservation while admission deliberately retains its higher pace.
package connect

import (
	"testing"
	"time"
)

// Two real sibling sequences allow ACK arrival order to differ from receiver
// head order. The pending original keeps every checkpoint short of a drain.
func windowOfferSelectionFixture(t *testing.T, nearestWait time.Duration) (*windowReceiverCreditFixture, time.Time, SendWindowEstimate) {
	t.Helper()
	const frameBytes ByteCount = 1000
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
		if service.sent != service.total || service.total != 9*frameBytes || service.pendingWrites != 0 ||
			service.pacingReservations != 0 || service.reservedByteCount != 0 {
			t.Error("closing the two exact owners retained physical flight or a synthetic reservation")
		}
	})
	var items [9]*sendItem
	for number := 0; number < 5; number++ {
		items[number] = fixture.write(uint64(number), frameBytes, start.Add(175*time.Millisecond))
	}
	for number := 5; number < 8; number++ {
		items[number] = fixture.write(uint64(number), frameBytes, start.Add(time.Duration(180+10*(number-5))*time.Millisecond))
	}
	first := sibling.write(0, frameBytes, start.Add(200*time.Millisecond))
	items[8] = fixture.write(8, frameBytes, start.Add(205*time.Millisecond))
	firstAt, nearestAt, at := start.Add(400*time.Millisecond), start.Add(425*time.Millisecond), start.Add(475*time.Millisecond)
	sibling.sequence.coalesceReceivedAck(sibling.ackWindow, sibling.ack(first, firstAt, 0))
	sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(items[4], nearestAt, nearestWait))
	before := sequence.sendWindowEstimate(nearestAt)
	if before.ServiceByteRate != 0 || before.ServiceEstablished || before.PacingByteRate <= 106666 ||
		!before.PacingDiscovery || before.ServiceBacklogged || service.heldPacingRate != before.PacingByteRate {
		t.Fatalf("ordinary admission did not hold the pre-measurement pace: %+v held=%d", before, service.heldPacingRate)
	}
	sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(items[7], at, 50*time.Millisecond))
	var samples [3]windowServiceSample
	count := 0
	for _, sample := range service.samples {
		if sample.bytes == 0 {
			continue
		}
		count++
		switch sample.lastAtNanos {
		case firstAt.UnixNano():
			samples[0] = sample
		case nearestAt.UnixNano():
			samples[1] = sample
		case at.UnixNano():
			samples[2] = sample
		default:
			t.Fatalf("unexpected physical checkpoint: %+v", sample)
		}
	}
	for index, sample := range samples {
		if sample.firstAtNanos != sample.lastAtNanos || !sample.supply.recorded || !sample.supply.complete ||
			!sample.supply.headComplete || sample.supply.minimumPath != 200*time.Millisecond || sample.supply.lastQueue != 0 {
			t.Fatalf("checkpoint %d lost exact bounded credit: %+v", index, sample)
		}
	}
	timing := service.roundTripEvidence(at)
	if count != 3 || samples[0].bytes != frameBytes || samples[1].bytes != 5*frameBytes || samples[2].bytes != 3*frameBytes ||
		samples[0].receiverBytes != frameBytes || samples[1].receiverBytes != 0 || samples[2].receiverBytes != 0 ||
		samples[0].supply.headLastMinAtNanos != firstAt.UnixNano() ||
		samples[1].supply.headLastMinAtNanos != nearestAt.Add(-nearestWait).UnixNano() ||
		samples[2].supply.headLastMinAtNanos != at.Add(-50*time.Millisecond).UnixNano() ||
		samples[1].supply.latestSentNanos != start.Add(175*time.Millisecond).UnixNano() ||
		items[5].pacingSentAtNanos != start.Add(180*time.Millisecond).UnixNano() ||
		samples[2].supply.latestSentNanos != start.Add(200*time.Millisecond).UnixNano() ||
		timing.minimum != 200*time.Millisecond || timing.latest != 225*time.Millisecond ||
		service.bucketInterval != 12500*time.Microsecond || !service.queueObservedAt.IsZero() ||
		service.heldPacingRate != before.PacingByteRate || service.serviceHoldRate != 0 ||
		service.sent != 10*frameBytes || service.total != 9*frameBytes ||
		sequence.windowPacer.serviceAcked != 8*frameBytes || sibling.sequence.windowPacer.serviceAcked != frameBytes ||
		service.pendingWrites != 1 || service.drained || service.feedbackPending || service.drainServiceEpoch ||
		service.receiverHeldPrefixAtNanos != 0 || items[8].serviceCreditObserved || service.probeSent != 0 {
		t.Fatalf("the paired heads, independent timing or real tail changed: samples=%+v timing=%+v sent=%d total=%d pending=%d held=%d",
			samples, timing, service.sent, service.total, service.pendingWrites, service.heldPacingRate)
	}
	// Raw 50 ms exceeds the 25 ms arrival-gap allowance. A 50 ms wait
	// puts the nearest head before these offers' 200 ms path boundary.
	nearestSupply := samples[2].supply
	nearestSupply.maximumGap = 50 * time.Millisecond
	wantNearestSpan := int64(0)
	if nearestWait == 25*time.Millisecond {
		wantNearestSpan = int64(50 * time.Millisecond)
	}
	if span := nearestSupply.qualifiedSpan(int64(50*time.Millisecond), 0, samples[1].supply.headLastMinAtNanos, service.bucketInterval, false); span != wantNearestSpan {
		t.Fatalf("nearest independent coverage = %s, want %s", time.Duration(span), time.Duration(wantNearestSpan))
	}
	// Extending to the first sibling adds five owned frames, excludes its
	// own one-frame checkpoint, and proves all selected offers by 200 ms.
	older := samples[2]
	older.mergeSupply(samples[1])
	older.supply.maximumGap = max(older.supply.maximumGap, nearestAt.Sub(firstAt))
	if span := older.supply.qualifiedSpan(int64(75*time.Millisecond), 0, samples[0].supply.headLastMinAtNanos, service.bucketInterval, false); span != int64(75*time.Millisecond) {
		t.Fatalf("older exact-head coverage = %s, want 75ms: %+v", time.Duration(span), older.supply)
	}
	return fixture, at, before
}

// The nearest offer-only 3000/50 ms lower bound is 60000 B/s; the older
// independently covered 8000/75 ms pair remains 106666 B/s after truncation.
func TestWindowPacingOfferOnlyPairKeepsCoveredBurst(t *testing.T) {
	fixture, at, before := windowOfferSelectionFixture(t, 50*time.Millisecond)
	service := fixture.service
	rate, supported, latest, proof := service.measureCapacityEvidence(time.Second, at, false)
	snapshot := fixture.sequence.sendWindowSnapshot(at)
	if service.serviceHoldRate != 0 || service.heldPacingRate != before.PacingByteRate {
		t.Fatal("a statistics read changed the retained service or admission")
	}
	after := fixture.sequence.sendWindowEstimate(at)
	if rate != 106666 || latest != 106666 || supported != kib(4) || proof != at.UnixNano() ||
		snapshot.ServiceByteRate != 106666 || after.ServiceByteRate != 106666 {
		t.Errorf("offer-only selection hid the covered pair: current=%d latest=%d support=%d proof=%d snapshot=%d admission=%d",
			rate, latest, supported, proof, snapshot.ServiceByteRate, after.ServiceByteRate)
	}
	for _, estimate := range []SendWindowEstimate{snapshot, after} {
		if !estimate.ServiceEstablished || estimate.ServiceBacklogged || !estimate.PacingDiscovery ||
			estimate.PacingByteRate != before.PacingByteRate || estimate.Window != before.Window || estimate.Ceiling != before.Ceiling {
			t.Errorf("selection changed the held pace or hard permission: before=%+v after=%+v", before, estimate)
		}
	}
	// Exercise the production reservation without the optional opening probe.
	// Cancel only this unmaterialized reservation; original flight is untouched.
	waiter := windowPacingWaiter{holdCompressionBurst: windowPacingHoldCompressionBurst(after, fixture.sequence.ackCompressionResidence())}
	deadline := service.reserve(at, 1000, after.PacingByteRate, after.ServiceByteRate, 0, 0, false, &waiter)
	defer releaseOpeningCreditReservation(service, &waiter, 1000, false)
	if service.burstMeter.limit != 1333 || service.feedbackBurstByteCount != 1333 ||
		service.burstEstimateTime != 12500*time.Microsecond || !deadline.Equal(at) {
		t.Errorf("lower-bound selection tightened the physical burst despite held pace: bytes=%d feedback=%d interval=%s deadline=%s held=%d",
			service.burstMeter.limit, service.feedbackBurstByteCount, service.burstEstimateTime, deadline.Sub(at), service.heldPacingRate)
	}
	if service.heldPacingRate != before.PacingByteRate || service.burstMeter.rate != before.PacingByteRate ||
		waiter.serialization != windowPacingSerializationTime(1000, before.PacingByteRate) ||
		service.sent != 11000 || service.total != 9000 || service.reservedByteCount != 1000 ||
		service.pacingReservations != 1 || service.probeSent != 0 {
		t.Fatal("the exact reservation lost its independent pace, probe or byte ownership")
	}
}

// A genuinely covered nearest pair retains its existing priority even when
// extending it could report a higher average. This is not a scan-all-rates rule.
func TestWindowPacingCoveredNearestPairKeepsPriority(t *testing.T) {
	fixture, at, before := windowOfferSelectionFixture(t, 25*time.Millisecond)
	service := fixture.service
	rate, supported, latest, proof := service.measureCapacityEvidence(time.Second, at, false)
	after := fixture.sequence.sendWindowEstimate(at)
	if rate != 60000 || latest != 60000 || supported != kib(4) || proof != at.UnixNano() ||
		after.ServiceByteRate != 60000 || after.ServiceBacklogged || after.PacingByteRate != before.PacingByteRate {
		t.Fatalf("a covered nearest interval lost current decrease proof or priority: rate=%d latest=%d support=%d proof=%d estimate=%+v",
			rate, latest, supported, proof, after)
	}
	waiter := windowPacingWaiter{holdCompressionBurst: windowPacingHoldCompressionBurst(after, fixture.sequence.ackCompressionResidence())}
	deadline := service.reserve(at, 1000, after.PacingByteRate, after.ServiceByteRate, 0, 0, false, &waiter)
	defer releaseOpeningCreditReservation(service, &waiter, 1000, false)
	if service.burstMeter.limit != 1000 || service.feedbackBurstByteCount != 1000 ||
		service.burstEstimateTime != 16666667*time.Nanosecond || deadline.Sub(at) != waiter.serialization ||
		service.burstMeter.rate != before.PacingByteRate || service.probeSent != 0 {
		t.Errorf("covered low service lost its one-message quantization: bytes=%d feedback=%d interval=%s deadline=%s serialization=%s",
			service.burstMeter.limit, service.feedbackBurstByteCount, service.burstEstimateTime, deadline.Sub(at), waiter.serialization)
	}
}
