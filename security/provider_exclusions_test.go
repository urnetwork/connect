package main

import (
	"net/netip"
	"testing"

	"github.com/urnetwork/connect"
)

// Feed subtraction must never leave these ranges reachable on a provider.
// Exercise both endpoints and IPv4-mapped spellings through the pre-connect check.
func TestFeedExclusionsAndProviderDestinations(t *testing.T) {
	for _, cidr := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "198.18.0.0/15", "240.0.0.0/4", "192.0.0.9/32", "192.0.0.10/32"} {
		r := cidrRange(cidr)
		if len(subtractAll([]iprange{r}, mergeRanges(parseReserved()))) != 0 {
			t.Errorf("feed retained %s", cidr)
		}
		for _, n := range []uint32{r.lo, r.hi} {
			addr := netip.MustParseAddr(u32Str(n))
			for _, ip := range []netip.Addr{addr, netip.AddrFrom16(addr.As16())} {
				address := netip.AddrPortFrom(ip, 443).String()
				if err := connect.ProviderDialControl("tcp", address, nil); err == nil {
					t.Errorf("provider admitted %s", address)
				}
			}
		}
	}
	for _, cidr := range []string{"100:0:0:1::/64", "2001::/32", "2001:2::/48", "2001:10::/28", "2001:db8::/32", "2002::/16", "3ffe::/16", "3fff::/20", "5f00::/16", "fec0::/10"} {
		lo, hi := prefixBounds6(netip.MustParsePrefix(cidr))
		if len(subtractAll6([]ip6range{{lo: lo, hi: hi}}, mergeRanges6(parseReserved6()))) != 0 {
			t.Errorf("feed retained %s", cidr)
		}
		for _, ip := range []netip.Addr{lo, hi} {
			if err := connect.ProviderDialControl("udp", netip.AddrPortFrom(ip, 53).String(), nil); err == nil {
				t.Errorf("provider admitted %s", ip)
			}
		}
	}
}
