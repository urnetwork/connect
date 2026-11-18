// Real recorded credits exercise queue-only admission and the bounded summaries
// that preserve serialization evidence after older ring endpoints expire.
package connect

import (
	"testing"
	"time"
)

// Another source's later offer defeats whole-cohort preoffering. Only the
// selected old tail's own residence can cover this complete 200 ms interval.
func TestWindowServiceSupplyOwnTailQueueCoversFullSpan(t *testing.T) {
	for _, shift := range []time.Duration{0, time.Nanosecond} {
		start := time.Unix(1700000000, 0)
		fixture := newWindowReceiverCreditFixture(t, nil, start, 100*time.Millisecond)
		first := fixture.write(0, 2670, start)
		tail := fixture.write(1, 2670, start.Add(shift))
		fixture.write(2, 2670, start.Add(shift))
		sibling := newWindowReceiverCreditFixture(t, fixture.service, start, fixture.compression)
		fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(first, start.Add(100*time.Millisecond), 0))
		later := sibling.write(0, 2670, start.Add(100*time.Millisecond))
		sibling.sequence.coalesceReceivedAck(sibling.ackWindow, sibling.ack(later, start.Add(220*time.Millisecond), 0))
		at := start.Add(300 * time.Millisecond)
		fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(tail, at, 0))

		func() {
			fixture.service.stateLock.Lock()
			defer fixture.service.stateLock.Unlock()
			var firstSample, laterSample, tailSample windowServiceSample
			for _, sample := range fixture.service.samples {
				switch sample.lastAtNanos {
				case start.Add(100 * time.Millisecond).UnixNano():
					firstSample = sample
				case start.Add(220 * time.Millisecond).UnixNano():
					laterSample = sample
				case at.UnixNano():
					tailSample = sample
				}
			}
			if firstSample.bytes != 2670 || laterSample.bytes != 2670 || tailSample.bytes != 2670 ||
				firstSample.receiverBytes != firstSample.bytes || laterSample.receiverBytes != laterSample.bytes || tailSample.receiverBytes != tailSample.bytes ||
				!laterSample.queued || fixture.service.serviceHoldRate != 0 || fixture.service.feedbackPending {
				t.Fatal("real cold points did not isolate the queued full-span selection")
			}
			coverage := tailSample
			coverage.mergeSupply(laterSample)
			span := tailSample.receiverLastAtNanos - firstSample.receiverLastAtNanos
			interval := max(deliverySizedWindowSampleInterval, fixture.service.bucketInterval)
			if span != int64(200*time.Millisecond) || !coverage.supply.recorded || !coverage.supply.complete ||
				coverage.supply.minimumPath != 100*time.Millisecond ||
				coverage.supply.receiverMaximumGap <= 2*interval ||
				coverage.supply.latestSentNanos <= firstSample.receiverLastAtNanos-int64(coverage.supply.minimumPath) ||
				coverage.supply.lastQueue != 200*time.Millisecond-shift {
				t.Fatalf("shift=%s: queue-only boundary was not physically witnessed: %+v", shift, coverage.supply)
			}
		}()
		want := ByteCount(26700)
		if shift != 0 {
			want = 0
		}
		if got := fixture.measured(at); got != want {
			t.Errorf("shift=%s: full-span queue boundary measured=%d want=%d", shift, got, want)
		}
		if fixture.service.total != 8010 || fixture.sequence.windowPacer.serviceAcked != 5340 ||
			sibling.sequence.windowPacer.serviceAcked != 2670 || fixture.service.drained {
			t.Fatal("queue admission changed real source credit or the unacknowledged tail")
		}
	}
}

// A completed slow turn outlives the ring. Its recorded-credit summary may
// replace the warm hold only with real coverage, never from late refill offers.
func TestWindowServiceSupplyCompleteFallbackNeedsRecordedCoverage(t *testing.T) {
	for _, preoffered := range []bool{true, false} {
		start := time.Unix(1700000000, 0)
		fixture := newWindowReceiverCreditFixture(t, nil, start, 10*time.Millisecond)
		first := fixture.write(0, 2670, start)
		second := fixture.write(1, 2670, start)
		var third, fourth *sendItem
		if preoffered {
			third = fixture.write(2, 2670, start)
			fourth = fixture.write(3, 2670, start)
		}
		// Independent outstanding ownership makes this a complete serializer
		// turn rather than a limited-flight partial, without seeding a bound.
		guard := newWindowReceiverCreditFixture(t, fixture.service, start, fixture.compression)
		guard.write(0, 64000, start)
		fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(first, start.Add(100*time.Millisecond), 0))
		fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(second, start.Add(110*time.Millisecond), 0))
		if got := fixture.measured(start.Add(110 * time.Millisecond)); got != 267000 {
			t.Fatalf("preoffered=%t: real warm pair measured=%d", preoffered, got)
		}
		if !preoffered {
			third = fixture.write(2, 2670, start.Add(890*time.Millisecond))
		}
		fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(third, start.Add(time.Second), 0))
		if !fixture.service.feedbackPending || fixture.service.feedbackLimited {
			t.Fatal("real outstanding ownership did not open the intended full feedback turn")
		}
		if !preoffered {
			fourth = fixture.write(3, 2670, start.Add(1890*time.Millisecond))
		}
		at := start.Add(2 * time.Second)
		fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(fourth, at, 0))
		func() {
			fixture.service.stateLock.Lock()
			defer fixture.service.stateLock.Unlock()
			count := 0
			for _, sample := range fixture.service.samples {
				if sample.bytes > 0 && fixture.service.newestBucket-int64(len(fixture.service.samples)) < sample.bucket &&
					sample.bucket <= fixture.service.newestBucket {
					count++
					if sample.firstAtNanos != sample.lastAtNanos {
						t.Fatal("a retained ring span could bypass completed-summary coverage")
					}
				}
			}
			cycle := fixture.service.feedbackComplete
			interval := max(deliverySizedWindowSampleInterval, fixture.service.bucketInterval)
			if count != 1 || fixture.service.feedbackPending || fixture.service.feedbackFresh.bytes != 0 ||
				cycle.bytes != 5340 || cycle.firstBytes != 2670 || !cycle.supply.recorded || !cycle.supply.complete ||
				cycle.receiverBytes != cycle.bytes || cycle.supply.rawClock || cycle.supply.maximumGap <= 2*interval ||
				cycle.lastAtNanos-cycle.firstAtNanos != int64(time.Second) ||
				fixture.service.serviceHoldRate != 267000 || fixture.service.drained {
				t.Fatalf("preoffered=%t: insufficient-ring completed fallback not isolated: count=%d cycle=%+v", preoffered, count, cycle)
			}
			if (cycle.supply.lastQueue >= time.Second) != preoffered {
				t.Fatal("the exact head residence did not distinguish the fallback rows")
			}
		}()
		want := ByteCount(2670)
		wantRate := want
		if !preoffered {
			want = 267000
			wantRate = 0
		}
		rate, total, latest := fixture.service.measure(time.Second, at, false)
		if rate != wantRate || latest != want || total != 10680 || fixture.service.serviceHoldRate != 267000 {
			t.Errorf("preoffered=%t: read-only completed fallback=%d/%d total=%d want=%d", preoffered, rate, latest, total, want)
		}
		if got := fixture.measured(at); got != want || fixture.sequence.windowPacer.serviceAcked != 10680 ||
			guard.sequence.windowPacer.serviceAcked != 0 {
			t.Errorf("preoffered=%t: retaining completed fallback=%d want=%d or source credit changed", preoffered, got, want)
		}
	}
}

// The physical tail can finish before its worker publishes old bytes. Fresh
// recorded slow evidence survives ring expiry and that exact delayed boundary.
func TestWindowServiceSupplyFreshFallbackKeepsDelayedAccounting(t *testing.T) {
	start := time.Unix(1700000000, 0)
	old := newWindowReceiverCreditFixture(t, nil, start, 10*time.Millisecond)
	first := old.write(0, 2670, start)
	second := old.write(1, 2670, start)
	partial := old.write(2, 2670, start)
	tail := old.write(3, 2670, start)
	old.sequence.coalesceReceivedAck(old.ackWindow, old.ack(first, start.Add(100*time.Millisecond), 0))
	old.sequence.coalesceReceivedAck(old.ackWindow, old.ack(second, start.Add(110*time.Millisecond), 0))
	if got := old.measured(start.Add(110 * time.Millisecond)); got != 267000 {
		t.Fatalf("real warm pair measured=%d", got)
	}
	old.sequence.coalesceReceivedAck(old.ackWindow, old.ack(partial, start.Add(250*time.Millisecond), 0))
	if !old.service.feedbackPending {
		t.Fatal("the physical old partial did not open a feedback turn")
	}
	// This call ordering is the publication barrier: the real cumulative
	// physical ACK happens now, but only its recorded credit waits below.
	tailAt := start.Add(290 * time.Millisecond)
	timing := old.sequence.observeReceiverAckRtt(old.ack(tail, tailAt, 0))
	old.service.acknowledgeWrite(old.sequence.sequenceId, tail.messageId, tail.sequenceNumber, false, old.compression, tailAt)
	if !old.service.drained || old.service.total != 8010 || old.service.drainedSent != 10680 {
		t.Fatal("the real tail did not prove the exact unapplied old credit")
	}
	fresh := newWindowReceiverCreditFixture(t, old.service, start, old.compression)
	firstFresh := fresh.write(0, 2670, start.Add(300*time.Millisecond))
	secondFresh := fresh.write(1, 2670, start.Add(300*time.Millisecond))
	thirdFresh := fresh.write(2, 2670, start.Add(300*time.Millisecond))
	fresh.write(3, 2670, start.Add(300*time.Millisecond))
	for _, point := range []struct {
		item *sendItem
		at   time.Duration
	}{
		{item: firstFresh, at: 400 * time.Millisecond},
		{item: secondFresh, at: 1400 * time.Millisecond},
		{item: thirdFresh, at: 2400 * time.Millisecond},
	} {
		fresh.sequence.coalesceReceivedAck(fresh.ackWindow, fresh.ack(point.item, start.Add(point.at), 0))
	}
	at := start.Add(2400 * time.Millisecond)
	func() {
		old.service.stateLock.Lock()
		defer old.service.stateLock.Unlock()
		count := 0
		for _, sample := range old.service.samples {
			if sample.bytes > 0 && old.service.newestBucket-int64(len(old.service.samples)) < sample.bucket &&
				sample.bucket <= old.service.newestBucket {
				count++
				if sample.firstAtNanos != sample.lastAtNanos {
					t.Fatal("a retained ring span could bypass fresh-summary coverage")
				}
			}
		}
		cycle := old.service.feedbackFresh
		interval := max(deliverySizedWindowSampleInterval, old.service.bucketInterval)
		if count != 1 || !old.service.feedbackPending || old.service.feedbackDrainPending != 2670 ||
			!old.service.feedbackDrainAt.Equal(start.Add(300*time.Millisecond)) || old.service.feedbackComplete.bytes != 0 ||
			cycle.bytes != 8010 || cycle.firstBytes != 2670 || !cycle.supply.recorded || !cycle.supply.complete ||
			cycle.receiverBytes != cycle.bytes || cycle.supply.rawClock || cycle.supply.maximumGap <= 2*interval ||
			cycle.lastAtNanos-cycle.firstAtNanos != int64(2*time.Second) || cycle.supply.lastQueue != 2*time.Second ||
			old.service.serviceHoldRate != 267000 || old.service.drained {
			t.Fatalf("insufficient-ring fresh fallback not isolated: count=%d cycle=%+v", count, cycle)
		}
	}()
	if rate, total, latest := old.service.measure(time.Second, at, false); max(rate, latest) != 2670 ||
		total != 16020 || old.service.serviceHoldRate != 267000 || !old.service.feedbackPending {
		t.Fatalf("read-only fresh fallback changed accounting or hold: %d/%d total=%d", rate, latest, total)
	}
	if got := fresh.measured(at); got != 2670 || old.service.feedbackPending || old.service.feedbackComplete.bytes != 8010 {
		t.Fatalf("fresh summary failed to become the retained completed evidence: %d", got)
	}
	old.sequence.publishAckServiceCreditWithTiming(tail.messageId, false, tailAt, timing)
	old.sequence.publishAckServiceCreditWithTiming(tail.messageId, false, tailAt, timing)
	if got := fresh.measured(at); got != 2670 || old.service.total != 18690 ||
		old.sequence.windowPacer.serviceAcked != 10680 || fresh.sequence.windowPacer.serviceAcked != 8010 {
		t.Fatalf("delayed duplicate old credit rewrote fresh service or ownership: rate=%d total=%d", got, old.service.total)
	}
}
