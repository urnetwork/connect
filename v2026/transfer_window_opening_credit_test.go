// Ended discovery must reach physical reservation and dispatch, while cold
// and explicitly restarted services keep their bounded unspent allowance.
package connect

import (
	"testing"
	"time"
)

// Five confirmed originals leave three pending after two covered ACK heads.
// The fixed-window reader shares this physical owner's service, not its queue.
func newOpeningCreditQueuedFixture(t *testing.T, covered bool) (*SendSequence, time.Time, SendWindowEstimate) {
	t.Helper()
	sequence, offerAt := newWindowFixedPacingFixture(t)
	service := sequence.windowPacer.service
	before := sequence.sendWindowEstimate(offerAt)
	if before.Window != mib(2) || !before.PacingDiscovery || before.ServiceBacklogged {
		t.Fatal("fixture lost its original fixed permission or unspent discovery")
	}
	fixture := newWindowReceiverCreditFixture(t, service, offerAt, 10*time.Millisecond)
	fixture.sequence.sendBufferSettings = sequence.sendBufferSettings
	priorTotal := service.total
	t.Cleanup(func() {
		fixture.sequence.windowPacer.close()
		fixture.sequence.resendQueue.Clear()
		if service.total != priorTotal+20000 || service.sent != service.total ||
			service.pendingWrites != 0 || service.pacingReservations != 0 || service.reservedByteCount != 0 {
			t.Error("physical owner or synthetic reservation retained flight after cleanup")
		}
	})
	var originals [5]*sendItem
	for number := range originals {
		originals[number] = fixture.write(uint64(number), 10000, offerAt)
	}
	if covered {
		at := offerAt.Add(21 * time.Millisecond)
		fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(originals[0], at, 10*time.Millisecond))
	}
	at := offerAt.Add(31 * time.Millisecond)
	ack := fixture.ack(originals[1], at, 10*time.Millisecond)
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, ack)
	timing := service.roundTripEvidence(at)
	var newest windowServiceSample
	for _, sample := range service.samples {
		if sample.lastAtNanos == at.UnixNano() {
			newest = sample
		}
	}
	wantBytes, wantReceiverBytes := ByteCount(20000), ByteCount(0)
	if covered {
		wantBytes, wantReceiverBytes = 10000, 10000
	}
	if timing.minimum != time.Millisecond || timing.latestRaw != 31*time.Millisecond ||
		timing.latest != 21*time.Millisecond || timing.residence != 11*time.Millisecond ||
		newest.bytes != wantBytes || newest.receiverBytes != wantReceiverBytes || !newest.queued ||
		!newest.supply.complete || !newest.supply.headComplete || newest.supply.latestSentNanos != offerAt.UnixNano() ||
		service.total-priorTotal != 20000 || fixture.sequence.windowPacer.serviceAcked != 20000 ||
		service.sent-service.total != 30000 || service.sent-priorTotal > before.Window ||
		service.drained || service.feedbackPending != !covered || service.heldPacingRate != before.PacingByteRate ||
		originals[4].serviceCreditObserved {
		t.Fatalf("covered=%t: credit or timing changed: timing=%+v newest=%+v delivered=%d owner-acked=%d flight=%d drained=%t pending=%t held=%d prior=%d tail-credited=%t",
			covered, timing, newest, service.total-priorTotal, fixture.sequence.windowPacer.serviceAcked,
			service.sent-service.total, service.drained, service.feedbackPending, service.heldPacingRate,
			before.PacingByteRate, originals[4].serviceCreditObserved)
	}
	// Without ACK0, the 31 ms gap exceeds the 10+2*10 ms feedback bound.
	// Its one arrival has no elapsed cycle; only the earlier hold remains.
	if !covered && (!service.feedbackCycleBefore.Equal(offerAt) || service.feedbackInterval != 10*time.Millisecond ||
		service.feedbackCycle.firstAtNanos != at.UnixNano() || service.feedbackCycle.lastAtNanos != at.UnixNano() ||
		service.feedbackCycle.bytes != 20000 || service.feedbackCycle.firstBytes != 20000 ||
		!service.feedbackDrainAt.IsZero() || service.feedbackDrainPending != 0) {
		t.Fatalf("the single pending prefix changed its boundary: before=%s interval=%s cycle=%+v drain=%s/%d",
			service.feedbackCycleBefore, service.feedbackInterval, service.feedbackCycle, service.feedbackDrainAt, service.feedbackDrainPending)
	}
	flight, currentBound, heldBound := func() (float64, float64, float64) {
		service.stateLock.Lock()
		defer service.stateLock.Unlock()
		return service.outstandingWithLock(), service.flightBoundAtWithLock(1000000, timing.residence),
			service.flightBoundAtWithLock(before.PacingByteRate, timing.residence)
	}()
	if flight <= currentBound || heldBound <= flight {
		t.Fatalf("flight did not isolate current-rate authority: flight=%f current=%f held=%f", flight, currentBound, heldBound)
	}
	rate, _, latest := service.measure(time.Second, at, false)
	wantCurrent := ByteCount(0)
	if covered {
		wantCurrent = 1000000
	}
	if rate != wantCurrent || latest != 1000000 {
		t.Fatalf("covered=%t: delivery selection=%d/%d, want %d/1000000", covered, rate, latest, wantCurrent)
	}
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, ack)
	if service.total != priorTotal+20000 || fixture.sequence.windowPacer.serviceAcked != 20000 ||
		originals[4].serviceCreditObserved {
		t.Fatal("duplicate cumulative publication changed exact owner credit")
	}
	return sequence, at, before
}

// A current covered pair qualifies the unchanged adaptive reservation price.
// Equal-rate old peaks cannot replace that pair's newer arrival endpoint.
func newOpeningCreditCongestedFixture(t *testing.T) (*SendSequence, time.Time, SendWindowEstimate) {
	t.Helper()
	sequence, at, before := newOpeningCreditQueuedFixture(t, true)
	service := sequence.windowPacer.service
	estimate := sequence.sendWindowEstimate(at)
	if estimate.PacingDiscovery || !estimate.ServiceEstablished || !estimate.ServiceBacklogged ||
		estimate.ServiceByteRate != 1000000 || estimate.PacingByteRate != 950000 {
		t.Fatalf("fixture did not establish congested adaptive service: %+v", estimate)
	}
	if estimate.PacingProbeByteRate <= estimate.PacingByteRate || estimate.PacingProbeByteCount <= 12000 ||
		service.probeSent != 0 {
		t.Fatal("fixture lost its unused faster opening allowance")
	}
	if estimate.Window != before.Window || estimate.Ceiling != before.Ceiling ||
		estimate.Initial != before.Initial || estimate.LearnedWindow != before.LearnedWindow {
		t.Fatal("queue evidence changed hard byte permission or retained window")
	}
	return sequence, at, estimate
}

// The identical twenty-ms queue ends discovery but does not clear its hold.
// Its incomplete feedback cycle retains old service without current proof.
func TestWindowOpeningCreditStalePrefixKeepsAdmittedPace(t *testing.T) {
	sequence, at, before := newOpeningCreditQueuedFixture(t, false)
	service := sequence.windowPacer.service
	snapshot := sequence.sendWindowSnapshot(at)
	after := sequence.sendWindowEstimate(at)
	for _, estimate := range []SendWindowEstimate{snapshot, after} {
		if estimate.PacingDiscovery || !estimate.ServiceEstablished || estimate.ServiceBacklogged ||
			estimate.ServiceByteRate != 1000000 || estimate.PacingByteRate != before.PacingByteRate ||
			estimate.Window != before.Window || estimate.Ceiling != before.Ceiling ||
			estimate.Initial != before.Initial || estimate.LearnedWindow != before.LearnedWindow {
			t.Errorf("an uncovered queued prefix revoked admission or byte permission: %+v", estimate)
		}
	}
	if service.heldPacingRate != before.PacingByteRate || service.probeSent != 0 {
		t.Fatal("statistics or admission changed the retained grant or opening credit")
	}
}

// Synthetic reservations never become physical writes. Cancel their local
// flight ownership without refunding meter, probe, or serialization debt.
func releaseOpeningCreditReservation(service *windowPacingService, waiter *windowPacingWaiter, bytes ByteCount, resend bool) {
	service.stateLock.Lock()
	defer service.stateLock.Unlock()
	service.removeWaiterWithLock(waiter)
	service.pacingReservations--
	if !resend {
		service.reservedByteCount -= bytes
		service.sent -= bytes
	}
}

// Copy the same estimate fields and burst choice used by paceWrite.
func reserveOpeningCredit(sequence *SendSequence, at time.Time, estimate SendWindowEstimate, bytes int, resend bool) (*windowPacingWaiter, time.Time) {
	waiter := &windowPacingWaiter{
		holdCompressionBurst: windowPacingHoldCompressionBurst(estimate, sequence.ackCompressionResidence()),
	}
	deadline := sequence.windowPacer.service.reserve(
		at, bytes, estimate.PacingByteRate, estimate.ServiceByteRate,
		estimate.PacingProbeByteRate, estimate.PacingProbeByteCount, resend, waiter,
	)
	return waiter, deadline
}

// Qualified congestion prices the next reservation at the adaptive pace.
func TestWindowOpeningCreditQueuedReservationUsesAdaptivePrice(t *testing.T) {
	sequence, at, estimate := newOpeningCreditCongestedFixture(t)
	service := sequence.windowPacer.service
	waiter, _ := reserveOpeningCredit(sequence, at, estimate, 4000, false)
	defer releaseOpeningCreditReservation(service, waiter, 4000, false)
	// 4000 bytes at 950000 B/s, independently truncated to integer ns.
	const wantSerialization = 4210526 * time.Nanosecond
	if waiter.serialization != wantSerialization || service.burstMeter.rate != 950000 || service.probeSent != 0 {
		t.Errorf("ended discovery still spent opening credit: serialization=%s refill=%d probe=%d; want %s/950000/0",
			waiter.serialization, service.burstMeter.rate, service.probeSent, wantSerialization)
	}
}

// Actual release obeys the same byte/time envelope as the estimator.
func TestWindowOpeningCreditQueuedDispatchHonorsAdaptiveEnvelope(t *testing.T) {
	sequence, at, estimate := newOpeningCreditCongestedFixture(t)
	service := sequence.windowPacer.service
	start := at
	for index := range 3 {
		waiter, deadline := reserveOpeningCredit(sequence, at, estimate, 4000, false)
		if at.Before(deadline) {
			at = deadline
		}
		admitted := false
		for range 8 {
			delay, _ := service.admitBurst(at, 4000, false, waiter)
			if delay <= 0 {
				admitted = true
				break
			}
			at = at.Add(delay)
		}
		releaseOpeningCreditReservation(service, waiter, 4000, false)
		if !admitted {
			t.Fatalf("reservation %d failed to reach its finite dispatch boundary", index)
		}
		// The 1 MB/s service's 2.5 ms dispatch interval is smaller than
		// this indivisible 4000-byte message: one whole message is allowed.
		// Every later byte must be paid at the admitted 950000 B/s rate.
		elapsed := at.Sub(start)
		// Each integer serialization truncates by less than one ns.
		neededByteNanos := int64((index+1)*4000-4000) * int64(time.Second)
		earnedByteNanos := int64(950000) * (int64(elapsed) + int64(index+1))
		if neededByteNanos > earnedByteNanos {
			t.Errorf("congested dispatch exceeded adaptive envelope: prefix=%d bytes=%d elapsed=%s required-byte-ns=%d earned-byte-ns=%d",
				index+1, (index+1)*4000, elapsed, neededByteNanos, earnedByteNanos)
		}
	}
}

// Recovery ends discovery before pricing that very same reservation.
func TestWindowOpeningCreditRecoveryEndsProbeWithinReservation(t *testing.T) {
	service, at := newWindowQualifiedServiceFixture(t, 1000000, 10*time.Millisecond, time.Millisecond)
	if discovering, _ := service.pacingHold(); !discovering {
		t.Fatal("fixture did not begin in discovery")
	}
	waiter := &windowPacingWaiter{holdCompressionBurst: true}
	service.reserve(at, 4000, 1100000, 1000000, 10000000, 15000, true, waiter)
	defer releaseOpeningCreditReservation(service, waiter, 4000, true)
	if discovering, held := service.pacingHold(); discovering || held != 0 {
		t.Fatal("recovery reservation failed to end discovery")
	}
	// Recovery must use the supplied current pace, not target-rate credit.
	if waiter.serialization != 3636363*time.Nanosecond || service.burstMeter.rate != 1100000 || service.probeSent != 0 {
		t.Errorf("recovery ended discovery but retained opening credit: serialization=%s refill=%d probe=%d",
			waiter.serialization, service.burstMeter.rate, service.probeSent)
	}
}

// Neither missing nor fast unqueued evidence throttles a bounded opening.
func TestWindowOpeningCreditColdAndFastServiceKeepRemainingProbe(t *testing.T) {
	for _, qualified := range []bool{false, true} {
		service := newWindowPacingService(DefaultSendBufferSettings())
		at := time.Unix(1700000000, 0)
		if qualified {
			service, at = newWindowQualifiedServiceFixture(t, 125000000, 10*time.Millisecond, time.Millisecond)
		}
		service.probeSent = 5000
		waiter := &windowPacingWaiter{holdCompressionBurst: true}
		service.reserve(at, 4000, 137500000, 125000000, 150000000, 15000, false, waiter)
		releaseOpeningCreditReservation(service, waiter, 4000, false)
		if discovering, _ := service.pacingHold(); !discovering || service.probeSent != 9000 ||
			waiter.serialization != 26666*time.Nanosecond || service.burstMeter.rate != 150000000 {
			t.Errorf("qualified=%t: uncongested opening was throttled: serialization=%s probe=%d refill=%d",
				qualified, waiter.serialization, service.probeSent, service.burstMeter.rate)
		}
	}
}

// Ending the target probe does not erase a previously admitted held pace.
func TestWindowOpeningCreditEndedDiscoveryPreservesHeldAdaptiveRate(t *testing.T) {
	sequence, at := newWindowFixedPacingFixture(t)
	service := sequence.windowPacer.service
	at = at.Add(time.Millisecond)
	service.observeReceiverRoundTrip(0, 31*time.Millisecond, 21*time.Millisecond, 10*time.Millisecond, at)
	at = at.Add(time.Millisecond)
	service.observeReceiverRoundTrip(0, 11*time.Millisecond, time.Millisecond, 10*time.Millisecond, at)
	estimate := sequence.sendWindowEstimate(at)
	if estimate.PacingDiscovery || estimate.ServiceBacklogged || estimate.PacingByteRate != 1100000 {
		t.Fatalf("fixture lost recovered held rate: %+v", estimate)
	}
	waiter, _ := reserveOpeningCredit(sequence, at, estimate, 4000, false)
	defer releaseOpeningCreditReservation(service, waiter, 4000, false)
	if waiter.serialization != 3636363*time.Nanosecond || service.burstMeter.rate != 1100000 {
		t.Errorf("ended discovery bypassed held adaptive rate: serialization=%s refill=%d",
			waiter.serialization, service.burstMeter.rate)
	}
	if _, held := service.pacingHold(); held != 1100000 {
		t.Fatalf("reservation changed the retained adaptive pace: %d", held)
	}
}

// A real quiet-service transition reopens only the historical remainder.
func TestWindowOpeningCreditQuietRestartDoesNotReplenishAllowance(t *testing.T) {
	service, at := newWindowQualifiedServiceFixture(t, 1000000, 10*time.Millisecond, time.Millisecond)
	service.probeSent = 5000
	at = at.Add(time.Millisecond)
	service.observeReceiverRoundTrip(0, 31*time.Millisecond, 21*time.Millisecond, 10*time.Millisecond, at)
	if discovering, _ := service.pacingHold(); discovering {
		t.Fatal("queue observation did not end discovery")
	}
	at = at.Add(70 * time.Second)
	service.observeRoundTrip(11*time.Millisecond, 10*time.Millisecond, at)
	if discovering, _ := service.pacingHold(); !discovering {
		t.Fatal("the explicit quiet-service transition did not reopen discovery")
	}
	for index := range 2 {
		waiter := &windowPacingWaiter{holdCompressionBurst: true}
		service.reserve(at, 4000, 1000000, 1000000, 10000000, 10000, false, waiter)
		releaseOpeningCreditReservation(service, waiter, 4000, false)
		want := []time.Duration{400 * time.Microsecond, 3100 * time.Microsecond}[index]
		if waiter.serialization != want {
			t.Errorf("quiet restart reservation %d repriced or replenished remaining credit: %s want %s", index, waiter.serialization, want)
		}
		at = at.Add(time.Second)
	}
	if service.probeSent != 10000 {
		t.Fatalf("quiet restart changed the historical one-shot bound: %d", service.probeSent)
	}
}

// A sibling cannot escape the common service's ended-discovery state.
func TestWindowOpeningCreditCongestionBelongsToSharedService(t *testing.T) {
	sequence, at, estimate := newOpeningCreditCongestedFixture(t)
	service := sequence.windowPacer.service
	other := &SendSequence{sendBufferSettings: sequence.sendBufferSettings, windowPacer: windowBurstPacer{service: service}}
	for index, owner := range []*SendSequence{sequence, other} {
		waiter, _ := reserveOpeningCredit(owner, at, estimate, 4000, false)
		releaseOpeningCreditReservation(service, waiter, 4000, false)
		if waiter.serialization != 4210526*time.Nanosecond || service.probeSent != 0 {
			t.Errorf("sibling %d escaped shared ended-discovery state: serialization=%s probe=%d", index, waiter.serialization, service.probeSent)
		}
	}
}
