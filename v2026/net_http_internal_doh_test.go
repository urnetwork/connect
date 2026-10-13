package connect

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientStrategiesOwnIndependentControlLimits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultClientStrategySettings()
	settings.InternalDohDomains = []string{"service.example.test"}
	first := NewClientStrategy(ctx, settings)
	defer first.Close()
	second := NewClientStrategy(ctx, settings)
	defer second.Close()
	if first.internalDohResolver == nil || second.internalDohResolver == nil {
		t.Fatal("protected-domain DoH controls were not created")
	}
	if first.internalDohResolver.cache == second.internalDohResolver.cache ||
		first.internalDohResolver.cache.resolveSem == second.internalDohResolver.cache.resolveSem ||
		first.internalDohResolver.cache.localClient.httpSem == second.internalDohResolver.cache.localClient.httpSem {
		t.Fatal("unrelated strategies shared DoH concurrency admission")
	}
	first.NextConnectTime()
	if second.nextConnectTime != (time.Time{}) {
		t.Fatal("one strategy's dial pacing advanced another strategy")
	}
}

// TestInternalDohBypassesPoisonedDefaultResolution is the field regression:
// the device/network DNS middleware never answers api.<domain>. The same HTTPS
// request succeeds when the NetworkSpace domain rule resolves over DoH and
// gives the socket layer a raw IP, while HTTP Host, TLS SNI, and certificate
// verification all continue to use the original api hostname.
func TestInternalDohBypassesPoisonedDefaultResolution(t *testing.T) {
	forEachIpVersion(t, func(t *testing.T, ipVersion int) {
		testInternalDohBypassesPoisonedDefaultResolution(t, ipVersion)
	})
}

func testInternalDohBypassesPoisonedDefaultResolution(t *testing.T, ipVersion int) {
	const domain = "service.test"
	const apiHost = "api." + domain

	certPem, keyPem, err := selfSign([]string{apiHost}, "internal doh test", time.Hour, 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair(certPem, keyPem)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certPem) {
		t.Fatal("could not add synthetic API certificate root")
	}

	requestHosts := make(chan string, 1)
	serverNames := make(chan string, 1)
	apiServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requestHosts <- request.Host
		_, _ = io.WriteString(w, "ok")
	}))
	// the api listens on the loopback of the family under test; the internal
	// DoH answer below points the protected name at that same loopback
	apiServer.Listener.Close()
	apiListener, err := net.Listen(testTcpNetwork(ipVersion), testLoopbackHostPort(ipVersion, 0))
	if err != nil {
		t.Fatal(err)
	}
	apiServer.Listener = apiListener
	apiServer.TLS = &tls.Config{
		Certificates: []tls.Certificate{certificate},
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			serverNames <- hello.ServerName
			return nil, nil
		},
	}
	apiServer.StartTLS()
	defer apiServer.Close()
	_, apiPort, err := net.SplitHostPort(apiServer.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	requestUrl := "https://" + net.JoinHostPort(apiHost, apiPort) + "/status"

	var dohQueries atomic.Int32
	dohServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		dohQueries.Add(1)
		writeDohWire(w, request, []netip.Addr{netip.MustParseAddr(testLoopbackIp(ipVersion))}, 60, false)
	}))
	defer dohServer.Close()

	var poisonedNameDials atomic.Int32
	poisonedDefaultDial := func(ctx context.Context, network string, address string) (net.Conn, error) {
		host, _, splitErr := net.SplitHostPort(address)
		if splitErr != nil {
			return nil, splitErr
		}
		if _, parseErr := netip.ParseAddr(host); parseErr != nil {
			poisonedNameDials.Add(1)
			<-ctx.Done()
			return nil, &net.DNSError{Err: "device DNS middleware timed out", Name: host, IsTimeout: true}
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}

	newSettings := func() *ClientStrategySettings {
		settings := DefaultClientStrategySettings()
		settings.EnableResilient = false
		settings.RequestTimeout = 200 * time.Millisecond
		settings.ConnectTimeout = 200 * time.Millisecond
		settings.TlsTimeout = time.Second
		settings.TlsConfig = &tls.Config{RootCAs: roots}
		settings.DialContextSettings = &DialContextSettings{DialContext: poisonedDefaultDial}
		dohSettings := DefaultDohSettings()
		dohSettings.RequestTimeout = time.Second
		dohSettings.DnsResolverSettings = &DnsResolverSettings{
			EnableRemoteDoh:   true,
			RemoteDohUrlsIpv4: []string{dohServer.URL},
		}
		settings.DohSettings = dohSettings
		return settings
	}

	// First pin the failure without the policy. The transport reaches the
	// poisoned/default name path and cannot connect.
	baseline := NewClientStrategy(t.Context(), newSettings())
	baselineClient := onlyClientStrategyHttpClient(t, baseline)
	_, err = baselineClient.Get(requestUrl)
	baseline.Close()
	if err == nil {
		t.Fatal("request unexpectedly survived the poisoned default resolver without internal DoH")
	}
	if poisonedNameDials.Load() == 0 {
		t.Fatal("baseline request did not reach the synthetic poisoned default resolver")
	}

	poisonedNameDials.Store(0)
	settings := newSettings()
	settings.InternalDohDomains = []string{domain}
	strategy := NewClientStrategy(t.Context(), settings)
	defer strategy.Close()
	response, err := onlyClientStrategyHttpClient(t, strategy).Get(requestUrl)
	if err != nil {
		t.Fatalf("request through internal DoH: %v", err)
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if string(body) != "ok" {
		t.Fatalf("response body = %q, expected ok", body)
	}
	if got := poisonedNameDials.Load(); got != 0 {
		t.Fatalf("protected request made %d hostname dials through poisoned default DNS", got)
	}
	if got := dohQueries.Load(); got == 0 {
		t.Fatal("protected request did not query the internal DoH resolver")
	}
	select {
	case got := <-serverNames:
		if got != apiHost {
			t.Fatalf("TLS SNI = %q, expected %q", got, apiHost)
		}
	case <-time.After(time.Second):
		t.Fatal("API server did not observe TLS SNI")
	}
	select {
	case got := <-requestHosts:
		expected := net.JoinHostPort(apiHost, apiPort)
		if got != expected {
			t.Fatalf("HTTP Host = %q, expected %q", got, expected)
		}
	case <-time.After(time.Second):
		t.Fatal("API server did not observe HTTP Host")
	}
}

func onlyClientStrategyHttpClient(t *testing.T, strategy *ClientStrategy) *http.Client {
	t.Helper()
	strategy.mutex.Lock()
	defer strategy.mutex.Unlock()
	if len(strategy.dialers) != 1 {
		t.Fatalf("strategy has %d dialers, expected one", len(strategy.dialers))
	}
	for dialer := range strategy.dialers {
		return dialer.HttpClient()
	}
	panic("unreachable")
}

func TestInternalDohDomainBoundary(t *testing.T) {
	resolver := &internalDohResolver{
		domains: normalizeInternalDohDomains([]string{
			" Example.COM. ",
			"migration.test",
			"example.com",
			"test",
		}),
	}
	tests := []struct {
		host    string
		matches bool
	}{
		{host: "example.com", matches: true},
		{host: "api.example.com", matches: true},
		{host: "deep.api.example.com.", matches: true},
		{host: "connect.migration.test", matches: true},
		{host: "evil-example.com", matches: false},
		{host: "example.com.attacker.test", matches: false},
		{host: "notexample.com", matches: false},
		{host: "api.test", matches: false},
		{host: "192.0.2.1", matches: false},
	}
	for _, test := range tests {
		t.Run(test.host, func(t *testing.T) {
			if got := resolver.matches(test.host); got != test.matches {
				t.Fatalf("matches(%q) = %t, expected %t", test.host, got, test.matches)
			}
		})
	}
}

func TestInternalDohDoesNotOverrideCustomResolver(t *testing.T) {
	custom := &net.Resolver{PreferGo: true}
	settings := DefaultClientStrategySettings()
	settings.InternalDohDomains = []string{"service.test"}
	settings.Resolver = custom
	strategy := NewClientStrategy(t.Context(), settings)
	defer strategy.Close()
	if strategy.internalDohResolver != nil {
		t.Fatal("strategy installed internal DoH over an explicit resolver")
	}
	if strategy.settings.Resolver != custom {
		t.Fatal("strategy did not preserve the explicit resolver")
	}
}

// Protected-domain UDP resolution is the direct QUIC/packet-translation path;
// it does not pass through ConnectSettings.DialContext. The family policy is
// therefore read inside resolveUDPAddr, on every call, rather than captured
// when the client strategy is constructed.
func TestInternalDohUdpFollowsRuntimeFamilyPolicy(t *testing.T) {
	dohServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		writeDohWire(w, request, []netip.Addr{
			netip.MustParseAddr("192.0.2.1"),
			netip.MustParseAddr("2001:db8::1"),
		}, 60, false)
	}))
	defer dohServer.Close()

	dohSettings := DefaultDohSettings()
	dohSettings.RequestTimeout = time.Second
	dohSettings.DnsResolverSettings = &DnsResolverSettings{
		EnableRemoteDoh:   true,
		RemoteDohUrlsIpv4: []string{dohServer.URL},
	}
	resolver := &internalDohResolver{
		cache: NewDohCache(internalDohSettings(dohSettings)),
	}
	defer resolver.Close()
	defer SetControlIpFamilyPolicy(IpFamilyAuto)

	tests := []struct {
		policy IpFamilyPolicy
		want   netip.Addr
	}{
		{policy: IpFamilyForce4, want: netip.MustParseAddr("192.0.2.1")},
		{policy: IpFamilyForce6, want: netip.MustParseAddr("2001:db8::1")},
	}
	for _, test := range tests {
		SetControlIpFamilyPolicy(test.policy)
		addr, err := resolver.resolveUDPAddr(t.Context(), "api.service.test:443")
		if err != nil {
			t.Fatalf("resolve under policy %d: %v", test.policy, err)
		}
		got, ok := netip.AddrFromSlice(addr.IP)
		if !ok || got.Unmap() != test.want {
			t.Fatalf("resolve under policy %d = %v, want %v", test.policy, addr.IP, test.want)
		}
	}
}

// The protected-domain UDP path parses its address before touching the DoH
// cache. Empty hosts and numeric ports net cannot dial must fail immediately.
func TestInternalDohUDPRejectsMalformedAddress(t *testing.T) {
	resolver := &internalDohResolver{}
	addrs := []string{
		"",
		":53",
		"service.test.:-1",
		"service.test.:65536",
	}
	for _, addr := range addrs {
		if _, err := resolver.resolveUDPAddr(t.Context(), addr); err == nil {
			t.Errorf("resolve %q succeeded, want an address error", addr)
		}
	}
}

func TestInternalDohRawDialFallsBackAcrossAddressFamilies(t *testing.T) {
	addrs := orderInternalDohAddrs([]netip.Addr{
		netip.MustParseAddr("192.0.2.1"),
		netip.MustParseAddr("2001:db8::1"),
	})
	var attempts atomic.Int32
	peer := make(chan net.Conn, 1)
	dial := func(ctx context.Context, network string, address string) (net.Conn, error) {
		attempts.Add(1)
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		if strings.Contains(host, ":") {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		client, server := net.Pipe()
		peer <- server
		return client, nil
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	started := time.Now()
	conn, err := dialInternalDohAddrs(ctx, "tcp", "443", addrs, dial)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	server := <-peer
	defer server.Close()
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("address-family fallback took %s, expected less than the request deadline", elapsed)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("raw dial attempts = %d, expected IPv6 then IPv4", got)
	}
}

// A protected name resolves through a user's bootstrap DoH server when every
// default server is black-holed (P216). The named server is tried first, in
// one of the internal cache's four request slots, rather than queued behind
// defaults that never answer: without that every lookup that drew four
// defaults first would time out.
func TestInternalDohResolvesThroughControlDohWhenTheDefaultsAreUnreachable(t *testing.T) {
	answer := netip.MustParseAddr("192.0.2.53")
	var queries atomic.Int32
	dohUrl, tlsConfig, address := newControlDohTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries.Add(1)
		writeDohWire(w, r, []netip.Addr{answer}, 60, false)
	}))
	dialer := &controlDohTestDialer{allowedAddresses: []string{address}}

	settings := DefaultClientStrategySettings()
	settings.InternalDohDomains = []string{"space.example"}
	settings.DohSettings = controlDohTestSettings(dohUrl, tlsConfig, dialer)
	strategy := NewClientStrategy(t.Context(), settings)
	defer strategy.Close()

	// distinct names, so each lookup is a fresh query that must reach a server
	for i := range 6 {
		host := fmt.Sprintf("api%d.space.example", i)
		addrs, err := strategy.internalDohResolver.resolve(t.Context(), "tcp4", host)
		if err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		if !slices.Equal(addrs, []netip.Addr{answer}) {
			t.Fatalf("%s = %v, expected %s", host, addrs, answer)
		}
	}
	if n := queries.Load(); n < 6 {
		t.Fatalf("the named server answered %d queries, expected one per name", n)
	}
	if dialer.blackholedCount.Load() == 0 {
		t.Fatal("no default server was dialed, so nothing showed them unreachable")
	}
}

// SetInternalDohSettings swaps the cache of a running strategy: a name the
// black-holed defaults could not resolve resolves through the named server
// once it is set, the replaced cache is retired, and the settings the extender
// bootstrap reads are the new ones. A closed strategy keeps nothing it is
// handed.
func TestSetInternalDohSettingsReplacesTheCacheInPlace(t *testing.T) {
	answer := netip.MustParseAddr("192.0.2.54")
	dohUrl, tlsConfig, address := newControlDohTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeDohWire(w, r, []netip.Addr{answer}, 60, false)
	}))
	dialer := &controlDohTestDialer{allowedAddresses: []string{address}}

	settings := DefaultClientStrategySettings()
	settings.InternalDohDomains = []string{"space.example"}
	settings.DohSettings = controlDohTestDefaultSettings(dialer)
	strategy := NewClientStrategy(t.Context(), settings)
	defer strategy.Close()

	if addrs, err := strategy.internalDohResolver.resolve(t.Context(), "tcp4", "api.space.example"); err == nil {
		t.Fatalf("resolved %v through black-holed defaults", addrs)
	}
	replacedCache := strategy.internalDohResolver.getCache()

	controlSettings := controlDohTestSettings(dohUrl, tlsConfig, dialer)
	strategy.SetInternalDohSettings(controlSettings)
	if strategy.DohSettings() != controlSettings {
		t.Fatal("the strategy does not report the settings it was given")
	}
	if strategy.internalDohResolver.getCache() == replacedCache {
		t.Fatal("the internal DoH cache was not replaced")
	}
	if !replacedCache.lifecycle.retired.Load() {
		t.Fatal("the replaced cache was left open")
	}
	addrs, err := strategy.internalDohResolver.resolve(t.Context(), "tcp4", "api.space.example")
	if err != nil {
		t.Fatalf("after the swap: %v", err)
	}
	if !slices.Equal(addrs, []netip.Addr{answer}) {
		t.Fatalf("after the swap = %v, expected %s", addrs, answer)
	}

	// nil restores the defaults
	strategy.SetInternalDohSettings(nil)
	if dohUrls := strategy.DohSettings().DnsResolverSettings.RemoteDohUrlsIpv4; !slices.Equal(dohUrls, DefaultDnsResolverSettings().RemoteDohUrlsIpv4) {
		t.Fatalf("nil settings = %v, expected the defaults", dohUrls)
	}

	strategy.Close()
	closedCache := strategy.internalDohResolver.getCache()
	if !closedCache.lifecycle.retired.Load() {
		t.Fatal("closing the strategy left its cache open")
	}
	strategy.SetInternalDohSettings(controlDohTestSettings(dohUrl, tlsConfig, dialer))
	if strategy.internalDohResolver.getCache() != closedCache {
		t.Fatal("a closed strategy installed a new cache")
	}
}
