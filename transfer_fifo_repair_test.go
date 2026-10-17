// Boundary tests keep exact FIFO proof separate from retry policy and ownership.
package connect

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Inert retained ownership allows each proof exclusion to vary independently
// of the actual-worker root regressions in the companion test file.
type fifoRepairFixture struct {
	sequence *SendSequence
	items    []*sendItem
	writer   *windowPacingPolicyWriter
	route    Route
	now      time.Time
}

// Build an unchanged physical FIFO with inert, confirmed retained originals.
func newFifoRepairFixture() fifoRepairFixture {
	now := time.Now()
	start := now.Add(-time.Second)
	sequence, items := newSelectiveAckRecoveryTestSequence(3, start)
	route := make(Route, 1)
	writer := &windowPacingPolicyWriter{policy: transferFlightPolicySnapshot{generation: 2, h1Only: true}}
	sequence.client = &Client{feedbackTimeBase: start}
	sequence.flightController = newSendFlightController(sequence.sendBufferSettings)
	sequence.contractMultiRouteWriter = writer
	sequence.sendBufferSettings.DeliverySizedWindowScale = 2
	sequence.lastCumulativeAckTime = now
	sequence.fifoRepair = fifoRepairState{generation: 2, generationStartNanos: start.UnixNano()}
	sequence.windowPacer.service = &windowPacingService{next: now.Add(time.Second)}
	sequence.windowPacer.serviceReserved = 1280
	for _, item := range items {
		item.expectsAck, item.reliableCarrierObserved, item.rttH1 = true, true, true
		item.carrierRoute, item.reliableRoute = route, route
		item.rttState = sendItemRttWriteConfirmed
		item.pacingSentAtNanos = start.Add(100 * time.Millisecond).UnixNano()
		item.ackTimeout = time.Minute
		item.deferralOutstanding, item.timeoutDeferCount = true, 7
		item.timeoutDeferAckTime = start
		sequence.setResendTime(item, now.Add(time.Second))
		sequence.ackLifetimes.update(item)
	}
	return fifoRepairFixture{sequence: sequence, items: items, writer: writer, route: route, now: now}
}

// Every omitted physical or envelope premise must leave ordinary policy intact.
func TestFifoRepairRequiresExactPhysicalProvenance(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(fifoRepairFixture, *sequenceTag, *sequenceTag, *bool)
		write   func(fifoRepairFixture, *transferWriteDisposition)
		ack     func(fifoRepairFixture, *sequenceTag)
		want    bool
	}{
		{name: "complete exact proof", want: true},
		{name: "zero original tag", prepare: func(_ fifoRepairFixture, original, _ *sequenceTag, _ *bool) { original.sendTime = 0 }},
		{name: "absent original tag", prepare: func(_ fifoRepairFixture, original, _ *sequenceTag, _ *bool) { original.set = false }},
		{name: "equal promoted tag", prepare: func(_ fifoRepairFixture, original, promoted *sequenceTag, _ *bool) {
			promoted.sendTime = original.sendTime
		}},
		{name: "decreasing promoted tag", prepare: func(_ fifoRepairFixture, _, promoted *sequenceTag, _ *bool) { promoted.sendTime = 1 }},
		{name: "zero promoted tag", prepare: func(_ fifoRepairFixture, _, promoted *sequenceTag, _ *bool) { promoted.sendTime = 0 }},
		{name: "absent promoted tag", prepare: func(_ fifoRepairFixture, _, promoted *sequenceTag, _ *bool) { promoted.set = false }},
		{name: "existing head envelope", prepare: func(f fifoRepairFixture, _, _ *sequenceTag, _ *bool) { f.items[0].head = true }},
		{name: "contract proof rewrite", prepare: func(_ fifoRepairFixture, _, _ *sequenceTag, force *bool) { *force = true }},
		{name: "original before generation", prepare: func(f fifoRepairFixture, _, _ *sequenceTag, _ *bool) {
			f.items[1].pacingSentAtNanos = f.sequence.fifoRepair.generationStartNanos - 1
		}},
		{name: "original after creation", prepare: func(f fifoRepairFixture, _, _ *sequenceTag, _ *bool) {
			f.items[1].pacingSentAtNanos = f.now.Add(time.Hour).UnixNano()
		}},
		{name: "unconfirmed original", prepare: func(f fifoRepairFixture, _, _ *sequenceTag, _ *bool) {
			// An initial pending write has not acquired successful H1 evidence.
			f.items[1].rttState, f.items[1].rttH1 = sendItemRttWritePending, false
		}},
		{name: "original not H1", prepare: func(f fifoRepairFixture, _, _ *sequenceTag, _ *bool) { f.items[1].rttH1 = false }},
		{name: "no monotonic epoch", prepare: func(f fifoRepairFixture, _, _ *sequenceTag, _ *bool) {
			f.sequence.client.feedbackTimeBase = time.Time{}
			f.sequence.fifoRepair = fifoRepairState{}
		}},
		{name: "unobserved original", prepare: func(f fifoRepairFixture, _, _ *sequenceTag, _ *bool) { f.items[1].reliableCarrierObserved = false }},
		{name: "changed original route", prepare: func(f fifoRepairFixture, _, _ *sequenceTag, _ *bool) { f.items[1].carrierChanged = true }},
		{name: "other original route", prepare: func(f fifoRepairFixture, _, _ *sequenceTag, _ *bool) { f.items[1].carrierRoute = make(Route) }},
		{name: "unreliable original", prepare: func(f fifoRepairFixture, _, _ *sequenceTag, _ *bool) { f.items[1].unreliableCarrierObserved = true }},
		{name: "hybrid original", prepare: func(f fifoRepairFixture, _, _ *sequenceTag, _ *bool) { f.items[1].hybridReliableCarrierObserved = true }},
		{name: "leased original", prepare: func(f fifoRepairFixture, _, _ *sequenceTag, _ *bool) { f.items[1].selectiveAcked = true }},
		{name: "delivered original", prepare: func(f fifoRepairFixture, _, _ *sequenceTag, _ *bool) { f.items[1].deliveryObserved = true }},
		{name: "explicit original recovery", prepare: func(f fifoRepairFixture, _, _ *sequenceTag, _ *bool) {
			f.items[1].recoveryKind = sendRecoverySelectiveGap
		}},
		{name: "mixed profile", prepare: func(f fifoRepairFixture, _, _ *sequenceTag, _ *bool) { f.writer.policy.h1Only = false }},
		{name: "unpaced profile", prepare: func(f fifoRepairFixture, _, _ *sequenceTag, _ *bool) {
			f.sequence.sendBufferSettings.DeliverySizedWindowScale = 0
		}},
		{name: "generation before physical confirmation", write: func(f fifoRepairFixture, _ *transferWriteDisposition) { f.writer.policy.generation++ }},
		{name: "physical other route", write: func(_ fifoRepairFixture, d *transferWriteDisposition) { d.route = make(Route) }},
		{name: "physical unreliable route", write: func(_ fifoRepairFixture, d *transferWriteDisposition) { d.unreliable, d.reliable = true, false }},
		{name: "physical hybrid route", write: func(_ fifoRepairFixture, d *transferWriteDisposition) { d.hybridReliable = true }},
		{name: "physical unknown route", write: func(_ fifoRepairFixture, d *transferWriteDisposition) { d.reliable = false }},
		{name: "physical other transport", write: func(_ fifoRepairFixture, d *transferWriteDisposition) { d.transportType = TransportTypeH3 }},
		{name: "generation before named ACK", ack: func(f fifoRepairFixture, _ *sequenceTag) { f.writer.policy.generation++ }},
		{name: "unknown ACK tag", ack: func(_ fifoRepairFixture, tag *sequenceTag) { tag.sendTime++ }},
		{name: "absent ACK tag", ack: func(_ fifoRepairFixture, tag *sequenceTag) { tag.set = false }},
		{name: "recycled prefix identity", ack: func(f fifoRepairFixture, _ *sequenceTag) { f.items[0].messageId = NewId() }},
	} {
		t.Log(test.name)
		f := newFifoRepairFixture()
		original, promoted := sequenceTag{set: true, sendTime: 100}, sequenceTag{set: true, sendTime: 200}
		force := false
		if test.prepare != nil {
			test.prepare(f, &original, &promoted, &force)
		}
		f.sequence.prepareFifoRepair(f.items[0], original, promoted, force)
		disposition := transferWriteDisposition{transportType: TransportTypeH1, reliable: true, route: f.route}
		if test.write != nil {
			test.write(f, &disposition)
		}
		f.sequence.observeCarrierWrite(f.items[0], disposition)
		tag := promoted
		if test.ack != nil {
			test.ack(f, &tag)
		}
		proof := f.sequence.acknowledgedFifoRepair(f.items[0], tag)
		if proof.set != test.want {
			t.Fatalf("named ACK proof=%t want=%t", proof.set, test.want)
		}
	}
}

// Once physical provenance is established, applying it changes only an already
// outstanding future deferral. The original timer, heaps and lifetime stay owned.
func TestFifoRepairPreservesRecoveryOwnership(t *testing.T) {
	for _, test := range []struct {
		name         string
		change       func(fifoRepairFixture)
		mark, revoke bool
	}{
		{name: "future ordinary deferral", mark: true, revoke: true},
		{name: "first deferral not yet taken", mark: true, change: func(f fifoRepairFixture) { f.items[1].deferralOutstanding = false; f.items[1].timeoutDeferCount = 0 }},
		{name: "already due ordinary deferral", mark: true, change: func(f fifoRepairFixture) { f.sequence.setResendTime(f.items[1], f.now.Add(-time.Second)) }},
		{name: "leased head", change: func(f fifoRepairFixture) { f.items[1].selectiveAcked = true }},
		{name: "delivered head", change: func(f fifoRepairFixture) { f.items[1].deliveryObserved = true }},
		{name: "explicit recovery", change: func(f fifoRepairFixture) { f.items[1].recoveryKind = sendRecoveryContractMissing }},
		{name: "changed route", change: func(f fifoRepairFixture) { f.items[1].carrierChanged = true }},
		{name: "generation changed after snapshot", change: func(f fifoRepairFixture) { f.writer.policy.generation++ }},
		{name: "head no longer retained", change: func(f fifoRepairFixture) { f.sequence.resendQueue.RemoveByMessageId(f.items[1].messageId) }},
		{name: "different exposed head", change: func(f fifoRepairFixture) { f.sequence.sendItems = f.sequence.sendItems[1:] }},
		{name: "reused successor number", change: func(f fifoRepairFixture) { f.items[1].sequenceNumber++ }},
	} {
		t.Log(test.name)
		f := newFifoRepairFixture()
		s, prefix, item := f.sequence, f.items[0], f.items[1]
		tag := sequenceTag{set: true, sendTime: 200}
		s.prepareFifoRepair(prefix, sequenceTag{set: true, sendTime: 100}, tag, false)
		s.observeCarrierWrite(prefix, transferWriteDisposition{transportType: TransportTypeH1, reliable: true, route: f.route})
		proof := s.acknowledgedFifoRepair(prefix, tag)
		if !proof.set {
			t.Fatal("missing physical premise")
		}
		s.resendQueue.RemoveByMessageId(prefix.messageId)
		s.ackLifetimes.remove(prefix)
		s.forgetFifoRepairItem(prefix)
		s.sendItems = s.sendItems[1:]
		if test.change != nil {
			test.change(f)
		}
		deadline, outstanding, kind := item.resendTime, item.deferralOutstanding, item.recoveryKind
		count, bytes := s.resendQueue.QueueSize()
		retained, index := s.resendQueue.GetByMessageId(item.messageId), item.ackLifetimeIndex
		lifetime, sends, deferrals, deferAck := item.sendTime.Add(item.ackTimeout), item.sendCount, item.timeoutDeferCount, item.timeoutDeferAckTime
		s.pendingRecovery = &sendRecoverySelection{messageId: item.messageId, number: item.sequenceNumber, sendTime: item.sendTime, due: deadline, sendCount: sends}
		s.applyFifoRepair(proof, f.now)
		if s.fifoRepairPassed(item) != test.mark {
			t.Fatalf("passed-head mark=%t want=%t", s.fifoRepairPassed(item), test.mark)
		}
		if test.revoke {
			if item.resendTime != f.now || item.deferralOutstanding || s.selectedRecoveryItem() != nil {
				t.Fatal("future deferral was not revoked with stale selection invalidated")
			}
		} else if item.resendTime != deadline || item.deferralOutstanding != outstanding {
			t.Fatal("untouched or earlier timer changed")
		}
		if test.mark && s.shouldDeferTimeoutResend(item, time.Second) {
			t.Fatal("accepted FIFO proof allowed a new queue-residence deferral")
		}
		newCount, newBytes := s.resendQueue.QueueSize()
		if newCount != count || newBytes != bytes || s.resendQueue.GetByMessageId(item.messageId) != retained ||
			item.sendCount != sends || item.timeoutDeferCount != deferrals || item.timeoutDeferAckTime != deferAck ||
			item.recoveryKind != kind || item.ackLifetimeIndex != index || item.sendTime.Add(item.ackTimeout) != lifetime ||
			s.windowPacer.service.next != f.now.Add(time.Second) || s.windowPacer.serviceReserved != 1280 {
			t.Fatal("proof changed retention, backoff, lifetime or pacing ownership")
		}
	}
}

// A new pending promotion must not erase accepted evidence for its own head.
// Successful dispatch can consume that evidence and confirm the new tuple.
func TestFifoRepairAcceptedHeadSurvivesFailedPromotion(t *testing.T) {
	f := newFifoRepairFixture()
	s, head, next := f.sequence, f.items[0], f.items[1]
	s.fifoRepair.passed = fifoPassedHead{messageId: head.messageId, number: head.sequenceNumber, route: f.route, set: true}
	s.prepareFifoRepair(head, sequenceTag{set: true, sendTime: 100}, sequenceTag{set: true, sendTime: 200}, false)
	if !s.fifoRepair.pending.set || s.fifoRepair.pending.successful || !s.fifoRepairPassed(head) {
		t.Fatal("new unconfirmed envelope erased accepted head proof")
	}
	s.observeCarrierWrite(next, transferWriteDisposition{transportType: TransportTypeH1, reliable: true, route: f.route})
	if s.fifoRepair.pending.set || !s.fifoRepairPassed(head) {
		t.Fatal("successor copy did not independently invalidate only its pending proof")
	}
	s.prepareFifoRepair(head, sequenceTag{set: true, sendTime: 100}, sequenceTag{set: true, sendTime: 200}, false)
	s.observeCarrierWrite(head, transferWriteDisposition{transportType: TransportTypeH1, reliable: true, route: f.route})
	if s.fifoRepairPassed(head) || !s.fifoRepair.pending.successful {
		t.Fatal("successful head copy did not consume accepted proof and confirm its successor")
	}
	created := s.fifoRepair.pending.createdNanos
	s.observeCarrierWrite(head, transferWriteDisposition{transportType: TransportTypeH1, reliable: true, route: f.route})
	if s.fifoRepair.pending.createdNanos != created {
		t.Fatal("repeated envelope acquired a newer physical cutoff")
	}
}

// The snapshot survives real pool return, while neither an old nor recycled
// message identity can inherit the sequence's accepted marker.
func TestFifoRepairSnapshotSurvivesPoolReuse(t *testing.T) {
	assertMessagePoolOwnership(t)
	clearSendItemPool()
	f := newFifoRepairFixture()
	s, prefix := f.sequence, f.items[0]
	tag := sequenceTag{set: true, sendTime: 200}
	s.prepareFifoRepair(prefix, sequenceTag{set: true, sendTime: 100}, tag, false)
	s.observeCarrierWrite(prefix, transferWriteDisposition{transportType: TransportTypeH1, reliable: true, route: f.route})
	proof := s.acknowledgedFifoRepair(prefix, tag)
	s.resendQueue.RemoveByMessageId(prefix.messageId)
	s.ackLifetimes.remove(prefix)
	s.sendItems = s.sendItems[1:]
	prefix.transferFrameBytes = nil
	s.returnSendItem(prefix)
	reused := takeSendItem()
	if reused != prefix {
		t.Fatal("pool reuse boundary not exercised")
	}
	defer reused.messagePoolReturn()
	*reused = sendItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: 999}}
	s.applyFifoRepair(proof, f.now)
	if !s.fifoRepairPassed(f.items[1]) || s.fifoRepairPassed(reused) {
		t.Fatal("pool reuse changed the value snapshot or inherited its mark")
	}
	s.forgetFifoRepairItem(f.items[1])
	if s.fifoRepair.passed.set {
		t.Fatal("delivery retained a stale pooled identity")
	}
}

// This carrier consumes successful shares and leaves refused shares with the
// caller, so the production selected-recovery path owns both outcomes.
type fifoRepairCopyWriter struct {
	windowPacingPolicyWriter
	route Route
	fail  bool
}

func (self *fifoRepairCopyWriter) writeDetailedWithCarrier(_ context.Context, wire []byte, _ time.Duration) (bool, transferWriteDisposition, error) {
	if self.fail {
		return false, transferWriteDisposition{}, nil
	}
	self.route <- wire
	return true, transferWriteDisposition{transportType: TransportTypeH1, reliable: true, route: self.route}, nil
}

// A selected copy is a physical write even when sendCount stays one. There is
// no SACK/delivery hook in this test that could independently clear the proof.
func TestFifoRepairSelectedPhysicalCopyInvalidatesProof(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, failed := range []bool{false, true} {
		func() {
			s, item, _, _ := newRecoveryAccountingFixture(t)
			route := make(Route, 1)
			writer := &fifoRepairCopyWriter{route: route, fail: failed}
			s.contractMultiRouteWriter = writer
			item.carrierRoute, item.reliableCarrierObserved = route, true
			item.recoveryKind = sendRecoverySelectiveGap
			s.fifoRepair.pending = fifoRepairFence{set: true, successful: true, prefixMessageId: NewId(), nextMessageId: item.messageId, nextNumber: item.sequenceNumber, route: route}
			s.fifoRepair.passed = fifoPassedHead{set: true, messageId: item.messageId, number: item.sequenceNumber, route: route}
			err := s.writeSelectedRecovery(item, sendRecoverySelection{kind: sendRecoverySelectiveGap})
			if err != nil {
				t.Fatal(err)
			}
			if item.sendCount != 1 || item.selectiveAcked || item.deliveryObserved || s.fifoRepair.pending.set != failed || s.fifoRepair.passed.set != failed {
				t.Fatalf("failed=%t: selected physical outcome lost proof ownership or changed ordinary count", failed)
			}
			if failed {
				if len(route) != 0 || s.resendWriteCount.Load() != 0 {
					t.Fatal("refused copy claimed physical success")
				}
			} else {
				wire := <-route
				pack := decodeSendPackLifecycleWirePack(t, wire)
				MessagePoolReturn(wire)
				if string(pack.MessageId) != string(item.messageId.Bytes()) || s.resendWriteCount.Load() != 1 || item.carrierChanged {
					t.Fatal("selected copy did not use the unchanged physical FIFO")
				}
			}
		}()
	}
}

// A completed earlier copy invalidates RTT sampling, not FIFO ordering. An
// initial refusal followed by a successful retry still lacks initial-H1 proof.
func TestFifoRepairPriorPhysicalCopiesPreserveInitialProof(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, test := range []struct {
		name         string
		initialFails bool
		kind         sendRecoveryKind
	}{
		{name: "earlier ordinary copy", kind: sendRecoveryNone},
		{name: "earlier selected eviction copy with unchanged ordinary count", kind: sendRecoveryEviction},
		{name: "failed initial then successful ordinary copy", initialFails: true, kind: sendRecoveryNone},
		{name: "failed initial then successful selected eviction copy", initialFails: true, kind: sendRecoveryEviction},
	} {
		t.Log(test.name)
		func() {
			s, item, _, _ := newRecoveryAccountingFixture(t)
			s.client.feedbackTimeBase = time.Now().Add(-time.Second)
			floor := s.client.feedbackArrivalNanos(time.Now())
			route := make(Route, 1)
			writer := &fifoRepairCopyWriter{route: route, fail: test.initialFails,
				windowPacingPolicyWriter: windowPacingPolicyWriter{policy: transferFlightPolicySnapshot{generation: 2, h1Only: true}}}
			s.contractMultiRouteWriter = writer
			consume := func() {
				t.Helper()
				if len(route) != 1 {
					t.Fatal("successful physical copy was not written")
				}
				wire := <-route
				pack := decodeSendPackLifecycleWirePack(t, wire)
				MessagePoolReturn(wire)
				if string(pack.MessageId) != string(item.messageId.Bytes()) {
					t.Fatal("physical copy changed identity")
				}
			}
			disposition, err := s.writeMaybeWrappedBytes(item.transferFrameBytes, TransferPath{}, true, item, false, false)
			if test.initialFails {
				if !errors.Is(err, errTransferRouteWriteTimeout) || len(route) != 0 || item.rttH1 {
					t.Fatal("initial physical refusal acquired H1 evidence")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				s.observeCarrierWrite(item, disposition)
				consume()
				if !item.rttH1 || item.rttState != sendItemRttWriteConfirmed {
					t.Fatal("initial physical H1 write was not confirmed")
				}
			}
			first := item.pacingSentAtNanos
			writer.fail = false
			if err := s.writeSelectedRecovery(item, sendRecoverySelection{kind: test.kind}); err != nil {
				t.Fatal(err)
			}
			consume()
			wantSends := 2
			if test.kind == sendRecoveryEviction {
				wantSends = 1
			}
			if item.rttState != sendItemRttUnavailable || item.rttH1 == test.initialFails ||
				item.pacingSentAtNanos != first || item.sendCount != wantSends || s.resendWriteCount.Load() != 1 {
				t.Fatal("earlier copy did not preserve the initial physical evidence independently of RTT ambiguity")
			}
			prefix := &sendItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: 0},
				expectsAck: true, carrierRoute: route, reliableCarrierObserved: true}
			s.sendItems = append([]*sendItem{prefix}, s.sendItems...)
			s.sendBufferSettings.DeliverySizedWindowScale = 2
			s.fifoRepair = fifoRepairState{generation: 2, generationStartNanos: floor}
			s.prepareFifoRepair(prefix, sequenceTag{set: true, sendTime: 100}, sequenceTag{set: true, sendTime: 200}, false)
			if s.fifoRepair.pending.set == test.initialFails {
				t.Fatalf("physical initial failure=%t yielded pending proof=%t", test.initialFails, s.fifoRepair.pending.set)
			}
		}()
	}
}
