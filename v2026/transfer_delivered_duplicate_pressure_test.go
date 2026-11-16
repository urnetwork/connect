// Losing an ACK must not make already-delivered DATA require application credit again.
package connect

import (
	"bytes"
	"github.com/urnetwork/connect/v2026/protocol"
	"testing"
	"testing/synctest"
)

func TestDeliveredDuplicateRecoversLostAckUnderReceivePressure(t *testing.T) {
	testDeliveredDuplicateReceivePressure(t, true)
}

func TestDeliveredDuplicateRecoversLostAckWithoutReceivePressure(t *testing.T) {
	testDeliveredDuplicateReceivePressure(t, false)
}

func testDeliveredDuplicateReceivePressure(t *testing.T, pressure bool) {
	t.Helper()
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		root := NewTransferMemoryBudget(1024 * 1024)
		rx := NewTransferMemoryBudgetWithParent(128*1024, root)
		recovery := NewTransferMemoryBudgetWithParent(128*1024, root)
		t.Cleanup(func() {
			for _, b := range []*TransferMemoryBudget{rx, recovery, root} {
				assertRetainedBudgetBalance(t, b)
			}
		})
		f := newWindowRoundFixture(t, nil, func(s *ReceiveBufferSettings) {
			s.ReceiveQueueBudget = rx
			s.ReceiveQueueRetainedByteAccounting = true
			s.ReceiveQueueMinByteCount = 0
			s.ReceiveHeadRecoveryBudget = recovery
		}, TransportTypeH1)
		original := f.write(1240)
		retry := &windowRoundFrame{bytes: MessagePoolCopy(original.bytes), pack: original.pack}
		f.heldFrames = append(f.heldFrames, retry)
		f.forward(original, f.receiverIn)
		lostAck := f.take(f.receiverOut)
		if lostAck.ack == nil || f.deliveredCount != 1 {
			t.Fatal("original did not reach delivered state")
		}
		f.drop(lostAck)
		if rx.UsedByteCount() != 0 {
			t.Fatal("delivered original retained DATA credit")
		}
		if pressure {
			if !rx.TryReserve(rx.TotalByteCount()) {
				t.Fatal("could not establish foreign DATA pressure")
			}
			defer rx.Release(rx.TotalByteCount())
		}
		f.forward(retry, f.receiverIn)
		select {
		case body := <-f.receiverOut:
			var wire protocol.TransferFrame
			if err := ProtoUnmarshal(body, &wire); err != nil || wire.Ack == nil || !bytes.Equal(wire.Ack.SequenceId, original.pack.SequenceId) || wire.Ack.Selective || !bytes.Equal(wire.Ack.MessageId, original.pack.MessageId) {
				MessagePoolReturn(body)
				t.Fatal("repeat response was not the exact lost ACK")
			}
			response := &windowRoundFrame{bytes: body, ack: wire.Ack}
			f.heldFrames = append(f.heldFrames, response)
			f.forward(response, f.senderIn)
			if f.ackedCount != 1 {
				t.Fatal("repeat ACK did not settle original sender exactly once")
			}
		default:
			t.Fatalf("DELIVERED_DUPLICATE_ACK_BLOCKED_BY_DATA_PRESSURE: pressure=%t floor=%d delivered=%d rx=%d/%d recovery=%d/%d", pressure, f.receiveSequence().nextSequenceNumber, f.deliveredCount, rx.UsedByteCount(), rx.TotalByteCount(), recovery.UsedByteCount(), recovery.TotalByteCount())
		}
		if f.deliveredCount != 1 {
			t.Fatal("duplicate delivered twice")
		}
	})
}
