// Exact signed promoted-copy receipts bound new ordinary admission while
// the old paid cohort, protocol traffic and caller cancellation stay live.
package connect

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func TestFifoRepairAdmissionPublicWaitsForFixedCohort(t *testing.T) {
	testFifoRepairAdmissionPublic(t, "drain")
}

func TestFifoRepairAdmissionPublicCallerCancellation(t *testing.T) {
	testFifoRepairAdmissionPublic(t, "cancel")
}

func TestFifoRepairAdmissionQueuedCallerDeadline(t *testing.T) {
	testFifoRepairAdmissionPublic(t, "queued_deadline")
}

// The barrier runs at a real tail timer after the head's successful natural
// promoted write. Its genuine receiver ACK is coalesced before that tail acts.
// Every earlier original has already traversed and been dropped at this FIFO.
func testFifoRepairAdmissionPublic(t *testing.T, mode string) {
	t.Helper()
	GetMessagePoolAggregateStats()
	var earlyAdmission bool
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		const leafBytes = ByteCount(128 * 1024)
		root := NewTransferMemoryBudget(4 * leafBytes)
		tx := NewTransferMemoryBudgetWithParent(leafBytes, root)
		rx := NewTransferMemoryBudgetWithParent(leafBytes, root)
		head := NewTransferMemoryBudgetWithParent(leafBytes, root)
		pack := NewTransferMemoryBudgetWithParent(leafBytes, root)
		t.Cleanup(func() {
			for _, budget := range []*TransferMemoryBudget{tx, rx, head, pack, root} {
				assertRetainedBudgetBalance(t, budget)
				if budget.UsedByteCount() != 0 || budget.Stats().QuotaByteCount != 0 {
					t.Error("FIFO admission retained physical or quota custody after joined cleanup")
				}
			}
		})
		var preparedOwner *fifoAdmissionPreparedOwner
		// This runs after the inherited Client cleanup has joined any owner
		// left by a failed premise, and before the budget reconciliation above.
		t.Cleanup(func() {
			if preparedOwner != nil {
				preparedOwner.credit.release()
			}
		})
		var senderRef atomic.Pointer[SendSequence]
		var tailNumber atomic.Uint64
		var barrierEntered atomic.Bool
		entered, release := make(chan struct{}), make(chan struct{})
		layout := consumerPaidPackByteLayout{label: "fixed-fifo-admission", byteLimit: 174762,
			leafBytes: leafBytes, rootBytes: 4 * leafBytes, countLimit: 85}
		fixture, log, _, _ := newConsumerPaidPackByteFixture(t, layout, rx, head, pack, nil,
			func(receiver bool, settings *ClientSettings) {
				settings.ReceiveBufferSettings.AdvertiseReceiveWindow = true
				if receiver {
					return
				}
				send := settings.SendBufferSettings
				send.ResendQueueBudget = tx
				send.ResendQueueRetainedByteAccounting = true
				send.ResendQueueMinByteCount = 0
				send.ResendQueueMaxByteCount = leafBytes
				send.WindowSizing = WindowSizingFromDelivery
				send.ApplyWindowSizing()
				// The queued-deadline cell owns one real admitted channel slot.
				// No hidden scheduler or unbounded producer queue is introduced.
				send.SequenceBufferSize = 1
				// This proof's resend timers age the DATA gap past the 2 s
				// immediate bound before its NoBudget ACK is offered. It pins
				// FIFO admission, not that bound, so the ACK stays immediate;
				// ip_provider_control_gap_age_test.go pins the bound itself.
				send.ImmediateDeliveryMaxGapAge = time.Hour
				send.beforeDueResendForTest = func(id sendSequenceId, number uint64) {
					owner := senderRef.Load()
					if owner == nil || owner.id() != id || number != tailNumber.Load() ||
						len(owner.sendItems) < 3 || owner.sendItems[0].sendCount < 2 ||
						!barrierEntered.CompareAndSwap(false, true) {
						return
					}
					close(entered)
					select {
					case <-release:
					case <-owner.ctx.Done():
					}
				}
			})
		t.Cleanup(func() {
			select {
			case <-release:
			default:
				close(release)
			}
		})
		var done [4]atomic.Bool
		var completed [4]atomic.Int64
		started := time.Now()
		warm := consumerPaidPackByteSend(t, fixture, 1, 1, 0, &done[0], &completed[0], started)
		fixture.forward(warm.frame, fixture.receiverIn)
		fixture.acknowledge()
		sender := inflightHoleSender(t, fixture, warm.frame)
		senderRef.Store(sender)
		// The first original after an empty flight is legitimately a Head.
		// Retain a real anchor while publishing the three non-Head targets.
		var anchorDone atomic.Bool
		var anchorCompleted atomic.Int64
		anchor := consumerPaidPackByteSend(t, fixture, 1, 1, 10, &anchorDone, &anchorCompleted, started)
		var originals [3]consumerPaidPackByteWire
		for index := range originals {
			originals[index] = consumerPaidPackByteSend(t, fixture, 1, 1, index+1, &done[index+1], &completed[index+1], started)
			if originals[index].frame.pack.Head || !bytes.Equal(warm.frame.pack.SequenceId, originals[index].frame.pack.SequenceId) {
				t.Fatal("fixed cohort did not use actual non-Head originals on its warm wire generation")
			}
			fixture.drop(originals[index].frame)
		}
		fixture.forward(anchor.frame, fixture.receiverIn)
		fixture.acknowledge()
		if !anchorDone.Load() || len(sender.sendItems) != len(originals) ||
			sender.sendItems[0].messageId != RequireIdFromBytes(originals[0].frame.pack.MessageId) ||
			done[1].Load() || done[2].Load() || done[3].Load() {
			t.Fatal("actual anchor ACK did not expose exactly the three undelivered non-Head originals")
		}
		tailNumber.Store(originals[1].frame.pack.SequenceNumber)
		select {
		case <-entered:
		case <-sender.ctx.Done():
			t.Fatal("sender closed before its real post-promotion tail timer")
		}
		promoted := fixture.take(fixture.senderOut)
		first := originals[0].frame.pack
		if !promoted.pack.Head || !bytes.Equal(promoted.pack.MessageId, first.MessageId) ||
			promoted.pack.SequenceNumber != first.SequenceNumber || promoted.pack.Tag.GetSendTime() <= first.Tag.GetSendTime() ||
			len(sender.sendItems) != 3 || sender.sendItems[1].sendCount != 1 || sender.sendItems[2].sendCount != 1 ||
			!sender.fifoRepair.pending.set || !sender.fifoRepair.pending.successful {
			t.Fatal("real natural promotion did not preserve the exact untouched pre-existing DATA cohort")
		}
		freshPacket := ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagAck, 9000, bytes.Repeat([]byte{0x63}, 1240))
		freshFrame := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider, Raw: true, MessageBytes: MessagePoolCopy(freshPacket)}
		freshCtx, cancelFresh := context.WithCancel(fixture.ctx)
		defer cancelFresh()
		type admissionResult struct {
			accepted bool
			err      error
		}
		admission := make(chan admissionResult, 1)
		terminal := make(chan error, 1)
		var terminalCount atomic.Int64
		freshAck := func(err error) {
			if terminalCount.Add(1) != 1 {
				t.Error("fresh DATA callback repeated")
				return
			}
			terminal <- err
		}
		freshStarted := false
		producerDone := make(chan struct{})
		t.Cleanup(func() {
			cancelFresh()
			if freshStarted {
				<-producerDone
			} else {
				MessagePoolReturn(freshFrame.MessageBytes)
			}
		})
		prepared := mode == "prepared_deadline" || mode == "prepared_cancel"
		queued := mode == "queued_deadline" || prepared
		preparedCreditByteCount := ByteCount(16 * 1024)
		if prepared {
			credit, ok := prepareSendMemory(tx, preparedCreditByteCount)
			if !ok {
				t.Fatal("exact prepared DATA credit did not fit before admission")
			}
			preparedOwner = &fifoAdmissionPreparedOwner{credit: credit, terminal: freshAck}
			preparedShape := &SendPack{TransferOptions: TransferOptions{Ack: true}, Frame: freshFrame,
				MessageByteCount: ByteCount(len(freshFrame.MessageBytes)), Destination: fixture.receiver.ClientId(),
				Ctx: freshCtx, ackTarget: preparedOwner}
			if !sender.preparedPackFits(preparedShape) {
				t.Fatal("exact fresh DATA shape did not qualify for prepared-flight admission before the proof")
			}
		}
		startFresh := func() {
			freshStarted = true
			go func() {
				defer close(producerDone)
				options := []any{TransferKey{LogicalLane: 1}, Ctx(freshCtx)}
				callback := AckFunction(freshAck)
				if prepared {
					options = append(options, sendAckTargetOption{target: preparedOwner},
						sendPackRecoveryOption{upstreamRecoverable: true, retainAfterAckTimeout: true})
					callback = nil
				}
				accepted, err := fixture.sender.SendWithTimeoutDetailed(freshFrame, fixture.receiver.ClientId(), callback,
					time.Second, options...)
				if !accepted || err != nil {
					MessagePoolReturn(freshFrame.MessageBytes)
				}
				admission <- admissionResult{accepted: accepted, err: err}
			}()
		}
		var admitted admissionResult
		queuedDeadline := time.Now().Add(time.Second)
		admissionRead := false
		if queued {
			startFresh()
			admitted = <-admission
			admissionRead = true
			if !admitted.accepted || admitted.err != nil {
				t.Fatal("real queued-deadline owner was not admitted before the proof")
			}
			if prepared {
				if bytes, moved, closed := preparedOwner.snapshot(); bytes != preparedCreditByteCount || moved || closed || preparedOwner.handoff.transferred() {
					t.Fatal("pre-admitted prepared DATA did not retain its exact original credit before the proof")
				}
			}
		}
		fixture.forward(promoted, fixture.receiverIn)
		fixture.acknowledge()
		if !sender.ackWindow.PendingCumulativeProgress() {
			t.Fatal("genuine promoted-copy receiver ACK missed the blocked owner's coalescer")
		}
		close(release)
		synctest.Wait()
		if !done[1].Load() || done[2].Load() || done[3].Load() || sender.ctx.Err() != nil {
			t.Fatal("promoted-copy receipt did not settle only its own actual prefix")
		}
		if !queued {
			startFresh()
			synctest.Wait()
			select {
			case admitted = <-admission:
				admissionRead = true
				earlyAdmission = admitted.accepted && admitted.err == nil
			default:
			}
			if !earlyAdmission && (!sender.resendCapacityUnavailable.Load() || !sender.preparedFlightUnavailable.Load() ||
				sender.resendCapacityUnavailable.waiterCount.Load() != 1 || sender.resendMemoryGrant != 0) {
				t.Fatal("fresh ordinary owner did not wait at the real published gates without a TX grant")
			}
		}
		// An actual same-generation TCP ACK uses its NoBudget/immediate path
		// while old DATA still lacks terminal ACKs. It supplies no loss proof.
		protocolPacket := MessagePoolCopy(ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagAck, 9001, nil))
		protocolFrame := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider, Raw: true, MessageBytes: protocolPacket}
		var protocolTerminal atomic.Int64
		accepted, err := fixture.sender.SendWithTimeoutDetailed(protocolFrame, fixture.receiver.ClientId(), func(err error) {
			if err != nil {
				t.Errorf("real protocol owner failed: %v", err)
			}
			protocolTerminal.Add(1)
		}, time.Second, TransferKey{LogicalLane: 1}, NoBudget(), DeliverImmediately())
		if !accepted || err != nil {
			MessagePoolReturn(protocolPacket)
			t.Fatalf("fixed DATA cohort blocked actual NoBudget immediate protocol: %t %v", accepted, err)
		}
		protocolSeen, freshWritten := false, false
		for len(fixture.senderOut) != 0 {
			wire := fixture.take(fixture.senderOut)
			if wire.pack == nil || wire.pack.LogicalLane != 1 || !bytes.Equal(wire.pack.SequenceId, warm.frame.pack.SequenceId) {
				t.Fatal("public admission control changed its actual wire generation")
			}
			for _, frame := range wire.pack.Frames {
				if bytes.Equal(frame.MessageBytes, freshPacket) {
					freshWritten = true
				}
			}
			if protocolOnlyPack(wire.pack) {
				protocolSeen = wire.pack.DeliverImmediately
			}
			fixture.forward(wire, fixture.receiverIn)
		}
		if !protocolSeen || done[3].Load() {
			t.Fatal("protocol did not physically pass while the fixed DATA tail still lacked its ACK")
		}
		if mode == "cancel" && !admissionRead {
			cancelFresh()
			admitted = <-admission
			admissionRead = true
			if admitted.accepted || admitted.err == nil || terminalCount.Load() != 0 || sender.ctx.Err() != nil {
				t.Fatal("caller cancellation did not return its exact unwritten owner independently of the live sequence")
			}
		}
		if queued {
			if freshWritten {
				t.Fatal("pre-admitted fresh DATA escaped the fixed cohort before its caller deadline")
			}
			if prepared {
				if bytes, moved, closed := preparedOwner.snapshot(); bytes != preparedCreditByteCount || moved || closed || preparedOwner.handoff.transferred() {
					t.Fatal("prepared credit bypassed the fixed DATA serialization hold")
				}
			}
			// No cumulative tail ACK is released until this admitted owner's
			// own absolute caller deadline disposes it without a wire write.
			if mode == "prepared_cancel" && preparedOwner.handoff.cancelBeforeHandoff() {
				t.Fatal("queued prepared cancellation invented an already joined handoff")
			}
			err := <-terminal
			synctest.Wait()
			if err == nil || terminalCount.Load() != 1 || done[3].Load() || sender.ctx.Err() != nil ||
				mode != "prepared_cancel" && time.Now() != queuedDeadline.Add(time.Nanosecond) {
				t.Fatal("queued owner did not expire independently of the still-live fixed cohort")
			}
			if mode == "prepared_cancel" && !errors.Is(err, errPreparedSendCanceled) ||
				mode != "prepared_cancel" && !errors.Is(err, errSendPackExpiredUnwritten) {
				t.Fatal("queued disposal lost its exact unwritten cancellation/deadline disposition", err)
			}
			if prepared {
				if bytes, moved, closed := preparedOwner.snapshot(); bytes != 0 || moved || !closed || preparedOwner.handoff.transferred() || !preparedOwner.handoff.cancelBeforeHandoff() {
					t.Fatal("prepared queued disposal did not join and return credit exactly once before cohort retirement")
				}
			}
		}
		fixture.acknowledge()
		for index := 2; index < len(done); index++ {
			if !done[index].Load() {
				retry := pressureContractNaturalRetry(t, fixture, originals[index-1].frame.pack)
				fixture.forward(retry, fixture.receiverIn)
				fixture.acknowledge()
			}
		}
		synctest.Wait()
		if !admissionRead {
			admitted = <-admission
			admissionRead = true
		}
		if mode == "drain" && (!admitted.accepted || admitted.err != nil) {
			t.Fatal("real cumulative cohort retirement did not reopen the waiting ordinary owner")
		}
		for len(fixture.senderOut) != 0 {
			fixture.forward(fixture.take(fixture.senderOut), fixture.receiverIn)
			fixture.acknowledge()
		}
		synctest.Wait()
		if !queued && admitted.accepted && admitted.err == nil {
			if err := <-terminal; err != nil || terminalCount.Load() != 1 {
				t.Fatal("accepted fresh DATA did not receive its one real terminal ACK", err)
			}
		}
		if protocolTerminal.Load() != 1 || len(sender.sendItems) != 0 || root.UsedByteCount() != 0 ||
			sender.resendCapacityUnavailable.waiterCount.Load() != 0 || len(log.messages()) != 0 {
			t.Fatal("public fixed-cohort control left protocol, DATA, waiter or paid ownership after actual ACKs")
		}
		t.Logf("FIFO_ADMISSION_PUBLIC mode=%s old_data=3 protocol_acked=1 fresh_terminal=%d early_admission=%t same_generation=true custody_drained=true", mode, terminalCount.Load(), earlyAdmission)
	})
	if earlyAdmission {
		t.Error("FIFO_REPAIR_REFILL_PRECEDES_FIXED_COHORT_RETIREMENT: new ordinary DATA passed while the exact acknowledged old cohort remained live; actual ACK and cleanup completed")
	}
}
