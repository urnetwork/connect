// Empty ordered protocol originals need bounded custody after their raw
// handoff drains. These tests use a verified public signed generation and
// actual receive wire, without inventing sender receipts for crafted Packs.
package connect

import (
	"bytes"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
	"google.golang.org/protobuf/proto"
)

func TestProtocolEmptyFutureHoldKeepsConfiguredCount(t *testing.T) {
	testProtocolFutureMetadataBound(t, false, false)
}

func TestProtocolContractOnlyFutureHoldKeepsConfiguredCount(t *testing.T) {
	testProtocolFutureMetadataBound(t, true, false)
}

func TestProtocolFutureHoldCancellationPreservesHealthySignedLane(t *testing.T) {
	testProtocolFutureMetadataBound(t, true, true)
}

// Every attempt is distinct wire identity in the same verified generation.
// The later receive count is tested after each mailbox has actually drained.
func testProtocolFutureMetadataBound(t *testing.T, withProof, cancelHeld bool) {
	t.Helper()
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		const lane = uint32(1)
		const limit = 4
		const offered = limit + 3
		const leaf = ByteCount(128 * 1024)
		root := NewTransferMemoryBudget(4 * leaf)
		rx := NewTransferMemoryBudgetWithParent(leaf, root)
		head := NewTransferMemoryBudgetWithParent(leaf, root)
		pack := NewTransferMemoryBudgetWithParent(leaf, root)
		t.Cleanup(func() {
			for _, budget := range []*TransferMemoryBudget{pack, head, rx, root} {
				assertRetainedBudgetBalance(t, budget)
				if budget.Stats().QuotaByteCount != 0 {
					t.Error("protocol hold retained quota after actual Client joins")
				}
			}
		})
		layout := consumerPaidPackByteLayout{label: "signed-empty-future-count", byteLimit: leaf,
			leafBytes: leaf, rootBytes: 4 * leaf, countLimit: limit * maxLogicalDataLaneCount}
		fixture, _, _, _ := newConsumerPaidPackByteFixture(t, layout, rx, head, pack, nil)
		started := time.Now()
		var receipts [3]atomic.Bool
		var receiptTimes [3]atomic.Int64
		warm := consumerPaidPackByteSend(t, fixture, lane, 1, 0, &receipts[0], &receiptTimes[0], started)
		fixture.forward(warm.frame, fixture.receiverIn)
		warmAck := fixture.take(fixture.receiverOut)
		if warmAck.ack == nil || warmAck.ack.Selective || !bytes.Equal(warmAck.ack.MessageId, warm.frame.pack.MessageId) {
			t.Fatal("actual signed warm DATA did not produce its cumulative ACK")
		}
		// Keeping the real warm ACK at this point produces a non-Head missing
		// DATA original. No synthetic Head bit resets the receive floor.
		missing := consumerPaidPackByteSend(t, fixture, lane, 1, 1, &receipts[1], &receiptTimes[1], started)
		sequence := consumerForeignHeadSequence(t, fixture, missing.frame)
		fixture.forward(warmAck, fixture.senderIn)
		base := missing.frame.pack.SequenceNumber
		if missing.frame.pack.Head || sequence.nextSequenceNumber != base || cap(sequence.protocolPacks) != limit ||
			sequence.receiveContract == nil || !sequence.receiveContract.verifiedReceiveProofSet ||
			fixture.receiver.ContractManager().ReceiveNoContract(fixture.sender.ClientId()) ||
			sequence.receiveQueue.Len() != 0 || root.UsedByteCount() != 0 || !receipts[0].Load() {
			t.Fatal("signed source did not establish the exact missing DATA floor and configured four protocol slots")
		}
		var template protocol.TransferFrame
		if err := ProtoUnmarshal(missing.frame.bytes, &template); err != nil || template.Pack == nil {
			t.Fatalf("decode captured source identity: %v", err)
		}
		var proof *protocol.Frame
		for _, wire := range fixture.heldFrames {
			if wire.pack != nil && bytes.Equal(wire.pack.SequenceId, missing.frame.pack.SequenceId) &&
				wire.pack.ContractFrame != nil && wire.pack.ContractFrame.MessageType == protocol.MessageType_TransferContract {
				proof = proto.Clone(wire.pack.ContractFrame).(*protocol.Frame)
				break
			}
		}
		if proof == nil {
			t.Fatal("actual signed opening has no captured proof")
		}
		// The proof is the real OOB-issued and worker-verified opening. Pack
		// metadata IDs are intentionally new, as a peer may legally emit them.
		makeWire := func(number uint64, id Id) *windowRoundFrame {
			message := proto.Clone(&template).(*protocol.TransferFrame)
			message.Pack = proto.Clone(template.Pack).(*protocol.Pack)
			message.Pack.MessageId = id.Bytes()
			message.Pack.SequenceNumber = number
			message.Pack.Frames = nil
			message.Pack.Head = false
			message.Pack.Nack = false
			message.Pack.DeliverImmediately = false
			message.Pack.ContractAhead = false
			message.Pack.ContractId = nil
			message.Pack.ContractFrame = nil
			if withProof {
				message.Pack.ContractFrame = proto.Clone(proof).(*protocol.Frame)
			}
			wireBytes, err := ProtoMarshal(message)
			if err != nil {
				t.Fatal(err)
			}
			wire := &windowRoundFrame{bytes: wireBytes, pack: message.Pack}
			fixture.heldFrames = append(fixture.heldFrames, wire)
			path, err := FilteredTransferPath(wireBytes)
			if err != nil {
				t.Fatal(err)
			}
			scan, status := scanReceiveMemoryPack(wireBytes, path)
			if status != receiveMemoryPackScanReady || scan.frameCount != 0 || scan.protocolFrameCount != 0 ||
				scan.nack || scan.head || scan.contractFrameSet != withProof || !protocolOnlyPack(message.Pack) {
				t.Fatal("crafted ordered metadata lost its independently classified protocol shape")
			}
			return wire
		}
		var ids [offered]Id
		var owners [offered]*receiveItem
		var beforeReceipts [offered]int
		// Synthetic future ACKs are inspected and retained by this fixture;
		// only the real public missing DATA ACK is delivered to its sender.
		drainAcks := func(beforeMissing bool) {
			synctest.Wait()
			for len(fixture.receiverOut) != 0 {
				wire := fixture.take(fixture.receiverOut)
				ack := wire.ack
				if ack == nil || len(ack.EvictedSequenceNumbers) != 0 || !bytes.Equal(ack.SequenceId, missing.frame.pack.SequenceId) {
					t.Fatal("protocol metadata queue produced an unrelated or withdrawn receipt")
				}
				if bytes.Equal(ack.MessageId, missing.frame.pack.MessageId) {
					if beforeMissing || ack.Selective {
						t.Fatal("absent DATA acquired false delivery authority")
					}
					fixture.forward(wire, fixture.senderIn)
					continue
				}
				if beforeMissing {
					matched := false
					for index, id := range ids {
						if id != (Id{}) && bytes.Equal(ack.MessageId, id.Bytes()) {
							matched = true
							owner := sequence.receiveQueue.GetByMessageId(id)
							if !ack.Selective || owner == nil || !owner.committed {
								t.Fatal("unretained metadata acquired a false selective receipt")
							}
							beforeReceipts[index]++
						}
					}
					if !matched {
						t.Fatal("missing DATA gap produced a cumulative or unknown protocol ACK")
					}
				}
			}
		}
		beforeDrops := fixture.receiver.ReceiveStats().ReceiveQueueDropCount
		for index := range offered {
			ids[index] = NewId()
			// Leave one extra gap after the real DATA. It lets that DATA's
			// actual ACK complete before the crafted metadata drain.
			wire := makeWire(base+2+uint64(index), ids[index])
			fixture.forward(wire, fixture.receiverIn)
			drainAcks(true)
			owners[index] = sequence.receiveQueue.GetByMessageId(ids[index])
			if len(sequence.protocolPacks) != 0 || sequence.nextSequenceNumber != base || receipts[1].Load() ||
				sequence.immediateHistory != nil || sequence.immediateDeliveryQueue != nil {
				t.Fatal("protocol count cut did not drain raw custody behind the unchanged missing DATA")
			}
			if owners[index] != nil && (!owners[index].noBudget || owners[index].memoryBudget != nil ||
				owners[index].MessageByteCount() != 0 || owners[index].QueueByteCount() != 0 || owners[index].committed) {
				t.Fatal("empty protocol owner unexpectedly consumed DATA permission or committed unpaid custody")
			}
		}
		held := sequence.receiveQueue.Len()
		overflow := held > limit
		if held != limit && held != offered || root.UsedByteCount() != 0 || rx.UsedByteCount() != 0 ||
			head.UsedByteCount() != 0 || pack.UsedByteCount() != 0 || pack.Stats().QuotaByteCount != 0 {
			t.Fatal("protocol hold cut lost its exact count-only retention premise")
		}
		if !overflow && fixture.receiver.ReceiveStats().ReceiveQueueDropCount != beforeDrops+offered-limit {
			t.Fatal("excess distinct future metadata did not produce exact truthful refusals")
		}
		for index := range offered {
			if index < limit && owners[index] == nil || !overflow && index >= limit && (owners[index] != nil || beforeReceipts[index] != 0) {
				t.Fatal("protocol count refusal discarded an earlier owner or acknowledged an excess identity")
			}
		}
		// At the bound, replay is still the same owner, not another metadata
		// slot. Every original pointer survives the independently decoded copy.
		fixture.forward(makeWire(base+2, ids[0]), fixture.receiverIn)
		drainAcks(true)
		if sequence.receiveQueue.Len() != held || sequence.receiveQueue.GetByMessageId(ids[0]) != owners[0] {
			t.Fatal("duplicate future protocol replaced or multiplied retained custody")
		}
		if cancelHeld {
			sequence.Cancel()
			<-sequence.done
			if root.UsedByteCount() != 0 || sequence.receiveQueue.Len() != 0 || fixture.ctx.Err() != nil || receipts[1].Load() {
				t.Fatal("protocol generation cancellation leaked ownership or acknowledged absent DATA")
			}
			healthy := consumerPaidPackByteSend(t, fixture, lane+1, 1, 2, &receipts[2], &receiptTimes[2], started)
			fixture.forward(healthy.frame, fixture.receiverIn)
			fixture.acknowledge()
			if !receipts[2].Load() || root.UsedByteCount() != 0 {
				t.Fatal("bounded protocol cancellation blocked the actual healthy signed sibling")
			}
		} else {
			fixture.forward(missing.frame, fixture.receiverIn)
			drainAcks(false)
			if !receipts[1].Load() || sequence.nextSequenceNumber != base+1 || sequence.receiveQueue.Len() != held ||
				fixture.deliveredCount != 2 {
				t.Fatal("actual current DATA failed to deliver and ACK through full protocol custody")
			}
			fixture.forward(makeWire(base+1, NewId()), fixture.receiverIn)
			drainAcks(false)
			if sequence.receiveQueue.Len() != 0 || sequence.nextSequenceNumber != base+2+uint64(held) {
				t.Fatal("actual protocol prefix did not drain every retained metadata owner")
			}
			for index := held; index < offered; index++ {
				fixture.forward(makeWire(base+2+uint64(index), ids[index]), fixture.receiverIn)
				drainAcks(false)
				if sequence.nextSequenceNumber != base+3+uint64(index) || sequence.receiveQueue.Len() != 0 {
					t.Fatal("identical refused metadata could not retry as the actual current owner")
				}
			}
		}
		if time.Since(started) != 0 || root.UsedByteCount() != 0 {
			t.Fatal("metadata cardinality proof used a deadline or retained DATA permission")
		}
		t.Logf("signed ordered metadata: proof=%t cancel=%t protocol_mailbox=%d offered=%d retained=%d committed=0 data_charge=%d elapsed=%s", withProof, cancelHeld, limit, offered, held, root.UsedByteCount(), time.Since(started))
		if overflow {
			t.Errorf("ordered protocol hold exceeded configured count after raw mailbox drained: retained=%d limit=%d", held, limit)
		}
	})
}
