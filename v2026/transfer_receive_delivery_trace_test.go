package connect

import "testing"

func TestReceiveDeliveryTraceNilIsAllocationFree(t *testing.T) {
	q := &receiveDeliveryQueue{sequence: &ReceiveSequence{receiveBufferSettings: DefaultReceiveBufferSettings(), client: &Client{}}}
	r := &receiveDeliveryReceipt{}
	if allocations := testing.AllocsPerRun(100, func() {
		q.traceReceiptLocked(r, "delivery_receipt_wait", false)
	}); allocations != 0 {
		t.Fatalf("disabled receipt trace allocated %g objects", allocations)
	}
}

// A later receipt owns its packet while the earlier one remains pending.
// Trace must not invent a cumulative ACK, merge their identities, or expose
// borrowed packet data. Duplicate claim completion cannot repeat evidence.
func TestReceiveDeliveryTraceDistinguishesSecuredOwnerFromPrefix(t *testing.T) {
	assertMessagePoolOwnership(t)
	q, _ := testReceiveDeliveryLedger(t, 4)
	q.sequence.client = &Client{}
	q.sequence.sequenceId = NewId()
	q.sequence.source = TransferPath{SourceId: NewId()}
	q.sequence.receiveBufferSettings = DefaultReceiveBufferSettings()
	var events []TransferProgressEvent
	q.sequence.receiveBufferSettings.ProgressObserver = func(event TransferProgressEvent) { events = append(events, event) }
	first, _ := q.append(testReceiveDeliveryItem(115), true)
	second, _ := q.append(testReceiveDeliveryItem(116), true)
	firstID, secondID := first.ack.messageId, second.ack.messageId
	firstClaim, secondClaim := first.hold(), second.hold()
	first.seal()
	second.seal()
	secondClaim.complete(true)
	if present, _ := reliableIngressCumulativeHead(q.sequence); present {
		t.Fatal("trace promoted an unowned prefix")
	}
	if ackLineageTraceEnabled {
		last := events[len(events)-1]
		if last.Stage != "delivery_receipt_secured" || last.MessageId != secondID || last.SequenceNumber != 116 || !last.Success || last.QueueLength != 2 {
			t.Fatalf("secured later receipt identity lost: %+v", last)
		}
		for _, event := range events {
			if event.Stage == "delivery_receipt_prefix" || event.SequenceId != q.sequence.sequenceId || event.PeerId != q.sequence.source.SourceId {
				t.Fatalf("prefix escaped or owner identity changed: %+v", event)
			}
		}
	}
	firstClaim.complete(true)
	count := len(events)
	firstClaim.complete(true)
	secondClaim.complete(false)
	if len(events) != count {
		t.Fatal("duplicate completion repeated diagnostic evidence")
	}
	if !ackLineageTraceEnabled {
		if count != 0 {
			t.Fatal("ordinary build emitted receipt diagnostics")
		}
		return
	}
	var prefix []Id
	for _, event := range events {
		if event.Stage == "delivery_receipt_prefix" {
			prefix = append(prefix, event.MessageId)
		}
	}
	if len(prefix) != 2 || prefix[0] != firstID || prefix[1] != secondID {
		t.Fatalf("exact cumulative receipt order lost: %+v", prefix)
	}
}

func TestReceiveDeliveryTraceClassifiesCapacityVsClaimFailure(t *testing.T) {
	for _, cause := range []string{"count", "unclaimed", "claim", "sequence"} {
		t.Run(cause, func(t *testing.T) {
			assertMessagePoolOwnership(t)
			q, _ := testReceiveDeliveryLedger(t, 1)
			q.sequence.client = &Client{}
			q.sequence.sequenceId = NewId()
			q.sequence.receiveBufferSettings = DefaultReceiveBufferSettings()
			var events []TransferProgressEvent
			q.sequence.receiveBufferSettings.ProgressObserver = func(event TransferProgressEvent) { events = append(events, event) }
			r, _ := q.append(testReceiveDeliveryItem(10), true)
			id, number := r.ack.messageId, r.ack.sequenceNumber
			var claim *receiveDeliveryClaim
			if cause != "unclaimed" {
				claim = r.hold()
			}
			r.seal()
			stage := "delivery_receipt_rejected"
			switch cause {
			case "count":
				stage = "delivery_receipt_refused"
				refused := testReceiveDeliveryItem(11)
				id, number = refused.messageId, refused.sequenceNumber
				if _, accepted := q.append(refused, true); accepted {
					t.Fatal("bounded fixture unexpectedly admitted a second owner")
				}
				refused.messagePoolReturn()
				q.cancel()
				claim.complete(false)
			case "claim":
				claim.complete(false)
			case "sequence":
				stage = "delivery_queue_canceled"
				q.cancel()
				claim.complete(false)
			}
			matches := 0
			for _, event := range events {
				if event.Stage != stage {
					continue
				}
				matches++
				if event.Outcome != cause || event.MessageId != id || event.SequenceNumber != number || event.Success || event.QueueLength != 1 || event.QueueCapacity != 1 || event.WireHash != 0 {
					t.Fatalf("receipt cause/identity or scalar-only boundary changed: %+v", event)
				}
			}
			if ackLineageTraceEnabled && matches != 1 || !ackLineageTraceEnabled && len(events) != 0 {
				t.Fatalf("cause trace matches=%d events=%d enabled=%t", matches, len(events), ackLineageTraceEnabled)
			}
		})
	}
}
