// A receive hold acknowledges an out-of-order Pack only while it can still
// admit every Pack missing below it. These rows lose a run of Packs, deliver the
// later ones first into a hold whose shared retained budget binds before its
// per-sequence window, and then deliver the run. Ordering is forced by the
// controlled wire and synctest quiescence; time moves only where a row sleeps.
package connect

import (
	"bytes"
	"math"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

const receiveHoldPromisePayloadByteCount = 2000

// The settings a hosted low-memory device runs: an exact shared retained
// budget charged at the packet's backing size, and a per-sequence window far
// larger than that budget can fund.
func newReceiveHoldPromiseFixture(t *testing.T, budget *TransferMemoryBudget) *windowRoundFixture {
	return newReceiveHoldPromiseFixtureWithEvictionNotice(t, budget, true)
}

// The same, with eviction notices on, as shipped, or off, the measurement arm.
func newReceiveHoldPromiseFixtureWithEvictionNotice(t *testing.T, budget *TransferMemoryBudget, evictionNotice bool, headRecovery ...*TransferMemoryBudget) *windowRoundFixture {
	t.Cleanup(func() { assertRetainedBudgetBalance(t, budget) })
	return newWindowRoundFixture(t, func(settings *SendBufferSettings) {
		settings.ResendQueueMaxByteCount = mib(4)
	}, func(settings *ReceiveBufferSettings) {
		settings.ReceiveQueueMaxByteCount = mib(4)
		settings.ReceiveQueueBudget = budget
		settings.ReceiveQueueRetainedByteAccounting = true
		settings.ReceiveHoldPolicy = ReceiveHoldCommittedPrefix
		settings.EvictionNotice = evictionNotice
		if len(headRecovery) != 0 {
			settings.ReceiveHeadRecoveryBudget = headRecovery[0]
		}
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

// Exact accounting can refuse the physical owner before the decoded Pack
// reaches the receive queue. Both boundaries leave its sender owning retry.
func receiveHoldPromiseRefusedCount(fixture *windowRoundFixture) uint64 {
	stats := fixture.receiver.ReceiveStats()
	return stats.PackHandoffDropCount + stats.ReceiveQueueDropCount
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

	dropBase := receiveHoldPromiseRefusedCount(fixture)
	for range 4 * budgetPackCount {
		if receiveHoldPromiseRefusedCount(fixture) != dropBase {
			break
		}
		fixture.forward(fixture.write(receiveHoldPromisePayloadByteCount), fixture.receiverIn)
	}
	if receiveHoldPromiseRefusedCount(fixture) == dropBase {
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
	dropBase := receiveHoldPromiseRefusedCount(fixture)
	for range 48 {
		if receiveHoldPromiseRefusedCount(fixture) != dropBase {
			break
		}
		fixture.forward(fixture.write(receiveHoldPromisePayloadByteCount), fixture.receiverIn)
	}
	if receiveHoldPromiseRefusedCount(fixture) == dropBase {
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
	path, err := FilteredTransferPath(large.bytes)
	if err != nil {
		t.Fatal(err)
	}
	scan, status := scanReceiveMemoryPack(large.bytes, path)
	if status != receiveMemoryPackScanReady || scan.retainedByteCount > budget.TotalByteCount() ||
		scan.retainedByteCount <= receiveSequence.holdPromise.reservedByteCount {
		t.Fatalf("large predecessor is not a genuine within-cap promise shortfall: cost=%d cap=%d prefix=%d status=%d",
			scan.retainedByteCount, budget.TotalByteCount(), receiveSequence.holdPromise.reservedByteCount, status)
	}
	return receiveSequence, next, large, committed
}

// Settles real selective acknowledgements before inspecting custody. The
// source keeps its exact retry originals leased while these owners remain held.
func receiveHoldPromiseCommittedOwners(t *testing.T, fixture *windowRoundFixture, sequence *ReceiveSequence, committed map[uint64]bool) map[uint64]*receiveItem {
	t.Helper()
	fixture.acknowledge()
	owners := make(map[uint64]*receiveItem, len(committed))
	for number := range committed {
		item := sequence.receiveQueue.GetBySequenceNumber(number)
		if item == nil || !item.committed {
			t.Fatal("committed owner vanished before the pressure control")
		}
		owners[number] = item
	}
	assertReceiveHoldPromiseCommittedOwners(t, fixture, sequence, owners)
	return owners
}

// A predecode refusal must preserve both exact physical owners and the source's
// selective leases. No acceptance or withdrawal may appear on the wire.
func assertReceiveHoldPromiseCommittedOwners(t *testing.T, fixture *windowRoundFixture, sequence *ReceiveSequence, owners map[uint64]*receiveItem) {
	t.Helper()
	for number, owner := range owners {
		if sequence.receiveQueue.GetBySequenceNumber(number) != owner || !owner.committed {
			t.Fatalf("predecode pressure changed committed owner %d", number)
		}
	}
	leased := 0
	for _, item := range fixture.sequence().sendItems {
		if item != nil && owners[item.sequenceNumber] != nil && item.selectiveAcked {
			leased++
		}
	}
	if leased != len(owners) || len(fixture.receiverOut) != 0 {
		t.Fatalf("pressure changed selective custody: leased=%d owners=%d wire=%d", leased, len(owners), len(fixture.receiverOut))
	}
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

// The old queue-only path withdrew committed acknowledgements to fund this
// larger predecessor. Physical predecode cannot discard SACK custody: it
// refuses the complete retriable offer while every original and lease stays.
func TestReceiveHoldKeepsAcknowledgementsForAnOversizedPromisedPredecessor(t *testing.T) {
	assertMessagePoolOwnership(t)
	const deliveredCount = 4
	synctest.Test(t, func(t *testing.T) {
		budget := NewTransferMemoryBudget(ByteCount(math.MaxInt32))
		fixture := newReceiveHoldPromiseFixture(t, budget)
		receiveSequence, next, large, committed := fillReceiveHoldPromiseBehindALargePredecessor(t, fixture, budget, deliveredCount)
		owners := receiveHoldPromiseCommittedOwners(t, fixture, receiveSequence, committed)
		prefix := receiveSequence.holdPromise.reservedByteCount
		reserved, released := budget.Counts()
		before := fixture.receiver.ReceiveStats()
		fixture.forward(large, fixture.receiverIn)
		after := fixture.receiver.ReceiveStats()
		afterReserved, afterReleased := budget.Counts()
		if after.PackHandoffDropCount != before.PackHandoffDropCount+1 ||
			after.PackHandoffDropByteCount-before.PackHandoffDropByteCount != uint64(MessageByteCount(large.pack.Frames)) ||
			after.ReceiveQueueDropCount != before.ReceiveQueueDropCount ||
			after.ReceiveQueueEvictionCount != before.ReceiveQueueEvictionCount ||
			after.ReceiveQueueTentativeEvictionCount != before.ReceiveQueueTentativeEvictionCount ||
			after.ReceiveQueueCommitShortfallCount != before.ReceiveQueueCommitShortfallCount ||
			prefix != receiveSequence.holdPromise.reservedByteCount || reserved != afterReserved || released != afterReleased ||
			fixture.deliveredCount != deliveredCount || receiveSequence.ctx.Err() != nil {
			t.Fatal("larger predecode refusal spent the promise, withdrew custody, or changed prefix/lifetime")
		}
		assertReceiveHoldPromiseCommittedOwners(t, fixture, receiveSequence, owners)
		fixture.drop(next)
	})
}

// Without eviction notices every committed original must survive. The first
// larger non-current predecessor is refused, then its natural exact-current
// retry uses the production consumer family's bounded sibling head budget.
// A generic uncharged direct callback is not a physical recovery path.
func TestReceiveHoldWithoutEvictionNoticesKeepsEveryAcknowledgementForALargerPredecessor(t *testing.T) {
	assertMessagePoolOwnership(t)
	const deliveredCount = 4
	synctest.Test(t, func(t *testing.T) {
		root := NewTransferMemoryBudget(mib(1))
		budget := NewTransferMemoryBudgetWithParent(mib(1), root)
		recovery := NewTransferMemoryBudgetWithParent(kib(64), root)
		t.Cleanup(func() {
			assertRetainedBudgetBalance(t, recovery)
			assertRetainedBudgetBalance(t, root)
		})
		fixture := newReceiveHoldPromiseFixtureWithEvictionNotice(t, budget, false, recovery)
		fixture.frameFactory = func(payloadByteCount int) *protocol.Frame {
			packet := ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagAck, 11, bytes.Repeat([]byte{0x5a}, payloadByteCount))
			return &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider, Raw: true, MessageBytes: packet}
		}
		receiveSequence, next, large, committed := fillReceiveHoldPromiseBehindALargePredecessor(t, fixture, budget, deliveredCount)
		root.SetTotalByteCount(budget.TotalByteCount() + recovery.TotalByteCount())
		heldCount, _ := receiveSequence.receiveQueue.QueueSize()
		owners := receiveHoldPromiseCommittedOwners(t, fixture, receiveSequence, committed)
		if recovery.UsedByteCount() != 0 || recovery.TotalByteCount() > budget.TotalByteCount() {
			t.Fatal("consumer recovery child is not an unused bounded sibling of RX")
		}

		before := fixture.receiver.ReceiveStats()
		fixture.forward(large, fixture.receiverIn)
		after := fixture.receiver.ReceiveStats()
		if discarded := after.ReceiveQueueEvictionCount - before.ReceiveQueueEvictionCount; discarded != 0 {
			t.Errorf("the hold discarded %d acknowledged Packs it could not announce; the sender leases each until its selective acknowledgement timeout", discarded)
		}
		assertReceiveHoldPromiseCommittedOwners(t, fixture, receiveSequence, owners)
		if after.PackHandoffDropCount != before.PackHandoffDropCount+1 ||
			after.PackHandoffDropByteCount-before.PackHandoffDropByteCount != uint64(MessageByteCount(large.pack.Frames)) ||
			after.ReceiveQueueDropCount != before.ReceiveQueueDropCount ||
			after.ReceiveQueueCommitShortfallCount != before.ReceiveQueueCommitShortfallCount || recovery.UsedByteCount() != 0 {
			t.Fatal("non-current predecessor was not one exact predecode refusal with custody preserved")
		}

		// The delivery point arrives, and the larger predecessor is next.
		retry := fixture.naturalRetry(large)
		fixture.forward(next, fixture.receiverIn)
		fixture.acknowledge()
		path, err := FilteredTransferPath(retry.bytes)
		if err != nil {
			t.Fatal(err)
		}
		scan, status := scanReceiveMemoryPack(retry.bytes, path)
		if status != receiveMemoryPackScanReady || retry.pack.Head || receiveSequence.nextSequenceNumber != retry.pack.SequenceNumber ||
			scan.retainedByteCount > recovery.TotalByteCount() || scan.retainedByteCount > budget.TotalByteCount() {
			t.Fatal("real retry is not a within-cap logical current consumer head")
		}
		observedChild := false
		receiveSequence.receiveBufferSettings.receiveOwnerStageForTest = func(sequence *ReceiveSequence, owner *decodedPackOwner, stage string) {
			if sequence == receiveSequence && stage == "pack_released" && owner != nil && owner.receiveItem.sequenceNumber == retry.pack.SequenceNumber {
				observedChild = owner.receiveItem.memoryBudget == recovery && owner.receiveItem.QueueByteCount() == scan.retainedByteCount &&
					root.UsedByteCount() <= root.TotalByteCount()
			}
		}
		fixture.forward(retry, fixture.receiverIn)
		fixture.acknowledge()
		if want := deliveredCount + 2 + heldCount; fixture.deliveredCount != want {
			t.Errorf("delivered %d, want %d once the larger predecessor's resend landed", fixture.deliveredCount, want)
		}
		if !observedChild || fixture.ackedCount != fixture.deliveredCount {
			t.Fatal("funded consumer retry did not deliver and cumulatively acknowledge the held prefix")
		}
		for _, item := range fixture.sequence().sendItems {
			if item != nil && (item.sequenceNumber < receiveSequence.nextSequenceNumber || committed[item.sequenceNumber]) {
				t.Fatal("cumulative delivery retained an original from the committed prefix")
			}
		}
		assertRetainedBudgetBalance(t, budget)
		assertRetainedBudgetBalance(t, recovery)
		assertRetainedBudgetBalance(t, root)
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
