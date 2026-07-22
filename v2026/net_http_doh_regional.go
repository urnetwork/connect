package connect

import (
	"strings"
)

// a well known regional dns server, associated to a country code
type RegionalDnsServer struct {
	CountryCode string
	Name        string
	Ipv4        string
}

// the well known regional dns servers, associated to country codes.
// these are suggestions that clients can surface when connected to the region
var regionalDnsServers = []*RegionalDnsServer{
	{CountryCode: "cn", Name: "Alidns", Ipv4: "223.5.5.5"},
	{CountryCode: "cn", Name: "DNSPod/Tencent", Ipv4: "119.29.29.29"},
	{CountryCode: "ru", Name: "Yandex.DNS", Ipv4: "77.88.8.8"},
	{CountryCode: "ru", Name: "National DNS (NSDI)", Ipv4: "195.208.6.1"},
	{CountryCode: "ir", Name: "Shecan", Ipv4: "178.22.122.100"},
	{CountryCode: "ir", Name: "403.online", Ipv4: "10.202.10.10"},
	{CountryCode: "tr", Name: "TTNET", Ipv4: "195.175.39.39"},
	{CountryCode: "tr", Name: "Turkcell Superonline", Ipv4: "212.252.114.8"},
	{CountryCode: "kz", Name: "Google Public DNS", Ipv4: "8.8.8.8"},
	{CountryCode: "tm", Name: "Turkmentelecom", Ipv4: "217.174.238.141"},
	{CountryCode: "tm", Name: "Google Public DNS", Ipv4: "8.8.4.4"},
}

// RegionalDnsServers enumerates the well known regional dns servers
func RegionalDnsServers() []*RegionalDnsServer {
	return regionalDnsServers
}

// RegionalDnsResolverSettings is the recommended dns resolver settings for a
// region where the strong-privacy defaults (DoH / cert-pinned) are known not to
// work: unencrypted remote dns only, using the region's known-working servers.
// nil when there is no regional recommendation and the universal resolver should
// be used. this is the single source of truth for the recommendation, used by
// both the apps (via the sdk) and the server proxy config
func RegionalDnsResolverSettings(countryCode string) *DnsResolverSettings {
	remoteDnsIpv4 := RegionalDnsServerIps(countryCode)
	if len(remoteDnsIpv4) == 0 {
		return nil
	}
	return &DnsResolverSettings{
		EnableRemoteDns: true,
		RemoteDnsIpv4:   remoteDnsIpv4,
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
