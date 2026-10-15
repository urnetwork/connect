package connect

import (
	"bytes"
	"encoding/binary"
	"net"
	"net/netip"
	"testing"

	"github.com/gopacket/gopacket/layers"
)

// Use the real UDP teardown builder: the MultiClient creates this packet
// locally, so it bypasses the provider-ingress ICMP intercept. WireGuard NAT
// must restore the quotation too, or a socket owning the peer address cannot
// recognize its own failed datagram.
func TestRewriteIpv4DestinationRestoresUdpTeardownQuote(t *testing.T) {
	peer := netip.MustParseAddr("10.55.12.34")
	nat := netip.MustParseAddr("169.254.7.9")
	path := udpTestPath(4)
	path.SourceIp = net.IP(nat.AsSlice())
	path.DestinationIp = net.IPv4(1, 1, 1, 1)
	path.DestinationPort = 53
	packet, ok := (&RemoteUserNatMultiClient{
		settings: &MultiClientSettings{UdpTeardownSignal: true},
	}).teardownSourcePacket(path, 0)
	if !ok {
		t.Fatal("UDP teardown was not generated")
	}
	if len(packet) != 56 {
		t.Fatalf("UDP teardown size = %d, want 56", len(packet))
	}
	if !RewriteIpv4Destination(packet, peer) {
		t.Fatal("return NAT refused UDP teardown")
	}
	quote := packet[28:]
	if got := netip.AddrFrom4([4]byte(quote[12:16])); got != peer {
		t.Fatalf("quoted source = %s, want socket-owned %s (internal NAT %s must not escape)", got, peer, nat)
	}
	if got := netip.AddrFrom4([4]byte(packet[16:20])); got != peer {
		t.Fatalf("outer destination = %s, want %s", got, peer)
	}
	if packet[20] != 3 || packet[21] != 3 ||
		binary.BigEndian.Uint16(quote[20:22]) != uint16(path.SourcePort) ||
		binary.BigEndian.Uint16(quote[22:24]) != 53 ||
		!bytes.Equal(quote[16:20], net.IPv4(1, 1, 1, 1).To4()) {
		t.Fatal("NAT changed the ICMP error or remote UDP tuple")
	}
	assertNatChecksums(t, packet, "outer teardown")
	assertNatChecksums(t, quote, "quoted UDP")
	once := bytes.Clone(packet)
	if !RewriteIpv4Destination(packet, peer) || !bytes.Equal(packet, once) {
		t.Fatal("return NAT is not idempotent")
	}
}

func natTestIcmpError(quote []byte, errorType byte, code byte, destination netip.Addr) []byte {
	packet := make([]byte, 28+len(quote))
	packet[0], packet[8], packet[9] = 0x45, 64, 1
	binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
	copy(packet[12:16], []byte{1, 1, 1, 1})
	copy(packet[16:20], destination.AsSlice())
	packet[20], packet[21] = errorType, code
	copy(packet[28:], quote)
	binary.BigEndian.PutUint16(packet[22:24], checksumFinish(checksumAdd(0, packet[20:])))
	binary.BigEndian.PutUint16(packet[10:12], checksumFinish(checksumAdd(0, packet[:20])))
	return packet
}

func TestRewriteIpv4DestinationIcmpQuoteChecksums(t *testing.T) {
	peer := netip.MustParseAddr("10.55.12.34")
	nat := netip.MustParseAddr("169.254.7.9")
	for _, name := range []string{"udp", "udp-zero-checksum", "udp-options", "udp-first-fragment", "tcp-full", "tcp-minimum"} {
		t.Run(name, func(t *testing.T) {
			var quote []byte
			if name == "tcp-full" || name == "tcp-minimum" {
				quote = buildNatTestPacket(t, nat.String(), "1.1.1.1", &layers.TCP{SrcPort: 31415, DstPort: 443, SYN: true}, []byte("quoted bytes"))
			} else {
				quote = buildNatTestPacket(t, nat.String(), "1.1.1.1", &layers.UDP{SrcPort: 31415, DstPort: 53}, []byte("quoted bytes"))
			}
			if name == "udp-zero-checksum" {
				binary.BigEndian.PutUint16(quote[26:28], 0)
			}
			if name == "udp-options" {
				quote = append(append(append([]byte{}, quote[:20]...), 1, 1, 1, 1), quote[20:]...)
				quote[0] = 0x46
				binary.BigEndian.PutUint16(quote[2:4], uint16(len(quote)))
			}
			if name == "udp-first-fragment" {
				binary.BigEndian.PutUint16(quote[6:8], 0x2000)
			}
			headerSize := int(quote[0]&15) * 4
			binary.BigEndian.PutUint16(quote[10:12], 0)
			binary.BigEndian.PutUint16(quote[10:12], checksumFinish(checksumAdd(0, quote[:headerSize])))
			fullQuote := bytes.Clone(quote)
			if name == "tcp-minimum" {
				// RFC 792 requires only the first eight transport bytes. The
				// original total length intentionally exceeds the quotation.
				quote = quote[:headerSize+8]
			}
			for _, errorType := range []byte{3, 11, 12} {
				packet := natTestIcmpError(quote, errorType, 0, nat)
				if !RewriteIpv4Destination(packet, peer) {
					t.Fatalf("ICMP type %d return NAT refused", errorType)
				}
				got := packet[28:]
				if !bytes.Equal(got[12:16], peer.AsSlice()) {
					t.Fatalf("type %d quoted source not restored", errorType)
				}
				assertNatChecksums(t, packet, "ICMP envelope")
				if name == "tcp-minimum" {
					if checksumFinish(checksumAdd(0, got[:headerSize])) != 0 || !bytes.Equal(got[headerSize:], fullQuote[headerSize:headerSize+8]) {
						t.Fatal("minimal TCP quote checksum or first eight bytes changed")
					}
				} else {
					assertNatChecksums(t, got, "quotation")
					if name == "udp-zero-checksum" && binary.BigEndian.Uint16(got[headerSize+6:headerSize+8]) != 0 {
						t.Fatal("absent UDP checksum became computed")
					}
				}
			}
		})
	}
}

func TestRewriteIpv4DestinationRejectsUnusableIcmpQuoteBeforeMutation(t *testing.T) {
	peer := netip.MustParseAddr("10.55.12.34")
	nat := netip.MustParseAddr("169.254.7.9")
	quote := buildNatTestPacket(t, nat.String(), "1.1.1.1", &layers.UDP{SrcPort: 31415, DstPort: 53}, nil)
	for _, name := range []string{"missing-icmp", "short-ip", "bad-version", "short-options", "short-udp", "inner-total-too-short", "foreign-source", "noninitial-inner-fragment", "short-outer-total", "outer-total-exceeds-buffer"} {
		t.Run(name, func(t *testing.T) {
			packet := natTestIcmpError(quote, 3, 3, nat)
			switch name {
			case "missing-icmp":
				packet = packet[:20]
			case "short-ip":
				packet = packet[:47]
			case "bad-version":
				packet[28] = 0x65
			case "short-options":
				packet[28] = 0x4f
			case "short-udp":
				packet = packet[:55]
			case "inner-total-too-short":
				binary.BigEndian.PutUint16(packet[30:32], 20)
			case "foreign-source":
				copy(packet[40:44], []byte{169, 254, 7, 10})
			case "noninitial-inner-fragment":
				binary.BigEndian.PutUint16(packet[34:36], 1)
			case "short-outer-total":
				// Bytes beyond the enclosing IP length are not a quotation.
				binary.BigEndian.PutUint16(packet[2:4], 47)
			case "outer-total-exceeds-buffer":
				binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)+8))
			}
			before := bytes.Clone(packet)
			if RewriteIpv4Destination(packet, peer) {
				t.Fatal("unusable ICMP error was accepted")
			}
			if !bytes.Equal(packet, before) {
				t.Fatal("rejected ICMP error was partially mutated")
			}
		})
	}
}

func TestRewriteIpv4DestinationIcmpEchoAndOuterFragments(t *testing.T) {
	peer := netip.MustParseAddr("10.55.12.34")
	nat := netip.MustParseAddr("169.254.7.9")
	for _, name := range []string{"echo", "later-fragment", "first-fragment"} {
		t.Run(name, func(t *testing.T) {
			quote := buildNatTestPacket(t, nat.String(), "1.1.1.1", &layers.UDP{SrcPort: 31415, DstPort: 53}, nil)
			packet := natTestIcmpError(quote, 3, 3, nat)
			switch name {
			case "echo":
				packet[20], packet[21] = 0, 0
			case "later-fragment":
				binary.BigEndian.PutUint16(packet[6:8], 8)
			case "first-fragment":
				binary.BigEndian.PutUint16(packet[6:8], 0x2000)
			}
			binary.BigEndian.PutUint16(packet[10:12], 0)
			binary.BigEndian.PutUint16(packet[10:12], checksumFinish(checksumAdd(0, packet[:20])))
			binary.BigEndian.PutUint16(packet[22:24], 0)
			binary.BigEndian.PutUint16(packet[22:24], checksumFinish(checksumAdd(0, packet[20:])))
			body := bytes.Clone(packet[20:])
			if !RewriteIpv4Destination(packet, peer) {
				t.Fatal("return NAT refused")
			}
			if name != "first-fragment" && !bytes.Equal(packet[20:], body) {
				t.Fatal("echo or later-fragment payload was interpreted as an ICMP error quote")
			}
			if name == "first-fragment" && !bytes.Equal(packet[40:44], peer.AsSlice()) {
				t.Fatal("first-fragment ICMP quotation was not restored")
			}
			assertNatChecksums(t, packet, "rewritten packet")
		})
	}
}

func TestRewriteIpv4DestinationIcmpQuoteDoesNotAllocate(t *testing.T) {
	peer := netip.MustParseAddr("10.55.12.34")
	nat := netip.MustParseAddr("169.254.7.9")
	quote := buildNatTestPacket(t, nat.String(), "1.1.1.1", &layers.UDP{SrcPort: 31415, DstPort: 53}, nil)
	original := natTestIcmpError(quote, 3, 3, nat)
	packet := bytes.Clone(original)
	if allocations := testing.AllocsPerRun(100, func() {
		copy(packet, original)
		if !RewriteIpv4Destination(packet, peer) {
			panic("valid ICMP quotation was refused")
		}
	}); allocations != 0 {
		t.Fatalf("ICMP return NAT allocated %g times, want zero", allocations)
	}
}
