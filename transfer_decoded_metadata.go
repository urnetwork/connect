// Charge overflow owned-decoder objects and pointer backing storage along
// with the existing inline owner; payload roots keep their own unchanged claims.
package connect

import "math"

// Overflow decoded frames have independent objects and a grown pointer
// slice. The inline pair remains one KiB. Each overflow frame gets
// a conservative 128-byte allowance; the actual pointer capacity is charged at
// eight bytes per slot plus 16 bytes. This is a logical allocation envelope,
// not a toolchain-specific physical-heap bound. No body is counted
// here: receiveQueueByteCount charges every payload root independently.
func (self *decodedPackOwner) metadataQueueByteCount() ByteCount {
	if self == nil {
		return 0
	}
	byteCount := decodedPackOwnerQueueByteCount
	if overflowCount := len(self.pack.Frames) - len(self.frames); 0 < overflowCount {
		if math.MaxInt64/128 < ByteCount(overflowCount) {
			return ByteCount(math.MaxInt64)
		}
		byteCount = addReceiveQueueByteCount(byteCount, ByteCount(overflowCount)*128)
	}
	if pointerCount := cap(self.pack.Frames); len(self.framePointers) < pointerCount {
		// Eight bytes covers a pointer on both supported mobile word sizes.
		if math.MaxInt64/8 < ByteCount(pointerCount) {
			return ByteCount(math.MaxInt64)
		}
		byteCount = addReceiveQueueByteCount(byteCount, ByteCount(pointerCount)*8)
		byteCount = addReceiveQueueByteCount(byteCount, 16)
	}
	return byteCount
}
