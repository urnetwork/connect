package connect

// Tests for the two capability axes of find-providers2's selector mode
// (server FP2-PROTOCOL.md §5, §7.1, §7.2): the label vocabulary, the
// compatibility table every placement site judges a flow by, and the wire
// shapes of `select`, `capabilities` and `overflow_step`.

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// The four classes of the doc, by their labels.
var (
	capabilityClassA = ProviderCapabilities{Ip: ProviderIpCapabilityDualstack, Protocol: ProviderProtocolCapabilityDualproto}
	capabilityClassB = ProviderCapabilities{Ip: ProviderIpCapabilityIpv4, Protocol: ProviderProtocolCapabilityDualproto}
	capabilityClassC = ProviderCapabilities{Ip: ProviderIpCapabilityDualstack, Protocol: ProviderProtocolCapabilityTcp}
	capabilityClassD = ProviderCapabilities{Ip: ProviderIpCapabilityIpv4, Protocol: ProviderProtocolCapabilityTcp}
)

// The compatibility table (FP2-PROTOCOL.md §7.2), for every class and every
// flow: tcp and icmp over v4 go anywhere, over v6 need dualstack; udp over v4
// needs dualproto, over v6 dualstack and dualproto. A tcp-only exit never
// carries udp.
func TestCapabilityCompatibilityTable(t *testing.T) {
	classes := map[string]ProviderCapabilities{
		"A": capabilityClassA,
		"B": capabilityClassB,
		"C": capabilityClassC,
		"D": capabilityClassD,
	}
	cases := []struct {
		need    flowNeed
		classes string
	}{
		{need: flowNeed{ipVersion: 4, ipProtocol: IpProtocolTcp}, classes: "ABCD"},
		{need: flowNeed{ipVersion: 6, ipProtocol: IpProtocolTcp}, classes: "AC"},
		{need: flowNeed{ipVersion: 4, ipProtocol: IpProtocolUdp}, classes: "AB"},
		{need: flowNeed{ipVersion: 6, ipProtocol: IpProtocolUdp}, classes: "A"},
		{need: flowNeed{ipVersion: 4, ipProtocol: IpProtocolIcmp}, classes: "ABCD"},
		{need: flowNeed{ipVersion: 6, ipProtocol: IpProtocolIcmp}, classes: "AC"},
		// a caller gathering exits without a flow in hand needs nothing
		{need: flowNeed{}, classes: "ABCD"},
	}
	for _, c := range cases {
		for name, capabilities := range classes {
			want := strings.Contains(c.classes, name)
			if got := c.need.carriedBy(capabilities.IpFamily(), capabilities); got != want {
				t.Errorf("class %s carries v%d protocol %d = %t, want %t", name, c.need.ipVersion, c.need.ipProtocol, got, want)
			}
			channel := &multiClientChannel{args: &multiClientChannelArgs{DestinationStats: DestinationStats{
				IpFamily:     capabilities.IpFamily(),
				Capabilities: capabilities,
			}}}
			if got := channel.supportsFlow(c.need.ipVersion, c.need.ipProtocol); got != want {
				t.Errorf("class %s exit supports v%d protocol %d = %t, want %t", name, c.need.ipVersion, c.need.ipProtocol, got, want)
			}
		}
	}
}

// A fixed destination carries every flow whatever its labels: the user chose
// the peer, with no alternative to prefer.
func TestFixedDestinationCarriesEveryFlow(t *testing.T) {
	channel := &multiClientChannel{args: &multiClientChannelArgs{
		FixedDestination: true,
		DestinationStats: DestinationStats{Capabilities: capabilityClassD},
	}}
	for _, ipVersion := range []int{4, 6} {
		for _, ipProtocol := range []IpProtocol{IpProtocolTcp, IpProtocolUdp, IpProtocolIcmp} {
			if !channel.supportsFlow(ipVersion, ipProtocol) {
				t.Errorf("a fixed destination refused v%d protocol %d", ipVersion, ipProtocol)
			}
		}
	}
}

// Normalization: an empty protocol is unlabelled and carries udp (no negative
// evidence means capable); a protocol or ip label this build does not know
// reads as the narrower label, so it never sends a flow into an exit that may
// not carry it.
func TestProviderCapabilitiesNormalize(t *testing.T) {
	cases := []struct {
		in   ProviderCapabilities
		want ProviderCapabilities
	}{
		{in: ProviderCapabilities{}, want: capabilityClassB},
		{in: capabilityClassA, want: capabilityClassA},
		{in: capabilityClassD, want: capabilityClassD},
		{in: ProviderCapabilities{Ip: "dualstack"}, want: capabilityClassA},
		{in: ProviderCapabilities{Ip: "ipv6", Protocol: "dualproto"}, want: capabilityClassB},
		{in: ProviderCapabilities{Ip: "dualstack", Protocol: "quic-only"}, want: capabilityClassC},
	}
	for _, c := range cases {
		if got := c.in.Normalize(); got != c.want {
			t.Errorf("%+v normalized to %+v, want %+v", c.in, got, c.want)
		}
	}
	if (ProviderCapabilities{}).CarriesUdp() != true || capabilityClassC.CarriesUdp() {
		t.Error("udp: an unlabelled provider must carry it and a tcp provider must not")
	}
	if capabilityClassC.IpFamily() != IpFamilyDualstack || capabilityClassB.IpFamily() != IpFamilyV4Only {
		t.Error("the ip label must map to the dualstack and v4-only categories")
	}
}

// An answer without labels is labelled from the proven category with
// dualproto (FP2-PROTOCOL.md §5.4).
func TestProviderCapabilitiesFromIpFamily(t *testing.T) {
	cases := map[IpFamily]ProviderCapabilities{
		IpFamilyDualstack: capabilityClassA,
		IpFamilyV4Only:    capabilityClassB,
		IpFamilyV6Only:    capabilityClassB,
		IpFamilyLegacy:    capabilityClassB,
	}
	for family, want := range cases {
		if got := providerCapabilitiesFromIpFamily(family); got != want {
			t.Errorf("%q labelled %+v, want %+v", family, got, want)
		}
	}
}

// `select` sends all six members, zeros included: an absent
// dualstack_target_count would take the server's default of 1.
func TestFindProvidersSelectSendsEveryMember(t *testing.T) {
	body, err := json.Marshal(FindProvidersSelect{
		Ip:       ProviderIpCapabilityDualstack,
		Protocol: ProviderProtocolCapabilityDualproto,
		// a speed window: no ipv6 minimum, nothing held yet
		TargetCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ip", "protocol", "target_count", "held_count", "dualstack_target_count", "held_dualstack_count"} {
		if _, ok := fields[name]; !ok {
			t.Errorf("select omits %s: %s", name, body)
		}
	}
	if len(fields) != 6 {
		t.Errorf("select has %d members, want 6: %s", len(fields), body)
	}
	if string(fields["dualstack_target_count"]) != "0" {
		t.Errorf("dualstack_target_count = %s, want an explicit 0", fields["dualstack_target_count"])
	}
}

// A request without `select` is legacy mode and sends nothing new: no
// select member at all (FP2-PROTOCOL.md §5.4, the old-request cell).
func TestLegacyFindProviders2RequestSendsNoSelect(t *testing.T) {
	body, err := json.Marshal(&FindProviders2Args{
		Specs:    []*ProviderSpec{{BestAvailable: true}},
		Count:    4,
		RankMode: "quality",
		IpFamily: IpFamilyFilterV4Capable,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "select") {
		t.Errorf("a legacy request carries select: %s", body)
	}
	want := `{"specs":[{"best_available":true}],"count":4,"exclude_client_ids":null,"rank_mode":"quality","ip_family":"v4-capable"}`
	if string(body) != want {
		t.Errorf("legacy request = %s, want %s", body, want)
	}
}

// A selector-mode answer decodes its echo, each provider's labels and the
// overflow step; a legacy answer has none of the three.
func TestFindProviders2ResultDecodesSelectorMode(t *testing.T) {
	clientId := NewId()
	body := `{"providers":[{"client_id":"` + clientId.String() + `","estimated_bytes_per_second":0,"has_estimated_bytes_per_second":false,"tier":33,` +
		`"ip_family":"v4-only","capabilities":{"ip":"ipv4","protocol":"tcp"},"overflow_step":1}],` +
		`"select":{"ip":"dualstack","protocol":"dualproto","target_count":6,"held_count":2,"dualstack_target_count":1,"held_dualstack_count":0}}`
	result := &FindProviders2Result{}
	if err := json.Unmarshal([]byte(body), result); err != nil {
		t.Fatal(err)
	}
	if result.Select == nil || result.Select.TargetCount != 6 || result.Select.DualstackTargetCount != 1 {
		t.Fatalf("echo = %+v, want the applied selectors", result.Select)
	}
	provider := result.Providers[0]
	if provider.Capabilities == nil || *provider.Capabilities != capabilityClassD || provider.OverflowStep != 1 {
		t.Fatalf("provider = %+v, want class D overflow", provider)
	}

	legacy := &FindProviders2Result{}
	if err := json.Unmarshal([]byte(`{"providers":[{"client_id":"`+clientId.String()+`","tier":0,"ip_family":"dualstack"}]}`), legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Select != nil || legacy.Providers[0].Capabilities != nil || legacy.Providers[0].OverflowStep != 0 {
		t.Errorf("legacy answer decoded selector-mode members: %+v", legacy)
	}
}

// addProviderDestinations labels each destination (FP2-PROTOCOL.md §7.1): in
// selector mode from the answer's labels, the ip label becoming the category;
// otherwise from ip_family with dualproto. A v6-only provider of a legacy
// answer keeps its v6-only category (a deviation from the doc's "anything
// else gives ipv4", which would route v4 into it).
func TestAddProviderDestinationsLabels(t *testing.T) {
	provider := func(ipFamily IpFamily, capabilities *ProviderCapabilities, overflowStep int) *FindProvidersProvider {
		return &FindProvidersProvider{
			ClientId:     NewId(),
			IpFamily:     ipFamily,
			Capabilities: capabilities,
			OverflowStep: overflowStep,
		}
	}
	cases := []struct {
		name         string
		provider     *FindProvidersProvider
		selectorMode bool
		wantFamily   IpFamily
		want         ProviderCapabilities
		wantOverflow int
	}{
		{name: "selector A", provider: provider(IpFamilyDualstack, &capabilityClassA, 0), selectorMode: true, wantFamily: IpFamilyDualstack, want: capabilityClassA},
		{name: "selector B from ipv6", provider: provider(IpFamilyDualstack, &capabilityClassB, 0), selectorMode: true, wantFamily: IpFamilyV4Only, want: capabilityClassB},
		{name: "selector C", provider: provider(IpFamilyDualstack, &capabilityClassC, 1), selectorMode: true, wantFamily: IpFamilyDualstack, want: capabilityClassC, wantOverflow: 1},
		{name: "selector D overflow", provider: provider(IpFamilyV4Only, &capabilityClassD, 1), selectorMode: true, wantFamily: IpFamilyV4Only, want: capabilityClassD, wantOverflow: 1},
		{name: "selector unlabelled", provider: provider(IpFamilyDualstack, nil, 0), selectorMode: true, wantFamily: IpFamilyDualstack, want: capabilityClassA},
		{name: "legacy dualstack", provider: provider(IpFamilyDualstack, nil, 0), wantFamily: IpFamilyDualstack, want: capabilityClassA},
		{name: "legacy v4-only", provider: provider(IpFamilyV4Only, nil, 0), wantFamily: IpFamilyV4Only, want: capabilityClassB},
		{name: "legacy v6-only", provider: provider(IpFamilyV6Only, nil, 0), wantFamily: IpFamilyV6Only, want: capabilityClassB},
		// an answer that did not echo is legacy whatever it carries
		{name: "no echo ignores labels", provider: provider(IpFamilyV4Only, &capabilityClassD, 1), wantFamily: IpFamilyV4Only, want: capabilityClassB},
	}
	for _, c := range cases {
		destinations := map[MultiHopId]DestinationStats{}
		addProviderDestinations(destinations, []*FindProvidersProvider{c.provider}, c.selectorMode)
		stats := destinations[RequireMultiHopId(c.provider.ClientId)]
		if stats.IpFamily != c.wantFamily || stats.Capabilities != c.want || stats.OverflowStep != c.wantOverflow {
			t.Errorf("%s: family %q labels %+v overflow %d, want %q %+v %d",
				c.name, stats.IpFamily, stats.Capabilities, stats.OverflowStep, c.wantFamily, c.want, c.wantOverflow)
		}
	}
}

// The limited window's size (FP2-PROTOCOL.md §7.4): half the largest active
// normal target, floored, at least one; the hard max likewise, unbounded when
// the normal hard max is; LimitedWindowSizeMax caps both; nothing to fill when
// off, unknown, or without a normal target.
func TestLimitedWindowSize(t *testing.T) {
	cases := []struct {
		name        string
		sizing      limitedSizing
		wantTarget  int
		wantHardMax int
	}{
		{name: "desktop auto", sizing: limitedSizing{enabled: true, fraction: 0.5, targetSize: 6, hardMax: 6}, wantTarget: 3, wantHardMax: 3},
		{name: "mobile auto", sizing: limitedSizing{enabled: true, fraction: 0.5, targetSize: 4, hardMax: 4}, wantTarget: 2, wantHardMax: 2},
		{name: "android auto", sizing: limitedSizing{enabled: true, fraction: 0.5, targetSize: 10, hardMax: 10}, wantTarget: 5, wantHardMax: 5},
		{name: "floor", sizing: limitedSizing{enabled: true, fraction: 0.5, targetSize: 5, hardMax: 5}, wantTarget: 2, wantHardMax: 2},
		{name: "at least one", sizing: limitedSizing{enabled: true, fraction: 0.5, targetSize: 1, hardMax: 4}, wantTarget: 1, wantHardMax: 2},
		{name: "fixed profile of 4", sizing: limitedSizing{enabled: true, fraction: 0.5, targetSize: 4, hardMaxUnbounded: true}, wantTarget: 2, wantHardMax: 0},
		{name: "hard max bounds the target", sizing: limitedSizing{enabled: true, fraction: 0.5, targetSize: 8, hardMax: 2}, wantTarget: 1, wantHardMax: 1},
		{name: "memory gate cap", sizing: limitedSizing{enabled: true, fraction: 0.5, sizeMax: 1, targetSize: 10, hardMax: 10}, wantTarget: 1, wantHardMax: 1},
		{name: "cap on an unbounded hard max", sizing: limitedSizing{enabled: true, fraction: 0.5, sizeMax: 2, targetSize: 8, hardMaxUnbounded: true}, wantTarget: 2, wantHardMax: 2},
		{name: "fraction zero", sizing: limitedSizing{enabled: true, fraction: 0, targetSize: 6, hardMax: 6}},
		{name: "fraction negative", sizing: limitedSizing{enabled: true, fraction: -0.5, targetSize: 6, hardMax: 6}},
		{name: "fraction nan", sizing: limitedSizing{enabled: true, fraction: math.NaN(), targetSize: 6, hardMax: 6}},
		{name: "fraction infinite", sizing: limitedSizing{enabled: true, fraction: math.Inf(1), targetSize: 6, hardMax: 6}},
		{name: "no normal target yet", sizing: limitedSizing{enabled: true, fraction: 0.5, hardMax: 6}},
		{name: "disabled", sizing: limitedSizing{fraction: 0.5, targetSize: 6, hardMax: 6}},
		{name: "huge fraction stays bounded", sizing: limitedSizing{enabled: true, fraction: 1e300, targetSize: 6, hardMaxUnbounded: true}, wantTarget: math.MaxInt32, wantHardMax: 0},
	}
	for _, c := range cases {
		windowSize := limitedWindowSize(c.sizing)
		if windowSize.WindowSizeMin != c.wantTarget || windowSize.WindowSizeMax != c.wantTarget || windowSize.WindowSizeHardMax != c.wantHardMax {
			t.Errorf("%s: min %d max %d hard max %d, want target %d hard max %d",
				c.name, windowSize.WindowSizeMin, windowSize.WindowSizeMax, windowSize.WindowSizeHardMax, c.wantTarget, c.wantHardMax)
		}
		if windowSize.FixedWindowSize != 0 || windowSize.WindowSizeReconnectScale != 1 || windowSize.WindowSizeMinIpv6Capable != 0 {
			t.Errorf("%s: %+v, want no fixed size, a reconnect scale of 1 and no ipv6 minimum", c.name, windowSize)
		}
		if err := windowSize.Validate(); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

// The ipv6 minimum a request carries: the quality window in auto and the
// profile's window are floored at one, the speed window in auto reads its
// setting, and the limited window has none.
func TestRequestDualstackTargetCount(t *testing.T) {
	quality := WindowTypeQuality
	speed := WindowTypeSpeed
	cases := []struct {
		name            string
		windowType      WindowType
		fixedWindowType *WindowType
		minIpv6Capable  int
		want            int
	}{
		{name: "desktop quality", windowType: WindowTypeQuality, minIpv6Capable: 1, want: 1},
		{name: "mobile quality", windowType: WindowTypeQuality, minIpv6Capable: 0, want: 1},
		{name: "speed in auto", windowType: WindowTypeSpeed, minIpv6Capable: 0, want: 0},
		{name: "fixed speed profile", windowType: WindowTypeSpeed, fixedWindowType: &speed, want: 1},
		{name: "fixed quality profile", windowType: WindowTypeQuality, fixedWindowType: &quality, want: 1},
		{name: "inactive window of a fixed profile", windowType: WindowTypeQuality, fixedWindowType: &speed, want: 0},
		{name: "a larger setting", windowType: WindowTypeQuality, minIpv6Capable: 2, want: 2},
		{name: "limited", windowType: WindowTypeLimited, minIpv6Capable: 1, want: 0},
	}
	for _, c := range cases {
		got := requestDualstackTargetCount(c.windowType, c.fixedWindowType, WindowSizeSettings{WindowSizeMinIpv6Capable: c.minIpv6Capable})
		if got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}

// The limited window's rank mode follows the performance profile (D10):
// auto asks quality, a fixed Quality profile quality, a fixed Speed profile
// speed.
func TestLimitedWindowRankModeFollowsTheProfile(t *testing.T) {
	cases := []struct {
		profile *PerformanceProfile
		want    string
	}{
		{profile: nil, want: "quality"},
		{profile: &PerformanceProfile{WindowType: WindowTypeAuto}, want: "quality"},
		{profile: &PerformanceProfile{WindowType: WindowTypeQuality, WindowSize: WindowSizeSettings{WindowSizeMin: 4, WindowSizeMax: 4}}, want: "quality"},
		{profile: &PerformanceProfile{WindowType: WindowTypeSpeed, WindowSize: WindowSizeSettings{WindowSizeMin: 4, WindowSizeMax: 4}}, want: "speed"},
	}
	for _, c := range cases {
		if got := limitedWindowRankMode(c.profile); got != c.want {
			t.Errorf("profile %+v: rank mode %q, want %q", c.profile, got, c.want)
		}
	}
	if WindowTypeLimited.RankMode() != "" {
		t.Errorf("WindowTypeLimited.RankMode() = %q, want \"\": its rank mode is the profile's", WindowTypeLimited.RankMode())
	}
}

// No performance profile can route every flow to the limited window.
func TestPerformanceProfileRefusesTheLimitedWindowType(t *testing.T) {
	profile := &PerformanceProfile{WindowType: WindowTypeLimited, WindowSize: WindowSizeSettings{WindowSizeMin: 1, WindowSizeMax: 1}}
	if profile.Validate() == nil {
		t.Error("a profile fixing the limited window validated")
	}
	for _, windowType := range []WindowType{WindowTypeAuto, WindowTypeQuality, WindowTypeSpeed} {
		profile := &PerformanceProfile{WindowType: windowType, WindowSize: WindowSizeSettings{WindowSizeMin: 1, WindowSizeMax: 1}}
		if err := profile.Validate(); err != nil {
			t.Errorf("window type %d refused: %v", windowType, err)
		}
	}
}
