// Explicit family policy must survive the shared 1280-byte packet/link mtu.
// These tests inspect real constructors and use only in-process stack state.
package connect

import (
	"net/netip"
	"syscall"
	"testing"

	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// Checks the link, allocated addresses and installed routes together so a
// cached capability bit alone cannot hide a partially enabled family.
func assertTunAddressFamilySnapshot(t *testing.T, tun *Tun, mtu int, ipv6Enabled bool) {
	t.Helper()
	if got := tun.ep.MTU(); got != uint32(mtu) {
		t.Fatalf("link mtu = %d, want %d", got, mtu)
	}
	if got := tun.Ipv6Enabled(); got != ipv6Enabled {
		t.Fatalf("TUN_IPV6_POLICY_IGNORED: mtu=%d ipv6 enabled=%t want=%t", mtu, got, ipv6Enabled)
	}
	wantCount := 1
	if ipv6Enabled {
		wantCount = 2
	}
	addresses := tun.LocalAddresses()
	if len(addresses) != wantCount || !addresses[0].Is4() {
		t.Fatalf("mtu=%d ipv6=%t: addresses = %v", mtu, ipv6Enabled, addresses)
	}
	if ipv6Enabled && !addresses[1].Is6() {
		t.Fatalf("ipv6-enabled tun lacks its ipv6 address: %v", addresses)
	}
	// gVisor installs its protocol broadcast endpoint when ipv4 is enabled.
	// It is not an allocated tunnel identity or an ipv6 capability.
	broadcastAddress, ok := netip.AddrFromSlice(header.IPv4Broadcast.AsSlice())
	if !ok {
		t.Fatal("invalid protocol broadcast address")
	}
	expectedAddresses := map[netip.Addr]bool{broadcastAddress: true}
	for _, address := range addresses {
		if expectedAddresses[address] {
			t.Fatalf("TUN_UNEXPECTED_OWNED_ADDRESS: %s", address)
		}
		expectedAddresses[address] = true
	}
	stackAddresses := tun.stack.AllAddresses()
	if len(stackAddresses) != 1 || len(stackAddresses[tun.nicId]) != len(expectedAddresses) {
		t.Fatalf("mtu=%d ipv6=%t: installed addresses = %v", mtu, ipv6Enabled, stackAddresses)
	}
	assignedAddresses := map[netip.Addr]bool{}
	for _, protocolAddress := range stackAddresses[tun.nicId] {
		address, ok := netip.AddrFromSlice(protocolAddress.AddressWithPrefix.Address.AsSlice())
		if !ok {
			t.Fatalf("invalid installed address: %v", protocolAddress)
		}
		if address.Is4() && protocolAddress.Protocol != header.IPv4ProtocolNumber || address.Is6() && protocolAddress.Protocol != header.IPv6ProtocolNumber {
			t.Fatalf("installed address uses the wrong protocol: %v", protocolAddress)
		}
		if !expectedAddresses[address] || assignedAddresses[address] || protocolAddress.AddressWithPrefix.PrefixLen != address.BitLen() {
			t.Fatalf("TUN_UNEXPECTED_INSTALLED_ADDRESS: %v", protocolAddress)
		}
		assignedAddresses[address] = true
	}
	for address := range expectedAddresses {
		if !assignedAddresses[address] {
			t.Fatalf("expected address %s is absent from the stack: %v", address, stackAddresses)
		}
	}
	routes := tun.stack.GetRouteTable()
	if len(routes) != wantCount || routes[0].Destination != header.IPv4EmptySubnet || routes[0].NIC != tun.nicId {
		t.Fatalf("mtu=%d ipv6=%t: routes = %v", mtu, ipv6Enabled, routes)
	}
	if ipv6Enabled && (routes[1].Destination != header.IPv6EmptySubnet || routes[1].NIC != tun.nicId) {
		t.Fatalf("ipv6-enabled tun lacks its ipv6 default route: %v", routes)
	}
}

// The zero policy retains dual stack at the minimum, and a lower custom mtu
// still refuses ipv6 regardless of whether the explicit policy is set.
func TestTunDisableIpv6ConstructorSnapshot(t *testing.T) {
	for _, test := range []struct {
		mtu         int
		disableIpv6 bool
		wantIpv6    bool
	}{
		{mtu: 1280, wantIpv6: true},
		{mtu: 1280, disableIpv6: true},
		{mtu: 1500, wantIpv6: true},
		{mtu: 1500, disableIpv6: true},
		{mtu: 1279},
		{mtu: 1279, disableIpv6: true},
	} {
		settings := DefaultTunSettings()
		settings.Log = NewNoopLogger()
		settings.Mtu = test.mtu
		settings.DisableIpv6 = test.disableIpv6
		tun, err := CreateTun(t.Context(), settings)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tun.Close() })
		assertTunAddressFamilySnapshot(t, tun, test.mtu, test.wantIpv6)
		if err := tun.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// Every public dial and injection path uses the constructor's family policy;
// ipv4 remains usable at the same 1280-byte link mtu.
func TestTunDisableIpv6RejectsUnsupportedTraffic(t *testing.T) {
	settings := DefaultTunSettings()
	settings.Log = NewNoopLogger()
	settings.Mtu = 1280
	settings.DisableIpv6 = true
	tun, err := CreateTun(t.Context(), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()
	assertTunAddressFamilySnapshot(t, tun, 1280, false)
	for _, test := range []struct {
		network string
		address string
	}{
		{network: "tcp6", address: "[2001:db8::25]:443"},
		{network: "tcp", address: "[2001:db8::25]:443"},
		{network: "udp6", address: "[2001:db8::25]:443"},
		{network: "udp", address: "[2001:db8::25]:443"},
		{network: "tcp6", address: "peer.example:443"},
		{network: "udp6", address: "peer.example:443"},
	} {
		conn, err := tun.DialContext(t.Context(), test.network, test.address)
		if conn != nil {
			_ = conn.Close()
			t.Fatalf("%s %s returned an unsupported connection", test.network, test.address)
		}
		if err != syscall.EAFNOSUPPORT {
			t.Fatalf("%s %s error = %v, want %v", test.network, test.address, err, syscall.EAFNOSUPPORT)
		}
	}
	for _, network := range []string{"tcp", "tcp6"} {
		conn, err := tun.DialResolvedContext(t.Context(), network, "peer.example:443", []netip.Addr{netip.MustParseAddr("2001:db8::25")})
		if conn != nil {
			_ = conn.Close()
			t.Fatalf("%s resolved dial returned an unsupported connection", network)
		}
		if err != syscall.EAFNOSUPPORT {
			t.Fatalf("%s resolved dial error = %v, want %v", network, err, syscall.EAFNOSUPPORT)
		}
	}
	if n, err := tun.Write([]byte{0x60}); n != 0 || err != syscall.EAFNOSUPPORT {
		t.Fatalf("ipv6 write = %d, %v; want 0, %v", n, err, syscall.EAFNOSUPPORT)
	}
	if n, err := tun.WriteBatch([][]byte{{0x60}, {0x60}}); n != 0 || err != nil {
		t.Fatalf("ipv6 batch = %d, %v; want unsupported packets skipped", n, err)
	}
	conn, err := tun.DialContext(t.Context(), "udp4", "192.0.2.25:443")
	if err != nil {
		t.Fatalf("ipv4 datagram dial failed: %v", err)
	}
	_ = conn.Close()
}

// Two owners with identical mtus may carry different families. Closing
// either owner must leave the surviving owner's routes and policy intact.
func TestTunDisableIpv6IsInstancePolicy(t *testing.T) {
	for _, closeIpv6First := range []bool{false, true} {
		newTun := func(disableIpv6 bool) *Tun {
			settings := DefaultTunSettings()
			settings.Log = NewNoopLogger()
			settings.DisableIpv6 = disableIpv6
			tun, err := CreateTun(t.Context(), settings)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = tun.Close() })
			return tun
		}
		ipv4Tun := newTun(true)
		dualStackTun := newTun(false)
		assertTunAddressFamilySnapshot(t, ipv4Tun, 1280, false)
		assertTunAddressFamilySnapshot(t, dualStackTun, 1280, true)
		closed, survivor := ipv4Tun, dualStackTun
		wantIpv6 := true
		if closeIpv6First {
			closed, survivor = dualStackTun, ipv4Tun
			wantIpv6 = false
		}
		if err := closed.Close(); err != nil {
			t.Fatal(err)
		}
		assertTunAddressFamilySnapshot(t, survivor, 1280, wantIpv6)
		n, err := survivor.Write([]byte{0x60})
		if wantIpv6 && (n != 1 || err != nil) {
			t.Fatalf("closing ipv4 owner changed sibling ipv6 admission: %d, %v", n, err)
		}
		if !wantIpv6 && (n != 0 || err != syscall.EAFNOSUPPORT) {
			t.Fatalf("closing dual-stack owner changed sibling ipv6 refusal: %d, %v", n, err)
		}
		if err := survivor.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
