package connect

// Resolver transport families must fit the owning tun without changing the
// caller's resolver choices or the independent host-side fallback path.

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Returns a real tun/cache with synthetic, caller-owned resolver endpoints.
func tunDohFamilyFixture(t *testing.T, mtu int, resolver *DnsResolverSettings, base *DohSettings) *Tun {
	t.Helper()
	settings := DefaultTunSettings()
	settings.Mtu = mtu
	settings.Log = NewNoopLogger()
	settings.DohSettings = base
	tun, err := CreateTunWithResolver(context.Background(), settings, resolver)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tun.Close() })
	return tun
}

// Authoritative misses force the complete configured fanout to finish, so the
// absence of an unsupported leg is proved without a sleep or first-winner race.
func TestTunDohQueryHonorsTunnelFamilies(t *testing.T) {
	for _, test := range []struct {
		mtu           int
		wantIpv6Dials int64
	}{
		{mtu: DefaultMtu, wantIpv6Dials: 0},
		{mtu: tunIpv6MinimumMtu - 1, wantIpv6Dials: 0},
		{mtu: tunIpv6MinimumMtu, wantIpv6Dials: 1},
		{mtu: DefaultTunnelMtu, wantIpv6Dials: 1},
	} {
		resolver := &DnsResolverSettings{
			EnableRemoteDoh:   true,
			RemoteDohUrlsIpv4: []string{"https://192.0.2.53/dns-query"},
			RemoteDohUrlsIpv6: []string{"https://[2001:db8::53]/dns-query"},
		}
		base := DefaultDohSettings()
		base.DohServerStagger = 0
		tun := tunDohFamilyFixture(t, test.mtu, resolver, base)
		cache := tun.DohCache()
		var ipv4Dials, ipv6Dials atomic.Int64
		cache.remoteClient.httpClient.Transport = dohRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
			switch request.URL.Hostname() {
			case "192.0.2.53":
				ipv4Dials.Add(1)
			case "2001:db8::53":
				ipv6Dials.Add(1)
			default:
				t.Error("resolver substituted an unconfigured target")
			}
			response := httptest.NewRecorder()
			writeDohWire(response, request, nil, 30, true)
			return response.Result(), nil
		})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		addrs, authoritative := cache.QueryResult(ctx, "A", "missing.example")
		cancel()
		if !authoritative || len(addrs) != 0 {
			t.Fatalf("mtu=%d: authoritative miss changed: addrs=%v authoritative=%v", test.mtu, addrs, authoritative)
		}
		if ipv4Dials.Load() != 1 || ipv6Dials.Load() != test.wantIpv6Dials {
			t.Errorf("mtu=%d: resolver legs v4=%d v6=%d, want v4=1 v6=%d", test.mtu, ipv4Dials.Load(), ipv6Dials.Load(), test.wantIpv6Dials)
		}
		_ = tun.Close()
	}
}

// Opaque record forwarding shares the same family bound. Failed synthetic
// responses drain every candidate and must remain non-authoritative.
func TestTunDohForwardHonorsTunnelFamilies(t *testing.T) {
	for _, test := range []struct {
		mtu           int
		wantIpv6Dials int64
	}{
		{mtu: DefaultMtu, wantIpv6Dials: 0},
		{mtu: DefaultTunnelMtu, wantIpv6Dials: 1},
	} {
		resolver := &DnsResolverSettings{
			EnableRemoteDoh:   true,
			RemoteDohUrlsIpv4: []string{"https://192.0.2.53/dns-query"},
			RemoteDohUrlsIpv6: []string{"https://[2001:db8::53]/dns-query"},
		}
		base := DefaultDohSettings()
		base.DohServerStagger = 0
		tun := tunDohFamilyFixture(t, test.mtu, resolver, base)
		cache := tun.DohCache()
		var ipv4Dials, ipv6Dials atomic.Int64
		cache.remoteClient.httpClient.Transport = dohRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Hostname() == "192.0.2.53" {
				ipv4Dials.Add(1)
			} else if request.URL.Hostname() == "2001:db8::53" {
				ipv6Dials.Add(1)
			} else {
				t.Error("resolver substituted an unconfigured target")
			}
			response := httptest.NewRecorder()
			response.WriteHeader(http.StatusServiceUnavailable)
			return response.Result(), nil
		})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		data, ok := cache.Forward(ctx, dnsmessage.Type(65), "forward.example")
		cancel()
		if ok || len(data) != 0 {
			t.Fatalf("mtu=%d: failed upstream became a usable answer", test.mtu)
		}
		if ipv4Dials.Load() != 1 || ipv6Dials.Load() != test.wantIpv6Dials {
			t.Errorf("mtu=%d: forwarding legs v4=%d v6=%d, want v4=1 v6=%d", test.mtu, ipv4Dials.Load(), ipv6Dials.Load(), test.wantIpv6Dials)
		}
		_ = tun.Close()
	}
}

// A resolver reload must remove only impossible remote legs, preserving the
// explicit family preference, resolver policy, order, and caller-owned input.
func TestTunDohFamilyConstraintPreservesCustomSettingsAndReload(t *testing.T) {
	base := DefaultDohSettings()
	base.IpVersion = 6
	base.DnsResolverSettings = &DnsResolverSettings{
		EnableRemoteDoh:   true,
		RemoteDohUrlsIpv4: []string{"https://192.0.2.54/dns-query", "https://192.0.2.53/dns-query"},
		RemoteDohUrlsIpv6: []string{"https://[2001:db8::53]/dns-query"},
		RemoteDnsIpv4:     []string{"192.0.2.53"},
		RemoteDnsIpv6:     []string{"2001:db8::53"},
		LocalDohUrlsIpv4:  []string{"https://192.0.2.55/dns-query"},
		LocalDohUrlsIpv6:  []string{"https://[2001:db8::55]/dns-query"},
		LocalDnsIpv4:      []string{"192.0.2.55"},
		LocalDnsIpv6:      []string{"2001:db8::55"},
		TlsConfig:         &tls.Config{ServerName: "resolver.example", MinVersion: tls.VersionTLS13},
	}
	original := *base.DnsResolverSettings
	tun := tunDohFamilyFixture(t, DefaultMtu, nil, base)
	for generation := range 2 {
		if generation == 1 {
			tun.SetDnsResolverSettings(base.DnsResolverSettings, time.Minute)
		}
		settings := tun.DohCache().settings
		if settings.IpVersion != 6 || base.IpVersion != 6 {
			t.Fatal("tunnel capability overwrote an explicit resolver family preference")
		}
		want := original
		want.RemoteDohUrlsIpv6 = nil
		want.RemoteDnsIpv6 = nil
		want.RemoteDnsIpv4 = nil
		if !reflect.DeepEqual(*settings.DnsResolverSettings, want) {
			t.Fatalf("generation=%d: resolver choices changed beyond the remote family constraint", generation)
		}
		if !reflect.DeepEqual(*base.DnsResolverSettings, original) {
			t.Fatal("tunnel mutated caller-owned resolver settings")
		}
		var remoteDials atomic.Int64
		tun.DohCache().remoteClient.httpClient.Transport = dohRoundTripperFunc(func(*http.Request) (*http.Response, error) {
			remoteDials.Add(1)
			return httptest.NewRecorder().Result(), nil
		})
		for _, recordType := range []string{"A", "AAAA"} {
			addrs, authoritative := tun.DohCache().QueryResult(context.Background(), recordType, "unsupported.example")
			if authoritative || len(addrs) != 0 || remoteDials.Load() != 0 {
				t.Fatalf("generation=%d record=%s: unsupported explicit IPv6 choice gained a DoH fallback", generation, recordType)
			}
		}
		if data, ok := tun.DohCache().Forward(context.Background(), dnsmessage.Type(65), "unsupported.example"); ok || len(data) != 0 || remoteDials.Load() != 0 {
			t.Fatal("raw forwarding substituted IPv4 for an unsupported explicit IPv6 remote choice")
		}
		if tun.DohCache().Warm(context.Background(), 2) || remoteDials.Load() != 0 {
			t.Fatal("warmup substituted IPv4 for an unsupported explicit IPv6 remote choice")
		}
		settings.DialContextSettings = &DialContextSettings{DialContext: func(context.Context, string, string) (net.Conn, error) {
			remoteDials.Add(1)
			return nil, errors.New("synthetic dial must not run")
		}}
		conn, err := tun.DohCache().remoteResolver.Dial(context.Background(), "udp", "192.0.2.1:53")
		if conn != nil || err == nil || remoteDials.Load() != 0 {
			t.Fatal("plain DNS substituted IPv4 for an unsupported explicit IPv6 remote choice")
		}
	}
}

// A configured IPv4 resolver still supplies real answers after impossible
// sibling endpoints are removed; no default target or fallback is added.
func TestTunDohIpv4ConstraintResolvesCustomAnswer(t *testing.T) {
	resolver := &DnsResolverSettings{
		EnableRemoteDoh:   true,
		RemoteDohUrlsIpv4: []string{"https://192.0.2.53/dns-query"},
		RemoteDohUrlsIpv6: []string{"https://[2001:db8::53]/dns-query"},
	}
	tun := tunDohFamilyFixture(t, DefaultMtu, resolver, nil)
	cache := tun.DohCache()
	answer := netip.MustParseAddr("192.0.2.80")
	cache.remoteClient.httpClient.Transport = dohRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Hostname() != "192.0.2.53" {
			t.Error("query used an unsupported or unconfigured resolver")
		}
		response := httptest.NewRecorder()
		writeDohWire(response, request, []netip.Addr{answer}, 30, false)
		return response.Result(), nil
	})
	addrs, authoritative := cache.QueryResult(context.Background(), "A", "answer.example")
	if !authoritative || !reflect.DeepEqual(addrs, []netip.Addr{answer}) {
		t.Fatalf("IPv4 answer changed: addrs=%v authoritative=%v", addrs, authoritative)
	}
}

// A host-side IPv6 resolver can return an AAAA answer even when its owning
// tunnel is IPv4-only; the constraint must not become a cache-wide family pin.
func TestTunDohIpv4ConstraintPreservesLocalIpv6Answers(t *testing.T) {
	resolver := &DnsResolverSettings{
		EnableLocalDoh:   true,
		LocalDohUrlsIpv6: []string{"https://[2001:db8::55]/dns-query"},
	}
	tun := tunDohFamilyFixture(t, DefaultMtu, resolver, nil)
	cache := tun.DohCache()
	var localDials atomic.Int64
	answer := netip.MustParseAddr("2001:db8::80")
	cache.localClient.httpClient.Transport = dohRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
		localDials.Add(1)
		if request.URL.Hostname() != "2001:db8::55" {
			t.Error("host-side resolver target changed")
		}
		response := httptest.NewRecorder()
		writeDohWire(response, request, []netip.Addr{answer}, 30, false)
		return response.Result(), nil
	})
	addrs, authoritative := cache.QueryResult(context.Background(), "AAAA", "answer.example")
	if !authoritative || !reflect.DeepEqual(addrs, []netip.Addr{answer}) || localDials.Load() != 1 {
		t.Fatalf("host resolver was narrowed by tunnel capability: addrs=%v authoritative=%v calls=%d", addrs, authoritative, localDials.Load())
	}
}
