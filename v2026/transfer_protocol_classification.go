package connect

import (
	"encoding/binary"

	"github.com/urnetwork/connect/v2026/protocol"
	"google.golang.org/protobuf/encoding/protowire"
)

// Classification is admission policy only. Proof, source, packet policy,
// decryption and reliable delivery remain with their ordinary owners. A mixed
// Pack is DATA even if it carries a contract, handshake or protocol option.
func protocolOnlyFrames(frames []*protocol.Frame) bool {
	for _, frame := range frames {
		if frame == nil || !protocolOnlyFrame(frame.MessageType, frame.Raw, frame.MessageBytes) {
			return false
		}
	}
	return true
}

// Unknown kinds and nested Pack/Subprotocol/application envelopes fail closed.
// Raw IP and legacy protobuf IP wrappers have the same packet classification.
func protocolOnlyFrame(kind protocol.MessageType, raw bool, messageBytes []byte) bool {
	switch kind {
	case protocol.MessageType_IpIpPacketToProvider, protocol.MessageType_IpIpPacketFromProvider:
		packet, ok := protocolClassIpPacket(messageBytes, raw)
		return ok && protocolClassPureTcpControl(packet)
	case protocol.MessageType_TransferAck,
		protocol.MessageType_TransferContract,
		protocol.MessageType_TransferProvide,
		protocol.MessageType_TransferAuth,
		protocol.MessageType_TransferCreateStream,
		protocol.MessageType_TransferCreateStreamResult,
		protocol.MessageType_TransferCloseStream,
		protocol.MessageType_TransferStreamOpen,
		protocol.MessageType_TransferStreamClose,
		protocol.MessageType_TransferCreateContract,
		protocol.MessageType_TransferCreateContractResult,
		protocol.MessageType_TransferCloseContract,
		protocol.MessageType_TransferPeerAudit,
		protocol.MessageType_IpIpPing,
		protocol.MessageType_TransferControlPing,
		protocol.MessageType_TransferProvidePing,
		protocol.MessageType_TransferExchangeSignals,
		protocol.MessageType_TransferExchangeSignal,
		protocol.MessageType_TransferStreamReset,
		protocol.MessageType_TransferEncryptedControl,
		protocol.MessageType_TransferEncryptedKey,
		protocol.MessageType_TransferClientKey,
		protocol.MessageType_TransferNetworkPeersReset,
		protocol.MessageType_TransferNetworkPeersUpdate,
		protocol.MessageType_TransferResidentMigrate,
		protocol.MessageType_IpIpProviderDiagnostics,
		protocol.MessageType_TransferSubprotocolsQuery,
		protocol.MessageType_TransferSubprotocolsQueryResult,
		protocol.MessageType_TransferNetworkSessionsChanged,
		protocol.MessageType_TransferStreamAuthorization,
		protocol.MessageType_TransferResidentMoveResult,
		protocol.MessageType_TransferCloseSequences:
		return true
	default:
		return false
	}
}

// The legacy wrapper is IpPacket{From,To}Provider.ip_packet.packet_bytes.
// Repeated nested message fields merge; the last bytes field wins exactly as
// ordinary protobuf decoding does. Groups and malformed fields are conservative.
func protocolClassIpPacket(message []byte, raw bool) ([]byte, bool) {
	if raw {
		return message, len(message) != 0
	}
	var packet []byte
	outer := receiveMemoryWireCursor{bytes: message}
	for outer.offset < len(message) {
		field, ok := outer.next()
		if !ok {
			return nil, false
		}
		if field.number != 1 {
			continue
		}
		if field.wireType != protowire.BytesType {
			return nil, false
		}
		inner := receiveMemoryWireCursor{bytes: field.bytes}
		for inner.offset < len(field.bytes) {
			value, ok := inner.next()
			if !ok {
				return nil, false
			}
			if value.number == 1 {
				if value.wireType != protowire.BytesType {
					return nil, false
				}
				packet = value.bytes
			}
		}
	}
	return packet, len(packet) != 0
}

// This does not infer TCP control from the ACK bit: almost every TCP DATA
// packet carries it. Both declared IP length and actual TCP header length must
// prove that no application byte exists. Fragmented and opaque payloads remain
// DATA; an atomic IPv6 fragment is complete and can be inspected normally.
func protocolClassTcpPacket(packet []byte) []byte {
	if len(packet) == 0 {
		return nil
	}
	var offset int
	switch packet[0] >> 4 {
	case 4:
		if len(packet) < 20 || int(binary.BigEndian.Uint16(packet[2:4])) != len(packet) || packet[9] != 6 {
			return nil
		}
		offset = int(packet[0]&15) * 4
		// MF or a nonzero fragment offset means this is not a whole TCP packet.
		if offset < 20 || offset > len(packet) || binary.BigEndian.Uint16(packet[6:8])&0xbfff != 0 {
			return nil
		}
	case 6:
		if len(packet) < 40 || int(binary.BigEndian.Uint16(packet[4:6]))+40 != len(packet) {
			return nil
		}
		offset = 40
		next := packet[6]
		for extensions := 0; next != 6; extensions++ {
			if extensions == 8 || offset+2 > len(packet) {
				return nil
			}
			var length int
			switch next {
			case 0, 43, 60: // hop-by-hop, routing, destination options
				if next == 0 && offset != 40 {
					return nil
				}
				length = (int(packet[offset+1]) + 1) * 8
			case 51: // authentication header
				length = (int(packet[offset+1]) + 2) * 4
				if length < 12 {
					return nil
				}
			case 44:
				length = 8
				if offset+length > len(packet) || packet[offset+1] != 0 || binary.BigEndian.Uint16(packet[offset+2:offset+4]) != 0 {
					return nil
				}
			default: // ESP, no-next-header, UDP and unknown extensions
				return nil
			}
			if length < 8 || length > len(packet)-offset {
				return nil
			}
			next = packet[offset]
			offset += length
		}
	default:
		return nil
	}
	if len(packet)-offset < 20 {
		return nil
	}
	tcp := packet[offset:]
	header := int(tcp[12]>>4) * 4
	if header < 20 || len(tcp) < header {
		return nil
	}
	return tcp
}

func protocolClassPureTcpControl(packet []byte) bool {
	tcp := protocolClassTcpPacket(packet)
	if tcp == nil || int(tcp[12]>>4)*4 != len(tcp) {
		return false
	}
	flags := tcp[13]
	// Zero-payload PSH/URG or reserved bits are not canonical control input.
	// ECN negotiation flags may accompany an otherwise valid SYN/ACK.
	return tcp[12]&0x0e == 0 && flags&0x28 == 0 && flags&0x17 != 0 && binary.BigEndian.Uint16(tcp[18:20]) == 0
}

// NoBudget is trusted byte policy. This independent local fence only controls
// reordering: every lifecycle member, including DATA-bearing SYN/FIN/RST,
// stays behind earlier owners of its flow. Opaque TCP is conservative.
func (self *SendPack) hasTcpOrderBarrier() bool {
	for _, frame := range self.frameList() {
		if frame == nil {
			return true
		}
		switch frame.MessageType {
		case protocol.MessageType_IpIpPacketToProvider, protocol.MessageType_IpIpPacketFromProvider:
			packet, ok := protocolClassIpPacket(frame.MessageBytes, frame.Raw)
			if !ok {
				return true
			}
			tcp := protocolClassTcpPacket(packet)
			if tcp == nil {
				if !self.schedulingKey.valid || self.schedulingKey.ipFlow.protocol == IpProtocolTcp {
					return true
				}
			} else if tcp[13]&0x07 != 0 {
				return true
			}
		}
	}
	return false
}
