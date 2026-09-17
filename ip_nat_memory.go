package connect

import (
	"errors"
	"unsafe"
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
// 16 KiB precharged ACK/RST admission; 64 KiB shared maps/channel backing,
// readiness buffers/workers and callback metadata; 32 KiB allocator slack.
// Packet owners
// in dispatch and flow queues are separately admitted, never multiplied into
// a guessed per-flow count. The strict profile has one send shard.
const natMemoryFixedBytes ByteCount = 256 * 1024

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
	tcp.WriteBatchSize = min(max(1, tcp.WriteBatchSize), natMemoryBatchCount)
	tcp.ReadBufferByteCount = min(max(1, tcp.ReadBufferByteCount), natMemoryReadBytes)
	tcp.Mtu = min(max(1, tcp.Mtu), DefaultTunnelMtu)
	copy.TcpBufferSettings = &tcp
	icmp := *copy.IcmpBufferSettings
	icmp.MemoryBudget = copy.MemoryBudget
	icmp.SequenceBufferSize = min(max(1, icmp.SequenceBufferSize), 8)
	icmp.ReadBufferByteCount = min(max(1, icmp.ReadBufferByteCount), natMemoryReadBytes)
	icmp.OutstandingLimit = min(max(1, icmp.OutstandingLimit), 2)
	icmp.Mtu = min(max(1, icmp.Mtu), DefaultTunnelMtu)
	copy.IcmpBufferSettings = &icmp
	return &copy
}

func natTcpFlowMemoryByteCount(s *TcpBufferSettings) ByteCount {
	// Two packetization slots, two callback queue slots, two callback-held
	// packets, and three regenerable controls. Chunking honors even MSS=1.
	// The remaining row covers the sequence, maps, channels, timers, socket
	// wrapper and the dedicated workers. Upload packet roots and return
	// chunks are separately charged for their complete lifetime.
	packets := 2 + min(s.SequenceBufferSize, max(1, s.WriteBatchSize)) + s.WriteBatchSize + 3
	return kib(24) + ByteCount(s.ReadBufferByteCount) +
		ByteCount(packets)*retainedMessageCapacity(ByteCount(s.Mtu)) +
		ByteCount(s.SequenceBufferSize)*128
}

// Header-only ACK/RST packets need no data reservation: their separately
// bounded 16-KiB allowance is already part of the NAT's fixed reservation.
// This breaks the full-replay-budget -> refused-ACK -> no-release cycle.
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

func (self *ConnectionState) natPacketizationByteLimit(mtu int) int {
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
	return natMemoryBatchCount * max(1, payload)
}

func natUdpFlowMemoryByteCount(s *UdpBufferSettings) ByteCount {
	// Covers the portable reader/writer fallback too, plus one complete
	// datagram and every pool-rounded fragment coexisting during packetization.
	// At a 576-byte path MTU, several small fragments each own a 2-KiB root;
	// multiplying only the payload length undercounts this working set.
	packetization := ByteCount(0)
	for _, ipVersion := range [...]int{4, 6} {
		header := Ipv4HeaderSizeWithoutExtensions
		fragmentHeader := header
		if ipVersion == 6 {
			header = Ipv6HeaderSize
			fragmentHeader = header + ipv6FragmentHeaderSize
		}
		working := retainedMessageCapacity(ByteCount(header + UdpHeaderSize + s.ReadBufferByteCount))
		mtu := min(s.Mtu, ipMinimumPathMtu(ipVersion))
		fragmentPayload := (mtu - fragmentHeader) &^ 7
		if fragmentPayload >= 8 {
			count := (s.ReadBufferByteCount + UdpHeaderSize + fragmentPayload - 1) / fragmentPayload
			working += ByteCount(count)*(retainedMessageCapacity(ByteCount(s.Mtu))+24) + 128
		}
		packetization = max(packetization, working)
	}
	return kib(16) + ByteCount(s.ReadBufferByteCount) + packetization +
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
}

func (item *IcmpSendItem) release() {
	MessagePoolReturn(item.ipPacket)
	item.ipPacket = nil
	item.icmp = parsedIcmp{}
	item.memory.release()
}
