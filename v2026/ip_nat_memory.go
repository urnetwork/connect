package connect

import (
	"context"
	"errors"
	"sync/atomic"
	"unsafe"

	"github.com/urnetwork/connect/v2026/protocol"
)

var ErrNatMemoryBudget = errors.New("local NAT retained memory budget is full")

// A reservation follows the retained owner, not its queue membership. Copies
// transfer ownership; exactly one copy may release it.
type natMemoryReservation struct {
	budget *TransferMemoryBudget
	bytes  ByteCount
}

func reserveNatMemory(budget *TransferMemoryBudget, bytes ByteCount) (natMemoryReservation, bool) {
	if budget == nil {
		return natMemoryReservation{}, true
	}
	if !budget.TryReserve(bytes) {
		return natMemoryReservation{}, false
	}
	return natMemoryReservation{budget: budget, bytes: bytes}, true
}

func (r *natMemoryReservation) release() {
	if r.budget != nil {
		budget, bytes := r.budget, r.bytes
		*r = natMemoryReservation{}
		budget.Release(bytes)
	}
}

// split hands part of an already charged owner to its downstream packet.
// It changes neither a root counter nor its admission ceiling. The caller
// exclusively owns r; the returned value has exactly one release owner.
func (r *natMemoryReservation) split(bytes ByteCount) (natMemoryReservation, bool) {
	if bytes < 0 {
		return natMemoryReservation{}, false
	}
	if r.budget == nil {
		return natMemoryReservation{}, true
	}
	if r.bytes < bytes {
		return natMemoryReservation{}, false
	}
	part := natMemoryReservation{budget: r.budget, bytes: bytes}
	r.bytes -= bytes
	if r.bytes == 0 {
		*r = natMemoryReservation{}
	}
	return part, true
}

func (r *natMemoryReservation) moveTo(target *TransferMemoryBudget) bool {
	if r.budget == nil || target == nil {
		return r.budget == target
	}
	if !r.budget.tryMoveReservation(target, r.bytes) {
		return false
	}
	r.budget = target
	return true
}

func natPacketMemoryByteCount(packet []byte) ByteCount {
	return ByteCount(cap(packet)) + ByteCount(unsafe.Sizeof(TcpSendItem{})) + 128
}

const (
	natMemoryFragmentBytes = 16 * 1024
	natMemoryReadBytes     = 4 * 1024
	natMemoryBatchCount    = 2
)

// Fixed ledger: 128 KiB for the two 16-KiB fragment caches plus worst-case
// tiny-fragment geometric metadata/maps; 16 KiB reconstruction scratch;
// 64 KiB shared maps/channel backing,
// readiness buffers/workers and callback metadata; 32 KiB allocator slack.
// Packet owners
// in dispatch and flow queues are separately admitted, never multiplied into
// a guessed per-flow count. The strict profile has one send shard.
const natMemoryFixedBytes ByteCount = 240 * 1024

// A small, explicit working set for byte-budgeted NATs. Flow counts remain
// secondary FD safeguards; they cannot override exact byte admission. Larger
// unbudgeted provider/server profiles are unchanged.
func boundedNatMemorySettings(settings *LocalUserNatSettings) *LocalUserNatSettings {
	if settings.MemoryBudget == nil {
		return settings
	}
	copy := *settings
	copy.SendShardCount = 1
	copy.SequenceBufferSize = min(max(1, copy.SequenceBufferSize), 16)
	udp := *copy.UdpBufferSettings
	udp.MemoryBudget = copy.MemoryBudget
	udp.SequenceBufferSize = min(max(1, udp.SequenceBufferSize), 16)
	udp.WriteBatchSize = min(max(1, udp.WriteBatchSize), natMemoryBatchCount)
	udp.ReadBufferByteCount = min(max(1, udp.ReadBufferByteCount), natMemoryReadBytes)
	udp.ReceiveShardCount = min(max(1, udp.ReceiveShardCount), 2)
	udp.SocketReadShardCount = min(max(0, udp.SocketReadShardCount), 2)
	udp.Mtu = min(max(1, udp.Mtu), DefaultTunnelMtu)
	copy.UdpBufferSettings = &udp
	tcp := *copy.TcpBufferSettings
	tcp.MemoryBudget = copy.MemoryBudget
	tcp.ReturnQueueBudget = copy.MemoryBudget
	tcp.SequenceBufferSize = min(max(1, tcp.SequenceBufferSize), 16)
	tcp.ReadBufferByteCount = min(max(1, tcp.ReadBufferByteCount), natMemoryReadBytes)
	tcp.Mtu = min(max(1, tcp.Mtu), DefaultTunnelMtu)
	copy.TcpBufferSettings = boundedNatTcpMemorySettings(&tcp, copy.MemoryBudget)
	icmp := *copy.IcmpBufferSettings
	icmp.MemoryBudget = copy.MemoryBudget
	icmp.SequenceBufferSize = min(max(1, icmp.SequenceBufferSize), 8)
	icmp.ReadBufferByteCount = min(max(1, icmp.ReadBufferByteCount), natMemoryReadBytes)
	icmp.OutstandingLimit = min(max(1, icmp.OutstandingLimit), 2)
	icmp.Mtu = min(max(1, icmp.Mtu), DefaultTunnelMtu)
	copy.IcmpBufferSettings = &icmp
	return &copy
}

// The NAT and direct TCP buffer constructors share the same owned policy.
// Return an already-normalized instance unchanged so its sequence and NAT keep
// the exact settings identity; all normalization changes are made on a copy.
func boundedNatTcpMemorySettings(settings *TcpBufferSettings, budget *TransferMemoryBudget) *TcpBufferSettings {
	if budget == nil {
		return settings
	}
	tcp := *settings
	packetBytes := retainedMessageCapacity(ByteCount(max(1, tcp.Mtu))) + natPacketMemoryByteCount(nil)
	// The cold operation owns only one provider slot: keep both the existing
	// packet-root envelope and the complete wrapping/descriptor row inside it.
	tcp.MemoryColdWriteBatchSize = min(max(1, tcp.MemoryColdWriteBatchSize),
		max(1, tcp.WriteBatchSize), max(1, tcp.SequenceBufferSize),
		int(kib(8)/packetBytes),
		int((natProviderTcpReturnSlotBytes-natProviderPacketOperationFixedBytes)/
			(packetBytes+natProviderPacketOperationPerPacketBytes)))
	tcp.MemoryColdWriteBatchSize = max(1, tcp.MemoryColdWriteBatchSize)
	maximum := tcp.MemoryWriteBatchSize
	if maximum <= 0 {
		maximum = tcp.MemoryColdWriteBatchSize
	}
	// The hot operation owns one provider slot plus the separately funded
	// equal-sized flow workspace. Descriptor capacity is already in the
	// sequence's existing per-slot metadata claim. No count is a global policy.
	maximum = min(maximum, max(1, tcp.SequenceBufferSize),
		int(natProviderTcpReturnSlotBytes/packetBytes),
		int((2*natProviderTcpReturnSlotBytes-natProviderPacketOperationFixedBytes)/
			(packetBytes+natProviderPacketOperationPerPacketBytes)))
	tcp.WriteBatchSize = min(max(1, tcp.WriteBatchSize), max(1, maximum))
	tcp.MemoryColdWriteBatchSize = min(tcp.MemoryColdWriteBatchSize, tcp.WriteBatchSize)
	if tcp.WriteBatchSize == settings.WriteBatchSize &&
		tcp.MemoryColdWriteBatchSize == settings.MemoryColdWriteBatchSize {
		return settings
	}
	return &tcp
}

// Cold flows retain their constructor-normalized prepaid root claim. Optional queue and
// callback ownership is admitted separately at an actual ready-work boundary.
func natTcpFlowMemoryByteCount(s *TcpBufferSettings) ByteCount {
	s = boundedNatTcpMemorySettings(s, s.MemoryBudget)
	if s.MemoryBudget == nil || s.WriteBatchSize <= s.MemoryColdWriteBatchSize {
		return natTcpFundedFlowMemoryByteCount(s)
	}
	cold := *s
	cold.WriteBatchSize = s.MemoryColdWriteBatchSize
	return natTcpFundedFlowMemoryByteCount(&cold)
}

func natTcpFundedFlowMemoryByteCount(s *TcpBufferSettings) ByteCount {
	s = boundedNatTcpMemorySettings(s, s.MemoryBudget)
	// The configured producer slots, one queued batch, one callback-held batch,
	// and three regenerable controls. Chunking honors even MSS=1.
	// The remaining row covers the sequence, maps, channels, timers, socket
	// wrapper and the dedicated workers. Upload packet roots and return
	// chunks are separately charged for their complete lifetime.
	// The recovery worker's cursor/flight metadata is explicitly charged too.
	packets := s.MemoryColdWriteBatchSize + min(s.SequenceBufferSize, max(1, s.WriteBatchSize)) + s.WriteBatchSize + 3
	return kib(24) + natTcpReturnBatchWorkspaceByteCount(s) +
		ByteCount(unsafe.Sizeof(tcpReturnRecoveryFlight{})) + ByteCount(s.ReadBufferByteCount) +
		ByteCount(packets)*retainedMessageCapacity(ByteCount(s.Mtu)) +
		ByteCount(s.SequenceBufferSize)*128
}

// A larger callback borrows an exclusive witness after its read queue has
// reserved the complete incremental flow claim. Equal32KiB provider slots
// and independently regenerable controls retain their original ownership.
type tcpReturnBatchWorkspace struct {
	ctx            context.Context
	budget         *TransferMemoryBudget
	incrementBytes ByteCount
	maximumBatch   int
	funded         atomic.Bool
	inUse          atomic.Bool
}

// The fixed-cap8 comparator includes this scratch in its funded-flow total.
// The adaptive queue reserves that entire incremental total before it admits
// any root beyond the configured cold count or publishes a larger callback.
func natTcpReturnBatchWorkspaceByteCount(settings *TcpBufferSettings) ByteCount {
	if settings.MemoryBudget != nil && settings.MemoryColdWriteBatchSize < settings.WriteBatchSize {
		return natProviderTcpReturnSlotBytes
	}
	return 0
}

// A cold constructor publishes identity only. The queue may publish funded
// permission after exact incremental admission; cancellation forbids borrow.
func newTcpReturnBatchWorkspace(ctx context.Context, settings *TcpBufferSettings) *tcpReturnBatchWorkspace {
	settings = boundedNatTcpMemorySettings(settings, settings.MemoryBudget)
	if natTcpReturnBatchWorkspaceByteCount(settings) == 0 {
		return nil
	}
	return &tcpReturnBatchWorkspace{ctx: ctx, budget: settings.MemoryBudget, maximumBatch: settings.WriteBatchSize,
		incrementBytes: natTcpFundedFlowMemoryByteCount(settings) - natTcpFlowMemoryByteCount(settings)}
}

// Borrowing does not release or move the queue's incremental reservation.
// Only callback return permits the queue to consider demotion.
func (self *tcpReturnBatchWorkspace) acquire() bool {
	if self == nil || !self.funded.Load() || self.ctx.Err() != nil || !self.inUse.CompareAndSwap(false, true) {
		return false
	}
	if self.ctx.Err() != nil {
		self.inUse.Store(false)
		return false
	}
	return true
}

// The original callback is the only owner allowed to end a successful borrow.
func (self *tcpReturnBatchWorkspace) release() {
	if !self.inUse.CompareAndSwap(true, false) {
		panic("TCP return workspace released without its callback owner")
	}
}

// A protocol packet carries no application bytes. Count and existing flow
// limits own its lifetime; no DATA credit or separate byte reserve is used.
func natProtocolOnlyPackets(packets [][]byte) bool {
	if len(packets) == 0 {
		return false
	}
	for _, packet := range packets {
		if !protocolOnlyFrame(protocol.MessageType_IpIpPacketToProvider, true, packet) {
			return false
		}
	}
	return true
}

// Only actual ToProvider TCP packets enter the byte-free provider workspace;
// control enums and empty lists do not confer NAT packet authority.
func natProtocolOnlyFrames(frames []*protocol.Frame) bool {
	if len(frames) == 0 {
		return false
	}
	for _, frame := range frames {
		if frame == nil || frame.MessageType != protocol.MessageType_IpIpPacketToProvider ||
			!protocolOnlyFrame(frame.MessageType, frame.Raw, frame.MessageBytes) {
			return false
		}
	}
	return true
}

// Small ACK/RST packets can pass pending DATA without consuming sequence
// space. SYN/FIN retain their ordinary ordering; this is not byte admission.
func natControlPackets(packets [][]byte) bool {
	if len(packets) == 0 || len(packets) > 16 {
		return false
	}
	for _, packet := range packets {
		if len(packet) == 0 || cap(packet) > 512 || isIpFragmentPacket(packet) {
			return false
		}
		var protocol ipProtocolNumber
		var transport []byte
		var ok bool
		if packet[0]>>4 == 4 {
			protocol, _, _, transport, ok = parseIpv4(packet)
		} else if packet[0]>>4 == 6 {
			protocol, _, _, transport, ok = parseIpv6(packet)
		}
		if !ok || protocol != ipProtocolNumberTcp || len(transport) < 20 ||
			int(transport[12]>>4)*4 != len(transport) || transport[13]&0x07 != 0 && transport[13]&0x04 == 0 ||
			transport[13]&0x14 == 0 {
			return false
		}
	}
	return true
}

func (self *ConnectionState) natPacketizationByteLimit(mtu int, maximumPackets int) int {
	header := Ipv6HeaderSize + TcpHeaderSizeWithoutExtensions
	if self.ipVersion == 4 {
		header = Ipv4HeaderSizeWithoutExtensions + TcpHeaderSizeWithoutExtensions
	}
	options := 0
	if self.enableTimestamp {
		options = tcpTimestampOptionByteCount
	}
	payload := self.clampPathMtu(mtu) - header - options
	if self.peerMss != 0 {
		payload = min(payload, int(self.peerMss)-options)
	}
	return max(1, maximumPackets) * max(1, payload)
}

func natUdpFlowMemoryByteCount(s *UdpBufferSettings) ByteCount {
	// Covers the portable reader/writer fallback too, plus one complete
	// datagram and every pool-rounded fragment coexisting during packetization.
	// At a 576-byte path MTU, several small fragments each own a 2-KiB root;
	// multiplying only the payload length undercounts this working set.
	acceptedReadBytes := udpReadScratchByteCount(s.ReadBufferByteCount) - 1
	packetization := ByteCount(0)
	for _, ipVersion := range [...]int{4, 6} {
		header := Ipv4HeaderSizeWithoutExtensions
		fragmentHeader := header
		if ipVersion == 6 {
			header = Ipv6HeaderSize
			fragmentHeader = header + ipv6FragmentHeaderSize
		}
		working := retainedMessageCapacity(ByteCount(header + UdpHeaderSize + acceptedReadBytes))
		mtu := min(s.Mtu, ipMinimumPathMtu(ipVersion))
		fragmentPayload := (mtu - fragmentHeader) &^ 7
		if fragmentPayload >= 8 {
			count := (acceptedReadBytes + UdpHeaderSize + fragmentPayload - 1) / fragmentPayload
			working += ByteCount(count)*(retainedMessageCapacity(ByteCount(s.Mtu))+24) + 128
		}
		packetization = max(packetization, working)
	}
	// Include the reader's extra detection byte in the prepaid flow claim.
	return kib(16) + ByteCount(acceptedReadBytes+1) + packetization +
		ByteCount(s.SequenceBufferSize)*16
}

func natIcmpFlowMemoryByteCount(s *IcmpBufferSettings) ByteCount {
	return kib(16) + ByteCount(2+s.OutstandingLimit*2)*ByteCount(s.ReadBufferByteCount+256) +
		ByteCount(s.SequenceBufferSize)*16 + retainedMessageCapacity(ByteCount(s.Mtu))
}

func (item *UdpSendItem) release() {
	MessagePoolReturn(item.ipPacket)
	item.ipPacket = nil
	item.udp = parsedUdp{}
	item.memory.release()
}

func (item *TcpSendItem) clearPacketViews() {
	// Flags and sequence numbers are still needed after a payload handoff.
	item.tcp.sourceIp = nil
	item.tcp.destinationIp = nil
	item.tcp.options = nil
	item.tcp.payload = nil
}

func (item *TcpSendItem) release() {
	MessagePoolReturn(item.ipPacket)
	item.ipPacket = nil
	item.clearPacketViews()
	item.memory.release()
	item.admissionCapacity.notify()
}

func (item *IcmpSendItem) release() {
	MessagePoolReturn(item.ipPacket)
	item.ipPacket = nil
	item.icmp = parsedIcmp{}
	item.memory.release()
}
