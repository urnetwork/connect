// Allocator/session guards use real pooled decoder roots and bounded budgets.
// The established bit below is explicit worker-free metadata, not a claim of
// signed wire authority; the separate Client roots cover that publication.
package connect

import (
	"bytes"
	"context"
	"testing"
	"unsafe"

	"github.com/urnetwork/connect/protocol"
)

type establishedHeadGuardFixture struct {
	root, parent, rx, head, clientPressure, rootPressure *TransferMemoryBudget
	buffer                                               *ReceiveBuffer
	sequence                                             *ReceiveSequence
	path                                                 TransferPath
	scan                                                 receiveMemoryPackScan
	wire                                                 []byte
	owners                                               []*decodedPackOwner
	cancel                                               context.CancelFunc
}

func newEstablishedHeadGuardFixture(t *testing.T) *establishedHeadGuardFixture {
	t.Helper()
	assertMessagePoolOwnership(t)
	path := TransferPath{SourceId: NewId(), DestinationId: NewId()}
	var frames []*protocol.Frame
	for range 8 {
		packet := ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagAck, 7, bytes.Repeat([]byte{0x42}, 1240))
		frame, err := ipPacketFromProviderFrame(packet, 2)
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, frame)
	}
	pack := sendPackFrame{messageId: NewId(), sequenceId: NewId(), sequenceNumber: 9, frames: frames,
		logicalLane: 3, forceStream: true, companionContract: true}
	wire := MessagePoolCopy(receiveMemoryScanTestWire(path, pack.appendPack(nil), false))
	scan, status := scanReceiveMemoryPack(wire, path)
	if status != receiveMemoryPackScanReady || !scan.loanSafe || scan.head || scan.contractIdSet || scan.contractFrameSet ||
		scan.consumerFrameCount != 8 || scan.frameCount != 8 || scan.retainedByteCount <= 0 {
		MessagePoolReturn(wire)
		t.Fatal("established-head guard has no complete consumer allocation envelope")
	}
	cost := scan.retainedByteCount
	root := NewTransferMemoryBudget(4 * cost)
	parent := NewTransferMemoryBudgetWithParent(3*cost, root)
	rx := NewTransferMemoryBudgetWithParent(cost, parent)
	head := NewTransferMemoryBudgetWithParent(cost, parent)
	settings := DefaultReceiveBufferSettingsWithBufferSize(64)
	settings.ReceiveQueueBudget, settings.ReceiveHeadRecoveryBudget = rx, head
	settings.ReceiveQueueRetainedByteAccounting = true
	ctx, cancel := context.WithCancel(t.Context())
	queue := newReceiveQueue(rx, 0)
	queue.lifetimeBudget = true
	sequence := &ReceiveSequence{ctx: ctx, source: path.SourceMask(), sequenceId: scan.sequenceId,
		receiveQueue: queue, receiveBufferSettings: settings, nextSequenceNumber: scan.sequenceNumber,
		transferKey: TransferKey{ForceStream: scan.forceStream, CompanionContract: scan.companionContract, LogicalLane: scan.logicalLane}}
	sequence.holdPromise.ingressFloor = scan.sequenceNumber
	sequence.holdPromise.establishedContract = true
	f := &establishedHeadGuardFixture{root: root, parent: parent, rx: rx, head: head,
		clientPressure: NewTransferMemoryBudgetWithParent(2*cost, parent), rootPressure: NewTransferMemoryBudgetWithParent(3*cost, root),
		buffer: &ReceiveBuffer{receiveBufferSettings: settings}, sequence: sequence, path: path, scan: scan, wire: wire, cancel: cancel}
	t.Cleanup(func() {
		cancel()
		for _, owner := range f.owners {
			owner.receivePack.messagePoolReturn()
		}
		MessagePoolReturn(f.wire)
		f.wire = nil
		sequence.releaseHoldPromise()
		for _, budget := range []*TransferMemoryBudget{head, rx, f.clientPressure, f.rootPressure, parent, root} {
			assertRetainedBudgetBalance(t, budget)
			if budget.Stats().QuotaByteCount != 0 {
				t.Error("established-head guard retained logical quota")
			}
		}
	})
	f.allocate(t, rx)
	if rx.Available() != 0 || head.Available() != cost || parent.Available() < cost || root.Available() < cost ||
		sequence.holdPromise.ingressHead != nil || sequence.holdPromise.set || sequence.holdPromise.reservedByteCount != 0 {
		t.Fatal("established-head guard did not isolate foreign-only RX pressure")
	}
	return f
}

// Each pressure owner decodes actual carrier/payload roots. Its allocator
// identity is not registered as target-sequence backlog or contract authority.
func (self *establishedHeadGuardFixture) allocate(t *testing.T, budget *TransferMemoryBudget) {
	t.Helper()
	settings := DefaultReceiveBufferSettings()
	settings.ReceiveQueueBudget = budget
	allocator := &ReceiveBuffer{receiveBufferSettings: settings}
	owner := allocator.takeReceiveMemoryOwner(nil, self.scan.messageId, self.scan.sequenceNumber, self.scan.retainedByteCount)
	if owner == nil {
		t.Fatal("pressure owner could not fund actual pooled decoder roots")
	}
	self.owners = append(self.owners, owner)
	wire := MessagePoolCopy(self.wire)
	self.decode(t, owner, wire)
}

func (self *establishedHeadGuardFixture) decode(t *testing.T, owner *decodedPackOwner, wire []byte) {
	t.Helper()
	owner.receivePack.TransferFrameBytes = wire
	decoded, ok := decodePackOwned(wire[self.scan.packOffset:self.scan.packOffset+self.scan.packByteCount], owner)
	if !ok || decoded != owner {
		t.Fatal("guard owner could not decode its actual funded wire")
	}
	owner.receivePack.Pack = &owner.pack
	owner.receivePack.MessageByteCount = MessageByteCount(owner.pack.Frames)
	owner.receiveItem.decodedOwner = owner
	physical := MessagePoolRootByteCount(wire) + owner.metadataQueueByteCount()
	for _, frame := range owner.pack.Frames {
		physical += MessagePoolRootByteCount(frame.MessageBytes)
	}
	if physical != self.scan.retainedByteCount || owner.receiveItem.QueueByteCount() != physical {
		t.Fatal("guard physical root census differs from the whole paid decoder owner")
	}
}

func (self *establishedHeadGuardFixture) take(scan receiveMemoryPackScan, decrypted bool) *decodedPackOwner {
	owner := self.buffer.takeReceiveHeadRecoveryOwnerInSession(self.sequence, scan, decrypted)
	if owner != nil {
		self.owners = append(self.owners, owner)
	}
	return owner
}

type establishedHeadGuardCounts struct {
	used, reserved, released, quota ByteCount
}

func (self *establishedHeadGuardFixture) counts() [4]establishedHeadGuardCounts {
	var counts [4]establishedHeadGuardCounts
	for index, budget := range []*TransferMemoryBudget{self.root, self.parent, self.rx, self.head} {
		s := budget.Stats()
		counts[index] = establishedHeadGuardCounts{s.UsedByteCount, s.ReservedByteCount, s.ReleasedByteCount, s.QuotaByteCount}
	}
	return counts
}

func (self *establishedHeadGuardFixture) reject(t *testing.T, scan receiveMemoryPackScan, decrypted bool) {
	t.Helper()
	before := self.counts()
	if owner := self.take(scan, decrypted); owner != nil {
		t.Fatal("guarded request acquired a child owner")
	}
	if self.counts() != before || self.sequence.holdPromise.ingressHead != nil {
		t.Fatal("refused request changed physical credit or registered target custody")
	}
}

func TestReceiveConsumerEstablishedHeadPaysActualIndependentRoots(t *testing.T) {
	f := newEstablishedHeadGuardFixture(t)
	owner := f.take(f.scan, false)
	if owner == nil {
		t.Fatal("established current consumer did not fund its independent physical envelope")
	}
	wire := f.wire
	f.wire = nil
	f.decode(t, owner, wire)
	if owner.receiveItem.memoryBudget != f.head || owner.receivePack.ingressSequence != f.sequence || owner.receivePack.ingressLoanByteCount != 0 ||
		f.rx.UsedByteCount() != f.scan.retainedByteCount || f.head.UsedByteCount() != f.scan.retainedByteCount ||
		f.root.UsedByteCount() != 2*f.scan.retainedByteCount || f.parent.UsedByteCount() != f.root.UsedByteCount() ||
		f.sequence.holdPromise.set || f.sequence.holdPromise.reservedByteCount != 0 {
		t.Fatal("established head borrowed foreign credit or lost exact child attribution")
	}
}

func TestReceiveConsumerEstablishedHeadRejectsUnknownAndWireClasses(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*establishedHeadGuardFixture, *receiveMemoryPackScan)
	}{
		{"unknown-generation", func(f *establishedHeadGuardFixture, _ *receiveMemoryPackScan) {
			f.sequence.holdPromise.establishedContract = false
		}},
		{"unsafe-scan", func(_ *establishedHeadGuardFixture, s *receiveMemoryPackScan) { s.loanSafe = false }},
		{"compatibility", func(_ *establishedHeadGuardFixture, s *receiveMemoryPackScan) { s.compatibility = true }},
		{"nack", func(_ *establishedHeadGuardFixture, s *receiveMemoryPackScan) { s.nack = true }},
		{"ahead", func(_ *establishedHeadGuardFixture, s *receiveMemoryPackScan) { s.contractAhead = true }},
		{"encrypted-control", func(_ *establishedHeadGuardFixture, s *receiveMemoryPackScan) { s.hasEncryptedControl = true }},
		{"head", func(_ *establishedHeadGuardFixture, s *receiveMemoryPackScan) { s.head = true }},
		{"unknown-contract-id", func(_ *establishedHeadGuardFixture, s *receiveMemoryPackScan) {
			s.contractIdSet, s.contractId = true, NewId()
		}},
		{"proof", func(_ *establishedHeadGuardFixture, s *receiveMemoryPackScan) {
			s.contractFrameSet = true
			s.contractFrameType = int32(protocol.MessageType_TransferContract)
		}},
		{"wrong-proof-type", func(_ *establishedHeadGuardFixture, s *receiveMemoryPackScan) {
			s.contractFrameSet = true
			s.contractFrameType = int32(protocol.MessageType_TestSimpleMessage)
		}},
		{"invalid-role", func(_ *establishedHeadGuardFixture, s *receiveMemoryPackScan) {
			s.sessionRoleSet, s.sessionRole = true, 99
		}},
		{"empty", func(_ *establishedHeadGuardFixture, s *receiveMemoryPackScan) {
			s.frameCount, s.consumerFrameCount = 0, 0
		}},
		{"mixed", func(_ *establishedHeadGuardFixture, s *receiveMemoryPackScan) { s.consumerFrameCount-- }},
		{"provider", func(_ *establishedHeadGuardFixture, s *receiveMemoryPackScan) {
			s.consumerFrameCount = 0
			s.providerFrameCount = s.frameCount
		}},
		{"zero-cost", func(_ *establishedHeadGuardFixture, s *receiveMemoryPackScan) { s.retainedByteCount = 0 }},
		{"past", func(_ *establishedHeadGuardFixture, s *receiveMemoryPackScan) { s.sequenceNumber-- }},
		{"future", func(_ *establishedHeadGuardFixture, s *receiveMemoryPackScan) { s.sequenceNumber++ }},
		{"future-span65", func(_ *establishedHeadGuardFixture, s *receiveMemoryPackScan) { s.sequenceNumber += 65 }},
	} {
		func() {
			t.Logf("ESTABLISHED_HEAD_GUARD_CASE %s", test.name)
			f := newEstablishedHeadGuardFixture(t)
			scan := f.scan
			test.change(f, &scan)
			f.reject(t, scan, false)
		}()
	}
}

func TestReceiveConsumerEstablishedHeadKeepsRequiredPlaintextGate(t *testing.T) {
	f := newEstablishedHeadGuardFixture(t)
	f.sequence.session = &peerEncryptionSession{settings: &EncryptionSettings{Mode: EncryptionModeRequired}}
	f.reject(t, f.scan, false)
	before := f.counts()
	if owner := f.buffer.takeReceiveHeadRecoveryOwner(f.sequence, f.scan); owner != nil {
		f.owners = append(f.owners, owner)
		t.Fatal("two-argument wrapper invented a decrypted context")
	}
	if f.counts() != before {
		t.Fatal("Required plaintext wrapper changed paid credit")
	}
	owner := f.take(f.scan, true)
	if owner == nil {
		t.Fatal("actual decrypted context lost the established current physical permission")
	}
	wire := f.wire
	f.wire = nil
	f.decode(t, owner, wire)
}

func TestReceiveConsumerEstablishedHeadKeepsChildClientAndRootCaps(t *testing.T) {
	for _, scope := range []string{"child", "client", "root"} {
		func() {
			f := newEstablishedHeadGuardFixture(t)
			switch scope {
			case "child":
				f.allocate(t, f.head)
			case "client":
				f.allocate(t, f.clientPressure)
				f.allocate(t, f.clientPressure)
			case "root":
				for range 3 {
					f.allocate(t, f.rootPressure)
				}
			}
			f.reject(t, f.scan, false)
		}()
	}
}

func TestReceiveConsumerEstablishedHeadKeepsDuplicateWithSpareCredit(t *testing.T) {
	f := newEstablishedHeadGuardFixture(t)
	cost := f.scan.retainedByteCount
	f.rx.SetTotalByteCount(2 * cost)
	f.head.SetTotalByteCount(2 * cost)
	f.parent.SetTotalByteCount(4 * cost)
	f.allocate(t, f.rx)
	owner := f.take(f.scan, false)
	if owner == nil {
		t.Fatal("duplicate guard lacks a real current owner")
	}
	wire := f.wire
	f.wire = nil
	f.decode(t, owner, wire)
	if f.rx.Available() != 0 || f.head.Available() != cost || f.parent.Available() != cost || f.root.Available() != cost {
		t.Fatal("duplicate guard did not leave independent child and ancestor credit")
	}
	before := f.counts()
	for _, id := range []Id{f.scan.messageId, NewId()} {
		scan := f.scan
		scan.messageId = id
		if duplicate := f.take(scan, false); duplicate != nil {
			t.Fatal("same current position acquired a second independently funded owner")
		}
	}
	if f.counts() != before {
		t.Fatal("duplicate attempt changed actual physical credit")
	}
}

func TestReceiveConsumerEstablishedHeadKeepsCloneSharedCap(t *testing.T) {
	f := newEstablishedHeadGuardFixture(t)
	owner := f.take(f.scan, false)
	if owner == nil {
		t.Fatal("duplicate guard lacks a real current owner")
	}
	wire := f.wire
	f.wire = nil
	f.decode(t, owner, wire)
	before := f.counts()
	settings := *f.sequence.receiveBufferSettings
	queue := newReceiveQueue(f.rx, 0)
	queue.lifetimeBudget = true
	clone := &ReceiveSequence{ctx: f.sequence.ctx, receiveQueue: queue, receiveBufferSettings: &settings}
	clone.holdPromise.ingressFloor = f.scan.sequenceNumber
	clone.holdPromise.establishedContract = true
	buffer := &ReceiveBuffer{receiveBufferSettings: &settings}
	if other := buffer.takeReceiveHeadRecoveryOwnerInSession(clone, f.scan, false); other != nil {
		f.owners = append(f.owners, other)
		t.Fatal("cloned settings created fresh head capacity")
	}
	if settings.ReceiveHeadRecoveryBudget != f.head || settings.ReceiveQueueBudget != f.rx || f.counts() != before {
		t.Fatal("cloned settings changed shared physical ownership")
	}
}

func TestReceiveConsumerEstablishedHeadRejectsCanceledAndClosedGeneration(t *testing.T) {
	for _, closed := range []bool{false, true} {
		func() {
			f := newEstablishedHeadGuardFixture(t)
			if closed {
				f.sequence.holdPromise.closed = true
			} else {
				f.cancel()
			}
			f.reject(t, f.scan, false)
		}()
	}
}

func TestReceiveConsumerEstablishedPublicationIsVerifiedAndStickyMetadata(t *testing.T) {
	f := newEstablishedHeadGuardFixture(t)
	f.sequence.holdPromise.establishedContract = false
	f.sequence.publishReceiveMemoryFloor()
	if f.sequence.holdPromise.establishedContract {
		t.Fatal("missing contract published established-generation permission")
	}
	f.sequence.receiveContract = &sequenceContract{}
	f.sequence.publishReceiveMemoryFloor()
	if f.sequence.holdPromise.establishedContract {
		t.Fatal("unverified metadata published established-generation permission")
	}
	f.sequence.receiveContract.verifiedReceiveProofSet = true
	f.sequence.publishReceiveMemoryFloor()
	if !f.sequence.holdPromise.establishedContract {
		t.Fatal("worker-owned verified metadata did not publish its established generation")
	}
	f.sequence.receiveContract = nil
	f.sequence.publishReceiveMemoryFloor()
	if !f.sequence.holdPromise.establishedContract {
		t.Fatal("historical physical permission became a second current-contract validator")
	}
}

func TestReceiveConsumerEstablishedPermissionUsesExistingPromisePadding(t *testing.T) {
	type priorPromise struct {
		set                  bool
		boundary             uint64
		reservedByteCount    ByteCount
		ingressHead          *ReceivePack
		ingressLoanByteCount ByteCount
		ingressFloor         uint64
		closed               bool
	}
	if unsafe.Sizeof(receiveHoldPromise{}) != unsafe.Sizeof(priorPromise{}) {
		t.Fatal("established-generation permission grew per-sequence standing storage")
	}
}
