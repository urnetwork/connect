package connect

import (
	"slices"
	"strings"
)

// a well known regional dns server, associated to a country code. Ipv6 is
// empty where the operator publishes no v6 resolver (or none is known).
type RegionalDnsServer struct {
	CountryCode string
	Name        string
	Ipv4        string
	Ipv6        string
}

// the well known regional dns servers, associated to country codes.
// these are suggestions that clients can surface when connected to the region.
//
// Each entry serves two paths. The egress recommendation
// (RegionalDnsResolverSettings), which the apps, the server's hosted proxies
// and its quality probe use, sends plain dns through the tunnel, so the
// server must answer queries from provider exits in the country, on any of
// its networks. The ingress fallback (IngressDnsStrategyForCountry) sends
// plain dns from the user's own network in the country. So an entry is a
// public address (isPublicUnicastAddr): the egress policy of both roles
// refuses any other destination (ip_security_public.go), and the plain dns
// path, which walks its servers one per resolver attempt, would spend an
// attempt of every other lookup on a query that never leaves the device. And
// an entry answers recursively from every network of its country, not only
// from its operator's own customers.
//
// No country lists Google Public DNS (owner decision 2026-11-08, see
// DefaultDnsResolverSettings). kz listed only Google; Cloudflare and Quad9,
// both default operators already, take its place. In OONI dnscheck from kz,
// Cloudflare's DoH answered every Oct-Nov 2026 measurement on three networks
// and Quad9's most of them, and the two networks that blocked DoH (Aug, Sep)
// blocked Google's too. Quad9 is its unfiltered 9.9.9.10, as in the defaults;
// the measurements were of its filtering 9.9.9.9, the same operator and
// anycast network.
//
// ir's recommendation is Shecan and Electro, open recursive resolvers in
// Iran (AS43754 and AS62442). Until 2026-11-11 it also named 403.online at
// 10.202.10.10, a private address that only Iranian networks route and that
// no query through the tunnel can reach. On 2026-11-11 both answered
// recursively from the US, 220-290 ms away and so in Iran, with the
// platform's names and common names right, and with Iran's rewrites
// (google.com as its SafeSearch address, some Cloudflare-hosted names as other
// Cloudflare addresses). URnetwork has no exit in Iran (provider-locations,
// 2026-11-11) and OONI does not query them, so neither is measured from
// inside. In OONI from Iran (Oct-Nov 2026) DoH to Cloudflare and Quad9 failed
// 99% of the time on nine networks, TCI and MCI among them, and plain UDP to
// 1.1.1.1 answered 94% of queries on eight networks (TCI, MCI, RighTel,
// Respina, Shatel, Farahoosh, FDI and Afagh), blocked names with Iran's
// injected 10.10.34.x. Shecan's second address (185.51.200.2) answers no
// recursive query from outside, and no v6 resolver of either is known.
//
// tr has no recommendation (owner decision 2026-11-11): DoH works in Turkey,
// so the apps never suggest unencrypted dns on a Turkish exit, and the
// defaults serve both the tunnel and the ingress fallback. Until 2026-11-11 tr
// named TTNET (195.175.39.39) and Turkcell Superonline (212.252.114.8), which
// answer only their own networks: through the 14 URnetwork exits in Turkey on
// 2026-11-11 (TurkNet, 2E Telekom, TTNET, two small networks and one that
// Cloudflare places in Germany), TTNET's resolvers answered only the 2E
// Telekom exit, Superonline's no exit, and TurkNet's own only TurkNet's, and
// only the ISP resolvers answered with Turkey's block address (discord.com as
// 195.175.254.2). 1.1.1.1 answered on all 13 exits that carried udp and
// 9.9.9.10 on 9, the other 4 all on one network, none intercepted. In OONI
// from Turkey (Oct-Nov 2026) DoH to Cloudflare and Quad9 answered on Türk
// Telekom, TT Mobil, Superonline, Turkcell and TurkNet, and plain UDP to them
// answered 99% of queries on Superonline, 97% on TurkNet and all on Turkcell
// mobile.
//
// tm has no recommendation. Until 2026-11-11 it named Turkmentelecom
// (217.174.238.141), which refuses recursion to clients outside its network
// and still answers them 127.0.0.1 for the names it blocks, bringyour.com and
// its subdomains among them, along with example.com and cloudflare.com, so as
// the ingress fallback it could not resolve the platform's names. URnetwork
// has no exit in Turkmenistan, and no plain resolver was measurable from
// inside: in OONI from its four networks (Apr 2025-Mar 2026) DoH and DoT to
// Cloudflare, Quad9 and Google failed in all but 18 of 2,527 measurements,
// and the system resolvers answered blocked names with 127.0.0.1.
// Turkmenistan stays an undisclosed connection region until a resolver is
// verified from there.
//
// ru's National DNS (NSDI) entries are its public recursive resolvers,
// a.res-nsdi.ru and b.res-nsdi.ru. Until 2026-11-11 the list named
// 195.208.6.1, which is a.auth-nsdi.ru, an authoritative server of NSDI's
// root zone: it answers every query with a referral and offers no recursion,
// and Go's resolver, which the remote plain dns path uses, reads a referral
// with glue as no such host, so a lookup that reached it failed as an
// authoritative miss. Their published v6 addresses (2a0c:a9c7:8::1 and
// 2a0c:a9c7:9::1) are left out until they are checked over v6.
var regionalDnsServers = []*RegionalDnsServer{
	{CountryCode: "cn", Name: "Alidns", Ipv4: "223.5.5.5", Ipv6: "2400:3200::1"},
	{CountryCode: "cn", Name: "DNSPod/Tencent", Ipv4: "119.29.29.29", Ipv6: "2402:4e00::"},
	{CountryCode: "ru", Name: "Yandex.DNS", Ipv4: "77.88.8.8", Ipv6: "2a02:6b8::feed:0ff"},
	{CountryCode: "ru", Name: "National DNS (NSDI)", Ipv4: "195.208.4.1"},
	{CountryCode: "ru", Name: "National DNS (NSDI)", Ipv4: "195.208.5.1"},
	{CountryCode: "ir", Name: "Shecan", Ipv4: "178.22.122.100"},
	{CountryCode: "ir", Name: "Electro", Ipv4: "78.157.42.100"},
	{CountryCode: "kz", Name: "Cloudflare", Ipv4: "1.1.1.1", Ipv6: "2606:4700:4700::1111"},
	{CountryCode: "kz", Name: "Quad9", Ipv4: "9.9.9.10", Ipv6: "2620:fe::10"},
}

// RegionalDnsServers enumerates the well known regional dns servers
func RegionalDnsServers() []*RegionalDnsServer {
	return regionalDnsServers
}

// a well known regional DoH server a user can take as a bootstrap DoH server
// for the control names (net_http_doh_control.go), associated to a country
// code. DohUrlIpv6 is empty where the operator serves no v6 DoH endpoint whose
// certificate names its v6 address.
type RegionalControlDohServer struct {
	CountryCode string
	Name        string
	DohUrlIpv4  string
	DohUrlIpv6  string
}

// the presets of the bootstrap DoH setting. Each was checked to answer RFC
// 8484 wire format (an application/dns-message GET and POST) with a
// certificate whose SAN carries the url's ip, which is what the DoH client
// verifies (2026-10-04). Alidns's v6 endpoints present a certificate without
// their v6 addresses and DNSPod serves no v6 DoH, so the cn presets are v4.
var regionalControlDohServers = []*RegionalControlDohServer{
	{CountryCode: "cn", Name: "Alidns", DohUrlIpv4: "https://223.5.5.5/dns-query"},
	{CountryCode: "cn", Name: "Alidns", DohUrlIpv4: "https://223.6.6.6/dns-query"},
	{CountryCode: "cn", Name: "DNSPod/Tencent", DohUrlIpv4: "https://1.12.12.12/dns-query"},
	{CountryCode: "cn", Name: "DNSPod/Tencent", DohUrlIpv4: "https://120.53.53.53/dns-query"},
}

// The bootstrap DoH server urls recommended for a country, v4 and v6, or none
// when there is no recommendation. This is the single source of the presets the
// apps offer (through the sdk).
func RegionalControlDohUrls(countryCode string) (dohUrlsIpv4 []string, dohUrlsIpv6 []string) {
	countryCode = strings.ToLower(strings.TrimSpace(countryCode))
	for _, server := range regionalControlDohServers {
		if server.CountryCode != countryCode {
			continue
		}
		if server.DohUrlIpv4 != "" {
			dohUrlsIpv4 = append(dohUrlsIpv4, server.DohUrlIpv4)
		}
		if server.DohUrlIpv6 != "" {
			dohUrlsIpv6 = append(dohUrlsIpv6, server.DohUrlIpv6)
		}
	}
	return
}

// RegionalDnsResolverSettings is the recommended dns resolver settings for a
// region where the strong-privacy defaults (DoH / cert-pinned) are known not to
// work: unencrypted remote dns only, using the region's known-working servers
// in both families. nil when there is no regional recommendation and the
// universal resolver should be used. this is the single source of truth for
// the recommendation, used by both the apps (via the sdk) and the server proxy
// config
func RegionalDnsResolverSettings(countryCode string) *DnsResolverSettings {
	remoteDnsIpv4 := RegionalDnsServerIps(countryCode)
	if len(remoteDnsIpv4) == 0 {
		return nil
	}
	return &DnsResolverSettings{
		EnableRemoteDns:       true,
		DnsUpgradeMaskAddress: DefaultDnsUpgradeMaskAddress,
		RemoteDnsIpv4:         remoteDnsIpv4,
		RemoteDnsIpv6:         RegionalDnsServerIpv6s(countryCode),
	}
}

// RegionalDnsServerIps returns the recommended dns server ipv4 addresses for a
// country, or nil when there is no recommendation
func RegionalDnsServerIps(countryCode string) []string {
	countryCode = strings.ToLower(countryCode)
	var ips []string
	for _, server := range regionalDnsServers {
		if server.CountryCode == countryCode {
			ips = append(ips, server.Ipv4)
		}
	}
	return ips
}

// RegionalDnsServerIpv6s returns the recommended dns server ipv6 addresses for
// a country, or nil when none of its servers publishes one. A region can have
// a v4 recommendation and no v6 one; the resolver then walks the v4 list.
func RegionalDnsServerIpv6s(countryCode string) []string {
	countryCode = strings.ToLower(countryCode)
	var ips []string
	for _, server := range regionalDnsServers {
		if server.CountryCode == countryCode && server.Ipv6 != "" {
			ips = append(ips, server.Ipv6)
		}
	}
	return ips
}

// Ingress dns is the resolution of the names the client dials to reach the
// platform itself (api, connect, extender: the control and bootstrap names,
// `ControlDohSettings` and the strategy's internal DoH resolver). Egress dns is
// the resolution of user traffic through the tunnel (`DefaultDohSettings` of
// the mux), which never reads the ingress country.
//
// The ingress country is the country the user declared they connect from
// (`DohSettings.IngressCountryCode`), lower case, or "" when undisclosed. It
// lives only in the ingress (control-name) settings, is local only and is never
// sent anywhere.

// The ingress country in its stored form: lower case and trimmed.
func NormalizeIngressCountryCode(countryCode string) string {
	return strings.ToLower(strings.TrimSpace(countryCode))
}

// The ingress dns strategy of an ingress country: the DoH presets queried ahead
// of the default DoH servers (none: the defaults only), and the plain resolvers
// of the last-resort bootstrap fallback (none: the local plain resolvers).
type IngressDnsStrategy struct {
	CountryCode  string
	DohUrlsIpv4  []string
	DohUrlsIpv6  []string
	PlainDnsIpv4 []string
	PlainDnsIpv6 []string
}

// The ingress dns strategy of a country from the per-country recommendations
// (`RegionalControlDohUrls`, `RegionalDnsServerIps`). Undisclosed ("") and a
// country without recommendations take the defaults.
func IngressDnsStrategyForCountry(countryCode string) *IngressDnsStrategy {
	countryCode = NormalizeIngressCountryCode(countryCode)
	dohUrlsIpv4, dohUrlsIpv6 := RegionalControlDohUrls(countryCode)
	return &IngressDnsStrategy{
		CountryCode:  countryCode,
		DohUrlsIpv4:  dohUrlsIpv4,
		DohUrlsIpv6:  dohUrlsIpv6,
		PlainDnsIpv4: RegionalDnsServerIps(countryCode),
		PlainDnsIpv6: RegionalDnsServerIpv6s(countryCode),
	}
}

// The ingress countries a user can pick: those with a per-country
// recommendation (DoH presets or plain resolvers), sorted. Any other country
// behaves as undisclosed, so it is not offered.
func IngressCountryCodes() []string {
	seen := map[string]bool{}
	for _, server := range regionalControlDohServers {
		seen[server.CountryCode] = true
	}
	for _, server := range regionalDnsServers {
		seen[server.CountryCode] = true
	}
	countryCodes := make([]string, 0, len(seen))
	for countryCode := range seen {
		countryCodes = append(countryCodes, countryCode)
	}
	slices.Sort(countryCodes)
	return countryCodes
}
