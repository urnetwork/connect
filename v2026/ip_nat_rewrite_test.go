package connect

// The rewrite is only safe if the repaired checksums are indistinguishable
// from recomputed ones. Every test here verifies by FULL recomputation rather
// than by reproducing the incremental math: a correct checksum makes its
// covered region sum to zero, so `checksumFinish(checksumAdd(0, region)) == 0`
// is an independent check of the arithmetic in ip_nat_rewrite.go.

import (
	"encoding/binary"
	"net"
	"net/netip"
	"testing"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
)

func natTestAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	addr, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("parse %s: %s", s, err)
	}
	return addr
}

// buildNatTestPacket serializes a valid IPv4 packet with computed checksums.
func buildNatTestPacket(
	t *testing.T,
	source string,
	destination string,
	transport gopacket.SerializableLayer,
	payload []byte,
) []byte {
	t.Helper()
	ip := &layers.IPv4{
		Version:  4,
		IHL:      5,
		TTL:      64,
		SrcIP:    net.ParseIP(source).To4(),
		DstIP:    net.ParseIP(destination).To4(),
		Protocol: layers.IPProtocolTCP,
	}
	switch v := transport.(type) {
	case *layers.TCP:
		ip.Protocol = layers.IPProtocolTCP
		if err := v.SetNetworkLayerForChecksum(ip); err != nil {
			t.Fatalf("tcp checksum layer: %s", err)
		}
	case *layers.UDP:
		ip.Protocol = layers.IPProtocolUDP
		if err := v.SetNetworkLayerForChecksum(ip); err != nil {
			t.Fatalf("udp checksum layer: %s", err)
		}
	case *layers.ICMPv4:
		ip.Protocol = layers.IPProtocolICMPv4
	}
	buf := gopacket.NewSerializeBuffer()
	opts := gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}
	if err := gopacket.SerializeLayers(buf, opts, ip, transport, gopacket.Payload(payload)); err != nil {
		t.Fatalf("serialize: %s", err)
	}
	return append([]byte{}, buf.Bytes()...)
}

// assertNatChecksums recomputes both checksums over the packet as it stands.
func assertNatChecksums(t *testing.T, packet []byte, what string) {
	t.Helper()
	headerSize := int(packet[0]&0x0f) * 4
	if got := checksumFinish(checksumAdd(0, packet[0:headerSize])); got != 0 {
		t.Errorf("%s: ipv4 header checksum does not verify (%#x)", what, got)
	}
	protocol := ipProtocolNumber(packet[9])
	if protocol == ipProtocolNumberIcmp4 {
		// no pseudo-header; the icmp checksum covers the message only
		if got := checksumFinish(checksumAdd(0, packet[headerSize:])); got != 0 {
			t.Errorf("%s: icmp checksum does not verify (%#x)", what, got)
		}
		return
	}
	transport := packet[headerSize:]
	if protocol == ipProtocolNumberUdp && binary.BigEndian.Uint16(transport[6:8]) == 0 {
		// "not computed" stays not computed
		return
	}
	got := transportChecksum(
		protocol,
		net.IP(packet[12:16]),
		net.IP(packet[16:20]),
		transport,
	)
	if got != 0 {
		t.Errorf("%s: transport checksum does not verify (%#x)", what, got)
	}
}

func TestRewriteIpv4SourceRepairsChecksums(t *testing.T) {
	next := natTestAddr(t, "169.254.7.9")

	cases := map[string]gopacket.SerializableLayer{
		"tcp": &layers.TCP{
			SrcPort: 51423, DstPort: 443, Seq: 0x11223344, SYN: true, Window: 65535,
		},
		"udp": &layers.UDP{SrcPort: 51423, DstPort: 53},
		"icmp": &layers.ICMPv4{
			TypeCode: layers.CreateICMPv4TypeCode(layers.ICMPv4TypeEchoRequest, 0),
			Id:       7, Seq: 1,
		},
	}
	for name, transport := range cases {
		packet := buildNatTestPacket(t, "10.55.12.34", "93.184.216.34", transport, []byte("payload-bytes"))
		assertNatChecksums(t, packet, name+" before")

		if !RewriteIpv4Source(packet, next) {
			t.Fatalf("%s: rewrite refused", name)
		}
		if got, _ := netip.AddrFromSlice(packet[12:16]); got != next {
			t.Fatalf("%s: source = %s, want %s", name, got, next)
		}
		// the destination must be untouched
		if got, _ := netip.AddrFromSlice(packet[16:20]); got.String() != "93.184.216.34" {
			t.Fatalf("%s: destination changed to %s", name, got)
		}
		assertNatChecksums(t, packet, name+" after")
	}
}

func TestRewriteIpv4DestinationRepairsChecksums(t *testing.T) {
	next := natTestAddr(t, "10.55.12.34")
	packet := buildNatTestPacket(t, "93.184.216.34", "169.254.7.9",
		&layers.TCP{SrcPort: 443, DstPort: 51423, Seq: 99, ACK: true, Window: 4096},
		[]byte("return-bytes"))
	assertNatChecksums(t, packet, "before")

	if !RewriteIpv4Destination(packet, next) {
		t.Fatal("rewrite refused")
	}
	if got, _ := netip.AddrFromSlice(packet[16:20]); got != next {
		t.Fatalf("destination = %s, want %s", got, next)
	}
	assertNatChecksums(t, packet, "after")
}

// A round trip must restore the original bytes exactly, checksums included.
// The proxy relies on this: it rewrites out and back on every flow.
func TestRewriteIpv4RoundTripIsExact(t *testing.T) {
	original := buildNatTestPacket(t, "10.55.12.34", "93.184.216.34",
		&layers.UDP{SrcPort: 51423, DstPort: 443}, []byte("quic-ish"))
	packet := append([]byte{}, original...)

	if !RewriteIpv4Source(packet, natTestAddr(t, "169.254.7.9")) {
		t.Fatal("out refused")
	}
	if !RewriteIpv4Source(packet, natTestAddr(t, "10.55.12.34")) {
		t.Fatal("back refused")
	}
	if string(packet) != string(original) {
		t.Fatal("round trip did not restore the original packet")
	}
}

// A zero UDP checksum means "not computed". Giving it a value would claim a
// guarantee the sender never made.
func TestRewriteIpv4PreservesZeroUdpChecksum(t *testing.T) {
	packet := buildNatTestPacket(t, "10.55.12.34", "93.184.216.34",
		&layers.UDP{SrcPort: 51423, DstPort: 443}, []byte("no-checksum"))
	headerSize := int(packet[0]&0x0f) * 4
	binary.BigEndian.PutUint16(packet[headerSize+6:headerSize+8], 0)

	if !RewriteIpv4Source(packet, natTestAddr(t, "169.254.7.9")) {
		t.Fatal("rewrite refused")
	}
	if got := binary.BigEndian.Uint16(packet[headerSize+6 : headerSize+8]); got != 0 {
		t.Fatalf("zero udp checksum became %#x", got)
	}
	assertNatChecksums(t, packet, "zero-udp")
}

// A later fragment has no transport header. Rewriting at the transport
// checksum offset would corrupt payload bytes, so only the ipv4 header is
// repaired.
func TestRewriteIpv4LeavesLaterFragmentPayloadIntact(t *testing.T) {
	packet := buildNatTestPacket(t, "10.55.12.34", "93.184.216.34",
		&layers.TCP{SrcPort: 51423, DstPort: 443, Seq: 1, Window: 512},
		[]byte("AAAAAAAAAAAAAAAA"))
	headerSize := int(packet[0]&0x0f) * 4
	// mark it a later fragment and repair the header checksum for the change
	binary.BigEndian.PutUint16(packet[6:8], 185)
	binary.BigEndian.PutUint16(packet[10:12], 0)
	binary.BigEndian.PutUint16(packet[10:12], checksumFinish(checksumAdd(0, packet[0:headerSize])))

	body := append([]byte{}, packet[headerSize:]...)
	if !RewriteIpv4Source(packet, natTestAddr(t, "169.254.7.9")) {
		t.Fatal("rewrite refused")
	}
	if string(packet[headerSize:]) != string(body) {
		t.Fatal("later fragment body was modified")
	}
	if got := checksumFinish(checksumAdd(0, packet[0:headerSize])); got != 0 {
		t.Fatalf("later fragment header checksum does not verify (%#x)", got)
	}
}

// Options move the transport header but not the addresses.
func TestRewriteIpv4WithHeaderOptions(t *testing.T) {
	base := buildNatTestPacket(t, "10.55.12.34", "93.184.216.34",
		&layers.UDP{SrcPort: 51423, DstPort: 53}, []byte("opt"))
	// splice in a 4-byte no-op option and re-length the header
	packet := append([]byte{}, base[:20]...)
	packet = append(packet, 0x01, 0x01, 0x01, 0x00)
	packet = append(packet, base[20:]...)
	packet[0] = 0x46
	binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
	binary.BigEndian.PutUint16(packet[10:12], 0)
	binary.BigEndian.PutUint16(packet[10:12], checksumFinish(checksumAdd(0, packet[0:24])))
	assertNatChecksums(t, packet, "options before")

	if !RewriteIpv4Source(packet, natTestAddr(t, "169.254.7.9")) {
		t.Fatal("rewrite refused")
	}
	assertNatChecksums(t, packet, "options after")
}

// Refusing is the safe outcome: a caller that drops on false is correct, one
// that forwards would be forwarding a stale checksum.
func TestRewriteIpv4RefusesWhatItCannotRepair(t *testing.T) {
	next := natTestAddr(t, "169.254.7.9")

	if RewriteIpv4Source([]byte{0x45, 0x00}, next) {
		t.Error("accepted a truncated packet")
	}
	if RewriteIpv4Source(nil, next) {
		t.Error("accepted an empty packet")
	}

	v6 := make([]byte, 40)
	v6[0] = 0x60
	if RewriteIpv4Source(v6, next) {
		t.Error("accepted an ipv6 packet")
	}

	packet := buildNatTestPacket(t, "10.55.12.34", "93.184.216.34",
		&layers.UDP{SrcPort: 1, DstPort: 2}, []byte("x"))
	if RewriteIpv4Source(packet, natTestAddr(t, "fd00::1")) {
		t.Error("accepted an ipv6 replacement address")
	}

	// a truncated tcp header has no checksum field to repair
	shortTcp := buildNatTestPacket(t, "10.55.12.34", "93.184.216.34",
		&layers.TCP{SrcPort: 1, DstPort: 2, Window: 1}, nil)
	if RewriteIpv4Source(shortTcp[:int(shortTcp[0]&0x0f)*4+10], next) {
		t.Error("accepted a truncated tcp header")
	}
}

// Rewriting to the address already present must be a no-op, not a double
// checksum application.
func TestRewriteIpv4ToSameAddressIsANoOp(t *testing.T) {
	original := buildNatTestPacket(t, "10.55.12.34", "93.184.216.34",
		&layers.TCP{SrcPort: 51423, DstPort: 443, Seq: 7, Window: 100}, []byte("same"))
	packet := append([]byte{}, original...)
	if !RewriteIpv4Source(packet, natTestAddr(t, "10.55.12.34")) {
		t.Fatal("rewrite refused")
	}
	if string(packet) != string(original) {
		t.Fatal("same-address rewrite modified the packet")
	}
}

// The incremental result must equal what a from-scratch computation produces,
// across many address pairs -- the property the whole file rests on.
func TestIncrementalChecksumMatchesFullRecomputation(t *testing.T) {
	destinations := []string{"93.184.216.34", "1.1.1.1", "255.255.255.255", "0.0.0.1"}
	sources := []string{"10.0.0.1", "10.255.255.254", "169.254.0.1", "169.254.255.254", "172.16.9.9"}
	for _, destination := range destinations {
		for _, source := range sources {
			for _, next := range sources {
				packet := buildNatTestPacket(t, source, destination,
					&layers.TCP{SrcPort: 4321, DstPort: 80, Seq: 0xdeadbeef, PSH: true, ACK: true, Window: 501},
					[]byte("checksum-sensitive-payload-\x00\xff"))
				if !RewriteIpv4Source(packet, natTestAddr(t, next)) {
					t.Fatalf("%s->%s: refused", source, next)
				}
				assertNatChecksums(t, packet, source+"->"+next)

				// and byte-identical to the packet built with that source
				expected := buildNatTestPacket(t, next, destination,
					&layers.TCP{SrcPort: 4321, DstPort: 80, Seq: 0xdeadbeef, PSH: true, ACK: true, Window: 501},
					[]byte("checksum-sensitive-payload-\x00\xff"))
				if string(packet) != string(expected) {
					t.Fatalf("%s->%s: rewritten packet differs from one built with that source", source, next)
				}
			}
		}
	}
}

// A protocol whose checksum does not cover the IP pseudo-header gets a
// header-only rewrite. Refusing instead would drop SCTP, ESP, AH and GRE over
// the wg path outright -- a worse failure than the one it would guard against.
func TestRewriteIpv4HeaderOnlyProtocolsAreNatted(t *testing.T) {
	next := natTestAddr(t, "169.254.7.9")
	for name, protocol := range map[string]byte{
		"sctp": 132, "esp": 50, "ah": 51, "gre": 47,
	} {
		packet := buildNatTestPacket(t, "10.55.12.34", "93.184.216.34",
			&layers.UDP{SrcPort: 1, DstPort: 2}, []byte("body-bytes-unchanged"))
		headerSize := int(packet[0]&0x0f) * 4
		packet[9] = protocol
		binary.BigEndian.PutUint16(packet[10:12], 0)
		binary.BigEndian.PutUint16(packet[10:12], checksumFinish(checksumAdd(0, packet[0:headerSize])))
		body := append([]byte{}, packet[headerSize:]...)

		if !RewriteIpv4Source(packet, next) {
			t.Fatalf("%s: refused", name)
		}
		if got, _ := netip.AddrFromSlice(packet[12:16]); got != next {
			t.Fatalf("%s: source not rewritten", name)
		}
		if got := checksumFinish(checksumAdd(0, packet[0:headerSize])); got != 0 {
			t.Errorf("%s: header checksum does not verify (%#x)", name, got)
		}
		// the transport bytes must be untouched -- there is no pseudo-header
		// checksum in them to repair
		if string(packet[headerSize:]) != string(body) {
			t.Errorf("%s: transport bytes were modified", name)
		}
	}
}

// The pseudo-header protocols are the ones that MUST be repaired. A truncated
// header for one of them is refused rather than left stale.
func TestRewriteIpv4RefusesTruncatedPseudoHeaderTransports(t *testing.T) {
	next := natTestAddr(t, "169.254.7.9")
	for name, protocol := range map[string]byte{
		"udplite": 136, "dccp": 33,
	} {
		packet := buildNatTestPacket(t, "10.55.12.34", "93.184.216.34",
			&layers.UDP{SrcPort: 1, DstPort: 2}, []byte("x"))
		headerSize := int(packet[0]&0x0f) * 4
		packet[9] = protocol
		if RewriteIpv4Source(packet[:headerSize+4], next) {
			t.Errorf("%s: accepted a truncated header", name)
		}
	}
}

// UDP-Lite and DCCP carry the address in their pseudo-header, so their
// checksums move with it -- and unlike UDP, zero is not a "not computed"
// sentinel for UDP-Lite.
func TestRewriteIpv4RepairsUdpLiteAndDccpChecksums(t *testing.T) {
	next := natTestAddr(t, "169.254.7.9")
	for name, protocol := range map[string]byte{
		"udplite": 136, "dccp": 33,
	} {
		packet := buildNatTestPacket(t, "10.55.12.34", "93.184.216.34",
			&layers.UDP{SrcPort: 51423, DstPort: 443}, []byte("pseudo-header-covered"))
		headerSize := int(packet[0]&0x0f) * 4
		packet[9] = protocol
		binary.BigEndian.PutUint16(packet[10:12], 0)
		binary.BigEndian.PutUint16(packet[10:12], checksumFinish(checksumAdd(0, packet[0:headerSize])))
		// recompute the transport checksum for the substituted protocol so the
		// starting packet is genuinely valid
		transport := packet[headerSize:]
		binary.BigEndian.PutUint16(transport[6:8], 0)
		binary.BigEndian.PutUint16(transport[6:8], transportChecksum(
			ipProtocolNumber(protocol), net.IP(packet[12:16]), net.IP(packet[16:20]), transport))

		if !RewriteIpv4Source(packet, next) {
			t.Fatalf("%s: refused", name)
		}
		if got := transportChecksum(ipProtocolNumber(protocol),
			net.IP(packet[12:16]), net.IP(packet[16:20]), packet[headerSize:]); got != 0 {
			t.Errorf("%s: transport checksum does not verify (%#x)", name, got)
		}
	}
}
