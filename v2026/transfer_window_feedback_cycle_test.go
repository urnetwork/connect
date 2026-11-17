// A long feedback gap starts a partial cycle, not a new serialization rate.
// Actual ACK timestamps complete it independently of the previously held rate.
package connect

import (
	"testing"
	"time"
)

// Returns a synthetic, continuously owned service with a measured 10 MB/s
// train and enough physical flight for the next ACK to be only a partial reply.
func newPartialFeedbackCycleFixture(t *testing.T) (*windowPacingService, time.Time) {
	t.Helper()
	start := time.Unix(1700000000, 0)
	service := &windowPacingService{sent: 100000000, minRoundTrip: 5 * time.Millisecond,
		latestRoundTrip: 5 * time.Millisecond, lastRoundTrip: start,
		compression: 10 * time.Millisecond, bucketInterval: 10 * time.Millisecond,
		maxMessageByteCount: 1000, burstMeter: windowPacingBurstMeter{limit: 100000}}
	service.observe(1, start)
	service.observe(100000, start.Add(10*time.Millisecond))
	service.observe(100000, start.Add(20*time.Millisecond))
	if rate, _, _ := service.measured(time.Second, start.Add(20*time.Millisecond)); rate != 10000000 {
		t.Fatalf("initial train rate=%d, want 10000000", rate)
	}
	return service, start
}

// Every interval can be a long gap on a genuinely slow serializer. The next
// ACK completes the pending cycle before it can open another one indefinitely.
func TestWindowPacingFeedbackCycleAllowsRepeatedSlowGaps(t *testing.T) {
	service, start := newPartialFeedbackCycleFixture(t)
	for i := 1; i <= 4; i++ {
		at := start.Add(time.Duration(i) * 100 * time.Millisecond)
		service.observe(1000, at)
		rate, total, latest := service.measured(time.Second, at)
		want := ByteCount(10000)
		if i == 1 {
			want = 10000000
		}
		if got := max(rate, latest); got != want || total != 200001+ByteCount(i)*1000 {
			t.Fatalf("gap=%d rate=%d/%d total=%d, want %d/%d", i, rate, latest, total, want, 200001+ByteCount(i)*1000)
		}
	}
}

// A completed compression turn accounts for all bytes and the real preceding
// gap when queued residence proves those timestamps belong to sustained service.
func TestWindowPacingFeedbackCycleKeepsFullGap(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		service, start := newPartialFeedbackCycleFixture(t)
		service.observeRoundTrip(200*time.Millisecond, 10*time.Millisecond, start.Add(100*time.Millisecond))
		arrivals := []struct {
			at    time.Duration
			bytes ByteCount
		}{{at: 100 * time.Millisecond, bytes: 1000}, {at: 110 * time.Millisecond, bytes: 9000}}
		if reverse {
			arrivals[0], arrivals[1] = arrivals[1], arrivals[0]
		}
		service.observe(arrivals[0].bytes, start.Add(arrivals[0].at))
		if rate, _, latest := service.measured(time.Second, start.Add(110*time.Millisecond)); max(rate, latest) != 10000000 {
			t.Fatalf("reverse=%t: partial cycle replaced service with %d/%d", reverse, rate, latest)
		}
		service.observe(arrivals[1].bytes, start.Add(arrivals[1].at))
		rate, total, _ := service.measured(time.Second, start.Add(110*time.Millisecond))
		if rate != 111111 || total != 210001 {
			t.Fatalf("reverse=%t: completed cycle lost 10000 bytes / 90 ms: rate=%d total=%d", reverse, rate, total)
		}
	}
}

// A full cumulative physical tail is a complete cycle even if its gap is
// long. Cancellation or merely reading the service cannot supply that proof.
func TestWindowPacingFeedbackCycleCompletesAtPhysicalTail(t *testing.T) {
	service, start := newPartialFeedbackCycleFixture(t)
	sequenceId, messageId := NewId(), NewId()
	service.sent = service.total + 1000
	service.beginWrite(sequenceId, messageId, 1, start.Add(20*time.Millisecond), false)
	service.finishWrite(sequenceId, messageId, true)
	at := start.Add(100 * time.Millisecond)
	service.acknowledgeWrite(sequenceId, messageId, 1, false, 10*time.Millisecond, at)
	if !service.drained {
		t.Fatal("the explicit cumulative ACK did not finish the physical flight")
	}
	service.observe(1000, at)
	if rate, total, _ := service.measured(time.Second, at); rate != 12500 || total != 201001 {
		t.Fatalf("completed physical cycle lost its actual 80 ms: rate=%d total=%d", rate, total)
	}
}

// Sibling ACKs compressed inside one turn do not manufacture a short service
// interval; only their complete byte/time cycle may replace the previous hold.
func TestWindowPacingFeedbackCycleDoesNotPromoteCompressedPartials(t *testing.T) {
	service, start := newPartialFeedbackCycleFixture(t)
	service.observeRoundTrip(200*time.Millisecond, 10*time.Millisecond, start.Add(100*time.Millisecond))
	for _, sample := range []struct {
		at    time.Duration
		bytes ByteCount
	}{{at: 100 * time.Millisecond, bytes: 500}, {at: 100 * time.Millisecond, bytes: 500}, {at: 105 * time.Millisecond, bytes: 5000}} {
		service.observe(sample.bytes, start.Add(sample.at))
		if rate, _, latest := service.measured(time.Second, start.Add(sample.at)); max(rate, latest) != 10000000 {
			t.Fatalf("compressed partial changed the hold at %s: %d/%d", sample.at, rate, latest)
		}
	}
	service.observe(5000, start.Add(110*time.Millisecond))
	if rate, _, _ := service.measured(time.Second, start.Add(110*time.Millisecond)); rate != 122222 {
		t.Fatalf("compressed complete cycle invented capacity: %d, want 11000 bytes / 90 ms", rate)
	}
}

// Bucket geometry and read frequency do not decide whether actual delivery
// has finished a cycle. All accounting is done on observation timestamps.
func TestWindowPacingFeedbackCycleSurvivesResizeAndStatistics(t *testing.T) {
	for _, read := range []bool{false, true} {
		service, start := newPartialFeedbackCycleFixture(t)
		service.observe(1000, start.Add(100*time.Millisecond))
		if read {
			for range 20 {
				service.measure(time.Second, start.Add(109*time.Millisecond), false)
			}
		}
		service.stateLock.Lock()
		service.observeRoundTripWithLock(800*time.Millisecond, 10*time.Millisecond, start.Add(100*time.Millisecond), true)
		service.stateLock.Unlock()
		if rate, _, latest := service.measured(10*time.Millisecond, start.Add(109*time.Millisecond)); rate != 0 || latest != 10000000 {
			t.Fatalf("read=%t: resize or elapsed time completed a partial cycle: %d/%d", read, rate, latest)
		}
		service.observe(9000, start.Add(110*time.Millisecond))
		if rate, total, _ := service.measured(10*time.Millisecond, start.Add(110*time.Millisecond)); rate != 900000 || total != 210001 {
			t.Fatalf("read=%t: resized complete cycle lost actual delivery: rate=%d total=%d", read, rate, total)
		}
	}
}

// With no complete prior evidence there is no positive value to hold. A
// single ACK or repeated reads cannot create a service rate from silence.
func TestWindowPacingFeedbackCycleWithoutEvidenceHoldsZero(t *testing.T) {
	start := time.Unix(1700000000, 0)
	service := &windowPacingService{sent: 100000000, compression: 10 * time.Millisecond}
	service.observe(1000, start)
	for _, at := range []time.Duration{0, time.Millisecond, time.Second} {
		if rate, total, latest := service.measured(time.Second, start.Add(at)); rate != 0 || latest != 0 || total != 1000 {
			t.Fatalf("unmeasured service fabricated evidence at %s: %d/%d total=%d", at, rate, latest, total)
		}
	}
}

// A real slow cycle can outlive the ring. Its bounded first/last summary
// survives without either retaining unbounded history or waiting for old-rate bytes.
func TestWindowPacingFeedbackCycleOutlivesRing(t *testing.T) {
	for _, intermediateRead := range []bool{false, true} {
		service, start := newPartialFeedbackCycleFixture(t)
		for i := 1; i <= 4; i++ {
			at := start.Add(time.Duration(i) * time.Second)
			service.observe(1000, at)
			if i != 4 && !intermediateRead {
				continue
			}
			rate, total, latest := service.measured(time.Second, at)
			want := ByteCount(1000)
			if i == 1 {
				want = 10000000
			}
			if max(rate, latest) != want || total != 200001+ByteCount(i)*1000 {
				t.Fatalf("intermediate-read=%t second=%d: slow completed cycle retained old service: %d/%d total=%d want=%d", intermediateRead, i, rate, latest, total, want)
			}
		}
	}
}

// A summarized completed cycle cannot return after a newer continuous train
// has established slower service and subsequently aged out of the sample ring.
func TestWindowPacingFeedbackCycleCannotRestoreSupersededEvidence(t *testing.T) {
	service, start := newPartialFeedbackCycleFixture(t)
	service.observe(1000, start.Add(100*time.Millisecond))
	service.observe(9000, start.Add(110*time.Millisecond))
	service.measured(time.Second, start.Add(110*time.Millisecond))
	for at := 120 * time.Millisecond; at <= 200*time.Millisecond; at += 10 * time.Millisecond {
		service.observe(1000, start.Add(at))
	}
	if rate, _, _ := service.measured(time.Second, start.Add(200*time.Millisecond)); rate != 100000 {
		t.Fatalf("fresh slower train was not established: %d", rate)
	}
	service.observe(1000, start.Add(time.Second))
	if rate, _, latest := service.measured(time.Second, start.Add(time.Second)); max(rate, latest) != 100000 {
		t.Fatalf("old completed cycle restored superseded evidence: %d/%d", rate, latest)
	}
}

// Completion by a physical tail keeps its whole real gap even after the
// previous ACK has left the bounded ring.
func TestWindowPacingFeedbackCyclePhysicalTailOutlivesRing(t *testing.T) {
	service, start := newPartialFeedbackCycleFixture(t)
	sequenceId, messageId := NewId(), NewId()
	service.sent = service.total + 1000
	service.beginWrite(sequenceId, messageId, 1, start.Add(20*time.Millisecond), false)
	service.finishWrite(sequenceId, messageId, true)
	at := start.Add(1020 * time.Millisecond)
	service.acknowledgeWrite(sequenceId, messageId, 1, false, 10*time.Millisecond, at)
	service.observe(1000, at)
	if rate, total, _ := service.measured(time.Second, at); rate != 1000 || total != 201001 {
		t.Fatalf("physical completion lost its 1000-byte/one-second cycle: rate=%d total=%d", rate, total)
	}
}

// A cumulative tail proves delivery before the send workers apply all of its
// old bytes. That partial application cannot reprice a waiting sibling.
func TestWindowPacingFeedbackCycleWaitsForDrainedAccounting(t *testing.T) {
	for _, resumed := range []bool{false, true} {
		service, start := newPartialFeedbackCycleFixture(t)
		sequenceId, messageId := NewId(), NewId()
		service.sent = service.total + 10000
		service.beginWrite(sequenceId, messageId, 1, start.Add(20*time.Millisecond), false)
		service.finishWrite(sequenceId, messageId, true)
		at := start.Add(100 * time.Millisecond)
		service.acknowledgeWrite(sequenceId, messageId, 1, false, 10*time.Millisecond, at)
		service.observe(1000, at)
		rate, _, latest := service.measured(time.Second, at)
		if max(rate, latest) != 10000000 || service.total >= service.drainedSent {
			t.Fatalf("resumed=%t: partial drained accounting changed service: %d/%d total=%d proved=%d", resumed, rate, latest, service.total, service.drainedSent)
		}
		waiter := &windowPacingWaiter{}
		service.reserve(at, 1000, max(rate, latest), max(rate, latest), 0, 0, false, waiter)
		if waiter.serialization != 100*time.Microsecond {
			t.Fatalf("resumed=%t: a partial old cycle repriced the sibling: %s", resumed, waiter.serialization)
		}
		if resumed {
			probe := NewId()
			service.beginWrite(NewId(), probe, 1, at.Add(time.Millisecond), false)
		}
		service.observe(9000, at)
		if rate, _, _ := service.measured(time.Second, at.Add(time.Millisecond)); rate != 125000 {
			t.Fatalf("resumed=%t: accounted drained cycle lost 10000 bytes / 80 ms: %d", resumed, rate)
		}
	}
}

// New bytes cannot numerically fill the old accounting deficit, but a fresh
// pair can independently replace the hold while those old workers are delayed.
func TestWindowPacingFeedbackCycleAcceptsFreshEvidenceBeforeOldAccounting(t *testing.T) {
	service, start := newPartialFeedbackCycleFixture(t)
	sequenceId, messageId := NewId(), NewId()
	service.sent = service.total + 10000
	service.beginWrite(sequenceId, messageId, 1, start.Add(20*time.Millisecond), false)
	service.finishWrite(sequenceId, messageId, true)
	service.acknowledgeWrite(sequenceId, messageId, 1, false, 10*time.Millisecond, start.Add(100*time.Millisecond))
	service.observe(1000, start.Add(100*time.Millisecond))
	service.measured(time.Second, start.Add(100*time.Millisecond))
	service.sent += 11000
	service.beginWrite(NewId(), NewId(), 1, start.Add(101*time.Millisecond), false)
	service.observe(9000, start.Add(120*time.Millisecond))
	if rate, _, latest := service.measured(time.Second, start.Add(120*time.Millisecond)); max(rate, latest) != 10000000 {
		t.Fatalf("new bytes pretended to finish the old cycle: %d/%d", rate, latest)
	}
	service.observe(1000, start.Add(130*time.Millisecond))
	if rate, _, _ := service.measured(time.Second, start.Add(130*time.Millisecond)); rate != 100000 {
		t.Fatalf("fresh independent pair could not replace the held rate: %d", rate)
	}
	service.observe(9000, start.Add(100*time.Millisecond))
	if rate, total, latest := service.measured(time.Second, start.Add(130*time.Millisecond)); max(rate, latest) != 100000 || total != 220001 {
		t.Fatalf("late old accounting re-entered accepted fresh evidence: %d/%d total=%d", rate, latest, total)
	}
}

// A partial cycle has no new rate evidence. Changes to queued residence,
// outstanding ownership, or read mode cannot recompute a different old mean.
func TestWindowPacingFeedbackCycleHoldIgnoresFlightAndReadMode(t *testing.T) {
	for _, outstanding := range []ByteCount{0, 1000, 10000000} {
		for _, retain := range []bool{false, true} {
			service, start := newPartialFeedbackCycleFixture(t)
			service.observe(1000, start.Add(100*time.Millisecond))
			service.sent = service.total + outstanding
			service.observeRoundTrip(time.Second, 10*time.Millisecond, start.Add(101*time.Millisecond))
			for _, elapsed := range []time.Duration{101 * time.Millisecond, 500 * time.Millisecond, time.Second} {
				rate, _, latest := service.measure(time.Second, start.Add(elapsed), retain)
				if rate != 0 || latest != 10000000 || service.serviceHoldRate != 10000000 {
					t.Fatalf("outstanding=%d retain=%t elapsed=%s: pending feedback changed hold=%d/%d stored=%d", outstanding, retain, elapsed, rate, latest, service.serviceHoldRate)
				}
			}
		}
	}
}

// A cycle pins its opening compression interval. A shorter advertisement
// cannot retroactively turn that compressed train into fast immediate ACKs.
func TestWindowPacingFeedbackCycleCompressionChanges(t *testing.T) {
	for _, c := range []struct {
		before, after time.Duration
	}{{before: 10 * time.Millisecond, after: 0}, {before: 0, after: 10 * time.Millisecond}, {before: 10 * time.Millisecond, after: 50 * time.Millisecond}} {
		service, start := newPartialFeedbackCycleFixture(t)
		service.compression = c.before
		service.observe(1000, start.Add(100*time.Millisecond))
		service.observeRoundTrip(5*time.Millisecond, c.after, start.Add(100*time.Millisecond))
		service.observe(1000, start.Add(100*time.Millisecond))
		if rate, _, latest := service.measured(time.Second, start.Add(100*time.Millisecond)); max(rate, latest) != 10000000 {
			t.Fatalf("compression=%s->%s: same-time ACKs fabricated a cycle: %d/%d", c.before, c.after, rate, latest)
		}
		span := max(c.before, c.after)
		service.observe(1000, start.Add(100*time.Millisecond+span-time.Nanosecond))
		if rate, _, latest := service.measured(time.Second, start.Add(100*time.Millisecond+span-time.Nanosecond)); max(rate, latest) != 10000000 {
			t.Fatalf("compression=%s->%s: incomplete compressed interval repriced service: %d/%d", c.before, c.after, rate, latest)
		}
		service.observe(1000, start.Add(100*time.Millisecond+span))
		if rate, _, latest := service.measured(time.Second, start.Add(100*time.Millisecond+span)); max(rate, latest) <= 0 || max(rate, latest) > ByteCount(float64(2000)/span.Seconds()) {
			t.Fatalf("compression=%s->%s: full cycle fabricated short-span capacity: %d/%d", c.before, c.after, rate, latest)
		}
	}
}

// A confirmed new service epoch retires an old incomplete cycle. Delayed old
// bytes remain accounting, while the first new pair supplies independent service.
func TestWindowPacingFeedbackCycleConfirmedEpochRetiresOldPartial(t *testing.T) {
	service, start := newPartialFeedbackCycleFixture(t)
	sequenceId, tail, probe := NewId(), NewId(), NewId()
	service.sent = service.total + 10000
	service.beginWrite(sequenceId, tail, 1, start.Add(20*time.Millisecond), false)
	service.finishWrite(sequenceId, tail, true)
	service.observeRoundTrip(500*time.Millisecond, 10*time.Millisecond, start.Add(80*time.Millisecond))
	if delay, _ := service.admitBurst(start.Add(90*time.Millisecond), 1000, false, &windowPacingWaiter{}); delay <= 0 {
		t.Fatal("the actual residence observation did not request a controlled drain")
	}
	service.observe(1000, start.Add(100*time.Millisecond))
	if rate, _, latest := service.measured(time.Second, start.Add(100*time.Millisecond)); max(rate, latest) != 10000000 {
		t.Fatalf("the incomplete cycle lost the initial hold: %d/%d", rate, latest)
	}
	service.acknowledgeWrite(sequenceId, tail, 1, false, 10*time.Millisecond, start.Add(110*time.Millisecond))
	service.sent += 1000
	service.beginWrite(sequenceId, probe, 2, start.Add(110*time.Millisecond), false)
	service.finishWrite(sequenceId, probe, true)
	at := start.Add(120 * time.Millisecond)
	service.acknowledgeWrite(sequenceId, probe, 2, false, 10*time.Millisecond, at)
	service.observe(1000, at)
	service.observe(9000, start.Add(100*time.Millisecond))
	if rate, total, latest := service.measured(time.Second, at); max(rate, latest) != 10000000 || total != 211001 || service.serviceEpochAt != at {
		t.Fatalf("late old ACK changed the confirmed epoch: %d/%d total=%d epoch=%s", rate, latest, total, service.serviceEpochAt)
	}
	newTail := NewId()
	service.sent += 10000
	service.beginWrite(sequenceId, newTail, 3, at, false)
	service.finishWrite(sequenceId, newTail, true)
	service.observe(5000, start.Add(130*time.Millisecond))
	service.acknowledgeWrite(sequenceId, newTail, 3, false, 10*time.Millisecond, start.Add(140*time.Millisecond))
	service.observe(5000, start.Add(140*time.Millisecond))
	if rate, total, _ := service.measured(time.Second, start.Add(140*time.Millisecond)); rate != 500000 || total != 221001 || service.serviceEpochAt != at {
		t.Fatalf("new epoch did not accept its own slower pair: rate=%d total=%d epoch=%s", rate, total, service.serviceEpochAt)
	}
}

// Statistics can report an eligible fresh pair, but cannot commit its epoch
// or decide whether a later old sample will be accepted by the controller.
func TestWindowPacingFeedbackCycleStatisticsCannotRetireOldEvidence(t *testing.T) {
	var rates []ByteCount
	for _, read := range []bool{false, true} {
		service, start := newPartialFeedbackCycleFixture(t)
		sequenceId, messageId := NewId(), NewId()
		service.sent = service.total + 10000
		service.beginWrite(sequenceId, messageId, 1, start.Add(20*time.Millisecond), false)
		service.finishWrite(sequenceId, messageId, true)
		service.acknowledgeWrite(sequenceId, messageId, 1, false, 10*time.Millisecond, start.Add(100*time.Millisecond))
		service.observe(1000, start.Add(100*time.Millisecond))
		service.measured(time.Second, start.Add(100*time.Millisecond))
		service.sent += 11000
		service.beginWrite(NewId(), NewId(), 1, start.Add(101*time.Millisecond), false)
		service.observe(9000, start.Add(120*time.Millisecond))
		service.observe(1000, start.Add(130*time.Millisecond))
		if read {
			for range 20 {
				if rate, _, _ := service.measure(time.Second, start.Add(130*time.Millisecond), false); rate != 100000 {
					t.Fatalf("statistics missed the eligible fresh pair: %d", rate)
				}
			}
		}
		if !service.serviceEpochAt.IsZero() || service.serviceHoldRate != 10000000 {
			t.Fatalf("read=%t: polling changed the controller epoch or hold", read)
		}
		service.observe(9000, start.Add(100*time.Millisecond))
		rate, _, latest := service.measured(time.Second, start.Add(130*time.Millisecond))
		rates = append(rates, max(rate, latest))
	}
	if rates[0] != rates[1] {
		t.Fatalf("polling changed which delayed old evidence the controller accepted: %v", rates)
	}
}

// A new slow pair remains valid when an old worker is delayed and each new
// ACK outlives the ring. Its actual elapsed second still measures 1000 B/s.
func TestWindowPacingFeedbackCycleFreshSlowPairOutlivesOldAccounting(t *testing.T) {
	service, start := newPartialFeedbackCycleFixture(t)
	sequenceId, messageId := NewId(), NewId()
	service.sent = service.total + 10000
	service.beginWrite(sequenceId, messageId, 1, start.Add(20*time.Millisecond), false)
	service.finishWrite(sequenceId, messageId, true)
	service.acknowledgeWrite(sequenceId, messageId, 1, false, 10*time.Millisecond, start.Add(100*time.Millisecond))
	service.observe(1000, start.Add(100*time.Millisecond))
	service.measured(time.Second, start.Add(100*time.Millisecond))
	service.sent += 2000
	service.beginWrite(NewId(), NewId(), 1, start.Add(101*time.Millisecond), false)
	service.observe(1000, start.Add(time.Second))
	service.observe(1000, start.Add(2*time.Second))
	if rate, _, latest := service.measured(time.Second, start.Add(2*time.Second)); max(rate, latest) != 1000 {
		t.Fatalf("fresh complete slow pair froze behind an older worker: %d/%d", rate, latest)
	}
	service.observe(9000, start.Add(100*time.Millisecond))
	if rate, total, latest := service.measured(time.Second, start.Add(2*time.Second)); max(rate, latest) != 1000 || total != 212001 {
		t.Fatalf("old accounting rewrote the accepted slow pair: %d/%d total=%d", rate, latest, total)
	}
	for _, elapsed := range []time.Duration{3 * time.Second, 10 * time.Second} {
		if rate, _, latest := service.measured(time.Second, start.Add(elapsed)); rate != 0 || latest != 1000 {
			t.Fatalf("accepted summary expired into an older hold at %s: %d/%d", elapsed, rate, latest)
		}
	}
}

// Accepting a fresh epoch cannot drop its pinned compression span in either
// the peak-pair sampler or the queued mean. Reads alone create no new evidence.
func TestWindowPacingFeedbackCycleRetirementPreservesCompressionEvidence(t *testing.T) {
	for _, queued := range []bool{false, true} {
		service, start := newPartialFeedbackCycleFixture(t)
		service.compression = 50 * time.Millisecond
		sequenceId, messageId := NewId(), NewId()
		service.sent = service.total + 10000
		service.beginWrite(sequenceId, messageId, 1, start.Add(20*time.Millisecond), false)
		service.finishWrite(sequenceId, messageId, true)
		service.acknowledgeWrite(sequenceId, messageId, 1, false, 50*time.Millisecond, start.Add(100*time.Millisecond))
		service.observe(1000, start.Add(100*time.Millisecond))
		service.measured(time.Second, start.Add(100*time.Millisecond))
		service.sent += 1000000
		service.beginWrite(NewId(), NewId(), 1, start.Add(101*time.Millisecond), false)
		service.observe(9000, start.Add(120*time.Millisecond))
		roundTrip := 5 * time.Millisecond
		if queued {
			roundTrip = 500 * time.Millisecond
		}
		service.observeRoundTrip(roundTrip, 0, start.Add(120*time.Millisecond))
		service.observe(10000, start.Add(125*time.Millisecond))
		for at := 130 * time.Millisecond; at <= 170*time.Millisecond; at += 10 * time.Millisecond {
			service.observe(1, start.Add(at))
		}
		first, _, _ := service.measured(time.Second, start.Add(170*time.Millisecond))
		if first != 200100 {
			t.Fatalf("queued=%t: compressed pair lost its 10005 bytes / 50 ms: %d", queued, first)
		}
		for _, retain := range []bool{false, true} {
			rate, _, _ := service.measure(time.Second, start.Add(170*time.Millisecond), retain)
			if rate != first {
				t.Fatalf("queued=%t retain=%t: accepting an epoch repriced unchanged ACK evidence: %d -> %d", queued, retain, first, rate)
			}
		}
	}
}

// Another tail may drain before old workers apply their ACK bytes. The new
// physical boundary must also re-anchor its bounded fresh-side summary.
func TestWindowPacingFeedbackCycleRepeatedDrainReanchorsFreshEvidence(t *testing.T) {
	service, start := newPartialFeedbackCycleFixture(t)
	sequenceId, messageId, resumed, successor := NewId(), NewId(), NewId(), NewId()
	service.sent = service.total + 10000
	service.beginWrite(sequenceId, messageId, 1, start.Add(20*time.Millisecond), false)
	service.finishWrite(sequenceId, messageId, true)
	service.acknowledgeWrite(sequenceId, messageId, 1, false, 10*time.Millisecond, start.Add(100*time.Millisecond))
	service.observe(1000, start.Add(100*time.Millisecond))
	service.measured(time.Second, start.Add(100*time.Millisecond))
	service.sent += 2000
	service.beginWrite(sequenceId, resumed, 2, start.Add(101*time.Millisecond), false)
	service.finishWrite(sequenceId, resumed, true)
	service.observe(1000, start.Add(time.Second))
	service.acknowledgeWrite(sequenceId, resumed, 2, false, 10*time.Millisecond, start.Add(1001*time.Millisecond))
	service.sent += 2000
	service.beginWrite(sequenceId, successor, 3, start.Add(1002*time.Millisecond), false)
	service.finishWrite(sequenceId, successor, true)
	service.observe(1000, start.Add(2*time.Second))
	service.observe(1000, start.Add(3*time.Second))
	if rate, total, latest := service.measured(time.Second, start.Add(3*time.Second)); max(rate, latest) != 1000 || total != 204001 {
		t.Fatalf("new drain boundary lost its complete fresh pair: %d/%d total=%d", rate, latest, total)
	}
}
