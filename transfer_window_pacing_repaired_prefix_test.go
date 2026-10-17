package connect

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

type fifoRepairPrefixCase struct {
	originalOnly   bool
	ackTag         string
	newGeneration  bool
	failedWrite    bool
	futureDeferral bool
	laterCopy      bool
	selectiveCopy  bool
	earlierCopy    bool
}

type fifoRepairPrefixDecision struct {
	at, retry, lifetime, deferAck              time.Time
	deferrals, sends                           int
	writes                                     uint64
	message                                    Id
	number, highest, generation                uint64
	retained, outstanding, leased, changed, h1 bool
}

// This assertion concerns the worker's queue-residence decision, not a new
// recovery deadline. The fixture lets the existing timer choose when to run.
func TestWindowPacingTaggedRepairCannotRenewQueueResidence(t *testing.T) {
	runFifoRepairPrefixCausality(t, fifoRepairPrefixCase{})
}

func TestWindowPacingTaggedRepairRevokesExistingQueueResidence(t *testing.T) {
	runFifoRepairPrefixCausality(t, fifoRepairPrefixCase{futureDeferral: true})
}

func TestWindowPacingOriginalAckKeepsQueueResidence(t *testing.T) {
	runFifoRepairPrefixCausality(t, fifoRepairPrefixCase{originalOnly: true})
}

func TestWindowPacingOriginalTagAfterRepairKeepsQueueResidence(t *testing.T) {
	runFifoRepairPrefixCausality(t, fifoRepairPrefixCase{ackTag: "original"})
}

func TestWindowPacingUnknownTagKeepsQueueResidence(t *testing.T) {
	runFifoRepairPrefixCausality(t, fifoRepairPrefixCase{ackTag: "unknown"})
}

func TestWindowPacingZeroTagKeepsQueueResidence(t *testing.T) {
	runFifoRepairPrefixCausality(t, fifoRepairPrefixCase{ackTag: "zero"})
}

func TestWindowPacingAbsentTagKeepsQueueResidence(t *testing.T) {
	runFifoRepairPrefixCausality(t, fifoRepairPrefixCase{ackTag: "absent"})
}

func TestWindowPacingSameRouteNewGenerationKeepsQueueResidence(t *testing.T) {
	runFifoRepairPrefixCausality(t, fifoRepairPrefixCase{newGeneration: true})
}

func TestWindowPacingFailedPromotedAttemptKeepsQueueResidence(t *testing.T) {
	runFifoRepairPrefixCausality(t, fifoRepairPrefixCase{failedWrite: true})
}

func TestWindowPacingSuccessfulLaterCopyKeepsQueueResidence(t *testing.T) {
	runFifoRepairPrefixCausality(t, fifoRepairPrefixCase{laterCopy: true})
}

func TestWindowPacingSuccessfulSelectiveEvictionCopyKeepsQueueResidence(t *testing.T) {
	runFifoRepairPrefixCausality(t, fifoRepairPrefixCase{laterCopy: true, selectiveCopy: true})
}

func TestWindowPacingPriorSuccessfulCopyCannotRenewQueueResidence(t *testing.T) {
	runFifoRepairPrefixCausality(t, fifoRepairPrefixCase{earlierCopy: true})
}

func TestWindowPacingPriorSuccessfulCopyRevokesExistingQueueResidence(t *testing.T) {
	runFifoRepairPrefixCausality(t, fifoRepairPrefixCase{earlierCopy: true, futureDeferral: true})
}

func TestWindowPacingPriorCopyNewGenerationKeepsQueueResidence(t *testing.T) {
	runFifoRepairPrefixCausality(t, fifoRepairPrefixCase{earlierCopy: true, newGeneration: true})
}

func runFifoRepairPrefixCausality(t *testing.T, c fifoRepairPrefixCase) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		destination := NewId()
		route := make(Route, 8)
		transport := NewSendGatewayTransportWithType(TransportTypeH1)
		written, initialResume := make(chan uint64, 4), make(chan struct{})
		dueReached, dueResume := make(chan struct{}), make(chan struct{})
		failedReached, failedResume := make(chan struct{}), make(chan struct{})
		ackReached, ackResume := make(chan struct{}), make(chan struct{})
		decisionReached := make(chan struct{})
		copyReached, copyResume := make(chan struct{}), make(chan struct{})
		priorReached, priorResume := make(chan struct{}), make(chan struct{})
		var dueOnce, failedOnce, copyOnce sync.Once
		var priorOnce sync.Once
		var priorCopyCompletedNanos, prefixSelectionNanos int64
		var futureOnce sync.Once
		var phase atomic.Int32
		var client *Client
		var before, after fifoRepairPrefixDecision
		var premise string
		snapshot := func(sequence *SendSequence, item *sendItem) fifoRepairPrefixDecision {
			highest, _ := sequence.laneHighestAcked(item.carrierRoute)
			return fifoRepairPrefixDecision{
				at: sequence.lastCumulativeAckTime, retry: item.resendTime,
				lifetime: item.sendTime.Add(item.ackTimeout), deferAck: item.timeoutDeferAckTime,
				deferrals: item.timeoutDeferCount, sends: item.sendCount,
				writes: sequence.resendWriteCount.Load(), message: item.messageId,
				number: item.sequenceNumber, highest: highest, generation: sequence.transferFlightPolicy().generation,
				retained:    sequence.resendQueue.GetByMessageId(item.messageId) == item,
				outstanding: item.deferralOutstanding, leased: item.selectiveAcked,
				changed: item.carrierChanged, h1: item.rttH1 && item.reliableCarrierObserved && !item.unreliableCarrierObserved,
			}
		}
		settings := DefaultClientSettings()
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		settings.SendBufferSettings.WindowSizing = WindowSizingFromDelivery
		settings.SendBufferSettings.ApplyWindowSizing()
		if c.failedWrite {
			// A full owned route refuses the actual retry immediately. This is
			// deterministic failure injection, not a shortened recovery clock.
			settings.SendBufferSettings.WriteTimeout = 0
		}
		if settings.SendBufferSettings.ReliableLaneProvenRecovery || settings.SendBufferSettings.DeferredItemIsLateForTheScoreboard {
			t.Fatal("fixture changed the production recovery policy")
		}
		settings.SendBufferSettings.afterInitialWriteQueuedForTest = func(id sendSequenceId, number uint64) {
			if id.Destination != destination {
				return
			}
			written <- number
			if number == 3 {
				select {
				case <-initialResume:
				case <-ctx.Done():
				}
			}
		}
		settings.SendBufferSettings.beforeDueResendForTest = func(id sendSequenceId, number uint64) {
			if id.Destination != destination {
				return
			}
			sequence := client.sendBuffer.lookupSendSequence(id, nil)
			var item *sendItem
			for _, candidate := range sequence.sendItems {
				if candidate.sequenceNumber == number {
					item = candidate
					break
				}
			}
			if item == nil {
				return
			}
			if c.earlierCopy && number == 3 && item.sendCount == 1 {
				priorOnce.Do(func() {
					close(priorReached)
					select {
					case <-priorResume:
					case <-ctx.Done():
					}
				})
			}
			if item.timeoutDeferCount == 0 {
				return
			}
			if c.earlierCopy && number == 1 {
				prefixSelectionNanos = client.feedbackArrivalNanos(time.Now())
			}
			if c.futureDeferral {
				return
			}
			if c.selectiveCopy && number == 2 {
				copyOnce.Do(func() {
					close(copyReached)
					select {
					case <-copyResume:
					case <-ctx.Done():
					}
				})
			}
			if c.failedWrite && number == 1 {
				failedOnce.Do(func() {
					close(failedReached)
					select {
					case <-failedResume:
					case <-ctx.Done():
					}
				})
			}
			want := uint64(2)
			if c.laterCopy {
				// Let this worker physically rewrite #2 after #1, then
				// stop at the next older timer before applying the ACK.
				want = 3
			}
			if c.originalOnly {
				want = 1
			}
			if number == want {
				dueOnce.Do(func() {
					close(dueReached)
					select {
					case <-dueResume:
					case <-ctx.Done():
					}
				})
			}
		}
		settings.SendBufferSettings.afterApplyAckSnapshotForTest = func(id sendSequenceId) {
			if id.Destination != destination {
				return
			}
			sequence := client.sendBuffer.lookupSendSequence(id, nil)
			if c.futureDeferral && phase.Load() == 0 && len(sequence.sendItems) > 0 &&
				sequence.sendItems[0].sequenceNumber == 1 && sequence.sendItems[0].promotedHead && sequence.resendWriteCount.Load() > 0 {
				futureOnce.Do(func() {
					close(dueReached)
					select {
					case <-dueResume:
					case <-ctx.Done():
					}
				})
				return
			}
			if phase.Load() == 0 {
				return
			}
			if len(sequence.sendItems) == 0 || sequence.sendItems[0].sequenceNumber != 2 {
				return
			}
			head := sequence.sendItems[0]
			if phase.CompareAndSwap(1, 2) {
				before = snapshot(sequence, head)
				close(ackReached)
				select {
				case <-ackResume:
				case <-ctx.Done():
				}
			}
			// The actual due-selection/dispatch path has made its next
			// decision once one of these ownership counters changes. No
			// extra timer, predicate call or pacing operation drives it.
			if phase.Load() == 3 && (head.timeoutDeferCount != before.deferrals || head.sendCount != before.sends) {
				after = snapshot(sequence, head)
				if after.message != before.message || !after.retained {
					premise = "the successor changed owner during its decision"
				}
				phase.Store(4)
				close(decisionReached)
				<-ctx.Done()
			}
		}
		client = NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		defer func() {
			cancel()
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
			for len(route) > 0 {
				MessagePoolReturn(<-route)
			}
		}()
		client.ContractManager().AddNoContractPeer(destination)
		client.RouteManager().UpdateTransport(transport, []Route{route})
		readPack := func() *protocol.Pack {
			select {
			case wire := <-route:
				defer MessagePoolReturn(wire)
				return decodeSendPackLifecycleWirePack(t, wire)
			default:
				t.Fatal("expected physical FIFO write missing")
				return nil
			}
		}
		packs := make([]*protocol.Pack, 0, 4)
		for number := range 4 {
			frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(1280)}
			clear(frame.MessageBytes)
			if !client.SendWithTimeout(frame, destination, func(error) {}, time.Second) {
				MessagePoolReturn(frame.MessageBytes)
				t.Fatal("original Pack not admitted")
			}
			if <-written != uint64(number) {
				t.Fatal("original FIFO order changed")
			}
			packs = append(packs, readPack())
			// Separate synthetic clocks pin heap order to physical order.
			time.Sleep(time.Millisecond)
		}
		sequence := client.sendBuffer.lookupSendSequence(sendSequenceId{Destination: destination}, nil)
		headId, _ := IdFromBytes(packs[2].MessageId)
		head := sequence.resendQueue.GetByMessageId(headId)
		originalDue, originalLifetime := head.resendTime, head.sendTime.Add(head.ackTimeout)
		originalPhysical := head.pacingSentAtNanos
		generation := sequence.transferFlightPolicy().generation
		time.Sleep(100 * time.Millisecond)
		if c.earlierCopy {
			// Consume three actual copies before allowing the same worker to
			// promote #1. No retry deadline or recovery decision is injected.
			close(initialResume)
			<-priorReached
			for number := range 3 {
				copy := readPack()
				if copy.SequenceNumber != uint64(number) || string(copy.MessageId) != string(packs[number].MessageId) ||
					copy.Tag == nil || copy.Tag.SendTime != packs[number].Tag.SendTime {
					t.Fatal("prior physical copies changed the original FIFO identities or tags")
				}
			}
			priorCopyCompletedNanos = client.feedbackArrivalNanos(time.Now())
			if sequence.resendWriteCount.Load() != 3 || head.sendCount != 2 || len(route) != 0 ||
				head.rttState != sendItemRttUnavailable || !head.rttH1 || !head.reliableCarrierObserved ||
				head.carrierChanged || head.unreliableCarrierObserved || head.hybridReliableCarrierObserved ||
				head.pacingSentAtNanos != originalPhysical || sequence.fifoRepair.generationStartNanos > originalPhysical ||
				originalPhysical <= 0 || priorCopyCompletedNanos <= originalPhysical {
				t.Fatal("prior copy lacks successful initial H1, current epoch, or completed physical ordering")
			}
			acknowledgeSendPackLifecycleWirePack(t, client, destination, packs[0])
			close(priorResume)
		} else {
			acknowledgeSendPackLifecycleWirePack(t, client, destination, packs[0])
			close(initialResume)
		}
		if c.selectiveCopy {
			<-copyReached
			// Public receipt plus eviction feedback schedules an explicit
			// recovery whose successful write does not increment sendCount.
			ack := &protocol.Ack{SequenceId: packs[2].SequenceId, MessageId: packs[2].MessageId,
				Selective: true, EvictedSequenceNumbers: []uint64{2}}
			if ok, err := sequence.Ack(ack, 0); !ok || err != nil {
				t.Fatalf("selective eviction feedback was not admitted: %t %v", ok, err)
			}
			synctest.Wait()
			close(copyResume)
		}
		if c.failedWrite {
			<-failedReached
			for range cap(route) {
				route <- MessagePoolGet(1)
			}
			close(failedResume)
		}
		<-dueReached
		prefix := packs[1]
		if c.failedWrite {
			prefixId, _ := IdFromBytes(prefix.MessageId)
			item := sequence.resendQueue.GetByMessageId(prefixId)
			prefix = decodeSendPackLifecycleWirePack(t, item.transferFrameBytes)
			if item.sendCount != 2 || sequence.resendWriteCount.Load() != 0 || len(route) != cap(route) {
				t.Fatal("failed control did not refuse one actual promoted write")
			}
			for len(route) > 0 {
				MessagePoolReturn(<-route)
			}
		} else if c.earlierCopy {
			found := false
			for len(route) > 0 {
				copy := readPack()
				if copy.SequenceNumber == 1 {
					if found {
						t.Fatal("more than one promoted prefix before its ACK")
					}
					prefix, found = copy, true
				} else if copy.SequenceNumber != 3 {
					t.Fatalf("unexpected later physical copy: %d", copy.SequenceNumber)
				}
			}
			if !found || head.sendCount != 2 || head.rttState != sendItemRttUnavailable || !head.rttH1 ||
				priorCopyCompletedNanos >= prefixSelectionNanos || prefixSelectionNanos == 0 {
				t.Fatal("prior successful successor copy did not precede the new prefix envelope")
			}
		} else if !c.originalOnly {
			prefix = readPack()
			wantWrites := uint64(1)
			if c.laterCopy {
				wantWrites = 2
				copy := readPack()
				wantSends := 2
				if c.selectiveCopy {
					wantSends = 1
					if client.sendEvictionResendCount.Load() != 1 || head.selectiveAcked || !head.deliveryObserved {
						t.Fatal("explicit selective-eviction recovery did not consume the actual feedback")
					}
					originalLifetime = head.sendTime.Add(head.ackTimeout)
				}
				if copy.SequenceNumber != 2 || string(copy.MessageId) != string(packs[2].MessageId) ||
					head.sendCount != wantSends || head.deferralOutstanding || head.carrierChanged ||
					!head.reliableCarrierObserved || head.unreliableCarrierObserved ||
					!head.resendTime.After(time.Now()) || len(route) != 0 {
					t.Fatal("later copy did not successfully rewrite the same successor after the prefix repair")
				}
			}
			if sequence.resendWriteCount.Load() != wantWrites {
				t.Fatal("missing successful physical repair")
			}
		} else if sequence.resendWriteCount.Load() != 0 || len(route) != 0 {
			t.Fatal("healthy original control already wrote a repair")
		}
		if !c.originalOnly {
			if string(prefix.MessageId) != string(packs[1].MessageId) || prefix.SequenceNumber != 1 || !prefix.Head ||
				prefix.Tag == nil || packs[1].Tag == nil || prefix.Tag.SendTime <= packs[1].Tag.SendTime ||
				originalPhysical == 0 || originalPhysical >= int64(prefix.Tag.SendTime)*int64(time.Millisecond) {
				t.Fatal("promoted envelope lacks distinct Tag and ordered original premise")
			}
		}
		if c.newGeneration {
			client.RouteManager().UpdateTransport(transport, nil)
			client.RouteManager().UpdateTransport(transport, []Route{route})
			policy := sequence.transferFlightPolicy()
			if !policy.h1Only || policy.generation == generation {
				t.Fatal("same-route generation did not change")
			}
		}
		ackTag := prefix.Tag
		switch c.ackTag {
		case "original":
			ackTag = packs[1].Tag
		case "unknown":
			ackTag = &protocol.Tag{SendTime: 1}
		case "zero":
			ackTag = &protocol.Tag{}
		case "absent":
			ackTag = nil
		}
		phase.Store(1)
		if ok, err := sequence.Ack(&protocol.Ack{SequenceId: prefix.SequenceId, MessageId: prefix.MessageId, Tag: ackTag}, 0); !ok || err != nil {
			t.Fatalf("named prefix ACK was not admitted: %t %v", ok, err)
		}
		close(dueResume)
		<-ackReached
		wantSends, wantHighest := 1, uint64(1)
		if c.laterCopy || c.earlierCopy {
			wantSends = 2
		}
		if c.selectiveCopy {
			wantSends, wantHighest = 1, 2
		}
		if before.deferrals != 1 || before.sends != wantSends || before.highest != wantHighest || !before.retained || !before.h1 ||
			before.leased || before.changed || before.lifetime != originalLifetime || before.at.Before(originalDue) {
			t.Fatalf("ACK did not expose the unchanged, expired, ordinary deferred head: %+v", before)
		}
		if (before.generation != generation) != c.newGeneration {
			t.Fatal("wrong route-generation premise")
		}
		if c.futureDeferral {
			if before.outstanding || before.retry.After(before.at) {
				t.Fatalf("acknowledged repair left an existing contradicted future queue-residence deferral: %+v", before)
			}
		}
		phase.Store(3)
		close(ackResume)
		<-decisionReached
		if premise != "" || after.lifetime != originalLifetime || after.leased || after.changed {
			t.Fatalf("decision changed retention/lifetime: %s %+v", premise, after)
		}
		wantDeferral := c.originalOnly || c.ackTag != "" || c.newGeneration || c.failedWrite || c.laterCopy
		renewed := after.deferrals > before.deferrals && after.deferAck.Equal(before.at)
		t.Logf("case=%+v prefixTag=%v generation=%d->%d before=%+v after=%+v", c, ackTag, generation, before.generation, before, after)
		if renewed != wantDeferral {
			t.Fatalf("actual worker reused prefix ACK as queue-residence evidence=%t want=%t; successful distinct promoted envelope=%t; no response-time bound asserted", renewed, wantDeferral, !wantDeferral)
		}
		if !wantDeferral && (after.deferrals != before.deferrals || after.sends != before.sends+1 || after.outstanding) {
			t.Fatalf("contradicted queue residence did not return to ordinary recovery: %+v", after)
		}
	})
}
