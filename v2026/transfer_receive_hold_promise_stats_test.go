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

// A predecessor larger than the reservation is the one counted shortfall, and
// each acknowledgement withdrawn for it is also an eviction.
func TestReceiveHoldCountsAShortfallForAPredecessorLargerThanItsReservation(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		budget := NewTransferMemoryBudget(ByteCount(math.MaxInt32))
		fixture := newReceiveHoldPromiseFixture(t, budget)
		_, next, large, _ := fillReceiveHoldPromiseBehindALargePredecessor(t, fixture, budget, 4)
		before := fixture.receiver.ReceiveStats()
		fixture.forward(large, fixture.receiverIn)
		after := fixture.receiver.ReceiveStats()
		evictions := after.ReceiveQueueEvictionCount - before.ReceiveQueueEvictionCount
		shortfalls := after.ReceiveQueueCommitShortfallCount - before.ReceiveQueueCommitShortfallCount
		if evictions == 0 || shortfalls != evictions {
			t.Errorf("%d acknowledgements withdrawn and %d counted as shortfall; every withdrawal for a promised predecessor is one", evictions, shortfalls)
		}
		fixture.drop(next)
	})
}
