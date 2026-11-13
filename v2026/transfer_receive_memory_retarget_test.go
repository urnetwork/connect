package connect

import (
	"testing"
	"testing/synctest"
)

// An earlier original supersedes a later owner's pending downstream growth.
// The restored current decode may be borrowed again while the worker plans;
// that live owner must defer installation without discarding the earlier request.
func TestReceiveMemoryMixedGrowthRetargetSurvivesBorrowedDecode(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		const cost = receivePressureTestCost
		root, _, buffer, sequence := receivePrefixPressureTestState(t, 3)
		if !sequence.reserveHoldPromise(cost / 2) {
			t.Fatal("retarget prefix setup")
		}
		sequence.settleHoldPromise(true, 3, cost/2)
		for _, number := range []uint64{10, 11} {
			owner := receivePressureInterleavingOwner(t, buffer, sequence, NewId(), number)
			sequence.receiveQueue.Add(&owner.receiveItem)
			sequence.adoptReceiveMemoryOwner(&owner.receiveItem)
		}
		laterId, earlierId := NewId(), NewId()
		owner := receivePressureInterleavingOwner(t, buffer, sequence, laterId, 2)
		defer func() {
			if owner != nil {
				owner.receiveItem.messagePoolReturn()
			}
		}()
		if owner.receiveItem.growReceiveMemory(cost) {
			t.Fatal("full-root later growth unexpectedly fit")
		}
		sequence.requestReceiveMemoryGrowth(&owner.receiveItem, cost)
		if sequence.memoryPressure.owner != owner || sequence.memoryPressure.borrowedByteCount != cost/2 ||
			!sequence.memoryPressure.pendingSet || sequence.memoryPressure.pending.queueByteCount != 2*cost {
			t.Fatal("later actual mixed owner did not retain its larger pending request")
		}
		sequence.requestReceiveMemoryPressure(receiveMemoryPackScan{
			loanSafe: true, messageId: earlierId, sequenceNumber: 1, retainedByteCount: cost, messageByteCount: 1280,
		}, true)
		earlier := sequence.memoryPressure.pending
		if !sequence.memoryPressure.pendingSet || earlier.messageId != earlierId || earlier.sequenceNumber != 1 || earlier.queueByteCount != cost {
			t.Fatal("earlier original failed to supersede later pending downstream growth")
		}
		sequence.applyReceiveMemoryPressure()
		if sequence.memoryPressure.owner != owner || sequence.memoryPressure.pending != earlier || sequence.receiveQueue.Len() != 2 {
			t.Fatal("retarget installation bypassed the still-live mixed owner")
		}
		owner.receiveItem.messagePoolReturn()
		owner = nil
		if !sequence.memoryPressure.ready || sequence.memoryPressure.reservedByteCount != cost/2 ||
			sequence.holdPromise.reservedByteCount != cost/2 || sequence.memoryPressure.pending != earlier || root.UsedByteCount() != 3*cost {
			t.Fatal("actual return lost current decode readiness or the earlier proposal")
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
		go func() { sequence.applyReceiveMemoryPressure(); close(done) }()
		<-entered
		owner = receivePressureInterleavingOwner(t, buffer, sequence, laterId, 2)
		if sequence.memoryPressure.owner != owner || sequence.memoryPressure.borrowedByteCount != cost/2 ||
			owner.receivePack.ingressLoanByteCount != cost/2 || sequence.memoryPressure.reservedByteCount != 0 || sequence.holdPromise.reservedByteCount != 0 {
			t.Fatal("restored later decode did not consume exactly its disjoint paid portions")
		}
		close(resume)
		<-done
		sequence.receiveQueue.cmp = cmp
		if sequence.memoryPressure.owner != owner || !sequence.memoryPressure.pendingSet || sequence.memoryPressure.pending != earlier ||
			sequence.memoryPressure.active.messageId != laterId || sequence.receiveQueue.Len() != 2 ||
			sequence.client.receiveQueueTentativeEvictionCount.Load() != 0 || root.UsedByteCount() != 3*cost {
			t.Fatal("borrowed current decode let retarget discard physical custody or its earlier proposal")
		}
		owner.receiveItem.messagePoolReturn()
		owner = nil
		sequence.applyReceiveMemoryPressure()
		if !sequence.memoryPressure.ready || sequence.memoryPressure.active != earlier || sequence.memoryPressure.pendingSet ||
			sequence.memoryPressure.reservedByteCount != cost/2 || sequence.holdPromise.reservedByteCount != cost/2 ||
			sequence.receiveQueue.Len() != 2 || root.UsedByteCount() != 3*cost {
			t.Fatal("actual mixed return failed to fund the earlier exact retry without extra eviction")
		}
		owner = receivePressureInterleavingOwner(t, buffer, sequence, earlierId, 1)
		sequence.adoptReceiveMemoryOwner(&owner.receiveItem)
		owner.receiveItem.messagePoolReturn()
		owner = nil
		sequence.releaseHoldPromise()
		for _, item := range sequence.receiveQueue.Clear() {
			item.messagePoolReturn()
		}
		if root.UsedByteCount() != 0 || sequence.memoryPressure != (receiveMemoryPressure{}) || sequence.holdPromise.ingressHead != nil {
			t.Fatal("mixed retarget did not retire all physical and promised custody")
		}
		t.Log("[rx-prefix-pressure-retarget] pending_growth_replaced_by_earlier=true current_decode_borrowed_during_plan=true proposal_preserved=true extra_evictions=0 root=0")
	})
}
