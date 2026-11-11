// Integration fixture accounting for the receiver's paid proof-metadata slot.
// These helpers require the receiver repair overlay; standalone provider r3
// controls remain frozen with their original ten-tail/prefix geometry.
package connect

import "testing"

// Derive all fixture leaf capacity before any tail is delivered. The shared
// root/provider-parent caps remain fixed; this is not a runtime rescue.
func privateProviderRepairFixtureGeometry(t *testing.T, sequence *ReceiveSequence, tailFull ByteCount, tailCount int) (ByteCount, ByteCount) {
	t.Helper()
	if sequence == nil || sequence.client == nil || sequence.client.ContractManager() == nil || tailFull <= 0 || tailCount <= 0 {
		t.Fatal("provider proof-metadata geometry requires an actual configured sequence")
	}
	noContract := sequence.client.ContractManager().ReceiveNoContract(sequence.source.SourceId)
	metadata := sequence.receiveBufferSettings.receiveHeldHeadRepairByteCount(noContract)
	if metadata <= 0 || sequence.heldHeadRepair.reservedByteCount != 0 || sequence.heldHeadRepair.eligibleCount != 0 || sequence.heldHeadRepair.owner != nil {
		t.Fatal("provider proof-metadata geometry must be captured before custody commitments")
	}
	target := metadata
	for range tailCount + 1 {
		target = addReceiveQueueByteCount(target, tailFull)
	}
	return metadata, target
}

// Call only after actual tail delivery and quiescence. The metadata reservation
// lives inside RX; every committed original contributes one exact eligibility.
func privateProviderAssertRepairFixtureCustody(t *testing.T, sequence *ReceiveSequence, metadata ByteCount, tailCount int) {
	t.Helper()
	root := sequence.receiveQueue.budget.admissionRoot()
	root.admissionLock.Lock()
	defer root.admissionLock.Unlock()
	state := &sequence.heldHeadRepair
	if state.reservedByteCount != metadata || state.eligibleCount != tailCount || state.owner != nil {
		t.Fatalf("provider tails did not fund the exact proof-metadata slot: metadata=%d reserved=%d eligible=%d owner=%p", metadata, state.reservedByteCount, state.eligibleCount, state.owner)
	}
	count := 0
	for pack := sequence.holdPromise.ingressHead; pack != nil; pack = pack.ingressNext {
		if pack.heldHeadRepairEligible {
			if pack.decodedOwner == nil || !pack.receiveMemoryOwned || pack.ingressSequence != sequence || pack.decodedOwner.receiveItem.memoryBudget != sequence.receiveQueue.budget {
				t.Fatal("provider proof metadata escaped the actual RX-backed committed originals")
			}
			count++
		}
	}
	if count != tailCount {
		t.Fatalf("provider proof metadata eligibility differs from live original custody: got=%d want=%d", count, tailCount)
	}
}
