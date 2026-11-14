// Fresh child proof copies keep tentative originals authoritative. Invalid
// proof, identity collisions and cancellation cannot create independent ACKs.
package connect

import (
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/v2026/protocol"
	"google.golang.org/protobuf/proto"
)

// Source prefix ACKs and a joined receiver retirement create the future head.
// Four live foreign owners supply pressure without undersizing the RX policy.
type consumerFutureAdjacentFixture struct {
	fixture         *windowRoundFixture
	log             *receiveGapTestLogger
	root, rx, child *TransferMemoryBudget
	sequence        *ReceiveSequence
	original        *receiveItem
	retry           *windowRoundFrame
	scan            receiveMemoryPackScan
}

// The original stays physically paid in RX; its sender-created promoted copy
// remains captured for independent decoder and public verifier transitions.
func newConsumerFutureAdjacentFixture(t *testing.T) *consumerFutureAdjacentFixture {
	t.Helper()
	assertMessagePoolOwnership(t)
	root := NewTransferMemoryBudget(2 * 22016)
	rx := NewTransferMemoryBudgetWithParent(22016, root)
	child := NewTransferMemoryBudgetWithParent(22016, root)
	t.Cleanup(func() {
		for _, budget := range []*TransferMemoryBudget{rx, child, root} {
			assertRetainedBudgetBalance(t, budget)
			if budget.Stats().QuotaByteCount != 0 {
				t.Error("future adjacent retained logical quota")
			}
		}
	})
	fixture, log := newConsumerTentativeHeadFixture(t, rx, child)
	repairBytes := fixture.receiver.settings.ReceiveBufferSettings.ReceiveHeadContractRepairByteCount
	if repairBytes != DefaultReceiveBufferSettings().ReceiveHeadContractRepairByteCount || repairBytes+3*1536 > rx.TotalByteCount() {
		t.Fatal("future adjacent leaf cannot fund ordinary default policy")
	}
	last := map[uint32]uint64{}
	foreignHead := writeConsumerFutureHeldData(t, fixture, 1, 1280, last)
	var foreignTails []*windowRoundFrame
	for range 4 {
		foreignTails = append(foreignTails, writeConsumerFutureHeldData(t, fixture, 1, 1280, last))
	}
	predecessor := writeConsumerFutureHeldData(t, fixture, 0, 32, last)
	tail := writeConsumerFutureHeldData(t, fixture, 0, 32, last)
	old, foreign := findConsumerAdjacentSequence(t, fixture, predecessor), findConsumerAdjacentSequence(t, fixture, foreignHead)
	fixture.forward(predecessor, fixture.receiverIn)
	fixture.acknowledge()
	if predecessor.pack.SequenceNumber != 1 || tail.pack.SequenceNumber != 2 || old.nextSequenceNumber != 2 ||
		tail.pack.Head || tail.pack.ContractFrame != nil || fixture.deliveredCount != 1 || fixture.ackedCount != 1 {
		t.Fatal("future adjacent actual prefix premise failed")
	}
	old.Cancel()
	<-old.done
	synctest.Wait()
	for index, frame := range foreignTails {
		fixture.forward(frame, fixture.receiverIn)
		item := foreign.receiveQueue.GetByMessageId(RequireIdFromBytes(frame.pack.MessageId))
		if item == nil || item.committed || item.QueueByteCount() != 5120 || item.memoryBudget != rx ||
			foreign.holdPromise.set || rx.UsedByteCount() != ByteCount(index+1)*5120 || len(fixture.receiverOut) != 0 {
			t.Fatal("future adjacent actual foreign custody premise failed")
		}
	}
	fixture.forward(tail, fixture.receiverIn)
	sequence := findConsumerAdjacentSequence(t, fixture, tail)
	original := sequence.receiveQueue.GetByMessageId(RequireIdFromBytes(tail.pack.MessageId))
	if sequence == old || original == nil || original.committed || original.head || !original.ack ||
		original.QueueByteCount() != 1536 || original.memoryBudget != rx || original.decodedOwner == nil ||
		!original.decodedOwner.receivePack.receiveConsumerData || original.decodedOwner.receivePack.heldHeadRepairEligible ||
		sequence.nextSequenceNumber != 0 || sequence.receiveContract != nil || sequence.holdPromise.set ||
		sequence.heldHeadRepair.eligibleCount != 0 || sequence.heldHeadRepair.reservedByteCount != 0 ||
		rx.UsedByteCount() != 22016 || root.UsedByteCount() != 22016 || child.UsedByteCount() != 0 || len(fixture.receiverOut) != 0 {
		t.Fatal("future adjacent paid tentative original premise failed")
	}
	retry := pressureContractNaturalRetry(t, fixture, tail.pack)
	if !retry.pack.Head || retry.pack.ContractFrame == nil || retry.pack.ContractId != nil || retry.pack.SequenceNumber != 2 ||
		len(retry.pack.Frames) != 1 || !proto.Equal(retry.pack.Frames[0], tail.pack.Frames[0]) {
		t.Fatal("future adjacent natural full proof premise failed")
	}
	path := TransferPath{SourceId: fixture.sender.ClientId(), DestinationId: fixture.receiver.ClientId()}
	scan, status := scanReceiveMemoryPack(retry.bytes, path)
	if status != receiveMemoryPackScanReady || !scan.loanSafe || scan.frameCount != 1 || scan.consumerFrameCount != 1 ||
		scan.messageId != original.messageId || scan.sequenceId != sequence.sequenceId || scan.retainedByteCount <= 1536 ||
		scan.retainedByteCount > child.TotalByteCount() || scan.retainedByteCount > root.Available() {
		t.Fatal("future adjacent physical retry envelope premise failed")
	}
	return &consumerFutureAdjacentFixture{fixture: fixture, log: log, root: root, rx: rx, child: child,
		sequence: sequence, original: original, retry: retry, scan: scan}
}

// Invalid proof reaches the unchanged verifier with a fully charged copied
// root. It cannot advance the replacement, ACK its original, or borrow RX.
func TestReceiveConsumerFutureAdjacentInvalidMacUsesOrdinaryVerifier(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		f := newConsumerFutureAdjacentFixture(t)
		pack := proto.Clone(f.retry.pack).(*protocol.Pack)
		var contract protocol.Contract
		if err := proto.Unmarshal(pack.ContractFrame.MessageBytes, &contract); err != nil || len(contract.StoredContractHmac) == 0 {
			t.Fatal("future adjacent actual proof has no MAC")
		}
		contract.StoredContractHmac[0] ^= 0x80
		var err error
		pack.ContractFrame.MessageBytes, err = proto.Marshal(&contract)
		if err != nil {
			t.Fatal(err)
		}
		rewriteHeldHeadContractWire(t, f.retry, pack)
		cost := decodedPackOwnerQueueByteCount + receiveMemoryScanCopyRoot(len(pack.ContractFrame.MessageBytes))
		var ackUpdates atomic.Int32
		f.sequence.ackWindow.afterUpdateForTest = func() { ackUpdates.Add(1) }
		observed, paid := false, false
		f.sequence.receiveBufferSettings.receiveOwnerStageForTest = func(actual *ReceiveSequence, owner *decodedPackOwner, stage string) {
			if actual == f.sequence && owner != nil && stage == "pack_released" && owner.receiveItem.messageId == f.original.messageId {
				observed = true
				paid = owner.receiveItem.memoryBudget == f.child && owner.receiveItem.QueueByteCount() == cost &&
					f.child.UsedByteCount() == cost && f.rx.UsedByteCount() == 22016 && f.root.UsedByteCount() == 22016+cost &&
					owner.receivePack.ingressLoanByteCount == 0 && len(owner.receivePack.TransferFrameBytes) == 0 &&
					len(owner.pack.Frames) == 0 && owner.pack.Tag == nil && !owner.receivePack.receiveConsumerData &&
					owner.receivePack.sequenceQueueByteCount == 0 && !owner.receivePack.sequenceQueueReserved &&
					f.sequence.heldHeadRepair.owner == owner && f.sequence.heldHeadRepair.eligibleCount == 0 &&
					f.sequence.heldHeadRepair.reservedByteCount == 0 && owner.receivePack.MessageByteCount == f.original.MessageByteCount()
			}
		}
		f.fixture.forward(f.retry, f.fixture.receiverIn)
		if !observed || !paid || f.sequence.ctx.Err() == nil || !f.sequence.rejectRetransmits ||
			f.sequence.nextSequenceNumber != 0 || f.sequence.receiveContract != nil || len(f.sequence.openReceiveContracts) != 0 ||
			ackUpdates.Load() != 0 || f.fixture.deliveredCount != 1 || f.fixture.ackedCount != 1 || len(f.log.messages()) == 0 ||
			f.child.UsedByteCount() != 0 || f.rx.UsedByteCount() != 20480 || f.root.UsedByteCount() != 20480 ||
			f.sequence.heldHeadRepair.owner != nil {
			t.Fatal("future child proof bypassed verification, ACKed or mis-refunded its original")
		}
	})
}

// An exact retained identity is required before copying its proof. An absent
// valid context may instead prepay a whole, unbound full-proof data head; it
// must leave the original and its actual retry/escrow authority untouched.
func TestReceiveConsumerFutureAdjacentIdentityAndRoleExclusions(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		f := newConsumerFutureAdjacentFixture(t)
		var original protocol.TransferFrame
		if err := proto.Unmarshal(f.retry.bytes, &original); err != nil {
			t.Fatal(err)
		}
		rows := []struct {
			name         string
			freshContext bool
			change       func(*protocol.TransferFrame)
		}{
			{name: "message", change: func(v *protocol.TransferFrame) { v.Pack.MessageId = NewId().Bytes() }},
			{name: "number", change: func(v *protocol.TransferFrame) { v.Pack.SequenceNumber++ }},
			{name: "generation", freshContext: true, change: func(v *protocol.TransferFrame) { v.Pack.SequenceId = NewId().Bytes() }},
			{name: "source", freshContext: true, change: func(v *protocol.TransferFrame) { v.TransferPath.SourceId = NewId().Bytes() }},
			{name: "destination", change: func(v *protocol.TransferFrame) { v.TransferPath.DestinationId = NewId().Bytes() }},
			{name: "lane", freshContext: true, change: func(v *protocol.TransferFrame) { v.Pack.LogicalLane = 1 }},
			{name: "stream", change: func(v *protocol.TransferFrame) { v.Pack.ForceStream = !v.Pack.ForceStream }},
			{name: "contract companion", change: func(v *protocol.TransferFrame) { v.Pack.CompanionContract = !v.Pack.CompanionContract }},
			{name: "session role", freshContext: true, change: func(v *protocol.TransferFrame) {
				role := protocol.SequenceRole_SequenceRoleServer
				v.SessionRole = &role
			}},
			{name: "unsupported role", change: func(v *protocol.TransferFrame) { role := protocol.SequenceRole(99); v.SessionRole = &role }},
			{name: "session companion", freshContext: true, change: func(v *protocol.TransferFrame) { value := true; v.SessionCompanion = &value }},
			{name: "non-head", change: func(v *protocol.TransferFrame) { v.Pack.Head = false }},
			{name: "no ack", change: func(v *protocol.TransferFrame) { v.Pack.Nack = true }},
			{name: "ahead", change: func(v *protocol.TransferFrame) { v.Pack.ContractAhead = true }},
			{name: "missing proof", change: func(v *protocol.TransferFrame) { v.Pack.ContractFrame = nil; v.Pack.ContractId = nil }},
			{name: "unknown compact proof", change: func(v *protocol.TransferFrame) { v.Pack.ContractFrame = nil; v.Pack.ContractId = NewId().Bytes() }},
			{name: "provider role", change: func(v *protocol.TransferFrame) {
				v.Pack.Frames[0].MessageType = protocol.MessageType_IpIpPacketToProvider
			}},
			{name: "wrong proof type", change: func(v *protocol.TransferFrame) {
				v.Pack.ContractFrame.MessageType = protocol.MessageType_TransferEncryptedControl
			}},
			{name: "encrypted control", change: func(v *protocol.TransferFrame) {
				v.Pack.Frames = append(v.Pack.Frames, &protocol.Frame{MessageType: protocol.MessageType_TransferEncryptedControl, MessageBytes: []byte{1}})
			}},
		}
		for _, row := range rows {
			frame := proto.Clone(&original).(*protocol.TransferFrame)
			row.change(frame)
			wire, err := ProtoMarshal(frame)
			if err != nil {
				t.Fatal(err)
			}
			func() {
				defer MessagePoolReturn(wire)
				if row.name == "source" {
					path := TransferPath{SourceId: f.fixture.sender.ClientId(), DestinationId: f.fixture.receiver.ClientId()}
					refuseConsumerAdjacentWireAtPath(t, f.fixture, f.root, f.child, wire, path, "source differs from original route")
				}
				if row.freshContext {
					requireConsumerAdjacentFreshPhysical(t, f.fixture, f.sequence, f.root, f.child, wire, row.name)
				} else {
					refuseConsumerAdjacentWire(t, f.fixture, f.root, f.child, wire, row.name)
				}
			}()
		}
		if f.sequence.receiveQueue.GetByMessageId(f.original.messageId) != f.original || f.original.committed ||
			f.sequence.nextSequenceNumber != 0 || f.sequence.receiveContract != nil || f.sequence.heldHeadRepair.owner != nil ||
			f.fixture.deliveredCount != 1 || f.fixture.ackedCount != 1 || len(f.fixture.receiverOut) != 0 {
			t.Fatal("future proof exclusions changed original custody or authority")
		}
	})
}

// A copied future proof can outlive its tentative original, but can never
// become application custody. Cancellation likewise retains only its live claim.
func TestReceiveConsumerFutureAdjacentDuplicateStaleAndCancel(t *testing.T) {
	GetMessagePoolAggregateStats()
	for _, cancelSequence := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			f := newConsumerFutureAdjacentFixture(t)
			pack := decodeConsumerAdjacentOwner(t, f.fixture, f.sequence, f.retry.bytes)
			defer func() {
				if pack != nil {
					pack.messagePoolReturn()
				}
			}()
			cost := pack.decodedOwner.receiveItem.QueueByteCount()
			if pack.decodedOwner.receiveItem.memoryBudget != f.child || f.sequence.heldHeadRepair.owner != pack.decodedOwner ||
				cost != decodedPackOwnerQueueByteCount+receiveMemoryScanCopyRoot(len(pack.Pack.ContractFrame.MessageBytes)) ||
				len(pack.Pack.Frames) != 0 || len(pack.TransferFrameBytes) != 0 || f.root.UsedByteCount() != 22016+cost {
				t.Fatal("future proof copy has no exclusive child claim")
			}
			refuseConsumerAdjacentWire(t, f.fixture, f.root, f.child, f.retry.bytes, "occupied future proof")
			var ackUpdates atomic.Int32
			f.sequence.ackWindow.afterUpdateForTest = func() { ackUpdates.Add(1) }
			if cancelSequence {
				f.sequence.Cancel()
				<-f.sequence.done
				synctest.Wait()
				if f.sequence.bindReceiveMemoryOwner(pack) {
					t.Fatal("closed future proof bound its old generation")
				}
				if owner := f.fixture.receiver.receiveBuffer.takeReceiveHeldHeadRepairOwner(f.sequence, f.scan, f.retry.bytes); owner != nil {
					owner.release()
					t.Fatal("closed generation acquired another future proof")
				}
			} else {
				if f.sequence.receiveQueue.RemoveByMessageId(f.original.messageId) != f.original {
					t.Fatal("stale original removal lost paid custody")
				}
				f.original.messagePoolReturn()
				accepted, err := f.sequence.receive(pack)
				if accepted || err != nil {
					t.Fatal("orphan child proof acquired payload authority")
				}
			}
			if f.rx.UsedByteCount() != 20480 || f.child.UsedByteCount() != cost || f.root.UsedByteCount() != 20480+cost ||
				f.sequence.heldHeadRepair.owner != pack.decodedOwner || f.sequence.heldHeadRepair.reservedByteCount != 0 ||
				f.sequence.heldHeadRepair.eligibleCount != 0 || f.sequence.nextSequenceNumber != 0 || f.sequence.receiveContract != nil ||
				ackUpdates.Load() != 0 || f.fixture.deliveredCount != 1 || f.fixture.ackedCount != 1 {
				t.Fatal("original return or closure surrendered live proof bytes or acquired ACK authority")
			}
			pack.messagePoolReturn()
			pack = nil
			if f.child.UsedByteCount() != 0 || f.root.UsedByteCount() != 20480 || f.sequence.heldHeadRepair.owner != nil ||
				f.sequence.holdPromise.ingressHead != nil || f.sequence.holdPromise.reservedByteCount != 0 {
				t.Fatal("last future proof return refunded RX or retained its child claim")
			}
		})
	}
}
