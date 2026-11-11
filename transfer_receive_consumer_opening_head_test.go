// A cold consumer lane can retain paid application data while its genuine
// opening contract is missing. Empty control must bypass DATA byte pressure.
package connect

import (
	"bytes"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// The sender's actual empty opening is held off the wire until a foreign lane
// and this lane's own typed tentative tail fill RX. No contract is preinstalled.
func testReceiveConsumerOpeningHead(t *testing.T, badMac bool) {
	t.Helper()
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		const foreignBytes, tailBytes = ByteCount(5120), ByteCount(1536)
		const rxBytes = foreignBytes + tailBytes
		root := NewTransferMemoryBudget(2 * rxBytes)
		rx := NewTransferMemoryBudgetWithParent(rxBytes, root)
		recovery := NewTransferMemoryBudgetWithParent(rxBytes, root)
		t.Cleanup(func() {
			for _, budget := range []*TransferMemoryBudget{rx, recovery, root} {
				assertRetainedBudgetBalance(t, budget)
				if budget.Stats().QuotaByteCount != 0 {
					t.Error("consumer opening retained logical quota after shutdown")
				}
			}
		})
		fixture, log := newConsumerTentativeHeadFixture(t, rx, recovery)
		var opening *windowRoundFrame
		lastApplication := map[uint32]uint64{}
		write := func(lane uint32, payloadBytes int) *windowRoundFrame {
			packet := ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagAck, 23, bytes.Repeat([]byte{0x6b}, payloadBytes))
			frame := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider, Raw: true, MessageBytes: packet}
			accepted, err := fixture.sender.SendWithTimeoutDetailed(frame, fixture.receiver.ClientId(), func(err error) {
				if err == nil {
					fixture.ackedCount++
				}
			}, time.Second, TransferKey{LogicalLane: lane})
			if !accepted || err != nil {
				MessagePoolReturn(frame.MessageBytes)
				t.Fatalf("CONSUMER_OPENING_PUBLIC_SEND_FAILED lane=%d accepted=%t error=%v", lane, accepted, err)
			}
			for range cap(fixture.senderOut) {
				wire := fixture.take(fixture.senderOut)
				if wire.pack == nil {
					t.Fatal("CONSUMER_OPENING_SENDER_BODY_MISSING")
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
				if lane == 0 {
					// Keep every target control copy off the receiver. The
					// first actual opening supplies identity and proof below.
					if opening == nil && wire.pack.Head && wire.pack.SequenceNumber == 0 &&
						wire.pack.ContractFrame != nil && !wire.pack.ContractAhead {
						opening = wire
					}
					continue
				}
				fixture.forward(wire, fixture.receiverIn)
				fixture.acknowledge()
			}
			t.Fatal("CONSUMER_OPENING_TYPED_SOURCE_NOT_PUBLISHED")
			return nil
		}
		foreignHead := write(1, 1280)
		foreignTail := write(1, 1280)
		findSequence := func(frame *windowRoundFrame) *ReceiveSequence {
			buffer := fixture.receiver.receiveBuffer
			buffer.mutex.Lock()
			defer buffer.mutex.Unlock()
			for id, sequence := range buffer.receiveSequences {
				if id.SequenceId == RequireIdFromBytes(frame.pack.SequenceId) && id.LogicalLane == frame.pack.LogicalLane {
					return sequence
				}
			}
			t.Fatal("CONSUMER_OPENING_REAL_RECEIVE_SEQUENCE_MISSING")
			return nil
		}
		foreign := findSequence(foreignHead)
		if foreignHead.pack.LogicalLane != 1 || foreignTail.pack.Head || foreignTail.pack.ContractFrame != nil ||
			!bytes.Equal(foreignTail.pack.SequenceId, foreignHead.pack.SequenceId) || foreign.receiveContract == nil ||
			foreign.nextSequenceNumber != foreignHead.pack.SequenceNumber || rx.UsedByteCount() != 0 {
			t.Fatal("CONSUMER_OPENING_FOREIGN_LANE_PREMISE_FAILED")
		}
		fixture.forward(foreignTail, fixture.receiverIn)
		foreignItem := foreign.receiveQueue.GetByMessageId(RequireIdFromBytes(foreignTail.pack.MessageId))
		if foreignItem == nil || foreignItem.committed || foreignItem.memoryBudget != rx || foreignItem.QueueByteCount() != foreignBytes ||
			rx.UsedByteCount() != foreignBytes || foreign.holdPromise.set || len(fixture.receiverOut) != 0 {
			t.Fatal("CONSUMER_OPENING_FOREIGN_CUSTODY_PREMISE_FAILED")
		}
		tail := write(0, 32)
		if opening == nil || !opening.pack.Head || opening.pack.Nack || opening.pack.ContractAhead || opening.pack.SequenceNumber != 0 ||
			len(opening.pack.Frames) != 0 || opening.pack.ContractFrame == nil || opening.pack.ContractId != nil ||
			opening.pack.LogicalLane != 0 || tail.pack.LogicalLane != 0 || tail.pack.Head || tail.pack.ContractFrame != nil ||
			tail.pack.SequenceNumber != 1 || !bytes.Equal(tail.pack.SequenceId, opening.pack.SequenceId) ||
			bytes.Equal(tail.pack.SequenceId, foreignHead.pack.SequenceId) || fixture.deliveredCount != 0 || fixture.ackedCount != 0 {
			t.Fatal("CONSUMER_OPENING_GENUINE_EMPTY_CONTROL_PREMISE_FAILED")
		}
		fixture.forward(tail, fixture.receiverIn)
		sequence := findSequence(tail)
		tailItem := sequence.receiveQueue.GetByMessageId(RequireIdFromBytes(tail.pack.MessageId))
		if tailItem == nil || tailItem.committed || tailItem.memoryBudget != rx || tailItem.QueueByteCount() != tailBytes ||
			len(tailItem.frames) != 1 || tailItem.frames[0].MessageType != protocol.MessageType_IpIpPacketFromProvider ||
			!tailItem.frames[0].Raw || !tailItem.ack || tailItem.decodedOwner == nil ||
			tailItem.decodedOwner.receivePack.ingressSequence != sequence || !tailItem.decodedOwner.receivePack.receiveMemoryOwned ||
			sequence.receiveContract != nil || len(sequence.openReceiveContracts) != 0 || sequence.nextSequenceNumber != 0 ||
			sequence.holdPromise.ingressFloor != 0 || sequence.holdPromise.set || sequence.holdPromise.reservedByteCount != 0 ||
			sequence.memoryPressure.activeSet || rx.UsedByteCount() != rxBytes || root.UsedByteCount() != rxBytes || recovery.UsedByteCount() != 0 ||
			len(fixture.receiverOut) != 0 || fixture.deliveredCount != 0 || fixture.ackedCount != 0 ||
			fixture.receiver.ContractManager().ReceiveNoContract(fixture.sender.ClientId()) {
			t.Fatal("CONSUMER_OPENING_COLD_FULL_RX_PREMISE_FAILED")
		}
		if badMac {
			pack := proto.Clone(opening.pack).(*protocol.Pack)
			var contract protocol.Contract
			if err := proto.Unmarshal(pack.ContractFrame.MessageBytes, &contract); err != nil || len(contract.StoredContractHmac) == 0 {
				t.Fatal("CONSUMER_OPENING_SIGNED_CONTROL_MALFORMED")
			}
			contract.StoredContractHmac[0] ^= 0x80
			var err error
			pack.ContractFrame.MessageBytes, err = proto.Marshal(&contract)
			if err != nil {
				t.Fatal(err)
			}
			rewriteHeldHeadContractWire(t, opening, pack)
		}
		path, err := FilteredTransferPath(opening.bytes)
		if err != nil {
			t.Fatal(err)
		}
		scan, status := scanReceiveMemoryPack(opening.bytes, path)
		if status != receiveMemoryPackScanReady || !scan.loanSafe || !scan.head || scan.nack || scan.contractAhead ||
			scan.hasEncryptedControl || !scan.contractFrameSet || scan.frameCount != 0 || scan.consumerFrameCount != 0 ||
			scan.sequenceNumber != 0 || scan.sequenceId != sequence.sequenceId || scan.retainedByteCount <= 0 ||
			scan.retainedByteCount > recovery.TotalByteCount() || scan.retainedByteCount > root.Available() ||
			path.SourceId != fixture.sender.ClientId() || path.DestinationId != fixture.receiver.ClientId() {
			t.Fatal("CONSUMER_OPENING_FUNDED_EXACT_FLOOR_PREMISE_FAILED")
		}
		var ackUpdates atomic.Int32
		sequence.ackWindow.afterUpdateForTest = func() { ackUpdates.Add(1) }
		observed, paid := false, false
		sequence.receiveBufferSettings.receiveOwnerStageForTest = func(actual *ReceiveSequence, owner *decodedPackOwner, stage string) {
			if actual == sequence && owner != nil && stage == "pack_released" && bytes.Equal(owner.pack.MessageId, scan.messageId[:]) {
				observed = true
				paid = owner.receiveItem.memoryBudget == nil && owner.receiveItem.QueueByteCount() == 0 &&
					!owner.receivePack.receiveMemoryOwned && owner.receivePack.ingressSequence == nil &&
					owner.receivePack.ingressLoanByteCount == 0 && recovery.UsedByteCount() == 0 &&
					rx.UsedByteCount() == rxBytes && root.UsedByteCount() == rxBytes &&
					root.UsedByteCount() <= root.TotalByteCount() && !sequence.holdPromise.set &&
					sequence.holdPromise.reservedByteCount == 0 && !sequence.memoryPressure.activeSet
			}
		}
		fixture.forward(opening, fixture.receiverIn)
		if !observed || !paid {
			t.Fatalf("CONSUMER_OPENING_HEAD_CHILD_UNUSED bad_mac=%t observed=%t paid=%t delivered=%d acked=%d next=%d rx=%d child=%d root=%d",
				badMac, observed, paid, fixture.deliveredCount, fixture.ackedCount, sequence.nextSequenceNumber,
				rx.UsedByteCount(), recovery.UsedByteCount(), root.UsedByteCount())
		}
		if badMac {
			if sequence.ctx.Err() == nil || !sequence.rejectRetransmits || sequence.receiveContract != nil ||
				len(sequence.openReceiveContracts) != 0 || sequence.nextSequenceNumber != 0 ||
				fixture.deliveredCount != 0 || fixture.ackedCount != 0 || ackUpdates.Load() != 0 || len(log.messages()) == 0 {
				t.Fatal("CONSUMER_OPENING_BAD_MAC_BYPASSED_ORDINARY_VERIFIER")
			}
		} else {
			fixture.acknowledge()
			if fixture.deliveredCount != 1 || fixture.ackedCount != 1 || sequence.nextSequenceNumber != tail.pack.SequenceNumber+1 ||
				sequence.receiveContract == nil || !sequence.receiveContract.verifiedReceiveProofSet ||
				sequence.receiveQueue.Len() != 0 || sequence.ctx.Err() != nil || len(log.messages()) != 0 {
				t.Fatal("CONSUMER_OPENING_HEAD_DID_NOT_DRAIN_EXACT_PREFIX")
			}
		}
		if foreign.ctx.Err() != nil || foreign.receiveQueue.GetByMessageId(foreignItem.messageId) != foreignItem || foreignItem.committed ||
			foreignItem.memoryBudget != rx || foreignItem.QueueByteCount() != foreignBytes || foreign.holdPromise.set ||
			rx.UsedByteCount() != foreignBytes || root.UsedByteCount() != foreignBytes || recovery.UsedByteCount() != 0 {
			t.Fatal("CONSUMER_OPENING_CHANGED_FOREIGN_CUSTODY_OR_LEAKED_CHILD")
		}
	})
}

// Empty opening control derives its consumer role from the same sequence's
// actual paid later FromProvider data, then uses ordinary contract verification.
func TestReceiveProtocolOpeningContractAtFullDataBudget(t *testing.T) {
	testReceiveConsumerOpeningHead(t, false)
}

// A paid control still has no proof authority. A changed MAC closes only its
// own cold lane without acknowledging or delivering the retained application.
func TestReceiveProtocolOpeningContractBadMacAtFullDataBudget(t *testing.T) {
	testReceiveConsumerOpeningHead(t, true)
}
