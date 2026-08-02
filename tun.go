package connect

// a userspace tun device backed by the gvisor network stack.
// `Tun` exposes a packet interface on one side (`Read`/`Write`) and
// socket interfaces on the other (`DialContext`, `ListenTCP`, `ListenUDP`).
// all tun instances share a single gvisor stack, with one nic and one
// link-local ipv4 address per instance.

import (
	// "bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"runtime"
	// "regexp"
	mathrand "math/rand"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	// "github.com/google/gopacket"
	// "github.com/google/gopacket/layers"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// const DefaultChannelSize = 64

func DefaultTunSettings() *TunSettings {
	return DefaultTunSettingsWithBufferSize(1024)
}

func DefaultTunSettingsWithBufferSize(bufferSize int) *TunSettings {
	return &TunSettings{
		ChannelSize: bufferSize,
		// must match `DefaultMtu`. packets are written directly into the
		// receiver tap/tun interface, so this must not exceed the device
		// interface mtu.
		Mtu: 1440,

		DialRace:          2,
		DialRaceTimeout:   2 * time.Second,
		DialTimeout:       30 * time.Second,
		DohRequestTimeout: 60 * time.Second,

		// the gvisor udp endpoint buffers default to 32KiB, which is too small for fast
		// transfer; cap at 1MiB (gvisor clamps to at most 4MiB) to bound per-endpoint
		// memory on the shared stack used by the server/proxy.
		// per endpoint, so scaled by the memory budget.
		UdpReceiveBufferByteCount: int(MemoryScaledByteCount(mib(1), kib(128))),
		UdpSendBufferByteCount:    int(MemoryScaledByteCount(mib(1), kib(128))),

		// tcp buffer auto-tuning ranges for the server/proxy data plane (the shared
		// stack). Max applies per connection, so it caps per-connection memory; a
		// memory-constrained IpMux on a private stack shrinks these much further.
		// default and max are per connection, so scaled by the memory budget.
		TcpReceiveBuffer: TcpBufferRange{
			Min:     4 * 1024,
			Default: int(MemoryScaledByteCount(kib(256), kib(64))),
			Max:     int(MemoryScaledByteCount(mib(1), kib(256))),
		},
		TcpSendBuffer: TcpBufferRange{
			Min:     4 * 1024,
			Default: int(MemoryScaledByteCount(kib(256), kib(64))),
			Max:     int(MemoryScaledByteCount(mib(1), kib(256))),
		},
	}
}

type TunSettings struct {
	// Log, when set, is used by the tun. nil resolves to `DefaultLogger()`.
	Log Logger

	ChannelSize int
	Mtu         int

	DialRace        int
	DialRaceTimeout time.Duration
	DialTimeout     time.Duration

	// DohRequestTimeout bounds a single DoH request through this tun's resolver (total connect +
	// TLS + query). The IpMux sets it from DnsUpgradeSettings.ResolveTimeout so DNS resolution has
	// a single timeout knob. 0 falls back to a default.
	DohRequestTimeout time.Duration

	// DohSettings, when set, is the base settings for the tun's resolver cache — a
	// memory-constrained consumer bounds the cache and fan-out here (see the UpgradeMux).
	// buildDohCache copies it and overlays the tun dialer, log, timeouts, and resolver
	// settings, so those fields of the base are ignored. nil uses DefaultDohSettings.
	DohSettings *DohSettings

	UdpReceiveBufferByteCount int
	UdpSendBufferByteCount    int

	// TcpReceiveBuffer/TcpSendBuffer are the gVisor TCP buffer auto-tuning ranges. Max
	// applies per connection, so these dominate per-connection memory; a memory-bound
	// consumer should lower them.
	TcpReceiveBuffer TcpBufferRange
	TcpSendBuffer    TcpBufferRange
}

// TcpBufferRange is a gVisor TCP buffer auto-tuning range in bytes.
type TcpBufferRange struct {
	Min     int
	Default int
	Max     int
}

func newTunStack(tcpReceive TcpBufferRange, tcpSend TcpBufferRange) *stack.Stack {
	opts := stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocolWithOptions(ipv4.Options{AllowExternalLoopbackTraffic: true})},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4},
		HandleLocal:        true,
	}
	s := stack.New(opts)

	// size the tcp buffer ranges above the gvisor defaults.
	// inbound segments are accounted against the receive buffer size, and
	// in-window segments that exceed it are dropped. senders into the tun do
	// not retransmit, so the receive buffer needs headroom for inbound bursts
	// above the advertised window.
	{
		opt := tcpip.TCPReceiveBufferSizeRangeOption{
			Min:     tcpReceive.Min,
			Default: tcpReceive.Default,
			Max:     tcpReceive.Max,
		}
		s.SetTransportProtocolOption(tcp.ProtocolNumber, &opt)
	}
	{
		opt := tcpip.TCPSendBufferSizeRangeOption{
			Min:     tcpSend.Min,
			Default: tcpSend.Default,
			Max:     tcpSend.Max,
		}
		s.SetTransportProtocolOption(tcp.ProtocolNumber, &opt)
	}

	return s
}

type NicIdAllocator struct {
	stateLock   sync.Mutex
	counter     uint32
	freeList    []tcpip.NICID
	maxFreeList int
}

func NewNicIdAllocator(maxFreeList int) *NicIdAllocator {
	return &NicIdAllocator{
		maxFreeList: maxFreeList,
	}
}

func (self *NicIdAllocator) TakeNicId() tcpip.NICID {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if n := len(self.freeList); n > 0 {
		id := self.freeList[n-1]
		self.freeList = self.freeList[:n-1]
		return id
	}
	self.counter += 1
	return tcpip.NICID(self.counter)
}

func (self *NicIdAllocator) ReturnNicId(id tcpip.NICID) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(self.freeList) >= self.maxFreeList {
		return
	}
	self.freeList = append(self.freeList, id)
}

var defaultNicIdAllocator = NewNicIdAllocator(128)

type LocalIpv4AddressAllocator struct {
	stateLock   sync.Mutex
	generator   *AddrGenerator
	freeList    []netip.Addr
	maxFreeList int
}

func NewLocalIpv4AddressAllocator(prefix netip.Prefix, maxFreeList int) *LocalIpv4AddressAllocator {
	return &LocalIpv4AddressAllocator{
		generator:   NewAddrGenerator(prefix),
		maxFreeList: maxFreeList,
	}
}

func (self *LocalIpv4AddressAllocator) TakeAddr() (netip.Addr, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if n := len(self.freeList); n > 0 {
		addr := self.freeList[n-1]
		self.freeList = self.freeList[:n-1]
		return addr, true
	}
	return self.generator.Next()
}

func (self *LocalIpv4AddressAllocator) ReturnAddr(addr netip.Addr) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(self.freeList) >= self.maxFreeList {
		return
	}
	self.freeList = append(self.freeList, addr)
}

// defaultLocalIpv4AddressAllocator is created lazily on first use so that merely
// importing connect does not spin up the generator goroutine (NewAddrGenerator
// launches one) unless a local address is actually reserved.
var defaultLocalIpv4AddressAllocator = sync.OnceValue(func() *LocalIpv4AddressAllocator {
	return NewLocalIpv4AddressAllocator(
		netip.MustParsePrefix("169.254.0.0/16"),
		128,
	)
})

// TakeLocalIpv4Address reserves a process-unique local IPv4 address from the default
// 169.254.0.0/16 pool shared by Tun and the SDK tunnel address. Return it with
// ReturnLocalIpv4Address when the address is no longer in use.
func TakeLocalIpv4Address() (netip.Addr, bool) {
	return defaultLocalIpv4AddressAllocator().TakeAddr()
}

// ReturnLocalIpv4Address returns an address previously taken with
// TakeLocalIpv4Address to the pool's free list.
func ReturnLocalIpv4Address(addr netip.Addr) {
	defaultLocalIpv4AddressAllocator().ReturnAddr(addr)
}

// LocalIpv4Networks returns the IPv4 networks currently assigned to the device's
// interfaces (each masked to its prefix), best-effort. Callers use it to avoid
// handing out a tunnel address that overlaps a real local subnet. On platforms
// where interface enumeration is restricted it returns nil, and the caller falls
// back to an unchecked random address.
func LocalIpv4Networks() []netip.Prefix {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var networks []netip.Prefix
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipNet.IP.To4()
			if ip4 == nil {
				continue
			}
			ones, bits := ipNet.Mask.Size()
			if bits != 32 {
				continue
			}
			prefix := netip.PrefixFrom(netip.AddrFrom4([4]byte{ip4[0], ip4[1], ip4[2], ip4[3]}), ones)
			networks = append(networks, prefix.Masked())
		}
	}
	return networks
}

// RandomLocalIpv4 returns a tunnel address in 10.0.0.0/8 whose /24 is the
// lexicographically smallest 10.a.b.0/24 that does not overlap any prefix in
// `avoid` (the device's real local subnets), with the host octet randomized in
// [2, 254] so it looks like an ordinary DHCP lease (never .0/.1/.255) instead of
// a fixed value that could fingerprint the network. Preferring the minimum free
// /24 lands on common subnets such as 10.0.0.0/24 that blend in. If every /24 is
// excluded (e.g. the device holds all of 10/8), it falls back to 10.0.0.h.
func RandomLocalIpv4(avoid []netip.Prefix) netip.Addr {
	h := byte(2 + mathrand.Intn(253)) // 2..254, never .0/.1/.255
	for a := 0; a < 256; a++ {
		for b := 0; b < 256; b++ {
			subnet := netip.PrefixFrom(netip.AddrFrom4([4]byte{10, byte(a), byte(b), 0}), 24)
			conflict := false
			for _, network := range avoid {
				if network.Overlaps(subnet) {
					conflict = true
					break
				}
			}
			if !conflict {
				return netip.AddrFrom4([4]byte{10, byte(a), byte(b), h})
			}
		}
	}
	return netip.AddrFrom4([4]byte{10, 0, 0, h})
}

type Tun struct {
	ctx    context.Context
	cancel context.CancelFunc
	log    Logger

	settings *TunSettings

	ep                        *tunLinkEndpoint
	stack                     *stack.Stack
	nicId                     tcpip.NICID
	nicIdAllocator            *NicIdAllocator
	localAddresses            []netip.Addr
	localIpv4AddressAllocator *LocalIpv4AddressAllocator
	// mtu                 int
	// registeredAddresses map[netip.Addr]bool
	dohResolver atomic.Pointer[DohCache]

	// tcpInboundShards preserve same-flow injection order while independent
	// browser connections continue in parallel.
	tcpInboundShards [tunTcpInboundShardCount]tunTcpInboundShard

	// closeOnce makes Close idempotent: a second Close must not return the nic id and
	// local address to the shared allocators again — a double return hands the same
	// address out twice, and two later tuns silently share it (breaking their routing).
	closeOnce sync.Once

	stateLock sync.Mutex
}

// The TCP inbound shards, the lossless tunLinkEndpoint, the InjectInbound
// PacketBuffer release, and the absolute-deadline dial race below are ported
// wholesale from upstream main e05ecee (our fork never modified tun.go, so
// the upstream file was taken as-is). The single entangled sibling,
// udp_receive_dispatch_test.go, was NOT taken: it exercises upstream's
// udpReceiveDispatcher, which lives in their restructured ip.go and comes
// with the eventual structural port.
const (
	// Reconcile each producer burst well below gVisor's 100-segment processing
	// quantum. A processor that meets a syscall-owned endpoint relies on the
	// subsequent user unlock to requeue it; the transfer shim has no return-path
	// retransmission with which to recover from a missed handoff.
	tunTcpInboundBurstPacketCount = 16
	tunTcpInboundShardCount       = 32
)

// tunTcpInboundShard bounds one set of TCP flow handoffs without serializing
// unrelated flows. Its fixed arrays make memory independent of flow churn.
type tunTcpInboundShard struct {
	writeLock     sync.Mutex
	packetCount   uint32
	endpointIds   [tunTcpInboundBurstPacketCount]stack.TransportEndpointID
	endpointCount int
}

// tcpInboundFlow parses the endpoint identity and stable shard of a complete,
// unfragmented IPv4 TCP packet.
func tcpInboundFlow(packet []byte) (stack.TransportEndpointID, int, bool) {
	if len(packet) < header.IPv4MinimumSize || packet[0]>>4 != 4 || packet[9] != uint8(header.TCPProtocolNumber) {
		return stack.TransportEndpointID{}, 0, false
	}
	ipHeaderByteCount := int(packet[0]&0x0f) * 4
	if ipHeaderByteCount < header.IPv4MinimumSize ||
		len(packet) < ipHeaderByteCount+header.TCPMinimumSize ||
		binary.BigEndian.Uint16(packet[6:8])&0x1fff != 0 {
		return stack.TransportEndpointID{}, 0, false
	}
	transport := packet[ipHeaderByteCount:]
	localPort := binary.BigEndian.Uint16(transport[2:4])
	remotePort := binary.BigEndian.Uint16(transport[0:2])
	endpointId := stack.TransportEndpointID{
		LocalPort:     localPort,
		LocalAddress:  tcpip.AddrFrom4Slice(packet[16:20]),
		RemotePort:    remotePort,
		RemoteAddress: tcpip.AddrFrom4Slice(packet[12:16]),
	}
	flowHash := uint32(localPort)<<16 | uint32(remotePort)
	flowHash ^= binary.BigEndian.Uint32(packet[12:16])
	flowHash ^= binary.BigEndian.Uint32(packet[16:20])
	flowHash ^= flowHash >> 16
	return endpointId, int(flowHash & (tunTcpInboundShardCount - 1)), true
}

// addTcpInboundEndpointWithLock records an endpoint once in the current
// bounded burst. The shard write lock must be held.
func (self *Tun) addTcpInboundEndpointWithLock(shard *tunTcpInboundShard, endpointId stack.TransportEndpointID) {
	for endpointIndex := 0; endpointIndex < shard.endpointCount; endpointIndex += 1 {
		if shard.endpointIds[endpointIndex] == endpointId {
			return
		}
	}
	shard.endpointIds[shard.endpointCount] = endpointId
	shard.endpointCount += 1
}

// advanceTcpInboundShardWithLock records one injection and reports when its
// shard needs an endpoint handoff. The shard write lock must be held.
func (self *Tun) advanceTcpInboundShardWithLock(shard *tunTcpInboundShard, endpointId stack.TransportEndpointID) bool {
	self.addTcpInboundEndpointWithLock(shard, endpointId)
	shard.packetCount += 1
	if shard.packetCount < tunTcpInboundBurstPacketCount {
		return false
	}
	shard.packetCount = 0
	return true
}

// synchronizeTcpInboundProcessorsWithLock performs gVisor's documented user
// unlock handoff for every endpoint touched in the burst. The shard write lock
// remains held so the next burst cannot overtake the handoff.
func (self *Tun) synchronizeTcpInboundProcessorsWithLock(shard *tunTcpInboundShard) {
	for endpointIndex := 0; endpointIndex < shard.endpointCount; endpointIndex += 1 {
		endpointId := shard.endpointIds[endpointIndex]
		stackEndpoint := self.stack.FindTransportEndpoint(
			ipv4.ProtocolNumber,
			tcp.ProtocolNumber,
			endpointId,
			self.nicId,
		)
		if endpoint, ok := stackEndpoint.(*tcp.Endpoint); ok {
			endpoint.LockUser()
			endpoint.UnlockUser()
		}
	}
	shard.endpointCount = 0
	// UnlockUser requeues protocol work but does not run it synchronously.
	// Yield once while this shard remains gated so the awakened worker cannot
	// be starved by an immediately reacquired producer lock.
	runtime.Gosched()
}

// tunLinkEndpoint converts channel.Endpoint's silent bounded-queue drop into
// bounded backpressure. The user-NAT TCP bridge is intentionally lossless and
// does not retransmit its return path, so dropping one ACK here can otherwise
// strand a flow forever at its advertised receive window.
type tunLinkEndpoint struct {
	*channel.Endpoint
	ctx   context.Context
	space chan struct{}
}

func newTunLinkEndpoint(ctx context.Context, size int, mtu uint32, linkAddr tcpip.LinkAddress) *tunLinkEndpoint {
	return &tunLinkEndpoint{
		Endpoint: channel.New(size, mtu, linkAddr),
		ctx:      ctx,
		space:    make(chan struct{}, 1),
	}
}

func (self *tunLinkEndpoint) notifySpace() {
	select {
	case self.space <- struct{}{}:
	default:
	}
}

func (self *tunLinkEndpoint) Read() *stack.PacketBuffer {
	packet := self.Endpoint.Read()
	if packet != nil {
		self.notifySpace()
	}
	return packet
}

func (self *tunLinkEndpoint) ReadContext(ctx context.Context) *stack.PacketBuffer {
	packet := self.Endpoint.ReadContext(ctx)
	if packet != nil {
		self.notifySpace()
	}
	return packet
}

func (self *tunLinkEndpoint) WritePackets(packets stack.PacketBufferList) (int, tcpip.Error) {
	packetSlice := packets.AsSlice()
	written := 0
	remaining := packets
	for {
		n, err := self.Endpoint.WritePackets(remaining)
		written += n
		if err != nil || written == len(packetSlice) {
			return written, err
		}

		// A partial batch is unusual (gVisor normally passes one packet), so
		// construct its suffix only on queue saturation. A completely rejected
		// one-packet write reuses the original list without allocating.
		if 0 < n {
			remaining = stack.PacketBufferList{}
			for _, packet := range packetSlice[written:] {
				remaining.PushBack(packet)
			}
		}

		select {
		case <-self.ctx.Done():
			return written, &tcpip.ErrClosedForSend{}
		case <-self.space:
		}
	}
}

func CreateTunWithDefaults(ctx context.Context) (*Tun, error) {
	return CreateTun(ctx, DefaultTunSettings())
}

func CreateTun(ctx context.Context, settings *TunSettings) (*Tun, error) {
	return CreateTunWithResolver(ctx, settings, nil)
}

func CreateTunWithResolver(ctx context.Context, settings *TunSettings, dnsResolverSettings *DnsResolverSettings) (*Tun, error) {
	cancelCtx, cancel := context.WithCancel(ctx)

	nicIdAllocator := defaultNicIdAllocator
	localIpv4AddressAllocator := defaultLocalIpv4AddressAllocator()

	localIpv4Address, ok := localIpv4AddressAllocator.TakeAddr()
	if !ok {
		cancel()
		return nil, fmt.Errorf("No more local addresses")
	}

	nicId := nicIdAllocator.TakeNicId()

	// each Tun owns a private gVisor stack, destroyed on Close() so all of its
	// endpoints are reclaimed. (There is no shared stack: it could not reclaim a
	// closed Tun's connection endpoints, leaking them under Tun churn.)
	tunStackInstance := newTunStack(settings.TcpReceiveBuffer, settings.TcpSendBuffer)

	localAddresses := []netip.Addr{
		localIpv4Address,
	}

	ep := newTunLinkEndpoint(
		cancelCtx,
		settings.ChannelSize,
		uint32(settings.Mtu),
		tcpip.LinkAddress(fmt.Sprintf("%x", nicId)),
	)

	releaseOnError := func() {
		ep.Close()
		nicIdAllocator.ReturnNicId(nicId)
		for _, addr := range localAddresses {
			if addr.Is4() {
				localIpv4AddressAllocator.ReturnAddr(addr)
			}
		}
		cancel()
	}

	tun := &Tun{
		ctx:                       cancelCtx,
		cancel:                    cancel,
		log:                       loggerOrDefault(settings.Log),
		settings:                  settings,
		ep:                        ep,
		stack:                     tunStackInstance,
		nicId:                     nicId,
		nicIdAllocator:            nicIdAllocator,
		localAddresses:            localAddresses,
		localIpv4AddressAllocator: localIpv4AddressAllocator,
	}

	tun.dohResolver.Store(tun.buildDohCache(dnsResolverSettings, settings.DohRequestTimeout))

	if tcpipErr := tun.stack.CreateNIC(nicId, ep); tcpipErr != nil {
		releaseOnError()
		return nil, fmt.Errorf("Could not create nic err=%s", tcpipErr)
	}

	for _, ip := range localAddresses {
		var protoNumber tcpip.NetworkProtocolNumber
		if ip.Is4() {
			protoNumber = ipv4.ProtocolNumber
		} else if ip.Is6() {
			protoNumber = ipv6.ProtocolNumber
		}
		protoAddr := tcpip.ProtocolAddress{
			Protocol:          protoNumber,
			AddressWithPrefix: tcpip.AddrFromSlice(ip.AsSlice()).WithPrefix(),
		}

		if tcpipErr := tun.stack.AddProtocolAddress(nicId, protoAddr, stack.AddressProperties{}); tcpipErr != nil {
			tun.stack.RemoveNIC(nicId)
			releaseOnError()
			return nil, fmt.Errorf("Could not create add nic address err=%s", tcpipErr)
		}
	}
	tun.stack.AddRoute(tcpip.Route{Destination: header.IPv4EmptySubnet, NIC: nicId})

	return tun, nil
}

func (self *Tun) DohCache() *DohCache {
	return self.dohResolver.Load()
}

// buildDohCache constructs a DohCache resolving through this tun (remote paths dial via
// the tun; local paths use the host), with the given resolver settings (nil = default).
func (self *Tun) buildDohCache(dnsResolverSettings *DnsResolverSettings, requestTimeout time.Duration) *DohCache {
	var dohSettings *DohSettings
	if self.settings.DohSettings != nil {
		// copy so the overlays below don't mutate the caller's base
		copied := *self.settings.DohSettings
		dohSettings = &copied
	} else {
		dohSettings = DefaultDohSettings()
	}
	dohSettings.ConnectSettings.Log = self.log
	dohSettings.RequestTimeout = requestTimeout
	if dohSettings.RequestTimeout <= 0 {
		dohSettings.RequestTimeout = 60 * time.Second
	}
	dohSettings.TlsTimeout = 30 * time.Second
	dohSettings.DialContextSettings = &DialContextSettings{
		DialContext: self.DialContext,
	}
	if dnsResolverSettings != nil {
		dohSettings.DnsResolverSettings = dnsResolverSettings
	}
	return NewDohCache(dohSettings)
}

// SetDnsResolverSettings rebuilds the tun's DohCache with new resolver settings and DoH request
// timeout, taking effect for subsequent queries. Retiring the prior cache cancels its in-flight
// requests so no old-path dial or pooled connection survives the swap. Safe to call concurrently
// with DohCache()/Query.
func (self *Tun) SetDnsResolverSettings(dnsResolverSettings *DnsResolverSettings, requestTimeout time.Duration) {
	if replaced := self.dohResolver.Swap(self.buildDohCache(dnsResolverSettings, requestTimeout)); replaced != nil {
		// release the replaced cache's pooled connections (and their endpoints on this
		// tun's stack) now instead of holding both generations until the idle timeout
		replaced.Close()
	}
}

// LocalAddresses returns the addresses assigned to the internal stack's NIC
// (reserved from the shared local-address pool). Callers must not mutate it.
func (self *Tun) LocalAddresses() []netip.Addr {
	return self.localAddresses
}

func (self *Tun) Read() ([]byte, error) {
	// read directly from the gvisor endpoint's outbound queue. that queue is
	// itself a buffered FIFO (size `ChannelSize`) that drops on overflow, so it
	// is the sequence buffer. ReadContext blocks until a packet is available or
	// the ctx is canceled.
	pkt := self.ep.ReadContext(self.ctx)
	if pkt == nil {
		return nil, fmt.Errorf("Done")
	}
	packet := messagePoolCopyPacketBuffer(pkt)
	pkt.DecRef()
	return packet, nil
}

// messagePoolCopyPacketBuffer copies a packet buffer's bytes into a pooled message
// with a single copy and no intermediate allocation (ToView would deep-copy into an
// intermediate view first, on every packet the stack emits).
func messagePoolCopyPacketBuffer(pkt *stack.PacketBuffer) []byte {
	packet := MessagePoolGet(pkt.Size())
	vl, offset := pkt.AsViewList()
	i := 0
	for v := vl.Front(); v != nil; v = v.Next() {
		s := v.AsSlice()
		if 0 < offset {
			if len(s) <= offset {
				offset -= len(s)
				continue
			}
			s = s[offset:]
			offset = 0
		}
		i += copy(packet[i:], s)
	}
	return packet[:i]
}

// reads one or more packets, blocking until at least one is available.
// fills up to `len(packets)` entries and returns the count.
// a batch read wakes the reader once per burst instead of once per packet.
func (self *Tun) ReadBatch(packets [][]byte) (int, error) {
	if len(packets) == 0 {
		return 0, nil
	}
	// block for the first packet, then drain whatever else is already queued
	// without blocking. the gvisor endpoint queue is the sequence buffer (it
	// drops on overflow), and a single reader popping it preserves per-flow order.
	pkt := self.ep.ReadContext(self.ctx)
	if pkt == nil {
		return 0, fmt.Errorf("Done")
	}
	n := 0
	for pkt != nil {
		packets[n] = messagePoolCopyPacketBuffer(pkt)
		pkt.DecRef()
		n += 1
		if n >= len(packets) {
			break
		}
		pkt = self.ep.Read()
	}
	return n, nil
}

// safe to call from multiple goroutines
func (self *Tun) Write(packet []byte) (int, error) {
	return self.write(packet, nil)
}

// write injects one packet and releases the creator's PacketBuffer reference
// after gVisor has taken any references it needs. onRelease is test
// instrumentation invoked when every gVisor reference has also been released.
func (self *Tun) write(packet []byte, onRelease func()) (int, error) {
	// defer MessagePoolReturn(packet)

	if len(packet) == 0 {
		return 0, nil
	}

	endpointId, shardIndex, tcpInbound := tcpInboundFlow(packet)
	var tcpInboundShard *tunTcpInboundShard
	synchronize := false
	if tcpInbound {
		tcpInboundShard = &self.tcpInboundShards[shardIndex]
		tcpInboundShard.writeLock.Lock()
		synchronize = self.advanceTcpInboundShardWithLock(tcpInboundShard, endpointId)
	}

	// copy the packet
	pkb := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload:   buffer.MakeWithData(packet),
		OnRelease: onRelease,
	})
	// InjectInbound borrows the creator's reference. The stack takes its own
	// references for asynchronous work, exactly as gVisor's TUN and veth link
	// endpoints do. Without this release, every inbound packet and its copied
	// payload remain live for the process lifetime.

	switch packet[0] >> 4 {
	case 4:
		self.ep.InjectInbound(header.IPv4ProtocolNumber, pkb)
		pkb.DecRef()
		if tcpInbound {
			if synchronize {
				self.synchronizeTcpInboundProcessorsWithLock(tcpInboundShard)
			}
			tcpInboundShard.writeLock.Unlock()
		}
		return len(packet), nil
	default:
		pkb.DecRef()
		if tcpInbound {
			tcpInboundShard.writeLock.Unlock()
		}
		return 0, syscall.EAFNOSUPPORT
	}
}

func (self *Tun) convertToFullAddr(endpoint netip.AddrPort) (tcpip.FullAddress, tcpip.NetworkProtocolNumber) {
	var protoNumber tcpip.NetworkProtocolNumber
	if endpoint.Addr().Is4() {
		protoNumber = ipv4.ProtocolNumber
	} else {
		protoNumber = ipv6.ProtocolNumber
	}
	return tcpip.FullAddress{
		NIC:  self.nicId,
		Addr: tcpip.AddrFromSlice(endpoint.Addr().AsSlice()),
		Port: endpoint.Port(),
	}, protoNumber
}

func (self *Tun) dialCtx(ctx context.Context) context.Context {
	if ctx == self.ctx {
		return ctx
	}
	dialCtx, dialCancel := context.WithCancel(self.ctx)
	go func() {
		defer dialCancel()
		select {
		case <-ctx.Done():
		case <-self.ctx.Done():
		}
	}()
	return dialCtx
}

func (self *Tun) ListenTCP(addr *net.TCPAddr) (*gonet.TCPListener, error) {
	var addrPort netip.AddrPort
	if addr != nil {
		ip, _ := netip.AddrFromSlice(addr.IP)
		addrPort = netip.AddrPortFrom(ip, uint16(addr.Port))
	}
	fa, pn := self.convertToFullAddr(addrPort)
	return gonet.ListenTCP(self.stack, fa, pn)
}

func (self *Tun) ListenUDP(laddr *net.UDPAddr) (*gonet.UDPConn, error) {
	var addrPort netip.AddrPort
	if laddr != nil {
		ip, _ := netip.AddrFromSlice(laddr.IP)
		addrPort = netip.AddrPortFrom(ip, uint16(laddr.Port))
	}
	lfa, pn := self.convertToFullAddr(addrPort)
	return self.dialUdp(&lfa, nil, pn)
}

// creates a udp endpoint with the tun buffer sizes applied.
// this mirrors `gonet.DialUDP` with sized endpoint buffers.
func (self *Tun) dialUdp(laddr *tcpip.FullAddress, raddr *tcpip.FullAddress, protoNumber tcpip.NetworkProtocolNumber) (*gonet.UDPConn, error) {
	wq := &waiter.Queue{}
	ep, tcpipErr := self.stack.NewEndpoint(udp.ProtocolNumber, protoNumber, wq)
	if tcpipErr != nil {
		return nil, fmt.Errorf("Could not create udp endpoint err=%s", tcpipErr)
	}

	ep.SocketOptions().SetReceiveBufferSize(int64(self.settings.UdpReceiveBufferByteCount), true)
	ep.SocketOptions().SetSendBufferSize(int64(self.settings.UdpSendBufferByteCount), true)

	if laddr != nil {
		if tcpipErr := ep.Bind(*laddr); tcpipErr != nil {
			ep.Close()
			return nil, fmt.Errorf("Could not bind udp endpoint err=%s", tcpipErr)
		}
	}

	conn := gonet.NewUDPConn(wq, ep)

	if raddr != nil {
		if tcpipErr := ep.Connect(*raddr); tcpipErr != nil {
			conn.Close()
			return nil, fmt.Errorf("Could not connect udp endpoint err=%s", tcpipErr)
		}
	}

	return conn, nil
}

// safe to call from multiple goroutines
func (self *Tun) DialContext(ctx context.Context, network string, address string) (net.Conn, error) {
	return raceTunDialContext(
		ctx,
		self.ctx,
		network,
		address,
		self.settings.DialRace,
		self.settings.DialRaceTimeout,
		self.settings.DialTimeout,
		self.dialContext,
	)
}

type tunDialResult struct {
	conn net.Conn
	err  error
}

func raceTunDialContext(
	ctx context.Context,
	tunCtx context.Context,
	network string,
	address string,
	dialRace int,
	dialRaceTimeout time.Duration,
	dialTimeout time.Duration,
	dialContext DialContextFunction,
) (net.Conn, error) {
	raceCtx, raceCancel := context.WithCancel(ctx)
	defer raceCancel()

	// One absolute deadline bounds the whole staggered race. The former shape
	// waited DialRaceTimeout after every launch and then waited
	// DialTimeout-DialRaceTimeout again; with the default two attempts a
	// nominal 30s dial could take 32s, and larger race counts grew without
	// bound.
	overallTimer := time.NewTimer(max(time.Duration(0), dialTimeout))
	defer overallTimer.Stop()

	attemptCount := max(1, dialRace)
	results := make(chan tunDialResult)
	launched := 0
	completed := 0
	var lastErr error
	launch := func() {
		launched++
		go HandleError(func() {
			conn, err := dialContext(raceCtx, network, address)
			select {
			case results <- tunDialResult{conn: conn, err: err}:
			case <-raceCtx.Done():
				if conn != nil {
					conn.Close()
				}
			}
		})
	}

	launch()
	if dialRaceTimeout <= 0 {
		for launched < attemptCount {
			launch()
		}
	}

	var staggerTimer *time.Timer
	var staggerC <-chan time.Time
	scheduleStagger := func() {
		if launched >= attemptCount || dialRaceTimeout <= 0 {
			staggerC = nil
			return
		}
		if staggerTimer == nil {
			staggerTimer = time.NewTimer(dialRaceTimeout)
		} else {
			staggerTimer.Reset(dialRaceTimeout)
		}
		staggerC = staggerTimer.C
	}
	stopStagger := func() {
		if staggerTimer != nil {
			staggerTimer.Stop()
		}
		staggerC = nil
	}
	defer stopStagger()
	scheduleStagger()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tunCtx.Done():
			return nil, tunCtx.Err()
		case <-raceCtx.Done():
			return nil, raceCtx.Err()
		case <-overallTimer.C:
			return nil, os.ErrDeadlineExceeded
		case <-staggerC:
			launch()
			scheduleStagger()
		case result := <-results:
			if result.err == nil && result.conn != nil {
				// The result channel is deliberately unbuffered. Exactly one
				// successful connection transfers ownership to this caller;
				// after return, raceCancel makes every other successful dial
				// close instead of leaving it queued with no receiver.
				return result.conn, nil
			}
			if result.conn != nil {
				result.conn.Close()
			}
			if result.err != nil {
				lastErr = result.err
			} else {
				lastErr = errors.New("dial returned no connection")
			}
			completed++
			if completed == attemptCount {
				return nil, lastErr
			}

			// A definitive failure is a stronger signal than the stagger:
			// replace it immediately rather than adding avoidable latency.
			if launched < attemptCount {
				stopStagger()
				launch()
				scheduleStagger()
			}
		}
	}
}

// safe to call from multiple goroutines
func (self *Tun) dialContext(ctx context.Context, network string, address string) (net.Conn, error) {
	dialCtx := self.dialCtx(ctx)

	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, err
	}

	var addrs []netip.Addr
	if addr, err := netip.ParseAddr(host); err == nil {
		// address is ip:port
		addrs = append(addrs, addr)
	} else {
		// resolve ips using doh, local

		resolvedAddrs := self.DohCache().Query(dialCtx, "A", host)
		if self.log.V(1).Enabled() {
			self.log.Infof("[tun]query doh (%s) found %v\n", host, resolvedAddrs)
		}
		for _, addr := range resolvedAddrs {
			addrs = append(addrs, addr)
		}
	}

	if len(addrs) == 0 {
		return nil, fmt.Errorf("Could not resolve %s", address)
	}

	addr := addrs[mathrand.Intn(len(addrs))]

	// var returnErr error
	// for _, addr := range addrs {
	addrPort := netip.AddrPortFrom(addr, uint16(port))

	switch network {
	case "tcp", "tcp4", "tcp6":
		fa, pn := self.convertToFullAddr(addrPort)
		conn, err := gonet.DialContextTCP(dialCtx, self.stack, fa, pn)
		if err == nil {
			if self.log.V(1).Enabled() {
				self.log.Infof("[tun]tcp connect (%s)->%s success\n", host, addrPort)
			}
			return conn, nil
		}
		if self.log.V(1).Enabled() {
			self.log.Infof("[tun]tcp connect (%s)->%s err = %s\n", host, addrPort, err)
		}
		return nil, err
	case "udp", "udp4", "udp6":
		fa, pn := self.convertToFullAddr(addrPort)
		conn, err := self.dialUdp(nil, &fa, pn)
		if err == nil {
			if self.log.V(1).Enabled() {
				self.log.Infof("[tun]udp connect (%s)->%s success\n", host, addrPort)
			}
			return conn, nil
		}
		if self.log.V(1).Enabled() {
			self.log.Infof("[tun]tcp connect (%s)->%s err = %s\n", host, addrPort, err)
		}
		return nil, err
	default:
		return nil, fmt.Errorf("Unsupported network %s", network)
	}
	// }

	// return nil, returnErr
}

func (self *Tun) Dial(network, address string) (net.Conn, error) {
	return self.DialContext(context.Background(), network, address)
}

func (self *Tun) Close() error {
	self.closeOnce.Do(func() {
		self.cancel()
		// Cancel and join resolver HTTP requests/dials before destroying the
		// stack, so net/http cannot install a late h2/TLS connection after the
		// idle pool was closed.
		self.dohResolver.Load().Close()
		self.stack.RemoveNIC(self.nicId)
		// ep.Close() drains and DecRefs any packets still queued in the endpoint.
		self.ep.Close()
		self.nicIdAllocator.ReturnNicId(self.nicId)
		for _, addr := range self.localAddresses {
			if addr.Is4() {
				self.localIpv4AddressAllocator.ReturnAddr(addr)
			}
		}
		// destroy this Tun's stack so its endpoints and background goroutines are released.
		// Do NOT stack.Wait() here: Close() can run under the device stateLock during a
		// reconfigure (SetDestination), and Wait() blocks until every stack goroutine halts —
		// one stuck goroutine would wedge the device, and DNS, until restart. The stack's
		// goroutines exit asynchronously after Close().
		self.stack.Close()
	})
	return nil
}

// Stats returns the gVisor stack statistics for this Tun's private stack.
func (self *Tun) Stats() tcpip.Stats {
	return self.stack.Stats()
}
