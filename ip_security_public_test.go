package connect

// The public-destination rule (ip_security_public.go) against the IANA
// special-purpose registries: every block's first and last address, the
// addresses just outside it, the globally reachable exceptions and their
// edges, the documentation decision, malformed addresses, and the synthetic
// speed exception. The expectations are literals, independent of the tables.

import (
	"context"
	"net"
	"net/netip"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

func TestIsPublicUnicastSpecialPurposeRegistry(t *testing.T) {
	nonPublic := []string{
		// 0.0.0.0/8
		"0.0.0.0", "0.0.0.1", "0.255.255.255",
		// 100.64.0.0/10: carrier-grade nat, tailscale peers, MagicDNS
		"100.64.0.0", "100.101.102.103", "100.100.100.100", "100.127.255.255",
		// 192.0.0.0/24 around its two anycast exceptions
		"192.0.0.0", "192.0.0.8", "192.0.0.11", "192.0.0.170", "192.0.0.171", "192.0.0.255",
		// 6a44 relay anycast
		"192.88.99.2",
		// 198.18.0.0/15
		"198.18.0.0", "198.18.0.1", "198.19.255.255",
		// 240.0.0.0/4 and the limited broadcast
		"240.0.0.0", "250.1.2.3", "255.255.255.254", "255.255.255.255",
		// the blocks that were already refused
		"10.0.0.5", "172.16.0.1", "172.31.255.255", "192.168.1.1", "127.0.0.1",
		"169.254.169.254", "224.0.0.1", "239.255.255.255",
		// their mapped forms
		"::ffff:100.64.0.1", "::ffff:198.18.0.1", "::ffff:255.255.255.255", "::ffff:0.0.0.1",

		// 100:0:0:1::/64, the dummy prefix
		"100:0:0:1::", "100:0:0:1:ffff:ffff:ffff:ffff",
		// 2001::/23 outside its exceptions: teredo, benchmarking, the
		// deprecated orchid, and the unassigned rest
		"2001::", "2001:0:4136:e378:8000:63bf:3fff:fdd2", "2001:1::", "2001:1::4",
		"2001:2::", "2001:2:0:ffff::1", "2001:10::1", "2001:1f:ffff::1",
		"2001:4:111::1", "2001:4:113::1", "2001:40::1", "2001:1ff:ffff:ffff:ffff:ffff:ffff:ffff",
		// 3fff::/20, documentation
		"3fff::", "3fff:fff:ffff:ffff:ffff:ffff:ffff:ffff",
		// 5f00::/16, srv6 sids
		"5f00::", "5f00:ffff:ffff:ffff:ffff:ffff:ffff:ffff",
		// the blocks that were already refused
		"::", "::1", "::10.0.0.5", "64:ff9b::a00:5", "64:ff9b:1::1", "100::1",
		"2001:db8::1", "2002::1", "3ffe::1", "fc00::1", "fd12:3456::1", "fe80::1",
		"fec0::1", "ff02::1",
	}
	public := []string{
		// the ipv4 anycast exceptions
		"192.0.0.9", "192.0.0.10",
		// just outside the new ipv4 blocks
		"1.0.0.0", "100.63.255.255", "100.128.0.0", "192.0.1.0", "192.88.99.1",
		"192.88.99.3", "198.17.255.255", "198.20.0.0", "223.255.255.255",
		// the documentation blocks stay public (ip_security_public.go)
		"192.0.2.1", "198.51.100.1", "203.0.113.1",
		// ordinary public destinations
		"8.8.8.8", "1.1.1.1", "::ffff:8.8.8.8",
		// the globally reachable blocks inside 2001::/23
		"2001:1::1", "2001:1::2", "2001:1::3",
		"2001:3::", "2001:3:ffff:ffff:ffff:ffff:ffff:ffff",
		"2001:4:112::", "2001:4:112:ffff:ffff:ffff:ffff:ffff",
		"2001:20::", "2001:2f:ffff:ffff:ffff:ffff:ffff:ffff",
		"2001:30::", "2001:3f:ffff:ffff:ffff:ffff:ffff:ffff",
		// just outside the new ipv6 blocks
		"100:0:0:2::", "2001:200::", "3fff:1000::", "3ffd:ffff::1",
		"5eff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", "5f01::",
		// as112 and ordinary public destinations
		"2620:4f:8000::1", "2606:4700:4700::1111", "2001:4860:4860::8888", "2a01:4f8:c17:1234::10",
	}
	for _, literal := range nonPublic {
		ip := net.ParseIP(literal)
		if ip == nil {
			t.Fatalf("bad literal %s", literal)
		}
		if isPublicUnicast(ip) {
			t.Errorf("isPublicUnicast(%s) = true, want false", literal)
		}
	}
	for _, literal := range public {
		ip := net.ParseIP(literal)
		if ip == nil {
			t.Fatalf("bad literal %s", literal)
		}
		if !isPublicUnicast(ip) {
			t.Errorf("isPublicUnicast(%s) = false, want true", literal)
		}
		if !isPublicUnicast(ip.To16()) {
			t.Errorf("isPublicUnicast(16-byte %s) = false, want true", literal)
		}
	}
}

// A destination that is not 4 or 16 bytes is never public: the rule fails
// closed rather than treating an unparsed address as the internet.
func TestIsPublicUnicastMalformedIsNotPublic(t *testing.T) {
	for _, ip := range []net.IP{nil, {}, {8, 8, 8}, {8, 8, 8, 8, 8}, make(net.IP, 15)} {
		if isPublicUnicast(ip) {
			t.Errorf("isPublicUnicast(%v) = true, want false", []byte(ip))
		}
	}
	if isPublicUnicastAddr(netip.Addr{}) {
		t.Error("isPublicUnicastAddr(zero) = true, want false")
	}
	if isPublicUnicastAddr(netip.MustParseAddr("2606:4700:4700::1111%eth0")) {
		t.Error("a zoned address is public, want not")
	}
}

// The tables are the registries: every exception lies inside a non-public
// block, and no two blocks of a family overlap.
func TestPublicRuleTablesAreConsistent(t *testing.T) {
	for _, tables := range []struct {
		nonPublic  []netip.Prefix
		exceptions []netip.Prefix
	}{
		{nonPublicIpv4Prefixes, publicIpv4Exceptions},
		{nonPublicIpv6Prefixes, publicIpv6Exceptions},
	} {
		for i, a := range tables.nonPublic {
			if a != a.Masked() {
				t.Errorf("%s is not masked", a)
			}
			for _, b := range tables.nonPublic[i+1:] {
				if a.Overlaps(b) {
					t.Errorf("%s overlaps %s", a, b)
				}
			}
		}
		for _, exception := range tables.exceptions {
			if !prefixesContain(tables.nonPublic, exception.Addr()) {
				t.Errorf("exception %s is not inside a non-public block", exception)
			}
		}
	}
}

// testConsumerEgressPolicies are both roles' views of a consumer's outbound
// packet: the client policy's egress, and the provider policy's ingress (it
// runs Reverse of the client policy), presented as egress.
func testConsumerEgressPolicies(t *testing.T, ctx context.Context, cidrs ...string) map[string]SecurityPolicy {
	networks, _ := newTestProviderLocalNetworks(t, newTestProviderLocalRead(t, cidrs...))
	return map[string]SecurityPolicy{
		"client":   DefaultSecurityPolicy(ctx),
		"provider": Reverse(newTestProviderSecurityPolicy(ctx, networks, DefaultSecurityPolicyStatsCollector())),
	}
}

// Under every relationship but a same-network one, the new non-public blocks
// are incidents in both roles. The client role has no provider-local guard,
// so this is the address class alone.
func TestPolicyRefusesNewNonPublicBlocksInBothRoles(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for role, policy := range testConsumerEgressPolicies(t, ctx) {
		for _, literal := range []string{"100.101.102.103", "100.100.100.100", "192.0.0.1", "240.0.0.1", "5f00::1", "3fff::1", "2001:2::1", "100:0:0:1::1"} {
			version := 6
			if net.ParseIP(literal).To4() != nil {
				version = 4
			}
			path := testEgressPath(version, literal)
			path.Protocol = IpProtocolUdp
			path.DestinationPort = 53
			path.Syn = false
			if r, _ := policy.InspectEgress(protocol.ProvideMode_Public, path, nil); r != SecurityPolicyResultIncident {
				t.Errorf("%s: udp %s = %s, want incident", role, literal, r)
			}
			if r, _ := policy.InspectEgress(protocol.ProvideMode_Network, path, nil); r != SecurityPolicyResultAllow {
				t.Errorf("%s: network udp %s = %s, want allow", role, literal, r)
			}
		}
	}
}

// The synthetic speed server keeps working: tcp to the benchmarking blocks
// passes the public rule in both roles and is judged by the rest of the
// pipeline, while any other protocol there is an incident. The nat never
// dials these flows (TestSyntheticSpeedRangeIsNeverDialed).
func TestPolicyAdmitsOnlyTcpToSyntheticSpeedServer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for role, policy := range testConsumerEgressPolicies(t, ctx, "2a01:4f8:c17:1234::10/64") {
		for i, literal := range []string{"198.18.0.1", "198.19.255.254", "2001:2::1", "::ffff:198.18.0.1"} {
			version := 6
			if net.ParseIP(literal).To4() != nil && literal[0] != ':' {
				version = 4
			}
			tcp := testEgressPath(version, literal)
			tcp.DestinationPort = 80
			tcp.SourcePort = 30000 + i
			if r, _ := policy.InspectEgress(protocol.ProvideMode_Public, tcp, nil); r != SecurityPolicyResultAllow {
				t.Errorf("%s: tcp syn to the synthetic server %s = %s, want allow", role, literal, r)
			}
			tcp.Syn = false
			if r, _ := policy.InspectEgress(protocol.ProvideMode_Public, tcp, []byte("GET /download/1000 HTTP/1.1\r\nHost: speed\r\n\r\n")); r != SecurityPolicyResultAllow {
				t.Errorf("%s: an http request to the synthetic server %s = %s, want allow", role, literal, r)
			}
			udp := testEgressPath(version, literal)
			udp.Protocol = IpProtocolUdp
			udp.DestinationPort = 443
			udp.Syn = false
			if r, _ := policy.InspectEgress(protocol.ProvideMode_Public, udp, nil); r != SecurityPolicyResultIncident {
				t.Errorf("%s: udp to %s = %s, want incident", role, literal, r)
			}
		}
	}
}

// The public rule never allocates.
func TestIsPublicUnicastZeroAlloc(t *testing.T) {
	ips := []net.IP{
		net.ParseIP("8.8.8.8").To4(),
		net.ParseIP("100.101.102.103"),
		net.ParseIP("192.0.0.9"),
		net.ParseIP("2606:4700:4700::1111"),
		net.ParseIP("2001:3::1"),
		net.ParseIP("5f00::1"),
		nil,
	}
	if allocs := testing.AllocsPerRun(1000, func() {
		for _, ip := range ips {
			isPublicUnicast(ip)
		}
	}); allocs != 0 {
		t.Fatalf("isPublicUnicast allocates %.1f per run, want 0", allocs)
	}
}
