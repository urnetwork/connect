package connect

// a userspace tun device backed by the gvisor network stack.
// `Tun` exposes a packet interface on one side (`Read`/`Write`) and
// socket interfaces on the other (`DialContext`, `ListenTCP`, `ListenUDP`).
// all tun instances share a single gvisor stack, with one nic and one
// link-local ipv4 address per instance.

import (
	// "bytes"
	"context"
	// "errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	// "regexp"
	mathrand "math/rand"
	"strconv"
	"sync"
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
// const DefaultProxySequenceSize = 64
// const DefaultWriteTimeout = 15 * time.Second

func DefaultTunSettings() *TunSettings {
	return DefaultTunSettingsWithBufferSize(32)
}

func DefaultTunSettingsWithBufferSize(bufferSize int) *TunSettings {
	return &TunSettings{
		ChannelSize: bufferSize,
		// must match `DefaultMtu`. packets are written directly into the
		// receiver tap/tun interface, so this must not exceed the device
		// interface mtu.
		Mtu: 1440,

		DialRace:        4,
		DialRaceTimeout: 2 * time.Second,
		DialTimeout:     30 * time.Second,

		// this works with `ProxySequenceSize` to control packet loss during back pressure
		WriteTimeout:      5 * time.Second,
		ProxySequenceSize: bufferSize,

		// the gvisor udp endpoint buffers default to 32KiB,
		// which is too small for fast transfer.
		// gvisor clamps these to at most 4MiB.
		UdpReceiveBufferByteCount: 4 * 1024 * 1024,
		UdpSendBufferByteCount:    4 * 1024 * 1024,
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

	WriteTimeout      time.Duration
	ProxySequenceSize int

	UdpReceiveBufferByteCount int
	UdpSendBufferByteCount    int
}

var tunStack = sync.OnceValue(func() *stack.Stack {
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
			Min:     4 << 10,
			Default: 4 << 20,
			Max:     16 << 20,
		}
		s.SetTransportProtocolOption(tcp.ProtocolNumber, &opt)
	}
	{
		opt := tcpip.TCPSendBufferSizeRangeOption{
			Min:     4 << 10,
			Default: 4 << 20,
			Max:     16 << 20,
		}
		s.SetTransportProtocolOption(tcp.ProtocolNumber, &opt)
	}

	return s
})

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

var defaultLocalIpv4AddressAllocator = NewLocalIpv4AddressAllocator(
	netip.MustParsePrefix("169.254.0.0/16"),
	128,
)

type Tun struct {
	ctx    context.Context
	cancel context.CancelFunc
	log    Logger

	settings *TunSettings

	ep                        *channel.Endpoint
	stack                     *stack.Stack
	nicId                     tcpip.NICID
	nicIdAllocator            *NicIdAllocator
	localAddresses            []netip.Addr
	localIpv4AddressAllocator *LocalIpv4AddressAllocator
	receivePacket             chan []byte
	// mtu                 int
	// registeredAddresses map[netip.Addr]bool
	dohResolver *DohCache

	// serializes the endpoint queue pop and receive queue add in `WriteNotify`
	writeNotifyLock sync.Mutex

	stateLock sync.Mutex
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
	localIpv4AddressAllocator := defaultLocalIpv4AddressAllocator

	localIpv4Address, ok := localIpv4AddressAllocator.TakeAddr()
	if !ok {
		cancel()
		return nil, fmt.Errorf("No more local addresses")
	}

	nicId := nicIdAllocator.TakeNicId()

	localAddresses := []netip.Addr{
		localIpv4Address,
	}

	ep := channel.New(settings.ChannelSize, uint32(settings.Mtu), tcpip.LinkAddress(fmt.Sprintf("%x", nicId)))

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
		stack:                     tunStack(),
		nicId:                     nicId,
		nicIdAllocator:            nicIdAllocator,
		localAddresses:            localAddresses,
		localIpv4AddressAllocator: localIpv4AddressAllocator,
		receivePacket:             make(chan []byte, settings.ProxySequenceSize),
	}

	dohSettings := DefaultDohSettings()
	dohSettings.ConnectSettings.Log = tun.log
	dohSettings.RequestTimeout = 60 * time.Second
	dohSettings.TlsTimeout = 30 * time.Second
	dohSettings.DialContextSettings = &DialContextSettings{
		DialContext: tun.DialContext,
	}

	if dnsResolverSettings != nil {
		dohSettings.DnsResolverSettings = dnsResolverSettings
	}
	tun.dohResolver = NewDohCache(dohSettings)

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

	ep.AddNotify(tun)

	return tun, nil
}

func (self *Tun) DohCache() *DohCache {
	return self.dohResolver
}

func (self *Tun) Read() ([]byte, error) {
	select {
	case <-self.ctx.Done():
		return nil, fmt.Errorf("Done")
	case m, ok := <-self.receivePacket:
		if !ok {
			return nil, os.ErrClosed
		}
		return m, nil
	}
}

// reads one or more packets, blocking until at least one is available.
// fills up to `len(packets)` entries and returns the count.
// a batch read wakes the reader once per burst instead of once per packet.
func (self *Tun) ReadBatch(packets [][]byte) (int, error) {
	if len(packets) == 0 {
		return 0, nil
	}
	select {
	case <-self.ctx.Done():
		return 0, fmt.Errorf("Done")
	case m, ok := <-self.receivePacket:
		if !ok {
			return 0, os.ErrClosed
		}
		packets[0] = m
		n := 1
		for n < len(packets) {
			select {
			case m, ok := <-self.receivePacket:
				if !ok {
					return n, nil
				}
				packets[n] = m
				n += 1
			default:
				return n, nil
			}
		}
		return n, nil
	}
}

// safe to call from multiple goroutines
func (self *Tun) Write(packet []byte) (int, error) {
	// defer MessagePoolReturn(packet)

	if len(packet) == 0 {
		return 0, nil
	}

	// copy the packet
	pkb := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(packet),
	})

	switch packet[0] >> 4 {
	case 4:
		self.ep.InjectInbound(header.IPv4ProtocolNumber, pkb)
		return len(packet), nil
	default:
		return 0, syscall.EAFNOSUPPORT
	}
}

func (self *Tun) WriteNotify() {
	// the stack notifies inline from concurrent writer goroutines, which all
	// pop a shared endpoint queue. the pop and the receive queue add must be
	// one atomic step, since packets of one flow that pass through different
	// notify handlers would otherwise reorder. receivers treat each flow as
	// in order, so a reorder shows up as packet loss.
	self.writeNotifyLock.Lock()
	defer self.writeNotifyLock.Unlock()

	pkt := self.ep.Read()

	// if pkt.IsNil() {
	// 	return
	// }

	// FIXME
	view := pkt.ToView()
	// m := MessagePoolGet(view.Capacity())
	// view.Read(m)
	packet := MessagePoolCopy(view.AsSlice())
	pkt.DecRef()

	// fast path without arming a timer
	select {
	case self.receivePacket <- packet:
		return
	default:
	}

	if 0 < self.settings.WriteTimeout {
		select {
		case <-self.ctx.Done():
			MessagePoolReturn(packet)
		case self.receivePacket <- packet:
		case <-time.After(self.settings.WriteTimeout):
			// drop
			MessagePoolReturn(packet)
		}
	} else {
		select {
		case <-self.ctx.Done():
			MessagePoolReturn(packet)
		case self.receivePacket <- packet:
		}
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
	raceCtx, raceCancel := context.WithCancel(ctx)
	defer raceCancel()
	raceOut := make(chan net.Conn)
	for range self.settings.DialRace {
		go HandleError(func() {
			conn, err := self.dialContext(raceCtx, network, address)
			if err == nil {
				select {
				case <-raceCtx.Done():
					conn.Close()
				case raceOut <- conn:
				}
			}
		})
		select {
		case conn := <-raceOut:
			return conn, nil
		case <-time.After(self.settings.DialRaceTimeout):
		}
	}
	select {
	case <-raceCtx.Done():
		return nil, fmt.Errorf("Done.")
	case conn := <-raceOut:
		return conn, nil
	case <-time.After(self.settings.DialTimeout - self.settings.DialRaceTimeout):
		return nil, fmt.Errorf("Timeout.")
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

		resolvedAddrs := self.dohResolver.Query(dialCtx, "A", host)
		self.log.V(1).Infof("[tun]query doh (%s) found %v\n", host, resolvedAddrs)
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
			self.log.V(1).Infof("[tun]tcp connect (%s)->%s success\n", host, addrPort)
			return conn, nil
		}
		self.log.V(1).Infof("[tun]tcp connect (%s)->%s err = %s\n", host, addrPort, err)
		return nil, err
	case "udp", "udp4", "udp6":
		fa, pn := self.convertToFullAddr(addrPort)
		conn, err := self.dialUdp(nil, &fa, pn)
		if err == nil {
			self.log.V(1).Infof("[tun]udp connect (%s)->%s success\n", host, addrPort)
			return conn, nil
		}
		self.log.V(1).Infof("[tun]tcp connect (%s)->%s err = %s\n", host, addrPort, err)
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
	self.cancel()
	self.stack.RemoveNIC(self.nicId)
	self.ep.Close()
	// Drain any queued packets so their pool buffers can be returned. Any
	// in-flight WriteNotify will see ctx.Done() and return its own packet.
drain:
	for {
		select {
		case packet := <-self.receivePacket:
			MessagePoolReturn(packet)
		default:
			break drain
		}
	}
	self.nicIdAllocator.ReturnNicId(self.nicId)
	for _, addr := range self.localAddresses {
		if addr.Is4() {
			self.localIpv4AddressAllocator.ReturnAddr(addr)
		}
	}
	return nil
}
