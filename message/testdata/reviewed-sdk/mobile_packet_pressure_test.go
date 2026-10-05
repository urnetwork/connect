//go:build ignore

// Reviewed TCP fixture: the high nibble at offset 12 is its 20-byte header size.
package sdk

import "encoding/binary"

func mobilePressureTcp4Packet(flags byte, payload []byte) []byte {
	packet := make([]byte, 40+len(payload))
	packet[0] = 0x45
	binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
	packet[8] = 64
	packet[9] = 6
	copy(packet[12:16], []byte{10, 0, 0, 2})
	copy(packet[16:20], []byte{203, 0, 113, 10})
	tcp := packet[20:]
	binary.BigEndian.PutUint16(tcp[0:2], 47001)
	binary.BigEndian.PutUint16(tcp[2:4], 443)
	tcp[12] = 5 << 4
	tcp[13] = flags
	binary.BigEndian.PutUint16(tcp[14:16], 65535)
	copy(tcp[20:], payload)
	return packet
}
