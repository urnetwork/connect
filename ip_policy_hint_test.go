// Policy hint cache, ICMP unreachable builder, block action reasons and the
// multi-client fail-fast paths that are not covered by the root-cause tests.
// Time is injected; no test sleeps.
package connect

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"

	"github.com/urnetwork/connect/protocol"
)

type policyHintTestClock struct {
	now time.Time
}

func (self *policyHintTestClock) Now() time.Time {
	return self.now
}

func policyHintTestPath(ip string, port int, transport IpProtocol) *IpPath {
	return &IpPath{
		Version:         4,
		Protocol:        transport,
		SourceIp:        net.ParseIP("192.0.2.40"),
		SourcePort:      40000,
		DestinationIp:   net.ParseIP(ip),
		DestinationPort: port,
	}
}

func TestPolicyHintCacheExpiresAndBounds(t *testing.T) {
	clock := &policyHintTestClock{now: time.Unix(1700000000, 0)}
	hints := newPolicyHintCache(10*time.Minute, 4, clock.Now)

	path := policyHintTestPath("203.0.113.70", 51820, IpProtocolUdp)
	if hints.has(path) {
		t.Fatal("empty cache has a hint")
	}
	hints.add(path)
	if !hints.has(path) {
		t.Fatal("hint missing after add")
	}
	// the key is (address, port, transport)
	if hints.has(policyHintTestPath("203.0.113.70", 51821, IpProtocolUdp)) ||
		hints.has(policyHintTestPath("203.0.113.70", 51820, IpProtocolTcp)) ||
		hints.has(policyHintTestPath("203.0.113.71", 51820, IpProtocolUdp)) {
		t.Fatal("hint matched a different destination")
	}
	// a v4-mapped v6 destination is the same destination
	mapped := policyHintTestPath("::ffff:203.0.113.70", 51820, IpProtocolUdp)
	mapped.Version = 6
	if !hints.has(mapped) {
		t.Fatal("v4-mapped destination did not match")
	}
	// icmp is never hinted
	hints.add(policyHintTestPath("203.0.113.72", 0, IpProtocolIcmp))
	if hints.len() != 1 {
		t.Fatalf("hint count = %d, want 1", hints.len())
	}

	clock.now = clock.now.Add(10*time.Minute - time.Nanosecond)
	if !hints.has(path) {
		t.Fatal("hint expired before its ttl")
	}
	clock.now = clock.now.Add(time.Nanosecond)
	if hints.has(path) {
		t.Fatal("hint outlived its ttl")
	}
	if hints.len() != 0 {
		t.Fatalf("expired hint retained, count = %d", hints.len())
	}

	for i := 0; i < 10; i += 1 {
		clock.now = clock.now.Add(time.Second)
		hints.add(policyHintTestPath(fmt.Sprintf("203.0.113.%d", 100+i), 50000, IpProtocolTcp))
		if 4 < hints.len() {
			t.Fatalf("hint count = %d over the bound", hints.len())
		}
	}
	// the most recent hint survives eviction
	if !hints.has(policyHintTestPath("203.0.113.109", 50000, IpProtocolTcp)) {
		t.Fatal("newest hint evicted")
	}

	if newPolicyHintCache(0, 4, nil) != nil || newPolicyHintCache(time.Minute, 0, nil) != nil {
		t.Fatal("a disabled cache must be nil")
	}
	var disabled *policyHintCache
	disabled.add(path)
	if disabled.has(path) {
		t.Fatal("nil cache has a hint")
	}
}

func TestIcmpUnreachableForPolicyRejectV6(t *testing.T) {
	source := net.ParseIP("2001:db8::10")
	destination := net.ParseIP("2001:db8::20")
	payload := make([]byte, 2000)
	ip := &layers.IPv6{Version: 6, HopLimit: 64, NextHeader: layers.IPProtocolUDP, SrcIP: source, DstIP: destination}
	udp := &layers.UDP{SrcPort: 40001, DstPort: 51820}
	udp.SetNetworkLayerForChecksum(ip)
	buffer := gopacket.NewSerializeBuffer()
	if err := gopacket.SerializeLayers(buffer, gopacket.SerializeOptions{ComputeChecksums: true, FixLengths: true}, ip, udp, gopacket.Payload(payload)); err != nil {
		t.Fatal(err)
	}
	packet := buffer.Bytes()
	reply := icmpUnreachableForPolicyReject(packet)
	if reply == nil {
		t.Fatal("no unreachable for a v6 udp datagram")
	}
	defer MessagePoolReturn(reply)
	if len(reply) != icmp6MinMtu {
		t.Fatalf("reply length = %d, want the 1280-byte minimum mtu cap", len(reply))
	}
	ipProtocol, replySource, replyDestination, icmp, ok := parseIpv6(reply)
	if !ok || ipProtocol != ipProtocolNumberIcmp6 || icmp[0] != 1 || icmp[1] != 4 {
		t.Fatal("reply is not an icmpv6 port unreachable")
	}
	if !replySource.Equal(destination) || !replyDestination.Equal(source) {
		t.Fatal("reply is not addressed back to the app")
	}
	if transportChecksum(ipProtocolNumberIcmp6, replySource, replyDestination, icmp) != 0 {
		t.Fatal("icmpv6 checksum invalid")
	}

	// never an error about an error, and never for tcp
	if icmpUnreachableForPolicyReject(reply) != nil {
		t.Fatal("unreachable built for an icmp packet")
	}
	tcp := craftSecurityPacket(IpProtocolTcp, net.ParseIP("192.0.2.1"), 1, net.ParseIP("203.0.113.1"), 2, true, nil)
	if icmpUnreachableForPolicyReject(tcp) != nil {
		t.Fatal("unreachable built for tcp")
	}
	if icmpUnreachableForPolicyReject(nil) != nil || icmpUnreachableForPolicyReject([]byte{0x45}) != nil {
		t.Fatal("unreachable built for a malformed packet")
	}
}

func TestBlockActionReasonPrecedence(t *testing.T) {
	override := &blockActionMatch{routeOverride: &RouteOverride{Local: true}}
	cases := []struct {
		result   SecurityPolicyResult
		reason   SecurityPolicyReason
		blocker  bool
		match    *blockActionMatch
		expected string
	}{
		{result: SecurityPolicyResultDrop, reason: SecurityPolicyReasonDropEncrypted, expected: BlockActionReasonSecurityEncrypted},
		{result: SecurityPolicyResultIncident, reason: SecurityPolicyReasonBittorrent, expected: BlockActionReasonSecurityBittorrent},
		{result: SecurityPolicyResultDrop, reason: SecurityPolicyReasonCfaaDropPort, expected: BlockActionReasonSecurityPort},
		{result: SecurityPolicyResultDrop, reason: SecurityPolicyReasonCfaaDropIp, expected: BlockActionReasonSecurityIp},
		{result: SecurityPolicyResultIncident, reason: SecurityPolicyReasonNotPublic, expected: BlockActionReasonSecurityIp},
		{result: SecurityPolicyResultDrop, reason: SecurityPolicyReasonUnknown, expected: BlockActionReasonSecurity},
		{result: SecurityPolicyResultAllow, reason: SecurityPolicyReasonAllowTls, expected: ""},
		{result: SecurityPolicyResultAllow, blocker: true, expected: BlockActionReasonBlocker},
		{result: SecurityPolicyResultDrop, reason: SecurityPolicyReasonDropEncrypted, blocker: true, expected: BlockActionReasonBlocker},
		{result: SecurityPolicyResultDrop, reason: SecurityPolicyReasonDropEncrypted, blocker: true, match: override, expected: BlockActionReasonOverride},
	}
	for _, c := range cases {
		if got := blockActionReason(c.result, c.reason, c.blocker, c.match); got != c.expected {
			t.Errorf("reason(%v, %s, blocker=%t) = %q, want %q", c.result, c.reason, c.blocker, got, c.expected)
		}
	}
}

func nextPolicyBlockActions(t *testing.T, actions <-chan []*BlockAction) []*BlockAction {
	t.Helper()
	select {
	case result := <-actions:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for block actions")
		return nil
	}
}

// TestBlockActionCarriesReason distinguishes a security drop from a blocker
// match through the multi-client's own collector.
func TestBlockActionCarriesReason(t *testing.T) {
	capture := &policyRejectCapture{}
	multi := newPolicyRejectMulti(t, capture, false)
	actions := make(chan []*BlockAction, 8)
	unsub := multi.AddBlockActionCallback(func(value []*BlockAction) {
		actions <- value
	})
	defer unsub()

	policyRejectEncryptedTcpFlow(t, multi, capture, 47001)
	reasons := map[string]bool{}
	for len(reasons) == 0 || !reasons[BlockActionReasonSecurityEncrypted] {
		for _, action := range nextPolicyBlockActions(t, actions) {
			if action.Block {
				reasons[action.Reason] = true
			}
		}
	}
	if !reasons[BlockActionReasonSecurityEncrypted] {
		t.Fatalf("block reasons = %v, want security-encrypted", reasons)
	}
}

// TestMultiClientRouteOverrideFlowNotResetOnDrop: a flow the user already
// routes locally never reached a provider, so its deciding packet keeps the
// local route instead of being reset.
func TestMultiClientRouteOverrideFlowNotResetOnDrop(t *testing.T) {
	capture := &policyRejectCapture{}
	multi := newPolicyRejectMulti(t, capture, true)
	multi.SetBlockActionOverrides([]*BlockActionOverride{{
		OverrideId:    NewId(),
		Hosts:         []string{policyRejectDestinationIp.String()},
		RouteOverride: &RouteOverride{Local: true},
	}})
	sourcePort := 47011
	policyRejectSend(multi, IpProtocolTcp, sourcePort, true, nil)
	for i := 0; i < 3; i += 1 {
		policyRejectSend(multi, IpProtocolTcp, sourcePort, false, encryptedPayload(512))
	}
	if packets := capture.take(); len(packets) != 0 {
		t.Fatalf("a locally routed flow received %d rejects", len(packets))
	}
	if stats := multi.PacketStats(); stats.LocalEgressPacketCount != 4 || stats.BlockEgressPacketCount != 0 {
		t.Fatalf("local=%d block=%d, want the whole flow local", stats.LocalEgressPacketCount, stats.BlockEgressPacketCount)
	}
}

// TestMultiClientFirstDropGroupPathResets covers the batch (flow group) send
// path: the deciding group is refused with a reset and later groups to the
// destination are refused at once.
func TestMultiClientFirstDropGroupPathResets(t *testing.T) {
	capture := &policyRejectCapture{}
	multi := newPolicyRejectMulti(t, capture, false)
	sourcePort := 47021
	send := func(packets ...[]byte) {
		pooled := make([][]byte, len(packets))
		for i, packet := range packets {
			pooled[i] = MessagePoolCopy(packet)
		}
		multi.SendPacketBatch(SourceId(NewId()), protocol.ProvideMode_Public, pooled, 0)
	}
	craft := func(port int, syn bool, payload []byte) []byte {
		return craftSecurityPacket(IpProtocolTcp, policyRejectSourceIp, port, policyRejectDestinationIp, policyRejectPort, syn, payload)
	}
	send(craft(sourcePort, true, nil))
	send(craft(sourcePort, false, encryptedPayload(512)), craft(sourcePort, false, encryptedPayload(512)))
	if packets := capture.take(); len(packets) != 0 {
		t.Fatalf("inspecting group delivered %d packets", len(packets))
	}
	send(craft(sourcePort, false, encryptedPayload(512)), craft(sourcePort, false, encryptedPayload(512)))
	requireTcpReset(t, "deciding group", capture.take(), sourcePort)

	send(craft(sourcePort+1, true, nil))
	requireTcpReset(t, "retry group", capture.take(), sourcePort+1)
}

// TestMultiClientPolicyHintsDisabled keeps the fail fast but routes retries
// exactly as before when the hint cache is off.
func TestMultiClientPolicyHintsDisabled(t *testing.T) {
	capture := &policyRejectCapture{}
	settings := DefaultMultiClientSettings()
	settings.EventEpoch = 10 * time.Millisecond
	settings.HeartbeatInterval = 0
	settings.ProviderProbe = false
	settings.IpAssocSettings = nil
	settings.PolicyHintTtl = 0
	multi := NewRemoteUserNatMultiClient(context.Background(), &testingEmptyMultiClientGenerator{}, capture.receive, protocol.ProvideMode_Public, settings)
	defer multi.Close()
	policyRejectEncryptedTcpFlow(t, multi, capture, 47031)
	policyRejectSend(multi, IpProtocolTcp, 47032, true, nil)
	if packets := capture.take(); len(packets) != 0 {
		t.Fatalf("retry without hints delivered %d packets", len(packets))
	}
}
