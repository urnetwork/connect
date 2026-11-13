// The standardized inner-IP MTU must preserve packet boundaries, peer limits,
// and bounded socket reads without changing packet ownership.
package connect

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"

	"github.com/urnetwork/connect/v2026/protocol"
)

// TCP, UDP and tunnel constructors share one inner-IP policy.
func TestStandardInnerMtuAliasesAndDefaults(t *testing.T) {
	if DefaultMtu != 1280 || DefaultTunnelMtu != DefaultMtu {
		t.Fatalf("inner MTU/default alias = %d/%d, want 1280/1280", DefaultMtu, DefaultTunnelMtu)
	}
	if DefaultTcpBufferSettingsWithBufferSize(16).Mtu != 1280 || DefaultUdpBufferSettings().Mtu != 1280 ||
		DefaultTunSettings().Mtu != 1280 {
		t.Fatal("a TCP, UDP, or TUN default retained a different inner MTU")
	}
}

// Returns actual parsed SYN bytes, including a deliberately absent MSS option.
// Timestamp-only SYNs exercise option parsing rather than assigning peerMss.
func standardInnerMtuSyn(t *testing.T, family int, timestamps bool, mss uint32) (*TcpSequence, parsedTcp) {
	t.Helper()
	stream := newFragmentTestStream(family)
	settings := DefaultTcpBufferSettingsWithBufferSize(16)
	sequence := &TcpSequence{tcpBufferSettings: settings, ConnectionState: ConnectionState{
		ipVersion: family, sourceIp: stream.sourceIp, sourcePort: stream.sourcePort,
		destinationIp: stream.destinationIp, destinationPort: stream.destinationPort,
		receiveSeq: 1000, sendSeq: 2000, windowSize: 65535, enableTimestamp: timestamps,
		timestampValueForTest: func() uint32 { return 3000 },
	}}
	var packet []byte
	if mss == 0 {
		packet = sequence.tcpPacket(tcpFlagSyn, 1000, nil)
	} else {
		var err error
		packet, err = sequence.SynAck(DefaultMtu)
		if err != nil {
			t.Fatal(err)
		}
		header := Ipv4HeaderSizeWithoutExtensions
		if family == 6 {
			header = Ipv6HeaderSize
		}
		transport := packet[header:]
		transport[13] = tcpFlagSyn
		binary.BigEndian.PutUint16(transport[TcpHeaderSizeWithoutExtensions+2:], uint16(mss))
		transport[16], transport[17] = 0, 0
		binary.BigEndian.PutUint16(transport[16:], transportChecksum(ipProtocolNumberTcp, sequence.destinationIp, sequence.sourceIp, transport))
	}
	defer MessagePoolReturn(packet)
	standardInnerMtuChecksums(t, packet, ipProtocolNumberTcp)
	var sourceIp, destinationIp net.IP
	var transport []byte
	var kind ipProtocolNumber
	var ok bool
	if family == 4 {
		kind, sourceIp, destinationIp, transport, ok = parseIpv4(packet)
	} else {
		kind, sourceIp, destinationIp, transport, ok = parseIpv6(packet)
	}
	var syn parsedTcp
	if !ok || kind != ipProtocolNumberTcp || !parseTcpPacket(sourceIp, destinationIp, transport, &syn) || !syn.syn {
		t.Fatal("test did not produce a checksum-valid TCP SYN")
	}
	// initializeSyn parses options synchronously, so they remain valid until
	// the producer's packet is returned at this helper's exit.
	sequence.mutex.Lock()
	sequence.initializeSynWithLock(&syn)
	sequence.mutex.Unlock()
	syn.options = nil
	return sequence, syn
}

// An absent IPv4 MSS must use 536, not the larger configured MTU. IPv6's
// default is 1220. Positive explicit MSS is preserved; MSS=1 with timestamps
// exercises the existing defensive one-byte payload floor.
func TestStandardInnerMtuParsedSynMssDefaults(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, family := range []int{4, 6} {
		for _, timestamps := range []bool{false, true} {
			for _, advertised := range []uint32{0, 1, 536, 900, 4096} {
				sequence, _ := standardInnerMtuSyn(t, family, timestamps, advertised)
				want := advertised
				if want == 0 {
					want = 536
					if family == 6 {
						want = 1220
					}
				}
				if sequence.peerMss != want || sequence.enableTimestamp != timestamps {
					t.Fatalf("IPv%d timestamp=%t advertised=%d: parsed MSS=%d, want %d", family, timestamps, advertised, sequence.peerMss, want)
				}
				standardInnerMtuPacketization(t, sequence, want, timestamps, 0)
				if family == 6 && advertised == 0 {
					// A larger configured path proves the default, rather than
					// coincidentally matching the 1280-byte packet geometry.
					standardInnerMtuPacketization(t, sequence, want, timestamps, 0, 1500)
				}
			}
		}
	}
}

// Negotiated timestamps and learned PMTU must lower data payloads while
// retaining every byte and sequence position; a later larger PMTU cannot undo it.
func TestStandardInnerMtuTcpOptionsAndLearnedPath(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, family := range []int{4, 6} {
		for _, timestamps := range []bool{false, true} {
			for _, mss := range []uint32{700, 4096} {
				for _, learned := range []int{0, 900, 1500} {
					sequence, _ := standardInnerMtuSyn(t, family, timestamps, mss)
					standardInnerMtuPacketization(t, sequence, mss, timestamps, learned)
				}
			}
		}
	}
}

// Validates independently calculated packet lengths against actual packetizer
// output and the two-root NAT producer bound, including the MSS=1 edge.
func standardInnerMtuPacketization(t *testing.T, sequence *TcpSequence, mss uint32, timestamps bool, learned int, configuredMtus ...int) {
	t.Helper()
	family := sequence.ipVersion
	header := Ipv4HeaderSizeWithoutExtensions + TcpHeaderSizeWithoutExtensions
	if family == 6 {
		header = Ipv6HeaderSize + TcpHeaderSizeWithoutExtensions
	}
	options := 0
	if timestamps {
		options = tcpTimestampOptionByteCount
	}
	configuredMtu := DefaultMtu
	if len(configuredMtus) != 0 {
		configuredMtu = configuredMtus[0]
	}
	pathMtu := configuredMtu
	if learned != 0 {
		sequence.applyPathMtu(learned)
		pathMtu = min(pathMtu, max(learned, ipMinimumPathMtu(family)))
		sequence.applyPathMtu(9000)
		if sequence.clampPathMtu(configuredMtu) != pathMtu {
			t.Fatal("a larger PMTU update undid the retained smaller path")
		}
	}
	packetPayload := max(1, min(pathMtu-header-options, int(mss)-options))
	if got := sequence.natPacketizationByteLimit(configuredMtu, 2); got != 2*packetPayload {
		t.Fatalf("IPv%d MSS=%d timestamps=%t PMTU=%d: producer limit=%d want=%d", family, mss, timestamps, learned, got, 2*packetPayload)
	}
	payload := fragmentTestPayload(2*packetPayload + 17)
	packets, err := sequence.DataPackets(payload, len(payload), configuredMtu)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, packet := range packets {
			MessagePoolReturn(packet)
		}
	}()
	if len(packets) != (len(payload)+packetPayload-1)/packetPayload {
		t.Fatal("packetizer changed the number of ordered segments")
	}
	offset := 0
	for _, packet := range packets {
		standardInnerMtuChecksums(t, packet, ipProtocolNumberTcp)
		var sourceIp, destinationIp net.IP
		var transport []byte
		var kind ipProtocolNumber
		var ok bool
		if family == 4 {
			kind, sourceIp, destinationIp, transport, ok = parseIpv4(packet)
		} else {
			kind, sourceIp, destinationIp, transport, ok = parseIpv6(packet)
		}
		var tcp parsedTcp
		if !ok || kind != ipProtocolNumberTcp || !parseTcpPacket(sourceIp, destinationIp, transport, &tcp) {
			t.Fatal("generated return segment is not checksum-valid TCP")
		}
		count := min(packetPayload, len(payload)-offset)
		if len(packet) != header+options+count || pathMtu < len(packet) ||
			tcp.seq != sequence.receiveSeq+uint32(offset) || !bytes.Equal(tcp.payload, payload[offset:offset+count]) ||
			len(tcp.options) != options {
			t.Fatalf("IPv%d MSS=%d timestamps=%t PMTU=%d: malformed segment at %d", family, mss, timestamps, learned, offset)
		}
		offset += count
	}
	if offset != len(payload) {
		t.Fatal("packetizer lost payload bytes")
	}
}

// QUIC-minimum UDP remains one datagram: 1228-byte IPv4 or 1248-byte IPv6 at
// 1280. The historical 1100 path still fragments and reassembles the same bytes.
func TestStandardInnerMtuUdp1200BothFamilies(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, family := range []int{4, 6} {
		standardInnerMtuUdp(t, family, 1100, 2)
		standardInnerMtuUdp(t, family, DefaultMtu, 1)
	}
}

// Owns every fragment through the production dual-stack reassembly gate.
func standardInnerMtuUdp(t *testing.T, family int, mtu int, wantCount int) {
	t.Helper()
	payload := fragmentTestPayload(1200)
	packets, err := newFragmentTestStream(family).DataPackets(payload, len(payload), mtu)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, packet := range packets {
			MessagePoolReturn(packet)
		}
	}()
	if len(packets) != wantCount {
		t.Fatalf("IPv%d MTU%d emitted %d packets, want %d", family, mtu, len(packets), wantCount)
	}
	var gate ipFragmentGate
	defer gate.close()
	var result ipFragmentProcessResult
	defer func() { returnIpFragmentProcessResult(result) }()
	source := SourceId(NewId())
	for index := len(packets) - 1; 0 <= index; index-- {
		packet := packets[index]
		if len(packet) > mtu || isIpFragmentPacket(packet) != (wantCount == 2) {
			t.Fatal("UDP packet crossed its path MTU or changed fragmentation semantics")
		}
		packets[index] = nil
		result = gate.processOwned(source, TransferKey{}, protocol.ProvideMode_Network, packet)
		if !result.accepted {
			t.Fatal("fragment gate rejected the packet")
		}
	}
	if result.packet == nil {
		t.Fatal("complete UDP datagram was not reconstructed")
	}
	standardInnerMtuChecksums(t, result.packet, ipProtocolNumberUdp)
	var kind ipProtocolNumber
	var sourceIp, destinationIp net.IP
	var transport []byte
	var ok bool
	if family == 4 {
		kind, sourceIp, destinationIp, transport, ok = parseIpv4(result.packet)
	} else {
		kind, sourceIp, destinationIp, transport, ok = parseIpv6(result.packet)
	}
	var udp parsedUdp
	wantBytes := 1228
	if family == 6 {
		wantBytes = 1248
	}
	if !ok || kind != ipProtocolNumberUdp || !parseUdpPacket(sourceIp, destinationIp, transport, &udp) ||
		len(result.packet) != wantBytes || !bytes.Equal(udp.payload, payload) || udp.sourcePort != 443 || udp.destinationPort != 42000 {
		t.Fatal("UDP reassembly changed length, ports, checksum, or payload")
	}
}

// Parsing checks shape, so validate both checksum layers explicitly.
func standardInnerMtuChecksums(t *testing.T, packet []byte, kind ipProtocolNumber) {
	t.Helper()
	var sourceIp, destinationIp net.IP
	var transport []byte
	var parsedKind ipProtocolNumber
	var ok bool
	if packet[0]>>4 == 4 {
		header := int(packet[0]&0x0f) * 4
		if checksumFinish(checksumAdd(0, packet[:header])) != 0 {
			t.Fatal("IPv4 header checksum changed")
		}
		parsedKind, sourceIp, destinationIp, transport, ok = parseIpv4(packet)
	} else {
		parsedKind, sourceIp, destinationIp, transport, ok = parseIpv6(packet)
	}
	if !ok || parsedKind != kind || transportChecksum(kind, sourceIp, destinationIp, transport) != 0 {
		t.Fatal("TCP/UDP transport checksum changed")
	}
}
