// Core child-owner transactions complement the real signed consumer burst.
// Synthetic occupied budget portions isolate refusal and return; actual pooled
// wire/payload roots exercise the admitted owner, bind, adoption and close.
package connect

import (
	"bytes"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

// The RX promise and escrow are paid in RX; only the head may enter the child.
type receiveHeadRecoveryFixture struct {
	t                  *testing.T
	root, rx, recovery *TransferMemoryBudget
	buffer             *ReceiveBuffer
	sequence           *ReceiveSequence
	wire               []byte
	scan               receiveMemoryPackScan
	owner              *decodedPackOwner
	foreign            ByteCount
}

// No worker runs here. Full Client routing/contract authority is exercised by
// the separate consumer burst, while these transitions use real decoder roots.
func newReceiveHeadRecoveryFixture(t *testing.T) *receiveHeadRecoveryFixture {
	t.Helper()
	path := TransferPath{SourceId: NewId(), DestinationId: NewId()}
	body, _, _ := receiveMemoryScanTestBody(t, 8, 1280)
	wire := MessagePoolCopy(receiveMemoryScanTestWire(path, body, false))
	scan, status := scanReceiveMemoryPack(wire, path)
	if status != receiveMemoryPackScanReady || scan.consumerFrameCount != 8 || scan.retainedByteCount <= 0 {
		MessagePoolReturn(wire)
		t.Fatal("consumer recovery fixture has no real eight-frame scan")
	}
	root, rx, buffer, sequence := receiveMemoryTestState(t, 3*scan.retainedByteCount)
	rx.SetTotalByteCount(scan.retainedByteCount)
	recovery := NewTransferMemoryBudgetWithParent(scan.retainedByteCount, root)
	sequence.receiveBufferSettings.ReceiveHeadRecoveryBudget = recovery
	sequence.nextSequenceNumber = scan.sequenceNumber
	self := &receiveHeadRecoveryFixture{t: t, root: root, rx: rx, recovery: recovery, buffer: buffer,
		sequence: sequence, wire: wire, scan: scan}
	t.Cleanup(func() {
		if self.owner != nil {
			self.owner.receivePack.messagePoolReturn()
			self.owner = nil
		}
		MessagePoolReturn(self.wire)
		self.wire = nil
		sequence.releaseHoldPromise()
		rx.Release(self.foreign)
		self.foreign = 0
		for _, budget := range []*TransferMemoryBudget{rx, recovery, root} {
			stats := budget.Stats()
			if stats.UsedByteCount != 0 || stats.QuotaByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
				t.Error("consumer recovery fixture did not return all physical claims")
			}
		}
	})
	prefix, escrow := scan.retainedByteCount/4, scan.retainedByteCount/4
	if !sequence.reserveHoldPromise(prefix) || !rx.TryReserve(escrow) {
		t.Fatal("consumer recovery fixture could not fund RX portions")
	}
	sequence.settleHoldPromise(true, scan.sequenceNumber+1, prefix)
	sequence.memoryPressure = receiveMemoryPressure{active: receiveMemoryPressureRequest{
		messageId: scan.messageId, sequenceNumber: scan.sequenceNumber, queueByteCount: scan.retainedByteCount},
		activeSet: true, ready: true, reservedByteCount: escrow}
	foreign := scan.retainedByteCount - prefix - escrow
	if !rx.TryReserve(foreign) {
		t.Fatal("consumer recovery fixture did not fill ordinary RX")
	}
	self.foreign = foreign
	return self
}

// Decode into the paid owner only after ordinary RX has actually refused it.
func (self *receiveHeadRecoveryFixture) decode() *decodedPackOwner {
	self.t.Helper()
	if owner := self.buffer.takeReceiveMemoryOwner(self.sequence, self.scan.messageId, self.scan.sequenceNumber, self.scan.retainedByteCount); owner != nil {
		owner.release()
		self.t.Fatal("ordinary RX unexpectedly admitted the larger head")
	}
	owner := self.buffer.takeReceiveHeadRecoveryOwner(self.sequence, self.scan)
	if owner == nil {
		self.t.Fatal("funded consumer child refused its exact promised head")
	}
	self.owner = owner
	decoded, ok := decodePackOwned(self.wire[self.scan.packOffset:self.scan.packOffset+self.scan.packByteCount], owner)
	if !ok || decoded != owner {
		self.t.Fatal("consumer child did not retain the actual decoded roots")
	}
	pack := &owner.receivePack
	pack.Pack = &owner.pack
	pack.TransferFrameBytes = self.wire
	self.wire = nil
	pack.MessageByteCount = MessageByteCount(pack.Pack.Frames)
	owner.receiveItem.decodedOwner = owner
	if !self.sequence.bindReceiveMemoryOwner(pack) {
		self.t.Fatal("consumer child lost its exact generation binding")
	}
	return owner
}

// Adopting a child owner retires RX credit in RX; the live child roots stay
// paid until their actual return, and cannot acquire downstream growth credit.
func TestReceiveMemoryHeadRecoveryAdoptionKeepsBudgetsSeparate(t *testing.T) {
	assertMessagePoolOwnership(t)
	f := newReceiveHeadRecoveryFixture(t)
	prefix, escrow := f.sequence.holdPromise.reservedByteCount, f.sequence.memoryPressure.reservedByteCount
	owner := f.decode()
	if owner.receiveItem.memoryBudget != f.recovery || owner.receivePack.ingressLoanByteCount != 0 ||
		f.sequence.holdPromise.reservedByteCount != prefix || f.sequence.memoryPressure.reservedByteCount != escrow ||
		f.root.UsedByteCount() != 2*f.scan.retainedByteCount || f.recovery.UsedByteCount() != f.scan.retainedByteCount {
		t.Fatal("child admission borrowed or double-counted an RX portion")
	}
	// Give this already-bound owner genuine spare child/ancestor capacity.
	// A refusal must come from fixed consumer custody, not a saturated cap.
	f.root.SetTotalByteCount(4 * f.scan.retainedByteCount)
	f.rx.SetTotalByteCount(2 * f.scan.retainedByteCount)
	f.recovery.SetTotalByteCount(2 * f.scan.retainedByteCount)
	if !f.rx.TryReserve(f.scan.retainedByteCount) {
		t.Fatal("growth guard ordinary RX fill")
	}
	f.foreign += f.scan.retainedByteCount
	if f.recovery.Available() != f.scan.retainedByteCount || f.root.Available() != f.scan.retainedByteCount {
		t.Fatal("growth guard did not leave independently available child and parent capacity")
	}
	before := f.root.UsedByteCount()
	if owner.receiveItem.growReceiveMemory(1) || f.root.UsedByteCount() != before ||
		f.sequence.holdPromise.reservedByteCount != prefix || f.sequence.memoryPressure.reservedByteCount != escrow {
		t.Fatal("consumer child spent RX growth or promise credit")
	}
	f.sequence.nextSequenceNumber++
	f.sequence.adoptReceiveMemoryOwner(&owner.receiveItem)
	f.sequence.settleHoldPromise(false, 0, 0)
	if f.rx.UsedByteCount() != f.foreign || f.recovery.UsedByteCount() != f.scan.retainedByteCount ||
		f.root.UsedByteCount() != f.foreign+f.scan.retainedByteCount || f.sequence.memoryPressure.activeSet {
		t.Fatal("adoption returned the live child or left RX pressure behind")
	}
	owner.receivePack.messagePoolReturn()
	f.owner = nil
	if f.recovery.UsedByteCount() != 0 || f.root.UsedByteCount() != f.foreign || f.sequence.holdPromise.ingressHead != nil {
		t.Fatal("real child return did not unlink and release its own budget once")
	}
}

// Deliberately remove only the child's claim at a worker-free boundary. The
// ordinary RX promise/escrow stay paid, masking the defect in a root-only
// lower-bound check. Restore custody before actual physical return or failure.
func TestReceiveMemoryHeadRecoveryAttributionRejectsMissingClaim(t *testing.T) {
	assertMessagePoolOwnership(t)
	f := newReceiveHeadRecoveryFixture(t)
	owner := f.decode()
	paid := receiveConsumerRecoveryState(f.root, f.rx, f.recovery, f.sequence, owner)
	f.recovery.Release(f.scan.retainedByteCount)
	masked := receiveConsumerRecoveryState(f.root, f.rx, f.recovery, f.sequence, owner)
	aggregateWouldPass := f.root.UsedByteCount() >= masked.physical
	if !f.recovery.TryReserve(f.scan.retainedByteCount) {
		t.Fatal("missing-claim control could not restore its isolated owner")
	}
	restored := receiveConsumerRecoveryState(f.root, f.rx, f.recovery, f.sequence, owner)
	if !paid.paid || masked.paid || !aggregateWouldPass || !restored.paid || masked.child != 0 ||
		masked.rx != paid.rx || masked.prefix != paid.prefix || masked.physical != paid.physical {
		t.Fatal("attributable child oracle did not distinguish the funded RX mask from this missing claim")
	}
}

// Closure revokes RX credit but never returns a still-live child carrier or
// payload. The late owner remains attached only until its actual final return.
func TestReceiveMemoryHeadRecoveryCloseKeepsLiveOwnerPaid(t *testing.T) {
	assertMessagePoolOwnership(t)
	f := newReceiveHeadRecoveryFixture(t)
	owner := f.decode()
	f.sequence.releaseHoldPromise()
	if f.recovery.UsedByteCount() != f.scan.retainedByteCount || f.rx.UsedByteCount() != f.foreign ||
		f.root.UsedByteCount() != f.foreign+f.scan.retainedByteCount || owner.receivePack.ingressLoanByteCount != 0 {
		t.Fatal("closing the generation returned a live child owner")
	}
	if duplicate := f.buffer.takeReceiveHeadRecoveryOwner(f.sequence, f.scan); duplicate != nil {
		duplicate.release()
		t.Fatal("closed generation acquired a child")
	}
	owner.receivePack.messagePoolReturn()
	f.owner = nil
	if f.recovery.UsedByteCount() != 0 || f.sequence.holdPromise.ingressHead != nil || f.root.UsedByteCount() != f.foreign {
		t.Fatal("late child return restored a closed RX promise or leaked")
	}
}

// A floor change can make an already-admitted copy obsolete. Revoking its
// logical eligibility leaves its own child claim intact until physical return.
func TestReceiveMemoryHeadRecoveryFloorAdvanceKeepsOwnerPaid(t *testing.T) {
	assertMessagePoolOwnership(t)
	f := newReceiveHeadRecoveryFixture(t)
	owner := f.decode()
	f.sequence.nextSequenceNumber += 2
	f.sequence.publishReceiveMemoryFloor()
	f.sequence.settleHoldPromise(false, 0, 0)
	if f.recovery.UsedByteCount() != f.scan.retainedByteCount || f.root.UsedByteCount() != f.foreign+f.scan.retainedByteCount ||
		owner.receivePack.ingressSequence != f.sequence || owner.receivePack.ingressLoanByteCount != 0 {
		t.Fatal("advancing the floor revoked a live child's physical claim")
	}
	if duplicate := f.buffer.takeReceiveHeadRecoveryOwner(f.sequence, f.scan); duplicate != nil {
		duplicate.release()
		t.Fatal("past original acquired another child")
	}
	owner.receivePack.messagePoolReturn()
	f.owner = nil
	if f.recovery.UsedByteCount() != 0 || f.sequence.holdPromise.ingressHead != nil || f.root.UsedByteCount() != f.foreign {
		t.Fatal("obsolete child restored credit or released another owner's bytes")
	}
}

// Fallback admission is both identity-specific and subordinate to every
// ordinary ancestor; no refused path consumes RX promise or child bytes.
func TestReceiveMemoryHeadRecoveryRefusalPreservesCredit(t *testing.T) {
	assertMessagePoolOwnership(t)
	f := newReceiveHeadRecoveryFixture(t)
	prefix, escrow := f.sequence.holdPromise.reservedByteCount, f.sequence.memoryPressure.reservedByteCount
	for _, number := range []uint64{f.scan.sequenceNumber - 1, f.scan.sequenceNumber + 1} {
		scan := f.scan
		scan.sequenceNumber = number
		if owner := f.buffer.takeReceiveHeadRecoveryOwner(f.sequence, scan); owner != nil {
			owner.release()
			t.Fatal("non-current head acquired recovery")
		}
	}
	if owner := f.buffer.takeReceiveHeadRecoveryOwner(nil, f.scan); owner != nil {
		owner.release()
		t.Fatal("unbound generation acquired recovery")
	}
	filled := f.root.Available()
	if !f.root.TryReserve(filled) {
		t.Fatal("shared-root pressure setup")
	}
	owner := f.buffer.takeReceiveHeadRecoveryOwner(f.sequence, f.scan)
	f.root.Release(filled)
	if owner != nil {
		owner.release()
		t.Fatal("child bypassed its full shared root")
	}
	f.recovery.SetTotalByteCount(f.scan.retainedByteCount - 1)
	owner = f.buffer.takeReceiveHeadRecoveryOwner(f.sequence, f.scan)
	f.recovery.SetTotalByteCount(f.scan.retainedByteCount)
	if owner != nil {
		owner.release()
		t.Fatal("head exceeded its child ceiling")
	}
	if f.recovery.UsedByteCount() != 0 || f.rx.UsedByteCount() != f.scan.retainedByteCount ||
		f.sequence.holdPromise.reservedByteCount != prefix || f.sequence.memoryPressure.reservedByteCount != escrow {
		t.Fatal("refusal changed existing RX credit")
	}
	for _, unrelated := range []*TransferMemoryBudget{NewTransferMemoryBudget(f.scan.retainedByteCount),
		NewTransferMemoryBudgetWithParent(f.scan.retainedByteCount, f.rx)} {
		f.sequence.receiveBufferSettings.ReceiveHeadRecoveryBudget = unrelated
		owner := f.buffer.takeReceiveHeadRecoveryOwner(f.sequence, f.scan)
		f.sequence.receiveBufferSettings.ReceiveHeadRecoveryBudget = f.recovery
		if owner != nil {
			owner.release()
			t.Fatal("recovery escaped its exact sibling-parent partition")
		}
	}
	first := f.decode()
	for _, scan := range []receiveMemoryPackScan{f.scan, func() receiveMemoryPackScan { scan := f.scan; scan.messageId = NewId(); return scan }()} {
		if duplicate := f.buffer.takeReceiveHeadRecoveryOwner(f.sequence, scan); duplicate != nil {
			duplicate.release()
			t.Fatal("duplicate head acquired a second child owner")
		}
	}
	if first.receiveItem.memoryBudget != f.recovery || f.root.UsedByteCount() != 2*f.scan.retainedByteCount {
		t.Fatal("duplicate refusal changed the original owner")
	}
}

// A matching budget pointer does not confer generation authority. Only the
// exact paid owner already registered at allocation can enter the handoff.
func TestReceiveMemoryHeadRecoveryBindingRequiresRegisteredOwner(t *testing.T) {
	assertMessagePoolOwnership(t)
	f := newReceiveHeadRecoveryFixture(t)
	owner := f.decode()
	copyPack := owner.receivePack
	if f.sequence.bindReceiveMemoryOwner(&copyPack) {
		t.Fatal("copied Pack impersonated its registered child owner")
	}
	replacement := &ReceiveSequence{ctx: f.sequence.ctx, receiveQueue: f.sequence.receiveQueue,
		receiveBufferSettings: f.sequence.receiveBufferSettings, nextSequenceNumber: f.sequence.nextSequenceNumber}
	if replacement.bindReceiveMemoryOwner(&owner.receivePack) {
		t.Fatal("another generation adopted the child")
	}
	f.root.admissionLock.Lock()
	f.sequence.holdPromise.ingressHead = nil
	f.root.admissionLock.Unlock()
	accepted := f.sequence.bindReceiveMemoryOwner(&owner.receivePack)
	f.root.admissionLock.Lock()
	f.sequence.holdPromise.ingressHead = &owner.receivePack
	f.root.admissionLock.Unlock()
	if accepted || !f.sequence.bindReceiveMemoryOwner(&owner.receivePack) || f.root.UsedByteCount() != 2*f.scan.retainedByteCount ||
		owner.receivePack.ingressLoanByteCount != 0 {
		t.Fatal("child binding did not require its exact paid ingress membership")
	}
}

// Actual Pack admission preserves target-exclusive quota. A Pack child below
// full ordinary RX must refuse; the SDK-shaped sibling can hand off at a full
// shared ancestor because its physical owner already paid that ancestor.
func TestReceiveMemoryHeadRecoveryPackQuotaPreservesLocalRefusal(t *testing.T) {
	assertMessagePoolOwnership(t)
	f := newReceiveHeadRecoveryFixture(t)
	owner := f.decode()
	sequence := f.sequence
	sequence.client = &Client{log: NewNoopLogger()}
	sequence.idleCondition = NewIdleCondition()
	sequence.packs = make(chan *ReceivePack, 1)
	sequence.packQueueSpace = make(chan struct{}, 1)
	sequence.packQueueBaseLimit = 1
	sequence.receiveBufferSettings.PackQueueRetainedByteAccounting = false
	t.Cleanup(func() {
		for len(sequence.packs) != 0 {
			sequence.releasePackQueue(<-sequence.packs)
		}
	})
	for _, packBudget := range []*TransferMemoryBudget{f.rx, NewTransferMemoryBudgetWithParent(f.scan.retainedByteCount, f.rx)} {
		sequence.receiveBufferSettings.PackQueueBudget = packBudget
		accepted, err := sequence.Pack(&owner.receivePack, 0)
		if accepted || err != nil || packBudget.Stats().QuotaByteCount != 0 || sequence.packQueueCount.Load() != 0 ||
			sequence.packQueueByteCount.Load() != 0 || f.root.UsedByteCount() != 2*f.scan.retainedByteCount {
			t.Fatal("child Pack bypassed or leaked target-exclusive RX quota")
		}
	}
	packBudget := NewTransferMemoryBudgetWithParent(f.scan.retainedByteCount, f.root)
	sequence.receiveBufferSettings.PackQueueBudget = packBudget
	foreign := f.root.Available()
	if !f.root.TryReserve(foreign) {
		t.Fatal("full common ancestor setup")
	}
	defer f.root.Release(foreign)
	before := f.root.UsedByteCount()
	accepted, err := sequence.Pack(&owner.receivePack, 0)
	wireCost := ByteCount(len(owner.receivePack.TransferFrameBytes))
	if !accepted || err != nil || packBudget.Stats().QuotaByteCount != wireCost || packBudget.UsedByteCount() != 0 ||
		f.root.UsedByteCount() != before || f.recovery.UsedByteCount() != f.scan.retainedByteCount {
		t.Fatal("sibling Pack did not reuse its owner's physical ancestor exactly once")
	}
	if pack := <-sequence.packs; pack != &owner.receivePack {
		t.Fatal("handoff changed the physical owner")
	} else {
		sequence.releasePackQueue(pack)
	}
	if packBudget.Stats().QuotaByteCount != 0 || sequence.packQueueCount.Load() != 0 || sequence.packQueueByteCount.Load() != 0 ||
		f.recovery.UsedByteCount() != f.scan.retainedByteCount || f.root.UsedByteCount() != before {
		t.Fatal("Pack dequeue released physical child roots or leaked quota")
	}
}

// Use the effective encoded application frames, not a claimed route family.
// Full contract proof is separate; empty/mixed/ToProvider/control/NoAck Packs
// must retain the existing RX/provider path and never acquire this partition.
func TestReceiveMemoryHeadRecoveryScannerExcludesOtherFrames(t *testing.T) {
	assertMessagePoolOwnership(t)
	f := newReceiveHeadRecoveryFixture(t)
	path := TransferPath{SourceId: NewId(), DestinationId: NewId()}
	packet := ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagAck, 11, bytes.Repeat([]byte{0x33}, 1240))
	consumer := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider, Raw: true, MessageBytes: packet}
	provider := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: packet}
	control := &protocol.Frame{MessageType: protocol.MessageType_TransferEncryptedControl, MessageBytes: []byte{1}}
	cases := []struct {
		name         string
		frames       []*protocol.Frame
		nack         bool
		wantConsumer int
	}{
		{name: "empty", frames: nil, wantConsumer: 0},
		{name: "empty payload", frames: []*protocol.Frame{{MessageType: protocol.MessageType_IpIpPacketFromProvider, Raw: true}}, wantConsumer: 0},
		{name: "provider", frames: []*protocol.Frame{provider}, wantConsumer: 0},
		{name: "mixed", frames: []*protocol.Frame{consumer, provider}, wantConsumer: 1},
		{name: "control", frames: []*protocol.Frame{consumer, control}, wantConsumer: 1},
		{name: "no ack", frames: []*protocol.Frame{consumer}, nack: true, wantConsumer: 1},
	}
	for _, row := range cases {
		pack := sendPackFrame{messageId: NewId(), sequenceId: NewId(), sequenceNumber: f.scan.sequenceNumber, frames: row.frames, nack: row.nack}
		wire := MessagePoolCopy(receiveMemoryScanTestWire(path, pack.appendPack(nil), false))
		scan, status := scanReceiveMemoryPack(wire, path)
		MessagePoolReturn(wire)
		if status != receiveMemoryPackScanReady || scan.consumerFrameCount != row.wantConsumer {
			t.Fatalf("%s: wrong effective consumer classification", row.name)
		}
		if owner := f.buffer.takeReceiveHeadRecoveryOwner(f.sequence, scan); owner != nil {
			owner.release()
			t.Fatalf("%s: excluded frame family entered recovery", row.name)
		}
	}
	if f.recovery.UsedByteCount() != 0 || f.root.UsedByteCount() != f.rx.UsedByteCount() {
		t.Fatal("excluded frame scan retained a claim")
	}
}

// The established local cipher isolates unwrap/owned-decode eligibility from
// real handshakes, which the separate client/session suites already exercise.
func (self *receiveHeadRecoveryFixture) encryptedClient() (*Client, *sequenceCipher) {
	settings := DefaultClientSettings()
	settings.ReceiveBufferSettings = self.sequence.receiveBufferSettings
	settings.EncryptionSettings.Mode = EncryptionModeRequired
	client := &Client{settings: settings, receiveBuffer: self.buffer, log: NewNoopLogger()}
	path := self.scan.path
	self.sequence.transferKey = TransferKey{ForceStream: true, CompanionContract: true, LogicalLane: 3}
	self.buffer.receiveSequences = map[receiveSequenceId]*ReceiveSequence{{Source: path.SourceMask(), SequenceId: self.scan.sequenceId,
		EncryptionRole: sequenceTlsRoleServer, LogicalLane: 3}: self.sequence}
	cipher := newFrameCodecTestSequenceCipher(self.t)
	client.encryptionSessionManager = &EncryptionSessionManager{settings: settings.EncryptionSettings,
		sessions: map[sessionKey]*peerEncryptionSession{{peerId: path.SourceId, role: sequenceTlsRoleServer}: {role: sequenceTlsRoleServer, establishedEpoch: &tlsHandshakeEpoch{derivedTlsCipher: cipher}}}}
	return client, cipher
}

// The child is available only after actual authenticated unwrap has exposed
// the application shape. Its claim is additional to separately paid scratch;
// this core test does not claim native-session or handshake performance.
func TestReceiveMemoryHeadRecoveryEncryptedInnerKeepsScratchSeparate(t *testing.T) {
	assertMessagePoolOwnership(t)
	f := newReceiveHeadRecoveryFixture(t)
	f.root.SetTotalByteCount(4 * f.scan.retainedByteCount)
	client, good := f.encryptedClient()
	defer client.releaseReceiveDecodeMemory()
	if !client.prepareReceiveDecodeMemory(nil, 16*1024) {
		t.Fatal("independent decrypt scratch could not be paid")
	}
	scratch := client.receiveDecodeReservedByteCount.Load()
	path := f.scan.path
	outer, err := good.SealOuterFrame(path, f.wire, protocol.SequenceRole_SequenceRoleClient, false)
	if err != nil {
		t.Fatal(err)
	}
	defer MessagePoolReturn(outer)
	outerDecoded := &decodedTransferFrame{}
	defer outerDecoded.release()
	if ok, pressure := client.unmarshalReceiveMemoryFrame(outer, outerDecoded, true, path, false, sequenceTlsRoleServer, false); !ok || pressure ||
		len(outerDecoded.frame.EncryptedTransferFrame) == 0 || f.recovery.UsedByteCount() != 0 {
		t.Fatal("opaque outer acquired a consumer child")
	}
	companion := false
	inner, role, _, err := client.unwrapFrame(path.SourceId, protocol.SequenceRole_SequenceRoleClient, &companion, outerDecoded.frame.EncryptedTransferFrame)
	if err != nil || role != sequenceTlsRoleServer || !bytes.Equal(inner, f.wire) {
		t.Fatal("actual encrypted consumer unwrap failed")
	}
	defer func() { MessagePoolReturn(inner) }()
	decoded := &decodedTransferFrame{}
	ok, pressure := client.unmarshalReceiveMemoryFrame(inner, decoded, true, path, true, role, false)
	if !ok || pressure {
		decoded.release()
		t.Fatal("paid decrypt did not reach the eligible child")
	}
	owner := decoded.detachPackOwner()
	decoded.release()
	if owner == nil {
		t.Fatal("decrypted child has no real owned roots")
	}
	f.owner = owner
	owner.receivePack.TransferFrameBytes = inner
	inner = nil
	if owner.receiveItem.memoryBudget != f.recovery || owner.receivePack.ingressLoanByteCount != 0 ||
		!f.sequence.bindReceiveMemoryOwner(&owner.receivePack) || f.root.UsedByteCount() != 2*f.scan.retainedByteCount+scratch ||
		client.receiveDecodeReservedByteCount.Load() != scratch {
		t.Fatal("encrypted child borrowed RX or scratch instead of paying its distinct claim")
	}
	owner.receivePack.messagePoolReturn()
	f.owner = nil
	if f.recovery.UsedByteCount() != 0 || f.root.UsedByteCount() != f.scan.retainedByteCount+scratch ||
		client.receiveDecodeReservedByteCount.Load() != scratch {
		t.Fatal("child return released another decode's standing scratch")
	}
}

// Real authenticated unwrap cannot turn an excluded application family into
// consumer eligibility. The decoder must retain ordinary RX refusal even
// though its separately paid scratch and recovery partition remain available.
func TestReceiveMemoryHeadRecoveryEncryptedExcludedFramesRefuse(t *testing.T) {
	assertMessagePoolOwnership(t)
	f := newReceiveHeadRecoveryFixture(t)
	f.root.SetTotalByteCount(4 * f.scan.retainedByteCount)
	client, cipher := f.encryptedClient()
	defer client.releaseReceiveDecodeMemory()
	if !client.prepareReceiveDecodeMemory(nil, 16*1024) {
		t.Fatal("excluded encrypted scratch setup")
	}
	scratch := client.receiveDecodeReservedByteCount.Load()
	packet := ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagAck, 11, bytes.Repeat([]byte{0x33}, 1240))
	consumer := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider, Raw: true, MessageBytes: packet}
	provider := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: packet}
	control := &protocol.Frame{MessageType: protocol.MessageType_TransferEncryptedControl, MessageBytes: []byte{1}}
	for _, frames := range [][]*protocol.Frame{{provider}, {consumer, provider}, {consumer, control}} {
		func() {
			// A fresh identity has no matching RX pressure escrow, while its
			// complete physical cost exceeds the remaining prefix portion.
			for len(frames) < 8 {
				frames = append(frames, consumer)
			}
			pack := sendPackFrame{messageId: NewId(), sequenceId: f.scan.sequenceId, sequenceNumber: f.scan.sequenceNumber,
				frames: frames, logicalLane: 3, forceStream: true, companionContract: true}
			wire := MessagePoolCopy(receiveMemoryScanTestWire(f.scan.path, pack.appendPack(nil), false))
			defer MessagePoolReturn(wire)
			scan, status := scanReceiveMemoryPack(wire, f.scan.path)
			if status != receiveMemoryPackScanReady || scan.retainedByteCount > f.recovery.TotalByteCount() ||
				scan.retainedByteCount <= f.sequence.holdPromise.reservedByteCount || scan.consumerFrameCount == scan.frameCount {
				t.Fatal("excluded encrypted shape did not isolate classification from available child capacity")
			}
			outer, err := cipher.SealOuterFrame(f.scan.path, wire, protocol.SequenceRole_SequenceRoleClient, false)
			if err != nil {
				t.Fatal(err)
			}
			defer MessagePoolReturn(outer)
			outerDecoded := &decodedTransferFrame{}
			defer outerDecoded.release()
			if ok, pressure := client.unmarshalReceiveMemoryFrame(outer, outerDecoded, true, f.scan.path, false, sequenceTlsRoleServer, false); !ok || pressure {
				t.Fatal("excluded outer could not be inspected")
			}
			companion := false
			inner, role, _, err := client.unwrapFrame(f.scan.path.SourceId, protocol.SequenceRole_SequenceRoleClient, &companion, outerDecoded.frame.EncryptedTransferFrame)
			if err != nil {
				t.Fatal(err)
			}
			defer MessagePoolReturn(inner)
			decoded := &decodedTransferFrame{}
			defer decoded.release()
			ok, pressure := client.unmarshalReceiveMemoryFrame(inner, decoded, true, f.scan.path, true, role, false)
			if ok || !pressure || decoded.packOwner != nil || f.recovery.UsedByteCount() != 0 ||
				f.rx.UsedByteCount() != f.scan.retainedByteCount || f.root.UsedByteCount() != f.scan.retainedByteCount+scratch {
				t.Fatal("authenticated excluded frames escaped ordinary RX refusal")
			}
		}()
	}
}

// An empty recovery child cannot fund decrypt scratch at a full ancestor.
// Opaque ciphertext is never classified as eligible consumer application data.
func TestReceiveMemoryHeadRecoveryCannotBypassScratchRefusal(t *testing.T) {
	assertMessagePoolOwnership(t)
	f := newReceiveHeadRecoveryFixture(t)
	settings := DefaultClientSettings()
	settings.ReceiveBufferSettings = f.sequence.receiveBufferSettings
	settings.EncryptionSettings.Mode = EncryptionModeRequired
	client := &Client{settings: settings, receiveBuffer: f.buffer, log: NewNoopLogger()}
	defer client.releaseReceiveDecodeMemory()
	good := newFrameCodecTestSequenceCipher(t)
	outer, err := good.SealOuterFrame(f.scan.path, f.wire, protocol.SequenceRole_SequenceRoleClient, false)
	if err != nil {
		t.Fatal(err)
	}
	defer MessagePoolReturn(outer)
	foreign := f.root.Available()
	if !f.root.TryReserve(foreign) {
		t.Fatal("scratch pressure setup")
	}
	defer f.root.Release(foreign)
	before := f.root.UsedByteCount()
	if client.prepareReceiveDecodeMemory(outer, 16*1024) || client.receiveDecodeReservedByteCount.Load() != 0 ||
		f.recovery.UsedByteCount() != 0 || f.root.UsedByteCount() != before {
		t.Fatal("recovery partition bypassed physical scratch refusal")
	}
	scan, status := scanReceiveMemoryPack(outer, f.scan.path)
	if status != receiveMemoryPackScanNotPack || !scan.encrypted || scan.consumerFrameCount != 0 {
		t.Fatal("opaque ciphertext claimed application eligibility")
	}
	if owner := f.buffer.takeReceiveHeadRecoveryOwner(f.sequence, scan); owner != nil {
		owner.release()
		t.Fatal("opaque ciphertext entered recovery child")
	}
}
