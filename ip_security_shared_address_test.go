package connect

// C6a: 100.64.0.0/10 under the public rules. isPublicUnicast keeps calling it
// public, so nothing here can become an Incident and call ReportAbuse. With
// `CfaaSecurityPolicySettings.DropSharedAddressSpace` off, the default, a packet
// keeps its verdict and only counts the reason. With it on, the packet is a Drop
// with that reason, on every public-rule path and through Reverse.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

var (
	sharedAddressIp = net.ParseIP("100.64.0.1").To4()
	// the same packets to an address the shared-address rule does not know
	sharedAddressPublicTwinIp = net.ParseIP("203.0.113.7").To4()
)

func sharedAddressPolicy(ctx context.Context, drop bool) (SecurityPolicy, *SecurityPolicyStatsCollector) {
	settings := DefaultCfaaSecurityPolicySettings()
	settings.DropSharedAddressSpace = drop
	stats := DefaultSecurityPolicyStatsCollector()
	policy := NewSecurityPolicy(ctx, settings, DefaultDmcaSecurityPolicySettings(), DefaultWebStandardSettings(), stats)
	return policy, stats
}

func sharedAddressPath(destinationIp net.IP, version int, protocol IpProtocol, port int) *IpPath {
	sourceIp := securityFixtureSourceIp
	if version == 6 {
		sourceIp = net.ParseIP("2001:db8::10")
	}
	return &IpPath{
		Version:         version,
		Protocol:        protocol,
		SourceIp:        sourceIp,
		SourcePort:      43000,
		DestinationIp:   destinationIp,
		DestinationPort: port,
	}
}

// One way a packet reaches the public rules: the direct egress calls, the group
// call, and the provider's view through Reverse, where the remote client's
// outbound packet is the provider's ingress.
type sharedAddressEntry struct {
	name string
	// the packets one call inspects
	packetCount uint64
	inspect     func(policy SecurityPolicy, provideMode protocol.ProvideMode, ipPath *IpPath) (SecurityPolicyResult, error)
}

func sharedAddressEntries() []sharedAddressEntry {
	senderId := NewId()
	return append([]sharedAddressEntry{
		{
			name:        "InspectEgress",
			packetCount: 1,
			inspect: func(policy SecurityPolicy, provideMode protocol.ProvideMode, ipPath *IpPath) (SecurityPolicyResult, error) {
				return policy.InspectEgress(provideMode, ipPath, nil)
			},
		},
		{
			name:        "detailed",
			packetCount: 1,
			inspect: func(policy SecurityPolicy, provideMode protocol.ProvideMode, ipPath *IpPath) (SecurityPolicyResult, error) {
				result, _, err := inspectEgressDetailed(policy, provideMode, ipPath, nil)
				return result, err
			},
		},
		{
			name:        "borrowed",
			packetCount: 1,
			inspect: func(policy SecurityPolicy, provideMode protocol.ProvideMode, ipPath *IpPath) (SecurityPolicyResult, error) {
				return inspectAndRefreshEgressBorrowed(policy, provideMode, *ipPath, nil)
			},
		},
		{
			name:        "for sender",
			packetCount: 1,
			inspect: func(policy SecurityPolicy, provideMode protocol.ProvideMode, ipPath *IpPath) (SecurityPolicyResult, error) {
				return inspectAndRefreshEgressForSenderBorrowed(policy, senderId, provideMode, *ipPath, nil)
			},
		},
		{
			name:        "group",
			packetCount: 3,
			inspect: func(policy SecurityPolicy, provideMode protocol.ProvideMode, ipPath *IpPath) (SecurityPolicyResult, error) {
				result, _, err := inspectAndRefreshEgressGroupDetailedBorrowed(
					policy,
					provideMode,
					[]IpPath{*ipPath, *ipPath, *ipPath},
					[][]byte{nil, nil, nil},
				)
				return result, err
			},
		},
	}, sharedAddressExitEntries()...)
}

// The ways a public exit, running Reverse of the client policy, judges a remote
// client's packet.
func sharedAddressExitEntries() []sharedAddressEntry {
	senderId := NewId()
	return []sharedAddressEntry{
		{
			name:        "Reverse InspectIngress",
			packetCount: 1,
			inspect: func(policy SecurityPolicy, provideMode protocol.ProvideMode, ipPath *IpPath) (SecurityPolicyResult, error) {
				return Reverse(policy).InspectIngress(provideMode, ipPath, nil)
			},
		},
		{
			name:        "Reverse borrowed",
			packetCount: 1,
			inspect: func(policy SecurityPolicy, provideMode protocol.ProvideMode, ipPath *IpPath) (SecurityPolicyResult, error) {
				return inspectAndRefreshIngressBorrowed(Reverse(policy), provideMode, *ipPath, nil)
			},
		},
		{
			name:        "Reverse for sender",
			packetCount: 1,
			inspect: func(policy SecurityPolicy, provideMode protocol.ProvideMode, ipPath *IpPath) (SecurityPolicyResult, error) {
				return inspectAndRefreshIngressForSenderBorrowed(Reverse(policy), senderId, provideMode, *ipPath, nil)
			},
		},
	}
}

func TestIsSharedAddressSpaceBoundaries(t *testing.T) {
	cases := []struct {
		ip       string
		expected bool
	}{
		{ip: "100.63.255.255", expected: false},
		{ip: "100.64.0.0", expected: true},
		{ip: "100.64.0.1", expected: true},
		{ip: "100.100.100.100", expected: true},
		{ip: "100.127.255.255", expected: true},
		{ip: "100.128.0.0", expected: false},
		{ip: "::ffff:100.64.0.1", expected: true},
		{ip: "::ffff:100.128.0.0", expected: false},
		{ip: "203.0.113.7", expected: false},
		{ip: "10.0.0.1", expected: false},
		// not v4-mapped, so not a v4 address at all
		{ip: "2001:db8::6440:1", expected: false},
		{ip: "64:ff9b::100.64.0.1", expected: false},
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if got := isSharedAddressSpace(ip); got != c.expected {
			t.Errorf("isSharedAddressSpace(%s) = %t, want %t", c.ip, got, c.expected)
		}
		if ip4 := ip.To4(); ip4 != nil {
			if got := isSharedAddressSpace(ip4); got != c.expected {
				t.Errorf("isSharedAddressSpace(4-byte %s) = %t, want %t", c.ip, got, c.expected)
			}
		}
	}
	if isSharedAddressSpace(nil) {
		t.Error("isSharedAddressSpace(nil) = true")
	}
}

// The separate check leaves isPublicUnicast alone: were this range not public
// there, a packet to it would become an Incident and call ReportAbuse.
func TestSharedAddressSpaceStaysPublicUnicast(t *testing.T) {
	for _, ip := range []string{"100.64.0.0", "100.64.0.1", "100.127.255.255", "::ffff:100.64.0.1"} {
		if !isPublicUnicast(net.ParseIP(ip)) {
			t.Errorf("isPublicUnicast(%s) = false, which makes it an Incident", ip)
		}
	}
}

func TestSharedAddressReasonNameAndRange(t *testing.T) {
	AssertEqual(t, SecurityPolicyReasonSharedAddressSpace.String(), "shared-address-space")
	if securityPolicyReasonEnd <= SecurityPolicyReasonSharedAddressSpace {
		t.Fatal("the reason is past securityPolicyReasonEnd, so its counts fall into the unknown bucket")
	}
}

// Off, the default, every path counts the reason and changes nothing else: the
// verdict and the deciding reason are those of the same packet to an address the
// rule does not know, and the result table counts the packet once.
func TestSharedAddressShadowCountsAndKeepsTheVerdict(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	packets := []struct {
		protocol IpProtocol
		port     int
		// the verdict the rules give both addresses
		result SecurityPolicyResult
		reason SecurityPolicyReason
	}{
		{protocol: IpProtocolTcp, port: 22, result: SecurityPolicyResultDrop, reason: SecurityPolicyReasonCfaaDropPort},
		{protocol: IpProtocolUdp, port: 123, result: SecurityPolicyResultAllow, reason: SecurityPolicyReasonCfaaAllow},
		{protocol: IpProtocolTcp, port: 8080, result: SecurityPolicyResultAllow, reason: SecurityPolicyReasonInspecting},
	}
	for _, entry := range sharedAddressEntries() {
		for _, packet := range packets {
			twinPolicy, twinStats := sharedAddressPolicy(ctx, false)
			twinResult, err := entry.inspect(
				twinPolicy,
				protocol.ProvideMode_Public,
				sharedAddressPath(sharedAddressPublicTwinIp, 4, packet.protocol, packet.port),
			)
			if err != nil {
				t.Fatal(err)
			}
			policy, stats := sharedAddressPolicy(ctx, false)
			ipPath := sharedAddressPath(sharedAddressIp, 4, packet.protocol, packet.port)
			result, err := entry.inspect(policy, protocol.ProvideMode_Public, ipPath)
			if err != nil {
				t.Fatal(err)
			}

			if result != packet.result || result != twinResult {
				t.Errorf("%s %s/%d: result %s, twin %s, want %s", entry.name, packet.protocol, packet.port, result, twinResult, packet.result)
			}
			destination := newSecurityDestinationPort(ipPath)
			reasons := stats.Reasons(false)
			if count := reasons[SecurityPolicyReasonSharedAddressSpace][destination]; count != entry.packetCount {
				t.Errorf("%s %s/%d: shared-address count %d, want %d", entry.name, packet.protocol, packet.port, count, entry.packetCount)
			}
			if count := reasons[packet.reason][destination]; count != entry.packetCount {
				t.Errorf("%s %s/%d: deciding reason %s counted %d, want %d; reasons = %v", entry.name, packet.protocol, packet.port, packet.reason, count, entry.packetCount, reasons)
			}
			// the shadow count adds to the reason table alone
			if count := stats.Stats(false)[packet.result][destination]; count != entry.packetCount {
				t.Errorf("%s %s/%d: result counted %d, want %d", entry.name, packet.protocol, packet.port, count, entry.packetCount)
			}
			if twinReasons := twinStats.Reasons(false); 0 < len(twinReasons[SecurityPolicyReasonSharedAddressSpace]) {
				t.Errorf("%s: the public twin counted the shared-address reason", entry.name)
			}
		}
	}
}

// A shadow count names nothing to the client's routing: the decision carries the
// deciding reason, and the shadow reason stays beside it.
func TestSharedAddressShadowLeavesTheDecidingReason(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	policy, _ := sharedAddressPolicy(ctx, false)

	result, decision, err := inspectEgressDetailed(
		policy,
		protocol.ProvideMode_Public,
		sharedAddressPath(sharedAddressIp, 4, IpProtocolTcp, 22),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	AssertEqual(t, result, SecurityPolicyResultDrop)
	AssertEqual(t, decision.reason, SecurityPolicyReasonCfaaDropPort)
	AssertEqual(t, decision.shadowReason, SecurityPolicyReasonSharedAddressSpace)

	result, decision, err = inspectAndRefreshEgressGroupDetailedBorrowed(
		policy,
		protocol.ProvideMode_Public,
		[]IpPath{*sharedAddressPath(sharedAddressIp, 4, IpProtocolUdp, 123)},
		[][]byte{nil},
	)
	if err != nil {
		t.Fatal(err)
	}
	AssertEqual(t, result, SecurityPolicyResultAllow)
	AssertEqual(t, decision.reason, SecurityPolicyReasonCfaaAllow)
	AssertEqual(t, decision.shadowReason, SecurityPolicyReasonSharedAddressSpace)
}

// On, every public-rule path drops with the reason and records no other: the rule
// runs before the static rules and DPI, so a port they would allow changes nothing.
func TestSharedAddressDropOnEveryPublicRulePath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, entry := range sharedAddressEntries() {
		for _, port := range []int{22, 123, 443, 8080} {
			policy, stats := sharedAddressPolicy(ctx, true)
			ipPath := sharedAddressPath(sharedAddressIp, 4, IpProtocolUdp, port)
			result, err := entry.inspect(policy, protocol.ProvideMode_Public, ipPath)
			if err != nil {
				t.Fatal(err)
			}
			if result != SecurityPolicyResultDrop {
				t.Errorf("%s port %d: result %s, want drop", entry.name, port, result)
			}
			destination := newSecurityDestinationPort(ipPath)
			reasons := stats.Reasons(false)
			if count := reasons[SecurityPolicyReasonSharedAddressSpace][destination]; count != entry.packetCount {
				t.Errorf("%s port %d: shared-address count %d, want %d", entry.name, port, count, entry.packetCount)
			}
			if len(reasons) != 1 {
				t.Errorf("%s port %d: reasons %v, want shared-address alone", entry.name, port, reasons)
			}
			results := stats.Stats(false)
			if count := results[SecurityPolicyResultDrop][destination]; count != entry.packetCount {
				t.Errorf("%s port %d: drops counted %d, want %d", entry.name, port, count, entry.packetCount)
			}
			if 0 < len(results[SecurityPolicyResultIncident]) {
				t.Errorf("%s port %d: an incident was counted: %v", entry.name, port, results)
			}
		}
	}
}

func TestSharedAddressDropDecisionCarriesItsReason(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	policy, _ := sharedAddressPolicy(ctx, true)

	result, decision, err := inspectEgressDetailed(
		policy,
		protocol.ProvideMode_Public,
		sharedAddressPath(sharedAddressIp, 4, IpProtocolTcp, 8080),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	AssertEqual(t, result, SecurityPolicyResultDrop)
	AssertEqual(t, decision, securityDecision{reason: SecurityPolicyReasonSharedAddressSpace})

	result, decision, err = inspectAndRefreshEgressGroupDetailedBorrowed(
		policy,
		protocol.ProvideMode_Public,
		[]IpPath{*sharedAddressPath(sharedAddressIp, 4, IpProtocolTcp, 8080)},
		[][]byte{nil},
	)
	if err != nil {
		t.Fatal(err)
	}
	AssertEqual(t, result, SecurityPolicyResultDrop)
	AssertEqual(t, decision, securityDecision{reason: SecurityPolicyReasonSharedAddressSpace})
}

// A v4-mapped v6 destination is judged as the v4 address it carries, in both
// switch positions and on every path.
func TestSharedAddressMappedV6JudgedAsItsV4Address(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mapped := net.ParseIP("::ffff:100.64.0.1")
	for _, entry := range sharedAddressEntries() {
		policy, stats := sharedAddressPolicy(ctx, false)
		ipPath := sharedAddressPath(mapped, 6, IpProtocolTcp, 8080)
		result, err := entry.inspect(policy, protocol.ProvideMode_Public, ipPath)
		if err != nil {
			t.Fatal(err)
		}
		if result != SecurityPolicyResultAllow {
			t.Errorf("%s shadow: result %s, want allow", entry.name, result)
		}
		destination := newSecurityDestinationPort(ipPath)
		if count := stats.Reasons(false)[SecurityPolicyReasonSharedAddressSpace][destination]; count != entry.packetCount {
			t.Errorf("%s shadow: shared-address count %d, want %d", entry.name, count, entry.packetCount)
		}

		policy, stats = sharedAddressPolicy(ctx, true)
		result, err = entry.inspect(policy, protocol.ProvideMode_Public, ipPath)
		if err != nil {
			t.Fatal(err)
		}
		if result != SecurityPolicyResultDrop {
			t.Errorf("%s drop: result %s, want drop", entry.name, result)
		}
		if count := stats.Reasons(false)[SecurityPolicyReasonSharedAddressSpace][destination]; count != entry.packetCount {
			t.Errorf("%s drop: shared-address count %d, want %d", entry.name, count, entry.packetCount)
		}
	}
}

// Just outside the range the rule says nothing, with the drop on or off.
func TestSharedAddressBoundariesAreUntouched(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, ip := range []string{"100.63.255.255", "100.128.0.0"} {
		for _, drop := range []bool{false, true} {
			for _, entry := range sharedAddressEntries() {
				policy, stats := sharedAddressPolicy(ctx, drop)
				ipPath := sharedAddressPath(net.ParseIP(ip).To4(), 4, IpProtocolTcp, 8080)
				result, err := entry.inspect(policy, protocol.ProvideMode_Public, ipPath)
				if err != nil {
					t.Fatal(err)
				}
				if result != SecurityPolicyResultAllow {
					t.Errorf("%s drop=%t %s: result %s, want allow", entry.name, drop, ip, result)
				}
				if counts := stats.Reasons(false)[SecurityPolicyReasonSharedAddressSpace]; 0 < len(counts) {
					t.Errorf("%s drop=%t %s: the shared-address reason was counted", entry.name, drop, ip)
				}
			}
		}
	}
}

// A Network source never reaches the public rules, whose switch this is. Every
// mode that is not Network does, FriendsAndFamily among them.
func TestSharedAddressRuleCoversPublicRelationshipsOnly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, entry := range sharedAddressEntries() {
		policy, stats := sharedAddressPolicy(ctx, true)
		result, err := entry.inspect(policy, protocol.ProvideMode_Network, sharedAddressPath(sharedAddressIp, 4, IpProtocolTcp, 8080))
		if err != nil {
			t.Fatal(err)
		}
		if result != SecurityPolicyResultAllow {
			t.Errorf("%s: a network source got %s, want allow", entry.name, result)
		}
		if counts := stats.Reasons(false)[SecurityPolicyReasonSharedAddressSpace]; 0 < len(counts) {
			t.Errorf("%s: a network source counted the shared-address reason", entry.name)
		}

		for _, provideMode := range []protocol.ProvideMode{
			protocol.ProvideMode_None,
			protocol.ProvideMode_FriendsAndFamily,
			protocol.ProvideMode_Public,
			protocol.ProvideMode_Stream,
		} {
			policy, _ := sharedAddressPolicy(ctx, true)
			result, err := entry.inspect(policy, provideMode, sharedAddressPath(sharedAddressIp, 4, IpProtocolTcp, 8080))
			if err != nil {
				t.Fatal(err)
			}
			if result != SecurityPolicyResultDrop {
				t.Errorf("%s %s: result %s, want drop", entry.name, provideMode, result)
			}
		}
	}
}

// The return direction is another rule's: the ingress mirror reads the static
// drops on the source, and a flow the drop refused never gets a return.
func TestSharedAddressDropLeavesTheReturnPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	policy, _ := sharedAddressPolicy(ctx, true)

	ipPath := &IpPath{
		Version:         4,
		Protocol:        IpProtocolTcp,
		SourceIp:        sharedAddressIp,
		SourcePort:      8080,
		DestinationIp:   securityFixtureSourceIp,
		DestinationPort: 43000,
	}
	result, err := policy.InspectIngress(protocol.ProvideMode_Public, ipPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	AssertEqual(t, result, SecurityPolicyResultAllow)
	result, err = Reverse(policy).InspectEgress(protocol.ProvideMode_Public, ipPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	AssertEqual(t, result, SecurityPolicyResultAllow)
}

// The rule sits beside the public-unicast rule, not inside the CFAA tables, so
// turning those off does not reopen the range.
func TestSharedAddressDropAppliesWithTheStaticPolicyDisabled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultCfaaSecurityPolicySettings()
	settings.Enabled = false
	settings.DropSharedAddressSpace = true
	policy := NewSecurityPolicy(ctx, settings, DefaultDmcaSecurityPolicySettings(), DefaultWebStandardSettings(), DefaultSecurityPolicyStatsCollector())
	for _, entry := range sharedAddressEntries() {
		result, err := entry.inspect(policy, protocol.ProvideMode_Public, sharedAddressPath(sharedAddressIp, 4, IpProtocolTcp, 8080))
		if err != nil {
			t.Fatal(err)
		}
		if result != SecurityPolicyResultDrop {
			t.Errorf("%s: result %s, want drop", entry.name, result)
		}
	}
}

// §22.7's assert in unit form: with the drop on, a public exit refuses overlay
// destinations in 100.64/10 and in RFC 1918, so no overlay flow leaves through it.
// The two refusals differ: only RFC 1918 is an Incident, as it is today.
func TestPublicExitsRefuseOverlayBlocksWithTheDropOn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cases := []struct {
		ip       string
		expected SecurityPolicyResult
	}{
		{ip: "100.64.0.1", expected: SecurityPolicyResultDrop},
		{ip: "100.100.7.9", expected: SecurityPolicyResultDrop},
		{ip: "100.127.255.254", expected: SecurityPolicyResultDrop},
		{ip: "10.0.0.1", expected: SecurityPolicyResultIncident},
		{ip: "10.255.0.9", expected: SecurityPolicyResultIncident},
		{ip: "172.16.0.1", expected: SecurityPolicyResultIncident},
		{ip: "172.31.0.9", expected: SecurityPolicyResultIncident},
		{ip: "192.168.0.1", expected: SecurityPolicyResultIncident},
		{ip: "203.0.113.7", expected: SecurityPolicyResultAllow},
	}
	policy, _ := sharedAddressPolicy(ctx, true)
	for _, c := range cases {
		for _, entry := range sharedAddressExitEntries() {
			for _, ipProtocol := range []IpProtocol{IpProtocolTcp, IpProtocolUdp} {
				ipPath := sharedAddressPath(net.ParseIP(c.ip).To4(), 4, ipProtocol, 8080)
				result, err := entry.inspect(policy, protocol.ProvideMode_Public, ipPath)
				if err != nil {
					t.Fatal(err)
				}
				if result != c.expected {
					t.Errorf("%s %s %s: result %s, want %s", entry.name, ipProtocol, c.ip, result, c.expected)
				}
			}
		}
	}
}

// An exit client that records abuse audits, and a provider running the policy on a
// NAT that dials nothing. A packet the policy passes stops at the dial, which reports
// its address, so no test here sends a packet anywhere.
func newSharedAddressProvider(t *testing.T, ctx context.Context, drop bool) (*RemoteUserNatProvider, *peerAuditRecordingOob, chan string) {
	t.Helper()
	settings := DefaultClientSettings()
	settings.Log = NewNoopLogger()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.SendPeerAudits = true
	// keep the client key registration out of the recorded control
	settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
	oob := &peerAuditRecordingOob{}
	client := NewClient(ctx, NewId(), oob, settings)
	t.Cleanup(client.Cancel)

	providerSettings := DefaultRemoteUserNatProviderSettings()
	providerSettings.SecurityPolicyGenerator = func(ctx context.Context, stats *SecurityPolicyStatsCollector) SecurityPolicy {
		cfaa := DefaultCfaaSecurityPolicySettings()
		cfaa.DropSharedAddressSpace = drop
		return Reverse(NewSecurityPolicy(ctx, cfaa, DefaultDmcaSecurityPolicySettings(), DefaultWebStandardSettings(), stats))
	}
	dials := make(chan string, 16)
	refuseDial := &DialContextSettings{
		DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			select {
			case dials <- address:
			default:
			}
			return nil, errors.New("this exit dials nothing")
		},
	}
	natSettings := DefaultLocalUserNatSettings()
	natSettings.TcpBufferSettings.DialContextSettings = refuseDial
	natSettings.UdpBufferSettings.DialContextSettings = refuseDial
	nat := NewLocalUserNat(ctx, "shared-address exit", natSettings)
	t.Cleanup(nat.Close)
	provider := NewRemoteUserNatProvider(client, nat, providerSettings)
	t.Cleanup(provider.Close)
	return provider, oob, dials
}

// Hands the provider one packet from the tunnel, as its receive callback gets it.
func sharedAddressProviderReceive(t *testing.T, provider *RemoteUserNatProvider, source TransferPath, packet []byte) {
	t.Helper()
	frame, err := ipPacketToProviderFrame(packet, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	defer MessagePoolReturn(frame.MessageBytes)
	provider.ClientReceive(source, []*protocol.Frame{frame}, Peer{ProvideMode: protocol.ProvideMode_Public})
}

// The abuse reports the client has sent.
func sharedAddressAbuseReports(oob *peerAuditRecordingOob) int {
	count := 0
	for _, pack := range oob.recordedPacks() {
		for _, audit := range pack.audits {
			if audit.Abuse {
				count += 1
			}
		}
	}
	return count
}

// At the provider's real entry, a refused packet to the shared address space
// blocks and reports nobody. The twin is the same packet to RFC 1918, an Incident,
// which reports one peer: it shows this rig would see a report. A report reaches
// the control synchronously, inside ClientReceive.
func TestProviderDropsSharedAddressWithoutReportingAbuse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	provider, oob, dials := newSharedAddressProvider(t, ctx, true)
	source := SourceId(NewId())

	flows := []*IpPath{
		sharedAddressPath(sharedAddressIp, 4, IpProtocolTcp, 8080),
		sharedAddressPath(sharedAddressIp, 4, IpProtocolUdp, 8080),
		sharedAddressPath(net.ParseIP("::ffff:100.64.0.1"), 6, IpProtocolUdp, 8080),
	}
	for _, flow := range flows {
		if flow.Protocol == IpProtocolTcp {
			sharedAddressProviderReceive(t, provider, source, ipOosTcpPacketSequence(flow, tcpFlagSyn, 0x5eed, nil))
		} else {
			sharedAddressProviderReceive(t, provider, source, ipOosUdpPacket(flow, []byte("query")))
		}
	}
	AssertEqual(t, provider.PacketStats().BlockIngressPacketCount, int64(len(flows)))
	AssertEqual(t, sharedAddressAbuseReports(oob), 0)
	reasons := provider.SecurityPolicyReasons(false)
	var refused uint64
	for _, count := range reasons[SecurityPolicyReasonSharedAddressSpace] {
		refused += count
	}
	AssertEqual(t, refused, uint64(len(flows)))
	if incidents := provider.SecurityPolicyStats(false)[SecurityPolicyResultIncident]; 0 < len(incidents) {
		t.Fatalf("the refused packets counted as incidents: %v", incidents)
	}

	private := sharedAddressPath(net.ParseIP("10.0.0.1").To4(), 4, IpProtocolUdp, 8080)
	sharedAddressProviderReceive(t, provider, source, ipOosUdpPacket(private, []byte("query")))
	if reports := sharedAddressAbuseReports(oob); reports != 1 {
		t.Fatalf("the incident twin reported %d abuse audits, want 1, so this rig reads nothing", reports)
	}
	select {
	case address := <-dials:
		t.Fatalf("a refused packet reached the dial of %s", address)
	default:
	}
}

// With the drop off, the provider's verdict on the same packet is today's: not
// blocked, counted, and on through the NAT to its dial.
func TestProviderShadowCountsSharedAddress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	provider, oob, dials := newSharedAddressProvider(t, ctx, false)

	flow := sharedAddressPath(sharedAddressIp, 4, IpProtocolUdp, 8080)
	sharedAddressProviderReceive(t, provider, SourceId(NewId()), ipOosUdpPacket(flow, []byte("query")))
	AssertEqual(t, provider.PacketStats().BlockIngressPacketCount, int64(0))
	AssertEqual(t, provider.SecurityPolicyReasons(false)[SecurityPolicyReasonSharedAddressSpace][newSecurityDestinationPort(flow)], uint64(1))
	AssertEqual(t, sharedAddressAbuseReports(oob), 0)
	select {
	case address := <-dials:
		AssertEqual(t, address, "100.64.0.1:8080")
	case <-ctx.Done():
		t.Fatal("the packet the policy passed never reached the dial")
	}
}

// The switch defaults off, and off it is absent from the policy's identity, so a
// default provider publishes the hash and rules pin it would without the switch
// (TestSecurityPolicyRulesGenerationPin holds the pin). On, the hash tells it
// apart in both directions, and the rules generation stays that of the build.
func TestSharedAddressDropIdentity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultCfaaSecurityPolicySettings()
	if settings.DropSharedAddressSpace {
		t.Fatal("the drop is on by default; that raises SecurityPolicyRulesGeneration")
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("DropSharedAddressSpace")) {
		t.Fatalf("the default settings encode the switch, which moves every default policy's hash: %s", encoded)
	}

	off, _ := sharedAddressPolicy(ctx, false)
	on, _ := sharedAddressPolicy(ctx, true)
	AssertEqual(t, SecurityPolicyHash(off), SecurityPolicyHash(DefaultSecurityPolicy(ctx)))
	if SecurityPolicyHash(on) == SecurityPolicyHash(off) {
		t.Fatal("the hash does not tell a policy with the drop from one without")
	}
	if SecurityPolicyHash(Reverse(on)) == SecurityPolicyHash(Reverse(off)) {
		t.Fatal("the reversed hash does not tell a policy with the drop from one without")
	}
	AssertEqual(t, SecurityPolicyGeneration(on), SecurityPolicyRulesGeneration)
	AssertEqual(t, SecurityPolicyGeneration(Reverse(on)), SecurityPolicyRulesGeneration)
}
