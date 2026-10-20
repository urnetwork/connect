// Selective proof may accelerate a retry, but cannot postpone work whose
// ordinary deadline has already elapsed. These tests use the real scheduler.
package connect

import (
	"testing"
	"time"
)

// Five overdue holes plus three later selective acknowledgements force the
// four-item promotion limit to compete with a younger ordinary deadline.
func TestSelectiveGapPreservesOverdueRecoveryOrder(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	now := base.Add(10 * time.Second)
	sequence, items := newSelectiveAckRecoveryTestSequence(8, base)
	if sequence.sendBufferSettings.SelectiveAckGapThreshold != 3 ||
		sequence.sendBufferSettings.SelectiveAckGapBurstSize != 4 {
		t.Fatal("fixture requires the default three-ack, four-recovery policy")
	}
	for index := 0; index < 5; index++ {
		sequence.setResendTime(items[index], base.Add(time.Duration(index)*time.Second))
	}
	for index := 5; index < 8; index++ {
		items[index].selectiveAcked = true
	}
	beforeBytes := sequence.resendQueue.byteCount
	beforeQueueBytes := sequence.resendQueue.queueByteCount
	beforeCount := len(sequence.resendQueue.orderedItems)
	var originalDue [8]time.Time
	for index, item := range items {
		originalDue[index] = item.resendTime
	}

	sequence.scheduleSelectiveAckRecovery(now)

	// On the old implementation, all four proven early holes move to now;
	// the fifth hole at base+4s becomes the next retry despite being younger.
	first := sequence.resendQueue.PeekFirst()
	if first != items[0] || items[0].resendTime != originalDue[0] {
		t.Fatalf("selective evidence postponed an already-due recovery: first=%d due=%s; earliest due=%s want first=0",
			first.sequenceNumber, items[0].resendTime, originalDue[0])
	}
	for index, item := range items {
		if item.resendTime != originalDue[index] {
			t.Fatalf("item %d deadline moved from %s to %s", index, originalDue[index], item.resendTime)
		}
		wantKind := sendRecoveryNone
		if index < 4 {
			wantKind = sendRecoverySelectiveGap
		}
		if item.recoveryKind != wantKind || item.selectiveGapRecovered != (index < 4) {
			t.Fatalf("item %d recovery kind=%d recovered=%t; burst policy changed",
				index, item.recoveryKind, item.selectiveGapRecovered)
		}
		if sequence.sendItems[index] != item ||
			sequence.resendQueue.GetByMessageId(item.messageId) != item ||
			sequence.resendQueue.GetBySequenceNumber(uint64(index)) != item ||
			item.sequenceNumber != uint64(index) || item.sendTime != base || item.sendCount != 1 ||
			item.selectiveAcked != (5 <= index) || item.deliveryObserved || item.serviceCreditObserved {
			t.Fatalf("item %d lost retry identity, original clocks, or acknowledgement custody", index)
		}
	}
	if len(sequence.resendQueue.orderedItems) != beforeCount ||
		sequence.resendQueue.byteCount != beforeBytes ||
		sequence.resendQueue.queueByteCount != beforeQueueBytes {
		t.Fatal("selective scheduling changed retained queue ownership or byte accounting")
	}
	if !sequence.selectiveGapRecoveryActive {
		t.Fatal("preserving overdue order failed to publish selective recovery activity")
	}
}

// Future timers still accelerate to the proof time; preserving old due work
// must not disable fast recovery or promote a fifth item beyond the burst cap.
func TestSelectiveGapStillAcceleratesFutureRecovery(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	now := base.Add(10 * time.Second)
	sequence, items := newSelectiveAckRecoveryTestSequence(8, base)
	for index := 0; index < 5; index++ {
		sequence.setResendTime(items[index], base.Add(time.Duration(20+index)*time.Second))
	}
	for index := 5; index < 8; index++ {
		items[index].selectiveAcked = true
	}
	beforeBytes := sequence.resendQueue.byteCount
	beforeQueueBytes := sequence.resendQueue.queueByteCount
	sequence.scheduleSelectiveAckRecovery(now)

	for index, item := range items {
		wantDue := base.Add(sequence.sendBufferSettings.SelectiveAckTimeout)
		wantKind := sendRecoveryNone
		if index < 4 {
			wantDue = now
			wantKind = sendRecoverySelectiveGap
		} else if index == 4 {
			wantDue = base.Add(24 * time.Second)
		}
		if item.resendTime != wantDue || item.recoveryKind != wantKind ||
			item.selectiveGapRecovered != (index < 4) {
			t.Fatalf("item %d future recovery changed: due=%s kind=%d recovered=%t",
				index, item.resendTime, item.recoveryKind, item.selectiveGapRecovered)
		}
		if sequence.sendItems[index] != item ||
			sequence.resendQueue.GetByMessageId(item.messageId) != item ||
			sequence.resendQueue.GetBySequenceNumber(uint64(index)) != item ||
			item.sendTime != base || item.sendCount != 1 ||
			item.selectiveAcked != (5 <= index) || item.deliveryObserved || item.serviceCreditObserved {
			t.Fatalf("item %d scheduling changed retry or acknowledgement custody", index)
		}
	}
	first := sequence.resendQueue.PeekFirst()
	if first.sequenceNumber >= 4 || first.resendTime != now {
		t.Fatal("future receiver-proven holes did not become due immediately")
	}
	if len(sequence.resendQueue.orderedItems) != 8 ||
		sequence.resendQueue.byteCount != beforeBytes ||
		sequence.resendQueue.queueByteCount != beforeQueueBytes {
		t.Fatal("future promotion changed retained queue ownership or byte accounting")
	}
}
