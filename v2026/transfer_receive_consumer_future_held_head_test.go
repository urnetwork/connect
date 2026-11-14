// A replacement consumer may own a tentative future original after the source
// already received every earlier ACK. Its genuine promoted head must recover it.
package connect

import (
	"bytes"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
	"google.golang.org/protobuf/proto"
)

// Actual public sends supply sequence identity and proof. Empty opening
// controls are delivered normally; all captured application copies stay owned.
func writeConsumerFutureHeldData(t *testing.T, fixture *windowRoundFixture, lane uint32, payloadBytes int, lastApplication map[uint32]uint64) *windowRoundFrame {
	t.Helper()
	packet := ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagAck, 29, bytes.Repeat([]byte{0x7c}, payloadBytes))
	frame := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider, Raw: true, MessageBytes: packet}
	accepted, err := fixture.sender.SendWithTimeoutDetailed(frame, fixture.receiver.ClientId(), func(err error) {
		if err == nil {
			fixture.ackedCount++
		}
	}, time.Second, TransferKey{LogicalLane: lane})
	if !accepted || err != nil {
		MessagePoolReturn(frame.MessageBytes)
		t.Fatalf("CONSUMER_FUTURE_HELD_PUBLIC_SEND_FAILED lane=%d accepted=%t error=%v", lane, accepted, err)
	}
	for range cap(fixture.senderOut) {
		wire := fixture.take(fixture.senderOut)
		if wire.pack == nil {
			t.Fatal("CONSUMER_FUTURE_HELD_SENDER_BODY_MISSING")
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
		if len(wire.pack.Frames) != 0 {
			continue
		}
		fixture.forward(wire, fixture.receiverIn)
		fixture.acknowledge()
	}
	t.Fatal("CONSUMER_FUTURE_HELD_TYPED_SOURCE_NOT_PUBLISHED")
	return nil
}

// The configured leaf can fund the target's ordinary whole-prefix promise
// and default repair slot by itself. Four genuine foreign owners create the
// shortage, while a bounded same-parent child still has physical capacity.
func TestReceiveConsumerFutureHeldHeadUsesPaidChild(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		const foreignOwnerBytes, tailBytes = ByteCount(5120), ByteCount(1536)
		const foreignCount = 4
		const foreignBytes = foreignCount * foreignOwnerBytes
		const rxBytes = foreignBytes + tailBytes
		root := NewTransferMemoryBudget(2 * rxBytes)
		rx := NewTransferMemoryBudgetWithParent(rxBytes, root)
		recovery := NewTransferMemoryBudgetWithParent(rxBytes, root)
		t.Cleanup(func() {
			for _, budget := range []*TransferMemoryBudget{rx, recovery, root} {
				assertRetainedBudgetBalance(t, budget)
				if budget.Stats().QuotaByteCount != 0 {
					t.Error("consumer future head retained logical quota after shutdown")
				}
			}
		})
		fixture, log := newConsumerTentativeHeadFixture(t, rx, recovery)
		repairBytes := fixture.receiver.settings.ReceiveBufferSettings.ReceiveHeadContractRepairByteCount
		if repairBytes != DefaultReceiveBufferSettings().ReceiveHeadContractRepairByteCount ||
			rx.TotalByteCount() < repairBytes+3*tailBytes {
			t.Fatal("CONSUMER_FUTURE_HELD_DEFAULT_REPAIR_CAPACITY_PREMISE_FAILED")
		}
		lastApplication := map[uint32]uint64{}
		foreignHead := writeConsumerFutureHeldData(t, fixture, 1, 1280, lastApplication)
		var foreignTails []*windowRoundFrame
		for range foreignCount {
			foreignTails = append(foreignTails, writeConsumerFutureHeldData(t, fixture, 1, 1280, lastApplication))
		}
		predecessor := writeConsumerFutureHeldData(t, fixture, 0, 32, lastApplication)
		tail := writeConsumerFutureHeldData(t, fixture, 0, 32, lastApplication)
		if predecessor.pack.SequenceNumber != 1 || tail.pack.SequenceNumber != 2 || tail.pack.Head ||
			tail.pack.ContractFrame != nil || tail.pack.ContractId != nil || tail.pack.LogicalLane != 0 ||
			!bytes.Equal(tail.pack.SequenceId, predecessor.pack.SequenceId) || bytes.Equal(tail.pack.SequenceId, foreignHead.pack.SequenceId) ||
			ByteCount(tail.pack.SequenceNumber)*tailBytes+tailBytes+repairBytes > rx.TotalByteCount() ||
			rx.UsedByteCount() != 0 || fixture.deliveredCount != 0 || fixture.ackedCount != 0 {
			t.Fatal("CONSUMER_FUTURE_HELD_REAL_PREFIX_PREMISE_FAILED")
		}
		findSequence := func(frame *windowRoundFrame) *ReceiveSequence {
			buffer := fixture.receiver.receiveBuffer
			buffer.mutex.Lock()
			defer buffer.mutex.Unlock()
			for id, sequence := range buffer.receiveSequences {
				if id.SequenceId == RequireIdFromBytes(frame.pack.SequenceId) && id.LogicalLane == frame.pack.LogicalLane {
					return sequence
				}
			}
			t.Fatal("CONSUMER_FUTURE_HELD_REAL_RECEIVE_SEQUENCE_MISSING")
			return nil
		}
		foreign, old := findSequence(foreignHead), findSequence(predecessor)
		fixture.forward(predecessor, fixture.receiverIn)
		fixture.acknowledge()
		if old.receiveContract == nil || old.nextSequenceNumber != tail.pack.SequenceNumber ||
			fixture.deliveredCount != 1 || fixture.ackedCount != 1 {
			t.Fatal("CONSUMER_FUTURE_HELD_SOURCE_PREFIX_NOT_ACKNOWLEDGED")
		}
		old.Cancel()
		<-old.done
		synctest.Wait()
		var sender *SendSequence
		func() {
			buffer := fixture.sender.sendBuffer
			buffer.mutex.Lock()
			defer buffer.mutex.Unlock()
			for id, sequence := range buffer.sendSequences {
				if id.LogicalLane == tail.pack.LogicalLane && sequence.sequenceId == RequireIdFromBytes(tail.pack.SequenceId) {
					sender = sequence
					return
				}
			}
		}()
		if sender == nil || len(sender.sendItems) != 1 || sender.sendItems[0].sequenceNumber != tail.pack.SequenceNumber ||
			sender.sendItems[0].messageId != RequireIdFromBytes(tail.pack.MessageId) || foreign.ctx.Err() != nil {
			t.Fatal("CONSUMER_FUTURE_HELD_SOURCE_RETRY_OR_FOREIGN_LIFETIME_CHANGED")
		}
		var foreignItems []*receiveItem
		for index, frame := range foreignTails {
			if frame.pack.Head || frame.pack.ContractFrame != nil || !bytes.Equal(frame.pack.SequenceId, foreignHead.pack.SequenceId) {
				t.Fatal("CONSUMER_FUTURE_HELD_FOREIGN_ORIGINAL_PREMISE_FAILED")
			}
			fixture.forward(frame, fixture.receiverIn)
			item := foreign.receiveQueue.GetByMessageId(RequireIdFromBytes(frame.pack.MessageId))
			if item == nil || item.committed || item.memoryBudget != rx || item.QueueByteCount() != foreignOwnerBytes ||
				foreign.holdPromise.set || foreign.heldHeadRepair.eligibleCount != 0 ||
				rx.UsedByteCount() != ByteCount(index+1)*foreignOwnerBytes || len(fixture.receiverOut) != 0 {
				t.Fatal("CONSUMER_FUTURE_HELD_FOREIGN_CUSTODY_PREMISE_FAILED")
			}
			foreignItems = append(foreignItems, item)
		}
		fixture.forward(tail, fixture.receiverIn)
		sequence := findSequence(tail)
		held := sequence.receiveQueue.GetByMessageId(RequireIdFromBytes(tail.pack.MessageId))
		if sequence == old || held == nil || held.committed || !held.ack || held.head || held.memoryBudget != rx ||
			held.QueueByteCount() != tailBytes || held.decodedOwner == nil || !held.decodedOwner.receivePack.receiveMemoryOwned ||
			held.decodedOwner.receivePack.ingressSequence != sequence || len(held.frames) != 1 ||
			held.frames[0].MessageType != protocol.MessageType_IpIpPacketFromProvider || !held.frames[0].Raw ||
			sequence.nextSequenceNumber != 0 || sequence.receiveContract != nil || len(sequence.openReceiveContracts) != 0 ||
			sequence.holdPromise.set || sequence.holdPromise.ingressFloor != 0 || sequence.holdPromise.reservedByteCount != 0 ||
			sequence.heldHeadRepair.eligibleCount != 0 || sequence.heldHeadRepair.reservedByteCount != 0 || sequence.heldHeadRepair.owner != nil ||
			sequence.memoryPressure.activeSet || rx.UsedByteCount() != rxBytes || root.UsedByteCount() != rxBytes || recovery.UsedByteCount() != 0 ||
			len(fixture.receiverOut) != 0 || fixture.deliveredCount != 1 || fixture.ackedCount != 1 {
			t.Fatal("CONSUMER_FUTURE_HELD_TENTATIVE_REPLACEMENT_PREMISE_FAILED")
		}
		originalOwner, originalTag, originalReceipt := held.decodedOwner, held.receiveTag(), held.receiveTime
		originalPayload := append([]byte(nil), held.frames[0].MessageBytes...)
		arrival := pressureContractNaturalRetry(t, fixture, tail.pack)
		if !arrival.pack.Head || arrival.pack.ContractFrame == nil || arrival.pack.ContractId != nil ||
			arrival.pack.SequenceNumber <= sequence.holdPromise.ingressFloor || arrival.pack.SequenceNumber != held.sequenceNumber ||
			len(arrival.pack.Frames) != 1 || !proto.Equal(arrival.pack.Frames[0], tail.pack.Frames[0]) || len(sender.sendItems) != 1 ||
			sender.sendItems[0].messageId != held.messageId {
			t.Fatal("CONSUMER_FUTURE_HELD_GENUINE_FULL_RETRY_PREMISE_FAILED")
		}
		path, err := FilteredTransferPath(arrival.bytes)
		if err != nil {
			t.Fatal(err)
		}
		scan, status := scanReceiveMemoryPack(arrival.bytes, path)
		if status != receiveMemoryPackScanReady || !scan.loanSafe || scan.nack || scan.contractAhead || scan.hasEncryptedControl ||
			scan.frameCount != 1 || scan.consumerFrameCount != 1 || scan.messageId != held.messageId || scan.sequenceId != sequence.sequenceId ||
			scan.retainedByteCount <= 0 || scan.retainedByteCount > recovery.TotalByteCount() || scan.retainedByteCount > root.Available() ||
			path.SourceId != fixture.sender.ClientId() || path.DestinationId != fixture.receiver.ClientId() {
			t.Fatal("CONSUMER_FUTURE_HELD_FUNDED_RETRY_PREMISE_FAILED")
		}
		minimumCopyBytes := decodedPackOwnerQueueByteCount + receiveMemoryScanCopyRoot(len(arrival.pack.ContractFrame.MessageBytes))
		observed, paid, originalDelivered := false, false, false
		sequence.receiveBufferSettings.receiveOwnerStageForTest = func(actual *ReceiveSequence, owner *decodedPackOwner, stage string) {
			if actual == sequence && owner != nil && stage == "pack_released" && owner.receiveItem.messageId == held.messageId {
				observed = true
				cost := owner.receiveItem.QueueByteCount()
				paid = owner != originalOwner && owner.receiveItem.memoryBudget == recovery && minimumCopyBytes <= cost &&
					cost <= scan.retainedByteCount && owner.receivePack.ingressLoanByteCount == 0 && recovery.UsedByteCount() == cost &&
					rx.UsedByteCount() == rxBytes && root.UsedByteCount() == rxBytes+cost && root.UsedByteCount() <= root.TotalByteCount() &&
					held.decodedOwner == originalOwner && held.memoryBudget == rx && !held.committed &&
					!sequence.holdPromise.set && sequence.holdPromise.reservedByteCount == 0 &&
					sequence.heldHeadRepair.eligibleCount == 0 && sequence.heldHeadRepair.reservedByteCount == 0 && !sequence.memoryPressure.activeSet
			}
		}
		fixture.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, peer Peer) {
			if peer.TransferKey.LogicalLane == tail.pack.LogicalLane {
				originalDelivered = held.decodedOwner == originalOwner && held.receiveTag() == originalTag && held.receiveTime == originalReceipt &&
					len(frames) == 1 && bytes.Equal(frames[0].MessageBytes, originalPayload)
			}
		})
		fixture.forward(arrival, fixture.receiverIn)
		if !observed || !paid {
			t.Fatalf("CONSUMER_FUTURE_HELD_PROOF_CHILD_UNUSED observed=%t paid=%t delivered=%d acked=%d next=%d future=%d rx=%d child=%d root=%d",
				observed, paid, fixture.deliveredCount, fixture.ackedCount, sequence.nextSequenceNumber, tail.pack.SequenceNumber,
				rx.UsedByteCount(), recovery.UsedByteCount(), root.UsedByteCount())
		}
		fixture.acknowledge()
		if !originalDelivered || fixture.deliveredCount != 2 || fixture.ackedCount != 2 || len(sender.sendItems) != 0 ||
			sequence.nextSequenceNumber != tail.pack.SequenceNumber+1 || sequence.receiveQueue.Len() != 0 ||
			sequence.receiveContract == nil || sequence.ctx.Err() != nil || len(log.messages()) != 0 {
			t.Fatal("CONSUMER_FUTURE_HELD_PROMOTION_LOST_ORIGINAL_OR_PROGRESS")
		}
		for _, item := range foreignItems {
			if foreign.receiveQueue.GetByMessageId(item.messageId) != item || item.committed ||
				item.memoryBudget != rx || item.QueueByteCount() != foreignOwnerBytes {
				t.Fatal("CONSUMER_FUTURE_HELD_PROMOTION_CHANGED_FOREIGN_OWNER")
			}
		}
		if foreign.ctx.Err() != nil || foreign.holdPromise.set || foreign.receiveQueue.Len() != foreignCount ||
			rx.UsedByteCount() != foreignBytes || root.UsedByteCount() != foreignBytes || recovery.UsedByteCount() != 0 {
			t.Fatal("CONSUMER_FUTURE_HELD_PROMOTION_LEAKED_OR_CHANGED_FOREIGN_CUSTODY")
		}
	})
}
