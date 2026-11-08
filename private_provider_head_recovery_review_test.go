// Deterministic provider recovery transactions complement the encoded route
// regressions. Real copied carrier and TCP roots retain one decoded identity;
// synthetic sibling claims isolate exact parent, root and child boundaries.
package connect

import (
	"context"
	"reflect"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

// No worker runs in this fixture. The same immutable provider parent contains
// ordinary RX and recovery, while another root child models an independent role.
type privateProviderHeadRecoveryReviewFixture struct {
	t                          *testing.T
	root, parent, rx, recovery *TransferMemoryBudget
	sibling                    *TransferMemoryBudget
	buffer                     *ReceiveBuffer
	sequence                   *ReceiveSequence
	cancel                     context.CancelFunc
	wire                       []byte
	scan                       receiveMemoryPackScan
	full, additional           ByteCount
	prefix, escrow             ByteCount
	claims                     map[*TransferMemoryBudget]ByteCount
	owner                      *decodedPackOwner
}

// Encodes genuine two-frame raw TCP ACK data and derives C and D from the real
// scanner and delivery prepayment path. Credit portions never exceed C.
func newPrivateProviderHeadRecoveryReviewFixture(t *testing.T, pressure bool, split bool) *privateProviderHeadRecoveryReviewFixture {
	t.Helper()
	frames := make([]*protocol.Frame, 2)
	for index := range frames {
		packet := ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagAck, 101, nil)
		frame, err := ipPacketToProviderFrame(packet, DefaultProtocolVersion)
		if err != nil || !frame.Raw {
			t.Fatal("review fixture failed to build actual raw provider TCP")
		}
		frames[index] = frame
	}
	messageId, sequenceId := NewId(), NewId()
	encoded := &sendPackFrame{messageId: messageId, sequenceId: sequenceId, sequenceNumber: 1, frames: frames}
	body := encoded.appendPack(nil)
	path := TransferPath{SourceId: NewId(), DestinationId: NewId()}
	wire := receiveMemoryScanTestWire(path, body, false)
	preview := MessagePoolCopy(wire)
	scan, status := scanReceiveMemoryPack(preview, path)
	MessagePoolReturn(preview)
	if status != receiveMemoryPackScanReady || !scan.loanSafe || scan.frameCount != 2 || scan.retainedByteCount <= 0 {
		t.Fatal("review fixture has no canonical paid decode geometry")
	}
	settings := DefaultReceiveBufferSettings()
	queue := newReceiveQueue(nil, 0)
	queue.lifetimeBudget = true
	ctx, cancel := context.WithCancel(context.Background())
	client := &Client{}
	client.reliableProviderIngress.Store(true)
	sequence := &ReceiveSequence{ctx: ctx, client: client, receiveQueue: queue, receiveBufferSettings: settings,
		nextSequenceNumber: 1, receiveContract: &sequenceContract{}, closeNotify: make(chan struct{}, 1)}
	probe := &receiveItem{transferItem: transferItem{queueByteCount: scan.retainedByteCount}, ack: true, frames: frames}
	if !sequence.prepareReceiveDeliveryCredit(probe) || !probe.deliveryPrepared || probe.QueueByteCount() <= scan.retainedByteCount {
		cancel()
		t.Fatal("review fixture has no actual downstream prepayment")
	}
	full := probe.QueueByteCount()
	root := NewTransferMemoryBudget(8 * full)
	parent := NewTransferMemoryBudgetWithParent(6*full, root)
	rx := NewTransferMemoryBudgetWithParent(2*full, parent)
	recovery := NewTransferMemoryBudgetWithParent(2*full, parent)
	sibling := NewTransferMemoryBudgetWithParent(8*full, root)
	settings.ReceiveQueueBudget, settings.ReceiveHeadRecoveryBudget = rx, recovery
	settings.ReceiveQueueRetainedByteAccounting = true
	settings.ReceiveHoldPolicy = ReceiveHoldCommittedPrefix
	queue.budget = rx
	self := &privateProviderHeadRecoveryReviewFixture{t: t, root: root, parent: parent, rx: rx, recovery: recovery,
		sibling: sibling, buffer: &ReceiveBuffer{client: client, receiveBufferSettings: settings}, sequence: sequence,
		cancel: cancel, wire: wire, scan: scan, full: full, additional: full - scan.retainedByteCount,
		prefix: scan.retainedByteCount, claims: map[*TransferMemoryBudget]ByteCount{}}
	t.Cleanup(self.close)
	if split {
		self.prefix = scan.retainedByteCount / 2
		if pressure {
			self.escrow = scan.retainedByteCount / 2
		}
	}
	if !sequence.reserveHoldPromise(self.prefix) {
		t.Fatal("review fixture could not fund its missing-head promise")
	}
	sequence.settleHoldPromise(true, 2, self.prefix)
	if pressure {
		if !rx.TryReserve(self.escrow) {
			t.Fatal("review fixture could not fund pressure escrow")
		}
		sequence.memoryPressure = receiveMemoryPressure{active: receiveMemoryPressureRequest{
			messageId: messageId, sequenceNumber: 1, queueByteCount: scan.retainedByteCount,
			messageByteCount: scan.messageByteCount}, activeSet: true, ready: true, reservedByteCount: self.escrow}
	}
	fresh := scan.retainedByteCount - self.prefix - self.escrow
	self.reserve(rx, rx.Available()-fresh)
	return self
}

// Synthetic occupied portions are owned by this fixture and returned once.
func (self *privateProviderHeadRecoveryReviewFixture) reserve(budget *TransferMemoryBudget, count ByteCount) {
	self.t.Helper()
	if count < 0 || !budget.TryReserve(count) {
		self.t.Fatal("review fixture could not reserve a competing portion")
	}
	self.claims[budget] += count
}

func (self *privateProviderHeadRecoveryReviewFixture) release(budget *TransferMemoryBudget, count ByteCount) {
	self.t.Helper()
	if count < 0 || self.claims[budget] < count {
		self.t.Fatal("review fixture does not own the released portion")
	}
	self.claims[budget] -= count
	budget.Release(count)
}

// Decode roots are materialized only after C is paid. The actual carrier root
// belongs to receiveItem and all payload roots belong to the same decoded owner.
func (self *privateProviderHeadRecoveryReviewFixture) take() *decodedPackOwner {
	self.t.Helper()
	if self.owner != nil {
		self.t.Fatal("review fixture attempted two live copies")
	}
	owner := self.buffer.takeReceiveMemoryOwner(self.sequence, self.scan.messageId, self.scan.sequenceNumber, self.scan.retainedByteCount)
	if owner == nil {
		self.t.Fatal("promised provider C did not fit its paid RX portions")
	}
	self.owner = owner
	owner.receiveItem.decodedOwner = owner
	carrier := MessagePoolCopy(self.wire)
	owner.receiveItem.transferFrameBytes = carrier
	decoded, ok := decodePackOwned(carrier[self.scan.packOffset:self.scan.packOffset+self.scan.packByteCount], owner)
	if !ok || decoded != owner {
		self.t.Fatal("actual provider decode lost its paid owner")
	}
	owner.receivePack.Pack = &owner.pack
	owner.receiveItem.decodedOwner, owner.receiveItem.ack = owner, true
	owner.receiveItem.frames, owner.receiveItem.messageByteCount = owner.pack.Frames, self.scan.messageByteCount
	if !self.sequence.bindReceiveMemoryOwner(&owner.receivePack) {
		self.t.Fatal("actual provider decode lost its generation binding")
	}
	physical := MessagePoolRootByteCount(carrier) + owner.metadataQueueByteCount()
	for _, frame := range owner.pack.Frames {
		physical += MessagePoolRootByteCount(frame.MessageBytes)
	}
	if physical != self.scan.retainedByteCount {
		self.t.Fatalf("review fixture paid C=%d for actual physical roots=%d", self.scan.retainedByteCount, physical)
	}
	return owner
}

func (self *privateProviderHeadRecoveryReviewFixture) returnOwner() {
	if self.owner != nil {
		self.owner.receiveItem.messagePoolReturn()
		self.owner = nil
	}
}

// Every path closes the original sequence, including an over-capacity shrink.
func (self *privateProviderHeadRecoveryReviewFixture) close() {
	self.cancel()
	self.returnOwner()
	self.sequence.releaseHoldPromise()
	for budget, count := range self.claims {
		budget.Release(count)
		delete(self.claims, budget)
	}
	for _, budget := range []*TransferMemoryBudget{self.root, self.parent, self.rx, self.recovery, self.sibling} {
		stats := budget.Stats()
		if stats.UsedByteCount != 0 || stats.QuotaByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
			self.t.Errorf("review fixture leaked actual ownership: %+v", stats)
		}
	}
	if self.sequence.holdPromise.ingressHead != nil || self.sequence.memoryPressure != (receiveMemoryPressure{}) {
		self.t.Error("review fixture retained a retired generation association")
	}
}

// Both zero and positive pressure borrowing must return to their exact RX
// accounts before the same live roots become a child-funded whole D owner.
func TestPrivateProviderRecoverySettlesExactPressureOwner(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, split := range []bool{false, true} {
		func() {
			f := newPrivateProviderHeadRecoveryReviewFixture(t, true, split)
			owner := f.take()
			if f.sequence.memoryPressure.owner != owner || f.sequence.memoryPressure.borrowedByteCount != f.escrow ||
				owner.receivePack.ingressLoanByteCount != f.prefix {
				t.Fatalf("split=%t: actual retry did not consume its disjoint paid portions", split)
			}
			before := f.root.UsedByteCount()
			if !f.sequence.prepareReceiveDeliveryCredit(&owner.receiveItem) || owner.receiveItem.memoryBudget != f.recovery ||
				owner.receiveItem.QueueByteCount() != f.full || owner.receivePack.ingressLoanByteCount != 0 ||
				f.sequence.holdPromise.reservedByteCount != f.prefix || f.sequence.holdPromise.ingressLoanByteCount != 0 ||
				f.sequence.memoryPressure.owner != nil || f.sequence.memoryPressure.borrowedByteCount != 0 ||
				f.sequence.memoryPressure.reservedByteCount != f.escrow || f.root.UsedByteCount() != before+f.full {
				t.Fatalf("split=%t: child migration did not settle the exact prefix and pressure association", split)
			}
			f.sequence.nextSequenceNumber = 2
			f.sequence.adoptReceiveMemoryOwner(&owner.receiveItem)
			f.sequence.settleHoldPromise(false, 0, 0)
			if f.sequence.memoryPressure.activeSet || f.rx.UsedByteCount() != f.claims[f.rx] ||
				f.recovery.UsedByteCount() != f.full || f.owner != owner {
				t.Fatal("adoption invalidated live child custody or retained old RX credit")
			}
			f.returnOwner()
			if f.recovery.UsedByteCount() != 0 || f.sequence.holdPromise.ingressHead != nil {
				t.Fatal("real provider root return did not release the same owner")
			}
		}()
	}
}

// Initial child or ancestor pressure must not make later recovery permanently
// ineligible through the prefix-only pressure-owner association it creates.
func TestPrivateProviderRecoveryRetriesAfterRealCapacityRelease(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, scope := range []string{"child", "parent", "root"} {
		func() {
			f := newPrivateProviderHeadRecoveryReviewFixture(t, false, false)
			blocked := f.recovery
			switch scope {
			case "parent":
				blocked = f.parent
			case "root":
				blocked = f.sibling
			}
			f.reserve(blocked, blocked.Available())
			owner := f.take()
			before := f.root.StatsWithDescendants(f.parent, f.rx, f.recovery, f.sibling)
			if f.sequence.prepareReceiveDeliveryCredit(&owner.receiveItem) || owner.receiveItem.memoryBudget != f.rx ||
				!reflect.DeepEqual(before, f.root.StatsWithDescendants(f.parent, f.rx, f.recovery, f.sibling)) {
				t.Fatalf("%s refusal changed a live physical claim", scope)
			}
			f.returnOwner()
			f.sequence.applyReceiveMemoryPressure()
			if !f.sequence.memoryPressure.ready || f.sequence.memoryPressure.borrowedByteCount != 0 ||
				f.sequence.holdPromise.reservedByteCount != f.prefix {
				t.Fatalf("%s refusal did not preserve retry custody", scope)
			}
			f.release(blocked, f.full)
			owner = f.take()
			if f.sequence.memoryPressure.owner != owner || f.sequence.memoryPressure.borrowedByteCount != 0 ||
				!f.sequence.prepareReceiveDeliveryCredit(&owner.receiveItem) || owner.receiveItem.memoryBudget != f.recovery {
				t.Fatalf("%s release left the exact source retry permanently barred", scope)
			}
			f.returnOwner()
		}()
	}
}

// Refused migrations leave counters and all three accounting portions intact.
// A later cap restoration can admit the same live owner without redecoding.
func TestPrivateProviderRecoveryAdmissionFencesAreAtomic(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, fence := range []string{"child_shrink", "parent_shrink", "root_shrink", "child_above_rx", "later_number", "unregistered_provider", "committed", "foreign_pressure_identity"} {
		func() {
			f := newPrivateProviderHeadRecoveryReviewFixture(t, true, true)
			owner := f.take()
			restore := func() {}
			switch fence {
			case "child_shrink":
				old := f.recovery.TotalByteCount()
				f.recovery.SetTotalByteCount(f.full - 1)
				restore = func() { f.recovery.SetTotalByteCount(old) }
			case "parent_shrink":
				old := f.parent.TotalByteCount()
				f.parent.SetTotalByteCount(f.parent.UsedByteCount() + f.full - 1)
				restore = func() { f.parent.SetTotalByteCount(old) }
			case "root_shrink":
				old := f.root.TotalByteCount()
				f.root.SetTotalByteCount(f.root.UsedByteCount() + f.full - 1)
				restore = func() { f.root.SetTotalByteCount(old) }
			case "child_above_rx":
				f.recovery.SetTotalByteCount(f.rx.TotalByteCount() + 1)
				restore = func() { f.recovery.SetTotalByteCount(f.rx.TotalByteCount()) }
			case "later_number":
				f.sequence.holdPromise.ingressFloor = 0
				restore = func() { f.sequence.holdPromise.ingressFloor = 1 }
			case "unregistered_provider":
				f.sequence.client.reliableProviderIngress.Store(false)
				restore = func() { f.sequence.client.reliableProviderIngress.Store(true) }
			case "committed":
				owner.receiveItem.committed = true
				restore = func() { owner.receiveItem.committed = false }
			case "foreign_pressure_identity":
				f.sequence.memoryPressure.active.messageId = NewId()
				restore = func() { f.sequence.memoryPressure.active.messageId = f.scan.messageId }
			}
			before := f.root.StatsWithDescendants(f.parent, f.rx, f.recovery)
			promise, pressure := f.sequence.holdPromise, f.sequence.memoryPressure
			if f.sequence.recoverProviderReceiveDeliveryCredit(&owner.receiveItem, f.additional) ||
				!reflect.DeepEqual(before, f.root.StatsWithDescendants(f.parent, f.rx, f.recovery)) ||
				promise != f.sequence.holdPromise || pressure != f.sequence.memoryPressure || owner.receiveItem.memoryBudget != f.rx {
				t.Fatalf("%s admission fence changed paid ownership", fence)
			}
			restore()
			if !f.sequence.prepareReceiveDeliveryCredit(&owner.receiveItem) || owner.receiveItem.memoryBudget != f.recovery {
				t.Fatalf("%s restoration did not admit the same funded owner", fence)
			}
			f.returnOwner()
		}()
	}
}

// Closing the sequence revokes restoration authority while a previously
// migrated owner keeps its actual child bytes through role shrink and return.
func TestPrivateProviderRecoveryCancellationPreservesLiveChild(t *testing.T) {
	assertMessagePoolOwnership(t)
	f := newPrivateProviderHeadRecoveryReviewFixture(t, true, true)
	owner := f.take()
	if !f.sequence.prepareReceiveDeliveryCredit(&owner.receiveItem) || owner.receiveItem.memoryBudget != f.recovery {
		t.Fatal("cancellation fixture did not migrate its real provider owner")
	}
	f.recovery.SetTotalByteCount(0)
	f.parent.SetTotalByteCount(0)
	f.cancel()
	f.sequence.releaseHoldPromise()
	if f.recovery.UsedByteCount() != f.full || owner.receiveItem.memoryBudget != f.recovery ||
		f.sequence.memoryPressure != (receiveMemoryPressure{}) || f.sequence.holdPromise.ingressHead != &owner.receivePack {
		t.Fatal("closed shrunken generation returned still-live child roots")
	}
	f.returnOwner()
	if f.recovery.UsedByteCount() != 0 || f.parent.UsedByteCount() != f.claims[f.rx] || f.sequence.holdPromise.ingressHead != nil {
		t.Fatal("canceled provider child did not release exactly its paid bytes")
	}
}

// Cancellation before migration cannot consume recovery or replenish a closed
// promise. A separately owned root sibling remains available and unchanged.
func TestPrivateProviderRecoveryCanceledPendingOwnerRefusesMigration(t *testing.T) {
	assertMessagePoolOwnership(t)
	f := newPrivateProviderHeadRecoveryReviewFixture(t, true, false)
	owner := f.take()
	f.reserve(f.sibling, f.full)
	f.cancel()
	f.sequence.releaseHoldPromise()
	before := f.root.StatsWithDescendants(f.parent, f.rx, f.recovery, f.sibling)
	if f.sequence.recoverProviderReceiveDeliveryCredit(&owner.receiveItem, f.additional) ||
		!reflect.DeepEqual(before, f.root.StatsWithDescendants(f.parent, f.rx, f.recovery, f.sibling)) {
		t.Fatal("canceled generation admitted new child custody")
	}
	f.returnOwner()
	if f.sibling.UsedByteCount() != f.full || f.recovery.UsedByteCount() != 0 || f.rx.UsedByteCount() != f.claims[f.rx] {
		t.Fatal("canceled owner return altered independent sibling or restored obsolete credit")
	}
}

// A partially promised decode already paid ordinary C in RX. Its atomic move
// needs only D minus those ordinary bytes at common ancestors; a transient
// whole-D reservation falsely refuses a final footprint that exactly fits.
func TestPrivateProviderRecoveryUsesOnlyNetAncestorGrowth(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, scope := range []string{"parent", "root"} {
		func() {
			f := newPrivateProviderHeadRecoveryReviewFixture(t, false, true)
			owner := f.take()
			ordinary := f.scan.retainedByteCount - f.prefix
			if ordinary <= 0 || owner.receivePack.ingressLoanByteCount != f.prefix ||
				f.sequence.memoryPressure != (receiveMemoryPressure{}) || f.rx.Available() != 0 {
				t.Fatal("net-growth fixture did not retain distinct prefix and fresh decoded bytes")
			}
			ancestor := f.parent
			if scope == "root" {
				ancestor = f.root
			}
			ancestor.SetTotalByteCount(ancestor.UsedByteCount() + f.full - ordinary)
			beforeRoot, beforeParent, beforeRx := f.root.Stats(), f.parent.Stats(), f.rx.Stats()
			if ancestor.Available() != f.full-ordinary || ancestor.Available() >= f.full {
				t.Fatal("net-growth boundary left room for a transient double reservation")
			}
			if !f.sequence.prepareReceiveDeliveryCredit(&owner.receiveItem) {
				t.Fatalf("%s exact final footprint was refused by transient double charging ordinary decoded C", scope)
			}
			if f.owner != owner || owner.receiveItem.memoryBudget != f.recovery || owner.receiveItem.QueueByteCount() != f.full ||
				owner.receivePack.ingressLoanByteCount != 0 || f.sequence.holdPromise.reservedByteCount != f.prefix ||
				f.sequence.holdPromise.ingressLoanByteCount != 0 || f.recovery.UsedByteCount() != f.full ||
				f.root.UsedByteCount() != beforeRoot.UsedByteCount+f.full-ordinary ||
				f.parent.UsedByteCount() != beforeParent.UsedByteCount+f.full-ordinary ||
				f.rx.UsedByteCount() != beforeRx.UsedByteCount-ordinary || ancestor.Available() != 0 {
				t.Fatalf("%s net migration did not preserve complete physical and loan ownership", scope)
			}
			for _, budget := range []*TransferMemoryBudget{f.root, f.parent, f.rx, f.recovery} {
				stats := budget.Stats()
				if stats.ReservedByteCount-stats.ReleasedByteCount != stats.UsedByteCount || stats.UsedByteCount > stats.TotalByteCount {
					t.Fatalf("%s migration violated an exact budget counter: %+v", scope, stats)
				}
			}
			f.returnOwner()
		}()
	}
}

// A duplicate may pay fresh C after the original consumed its prefix loan.
// That copy cannot occupy the recovery child while the exact original still
// owns custody, even though its own ordinary decode was fully funded.
func TestPrivateProviderRecoveryDuplicateCannotOccupyChild(t *testing.T) {
	assertMessagePoolOwnership(t)
	f := newPrivateProviderHeadRecoveryReviewFixture(t, false, false)
	original := f.take()
	f.release(f.rx, f.scan.retainedByteCount)
	duplicate := f.buffer.takeReceiveMemoryOwner(f.sequence, f.scan.messageId, f.scan.sequenceNumber, f.scan.retainedByteCount)
	if duplicate == nil {
		t.Fatal("fresh RX capacity did not pay a duplicate decode")
	}
	duplicate.receiveItem.decodedOwner = duplicate
	defer duplicate.receiveItem.messagePoolReturn()
	carrier := MessagePoolCopy(f.wire)
	duplicate.receiveItem.transferFrameBytes = carrier
	decoded, ok := decodePackOwned(carrier[f.scan.packOffset:f.scan.packOffset+f.scan.packByteCount], duplicate)
	if !ok || decoded != duplicate {
		t.Fatal("funded duplicate did not materialize its actual independent roots")
	}
	duplicate.receivePack.Pack = &duplicate.pack
	duplicate.receiveItem.decodedOwner, duplicate.receiveItem.ack = duplicate, true
	duplicate.receiveItem.frames, duplicate.receiveItem.messageByteCount = duplicate.pack.Frames, f.scan.messageByteCount
	if !f.sequence.bindReceiveMemoryOwner(&duplicate.receivePack) || duplicate.receivePack.ingressLoanByteCount != 0 ||
		original.receivePack.ingressLoanByteCount != f.prefix || f.rx.Available() != 0 {
		t.Fatal("duplicate did not preserve the original's exclusive borrowed prefix")
	}
	if duplicate.receiveItem.growReceiveMemory(f.additional) {
		t.Fatal("duplicate downstream growth did not encounter actual full RX")
	}
	before := f.root.StatsWithDescendants(f.parent, f.rx, f.recovery)
	promise, pressure := f.sequence.holdPromise, f.sequence.memoryPressure
	if f.sequence.recoverProviderReceiveDeliveryCredit(&duplicate.receiveItem, f.additional) ||
		!reflect.DeepEqual(before, f.root.StatsWithDescendants(f.parent, f.rx, f.recovery)) ||
		promise != f.sequence.holdPromise || pressure != f.sequence.memoryPressure ||
		duplicate.receiveItem.memoryBudget != f.rx || original.receiveItem.memoryBudget != f.rx || f.recovery.UsedByteCount() != 0 {
		t.Fatal("fresh-funded duplicate occupied promised-head recovery beside the original")
	}
}
