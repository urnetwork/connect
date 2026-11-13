// A known current consumer original meets shared RX pressure held entirely
// by another signed sequence. This is a bounded liveness witness, not proof
// of a physical Fast request's provider/sequence or permission to widen policy.
package connect

import (
	"bytes"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

type consumerForeignHeadResult struct {
	blocked                    bool
	firstDrops, firstDropBytes uint64
	physicalBytes              ByteCount
	foreignPromisedBytes       ByteCount
}

func TestReceiveConsumerForeignFullRxKnownCurrentHeadProgresses(t *testing.T) {
	result := testReceiveConsumerForeignCurrentHead(t, false)
	if t.Failed() {
		return
	}
	t.Log("FOREIGN_SEQUENCE_HEAD_CLEANUP_JOINED")
	if result.blocked {
		t.Fatalf("FOREIGN_RX_STRANDED_KNOWN_CURRENT_CONSUMER_HEAD: drops=%d dropped_bytes=%d current_physical_bytes=%d; exact signed original succeeded after foreign source drained", result.firstDrops, result.firstDropBytes, result.physicalBytes)
	}
}

// A target may be refused or independently funded. Neither outcome may borrow,
// evict or relabel a different sequence's committed predecessor reservation.
func TestReceiveConsumerForeignPromisedCreditRemainsOwned(t *testing.T) {
	result := testReceiveConsumerForeignCurrentHead(t, true)
	if !t.Failed() {
		t.Logf("FOREIGN_SEQUENCE_PROMISE_CLEANUP_JOINED promised_bytes=%d target_blocked=%t", result.foreignPromisedBytes, result.blocked)
	}
}

// This established real-wire positive differs by adding the target's own
// later paid tail. Its original assertions and sibling hard ceilings remain.
func TestReceiveConsumerSameSequenceBacklogKeepsCurrentHeadProgress(t *testing.T) {
	testReceiveConsumerFinalHandoff(t, "sibling-pack")
}

// Exact typed application frames come from the ordinary sender worker. Every
// contract-only opening traverses the real receiver and ACK route first.
func consumerForeignHeadWrite(t *testing.T, fixture *windowRoundFixture, last map[uint32]uint64, lane uint32) *windowRoundFrame {
	t.Helper()
	packet := ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagAck, 19, bytes.Repeat([]byte{0x61}, 1280))
	frame := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider, Raw: true, MessageBytes: packet}
	accepted, err := fixture.sender.SendWithTimeoutDetailed(frame, fixture.receiver.ClientId(), func(err error) {
		if err == nil {
			fixture.ackedCount++
		}
	}, time.Second, TransferKey{LogicalLane: lane})
	if !accepted || err != nil {
		MessagePoolReturn(frame.MessageBytes)
		t.Fatalf("FOREIGN_SEQUENCE_PUBLIC_SEND_FAILED lane=%d accepted=%t error=%v", lane, accepted, err)
	}
	for range cap(fixture.senderOut) {
		wire := fixture.take(fixture.senderOut)
		if wire.pack == nil {
			t.Fatal("foreign-sequence sender did not materialize a Pack")
		}
		if wire.pack.LogicalLane != lane {
			continue
		}
		for _, payload := range wire.pack.Frames {
			if payload.MessageType == protocol.MessageType_IpIpPacketFromProvider {
				if previous, seen := last[lane]; seen && wire.pack.SequenceNumber <= previous {
					break
				}
				last[lane] = wire.pack.SequenceNumber
				return wire
			}
		}
		if len(wire.pack.Frames) == 0 {
			fixture.forward(wire, fixture.receiverIn)
			fixture.acknowledge()
		}
	}
	t.Fatal("foreign-sequence typed application source never materialized")
	return nil
}

func consumerForeignHeadSequence(t *testing.T, fixture *windowRoundFixture, frame *windowRoundFrame) *ReceiveSequence {
	t.Helper()
	buffer := fixture.receiver.receiveBuffer
	buffer.mutex.Lock()
	defer buffer.mutex.Unlock()
	for id, sequence := range buffer.receiveSequences {
		if id.SequenceId == RequireIdFromBytes(frame.pack.SequenceId) && id.LogicalLane == frame.pack.LogicalLane {
			return sequence
		}
	}
	t.Fatal("real signed receive sequence was not installed by its opening")
	return nil
}

func testReceiveConsumerForeignCurrentHead(t *testing.T, foreignPromise bool) consumerForeignHeadResult {
	t.Helper()
	GetMessagePoolAggregateStats()
	var result consumerForeignHeadResult
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		const foreignPhysical = ByteCount(5120)
		rxBytes := foreignPhysical
		if foreignPromise {
			// A real commitment funds both its missing predecessor and the
			// configured copied-proof escrow before it can send a SACK.
			repair := DefaultClientSettingsWithBufferSize(64).ReceiveBufferSettings.ReceiveHeadContractRepairByteCount
			rxBytes = 2*foreignPhysical + repair
		}
		root := NewTransferMemoryBudget(2 * rxBytes)
		client := NewTransferMemoryBudgetWithParent(2*rxBytes, root)
		rx := NewTransferMemoryBudgetWithParent(rxBytes, client)
		recovery := NewTransferMemoryBudgetWithParent(rxBytes, client)
		pack := NewTransferMemoryBudgetWithParent(rxBytes, client)
		t.Cleanup(func() {
			for _, budget := range []*TransferMemoryBudget{pack, recovery, rx, client, root} {
				assertRetainedBudgetBalance(t, budget)
				if budget.Stats().QuotaByteCount != 0 {
					t.Error("foreign-sequence fixture retained logical permission after client join")
				}
			}
		})
		var targetId Id
		var target *ReceiveSequence
		var targetScan receiveMemoryPackScan
		var began, ended, released, refused int
		var finalSuccess, independentlyPaid bool
		var finalError string
		ownerStage := func(sequence *ReceiveSequence, owner *decodedPackOwner, stage string) {
			if targetId == (Id{}) || owner == nil || owner.receiveItem.messageId != targetId {
				return
			}
			if stage == "handoff_refused" {
				refused++
			}
			if stage != "pack_released" {
				return
			}
			released++
			independentlyPaid = sequence == target && owner.inUse && owner.receivePack.receiveMemoryOwned &&
				owner.receiveItem.memoryBudget == recovery && owner.receiveItem.QueueByteCount() == targetScan.retainedByteCount &&
				owner.receivePack.ingressSequence == target && owner.receivePack.ingressLoanByteCount == 0 &&
				recovery.UsedByteCount() == targetScan.retainedByteCount && rx.UsedByteCount() > 0 &&
				root.UsedByteCount() == rx.UsedByteCount()+targetScan.retainedByteCount &&
				client.UsedByteCount() == root.UsedByteCount() && pack.UsedByteCount() == 0 && pack.Stats().QuotaByteCount == 0
		}
		progress := func(event TransferProgressEvent) {
			if targetId == (Id{}) || event.MessageId != targetId {
				return
			}
			if event.Stage == "receive_pack_begin" {
				began++
			}
			if event.Stage == "receive_pack_end" {
				ended++
				finalSuccess, finalError = event.Success, event.ErrorKind
			}
		}
		fixture, log := newConsumerFinalHandoffFixture(t, rx, recovery, pack, ownerStage, progress)
		last := map[uint32]uint64{}
		foreignHead := consumerForeignHeadWrite(t, fixture, last, 1)
		head := consumerForeignHeadWrite(t, fixture, last, 0)
		foreignTail := consumerForeignHeadWrite(t, fixture, last, 1)
		foreign := consumerForeignHeadSequence(t, fixture, foreignHead)
		target = consumerForeignHeadSequence(t, fixture, head)
		contract := target.receiveContract
		if foreign == target || foreignHead.pack.LogicalLane != 1 || head.pack.LogicalLane != 0 ||
			!bytes.Equal(foreignTail.pack.SequenceId, foreignHead.pack.SequenceId) || head.pack.Head || head.pack.ContractFrame != nil ||
			foreignTail.pack.Head || foreignTail.pack.ContractFrame != nil || contract == nil || !contract.verifiedReceiveProofSet ||
			foreign.receiveContract == nil || !foreign.receiveContract.verifiedReceiveProofSet ||
			fixture.receiver.ContractManager().ReceiveNoContract(fixture.sender.ClientId()) ||
			target.nextSequenceNumber != head.pack.SequenceNumber || target.receiveQueue.Len() != 0 ||
			target.holdPromise.set || target.holdPromise.reservedByteCount != 0 || target.holdPromise.ingressHead != nil ||
			fixture.deliveredCount != 0 || fixture.ackedCount != 0 || rx.UsedByteCount() != 0 || root.UsedByteCount() != 0 {
			t.Fatal("foreign-sequence fixture lacks two fully drained verified openings and a target current original")
		}
		fixture.forward(foreignTail, fixture.receiverIn)
		foreignItem := foreign.receiveQueue.GetByMessageId(RequireIdFromBytes(foreignTail.pack.MessageId))
		if foreignItem == nil || foreignItem.decodedOwner == nil || !foreignItem.decodedOwner.receivePack.receiveMemoryOwned ||
			foreignItem.decodedOwner.receivePack.ingressSequence != foreign || foreignItem.memoryBudget != rx || foreignItem.QueueByteCount() != foreignPhysical ||
			foreignItem.committed != foreignPromise || foreign.holdPromise.set != foreignPromise ||
			foreignPromise && foreign.holdPromise.reservedByteCount <= 0 ||
			rx.UsedByteCount() != rx.TotalByteCount() || !foreignPromise && foreign.holdPromise.reservedByteCount != 0 {
			t.Fatal("actual foreign tail did not establish its exact retained/promise geometry")
		}
		fixture.acknowledge()
		foreignUsed := rx.UsedByteCount()
		foreignReserved := foreign.holdPromise.reservedByteCount
		foreignBoundary := foreign.holdPromise.boundary
		foreignLoan := foreign.holdPromise.ingressLoanByteCount
		foreignFloor := foreign.holdPromise.ingressFloor
		foreignRepair := foreign.heldHeadRepair
		foreignRepairEligible := foreignItem.decodedOwner.receivePack.heldHeadRepairEligible
		ackedBefore := fixture.ackedCount
		path, err := FilteredTransferPath(head.bytes)
		if err != nil {
			t.Fatal(err)
		}
		scan, status := scanReceiveMemoryPack(head.bytes, path)
		targetScan = scan
		if status != receiveMemoryPackScanReady || !targetScan.loanSafe || targetScan.compatibility || targetScan.nack || targetScan.contractAhead ||
			targetScan.consumerFrameCount != 1 || targetScan.frameCount != 1 || targetScan.retainedByteCount != foreignPhysical ||
			targetScan.sequenceNumber != target.holdPromise.ingressFloor || targetScan.sequenceNumber != target.nextSequenceNumber ||
			targetScan.retainedByteCount <= rx.Available() || targetScan.retainedByteCount > recovery.Available() ||
			targetScan.retainedByteCount > root.Available() || targetScan.retainedByteCount > client.Available() ||
			path.SourceId != fixture.sender.ClientId() || path.DestinationId != fixture.receiver.ClientId() ||
			target.receiveQueue.Len() != 0 || target.holdPromise.ingressHead != nil || target.holdPromise.set ||
			target.holdPromise.reservedByteCount != 0 || target.receiveContract != contract || recovery.UsedByteCount() != 0 {
			t.Fatal("foreign-only RX pressure is not the sole unavailable physical permission for this exact known current source")
		}
		targetId = targetScan.messageId
		dropBefore := fixture.receiver.receivePackHandoffDropCount.Load()
		byteBefore := fixture.receiver.receivePackHandoffDropByteCount.Load()
		// forward consumes its caller reference. Keep a distinct immutable
		// wire share for the later exact retry; cleanup also returns it when
		// the first target offer already progressed or a premise fails.
		MessagePoolShareReadOnly(head.bytes)
		headRetry := &windowRoundFrame{bytes: head.bytes, pack: head.pack}
		fixture.heldFrames = append(fixture.heldFrames, headRetry)
		fixture.forward(head, fixture.receiverIn)
		result.firstDrops = fixture.receiver.receivePackHandoffDropCount.Load() - dropBefore
		result.firstDropBytes = fixture.receiver.receivePackHandoffDropByteCount.Load() - byteBefore
		result.physicalBytes, result.foreignPromisedBytes = targetScan.retainedByteCount, foreignReserved
		result.blocked = fixture.deliveredCount == 0
		if result.blocked {
			if began != 0 || ended != 0 || released != 0 || refused != 0 || result.firstDrops != 1 ||
				result.firstDropBytes != uint64(targetScan.messageByteCount) || fixture.ackedCount != ackedBefore ||
				target.nextSequenceNumber != head.pack.SequenceNumber || target.receiveQueue.Len() != 0 || target.holdPromise.ingressHead != nil {
				t.Fatal("target refusal was not isolated before decode/final callback and without an ACK")
			}
		} else if fixture.deliveredCount != 1 || began != 1 || ended != 1 || released != 1 || refused != 0 ||
			!finalSuccess || finalError != "" || !independentlyPaid || result.firstDrops != 0 || result.firstDropBytes != 0 {
			t.Fatal("target progress did not use its independently paid decoded owner and real final callback")
		}
		if foreign.ctx.Err() != nil || target.ctx.Err() != nil || foreign.receiveQueue.GetByMessageId(foreignItem.messageId) != foreignItem ||
			foreignItem.committed != foreignPromise || foreignItem.memoryBudget != rx || foreignItem.QueueByteCount() != foreignPhysical ||
			foreign.holdPromise.set != foreignPromise || foreign.holdPromise.reservedByteCount != foreignReserved ||
			foreign.holdPromise.boundary != foreignBoundary || foreign.holdPromise.ingressLoanByteCount != foreignLoan ||
			foreign.holdPromise.ingressFloor != foreignFloor || foreign.heldHeadRepair != foreignRepair ||
			foreignItem.decodedOwner.receivePack.heldHeadRepairEligible != foreignRepairEligible ||
			rx.UsedByteCount() != foreignUsed || recovery.UsedByteCount() != 0 ||
			root.UsedByteCount() != foreignUsed || client.UsedByteCount() != foreignUsed || pack.UsedByteCount() != 0 || pack.Stats().QuotaByteCount != 0 {
			t.Fatal("target offer changed foreign owned bytes/promise, canceled either source, or leaked recovery credit")
		}
		// The same real foreign predecessor releases only its own hold/promise.
		// Then retry the exact original bytes if the first target offer refused.
		fixture.forward(foreignHead, fixture.receiverIn)
		fixture.acknowledge()
		if foreign.receiveQueue.Len() != 0 || rx.UsedByteCount() != 0 || foreign.ctx.Err() != nil {
			t.Fatal("real foreign predecessor did not drain its exact retained hold")
		}
		if result.blocked {
			fixture.forward(headRetry, fixture.receiverIn)
		}
		fixture.acknowledge()
		if fixture.deliveredCount != 3 || fixture.ackedCount != 3 || began != 1 || ended != 1 || released != 1 || refused != 0 ||
			!finalSuccess || finalError != "" || target.nextSequenceNumber != head.pack.SequenceNumber+1 ||
			target.receiveQueue.Len() != 0 || target.ctx.Err() != nil || foreign.ctx.Err() != nil ||
			rx.UsedByteCount() != 0 || recovery.UsedByteCount() != 0 || root.UsedByteCount() != 0 || pack.UsedByteCount() != 0 ||
			pack.Stats().QuotaByteCount != 0 || len(log.messages()) != 0 {
			t.Fatal("exact signed source retry did not deliver/ACK all three payloads and release every physical owner")
		}
		t.Logf("FOREIGN_SEQUENCE_HEAD_BEHAVIOR_DRAINED blocked=%t foreign_promise=%t physical_bytes=%d", result.blocked, foreignPromise, result.physicalBytes)
	})
	return result
}
