// Signed public DATA must reach its callback without waiting for ingress to
// become quiet. The 32-MiB component geometry is not a whole SDK/device fixture.
package connect

import (
	"bytes"
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// A copied owner-stage event contains no pooled item or borrowed payload.
type bufferedDeliveryFairnessGate struct {
	stage         string
	turn          int
	sequence      Id
	number, floor uint64
	deliverItems  int
	deliverFrames int
	dataCapacity  int
}

// The desired bound is expressed in actual owner turns, independently of host
// speed, ACK/RTO timers, and how many redundant frames append to the batch.
func TestReceiveBufferedDataFlushDoesNotDependOnPastDuplicateQuiescence(t *testing.T) {
	testReceiveBufferedDeliveryFairness(t, "continuous-past-duplicates")
}

// With no competing input the ordinary channel-drain path must deliver/ACK.
func TestReceiveBufferedDataFlushesAfterQuiescentIngress(t *testing.T) {
	testReceiveBufferedDeliveryFairness(t, "quiescent")
}

// A retained future original is also legitimate input that appends no new
// callback frames while its missing predecessor remains source-owned.
func TestReceiveBufferedDataFlushDoesNotDependOnHeldFutureDuplicateQuiescence(t *testing.T) {
	testReceiveBufferedDeliveryFairness(t, "continuous-held-duplicates")
}

// Cancellation releases the paused worker and every independently paid copy.
func TestReceiveBufferedDataDuplicateDrainCancellationReturnsOwners(t *testing.T) {
	testReceiveBufferedDeliveryFairness(t, "cancel")
}

// The maintained signed fixture supplies actual contracts, public SendMulti,
// H1 carrier classification, owned decode, callbacks, and terminal ACKs. Its
// immutable queue projection matches the existing 32-MiB component controls:
// RX/Head/Pack 2,181,038, root 21,810,380, H1 85 owners / 174,762 encoded bytes.
// It deliberately retains that fixture's encryption-off, zero ACK compression,
// constant sender-window and lane-zero setup; this is no Fast attribution claim.
func testReceiveBufferedDeliveryFairness(t *testing.T, mode string) {
	t.Helper()
	GetMessagePoolAggregateStats()
	var cause string
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		layout := consumerPaidPackByteLayout{label: "32-buffered-delivery-fairness", byteLimit: 174762,
			leafBytes: 2181038, rootBytes: 21810380, countLimit: 85}
		root := NewTransferMemoryBudget(layout.rootBytes)
		rx := NewTransferMemoryBudgetWithParent(layout.leafBytes, root)
		head := NewTransferMemoryBudgetWithParent(layout.leafBytes, root)
		pack := NewTransferMemoryBudgetWithParent(layout.leafBytes, root)
		var warmId, duplicateId, targetId atomic.Pointer[Id]
		var armed atomic.Bool
		var turns atomic.Int32
		var overflow atomic.Bool
		var targetCallbacks atomic.Int64
		var targetAckBeforeCallback atomic.Bool
		var sourceOriginals, targetTerminals atomic.Int64
		var targetToken atomic.Uint64
		var targetTerminalFailed atomic.Bool
		gates := make(chan bufferedDeliveryFairnessGate, 1)
		resume := make(chan struct{}, 1)
		var ownerSequence atomic.Pointer[ReceiveSequence]
		fixture, log, _, _ := newConsumerPaidPackByteFixture(t, layout, rx, head, pack,
			func(event TransferProgressEvent) {
				id := targetId.Load()
				if id != nil && event.Stage == "receive_ack_begin" && event.MessageId == *id && targetCallbacks.Load() == 0 {
					targetAckBeforeCallback.Store(true)
				}
			}, func(receiver bool, settings *ClientSettings) {
				if !receiver {
					settings.SendBufferSettings.SendPackLifecycleObserver = func(event SendPackLifecycleObservation) {
						if event.MessageType != protocol.MessageType_IpIpPacketFromProvider {
							return
						}
						if event.Phase == SendPackLifecyclePhaseStarted && sourceOriginals.Add(1) == 2 {
							targetToken.Store(event.Token)
						}
						if event.Phase == SendPackLifecyclePhaseTerminal && event.Token == targetToken.Load() {
							targetTerminalFailed.Store(event.Err != nil)
							targetTerminals.Add(1)
						}
					}
					return
				}
				settings.ReceiveBufferSettings.receiveOwnerStageForTest = func(sequence *ReceiveSequence, owner *decodedPackOwner, stage string) {
					if !armed.Load() || owner == nil {
						return
					}
					duplicate, target := duplicateId.Load(), targetId.Load()
					if duplicate == nil || target == nil {
						return
					}
					item := &owner.receiveItem
					turn := 0
					if stage == "head_buffered" && item.messageId == *target {
						ownerSequence.Store(sequence)
					} else if stage == "pack_released" && item.messageId == *duplicate {
						turn = int(turns.Add(1))
					} else {
						return
					}
					event := bufferedDeliveryFairnessGate{stage: stage, turn: turn, sequence: sequence.sequenceId,
						number: item.sequenceNumber, floor: sequence.nextSequenceNumber,
						deliverItems: len(sequence.deliverItems), deliverFrames: len(sequence.deliverFrames), dataCapacity: cap(sequence.packs)}
					select {
					case gates <- event:
					default:
						overflow.Store(true)
					}
					select {
					case <-resume:
					case <-sequence.ctx.Done():
					}
				}
			})
		// Cleanup is armed before any barrier. A fatal setup/identity assertion
		// cannot leave the receive worker waiting for the test's next turn.
		t.Cleanup(func() { armed.Store(false); fixture.cancel() })
		var targetPayload []byte
		fixture.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
			for _, frame := range frames {
				if frame.MessageType == protocol.MessageType_IpIpPacketFromProvider && targetPayload != nil && bytes.Equal(frame.MessageBytes, targetPayload) {
					targetCallbacks.Add(1)
				}
			}
		})
		var warmAcked, targetAcked atomic.Bool
		var warmCompleted, targetCompleted atomic.Int64
		started := time.Now()
		warm := consumerPaidPackByteSend(t, fixture, 0, 1, 0, &warmAcked, &warmCompleted, started)
		warmIdentity := warm.scan.messageId
		warmId.Store(&warmIdentity)
		duplicateId.Store(&warmIdentity)
		warmWire := MessagePoolCopy(warm.frame.bytes)
		t.Cleanup(func() {
			if warmWire != nil {
				MessagePoolReturn(warmWire)
			}
		})
		fixture.forward(warm.frame, fixture.receiverIn)
		fixture.acknowledge()
		if !warmAcked.Load() || fixture.deliveredCount != 1 || rx.UsedByteCount() != 0 || head.UsedByteCount() != 0 {
			t.Fatal("actual signed warm DATA did not deliver, really ACK and release its decode owner")
		}
		target := consumerPaidPackByteSend(t, fixture, 0, 1, 1, &targetAcked, &targetCompleted, started)
		targetIdentity := target.scan.messageId
		targetId.Store(&targetIdentity)
		targetPayload = bytes.Clone(target.frame.pack.Frames[0].MessageBytes)
		if target.scan.sequenceId != warm.scan.sequenceId || target.scan.sequenceNumber != warm.scan.sequenceNumber+1 ||
			target.frame.pack.Nack || target.frame.pack.DeliverImmediately || protocolOnlyPack(target.frame.pack) ||
			len(target.frame.pack.Frames) != 1 || target.scan.consumerFrameCount != 1 {
			t.Fatal("target is not the next real reliable ordinary consumer DATA on the signed warm generation")
		}
		var gap, future consumerPaidPackByteWire
		var gapAcked, futureAcked atomic.Bool
		var gapCompleted, futureCompleted atomic.Int64
		futureSelectiveReceipts := 0
		duplicateNumber := warm.scan.sequenceNumber
		duplicatePack := warm.frame.pack
		if mode == "continuous-held-duplicates" {
			gap = consumerPaidPackByteSend(t, fixture, 0, 1, 2, &gapAcked, &gapCompleted, started)
			future = consumerPaidPackByteSend(t, fixture, 0, 1, 3, &futureAcked, &futureCompleted, started)
			if gap.scan.sequenceNumber != target.scan.sequenceNumber+1 || future.scan.sequenceNumber != gap.scan.sequenceNumber+1 ||
				gap.scan.sequenceId != target.scan.sequenceId || future.scan.sequenceId != target.scan.sequenceId || future.frame.pack.Head {
				t.Fatal("held-duplicate premise lost its real consecutive missing/current/future originals")
			}
			MessagePoolReturn(warmWire)
			warmWire = MessagePoolCopy(future.frame.bytes)
			futureIdentity := future.scan.messageId
			duplicateId.Store(&futureIdentity)
			duplicateNumber, duplicatePack = future.scan.sequenceNumber, future.frame.pack
			fixture.forward(future.frame, fixture.receiverIn)
			for len(fixture.receiverOut) != 0 {
				ack := fixture.take(fixture.receiverOut)
				if ack.ack == nil || !bytes.Equal(ack.ack.SequenceId, future.frame.pack.SequenceId) {
					t.Fatal("real held-future feedback lost its exact signed sequence")
				}
				if bytes.Equal(ack.ack.MessageId, future.frame.pack.MessageId) {
					if !ack.ack.Selective || ack.ack.DeliveredImmediately {
						t.Fatal("future receipt claimed delivery across the actual target/predecessor holes")
					}
					futureSelectiveReceipts++
				}
				fixture.forward(ack, fixture.senderIn)
			}
			if fixture.deliveredCount != 1 || targetAcked.Load() || gapAcked.Load() || futureAcked.Load() {
				t.Fatal("real future custody invented terminal progress over the two actual missing originals")
			}
			if futureSelectiveReceipts == 0 {
				t.Fatal("future duplicate premise did not retain a real selectively acknowledged owner")
			}
		}
		takeGate := func(stage string, turn int) bufferedDeliveryFairnessGate {
			t.Helper()
			synctest.Wait()
			select {
			case event := <-gates:
				if event.stage != stage || event.turn != turn || event.sequence != target.scan.sequenceId || overflow.Load() {
					t.Fatalf("actual owner gate lost its fixed identity/order: %+v", event)
				}
				return event
			default:
				t.Fatal("public DATA did not reach its expected owner barrier")
				return bufferedDeliveryFairnessGate{}
			}
		}
		if mode != "quiescent" {
			armed.Store(true)
		}
		fixture.forward(target.frame, fixture.receiverIn)
		var checkpoint bufferedDeliveryFairnessGate
		var queuedCopies int
		var cutStarted time.Time
		if mode != "quiescent" {
			checkpoint = takeGate("head_buffered", 0)
			if checkpoint.number != target.scan.sequenceNumber || checkpoint.floor != target.scan.sequenceNumber+1 ||
				checkpoint.deliverItems != 1 || checkpoint.deliverFrames != 1 || checkpoint.dataCapacity != layout.countLimit ||
				targetCallbacks.Load() != 0 || targetAcked.Load() || targetAckBeforeCallback.Load() {
				t.Fatal("actual new DATA was not buffered and unacknowledged before duplicate ingress")
			}
			sequence := ownerSequence.Load()
			if sequence == nil {
				t.Fatal("buffered target lost its actual worker")
			}
			cutStarted = time.Now()
			if mode == "continuous-held-duplicates" {
				held := sequence.receiveQueue.GetByMessageId(future.scan.messageId)
				if held == nil || held.sequenceNumber != future.scan.sequenceNumber || sequence.receiveQueue.Len() != 1 ||
					gapAcked.Load() || futureAcked.Load() {
					t.Fatal("duplicate source was not actually retained beyond the next missing predecessor")
				}
			}
			queueDuplicate := func() {
				t.Helper()
				before := fixture.receiver.ReceiveStats().PackHandoffDropCount
				copy := &windowRoundFrame{bytes: MessagePoolCopy(warmWire), pack: duplicatePack}
				fixture.heldFrames = append(fixture.heldFrames, copy)
				fixture.forward(copy, fixture.receiverIn)
				queuedCopies++
				if len(sequence.packs) != 1 || sequence.packQueueCount.Load() != 1 ||
					fixture.receiver.ReceiveStats().PackHandoffDropCount != before {
					t.Fatal("independent public past duplicate did not acquire one real ready DATA slot")
				}
			}
			limit := 2 * checkpoint.dataCapacity
			if mode == "cancel" {
				limit = 3
			}
			for turn := 1; turn <= limit; turn++ {
				queueDuplicate()
				resume <- struct{}{}
				checkpoint = takeGate("pack_released", turn)
				if checkpoint.number != duplicateNumber || checkpoint.floor != target.scan.sequenceNumber+1 ||
					targetAckBeforeCallback.Load() || targetCallbacks.Load() > 1 || fixture.deliveredCount > 2 || time.Now() != cutStarted ||
					fixture.sender.SendRecoveryStats().TimeoutResendWriteCount != 0 || gapAcked.Load() || futureAcked.Load() {
					t.Fatal("past duplicate changed original order, callback truth, or advanced a recovery timer")
				}
				fixture.acknowledge()
			}
			// The measured callback/ACK oracle runs while both the current
			// duplicate and one further ready duplicate still belong to ingress.
			queueDuplicate()
			if mode != "cancel" && (targetCallbacks.Load() != 1 || !targetAcked.Load()) {
				if targetCallbacks.Load() != 0 || targetAcked.Load() {
					t.Fatal("fairness cut did not isolate the zero-callback/zero-terminal-ACK starvation mechanism")
				}
				cause = fmt.Sprintf("BUFFERED_DATA_CALLBACK_STARVED_BY_DUPLICATE_DRAIN: mode=%s turns=%d ready=1 callback=%d real_terminal_ack=%t", mode, limit, targetCallbacks.Load(), targetAcked.Load())
			}
			t.Logf("BUFFERED_DATA_FAIRNESS_CUT mode=%s turns=%d ready=%d callbacks=%d real_terminal_ack=%t buffered_items=%d buffered_frames=%d elapsed=%s",
				mode, limit, len(sequence.packs), targetCallbacks.Load(), targetAcked.Load(), checkpoint.deliverItems, checkpoint.deliverFrames, time.Since(cutStarted))
			armed.Store(false)
			if mode == "cancel" {
				fixture.cancel()
			} else {
				resume <- struct{}{}
			}
		}
		synctest.Wait()
		if mode != "cancel" {
			fixture.acknowledge()
			if targetCallbacks.Load() != 1 || !targetAcked.Load() || fixture.deliveredCount != 2 || targetAckBeforeCallback.Load() ||
				fixture.sender.SendRecoveryStats().TimeoutResendWriteCount != 0 || !cutStarted.IsZero() && time.Now() != cutStarted {
				t.Fatal("stopping duplicate ingress did not deliver/ACK exactly once without another original or RTO")
			}
			if mode == "continuous-held-duplicates" {
				if gapAcked.Load() || futureAcked.Load() {
					t.Fatal("flushing the fresh head falsely covered the still-missing predecessor/future custody")
				}
				fixture.forward(gap.frame, fixture.receiverIn)
				fixture.acknowledge()
				if !gapAcked.Load() || !futureAcked.Load() || fixture.deliveredCount != 4 || targetCallbacks.Load() != 1 || time.Now() != cutStarted {
					t.Fatal("actual missing predecessor did not finish the retained future's real delivery/ACK exactly once")
				}
			}
		}
		fixture.cancel()
		joined, cancelJoin := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelJoin()
		for _, client := range []*Client{fixture.sender, fixture.receiver} {
			if err := client.CloseAndWait(joined); err != nil {
				t.Fatalf("join buffered-delivery fairness client: %v", err)
			}
		}
		for _, route := range []Route{fixture.senderOut, fixture.senderIn, fixture.receiverOut, fixture.receiverIn} {
			for len(route) != 0 {
				MessagePoolReturn(<-route)
			}
		}
		for _, budget := range []*TransferMemoryBudget{pack, head, rx, root} {
			assertRetainedBudgetBalance(t, budget)
			if budget.UsedByteCount() != 0 || budget.Stats().QuotaByteCount != 0 {
				t.Fatal("buffered delivery/duplicate cancellation retained paid memory after both clients joined")
			}
		}
		if targetCallbacks.Load() > 1 || targetAckBeforeCallback.Load() || overflow.Load() || len(log.messages()) != 0 ||
			targetTerminals.Load() != 1 || targetTerminalFailed.Load() == targetAcked.Load() {
			t.Fatal("callback/ACK/observer/worker truth failed independently of the fairness oracle")
		}
		MessagePoolReturn(warmWire)
		warmWire = nil
		t.Logf("BUFFERED_DATA_FAIRNESS_OWNERS_JOINED mode=%s independently_owned_duplicate_copies=%d final_rx=0 final_head=0 final_root=0", mode, queuedCopies)
	})
	t.Log("BUFFERED_DATA_FAIRNESS_CLEANUP_COMPLETE")
	if cause != "" {
		t.Error(cause)
	}
}
