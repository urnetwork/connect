// A real signed contract switch can be current at the receiver while an
// earlier delivered original still awaits its ACK at the public sender.
package connect

import (
	"bytes"
	"context"
	"crypto/sha256"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// Keep the predecessor's actual ACK in flight, rather than rewriting Head or
// contract fields. The unchanged original must progress through foreign RX
// pressure; its natural promoted retry is a separate ownership/authority control.
func TestReceiveConsumerPublicNonHeadContractSwitchUnderForeignRxPressure(t *testing.T) {
	GetMessagePoolAggregateStats()
	blocked := false
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		const foreignBytes = ByteCount(5120)
		root := NewTransferMemoryBudget(2 * foreignBytes)
		client := NewTransferMemoryBudgetWithParent(2*foreignBytes, root)
		rx := NewTransferMemoryBudgetWithParent(foreignBytes, client)
		head := NewTransferMemoryBudgetWithParent(foreignBytes, client)
		pack := NewTransferMemoryBudgetWithParent(foreignBytes, client)
		t.Cleanup(func() {
			for _, budget := range []*TransferMemoryBudget{pack, head, rx, client, root} {
				assertRetainedBudgetBalance(t, budget)
				if budget.Stats().QuotaByteCount != 0 {
					t.Error("contract switch retained shared Pack quota")
				}
			}
		})
		layout := consumerPaidPackByteLayout{label: "current-proof-switch", byteLimit: 174762,
			leafBytes: foreignBytes, rootBytes: 2 * foreignBytes, countLimit: 85}
		var target *ReceiveSequence
		var targetId atomic.Pointer[Id]
		var targetCost ByteCount
		var targetOwners, targetPaidOwners atomic.Int32
		fixture, _, _, _ := newConsumerPaidPackByteFixture(t, layout, rx, head, pack, nil,
			func(receiver bool, settings *ClientSettings) {
				settings.ContractManagerSettings.InitialContractTransferByteCount = 64 * 1024
				settings.ContractManagerSettings.StandardContractTransferByteCount = 64 * 1024
				settings.SendBufferSettings.ContractAheadFloorByteCount = 16 * 1024
				if receiver {
					settings.ReceiveBufferSettings.AdvertiseReceiveWindow = true
					settings.ReceiveBufferSettings.receiveOwnerStageForTest = func(sequence *ReceiveSequence, owner *decodedPackOwner, stage string) {
						id := targetId.Load()
						if id == nil || owner == nil || stage != "pack_released" || owner.receiveItem.messageId != *id {
							return
						}
						targetOwners.Add(1)
						if sequence == target && owner.inUse && owner.receivePack.receiveMemoryOwned &&
							owner.receivePack.ingressSequence == target && owner.receivePack.ingressLoanByteCount == 0 &&
							owner.receiveItem.memoryBudget == head && owner.receiveItem.QueueByteCount() == targetCost &&
							head.UsedByteCount() == targetCost && rx.UsedByteCount() == foreignBytes &&
							client.UsedByteCount() == foreignBytes+targetCost && root.UsedByteCount() == foreignBytes+targetCost &&
							pack.UsedByteCount() == 0 && pack.Stats().QuotaByteCount == 0 {
							targetPaidOwners.Add(1)
						}
					}
				}
			})
		var calls, successes [128]atomic.Int32
		sent := 0
		// One public input per captured DATA wire prevents coalescing from
		// changing the contract boundary. Empty openings/announcements use the
		// actual peer and ACK path, without forged sequence or proof state.
		send := func(lane uint32, byteCount int) (*windowRoundFrame, int) {
			if byteCount <= 40 || sent == len(calls) {
				t.Fatal("invalid bounded public contract switch input")
			}
			slot := sent
			sent++
			expected := ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagAck,
				uint32(1000+slot), bytes.Repeat([]byte{0x63}, byteCount-40))
			packet := MessagePoolCopy(expected)
			frame, err := ipPacketFromProviderFrame(packet, 2)
			if err != nil {
				MessagePoolReturn(packet)
				t.Fatal(err)
			}
			accepted, err := fixture.sender.SendWithTimeoutDetailed(frame, fixture.receiver.ClientId(), func(err error) {
				calls[slot].Add(1)
				if err == nil {
					successes[slot].Add(1)
				}
			}, time.Second, TransferKey{LogicalLane: lane})
			if !accepted || err != nil {
				MessagePoolReturn(packet)
				t.Fatalf("public contract switch source refused lane=%d accepted=%t error=%v", lane, accepted, err)
			}
			for range cap(fixture.senderOut) {
				wire := fixture.take(fixture.senderOut)
				if wire.pack == nil || wire.pack.LogicalLane != lane {
					t.Fatal("public contract switch source changed its lane or wire class")
				}
				if len(wire.pack.Frames) == 0 {
					fixture.forward(wire, fixture.receiverIn)
					fixture.acknowledge()
					continue
				}
				if wire.pack.Nack || len(wire.pack.Frames) != 1 || !wire.pack.Frames[0].Raw ||
					!bytes.Equal(wire.pack.Frames[0].MessageBytes, expected) {
					t.Fatal("public contract switch source changed its reliable DATA")
				}
				return wire, slot
			}
			t.Fatal("public contract switch source produced no DATA wire")
			return nil, 0
		}
		drainControls := func() {
			synctest.Wait()
			for len(fixture.senderOut) != 0 {
				wire := fixture.take(fixture.senderOut)
				if wire.pack == nil || len(wire.pack.Frames) != 0 {
					t.Fatal("warm successor control drain encountered unexpected DATA")
				}
				fixture.forward(wire, fixture.receiverIn)
				fixture.acknowledge()
			}
		}
		warm, _ := send(1, 1280)
		fixture.forward(warm, fixture.receiverIn)
		fixture.acknowledge()
		drainControls()
		target = consumerForeignHeadSequence(t, fixture, warm)
		sender := inflightHoleSender(t, fixture, warm)
		previous := sender.sendContract
		for range 64 {
			if sender.sendContractRemainingByteCount() <= 1280 {
				break
			}
			wire, _ := send(1, 1280)
			fixture.forward(wire, fixture.receiverIn)
			fixture.acknowledge()
			drainControls()
		}
		remaining := sender.sendContractRemainingByteCount()
		successor := sender.aheadSendContract
		if previous == nil || sender.sendContract != previous || remaining <= 40 || remaining > 1280 ||
			successor == nil || !successor.acknowledgedAhead || len(sender.sendItems) != 0 ||
			target.receiveContract == nil || target.receiveContract.contractId != previous.contractId ||
			!target.holdPromise.establishedContract || root.UsedByteCount() != 0 {
			t.Fatalf("actual signed successor was not ready before exhaustion: remaining=%d successor=%t", remaining, successor != nil)
		}
		verified := target.openReceiveContracts[successor.contractId]
		if verified == nil || !verified.verifiedReceiveProofSet || verified.expired() {
			t.Fatal("real successor announcement did not establish authenticated proof")
		}
		// Consume precisely the remaining grant with actual TCP DATA. Its
		// delivery advances RX, but its captured real ACK leaves one TX owner.
		predecessor, predecessorSlot := send(1, int(remaining))
		fixture.forward(predecessor, fixture.receiverIn)
		heldAck := fixture.take(fixture.receiverOut)
		if heldAck.ack == nil || heldAck.ack.Selective || !bytes.Equal(heldAck.ack.MessageId, predecessor.pack.MessageId) ||
			len(fixture.receiverOut) != 0 || calls[predecessorSlot].Load() != 0 || len(sender.sendItems) != 1 ||
			sender.sendContract != previous || sender.sendContractRemainingByteCount() != 0 ||
			target.holdPromise.ingressFloor != predecessor.pack.SequenceNumber+1 ||
			target.holdPromise.ingressHead != nil || target.holdPromise.set || target.receiveQueue.Len() != 0 || root.UsedByteCount() != 0 {
			t.Fatal("withheld actual predecessor ACK did not isolate empty RX from retained TX")
		}
		foreignHead, _ := send(0, 1320)
		foreignTail, _ := send(0, 1320)
		foreign := consumerForeignHeadSequence(t, fixture, foreignHead)
		fixture.forward(foreignTail, fixture.receiverIn)
		foreignId := RequireIdFromBytes(foreignTail.pack.MessageId)
		foreignOwner := foreign.receiveQueue.GetByMessageId(foreignId)
		if foreignOwner == nil || foreignOwner.committed || foreignOwner.memoryBudget != rx || foreignOwner.QueueByteCount() != foreignBytes ||
			foreign.holdPromise.set || foreign.holdPromise.reservedByteCount != 0 || rx.UsedByteCount() != foreignBytes ||
			head.Available() != foreignBytes || client.Available() != foreignBytes || root.Available() != foreignBytes {
			t.Fatal("actual foreign tentative original did not fill RX with independent Head/root room")
		}
		original, targetSlot := send(1, 72)
		path, err := FilteredTransferPath(original.bytes)
		if err != nil {
			t.Fatal(err)
		}
		scan, status := scanReceiveMemoryPack(original.bytes, path)
		if status != receiveMemoryPackScanReady || !scan.loanSafe || scan.head || scan.nack || scan.contractAhead ||
			!scan.contractFrameSet || scan.contractIdSet || scan.consumerFrameCount != 1 || scan.protocolFrameCount != 0 ||
			original.pack.ContractFrame == nil || sha256.Sum256(original.pack.ContractFrame.MessageBytes) != verified.verifiedReceiveProof ||
			sender.sendContract != successor || target.receiveContract.contractId != previous.contractId ||
			scan.sequenceNumber != target.holdPromise.ingressFloor || target.holdPromise.ingressHead != nil || target.holdPromise.set ||
			target.receiveQueue.Len() != 0 || calls[predecessorSlot].Load() != 0 || len(sender.sendItems) != 2 ||
			scan.retainedByteCount <= 0 || scan.retainedByteCount > head.Available() || scan.retainedByteCount > root.Available() {
			t.Fatalf("public producer did not isolate payable current non-Head successor proof: head=%t proof=%t compact=%t cost=%d", scan.head, scan.contractFrameSet, scan.contractIdSet, scan.retainedByteCount)
		}
		targetCost = scan.retainedByteCount
		id := scan.messageId
		targetId.Store(&id)
		beforeDrops := fixture.receiver.ReceiveStats().PackHandoffDropCount
		beforeDeliveries := fixture.deliveredCount
		began := time.Now()
		fixture.forward(original, fixture.receiverIn)
		blocked = fixture.receiver.ReceiveStats().PackHandoffDropCount != beforeDrops
		if blocked && (targetOwners.Load() != 0 || fixture.deliveredCount != beforeDeliveries || len(fixture.receiverOut) != 0 ||
			target.holdPromise.ingressFloor != scan.sequenceNumber || target.holdPromise.ingressHead != nil ||
			target.holdPromise.set || calls[targetSlot].Load() != 0 || head.UsedByteCount() != 0) {
			t.Fatal("refused non-Head proof changed delivery, ACK, authority or memory custody")
		}
		// Release only the predecessor's real ACK. Foreign RX remains full.
		// A blocked original must recover through its sender's actual RTO/Head
		// promotion, retaining identity, payload and the signed switch proof.
		fixture.forward(heldAck, fixture.senderIn)
		if blocked {
			retry := pressureContractNaturalRetry(t, fixture, original.pack)
			if !retry.pack.Head || retry.pack.ContractFrame == nil || retry.pack.ContractId != nil ||
				!bytes.Equal(retry.pack.MessageId, original.pack.MessageId) || !bytes.Equal(retry.pack.SequenceId, original.pack.SequenceId) ||
				retry.pack.SequenceNumber != original.pack.SequenceNumber || len(retry.pack.Frames) != 1 ||
				!bytes.Equal(retry.pack.Frames[0].MessageBytes, original.pack.Frames[0].MessageBytes) ||
				!bytes.Equal(retry.pack.ContractFrame.MessageBytes, original.pack.ContractFrame.MessageBytes) || rx.UsedByteCount() != foreignBytes {
				t.Fatal("natural promoted retry changed original authority or foreign pressure")
			}
			retryPath, err := FilteredTransferPath(retry.bytes)
			if err != nil {
				t.Fatal(err)
			}
			retryScan, status := scanReceiveMemoryPack(retry.bytes, retryPath)
			if status != receiveMemoryPackScanReady || retryScan.retainedByteCount <= 0 ||
				retryScan.retainedByteCount > head.Available() || retryScan.retainedByteCount > root.Available() {
				t.Fatal("natural promoted retry exceeded its unchanged physical caps")
			}
			targetCost = retryScan.retainedByteCount
			targetId.Store(&id)
			fixture.forward(retry, fixture.receiverIn)
		}
		fixture.acknowledge()
		if targetOwners.Load() != 1 || targetPaidOwners.Load() != 1 || successes[targetSlot].Load() != 1 ||
			successes[predecessorSlot].Load() != 1 || fixture.deliveredCount != beforeDeliveries+1 ||
			target.receiveContract.contractId != successor.contractId || target.holdPromise.ingressFloor != scan.sequenceNumber+1 ||
			target.holdPromise.ingressHead != nil || target.holdPromise.set || head.UsedByteCount() != 0 ||
			rx.UsedByteCount() != foreignBytes || foreign.receiveQueue.GetByMessageId(foreignId) != foreignOwner || foreignOwner.committed {
			t.Fatal("real successor delivery/ACK did not retain exact independent custody and foreign original")
		}
		t.Logf("CURRENT_NONHEAD_SWITCH_WIRE number=%d original_cost=%d delivered_cost=%d blocked=%t recovery_elapsed=%s foreign_rx=%d own_promise=false actual_proof_verified=true", scan.sequenceNumber, scan.retainedByteCount, targetCost, blocked, time.Since(began), rx.UsedByteCount())
		fixture.forward(foreignHead, fixture.receiverIn)
		fixture.acknowledge()
		if root.UsedByteCount() != 0 || foreign.receiveQueue.Len() != 0 {
			t.Fatal("healthy foreign source did not drain after its real predecessor delivery")
		}
		for slot := range sent {
			if calls[slot].Load() != 1 || successes[slot].Load() != 1 {
				t.Fatalf("actual public source callback was not exactly once successful: slot=%d calls=%d successes=%d", slot, calls[slot].Load(), successes[slot].Load())
			}
		}
		fixture.cancel()
		owner, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if fixture.sender.CloseAndWait(owner) != nil || fixture.receiver.CloseAndWait(owner) != nil {
			t.Fatal("signed contract switch clients did not join")
		}
	})
	if !t.Failed() {
		t.Log("CURRENT_NONHEAD_SWITCH_CLEANUP_JOINED clients=true budgets=true pool=true")
		if blocked {
			t.Error("CURRENT_NONHEAD_FULL_PROOF_REFUSED_UNTIL_NATURAL_HEAD_PROMOTION")
		}
	}
}
