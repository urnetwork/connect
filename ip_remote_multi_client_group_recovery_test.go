// Public native admission controls hold the real source worker before Run,
// exposing queued ownership without manufacturing materialization or timeouts.
package connect

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"unsafe"

	"github.com/urnetwork/connect/protocol"
)

// Owns one actual client, selected channel and pre-Run barrier. Cleanup joins
// production before checking witnesses, including every early fatal path.
type groupDispositionQueueFixture struct {
	t                   *testing.T
	client              *Client
	cancel              context.CancelFunc
	parent              *RemoteUserNatMultiClient
	update              *multiClientChannelUpdate
	selected            *multiClientChannel
	peer                Id
	source              TransferPath
	route               Route
	transport           *sendClientTransport
	entered             chan struct{}
	keyEntered          chan struct{}
	encryptedKeyEntered chan struct{}
	release             chan struct{}
	releaseOnce         sync.Once
	started             atomic.Int64
	terminal            atomic.Int64
	witnesses           [][]byte
}

// A configuration callback changes only the specific boundary under test.
func newGroupDispositionQueueFixture(
	t *testing.T,
	budget *TransferMemoryBudget,
	configure func(*ClientSettings),
) *groupDispositionQueueFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f := &groupDispositionQueueFixture{t: t, cancel: cancel, peer: NewId(), source: SourceId(NewId()),
		route: make(Route, 256), entered: make(chan struct{}), keyEntered: make(chan struct{}),
		encryptedKeyEntered: make(chan struct{}), release: make(chan struct{})}
	settings := closeWaitClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	var keyOnce sync.Once
	settings.beforeClientKeyPublishForTest = func() {
		keyOnce.Do(func() { close(f.keyEntered) })
		<-ctx.Done()
	}
	var encryptedKeyOnce sync.Once
	settings.EncryptionSettings.beforeEncryptedKeyPublishForTest = func() {
		encryptedKeyOnce.Do(func() { close(f.encryptedKeyEntered) })
		<-ctx.Done()
	}
	settings.SendBufferSettings.ResendQueueBudget = budget
	settings.SendBufferSettings.ResendQueueRetainedByteAccounting = budget != nil
	settings.SendBufferSettings.PrewarmOpeningContract = false
	var enteredOnce sync.Once
	settings.SendBufferSettings.beforeRunSendSequenceForTest = func(id sendSequenceId) {
		if id.Destination == f.peer {
			enteredOnce.Do(func() { close(f.entered) })
			select {
			case <-f.release:
			case <-ctx.Done():
			}
		}
	}
	settings.SendBufferSettings.SendPackLifecycleObserver = func(event SendPackLifecycleObservation) {
		if event.DestinationId == f.peer {
			if event.Phase == SendPackLifecyclePhaseStarted {
				f.started.Add(1)
			}
			if event.Phase == SendPackLifecyclePhaseTerminal {
				f.terminal.Add(1)
			}
		}
	}
	if configure != nil {
		configure(settings)
	}
	f.client = NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	f.client.ContractManager().AddNoContractPeer(f.peer)
	f.transport = NewSendClientTransport(DestinationId(f.peer))
	f.client.RouteManager().UpdateTransport(f.transport, []Route{f.route})
	var closeParent func()
	f.parent, f.update, closeParent = groupTestParent(t, DisableSecurityPolicy())
	f.parent.settings.TcpCollapsePrevention = true
	f.parent.settings.TcpCollapseMaxHold = 0
	f.parent.settings.DialFailureRerace = false
	f.selected = newPacketTransferTestChannel()
	f.selected.ctx, f.selected.client = f.parent.ctx, f.client
	f.selected.args = &multiClientChannelArgs{Destination: RequireMultiHopId(f.peer)}
	f.update.client.Store(f.selected)
	t.Cleanup(func() {
		if err := f.closeClient(context.Background()); err != nil {
			t.Errorf("join queued group client: %v", err)
		}
		closeParent()
		for len(f.route) != 0 {
			MessagePoolReturn(<-f.route)
		}
		for _, witness := range f.witnesses {
			if witness != nil && !MessagePoolReturn(witness) {
				t.Error("queued original survived source cleanup")
			}
		}
	})
	return f
}

// Releases the one source barrier, also safe from joined fatal cleanup.
func (self *groupDispositionQueueFixture) unpark() {
	self.releaseOnce.Do(func() { close(self.release) })
}

// Fixture-owned barriers share the fixture lifetime, not the client's child
// context. Release every fixture barrier before joining its parked workers.
func (self *groupDispositionQueueFixture) closeClient(ctx context.Context) error {
	self.cancel()
	self.unpark()
	return self.client.CloseAndWait(ctx)
}

// Closing a child client cannot release a barrier owned by its parent fixture.
// The explicit fixture close must cancel that owner before its synchronous join.
func TestTcpGroupRecoveryOwnedPublisherClose(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newGroupDispositionQueueFixture(t, nil, nil)
		synctest.Wait()
		select {
		case <-f.keyEntered:
		default:
			t.Fatal("owned key publisher never reached its fixture barrier")
		}
		f.client.Close()
		joined := make(chan error, 1)
		go func() { joined <- f.client.CloseAndWait(t.Context()) }()
		synctest.Wait()
		select {
		case err := <-joined:
			t.Fatalf("client child close bypassed fixture-owned publisher: %v", err)
		default:
		}
		if err := f.closeClient(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := <-joined; err != nil {
			t.Fatal(err)
		}
	})
}

// The native batch takes every input, including refused members. Retained
// witnesses never substitute for, or prematurely release, production owners.
func (self *groupDispositionQueueFixture) offer(templates ...[]byte) int {
	return self.offerTimeout(0, templates...)
}

// Uses the public timeout unchanged, including indefinite caller backpressure.
func (self *groupDispositionQueueFixture) offerTimeout(timeout time.Duration, templates ...[]byte) int {
	packets := make([][]byte, len(templates))
	for index, template := range templates {
		packets[index] = MessagePoolCopy(template)
		self.witnesses = append(self.witnesses, MessagePoolShareReadOnly(packets[index]))
	}
	return self.parent.SendPacketBatch(self.source, protocol.ProvideMode_Network, packets, timeout)
}

// Quiescence proves the barrier was reached, rather than waiting for a timer
// if a changed precondition diverts the source down a different path.
func (self *groupDispositionQueueFixture) requireParked() {
	self.t.Helper()
	synctest.Wait()
	select {
	case <-self.entered:
	default:
		self.t.Fatal("source never reached its pre-Run barrier")
	}
	if len(self.route) != 0 {
		self.t.Fatal("parked source published a physical Pack")
	}
}

// Applies peer acknowledgements to the independently decoded physical wire.
// No callback result, cursor, or queue entry is treated as a physical write.
func (self *groupDispositionQueueFixture) acknowledgeAll(templates ...[]byte) {
	self.t.Helper()
	synctest.Wait()
	seen := 0
	for len(self.route) != 0 {
		wire := <-self.route
		pack := decodeSendPackLifecycleWirePack(self.t, wire)
		if pack.Nack {
			MessagePoolReturn(wire)
			self.t.Fatal("TCP group lost reliable retention")
		}
		for _, frame := range pack.Frames {
			if seen >= len(templates) || !bytes.Equal(frame.MessageBytes, templates[seen]) {
				MessagePoolReturn(wire)
				self.t.Fatalf("physical group frame %d differs from source order", seen)
			}
			seen++
		}
		accepted := self.client.sendBuffer.Ack(self.peer, &protocol.Ack{MessageId: pack.MessageId, SequenceId: pack.SequenceId}, 0)
		MessagePoolReturn(wire)
		if !accepted {
			self.t.Fatal("actual peer acknowledgement refused")
		}
	}
	if seen != len(templates) {
		self.t.Fatalf("physically wrote %d originals, want %d", seen, len(templates))
	}
	synctest.Wait()
}

// Constructs a full native TCP control with the requested state and checksum.
func groupDispositionControlPacket(sequence, ack uint32, window uint16, flags byte) []byte {
	return groupRecoveryPacket(sequence, ack, window, flags, nil)
}

// Synthetic data uses exactly the same checksummed native path as controls.
func groupRecoveryPacket(sequence, ack uint32, window uint16, flags byte, payload []byte) []byte {
	path := &IpPath{Version: 4, Protocol: IpProtocolTcp,
		SourceIp: net.IPv4(192, 0, 2, 91), DestinationIp: net.IPv4(198, 51, 100, 92),
		SourcePort: 44000, DestinationPort: 443}
	packet := ipOosTcpPacketSequence(path, flags, sequence, payload)
	tcp := packet[Ipv4HeaderSizeWithoutExtensions:]
	binary.BigEndian.PutUint32(tcp[8:12], ack)
	binary.BigEndian.PutUint16(tcp[14:16], window)
	binary.BigEndian.PutUint16(tcp[16:18], 0)
	binary.BigEndian.PutUint16(tcp[16:18], ipPathTransportChecksum(path, ipProtocolNumberTcp, tcp))
	return packet
}

// Only immutable metadata is needed for the public collapse predicate.
func groupRecoveryParsed(t *testing.T, packet []byte) *parsedPacket {
	t.Helper()
	path, payload, err := ParseIpPathWithPayload(packet)
	if err != nil {
		t.Fatal(err)
	}
	return &parsedPacket{packet: packet, ipPath: path, payload: payload}
}

// Local memory refusal retires its original outstanding accounting without
// creating provider error, RTT/ACK credit, or a future false stall verdict.
func (self *groupDispositionQueueFixture) requireLocalRefusalHealthy() {
	self.t.Helper()
	if _, err := self.selected.WindowStats(); err != nil {
		self.t.Fatalf("local capacity refusal poisoned provider health: %v", err)
	}
	self.selected.stateLock.Lock()
	count, bytes := self.selected.packetStats.sendNackCount, self.selected.packetStats.sendNackByteCount
	pending := self.selected.pendingSendTime
	self.selected.stateLock.Unlock()
	if count != 0 || bytes != 0 || !pending.IsZero() || self.selected.sendStalled(time.Nanosecond) {
		self.t.Fatalf("local refusal left false outstanding/stall state: count=%d bytes=%d pending=%v", count, bytes, pending)
	}
}

// A enqueues and pauses, B enqueues and returns, then A returns. The real
// source FIFO determines controls; producer return order supplies no proof.
func checkGroupRecoveryReturnOrder(t *testing.T, ackChange bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		var calls atomic.Int64
		var releaseOnce sync.Once
		unpark := func() { releaseOnce.Do(func() { close(release) }) }
		budget := NewTransferMemoryBudget(kib(64))
		f := newGroupDispositionQueueFixture(t, budget, func(settings *ClientSettings) {
			settings.SendBufferSettings.beforeGroupAdmissionForTest = func() {
				if calls.Add(1) == 1 {
					close(entered)
					<-release
				}
			}
		})
		t.Cleanup(unpark)
		first := groupDispositionControlPacket(100, 500, 0, tcpFlagAck)
		second := groupDispositionControlPacket(100, 500, 64, tcpFlagAck)
		if ackChange {
			second = groupDispositionControlPacket(100, 600, 0, tcpFlagAck)
		}
		result := make(chan int, 1)
		go func() { result <- f.offer(first) }()
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("first public offer never reached irreversible enqueue")
		}
		if f.offer(second) != 1 {
			t.Fatal("second control refused while first return was held")
		}
		unpark()
		if <-result != 1 {
			t.Fatal("first actual queue admission did not return success")
		}
		f.requireParked()
		if budget.UsedByteCount() != 0 {
			t.Fatal("prequeue descriptor took retained budget")
		}
		if !f.update.canUpdateSequenceForClient(groupRecoveryParsed(t, first), f.selected) ||
			!f.update.canUpdateSequenceForClient(groupRecoveryParsed(t, second), f.selected) {
			t.Fatal("conflicting predequeue controls invented a FIFO winner")
		}
		f.unpark()
		f.acknowledgeAll(first, second)
		if f.offer(second) != 0 || !f.update.canUpdateSequenceForClient(groupRecoveryParsed(t, first), f.selected) {
			t.Fatal("source FIFO did not preserve the latest control after inverted public returns")
		}
		if f.update.sourceRstSequence() != groupRecoveryParsed(t, second).ipPath.AckSequenceNumber || budget.UsedByteCount() != 0 {
			t.Fatal("latest admission ACK or released memory differed after source completion")
		}
	})
}

func TestTcpGroupRecoveryInvertedWindowReturnsFollowSource(t *testing.T) {
	checkGroupRecoveryReturnOrder(t, false)
}

func TestTcpGroupRecoveryInvertedAckReturnsFollowSource(t *testing.T) {
	checkGroupRecoveryReturnOrder(t, true)
}

// Accounting occurs at accepted admission while Run is parked. Later real
// retention must not add a second ACK sample or renew a positive hold.
func TestTcpGroupRecoveryAdmissionMetricsAndHoldPrecedeRetention(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newGroupDispositionQueueFixture(t, NewTransferMemoryBudget(kib(64)), nil)
		f.parent.settings.TcpCollapseMaxHold = 100 * time.Millisecond
		first := groupDispositionControlPacket(100, 500, 64, tcpFlagAck)
		second := groupDispositionControlPacket(100, 600, 64, tcpFlagAck)
		at := time.Now()
		if f.offer(first, second) != 2 {
			t.Fatal("two-ACK group refused")
		}
		f.requireParked()
		if f.update.ackPerformance.totalBytes != 100 || !f.update.sequenceTime.Equal(at) ||
			f.update.sourceRstSequence() != 600 || f.update.sequenceCovered {
			t.Fatal("accepted metrics/ACK clock waited for serialization")
		}
		// Advance the explicit hold deadline while the source stays at its
		// barrier; the clock, not scheduler luck, is the behavior under test.
		time.Sleep(100 * time.Millisecond)
		if !f.parent.canSendPacket(groupRecoveryParsed(t, second), f.update, f.selected) {
			t.Fatal("positive hold did not use successful admission time")
		}
		f.unpark()
		f.acknowledgeAll(first, second)
		if f.update.ackPerformance.totalBytes != 100 || !f.update.sequenceTime.Equal(at) ||
			!f.parent.canSendPacket(groupRecoveryParsed(t, second), f.update, f.selected) {
			t.Fatal("retention changed admission accounting or renewed the hold")
		}
	})
}

// The caller owns these originals until a true final-group send. The witness
// is independent and reconciled by the fixture after the actual source joins.
func groupRecoveryCallerGroup(t *testing.T, f *groupDispositionQueueFixture, templates ...[]byte) *parsedPacketGroup {
	t.Helper()
	group := &parsedPacketGroup{packets: make([]parsedPacket, len(templates))}
	for index, template := range templates {
		packet := MessagePoolCopy(template)
		f.witnesses = append(f.witnesses, MessagePoolShareReadOnly(packet))
		group.packets[index] = *groupRecoveryParsed(t, packet)
		group.byteCount += ByteCount(len(packet))
	}
	group.ipPath = group.packets[0].ipPath
	group.prepareCollapseAdmission(f.update)
	return group
}

// Refusal leaves this descriptor reusable. On its second attempt the source
// ACK completes before the held public return, so its lazy charge must remain.
func TestTcpGroupRecoveryRefusedDescriptorReuseKeepsChargeUntilReturn(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		unpark := func() { releaseOnce.Do(func() { close(release) }) }
		budget := NewTransferMemoryBudget(kib(64))
		f := newGroupDispositionQueueFixture(t, budget, func(settings *ClientSettings) {
			settings.SendBufferSettings.SequenceBufferSize = 0
			settings.SendBufferSettings.afterGroupAdmissionForTest = func(sendGroupAdmissionTarget) {
				close(entered)
				<-release
			}
		})
		t.Cleanup(unpark)
		template := groupDispositionControlPacket(100, 500, 64, tcpFlagAck)
		group := groupRecoveryCallerGroup(t, f, template)
		callerOwns := true
		t.Cleanup(func() {
			if callerOwns {
				MessagePoolReturn(group.packets[0].packet)
			}
		})
		if accepted, err := f.selected.SendGroupDetailedWithAck(group, 0, true); accepted || err != nil {
			t.Fatalf("readerless first offer accepted=%t err=%v", accepted, err)
		}
		f.requireParked()
		if budget.UsedByteCount() != 0 || group.collapseOwner != nil {
			t.Fatal("unbuffered refusal created a retained owner")
		}
		f.unpark()
		result := make(chan bool, 1)
		go func() { result <- f.selected.SendGroupWithAck(group, -1, true) }()
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("second offer did not reach the held successful handoff")
		}
		callerOwns = false
		f.acknowledgeAll(template)
		if group.collapseOwner == nil || group.completionFlags.Load()&groupCompletionTerminal == 0 ||
			group.completionFlags.Load()&groupCompletionReturned != 0 || budget.UsedByteCount() != group.collapseOwner.budgetBytes ||
			budget.UsedByteCount() == 0 {
			t.Fatal("callback-before-return reused a refusal flag or released live descriptor charge")
		}
		unpark()
		if !<-result {
			t.Fatal("physically acknowledged second offer did not return success")
		}
		if budget.UsedByteCount() != 0 || group.collapseSource != nil {
			t.Fatal("complete public return retained a budget or source lifetime")
		}
	})
}

// The fit check succeeds, then a barrier removes capacity before the atomic
// item+owner reservation. The source must dispose all raw suffix members now.
func TestTcpGroupRecoveryJointReservationLossDisposesRawRemainder(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		budget := NewTransferMemoryBudget(kib(64))
		var reservations atomic.Int64
		f := newGroupDispositionQueueFixture(t, budget, func(settings *ClientSettings) {
			settings.SendBufferSettings.beforeGroupRetainForTest = func() {
				if reservations.Add(1) == 1 {
					budget.SetTotalByteCount(0)
				}
			}
		})
		templates := [][]byte{
			groupRecoveryPacket(100, 500, 64, tcpFlagAck, make([]byte, 10)),
			groupRecoveryPacket(110, 500, 64, tcpFlagAck, make([]byte, 10)),
			groupRecoveryPacket(120, 500, 64, tcpFlagAck, make([]byte, 10)),
		}
		if f.offer(templates...) != 3 {
			t.Fatal("raw three-member group refused")
		}
		f.requireParked()
		group := f.update.sequenceClaims
		if group == nil || group.collapseOwner != nil || budget.UsedByteCount() != 0 {
			t.Fatal("raw admission allocated range memory")
		}
		f.unpark()
		synctest.Wait()
		if reservations.Load() != 1 || f.terminal.Load() != 1 || group.collapseOwner != nil ||
			budget.UsedByteCount() != 0 || len(f.route) != 0 || f.update.sequenceClaims != nil || f.update.sequenceCovered {
			t.Fatal("failed joint reservation requeued a suffix, retained metadata, or fabricated coverage")
		}
		for _, template := range templates {
			if !f.update.canUpdateSequenceForClient(groupRecoveryParsed(t, template), f.selected) {
				t.Fatal("an unfunded original stayed collapsed")
			}
		}
		f.requireLocalRefusalHealthy()
	})
}

// Replaying a pre-SYN range into its new cohort could falsely prove bytes
// after the SYN. Both raw and durable proof must start at the last reset.
func TestTcpGroupRecoveryOverlappingNewSynKeepsItsOwnCohort(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newGroupDispositionQueueFixture(t, nil, nil)
		old := groupRecoveryPacket(100, 500, 64, tcpFlagAck, make([]byte, 500))
		syn := groupDispositionControlPacket(500, 500, 64, tcpFlagSyn)
		unwritten := groupRecoveryPacket(501, 500, 64, tcpFlagAck, make([]byte, 20))
		if f.offer(old, syn) != 2 {
			t.Fatal("mixed generation group refused")
		}
		f.requireParked()
		if !f.update.canUpdateSequenceForClient(groupRecoveryParsed(t, unwritten), f.selected) {
			t.Fatal("unknown predequeue reset donated prior-generation raw coverage")
		}
		if f.offer(syn) != 0 {
			t.Fatal("identical pending SYN retransmission was not gated")
		}
		f.unpark()
		f.acknowledgeAll(old, syn)
		if !f.update.synGenerationSeen || f.update.synGenerationNumber != 500 ||
			!f.update.canUpdateSequenceForClient(groupRecoveryParsed(t, unwritten), f.selected) || f.offer(syn) != 0 {
			t.Fatal("materialization lost the SYN cohort boundary or same-SYN gate")
		}
		fresh := groupDispositionControlPacket(900, 500, 64, tcpFlagSyn)
		if f.offer(fresh) != 1 {
			t.Fatal("different SYN did not establish a new source cohort")
		}
		f.acknowledgeAll(fresh)
		if f.update.synGenerationNumber != 900 || f.offer(fresh) != 0 {
			t.Fatal("new cohort failed to retain its own identical-SYN gate")
		}
	})
}

// An unresolved different SYN may intervene after a matching original. Only
// an identical-only pending cohort can suppress the next public SYN offer.
func testTcpGroupRecoveryPendingSynConflict(t *testing.T, grouped bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newGroupDispositionQueueFixture(t, nil, nil)
		first := groupDispositionControlPacket(100, 0, 64, tcpFlagSyn)
		other := groupDispositionControlPacket(200, 0, 64, tcpFlagSyn)
		if grouped {
			if f.offer(first, other) != 2 {
				t.Fatal("competing grouped SYN controls refused")
			}
		} else {
			if f.offer(first) != 1 || f.offer(first) != 0 {
				t.Fatal("identical-only pending SYN failed its public gate")
			}
			if f.offer(other) != 1 {
				t.Fatal("different queued SYN was incorrectly suppressed")
			}
		}
		f.requireParked()
		if f.offer(first) != 1 {
			t.Fatal("an earlier matching SYN suppressed the new cohort after a competing SYN")
		}
		f.unpark()
		f.acknowledgeAll(first, other, first)
		if f.update.synGenerationNumber != 100 || f.offer(first) != 0 {
			t.Fatal("source-ordered final SYN did not own its durable duplicate gate")
		}
	})
}

// Separate physical offers still have no source-owned order before Run.
func TestTcpGroupRecoveryPendingSynConflictAcrossOffers(t *testing.T) {
	testTcpGroupRecoveryPendingSynConflict(t, false)
}

// Group membership alone cannot make its earlier matching SYN the last reset.
func TestTcpGroupRecoveryPendingSynConflictWithinGroup(t *testing.T) {
	testTcpGroupRecoveryPendingSynConflict(t, true)
}

// Teardown cannot reapply an older RST after a
// later accepted ACK. Its reset sequence is available before serialization.
func TestTcpGroupRecoveryTeardownKeepsLatestAcceptedAck(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newGroupDispositionQueueFixture(t, nil, nil)
		first := groupRecoveryCallerGroup(t, f, groupDispositionControlPacket(100, 700, 64, tcpFlagRst|tcpFlagAck))
		second := groupRecoveryCallerGroup(t, f, groupDispositionControlPacket(100, 800, 64, tcpFlagAck))
		for _, group := range []*parsedPacketGroup{first, second} {
			if !f.selected.SendGroupWithAck(group, 0, true) {
				MessagePoolReturn(group.packets[0].packet)
				t.Fatal("direct successful control admission refused")
			}
		}
		f.requireParked()
		if f.update.sourceRstSequence() != 800 {
			t.Fatal("synthesized source reset waited for accepted ACK serialization")
		}
		if err := f.closeClient(t.Context()); err != nil {
			t.Fatal(err)
		}
		if f.update.sourceRstSequence() != 800 || f.terminal.Load() != 2 || len(f.route) != 0 {
			t.Fatal("old raw RST disposal overwrote the later accepted ACK")
		}
	})
}

// Cancel exactly after the real source receives its first queued Pack and
// before it can observe control metadata. This forces the ready-queue versus
// canceled-context case without asking a select to choose a particular branch.
func TestTcpGroupRecoveryCanceledDequeueKeepsLatestAcceptedAck(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		dequeued := make(chan struct{})
		var cancelOnce sync.Once
		budget := NewTransferMemoryBudget(kib(64))
		f := newGroupDispositionQueueFixture(t, budget, func(settings *ClientSettings) {
			settings.SendBufferSettings.beforeGroupDequeueForTest = func(source *SendSequence) {
				cancelOnce.Do(func() { source.Cancel(); close(dequeued) })
			}
		})
		first := groupRecoveryCallerGroup(t, f, groupDispositionControlPacket(100, 700, 64, tcpFlagRst|tcpFlagAck))
		second := groupRecoveryCallerGroup(t, f, groupDispositionControlPacket(100, 800, 64, tcpFlagAck))
		for _, group := range []*parsedPacketGroup{first, second} {
			if !f.selected.SendGroupWithAck(group, 0, true) {
				MessagePoolReturn(group.packets[0].packet)
				t.Fatal("control did not reach its actual queued admission")
			}
		}
		f.requireParked()
		source := first.collapseSource
		if source == nil || f.update.sourceRstSequence() != 800 {
			t.Fatal("latest successful accepted ACK was not recorded before dequeue")
		}
		f.unpark()
		synctest.Wait()
		select {
		case <-dequeued:
		default:
			t.Fatal("source never reached the forced canceled dequeue boundary")
		}
		select {
		case <-source.done:
		default:
			t.Fatal("canceled source did not finish its real terminal cleanup")
		}
		if f.update.sourceRstSequence() != 800 || f.update.IsDone() || f.update.sequenceControlOrder != 0 ||
			f.terminal.Load() != 2 || len(f.route) != 0 || budget.UsedByteCount() != 0 || f.update.sequenceClaims != nil {
			t.Fatal("canceled dequeue replayed a stale RST or retained source ownership")
		}
		for _, group := range []*parsedPacketGroup{first, second} {
			if group.completionFlags.Load()&groupCompletionDequeued != 0 || group.collapseSource != nil {
				t.Fatal("canceled source acquired a control ordinal or survived terminal return")
			}
		}
	})
}

// Required mode has an independent certificate publisher even before any
// application offer. Its actual retained control is not TCP range metadata.
func TestTcpGroupRecoveryEncryptedKeyOwnsIndependentBudget(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		var enteredOnce, releaseOnce sync.Once
		unpark := func() { releaseOnce.Do(func() { close(release) }) }
		budget := NewTransferMemoryBudget(kib(64))
		f := newGroupDispositionQueueFixture(t, budget, func(settings *ClientSettings) {
			settings.EncryptionSettings.Mode = EncryptionModeRequired
			settings.EncryptionSettings.beforeEncryptedKeyPublishForTest = func() {
				enteredOnce.Do(func() { close(entered) })
				<-release
			}
		})
		t.Cleanup(unpark)
		controlRoute := make(Route, 8)
		f.client.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(ControlId)), []Route{controlRoute})
		t.Cleanup(func() {
			unpark()
			if err := f.closeClient(context.Background()); err != nil {
				t.Error(err)
			}
			for len(controlRoute) != 0 {
				MessagePoolReturn(<-controlRoute)
			}
		})
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("encrypted-key publisher did not reach its owned barrier")
		}
		if budget.UsedByteCount() != 0 || f.started.Load() != 0 {
			t.Fatal("unreleased publisher or nonexistent application already retained a send")
		}
		unpark()
		synctest.Wait()
		if len(controlRoute) != 1 || budget.UsedByteCount() == 0 || f.started.Load() != 0 ||
			f.update.sequenceClaims != nil || !f.update.sequenceTime.IsZero() {
			t.Fatal("independent encrypted-key send did not exclusively own its retained budget")
		}
		wire := <-controlRoute
		pack := decodeSendPackLifecycleWirePack(t, wire)
		if pack.Nack || len(pack.Frames) != 1 || pack.Frames[0].MessageType != protocol.MessageType_TransferEncryptedKey {
			MessagePoolReturn(wire)
			t.Fatal("independent retained wire was not the reliable encrypted-key publication")
		}
		accepted := f.client.sendBuffer.Ack(ControlId, &protocol.Ack{MessageId: pack.MessageId, SequenceId: pack.SequenceId}, 0)
		MessagePoolReturn(wire)
		if !accepted {
			t.Fatal("actual control peer acknowledgement refused")
		}
		synctest.Wait()
		if budget.UsedByteCount() != 0 || f.started.Load() != 0 {
			t.Fatal("independent control ACK failed to release its own retained budget")
		}
	})
}

// Both independent key publishers are held. A Required application wait owns
// no range metadata and leaves the whole budget available for its handshake.
func TestTcpGroupRecoveryRequiredWaitHasNoRangeReservation(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		entered := make(chan struct{})
		var once sync.Once
		budget := NewTransferMemoryBudget(kib(64))
		f := newGroupDispositionQueueFixture(t, budget, func(settings *ClientSettings) {
			settings.EncryptionSettings.Mode = EncryptionModeRequired
			settings.SendBufferSettings.beforeRequiredEncryptionWaitForTest = func(sendSequenceId) {
				once.Do(func() { close(entered) })
			}
		})
		result := make(chan int, 1)
		go func() { result <- f.offerTimeout(-1, groupDispositionControlPacket(100, 500, 64, tcpFlagAck)) }()
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("application did not enter the actual Required gate")
		}
		select {
		case <-result:
			t.Fatal("Required application unexpectedly left the cipher wait")
		default:
		}
		select {
		case <-f.encryptedKeyEntered:
		default:
			t.Fatal("independent encrypted-key publisher was not held before its control send")
		}
		if budget.UsedByteCount() != 0 {
			t.Fatalf("pre-TLS application retained %d metadata bytes with independent publishers held", budget.UsedByteCount())
		}
		if f.update.sequenceClaims != nil || !f.update.sequenceTime.IsZero() {
			t.Fatal("pre-TLS application published an admission claim or clock")
		}
		if !budget.TryReserve(budget.TotalByteCount()) {
			t.Fatal("application metadata starved otherwise available handshake memory")
		}
		budget.Release(budget.TotalByteCount())
		f.client.Close()
		f.unpark()
		if <-result != 0 {
			t.Fatal("cancelled cipher wait transferred caller ownership")
		}
	})
}

// A real retained prefix pays for its lazy owner. Losing all remaining
// capacity disposes only the raw suffix and holds that charge until peer ACK.
func TestTcpGroupRecoveryUnfundedSuffixPreservesRetainedPrefix(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		first, release := make(chan struct{}), make(chan struct{})
		var firstOnce, releaseOnce sync.Once
		unpark := func() { releaseOnce.Do(func() { close(release) }) }
		budget := NewTransferMemoryBudget(kib(64))
		f := newGroupDispositionQueueFixture(t, budget, func(settings *ClientSettings) {
			settings.SendBufferSettings.afterInitialWriteQueuedForTest = func(_ sendSequenceId, number uint64) {
				if number == 0 {
					firstOnce.Do(func() { close(first); <-release })
				}
			}
		})
		t.Cleanup(unpark)
		templates := make([][]byte, 3)
		for index := range templates {
			templates[index] = groupRecoveryPacket(100+uint32(10*index), 500, 64, tcpFlagAck, make([]byte, 10))
		}
		if f.offer(templates...) != 3 {
			t.Fatal("three-member raw group refused")
		}
		f.requireParked()
		group := f.update.sequenceClaims
		f.unpark()
		synctest.Wait()
		select {
		case <-first:
		default:
			t.Fatal("source did not reach the physical prefix write")
		}
		if group == nil || group.collapseOwner == nil || len(f.route) != 1 {
			t.Fatal("physical prefix did not create one lazy range owner")
		}
		budget.SetTotalByteCount(budget.UsedByteCount())
		unpark()
		synctest.Wait()
		owner := group.collapseOwner
		if owner.remaining != 0 || owner.materialized != 3 || owner.disposed != 4 ||
			f.terminal.Load() != 0 || f.update.sequenceClaims != nil || budget.UsedByteCount() <= owner.budgetBytes {
			t.Fatal("unfunded suffix changed retained prefix ownership")
		}
		for index, template := range templates {
			if f.update.canUpdateSequenceForClient(groupRecoveryParsed(t, template), f.selected) != (index == 2) {
				t.Fatalf("original %d has incorrect prefix/suffix proof", index)
			}
		}
		f.acknowledgeAll(templates[:2]...)
		if budget.UsedByteCount() != 0 || f.terminal.Load() != 1 {
			t.Fatal("actual prefix ACK did not settle the mixed group")
		}
		f.requireLocalRefusalHealthy()
		budget.SetTotalByteCount(kib(64))
		// Budget retuning notifies Run; it does not synchronously republish
		// the source's zero-timeout admission gate.
		synctest.Wait()
		if f.offer(templates[2]) != 1 {
			t.Fatal("identical suffix retry stayed collapsed")
		}
		f.acknowledgeAll(templates[2])
	})
}

// Larger configured groups retain their exact bitmap only at first physical
// retention, then release it after the original group's final peer ACK.
func TestTcpGroupRecoveryConfiguredOverflowIsLazyAndCharged(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		budget := NewTransferMemoryBudget(mib(8))
		f := newGroupDispositionQueueFixture(t, budget, nil)
		f.parent.settings.PacketGroupMaxPacketCount = 129
		templates := make([][]byte, 129)
		for index := range templates {
			templates[index] = groupRecoveryPacket(100+uint32(10*index), 500, 64, tcpFlagAck, make([]byte, 10))
		}
		if f.offer(templates...) != len(templates) {
			t.Fatal("configured logical group was narrowed")
		}
		f.requireParked()
		group := f.update.sequenceClaims
		if group == nil || len(group.packets) != 129 || group.collapseOwner != nil || budget.UsedByteCount() != 0 {
			t.Fatal("overflow bitmap or range owner existed before retention")
		}
		f.unpark()
		synctest.Wait()
		if group.collapseOwner == nil || len(group.collapseOwner.stateWords) != 4 ||
			budget.UsedByteCount() <= group.collapseOwner.budgetBytes {
			t.Fatal("retention failed to charge exact overflow and item ownership")
		}
		owner := group.collapseOwner
		wantOwnerBytes := (ByteCount(unsafe.Sizeof(*owner))+63)/64*64 + ByteCount(cap(owner.stateWords))*8
		if owner.budgetBytes != wantOwnerBytes {
			t.Fatalf("lazy owner charge=%d want=%d", owner.budgetBytes, wantOwnerBytes)
		}
		f.acknowledgeAll(templates...)
		if budget.UsedByteCount() != 0 || f.terminal.Load() != 1 || f.update.sequenceClaims != nil {
			t.Fatal("configured group did not release its original lifetime")
		}
		for _, index := range []int{63, 64, 127, 128} {
			if f.offer(templates[index]) != 0 {
				t.Fatalf("materialized bitmap member %d lost its public duplicate gate", index)
			}
		}
		if f.started.Load() != 1 || len(f.route) != 0 {
			t.Fatal("overflow duplicate checks created an extra lifecycle or wire")
		}
	})
}

// The second chunk passes scheduler fit, then loses its actual reservation.
// Even an already-ACKed prefix stays collapsed while that exact suffix retries.
func TestTcpGroupRecoverySecondReservationLossKeepsAckedPrefix(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		first, release := make(chan struct{}), make(chan struct{})
		var firstOnce, releaseOnce sync.Once
		unpark := func() { releaseOnce.Do(func() { close(release) }) }
		capacityEntered, capacityRelease := make(chan struct{}), make(chan struct{})
		var capacityOnce, capacityReleaseOnce sync.Once
		unparkCapacity := func() { capacityReleaseOnce.Do(func() { close(capacityRelease) }) }
		budget := NewTransferMemoryBudget(kib(64))
		var reservations, appliedAcks atomic.Int64
		f := newGroupDispositionQueueFixture(t, budget, func(settings *ClientSettings) {
			settings.SendBufferSettings.beforeResendCapacityWaitForTest = func(sendSequenceId) {
				if reservations.Load() == 2 {
					capacityOnce.Do(func() { close(capacityEntered); <-capacityRelease })
				}
			}
			settings.SendBufferSettings.afterAckSendItemForTest = func(_ sendSequenceId, sequence uint64) {
				if sequence == 0 {
					appliedAcks.Add(1)
				}
			}
			settings.SendBufferSettings.afterInitialWriteQueuedForTest = func(_ sendSequenceId, number uint64) {
				if number == 0 {
					firstOnce.Do(func() { close(first); <-release })
				}
			}
			settings.SendBufferSettings.beforeGroupRetainForTest = func() {
				if reservations.Add(1) == 2 {
					if appliedAcks.Load() != 1 {
						t.Error("prefix peer ACK was not applied before the second reservation")
					}
					budget.SetTotalByteCount(budget.UsedByteCount())
				}
			}
		})
		t.Cleanup(unpark)
		t.Cleanup(unparkCapacity)
		templates := make([][]byte, 3)
		for index := range templates {
			templates[index] = groupRecoveryPacket(100+uint32(10*index), 500, 64, tcpFlagAck, make([]byte, 10))
		}
		if f.offer(templates...) != 3 {
			t.Fatal("three-member group refused")
		}
		f.requireParked()
		group := f.update.sequenceClaims
		source := group.collapseSource
		f.unpark()
		synctest.Wait()
		select {
		case <-first:
		default:
			t.Fatal("physical prefix barrier was not reached")
		}
		f.acknowledgeAll(templates[:2]...)
		unpark()
		synctest.Wait()
		if reservations.Load() != 2 || f.terminal.Load() != 1 || budget.UsedByteCount() != 0 ||
			group.collapseOwner.materialized != 3 || group.collapseOwner.disposed != 4 || len(f.route) != 0 {
			t.Fatal("actual second reservation failure did not preserve only its ACKed prefix")
		}
		select {
		case <-capacityEntered:
		default:
			t.Fatal("source did not publish its closed capacity gate after the local refusal")
		}
		if source == nil || source.ctx.Err() != nil || !source.resendCapacityUnavailable.Load() ||
			!f.update.canUpdateSequenceForClient(groupRecoveryParsed(t, templates[2]), f.selected) {
			t.Fatal("refused suffix lost its live source or remained covered instead of capacity-gated")
		}
		f.requireLocalRefusalHealthy()
		for _, prefix := range templates[:2] {
			if f.offer(prefix) != 0 {
				t.Fatal("second-chunk memory refusal erased public prefix suppression")
			}
		}
		budget.SetTotalByteCount(kib(64))
		// A retune wakes Run but cannot synchronously change its published
		// gate while that worker is deliberately held before the next loop.
		if f.offer(templates[2]) != 0 || f.update.client.Load() != f.selected ||
			f.terminal.Load() != 2 || budget.UsedByteCount() != 0 || len(f.route) != 0 {
			t.Fatal("zero-timeout retry bypassed the held capacity publication or changed ownership")
		}
		f.requireLocalRefusalHealthy()
		unparkCapacity()
		synctest.Wait()
		if source.resendCapacityUnavailable.Load() || source.ctx.Err() != nil {
			t.Fatal("budget notification did not reopen the same source admission gate")
		}
		if f.offer(templates[2]) != 1 {
			t.Fatal("actual refused suffix could not retry through the same public provider")
		}
		f.acknowledgeAll(templates[2])
		if f.terminal.Load() != 3 || budget.UsedByteCount() != 0 {
			t.Fatal("identical suffix retry did not finish its independent ownership")
		}
	})
}

// An identical SYN inside a progressing group is not a generation reset.
// Its retained data prefix must survive before and after the source ACK.
func TestTcpGroupRecoveryDataBeforeSameSynKeepsRetainedPrefix(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newGroupDispositionQueueFixture(t, nil, nil)
		syn := groupDispositionControlPacket(500, 500, 64, tcpFlagSyn)
		if f.offer(syn) != 1 {
			t.Fatal("initial SYN refused")
		}
		f.requireParked()
		f.unpark()
		f.acknowledgeAll(syn)
		epoch := f.update.sequenceAdmissionEpoch
		f.update.receivedInbound.Store(true)
		data := groupRecoveryPacket(501, 500, 64, tcpFlagAck, make([]byte, 99))
		if f.offer(data, syn) != 2 {
			t.Fatal("new data with same-SYN control refused")
		}
		f.acknowledgeAll(data, syn)
		if f.update.sequenceAdmissionEpoch != epoch || !f.update.receivedInbound.Load() ||
			f.offer(data) != 0 || f.offer(syn) != 0 {
			t.Fatal("identical SYN reset its cohort or erased the retained data prefix")
		}
	})
}

// Only wholly local capacity trees excuse provider health. A real retained
// sibling or contract failure remains hard even beside a refused chunk.
func TestTcpGroupRecoveryCapacityProvenanceRequiresEveryCause(t *testing.T) {
	local := &sendGroupCapacityError{cause: ErrSendPackNotAdmitted}
	if !errors.Is(local, ErrSendPackNotAdmitted) || !sendGroupCapacityFailure(errors.Join(local, local)) {
		t.Fatal("local marker lost not-admitted identity or joined provenance")
	}
	if sendGroupCapacityFailure(errors.Join(local, errors.New("synthetic contract failure"))) ||
		sendGroupCapacityFailure(ErrSendPackNotAdmitted) {
		t.Fatal("local capacity marker excused an unqualified provider failure")
	}
}

// A provider can already own an older Transfer sequence when the flow moves
// to it. Source-order comparisons belong to that provider, not its predecessor.
func TestTcpGroupRecoveryProviderRebindAdoptsOlderSourceSequence(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		candidate := newGroupDispositionQueueFixture(t, nil, func(settings *ClientSettings) {
			settings.SendBufferSettings.SequenceBufferSize = 0
		})
		first := groupRecoveryPacket(100, 500, 64, tcpFlagAck, make([]byte, 10))
		if candidate.offer(first) != 0 {
			t.Fatal("readerless candidate unexpectedly admitted its warmup")
		}
		candidate.requireParked()
		active := newGroupDispositionQueueFixture(t, nil, nil)
		if active.offer(first) != 1 {
			t.Fatal("initial provider refused its first packet")
		}
		active.requireParked()
		active.unpark()
		active.acknowledgeAll(first)
		oldSourceId := active.update.sequenceSourceId
		candidate.unpark()
		synctest.Wait()
		active.update.client.Store(candidate.selected)
		next := groupRecoveryPacket(110, 600, 64, tcpFlagAck, make([]byte, 10))
		if active.offerTimeout(-1, next) != 1 {
			t.Fatal("rebound provider refused its established source")
		}
		candidate.acknowledgeAll(next)
		if !active.update.sequenceSourceId.LessThan(oldSourceId) ||
			active.update.sequenceClient != candidate.selected || active.update.sourceRstSequence() != 600 ||
			active.offer(next) != 0 {
			t.Fatal("prior provider's newer source stamp rejected the selected provider's exact proof")
		}
	})
}

// A real Transfer acknowledgement can finish both candidate attempts before
// the public offers return or any provider has been selected. The retained
// range owner then belongs to the existing race until promotion or flow close.
func testTcpGroupRecoveryAckBeforePromotion(t *testing.T, wide, abandon bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		var hold atomic.Bool
		release := make(chan struct{})
		var releaseOnce sync.Once
		unpark := func() { releaseOnce.Do(func() { close(release) }) }
		admitted := make(chan *parsedPacketGroup, 2)
		configure := func(settings *ClientSettings) {
			settings.SendBufferSettings.afterGroupAdmissionForTest = func(target sendGroupAdmissionTarget) {
				if hold.Load() {
					admitted <- target.(*parsedPacketGroup)
					<-release
				}
			}
		}
		budget := NewTransferMemoryBudget(kib(64))
		f := newGroupDispositionQueueFixture(t, budget, configure)
		var other *groupDispositionQueueFixture
		var otherBudget *TransferMemoryBudget
		if wide {
			otherBudget = NewTransferMemoryBudget(kib(64))
			other = newGroupDispositionQueueFixture(t, otherBudget, configure)
		}
		t.Cleanup(unpark)
		oldSyn := groupDispositionControlPacket(100, 500, 64, tcpFlagSyn)
		oldFin := groupDispositionControlPacket(101, 500, 64, tcpFlagFin|tcpFlagAck)
		if f.offer(oldSyn, oldFin) != 2 {
			t.Fatal("prior cohort failed its actual admission")
		}
		f.requireParked()
		f.unpark()
		f.acknowledgeAll(oldSyn, oldFin)
		if f.update.synGenerationNumber != 100 || !f.update.egressFinSeen {
			t.Fatal("prior admitted SYN/FIN cohort was not established")
		}
		f.update.receivedInbound.Store(true)
		f.update.client.Store(nil)
		f.parent.settings.MultiRaceSetOnResponseTimeout = time.Hour
		f.parent.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
			if wide {
				return []*multiClientChannel{f.selected, other.selected}
			}
			return []*multiClientChannel{f.selected}
		}
		hold.Store(true)
		syn := groupDispositionControlPacket(900, 500, 64, tcpFlagSyn)
		data := groupRecoveryPacket(901, 500, 64, tcpFlagAck, []byte("proof"))
		result := make(chan int, 1)
		go func() { result <- f.offerTimeout(-1, syn, data) }()
		synctest.Wait()
		count := 1
		if wide {
			count = 2
			other.requireParked()
			other.unpark()
		}
		if len(admitted) != count {
			t.Fatalf("held actual candidate admissions = %d, want %d", len(admitted), count)
		}
		groups := make([]*parsedPacketGroup, 0, count)
		for range count {
			groups = append(groups, <-admitted)
		}
		f.acknowledgeAll(syn, data)
		if wide {
			other.acknowledgeAll(syn, data)
		}
		for _, group := range groups {
			flags := group.completionFlags.Load()
			if flags&groupCompletionTerminal == 0 || flags&groupCompletionReturned != 0 ||
				flags&groupCompletionLinked == 0 || group.collapseOwner == nil || group.collapseOwner.budgetBytes == 0 ||
				group.collapseOwner.remaining != 0 || group.collapseSource == nil ||
				group.collapseOwner.budget.UsedByteCount() != group.collapseOwner.budgetBytes {
				t.Fatal("acknowledged unselected candidate lost its charged promotion metadata")
			}
		}
		if f.update.client.Load() != nil || f.update.synGenerationNumber != 100 || !f.update.egressFinSeen ||
			budget.UsedByteCount() == 0 || wide && otherBudget.UsedByteCount() == 0 {
			t.Fatal("candidate completion activated controls or refunded pending promotion")
		}
		unpark()
		if <-result != 2 {
			t.Fatal("physically acknowledged candidate admission did not return success")
		}
		synctest.Wait()
		if wide {
			if f.update.client.Load() != nil || f.update.race == nil {
				t.Fatal("unanswered SYN race selected a provider before response evidence")
			}
			for _, group := range groups {
				minimumGraph := ByteCount(unsafe.Sizeof(parsedPacketGroup{})) +
					ByteCount(cap(group.packets))*ByteCount(unsafe.Sizeof(parsedPacket{})) +
					ByteCount(len(group.packets))*ByteCount(unsafe.Sizeof(IpPath{}))
				if group.completionFlags.Load()&groupCompletionReturned == 0 || group.collapseOwner.budgetBytes < minimumGraph ||
					group.collapseSource != nil {
					t.Fatal("returned terminal race candidate released its promotion charge")
				}
				for _, packet := range group.packets {
					if packet.packet != nil || packet.payload != nil {
						t.Fatal("terminal candidate retained an original pool root after its offer returned")
					}
				}
			}
			if abandon {
				f.update.Close()
			} else {
				f.update.stateLock.Lock()
				f.update.race.responseWindowElapsed = true
				f.update.stateLock.Unlock()
				f.parent.ip4PathUpdates = map[Ip4Path]*multiClientChannelUpdate{f.update.ipPath.ToIp4Path(): f.update}
				response := MessagePoolCopy(groupDispositionControlPacket(700, 901, 64, tcpFlagSyn|tcpFlagAck))
				f.witnesses = append(f.witnesses, MessagePoolShareReadOnly(response))
				control := tcpControlFromIpPath(groupRecoveryParsed(t, response).ipPath)
				f.parent.clientReceivePacketResolve(f.selected, TransferPath{}, protocol.ProvideMode_Network,
					f.update.ipPath, response, control)
				MessagePoolReturn(response)
				reset, ok := ipOosRst(f.update.ipPath)
				if !ok {
					t.Fatal("winner cleanup did not have a TCP reset control")
				}
				other.acknowledgeAll(reset)
			}
		}
		if budget.UsedByteCount() != 0 || wide && otherBudget.UsedByteCount() != 0 ||
			f.update.sequenceClaims != nil || f.update.sequenceReleasedClaims.Load() != nil {
			t.Fatal("race outcome did not release every terminal candidate charge/link")
		}
		for _, group := range groups {
			if group.collapseSource != nil || group.collapseOwner.budgetBytes != 0 {
				t.Fatal("race outcome retained a terminal source or range charge")
			}
		}
		if abandon {
			if f.update.synGenerationNumber != 100 || !f.update.egressFinSeen {
				t.Fatal("abandoned candidate reset the prior flow cohort")
			}
			return
		}
		if f.update.client.Load() != f.selected || f.update.synGenerationNumber != 900 ||
			f.update.egressFinSeen || f.update.ingressFinSeen || f.update.receivedInbound.Load() != wide {
			t.Fatal("selected acknowledged SYN did not reset the prior FIN/inbound cohort exactly once")
		}
		started := f.started.Load()
		if f.offer(syn, data) != 0 || f.started.Load() != started || len(f.route) != 0 {
			t.Fatal("promoted retained originals failed their public durable duplicate gate")
		}
	})
}

// The one-candidate path commits only after its successful public return.
func TestTcpGroupRecoveryAckBeforeSingleCandidatePromotion(t *testing.T) {
	testTcpGroupRecoveryAckBeforePromotion(t, false, false)
}

// A wide race may choose its winner after every source target has completed.
func TestTcpGroupRecoveryAckBeforeLateRaceWinnerPromotion(t *testing.T) {
	testTcpGroupRecoveryAckBeforePromotion(t, true, false)
}

// Flow close is the terminal owner when no candidate response ever arrives.
func TestTcpGroupRecoveryCompletedRaceCandidatesReleaseOnFlowClose(t *testing.T) {
	testTcpGroupRecoveryAckBeforePromotion(t, true, true)
}

// Either pause happens after actual candidate registration. A replacement
// race with the same provider cannot adopt this old producer's source claim.
func testTcpGroupRecoveryReplacedRaceProducer(t *testing.T, beforePrepare bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		var enterOnce, releaseOnce sync.Once
		unpark := func() { releaseOnce.Do(func() { close(release) }) }
		pause := func() { enterOnce.Do(func() { close(entered); <-release }) }
		budget := NewTransferMemoryBudget(kib(64))
		f := newGroupDispositionQueueFixture(t, budget, func(settings *ClientSettings) {
			if !beforePrepare {
				settings.SendBufferSettings.beforeGroupAdmissionForTest = pause
			}
		})
		t.Cleanup(unpark)
		if beforePrepare {
			f.selected.beforeGroupCompletionForTest = pause
		}
		f.update.client.Store(nil)
		f.parent.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
			return []*multiClientChannel{f.selected}
		}
		syn := groupDispositionControlPacket(900, 500, 64, tcpFlagSyn)
		result := make(chan int, 1)
		go func() { result <- f.offerTimeout(-1, syn) }()
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("registered producer did not reach its exact pause")
		}
		f.update.stateLock.Lock()
		oldRace := f.update.race
		f.update.clearRaceWithLock()
		f.update.initRaceWithLock()
		newRace := f.update.race
		newRace.clientStates[f.selected] = &multiClientChannelRaceClientState{sendTime: time.Now()}
		f.update.stateLock.Unlock()
		f.update.releaseCollapseClaims()
		if oldRace == nil || oldRace.collapseOrder == newRace.collapseOrder {
			t.Fatal("replacement race did not receive a distinct registration stamp")
		}
		unpark()
		if <-result != 1 {
			t.Fatal("old producer lost its actual transport admission")
		}
		f.requireParked()
		if f.update.race != newRace || f.update.client.Load() != nil || f.update.sequenceClaims != nil {
			t.Fatal("old registered producer attached to the replacement race")
		}
		f.unpark()
		f.acknowledgeAll(syn)
		f.update.stateLock.Lock()
		f.update.commitRaceClientWithLock(f.selected)
		f.update.stateLock.Unlock()
		f.update.releaseCollapseClaims()
		if budget.UsedByteCount() != 0 || f.update.sequenceCovered || f.update.synGenerationSeen || f.offer(syn) != 1 {
			t.Fatal("replacement winner inherited old producer proof or retained charge")
		}
		f.acknowledgeAll(syn)
	})
}

// Preparation must use the captured registration, not the then-current race.
func TestTcpGroupRecoveryReplacedRaceBeforePreparation(t *testing.T) {
	testTcpGroupRecoveryReplacedRaceProducer(t, true)
}

// The same identity boundary applies after irreversible channel enqueue.
func TestTcpGroupRecoveryReplacedRaceAfterEnqueue(t *testing.T) {
	testTcpGroupRecoveryReplacedRaceProducer(t, false)
}

// No first retained item means no funded promotion receipt. Local disposal
// must drop the entire raw candidate even while its race and offer stay live.
func TestTcpGroupRecoveryUnwrittenCandidateDoesNotRetainPromotionHistory(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		admitted := make(chan *parsedPacketGroup, 1)
		release := make(chan struct{})
		var releaseOnce sync.Once
		unpark := func() { releaseOnce.Do(func() { close(release) }) }
		budget := NewTransferMemoryBudget(kib(64))
		f := newGroupDispositionQueueFixture(t, budget, func(settings *ClientSettings) {
			settings.SendBufferSettings.beforeGroupRetainForTest = func() { budget.SetTotalByteCount(0) }
			settings.SendBufferSettings.afterGroupAdmissionForTest = func(target sendGroupAdmissionTarget) {
				admitted <- target.(*parsedPacketGroup)
				<-release
			}
		})
		t.Cleanup(unpark)
		f.update.client.Store(nil)
		f.parent.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
			return []*multiClientChannel{f.selected}
		}
		result := make(chan int, 1)
		syn := groupDispositionControlPacket(900, 500, 64, tcpFlagSyn)
		go func() { result <- f.offerTimeout(-1, syn) }()
		f.requireParked()
		if len(admitted) != 1 {
			t.Fatal("candidate admission did not reach its held return")
		}
		group := <-admitted
		f.unpark()
		synctest.Wait()
		if group.completionFlags.Load()&groupCompletionTerminal == 0 || group.collapseOwner != nil ||
			f.update.sequenceClaims != nil || budget.UsedByteCount() != 0 || len(f.route) != 0 {
			t.Fatal("unfunded terminal candidate retained uncharged promotion history")
		}
		f.requireLocalRefusalHealthy()
		unpark()
		if <-result != 1 || group.collapseSource != nil || f.update.sequenceCovered ||
			!f.update.canUpdateSequenceForClient(groupRecoveryParsed(t, syn), f.selected) {
			t.Fatal("unwritten candidate return fabricated durable proof or leaked source ownership")
		}
	})
}

// These old FIN/ACK observations are deliberately one edge short of closure.
// A new queued cohort's ACK would complete the old one if public-return order
// were allowed to apply close controls before the accepted SYN's source reset.
func testTcpGroupRecoveryQueuedSynCloseOrder(t *testing.T, grouped bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newGroupDispositionQueueFixture(t, nil, nil)
		f.parent.flowReaperWake = make(chan struct{}, 1)
		oldSyn := groupDispositionControlPacket(100, 500, 64, tcpFlagSyn)
		f.update.updateSequence(groupRecoveryParsed(t, oldSyn))
		f.update.observeTcpControl(tcpControlObservation{valid: true, fin: true, sequenceNumber: 101}, false)
		f.update.observeTcpControl(tcpControlObservation{valid: true, fin: true, ack: true,
			sequenceNumber: 500, ackSequenceNumber: 102}, true)
		f.update.receivedInbound.Store(true)
		syn := groupDispositionControlPacket(1000, 500, 64, tcpFlagSyn)
		fin := groupDispositionControlPacket(1001, 501, 64, tcpFlagFin|tcpFlagAck)
		if grouped {
			if f.offer(syn, fin) != 2 {
				t.Fatal("new grouped SYN/FIN admission refused")
			}
		} else if f.offer(syn) != 1 || f.offer(fin) != 1 {
			t.Fatal("separate queued SYN/FIN admission refused")
		}
		f.requireParked()
		if f.update.IsDone() || f.update.synGenerationNumber != 100 || !f.update.egressFinSeen {
			t.Fatal("queued new-cohort control prematurely retired or reset the old generation")
		}
		if f.update.observeTcpControl(tcpControlObservation{valid: true, ack: true, ackSequenceNumber: 1002}, true) ||
			f.update.ingressAckSequence != 102 {
			t.Fatal("unknown pre-dequeue ingress close edge entered the pending cohort")
		}
		f.unpark()
		f.acknowledgeAll(syn, fin)
		if f.update.IsDone() || f.update.synGenerationNumber != 1000 || !f.update.egressFinSeen ||
			f.update.egressFinSequence != 1002 || f.update.egressAckSequence != 501 ||
			f.update.ingressFinSeen || f.update.ingressAckSeen || f.update.receivedInbound.Load() {
			t.Fatal("source reset erased the new FIN/ACK or inherited prior close/inbound state")
		}
		if f.update.observeTcpControl(tcpControlObservation{valid: true, fin: true, ack: true,
			sequenceNumber: 700, ackSequenceNumber: 1002}, true) {
			t.Fatal("new ingress FIN closed before its source acknowledgement")
		}
		ack := groupDispositionControlPacket(1002, 701, 64, tcpFlagAck)
		if f.offer(ack) != 1 {
			t.Fatal("new close acknowledgement refused")
		}
		f.acknowledgeAll(ack)
		if !f.update.IsDone() || len(f.parent.flowReaperWake) != 1 {
			t.Fatal("source-ordered final close did not retire and notify the flow owner")
		}
	})
}

// All members remain in source order even when grouped into one admission.
func TestTcpGroupRecoveryNewSynFinGroupPreservesCloseState(t *testing.T) {
	testTcpGroupRecoveryQueuedSynCloseOrder(t, true)
}

// Separate queued offers cannot apply their close edges ahead of the new SYN.
func TestTcpGroupRecoveryQueuedSynThenFinAckPreservesCloseState(t *testing.T) {
	testTcpGroupRecoveryQueuedSynCloseOrder(t, false)
}

// An exhausted compact order field can never revive obsolete durable proof.
func TestTcpGroupRecoveryOrderExhaustionFailsOpenOverPriorCohort(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newGroupDispositionQueueFixture(t, nil, nil)
		old := groupDispositionControlPacket(100, 500, 64, tcpFlagSyn)
		if f.offer(old) != 1 {
			t.Fatal("old cohort admission refused")
		}
		f.requireParked()
		f.unpark()
		f.acknowledgeAll(old)
		if f.offer(old) != 0 {
			t.Fatal("old cohort did not establish a durable gate")
		}
		f.update.stateLock.Lock()
		f.update.sequenceSourceOrder = 1<<48 - 1
		f.update.stateLock.Unlock()
		next := groupDispositionControlPacket(900, 500, 64, tcpFlagSyn)
		if f.offer(next) != 1 {
			t.Fatal("order exhaustion refused a new cohort")
		}
		f.acknowledgeAll(next)
		if f.offer(old) != 1 {
			t.Fatal("exhausted source ordering reused obsolete prior-cohort proof")
		}
		f.acknowledgeAll(old)
		if f.update.sequenceSourceOrder != 1<<48-1 || f.update.sequenceClaims != nil {
			t.Fatal("compact order wrapped or retained an unverifiable claim")
		}
	})
}

// A reset must end an unanswered race from source order without manufacturing
// a winner. A genuinely newer SYN after that reset in one group supersedes it.
func testTcpGroupRecoveryUnselectedRaceReset(t *testing.T, newSynAfter bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		firstBudget, secondBudget := NewTransferMemoryBudget(kib(64)), NewTransferMemoryBudget(kib(64))
		f := newGroupDispositionQueueFixture(t, firstBudget, nil)
		other := newGroupDispositionQueueFixture(t, secondBudget, nil)
		f.update.updateSequence(groupRecoveryParsed(t, groupDispositionControlPacket(100, 500, 64, tcpFlagSyn)))
		f.update.client.Store(nil)
		f.parent.settings.MultiRaceSetOnResponseTimeout = time.Hour
		f.parent.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
			return []*multiClientChannel{f.selected, other.selected}
		}
		syn := groupDispositionControlPacket(900, 500, 64, tcpFlagSyn)
		rst := groupDispositionControlPacket(901, 500, 64, tcpFlagRst|tcpFlagAck)
		next := groupDispositionControlPacket(1000, 500, 64, tcpFlagSyn)
		if f.offer(syn) != 1 {
			t.Fatal("unanswered candidate SYN admission refused")
		}
		templates := [][]byte{syn, rst}
		if newSynAfter {
			if f.offer(rst, next) != 2 {
				t.Fatal("candidate reset/new-SYN group refused")
			}
			templates = append(templates, next)
		} else if f.offer(rst) != 1 {
			t.Fatal("candidate reset admission refused")
		}
		f.requireParked()
		other.requireParked()
		if f.update.IsDone() || f.update.race == nil {
			t.Fatal("public return applied race close ahead of source order")
		}
		f.unpark()
		other.unpark()
		f.acknowledgeAll(templates...)
		other.acknowledgeAll(templates...)
		if newSynAfter {
			if f.update.IsDone() || f.update.race == nil {
				t.Fatal("earlier grouped reset retired the later new SYN")
			}
			f.update.stateLock.Lock()
			f.update.commitRaceClientWithLock(f.selected)
			f.update.stateLock.Unlock()
			f.update.releaseCollapseClaims()
			if f.update.IsDone() || f.update.synGenerationNumber != 1000 || f.offer(next) != 0 {
				t.Fatal("source-ordered promotion did not preserve the post-reset SYN cohort")
			}
		} else if !f.update.IsDone() || f.update.race != nil {
			t.Fatal("source reset left an unanswered race waiting for impossible response evidence")
		}
		if firstBudget.UsedByteCount() != 0 || secondBudget.UsedByteCount() != 0 || f.update.sequenceClaims != nil {
			t.Fatal("race reset outcome retained candidate receipts or range charge")
		}
	})
}

// A stand-alone reset closes every candidate owner after the real source sees it.
func TestTcpGroupRecoveryUnselectedRaceResetReleasesCandidates(t *testing.T) {
	testTcpGroupRecoveryUnselectedRaceReset(t, false)
}

// The last genuine generation transition in one logical group wins.
func TestTcpGroupRecoveryUnselectedResetThenNewSynKeepsCohort(t *testing.T) {
	testTcpGroupRecoveryUnselectedRaceReset(t, true)
}
