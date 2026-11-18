package connect

import (
	"context"
	"net/netip"
	"slices"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// DNS.SB joined the default resolvers (owner decision 2026-11-09), after
// Cloudflare and Quad9, at both of its published servers. These tests pin what
// a default operator is: on every list built from the defaults, paired with
// its own v6 endpoints, a built-in endpoint under the policy-filter guard, and
// in the block-action ignore baseline. The addresses are the operator's public
// service constants, the subject of the tests, not live configuration. The
// fake servers answer by host, with no network.

// DNS.SB's DoH urls and plain servers, v4 and v6, in pair order: public-dns-a
// then public-dns-b.
var (
	testDnsSbDohUrlsIpv4 = []string{"https://185.222.222.222/dns-query", "https://45.11.45.11/dns-query"}
	testDnsSbDohUrlsIpv6 = []string{"https://[2a09::]/dns-query", "https://[2a11::]/dns-query"}
	testDnsSbDnsIpv4     = []string{"185.222.222.222", "45.11.45.11"}
	testDnsSbDnsIpv6     = []string{"2a09::", "2a11::"}
)

// Every list built from the defaults carries DNS.SB: the tunnel resolver of
// the mux, the host-side projection of Fast DNS on connect, the control-name
// resolver and its built-in copy, the internal resolver, the ingress lists of
// an undisclosed region, and the Linux and Windows egress fallback.
func TestDnsSbIsOnEveryListBuiltFromTheDefaults(t *testing.T) {
	containsAll := func(name string, list []string, want []string) {
		t.Helper()
		for _, entry := range want {
			if !slices.Contains(list, entry) {
				t.Errorf("%s lacks DNS.SB's %s: %v", name, entry, list)
			}
		}
	}
	muxSettings := DefaultUpgradeMuxSettings()
	if muxSettings == nil || muxSettings.Dns == nil || muxSettings.Dns.Resolver == nil {
		t.Fatal("default mux resolver missing")
	}
	mux := muxSettings.Dns.Resolver
	containsAll("mux RemoteDohUrlsIpv4", mux.RemoteDohUrlsIpv4, testDnsSbDohUrlsIpv4)
	containsAll("mux RemoteDohUrlsIpv6", mux.RemoteDohUrlsIpv6, testDnsSbDohUrlsIpv6)
	containsAll("mux RemoteDnsIpv4", mux.RemoteDnsIpv4, testDnsSbDnsIpv4)
	containsAll("mux RemoteDnsIpv6", mux.RemoteDnsIpv6, testDnsSbDnsIpv6)

	fallback := DefaultDnsUpgradeFallbackSettings()
	containsAll("fallback LocalDohUrlsIpv4", fallback.LocalDohUrlsIpv4, testDnsSbDohUrlsIpv4)
	containsAll("fallback LocalDohUrlsIpv6", fallback.LocalDohUrlsIpv6, testDnsSbDohUrlsIpv6)

	for name, resolver := range map[string]*DnsResolverSettings{
		"DefaultDohSettings":       DefaultDohSettings().DnsResolverSettings,
		"builtInDohServerSettings": builtInDohServerSettings(nil).DnsResolverSettings,
		"internalDohSettings":      internalDohSettings(nil, []string{"api.example.test"}).DnsResolverSettings,
	} {
		containsAll(name+" RemoteDohUrlsIpv4", resolver.RemoteDohUrlsIpv4, testDnsSbDohUrlsIpv4)
		containsAll(name+" RemoteDohUrlsIpv6", resolver.RemoteDohUrlsIpv6, testDnsSbDohUrlsIpv6)
	}
	containsAll("ingress dual-stack DoH", remoteDohUrls(IngressDohSettings("", nil, nil), 0), slices.Concat(testDnsSbDohUrlsIpv4, testDnsSbDohUrlsIpv6))
	containsAll("egressFallbackDnsServers", egressFallbackDnsServers, testDnsSbDnsIpv4)
}

// Each DNS.SB v4 endpoint pairs with its own v6 endpoint by the pair tables,
// public-dns-a and public-dns-b, so a fan-out races the pair as one wave. In a
// user's list that names DNS.SB beside their own servers, DNS.SB still pairs
// with its own endpoints and the user's servers with each other, which index
// pairing alone would cross.
func TestDnsSbPairsWithItsOwnIpv6Endpoints(t *testing.T) {
	for i := range testDnsSbDohUrlsIpv4 {
		pair := [2]string{testDnsSbDohUrlsIpv4[i], testDnsSbDohUrlsIpv6[i]}
		if !slices.Contains(knownDohUrlPairs, pair) {
			t.Errorf("knownDohUrlPairs lacks %v", pair)
		}
		dnsPair := [2]string{testDnsSbDnsIpv4[i], testDnsSbDnsIpv6[i]}
		if !slices.Contains(knownDnsServerPairs, dnsPair) {
			t.Errorf("knownDnsServerPairs lacks %v", dnsPair)
		}
	}

	custom4 := "https://192.0.2.81/dns-query"
	custom6 := "https://[2001:db8::81]/dns-query"
	siblings := familySiblings(
		[]string{testDnsSbDohUrlsIpv4[0], custom4},
		[]string{custom6, testDnsSbDohUrlsIpv6[0]},
		knownDohUrlPairs,
	)
	want := map[string]string{
		testDnsSbDohUrlsIpv4[0]: testDnsSbDohUrlsIpv6[0],
		testDnsSbDohUrlsIpv6[0]: testDnsSbDohUrlsIpv4[0],
		custom4:                 custom6,
		custom6:                 custom4,
	}
	if len(siblings) != len(want) {
		t.Fatalf("siblings = %v, want %v", siblings, want)
	}
	for entry, sibling := range want {
		if siblings[entry] != sibling {
			t.Errorf("%s pairs with %q, want %s", entry, siblings[entry], sibling)
		}
	}

	dnsSiblings := familySiblings(
		[]string{testDnsSbDnsIpv4[1], "192.0.2.82"},
		[]string{"2001:db8::82", testDnsSbDnsIpv6[1]},
		knownDnsServerPairs,
	)
	if dnsSiblings[testDnsSbDnsIpv4[1]] != testDnsSbDnsIpv6[1] || dnsSiblings["192.0.2.82"] != "2001:db8::82" {
		t.Errorf("plain dns siblings = %v, want DNS.SB with its own v6 address and the custom pair together", dnsSiblings)
	}
}

// Every DNS.SB endpoint is a built-in endpoint, in any spelling of it, so the
// policy-filter guard applies: its filtered response (an extended dns error of
// 4 or 15-18, or only 0.0.0.0 or ::) never ends the race, earns no success
// credit and is never cached, and a lone NXDOMAIN from it is cached for
// LoneMissExpiration. Its genuine answers stay answers: DNS.SB marks a
// synthesized NXDOMAIN with extended error 29, which keeps it an authoritative
// miss.
func TestDnsSbEndpointsAreGuardedBuiltInEndpoints(t *testing.T) {
	for _, dohUrl := range slices.Concat(
		testDnsSbDohUrlsIpv4,
		testDnsSbDohUrlsIpv6,
		[]string{
			"https://185.222.222.222:443/dns-query/",
			"HTTPS://45.11.45.11/dns-query",
			"https://[2a09:0:0:0:0:0:0:0]/dns-query",
			"https://[2A11::]/dns-query",
		},
	) {
		if !dohEndpointGuarded(dohUrl) {
			t.Errorf("%s is not a guarded built-in endpoint", dohUrl)
		}
	}
	for _, dohUrl := range []string{"https://185.222.222.222:8443/dns-query", "https://45.11.45.11/other-path"} {
		if dohEndpointGuarded(dohUrl) {
			t.Errorf("%s is guarded, want another endpoint", dohUrl)
		}
	}

	dnsSbA := testDnsSbDohUrlsIpv4[0]
	dnsSbB := testDnsSbDohUrlsIpv4[1]
	for _, c := range []struct {
		name       string
		recordType string
		answer     testingDohPolicyAnswer
	}{
		{name: "0.0.0.0 without ede", recordType: "A", answer: testingDohNoError(-1, netip.MustParseAddr("0.0.0.0"))},
		{name: ":: without ede", recordType: "AAAA", answer: testingDohNoError(-1, netip.MustParseAddr("::"))},
		{name: "nxdomain ede 16 censored", recordType: "A", answer: testingDohNxdomain(16)},
		{name: "refused ede 18 prohibited", recordType: "A", answer: testingDohPolicyAnswer{rcode: dnsmessage.RCodeRefused, ede: 18}},
	} {
		cache := testingDohPolicyCache(t, map[string]testingDohPolicyAnswer{
			"185.222.222.222": c.answer,
			"45.11.45.11":     testingDohFailure(),
		}, []string{dnsSbA, dnsSbB}, nil)
		addrs, authoritative := cache.QueryResult(context.Background(), c.recordType, "filtered.example")
		if authoritative || len(addrs) != 0 {
			t.Errorf("%s: lookup = %v authoritative %t, want a SERVFAIL", c.name, addrs, authoritative)
		}
		if entry := testingDohCacheEntry(cache, c.recordType, "filtered.example"); entry != nil {
			t.Errorf("%s: cached %+v, want nothing", c.name, entry)
		}
		if score := cache.remoteClient.stats.scores()[dnsSbA]; 0 < score {
			t.Errorf("%s: the filtering endpoint earned success credit %f", c.name, score)
		}
	}

	// a lone nxdomain, with the other server failing, is cached briefly
	cache := testingDohPolicyCache(t, map[string]testingDohPolicyAnswer{
		"185.222.222.222": testingDohNxdomain(-1),
		"45.11.45.11":     testingDohFailure(),
	}, []string{dnsSbA, dnsSbB}, nil)
	if addrs, authoritative := cache.QueryResult(context.Background(), "A", "absent.example"); !authoritative || len(addrs) != 0 {
		t.Fatalf("lone nxdomain = %v authoritative %t, want an authoritative miss", addrs, authoritative)
	}
	entry := testingDohCacheEntry(cache, "A", "absent.example")
	if entry == nil || !entry.Miss {
		t.Fatalf("lone nxdomain cached %+v, want a miss", entry)
	}
	afterLone := entry.Time.Add(cache.settings.LoneMissExpiration + time.Second)
	if entry.Valid(afterLone, cache.settings.MissExpiration) {
		t.Errorf("a lone nxdomain from DNS.SB is cached past LoneMissExpiration %s", cache.settings.LoneMissExpiration)
	}

	// a synthesized nxdomain (ede 29) from both servers is an authoritative miss
	cache = testingDohPolicyCache(t, map[string]testingDohPolicyAnswer{
		"185.222.222.222": testingDohNxdomain(29),
		"45.11.45.11":     testingDohNxdomain(29),
	}, []string{dnsSbA, dnsSbB}, nil)
	if addrs, authoritative := cache.QueryResult(context.Background(), "A", "synthesized.invalid"); !authoritative || len(addrs) != 0 {
		t.Fatalf("ede 29 nxdomain = %v authoritative %t, want an authoritative miss", addrs, authoritative)
	}
	if entry := testingDohCacheEntry(cache, "A", "synthesized.invalid"); entry == nil || !entry.Miss {
		t.Fatalf("ede 29 nxdomain cached %+v, want a miss", entry)
	}
}
