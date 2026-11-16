package connect

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/v2026/protocol"
)

// These transaction tests own real carrier/payload roots. Only the existing
// heap comparators stop the worker; no production hook or owner field is added.
func receivePressureInterleavingOwner(t *testing.T, buffer *ReceiveBuffer, sequence *ReceiveSequence, id Id, number uint64) *decodedPackOwner {
	t.Helper()
	owner := buffer.takeReceiveMemoryOwner(sequence, id, number, receivePressureTestCost)
	if owner == nil {
		t.Fatal("interleaving could not acquire its paid owner")
	}
	owner.receivePack.Pack = &owner.pack
	owner.frames[0] = protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider, Raw: true, MessageBytes: MessagePoolGet(1280)}
	owner.pack.Frames = append(owner.pack.Frames, &owner.frames[0])
	item := &owner.receiveItem
	item.decodedOwner, item.ack = owner, true
	item.frames, item.messageByteCount = owner.pack.Frames, 1280
	item.transferFrameBytes = MessagePoolGet(1280)
	if MessagePoolRootByteCount(item.transferFrameBytes)+MessagePoolRootByteCount(item.frames[0].MessageBytes)+owner.metadataQueueByteCount() != item.QueueByteCount() {
		item.messagePoolReturn()
		t.Fatal("interleaving physical roots do not match the paid owner class")
	}
	return owner
}

// Ingress consumes the old escrow after the worker snapshots it and while
// AscendingItems is sorting. Installation must defer without losing the new
// earlier proposal or evicting a tail from a stale credit snapshot.
func TestReceiveMemoryPressurePlanningDefersForActualIngressOwner(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		root, rx, buffer, sequence := receiveMemoryTestState(t, 3*receivePressureTestCost)
		sequence.client = &Client{}
		sequence.receiveContract = &sequenceContract{}
		sequence.closeNotify = make(chan struct{}, 1)
		sequence.receiveBufferSettings.ReceiveHoldPolicy = ReceiveHoldCommittedPrefix
		t.Cleanup(func() {
			sequence.releaseHoldPromise()
			for _, item := range sequence.receiveQueue.Clear() {
				item.messagePoolReturn()
			}
		})
		for _, number := range []uint64{10, 11} {
			owner := receivePressureInterleavingOwner(t, buffer, sequence, NewId(), number)
			sequence.receiveQueue.Add(&owner.receiveItem)
			sequence.adoptReceiveMemoryOwner(&owner.receiveItem)
		}
		oldId, nextId := NewId(), NewId()
		if !rx.TryReserve(receivePressureTestCost) {
			t.Fatal("old pressure escrow setup")
		}
		request := receiveMemoryPressureRequest{messageId: nextId, sequenceNumber: 1, queueByteCount: receivePressureTestCost, messageByteCount: 1280}
		sequence.memoryPressure = receiveMemoryPressure{
			active:    receiveMemoryPressureRequest{messageId: oldId, sequenceNumber: 8, queueByteCount: receivePressureTestCost, messageByteCount: 1280},
			activeSet: true, ready: true, reservedByteCount: receivePressureTestCost, pending: request, pendingSet: true,
		}
		entered, resume, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
		cmp := sequence.receiveQueue.cmp
		stopped := false
		sequence.receiveQueue.cmp = func(a, b *receiveItem) int {
			if !stopped {
				stopped = true
				close(entered)
				<-resume
			}
			return cmp(a, b)
		}
		t.Cleanup(func() {
			select {
			case <-resume:
			default:
				close(resume)
			}
			<-done
		})
		before := root.Stats()
		go func() { sequence.applyReceiveMemoryPressure(); close(done) }()
		<-entered
		owner := receivePressureInterleavingOwner(t, buffer, sequence, oldId, 8)
		defer func() {
			if owner != nil {
				owner.receiveItem.messagePoolReturn()
			}
		}()
		if sequence.memoryPressure.owner != owner || sequence.memoryPressure.reservedByteCount != 0 || root.Stats() != before {
			t.Fatal("actual ingress did not consume precisely the snapshotted old escrow")
		}
		close(resume)
		<-done
		sequence.receiveQueue.cmp = cmp
		state := sequence.memoryPressure
		if state.owner != owner || !state.pendingSet || state.pending != request || state.active.messageId != oldId ||
			state.borrowedByteCount != receivePressureTestCost || sequence.receiveQueue.Len() != 2 || root.Stats() != before ||
			sequence.client.receiveQueueTentativeEvictionCount.Load() != 0 {
			t.Fatal("stale planning credit replaced an in-flight owner or lost its earlier request")
		}
		owner.receiveItem.messagePoolReturn()
		owner = nil
		sequence.applyReceiveMemoryPressure()
		state = sequence.memoryPressure
		if state.owner != nil || state.pendingSet || !state.ready || state.active != request ||
			state.reservedByteCount != receivePressureTestCost || sequence.receiveQueue.Len() != 2 || root.Stats() != before {
			t.Fatal("actual old-owner return did not let the earlier proposal install without extra eviction")
		}
		owner = receivePressureInterleavingOwner(t, buffer, sequence, nextId, 1)
		sequence.adoptReceiveMemoryOwner(&owner.receiveItem)
		owner.receiveItem.messagePoolReturn()
		owner = nil
		sequence.releaseHoldPromise()
		for _, item := range sequence.receiveQueue.Clear() {
			item.messagePoolReturn()
		}
		if root.UsedByteCount() != 0 || rx.UsedByteCount() != 0 || sequence.memoryPressure != (receiveMemoryPressure{}) || sequence.holdPromise.ingressHead != nil {
			t.Fatal("planning interleaving failed terminal owner retirement")
		}
		t.Log("[rx-pressure-plan-install] ingress_after_snapshot=true proposal_preserved=true extra_evictions=0 actual_retry_adopted=true root=0")
	})
}

// Cancellation lands between two removals, after the first real owner's
// roots have returned into escrow. The bounded worker batch must preserve
// every remaining physical claim until return and then close its exact credit.
func TestReceiveMemoryPressureCancellationDuringActualReclaimBatch(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		root, rx, buffer, sequence := receiveMemoryTestState(t, 5*receivePressureTestCost)
		sequence.ctx, sequence.cancel = context.WithCancel(sequence.ctx)
		defer sequence.cancel()
		sequence.client = &Client{}
		sequence.receiveContract = &sequenceContract{}
		sequence.closeNotify = make(chan struct{}, 1)
		sequence.receiveBufferSettings.ReceiveHoldPolicy = ReceiveHoldCommittedPrefix
		t.Cleanup(func() {
			sequence.releaseHoldPromise()
			for _, item := range sequence.receiveQueue.Clear() {
				item.messagePoolReturn()
			}
		})
		for number := uint64(10); number <= 14; number++ {
			owner := receivePressureInterleavingOwner(t, buffer, sequence, NewId(), number)
			sequence.receiveQueue.Add(&owner.receiveItem)
			sequence.adoptReceiveMemoryOwner(&owner.receiveItem)
		}
		sequence.memoryPressure.pending = receiveMemoryPressureRequest{messageId: NewId(), sequenceNumber: 1, queueByteCount: 3 * receivePressureTestCost, messageByteCount: 1280}
		sequence.memoryPressure.pendingSet = true
		entered, resume, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
		cmp := sequence.receiveQueue.maxHeap.cmp
		stopped := false
		sequence.receiveQueue.maxHeap.cmp = func(a, b *receiveItem) int {
			root.admissionLock.Lock()
			afterFirstReturn := sequence.memoryPressure.reservedByteCount == receivePressureTestCost && sequence.memoryPressure.reclaimOwner == nil
			root.admissionLock.Unlock()
			if afterFirstReturn && !stopped {
				stopped = true
				close(entered)
				<-resume
			}
			return cmp(a, b)
		}
		t.Cleanup(func() {
			select {
			case <-resume:
			default:
				close(resume)
			}
			<-done
		})
		beforeRoots, beforeBytes := messagePoolOutstandingSnapshot()
		go func() { sequence.applyReceiveMemoryPressure(); close(done) }()
		<-entered
		afterRoots, afterBytes := messagePoolOutstandingSnapshot()
		if afterRoots != beforeRoots-2 || afterBytes != beforeBytes-4096 || root.UsedByteCount() != 5*receivePressureTestCost ||
			sequence.memoryPressure.reservedByteCount != receivePressureTestCost || sequence.memoryPressure.ready ||
			sequence.client.receiveQueueTentativeEvictionCount.Load() != 1 {
			t.Fatal("reclaim barrier did not follow one actual fully paid root return")
		}
		sequence.cancel()
		if root.TryReserve(1) {
			root.Release(1)
			t.Fatal("cancellation freed physical custody before the reclaim batch returned it")
		}
		close(resume)
		<-done
		sequence.receiveQueue.maxHeap.cmp = cmp
		if sequence.ctx.Err() == nil || sequence.memoryPressure.reclaimOwner != nil ||
			sequence.memoryPressure.reservedByteCount != 3*receivePressureTestCost || sequence.receiveQueue.Len() != 2 ||
			root.UsedByteCount() != 5*receivePressureTestCost || sequence.client.receiveQueueTentativeEvictionCount.Load() != 3 {
			t.Fatal("cancellation split the exact reclaim transaction or released a still-live held owner")
		}
		sequence.releaseHoldPromise()
		for _, item := range sequence.receiveQueue.Clear() {
			item.messagePoolReturn()
		}
		if root.UsedByteCount() != 0 || rx.UsedByteCount() != 0 || sequence.memoryPressure != (receiveMemoryPressure{}) || sequence.holdPromise.ingressHead != nil {
			t.Fatal("canceled reclaim batch did not retire escrow and remaining actual roots")
		}
		t.Log("[rx-pressure-cancel-reclaim] canceled_after_actual_return=1 batch_returns=3 live_claims_preserved=true escrow_closed=true root=0")
	})
}
