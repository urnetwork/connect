package connect

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	mathrand "math/rand"
	"net"
	// "syscall"
	// "runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"

	"golang.org/x/exp/maps"

	// "google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

// implements user-space NAT (UNAT) and packet inspection
// The UNAT emulates a raw socket using user-space sockets.

// use 0 for deadlock testing
const defaultIpBufferSize = 1024

// packets are written directly into the receiver tap/tun interface,
// so the packet size must not exceed the device interface mtu (1440).
// this is a contract with the devices and must not be raised.
const DefaultMtu = 1440
const Ipv4HeaderSizeWithoutExtensions = 20
const Ipv6HeaderSize = 40
const UdpHeaderSize = 8
const TcpHeaderSizeWithoutExtensions = 20

const debugVerifyHeaders = false

// send from a raw socket
// note `ipProtocol` is not supplied. The implementation must do a packet inspection to determine protocol
// `provideMode` is the relationship between the source and this device
type SendPacketFunction func(provideMode protocol.ProvideMode, packet []byte, timeout time.Duration) bool

// receive into a raw socket
type ReceivePacketFunction func(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte)

type UserNatClient interface {
	// `SendPacketFunction`
	SendPacket(source TransferPath, provideMode protocol.ProvideMode, packet []byte, timeout time.Duration) bool
	Close()
	Shuffle()

	SecurityPolicyStats(reset bool) SecurityPolicyStats

	// allow traffic that fails the security policy of the peers to stay local
	SetLocalSecurityBypass(localSecurityBypass bool)
}

func DefaultUdpBufferSettings() *UdpBufferSettings {
	return DefaultUdpBufferSettingsWithBufferSize(defaultIpBufferSize)
}

func DefaultUdpBufferSettingsWithBufferSize(bufferSize int) *UdpBufferSettings {
	return &UdpBufferSettings{
		ReadTimeout:         300 * time.Second,
		WriteTimeout:        15 * time.Second,
		IdleTimeout:         300 * time.Second,
		Mtu:                 DefaultMtu,
		ReadBufferByteCount: DefaultMtu,
		SequenceBufferSize:  bufferSize,
		WriteBatchSize:      64,
		UserLimit:           0,
		MaxWindowSize:       uint32(mib(1)),
		ConnectSettings:     *DefaultConnectSettings(),
	}
}

func DefaultTcpBufferSettings() *TcpBufferSettings {
	return DefaultTcpBufferSettingsWithBufferSize(defaultIpBufferSize)
}

func DefaultTcpBufferSettingsWithBufferSize(bufferSize int) *TcpBufferSettings {
	tcpBufferSettings := &TcpBufferSettings{
		// ConnectTimeout:     60 * time.Second,
		ReadTimeout:        300 * time.Second,
		WriteTimeout:       15 * time.Second,
		AckCompressTimeout: 50 * time.Millisecond,
		IdleTimeout:        300 * time.Second,
		SequenceBufferSize: bufferSize,
		Mtu:                DefaultMtu,
		// large socket reads are split into mtu-sized data packets by `DataPackets`
		ReadBufferByteCount: int(kib(64)),
		WriteBatchSize:      64,
		MinWindowSize:       uint32(kib(64)),
		MaxWindowSize:       uint32(mib(1)),
		UserLimit:           0,
		ConnectSettings:     *DefaultConnectSettings(),
	}
	return tcpBufferSettings
}

func DefaultLocalUserNatSettings() *LocalUserNatSettings {
	return DefaultLocalUserNatSettingsWithBufferSize(defaultIpBufferSize)
}

func DefaultLocalUserNatSettingsWithBufferSize(bufferSize int) *LocalUserNatSettings {
	return &LocalUserNatSettings{
		SequenceBufferSize: bufferSize,
		SendShardCount:     1,
		// BufferTimeout:      15 * time.Second,
		UdpBufferSettings: DefaultUdpBufferSettingsWithBufferSize(bufferSize),
		TcpBufferSettings: DefaultTcpBufferSettingsWithBufferSize(bufferSize),
	}
}

type LocalUserNatSettings struct {
	SequenceBufferSize int
	// the number of send dispatch shards.
	// flows are pinned to a shard by their address tuple, which preserves
	// the per flow lossless in-order assumption.
	// the default is 1, since the per flow sequences already parallelize the
	// heavy work and sharding measured neutral at eight parallel flows.
	// note the tcp/udp user limits apply per shard.
	SendShardCount int
	// BufferTimeout      time.Duration
	UdpBufferSettings *UdpBufferSettings
	TcpBufferSettings *TcpBufferSettings

	// Log, when set, is used by the local user nat and its udp/tcp buffers
	// and sequences (propagated to the buffer settings `Log` fields that are
	// nil). nil resolves to `DefaultLogger()`.
	Log Logger
}

// forwards packets using user space sockets
// this assumes transfer between the packet source and this is lossless and in order,
// so the protocol stack implementations do not implement any retransmit logic
type LocalUserNat struct {
	ctx       context.Context
	cancel    context.CancelFunc
	clientTag string
	log       Logger

	sendPackets chan *SendPacket

	settings *LocalUserNatSettings

	// receive callback
	receiveCallbacks *CallbackList[ReceivePacketFunction]
}

func NewLocalUserNatWithDefaults(ctx context.Context, clientTag string) *LocalUserNat {
	return NewLocalUserNat(ctx, clientTag, DefaultLocalUserNatSettings())
}

func NewLocalUserNat(ctx context.Context, clientTag string, settings *LocalUserNatSettings) *LocalUserNat {
	cancelCtx, cancel := context.WithCancel(ctx)

	log := loggerOrDefault(settings.Log)
	// propagate so a nat-level logger covers the udp/tcp buffers and sequences
	if settings.UdpBufferSettings != nil && settings.UdpBufferSettings.Log == nil {
		settings.UdpBufferSettings.Log = log
	}
	if settings.TcpBufferSettings != nil && settings.TcpBufferSettings.Log == nil {
		settings.TcpBufferSettings.Log = log
	}
	localUserNat := &LocalUserNat{
		ctx:              cancelCtx,
		cancel:           cancel,
		clientTag:        clientTag,
		log:              log,
		sendPackets:      make(chan *SendPacket, settings.SequenceBufferSize),
		settings:         settings,
		receiveCallbacks: NewCallbackList[ReceivePacketFunction](),
	}
	go HandleError(localUserNat.Run)

	return localUserNat
}

func (self *LocalUserNat) SecurityPolicyStats(reset bool) SecurityPolicyStats {
	return SecurityPolicyStats{}
}

func (self *LocalUserNat) SendPacketWithTimeout(source TransferPath, provideMode protocol.ProvideMode,
	packet []byte, timeout time.Duration) bool {
	return self.SendPacketsWithTimeout(source, provideMode, [][]byte{packet}, timeout)
}

// `SendPacketWithTimeout` for a batch of packets from one source.
// queueing a batch is one channel operation, which reduces wakeups when the
// caller already has multiple packets in hand.
// on success the local user nat owns `packets` and each packet in it.
// on failure the caller keeps ownership of all of the packets.
func (self *LocalUserNat) SendPacketsWithTimeout(source TransferPath, provideMode protocol.ProvideMode,
	packets [][]byte, timeout time.Duration) bool {
	sendPacket := &SendPacket{
		source:      source,
		provideMode: provideMode,
		packets:     packets,
	}
	if timeout < 0 {
		select {
		case <-self.ctx.Done():
			return false
		case self.sendPackets <- sendPacket:
			return true
		}
	} else if 0 == timeout {
		select {
		case <-self.ctx.Done():
			return false
		case self.sendPackets <- sendPacket:
			return true
		default:
			// full
			return false
		}
	} else {
		select {
		case <-self.ctx.Done():
			return false
		case self.sendPackets <- sendPacket:
			return true
		case <-time.After(timeout):
			// full
			return false
		}
	}
}

// `SendPacketFunction`
func (self *LocalUserNat) SendPacket(source TransferPath, provideMode protocol.ProvideMode, packet []byte, timeout time.Duration) bool {
	return self.SendPacketWithTimeout(source, provideMode, packet, timeout)
}

// `SendPackets` for a batch of packets from one source. see `SendPacketsWithTimeout`.
func (self *LocalUserNat) SendPackets(source TransferPath, provideMode protocol.ProvideMode, packets [][]byte, timeout time.Duration) bool {
	return self.SendPacketsWithTimeout(source, provideMode, packets, timeout)
}

// func (self *LocalUserNat) ReceiveN(source TransferPath, provideMode protocol.ProvideMode, packet []byte, n int) {
//     self.Receive(source, provideMode, packet[0:n])
// }

func (self *LocalUserNat) AddReceivePacketCallback(receiveCallback ReceivePacketFunction) func() {
	callbackId := self.receiveCallbacks.Add(receiveCallback)
	return func() {
		self.receiveCallbacks.Remove(callbackId)
	}
}

// func (self *LocalUserNat) RemoveReceivePacketCallback(receiveCallback ReceivePacketFunction) {
//     self.receiveCallbacks.Remove(receiveCallback)
// }

// `ReceivePacketFunction`
func (self *LocalUserNat) receive(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {
	for _, receiveCallback := range self.receiveCallbacks.Get() {
		HandleError(func() {
			receiveCallback(source, provideMode, ipPath, packet)
		})
	}
}

func (self *LocalUserNat) Run() {
	defer self.cancel()

	shardCount := max(1, self.settings.SendShardCount)
	if shardCount == 1 {
		self.runSendShard(self.sendPackets)
		return
	}

	// shard the send dispatch. flows are pinned to a shard by their address
	// tuple, which preserves the per flow lossless in-order assumption.
	shardSendPackets := make([]chan *SendPacket, shardCount)
	for i := 0; i < shardCount; i += 1 {
		sendPackets := make(chan *SendPacket, self.settings.SequenceBufferSize)
		shardSendPackets[i] = sendPackets
		go HandleError(func() {
			self.runSendShard(sendPackets)
		}, self.cancel)
	}

	forward := func(shard int, sendPacket *SendPacket) bool {
		select {
		case <-self.ctx.Done():
			for _, packet := range sendPacket.packets {
				MessagePoolReturn(packet)
			}
			return false
		case shardSendPackets[shard] <- sendPacket:
			return true
		}
	}

	route := func(sendPacket *SendPacket) bool {
		if len(sendPacket.packets) == 0 {
			return true
		}
		// common case: all packets in the batch are for one shard
		shard := sendShard(sendPacket.packets[0], shardCount)
		split := false
		for _, packet := range sendPacket.packets[1:] {
			if sendShard(packet, shardCount) != shard {
				split = true
				break
			}
		}
		if !split {
			return forward(shard, sendPacket)
		}

		shardPackets := make([][][]byte, shardCount)
		for _, packet := range sendPacket.packets {
			packetShard := sendShard(packet, shardCount)
			shardPackets[packetShard] = append(shardPackets[packetShard], packet)
		}
		for packetShard, packets := range shardPackets {
			if len(packets) == 0 {
				continue
			}
			shardSendPacket := &SendPacket{
				source:      sendPacket.source,
				provideMode: sendPacket.provideMode,
				packets:     packets,
			}
			if !forward(packetShard, shardSendPacket) {
				for _, returnPackets := range shardPackets[packetShard+1:] {
					for _, packet := range returnPackets {
						MessagePoolReturn(packet)
					}
				}
				return false
			}
		}
		return true
	}

send:
	for {
		select {
		case <-self.ctx.Done():
			return
		case sendPacket := <-self.sendPackets:
			if !route(sendPacket) {
				return
			}
			// opportunistically drain queued packets to reduce wakeups
			for {
				select {
				case <-self.ctx.Done():
					return
				case sendPacket := <-self.sendPackets:
					if !route(sendPacket) {
						return
					}
				default:
					continue send
				}
			}
		}
	}
}

// pins a packet to a send shard by its address tuple.
// all packets of a flow map to the same shard.
func sendShard(ipPacket []byte, shardCount int) int {
	// fnv-1a over the address tuple
	hash := uint32(2166136261)
	hashBytes := func(b []byte) {
		for _, c := range b {
			hash = (hash ^ uint32(c)) * 16777619
		}
	}
	if 0 < len(ipPacket) {
		switch uint8(ipPacket[0]) >> 4 {
		case 4:
			if Ipv4HeaderSizeWithoutExtensions <= len(ipPacket) {
				hashBytes(ipPacket[12:20])
				headerByteCount := int(ipPacket[0]&0xf) * 4
				if headerByteCount+4 <= len(ipPacket) {
					hashBytes(ipPacket[headerByteCount : headerByteCount+4])
				}
			}
		case 6:
			if Ipv6HeaderSize+4 <= len(ipPacket) {
				hashBytes(ipPacket[8:40])
				hashBytes(ipPacket[Ipv6HeaderSize : Ipv6HeaderSize+4])
			}
		}
	}
	return int(hash % uint32(shardCount))
}

func (self *LocalUserNat) runSendShard(sendPackets chan *SendPacket) {
	defer self.cancel()

	udp4Buffer := NewUdp4Buffer(self.ctx, self.receive, self.settings.UdpBufferSettings)
	udp6Buffer := NewUdp6Buffer(self.ctx, self.receive, self.settings.UdpBufferSettings)
	tcp4Buffer := NewTcp4Buffer(self.ctx, self.receive, self.settings.TcpBufferSettings)
	tcp6Buffer := NewTcp6Buffer(self.ctx, self.receive, self.settings.TcpBufferSettings)

	// parsed per-packet views. these are copied by value into the send items,
	// so the locals can be reused across packets.
	var udpPacket parsedUdp
	var tcpPacket parsedTcp

	handleIpPacket := func(source TransferPath, provideMode protocol.ProvideMode, ipPacket []byte) {
		if len(ipPacket) == 0 {
			MessagePoolReturn(ipPacket)
			return
		}
		ipVersion := uint8(ipPacket[0]) >> 4
		switch ipVersion {
		case 4:
			ipProtocol, sourceIp, destinationIp, transport, ok := parseIpv4(ipPacket)
			if !ok {
				// malformed, drop
				MessagePoolReturn(ipPacket)
				return
			}
			switch ipProtocol {
			case layers.IPProtocolUDP:
				if !parseUdpPacket(sourceIp, destinationIp, transport, &udpPacket) {
					// malformed, drop
					MessagePoolReturn(ipPacket)
					return
				}
				c := func() bool {
					success, err := udp4Buffer.send(
						source,
						provideMode,
						&udpPacket,
						self.settings.UdpBufferSettings.WriteTimeout,
						ipPacket,
					)
					return success && err == nil
				}
				if self.log.V(2).Enabled() {
					TraceWithReturn(
						fmt.Sprintf("[lnr]send udp4 %s<-%s s(%s)", self.clientTag, source.SourceId, source.StreamId),
						c,
					)
				} else {
					c()
				}
			case layers.IPProtocolTCP:
				if !parseTcpPacket(sourceIp, destinationIp, transport, &tcpPacket) {
					// malformed, drop
					MessagePoolReturn(ipPacket)
					return
				}
				c := func() bool {
					success, err := tcp4Buffer.send(
						source,
						provideMode,
						&tcpPacket,
						self.settings.TcpBufferSettings.WriteTimeout,
						ipPacket,
					)
					return success && err == nil
				}
				if self.log.V(2).Enabled() {
					TraceWithReturn(
						fmt.Sprintf("[lnr]send tcp4 %s<-%s s(%s)", self.clientTag, source.SourceId, source.StreamId),
						c,
					)
				} else {
					c()
				}
			default:
				// no support for this protocol, drop
				MessagePoolReturn(ipPacket)
			}
		case 6:
			ipProtocol, sourceIp, destinationIp, transport, ok := parseIpv6(ipPacket)
			if !ok {
				// malformed, drop
				MessagePoolReturn(ipPacket)
				return
			}
			switch ipProtocol {
			case layers.IPProtocolUDP:
				if !parseUdpPacket(sourceIp, destinationIp, transport, &udpPacket) {
					// malformed, drop
					MessagePoolReturn(ipPacket)
					return
				}
				c := func() bool {
					success, err := udp6Buffer.send(
						source,
						provideMode,
						&udpPacket,
						self.settings.UdpBufferSettings.WriteTimeout,
						ipPacket,
					)
					return success && err == nil
				}
				if self.log.V(2).Enabled() {
					TraceWithReturn(
						fmt.Sprintf("[lnr]send udp6 %s<-%s s(%s)", self.clientTag, source.SourceId, source.StreamId),
						c,
					)
				} else {
					c()
				}
			case layers.IPProtocolTCP:
				if !parseTcpPacket(sourceIp, destinationIp, transport, &tcpPacket) {
					// malformed, drop
					MessagePoolReturn(ipPacket)
					return
				}
				c := func() bool {
					success, err := tcp6Buffer.send(
						source,
						provideMode,
						&tcpPacket,
						self.settings.TcpBufferSettings.WriteTimeout,
						ipPacket,
					)
					return success && err == nil
				}
				if self.log.V(2).Enabled() {
					TraceWithReturn(
						fmt.Sprintf("[lnr]send tcp6 %s<-%s s(%s)", self.clientTag, source.SourceId, source.StreamId),
						c,
					)
				} else {
					c()
				}
			default:
				// no support for this protocol, drop
				MessagePoolReturn(ipPacket)
			}
		default:
			// unknown IP version, drop
			MessagePoolReturn(ipPacket)
		}
	}

	handleSendPacket := func(sendPacket *SendPacket) {
		for _, ipPacket := range sendPacket.packets {
			handleIpPacket(sendPacket.source, sendPacket.provideMode, ipPacket)
		}
	}

send:
	for {
		select {
		case <-self.ctx.Done():
			return
		case sendPacket := <-sendPackets:
			handleSendPacket(sendPacket)
			// opportunistically drain queued packets to reduce wakeups
			for {
				select {
				case <-self.ctx.Done():
					return
				case sendPacket := <-sendPackets:
					handleSendPacket(sendPacket)
				default:
					continue send
				}
			}
		}
	}
}

func (self *LocalUserNat) Close() {
	self.cancel()
}

// a batch of packets from one source
type SendPacket struct {
	source      TransferPath
	provideMode protocol.ProvideMode
	packets     [][]byte
}

// comparable
type BufferId4 struct {
	source          TransferPath
	sourceIp        [4]byte
	sourcePort      int
	destinationIp   [4]byte
	destinationPort int
}

func NewBufferId4(source TransferPath, sourceIp net.IP, sourcePort int, destinationIp net.IP, destinationPort int) BufferId4 {
	return BufferId4{
		source:          source,
		sourceIp:        [4]byte(sourceIp),
		sourcePort:      sourcePort,
		destinationIp:   [4]byte(destinationIp),
		destinationPort: destinationPort,
	}
}

// comparable
type BufferId6 struct {
	source          TransferPath
	sourceIp        [16]byte
	sourcePort      int
	destinationIp   [16]byte
	destinationPort int
}

func NewBufferId6(source TransferPath, sourceIp net.IP, sourcePort int, destinationIp net.IP, destinationPort int) BufferId6 {
	return BufferId6{
		source:          source,
		sourceIp:        [16]byte(sourceIp),
		sourcePort:      sourcePort,
		destinationIp:   [16]byte(destinationIp),
		destinationPort: destinationPort,
	}
}

type UdpBufferSettings struct {
	// nil resolves to the local user nat `Log`
	Log                 Logger
	ReadTimeout         time.Duration
	WriteTimeout        time.Duration
	IdleTimeout         time.Duration
	Mtu                 int
	ReadBufferByteCount int
	SequenceBufferSize  int
	// the maximum number of payloads to process under a single write deadline.
	// udp datagrams cannot be coalesced, so each payload is one socket write.
	WriteBatchSize int
	// the number of open sockets per user
	// uses an lru cleanup where new sockets over the limit close old sockets
	UserLimit     int
	MaxWindowSize uint32

	ConnectSettings
}

// minimal parsed views of packets on the send path.
// these avoid the gopacket decode allocations in the hot dispatch.
// all slices alias the backing ip packet, which stays valid while the
// owning send item holds the packet.

type parsedUdp struct {
	sourceIp        net.IP
	destinationIp   net.IP
	sourcePort      layers.UDPPort
	destinationPort layers.UDPPort
	payload         []byte
}

type parsedTcp struct {
	sourceIp        net.IP
	destinationIp   net.IP
	sourcePort      layers.TCPPort
	destinationPort layers.TCPPort
	fin             bool
	syn             bool
	rst             bool
	psh             bool
	ack             bool
	seq             uint32
	ackNumber       uint32
	windowSize      uint16
	options         []byte
	payload         []byte
}

func (self *parsedTcp) flagsString() string {
	flags := []string{}
	if self.fin {
		flags = append(flags, "FIN")
	}
	if self.syn {
		flags = append(flags, "SYN")
	}
	if self.rst {
		flags = append(flags, "RST")
	}
	if self.psh {
		flags = append(flags, "PSH")
	}
	if self.ack {
		flags = append(flags, "ACK")
	}
	return strings.Join(flags, ", ")
}

// parses the ipv4 header. the returned slices alias `ipPacket`.
func parseIpv4(ipPacket []byte) (ipProtocol layers.IPProtocol, sourceIp net.IP, destinationIp net.IP, transport []byte, ok bool) {
	if len(ipPacket) < Ipv4HeaderSizeWithoutExtensions {
		return
	}
	headerByteCount := int(ipPacket[0]&0xf) * 4
	totalByteCount := int(binary.BigEndian.Uint16(ipPacket[2:4]))
	if headerByteCount < Ipv4HeaderSizeWithoutExtensions || totalByteCount < headerByteCount || len(ipPacket) < totalByteCount {
		return
	}
	ipProtocol = layers.IPProtocol(ipPacket[9])
	sourceIp = net.IP(ipPacket[12:16])
	destinationIp = net.IP(ipPacket[16:20])
	transport = ipPacket[headerByteCount:totalByteCount]
	ok = true
	return
}

// parses the ipv6 header. the returned slices alias `ipPacket`.
// extension headers are not walked, matching the previous decode behavior
// which dropped non tcp/udp next headers.
func parseIpv6(ipPacket []byte) (ipProtocol layers.IPProtocol, sourceIp net.IP, destinationIp net.IP, transport []byte, ok bool) {
	if len(ipPacket) < Ipv6HeaderSize {
		return
	}
	payloadByteCount := int(binary.BigEndian.Uint16(ipPacket[4:6]))
	if len(ipPacket) < Ipv6HeaderSize+payloadByteCount {
		return
	}
	ipProtocol = layers.IPProtocol(ipPacket[6])
	sourceIp = net.IP(ipPacket[8:24])
	destinationIp = net.IP(ipPacket[24:40])
	transport = ipPacket[Ipv6HeaderSize : Ipv6HeaderSize+payloadByteCount]
	ok = true
	return
}

// parses a udp packet into `udp`. the slices alias the backing packet.
func parseUdpPacket(sourceIp net.IP, destinationIp net.IP, transport []byte, udp *parsedUdp) bool {
	if len(transport) < UdpHeaderSize {
		return false
	}
	udpByteCount := int(binary.BigEndian.Uint16(transport[4:6]))
	if udpByteCount < UdpHeaderSize || len(transport) < udpByteCount {
		return false
	}
	udp.sourceIp = sourceIp
	udp.destinationIp = destinationIp
	udp.sourcePort = layers.UDPPort(binary.BigEndian.Uint16(transport[0:2]))
	udp.destinationPort = layers.UDPPort(binary.BigEndian.Uint16(transport[2:4]))
	udp.payload = transport[UdpHeaderSize:udpByteCount]
	return true
}

// parses a tcp packet into `tcp`. the slices alias the backing packet.
func parseTcpPacket(sourceIp net.IP, destinationIp net.IP, transport []byte, tcp *parsedTcp) bool {
	if len(transport) < TcpHeaderSizeWithoutExtensions {
		return false
	}
	headerByteCount := int(transport[12]>>4) * 4
	if headerByteCount < TcpHeaderSizeWithoutExtensions || len(transport) < headerByteCount {
		return false
	}
	flags := transport[13]
	tcp.sourceIp = sourceIp
	tcp.destinationIp = destinationIp
	tcp.sourcePort = layers.TCPPort(binary.BigEndian.Uint16(transport[0:2]))
	tcp.destinationPort = layers.TCPPort(binary.BigEndian.Uint16(transport[2:4]))
	tcp.seq = binary.BigEndian.Uint32(transport[4:8])
	tcp.ackNumber = binary.BigEndian.Uint32(transport[8:12])
	tcp.fin = (flags & 0x01) != 0
	tcp.syn = (flags & 0x02) != 0
	tcp.rst = (flags & 0x04) != 0
	tcp.psh = (flags & 0x08) != 0
	tcp.ack = (flags & 0x10) != 0
	tcp.windowSize = binary.BigEndian.Uint16(transport[14:16])
	tcp.options = transport[TcpHeaderSizeWithoutExtensions:headerByteCount]
	tcp.payload = transport[headerByteCount:]
	return true
}

// tcp flag bits
const (
	tcpFlagFin = byte(0x01)
	tcpFlagSyn = byte(0x02)
	tcpFlagRst = byte(0x04)
	tcpFlagPsh = byte(0x08)
	tcpFlagAck = byte(0x10)
)

// ones' complement sum in the style of rfc 1071.
// an odd final byte is padded high.
func checksumAdd(sum uint32, b []byte) uint32 {
	i := 0
	for ; i+1 < len(b); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(b[i : i+2]))
	}
	if i < len(b) {
		sum += uint32(b[i]) << 8
	}
	return sum
}

func checksumFinish(sum uint32) uint16 {
	for 0xffff < sum {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

// computes the transport checksum with the ipv4 or ipv6 pseudo header.
// the two pseudo headers sum identically for transport lengths that fit
// in 16 bits.
func transportChecksum(ipProtocol layers.IPProtocol, packetSourceIp net.IP, packetDestinationIp net.IP, transport []byte) uint16 {
	sum := checksumAdd(0, packetSourceIp)
	sum = checksumAdd(sum, packetDestinationIp)
	sum += uint32(ipProtocol)
	sum += uint32(len(transport))
	return checksumFinish(checksumAdd(sum, transport))
}

// writes an ipv4 header with no options.
// `packet` must be sized to the full packet.
func writeIpv4Header(packet []byte, ipProtocol layers.IPProtocol, packetSourceIp net.IP, packetDestinationIp net.IP) {
	// version 4, header length 5 words
	packet[0] = 0x45
	// tos
	packet[1] = 0
	binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
	// id, flags, fragment offset
	packet[4] = 0
	packet[5] = 0
	packet[6] = 0
	packet[7] = 0
	// ttl
	packet[8] = 64
	packet[9] = byte(ipProtocol)
	// checksum, set below
	packet[10] = 0
	packet[11] = 0
	copy(packet[12:16], packetSourceIp)
	copy(packet[16:20], packetDestinationIp)
	binary.BigEndian.PutUint16(packet[10:12], checksumFinish(checksumAdd(0, packet[0:Ipv4HeaderSizeWithoutExtensions])))
}

// writes an ipv6 header with no extensions.
// `packet` must be sized to the full packet.
func writeIpv6Header(packet []byte, ipProtocol layers.IPProtocol, packetSourceIp net.IP, packetDestinationIp net.IP) {
	// version 6, traffic class and flow label zero
	packet[0] = 0x60
	packet[1] = 0
	packet[2] = 0
	packet[3] = 0
	binary.BigEndian.PutUint16(packet[4:6], uint16(len(packet)-Ipv6HeaderSize))
	packet[6] = byte(ipProtocol)
	// hop limit
	packet[7] = 64
	copy(packet[8:24], packetSourceIp)
	copy(packet[24:40], packetDestinationIp)
}

type Udp4Buffer struct {
	UdpBuffer[BufferId4]
}

func NewUdp4Buffer(ctx context.Context, receiveCallback ReceivePacketFunction,
	udpBufferSettings *UdpBufferSettings) *Udp4Buffer {
	return &Udp4Buffer{
		UdpBuffer: *newUdpBuffer[BufferId4](ctx, receiveCallback, udpBufferSettings),
	}
}

func (self *Udp4Buffer) send(source TransferPath, provideMode protocol.ProvideMode,
	udp *parsedUdp, timeout time.Duration, ipPacket []byte) (bool, error) {
	bufferId := NewBufferId4(
		source,
		udp.sourceIp, int(udp.sourcePort),
		udp.destinationIp, int(udp.destinationPort),
	)

	return self.udpSend(
		bufferId,
		source,
		provideMode,
		4,
		udp,
		timeout,
		ipPacket,
	)
}

type Udp6Buffer struct {
	UdpBuffer[BufferId6]
}

func NewUdp6Buffer(ctx context.Context, receiveCallback ReceivePacketFunction,
	udpBufferSettings *UdpBufferSettings) *Udp6Buffer {
	return &Udp6Buffer{
		UdpBuffer: *newUdpBuffer[BufferId6](ctx, receiveCallback, udpBufferSettings),
	}
}

func (self *Udp6Buffer) send(source TransferPath, provideMode protocol.ProvideMode,
	udp *parsedUdp, timeout time.Duration, ipPacket []byte) (bool, error) {
	bufferId := NewBufferId6(
		source,
		udp.sourceIp, int(udp.sourcePort),
		udp.destinationIp, int(udp.destinationPort),
	)

	return self.udpSend(
		bufferId,
		source,
		provideMode,
		6,
		udp,
		timeout,
		ipPacket,
	)
}

type UdpBuffer[BufferId comparable] struct {
	ctx               context.Context
	log               Logger
	receiveCallback   ReceivePacketFunction
	udpBufferSettings *UdpBufferSettings

	mutex sync.Mutex

	sequences       map[BufferId]*UdpSequence
	sourceSequences map[TransferPath]map[BufferId]*UdpSequence
}

func newUdpBuffer[BufferId comparable](
	ctx context.Context,
	receiveCallback ReceivePacketFunction,
	udpBufferSettings *UdpBufferSettings,
) *UdpBuffer[BufferId] {
	return &UdpBuffer[BufferId]{
		ctx:               ctx,
		log:               loggerOrDefault(udpBufferSettings.Log),
		receiveCallback:   receiveCallback,
		udpBufferSettings: udpBufferSettings,
		sequences:         map[BufferId]*UdpSequence{},
		sourceSequences:   map[TransferPath]map[BufferId]*UdpSequence{},
	}
}

func (self *UdpBuffer[BufferId]) udpSend(
	bufferId BufferId,
	source TransferPath,
	provideMode protocol.ProvideMode,
	ipVersion int,
	udp *parsedUdp,
	timeout time.Duration,
	ipPacket []byte,
) (bool, error) {
	initSequence := func(skip *UdpSequence) *UdpSequence {
		self.mutex.Lock()
		defer self.mutex.Unlock()

		sequence, ok := self.sequences[bufferId]
		if ok {
			if skip == nil || skip != sequence {
				return sequence
			} else {
				sequence.Cancel()
				delete(self.sequences, bufferId)
				sourceSequences := self.sourceSequences[sequence.source]
				delete(sourceSequences, bufferId)
				if 0 == len(sourceSequences) {
					delete(self.sourceSequences, sequence.source)
				}
			}
		}

		if 0 < self.udpBufferSettings.UserLimit {
			// limit the total connections per source to avoid blowing up the ulimit
			if sourceSequences := self.sourceSequences[source]; self.udpBufferSettings.UserLimit < len(sourceSequences) {
				applyLruUserLimit(maps.Values(sourceSequences), self.udpBufferSettings.UserLimit, func(sequence *UdpSequence) bool {
					if self.log.V(1).Enabled() {
						self.log.Infof(
							"[lnr]udp limit source %s->%s\n",
							source,
							net.JoinHostPort(
								sequence.destinationIp.String(),
								strconv.Itoa(int(sequence.destinationPort)),
							),
						)
					}
					return true
				})
			}
		}

		// TODO
		// limit the number of new connections per second per source
		// self.sourceLimiter[source].Limit()

		sourceIpCopy := make(net.IP, len(udp.sourceIp))
		copy(sourceIpCopy, udp.sourceIp)

		destinationIpCopy := make(net.IP, len(udp.destinationIp))
		copy(destinationIpCopy, udp.destinationIp)

		sequence = NewUdpSequence(
			self.ctx,
			self.receiveCallback,
			source,
			provideMode,
			ipVersion,
			sourceIpCopy,
			udp.sourcePort,
			destinationIpCopy,
			udp.destinationPort,
			self.udpBufferSettings,
		)
		self.sequences[bufferId] = sequence
		sourceSequences := self.sourceSequences[source]
		if sourceSequences == nil {
			sourceSequences = map[BufferId]*UdpSequence{}
			self.sourceSequences[source] = sourceSequences
		}
		sourceSequences[bufferId] = sequence
		go HandleError(func() {
			defer func() {
				self.mutex.Lock()
				defer self.mutex.Unlock()
				sequence.Close()
				// clean up
				if sequence == self.sequences[bufferId] {
					delete(self.sequences, bufferId)
					sourceSequences := self.sourceSequences[sequence.source]
					delete(sourceSequences, bufferId)
					if 0 == len(sourceSequences) {
						delete(self.sourceSequences, sequence.source)
					}
				}
			}()
			sequence.Run()
		})
		return sequence
	}

	sendItem := &UdpSendItem{
		provideMode: provideMode,
		udp:         *udp,
		ipPacket:    ipPacket,
	}
	sequence := initSequence(nil)
	if success, err := sequence.send(sendItem, timeout); err == nil {
		return success, nil
	} else {
		// sequence closed
		return initSequence(sequence).send(sendItem, timeout)
	}
}

type UdpSequence struct {
	ctx               context.Context
	cancel            context.CancelFunc
	log               Logger
	receiveCallback   ReceivePacketFunction
	udpBufferSettings *UdpBufferSettings

	sendMutex sync.Mutex
	sendItems chan *UdpSendItem

	idleCondition *IdleCondition

	StreamState
}

func NewUdpSequence(ctx context.Context, receiveCallback ReceivePacketFunction,
	source TransferPath,
	provideMode protocol.ProvideMode,
	ipVersion int,
	sourceIp net.IP, sourcePort layers.UDPPort,
	destinationIp net.IP, destinationPort layers.UDPPort,
	udpBufferSettings *UdpBufferSettings) *UdpSequence {
	cancelCtx, cancel := context.WithCancel(ctx)
	return &UdpSequence{
		ctx:               cancelCtx,
		cancel:            cancel,
		log:               loggerOrDefault(udpBufferSettings.Log),
		receiveCallback:   receiveCallback,
		sendItems:         make(chan *UdpSendItem, udpBufferSettings.SequenceBufferSize),
		udpBufferSettings: udpBufferSettings,
		idleCondition:     NewIdleCondition(),
		StreamState: StreamState{
			source:          source,
			provideMode:     provideMode,
			ipVersion:       ipVersion,
			sourceIp:        sourceIp,
			sourcePort:      sourcePort,
			destinationIp:   destinationIp,
			destinationPort: destinationPort,
			userLimited: userLimited{
				lastActivityTime: time.Now(),
			},
		},
	}
}

func (self *UdpSequence) send(sendItem *UdpSendItem, timeout time.Duration) (bool, error) {
	self.sendMutex.Lock()
	defer self.sendMutex.Unlock()

	select {
	case <-self.ctx.Done():
		return false, errors.New("Done.")
	default:
	}

	if !self.idleCondition.UpdateOpen() {
		return false, nil
	}
	defer self.idleCondition.UpdateClose()

	select {
	case <-self.ctx.Done():
		return false, errors.New("Done.")
	default:
	}

	// fast path without arming a timer
	select {
	case self.sendItems <- sendItem:
		return true, nil
	default:
	}

	if timeout < 0 {
		select {
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.sendItems <- sendItem:
			return true, nil
		}
	} else if timeout == 0 {
		select {
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.sendItems <- sendItem:
			return true, nil
		default:
			return false, nil
		}
	} else {
		select {
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.sendItems <- sendItem:
			return true, nil
		case <-time.After(timeout):
			return false, nil
		}
	}
}

func (self *UdpSequence) Run() {
	defer func() {
		self.cancel()

		func() {
			self.sendMutex.Lock()
			defer self.sendMutex.Unlock()
			close(self.sendItems)
		}()

		// drain the channel
		func() {
			for {
				select {
				case sendItem, ok := <-self.sendItems:
					if !ok {
						return
					}
					MessagePoolReturn(sendItem.ipPacket)
				default:
					return
				}
			}
		}()
	}()

	receive := func(packet []byte) {
		self.receiveCallback(self.source, self.provideMode, self.IpPath(), packet)
		MessagePoolReturn(packet)
	}

	self.log.V(2).Infof("[init]udp connect\n")
	socket, err := self.udpBufferSettings.DialContext(
		self.ctx,
		"udp",
		self.IpPath().DestinationHostPort(),
	)
	if err != nil {
		if self.log.V(1).Enabled() {
			self.log.Infof("[init]udp connect error = %s\n", err)
		}
		return
	}
	defer socket.Close()
	self.UpdateLastActivityTime()
	self.log.V(2).Infof("[init]connect success\n")

	if udpConn, ok := socket.(*net.UDPConn); ok {
		// size the kernel buffers to the max window.
		// the os may silently cap these at system limits.
		udpConn.SetReadBuffer(int(self.udpBufferSettings.MaxWindowSize))
		udpConn.SetWriteBuffer(int(self.udpBufferSettings.MaxWindowSize))
	}
	// f, _ := udpConn.File()
	// fd := SocketHandle(f.Fd())
	// syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_MTU, self.udpBufferSettings.Mtu)

	// pipelines

	type writePayload struct {
		sendIter uint64
		payload  []byte
		ipPacket []byte
	}

	writePayloads := make(chan writePayload, self.udpBufferSettings.SequenceBufferSize)
	go HandleError(func() {
		// best effort return of queued payloads after cancel
		defer func() {
			for {
				select {
				case writePayload, ok := <-writePayloads:
					if !ok {
						return
					}
					MessagePoolReturn(writePayload.ipPacket)
				default:
					return
				}
			}
		}()
		defer self.cancel()

		batch := make([]writePayload, 0, self.udpBufferSettings.WriteBatchSize)

		for {
			batch = batch[:0]
			closed := false
			select {
			case <-self.ctx.Done():
				return
			case writePayload, ok := <-writePayloads:
				if !ok {
					return
				}
				batch = append(batch, writePayload)
			}
			// opportunistically batch queued payloads under a single write deadline
		drain:
			for len(batch) < self.udpBufferSettings.WriteBatchSize {
				select {
				case writePayload, ok := <-writePayloads:
					if !ok {
						closed = true
						break drain
					}
					batch = append(batch, writePayload)
				default:
					break drain
				}
			}

			writeEndTime := time.Now().Add(self.udpBufferSettings.WriteTimeout)
			socket.SetWriteDeadline(writeEndTime)
			var writeErr error
			for _, writePayload := range batch {
				if writeErr == nil {
					// each payload is one datagram. datagrams cannot be coalesced.
					n, err := socket.Write(writePayload.payload)
					if err == nil {
						if self.log.V(2).Enabled() {
							self.log.Infof("[f%d]udp forward %d\n", writePayload.sendIter, n)
						}
					} else {
						if self.log.V(1).Enabled() {
							self.log.Infof("[f%d]udp forward %d error = %s", writePayload.sendIter, n, err)
						}
					}
					if 0 < n {
						self.UpdateLastActivityTime()
					}
					writeErr = err
				}
				MessagePoolReturn(writePayload.ipPacket)
			}
			if writeErr != nil {
				// timeout or socket error
				return
			}
			if closed {
				return
			}
		}
	}, self.cancel)

	readPackets := make(chan []byte, self.udpBufferSettings.SequenceBufferSize)
	go HandleError(func() {
		defer self.cancel()

		defer func() {
			// drain to the close so that ordered data and any final
			// fin/rst reach the source on teardown. the socket read side
			// always closes `readPackets` on exit, which is unblocked by
			// the deferred socket close in `Run`
			for packet := range readPackets {
				receive(packet)
			}
		}()

	read:
		for {
			select {
			case <-self.ctx.Done():
				return
			case packet, ok := <-readPackets:
				if !ok {
					return
				}
				receive(packet)
				// opportunistically drain queued packets to reduce wakeups
				for {
					select {
					case packet, ok := <-readPackets:
						if !ok {
							return
						}
						receive(packet)
					default:
						continue read
					}
				}
			}
		}
	}, self.cancel)

	go HandleError(func() {
		// close without cancel so that the receive pipeline drains all
		// queued packets before the sequence cancels.
		// the receive pipeline cancels after the drain.
		defer close(readPackets)

		buffer := make([]byte, self.udpBufferSettings.ReadBufferByteCount)

		for forwardIter := uint64(0); ; forwardIter += 1 {
			select {
			case <-self.ctx.Done():
				return
			default:
			}

			readTimeout := time.Now().Add(self.udpBufferSettings.ReadTimeout)
			socket.SetReadDeadline(readTimeout)
			n, err := socket.Read(buffer)

			if err != nil {
				if self.log.V(1).Enabled() {
					self.log.Infof("[f%d]udp receive err = %s\n", forwardIter, err)
				}
			}

			if 0 < n {
				self.UpdateLastActivityTime()

				packets, packetsErr := self.DataPackets(buffer, n, self.udpBufferSettings.Mtu)
				if packetsErr != nil {
					self.log.Infof("[f%d]udp receive packets error = %s\n", forwardIter, packetsErr)
					return
				}
				if 1 < len(packets) {
					if self.log.V(2).Enabled() {
						self.log.Infof("[f%d]udp receive segemented packets = %d\n", forwardIter, len(packets))
					}
				}
				for _, packet := range packets {
					if self.log.V(1).Enabled() {
						self.log.Infof("[f%d]udp receive %d\n", forwardIter, len(packet))
					}
					select {
					case <-self.ctx.Done():
						MessagePoolReturn(packet)
					case readPackets <- packet:
					}
				}
			}

			if err != nil {
				if err == io.EOF {
					return
				} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					if self.log.V(1).Enabled() {
						self.log.Infof("[f%d]timeout\n", forwardIter)
					}
					return
				} else {
					// some other error
					return
				}
			}
		}
	}, self.cancel)

	sendIter := uint64(0)
	// returns false when the sequence must stop
	handleSendItem := func(sendItem *UdpSendItem) bool {
		payload := sendItem.udp.payload

		if 0 < len(payload) {
			writePayload := writePayload{
				payload:  payload,
				sendIter: sendIter,
				ipPacket: sendItem.ipPacket,
			}
			select {
			case writePayloads <- writePayload:
			case <-self.ctx.Done():
				MessagePoolReturn(sendItem.ipPacket)
				return false
			}
		} else {
			MessagePoolReturn(sendItem.ipPacket)
		}
		return true
	}

	// reusable idle timer: this send loop wakes per datagram, so a per-iteration
	// time.After allocated a timer per packet (the dominant alloc in the udp
	// egress profile). hot-path timer reuse per CODESTYLE.
	idleTimer := time.NewTimer(0)
	defer idleTimer.Stop()

send:
	for {
		checkpointId := self.idleCondition.Checkpoint()
		idleTimer.Reset(self.udpBufferSettings.IdleTimeout)
		select {
		case <-self.ctx.Done():
			return
		case sendItem, ok := <-self.sendItems:
			if !ok {
				return
			}
			if !handleSendItem(sendItem) {
				return
			}
			sendIter += 1
			// opportunistically drain queued send items to reduce wakeups
			for {
				select {
				case sendItem, ok := <-self.sendItems:
					if !ok {
						return
					}
					if !handleSendItem(sendItem) {
						return
					}
					sendIter += 1
				default:
					continue send
				}
			}
		case <-idleTimer.C:
			done := false
			func() {
				self.sendMutex.Lock()
				defer self.sendMutex.Unlock()
				if self.idleCondition.Close(checkpointId) {
					// close the sequence
					done = true
				}
			}()
			if done {
				// close the sequence
				return
			}
			// else there pending updates
		}
	}
}

func (self *UdpSequence) Cancel() {
	self.cancel()
}

func (self *UdpSequence) Close() {
	self.cancel()
}

type UdpSendItem struct {
	source      TransferPath
	provideMode protocol.ProvideMode
	udp         parsedUdp
	ipPacket    []byte
}

type StreamState struct {
	source          TransferPath
	provideMode     protocol.ProvideMode
	ipVersion       int
	sourceIp        net.IP
	sourcePort      layers.UDPPort
	destinationIp   net.IP
	destinationPort layers.UDPPort
	userLimited

	// cached immutable ip path for this stream (see IpPath). primed by the
	// first call, which happens at sequence setup (DialContext) before the
	// per-packet goroutines start, so it is written once and then read-only.
	ipPath *IpPath

	// reusable backing for the common single-datagram DataPackets result.
	// DataPackets is called from one goroutine and its result is consumed
	// before the next call, so the backing can be reused; fragmented payloads
	// allocate a fresh slice.
	singleDataPacket [1][]byte
}

// IpPath returns the immutable ip path for this stream. The path is built once
// and cached; the stream identity (version, ips, ports) never changes.
func (self *StreamState) IpPath() *IpPath {
	if self.ipPath == nil {
		self.ipPath = &IpPath{
			Version:         self.ipVersion,
			Protocol:        IpProtocolUdp,
			SourceIp:        self.sourceIp,
			SourcePort:      int(self.sourcePort),
			DestinationIp:   self.destinationIp,
			DestinationPort: int(self.destinationPort),
		}
	}
	return self.ipPath
}

// this must only be called from one goroutine
// this is called from the writer only and does not need to syncrhronize with the reader state
func (self *StreamState) DataPackets(payload []byte, n int, mtu int) ([][]byte, error) {
	var headerByteCount int
	switch self.ipVersion {
	case 4:
		headerByteCount = Ipv4HeaderSizeWithoutExtensions + UdpHeaderSize
	case 6:
		headerByteCount = Ipv6HeaderSize + UdpHeaderSize
	}

	packetByteCount := mtu - headerByteCount
	if n <= packetByteCount {
		// reuse the single-packet backing for the common unfragmented case
		// (see singleDataPacket); the result is consumed before the next call.
		self.singleDataPacket[0] = self.udpPacket(payload[0:n])
		return self.singleDataPacket[:], nil
	}
	// fragment into separate datagrams
	packets := make([][]byte, 0, (n+packetByteCount-1)/packetByteCount)
	for i := 0; i < n; {
		j := min(i+packetByteCount, n)
		packets = append(packets, self.udpPacket(payload[i:j]))
		i = j
	}
	return packets, nil
}

// builds a udp packet from the stream destination to the stream source
// into a single pool buffer
func (self *StreamState) udpPacket(payload []byte) []byte {
	var ipHeaderByteCount int
	switch self.ipVersion {
	case 4:
		ipHeaderByteCount = Ipv4HeaderSizeWithoutExtensions
	case 6:
		ipHeaderByteCount = Ipv6HeaderSize
	}

	packet := MessagePoolGet(ipHeaderByteCount + UdpHeaderSize + len(payload))
	switch self.ipVersion {
	case 4:
		writeIpv4Header(packet, layers.IPProtocolUDP, self.destinationIp, self.sourceIp)
	case 6:
		writeIpv6Header(packet, layers.IPProtocolUDP, self.destinationIp, self.sourceIp)
	}

	udp := packet[ipHeaderByteCount:]
	binary.BigEndian.PutUint16(udp[0:2], uint16(self.destinationPort))
	binary.BigEndian.PutUint16(udp[2:4], uint16(self.sourcePort))
	binary.BigEndian.PutUint16(udp[4:6], uint16(UdpHeaderSize+len(payload)))
	// checksum, set below
	udp[6] = 0
	udp[7] = 0
	copy(udp[UdpHeaderSize:], payload)
	checksum := transportChecksum(layers.IPProtocolUDP, self.destinationIp, self.sourceIp, udp)
	if checksum == 0 {
		// zero means no checksum
		checksum = 0xffff
	}
	binary.BigEndian.PutUint16(udp[6:8], checksum)
	return packet
}

type TcpBufferSettings struct {
	// nil resolves to the local user nat `Log`
	Log Logger
	// ConnectTimeout     time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	// coalesce pure acks for up to this duration.
	// an ack is sent sooner when the unacked byte count reaches half the window.
	// zero sends a pure ack on every send seq advance.
	AckCompressTimeout time.Duration
	// ReadPollTimeout time.Duration
	// WritePollTimeout time.Duration
	IdleTimeout         time.Duration
	ReadBufferByteCount int
	// the maximum number of payloads to coalesce into a single socket write
	WriteBatchSize     int
	SequenceBufferSize int
	Mtu                int
	// the window size is the max amount of packet data in memory for each sequence
	// `WindowSize / 2^WindowScale` must fit in uint16
	// see https://datatracker.ietf.org/doc/html/rfc1323#page-8
	WindowScale uint32
	// the initial window size
	MinWindowSize uint32
	// `MaxWindowSize` should be a power of 2 multiple of `MinWindowSize`
	MaxWindowSize uint32
	// the number of open sockets per user
	// uses an lru cleanup where new sockets over the limit close old sockets
	UserLimit int

	ConnectSettings
}

type Tcp4Buffer struct {
	TcpBuffer[BufferId4]
}

func NewTcp4Buffer(ctx context.Context, receiveCallback ReceivePacketFunction,
	tcpBufferSettings *TcpBufferSettings) *Tcp4Buffer {
	return &Tcp4Buffer{
		TcpBuffer: *newTcpBuffer[BufferId4](ctx, receiveCallback, tcpBufferSettings),
	}
}

func (self *Tcp4Buffer) send(source TransferPath, provideMode protocol.ProvideMode,
	tcp *parsedTcp, timeout time.Duration, ipPacket []byte) (bool, error) {
	bufferId := NewBufferId4(
		source,
		tcp.sourceIp, int(tcp.sourcePort),
		tcp.destinationIp, int(tcp.destinationPort),
	)

	return self.tcpSend(
		bufferId,
		source,
		provideMode,
		4,
		tcp,
		timeout,
		ipPacket,
	)
}

type Tcp6Buffer struct {
	TcpBuffer[BufferId6]
}

func NewTcp6Buffer(ctx context.Context, receiveCallback ReceivePacketFunction,
	tcpBufferSettings *TcpBufferSettings) *Tcp6Buffer {
	return &Tcp6Buffer{
		TcpBuffer: *newTcpBuffer[BufferId6](ctx, receiveCallback, tcpBufferSettings),
	}
}

func (self *Tcp6Buffer) send(source TransferPath, provideMode protocol.ProvideMode,
	tcp *parsedTcp, timeout time.Duration, ipPacket []byte) (bool, error) {
	bufferId := NewBufferId6(
		source,
		tcp.sourceIp, int(tcp.sourcePort),
		tcp.destinationIp, int(tcp.destinationPort),
	)

	return self.tcpSend(
		bufferId,
		source,
		provideMode,
		6,
		tcp,
		timeout,
		ipPacket,
	)
}

type TcpBuffer[BufferId comparable] struct {
	log               Logger
	ctx               context.Context
	receiveCallback   ReceivePacketFunction
	tcpBufferSettings *TcpBufferSettings

	mutex sync.Mutex

	sequences       map[BufferId]*TcpSequence
	sourceSequences map[TransferPath]map[BufferId]*TcpSequence
}

func newTcpBuffer[BufferId comparable](
	ctx context.Context,
	receiveCallback ReceivePacketFunction,
	tcpBufferSettings *TcpBufferSettings,
) *TcpBuffer[BufferId] {
	return &TcpBuffer[BufferId]{
		log:               loggerOrDefault(tcpBufferSettings.Log),
		ctx:               ctx,
		receiveCallback:   receiveCallback,
		tcpBufferSettings: tcpBufferSettings,
		sequences:         map[BufferId]*TcpSequence{},
		sourceSequences:   map[TransferPath]map[BufferId]*TcpSequence{},
	}
}

func (self *TcpBuffer[BufferId]) tcpSend(
	bufferId BufferId,
	source TransferPath,
	provideMode protocol.ProvideMode,
	ipVersion int,
	tcp *parsedTcp,
	timeout time.Duration,
	ipPacket []byte,
) (bool, error) {
	initSequence := func() *TcpSequence {
		self.mutex.Lock()
		defer self.mutex.Unlock()

		if sequence, ok := self.sequences[bufferId]; ok {
			if tcp.rst {
				// drop the packet
				sequence.Cancel()
				delete(self.sequences, bufferId)
				sourceSequences := self.sourceSequences[sequence.source]
				delete(sourceSequences, bufferId)
				if 0 == len(sourceSequences) {
					delete(self.sourceSequences, sequence.source)
				}
				MessagePoolReturn(ipPacket)
				return nil
			}
			return sequence
		}

		if !tcp.syn {
			// drop the packet; only create a new sequence on SYN
			MessagePoolReturn(ipPacket)
			if self.log.V(2).Enabled() {
				self.log.Infof("[lnr]tcp drop no syn (%s)\n", tcp.flagsString())
			}
			return nil
		}

		// else new sequence
		// if sequence, ok := self.sequences[bufferId]; ok {
		// 	sequence.Cancel()
		// 	delete(self.sequences, bufferId)
		// 	sourceSequences := self.sourceSequences[sequence.source]
		// 	delete(sourceSequences, bufferId)
		// 	if 0 == len(sourceSequences) {
		// 		delete(self.sourceSequences, sequence.source)
		// 	}
		// }
		if 0 < self.tcpBufferSettings.UserLimit {
			// limit the total connections per source to avoid blowing up the ulimit
			if sourceSequences := self.sourceSequences[source]; self.tcpBufferSettings.UserLimit < len(sourceSequences) {
				applyLruUserLimit(maps.Values(sourceSequences), self.tcpBufferSettings.UserLimit, func(sequence *TcpSequence) bool {
					if self.log.V(1).Enabled() {
						self.log.Infof(
							"[lnr]tcp limit source %s->%s\n",
							source,
							net.JoinHostPort(
								sequence.destinationIp.String(),
								strconv.Itoa(int(sequence.destinationPort)),
							),
						)
					}
					return true
				})
			}
		}

		// TODO
		// limit the number of new connections per second per source
		// self.sourceLimiter[source].Limit()

		sourceIpCopy := make(net.IP, len(tcp.sourceIp))
		copy(sourceIpCopy, tcp.sourceIp)

		destinationIpCopy := make(net.IP, len(tcp.destinationIp))
		copy(destinationIpCopy, tcp.destinationIp)

		sequence := NewTcpSequence(
			self.ctx,
			self.receiveCallback,
			source,
			provideMode,
			ipVersion,
			sourceIpCopy,
			tcp.sourcePort,
			destinationIpCopy,
			tcp.destinationPort,
			self.tcpBufferSettings,
		)
		self.sequences[bufferId] = sequence
		sourceSequences := self.sourceSequences[source]
		if sourceSequences == nil {
			sourceSequences = map[BufferId]*TcpSequence{}
			self.sourceSequences[source] = sourceSequences
		}
		sourceSequences[bufferId] = sequence
		go HandleError(func() {
			defer func() {
				self.mutex.Lock()
				defer self.mutex.Unlock()
				sequence.Close()
				// clean up
				if sequence == self.sequences[bufferId] {
					delete(self.sequences, bufferId)
					sourceSequences := self.sourceSequences[sequence.source]
					delete(sourceSequences, bufferId)
					if 0 == len(sourceSequences) {
						delete(self.sourceSequences, sequence.source)
					}
				}
			}()
			sequence.Run()
		})
		return sequence
	}
	sendItem := &TcpSendItem{
		provideMode: provideMode,
		tcp:         *tcp,
		ipPacket:    ipPacket,
	}
	if sequence := initSequence(); sequence == nil {
		// sequence does not exist and not a syn packet, drop
		return false, nil
	} else {
		return sequence.send(sendItem, timeout)
	}
}

/*
** Important implementation note **
In this implementation, packet flow from the UNAT to the source
is assumed to never require retransmits. The retrasmit logic
is not implemented.
This is a safe assumption when moving packets from local raw socket
to the UNAT via `transfer`, which is lossless and in-order.
*/
type TcpSequence struct {
	ctx    context.Context
	cancel context.CancelFunc
	log    Logger

	receiveCallback ReceivePacketFunction

	tcpBufferSettings *TcpBufferSettings

	sendMutex sync.Mutex
	sendItems chan *TcpSendItem

	idleCondition *IdleCondition

	ConnectionState
}

func NewTcpSequence(ctx context.Context, receiveCallback ReceivePacketFunction,
	source TransferPath,
	provideMode protocol.ProvideMode,
	ipVersion int,
	sourceIp net.IP, sourcePort layers.TCPPort,
	destinationIp net.IP, destinationPort layers.TCPPort,
	tcpBufferSettings *TcpBufferSettings) *TcpSequence {
	cancelCtx, cancel := context.WithCancel(ctx)

	return &TcpSequence{
		ctx:               cancelCtx,
		cancel:            cancel,
		log:               loggerOrDefault(tcpBufferSettings.Log),
		receiveCallback:   receiveCallback,
		tcpBufferSettings: tcpBufferSettings,
		sendItems:         make(chan *TcpSendItem, tcpBufferSettings.SequenceBufferSize),
		idleCondition:     NewIdleCondition(),
		ConnectionState: ConnectionState{
			source:          source,
			provideMode:     provideMode,
			ipVersion:       ipVersion,
			sourceIp:        sourceIp,
			sourcePort:      sourcePort,
			destinationIp:   destinationIp,
			destinationPort: destinationPort,
			// the window size starts at the fixed value
			enableWindowScale: false,
			// FIXME start this at initial window size, and it grows up to max window size
			// FIXME initial window size should be ~4k, set max window size as a 2^amount multiplier of initial size
			windowSize:  tcpBufferSettings.MinWindowSize,
			windowScale: 0,
			buffer:      gopacket.NewSerializeBufferExpectedSize(128, 2048),
			userLimited: userLimited{
				lastActivityTime: time.Now(),
			},
		},
	}
}

func (self *TcpSequence) send(sendItem *TcpSendItem, timeout time.Duration) (bool, error) {
	self.sendMutex.Lock()
	defer self.sendMutex.Unlock()

	select {
	case <-self.ctx.Done():
		return false, errors.New("Done.")
	default:
	}

	if !self.idleCondition.UpdateOpen() {
		return false, nil
	}
	defer self.idleCondition.UpdateClose()

	select {
	case <-self.ctx.Done():
		return false, errors.New("Done.")
	default:
	}

	// fast path without arming a timer
	select {
	case self.sendItems <- sendItem:
		return true, nil
	default:
	}

	if timeout < 0 {
		select {
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.sendItems <- sendItem:
			return true, nil
		}
	} else if timeout == 0 {
		select {
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.sendItems <- sendItem:
			return true, nil
		default:
			return false, nil
		}
	} else {
		select {
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.sendItems <- sendItem:
			return true, nil
		case <-time.After(timeout):
			return false, nil
		}
	}
}

func (self *TcpSequence) Run() {
	defer func() {
		self.cancel()

		func() {
			self.sendMutex.Lock()
			defer self.sendMutex.Unlock()
			close(self.sendItems)
		}()

		// drain the channel
		func() {
			for {
				select {
				case sendItem, ok := <-self.sendItems:
					if !ok {
						return
					}
					MessagePoolReturn(sendItem.ipPacket)
				default:
					return
				}
			}
		}()
	}()

	// note receive is called from multiple goroutines
	// tcp packets with ack may be reordered due to being written in parallel
	receive := func(packet []byte) {
		self.receiveCallback(self.source, self.provideMode, self.IpPath(), packet)
		MessagePoolReturn(packet)
	}

	// f, _ := tcpConn.File()
	// fd := SocketHandle(f.Fd())
	// syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_MTU, self.tcpBufferSettings.Mtu)

	var packet []byte
	var packetErr error
	for syn := false; !syn; {
		checkpointId := self.idleCondition.Checkpoint()
		select {
		case <-self.ctx.Done():
			return
		case sendItem := <-self.sendItems:
			if self.log.V(2).Enabled() {
				self.log.Infof("[init]send(%d)\n", len(sendItem.tcp.payload))
			}
			// the first packet must be a syn
			if sendItem.tcp.syn {
				self.log.V(2).Infof("[init]SYN\n")

				func() {
					self.mutex.Lock()
					defer self.mutex.Unlock()

					// sendSeq is the next expected sequence number
					// SYN and FIN consume one
					self.sendSeq = sendItem.tcp.seq + 1
					// start the send seq at send seq
					// this is arbitrary, and since there is no transport security risk back to sender is fine
					self.receiveSeq = sendItem.tcp.seq
					self.receiveSeqAck = sendItem.tcp.seq

					parseWindowScaleOpts := func() (bool, uint32) {
						options := sendItem.tcp.options
						for i := 0; i < len(options); {
							switch options[i] {
							case 0:
								// end of options
								return false, 0
							case 1:
								// nop
								i += 1
							default:
								if len(options) < i+2 {
									return false, 0
								}
								optionByteCount := int(options[i+1])
								if optionByteCount < 2 || len(options) < i+optionByteCount {
									return false, 0
								}
								if options[i] == 3 && optionByteCount == 3 {
									// window scale
									// see 2.3  Using the Window Scale Option
									return true, min(uint32(options[i+2]), 14)
								}
								i += optionByteCount
							}
						}
						return false, 0
					}

					self.enableWindowScale, self.receiveWindowScale = parseWindowScaleOpts()
					self.receiveWindowSize = uint32(sendItem.tcp.windowSize) << self.receiveWindowScale
					if self.enableWindowScale {
						// compute the window scale to fit the window size in uint16
						bits := math.Log2(float64(self.tcpBufferSettings.MaxWindowSize) / float64(math.MaxUint16))
						if 0 <= bits {
							self.windowScale = uint32(math.Ceil(bits))
						} else {
							self.windowScale = 0
						}
					} else {
						// turn off window scale for send
						self.windowScale = 0
					}
					if self.log.V(2).Enabled() {
						self.log.Infof("[init]window=%d/%d, receive=%d/%d\n", self.windowSize, self.windowScale, self.receiveWindowSize, self.receiveWindowScale)
					}

					packet, packetErr = self.SynAck(self.tcpBufferSettings.Mtu)
					self.receiveSeq += 1
				}()

				syn = true
			} else {
				// an ACK here could be for a previous FIN
				if self.log.V(2).Enabled() {
					self.log.Infof("[init]waiting for SYN (%s)\n", sendItem.tcp.flagsString())
				}
			}
			MessagePoolReturn(sendItem.ipPacket)
		case <-time.After(self.tcpBufferSettings.ConnectTimeout):
			if self.idleCondition.Close(checkpointId) {
				// close the sequence
				self.log.V(2).Infof("[init]connect timeout\n")
				return
			}
			// else there pending updates
		}
	}

	if packetErr != nil {
		return
	}

	// connect to upstream before sending the syn+ack
	self.log.V(2).Infof("[init]tcp connect\n")
	socket, err := self.tcpBufferSettings.DialContext(
		self.ctx,
		"tcp",
		self.IpPath().DestinationHostPort(),
	)
	if err != nil {
		if self.log.V(1).Enabled() {
			self.log.Infof("[init]tcp connect error = %s\n", err)
		}
		return
	}
	self.UpdateLastActivityTime()
	self.log.V(2).Infof("[init]connect success\n")

	defer socket.Close()
	if tcpConn, ok := socket.(*net.TCPConn); ok {
		tcpConn.SetKeepAlive(true)
		tcpConn.SetNoDelay(true)
		// size the kernel buffers to the max window.
		// the os may silently cap these at system limits.
		tcpConn.SetReadBuffer(int(self.tcpBufferSettings.MaxWindowSize))
		tcpConn.SetWriteBuffer(int(self.tcpBufferSettings.MaxWindowSize))
	}

	self.log.V(2).Infof("[init]receive SYN+ACK\n")
	receive(packet)

	/*
		if v, ok := socket.(*net.TCPConn); ok {
			if err := v.SetWriteBuffer(int(self.windowSize)); err != nil {
				self.log.Infof("[init]could not set write buffer = %d\n", self.windowSize)
			}
			// if err := v.SetReadBuffer(int(self.receiveWindowSize)); err != nil {
			// 	self.log.Infof("[init]could not set read buffer = %d\n", self.receiveWindowSize)
			// }
		}
	*/

	receiveAckCond := sync.NewCond(&self.mutex)
	ackCond := sync.NewCond(&self.mutex)
	defer func() {
		self.mutex.Lock()
		defer self.mutex.Unlock()

		receiveAckCond.Broadcast()
		ackCond.Broadcast()
	}()

	// signals the ack pipeline to send a coalesced ack now
	ackSignal := make(chan struct{}, 1)

	var ackedSendSeq uint32
	func() {
		self.mutex.Lock()
		defer self.mutex.Unlock()

		ackedSendSeq = self.sendSeq
	}()

	// pipelines

	type writePayload struct {
		sendIter uint64
		payload  []byte
		ipPacket []byte
	}

	writePayloads := make(chan writePayload, self.tcpBufferSettings.SequenceBufferSize)
	go HandleError(func() {
		// best effort return of queued payloads after cancel
		defer func() {
			for {
				select {
				case writePayload, ok := <-writePayloads:
					if !ok {
						return
					}
					MessagePoolReturn(writePayload.ipPacket)
				default:
					return
				}
			}
		}()
		defer self.cancel()

		batch := make([]writePayload, 0, self.tcpBufferSettings.WriteBatchSize)
		bufferStorage := make([][]byte, 0, self.tcpBufferSettings.WriteBatchSize)

		for {
			batch = batch[:0]
			closed := false
			select {
			case <-self.ctx.Done():
				return
			case writePayload, ok := <-writePayloads:
				if !ok {
					return
				}
				batch = append(batch, writePayload)
			}
			// opportunistically coalesce queued payloads into a single socket write
		drain:
			for len(batch) < self.tcpBufferSettings.WriteBatchSize {
				select {
				case writePayload, ok := <-writePayloads:
					if !ok {
						closed = true
						break drain
					}
					batch = append(batch, writePayload)
				default:
					break drain
				}
			}

			bufferStorage = bufferStorage[:0]
			byteCount := 0
			for _, writePayload := range batch {
				bufferStorage = append(bufferStorage, writePayload.payload)
				byteCount += len(writePayload.payload)
			}
			// `net.Buffers` uses a single vectored write when the socket supports it.
			// `WriteTo` retries partial writes until fully written, a timeout, or an error.
			buffers := net.Buffers(bufferStorage)

			writeEndTime := time.Now().Add(self.tcpBufferSettings.WriteTimeout)
			socket.SetWriteDeadline(writeEndTime)
			n, err := buffers.WriteTo(socket)

			if err == nil {
				if self.log.V(2).Enabled() {
					self.log.Infof("[f%d]tcp forward %d/%d\n", batch[0].sendIter, n, byteCount)
				}
			} else {
				if self.log.V(1).Enabled() {
					self.log.Infof("[f%d]tcp forward %d/%d error = %s\n", batch[0].sendIter, n, byteCount, err)
				}
			}
			if 0 < n {
				self.UpdateLastActivityTime()
			}
			for _, writePayload := range batch {
				MessagePoolReturn(writePayload.ipPacket)
			}
			if err != nil {
				// timeout or socket error
				return
			}
			if closed {
				return
			}
		}
	}, self.cancel)

	readPackets := make(chan []byte, self.tcpBufferSettings.SequenceBufferSize)
	go HandleError(func() {
		defer self.cancel()

		defer func() {
			// drain to the close so that ordered data and any final
			// fin/rst reach the source on teardown. the socket read side
			// always closes `readPackets` on exit, which is unblocked by
			// the deferred socket close in `Run`
			for packet := range readPackets {
				receive(packet)
			}
		}()

	read:
		for {
			select {
			case <-self.ctx.Done():
				return
			case packet, ok := <-readPackets:
				if !ok {
					return
				}
				receive(packet)
				// opportunistically drain queued packets to reduce wakeups
				for {
					select {
					case packet, ok := <-readPackets:
						if !ok {
							return
						}
						receive(packet)
					default:
						continue read
					}
				}
			}
		}
	}, self.cancel)

	go HandleError(func() {
		fin := false
		defer func() {
			// close without cancel so that the receive pipeline drains all
			// queued packets before the sequence cancels.
			// the receive pipeline cancels after the drain.
			if !fin {
				var packet []byte
				var err error
				func() {
					self.mutex.Lock()
					defer self.mutex.Unlock()

					packet, err = self.RstAck()
				}()
				if err == nil {
					select {
					case readPackets <- packet:
						fin = true
					}
				}
			}

			close(readPackets)
		}()

		buffer := make([]byte, self.tcpBufferSettings.ReadBufferByteCount)

		for forwardIter := uint64(0); ; forwardIter += 1 {
			select {
			case <-self.ctx.Done():
				return
			default:
			}

			readTimeout := time.Now().Add(self.tcpBufferSettings.ReadTimeout)
			socket.SetReadDeadline(readTimeout)

			n, err := socket.Read(buffer)

			if err != nil {
				if self.log.V(1).Enabled() {
					self.log.Infof("[f%d]tcp receive error = %s\n", forwardIter, err)
				}
			}

			if 0 < n {
				self.UpdateLastActivityTime()

				// since the transfer from local to remove is lossless and preserves order,
				// do not worry about retransmits.
				// packetize and emit one window-sized chunk at a time, so that a
				// read larger than the receive window cannot stall. each chunk
				// must be emitted before waiting for window room for the next
				// chunk, since the window only opens as the source acks
				// emitted data.
				stop := false
				packetCount := 0
				for i := 0; i < n && !stop; {
					var chunkPackets [][]byte
					func() {
						self.mutex.Lock()
						defer self.mutex.Unlock()

						for {
							select {
							case <-self.ctx.Done():
								stop = true
								return
							default:
							}

							windowByteCount := int(int64(self.receiveWindowSize) - int64(self.receiveSeq-self.receiveSeqAck))
							if 0 < windowByteCount {
								j := min(i+windowByteCount, n)
								var err error
								chunkPackets, err = self.DataPackets(buffer[i:j], j-i, self.tcpBufferSettings.Mtu)
								if err != nil {
									self.log.Infof("[f%d]tcp receive packets error = %s\n", forwardIter, err)
									stop = true
									return
								}
								self.receiveSeq += uint32(j - i)
								ackedSendSeq = self.sendSeq
								i = j
								return
							}

							if self.log.V(2).Enabled() {
								self.log.Infof("[f%d]tcp receive window wait\n", forwardIter)
							}
							receiveAckCond.Wait()
						}
					}()
					for _, packet := range chunkPackets {
						if stop {
							MessagePoolReturn(packet)
						} else {
							select {
							case <-self.ctx.Done():
								MessagePoolReturn(packet)
								stop = true
							case readPackets <- packet:
								packetCount += 1
							}
						}
					}
				}
				if stop {
					return
				}

				if 1 < packetCount {
					if self.log.V(2).Enabled() {
						self.log.Infof("[f%d]tcp receive segmented packets %d\n", forwardIter, packetCount)
					}
				}
				if self.log.V(2).Enabled() {
					self.log.Infof("[f%d]tcp receive %d %d\n", forwardIter, n, packetCount)
				}
			}

			if err != nil {
				if err == io.EOF {
					// closed (FIN)
					// propagate the FIN and close the sequence
					self.log.V(2).Infof("[final]FIN\n")
					var finPacket []byte
					var finErr error
					func() {
						self.mutex.Lock()
						defer self.mutex.Unlock()

						finPacket, finErr = self.FinAck()
						self.receiveSeq += 1
					}()
					if finErr == nil {
						select {
						case <-self.ctx.Done():
							MessagePoolReturn(finPacket)
						case readPackets <- finPacket:
							fin = true
						}
					}
					return
				} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					if self.log.V(2).Enabled() {
						self.log.Infof("[f%d]timeout\n", forwardIter)
					}
					return
				} else {
					// some other error
					return
				}
			}
		}
	}, self.cancel)

	go HandleError(func() {
		defer self.cancel()

		for {
			select {
			case <-self.ctx.Done():
				return
			default:
			}

			var packet []byte
			func() {
				self.mutex.Lock()
				defer self.mutex.Unlock()

				select {
				case <-self.ctx.Done():
					return
				default:
				}

				for self.sendSeq == ackedSendSeq {
					ackCond.Wait()
					select {
					case <-self.ctx.Done():
						return
					default:
					}
				}

				var err error
				packet, err = self.PureAck()
				if err != nil {
					self.log.Infof("[r]ack err = %s\n", err)
				}
				ackedSendSeq = self.sendSeq
			}()
			if packet == nil {
				return
			}

			select {
			case <-self.ctx.Done():
				return
			default:
			}

			receive(packet)

			if 0 < self.tcpBufferSettings.AckCompressTimeout {
				// coalesce acks up to the timeout.
				// the send loop signals to ack sooner when the unacked byte
				// count reaches half the window, so the source never stalls
				// on a full window waiting for the timeout.
				select {
				case <-time.After(self.tcpBufferSettings.AckCompressTimeout):
				case <-ackSignal:
				case <-self.ctx.Done():
					return
				}
			}
		}
	}, self.cancel)

	// window scaling depends on `nonBlockingByteCount` and `blockingByteCount` per `self.windowSize`
	nonBlockingByteCount := uint32(0)
	blockingByteCount := uint32(0)
	fin := false
	sendIter := uint64(0)
	// returns false when the send loop must stop:
	// rst or cancel with `fin` false, or fin flush with `fin` true
	handleSendItem := func(sendItem *TcpSendItem) bool {
		if self.log.V(2).Enabled() {
			if "ACK" != sendItem.tcp.flagsString() {
				self.log.Infof("[r%d]receive(%d %s)\n", sendIter, len(sendItem.tcp.payload), sendItem.tcp.flagsString())
			}
		}

		if sendItem.tcp.rst {
			// a RST typically appears for a bad TCP segment
			if self.log.V(2).Enabled() {
				self.log.Infof("[r%d]RST\n", sendIter)
			}
			MessagePoolReturn(sendItem.ipPacket)
			// FIXME
			return false
			// continue
		}

		drop := false
		// seq := uint32(0)

		func() {
			self.mutex.Lock()
			defer self.mutex.Unlock()

			// signed-delta comparisons are wraparound-tolerant across the
			// 4 GB uint32 boundary. since the transfer from local to remote
			// is lossless and preserves order, any seq other than the
			// exact expected sendSeq must be a retransmit.
			if int32(sendItem.tcp.seq-self.sendSeq) != 0 || sendItem.tcp.syn {
				// a retransmit; ignore
				drop = true
			} else if sendItem.tcp.ack {
				// acks are reliably delivered (see above).
				// note the window size can be adjusted at any time for the same
				// receive seq number, e.g. ->0 then ->full on receiver full.
				// use signed delta so wrapped ack numbers compare correctly.
				if 0 <= int32(sendItem.tcp.ackNumber-self.receiveSeqAck) {
					self.receiveWindowSize = uint32(sendItem.tcp.windowSize) << self.receiveWindowScale
					self.receiveSeqAck = sendItem.tcp.ackNumber
					receiveAckCond.Broadcast()
				}
			}
		}()

		if drop {
			MessagePoolReturn(sendItem.ipPacket)
			return true
		}

		if sendItem.tcp.fin {
			if self.log.V(2).Enabled() {
				self.log.Infof("[r%d]FIN\n", sendIter)
			}
			func() {
				self.mutex.Lock()
				defer self.mutex.Unlock()

				self.sendSeq += 1
				ackCond.Broadcast()
			}()
		}

		payload := sendItem.tcp.payload
		if 0 < len(payload) {
			// seq += uint32(len(payload))
			writePayload := writePayload{
				payload:  payload,
				sendIter: sendIter,
				ipPacket: sendItem.ipPacket,
			}
			// FIXME count the number of non-blocking versus blocking channel adds
			// FIXME every window size, check the count:
			// FIXME - if 0 blocking, double window size
			// FIXME - if >half blocking, half the window size
			// FIXME else leave the window size unchanged
			select {
			case writePayloads <- writePayload:
				nonBlockingByteCount += uint32(len(payload))
			default:
				select {
				case writePayloads <- writePayload:
					blockingByteCount += uint32(len(payload))
				case <-self.ctx.Done():
					MessagePoolReturn(sendItem.ipPacket)
					return false
				}
			}
			func() {
				self.mutex.Lock()
				defer self.mutex.Unlock()
				// self.log.Infof("[r%d]eval window size (%d, %d, %d)\n", sendIter, self.windowSize, nonBlockingByteCount, blockingByteCount)
				if self.windowSize <= blockingByteCount+nonBlockingByteCount {
					if self.windowSize <= nonBlockingByteCount {
						nextWindowSize := min(self.windowSize*2, self.tcpBufferSettings.MaxWindowSize)
						if self.windowSize != nextWindowSize {
							if self.log.V(1).Enabled() {
								self.log.Infof("[r%d]increase window size %d -> %d\n", sendIter, self.windowSize, nextWindowSize)
							}
							self.windowSize = nextWindowSize
						}
					} else if self.windowSize/2 <= blockingByteCount {
						nextWindowSize := max(self.windowSize/2, self.tcpBufferSettings.MinWindowSize)
						if self.windowSize != nextWindowSize {
							if self.log.V(1).Enabled() {
								self.log.Infof("[r%d]decrease window size %d -> %d\n", sendIter, self.windowSize, nextWindowSize)
							}
							self.windowSize = nextWindowSize
						}
					}
					// else no change to the window
					// reset the stats
					nonBlockingByteCount = uint32(0)
					blockingByteCount = uint32(0)
				}

				self.sendSeq += uint32(len(payload))
				ackCond.Broadcast()
				if self.windowSize/2 <= self.sendSeq-ackedSendSeq {
					select {
					case ackSignal <- struct{}{}:
					default:
					}
				}
			}()
		} else {
			MessagePoolReturn(sendItem.ipPacket)
		}

		// if 0 < seq {
		// 	func() {
		// 		self.mutex.Lock()
		// 		defer self.mutex.Unlock()

		// 		self.sendSeq += seq
		// 		ackCond.Broadcast()
		// 	}()
		// }

		if sendItem.tcp.fin {
			// flush the write channel to propage the FIN and close the sequence
			close(writePayloads)
			fin = true
			return false
		}

		return true
	}

send:
	for {
		checkpointId := self.idleCondition.Checkpoint()
		select {
		case <-self.ctx.Done():
			return
		case sendItem := <-self.sendItems:
			if !handleSendItem(sendItem) {
				if !fin {
					return
				}
				break send
			}
			sendIter += 1
			// opportunistically drain queued send items to reduce wakeups
			for {
				select {
				case sendItem := <-self.sendItems:
					if !handleSendItem(sendItem) {
						if !fin {
							return
						}
						break send
					}
					sendIter += 1
				default:
					continue send
				}
			}
		case <-time.After(self.tcpBufferSettings.IdleTimeout):
			done := false
			func() {
				self.sendMutex.Lock()
				defer self.sendMutex.Unlock()
				if self.idleCondition.Close(checkpointId) {
					// close the sequence
					done = true
				}
			}()
			if done {
				// close the sequence
				if self.log.V(2).Enabled() {
					self.log.Infof("[r%d]timeout\n", sendIter)
				}
				return
			}
			// else there pending updates
		}
	}

	// wait for `writePayloads` to finish
	select {
	case <-self.ctx.Done():
	}
}

func (self *TcpSequence) Cancel() {
	self.cancel()
}

func (self *TcpSequence) Close() {
	self.cancel()
}

type TcpSendItem struct {
	provideMode protocol.ProvideMode
	tcp         parsedTcp
	ipPacket    []byte
}

type ConnectionState struct {
	source          TransferPath
	provideMode     protocol.ProvideMode
	ipVersion       int
	sourceIp        net.IP
	sourcePort      layers.TCPPort
	destinationIp   net.IP
	destinationPort layers.TCPPort

	mutex sync.Mutex

	sendSeq            uint32
	receiveSeq         uint32
	receiveSeqAck      uint32
	receiveWindowSize  uint32
	receiveWindowScale uint32
	enableWindowScale  bool
	windowSize         uint32
	windowScale        uint32
	// encodedWindowSize  uint16

	buffer gopacket.SerializeBuffer

	userLimited
}

func (self *ConnectionState) IpPath() *IpPath {
	return &IpPath{
		Version:         self.ipVersion,
		Protocol:        IpProtocolTcp,
		SourceIp:        self.sourceIp,
		SourcePort:      int(self.sourcePort),
		DestinationIp:   self.destinationIp,
		DestinationPort: int(self.destinationPort),
	}
}

func (self *ConnectionState) encodedWindowSize() uint16 {
	return uint16(min(
		uint32(self.windowSize>>self.windowScale),
		uint32(math.MaxUint16),
	))
}

func (self *ConnectionState) SynAck(mtu int) ([]byte, error) {
	headerSize := 0
	var ip gopacket.NetworkLayer
	switch self.ipVersion {
	case 4:
		ip = &layers.IPv4{
			Version:  4,
			TTL:      64,
			SrcIP:    self.destinationIp,
			DstIP:    self.sourceIp,
			Protocol: layers.IPProtocolTCP,
		}
		headerSize += Ipv4HeaderSizeWithoutExtensions
	case 6:
		ip = &layers.IPv6{
			Version:    6,
			HopLimit:   64,
			SrcIP:      self.destinationIp,
			DstIP:      self.sourceIp,
			NextHeader: layers.IPProtocolTCP,
		}
		headerSize += Ipv6HeaderSize
	}

	opts := []layers.TCPOption{}

	// advertise the mss so the source does not segment to a conservative default
	mssBytes := make([]byte, 2)
	binary.BigEndian.PutUint16(mssBytes, uint16(mtu-headerSize-TcpHeaderSizeWithoutExtensions))
	opts = append(opts, layers.TCPOption{
		OptionType:   layers.TCPOptionKindMSS,
		OptionLength: 4,
		OptionData:   mssBytes,
	})

	if self.enableWindowScale {
		windowScaleBytes := make([]byte, 4)
		binary.BigEndian.PutUint32(windowScaleBytes[0:4], self.windowScale)

		windowScaleOpt := layers.TCPOption{
			OptionType:   layers.TCPOptionKindWindowScale,
			OptionLength: 3,
			// one byte
			OptionData: windowScaleBytes[3:4],
		}
		opts = append(opts, windowScaleOpt)
	}

	tcp := layers.TCP{
		SrcPort: self.destinationPort,
		DstPort: self.sourcePort,
		Seq:     self.receiveSeq,
		Ack:     self.sendSeq,
		ACK:     true,
		SYN:     true,
		Window:  self.encodedWindowSize(),
		Options: opts,
	}
	tcp.SetNetworkLayerForChecksum(ip)
	headerSize += TcpHeaderSizeWithoutExtensions

	options := gopacket.SerializeOptions{
		ComputeChecksums: true,
		FixLengths:       true,
	}

	self.buffer.Clear()
	err := gopacket.SerializeLayers(self.buffer, options,
		ip.(gopacket.SerializableLayer),
		&tcp,
	)

	if err != nil {
		return nil, err
	}
	packet := MessagePoolCopy(self.buffer.Bytes())
	return packet, nil
}

func (self *ConnectionState) PureAck() ([]byte, error) {
	return self.tcpPacket(tcpFlagAck, self.receiveSeq, nil), nil
}

func (self *ConnectionState) FinAck() ([]byte, error) {
	return self.tcpPacket(tcpFlagAck|tcpFlagFin, self.receiveSeq, nil), nil
}

func (self *ConnectionState) RstAck() ([]byte, error) {
	return self.tcpPacket(tcpFlagAck|tcpFlagRst, self.receiveSeq, nil), nil
}

func (self *ConnectionState) DataPackets(payload []byte, n int, mtu int) ([][]byte, error) {
	var headerByteCount int
	switch self.ipVersion {
	case 4:
		headerByteCount = Ipv4HeaderSizeWithoutExtensions + TcpHeaderSizeWithoutExtensions
	case 6:
		headerByteCount = Ipv6HeaderSize + TcpHeaderSizeWithoutExtensions
	}

	packetByteCount := mtu - headerByteCount
	if n <= packetByteCount {
		return [][]byte{self.tcpPacket(tcpFlagAck, self.receiveSeq, payload[0:n])}, nil
	}
	// segment
	packets := make([][]byte, 0, (n+packetByteCount-1)/packetByteCount)
	for i := 0; i < n; {
		j := min(i+packetByteCount, n)
		packets = append(packets, self.tcpPacket(tcpFlagAck, self.receiveSeq+uint32(i), payload[i:j]))
		i = j
	}
	return packets, nil
}

// builds a tcp packet from the stream destination to the stream source
// into a single pool buffer. the ack number is always set.
func (self *ConnectionState) tcpPacket(flags byte, seq uint32, payload []byte) []byte {
	var ipHeaderByteCount int
	switch self.ipVersion {
	case 4:
		ipHeaderByteCount = Ipv4HeaderSizeWithoutExtensions
	case 6:
		ipHeaderByteCount = Ipv6HeaderSize
	}

	packet := MessagePoolGet(ipHeaderByteCount + TcpHeaderSizeWithoutExtensions + len(payload))
	switch self.ipVersion {
	case 4:
		writeIpv4Header(packet, layers.IPProtocolTCP, self.destinationIp, self.sourceIp)
	case 6:
		writeIpv6Header(packet, layers.IPProtocolTCP, self.destinationIp, self.sourceIp)
	}

	tcp := packet[ipHeaderByteCount:]
	binary.BigEndian.PutUint16(tcp[0:2], uint16(self.destinationPort))
	binary.BigEndian.PutUint16(tcp[2:4], uint16(self.sourcePort))
	binary.BigEndian.PutUint32(tcp[4:8], seq)
	binary.BigEndian.PutUint32(tcp[8:12], self.sendSeq)
	// data offset, no options
	tcp[12] = byte(TcpHeaderSizeWithoutExtensions/4) << 4
	tcp[13] = flags
	binary.BigEndian.PutUint16(tcp[14:16], self.encodedWindowSize())
	// checksum, set below
	tcp[16] = 0
	tcp[17] = 0
	// urgent
	tcp[18] = 0
	tcp[19] = 0
	copy(tcp[TcpHeaderSizeWithoutExtensions:], payload)
	binary.BigEndian.PutUint16(tcp[16:18], transportChecksum(layers.IPProtocolTCP, self.destinationIp, self.sourceIp, tcp))
	return packet
}

func DefaultRemoteUserNatProviderSettings() *RemoteUserNatProviderSettings {
	return &RemoteUserNatProviderSettings{
		WriteTimeout:            30 * time.Second,
		ProtocolVersion:         DefaultProtocolVersion,
		SecurityPolicyGenerator: DefaultProviderSecurityPolicyWithStats,
	}
}

type RemoteUserNatProviderSettings struct {
	WriteTimeout time.Duration

	ProtocolVersion int

	SecurityPolicyGenerator func(context.Context, *SecurityPolicyStatsCollector) SecurityPolicy
}

type RemoteUserNatProvider struct {
	client            *Client
	cancel            context.CancelFunc
	localUserNat      *LocalUserNat
	securityPolicy    SecurityPolicy
	settings          *RemoteUserNatProviderSettings
	localUserNatUnsub func()
	clientUnsub       func()

	// the min (most private) provide mode each source has sent under, so the
	// return path can echo it. A source on the same network sends under
	// ProvideMode_Network and its return traffic should also be network mode,
	// which skips the public security rules and forgoes the companion contract.
	stateLock         sync.Mutex
	sourceProvideMode map[Id]protocol.ProvideMode
}

func NewRemoteUserNatProviderWithDefaults(
	client *Client,
	localUserNat *LocalUserNat,
) *RemoteUserNatProvider {
	return NewRemoteUserNatProvider(client, localUserNat, DefaultRemoteUserNatProviderSettings())
}

func NewRemoteUserNatProvider(
	client *Client,
	localUserNat *LocalUserNat,
	settings *RemoteUserNatProviderSettings,
) *RemoteUserNatProvider {
	// the security policy runs a background scan goroutine; scope it to this provider (a child of
	// the client ctx) so Close stops it, rather than leaking it for the life of the client
	cancelCtx, cancel := context.WithCancel(client.Ctx())
	userNatProvider := &RemoteUserNatProvider{
		client:            client,
		cancel:            cancel,
		localUserNat:      localUserNat,
		securityPolicy:    settings.SecurityPolicyGenerator(cancelCtx, DefaultSecurityPolicyStatsCollector()),
		settings:          settings,
		sourceProvideMode: map[Id]protocol.ProvideMode{},
	}

	localUserNatUnsub := localUserNat.AddReceivePacketCallback(userNatProvider.Receive)
	userNatProvider.localUserNatUnsub = localUserNatUnsub
	clientUnsub := client.AddReceiveCallback(userNatProvider.ClientReceive)
	userNatProvider.clientUnsub = clientUnsub

	return userNatProvider
}

func (self *RemoteUserNatProvider) SecurityPolicyStats(reset bool) SecurityPolicyStats {
	return self.securityPolicy.Stats().Stats(reset)
}

// recordSourceProvideMode keeps the min (most private) provide mode a source
// has egressed under, so the return path can echo it
func (self *RemoteUserNatProvider) recordSourceProvideMode(sourceId Id, provideMode protocol.ProvideMode) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if existing, ok := self.sourceProvideMode[sourceId]; !ok || provideMode < existing {
		self.sourceProvideMode[sourceId] = provideMode
	}
}

// minSourceProvideMode returns the recorded min provide mode for a source,
// falling back to the provide mode of the current return packet (carried back
// through the local nat conntrack) if the source is not yet tracked
func (self *RemoteUserNatProvider) minSourceProvideMode(sourceId Id, fallback protocol.ProvideMode) protocol.ProvideMode {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if provideMode, ok := self.sourceProvideMode[sourceId]; ok {
		return provideMode
	}
	return fallback
}

// `ReceivePacketFunction`
func (self *RemoteUserNatProvider) Receive(
	source TransferPath,
	provideMode protocol.ProvideMode,
	ipPath *IpPath,
	packet []byte,
) {
	// self.client.log.Infof("[trace]provider return packet for %s\n", source.SourceId)

	if self.client.ClientId() == source.SourceId {
		// locally generated traffic should use a separate local user nat
		if self.client.log.V(2).Enabled() {
			self.client.log.Infof("drop remote user nat provider s packet ->%s\n", source.SourceId)
		}
		return
	}

	// the provider's egress is the return into the tunnel (destination->client); the reversed
	// provider policy applies the client-ingress source check here, then refreshes the flow so an
	// active download isn't reclaimed while the outbound side is quiet
	r, err := self.securityPolicy.InspectEgress(provideMode, ipPath, nil)
	if err != nil {
		return
	}
	self.securityPolicy.RefreshEgress(ipPath)
	if r != SecurityPolicyResultAllow {
		return
	}

	ipPacketFromProvider := &protocol.IpPacketFromProvider{
		IpPacket: &protocol.IpPacket{
			PacketBytes: MessagePoolShareReadOnly(packet),
		},
	}
	frame, err := ToFrame(ipPacketFromProvider, self.settings.ProtocolVersion)
	if err != nil {
		if self.client.log.V(2).Enabled() {
			self.client.log.Infof("drop remote user nat provider s packet ->%s = %s\n", source.SourceId, err)
		}
		panic(err)
	}
	if !frame.Raw {
		defer MessagePoolReturn(ipPacketFromProvider.IpPacket.PacketBytes)
	}

	// echo the min provide mode the source sent under. A same-network source
	// sends under ProvideMode_Network; its return traffic is also network mode,
	// which uses the network relationship (no companion contract) so the device
	// receives it as network mode and skips the public ingress rules. Other
	// modes ride a companion contract (verified as Stream) as before.
	returnProvideMode := self.minSourceProvideMode(source.SourceId, provideMode)
	opts := []any{}
	if returnProvideMode != protocol.ProvideMode_Network {
		opts = append(opts, CompanionContract())
	}
	// note udp is sent with ack because because otherwise the delivery reliability will mulitply with the egress
	c := func() bool {
		// ack := make(chan error)
		sent := self.client.SendWithTimeout(
			frame,
			source.Reverse(),
			func(err error) {},
			self.settings.WriteTimeout,
			opts...,
		)
		if !sent {
			// the send did not take the frame: free it. For raw frames this undoes
			// the packet share above; for wrapped frames it frees the marshal buffer.
			MessagePoolReturn(frame.MessageBytes)
		}
		// if sent {
		// 	self.client.log.Infof("[trace]provider return packet sent for %s\n", source.SourceId)
		// }
		return sent
	}
	if self.client.log.V(2).Enabled() {
		TraceWithReturn(
			fmt.Sprintf("[unps]%s %s->%s s(%s)", ipPath.Protocol, self.client.ClientTag(), source.SourceId, source.StreamId),
			c,
		)
	} else {
		c()
	}

}

// `connect.ReceiveFunction`
func (self *RemoteUserNatProvider) ClientReceive(source TransferPath, frames []*protocol.Frame, provideMode protocol.ProvideMode) {
	// receive functions should be non-blocking
	// clients should manage their own congestion protocols on top to avoid overflowing the sequence queues

	// collect the allowed packets and queue them into the local user nat as one batch
	var packets [][]byte
	for _, frame := range frames {
		switch frame.MessageType {
		case protocol.MessageType_IpIpPing:
			if self.client.log.V(1).Enabled() {
				self.client.log.Infof("[ip]provider ping <- %s(%d)\n", source, provideMode)
			}
			// echo back over a companion contract, like the provider's other
			// return traffic; the source only provides ProvideMode_Stream, so a
			// forward contract here would be rejected (no permission).
			self.client.SendWithTimeout(
				frame,
				source.Reverse(),
				func(err error) {},
				0,
				CompanionContract(),
			)
		case protocol.MessageType_IpIpPacketToProvider:
			ipPacketToProvider_, err := FromFrame(frame)
			if err != nil {
				panic(err)
			}
			ipPacketToProvider := ipPacketToProvider_.(*protocol.IpPacketToProvider)

			ipPath, payload, err := ParseIpPathWithPayload(ipPacketToProvider.IpPacket.PacketBytes)
			if err == nil {
				// the provider's ingress is the remote client's egress (outbound, received from the
				// tunnel); the reversed provider policy applies the client-egress DPI here
				r, err := self.securityPolicy.InspectIngress(provideMode, ipPath, payload)
				self.securityPolicy.RefreshIngress(ipPath)
				if err == nil {
					switch r {
					case SecurityPolicyResultAllow:
						var packet []byte
						if frame.Raw {
							packet = MessagePoolShareReadOnly(ipPacketToProvider.IpPacket.PacketBytes)
						} else {
							packet = MessagePoolCopy(ipPacketToProvider.IpPacket.PacketBytes)
						}
						packets = append(packets, packet)
						self.recordSourceProvideMode(source.SourceId, provideMode)
					case SecurityPolicyResultIncident:
						self.client.ReportAbuse(source)
					}
				}
			}
		}
	}

	if 0 < len(packets) {
		c := func() bool {
			success := self.localUserNat.SendPacketsWithTimeout(
				source,
				provideMode,
				packets,
				0,
			)
			if !success {
				for _, packet := range packets {
					MessagePoolReturn(packet)
				}
			}
			return success
		}
		if self.client.log.V(2).Enabled() {
			TraceWithReturn(
				fmt.Sprintf("[unpr]%d %s<-%s s(%s)", len(packets), self.client.ClientTag(), source.SourceId, source.StreamId),
				c,
			)
		} else {
			c()
		}
	}
}

func (self *RemoteUserNatProvider) Close() {
	// self.client.RemoveReceiveCallback(self.clientCallbackId)
	// self.localUserNat.RemoveReceivePacketCallback(self.localUserNatCallbackId)
	self.cancel()
	self.clientUnsub()
	self.localUserNatUnsub()
}

// this is a basic implementation. See `RemoteUserNatWindowedClient` for a more robust implementation
type RemoteUserNatClient struct {
	client                *Client
	cancel                context.CancelFunc
	receivePacketCallback ReceivePacketFunction
	securityPolicy        SecurityPolicy
	pathTable             *pathTable
	// the provide mode of the source packets
	// for locally generated packets this is `ProvideMode_Network`
	provideMode       protocol.ProvideMode
	localUserNat      *LocalUserNat
	closeCallback     func()
	clientUnsub       func()
	localUserNatUnsub func()

	stateLock           sync.Mutex
	allowDirect         bool
	localSecurityBypass bool
}

func NewRemoteUserNatClient(
	client *Client,
	receivePacketCallback ReceivePacketFunction,
	destinations []MultiHopId,
	provideMode protocol.ProvideMode,
) *RemoteUserNatClient {
	return NewRemoteUserNatClientWithClose(client, receivePacketCallback, destinations, provideMode, nil)
}

func NewRemoteUserNatClientWithClose(
	client *Client,
	receivePacketCallback ReceivePacketFunction,
	destinations []MultiHopId,
	provideMode protocol.ProvideMode,
	closeCallback func(),
) *RemoteUserNatClient {
	pathTable := newPathTable(destinations)

	localUserNatSettings := DefaultLocalUserNatSettings()
	// no ulimit for local traffic
	localUserNatSettings.UdpBufferSettings.UserLimit = 0
	localUserNatSettings.TcpBufferSettings.UserLimit = 0
	localUserNat := NewLocalUserNat(client.Ctx(), "remote local", localUserNatSettings)

	// the security policy runs a background scan goroutine; scope it to this client (a child of
	// the client ctx) so Close stops it rather than leaking it for the life of the client
	cancelCtx, cancel := context.WithCancel(client.Ctx())
	userNatClient := &RemoteUserNatClient{
		client:                client,
		cancel:                cancel,
		receivePacketCallback: receivePacketCallback,
		securityPolicy:        DefaultSecurityPolicy(cancelCtx),
		pathTable:             pathTable,
		provideMode:           provideMode,
		localUserNat:          localUserNat,
		closeCallback:         closeCallback,
	}

	clientUnsub := client.AddReceiveCallback(userNatClient.ClientReceive)
	userNatClient.clientUnsub = clientUnsub

	userNatClient.localUserNatUnsub = localUserNat.AddReceivePacketCallback(receivePacketCallback)

	return userNatClient
}

func (self *RemoteUserNatClient) Destinations() []MultiHopId {
	return self.pathTable.Destinations()
}

func (self *RemoteUserNatClient) DestinationIds() []Id {
	return self.pathTable.DestinationIds()
}

func (self *RemoteUserNatClient) SecurityPolicyStats(reset bool) SecurityPolicyStats {
	return self.securityPolicy.Stats().Stats(reset)
}

// `SendPacketFunction`
func (self *RemoteUserNatClient) SendPacket(source TransferPath, provideMode protocol.ProvideMode, packet []byte, timeout time.Duration) bool {
	minRelationship := max(provideMode, self.provideMode)

	ipPath, payload, err := ParseIpPathWithPayload(packet)
	if err != nil {
		return false
	}
	r, err := self.securityPolicy.InspectEgress(minRelationship, ipPath, payload)
	if err != nil {
		return false
	}
	self.securityPolicy.RefreshEgress(ipPath)

	switch r {
	case SecurityPolicyResultAllow:
		destination, err := self.pathTable.SelectDestination(packet)
		if err != nil {
			// drop
			return false
		}

		ipPacketToProvider := &protocol.IpPacketToProvider{
			IpPacket: &protocol.IpPacket{
				PacketBytes: MessagePoolShareReadOnly(packet),
			},
		}
		frame, err := ToFrame(ipPacketToProvider, DefaultProtocolVersion)
		if err != nil {
			panic(err)
		}
		if !frame.Raw {
			defer MessagePoolReturn(packet)
		}

		// the sender will control transfer
		opts := []any{}
		// note udp is sent with ack because because otherwise the delivery reliability will mulitply with the egress
		success := self.client.SendMultiHopWithTimeout(frame, destination, func(err error) {}, timeout, opts...)
		if !success {
			// the send did not take the frame: free it. For raw frames this undoes
			// the packet share above; for wrapped frames it frees the marshal buffer.
			MessagePoolReturn(frame.MessageBytes)
		}
		return success
	case SecurityPolicyResultDrop:
		if self.LocalSecurityBypass() {
			return self.localUserNat.SendPacket(source, provideMode, packet, 0)
		} else {
			return false
		}
	default:
		return false
	}
}

// `connect.ReceiveFunction`
func (self *RemoteUserNatClient) ClientReceive(source TransferPath, frames []*protocol.Frame, provideMode protocol.ProvideMode) {
	// only process frames from the destinations
	// if allow := self.sourceFilter[source]; !allow {
	//     return
	// }

	for _, frame := range frames {
		// self.client.log.Infof("[trace]receive frame %s\n", frame.MessageType)
		switch frame.MessageType {
		case protocol.MessageType_IpIpPacketFromProvider:
			ipPacketFromProvider_, err := FromFrame(frame)
			if err != nil {
				panic(err)
			}
			ipPacketFromProvider := ipPacketFromProvider_.(*protocol.IpPacketFromProvider)

			packet := ipPacketFromProvider.IpPacket.PacketBytes

			ipPath, err := ParseIpPath(packet)
			if err == nil {
				self.securityPolicy.RefreshIngress(ipPath)
				HandleError(func() {
					self.receivePacketCallback(
						source,
						provideMode,
						ipPath,
						packet,
					)
				})
			}
			// else not an ip packet, drop
		}
	}
}

func (self *RemoteUserNatClient) Shuffle() {
}

func (self *RemoteUserNatClient) Close() {
	// self.client.RemoveReceiveCallback(self.clientCallbackId)
	self.cancel()
	self.localUserNat.Close()
	self.localUserNatUnsub()
	self.clientUnsub()
	if self.closeCallback != nil {
		self.closeCallback()
	}
}

func (self *RemoteUserNatClient) SetAllowDirect(allowDirect bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.allowDirect = allowDirect
}

func (self *RemoteUserNatClient) AllowDirect() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.allowDirect
}

func (self *RemoteUserNatClient) SetLocalSecurityBypass(localSecurityBypass bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.localSecurityBypass = localSecurityBypass
}

func (self *RemoteUserNatClient) LocalSecurityBypass() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.localSecurityBypass
}

type pathTable struct {
	destinations []MultiHopId

	// TODO clean up entries that haven't been used in some time
	paths4 map[Ip4Path]MultiHopId
	paths6 map[Ip6Path]MultiHopId
}

func newPathTable(destinations []MultiHopId) *pathTable {
	return &pathTable{
		destinations: destinations,
		paths4:       map[Ip4Path]MultiHopId{},
		paths6:       map[Ip6Path]MultiHopId{},
	}
}

func (self *pathTable) Destinations() []MultiHopId {
	return slices.Clone(self.destinations)
}

func (self *pathTable) DestinationIds() []Id {
	var clientIds []Id
	for _, destination := range self.destinations {
		clientIds = append(clientIds, destination.Tail())
	}
	return clientIds
}

func (self *pathTable) SelectDestination(packet []byte) (MultiHopId, error) {
	if len(self.destinations) == 0 {
		return MultiHopId{}, fmt.Errorf("No destinations")
	}
	if len(self.destinations) == 1 {
		return self.destinations[0], nil
	}

	ipPath, err := ParseIpPath(packet)
	if err != nil {
		return MultiHopId{}, err
	}
	switch ipPath.Version {
	case 4:
		ip4Path := ipPath.ToIp4Path()
		if destination, ok := self.paths4[ip4Path]; ok {
			return destination, nil
		}
		i := mathrand.Intn(len(self.destinations))
		destination := self.destinations[i]
		self.paths4[ip4Path] = destination
		return destination, nil
	case 6:
		ip6Path := ipPath.ToIp6Path()
		if destination, ok := self.paths6[ip6Path]; ok {
			return destination, nil
		}
		i := mathrand.Intn(len(self.destinations))
		destination := self.destinations[i]
		self.paths6[ip6Path] = destination
		return destination, nil
	default:
		// no support for this version
		return MultiHopId{}, fmt.Errorf("No support for ip version %d", ipPath.Version)
	}
}

type IpProtocol int

const (
	IpProtocolUnknown IpProtocol = 0
	IpProtocolTcp     IpProtocol = 1
	IpProtocolUdp     IpProtocol = 2
)

func (self IpProtocol) String() string {
	switch self {
	case IpProtocolTcp:
		return "tcp"
	case IpProtocolUdp:
		return "udp"
	default:
		return "unknown"
	}
}

type IpPath struct {
	Version         int
	Protocol        IpProtocol
	SourceIp        net.IP
	SourcePort      int
	DestinationIp   net.IP
	DestinationPort int

	SequenceNumber    uint32
	AckSequenceNumber uint32
	Syn               bool
	Rst               bool
	Ack               bool

	ServerName string
}

func ParseIpPath(ipPacket []byte) (*IpPath, error) {
	ipPath, _, err := ParseIpPathWithPayload(ipPacket)
	return ipPath, err
}

func ParseIpPathWithPayload(ipPacket []byte) (*IpPath, []byte, error) {
	if len(ipPacket) == 0 {
		return nil, nil, fmt.Errorf("Empty packet.")
	}
	ipVersion := uint8(ipPacket[0]) >> 4
	var ipProtocol layers.IPProtocol
	var sourceIp net.IP
	var destinationIp net.IP
	var transport []byte
	var ok bool
	switch ipVersion {
	case 4:
		ipProtocol, sourceIp, destinationIp, transport, ok = parseIpv4(ipPacket)
	case 6:
		ipProtocol, sourceIp, destinationIp, transport, ok = parseIpv6(ipPacket)
	default:
		// no support for this version
		return nil, nil, fmt.Errorf("No support for ip version %d", ipVersion)
	}
	if !ok {
		return nil, nil, fmt.Errorf("Malformed ip packet.")
	}

	// copy the ips so the ip path can be retained independently of the shared
	// packet buffer (which is recycled after the handoff call). both copies share
	// one backing allocation instead of one per address.
	ipBacking := make(net.IP, len(sourceIp)+len(destinationIp))
	sn := copy(ipBacking, sourceIp)
	copy(ipBacking[sn:], destinationIp)
	sourceIpCopy := ipBacking[:sn:sn]
	destinationIpCopy := ipBacking[sn:]

	switch ipProtocol {
	case layers.IPProtocolUDP:
		var udp parsedUdp
		if !parseUdpPacket(sourceIp, destinationIp, transport, &udp) {
			return nil, nil, fmt.Errorf("Malformed udp packet.")
		}

		return &IpPath{
			Version:         int(ipVersion),
			Protocol:        IpProtocolUdp,
			SourceIp:        sourceIpCopy,
			SourcePort:      int(udp.sourcePort),
			DestinationIp:   destinationIpCopy,
			DestinationPort: int(udp.destinationPort),
		}, udp.payload, nil
	case layers.IPProtocolTCP:
		var tcp parsedTcp
		if !parseTcpPacket(sourceIp, destinationIp, transport, &tcp) {
			return nil, nil, fmt.Errorf("Malformed tcp packet.")
		}

		return &IpPath{
			Version:           int(ipVersion),
			Protocol:          IpProtocolTcp,
			SourceIp:          sourceIpCopy,
			SourcePort:        int(tcp.sourcePort),
			DestinationIp:     destinationIpCopy,
			DestinationPort:   int(tcp.destinationPort),
			SequenceNumber:    tcp.seq,
			AckSequenceNumber: tcp.ackNumber,
			Syn:               tcp.syn,
			Rst:               tcp.rst,
			Ack:               tcp.ack,
		}, tcp.payload, nil
	default:
		// no support for this protocol
		return nil, nil, fmt.Errorf("No support for protocol %d", ipProtocol)
	}
}

func (self *IpPath) SourceHostPort() string {
	return net.JoinHostPort(
		self.SourceIp.String(),
		strconv.Itoa(self.SourcePort),
	)
}

func (self *IpPath) DestinationHostPort() string {
	return net.JoinHostPort(
		self.DestinationIp.String(),
		strconv.Itoa(self.DestinationPort),
	)
}

func (self *IpPath) ToIp4Path() Ip4Path {
	var sourceIp [4]byte
	if self.SourceIp != nil {
		if sourceIp4 := self.SourceIp.To4(); sourceIp4 != nil {
			sourceIp = [4]byte(sourceIp4)
		}
	}
	var destinationIp [4]byte
	if self.DestinationIp != nil {
		if destinationIp4 := self.DestinationIp.To4(); destinationIp4 != nil {
			destinationIp = [4]byte(destinationIp4)
		}
	}
	return Ip4Path{
		Protocol:        self.Protocol,
		SourceIp:        sourceIp,
		SourcePort:      self.SourcePort,
		DestinationIp:   destinationIp,
		DestinationPort: self.DestinationPort,
		ServerName:      self.ServerName,
	}
}

func (self *IpPath) ToIp6Path() Ip6Path {
	var sourceIp [16]byte
	if self.SourceIp != nil {
		if sourceIp6 := self.SourceIp.To16(); sourceIp6 != nil {
			sourceIp = [16]byte(sourceIp6)
		}
	}
	var destinationIp [16]byte
	if self.DestinationIp != nil {
		if destinationIp6 := self.DestinationIp.To16(); destinationIp6 != nil {
			destinationIp = [16]byte(destinationIp6)
		}
	}
	return Ip6Path{
		Protocol:        self.Protocol,
		SourceIp:        sourceIp,
		SourcePort:      self.SourcePort,
		DestinationIp:   destinationIp,
		DestinationPort: self.DestinationPort,
		ServerName:      self.ServerName,
	}
}

func (self *IpPath) Source() *IpPath {
	return &IpPath{
		Protocol:   self.Protocol,
		Version:    self.Version,
		SourceIp:   self.SourceIp,
		SourcePort: self.SourcePort,
	}
}

func (self *IpPath) Destination() *IpPath {
	return &IpPath{
		Protocol:        self.Protocol,
		Version:         self.Version,
		DestinationIp:   self.DestinationIp,
		DestinationPort: self.DestinationPort,
	}
}

func (self *IpPath) Reverse() *IpPath {
	return &IpPath{
		Protocol:        self.Protocol,
		Version:         self.Version,
		SourceIp:        self.DestinationIp,
		SourcePort:      self.DestinationPort,
		DestinationIp:   self.SourceIp,
		DestinationPort: self.SourcePort,
	}
}

// comparable
type Ip4Path struct {
	Protocol        IpProtocol
	SourceIp        [4]byte
	SourcePort      int
	DestinationIp   [4]byte
	DestinationPort int
	ServerName      string
}

func (self *Ip4Path) Source() Ip4Path {
	return Ip4Path{
		Protocol:   self.Protocol,
		SourceIp:   self.SourceIp,
		SourcePort: self.SourcePort,
	}
}

func (self *Ip4Path) Destination() Ip4Path {
	return Ip4Path{
		Protocol:        self.Protocol,
		DestinationIp:   self.DestinationIp,
		DestinationPort: self.DestinationPort,
	}
}

// comparable
type Ip6Path struct {
	Protocol        IpProtocol
	SourceIp        [16]byte
	SourcePort      int
	DestinationIp   [16]byte
	DestinationPort int
	ServerName      string
}

func (self *Ip6Path) Source() Ip6Path {
	return Ip6Path{
		Protocol:   self.Protocol,
		SourceIp:   self.SourceIp,
		SourcePort: self.SourcePort,
	}
}

func (self *Ip6Path) Destination() Ip6Path {
	return Ip6Path{
		Protocol:        self.Protocol,
		DestinationIp:   self.DestinationIp,
		DestinationPort: self.DestinationPort,
	}
}

type UserLimited interface {
	LastActivityTime() time.Time
	Cancel()
}

type userLimited struct {
	mutex            sync.Mutex
	lastActivityTime time.Time
}

func newUserLimited() *userLimited {
	return &userLimited{
		lastActivityTime: time.Now(),
	}
}

func (self *userLimited) LastActivityTime() time.Time {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.lastActivityTime
}

func (self *userLimited) UpdateLastActivityTime() {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.lastActivityTime = time.Now()
}

func applyLruUserLimit[R UserLimited](resources []R, ulimit int, limitCallback func(R) bool) {
	// limit the total connections per source to avoid blowing up the ulimit
	if n := len(resources) - ulimit; 0 < n {
		resourceLastActivityTimes := map[UserLimited]time.Time{}
		for _, resource := range resources {
			resourceLastActivityTimes[resource] = resource.LastActivityTime()
		}
		// order by last activity time
		slices.SortFunc(resources, func(a R, b R) int {
			lastActivityTimeA := resourceLastActivityTimes[a]
			lastActivityTimeB := resourceLastActivityTimes[b]
			if lastActivityTimeA.Before(lastActivityTimeB) {
				return -1
			} else if lastActivityTimeB.Before(lastActivityTimeA) {
				return 1
			} else {
				return 0
			}
		})
		i := 0
		for _, resource := range resources {
			if limitCallback(resource) {
				i += 1
				resource.Cancel()
			}
			if n <= i {
				break
			}
		}
	}
}
