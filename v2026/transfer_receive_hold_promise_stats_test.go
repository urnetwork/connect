// The withdrawals a receive hold makes are counted where they are made, so a
// field reading can tell an announced withdrawal from a silent one.
package connect

import (
	"math"
	"testing"
	"testing/synctest"
	"time"
)

// Every acknowledged Pack a gap-timeout exit discards is counted as withdrawn.
func TestReceiveGapTimeoutCountsTheAcknowledgementsItWithdraws(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		budget := NewTransferMemoryBudget(ByteCount(math.MaxInt32))
		fixture := newReceiveHoldPromiseFixture(t, budget)
		receiveSequence, gap := fillReceiveHoldPromise(t, fixture, budget, 4, 3, 12)
		committed := 0
		for _, held := range receiveSequence.receiveQueue.AscendingItems(nil) {
			if held.committed {
				committed += 1
			}
		}
		for _, frame := range gap {
			fixture.drop(frame)
		}
		time.Sleep(fixture.receiver.settings.ReceiveBufferSettings.GapTimeout + time.Second)
		synctest.Wait()
		stats := fixture.receiver.ReceiveStats()
		if stats.ReceiveQueueExitWithdrawCount != uint64(committed) {
			t.Errorf("the exit withdrew %d acknowledgements, want the %d it held", stats.ReceiveQueueExitWithdrawCount, committed)
		}
		if stats.ReceiveQueueCommitShortfallCount != 0 {
			t.Errorf("%d shortfalls counted with no predecessor arriving", stats.ReceiveQueueCommitShortfallCount)
		}
	})
}

// The former queue-admission regression expected committed withdrawals here.
// Owned predecode now refuses first and preserves every SACK owner: its loss
// belongs to handoff counters, with no queue shortfall or eviction invented.
func TestReceiveHoldCountsPredecodeRefusalWithoutWithdrawingAcknowledgements(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		budget := NewTransferMemoryBudget(ByteCount(math.MaxInt32))
		fixture := newReceiveHoldPromiseFixture(t, budget)
		sequence, next, large, committed := fillReceiveHoldPromiseBehindALargePredecessor(t, fixture, budget, 4)
		owners := receiveHoldPromiseCommittedOwners(t, fixture, sequence, committed)
		reserved, released := budget.Counts()
		before := fixture.receiver.ReceiveStats()
		fixture.forward(large, fixture.receiverIn)
		after := fixture.receiver.ReceiveStats()
		afterReserved, afterReleased := budget.Counts()
		if after.PackHandoffDropCount != before.PackHandoffDropCount+1 ||
			after.PackHandoffDropByteCount-before.PackHandoffDropByteCount != uint64(MessageByteCount(large.pack.Frames)) ||
			after.ReceiveQueueDropCount != before.ReceiveQueueDropCount ||
			after.ReceiveQueueCommitShortfallCount != before.ReceiveQueueCommitShortfallCount ||
			after.ReceiveQueueEvictionCount != before.ReceiveQueueEvictionCount ||
			after.ReceiveQueueTentativeEvictionCount != before.ReceiveQueueTentativeEvictionCount ||
			reserved != afterReserved || released != afterReleased || fixture.deliveredCount != 4 {
			t.Fatal("predecode refusal changed funded custody or counted an unexecuted queue admission")
		}
		assertReceiveHoldPromiseCommittedOwners(t, fixture, sequence, owners)
		fixture.drop(next)
	})
}
