package connect

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"sync"

	"github.com/urnetwork/connect/protocol"
)

var errSmtpEncryptionRequired = errors.New("SMTP encryption required")

// SMTP routing and encryption policy is enforced before the general CFAA
// policy. Port 25 is deliberately local-only. Port 465 must begin with a TLS
// ClientHello; port 587 may send only bounded SMTP negotiation until STARTTLS
// is immediately followed by a TLS ClientHello.
const (
	smtpLocalPort                 = 25
	smtpImplicitTlsPort           = 465
	smtpStartTlsPort              = 587
	smtpTlsClientHelloPrefixBytes = 9
	smtpMaxNegotiationBytes       = 2048
	smtpMaxCommandLineBytes       = 510 // RFC 5321's 512 includes CRLF.
	smtpMaxFlowCount              = 1024
	smtpFlowEvictionSampleSize    = 32
)

type smtpEgressVerdict uint8

const (
	smtpEgressNotApplicable smtpEgressVerdict = iota
	smtpEgressAllow
	smtpEgressReject
)

func smtpRoutesLocally(ipPath *IpPath) bool {
	return ipPath != nil &&
		ipPath.Protocol == IpProtocolTcp &&
		ipPath.DestinationPort == smtpLocalPort
}

func smtpNeedsEncryptionInspection(ipPath *IpPath) bool {
	if ipPath == nil || ipPath.Protocol != IpProtocolTcp {
		return false
	}
	return ipPath.DestinationPort == smtpImplicitTlsPort ||
		ipPath.DestinationPort == smtpStartTlsPort
}

func smtpNeedsOrderedSend(ipPath *IpPath) bool {
	return smtpRoutesLocally(ipPath) || smtpNeedsEncryptionInspection(ipPath)
}

// smtpFlowKey is the exact outbound tuple. The IP version is explicit so an
// IPv4 flow cannot collide with an IPv4-mapped IPv6 flow.
type smtpFlowKey struct {
	// ownerId namespaces provider-side flows by the authenticated remote
	// client. Tunnel clients can reuse the same virtual IP address and TCP
	// tuple, so the provider must not let one client's verdict latch another
	// client's connection. Client-side guards use the zero Id.
	ownerId         Id
	sourceIp        [net.IPv6len]byte
	destinationIp   [net.IPv6len]byte
	sourcePort      uint16
	destinationPort uint16
	ipVersion       uint8
}

func smtpFlowKeyForOwnerPath(ownerId Id, ipPath *IpPath) (smtpFlowKey, bool) {
	if ipPath == nil ||
		ipPath.Protocol != IpProtocolTcp ||
		ipPath.SourcePort < 0 || 65535 < ipPath.SourcePort ||
		ipPath.DestinationPort < 0 || 65535 < ipPath.DestinationPort {
		return smtpFlowKey{}, false
	}

	var sourceIp net.IP
	var destinationIp net.IP
	switch ipPath.Version {
	case 4:
		sourceIp = ipPath.SourceIp.To4()
		destinationIp = ipPath.DestinationIp.To4()
	case 6:
		sourceIp = ipPath.SourceIp.To16()
		destinationIp = ipPath.DestinationIp.To16()
	default:
		return smtpFlowKey{}, false
	}
	if sourceIp == nil || destinationIp == nil {
		return smtpFlowKey{}, false
	}

	key := smtpFlowKey{
		ownerId:         ownerId,
		sourcePort:      uint16(ipPath.SourcePort),
		destinationPort: uint16(ipPath.DestinationPort),
		ipVersion:       uint8(ipPath.Version),
	}
	copy(key.sourceIp[:], sourceIp)
	copy(key.destinationIp[:], destinationIp)
	return key, true
}

type smtp587Phase uint8

const (
	smtp587Negotiating smtp587Phase = iota
	smtp587ExpectClientHello
)

type smtpFlowState struct {
	destinationPort int

	synSeen bool
	synSeq  uint32

	baseSequence uint32
	baseSet      bool
	stream       []byte
	parseOffset  int
	phase587     smtp587Phase

	secure   bool
	rejected bool
	lastUsed uint64
}

// smtpEgressGuard keeps only the bounded, pre-encryption prefix of each SMTP
// flow. Once TLS is identified the prefix remains as a retransmission guard:
// bytes that overlap the validated negotiation must match, while later TLS
// sequence space stays opaque. A fresh SYN replaces the marker on tuple reuse.
//
// One lock is intentionally sufficient: it is touched only by ports 465/587,
// and a single SMTP stream's packets are ordered through the lock. The map and
// every field have useful zero values, which keeps fixture-built clients safe.
type smtpEgressGuard struct {
	stateLock sync.Mutex
	flows     map[smtpFlowKey]*smtpFlowState
	clock     uint64
}

func (self *smtpEgressGuard) inspect(ipPath *IpPath, payload []byte) smtpEgressVerdict {
	return self.inspectForOwner(Id{}, ipPath, payload)
}

// inspectForOwner is the provider-side form of inspect. The authenticated
// source id is part of the key because separate remote clients commonly use
// identical tunnel addresses and ephemeral TCP tuples.
func (self *smtpEgressGuard) inspectForOwner(
	ownerId Id,
	ipPath *IpPath,
	payload []byte,
) smtpEgressVerdict {
	if !smtpNeedsEncryptionInspection(ipPath) {
		return smtpEgressNotApplicable
	}
	key, ok := smtpFlowKeyForOwnerPath(ownerId, ipPath)
	if !ok {
		return smtpEgressReject
	}

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.flows == nil {
		self.flows = map[smtpFlowKey]*smtpFlowState{}
	}
	if ipPath.Rst {
		delete(self.flows, key)
		return smtpEgressAllow
	}

	flow := self.flows[key]
	segmentSequence := ipPath.SequenceNumber
	if ipPath.Syn {
		segmentSequence += 1
		if flow == nil || !flow.synSeen || flow.synSeq != ipPath.SequenceNumber {
			flow = self.newFlowWithLock(key, ipPath.DestinationPort)
			flow.synSeen = true
			flow.synSeq = ipPath.SequenceNumber
			flow.baseSequence = segmentSequence
			flow.baseSet = true
		}
	} else if flow == nil {
		flow = self.newFlowWithLock(key, ipPath.DestinationPort)
	}

	self.clock += 1
	flow.lastUsed = self.clock
	if flow.rejected {
		return smtpEgressReject
	}
	if len(payload) == 0 {
		return smtpEgressAllow
	}
	if !flow.baseSet {
		flow.baseSequence = segmentSequence
		flow.baseSet = true
	}
	if !flow.inspectPayload(segmentSequence, payload) {
		flow.rejected = true
		return smtpEgressReject
	}
	return smtpEgressAllow
}

func (self *smtpEgressGuard) newFlowWithLock(key smtpFlowKey, destinationPort int) *smtpFlowState {
	if smtpMaxFlowCount <= len(self.flows) {
		var oldestKey smtpFlowKey
		var oldestUse uint64
		found := false
		sampled := 0
		for candidateKey, candidate := range self.flows {
			if !found || candidate.lastUsed < oldestUse {
				oldestKey = candidateKey
				oldestUse = candidate.lastUsed
				found = true
			}
			sampled += 1
			if smtpFlowEvictionSampleSize <= sampled {
				break
			}
		}
		if found {
			delete(self.flows, oldestKey)
		}
	}
	flow := &smtpFlowState{destinationPort: destinationPort}
	self.flows[key] = flow
	return flow
}

func (self *smtpFlowState) inspectPayload(sequence uint32, payload []byte) bool {
	if self.secure {
		return self.inspectSecureRetransmission(sequence, payload)
	}

	// TCP serial arithmetic is safe here because the retained prefix is at
	// most 2 KiB, far below the half-sequence-space ambiguity boundary.
	relative := int64(int32(sequence - self.baseSequence))
	if relative < 0 || int64(len(self.stream)) < relative {
		// A gap cannot be forwarded safely: bytes hidden in the gap could turn
		// an apparently harmless suffix into AUTH or plaintext credentials.
		return false
	}

	offset := int(relative)
	overlap := len(self.stream) - offset
	if len(payload) < overlap {
		overlap = len(payload)
	}
	if 0 < overlap && !bytes.Equal(self.stream[offset:offset+overlap], payload[:overlap]) {
		// A retransmission must reproduce the bytes already validated for that
		// sequence range. Conflicting overlap is a fail-closed stream splice.
		return false
	}
	if overlap == len(payload) {
		return true
	}

	newBytes := payload[overlap:]
	limit := smtpTlsClientHelloPrefixBytes
	if self.destinationPort == smtpStartTlsPort {
		limit = smtpMaxNegotiationBytes
	}
	remainingCapacity := limit - len(self.stream)
	retainedBytes := newBytes
	if remainingCapacity < len(retainedBytes) {
		retainedBytes = retainedBytes[:remainingCapacity]
	}
	self.stream = append(self.stream, retainedBytes...)

	var valid bool
	switch self.destinationPort {
	case smtpImplicitTlsPort:
		valid, self.secure = tlsClientHelloStreamPrefix(self.stream)
	case smtpStartTlsPort:
		valid, self.secure = self.inspect587Stream()
	default:
		return false
	}
	if self.secure {
		// Keep only the already-bounded verified prefix. It prevents a
		// conflicting retransmission from replacing the negotiation bytes after
		// the connection has moved into opaque TLS sequence space.
		return valid
	}
	// More unverified bytes than the bounded prefix can represent fail closed.
	return valid && len(retainedBytes) == len(newBytes)
}

func (self *smtpFlowState) inspectSecureRetransmission(sequence uint32, payload []byte) bool {
	relative := int64(int32(sequence - self.baseSequence))
	if relative < 0 {
		return false
	}
	offset := int(relative)
	if len(self.stream) <= offset {
		return true
	}
	overlap := len(self.stream) - offset
	if len(payload) < overlap {
		overlap = len(payload)
	}
	return bytes.Equal(self.stream[offset:offset+overlap], payload[:overlap])
}

// tlsClientHelloStreamPrefix validates the TLS record header and handshake
// header as bytes arrive. A complete prefix is nine bytes: TLS Handshake,
// legacy record version, a sane non-empty record, ClientHello, and a sane
// ClientHello body length. The ClientHello body may span TLS records.
func tlsClientHelloStreamPrefix(stream []byte) (valid bool, complete bool) {
	if smtpTlsClientHelloPrefixBytes < len(stream) {
		stream = stream[:smtpTlsClientHelloPrefixBytes]
	}
	for index, value := range stream {
		switch index {
		case 0:
			if value != 0x16 { // Handshake record.
				return false, false
			}
		case 1:
			if value != 0x03 {
				return false, false
			}
		case 2:
			if value < 0x01 || 0x04 < value {
				return false, false
			}
		case 5:
			if value != 0x01 { // ClientHello handshake message.
				return false, false
			}
		}
	}
	if 5 <= len(stream) {
		recordBytes := int(binary.BigEndian.Uint16(stream[3:5]))
		if recordBytes < 4 || 1<<14 < recordBytes {
			return false, false
		}
	}
	if len(stream) < smtpTlsClientHelloPrefixBytes {
		return true, false
	}
	handshakeBytes := int(stream[6])<<16 | int(stream[7])<<8 | int(stream[8])
	// 41 bytes is the minimum ClientHello body (legacy version, random,
	// empty session id, one cipher suite, and one compression method).
	if handshakeBytes < 41 || 1<<20 < handshakeBytes {
		return false, false
	}
	return true, true
}

func (self *smtpFlowState) inspect587Stream() (valid bool, secure bool) {
	for {
		if self.phase587 == smtp587ExpectClientHello {
			return tlsClientHelloStreamPrefix(self.stream[self.parseOffset:])
		}
		if self.parseOffset == len(self.stream) {
			return true, false
		}

		remaining := self.stream[self.parseOffset:]
		lineEnd := bytes.Index(remaining, []byte("\r\n"))
		if lineEnd < 0 {
			return validPartialSmtpNegotiationLine(remaining), false
		}
		if smtpMaxCommandLineBytes < lineEnd ||
			bytes.IndexByte(remaining[:lineEnd], '\r') >= 0 ||
			bytes.IndexByte(remaining[:lineEnd], '\n') >= 0 {
			return false, false
		}

		command, ok := completeSmtpNegotiationCommand(remaining[:lineEnd])
		if !ok {
			return false, false
		}
		self.parseOffset += lineEnd + 2
		if command == smtpCommandStartTls {
			self.phase587 = smtp587ExpectClientHello
		}
	}
}

type smtpCommand uint8

const (
	smtpCommandEhlo smtpCommand = iota
	smtpCommandHelo
	smtpCommandQuit
	smtpCommandStartTls
)

var smtpNegotiationCommands = [...]struct {
	name    string
	command smtpCommand
}{
	// Keep the pre-TLS vocabulary deliberately smaller than SMTP itself:
	// identify the client, request TLS, or leave. Transaction commands and
	// extensible commands such as NOOP are unnecessary before encryption and
	// could otherwise carry arbitrary caller-supplied text.
	{name: "EHLO", command: smtpCommandEhlo},
	{name: "HELO", command: smtpCommandHelo},
	{name: "QUIT", command: smtpCommandQuit},
	{name: "STARTTLS", command: smtpCommandStartTls},
}

func validPartialSmtpNegotiationLine(line []byte) bool {
	if smtpMaxCommandLineBytes+1 < len(line) {
		return false
	}
	if 0 < len(line) && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	if smtpMaxCommandLineBytes < len(line) {
		return false
	}
	if bytes.IndexByte(line, '\r') >= 0 || bytes.IndexByte(line, '\n') >= 0 ||
		!validSmtpAscii(line) {
		return false
	}

	token, argument, separated := splitSmtpCommand(line)
	if !separated {
		for _, candidate := range smtpNegotiationCommands {
			if asciiCasePrefix(candidate.name, token) {
				return true
			}
		}
		return false
	}

	command, ok := smtpNegotiationCommand(token)
	if !ok {
		return false
	}
	switch command {
	case smtpCommandEhlo, smtpCommandHelo:
		return true
	case smtpCommandQuit, smtpCommandStartTls:
		return len(bytes.Trim(argument, " \t")) == 0
	default:
		return false
	}
}

func completeSmtpNegotiationCommand(line []byte) (smtpCommand, bool) {
	if len(line) == 0 || !validSmtpAscii(line) {
		return 0, false
	}
	token, argument, _ := splitSmtpCommand(line)
	command, ok := smtpNegotiationCommand(token)
	if !ok {
		return 0, false
	}
	switch command {
	case smtpCommandEhlo, smtpCommandHelo:
		if len(bytes.Trim(argument, " \t")) == 0 {
			return 0, false
		}
	case smtpCommandQuit, smtpCommandStartTls:
		if len(bytes.Trim(argument, " \t")) != 0 {
			return 0, false
		}
	}
	return command, true
}

func splitSmtpCommand(line []byte) (token []byte, argument []byte, separated bool) {
	for index, value := range line {
		if value == ' ' || value == '\t' {
			return line[:index], line[index+1:], true
		}
	}
	return line, nil, false
}

func smtpNegotiationCommand(token []byte) (smtpCommand, bool) {
	for _, candidate := range smtpNegotiationCommands {
		if asciiCaseEqual(candidate.name, token) {
			return candidate.command, true
		}
	}
	return 0, false
}

func asciiCasePrefix(candidate string, prefix []byte) bool {
	if len(candidate) < len(prefix) {
		return false
	}
	return asciiCaseEqual(candidate[:len(prefix)], prefix)
}

func asciiCaseEqual(candidate string, value []byte) bool {
	if len(candidate) != len(value) {
		return false
	}
	for index, b := range value {
		if 'a' <= b && b <= 'z' {
			b -= 'a' - 'A'
		}
		if candidate[index] != b {
			return false
		}
	}
	return true
}

func validSmtpAscii(value []byte) bool {
	for _, b := range value {
		if b == '\t' {
			continue
		}
		if b < 0x20 || 0x7e < b {
			return false
		}
	}
	return true
}

// tcpRstForPolicyReject returns an RFC 793 reset addressed to the source of a
// rejected TCP packet. It shares the LocalUserNat orphan-reset builder so ACK
// and sequence behavior, IP checksums, and TCP checksums stay identical.
func tcpRstForPolicyReject(packet []byte) []byte {
	if len(packet) == 0 {
		return nil
	}
	ipVersion := int(packet[0] >> 4)
	var ipProtocol ipProtocolNumber
	var sourceIp net.IP
	var destinationIp net.IP
	var transport []byte
	var ok bool
	switch ipVersion {
	case 4:
		ipProtocol, sourceIp, destinationIp, transport, ok = parseIpv4(packet)
	case 6:
		ipProtocol, sourceIp, destinationIp, transport, ok = parseIpv6(packet)
	default:
		return nil
	}
	if !ok || ipProtocol != ipProtocolNumberTcp {
		return nil
	}
	var tcp parsedTcp
	if !parseTcpPacket(sourceIp, destinationIp, transport, &tcp) || tcp.rst {
		return nil
	}
	return tcpRstForOrphan(ipVersion, &tcp)
}

func deliverTcpPolicyReset(
	receive ReceivePacketFunction,
	source TransferPath,
	provideMode protocol.ProvideMode,
	ipPath *IpPath,
	packet []byte,
) {
	reset := tcpRstForPolicyReject(packet)
	if reset == nil {
		return
	}
	defer MessagePoolReturn(reset)
	if receive != nil {
		HandleError(func() {
			receive(source, provideMode, ipPath, reset)
		})
	}
}
