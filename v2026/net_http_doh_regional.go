package connect

import (
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
// these are suggestions that clients can surface when connected to the region
var regionalDnsServers = []*RegionalDnsServer{
	{CountryCode: "cn", Name: "Alidns", Ipv4: "223.5.5.5", Ipv6: "2400:3200::1"},
	{CountryCode: "cn", Name: "DNSPod/Tencent", Ipv4: "119.29.29.29", Ipv6: "2402:4e00::"},
	{CountryCode: "ru", Name: "Yandex.DNS", Ipv4: "77.88.8.8", Ipv6: "2a02:6b8::feed:0ff"},
	{CountryCode: "ru", Name: "National DNS (NSDI)", Ipv4: "195.208.6.1"},
	{CountryCode: "ir", Name: "Shecan", Ipv4: "178.22.122.100"},
	{CountryCode: "ir", Name: "403.online", Ipv4: "10.202.10.10"},
	{CountryCode: "tr", Name: "TTNET", Ipv4: "195.175.39.39"},
	{CountryCode: "tr", Name: "Turkcell Superonline", Ipv4: "212.252.114.8"},
	{CountryCode: "kz", Name: "Google Public DNS", Ipv4: "8.8.8.8", Ipv6: "2001:4860:4860::8888"},
	{CountryCode: "tm", Name: "Turkmentelecom", Ipv4: "217.174.238.141"},
	{CountryCode: "tm", Name: "Google Public DNS", Ipv4: "8.8.4.4", Ipv6: "2001:4860:4860::8844"},
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
