package connect

// The root cause of "provider egress reaches the provider host and LAN"
// (security review, 2026-11-09). The provider (exit) role runs
// Reverse(client policy), whose only destination rule outside a same-network
// relationship was the address class: it did not know the provider host's
// own addresses, its on-link prefixes, or IPv4 ranges such as 100.64.0.0/10
// that net.IP.IsPrivate omits. The CFAA port policy then passes 80, 443,
// 8080, DNS and every user port to DPI, which passes plaintext and TLS, so a
// public consumer reached the provider's own global IPv6 address, its router
// at <prefix>::1, the rest of its /64, its public IPv4 address, and
// carrier-grade NAT or tailscale peers through the provider.
//
// This test drives the provider role's policy through each entry the
// provider can take (the public reversed API, the borrowed sender path
// RemoteUserNatProvider.ClientReceive calls, and the policy's group path)
// for every relationship but a same-network one, unspecified included, on a
// host with a global IPv6 /64 and a public IPv4 address: tcp, udp dns and
// icmp echo, to the host, its router, its /64, its IPv4 address in both
// forms, carrier-grade nat and tailscale's MagicDNS. It asserts results only,
// so it runs unchanged against the code before the fix.

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

func TestProviderPolicyRefusesProviderHostAndLink(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// the provider host: a global IPv6 address on its /64, a public IPv4 address
	policy := newRootCauseProviderPolicy(t, ctx, []string{"2a01:4f8:c17:1234::10/64", "203.0.113.10/32"})

	httpGet := []byte("GET / HTTP/1.1\r\nHost: router\r\n\r\n")
	redisPing := []byte("PING\r\n")
	dnsQuery := []byte{0x12, 0x34, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x06, 'r', 'o', 'u', 't', 'e', 'r', 0x00, 0x00, 0x01, 0x00, 0x01}
	type destination struct {
		what     string
		ip       string
		protocol IpProtocol
		port     int
		payload  []byte
	}
	destinations := []destination{}
	for _, service := range []struct {
		protocol IpProtocol
		port     int
		payload  []byte
	}{
		{IpProtocolTcp, 443, tlsClientHello()},
		{IpProtocolTcp, 80, httpGet},
		{IpProtocolTcp, 8080, httpGet},
		// the docker api and redis
		{IpProtocolTcp, 2375, httpGet},
		{IpProtocolTcp, 6379, redisPing},
		{IpProtocolUdp, 53, dnsQuery},
		// an echo request: icmp has no port
		{IpProtocolIcmp, 0, nil},
	} {
		for _, host := range []struct {
			what string
			ip   string
		}{
			{"the host's own ipv6", "2a01:4f8:c17:1234::10"},
			{"the router at <prefix>::1", "2a01:4f8:c17:1234::1"},
			{"another host in the /64", "2a01:4f8:c17:1234::beef"},
			{"the host's own public ipv4", "203.0.113.10"},
			{"the host's own public ipv4, v4-mapped", "::ffff:203.0.113.10"},
			{"a 100.64.0.0/10 address", "100.101.102.103"},
			{"tailscale MagicDNS", "100.100.100.100"},
		} {
			destinations = append(destinations, destination{
				what:     host.what,
				ip:       host.ip,
				protocol: service.protocol,
				port:     service.port,
				payload:  service.payload,
			})
		}
	}

	sourcePort := 40000
	path := func(d destination) *IpPath {
		sourcePort += 1
		ip := net.ParseIP(d.ip)
		version := 6
		sourceIp := net.ParseIP("fd00::9")
		// a v4-mapped destination stays in its v6 packet
		if ip4 := ip.To4(); ip4 != nil && !strings.HasPrefix(d.ip, "::ffff:") {
			ip = ip4
			version = 4
			sourceIp = net.ParseIP("10.0.0.9").To4()
		}
		return &IpPath{
			Version:         version,
			Protocol:        d.protocol,
			SourceIp:        sourceIp,
			SourcePort:      sourcePort,
			DestinationIp:   ip,
			DestinationPort: d.port,
		}
	}
	inner := policy.(*reverseSecurityPolicy).policy.(*securityPolicy)
	sender := NewId()
	check := func(mode protocol.ProvideMode, want SecurityPolicyResult) {
		for _, d := range destinations {
			name := fmt.Sprintf("%s %s %s %s/%d", mode, d.what, d.ip, d.protocol, d.port)
			if r, err := policy.InspectIngress(mode, path(d), d.payload); err != nil || r != want {
				t.Errorf("%s: InspectIngress = %s (%v), want %s", name, r, err, want)
			}
			if r, err := inspectAndRefreshIngressForSenderBorrowed(policy, sender, mode, *path(d), d.payload); err != nil || r != want {
				t.Errorf("%s: provider receive path = %s (%v), want %s", name, r, err, want)
			}
			if r, _, err := inner.inspectAndRefreshEgressGroupDetailedBorrowed(mode, []IpPath{*path(d)}, [][]byte{d.payload}); err != nil || r != want {
				t.Errorf("%s: group path = %s (%v), want %s", name, r, err, want)
			}
		}
	}

	for _, mode := range []protocol.ProvideMode{
		// unspecified is treated as public
		protocol.ProvideMode_None,
		protocol.ProvideMode_Public,
		protocol.ProvideMode_FriendsAndFamily,
		protocol.ProvideMode_Stream,
		protocol.ProvideMode_PublicStream,
	} {
		check(mode, SecurityPolicyResultIncident)
	}
	// a user's own devices may still reach their own LAN
	check(protocol.ProvideMode_Network, SecurityPolicyResultAllow)
}
