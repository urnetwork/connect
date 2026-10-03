// Application-standard detectors (WireGuard, OpenVPN, RTMP, Levin, RakNet) and
// the privileged-port BitTorrent signatures. Every positive fixture first
// proves it exercises the encrypted heuristic, every detector is checked to
// keep BitTorrent precedence, and the encrypted BitTorrent variants (MSE/PE,
// encrypted uTP) must stay dropped. All bytes are deterministic.
package connect

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

// appTestBytes is a deterministic, random-looking byte string: a SHA-256
// counter stream keyed by seed.
func appTestBytes(seed string, n int) []byte {
	out := make([]byte, 0, n)
	for counter := uint64(0); len(out) < n; counter += 1 {
		var counterBytes [8]byte
		binary.BigEndian.PutUint64(counterBytes[:], counter)
		sum := sha256.Sum256(append([]byte(seed), counterBytes[:]...))
		out = append(out, sum[:min(n-len(out), len(sum))]...)
	}
	return out
}

func appTestCat(parts ...[]byte) []byte {
	var out []byte
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
}

// appTestPath addresses a flow with documentation addresses.
func appTestPath(transport IpProtocol, sourcePort int, destinationPort int, syn bool) *IpPath {
	return &IpPath{
		Version:         4,
		Protocol:        transport,
		SourceIp:        net.ParseIP("192.0.2.20"),
		SourcePort:      sourcePort,
		DestinationIp:   net.ParseIP("203.0.113.50"),
		DestinationPort: destinationPort,
		Syn:             syn,
	}
}

func appTestWireGuardInitiation(seed string) []byte {
	return appTestCat([]byte{1, 0, 0, 0}, appTestBytes(seed, 128), make([]byte, 16))
}

func appTestWireGuardTransport(seed string, receiver uint32, counter uint64, length int) []byte {
	b := appTestCat([]byte{4, 0, 0, 0}, make([]byte, 12), appTestBytes(seed, length-16))
	binary.LittleEndian.PutUint32(b[4:8], receiver)
	binary.LittleEndian.PutUint64(b[8:16], counter)
	return b
}

func appTestOpenVpn(opcode byte, keyId byte, sid []byte, body []byte) []byte {
	return appTestCat([]byte{opcode<<3 | keyId}, sid, body)
}

func appTestOpenVpnDataV2(keyId byte, peerId uint32, body []byte) []byte {
	return appTestCat([]byte{openVpnOpcodeDataV2<<3 | keyId, byte(peerId >> 16), byte(peerId >> 8), byte(peerId)}, body)
}

func appTestRtmpC0C1(seed string) []byte {
	return appTestCat([]byte{3}, appTestBytes(seed+"-time", 4), make([]byte, 4), appTestBytes(seed, 1451))
}

func appTestLevin(seed string, bodyLength int) []byte {
	head := make([]byte, levinHeadLength)
	copy(head, levinSignature)
	binary.LittleEndian.PutUint64(head[8:16], uint64(bodyLength))
	head[16] = 1
	binary.LittleEndian.PutUint32(head[17:21], 2002)
	binary.LittleEndian.PutUint32(head[25:29], 1)
	binary.LittleEndian.PutUint32(head[29:33], levinProtocolVersion1)
	return appTestCat(head, appTestBytes(seed, bodyLength))
}

func appTestRakNetOpenConnection1(padding []byte) []byte {
	return appTestCat([]byte{rakNetIdOpenConnectionRequest1}, rakNetOfflineMagic, []byte{0x06}, padding)
}

func appTestDhtPing() []byte {
	return []byte("d1:ad2:id20:abcdefghij0123456789e1:q4:ping1:t2:aa1:y1:qe")
}

func newAppTestDetector(configure func(settings *DmcaSecurityPolicySettings)) *dmcaDetector {
	settings := DefaultDmcaSecurityPolicySettings()
	if configure != nil {
		configure(settings)
	}
	return newDmcaDetector(nil, settings, newWebStandardDetector(DefaultWebStandardSettings()))
}

// legacyDmcaSettings is the policy before IPSECURITY-UPDATE4: no application
// standards and no privileged-port signatures.
func legacyDmcaSettings(settings *DmcaSecurityPolicySettings) {
	settings.App = nil
	settings.InspectPrivilegedSignatures = false
}

func appTestFlowState(t *testing.T, detector *dmcaDetector, ipPath *IpPath) *dmcaFlowState {
	t.Helper()
	key := dmcaFlowKeyForPath(Id{}, ipPath)
	shard := detector.shards[dmcaShardIndex(key)]
	shard.mu.RLock()
	defer shard.mu.RUnlock()
	state := shard.flows[key]
	if state == nil {
		t.Fatal("flow state missing")
	}
	return state
}

func requireLooksEncrypted(t *testing.T, name string, payload []byte) {
	t.Helper()
	settings := DefaultDmcaSecurityPolicySettings()
	b := payload
	if settings.MaxInspectionPayload < len(b) {
		b = b[:settings.MaxInspectionPayload]
	}
	if !payloadLooksEncrypted(b, settings) {
		t.Fatalf("%s: fixture must exercise the encrypted heuristic", name)
	}
}

// classifyAll returns the verdict of every payload. For TCP a SYN goes first
// and its verdict is not returned.
func classifyAll(detector *dmcaDetector, transport IpProtocol, sourcePort int, destinationPort int, payloads ...[]byte) []dmcaVerdict {
	if transport == IpProtocolTcp {
		detector.classify(appTestPath(transport, sourcePort, destinationPort, true), nil)
	}
	verdicts := []dmcaVerdict{}
	for _, payload := range payloads {
		verdicts = append(verdicts, detector.classify(appTestPath(transport, sourcePort, destinationPort, false), payload))
	}
	return verdicts
}

// firstDecision returns the index and value of the first verdict that is not
// inspecting, or -1.
func firstDecision(verdicts []dmcaVerdict) (int, dmcaVerdict) {
	for i, verdict := range verdicts {
		if verdict != dmcaInspecting {
			return i, verdict
		}
	}
	return -1, dmcaInspecting
}

func appTestRandomPackets(seed string, count int, length int) [][]byte {
	packets := [][]byte{}
	for i := 0; i < count; i += 1 {
		packets = append(packets, appTestBytes(fmt.Sprintf("%s-%d", seed, i), length))
	}
	return packets
}

func TestDmcaWireGuardHandshakeThenTransportAllowed(t *testing.T) {
	initiation := appTestWireGuardInitiation("wg-init")
	transports := [][]byte{
		appTestWireGuardTransport("wg-t0", 0x11223344, 0, 96),
		appTestWireGuardTransport("wg-t1", 0x11223344, 1, 128),
	}
	requireLooksEncrypted(t, "initiation", initiation)
	for _, transport := range transports {
		requireLooksEncrypted(t, "transport", transport)
	}
	payloads := appTestCat2(initiation, transports...)

	before := classifyAll(newAppTestDetector(legacyDmcaSettings), IpProtocolUdp, 40100, 51820, payloads...)
	if index, verdict := firstDecision(before); index != 2 || verdict != dmcaDropEncrypted {
		t.Fatalf("legacy policy decision = %d at %d, want drop at the third datagram", verdict, index)
	}

	detector := newAppTestDetector(nil)
	after := classifyAll(detector, IpProtocolUdp, 40100, 51820, payloads...)
	if after[0] != dmcaInspecting || after[1] != dmcaAllow || after[2] != dmcaAllow {
		t.Fatalf("verdicts = %v, want inspecting then allow", after)
	}
	// the allow becomes terminal (lock-free) once the budget is spent
	path := appTestPath(IpProtocolUdp, 40100, 51820, false)
	for counter := uint64(2); counter < uint64(DefaultDmcaSecurityPolicySettings().InspectionPacketBudget); counter += 1 {
		if verdict := detector.classify(path, appTestWireGuardTransport("wg-steady", 0x11223344, counter, 144)); verdict != dmcaAllow {
			t.Fatalf("steady state verdict = %d, want allow", verdict)
		}
	}
	if verdict, reason := appTestFlowState(t, detector, path).terminalVerdict(); verdict != dmcaAllow || reason != SecurityPolicyReasonAllowWireGuard {
		t.Fatalf("terminal = %d/%s, want allow/wireguard", verdict, reason)
	}
}

func appTestCat2(first []byte, rest ...[]byte) [][]byte {
	return append([][]byte{first}, rest...)
}

func TestDmcaWireGuardMidStreamCountersAllowed(t *testing.T) {
	payloads := [][]byte{
		appTestWireGuardTransport("wg-mid-0", 0x0a0b0c0d, 17, 112),
		appTestWireGuardTransport("wg-mid-1", 0x0a0b0c0d, 18, 144),
		appTestWireGuardTransport("wg-mid-2", 0x0a0b0c0d, 19, 128),
	}
	for _, payload := range payloads {
		requireLooksEncrypted(t, "transport", payload)
	}
	before := classifyAll(newAppTestDetector(legacyDmcaSettings), IpProtocolUdp, 40101, 41641, payloads...)
	if index, verdict := firstDecision(before); index != 2 || verdict != dmcaDropEncrypted {
		t.Fatalf("legacy policy decision = %d at %d, want drop at the third datagram", verdict, index)
	}
	after := classifyAll(newAppTestDetector(nil), IpProtocolUdp, 40101, 41641, payloads...)
	if index, verdict := firstDecision(after); index != 1 || verdict != dmcaAllow {
		t.Fatalf("decision = %d at %d, want allow at the second datagram", verdict, index)
	}
}

// requireDropWithoutAllow sends the near-miss packets, then random datagrams
// up to the budget, and requires the flow to be dropped without ever being
// allowed.
func requireDropWithoutAllow(t *testing.T, name string, transport IpProtocol, port int, payloads [][]byte) {
	t.Helper()
	settings := DefaultDmcaSecurityPolicySettings()
	filler := appTestRandomPackets(name+"-filler", settings.InspectionPacketBudget, 200)
	all := append(append([][]byte{}, payloads...), filler...)
	all = all[:settings.InspectionPacketBudget]
	verdicts := classifyAll(newAppTestDetector(nil), transport, 40200, port, all...)
	index, verdict := firstDecision(verdicts)
	if verdict != dmcaDropEncrypted {
		t.Fatalf("%s: decision = %d at %d (%v), want drop", name, verdict, index, verdicts)
	}
}

func TestDmcaWireGuardNearMissesDrop(t *testing.T) {
	reservedNonZero := appTestWireGuardInitiation("wg-near-a")
	reservedNonZero[1] = 5
	short := appTestWireGuardInitiation("wg-near-b")[:147]
	unaligned := appTestWireGuardTransport("wg-near-c", 1, 1, 100)
	cases := []struct {
		name     string
		payloads [][]byte
	}{
		{name: "reserved byte set", payloads: [][]byte{reservedNonZero}},
		{name: "length 147", payloads: [][]byte{short}},
		{name: "unaligned transport", payloads: [][]byte{unaligned, appTestWireGuardTransport("wg-near-c2", 1, 2, 100)}},
		{name: "decreasing counters", payloads: [][]byte{
			appTestWireGuardTransport("wg-near-d0", 7, 18, 112),
			appTestWireGuardTransport("wg-near-d1", 7, 17, 112),
		}},
		{name: "different receiver", payloads: [][]byte{
			appTestWireGuardTransport("wg-near-r0", 7, 18, 112),
			appTestWireGuardTransport("wg-near-r1", 8, 19, 112),
		}},
		{name: "counter jump", payloads: [][]byte{
			appTestWireGuardTransport("wg-near-j0", 7, 18, 112),
			appTestWireGuardTransport("wg-near-j1", 7, 18+wireGuardMaxCounterAdvance, 112),
		}},
		{name: "initiation then random", payloads: [][]byte{
			appTestWireGuardInitiation("wg-near-e"),
			appTestBytes("wg-near-e-blob", 96),
		}},
	}
	for _, c := range cases {
		requireDropWithoutAllow(t, c.name, IpProtocolUdp, 51820, c.payloads)
	}
}

func TestDmcaOpenVpnResetThenControlAllowed(t *testing.T) {
	sid := appTestBytes("ovpn-sid", 8)
	payloads := [][]byte{
		appTestOpenVpn(openVpnOpcodeHardResetClientV2, 0, sid, appTestBytes("ovpn-reset", 80)),
		appTestOpenVpn(openVpnOpcodeControlV1, 0, sid, appTestBytes("ovpn-control", 96)),
		appTestOpenVpn(openVpnOpcodeAckV1, 0, sid, appTestBytes("ovpn-ack", 64)),
	}
	for _, payload := range payloads {
		requireLooksEncrypted(t, "openvpn", payload)
	}
	before := classifyAll(newAppTestDetector(legacyDmcaSettings), IpProtocolUdp, 40300, 1194, payloads...)
	if index, verdict := firstDecision(before); index != 2 || verdict != dmcaDropEncrypted {
		t.Fatalf("legacy policy decision = %d at %d, want drop", verdict, index)
	}
	after := classifyAll(newAppTestDetector(nil), IpProtocolUdp, 40300, 1194, payloads...)
	if index, verdict := firstDecision(after); index != 1 || verdict != dmcaAllow {
		t.Fatalf("decision = %d at %d, want allow at the control packet", verdict, index)
	}

	// over TCP every packet carries a 2-byte length prefix; V3 reset and ack
	framed := func(packet []byte) []byte {
		return appTestCat([]byte{byte(len(packet) >> 8), byte(len(packet))}, packet)
	}
	tcpPayloads := [][]byte{
		framed(appTestOpenVpn(openVpnOpcodeHardResetClientV3, 0, sid, appTestBytes("ovpn-tcp-reset", 48))),
		framed(appTestOpenVpn(openVpnOpcodeAckV1, 0, sid, appTestBytes("ovpn-tcp-ack", 80))),
		framed(appTestOpenVpn(openVpnOpcodeControlV1, 0, sid, appTestBytes("ovpn-tcp-control", 160))),
	}
	tcpAfter := classifyAll(newAppTestDetector(nil), IpProtocolTcp, 40301, 1194, tcpPayloads...)
	if index, verdict := firstDecision(tcpAfter); index != 1 || verdict != dmcaAllow {
		t.Fatalf("tcp decision = %d at %d, want allow at the second packet", verdict, index)
	}
}

func TestDmcaOpenVpnSessionIdMismatchDrops(t *testing.T) {
	sid := appTestBytes("ovpn-sid-a", 8)
	other := appTestBytes("ovpn-sid-b", 8)
	requireDropWithoutAllow(t, "session id mismatch", IpProtocolUdp, 1194, [][]byte{
		appTestOpenVpn(openVpnOpcodeHardResetClientV2, 0, sid, appTestBytes("ovpn-m-reset", 40)),
		appTestOpenVpn(openVpnOpcodeControlV1, 0, other, appTestBytes("ovpn-m-control", 96)),
	})
	requireDropWithoutAllow(t, "reset with key id set", IpProtocolUdp, 1194, [][]byte{
		appTestOpenVpn(openVpnOpcodeHardResetClientV2, 1, sid, appTestBytes("ovpn-k-reset", 40)),
		appTestOpenVpn(openVpnOpcodeControlV1, 1, sid, appTestBytes("ovpn-k-control", 96)),
	})
	requireDropWithoutAllow(t, "data opcode after reset", IpProtocolUdp, 1194, [][]byte{
		appTestOpenVpn(openVpnOpcodeHardResetClientV2, 0, sid, appTestBytes("ovpn-d-reset", 40)),
		appTestOpenVpn(openVpnOpcodeDataV2, 0, sid, appTestBytes("ovpn-d-data", 96)),
	})
}

func TestDmcaOpenVpnDataV2MidStreamAllowed(t *testing.T) {
	payloads := [][]byte{
		appTestOpenVpnDataV2(2, 0x000102, appTestBytes("ovpn-data-0", 120)),
		appTestOpenVpnDataV2(2, 0x000102, appTestBytes("ovpn-data-1", 200)),
	}
	after := classifyAll(newAppTestDetector(nil), IpProtocolUdp, 40302, 1194, payloads...)
	if index, verdict := firstDecision(after); index != 1 || verdict != dmcaAllow {
		t.Fatalf("decision = %d at %d, want allow at the second data packet", verdict, index)
	}
	requireDropWithoutAllow(t, "peer id mismatch", IpProtocolUdp, 1194, [][]byte{
		appTestOpenVpnDataV2(2, 0x000102, appTestBytes("ovpn-data-m0", 120)),
		appTestOpenVpnDataV2(2, 0x000103, appTestBytes("ovpn-data-m1", 200)),
	})
	requireDropWithoutAllow(t, "key id mismatch", IpProtocolUdp, 1194, [][]byte{
		appTestOpenVpnDataV2(2, 0x000102, appTestBytes("ovpn-data-k0", 120)),
		appTestOpenVpnDataV2(3, 0x000102, appTestBytes("ovpn-data-k1", 200)),
	})
}

func TestDmcaRtmpHandshakeAllowed(t *testing.T) {
	payloads := [][]byte{
		appTestRtmpC0C1("rtmp"),
		appTestBytes("rtmp-c1-rest", 77),
		appTestBytes("rtmp-c2", 1536),
	}
	for _, payload := range payloads {
		requireLooksEncrypted(t, "rtmp", payload)
	}
	before := classifyAll(newAppTestDetector(legacyDmcaSettings), IpProtocolTcp, 40400, 1935, payloads...)
	if index, verdict := firstDecision(before); index != 2 || verdict != dmcaDropEncrypted {
		t.Fatalf("legacy policy decision = %d at %d, want drop at C2", verdict, index)
	}
	after := classifyAll(newAppTestDetector(nil), IpProtocolTcp, 40400, 1935, payloads...)
	if index, verdict := firstDecision(after); index != 0 || verdict != dmcaAllow {
		t.Fatalf("decision = %d at %d, want allow at C0/C1", verdict, index)
	}
}

func TestDmcaRtmpDigestVariantNotMatched(t *testing.T) {
	digest := appTestRtmpC0C1("rtmp-digest")
	copy(digest[5:9], []byte{0x80, 0x00, 0x07, 0x02})
	requireDropWithoutAllow(t, "rtmp digest", IpProtocolTcp, 1935, [][]byte{digest})
	// an RTMP-shaped payload later in the stream is not a handshake
	requireDropWithoutAllow(t, "rtmp not first", IpProtocolTcp, 1935, [][]byte{
		appTestBytes("rtmp-late-0", 300),
		appTestRtmpC0C1("rtmp-late"),
	})
}

func TestDmcaLevinHandshakeAllowed(t *testing.T) {
	payloads := [][]byte{
		appTestLevin("levin-0", 400),
		appTestLevin("levin-1", 400),
		appTestLevin("levin-2", 400),
	}
	for _, payload := range payloads {
		requireLooksEncrypted(t, "levin", payload)
	}
	before := classifyAll(newAppTestDetector(legacyDmcaSettings), IpProtocolTcp, 40500, 18080, payloads...)
	if index, verdict := firstDecision(before); index != 2 || verdict != dmcaDropEncrypted {
		t.Fatalf("legacy policy decision = %d at %d, want drop", verdict, index)
	}
	detector := newAppTestDetector(nil)
	after := classifyAll(detector, IpProtocolTcp, 40500, 18080, payloads...)
	if index, verdict := firstDecision(after); index != 0 || verdict != dmcaAllow {
		t.Fatalf("decision = %d at %d, want allow at the first packet", verdict, index)
	}
	_, reason, _ := detector.classifyForSenderDetailed(Id{}, appTestPath(IpProtocolTcp, 40500, 18080, false), appTestBytes("levin-3", 300))
	if reason != SecurityPolicyReasonAllowLevin {
		t.Fatalf("reason = %s, want allow-app-standard:levin", reason)
	}

	// protocol version 2 and an oversized body are not levin 1
	version2 := appTestLevin("levin-v2", 400)
	binary.LittleEndian.PutUint32(version2[29:33], 2)
	requireDropWithoutAllow(t, "levin version 2", IpProtocolTcp, 18080, [][]byte{version2})
	oversized := appTestLevin("levin-big", 400)
	binary.LittleEndian.PutUint64(oversized[8:16], levinMaxPacketSize+1)
	requireDropWithoutAllow(t, "levin oversized", IpProtocolTcp, 18080, [][]byte{oversized})
}

func TestDmcaRakNetOpenConnectionAllowed(t *testing.T) {
	zeroPadded := appTestRakNetOpenConnection1(make([]byte, 1385))
	if verdicts := classifyAll(newAppTestDetector(nil), IpProtocolUdp, 40600, 19132, zeroPadded); verdicts[0] != dmcaAllow {
		t.Fatalf("zero padded verdict = %d, want allow", verdicts[0])
	}

	randomPadded := [][]byte{
		appTestRakNetOpenConnection1(appTestBytes("raknet-0", 1400)),
		appTestRakNetOpenConnection1(appTestBytes("raknet-1", 1100)),
		appTestRakNetOpenConnection1(appTestBytes("raknet-2", 560)),
	}
	for _, payload := range randomPadded {
		requireLooksEncrypted(t, "raknet", payload)
	}
	before := classifyAll(newAppTestDetector(legacyDmcaSettings), IpProtocolUdp, 40601, 49152, randomPadded...)
	if index, verdict := firstDecision(before); index != 2 || verdict != dmcaDropEncrypted {
		t.Fatalf("legacy policy decision = %d at %d, want drop", verdict, index)
	}
	after := classifyAll(newAppTestDetector(nil), IpProtocolUdp, 40601, 49152, randomPadded...)
	if index, verdict := firstDecision(after); index != 0 || verdict != dmcaAllow {
		t.Fatalf("decision = %d at %d, want allow", verdict, index)
	}

	ping := appTestCat([]byte{rakNetIdUnconnectedPing}, appTestBytes("raknet-time", 8), rakNetOfflineMagic, appTestBytes("raknet-guid", 200))
	if verdicts := classifyAll(newAppTestDetector(nil), IpProtocolUdp, 40602, 19132, ping); verdicts[0] != dmcaAllow {
		t.Fatalf("unconnected ping verdict = %d, want allow", verdicts[0])
	}

	badMagic := appTestRakNetOpenConnection1(appTestBytes("raknet-bad", 1000))
	badMagic[16] ^= 0x01
	requireDropWithoutAllow(t, "raknet magic off by one bit", IpProtocolUdp, 49152, [][]byte{badMagic})
}

// appStandardFixtureFlows are the positive flows the default policy must
// admit end to end.
func appStandardFixtureFlows() []struct {
	name      string
	transport IpProtocol
	port      int
	payloads  [][]byte
} {
	sid := appTestBytes("e2e-ovpn-sid", 8)
	return []struct {
		name      string
		transport IpProtocol
		port      int
		payloads  [][]byte
	}{
		{name: "wireguard", transport: IpProtocolUdp, port: 51820, payloads: [][]byte{
			appTestWireGuardInitiation("e2e-wg"),
			appTestWireGuardTransport("e2e-wg-0", 9, 0, 96),
			appTestWireGuardTransport("e2e-wg-1", 9, 1, 128),
			appTestWireGuardTransport("e2e-wg-2", 9, 2, 144),
		}},
		{name: "openvpn", transport: IpProtocolUdp, port: 1194, payloads: [][]byte{
			appTestOpenVpn(openVpnOpcodeHardResetClientV2, 0, sid, appTestBytes("e2e-ovpn-0", 40)),
			appTestOpenVpn(openVpnOpcodeControlV1, 0, sid, appTestBytes("e2e-ovpn-1", 96)),
			appTestOpenVpn(openVpnOpcodeControlV1, 0, sid, appTestBytes("e2e-ovpn-2", 160)),
			appTestOpenVpn(openVpnOpcodeAckV1, 0, sid, appTestBytes("e2e-ovpn-3", 64)),
		}},
		{name: "rtmp", transport: IpProtocolTcp, port: 1935, payloads: [][]byte{
			appTestRtmpC0C1("e2e-rtmp"),
			appTestBytes("e2e-rtmp-1", 77),
			appTestBytes("e2e-rtmp-2", 1536),
			appTestBytes("e2e-rtmp-3", 1400),
		}},
		{name: "levin", transport: IpProtocolTcp, port: 18080, payloads: [][]byte{
			appTestLevin("e2e-levin-0", 400),
			appTestLevin("e2e-levin-1", 400),
			appTestLevin("e2e-levin-2", 400),
		}},
		{name: "raknet", transport: IpProtocolUdp, port: 49152, payloads: [][]byte{
			appTestRakNetOpenConnection1(appTestBytes("e2e-raknet-0", 1400)),
			appTestRakNetOpenConnection1(appTestBytes("e2e-raknet-1", 1100)),
			appTestBytes("e2e-raknet-2", 400),
			appTestBytes("e2e-raknet-3", 400),
		}},
	}
}

func TestEgressSecurityPolicyAppStandards(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i, flow := range appStandardFixtureFlows() {
		policy := DefaultSecurityPolicy(ctx)
		sourcePort := 40700 + i
		if flow.transport == IpProtocolTcp {
			if r, err := policy.InspectEgress(protocol.ProvideMode_Public, appTestPath(flow.transport, sourcePort, flow.port, true), nil); err != nil || r != SecurityPolicyResultAllow {
				t.Fatalf("%s: syn = %v %v", flow.name, r, err)
			}
		}
		for j, payload := range flow.payloads {
			r, err := policy.InspectEgress(protocol.ProvideMode_Public, appTestPath(flow.transport, sourcePort, flow.port, false), payload)
			if err != nil || r != SecurityPolicyResultAllow {
				t.Fatalf("%s: packet %d = %v %v, want allow", flow.name, j, r, err)
			}
		}
	}
}

func TestProviderReversePolicyAdmitsAppStandards(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sender := NewId()
	for i, flow := range appStandardFixtureFlows() {
		for _, legacy := range []bool{true, false} {
			var policy SecurityPolicy
			if legacy {
				dmca := DefaultDmcaSecurityPolicySettings()
				legacyDmcaSettings(dmca)
				policy = Reverse(NewSecurityPolicy(ctx, DefaultCfaaSecurityPolicySettings(), dmca, DefaultWebStandardSettings(), DefaultSecurityPolicyStatsCollector()))
			} else {
				policy = DefaultProviderSecurityPolicy(ctx)
			}
			sourcePort := 40800 + i
			if flow.transport == IpProtocolTcp {
				inspectAndRefreshIngressForSenderBorrowed(policy, sender, protocol.ProvideMode_Public, *appTestPath(flow.transport, sourcePort, flow.port, true), nil)
			}
			final := SecurityPolicyResultAllow
			for _, payload := range flow.payloads {
				r, err := inspectAndRefreshIngressForSenderBorrowed(policy, sender, protocol.ProvideMode_Public, *appTestPath(flow.transport, sourcePort, flow.port, false), payload)
				if err != nil {
					t.Fatal(err)
				}
				final = r
			}
			want := SecurityPolicyResultAllow
			if legacy {
				want = SecurityPolicyResultDrop
			}
			if final != want {
				t.Fatalf("%s legacy=%t: provider result = %v, want %v", flow.name, legacy, final, want)
			}
		}
	}
}

func TestDmcaBittorrentPrecedenceOverAppStandards(t *testing.T) {
	sid := appTestBytes("bt-ovpn-sid", 8)
	cases := []struct {
		name      string
		transport IpProtocol
		port      int
		payloads  [][]byte
	}{
		// a two-packet opener followed by a signature
		{name: "wireguard initiation then dht", transport: IpProtocolUdp, port: 51820, payloads: [][]byte{appTestWireGuardInitiation("bt-wg"), appTestDhtPing()}},
		{name: "wireguard transport then dht", transport: IpProtocolUdp, port: 51820, payloads: [][]byte{appTestWireGuardTransport("bt-wgt", 3, 4, 96), appTestDhtPing()}},
		{name: "openvpn reset then dht", transport: IpProtocolUdp, port: 1194, payloads: [][]byte{appTestOpenVpn(openVpnOpcodeHardResetClientV2, 0, sid, appTestBytes("bt-ovpn", 40)), appTestDhtPing()}},
		{name: "openvpn tcp reset then handshake", transport: IpProtocolTcp, port: 1194, payloads: [][]byte{
			appTestCat([]byte{0, 49}, appTestOpenVpn(openVpnOpcodeHardResetClientV2, 0, sid, appTestBytes("bt-ovpn-tcp", 40))),
			btHandshake(),
		}},
		// a confirmed two-packet standard followed by a signature within the budget
		{name: "wireguard confirmed then dht", transport: IpProtocolUdp, port: 51820, payloads: [][]byte{
			appTestWireGuardInitiation("bt-wgc"),
			appTestWireGuardTransport("bt-wgc-0", 3, 0, 96),
			appTestDhtPing(),
		}},
		// a single-packet standard followed by a signature within the budget
		{name: "rtmp then handshake", transport: IpProtocolTcp, port: 1935, payloads: [][]byte{appTestRtmpC0C1("bt-rtmp"), btHandshake()}},
		{name: "levin then handshake", transport: IpProtocolTcp, port: 18080, payloads: [][]byte{appTestLevin("bt-levin", 200), btHandshake()}},
		{name: "raknet then dht", transport: IpProtocolUdp, port: 49152, payloads: [][]byte{appTestRakNetOpenConnection1(make([]byte, 600)), appTestDhtPing()}},
		// a signature carried behind a single-packet standard's header
		{name: "levin head carrying a handshake", transport: IpProtocolTcp, port: 18080, payloads: [][]byte{appTestCat(appTestLevin("bt-levin-h", 0), btHandshake())}},
		{name: "raknet magic carrying dht", transport: IpProtocolUdp, port: 49152, payloads: [][]byte{appTestCat([]byte{rakNetIdOpenConnectionRequest1}, rakNetOfflineMagic, appTestDhtPing())}},
		{name: "rtmp header carrying a handshake", transport: IpProtocolTcp, port: 1935, payloads: [][]byte{appTestCat([]byte{3, 0, 0, 0, 0, 0, 0, 0, 0}, btHandshake())}},
	}
	for _, c := range cases {
		verdicts := classifyAll(newAppTestDetector(nil), c.transport, 40900, c.port, c.payloads...)
		if verdicts[len(verdicts)-1] != dmcaBittorrent {
			t.Errorf("%s: verdicts = %v, want bittorrent last", c.name, verdicts)
		}
	}

	// end to end the signature is an incident
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	policy := DefaultSecurityPolicy(ctx)
	path := appTestPath(IpProtocolUdp, 40901, 49152, false)
	policy.InspectEgress(protocol.ProvideMode_Public, path, appTestRakNetOpenConnection1(make([]byte, 600)))
	if r, _ := policy.InspectEgress(protocol.ProvideMode_Public, path, appTestDhtPing()); r != SecurityPolicyResultIncident {
		t.Fatalf("dht after raknet = %v, want incident", r)
	}
}

// mseFirstMessage is MSE/PE's opening: Ya (96 random bytes) + PadA (0-512
// random bytes).
func mseFirstMessage(seed string, padding int) []byte {
	return appTestBytes(seed, 96+padding)
}

func TestDmcaMseHandshakeStillDropped(t *testing.T) {
	for _, padding := range []int{0, 52, 148, 300, 512} {
		first := mseFirstMessage(fmt.Sprintf("mse-%d", padding), padding)
		if len(first) == wireGuardInitiationLength && first[0] == 1 && first[1] == 0 && first[2] == 0 && first[3] == 0 {
			t.Fatal("the 148-byte variant must not start like a wireguard initiation")
		}
		payloads := [][]byte{first, appTestBytes("mse-2", 120), appTestBytes("mse-3", 300)}
		for _, transport := range []IpProtocol{IpProtocolTcp, IpProtocolUdp} {
			verdicts := classifyAll(newAppTestDetector(nil), transport, 41000+padding, 50321, payloads...)
			if index, verdict := firstDecision(verdicts); index != 2 || verdict != dmcaDropEncrypted {
				t.Fatalf("mse pad %d transport %v: decision %d at %d, want drop at the third packet", padding, transport, verdict, index)
			}
		}
	}

	// the accepted leak: an encrypted datagram that happens to look like a
	// wireguard initiation is dropped one packet later, on the fourth
	prefixed := appTestCat([]byte{1, 0, 0, 0}, appTestBytes("mse-prefixed", 144))
	payloads := [][]byte{prefixed, appTestBytes("mse-p2", 120), appTestBytes("mse-p3", 300), appTestBytes("mse-p4", 200)}
	verdicts := classifyAll(newAppTestDetector(nil), IpProtocolUdp, 41600, 50322, payloads...)
	if index, verdict := firstDecision(verdicts); index != 3 || verdict != dmcaDropEncrypted {
		t.Fatalf("prefixed mse decision %d at %d, want drop at the fourth packet", verdict, index)
	}
}

func TestDmcaEncryptedUtpStillDropped(t *testing.T) {
	utpHeader := func(packetType byte, sequence uint16) []byte {
		header := make([]byte, 20)
		header[0] = packetType<<4 | 1
		copy(header[2:12], appTestBytes(fmt.Sprintf("utp-%d", sequence), 10))
		binary.BigEndian.PutUint16(header[16:18], sequence)
		return header
	}
	payloads := [][]byte{
		utpHeader(4, 1),
		appTestCat(utpHeader(0, 2), appTestBytes("utp-d2", 400)),
		appTestCat(utpHeader(0, 3), appTestBytes("utp-d3", 400)),
		appTestCat(utpHeader(0, 4), appTestBytes("utp-d4", 400)),
	}
	verdicts := classifyAll(newAppTestDetector(nil), IpProtocolUdp, 41700, 50323, payloads...)
	if index, verdict := firstDecision(verdicts); index != 3 || verdict != dmcaDropEncrypted {
		t.Fatalf("encrypted utp decision %d at %d, want drop", verdict, index)
	}
}

func TestDmcaPrivilegedPortBittorrentIsIncident(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cases := []struct {
		transport IpProtocol
		port      int
		payload   []byte
	}{
		{transport: IpProtocolTcp, port: 443, payload: btHandshake()},
		{transport: IpProtocolTcp, port: 80, payload: btHandshake()},
		{transport: IpProtocolTcp, port: 80, payload: []byte("GET /announce?info_hash=%01%02&peer_id=x HTTP/1.1\r\nHost: tracker.example\r\n\r\n")},
		{transport: IpProtocolUdp, port: 443, payload: appTestDhtPing()},
	}
	for _, c := range cases {
		policy := DefaultSecurityPolicy(ctx)
		r, err := policy.InspectEgress(protocol.ProvideMode_Public, appTestPath(c.transport, 42000, c.port, false), c.payload)
		if err != nil || r != SecurityPolicyResultIncident {
			t.Errorf("%v/%d: result = %v %v, want incident", c.transport, c.port, r, err)
		}
		if count := policy.(interface{ Testing_FlowCount() int }).Testing_FlowCount(); count != 0 {
			t.Errorf("%v/%d: privileged port created %d flows", c.transport, c.port, count)
		}

		provider := DefaultProviderSecurityPolicy(ctx)
		r, err = inspectAndRefreshIngressForSenderBorrowed(provider, NewId(), protocol.ProvideMode_Public, *appTestPath(c.transport, 42001, c.port, false), c.payload)
		if err != nil || r != SecurityPolicyResultIncident {
			t.Errorf("%v/%d: provider result = %v %v, want incident", c.transport, c.port, r, err)
		}
	}

	// the toggle restores the uninspected privileged allow
	detector := newAppTestDetector(func(settings *DmcaSecurityPolicySettings) {
		settings.InspectPrivilegedSignatures = false
	})
	if verdict := detector.classify(appTestPath(IpProtocolTcp, 42002, 443, false), btHandshake()); verdict != dmcaAllow {
		t.Fatalf("toggle off verdict = %d, want allow", verdict)
	}
}

func TestDmcaPrivilegedPortTlsUnaffected(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	policy := DefaultSecurityPolicy(ctx)
	path := appTestPath(IpProtocolTcp, 42100, 443, false)
	if r, _ := policy.InspectEgress(protocol.ProvideMode_Public, appTestPath(IpProtocolTcp, 42100, 443, true), nil); r != SecurityPolicyResultAllow {
		t.Fatalf("syn = %v", r)
	}
	if r, _ := policy.InspectEgress(protocol.ProvideMode_Public, path, tlsClientHello()); r != SecurityPolicyResultAllow {
		t.Fatalf("client hello = %v", r)
	}
	for i := 0; i < 8; i += 1 {
		if r, _ := policy.InspectEgress(protocol.ProvideMode_Public, path, appTestBytes(fmt.Sprintf("tls-app-%d", i), 512)); r != SecurityPolicyResultAllow {
			t.Fatalf("application data %d = %v, want allow", i, r)
		}
	}
	// non-tls encrypted openers on 443 (e.g. mtproto) stay trusted
	for i := 0; i < 8; i += 1 {
		if r, _ := policy.InspectEgress(protocol.ProvideMode_Public, appTestPath(IpProtocolTcp, 42101, 443, false), appTestBytes(fmt.Sprintf("mtproto-%d", i), 512)); r != SecurityPolicyResultAllow {
			t.Fatalf("encrypted 443 packet %d = %v, want allow", i, r)
		}
	}
	if count := policy.(interface{ Testing_FlowCount() int }).Testing_FlowCount(); count != 0 {
		t.Fatalf("privileged port flows = %d, want 0", count)
	}
}

func TestDmcaAppStandardsDisabledRestoreDrop(t *testing.T) {
	toggles := map[string]func(*AppStandardSettings){
		"wireguard": func(settings *AppStandardSettings) { settings.WireGuard = false },
		"openvpn":   func(settings *AppStandardSettings) { settings.OpenVpn = false },
		"rtmp":      func(settings *AppStandardSettings) { settings.Rtmp = false },
		"levin":     func(settings *AppStandardSettings) { settings.Levin = false },
		"raknet":    func(settings *AppStandardSettings) { settings.RakNet = false },
	}
	for i, flow := range appStandardFixtureFlows() {
		configurations := map[string]func(*DmcaSecurityPolicySettings){
			"master switch": func(settings *DmcaSecurityPolicySettings) { settings.App.Enabled = false },
			"nil settings":  func(settings *DmcaSecurityPolicySettings) { settings.App = nil },
			"own toggle":    func(settings *DmcaSecurityPolicySettings) { toggles[flow.name](settings.App) },
		}
		for name, configure := range configurations {
			verdicts := classifyAll(newAppTestDetector(configure), flow.transport, 43000+i, flow.port, flow.payloads...)
			if _, verdict := firstDecision(verdicts); verdict != dmcaDropEncrypted {
				t.Errorf("%s with %s: verdicts = %v, want drop", flow.name, name, verdicts)
			}
		}
		// another detector's toggle does not affect this one
		for other, toggle := range toggles {
			if other == flow.name {
				continue
			}
			verdicts := classifyAll(newAppTestDetector(func(settings *DmcaSecurityPolicySettings) { toggle(settings.App) }), flow.transport, 43100+i, flow.port, flow.payloads...)
			if _, verdict := firstDecision(verdicts); verdict != dmcaAllow {
				t.Errorf("%s with %s off: verdicts = %v, want allow", flow.name, other, verdicts)
			}
		}
	}
}

func TestDmcaAppCandidateDoesNotStallBudget(t *testing.T) {
	settings := DefaultDmcaSecurityPolicySettings()
	detector := newAppTestDetector(nil)
	path := appTestPath(IpProtocolUdp, 43200, 51820, false)
	var verdict dmcaVerdict
	for i := 0; i < settings.InspectionPacketBudget; i += 1 {
		initiation := appTestCat([]byte{1, 0, 0, 0}, appTestBytes(fmt.Sprintf("stall-%d", i), 144))
		requireLooksEncrypted(t, "fake initiation", initiation)
		verdict = detector.classify(path, initiation)
		if i < settings.InspectionPacketBudget-1 && verdict != dmcaInspecting {
			t.Fatalf("initiation %d verdict = %d, want inspecting", i, verdict)
		}
	}
	state := appTestFlowState(t, detector, path)
	terminal, reason := state.terminalVerdict()
	if verdict != dmcaAllow || terminal != dmcaAllow || reason != SecurityPolicyReasonAllowBudget {
		t.Fatalf("final = %d terminal %d/%s, want allow by budget", verdict, terminal, reason)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.encryptedPackets != 0 || state.inspectedPackets != settings.InspectionPacketBudget {
		t.Fatalf("encrypted=%d inspected=%d, want 0 and the budget: openers are bounded and not counted", state.encryptedPackets, state.inspectedPackets)
	}
}

// TestFixtureReplayLegacyPolicyMatchesBefore checks every fixture's
// expect_before against the policy with the update's detectors off.
func TestFixtureReplayLegacyPolicyMatchesBefore(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i, fixture := range loadSecurityFixtures(t) {
		dmca := DefaultDmcaSecurityPolicySettings()
		legacyDmcaSettings(dmca)
		policy := NewSecurityPolicy(ctx, DefaultCfaaSecurityPolicySettings(), dmca, DefaultWebStandardSettings(), DefaultSecurityPolicyStatsCollector())
		results := replayFixture(t, policy, fixture, 44000+i)
		want := parseSecurityPolicyResult(t, fixture.Name, fixture.ExpectBefore)
		if got := results[len(results)-1]; got != want {
			t.Errorf("%s: legacy last result = %v (all %v), want %v", fixture.Name, got, results, want)
		}
	}
}

func appStandardFuzzNearMisses() [][]byte {
	reserved := appTestWireGuardInitiation("fuzz-near-wg")
	reserved[2] = 1
	digest := appTestRtmpC0C1("fuzz-near-rtmp")
	digest[6] = 7
	levin := appTestLevin("fuzz-near-levin", 64)
	levin[29] = 2
	raknet := appTestRakNetOpenConnection1(appTestBytes("fuzz-near-raknet", 64))
	raknet[1] = 0x01
	return [][]byte{
		reserved,
		appTestWireGuardInitiation("fuzz-near-wg-short")[:147],
		appTestWireGuardTransport("fuzz-near-wg-t", 1, 1, 100),
		digest,
		levin,
		raknet,
		appTestOpenVpn(openVpnOpcodeHardResetClientV2, 1, appTestBytes("fuzz-sid", 8), appTestBytes("fuzz-ovpn", 40)),
	}
}

func TestAppStandardDetectorsZeroAlloc(t *testing.T) {
	app := newAppStandardDetector(DefaultAppStandardSettings())
	udp := appTestPath(IpProtocolUdp, 1, 51820, false)
	tcp := appTestPath(IpProtocolTcp, 1, 1935, false)
	initiation := appTestWireGuardInitiation("alloc-wg")
	transport := appTestWireGuardTransport("alloc-wg-t", 1, 1, 96)
	rtmp := appTestRtmpC0C1("alloc-rtmp")
	if allocations := testing.AllocsPerRun(1000, func() {
		candidate, ok := app.open(udp, initiation)
		if !ok {
			t.Fatal("initiation did not open")
		}
		if _, ok := app.confirm(&candidate, udp, transport); !ok {
			t.Fatal("transport did not confirm")
		}
		if _, _, ok := app.match(tcp, rtmp, true); !ok {
			t.Fatal("rtmp did not match")
		}
		containsBittorrentSignature(rtmp[9:])
	}); allocations != 0 {
		t.Fatalf("app standard detectors allocate %.2f objects per call, want 0", allocations)
	}
}

func FuzzAppStandardDetectors(f *testing.F) {
	for _, flow := range appStandardFixtureFlows() {
		for _, payload := range flow.payloads {
			f.Add(payload, flow.transport == IpProtocolTcp)
		}
	}
	nearMisses := appStandardFuzzNearMisses()
	for _, payload := range nearMisses {
		f.Add(payload, false)
		f.Add(payload, true)
	}
	app := newAppStandardDetector(DefaultAppStandardSettings())
	for _, payload := range nearMisses {
		for _, transport := range []IpProtocol{IpProtocolTcp, IpProtocolUdp} {
			path := appTestPath(transport, 1, 9000, false)
			if _, _, ok := app.match(path, payload, true); ok {
				f.Fatalf("near miss matched as %v", transport)
			}
		}
	}
	f.Fuzz(func(t *testing.T, payload []byte, tcp bool) {
		transport := IpProtocolUdp
		if tcp {
			transport = IpProtocolTcp
		}
		path := appTestPath(transport, 1, 9000, false)
		app.match(path, payload, true)
		if candidate, ok := app.open(path, payload); ok {
			app.confirm(&candidate, path, payload)
		}
		containsBittorrentSignature(payload)
		detector := newAppTestDetector(nil)
		detector.classify(path, payload)
		detector.classify(path, payload)
	})
}
