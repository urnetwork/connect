package connect

import (
	"context"
	"net"
	"testing"

	"github.com/urnetwork/connect/v2026/protocol"
)

// ip_ipv6_hygiene_test.go — egress address hygiene (IPV6.md C4): the v6
// prefixes that embed or tunnel to v4, the v6 blocks the special-purpose
// registry marks not globally reachable, and the IPv4-mapped form must never
// reach an egress socket under a public relationship, and a mapped address
// is judged by the v4 tables it wraps.

// testEgressPath6 is testEgressPath for a v6 destination string (which may
// be an IPv4-mapped literal).
func testEgressPath6(dstIp string) *IpPath {
	return &IpPath{
		Version:         6,
		Protocol:        IpProtocolTcp,
		SourceIp:        net.ParseIP("fd00::2"),
		SourcePort:      12345,
		DestinationIp:   net.ParseIP(dstIp),
		DestinationPort: 443,
		Syn:             true,
	}
}

// every reserved prefix, one representative address each, plus the mapped
// forms of private and public v4 addresses. The benchmarking block
// 2001:2::/48 is not here: tcp to it is the synthetic speed server
// (TestPolicyAdmitsOnlyTcpToSyntheticSpeedServer).
var testNonPublicIpv6 = []string{
	"::ffff:10.0.0.5",                      // v4-mapped RFC1918
	"::ffff:127.0.0.1",                     // v4-mapped loopback
	"::ffff:169.254.169.254",               // v4-mapped link-local (cloud metadata)
	"::10.0.0.5",                           // ipv4-compatible (deprecated)
	"2002:0a00:0005::1",                    // 6to4 carrying 10.0.0.5
	"2001:0:4136:e378:8000:63bf:3fff:fdd2", // teredo
	"64:ff9b::10.0.0.5",                    // nat64 well-known prefix
	"64:ff9b:1::10.0.0.5",                  // nat64 local-use prefix
	"100::1",                               // discard-only
	"100:0:0:1::1",                         // dummy prefix (RFC 9780)
	"2001:db8::1",                          // documentation
	"3fff::1",                              // documentation (RFC 9637)
	"2001:10::1",                           // the deprecated orchid
	"2001:1::4",                            // ietf protocol assignments outside the anycast exceptions
	"5f00::1",                              // srv6 sids (RFC 9602)
	"::ffff:100.64.0.1",                    // v4-mapped shared address space
	"fc00::1",                              // ula
	"fd12:3456::1",                         // ula
	"fe80::1",                              // link-local
	"fec0::1",                              // site-local (deprecated)
	"3ffe::1",                              // 6bone (historical)
	"ff02::1",                              // multicast
	"::1",                                  // loopback
	"::",                                   // unspecified
}

var testPublicIpv6 = []string{
	"2606:4700:4700::1111",
	"2001:4860:4860::8888",
	"::ffff:8.8.8.8", // v4-mapped public
	"2a00:1450:4001:80e::200e",
	"2001:1::1", // pcp anycast, globally reachable inside 2001::/23 and next to teredo
	"2001:3::1", // amt, globally reachable inside 2001::/23
	"2001:20::1",
	"2003::1", // adjacent to 6to4 (2002::/16) but outside it
	"64:ff9c::1",
	"101::1", // adjacent to discard-only
}

func TestIsPublicUnicastIpv6Reserved(t *testing.T) {
	for _, literal := range testNonPublicIpv6 {
		if isPublicUnicast(net.ParseIP(literal)) {
			t.Errorf("isPublicUnicast(%s) = true, want false", literal)
		}
	}
	for _, literal := range testPublicIpv6 {
		if !isPublicUnicast(net.ParseIP(literal)) {
			t.Errorf("isPublicUnicast(%s) = false, want true", literal)
		}
	}
	// the v4 answers: the predicates' blocks and the registry's other
	// non-globally-reachable blocks (ip_security_public_test.go has every edge)
	for _, literal := range []string{"10.0.0.5", "127.0.0.1", "169.254.169.254", "224.0.0.1", "0.0.0.0", "100.64.0.1", "198.18.0.1", "240.0.0.1", "255.255.255.255", "192.0.0.1"} {
		if isPublicUnicast(net.ParseIP(literal)) {
			t.Errorf("isPublicUnicast(%s) = true, want false", literal)
		}
	}
	for _, literal := range []string{"8.8.8.8", "192.0.0.9"} {
		if !isPublicUnicast(net.ParseIP(literal)) {
			t.Errorf("isPublicUnicast(%s) = false, want true", literal)
		}
	}
}

func TestPolicyAddressUnmapsIpv4Mapped(t *testing.T) {
	mapped := net.ParseIP("::ffff:203.0.113.9")
	ip, version := policyAddress(mapped, 6)
	if version != 4 || len(ip) != net.IPv4len || !ip.Equal(net.IPv4(203, 0, 113, 9)) {
		t.Fatalf("policyAddress(mapped) = %v, %d; want 203.0.113.9, 4", ip, version)
	}
	native := net.ParseIP("2606:4700:4700::1111")
	if ip, version := policyAddress(native, 6); version != 6 || !ip.Equal(native) {
		t.Fatalf("policyAddress(native v6) = %v, %d; want unchanged, 6", ip, version)
	}
	v4 := net.IPv4(203, 0, 113, 9)
	if ip, version := policyAddress(v4, 4); version != 4 || !ip.Equal(v4) {
		t.Fatalf("policyAddress(v4) = %v, %d; want unchanged, 4", ip, version)
	}

	// the v4 block table applies through the mapped form: a blocked v4 range
	// low address, wrapped, drops exactly as its v4 form does
	d := newCfaaDetector(DefaultCfaaSecurityPolicySettings())
	lo, _ := cfaaRangeAt(0)
	blocked4 := net.IPv4(byte(lo>>24), byte(lo>>16), byte(lo>>8), byte(lo))
	blockedMapped := net.ParseIP("::ffff:" + blocked4.String())
	ip, version = policyAddress(blockedMapped, 6)
	if got := d.inspect(ip, 443, IpProtocolTcp, version); got != cfaaDrop {
		t.Fatalf("blocked v4 %s through its mapped form: got %s, want drop", blocked4, cfaaVerdictName(got))
	}
}

// the public relationship rejects every reserved v6 destination and the
// mapped private forms as incidents, while a public v6 (or mapped public v4)
// destination passes the reachability rule
func TestInspectEgressIpv6ReservedIsIncident(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	policy := DefaultSecurityPolicy(ctx)

	for _, literal := range testNonPublicIpv6 {
		r, err := policy.InspectEgress(protocol.ProvideMode_Public, testEgressPath6(literal), nil)
		if err != nil {
			t.Fatalf("InspectEgress(Public, %s): unexpected error %v", literal, err)
		}
		if r != SecurityPolicyResultIncident {
			t.Errorf("InspectEgress(Public, %s) = %v, want Incident", literal, r)
		}
	}
	for _, literal := range testPublicIpv6 {
		r, err := policy.InspectEgress(protocol.ProvideMode_Public, testEgressPath6(literal), nil)
		if err != nil {
			t.Fatalf("InspectEgress(Public, %s): unexpected error %v", literal, err)
		}
		if r == SecurityPolicyResultIncident {
			t.Errorf("InspectEgress(Public, %s) = Incident, want a public destination to pass", literal)
		}
	}
	// the same-network relationship keeps its LAN bypass for v6 too
	for _, literal := range []string{"fd12:3456::1", "fe80::1", "::ffff:10.0.0.5"} {
		r, err := policy.InspectEgress(protocol.ProvideMode_Network, testEgressPath6(literal), nil)
		if err != nil {
			t.Fatalf("InspectEgress(Network, %s): unexpected error %v", literal, err)
		}
		if r != SecurityPolicyResultAllow {
			t.Errorf("InspectEgress(Network, %s) = %v, want Allow (same-network bypass)", literal, r)
		}
	}
}
