package connect

// Application standards: positive detectors for non-web protocols whose
// payloads look fully encrypted and would otherwise be dropped by the
// encrypted-traffic heuristic. Each detector is a fixed-format header or a
// cross-packet invariant from a public specification, so no probabilistic
// judgement is involved:
//
//   - WireGuard (whitepaper 5.4.2, 5.4.6): a 148-byte handshake initiation
//     (type 1, three reserved zero bytes) confirmed by a transport datagram
//     (type 4, reserved zero, 16-byte aligned); or, for a flow re-inspected
//     mid-stream, two transport datagrams with the same receiver index and a
//     strictly increasing little-endian counter.
//   - OpenVPN (protocol documentation, ssl_pkt.h): a hard reset from the client
//     (opcode 7 or 10, key id 0) confirmed by a control/ack packet repeating its
//     8-byte session id; or two P_DATA_V2 packets with the same key id and peer
//     id. Over TCP each packet carries a 2-byte length prefix.
//   - RTMP (Adobe RTMP 1.0 5.2): the first TCP payload is C0 version 3 and C1
//     whose bytes 5-8 are the zero word.
//   - Levin (Monero levin_base.h): the first TCP payload is a bucket head with
//     the levin signature, protocol version 1 and a body no larger than the
//     levin packet limit.
//   - RakNet (MessageIdentifiers.h, RakPeer.cpp): an offline message (unconnected
//     ping, open connection request 1 or 2) carrying the 16-byte offline magic.
//   - Ethereum devp2p discovery v4 and RLPx: a keccak256 hash over the packet,
//     and an on-curve secp256k1 ephemeral key in a length-exact auth message
//     (ip_security_appstandard_ethereum.go).
//
// The Noise "WA" framing used by WhatsApp on 5222 is not implemented: its bytes
// must be confirmed against a capture first (IPSECURITY-UPDATE4 6.2).
//
// None of these is reached unless the positive BitTorrent signatures have
// already failed on the packet, and a match never lets a BitTorrent payload
// through: the bytes after the recognized header are checked for every
// BitTorrent signature, and the flow keeps checking BitTorrent signatures for
// the rest of its inspection budget (see dmcaFlowState.advance).

import (
	"bytes"
	"encoding/binary"
)

// AppStandardSettings selects which non-web application protocols are
// positively recognized (and therefore allowed through the encrypted-traffic
// heuristic). Use DefaultAppStandardSettings for the defaults.
type AppStandardSettings struct {
	// Enabled is the master switch. When false no application standard is
	// recognized.
	Enabled   bool
	WireGuard bool
	OpenVpn   bool
	Rtmp      bool
	Levin     bool
	RakNet    bool
	// devp2p discovery v4 (udp)
	EthereumDiscv4 bool
	// devp2p RLPx auth (tcp)
	EthereumRlpx bool
}

func DefaultAppStandardSettings() *AppStandardSettings {
	return &AppStandardSettings{
		Enabled:        true,
		WireGuard:      true,
		OpenVpn:        true,
		Rtmp:           true,
		Levin:          true,
		RakNet:         true,
		EthereumDiscv4: true,
		EthereumRlpx:   true,
	}
}

const (
	wireGuardMessageInitiation = 1
	wireGuardMessageTransport  = 4
	wireGuardInitiationLength  = 148
	// header (16) + an empty payload's tag (16)
	wireGuardMinTransportLength = 32
	// a later transport datagram on the same receiver must advance the counter
	// by less than this; the sender's counter is strictly monotonic
	wireGuardMaxCounterAdvance = 1024 * 1024

	openVpnOpcodeControlV1            = 4
	openVpnOpcodeAckV1                = 5
	openVpnOpcodeHardResetClientV2    = 7
	openVpnOpcodeDataV2               = 9
	openVpnOpcodeHardResetClientV3    = 10
	openVpnOpcodeControlWkcV1         = 11
	openVpnSessionIdLength            = 8
	openVpnMinControlLength           = 1 + openVpnSessionIdLength + 5
	openVpnMinDataV2Length            = 1 + 3 + 4 + 16
	openVpnTcpLengthPrefix            = 2
	openVpnTcpMaxPacketLength         = 64 * 1024
	rtmpVersion                       = 3
	rtmpMinHandshakePrefix            = 9
	levinHeadLength                   = 33
	levinProtocolVersion1             = 1
	levinMaxPacketSize                = 100 * 1000 * 1000
	rakNetIdUnconnectedPing           = 0x01
	rakNetIdOpenConnectionRequest1    = 0x05
	rakNetIdOpenConnectionRequest2    = 0x07
	rakNetUnconnectedPingMagicOffset  = 9
	rakNetOpenConnectionMagicOffset   = 1
	rakNetOfflineMagicLength          = 16
	appStandardMaxCandidatePacketSize = 64 * 1024
)

var levinSignature = []byte{0x01, 0x21, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01}

var rakNetOfflineMagic = []byte{
	0x00, 0xff, 0xff, 0x00, 0xfe, 0xfe, 0xfe, 0xfe,
	0xfd, 0xfd, 0xfd, 0xfd, 0x12, 0x34, 0x56, 0x78,
}

type appCandidateKind uint8

const (
	appCandidateNone appCandidateKind = iota
	appCandidateWireGuardInitiation
	appCandidateWireGuardTransport
	appCandidateOpenVpnReset
	appCandidateOpenVpnData
)

// appCandidate is the small per-flow state a two-packet detector keeps between
// its opening and confirming packets.
type appCandidate struct {
	kind     appCandidateKind
	sid      [openVpnSessionIdLength]byte
	receiver uint32
	counter  uint64
	keyId    uint8
}

type appStandardDetector struct {
	settings *AppStandardSettings
}

// newAppStandardDetector accepts nil settings, which disable every detector.
func newAppStandardDetector(settings *AppStandardSettings) *appStandardDetector {
	return &appStandardDetector{
		settings: settings,
	}
}

func (self *appStandardDetector) enabled() bool {
	return self != nil && self.settings != nil && self.settings.Enabled
}

// match recognizes a single-packet application standard. first is true for
// the flow's first payload-bearing packet; the TCP detectors only match there.
// payload is the complete payload, since the Ethereum invariants cover every
// byte. It returns the reason and the offset where the recognized header ends.
func (self *appStandardDetector) match(ipPath *IpPath, payload []byte, first bool) (SecurityPolicyReason, int, bool) {
	if !self.enabled() {
		return SecurityPolicyReasonUnknown, 0, false
	}
	switch ipPath.Protocol {
	case IpProtocolTcp:
		if !first {
			return SecurityPolicyReasonUnknown, 0, false
		}
		if self.settings.Rtmp && isRtmpHandshake(payload) {
			return SecurityPolicyReasonAllowRtmp, rtmpMinHandshakePrefix, true
		}
		if self.settings.Levin && isLevinHead(payload) {
			return SecurityPolicyReasonAllowLevin, levinHeadLength, true
		}
		if self.settings.EthereumRlpx {
			if headerEnd, ok := ethereumRlpxAuth(payload); ok {
				return SecurityPolicyReasonAllowEthereumRlpx, headerEnd, true
			}
		}
	case IpProtocolUdp:
		if self.settings.RakNet {
			if headerEnd, ok := rakNetOfflineMessage(payload); ok {
				return SecurityPolicyReasonAllowRakNet, headerEnd, true
			}
		}
		if self.settings.EthereumDiscv4 {
			if headerEnd, ok := ethereumDiscv4Packet(payload); ok {
				return SecurityPolicyReasonAllowEthereumDiscv4, headerEnd, true
			}
		}
	}
	return SecurityPolicyReasonUnknown, 0, false
}

// open reports whether payload opens a two-packet candidate.
func (self *appStandardDetector) open(ipPath *IpPath, payload []byte) (appCandidate, bool) {
	if !self.enabled() {
		return appCandidate{}, false
	}
	switch ipPath.Protocol {
	case IpProtocolUdp:
		if self.settings.WireGuard {
			if candidate, ok := openWireGuard(payload); ok {
				return candidate, true
			}
		}
		if self.settings.OpenVpn {
			if candidate, ok := openOpenVpn(payload); ok {
				return candidate, true
			}
		}
	case IpProtocolTcp:
		if self.settings.OpenVpn {
			if packet, ok := openVpnTcpPacket(payload); ok {
				if candidate, ok := openOpenVpn(packet); ok {
					return candidate, true
				}
			}
		}
	}
	return appCandidate{}, false
}

// confirm reports whether payload confirms candidate.
func (self *appStandardDetector) confirm(candidate *appCandidate, ipPath *IpPath, payload []byte) (SecurityPolicyReason, bool) {
	if !self.enabled() {
		return SecurityPolicyReasonUnknown, false
	}
	switch candidate.kind {
	case appCandidateWireGuardInitiation:
		if self.settings.WireGuard && ipPath.Protocol == IpProtocolUdp && isWireGuardTransport(payload) {
			return SecurityPolicyReasonAllowWireGuard, true
		}
	case appCandidateWireGuardTransport:
		if self.settings.WireGuard && ipPath.Protocol == IpProtocolUdp && isWireGuardTransport(payload) {
			receiver := binary.LittleEndian.Uint32(payload[4:8])
			counter := binary.LittleEndian.Uint64(payload[8:16])
			if receiver == candidate.receiver && candidate.counter < counter &&
				counter-candidate.counter < wireGuardMaxCounterAdvance {
				return SecurityPolicyReasonAllowWireGuard, true
			}
		}
	case appCandidateOpenVpnReset, appCandidateOpenVpnData:
		if !self.settings.OpenVpn {
			return SecurityPolicyReasonUnknown, false
		}
		packet := payload
		if ipPath.Protocol == IpProtocolTcp {
			var ok bool
			if packet, ok = openVpnTcpPacket(payload); !ok {
				return SecurityPolicyReasonUnknown, false
			}
		}
		if confirmOpenVpn(candidate, packet) {
			return SecurityPolicyReasonAllowOpenVpn, true
		}
	}
	return SecurityPolicyReasonUnknown, false
}

// isRtmpHandshake: RTMP 1.0 5.2.2-5.2.3, C0 is the version byte 3 and C1 is a
// 4-byte time followed by a 4-byte zero word. The digest handshake variant puts
// a version in bytes 5-8 and is not matched.
func isRtmpHandshake(b []byte) bool {
	return rtmpMinHandshakePrefix <= len(b) &&
		b[0] == rtmpVersion &&
		b[5] == 0 && b[6] == 0 && b[7] == 0 && b[8] == 0
}

// isLevinHead: levin_base.h bucket head, little-endian: signature (8), cb (8),
// have_to_return (1), command (4), return_code (4), flags (4), protocol_version
// (4) = 33 bytes.
func isLevinHead(b []byte) bool {
	if len(b) < levinHeadLength || !bytes.Equal(b[0:8], levinSignature) {
		return false
	}
	if binary.LittleEndian.Uint32(b[29:33]) != levinProtocolVersion1 {
		return false
	}
	return binary.LittleEndian.Uint64(b[8:16]) <= levinMaxPacketSize
}

// rakNetOfflineMessage: an unconnected ping carries the magic after its 8-byte
// time; the open connection requests carry it right after the message id.
func rakNetOfflineMessage(b []byte) (int, bool) {
	if len(b) == 0 {
		return 0, false
	}
	offset := 0
	switch b[0] {
	case rakNetIdOpenConnectionRequest1, rakNetIdOpenConnectionRequest2:
		offset = rakNetOpenConnectionMagicOffset
	case rakNetIdUnconnectedPing:
		offset = rakNetUnconnectedPingMagicOffset
	default:
		return 0, false
	}
	end := offset + rakNetOfflineMagicLength
	if len(b) < end || !bytes.Equal(b[offset:end], rakNetOfflineMagic) {
		return 0, false
	}
	return end, true
}

func isWireGuardTransport(b []byte) bool {
	return wireGuardMinTransportLength <= len(b) &&
		len(b) <= appStandardMaxCandidatePacketSize &&
		len(b)%16 == 0 &&
		b[0] == wireGuardMessageTransport &&
		b[1] == 0 && b[2] == 0 && b[3] == 0
}

func openWireGuard(b []byte) (appCandidate, bool) {
	if len(b) == wireGuardInitiationLength &&
		b[0] == wireGuardMessageInitiation &&
		b[1] == 0 && b[2] == 0 && b[3] == 0 {
		return appCandidate{kind: appCandidateWireGuardInitiation}, true
	}
	if isWireGuardTransport(b) {
		return appCandidate{
			kind:     appCandidateWireGuardTransport,
			receiver: binary.LittleEndian.Uint32(b[4:8]),
			counter:  binary.LittleEndian.Uint64(b[8:16]),
		}, true
	}
	return appCandidate{}, false
}

// openVpnTcpPacket strips the TCP length prefix of the first packet in a
// segment. Segments may coalesce packets, so the declared length must fit but
// need not fill the segment.
func openVpnTcpPacket(b []byte) ([]byte, bool) {
	if len(b) < openVpnTcpLengthPrefix {
		return nil, false
	}
	packetLength := int(binary.BigEndian.Uint16(b[0:2]))
	if packetLength < openVpnMinControlLength || len(b) < openVpnTcpLengthPrefix+packetLength {
		return nil, false
	}
	return b[openVpnTcpLengthPrefix : openVpnTcpLengthPrefix+packetLength], true
}

func openOpenVpn(b []byte) (appCandidate, bool) {
	if len(b) == 0 {
		return appCandidate{}, false
	}
	opcode := b[0] >> 3
	keyId := b[0] & 0x07
	switch opcode {
	case openVpnOpcodeHardResetClientV2, openVpnOpcodeHardResetClientV3:
		// a hard reset always starts key id 0
		if keyId != 0 || len(b) < openVpnMinControlLength {
			return appCandidate{}, false
		}
		candidate := appCandidate{kind: appCandidateOpenVpnReset}
		copy(candidate.sid[:], b[1:1+openVpnSessionIdLength])
		return candidate, true
	case openVpnOpcodeDataV2:
		if len(b) < openVpnMinDataV2Length {
			return appCandidate{}, false
		}
		return appCandidate{
			kind:     appCandidateOpenVpnData,
			receiver: uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]),
			keyId:    keyId,
		}, true
	}
	return appCandidate{}, false
}

func confirmOpenVpn(candidate *appCandidate, b []byte) bool {
	if len(b) == 0 {
		return false
	}
	opcode := b[0] >> 3
	keyId := b[0] & 0x07
	switch candidate.kind {
	case appCandidateOpenVpnReset:
		switch opcode {
		case openVpnOpcodeControlV1, openVpnOpcodeAckV1, openVpnOpcodeHardResetClientV2,
			openVpnOpcodeHardResetClientV3, openVpnOpcodeControlWkcV1:
		default:
			return false
		}
		return openVpnMinControlLength <= len(b) &&
			bytes.Equal(b[1:1+openVpnSessionIdLength], candidate.sid[:])
	case appCandidateOpenVpnData:
		return opcode == openVpnOpcodeDataV2 &&
			keyId == candidate.keyId &&
			openVpnMinDataV2Length <= len(b) &&
			uint32(b[1])<<16|uint32(b[2])<<8|uint32(b[3]) == candidate.receiver
	}
	return false
}

// containsBittorrentSignature checks bytes carried after a recognized
// application header for every BitTorrent signature regardless of transport,
// so no BitTorrent payload can ride behind an application standard's header.
func containsBittorrentSignature(b []byte) bool {
	return hasBittorrentHandshake(b) ||
		hasHttpTrackerRequest(b) ||
		isDhtKrpc(b) ||
		isUdpTrackerConnect(b) ||
		utpV1CarriesHandshake(b)
}
