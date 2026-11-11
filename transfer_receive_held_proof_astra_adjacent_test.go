// Paid head repair retains only proof authority. Controlled decoder and worker
// boundaries cover source binding, stale originals and the final physical return.
package connect

import (
	"bytes"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// Borrows one test-owned wire while the real receive worker is quiescent. The
// returned owner has copied ids/proof and owns no carrier or application root.
func decodeHeldProofAdjacent(t *testing.T, fixture *windowRoundFixture, sequence *ReceiveSequence, wire []byte) *ReceivePack {
	t.Helper()
	path := TransferPath{SourceId: fixture.sender.ClientId(), DestinationId: fixture.receiver.ClientId()}
	decoded := &decodedTransferFrame{}
	ok, pressure := fixture.receiver.unmarshalReceiveMemoryFrame(wire, decoded, true, path, false, sequenceTlsRoleServer, false)
	if !ok || pressure {
		decoded.release()
		t.Fatalf("full-root exact held proof decode: ok=%t pressure=%t", ok, pressure)
	}
	owner := decoded.detachPackOwner()
	decoded.release()
	if owner == nil {
		t.Fatal("held proof decode returned no physical owner")
	}
	pack := &owner.receivePack
	pack.Source, pack.SequenceId = sequence.source, sequence.sequenceId
	pack.ReceiveCallback, pack.Unwrapped = fixture.receiver.receiveCallback, true
	pack.sequenceQueueByteCount = ByteCount(len(wire))
	pack.TransportType = TransportTypeH1
	if !sequence.bindReceiveMemoryOwner(pack) {
		pack.messagePoolReturn()
		t.Fatal("head proof owner lost its exact generation")
	}
	return pack
}

// One escrow can fund only one live proof. A refused or canceled copy refunds
// its own bytes, and closure keeps both actual owners paid until real return.
func TestReceiveHeldProofAdjacentHierarchicalEscrowAndCancel(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		root := NewTransferMemoryBudget(kib(128))
		budget := NewTransferMemoryBudgetWithParent(kib(128), root)
		fixture, _ := newHeldHeadContractFixture(t, budget)
		sequence, held, promoted := stageHeldHeadContract(t, fixture)
		root.SetTotalByteCount(root.UsedByteCount())
		budget.SetTotalByteCount(budget.UsedByteCount())
		reserved, released := root.Counts()
		originalBytes, slot := held.queueByteCount, sequence.heldHeadRepair.reservedByteCount
		if slot <= 0 || sequence.heldHeadRepair.eligibleCount != 1 {
			t.Fatal("acknowledged original did not prepay its repair")
		}
		proof := decodeHeldProofAdjacent(t, fixture, sequence, promoted.bytes)
		defer func() {
			if proof != nil {
				proof.messagePoolReturn()
			}
		}()
		proofBytes := proof.decodedOwner.receiveItem.queueByteCount
		if proof.ingressLoanByteCount != 0 || proof.decodedOwner.receiveItem.memoryBudget != budget ||
			sequence.heldHeadRepair.owner != proof.decodedOwner ||
			sequence.heldHeadRepair.reservedByteCount+proofBytes != slot {
			t.Fatal("repair borrowed another credit or lost its exact paid slot")
		}
		decoded := &decodedTransferFrame{}
		path := TransferPath{SourceId: fixture.sender.ClientId(), DestinationId: fixture.receiver.ClientId()}
		ok, pressure := fixture.receiver.unmarshalReceiveMemoryFrame(promoted.bytes, decoded, true, path, false, sequenceTlsRoleServer, false)
		decoded.release()
		if ok || !pressure {
			t.Fatal("concurrent duplicate borrowed the occupied proof slot")
		}
		afterReserved, afterReleased := root.Counts()
		if reserved != afterReserved || released != afterReleased {
			t.Fatal("proof conversion or duplicate changed root reservation counters")
		}
		proof.messagePoolReturn()
		proof = nil
		if sequence.heldHeadRepair.owner != nil || sequence.heldHeadRepair.reservedByteCount != slot ||
			root.UsedByteCount() != originalBytes+slot {
			t.Fatal("unused copy failed to restore only its own escrow")
		}
		proof = decodeHeldProofAdjacent(t, fixture, sequence, promoted.bytes)
		sequence.releaseHoldPromise()
		if root.UsedByteCount() != originalBytes+proof.decodedOwner.receiveItem.queueByteCount ||
			sequence.heldHeadRepair.reservedByteCount != 0 {
			t.Fatal("closure released a live original or proof")
		}
		removed := sequence.receiveQueue.RemoveByMessageId(held.messageId)
		if removed != held {
			t.Fatal("controlled close lost its original queue owner")
		}
		removed.messagePoolReturn()
		if root.UsedByteCount() != proof.decodedOwner.receiveItem.queueByteCount || sequence.heldHeadRepair.eligibleCount != 0 {
			t.Fatal("original return refunded the still-live proof")
		}
		proof.messagePoolReturn()
		proof = nil
		assertRetainedBudgetBalance(t, budget)
		assertRetainedBudgetBalance(t, root)
	})
}

// A copied proof can outlive a tentative original. Its arrival has no payload
// custody of its own, so it cannot advance the head or issue acceptance.
func TestReceiveHeldProofAdjacentStaleOriginalCannotAdvance(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		budget := NewTransferMemoryBudget(kib(64))
		fixture, _ := newHeldHeadContractFixture(t, budget)
		sequence, held, promoted := stageHeldHeadContract(t, fixture)
		budget.SetTotalByteCount(budget.UsedByteCount())
		proof := decodeHeldProofAdjacent(t, fixture, sequence, promoted.bytes)
		defer proof.messagePoolReturn()
		removed := sequence.receiveQueue.RemoveByMessageId(held.messageId)
		if removed != held {
			t.Fatal("controlled eviction lost the original")
		}
		removed.messagePoolReturn()
		var updates atomic.Int32
		sequence.ackWindow.afterUpdateForTest = func() { updates.Add(1) }
		accepted, err := sequence.receive(proof)
		if accepted || err != nil || sequence.nextSequenceNumber != 0 || sequence.receiveContract != nil ||
			len(sequence.openReceiveContracts) != 0 || sequence.receiveQueue.Len() != 0 ||
			fixture.deliveredCount != 2 || updates.Load() != 0 {
			t.Fatal("proof without its original acquired contract, prefix, delivery or ack authority")
		}
	})
}

// The carrier is returned and deterministically reacquired before verification.
// Overwriting it cannot corrupt copied proof authority or replace original data.
func TestReceiveHeldProofAdjacentReturnedCarrierCannotChangeProof(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		budget := NewTransferMemoryBudget(kib(64))
		fixture, log := newHeldHeadContractFixture(t, budget)
		sequence, held, promoted := stageHeldHeadContract(t, fixture)
		budget.SetTotalByteCount(budget.UsedByteCount())
		// A network read owns an independent carrier, unlike setHead's share
		// of the sender retry root. Recreate that ownership before forwarding.
		wire := MessagePoolCopy(promoted.bytes)
		MessagePoolReturn(promoted.bytes)
		promoted.bytes = wire
		address, size := &wire[0], len(wire)
		proofBytes := append([]byte(nil), promoted.pack.ContractFrame.MessageBytes...)
		messageBytes := MessageByteCount(promoted.pack.Frames)
		fixture.receiver.receivePackHandoffMaxByteCount.Store(0)
		originalOwner, originalTag := held.decodedOwner, held.receiveTag()
		originalPayload := append([]byte(nil), held.frames[0].MessageBytes...)
		var reacquired [][]byte
		defer func() {
			for _, carrier := range reacquired {
				MessagePoolReturn(carrier)
			}
		}()
		found, observed, valid := false, false, true
		sequence.receiveBufferSettings.receiveOwnerStageForTest = func(current *ReceiveSequence, owner *decodedPackOwner, stage string) {
			if current != sequence || stage != "pack_released" || owner == nil {
				return
			}
			observed = true
			proof := &owner.receivePack
			if len(owner.pack.Frames) != 0 || owner.pack.Tag != nil || len(proof.TransferFrameBytes) != 0 ||
				proof.sequenceQueueByteCount != 0 || proof.sequenceQueueReserved ||
				fixture.receiver.receivePackHandoffMaxByteCount.Load() != uint64(size) || proof.MessageByteCount != messageBytes ||
				owner.pack.ContractFrame == nil ||
				owner.receiveItem.queueByteCount != decodedPackOwnerQueueByteCount+MessagePoolRootByteCount(owner.pack.ContractFrame.MessageBytes) {
				valid = false
				return
			}
			for range messagePoolShardCount {
				carrier := MessagePoolGet(size)
				reacquired = append(reacquired, carrier)
				if &carrier[0] == address {
					for i := range carrier {
						carrier[i] = 0xd3
					}
					found = true
					break
				}
			}
			valid = bytes.Equal(owner.pack.ContractFrame.MessageBytes, proofBytes)
		}
		fixture.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
			if held.decodedOwner != originalOwner || held.receiveTag() != originalTag || len(frames) != 1 ||
				!bytes.Equal(frames[0].MessageBytes, originalPayload) {
				valid = false
			}
		})
		fixture.forward(promoted, fixture.receiverIn)
		fixture.acknowledge()
		if !observed || !found || !valid || fixture.deliveredCount != 3 || fixture.ackedCount != 3 || len(log.messages()) != 0 {
			t.Fatal("carrier reuse changed proof/data authority or lost logical handoff accounting")
		}
		assertRetainedBudgetBalance(t, budget)
	})
}

// The real wire/worker path must still reject an invalid MAC after borrowing
// repair storage. Cancellation returns the original and the in-flight proof.
func TestReceiveHeldProofAdjacentWireInvalidSignature(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		root := NewTransferMemoryBudget(kib(128))
		budget := NewTransferMemoryBudgetWithParent(kib(128), root)
		fixture, log := newHeldHeadContractFixture(t, budget)
		sequence, _, promoted := stageHeldHeadContract(t, fixture)
		pack := proto.Clone(promoted.pack).(*protocol.Pack)
		var contract protocol.Contract
		if err := proto.Unmarshal(pack.ContractFrame.MessageBytes, &contract); err != nil || len(contract.StoredContractHmac) == 0 {
			t.Fatal("signed fixture proof has no MAC")
		}
		contract.StoredContractHmac[0] ^= 0x80
		var err error
		pack.ContractFrame.MessageBytes, err = proto.Marshal(&contract)
		if err != nil {
			t.Fatal(err)
		}
		rewriteHeldHeadContractWire(t, promoted, pack)
		root.SetTotalByteCount(root.UsedByteCount())
		budget.SetTotalByteCount(budget.UsedByteCount())
		var updates atomic.Int32
		sequence.ackWindow.afterUpdateForTest = func() { updates.Add(1) }
		fixture.forward(promoted, fixture.receiverIn)
		if sequence.ctx.Err() == nil || sequence.receiveContract != nil || len(sequence.openReceiveContracts) != 0 ||
			sequence.nextSequenceNumber != 0 || fixture.deliveredCount != 2 || fixture.ackedCount != 2 ||
			updates.Load() != 0 || len(log.messages()) == 0 {
			t.Fatal("full-root proof repair bypassed ordinary contract failure")
		}
		assertRetainedBudgetBalance(t, budget)
		assertRetainedBudgetBalance(t, root)
	})
}

// Insufficient proof escrow refuses the original before selective acceptance.
// Its sender-owned full head can then make ordinary progress using freed RX.
func TestReceiveHeldProofAdjacentUnfundedOriginalKeepsRetry(t *testing.T) {
	GetMessagePoolAggregateStats()
	for _, limit := range []ByteCount{0, decodedPackOwnerQueueByteCount - 1} {
		synctest.Test(t, func(t *testing.T) {
			assertMessagePoolOwnership(t)
			budget := NewTransferMemoryBudget(kib(64))
			fixture, log := newHeldHeadContractFixture(t, budget)
			fixture.forward(writeHeldHeadContractData(t, fixture), fixture.receiverIn)
			fixture.acknowledge()
			predecessor := writeHeldHeadContractData(t, fixture)
			tail := writeHeldHeadContractData(t, fixture)
			fixture.forward(predecessor, fixture.receiverIn)
			fixture.acknowledge()
			old := fixture.receiveSequence()
			old.Cancel()
			<-old.done
			synctest.Wait()
			fixture.receiver.settings.ReceiveBufferSettings.ReceiveHeadContractRepairByteCount = limit
			sender := fixture.sequence()
			wire, full, err := sender.setHead(sender.sendItems[0], true)
			if err != nil || !full {
				t.Fatal("sender could not retain a full-proof retry")
			}
			promoted := &windowRoundFrame{bytes: wire, pack: decodeCompactContractTestPack(t, wire)}
			fixture.heldFrames = append(fixture.heldFrames, promoted)
			fixture.forward(tail, fixture.receiverIn)
			sequence := fixture.receiveSequence()
			if sequence.receiveQueue.Len() != 0 || sequence.nextSequenceNumber != 0 || len(fixture.receiverOut) != 0 ||
				fixture.deliveredCount != 2 || fixture.ackedCount != 2 || len(sender.sendItems) != 1 ||
				sequence.heldHeadRepair.eligibleCount != 0 || sequence.heldHeadRepair.reservedByteCount != 0 {
				t.Fatal("unfunded original acquired an ack lease or lost its sender retry")
			}
			assertRetainedBudgetBalance(t, budget)
			fixture.forward(promoted, fixture.receiverIn)
			fixture.acknowledge()
			if fixture.deliveredCount != 3 || fixture.ackedCount != 3 || len(sender.sendItems) != 0 || len(log.messages()) != 0 {
				t.Fatal("refused original prevented its fresh full head from making progress")
			}
			assertRetainedBudgetBalance(t, budget)
		})
	}
}

// Legacy protobuf keeps an unpooled proof, while a later proof-only repair
// copies into a pooled root. Custody must fund that future class before ACK.
func TestReceiveHeldProofAdjacentLegacyCopyRootRefusesUndersizedEscrow(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		const proofRoot = ByteCount(256)
		const limit = decodedPackOwnerQueueByteCount + proofRoot - 1
		budget := NewTransferMemoryBudget(kib(64))
		var originalId Id
		armed := false
		unarmedZeroMatches := 0
		unarmedProtocolHead := false
		observed, fundedLegacy := false, false
		var boundary struct {
			packPresent, proofPresent, external, head, budgetMatches, memoryOwned bool
			queueBytes, proofBytes, proofBackingBytes, copyRootBytes              ByteCount
		}
		fixture, log := newHeldHeadContractFixture(t, budget, func(settings *ReceiveBufferSettings) {
			settings.ReceiveHeadContractRepairByteCount = limit
			settings.receiveOwnerStageForTest = func(_ *ReceiveSequence, owner *decodedPackOwner, stage string) {
				if owner == nil || stage != "pack_released" {
					return
				}
				pack := owner.receivePack.Pack
				if !armed {
					// Protocol decode leaves receiveItem empty until receive().
					// Its zero ID must not match the not-yet-selected DATA ID.
					if originalId == (Id{}) && owner.receiveItem.messageId == originalId {
						unarmedZeroMatches++
						if pack != nil && pack == &owner.pack && pack.Head && pack.ContractFrame != nil &&
							protocolOnlyPack(pack) && !owner.receivePack.receiveMemoryOwned &&
							owner.receiveItem.memoryBudget == nil && owner.receiveItem.QueueByteCount() == 0 {
							unarmedProtocolHead = true
						}
					}
					return
				}
				if observed || owner.receiveItem.messageId != originalId || pack == nil {
					return
				}
				wireId, err := IdFromBytes(pack.MessageId)
				if err != nil || wireId != originalId {
					return
				}
				observed = true
				boundary.packPresent = pack != nil
				boundary.budgetMatches = owner.receiveItem.memoryBudget == budget
				boundary.memoryOwned = owner.receivePack.receiveMemoryOwned
				boundary.queueBytes = owner.receiveItem.QueueByteCount()
				if pack == nil {
					return
				}
				boundary.external = pack != &owner.pack
				boundary.head = pack.Head
				boundary.proofPresent = pack.ContractFrame != nil
				if pack.ContractFrame == nil {
					return
				}
				proof := pack.ContractFrame.MessageBytes
				boundary.proofBytes = ByteCount(len(proof))
				boundary.proofBackingBytes = MessagePoolRootByteCount(proof)
				boundary.copyRootBytes = receiveMemoryScanCopyRoot(len(proof))
				fundedLegacy = pack != &owner.pack && !pack.Head && owner.receiveItem.memoryBudget == budget &&
					owner.receivePack.receiveMemoryOwned && owner.receiveItem.QueueByteCount() > decodedPackOwnerQueueByteCount &&
					0 < len(proof) && ByteCount(len(proof)) < proofRoot && MessagePoolRootByteCount(proof) == ByteCount(len(proof)) &&
					receiveMemoryScanCopyRoot(len(proof)) == proofRoot && decodedPackOwnerQueueByteCount+ByteCount(len(proof)) <= limit
			}
		})
		fixture.forward(writeHeldHeadContractData(t, fixture), fixture.receiverIn)
		fixture.acknowledge()
		predecessor := writeHeldHeadContractData(t, fixture)
		tail := writeHeldHeadContractData(t, fixture)
		if tail.pack.Head || tail.pack.ContractFrame != nil || tail.pack.ContractId != nil {
			t.Fatal("legacy proof boundary lost its original non-head premise")
		}
		fixture.forward(predecessor, fixture.receiverIn)
		fixture.acknowledge()
		old := fixture.receiveSequence()
		old.Cancel()
		<-old.done
		synctest.Wait()
		sender := fixture.sequence()
		if len(sender.sendItems) != 1 || sender.sendItems[0].sequenceNumber != tail.pack.SequenceNumber {
			t.Fatal("legacy proof boundary lost its sender-owned retry")
		}
		wire, full, err := sender.setHead(sender.sendItems[0], true)
		if err != nil || !full {
			t.Fatal("legacy proof boundary could not restore the sender's full proof")
		}
		promoted := &windowRoundFrame{bytes: wire, pack: decodeCompactContractTestPack(t, wire)}
		fixture.heldFrames = append(fixture.heldFrames, promoted)
		pack := proto.Clone(tail.pack).(*protocol.Pack)
		pack.ContractFrame = proto.Clone(promoted.pack.ContractFrame).(*protocol.Frame)
		rewriteHeldHeadContractWire(t, tail, pack, true)
		originalId = RequireIdFromBytes(pack.MessageId)
		path := TransferPath{SourceId: fixture.sender.ClientId(), DestinationId: fixture.receiver.ClientId()}
		scan, status := scanReceiveMemoryPack(tail.bytes, path)
		if originalId == (Id{}) || status != receiveMemoryPackScanReady || !scan.legacy || scan.head || scan.nack ||
			scan.messageId != originalId || scan.protocolFrameCount == scan.frameCount || protocolOnlyPack(pack) {
			t.Fatal("legacy proof boundary lost its exact paid non-head DATA wire premise")
		}
		if unarmedZeroMatches == 0 || !unarmedProtocolHead || observed {
			t.Fatalf("opening protocol control no longer demonstrates the unarmed zero-ID collision: matches=%d protocol_head=%t observed=%t", unarmedZeroMatches, unarmedProtocolHead, observed)
		}
		// All earlier forwards are quiescent. Select and arm the actual wire
		// identity immediately before that single legacy original is published.
		armed = true
		fixture.forward(tail, fixture.receiverIn)
		sequence := fixture.receiveSequence()
		if !observed || !fundedLegacy {
			t.Fatalf("legacy proof boundary did not reach the real funded external decoder graph: observed=%t pack=%t proof=%t external=%t head=%t budget=%t owned=%t queue_bytes=%d metadata_bytes=%d proof_bytes=%d proof_backing_bytes=%d copy_root_bytes=%d expected_root_bytes=%d escrow_limit_bytes=%d", observed, boundary.packPresent, boundary.proofPresent, boundary.external, boundary.head, boundary.budgetMatches, boundary.memoryOwned, boundary.queueBytes, decodedPackOwnerQueueByteCount, boundary.proofBytes, boundary.proofBackingBytes, boundary.copyRootBytes, proofRoot, limit)
		}
		if sequence == old || sequence.nextSequenceNumber != 0 || sequence.receiveContract != nil ||
			sequence.receiveQueue.Len() != 0 || len(sequence.ackWindow.selectiveAcks) != 0 || len(fixture.receiverOut) != 0 ||
			sequence.heldHeadRepair.eligibleCount != 0 || sequence.heldHeadRepair.reservedByteCount != 0 ||
			fixture.deliveredCount != 2 || fixture.ackedCount != 2 || len(sender.sendItems) != 1 {
			t.Fatal("LEGACY_PROOF_COPY_ROOT_UNFUNDED_ACK: legacy byte length allowed an undersized future proof reservation")
		}
		assertRetainedBudgetBalance(t, budget)
		fixture.forward(promoted, fixture.receiverIn)
		fixture.acknowledge()
		if fixture.deliveredCount != 3 || fixture.ackedCount != 3 || len(sender.sendItems) != 0 ||
			sequence.nextSequenceNumber != pack.SequenceNumber+1 || sequence.receiveQueue.Len() != 0 || len(log.messages()) != 0 {
			t.Fatal("legacy proof refusal prevented ordinary funded full-head progress")
		}
		assertRetainedBudgetBalance(t, budget)
	})
}

// Same-number collisions and foreign source/session/generation bodies cannot
// take a paid exact-original slot, even if they carry a syntactically valid proof.
func TestReceiveHeldProofAdjacentIdentityAndMalformedExclusions(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		budget := NewTransferMemoryBudget(kib(64))
		fixture, _ := newHeldHeadContractFixture(t, budget)
		sequence, held, promoted := stageHeldHeadContract(t, fixture)
		budget.SetTotalByteCount(budget.UsedByteCount())
		reserved, released := budget.Counts()
		var original protocol.TransferFrame
		if err := proto.Unmarshal(promoted.bytes, &original); err != nil {
			t.Fatal(err)
		}
		path := TransferPath{SourceId: fixture.sender.ClientId(), DestinationId: fixture.receiver.ClientId()}
		rows := []struct {
			name   string
			change func(*protocol.TransferFrame)
		}{
			{name: "message", change: func(frame *protocol.TransferFrame) { frame.Pack.MessageId = NewId().Bytes() }},
			{name: "number", change: func(frame *protocol.TransferFrame) { frame.Pack.SequenceNumber++ }},
			{name: "generation", change: func(frame *protocol.TransferFrame) { frame.Pack.SequenceId = NewId().Bytes() }},
			{name: "source", change: func(frame *protocol.TransferFrame) { frame.TransferPath.SourceId = NewId().Bytes() }},
			{name: "destination", change: func(frame *protocol.TransferFrame) { frame.TransferPath.DestinationId = NewId().Bytes() }},
			{name: "lane", change: func(frame *protocol.TransferFrame) { frame.Pack.LogicalLane++ }},
			{name: "stream", change: func(frame *protocol.TransferFrame) { frame.Pack.ForceStream = !frame.Pack.ForceStream }},
			{name: "contract companion", change: func(frame *protocol.TransferFrame) { frame.Pack.CompanionContract = !frame.Pack.CompanionContract }},
			{name: "session role", change: func(frame *protocol.TransferFrame) {
				role := protocol.SequenceRole_SequenceRoleServer
				frame.SessionRole = &role
			}},
			{name: "session companion", change: func(frame *protocol.TransferFrame) {
				companion := true
				frame.SessionCompanion = &companion
			}},
			{name: "non-head", change: func(frame *protocol.TransferFrame) { frame.Pack.Head = false }},
			{name: "unreliable", change: func(frame *protocol.TransferFrame) { frame.Pack.Nack = true }},
			{name: "ahead", change: func(frame *protocol.TransferFrame) { frame.Pack.ContractAhead = !frame.Pack.ContractAhead }},
			{name: "encrypted control", change: func(frame *protocol.TransferFrame) {
				frame.Pack.Frames = append(frame.Pack.Frames, &protocol.Frame{MessageType: protocol.MessageType_TransferEncryptedControl, MessageBytes: []byte{1}})
			}},
			{name: "oversize proof", change: func(frame *protocol.TransferFrame) {
				frame.Pack.ContractFrame.MessageBytes = bytes.Repeat([]byte{0x55}, int(sequence.receiveBufferSettings.ReceiveHeadContractRepairByteCount))
			}},
		}
		for _, row := range rows {
			frame := proto.Clone(&original).(*protocol.TransferFrame)
			row.change(frame)
			wire, err := ProtoMarshal(frame)
			if err != nil {
				t.Fatal(err)
			}
			expected := path
			if row.name == "source" {
				expected.SourceId = RequireIdFromBytes(frame.TransferPath.SourceId)
			}
			decoded := &decodedTransferFrame{}
			ok, _ := fixture.receiver.unmarshalReceiveMemoryFrame(wire, decoded, true, expected, false, sequenceTlsRoleServer, false)
			decoded.release()
			MessagePoolReturn(wire)
			afterReserved, afterReleased := budget.Counts()
			if ok || sequence.heldHeadRepair.owner != nil || reserved != afterReserved || released != afterReleased ||
				sequence.receiveQueue.GetByMessageId(held.messageId) != held || sequence.nextSequenceNumber != 0 {
				t.Fatalf("%s acquired or changed exact held custody", row.name)
			}
		}
		body, err := proto.Marshal(promoted.pack)
		if err != nil {
			t.Fatal(err)
		}
		// A Frame.message_type encoded as bytes is structurally malformed.
		body = append(body, 0x2a, 0x03, 0x0a, 0x01, 0x01)
		wire := MessagePoolCopy(receiveMemoryScanTestWire(path, body, false))
		decoded := &decodedTransferFrame{}
		ok, pressure := fixture.receiver.unmarshalReceiveMemoryFrame(wire, decoded, true, path, false, sequenceTlsRoleServer, false)
		decoded.release()
		MessagePoolReturn(wire)
		afterReserved, afterReleased := budget.Counts()
		if ok || pressure || sequence.heldHeadRepair.owner != nil || reserved != afterReserved || released != afterReleased {
			t.Fatal("malformed skipped application frame acquired proof storage")
		}
	})
}

// Valid proof rotation may repair the exact identity, but changed retry data
// and tags never replace the already-acknowledged original's delivery custody.
func TestReceiveHeldProofAdjacentRotatedProofKeepsOriginalBody(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		budget := NewTransferMemoryBudget(kib(64))
		fixture, log := newHeldHeadContractFixture(t, budget)
		sequence, held, promoted := stageHeldHeadContract(t, fixture)
		originalOwner, originalTag := held.decodedOwner, held.receiveTag()
		originalPayload := append([]byte(nil), held.frames[0].MessageBytes...)
		pack := proto.Clone(promoted.pack).(*protocol.Pack)
		pack.ContractFrame = newHeldHeadContractProof(t, fixture.receiver, pack.ContractFrame)
		pack.Frames[0].MessageBytes = []byte("synthetic replacement payload")
		pack.Tag = &protocol.Tag{SendTime: 7}
		rewriteHeldHeadContractWire(t, promoted, pack)
		budget.SetTotalByteCount(budget.UsedByteCount())
		observed := false
		fixture.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
			observed = true
			if held.decodedOwner != originalOwner || held.receiveTag() != originalTag || len(frames) != 1 ||
				!bytes.Equal(frames[0].MessageBytes, originalPayload) || held.contractId == nil ||
				sequence.receiveContract == nil || *held.contractId != sequence.receiveContract.contractId {
				t.Error("rotated proof replaced original application/tag authority")
			}
		})
		fixture.forward(promoted, fixture.receiverIn)
		fixture.acknowledge()
		if !observed || fixture.deliveredCount != 3 || fixture.ackedCount != 3 || len(log.messages()) != 0 {
			t.Fatal("same-identity valid proof rotation failed to deliver the original once")
		}
		assertRetainedBudgetBalance(t, budget)
	})
}

// Explicit no-contract peers need the same paid metadata-only head promotion
// when a replacement receiver has acknowledged a later original at full RX.
func TestReceiveHeldProofAdjacentNoContractAtFullRoot(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		root := NewTransferMemoryBudget(kib(64))
		budget := NewTransferMemoryBudgetWithParent(kib(64), root)
		fixture := newWindowRoundFixture(t, nil, func(settings *ReceiveBufferSettings) {
			settings.ReceiveQueueBudget = budget
			settings.ReceiveQueueRetainedByteAccounting = true
			settings.ReceiveHoldPolicy = ReceiveHoldRefuse
		})
		fixture.forward(fixture.write(100), fixture.receiverIn)
		fixture.acknowledge()
		predecessor, tail := fixture.write(100), fixture.write(100)
		fixture.forward(predecessor, fixture.receiverIn)
		fixture.acknowledge()
		old := fixture.receiveSequence()
		old.Cancel()
		<-old.done
		synctest.Wait()
		sender := fixture.sequence()
		wire, full, err := sender.setHead(sender.sendItems[0], true)
		if err != nil || full {
			t.Fatal("no-contract sender could not form its metadata-free promoted head")
		}
		promoted := &windowRoundFrame{bytes: wire, pack: decodeCompactContractTestPack(t, wire)}
		fixture.heldFrames = append(fixture.heldFrames, promoted)
		fixture.forward(tail, fixture.receiverIn)
		sequence := fixture.receiveSequence()
		if sequence.receiveQueue.Len() != 1 || sequence.heldHeadRepair.reservedByteCount != decodedPackOwnerQueueByteCount {
			t.Fatal("no-contract held original did not reserve one metadata owner")
		}
		for len(fixture.receiverOut) != 0 {
			fixture.take(fixture.receiverOut)
		}
		root.SetTotalByteCount(root.UsedByteCount())
		budget.SetTotalByteCount(budget.UsedByteCount())
		fixture.forward(promoted, fixture.receiverIn)
		fixture.acknowledge()
		if fixture.deliveredCount != 3 || fixture.ackedCount != 3 || sequence.receiveContract != nil || len(sender.sendItems) != 0 {
			t.Fatal("full-root metadata-free head could not recover its held owner")
		}
		assertRetainedBudgetBalance(t, budget)
		assertRetainedBudgetBalance(t, root)
	})
}

// Authenticated unwrap exposes the same funded proof path for both protocol
// encodings. Its transient scratch and the legacy original graph stay separate.
func TestReceiveHeldProofAdjacentEncryptedLegacyOwner(t *testing.T) {
	GetMessagePoolAggregateStats()
	for _, legacy := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			assertMessagePoolOwnership(t)
			root := NewTransferMemoryBudget(kib(128))
			budget := NewTransferMemoryBudgetWithParent(kib(128), root)
			t.Cleanup(func() {
				assertRetainedBudgetBalance(t, budget)
				assertRetainedBudgetBalance(t, root)
			})
			fixture, _ := newHeldHeadContractFixture(t, budget)
			sequence, held, promoted := stageHeldHeadContract(t, fixture, true, legacy)
			originalOwner := held.decodedOwner
			if originalOwner == nil || (originalOwner.receivePack.Pack != &originalOwner.pack) != legacy {
				t.Fatal("original did not use the selected funded decoder graph")
			}
			rewriteHeldHeadContractWire(t, promoted, proto.Clone(promoted.pack).(*protocol.Pack), legacy)
			settings := *fixture.receiver.settings
			encryption := *settings.EncryptionSettings
			encryption.Mode = EncryptionModeRequired
			settings.EncryptionSettings = &encryption
			client := &Client{settings: &settings, receiveBuffer: fixture.receiver.receiveBuffer, log: NewNoopLogger()}
			path := TransferPath{SourceId: fixture.sender.ClientId(), DestinationId: fixture.receiver.ClientId()}
			cipher := newFrameCodecTestSequenceCipher(t)
			client.encryptionSessionManager = &EncryptionSessionManager{settings: settings.EncryptionSettings,
				sessions: map[sessionKey]*peerEncryptionSession{{peerId: path.SourceId, role: sequenceTlsRoleServer}: {
					role: sequenceTlsRoleServer, establishedEpoch: &tlsHandshakeEpoch{derivedTlsCipher: cipher},
				}}}
			defer client.releaseReceiveDecodeMemory()
			if !client.prepareReceiveDecodeMemory(nil, 16*1024) {
				t.Fatal("independent authenticated decode scratch was not funded")
			}
			scratch := client.receiveDecodeReservedByteCount.Load()
			root.SetTotalByteCount(root.UsedByteCount())
			budget.SetTotalByteCount(budget.UsedByteCount())
			reserved, released := root.Counts()
			outer, err := cipher.SealOuterFrame(path, promoted.bytes, protocol.SequenceRole_SequenceRoleClient, false)
			if err != nil {
				t.Fatal(err)
			}
			defer MessagePoolReturn(outer)
			outerDecoded := &decodedTransferFrame{}
			defer outerDecoded.release()
			if ok, pressure := client.unmarshalReceiveMemoryFrame(outer, outerDecoded, true, path, false, sequenceTlsRoleServer, false); !ok || pressure ||
				len(outerDecoded.frame.EncryptedTransferFrame) == 0 || sequence.heldHeadRepair.owner != nil {
				t.Fatal("opaque encrypted body acquired a proof owner")
			}
			companion := false
			inner, role, _, err := client.unwrapFrame(path.SourceId, protocol.SequenceRole_SequenceRoleClient, &companion, outerDecoded.frame.EncryptedTransferFrame)
			if err != nil || role != sequenceTlsRoleServer || !bytes.Equal(inner, promoted.bytes) {
				t.Fatal("authenticated synthetic unwrap failed")
			}
			defer func() { MessagePoolReturn(inner) }()
			decoded := &decodedTransferFrame{}
			if ok, pressure := client.unmarshalReceiveMemoryFrame(inner, decoded, true, path, true, sequenceTlsRoleClient, false); ok || !pressure {
				decoded.release()
				t.Fatal("another decrypting role borrowed the original's proof slot")
			}
			decoded.release()
			if ok, pressure := client.unmarshalReceiveMemoryFrame(inner, decoded, true, path, true, role, false); !ok || pressure {
				decoded.release()
				t.Fatal("authenticated exact legacy/v2 head could not borrow paid proof storage")
			}
			owner := decoded.detachPackOwner()
			decoded.release()
			if owner == nil {
				t.Fatal("authenticated proof has no physical owner")
			}
			defer owner.receivePack.messagePoolReturn()
			MessagePoolReturn(inner)
			inner = nil
			afterReserved, afterReleased := root.Counts()
			if owner.receivePack.Pack != &owner.pack || len(owner.pack.Frames) != 0 ||
				!bytes.Equal(owner.pack.ContractFrame.MessageBytes, promoted.pack.ContractFrame.MessageBytes) ||
				owner.receiveItem.memoryBudget != budget || owner.receivePack.ingressLoanByteCount != 0 ||
				reserved != afterReserved || released != afterReleased ||
				root.UsedByteCount() != budget.UsedByteCount()+scratch || held.decodedOwner != originalOwner {
				t.Fatal("authenticated repair mixed scratch, proof custody or legacy original backing")
			}
		})
	}
}

// A fast whole-hold attempt may meet an unsupported proof after committing an
// earlier original. It must publish exactly that prefix and leave later items
// tentative, without reserving repair eligibility for an unacknowledged tail.
func TestReceiveHeldProofAdjacentPartialCommitSettlesActualBoundary(t *testing.T) {
	assertMessagePoolOwnership(t)
	root, budget, buffer, sequence := receiveMemoryTestState(t, kib(64))
	sequence.client = &Client{settings: DefaultClientSettings()}
	sequence.ackWindow = newSequenceAckWindow()
	sequence.receiveBufferSettings.ReceiveHeadContractRepairByteCount = decodedPackOwnerQueueByteCount + 256
	sequence.receiveBufferSettings.ReceiveHoldPolicy = ReceiveHoldCommittedPrefix
	path := TransferPath{SourceId: NewId(), DestinationId: NewId()}
	sequenceId := NewId()
	var originals []*receiveItem
	t.Cleanup(func() {
		for _, item := range originals {
			if sequence.receiveQueue.RemoveByMessageId(item.messageId) != item {
				t.Error("controlled commitment lost a queue owner")
			}
			item.messagePoolReturn()
		}
		sequence.releaseHoldPromise()
		assertRetainedBudgetBalance(t, budget)
		assertRetainedBudgetBalance(t, root)
	})
	for _, number := range []uint64{2, 3, 4} {
		pack := &protocol.Pack{MessageId: NewId().Bytes(), SequenceId: sequenceId.Bytes(), SequenceNumber: number,
			Frames: []*protocol.Frame{{MessageType: protocol.MessageType_TestSimpleMessage, MessageBytes: []byte("synthetic")}}}
		if number == 3 {
			proof, err := proto.Marshal(&protocol.Contract{StoredContractBytes: bytes.Repeat([]byte{0x07}, 300), StoredContractHmac: make([]byte, 32)})
			if err != nil {
				t.Fatal(err)
			}
			pack.ContractFrame = &protocol.Frame{MessageType: protocol.MessageType_TransferContract, MessageBytes: proof}
		}
		body, err := proto.Marshal(pack)
		if err != nil {
			t.Fatal(err)
		}
		wire := MessagePoolCopy(receiveMemoryScanTestWire(path, body, false))
		scan, status := scanReceiveMemoryPack(wire, path)
		if status != receiveMemoryPackScanReady {
			MessagePoolReturn(wire)
			t.Fatal("controlled original failed structural preflight")
		}
		owner := buffer.takeReceiveMemoryOwner(sequence, scan.messageId, number, scan.retainedByteCount)
		if owner == nil {
			MessagePoolReturn(wire)
			t.Fatal("controlled original has no physical backing")
		}
		if _, ok := decodePackOwned(body, owner); !ok {
			owner.release()
			MessagePoolReturn(wire)
			t.Fatal("controlled original did not decode")
		}
		owner.receivePack.Pack = &owner.pack
		item := &owner.receiveItem
		item.decodedOwner, item.transferFrameBytes = owner, wire
		item.frames, item.contractFrame, item.ack = owner.pack.Frames, owner.pack.ContractFrame, true
		item.messageByteCount = MessageByteCount(item.frames)
		sequence.receiveQueue.Add(item)
		sequence.maxHeldByteCount = max(sequence.maxHeldByteCount, item.messageByteCount)
		sequence.maxHeldQueueByteCount = max(sequence.maxHeldQueueByteCount, item.queueByteCount)
		originals = append(originals, item)
	}
	sequence.commitHeldPrefix()
	if !originals[0].committed || originals[1].committed || originals[2].committed ||
		!sequence.holdPromise.set || sequence.holdPromise.boundary != originals[0].sequenceNumber ||
		sequence.holdPromise.reservedByteCount != sequence.maxHeldQueueByteCount ||
		sequence.heldHeadRepair.eligibleCount != 1 || !originals[0].decodedOwner.receivePack.heldHeadRepairEligible ||
		originals[1].decodedOwner.receivePack.heldHeadRepairEligible || originals[2].decodedOwner.receivePack.heldHeadRepairEligible ||
		len(sequence.ackWindow.selectiveAcks) != 1 {
		t.Fatal("partial fast commitment lost its actual promise boundary or leased an unsupported later original")
	}
}

// A full logical handoff can refuse a physically funded proof. The refusal
// counts the offered application bytes and refunds only its slot; resuming or
// canceling the actual worker releases every queued reservation exactly once.
func TestReceiveHeldProofAdjacentHandoffRefusalAndCancellation(t *testing.T) {
	GetMessagePoolAggregateStats()
	for _, cancelSequence := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			assertMessagePoolOwnership(t)
			budget := NewTransferMemoryBudget(kib(128))
			fixture, log := newHeldHeadContractFixture(t, budget)
			settings := fixture.receiver.settings.ReceiveBufferSettings
			settings.SequenceBufferSize, settings.H1SequenceBufferSize = 1, 1
			settings.ReliablePackHandoffTimeout, settings.H1PackHandoffTimeout = 0, 0
			sequence, held, promoted := stageHeldHeadContract(t, fixture)
			entered, resume := make(chan struct{}, 1), make(chan struct{})
			resumed := false
			defer func() {
				if !resumed {
					close(resume)
				}
			}()
			var blocked atomic.Bool
			refusedObserved := false
			settings.receiveOwnerStageForTest = func(current *ReceiveSequence, owner *decodedPackOwner, stage string) {
				if owner == nil {
					return
				}
				if stage == "handoff_refused" && owner.pack.Head {
					refusedObserved = owner.receivePack.TransferFrameBytes == nil &&
						owner.receivePack.sequenceQueueByteCount == 0 && !owner.receivePack.sequenceQueueReserved &&
						sequence.heldHeadRepair.owner == owner
				}
				if current == sequence && stage == "pack_released" && !owner.pack.Head && blocked.CompareAndSwap(false, true) {
					entered <- struct{}{}
					<-resume
				}
			}
			for i := range 2 {
				copy := &windowRoundFrame{bytes: MessagePoolCopy(promoted.bytes)}
				fixture.heldFrames = append(fixture.heldFrames, copy)
				rewriteHeldHeadContractWire(t, copy, proto.Clone(held.decodedOwner.receivePack.Pack).(*protocol.Pack))
				fixture.forward(copy, fixture.receiverIn)
				if i == 0 {
					select {
					case <-entered:
					default:
						t.Fatal("actual receive worker did not enter the explicit barrier")
					}
				}
			}
			if sequence.packQueueCount.Load() != 1 || sequence.packQueueByteCount.Load() <= 0 {
				t.Fatal("second paid duplicate did not fill the real one-slot handoff")
			}
			budget.SetTotalByteCount(budget.UsedByteCount())
			beforeReserved, beforeReleased := budget.Counts()
			slot := sequence.heldHeadRepair.reservedByteCount
			drops, droppedBytes := fixture.receiver.receivePackHandoffDropCount.Load(), fixture.receiver.receivePackHandoffDropByteCount.Load()
			messageBytes := MessageByteCount(promoted.pack.Frames)
			retry := &windowRoundFrame{bytes: MessagePoolCopy(promoted.bytes), pack: promoted.pack}
			fixture.heldFrames = append(fixture.heldFrames, retry)
			fixture.forward(promoted, fixture.receiverIn)
			afterReserved, afterReleased := budget.Counts()
			if !refusedObserved || sequence.heldHeadRepair.owner != nil || sequence.heldHeadRepair.reservedByteCount != slot ||
				beforeReserved != afterReserved || beforeReleased != afterReleased || sequence.nextSequenceNumber != 0 ||
				sequence.receiveQueue.GetByMessageId(held.messageId) != held || sequence.packQueueCount.Load() != 1 ||
				fixture.receiver.receivePackHandoffDropCount.Load() != drops+1 ||
				fixture.receiver.receivePackHandoffDropByteCount.Load() != droppedBytes+uint64(messageBytes) ||
				fixture.deliveredCount != 2 || fixture.ackedCount != 2 || sequence.ctx.Err() != nil {
				t.Fatal("logical handoff refusal changed original custody or lost the exact proof/drop accounting")
			}
			if cancelSequence {
				sequence.Cancel()
			}
			close(resume)
			resumed = true
			synctest.Wait()
			if sequence.packQueueCount.Load() != 0 || sequence.packQueueByteCount.Load() != 0 {
				t.Fatal("worker drain left a logical handoff reservation")
			}
			if !cancelSequence {
				fixture.forward(retry, fixture.receiverIn)
				fixture.acknowledge()
				if fixture.deliveredCount != 3 || fixture.ackedCount != 3 || len(log.messages()) != 0 {
					t.Fatal("retry after a refunded handoff refusal did not deliver exactly once")
				}
			} else if sequence.ctx.Err() == nil || fixture.deliveredCount != 2 || fixture.ackedCount != 2 {
				t.Fatal("canceled worker delivered or acknowledged its held original")
			}
			assertRetainedBudgetBalance(t, budget)
		})
	}
}
