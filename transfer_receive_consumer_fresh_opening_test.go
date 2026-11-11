// An empty protocol bootstrap needs no DATA permission. Its following real
// DATA and an explicitly formatted full-proof DATA head still need paid storage.
package connect

import (
	"bytes"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

type freshConsumerOpeningResult struct {
	blocked bool
	cost    ByteCount
	drops   uint64
}

func freshConsumerOpeningClone(f *windowRoundFixture, original *windowRoundFrame) *windowRoundFrame {
	MessagePoolShareReadOnly(original.bytes)
	copy := &windowRoundFrame{bytes: original.bytes, pack: original.pack}
	f.heldFrames = append(f.heldFrames, copy)
	return copy
}

func freshConsumerOpeningSequence(f *windowRoundFixture, sequenceId Id) *ReceiveSequence {
	synctest.Wait()
	buffer := f.receiver.receiveBuffer
	buffer.mutex.Lock()
	defer buffer.mutex.Unlock()
	for id, sequence := range buffer.receiveSequences {
		if id.Source.SourceId == f.sender.ClientId() && id.SequenceId == sequenceId && id.LogicalLane == 0 {
			return sequence
		}
	}
	return nil
}

// Public Send emits both real wire records. No target control, data, ACK or
// proof is forwarded while collecting them, so the receiver has no generation.
func freshConsumerOpeningCapture(t *testing.T, f *windowRoundFixture, applicationPayload ...[]byte) (*windowRoundFrame, *windowRoundFrame) {
	t.Helper()
	var payload []byte
	if len(applicationPayload) != 0 {
		payload = applicationPayload[0]
	}
	packet := ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagSyn|tcpFlagAck, 23, payload)
	frame := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider, Raw: true, MessageBytes: packet}
	accepted, err := f.sender.SendWithTimeoutDetailed(frame, f.receiver.ClientId(), func(err error) {
		if err == nil {
			f.ackedCount++
		}
	}, time.Second, TransferKey{LogicalLane: 0})
	if !accepted || err != nil {
		MessagePoolReturn(frame.MessageBytes)
		t.Fatal("fresh signed SYNACK source was not admitted", err)
	}
	var opening *windowRoundFrame
	for range cap(f.senderOut) {
		wire := f.take(f.senderOut)
		if wire.pack == nil || wire.pack.LogicalLane != 0 {
			continue
		}
		pack := wire.pack
		if pack.Head && !pack.Nack && !pack.ContractAhead && pack.SequenceNumber == 0 &&
			len(pack.Frames) == 0 && pack.ContractFrame != nil && pack.ContractId == nil {
			if opening == nil {
				opening = wire
			}
			continue
		}
		if len(pack.Frames) == 1 && pack.Frames[0].MessageType == protocol.MessageType_IpIpPacketFromProvider {
			if opening == nil || pack.Head || pack.Nack || pack.ContractAhead || pack.ContractFrame != nil || pack.ContractId != nil ||
				pack.SequenceNumber != 1 || !bytes.Equal(pack.SequenceId, opening.pack.SequenceId) {
				t.Fatal("real initial writer did not preserve empty signed opening0 followed by nonhead data1")
			}
			return opening, wire
		}
	}
	t.Fatal("real sender did not materialize its signed opening and first SYNACK data")
	return nil, nil
}

// Physical-budget controls use an actual application byte. A bare SYNACK
// is protocol traffic and must not be used to demand DATA permission.
func freshConsumerDataOpeningCapture(t *testing.T, f *windowRoundFixture) (*windowRoundFrame, *windowRoundFrame) {
	t.Helper()
	opening, data := freshConsumerOpeningCapture(t, f, []byte{0x64})
	if !protocolOnlyPack(opening.pack) || protocolOnlyPack(data.pack) {
		t.Fatal("physical fixture did not distinguish protocol bootstrap from actual DATA")
	}
	return opening, data
}

// The adjacent data-bearing shape is produced by the real retry formatter on
// its genuinely retained item. It is not claimed to be the natural cold first
// write: an unACKed empty opening remains the ordinary worker's oldest item.
func freshConsumerOpeningFullDataHead(t *testing.T, f *windowRoundFixture, data *windowRoundFrame) *windowRoundFrame {
	t.Helper()
	synctest.Wait()
	var sender *SendSequence
	f.sender.sendBuffer.mutex.Lock()
	for _, candidate := range f.sender.sendBuffer.sendSequencesBySequenceId {
		if candidate.sequenceId == RequireIdFromBytes(data.pack.SequenceId) && candidate.logicalLane == 0 {
			sender = candidate
			break
		}
	}
	f.sender.sendBuffer.mutex.Unlock()
	if sender == nil {
		t.Fatal("full-data head has no genuine retained sender generation")
	}
	item := sender.resendQueue.GetByMessageId(RequireIdFromBytes(data.pack.MessageId))
	if item == nil || item.contractId == nil || item.head || item.sequenceNumber != data.pack.SequenceNumber {
		t.Fatal("full-data head has no genuine nonhead retry owner")
	}
	wire, full, err := sender.setHead(item, true)
	if err != nil || !full {
		t.Fatal("ordinary producer did not restore the genuine full-proof data head", err)
	}
	head := &windowRoundFrame{bytes: wire, pack: decodeCompactContractTestPack(t, wire)}
	f.heldFrames = append(f.heldFrames, head)
	if !head.pack.Head || head.pack.Nack || head.pack.ContractAhead || head.pack.ContractFrame == nil || head.pack.ContractId != nil ||
		len(head.pack.Frames) != 1 || head.pack.Frames[0].MessageType != protocol.MessageType_IpIpPacketFromProvider ||
		head.pack.SequenceNumber != data.pack.SequenceNumber || !bytes.Equal(head.pack.SequenceId, data.pack.SequenceId) ||
		!bytes.Equal(head.pack.MessageId, data.pack.MessageId) || !bytes.Equal(head.pack.Frames[0].MessageBytes, data.pack.Frames[0].MessageBytes) {
		t.Fatal("actual retry formatter changed the data identity, signed proof or owned SYNACK payload")
	}
	return head
}

func testFreshConsumerOpeningForeignRx(t *testing.T, fullDataHead bool) freshConsumerOpeningResult {
	t.Helper()
	GetMessagePoolAggregateStats()
	var result freshConsumerOpeningResult
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		const foreignCost = ByteCount(5120)
		root := NewTransferMemoryBudget(2 * foreignCost)
		client := NewTransferMemoryBudgetWithParent(2*foreignCost, root)
		rx := NewTransferMemoryBudgetWithParent(foreignCost, client)
		head := NewTransferMemoryBudgetWithParent(foreignCost, client)
		packBudget := NewTransferMemoryBudgetWithParent(foreignCost, client)
		t.Cleanup(func() {
			for _, budget := range []*TransferMemoryBudget{packBudget, head, rx, client, root} {
				assertRetainedBudgetBalance(t, budget)
				if budget.Stats().QuotaByteCount != 0 {
					t.Error("fresh opening retained logical quota after Clients joined")
				}
			}
		})
		probe := true
		targetCosts := map[Id]ByteCount{}
		observed, paid := 0, true
		stage := func(sequence *ReceiveSequence, owner *decodedPackOwner, event string) {
			if !probe || event != "pack_released" || owner == nil {
				return
			}
			cost, target := targetCosts[owner.receiveItem.messageId]
			if !target {
				return
			}
			observed++
			paid = paid && sequence != nil && owner.inUse && owner.receivePack.receiveMemoryOwned &&
				owner.receiveItem.memoryBudget == head && owner.receiveItem.QueueByteCount() == cost &&
				owner.receivePack.ingressSequence == sequence && owner.receivePack.ingressLoanByteCount == 0 &&
				rx.UsedByteCount() == foreignCost && head.UsedByteCount() == cost &&
				root.UsedByteCount() == foreignCost+cost && client.UsedByteCount() == root.UsedByteCount() &&
				packBudget.UsedByteCount() == 0 && packBudget.Stats().QuotaByteCount == 0
		}
		f, log := newConsumerFinalHandoffFixture(t, rx, head, packBudget, stage, nil)
		last := map[uint32]uint64{}
		foreignHead := consumerForeignHeadWrite(t, f, last, 1)
		foreignTail := consumerForeignHeadWrite(t, f, last, 1)
		foreign := consumerForeignHeadSequence(t, f, foreignHead)
		if foreign.receiveContract == nil || !foreign.receiveContract.verifiedReceiveProofSet ||
			f.deliveredCount != 0 || f.ackedCount != 0 || rx.UsedByteCount() != 0 {
			t.Fatal("fresh opening has no fully verified and drained foreign opening")
		}
		f.forward(foreignTail, f.receiverIn)
		heldId := RequireIdFromBytes(foreignTail.pack.MessageId)
		held := foreign.receiveQueue.GetByMessageId(heldId)
		if held == nil || held.committed || held.memoryBudget != rx || held.QueueByteCount() != foreignCost ||
			rx.Available() != 0 || rx.UsedByteCount() != foreignCost || foreign.holdPromise.set ||
			foreign.holdPromise.reservedByteCount != 0 || foreign.holdPromise.ingressLoanByteCount != 0 ||
			head.UsedByteCount() != 0 || head.Available() != foreignCost || root.Available() != foreignCost || client.Available() != foreignCost || len(f.receiverOut) != 0 {
			t.Fatal("the entire RX leaf is not held exclusively by the real foreign tentative tail")
		}
		opening, data := freshConsumerDataOpeningCapture(t, f)
		expectedSynAck := bytes.Clone(data.pack.Frames[0].MessageBytes)
		targetDelivered := 0
		f.receiver.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, _ Peer) {
			if source.SourceId != f.sender.ClientId() {
				return
			}
			for _, frame := range frames {
				if frame.MessageType == protocol.MessageType_IpIpPacketFromProvider && bytes.Equal(frame.MessageBytes, expectedSynAck) {
					targetDelivered++
				}
			}
		})
		targetId := RequireIdFromBytes(opening.pack.SequenceId)
		if targetId == RequireIdFromBytes(foreignHead.pack.SequenceId) || freshConsumerOpeningSequence(f, targetId) != nil ||
			f.receiver.ContractManager().ReceiveNoContract(f.sender.ClientId()) {
			t.Fatal("target generation existed before any target wire was forwarded, or contracts are bypassed")
		}
		source := opening
		if fullDataHead {
			source = freshConsumerOpeningFullDataHead(t, f, data)
		}
		path, err := FilteredTransferPath(source.bytes)
		if err != nil {
			t.Fatal(err)
		}
		scan, status := scanReceiveMemoryPack(source.bytes, path)
		if status != receiveMemoryPackScanReady || !scan.loanSafe || !scan.head || scan.nack || scan.contractAhead || scan.hasEncryptedControl ||
			!scan.contractFrameSet || scan.contractIdSet || scan.sequenceId != targetId || scan.retainedByteCount <= 0 ||
			scan.retainedByteCount > head.Available() || scan.retainedByteCount > root.Available() || scan.retainedByteCount > client.Available() ||
			path.SourceId != f.sender.ClientId() || path.DestinationId != f.receiver.ClientId() ||
			(fullDataHead && (scan.frameCount != 1 || scan.consumerFrameCount != 1)) || (!fullDataHead && scan.frameCount != 0) {
			t.Fatal("genuine new full-proof head lacks bounded independent physical capacity or correct source shape")
		}
		if fullDataHead {
			targetCosts[scan.messageId] = scan.retainedByteCount
		}
		result.cost = scan.retainedByteCount
		retry := freshConsumerOpeningClone(f, source)
		drops := f.receiver.ReceiveStats().PackHandoffDropCount
		beforeOpening := root.StatsWithDescendants(client, rx, head, packBudget)
		f.forward(source, f.receiverIn)
		result.drops = f.receiver.ReceiveStats().PackHandoffDropCount - drops
		target := freshConsumerOpeningSequence(f, targetId)
		result.blocked = target == nil
		if result.blocked {
			if observed != 0 || result.drops != 1 || f.deliveredCount != 0 || f.ackedCount != 0 || len(f.receiverOut) != 0 {
				t.Fatal("new target was not isolated before decode without any application, debit or ACK progress")
			}
		} else {
			expectedPaid := 0
			if fullDataHead {
				expectedPaid = 1
			} else {
				// The empty signed bootstrap is protocol state, not a DATA owner.
				afterOpening := root.StatsWithDescendants(client, rx, head, packBudget)
				for index := range beforeOpening {
					if beforeOpening[index] != afterOpening[index] {
						t.Fatal("protocol bootstrap changed DATA/root physical or quota permission")
					}
				}
			}
			if observed != expectedPaid || !paid || result.drops != 0 || target.receiveContract == nil || !target.receiveContract.verifiedReceiveProofSet ||
				target.ctx.Err() != nil || target.nextSequenceNumber != source.pack.SequenceNumber+1 {
				t.Fatal("new head did not reach the normal verifier and exact independently paid worker")
			}
			f.acknowledge()
			if !fullDataHead {
				dataPath, err := FilteredTransferPath(data.bytes)
				if err != nil {
					t.Fatal(err)
				}
				dataScan, dataStatus := scanReceiveMemoryPack(data.bytes, dataPath)
				if dataStatus != receiveMemoryPackScanReady || dataScan.retainedByteCount > head.Available() || dataScan.retainedByteCount > root.Available() {
					t.Fatal("first real SYNACK cannot fit after its opening released the bounded child")
				}
				targetCosts[dataScan.messageId] = dataScan.retainedByteCount
				f.forward(data, f.receiverIn)
				f.acknowledge()
				if observed != 1 || !paid {
					t.Fatal("actual DATA did not follow uncharged verified opening with independent paid ownership")
				}
			}
			if f.deliveredCount != 1 || f.ackedCount != 1 || targetDelivered != 1 {
				t.Fatal("new signed target did not deliver and ACK exactly its one SYNACK payload")
			}
		}
		if foreign.ctx.Err() != nil || foreign.receiveQueue.GetByMessageId(heldId) != held || held.committed || held.memoryBudget != rx ||
			held.QueueByteCount() != foreignCost || foreign.holdPromise.set || foreign.holdPromise.reservedByteCount != 0 ||
			foreign.holdPromise.ingressLoanByteCount != 0 || rx.UsedByteCount() != foreignCost ||
			head.UsedByteCount() != 0 || root.UsedByteCount() != foreignCost || client.UsedByteCount() != foreignCost || packBudget.Stats().QuotaByteCount != 0 {
			t.Fatal("target bootstrap borrowed or changed the foreign owner or leaked its independently funded child")
		}
		probe = false
		f.forward(foreignHead, f.receiverIn)
		f.acknowledge()
		if foreign.receiveQueue.Len() != 0 || rx.UsedByteCount() != 0 || foreign.ctx.Err() != nil {
			t.Fatal("real foreign predecessor did not release the exclusively foreign RX hold")
		}
		if result.blocked {
			f.forward(retry, f.receiverIn)
			f.acknowledge()
			if !fullDataHead {
				f.forward(data, f.receiverIn)
				f.acknowledge()
			}
		}
		target = freshConsumerOpeningSequence(f, targetId)
		if target == nil || target.receiveContract == nil || !target.receiveContract.verifiedReceiveProofSet || target.ctx.Err() != nil ||
			f.deliveredCount != 3 || f.ackedCount != 3 || targetDelivered != 1 || target.receiveQueue.Len() != 0 ||
			target.receiveContract.ackedByteCount < MessageByteCount(data.pack.Frames) ||
			rx.UsedByteCount() != 0 || head.UsedByteCount() != 0 || root.UsedByteCount() != 0 || client.UsedByteCount() != 0 ||
			packBudget.UsedByteCount() != 0 || packBudget.Stats().QuotaByteCount != 0 || len(log.messages()) != 0 {
			t.Fatal("same real source did not deliver/ACK all three payloads and release physical owners after foreign capacity returned")
		}
		t.Logf("FRESH_OPENING_BEHAVIOR_DRAINED blocked=%t full_data_head=%t cost=%d", result.blocked, fullDataHead, result.cost)
	})
	return result
}

func TestReceiveConsumerFreshSignedOpeningWithoutOwnTailProgresses(t *testing.T) {
	result := testFreshConsumerOpeningForeignRx(t, false)
	if t.Failed() {
		return
	}
	t.Log("FRESH_OPENING_CLEANUP_JOINED")
	if result.blocked {
		t.Fatalf("FOREIGN_RX_STRANDED_NEW_SIGNED_CONSUMER_OPENING: cost=%d drops=%d; exact original progressed after real foreign predecessor drained", result.cost, result.drops)
	}
}

func TestReceiveConsumerFreshFullProofDataHeadWithoutOwnTailProgresses(t *testing.T) {
	result := testFreshConsumerOpeningForeignRx(t, true)
	if t.Failed() {
		return
	}
	t.Log("FRESH_DATA_HEAD_CLEANUP_JOINED")
	if result.blocked {
		t.Fatalf("FOREIGN_RX_STRANDED_NEW_FULL_PROOF_DATA_HEAD: cost=%d drops=%d; explicitly formatted real retry progressed after foreign predecessor drained", result.cost, result.drops)
	}
}
