package connect

import (
	"strings"
)

// regional dns resolver settings tune the dns resolver for regions
// if the result is nil, there is no regional specific setting and a universal resolver should be used
func RegionalDnsResolverSettings(countryCode string) *DnsResolverSettings {
	switch strings.ToLower(countryCode) {
	case "cn":
		return &DnsResolverSettings{
			EnableRemoteDns: true,
			EnableLocalDns:  true,
			RemoteDnsIpv4: []string{
				// Alidns
				"223.5.5.5",
				// DNSPod/Tencent
				"119.29.29.29",
			},
			LocalDnsIpv4: []string{
				"1.1.1.1",
			},
		}

	case "ru":
		return &DnsResolverSettings{
			EnableRemoteDns: true,
			EnableLocalDns:  true,
			RemoteDnsIpv4: []string{
				// Yandex.DNS
				"77.88.8.8",
				// National DNS (NSDI)
				"195.208.6.1",
			},
			LocalDnsIpv4: []string{
				"1.1.1.1",
			},
		}

	case "ir":
		return &DnsResolverSettings{
			EnableRemoteDns: true,
			EnableLocalDns:  true,
			RemoteDnsIpv4: []string{
				// Shecan
				"178.22.122.100",
				// 403.online
				"10.202.10.10",
			},
			LocalDnsIpv4: []string{
				"1.1.1.1",
			},
		}

	case "tr":
		return &DnsResolverSettings{
			EnableRemoteDns: true,
			EnableLocalDns:  true,
			RemoteDnsIpv4: []string{
				// TTNET
				"195.175.39.39",
				// Turkcell Superonline
				"212.252.114.8",
			},
			LocalDnsIpv4: []string{
				"1.1.1.1",
			},
		}

	case "kz":
		return &DnsResolverSettings{
			EnableRemoteDns: true,
			EnableLocalDns:  true,
			RemoteDnsIpv4: []string{
				// Google Public DNS
				"8.8.8.8",
			},
			LocalDnsIpv4: []string{
				"1.1.1.1",
			},
		}

	case "tm":
		return &DnsResolverSettings{
			EnableRemoteDns: true,
			EnableLocalDns:  true,
			RemoteDnsIpv4: []string{
				// Turkmentelecom
				"217.174.238.141",
				// Google Public DNS
				"8.8.4.4",
			},
			LocalDnsIpv4: []string{
				"1.1.1.1",
			},
		}

	default:
		return nil
	}
}
