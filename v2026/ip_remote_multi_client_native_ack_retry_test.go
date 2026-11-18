// Detached owner controls complement the real native TCP/Transfer roots.
package connect

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
	"unsafe"

	"github.com/urnetwork/connect/v2026/protocol"
)

// No actor or delivery receipt is fabricated here. These controls exercise
// actual count-gate registration and local-copy lifetime transactions only.
type nativeAckRetryOwnerFixture struct {
	parent       *RemoteUserNatMultiClient
	update       *multiClientChannelUpdate
	source       *SendSequence
	packet       []byte
	cancelSource context.CancelFunc
}

func newNativeAckRetryOwnerFixture(t *testing.T, version int) *nativeAckRetryOwnerFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	sourceCtx, cancelSource := context.WithCancel(ctx)
	path := reraceFlow(version, 0, 51543)
	packet := reraceTcpPacket(path, tcpFlagAck, 100, 500, nil)
	path, _, err := ParseIpPathWithPayload(packet)
	if err != nil {
		t.Fatal(err)
	}
	parent := &RemoteUserNatMultiClient{ctx: ctx, flowReaperDone: make(chan struct{}), flowReaperWake: make(chan struct{}, 1),
		ip4PathUpdates: map[Ip4Path]*multiClientChannelUpdate{}, ip6PathUpdates: map[Ip6Path]*multiClientChannelUpdate{}}
	update := newMultiClientChannelUpdate(ctx, path)
	update.sequenceParent = parent
	update.receivedInbound.Store(true)
	update.sequenceAckPositionSeen, update.sequenceAckPosition = true, 100
	update.sequenceAdmissionStateSeen, update.sequenceAdmissionAck, update.sequenceAdmissionWindow = true, 500, 0
	update.client.Store(&multiClientChannel{ctx: ctx})
	if version == 4 {
		parent.ip4PathUpdates[path.ToIp4Path()] = update
	} else {
		parent.ip6PathUpdates[path.ToIp6Path()] = update
	}
	source := &SendSequence{ctx: sourceCtx, resendQueue: retainedControlTestSendQueue(), retainedAdmissionWake: make(chan struct{}, 1), protocolAdmission: newSendPackAdmission(3)}
	for range 3 {
		if acquired, closed, _ := source.protocolAdmission.tryAcquire(sendSchedulingKey{}, false); !acquired || closed {
			t.Fatal("real raw count fixture did not fill")
		}
	}
	f := &nativeAckRetryOwnerFixture{parent: parent, update: update, source: source, packet: packet, cancelSource: cancelSource}
	t.Cleanup(func() {
		cancelSource()
		cancel()
		parent.drainNativeAckRetries()
		for range 3 {
			source.protocolAdmission.release(sendSchedulingKey{})
		}
		if update.nativeAckRetry != nil || parent.nativeAckRetryCount != 0 || parent.nativeAckRetryHead != nil || parent.nativeAckRetryTail != nil {
			t.Error("local retry owner survived fixture close")
		}
	})
	return f
}

// The packet is caller-owned; retention must copy it before its caller returns.
func (self *nativeAckRetryOwnerFixture) prepare(t *testing.T, packet []byte) *parsedPacketGroup {
	t.Helper()
	path, payload, err := ParseIpPathWithPayload(packet)
	if err != nil {
		t.Fatal(err)
	}
	group := &parsedPacketGroup{ipPath: path, byteCount: ByteCount(len(packet)), packets: []parsedPacket{{packet: packet, ipPath: path, payload: payload}}}
	self.update.stateLock.Lock()
	group.collapseAdmission = tcpCollapseAdmission{update: self.update, epoch: self.update.sequenceAdmissionEpoch}
	group.prepareNativeAckRetryWithLock(self.update)
	self.update.stateLock.Unlock()
	group.noteNativeAckCapacityRefusal(self.source, self.source.protocolAdmission.protocolReadinessGate())
	return group
}

func (self *nativeAckRetryOwnerFixture) retain(t *testing.T, group *parsedPacketGroup) *tcpNativeAckRetry {
	t.Helper()
	if group.nativeAckRetry == nil {
		t.Fatal("exact established reopening lacks eligibility")
	}
	group.noteNativeAckCapacityRefusal(self.source, self.source.protocolAdmission.protocolReadinessGate())
	self.parent.retainNativeAckRetry(group, group.nativeAckRetry, SourceId(NewId()), protocol.ProvideMode_Network)
	owner := self.update.nativeAckRetry
	if owner == nil || self.parent.nativeAckRetryCount != 1 || !bytes.Equal(owner.packet, group.packets[0].packet) || owner.credit == nil || !owner.credit.noBudget || owner.credit.budget != nil || owner.credit.local != nil || owner.credit.byteCount != 0 {
		t.Fatal("retained protocol copy lacks exact bounded no-DATA lifetime")
	}
	return owner
}

// A real raw release preceding watch registration remains a level, not a lost edge.
func TestNativeWindowRetryCapacityOpeningBeforeRegistrationIsObserved(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	f := newNativeAckRetryOwnerFixture(t, 4)
	group := f.prepare(t, f.packet)
	gate := f.source.protocolAdmission.protocolReadinessGate()
	if !gate.Load() {
		t.Fatal("full raw count did not close readiness")
	}
	f.source.protocolAdmission.release(sendSchedulingKey{})
	owner := f.retain(t, group)
	if owner.attempt.gate != gate || !gate.watchGranted(&owner.waiter, owner.installation) {
		t.Fatal("actual opening before registration lost its directed retry turn")
	}
	select {
	case <-f.parent.flowReaperWake:
	default:
		t.Fatal("registration did not wake the existing reaper")
	}
	// Source close, not a counterfeit send result, disposes this metadata-only control.
	credit := owner.credit
	f.cancelSource()
	f.parent.drainNativeAckRetries()
	if !credit.released.Load() || owner.packet != nil || owner.credit != nil {
		t.Fatal("closed source retained its observed ready owner")
	}
}

// A closed source cannot migrate its pending reopening to another generation.
func TestNativeWindowRetrySourceShutdownReturnsWaitingOwner(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	f := newNativeAckRetryOwnerFixture(t, 4)
	owner := f.retain(t, f.prepare(t, f.packet))
	credit := owner.credit
	witness := MessagePoolShareReadOnly(owner.packet)
	f.cancelSource()
	f.parent.drainNativeAckRetries()
	if !credit.released.Load() || owner.packet != nil || owner.credit != nil || owner.memoryByteCount != 0 || f.parent.nativeAckRetryCount != 0 || !MessagePoolReturn(witness) {
		t.Fatal("source shutdown lost exact copied packet/token custody")
	}
	f.source.protocolAdmission.release(sendSchedulingKey{})
	f.parent.drainNativeAckRetries()
	if f.update.nativeAckRetry != nil {
		t.Fatal("later capacity revived the canceled source copy")
	}
}

// IPv6 destination options cross a real pool class without creating DATA.
// Both old and replacement tokens remain lifetime-only; size is descriptive.
func TestNativeWindowRetryLargerReplacementReturnsPriorPacketAndToken(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	f := newNativeAckRetryOwnerFixture(t, 6)
	owner := f.retain(t, f.prepare(t, f.packet))
	oldCredit, oldSize, oldCapacity := owner.credit, owner.memoryByteCount, cap(owner.packet)
	witness := MessagePoolShareReadOnly(owner.packet)
	large := make([]byte, len(f.packet)+256)
	copy(large[:40], f.packet[:40])
	large[6] = 60
	large[40], large[41] = 6, 31
	copy(large[40+256:], f.packet[40:])
	binary.BigEndian.PutUint16(large[4:6], uint16(len(large)-40))
	if !protocolClassPureTcpControl(large) {
		t.Fatal("option-bearing replacement is not a complete protocol packet")
	}
	replaced := f.retain(t, f.prepare(t, large))
	wanted := addReceiveQueueByteCount(addReceiveQueueByteCount(retainedMessageCapacity(ByteCount(len(large))), retainedMessageCapacity(ByteCount(unsafe.Sizeof(tcpNativeAckRetry{})))), transferRetainedControlCreditByteCount())
	if replaced != owner || owner.credit == oldCredit || !oldCredit.released.Load() || !MessagePoolReturn(witness) || cap(owner.packet) <= oldCapacity || owner.memoryByteCount <= oldSize || owner.memoryByteCount != wanted || f.parent.nativeAckRetryCount != 1 {
		t.Fatal("larger replacement reused stale lifetime/size or retained a second packet owner")
	}
	newCredit := owner.credit
	newWitness := MessagePoolShareReadOnly(owner.packet)
	f.cancelSource()
	f.parent.drainNativeAckRetries()
	if !newCredit.released.Load() || owner.packet != nil || owner.memoryByteCount != 0 || !MessagePoolReturn(newWitness) {
		t.Fatal("replacement final return was not exact")
	}
}

// Unknown batch custody and non-reopening feedback never gain a local copy.
// Later authoritative feedback still revokes an older pending reopening.
func TestNativeWindowRetryEligibilityAndSupersessionRemainNarrow(t *testing.T) {
	for _, row := range []struct {
		name  string
		alter func(*nativeAckRetryOwnerFixture, *parsedPacketGroup)
	}{
		{name: "multiple ACKs", alter: func(_ *nativeAckRetryOwnerFixture, g *parsedPacketGroup) { g.packets = append(g.packets, g.packets[0]) }},
		{name: "mixed ACK and actual DATA", alter: func(_ *nativeAckRetryOwnerFixture, g *parsedPacketGroup) {
			packet := reraceTcpPacket(g.ipPath, tcpFlagAck|tcpFlagPsh, 100, 500, []byte("native-data"))
			path, payload, err := ParseIpPathWithPayload(packet)
			if err != nil || len(payload) == 0 || protocolClassPureTcpControl(packet) {
				t.Fatal("mixed control lacks genuine parsed budgeted DATA")
			}
			g.packets = append(g.packets, parsedPacket{packet: packet, ipPath: path, payload: payload})
		}},
		{name: "payload", alter: func(_ *nativeAckRetryOwnerFixture, g *parsedPacketGroup) { g.packets[0].payload = []byte{1} }},
		{name: "SYN", alter: func(_ *nativeAckRetryOwnerFixture, g *parsedPacketGroup) { g.ipPath.Syn = true }},
		{name: "FIN", alter: func(_ *nativeAckRetryOwnerFixture, g *parsedPacketGroup) { g.ipPath.Fin = true }},
		{name: "RST", alter: func(_ *nativeAckRetryOwnerFixture, g *parsedPacketGroup) { g.ipPath.Rst = true }},
		{name: "later zero", alter: func(_ *nativeAckRetryOwnerFixture, g *parsedPacketGroup) { g.ipPath.TcpWindowSize = 0 }},
		{name: "older ACK", alter: func(_ *nativeAckRetryOwnerFixture, g *parsedPacketGroup) { g.ipPath.AckSequenceNumber-- }},
		{name: "different SEQ", alter: func(_ *nativeAckRetryOwnerFixture, g *parsedPacketGroup) { g.ipPath.SequenceNumber++ }},
		{name: "prior positive", alter: func(f *nativeAckRetryOwnerFixture, _ *parsedPacketGroup) { f.update.sequenceAdmissionWindow = 1 }},
		{name: "unestablished", alter: func(f *nativeAckRetryOwnerFixture, _ *parsedPacketGroup) { f.update.receivedInbound.Store(false) }},
	} {
		f := newNativeAckRetryOwnerFixture(t, 4)
		group := f.prepare(t, f.packet)
		prior := group.nativeAckRetry
		if prior == nil || !prior.current() {
			t.Fatalf("%s: missing positive premise", row.name)
		}
		group.nativeAckRetry = nil
		func() {
			f.update.stateLock.Lock()
			defer f.update.stateLock.Unlock()
			row.alter(f, group)
			group.prepareNativeAckRetryWithLock(f.update)
		}()
		if group.nativeAckRetry != nil || group.nativeAckRetrySelection != nil || group.nativeAckRetrySerial != 0 || prior.current() {
			t.Fatalf("%s: unsupported feedback acquired recovery or left stale retry current", row.name)
		}
	}
	for _, flags := range []byte{tcpFlagSyn, tcpFlagFin | tcpFlagAck, tcpFlagRst | tcpFlagAck} {
		f := newNativeAckRetryOwnerFixture(t, 4)
		prior := f.prepare(t, f.packet).nativeAckRetry
		packet := reraceTcpPacket(f.update.ipPath, flags, 100, 500, nil)
		path, _, err := ParseIpPathWithPayload(packet)
		if err != nil {
			t.Fatal(err)
		}
		f.update.noteConnectAttempt(path)
		if prior.current() {
			t.Fatalf("lifecycle flags=%d left old local feedback eligible before collapse", flags)
		}
	}
}

// An advancing frontier is useful even when the last admitted window was
// positive; ordering across sequence wrap retains TCP's half-range rule.
func TestNativeAdvancingAckRetryRetainsExactFeedbackAcrossWindowAndWrap(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	for _, version := range []int{4, 6} {
		for _, row := range []struct {
			priorAck uint32
			nextAck  uint32
			window   uint16
		}{
			{priorAck: 500, nextAck: 501, window: 4096},
			{priorAck: 500, nextAck: 501, window: 0},
			{priorAck: ^uint32(0) - 10, nextAck: 25, window: 4096},
		} {
			f := newNativeAckRetryOwnerFixture(t, version)
			f.update.sequenceAdmissionAck, f.update.sequenceAdmissionWindow = row.priorAck, row.window
			path, _, err := ParseIpPathWithPayload(f.packet)
			if err != nil {
				t.Fatal(err)
			}
			packet := reraceTcpPacket(path, tcpFlagAck, 100, row.nextAck, nil)
			group := f.prepare(t, packet)
			if group.nativeAckRetry == nil || !group.nativeAckRetry.current() {
				t.Fatalf("IPv%d prior=%d next=%d window=%d: advancing feedback lacks refusal ownership", version, row.priorAck, row.nextAck, row.window)
			}
			owner := f.retain(t, group)
			if !bytes.Equal(owner.packet, packet) {
				t.Fatal("advancing retry changed native bytes")
			}
		}
	}
}

// Preparation runs on every ordinary advancing ACK. Only an actual refused
// source admission may allocate the optional attempt or retain a packet.
func TestNativeAdvancingAckPreparationHasNoHeapAllocation(t *testing.T) {
	f := newNativeAckRetryOwnerFixture(t, 4)
	f.update.sequenceAdmissionWindow = 4096
	path, _, err := ParseIpPathWithPayload(f.packet)
	if err != nil {
		t.Fatal(err)
	}
	packet := reraceTcpPacket(path, tcpFlagAck, 100, 501, nil)
	path, payload, err := ParseIpPathWithPayload(packet)
	if err != nil {
		t.Fatal(err)
	}
	group := parsedPacketGroup{ipPath: path, packets: []parsedPacket{{packet: packet, ipPath: path, payload: payload}},
		collapseAdmission: tcpCollapseAdmission{update: f.update, epoch: f.update.sequenceAdmissionEpoch}}
	allocations := testing.AllocsPerRun(100, func() {
		f.update.stateLock.Lock()
		group.prepareNativeAckRetryWithLock(f.update)
		f.update.stateLock.Unlock()
	})
	if allocations != 0 || group.nativeAckRetry != nil || group.nativeAckRetrySelection == nil || group.nativeAckRetrySerial == 0 {
		t.Fatalf("ordinary advancing preparation: allocations=%g attempt=%p serial=%d", allocations, group.nativeAckRetry, group.nativeAckRetrySerial)
	}
}
