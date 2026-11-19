// Automatic unordered IP delivery admits validated TCP feedback and SYNACKs.
package connect

import "github.com/urnetwork/connect/v2026/protocol"

func immediateIpAckFrames(frames []*protocol.Frame) bool {
	if len(frames) == 0 {
		return false
	}
	for _, frame := range frames {
		if frame == nil || !immediateIpAckFrame(frame.MessageType, frame.Raw, frame.MessageBytes) {
			return false
		}
	}
	return true
}

// Reports whether a group carries a SYN-ACK. A SYN-ACK completes a new
// handshake, so a stalled return stream must not deliver one ahead of an old
// gap (SendBufferSettings.ImmediateHandshakeMaxGapAge). A pure ACK carries the
// window and acknowledgement that another flow, often the other direction's,
// needs to move, so it stays immediate behind a gap of any age.
func immediateIpHandshakeFrames(frames []*protocol.Frame) bool {
	for _, frame := range frames {
		if frame == nil || frame.MessageType != protocol.MessageType_IpIpPacketToProvider &&
			frame.MessageType != protocol.MessageType_IpIpPacketFromProvider {
			continue
		}
		packet, ok := protocolClassIpPacket(frame.MessageBytes, frame.Raw)
		if !ok {
			continue
		}
		if tcp := protocolClassTcpPacket(packet); 13 < len(tcp) && tcp[13]&0x02 != 0 {
			return true
		}
	}
	return false
}

// Payload, malformed packets and SYN-only/FIN/RST stay ordered. SYNACKs may
// finish another flow's handshake without waiting for an unrelated DATA gap.
// This chooses local policy; downstream TCP and authentication still validate.
func immediateIpAckFrame(kind protocol.MessageType, raw bool, message []byte) bool {
	if kind != protocol.MessageType_IpIpPacketToProvider && kind != protocol.MessageType_IpIpPacketFromProvider {
		return false
	}
	packet, ok := protocolClassIpPacket(message, raw)
	if !ok || !protocolClassPureTcpControl(packet) {
		return false
	}
	tcp := protocolClassTcpPacket(packet)
	if tcp == nil || tcp[13]&0x10 == 0 || tcp[13]&0x05 != 0 {
		return false
	}
	if tcp[13]&0x02 == 0 || packet[0]>>4 != 6 {
		return true
	}
	// Existing ACK policy accepts complete atomic IPv6 fragments. Do not
	// extend that exception to the newly eligible handshake response.
	next, offset := packet[6], 40
	for next != 6 {
		if next == 44 {
			return false
		}
		length := (int(packet[offset+1]) + 1) * 8
		if next == 51 {
			length = (int(packet[offset+1]) + 2) * 4
		}
		next, offset = packet[offset], offset+length
	}
	return true
}
