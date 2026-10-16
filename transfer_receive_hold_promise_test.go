// A receive hold acknowledges an out-of-order Pack only while it can still
// admit every Pack missing below it. These rows lose a run of Packs, deliver the
// later ones first into a hold whose shared retained budget binds before its
// per-sequence window, and then deliver the run. Ordering is forced by the
// controlled wire and synctest quiescence; time moves only where a row sleeps.
package connect

import (
	"math"
	"slices"
	"testing"
	"testing/synctest"
	"time"
)

const receiveHoldPromisePayloadByteCount = 2000

// The settings a hosted low-memory device runs: an exact shared retained
// budget charged at the packet's backing size, and a per-sequence window far
// larger than that budget can fund.
func newReceiveHoldPromiseFixture(t *testing.T, budget *TransferMemoryBudget) *windowRoundFixture {
	return newReceiveHoldPromiseFixtureWithEvictionNotice(t, budget, true)
}

// The same, with eviction notices on, as shipped, or off, the measurement arm.
func newReceiveHoldPromiseFixtureWithEvictionNotice(t *testing.T, budget *TransferMemoryBudget, evictionNotice bool) *windowRoundFixture {
	return newWindowRoundFixture(t, func(settings *SendBufferSettings) {
		settings.ResendQueueMaxByteCount = mib(4)
	}, func(settings *ReceiveBufferSettings) {
		settings.ReceiveQueueMaxByteCount = mib(4)
		settings.ReceiveQueueBudget = budget
		settings.ReceiveQueueRetainedByteAccounting = true
		settings.ReceiveHoldPolicy = ReceiveHoldCommittedPrefix
		settings.EvictionNotice = evictionNotice
	})
}

// Looks up the controlled receive sequence, or nil once it has exited.
func receiveHoldPromiseSequence(fixture *windowRoundFixture) *ReceiveSequence {
	synctest.Wait()
	buffer := fixture.receiver.receiveBuffer
	buffer.mutex.Lock()
	defer buffer.mutex.Unlock()
	for _, sequence := range buffer.receiveSequences {
		if sequence.source.SourceId == fixture.sender.ClientId() {
			return sequence
		}
	}
	return nil
}

// Delivers `deliveredCount` Packs, withholds the next `gapCount` the way a
// retired relay generation loses them, then forwards later Packs until the
// shared budget refuses one. The budget is sized from the first held Pack's
// exact charge so that it, and not the window, is what binds.
func fillReceiveHoldPromise(
	t *testing.T,
	fixture *windowRoundFixture,
	budget *TransferMemoryBudget,
	deliveredCount int,
	gapCount int,
	budgetPackCount int,
) (*ReceiveSequence, []*windowRoundFrame) {
	t.Helper()
	for range deliveredCount {
		fixture.forward(fixture.write(receiveHoldPromisePayloadByteCount), fixture.receiverIn)
	}
	gap := make([]*windowRoundFrame, gapCount)
	for i := range gap {
		gap[i] = fixture.write(receiveHoldPromisePayloadByteCount)
	}
	receiveSequence := fixture.receiveSequence()
	fixture.forward(fixture.write(receiveHoldPromisePayloadByteCount), fixture.receiverIn)
	first := receiveSequence.receiveQueue.PeekLast()
	if first == nil || first.sequenceNumber != uint64(deliveredCount+gapCount) {
		t.Fatalf("the first Pack past the gap was not held: %+v", first)
	}
	charge := first.QueueByteCount()
	if charge <= first.MessageByteCount() {
		t.Fatalf("retained charge %d does not exceed the message's %d bytes; the budget would not be the binding limit", charge, first.MessageByteCount())
	}
	budget.SetTotalByteCount(ByteCount(budgetPackCount) * charge)

	dropBase := fixture.receiver.ReceiveStats().ReceiveQueueDropCount
	for range 4 * budgetPackCount {
		if fixture.receiver.ReceiveStats().ReceiveQueueDropCount != dropBase {
			break
		}
		fixture.forward(fixture.write(receiveHoldPromisePayloadByteCount), fixture.receiverIn)
	}
	if fixture.receiver.ReceiveStats().ReceiveQueueDropCount == dropBase {
		t.Fatal("the shared budget never refused a later Pack, so the hold was never budget bound")
	}
	newest := receiveSequence.receiveQueue.PeekLast()
	oldest := receiveSequence.receiveQueue.PeekFirst()
	if newest == nil || oldest == nil || !oldest.committed {
		t.Fatal("the hold acknowledged nothing past the gap, so the gap carries no promise")
	}
	// The window has room for every withheld Pack; only the budget is spent.
	_, heldByteCount := receiveSequence.receiveQueue.QueueSize()
	if mib(4) <= heldByteCount+ByteCount(gapCount)*newest.MessageByteCount() {
		t.Fatal("the per-sequence window binds; this row is about the shared budget")
	}
	if budget.Available() >= charge {
		t.Fatalf("the shared budget still has %d of %d free; a predecessor would be admitted without any promise", budget.Available(), charge)
	}
	return receiveSequence, gap
}

// Delivers `deliveredCount` Packs and withholds the next two: the delivery
// point, and one eight times the size of anything the hold will take. Later
// Packs are then forwarded until the shared budget refuses one. Returns the two
// withheld Packs and the numbers the hold acknowledged.
func fillReceiveHoldPromiseBehindALargePredecessor(
	t *testing.T,
	fixture *windowRoundFixture,
	budget *TransferMemoryBudget,
	deliveredCount int,
) (receiveSequence *ReceiveSequence, next *windowRoundFrame, large *windowRoundFrame, committed map[uint64]bool) {
	t.Helper()
	for range deliveredCount {
		fixture.forward(fixture.write(receiveHoldPromisePayloadByteCount), fixture.receiverIn)
	}
	next = fixture.write(receiveHoldPromisePayloadByteCount)
	large = fixture.write(8 * receiveHoldPromisePayloadByteCount)
	receiveSequence = fixture.receiveSequence()
	fixture.forward(fixture.write(receiveHoldPromisePayloadByteCount), fixture.receiverIn)
	first := receiveSequence.receiveQueue.PeekLast()
	budget.SetTotalByteCount(12 * first.QueueByteCount())
	dropBase := fixture.receiver.ReceiveStats().ReceiveQueueDropCount
	for range 48 {
		if fixture.receiver.ReceiveStats().ReceiveQueueDropCount != dropBase {
			break
		}
		fixture.forward(fixture.write(receiveHoldPromisePayloadByteCount), fixture.receiverIn)
	}
	if fixture.receiver.ReceiveStats().ReceiveQueueDropCount == dropBase {
		t.Fatal("the shared budget never refused a later Pack")
	}
	committed = map[uint64]bool{}
	for _, held := range receiveSequence.receiveQueue.AscendingItems(nil) {
		if held.committed {
			committed[held.sequenceNumber] = true
		}
	}
	if len(committed) == 0 {
		t.Fatal("the hold acknowledged nothing, so the gap carries no promise")
	}
	return receiveSequence, next, large, committed
}

// Root cause. A hold whose shared budget is spent had acknowledged Packs whose
// missing predecessors it then refused, because it committed against the
// per-sequence window alone. Only the exact next Pack could still land, on
// the direct path, so a gap of several Packs waited out the gap timeout and
// the sequence exited holding everything it had acknowledged. A commitment now
// reserves what its predecessors will charge, and they are admitted from it in
// any order.
func TestReceiveHoldAdmitsEveryPromisedPredecessorWhenTheSharedBudgetIsSpent(t *testing.T) {
	assertMessagePoolOwnership(t)
	const deliveredCount = 4
	const gapCount = 3
	synctest.Test(t, func(t *testing.T) {
		budget := NewTransferMemoryBudget(ByteCount(math.MaxInt32))
		fixture := newReceiveHoldPromiseFixture(t, budget)
		receiveSequence, gap := fillReceiveHoldPromise(t, fixture, budget, deliveredCount, gapCount, 12)
		heldCount, _ := receiveSequence.receiveQueue.QueueSize()
		highestCommitted := uint64(0)
		for _, held := range receiveSequence.receiveQueue.AscendingItems(nil) {
			if held.committed {
				highestCommitted = held.sequenceNumber
			}
		}

		// The predecessors arrive out of order, the shape a resend round has
		// after the loss: everything but the next Pack first.
		dropBase := fixture.receiver.ReceiveStats().ReceiveQueueDropCount
		for _, frame := range gap[1:] {
			fixture.forward(frame, fixture.receiverIn)
		}
		if drops := fixture.receiver.ReceiveStats().ReceiveQueueDropCount - dropBase; drops != 0 {
			t.Errorf("the hold refused %d predecessors of Packs it had acknowledged; a commitment must keep them admissible", drops)
		}
		fixture.forward(gap[0], fixture.receiverIn)
		if want := deliveredCount + gapCount + heldCount; fixture.deliveredCount != want {
			t.Errorf("delivered %d of %d once every predecessor had arrived once; the gap stayed open at %d",
				fixture.deliveredCount, want, receiveSequence.nextSequenceNumber)
		}

		// With no further resend, the gap timeout is the hold's last word. It
		// must not be able to discard anything this hold acknowledged.
		time.Sleep(fixture.receiver.settings.ReceiveBufferSettings.GapTimeout + time.Second)
		synctest.Wait()
		if receiveSequence.nextSequenceNumber <= highestCommitted {
			t.Errorf("the gap timeout ended the sequence at %d with acknowledged Packs through %d still held; it discarded what it had acknowledged",
				receiveSequence.nextSequenceNumber, highestCommitted)
		}
		if used := budget.UsedByteCount(); used != 0 {
			t.Errorf("the hold still charges %d bytes to the shared budget; the promise's reservation must go with the gap", used)
		}
	})
}

// A sequence that exits on its gap timeout discards a hold it acknowledged.
// It withdraws those acknowledgements first, by eviction notice, so the sender
// resends the Packs at once instead of leasing them for its selective
// acknowledgement timeout while a replacement sequence waits for them.
func TestReceiveGapTimeoutWithdrawsTheAcknowledgementsItDiscards(t *testing.T) {
	assertMessagePoolOwnership(t)
	const deliveredCount = 4
	const gapCount = 3
	synctest.Test(t, func(t *testing.T) {
		budget := NewTransferMemoryBudget(ByteCount(math.MaxInt32))
		fixture := newReceiveHoldPromiseFixture(t, budget)
		receiveSequence, gap := fillReceiveHoldPromise(t, fixture, budget, deliveredCount, gapCount, 12)
		committed := map[uint64]bool{}
		for _, held := range receiveSequence.receiveQueue.AscendingItems(nil) {
			if held.committed {
				committed[held.sequenceNumber] = true
			}
		}
		// The sender learns what the receiver acknowledged and leases it.
		fixture.acknowledge()
		sequence := fixture.sequence()
		leased := 0
		for _, item := range sequence.sendItems {
			if item != nil && committed[item.sequenceNumber] && item.selectiveAcked {
				leased += 1
			}
		}
		if leased != len(committed) {
			t.Fatalf("the sender leased %d of the %d Packs the receiver acknowledged", leased, len(committed))
		}

		// The run never arrives, so the gap timeout expires the hold.
		for _, frame := range gap {
			fixture.drop(frame)
		}
		time.Sleep(fixture.receiver.settings.ReceiveBufferSettings.GapTimeout + time.Second)
		if receiveHoldPromiseSequence(fixture) != nil {
			t.Fatal("the receive sequence outlived its gap timeout")
		}

		// Its last acknowledgements withdraw every Pack it discarded.
		withdrawn := map[uint64]bool{}
		synctest.Wait()
		for len(fixture.receiverOut) != 0 {
			frame := fixture.take(fixture.receiverOut)
			if frame.ack != nil {
				for _, number := range frame.ack.EvictedSequenceNumbers {
					withdrawn[number] = true
				}
			}
			fixture.forward(frame, fixture.senderIn)
		}
		silent := []uint64{}
		for number := range committed {
			if !withdrawn[number] {
				silent = append(silent, number)
			}
		}
		if len(silent) != 0 {
			slices.Sort(silent)
			t.Errorf("%d of %d acknowledged Packs were discarded with no withdrawal, so the sender keeps its lease on them: %v",
				len(silent), len(committed), silent)
		}
		synctest.Wait()
		stillLeased := 0
		for _, item := range sequence.sendItems {
			if item != nil && committed[item.sequenceNumber] && item.selectiveAcked {
				stillLeased += 1
			}
		}
		if stillLeased != 0 {
			t.Errorf("the sender still leases %d Packs the receiver discarded", stillLeased)
		}
	})
}

// The reservation is sized from the largest Pack the hold has taken. A lost
// Pack can be larger than all of them, and then admitting it needs more than
// was reserved. Refusing it would bring back the stall, so the hold withdraws
// its newest acknowledgements, with notice, until the predecessor fits.
func TestReceiveHoldWithdrawsAnAcknowledgementForAPredecessorLargerThanItsReservation(t *testing.T) {
	assertMessagePoolOwnership(t)
	const deliveredCount = 4
	synctest.Test(t, func(t *testing.T) {
		budget := NewTransferMemoryBudget(ByteCount(math.MaxInt32))
		fixture := newReceiveHoldPromiseFixture(t, budget)
		receiveSequence, next, large, committed := fillReceiveHoldPromiseBehindALargePredecessor(t, fixture, budget, deliveredCount)
		heldCount, _ := receiveSequence.receiveQueue.QueueSize()
		// Settle the acknowledgements published so far; what follows on the
		// wire is the receiver's answer to the large predecessor.
		synctest.Wait()
		for len(fixture.receiverOut) != 0 {
			fixture.forward(fixture.take(fixture.receiverOut), fixture.senderIn)
		}

		before := fixture.receiver.ReceiveStats()
		fixture.forward(large, fixture.receiverIn)
		after := fixture.receiver.ReceiveStats()
		if after.ReceiveQueueDropCount != before.ReceiveQueueDropCount {
			t.Fatal("the hold refused a predecessor of Packs it had acknowledged")
		}
		withdrawnCount := int(after.ReceiveQueueEvictionCount - before.ReceiveQueueEvictionCount)
		if withdrawnCount == 0 {
			t.Fatal("the larger predecessor was admitted without withdrawing anything; this row no longer exceeds the reservation")
		}

		// The withdrawal is announced, never silent.
		withdrawn := map[uint64]bool{}
		synctest.Wait()
		for len(fixture.receiverOut) != 0 {
			frame := fixture.take(fixture.receiverOut)
			if frame.ack != nil {
				for _, number := range frame.ack.EvictedSequenceNumbers {
					withdrawn[number] = true
				}
			}
			fixture.forward(frame, fixture.senderIn)
		}
		if len(withdrawn) != withdrawnCount {
			t.Errorf("%d acknowledgements were withdrawn and %d announced", withdrawnCount, len(withdrawn))
		}
		for number := range withdrawn {
			if !committed[number] {
				t.Errorf("Pack %d was announced as withdrawn but had not been acknowledged", number)
			}
		}

		// The gap fills: the delivery point arrives and everything still held
		// below the withdrawn Packs drains with it.
		fixture.forward(next, fixture.receiverIn)
		if want := deliveredCount + 2 + heldCount - withdrawnCount; fixture.deliveredCount != want {
			t.Errorf("delivered %d, want %d once the gap's two Packs had arrived", fixture.deliveredCount, want)
		}
	})
}

// With eviction notices off a withdrawal could not reach the sender, which
// would lease the withdrawn Pack until its selective acknowledgement timeout:
// the sixty second wait on one Pack that a hold must never cause. The hold
// keeps every acknowledgement and turns the larger predecessor away instead.
// The gap still closes, because that predecessor becomes the delivery point's
// next Pack once the gap below it fills, and its resend takes the direct path.
func TestReceiveHoldWithoutEvictionNoticesKeepsEveryAcknowledgementForALargerPredecessor(t *testing.T) {
	assertMessagePoolOwnership(t)
	const deliveredCount = 4
	synctest.Test(t, func(t *testing.T) {
		budget := NewTransferMemoryBudget(ByteCount(math.MaxInt32))
		fixture := newReceiveHoldPromiseFixtureWithEvictionNotice(t, budget, false)
		receiveSequence, next, large, committed := fillReceiveHoldPromiseBehindALargePredecessor(t, fixture, budget, deliveredCount)
		heldCount, _ := receiveSequence.receiveQueue.QueueSize()
		fixture.acknowledge()

		before := fixture.receiver.ReceiveStats()
		fixture.forward(large, fixture.receiverIn)
		after := fixture.receiver.ReceiveStats()
		if discarded := after.ReceiveQueueEvictionCount - before.ReceiveQueueEvictionCount; discarded != 0 {
			t.Errorf("the hold discarded %d acknowledged Packs it could not announce; the sender leases each until its selective acknowledgement timeout", discarded)
		}
		for _, held := range receiveSequence.receiveQueue.AscendingItems(nil) {
			if held.committed {
				delete(committed, held.sequenceNumber)
			}
		}
		if len(committed) != 0 {
			gone := []uint64{}
			for number := range committed {
				gone = append(gone, number)
			}
			slices.Sort(gone)
			t.Errorf("acknowledged Packs %v left the hold with no notice", gone)
		}
		if refused := after.ReceiveQueueDropCount - before.ReceiveQueueDropCount; refused != 1 {
			t.Errorf("%d arrivals refused, want the one larger predecessor", refused)
		}
		if shortfalls := after.ReceiveQueueCommitShortfallCount - before.ReceiveQueueCommitShortfallCount; shortfalls != 1 {
			t.Errorf("%d shortfalls counted, want the one refusal", shortfalls)
		}

		// The delivery point arrives, and the larger predecessor is next.
		fixture.forward(next, fixture.receiverIn)
		fixture.acknowledge()
		fixture.forward(fixture.recovery(large), fixture.receiverIn)
		if want := deliveredCount + 2 + heldCount; fixture.deliveredCount != want {
			t.Errorf("delivered %d, want %d once the larger predecessor's resend landed", fixture.deliveredCount, want)
		}
	})
}

// End to end through the retire shape: a relay generation dies with a run of
// Packs in flight, the Packs behind it reach a budget-bound hold first, and
// recovery is left to the real sender over a live wire. The run must close
// within one recovery cycle, not one exact-next retransmission at a time.
func TestTransferRecoversAGenerationLossBehindABudgetBoundHold(t *testing.T) {
	assertMessagePoolOwnership(t)
	const deliveredCount = 16
	const lostCount = 40
	const remainingCount = 120
	synctest.Test(t, func(t *testing.T) {
		budget := NewTransferMemoryBudget(ByteCount(math.MaxInt32))
		fixture := newReceiveHoldPromiseFixture(t, budget)
		receiveSequence, lost := fillReceiveHoldPromise(t, fixture, budget, deliveredCount, lostCount, 96)
		lowest := lost[0].pack.SequenceNumber
		highest := lost[len(lost)-1].pack.SequenceNumber
		for _, frame := range lost {
			fixture.drop(frame)
		}
		// The sender learns what the hold took and recovers the run. The
		// replacement generation also loses the first recovery of the run's
		// first Pack, so the rest of the run reaches the hold ahead of it, as
		// a resend round interleaved with the sender's other recoveries does.
		fixture.acknowledge()
		fixture.drop(fixture.takePack(lowest))
		start := time.Now()
		fixture.startWire(25*time.Millisecond, DefaultReceiveBufferSettings().AckCompressTimeout)
		fixture.offer(remainingCount, receiveHoldPromisePayloadByteCount)
		// Well inside the gap timeout: recovery is a round trip and a resend
		// interval, not the gap timeout.
		time.Sleep(20 * time.Second)
		synctest.Wait()
		if current := receiveHoldPromiseSequence(fixture); current != receiveSequence {
			t.Fatal("the receive sequence did not survive the loss")
		}
		if receiveSequence.nextSequenceNumber <= highest {
			t.Fatalf("after %s the delivery point is %d, still inside the lost run %d..%d",
				time.Since(start), receiveSequence.nextSequenceNumber, lowest, highest)
		}
		if want := int(fixture.nextNumber) + remainingCount; fixture.deliveredCount != want {
			t.Errorf("delivered %d of the %d messages sent", fixture.deliveredCount, want)
		}
	})
}
