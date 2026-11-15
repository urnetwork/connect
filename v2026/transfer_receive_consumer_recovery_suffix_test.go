// A future child owner cannot enter RX's hold. Refusing that owner must not
// evict a paid suffix before the inevitable reservation refusal.
package connect

import (
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

func TestReceiveConsumerRecoveryFutureRefusalPreservesCommittedSuffix(t *testing.T) {
	testReceiveConsumerRecoverySuffix(t, true)
}

func TestReceiveConsumerRecoveryFutureRefusalPreservesTentativeSuffix(t *testing.T) {
	testReceiveConsumerRecoverySuffix(t, false)
}

// Every identity, contract, selective lease and payload comes from the signed
// public fixture. Only the first predecessor's existing pre-Pack progress seam
// is parked; a genuine second wire is decoded at that quiescent worker boundary.
func testReceiveConsumerRecoverySuffix(t *testing.T, committed bool) {
	t.Helper()
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		var cause string
		// Registered first so the typed counterfactual follows client joins,
		// physical/quota balance checks and message-pool ownership checks.
		t.Cleanup(func() {
			if cause != "" {
				t.Fatal(cause)
			}
		})
		assertMessagePoolOwnership(t)
		const smallCost, largeCost = ByteCount(1536), ByteCount(5120)
		repairCost := DefaultReceiveBufferSettings().receiveHeldHeadRepairByteCount(false)
		rxLimit := 3*smallCost + 2*smallCost + repairCost
		if !committed {
			rxLimit = largeCost + smallCost
		}
		root := NewTransferMemoryBudget(2 * rxLimit)
		client := NewTransferMemoryBudgetWithParent(2*rxLimit, root)
		rx := NewTransferMemoryBudgetWithParent(rxLimit, client)
		head := NewTransferMemoryBudgetWithParent(rxLimit, client)
		packBudget := NewTransferMemoryBudgetWithParent(rxLimit, client)
		t.Cleanup(func() {
			for _, budget := range []*TransferMemoryBudget{packBudget, head, rx, client, root} {
				assertRetainedBudgetBalance(t, budget)
				if budget.Stats().QuotaByteCount != 0 {
					t.Error("suffix refusal retained logical quota after joined clients")
				}
			}
			t.Log("RECEIVE_RECOVERY_SUFFIX_CLEANUP_JOINED")
		})

		entered, releaseFirst := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
		var firstTarget atomic.Pointer[Id]
		var armed atomic.Bool
		progress := func(event TransferProgressEvent) {
			id := firstTarget.Load()
			if event.Stage == "receive_pack_begin" && id != nil && event.MessageId == *id && armed.CompareAndSwap(true, false) {
				close(entered)
				<-releaseFirst
			}
		}
		fixture, logger := newConsumerFinalHandoffFixture(t, rx, head, packBudget, nil, progress)
		// Unblock the pre-Pack observer before the fixture's close-and-join.
		t.Cleanup(release)
		if fixture.receiver.settings.ReceiveBufferSettings.ReceiveHeadContractRepairByteCount != repairCost ||
			!fixture.receiver.settings.ReceiveBufferSettings.EvictionNotice ||
			fixture.receiver.ContractManager().ReceiveNoContract(fixture.sender.ClientId()) {
			t.Fatal("suffix fixture did not retain signed proof policy and valid eviction notices")
		}
		last := map[uint32]uint64{}
		var foreignFirst *windowRoundFrame
		if !committed {
			foreignFirst = writeConsumerFutureHeldData(t, fixture, 1, 1280, last)
		}
		payloadBytes, predecessorCost := 1280, largeCost
		if !committed {
			payloadBytes, predecessorCost = 32, smallCost
		}
		first := writeConsumerFutureHeldData(t, fixture, 0, payloadBytes, last)
		second := writeConsumerFutureHeldData(t, fixture, 0, payloadBytes, last)
		var suffix, suffixRetries []*windowRoundFrame
		suffixCount := 3
		if !committed {
			suffixCount = 1
		}
		for range suffixCount {
			wire := writeConsumerFutureHeldData(t, fixture, 0, 32, last)
			suffix = append(suffix, wire)
			retry := &windowRoundFrame{bytes: MessagePoolCopy(wire.bytes), pack: wire.pack}
			fixture.heldFrames = append(fixture.heldFrames, retry)
			suffixRetries = append(suffixRetries, retry)
		}
		var foreignTail *windowRoundFrame
		if !committed {
			foreignTail = writeConsumerFutureHeldData(t, fixture, 1, 1280, last)
			fixture.forward(foreignTail, fixture.receiverIn)
		}
		sequence := findConsumerAdjacentSequence(t, fixture, first)
		firstNumber := first.pack.SequenceNumber
		if firstNumber != 1 || first.pack.Head || second.pack.Head || first.pack.Nack || second.pack.Nack ||
			first.pack.ContractFrame != nil || second.pack.ContractFrame != nil ||
			second.pack.SequenceNumber != firstNumber+1 || sequence.nextSequenceNumber != firstNumber ||
			sequence.receiveContract == nil || !sequence.receiveContract.verifiedReceiveProofSet {
			t.Fatal("suffix fixture did not capture actual consecutive originals behind verified authority")
		}
		for index, wire := range suffix {
			if wire.pack.SequenceNumber != firstNumber+2+uint64(index) || wire.pack.Head || wire.pack.ContractFrame != nil {
				t.Fatal("suffix source did not preserve its actual sequence positions")
			}
			fixture.forward(wire, fixture.receiverIn)
		}
		fixture.acknowledge()
		owners := make(map[uint64]*receiveItem, len(suffix))
		decodedOwners := make(map[uint64]*decodedPackOwner, len(suffix))
		for _, wire := range suffix {
			number := wire.pack.SequenceNumber
			item := sequence.receiveQueue.GetBySequenceNumber(number)
			if item == nil || item.committed != committed || item.QueueByteCount() != smallCost ||
				item.memoryBudget != rx || item.decodedOwner == nil ||
				!item.decodedOwner.receivePack.receiveConsumerData || !item.decodedOwner.receivePack.receivePublishedReliable {
				t.Fatal("actual suffix did not retain its complete physical original")
			}
			owners[number], decodedOwners[number] = item, item.decodedOwner
		}
		var sender *SendSequence
		fixture.sender.sendBuffer.mutex.Lock()
		for _, candidate := range fixture.sender.sendBuffer.sendSequencesBySequenceId {
			if candidate.sequenceId == RequireIdFromBytes(first.pack.SequenceId) {
				sender = candidate
				break
			}
		}
		fixture.sender.sendBuffer.mutex.Unlock()
		if sender == nil {
			t.Fatal("suffix source lost its exact send generation")
		}
		leased := func() int {
			n := 0
			for _, item := range sender.sendItems {
				if item != nil && owners[item.sequenceNumber] != nil && item.selectiveAcked {
					n++
				}
			}
			return n
		}
		wantLeases := 0
		if committed {
			wantLeases = suffixCount
		}
		if rx.UsedByteCount() != rxLimit || head.UsedByteCount() != 0 || root.UsedByteCount() != rxLimit ||
			sequence.holdPromise.set != committed || leased() != wantLeases ||
			fixture.deliveredCount != 0 || fixture.ackedCount != 0 || len(fixture.receiverOut) != 0 {
			t.Fatal("suffix fixture did not fill immutable RX with actual custody and selective leases")
		}
		if committed && (sequence.holdPromise.reservedByteCount != 2*smallCost ||
			sequence.heldHeadRepair.reservedByteCount != repairCost || sequence.heldHeadRepair.eligibleCount != suffixCount) {
			t.Fatal("committed suffix has no complete predecessor and copied-proof reservation")
		}
		path := TransferPath{SourceId: fixture.sender.ClientId(), DestinationId: fixture.receiver.ClientId()}
		for _, wire := range []*windowRoundFrame{first, second} {
			scan, status := scanReceiveMemoryPack(wire.bytes, path)
			if status != receiveMemoryPackScanReady || !scan.loanSafe || scan.retainedByteCount != predecessorCost ||
				scan.frameCount != 1 || scan.consumerFrameCount != 1 || scan.head || scan.nack || scan.contractAhead {
				t.Fatal("actual predecessor does not have the required complete child envelope")
			}
		}
		firstId := RequireIdFromBytes(first.pack.MessageId)
		firstTarget.Store(&firstId)
		armed.Store(true)
		fixture.forward(first, fixture.receiverIn)
		select {
		case <-entered:
		default:
			t.Fatal("actual Client did not pause after publication and before Pack admission")
		}
		var firstOwner *decodedPackOwner
		func() {
			root.admissionLock.Lock()
			defer root.admissionLock.Unlock()
			for current := sequence.holdPromise.ingressHead; current != nil; current = current.ingressNext {
				if current.decodedOwner != nil && current.decodedOwner.receiveItem.messageId == firstId {
					firstOwner = current.decodedOwner
					return
				}
			}
		}()
		if firstOwner == nil || firstOwner.receiveItem.memoryBudget != head || firstOwner.receiveItem.QueueByteCount() != predecessorCost ||
			!firstOwner.receivePack.receivePublishedReliable || !firstOwner.receivePack.receiveConsumerData ||
			firstOwner.receivePack.ingressSequence != sequence || firstOwner.receivePack.ingressLoanByteCount != 0 ||
			sequence.packQueueCount.Load() != 0 || sequence.packQueueByteCount.Load() != 0 || len(sequence.packs) != 0 ||
			sequence.holdPromise.ingressFloor != firstNumber || sequence.nextSequenceNumber != firstNumber ||
			rx.UsedByteCount() != rxLimit || head.UsedByteCount() != predecessorCost {
			t.Fatal("published first predecessor was not a complete child owner before logical handoff")
		}

		beforeRxReserved, beforeRxReleased := rx.Counts()
		beforeHeadReserved, beforeHeadReleased := head.Counts()
		beforeEvicted := fixture.receiver.receiveQueueEvictionCount.Load()
		beforeTentative := fixture.receiver.receiveQueueTentativeEvictionCount.Load()
		beforeCommits := fixture.receiver.receiveQueueCommitCount.Load()
		beforeDrops, beforeDropBytes := fixture.receiver.receiveQueueDropCount.Load(), fixture.receiver.receiveQueueDropByteCount.Load()
		before := sequence.receiveContract
		beforePrefix := sequence.holdPromise.reservedByteCount
		beforeRepair := sequence.heldHeadRepair.reservedByteCount
		beforeEligible := sequence.heldHeadRepair.eligibleCount
		offer := decodeConsumerAdjacentOwner(t, fixture, sequence, second.bytes)
		// The owned decoder helper stops before Client's publication. Copy
		// only the same actual decoded role facts under the same root lock;
		// no wire identity, order, contract or acknowledgement is changed.
		root.admissionLock.Lock()
		offer.receiveConsumerData = receiveHeadRecoveryPack(offer.Pack)
		offer.receivePublishedReliable = !offer.Pack.Nack
		root.admissionLock.Unlock()
		t.Cleanup(func() {
			if offer != nil {
				offer.messagePoolReturn()
			}
		})
		messageBytes := offer.MessageByteCount
		if offer.decodedOwner.receiveItem.memoryBudget != head || offer.decodedOwner.receiveItem.QueueByteCount() != predecessorCost ||
			!offer.receiveConsumerData || !offer.receivePublishedReliable || offer.Pack.Head || offer.Pack.Nack ||
			head.UsedByteCount() != 2*predecessorCost || offer.ingressSequence != sequence || offer.ingressLoanByteCount != 0 ||
			root.UsedByteCount() != rxLimit+2*predecessorCost || root.UsedByteCount() > root.TotalByteCount() {
			t.Fatal("real second predecessor did not acquire its own bounded future child claim")
		}
		accepted, err := sequence.receive(offer)
		if accepted {
			offer = nil
			t.Fatal("future child unexpectedly acquired held or delivery ownership")
		}
		if err != nil {
			t.Fatalf("ordinary future refusal unexpectedly failed its generation: %v", err)
		}
		offer.messagePoolReturn()
		offer = nil
		synctest.Wait()
		afterRxReserved, afterRxReleased := rx.Counts()
		afterHeadReserved, afterHeadReleased := head.Counts()
		changed := afterRxReserved != beforeRxReserved || afterRxReleased != beforeRxReleased ||
			fixture.receiver.receiveQueueEvictionCount.Load() != beforeEvicted ||
			fixture.receiver.receiveQueueTentativeEvictionCount.Load() != beforeTentative ||
			fixture.receiver.receiveQueueCommitCount.Load() != beforeCommits || len(fixture.receiverOut) != 0 || leased() != wantLeases ||
			sequence.holdPromise.reservedByteCount != beforePrefix || sequence.heldHeadRepair.reservedByteCount != beforeRepair ||
			sequence.heldHeadRepair.eligibleCount != beforeEligible
		for number, owner := range owners {
			changed = changed || sequence.receiveQueue.GetBySequenceNumber(number) != owner || owner.committed != committed ||
				owner.decodedOwner != decodedOwners[number] || owner.memoryBudget != rx || owner.QueueByteCount() != smallCost
		}
		if afterHeadReserved != beforeHeadReserved+predecessorCost || afterHeadReleased != beforeHeadReleased+predecessorCost ||
			head.UsedByteCount() != predecessorCost || sequence.nextSequenceNumber != firstNumber ||
			sequence.holdPromise.ingressFloor != firstNumber || sequence.receiveContract != before || sequence.ctx.Err() != nil ||
			fixture.deliveredCount != 0 || fixture.ackedCount != 0 ||
			fixture.receiver.receiveQueueDropCount.Load() != beforeDrops+1 ||
			fixture.receiver.receiveQueueDropByteCount.Load() != beforeDropBytes+uint64(messageBytes) {
			t.Fatal("future refusal lost its independent refund, logical drop accounting or no-ACK boundary")
		}
		t.Logf("[recovery-suffix] committed=%t changed=%t rx=%d child=%d cost=%d", committed, changed, rx.UsedByteCount(), head.UsedByteCount(), predecessorCost)
		// Process a real old eviction notice before cleanup retries. This keeps
		// the counterfactual honest while the fixed path publishes no notice.
		fixture.acknowledge()
		release()
		synctest.Wait()
		fixture.acknowledge()
		fixture.forward(second, fixture.receiverIn)
		fixture.acknowledge()
		for _, retry := range suffixRetries {
			fixture.forward(retry, fixture.receiverIn)
			fixture.acknowledge()
		}
		wantDelivered := 2 + suffixCount
		if foreignFirst != nil {
			fixture.forward(foreignFirst, fixture.receiverIn)
			fixture.acknowledge()
			wantDelivered += 2
		}
		if fixture.deliveredCount != wantDelivered || fixture.ackedCount != wantDelivered || sequence.receiveQueue.Len() != 0 ||
			rx.UsedByteCount() != 0 || head.UsedByteCount() != 0 || packBudget.UsedByteCount() != 0 || root.UsedByteCount() != 0 ||
			len(logger.messages()) != 0 || sequence.ctx.Err() != nil {
			t.Fatal("genuine retries did not restore ordered progress and return every physical owner")
		}
		if changed {
			cause = "FUTURE_CHILD_REFUSAL_CHANGED_SUFFIX"
		}
	})
}
