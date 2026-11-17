package connect

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Bootstrap fallback tests (support inbox 7956, 7957, 7973): the China DoH
// presets after the default DoH servers fail, then plain dns for the space's
// own names only, after every encrypted resolver failed. The transports are
// fakes, so nothing leaves the process.

// A DoH transport that answers only the given hosts, and counts every request
// by host.
type bootstrapDohTransport struct {
	answer       netip.Addr
	answerHosts  map[string]bool
	nxdomain     bool
	stateLock    sync.Mutex
	requestHosts []string
}

func (self *bootstrapDohTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.requestHosts = append(self.requestHosts, request.URL.Hostname())
	}()
	if !self.answerHosts[request.URL.Hostname()] {
		return nil, errors.New("blocked")
	}
	recorder := httptest.NewRecorder()
	writeDohWire(recorder, request, []netip.Addr{self.answer}, 60, self.nxdomain)
	response := recorder.Result()
	response.Request = request
	return response, nil
}

func (self *bootstrapDohTransport) hosts() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.requestHosts)
}

// The hosts of the cn DoH presets.
func bootstrapCnPresetHosts(t *testing.T) map[string]bool {
	t.Helper()
	presetsIpv4, _ := RegionalControlDohUrls("cn")
	if len(presetsIpv4) == 0 {
		t.Fatal("no cn presets")
	}
	hosts := map[string]bool{}
	for _, presetUrl := range presetsIpv4 {
		_, addr, err := ParseControlDohUrl(presetUrl)
		if err != nil {
			t.Fatal(err)
		}
		hosts[addr.String()] = true
	}
	return hosts
}

// A fake plain dns lookup that records the names it was asked.
type bootstrapPlainDns struct {
	answer    net.IP
	stateLock sync.Mutex
	names     []string
}

func (self *bootstrapPlainDns) lookupIP(ctx context.Context, network string, host string) ([]net.IP, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.names = append(self.names, host)
	return []net.IP{self.answer}, nil
}

func (self *bootstrapPlainDns) queried() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.names)
}

// The control-name cache of a space whose domain is example.test, on the
// fake transports.
func newBootstrapTestCache(t *testing.T, ingressCountryCode string, transport http.RoundTripper, plainDns *bootstrapPlainDns) *DohCache {
	t.Helper()
	settings := IngressDohSettings(ingressCountryCode, nil, nil)
	settings.DohServerStagger = 0
	cache := NewDohCache(internalDohSettings(settings, []string{"example.test"}))
	t.Cleanup(cache.Close)
	cache.remoteClient.httpClient = &http.Client{Transport: transport}
	cache.bootstrapLookupIP = plainDns.lookupIP
	return cache
}

// The ingress country picks the resolvers from the per-country
// recommendations: cn the AliDNS/DNSPod DoH presets and the cn plain
// resolvers, undisclosed and a country without recommendations the defaults.
func TestIngressDnsStrategyForCountry(t *testing.T) {
	cn := IngressDnsStrategyForCountry(" CN ")
	presetsIpv4, _ := RegionalControlDohUrls("cn")
	if cn.CountryCode != "cn" || !slices.Equal(cn.DohUrlsIpv4, presetsIpv4) || len(cn.DohUrlsIpv4) == 0 {
		t.Fatalf("cn doh = %v, expected %v", cn.DohUrlsIpv4, presetsIpv4)
	}
	if !slices.Equal(cn.PlainDnsIpv4, []string{"223.5.5.5", "119.29.29.29"}) {
		t.Fatalf("cn plain = %v", cn.PlainDnsIpv4)
	}
	for _, countryCode := range []string{"", "us"} {
		strategy := IngressDnsStrategyForCountry(countryCode)
		if len(strategy.DohUrlsIpv4) != 0 || len(strategy.DohUrlsIpv6) != 0 || len(strategy.PlainDnsIpv4) != 0 || len(strategy.PlainDnsIpv6) != 0 {
			t.Fatalf("%q strategy = %+v, expected defaults", countryCode, strategy)
		}
	}

	// the ingress lookups follow the country
	defaults := DefaultDnsResolverSettings()
	if urls := remoteDohUrls(IngressDohSettings("", nil, nil), 4); !slices.Equal(urls, defaults.RemoteDohUrlsIpv4) {
		t.Fatalf("undisclosed v4 = %v, expected %v", urls, defaults.RemoteDohUrlsIpv4)
	}
	if urls := remoteDohUrls(IngressDohSettings("cn", nil, nil), 4); !slices.Equal(urls, slices.Concat(presetsIpv4, defaults.RemoteDohUrlsIpv4)) {
		t.Fatalf("cn v4 = %v", urls)
	}
}

// Ingress and egress are separate: the ingress country never changes the
// egress (tunnel) resolver, and the egress settings never change the ingress
// resolvers.
func TestIngressEgressDnsSeparate(t *testing.T) {
	defaults := DefaultDnsResolverSettings()
	// the egress settings carry no ingress country and no bootstrap names,
	// whatever ingress country is in force
	for _, countryCode := range []string{"", "cn", "ru"} {
		ingress := IngressDohSettings(countryCode, nil, nil)
		egress := DefaultDohSettings()
		if egress.IngressCountryCode != "" || egress.RegionalRemoteDoh || 0 < len(egress.BootstrapPlainDnsDomains) {
			t.Fatalf("%q egress settings carry ingress values", countryCode)
		}
		if urls := remoteDohUrls(egress, 4); !slices.Equal(urls, defaults.RemoteDohUrlsIpv4) {
			t.Fatalf("%q egress v4 = %v, expected %v", countryCode, urls, defaults.RemoteDohUrlsIpv4)
		}
		if ingress.DnsResolverSettings == egress.DnsResolverSettings {
			t.Fatalf("%q ingress and egress share resolver settings", countryCode)
		}
	}

	// an egress change (the user's tunnel dns settings) leaves ingress alone
	ingress := IngressDohSettings("cn", nil, nil)
	ingressBefore := remoteDohUrls(ingress, 4)
	egress := DefaultDohSettings()
	egress.DnsResolverSettings.RemoteDohUrlsIpv4 = []string{"https://192.0.2.53/dns-query"}
	egress.DnsResolverSettings.EnableRemoteDns = true
	if urls := remoteDohUrls(ingress, 4); !slices.Equal(urls, ingressBefore) {
		t.Fatalf("ingress = %v after an egress change, expected %v", urls, ingressBefore)
	}
	if urls := remoteDohUrls(IngressDohSettings("cn", nil, nil), 4); !slices.Equal(urls, ingressBefore) {
		t.Fatalf("new ingress = %v after an egress change, expected %v", urls, ingressBefore)
	}

	// a preset selected for the tunnel (ADBLOCK3RD 6.3) changes nothing on the
	// ingress side. The ingress servers of each country are read first; then a
	// preset is applied in place to the settings objects a device starts from
	// (the mux and egress defaults), a mux runs with it, and the ingress
	// settings and control-name resolver built afterwards must match what was
	// read, with no pins, so a filter can never affect the control names.
	ingressCountryCodes := []string{"", "cn", "ru"}
	ingressBeforePreset := map[string][][]string{}
	for _, countryCode := range ingressCountryCodes {
		ingress := IngressDohSettings(countryCode, nil, nil)
		for _, ipVersion := range []int{0, 4, 6} {
			ingressBeforePreset[countryCode] = append(ingressBeforePreset[countryCode], remoteDohUrls(ingress, ipVersion))
		}
	}
	const presetDohUrl = "https://dns.example/dns-query"
	presetPins := map[string][]string{"dns.example": {"192.0.2.95", "2001:db8::95"}}
	presetMux := DefaultUpgradeMuxSettings()
	presetMux.Dns.Resolver.RemoteDohUrlsIpv4 = []string{presetDohUrl}
	presetMux.Dns.Resolver.RemoteDohUrlsIpv6 = []string{presetDohUrl}
	presetMux.Dns.Resolver.RemoteDnsIpv4 = nil
	presetMux.Dns.Resolver.RemoteDnsIpv6 = nil
	presetMux.Dns.Resolver.DohServerHostAddrs = presetPins
	presetMux.Dns.Failover = DefaultUpgradeMuxSettings().Dns.Resolver
	presetMux.Dns.Fallback = DefaultDnsUpgradeFallbackSettings()
	presetMux.Dns.FallbackUntilTunnelReady = true
	presetEgress := DefaultDohSettings()
	presetEgress.DnsResolverSettings.RemoteDohUrlsIpv4 = []string{presetDohUrl}
	presetEgress.DnsResolverSettings.RemoteDohUrlsIpv6 = []string{presetDohUrl}
	presetEgress.DnsResolverSettings.DohServerHostAddrs = presetPins
	presetCtx, presetCancel := context.WithCancel(context.Background())
	defer presetCancel()
	presetUpgradeMux, err := NewUpgradeMux(presetCtx, TransferPath{}, protocol.ProvideMode_Network, 0, func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {}, presetMux, NewNoopLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer presetUpgradeMux.Close()
	presetCache := NewDohCache(presetEgress)
	defer presetCache.Close()
	if presetCache.pinnedHostAddrs == nil || presetUpgradeMux.mux.Tun().DohCache().pinnedHostAddrs == nil {
		t.Fatal("the preset settings did not pin their host")
	}
	for _, countryCode := range ingressCountryCodes {
		ingress := IngressDohSettings(countryCode, nil, nil)
		internal := internalDohSettings(ingress, []string{"api.example"})
		for i, ipVersion := range []int{0, 4, 6} {
			want := ingressBeforePreset[countryCode][i]
			if urls := remoteDohUrls(ingress, ipVersion); !slices.Equal(urls, want) {
				t.Fatalf("%q ingress v%d = %v with a preset running, expected %v", countryCode, ipVersion, urls, want)
			}
			if urls := remoteDohUrls(internal, ipVersion); !slices.Equal(urls, want) {
				t.Fatalf("%q control-name resolver v%d = %v with a preset running, expected %v", countryCode, ipVersion, urls, want)
			}
		}
		if ingress.DnsResolverSettings.DohServerHostAddrs != nil || internal.DnsResolverSettings.DohServerHostAddrs != nil {
			t.Fatalf("%q ingress carries the preset's pins", countryCode)
		}
		if ingress.DnsResolverSettings == presetMux.Dns.Resolver || ingress.DnsResolverSettings == presetEgress.DnsResolverSettings {
			t.Fatalf("%q ingress shares resolver settings with the preset", countryCode)
		}
		internalCache := NewDohCache(internal)
		if internalCache.pinnedHostAddrs != nil || internalCache.dohServerNames["dns.example"] {
			t.Fatalf("%q control-name resolver knows the preset's host", countryCode)
		}
		internalCache.Close()
	}

	// a tunnel cache with every DoH blocked never falls back to plain dns
	plainDns := &bootstrapPlainDns{answer: net.ParseIP("192.0.2.99")}
	tunnelSettings := DefaultDohSettings()
	tunnelSettings.DnsResolverSettings.EnableLocalDns = false
	tunnelSettings.DohServerStagger = 0
	tunnelCache := NewDohCache(tunnelSettings)
	defer tunnelCache.Close()
	tunnelCache.remoteClient.httpClient = &http.Client{Transport: &bootstrapDohTransport{}}
	tunnelCache.bootstrapLookupIP = plainDns.lookupIP
	if addrs, _ := tunnelCache.QueryResult(context.Background(), "A", "api.example.test"); len(addrs) != 0 {
		t.Fatalf("tunnel lookup = %v, expected none", addrs)
	}
	if names := plainDns.queried(); len(names) != 0 {
		t.Fatalf("tunnel plain dns queried %v", names)
	}
}

// Every DoH server blocked: the space's own names fall back to plain dns, and
// any other name never does.
func TestBootstrapPlainDnsOnlyForBootstrapNames(t *testing.T) {
	for _, countryCode := range []string{"", "cn", "us"} {
		t.Run(countryCode, func(t *testing.T) {
			transport := &bootstrapDohTransport{}
			plainAnswer := netip.MustParseAddr("192.0.2.20")
			plainDns := &bootstrapPlainDns{answer: net.IP(plainAnswer.AsSlice())}
			cache := newBootstrapTestCache(t, countryCode, transport, plainDns)

			addrs, _ := cache.QueryResult(context.Background(), "A", "api.example.test")
			if !slices.Equal(addrs, []netip.Addr{plainAnswer}) {
				t.Fatalf("bootstrap lookup = %v, expected %v over plain dns", addrs, plainAnswer)
			}
			// every encrypted resolver was tried before plain dns
			if len(transport.hosts()) == 0 {
				t.Fatal("no doh request before plain dns")
			}

			for _, name := range []string{"user-site.test", "example.test.evil.test", "notexample.test"} {
				if addrs, _ := cache.QueryResult(context.Background(), "A", name); len(addrs) != 0 {
					t.Fatalf("%s = %v, expected none", name, addrs)
				}
			}
			if names := plainDns.queried(); !slices.Equal(names, []string{"api.example.test"}) {
				t.Fatalf("plain dns queried %v, expected only the bootstrap name", names)
			}
		})
	}
}

// Every DoH server black-holed and no ingress country (support inbox 8397,
// urnetwork/windows#26; 7956): a control dial whose own deadline ends before
// the encrypted fan-out would, as the platform websocket's 5s handshake ends
// before the 15s DoH request timeout, still reaches the bootstrap plain dns
// fallback inside that deadline and dials its answer. Before, the fan-out held
// the dial's whole deadline, so the fallback never ran and every platform dial
// ended in "context deadline exceeded" with no address tried.
func TestInternalDohDialReachesBootstrapPlainDnsBeforeItsDeadline(t *testing.T) {
	dohDialer := &controlDohTestDialer{}
	dohSettings := controlDohTestDefaultSettings(dohDialer)
	// the fan-out outlasts the dial's own deadline below
	dohSettings.RequestTimeout = 6 * time.Second

	settings := DefaultClientStrategySettings()
	settings.InternalDohDomains = []string{"space.example"}
	settings.DohSettings = dohSettings
	strategy := NewClientStrategy(t.Context(), settings)
	defer strategy.Close()

	plainAnswer := netip.MustParseAddr("192.0.2.81")
	plainDns := &bootstrapPlainDns{answer: net.IP(plainAnswer.AsSlice())}
	strategy.internalDohResolver.getCache().bootstrapLookupIP = plainDns.lookupIP

	dialedAddresses := make(chan string, 4)
	dial := strategy.internalDohResolver.wrapDialContext(func(ctx context.Context, network string, address string) (net.Conn, error) {
		dialedAddresses <- address
		client, server := net.Pipe()
		server.Close()
		return client, nil
	})

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	conn, err := dial(ctx, "tcp4", "connect.space.example:443")
	if err != nil {
		t.Fatalf("control dial = %v, expected the bootstrap plain dns answer inside the dial's deadline (plain dns queried %v)", err, plainDns.queried())
	}
	conn.Close()
	if address := <-dialedAddresses; address != "192.0.2.81:443" {
		t.Fatalf("dialed %s, expected the plain dns answer", address)
	}
	if names := plainDns.queried(); !slices.Equal(names, []string{"connect.space.example"}) {
		t.Fatalf("plain dns queried %v, expected only the dialed name", names)
	}
	// the encrypted resolvers were asked first
	if dohDialer.blackholedCount.Load() == 0 {
		t.Fatal("no DoH server was dialed before plain dns")
	}
}

// A DoH answer is final: a found record or an NXDOMAIN never reaches plain
// dns, also for a caller whose deadline sets aside the fallback's share
// (`DohSettings.BootstrapPlainDnsReserve`).
func TestBootstrapPlainDnsNotQueriedWhenDohAnswers(t *testing.T) {
	defaults := DefaultDnsResolverSettings()
	defaultHosts := map[string]bool{}
	for _, dohUrl := range defaults.RemoteDohUrlsIpv4 {
		_, addr, err := ParseControlDohUrl(dohUrl)
		if err != nil {
			t.Fatal(err)
		}
		defaultHosts[addr.String()] = true
	}
	answer := netip.MustParseAddr("192.0.2.30")
	// no deadline, and one shorter than the DoH request timeout
	for _, timeout := range []time.Duration{0, 3 * time.Second} {
		for _, nxdomain := range []bool{false, true} {
			transport := &bootstrapDohTransport{answer: answer, answerHosts: defaultHosts, nxdomain: nxdomain}
			plainDns := &bootstrapPlainDns{answer: net.ParseIP("192.0.2.99")}
			cache := newBootstrapTestCache(t, "", transport, plainDns)
			ctx := context.Background()
			if 0 < timeout {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
			}
			addrs, authoritative := cache.QueryResult(ctx, "A", "api.example.test")
			if !authoritative {
				t.Fatalf("timeout %s, nxdomain %t: not authoritative", timeout, nxdomain)
			}
			if !nxdomain && !slices.Equal(addrs, []netip.Addr{answer}) {
				t.Fatalf("timeout %s: lookup = %v, expected %v", timeout, addrs, answer)
			}
			if nxdomain && len(addrs) != 0 {
				t.Fatalf("timeout %s: nxdomain lookup = %v, expected none", timeout, addrs)
			}
			if names := plainDns.queried(); len(names) != 0 {
				t.Fatalf("timeout %s, nxdomain %t: plain dns queried %v, expected never", timeout, nxdomain, names)
			}
		}
	}
}

// The plain servers of the fallback: those of the ingress country (cn), else
// the local (host) resolvers.
func TestBootstrapPlainDnsServers(t *testing.T) {
	settings := internalDohSettings(IngressDohSettings("cn", nil, nil), []string{"example.test"})
	if servers := bootstrapPlainDnsServers(settings, "udp4"); !slices.Equal(servers, RegionalDnsServerIps("cn")) || len(servers) == 0 {
		t.Fatalf("cn servers = %v, expected %v", servers, RegionalDnsServerIps("cn"))
	}
	for _, countryCode := range []string{"", "us"} {
		settings := internalDohSettings(IngressDohSettings(countryCode, nil, nil), []string{"example.test"})
		if servers := bootstrapPlainDnsServers(settings, "udp4"); !slices.Equal(servers, DefaultDnsResolverSettings().LocalDnsIpv4) {
			t.Fatalf("%q servers = %v, expected %v", countryCode, servers, DefaultDnsResolverSettings().LocalDnsIpv4)
		}
	}
}

// The pickable ingress countries are exactly those with a recommendation.
func TestIngressCountryCodes(t *testing.T) {
	countryCodes := IngressCountryCodes()
	if !slices.Contains(countryCodes, "cn") || slices.Contains(countryCodes, "us") || slices.Contains(countryCodes, "") {
		t.Fatalf("ingress countries = %v", countryCodes)
	}
	for _, countryCode := range countryCodes {
		strategy := IngressDnsStrategyForCountry(countryCode)
		if len(strategy.DohUrlsIpv4)+len(strategy.DohUrlsIpv6)+len(strategy.PlainDnsIpv4)+len(strategy.PlainDnsIpv6) == 0 {
			t.Fatalf("%s has no recommendation", countryCode)
		}
	}
}

// The ingress DoH clients always verify the server certificate: a server whose
// certificate is not trusted never answers, in any ingress country.
func TestIngressDohVerifiesCertificates(t *testing.T) {
	answer := netip.MustParseAddr("192.0.2.40")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeDohWire(w, r, []netip.Addr{answer}, 60, false)
	}))
	defer server.Close()

	for _, countryCode := range []string{"", "cn"} {
		settings := IngressDohSettings(countryCode, nil, nil)
		settings.DohServerStagger = 0
		settings.DialContextSettings = &DialContextSettings{
			DialContext: func(ctx context.Context, network string, addr string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
			},
		}
		cache := NewDohCache(internalDohSettings(settings, []string{"example.test"}))
		cache.bootstrapLookupIP = func(ctx context.Context, network string, host string) ([]net.IP, error) {
			return nil, errors.New("no plain dns in this test")
		}
		for _, client := range []*dohClient{cache.remoteClient, cache.localClient} {
			tlsConfig := client.httpClient.Transport.(*http.Transport).TLSClientConfig
			if tlsConfig.InsecureSkipVerify || tlsConfig.VerifyPeerCertificate != nil || tlsConfig.RootCAs != nil {
				t.Fatalf("%q ingress doh tls config skips or replaces verification", countryCode)
			}
		}
		if addrs, _ := cache.QueryResult(context.Background(), "A", "api.example.test"); len(addrs) != 0 {
			t.Fatalf("%q lookup = %v from an untrusted server, expected none", countryCode, addrs)
		}
		cache.Close()
	}
}
