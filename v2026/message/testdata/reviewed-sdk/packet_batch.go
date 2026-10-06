//go:build ignore

// Reviewed IP packet provenance and version decoding, not retention encoding.
package sdk

type PacketBatch struct {
	packets [][]byte
}

func (self *PacketBatch) Get(index int) []byte {
	if self == nil || index < 0 || len(self.packets) <= index {
		return nil
	}
	return self.packets[index]
}

func (self *PacketBatch) IpVersion(index int) int {
	packet := self.Get(index)
	if len(packet) == 0 {
		return 0
	}
	switch packet[0] >> 4 {
	case 4:
		return 4
	case 6:
		return 6
	default:
		return 0
	}
}
