// A reserved decoder owner precedes publication of its reliable payload role.
// Root linkage supplies no backlog authority; established generations have a
// separate, independently paid permission for their exact current original.
package connect

import (
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Pause an actual paid tail between owned decode and Client publication. The
// signed established generation can fund its current original independently;
// the unfinished tail must remain neither backlog nor delivery authority.
func TestReceiveConsumerEstablishedCurrentHeadPreservesUnpublishedTail(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		root := NewTransferMemoryBudget(2 * 6656)
		rx := NewTransferMemoryBudgetWithParent(6656, root)
		child := NewTransferMemoryBudgetWithParent(6656, root)
		t.Cleanup(func() {
			for _, budget := range []*TransferMemoryBudget{rx, child, root} {
				assertRetainedBudgetBalance(t, budget)
				if budget.Stats().QuotaByteCount != 0 {
					t.Error("publication fixture retained logical quota")
				}
			}
		})
		fixture, log := newConsumerTentativeHeadFixture(t, rx, child)
		last := map[uint32]uint64{}
		foreignHead := writeConsumerFutureHeldData(t, fixture, 1, 1280, last)
		foreignTail := writeConsumerFutureHeldData(t, fixture, 1, 1280, last)
		current := writeConsumerFutureHeldData(t, fixture, 0, 1280, last)
		tail := writeConsumerFutureHeldData(t, fixture, 0, 32, last)
		foreign := findConsumerAdjacentSequence(t, fixture, foreignHead)
		sequence := findConsumerAdjacentSequence(t, fixture, current)
		if current.pack.SequenceNumber != 1 || current.pack.Head || current.pack.ContractFrame != nil ||
			tail.pack.SequenceNumber != 2 || tail.pack.Head || tail.pack.ContractFrame != nil ||
			sequence.nextSequenceNumber != 1 || sequence.receiveContract == nil || !sequence.receiveContract.verifiedReceiveProofSet ||
			foreign.nextSequenceNumber != foreignHead.pack.SequenceNumber || foreign.receiveContract == nil ||
			rx.UsedByteCount() != 0 || fixture.deliveredCount != 0 || fixture.ackedCount != 0 {
			t.Fatal("publication fixture did not establish actual signed current authority")
		}
		fixture.forward(foreignTail, fixture.receiverIn)
		foreignItem := foreign.receiveQueue.GetByMessageId(RequireIdFromBytes(foreignTail.pack.MessageId))
		if foreignItem == nil || foreignItem.committed || foreignItem.QueueByteCount() != 5120 || foreignItem.memoryBudget != rx ||
			foreign.holdPromise.set || rx.UsedByteCount() != 5120 || len(fixture.receiverOut) != 0 {
			t.Fatal("publication fixture did not retain actual foreign custody")
		}
		path := TransferPath{SourceId: fixture.sender.ClientId(), DestinationId: fixture.receiver.ClientId()}
		pendingCarrier := MessagePoolCopy(tail.bytes)
		var pending *decodedPackOwner
		t.Cleanup(func() {
			// The scanner's whole claim includes the carrier. Return it before
			// the owner releases the corresponding physical budget reservation.
			MessagePoolReturn(pendingCarrier)
			if pending != nil {
				pending.release()
			}
		})
		tailScan, status := scanReceiveMemoryPack(pendingCarrier, path)
		if status != receiveMemoryPackScanReady || !tailScan.loanSafe || tailScan.retainedByteCount != 1536 ||
			tailScan.frameCount != 1 || tailScan.consumerFrameCount != 1 || tailScan.sequenceNumber != 2 {
			t.Fatal("publication fixture actual tail has no complete physical envelope")
		}
		pending = fixture.receiver.receiveBuffer.takeReceiveMemoryOwner(sequence, tailScan.messageId, tailScan.sequenceNumber, tailScan.retainedByteCount)
		if pending == nil {
			t.Fatal("publication fixture could not fund the actual pending tail")
		}
		body := pendingCarrier[tailScan.packOffset : tailScan.packOffset+tailScan.packByteCount]
		if decoded, ok := decodePackOwned(body, pending); !ok || decoded != pending {
			t.Fatal("publication fixture owned decode failed")
		}
		// This assignment is the decoder's real prepublication boundary. No
		// Client fields, publication flags, ordering state or authority are set.
		pending.receivePack.Pack = &pending.pack
		if pending.receivePack.receivePublishedReliable || pending.receivePack.receiveConsumerData || pending.receivePack.heldHeadRepairEligible ||
			!pending.receivePack.receiveMemoryOwned || pending.receivePack.ingressSequence != sequence ||
			sequence.holdPromise.ingressHead != &pending.receivePack || sequence.receiveQueue.Len() != 0 || sequence.holdPromise.set ||
			rx.UsedByteCount() != 6656 || root.UsedByteCount() != 6656 || child.UsedByteCount() != 0 ||
			len(pending.pack.Frames) != 1 || pending.pack.Frames[0].MessageType != protocol.MessageType_IpIpPacketFromProvider {
			t.Fatal("publication fixture crossed Client publication or lost paid pending custody")
		}
		currentScan, status := scanReceiveMemoryPack(current.bytes, path)
		if status != receiveMemoryPackScanReady || currentScan.retainedByteCount <= 1536 || currentScan.retainedByteCount > child.TotalByteCount() {
			t.Fatal("publication fixture current predecessor lost its funded envelope")
		}
		root.admissionLock.Lock()
		established := sequence.holdPromise.establishedContract
		unpublishedBacklog := sequence.receiveConsumerHeadBacklogWithLock(currentScan)
		root.admissionLock.Unlock()
		if !established || unpublishedBacklog {
			t.Fatal("unpublished tail became backlog authority or signed generation was not established")
		}
		reserved, released := root.Counts()
		decoded := &decodedTransferFrame{}
		ok, pressure := fixture.receiver.unmarshalReceiveMemoryFrame(current.bytes, decoded, true, path, false, sequenceTlsRoleServer, false)
		admitted := decoded.packOwner != nil
		paid := admitted && decoded.packOwner.receiveItem.memoryBudget == child &&
			decoded.packOwner.receiveItem.QueueByteCount() == currentScan.retainedByteCount &&
			decoded.packOwner.receivePack.ingressLoanByteCount == 0 &&
			child.UsedByteCount() == currentScan.retainedByteCount &&
			rx.UsedByteCount() == 6656 && root.UsedByteCount() == 6656+currentScan.retainedByteCount
		decoded.release()
		synctest.Wait()
		afterReserved, afterReleased := root.Counts()
		if !ok || !admitted || !paid || pressure ||
			afterReserved-reserved != currentScan.retainedByteCount || afterReleased-released != currentScan.retainedByteCount ||
			child.UsedByteCount() != 0 || rx.UsedByteCount() != 6656 || root.UsedByteCount() != 6656 {
			t.Fatalf("ESTABLISHED_CURRENT_DECODE_LOST_INDEPENDENT_CUSTODY ok=%t owner=%t paid=%t pressure=%t rx=%d child=%d root=%d reserved_delta=%d released_delta=%d",
				ok, admitted, paid, pressure, rx.UsedByteCount(), child.UsedByteCount(), root.UsedByteCount(),
				afterReserved-reserved, afterReleased-released)
		}
		if sequence.nextSequenceNumber != 1 || sequence.receiveQueue.Len() != 0 || fixture.deliveredCount != 0 || fixture.ackedCount != 0 {
			t.Fatal("unpublished tail changed actual current progress")
		}
		MessagePoolReturn(pendingCarrier)
		pendingCarrier = nil
		pending.release()
		pending = nil
		if rx.UsedByteCount() != 5120 || sequence.holdPromise.ingressHead != nil {
			t.Fatal("pending decoder return retained its unpublished claim")
		}
		fixture.forward(tail, fixture.receiverIn)
		held := sequence.receiveQueue.GetByMessageId(RequireIdFromBytes(tail.pack.MessageId))
		if held == nil || held.committed || held.decodedOwner == nil || held.QueueByteCount() != 1536 ||
			!held.decodedOwner.receivePack.receivePublishedReliable || !held.decodedOwner.receivePack.receiveConsumerData ||
			rx.UsedByteCount() != 6656 || root.UsedByteCount() != 6656 || sequence.holdPromise.set || len(fixture.receiverOut) != 0 {
			t.Fatal("actual Client did not publish the reliable tail role")
		}
		root.admissionLock.Lock()
		publishedBacklog := sequence.receiveConsumerHeadBacklogWithLock(currentScan)
		root.admissionLock.Unlock()
		if !publishedBacklog {
			t.Fatal("published actual reliable tail did not acquire its separate backlog authority")
		}
		observed, paid := false, false
		sequence.receiveBufferSettings.receiveOwnerStageForTest = func(actual *ReceiveSequence, owner *decodedPackOwner, stage string) {
			if actual == sequence && owner != nil && stage == "pack_released" && owner.receiveItem.messageId == currentScan.messageId {
				observed = true
				paid = owner.receiveItem.memoryBudget == child && owner.receiveItem.QueueByteCount() == currentScan.retainedByteCount &&
					child.UsedByteCount() == currentScan.retainedByteCount && rx.UsedByteCount() == 6656 &&
					root.UsedByteCount() == 6656+currentScan.retainedByteCount && owner.receivePack.ingressLoanByteCount == 0
			}
		}
		fixture.forward(current, fixture.receiverIn)
		fixture.acknowledge()
		if !observed || !paid || fixture.deliveredCount != 2 || fixture.ackedCount != 2 || sequence.nextSequenceNumber != 3 ||
			sequence.receiveQueue.Len() != 0 || sequence.ctx.Err() != nil || len(log.messages()) != 0 ||
			foreign.receiveQueue.GetByMessageId(foreignItem.messageId) != foreignItem || foreignItem.committed ||
			foreign.ctx.Err() != nil || rx.UsedByteCount() != 5120 || root.UsedByteCount() != 5120 || child.UsedByteCount() != 0 {
			t.Fatal("published reliable tail did not unlock paid current-head progress while preserving foreign custody")
		}
	})
}

// Returning payload roots can overlap recovery's root-locked metadata lookup.
// Published flags and identity remain stable until unlink; no returned frame
// is read, and a copy whose original left the queue gains no delivery authority.
func TestReceiveConsumerFutureProofDuringOriginalRootReturn(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		f := newConsumerFutureAdjacentFixture(t)
		originalOwner := f.original.decodedOwner
		originalId, originalNumber := f.original.messageId, f.original.sequenceNumber
		if originalOwner == nil || originalOwner.receivePack.Pack != &originalOwner.pack ||
			!originalOwner.receivePack.receivePublishedReliable || !originalOwner.receivePack.receiveConsumerData ||
			originalOwner.receivePack.heldHeadRepairEligible || originalOwner.pack.Nack || originalOwner.pack.Head || originalOwner.pack.ContractAhead ||
			f.sequence.receiveQueue.RemoveByMessageId(originalId) != f.original {
			t.Fatal("root-return control did not own the actual published tentative original")
		}
		var proof *ReceivePack
		var rootsReturned <-chan struct{}
		t.Cleanup(func() {
			if rootsReturned != nil {
				<-rootsReturned
			}
			if originalOwner != nil {
				carrier := originalOwner.receiveItem.transferFrameBytes
				originalOwner.receiveItem.transferFrameBytes = nil
				originalOwner.receiveItem.decodedOwner = nil
				MessagePoolReturn(carrier)
				originalOwner.release()
			}
			if proof != nil {
				proof.messagePoolReturn()
			}
		})
		// Match receiveItem.messagePoolReturn through its independent carrier
		// return, then pause owner.release before its root-locked unlink.
		carrier := f.original.transferFrameBytes
		f.original.transferFrameBytes = nil
		f.original.decodedOwner = nil
		MessagePoolReturn(carrier)
		start, returned := make(chan struct{}), make(chan struct{})
		rootsReturned = returned
		go func(owner *decodedPackOwner) {
			<-start
			returnDecodedPackMessageBytes(&owner.pack)
			close(returned)
		}(originalOwner)
		close(start)
		// The lookup may run on either side of payload return. Its result is
		// identical, and a race run verifies that only stable fields are read.
		proof = decodeConsumerAdjacentOwner(t, f.fixture, f.sequence, f.retry.bytes)
		<-returned
		cost := proof.decodedOwner.receiveItem.QueueByteCount()
		if originalOwner.pack.Frames != nil || originalOwner.pack.ContractFrame != nil ||
			originalOwner.receivePack.Pack != &originalOwner.pack || !originalOwner.receivePack.receivePublishedReliable ||
			!originalOwner.receivePack.receiveConsumerData || originalOwner.receiveItem.messageId != originalId ||
			originalOwner.receiveItem.sequenceNumber != originalNumber || originalOwner.receiveItem.memoryBudget != f.rx ||
			originalOwner.receivePack.ingressSequence != f.sequence || proof.decodedOwner.receiveItem.memoryBudget != f.child ||
			cost != decodedPackOwnerQueueByteCount+receiveMemoryScanCopyRoot(len(proof.Pack.ContractFrame.MessageBytes)) ||
			len(proof.Pack.Frames) != 0 || len(proof.TransferFrameBytes) != 0 || f.sequence.heldHeadRepair.owner != proof.decodedOwner ||
			f.rx.UsedByteCount() != 22016 || f.child.UsedByteCount() != cost || f.root.UsedByteCount() != 22016+cost {
			t.Fatal("payload return changed published identity or surrendered a still-linked physical claim")
		}
		var ackUpdates atomic.Int32
		f.sequence.ackWindow.afterUpdateForTest = func() { ackUpdates.Add(1) }
		accepted, err := f.sequence.receive(proof)
		if accepted || err != nil || f.sequence.nextSequenceNumber != 0 || f.sequence.receiveContract != nil ||
			len(f.sequence.openReceiveContracts) != 0 || ackUpdates.Load() != 0 || f.fixture.deliveredCount != 1 || f.fixture.ackedCount != 1 {
			t.Fatal("proof for an original in terminal return acquired prefix, contract or ACK authority")
		}
		originalOwner.release()
		originalOwner = nil
		if f.rx.UsedByteCount() != 20480 || f.child.UsedByteCount() != cost || f.root.UsedByteCount() != 20480+cost ||
			f.sequence.holdPromise.ingressHead != proof || proof.ingressNext != nil || f.sequence.heldHeadRepair.owner != proof.decodedOwner {
			t.Fatal("original unlink released the independent live proof owner")
		}
		proof.messagePoolReturn()
		proof = nil
		if f.rx.UsedByteCount() != 20480 || f.root.UsedByteCount() != 20480 || f.child.UsedByteCount() != 0 ||
			f.sequence.holdPromise.ingressHead != nil || f.sequence.heldHeadRepair.owner != nil ||
			f.sequence.heldHeadRepair.eligibleCount != 0 || f.sequence.heldHeadRepair.reservedByteCount != 0 {
			t.Fatal("final proof return retained a claim or refunded foreign custody")
		}
	})
}
