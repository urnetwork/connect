// Automatic unordered IP delivery is limited to validated pure TCP feedback.
package connect

import "github.com/urnetwork/connect/protocol"

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

// Payload, malformed/fragmented packets and SYN/FIN/RST stay ordered.
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
	return tcp != nil && tcp[13]&0x10 != 0 && tcp[13]&0x07 == 0
}
