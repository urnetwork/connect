// Packet-fixture probe harness for the security policy. Each fixture in
// testdata/ipsecurity holds the first client payloads of one protocol flow,
// synthesized from public protocol facts (see testdata/ipsecurity/README.md),
// and the verdict the policy must reach. Fixtures are replayed through the
// same entry point RemoteUserNatMultiClient.SendPacket uses: a real IP packet
// is built per payload and parsed with ParseIpPathWithPayload, with a SYN
// first for TCP.
package connect

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

type securityFixture struct {
	Name            string   `json:"name"`
	Protocol        string   `json:"protocol"`
	Provenance      string   `json:"provenance"`
	Transport       string   `json:"transport"`
	DestinationPort int      `json:"destination_port"`
	Payloads        []string `json:"payloads"`
	ExpectBefore    string   `json:"expect_before"`
	ExpectAfter     string   `json:"expect_after"`
	Note            string   `json:"note"`
}

// documentation addresses (RFC 5737); the destination is public unicast for
// the policy
var (
	securityFixtureSourceIp      = net.ParseIP("192.0.2.10")
	securityFixtureDestinationIp = net.ParseIP("203.0.113.40")
)

func loadSecurityFixtures(t *testing.T) []*securityFixture {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "ipsecurity", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no security fixtures")
	}
	fixtures := []*securityFixture{}
	for _, path := range paths {
		fixture := loadSecurityFixture(t, path)
		fixtures = append(fixtures, fixture)
	}
	return fixtures
}

func loadSecurityFixture(t *testing.T, path string) *securityFixture {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture securityFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if fixture.Name+".json" != filepath.Base(path) {
		t.Fatalf("%s: fixture name %q does not match the file", path, fixture.Name)
	}
	return &fixture
}

func (self *securityFixture) transportProtocol(t *testing.T) IpProtocol {
	t.Helper()
	switch self.Transport {
	case "tcp":
		return IpProtocolTcp
	case "udp":
		return IpProtocolUdp
	default:
		t.Fatalf("%s: unknown transport %q", self.Name, self.Transport)
		return IpProtocolUnknown
	}
}

func (self *securityFixture) payloadBytes(t *testing.T) [][]byte {
	t.Helper()
	payloads := [][]byte{}
	for i, payloadHex := range self.Payloads {
		payload, err := hex.DecodeString(payloadHex)
		if err != nil {
			t.Fatalf("%s: payload %d: %v", self.Name, i, err)
		}
		payloads = append(payloads, payload)
	}
	return payloads
}

// fixturePackets builds the wire packets for a fixture: a SYN first for TCP,
// then one packet per payload.
func fixturePackets(t *testing.T, fixture *securityFixture, sourcePort int) [][]byte {
	t.Helper()
	transport := fixture.transportProtocol(t)
	packets := [][]byte{}
	if transport == IpProtocolTcp {
		packets = append(packets, craftSecurityPacket(
			transport,
			securityFixtureSourceIp,
			sourcePort,
			securityFixtureDestinationIp,
			fixture.DestinationPort,
			true,
			nil,
		))
	}
	for _, payload := range fixture.payloadBytes(t) {
		packets = append(packets, craftSecurityPacket(
			transport,
			securityFixtureSourceIp,
			sourcePort,
			securityFixtureDestinationIp,
			fixture.DestinationPort,
			false,
			payload,
		))
	}
	for i, packet := range packets {
		if packet == nil {
			t.Fatalf("%s: packet %d could not be built", fixture.Name, i)
		}
	}
	return packets
}

// replayFixture drives InspectEgress exactly as the multi-client send path
// does and returns the result of every packet, SYN included.
func replayFixture(t *testing.T, policy SecurityPolicy, fixture *securityFixture, sourcePort int) []SecurityPolicyResult {
	t.Helper()
	results := []SecurityPolicyResult{}
	for i, packet := range fixturePackets(t, fixture, sourcePort) {
		ipPath, payload, err := ParseIpPathWithPayload(packet)
		if err != nil {
			t.Fatalf("%s: packet %d: %v", fixture.Name, i, err)
		}
		result, err := policy.InspectEgress(protocol.ProvideMode_Public, ipPath, payload)
		if err != nil {
			t.Fatalf("%s: packet %d: %v", fixture.Name, i, err)
		}
		policy.RefreshEgress(ipPath)
		results = append(results, result)
	}
	return results
}

func parseSecurityPolicyResult(t *testing.T, name string, value string) SecurityPolicyResult {
	t.Helper()
	switch strings.ToLower(value) {
	case "allow":
		return SecurityPolicyResultAllow
	case "drop":
		return SecurityPolicyResultDrop
	case "incident":
		return SecurityPolicyResultIncident
	default:
		t.Fatalf("%s: unknown expected result %q", name, value)
		return SecurityPolicyResultIncident
	}
}

// TestFixtureReplayMatchesExpectedVerdicts replays every fixture through the
// default policy and checks the verdict of its last packet against
// expect_after. ip_security_appstandard_test.go checks expect_before against
// the policy with the update's detectors turned off, so drift in either
// direction fails.
func TestFixtureReplayMatchesExpectedVerdicts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for i, fixture := range loadSecurityFixtures(t) {
		policy := DefaultSecurityPolicy(ctx)
		results := replayFixture(t, policy, fixture, 40000+i)
		want := parseSecurityPolicyResult(t, fixture.Name, fixture.ExpectAfter)
		if got := results[len(results)-1]; got != want {
			t.Errorf("%s: last result = %v (all %v), want %v", fixture.Name, got, results, want)
		}
	}
}

// TestFixturePayloadsUseNoProductionIdentity keeps the fixture corpus
// synthesized: every fixture names its public provenance and stays within the
// inspection window the harness documents.
func TestFixturePayloadsUseNoProductionIdentity(t *testing.T) {
	for _, fixture := range loadSecurityFixtures(t) {
		if fixture.Provenance == "" {
			t.Errorf("%s: missing provenance", fixture.Name)
		}
		if len(fixture.Payloads) == 0 || 8 < len(fixture.Payloads) {
			t.Errorf("%s: %d payloads, want 1-8", fixture.Name, len(fixture.Payloads))
		}
		if fixture.DestinationPort <= 0 || 65535 < fixture.DestinationPort {
			t.Errorf("%s: invalid destination port %d", fixture.Name, fixture.DestinationPort)
		}
	}
}
