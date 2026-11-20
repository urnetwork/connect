// Adjacent physical bounds never authorize delivery, steal foreign custody,
// or turn an observed signature into preverified worker state.
package connect

import (
	"sync"
	"testing"
	"testing/synctest"
)

func TestReceiveConsumerFreshOpeningCannotExceedPhysicalRoot(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		const cost = ByteCount(5120)
		root := NewTransferMemoryBudget(cost)
		client := NewTransferMemoryBudgetWithParent(2*cost, root)
		rx := NewTransferMemoryBudgetWithParent(cost, client)
		head := NewTransferMemoryBudgetWithParent(cost, client)
		pack := NewTransferMemoryBudgetWithParent(cost, client)
		t.Cleanup(func() {
			for _, budget := range []*TransferMemoryBudget{pack, head, rx, client, root} {
				assertRetainedBudgetBalance(t, budget)
				if budget.Stats().QuotaByteCount != 0 {
					t.Error("root-cap control retained logical permission")
				}
			}
		})
		f, _ := newConsumerFinalHandoffFixture(t, rx, head, pack, nil, nil)
		last := map[uint32]uint64{}
		foreignHead := consumerForeignHeadWrite(t, f, last, 1)
		foreignTail := consumerForeignHeadWrite(t, f, last, 1)
		foreign := consumerForeignHeadSequence(t, f, foreignHead)
		f.forward(foreignTail, f.receiverIn)
		heldId := RequireIdFromBytes(foreignTail.pack.MessageId)
		held := foreign.receiveQueue.GetByMessageId(heldId)
		if held == nil || held.committed || held.memoryBudget != rx || held.QueueByteCount() != cost ||
			rx.UsedByteCount() != cost || root.UsedByteCount() != cost || root.Available() != 0 ||
			head.TotalByteCount() != cost || head.UsedByteCount() != 0 || foreign.holdPromise.set {
			t.Fatal("real foreign graph did not occupy the entire hard physical root")
		}
		_, data := freshConsumerDataOpeningCapture(t, f)
		// The real retained DATA original is promoted by the ordinary retry
		// formatter; the empty bootstrap remains unoffered in this bounds case.
		opening := freshConsumerOpeningFullDataHead(t, f, data)
		id := RequireIdFromBytes(opening.pack.SequenceId)
		retry := freshConsumerOpeningClone(f, opening)
		before := root.StatsWithDescendants(client, rx, head, pack)
		f.forward(opening, f.receiverIn)
		after := root.StatsWithDescendants(client, rx, head, pack)
		if freshConsumerOpeningSequence(f, id) != nil || f.deliveredCount != 0 || f.ackedCount != 0 || len(f.receiverOut) != 0 ||
			foreign.receiveQueue.GetByMessageId(heldId) != held || held.committed || foreign.holdPromise.set ||
			head.UsedByteCount() != 0 || after[0] != before[0] || after[1] != before[1] || after[2] != before[2] || after[3] != before[3] || after[4] != before[4] {
			t.Fatal("new opening spent unavailable physical bytes, changed foreign custody, or gained authority")
		}
		// The root cannot decode even the foreign missing predecessor. Actual
		// sequence shutdown returns its tentative graph; no fake quota release
		// or application acknowledgement creates space for the target.
		foreign.Cancel()
		<-foreign.done
		synctest.Wait()
		if rx.UsedByteCount() != 0 || root.UsedByteCount() != 0 || f.deliveredCount != 0 || f.ackedCount != 0 {
			t.Fatal("real foreign shutdown did not refund without manufacturing delivery")
		}
		f.forward(retry, f.receiverIn)
		f.acknowledge()
		target := freshConsumerOpeningSequence(f, id)
		if target == nil || target.receiveContract == nil || !target.receiveContract.verifiedReceiveProofSet ||
			f.deliveredCount != 1 || f.ackedCount != 1 || rx.UsedByteCount() != 0 || head.UsedByteCount() != 0 || root.UsedByteCount() != 0 {
			t.Fatal("same signed opening did not verify and progress after real root capacity returned")
		}
		t.Log("FRESH_OPENING_ROOT_CAP_BEHAVIOR_DRAINED")
	})
	if !t.Failed() {
		t.Log("FRESH_OPENING_ROOT_CAP_CLEANUP_JOINED")
	}
}

// The physical DATA positive holds the new worker before it can verify the
// full-proof DATA head (formatted from a real retained original). A second exact wire copy cannot consume its already-paid C twice.
// On old source its initial winner is refused; that stored cause is published
// only after the same source's real capacity-return retry and joined cleanup.
func TestReceiveConsumerFreshOpeningDuplicateKeepsOnePaidOwner(t *testing.T) {
	GetMessagePoolAggregateStats()
	blocked := false
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		const cost = ByteCount(5120)
		root := NewTransferMemoryBudget(4 * cost)
		client := NewTransferMemoryBudgetWithParent(4*cost, root)
		rx := NewTransferMemoryBudgetWithParent(2*cost, client)
		head := NewTransferMemoryBudgetWithParent(2*cost, client)
		pack := NewTransferMemoryBudgetWithParent(2*cost, client)
		t.Cleanup(func() {
			for _, budget := range []*TransferMemoryBudget{pack, head, rx, client, root} {
				assertRetainedBudgetBalance(t, budget)
				if budget.Stats().QuotaByteCount != 0 {
					t.Error("duplicate control retained logical permission")
				}
			}
		})
		f, _ := newConsumerFinalHandoffFixture(t, rx, head, pack, nil, nil)
		entered, release := make(chan struct{}), make(chan struct{})
		var enterOnce, releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		t.Cleanup(unblock)
		f.receiver.receiveBuffer.beforeRunReceiveSequenceForTest = func(id receiveSequenceId) {
			if id.LogicalLane == 0 {
				enterOnce.Do(func() { close(entered) })
				select {
				case <-release:
				case <-f.ctx.Done():
				}
			}
		}
		last := map[uint32]uint64{}
		foreignHead := consumerForeignHeadWrite(t, f, last, 1)
		foreignTail := consumerForeignHeadWrite(t, f, last, 1)
		secondForeignTail := consumerForeignHeadWrite(t, f, last, 1)
		foreign := consumerForeignHeadSequence(t, f, foreignHead)
		f.forward(foreignTail, f.receiverIn)
		f.forward(secondForeignTail, f.receiverIn)
		if rx.UsedByteCount() != 2*cost || head.UsedByteCount() != 0 || foreign.holdPromise.set {
			t.Fatal("duplicate control has no entirely foreign full RX")
		}
		_, data := freshConsumerDataOpeningCapture(t, f)
		// The real retained DATA original is promoted by the ordinary retry
		// formatter; the empty bootstrap remains unoffered in this bounds case.
		opening := freshConsumerOpeningFullDataHead(t, f, data)
		id := RequireIdFromBytes(opening.pack.SequenceId)
		duplicate := freshConsumerOpeningClone(f, opening)
		retry := freshConsumerOpeningClone(f, opening)
		f.forward(opening, f.receiverIn)
		target := freshConsumerOpeningSequence(f, id)
		blocked = target == nil
		if !blocked {
			select {
			case <-entered:
			default:
				t.Fatal("new target worker was not held before proof and ACK processing")
			}
			if target.receiveContract != nil || target.nextSequenceNumber != 0 || target.holdPromise.ingressHead == nil ||
				head.UsedByteCount() <= 0 || f.deliveredCount != 0 || f.ackedCount != 0 || len(f.receiverOut) != 0 {
				t.Fatal("unverified DATA winner gained delivery authority or lost its actual paid owner")
			}
		}
		if !blocked {
			actualCost := head.UsedByteCount()
			if actualCost <= 0 || head.Available() < actualCost || root.Available() < actualCost {
				t.Fatal("duplicate DATA stimulus lacks independently spendable full-C head/root room")
			}
		}
		before := root.StatsWithDescendants(client, rx, head, pack)
		f.forward(duplicate, f.receiverIn)
		after := root.StatsWithDescendants(client, rx, head, pack)
		for index := range before {
			if before[index] != after[index] {
				t.Fatal("exact duplicate changed the winner's physical/quota credit or spent another C")
			}
		}
		if f.deliveredCount != 0 || f.ackedCount != 0 || len(f.receiverOut) != 0 || foreign.holdPromise.set || rx.UsedByteCount() != 2*cost {
			t.Fatal("unverified duplicate gained ACK, delivery or foreign permission")
		}
		unblock()
		synctest.Wait()
		f.acknowledge()
		if !blocked {
			if f.deliveredCount != 1 || f.ackedCount != 1 || target.receiveContract == nil || !target.receiveContract.verifiedReceiveProofSet || head.UsedByteCount() != 0 {
				t.Fatal("one admitted opening did not verify and release exactly one SYNACK payload")
			}
		}
		f.forward(foreignHead, f.receiverIn)
		f.acknowledge()
		if blocked {
			f.forward(retry, f.receiverIn)
			f.acknowledge()
		}
		if f.deliveredCount != 4 || f.ackedCount != 4 || rx.UsedByteCount() != 0 || head.UsedByteCount() != 0 || root.UsedByteCount() != 0 || pack.Stats().QuotaByteCount != 0 {
			t.Fatal("duplicate control did not complete the real recovery and exact owner refunds")
		}
		t.Logf("FRESH_OPENING_DUPLICATE_BEHAVIOR_DRAINED initial_blocked=%t", blocked)
	})
	if t.Failed() {
		return
	}
	t.Log("FRESH_OPENING_DUPLICATE_CLEANUP_JOINED")
	if blocked {
		t.Fatal("FRESH_OPENING_DUPLICATE_WINNER_NOT_ADMITTED: exact duplicate stayed safe; first signed source only progressed after foreign capacity returned")
	}
}
