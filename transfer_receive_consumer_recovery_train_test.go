// A real receive worker can lag its already-paid incoming prefix. Future
// tentative custody cannot authorize a gap, but contiguous paid predecessors
// must not turn an admitted recovery train into one retry per scheduling turn.
package connect

import (
	"bytes"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func TestReceiveConsumerFullRxContiguousRecoveryTrainAdmitsPaidSuccessors(t *testing.T) {
	testReceiveConsumerPaidRecoveryTrain(t, "contiguous")
}

func TestReceiveConsumerFullRxRecoveryTrainCannotJumpUnpaidGap(t *testing.T) {
	testReceiveConsumerPaidRecoveryTrain(t, "gap")
}

func TestReceiveConsumerFullRxRecoveryTrainKeepsChildCap(t *testing.T) {
	testReceiveConsumerPaidRecoveryTrain(t, "child-cap")
}

func TestReceiveConsumerFullRxRecoveryTrainCanceledOwnerCannotAuthorize(t *testing.T) {
	testReceiveConsumerPaidRecoveryTrain(t, "canceled")
}

func TestReceiveConsumerFullRxRecoveryTrainCannotUseOtherSequencePrefix(t *testing.T) {
	testReceiveConsumerPaidRecoveryTrain(t, "other-sequence")
}

func TestReceiveConsumerRecoveryTrainUnknownCompactPredecessorRefundsWithoutAck(t *testing.T) {
	testReceiveConsumerPaidRecoveryTrain(t, "unknown-contract")
}

func TestReceiveConsumerRecoveryTrainUnsupportedWireRoleCannotExtendPrefix(t *testing.T) {
	testReceiveConsumerPaidRecoveryTrain(t, "invalid-role")
}

// This witness controls route ordering and the existing receive-worker seam;
// it never changes a budget, sequence number, contract or ACK after construction.
func testReceiveConsumerPaidRecoveryTrain(t *testing.T, mode string) {
	t.Helper()
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		var cause string
		t.Cleanup(func() {
			if cause != "" {
				t.Fatal(cause)
			}
		})
		const foreignCost, smallCost = ByteCount(5120), ByteCount(1536)
		const rxLimit = foreignCost + smallCost
		root := NewTransferMemoryBudget(2 * rxLimit)
		clientBudget := NewTransferMemoryBudgetWithParent(2*rxLimit, root)
		rx := NewTransferMemoryBudgetWithParent(rxLimit, clientBudget)
		head := NewTransferMemoryBudgetWithParent(rxLimit, clientBudget)
		pack := NewTransferMemoryBudgetWithParent(rxLimit, clientBudget)
		t.Cleanup(func() {
			for _, budget := range []*TransferMemoryBudget{pack, head, rx, clientBudget, root} {
				assertRetainedBudgetBalance(t, budget)
				if budget.Stats().QuotaByteCount != 0 {
					t.Error("recovery train retained logical quota after joined clients")
				}
			}
			t.Log("RECEIVE_RECOVERY_TRAIN_CLEANUP_JOINED")
		})
		firstEntered, releaseFirst := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
		var armed atomic.Bool
		var firstId Id
		var firstOwner *decodedPackOwner
		var firstSequence *ReceiveSequence
		ownerStage := func(sequence *ReceiveSequence, owner *decodedPackOwner, stage string) {
			if stage == "pack_released" && owner != nil && owner.receiveItem.messageId == firstId && armed.CompareAndSwap(true, false) {
				firstOwner, firstSequence = owner, sequence
				close(firstEntered)
				<-releaseFirst
			}
		}
		fixture, logger := newConsumerFinalHandoffFixture(t, rx, head, pack, ownerStage, nil)
		// This cleanup must unblock the worker before the fixture joins it.
		t.Cleanup(release)
		lastApplication := map[uint32]uint64{}
		write := func(lane uint32, payloadBytes int) *windowRoundFrame {
			packet := ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagAck, 19, bytes.Repeat([]byte{0x5a}, payloadBytes))
			frame := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider, Raw: true, MessageBytes: packet}
			accepted, err := fixture.sender.SendWithTimeoutDetailed(frame, fixture.receiver.ClientId(), func(err error) {
				if err == nil {
					fixture.ackedCount++
				}
			}, time.Second, TransferKey{LogicalLane: lane})
			if !accepted || err != nil {
				MessagePoolReturn(frame.MessageBytes)
				t.Fatalf("real recovery train source refused lane=%d accepted=%t error=%v", lane, accepted, err)
			}
			for range cap(fixture.senderOut) {
				wire := fixture.take(fixture.senderOut)
				if wire.pack == nil {
					t.Fatal("recovery train source did not encode a Pack")
				}
				if wire.pack.LogicalLane != lane {
					continue
				}
				for _, payload := range wire.pack.Frames {
					if payload.MessageType == protocol.MessageType_IpIpPacketFromProvider {
						if last, present := lastApplication[lane]; present && wire.pack.SequenceNumber <= last {
							break
						}
						lastApplication[lane] = wire.pack.SequenceNumber
						return wire
					}
				}
				if len(wire.pack.Frames) == 0 {
					fixture.forward(wire, fixture.receiverIn)
					fixture.acknowledge()
				}
			}
			t.Fatal("real recovery train source exhausted its bounded wire capture")
			return nil
		}
		foreignFirst := write(1, 1280)
		var originals, backups []*windowRoundFrame
		for index := range 4 {
			payload := 32
			if mode == "child-cap" && index == 0 {
				payload = 1280
			}
			original := write(0, payload)
			originals = append(originals, original)
			// A separate caller-owned immutable share is a later controlled
			// reoffer, never a second physical owner or an invented source ACK.
			MessagePoolShareReadOnly(original.bytes)
			backup := &windowRoundFrame{bytes: original.bytes, pack: original.pack}
			fixture.heldFrames = append(fixture.heldFrames, backup)
			backups = append(backups, backup)
		}
		foreignTail := write(1, 1280)
		targetTail := write(0, 32)
		MessagePoolShareReadOnly(targetTail.bytes)
		tailBackup := &windowRoundFrame{bytes: targetTail.bytes, pack: targetTail.pack}
		fixture.heldFrames = append(fixture.heldFrames, tailBackup)
		var otherCurrent, other *windowRoundFrame
		if mode == "other-sequence" {
			// A signed current original can now reserve its own child credit.
			// Keep that real source unoffered and test its future successor,
			// which has no same-sequence paid prefix or independent entitlement.
			otherCurrent = write(2, 32)
			other = write(2, 32)
		}
		findSequence := func(frame *windowRoundFrame) *ReceiveSequence {
			fixture.receiver.receiveBuffer.mutex.Lock()
			defer fixture.receiver.receiveBuffer.mutex.Unlock()
			for id, sequence := range fixture.receiver.receiveBuffer.receiveSequences {
				if id.SequenceId == RequireIdFromBytes(frame.pack.SequenceId) && id.LogicalLane == frame.pack.LogicalLane {
					return sequence
				}
			}
			t.Fatal("real receiver generation is absent")
			return nil
		}
		rewrite := func(original *windowRoundFrame, change func(*protocol.TransferFrame)) *windowRoundFrame {
			var frame protocol.TransferFrame
			if err := ProtoUnmarshal(original.bytes, &frame); err != nil {
				t.Fatal(err)
			}
			change(&frame)
			wire, err := ProtoMarshal(&frame)
			if err != nil {
				t.Fatal(err)
			}
			changed := &windowRoundFrame{bytes: wire, pack: frame.Pack}
			fixture.heldFrames = append(fixture.heldFrames, changed)
			return changed
		}
		sequence, foreign := findSequence(originals[0]), findSequence(foreignFirst)
		if mode == "other-sequence" {
			unrelated := findSequence(other)
			if unrelated == sequence || unrelated == foreign || unrelated.receiveContract == nil ||
				!unrelated.receiveContract.verifiedReceiveProofSet || other.pack.Head ||
				other.pack.SequenceNumber != otherCurrent.pack.SequenceNumber+1 ||
				unrelated.nextSequenceNumber != otherCurrent.pack.SequenceNumber ||
				unrelated.holdPromise.ingressFloor != otherCurrent.pack.SequenceNumber ||
				unrelated.holdPromise.ingressHead != nil || unrelated.holdPromise.set || unrelated.receiveQueue.Len() != 0 {
				t.Fatal("unrelated successor did not retain a genuine unpaid predecessor")
			}
		}
		firstId = RequireIdFromBytes(originals[0].pack.MessageId)
		firstNumber := originals[0].pack.SequenceNumber
		if targetTail.pack.SequenceNumber != firstNumber+uint64(len(originals)) {
			t.Fatal("actual tentative tail does not follow all four missing application originals")
		}
		for index, original := range originals {
			if original.pack.SequenceNumber != firstNumber+uint64(index) || original.pack.Head || original.pack.Nack ||
				!bytes.Equal(original.pack.SequenceId, targetTail.pack.SequenceId) {
				t.Fatal("recovery train does not contain consecutive original payloads")
			}
		}
		fixture.forward(foreignTail, fixture.receiverIn)
		fixture.forward(targetTail, fixture.receiverIn)
		foreignItem := foreign.receiveQueue.GetByMessageId(RequireIdFromBytes(foreignTail.pack.MessageId))
		tailItem := sequence.receiveQueue.GetByMessageId(RequireIdFromBytes(targetTail.pack.MessageId))
		if foreignItem == nil || tailItem == nil || foreignItem.committed || tailItem.committed ||
			foreignItem.QueueByteCount() != foreignCost || tailItem.QueueByteCount() != smallCost ||
			rx.UsedByteCount() != rxLimit || head.UsedByteCount() != 0 || root.UsedByteCount() != rxLimit ||
			sequence.holdPromise.set || foreign.holdPromise.set || len(fixture.receiverOut) != 0 ||
			sequence.nextSequenceNumber != firstNumber || sequence.holdPromise.ingressFloor != firstNumber ||
			fixture.deliveredCount != 0 || fixture.ackedCount != 0 {
			t.Fatal("actual foreign and target tentative custody did not fill RX without a SACK promise")
		}
		before := time.Now()
		armed.Store(true)
		firstOffer := originals[0]
		var unknownContract Id
		if mode == "unknown-contract" {
			unknownContract = NewId()
			firstOffer = rewrite(originals[0], func(frame *protocol.TransferFrame) {
				frame.Pack.Head = true
				frame.Pack.ContractFrame = nil
				frame.Pack.ContractId = unknownContract.Bytes()
			})
		}
		fixture.forward(firstOffer, fixture.receiverIn)
		select {
		case <-firstEntered:
		default:
			t.Fatal("first actual recovery did not reach the held worker")
		}
		firstCost := smallCost
		if mode == "child-cap" {
			firstCost = foreignCost
		}
		if firstSequence != sequence || firstOwner == nil || firstOwner.receiveItem.memoryBudget != head ||
			firstOwner.receiveItem.QueueByteCount() != firstCost || !firstOwner.receivePack.receivePublishedReliable ||
			!firstOwner.receivePack.receiveConsumerData || head.UsedByteCount() != firstCost ||
			rx.UsedByteCount() != rxLimit || root.UsedByteCount() != rxLimit+firstCost ||
			sequence.holdPromise.ingressFloor != firstNumber || fixture.deliveredCount != 0 {
			t.Fatal("held first predecessor lacks exact paid published recovery ownership")
		}
		if mode == "canceled" {
			sequence.Cancel()
		}
		dropBefore := fixture.receiver.receivePackHandoffDropCount.Load()
		indices := []int{1, 2, 3}
		if mode == "gap" {
			indices = []int{2}
		} else if mode == "canceled" {
			indices = []int{1}
		} else if mode == "other-sequence" {
			indices = nil
			fixture.forward(other, fixture.receiverIn)
		} else if mode == "invalid-role" {
			indices = nil
			invalid := rewrite(originals[1], func(frame *protocol.TransferFrame) {
				role := protocol.SequenceRole(99)
				frame.SessionRole = &role
			})
			fixture.forward(invalid, fixture.receiverIn)
		}
		for _, index := range indices {
			fixture.forward(originals[index], fixture.receiverIn)
		}
		drops := fixture.receiver.receivePackHandoffDropCount.Load() - dropBefore
		gotBytes := head.UsedByteCount()
		if time.Now() != before || rx.UsedByteCount() != rxLimit || root.UsedByteCount() != rxLimit+gotBytes ||
			gotBytes > head.TotalByteCount() || sequence.holdPromise.ingressFloor != firstNumber ||
			fixture.deliveredCount != 0 || fixture.ackedCount != 0 || len(fixture.receiverOut) != 0 {
			t.Fatal("receive train changed time, ancestor accounting, delivery or ACK authority while its worker was held")
		}
		if mode == "contiguous" || mode == "unknown-contract" {
			if gotBytes != 4*smallCost || drops != 0 {
				if mode == "contiguous" {
					cause = "RECEIVE_CONTIGUOUS_PAID_PREFIX_REJECTED"
				} else {
					t.Fatal("unknown-compact control did not first admit the real paid contiguous train")
				}
			}
		} else if mode == "child-cap" {
			if gotBytes != rxLimit || drops != 2 {
				cause = "RECEIVE_CONTIGUOUS_CHILD_CAP_BOUNDARY_NOT_REACHED"
			}
		} else if gotBytes != firstCost || drops != 1 {
			t.Fatal("a gap, canceled generation or unrelated sequence borrowed the first predecessor's authority")
		}
		t.Logf("[recovery-train] mode=%s missing=4 admitted_child_bytes=%d first_child_bytes=%d refused=%d rx=%d head_cap=%d timer_advance=0", mode, gotBytes, firstCost, drops, rx.UsedByteCount(), head.TotalByteCount())
		release()
		synctest.Wait()
		if mode == "canceled" {
			// The already-admitted first owner may complete before Run observes
			// cancellation. It cannot authorize the subsequently offered Pack.
			if sequence.ctx.Err() == nil || fixture.deliveredCount > 1 {
				t.Fatal("canceled paid predecessor authorized additional application data")
			}
			return
		}
		if mode == "unknown-contract" {
			request := fixture.take(fixture.receiverOut)
			if request.ack == nil || request.ack.Selective ||
				!bytes.Equal(request.ack.MissingContractId, unknownContract.Bytes()) ||
				!bytes.Equal(request.ack.MessageId, originals[0].pack.MessageId) ||
				len(fixture.receiverOut) != 0 || fixture.deliveredCount != 0 || fixture.ackedCount != 0 ||
				sequence.nextSequenceNumber != firstNumber || sequence.holdPromise.ingressFloor != firstNumber ||
				sequence.receiveQueue.GetBySequenceNumber(targetTail.pack.SequenceNumber) != tailItem ||
				tailItem.committed || sequence.receiveQueue.Len() != 1 || sequence.ctx.Err() != nil ||
				head.UsedByteCount() != 0 || rx.UsedByteCount() != rxLimit || root.UsedByteCount() != rxLimit ||
				fixture.receiver.receiveQueueCommitCount.Load() != 0 || len(logger.messages()) != 0 {
				t.Fatal("failed compact predecessor acknowledged data, stranded child ownership or changed tentative suffix custody")
			}
			t.Log("RECEIVE_RECOVERY_PREFIX_FAILURE_REFUNDED_WITHOUT_ACK")
		}
		if mode == "gap" {
			root.admissionLock.Lock()
			pressure := sequence.memoryPressure
			root.admissionLock.Unlock()
			if !pressure.activeSet || !pressure.ready || pressure.owner != nil ||
				pressure.active.sequenceNumber != originals[2].pack.SequenceNumber ||
				pressure.reservedByteCount != smallCost || sequence.receiveQueue.Len() != 0 ||
				sequence.nextSequenceNumber != firstNumber+1 || fixture.deliveredCount != 1 ||
				fixture.ackedCount != 0 || fixture.receiver.receiveQueueCommitCount.Load() != 0 {
				t.Fatal("unpaid-gap pressure did not reclaim only the tentative tail for its exact refused identity")
			}
		}
		// Complete both old and fixed cases using immutable caller-owned wire
		// shares after the worker runs. This is cleanup evidence, not a timer
		// or native retransmission latency claim.
		// A refused successor can request ordinary worker-side pressure. It may
		// reclaim the tentative tail, or replace a later demand only after the
		// earlier retry is offered once. Two bounded ordered rounds finish both
		// cases using the genuine retained source wire; neither is timer proof.
		seeds := append(backups, tailBackup)
		for round := 0; round < 2 && sequence.nextSequenceNumber <= targetTail.pack.SequenceNumber; round++ {
			for _, seed := range seeds {
				MessagePoolShareReadOnly(seed.bytes)
				retry := &windowRoundFrame{bytes: seed.bytes, pack: seed.pack}
				fixture.heldFrames = append(fixture.heldFrames, retry)
				fixture.forward(retry, fixture.receiverIn)
				fixture.acknowledge()
			}
			if mode == "gap" && round == 0 {
				root.admissionLock.Lock()
				pressure := sequence.memoryPressure
				root.admissionLock.Unlock()
				if pressure.activeSet || pressure.owner != nil || pressure.reservedByteCount != 0 ||
					pressure.borrowedByteCount != 0 || sequence.nextSequenceNumber != targetTail.pack.SequenceNumber+1 ||
					sequence.receiveQueue.Len() != 0 || fixture.deliveredCount != 5 || fixture.ackedCount != 5 ||
					fixture.receiver.receiveQueueCommitCount.Load() != 0 || rx.UsedByteCount() != foreignCost ||
					head.UsedByteCount() != 0 || root.UsedByteCount() != foreignCost {
					t.Fatal("independently paid exact-current retry did not drain the genuine gap without suffix SACK or residual credit")
				}
				t.Log("RECEIVE_RECOVERY_GAP_CURRENT_PROGRESS_AFTER_WORKER_RELEASE no_suffix_sack=true delivered=5 acked=5")
			}
			t.Logf("[recovery-train-completion] round=%d first=%d last=%d tail=%d target_next=%d tail_retained=%t delivered=%d", round+1, firstNumber, originals[len(originals)-1].pack.SequenceNumber, targetTail.pack.SequenceNumber, sequence.nextSequenceNumber, sequence.receiveQueue.GetByMessageId(RequireIdFromBytes(targetTail.pack.MessageId)) != nil, fixture.deliveredCount)
		}
		fixture.forward(foreignFirst, fixture.receiverIn)
		fixture.acknowledge()
		if mode == "other-sequence" {
			// The unrelated future original was truly refused; both its real
			// missing predecessor and this successor stay sender-owned until
			// cleanup. Its ordinary receive worker stays empty.
			if findSequence(other).receiveQueue.Len() != 0 {
				t.Fatal("unrelated generation acquired a retained payload")
			}
		}
		if fixture.deliveredCount != 7 || fixture.ackedCount != 7 || rx.UsedByteCount() != 0 ||
			head.UsedByteCount() != 0 || pack.UsedByteCount() != 0 || root.UsedByteCount() != 0 ||
			sequence.nextSequenceNumber != targetTail.pack.SequenceNumber+1 || sequence.holdPromise.ingressFloor != sequence.nextSequenceNumber ||
			sequence.receiveQueue.Len() != 0 || foreign.receiveQueue.Len() != 0 || len(logger.messages()) != 0 {
			t.Fatalf("real recovery train did not drain exact payloads and physical owners before cleanup: delivered=%d acked=%d rx=%d head=%d pack=%d root=%d target_queue=%d foreign_queue=%d target_next=%d target_floor=%d foreign_next=%d foreign_floor=%d errors=%d", fixture.deliveredCount, fixture.ackedCount, rx.UsedByteCount(), head.UsedByteCount(), pack.UsedByteCount(), root.UsedByteCount(), sequence.receiveQueue.Len(), foreign.receiveQueue.Len(), sequence.nextSequenceNumber, sequence.holdPromise.ingressFloor, foreign.nextSequenceNumber, foreign.holdPromise.ingressFloor, len(logger.messages()))
		}
	})
}
