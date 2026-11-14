package connect

// Service anycast in the provider role (ip_security_local.go,
// providerLocalAnycastAddrs). The special-purpose registries mark the anycast
// addresses of port control (192.0.0.9, 2001:1::1), TURN (192.0.0.10,
// 2001:1::2) and DNS-SD service registration (2001:1::3) globally reachable,
// so the public rule admits them, and through generation 3 so did the
// provider role. A request to one is answered by the network in front of the
// provider: a consumer could have the provider's gateway map a port of its
// public address to the provider host (a PCP MAP request), allocate relays on
// the TURN server of the provider's network, or publish services on the
// provider's links through its registrar (an SRP update). These tests assert
// results only, so they run unchanged against the code before the change.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"testing"

	"github.com/urnetwork/connect/v2026/protocol"
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

// A TURN Allocate request (RFC 8656 §7.1): a STUN header with the method
// Allocate, no attributes, the magic cookie and a transaction id.
func testTurnAllocateRequest() []byte {
	request := make([]byte, 20)
	request[0], request[1] = 0x00, 0x03
	request[4], request[5], request[6], request[7] = 0x21, 0x12, 0xa4, 0x42
	copy(request[8:20], "synthetic-tx")
	return request
}

// The start of an SRP registration (RFC 9665 §3.3): a DNS UPDATE (opcode 5)
// header and its zone section, default.service.arpa SOA IN, with no records.
func testSrpUpdate() []byte {
	return []byte{
		0x12, 0x34, 0x28, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x07, 'd', 'e', 'f', 'a', 'u', 'l', 't', 0x07, 's', 'e', 'r', 'v', 'i', 'c', 'e', 0x04, 'a', 'r', 'p', 'a', 0x00,
		0x00, 0x06, 0x00, 0x01,
	}
}

// One kind of packet to an anycast address: its service's request and port,
// or anything else.
type testAnycastDestination struct {
	ip       string
	protocol IpProtocol
	port     int
	payload  []byte
}

// What a consumer sends each service anycast address: the service's own
// request, TLS to 443, and an echo request, to both families and to the
// IPv4-mapped form of each IPv4 address.
func testServiceAnycastDestinations() []testAnycastDestination {
	destinations := []testAnycastDestination{}
	for _, service := range []struct {
		ips      []string
		protocol IpProtocol
		port     int
		payload  []byte
	}{
		{[]string{"192.0.0.9", "::ffff:192.0.0.9", "2001:1::1"}, IpProtocolUdp, 5351, testPcpMapRequest()},
		{[]string{"192.0.0.10", "::ffff:192.0.0.10", "2001:1::2"}, IpProtocolUdp, 3478, testTurnAllocateRequest()},
		{[]string{"2001:1::3"}, IpProtocolUdp, 53, testSrpUpdate()},
	} {
		for _, ip := range service.ips {
			destinations = append(destinations,
				testAnycastDestination{ip, service.protocol, service.port, service.payload},
				testAnycastDestination{ip, IpProtocolTcp, 443, tlsClientHello()},
				testAnycastDestination{ip, IpProtocolIcmp, 0, nil},
			)
		}
	}
	return destinations
}

// The packet of d from a consumer of the provider: a v4-mapped destination
// stays in its v6 packet.
func testAnycastPath(d testAnycastDestination, sourcePort int) *IpPath {
	ip := net.ParseIP(d.ip)
	version := 6
	sourceIp := net.ParseIP("fd00::9")
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

// A consumer's packets to the service anycast addresses, each service's own
// request and anything else, are provider-local incidents on every entry the
// provider takes and for every relationship but a same-network one, in both
// families and the IPv4-mapped form.
func TestProviderPolicyRefusesServiceAnycast(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stats := DefaultSecurityPolicyStatsCollector()
	networks, _ := newTestProviderLocalNetworks(t, newTestProviderLocalRead(t, "2a01:4f8:c17:1234::10/64", "198.51.100.20/32"))
	policy := newTestProviderSecurityPolicy(ctx, networks, stats)
	inner := policy.(*reverseSecurityPolicy).policy.(*securityPolicy)
	anycast := testServiceAnycastDestinations()

	sourcePort := 46000
	path := func(d testAnycastDestination) *IpPath {
		sourcePort += 1
		return testAnycastPath(d, sourcePort)
	}
	sender := NewId()
	check := func(mode protocol.ProvideMode, want SecurityPolicyResult) {
		for _, d := range anycast {
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
		check(mode, SecurityPolicyResultIncident)
	}
	// a user's own devices may still use their own network's services
	check(protocol.ProvideMode_Network, SecurityPolicyResultAllow)

	reasons := stats.Reasons(false)
	for _, d := range anycast {
		if d.protocol == IpProtocolTcp || d.protocol == IpProtocolIcmp {
			continue
		}
		if count := reasons[SecurityPolicyReasonProviderLocal][newSecurityDestinationPort(path(d))]; count == 0 {
			t.Errorf("no provider-local count for %s/%d to %s; reasons = %v", d.protocol, d.port, d.ip, reasons)
		}
	}
}

// Service anycast is local before any read of this host's addresses has
// succeeded: an incident with the reason provider-local, not the fail-closed
// drop the guard gives every other public destination then.
func TestProviderPolicyRefusesServiceAnycastBeforeTheFirstRead(t *testing.T) {
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

	for i, d := range testServiceAnycastDestinations() {
		if d.protocol != IpProtocolUdp {
			continue
		}
		request := testAnycastPath(d, 47000+i)
		if r, err := policy.InspectIngress(protocol.ProvideMode_Public, request, d.payload); err != nil || r != SecurityPolicyResultIncident {
			t.Errorf("udp/%d to %s before the first read = %s (%v), want incident", d.port, d.ip, r, err)
		}
		if count := stats.Reasons(false)[SecurityPolicyReasonProviderLocal][newSecurityDestinationPort(request)]; count == 0 {
			t.Errorf("udp/%d to %s before the first read: no provider-local count; reasons = %v", d.port, d.ip, stats.Reasons(false))
		}
	}
}

// The client role and the public rule are unchanged: the service anycast
// addresses are public, and the client role's own egress to them is judged as
// before.
func TestClientPolicyKeepsServiceAnycastPublic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := DefaultSecurityPolicy(ctx)
	for i, literal := range []string{"192.0.0.9", "::ffff:192.0.0.9", "2001:1::1", "192.0.0.10", "::ffff:192.0.0.10", "2001:1::2", "2001:1::3"} {
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

// A consumer's request to each service anycast address, PCP MAP, TURN
// Allocate and an SRP update, never reaches the exit LocalUserNat at the
// provider's real entry point, and is counted as provider-local.
func TestRemoteUserNatProviderRefusesServiceAnycastAtClientReceive(t *testing.T) {
	rig := newProviderLocalEntryRig(t, "2a01:4f8:c17:1234::10/64", "198.51.100.20/32")
	source := SourceId(NewId())

	blocked := []*IpPath{}
	for _, request := range []struct {
		ip      string
		port    int
		payload []byte
	}{
		{"192.0.0.9", 5351, testPcpMapRequest()},
		{"2001:1::1", 5351, testPcpMapRequest()},
		{"192.0.0.10", 3478, testTurnAllocateRequest()},
		{"2001:1::2", 3478, testTurnAllocateRequest()},
		{"2001:1::3", 53, testSrpUpdate()},
	} {
		flow := providerLocalEntryFlow(IpProtocolUdp, request.ip, request.port)
		flow.SourcePort = 49000 + len(blocked)
		rig.receivePacket(t, source, ipOosUdpPacket(flow, request.payload))
		if rig.admittedTo(flow.DestinationIp.String()) {
			t.Errorf("udp/%d to %s reached the exit LocalUserNat", request.port, request.ip)
		}
		blocked = append(blocked, flow)
	}
	reasons := rig.provider.SecurityPolicyReasons(false)
	for _, flow := range blocked {
		if count := reasons[SecurityPolicyReasonProviderLocal][newSecurityDestinationPort(flow)]; count == 0 {
			t.Errorf("udp/%d to %s: no provider-local count; reasons = %v", flow.DestinationPort, flow.DestinationIp, reasons)
		}
	}
}

// A peer's ICE candidate at a service anycast address would have this host
// send its connectivity checks to its own network's gateway, TURN server or
// registrar; it is refused like the host's own addresses, before any read,
// and a same-network peer may still offer it.
func TestRemoteIceCandidateRefusesServiceAnycast(t *testing.T) {
	read := newTestProviderLocalRead(t)
	read.fail(errors.New("netlink refused"))
	networks, _ := newTestProviderLocalNetworks(t, read)

	for _, candidate := range []string{
		"candidate:1 1 udp 2130706431 192.0.0.9 5351 typ host",
		"candidate:2 1 udp 2130706431 2001:1::1 5351 typ host",
		"candidate:3 1 udp 2130706431 192.0.0.10 3478 typ host",
		"candidate:4 1 udp 2130706431 2001:1::2 3478 typ host",
		"candidate:5 1 udp 2130706431 2001:1::3 53 typ host",
	} {
		if remoteIceCandidateAllowed(candidate, false, networks) {
			t.Errorf("%q from an untrusted peer: allowed", candidate)
		}
		if !remoteIceCandidateAllowed(candidate, true, networks) {
			t.Errorf("%q from a same-network peer: refused", candidate)
		}
	}
	for _, addr := range []string{"192.0.0.9", "::ffff:192.0.0.9", "2001:1::1", "192.0.0.10", "::ffff:192.0.0.10", "2001:1::2", "2001:1::3"} {
		if !networks.containsAddr(netip.MustParseAddr(addr)) {
			t.Errorf("%s is not provider-local before the first read", addr)
		}
	}
	// the registries' other anycast, global services that no network in
	// front of a provider answers, stays public: as112 and amt relays
	for _, addr := range []string{"192.31.196.1", "192.175.48.1", "2001:4:112::1", "192.52.193.1", "2001:3::1"} {
		if networks.containsAddr(netip.MustParseAddr(addr)) {
			t.Errorf("%s is provider-local", addr)
		}
	}
}
