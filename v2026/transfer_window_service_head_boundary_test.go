// Real coalesced credit preserves independent head bounds through ties,
// reordered receiver arrivals and changes to the bounded sampling buckets.
package connect

import (
	"testing"
	"time"
)

// Both cohorts are physically offered before either checkpoint. The second
// cohort can add a previously SACKed head's unknown cumulative-prefix boundary.
func windowServiceHeadTieFixture(t *testing.T, unknown, reverse bool) (*windowReceiverCreditFixture, time.Time) {
	t.Helper()
	start := time.Unix(1700000000, 0)
	first := newWindowReceiverCreditFixture(t, nil, start, 50*time.Millisecond)
	second := newWindowReceiverCreditFixture(t, first.service, start, 50*time.Millisecond)
	var originals [5]*sendItem
	for number := range originals {
		originals[number] = first.write(uint64(number), 2670, start)
	}
	var sibling [3]*sendItem
	for number := range sibling {
		sibling[number] = second.write(uint64(number), 2670, start)
	}
	leftAt := start.Add(150 * time.Millisecond)
	publishFirst := func() {
		first.sequence.coalesceReceivedAck(first.ackWindow, first.ack(originals[1], leftAt, 50*time.Millisecond))
	}
	publishSecond := func() {
		ack := second.ack(sibling[1], leftAt, 0)
		if unknown {
			ack.selective = true
			second.sequence.coalesceReceivedAck(second.ackWindow, ack)
			ack.selective = false
		}
		second.sequence.coalesceReceivedAck(second.ackWindow, ack)
	}
	if reverse {
		publishSecond()
		publishFirst()
	} else {
		publishFirst()
		publishSecond()
	}
	var left windowServiceSample
	for _, sample := range first.service.samples {
		if sample.bytes > 0 && sample.lastAtNanos == leftAt.UnixNano() {
			left = sample
		}
	}
	if left.bytes != 10680 || !left.supply.complete || left.supply.headComplete == unknown ||
		left.supply.headMaximumAtNanos != leftAt.UnixNano() {
		t.Fatalf("unknown=%t reverse=%t: tied boundary provenance changed: %+v", unknown, reverse, left)
	}
	if !unknown && (left.supply.headFirstAtNanos != start.Add(100*time.Millisecond).UnixNano() ||
		left.supply.headLastMinAtNanos != start.Add(100*time.Millisecond).UnixNano()) {
		t.Fatal("valid tied heads lost the conservative left endpoint")
	}
	at := start.Add(200 * time.Millisecond)
	first.sequence.coalesceReceivedAck(first.ackWindow, first.ack(originals[3], at, 0))
	if first.service.total != 16020 || first.service.sent != 21360 || first.service.drained ||
		first.sequence.windowPacer.serviceAcked != 10680 || second.sequence.windowPacer.serviceAcked != 5340 ||
		first.service.receiverHeldPrefixAtNanos != 0 {
		t.Fatal("tied boundary changed real credit, outstanding guards or held-prefix evidence")
	}
	return first, at
}

// A valid head at the same raw time cannot repair another constituent's
// absent boundary, in either publication order or after bucket merging.
func TestWindowServiceSupplyHeadTieUnknownStaysUnknown(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		fixture, at := windowServiceHeadTieFixture(t, true, reverse)
		for phase := range 2 {
			if phase != 0 {
				fixture.service.observeRoundTrip(100*time.Millisecond, time.Second, at.Add(time.Nanosecond))
				fixture.service.observeRoundTrip(100*time.Millisecond, 50*time.Millisecond, at.Add(2*time.Nanosecond))
			}
			if got := fixture.measured(at.Add(time.Duration(phase) * 2 * time.Nanosecond)); got != 0 {
				t.Errorf("reverse=%t phase=%d: unknown tied boundary acquired service=%d", reverse, phase, got)
			}
			if fixture.service.total != 16020 {
				t.Fatal("rebucketing duplicated raw credit")
			}
		}
	}
}

// Delayed first-write confirmation cannot replace a sibling's earlier head
// at the same raw checkpoint, or repair subsequently unknown credit there.
func TestWindowServiceSupplyHeadProbeConfirmationKeepsTiedBoundary(t *testing.T) {
	start := time.Unix(1700000000, 0)
	fixture := newWindowReceiverCreditFixture(t, nil, start, 100*time.Millisecond)
	first := fixture.write(0, 2670, start)
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(first, start.Add(100*time.Millisecond), 0))
	probe := fixture.offer(1, 2670, start.Add(200*time.Millisecond))
	if !fixture.service.roundTripProbe.resetInitialService || fixture.service.roundTripProbe.messageId != probe.messageId {
		t.Fatal("real empty first flight did not create the initial-service probe")
	}
	sibling := newWindowReceiverCreditFixture(t, fixture.service, start, 100*time.Millisecond)
	siblingHead := sibling.write(0, 2670, start.Add(200*time.Millisecond))
	sibling.write(1, 2670, start.Add(200*time.Millisecond))
	at := start.Add(400 * time.Millisecond)
	sibling.sequence.coalesceReceivedAck(sibling.ackWindow, sibling.ack(siblingHead, at, 100*time.Millisecond))
	if fixture.service.feedbackHeadAtNanos != start.Add(300*time.Millisecond).UnixNano() {
		t.Fatal("sibling did not publish its exact earlier head boundary")
	}
	ack := fixture.ack(probe, at, 0)
	if timing := fixture.sequence.observeReceiverAckRtt(ack); timing.receivedAtNanos != 0 {
		t.Fatal("pending writer supplied confirmed credit timing")
	}
	fixture.service.acknowledgeWrite(fixture.sequence.sequenceId, probe.messageId, probe.sequenceNumber, false, fixture.compression, at)
	fixture.confirm(probe)
	if !fixture.service.feedbackAt.Equal(at) ||
		fixture.service.feedbackHeadAtNanos != start.Add(300*time.Millisecond).UnixNano() {
		t.Fatal("delayed probe confirmation promoted the tied left boundary")
	}
	// Only the ordinary coalescer publishes these still-uncredited bytes.
	// The spent pending tuple is unavailable for a second timing observation.
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, ack)
	if fixture.service.feedbackHeadAtNanos != 0 || fixture.service.total != 8010 ||
		fixture.sequence.windowPacer.serviceAcked != 5340 || sibling.sequence.windowPacer.serviceAcked != 2670 ||
		fixture.service.drained {
		t.Fatal("unknown same-time credit repaired timing or changed ownership")
	}
}

// Real slow delivery establishes the hold before a later cumulative prefix
// opens an incomplete feedback cycle. Independent flight prevents drain proofs.
func windowServiceHeadFeedbackFixture(t *testing.T, wait time.Duration) (*windowReceiverCreditFixture, [7]*sendItem, time.Time) {
	t.Helper()
	start := time.Unix(1700000000, 0)
	fixture := newWindowReceiverCreditFixture(t, nil, start, 50*time.Millisecond)
	var originals [7]*sendItem
	for number := range originals {
		originals[number] = fixture.write(uint64(number), 2670, start)
	}
	guard := newWindowReceiverCreditFixture(t, fixture.service, start, 50*time.Millisecond)
	guard.write(0, 64000, start)
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(originals[0], start.Add(150*time.Millisecond), 50*time.Millisecond))
	warmAt := start.Add(650 * time.Millisecond)
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(originals[1], warmAt, 50*time.Millisecond))
	if got := fixture.measured(warmAt); got != 5340 {
		t.Fatalf("real slow pair did not establish its hold: got=%d want=5340", got)
	}
	at := start.Add(750 * time.Millisecond)
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(originals[3], at, wait))
	if !fixture.service.feedbackPending || fixture.service.feedbackLimited ||
		!fixture.service.feedbackCycleBefore.Equal(warmAt) ||
		fixture.service.feedbackCycleHeadAtNanos != start.Add(600*time.Millisecond).UnixNano() ||
		fixture.service.feedbackCycle.bytes != 5340 || fixture.service.total != 10680 || fixture.service.drained {
		t.Fatalf("real prefix did not open the exact incomplete turn: pending=%t limited=%t before=%s head=%d bytes=%d total=%d",
			fixture.service.feedbackPending, fixture.service.feedbackLimited, fixture.service.feedbackCycleBefore,
			fixture.service.feedbackCycleHeadAtNanos, fixture.service.feedbackCycle.bytes, fixture.service.total)
	}
	return fixture, originals, at
}

// The pending cycle's first newly credited bytes use the captured preceding
// head, not an invented boundary at their own first arrival.
func TestWindowServiceSupplyHeadPendingCycleKeepsPhysicalLeft(t *testing.T) {
	fixture, _, at := windowServiceHeadFeedbackFixture(t, 0)
	if got := fixture.measured(at); got != 35600 {
		t.Fatalf("pending raw100ms hid its150ms head extent: got=%d want=35600", got)
	}
	if !fixture.service.feedbackPending || fixture.service.total != 10680 {
		t.Fatal("a rate read completed the pending credit turn")
	}
}

// An expired ring cannot shorten the mixed-clock denominator retained in a
// completed summary. Raw ownership and first-checkpoint exclusion stay exact.
func TestWindowServiceSupplyHeadCompleteFallbackKeepsChangedWait(t *testing.T) {
	fixture, originals, at := windowServiceHeadFeedbackFixture(t, 50*time.Millisecond)
	if got := fixture.measured(at); got != 53400 {
		t.Fatalf("equal-wait pending turn lost its100ms price: got=%d want=53400", got)
	}
	at = at.Add(time.Second)
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(originals[5], at, 0))
	count := 0
	for _, sample := range fixture.service.samples {
		if sample.bytes > 0 && sample.bucket <= fixture.service.newestBucket &&
			sample.bucket > fixture.service.newestBucket-int64(len(fixture.service.samples)) {
			count++
			if sample.firstAtNanos != sample.lastAtNanos {
				t.Fatal("a live ring interval could bypass the summary")
			}
		}
	}
	cycle := fixture.service.feedbackComplete
	if count != 1 || fixture.service.feedbackPending || cycle.bytes != 10680 || cycle.firstBytes != 5340 ||
		cycle.receiverBytes != 5340 || !cycle.supply.headComplete ||
		cycle.supply.headMaximumAtNanos-cycle.supply.headFirstAtNanos != int64(1050*time.Millisecond) {
		t.Fatalf("mixed completed summary not isolated: count=%d pending=%t cycle=%+v", count, fixture.service.feedbackPending, cycle)
	}
	if got := fixture.measured(at); got != 5085 {
		t.Fatalf("completed raw1s hid its1050ms head extent: got=%d want=5085", got)
	}
	if fixture.service.total != 16020 || fixture.sequence.windowPacer.serviceAcked != 16020 ||
		fixture.service.drained || originals[6].serviceCreditObserved {
		t.Fatal("fallback changed raw credit or acknowledged the guard")
	}
}

// Late publication at the captured left timestamp may revoke a boundary,
// never repair unknown evidence by borrowing another head at the same time.
func TestWindowServiceSupplyHeadLateUnknownRevokesPendingLeft(t *testing.T) {
	fixture, _, at := windowServiceHeadFeedbackFixture(t, 0)
	start := at.Add(-750 * time.Millisecond)
	late := newWindowReceiverCreditFixture(t, fixture.service, start, 50*time.Millisecond)
	late.write(0, 2670, start)
	head := late.write(1, 2670, start)
	late.write(2, 2670, start)
	ack := late.ack(head, start.Add(650*time.Millisecond), 0)
	ack.selective = true
	late.sequence.coalesceReceivedAck(late.ackWindow, ack)
	ack.selective = false
	late.sequence.coalesceReceivedAck(late.ackWindow, ack)
	if fixture.service.feedbackCycleHeadAtNanos != 0 || !fixture.service.feedbackPending {
		t.Fatal("previously SACKed head left a valid pending-cycle boundary")
	}
	if got := fixture.measured(at); got != 5340 {
		t.Fatalf("unknown late left endpoint granted a new rate: got=%d want held5340", got)
	}
	if fixture.service.total != 16020 || late.sequence.windowPacer.serviceAcked != 5340 ||
		fixture.sequence.windowPacer.serviceAcked != 10680 || fixture.service.drained {
		t.Fatal("late boundary invalidation duplicated credit or completed physical flight")
	}
}

// Different valid head waits at one raw checkpoint use the earliest head
// for the left boundary, rather than shortening the interval with the latest.
func TestWindowServiceSupplyHeadTieKeepsConservativeRange(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		fixture, at := windowServiceHeadTieFixture(t, false, reverse)
		for phase := range 2 {
			if phase != 0 {
				fixture.service.observeRoundTrip(100*time.Millisecond, time.Second, at.Add(time.Nanosecond))
				fixture.service.observeRoundTrip(100*time.Millisecond, 50*time.Millisecond, at.Add(2*time.Nanosecond))
			}
			if got := fixture.measured(at.Add(time.Duration(phase) * 2 * time.Nanosecond)); got != 53400 {
				t.Errorf("reverse=%t phase=%d: conservative tied-head service=%d want=53400", reverse, phase, got)
			}
			if fixture.service.total != 16020 {
				t.Fatal("rebucketing duplicated raw credit")
			}
		}
	}
}

// A middle head may have arrived later than the last raw ACK's head. Both
// raw-prefix and exact single-head intervals retain that whole-numerator bound.
func TestWindowServiceSupplyHeadRangeIncludesReorderedMiddle(t *testing.T) {
	for _, prefix := range []bool{false, true} {
		start := time.Unix(1700000000, 0)
		fixture := newWindowReceiverCreditFixture(t, nil, start, 300*time.Millisecond)
		stride := 1
		if prefix {
			stride = 2
		}
		originals := make([]*sendItem, 3*stride+1)
		for number := range originals {
			originals[number] = fixture.write(uint64(number), 2670, start)
		}
		for index, point := range []struct {
			at   time.Duration
			wait time.Duration
		}{
			{at: 400 * time.Millisecond, wait: 300 * time.Millisecond},
			{at: 750 * time.Millisecond, wait: 50 * time.Millisecond},
			{at: 800 * time.Millisecond, wait: 300 * time.Millisecond},
		} {
			ack := fixture.ack(originals[(index+1)*stride-1], start.Add(point.at), point.wait)
			ack.selective = !prefix
			fixture.sequence.coalesceReceivedAck(fixture.ackWindow, ack)
			if index == 1 {
				want := ByteCount(stride * 4450)
				if got := fixture.measured(start.Add(point.at)); got != want || fixture.service.serviceHoldRate != want {
					t.Fatalf("prefix=%t: real first pair did not establish the positive hold: got=%d want=%d", prefix, got, want)
				}
			}
		}
		at := start.Add(800 * time.Millisecond)
		want := ByteCount(stride * 8900)
		for phase := range 2 {
			if phase != 0 {
				fixture.service.observeRoundTrip(100*time.Millisecond, 4*time.Second, at.Add(time.Nanosecond))
				fixture.service.observeRoundTrip(100*time.Millisecond, 300*time.Millisecond, at.Add(2*time.Nanosecond))
			}
			if got := fixture.measured(at.Add(time.Duration(phase) * 2 * time.Nanosecond)); got != want {
				t.Errorf("prefix=%t phase=%d: lost the middle head's600ms extent: got=%d want=%d", prefix, phase, got, want)
			}
		}
		if fixture.service.total != ByteCount(3*stride*2670) || fixture.service.drained ||
			fixture.service.receiverHeldPrefixAtNanos != 0 || originals[len(originals)-1].serviceCreditObserved {
			t.Fatal("reordered head bounds changed physical credit, guard or held-prefix authority")
		}
	}
}
