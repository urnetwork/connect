// Interval-local controls keep incomplete supply distinct from cold or excluded credit.
package connect

import (
	"testing"
	"time"
)

// Inert fixtures own the sampler, so inspection needs no worker synchronization.
func windowRecoveryServiceSampleAt(t *testing.T, service *windowPacingService, at time.Time) windowServiceSample {
	t.Helper()
	for _, sample := range service.samples {
		if sample.lastAtNanos == at.UnixNano() && sample.bytes > 0 {
			return sample
		}
	}
	t.Fatal("the actual ACK did not publish its sample")
	return windowServiceSample{}
}

// Confirmed originals can precede all RTT metadata. Their zero baseline must
// keep the established cold raw-clock fallback, despite sample.complete=false.
func TestWindowServiceRecoveryColdOriginalWithoutMinimumRetainsFallback(t *testing.T) {
	start := time.Unix(1700000000, 0)
	fixture := newWindowReceiverCreditFixture(t, nil, start, 10*time.Millisecond)
	left := fixture.write(0, 2315, start)
	right := fixture.write(1, 154, start)
	fixture.write(2, 3445, start)
	leftAt := start.Add(100 * time.Millisecond)
	rightAt := leftAt.Add(16298750 * time.Nanosecond)
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
	sample := windowRecoveryServiceSampleAt(t, fixture.service, rightAt)
	if fixture.service.roundTripEvidence(rightAt).minimum != 0 || sample.supply.minimumPath != 0 ||
		!sample.supply.recorded || sample.supply.complete || !sample.supply.rawClock ||
		!left.rttH1 || !right.rttH1 || left.rttState != sendItemRttWriteConfirmed || right.rttState != sendItemRttWriteConfirmed {
		t.Fatalf("cold confirmed originals acquired or lost timing provenance: %+v", sample)
	}
	if rate := fixture.measured(rightAt); rate != 9448 || fixture.service.total != 2469 || fixture.sequence.windowPacer.serviceAcked != 2469 {
		t.Fatalf("cold first-send fallback changed: rate=%d credit=%d", rate, fixture.service.total)
	}
}

// The left checkpoint is excluded from the selected numerator. Its pending
// initial write cannot erase the complete physical offer of the selected right.
func TestWindowServiceRecoveryIncompleteExcludedLeftKeepsCompleteRight(t *testing.T) {
	service, start := newWindowQualifiedServiceFixture(t, 100000, 10*time.Millisecond, time.Millisecond)
	fixture := newWindowReceiverCreditFixture(t, service, start, 10*time.Millisecond)
	left := fixture.offer(0, 2315, start)
	right := fixture.write(1, 154, start)
	fixture.write(2, 3445, start)
	prior := service.total
	leftAt := start.Add(time.Second)
	rightAt := leftAt.Add(16298750 * time.Nanosecond)
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
	leftSample := windowRecoveryServiceSampleAt(t, service, leftAt)
	rightSample := windowRecoveryServiceSampleAt(t, service, rightAt)
	if left.rttState != sendItemRttWritePending || leftSample.supply.complete || !leftSample.supply.recorded ||
		!rightSample.supply.recorded || !rightSample.supply.complete || rightSample.bytes != 154 || rightSample.supply.minimumPath <= 0 {
		t.Fatalf("excluded-left ambiguity leaked into selected-right evidence: left=%+v right=%+v", leftSample, rightSample)
	}
	if rate := fixture.measured(rightAt); rate != 9448 || service.total-prior != 2469 {
		t.Fatalf("excluded left vetoed the complete selected pair: rate=%d credit=%d", rate, service.total-prior)
	}
}

// A legacy constituent remains real delivery but cannot borrow the selected
// original's recorded physical completeness when their byte counts merge.
func TestWindowServiceRecoveryMixedSelectedCreditKeepsIncompleteSupply(t *testing.T) {
	service, start := newWindowQualifiedServiceFixture(t, 100000, 10*time.Millisecond, time.Millisecond)
	fixture := newWindowReceiverCreditFixture(t, service, start, 10*time.Millisecond)
	left := fixture.write(0, 2315, start)
	right := fixture.write(1, 154, start)
	fixture.write(2, 3445, start)
	prior := service.total
	leftAt := start.Add(time.Second)
	rightAt := leftAt.Add(16298750 * time.Nanosecond)
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
	// This bare legacy lane owns its own sent/delivered bytes; the recorded
	// sequence's one-shot credit remains exactly the two actual ACKs above.
	service.sent += 17
	service.observe(17, rightAt)
	sample := windowRecoveryServiceSampleAt(t, service, rightAt)
	if sample.bytes != 171 || !sample.supply.recorded || sample.supply.complete ||
		!sample.supply.rawClock || sample.supply.minimumPath <= 0 || sample.supply.headComplete {
		t.Fatalf("mixed selected credit lost its incomplete physical provenance: %+v", sample)
	}
	ack := fixture.ack(right, rightAt, 0)
	ack.selective, ack.receiverAckDelaySet = true, false
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, ack)
	if service.total-prior != 2486 || fixture.sequence.windowPacer.serviceAcked != 2469 {
		t.Fatal("mixed classification changed exact-once recorded or legacy bytes")
	}
	if rate := fixture.measured(rightAt); rate != 100000 || service.serviceHoldRate != 100000 {
		t.Errorf("mixed incomplete selected credit repriced service: rate=%d hold=%d", rate, service.serviceHoldRate)
	}
}
