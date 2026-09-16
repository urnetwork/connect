// Causal worker waits, not elapsed-gap thresholds, partition offered trains in
// this isolated diagnostic. The receiver tuple remains an optimistic wire bound.
package connect

import (
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A queued head remains blocked at a full window while other messages fly.
func receiverOfferTrainWindowWait(fixture *receiverPhysicalSpanFixture, sequence *SendSequence, start, end time.Duration) {
	recordIngressOfferWaitStart(sequence, fixture.start.Add(start), true, 1)
	recordIngressOfferWaitEnd(sequence, fixture.start.Add(end))
}

// An entire fast burst can produce only one named cumulative head. Its lack of
// a second within-train endpoint must retain the established service rate.
func TestWindowReceiverIngressOfferTrainSingleHeadKeepsHold(t *testing.T) {
	for _, pause := range []time.Duration{2 * time.Millisecond, 10128320 * time.Nanosecond} {
		fixture := newReceiverPhysicalSpanFixture(t, 12500000)
		sequence := fixture.sequence(0)
		first := fixture.offer(sequence, 2671, 0)
		fixture.ingress(first, 2671, 50*time.Millisecond)
		receiverOfferTrainWindowWait(fixture, sequence, 0, pause)
		var last *protocol.Pack
		for i := 1; i <= 24; i++ {
			last = fixture.offer(sequence, 2671, pause)
			fixture.ingress(last, 2671, 50*time.Millisecond+pause+time.Duration(i)*213680*time.Nanosecond)
		}
		fixture.ack(t, sequence, first, 100*time.Millisecond)
		fixture.ack(t, sequence, last, 120*time.Millisecond)
		fixture.requireRate(t, 120*time.Millisecond, 12500000)
		fixture.requireRate(t, 2*time.Second, 12500000)
	}
}

// A short application pause uses the same explicit worker-select fact. Its
// duration has no relationship to the receiver bucket width or prior rate.
func TestWindowReceiverIngressOfferTrainSourceWaitKeepsHold(t *testing.T) {
	fixture := newReceiverPhysicalSpanFixture(t, 12500000)
	sequence := fixture.sequence(0)
	first := fixture.offer(sequence, 2671, 0)
	fixture.ingress(first, 2671, 50*time.Millisecond)
	recordIngressOfferWaitStart(sequence, fixture.start, false, 0)
	recordIngressOfferWaitEnd(sequence, fixture.start.Add(2*time.Millisecond))
	var last *protocol.Pack
	for i := 1; i <= 24; i++ {
		last = fixture.offer(sequence, 2671, 2*time.Millisecond)
		fixture.ingress(last, 2671, 52*time.Millisecond+time.Duration(i)*213680*time.Nanosecond)
	}
	fixture.ack(t, sequence, first, 100*time.Millisecond)
	fixture.ack(t, sequence, last, 120*time.Millisecond)
	fixture.requireRate(t, 120*time.Millisecond, 12500000)
}

// A genuinely slower complete window spans multiple feedback turns while all
// messages retain their original continuously offered train identity.
func TestWindowReceiverIngressOfferTrainAcceptsSlowerWindow(t *testing.T) {
	fixture := newReceiverPhysicalSpanFixture(t, 12500000)
	sequence := fixture.sequence(0)
	first := fixture.offer(sequence, 2671, 0)
	fixture.ingress(first, 2671, 50*time.Millisecond)
	var last *protocol.Pack
	for i := 1; i <= 24; i++ {
		last = fixture.offer(sequence, 2671, 0)
		fixture.ingress(last, 2671, 50*time.Millisecond+time.Duration(i)*2136800*time.Nanosecond)
	}
	recordIngressOfferWaitStart(sequence, fixture.start, true, 1)
	fixture.ack(t, sequence, first, 100*time.Millisecond)
	// The real worker may resume after the first reply; the already-written
	// tail still belongs to the old train and can complete its slow evidence.
	recordIngressOfferWaitEnd(sequence, fixture.start.Add(100*time.Millisecond))
	fixture.ack(t, sequence, last, 160*time.Millisecond)
	fixture.requireRate(t, 160*time.Millisecond, 1250000)
}

// With no established rate and only one head per train, the available pairs do
// not identify serialization. Startup must remain unknown, not price idle.
func TestWindowReceiverIngressOfferTrainColdSingleHeadIsUnknown(t *testing.T) {
	fixture := newReceiverPhysicalSpanFixture(t, 0)
	sequence := fixture.sequence(0)
	first := fixture.offer(sequence, 2671, 0)
	fixture.ingress(first, 2671, 50*time.Millisecond)
	receiverOfferTrainWindowWait(fixture, sequence, 0, 2*time.Millisecond)
	var last *protocol.Pack
	for i := 1; i <= 24; i++ {
		last = fixture.offer(sequence, 2671, 2*time.Millisecond)
		fixture.ingress(last, 2671, 52*time.Millisecond+time.Duration(i)*213680*time.Nanosecond)
	}
	fixture.ack(t, sequence, first, 100*time.Millisecond)
	fixture.ack(t, sequence, last, 120*time.Millisecond)
	fixture.requireRate(t, 120*time.Millisecond, 0)
}

// A stalled lane cannot split a service train while a sibling keeps offering.
func TestWindowReceiverIngressOfferTrainActiveSiblingPreservesEvidence(t *testing.T) {
	fixture := newReceiverPhysicalSpanFixture(t, 12500000)
	firstSequence, secondSequence := fixture.sequence(1), fixture.sequence(2)
	first := fixture.offer(firstSequence, 2671, 0)
	fixture.offer(secondSequence, 2671, 0)
	fixture.ingress(first, 2671, 50*time.Millisecond)
	receiverOfferTrainWindowWait(fixture, firstSequence, 0, 2*time.Millisecond)
	last := fixture.offer(secondSequence, 2671, 2*time.Millisecond)
	fixture.ingress(last, 2671, 55*time.Millisecond)
	fixture.ack(t, firstSequence, first, 100*time.Millisecond)
	fixture.ack(t, secondSequence, last, 120*time.Millisecond)
	fixture.requireRate(t, 120*time.Millisecond, 534200)
}

// An all-lane wait creates one shared boundary when the first lane resumes.
// Subsequent sibling resumptions at that clock cannot fragment the new train.
func TestWindowReceiverIngressOfferTrainSiblingResumeSharesBoundary(t *testing.T) {
	fixture := newReceiverPhysicalSpanFixture(t, 12500000)
	firstSequence, secondSequence := fixture.sequence(1), fixture.sequence(2)
	fixture.offer(firstSequence, 2671, 0)
	fixture.offer(secondSequence, 2671, 0)
	recordIngressOfferWaitStart(firstSequence, fixture.start, true, 1)
	recordIngressOfferWaitStart(secondSequence, fixture.start, true, 1)
	recordIngressOfferWaitEnd(firstSequence, fixture.start.Add(2*time.Millisecond))
	first := fixture.offer(firstSequence, 2671, 2*time.Millisecond)
	recordIngressOfferWaitEnd(secondSequence, fixture.start.Add(2*time.Millisecond))
	last := fixture.offer(secondSequence, 2671, 2*time.Millisecond)
	fixture.ingress(first, 2671, 50*time.Millisecond)
	fixture.ingress(last, 2671, 55*time.Millisecond)
	fixture.ack(t, firstSequence, first, 100*time.Millisecond)
	fixture.ack(t, secondSequence, last, 120*time.Millisecond)
	fixture.requireRate(t, 120*time.Millisecond, 534200)
}

// An already-available notification takes no virtual time and provides no
// source-starvation evidence, even if the window was briefly marked full.
func TestWindowReceiverIngressOfferTrainImmediateWakeKeepsEvidence(t *testing.T) {
	fixture := newReceiverPhysicalSpanFixture(t, 12500000)
	sequence := fixture.sequence(0)
	first := fixture.offer(sequence, 2671, 0)
	fixture.ingress(first, 2671, 50*time.Millisecond)
	receiverOfferTrainWindowWait(fixture, sequence, 0, 0)
	last := fixture.offer(sequence, 2671, 0)
	fixture.ingress(last, 2671, 55*time.Millisecond)
	fixture.ack(t, sequence, first, 100*time.Millisecond)
	fixture.ack(t, sequence, last, 120*time.Millisecond)
	fixture.requireRate(t, 120*time.Millisecond, 534200)
}
