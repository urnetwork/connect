package connect

import (
	"unsafe"

	"google.golang.org/protobuf/proto"
)

// Exact admission is opt-in for mobile. Unlike the legacy per-flow floor,
// every retained owner participates in the same atomic aggregate ceiling.
func (self *transferItem) reserveMemory(budget *TransferMemoryBudget, byteCount ByteCount) bool {
	if budget == nil {
		return true
	}
	if self.memoryBudget != nil {
		panic("transfer owner already has a memory reservation")
	}
	if !budget.TryReserve(byteCount) {
		return false
	}
	self.memoryBudget = budget
	self.queueByteCount = byteCount
	return true
}

func (self *transferItem) releaseMemory() {
	if budget := self.memoryBudget; budget != nil {
		self.memoryBudget = nil
		budget.Release(self.queueByteCount)
	}
}

func retainedMessageCapacity(byteCount ByteCount) ByteCount {
	for _, pool := range orderedMessagePools() {
		if byteCount <= ByteCount(pool.size) {
			return ByteCount(pool.size) + MessagePoolMetaByteCount
		}
	}
	return addReceiveQueueByteCount(byteCount, MessagePoolMetaByteCount)
}

func (self *sendItem) retainedMemoryByteCount(frameByteCount ByteCount, frameCount int, legacy bool) ByteCount {
	// A rounded owner envelope covers the item, queue/map/ACK-lookup entries,
	// decoded protobuf frames and allocator rounding. Scratch roots are reserved
	// for decoding and rewriting a full head/contract, so a full budget can make
	// progress without releasing a still-live retry owner or borrowing memory.
	root := retainedMessageCapacity(frameByteCount)
	byteCount := addReceiveQueueByteCount(root, root)
	byteCount = addReceiveQueueByteCount(byteCount, root)
	if legacy {
		// v1 decodes the nested Frame.MessageBytes before decoding Pack, then
		// re-marshals Pack before TransferFrame. Both are additional roots.
		byteCount = addReceiveQueueByteCount(byteCount, addReceiveQueueByteCount(root, root))
	}
	ownerBytes := ByteCount(unsafe.Sizeof(sendItem{})) + 128*(ByteCount(frameCount)+4) + 128
	byteCount = addReceiveQueueByteCount(byteCount, (ownerBytes+1023)/1024*1024)
	if self.acks.overflow != nil {
		byteCount = addReceiveQueueByteCount(byteCount,
			(ByteCount(unsafe.Sizeof(sendAckSetOverflow{}))+1023)/1024*1024)
	}
	return byteCount
}

// Called before allocating a replacement frame. Usually the initial
// full-contract envelope already covers it; an unexpected larger rewrite
// must atomically acquire its growth rather than escape the shared budget.
func (self *sendItem) reserveFrameRewrite(frameByteCount ByteCount, frameCount int, legacy bool) bool {
	if self.memoryBudget == nil {
		return true
	}
	byteCount := self.retainedMemoryByteCount(frameByteCount, frameCount, legacy)
	if byteCount <= self.queueByteCount {
		return true
	}
	if !self.memoryBudget.TryReserve(byteCount - self.queueByteCount) {
		return false
	}
	self.queueByteCount = byteCount
	return true
}

func (self *ReceiveSequence) reserveHeldItem(item *receiveItem) bool {
	return !self.receiveQueue.lifetimeBudget ||
		item.reserveMemory(self.receiveQueue.budget, item.QueueByteCount())
}

// Covers both protocol encodings, bounded per-frame field overhead, and a
// future full-contract head. This is evaluated before allocating wire bytes.
func (self *SendSequence) retainedSendFrameByteCount(messageByteCount ByteCount, frameCount int) ByteCount {
	byteCount := addReceiveQueueByteCount(messageByteCount, 512+32*ByteCount(frameCount))
	if self.sendContract != nil {
		byteCount = addReceiveQueueByteCount(byteCount, ByteCount(proto.Size(self.sendContract.contract)))
	}
	return byteCount
}

func (self *SendSequence) retainedSendPackByteCount(pack *SendPack) ByteCount {
	frames := pack.frameList()
	frameCount := len(frames)
	if pack.logicalGroup {
		maxFrames, maxMessageByteCount := pack.groupChunkLimits()
		frameCount = nextSendGroupChunkEndWithLimits(pack.Frames, pack.groupFrameIndex,
			maxFrames, maxMessageByteCount) - pack.groupFrameIndex
	}
	return (&sendItem{}).retainedMemoryByteCount(
		self.retainedSendFrameByteCount(pack.nextSerializedMessageByteCount(), frameCount),
		frameCount, self.sendBufferSettings.ProtocolVersion < 2)
}

func (self *SendSequence) retainedSendPackFits(pack *SendPack) bool {
	return !self.resendQueue.lifetimeBudget || self.resendQueue.budget == nil ||
		self.retainedSendPackByteCount(pack) <= self.resendQueue.budget.Available()
}
