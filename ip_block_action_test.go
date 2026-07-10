package connect

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"

	"github.com/urnetwork/connect/protocol"
)

func TestBlockActionMatcher(t *testing.T) {
	exactOverride := &BlockActionOverride{
		OverrideId:    NewId(),
		Hosts:         []string{"Example.com "},
		BlockOverride: &BlockOverride{Block: true},
	}
	wildcardOverride := &BlockActionOverride{
		OverrideId:    NewId(),
		Hosts:         []string{"*.tracker.net"},
		BlockOverride: &BlockOverride{Block: true},
	}
	wildcardBaseOverride := &BlockActionOverride{
		OverrideId:    NewId(),
		Hosts:         []string{"**.ads.io"},
		BlockOverride: &BlockOverride{Block: true},
	}
	subnetOverride := &BlockActionOverride{
		OverrideId:    NewId(),
		Hosts:         []string{"10.9.0.0/16"},
		RouteOverride: &RouteOverride{Local: true},
	}
	addrOverride := &BlockActionOverride{
		OverrideId:    NewId(),
		Hosts:         []string{"1.2.3.4"},
		RouteOverride: &RouteOverride{Local: true},
	}

	matcher := newBlockActionMatcher([]*BlockActionOverride{
		exactOverride,
		wildcardOverride,
		wildcardBaseOverride,
		subnetOverride,
		addrOverride,
	})

	match := func(addr string, serverNames ...string) *blockActionMatch {
		m := &blockActionMatch{}
		matcher.matchAddr(m, netip.MustParseAddr(addr), serverNames)
		return m
	}

	// exact host, case and space normalized
	m := match("9.9.9.9", "example.com")
	assert.Equal(t, true, m.blockOverride != nil)
	assert.Equal(t, exactOverride.OverrideId, m.blockOverrideId)
	// exact host does not match subdomains
	assert.Equal(t, false, match("9.9.9.9", "sub.example.com").any())

	// *.h matches subdomains only
	assert.Equal(t, true, match("9.9.9.9", "a.tracker.net").any())
	assert.Equal(t, true, match("9.9.9.9", "A.B.tracker.net").any())
	assert.Equal(t, false, match("9.9.9.9", "tracker.net").any())

	// **.h matches the base and subdomains
	assert.Equal(t, true, match("9.9.9.9", "ads.io").any())
	assert.Equal(t, true, match("9.9.9.9", "cdn.ads.io").any())

	// subnet
	m = match("10.9.42.1")
	assert.Equal(t, true, m.routeOverride != nil)
	assert.Equal(t, subnetOverride.OverrideId, m.routeOverrideId)
	assert.Equal(t, false, match("10.8.0.1").any())

	// exact ip
	assert.Equal(t, true, match("1.2.3.4").any())
	assert.Equal(t, false, match("1.2.3.5").any())

	// no overrides compiles to nil
	assert.Equal(t, true, newBlockActionMatcher(nil) == nil)
}

func TestBlockActionMatchMerge(t *testing.T) {
	blockTrue := &BlockActionOverride{
		OverrideId:    NewId(),
		BlockOverride: &BlockOverride{Block: true},
	}
	blockFalse := &BlockActionOverride{
		OverrideId:    NewId(),
		BlockOverride: &BlockOverride{Block: false},
	}
	localTrue := &BlockActionOverride{
		OverrideId:    NewId(),
		RouteOverride: &RouteOverride{Local: true},
	}
	localFalse := &BlockActionOverride{
		OverrideId:    NewId(),
		RouteOverride: &RouteOverride{Local: false},
	}

	// block=true wins over block=false, in either order
	m := &blockActionMatch{}
	m.merge(blockFalse)
	m.merge(blockTrue)
	assert.Equal(t, true, m.blockOverride.Block)
	assert.Equal(t, blockTrue.OverrideId, m.blockOverrideId)

	m = &blockActionMatch{}
	m.merge(blockTrue)
	m.merge(blockFalse)
	assert.Equal(t, true, m.blockOverride.Block)
	assert.Equal(t, blockTrue.OverrideId, m.blockOverrideId)

	// local=true wins over local=false, in either order
	m = &blockActionMatch{}
	m.merge(localFalse)
	m.merge(localTrue)
	assert.Equal(t, true, m.routeOverride.Local)
	assert.Equal(t, localTrue.OverrideId, m.routeOverrideId)

	m = &blockActionMatch{}
	m.merge(localTrue)
	m.merge(localFalse)
	assert.Equal(t, true, m.routeOverride.Local)
	assert.Equal(t, localTrue.OverrideId, m.routeOverrideId)
}

func TestBlockActionApply(t *testing.T) {
	blockTrue := &blockActionMatch{blockOverride: &BlockOverride{Block: true}}
	blockFalse := &blockActionMatch{blockOverride: &BlockOverride{Block: false}}
	localTrue := &blockActionMatch{routeOverride: &RouteOverride{Local: true}}
	localFalse := &blockActionMatch{routeOverride: &RouteOverride{Local: false}}
	blockFalseLocalFalse := &blockActionMatch{
		blockOverride: &BlockOverride{Block: false},
		routeOverride: &RouteOverride{Local: false},
	}
	blockTrueLocalTrue := &blockActionMatch{
		blockOverride: &BlockOverride{Block: true},
		routeOverride: &RouteOverride{Local: true},
	}

	type applyCase struct {
		r      SecurityPolicyResult
		bypass bool
		match  *blockActionMatch
		block  bool
		local  bool
	}
	cases := []applyCase{
		// defaults with no overrides
		{SecurityPolicyResultAllow, false, nil, false, false},
		{SecurityPolicyResultAllow, true, nil, false, false},
		{SecurityPolicyResultDrop, false, nil, true, false},
		{SecurityPolicyResultDrop, true, nil, false, true},
		{SecurityPolicyResultIncident, true, nil, true, false},
		// block override
		{SecurityPolicyResultAllow, false, blockTrue, true, false},
		{SecurityPolicyResultDrop, true, blockTrue, true, false},
		// un-blocked drop traffic never egresses. it routes local
		{SecurityPolicyResultDrop, false, blockFalse, false, true},
		{SecurityPolicyResultDrop, false, blockFalseLocalFalse, false, true},
		// route override
		{SecurityPolicyResultAllow, false, localTrue, false, true},
		{SecurityPolicyResultDrop, true, localFalse, false, true},
		// block wins over local
		{SecurityPolicyResultAllow, false, blockTrueLocalTrue, true, false},
		// incident is not overridable
		{SecurityPolicyResultIncident, false, blockFalse, true, false},
		{SecurityPolicyResultIncident, false, localTrue, true, false},
	}
	for i, c := range cases {
		block, local := blockActionApply(c.r, c.bypass, c.match)
		assert.Equal(t, c.block, block)
		assert.Equal(t, c.local, local)
		if c.block != block || c.local != local {
			t.Logf("case %d failed: %+v", i, c)
		}
	}
}

func TestBlockActionCollector(t *testing.T) {
	collector := newBlockActionCollector(8)

	assert.Equal(t, false, collector.hasCallbacks())

	var flushed [][]*BlockAction
	unsub := collector.addCallback(func(blockActions []*BlockAction) {
		flushed = append(flushed, blockActions)
	})
	assert.Equal(t, true, collector.hasCallbacks())

	a := netip.MustParseAddr("1.0.0.1")
	b := netip.MustParseAddr("1.0.0.2")
	overrideId := NewId()
	match := &blockActionMatch{
		blockOverride:   &BlockOverride{Block: true},
		blockOverrideId: overrideId,
	}
	// a cluster of two ips. decisions on either member aggregate into one action
	clusterDecision := &blockActionDecision{
		clusterKey:   a,
		clusterIps:   []netip.Addr{a, b},
		clusterHosts: []string{"example.com"},
	}
	otherDecision := &blockActionDecision{
		clusterKey: b,
		clusterIps: []netip.Addr{b},
	}

	// two packets aggregate into one action per key
	collector.add(clusterDecision, true, false, match, 100)
	collector.add(clusterDecision, true, false, match, 50)
	// a different decision for the same cluster is a separate action
	collector.add(clusterDecision, false, false, nil, 25)
	collector.add(otherDecision, false, true, nil, 10)

	collector.flush()
	assert.Equal(t, 1, len(flushed))
	blockActions := flushed[0]
	assert.Equal(t, 3, len(blockActions))

	byKey := map[string]*BlockAction{}
	for _, blockAction := range blockActions {
		byKey[fmt.Sprintf("%s-%t-%t", blockAction.Ips[0], blockAction.Block, blockAction.Local)] = blockAction
	}
	blocked := byKey["1.0.0.1-true-false"]
	assert.Equal(t, 2, blocked.PacketCount)
	assert.Equal(t, ByteCount(150), blocked.ByteCount)
	// the cluster action carries all the cluster ips and hosts
	assert.Equal(t, []netip.Addr{a, b}, blocked.Ips)
	assert.Equal(t, []string{"example.com"}, blocked.Hosts)
	assert.Equal(t, true, blocked.BlockOverrideId != nil)
	assert.Equal(t, overrideId, *blocked.BlockOverrideId)
	assert.Equal(t, true, blocked.RouteOverrideId == nil)

	localAction := byKey["1.0.0.2-false-true"]
	assert.Equal(t, []netip.Addr{b}, localAction.Ips)
	assert.Equal(t, 0, len(localAction.Hosts))

	// the epoch was drained
	collector.flush()
	assert.Equal(t, 1, len(flushed))

	unsub()
	assert.Equal(t, false, collector.hasCallbacks())
}

// a security policy with a fixed egress result
type testingFixedSecurityPolicy struct {
	stats  *SecurityPolicyStatsCollector
	result SecurityPolicyResult
}

func (self *testingFixedSecurityPolicy) Stats() *SecurityPolicyStatsCollector {
	return self.stats
}

func (self *testingFixedSecurityPolicy) InspectEgress(provideMode protocol.ProvideMode, ipPath *IpPath, payload []byte) (SecurityPolicyResult, error) {
	return self.result, nil
}

func (self *testingFixedSecurityPolicy) InspectIngress(provideMode protocol.ProvideMode, ipPath *IpPath, payload []byte) (SecurityPolicyResult, error) {
	return SecurityPolicyResultAllow, nil
}

func (self *testingFixedSecurityPolicy) RefreshEgress(ipPath *IpPath) {
}

func (self *testingFixedSecurityPolicy) RefreshIngress(ipPath *IpPath) {
}

// a generator with no available destinations
type testingEmptyMultiClientGenerator struct {
}

func (self *testingEmptyMultiClientGenerator) NextDestinations(count int, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
	return map[MultiHopId]DestinationStats{}, nil
}

func (self *testingEmptyMultiClientGenerator) NewClientArgs() (*MultiClientGeneratorClientArgs, error) {
	return nil, fmt.Errorf("no clients")
}

func (self *testingEmptyMultiClientGenerator) RemoveClientArgs(args *MultiClientGeneratorClientArgs) {
}

func (self *testingEmptyMultiClientGenerator) RemoveClientWithArgs(client *Client, args *MultiClientGeneratorClientArgs) {
}

func (self *testingEmptyMultiClientGenerator) NewClientSettings() *ClientSettings {
	return DefaultClientSettings()
}

func (self *testingEmptyMultiClientGenerator) NewClient(ctx context.Context, args *MultiClientGeneratorClientArgs, clientSettings *ClientSettings) (*Client, error) {
	return nil, fmt.Errorf("no clients")
}

func (self *testingEmptyMultiClientGenerator) FixedDestinationSize() (int, bool) {
	return 0, false
}

func testingUdp4Packet(sourceIp string, destinationIp string, destinationPort int, payload []byte) []byte {
	ip := &layers.IPv4{
		Version:  4,
		TTL:      64,
		SrcIP:    net.ParseIP(sourceIp).To4(),
		DstIP:    net.ParseIP(destinationIp).To4(),
		Protocol: layers.IPProtocolUDP,
	}
	udp := &layers.UDP{
		SrcPort: layers.UDPPort(40000),
		DstPort: layers.UDPPort(destinationPort),
	}
	udp.SetNetworkLayerForChecksum(ip)
	buffer := gopacket.NewSerializeBuffer()
	err := gopacket.SerializeLayers(
		buffer,
		gopacket.SerializeOptions{ComputeChecksums: true, FixLengths: true},
		ip,
		udp,
		gopacket.Payload(payload),
	)
	if err != nil {
		panic(err)
	}
	packet := make([]byte, len(buffer.Bytes()))
	copy(packet, buffer.Bytes())
	return packet
}

func TestMultiClientBlockActionOverrides(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	securityPolicy := &testingFixedSecurityPolicy{
		stats:  DefaultSecurityPolicyStatsCollector(),
		result: SecurityPolicyResultDrop,
	}

	settings := DefaultMultiClientSettings()
	settings.EventEpoch = 20 * time.Millisecond
	settings.SecurityPolicyGenerator = func(ctx context.Context, stats *SecurityPolicyStatsCollector) SecurityPolicy {
		return securityPolicy
	}

	multiClient := NewRemoteUserNatMultiClient(
		ctx,
		&testingEmptyMultiClientGenerator{},
		func(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {
		},
		protocol.ProvideMode_Network,
		settings,
	)
	defer multiClient.Close()

	source := SourceId(NewId())

	blockActionsChannel := make(chan []*BlockAction, 16)
	unsub := multiClient.AddBlockActionCallback(func(blockActions []*BlockAction) {
		blockActionsChannel <- blockActions
	})
	defer unsub()

	nextBlockActions := func() []*BlockAction {
		select {
		case blockActions := <-blockActionsChannel:
			return blockActions
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for block actions")
			return nil
		}
	}

	// drop policy, no bypass, no overrides -> blocked
	packet := testingUdp4Packet("10.0.0.5", "127.0.0.1", 9, []byte("hello"))
	success := multiClient.SendPacket(source, protocol.ProvideMode_Network, packet, 0)
	assert.Equal(t, false, success)

	blockActions := nextBlockActions()
	assert.Equal(t, 1, len(blockActions))
	assert.Equal(t, true, blockActions[0].Block)
	assert.Equal(t, false, blockActions[0].Local)
	assert.Equal(t, true, blockActions[0].BlockOverrideId == nil)
	assert.Equal(t, 1, blockActions[0].PacketCount)

	packetStats := multiClient.PacketStats()
	assert.Equal(t, int64(1), packetStats.BlockEgressPacketCount)
	assert.Equal(t, ByteCount(len(packet)), packetStats.BlockEgressByteCount)

	// an un-block override routes the drop-classified traffic locally, never egress
	unblockOverride := &BlockActionOverride{
		OverrideId:    NewId(),
		Hosts:         []string{"127.0.0.1"},
		BlockOverride: &BlockOverride{Block: false},
	}
	multiClient.SetBlockActionOverrides([]*BlockActionOverride{unblockOverride})

	success = multiClient.SendPacket(source, protocol.ProvideMode_Network, packet, 0)
	assert.Equal(t, true, success)

	blockActions = nextBlockActions()
	assert.Equal(t, 1, len(blockActions))
	assert.Equal(t, false, blockActions[0].Block)
	assert.Equal(t, true, blockActions[0].Local)
	assert.Equal(t, true, blockActions[0].BlockOverrideId != nil)
	assert.Equal(t, unblockOverride.OverrideId, *blockActions[0].BlockOverrideId)

	packetStats = multiClient.PacketStats()
	assert.Equal(t, int64(1), packetStats.LocalEgressPacketCount)
	assert.Equal(t, int64(1), packetStats.BlockEgressPacketCount)

	// with bypass on, a block override blocks traffic that would route local
	multiClient.SetLocalSecurityBypass(true)
	blockOverride := &BlockActionOverride{
		OverrideId:    NewId(),
		Hosts:         []string{"127.0.0.0/8"},
		BlockOverride: &BlockOverride{Block: true},
	}
	multiClient.SetBlockActionOverrides([]*BlockActionOverride{blockOverride})

	success = multiClient.SendPacket(source, protocol.ProvideMode_Network, packet, 0)
	assert.Equal(t, false, success)

	blockActions = nextBlockActions()
	assert.Equal(t, true, blockActions[0].Block)
	assert.Equal(t, blockOverride.OverrideId, *blockActions[0].BlockOverrideId)

	// packet stats listener fires on the epoch with the cumulative counts
	packetStatsChannel := make(chan *PacketStats, 16)
	unsubPacketStats := multiClient.AddPacketStatsCallback(func(packetStats *PacketStats) {
		packetStatsChannel <- packetStats
	})
	defer unsubPacketStats()

	multiClient.SetBlockActionOverrides(nil)
	success = multiClient.SendPacket(source, protocol.ProvideMode_Network, packet, 0)
	assert.Equal(t, true, success)

	select {
	case packetStats = <-packetStatsChannel:
		assert.Equal(t, int64(2), packetStats.LocalEgressPacketCount)
		assert.Equal(t, int64(2), packetStats.BlockEgressPacketCount)
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for packet stats")
	}
}

func TestMultiClientBlockActionIgnoreHosts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	securityPolicy := &testingFixedSecurityPolicy{
		stats:  DefaultSecurityPolicyStatsCollector(),
		result: SecurityPolicyResultDrop,
	}

	settings := DefaultMultiClientSettings()
	settings.EventEpoch = 20 * time.Millisecond
	settings.SecurityPolicyGenerator = func(ctx context.Context, stats *SecurityPolicyStatsCollector) SecurityPolicy {
		return securityPolicy
	}

	multiClient := NewRemoteUserNatMultiClient(
		ctx,
		&testingEmptyMultiClientGenerator{},
		func(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {
		},
		protocol.ProvideMode_Network,
		settings,
	)
	defer multiClient.Close()

	source := SourceId(NewId())

	blockActionsChannel := make(chan []*BlockAction, 16)
	unsub := multiClient.AddBlockActionCallback(func(blockActions []*BlockAction) {
		blockActionsChannel <- blockActions
	})
	defer unsub()

	nextBlockActions := func() []*BlockAction {
		select {
		case blockActions := <-blockActionsChannel:
			return blockActions
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for block actions")
			return nil
		}
	}

	// an ignored destination is excluded from the override logic:
	// the un-block override must not match, so the drop policy blocks
	unblockOverride := &BlockActionOverride{
		OverrideId:    NewId(),
		Hosts:         []string{"127.0.0.1"},
		BlockOverride: &BlockOverride{Block: false},
	}
	multiClient.SetBlockActionOverrides([]*BlockActionOverride{unblockOverride})
	multiClient.SetBlockActionIgnoreHosts([]string{"127.0.0.1"})

	packet := testingUdp4Packet("10.0.0.5", "127.0.0.1", 9, []byte("hello"))
	success := multiClient.SendPacket(source, protocol.ProvideMode_Network, packet, 0)
	assert.Equal(t, false, success)

	// no block action is surfaced for the ignored destination
	select {
	case blockActions := <-blockActionsChannel:
		t.Fatalf("expected no block actions for the ignored destination, got %d", len(blockActions))
	case <-time.After(4 * settings.EventEpoch):
	}

	// the default decision and packet stats still apply
	packetStats := multiClient.PacketStats()
	assert.Equal(t, int64(1), packetStats.BlockEgressPacketCount)

	// clearing the ignore list restores the override match and the block actions
	multiClient.SetBlockActionIgnoreHosts(nil)

	success = multiClient.SendPacket(source, protocol.ProvideMode_Network, packet, 0)
	assert.Equal(t, true, success)

	blockActions := nextBlockActions()
	assert.Equal(t, 1, len(blockActions))
	assert.Equal(t, false, blockActions[0].Block)
	assert.Equal(t, true, blockActions[0].Local)
	assert.Equal(t, true, blockActions[0].BlockOverrideId != nil)

	// ignore by subnet also excludes the destination
	multiClient.SetBlockActionIgnoreHosts([]string{"127.0.0.0/8"})

	success = multiClient.SendPacket(source, protocol.ProvideMode_Network, packet, 0)
	assert.Equal(t, false, success)

	select {
	case blockActions := <-blockActionsChannel:
		t.Fatalf("expected no block actions for the ignored subnet, got %d", len(blockActions))
	case <-time.After(4 * settings.EventEpoch):
	}
}

// TestMultiClientBlockActionDefaultRemoteDohIgnored verifies the built-in
// ignore baseline: the hard coded remote doh resolver ips are excluded from
// override decisions with NO SetBlockActionIgnoreHosts wiring at all, while
// a neighboring non-baseline ip still matches overrides.
func TestMultiClientBlockActionDefaultRemoteDohIgnored(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	securityPolicy := &testingFixedSecurityPolicy{
		stats:  DefaultSecurityPolicyStatsCollector(),
		result: SecurityPolicyResultDrop,
	}

	settings := DefaultMultiClientSettings()
	settings.EventEpoch = 20 * time.Millisecond
	settings.SecurityPolicyGenerator = func(ctx context.Context, stats *SecurityPolicyStatsCollector) SecurityPolicy {
		return securityPolicy
	}

	multiClient := NewRemoteUserNatMultiClient(
		ctx,
		&testingEmptyMultiClientGenerator{},
		func(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {
		},
		protocol.ProvideMode_Network,
		settings,
	)
	defer multiClient.Close()

	source := SourceId(NewId())

	blockActionsChannel := make(chan []*BlockAction, 16)
	unsub := multiClient.AddBlockActionCallback(func(blockActions []*BlockAction) {
		blockActionsChannel <- blockActions
	})
	defer unsub()

	// an un-block override covering the hard coded resolver ips and a
	// non-baseline control ip. note: no SetBlockActionIgnoreHosts call.
	hardCodedIps := []string{"1.1.1.1", "8.8.8.8", "9.9.9.9", "208.67.222.222"}
	controlIp := "1.0.0.1"
	unblockOverride := &BlockActionOverride{
		OverrideId:    NewId(),
		Hosts:         append(append([]string{}, hardCodedIps...), controlIp),
		BlockOverride: &BlockOverride{Block: false},
	}
	multiClient.SetBlockActionOverrides([]*BlockActionOverride{unblockOverride})

	// the baseline ignores the override for every hard coded resolver ip:
	// the drop policy blocks, and no block action is surfaced
	for _, ip := range hardCodedIps {
		packet := testingUdp4Packet("10.0.0.5", ip, 9, []byte("hello"))
		success := multiClient.SendPacket(source, protocol.ProvideMode_Network, packet, 0)
		assert.Equal(t, false, success)
	}
	select {
	case blockActions := <-blockActionsChannel:
		t.Fatalf("expected no block actions for the hard coded resolver ips, got %d", len(blockActions))
	case <-time.After(4 * settings.EventEpoch):
	}

	// the control ip is not in the baseline: the override applies
	packet := testingUdp4Packet("10.0.0.5", controlIp, 9, []byte("hello"))
	success := multiClient.SendPacket(source, protocol.ProvideMode_Network, packet, 0)
	assert.Equal(t, true, success)

	select {
	case blockActions := <-blockActionsChannel:
		assert.Equal(t, 1, len(blockActions))
		assert.Equal(t, false, blockActions[0].Block)
		assert.Equal(t, true, blockActions[0].BlockOverrideId != nil)
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for the control ip block action")
	}
}
