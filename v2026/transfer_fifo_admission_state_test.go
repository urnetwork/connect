// Fixed FIFO recovery holds retain exact owner boundaries without changing
// delivery authority, retry clocks or physical custody.
package connect

import (
	"bytes"
	"context"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Reflection keeps this complete test package compilable with the old product.
func fifoAdmissionStateForTest(t *testing.T, sequence *SendSequence) (uint64, bool) {
	t.Helper()
	state := reflect.ValueOf(sequence).Elem().FieldByName("fifoRepair").FieldByName("admission")
	if !state.IsValid() {
		t.Fatal("candidate has no fixed FIFO admission boundary")
	}
	through, set := state.FieldByName("through"), state.FieldByName("set")
	if !through.IsValid() || through.Kind() != reflect.Uint64 || !set.IsValid() || set.Kind() != reflect.Bool {
		t.Fatal("candidate FIFO admission boundary has an unexpected shape")
	}
	return through.Uint(), set.Bool()
}

// Inert owner construction selects ordinary expiry callbacks before publication.
// The final protocol owner instead keeps its explicit retained promise.
func newFifoAdmissionOrdinaryFixture(t *testing.T, protocolLast bool) (*fifoCohortFixture, []error) {
	t.Helper()
	settings := DefaultSendBufferSettings()
	settings.WindowSizing = WindowSizingFromDelivery
	settings.ApplyWindowSizing()
	settings.ResendQueueRetainedByteAccounting = true
	budget := NewTransferMemoryBudget(1024 * 1024)
	settings.ResendQueueBudget = budget
	queue := newResendQueue(budget, 0)
	queue.setLifetimeBudget()
	started := time.Now()
	base := &timeoutProgressFreshnessFixture{budget: budget, started: started,
		terminal: make([]int, 5), success: make([]int, 5)}
	route := make(Route, 1)
	writer := &windowPacingPolicyWriter{policy: transferFlightPolicySnapshot{generation: 7, h1Only: true}}
	sequence := &SendSequence{ctx: context.Background(), client: &Client{feedbackTimeBase: started.Add(-2 * time.Second)},
		log: NewNoopLogger(), sequenceId: NewId(), sendBufferSettings: settings,
		resendQueue: queue, ackWindow: newSequenceAckWindow(), contractMultiRouteWriter: writer,
		rttWindow: NewRttWindow(NewNoopLogger(), settings.RttWindowSize, settings.RttWindowTimeout,
			settings.RttScale, settings.MinResendInterval, settings.RttMinResendInterval, settings.MaxResendInterval)}
	sequence.flightController = newSendFlightController(settings)
	sequence.fifoRepair = fifoRepairState{generation: 7, generationStartNanos: started.Add(-2 * time.Second).UnixNano()}
	sequence.lastCumulativeAckTime = started
	base.sequence = sequence
	fixture := &fifoCohortFixture{timeoutProgressFreshnessFixture: base, writer: writer, route: route}
	results := make([]error, 5)
	t.Cleanup(func() { fixture.close(t) })
	for index := range 5 {
		protocolOwner := protocolLast && index == 4
		item := takeSendItem()
		*item = sendItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: uint64(index)},
			expectsAck: true, sendTime: started, resendTime: started.Add(2 * time.Second), sendCount: 1,
			ackTimeout: settings.AckTimeout, noBudget: protocolOwner, reliableCarrierObserved: true,
			carrierRoute: route, reliableRoute: route, rttH1: true, rttState: sendItemRttWriteConfirmed,
			pacingSentAtNanos: started.Add(-time.Second).UnixNano()}
		originalTag := fixture.originalTag()
		frame := &protocol.TransferFrame{Pack: &protocol.Pack{MessageId: item.messageId.Bytes(),
			SequenceId: sequence.sequenceId.Bytes(), SequenceNumber: item.sequenceNumber,
			Tag: originalTag.protocol(), Frames: []*protocol.Frame{{
				MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: bytes.Repeat([]byte{0x57}, 32)}}}}
		wire, err := ProtoMarshal(frame)
		if err != nil {
			item.messagePoolReturn()
			t.Fatal(err)
		}
		item.transferFrameBytes = wire
		if !protocolOwner && !item.reserveMemory(budget,
			item.retainedMemoryByteCount(ByteCount(len(wire)), 1, settings.ProtocolVersion < 2)) {
			item.messagePoolReturn()
			t.Fatal("ordinary expiry owner did not fit its paid fixture root")
		}
		item.acks.add(sendAckRecord{noBudget: protocolOwner, retainAfterAckTimeout: protocolOwner, callback: func(err error) {
			base.terminal[index]++
			results[index] = err
			if err == nil {
				base.success[index]++
			} else if err != context.Canceled && err != context.DeadlineExceeded {
				t.Errorf("ordinary fixture owner returned an unexpected disposition: %v", err)
			}
		}})
		base.items = append(base.items, item)
		sequence.sendItems = append(sequence.sendItems, item)
		sequence.addResendItem(item)
		sequence.ackLifetimes.update(item)
	}
	sequence.nextSequenceNumber = 5
	return fixture, results
}

// Only an exact successful promoted-copy receipt can begin a DATA hold.
func TestFifoRepairAdmissionRequiresExactPromotedReceipt(t *testing.T) {
	GetMessagePoolAggregateStats()
	for _, mode := range []string{"exact", "failed-write", "original-tag", "unknown-tag", "implicit-prefix", "new-generation", "mixed-policy"} {
		synctest.Test(t, func(t *testing.T) {
			assertMessagePoolOwnership(t)
			t.Log(mode)
			fixture := newFifoCohortFixture(t)
			sequence := fixture.sequence
			through := fixture.items[4].sequenceNumber
			tag := fixture.prepare()
			if mode != "failed-write" {
				fixture.confirmPrefix()
			}
			prefixId := fixture.items[0].messageId
			switch mode {
			case "original-tag":
				tag = fixture.originalTag()
			case "unknown-tag":
				tag.sendTime++
			case "implicit-prefix":
				prefixId = fixture.items[1].messageId
			case "new-generation":
				fixture.writer.policy.generation++
			case "mixed-policy":
				fixture.writer.policy.h1Only = false
			}
			sequence.receiveAck(prefixId, false, tag, false)
			gotThrough, held := fifoAdmissionStateForTest(t, sequence)
			if held != (mode == "exact") || held && gotThrough != through {
				t.Fatalf("exact receipt mode=%s held=%t through=%d, want boundary=%d", mode, held, gotThrough, through)
			}
			fixture.close(t)
		})
	}
}

// A later independently valid proof may extend FIFO evidence, never this hold.
func TestFifoRepairAdmissionKeepsItsOriginalBoundary(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		fixture := newFifoCohortFixture(t)
		sequence := fixture.sequence
		setFifoCohortLimit(t, sequence, 3)
		boundary, later := fixture.items[2].sequenceNumber, fixture.items[3].sequenceNumber
		tag := fixture.prepare()
		fixture.confirmPrefix()
		sequence.receiveAck(fixture.items[0].messageId, false, tag, false)
		if through, held := fifoAdmissionStateForTest(t, sequence); !held || through != boundary {
			t.Fatal("initial bounded proof did not capture its exact first DATA cohort")
		}
		time.Sleep(time.Millisecond)
		next := fixture.items[1]
		nextId := next.messageId
		laterTag := sequenceTag{set: true, sendTime: uint64(time.Now().UnixMilli())}
		sequence.prepareFifoRepair(next, fixture.originalTag(), laterTag, false)
		sequence.observeCarrierWrite(next, transferWriteDisposition{transportType: TransportTypeH1, reliable: true, route: fixture.route})
		sequence.receiveAck(nextId, false, laterTag, false)
		if !fixture.items[3].fifoRepairAccepted || fixture.items[3].sequenceNumber != later {
			t.Fatal("second exact proof did not establish the independently later cohort premise")
		}
		if through, held := fifoAdmissionStateForTest(t, sequence); !held || through != boundary {
			t.Fatal("a later proof extended the fixed admission hold")
		}
		sequence.receiveAck(fixture.items[2].messageId, false, sequenceTag{}, false)
		if _, held := fifoAdmissionStateForTest(t, sequence); held || len(sequence.sendItems) != 2 {
			t.Fatal("actual original boundary retirement waited for newer unrelated owners")
		}
		fixture.close(t)
	})
}

// Retransmission clears mutable FIFO markers, and SACK changes retry timing;
// neither transition retires the original DATA ownership held by admission.
func TestFifoRepairAdmissionSurvivesRetryAndSelectiveCustody(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		fixture := newFifoCohortFixture(t)
		sequence := fixture.sequence
		boundary := fixture.items[4].sequenceNumber
		tag := fixture.prepare()
		fixture.confirmPrefix()
		sequence.receiveAck(fixture.items[0].messageId, false, tag, false)
		fixture.copy(t, 1, false)
		if fixture.items[1].fifoRepairAccepted {
			t.Fatal("physical retry did not clear its existing mutable cohort marker")
		}
		sequence.receiveAck(fixture.items[4].messageId, true, sequenceTag{}, false)
		if through, held := fifoAdmissionStateForTest(t, sequence); !held || through != boundary || fixture.terminal[4] != 0 {
			t.Fatal("retry or selective custody released an undelivered DATA hold")
		}
		sequence.receiveAck(fixture.items[3].messageId, false, sequenceTag{}, false)
		if _, held := fifoAdmissionStateForTest(t, sequence); !held || len(sequence.sendItems) != 1 || !sequence.sendItems[0].selectiveAcked {
			t.Fatal("a selectively held final original was mistaken for retirement")
		}
		sequence.receiveAck(fixture.items[4].messageId, false, sequenceTag{}, false)
		if _, held := fifoAdmissionStateForTest(t, sequence); held || fixture.budget.UsedByteCount() != 0 {
			t.Fatal("actual cumulative delivery did not release the paid cohort hold")
		}
		fixture.close(t)
	})
}

// A protocol successor keeps its independent ACK custody and cannot lengthen
// a DATA admission boundary or keep it closed after all captured DATA retires.
func TestFifoRepairAdmissionExcludesProtocolOwnership(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		fixture, _ := newFifoAdmissionOrdinaryFixture(t, true)
		sequence := fixture.sequence
		boundary := fixture.items[3].sequenceNumber
		tag := fixture.prepare()
		fixture.confirmPrefix()
		sequence.receiveAck(fixture.items[0].messageId, false, tag, false)
		if through, held := fifoAdmissionStateForTest(t, sequence); !held || through != boundary {
			t.Fatal("NoBudget protocol ownership extended the DATA hold")
		}
		sequence.receiveAck(fixture.items[3].messageId, false, sequenceTag{}, false)
		if _, held := fifoAdmissionStateForTest(t, sequence); held || len(sequence.sendItems) != 1 || !sequence.sendItems[0].noBudget || fixture.terminal[4] != 0 {
			t.Fatal("remaining protocol custody kept ordinary DATA admission closed")
		}
		sequence.receiveAck(fixture.items[4].messageId, false, sequenceTag{}, false)
		fixture.close(t)
	})
}

// Interior and tail expiry cannot release a surviving older DATA owner. The
// final ordinary expiry may reopen admission while retained protocol stays live.
func TestFifoRepairAdmissionExpiresOnlyItsLastDataOwner(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		fixture, results := newFifoAdmissionOrdinaryFixture(t, true)
		sequence := fixture.sequence
		tag := fixture.prepare()
		fixture.confirmPrefix()
		sequence.receiveAck(fixture.items[0].messageId, false, tag, false)
		time.Sleep(sequence.sendBufferSettings.AckTimeout)
		for _, index := range []int{2, 3, 1} {
			item := fixture.items[index]
			if item.acks.retainPastAckTimeout() || time.Now().Before(item.sendTime.Add(item.ackTimeout)) {
				t.Fatal("ordinary owner did not reach its unchanged actual expiry")
			}
			sequence.expireSendItem(item, time.Now())
			_, held := fifoAdmissionStateForTest(t, sequence)
			if held != (index != 1) || results[index] != context.DeadlineExceeded || fixture.success[index] != 0 || fixture.terminal[index] != 1 {
				t.Fatal("expiry changed delivery authority or released another live DATA owner")
			}
		}
		if len(sequence.sendItems) != 1 || !sequence.sendItems[0].noBudget || fixture.terminal[4] != 0 || fixture.budget.UsedByteCount() != 0 {
			t.Fatal("ordinary expiry lost the independently retained protocol owner")
		}
		sequence.receiveAck(fixture.items[4].messageId, false, sequenceTag{}, false)
		fixture.close(t)
	})
}

// A replacement carrier epoch cannot inherit the prior FIFO's admission hold.
func TestFifoRepairAdmissionGenerationResetPreservesPaidOwners(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		fixture := newFifoCohortFixture(t)
		sequence := fixture.sequence
		tag := fixture.prepare()
		fixture.confirmPrefix()
		sequence.receiveAck(fixture.items[0].messageId, false, tag, false)
		before := fixture.custody(1)
		fixture.writer.policy.generation++
		sequence.fifoRepairPolicy()
		if _, held := fifoAdmissionStateForTest(t, sequence); held {
			t.Fatal("new route generation inherited the retired FIFO admission hold")
		}
		fixture.requireCustody(t, 1, before)
		fixture.close(t)
	})
}

// The existing canceled-owner close path settles every callback exactly once
// and clears admission independently of successful terminal delivery.
func TestFifoRepairAdmissionCanceledCloseReturnsEveryOwner(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		fixture := newFifoCohortFixture(t)
		sequence := fixture.sequence
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sequence.ctx = ctx
		tag := fixture.prepare()
		fixture.confirmPrefix()
		sequence.receiveAck(fixture.items[0].messageId, false, tag, false)
		cancel()
		fixture.close(t)
		if _, held := fifoAdmissionStateForTest(t, sequence); held || len(sequence.sendItems) != 0 || fixture.budget.UsedByteCount() != 0 {
			t.Fatal("canceled close retained an admission boundary or paid DATA owner")
		}
		for index := 1; index < len(fixture.terminal); index++ {
			if fixture.terminal[index] != 1 || fixture.success[index] != 0 {
				t.Fatal("cancellation manufactured delivery or redispatched an owner")
			}
		}
	})
}
