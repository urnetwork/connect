// A stay pinned to an extender (EXTENDER.md K10): the spec's hint reaches its
// destination only, the directory hands the pinned address over whatever its
// hold, and the pinned client's transports dial H1 through a strategy that has
// that address's extender dialers and nothing else.
package connect

import (
	"context"
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
)

func TestTransportHintReachesOnlyItsDestination(t *testing.T) {
	pinnedId := NewId()
	otherId := NewId()
	extenderIp := netip.MustParseAddr("203.0.113.7")
	specs := []*ProviderSpec{
		{ClientId: &pinnedId, TransportHint: &TransportHint{ExtenderIp: extenderIp}},
		{ClientId: &otherId},
	}

	pinned, err := NewMultiHopId(pinnedId)
	if err != nil {
		t.Fatal(err)
	}
	if hint := providerSpecsTransportHint(specs, pinned); hint == nil || hint.ExtenderIp != extenderIp {
		t.Fatalf("the pinned destination's hint = %+v", hint)
	}
	other, err := NewMultiHopId(otherId)
	if err != nil {
		t.Fatal(err)
	}
	if hint := providerSpecsTransportHint(specs, other); hint != nil {
		t.Fatalf("an unpinned destination got the hint %+v", hint)
	}
	// the pinned exit reached through an intermediary is not the pinned stay
	multiHop, err := NewMultiHopId(otherId, pinnedId)
	if err != nil {
		t.Fatal(err)
	}
	if hint := providerSpecsTransportHint(specs, multiHop); hint != nil {
		t.Fatalf("a multi-hop destination got the hint %+v", hint)
	}
	// an invalid address pins nothing
	specs[0].TransportHint = &TransportHint{}
	if hint := providerSpecsTransportHint(specs, pinned); hint != nil {
		t.Fatalf("an invalid address pinned the destination: %+v", hint)
	}
}

// The hint is client-side state: it is never on the find-providers2 wire, and
// the spec fingerprint that scopes window identities does not change with it.
func TestTransportHintIsNeverSent(t *testing.T) {
	clientId := NewId()
	spec := &ProviderSpec{ClientId: &clientId}
	unpinned, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	spec.TransportHint = &TransportHint{ExtenderIp: netip.MustParseAddr("203.0.113.7")}
	pinned, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if string(pinned) != string(unpinned) || strings.Contains(string(pinned), "203.0.113.7") {
		t.Fatalf("the hint reached the wire: %s", pinned)
	}
}

func TestPinnedCandidateIgnoresTheHold(t *testing.T) {
	clock := newTestClock()
	directory, _ := newTestExtenderDirectory(t, clock, nil)
	ip := netip.MustParseAddr("192.0.2.20")
	directory.AddBootstrap(ip, ExtenderSourceManual)

	directory.RecordFailure(ip, ExtenderConnectModeTcpTls)
	if directory.AddressUsable(ip) {
		t.Fatal("the failure did not hold the address")
	}
	if candidate := directory.PinnedCandidate(ip); candidate == nil || candidate.Ip != ip {
		t.Fatalf("the held address was not handed to the pin: %+v", candidate)
	}
	if candidate := directory.PinnedCandidate(netip.MustParseAddr("192.0.2.21")); candidate != nil {
		t.Fatalf("an unknown address was handed to the pin: %+v", candidate)
	}
	var noDirectory *ExtenderDirectory
	if candidate := noDirectory.PinnedCandidate(ip); candidate != nil {
		t.Fatalf("no directory handed over %+v", candidate)
	}
}

func TestPinnedExtenderStrategyDialsOnlyItsAddress(t *testing.T) {
	clock := newTestClock()
	directory, _ := newTestExtenderDirectory(t, clock, nil)
	pinnedIp := netip.MustParseAddr("192.0.2.30")
	otherIp := netip.MustParseAddr("192.0.2.31")
	directory.AddBootstrap(pinnedIp, ExtenderSourceManual)
	directory.AddBootstrap(otherIp, ExtenderSourceManual)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultClientStrategySettings()
	settings.ExtenderDirectory = directory
	base := NewClientStrategy(ctx, settings)
	pinned := base.newPinnedExtenderClientStrategy(ctx, pinnedIp)

	// no normal, resilient, VLESS, configured or alt dialer
	if 0 < len(pinned.dialers) {
		for dialer := range pinned.dialers {
			t.Fatalf("the pinned strategy started with %s", dialer.description)
		}
	}
	expanded := pinned.expandExtenderDialers()
	if len(expanded) == 0 {
		t.Fatal("the pinned address got no dialer")
	}
	for _, dialer := range expanded {
		if dialer.extenderConfig == nil || dialer.extenderConfig.Ip != pinnedIp {
			t.Fatalf("the pinned strategy expanded %s", dialer.description)
		}
	}
	if again := pinned.expandExtenderDialers(); len(again) != 0 {
		t.Fatalf("a second expand added %d dialers", len(again))
	}

	// a hold benches the address for every other dial, not for the pin
	directory.RecordFailure(pinnedIp, ExtenderConnectModeTcpTls)
	pinned.collapseExtenderDialers()
	if len(pinned.dialers) != len(expanded) {
		t.Fatalf("the hold dropped the pinned dialers: %d of %d left", len(pinned.dialers), len(expanded))
	}
	for dialer := range pinned.dialers {
		if dialer.extenderConfig == nil || dialer.extenderConfig.Ip != pinnedIp {
			t.Fatalf("the pinned strategy holds %s", dialer.description)
		}
	}
}

// A pin to the legacy private extender dials it with its secret, as the
// space's own strategy does.
func TestPinnedStrategyDialsACustomExtenderWithItsSecret(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base := NewClientStrategy(ctx, DefaultClientStrategySettings())
	ip := netip.MustParseAddr("198.51.100.7")
	base.SetCustomExtenders(map[netip.Addr]string{ip: "secret"})
	pinned := base.newPinnedExtenderClientStrategy(ctx, ip)

	expanded := pinned.expandExtenderDialers()
	if len(expanded) == 0 {
		t.Fatal("the custom extender got no dialer")
	}
	for _, dialer := range expanded {
		if dialer.extenderConfig.Ip != ip || dialer.extenderConfig.Secret != "secret" {
			t.Fatalf("dialer %s = %s with secret %q", dialer.description, dialer.extenderConfig.Ip, dialer.extenderConfig.Secret)
		}
	}
}

// A pinned client's transports dial H1 through its own strategy, whatever the
// window's mode policy; every other client keeps the policy and the shared
// strategy.
func TestPinnedStayTransportDialsH1ThroughItsOwnStrategy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base := NewClientStrategy(ctx, DefaultClientStrategySettings())
	pinned := base.newPinnedExtenderClientStrategy(ctx, netip.MustParseAddr("203.0.113.7"))

	var builtMode TransportMode
	var builtPreferences map[TransportMode]int
	var builtStrategy *ClientStrategy
	generator := &ApiMultiClientGenerator{
		settings:                DefaultApiMultiClientGeneratorSettings(),
		clientStrategy:          base,
		platformTransportMode:   TransportModeAuto,
		platformModePreferences: DefaultTransportModePreferences(),
		newPlatformTransport: func(_ *Client, _ *ClientAuth, mode TransportMode, settings *PlatformTransportSettings) apiWindowPlatformTransport {
			builtMode = mode
			builtPreferences = settings.ModePreferences
			return newFakeWindowPlatformTransport(false)
		},
		platformTransportStrategyForTest: func(_ *Client, clientStrategy *ClientStrategy) {
			builtStrategy = clientStrategy
		},
	}

	generator.createPlatformTransport(nil, &ClientAuth{}, &PlatformTransportSettings{}, pinned)
	if builtMode != TransportModeH1 || builtStrategy != pinned {
		t.Fatalf("pinned: mode %s, own strategy %t", builtMode, builtStrategy == pinned)
	}
	if builtPreferences != nil {
		t.Fatalf("pinned: the window's preferences %v reached an H1-only transport", builtPreferences)
	}

	generator.createPlatformTransport(nil, &ClientAuth{}, &PlatformTransportSettings{}, nil)
	if builtMode != TransportModeAuto || builtStrategy != base {
		t.Fatalf("unpinned: mode %s, shared strategy %t", builtMode, builtStrategy == base)
	}
}
