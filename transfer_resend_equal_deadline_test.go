// Equal retry deadlines keep the ordered receiver's earliest hole runnable.
package connect

import (
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Four retained holes and three real tail identities share one sequence.
// Reverse insertion is a legal heap topology after earlier retry retiming;
// the actual scoreboard then detaches/reinserts all four proved holes.
func newEqualDeadlineRecoverySequence(t *testing.T) (*SendSequence, []*sendItem, time.Time) {
	t.Helper()
	base := time.Unix(1_700_000_000, 0)
	sequence, items := newSelectiveAckRecoveryTestSequence(7, base)
	sequence.resendQueue.Clear()
	sequence.client = &Client{}
	sequence.log = NewNoopLogger()
	sequence.sequenceId = NewId()
	sequence.ackWindow = newSequenceAckWindow()
	var pooled [7]bool
	t.Cleanup(func() {
		sequence.ackLifetimes.clear()
		sequence.resendQueue.Clear()
		// Also own a partially built or temporarily detached frame on failure.
		for index, item := range items {
			if pooled[index] {
				item.messagePoolReturn()
			}
		}
	})
	for index, item := range items {
		item.sequenceNumber = uint64(107 + index)
		item.expectsAck = true
		item.acks.add(sendAckRecord{retainAfterAckTimeout: true})
		item.ackTimeout = time.Second
		item.resendTime = base
		if index >= 4 {
			item.resendTime = base.Add(time.Hour)
			item.selectiveAcked = true
		}
		frame, err := ProtoMarshal(&protocol.TransferFrame{Pack: &protocol.Pack{
			MessageId: item.messageId.Bytes(), SequenceId: sequence.sequenceId.Bytes(),
			SequenceNumber: item.sequenceNumber,
		}})
		if err != nil {
			t.Fatal(err)
		}
		item.transferFrameBytes = frame
		pooled[index] = true
	}
	for _, index := range []int{3, 2, 1, 0, 4, 5, 6} {
		sequence.addResendItem(items[index])
	}
	return sequence, items, base.Add(10 * time.Second)
}

// The semantic assertion is the next actual recovery selection, not the
// stable ordering of equal keys inside a heap. Old code selects hole 109.
func TestResendEqualDeadlineSelectsEarliestRetainedHole(t *testing.T) {
	assertMessagePoolOwnership(t)
	sequence, items, now := newEqualDeadlineRecoverySequence(t)
	if sequence.sendBufferSettings.SelectiveAckGapThreshold != 3 ||
		sequence.sendBufferSettings.SelectiveAckGapBurstSize != 4 {
		t.Fatal("fixture requires the default three-receipt/four-hole scoreboard")
	}
	beforeBytes := sequence.resendQueue.byteCount
	beforeQueueBytes := sequence.resendQueue.queueByteCount
	sequence.scheduleSelectiveAckRecovery(now)
	for index, item := range items {
		if item.selectiveGapRecovered != (index < 4) ||
			(index < 4 && item.recoveryKind != sendRecoverySelectiveGap) ||
			item.sendCount != 1 || item.deliveryObserved || item.serviceCreditObserved ||
			item.ackLifetimeIndex != 0 || sequence.resendQueue.GetByMessageId(item.messageId) != item {
			t.Fatalf("scoreboard changed retry, acknowledgement, or retained ownership at %d", item.sequenceNumber)
		}
	}
	if sequence.resendQueue.byteCount != beforeBytes || sequence.resendQueue.queueByteCount != beforeQueueBytes {
		t.Fatal("scoreboard changed retained byte ownership")
	}
	selected := sequence.resendQueue.PeekFirst()
	selection, outcome, err := sequence.selectDueRecovery(selected, now, transferFlightPolicySnapshot{}, sequence.ackWindow)
	if err != nil || outcome != sendRecoverySelectionReady {
		t.Fatalf("recovery selection failed: outcome=%d error=%v", outcome, err)
	}
	if selection.messageId != items[0].messageId || selection.number != 107 || selection.kind != sendRecoverySelectiveGap {
		t.Fatalf("equal-deadline recovery bypassed earliest retained hole: selected=%d want=107 kind=%d", selection.number, selection.kind)
	}
	head := items[0]
	if !head.head || !head.promotedHead || head.recoveryKind != sendRecoveryNone ||
		!head.selectiveGapRecovered || head.sendCount != 1 || head.ackLifetimeIndex != 0 ||
		sequence.resendQueue.GetByMessageId(head.messageId) != head || len(sequence.ackLifetimes.items) != 0 {
		t.Fatal("selected retained head lost promotion or independent lifetime ownership")
	}
	var frame protocol.TransferFrame
	if err := ProtoUnmarshal(head.transferFrameBytes, &frame); err != nil || frame.Pack == nil || !frame.Pack.Head {
		t.Fatalf("selected head was not rewritten through the real frame path: %v", err)
	}
	sequence.pendingRecovery = &selection
	if sequence.selectedRecoveryItem() != head {
		t.Fatal("head selection lost its exact queued retry owner")
	}
	for _, item := range items[1:4] {
		if item.recoveryKind != sendRecoverySelectiveGap || item.head || item.promotedHead {
			t.Fatal("selecting the first hole consumed or promoted a later recovery")
		}
	}
}

// An unrelated tail receipt cannot defer the earliest hole; an exact receipt
// can. Pending feedback must win without consuming its selective marker.
func TestResendEqualDeadlineKeepsExactAckPreemption(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, exact := range []bool{false, true} {
		sequence, items, now := newEqualDeadlineRecoverySequence(t)
		sequence.scheduleSelectiveAckRecovery(now)
		head := items[0]
		ackItem := items[6]
		if exact {
			ackItem = head
		}
		sequence.ackWindow.Update(sequenceAck{messageId: ackItem.messageId, sequenceNumber: ackItem.sequenceNumber, selective: true})
		selection, outcome, err := sequence.selectDueRecovery(head, now, transferFlightPolicySnapshot{}, sequence.ackWindow)
		if err != nil {
			t.Fatal(err)
		}
		if exact {
			if outcome != sendRecoverySelectionFeedback || head.recoveryKind != sendRecoverySelectiveGap || head.head || head.promotedHead {
				t.Fatal("exact pending delivery did not preempt unchanged recovery")
			}
		} else if outcome != sendRecoverySelectionReady || selection.messageId != head.messageId || !head.promotedHead {
			t.Fatal("unrelated tail feedback hid the first hole")
		}
		if head.deliveryObserved || head.serviceCreditObserved || head.sendCount != 1 ||
			sequence.resendQueue.GetByMessageId(head.messageId) != head ||
			!sequence.ackWindow.PendingDispositionFor(ackItem.sequenceNumber, ackItem.messageId) {
			t.Fatal("selection consumed delivery or changed retry ownership")
		}
	}
}

// Absolute due time stays primary, including zero. Unsigned comparisons keep
// equal deadlines ordered correctly across the full sequence-number range.
func TestResendEqualDeadlineKeepsEarlierTimesAndUnsignedOrder(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	for _, test := range []struct {
		name         string
		early        time.Time
		late         time.Time
		firstNumber  uint64
		secondNumber uint64
	}{
		{name: "earlier deadline wins", early: base, late: base.Add(time.Nanosecond), firstNumber: ^uint64(0), secondNumber: 0},
		{name: "zero remains earliest", early: time.Time{}, late: base, firstNumber: ^uint64(0), secondNumber: 0},
		{name: "unsigned range", early: base, late: base, firstNumber: 0, secondNumber: ^uint64(0)},
		{name: "upper boundary", early: base, late: base, firstNumber: ^uint64(0) - 1, secondNumber: ^uint64(0)},
	} {
		sequence, items := newSelectiveAckRecoveryTestSequence(2, base)
		sequence.resendQueue.Clear()
		items[0].sequenceNumber, items[1].sequenceNumber = test.firstNumber, test.secondNumber
		items[0].resendTime, items[1].resendTime = test.early, test.late
		sequence.addResendItem(items[1])
		sequence.addResendItem(items[0])
		for range 3 {
			if sequence.resendQueue.PeekFirst() != items[0] || sequence.resendQueue.PeekLast() != items[1] {
				t.Fatalf("%s: earliest/latest retry ordering changed", test.name)
			}
			if sequence.detachResendItem(items[0].messageId) != items[0] {
				t.Fatal("exact owner could not detach")
			}
			if number, ok := sequence.retainedAckSequenceNumber(items[0].messageId); !ok || number != test.firstNumber {
				t.Fatal("heap detachment lost ACK identity")
			}
			sequence.addResendItem(items[0])
			if !sequence.setResendTimeIfRetained(items[0], test.early) || sequence.resendQueue.GetBySequenceNumber(test.firstNumber) != items[0] {
				t.Fatal("heap repair lost exact sequence identity")
			}
		}
		sequence.resendQueue.Clear()
	}
}

// A deadline tie is not permission to alter an ordinary ACK expiry or enroll
// a deferred/recovered item again. Each sequence retains its own heap.
func TestResendEqualDeadlineKeepsLifetimesAndDeferredKinds(t *testing.T) {
	assertMessagePoolOwnership(t)
	sequence, items, now := newEqualDeadlineRecoverySequence(t)
	ordinary := items[1]
	ordinary.acks = sendAckSet{}
	ordinary.ackTimeout = 20 * time.Second
	sequence.ackLifetimes.update(ordinary)
	ordinaryDeadline := ordinary.sendTime.Add(ordinary.ackTimeout)
	deferred := items[2]
	deferred.deferralOutstanding = true
	sequence.sendBufferSettings.DeferredItemIsLateForTheScoreboard = true
	sequence.setResendTime(deferred, now.Add(time.Second))
	alreadyRecovered := items[3]
	alreadyRecovered.selectiveGapRecovered = true
	alreadyRecovered.recoveryKind = sendRecoveryAckTailProbe
	sequence.scheduleSelectiveAckRecovery(now)
	if len(sequence.ackLifetimes.items) != 1 || sequence.ackLifetimes.items[0].item != ordinary ||
		sequence.ackLifetimes.items[0].at != ordinaryDeadline || ordinary.ackLifetimeIndex == 0 {
		t.Fatal("retry ordering changed independent ordinary lifetime")
	}
	if deferred.selectiveGapRecovered || deferred.recoveryKind != sendRecoveryNone || !deferred.deferralOutstanding ||
		deferred.resendTime != now.Add(time.Second) || alreadyRecovered.recoveryKind != sendRecoveryAckTailProbe {
		t.Fatal("tie ordering bypassed deferred or already-recovered policy")
	}
	other, otherItems, _ := newEqualDeadlineRecoverySequence(t)
	if other.resendQueue == sequence.resendQueue || otherItems[0].messageId == items[0].messageId || other.sequenceId == sequence.sequenceId {
		t.Fatal("independent sequences shared an ordering owner")
	}
	other.scheduleSelectiveAckRecovery(now)
	if other.resendQueue.GetByMessageId(items[0].messageId) != nil || sequence.resendQueue.GetByMessageId(otherItems[0].messageId) != nil {
		t.Fatal("equal sequence numbers crossed sequence identity")
	}
}
