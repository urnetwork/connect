// Real first offers and ACK publication qualify serializer intervals without
// seeding a service rate, a pacing hold, or a physical-drain result.
package connect

import (
	"testing"
	"time"
)

// The existing cold opening has no exact service before its first refill.
// That isolated refill cannot make the older propagation gap a rate sample.
func TestWindowServiceSupplyColdRefillRejectsGap(t *testing.T) {
	for _, path := range []time.Duration{100 * time.Millisecond, 400 * time.Millisecond} {
		for _, phase := range []time.Duration{0, 8 * time.Millisecond} {
			fixture := newWindowColdRefillFixture(t, phase)
			items, at, _ := fixture.opening(t, path, 3)
			estimate := fixture.ack(t, items[0], 2670, at, 0)
			if estimate.ServiceByteRate != 0 {
				t.Errorf("path=%s phase=%s: refill silence supplied service=%d", path, phase, estimate.ServiceByteRate)
			}
			estimate = fixture.ack(t, items[1], 2670, at.Add(10*time.Millisecond), 0)
			if estimate.ServiceByteRate != 267000 {
				t.Errorf("path=%s phase=%s: independent continuous pair failed to recover: %d", path, phase, estimate.ServiceByteRate)
			}
		}
	}
}

// Equal receiver waits cancel in the interval; they cannot explain its gap.
// Reversing actual selective publication must preserve that same rejection.
func TestWindowServiceSupplyCorrectedGapRejectsLateOffer(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		start := time.Unix(1700000000, 0)
		fixture := newWindowReceiverCreditFixture(t, nil, start, 100*time.Millisecond)
		first := fixture.write(0, 2670, start)
		late := fixture.write(1, 2670, start.Add(90*time.Millisecond))
		fixture.write(2, 2670, start.Add(90*time.Millisecond))
		acks := []receiveAckMessage{
			fixture.ack(first, start.Add(200*time.Millisecond), 100*time.Millisecond),
			fixture.ack(late, start.Add(290*time.Millisecond), 100*time.Millisecond),
		}
		for offset := range acks {
			index := offset
			if reverse {
				index = len(acks) - 1 - offset
			}
			ack := acks[index]
			ack.selective = true
			fixture.sequence.coalesceReceivedAck(fixture.ackWindow, ack)
		}
		at := start.Add(290 * time.Millisecond)
		if got := fixture.measured(at); got != 0 {
			t.Errorf("reverse=%t: equal waits hid the corrected 90ms supply gap: %d", reverse, got)
		}
		if fixture.service.total != 5340 || fixture.sequence.windowPacer.serviceAcked != 5340 || fixture.service.drained {
			t.Fatal("gap rejection changed real credit or physical flight")
		}
	}
}

// No receiver extension retains the established raw-clock fallback. These are
// real confirmed H1 credits, not the legacy bare service.observe helper.
func TestWindowServiceSupplyLegacyPreofferedSlowPair(t *testing.T) {
	start := time.Unix(1700000000, 0)
	fixture := newWindowReceiverCreditFixture(t, nil, start, 10*time.Millisecond)
	first := fixture.write(0, 2670, start)
	last := fixture.write(1, 2670, start)
	fixture.write(2, 2670, start)
	for index, item := range []*sendItem{first, last} {
		at := start.Add(time.Duration(index+1) * 100 * time.Millisecond)
		ack := fixture.ack(item, at, 0)
		ack.receiverAckDelaySet = false
		fixture.sequence.coalesceReceivedAck(fixture.ackWindow, ack)
		fixture.service.observeRoundTrip(at.Sub(item.sendTime), fixture.compression, at)
	}
	if got := fixture.measured(start.Add(200 * time.Millisecond)); got != 26700 {
		t.Fatalf("confirmed legacy preoffered slow service was lost: %d", got)
	}
}

// One old constituent cannot certify a prefix containing later or ambiguous
// offers. Every raw byte remains once-only credit, including duplicate ACKs.
func TestWindowServiceSupplyWholePrefixNeedsEveryOffer(t *testing.T) {
	for _, kind := range []string{"late", "missing", "pending", "retry"} {
		start := time.Unix(1700000000, 0)
		fixture := newWindowReceiverCreditFixture(t, nil, start, 10*time.Millisecond)
		first := fixture.write(0, 2670, start)
		var old *sendItem
		if kind == "pending" {
			old = fixture.offer(1, 2670, start)
		} else {
			old = fixture.write(1, 2670, start)
		}
		if kind == "missing" {
			fixture.sequence.resendQueue.stateLock.Lock()
			old.pacingSentAtNanos = 0
			fixture.sequence.resendQueue.stateLock.Unlock()
		} else if kind == "retry" {
			fixture.sequence.invalidateReceiverRttWrite(old)
		}
		late := fixture.write(2, 2670, start.Add(90*time.Millisecond))
		tail := fixture.write(3, 2670, start.Add(90*time.Millisecond))
		fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(first, start.Add(100*time.Millisecond), 0))
		ack := fixture.ack(late, start.Add(200*time.Millisecond), 0)
		fixture.sequence.coalesceReceivedAck(fixture.ackWindow, ack)
		fixture.sequence.coalesceReceivedAck(fixture.ackWindow, ack)
		if fixture.service.total != 8010 || fixture.sequence.windowPacer.serviceAcked != 8010 || fixture.service.drained {
			t.Fatalf("kind=%s: raw credit or physical flight changed", kind)
		}
		if got := fixture.measured(start.Add(200 * time.Millisecond)); got != 0 {
			t.Errorf("kind=%s: one old offer certified the entire mixed prefix: %d", kind, got)
		}
		if kind == "pending" {
			fixture.confirm(old)
			fixture.sequence.coalesceReceivedAck(fixture.ackWindow, ack)
			if fixture.service.total != 8010 || fixture.sequence.windowPacer.serviceAcked != 8010 ||
				fixture.measured(start.Add(200*time.Millisecond)) != 0 {
				t.Fatal("late carrier confirmation restored already-spent credit or supply proof")
			}
		}
		fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(tail, start.Add(210*time.Millisecond), 0))
		if got := fixture.measured(start.Add(210 * time.Millisecond)); got != 267000 {
			t.Errorf("kind=%s: later continuous evidence failed to recover: %d", kind, got)
		}
	}
}

// A paired summary may later mix with real raw-clock credit. Resizing twice
// cannot replace its old raw 90ms gap with the paired clock's shorter 10ms gap.
func TestWindowServiceSupplyMixedClockRebucketKeepsRawGap(t *testing.T) {
	start := time.Unix(1700000000, 0)
	fixture := newWindowReceiverCreditFixture(t, nil, start, 10*time.Millisecond)
	first := fixture.write(0, 2670, start)
	second := fixture.write(1, 2670, start.Add(90*time.Millisecond))
	raw := fixture.write(2, 2670, start.Add(180*time.Millisecond))
	next := fixture.write(3, 2670, start.Add(210*time.Millisecond))
	fixture.write(4, 2670, start.Add(210*time.Millisecond))
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(first, start.Add(200*time.Millisecond), 100*time.Millisecond))
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(second, start.Add(290*time.Millisecond), 180*time.Millisecond))
	at := start.Add(300 * time.Millisecond)
	// This real raw head advertises a wider timer before its bytes publish.
	fixture.service.observeRoundTrip(at.Sub(raw.sendTime), 800*time.Millisecond, at)
	ack := fixture.ack(raw, at, 0)
	ack.receiverAckDelaySet = false
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, ack)
	// A fresh head's timing can resize before its independent bytes publish.
	// It must not retime the already-retained mixed interval or own its credit.
	nextAt := start.Add(310 * time.Millisecond)
	timing := fixture.sequence.observeReceiverAckRtt(fixture.ack(next, nextAt, 0))
	if fixture.service.bucketInterval != 10*time.Millisecond {
		t.Fatal("the physical timing did not exercise the intended rebucket")
	}
	if got := fixture.measured(nextAt); got != 0 {
		t.Errorf("mixed-clock rebucket erased the raw supply gap: %d", got)
	}
	if fixture.service.total != 8010 || fixture.sequence.windowPacer.serviceAcked != 8010 {
		t.Fatal("timing-only resize manufactured credit")
	}
	fixture.service.acknowledgeWrite(fixture.sequence.sequenceId, next.messageId, next.sequenceNumber, true, fixture.compression, nextAt)
	fixture.sequence.publishAckServiceCreditWithTiming(next.messageId, true, nextAt, timing)
	if fixture.service.total != 10680 || fixture.sequence.windowPacer.serviceAcked != 10680 {
		t.Fatal("later independently published credit was not repaid exactly once")
	}
}

// A sibling may publish RTT between this head's timing and byte-credit calls.
// Its queued tuple cannot qualify this clean refill's otherwise idle interval.
func TestWindowServiceSupplySiblingQueueCannotQualifyGap(t *testing.T) {
	start := time.Unix(1700000000, 0)
	fixture := newWindowReceiverCreditFixture(t, nil, start, 10*time.Millisecond)
	first := fixture.write(0, 2670, start)
	late := fixture.write(1, 2670, start.Add(90*time.Millisecond))
	fixture.write(2, 2670, start.Add(90*time.Millisecond))
	sibling := newWindowReceiverCreditFixture(t, fixture.service, start, 10*time.Millisecond)
	queued := sibling.write(0, 2670, start)
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(first, start.Add(100*time.Millisecond), 0))
	at := start.Add(200 * time.Millisecond)
	timing := fixture.sequence.observeReceiverAckRtt(fixture.ack(late, at, 0))
	sibling.sequence.observeReceiverAckRtt(sibling.ack(queued, at, 0))
	fixture.service.acknowledgeWrite(fixture.sequence.sequenceId, late.messageId, late.sequenceNumber, true, fixture.compression, at)
	fixture.sequence.publishAckServiceCreditWithTiming(late.messageId, true, at, timing)
	if got := fixture.measured(at); got != 0 {
		t.Errorf("a sibling's fresh queue certified another source's refill gap: %d", got)
	}
	if fixture.service.total != 5340 || fixture.sequence.windowPacer.serviceAcked != 5340 || sibling.sequence.windowPacer.serviceAcked != 0 {
		t.Fatal("source-local coverage changed shared physical credit ownership")
	}
}
