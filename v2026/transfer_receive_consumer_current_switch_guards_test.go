// Non-Head contract switches buy physical storage without Head authority,
// legacy compatibility, a shared-cap exception or weaker proof verification.
package connect

import "testing"

// Binding and adoption preserve the actual decoded proof/payload owner; they
// do not manufacture Head, advance ordering or install contract authority.
func TestReceiveConsumerCurrentSwitchBoundOwnerKeepsHeadAndAuthority(t *testing.T) {
	f := newEstablishedHeadGuardFixtureWithShape(t, "switch", 1)
	owner := f.take(f.scan, false)
	if owner == nil {
		t.Fatal("actual current non-Head proof had no independent owner")
	}
	wire := f.wire
	f.wire = nil
	f.decode(t, owner, wire)
	before := f.counts()
	if owner.pack.Head || owner.pack.ContractFrame == nil || owner.pack.ContractId != nil ||
		!f.sequence.bindReceiveMemoryOwner(&owner.receivePack) {
		t.Fatal("binding changed or rejected the actual non-Head proof shape")
	}
	f.sequence.adoptReceiveMemoryOwner(&owner.receiveItem)
	if owner.pack.Head || owner.receiveItem.memoryBudget != f.head || owner.receivePack.ingressSequence != f.sequence ||
		owner.receivePack.ingressLoanByteCount != 0 || f.sequence.holdPromise.ingressHead != &owner.receivePack ||
		f.sequence.holdPromise.ingressFloor != f.scan.sequenceNumber || f.sequence.nextSequenceNumber != f.scan.sequenceNumber ||
		f.sequence.holdPromise.set || f.sequence.receiveContract != nil || len(f.sequence.openReceiveContracts) != 0 ||
		f.counts() != before {
		t.Fatal("non-Head physical adoption changed ordering, authority, credit or exact ownership")
	}
}

// The legacy envelope has independently sufficient spare capacity. Refusal
// remains a wire-class decision, not an accidental child/root capacity cut.
func TestReceiveConsumerCurrentSwitchLegacyEnvelopeRemainsExcluded(t *testing.T) {
	f := newEstablishedHeadGuardFixtureWithShape(t, "switch", 2)
	frame := &windowRoundFrame{bytes: f.wire, pack: decodeCompactContractTestPack(t, f.wire)}
	rewriteHeldHeadContractWire(t, frame, frame.pack, true)
	f.wire = frame.bytes
	scan, status := scanReceiveMemoryPack(f.wire, f.path)
	if status != receiveMemoryPackScanReady || !scan.legacy || !scan.loanSafe || scan.head ||
		!scan.contractFrameSet || scan.contractIdSet || scan.consumerFrameCount != scan.frameCount ||
		scan.retainedByteCount <= 0 || scan.retainedByteCount > f.head.Available() ||
		scan.retainedByteCount > f.parent.Available() || scan.retainedByteCount > f.root.Available() {
		t.Fatal("legacy full-proof control did not retain its payable canonical DATA shape")
	}
	f.reject(t, scan, false)
}

// Invalid proof still reaches the ordinary live verifier after paying its
// full child owner; delivery, ACK, debit and foreign custody remain unchanged.
func TestReceiveConsumerCurrentSwitchPaidStorageDoesNotVerifyBadMac(t *testing.T) {
	if !testConsumerEstablishedProofAuthority(t, "bad-mac", true) && !t.Failed() {
		t.Fatal("non-Head MAC refusal did not exercise its independently paid worker")
	}
}

// A historical established bit cannot extend the contract's signed deadline.
func TestReceiveConsumerCurrentSwitchPaidStorageDoesNotExtendExpiry(t *testing.T) {
	if !testConsumerEstablishedProofAuthority(t, "expired", true) && !t.Failed() {
		t.Fatal("non-Head expiry refusal did not exercise its independently paid worker")
	}
}

// A valid MAC for another source is insufficient on this actual generation.
func TestReceiveConsumerCurrentSwitchPaidStorageDoesNotChangeSignedSource(t *testing.T) {
	if !testConsumerEstablishedProofAuthority(t, "source", true) && !t.Failed() {
		t.Fatal("non-Head signed-source refusal did not exercise its independently paid worker")
	}
}
