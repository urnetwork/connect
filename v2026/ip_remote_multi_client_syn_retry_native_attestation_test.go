// Caller-owned bytes attest one ordinary native SYN without borrowing queued bytes.
package connect

import (
	"bytes"
	"context"
	"testing"
	"unsafe"
)

// Detached scalar controls complement the unchanged real native/Transfer root.
type nativeSynAttestationFixture struct {
	parent *RemoteUserNatMultiClient
	update *multiClientChannelUpdate
	owner  *tcpSynRetry
	group  *parsedPacketGroup
	source TransferPath
	cancel context.CancelFunc
}

// All bytes are ordinary caller-owned allocations; no packet-pool share is invented.
func newNativeSynAttestationFixture(t *testing.T, version int) *nativeSynAttestationFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	path := reraceFlow(version, 0, 51541)
	packet := reraceTcpPacket(path, tcpFlagSyn, path.SequenceNumber, 0, nil)
	parsed, payload, err := ParseIpPathWithPayload(packet)
	if err != nil {
		t.Fatal(err)
	}
	source := SourceId(NewId())
	pin := flowPin{appId: "native-attestation", site: true}
	update := &multiClientChannelUpdate{ctx: ctx, sequenceAdmissionEpoch: 7, synGenerationSeen: true, synGenerationNumber: parsed.SequenceNumber}
	owner := &tcpSynRetry{update: update, packet: bytes.Clone(packet), source: source, pin: pin, sequence: parsed.SequenceNumber, serial: 17, references: 1}
	update.synRetry = owner
	return &nativeSynAttestationFixture{
		parent: &RemoteUserNatMultiClient{ctx: context.Background(), flowReaperDone: make(chan struct{})}, update: update, owner: owner, source: source, cancel: cancel,
		group: &parsedPacketGroup{ipPath: parsed, pin: pin, packets: []parsedPacket{{packet: packet, ipPath: parsed, payload: payload, pin: pin}}},
	}
}

// Call the actual pre-handoff consumer without holding its flow lock.
func (self *nativeSynAttestationFixture) prepare(group *parsedPacketGroup) {
	group.prepareSynRetry(self.source, 0, self.parent, self.update, group.ipPath)
}

// Exact identity remains ordinary ingress rather than an owned dispatcher request.
func (self *nativeSynAttestationFixture) requireAttested(t *testing.T, group *parsedPacketGroup, label string) {
	t.Helper()
	request := group.synRetry
	if request == nil || request.serial != 0 || request.nativeOwnerSerial != self.owner.serial || request.update != self.update ||
		request.source != self.source || request.provideMode != self.owner.provideMode || request.pin != self.owner.pin || group.isSynRetry() {
		t.Fatalf("%s: exact caller did not obtain only the ordinary scalar owner attestation", label)
	}
}

// Publish only the detached metadata used by the existing pending-claim guard.
func (self *nativeSynAttestationFixture) makePending(group *parsedPacketGroup) {
	client := &multiClientChannel{ctx: context.Background()}
	self.update.race = &multiClientChannelUpdateRace{collapseOrder: 9, clientStates: map[*multiClientChannel]*multiClientChannelRaceClientState{client: {}}}
	group.collapseAdmission = tcpCollapseAdmission{update: self.update, epoch: self.update.sequenceAdmissionEpoch}
	group.completionClient = client
	group.collapseSource = &SendSequence{ctx: context.Background()}
	const raw = groupCompletionAck | groupCompletionTracked | groupCompletionAdmitted | groupCompletionLinked | groupCompletionCandidate
	group.completionFlags.Store(raw | 9<<16)
	self.update.sequenceClaims = group
}

// Every classification is made with the same lock required by production.
func (self *nativeSynAttestationFixture) valid() bool {
	self.update.stateLock.Lock()
	defer self.update.stateLock.Unlock()
	return self.owner.validWithLock(self.update)
}

// Each negative first proves the real preparation positive before changing one premise.
func TestNativeSynRetryAttestsOnlyExactCallerOwner(t *testing.T) {
	for _, test := range []struct {
		name  string
		alter func(*nativeSynAttestationFixture)
	}{
		{name: "different full packet", alter: func(f *nativeSynAttestationFixture) {
			f.group.packets[0].packet = reraceTcpPacket(f.group.ipPath, tcpFlagSyn, f.group.ipPath.SequenceNumber, 1, nil)
		}},
		{name: "different source", alter: func(f *nativeSynAttestationFixture) { f.source.SourceId = NewId() }},
		{name: "different mode", alter: func(f *nativeSynAttestationFixture) { f.owner.provideMode = 1 }},
		{name: "different app pin", alter: func(f *nativeSynAttestationFixture) { f.group.pin.appId += "-foreign" }},
		{name: "different site pin", alter: func(f *nativeSynAttestationFixture) { f.group.pin.site = false }},
		{name: "different sequence", alter: func(f *nativeSynAttestationFixture) { f.owner.sequence++ }},
		{name: "different generation", alter: func(f *nativeSynAttestationFixture) { f.update.synGenerationNumber++ }},
		{name: "foreign owner update", alter: func(f *nativeSynAttestationFixture) {
			f.owner.update = &multiClientChannelUpdate{ctx: context.Background()}
		}},
		{name: "missing owner", alter: func(f *nativeSynAttestationFixture) { f.update.synRetry = nil }},
		{name: "zero owner serial", alter: func(f *nativeSynAttestationFixture) { f.owner.serial = 0 }},
		{name: "empty owner bytes", alter: func(f *nativeSynAttestationFixture) { f.owner.packet = nil }},
		{name: "multiple packets", alter: func(f *nativeSynAttestationFixture) { f.group.packets = append(f.group.packets, f.group.packets[0]) }},
		{name: "reset control", alter: func(f *nativeSynAttestationFixture) { f.group.ipPath.Rst = true }},
		{name: "fin control", alter: func(f *nativeSynAttestationFixture) { f.group.ipPath.Fin = true }},
		{name: "ack control", alter: func(f *nativeSynAttestationFixture) { f.group.ipPath.Ack = true }},
		{name: "non tcp", alter: func(f *nativeSynAttestationFixture) { f.group.ipPath.Protocol = IpProtocolUdp }},
		{name: "reset flow", alter: func(f *nativeSynAttestationFixture) { f.update.sourceReset.Store(true) }},
		{name: "established flow", alter: func(f *nativeSynAttestationFixture) { f.update.receivedInbound.Store(true) }},
		{name: "closed source enrollment", alter: func(f *nativeSynAttestationFixture) { f.update.affinitySources.closed = true }},
		{name: "closed flow", alter: func(f *nativeSynAttestationFixture) { f.cancel() }},
	} {
		f := newNativeSynAttestationFixture(t, 4)
		f.prepare(f.group)
		f.requireAttested(t, f.group, test.name)
		f.group.synRetry = nil
		test.alter(f)
		f.prepare(f.group)
		if request := f.group.synRetry; request != nil && (request.serial != 0 || request.nativeOwnerSerial != 0 || request.update != nil) {
			t.Fatalf("%s: foreign or closed caller earned retained owner identity", test.name)
		}
	}
	for _, version := range []int{4, 6} {
		f := newNativeSynAttestationFixture(t, version)
		f.prepare(f.group)
		f.requireAttested(t, f.group, "positive")
	}
}

// An attested ordinary request must pass every existing raw-candidate fence.
func TestNativeSynRetryAttestedNativeKeepsPendingControlFences(t *testing.T) {
	for _, test := range []struct {
		name  string
		alter func(*nativeSynAttestationFixture)
	}{
		{name: "unattested native", alter: func(f *nativeSynAttestationFixture) { f.group.synRetry.nativeOwnerSerial = 0 }},
		{name: "foreign native serial", alter: func(f *nativeSynAttestationFixture) { f.group.synRetry.nativeOwnerSerial++ }},
		{name: "foreign request update", alter: func(f *nativeSynAttestationFixture) {
			f.group.synRetry.update = &multiClientChannelUpdate{ctx: context.Background()}
		}},
		{name: "missing request", alter: func(f *nativeSynAttestationFixture) { f.group.synRetry = nil }},
		{name: "stale epoch", alter: func(f *nativeSynAttestationFixture) { f.group.collapseAdmission.epoch-- }},
		{name: "foreign claim update", alter: func(f *nativeSynAttestationFixture) {
			f.group.collapseAdmission.update = &multiClientChannelUpdate{ctx: context.Background(), race: f.update.race}
		}},
		{name: "stale race stamp", alter: func(f *nativeSynAttestationFixture) { f.group.completionFlags.Add(1 << 16) }},
		{name: "stale continuation", alter: func(f *nativeSynAttestationFixture) { f.group.synRetry.raceOrder = 8 }},
		{name: "single race", alter: func(f *nativeSynAttestationFixture) { f.update.race.single = true }},
		{name: "different syn", alter: func(f *nativeSynAttestationFixture) { f.group.ipPath.SequenceNumber++ }},
		{name: "reset control", alter: func(f *nativeSynAttestationFixture) { f.group.ipPath.Syn, f.group.ipPath.Rst = false, true }},
		{name: "syn ack", alter: func(f *nativeSynAttestationFixture) { f.group.ipPath.Ack = true }},
		{name: "syn fin", alter: func(f *nativeSynAttestationFixture) { f.group.ipPath.Fin = true }},
		{name: "multiple packets", alter: func(f *nativeSynAttestationFixture) { f.group.packets = append(f.group.packets, f.group.packets[0]) }},
		{name: "non tcp", alter: func(f *nativeSynAttestationFixture) { f.group.ipPath.Protocol = IpProtocolUdp }},
		{name: "missing source", alter: func(f *nativeSynAttestationFixture) { f.group.collapseSource = nil }},
		{name: "canceled source", alter: func(f *nativeSynAttestationFixture) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			f.group.collapseSource.ctx = ctx
		}},
		{name: "unreliable claim", alter: func(f *nativeSynAttestationFixture) { f.group.completionFlags.And(^groupCompletionAck) }},
		{name: "untracked claim", alter: func(f *nativeSynAttestationFixture) { f.group.completionFlags.And(^groupCompletionTracked) }},
		{name: "unadmitted claim", alter: func(f *nativeSynAttestationFixture) { f.group.completionFlags.And(^groupCompletionAdmitted) }},
		{name: "unlinked claim", alter: func(f *nativeSynAttestationFixture) { f.group.completionFlags.And(^groupCompletionLinked) }},
		{name: "disposed claim", alter: func(f *nativeSynAttestationFixture) { f.group.completionFlags.Or(groupCompletionDisposed) }},
		{name: "done claim", alter: func(f *nativeSynAttestationFixture) { f.group.completionFlags.Or(groupCompletionDone) }},
	} {
		f := newNativeSynAttestationFixture(t, 4)
		f.prepare(f.group)
		f.requireAttested(t, f.group, test.name)
		f.makePending(f.group)
		if !f.valid() {
			t.Fatalf("%s: valid attested native raw candidate premise failed", test.name)
		}
		test.alter(f)
		f.update.stateLock.Lock()
		pending, valid := f.update.pendingCollapseCloseWithLock(), f.owner.validWithLock(f.update)
		f.update.stateLock.Unlock()
		if !pending || valid {
			t.Fatalf("%s: pending fence was bypassed or its premise disappeared: pending=%t valid=%t", test.name, pending, valid)
		}
	}
}

// Repeated callers use scalar identity even when queued packet storage is unavailable.
func TestNativeSynRetryRepeatedNativeAttestationUsesOnlyCurrentOwner(t *testing.T) {
	f := newNativeSynAttestationFixture(t, 4)
	f.prepare(f.group)
	f.requireAttested(t, f.group, "positive")
	f.makePending(f.group)
	f.group.packets[0].packet = nil
	if !f.valid() {
		t.Fatal("pending admission read unavailable queued packet storage")
	}
	fresh := func() *parsedPacketGroup {
		packet := bytes.Clone(f.owner.packet)
		path, payload, err := ParseIpPathWithPayload(packet)
		if err != nil {
			t.Fatal(err)
		}
		return &parsedPacketGroup{ipPath: path, pin: f.owner.pin, packets: []parsedPacket{{packet: packet, ipPath: path, payload: payload, pin: f.owner.pin}}}
	}
	native3 := fresh()
	f.prepare(native3)
	f.requireAttested(t, native3, "native3")
	native3.collapseAdmission = f.group.collapseAdmission
	native3.completionClient = f.group.completionClient
	native3.collapseSource = f.group.collapseSource
	native3.completionFlags.Store(f.group.completionFlags.Load())
	f.group.collapseNext = native3
	if !f.valid() {
		t.Fatal("the second exact ordinary attestation could not coexist with the first pending claim")
	}
	for _, serial := range []uint64{0, f.owner.serial + 1} {
		f.group.synRetry.nativeOwnerSerial = serial
		if f.valid() {
			t.Fatal("unattested or foreign preexisting claim did not fence its owner")
		}
		next := fresh()
		f.prepare(next)
		if next.synRetry == nil || next.synRetry.serial != 0 || next.synRetry.nativeOwnerSerial != 0 || next.synRetry.update != nil {
			t.Fatal("preexisting unmatched claim permitted a fresh caller to mint owner identity")
		}
	}
	f.group.synRetry.nativeOwnerSerial = f.owner.serial
	if !f.valid() {
		t.Fatal("restored exact attestation did not restore its positive premise")
	}
	old := f.owner
	next := &tcpSynRetry{update: f.update, packet: bytes.Clone(old.packet), source: old.source, provideMode: old.provideMode, pin: old.pin, sequence: old.sequence, serial: old.serial + 1, references: 1}
	f.update.synRetry = next
	f.update.stateLock.Lock()
	staleValid, nextValid := old.validWithLock(f.update), next.validWithLock(f.update)
	f.update.stateLock.Unlock()
	if staleValid || nextValid || native3.synRetry.nativeOwnerSerial != old.serial || native3.synRetry.serial != 0 {
		t.Fatal("old scalar attestation crossed replacement cache ownership")
	}
}

// The stamp adds no packet/share/credit allocation beyond the existing request.
func TestNativeSynRetryNativeAttestationKeepsAllocationAndCreditAccounting(t *testing.T) {
	f := newNativeSynAttestationFixture(t, 4)
	budget := NewTransferMemoryBudget(4096)
	queue := retainedControlTestSendQueue()
	queue.setBudget(budget, 0)
	credit, ok := queue.tryReserveRetainedControl(256, 4096, nil)
	if !ok || credit == nil {
		t.Fatal("actual retained-control budget reservation failed")
	}
	t.Cleanup(func() {
		credit.Release()
		reserved, released := budget.Counts()
		if budget.UsedByteCount() != 0 || reserved != released {
			t.Error("final actual retained-control credit did not balance")
		}
	})
	f.owner.credit = credit
	reserved, released := budget.Counts()
	owner, original, refs := f.owner, bytes.Clone(f.owner.packet), f.owner.references
	f.update.synRetry = nil
	baseline := testing.AllocsPerRun(1000, func() { f.group.synRetry = nil; f.prepare(f.group) })
	f.update.synRetry = owner
	attested := testing.AllocsPerRun(1000, func() { f.group.synRetry = nil; f.prepare(f.group) })
	f.requireAttested(t, f.group, "positive")
	afterReserved, afterReleased := budget.Counts()
	if baseline != 1 || attested != baseline || budget.UsedByteCount() != 256 || reserved != afterReserved || released != afterReleased ||
		f.update.synRetry != owner || owner.references != refs || owner.credit != credit || credit.released.Load() || !bytes.Equal(owner.packet, original) {
		t.Fatalf("attestation changed request allocation or retained ownership: baseline=%v attested=%v used=%d", baseline, attested, budget.UsedByteCount())
	}
	f.group.completionFlags.Store(groupCompletionTracked)
	withRequest := f.group.sendGroupMemoryByteCount()
	f.group.synRetry = nil
	withoutRequest := f.group.sendGroupMemoryByteCount()
	wanted := (ByteCount(unsafe.Sizeof(tcpSynRetryRequest{})) + 63) / 64 * 64
	if withRequest-withoutRequest != wanted {
		t.Fatal("the full request including its scalar stamp was not charged by the existing metadata budget")
	}
}
