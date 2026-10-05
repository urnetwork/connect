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
//   - WhatsApp's Noise transport on TCP 5222 and 443: the connection header
//     'W','A' with two version bytes, after an optional edge routing prefix,
//     then the client's first Noise frame: a 3-byte length equal to the
//     length of the HandshakeMessage protobuf it carries, whose clientHello
//     starts with the 32-byte ephemeral key (whatsAppStream). The prefix may
//     span several segments.
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
	// WhatsApp's Noise transport (tcp 5222 and 443)
	WhatsApp bool
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
		WhatsApp:       true,
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
	appCandidateWhatsApp
)

// appCandidate is the small per-flow state a two-packet detector keeps between
// its opening and confirming packets.
type appCandidate struct {
	kind     appCandidateKind
	sid      [openVpnSessionIdLength]byte
	receiver uint32
	counter  uint64
	keyId    uint8
	// the progress through a WhatsApp stream prefix that spans segments
	whatsApp whatsAppStream
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
		if self.settings.WhatsApp && isWhatsAppPort(ipPath.DestinationPort) {
			if headerEnd, ok := whatsAppNoise(payload); ok {
				return SecurityPolicyReasonAllowWhatsApp, headerEnd, true
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

// Reports whether payload opens a candidate: the opener of a two-packet
// standard, or a WhatsApp stream prefix that continues in later segments, which
// only the flow's first payload (first) can start.
func (self *appStandardDetector) open(ipPath *IpPath, payload []byte, first bool) (appCandidate, bool) {
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
		if self.settings.WhatsApp && first && isWhatsAppPort(ipPath.DestinationPort) {
			candidate := appCandidate{kind: appCandidateWhatsApp}
			if progress, _ := candidate.whatsApp.consume(payload); progress == whatsAppNeedMore {
				return candidate, true
			}
		}
	}
	return appCandidate{}, false
}

// Reports whether payload confirms candidate. A WhatsApp candidate whose prefix
// continues in a later segment keeps its progress in candidate and returns
// SecurityPolicyReasonInspecting.
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
	case appCandidateWhatsApp:
		if self.settings.WhatsApp && ipPath.Protocol == IpProtocolTcp {
			switch progress, _ := candidate.whatsApp.consume(payload); progress {
			case whatsAppMatched:
				return SecurityPolicyReasonAllowWhatsApp, true
			case whatsAppNeedMore:
				return SecurityPolicyReasonInspecting, false
			}
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

// WhatsApp's chat transport is Noise Pipes over TCP (Meta's WhatsApp
// encryption whitepaper: Curve25519, AES-GCM and SHA256 from the Noise
// Protocol Framework). WhatsApp publishes no wire format, so the client's
// opening bytes are taken from public interoperable clients; no code from
// them is used:
//
//   - edge routing prefix, optional: 'E','D',0,1, the routing info length in
//     3 big-endian bytes, then the routing info (Baileys
//     src/Utils/noise-handler.ts introHeader; yowsup
//     yowsup/layers/noise/layer.py EDGE_HEADER). The nDPI traffic classifier
//     (src/lib/protocols/whatsapp.c) matches this prefix with a 2- or 4-byte
//     routing info, across packets.
//   - connection header, mixed into the handshake as the Noise prologue:
//     'W','A' and two version bytes. whatsmeow sends 'W','A',6,3 (socket
//     WAConnHeader: WAMagicValue 6, token.DictVersion 3) and starts
//     Noise_XX_25519_AESGCM_SHA256 with it as the prologue, as does Baileys
//     (src/Defaults NOISE_WA_HEADER); yowsup's mobile Noise layer sent
//     'W','A',4,0. Any version byte below 16 is accepted, so a version bump
//     does not break the detector.
//   - frames: a 3-byte big-endian length, then the frame (whatsmeow
//     socket/framesocket.go SendFrame, Baileys encodeFrame). The client's
//     first frame is a HandshakeMessage protobuf with only clientHello (field
//     2) set, and the ClientHello's first field is the 32-byte X25519
//     ephemeral key (field 1) that opens the Noise XX and IK handshakes; the
//     static key, payload and newer fields follow it (whatsmeow handshake.go
//     and proto/waWa6, Baileys src/Socket/socket.ts, and for the mobile IK
//     and XX handshakes consonance handshake.py and its wa20 protobuf).
//
// The frame length must equal the protobuf's own length, so two independent
// length fields agree, and the key must be complete. The frame may continue
// past the segment, since a large hello spans segments, and the verdict never
// depends on where the segments end. whatsmeow and Baileys write the prefix
// and the hello at once; yowsup writes the edge header, the routing info and
// the header separately, so the parser keeps its progress across the flow's
// first segments. A retransmitted or reordered segment inside the prefix
// fails the parse, and the flow is then judged as without this detector. Like
// a two-packet confirmation, the segment that completes a split prefix is not
// searched behind the key; the BitTorrent signatures still run on every later
// packet within the budget.
const (
	// the native apps reach the chat service on whatsAppChatPort and on 443.
	// 443 is a privileged port, admitted before any flow state exists, so the
	// policy consults this detector only for 5222 today.
	whatsAppHttpsPort = 443
	// the routing info is a short server-issued blob; the bound is generous
	whatsAppMaxRoutingInfoLength = 1024
	whatsAppVersionLimit         = 16
	whatsAppHeaderLength         = 4
	whatsAppLengthSize           = 3
	whatsAppMaxVarintSize        = 3
	whatsAppEphemeralLength      = 32
	// the clientHello tag and a 1-byte length, the ephemeral tag and length,
	// and the key: whatsmeow's and Baileys' XX hello is exactly this
	whatsAppMinHelloFrameLength = 1 + 1 + 2 + whatsAppEphemeralLength
	whatsAppMaxHelloFrameLength = 64 * 1024
	// protobuf tags: the field number << 3 | wire type 2 (length-delimited)
	whatsAppClientHelloTag = 2<<3 | 2
	whatsAppEphemeralTag   = 1<<3 | 2
)

var (
	whatsAppEdgeHeader  = []byte{'E', 'D', 0, 1}
	whatsAppHeaderMagic = []byte{'W', 'A'}
)

// Reports whether a destination port is one the native apps reach the chat
// service on: whatsAppChatPort or whatsAppHttpsPort.
func isWhatsAppPort(port int) bool {
	return port == whatsAppChatPort || port == whatsAppHttpsPort
}

// The part of the stream prefix the parser reads next.
type whatsAppField uint8

const (
	whatsAppFieldStart whatsAppField = iota
	whatsAppFieldEdgeHeader
	whatsAppFieldRoutingLength
	whatsAppFieldRoutingInfo
	whatsAppFieldHeader
	whatsAppFieldFrameLength
	whatsAppFieldHelloTag
	whatsAppFieldHelloLength
	whatsAppFieldEphemeralHeader
	whatsAppFieldEphemeral
)

// How far the stream prefix has parsed: not WhatsApp's, continuing in a later
// segment, or complete through the ephemeral key.
type whatsAppProgress uint8

const (
	whatsAppMismatch whatsAppProgress = iota
	whatsAppNeedMore
	whatsAppMatched
)

// Parses the start of a WhatsApp client stream. Between segments it keeps only
// counters, never payload bytes.
type whatsAppStream struct {
	field whatsAppField
	// bytes of the current fixed-size field read so far
	index uint8
	// the encoded size of the clientHello length
	helloLengthSize uint8
	// the routing info length, then the routing bytes left to skip; the
	// clientHello length; the key bytes left to skip
	value       uint32
	frameLength uint32
}

// Parses the next segment of the stream. Once the ephemeral key is complete it
// returns whatsAppMatched and the offset in b where the key ends.
func (self *whatsAppStream) consume(b []byte) (whatsAppProgress, int) {
	for i := 0; i < len(b); {
		c := b[i]
		switch self.field {
		case whatsAppFieldStart:
			switch c {
			case whatsAppEdgeHeader[0]:
				self.field = whatsAppFieldEdgeHeader
			case whatsAppHeaderMagic[0]:
				self.field = whatsAppFieldHeader
			default:
				return whatsAppMismatch, 0
			}
			self.index = 1
			i += 1
		case whatsAppFieldEdgeHeader:
			if c != whatsAppEdgeHeader[self.index] {
				return whatsAppMismatch, 0
			}
			self.index += 1
			i += 1
			if int(self.index) == len(whatsAppEdgeHeader) {
				self.field = whatsAppFieldRoutingLength
				self.index = 0
			}
		case whatsAppFieldRoutingLength:
			self.value = self.value<<8 | uint32(c)
			self.index += 1
			i += 1
			if self.index == whatsAppLengthSize {
				if whatsAppMaxRoutingInfoLength < self.value {
					return whatsAppMismatch, 0
				}
				// value is now the routing bytes left to skip
				self.field = whatsAppFieldRoutingInfo
				self.index = 0
				if self.value == 0 {
					self.field = whatsAppFieldHeader
				}
			}
		case whatsAppFieldRoutingInfo:
			skip := min(len(b)-i, int(self.value))
			self.value -= uint32(skip)
			i += skip
			if self.value == 0 {
				self.field = whatsAppFieldHeader
			}
		case whatsAppFieldHeader:
			if int(self.index) < len(whatsAppHeaderMagic) {
				if c != whatsAppHeaderMagic[self.index] {
					return whatsAppMismatch, 0
				}
			} else if whatsAppVersionLimit <= c {
				// the two version bytes
				return whatsAppMismatch, 0
			}
			self.index += 1
			i += 1
			if self.index == whatsAppHeaderLength {
				self.field = whatsAppFieldFrameLength
				self.index = 0
			}
		case whatsAppFieldFrameLength:
			self.frameLength = self.frameLength<<8 | uint32(c)
			self.index += 1
			i += 1
			if self.index == whatsAppLengthSize {
				if self.frameLength < whatsAppMinHelloFrameLength || whatsAppMaxHelloFrameLength < self.frameLength {
					return whatsAppMismatch, 0
				}
				self.field = whatsAppFieldHelloTag
				self.index = 0
			}
		case whatsAppFieldHelloTag:
			if c != whatsAppClientHelloTag {
				return whatsAppMismatch, 0
			}
			i += 1
			self.field = whatsAppFieldHelloLength
		case whatsAppFieldHelloLength:
			// a canonical protobuf varint
			self.value |= uint32(c&0x7f) << (7 * self.helloLengthSize)
			self.helloLengthSize += 1
			i += 1
			if c&0x80 != 0 {
				if self.helloLengthSize == whatsAppMaxVarintSize {
					return whatsAppMismatch, 0
				}
				continue
			}
			if 1 < self.helloLengthSize && c == 0 {
				// an encoder never pads a varint with a zero byte
				return whatsAppMismatch, 0
			}
			// the frame holds the clientHello and nothing else
			if 1+uint32(self.helloLengthSize)+self.value != self.frameLength {
				return whatsAppMismatch, 0
			}
			self.field = whatsAppFieldEphemeralHeader
		case whatsAppFieldEphemeralHeader:
			expected := byte(whatsAppEphemeralTag)
			if self.index == 1 {
				expected = whatsAppEphemeralLength
			}
			if c != expected {
				return whatsAppMismatch, 0
			}
			self.index += 1
			i += 1
			if self.index == 2 {
				self.field = whatsAppFieldEphemeral
				self.index = 0
				self.value = whatsAppEphemeralLength
			}
		case whatsAppFieldEphemeral:
			skip := min(len(b)-i, int(self.value))
			self.value -= uint32(skip)
			i += skip
			if self.value == 0 {
				return whatsAppMatched, i
			}
		}
	}
	return whatsAppNeedMore, len(b)
}

// Recognizes a first payload that carries the whole stream prefix and returns
// the end of the ephemeral key.
func whatsAppNoise(b []byte) (int, bool) {
	var stream whatsAppStream
	progress, end := stream.consume(b)
	return end, progress == whatsAppMatched
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
