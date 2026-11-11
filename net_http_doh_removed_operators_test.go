package connect

import (
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// Google Public DNS (owner decision 2026-11-08, support inbox 8236) and OpenDNS
// (it refuses French clients, has left Portugal and Belgium, answers blocks
// with ordinary records, forwards the client's /24 as ECS, and its free terms
// are marked expired) are no resolvers the product picks for a user. These
// tests read every default resolver list, the regional and ingress (bootstrap)
// chains built from them, and the platform fallback, so an operator removed
// from the defaults cannot survive in a copy. A user who names one in their
// own settings is not covered: that list is theirs. Nor is the block-action
// ignore baseline, which is no resolver list and keeps both so that rules
// never capture a device's own traffic to them
// (TestResolverIgnoreBaselineKeepsWellKnownResolvers).

// The removed operator a resolver list entry names, or "" for none: one of
// Google Public DNS's published anycast addresses (v4, or in its v6 /48, dns64
// included) or OpenDNS's (its v4 /21 and v6 prefixes), a DoH url on one, or
// one of their host names. The addresses are the operators' public service
// constants, the subject of the test, not live configuration.
func testRemovedResolverOperator(entry string) string {
	host := strings.TrimSpace(entry)
	if u, err := url.Parse(host); err == nil && u.Host != "" {
		host = u.Hostname()
	} else if addrPort, err := netip.ParseAddrPort(host); err == nil {
		host = addrPort.Addr().String()
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if addr, err := netip.ParseAddr(host); err == nil {
		addr = addr.Unmap()
		switch {
		case addr == netip.MustParseAddr("8.8.8.8"),
			addr == netip.MustParseAddr("8.8.4.4"),
			netip.MustParsePrefix("2001:4860:4860::/48").Contains(addr):
			return "Google Public DNS"
		case netip.MustParsePrefix("208.67.216.0/21").Contains(addr),
			netip.MustParsePrefix("2620:119::/32").Contains(addr),
			netip.MustParsePrefix("2620:0:ccc::/48").Contains(addr),
			netip.MustParsePrefix("2620:0:ccd::/48").Contains(addr):
			return "OpenDNS"
		}
		return ""
	}
	switch {
	case host == "dns.google", strings.HasSuffix(host, ".dns.google"),
		host == "dns.google.com", strings.HasPrefix(host, "google-public-dns-"):
		return "Google Public DNS"
	case host == "opendns.com", strings.HasSuffix(host, ".opendns.com"):
		return "OpenDNS"
	}
	return ""
}

// The detector itself, so a passing scan below means no removed operator
// rather than a detector that matches nothing.
func TestRemovedOperatorDetectorDetectsEveryForm(t *testing.T) {
	removed := map[string]string{
		"8.8.8.8":                                  "Google Public DNS",
		"8.8.4.4":                                  "Google Public DNS",
		"8.8.8.8:53":                               "Google Public DNS",
		"2001:4860:4860::8888":                     "Google Public DNS",
		"2001:4860:4860::8844":                     "Google Public DNS",
		"2001:4860:4860::6464":                     "Google Public DNS",
		"[2001:4860:4860::8888]:53":                "Google Public DNS",
		"::ffff:8.8.8.8":                           "Google Public DNS",
		"https://8.8.8.8/dns-query":                "Google Public DNS",
		"https://dns.google/dns-query":             "Google Public DNS",
		"https://dns64.dns.google/dns-query":       "Google Public DNS",
		"https://[2001:4860:4860::8844]/dns-query": "Google Public DNS",
		"dns.google.":                              "Google Public DNS",
		"dns.google.com":                           "Google Public DNS",
		"google-public-dns-a.google.com":           "Google Public DNS",
		"208.67.222.222":                           "OpenDNS",
		"208.67.220.220":                           "OpenDNS",
		"208.67.222.123":                           "OpenDNS",
		"2620:119:35::35":                          "OpenDNS",
		"2620:119:53::53":                          "OpenDNS",
		"2620:0:ccc::2":                            "OpenDNS",
		"https://208.67.222.222/dns-query":         "OpenDNS",
		"https://[2620:119:35::35]/dns-query":      "OpenDNS",
		"https://doh.opendns.com/dns-query":        "OpenDNS",
		"208.67.222.222:53":                        "OpenDNS",
	}
	for entry, operator := range removed {
		if got := testRemovedResolverOperator(entry); got != operator {
			t.Errorf("%q is detected as %q, want %q", entry, got, operator)
		}
	}
	kept := []string{
		"1.1.1.1",
		"1.0.0.1",
		"9.9.9.9",
		"8.8.8.9",
		"208.67.224.1",
		"2606:4700:4700::1111",
		"2606:4700:4700::1001",
		"2620:fe::fe",
		"2001:4860:4861::8888",
		"https://9.9.9.9/dns-query",
		"https://dns.quad9.net/dns-query",
		"www.google.com",
		"192.0.2.53",
	}
	for _, entry := range kept {
		if got := testRemovedResolverOperator(entry); got != "" {
			t.Errorf("%q is detected as %q", entry, got)
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

func testErrorRemovedOperatorEntries(t *testing.T, lists map[string][]string) {
	t.Helper()
	for name, list := range lists {
		for _, entry := range list {
			if operator := testRemovedResolverOperator(entry); operator != "" {
				t.Errorf("%s names %s: %s", name, operator, entry)
			}
		}
	}
}

// Every built-in resolver list: the defaults the tunnel's DoH and the
// host-side fallback ("fast DNS on connect") are built from, the built-in
// servers a strategy that refuses custom servers falls back to, the
// control-name resolver's copy, and the Linux and Windows egress fallback.
func TestNoDefaultResolverListNamesARemovedOperator(t *testing.T) {
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
	testErrorRemovedOperatorEntries(t, lists)
}

// The default lists exactly: Cloudflare with both of its addresses, Quad9 on
// its unfiltered 9.9.9.10, never its filtering 9.9.9.9, then DNS.SB with both
// of its addresses (2026-11-09). Cloudflare and Quad9 interleave, so a list
// walked in order asks Quad9 second; DNS.SB follows them, and the sdk's
// migration of an older default list reproduces this order.
func TestDefaultResolverListsAreCloudflareQuad9AndDnsSb(t *testing.T) {
	resolver := DefaultDnsResolverSettings()
	for name, c := range map[string]struct {
		got  []string
		want []string
	}{
		"RemoteDohUrlsIpv4": {resolver.RemoteDohUrlsIpv4, []string{
			"https://1.1.1.1/dns-query", "https://9.9.9.10/dns-query", "https://1.0.0.1/dns-query",
			"https://185.222.222.222/dns-query", "https://45.11.45.11/dns-query",
		}},
		"RemoteDohUrlsIpv6": {resolver.RemoteDohUrlsIpv6, []string{
			"https://[2606:4700:4700::1111]/dns-query", "https://[2620:fe::10]/dns-query", "https://[2606:4700:4700::1001]/dns-query",
			"https://[2a09::]/dns-query", "https://[2a11::]/dns-query",
		}},
		"RemoteDnsIpv4":    {resolver.RemoteDnsIpv4, []string{"1.1.1.1", "9.9.9.10", "1.0.0.1", "185.222.222.222", "45.11.45.11"}},
		"RemoteDnsIpv6":    {resolver.RemoteDnsIpv6, []string{"2606:4700:4700::1111", "2620:fe::10", "2606:4700:4700::1001", "2a09::", "2a11::"}},
		"LocalDohUrlsIpv4": {resolver.LocalDohUrlsIpv4, nil},
		"LocalDohUrlsIpv6": {resolver.LocalDohUrlsIpv6, nil},
		"LocalDnsIpv4":     {resolver.LocalDnsIpv4, []string{"9.9.9.10", "1.1.1.1", "1.0.0.1", "185.222.222.222", "45.11.45.11"}},
		"LocalDnsIpv6":     {resolver.LocalDnsIpv6, []string{"2620:fe::10", "2606:4700:4700::1111", "2606:4700:4700::1001", "2a09::", "2a11::"}},
	} {
		if !slices.Equal(c.got, c.want) {
			t.Errorf("default %s = %v, want %v", name, c.got, c.want)
		}
	}
	// every v4 entry races its v6 sibling: the pair tables know both
	// Cloudflare pairs, Quad9's and both of DNS.SB's
	dohSiblings := familySiblings(resolver.RemoteDohUrlsIpv4, resolver.RemoteDohUrlsIpv6, knownDohUrlPairs)
	dnsSiblings := familySiblings(resolver.RemoteDnsIpv4, resolver.RemoteDnsIpv6, knownDnsServerPairs)
	localDnsSiblings := familySiblings(resolver.LocalDnsIpv4, resolver.LocalDnsIpv6, knownDnsServerPairs)
	for i := range resolver.RemoteDohUrlsIpv4 {
		if dohSiblings[resolver.RemoteDohUrlsIpv4[i]] != resolver.RemoteDohUrlsIpv6[i] {
			t.Errorf("doh %s does not pair with %s", resolver.RemoteDohUrlsIpv4[i], resolver.RemoteDohUrlsIpv6[i])
		}
		if dnsSiblings[resolver.RemoteDnsIpv4[i]] != resolver.RemoteDnsIpv6[i] {
			t.Errorf("dns %s does not pair with %s", resolver.RemoteDnsIpv4[i], resolver.RemoteDnsIpv6[i])
		}
	}
	for i := range resolver.LocalDnsIpv4 {
		if localDnsSiblings[resolver.LocalDnsIpv4[i]] != resolver.LocalDnsIpv6[i] {
			t.Errorf("local dns %s does not pair with %s", resolver.LocalDnsIpv4[i], resolver.LocalDnsIpv6[i])
		}
	}
}

// The regional recommendations, the bootstrap DoH presets, and for every
// ingress country (and undisclosed) the lists a control-name lookup fans out
// to and the plain servers of its last-resort bootstrap fallback.
func TestNoRegionalOrBootstrapListNamesARemovedOperator(t *testing.T) {
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
	testErrorRemovedOperatorEntries(t, lists)
}

// Removing operators left no regional recommendation empty: every country
// that had one keeps one, stays offered as a connection region, and pairs each
// v6 server with its v4 one so the dual-stack walk keeps both.
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

// kz's recommendation is Cloudflare and Quad9's unfiltered 9.9.9.10, as the
// defaults are, and no regional recommendation or bootstrap preset names
// Quad9's filtering 9.9.9.9 family.
func TestKzRecommendationIsCloudflareAndUnfilteredQuad9(t *testing.T) {
	if got, want := RegionalDnsServerIps("kz"), []string{"1.1.1.1", "9.9.9.10"}; !slices.Equal(got, want) {
		t.Errorf("kz v4 recommendation = %v, want %v", got, want)
	}
	if got, want := RegionalDnsServerIpv6s("kz"), []string{"2606:4700:4700::1111", "2620:fe::10"}; !slices.Equal(got, want) {
		t.Errorf("kz v6 recommendation = %v, want %v", got, want)
	}
	filteringQuad9 := []string{"9.9.9.9", "149.112.112.112", "2620:fe::fe", "2620:fe::9"}
	for _, server := range RegionalDnsServers() {
		if slices.Contains(filteringQuad9, server.Ipv4) || slices.Contains(filteringQuad9, server.Ipv6) {
			t.Errorf("%s %s names Quad9's filtering service: %s %s", server.CountryCode, server.Name, server.Ipv4, server.Ipv6)
		}
	}
	for _, server := range regionalControlDohServers {
		for _, dohUrl := range []string{server.DohUrlIpv4, server.DohUrlIpv6} {
			if u, err := url.Parse(dohUrl); err == nil && slices.Contains(filteringQuad9, u.Hostname()) {
				t.Errorf("%s %s names Quad9's filtering service: %s", server.CountryCode, server.Name, dohUrl)
			}
		}
	}
}

// Google Public DNS and OpenDNS left every default list, but not the
// block-action ignore baseline (wellKnownResolverAddrs): a device's os or
// browser still queries them directly (Android private dns, a browser's secure
// dns), and a user's override rule that captured that flow could cut off all
// of the device's dns. The baseline holds every address of the well known
// operators, second addresses included, and every address the defaults name;
// the exported copy the blocker generator subtracts is the same set.
func TestResolverIgnoreBaselineKeepsWellKnownResolvers(t *testing.T) {
	baseline := wellKnownResolverAddrs()
	removedAddrs := []string{
		"8.8.8.8", "8.8.4.4", "2001:4860:4860::8888", "2001:4860:4860::8844",
		"208.67.222.222", "208.67.220.220", "2620:119:35::35", "2620:119:53::53",
	}
	otherAddrs := []string{
		"1.1.1.1", "1.0.0.1", "2606:4700:4700::1111", "2606:4700:4700::1001",
		"9.9.9.10", "2620:fe::10", "9.9.9.9", "149.112.112.112", "2620:fe::fe", "2620:fe::9",
		"185.222.222.222", "45.11.45.11", "2a09::", "2a11::",
	}
	for _, resolver := range slices.Concat(removedAddrs, otherAddrs) {
		if !baseline[netip.MustParseAddr(resolver)] {
			t.Errorf("well known resolver %s is not in the ignore baseline", resolver)
		}
	}
	for _, removed := range removedAddrs {
		if testRemovedResolverOperator(removed) == "" {
			t.Errorf("%s is not detected as a removed operator", removed)
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
