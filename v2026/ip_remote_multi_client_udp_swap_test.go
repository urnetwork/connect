package connect

// Unit tests for udp in the windows that ask for any protocol
// (ip_remote_multi_client_udp_swap.go): the udp demand, the victim, the
// reservation, the single-exit order, and the guards that leave a window that
// asks for dualproto, as every app's does, a window whose udp flows never
// failed, and the user's Fixed IP unchanged. The composed regressions are in
// ip_remote_multi_client_udp_swap_composed_test.go.

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// The selectors the proxy service sets for its hosted devices.
var udpSwapTestHostedSelectors = ProviderSelectors{
	Ip:       ProviderIpCapabilityIpv4,
	Protocol: ProviderProtocolCapabilityAny,
}

// A udp flow without an exit opens the demand of a window that asks for any
// protocol, for UdpSwapDemandHold, and the window has a udp shortfall of one
// only while the demand is open and none of its exits carries udp. A window
// that asks for dualproto, the limited window and a non-positive hold ignore
// it, and the user's Fixed IP has no shortfall.
func TestUdpDemandOpensOnlyForWindowsThatAskForAnyProtocol(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	now := time.Now()

	settings := DefaultMultiClientSettings()
	settings.NormalWindowSelectors = udpSwapTestHostedSelectors
	window := capabilityTestWindow(ctx, nil, settings, WindowTypeQuality)
	if window.udpSwapShortfall(now, false, false, false, 0) != 0 {
		t.Fatal("a udp shortfall before any udp flow failed")
	}
	wake := window.resizeMonitor.NotifyChannel()
	window.noteUdpDemand(now)
	select {
	case <-wake:
	default:
		t.Error("opening the udp demand did not wake the resize pass")
	}
	if !window.udpDemandOpen(now) || window.udpSwapShortfall(now, false, false, false, 0) != 1 {
		t.Fatal("an unroutable udp flow did not open the udp shortfall")
	}
	// an exit that carries udp, a fixed destination, an inactive window or
	// the user's Fixed IP closes it
	if window.udpSwapShortfall(now, false, false, false, 1) != 0 ||
		window.udpSwapShortfall(now, true, false, false, 0) != 0 ||
		window.udpSwapShortfall(now, false, true, false, 0) != 0 ||
		window.udpSwapShortfall(now, false, false, true, 0) != 0 {
		t.Error("a udp shortfall beside an exit that carries udp, a fixed destination, an inactive window or the Fixed IP")
	}
	// it lapses UdpSwapDemandHold after the last udp flow without an exit
	later := now.Add(settings.UdpSwapDemandHold)
	if window.udpDemandOpen(later) || window.udpSwapShortfall(later, false, false, false, 0) != 0 {
		t.Error("the udp demand outlived its hold")
	}

	for name, configure := range map[string]func(*MultiClientSettings, *multiClientWindow){
		"dualproto selectors": func(settings *MultiClientSettings, window *multiClientWindow) {},
		"limited window": func(settings *MultiClientSettings, window *multiClientWindow) {
			settings.NormalWindowSelectors = udpSwapTestHostedSelectors
			window.windowType = WindowTypeLimited
		},
		"no hold": func(settings *MultiClientSettings, window *multiClientWindow) {
			settings.NormalWindowSelectors = udpSwapTestHostedSelectors
			settings.UdpSwapDemandHold = 0
		},
	} {
		settings := DefaultMultiClientSettings()
		window := capabilityTestWindow(ctx, nil, settings, WindowTypeQuality)
		configure(settings, window)
		window.noteUdpDemand(now)
		if window.udpDemandOpen(now) || window.udpSwapShortfall(now, false, false, false, 0) != 0 {
			t.Errorf("%s: an unroutable udp flow opened a udp shortfall", name)
		}
	}
}

// The udp swap gives up the slot of a flowless tcp-only exit, the lightest,
// and never one that carries udp or flows.
func TestSelectUdpSwapVictim(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()
	settings.NormalWindowSelectors = udpSwapTestHostedSelectors
	window := capabilityTestWindow(ctx, nil, settings, WindowTypeQuality)
	flows := map[*multiClientChannel]int{}
	window.flowCountFunc = func(client *multiClientChannel) int { return flows[client] }

	dualproto := capabilityTestChannel(t, ctx, settings, WindowTypeQuality, capabilityClassB)
	heavyTcp := capabilityTestChannel(t, ctx, settings, WindowTypeQuality, capabilityClassD)
	lightTcp := capabilityTestChannel(t, ctx, settings, WindowTypeQuality, capabilityClassC)
	weights := map[*multiClientChannel]float32{dualproto: 1, heavyTcp: 9, lightTcp: 3}
	clients := []*multiClientChannel{dualproto, heavyTcp, lightTcp}

	if victim := window.selectUdpSwapVictim(clients, weights); victim != lightTcp {
		t.Errorf("victim %v, want the lightest flowless tcp-only exit", victim)
	}
	flows[lightTcp] = 2
	if victim := window.selectUdpSwapVictim(clients, weights); victim != heavyTcp {
		t.Errorf("victim %v, want the flowless tcp-only exit", victim)
	}
	flows[heavyTcp] = 5
	if victim := window.selectUdpSwapVictim(clients, weights); victim != nil {
		t.Errorf("victim %v, want none while every tcp-only exit carries flows", victim)
	}
	if victim := window.selectUdpSwapVictim([]*multiClientChannel{dualproto}, weights); victim != nil {
		t.Errorf("victim %v, want none when every exit carries udp", victim)
	}
}

// A udp flow the windows hold no exit for is answered as unroutable, and the
// first window it would use opens its udp demand when it asks for any
// protocol: quality, not speed, in auto. With windows that ask for dualproto
// nothing opens.
func TestUnroutableUdpFlowOpensTheDemandOfItsFirstWindow(t *testing.T) {
	for _, selectors := range []ProviderSelectors{udpSwapTestHostedSelectors, DefaultMultiClientSettings().NormalWindowSelectors} {
		multiClient, _, closeParent := groupTestParent(t, DisableSecurityPolicy())
		ctx := multiClient.ctx
		multiClient.groupRaceCandidatesForTest = func(group *parsedPacketGroup) []*multiClientChannel {
			return nil
		}
		settings := DefaultMultiClientSettings()
		settings.NormalWindowSelectors = selectors
		windows := map[WindowType]*multiClientWindow{}
		for _, windowType := range []WindowType{WindowTypeQuality, WindowTypeSpeed} {
			window := capabilityTestWindow(ctx, newCapabilityTestGenerator(true, nil), settings, windowType)
			tcpOnly := capabilityTestChannel(t, ctx, settings, windowType, capabilityClassD)
			window.clients[tcpOnly.ClientId()] = tcpOnly
			multiClient.windows[windowType] = window
			windows[windowType] = window
		}

		egress := capabilityTestPath(4, IpProtocolUdp, 44001)
		packet := MessagePoolCopy(ipOosUdpPacket(egress, []byte{0xc0, 0x00, 0x00, 0x00, 0x01}))
		multiClient.sendPacketGroup(SourceId(NewId()), protocol.ProvideMode_Network, requireGroupTestPacketGroup(t, packet), 50*time.Millisecond)

		if multiClient.UdpNoRouteCount() != 1 {
			t.Errorf("selectors %+v: udp no-route %d, want the one unroutable flow", selectors, multiClient.UdpNoRouteCount())
		}
		anyProtocol := selectors.Protocol == ProviderProtocolCapabilityAny
		now := time.Now()
		if windows[WindowTypeQuality].udpDemandOpen(now) != anyProtocol || windows[WindowTypeSpeed].udpDemandOpen(now) {
			t.Errorf("selectors %+v: quality demand %t, speed demand %t, want %t and false",
				selectors, windows[WindowTypeQuality].udpDemandOpen(now), windows[WindowTypeSpeed].udpDemandOpen(now), anyProtocol)
		}
		closeParent()
	}
}

// One full window of a hosted device, seeded with tcp-only exits and ready for
// resize passes behind barriers.
func udpSwapTestFullWindow(t *testing.T, size WindowSizeSettings, count int, profile *PerformanceProfile) (*multiClientExpandLifecycleFixture, []*multiClientChannel) {
	t.Helper()
	fixture := newMultiClientExpandLifecycleFixture(t)
	window := fixture.window
	first := <-window.clientChannelArgs
	first.IpFamily, first.Capabilities = IpFamilyV4Only, capabilityClassD
	generator := newIpv6AdmissionGenerator(fixture, first, fixture, first)
	window.generator = generator
	window.settings.NormalWindowSelectors = udpSwapTestHostedSelectors
	window.settings.StandingReserve = false
	window.settings.StrictWindowSizeHardMax = true
	window.settings.WindowSizes[WindowTypeQuality] = size
	window.performanceProfile = profile
	window.backendDegradedForTest = func() bool { return false }
	window.clientRemoveCallback = func(*multiClientChannel) {}
	window.clientChannelArgs = make(chan *multiClientChannelArgs, count)
	window.clientChannelArgs <- first
	generator.extraClientIdGenerators = map[Id]MultiClientGenerator{}
	for range count - 1 {
		sibling := newMultiClientExpandLifecycleFixture(t)
		args := <-sibling.window.clientChannelArgs
		args.IpFamily, args.Capabilities = IpFamilyV4Only, capabilityClassD
		generator.extraClientIdGenerators[args.ClientId] = sibling.window.generator
		window.clientChannelArgs <- args
	}
	fixture.releasePing()
	for seeded := 0; seeded < count; {
		added := window.expand(size, seeded, 0, count, count, count-seeded, 0, windowExpandPassPlan{})
		if added <= 0 {
			t.Fatal("seed expansion made no progress")
		}
		seeded += added
	}
	return fixture, window.unorderedClients()
}

// Runs one resize pass of the fixture's window and returns with the pass
// parked at its end; cancelling the window ends it.
func udpSwapTestResizePass(t *testing.T, fixture *multiClientExpandLifecycleFixture) <-chan struct{} {
	t.Helper()
	window := fixture.window
	passDone := make(chan struct{}, 1)
	window.afterResizePassForTest = func() {
		passDone <- struct{}{}
		<-window.ctx.Done()
	}
	resizeDone := make(chan struct{})
	go func() { defer close(resizeDone); window.resize() }()
	fixture.wait(t, "resize pass", passDone)
	return resizeDone
}

// A full window that asks for any protocol and holds only flowless tcp-only
// exits asks for nothing and gives nothing up while none of its udp flows
// failed, and the user's Fixed IP gives nothing up even after one did: its
// stable egress ip is the point.
func TestResizeSwapsNothingWithoutAFailedUdpFlowOrForTheFixedIp(t *testing.T) {
	for name, c := range map[string]struct {
		size          WindowSizeSettings
		count         int
		profile       *PerformanceProfile
		udpFlowFailed bool
	}{
		"no failed udp flow": {
			size:  WindowSizeSettings{WindowSizeMin: 2, WindowSizeMax: 2, WindowSizeHardMax: 2},
			count: 2,
		},
		"the Fixed IP": {
			size:  WindowSizeSettings{FixedWindowSize: 1, WindowSizeMin: 1, WindowSizeMax: 1, WindowSizeHardMax: 1},
			count: 1,
			profile: &PerformanceProfile{
				WindowType: WindowTypeQuality,
				WindowSize: WindowSizeSettings{FixedWindowSize: 1, WindowSizeMin: 1, WindowSizeMax: 1, WindowSizeHardMax: 1},
			},
			udpFlowFailed: true,
		},
	} {
		fixture, previous := udpSwapTestFullWindow(t, c.size, c.count, c.profile)
		window := fixture.window
		if c.udpFlowFailed {
			window.noteUdpDemand(time.Now())
		}
		resizeDone := udpSwapTestResizePass(t, fixture)
		if window.udpFillWanted(time.Now()) != 0 || window.takeUdpProbeRequest() {
			t.Errorf("%s: the window asked for a provider that carries udp", name)
		}
		if len(window.unorderedClients()) != c.count || slices.ContainsFunc(previous, (*multiClientChannel).IsDone) ||
			slices.ContainsFunc(previous, (*multiClientChannel).isWarning) {
			t.Errorf("%s: the window gave up an exit", name)
		}
		fixture.cancelWindow()
		fixture.wait(t, "resize join", resizeDone)
	}
}

// While the udp shortfall is open the last slot of a pass waits for a
// candidate that carries udp, as the ipv6 reservation does: a tcp-only answer
// is admitted when the pass's acquisition ends, and at once while the swap is
// starved, so a platform with no dualproto provider never leaves the slot
// empty.
func TestExpandUdpReservationFallsBackWhenSupplyEnds(t *testing.T) {
	for _, starved := range []bool{false, true} {
		fixture := newMultiClientExpandLifecycleFixture(t)
		window := fixture.window
		window.settings.EvaluationPoolMultiple = 2
		window.settings.NormalWindowSelectors = udpSwapTestHostedSelectors
		first := <-window.clientChannelArgs
		first.IpFamily, first.Capabilities = IpFamilyV4Only, capabilityClassD
		window.clientChannelArgs <- first
		if starved {
			window.noteUdpStarved(time.Now())
		}
		finish := make(chan struct{})
		window.finishExpandRequestsForTest = finish
		done := make(chan int, 1)
		go func() {
			done <- window.expand(WindowSizeSettings{FixedWindowSize: 1, WindowSizeHardMax: 1}, 0, 0, 1, 1, 1, 0, windowExpandPassPlan{udpShortfall: 1})
		}()
		fixture.wait(t, "tcp-only ping", fixture.pingResultEntered)
		fixture.releasePing()
		fixture.wait(t, "tcp-only ping processed", fixture.pingResultDone)
		if (len(window.unorderedClients()) == 1) != starved {
			t.Errorf("starved %t: admission did not match the reservation", starved)
		}
		close(finish)
		if admitted := fixture.result(t, done); admitted != 1 || len(window.unorderedClients()) != 1 {
			t.Fatalf("starved %t: bounded completion admitted %d", starved, admitted)
		}
	}
}

// --- the single-exit preference ---

// The listing of a window sized to one exit: the first provider that carries
// udp and ranks with the best listed one goes first, the rest keep their
// order, and with none the listing is unchanged. Before the preference every
// listing kept the order of the platform's answer map.
func TestSingleExitListingOrder(t *testing.T) {
	type entry struct {
		udp  bool
		rank singleExitRank
	}
	tcp := func(tier int) entry { return entry{rank: singleExitRank{tier: tier}} }
	udp := func(tier int) entry { return entry{udp: true, rank: singleExitRank{tier: tier}} }
	cases := []struct {
		name     string
		entries  []entry
		tierBand int
		want     []int
	}{
		{name: "an equal udp provider listed second", entries: []entry{tcp(0), udp(0)}, want: []int{1, 0}},
		{name: "a udp provider listed first", entries: []entry{udp(0), tcp(0)}, want: []int{0, 1}},
		{name: "the first of several", entries: []entry{tcp(0), tcp(0), udp(0), udp(0)}, want: []int{2, 0, 1, 3}},
		{name: "a better udp provider", entries: []entry{tcp(1), tcp(0), udp(0)}, want: []int{2, 0, 1}},
		{name: "a worse udp provider", entries: []entry{tcp(0), udp(1)}, want: []int{0, 1}},
		{name: "a worse udp provider within the band", entries: []entry{tcp(0), udp(1)}, tierBand: 1, want: []int{1, 0}},
		{name: "the best decides, not the first", entries: []entry{tcp(1), udp(1), tcp(0)}, want: []int{0, 1, 2}},
		{name: "a less qualified udp provider", entries: []entry{{rank: singleExitRank{qualified: true}}, udp(0)}, want: []int{0, 1}},
		{name: "a farther udp provider", entries: []entry{tcp(0), {udp: true, rank: singleExitRank{locationLevel: 1}}}, want: []int{0, 1}},
		{name: "no udp provider", entries: []entry{tcp(0), tcp(0)}, want: []int{0, 1}},
	}
	for _, c := range cases {
		listed := make([]enumeratedDestination, len(c.entries))
		ranks := make([]singleExitRank, len(c.entries))
		for i, e := range c.entries {
			capabilities := capabilityClassD
			if e.udp {
				capabilities = capabilityClassB
			}
			listed[i] = enumeratedDestination{
				destination: RequireMultiHopId(NewId()),
				stats:       DestinationStats{IpFamily: capabilities.IpFamily(), Capabilities: capabilities, Tier: e.rank.tier},
			}
			ranks[i] = e.rank
		}
		ordered := singleExitListingOrder(slices.Clone(listed), ranks, c.tierBand)
		got := make([]int, len(ordered))
		for i, enumerated := range ordered {
			got[i] = slices.IndexFunc(listed, func(l enumeratedDestination) bool { return l.destination == enumerated.destination })
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: order %v, want %v", c.name, got, c.want)
		}
	}
}

// A listed provider ranks by the window's qualification table, the location
// level the platform found it at and its server tier.
func TestSingleExitRankOf(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	window := capabilityTestWindow(ctx, nil, DefaultMultiClientSettings(), WindowTypeQuality)
	qualified := RequireMultiHopId(NewId())
	window.providerQualifiedFunc = func(destination MultiHopId) bool { return destination == qualified }
	got := window.singleExitRankOf(enumeratedDestination{
		destination: qualified,
		stats:       DestinationStats{Tier: 3, LocationMatch: LocationMatchCountry},
	})
	if got != (singleExitRank{qualified: true, locationLevel: 2, tier: 3}) {
		t.Errorf("rank %+v, want qualified, the country level and tier 3", got)
	}
	got = window.singleExitRankOf(enumeratedDestination{destination: RequireMultiHopId(NewId())})
	if got != (singleExitRank{}) {
		t.Errorf("rank %+v, want unqualified, exact and tier 0", got)
	}
}

// Only a window that asks for any protocol and is sized to one exit orders
// its listing: never a window that asks for dualproto, as every app's does, a
// window with more than one exit, or the limited window.
func TestSingleExitListingPreferredOnlyForAnyWindowsSizedToOneExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hosted := DefaultMultiClientSettings()
	hosted.NormalWindowSelectors = udpSwapTestHostedSelectors
	one := FindProvidersSelect{TargetCount: 1}
	if !capabilityTestWindow(ctx, nil, hosted, WindowTypeQuality).singleExitListingPreferred(one) {
		t.Error("a hosted window sized to one exit keeps the platform's order")
	}
	if capabilityTestWindow(ctx, nil, hosted, WindowTypeQuality).singleExitListingPreferred(FindProvidersSelect{TargetCount: 2}) {
		t.Error("a hosted window of two exits orders its listing")
	}
	if capabilityTestWindow(ctx, nil, hosted, WindowTypeLimited).singleExitListingPreferred(one) {
		t.Error("the limited window orders its listing")
	}
	if capabilityTestWindow(ctx, nil, DefaultMultiClientSettings(), WindowTypeQuality).singleExitListingPreferred(one) {
		t.Error("an app's window orders its listing")
	}
}
