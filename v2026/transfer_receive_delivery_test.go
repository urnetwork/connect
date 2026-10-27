package connect

import (
	"context"
	"sync"
	"testing"

	"github.com/urnetwork/connect/v2026/protocol"
)

func testReceiveDeliveryLedger(t *testing.T, count int) (*receiveDeliveryQueue, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	sequence := &ReceiveSequence{ctx: ctx, cancel: cancel, ackWindow: newSequenceAckWindow()}
	queue := newReceiveDeliveryQueue(sequence, count, 4096)
	t.Cleanup(func() { cancel(); queue.cancel() })
	return queue, ctx
}

func testReceiveDeliveryItem(number uint64) *receiveItem {
	return &receiveItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: number, queueByteCount: 256},
		ack: true, frames: []*protocol.Frame{{Raw: true, MessageBytes: MessagePoolCopy([]byte("owned original"))}}}
}

func TestReceiveDeliveryLedgerSecuredPrefixAndDuplicate(t *testing.T) {
	assertMessagePoolOwnership(t)
	q, _ := testReceiveDeliveryLedger(t, 4)
	firstItem, secondItem := testReceiveDeliveryItem(0), testReceiveDeliveryItem(1)
	firstID, secondID := firstItem.messageId, secondItem.messageId
	first, _ := q.append(firstItem, true)
	second, _ := q.append(secondItem, true)
	firstClaim, secondClaim := first.hold(), second.hold()
	first.seal()
	second.seal()
	secondClaim.complete(true)
	if present, _ := reliableIngressCumulativeHead(q.sequence); present {
		t.Fatal("later secured control cumulatively ACKed the pending data")
	}
	if known, secured := q.duplicate(0, firstID); !known || secured {
		t.Fatal("pending exact duplicate escaped the delivery ledger")
	}
	if known, secured := q.duplicate(1, secondID); !known || !secured {
		t.Fatal("secured later item lost its selective lease")
	}
	ack := q.sequence.ackWindow.Snapshot(false)
	if ack.selectiveAcks[secondID].messageId != secondID || len(ack.selectiveAcks) != 1 {
		t.Fatal("selective ACK did not name only the secured later item")
	}
	firstClaim.complete(true)
	if present, head := reliableIngressCumulativeHead(q.sequence); !present || head.messageId != secondID {
		t.Fatal("complete secured prefix did not advance exactly once")
	}
	firstClaim.complete(true)
	secondClaim.complete(false)
	if len(q.items) != 0 || q.bytes != 0 || q.sequence.ackWindow.Snapshot(false).ackUpdateCount != 2 {
		t.Fatal("duplicate completion changed delivery or retained ownership")
	}
}

func TestReceiveDeliveryLedgerSealBeforePoolReturn(t *testing.T) {
	assertMessagePoolOwnership(t)
	q, _ := testReceiveDeliveryLedger(t, 1)
	item := testReceiveDeliveryItem(0)
	r, _ := q.append(item, true)
	claim := r.hold()
	claim.complete(true)
	if len(item.frames) != 1 || q.sequence.ackWindow.Pending() {
		t.Fatal("synchronous completion returned a still-borrowed callback item")
	}
	r.seal()
	if item.frames != nil || !q.sequence.ackWindow.Pending() {
		t.Fatal("callback return did not secure and release the exact item")
	}
}

func TestReceiveDeliveryLedgerFailureAndCancellationNeverPromote(t *testing.T) {
	for _, cause := range []string{"downstream-refusal", "cancel", "missing-owner"} {
		t.Run(cause, func(t *testing.T) {
			assertMessagePoolOwnership(t)
			q, ctx := testReceiveDeliveryLedger(t, 2)
			item := testReceiveDeliveryItem(0)
			id := item.messageId
			r, _ := q.append(item, true)
			var claim *receiveDeliveryClaim
			if cause != "missing-owner" {
				claim = r.hold()
			}
			r.seal()
			switch cause {
			case "cancel":
				q.cancel()
				claim.complete(true)
			case "downstream-refusal":
				claim.complete(false)
			}
			if q.sequence.ackWindow.Pending() || item.frames != nil || len(q.items) != 0 || q.bytes != 0 {
				t.Fatal("failed receipt ACKed or leaked its original item")
			}
			if known, secured := q.duplicate(0, id); !known || secured {
				t.Fatal("duplicate of failed original could invent an ACK")
			}
			if cause != "cancel" && ctx.Err() == nil {
				t.Fatal("permanent downstream refusal did not close the receive owner")
			}
		})
	}
}

func TestReceiveDeliveryLedgerCancelJoinsBorrowedAndPendingOwners(t *testing.T) {
	assertMessagePoolOwnership(t)
	q, _ := testReceiveDeliveryLedger(t, 2)
	borrowed := testReceiveDeliveryItem(0)
	pending := testReceiveDeliveryItem(1)
	first, _ := q.append(borrowed, true)
	second, _ := q.append(pending, true)
	firstClaim, secondClaim := first.hold(), second.hold()
	second.seal()
	q.cancel()
	if len(q.items) != 2 || borrowed.frames == nil || pending.frames == nil {
		t.Fatal("cancel returned ownership still borrowed by callback/downstream")
	}
	firstClaim.complete(false)
	if borrowed.frames == nil {
		t.Fatal("claim completion returned a callback's still-borrowed frame")
	}
	secondClaim.complete(false)
	if pending.frames != nil || len(q.items) != 1 {
		t.Fatal("cancel did not release the joined downstream owner")
	}
	first.seal()
	first.seal()
	if borrowed.frames != nil || q.items != nil || q.bytes != 0 || q.sequence.ackWindow.Pending() {
		t.Fatal("final callback join retained ledger/backing or emitted an ACK")
	}
}

func TestReceiveDeliveryLedgerCapacityAndConcurrentCompletion(t *testing.T) {
	assertMessagePoolOwnership(t)
	q, _ := testReceiveDeliveryLedger(t, 2)
	first, _ := q.append(testReceiveDeliveryItem(0), true)
	second, _ := q.append(testReceiveDeliveryItem(1), true)
	rejected := testReceiveDeliveryItem(2)
	if _, ok := q.append(rejected, true); ok {
		t.Fatal("bounded receipt ledger admitted too many owners")
	}
	rejected.messagePoolReturn()
	firstClaim, secondClaim := first.hold(), second.hold()
	first.seal()
	second.seal()
	var workers sync.WaitGroup
	for range 20 {
		workers.Go(func() { firstClaim.complete(true) })
		workers.Go(func() { secondClaim.complete(true) })
	}
	workers.Wait()
	if len(q.items) != 0 || q.bytes != 0 || q.sequence.ackWindow.Snapshot(false).ackUpdateCount != 2 {
		t.Fatal("racing completion lost the exact secured prefix")
	}
}

func testReceiveDeliveryTcpControl(number uint64) *receiveItem {
	packet := MessagePoolCopy(ipOosTcpPacketSequence(icmpTcpTestPath(4), tcpFlagAck, 101, nil))
	return &receiveItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: number,
		queueByteCount: MessagePoolRootByteCount(packet)}, ack: true,
		frames: []*protocol.Frame{{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: packet}}}
}

func TestReceiveDeliveryLedgerFullDataStillAdmitsBoundedControl(t *testing.T) {
	assertMessagePoolOwnership(t)
	q, _ := testReceiveDeliveryLedger(t, 1)
	data, _ := q.append(testReceiveDeliveryItem(0), true)
	dataClaim := data.hold()
	data.seal()
	q.maxBytes = q.bytes
	controlItem := testReceiveDeliveryTcpControl(1)
	control, ok := q.append(controlItem, true)
	if !ok {
		controlItem.messagePoolReturn()
		dataClaim.complete(false)
		t.Fatal("full data count/bytes refused the TCP control needed to release it")
	}
	controlClaim := control.hold()
	control.seal()
	controlClaim.complete(true)
	if present, _ := reliableIngressCumulativeHead(q.sequence); present {
		t.Fatal("reserved control cumulatively ACKed pending data")
	}
	dataClaim.complete(true)
}

func TestReceiveDeliveryLedgerHealthyControlsUseRegularCapacity(t *testing.T) {
	assertMessagePoolOwnership(t)
	q, _ := testReceiveDeliveryLedger(t, 4)
	q.maxBytes = 64 * 1024
	var claims []*receiveDeliveryClaim
	for number := uint64(0); number < 7; number++ {
		item := testReceiveDeliveryTcpControl(number)
		r, ok := q.append(item, true)
		if number == 6 {
			if ok {
				t.Fatal("control burst escaped combined regular-plus-reserve count bound")
			}
			item.messagePoolReturn()
			continue
		}
		if !ok {
			item.messagePoolReturn()
			t.Fatalf("ordinary bounded control capacity was unavailable at item %d", number)
		}
		claims = append(claims, r.hold())
		r.seal()
		if r.controlReserve != (number >= 4) || q.controlCount != max(0, int(number)-3) {
			t.Fatal("healthy controls spent the fallback reserve before regular capacity was full")
		}
	}
	if ack := q.sequence.ackWindow.Snapshot(false); ack.ackUpdateCount != 0 || len(ack.selectiveAcks) != 0 {
		t.Fatal("unsecured control burst emitted ownership evidence")
	}
	q.cancel()
	for _, claim := range claims {
		claim.complete(false)
	}
	if q.bytes != 0 || q.controlBytes != 0 || q.controlCount != 0 || len(q.items) != 0 {
		t.Fatal("cancellation leaked combined regular/control capacity")
	}
}

func TestReceiveDeliveryLedgerControlReserveIsBoundedAndReusable(t *testing.T) {
	assertMessagePoolOwnership(t)
	q, _ := testReceiveDeliveryLedger(t, 1)
	q.sequence.ackWindow = newSequenceAckWindowWithGapWake(3)
	data, _ := q.append(testReceiveDeliveryItem(0), true)
	dataClaim := data.hold()
	data.seal()
	q.maxBytes = q.bytes
	var firstControlID, lastControlID Id
	for number := uint64(1); number <= 64; number++ {
		item := testReceiveDeliveryTcpControl(number)
		lastControlID = item.messageId
		if number == 1 {
			firstControlID = item.messageId
		}
		r, ok := q.append(item, true)
		if !ok {
			item.messagePoolReturn()
			dataClaim.complete(false)
			t.Fatalf("applied control did not make bounded room for successor %d", number)
		}
		claim := r.hold()
		r.seal()
		claim.complete(true)
		if q.controlCount > 2 || q.controlBytes > receiveDeliveryControlMaxBytes || len(q.items) > 3 {
			t.Fatal("control stream grew beyond its fixed reserve")
		}
		if len(q.sequence.ackWindow.Snapshot(false).selectiveAcks) > 2 {
			t.Fatal("compacted control retained unbounded pending ACK metadata")
		}
		q.sequence.ackWindow.ackLock.Lock()
		maximum, evidence := q.sequence.ackWindow.gapSelectiveMax, len(q.sequence.ackWindow.gapEvidence)
		q.sequence.ackWindow.ackLock.Unlock()
		if maximum != number || evidence > 3 {
			t.Fatalf("compacted gap evidence drifted: maximum=%d count=%d, latest=%d", maximum, evidence, number)
		}
	}
	if known, secured := q.duplicate(1, firstControlID); !known || secured {
		t.Fatal("compacted identity could escape through the cumulative duplicate shortcut")
	}
	if present, _ := reliableIngressCumulativeHead(q.sequence); present {
		t.Fatal("control stream cumulatively jumped pending data")
	}
	dataClaim.complete(true)
	if present, head := reliableIngressCumulativeHead(q.sequence); !present || head.messageId != lastControlID || q.bytes != 0 || q.controlBytes != 0 || q.controlCount != 0 {
		t.Fatal("secured prefix failed to release the bounded control span")
	}
}

func TestReceiveDeliveryLedgerControlReserveRejectsDataAndExcess(t *testing.T) {
	assertMessagePoolOwnership(t)
	q, _ := testReceiveDeliveryLedger(t, 1)
	data, _ := q.append(testReceiveDeliveryItem(0), true)
	dataClaim := data.hold()
	data.seal()
	q.maxBytes = q.bytes
	var controls []*receiveDeliveryClaim
	for number := uint64(1); number <= 3; number++ {
		item := testReceiveDeliveryTcpControl(number)
		r, ok := q.append(item, true)
		if number == 3 {
			if ok {
				t.Fatal("third pending control escaped the two-owner bound")
			}
			item.messagePoolReturn()
			continue
		}
		if !ok {
			item.messagePoolReturn()
			t.Fatal("reserved control admission failed")
		}
		controls = append(controls, r.hold())
		r.seal()
	}
	packet := MessagePoolCopy(ipOosTcpPacketSequence(icmpTcpTestPath(4), tcpFlagAck, 101, []byte("not a control")))
	dataItem := &receiveItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: 4, queueByteCount: 256}, ack: true,
		frames: []*protocol.Frame{{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: packet}}}
	if _, ok := q.append(dataItem, true); ok {
		t.Fatal("TCP payload spent the independent control reserve")
	}
	dataItem.messagePoolReturn()
	if ack := q.sequence.ackWindow.Snapshot(false); ack.ackUpdateCount != 0 || len(ack.selectiveAcks) != 0 {
		t.Fatal("reserved but unsecured controls produced feedback")
	}
	if wakes := q.pump(); len(wakes) != 0 {
		t.Fatal("uncompleted passive claims invented retry wake sources")
	}
	q.cancel()
	dataClaim.complete(false)
	for _, claim := range controls {
		claim.complete(false)
	}
	if q.bytes != 0 || q.controlBytes != 0 || q.controlCount != 0 || len(q.items) != 0 {
		t.Fatal("canceled reserve retained packet/count/byte ownership")
	}
}
