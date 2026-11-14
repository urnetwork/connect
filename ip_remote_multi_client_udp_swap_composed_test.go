package connect

// Composed regressions for udp in the windows that ask for any protocol, the
// hosted devices' (ip_remote_multi_client_udp_swap.go). They drive only window
// and parent surfaces that predate the udp swap and the single-exit
// preference, so they run unchanged against the code before them, where they
// fail.

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A full window that asks for any protocol and holds only tcp-only exits, as
// a hosted device's can. A udp flow finds no exit and is answered as
// unroutable; the resize pass then has the enumerator find a provider that
// carries udp and gives it a flowless tcp-only exit's slot, so the next udp
// flow has an exit. Before the udp swap nothing asked for one, and every udp
// flow was unroutable for as long as the window kept its exits. A strict
// window removes the exit; a non-strict one warns it out of selection.
func TestHostedWindowSwapsInAUdpExitAfterAUdpFlowFoundNone(t *testing.T) {
	for _, strict := range []bool{true, false} {
		fixture := newMultiClientExpandLifecycleFixture(t)
		udpFixture := newMultiClientExpandLifecycleFixture(t)
		window := fixture.window
		first := <-window.clientChannelArgs
		candidate := <-udpFixture.window.clientChannelArgs
		first.IpFamily, first.Capabilities = IpFamilyV4Only, capabilityClassD
		candidate.IpFamily, candidate.Capabilities = IpFamilyV4Only, capabilityClassB
		generator := newIpv6AdmissionGenerator(fixture, first, udpFixture, candidate)
		window.generator = generator
		window.settings.NormalWindowSelectors = ProviderSelectors{
			Ip:       ProviderIpCapabilityIpv4,
			Protocol: ProviderProtocolCapabilityAny,
		}
		window.settings.StandingReserve = false
		window.settings.StrictWindowSizeHardMax = strict
		count := 2
		size := WindowSizeSettings{WindowSizeMin: count, WindowSizeMax: count, WindowSizeHardMax: count}
		window.settings.WindowSizes[WindowTypeQuality] = size
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
		previous := window.unorderedClients()

		// the device's quality window: a udp flow finds no exit there
		multiClient, _, closeParent := groupTestParent(t, DisableSecurityPolicy())
		multiClient.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
			return nil
		}
		multiClient.windows[WindowTypeQuality] = window
		egress := capabilityTestPath(4, IpProtocolUdp, 44002)
		windowTypes := []WindowType{WindowTypeQuality, WindowTypeSpeed}
		packet := MessagePoolCopy(ipOosUdpPacket(egress, []byte{0xc0, 0x00, 0x00, 0x00, 0x01}))
		multiClient.sendPacketGroup(SourceId(NewId()), protocol.ProvideMode_Network, requireGroupTestPacketGroup(t, packet), 50*time.Millisecond)
		if multiClient.UdpNoRouteCount() != 1 {
			t.Fatalf("strict %t: udp no-route %d, want the one unroutable flow", strict, multiClient.UdpNoRouteCount())
		}

		// an unbuffered offer keeps the candidate ready until the expansion
		// takes it; the pass barriers exclude timer and scheduler luck
		window.clientChannelArgs = make(chan *multiClientChannelArgs)
		passDone, nextPass := make(chan struct{}, 1), make(chan struct{})
		window.afterResizePassForTest = func() {
			passDone <- struct{}{}
			select {
			case <-nextPass:
			case <-window.ctx.Done():
			}
		}
		resizeDone := make(chan struct{})
		go func() { defer close(resizeDone); window.resize() }()
		fixture.wait(t, "first full-window resize", passDone)
		enumerateDone := make(chan struct{})
		go func() { defer close(enumerateDone); window.randomEnumerateClientArgs() }()
		fixture.wait(t, "udp candidate minted after discovery", generator.minted)
		if len(window.unorderedClients()) != count || slices.ContainsFunc(previous, (*multiClientChannel).IsDone) {
			t.Fatalf("strict %t: the full window gave up an exit before the candidate was minted", strict)
		}
		fixture.send(t, "next resize pass", nextPass)
		fixture.wait(t, "udp swap pass", passDone)

		retired, warned := 0, 0
		for _, old := range previous {
			if old.IsDone() {
				retired += 1
			} else if old.isWarning() {
				warned += 1
			}
		}
		clients := len(window.unorderedClients())
		if strict && (clients != count || retired != 1 || warned != 0) {
			t.Errorf("strict swap left %d clients, retired %d, warned %d", clients, retired, warned)
		}
		if !strict && (clients != count+1 || retired != 0 || warned != 1) {
			t.Errorf("non-strict swap left %d clients, retired %d, warned %d", clients, retired, warned)
		}
		if !window.udpAvailable() || multiClient.flowUnroutable(flowNeedOf(egress), windowTypes) {
			t.Errorf("strict %t: the next udp flow still has no exit", strict)
		}

		closeParent()
		fixture.cancelWindow()
		fixture.wait(t, "resize join", resizeDone)
		fixture.wait(t, "enumeration join", enumerateDone)
	}
}

// A hosted window sized to one exit (a single-exit profile, the user's Fixed
// IP) is offered tcp-only and dualproto providers that rank alike: it lists a
// dualproto one first, so the provider its one slot gets carries udp. Before
// the single-exit preference the listing followed the platform's answer map,
// and a tcp-only provider came first about half the time.
func TestSingleExitHostedWindowListsAUdpProviderFirst(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()
	settings.NormalWindowSelectors = ProviderSelectors{
		Ip:       ProviderIpCapabilityIpv4,
		Protocol: ProviderProtocolCapabilityAny,
	}
	destinations := map[MultiHopId]DestinationStats{}
	for i := range 8 {
		capabilities := capabilityClassD
		if i%2 == 1 {
			capabilities = capabilityClassB
		}
		destinations[RequireMultiHopId(NewId())] = DestinationStats{
			IpFamily:     capabilities.IpFamily(),
			Capabilities: capabilities,
		}
	}
	generator := newCapabilityTestGenerator(true, func(capabilityTestCall) map[MultiHopId]DestinationStats {
		return maps.Clone(destinations)
	})
	window := capabilityTestWindow(ctx, generator, settings, WindowTypeQuality)
	window.setRequestSizing(1, 0)

	listings := 32
	tcpFirst := 0
	for range listings {
		listed, err := window.discoverDestinations(nil, false, window.requestRankMode())
		if err != nil {
			t.Fatal(err)
		}
		if len(listed) != len(destinations) {
			t.Fatalf("listed %d providers, want all %d", len(listed), len(destinations))
		}
		if !listed[0].stats.Capabilities.CarriesUdp() {
			tcpFirst += 1
		}
	}
	if tcpFirst != 0 {
		t.Errorf("a tcp-only provider was listed first in %d of %d listings", tcpFirst, listings)
	}
}
