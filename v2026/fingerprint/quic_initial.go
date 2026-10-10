package fingerprint

// quic_initial.go -- the parser and drift check for a QUIC Initial's first
// datagram, the udp half of the harness (EXTENDER.md A13, net_quic_version.go).
//
// what is read is only the unprotected long header: the form and fixed bits,
// the version, the packet type (its code differs between RFC 9000 version 1 and
// RFC 9369 version 2), the connection-id lengths, the token length and the
// datagram's padded length. the long-header VERSION is the drift signal the
// merged QuicVersionPolicy fixes -- a version 2 Initial is not decrypted by the
// GFW/TSPU version 1 parsers, so a silent regression to a version 1 first
// Initial is exactly the udp drift this gate must catch, and the version is in
// the clear.
//
// the ClientHello's transport parameters and the CRYPTO-frame layout live
// inside the Initial's aead-encrypted payload. reading them needs the Initial
// keys derived from the version salt and the destination connection id (RFC
// 9001 / RFC 9369), which this file deliberately does NOT implement yet; the
// README names it the next increment. the version, the packet-type code and the
// 1200-byte padding are observable without it and are the primary gate.

import (
	"encoding/binary"
	"fmt"
)

// RFC 9000 14.1: a client's Initial datagram is padded to at least this many
// bytes, so an on-path parser cannot tell a small Initial from a large one and
// amplification is bounded. a client that stopped padding is a fingerprint
// drift.
const quicMinInitialDatagramLength = 1200

// the two QUIC versions the carriers use: RFC 9000 (version 1) and RFC 9369
// (version 2). named so the parser and the diff read the same; the values are
// quic.Version1 and quic.Version2.
const (
	QuicVersion1 uint32 = 0x00000001
	QuicVersion2 uint32 = 0x6b3343cf
)

// QuicInitialFingerprint is the unprotected long-header shape of a QUIC
// Initial's first datagram.
type QuicInitialFingerprint struct {
	// whether the header-form bit (0x80) is set: a long header, as every
	// Initial is.
	LongHeader bool
	// whether the fixed bit (0x40) is set, as every non-greased QUIC packet
	// sets it.
	FixedBit bool
	// the long-header version (bytes 1..4), e.g. QuicVersion2.
	Version uint32
	// the decoded long-header packet type, read against the version because
	// the type codes differ between version 1 and version 2.
	PacketType QuicPacketType
	// the destination and source connection id lengths.
	DcidLength int
	ScidLength int
	// the Initial token length (0 for a fresh first Initial, non-zero only
	// after a Retry).
	TokenLength int
	// the whole first datagram's length, which an Initial pads to at least
	// quicMinInitialDatagramLength.
	DatagramLength int
}

// QuicPacketType is a long-header packet type, decoded against the version.
type QuicPacketType string

const (
	QuicPacketInitial   QuicPacketType = "initial"
	QuicPacketZeroRtt   QuicPacketType = "0-rtt"
	QuicPacketHandshake QuicPacketType = "handshake"
	QuicPacketRetry     QuicPacketType = "retry"
	QuicPacketUnknown   QuicPacketType = "unknown"
)

// ParseQuicInitial parses the unprotected long header of a QUIC Initial's first
// datagram. a datagram too short or not a long header is a named error, never a
// panic.
func ParseQuicInitial(datagram []byte) (*QuicInitialFingerprint, error) {
	// first byte, version (4), dcid len (1): the minimum before a connection id.
	if len(datagram) < 6 {
		return nil, fmt.Errorf("malformed quic initial: datagram too short (%d bytes)", len(datagram))
	}
	firstByte := datagram[0]
	fingerprint := &QuicInitialFingerprint{
		LongHeader:     firstByte&0x80 != 0,
		FixedBit:       firstByte&0x40 != 0,
		Version:        binary.BigEndian.Uint32(datagram[1:5]),
		DatagramLength: len(datagram),
	}
	if !fingerprint.LongHeader {
		return nil, fmt.Errorf("malformed quic initial: not a long header (first byte %02x)", firstByte)
	}
	fingerprint.PacketType = quicPacketType(fingerprint.Version, (firstByte&0x30)>>4)

	offset := 5
	dcidLength := int(datagram[offset])
	offset += 1
	fingerprint.DcidLength = dcidLength
	offset += dcidLength
	if len(datagram) < offset+1 {
		return nil, fmt.Errorf("malformed quic initial: truncated before source connection id")
	}
	scidLength := int(datagram[offset])
	offset += 1
	fingerprint.ScidLength = scidLength
	offset += scidLength

	// the token length follows the source connection id, as a varint, only on
	// an Initial.
	if fingerprint.PacketType == QuicPacketInitial {
		tokenLength, read, ok := readQuicVarint(datagram[min(offset, len(datagram)):])
		if !ok {
			return nil, fmt.Errorf("malformed quic initial: truncated token length")
		}
		offset += read
		fingerprint.TokenLength = int(tokenLength)
		_ = offset
	}
	return fingerprint, nil
}

// quicPacketType decodes the two long-header type bits against the version: RFC
// 9369 (version 2) renumbers the types relative to RFC 9000 (version 1), which
// is itself a fingerprint of the version.
func quicPacketType(version uint32, typeBits byte) QuicPacketType {
	if version == QuicVersion2 {
		switch typeBits {
		case 0x00:
			return QuicPacketRetry
		case 0x01:
			return QuicPacketInitial
		case 0x02:
			return QuicPacketZeroRtt
		case 0x03:
			return QuicPacketHandshake
		}
		return QuicPacketUnknown
	}
	// version 1 and, for this harness's purposes, any other version follow RFC
	// 9000's numbering.
	switch typeBits {
	case 0x00:
		return QuicPacketInitial
	case 0x01:
		return QuicPacketZeroRtt
	case 0x02:
		return QuicPacketHandshake
	case 0x03:
		return QuicPacketRetry
	}
	return QuicPacketUnknown
}

// readQuicVarint reads an RFC 9000 16 variable-length integer, returning the
// value, the bytes consumed, and whether it parsed.
func readQuicVarint(b []byte) (value uint64, read int, ok bool) {
	if len(b) == 0 {
		return 0, 0, false
	}
	length := 1 << (b[0] >> 6)
	if len(b) < length {
		return 0, 0, false
	}
	value = uint64(b[0] & 0x3f)
	for i := 1; i < length; i += 1 {
		value = value<<8 | uint64(b[i])
	}
	return value, length, true
}

// QuicDiffOptions says what Initial a dial should present: its version (the
// first of the policy's offer) and the minimum padded datagram length.
type QuicDiffOptions struct {
	// the long-header version the first Initial must carry. zero skips the
	// check.
	ExpectedVersion uint32
	// the least the first datagram must be padded to. zero defaults to the
	// RFC 9000 minimum of 1200.
	MinDatagramLength int
}

// DiffQuicInitial returns every way a captured Initial drifts from the policy's
// expected shape: a long header with the fixed bit, the Initial packet type of
// the expected version, and the 1200-byte padding. an empty result is a clean
// match.
func DiffQuicInitial(got *QuicInitialFingerprint, opts QuicDiffOptions) []Drift {
	minDatagramLength := opts.MinDatagramLength
	if minDatagramLength == 0 {
		minDatagramLength = quicMinInitialDatagramLength
	}
	var drifts []Drift
	add := func(field string, want string, gotValue string) {
		drifts = append(drifts, Drift{Field: field, Want: want, Got: gotValue})
	}
	if !got.LongHeader {
		add("quic_long_header", "true", "false")
	}
	if !got.FixedBit {
		add("quic_fixed_bit", "true", "false")
	}
	if opts.ExpectedVersion != 0 && got.Version != opts.ExpectedVersion {
		add("quic_version", fmt.Sprintf("%08x", opts.ExpectedVersion), fmt.Sprintf("%08x", got.Version))
	}
	if got.PacketType != QuicPacketInitial {
		add("quic_packet_type", string(QuicPacketInitial), string(got.PacketType))
	}
	if got.DatagramLength < minDatagramLength {
		add("quic_initial_padding", fmt.Sprintf(">=%d", minDatagramLength), fmt.Sprintf("%d", got.DatagramLength))
	}
	return drifts
}
