package connect

// In-place IPv4 address substitution for NAT, with incremental checksum repair.
//
// The server-side WireGuard proxy needs this: a wg peer's tunnel address is
// allocated from a durable pool and written into the peer's config, so it is
// stable across sessions AND identical at every provider in the peer's window.
// Threading it to the egress unmodified lets colluding providers tell that
// those flows belong to one client -- precisely the property the multi-provider
// window otherwise provides. The proxy substitutes a per-device address from
// `TakeLocalIpv4Address` on the way out and restores the peer's address on the
// way back, which puts the wg path on the same footing as the app path, whose
// tun address is per session and drawn from the same pool.
//
// Incremental repair rather than recomputation, for two reasons. It is the
// standard NAT technique (RFC 1624), and it is O(1) in the packet rather than
// O(n) -- this runs on every packet in both directions on a server carrying
// many peers. Recomputing a transport checksum would also require walking a
// payload the proxy has no other reason to touch.

import (
	"bytes"
	"encoding/binary"
	"net/netip"
)

// ipv4SourceOffset and ipv4DestinationOffset are fixed in the IPv4 base header;
// options extend the header after them, so neither moves.
const (
	ipv4SourceOffset      = 12
	ipv4DestinationOffset = 16
)

// The transport protocols whose checksum covers an IP pseudo-header, and which
// therefore have to be repaired when an address changes. Declared here rather
// than beside the ipProtocolNumber block because this is the complete list the
// rewrite depends on, and a protocol missing from it is a silent corruption.
const (
	ipProtocolNumberDccp    = ipProtocolNumber(33)
	ipProtocolNumberUdpLite = ipProtocolNumber(136)
)

// incrementalChecksum applies RFC 1624 eqn. 3 -- HC' = ~(~HC + ~m + m') -- for
// an arbitrary-length aligned field replacement.
//
// The one's-complement arithmetic is what makes a NAT rewrite cheap: the
// checksum of the whole packet can be corrected knowing only the bytes that
// changed. `old` and `new` must be the same even length.
func incrementalChecksum(checksum uint16, old []byte, new []byte) uint16 {
	sum := uint32(^checksum)
	for i := 0; i+1 < len(old); i += 2 {
		sum += uint32(^binary.BigEndian.Uint16(old[i:i+2])) & 0xffff
	}
	for i := 0; i+1 < len(new); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(new[i : i+2]))
	}
	for 0xffff < sum {
		sum = (sum >> 16) + (sum & 0xffff)
	}
	return ^uint16(sum)
}

// RewriteIpv4Source replaces an IPv4 packet's source address in place and
// repairs the IPv4 header checksum and, on a first fragment, the transport
// checksum. Reports whether the packet was rewritten.
//
// Returns false only when the packet is structurally unusable -- too short, not
// IPv4, or a pseudo-header transport whose header is truncated. A caller that
// drops on false is correct; a caller that forwards on false would be
// forwarding a stale checksum.
func RewriteIpv4Source(packet []byte, source netip.Addr) bool {
	return rewriteIpv4Addr(packet, ipv4SourceOffset, source)
}

// RewriteIpv4Destination is the return-path counterpart of
// `RewriteIpv4Source`. For ICMP errors it also restores the quoted packet's
// source, so the receiving socket can recognize the failed outbound packet.
func RewriteIpv4Destination(packet []byte, destination netip.Addr) bool {
	return rewriteIpv4Addr(packet, ipv4DestinationOffset, destination)
}

func rewriteIpv4Addr(packet []byte, offset int, addr netip.Addr) bool {
	if len(packet) < Ipv4HeaderSizeWithoutExtensions {
		return false
	}
	if packet[0]>>4 != 4 {
		return false
	}
	if !addr.Is4() {
		return false
	}
	headerSize := int(packet[0]&0x0f) * 4
	if headerSize < Ipv4HeaderSizeWithoutExtensions || len(packet) < headerSize {
		return false
	}

	next := addr.As4()
	var prior [4]byte
	copy(prior[:], packet[offset:offset+4])
	if bytes.Equal(prior[:], next[:]) {
		return true
	}

	fragmentOffset := binary.BigEndian.Uint16(packet[6:8]) & 0x1fff
	if offset == ipv4DestinationOffset && fragmentOffset == 0 &&
		ipProtocolNumber(packet[9]) == ipProtocolNumberIcmp4 &&
		!rewriteIpv4IcmpErrorQuote(packet, headerSize, prior[:], next[:]) {
		return false
	}

	headerChecksum := binary.BigEndian.Uint16(packet[10:12])
	binary.BigEndian.PutUint16(
		packet[10:12],
		incrementalChecksum(headerChecksum, prior[:], next[:]),
	)
	copy(packet[offset:offset+4], next[:])

	// Only the first fragment carries the transport header. A later fragment's
	// transport checksum lives in the first one and is already correct there,
	// so rewriting at this offset would corrupt payload bytes.
	if fragmentOffset != 0 {
		return true
	}

	transport := packet[headerSize:]
	switch ipProtocolNumber(packet[9]) {
	case ipProtocolNumberTcp:
		// the address is in the TCP pseudo-header, so the same delta applies
		if len(transport) < 18 {
			return false
		}
		checksum := binary.BigEndian.Uint16(transport[16:18])
		binary.BigEndian.PutUint16(
			transport[16:18],
			incrementalChecksum(checksum, prior[:], next[:]),
		)
	case ipProtocolNumberUdp:
		if len(transport) < 8 {
			return false
		}
		checksum := binary.BigEndian.Uint16(transport[6:8])
		// a zero UDP checksum means "not computed" and must stay zero; giving
		// it a value would claim a guarantee the sender never made
		if checksum == 0 {
			return true
		}
		updated := incrementalChecksum(checksum, prior[:], next[:])
		// 0 is reserved for "no checksum", so a computed zero is transmitted
		// as its equivalent all-ones form (RFC 768)
		if updated == 0 {
			updated = 0xffff
		}
		binary.BigEndian.PutUint16(transport[6:8], updated)
	case ipProtocolNumberUdpLite:
		// UDP-Lite's checksum also covers the pseudo-header, and unlike UDP a
		// zero value is illegal rather than "not computed" (RFC 3828).
		if len(transport) < 8 {
			return false
		}
		checksum := binary.BigEndian.Uint16(transport[6:8])
		binary.BigEndian.PutUint16(
			transport[6:8],
			incrementalChecksum(checksum, prior[:], next[:]),
		)
	case ipProtocolNumberDccp:
		if len(transport) < 8 {
			return false
		}
		checksum := binary.BigEndian.Uint16(transport[6:8])
		binary.BigEndian.PutUint16(
			transport[6:8],
			incrementalChecksum(checksum, prior[:], next[:]),
		)
	case ipProtocolNumberIcmp4:
		// ICMPv4's checksum covers the ICMP message only -- there is no
		// pseudo-header. Return-path errors have already had their quotation
		// and ICMP checksum repaired above; echo messages need no repair.
	default:
		// Everything else is header-only. The protocols whose checksum covers
		// a pseudo-header -- and therefore the address -- are enumerated above;
		// the rest either carry no checksum (ESP, AH, GRE) or checksum their
		// own bytes without the IP header (SCTP's CRC32c). Refusing here
		// instead would drop those protocols outright rather than NAT them,
		// which is a worse failure than the one it would be guarding against.
	}
	return true
}

// Locally generated UDP teardown errors bypass the provider-ingress ICMP
// intercept. Their quotation must follow the same NAT mapping as the envelope.
// Validate before mutating, and only inspect a bounded header prefix: an ICMP
// quotation need not contain the original payload (or even a TCP checksum).
func rewriteIpv4IcmpErrorQuote(packet []byte, headerSize int, prior, next []byte) bool {
	totalSize := int(binary.BigEndian.Uint16(packet[2:4]))
	if totalSize < headerSize+8 || len(packet) < totalSize {
		return false
	}
	icmp := packet[headerSize:totalSize]
	switch icmp[0] {
	case 3, 11, 12: // destination unreachable, time exceeded, parameter problem
	default:
		return true
	}
	quote := icmp[8:]
	if len(quote) < Ipv4HeaderSizeWithoutExtensions || quote[0]>>4 != 4 {
		return false
	}
	quoteHeaderSize := int(quote[0]&0x0f) * 4
	quoteTotalSize := int(binary.BigEndian.Uint16(quote[2:4]))
	if quoteHeaderSize < Ipv4HeaderSizeWithoutExtensions ||
		len(quote) < quoteHeaderSize+8 || quoteTotalSize < quoteHeaderSize+8 ||
		binary.BigEndian.Uint16(quote[6:8])&0x1fff != 0 ||
		!bytes.Equal(quote[ipv4SourceOffset:ipv4SourceOffset+4], prior) {
		return false
	}

	checksumOffset := -1
	protocol := ipProtocolNumber(quote[9])
	switch protocol {
	case ipProtocolNumberUdp, ipProtocolNumberUdpLite, ipProtocolNumberDccp:
		checksumOffset = quoteHeaderSize + 6
	case ipProtocolNumberTcp:
		if quoteHeaderSize+18 <= len(quote) && quoteHeaderSize+18 <= quoteTotalSize {
			checksumOffset = quoteHeaderSize + 16
		}
	}
	prefixSize := quoteHeaderSize + 8
	if prefixSize < checksumOffset+2 {
		prefixSize = checksumOffset + 2
	}
	var before [60 + 18]byte // maximum IPv4 header plus TCP checksum
	copy(before[:], quote[:prefixSize])
	binary.BigEndian.PutUint16(quote[10:12], incrementalChecksum(
		binary.BigEndian.Uint16(quote[10:12]), prior, next,
	))
	copy(quote[ipv4SourceOffset:ipv4SourceOffset+4], next)
	if checksumOffset >= 0 {
		checksum := binary.BigEndian.Uint16(quote[checksumOffset : checksumOffset+2])
		if protocol != ipProtocolNumberUdp || checksum != 0 {
			updated := incrementalChecksum(checksum, prior, next)
			if protocol == ipProtocolNumberUdp && updated == 0 {
				updated = 0xffff
			}
			binary.BigEndian.PutUint16(quote[checksumOffset:checksumOffset+2], updated)
		}
	}
	binary.BigEndian.PutUint16(icmp[2:4], incrementalChecksum(
		binary.BigEndian.Uint16(icmp[2:4]), before[:prefixSize], quote[:prefixSize],
	))
	return true
}
