package connect

// Port control anycast in the provider role (ip_security_local.go,
// providerLocalAnycastAddrs). The special-purpose registries mark the port
// control anycast addresses, 192.0.0.9 and 2001:1::1, globally reachable, so
// the public rule admits them, and through generation 3 so did the provider
// role: a consumer could send a PCP MAP request through a provider, and the
// gateway in front of the provider, its home router or its ISP's
// carrier-grade NAT, would map a port of its public address to the provider
// host. These tests assert results only, so they run unchanged against the
// code before the change.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

// A PCP MAP request (RFC 6887 §7.1, §11.1): version 2, opcode MAP, a one hour
// lifetime, the client's address, then a nonce, tcp, the internal port 22
// and no suggested external port or address.
func testPcpMapRequest() []byte {
	request := make([]byte, 60)
	request[0] = 2
	request[1] = 1
	request[6], request[7] = 0x0e, 0x10
	copy(request[8:24], net.ParseIP("10.0.0.9").To16())
	copy(request[24:36], "synthetic-pc")
	request[36] = byte(IpProtocolTcp)
	request[40], request[41] = 0, 22
	return request
}

// A consumer's packets to the port control anycast addresses, PCP to port
// 5351 and anything else, are provider-local incidents on every entry the
// provider takes and for every relationship but a same-network one, in both
// families and the IPv4-mapped form, while the TURN anycast addresses beside
// them stay public.
func TestProviderPolicyRefusesPortControlAnycast(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stats := DefaultSecurityPolicyStatsCollector()
	networks, _ := newTestProviderLocalNetworks(t, newTestProviderLocalRead(t, "2a01:4f8:c17:1234::10/64", "198.51.100.20/32"))
	policy := newTestProviderSecurityPolicy(ctx, networks, stats)
	inner := policy.(*reverseSecurityPolicy).policy.(*securityPolicy)

	type destination struct {
		ip       string
		protocol IpProtocol
		port     int
		payload  []byte
	}
	anycast := []destination{}
	for _, ip := range []string{"192.0.0.9", "::ffff:192.0.0.9", "2001:1::1"} {
		anycast = append(anycast,
			destination{ip, IpProtocolUdp, 5351, testPcpMapRequest()},
			destination{ip, IpProtocolTcp, 443, tlsClientHello()},
			destination{ip, IpProtocolIcmp, 0, nil},
		)
	}
	turn := []destination{
		{"192.0.0.10", IpProtocolUdp, 3478, stunBinding()},
		{"2001:1::2", IpProtocolUdp, 3478, stunBinding()},
	}

	sourcePort := 46000
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
	sender := NewId()
	check := func(mode protocol.ProvideMode, destinations []destination, want SecurityPolicyResult) {
		for _, d := range destinations {
			name := fmt.Sprintf("%s %s %s/%d", mode, d.ip, d.protocol, d.port)
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
		check(mode, anycast, SecurityPolicyResultIncident)
		check(mode, turn, SecurityPolicyResultAllow)
	}
	// a user's own devices may still ask their own gateway for a mapping
	check(protocol.ProvideMode_Network, anycast, SecurityPolicyResultAllow)

	reasons := stats.Reasons(false)
	pcp := path(anycast[0])
	if count := reasons[SecurityPolicyReasonProviderLocal][newSecurityDestinationPort(pcp)]; count == 0 {
		t.Errorf("no provider-local count for PCP to 192.0.0.9; reasons = %v", reasons)
	}
}

// Port control anycast is local before any read of this host's addresses has
// succeeded: an incident with the reason provider-local, not the fail-closed
// drop the guard gives every other public destination then.
func TestProviderPolicyRefusesPortControlAnycastBeforeTheFirstRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	read := newTestProviderLocalRead(t)
	read.fail(errors.New("netlink refused"))
	networks, _ := newTestProviderLocalNetworks(t, read)
	stats := DefaultSecurityPolicyStatsCollector()
	policy := newTestProviderSecurityPolicy(ctx, networks, stats)
	if networks.snapshot.Load() != nil {
		t.Fatal("a snapshot exists after a failed read")
	}

	for i, literal := range []string{"192.0.0.9", "2001:1::1"} {
		version := 6
		if net.ParseIP(literal).To4() != nil {
			version = 4
		}
		pcp := testEgressPath(version, literal)
		pcp.Protocol = IpProtocolUdp
		pcp.DestinationPort = 5351
		pcp.SourcePort = 47000 + i
		pcp.Syn = false
		if r, err := policy.InspectIngress(protocol.ProvideMode_Public, pcp, testPcpMapRequest()); err != nil || r != SecurityPolicyResultIncident {
			t.Errorf("PCP to %s before the first read = %s (%v), want incident", literal, r, err)
		}
		if count := stats.Reasons(false)[SecurityPolicyReasonProviderLocal][newSecurityDestinationPort(pcp)]; count != 1 {
			t.Errorf("PCP to %s before the first read: provider-local count = %d, want 1; reasons = %v", literal, count, stats.Reasons(false))
		}
	}
}

// The client role and the public rule are unchanged: the port control
// anycast addresses are public, and the client role's own egress to them is
// judged as before.
func TestClientPolicyKeepsPortControlAnycastPublic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := DefaultSecurityPolicy(ctx)
	for i, literal := range []string{"192.0.0.9", "::ffff:192.0.0.9", "2001:1::1"} {
		if !isPublicUnicast(net.ParseIP(literal)) {
			t.Errorf("%s is not public", literal)
		}
		version := 6
		if !strings.HasPrefix(literal, "::ffff:") && net.ParseIP(literal).To4() != nil {
			version = 4
		}
		path := testEgressPath(version, literal)
		path.Syn = false
		path.SourcePort = 48000 + i
		if r, err := client.InspectEgress(protocol.ProvideMode_Public, path, tlsClientHello()); err != nil || r != SecurityPolicyResultAllow {
			t.Errorf("client tcp %s/443 = %s (%v), want allow", literal, r, err)
		}
	}
}

// A consumer's PCP MAP request to either port control anycast address never
// reaches the exit LocalUserNat at the provider's real entry point, and is
// counted as provider-local.
func TestRemoteUserNatProviderRefusesPortControlAnycastAtClientReceive(t *testing.T) {
	rig := newProviderLocalEntryRig(t, "2a01:4f8:c17:1234::10/64", "198.51.100.20/32")
	source := SourceId(NewId())

	blocked := []*IpPath{
		providerLocalEntryFlow(IpProtocolUdp, "192.0.0.9", 5351),
		providerLocalEntryFlow(IpProtocolUdp, "2001:1::1", 5351),
	}
	for i, flow := range blocked {
		flow.SourcePort = 49000 + i
		packet := ipOosUdpPacket(flow, testPcpMapRequest())
		rig.receivePacket(t, source, packet)
		if rig.admittedTo(flow.DestinationIp.String()) {
			t.Errorf("PCP to %s reached the exit LocalUserNat", flow.DestinationIp)
		}
	}
	reasons := rig.provider.SecurityPolicyReasons(false)
	for _, flow := range blocked {
		if count := reasons[SecurityPolicyReasonProviderLocal][newSecurityDestinationPort(flow)]; count == 0 {
			t.Errorf("PCP to %s: no provider-local count; reasons = %v", flow.DestinationIp, reasons)
		}
	}
}

// A peer's ICE candidate at a port control anycast address would have this
// host send its connectivity checks to its own gateway's port control
// service; it is refused like the host's own addresses, before any read, and
// a same-network peer may still offer it.
func TestRemoteIceCandidateRefusesPortControlAnycast(t *testing.T) {
	read := newTestProviderLocalRead(t)
	read.fail(errors.New("netlink refused"))
	networks, _ := newTestProviderLocalNetworks(t, read)

	for _, candidate := range []string{
		"candidate:1 1 udp 2130706431 192.0.0.9 5351 typ host",
		"candidate:2 1 udp 2130706431 2001:1::1 5351 typ host",
	} {
		if remoteIceCandidateAllowed(candidate, false, networks) {
			t.Errorf("%q from an untrusted peer: allowed", candidate)
		}
		if !remoteIceCandidateAllowed(candidate, true, networks) {
			t.Errorf("%q from a same-network peer: refused", candidate)
		}
	}
	for _, addr := range []string{"192.0.0.9", "::ffff:192.0.0.9", "2001:1::1"} {
		if !networks.containsAddr(netip.MustParseAddr(addr)) {
			t.Errorf("%s is not provider-local before the first read", addr)
		}
	}
	for _, addr := range []string{"192.0.0.10", "2001:1::2", "2001:1::3"} {
		if networks.containsAddr(netip.MustParseAddr(addr)) {
			t.Errorf("%s is provider-local", addr)
		}
	}
}
