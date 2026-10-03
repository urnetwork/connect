package connect

import (
	"errors"
	"testing"
)

func TestPreparedHandoffCoalescedCancellationKeepsLiveCreditRetryable(t *testing.T) {
	for _, canceled := range []int{-1, 0, 1} {
		budget := NewTransferMemoryBudget(kib(16))
		var owners [2]*providerDatagramAck
		var item sendItem
		for index := range owners {
			credit, ok := prepareSendMemory(budget, kib(8))
			if !ok {
				t.Fatal("credit admission")
			}
			owners[index] = &providerDatagramAck{credit: credit}
			item.acks.add(sendAckRecord{target: owners[index]})
		}
		if canceled >= 0 {
			owners[canceled].handoff.cancelBeforeHandoff()
		}
		reserved := false
		err := item.acks.commitPreparedHandoffs(func() error {
			reserved = true
			if !item.reservePreparedMemory(budget, kib(12)) {
				t.Fatal("full-parent credit handoff failed")
			}
			return nil
		})
		if canceled >= 0 {
			if !errors.Is(err, errPreparedSendCanceled) || reserved || item.memoryBudget != nil {
				t.Fatal("canceled batch consumed a flight claim")
			}
			for _, owner := range owners {
				if owner.handoff.transferred() || owner.credit.flightMoved || owner.credit.bytes != kib(8) {
					t.Fatal("canceled batch changed its live sibling's retryable ownership")
				}
			}
		} else {
			if err != nil || !reserved || budget.UsedByteCount() != kib(16) {
				t.Fatalf("coalesced handoff: err=%v used=%d", err, budget.UsedByteCount())
			}
			for _, owner := range owners {
				if !owner.handoff.cancelBeforeHandoff() || owner.handoff.cancellationRequested() {
					t.Fatal("late cancellation withdrew a materialized reliable owner")
				}
			}
		}
		for _, owner := range owners {
			owner.credit.release()
		}
		item.releaseMemory()
		assertRetainedBudgetBalance(t, budget)
	}
}

func TestPreparedHandoffFailedReservationDoesNotCommit(t *testing.T) {
	owner := &providerDatagramAck{}
	var acks sendAckSet
	acks.add(sendAckRecord{target: owner})
	if err := acks.commitPreparedHandoffs(func() error { return errPreparedSendMemoryUnavailable }); err != errPreparedSendMemoryUnavailable {
		t.Fatalf("wrong refusal: %v", err)
	}
	if owner.handoff.transferred() || owner.handoff.cancelBeforeHandoff() {
		t.Fatal("failed reservation manufactured a joined reliable owner")
	}
	owner.handoff.settle()
	if !owner.handoff.cancelBeforeHandoff() {
		t.Fatal("queued disposal did not join cancellation")
	}
}
