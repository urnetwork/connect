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

func TestParseControlDohUrl(t *testing.T) {
	valid := []struct {
		dohUrl    string
		expected  string
		ipVersion int
	}{
		{dohUrl: "https://223.5.5.5/dns-query", expected: "https://223.5.5.5/dns-query", ipVersion: 4},
		{dohUrl: "  https://1.12.12.12/dns-query\n", expected: "https://1.12.12.12/dns-query", ipVersion: 4},
		{dohUrl: "HTTPS://223.6.6.6/dns-query", expected: "https://223.6.6.6/dns-query", ipVersion: 4},
		{dohUrl: "https://120.53.53.53:8443/resolve", expected: "https://120.53.53.53:8443/resolve", ipVersion: 4},
		{dohUrl: "https://[2400:3200::1]/dns-query", expected: "https://[2400:3200::1]/dns-query", ipVersion: 6},
		// the ip in its canonical form, so a list never holds one server twice
		{dohUrl: "https://[2400:3200:0:0::ABCD]:443/dns-query", expected: "https://[2400:3200::abcd]:443/dns-query", ipVersion: 6},
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
		{dohUrl: "https://dns.alidns.com/dns-query", code: ControlDohErrorIpRequired},
		{dohUrl: "https://doh.pub/dns-query", code: ControlDohErrorIpRequired},
		{dohUrl: "http://223.5.5.5/dns-query", code: ControlDohErrorHttpsRequired},
		{dohUrl: "223.5.5.5/dns-query", code: ControlDohErrorHttpsRequired},
		{dohUrl: "tls://223.5.5.5", code: ControlDohErrorHttpsRequired},
		{dohUrl: "https://223.5.5.5", code: ControlDohErrorUrlInvalid},
		{dohUrl: "https://223.5.5.5/", code: ControlDohErrorUrlInvalid},
		{dohUrl: "https://223.5.5.5/dns-query?dns=x", code: ControlDohErrorUrlInvalid},
		{dohUrl: "https://223.5.5.5/dns-query?", code: ControlDohErrorUrlInvalid},
		{dohUrl: "https://223.5.5.5/dns-query#top", code: ControlDohErrorUrlInvalid},
		{dohUrl: "https://user:secret@223.5.5.5/dns-query", code: ControlDohErrorUrlInvalid},
		{dohUrl: "https://223.5.5.5:0/dns-query", code: ControlDohErrorUrlInvalid},
		{dohUrl: "https://223.5.5.5:70000/dns-query", code: ControlDohErrorUrlInvalid},
		{dohUrl: "https://[fe80::1%25en0]/dns-query", code: ControlDohErrorUrlInvalid},
		{dohUrl: "https://2400:3200::1/dns-query", code: ControlDohErrorUrlInvalid},
		{dohUrl: "https:223.5.5.5/dns-query", code: ControlDohErrorUrlInvalid},
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

	settings := ControlDohSettings(
		[]string{"https://223.5.5.5/dns-query", "https://1.1.1.1/dns-query"},
		[]string{"https://[2400:3200::1]/dns-query"},
	)
	expectedIpv4 := []string{"https://223.5.5.5/dns-query", "https://1.1.1.1/dns-query"}
	for _, dohUrl := range defaults.RemoteDohUrlsIpv4 {
		if dohUrl != "https://1.1.1.1/dns-query" {
			expectedIpv4 = append(expectedIpv4, dohUrl)
		}
	}
	if !slices.Equal(settings.DnsResolverSettings.RemoteDohUrlsIpv4, expectedIpv4) {
		t.Fatalf("v4 servers = %v, expected %v", settings.DnsResolverSettings.RemoteDohUrlsIpv4, expectedIpv4)
	}
	expectedIpv6 := append([]string{"https://[2400:3200::1]/dns-query"}, defaults.RemoteDohUrlsIpv6...)
	if !slices.Equal(settings.DnsResolverSettings.RemoteDohUrlsIpv6, expectedIpv6) {
		t.Fatalf("v6 servers = %v, expected %v", settings.DnsResolverSettings.RemoteDohUrlsIpv6, expectedIpv6)
	}
	for _, dohUrl := range []string{"https://223.5.5.5/dns-query", "https://1.1.1.1/dns-query", "https://[2400:3200::1]/dns-query"} {
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
		if ordered[0] == "https://223.5.5.5/dns-query" || ordered[0] == "https://1.1.1.1/dns-query" {
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

// The cn presets are the checked v4 servers, read by the same rule a user's
// url is, and a country without a recommendation has none.
func TestRegionalControlDohUrls(t *testing.T) {
	expected := []string{
		"https://223.5.5.5/dns-query",
		"https://223.6.6.6/dns-query",
		"https://1.12.12.12/dns-query",
		"https://120.53.53.53/dns-query",
	}
	for _, countryCode := range []string{"cn", "CN", " Cn "} {
		dohUrlsIpv4, dohUrlsIpv6 := RegionalControlDohUrls(countryCode)
		if !slices.Equal(dohUrlsIpv4, expected) {
			t.Fatalf("%q v4 = %v, expected %v", countryCode, dohUrlsIpv4, expected)
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
