// The egress hook's types and the router's refusal packet (sidecar design
// §17.5, §17.6). Addresses are documentation and private test ranges.
package connect

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"
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
		DestinationPort: 5432,
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
