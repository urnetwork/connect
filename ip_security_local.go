package connect

// The provider-local guard. A provider (exit) egresses remote clients'
// traffic from its own host, so a destination that is that host, or that sits
// on one of its links, reaches the provider's own services and LAN however
// public its address looks: the host's global IPv6 address and the rest of
// its delegated prefix, the router at <prefix>::1, a public IPv4 address bound
// to an interface and the subnet around it. In the provider role such a
// destination is not public: its packets are incidents with the reason
// provider-local. The client role (the device's own egress) has no guard, and
// a same-network relationship (ProvideMode_Network) bypasses it with the rest
// of the public rules, so a user's own devices still reach their own LAN.
//
// What is local:
//   - every public address on every interface, up or down (physical,
//     tun/utun, tailscale, docker, wireguard and the rest), with the interface
//     prefix it is configured with;
//   - around each IPv6 one, at least the prefix of
//     ProviderLocalSecurityPolicySettings.Ipv6PrefixBits (default /56, a
//     household's delegated prefix), even when the interface reports a /128
//     (DHCPv6);
//   - every address the platform observes this host connecting from
//     (SetProviderObservedAddresses): an IPv4 one as itself, an IPv6 one as
//     the IPv6 prefix above. Behind a NAT that is the router's public address,
//     which a consumer would otherwise reach by hairpin NAT, and in a
//     container it is the host's;
//   - the service anycast addresses (providerLocalAnycastAddrs), which the
//     network in front of this host answers, snapshot or not.
//
// A non-public interface address (RFC 1918, CGNAT, ULA, link-local, ...)
// contributes nothing: its link is inside a block the public rule already
// refuses for every relationship but a same-network one, and leaving it out
// keeps a bogus wide mask on such an interface from making public space local.
//
// The addresses are one snapshot per process behind an atomic pointer, since
// the host and its links are the same for every provider of the process. A
// guard that is the only live one reads them before its policy is returned,
// and the snapshot's refresh loop re-reads them while any guard is live: on
// an interval (providerLocalNetworksSettings) and after every
// NetworkChanged. With no live guard the loop idles and reads nothing. The
// process snapshot's loop starts with the package, so it belongs to no caller:
// a provider built inside a test's synctest bubble cannot capture it. The
// per-packet check loads the pointer and searches two small sorted tables; it
// never blocks, reads interfaces or allocates.
//
// Before the first read succeeds there is no snapshot, and the guard fails
// closed: every otherwise public destination is a drop with the reason
// provider-local-unknown. A drop and not an incident, because the consumer
// did nothing wrong and an incident reports it for abuse once per packet.
// Because a guard reads synchronously, a provider is in that state only while
// the platform refuses the read, which the refresh loop retries every
// interval and after every network change. A failed later read keeps the last
// snapshot.

import (
	"context"
	"encoding/binary"
	"errors"
	"math/bits"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wlynxg/anet"
)

// The width of an IPv6 subnet (RFC 4291): the local prefix around an IPv6
// address is never narrower.
const ipv6SubnetBits = 64

// The service anycast addresses: every anycast address the IANA
// special-purpose registries list for a service the network in front of the
// sender provides (checked 2026-11-09). The registries mark them globally
// reachable, so the public rule admits them (ip_security_public.go), but a
// request to one is answered by that network, and from a provider the sender
// is the provider host:
//   - port control (PCP, RFC 7723): the gateway in front of the sender, a home
//     router or an ISP's carrier-grade NAT, maps a port of its public address
//     to the sender for whoever asks;
//   - TURN (RFC 8155): the network's own TURN server allocates relays for the
//     sender, from inside that network;
//   - DNS-SD service registration (SRP, RFC 9665): the network's registrar
//     publishes services on its links for the sender.
//
// They are local wherever the provider is, with or without a snapshot.
var providerLocalAnycastAddrs = []netip.Addr{
	// port control
	netip.MustParseAddr("192.0.0.9"),
	netip.MustParseAddr("2001:1::1"),
	// turn
	netip.MustParseAddr("192.0.0.10"),
	netip.MustParseAddr("2001:1::2"),
	// dns-sd service registration
	netip.MustParseAddr("2001:1::3"),
}

// isProviderLocalAnycastAddr reports whether addr, or the IPv4 address it
// maps, is a service anycast address. It never allocates.
func isProviderLocalAnycastAddr(addr netip.Addr) bool {
	return slices.Contains(providerLocalAnycastAddrs, addr.Unmap().WithZone(""))
}

// ProviderLocalSecurityPolicySettings configures the provider-local guard of
// the provider role's policy (DefaultProviderSecurityPolicy). The client role
// has no guard.
type ProviderLocalSecurityPolicySettings struct {
	// Ipv6PrefixBits is the width, in bits, of the local prefix around each
	// of this host's IPv6 addresses and each observed IPv6 address. An
	// interface prefix wider than this is kept as the interface reports it.
	// The default, 56, covers the /56 a household is commonly delegated, so
	// the router's other subnets are local, not only the provider's own /64.
	// Values outside 1 to 64 mean 64, the narrowest width that still covers
	// a whole subnet, so a /128 (DHCPv6) never narrows the guard to the
	// address itself.
	Ipv6PrefixBits int
}

func DefaultProviderLocalSecurityPolicySettings() *ProviderLocalSecurityPolicySettings {
	return &ProviderLocalSecurityPolicySettings{
		Ipv6PrefixBits: 56,
	}
}

// The IPv6 width the guard applies, 1 to 64.
func (self *ProviderLocalSecurityPolicySettings) effectiveIpv6PrefixBits() int {
	if self.Ipv6PrefixBits < 1 || ipv6SubnetBits < self.Ipv6PrefixBits {
		return ipv6SubnetBits
	}
	return self.Ipv6PrefixBits
}

// providerLocalNetworksSettings tunes the process-wide snapshot of this
// host's addresses.
type providerLocalNetworksSettings struct {
	// RefreshInterval is how often the snapshot re-reads the interface
	// addresses while a provider guard is live. 30s: a desktop or server
	// host has nothing that reports an address change, and the read is one
	// address dump. Non-positive disables the periodic read; network changes
	// and a guard that is the only live one still read.
	RefreshInterval time.Duration
	// NetworkChangeDelay is how long after a network change the snapshot
	// re-reads, so a burst of path updates becomes one read and the new
	// link's addresses have a moment to be configured. Non-positive reads at
	// once.
	NetworkChangeDelay time.Duration
	// MaxObservedAddressCount bounds the platform-observed addresses kept
	// (SetProviderObservedAddresses); later ones are ignored. 16: a provider
	// has a few transports, each seen from one public address per family.
	// Non-positive keeps none.
	MaxObservedAddressCount int
}

func defaultProviderLocalNetworksSettings() *providerLocalNetworksSettings {
	return &providerLocalNetworksSettings{
		RefreshInterval:         30 * time.Second,
		NetworkChangeDelay:      1 * time.Second,
		MaxObservedAddressCount: 16,
	}
}

// processProviderLocalNetworks is the snapshot every provider policy of this
// process reads. Its loop runs for the life of the process, idle while no
// provider policy is live.
var processProviderLocalNetworks = newProviderLocalNetworks(
	context.Background(),
	defaultProviderLocalNetworksSettings(),
	hostProviderLocalAddresses,
	time.After,
)

// SetProviderObservedAddresses replaces the public addresses the platform
// observes this host connecting from. In the provider role each is local: an
// IPv4 address as itself, an IPv6 address as the IPv6 local prefix around
// it (default /56). An address that is not public unicast is ignored, and at
// most 16 are kept, in order. It never waits on an interface read.
func SetProviderObservedAddresses(addrs []netip.Addr) {
	processProviderLocalNetworks.setObservedAddresses(addrs)
}

// providerLocalNetworks owns a snapshot of this host's addresses. Safe for
// concurrent use; the snapshot is read without locks.
type providerLocalNetworks struct {
	settings *providerLocalNetworksSettings
	// reads every interface address
	readAddresses func() ([]net.Addr, error)
	// the refresh loop's clock
	after func(time.Duration) <-chan time.Time

	// nil until the first read succeeds
	snapshot atomic.Pointer[providerLocalSnapshot]
	// a coalesced network-change wake for the refresh loop
	networkChange chan struct{}
	// a coalesced wake for the refresh loop when a guard comes or goes
	guardChange chan struct{}

	// serializes reads, so an older read never publishes over a newer one.
	// Taken before stateLock.
	readLock sync.Mutex

	stateLock sync.Mutex
	// the last successful read; read is false until there is one
	read               bool
	interfaceAddresses []providerLocalAddress
	observedAddresses  []netip.Addr
	guardCount         int
}

// newProviderLocalNetworks starts the refresh loop, which runs until ctx ends.
// The loop belongs to the caller's goroutine context, so the process snapshot
// is built at package initialization, before any test bubble exists.
func newProviderLocalNetworks(
	ctx context.Context,
	settings *providerLocalNetworksSettings,
	readAddresses func() ([]net.Addr, error),
	after func(time.Duration) <-chan time.Time,
) *providerLocalNetworks {
	networks := &providerLocalNetworks{
		settings:      settings,
		readAddresses: readAddresses,
		after:         after,
		networkChange: make(chan struct{}, 1),
		guardChange:   make(chan struct{}, 1),
	}
	go HandleError(func() {
		networks.run(ctx)
	})
	return networks
}

// acquire registers one provider guard and returns its release, which is
// safe to call more than once. A guard that is the only live one reads the
// addresses before acquire returns, so a provider starts from a current
// snapshot unless the platform refuses the read; so does every guard while no
// read has succeeded. acquire wakes the refresh loop but never waits for it.
func (self *providerLocalNetworks) acquire() (release func()) {
	only := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.guardCount += 1
		return self.guardCount == 1
	}()
	self.wake(self.guardChange)
	if only || self.snapshot.Load() == nil {
		self.refresh()
	}
	var releaseOnce sync.Once
	return func() {
		releaseOnce.Do(func() {
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				self.guardCount -= 1
			}()
			self.wake(self.guardChange)
		})
	}
}

func (self *providerLocalNetworks) guarded() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return 0 < self.guardCount
}

// run re-reads the addresses on the refresh interval and after network
// changes while any guard is live, and idles without reading while none is,
// until ctx ends. Every wait is on ctx, a guard change, the interval or a
// network change, so the loop cannot spin. The interval wait in progress
// survives guard changes, so guards that come and go cannot postpone a read.
func (self *providerLocalNetworks) run(ctx context.Context) {
	defer AddNetworkChangeListener(self.networkChanged)()
	var refreshTimeout <-chan time.Time
	for {
		if !self.guarded() {
			// the next guard reads before it returns, and the interval starts
			// over with it
			refreshTimeout = nil
			select {
			case <-ctx.Done():
				return
			case <-self.guardChange:
			}
			continue
		}
		if refreshTimeout == nil && 0 < self.settings.RefreshInterval {
			refreshTimeout = self.after(self.settings.RefreshInterval)
		}
		select {
		case <-ctx.Done():
			return
		case <-self.guardChange:
			continue
		case <-refreshTimeout:
		case <-self.networkChange:
			if 0 < self.settings.NetworkChangeDelay {
				select {
				case <-ctx.Done():
					return
				case <-self.after(self.settings.NetworkChangeDelay):
				}
			}
			// the read below covers a change signaled during the delay
			select {
			case <-self.networkChange:
			default:
			}
		}
		refreshTimeout = nil
		self.refresh()
	}
}

// networkChanged wakes the refresh loop. It never blocks.
func (self *providerLocalNetworks) networkChanged() {
	self.wake(self.networkChange)
}

// wake signals one of the loop's coalesced wake channels without blocking.
func (self *providerLocalNetworks) wake(change chan struct{}) {
	select {
	case change <- struct{}{}:
	default:
	}
}

// refresh reads the interface addresses and publishes a new snapshot. A
// failed read keeps the last snapshot, or none.
func (self *providerLocalNetworks) refresh() {
	self.readLock.Lock()
	defer self.readLock.Unlock()

	netAddresses, err := self.readAddresses()
	if err != nil {
		if self.snapshot.Load() == nil {
			loggerOrDefault(nil).Warningf("[security]provider local addresses unreadable, refusing public destinations until a read succeeds: %s\n", err)
		} else {
			loggerOrDefault(nil).Infof("[security]provider local addresses unreadable, keeping the last read: %s\n", err)
		}
		return
	}
	interfaceAddresses := make([]providerLocalAddress, 0, len(netAddresses))
	for _, netAddress := range netAddresses {
		if address, ok := providerLocalAddressOf(netAddress); ok {
			interfaceAddresses = append(interfaceAddresses, address)
		}
	}

	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.read = true
	self.interfaceAddresses = interfaceAddresses
	self.snapshot.Store(newProviderLocalSnapshot(self.interfaceAddresses, self.observedAddresses))
}

// setObservedAddresses validates and keeps the observed addresses, then
// republishes the snapshot from the last read. Before the first read it only
// keeps them: an observed address alone must not end the fail-closed state.
func (self *providerLocalNetworks) setObservedAddresses(addrs []netip.Addr) {
	maxCount := max(0, self.settings.MaxObservedAddressCount)
	observedAddresses := make([]netip.Addr, 0, min(len(addrs), maxCount))
	for _, addr := range addrs {
		if len(observedAddresses) == maxCount {
			break
		}
		addr = addr.Unmap()
		if !isPublicUnicastAddr(addr) || slices.Contains(observedAddresses, addr) {
			continue
		}
		observedAddresses = append(observedAddresses, addr)
	}

	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if slices.Equal(self.observedAddresses, observedAddresses) {
		return
	}
	self.observedAddresses = observedAddresses
	if self.read {
		self.snapshot.Store(newProviderLocalSnapshot(self.interfaceAddresses, self.observedAddresses))
	}
}

// containsAddr reports whether addr is this host or on one of its links in
// the current snapshot, at the default provider policy's IPv6 width, or a
// service anycast address. Before the first read it reports false for all but
// the anycast addresses: a caller that must fail closed uses a guard instead.
// Not for the packet path: it allocates.
func (self *providerLocalNetworks) containsAddr(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	if isProviderLocalAnycastAddr(addr) {
		return true
	}
	snapshot := self.snapshot.Load()
	if snapshot == nil {
		return false
	}
	return snapshot.contains(net.IP(addr.Unmap().AsSlice()), DefaultProviderLocalSecurityPolicySettings().effectiveIpv6PrefixBits())
}

// hostProviderLocalAddresses reads every address of every interface, up or
// down. anet and not net, for the reason hostControlFamilyInterfaces gives:
// Android 11 refuses the netlink request net uses. One address dump serves
// every interface. When it fails (an interface that disappears between the
// dump and the interface table it is joined with fails the whole read), the
// per-interface read control_family uses skips the vanished interface.
func hostProviderLocalAddresses() ([]net.Addr, error) {
	netAddresses, err := anet.InterfaceAddrs()
	if err == nil {
		return netAddresses, nil
	}
	interfaces, interfacesErr := hostControlFamilyInterfaces()
	if interfacesErr != nil {
		return nil, errors.Join(err, interfacesErr)
	}
	netAddresses = nil
	for _, iface := range interfaces {
		netAddresses = append(netAddresses, iface.addrs...)
	}
	return netAddresses, nil
}

// providerLocalAddress is one interface address and the prefix length of its
// interface.
type providerLocalAddress struct {
	addr netip.Addr
	// the address's own length when the platform reported no mask
	bits int
}

// providerLocalAddressOf reads an interface address. A mask the platform did
// not report keeps only the address. A non-canonical mask keeps its leading
// ones, the widest prefix the mask could describe, so it never makes the
// link smaller than it is.
func providerLocalAddressOf(netAddress net.Addr) (providerLocalAddress, bool) {
	var ip net.IP
	var mask net.IPMask
	switch v := netAddress.(type) {
	case *net.IPNet:
		ip, mask = v.IP, v.Mask
	case *net.IPAddr:
		ip = v.IP
	default:
		return providerLocalAddress{}, false
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return providerLocalAddress{}, false
	}
	addr = addr.Unmap()
	if addr.Is4() && len(mask) == net.IPv6len {
		// a v4 mask in its 16-byte form
		mask = mask[12:]
	}
	prefixBits := addr.BitLen()
	if len(mask)*8 == addr.BitLen() {
		prefixBits = 0
		for _, b := range mask {
			ones := bits.LeadingZeros8(^b)
			prefixBits += ones
			if ones < 8 {
				break
			}
		}
	}
	return providerLocalAddress{addr: addr, bits: prefixBits}, true
}

// providerLocalGuard is one provider policy's view of the snapshot.
type providerLocalGuard struct {
	settings *ProviderLocalSecurityPolicySettings
	networks *providerLocalNetworks
	// the IPv6 width, 1 to 64
	ipv6PrefixBits int
}

// newProviderLocalGuard registers the guard with networks until ctx ends.
func newProviderLocalGuard(ctx context.Context, settings *ProviderLocalSecurityPolicySettings, networks *providerLocalNetworks) *providerLocalGuard {
	if settings == nil {
		settings = DefaultProviderLocalSecurityPolicySettings()
	}
	context.AfterFunc(ctx, networks.acquire())
	return &providerLocalGuard{
		settings:       settings,
		networks:       networks,
		ipv6PrefixBits: settings.effectiveIpv6PrefixBits(),
	}
}

type providerLocalVerdict int

const (
	// not this host and not on its links
	providerLocalNo providerLocalVerdict = iota
	// this host, on one of its links, an observed address or a service
	// anycast address
	providerLocalYes
	// no read of this host's addresses has succeeded yet
	providerLocalUnknown
)

// inspect judges a destination the public rule admitted. It never blocks or
// allocates.
func (self *providerLocalGuard) inspect(ip net.IP) providerLocalVerdict {
	if addr, ok := netip.AddrFromSlice(ip); ok && isProviderLocalAnycastAddr(addr) {
		return providerLocalYes
	}
	snapshot := self.networks.snapshot.Load()
	if snapshot == nil {
		return providerLocalUnknown
	}
	if snapshot.contains(ip, self.ipv6PrefixBits) {
		return providerLocalYes
	}
	return providerLocalNo
}

// providerLocalSnapshot is an immutable view of this host's addresses.
type providerLocalSnapshot struct {
	// sorted, disjoint and not adjacent
	ipv4Ranges []providerLocalIpv4Range
	// the upper 64 bits of each IPv6 anchor whose prefix is /64 or narrower
	// (every address of a /64 or longer interface prefix, every observed
	// address), sorted and distinct. The guard widens each to its IPv6 width.
	ipv6Subnets []uint64
	// the IPv6 interface prefixes wider than /64, masked
	ipv6Prefixes []providerLocalIpv6Prefix
}

type providerLocalIpv4Range struct {
	first uint32
	last  uint32
}

type providerLocalIpv6Prefix struct {
	// the upper 64 bits, masked to bits
	upper uint64
	bits  int
}

// newProviderLocalSnapshot builds the tables from the public interface
// addresses and the observed addresses.
func newProviderLocalSnapshot(interfaceAddresses []providerLocalAddress, observedAddresses []netip.Addr) *providerLocalSnapshot {
	snapshot := &providerLocalSnapshot{}
	var ipv4Ranges []providerLocalIpv4Range
	addIpv4 := func(addr netip.Addr, prefixBits int) {
		hostMask := uint32(uint64(1)<<(32-prefixBits) - 1)
		addr4 := addr.As4()
		value := binary.BigEndian.Uint32(addr4[:])
		ipv4Ranges = append(ipv4Ranges, providerLocalIpv4Range{
			first: value &^ hostMask,
			last:  value | hostMask,
		})
	}
	ipv6Upper := func(addr netip.Addr) uint64 {
		addr16 := addr.As16()
		return binary.BigEndian.Uint64(addr16[:8])
	}
	for _, address := range interfaceAddresses {
		if !isPublicUnicastAddr(address.addr) {
			continue
		}
		prefixBits := min(max(0, address.bits), address.addr.BitLen())
		if address.addr.Is4() {
			addIpv4(address.addr, prefixBits)
		} else if ipv6SubnetBits <= prefixBits {
			snapshot.ipv6Subnets = append(snapshot.ipv6Subnets, ipv6Upper(address.addr))
		} else {
			snapshot.ipv6Prefixes = append(snapshot.ipv6Prefixes, providerLocalIpv6Prefix{
				upper: ipv6Upper(address.addr) &^ (uint64(1)<<(ipv6SubnetBits-prefixBits) - 1),
				bits:  prefixBits,
			})
		}
	}
	for _, addr := range observedAddresses {
		if addr.Is4() {
			addIpv4(addr, 32)
		} else {
			snapshot.ipv6Subnets = append(snapshot.ipv6Subnets, ipv6Upper(addr))
		}
	}

	slices.SortFunc(ipv4Ranges, func(a providerLocalIpv4Range, b providerLocalIpv4Range) int {
		switch {
		case a.first < b.first:
			return -1
		case b.first < a.first:
			return 1
		default:
			return 0
		}
	})
	for _, ipv4Range := range ipv4Ranges {
		if n := len(snapshot.ipv4Ranges); 0 < n && uint64(ipv4Range.first) <= uint64(snapshot.ipv4Ranges[n-1].last)+1 {
			// overlapping or adjacent
			snapshot.ipv4Ranges[n-1].last = max(snapshot.ipv4Ranges[n-1].last, ipv4Range.last)
		} else {
			snapshot.ipv4Ranges = append(snapshot.ipv4Ranges, ipv4Range)
		}
	}
	slices.Sort(snapshot.ipv6Subnets)
	snapshot.ipv6Subnets = slices.Compact(snapshot.ipv6Subnets)
	slices.SortFunc(snapshot.ipv6Prefixes, func(a providerLocalIpv6Prefix, b providerLocalIpv6Prefix) int {
		switch {
		case a.upper < b.upper:
			return -1
		case b.upper < a.upper:
			return 1
		default:
			return a.bits - b.bits
		}
	})
	snapshot.ipv6Prefixes = slices.Compact(snapshot.ipv6Prefixes)
	return snapshot
}

// contains reports whether ip is local, with an IPv4-mapped address judged as
// its IPv4 address. ipv6PrefixBits must be 1 to 64. A malformed address is
// local: the guard fails closed.
func (self *providerLocalSnapshot) contains(ip net.IP, ipv6PrefixBits int) bool {
	if ip4 := ip.To4(); ip4 != nil {
		return self.containsIpv4(binary.BigEndian.Uint32(ip4))
	}
	if len(ip) != net.IPv6len {
		return true
	}
	return self.containsIpv6(binary.BigEndian.Uint64(ip[:8]), ipv6PrefixBits)
}

func (self *providerLocalSnapshot) containsIpv4(value uint32) bool {
	// the first range that ends at or after value
	i, j := 0, len(self.ipv4Ranges)
	for i < j {
		h := int(uint(i+j) >> 1)
		if self.ipv4Ranges[h].last < value {
			i = h + 1
		} else {
			j = h
		}
	}
	return i < len(self.ipv4Ranges) && self.ipv4Ranges[i].first <= value
}

func (self *providerLocalSnapshot) containsIpv6(upper uint64, ipv6PrefixBits int) bool {
	// the anchors are sorted, so their prefixes at any one width are too
	shift := uint(ipv6SubnetBits - ipv6PrefixBits)
	target := upper >> shift
	i, j := 0, len(self.ipv6Subnets)
	for i < j {
		h := int(uint(i+j) >> 1)
		if self.ipv6Subnets[h]>>shift < target {
			i = h + 1
		} else {
			j = h
		}
	}
	if i < len(self.ipv6Subnets) && self.ipv6Subnets[i]>>shift == target {
		return true
	}
	for _, prefix := range self.ipv6Prefixes {
		// the wider of the interface prefix and the guard's width; a shift
		// of 64 compares nothing, which is right for a /0
		prefixBits := min(prefix.bits, ipv6PrefixBits)
		if (upper^prefix.upper)>>uint(ipv6SubnetBits-prefixBits) == 0 {
			return true
		}
	}
	return false
}
