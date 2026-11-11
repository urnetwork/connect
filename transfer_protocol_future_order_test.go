//go:build urnetwork_synthetic_fixture

// Distinct reliable protocol originals retain ordering and retry identity.
// A missing DATA predecessor must not make the second future control depend
// on its retransmission timer when the configured owner count has room.
package connect

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func TestProtocolPublicFutureOrderDrainsWithoutRetryAfterDataPredecessor(t *testing.T) {
	GetMessagePoolAggregateStats()
	refusedEarlier := false
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		const leafBytes = ByteCount(128 * 1024)
		root := NewTransferMemoryBudget(4 * leafBytes)
		client := NewTransferMemoryBudgetWithParent(4*leafBytes, root)
		tx := NewTransferMemoryBudgetWithParent(leafBytes, client)
		rx := NewTransferMemoryBudgetWithParent(leafBytes, client)
		head := NewTransferMemoryBudgetWithParent(leafBytes, client)
		pack := NewTransferMemoryBudgetWithParent(leafBytes, client)
		budgets := []*TransferMemoryBudget{pack, head, rx, tx, client, root}
		t.Cleanup(func() {
			for _, budget := range budgets {
				assertRetainedBudgetBalance(t, budget)
				if budget.Stats().QuotaByteCount != 0 {
					t.Error("future protocol ordering retained DATA quota")
				}
			}
		})
		layout := consumerPaidPackByteLayout{label: "future-protocol-order", byteLimit: 174762,
			leafBytes: leafBytes, rootBytes: 4 * leafBytes, countLimit: 85}
		fixture, log, _, _ := newConsumerPaidPackByteFixture(t, layout, rx, head, pack, nil,
			func(receiver bool, settings *ClientSettings) {
				settings.ReceiveBufferSettings.AdvertiseReceiveWindow = true
				if !receiver {
					settings.SendBufferSettings.ResendQueueBudget = tx
					settings.SendBufferSettings.ResendQueueRetainedByteAccounting = true
					settings.SendBufferSettings.ResendQueueMinByteCount = 0
					settings.SendBufferSettings.ResendQueueMaxByteCount = leafBytes
				}
			})
		var results [4]protocolPressureCompletion
		var expected [4][]byte
		var originals [4]*windowRoundFrame
		var selectiveReceipts [4]int
		var delivered [4]int
		var order []int
		var deliveryLock sync.Mutex
		fixture.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
			deliveryLock.Lock()
			defer deliveryLock.Unlock()
			for _, frame := range frames {
				if frame.MessageType != protocol.MessageType_IpIpPacketFromProvider {
					continue
				}
				for slot := range expected {
					if expected[slot] != nil && bytes.Equal(frame.MessageBytes, expected[slot]) {
						delivered[slot]++
						order = append(order, slot)
					}
				}
			}
		})
		send := func(slot int, control bool) *windowRoundFrame {
			payload := []byte{0x61 + byte(slot)}
			if control {
				payload = nil
			}
			expected[slot] = ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagAck, uint32(4000+slot), payload)
			packet := MessagePoolCopy(expected[slot])
			frame, err := ipPacketFromProviderFrame(packet, 2)
			if err != nil || frame == nil || !frame.Raw || protocolOnlyFrames([]*protocol.Frame{frame}) != control {
				MessagePoolReturn(packet)
				t.Fatal("public future-order source did not construct exact DATA or pure ACK")
			}
			opts := []any{TransferKey{LogicalLane: 1}}
			if control {
				opts = append(opts, NoBudget())
			}
			accepted, err := fixture.sender.SendWithTimeoutDetailed(frame, fixture.receiver.ClientId(), results[slot].record, time.Second, opts...)
			if !accepted || err != nil {
				MessagePoolReturn(packet)
				t.Fatalf("public future-order source admission failed: slot=%d accepted=%t error=%v", slot, accepted, err)
			}
			for range cap(fixture.senderOut) {
				wire := fixture.take(fixture.senderOut)
				if wire.pack == nil || wire.pack.LogicalLane != 1 || wire.pack.Nack {
					t.Fatal("public future-order source changed lane or reliable wire class")
				}
				if len(wire.pack.Frames) == 0 {
					if slot != 0 {
						t.Fatal("small established source unexpectedly emitted a new empty control")
					}
					fixture.forward(wire, fixture.receiverIn)
					fixture.acknowledge()
					synctest.Wait()
					continue
				}
				if len(wire.pack.Frames) != 1 || !wire.pack.Frames[0].Raw ||
					!bytes.Equal(wire.pack.Frames[0].MessageBytes, expected[slot]) || protocolOnlyPack(wire.pack) != control {
					t.Fatal("public future-order source changed original payload or classification")
				}
				originals[slot] = wire
				return wire
			}
			t.Fatal("public future-order source emitted no selected original")
			return nil
		}
		// Forward only actual ACK frames. Before the DATA predecessor arrives,
		// a selective receipt may name a retained future control, never a
		// delivered prefix or the absent original.
		pumpAcks := func(predecessor *windowRoundFrame, beforePredecessor bool) {
			synctest.Wait()
			for len(fixture.receiverOut) != 0 {
				ack := fixture.take(fixture.receiverOut)
				if ack.ack == nil || len(ack.ack.EvictedSequenceNumbers) != 0 {
					t.Fatal("future-order ACK route contained an unrelated wire")
				}
				matched := false
				acknowledgedNumber := uint64(0)
				for slot, original := range originals {
					if original != nil && bytes.Equal(ack.ack.MessageId, original.pack.MessageId) &&
						bytes.Equal(ack.ack.SequenceId, original.pack.SequenceId) {
						matched = true
						acknowledgedNumber = original.pack.SequenceNumber
						if ack.ack.Selective {
							selectiveReceipts[slot]++
						}
					}
				}
				if !matched {
					t.Fatal("actual ACK did not name one of the captured original identities")
				}
				if beforePredecessor && (!ack.ack.Selective || acknowledgedNumber <= predecessor.pack.SequenceNumber ||
					bytes.Equal(ack.ack.MessageId, predecessor.pack.MessageId)) {
					t.Fatal("future protocol emitted a false cumulative or missing-original receipt")
				}
				fixture.forward(ack, fixture.senderIn)
				synctest.Wait()
			}
		}
		warm := send(0, false)
		fixture.forward(warm, fixture.receiverIn)
		pumpAcks(nil, false)
		sequence := consumerForeignHeadSequence(t, fixture, warm)
		if results[0].success.Load() != 1 || sequence.receiveContract == nil || !sequence.receiveContract.verifiedReceiveProofSet ||
			root.UsedByteCount() != 0 || sequence.receiveQueue.Len() != 0 {
			t.Fatal("real signed warming delivery did not establish an empty verified generation")
		}
		predecessor := send(1, false)
		earlier := send(2, true)
		later := send(3, true)
		base := predecessor.pack.SequenceNumber
		if earlier.pack.SequenceNumber != base+1 || later.pack.SequenceNumber != base+2 || earlier.pack.Head || later.pack.Head ||
			!bytes.Equal(predecessor.pack.SequenceId, earlier.pack.SequenceId) || !bytes.Equal(predecessor.pack.SequenceId, later.pack.SequenceId) ||
			sequence.nextSequenceNumber != base || sequence.holdPromise.ingressFloor != base ||
			cap(sequence.protocolPacks) < 2 || tx.UsedByteCount() <= 0 || rx.UsedByteCount() != 0 {
			t.Fatal("actual public originals did not isolate DATA0 then non-Head protocol1/2 in one live generation")
		}
		beforeTx := tx.Stats()
		fixture.forward(later, fixture.receiverIn)
		pumpAcks(predecessor, true)
		laterId := RequireIdFromBytes(later.pack.MessageId)
		laterOwner := sequence.receiveQueue.GetByMessageId(laterId)
		if laterOwner == nil || !laterOwner.noBudget || laterOwner.memoryBudget != nil || laterOwner.MessageByteCount() != 0 || laterOwner.QueueByteCount() != 0 ||
			sequence.nextSequenceNumber != base || results[1].calls.Load() != 0 || results[2].calls.Load() != 0 || results[3].calls.Load() != 0 {
			t.Fatal("actual future protocol2 was not retained without delivery or DATA charge")
		}
		drops := fixture.receiver.ReceiveStats().ReceiveQueueDropCount
		fixture.forward(earlier, fixture.receiverIn)
		pumpAcks(predecessor, true)
		earlierId := RequireIdFromBytes(earlier.pack.MessageId)
		earlierOwner := sequence.receiveQueue.GetByMessageId(earlierId)
		refusedEarlier = earlierOwner == nil
		if sequence.receiveQueue.GetByMessageId(laterId) != laterOwner ||
			(!refusedEarlier && (!earlierOwner.noBudget || earlierOwner.memoryBudget != nil || sequence.receiveQueue.Len() != 2)) ||
			refusedEarlier && fixture.receiver.ReceiveStats().ReceiveQueueDropCount != drops+1 ||
			!refusedEarlier && fixture.receiver.ReceiveStats().ReceiveQueueDropCount != drops ||
			sequence.nextSequenceNumber != base || sequence.holdPromise.ingressFloor != base {
			t.Fatal("second future protocol changed retained identity, ordering or its actual refusal census")
		}
		if refusedEarlier && selectiveReceipts[2] != 0 {
			t.Fatal("refused earlier protocol original acquired a false selective receipt")
		}
		// Committed-prefix ACKs can reserve DATA permission for an unknown
		// missing predecessor. That promise is distinct from charging either
		// protocol owner; account for it rather than hiding or disabling it.
		if tx.Stats() != beforeTx || rx.UsedByteCount() != sequence.holdPromise.reservedByteCount ||
			head.UsedByteCount() != 0 || pack.UsedByteCount() != 0 || pack.Stats().QuotaByteCount != 0 ||
			client.UsedByteCount() != tx.UsedByteCount()+rx.UsedByteCount() || root.UsedByteCount() != client.UsedByteCount() {
			t.Fatal("future protocol ownership changed TX DATA custody or its exact missing-DATA promise")
		}
		began := time.Now()
		fixture.forward(predecessor, fixture.receiverIn)
		pumpAcks(nil, false)
		if time.Since(began) != 0 || results[1].success.Load() != 1 {
			t.Fatal("actual DATA predecessor did not progress without a recovery clock advance")
		}
		if refusedEarlier {
			if results[2].calls.Load() != 0 || results[3].calls.Load() != 0 || sequence.nextSequenceNumber != base+1 ||
				sequence.receiveQueue.GetByMessageId(laterId) != laterOwner || sequence.receiveQueue.GetByMessageId(earlierId) != nil {
				t.Fatal("old future protocol refusal did not leave the exact unacknowledged prefix gap")
			}
			// Old cleanup observes the public sender's normal immutable retry;
			// no caller replay, forced timer or synthetic acknowledgement closes it.
			retry := pressureContractNaturalRetry(t, fixture, earlier.pack)
			if retry.pack.SequenceNumber != earlier.pack.SequenceNumber || !protocolOnlyPack(retry.pack) ||
				!bytes.Equal(retry.pack.MessageId, earlier.pack.MessageId) || !bytes.Equal(retry.pack.SequenceId, earlier.pack.SequenceId) ||
				len(retry.pack.Frames) != 1 || !bytes.Equal(retry.pack.Frames[0].MessageBytes, expected[2]) {
				t.Fatal("natural earlier-control retry changed original identity or packet")
			}
			fixture.forward(retry, fixture.receiverIn)
			pumpAcks(nil, false)
		}
		if sequence.nextSequenceNumber != base+3 || sequence.receiveQueue.Len() != 0 || root.UsedByteCount() != 0 {
			t.Fatal("actual predecessor and protocol originals did not drain and refund")
		}
		deliveryLock.Lock()
		if len(order) != 4 || order[0] != 0 || order[1] != 1 || order[2] != 2 || order[3] != 3 {
			t.Error("actual callback order did not preserve distinct DATA0/protocol1/protocol2 originals")
		}
		for slot := range delivered {
			if delivered[slot] != 1 || results[slot].calls.Load() != 1 || results[slot].success.Load() != 1 ||
				results[slot].canceled.Load() != 0 || results[slot].other.Load() != 0 {
				t.Errorf("public original did not have exactly one real delivery and successful ACK: slot=%d", slot)
			}
		}
		deliveryLock.Unlock()
		if len(log.messages()) != 0 {
			t.Error("future protocol ordering produced an unrelated worker error")
		}
		t.Logf("FUTURE_PROTOCOL_ORDER_BEHAVIOR_DRAINED refused_earlier=%t predecessor=%d first_control=%d second_control=%d recovery_elapsed=%s configured_count=%d selective_receipts=%v", refusedEarlier, base, base+1, base+2, time.Since(began), cap(sequence.protocolPacks), selectiveReceipts)
		fixture.cancel()
		owner, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if fixture.sender.CloseAndWait(owner) != nil || fixture.receiver.CloseAndWait(owner) != nil {
			t.Fatal("future protocol clients did not join")
		}
	})
	if t.Failed() {
		return
	}
	t.Log("FUTURE_PROTOCOL_ORDER_CLEANUP_JOINED clients=true budgets=true pool=true")
	if refusedEarlier {
		t.Fatal("FUTURE_PROTOCOL_SECOND_ORIGINAL_REFUSED_BEFORE_DATA_PREDECESSOR")
	}
}
