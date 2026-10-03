// A deliberate local drain pause remains a supply boundary when its RTT proof
// is abandoned. This diagnostic changes only service evidence eligibility.
package connect

import (
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Enter the actual drain decision with an unambiguous, confirmed live tail.
func receiverControlledTrainPause(t *testing.T, fixture *receiverPhysicalSpanFixture, sequence *SendSequence, first *protocol.Pack) Id {
	t.Helper()
	service := fixture.service
	service.observeRoundTrip(time.Millisecond, 0, fixture.start.Add(-90*time.Millisecond))
	for ago := 8; ago > 0; ago-- {
		service.observeRoundTrip(100*time.Millisecond, 0, fixture.start.Add(-time.Duration(ago)*10*time.Millisecond))
	}
	messageId, err := IdFromBytes(first.MessageId)
	if err != nil {
		t.Fatal(err)
	}
	service.beginWrite(sequence.sequenceId, messageId, first.SequenceNumber, fixture.start, false)
	service.finishWrite(sequence.sequenceId, messageId, true)
	waiter := &windowPacingWaiter{deadline: fixture.start}
	delay, _ := service.admitBurst(fixture.start, 2671, false, waiter)
	if delay <= 2*time.Millisecond || service.drained || !service.drainServiceEpoch {
		t.Fatal("fixture did not return a real controlled drain wait")
	}
	return messageId
}

// Losing a physical tail's proof wakes the FIFO waiter, but cannot make the
// local pause between its old and new ingress endpoints into slow service.
func TestWindowReceiverIngressControlledAbortKeepsHold(t *testing.T) {
	fixture := newReceiverPhysicalSpanFixture(t, 12500000)
	sequence := fixture.sequence(0)
	first := fixture.offer(sequence, 2671, 0)
	fixture.ingress(first, 2671, 50*time.Millisecond)
	messageId := receiverControlledTrainPause(t, fixture, sequence, first)
	fixture.service.invalidateMessageProbe(sequence.sequenceId, messageId)
	var last *protocol.Pack
	for i := 1; i <= 24; i++ {
		last = fixture.offer(sequence, 2671, 2*time.Millisecond)
		fixture.ingress(last, 2671, 52*time.Millisecond+time.Duration(i)*213680*time.Nanosecond)
	}
	fixture.ack(t, sequence, first, 100*time.Millisecond)
	fixture.ack(t, sequence, last, 120*time.Millisecond)
	fixture.requireRate(t, 120*time.Millisecond, 12500000)
	if fixture.service.drained || !fixture.service.drainUntil.IsZero() || fixture.service.minRoundTrip != time.Millisecond {
		t.Fatal("rate evidence changed physical proof or liveness")
	}
}

// A confirmed drain and an abandoned drain have the same known offer pause;
// only the former is allowed to establish a later unqueued RTT probe.
func TestWindowReceiverIngressControlledSuccessKeepsHold(t *testing.T) {
	fixture := newReceiverPhysicalSpanFixture(t, 12500000)
	sequence := fixture.sequence(0)
	first := fixture.offer(sequence, 2671, 0)
	fixture.ingress(first, 2671, 50*time.Millisecond)
	messageId := receiverControlledTrainPause(t, fixture, sequence, first)
	fixture.service.acknowledgeWrite(sequence.sequenceId, messageId, first.SequenceNumber, false, 0, fixture.start.Add(100*time.Millisecond))
	if !fixture.service.drained {
		t.Fatal("covering physical tail did not prove drain")
	}
	var last *protocol.Pack
	for i := 1; i <= 24; i++ {
		last = fixture.offer(sequence, 2671, 100*time.Millisecond)
		fixture.ingress(last, 2671, 150*time.Millisecond+time.Duration(i)*213680*time.Nanosecond)
	}
	fixture.ack(t, sequence, first, 100*time.Millisecond)
	fixture.ack(t, sequence, last, 220*time.Millisecond)
	fixture.requireRate(t, 220*time.Millisecond, 12500000)
}

// An immediately aborted wait consumed no physical time and cannot discard a
// genuine slow pair from the continuously offered train.
func TestWindowReceiverIngressControlledImmediateAbortKeepsSlowEvidence(t *testing.T) {
	fixture := newReceiverPhysicalSpanFixture(t, 12500000)
	sequence := fixture.sequence(0)
	first := fixture.offer(sequence, 2671, 0)
	fixture.ingress(first, 2671, 50*time.Millisecond)
	messageId := receiverControlledTrainPause(t, fixture, sequence, first)
	fixture.service.invalidateMessageProbe(sequence.sequenceId, messageId)
	last := fixture.offer(sequence, 2671, 0)
	fixture.ingress(last, 2671, 55*time.Millisecond)
	fixture.ack(t, sequence, first, 100*time.Millisecond)
	fixture.ack(t, sequence, last, 120*time.Millisecond)
	fixture.requireRate(t, 120*time.Millisecond, 534200)
}

// The first actual retry consumes the one-shot boundary too. A later original
// write cannot spend the same abandoned pause again or manufacture RTT proof.
func TestWindowReceiverIngressControlledRetryConsumesOnce(t *testing.T) {
	fixture := newReceiverPhysicalSpanFixture(t, 12500000)
	sequence := fixture.sequence(0)
	first := fixture.offer(sequence, 2671, 0)
	messageId := receiverControlledTrainPause(t, fixture, sequence, first)
	fixture.service.beginWrite(sequence.sequenceId, messageId, first.SequenceNumber, fixture.start.Add(2*time.Millisecond), true)
	fixture.service.finishWrite(sequence.sequenceId, messageId, true)
	fixture.offer(sequence, 2671, 3*time.Millisecond)
	d := &ingressCounterDiagnostic
	d.stateLock.Lock()
	defer d.stateLock.Unlock()
	if d.controlledBoundaries != 1 || len(d.pauses) != 0 || fixture.service.drained || !fixture.service.roundTripProbe.sentAt.IsZero() {
		t.Fatalf("controlled pause was lost or repeated: count=%d retained=%d", d.controlledBoundaries, len(d.pauses))
	}
}
