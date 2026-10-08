package connect

// Tests for the client strategy's refusals of endpoints configured by hand,
// which a hosted (cloud) device's strategy sets: extenders
// (`DisableManualExtenders`) and DoH servers (`DisableCustomDohServers`). The
// VLESS refusal (`DisableVless`) is tested with the VLESS dialer.

import (
	"context"
	"net/netip"
	"slices"
	"testing"
	"time"
)

// A strategy that refuses extenders configured by hand takes none of its
// `ExtenderConfigs`, keeps none of the custom extenders a later
// `SetCustomExtenders` names, and offers no configured extender to the h3
// carrier. The same settings without the refusal take all three.
func TestClientStrategyDisableManualExtendersRefusesCustomAndConfiguredExtenders(t *testing.T) {
	clock := newTestClock()
	customIp := netip.MustParseAddr("198.51.100.7")
	configure := func(disableManualExtenders bool) func(settings *ClientStrategySettings) {
		return func(settings *ClientStrategySettings) {
			// no direct path, which is when the h3 carrier asks for an extender
			settings.EnableNormal = false
			settings.EnableResilient = false
			settings.ExtenderConfigs = []*ExtenderConfig{{
				Profile: ExtenderProfile{
					ConnectMode: ExtenderConnectModeTcpTls,
					Port:        ExtenderTcpPort,
				},
				Ip: netip.MustParseAddr("192.0.2.110"),
			}}
			settings.DisableManualExtenders = disableManualExtenders
		}
	}

	allowing, _, _ := newTestExtenderStrategy(t, clock, configure(false))
	allowing.SetCustomExtenders(map[netip.Addr]string{customIp: "secret"})
	if customExtenders := allowing.CustomExtenders(); len(customExtenders) != 1 {
		t.Fatalf("a strategy that allows manual extenders has %d custom extenders, expected 1", len(customExtenders))
	}
	if dialers := testExtenderDialers(allowing); len(dialers) != 1 {
		t.Fatalf("a strategy that allows manual extenders has %d configured extender dialers, expected 1", len(dialers))
	}
	if allowing.H3ExtenderConfig() == nil {
		t.Fatal("a strategy that allows manual extenders offers no configured extender to h3")
	}
	if expandedDialers := allowing.expandExtenderDialers(); len(expandedDialers) == 0 || expandedDialers[0].extenderConfig.Ip != customIp {
		t.Fatalf("a strategy that allows manual extenders did not expand the custom extender: %d dialers", len(expandedDialers))
	}

	refusing, _, _ := newTestExtenderStrategy(t, clock, configure(true))
	refusing.SetCustomExtenders(map[netip.Addr]string{customIp: "secret"})
	if customExtenders := refusing.CustomExtenders(); len(customExtenders) != 0 {
		t.Fatalf("a strategy that refuses manual extenders kept %d custom extenders", len(customExtenders))
	}
	if dialers := testExtenderDialers(refusing); len(dialers) != 0 {
		t.Fatalf("a strategy that refuses manual extenders took %d configured extender dialers", len(dialers))
	}
	if refusing.H3ExtenderConfig() != nil {
		t.Fatal("a strategy that refuses manual extenders offered a configured extender to h3")
	}
	if expandedDialers := refusing.expandExtenderDialers(); len(expandedDialers) != 0 {
		t.Fatalf("a strategy that refuses manual extenders expanded %d dialers from an empty directory", len(expandedDialers))
	}
}

// A strategy that refuses extenders configured by hand expands only what a
// signed record verifies. A manual address that no record verifies, the one
// unverified kind a strategy otherwise dials
// (TestClientStrategyExpandsOnlyManualUnverifiedAddresses), is never drawn,
// and does not crowd out the verified address behind it. A manual address a
// record verifies is an operator extender like any other, and is drawn.
func TestClientStrategyDisableManualExtendersExpandsOnlyVerifiedAddresses(t *testing.T) {
	clock := newTestClock()
	clientStrategy, directory, rootPrivateKey := newTestExtenderStrategy(t, clock, func(settings *ClientStrategySettings) {
		settings.DisableManualExtenders = true
	})

	manualIp := netip.MustParseAddr("192.0.2.108")
	directory.AddManual(manualIp)
	if candidates := directory.Candidates(0, 4); len(candidates) != 1 || candidates[0].Ip != manualIp {
		t.Fatalf("the directory does not offer the manual address: %d candidates", len(candidates))
	}
	if candidates := directory.VerifiedCandidates(0, 4); len(candidates) != 0 {
		t.Fatalf("the verified candidates include %d unverified addresses", len(candidates))
	}
	if expandedDialers := clientStrategy.expandExtenderDialers(); len(expandedDialers) != 0 {
		t.Fatalf("dialers = %d, expected none for a manual address no record verifies", len(expandedDialers))
	}

	verifiedIp := netip.MustParseAddr("192.0.2.100")
	verifiedRecord := signTestRecord(
		t,
		rootPrivateKey,
		newTestExtenderKey(t),
		clock.Now(),
		clock.Now().Add(14*24*time.Hour),
		testExtenderAddress(verifiedIp.String(), ExtenderCarrierTcp),
	)
	if _, err := directory.ApplyRecord(verifiedRecord, ExtenderSourceFeed); err != nil {
		t.Fatal(err)
	}
	// a count of one would be taken by the manual address if it were drawn
	if candidates := directory.VerifiedCandidates(0, 1); len(candidates) != 1 || candidates[0].Ip != verifiedIp {
		t.Fatalf("the verified candidates are not the verified address: %+v", candidates)
	}
	expandedDialers := clientStrategy.expandExtenderDialers()
	if len(expandedDialers) != 1 || expandedDialers[0].extenderConfig.Ip != verifiedIp {
		t.Fatalf("expanded %d dialers, expected only the verified address", len(expandedDialers))
	}

	manualRecord := signTestRecord(
		t,
		rootPrivateKey,
		newTestExtenderKey(t),
		clock.Now(),
		clock.Now().Add(14*24*time.Hour),
		testExtenderAddress(manualIp.String(), ExtenderCarrierTcp),
	)
	if _, err := directory.ApplyRecord(manualRecord, ExtenderSourceFeed); err != nil {
		t.Fatal(err)
	}
	expandedDialers = clientStrategy.expandExtenderDialers()
	if len(expandedDialers) != 1 || expandedDialers[0].extenderConfig.Ip != manualIp {
		t.Fatalf("expanded %d dialers, expected the manual address once a record verifies it", len(expandedDialers))
	}
}

// A strategy that refuses custom DoH servers keeps the built-in servers
// whatever settings name, in the settings it exposes and in the internal DoH
// cache its control names resolve through: not the bootstrap DoH servers a user
// names at construction, and not those of a later `SetInternalDohSettings`. The
// same settings without the refusal take the named servers first.
func TestClientStrategyDisableCustomDohServersKeepsBuiltInServers(t *testing.T) {
	namedIpv4 := "https://192.0.2.53/dns-query"
	namedIpv6 := "https://[2001:db8::53]/dns-query"
	builtInSettings := DefaultDnsResolverSettings()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	internalDohSettings := func(strategy *ClientStrategy) *DohSettings {
		resolver := strategy.internalDohResolver
		resolver.stateLock.Lock()
		defer resolver.stateLock.Unlock()
		return resolver.cache.settings
	}
	namesNamedServer := func(settings *DohSettings) bool {
		resolverSettings := settings.DnsResolverSettings
		for _, dohUrls := range [][]string{
			resolverSettings.RemoteDohUrlsIpv4,
			resolverSettings.RemoteDohUrlsIpv6,
			resolverSettings.LocalDohUrlsIpv4,
			resolverSettings.LocalDohUrlsIpv6,
		} {
			if slices.Contains(dohUrls, namedIpv4) || slices.Contains(dohUrls, namedIpv6) {
				return true
			}
		}
		return 0 < len(settings.ServerStatsSeed)
	}
	newStrategySettings := func(disableCustomDohServers bool) *ClientStrategySettings {
		settings := DefaultClientStrategySettings()
		settings.InternalDohDomains = []string{"api.space.example"}
		settings.DohSettings = ControlDohSettings([]string{namedIpv4}, []string{namedIpv6})
		settings.DisableCustomDohServers = disableCustomDohServers
		return settings
	}

	allowing := NewClientStrategy(ctx, newStrategySettings(false))
	defer allowing.Close()
	if !namesNamedServer(allowing.DohSettings()) || !namesNamedServer(internalDohSettings(allowing)) {
		t.Fatal("a strategy that allows custom DoH servers does not query the named ones")
	}

	refusingSettings := newStrategySettings(true)
	refusing := NewClientStrategy(ctx, refusingSettings)
	defer refusing.Close()
	for i, settings := range []*DohSettings{refusing.DohSettings(), internalDohSettings(refusing)} {
		if namesNamedServer(settings) {
			t.Fatalf("settings %d of a strategy that refuses custom DoH servers name a custom server", i)
		}
		if !slices.Equal(settings.DnsResolverSettings.RemoteDohUrlsIpv4, builtInSettings.RemoteDohUrlsIpv4) ||
			!slices.Equal(settings.DnsResolverSettings.RemoteDohUrlsIpv6, builtInSettings.RemoteDohUrlsIpv6) {
			t.Fatalf("settings %d of a strategy that refuses custom DoH servers are not the built-in servers", i)
		}
	}
	if refusingSettings.DohSettings.DnsResolverSettings.RemoteDohUrlsIpv4[0] != namedIpv4 {
		t.Fatal("the caller's settings must not change")
	}

	refusing.SetInternalDohSettings(ControlDohSettings([]string{namedIpv4}, []string{namedIpv6}))
	if namesNamedServer(refusing.DohSettings()) || namesNamedServer(internalDohSettings(refusing)) {
		t.Fatal("a strategy that refuses custom DoH servers took them from SetInternalDohSettings")
	}
}

// A family-pinned direct strategy keeps its extenders and VLESS servers
// dropped after construction: a later `SetVlessConfigs` or
// `SetCustomExtenders` adds none.
func TestDirectClientStrategyRefusesLaterVlessAndCustomExtenders(t *testing.T) {
	settings := DefaultClientStrategySettings()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	strategy := NewDirectClientStrategy(ctx, settings, 4)
	defer strategy.Close()

	strategy.SetVlessConfigs([]*VlessConfig{{
		Address:  "192.0.2.1",
		Port:     443,
		Id:       testVlessUserId,
		Network:  VlessNetworkTcp,
		Security: VlessSecurityNone,
	}})
	if vlessConfigs := strategy.VlessConfigs(); len(vlessConfigs) != 0 {
		t.Fatalf("direct strategy took %d VLESS dialers after construction", len(vlessConfigs))
	}
	strategy.SetCustomExtenders(map[netip.Addr]string{netip.MustParseAddr("198.51.100.7"): "secret"})
	if customExtenders := strategy.CustomExtenders(); len(customExtenders) != 0 {
		t.Fatalf("direct strategy took %d custom extenders after construction", len(customExtenders))
	}
	if settings.DisableVless || settings.DisableManualExtenders {
		t.Fatal("the caller's settings must not change")
	}
}
