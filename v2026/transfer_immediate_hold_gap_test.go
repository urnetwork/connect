// Secured immediate protocol receipts are received ordinals, not missing DATA.
// The public path must promise only actual holes when later DATA arrives.
package connect

import (
	"bytes"
	"testing"
	"testing/synctest"
	"time"
)

// A real reliable DATA hole survives 128 early protocol deliveries. The next
// DATA fits two retained originals; phantom protocol holes must not bar its SACK.
func TestImmediateReceiptTrainDoesNotSpendDataHoldPromise(t *testing.T) {
	testImmediateReceiptDataHoldPromise(t, 128)
}

// The same complete wire path without early protocol delivery establishes the
// one-hole admission premise independently of the new history integration.
func TestOrderedDataHoldPromiseWithoutImmediateTrain(t *testing.T) {
	testImmediateReceiptDataHoldPromise(t, 0)
}

// Borrows no caller buffer: every successful public send transfers its roots.
func testImmediateReceiptDataHoldPromise(t *testing.T, count int) {
	t.Helper()
	// Initialize the process-global pool worker outside the isolated clock.
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		root := NewTransferMemoryBudget(2 * 1024 * 1024)
		tx := NewTransferMemoryBudgetWithParent(1024*1024, root)
		rx := NewTransferMemoryBudgetWithParent(128*1024, root)
		t.Cleanup(func() {
			for _, budget := range []*TransferMemoryBudget{tx, rx, root} {
				assertRetainedBudgetBalance(t, budget)
			}
		})
		f := newWindowRoundFixture(t, func(settings *SendBufferSettings) {
			settings.SequenceBufferSize = 64
			settings.ResendQueueBudget = tx
			settings.ResendQueueRetainedByteAccounting = true
			settings.ResendQueueMinByteCount = 0
		}, func(settings *ReceiveBufferSettings) {
			settings.SequenceBufferSize = 64
			settings.ReceiveQueueMaxByteCount = 4 * 1024 * 1024
			settings.ReceiveQueueBudget = rx
			settings.ReceiveQueueRetainedByteAccounting = true
			settings.ReceiveQueueMinByteCount = 0
			settings.ReceiveHoldPolicy = ReceiveHoldCommittedPrefix
		}, TransportTypeH1)
		warm := f.write(16)
		f.forward(warm, f.receiverIn)
		f.acknowledge()
		missing := f.write(1240)
		receiver := f.receiveSequence()
		base := missing.pack.SequenceNumber
		for index := range count {
			packet := MessagePoolCopy(ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagAck,
				uint32(500000+index), nil))
			frame, err := ipPacketFromProviderFrame(packet, 2)
			if err != nil {
				MessagePoolReturn(packet)
				t.Fatal(err)
			}
			accepted, err := f.sender.SendWithTimeoutDetailed(frame, f.receiver.ClientId(), nil,
				time.Second, NoBudget(), DeliverImmediately())
			if !accepted || err != nil {
				MessagePoolReturn(packet)
				t.Fatalf("protocol send refused: accepted=%t err=%v", accepted, err)
			}
			wire := f.take(f.senderOut)
			if wire.pack == nil || !wire.pack.DeliverImmediately || wire.pack.SequenceNumber != base+1+uint64(index) {
				t.Fatal("protocol train lost its exact reliable ordinal")
			}
			f.forward(wire, f.receiverIn)
			ack := f.take(f.receiverOut)
			if ack.ack == nil || !ack.ack.DeliveredImmediately || !ack.ack.Selective {
				t.Fatal("protocol delivery did not secure an exact terminal receipt")
			}
			f.forward(ack, f.senderIn)
		}
		if receiver.nextSequenceNumber != base || rx.UsedByteCount() != 0 || f.deliveredCount != 1+count {
			t.Fatal("protocol train changed the missing DATA owner or consumed DATA credit")
		}
		f.nextNumber = base + 1 + uint64(count)
		tail := f.write(1240)
		tailId := RequireIdFromBytes(tail.pack.MessageId)
		f.forward(tail, f.receiverIn)
		held := receiver.receiveQueue.GetByMessageId(tailId)
		if held == nil || !held.committed {
			t.Fatalf("IMMEDIATE_RECEIPTS_COUNTED_AS_MISSING_DATA: receipts=%d next=%d tail=%d held=%t promise=%d used=%d available=%d",
				count, receiver.nextSequenceNumber, f.nextNumber-1, held != nil,
				receiver.holdPromise.reservedByteCount, rx.UsedByteCount(), rx.Available())
		}
		if receiver.holdPromise.reservedByteCount != held.queueByteCount {
			t.Fatalf("one real hole has phantom protocol credit: promise=%d original=%d", receiver.holdPromise.reservedByteCount, held.queueByteCount)
		}
		ack := f.take(f.receiverOut)
		if ack.ack == nil || !ack.ack.Selective || !bytes.Equal(ack.ack.MessageId, tailId[:]) {
			t.Fatal("held DATA did not get its actual selective receipt")
		}
		f.forward(ack, f.senderIn)
		f.forward(missing, f.receiverIn)
		f.acknowledge()
		synctest.Wait()
		if receiver.nextSequenceNumber != f.nextNumber || f.ackedCount != 3 || f.deliveredCount != 3+count || rx.UsedByteCount() != 0 {
			t.Fatal("healing one DATA hole did not drain exactly once through secured protocol history")
		}
	})
}
