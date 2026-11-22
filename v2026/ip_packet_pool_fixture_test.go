// Shares pooled packet construction across portable and native ownership fixtures.
package connect

import "net"

// Hand-rolls an IPv4/UDP packet; the caller owns the returned pooled buffer.
func poolBalanceUdp4Packet(srcIp net.IP, srcPort int, dstIp net.IP, dstPort int, payload []byte) []byte {
	total := 28 + len(payload)
	packet := MessagePoolGet(total)
	packet[0] = 0x45
	packet[1] = 0
	packet[2] = byte(total >> 8)
	packet[3] = byte(total)
	packet[4], packet[5], packet[6], packet[7] = 0, 0, 0, 0
	packet[8] = 64
	packet[9] = 17
	packet[10], packet[11] = 0, 0
	copy(packet[12:16], srcIp.To4())
	copy(packet[16:20], dstIp.To4())
	packet[20] = byte(srcPort >> 8)
	packet[21] = byte(srcPort)
	packet[22] = byte(dstPort >> 8)
	packet[23] = byte(dstPort)
	udpLen := 8 + len(payload)
	packet[24] = byte(udpLen >> 8)
	packet[25] = byte(udpLen)
	packet[26], packet[27] = 0, 0
	copy(packet[28:], payload)
	return packet
}
