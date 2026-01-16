package connect

import (
	"net"
	"net/netip"
	"testing"

	"github.com/go-playground/assert/v2"
)

func TestIpsInNet(t *testing.T) {
	_, ipNet, err := net.ParseCIDR("192.168.1.0/30")
	assert.Equal(t, err, nil)
	ips := []net.IP{}
	for ip := range IpsInNet(ipNet) {
		ips = append(ips, ip)
	}
	assert.Equal(t, len(ips), 4)
	assert.Equal(t, ips[0].To4(), net.ParseIP("192.168.1.1").To4())
	assert.Equal(t, ips[1].To4(), net.ParseIP("192.168.1.2").To4())
	assert.Equal(t, ips[2].To4(), net.ParseIP("192.168.1.3").To4())
	assert.Equal(t, ips[3].To4(), net.ParseIP("192.168.1.4").To4())
}

func TestAddrsInPrefix(t *testing.T) {
	prefix := netip.MustParsePrefix("192.168.1.0/30")
	addrs := []netip.Addr{}
	for addr := range AddrsInPrefix(prefix) {
		addrs = append(addrs, addr)
	}
	assert.Equal(t, len(addrs), 4)
	assert.Equal(t, addrs[0], netip.MustParseAddr("192.168.1.1"))
	assert.Equal(t, addrs[1], netip.MustParseAddr("192.168.1.2"))
	assert.Equal(t, addrs[2], netip.MustParseAddr("192.168.1.3"))
	assert.Equal(t, addrs[3], netip.MustParseAddr("192.168.1.4"))
}

func TestAddrGenerator(t *testing.T) {
	prefix := netip.MustParsePrefix("169.254.0.0/16")
	g := NewAddrGenerator(prefix)
	addr, ok := g.Next()
	assert.Equal(t, ok, true)
	assert.Equal(t, addr, netip.MustParseAddr("169.254.0.1"))
	addr, ok = g.Next()
	assert.Equal(t, ok, true)
	assert.Equal(t, addr, netip.MustParseAddr("169.254.0.2"))
	addr, ok = g.Next()
	assert.Equal(t, ok, true)
	assert.Equal(t, addr, netip.MustParseAddr("169.254.0.3"))
	addr, ok = g.Next()
	assert.Equal(t, ok, true)
	assert.Equal(t, addr, netip.MustParseAddr("169.254.0.4"))
}
