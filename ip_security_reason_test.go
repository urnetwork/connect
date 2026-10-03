// Verdict-reason statistics: bounded, port-keyed, and attributed to the rule
// that decided each packet.
package connect

import (
	"context"
	"net"
	"path/filepath"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

// TestSecurityPolicyReasonStatsBounded verifies the reason table has the same
// per-key destination bound as the result table, and that out-of-range reasons
// share one bucket.
func TestSecurityPolicyReasonStatsBounded(t *testing.T) {
	stats := DefaultSecurityPolicyStatsCollector()
	const extraDestinationCount = 100
	for port := 1; port <= securityPolicyStatsMaxDestinationsPerResult+extraDestinationCount; port++ {
		stats.addDestinationReason(
			&IpPath{
				Version:         4,
				Protocol:        IpProtocolUdp,
				DestinationIp:   testStatsDestinationIp(4),
				DestinationPort: port,
			},
			SecurityPolicyResultDrop,
			SecurityPolicyReasonDropEncrypted,
			1,
		)
	}
	reasons := stats.Reasons(false)
	destinationCounts := reasons[SecurityPolicyReasonDropEncrypted]
	if count := len(destinationCounts); count != securityPolicyStatsMaxDestinationsPerResult {
		t.Fatalf("reason destination cardinality = %d, want %d", count, securityPolicyStatsMaxDestinationsPerResult)
	}
	if overflow := destinationCounts[securityPolicyStatsOverflowDestination]; overflow != extraDestinationCount+1 {
		t.Fatalf("overflow count = %d, want %d", overflow, extraDestinationCount+1)
	}

	for _, reason := range []SecurityPolicyReason{-5, 1000, 1001} {
		stats.addDestinationReason(
			&IpPath{Version: 4, Protocol: IpProtocolTcp, DestinationIp: testStatsDestinationIp(4), DestinationPort: 9000},
			SecurityPolicyResultAllow,
			reason,
			1,
		)
	}
	reasons = stats.Reasons(true)
	if len(reasons) != 2 {
		t.Fatalf("reason keys = %d, want drop-encrypted and one unknown bucket", len(reasons))
	}
	if count := reasons[SecurityPolicyReasonUnknown][SecurityDestination{Version: 4, Protocol: IpProtocolTcp, Port: 9000}]; count != 3 {
		t.Fatalf("unknown reason count = %d, want 3", count)
	}
	if after := stats.Reasons(false); len(after) != 0 {
		t.Fatalf("reasons after reset = %v, want empty", after)
	}
	// the result table is reset independently
	if results := stats.Stats(false); len(results[SecurityPolicyResultDrop]) == 0 {
		t.Fatal("reason reset cleared the result table")
	}
}

// TestSecurityPolicyReasonStatsNeverKeyByIp verifies reasons stay port-only
// even when the result table is configured to include addresses.
func TestSecurityPolicyReasonStatsNeverKeyByIp(t *testing.T) {
	stats := DefaultSecurityPolicyStatsCollector()
	stats.includeIp = true
	ipPath := &IpPath{
		Version:         4,
		Protocol:        IpProtocolTcp,
		DestinationIp:   testStatsDestinationIp(4),
		DestinationPort: 1935,
	}
	stats.addDestinationReason(ipPath, SecurityPolicyResultDrop, SecurityPolicyReasonDropEncrypted, 1)
	for destination := range stats.Reasons(false)[SecurityPolicyReasonDropEncrypted] {
		if destination.Ip != "" {
			t.Fatalf("reason keyed by ip %q", destination.Ip)
		}
	}
	for destination := range stats.Stats(false)[SecurityPolicyResultDrop] {
		if destination.Ip == "" {
			t.Fatal("result table lost its configured ip key")
		}
	}
}

// TestSecurityPolicyReasonsAttributeEachRule replays fixtures and static
// destinations through the default policy and checks the recorded reason.
func TestSecurityPolicyReasonsAttributeEachRule(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cases := []struct {
		fixture string
		reason  SecurityPolicyReason
	}{
		{fixture: "rtmp-digest", reason: SecurityPolicyReasonDropEncrypted},
		{fixture: "wireguard-handshake", reason: SecurityPolicyReasonAllowWireGuard},
		{fixture: "openvpn-udp", reason: SecurityPolicyReasonAllowOpenVpn},
		{fixture: "rtmp-publish", reason: SecurityPolicyReasonAllowRtmp},
		{fixture: "levin-handshake", reason: SecurityPolicyReasonAllowLevin},
		{fixture: "bittorrent-tcp-443", reason: SecurityPolicyReasonBittorrent},
		{fixture: "tls-443", reason: SecurityPolicyReasonAllowPrivileged},
		{fixture: "simplex-tls", reason: SecurityPolicyReasonAllowTls},
		{fixture: "monero-rpc-http", reason: SecurityPolicyReasonAllowHttp},
		{fixture: "bittorrent-tcp-51413", reason: SecurityPolicyReasonBittorrent},
		{fixture: "raknet-open-connection-zero-padded", reason: SecurityPolicyReasonAllowRakNet},
		{fixture: "ethereum-discv4", reason: SecurityPolicyReasonAllowEthereumDiscv4},
		{fixture: "ethereum-rlpx-eip8", reason: SecurityPolicyReasonAllowEthereumRlpx},
		{fixture: "ethereum-rlpx-off-curve", reason: SecurityPolicyReasonDropEncrypted},
	}
	for i, c := range cases {
		stats := DefaultSecurityPolicyStatsCollector()
		policy := DefaultSecurityPolicyWithStats(ctx, stats)
		fixture := loadSecurityFixture(t, filepath.Join("testdata", "ipsecurity", c.fixture+".json"))
		replayFixture(t, policy, fixture, 41000+i)
		portDestination := SecurityDestination{
			Version:  4,
			Protocol: fixture.transportProtocol(t),
			Port:     fixture.DestinationPort,
		}
		if count := stats.Reasons(false)[c.reason][portDestination]; count == 0 {
			t.Errorf("%s: reason %s not recorded; reasons = %v", c.fixture, c.reason, stats.Reasons(false))
		}
	}

	stats := DefaultSecurityPolicyStatsCollector()
	policy := DefaultSecurityPolicyWithStats(ctx, stats)
	static := []struct {
		ipPath *IpPath
		mode   protocol.ProvideMode
		reason SecurityPolicyReason
	}{
		{ipPath: dmcaPath(IpProtocolTcp, 42001, 22, true), mode: protocol.ProvideMode_Public, reason: SecurityPolicyReasonCfaaDropPort},
		{ipPath: dmcaPath(IpProtocolUdp, 42002, 123, false), mode: protocol.ProvideMode_Public, reason: SecurityPolicyReasonCfaaAllow},
		{ipPath: dmcaPath(IpProtocolTcp, 42003, 51413, true), mode: protocol.ProvideMode_Network, reason: SecurityPolicyReasonNetwork},
		{ipPath: dmcaPath(IpProtocolTcp, 42004, 8080, true), mode: protocol.ProvideMode_Public, reason: SecurityPolicyReasonInspecting},
		{
			ipPath: &IpPath{Version: 4, Protocol: IpProtocolTcp, SourceIp: securityFixtureSourceIp, DestinationIp: net.ParseIP("10.0.0.5").To4(), DestinationPort: 8081},
			mode:   protocol.ProvideMode_Public,
			reason: SecurityPolicyReasonNotPublic,
		},
	}
	for _, c := range static {
		if _, err := policy.InspectEgress(c.mode, c.ipPath, nil); err != nil {
			t.Fatal(err)
		}
		portDestination := newSecurityDestinationPort(c.ipPath)
		if count := stats.Reasons(false)[c.reason][portDestination]; count == 0 {
			t.Errorf("port %d: reason %s not recorded; reasons = %v", c.ipPath.DestinationPort, c.reason, stats.Reasons(false))
		}
	}
}
