// DATA admission must not discard an ordered protocol original whose removal
// creates no DATA capacity. Real public originals and retries own every wire.
package connect

import (
	"bytes"
	"context"
	"testing"
	"testing/synctest"
	"time"
)

// The DATA byte window is full while physical RX still has room. An earlier
// DATA original may replace later tentative DATA, but must keep the later pure
// ACK owner and its retry identity. No selective ACK is manufactured here.
func TestProtocolFutureOwnerSurvivesDataWindowEviction(t *testing.T) {
	GetMessagePoolAggregateStats()
	protocolEvicted := false
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		const leafBytes = ByteCount(64 * 1024)
		const dataBytes = ByteCount(128)
		const dataLimit = dataBytes + 1
		root := NewTransferMemoryBudget(2 * leafBytes)
		tx := NewTransferMemoryBudgetWithParent(leafBytes, root)
		rx := NewTransferMemoryBudgetWithParent(leafBytes, root)
		t.Cleanup(func() {
			for _, budget := range []*TransferMemoryBudget{rx, tx, root} {
				assertRetainedBudgetBalance(t, budget)
				if budget.Stats().QuotaByteCount != 0 {
					t.Error("DATA eviction retained shared quota")
				}
			}
		})
		fixture := newWindowRoundFixture(t, func(settings *SendBufferSettings) {
			settings.ProtocolVersion = 2
			settings.ResendQueueBudget = tx
			settings.ResendQueueRetainedByteAccounting = true
			settings.ResendQueueMinByteCount = 0
			settings.ResendQueueMaxByteCount = leafBytes
		}, func(settings *ReceiveBufferSettings) {
			settings.ReceiveQueueBudget = rx
			settings.ReceiveQueueRetainedByteAccounting = true
			settings.ReceiveQueueMinByteCount = 0
			settings.ReceiveQueueMaxByteCount = dataLimit
			settings.ReceiveHoldPolicy = ReceiveHoldCommittedPrefix
			settings.AdvertiseReceiveWindow = false
			settings.EvictionNotice = false
		})
		var originals [5]*windowRoundFrame
		var results [5]protocolPressureCompletion
		send := func(slot int, control bool) *windowRoundFrame {
			var payload []byte
			if !control {
				payload = bytes.Repeat([]byte{byte(0x70 + slot)}, int(dataBytes)-40)
			}
			expected := ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagAck, uint32(9000+slot), payload)
			packet := MessagePoolCopy(expected)
			frame, err := ipPacketFromProviderFrame(packet, 2)
			if err != nil {
				MessagePoolReturn(packet)
				t.Fatal(err)
			}
			var options []any
			if control {
				options = append(options, NoBudget())
			}
			accepted, err := fixture.sender.SendWithTimeoutDetailed(frame, fixture.receiver.ClientId(), results[slot].record, time.Second, options...)
			if !accepted || err != nil {
				MessagePoolReturn(packet)
				t.Fatalf("public DATA eviction source refused slot=%d accepted=%t error=%v", slot, accepted, err)
			}
			wire := fixture.take(fixture.senderOut)
			if wire.pack == nil || wire.pack.Nack || wire.pack.SequenceNumber != uint64(slot) ||
				len(wire.pack.Frames) != 1 || !bytes.Equal(wire.pack.Frames[0].MessageBytes, expected) ||
				protocolOnlyPack(wire.pack) != control {
				t.Fatal("public DATA eviction source changed its identity, reliable order or packet class")
			}
			originals[slot] = wire
			return wire
		}
		warm := send(0, false)
		fixture.forward(warm, fixture.receiverIn)
		fixture.acknowledge()
		sequence := fixture.receiveSequence()
		if fixture.deliveredCount != 1 || results[0].success.Load() != 1 || rx.UsedByteCount() != 0 || tx.UsedByteCount() != 0 {
			t.Fatal("public warm DATA did not deliver, ACK and release its exact owners")
		}
		missing := send(1, false)
		earlier := send(2, false)
		later := send(3, false)
		control := send(4, true)
		for _, wire := range []*windowRoundFrame{earlier, later, control} {
			if wire.pack.Head || !bytes.Equal(wire.pack.SequenceId, missing.pack.SequenceId) {
				t.Fatal("actual missing predecessor did not retain one non-Head ordered generation")
			}
		}
		fixture.forward(later, fixture.receiverIn)
		fixture.forward(control, fixture.receiverIn)
		laterId := RequireIdFromBytes(later.pack.MessageId)
		controlId := RequireIdFromBytes(control.pack.MessageId)
		laterOwner := sequence.receiveQueue.GetByMessageId(laterId)
		controlOwner := sequence.receiveQueue.GetByMessageId(controlId)
		count, heldBytes := sequence.receiveQueue.QueueSize()
		if laterOwner == nil || controlOwner == nil || count != 2 || heldBytes != dataBytes ||
			laterOwner.noBudget || laterOwner.committed || laterOwner.memoryBudget != rx ||
			!controlOwner.noBudget || controlOwner.committed || controlOwner.memoryBudget != nil ||
			controlOwner.MessageByteCount() != 0 || controlOwner.QueueByteCount() != 0 ||
			controlOwner.transferItem.messageByteCount != 40 || sequence.receiveQueue.PeekLast() != controlOwner ||
			sequence.holdPromise.set || sequence.nextSequenceNumber != 1 || len(fixture.receiverOut) != 0 || fixture.deliveredCount != 1 {
			t.Fatal("real later DATA and zero-DATA-cost protocol owner were not retained tentatively at the window cut")
		}
		path, err := FilteredTransferPath(earlier.bytes)
		if err != nil {
			t.Fatal(err)
		}
		scan, status := scanReceiveMemoryPack(earlier.bytes, path)
		if status != receiveMemoryPackScanReady || scan.retainedByteCount > rx.Available() || scan.retainedByteCount > root.Available() ||
			sequence.receiveQueue.CanAddWithQueueByteCountAndCredit(dataBytes, scan.retainedByteCount, dataLimit, scan.retainedByteCount) {
			t.Fatal("earlier DATA did not isolate the DATA window from physical decode capacity")
		}
		before := fixture.receiver.ReceiveStats()
		cutTime := time.Now()
		fixture.forward(earlier, fixture.receiverIn)
		after := fixture.receiver.ReceiveStats()
		kept := sequence.receiveQueue.GetByMessageId(controlId)
		protocolEvicted = kept == nil
		count, heldBytes = sequence.receiveQueue.QueueSize()
		wantCount, wantEvictions := 2, uint64(1)
		if protocolEvicted {
			wantCount, wantEvictions = 1, 2
		}
		if !protocolEvicted && (kept != controlOwner || !kept.noBudget || kept.committed) ||
			sequence.receiveQueue.GetByMessageId(laterId) != nil || sequence.receiveQueue.GetByMessageId(RequireIdFromBytes(earlier.pack.MessageId)) == nil ||
			count != wantCount || heldBytes != dataBytes || rx.UsedByteCount() != scan.retainedByteCount ||
			after.PackHandoffDropCount != before.PackHandoffDropCount || after.ReceiveQueueDropCount != before.ReceiveQueueDropCount ||
			after.ReceiveQueueEvictionCount != before.ReceiveQueueEvictionCount ||
			after.ReceiveQueueTentativeEvictionCount-before.ReceiveQueueTentativeEvictionCount != wantEvictions ||
			len(fixture.receiverOut) != 0 || fixture.deliveredCount != 1 || sequence.nextSequenceNumber != 1 || time.Now() != cutTime {
			t.Fatal("DATA replacement changed unrelated admission, order, ACK or exact physical custody")
		}
		for slot := 1; slot < len(results); slot++ {
			if results[slot].calls.Load() != 0 {
				t.Fatal("held or evicted original acquired a false completion")
			}
		}
		t.Logf("PROTOCOL_DATA_EVICTION_CUT protocol_evicted=%t data_window=%d protocol_data_bytes=0 tentative_evictions=%d retained=%d elapsed=0", protocolEvicted, heldBytes, wantEvictions, count)
		// The real missing prefix and the ordinary DATA victim's real retry
		// drain the queue. Only the old zero-byte eviction needs an additional
		// protocol retry; cleanup never turns its discarded copy into an ACK.
		fixture.forward(missing, fixture.receiverIn)
		fixture.acknowledge()
		if fixture.deliveredCount != 3 || sequence.nextSequenceNumber != 3 || results[4].calls.Load() != 0 {
			t.Fatal("protocol owner passed its still-missing DATA predecessor")
		}
		dataRetry := pressureContractNaturalRetry(t, fixture, later.pack)
		fixture.forward(dataRetry, fixture.receiverIn)
		fixture.acknowledge()
		if protocolEvicted {
			if fixture.deliveredCount != 4 || sequence.nextSequenceNumber != 4 || results[4].calls.Load() != 0 {
				t.Fatal("discarded protocol original gained delivery or ACK before its retry")
			}
			controlRetry := pressureContractNaturalRetry(t, fixture, control.pack)
			fixture.forward(controlRetry, fixture.receiverIn)
			fixture.acknowledge()
		}
		if fixture.deliveredCount != 5 || sequence.nextSequenceNumber != 5 || sequence.receiveQueue.Len() != 0 || root.UsedByteCount() != 0 {
			t.Fatal("actual original/retry delivery did not drain all five public owners")
		}
		for slot := range results {
			if results[slot].calls.Load() != 1 || results[slot].success.Load() != 1 {
				t.Fatalf("source completion changed during actual retry cleanup: slot=%d calls=%d success=%d", slot, results[slot].calls.Load(), results[slot].success.Load())
			}
		}
		fixture.cancel()
		owner, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if fixture.sender.CloseAndWait(owner) != nil || fixture.receiver.CloseAndWait(owner) != nil {
			t.Fatal("DATA eviction fixture Clients did not join")
		}
	})
	if !t.Failed() {
		t.Log("PROTOCOL_DATA_EVICTION_CLEANUP_JOINED clients=true budgets=true pool=true")
		if protocolEvicted {
			t.Error("DATA_WINDOW_PRESSURE_EVICTED_ZERO_DATA_PROTOCOL_OWNER")
		}
	}
}
