// An exact promoted-envelope receipt can establish physical FIFO passage for
// multiple older writes. Each later cumulative advance exposes at most one.
package connect

import (
	"bytes"
	"context"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// New state is inspected through reflection only to keep the complete test
// package compilable for the unchanged old-product causal run. These helpers
// run only in candidate controls; the old control calls public worker APIs.
func fifoCohortFlag(t *testing.T, item *sendItem, name string) bool {
	t.Helper()
	field := reflect.ValueOf(item).Elem().FieldByName(name)
	if !field.IsValid() || field.Kind() != reflect.Bool {
		t.Fatalf("candidate has no declared boolean %s", name)
	}
	return field.Bool()
}

func setFifoCohortLimit(t *testing.T, sequence *SendSequence, limit int) {
	t.Helper()
	field := reflect.ValueOf(sequence.sendBufferSettings).Elem().FieldByName("FifoRepairMaxItems")
	if !field.IsValid() || field.Kind() != reflect.Int || !field.CanSet() {
		t.Fatal("candidate has no declared configurable FIFO scan bound")
	}
	field.SetInt(int64(limit))
}

// This worker-edge fixture reuses real encoded originals, paid retained owners,
// ordinary ACK handling and callback cleanup from the freshness controls. It
// does not manufacture authenticated public ingress; the unchanged signed SDK
// roots provide that independent integration boundary.
type fifoCohortFixture struct {
	*timeoutProgressFreshnessFixture
	writer *windowPacingPolicyWriter
	route  Route
}

func newFifoCohortFixture(t *testing.T) *fifoCohortFixture {
	t.Helper()
	f := newTimeoutProgressFreshnessFixture(t, 5)
	route := make(Route, 1)
	writer := &windowPacingPolicyWriter{policy: transferFlightPolicySnapshot{generation: 7, h1Only: true}}
	s := f.sequence
	s.client.feedbackTimeBase = f.started.Add(-2 * time.Second)
	s.contractMultiRouteWriter = writer
	s.flightController = newSendFlightController(s.sendBufferSettings)
	s.fifoRepair = fifoRepairState{generation: 7, generationStartNanos: f.started.Add(-2 * time.Second).UnixNano()}
	s.lastCumulativeAckTime = f.started
	for _, item := range f.items {
		// Match the physical original to its worker-side state before any
		// promoted-envelope proof is captured. Reprice through real custody.
		var original protocol.TransferFrame
		if err := ProtoUnmarshal(item.transferFrameBytes, &original); err != nil {
			t.Fatal(err)
		}
		original.Pack.Head = false
		originalTag := sequenceTag{set: true, sendTime: uint64(f.started.Add(-100 * time.Millisecond).UnixMilli())}
		original.Pack.Tag = originalTag.protocol()
		wire, err := ProtoMarshal(&original)
		if err != nil {
			t.Fatal(err)
		}
		if s.detachResendItem(item.messageId) != item || !item.reserveFrameRewrite(ByteCount(len(wire)), len(original.Pack.Frames), s.sendBufferSettings.ProtocolVersion < 2) {
			MessagePoolReturn(wire)
			t.Fatal("could not retain the truthful original fixture frame")
		}
		s.replaceSendItemFrame(item, wire)
		s.addResendItem(item)
		item.head = false
		item.carrierRoute, item.reliableRoute = route, route
		item.expectsAck, item.reliableCarrierObserved, item.rttH1 = true, true, true
		item.rttState = sendItemRttWriteConfirmed
		item.pacingSentAtNanos = f.started.Add(-time.Second).UnixNano()
		item.deferralOutstanding = false
		item.timeoutDeferCount = 0
		item.timeoutDeferAckTime = time.Time{}
		s.ackLifetimes.update(item)
	}
	return &fifoCohortFixture{timeoutProgressFreshnessFixture: f, writer: writer, route: route}
}

func (f *fifoCohortFixture) prepare() sequenceTag {
	tag := sequenceTag{set: true, sendTime: uint64(time.Now().UnixMilli())}
	f.sequence.prepareFifoRepair(f.items[0], f.originalTag(), tag, false)
	return tag
}

func (f *fifoCohortFixture) originalTag() sequenceTag {
	return sequenceTag{set: true, sendTime: uint64(f.started.Add(-100 * time.Millisecond).UnixMilli())}
}

func (f *fifoCohortFixture) confirmPrefix() {
	f.sequence.observeCarrierWrite(f.items[0], transferWriteDisposition{transportType: TransportTypeH1, reliable: true, route: f.route})
}

// A failed writer retains the copied buffer with its caller. Only a successful
// actual physical offer invokes the production carrier-write observation edge.
func (f *fifoCohortFixture) copy(t *testing.T, index int, fail bool) {
	t.Helper()
	item := f.items[index]
	writer := &fifoRepairCopyWriter{route: f.route, fail: fail}
	wire := MessagePoolCopy(item.transferFrameBytes)
	ok, disposition, err := writer.writeDetailedWithCarrier(context.Background(), wire, 0)
	if err != nil || ok == fail {
		MessagePoolReturn(wire)
		t.Fatalf("unexpected physical copy outcome: failed=%t accepted=%t err=%v", fail, ok, err)
	}
	if !ok {
		MessagePoolReturn(wire)
		return
	}
	f.sequence.observeCarrierWrite(item, disposition)
	actual := <-f.route
	if !bytes.Equal(actual, item.transferFrameBytes) {
		MessagePoolReturn(actual)
		t.Fatal("physical copy changed the exact retained original")
	}
	MessagePoolReturn(actual)
}

// Snapshot only still-live owners. ACKs return earlier pooled sendItems, so no
// caller may inspect those pointers after the ordinary handler has retired them.
type fifoCohortCustody struct {
	id        Id
	number    uint64
	encoded   []byte
	charge    ByteCount
	sendTime  time.Time
	lifetime  time.Time
	sends     int
	deferrals int
	deferAck  time.Time
	kind      sendRecoveryKind
}

func (f *fifoCohortFixture) custody(index int) fifoCohortCustody {
	item := f.items[index]
	return fifoCohortCustody{id: item.messageId, number: item.sequenceNumber,
		encoded: bytes.Clone(item.transferFrameBytes), charge: item.QueueByteCount(),
		sendTime: item.sendTime, lifetime: item.sendTime.Add(item.ackTimeout), sends: item.sendCount,
		deferrals: item.timeoutDeferCount, deferAck: item.timeoutDeferAckTime, kind: item.recoveryKind}
}

func (f *fifoCohortFixture) requireCustody(t *testing.T, index int, before fifoCohortCustody) {
	t.Helper()
	item := f.items[index]
	if f.sequence.resendQueue.GetByMessageId(before.id) != item || item.messageId != before.id ||
		item.sequenceNumber != before.number || !bytes.Equal(item.transferFrameBytes, before.encoded) ||
		item.QueueByteCount() != before.charge || item.sendTime != before.sendTime ||
		item.sendTime.Add(item.ackTimeout) != before.lifetime || item.sendCount != before.sends ||
		item.timeoutDeferCount != before.deferrals || item.timeoutDeferAckTime != before.deferAck ||
		item.recoveryKind != before.kind ||
		f.terminal[index] != 0 || f.success[index] != 0 {
		t.Fatal("FIFO evidence changed exact original, retained charge, callback, lifetime or retry custody")
	}
}

// Old successor-only evidence loses the cohort after the first real cumulative
// advance. Collect the one expected causal error only after all owners close.
func TestFifoRepairCohortAdvancesOnlyExposedOriginal(t *testing.T) {
	GetMessagePoolAggregateStats()
	var missing bool
	synctest.Test(t, func(t *testing.T) {
		f := newFifoCohortFixture(t)
		s := f.sequence
		tag := f.prepare()
		f.confirmPrefix()
		if !s.acknowledgedFifoRepair(f.items[0], tag).set {
			t.Fatal("exact successful promoted-envelope prerequisite did not hold")
		}
		prefixId := f.items[0].messageId
		s.receiveAck(prefixId, false, tag, false)
		if f.success[0] != 1 || s.resendQueue.GetByMessageId(prefixId) != nil {
			t.Fatal("ordinary exact prefix receipt did not retire only its actual owner")
		}
		for index := 1; index < len(f.items); index++ {
			item := f.items[index]
			before := f.custody(index)
			if !s.fifoRepairPassed(item) || item.resendTime.After(time.Now()) {
				missing = true
			}
			if s.shouldDeferTimeoutResend(item, 2*time.Second) && s.fifoRepairPassed(item) {
				t.Fatal("accepted FIFO passage was reclassified as queued behind fresh cumulative progress")
			}
			// Accepted cohort membership is not a blanket burst. Later owners
			// keep their initial clocks until this real ACK exposes their head.
			for later := index + 1; later < len(f.items); later++ {
				next := f.items[later]
				if s.fifoRepairPassed(next) || next.resendTime != f.started.Add(2*time.Second) ||
					f.terminal[later] != 0 || next.sendCount != 1 {
					t.Fatal("one prefix receipt dispatched, retimed or settled an unexposed cohort tail")
				}
			}
			used, count := f.budget.UsedByteCount(), s.resendQueue.Len()
			f.requireCustody(t, index, before)
			if count != len(f.items)-index || used <= 0 || f.budget.Stats().QuotaByteCount != 0 {
				t.Fatal("FIFO cohort lost paid original or introduced a grant")
			}
			id := item.messageId
			// No promoted tag is supplied by these later receipts. They may
			// expose existing proof but must not invent a fresh physical fence.
			s.receiveAck(id, false, sequenceTag{}, false)
			if f.success[index] != 1 || s.resendQueue.Len() != count-1 ||
				f.budget.UsedByteCount() != used-before.charge {
				t.Fatal("ordinary cumulative advance lost exact callback or retained refund")
			}
		}
		f.close(t)
		t.Log("[fifo-cohort-cleanup] originals=5 callbacks_once=true budgets_balanced=true")
	})
	if missing {
		t.Error("FIFO_PREWRITE_COHORT_LOST_AFTER_EXACT_PROMOTED_ACK")
	}
}

// The physical fence belongs to the newest successful write, not the immutable
// first-write pacing timestamp. Failed offers cannot erase accepted authority.
func TestFifoRepairCohortPhysicalCopyFences(t *testing.T) {
	GetMessagePoolAggregateStats()
	for _, mode := range []struct {
		name       string
		beforeAck  bool
		failedCopy bool
	}{
		{name: "successful copy before exact receipt", beforeAck: true},
		{name: "refused copy before exact receipt", beforeAck: true, failedCopy: true},
		{name: "successful copy after exact receipt"},
		{name: "refused copy after exact receipt", failedCopy: true},
	} {
		synctest.Test(t, func(t *testing.T) {
			t.Log(mode.name)
			f := newFifoCohortFixture(t)
			s := f.sequence
			tag := f.prepare()
			f.confirmPrefix()
			target := f.items[3]
			before, firstWrite := f.custody(3), target.pacingSentAtNanos
			if mode.beforeAck {
				f.copy(t, 3, mode.failedCopy)
			}
			s.receiveAck(f.items[0].messageId, false, tag, false)
			if !mode.beforeAck {
				f.copy(t, 3, mode.failedCopy)
			}
			if target.pacingSentAtNanos != firstWrite {
				t.Fatal("copy control changed the original first-write timestamp instead of testing independent fence state")
			}
			f.requireCustody(t, 3, before)
			// One later cumulative receipt exposes target without carrying
			// the exact promoted envelope's tag or manufacturing new proof.
			s.receiveAck(f.items[2].messageId, false, sequenceTag{}, false)
			passed := s.fifoRepairPassed(target)
			if passed != mode.failedCopy || passed && target.resendTime.After(time.Now()) {
				t.Fatalf("failed_copy=%t inherited_fifo_proof=%t deadline=%s now=%s", mode.failedCopy, passed, target.resendTime, time.Now())
			}
			f.close(t)
		})
	}
}

// A cumulative prefix, an old envelope, and another route/generation supply no
// authority for the captured cohort. SACK custody must never be revoked by it.
func TestFifoRepairCohortRequiresExactEnvelopeAndLiveOwners(t *testing.T) {
	GetMessagePoolAggregateStats()
	for _, mode := range []string{"original tag", "unknown tag", "zero tag", "implicit prefix", "failed promoted write",
		"new generation", "changed route", "SACK custody", "delivered owner", "explicit recovery"} {
		synctest.Test(t, func(t *testing.T) {
			t.Log(mode)
			f := newFifoCohortFixture(t)
			s := f.sequence
			tag := f.prepare()
			if mode != "failed promoted write" {
				f.confirmPrefix()
			}
			target := f.items[3]
			switch mode {
			case "original tag":
				tag = f.originalTag()
			case "unknown tag":
				tag.sendTime++
			case "zero tag":
				tag = sequenceTag{}
			case "new generation":
				f.writer.policy.generation++
			case "changed route":
				target.carrierChanged = true
			case "SACK custody":
				s.receiveAck(target.messageId, true, sequenceTag{}, false)
				if !target.selectiveAcked || f.terminal[3] != 0 {
					t.Fatal("real selective control did not preserve receiver custody")
				}
			case "delivered owner":
				target.deliveryObserved = true
			case "explicit recovery":
				target.recoveryKind = sendRecoveryContractMissing
			}
			if mode == "implicit prefix" {
				// ACK names original1, so original0's implicit retirement must
				// not be mistaken for its exact promoted-envelope receipt.
				s.receiveAck(f.items[1].messageId, false, tag, false)
			} else {
				s.receiveAck(f.items[0].messageId, false, tag, false)
			}
			deadline, count, used := target.resendTime, s.resendQueue.Len(), f.budget.UsedByteCount()
			s.receiveAck(f.items[2].messageId, false, sequenceTag{}, false)
			if s.fifoRepairPassed(target) || target.resendTime != deadline || target.sendCount != 1 ||
				f.terminal[3] != 0 || s.resendQueue.Len() >= count || f.budget.UsedByteCount() >= used {
				t.Fatal("absent exact FIFO authority retimed or settled an excluded retained owner")
			}
			if mode == "SACK custody" && !target.selectiveAcked {
				t.Fatal("FIFO proof revoked actual selective custody")
			}
			f.close(t)
		})
	}
}

// The accepted old cohort survives a failed overlapping promotion. Repeated
// writes of one promoted envelope never resurrect a later successfully copied
// member, and that excluded member does not erase the other paid successors.
func TestFifoRepairCohortOverlappingPromotionsKeepIndependentEvidence(t *testing.T) {
	GetMessagePoolAggregateStats()
	for _, successful := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			f := newFifoCohortFixture(t)
			s := f.sequence
			tag := f.prepare()
			f.confirmPrefix()
			s.receiveAck(f.items[0].messageId, false, tag, false)
			head := f.items[1]
			if !s.fifoRepairPassed(head) {
				t.Fatal("first exact receipt did not establish the overlapping control")
			}
			nextTag := sequenceTag{set: true, sendTime: uint64(time.Now().UnixMilli())}
			s.prepareFifoRepair(head, sequenceTag{set: true, sendTime: uint64(time.Now().Add(-time.Millisecond).UnixMilli())}, nextTag, false)
			if !s.fifoRepairPassed(head) {
				t.Fatal("unconfirmed later promotion erased accepted current-head evidence")
			}
			f.copy(t, 3, false)
			if successful {
				disposition := transferWriteDisposition{transportType: TransportTypeH1, reliable: true, route: f.route}
				s.observeCarrierWrite(head, disposition)
				created := s.fifoRepair.pending.createdNanos
				s.observeCarrierWrite(head, disposition)
				if s.fifoRepair.pending.createdNanos != created || !s.fifoRepair.pending.successful {
					t.Fatal("repeated promoted envelope created a newer cohort cutoff")
				}
			}
			s.receiveAck(head.messageId, false, nextTag, false)
			if !s.fifoRepairPassed(f.items[2]) {
				t.Fatal("overlapping successful or failed promotion erased an independently accepted successor")
			}
			s.receiveAck(f.items[2].messageId, false, sequenceTag{}, false)
			if s.fifoRepairPassed(f.items[3]) {
				t.Fatal("repeated prefix copies resurrected a post-fence successful copy")
			}
			s.receiveAck(f.items[3].messageId, false, sequenceTag{}, false)
			if !s.fifoRepairPassed(f.items[4]) || f.items[4].resendTime.After(time.Now()) {
				t.Fatal("excluded middle owner erased a surviving captured tail")
			}
			f.close(t)
		})
	}
}

// An actual nonzero RTT sample and a delayed lower ACK keep healthy residence
// eligible. Only the exact promoted-envelope evidence changes that verdict.
func TestFifoRepairCohortWarmRttAndLateAckKeepHealthyProtection(t *testing.T) {
	GetMessagePoolAggregateStats()
	for _, proved := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			f := newFifoCohortFixture(t)
			s := f.sequence
			warm := s.rttWindow.OpenTag()
			time.Sleep(40 * time.Millisecond)
			s.rttWindow.CloseTag(warm)
			if s.rttWindow.Estimate().Mean != 40*time.Millisecond || s.rttWindow.ScaledRtt() != 300*time.Millisecond {
				t.Fatal("unchanged shipping RTT policy did not learn the actual nonzero sample")
			}
			tag := sequenceTag{}
			if proved {
				tag = f.prepare()
				f.confirmPrefix()
			}
			time.Sleep(time.Millisecond)
			s.receiveAck(f.items[0].messageId, false, tag, false)
			item := f.items[1]
			if !s.lastCumulativeAckTime.After(item.sendTime) || s.rttWindow.Estimate().Mean <= 0 {
				t.Fatal("late real cumulative control or nonzero RTT sample was lost")
			}
			if s.shouldDeferTimeoutResend(item, s.rttWindow.ScaledRtt()) == proved {
				t.Fatalf("proved=%t failed to separate exact FIFO absence from a healthy lower-ACK drain", proved)
			}
			f.close(t)
		})
	}
}

// A sequence's snapshotted scan bound keeps fixed work even if the caller later
// mutates the shared settings pointer. Shrinking or expanding that field must
// neither strand evidence outside the clear span nor resurrect a new cohort.
func TestFifoRepairCohortScanLimitIsCapturedAndNonpositiveKeepsSuccessor(t *testing.T) {
	GetMessagePoolAggregateStats()
	for _, mode := range []struct {
		initial int
		nilTail bool
	}{{initial: 0}, {initial: -1}, {initial: 3}, {initial: 3, nilTail: true}} {
		synctest.Test(t, func(t *testing.T) {
			f := newFifoCohortFixture(t)
			s := f.sequence
			setFifoCohortLimit(t, s, mode.initial)
			if mode.nilTail {
				// An inert interior hole is only a defensive scan adjacency;
				// restore the actual paid owner before ACK traversal/cleanup.
				s.sendItems[2] = nil
				defer func() {
					for index, item := range s.sendItems {
						if item == nil {
							s.sendItems[index] = f.items[2]
						}
					}
				}()
			}
			tag := f.prepare()
			limit := max(2, mode.initial)
			for index := 1; index < len(f.items); index++ {
				if fifoCohortFlag(t, f.items[index], "fifoRepairPending") != (index < limit && !(mode.nilTail && index == 2)) {
					t.Fatal("configured scan prefix failed its exact capture boundary")
				}
			}
			for _, changed := range []int{1, 256} {
				setFifoCohortLimit(t, s, changed)
				tag = f.prepare()
				for index := 1; index < len(f.items); index++ {
					if fifoCohortFlag(t, f.items[index], "fifoRepairPending") != (index < limit && !(mode.nilTail && index == 2)) {
						t.Fatal("live settings mutation changed the captured clear/capture span")
					}
				}
			}
			if mode.nilTail {
				s.sendItems[2] = f.items[2]
			}
			f.confirmPrefix()
			s.receiveAck(f.items[0].messageId, false, tag, false)
			if !s.fifoRepairPassed(f.items[1]) {
				t.Fatal("nonpositive or bounded setting disabled the existing successor proof")
			}
			// The active prefix only moves toward the front after retirement.
			// A route-generation reset must clear every surviving marked owner.
			f.writer.policy.generation++
			s.fifoRepairPolicy()
			for index := 1; index < len(f.items); index++ {
				if fifoCohortFlag(t, f.items[index], "fifoRepairPending") ||
					fifoCohortFlag(t, f.items[index], "fifoRepairAccepted") || s.fifoRepairPassed(f.items[index]) {
					t.Fatal("generation reset left captured or accepted membership outside its stable scan prefix")
				}
			}
			f.close(t)
		})
	}
}

// Accepted flags belong to one actual paid original. The real ACK/return path
// must clear them before the pooled allocation is reused by another identity.
func TestFifoRepairCohortPoolReturnClearsAcceptedMembership(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		clearSendItemPool()
		f := newFifoCohortFixture(t)
		s := f.sequence
		tag := f.prepare()
		f.confirmPrefix()
		prefix := f.items[0]
		s.receiveAck(prefix.messageId, false, tag, false)
		firstReturned := takeSendItem()
		if firstReturned != prefix {
			firstReturned.messagePoolReturn()
			t.Fatal("first actual ACK did not exercise the expected real pool return")
		}
		defer firstReturned.messagePoolReturn()
		target := f.items[1]
		if !fifoCohortFlag(t, target, "fifoRepairAccepted") {
			t.Fatal("pool control has no actual accepted cohort owner")
		}
		oldId, oldNumber := target.messageId, target.sequenceNumber
		s.receiveAck(oldId, false, sequenceTag{}, false)
		reused := takeSendItem()
		if reused != target {
			reused.messagePoolReturn()
			t.Fatal("accepted owner did not exercise real allocation reuse")
		}
		defer reused.messagePoolReturn()
		if fifoCohortFlag(t, reused, "fifoRepairPending") || fifoCohortFlag(t, reused, "fifoRepairAccepted") ||
			s.resendQueue.GetByMessageId(oldId) != nil {
			t.Fatal("acknowledged allocation retained old cohort identity or membership")
		}
		*reused = sendItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: oldNumber}}
		if reused.messageId == oldId || s.fifoRepairPassed(reused) {
			t.Fatal("a reused allocation and ordinal inherited the old exact-message proof")
		}
		f.close(t)
	})
}

// Historical raw residence is a reason to protect an unproved initial write.
// It cannot supersede the exact newer envelope's evidence of FIFO passage.
func TestFifoRepairCohortProofPreemptsRawResidenceOnlyWhenProved(t *testing.T) {
	GetMessagePoolAggregateStats()
	for _, proved := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			f := newFifoCohortFixture(t)
			s := f.sequence
			tag := sequenceTag{}
			if proved {
				tag = f.prepare()
				f.confirmPrefix()
			}
			s.receiveAck(f.items[0].messageId, false, tag, false)
			item := f.items[1]
			if !proved {
				time.Sleep(time.Until(item.resendTime))
			}
			s.windowPacer.service = &windowPacingService{roundTripProbe: windowPacingRoundTripProbe{
				sequenceId: s.sequenceId, messageId: item.messageId, sentAt: time.Now(),
				observedRoundTrip: 4 * time.Second, resetService: true, written: true,
			}}
			probeDue := s.windowPacer.service.probeRecoveryDeadline(s.sequenceId, item.messageId,
				s.sendBufferSettings.RttScale, s.sendBufferSettings.MaxResendInterval)
			if !probeDue.After(time.Now()) {
				t.Fatal("raw-residence adverse control has no actual later service deadline")
			}
			var outcome sendRecoverySelectionOutcome
			if !proved {
				_, outcome, _ = f.selectAt(t, item)
			} else {
				// A proved, non-Head original is legitimately promoted here.
				// Verify that exact paid rewrite instead of using the generic
				// preexisting-Head helper's byte-identical wire premise.
				var before, after protocol.TransferFrame
				if err := ProtoUnmarshal(item.transferFrameBytes, &before); err != nil || before.Pack == nil || before.Pack.Head {
					t.Fatal("proved control did not start with a real non-Head original", err)
				}
				id, number, sent, count := item.messageId, item.sequenceNumber, item.sendTime, item.sendCount
				callbacks := append([]int(nil), f.terminal...)
				var err error
				_, outcome, err = s.selectDueRecovery(item, time.Now(), s.transferFlightPolicy(), s.ackWindow)
				if err != nil {
					t.Fatal("proved recovery selection failed", err)
				}
				if err := ProtoUnmarshal(item.transferFrameBytes, &after); err != nil || after.Pack == nil || !after.Pack.Head || after.Pack.Tag == nil {
					t.Fatal("actual promoted Head/tag was not retained", err)
				}
				before.Pack.Head, before.Pack.Tag = true, after.Pack.Tag
				if !proto.Equal(&before, &after) || item.messageId != id || item.sequenceNumber != number || item.sendTime != sent || item.sendCount != count ||
					!item.head || !item.promotedHead || item.deliveryObserved || item.serviceCreditObserved || s.resendQueue.GetByMessageId(id) != item ||
					s.detachedAck.active || s.pendingRecovery != nil || s.client.SendRecoveryStats().TimeoutResendWriteCount != 0 || !reflect.DeepEqual(callbacks, f.terminal) {
					t.Fatal("promotion changed payload, exact ACK custody, callbacks, or dispatch state")
				}
				var encoded, charged ByteCount
				for _, retained := range s.sendItems {
					encoded += retained.MessageByteCount()
					charged += retained.QueueByteCount()
				}
				if s.resendQueue.byteCount != encoded || s.resendQueue.queueByteCount != charged || f.budget.UsedByteCount() != charged ||
					item.QueueByteCount() < item.retainedMemoryByteCount(ByteCount(len(item.transferFrameBytes)), len(after.Pack.Frames), s.sendBufferSettings.ProtocolVersion < 2) {
					t.Fatal("actual promoted frame escaped its exact retained charge")
				}
			}
			if proved && outcome != sendRecoverySelectionReady || !proved && outcome != sendRecoverySelectionDeferred {
				t.Fatalf("proved=%t selection=%d raw_deadline=%s actual_deadline=%s", proved, outcome, probeDue, item.resendTime)
			}
			if !proved && item.resendTime.Before(probeDue) {
				t.Fatal("unproved original lost its healthy raw-residence protection")
			}
			// Cleanup needs no service completion: selection did not spend or
			// fabricate a pacing reservation or a physical write.
			s.windowPacer.service = nil
			f.close(t)
		})
	}
}

// A healthy but silent carrier supplies no exact promoted-copy receipt. The
// cohort change must leave this adverse freshness case at its baseline verdict,
// rather than manufacture proof from a lower ACK or an attempted promotion.
func TestFifoRepairCohortAckSilenceDoesNotManufacturePassage(t *testing.T) {
	GetMessagePoolAggregateStats()
	for _, attempted := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			f := newFifoCohortFixture(t)
			s := f.sequence
			warm := s.rttWindow.OpenTag()
			time.Sleep(40 * time.Millisecond)
			s.rttWindow.CloseTag(warm)
			interval := s.rttWindow.ScaledRtt()
			if interval != 300*time.Millisecond {
				t.Fatal("silence control changed the actual warm shipping timer floor")
			}
			// Model initial writes made on this already warm path. Both arms
			// have identical paid wire owners and initial timer construction.
			for _, item := range f.items {
				item.sendTime = time.Now()
				s.setResendTime(item, item.sendTime.Add(interval))
				s.ackLifetimes.update(item)
			}
			if attempted {
				f.prepare() // No successful promoted write and no matching ACK.
			}
			time.Sleep(time.Millisecond)
			s.receiveAck(f.items[0].messageId, false, sequenceTag{}, false)
			time.Sleep(interval + interval/2)
			item := f.items[1]
			if s.fifoRepairPassed(item) || item.sendCount != 1 || item.timeoutDeferCount != 0 {
				t.Fatal("ACK silence or an unconfirmed promotion manufactured receiver-loss authority")
			}
			_, outcome, _ := f.selectAt(t, item)
			// A lower ACK with no exact loss proof still protects the initial
			// residence once, even after 1.5 warm RTTs of carrier silence.
			// The rejected generic decision-age guard instead permitted a
			// whole-window retry here; the full public 97/128 control remains
			// unchanged and is the independently observed adverse regression.
			if outcome != sendRecoverySelectionDeferred || !item.deferralOutstanding || item.timeoutDeferCount != 1 ||
				item.resendTime != time.Now().Add(interval) || s.client.timeoutResendDeferCount.Load() != 1 {
				t.Fatal("healthy initial residence lost its protective first deferral without exact FIFO loss proof")
			}
			f.close(t)
		})
	}
}
