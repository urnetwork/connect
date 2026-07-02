package connect

// Web-standard payload classifier: a stateless detector for the encrypted web
// protocols that egress treats as the sanctioned fallback when the BitTorrent
// detector finds a flow that merely looks fully encrypted. Recognizing one of
// these on the opening packet is what keeps the entropy heuristic from dropping
// legitimate encrypted traffic.
//
// All checks are clean-room from the RFCs: TLS RFC 8446/5246, DTLS RFC 6347/9147,
// QUIC RFC 9000/9369, STUN RFC 5389. The matchers are pure functions of a single
// packet payload — there is NO per-flow state here. The DMCA detector remembers a
// match for the life of the flow; this component only answers "is this packet the
// opening of a recognized web standard?".

import (
	"encoding/binary"
)

// WebStandardSettings selects which web standards are recognized (and therefore
// allowed through the egress encrypted-traffic heuristic). Use
// DefaultWebStandardSettings for reasonable defaults.
type WebStandardSettings struct {
	// Enabled is the master switch. When false, match always returns false (no
	// web standard is recognized, so the encrypted heuristic is not vetoed).
	Enabled bool
	Tls     bool
	Dtls    bool
	Quic    bool
	Stun    bool
}

func DefaultWebStandardSettings() *WebStandardSettings {
	return &WebStandardSettings{
		Enabled: true,
		Tls:     true,
		Dtls:    true,
		Quic:    true,
		Stun:    true,
	}
}

type webStandardDetector struct {
	settings *WebStandardSettings
}

func newWebStandardDetector(settings *WebStandardSettings) *webStandardDetector {
	return &webStandardDetector{
		settings: settings,
	}
}

// match reports whether payload is the opening packet of a recognized, enabled
// web standard for ipPath's protocol.
func (self *webStandardDetector) match(ipPath *IpPath, payload []byte) bool {
	if !self.settings.Enabled {
		return false
	}
	switch ipPath.Protocol {
	case IpProtocolTcp:
		return self.settings.Tls && isTlsClientHello(payload)
	case IpProtocolUdp:
		return (self.settings.Dtls && isDtlsClientHello(payload)) ||
			(self.settings.Quic && isQuicLongHeader(payload)) ||
			(self.settings.Stun && isStun(payload))
	}
	return false
}

const (
	// DTLS uses 1's-complement version numbers (RFC 6347/9147).
	dtlsVersionMajor          = 0xFE
	dtlsVersion10Minor        = 0xFF
	dtlsVersion12Minor        = 0xFD
	stunMagicCookie           = 0x2112A442
	quicVersion1       uint32 = 0x00000001
	quicVersion2       uint32 = 0x6B3343CF
)

// TLS RFC 8446/5246 record: handshake content type, 0x03xx version, first
// handshake byte == ClientHello (0x01).
func isTlsClientHello(b []byte) bool {
	if len(b) < 6 {
		return false
	}
	if TlsContentTypeHandshake != b[0] {
		return false
	}
	if 0x03 != b[1] || 0x04 < b[2] {
		return false
	}
	return 0x01 == b[5]
}

// DTLS RFC 6347/9147 record (13-byte header) carrying a ClientHello (0x01).
func isDtlsClientHello(b []byte) bool {
	if len(b) < 14 {
		return false
	}
	if TlsContentTypeHandshake != b[0] {
		return false
	}
	if dtlsVersionMajor != b[1] {
		return false
	}
	if dtlsVersion10Minor != b[2] && dtlsVersion12Minor != b[2] {
		return false
	}
	return 0x01 == b[13]
}

// QUIC RFC 9000/9369 long header: header-form + fixed bit set, recognized version.
func isQuicLongHeader(b []byte) bool {
	if len(b) < 5 {
		return false
	}
	if 0xc0 != b[0]&0xc0 {
		return false
	}
	version := binary.BigEndian.Uint32(b[1:5])
	switch {
	case quicVersion1 == version, quicVersion2 == version:
		return true
	case 0x00000000 == version:
		// version negotiation
		return true
	case 0x0a0a0a0a == version&0x0f0f0f0f:
		// forced-negotiation greasing pattern (RFC 9000 section 15)
		return true
	case 0xff000000 == version&0xffffff00:
		// IETF drafts
		return true
	}
	return false
}

// STUN RFC 5389: two leading zero bits, message length a multiple of 4, and the
// fixed magic cookie at offset 4. Covers WebRTC connectivity checks and TURN.
func isStun(b []byte) bool {
	if len(b) < 20 {
		return false
	}
	if 0 != b[0]&0xc0 {
		return false
	}
	if 0 != binary.BigEndian.Uint16(b[2:4])%4 {
		return false
	}
	return stunMagicCookie == binary.BigEndian.Uint32(b[4:8])
}
