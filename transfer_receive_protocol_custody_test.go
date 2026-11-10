package connect

import "testing"

// Two pending protocol operations must not exclude the next flow's TCP ACK:
// that ACK may be what releases the DATA ownership blocking those operations.
func TestProtocolReceiptsProgressPastUnrelatedPendingControls(t *testing.T) {
	assertMessagePoolOwnership(t)
	queue, _ := testReceiveDeliveryLedger(t, 1)
	data, accepted := queue.append(testReceiveDeliveryItem(0), true)
	if !accepted {
		t.Fatal("initial DATA receipt refused")
	}
	dataClaim := data.hold()
	data.seal()
	queue.maxBytes = queue.bytes
	dataBytes := queue.bytes
	var claims []*receiveDeliveryClaim
	var ids []Id
	for number := uint64(1); number <= 3; number++ {
		item := testReceiveDeliveryTcpControl(number)
		item.noBudget = true
		id := item.messageId
		receipt, accepted := queue.append(item, true)
		if !accepted {
			item.messagePoolReturn()
			t.Fatalf("protocol control %d was blocked by unrelated pending controls under full DATA", number)
		}
		claims = append(claims, receipt.hold())
		ids = append(ids, id)
		receipt.seal()
		if known, secured := queue.duplicate(number, id); !known || secured {
			t.Fatal("retained but unapplied control lost custody or acquired a false delivery ACK")
		}
	}
	if queue.bytes != dataBytes || queue.controlBytes != 0 {
		t.Fatal("protocol receipts consumed the DATA byte ledger")
	}
	claims[2].complete(true)
	if present, _ := reliableIngressCumulativeHead(queue.sequence); present {
		t.Fatal("later applied protocol skipped pending DATA in the cumulative ACK")
	}
	ack := queue.sequence.ackWindow.Snapshot(false)
	if len(ack.selectiveAcks) != 1 || ack.selectiveAcks[ids[2]].messageId != ids[2] {
		t.Fatal("selective delivery evidence did not name only the applied control")
	}
	for index := 0; index < 2; index++ {
		if known, secured := queue.duplicate(uint64(index+1), ids[index]); !known || secured {
			t.Fatal("another flow's applied control falsely secured a pending operation")
		}
	}
	claims[1].complete(true)
	claims[0].complete(true)
	if present, _ := reliableIngressCumulativeHead(queue.sequence); present {
		t.Fatal("completed controls cumulatively acknowledged unapplied DATA")
	}
	dataClaim.complete(true)
	if present, head := reliableIngressCumulativeHead(queue.sequence); !present || head.messageId != ids[2] ||
		len(queue.items) != 0 || queue.bytes != 0 || queue.controlCount != 0 || queue.controlBytes != 0 {
		t.Fatal("actual DATA completion did not retire the exact secured prefix and its owners")
	}
}
