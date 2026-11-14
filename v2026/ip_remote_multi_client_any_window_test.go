package connect

// Tests for normal windows that ask for every provider
// (MultiClientSettings.NormalWindowSelectors ipv4 and any; owner, 2026-11-13:
// "hosted proxies should use main window as the limited window, so that
// dualproto and dualstack requirements are not applied on the main window for
// hosted proxies, and there is no need for a separate limited window"): the
// `select` they ask with, every label admitted and ranked alike, udp never
// raced to a tcp exit, and no limited window beside them.

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

// The selectors the proxy service's hosted devices set.
func anyWindowTestSettings() *MultiClientSettings {
	settings := DefaultMultiClientSettings()
	settings.Log = NewNoopLogger()
	settings.NormalWindowSelectors = ProviderSelectors{
		Ip:       ProviderIpCapabilityIpv4,
		Protocol: ProviderProtocolCapabilityAny,
	}
	return settings
}

// A normal window under ipv4 and any asks with them. It has no ipv6 minimum,
// from its configuration, a fixed profile or a stale resize pass, so no
// shortfall either, and it counts every live, unwarned client as held, since
// each label is the protocol any asks for.
func TestAnyWindowAsksWithIpv4AndAny(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := anyWindowTestSettings()
	generator := newCapabilityTestGenerator(true, nil)

	for _, profile := range []*PerformanceProfile{
		nil,
		{WindowType: WindowTypeQuality, WindowSize: WindowSizeSettings{WindowSizeMin: 4, WindowSizeMax: 4, FixedWindowSize: 4}},
	} {
		window := capabilityTestWindow(ctx, generator, settings, WindowTypeQuality)
		window.performanceProfile = profile
		window.initRequestSizing()
		if window.requestDualstackTargetCount != 0 || window.ipv6Shortfall != 0 {
			t.Errorf("profile %+v: ipv6 minimum %d, shortfall %d, want none", profile, window.requestDualstackTargetCount, window.ipv6Shortfall)
		}
	}

	quality := capabilityTestWindow(ctx, generator, settings, WindowTypeQuality)
	quality.initRequestSizing()
	for _, capabilities := range []ProviderCapabilities{capabilityClassA, capabilityClassB, capabilityClassC, capabilityClassD} {
		client := capabilityTestChannel(t, ctx, settings, WindowTypeQuality, capabilities)
		quality.clients[client.ClientId()] = client
	}
	warned := capabilityTestChannel(t, ctx, settings, WindowTypeQuality, capabilityClassC)
	warned.setWarning(true, warnUnhealthy)
	quality.clients[warned.ClientId()] = warned
	want := FindProvidersSelect{
		Ip:                   ProviderIpCapabilityIpv4,
		Protocol:             ProviderProtocolCapabilityAny,
		TargetCount:          settings.WindowSizes[WindowTypeQuality].WindowSizeMin,
		HeldCount:            4,
		DualstackTargetCount: 0,
		HeldDualstackCount:   2,
	}
	if got := quality.providerSelect(); got != want {
		t.Errorf("quality select %+v, want %+v", got, want)
	}
	// an ipv4 request carries no ipv6 minimum whatever a pass recorded
	quality.setRequestSizing(6, 1)
	if got := quality.providerSelect(); got.DualstackTargetCount != 0 {
		t.Errorf("an ipv4 request carried an ipv6 minimum of %d", got.DualstackTargetCount)
	}

	speed := capabilityTestWindow(ctx, generator, settings, WindowTypeSpeed)
	speed.initRequestSizing()
	if got := speed.providerSelect(); got.Ip != ProviderIpCapabilityIpv4 || got.Protocol != ProviderProtocolCapabilityAny || got.DualstackTargetCount != 0 {
		t.Errorf("speed select %+v, want ipv4 and any with no ipv6 minimum", got)
	}

	// the default asks as every window did before the setting
	defaultSettings := DefaultMultiClientSettings()
	defaultQuality := capabilityTestWindow(ctx, generator, defaultSettings, WindowTypeQuality)
	defaultQuality.initRequestSizing()
	if got := defaultQuality.providerSelect(); got.Ip != ProviderIpCapabilityDualstack || got.Protocol != ProviderProtocolCapabilityDualproto || got.DualstackTargetCount != 1 {
		t.Errorf("default quality select %+v, want dualstack and dualproto with an ipv6 minimum of 1", got)
	}
}

// An any window keeps every provider the server answers, dualproto and tcp
// alike, asks once, v4-capable, and never asks the separate v6-capable
// request or starves the family over an answer without a dualstack provider:
// it has no ipv6 need, from a server that echoes `select` or from one that
// does not.
func TestAnyWindowAdmitsEveryLabel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := anyWindowTestSettings()

	for _, capabilities := range []ProviderCapabilities{capabilityClassA, capabilityClassB, capabilityClassC, capabilityClassD} {
		for _, windowType := range []WindowType{WindowTypeQuality, WindowTypeSpeed} {
			window := capabilityTestWindow(ctx, nil, settings, windowType)
			if !window.admitsCapabilities(capabilities) {
				t.Errorf("window %d refused a %+v provider", windowType, capabilities)
			}
		}
	}

	answer := capabilityTestClassDestinations()
	generator := newCapabilityTestGenerator(true, func(call capabilityTestCall) map[MultiHopId]DestinationStats {
		return answer
	})
	generator.mode.observe(true)
	for _, windowType := range []WindowType{WindowTypeQuality, WindowTypeSpeed} {
		window := capabilityTestWindow(ctx, generator, settings, windowType)
		window.initRequestSizing()
		destinations, err := window.discoverDestinations(nil, false, windowType.RankMode())
		if err != nil {
			t.Fatal(err)
		}
		protocols := map[ProviderProtocolCapability]int{}
		for _, destination := range destinations {
			protocols[destination.stats.Capabilities.Protocol] += 1
		}
		if len(destinations) != 4 || protocols[ProviderProtocolCapabilityDualproto] != 2 || protocols[ProviderProtocolCapabilityTcp] != 2 {
			t.Errorf("window %d kept %d destinations (%v), want all four classes", windowType, len(destinations), protocols)
		}
	}
	calls := generator.recordedCalls()
	if len(calls) != 2 {
		t.Fatalf("calls %d, want one per window", len(calls))
	}
	for _, call := range calls {
		if call.ipFamily != IpFamilyFilterV4Capable || call.providerSelect.Ip != ProviderIpCapabilityIpv4 ||
			call.providerSelect.Protocol != ProviderProtocolCapabilityAny || call.providerSelect.DualstackTargetCount != 0 {
			t.Errorf("request %+v, want one v4-capable ipv4 and any request with no ipv6 need", call)
		}
	}

	// answers without a dualstack provider, from a server that echoes and
	// from one known not to
	for _, echo := range []bool{true, false} {
		ipv4Only := newCapabilityTestGenerator(echo, func(call capabilityTestCall) map[MultiHopId]DestinationStats {
			return map[MultiHopId]DestinationStats{
				RequireMultiHopId(NewId()): {IpFamily: IpFamilyV4Only, Capabilities: capabilityClassB},
				RequireMultiHopId(NewId()): {IpFamily: IpFamilyV4Only, Capabilities: capabilityClassD},
			}
		})
		ipv4Only.mode.observe(echo)
		window := capabilityTestWindow(ctx, ipv4Only, settings, WindowTypeQuality)
		window.initRequestSizing()
		if _, err := window.discoverDestinations(nil, false, "quality"); err != nil {
			t.Fatal(err)
		}
		if got := ipv4Only.recordedCalls(); len(got) != 1 || got[0].ipFamily != IpFamilyFilterV4Capable {
			t.Errorf("echo %t: calls %+v, want the main request alone", echo, got)
		}
		if window.ipv6Starved(time.Now()) {
			t.Errorf("echo %t: an answer without a dualstack provider starved a window with no ipv6 need", echo)
		}
	}
}

// An any window ranks every label alike, so a tcp exit may hold its best
// rank. A udp flow still races only the exits labelled dualproto, ranked
// among themselves; a tcp flow races the window's best rank, tcp exits
// included; and once the windows hold no exit that carries udp, a udp flow is
// unroutable while tcp is not.
func TestAnyWindowUdpNeverRacesATcpExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := anyWindowTestSettings()
	generator := newCapabilityTestGenerator(true, nil)
	quality := capabilityTestWindow(ctx, generator, settings, WindowTypeQuality)
	speed := capabilityTestWindow(ctx, generator, settings, WindowTypeSpeed)
	// no limited window: an any multi client builds none
	parent := capabilityTestParent(ctx, cancel, generator, settings, map[WindowType]*multiClientWindow{
		WindowTypeQuality: quality,
		WindowTypeSpeed:   speed,
	})
	byName := map[*multiClientChannel]string{}
	add := func(window *multiClientWindow, windowType WindowType, name string, capabilities ProviderCapabilities, tier int) *multiClientChannel {
		client := capabilityTestChannel(t, ctx, settings, windowType, capabilities)
		client.args.DestinationStats.Tier = tier
		window.clients[client.ClientId()] = client
		byName[client] = name
		return client
	}
	// the tcp exits rank best in the window
	a := add(quality, WindowTypeQuality, "A", capabilityClassA, 11)
	b := add(quality, WindowTypeQuality, "B", capabilityClassB, 11)
	add(quality, WindowTypeQuality, "C", capabilityClassC, 0)
	add(quality, WindowTypeQuality, "D", capabilityClassD, 0)
	add(speed, WindowTypeSpeed, "d", capabilityClassD, 0)

	names := func(field []*multiClientChannel) string {
		named := []string{}
		for _, client := range field {
			named = append(named, byName[client])
		}
		slices.Sort(named)
		return strings.Join(named, "")
	}
	cases := []struct {
		need          flowNeed
		wantQuality   string
		wantCrossTier string
		wantSpeed     string
	}{
		{need: flowNeed{ipVersion: 4, ipProtocol: IpProtocolTcp}, wantQuality: "CD", wantCrossTier: "ABCD", wantSpeed: "d"},
		{need: flowNeed{ipVersion: 6, ipProtocol: IpProtocolTcp}, wantQuality: "C", wantCrossTier: "AC", wantSpeed: ""},
		{need: flowNeed{ipVersion: 4, ipProtocol: IpProtocolUdp}, wantQuality: "AB", wantCrossTier: "AB", wantSpeed: ""},
		{need: flowNeed{ipVersion: 6, ipProtocol: IpProtocolUdp}, wantQuality: "A", wantCrossTier: "A", wantSpeed: ""},
		{need: flowNeed{ipVersion: 4, ipProtocol: IpProtocolIcmp}, wantQuality: "CD", wantCrossTier: "ABCD", wantSpeed: "d"},
	}
	for _, c := range cases {
		var offerChanged, limitedOfferChanged <-chan struct{}
		if got := names(parent.raceField(quality, c.need, &offerChanged, &limitedOfferChanged)); got != c.wantQuality {
			t.Errorf("v%d protocol %d: quality field %q, want %q", c.need.ipVersion, c.need.ipProtocol, got, c.wantQuality)
		}
		if got := names(parent.crossTierRaceField(quality, c.need)); got != c.wantCrossTier {
			t.Errorf("v%d protocol %d: cross-tier field %q, want %q", c.need.ipVersion, c.need.ipProtocol, got, c.wantCrossTier)
		}
		if got := names(parent.lastResortRaceField(quality, c.need)); got != c.wantCrossTier {
			t.Errorf("v%d protocol %d: last-resort field %q, want %q", c.need.ipVersion, c.need.ipProtocol, got, c.wantCrossTier)
		}
		if got := names(parent.raceField(speed, c.need, nil, nil)); got != c.wantSpeed {
			t.Errorf("v%d protocol %d: speed field %q, want %q", c.need.ipVersion, c.need.ipProtocol, got, c.wantSpeed)
		}
	}

	windowTypes := []WindowType{WindowTypeQuality, WindowTypeSpeed}
	udp := flowNeed{ipVersion: 4, ipProtocol: IpProtocolUdp}
	tcp := flowNeed{ipVersion: 4, ipProtocol: IpProtocolTcp}
	if parent.flowUnroutable(udp, windowTypes) || parent.UdpUnroutable() {
		t.Error("udp unroutable while the quality window holds dualproto exits")
	}
	// the windows now hold tcp exits alone
	delete(quality.clients, a.ClientId())
	delete(quality.clients, b.ClientId())
	if got := names(parent.raceField(quality, udp, nil, nil)); got != "" {
		t.Errorf("udp raced %q with only tcp exits in the windows", got)
	}
	if got := names(parent.raceField(speed, udp, nil, nil)); got != "" {
		t.Errorf("udp raced %q in speed with only a tcp exit", got)
	}
	if !parent.flowUnroutable(udp, windowTypes) || !parent.UdpUnroutable() {
		t.Error("udp not unroutable with only tcp exits in the windows")
	}
	if parent.flowUnroutable(tcp, windowTypes) {
		t.Error("tcp unroutable with tcp exits in the windows")
	}
}

// The merged race (mergeRaceField, raceWindowTopsFirst) leaves an any
// window's field as it is: with no limited window there is nothing to merge,
// so the window's race field holds exactly its own candidates, and the tops
// step keeps a one-window field in its order, in the same slice.
func TestAnyWindowRaceFieldIsTheWindowsOwn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := anyWindowTestSettings()
	generator := newCapabilityTestGenerator(true, nil)
	quality := capabilityTestWindow(ctx, generator, settings, WindowTypeQuality)
	parent := capabilityTestParent(ctx, cancel, generator, settings, map[WindowType]*multiClientWindow{
		WindowTypeQuality: quality,
	})
	for _, capabilities := range []ProviderCapabilities{capabilityClassA, capabilityClassB, capabilityClassC, capabilityClassD} {
		client := capabilityTestChannel(t, ctx, settings, WindowTypeQuality, capabilities)
		quality.clients[client.ClientId()] = client
	}
	clientIds := func(field []*multiClientChannel) []Id {
		ids := []Id{}
		for _, client := range field {
			ids = append(ids, client.ClientId())
		}
		slices.SortFunc(ids, func(a Id, b Id) int {
			return a.Cmp(b)
		})
		return ids
	}
	for _, c := range []struct {
		need      flowNeed
		wantCount int
	}{
		{need: flowNeed{ipVersion: 4, ipProtocol: IpProtocolTcp}, wantCount: 4},
		{need: flowNeed{ipVersion: 4, ipProtocol: IpProtocolUdp}, wantCount: 2},
	} {
		field := parent.raceField(quality, c.need, nil, nil)
		own := parent.raceCandidates(quality, c.need)
		if len(field) != c.wantCount || !slices.Equal(clientIds(field), clientIds(own)) {
			t.Fatalf("protocol %d: race field of %d exits, want the window's own %d", c.need.ipProtocol, len(field), len(own))
		}
		ordered := raceWindowTopsFirst(field, settings, time.Now())
		if !slices.Equal(ordered, field) || &ordered[0] != &field[0] {
			t.Errorf("protocol %d: the tops step reordered a one-window field", c.need.ipProtocol)
		}
	}
}

// A multi client whose normal windows ask for any builds no limited window,
// whatever the fraction says at construction or at runtime (connect's default
// and the sdk's hold of its 32-MiB tier among them), and its windows ask with
// ipv4 and any. The default selectors still build one, even at a fraction of
// 0, which the developer override can turn on.
func TestAnyWindowsBuildNoLimitedWindow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, fraction := range []float64{DefaultMultiClientSettings().LimitedWindowSizeFraction, 0.5, 1} {
		settings := anyWindowTestSettings()
		settings.LimitedWindowSizeFraction = fraction
		multi := NewRemoteUserNatMultiClient(ctx, newCapabilityTestGenerator(true, nil), nil, 0, settings)
		defer multi.Close()
		if multi.windows[WindowTypeLimited] != nil || len(multi.windows) != 2 {
			t.Fatalf("fraction %v: windows %d with limited %t, want quality and speed alone", fraction, len(multi.windows), multi.windows[WindowTypeLimited] != nil)
		}
		multi.SetLimitedWindowSizeFraction(1)
		if multi.windows[WindowTypeLimited] != nil || multi.LimitedWindowSize() != 0 || multi.LimitedWindowTargetSize() != 0 {
			t.Errorf("fraction %v: a runtime fraction gave an any multi client a limited window", fraction)
		}
		for windowType, window := range multi.windows {
			if got := window.providerSelect(); got.Ip != ProviderIpCapabilityIpv4 || got.Protocol != ProviderProtocolCapabilityAny || got.DualstackTargetCount != 0 {
				t.Errorf("fraction %v: window %d asks %+v, want ipv4 and any with no ipv6 minimum", fraction, windowType, got)
			}
		}
	}

	// any decides, whatever the ip selector
	dualstackAny := anyWindowTestSettings()
	dualstackAny.NormalWindowSelectors.Ip = ProviderIpCapabilityDualstack
	dualstackMulti := NewRemoteUserNatMultiClient(ctx, newCapabilityTestGenerator(true, nil), nil, 0, dualstackAny)
	defer dualstackMulti.Close()
	if dualstackMulti.windows[WindowTypeLimited] != nil {
		t.Error("dualstack and any built a limited window")
	}

	appSettings := DefaultMultiClientSettings()
	appSettings.Log = NewNoopLogger()
	appSettings.LimitedWindowSizeFraction = 0
	app := NewRemoteUserNatMultiClient(ctx, newCapabilityTestGenerator(true, nil), nil, 0, appSettings)
	defer app.Close()
	if app.windows[WindowTypeLimited] == nil {
		t.Error("the default selectors built no limited window at a fraction of 0")
	}
}
