package connect

// Tests that a flow is only ever placed on an exit whose server labels can
// carry it (server FP2-PROTOCOL.md §7.2-§7.7): the merged race field, the
// bounded race's order, auto's udp order, affinity donors, quic rebinds,
// adoption, the no-route replies, and a source inspection of every placement
// site of §7.3.

import (
	"context"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The race field of a selected window (§7.5): the window's exits that carry
// the flow, then the limited window's. A udp flow never sees a limited exit;
// a v6 flow only a limited exit labelled dualstack; and a tcp flow the window
// offers nothing for races the limited window's alone.
func TestRaceFieldMergesTheLimitedWindow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()
	generator := newCapabilityTestGenerator(true, nil)
	quality := capabilityTestWindow(ctx, generator, settings, WindowTypeQuality)
	limited := capabilityTestWindow(ctx, generator, settings, WindowTypeLimited)
	parent := capabilityTestParent(ctx, cancel, generator, settings, map[WindowType]*multiClientWindow{
		WindowTypeQuality: quality,
		WindowTypeLimited: limited,
	})
	byClass := map[*multiClientChannel]string{}
	add := func(window *multiClientWindow, windowType WindowType, name string, capabilities ProviderCapabilities) {
		client := capabilityTestChannel(t, ctx, settings, windowType, capabilities)
		window.clients[client.ClientId()] = client
		byClass[client] = name
	}
	add(quality, WindowTypeQuality, "A", capabilityClassA)
	add(quality, WindowTypeQuality, "B", capabilityClassB)
	add(limited, WindowTypeLimited, "C", capabilityClassC)
	add(limited, WindowTypeLimited, "D", capabilityClassD)

	classes := func(field []*multiClientChannel) string {
		names := []string{}
		for _, client := range field {
			names = append(names, byClass[client])
		}
		// the window's own come first, each part in its own order
		quality := []string{}
		limited := []string{}
		for _, name := range names {
			if name == "A" || name == "B" {
				if 0 < len(limited) {
					return "limited before quality: " + strings.Join(names, "")
				}
				quality = append(quality, name)
			} else {
				limited = append(limited, name)
			}
		}
		slices.Sort(quality)
		slices.Sort(limited)
		return strings.Join(quality, "") + "|" + strings.Join(limited, "")
	}
	cases := []struct {
		need flowNeed
		want string
	}{
		{need: flowNeed{ipVersion: 4, ipProtocol: IpProtocolTcp}, want: "AB|CD"},
		{need: flowNeed{ipVersion: 6, ipProtocol: IpProtocolTcp}, want: "A|C"},
		{need: flowNeed{ipVersion: 4, ipProtocol: IpProtocolUdp}, want: "AB|"},
		{need: flowNeed{ipVersion: 6, ipProtocol: IpProtocolUdp}, want: "A|"},
		{need: flowNeed{ipVersion: 4, ipProtocol: IpProtocolIcmp}, want: "AB|CD"},
	}
	for _, c := range cases {
		var offerChanged, limitedOfferChanged <-chan struct{}
		field := parent.raceField(quality, c.need, &offerChanged, &limitedOfferChanged)
		if got := classes(field); got != c.want {
			t.Errorf("v%d protocol %d: field %s, want %s", c.need.ipVersion, c.need.ipProtocol, got, c.want)
		}
		if offerChanged == nil || limitedOfferChanged == nil {
			t.Errorf("v%d protocol %d: the formation wait was not armed on both windows", c.need.ipVersion, c.need.ipProtocol)
		}
		if got := classes(parent.crossTierRaceField(quality, c.need)); got != c.want {
			t.Errorf("v%d protocol %d: cross-tier field %s, want %s", c.need.ipVersion, c.need.ipProtocol, got, c.want)
		}
		if got := classes(parent.lastResortRaceField(quality, c.need)); got != c.want {
			t.Errorf("v%d protocol %d: last-resort field %s, want %s", c.need.ipVersion, c.need.ipProtocol, got, c.want)
		}
	}

	// the window offers nothing a v6 tcp flow can use: the limited window's
	// dualstack exit races alone
	for client, name := range byClass {
		if name == "A" {
			delete(quality.clients, client.ClientId())
		}
	}
	if got := classes(parent.raceField(quality, flowNeed{ipVersion: 6, ipProtocol: IpProtocolTcp}, nil, nil)); got != "|C" {
		t.Errorf("v6 tcp with nothing in quality: field %s, want the limited dualstack exit alone", got)
	}
	if got := classes(parent.raceField(quality, flowNeed{ipVersion: 6, ipProtocol: IpProtocolUdp}, nil, nil)); got != "|" {
		t.Errorf("v6 udp with nothing compatible: field %s, want none", got)
	}
}

// A bounded race spends its slots on the normal windows' candidates first
// (§7.5), and keeps held clients last within each part.
func TestLimitedClientsLastOrdersTheBoundedRace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()
	normal1 := capabilityTestChannel(t, ctx, settings, WindowTypeQuality, capabilityClassB)
	normal2 := capabilityTestChannel(t, ctx, settings, WindowTypeSpeed, capabilityClassB)
	limited1 := capabilityTestChannel(t, ctx, settings, WindowTypeLimited, capabilityClassD)
	limited2 := capabilityTestChannel(t, ctx, settings, WindowTypeLimited, capabilityClassC)

	field := []*multiClientChannel{limited1, normal1, limited2, normal2}
	ordered := limitedClientsLast(field)
	if !slices.Equal(ordered, []*multiClientChannel{normal1, normal2, limited1, limited2}) {
		t.Error("the limited window's candidates did not follow the normal windows' in their own order")
	}
	if !slices.Equal(field, []*multiClientChannel{limited1, normal1, limited2, normal2}) {
		t.Error("the caller's field was reordered in place")
	}
	normals := []*multiClientChannel{normal1, normal2}
	if got := limitedClientsLast(normals); &got[0] != &normals[0] {
		t.Error("a field without a limited exit was copied")
	}
	// a two-way bound now dials the normal windows' two
	if width := multiRaceClientCount(len(ordered), 0, 2, true); !slices.Equal(ordered[:width], normals) {
		t.Error("a degraded race of two does not dial the normal windows' candidates")
	}

	// the send path orders the field this way before it bounds the race
	source, err := readSource("ip_remote_multi_client.go")
	if err != nil {
		t.Fatal(err)
	}
	body, ok := packetGroupSendAttemptBody(source)
	if !ok {
		t.Fatal("could not find the packet-group attempt")
	}
	order := strings.Index(body, "orderedClients = limitedClientsLast(contractRateLimitHeldLast(orderedClients, raceTime))")
	bound := strings.Index(body, "raceOrderedClients := orderedClients[:multiRaceClientCount(")
	if order < 0 || bound < 0 || bound < order {
		t.Error("the race is bounded before the limited window's candidates are ordered last")
	}
}

// In auto, udp of any port goes to quality first, then to speed (D11.4);
// https and streaming stay quality first and other tcp speed first. Before
// FP2, udp other than 443 went speed first.
func TestAutoRoutesUdpQualityFirst(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()
	parent := capabilityTestParent(ctx, cancel, newCapabilityTestGenerator(true, nil), settings, map[WindowType]*multiClientWindow{})
	cases := []struct {
		protocol IpProtocol
		port     int
		want     []WindowType
	}{
		{protocol: IpProtocolUdp, port: 53, want: []WindowType{WindowTypeQuality, WindowTypeSpeed}},
		{protocol: IpProtocolUdp, port: 3478, want: []WindowType{WindowTypeQuality, WindowTypeSpeed}},
		{protocol: IpProtocolUdp, port: 443, want: []WindowType{WindowTypeQuality, WindowTypeSpeed}},
		{protocol: IpProtocolTcp, port: 443, want: []WindowType{WindowTypeQuality, WindowTypeSpeed}},
		{protocol: IpProtocolTcp, port: 22, want: []WindowType{WindowTypeSpeed, WindowTypeQuality}},
	}
	for _, c := range cases {
		path := capabilityTestPath(4, c.protocol, 41000)
		path.DestinationPort = c.port
		if got := parent.selectWindowTypes(&parsedPacket{ipPath: path}, ""); !slices.Equal(got, c.want) {
			t.Errorf("protocol %d port %d: windows %v, want %v", c.protocol, c.port, got, c.want)
		}
	}
	// a fixed profile routes everything through its window
	parent.config.Store(&multiClientConfig{performanceProfile: &PerformanceProfile{WindowType: WindowTypeSpeed, WindowSize: WindowSizeSettings{WindowSizeMin: 2, WindowSizeMax: 2}}})
	if got := parent.selectWindowTypes(&parsedPacket{ipPath: capabilityTestPath(4, IpProtocolUdp, 41001)}, ""); !slices.Equal(got, []WindowType{WindowTypeSpeed}) {
		t.Errorf("fixed speed profile: windows %v, want speed alone", got)
	}
}

// A quic flow never inherits the tcp exit its site's tcp flows share (§7.6):
// it races for a compatible exit and egresses from another address. A tcp
// flow of the same site still inherits it.
func TestUdpFlowNeverInheritsATcpDonor(t *testing.T) {
	for _, donorCapabilities := range []ProviderCapabilities{capabilityClassD, capabilityClassB} {
		parent := bindFlowTestParent()
		parent.ip4PathUpdates = map[Ip4Path]*multiClientChannelUpdate{}
		donor := bindFlowTestChannel(parent)
		donor.args = &multiClientChannelArgs{DestinationStats: DestinationStats{
			IpFamily:     donorCapabilities.IpFamily(),
			Capabilities: donorCapabilities,
		}}

		donorPath := capabilityTestPath(4, IpProtocolTcp, 42000)
		donorUpdate := newMultiClientChannelUpdate(context.Background(), donorPath)
		donorUpdate.selectAffinityClient(donor)
		parent.bindClientFlow(donorUpdate, donor)
		donorKey := donorPath.ToIp4Path()
		parent.ip4PathUpdates[donorKey] = donorUpdate
		donorPaths := map[Ip4Path]time.Time{donorKey: time.Now()}

		inherit := func(path *IpPath) *multiClientChannel {
			update := newMultiClientChannelUpdate(context.Background(), path)
			defer update.Close()
			parent.stateLock.Lock()
			parent.inheritAffinityClient4WithLock(update, donorPaths)
			parent.stateLock.Unlock()
			return update.client.Load()
		}
		carriesUdp := donorCapabilities.CarriesUdp()
		if got := inherit(capabilityTestPath(4, IpProtocolUdp, 42001)); (got == donor) != carriesUdp {
			t.Errorf("donor labelled %q: a quic flow inherited %t, want %t", donorCapabilities.Protocol, got == donor, carriesUdp)
		}
		if got := inherit(capabilityTestPath(4, IpProtocolTcp, 42002)); got != donor {
			t.Errorf("donor labelled %q: a tcp flow of the site did not inherit it", donorCapabilities.Protocol)
		}
		donorUpdate.Close()
	}
}

// A dying exit's quic flows rebind only to an exit labelled to carry udp
// (§7.6, carriesUpdates): with a tcp exit as the only candidate they are
// left for the teardown rather than moved into an exit that drops them.
func TestQuicRebindSkipsTcpExits(t *testing.T) {
	parent := bindFlowTestParent()
	labelled := func(capabilities ProviderCapabilities) *multiClientChannel {
		client := bindFlowTestChannel(parent)
		client.args = &multiClientChannelArgs{DestinationStats: DestinationStats{
			IpFamily:     capabilities.IpFamily(),
			Capabilities: capabilities,
		}}
		return client
	}
	dying := labelled(capabilityClassB)
	tcpExit := labelled(capabilityClassD)
	udpExit := labelled(capabilityClassB)
	quic := newMultiClientChannelUpdate(context.Background(), capabilityTestPath(4, IpProtocolUdp, 43000))
	defer quic.Close()
	quic.selectAffinityClient(dying)

	parent.stateLock.Lock()
	_, _, unplaced := parent.rebindFlowsWithLock(dying, []*multiClientChannelUpdate{quic}, []*multiClientChannel{tcpExit})
	parent.stateLock.Unlock()
	if len(unplaced) != 1 || quic.client.Load() == tcpExit {
		t.Fatal("a quic flow was rebound to a tcp exit")
	}

	parent.stateLock.Lock()
	rebounds, _, unplaced := parent.rebindFlowsWithLock(dying, []*multiClientChannelUpdate{quic}, []*multiClientChannel{tcpExit, udpExit})
	parent.stateLock.Unlock()
	if len(rebounds) != 1 || len(unplaced) != 0 || quic.client.Load() != udpExit {
		t.Error("a quic flow was not rebound to the exit labelled dualproto")
	}
}

// An exit labelled tcp still adopts the unbound tcp flow that it answered:
// tcp follows every protocol label.
func TestUnboundTcpFlowIsAdoptedByAnExitLabelledTcp(t *testing.T) {
	path := unboundFlowTcpPath(51700)
	parent, exit, update, delivered := unboundFlowFixture(t, path)
	exit.args = &multiClientChannelArgs{DestinationStats: DestinationStats{
		IpFamily:     capabilityClassD.IpFamily(),
		Capabilities: capabilityClassD,
	}}
	if !unboundFlowAnswer(t, parent, exit, delivered, reraceSynAck(path)) || update.client.Load() != exit {
		t.Error("an exit labelled tcp did not adopt the tcp flow it answered")
	}
}

// The availability verdicts read the labels (§7.7): udp needs an exit
// labelled dualproto anywhere, and a flow is unroutable only once a normal
// window has formed; the limited window never counts toward formation, and
// its dualstack exit counts toward ipv6.
func TestUnroutableVerdictsReadTheLabels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()
	generator := newCapabilityTestGenerator(true, nil)
	quality := capabilityTestWindow(ctx, generator, settings, WindowTypeQuality)
	limited := capabilityTestWindow(ctx, generator, settings, WindowTypeLimited)
	parent := capabilityTestParent(ctx, cancel, generator, settings, map[WindowType]*multiClientWindow{
		WindowTypeQuality: quality,
		WindowTypeLimited: limited,
	})
	add := func(window *multiClientWindow, windowType WindowType, capabilities ProviderCapabilities) *multiClientChannel {
		client := capabilityTestChannel(t, ctx, settings, windowType, capabilities)
		window.clients[client.ClientId()] = client
		return client
	}
	udp4 := flowNeed{ipVersion: 4, ipProtocol: IpProtocolUdp}
	udp6 := flowNeed{ipVersion: 6, ipProtocol: IpProtocolUdp}
	selected := []WindowType{WindowTypeQuality, WindowTypeSpeed}

	// only the limited window holds an exit: still forming
	add(limited, WindowTypeLimited, capabilityClassC)
	if parent.UdpUnroutable() || parent.flowUnroutable(udp4, selected) || parent.Ipv6Unroutable() {
		t.Error("an exit in the limited window alone made the windows formed")
	}
	if !parent.Ipv6Available() || parent.UdpAvailable() {
		t.Error("a limited dualstack tcp exit: want ipv6 available and udp not")
	}

	// formed with a tcp exit: udp is unroutable, v6 tcp is not
	add(quality, WindowTypeQuality, capabilityClassD)
	if !parent.UdpUnroutable() || !parent.flowUnroutable(udp4, selected) || !parent.flowUnroutable(udp6, selected) {
		t.Error("formed without a dualproto exit, udp is not unroutable")
	}
	if parent.flowUnroutable(flowNeed{ipVersion: 6, ipProtocol: IpProtocolTcp}, selected) || parent.Ipv6Unroutable() {
		t.Error("v6 tcp is unroutable while the limited window holds a dualstack exit")
	}

	// a dualproto exit that cannot carry v6: v4 udp routes, v6 udp still not
	ipv4Udp := add(quality, WindowTypeQuality, capabilityClassB)
	if parent.UdpUnroutable() || parent.flowUnroutable(udp4, selected) || !parent.flowUnroutable(udp6, selected) {
		t.Error("a class B exit: want v4 udp routable and v6 udp unroutable")
	}
	// a warned exit carries no new flow
	ipv4Udp.setWarning(true, warnUnhealthy)
	if !parent.flowUnroutable(udp4, selected) {
		t.Error("a warned dualproto exit kept udp routable")
	}
	add(quality, WindowTypeQuality, capabilityClassA)
	if parent.flowUnroutable(udp6, selected) || parent.flowUnroutable(udp4, selected) {
		t.Error("a class A exit left udp unroutable")
	}
}

// Once the windows have formed, a udp flow no window holds an exit labelled
// to carry is answered with an icmp port unreachable as if from the
// destination, and counted (§7.7); while they are still forming it waits like
// any flow.
func TestUdpNoRouteReply(t *testing.T) {
	multiClient, _, closeParent := groupTestParent(t, DisableSecurityPolicy())
	defer closeParent()
	ctx := multiClient.ctx
	multiClient.settings.WindowClientStats = NewWindowClientStats()

	var mutex sync.Mutex
	replies := []*receivePacket{}
	multiClient.SetReceivePacketCallback(func(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {
		mutex.Lock()
		defer mutex.Unlock()
		replies = append(replies, &receivePacket{IpPath: ipPath, Packet: slices.Clone(packet)})
	})
	multiClient.groupRaceCandidatesForTest = func(group *parsedPacketGroup) []*multiClientChannel {
		return nil
	}
	replyCount := func() int {
		mutex.Lock()
		defer mutex.Unlock()
		return len(replies)
	}
	egress := capabilityTestPath(4, IpProtocolUdp, 44000)
	egress.DestinationPort = 443
	send := func() {
		t.Helper()
		packet := MessagePoolCopy(ipOosUdpPacket(egress, []byte{0xc0, 0x00, 0x00, 0x00, 0x01}))
		group := requireGroupTestPacketGroup(t, packet)
		multiClient.sendPacketGroup(SourceId(NewId()), protocol.ProvideMode_Network, group, 50*time.Millisecond)
	}

	// still forming: no exit at all, no reply
	send()
	if replyCount() != 0 {
		t.Fatal("a udp flow was answered while the windows were still forming")
	}

	// formed with a tcp exit: one port unreachable carrying the flow
	window := capabilityTestWindow(ctx, newCapabilityTestGenerator(true, nil), multiClient.settings, WindowTypeQuality)
	tcpOnly := capabilityTestChannel(t, ctx, multiClient.settings, WindowTypeQuality, capabilityClassD)
	window.clients[tcpOnly.ClientId()] = tcpOnly
	multiClient.windows[WindowTypeQuality] = window
	send()
	mutex.Lock()
	got := slices.Clone(replies)
	mutex.Unlock()
	if len(got) != 1 {
		t.Fatalf("replies = %d, want exactly one port unreachable", len(got))
	}
	parsed, ok := ipParseIcmpUnreachable(got[0].Packet)
	if !ok {
		t.Fatal("the reply is not an icmp unreachable")
	}
	if parsed.Protocol != IpProtocolUdp || parsed.ToIp4Path() != egress.ToIp4Path() {
		t.Errorf("reply embeds %+v, want the udp flow %+v", parsed, egress)
	}
	if multiClient.UdpNoRouteCount() != 1 || multiClient.Ipv6NoRouteCount() != 0 {
		t.Errorf("udp no-route %d, ipv6 no-route %d, want 1 and 0", multiClient.UdpNoRouteCount(), multiClient.Ipv6NoRouteCount())
	}
	noCompatible := uint64(0)
	multiClient.settings.WindowClientStats.VisitNoCompatible(func(ipVersion string, protocol string, count uint64) {
		if ipVersion == "4" && protocol == "udp" {
			noCompatible = count
		}
	})
	if noCompatible != 1 {
		t.Errorf("no-compatible counter %d, want 1", noCompatible)
	}

	// an exit labelled dualproto exists: no reply
	dualproto := capabilityTestChannel(t, ctx, multiClient.settings, WindowTypeQuality, capabilityClassB)
	window.stateLock.Lock()
	window.clients[dualproto.ClientId()] = dualproto
	window.stateLock.Unlock()
	send()
	if replyCount() != 1 {
		t.Error("a udp flow was answered while an exit labelled dualproto exists")
	}
}

// --- every placement site of §7.3 ---

// Every site that places, adopts, rebinds or donates a flow reads the flow's
// labels, never the ip version alone, and the B5 latch is gone. Read the
// sources rather than trusting each call site.
func TestEveryPlacementSiteReadsTheFlowLabels(t *testing.T) {
	sites := []struct {
		file      string
		function  string
		contains  []string
		rationale string
	}{
		{file: "ip_remote_multi_client.go", function: "func (self *RemoteUserNatMultiClient) raceCandidates(", contains: []string{"window.orderedClients(false, need, changed...)", "window.orderedClientsCrossTierForFlow(need)", "window.lastResortClientsForFlow(need)"}, rationale: "raceCandidates"},
		{file: "ip_remote_multi_client_limited.go", function: "func (self *RemoteUserNatMultiClient) raceField(", contains: []string{"self.raceCandidates(window, need, offerChanged)", "self.raceCandidates(limitedWindow, need, limitedOfferChanged)"}, rationale: "the merged race field"},
		{file: "ip_remote_multi_client.go", function: "func (self *multiClientWindow) orderedClients(", contains: []string{"client.supportsFlow(need.ipVersion, need.ipProtocol)"}, rationale: "orderedClients"},
		{file: "ip_remote_multi_client.go", function: "func (self *multiClientWindow) OrderedClientsForFlow(", contains: []string{"self.orderedClients(false, need)"}, rationale: "OrderedClientsForFlow"},
		{file: "ip_remote_multi_client.go", function: "func (self *multiClientWindow) orderedClientsCrossTierForFlow(", contains: []string{"self.orderedClients(true, need)"}, rationale: "orderedClientsCrossTierForFlow"},
		{file: "ip_remote_multi_client.go", function: "func (self *multiClientWindow) lastResortClientsForFlow(", contains: []string{"client.supportsFlow(need.ipVersion, need.ipProtocol)"}, rationale: "lastResortClientsForFlow"},
		{file: "ip_remote_multi_client.go", function: "func (self *RemoteUserNatMultiClient) inheritAffinityClient4WithLock(", contains: []string{"c.supportsFlow(4, flowNeedOf(update.ipPath).ipProtocol)"}, rationale: "the v4 affinity donor"},
		{file: "ip_remote_multi_client.go", function: "func (self *RemoteUserNatMultiClient) inheritAffinityClient6WithLock(", contains: []string{"c.supportsFlow(6, flowNeedOf(update.ipPath).ipProtocol)"}, rationale: "the v6 affinity donor"},
		{file: "ip_remote_multi_client.go", function: "func (self *RemoteUserNatMultiClient) sendUpdate(", contains: []string{"donor.supportsFlow(4, ipPath.Protocol)", "donor.supportsFlow(6, ipPath.Protocol)"}, rationale: "the app pin donors"},
		{file: "ip_remote_multi_client.go", function: "func (self *RemoteUserNatMultiClient) rebindCandidates(", contains: []string{"for _, window := range self.windows {"}, rationale: "rebind candidates from every window, the limited one included"},
		{file: "ip_remote_multi_client.go", function: "func (self *RemoteUserNatMultiClient) rebindFlowsWithLock(", contains: []string{"c.supportsIpPath(update.ipPath)"}, rationale: "carriesUpdates"},
		{file: "ip_remote_multi_client.go", function: "func (self *RemoteUserNatMultiClient) clientReceivePacketResolve(", contains: []string{"sourceClient.supportsIpPath(adoptUpdate.ipPath)"}, rationale: "adoption"},
		{file: "ip_remote_multi_client_affinity_syn.go", function: "func (self *RemoteUserNatMultiClient) reuseAffinitySyn(", contains: []string{"client.supportsIpPath(path)"}, rationale: "syn affinity reuse"},
		{file: "ip_remote_multi_client_affinity_time.go", function: "func (self *RemoteUserNatMultiClient) adoptAffinityClientWithLock(", contains: []string{"client.supportsIpPath(update.ipPath)"}, rationale: "time affinity adoption"},
		{file: "ip_remote_multi_client_dns_affinity.go", function: "func (self *RemoteUserNatMultiClient) inheritDnsExitHintWithLock(", contains: []string{"client.supportsIpPath(ipPath)"}, rationale: "the dns exit hint"},
		{file: "ip_remote_multi_client_service_refresh.go", function: "func (self *RemoteUserNatMultiClient) requestServiceRefresh(", contains: []string{"client.supportsIpPath(path)", "victim.supportsIpPath(path)", "windowTypes = append(windowTypes, WindowTypeLimited)"}, rationale: "the service refresh"},
		{file: "ip_remote_multi_client_syn_retry_publication.go", function: "func (self *RemoteUserNatMultiClient) enqueuePublishedSynOwner(", contains: []string{"client.supportsIpPath(update.ipPath)"}, rationale: "the published syn owner"},
		{file: "ip_remote_multi_client_discovery_demand.go", function: "func (self *multiClientWindow) requestDiscoveryDemand(", contains: []string{"client.supportsFlow(need.ipVersion, need.ipProtocol)"}, rationale: "discovery demand"},
		{file: "ip_remote_multi_client_prober.go", function: "func (self *RemoteUserNatMultiClient) probeProviderPass(", contains: []string{"client.supportsFlow(ipVersion, IpProtocolTcp)"}, rationale: "the probe family"},
		{file: "ip_remote_multi_client_family.go", function: "func (self *multiClientWindow) flowAvailable(", contains: []string{"client.supportsFlow(need.ipVersion, need.ipProtocol)"}, rationale: "ipv6 and udp availability"},
		{file: "ip_remote_multi_client_family.go", function: "func (self *multiClientWindow) selectFamilySwapVictim(", contains: []string{"client.supportsFlow(6, IpProtocolUnknown)"}, rationale: "the family swap victim"},
		{file: "ip_remote_multi_client_family.go", function: "func candidatesForFlow(", contains: []string{"client.supportsFlow(need.ipVersion, need.ipProtocol)"}, rationale: "candidatesForFlow"},
	}
	for _, site := range sites {
		source, err := readSource(site.file)
		if err != nil {
			t.Fatal(err)
		}
		body, ok := functionBody(source, site.function)
		if !ok {
			t.Errorf("%s: could not find %s", site.rationale, site.function)
			continue
		}
		for _, contains := range site.contains {
			if !strings.Contains(body, contains) {
				t.Errorf("%s does not read the flow's labels: missing %q", site.rationale, contains)
			}
		}
	}

	// the send path assembles every selected window's field with the limited
	// window merged
	source, err := readSource("ip_remote_multi_client.go")
	if err != nil {
		t.Fatal(err)
	}
	body, ok := packetGroupSendAttemptBody(source)
	if !ok {
		t.Fatal("could not find the packet-group attempt")
	}
	for _, contains := range []string{
		"self.raceField(window, need, offerChanged, &limitedOfferChanged)",
		"self.crossTierRaceField(window, need)",
		"self.lastResortRaceField(window, need)",
		"case <-limitedOfferChanged:",
		"self.flowUnroutable(flowNeedOf(ipPath), self.selectWindowTypes(firstPacket, sendPacketGroup.pin.appId))",
	} {
		if !strings.Contains(body, contains) {
			t.Errorf("the send path is missing %q", contains)
		}
	}

	// the local downgrade is gone everywhere (D11.8)
	for _, file := range []string{"ip_remote_multi_client.go", "ip_remote_multi_client_family.go"} {
		source, err := readSource(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, retired := range []string{"maybeDowngradeIpv6", "ipFamilyDowngraded", "noteIpFamilyDowngrade", "ipv6DialStarvedWithLock", "supportsIpVersion("} {
			if strings.Contains(source, retired) {
				t.Errorf("%s still names %s", file, retired)
			}
		}
	}

	// every window client is built under its own window's profile, so a
	// limited client runs EncryptionModeRequired under post-quantum (§12.5)
	source, err = readSource("ip_remote_multi_client.go")
	if err != nil {
		t.Fatal(err)
	}
	body, ok = functionBody(source, "func (self *multiClientWindow) expand(")
	if !ok {
		t.Fatal("could not find expand")
	}
	if !strings.Contains(body, "self.performanceProfile,") || !strings.Contains(body, "args.windowType = self.windowType") {
		t.Error("expand does not build each window's clients under that window's profile and type")
	}
}

// A quic flow never inherits the tcp exit its DoH lookup went through
// (§7.6): the hint places only a flow the exit's labels carry, so a tcp flow
// to the same address still inherits it.
func TestDnsExitHintSkipsAnExitThatCannotCarryTheFlow(t *testing.T) {
	parent := dnsAffinityTestParent()
	exit := &multiClientChannel{
		ctx:      context.Background(),
		settings: parent.settings,
		args: &multiClientChannelArgs{DestinationStats: DestinationStats{
			IpFamily:     capabilityClassD.IpFamily(),
			Capabilities: capabilityClassD,
		}},
	}
	address := netip.MustParseAddr("203.0.113.40")
	dohPath := destinationServiceTestPath(4, "192.0.2.53", 443)
	dohUpdate := bindDnsAffinityTestFlow(parent, exit, dohPath)
	defer dohUpdate.Close()
	if !parent.bindDnsResultToExit(dohPath, "media.example.test", []netip.Addr{address}) {
		t.Fatal("the answer was not joined to its DoH exit")
	}
	affinityPaths := []*IpPath{{ServerName: affinityNameForServerName("media.example.test")}}

	quic := destinationServiceTestPath(4, address.String(), 443)
	quic.Protocol = IpProtocolUdp
	quicUpdate := newMultiClientChannelUpdate(context.Background(), quic)
	defer quicUpdate.Close()
	parent.stateLock.Lock()
	bound := parent.inheritDnsExitHintWithLock(quicUpdate, quic, affinityPaths)
	parent.stateLock.Unlock()
	if bound || quicUpdate.client.Load() == exit {
		t.Error("a quic flow inherited the tcp exit its lookup went through")
	}

	tls := destinationServiceTestPath(4, address.String(), 443)
	tlsUpdate := newMultiClientChannelUpdate(context.Background(), tls)
	defer tlsUpdate.Close()
	parent.stateLock.Lock()
	bound = parent.inheritDnsExitHintWithLock(tlsUpdate, tls, affinityPaths)
	parent.stateLock.Unlock()
	if !bound || tlsUpdate.client.Load() != exit {
		t.Error("a tcp flow did not inherit the exit its lookup went through")
	}
}

// The udp verdict reads only the windows the flow may use: under a fixed
// Speed profile a dualproto exit left in the inactive quality window carries
// no new flow, so it does not keep udp routable, and the speed window's
// formation alone counts.
func TestUdpVerdictReadsOnlyTheFlowsWindows(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()
	generator := newCapabilityTestGenerator(true, nil)
	quality := capabilityTestWindow(ctx, generator, settings, WindowTypeQuality)
	speed := capabilityTestWindow(ctx, generator, settings, WindowTypeSpeed)
	parent := capabilityTestParent(ctx, cancel, generator, settings, map[WindowType]*multiClientWindow{
		WindowTypeQuality: quality,
		WindowTypeSpeed:   speed,
	})
	udp := flowNeed{ipVersion: 4, ipProtocol: IpProtocolUdp}
	speedOnly := []WindowType{WindowTypeSpeed}

	inactive := capabilityTestChannel(t, ctx, settings, WindowTypeQuality, capabilityClassA)
	quality.clients[inactive.ClientId()] = inactive
	if parent.flowUnroutable(udp, speedOnly) {
		t.Error("an unformed speed window answered udp as unroutable")
	}
	tcpOnly := capabilityTestChannel(t, ctx, settings, WindowTypeSpeed, capabilityClassD)
	speed.clients[tcpOnly.ClientId()] = tcpOnly
	if !parent.flowUnroutable(udp, speedOnly) {
		t.Error("the inactive window's dualproto exit kept udp routable under a fixed speed profile")
	}
	if parent.flowUnroutable(udp, []WindowType{WindowTypeQuality, WindowTypeSpeed}) {
		t.Error("in auto, quality's dualproto exit did not keep udp routable")
	}
}
