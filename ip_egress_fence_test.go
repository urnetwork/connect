// The egress hook's dial fence (sidecar design §17.6, D-BRIDGE-21; FS-56 at
// connect level): a revocation published while a flow's dial is in progress
// answers the source at once and lets no byte reach the target, for TCP, UDP
// and ICMP, while the twin with no revocation forwards. A revocation that lands
// between a flow's resolve and its register is caught by the recheck before the
// first byte. Dials hold at test barriers and then complete against loopback
// listeners whatever the flow's context says, as a dial already in the kernel
// would.
package connect

import (
	"context"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A dial that waits at a barrier, then completes against the real address
// whatever the flow's context says.
type egressFenceDial struct {
	dialing chan struct{}
	release chan struct{}
}

func newEgressFenceDial() *egressFenceDial {
	return &egressFenceDial{dialing: make(chan struct{}, 4), release: make(chan struct{})}
}

func (self *egressFenceDial) settings() *DialContextSettings {
	return &DialContextSettings{
		DialContext: func(_ context.Context, network string, address string) (net.Conn, error) {
			self.dialing <- struct{}{}
			<-self.release
			return (&net.Dialer{}).DialContext(context.Background(), network, address)
		},
	}
}

func (self *egressFenceDial) awaitDialing(t *testing.T) {
	t.Helper()
	select {
	case <-self.dialing:
	case <-time.After(egressTestTimeout):
		t.Fatal("the flow never dialed")
	}
}

// A hook that passes every flow under one rule tag at its published generation.
func newEgressFenceHook(ruleTag uint64) *egressTestHook {
	hook := newEgressTestHook()
	hook.generation.Store(1)
	hook.setResolver(func(*EgressFlow) EgressDecision {
		return EgressDecision{Verdict: EgressVerdictPass, RuleTag: ruleTag, PolicyGeneration: hook.generation.Load()}
	})
	return hook
}

// Revokes the rule as a policy owner does: the new generation first, then the
// close.
func revokeEgressFenceRule(t *testing.T, rig *egressTestNat, hook *egressTestHook, ruleTag uint64, want int) {
	t.Helper()
	hook.generation.Add(1)
	if closed := rig.nat.CloseFlows(ruleTag); closed != want {
		t.Fatalf("CloseFlows(%d) ended %d flows, want %d", ruleTag, closed, want)
	}
}

// Reads what reached an accepted upstream connection before it closed.
func readEgressFenceUpstream(t *testing.T, conn net.Conn) []byte {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(egressTestTimeout))
	var received []byte
	buffer := make([]byte, 64)
	for {
		n, err := conn.Read(buffer)
		received = append(received, buffer[:n]...)
		if err != nil {
			return received
		}
	}
}

// FS-56, TCP: the dial holds, the rule is revoked, the dial completes; the
// source is reset, never sees a SYN-ACK, and the target receives nothing.
func TestEgressFenceTcpRevokedDuringDialForwardsNothing(t *testing.T) {
	assertMessagePoolOwnership(t)
	listener, port := newEgressTestListener(t)
	hook := newEgressFenceHook(7)
	dial := newEgressFenceDial()
	rig := newEgressTestNat(t, hook, func(settings *LocalUserNatSettings) {
		settings.TcpBufferSettings.DialContextSettings = dial.settings()
	})
	path := egressTestFlowPath(IpProtocolTcp, net.IPv4(127, 0, 0, 1), port)
	rig.send(egressTestTcpPacket(path, tcpFlagSyn, 100, 0, nil))
	dial.awaitDialing(t)

	revokeEgressFenceRule(t, rig, hook, 7, 1)
	reset := egressTestParse(t, rig.receive())
	if !reset.Rst || reset.Syn {
		t.Fatalf("the revoked flow answered %+v, want a reset", reset)
	}
	close(dial.release)
	upstream, err := listener.Accept()
	if err != nil {
		t.Fatalf("the held dial never completed: %v", err)
	}
	defer upstream.Close()
	// the source's data, sent the moment it could, finds no flow
	rig.send(egressTestTcpPacket(path, tcpFlagAck|tcpFlagPsh, 101, 1, []byte("secret")))
	if received := readEgressFenceUpstream(t, upstream); len(received) != 0 {
		t.Fatalf("the target received %q after the revocation", received)
	}
	rig.close()
	for {
		select {
		case packet := <-rig.received:
			if answer := egressTestParse(t, packet); answer.Syn {
				t.Fatal("the source saw a SYN-ACK after the revocation")
			}
			continue
		default:
		}
		break
	}
}

// FS-56's twin, TCP: the same held dial with no revocation forwards.
func TestEgressFenceTcpUnrevokedDialForwards(t *testing.T) {
	assertMessagePoolOwnership(t)
	listener, port := newEgressTestListener(t)
	hook := newEgressFenceHook(7)
	dial := newEgressFenceDial()
	rig := newEgressTestNat(t, hook, func(settings *LocalUserNatSettings) {
		settings.TcpBufferSettings.DialContextSettings = dial.settings()
	})
	path := egressTestFlowPath(IpProtocolTcp, net.IPv4(127, 0, 0, 1), port)
	rig.send(egressTestTcpPacket(path, tcpFlagSyn, 100, 0, nil))
	dial.awaitDialing(t)
	close(dial.release)
	synAck := receiveEgressTestSynAck(t, rig, path)
	upstream, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	rig.send(egressTestTcpPacket(path, tcpFlagAck, 101, synAck.SequenceNumber+1, nil))
	rig.send(egressTestTcpPacket(path, tcpFlagAck|tcpFlagPsh, 101, synAck.SequenceNumber+1, []byte("hello")))
	upstream.SetReadDeadline(time.Now().Add(egressTestTimeout))
	buffer := make([]byte, 5)
	if _, err := upstream.Read(buffer); err != nil || string(buffer) != "hello" {
		t.Fatalf("the target read %q err=%v", buffer, err)
	}
}

// FS-56, UDP: the source gets the flow's unreachable, and the target no
// datagram; the twin forwards.
func TestEgressFenceUdpRevokedDuringDialForwardsNothing(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, revoke := range []bool{true, false} {
		sink, port := newEgressTestUdpSink(t)
		hook := newEgressFenceHook(9)
		dial := newEgressFenceDial()
		rig := newEgressTestNat(t, hook, func(settings *LocalUserNatSettings) {
			settings.UdpBufferSettings.DialContextSettings = dial.settings()
		})
		path := egressTestFlowPath(IpProtocolUdp, net.IPv4(127, 0, 0, 1), port)
		rig.send(ipOosUdpPacket(path, []byte("secret")))
		dial.awaitDialing(t)
		if revoke {
			revokeEgressFenceRule(t, rig, hook, 9, 1)
			if _, ok := providerDialFailureReturnPath(path, rig.receive()); !ok {
				t.Fatal("the revoked flow's answer is not its unreachable")
			}
		}
		close(dial.release)
		if !revoke {
			buffer := make([]byte, 16)
			if n, _, err := sink.ReadFromUDP(buffer); err != nil || string(buffer[:n]) != "secret" {
				t.Fatalf("the unrevoked flow's target read %q err=%v", buffer[:n], err)
			}
			rig.close()
			continue
		}
		rig.close()
		sink.SetReadDeadline(time.Now())
		if n, _, err := sink.ReadFromUDP(make([]byte, 16)); err == nil {
			t.Fatalf("the target received %d bytes after the revocation", n)
		}
	}
}

// An echo backend that holds its open at a barrier and records every echo.
type egressFenceEchoes struct {
	opening chan struct{}
	release chan struct{}
	writes  atomic.Int64
}

type egressFenceEcho struct {
	echoes  *egressFenceEchoes
	replies chan icmpTimerTestReply
	closed  chan struct{}
	once    sync.Once
}

func (self *egressFenceEchoes) newEgress(context.Context, int, net.IP, *IcmpBufferSettings) (icmpEgress, error) {
	self.opening <- struct{}{}
	<-self.release
	return &egressFenceEcho{echoes: self, replies: make(chan icmpTimerTestReply, 4), closed: make(chan struct{})}, nil
}

func (self *egressFenceEcho) WriteEcho(_ time.Time, _ int, sequenceNumber uint16, payload []byte) error {
	self.echoes.writes.Add(1)
	self.replies <- icmpTimerTestReply{sequenceNumber: sequenceNumber, payload: append([]byte(nil), payload...)}
	return nil
}

func (self *egressFenceEcho) ReadEcho(time.Time) (uint16, []byte, error) {
	select {
	case reply := <-self.replies:
		return reply.sequenceNumber, reply.payload, nil
	case <-self.closed:
		return 0, nil, net.ErrClosed
	}
}

func (self *egressFenceEcho) Close() {
	self.once.Do(func() { close(self.closed) })
}

// FS-56, ICMP: an echo revoked while its backend opens is never sent and gets
// no answer; the twin is echoed.
func TestEgressFenceIcmpRevokedDuringOpenEchoesNothing(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, revoke := range []bool{true, false} {
		echoes := &egressFenceEchoes{opening: make(chan struct{}, 4), release: make(chan struct{})}
		hook := newEgressFenceHook(11)
		rig := newEgressTestNat(t, hook, func(settings *LocalUserNatSettings) {
			settings.IcmpBufferSettings.newEgressForTest = echoes.newEgress
		})
		rig.send(testingIcmp4EchoPacket("192.0.2.2", "192.0.2.85", 0x4444, 1, []byte("ping")))
		select {
		case <-echoes.opening:
		case <-time.After(egressTestTimeout):
			t.Fatal("the echo never opened")
		}
		if revoke {
			revokeEgressFenceRule(t, rig, hook, 11, 1)
		}
		close(echoes.release)
		if !revoke {
			if reply := egressTestParse(t, rig.receive()); reply.Protocol != IpProtocolIcmp {
				t.Fatalf("the unrevoked echo got %+v", reply)
			}
			rig.close()
			continue
		}
		rig.close()
		if writes := echoes.writes.Load(); writes != 0 {
			t.Fatalf("a revoked echo was sent %d times", writes)
		}
		select {
		case packet := <-rig.received:
			t.Fatalf("a revoked echo was answered: %x", packet)
		default:
		}
	}
}

// The gap: the rule is revoked and `CloseFlows` runs after the flow resolved
// but before it registered, so the close cannot see it. The recheck before the
// first byte resolves the flow again under the new policy and ends it.
func TestEgressFenceRecheckCatchesARevocationBeforeRegister(t *testing.T) {
	assertMessagePoolOwnership(t)
	listener, port := newEgressTestListener(t)
	hook := newEgressTestHook()
	hook.generation.Store(1)
	var rig *egressTestNat
	var resolves atomic.Int64
	var gapClosed atomic.Int64
	hook.setResolver(func(*EgressFlow) EgressDecision {
		if resolves.Add(1) == 1 {
			decision := EgressDecision{Verdict: EgressVerdictPass, RuleTag: 5, PolicyGeneration: hook.generation.Load()}
			hook.generation.Add(1)
			gapClosed.Store(int64(rig.nat.CloseFlows(5)))
			return decision
		}
		return EgressDecision{Verdict: EgressVerdictDeny, PolicyGeneration: hook.generation.Load()}
	})
	rig = newEgressTestNat(t, hook, nil)
	path := egressTestFlowPath(IpProtocolTcp, net.IPv4(127, 0, 0, 1), port)
	rig.send(egressTestTcpPacket(path, tcpFlagSyn, 100, 0, nil))
	reset := egressTestParse(t, rig.receive())
	if !reset.Rst {
		t.Fatalf("the gap's flow answered %+v, want a reset", reset)
	}
	if gapClosed.Load() != 0 || resolves.Load() != 2 {
		t.Fatalf("the gap's close ended %d flows and the hook resolved %d times, want 0 and 2", gapClosed.Load(), resolves.Load())
	}
	upstream, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	if received := readEgressFenceUpstream(t, upstream); len(received) != 0 {
		t.Fatalf("the target received %q", received)
	}
}

// After a generation change only the same verdict and target keep a flow; a
// map to another target ends it as a deny (plan-BRIDGE-small-readings #10),
// and the same decision under a new tag keeps it, registered anew.
func TestEgressFenceRecheckKeepsOnlyTheSameDecision(t *testing.T) {
	first := netip.MustParseAddrPort("127.0.0.1:9001")
	path := egressTestFlowPath(IpProtocolTcp, net.IPv4(192, 0, 2, 86), 8443)
	for _, c := range []struct {
		again EgressDecision
		keep  bool
	}{
		{again: EgressDecision{Verdict: EgressVerdictMap, Target: first, RuleTag: 6, PolicyGeneration: 2}, keep: true},
		{again: EgressDecision{Verdict: EgressVerdictMap, Target: netip.MustParseAddrPort("127.0.0.1:9002"), PolicyGeneration: 2}, keep: false},
		{again: EgressDecision{Verdict: EgressVerdictPass, PolicyGeneration: 2}, keep: false},
		{again: EgressDecision{Verdict: EgressVerdictMap, Target: first, PolicyGeneration: 1}, keep: false},
	} {
		hook := newEgressTestHook()
		hook.generation.Store(2)
		hook.setResolver(func(*EgressFlow) EgressDecision { return c.again })
		egress := newLocalUserNatEgress(DefaultLocalUserNatSettings())
		box := &egressHookBox{hook: hook}
		egress.hook.Store(box)
		state := &egressFlowState{nat: egress}
		state.registration.Store(&egressRegistration{
			box:      box,
			decision: EgressDecision{Verdict: EgressVerdictMap, Target: first, RuleTag: 5, PolicyGeneration: 1},
			dial:     first,
		})
		kept := state.recheck(context.Background(), SourceId(NewId()), TransferKey{}, 0, path)
		if kept != c.keep {
			t.Fatalf("a recheck answering %+v kept the flow: %t, want %t", c.again, kept, c.keep)
		}
		if kept && state.registration.Load().decision.RuleTag != 6 {
			t.Fatal("a kept flow did not register its new decision")
		}
	}
}

// A revocation ends only its tag's flows decided below the new generation:
// another tag's flow keeps forwarding, and so does a flow of the same tag that
// resolved under the new generation.
func TestEgressFenceSparesOtherTagsAndNewerDecisions(t *testing.T) {
	assertMessagePoolOwnership(t)
	listener, port := newEgressTestListener(t)
	hook := newEgressTestHook()
	hook.generation.Store(1)
	hook.setResolver(func(flow *EgressFlow) EgressDecision {
		ruleTag := uint64(1)
		if flow.From.Port() == 42011 {
			ruleTag = 2
		}
		return EgressDecision{Verdict: EgressVerdictPass, RuleTag: ruleTag, PolicyGeneration: hook.generation.Load()}
	})
	rig := newEgressTestNat(t, hook, nil)
	type openFlow struct {
		path     *IpPath
		ack      uint32
		upstream net.Conn
	}
	open := func(sourcePort int) openFlow {
		path := egressTestFlowPath(IpProtocolTcp, net.IPv4(127, 0, 0, 1), port)
		path.SourcePort = sourcePort
		rig.send(egressTestTcpPacket(path, tcpFlagSyn, 100, 0, nil))
		synAck := receiveEgressTestSynAck(t, rig, path)
		upstream, err := listener.Accept()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { upstream.Close() })
		rig.send(egressTestTcpPacket(path, tcpFlagAck, 101, synAck.SequenceNumber+1, nil))
		return openFlow{path: path, ack: synAck.SequenceNumber + 1, upstream: upstream}
	}
	forwards := func(flow openFlow, payload string) {
		t.Helper()
		rig.send(egressTestTcpPacket(flow.path, tcpFlagAck|tcpFlagPsh, 101, flow.ack, []byte(payload)))
		flow.upstream.SetReadDeadline(time.Now().Add(egressTestTimeout))
		buffer := make([]byte, len(payload))
		if _, err := flow.upstream.Read(buffer); err != nil || string(buffer) != payload {
			t.Fatalf("a spared flow stopped forwarding: %q err=%v", buffer, err)
		}
	}
	revoked := open(42010)
	otherTag := open(42011)
	hook.generation.Add(1)
	newer := open(42012)
	if closed := rig.nat.CloseFlows(1); closed != 1 {
		t.Fatalf("CloseFlows ended %d flows, want 1", closed)
	}
	// the spared flows may acknowledge at any time; the revoked one's reset
	// is the answer this waits for
	for {
		answer := egressTestParse(t, rig.receive())
		if answer.DestinationPort != 42010 {
			if answer.Rst {
				t.Fatalf("a spared flow was reset: %+v", answer)
			}
			continue
		}
		if !answer.Rst {
			t.Fatalf("the revocation answered %+v", answer)
		}
		break
	}
	if received := readEgressFenceUpstream(t, revoked.upstream); len(received) != 0 {
		t.Fatalf("the revoked flow's target received %q", received)
	}
	forwards(otherTag, "other tag")
	forwards(newer, "newer")
}

// An established flow's revocation sends its source one reset and closes its
// upstream socket, and its packets take no egress lock while it lives.
func TestEgressFenceClosesAnEstablishedFlowOnce(t *testing.T) {
	assertMessagePoolOwnership(t)
	listener, port := newEgressTestListener(t)
	hook := newEgressFenceHook(3)
	var stateLocks atomic.Int64
	rig := newEgressTestNat(t, hook, func(settings *LocalUserNatSettings) {
		settings.afterEgressStateLockForTest = func() { stateLocks.Add(1) }
	})
	path := egressTestFlowPath(IpProtocolTcp, net.IPv4(127, 0, 0, 1), port)
	rig.send(egressTestTcpPacket(path, tcpFlagSyn, 100, 0, nil))
	synAck := receiveEgressTestSynAck(t, rig, path)
	upstream, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	upstream.SetReadDeadline(time.Now().Add(egressTestTimeout))
	ack := synAck.SequenceNumber + 1
	rig.send(egressTestTcpPacket(path, tcpFlagAck, 101, ack, nil))
	locksEstablished := stateLocks.Load()
	sequence := uint32(101)
	for i := 0; i < 16; i += 1 {
		rig.send(egressTestTcpPacket(path, tcpFlagAck|tcpFlagPsh, sequence, ack, []byte("x")))
		sequence += 1
	}
	// every segment the source sent has arrived, so none is left to meet the
	// closed flow and draw an orphan reset of its own
	if _, err := io.ReadFull(upstream, make([]byte, 16)); err != nil {
		t.Fatal(err)
	}
	if locks := stateLocks.Load(); locks != locksEstablished {
		t.Fatalf("the established path took the egress lock %d times", locks-locksEstablished)
	}

	revokeEgressFenceRule(t, rig, hook, 3, 1)
	readEgressFenceUpstream(t, upstream)
	rig.close()
	resets := 0
	for {
		select {
		case packet := <-rig.received:
			if egressTestParse(t, packet).Rst {
				resets += 1
			}
			continue
		default:
		}
		break
	}
	if resets != 1 {
		t.Fatalf("the revoked flow's source got %d resets, want 1", resets)
	}
}

// A binder hook reaches every NAT it is installed on, and lets each go when
// it is replaced or the NAT ends.
type egressBinderTestHook struct {
	*egressTestHook
	stateLock sync.Mutex
	closers   []EgressFlowCloser
	unbinds   int
}

func (self *egressBinderTestHook) BindFlowCloser(closer EgressFlowCloser) func() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.closers = append(self.closers, closer)
	return func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.unbinds += 1
	}
}

func (self *egressBinderTestHook) counts() (int, int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.closers), self.unbinds
}

func TestEgressHookBinderReachesEveryNat(t *testing.T) {
	assertMessagePoolOwnership(t)
	hook := &egressBinderTestHook{egressTestHook: newEgressFenceHook(4)}
	constructed := newEgressTestNat(t, hook, nil)
	installed := newEgressTestNat(t, nil, nil)
	installed.nat.SetEgressHook(hook)
	if binds, unbinds := hook.counts(); binds != 2 || unbinds != 0 {
		t.Fatalf("binds=%d unbinds=%d, want 2 and 0", binds, unbinds)
	}
	for _, closer := range hook.closers {
		if closed := closer.CloseFlows(4); closed != 0 {
			t.Fatalf("an idle NAT ended %d flows", closed)
		}
	}
	installed.nat.SetEgressHook(newEgressTestHook())
	if _, unbinds := hook.counts(); unbinds != 1 {
		t.Fatalf("a replaced hook was unbound %d times", unbinds)
	}
	constructed.close()
	if _, unbinds := hook.counts(); unbinds != 2 {
		t.Fatalf("an ended NAT unbound %d times in all", unbinds)
	}
}
