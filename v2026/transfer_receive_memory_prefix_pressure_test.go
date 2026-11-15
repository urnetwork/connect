package connect

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/v2026/protocol"
)

// These core transactions retain real pooled roots through the existing
// interleaving owner helper. Real encoded provider delivery has separate roots.
func receivePrefixPressureTestState(t *testing.T, cells ByteCount) (*TransferMemoryBudget, *TransferMemoryBudget, *ReceiveBuffer, *ReceiveSequence) {
	t.Helper()
	root, rx, buffer, sequence := receiveMemoryTestState(t, cells*receivePressureTestCost)
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
	return root, rx, buffer, sequence
}

// Real return must restore C as disjoint L and C-L, then erase the old owner
// association before that same pooled address can belong to another sequence.
func TestReceiveMemoryFreshPrefixGrowthPartitionsCreditAcrossPoolReuse(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		const cost = receivePressureTestCost
		root, rx, buffer, sequence := receivePrefixPressureTestState(t, 3)
		if !sequence.reserveHoldPromise(cost / 2) {
			t.Fatal("mixed promise setup")
		}
		sequence.settleHoldPromise(true, 3, cost/2)
		for _, number := range []uint64{10, 11} {
			owner := receivePressureInterleavingOwner(t, buffer, sequence, NewId(), number)
			sequence.receiveQueue.Add(&owner.receiveItem)
			sequence.adoptReceiveMemoryOwner(&owner.receiveItem)
		}
		id := NewId()
		owner := receivePressureInterleavingOwner(t, buffer, sequence, id, 1)
		defer func() {
			if owner != nil {
				owner.receiveItem.messagePoolReturn()
			}
		}()
		before := root.Stats()
		if owner.receivePack.ingressLoanByteCount != cost/2 || owner.receiveItem.growReceiveMemory(cost/2) {
			t.Fatal("mixed original did not isolate failed downstream growth")
		}
		sequence.requestReceiveMemoryGrowth(&owner.receiveItem, cost/2)
		state := sequence.memoryPressure
		if !state.activeSet || state.owner != owner || state.borrowedByteCount != cost/2 || !state.pendingSet ||
			state.pending.queueByteCount != 3*cost/2 || root.UsedByteCount() != 3*cost || rx.UsedByteCount() != 3*cost {
			t.Fatal("fresh growth double-attributed the prefix portion or lost its paid owner")
		}
		sequence.applyReceiveMemoryPressure()
		if sequence.memoryPressure != state || sequence.receiveQueue.Len() != 2 {
			t.Fatal("live mixed owner was reclaimed before its roots returned")
		}
		oldOwner := owner
		owner.receiveItem.messagePoolReturn()
		owner = nil
		state = sequence.memoryPressure
		if state.owner != nil || state.borrowedByteCount != 0 || state.reservedByteCount != cost/2 || !state.ready ||
			sequence.holdPromise.reservedByteCount != cost/2 || sequence.holdPromise.ingressLoanByteCount != 0 ||
			root.UsedByteCount() != before.UsedByteCount || sequence.holdPromise.ingressHead == nil {
			t.Fatal("actual mixed return did not restore two disjoint paid portions")
		}
		otherRoot, _, otherBuffer, otherSequence := receivePrefixPressureTestState(t, decodedPackOwnerPoolShards)
		var otherOwners []*decodedPackOwner
		defer func() {
			for _, other := range otherOwners {
				other.receiveItem.messagePoolReturn()
			}
		}()
		reused := false
		for index := range decodedPackOwnerPoolShards {
			other := receivePressureInterleavingOwner(t, otherBuffer, otherSequence, NewId(), uint64(100+index))
			otherOwners = append(otherOwners, other)
			reused = reused || other == oldOwner
		}
		if !reused || sequence.memoryPressure != state || root.UsedByteCount() != 3*cost ||
			sequence.holdPromise.reservedByteCount != cost/2 {
			t.Fatal("actual owner-pool reuse inherited an old sequence's credit association")
		}
		for _, other := range otherOwners {
			other.receiveItem.messagePoolReturn()
		}
		otherOwners = nil
		if otherRoot.UsedByteCount() != 0 || sequence.memoryPressure != state || root.UsedByteCount() != 3*cost {
			t.Fatal("foreign reused-owner return altered the retired association")
		}
		sequence.applyReceiveMemoryPressure()
		state = sequence.memoryPressure
		if state.reservedByteCount != cost || sequence.holdPromise.reservedByteCount != cost/2 ||
			!state.ready || state.pendingSet || state.active.queueByteCount != 3*cost/2 ||
			sequence.receiveQueue.Len() != 1 || sequence.receiveQueue.GetBySequenceNumber(10) == nil || root.UsedByteCount() != 5*cost/2 {
			t.Fatal("mixed recovery did not combine prefix and returned tail exactly once")
		}
		foreign := root.Available()
		if !root.TryReserve(foreign) {
			t.Fatal("mixed competing claim setup")
		}
		defer func() {
			if foreign != 0 {
				root.Release(foreign)
			}
		}()
		if other := buffer.takeReceiveMemoryOwner(sequence, NewId(), 1, cost); other != nil {
			other.release()
			t.Fatal("new identity consumed exact-head prefix protection")
		}
		owner = receivePressureInterleavingOwner(t, buffer, sequence, id, 1)
		if !owner.receiveItem.growReceiveMemory(cost/2) || owner.receiveItem.QueueByteCount() != 3*cost/2 ||
			sequence.memoryPressure.borrowedByteCount != cost || owner.receivePack.ingressLoanByteCount != cost/2 {
			t.Fatal("full-root retry could not spend the two exact portions")
		}
		sequence.adoptReceiveMemoryOwner(&owner.receiveItem)
		owner.receiveItem.messagePoolReturn()
		owner = nil
		sequence.releaseHoldPromise()
		for _, item := range sequence.receiveQueue.Clear() {
			item.messagePoolReturn()
		}
		root.Release(foreign)
		foreign = 0
		if root.UsedByteCount() != 0 || rx.UsedByteCount() != 0 || sequence.memoryPressure != (receiveMemoryPressure{}) || sequence.holdPromise.ingressHead != nil {
			t.Fatal("mixed partition or reused owner failed final retirement")
		}
		t.Log("[rx-prefix-pressure-partition] prefix_plus_pressure_equals_claim=true actual_pool_reuse=true foreign_return_isolated=true one_tail=true root=0")
	})
}

// An exact retry can be funded entirely by the promise after another pending
// prefix owner returns. It still needs an owner association for failed growth.
func TestReceiveMemoryPrefixOnlyPressureOwnerCanRequestGrowth(t *testing.T) {
	assertMessagePoolOwnership(t)
	const cost = receivePressureTestCost
	root, _, buffer, sequence := receivePrefixPressureTestState(t, 3)
	if !sequence.reserveHoldPromise(cost) {
		t.Fatal("prefix-only promise setup")
	}
	sequence.settleHoldPromise(true, 3, cost)
	tail := receivePressureInterleavingOwner(t, buffer, sequence, NewId(), 10)
	sequence.receiveQueue.Add(&tail.receiveItem)
	sequence.adoptReceiveMemoryOwner(&tail.receiveItem)
	if !root.TryReserve(cost) {
		t.Fatal("prefix-only competing claim")
	}
	foreign := true
	defer func() {
		if foreign {
			root.Release(cost)
		}
	}()
	id := NewId()
	sequence.memoryPressure.pending = receiveMemoryPressureRequest{messageId: id, sequenceNumber: 1, queueByteCount: cost, messageByteCount: 1280}
	sequence.memoryPressure.pendingSet = true
	sequence.applyReceiveMemoryPressure()
	if !sequence.memoryPressure.ready || sequence.memoryPressure.reservedByteCount != 0 || sequence.receiveQueue.Len() != 1 {
		t.Fatal("prefix-only decoded request unnecessarily evicted a tail")
	}
	owner := receivePressureInterleavingOwner(t, buffer, sequence, id, 1)
	defer func() {
		if owner != nil {
			owner.receiveItem.messagePoolReturn()
		}
	}()
	if sequence.memoryPressure.owner != owner || sequence.memoryPressure.borrowedByteCount != 0 || owner.receivePack.ingressLoanByteCount != cost {
		t.Fatal("prefix-only exact retry has no real-owner association")
	}
	if owner.receiveItem.growReceiveMemory(cost) {
		t.Fatal("full-root prefix-only growth unexpectedly succeeded")
	}
	sequence.requestReceiveMemoryGrowth(&owner.receiveItem, cost)
	if !sequence.memoryPressure.pendingSet || sequence.memoryPressure.pending.queueByteCount != 2*cost {
		t.Fatal("zero pressure loan hid the actual paid prefix owner's growth request")
	}
	owner.receiveItem.messagePoolReturn()
	owner = nil
	sequence.applyReceiveMemoryPressure()
	if sequence.memoryPressure.reservedByteCount != cost || sequence.holdPromise.reservedByteCount != cost || !sequence.memoryPressure.ready || root.Available() != 0 {
		t.Fatal("prefix-only growth failed to preserve the full two-portion retry")
	}
	owner = receivePressureInterleavingOwner(t, buffer, sequence, id, 1)
	if !owner.receiveItem.growReceiveMemory(cost) {
		t.Fatal("prefix-only recovery failed at full root")
	}
	sequence.releaseHoldPromise()
	if owner.receiveItem.QueueByteCount() != 2*cost || owner.receivePack.ingressLoanByteCount != 0 || root.UsedByteCount() != 3*cost {
		t.Fatal("prefix closure released a live mixed owner")
	}
	owner.receiveItem.messagePoolReturn()
	owner = nil
	root.Release(cost)
	foreign = false
	if root.UsedByteCount() != 0 || sequence.holdPromise.ingressHead != nil {
		t.Fatal("closed prefix-only owner failed actual return")
	}
	t.Log("[rx-prefix-pressure-zero] prefix_only_owner_bound=true larger_request=true close_keeps_live_owner_paid=true root=0")
}

// Prefix borrowing between plan and install invalidates the old plan just as
// taking pressure escrow does. No tail may be removed from that stale census.
func TestReceiveMemoryPressurePlanningRechecksActualPrefixBorrow(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		const cost = receivePressureTestCost
		root, _, buffer, sequence := receivePrefixPressureTestState(t, 3)
		if !sequence.reserveHoldPromise(cost) {
			t.Fatal("planning prefix setup")
		}
		sequence.settleHoldPromise(true, 3, cost)
		for _, number := range []uint64{10, 11} {
			owner := receivePressureInterleavingOwner(t, buffer, sequence, NewId(), number)
			sequence.receiveQueue.Add(&owner.receiveItem)
			sequence.adoptReceiveMemoryOwner(&owner.receiveItem)
		}
		request := receiveMemoryPressureRequest{messageId: NewId(), sequenceNumber: 1, queueByteCount: 2 * cost, messageByteCount: 1280}
		sequence.memoryPressure.pending, sequence.memoryPressure.pendingSet = request, true
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
		owner := receivePressureInterleavingOwner(t, buffer, sequence, NewId(), 2)
		defer func() {
			if owner != nil {
				owner.receiveItem.messagePoolReturn()
			}
		}()
		if owner.receivePack.ingressLoanByteCount != cost || sequence.holdPromise.reservedByteCount != 0 || root.Available() != 0 {
			t.Fatal("ingress did not borrow the snapshotted prefix")
		}
		close(resume)
		<-done
		sequence.receiveQueue.cmp = cmp
		if sequence.memoryPressure.activeSet || !sequence.memoryPressure.pendingSet || sequence.memoryPressure.pending != request ||
			sequence.receiveQueue.Len() != 2 || sequence.client.receiveQueueTentativeEvictionCount.Load() != 0 || root.UsedByteCount() != 3*cost {
			t.Fatal("planning consumed a tail using prefix credit already owned by another ingress")
		}
		owner.receiveItem.messagePoolReturn()
		owner = nil
		sequence.applyReceiveMemoryPressure()
		if !sequence.memoryPressure.ready || sequence.memoryPressure.reservedByteCount != cost || sequence.holdPromise.reservedByteCount != cost || sequence.receiveQueue.Len() != 1 {
			t.Fatal("actual prefix-owner return did not make the complete replacement fundable")
		}
		sequence.releaseHoldPromise()
		for _, item := range sequence.receiveQueue.Clear() {
			item.messagePoolReturn()
		}
		if root.UsedByteCount() != 0 || sequence.holdPromise.ingressHead != nil {
			t.Fatal("prefix planning control leaked a real owner")
		}
		t.Log("[rx-prefix-pressure-plan] prefix_taken_after_snapshot=true no_partial_eviction=true return_then_replan=true root=0")
	})
}

// The same existing max-heap seam stops the second removal after the first
// real return. Neither ingress nor resettlement may spend the reserved prefix;
// cancellation revokes restoration only after the bounded batch completes.
func TestReceiveMemoryPrefixCreditSurvivesReclaimAndResettle(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, cancelBatch := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			const cost = receivePressureTestCost
			const smallCost = cost - 2*ByteCount(packetPoolSize) + 2*ByteCount(smallPacketPoolSize)
			const prefixRemainder = 3*cost - smallCost
			root, _, buffer, sequence := receivePrefixPressureTestState(t, 7)
			sequence.ctx, sequence.cancel = context.WithCancel(sequence.ctx)
			defer sequence.cancel()
			if !sequence.reserveHoldPromise(3 * cost) {
				t.Fatal("reclaim prefix setup")
			}
			sequence.settleHoldPromise(true, 4, 3*cost)
			for number := uint64(10); number < 14; number++ {
				owner := receivePressureInterleavingOwner(t, buffer, sequence, NewId(), number)
				sequence.receiveQueue.Add(&owner.receiveItem)
				sequence.adoptReceiveMemoryOwner(&owner.receiveItem)
			}
			smallOwner := buffer.takeReceiveMemoryOwner(sequence, NewId(), 3, smallCost)
			if smallOwner == nil {
				t.Fatal("smaller promised ingress could not take its own paid portion")
			}
			defer func() {
				if smallOwner != nil {
					smallOwner.receiveItem.messagePoolReturn()
				}
			}()
			smallOwner.receivePack.Pack = &smallOwner.pack
			smallOwner.frames[0] = protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider, Raw: true, MessageBytes: MessagePoolGet(128)}
			smallOwner.pack.Frames = append(smallOwner.pack.Frames, &smallOwner.frames[0])
			smallOwner.receiveItem.decodedOwner, smallOwner.receiveItem.ack = smallOwner, true
			smallOwner.receiveItem.frames, smallOwner.receiveItem.messageByteCount = smallOwner.pack.Frames, 128
			smallOwner.receiveItem.transferFrameBytes = MessagePoolGet(128)
			if smallOwner.receivePack.ingressLoanByteCount != smallCost ||
				MessagePoolRootByteCount(smallOwner.receiveItem.transferFrameBytes)+MessagePoolRootByteCount(smallOwner.frames[0].MessageBytes)+smallOwner.metadataQueueByteCount() != smallCost {
				t.Fatal("smaller promised ingress is not an actual small-class paid owner")
			}
			id := NewId()
			sequence.memoryPressure.pending = receiveMemoryPressureRequest{messageId: id, sequenceNumber: 1, queueByteCount: 2*cost + prefixRemainder, messageByteCount: 1280}
			sequence.memoryPressure.pendingSet = true
			entered, resume, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			cmp := sequence.receiveQueue.maxHeap.cmp
			stopped := false
			sequence.receiveQueue.maxHeap.cmp = func(a, b *receiveItem) int {
				root.admissionLock.Lock()
				afterFirst := sequence.memoryPressure.reservedByteCount == cost && sequence.memoryPressure.reclaimOwner == nil
				root.admissionLock.Unlock()
				if afterFirst && !stopped {
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
			if afterRoots != beforeRoots-2 || afterBytes != beforeBytes-4096 || root.UsedByteCount() != 7*cost || sequence.memoryPressure.ready {
				t.Fatal("prefix reclaim barrier did not follow exactly one paid physical return")
			}
			for _, arrival := range []struct {
				id     Id
				number uint64
			}{{id: id, number: 1}, {id: NewId(), number: 2}} {
				if owner := buffer.takeReceiveMemoryOwner(sequence, arrival.id, arrival.number, cost); owner != nil {
					owner.release()
					t.Fatal("ingress spent protected prefix during the actual reclaim batch")
				}
			}
			if cancelBatch {
				sequence.cancel()
			}
			close(resume)
			<-done
			sequence.receiveQueue.maxHeap.cmp = cmp
			if sequence.memoryPressure.reservedByteCount != 2*cost || sequence.holdPromise.reservedByteCount != prefixRemainder ||
				sequence.memoryPressure.reclaimOwner != nil || sequence.receiveQueue.Len() != 2 || root.UsedByteCount() != 7*cost {
				t.Fatal("mixed reclaim lost or double-counted its prefix and returned-tail portions")
			}
			if !cancelBatch {
				sequence.receiveQueue.Add(&smallOwner.receiveItem)
				sequence.adoptReceiveMemoryOwner(&smallOwner.receiveItem)
				smallOwner = nil
				sequence.settleHoldPromise(true, 4, 2*cost)
				if sequence.holdPromise.ingressLoanByteCount != 0 || sequence.holdPromise.reservedByteCount != prefixRemainder || root.UsedByteCount() != 7*cost {
					t.Fatal("smaller promised adoption released the exact head's protected remainder")
				}
			}
			sequence.releaseHoldPromise()
			for _, item := range sequence.receiveQueue.Clear() {
				item.messagePoolReturn()
			}
			if smallOwner != nil {
				smallOwner.receiveItem.messagePoolReturn()
				smallOwner = nil
			}
			if root.UsedByteCount() != 0 || sequence.memoryPressure != (receiveMemoryPressure{}) || sequence.holdPromise.ingressHead != nil {
				t.Fatal("mixed reclaim closure leaked physical roots or protected credit")
			}
			t.Logf("[rx-prefix-pressure-reclaim] cancel=%t actual_first_return=true protected_ingress_refused=true resettle_preserved=%t root=0", cancelBatch, !cancelBatch)
		})
	}
}

// A later exact retry must not fence an earlier predecessor's ordinary paid
// verification path. This core control deliberately uses proof facts that the
// narrow pressure authorizer cannot accept; it makes no proof-validation claim.
func TestReceiveMemoryLaterPressureLeavesEarlierProofDecodeFunded(t *testing.T) {
	assertMessagePoolOwnership(t)
	const cost = receivePressureTestCost
	root, _, buffer, sequence := receivePrefixPressureTestState(t, 4)
	if !sequence.reserveHoldPromise(2 * cost) {
		t.Fatal("earlier proof prefix setup")
	}
	sequence.settleHoldPromise(true, 4, 2*cost)
	for _, number := range []uint64{10, 11} {
		owner := receivePressureInterleavingOwner(t, buffer, sequence, NewId(), number)
		sequence.receiveQueue.Add(&owner.receiveItem)
		sequence.adoptReceiveMemoryOwner(&owner.receiveItem)
	}
	laterId, earlierId := NewId(), NewId()
	sequence.memoryPressure.pending = receiveMemoryPressureRequest{messageId: laterId, sequenceNumber: 2, queueByteCount: 2 * cost, messageByteCount: 1280}
	sequence.memoryPressure.pendingSet = true
	sequence.applyReceiveMemoryPressure()
	if !sequence.memoryPressure.ready || sequence.memoryPressure.reservedByteCount != 2*cost || sequence.holdPromise.reservedByteCount != 2*cost || sequence.receiveQueue.Len() != 0 {
		t.Fatal("later pressure borrowed an earlier predecessor's ordinary promise")
	}
	unknown := receiveMemoryPressureRequest{messageId: earlierId, sequenceNumber: 1, queueByteCount: cost, messageByteCount: 1280, contractFrameSet: true}
	if sequence.receiveMemoryPressureAuthorized(unknown) {
		t.Fatal("earlier proof control accidentally had old scalar authority")
	}
	owner := receivePressureInterleavingOwner(t, buffer, sequence, earlierId, 1)
	defer func() {
		if owner != nil {
			owner.receiveItem.messagePoolReturn()
		}
	}()
	if owner.receivePack.ingressLoanByteCount != cost || !owner.receiveItem.growReceiveMemory(cost) ||
		owner.receivePack.ingressLoanByteCount != 2*cost || sequence.memoryPressure.owner != nil || root.Available() != 0 {
		t.Fatal("later exact pressure prevented earlier ordinary funded decode and growth")
	}
	sequence.nextSequenceNumber = 2
	sequence.publishReceiveMemoryFloor()
	sequence.adoptReceiveMemoryOwner(&owner.receiveItem)
	owner.receiveItem.messagePoolReturn()
	owner = nil
	owner = receivePressureInterleavingOwner(t, buffer, sequence, laterId, 2)
	if !owner.receiveItem.growReceiveMemory(cost) {
		t.Fatal("later exact retry lost its disjoint funded pressure")
	}
	sequence.adoptReceiveMemoryOwner(&owner.receiveItem)
	owner.receiveItem.messagePoolReturn()
	owner = nil
	sequence.releaseHoldPromise()
	if root.UsedByteCount() != 0 || sequence.memoryPressure != (receiveMemoryPressure{}) || sequence.holdPromise.ingressHead != nil {
		t.Fatal("earlier proof/floor transition leaked exact credit")
	}
	t.Log("[rx-prefix-pressure-earlier] unknown_scalar_authority=true ordinary_prefix_available=true later_credit_disjoint=true floor_advance=true root=0")
}

// Successful ordinary growth adds a third portion to a pressure-backed owner.
// Its actual return restores only E and L; F becomes ordinary root capacity
// that a sibling may acquire before the next exact retry.
func TestReceiveMemoryPressureGrowthReturnsOnlyBorrowedCredit(t *testing.T) {
	assertMessagePoolOwnership(t)
	const cost = receivePressureTestCost
	root, rx, buffer, sequence := receivePrefixPressureTestState(t, 4)
	if !sequence.reserveHoldPromise(cost/2) || !rx.TryReserve(cost/2) {
		t.Fatal("three-portion credit setup")
	}
	sequence.settleHoldPromise(true, 3, cost/2)
	id := NewId()
	sequence.memoryPressure = receiveMemoryPressure{
		active:    receiveMemoryPressureRequest{messageId: id, sequenceNumber: 1, queueByteCount: cost, messageByteCount: 1280},
		activeSet: true, ready: true, reservedByteCount: cost / 2,
	}
	owner := receivePressureInterleavingOwner(t, buffer, sequence, id, 1)
	defer func() {
		if owner != nil {
			owner.receiveItem.messagePoolReturn()
		}
	}()
	if !owner.receiveItem.growReceiveMemory(cost/2) || owner.receiveItem.QueueByteCount() != 3*cost/2 ||
		sequence.memoryPressure.borrowedByteCount != cost/2 || owner.receivePack.ingressLoanByteCount != cost/2 || rx.UsedByteCount() != 3*cost/2 {
		t.Fatal("active retry did not acquire distinct escrow, prefix and fresh growth portions")
	}
	foreign := root.Available()
	if !root.TryReserve(foreign) {
		t.Fatal("three-portion competing claim")
	}
	defer func() {
		if foreign != 0 {
			root.Release(foreign)
		}
	}()
	if owner.receiveItem.growReceiveMemory(cost / 2) {
		t.Fatal("full-root fourth portion unexpectedly fit")
	}
	sequence.requestReceiveMemoryGrowth(&owner.receiveItem, cost/2)
	if !sequence.memoryPressure.pendingSet || sequence.memoryPressure.pending.queueByteCount != 2*cost ||
		sequence.memoryPressure.borrowedByteCount+owner.receivePack.ingressLoanByteCount+cost/2 != owner.receiveItem.QueueByteCount() {
		t.Fatal("larger pending request confused actual fresh growth with borrowed credit")
	}
	beforeRoots, beforeBytes := messagePoolOutstandingSnapshot()
	owner.receiveItem.messagePoolReturn()
	owner = nil
	afterRoots, afterBytes := messagePoolOutstandingSnapshot()
	if afterRoots != beforeRoots-2 || afterBytes != beforeBytes-4096 ||
		sequence.memoryPressure.owner != nil || sequence.memoryPressure.borrowedByteCount != 0 ||
		sequence.memoryPressure.reservedByteCount != cost/2 || sequence.holdPromise.reservedByteCount != cost/2 ||
		sequence.holdPromise.ingressLoanByteCount != 0 || !sequence.memoryPressure.ready ||
		rx.UsedByteCount() != cost || root.Available() != cost/2 || root.UsedByteCount() != rx.UsedByteCount()+foreign {
		t.Fatal("actual return retained fresh growth as escrow or released a borrowed portion")
	}
	sibling := NewTransferMemoryBudgetWithParent(cost/2, root)
	if !sibling.TryReserve(cost / 2) {
		t.Fatal("sibling could not acquire exactly the returned fresh portion")
	}
	siblingOwns := true
	defer func() {
		if siblingOwns {
			sibling.Release(cost / 2)
		}
	}()
	sequence.applyReceiveMemoryPressure()
	if sequence.memoryPressure.pendingSet || !sequence.memoryPressure.ready ||
		sequence.memoryPressure.active.queueByteCount != cost || root.Available() != 0 ||
		root.UsedByteCount() != rx.UsedByteCount()+sibling.UsedByteCount()+foreign {
		t.Fatal("unfundable larger request consumed the sibling or disabled the restored decode")
	}
	owner = receivePressureInterleavingOwner(t, buffer, sequence, id, 1)
	if owner.receiveItem.growReceiveMemory(cost) {
		t.Fatal("retry reused fresh growth already owned by the sibling")
	}
	owner.receiveItem.messagePoolReturn()
	owner = nil
	sibling.Release(cost / 2)
	siblingOwns = false
	root.Release(cost / 2)
	foreign -= cost / 2
	owner = receivePressureInterleavingOwner(t, buffer, sequence, id, 1)
	if !owner.receiveItem.growReceiveMemory(cost) || owner.receiveItem.QueueByteCount() != 2*cost ||
		sequence.memoryPressure.borrowedByteCount != cost/2 || owner.receivePack.ingressLoanByteCount != cost/2 || root.Available() != 0 {
		t.Fatal("exact retry failed to acquire the larger ordinary growth after its real release")
	}
	sequence.adoptReceiveMemoryOwner(&owner.receiveItem)
	owner.receiveItem.messagePoolReturn()
	owner = nil
	sequence.releaseHoldPromise()
	root.Release(foreign)
	foreign = 0
	if root.UsedByteCount() != 0 || rx.UsedByteCount() != 0 || sibling.UsedByteCount() != 0 ||
		sequence.memoryPressure != (receiveMemoryPressure{}) || sequence.holdPromise.ingressHead != nil {
		t.Fatal("three-portion return failed final retirement")
	}
	t.Log("[rx-prefix-pressure-fresh] claim_equals_escrow_plus_prefix_plus_fresh=true actual_return_releases_only_fresh=true sibling_owns_fresh=true larger_retry=true root=0")
}
