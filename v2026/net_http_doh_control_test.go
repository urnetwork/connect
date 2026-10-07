package connect

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// Bootstrap DoH server tests (net_http_doh_control.go): the url rule, the
// order the named servers take against the defaults, and the presets.

// A DoH server for the bootstrap DoH tests: https on the v4 loopback, which
// its certificate names as an ip, so its url is one the control DoH rule
// accepts. Returns the url, a tls config that trusts the certificate, and the
// address the DoH client dials.
func newControlDohTestServer(t *testing.T, handler http.Handler) (string, *tls.Config, string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/dns-query", handler)
	server := newFamilyHttptestTlsServer(t, 4, mux)
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	dohUrl, addr, err := ParseControlDohUrl(server.URL + "/dns-query")
	if err != nil {
		t.Fatal(err)
	}
	if !addr.Is4() {
		t.Fatalf("test server ip = %s, expected the v4 loopback", addr)
	}
	return dohUrl, &tls.Config{RootCAs: roots}, server.Listener.Addr().String()
}

// The dial of a network that black-holes every DoH server but the ones it
// lets through: a dial to any other address hangs until its context ends,
// which is what a censor that drops the default servers' packets does.
type controlDohTestDialer struct {
	allowedAddresses []string
	blackholedCount  atomic.Int32
}

// The dial seam of a strategy or a DoH client on that network.
func (self *controlDohTestDialer) dialContextSettings() *DialContextSettings {
	return &DialContextSettings{
		DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			if slices.Contains(self.allowedAddresses, address) {
				return (&net.Dialer{}).DialContext(ctx, network, address)
			}
			self.blackholedCount.Add(1)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
}

// The DoH settings a test strategy starts from: the defaults alone, every one
// of them black-holed.
func controlDohTestDefaultSettings(dialer *controlDohTestDialer) *DohSettings {
	settings := DefaultDohSettings()
	settings.RequestTimeout = time.Second
	settings.DialContextSettings = dialer.dialContextSettings()
	return settings
}

// The control DoH settings of one named test server, ahead of the black-holed
// defaults.
func controlDohTestSettings(dohUrl string, tlsConfig *tls.Config, dialer *controlDohTestDialer) *DohSettings {
	settings := ControlDohSettings([]string{dohUrl}, nil)
	settings.RequestTimeout = 2 * time.Second
	settings.DnsResolverSettings.TlsConfig = tlsConfig
	settings.DialContextSettings = dialer.dialContextSettings()
	return settings
}

// The url rule: https to an ip literal with a path, read into its canonical
// form, and one error code for each thing a user fixes. Every address here is
// from the documentation ranges.
func TestParseControlDohUrl(t *testing.T) {
	valid := []struct {
		dohUrl    string
		expected  string
		ipVersion int
	}{
		{dohUrl: "https://192.0.2.53/dns-query", expected: "https://192.0.2.53/dns-query", ipVersion: 4},
		{dohUrl: "  https://198.51.100.53/dns-query\n", expected: "https://198.51.100.53/dns-query", ipVersion: 4},
		{dohUrl: "HTTPS://203.0.113.53/dns-query", expected: "https://203.0.113.53/dns-query", ipVersion: 4},
		{dohUrl: "https://192.0.2.54:8443/resolve", expected: "https://192.0.2.54:8443/resolve", ipVersion: 4},
		{dohUrl: "https://[2001:db8::53]/dns-query", expected: "https://[2001:db8::53]/dns-query", ipVersion: 6},
		// the ip in its canonical form, so a list never holds one server twice
		{dohUrl: "https://[2001:db8:0:0::ABCD]:443/dns-query", expected: "https://[2001:db8::abcd]:443/dns-query", ipVersion: 6},
	}
	for _, c := range valid {
		dohUrl, addr, err := ParseControlDohUrl(c.dohUrl)
		if err != nil {
			t.Errorf("%q: %v", c.dohUrl, err)
			continue
		}
		if dohUrl != c.expected {
			t.Errorf("%q read as %q, expected %q", c.dohUrl, dohUrl, c.expected)
		}
		if (c.ipVersion == 4) != addr.Is4() {
			t.Errorf("%q is %s, expected ipv%d", c.dohUrl, addr, c.ipVersion)
		}
	}

	invalid := []struct {
		dohUrl string
		code   string
	}{
		{dohUrl: "", code: ControlDohErrorUrlInvalid},
		{dohUrl: "   ", code: ControlDohErrorUrlInvalid},
		// a host name would need a plaintext lookup of its own
		{dohUrl: "https://dns.resolver.example/dns-query", code: ControlDohErrorIpRequired},
		{dohUrl: "https://doh.example/dns-query", code: ControlDohErrorIpRequired},
		{dohUrl: "http://192.0.2.53/dns-query", code: ControlDohErrorHttpsRequired},
		{dohUrl: "192.0.2.53/dns-query", code: ControlDohErrorHttpsRequired},
		{dohUrl: "tls://192.0.2.53", code: ControlDohErrorHttpsRequired},
		{dohUrl: "https://192.0.2.53", code: ControlDohErrorUrlInvalid},
		{dohUrl: "https://192.0.2.53/", code: ControlDohErrorUrlInvalid},
		{dohUrl: "https://192.0.2.53/dns-query?dns=x", code: ControlDohErrorUrlInvalid},
		{dohUrl: "https://192.0.2.53/dns-query?", code: ControlDohErrorUrlInvalid},
		{dohUrl: "https://192.0.2.53/dns-query#top", code: ControlDohErrorUrlInvalid},
		{dohUrl: "https://user:secret@192.0.2.53/dns-query", code: ControlDohErrorUrlInvalid},
		{dohUrl: "https://192.0.2.53:0/dns-query", code: ControlDohErrorUrlInvalid},
		{dohUrl: "https://192.0.2.53:70000/dns-query", code: ControlDohErrorUrlInvalid},
		{dohUrl: "https://[fe80::1%25en0]/dns-query", code: ControlDohErrorUrlInvalid},
		{dohUrl: "https://2001:db8::53/dns-query", code: ControlDohErrorUrlInvalid},
		{dohUrl: "https:192.0.2.53/dns-query", code: ControlDohErrorUrlInvalid},
	}
	for _, c := range invalid {
		dohUrl, _, err := ParseControlDohUrl(c.dohUrl)
		if err == nil {
			t.Errorf("%q read as %q, expected %s", c.dohUrl, dohUrl, c.code)
			continue
		}
		if code := ControlDohUrlErrorCode(err); code != c.code {
			t.Errorf("%q = %s (%v), expected %s", c.dohUrl, code, err, c.code)
		}
	}
	if code := ControlDohUrlErrorCode(nil); code != "" {
		t.Errorf("nil error code = %q", code)
	}
}

// The named servers go ahead of the default servers of their family, the
// defaults stay behind them once each, and the named servers are seeded so a
// query's weighted order starts with them. With none named the settings are
// the defaults.
func TestControlDohSettingsPutsTheNamedServersFirst(t *testing.T) {
	defaults := DefaultDnsResolverSettings()
	namedDohUrlIpv4 := "https://192.0.2.53/dns-query"
	namedDohUrlIpv6 := "https://[2001:db8::53]/dns-query"
	// a named server that is also a default stays in the list once
	defaultDohUrlIpv4 := defaults.RemoteDohUrlsIpv4[0]

	settings := ControlDohSettings(
		[]string{namedDohUrlIpv4, defaultDohUrlIpv4},
		[]string{namedDohUrlIpv6},
	)
	expectedIpv4 := []string{namedDohUrlIpv4, defaultDohUrlIpv4}
	for _, dohUrl := range defaults.RemoteDohUrlsIpv4 {
		if dohUrl != defaultDohUrlIpv4 {
			expectedIpv4 = append(expectedIpv4, dohUrl)
		}
	}
	if !slices.Equal(settings.DnsResolverSettings.RemoteDohUrlsIpv4, expectedIpv4) {
		t.Fatalf("v4 servers = %v, expected %v", settings.DnsResolverSettings.RemoteDohUrlsIpv4, expectedIpv4)
	}
	expectedIpv6 := append([]string{namedDohUrlIpv6}, defaults.RemoteDohUrlsIpv6...)
	if !slices.Equal(settings.DnsResolverSettings.RemoteDohUrlsIpv6, expectedIpv6) {
		t.Fatalf("v6 servers = %v, expected %v", settings.DnsResolverSettings.RemoteDohUrlsIpv6, expectedIpv6)
	}
	for _, dohUrl := range []string{namedDohUrlIpv4, defaultDohUrlIpv4, namedDohUrlIpv6} {
		if score := settings.ServerStatsSeed[dohUrl]; score != dohSeedMaxScore {
			t.Errorf("seed of %s = %v, expected %v", dohUrl, score, dohSeedMaxScore)
		}
	}
	if len(settings.ServerStatsSeed) != 3 {
		t.Errorf("seeds = %v, expected the named servers only", settings.ServerStatsSeed)
	}
	// the weighted order a fresh cache starts from puts a named server first,
	// all but about one time in a hundred (8.05 against 0.05 per default)
	stats := newServerStats()
	stats.seed(settings.ServerStatsSeed)
	namedFirstCount := 0
	for range 400 {
		ordered := stats.order(remoteDohUrls(settings, 4))
		if ordered[0] == namedDohUrlIpv4 || ordered[0] == defaultDohUrlIpv4 {
			namedFirstCount += 1
		}
	}
	if namedFirstCount < 380 {
		t.Fatalf("a named server was first in %d of 400 orders, expected nearly all", namedFirstCount)
	}
	// the defaults are untouched
	if !slices.Equal(DefaultDnsResolverSettings().RemoteDohUrlsIpv4, defaults.RemoteDohUrlsIpv4) {
		t.Fatal("building the settings changed the defaults")
	}

	none := ControlDohSettings(nil, nil)
	if !slices.Equal(none.DnsResolverSettings.RemoteDohUrlsIpv4, defaults.RemoteDohUrlsIpv4) ||
		!slices.Equal(none.DnsResolverSettings.RemoteDohUrlsIpv6, defaults.RemoteDohUrlsIpv6) {
		t.Fatalf("no named servers = %+v, expected the defaults", none.DnsResolverSettings)
	}
	if none.ServerStatsSeed != nil {
		t.Fatalf("no named servers seeded %v", none.ServerStatsSeed)
	}
}

// The cn presets are v4 servers, in table order for any case and spacing of the
// code, read by the same rule a user's url is, and a country without a
// recommendation has none. The expected lists come from the table, so no
// production address is written into the test.
func TestRegionalControlDohUrls(t *testing.T) {
	expectedIpv4 := []string{}
	for _, server := range regionalControlDohServers {
		if server.CountryCode != "cn" {
			continue
		}
		if server.DohUrlIpv6 != "" {
			t.Fatalf("cn preset %s has a v6 url, expected the v4 presets only", server.Name)
		}
		expectedIpv4 = append(expectedIpv4, server.DohUrlIpv4)
	}
	if len(expectedIpv4) == 0 {
		t.Fatal("no cn presets")
	}
	for _, countryCode := range []string{"cn", "CN", " Cn "} {
		dohUrlsIpv4, dohUrlsIpv6 := RegionalControlDohUrls(countryCode)
		if !slices.Equal(dohUrlsIpv4, expectedIpv4) {
			t.Fatalf("%q v4 = %v, expected %v", countryCode, dohUrlsIpv4, expectedIpv4)
		}
		if len(dohUrlsIpv6) != 0 {
			t.Fatalf("%q v6 = %v, expected none", countryCode, dohUrlsIpv6)
		}
	}
	for _, server := range regionalControlDohServers {
		for _, dohUrl := range []string{server.DohUrlIpv4, server.DohUrlIpv6} {
			if dohUrl == "" {
				continue
			}
			parsed, addr, err := ParseControlDohUrl(dohUrl)
			if err != nil {
				t.Fatalf("preset %s: %v", dohUrl, err)
			}
			if parsed != dohUrl {
				t.Fatalf("preset %s is not in its canonical form %s", dohUrl, parsed)
			}
			if (dohUrl == server.DohUrlIpv4) != addr.Is4() {
				t.Fatalf("preset %s is in the wrong family", dohUrl)
			}
		}
	}
	for _, countryCode := range []string{"us", "", "ru"} {
		if dohUrlsIpv4, dohUrlsIpv6 := RegionalControlDohUrls(countryCode); 0 < len(dohUrlsIpv4)+len(dohUrlsIpv6) {
			t.Fatalf("%q = %v %v, expected no preset", countryCode, dohUrlsIpv4, dohUrlsIpv6)
		}
	}
}
