package connect

// The provider-local guard (ip_security_local.go): what the snapshot counts
// as local, how it is read and refreshed, what happens before the first read,
// the observed addresses, and the per-packet cost. Every test injects the
// address read and the refresh loop's clock: none reads this host's
// interfaces or waits on real time.

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// testProviderLocalRead is an injected interface read whose addresses a test
// changes between refreshes.
type testProviderLocalRead struct {
	stateLock sync.Mutex
	addresses []net.Addr
	err       error
	readCount int
}

func newTestProviderLocalRead(t *testing.T, cidrs ...string) *testProviderLocalRead {
	read := &testProviderLocalRead{}
	read.set(t, cidrs...)
	return read
}

// set replaces the addresses the next read returns, each an address with its
// interface prefix length.
func (self *testProviderLocalRead) set(t *testing.T, cidrs ...string) {
	t.Helper()
	addresses := []net.Addr{}
	for _, cidr := range cidrs {
		ip, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatal(err)
		}
		ipNet.IP = ip
		addresses = append(addresses, ipNet)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addresses = addresses
	self.err = nil
}

func (self *testProviderLocalRead) fail(err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.err = err
}

func (self *testProviderLocalRead) read() ([]net.Addr, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.readCount += 1
	if self.err != nil {
		return nil, self.err
	}
	return append([]net.Addr(nil), self.addresses...), nil
}

func (self *testProviderLocalRead) count() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.readCount
}

// testProviderLocalClock is the refresh loop's clock. Each wait the loop
// starts is handed to the test, which fires it, so the loop and the test move
// in lockstep: when the loop's next wait arrives, the read before it has
// published.
type testProviderLocalClock struct {
	waits chan testProviderLocalWait
	done  chan struct{}
}

type testProviderLocalWait struct {
	duration time.Duration
	fire     chan time.Time
}

func newTestProviderLocalClock(t *testing.T) *testProviderLocalClock {
	clock := &testProviderLocalClock{
		waits: make(chan testProviderLocalWait),
		done:  make(chan struct{}),
	}
	t.Cleanup(func() {
		close(clock.done)
	})
	return clock
}

func (self *testProviderLocalClock) after(duration time.Duration) <-chan time.Time {
	fire := make(chan time.Time, 1)
	select {
	case self.waits <- testProviderLocalWait{duration: duration, fire: fire}:
	case <-self.done:
	}
	return fire
}

// next is the loop's next wait. The timeout only bounds a hang.
func (self *testProviderLocalClock) next(t *testing.T) testProviderLocalWait {
	t.Helper()
	select {
	case wait := <-self.waits:
		return wait
	case <-time.After(10 * time.Second):
		t.Fatal("the refresh loop never started its next wait")
		return testProviderLocalWait{}
	}
}

func testProviderLocalNetworksSettings() *providerLocalNetworksSettings {
	return &providerLocalNetworksSettings{
		RefreshInterval:         time.Minute,
		NetworkChangeDelay:      time.Second,
		MaxObservedAddressCount: 16,
	}
}

// newTestProviderLocalNetworks is a snapshot over an injected read and clock.
// Its refresh loop ends with the test.
func newTestProviderLocalNetworks(t *testing.T, read *testProviderLocalRead) (*providerLocalNetworks, *testProviderLocalClock) {
	clock := newTestProviderLocalClock(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return newProviderLocalNetworks(ctx, testProviderLocalNetworksSettings(), read.read, clock.after), clock
}

// newTestProviderSecurityPolicy is the provider role's policy, exactly as
// DefaultProviderSecurityPolicyWithStats builds it, over injected networks.
func newTestProviderSecurityPolicy(ctx context.Context, networks *providerLocalNetworks, stats *SecurityPolicyStatsCollector) SecurityPolicy {
	return newProviderSecurityPolicy(
		ctx,
		DefaultCfaaSecurityPolicySettings(),
		DefaultDmcaSecurityPolicySettings(),
		DefaultWebStandardSettings(),
		DefaultProviderLocalSecurityPolicySettings(),
		networks,
		stats,
	)
}

// newRootCauseProviderPolicy is the provider role's policy on a host with the
// given interface addresses (ip_security_provider_local_root_cause_test.go).
func newRootCauseProviderPolicy(t *testing.T, ctx context.Context, cidrs []string) SecurityPolicy {
	networks, _ := newTestProviderLocalNetworks(t, newTestProviderLocalRead(t, cidrs...))
	return newTestProviderSecurityPolicy(ctx, networks, DefaultSecurityPolicyStatsCollector())
}

func testProviderLocalGuard(ctx context.Context, networks *providerLocalNetworks, ipv6PrefixBits int) *providerLocalGuard {
	return newProviderLocalGuard(ctx, &ProviderLocalSecurityPolicySettings{Ipv6PrefixBits: ipv6PrefixBits}, networks)
}

func verdictName(verdict providerLocalVerdict) string {
	switch verdict {
	case providerLocalNo:
		return "no"
	case providerLocalYes:
		return "yes"
	case providerLocalUnknown:
		return "unknown"
	default:
		return "invalid"
	}
}

func assertProviderLocal(t *testing.T, guard *providerLocalGuard, literal string, want providerLocalVerdict) {
	t.Helper()
	if got := guard.inspect(net.ParseIP(literal)); got != want {
		t.Errorf("local(%s) = %s, want %s", literal, verdictName(got), verdictName(want))
	}
}

// What counts as local: each public interface address and its interface
// prefix, every IPv6 one widened to the guard's width even from a /128, a
// wider IPv6 interface prefix kept as reported, the IPv4-mapped form of a
// local IPv4 address, and nothing from a non-public interface address.
func TestProviderLocalSnapshotCoversHostAndLinks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	read := newTestProviderLocalRead(t,
		// slaac on the provider's /64
		"2a01:4f8:c17:1234::10/64",
		// a dhcpv6 /128 in another delegation
		"2a02:8070:aa00:1::7/128",
		// a public ipv4 address on a /29
		"198.51.100.20/29",
		// a public ipv4 address on a /32 (a vps)
		"203.0.113.10/32",
		// an interface prefix wider than the guard's width, as reported
		"2a03:2000::5/40",
		// non-public: a lan, cgnat, a ula, link-local, tailscale
		"192.168.1.20/24",
		"100.101.102.103/10",
		"fd7a:115c:a1e0::5/48",
		"fe80::1/64",
	)
	networks, _ := newTestProviderLocalNetworks(t, read)
	guard := testProviderLocalGuard(ctx, networks, 56)

	for _, c := range []struct {
		literal string
		want    providerLocalVerdict
	}{
		// the host's own ipv6, its router and another host on its /64
		{"2a01:4f8:c17:1234::10", providerLocalYes},
		{"2a01:4f8:c17:1234::1", providerLocalYes},
		{"2a01:4f8:c17:1234::beef", providerLocalYes},
		// the household's other subnets in the /56
		{"2a01:4f8:c17:12ab::5", providerLocalYes},
		{"2a01:4f8:c17:1200::", providerLocalYes},
		{"2a01:4f8:c17:12ff:ffff:ffff:ffff:ffff", providerLocalYes},
		// just outside the /56
		{"2a01:4f8:c17:11ff:ffff:ffff:ffff:ffff", providerLocalNo},
		{"2a01:4f8:c17:1300::", providerLocalNo},
		// the dhcpv6 /128 still covers its /56
		{"2a02:8070:aa00:1::7", providerLocalYes},
		{"2a02:8070:aa00:ff::1", providerLocalYes},
		{"2a02:8070:aa01::1", providerLocalNo},
		// the /40 interface prefix (2a03:2000::/40) is kept as reported
		{"2a03:2000::5", providerLocalYes},
		{"2a03:2000:ff:ffff::1", providerLocalYes},
		{"2a03:2000:100::1", providerLocalNo},
		{"2a03:2001::1", providerLocalNo},
		// the ipv4 /29 and its neighbors
		{"198.51.100.16", providerLocalYes},
		{"198.51.100.20", providerLocalYes},
		{"198.51.100.23", providerLocalYes},
		{"198.51.100.15", providerLocalNo},
		{"198.51.100.24", providerLocalNo},
		// the vps /32
		{"203.0.113.10", providerLocalYes},
		{"203.0.113.9", providerLocalNo},
		{"203.0.113.11", providerLocalNo},
		// the mapped form of a local ipv4 address is local too
		{"::ffff:203.0.113.10", providerLocalYes},
		{"::ffff:198.51.100.17", providerLocalYes},
		{"::ffff:203.0.113.11", providerLocalNo},
		// ordinary public destinations
		{"2001:4860:4860::8888", providerLocalNo},
		{"8.8.8.8", providerLocalNo},
	} {
		assertProviderLocal(t, guard, c.literal, c.want)
	}

	snapshot := networks.snapshot.Load()
	if snapshot == nil {
		t.Fatal("no snapshot after the first guard")
	}
	// the non-public interface addresses contribute nothing
	if len(snapshot.ipv4Ranges) != 2 || len(snapshot.ipv6Subnets) != 2 || len(snapshot.ipv6Prefixes) != 1 {
		t.Fatalf("snapshot = %+v, want two ipv4 ranges, two ipv6 subnets and one wider ipv6 prefix", snapshot)
	}

	// the width is the policy's: /64 keeps the household's other subnets public
	guard64 := testProviderLocalGuard(ctx, networks, 64)
	assertProviderLocal(t, guard64, "2a01:4f8:c17:1234::beef", providerLocalYes)
	assertProviderLocal(t, guard64, "2a01:4f8:c17:12ab::5", providerLocalNo)
	// a wider interface prefix is kept at any width
	assertProviderLocal(t, guard64, "2a03:2000:ff:ffff::1", providerLocalYes)
	assertProviderLocal(t, guard64, "2a03:2000:100::1", providerLocalNo)
}

// The width setting: the default is /56, and values outside 1 to 64 mean 64.
func TestProviderLocalIpv6PrefixBitsSetting(t *testing.T) {
	if bits := DefaultProviderLocalSecurityPolicySettings().Ipv6PrefixBits; bits != 56 {
		t.Fatalf("default Ipv6PrefixBits = %d, want 56", bits)
	}
	for _, c := range []struct {
		setting int
		want    int
	}{
		{56, 56},
		{48, 48},
		{64, 64},
		{1, 1},
		{0, 64},
		{-8, 64},
		{65, 64},
		{128, 64},
	} {
		settings := &ProviderLocalSecurityPolicySettings{Ipv6PrefixBits: c.setting}
		if got := settings.effectiveIpv6PrefixBits(); got != c.want {
			t.Errorf("effectiveIpv6PrefixBits(%d) = %d, want %d", c.setting, got, c.want)
		}
	}
}

// How an interface address's prefix is read: the reported mask, a v4 mask in
// its 16-byte form, no mask (the address alone), and a non-canonical mask
// (its leading ones, the widest prefix it could mean).
func TestProviderLocalAddressOfMasks(t *testing.T) {
	for _, c := range []struct {
		name    string
		address net.Addr
		addr    string
		bits    int
	}{
		{"ipv4 /24", &net.IPNet{IP: net.ParseIP("198.51.100.20").To4(), Mask: net.CIDRMask(24, 32)}, "198.51.100.20", 24},
		{"ipv4 in 16 bytes, mask in 16 bytes", &net.IPNet{IP: net.ParseIP("198.51.100.20"), Mask: net.CIDRMask(120, 128)}, "198.51.100.20", 24},
		{"ipv4 without a mask", &net.IPAddr{IP: net.ParseIP("198.51.100.20")}, "198.51.100.20", 32},
		{"ipv4 with an empty mask", &net.IPNet{IP: net.ParseIP("198.51.100.20").To4()}, "198.51.100.20", 32},
		{"ipv4 non-canonical mask", &net.IPNet{IP: net.ParseIP("198.51.100.20").To4(), Mask: net.IPv4Mask(255, 255, 0, 255)}, "198.51.100.20", 16},
		{"ipv4 /0", &net.IPNet{IP: net.ParseIP("198.51.100.20").To4(), Mask: net.CIDRMask(0, 32)}, "198.51.100.20", 0},
		{"ipv6 /64", &net.IPNet{IP: net.ParseIP("2a01:4f8:c17:1234::10"), Mask: net.CIDRMask(64, 128)}, "2a01:4f8:c17:1234::10", 64},
		{"ipv6 /128", &net.IPNet{IP: net.ParseIP("2a01:4f8:c17:1234::10"), Mask: net.CIDRMask(128, 128)}, "2a01:4f8:c17:1234::10", 128},
		{"ipv6 without a mask", &net.IPAddr{IP: net.ParseIP("2a01:4f8:c17:1234::10")}, "2a01:4f8:c17:1234::10", 128},
		{"ipv6 with a v4-sized mask", &net.IPNet{IP: net.ParseIP("2a01:4f8:c17:1234::10"), Mask: net.CIDRMask(24, 32)}, "2a01:4f8:c17:1234::10", 128},
	} {
		address, ok := providerLocalAddressOf(c.address)
		if !ok {
			t.Errorf("%s: not read", c.name)
			continue
		}
		if address.addr != netip.MustParseAddr(c.addr) || address.bits != c.bits {
			t.Errorf("%s: read %s/%d, want %s/%d", c.name, address.addr, address.bits, c.addr, c.bits)
		}
	}
	for _, address := range []net.Addr{
		&net.IPNet{IP: net.IP{1, 2, 3}, Mask: net.CIDRMask(24, 32)},
		&net.UnixAddr{Name: "/tmp/socket", Net: "unix"},
	} {
		if _, ok := providerLocalAddressOf(address); ok {
			t.Errorf("%v: read, want skipped", address)
		}
	}
}

// Refresh: an address added or removed is honored after the next read, and
// not before. The loop's interval wait is fired by hand, and the loop's next
// wait is the barrier that says the read published.
func TestProviderLocalRefreshHonorsAddedAndRemovedAddresses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	read := newTestProviderLocalRead(t, "2a01:4f8:c17:1234::10/64", "203.0.113.10/32")
	networks, clock := newTestProviderLocalNetworks(t, read)
	guard := testProviderLocalGuard(ctx, networks, 56)
	if read.count() != 1 {
		t.Fatalf("reads after the first guard = %d, want 1 (before the policy is returned)", read.count())
	}

	wait := clock.next(t)
	if wait.duration != time.Minute {
		t.Fatalf("the loop waits %s, want the refresh interval", wait.duration)
	}
	assertProviderLocal(t, guard, "2a01:4f8:c17:1234::10", providerLocalYes)
	assertProviderLocal(t, guard, "203.0.113.10", providerLocalYes)
	assertProviderLocal(t, guard, "2a02:8070:aa00:1::7", providerLocalNo)
	assertProviderLocal(t, guard, "198.51.100.20", providerLocalNo)

	// the host moves: the old addresses go, new ones come
	read.set(t, "2a02:8070:aa00:1::7/64", "198.51.100.20/32")
	// nothing changes until the loop reads
	assertProviderLocal(t, guard, "2a01:4f8:c17:1234::10", providerLocalYes)
	assertProviderLocal(t, guard, "2a02:8070:aa00:1::7", providerLocalNo)

	wait.fire <- time.Time{}
	clock.next(t)
	if read.count() != 2 {
		t.Fatalf("reads after the interval = %d, want 2", read.count())
	}
	assertProviderLocal(t, guard, "2a01:4f8:c17:1234::10", providerLocalNo)
	assertProviderLocal(t, guard, "203.0.113.10", providerLocalNo)
	assertProviderLocal(t, guard, "2a02:8070:aa00:1::7", providerLocalYes)
	assertProviderLocal(t, guard, "198.51.100.20", providerLocalYes)
}

// A network change reads again after the settle delay, through the host's
// NetworkChanged hook, without waiting for the interval.
func TestProviderLocalRefreshOnNetworkChange(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	read := newTestProviderLocalRead(t, "2a01:4f8:c17:1234::10/64")
	networks, clock := newTestProviderLocalNetworks(t, read)
	guard := testProviderLocalGuard(ctx, networks, 56)
	// the loop is waiting on its interval, with its change listener registered
	clock.next(t)

	read.set(t, "2a02:8070:aa00:1::7/64")
	NetworkChanged()
	settle := clock.next(t)
	if settle.duration != time.Second {
		t.Fatalf("after a network change the loop waits %s, want the settle delay", settle.duration)
	}
	// a burst of changes during the delay is covered by the one read
	NetworkChanged()
	NetworkChanged()
	assertProviderLocal(t, guard, "2a02:8070:aa00:1::7", providerLocalNo)
	settle.fire <- time.Time{}
	next := clock.next(t)
	if next.duration != time.Minute {
		t.Fatalf("after the change's read the loop waits %s, want the refresh interval (the burst coalesced)", next.duration)
	}
	if read.count() != 2 {
		t.Fatalf("reads = %d, want 2: the first guard's and the change's", read.count())
	}
	assertProviderLocal(t, guard, "2a01:4f8:c17:1234::10", providerLocalNo)
	assertProviderLocal(t, guard, "2a02:8070:aa00:1::7", providerLocalYes)
}

// Before the first read succeeds the guard fails closed: every otherwise
// public destination is a drop with its own reason, not an incident. The
// loop retries on its interval, and the first good read ends it.
func TestProviderLocalFailsClosedBeforeFirstRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	read := newTestProviderLocalRead(t)
	read.fail(errors.New("netlink refused"))
	networks, clock := newTestProviderLocalNetworks(t, read)
	stats := DefaultSecurityPolicyStatsCollector()
	policy := newTestProviderSecurityPolicy(ctx, networks, stats)
	wait := clock.next(t)
	if networks.snapshot.Load() != nil {
		t.Fatal("a snapshot exists after a failed read")
	}

	public := testEgressPath(4, "198.51.100.7")
	public.DestinationPort = 8080
	if r, err := policy.InspectIngress(protocol.ProvideMode_Public, public, nil); err != nil || r != SecurityPolicyResultDrop {
		t.Fatalf("before the first read: %s, %v; want drop", r, err)
	}
	if count := stats.Reasons(false)[SecurityPolicyReasonProviderLocalUnknown][newSecurityDestinationPort(public)]; count != 1 {
		t.Fatalf("provider-local-unknown count = %d, want 1; reasons = %v", count, stats.Reasons(false))
	}
	// the same-network relationship keeps its bypass, and non-public is still an incident
	if r, _ := policy.InspectIngress(protocol.ProvideMode_Network, public, nil); r != SecurityPolicyResultAllow {
		t.Fatalf("network before the first read: %s, want allow", r)
	}
	if r, _ := policy.InspectIngress(protocol.ProvideMode_Public, testEgressPath(4, "10.0.0.5"), nil); r != SecurityPolicyResultIncident {
		t.Fatalf("a private destination before the first read: %s, want incident", r)
	}
	// observed addresses alone do not end the fail-closed state
	networks.setObservedAddresses([]netip.Addr{netip.MustParseAddr("203.0.113.10")})
	if networks.snapshot.Load() != nil {
		t.Fatal("an observed address published a snapshot before the first read")
	}

	read.set(t, "2a01:4f8:c17:1234::10/64")
	wait.fire <- time.Time{}
	clock.next(t)
	other := testEgressPath(4, "198.51.100.8")
	other.DestinationPort = 8080
	if r, _ := policy.InspectIngress(protocol.ProvideMode_Public, other, nil); r != SecurityPolicyResultAllow {
		t.Fatalf("a public destination after the first good read: %s, want allow", r)
	}
	// and the observed address kept before the read now counts
	if r, _ := policy.InspectIngress(protocol.ProvideMode_Public, testEgressPath(4, "203.0.113.10"), nil); r != SecurityPolicyResultIncident {
		t.Fatalf("the observed address after the first good read: %s, want incident", r)
	}
}

// A failed read after a good one keeps the last snapshot.
func TestProviderLocalFailedReadKeepsLastSnapshot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	read := newTestProviderLocalRead(t, "2a01:4f8:c17:1234::10/64")
	networks, clock := newTestProviderLocalNetworks(t, read)
	guard := testProviderLocalGuard(ctx, networks, 56)
	wait := clock.next(t)

	read.fail(errors.New("interface vanished"))
	wait.fire <- time.Time{}
	clock.next(t)
	if read.count() != 2 {
		t.Fatalf("reads = %d, want 2", read.count())
	}
	assertProviderLocal(t, guard, "2a01:4f8:c17:1234::10", providerLocalYes)
	assertProviderLocal(t, guard, "198.51.100.7", providerLocalNo)
}

// Observed addresses: an IPv4 one is local as itself, an IPv6 one as the
// guard's width around it; a non-public one is ignored, a mapped one is its
// IPv4 address, duplicates count once, and at most the configured count are
// kept, in order.
func TestProviderLocalObservedAddresses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	read := newTestProviderLocalRead(t, "192.168.1.20/24")
	networks, _ := newTestProviderLocalNetworks(t, read)
	guard := testProviderLocalGuard(ctx, networks, 56)
	assertProviderLocal(t, guard, "203.0.113.77", providerLocalNo)

	networks.setObservedAddresses([]netip.Addr{
		netip.MustParseAddr("203.0.113.77"),
		netip.MustParseAddr("2a01:4f8:c17:5678::99"),
		netip.MustParseAddr("10.0.0.1"),
		netip.MustParseAddr("fe80::1"),
		netip.MustParseAddr("100.64.0.1"),
		netip.MustParseAddr("::ffff:198.51.100.5"),
		netip.MustParseAddr("203.0.113.77"),
		{},
	})
	if want := []netip.Addr{
		netip.MustParseAddr("203.0.113.77"),
		netip.MustParseAddr("2a01:4f8:c17:5678::99"),
		netip.MustParseAddr("198.51.100.5"),
	}; !slices.Equal(networks.observedAddresses, want) {
		t.Fatalf("observed = %v, want %v", networks.observedAddresses, want)
	}
	assertProviderLocal(t, guard, "203.0.113.77", providerLocalYes)
	assertProviderLocal(t, guard, "203.0.113.78", providerLocalNo)
	assertProviderLocal(t, guard, "198.51.100.5", providerLocalYes)
	assertProviderLocal(t, guard, "::ffff:198.51.100.5", providerLocalYes)
	assertProviderLocal(t, guard, "2a01:4f8:c17:5678::99", providerLocalYes)
	assertProviderLocal(t, guard, "2a01:4f8:c17:56ff::1", providerLocalYes)
	assertProviderLocal(t, guard, "2a01:4f8:c17:5700::1", providerLocalNo)
	assertProviderLocal(t, guard, "10.0.0.1", providerLocalNo)

	// a new set replaces the old
	networks.setObservedAddresses([]netip.Addr{netip.MustParseAddr("198.51.100.6")})
	assertProviderLocal(t, guard, "203.0.113.77", providerLocalNo)
	assertProviderLocal(t, guard, "198.51.100.6", providerLocalYes)

	// the cap keeps the first 16
	many := []netip.Addr{}
	for i := range 20 {
		many = append(many, netip.AddrFrom4([4]byte{198, 51, 100, byte(100 + i)}))
	}
	networks.setObservedAddresses(many)
	if len(networks.observedAddresses) != 16 {
		t.Fatalf("observed count = %d, want the cap of 16", len(networks.observedAddresses))
	}
	assertProviderLocal(t, guard, "198.51.100.115", providerLocalYes)
	assertProviderLocal(t, guard, "198.51.100.116", providerLocalNo)

	// an empty set clears them
	networks.setObservedAddresses(nil)
	assertProviderLocal(t, guard, "198.51.100.100", providerLocalNo)
}

// The refresh loop reads only while a guard is live: with none it idles,
// with neither a read nor an interval wait. A guard is released when its
// policy's context ends, and a guard that is the only live one reads before
// it returns. synctest.Wait is the barrier for each release, which runs after
// its context ends, and for the loop settling into its wait.
func TestProviderLocalRefreshLoopFollowsGuards(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		read := newTestProviderLocalRead(t, "2a01:4f8:c17:1234::10/64")
		clock := newTestProviderLocalClock(t)
		networksCtx, networksCancel := context.WithCancel(context.Background())
		networks := newProviderLocalNetworks(networksCtx, testProviderLocalNetworksSettings(), read.read, clock.after)
		guardCount := func() int {
			networks.stateLock.Lock()
			defer networks.stateLock.Unlock()
			return networks.guardCount
		}
		// a loop that started an interval wait is parked handing it over
		intervalWaitStarted := func() bool {
			select {
			case <-clock.waits:
				return true
			default:
				return false
			}
		}

		synctest.Wait()
		if read.count() != 0 || intervalWaitStarted() {
			t.Fatalf("with no guard the loop read %d times or started an interval wait", read.count())
		}

		firstCtx, firstCancel := context.WithCancel(context.Background())
		secondCtx, secondCancel := context.WithCancel(context.Background())
		testProviderLocalGuard(firstCtx, networks, 56)
		testProviderLocalGuard(secondCtx, networks, 56)
		if wait := clock.next(t); wait.duration != time.Minute {
			t.Fatalf("the guarded loop waits %s, want the refresh interval", wait.duration)
		}
		if read.count() != 1 {
			t.Fatalf("reads = %d, want 1: the second guard reuses the first one's read", read.count())
		}

		firstCancel()
		synctest.Wait()
		if count := guardCount(); count != 1 {
			t.Fatalf("after one release: guards = %d, want 1", count)
		}
		secondCancel()
		synctest.Wait()
		if count := guardCount(); count != 0 {
			t.Fatalf("after both releases: guards = %d, want 0", count)
		}
		if intervalWaitStarted() {
			t.Fatal("the loop started an interval wait with no guard")
		}

		thirdCtx, thirdCancel := context.WithCancel(context.Background())
		guard := testProviderLocalGuard(thirdCtx, networks, 56)
		if read.count() != 2 {
			t.Fatalf("reads = %d, want 2: the only live guard reads before it returns", read.count())
		}
		clock.next(t)
		assertProviderLocal(t, guard, "2a01:4f8:c17:1234::10", providerLocalYes)
		thirdCancel()
		networksCancel()
		synctest.Wait()
	})
}

// The process snapshot's loop is not a caller's: a provider policy built
// inside a synctest bubble neither starts a goroutine there nor leaves one
// behind, so the bubble settles. When the first provider guard of a process
// started the process loop, a first provider built inside a bubble started it
// there, and synctest.Wait never returned
// (TestInitialPingObservationCausalDependencies hung on it).
func TestProcessProviderLocalLoopStaysOutOfBubbles(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		policy := DefaultProviderSecurityPolicy(ctx)
		if policy.(*reverseSecurityPolicy).policy.(*securityPolicy).providerLocal.networks != processProviderLocalNetworks {
			t.Fatal("the default provider policy does not read the process snapshot")
		}
		synctest.Wait()
		cancel()
		synctest.Wait()
	})
}

// The per-packet check never allocates: the public rule, the guard on a hit
// and a miss of both families and the mapped form, the port control anycast
// addresses, and the provider policy's refusal of a local destination.
func TestProviderLocalCheckZeroAlloc(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	read := newTestProviderLocalRead(t, "2a01:4f8:c17:1234::10/64", "198.51.100.20/29", "2a03:2000::5/40")
	networks, _ := newTestProviderLocalNetworks(t, read)
	networks.setObservedAddresses([]netip.Addr{netip.MustParseAddr("203.0.113.77")})
	guard := testProviderLocalGuard(ctx, networks, 56)

	destinations := []net.IP{
		net.ParseIP("2a01:4f8:c17:1234::1"),
		net.ParseIP("2001:4860:4860::8888"),
		net.ParseIP("198.51.100.17").To4(),
		net.ParseIP("8.8.8.8").To4(),
		net.ParseIP("::ffff:203.0.113.77"),
		net.ParseIP("100.101.102.103").To4(),
		net.ParseIP("192.0.0.9").To4(),
		net.ParseIP("::ffff:192.0.0.9"),
		net.ParseIP("2001:1::1"),
	}
	if allocs := testing.AllocsPerRun(1000, func() {
		for _, destination := range destinations {
			isPublicUnicast(destination)
			guard.inspect(destination)
		}
	}); allocs != 0 {
		t.Fatalf("public rule and guard allocate %.1f per run, want 0", allocs)
	}

	policy := newTestProviderSecurityPolicy(ctx, networks, DefaultSecurityPolicyStatsCollector())
	sender := NewId()
	local := *testEgressPath(6, "2a01:4f8:c17:1234::1")
	local.DestinationPort = 8080
	payload := []byte("GET / HTTP/1.1\r\nHost: router\r\n\r\n")
	if allocs := testing.AllocsPerRun(1000, func() {
		inspectAndRefreshIngressForSenderBorrowed(policy, sender, protocol.ProvideMode_Public, local, payload)
	}); allocs != 0 {
		t.Fatalf("refusing a local destination allocates %.1f per packet, want 0", allocs)
	}
}

// Every provider construction path installs the guard over the process-wide
// snapshot, and the client role's policies have none.
func TestProviderConstructionPathsInstallProviderLocalGuard(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	providerGuard := func(policy SecurityPolicy) *providerLocalGuard {
		t.Helper()
		reverse, ok := policy.(*reverseSecurityPolicy)
		if !ok {
			t.Fatalf("provider policy %T is not reversed", policy)
		}
		builtin, ok := reverse.policy.(*securityPolicy)
		if !ok {
			t.Fatalf("provider policy wraps %T", reverse.policy)
		}
		return builtin.providerLocal
	}
	stats := DefaultSecurityPolicyStatsCollector()
	for name, policy := range map[string]SecurityPolicy{
		"DefaultProviderSecurityPolicy":          DefaultProviderSecurityPolicy(ctx),
		"DefaultProviderSecurityPolicyWithStats": DefaultProviderSecurityPolicyWithStats(ctx, stats),
		"provider nat, unbounded":                newNatProviderSecurityPolicy(ctx, DefaultRemoteUserNatProviderSettings(), false),
		"provider nat, memory bounded":           newNatProviderSecurityPolicy(ctx, DefaultRemoteUserNatProviderSettings(), true),
	} {
		guard := providerGuard(policy)
		if guard == nil {
			t.Errorf("%s: no provider-local guard", name)
			continue
		}
		if guard.networks != processProviderLocalNetworks {
			t.Errorf("%s: the guard does not read the process-wide snapshot", name)
		}
		if guard.ipv6PrefixBits != 56 {
			t.Errorf("%s: ipv6 width %d, want the default 56", name, guard.ipv6PrefixBits)
		}
	}
	for name, policy := range map[string]SecurityPolicy{
		"DefaultSecurityPolicy": DefaultSecurityPolicy(ctx),
		"NewSecurityPolicy":     NewSecurityPolicy(ctx, DefaultCfaaSecurityPolicySettings(), DefaultDmcaSecurityPolicySettings(), DefaultWebStandardSettings(), stats),
	} {
		if policy.(*securityPolicy).providerLocal != nil {
			t.Errorf("%s: the client role has a provider-local guard", name)
		}
	}
}

// The client role is unchanged: DefaultSecurityPolicy, used directly, allows
// the public destinations the provider role refuses because they are inside
// the provider host's prefixes. The guard belongs to the provider role only.
func TestClientPolicyAllowsProviderLocalDestinations(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	read := newTestProviderLocalRead(t, "2a01:4f8:c17:1234::10/64", "203.0.113.10/32")
	networks, _ := newTestProviderLocalNetworks(t, read)
	provider := newTestProviderSecurityPolicy(ctx, networks, DefaultSecurityPolicyStatsCollector())
	client := DefaultSecurityPolicy(ctx)

	sourcePort := 41000
	for _, literal := range []string{"2a01:4f8:c17:1234::10", "2a01:4f8:c17:1234::1", "2a01:4f8:c17:12ab::5", "203.0.113.10", "::ffff:203.0.113.10"} {
		for _, service := range []struct {
			protocol IpProtocol
			port     int
			payload  []byte
		}{
			{IpProtocolTcp, 443, tlsClientHello()},
			{IpProtocolTcp, 8080, []byte("GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")},
			{IpProtocolUdp, 53, []byte{0x12, 0x34, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01}},
		} {
			version := 6
			if ip := net.ParseIP(literal); ip.To4() != nil && literal[0] != ':' {
				version = 4
			}
			path := testEgressPath(version, literal)
			path.Protocol = service.protocol
			path.DestinationPort = service.port
			path.Syn = false
			sourcePort += 1
			path.SourcePort = sourcePort
			if r, _ := client.InspectEgress(protocol.ProvideMode_Public, path, service.payload); r != SecurityPolicyResultAllow {
				t.Errorf("client %s %s/%d = %s, want allow", literal, service.protocol, service.port, r)
			}
			if r, _ := provider.InspectIngress(protocol.ProvideMode_Public, path, service.payload); r != SecurityPolicyResultIncident {
				t.Errorf("provider %s %s/%d = %s, want incident", literal, service.protocol, service.port, r)
			}
		}
	}
}

// A provider-local refusal is counted under its own reason, apart from
// not-public, on the per-packet and the group paths.
func TestSecurityPolicyReasonProviderLocal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	read := newTestProviderLocalRead(t, "2a01:4f8:c17:1234::10/64")
	networks, _ := newTestProviderLocalNetworks(t, read)
	stats := DefaultSecurityPolicyStatsCollector()
	policy := newTestProviderSecurityPolicy(ctx, networks, stats)

	local := testEgressPath(6, "2a01:4f8:c17:1234::1")
	local.DestinationPort = 8080
	if r, _ := policy.InspectIngress(protocol.ProvideMode_Public, local, nil); r != SecurityPolicyResultIncident {
		t.Fatalf("local destination = %s, want incident", r)
	}
	group := *local
	group.DestinationPort = 8081
	inner := policy.(*reverseSecurityPolicy).policy.(*securityPolicy)
	if r, decision, _ := inner.inspectAndRefreshEgressGroupDetailedBorrowed(protocol.ProvideMode_Public, []IpPath{group, group}, [][]byte{nil, nil}); r != SecurityPolicyResultIncident || decision.reason != SecurityPolicyReasonProviderLocal {
		t.Fatalf("group to a local destination = %s, %s; want incident, provider-local", r, decision.reason)
	}
	private := testEgressPath(6, "fd00::5")
	private.DestinationPort = 8082
	policy.InspectIngress(protocol.ProvideMode_Public, private, nil)

	reasons := stats.Reasons(false)
	if count := reasons[SecurityPolicyReasonProviderLocal][newSecurityDestinationPort(local)]; count != 1 {
		t.Errorf("provider-local count for 8080 = %d, want 1; reasons = %v", count, reasons)
	}
	if count := reasons[SecurityPolicyReasonProviderLocal][newSecurityDestinationPort(&group)]; count != 2 {
		t.Errorf("provider-local count for the group = %d, want 2; reasons = %v", count, reasons)
	}
	if count := reasons[SecurityPolicyReasonNotPublic][newSecurityDestinationPort(private)]; count != 1 {
		t.Errorf("not-public count = %d, want 1; reasons = %v", count, reasons)
	}
	if SecurityPolicyReasonProviderLocal.String() != "provider-local" || SecurityPolicyReasonProviderLocalUnknown.String() != "provider-local-unknown" {
		t.Errorf("reason names = %q, %q", SecurityPolicyReasonProviderLocal, SecurityPolicyReasonProviderLocalUnknown)
	}
}
