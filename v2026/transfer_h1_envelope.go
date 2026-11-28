// H1 groups reserve wire room for the exact full proof that any retained item
// may need on head repair, including later encryption of an initially plain Pack.
package connect

import (
	"errors"

	"github.com/urnetwork/connect/v2026/protocol"
	"google.golang.org/protobuf/proto"
)

// The default transport floor excludes H1's own framing/delay header. Actual
// admission uses each advertised ceiling, including larger custom carriers.
// GCM uses its standard 16-byte authentication tag.
const (
	sendPackH1WireByteCount    = 16 * 1024
	sendPackH1AeadTagByteCount = 16
)

var errSendPackH1Envelope = errors.New("full-contract Transfer envelope exceeds advertised H1 message limit")

// Counts a full head using the actual encoded contract length and frame-entry
// bytes. Maximal sequence/tag varints and all supported presence bits cover
// initial, compact, NoAck, announcement, and later full-head forms. End-to-end
// paths contain source/destination only; transport-local stream ids stay outside.
func h1FullHeadEnvelopeByteCount(frameWireByteCount int, contractByteCount int, hasContract bool, legacy bool) int {
	path := TransferPath{SourceId: Id{1}, DestinationId: Id{2}}
	contractId := Id{3}
	pack := sendPackFrame{
		deliverImmediately: true,
		path:               path, sequenceNumber: ^uint64(0), head: true, nack: true,
		tagSendTime: ^uint64(0), contractId: &contractId,
		forceStream: true, companionContract: true, logicalLane: 8,
		contractAhead: true,
	}
	packByteCount := pack.sizePack() + frameWireByteCount
	if hasContract {
		frameByteCount := protoSizeTag(1) + protoSizeVarint(uint64(protocol.MessageType_TransferContract))
		if contractByteCount > 0 {
			frameByteCount += protoSizeTag(2) + protoSizeVarint(uint64(contractByteCount)) + contractByteCount
		}
		packByteCount += protoSizeTag(7) + protoSizeVarint(uint64(frameByteCount)) + frameByteCount
	}
	pathByteCount := sizeTransferPath(path)
	plainByteCount := protoSizeTag(1) + protoSizeVarint(uint64(pathByteCount)) + pathByteCount
	if legacy {
		// Frame.message_type is TransferPack=0 and is omitted by proto3.
		frameByteCount := protoSizeTag(2) + protoSizeVarint(uint64(packByteCount)) + packByteCount
		plainByteCount += protoSizeTag(2) + protoSizeVarint(uint64(frameByteCount)) + frameByteCount
	} else {
		plainByteCount += protoSizeTag(3) + protoSizeVarint(uint64(protocol.MessageType_TransferPack))
		plainByteCount += protoSizeTag(4) + protoSizeVarint(uint64(packByteCount)) + packByteCount
	}
	plainByteCount += protoSizeTag(7) + protoSizeVarint(uint64(protocol.SequenceRole_SequenceRoleServer))
	plainByteCount += protoSizeTag(8) + 1
	return sizeEncryptedOuterTransferFrame(path,
		sequenceTlsAeadNonceSize+plainByteCount+sendPackH1AeadTagByteCount,
		protocol.SequenceRole_SequenceRoleServer)
}

// Includes unknown Frame fields on legacy encodings as well as the hot codec's
// normal fields; no payload is marshalled or copied to calculate this bound.
func h1FrameWireByteCount(frames []*protocol.Frame) int {
	byteCount := 0
	for _, frame := range frames {
		frameByteCount := proto.Size(frame)
		byteCount += protoSizeTag(5) + protoSizeVarint(uint64(frameByteCount)) + frameByteCount
	}
	return byteCount
}

// A contract too large even without application frames cannot be announced or
// installed on H1. Refusal is before its opening debit or any wire publication.
func (self *SendSequence) h1ContractEnvelopeFits(contract *protocol.Contract) bool {
	policy := self.transferFlightPolicy()
	return !policy.h1Only || policy.h1MaxMessageByteCount <= 0 ||
		h1FullHeadEnvelopeByteCount(0, proto.Size(contract), contract != nil,
			self.sendBufferSettings.ProtocolVersion < 2) <= policy.h1MaxMessageByteCount
}

// Tests the actual immutable frames, including an indivisible oversized first
// frame that the historical payload chunker deliberately advances on its own.
func (self *SendSequence) h1FramesEnvelopeFits(frames []*protocol.Frame, maxWireByteCount int) bool {
	var contract *protocol.Contract
	if self.sendContract != nil {
		contract = self.sendContract.contract
	}
	return h1FullHeadEnvelopeByteCount(h1FrameWireByteCount(frames), proto.Size(contract), contract != nil,
		self.sendBufferSettings.ProtocolVersion < 2) <= maxWireByteCount
}

// Finds a payload limit whose worst frame-entry overhead still fits. The
// remaining frames are immutable, so their largest per-frame overhead bounds
// every later chunk without allocating an encoded proof or a trial Pack.
func (self *SendSequence) h1PayloadEnvelopeLimit(frames []*protocol.Frame, maxFrames int, requested ByteCount, maxWireByteCount int) ByteCount {
	maxFrameOverhead := 0
	for _, frame := range frames {
		frameByteCount := proto.Size(frame)
		overhead := protoSizeTag(5) + protoSizeVarint(uint64(frameByteCount)) + frameByteCount - len(frame.MessageBytes)
		maxFrameOverhead = max(maxFrameOverhead, overhead)
	}
	frameOverhead := min(maxFrames, len(frames)) * maxFrameOverhead
	var contract *protocol.Contract
	if self.sendContract != nil {
		contract = self.sendContract.contract
	}
	contractByteCount := proto.Size(contract)
	legacy := self.sendBufferSettings.ProtocolVersion < 2
	low, high := ByteCount(0), max(ByteCount(0), requested)
	for low < high {
		middle := low + (high-low+1)/2
		if h1FullHeadEnvelopeByteCount(int(middle)+frameOverhead, contractByteCount, contract != nil, legacy) <= maxWireByteCount {
			low = middle
		} else {
			high = middle - 1
		}
	}
	return low
}

// A conservative multi-frame overhead estimate may leave no payload room
// although one actual frame fits. Fall back to singletons in that case; zero
// must never reach the historical chunker's default-limit fallback.
func (self *SendSequence) boundH1LogicalGroup(sendPack *SendPack, maxWireByteCount int) (int, error) {
	maxFrames, oldLimit := sendPack.groupChunkLimits()
	start := sendPack.groupFrameIndex
	limit := self.h1PayloadEnvelopeLimit(sendPack.Frames[start:], maxFrames, oldLimit, maxWireByteCount)
	if limit > 0 {
		sendPack.shrinkGroupLimits(maxFrames, limit)
	} else if self.h1FramesEnvelopeFits(sendPack.Frames[start:start+1], maxWireByteCount) {
		sendPack.shrinkGroupLimits(1, oldLimit)
	} else {
		return start, errSendPackH1Envelope
	}
	maxFrames, limit = sendPack.groupChunkLimits()
	end := nextSendGroupChunkEndWithLimits(sendPack.Frames, start, maxFrames, limit)
	if !self.h1FramesEnvelopeFits(sendPack.Frames[start:end], maxWireByteCount) {
		return start, errSendPackH1Envelope
	}
	return end, nil
}

// Shrinking a live group's remaining partition adds expected chunks before
// another item is materialized. At least its current unsent chunk still owes
// every completion phase, so a concurrent earlier ACK cannot finish the group.
func (self *SendPack) shrinkGroupLimits(maxFrames int, limit ByteCount) {
	oldMaxFrames, oldLimit := self.groupChunkLimits()
	if maxFrames <= 0 || oldMaxFrames < maxFrames || limit <= 0 || oldLimit < limit {
		panic("logical group limits must only shrink")
	}
	if maxFrames == oldMaxFrames && limit == oldLimit {
		return
	}
	if completion := self.groupCompletion; completion != nil {
		remaining := self.Frames[self.groupFrameIndex:]
		oldCount := sendGroupChunkCountWithLimits(remaining, oldMaxFrames, oldLimit)
		newCount := sendGroupChunkCountWithLimits(remaining, maxFrames, limit)
		completion.mutex.Lock()
		if newCount < oldCount || completion.firstRouteDone || completion.noAckDone || completion.terminalDone {
			completion.mutex.Unlock()
			panic("completed logical group cannot change its unsent partition")
		}
		completion.chunkCount += newCount - oldCount
		completion.mutex.Unlock()
	}
	self.groupChunkMaxFrames = maxFrames
	self.groupChunkMaxMessageByteCount = limit
}

// Refunds only the difference between two minimum-clamped debits. The opening
// contract-only debit remains independent, and expiry cannot veto a reduction
// of an already admitted, still-unwritten message.
func (self *sequenceContract) reduceUnwrittenDebit(original ByteCount, reduced ByteCount) {
	if reduced < 0 || original < reduced {
		panic("unwritten transfer debit must only shrink")
	}
	refund := max(self.minUpdateByteCount, original) - max(self.minUpdateByteCount, reduced)
	if self.unackedByteCount < refund {
		panic("unwritten transfer contract debit missing")
	}
	self.unackedByteCount -= refund
	if self.noAckBudgetPublished {
		self.noAckRemainingByteCount.Add(int64(refund))
	}
	if self.statsEntry != nil {
		self.statsEntry.updateUsedByteCount(self.ackedByteCount + self.unackedByteCount)
	}
}
