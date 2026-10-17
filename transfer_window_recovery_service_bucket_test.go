// A bucket's excluded first checkpoint does not own its selected bytes' proof.
package connect

import (
	"testing"
	"time"
)

// The first reply can race its physical writer's return. Removing that
// checkpoint must leave the later confirmed original's capacity measurable.
func TestWindowServiceRecoverySameBucketExcludedFirstKeepsConfirmedGrowth(t *testing.T) {
	service, start := newWindowQualifiedServiceFixture(t, 100000, 0, time.Millisecond)
	fixture := newWindowReceiverCreditFixture(t, service, start, 0)
	left := fixture.offer(0, 2315, start)
	right := fixture.write(1, 154, start)
	fixture.write(2, 3445, start)
	prior := service.total
	// Stay within the continuous feedback allowance, in the next bucket.
	// Older warmup pairs remain slower than this selected 154-byte interval.
	leftAt := start.Add(18 * time.Millisecond)
	rightAt := leftAt.Add(time.Millisecond)
	for _, point := range []struct {
		item *sendItem
		at   time.Time
	}{
		{item: left, at: leftAt}, {item: right, at: rightAt},
	} {
		ack := fixture.ack(point.item, point.at, 0)
		ack.selective, ack.receiverAckDelaySet = true, false
		fixture.sequence.coalesceReceivedAck(fixture.ackWindow, ack)
	}
	sample := windowRecoveryServiceSampleAt(t, service, rightAt)
	interval := max(deliverySizedWindowSampleInterval, service.bucketInterval)
	if leftAt.UnixNano()/int64(interval) != rightAt.UnixNano()/int64(interval) ||
		service.compression != 0 || sample.firstAtNanos != leftAt.UnixNano() ||
		sample.lastAtNanos != rightAt.UnixNano() || sample.bytes != 2469 || sample.firstBytes != 2315 ||
		!sample.supply.recorded || sample.supply.complete || !sample.supply.rawClock ||
		sample.supply.minimumPath <= 0 || sample.receiverBytes != 0 || sample.queued ||
		service.feedbackPending || service.feedbackComplete.bytes != 0 || service.drained || service.receiverHeldPrefixAtNanos != 0 ||
		left.rttState != sendItemRttWritePending || right.rttState != sendItemRttWriteConfirmed || !right.rttH1 {
		t.Fatalf("same-bucket excluded-first premises changed: sample=%+v left=%d right=%d", sample, left.rttState, right.rttState)
	}
	ack := fixture.ack(right, rightAt, 0)
	ack.selective, ack.receiverAckDelaySet = true, false
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, ack)
	if service.total-prior != 2469 || fixture.sequence.windowPacer.serviceAcked != 2469 {
		t.Fatal("same-bucket classification changed exact-once credit")
	}
	if rate := fixture.measured(rightAt); rate != 154000 || service.serviceHoldRate != 154000 {
		t.Errorf("excluded pending first checkpoint vetoed confirmed selected growth: rate=%d hold=%d", rate, service.serviceHoldRate)
	}
}

// Moving the pending original inside the selected suffix changes the proof.
// Its real delivery cannot borrow the confirmed neighbor's physical certainty.
func TestWindowServiceRecoverySameBucketSelectedMixKeepsIncompleteSupply(t *testing.T) {
	service, start := newWindowQualifiedServiceFixture(t, 100000, 0, time.Millisecond)
	fixture := newWindowReceiverCreditFixture(t, service, start, 0)
	left := fixture.write(0, 2315, start)
	unknown := fixture.offer(1, 17, start)
	right := fixture.write(2, 154, start)
	fixture.write(3, 3445, start)
	prior := service.total
	leftAt := start.Add(18 * time.Millisecond)
	rightAt := leftAt.Add(time.Millisecond)
	for _, point := range []struct {
		item *sendItem
		at   time.Time
	}{
		{item: left, at: leftAt},
		{item: unknown, at: leftAt.Add(500 * time.Microsecond)},
		{item: right, at: rightAt},
	} {
		ack := fixture.ack(point.item, point.at, 0)
		ack.selective, ack.receiverAckDelaySet = true, false
		fixture.sequence.coalesceReceivedAck(fixture.ackWindow, ack)
	}
	sample := windowRecoveryServiceSampleAt(t, service, rightAt)
	interval := max(deliverySizedWindowSampleInterval, service.bucketInterval)
	if leftAt.UnixNano()/int64(interval) != rightAt.UnixNano()/int64(interval) ||
		service.compression != 0 || sample.firstAtNanos != leftAt.UnixNano() ||
		sample.lastAtNanos != rightAt.UnixNano() || sample.bytes != 2486 || sample.firstBytes != 2315 ||
		!sample.supply.recorded || sample.supply.complete || !sample.supply.rawClock ||
		sample.supply.minimumPath <= 0 || sample.receiverBytes != 0 || sample.queued ||
		service.feedbackPending || service.feedbackComplete.bytes != 0 || service.drained || service.receiverHeldPrefixAtNanos != 0 ||
		unknown.rttState != sendItemRttWritePending || left.rttState != sendItemRttWriteConfirmed ||
		right.rttState != sendItemRttWriteConfirmed || !left.rttH1 || !right.rttH1 {
		t.Fatalf("same-bucket selected-mix premises changed: sample=%+v left=%d pending=%d right=%d", sample, left.rttState, unknown.rttState, right.rttState)
	}
	ack := fixture.ack(unknown, rightAt, 0)
	ack.selective, ack.receiverAckDelaySet = true, false
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, ack)
	if service.total-prior != 2486 || fixture.sequence.windowPacer.serviceAcked != 2486 {
		t.Fatal("same-bucket mixed classification changed exact-once credit")
	}
	if rate := fixture.measured(rightAt); rate != 100000 || service.serviceHoldRate != 100000 {
		t.Errorf("selected pending credit borrowed confirmed growth authority: rate=%d hold=%d", rate, service.serviceHoldRate)
	}
}
