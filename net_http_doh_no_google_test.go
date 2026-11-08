package connect

import (
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// Google Public DNS is no resolver the product picks for a user (owner
// decision 2026-11-08, support inbox 8236). These tests read every default
// resolver list, the regional and ingress (bootstrap) chains built from them,
// and the platform fallback, so an operator removed from the defaults cannot
// survive in a copy. A user who names Google in their own settings is not
// covered: that list is theirs. Nor is the block-action ignore baseline, which
// is no resolver list and keeps Google so that rules never capture a device's
// own Google DNS traffic (TestResolverIgnoreBaselineKeepsWellKnownResolvers).

// Whether a resolver list entry names Google Public DNS: one of its
// published anycast addresses (v4, or in its v6 /48,
// dns64 included), a DoH url on one, or one of its host names. The addresses
// are the operator's public service constants, the subject of the test, not
// live configuration.
func testIsGoogleDnsEndpoint(entry string) bool {
	host := strings.TrimSpace(entry)
	if u, err := url.Parse(host); err == nil && u.Host != "" {
		host = u.Hostname()
	} else if addrPort, err := netip.ParseAddrPort(host); err == nil {
		host = addrPort.Addr().String()
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if addr, err := netip.ParseAddr(host); err == nil {
		addr = addr.Unmap()
		return addr == netip.MustParseAddr("8.8.8.8") ||
			addr == netip.MustParseAddr("8.8.4.4") ||
			netip.MustParsePrefix("2001:4860:4860::/48").Contains(addr)
	}
	return host == "dns.google" || strings.HasSuffix(host, ".dns.google") ||
		host == "dns.google.com" || strings.HasPrefix(host, "google-public-dns-")
}

// The detector itself, so a passing scan below means no Google entry rather
// than a detector that matches nothing.
func TestIsGoogleDnsEndpointDetectsEveryForm(t *testing.T) {
	google := []string{
		"8.8.8.8",
		"8.8.4.4",
		"8.8.8.8:53",
		"2001:4860:4860::8888",
		"2001:4860:4860::8844",
		"2001:4860:4860::6464",
		"[2001:4860:4860::8888]:53",
		"::ffff:8.8.8.8",
		"https://8.8.8.8/dns-query",
		"https://[2001:4860:4860::8844]/dns-query",
		"https://dns.google/dns-query",
		"https://dns64.dns.google/dns-query",
		"dns.google.",
		"dns.google.com",
		"google-public-dns-a.google.com",
	}
	for _, entry := range google {
		if !testIsGoogleDnsEndpoint(entry) {
			t.Errorf("%q is not detected as Google Public DNS", entry)
		}
	}
	other := []string{
		"1.1.1.1",
		"9.9.9.9",
		"208.67.222.222",
		"8.8.8.9",
		"2606:4700:4700::1111",
		"2001:4860:4861::8888",
		"https://9.9.9.9/dns-query",
		"https://dns.quad9.net/dns-query",
		"www.google.com",
		"192.0.2.53",
	}
	for _, entry := range other {
		if testIsGoogleDnsEndpoint(entry) {
			t.Errorf("%q is detected as Google Public DNS", entry)
		}
	}
}

// Each server list of resolver settings, by name.
func testResolverLists(name string, resolver *DnsResolverSettings) map[string][]string {
	if resolver == nil {
		return map[string][]string{}
	}
	return map[string][]string{
		name + ".RemoteDohUrlsIpv4": resolver.RemoteDohUrlsIpv4,
		name + ".RemoteDohUrlsIpv6": resolver.RemoteDohUrlsIpv6,
		name + ".LocalDohUrlsIpv4":  resolver.LocalDohUrlsIpv4,
		name + ".LocalDohUrlsIpv6":  resolver.LocalDohUrlsIpv6,
		name + ".RemoteDnsIpv4":     resolver.RemoteDnsIpv4,
		name + ".RemoteDnsIpv6":     resolver.RemoteDnsIpv6,
		name + ".LocalDnsIpv4":      resolver.LocalDnsIpv4,
		name + ".LocalDnsIpv6":      resolver.LocalDnsIpv6,
	}
}

func testErrorGoogleEntries(t *testing.T, lists map[string][]string) {
	t.Helper()
	for name, list := range lists {
		for _, entry := range list {
			if testIsGoogleDnsEndpoint(entry) {
				t.Errorf("%s names Google Public DNS: %s", name, entry)
			}
		}
	}
}

// Every built-in resolver list: the defaults the tunnel's DoH and the
// host-side fallback ("fast DNS on connect") are built from, the built-in
// servers a strategy that refuses custom servers falls back to, the
// control-name resolver's copy, and the Linux and Windows egress fallback.
func TestNoDefaultResolverListNamesGoogleDns(t *testing.T) {
	lists := map[string][]string{}
	add := func(more map[string][]string) {
		for name, list := range more {
			lists[name] = list
		}
	}
	add(testResolverLists("DefaultDnsResolverSettings", DefaultDnsResolverSettings()))
	add(testResolverLists("DefaultDohSettings", DefaultDohSettings().DnsResolverSettings))
	muxSettings := DefaultUpgradeMuxSettings()
	if muxSettings == nil || muxSettings.Dns == nil || muxSettings.Dns.Resolver == nil {
		t.Fatal("default mux resolver missing")
	}
	add(testResolverLists("DefaultUpgradeMuxSettings", muxSettings.Dns.Resolver))
	add(testResolverLists("DefaultDnsUpgradeFallbackSettings", DefaultDnsUpgradeFallbackSettings()))
	add(testResolverLists("builtInDohServerSettings", builtInDohServerSettings(nil).DnsResolverSettings))
	add(testResolverLists("internalDohSettings", internalDohSettings(nil, []string{"api.example.test"}).DnsResolverSettings))
	lists["egressFallbackDnsServers"] = egressFallbackDnsServers
	testErrorGoogleEntries(t, lists)

	// the remote lists still carry the three remaining operators in both
	// families, so removing one did not empty a family
	resolver := DefaultDnsResolverSettings()
	for name, list := range map[string][]string{
		"RemoteDohUrlsIpv4": resolver.RemoteDohUrlsIpv4,
		"RemoteDohUrlsIpv6": resolver.RemoteDohUrlsIpv6,
		"RemoteDnsIpv4":     resolver.RemoteDnsIpv4,
		"RemoteDnsIpv6":     resolver.RemoteDnsIpv6,
	} {
		if len(list) < 3 {
			t.Errorf("default %s = %v, want at least three operators", name, list)
		}
	}
}

// The regional recommendations, the bootstrap DoH presets, and for every
// ingress country (and undisclosed) the lists a control-name lookup fans out
// to and the plain servers of its last-resort bootstrap fallback.
func TestNoRegionalOrBootstrapListNamesGoogleDns(t *testing.T) {
	lists := map[string][]string{}
	for _, server := range RegionalDnsServers() {
		key := "regionalDnsServers." + server.CountryCode + "." + server.Name
		lists[key] = []string{server.Ipv4, server.Ipv6}
	}
	countryCodes := slices.Concat(IngressCountryCodes(), []string{"", "zz"})
	for _, countryCode := range countryCodes {
		prefix := "[" + countryCode + "]"
		for name, list := range testResolverLists(prefix+"RegionalDnsResolverSettings", RegionalDnsResolverSettings(countryCode)) {
			lists[name] = list
		}
		presetsIpv4, presetsIpv6 := RegionalControlDohUrls(countryCode)
		lists[prefix+"RegionalControlDohUrls.Ipv4"] = presetsIpv4
		lists[prefix+"RegionalControlDohUrls.Ipv6"] = presetsIpv6
		strategy := IngressDnsStrategyForCountry(countryCode)
		lists[prefix+"IngressDnsStrategy.PlainDnsIpv4"] = strategy.PlainDnsIpv4
		lists[prefix+"IngressDnsStrategy.PlainDnsIpv6"] = strategy.PlainDnsIpv6
		ingressSettings := IngressDohSettings(countryCode, nil, nil)
		for _, ipVersion := range []int{0, 4, 6} {
			lists[fmt.Sprintf("%sremoteDohUrls.%d", prefix, ipVersion)] = remoteDohUrls(ingressSettings, ipVersion)
		}
		for _, network := range []string{"udp", "udp4", "udp6", "tcp"} {
			lists[prefix+"bootstrapPlainDnsServers."+network] = bootstrapPlainDnsServers(ingressSettings, network)
		}
	}
	testErrorGoogleEntries(t, lists)
}

// Removing Google left no regional recommendation empty: every country that
// had one keeps one, stays offered as a connection region, and pairs each v6
// server with its v4 one so the dual-stack walk keeps both.
func TestRegionalRecommendationsSurviveGoogleRemoval(t *testing.T) {
	ingressCountryCodes := IngressCountryCodes()
	for _, countryCode := range []string{"cn", "ru", "ir", "tr", "kz", "tm"} {
		if !slices.Contains(ingressCountryCodes, countryCode) {
			t.Errorf("%s is no longer an ingress country: %v", countryCode, ingressCountryCodes)
		}
		ipv4s := RegionalDnsServerIps(countryCode)
		if len(ipv4s) == 0 || RegionalDnsResolverSettings(countryCode) == nil {
			t.Errorf("%s lost its regional recommendation", countryCode)
		}
		siblings := familySiblings(ipv4s, RegionalDnsServerIpv6s(countryCode), knownDnsServerPairs)
		for _, server := range RegionalDnsServers() {
			if server.CountryCode != countryCode || server.Ipv6 == "" {
				continue
			}
			if siblings[server.Ipv4] != server.Ipv6 {
				t.Errorf("%s %s: v6 %s does not pair with v4 %s", countryCode, server.Name, server.Ipv6, server.Ipv4)
			}
		}
	}
}

// Google Public DNS left every default list, but not the block-action ignore
// baseline (wellKnownResolverAddrs): a device's os or browser still queries it
// directly (Android private dns, a browser's secure dns), and a user's
// override rule that captured that flow could cut off all of the device's
// dns. The baseline holds every address of the well known operators, second
// addresses included, and every address the defaults name; the exported copy
// the blocker generator subtracts is the same set.
func TestResolverIgnoreBaselineKeepsWellKnownResolvers(t *testing.T) {
	baseline := wellKnownResolverAddrs()
	googleAddrs := []string{"8.8.8.8", "8.8.4.4", "2001:4860:4860::8888", "2001:4860:4860::8844"}
	otherAddrs := []string{
		"1.1.1.1", "1.0.0.1", "2606:4700:4700::1111", "2606:4700:4700::1001",
		"9.9.9.9", "149.112.112.112", "2620:fe::fe", "2620:fe::9",
		"208.67.222.222", "208.67.220.220", "2620:119:35::35", "2620:119:53::53",
	}
	for _, resolver := range slices.Concat(googleAddrs, otherAddrs) {
		if !baseline[netip.MustParseAddr(resolver)] {
			t.Errorf("well known resolver %s is not in the ignore baseline", resolver)
		}
	}
	for _, google := range googleAddrs {
		if !testIsGoogleDnsEndpoint(google) {
			t.Errorf("%s is not detected as Google Public DNS", google)
		}
	}

	resolver := DefaultDnsResolverSettings()
	defaultHosts := slices.Concat(resolver.RemoteDnsIpv4, resolver.RemoteDnsIpv6, resolver.LocalDnsIpv4, resolver.LocalDnsIpv6)
	for _, dohUrl := range slices.Concat(resolver.RemoteDohUrlsIpv4, resolver.RemoteDohUrlsIpv6) {
		u, err := url.Parse(dohUrl)
		if err != nil {
			t.Fatal(err)
		}
		defaultHosts = append(defaultHosts, u.Hostname())
	}
	for _, host := range defaultHosts {
		if addr := netip.MustParseAddr(host); !baseline[addr] {
			t.Errorf("default resolver address %s is not in the ignore baseline", addr)
		}
	}

	exported := WellKnownResolverAddrs()
	if len(exported) != len(baseline) {
		t.Fatalf("WellKnownResolverAddrs has %d addresses, the baseline %d", len(exported), len(baseline))
	}
	for i, addr := range exported {
		if !baseline[addr] {
			t.Errorf("WellKnownResolverAddrs holds %s, which is not in the baseline", addr)
		}
		if 0 < i && 0 <= exported[i-1].Compare(addr) {
			t.Errorf("WellKnownResolverAddrs is not sorted and unique at %s", addr)
		}
	}
}
