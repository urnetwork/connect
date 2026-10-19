package connect

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"time"
)

// The bootstrap plain dns fallback of the control-name lookups (support inbox
// 7956, 7957, 7973): when every encrypted resolver fails, the space's own
// names (and only those) resolve over plain dns.
//
// On a censored network a poisoned plain answer gains nothing: the dial
// that follows still verifies the platform's TLS certificate against the
// original hostname, so a forged address fails the handshake. The fallback
// mainly helps networks whose dns is broken (DoH blocked by a captive or
// misconfigured network) rather than censored.

// A plain dns answer of the bootstrap fallback is held at most this long, so
// DoH is asked again soon.
const bootstrapPlainDnsExpiration = 60 * time.Second

// Whether the name is one of `DohSettings.BootstrapPlainDnsDomains` or under
// one.
func bootstrapPlainDnsName(settings *DohSettings, name string) bool {
	name = normalizeInternalDohName(name)
	if name == "" {
		return false
	}
	for _, domain := range settings.BootstrapPlainDnsDomains {
		domain = normalizeInternalDohName(domain)
		if domain == "" {
			continue
		}
		if name == domain || strings.HasSuffix(name, "."+domain) {
			return true
		}
	}
	return false
}

// The plain dns servers of the bootstrap fallback, read at each lookup: the
// plain resolvers of the ingress country when it has them, else the local
// (host) plain resolvers of the settings, else of the defaults.
func bootstrapPlainDnsServers(settings *DohSettings, network string) []string {
	ingressDnsStrategy := IngressDnsStrategyForCountry(settings.IngressCountryCode)
	if 0 < len(ingressDnsStrategy.PlainDnsIpv4) || 0 < len(ingressDnsStrategy.PlainDnsIpv6) {
		regionalSettings := *settings
		regionalSettings.DnsResolverSettings = &DnsResolverSettings{
			RemoteDnsIpv4: ingressDnsStrategy.PlainDnsIpv4,
			RemoteDnsIpv6: ingressDnsStrategy.PlainDnsIpv6,
		}
		return dnsResolverAddrs(&regionalSettings, true, network)
	}
	localSettings := *settings
	if settings.DnsResolverSettings == nil ||
		(len(settings.DnsResolverSettings.LocalDnsIpv4) == 0 && len(settings.DnsResolverSettings.LocalDnsIpv6) == 0) {
		localSettings.DnsResolverSettings = DefaultDnsResolverSettings()
	}
	return dnsResolverAddrs(&localSettings, false, network)
}

// The plain dns lookup of the bootstrap fallback, dialed from the host.
func newBootstrapPlainDnsLookup(
	settings *DohSettings,
	lifecycle *dohCacheLifecycle,
	netDialer *net.Dialer,
) func(ctx context.Context, network string, host string) ([]net.IP, error) {
	var next atomic.Uint64
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: lifecycle.dialContext(func(ctx context.Context, network string, addr string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			servers := bootstrapPlainDnsServers(settings, network)
			if len(servers) == 0 {
				return nil, fmt.Errorf("no bootstrap DNS resolvers")
			}
			server := servers[int((next.Add(1)-1)%uint64(len(servers)))]
			addr = net.JoinHostPort(server, port)
			conn, err := netDialer.DialContext(ctx, network, addr)
			logControlDialResult(settings.Log, "dns", true, network, addr, conn, err)
			return conn, err
		}),
	}
	return resolver.LookupIP
}
