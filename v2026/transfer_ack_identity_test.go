package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

// Cancellation and a failed frame rewrite can exit before requeueing. The
// detached slot must lose its identity before its retained owner is released.
func TestTransferAckHeadRewriteClearsIdentityOnExit(t *testing.T) {
	for _, exit := range []string{"cancel", "rewrite_error"} {
		t.Run(exit, func(t *testing.T) {
			sequence := runTransferAckDuringHeadRewrite(t, TransportTypeH1, 2, exit)
			if sequence.detachedAck != (sendAckIdentity{}) {
				t.Fatal("terminal rewrite retained an ACK identity after owner release")
			}
		})
	}
}

// The one-slot bound is an owner invariant, not permission to overwrite an
// outstanding identity if a new recovery branch ever nests another rewrite.
func TestRetainedAckIdentityCannotOverwriteDetachedOwner(t *testing.T) {
	sequence := &SendSequence{resendQueue: newResendQueue(nil, 0)}
	first := &sendItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: 1}}
	second := &sendItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: 2}}
	sequence.resendQueue.Add(first)
	sequence.resendQueue.Add(second)
	sequence.detachResendItem(first.messageId)
	var caught any
	func() {
		defer func() { caught = recover() }()
		sequence.detachResendItem(second.messageId)
	}()
	if caught == nil {
		t.Fatal("nested rewrite overwrote an outstanding ACK identity")
	}
	if number, ok := sequence.retainedAckSequenceNumber(first.messageId); !ok || number != 1 {
		t.Fatal("nested rewrite lost the first retained ACK identity")
	}
	if sequence.resendQueue.GetByMessageId(second.messageId) != second {
		t.Fatal("rejected nested rewrite removed the second owner")
	}
	sequence.resendQueue.Clear()
}

// A recovery changes its carrier before it rejoins the heap. Promoting the
// old lane's successor is genuinely nested work and must preserve both ACK
// identities without a second detached slot or an overwritten first slot.
func TestRetainedAckIdentitySurvivesNestedLanePromotion(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		sequence, items, _ := newLaneHeadPromotionSequence(t)
		head, next := items[0], items[1]
		sequence.detachResendItem(head.messageId)
		before := sequence.detachedAck
		wantDue := time.Now().Add(sequence.rttWindow.ProbeRtt())
		sequence.observeCarrierWrite(head, transferWriteDisposition{route: make(Route, 4), reliable: true})
		if sequence.detachedAck != before || next.resendTime != wantDue {
			t.Fatal("nested lane promotion lost its parent's ACK identity or successor schedule")
		}
		for _, item := range []*sendItem{head, next} {
			if number, ok := sequence.retainedAckSequenceNumber(item.messageId); !ok || number != item.sequenceNumber {
				t.Fatal("nested lane promotion withdrew a retained ACK identity")
			}
		}
		sequence.addResendItem(head)
		if sequence.detachedAck != (sendAckIdentity{}) {
			t.Fatal("requeue did not clear the detached slot")
		}
	})
}

// Repeated rewrites keep one fixed identity and reuse the existing queue and
// ACK compressor. No auxiliary entry is allocated per rewrite or feedback.
func TestRetainedAckIdentityRewriteDoesNotAllocate(t *testing.T) {
	sequence := &SendSequence{client: &Client{}, resendQueue: newResendQueue(nil, 0)}
	item := &sendItem{
		transferItem: transferItem{messageId: NewId(), sequenceNumber: 7},
		expectsAck:   true, sendTime: time.Now(), ackTimeout: 30 * time.Second,
	}
	sequence.addResendItem(item)
	window := newSequenceAckWindow()
	ack := receiveAckMessage{messageId: item.messageId}
	allocs := testing.AllocsPerRun(1000, func() {
		sequence.detachResendItem(item.messageId)
		sequence.coalesceReceivedAck(window, ack)
		sequence.addResendItem(item)
		if snapshot := window.Snapshot(true); snapshot.ackUpdateCount != 1 || snapshot.headAck.messageId != item.messageId {
			t.Fatal("rewrite lost its exact feedback")
		}
	})
	if allocs != 0 {
		t.Fatalf("rewrite ACK identity allocated %g objects", allocs)
	}
	sequence.ackLifetimes.clear()
	sequence.resendQueue.Clear()
}
