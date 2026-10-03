// These isolated roots exercise the diagnostic receiver sampler with explicit
// physical offer, ingress, and ACK transitions. No wire or normal test changes.
package connect

import (
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Generated identities and explicit clocks keep unrelated live traffic out.
type receiverPhysicalSpanFixture struct {
	start    time.Time
	receiver Id
	source   Id
	service  *windowPacingService
	number   uint64
}

// Each test owns and clears the bounded diagnostic ledger.
func newReceiverPhysicalSpanFixture(t *testing.T, rate ByteCount) *receiverPhysicalSpanFixture {
	t.Helper()
	t.Cleanup(startIngressCounterDiagnostic(t))
	return &receiverPhysicalSpanFixture{
		start:    time.Unix(1000, 0),
		receiver: NewId(),
		source:   NewId(),
		service:  &windowPacingService{serviceHoldRate: rate},
	}
}

// Sibling lanes deliberately share one physical-service estimate.
func (self *receiverPhysicalSpanFixture) sequence(lane uint32) *SendSequence {
	return &SendSequence{
		sequenceId:  NewId(),
		logicalLane: lane,
		resendQueue: newResendQueue(nil, 0),
		windowPacer: windowBurstPacer{service: self.service},
	}
}

// Publish only the already-confirmed first-write facts consumed by the sampler.
func (self *receiverPhysicalSpanFixture) offer(sequence *SendSequence, bytes ByteCount, at time.Duration) *protocol.Pack {
	messageId := NewId()
	self.service.stateLock.Lock()
	recordIngressOfferDiagnosticWithLock(self.service, sequence.sequenceId, messageId, self.start.Add(at), false)
	self.service.stateLock.Unlock()
	pack := &protocol.Pack{
		MessageId:      messageId.Bytes(),
		SequenceId:     sequence.sequenceId.Bytes(),
		SequenceNumber: self.number,
		LogicalLane:    sequence.logicalLane,
	}
	item := &sendItem{
		transferItem:      transferItem{messageId: messageId, messageByteCount: bytes, sequenceNumber: self.number},
		pacingByteCount:   bytes,
		pacingSentAtNanos: self.start.Add(at).UnixNano(),
		rttState:          sendItemRttObserved,
		rttH1:             true,
	}
	self.number++
	sequence.resendQueue.Add(item)
	self.service.sent += bytes
	sequence.windowPacer.serviceSent += bytes
	return pack
}

// Retried copies consume physical bytes while retaining the original tuple.
func (self *receiverPhysicalSpanFixture) ingress(pack *protocol.Pack, bytes ByteCount, at time.Duration) {
	recordIngressCounterDiagnostic(self.receiver, self.source, pack, sequenceTlsRoleServer, false, TransportTypeH1, bytes, self.start.Add(at))
}

// Delivery ownership is intentionally separate from diagnostic rate evidence.
func (self *receiverPhysicalSpanFixture) ack(t *testing.T, sequence *SendSequence, pack *protocol.Pack, at time.Duration) {
	t.Helper()
	messageId, err := IdFromBytes(pack.MessageId)
	if err != nil {
		t.Fatal(err)
	}
	observeIngressCounterDiagnostic(sequence, receiveAckMessage{messageId: messageId, receivedAtNanos: self.start.Add(at).UnixNano()})
}

// The normal retaining consumer must expose the corrected rate.
func (self *receiverPhysicalSpanFixture) requireRate(t *testing.T, at time.Duration, expected ByteCount) {
	t.Helper()
	rate, _, latest := self.service.measured(time.Second, self.start.Add(at))
	if actual := max(rate, latest); actual != expected {
		t.Fatalf("receiver physical service=%d B/s; expected=%d B/s", actual, expected)
	}
}

// A receiver-clock interval is not a lower rate merely because the sender paced
// its named endpoints a little farther apart than their physical arrivals.
func TestWindowReceiverIngressClockDoesNotMeasureOwnPacing(t *testing.T) {
	fixture := newReceiverPhysicalSpanFixture(t, 12500000)
	sequence := fixture.sequence(0)
	first := fixture.offer(sequence, 2673, 0)
	last := fixture.offer(sequence, 8019, 675284*time.Nanosecond)
	fixture.ingress(first, 2673, 50*time.Millisecond)
	fixture.ingress(last, 8019, 50*time.Millisecond+641520*time.Nanosecond)
	fixture.ack(t, sequence, first, 100*time.Millisecond)
	fixture.ack(t, sequence, last, 101*time.Millisecond)
	fixture.requireRate(t, 101*time.Millisecond, 12500000)
}

// A retry consumes link time, but cannot create another logical delivery credit.
func TestWindowReceiverIngressCountsRetrySerializationOncePerPhysicalCopy(t *testing.T) {
	fixture := newReceiverPhysicalSpanFixture(t, 12500000)
	sequence := fixture.sequence(0)
	var retries []*protocol.Pack
	for i := 0; i < 6; i++ {
		pack := fixture.offer(sequence, 2673, -time.Second+time.Duration(i)*213840*time.Nanosecond)
		fixture.ingress(pack, 2673, 48500*time.Microsecond+time.Duration(i)*213840*time.Nanosecond)
		messageId, _ := IdFromBytes(pack.MessageId)
		sequence.resendQueue.stateLock.Lock()
		sequence.resendQueue.messageIdItems[messageId].rttState = sendItemRttUnavailable
		sequence.resendQueue.stateLock.Unlock()
		retries = append(retries, pack)
	}
	// Both measured endpoints remain unambiguous; only other older messages retry.
	first := fixture.offer(sequence, 2673, 0)
	last := fixture.offer(sequence, 1383, 0)
	fixture.ingress(first, 2673, 50*time.Millisecond)
	for i, pack := range retries {
		fixture.ingress(pack, 2673, 50*time.Millisecond+time.Duration(i+1)*213840*time.Nanosecond)
	}
	fixture.ingress(last, 1383, 50*time.Millisecond+1393680*time.Nanosecond)
	fixture.ack(t, sequence, first, 100*time.Millisecond)
	fixture.ack(t, sequence, last, 102*time.Millisecond)
	fixture.requireRate(t, 102*time.Millisecond, 12500000)

	firstId, _ := IdFromBytes(first.MessageId)
	lastId, _ := IdFromBytes(last.MessageId)
	sequence.publishAckServiceCredit(firstId, true, fixture.start.Add(102*time.Millisecond))
	sequence.publishAckServiceCredit(lastId, false, fixture.start.Add(103*time.Millisecond))
	sequence.publishAckServiceCredit(firstId, true, fixture.start.Add(104*time.Millisecond))
	sequence.publishAckServiceCredit(lastId, false, fixture.start.Add(105*time.Millisecond))
	if fixture.service.total != 20094 || sequence.windowPacer.serviceAcked != 20094 {
		t.Fatalf("physical retries changed logical ownership: total=%d credited=%d", fixture.service.total, sequence.windowPacer.serviceAcked)
	}
}

// Matching the current sender pace is insufficient evidence of lower capacity.
func TestWindowReceiverIngressPacedOfferCannotLowerCapacity(t *testing.T) {
	fixture := newReceiverPhysicalSpanFixture(t, 10000000)
	sequence := fixture.sequence(0)
	first := fixture.offer(sequence, 2673, 0)
	last := fixture.offer(sequence, 7600, 800*time.Microsecond)
	fixture.ingress(first, 2673, 50*time.Millisecond)
	fixture.ingress(last, 7600, 50*time.Millisecond+800*time.Microsecond)
	fixture.ack(t, sequence, first, 100*time.Millisecond)
	fixture.ack(t, sequence, last, 101*time.Millisecond)
	fixture.requireRate(t, 101*time.Millisecond, 10000000)
}

// A genuinely slower receiver replaces the hold when the actual offered train
// spans much less time than the receiver's physical serialization interval.
func TestWindowReceiverIngressAcceptsGenuineSlowerSerialization(t *testing.T) {
	fixture := newReceiverPhysicalSpanFixture(t, 12500000)
	sequence := fixture.sequence(0)
	first := fixture.offer(sequence, 2673, 0)
	fixture.ingress(first, 2673, 50*time.Millisecond)
	var last *protocol.Pack
	for i := 1; i <= 10; i++ {
		last = fixture.offer(sequence, 8019, time.Duration(i)*100*time.Microsecond)
		fixture.ingress(last, 8019, 50*time.Millisecond+time.Duration(i)*6415200*time.Nanosecond)
	}
	fixture.ack(t, sequence, first, 100*time.Millisecond)
	fixture.ack(t, sequence, last, 165*time.Millisecond)
	fixture.requireRate(t, 165*time.Millisecond, 1250000)
}

// A new physical offer after prior feedback cannot price the intervening idle.
func TestWindowReceiverIngressSourceIdleKeepsCapacity(t *testing.T) {
	fixture := newReceiverPhysicalSpanFixture(t, 12500000)
	sequence := fixture.sequence(0)
	first := fixture.offer(sequence, 2673, 0)
	second := fixture.offer(sequence, 2673, 100*time.Microsecond)
	fixture.ingress(first, 2673, 50*time.Millisecond)
	fixture.ingress(second, 2673, 50*time.Millisecond+213840*time.Nanosecond)
	fixture.ack(t, sequence, first, 100*time.Millisecond)
	fixture.ack(t, sequence, second, 101*time.Millisecond)
	fixture.requireRate(t, 101*time.Millisecond, 12500000)

	last := fixture.offer(sequence, 2673, time.Second)
	fixture.ingress(last, 2673, time.Second+50*time.Millisecond)
	fixture.ack(t, sequence, last, time.Second+100*time.Millisecond)
	fixture.requireRate(t, time.Second+100*time.Millisecond, 12500000)
	if ingressCounterDiagnostic.resets != 1 || ingressCounterDiagnostic.pairs != 1 {
		t.Fatalf("idle created rate evidence: resets=%d pairs=%d", ingressCounterDiagnostic.resets, ingressCounterDiagnostic.pairs)
	}
}

// Receiver counters span logical lanes, while an older applied ACK cannot
// rewind the shared physical checkpoint or duplicate its byte interval.
func TestWindowReceiverIngressSiblingAndReorderedAckUseOneCounter(t *testing.T) {
	fixture := newReceiverPhysicalSpanFixture(t, 12500000)
	firstSequence, secondSequence := fixture.sequence(1), fixture.sequence(2)
	first := fixture.offer(firstSequence, 2673, 0)
	last := fixture.offer(secondSequence, 8019, 100*time.Microsecond)
	fixture.ingress(first, 2673, 50*time.Millisecond)
	fixture.ingress(last, 8019, 50*time.Millisecond+641520*time.Nanosecond)
	fixture.ack(t, firstSequence, first, 100*time.Millisecond)
	fixture.ack(t, secondSequence, last, 101*time.Millisecond)
	fixture.requireRate(t, 101*time.Millisecond, 12500000)
	fixture.ack(t, firstSequence, first, 102*time.Millisecond)
	fixture.requireRate(t, 102*time.Millisecond, 12500000)
	if len(ingressCounterDiagnostic.groups) != 1 || ingressCounterDiagnostic.pairs != 1 {
		t.Fatalf("sibling or old ACK split rate evidence: groups=%d pairs=%d", len(ingressCounterDiagnostic.groups), ingressCounterDiagnostic.pairs)
	}
}

// Other physical carriers and no-ACK Packs do not enter the H1 service counter;
// a different logical service generation cannot complete the old one's pair.
func TestWindowReceiverIngressPreservesPhysicalAndLogicalScope(t *testing.T) {
	fixture := newReceiverPhysicalSpanFixture(t, 12500000)
	sequence := fixture.sequence(0)
	first := fixture.offer(sequence, 2673, 0)
	last := fixture.offer(sequence, 8019, 100*time.Microsecond)
	fixture.ingress(first, 2673, 50*time.Millisecond)
	for _, transport := range []TransportType{TransportTypeUnknown, TransportTypeH3} {
		recordIngressCounterDiagnostic(fixture.receiver, fixture.source, first, sequenceTlsRoleServer, false, transport, 8000, fixture.start.Add(50*time.Millisecond+100*time.Nanosecond))
	}
	first.Nack = true
	fixture.ingress(first, 8000, 50*time.Millisecond+100*time.Nanosecond)
	first.Nack = false
	fixture.ingress(last, 8019, 50*time.Millisecond+641520*time.Nanosecond)
	fixture.ack(t, sequence, first, 100*time.Millisecond)
	fixture.ack(t, sequence, last, 101*time.Millisecond)
	fixture.requireRate(t, 101*time.Millisecond, 12500000)

	other := fixture.offer(sequence, 8019, time.Millisecond)
	other.ForceStream = true
	fixture.ingress(other, 8019, 55*time.Millisecond)
	fixture.ack(t, sequence, other, 105*time.Millisecond)
	if len(ingressCounterDiagnostic.groups) != 2 || ingressCounterDiagnostic.pairs != 1 {
		t.Fatalf("logical services shared a pair: groups=%d pairs=%d", len(ingressCounterDiagnostic.groups), ingressCounterDiagnostic.pairs)
	}
}

// A retransmitted endpoint has no unambiguous physical first-write match and
// cannot make its first-copy offer time look like current pacing evidence.
func TestWindowReceiverIngressAmbiguousRetryEndpointIsUnavailable(t *testing.T) {
	fixture := newReceiverPhysicalSpanFixture(t, 12500000)
	sequence := fixture.sequence(0)
	first := fixture.offer(sequence, 2673, 0)
	last := fixture.offer(sequence, 8019, 100*time.Microsecond)
	fixture.ingress(first, 2673, 50*time.Millisecond)
	fixture.ingress(last, 8019, 55*time.Millisecond)
	lastId, _ := IdFromBytes(last.MessageId)
	sequence.resendQueue.stateLock.Lock()
	sequence.resendQueue.messageIdItems[lastId].rttState = sendItemRttUnavailable
	sequence.resendQueue.stateLock.Unlock()
	fixture.ack(t, sequence, first, 100*time.Millisecond)
	fixture.ack(t, sequence, last, 105*time.Millisecond)
	if ingressCounterDiagnostic.pairs != 0 {
		t.Fatal("ambiguous retry endpoint became a receiver rate pair")
	}
}

// Genuine slower arrival remains measurable while every source interval uses
// the normal 0.95 pacing factor of the previously held physical capacity.
func TestWindowReceiverIngressSlowerServiceBelowContinuouslyPacedOffer(t *testing.T) {
	fixture := newReceiverPhysicalSpanFixture(t, 12500000)
	sequence := fixture.sequence(0)
	first := fixture.offer(sequence, 2673, 0)
	fixture.ingress(first, 2673, 50*time.Millisecond)
	var last *protocol.Pack
	for i := 1; i <= 10; i++ {
		last = fixture.offer(sequence, 7600, time.Duration(i)*640*time.Microsecond)
		fixture.ingress(last, 7600, 50*time.Millisecond+time.Duration(i)*6080*time.Microsecond)
	}
	fixture.ack(t, sequence, first, 100*time.Millisecond)
	fixture.ack(t, sequence, last, 165*time.Millisecond)
	fixture.requireRate(t, 165*time.Millisecond, 1250000)
}
