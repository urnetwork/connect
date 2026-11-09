package connect

// The public-destination rule. Under every relationship but a same-network
// one, an egress destination must be public unicast: globally reachable per
// the "Globally Reachable" column of the IANA IPv4 and IPv6 Special-Purpose
// Address Registries (checked 2026-11-09), and not one of the IPv6 forms that
// embed or tunnel to an IPv4 address. An IPv4-mapped IPv6 address
// (::ffff:a.b.c.d) is judged as the IPv4 address it carries, so a v6 packet
// cannot reach private v4 space by wrapping the address. See
// docs/IP_SECURITY.md §2.2 and IPV6.md C4.
//
// The registries are tables here, not predicates, so each block names its
// source and the rules generation pin (ip_provider_diagnostics_test.go)
// covers them as hand-maintained tables: a change to either table raises
// SecurityPolicyRulesGeneration.
//
// Deliberately public although the registry marks them not globally
// reachable: the IPv4 documentation blocks 192.0.2.0/24, 198.51.100.0/24 and
// 203.0.113.0/24. No network routes them, so a public relationship reaches
// nothing through them except a network that numbers itself from them, and
// in the provider role such a network is on one of the provider's links,
// which the provider-local guard refuses (ip_security_local.go). Android's
// escape-mode tunnel numbers itself from 192.0.2.0/24, and the tests use
// these blocks as their public destinations, as CODESTYLE.md requires. The
// IPv6 documentation blocks are not public: 2001:db8::/32 never was, and no
// test uses 3fff::/20 as a public destination.
//
// Deliberately non-public although the registry marks it globally reachable:
// the NAT64 well-known prefix 64:ff9b::/96, which embeds an IPv4 address a
// translator would reach (possibly a private one).
//
// The benchmarking blocks 198.18.0.0/15 and 2001:2::/48 are not public, which
// also keeps a provider host's local "fake-ip" proxies (Clash, sing-box and
// Surge map hostnames into 198.18.0.0/15) out of reach. TCP to them is the
// one exception the policy makes (isSyntheticSpeedDestination): every NAT
// terminates it at the in-memory synthetic speed server and never dials it
// (ip_synthetic_speed.go).

import (
	"net"
	"net/netip"
)

// nonPublicIpv4Prefixes are the IPv4 blocks that are not globally reachable,
// and multicast.
var nonPublicIpv4Prefixes = []netip.Prefix{
	// "this network" (RFC 791), with the unspecified address
	netip.MustParsePrefix("0.0.0.0/8"),
	// private-use (RFC 1918)
	netip.MustParsePrefix("10.0.0.0/8"),
	// shared address space (RFC 6598): carrier-grade nat, tailscale peers and
	// MagicDNS 100.100.100.100
	netip.MustParsePrefix("100.64.0.0/10"),
	// loopback
	netip.MustParsePrefix("127.0.0.0/8"),
	// link local (RFC 3927), with the cloud metadata endpoint
	netip.MustParsePrefix("169.254.0.0/16"),
	// private-use (RFC 1918)
	netip.MustParsePrefix("172.16.0.0/12"),
	// ietf protocol assignments (RFC 6890): the 464XLAT service continuity
	// prefix, the dummy address and nat64 discovery; see the exceptions
	netip.MustParsePrefix("192.0.0.0/24"),
	// 6a44 relay anycast (RFC 6751)
	netip.MustParsePrefix("192.88.99.2/32"),
	// private-use (RFC 1918)
	netip.MustParsePrefix("192.168.0.0/16"),
	// benchmarking (RFC 2544)
	netip.MustParsePrefix("198.18.0.0/15"),
	// multicast
	netip.MustParsePrefix("224.0.0.0/4"),
	// reserved (RFC 1112), with the limited broadcast 255.255.255.255
	netip.MustParsePrefix("240.0.0.0/4"),
}

// publicIpv4Exceptions are the globally reachable blocks inside
// nonPublicIpv4Prefixes.
var publicIpv4Exceptions = []netip.Prefix{
	// port control protocol anycast (RFC 7723), provider-local in the provider
	// role (ip_security_local.go)
	netip.MustParsePrefix("192.0.0.9/32"),
	// traversal using relays around nat anycast (RFC 8155)
	netip.MustParsePrefix("192.0.0.10/32"),
}

// nonPublicIpv6Prefixes are the IPv6 blocks that are not globally reachable,
// that embed an IPv4 address, or that are deprecated or historical, and
// multicast.
var nonPublicIpv6Prefixes = []netip.Prefix{
	// unspecified ::, loopback ::1 and the deprecated ipv4-compatible
	// ::a.b.c.d. ::ffff:0:0/96 never reaches the table: it is judged as ipv4.
	netip.MustParsePrefix("::/96"),
	// nat64 well-known prefix (RFC 6052): embeds an ipv4 address
	netip.MustParsePrefix("64:ff9b::/96"),
	// nat64 local-use prefix (RFC 8215)
	netip.MustParsePrefix("64:ff9b:1::/48"),
	// discard-only (RFC 6666)
	netip.MustParsePrefix("100::/64"),
	// dummy prefix (RFC 9780)
	netip.MustParsePrefix("100:0:0:1::/64"),
	// ietf protocol assignments (RFC 2928), with teredo 2001::/32 (embeds an
	// ipv4 address), benchmarking 2001:2::/48 and the deprecated orchid
	// 2001:10::/28; see the exceptions
	netip.MustParsePrefix("2001::/23"),
	// documentation (RFC 3849)
	netip.MustParsePrefix("2001:db8::/32"),
	// 6to4 (RFC 3056): embeds an ipv4 address
	netip.MustParsePrefix("2002::/16"),
	// the historical 6bone
	netip.MustParsePrefix("3ffe::/16"),
	// documentation (RFC 9637)
	netip.MustParsePrefix("3fff::/20"),
	// segment routing sids (RFC 9602)
	netip.MustParsePrefix("5f00::/16"),
	// unique-local (RFC 4193)
	netip.MustParsePrefix("fc00::/7"),
	// link-local
	netip.MustParsePrefix("fe80::/10"),
	// the deprecated site-local
	netip.MustParsePrefix("fec0::/10"),
	// multicast
	netip.MustParsePrefix("ff00::/8"),
}

// publicIpv6Exceptions are the globally reachable blocks inside
// nonPublicIpv6Prefixes (all inside 2001::/23).
var publicIpv6Exceptions = []netip.Prefix{
	// port control protocol anycast (RFC 7723), provider-local in the provider
	// role (ip_security_local.go)
	netip.MustParsePrefix("2001:1::1/128"),
	// traversal using relays around nat anycast (RFC 8155)
	netip.MustParsePrefix("2001:1::2/128"),
	// dns-sd service registration protocol anycast (RFC 9665)
	netip.MustParsePrefix("2001:1::3/128"),
	// automatic multicast tunneling (RFC 7450)
	netip.MustParsePrefix("2001:3::/32"),
	// as112-v6 (RFC 7535)
	netip.MustParsePrefix("2001:4:112::/48"),
	// orchidv2 (RFC 7343)
	netip.MustParsePrefix("2001:20::/28"),
	// drone remote id protocol entity tags (RFC 9374)
	netip.MustParsePrefix("2001:30::/28"),
}

// isPublicUnicast reports whether an egress destination is on the public
// internet. A malformed address (neither 4 nor 16 bytes) is never public.
func isPublicUnicast(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	return isPublicUnicastAddr(addr)
}

// isPublicUnicastAddr is isPublicUnicast for a netip address. It never
// allocates.
func isPublicUnicastAddr(addr netip.Addr) bool {
	addr = addr.Unmap()
	if addr.Is4() {
		return !prefixesContain(nonPublicIpv4Prefixes, addr) || prefixesContain(publicIpv4Exceptions, addr)
	}
	if !addr.Is6() || addr.Zone() != "" {
		return false
	}
	return !prefixesContain(nonPublicIpv6Prefixes, addr) || prefixesContain(publicIpv6Exceptions, addr)
}

func prefixesContain(prefixes []netip.Prefix, addr netip.Addr) bool {
	for _, prefix := range prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// isSyntheticSpeedDestination reports whether a packet goes to the synthetic
// speed server: TCP to a benchmarking address. The NAT terminates these flows
// in memory and never dials them (ip_synthetic_speed.go), so the public rule
// admits them although the benchmarking blocks are not public.
func isSyntheticSpeedDestination(ipPath *IpPath) bool {
	return ipPath.Protocol == IpProtocolTcp && isSyntheticSpeedIp(ipPath.DestinationIp)
}
