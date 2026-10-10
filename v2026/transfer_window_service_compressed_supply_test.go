// Exact cumulative heads separate supply evidence from permission to remove
// receiver waiting. These synchronous fixtures exercise the real coalescer.
package connect

import (
	"testing"
	"time"
)

// Confirmed physical offers precede both heads; a fifth original keeps the
// flight open. No sender worker, seeded service rate or synthetic credit is used.
func windowServiceCompressedPrefixPair(t *testing.T, lastWait time.Duration) (*windowReceiverCreditFixture, time.Time) {
	t.Helper()
	start := time.Unix(1700000000, 0)
	fixture := newWindowReceiverCreditFixture(t, nil, start, 50*time.Millisecond)
	var originals [5]*sendItem
	for number := range originals {
		originals[number] = fixture.write(uint64(number), 2670, start)
	}
	firstAt := start.Add(150 * time.Millisecond)
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(originals[1], firstAt, 50*time.Millisecond))
	if got := fixture.measured(firstAt); got != 0 {
		t.Fatalf("one cumulative head manufactured service=%d", got)
	}
	lastAt := start.Add(200 * time.Millisecond)
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(originals[3], lastAt, lastWait))
	var first, last windowServiceSample
	func() {
		fixture.service.stateLock.Lock()
		defer fixture.service.stateLock.Unlock()
		for _, sample := range fixture.service.samples {
			if sample.bytes > 0 && sample.firstAtNanos == firstAt.UnixNano() {
				first = sample
			}
			if sample.bytes > 0 && sample.lastAtNanos == lastAt.UnixNano() {
				last = sample
			}
		}
		if fixture.service.total != 10680 || fixture.service.sent != 13350 || fixture.service.drained ||
			fixture.sequence.windowPacer.serviceAcked != 10680 || fixture.sequence.windowPacer.serviceSent != 13350 ||
			fixture.service.receiverHeldPrefixAtNanos != 0 {
			t.Fatalf("prefix changed ownership or acquired held-prefix authority: total=%d sent=%d acked=%d offered=%d drained=%t held=%d",
				fixture.service.total, fixture.service.sent, fixture.sequence.windowPacer.serviceAcked,
				fixture.sequence.windowPacer.serviceSent, fixture.service.drained, fixture.service.receiverHeldPrefixAtNanos)
		}
		if timing := fixture.service.roundTripEvidenceWithLock(lastAt); timing.minimum != 100*time.Millisecond {
			t.Fatalf("exact heads did not establish the independent100ms minimum: %+v", timing)
		}
	}()
	for index, sample := range []windowServiceSample{first, last} {
		if sample.bytes != 5340 || sample.firstAtNanos != sample.lastAtNanos ||
			!sample.supply.recorded || !sample.supply.complete || sample.supply.rawClock ||
			sample.supply.latestSentNanos != start.UnixNano() || sample.supply.minimumPath != 100*time.Millisecond {
			t.Fatalf("head%d lost exact complete physical offers: %+v", index, sample)
		}
	}
	if first.receiverBytes != 0 {
		t.Fatalf("positive-wait head retimed its two-item prefix: bytes=%d", first.receiverBytes)
	}
	wantLastReceiverBytes := ByteCount(0)
	if lastWait == 0 {
		wantLastReceiverBytes = last.bytes
	}
	if last.receiverBytes != wantLastReceiverBytes {
		t.Fatalf("last head changed whole-prefix retiming eligibility: wait=%s bytes=%d want=%d",
			lastWait, last.receiverBytes, wantLastReceiverBytes)
	}
	for number, item := range originals {
		if item.serviceCreditObserved != (number < 4) {
			t.Fatalf("original%d credit changed or the fifth guard was acknowledged: %+v", number, item)
		}
	}
	// Read-only witnesses retain the existing branch shape without requiring a
	// repair to store its independent head boundary in any particular field.
	t.Logf("raw_span_ns=%d head_span_ns=%d first_receiver_bytes=%d last_receiver_bytes=%d first_queue_ns=%d last_queue_ns=%d complete=true raw_credit=%d",
		lastAt.Sub(firstAt), lastAt.Add(-lastWait).Sub(firstAt.Add(-50*time.Millisecond)),
		first.receiverBytes, last.receiverBytes, first.supply.lastQueue, last.supply.lastQueue, fixture.service.total)
	return fixture, lastAt
}

// Equal own waits leave the raw denominator unchanged. Complete preoffering
// cannot disappear merely because neither head may retime its whole prefix.
func TestWindowServiceSupplyCompressedPrefixRetainsSupplyBoundary(t *testing.T) {
	fixture, at := windowServiceCompressedPrefixPair(t, 50*time.Millisecond)
	if got := fixture.measured(at); got != 106800 {
		t.Fatalf("two preoffered compressed prefixes lost raw50ms service: got=%d want=106800", got)
	}
}

// Changing the head wait makes raw arrivals closer than their own receiver
// endpoints. The selected tail's real queue must not certify an inflated rate.
func TestWindowServiceSupplyCompressedPrefixChangingWaitCannotInflate(t *testing.T) {
	fixture, at := windowServiceCompressedPrefixPair(t, 0)
	if got := fixture.measured(at); got > 53400 {
		t.Fatalf("mixed prefix clocks inflated service over the100ms head interval: got=%d ceiling=53400", got)
	}
}
