package connect

import (
	"net"
	"net/netip"
	"testing"
)

func TestIpsInNet(t *testing.T) {
	_, ipNet, err := net.ParseCIDR("192.168.1.0/30")
	AssertEqual(t, err, nil)
	ips := []net.IP{}
	for ip := range IpsInNet(ipNet) {
		ips = append(ips, ip)
	}
	AssertEqual(t, len(ips), 4)
	AssertEqual(t, ips[0].To4(), net.ParseIP("192.168.1.1").To4())
	AssertEqual(t, ips[1].To4(), net.ParseIP("192.168.1.2").To4())
	AssertEqual(t, ips[2].To4(), net.ParseIP("192.168.1.3").To4())
	AssertEqual(t, ips[3].To4(), net.ParseIP("192.168.1.4").To4())
}

func TestAddrsInPrefix(t *testing.T) {
	prefix := netip.MustParsePrefix("192.168.1.0/30")
	addrs := []netip.Addr{}
	for addr := range AddrsInPrefix(prefix) {
		addrs = append(addrs, addr)
	}
	AssertEqual(t, len(addrs), 4)
	AssertEqual(t, addrs[0], netip.MustParseAddr("192.168.1.1"))
	AssertEqual(t, addrs[1], netip.MustParseAddr("192.168.1.2"))
	AssertEqual(t, addrs[2], netip.MustParseAddr("192.168.1.3"))
	AssertEqual(t, addrs[3], netip.MustParseAddr("192.168.1.4"))
}

func TestAddrGenerator(t *testing.T) {
	prefix := netip.MustParsePrefix("169.254.0.0/16")
	g := NewAddrGenerator(prefix)
	addr, ok := g.Next()
	AssertEqual(t, ok, true)
	AssertEqual(t, addr, netip.MustParseAddr("169.254.0.1"))
	addr, ok = g.Next()
	AssertEqual(t, ok, true)
	AssertEqual(t, addr, netip.MustParseAddr("169.254.0.2"))
	addr, ok = g.Next()
	AssertEqual(t, ok, true)
	AssertEqual(t, addr, netip.MustParseAddr("169.254.0.3"))
	addr, ok = g.Next()
	AssertEqual(t, ok, true)
	AssertEqual(t, addr, netip.MustParseAddr("169.254.0.4"))
}
