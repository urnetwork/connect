// This file defines the native P2P datagram carrier's wire fragmentation,
// bounded reassembly, selection controls, and observable counters. The carrier
// moves an already end-to-end-encrypted TransferFrame; it never decrypts the
// application payload at an intermediary stream hop.
package connect

import (
	"context"
	"encoding/binary"
	"errors"
	"sync/atomic"
	"time"
)

const (
	p2pFastPathFragmentHeaderByteCount = 16
	// RTP(12) + this carrier header(16) + SRTP authentication + IPv6/UDP
	// remains below a 1,500-byte path for a 1,400-byte fragment payload.
	p2pFastPathFragmentPayloadByteCount = 1400
	p2pFastPathMaximumFragmentCount     = 64
	p2pFastPathReassemblySlotCount      = 64
	p2pFastPathReassemblyTimeout        = 2 * time.Second
	p2pFastPathWarmupInterval           = 25 * time.Millisecond
	p2pFastPathWarmupTimeout            = 5 * time.Second
	p2pFastPathRtpClockRate             = 90000
	p2pFastPathRtpPayloadType           = 127
	p2pFastPathMimeType                 = "video/urnetwork-fast-path"
)

var (
	errP2pFastPathMessageTooLarge = errors.New("p2p fast-path message is too large")
	errP2pFastPathNotReady        = errors.New("p2p fast-path carrier is not ready")
	errP2pFastPathPacket          = errors.New("invalid p2p fast-path packet")
)

// P2pDataPlaneMode controls whether a P2P route requires, forbids, or
// automatically selects the negotiated datagram carrier.
type P2pDataPlaneMode int

const (
	P2pDataPlaneModeAuto P2pDataPlaneMode = iota
	P2pDataPlaneModeLegacyOnly
	P2pDataPlaneModeFastOnly
)

// P2pDataPlaneStatsSnapshot is an immutable view of P2P carrier use. Message
// counts refer to complete TransferFrame messages; fragment counts refer to
// independently authenticated SRTP datagrams.
type P2pDataPlaneStatsSnapshot struct {
	FastSendMessageCount      uint64
	FastSendByteCount         uint64
	FastSendFragmentCount     uint64
	FastReceiveMessageCount   uint64
	FastReceiveByteCount      uint64
	FastReceiveFragmentCount  uint64
	LegacySendMessageCount    uint64
	LegacySendByteCount       uint64
	LegacyReceiveMessageCount uint64
	LegacyReceiveByteCount    uint64
	FastFallbackCount         uint64
	FastDropCount             uint64
}

// P2pDataPlaneStats holds lock-free counters shared by all P2P streams owned
// by one client settings tree.
type P2pDataPlaneStats struct {
	fastSendMessageCount      atomic.Uint64
	fastSendByteCount         atomic.Uint64
	fastSendFragmentCount     atomic.Uint64
	fastReceiveMessageCount   atomic.Uint64
	fastReceiveByteCount      atomic.Uint64
	fastReceiveFragmentCount  atomic.Uint64
	legacySendMessageCount    atomic.Uint64
	legacySendByteCount       atomic.Uint64
	legacyReceiveMessageCount atomic.Uint64
	legacyReceiveByteCount    atomic.Uint64
	fastFallbackCount         atomic.Uint64
	fastDropCount             atomic.Uint64
}

// Snapshot reads a consistent-enough monotonic view without stopping packet
// processing. Individual fields can advance while the snapshot is assembled.
func (self *P2pDataPlaneStats) Snapshot() P2pDataPlaneStatsSnapshot {
	if self == nil {
		return P2pDataPlaneStatsSnapshot{}
	}
	return P2pDataPlaneStatsSnapshot{
		FastSendMessageCount:      self.fastSendMessageCount.Load(),
		FastSendByteCount:         self.fastSendByteCount.Load(),
		FastSendFragmentCount:     self.fastSendFragmentCount.Load(),
		FastReceiveMessageCount:   self.fastReceiveMessageCount.Load(),
		FastReceiveByteCount:      self.fastReceiveByteCount.Load(),
		FastReceiveFragmentCount:  self.fastReceiveFragmentCount.Load(),
		LegacySendMessageCount:    self.legacySendMessageCount.Load(),
		LegacySendByteCount:       self.legacySendByteCount.Load(),
		LegacyReceiveMessageCount: self.legacyReceiveMessageCount.Load(),
		LegacyReceiveByteCount:    self.legacyReceiveByteCount.Load(),
		FastFallbackCount:         self.fastFallbackCount.Load(),
		FastDropCount:             self.fastDropCount.Load(),
	}
}

// A p2pFastPathFragmentHeader precedes every RTP payload. Every fragment
// repeats the total length so reassembly can begin after reordering.
type p2pFastPathFragmentHeader struct {
	messageId     uint32
	messageLength int
	fragmentIndex int
	fragmentCount int
}

// p2pFastPathFragmentCount returns the exact number of independently carried
// fragments needed for one message.
func p2pFastPathFragmentCount(messageByteCount int) int {
	return (messageByteCount + p2pFastPathFragmentPayloadByteCount - 1) /
		p2pFastPathFragmentPayloadByteCount
}

// writeP2pFastPathFragmentHeader serializes one fixed-size routing header.
func writeP2pFastPathFragmentHeader(
	packet []byte,
	header p2pFastPathFragmentHeader,
) error {
	if len(packet) < p2pFastPathFragmentHeaderByteCount ||
		header.messageId == 0 ||
		header.messageLength <= 0 ||
		header.fragmentCount <= 0 ||
		p2pFastPathMaximumFragmentCount < header.fragmentCount ||
		header.fragmentIndex < 0 ||
		header.fragmentCount <= header.fragmentIndex {
		return errP2pFastPathPacket
	}
	packet[0] = 'U'
	packet[1] = 'R'
	packet[2] = 'D'
	packet[3] = 1
	binary.BigEndian.PutUint32(packet[4:8], header.messageId)
	binary.BigEndian.PutUint32(packet[8:12], uint32(header.messageLength))
	binary.BigEndian.PutUint16(packet[12:14], uint16(header.fragmentIndex))
	binary.BigEndian.PutUint16(packet[14:16], uint16(header.fragmentCount))
	return nil
}

// parseP2pFastPathFragmentHeader validates one fixed-size routing header.
func parseP2pFastPathFragmentHeader(packet []byte) (p2pFastPathFragmentHeader, error) {
	if len(packet) <= p2pFastPathFragmentHeaderByteCount ||
		packet[0] != 'U' ||
		packet[1] != 'R' ||
		packet[2] != 'D' ||
		packet[3] != 1 {
		return p2pFastPathFragmentHeader{}, errP2pFastPathPacket
	}
	messageLength := binary.BigEndian.Uint32(packet[8:12])
	if uint64(messageLength) > uint64(^uint(0)>>1) {
		return p2pFastPathFragmentHeader{}, errP2pFastPathPacket
	}
	header := p2pFastPathFragmentHeader{
		messageId:     binary.BigEndian.Uint32(packet[4:8]),
		messageLength: int(messageLength),
		fragmentIndex: int(binary.BigEndian.Uint16(packet[12:14])),
		fragmentCount: int(binary.BigEndian.Uint16(packet[14:16])),
	}
	if header.messageId == 0 ||
		header.messageLength <= 0 ||
		header.fragmentCount != p2pFastPathFragmentCount(header.messageLength) ||
		p2pFastPathMaximumFragmentCount < header.fragmentCount ||
		header.fragmentIndex < 0 ||
		header.fragmentCount <= header.fragmentIndex {
		return p2pFastPathFragmentHeader{}, errP2pFastPathPacket
	}
	fragmentByteCount := len(packet) - p2pFastPathFragmentHeaderByteCount
	expectedFragmentByteCount := min(
		p2pFastPathFragmentPayloadByteCount,
		header.messageLength-header.fragmentIndex*p2pFastPathFragmentPayloadByteCount,
	)
	if fragmentByteCount != expectedFragmentByteCount {
		return p2pFastPathFragmentHeader{}, errP2pFastPathPacket
	}
	return header, nil
}

// One p2pFastPathReassemblySlot owns a pooled complete-message buffer until
// all fragments arrive, the slot collides, or its deadline expires.
type p2pFastPathReassemblySlot struct {
	messageId      uint32
	message        []byte
	fragmentCount  int
	receivedBits   uint64
	receivedCount  int
	expirationTime time.Time
}

// p2pFastPathReassembler bounds incomplete messages without a map allocation
// on the receive path. Message ids select fixed slots; a collision drops only
// the older incomplete message.
type p2pFastPathReassembler struct {
	maximumMessageByteCount int
	slots                   [p2pFastPathReassemblySlotCount]p2pFastPathReassemblySlot
}

// newP2pFastPathReassembler creates a generation-local reassembler.
func newP2pFastPathReassembler(maximumMessageByteCount int) *p2pFastPathReassembler {
	return &p2pFastPathReassembler{
		maximumMessageByteCount: maximumMessageByteCount,
	}
}

// clearP2pFastPathReassemblySlot releases any incomplete owning buffer.
func clearP2pFastPathReassemblySlot(slot *p2pFastPathReassemblySlot) {
	if slot.message != nil {
		MessagePoolReturn(slot.message)
	}
	*slot = p2pFastPathReassemblySlot{}
}

// accept copies one authenticated fragment and returns a complete pooled
// message when the final missing fragment arrives.
func (self *p2pFastPathReassembler) accept(packet []byte, now time.Time) ([]byte, error) {
	header, err := parseP2pFastPathFragmentHeader(packet)
	if err != nil || self.maximumMessageByteCount < header.messageLength {
		return nil, errP2pFastPathPacket
	}
	slot := &self.slots[int(header.messageId)%len(self.slots)]
	if slot.messageId != 0 &&
		(slot.messageId != header.messageId || slot.expirationTime.Before(now)) {
		clearP2pFastPathReassemblySlot(slot)
	}
	if slot.messageId == 0 {
		*slot = p2pFastPathReassemblySlot{
			messageId:      header.messageId,
			message:        MessagePoolGet(header.messageLength),
			fragmentCount:  header.fragmentCount,
			expirationTime: now.Add(p2pFastPathReassemblyTimeout),
		}
	}
	if len(slot.message) != header.messageLength ||
		slot.fragmentCount != header.fragmentCount {
		clearP2pFastPathReassemblySlot(slot)
		return nil, errP2pFastPathPacket
	}
	bit := uint64(1) << uint(header.fragmentIndex)
	if slot.receivedBits&bit != 0 {
		return nil, nil
	}
	fragment := packet[p2pFastPathFragmentHeaderByteCount:]
	offset := header.fragmentIndex * p2pFastPathFragmentPayloadByteCount
	copy(slot.message[offset:offset+len(fragment)], fragment)
	slot.receivedBits |= bit
	slot.receivedCount += 1
	if slot.receivedCount != slot.fragmentCount {
		return nil, nil
	}
	message := slot.message
	slot.message = nil
	*slot = p2pFastPathReassemblySlot{}
	return message, nil
}

// close releases every incomplete message owned by the reassembler.
func (self *p2pFastPathReassembler) close() {
	for slotIndex := range self.slots {
		clearP2pFastPathReassemblySlot(&self.slots[slotIndex])
	}
}

// webRtcFastPathConn is the optional native capability used by P2P transport.
// Browser and old connection implementations simply do not implement it.
type webRtcFastPathConn interface {
	FastPathReady() bool
	WaitFastPathReady(ctx context.Context, timeout time.Duration) bool
	WriteFastPathMessage(message []byte) (fragmentCount int, err error)
	FastPathMessages() <-chan p2pFastPathReceivedMessage
}

// p2pFastPathReceivedMessage transfers ownership of one pooled, completely
// reassembled message from the WebRTC receiver to the P2P route worker.
type p2pFastPathReceivedMessage struct {
	message       []byte
	fragmentCount int
}
