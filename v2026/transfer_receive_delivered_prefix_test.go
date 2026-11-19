// Delivered progress reaches the ACK compressor's one draining consumer
// entirely or not at all. A batch delivered by one callback, and a receipt
// run secured by one completion, are each one cumulative prefix: between two
// separate updates the ACK worker could write a stale intermediate head. These
// tests let the consumer run at every update edge, the interleaving a loaded
// race run produced at random.
package connect

import (
	"sync/atomic"
	"testing"
	"testing/synctest"
)

// G's retransmission fills the hole below the stored P, and both drain in one
// callback. The real ACK worker writes at every update edge of that delivery,
// and the hole fill must still be acknowledged once, cumulatively, at P.
func TestReceiveSequenceHoleFillWritesOneCumulativeAckForTheBatch(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		var delivering atomic.Bool
		sequence := newAckGapTestSequence(t, func(settings *ReceiveBufferSettings) {
			// A head that fills a selectively acknowledged hole ends the
			// compression wait, as in production.
			settings.AckGapWakeSelectiveCount = 3
			settings.afterAckWindowUpdateForTest = func(receiveSequenceId) {
				if delivering.Load() {
					// The worker writes whatever head this edge exposes.
					synctest.Wait()
				}
			}
		})
		prefixId := sequence.updateSelective(2)
		if ack := readBurstTailTestAck(t, sequence); Id(ack.MessageId) != prefixId || !ack.Selective {
			t.Fatal("stored P was not selectively acknowledged before the hole fill")
		}

		gapId := NewId()
		names := map[Id]string{gapId: "G(1)", prefixId: "P(2)"}
		delivering.Store(true)
		deliverBurstTailTestBatch(t, sequence, []*receiveItem{
			{
				transferItem: transferItem{messageId: gapId, sequenceNumber: 1, messageByteCount: 64},
				ack:          true, transportType: TransportTypeH1,
			},
			{
				transferItem: transferItem{messageId: prefixId, sequenceNumber: 2, messageByteCount: 64},
				ack:          true, transportType: TransportTypeH1,
			},
		})
		delivering.Store(false)
		ack := readBurstTailTestAck(t, sequence)
		if Id(ack.MessageId) != prefixId || ack.Selective {
			t.Fatalf("hole fill first acknowledged %s selective=%t, want one cumulative ACK at P(2)",
				names[Id(ack.MessageId)], ack.Selective)
		}
		if len(sequence.route) != 0 {
			t.Fatal("one delivered batch wrote more than one acknowledgement")
		}
	})
}

// The later owner finishes first and is acknowledged selectively; the earlier
// owner's completion then secures both receipts at once. A consumer draining
// at every update edge must take only the run's last receipt as a head.
func TestReceiveDeliveryLedgerSecuredRunPublishesOneCumulativeHead(t *testing.T) {
	assertMessagePoolOwnership(t)
	q, _ := testReceiveDeliveryLedger(t, 4)
	window := q.sequence.ackWindow
	var heads []Id
	window.afterUpdateForTest = func() {
		var scratch [ackResponseMaxCount]sequenceAck
		acks, _ := window.takeResponse(scratch[:0], ackResponseMaxCount)
		for _, ack := range acks {
			if !ack.selective {
				heads = append(heads, ack.messageId)
			}
		}
	}
	firstItem, secondItem := testReceiveDeliveryItem(0), testReceiveDeliveryItem(1)
	firstId, secondId := firstItem.messageId, secondItem.messageId
	first, _ := q.append(firstItem, true)
	second, _ := q.append(secondItem, true)
	firstClaim, secondClaim := first.hold(), second.hold()
	first.seal()
	second.seal()
	secondClaim.complete(true)
	if len(heads) != 0 {
		t.Fatal("a later secured receipt advanced the cumulative head over a pending owner")
	}
	firstClaim.complete(true)
	if len(heads) != 1 || heads[0] != secondId {
		names := map[Id]string{firstId: "first", secondId: "second"}
		got := make([]string, 0, len(heads))
		for _, head := range heads {
			got = append(got, names[head])
		}
		t.Fatalf("one completion published cumulative heads %v, want only the second receipt", got)
	}
}
