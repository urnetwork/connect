package connect

import (
	"testing"
	"time"
)

func TestSequenceAckWindowCoalescesReusableNotification(t *testing.T) {
	window := newSequenceAckWindow()
	empty := window.Snapshot(false)

	window.Update(sequenceAck{sequenceNumber: 1, messageId: NewId()})
	select {
	case <-empty.ackNotify:
	case <-time.After(time.Second):
		t.Fatal("ack update did not notify snapshot waiter")
	}

	first := window.Snapshot(true)
	if first.ackUpdateCount != 1 {
		t.Fatalf("first snapshot update count = %d, want 1", first.ackUpdateCount)
	}
	select {
	case <-first.ackNotify:
		t.Fatal("reset left a stale ack notification")
	default:
	}

	window.Update(sequenceAck{sequenceNumber: 2, messageId: NewId()})
	window.Update(sequenceAck{sequenceNumber: 3, messageId: NewId()})
	select {
	case <-first.ackNotify:
	default:
		t.Fatal("coalesced updates did not notify")
	}
	select {
	case <-first.ackNotify:
		t.Fatal("coalesced updates queued more than one notification")
	default:
	}

	coalesced := window.Snapshot(true)
	if coalesced.ackUpdateCount != 2 {
		t.Fatalf("coalesced snapshot update count = %d, want 2", coalesced.ackUpdateCount)
	}
}

func TestSequenceAckWindowSteadyStateDoesNotAllocate(t *testing.T) {
	window := newSequenceAckWindow()
	ack := sequenceAck{
		sequenceNumber: 1,
		messageId:      NewId(),
		tag:            sequenceTag{sendTime: 1_700_000_000_000, set: true},
	}

	allocs := testing.AllocsPerRun(1000, func() {
		window.Update(ack)
		window.Snapshot(true)
	})
	if allocs != 0 {
		t.Fatalf("ack update + reset allocated %.0f times, want 0", allocs)
	}
}

func TestSequenceAckWindowPreservesCompactRecoveryCapability(t *testing.T) {
	window := newSequenceAckWindow()
	window.Update(sequenceAck{
		sequenceNumber:                   1,
		messageId:                        NewId(),
		compactContractRecoverySupported: true,
	})
	window.Update(sequenceAck{
		sequenceNumber: 2,
		messageId:      NewId(),
	})

	snapshot := window.Snapshot(true)
	if snapshot.ackUpdateCount != 2 ||
		!snapshot.headAck.compactContractRecoverySupported {
		t.Fatalf("coalesced capability snapshot=%+v", snapshot)
	}
}

func TestSequenceAckWindowTracksCarrierOfLatestAckTrigger(t *testing.T) {
	window := newSequenceAckWindow()
	window.Update(sequenceAck{
		sequenceNumber: 1,
		messageId:      NewId(),
		transportType:  TransportTypeH1,
	})
	window.Update(sequenceAck{
		sequenceNumber: 2,
		messageId:      NewId(),
		transportType:  TransportTypeH3,
	})
	if got := window.Snapshot(true).headAck.transportType; got != TransportTypeH3 {
		t.Fatalf("coalesced head carrier=%s, want H3", got)
	}

	// A retransmit below the cumulative head causes that head ACK to be sent
	// again, but cannot move an ACK covering newer H3 data onto the old H1 lane.
	window.Update(sequenceAck{
		sequenceNumber: 1,
		messageId:      NewId(),
		transportType:  TransportTypeH1,
	})
	if got := window.Snapshot(true).headAck.transportType; got != TransportTypeH3 {
		t.Fatalf("retransmit-triggered head carrier=%s, want preserved H3", got)
	}

	selectiveId := NewId()
	window.Update(sequenceAck{
		sequenceNumber: 3,
		messageId:      selectiveId,
		selective:      true,
		transportType:  TransportTypeH3,
	})
	window.Update(sequenceAck{
		sequenceNumber: 3,
		messageId:      selectiveId,
		selective:      true,
		transportType:  TransportTypeUnknown,
	})
	snapshot := window.Snapshot(true)
	if got := snapshot.selectiveAcks[selectiveId].transportType; got != TransportTypeH3 {
		t.Fatalf("selective ACK carrier=%s, want preserved H3", got)
	}
}
