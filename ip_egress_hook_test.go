// The egress hook's types, the router's refusal packet, and the fake hook and
// NAT the hook's tests share (sidecar design §17.5, §17.6). Upstreams are
// loopback listeners on ports the kernel picks; every other address is a
// documentation or private test range.
package connect

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A decision a hook leaves unset refuses its flow.
func TestEgressVerdictZeroValueDenies(t *testing.T) {
	var decision EgressDecision
	if decision.Verdict != EgressVerdictDeny {
		t.Fatalf("zero verdict = %s, want deny", decision.Verdict)
	}
	for verdict, name := range map[EgressVerdict]string{
		EgressVerdictDeny: "deny",
		EgressVerdictPass: "pass",
		EgressVerdictMap:  "map",
		EgressVerdict(9):  "unknown(9)",
	} {
		if verdict.String() != name {
			t.Fatalf("%d named %q, want %q", int(verdict), verdict.String(), name)
		}
	}
}

func egressRejectTestPath(protocol IpProtocol, version int) *IpPath {
	path := &IpPath{
		Version:         4,
		Protocol:        protocol,
		SourceIp:        net.ParseIP("100.79.0.20").To4(),
		SourcePort:      40100,
		DestinationIp:   net.ParseIP("100.79.1.30").To4(),
		DestinationPort: 15432,
	}
	if version == 6 {
		path.Version = 6
		path.SourceIp = net.ParseIP("fd00:a::20")
		path.DestinationIp = net.ParseIP("2001:db8::30")
	}
	return path
}

// Checks an ICMP refusal's header, addresses and checksums against the packet
// it refuses, and returns the quoted bytes.
func checkAdministrativeReject(t *testing.T, version int, packet []byte, reply []byte) []byte {
	t.Helper()
	if reply == nil {
		t.Fatalf("v%d: no refusal", version)
	}
	var icmp []byte
	var sourceIp, destinationIp net.IP
	switch version {
	case 4:
		if len(reply) < Ipv4HeaderSizeWithoutExtensions+IcmpHeaderSize || reply[0] != 0x45 || reply[9] != byte(ipProtocolNumberIcmp4) {
			t.Fatalf("v4 refusal is not an icmp packet: %x", reply)
		}
		if checksumFinish(checksumAdd(0, reply[:Ipv4HeaderSizeWithoutExtensions])) != 0 {
			t.Fatal("v4 refusal header checksum invalid")
		}
		if int(binary.BigEndian.Uint16(reply[2:4])) != len(reply) {
			t.Fatal("v4 refusal total length wrong")
		}
		sourceIp, destinationIp = net.IP(reply[12:16]), net.IP(reply[16:20])
		icmp = reply[Ipv4HeaderSizeWithoutExtensions:]
		if icmp[0] != 3 || icmp[1] != 13 {
			t.Fatalf("v4 refusal type %d code %d, want 3/13", icmp[0], icmp[1])
		}
		if checksumFinish(checksumAdd(0, icmp)) != 0 {
			t.Fatal("v4 refusal icmp checksum invalid")
		}
		if !sourceIp.Equal(net.IP(packet[16:20])) || !destinationIp.Equal(net.IP(packet[12:16])) {
			t.Fatalf("v4 refusal runs %s->%s, want it back to the source", sourceIp, destinationIp)
		}
	case 6:
		if len(reply) < Ipv6HeaderSize+IcmpHeaderSize || reply[0]>>4 != 6 || reply[6] != byte(ipProtocolNumberIcmp6) {
			t.Fatalf("v6 refusal is not an icmpv6 packet: %x", reply)
		}
		sourceIp, destinationIp = net.IP(reply[8:24]), net.IP(reply[24:40])
		icmp = reply[Ipv6HeaderSize:]
		if icmp[0] != 1 || icmp[1] != 1 {
			t.Fatalf("v6 refusal type %d code %d, want 1/1", icmp[0], icmp[1])
		}
		if transportChecksum(ipProtocolNumberIcmp6, sourceIp, destinationIp, icmp) != 0 {
			t.Fatal("v6 refusal checksum invalid")
		}
		if !sourceIp.Equal(net.IP(packet[24:40])) || !destinationIp.Equal(net.IP(packet[8:24])) {
			t.Fatalf("v6 refusal runs %s->%s, want it back to the source", sourceIp, destinationIp)
		}
	}
	return icmp[IcmpHeaderSize:]
}

// A TCP segment is refused with a reset back to its source, and a reset gets
// nothing.
func TestAdministrativeRejectPacketResetsTcp(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{4, 6} {
		path := egressRejectTestPath(IpProtocolTcp, version)
		syn := ipOosTcpPacketSequence(path, tcpFlagSyn, 7000, nil)
		reply := AdministrativeRejectPacket(syn)
		if reply == nil {
			t.Fatalf("v%d: a SYN got no reset", version)
		}
		replyPath, err := ParseIpPath(reply)
		if err != nil || replyPath.Protocol != IpProtocolTcp || !replyPath.Rst ||
			!replyPath.SourceIp.Equal(path.DestinationIp) || replyPath.SourcePort != path.DestinationPort ||
			!replyPath.DestinationIp.Equal(path.SourceIp) || replyPath.DestinationPort != path.SourcePort {
			t.Fatalf("v%d: reset %+v err=%v does not answer the SYN", version, replyPath, err)
		}
		MessagePoolReturn(reply)
		reset, _ := ipOosRst(path)
		if reply := AdministrativeRejectPacket(reset); reply != nil {
			MessagePoolReturn(reply)
			t.Fatalf("v%d: a reset was answered", version)
		}
	}
}

// A UDP datagram is refused as administratively prohibited, quoting its start.
func TestAdministrativeRejectPacketProhibitsUdp(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{4, 6} {
		path := egressRejectTestPath(IpProtocolUdp, version)
		datagram := ipOosUdpPacket(path, bytes.Repeat([]byte("q"), 64))
		reply := AdministrativeRejectPacket(datagram)
		quoted := checkAdministrativeReject(t, version, datagram, reply)
		want := datagram[:Ipv4HeaderSizeWithoutExtensions+icmp4UnreachableQuotedTransport]
		if version == 6 {
			want = datagram
		}
		if !bytes.Equal(quoted, want) {
			t.Fatalf("v%d: quoted %x, want %x", version, quoted, want)
		}
		MessagePoolReturn(reply)
	}
}

// An echo is refused; an ICMP error is not.
func TestAdministrativeRejectPacketAnswersEchoButNoError(t *testing.T) {
	assertMessagePoolOwnership(t)
	echo4 := testingIcmp4EchoPacket("100.79.0.20", "100.79.1.30", 9, 1, []byte("ping"))
	reply := AdministrativeRejectPacket(echo4)
	checkAdministrativeReject(t, 4, echo4, reply)
	MessagePoolReturn(reply)
	echo6 := testingIcmp6EchoPacket("fd00:a::20", "2001:db8::30", 9, 1, []byte("ping"))
	reply = AdministrativeRejectPacket(echo6)
	checkAdministrativeReject(t, 6, echo6, reply)
	MessagePoolReturn(reply)

	for name, packet := range map[string][]byte{
		"v4 unreachable":    testingIcmp4Packet("100.79.0.20", "100.79.1.30", 3, 1, 0, 0, nil),
		"v4 time exceeded":  testingIcmp4Packet("100.79.0.20", "100.79.1.30", 11, 0, 0, 0, nil),
		"v6 unreachable":    testingIcmp6EchoTypePacket("fd00:a::20", "2001:db8::30", 1, 0, 0, nil),
		"v6 packet too big": testingIcmp6EchoTypePacket("fd00:a::20", "2001:db8::30", 2, 0, 0, nil),
	} {
		if reply := AdministrativeRejectPacket(packet); reply != nil {
			MessagePoolReturn(reply)
			t.Fatalf("%s was answered", name)
		}
	}
}

// Nothing answers a fragment, a packet from or to a non-host address, or
// anything malformed.
func TestAdministrativeRejectPacketRefusesNonHosts(t *testing.T) {
	assertMessagePoolOwnership(t)
	udp4 := egressRejectTestPath(IpProtocolUdp, 4)
	fragment := ipOosUdpPacket(udp4, []byte("fragment"))
	binary.BigEndian.PutUint16(fragment[6:8], 0x2000)
	multicast := *udp4
	multicast.DestinationIp = net.ParseIP("224.0.0.9").To4()
	broadcast := *udp4
	broadcast.DestinationIp = net.IPv4bcast.To4()
	unspecified := *udp4
	unspecified.SourceIp = net.IPv4zero.To4()
	udp6 := egressRejectTestPath(IpProtocolUdp, 6)
	multicast6 := *udp6
	multicast6.DestinationIp = net.ParseIP("ff02::1")
	full := ipOosUdpPacket(udp4, []byte("whole"))
	for name, packet := range map[string][]byte{
		"empty":               nil,
		"truncated":           full[:10],
		"unknown version":     {0x15, 0, 0, 0},
		"v4 fragment":         fragment,
		"v4 multicast":        ipOosUdpPacket(&multicast, []byte("m")),
		"v4 broadcast":        ipOosUdpPacket(&broadcast, []byte("b")),
		"v4 unspecified":      ipOosUdpPacket(&unspecified, []byte("u")),
		"v6 multicast":        ipOosUdpPacket(&multicast6, []byte("m")),
		"v4 total over bytes": append(append([]byte(nil), full[:2]...), append([]byte{0xff, 0xff}, full[4:]...)...),
	} {
		if reply := AdministrativeRejectPacket(packet); reply != nil {
			MessagePoolReturn(reply)
			t.Fatalf("%s was answered", name)
		}
	}
}

// How long a test waits for an event it is owed. A bound on a failure, never a
// schedule: every wait ends on the event itself.
const egressTestTimeout = 10 * time.Second

// A hook that records what it is asked and answers as the test says: pass
// when no resolver is set, and no principal when no verifier is.
type egressTestHook struct {
	generation       atomic.Uint64
	friendsAndFamily atomic.Bool

	stateLock   sync.Mutex
	resolver    func(*EgressFlow) EgressDecision
	verifier    func(TransferPath, Peer) *BridgePrincipal
	flows       []EgressFlow
	verifyPeers []Peer

	// every flow it decides, as it decides it
	flowsSeen chan EgressFlow
}

func newEgressTestHook() *egressTestHook {
	return &egressTestHook{flowsSeen: make(chan EgressFlow, 64)}
}

func (self *egressTestHook) setResolver(resolver func(*EgressFlow) EgressDecision) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.resolver = resolver
}

func (self *egressTestHook) setVerifier(verifier func(TransferPath, Peer) *BridgePrincipal) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.verifier = verifier
}

func (self *egressTestHook) VerifyPrincipal(source TransferPath, peer Peer) *BridgePrincipal {
	var verifier func(TransferPath, Peer) *BridgePrincipal
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		verifier = self.verifier
		self.verifyPeers = append(self.verifyPeers, peer)
	}()
	if verifier == nil {
		return nil
	}
	return verifier(source, peer)
}

func (self *egressTestHook) ResolveEgress(flow *EgressFlow) EgressDecision {
	var resolver func(*EgressFlow) EgressDecision
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		resolver = self.resolver
		self.flows = append(self.flows, *flow)
	}()
	select {
	case self.flowsSeen <- *flow:
	default:
	}
	if resolver == nil {
		return EgressDecision{Verdict: EgressVerdictPass, PolicyGeneration: self.generation.Load()}
	}
	return resolver(flow)
}

func (self *egressTestHook) PolicyGeneration() uint64 {
	return self.generation.Load()
}

func (self *egressTestHook) FriendsAndFamilyEgress() bool {
	return self.friendsAndFamily.Load()
}

func (self *egressTestHook) verifyCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.verifyPeers)
}

func (self *egressTestHook) takeFlow(t *testing.T) EgressFlow {
	t.Helper()
	select {
	case flow := <-self.flowsSeen:
		return flow
	case <-time.After(egressTestTimeout):
		t.Fatal("the hook was never asked")
		return EgressFlow{}
	}
}

// A NAT with the test's hook, whose packets toward the source arrive on
// `received` as copies. Its dials go through a counting dialer.
type egressTestNat struct {
	t        *testing.T
	nat      *LocalUserNat
	source   TransferPath
	received chan []byte
	dials    atomic.Int64
	closed   bool
}

func newEgressTestNat(t *testing.T, hook EgressHook, configure func(*LocalUserNatSettings)) *egressTestNat {
	t.Helper()
	self := &egressTestNat{
		t:        t,
		source:   SourceId(NewId()),
		received: make(chan []byte, 256),
	}
	settings := DefaultLocalUserNatSettingsWithBufferSize(16)
	settings.Log = NewNoopLogger()
	if hook != nil {
		settings.EgressHook = hook
	}
	dialer := &net.Dialer{}
	dial := &DialContextSettings{
		DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			self.dials.Add(1)
			return dialer.DialContext(ctx, network, address)
		},
	}
	settings.TcpBufferSettings.DialContextSettings = dial
	settings.UdpBufferSettings.DialContextSettings = dial
	if configure != nil {
		configure(settings)
	}
	self.nat = NewLocalUserNat(context.Background(), "egress test", settings)
	self.nat.AddReceivePacketCallback(func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
		select {
		case self.received <- slices.Clone(packet):
		default:
			t.Error("the source's receive queue overflowed")
		}
	})
	t.Cleanup(self.close)
	return self
}

// Joins every flow, so what the NAT did is final.
func (self *egressTestNat) close() {
	if self.closed {
		return
	}
	self.closed = true
	ctx, cancel := context.WithTimeout(context.Background(), egressTestTimeout)
	defer cancel()
	if err := self.nat.CloseAndWait(ctx); err != nil {
		self.t.Error(err)
	}
}

// Hands one packet from the source to the NAT.
func (self *egressTestNat) send(packet []byte) {
	self.t.Helper()
	owned := MessagePoolCopy(packet)
	if !self.nat.SendPacket(self.source, protocol.ProvideMode_Network, owned, -1) {
		MessagePoolReturn(owned)
		self.t.Fatal("the NAT refused a packet")
	}
}

// The next packet toward the source.
func (self *egressTestNat) receive() []byte {
	self.t.Helper()
	select {
	case packet := <-self.received:
		return packet
	case <-time.After(egressTestTimeout):
		self.t.Fatal("nothing reached the source")
		return nil
	}
}

// Builds a TCP segment from the source with an acknowledgement number.
func egressTestTcpPacket(path *IpPath, flags byte, sequenceNumber uint32, ackNumber uint32, payload []byte) []byte {
	packet := ipOosTcpPacketSequence(path, flags, sequenceNumber, payload)
	headerByteCount := Ipv4HeaderSizeWithoutExtensions
	if path.Version == 6 {
		headerByteCount = Ipv6HeaderSize
	}
	tcp := packet[headerByteCount:]
	binary.BigEndian.PutUint32(tcp[8:12], ackNumber)
	tcp[16], tcp[17] = 0, 0
	binary.BigEndian.PutUint16(tcp[16:18], ipPathTransportChecksum(path, ipProtocolNumberTcp, tcp))
	return packet
}

// Parses a packet toward the source and checks its transport checksum.
func egressTestParse(t *testing.T, packet []byte) *IpPath {
	t.Helper()
	path, err := ParseIpPath(packet)
	if err != nil {
		t.Fatalf("a packet toward the source does not parse: %v", err)
	}
	var ipProtocol ipProtocolNumber
	var sourceIp, destinationIp net.IP
	var transport []byte
	switch path.Version {
	case 4:
		ipProtocol, sourceIp, destinationIp, transport, _ = parseIpv4(packet)
		if checksumFinish(checksumAdd(0, packet[:Ipv4HeaderSizeWithoutExtensions])) != 0 {
			t.Fatal("a packet toward the source has a bad header checksum")
		}
	default:
		ipProtocol, sourceIp, destinationIp, transport, _ = parseIpv6(packet)
	}
	checksum := transportChecksum(ipProtocol, sourceIp, destinationIp, transport)
	if ipProtocol == ipProtocolNumberIcmp4 {
		// icmpv4 checksums the message alone
		checksum = checksumFinish(checksumAdd(0, transport))
	}
	if checksum != 0 {
		t.Fatalf("a %s packet toward the source has a bad checksum", path.Protocol)
	}
	return path
}
