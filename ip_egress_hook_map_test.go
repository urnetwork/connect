// The egress hook's three verdicts on each protocol (sidecar design §17.6): a
// pass dials the destination, a map dials its target while every packet toward
// the source keeps the flow's destination, and a deny answers with a reset, an
// unreachable or nothing, and dials nothing. Upstreams are loopback listeners
// on ports the kernel picks; mapped destinations are documentation addresses
// that are never dialed.
package connect

import (
	"bytes"
	"context"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// A loopback TCP listener the test accepts from.
func newEgressTestListener(t *testing.T) (*net.TCPListener, int) {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	listener.SetDeadline(time.Now().Add(egressTestTimeout))
	return listener, listener.Addr().(*net.TCPAddr).Port
}

// A loopback UDP socket the test reads from.
func newEgressTestUdpSink(t *testing.T) (*net.UDPConn, int) {
	t.Helper()
	sink, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sink.Close() })
	sink.SetDeadline(time.Now().Add(egressTestTimeout))
	return sink, sink.LocalAddr().(*net.UDPAddr).Port
}

func egressTestFlowPath(protocol IpProtocol, destinationIp net.IP, destinationPort int) *IpPath {
	return &IpPath{
		Version:         4,
		Protocol:        protocol,
		SourceIp:        net.IPv4(192, 0, 2, 2).To4(),
		SourcePort:      42000,
		DestinationIp:   destinationIp.To4(),
		DestinationPort: destinationPort,
	}
}

// The SYN-ACK toward the source, checked to come from the flow's destination.
func receiveEgressTestSynAck(t *testing.T, rig *egressTestNat, path *IpPath) *IpPath {
	t.Helper()
	synAck := egressTestParse(t, rig.receive())
	if !synAck.Syn || !synAck.Ack ||
		!synAck.SourceIp.Equal(path.DestinationIp) || synAck.SourcePort != path.DestinationPort ||
		!synAck.DestinationIp.Equal(path.SourceIp) || synAck.DestinationPort != path.SourcePort {
		t.Fatalf("SYN-ACK %+v does not come from the flow's destination %s:%d", synAck, path.DestinationIp, path.DestinationPort)
	}
	return synAck
}

// A pass dials the destination as sent, with the flow's protocol and
// addresses given to the hook.
func TestEgressHookTcpPassDialsDestination(t *testing.T) {
	assertMessagePoolOwnership(t)
	listener, port := newEgressTestListener(t)
	hook := newEgressTestHook()
	rig := newEgressTestNat(t, hook, nil)
	path := egressTestFlowPath(IpProtocolTcp, net.IPv4(127, 0, 0, 1), port)
	rig.send(egressTestTcpPacket(path, tcpFlagSyn, 100, 0, nil))

	flow := hook.takeFlow(t)
	if flow.Protocol != IpProtocolTcp ||
		flow.From != netip.MustParseAddrPort("192.0.2.2:42000") ||
		flow.To != netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(port)) {
		t.Fatalf("hook saw %+v", flow)
	}
	receiveEgressTestSynAck(t, rig, path)
	conn, err := listener.Accept()
	if err != nil {
		t.Fatalf("the destination was not dialed: %v", err)
	}
	conn.Close()
}

// A map dials its target, and the source sees only the destination it sent to:
// the SYN-ACK and the upstream's data come from it, with valid checksums.
func TestEgressHookTcpMapDialsTargetAndKeepsDestination(t *testing.T) {
	assertMessagePoolOwnership(t)
	listener, port := newEgressTestListener(t)
	hook := newEgressTestHook()
	hook.setResolver(func(flow *EgressFlow) EgressDecision {
		return EgressDecision{
			Verdict: EgressVerdictMap,
			Target:  netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(port)),
			RuleTag: 7,
		}
	})
	rig := newEgressTestNat(t, hook, nil)
	path := egressTestFlowPath(IpProtocolTcp, net.IPv4(192, 0, 2, 80), 8443)
	rig.send(egressTestTcpPacket(path, tcpFlagSyn, 100, 0, nil))

	synAck := receiveEgressTestSynAck(t, rig, path)
	conn, err := listener.Accept()
	if err != nil {
		t.Fatalf("the map's target was not dialed: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(egressTestTimeout))

	ack := synAck.SequenceNumber + 1
	rig.send(egressTestTcpPacket(path, tcpFlagAck, 101, ack, nil))
	rig.send(egressTestTcpPacket(path, tcpFlagAck|tcpFlagPsh, 101, ack, []byte("hello")))
	buffer := make([]byte, 5)
	if _, err := conn.Read(buffer); err != nil || string(buffer) != "hello" {
		t.Fatalf("target read %q err=%v", buffer, err)
	}
	if _, err := conn.Write([]byte("world")); err != nil {
		t.Fatal(err)
	}
	for {
		packet := rig.receive()
		reply := egressTestParse(t, packet)
		if !reply.SourceIp.Equal(path.DestinationIp) || reply.SourcePort != path.DestinationPort {
			t.Fatalf("a reply came from %s:%d, not the flow's destination", reply.SourceIp, reply.SourcePort)
		}
		_, payload, _ := ParseIpPathWithPayload(packet)
		if 0 < len(payload) {
			if string(payload) != "world" {
				t.Fatalf("reply payload %q", payload)
			}
			break
		}
	}
}

// A deny resets the source and dials nothing.
func TestEgressHookTcpDenyResetsWithoutDialing(t *testing.T) {
	assertMessagePoolOwnership(t)
	hook := newEgressTestHook()
	hook.setResolver(func(*EgressFlow) EgressDecision {
		return EgressDecision{Verdict: EgressVerdictDeny}
	})
	rig := newEgressTestNat(t, hook, nil)
	path := egressTestFlowPath(IpProtocolTcp, net.IPv4(192, 0, 2, 81), 8443)
	rig.send(egressTestTcpPacket(path, tcpFlagSyn, 100, 0, nil))
	reset := egressTestParse(t, rig.receive())
	if !reset.Rst || !reset.SourceIp.Equal(path.DestinationIp) || reset.AckSequenceNumber != 101 {
		t.Fatalf("deny answered %+v, want a reset acknowledging the SYN", reset)
	}
	rig.close()
	if dials := rig.dials.Load(); dials != 0 {
		t.Fatalf("a denied flow dialed %d times", dials)
	}
}

// A map of a UDP flow whose target names port 0 keeps the destination's port,
// and the target's datagram returns from the flow's destination.
func TestEgressHookUdpMapKeepsPortAndDestination(t *testing.T) {
	assertMessagePoolOwnership(t)
	sink, port := newEgressTestUdpSink(t)
	hook := newEgressTestHook()
	hook.setResolver(func(flow *EgressFlow) EgressDecision {
		return EgressDecision{Verdict: EgressVerdictMap, Target: netip.MustParseAddrPort("127.0.0.1:0")}
	})
	rig := newEgressTestNat(t, hook, nil)
	path := egressTestFlowPath(IpProtocolUdp, net.IPv4(192, 0, 2, 82), port)
	rig.send(ipOosUdpPacket(path, []byte("ping")))

	buffer := make([]byte, 16)
	n, from, err := sink.ReadFromUDP(buffer)
	if err != nil || string(buffer[:n]) != "ping" {
		t.Fatalf("target read %q err=%v", buffer[:n], err)
	}
	if _, err := sink.WriteToUDP([]byte("pong"), from); err != nil {
		t.Fatal(err)
	}
	packet := rig.receive()
	reply := egressTestParse(t, packet)
	_, payload, _ := ParseIpPathWithPayload(packet)
	if !reply.SourceIp.Equal(path.DestinationIp) || reply.SourcePort != port || string(payload) != "pong" {
		t.Fatalf("reply %+v %q, want pong from the flow's destination", reply, payload)
	}
}

// A pass dials the UDP destination as sent.
func TestEgressHookUdpPassDialsDestination(t *testing.T) {
	assertMessagePoolOwnership(t)
	sink, port := newEgressTestUdpSink(t)
	hook := newEgressTestHook()
	rig := newEgressTestNat(t, hook, nil)
	path := egressTestFlowPath(IpProtocolUdp, net.IPv4(127, 0, 0, 1), port)
	rig.send(ipOosUdpPacket(path, []byte("ping")))
	buffer := make([]byte, 16)
	if n, _, err := sink.ReadFromUDP(buffer); err != nil || string(buffer[:n]) != "ping" {
		t.Fatalf("destination read %q err=%v", buffer[:n], err)
	}
	if flow := hook.takeFlow(t); flow.Protocol != IpProtocolUdp || int(flow.To.Port()) != port {
		t.Fatalf("hook saw %+v", flow)
	}
}

// A deny answers a UDP flow with the one unreachable the provider's return
// path admits for it, and dials nothing.
func TestEgressHookUdpDenyAnswersUnreachable(t *testing.T) {
	assertMessagePoolOwnership(t)
	hook := newEgressTestHook()
	hook.setResolver(func(*EgressFlow) EgressDecision {
		return EgressDecision{Verdict: EgressVerdictDeny}
	})
	rig := newEgressTestNat(t, hook, nil)
	path := egressTestFlowPath(IpProtocolUdp, net.IPv4(192, 0, 2, 83), 5353)
	rig.send(ipOosUdpPacket(path, []byte("query")))
	unreachable := rig.receive()
	if _, ok := providerDialFailureReturnPath(path, unreachable); !ok {
		t.Fatalf("deny answered %x, not the flow's unreachable", unreachable)
	}
	rig.close()
	if dials := rig.dials.Load(); dials != 0 {
		t.Fatalf("a denied flow dialed %d times", dials)
	}
}

// An in-memory echo backend that answers each request at once and records
// where each flow echoed.
type egressTestEchoes struct {
	stateLock sync.Mutex
	targets   []net.IP
}

type egressTestEcho struct {
	replies chan icmpTimerTestReply
	closed  chan struct{}
	once    sync.Once
}

func (self *egressTestEchoes) newEgress(_ context.Context, _ int, destinationIp net.IP, _ *IcmpBufferSettings) (icmpEgress, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.targets = append(self.targets, append(net.IP(nil), destinationIp...))
	return &egressTestEcho{replies: make(chan icmpTimerTestReply, 8), closed: make(chan struct{})}, nil
}

func (self *egressTestEchoes) echoTargets() []net.IP {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]net.IP(nil), self.targets...)
}

func (self *egressTestEcho) WriteEcho(_ time.Time, _ int, sequenceNumber uint16, payload []byte) error {
	self.replies <- icmpTimerTestReply{sequenceNumber: sequenceNumber, payload: bytes.Clone(payload)}
	return nil
}

func (self *egressTestEcho) ReadEcho(time.Time) (uint16, []byte, error) {
	select {
	case reply := <-self.replies:
		return reply.sequenceNumber, reply.payload, nil
	case <-self.closed:
		return 0, nil, net.ErrClosed
	}
}

func (self *egressTestEcho) Close() {
	self.once.Do(func() { close(self.closed) })
}

// A pass echoes the destination; a map echoes its target and the reply comes
// from the flow's destination; a deny opens no echo and answers nothing.
func TestEgressHookIcmpPassMapAndDeny(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, verdict := range []EgressVerdict{EgressVerdictPass, EgressVerdictMap, EgressVerdictDeny} {
		echoes := &egressTestEchoes{}
		hook := newEgressTestHook()
		hook.setResolver(func(flow *EgressFlow) EgressDecision {
			if flow.To.Port() != 0 || flow.From.Port() != 0 {
				t.Errorf("an echo reached the hook with ports %d and %d", flow.From.Port(), flow.To.Port())
			}
			return EgressDecision{Verdict: verdict, Target: netip.MustParseAddrPort("198.51.100.9:0")}
		})
		rig := newEgressTestNat(t, hook, func(settings *LocalUserNatSettings) {
			settings.IcmpBufferSettings.newEgressForTest = echoes.newEgress
		})
		rig.send(testingIcmp4EchoPacket("192.0.2.2", "192.0.2.84", 0x2222, 1, []byte("ping")))
		if verdict == EgressVerdictDeny {
			hook.takeFlow(t)
			rig.close()
			if targets := echoes.echoTargets(); len(targets) != 0 {
				t.Fatalf("a denied echo opened %d backends", len(targets))
			}
			select {
			case packet := <-rig.received:
				t.Fatalf("a denied echo was answered: %x", packet)
			default:
			}
			continue
		}
		reply := egressTestParse(t, rig.receive())
		if reply.Protocol != IpProtocolIcmp || !reply.SourceIp.Equal(net.IPv4(192, 0, 2, 84)) {
			t.Fatalf("%s: reply %+v, want an echo reply from the flow's destination", verdict, reply)
		}
		want := net.IPv4(192, 0, 2, 84)
		if verdict == EgressVerdictMap {
			want = net.IPv4(198, 51, 100, 9)
		}
		if targets := echoes.echoTargets(); len(targets) != 1 || !targets[0].Equal(want) {
			t.Fatalf("%s: echoed %v, want %s", verdict, targets, want)
		}
		rig.close()
	}
}

// An echo mapped across address families, and a map to no address, are
// refused as denies.
func TestEgressHookRefusesUnusableMapTargets(t *testing.T) {
	icmpFlow := &EgressFlow{Protocol: IpProtocolIcmp, To: netip.MustParseAddrPort("192.0.2.84:0")}
	if _, ok := egressDial(icmpFlow, EgressDecision{Verdict: EgressVerdictMap, Target: netip.MustParseAddrPort("[2001:db8::9]:0")}, 4); ok {
		t.Fatal("an echo mapped from v4 to v6")
	}
	tcpFlow := &EgressFlow{Protocol: IpProtocolTcp, To: netip.MustParseAddrPort("192.0.2.84:8443")}
	for name, target := range map[string]netip.AddrPort{
		"no address":  {},
		"unspecified": netip.MustParseAddrPort("0.0.0.0:80"),
	} {
		if _, ok := egressDial(tcpFlow, EgressDecision{Verdict: EgressVerdictMap, Target: target}, 4); ok {
			t.Fatalf("a map to %s was allowed", name)
		}
	}
	if _, ok := egressDial(tcpFlow, EgressDecision{Verdict: EgressVerdict(7)}, 4); ok {
		t.Fatal("an unknown verdict was allowed")
	}
	dial, ok := egressDial(tcpFlow, EgressDecision{Verdict: EgressVerdictMap, Target: netip.MustParseAddrPort("[::ffff:127.0.0.1]:0")}, 4)
	if !ok || dial != netip.MustParseAddrPort("127.0.0.1:8443") {
		t.Fatalf("a mapped v4 target with port 0 dialed %s ok=%t", dial, ok)
	}
}

// A datagram or echo whose flow the hook refused before it was queued is
// refused with that flow, never retried into a second flow that asks the hook
// and answers the source again.
func TestEgressHookRefusedFlowsDatagramIsNotRetried(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, ipProtocol := range []IpProtocol{IpProtocolUdp, IpProtocolIcmp} {
		hook := newEgressTestHook()
		hook.setResolver(func(*EgressFlow) EgressDecision {
			return EgressDecision{Verdict: EgressVerdictDeny}
		})
		rig := newEgressTestNat(t, hook, func(settings *LocalUserNatSettings) {
			// the shard queues the first datagram only once its flow has ended
			settings.UdpBufferSettings.beforeSequenceSendForTest = func(sequence *UdpSequence) {
				<-sequence.ctx.Done()
			}
			settings.IcmpBufferSettings.beforeSequenceSendForTest = func(sequence *IcmpSequence) {
				<-sequence.ctx.Done()
			}
		})
		handled := make(chan struct{}, 1)
		rig.nat.afterSendPacketForTest = func() {
			handled <- struct{}{}
		}
		if ipProtocol == IpProtocolUdp {
			rig.send(ipOosUdpPacket(egressTestFlowPath(IpProtocolUdp, net.IPv4(192, 0, 2, 97), 5353), []byte("query")))
		} else {
			rig.send(testingIcmp4EchoPacket("192.0.2.2", "192.0.2.97", 0x6666, 1, []byte("ping")))
		}
		select {
		case <-handled:
		case <-time.After(egressTestTimeout):
			t.Fatalf("the %s datagram was never handled", ipProtocol)
		}
		rig.close()
		if asked := len(hook.flowsSeen); asked != 1 {
			t.Fatalf("one refused %s datagram asked the hook %d times", ipProtocol, asked)
		}
		answers := len(rig.received)
		if want := map[IpProtocol]int{IpProtocolUdp: 1, IpProtocolIcmp: 0}[ipProtocol]; answers != want {
			t.Fatalf("one refused %s datagram was answered %d times, want %d", ipProtocol, answers, want)
		}
		for len(rig.received) != 0 {
			<-rig.received
		}
	}
}
