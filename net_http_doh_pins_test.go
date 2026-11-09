package connect

// The pinned DoH server hosts (DnsResolverSettings.DohServerHostAddrs,
// ADBLOCK3RD B1, B3, C11), the redaction of DoH urls (B7), the sink answers'
// exit binding (B4) and the HTTP/2 negotiation (section 7) at the DohCache.
// A pinned host is dialed at its addresses and never looked up, on the tunnel
// path, the host path and the one-shot path; a lookup of it answers the pins
// for the query's family; a dead pin set fails closed. The servers are local
// httptest servers with certificates from a private root, so nothing here
// touches the network.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

const testPinnedDohHost = "dns.example"

// A DoH server on the loopback whose certificate names certName, issued by a
// private root the client trusts (rootCAs), over TLS with h2 enabled. It
// answers every query with records (writeDohWire) and counts its requests by
// HTTP version.
type testPinnedDohServer struct {
	server        *httptest.Server
	rootCAs       *x509.CertPool
	requests      atomic.Int32
	http2Requests atomic.Int32
}

func newTestPinnedDohServer(t *testing.T, certName string, records ...netip.Addr) *testPinnedDohServer {
	t.Helper()
	rootCAs, certificate := newTestTlsHelloCertificates(t, certName)
	self := &testPinnedDohServer{rootCAs: rootCAs}
	self.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		self.requests.Add(1)
		if r.ProtoMajor == 2 {
			self.http2Requests.Add(1)
		}
		writeDohWire(w, r, records, 60, false)
	}))
	self.server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}}
	self.server.EnableHTTP2 = true
	self.server.StartTLS()
	t.Cleanup(self.server.Close)
	return self
}

// The server's listening address.
func (self *testPinnedDohServer) addrPort(t *testing.T) netip.AddrPort {
	t.Helper()
	addrPort, err := netip.ParseAddrPort(self.server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return addrPort
}

// A DoH url naming the server by host, on the server's port.
func (self *testPinnedDohServer) url(t *testing.T, host string) string {
	t.Helper()
	return "https://" + net.JoinHostPort(host, strconv.Itoa(int(self.addrPort(t).Port()))) + "/dns-query"
}

// A stand-in for a DoH client's dialer: records every network and address it
// is asked for, sends plain dns (:53) to the fake dns server when there is one,
// and refuses or dials the rest.
type testPinDialer struct {
	stateLock sync.Mutex
	networks  []string
	addresses []string
	dns       *fakeDnsServer
	refuse    atomic.Bool
}

func (self *testPinDialer) dial(ctx context.Context, network string, address string) (net.Conn, error) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.networks = append(self.networks, network)
		self.addresses = append(self.addresses, address)
	}()
	if strings.HasSuffix(address, ":53") && self.dns != nil {
		return (&net.Dialer{}).DialContext(ctx, network, self.dns.addr())
	}
	if self.refuse.Load() {
		return nil, &net.OpError{Op: "dial", Net: network, Err: syscall.ECONNREFUSED}
	}
	return (&net.Dialer{}).DialContext(ctx, network, address)
}

func (self *testPinDialer) dials() (networks []string, addresses []string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.networks), slices.Clone(self.addresses)
}

// A host resolver that fails the test if anything resolves through it.
func testFailingHostResolver(t *testing.T, used *atomic.Int32) *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network string, address string) (net.Conn, error) {
			used.Add(1)
			t.Errorf("the host resolver was asked to dial %s %s", network, address)
			return nil, errors.New("synthetic host resolver refusal")
		},
	}
}

// The preset expansion's shape (ADBLOCK3RD A6) for one hostname url: remote
// DoH only, the url in both lists, the pins, and remote plain dns disabled.
// The remote plain dns list names a server anyway, so a lookup of the server
// name, were one attempted, would reach the fake dns server.
func testPinnedResolverSettings(dohUrl string, pins []string, rootCAs *x509.CertPool) *DnsResolverSettings {
	return &DnsResolverSettings{
		EnableRemoteDoh:    true,
		RemoteDohUrlsIpv4:  []string{dohUrl},
		RemoteDohUrlsIpv6:  []string{dohUrl},
		RemoteDnsIpv4:      []string{"192.0.2.53"},
		DohServerHostAddrs: map[string][]string{testPinnedDohHost: pins},
		TlsConfig:          &tls.Config{RootCAs: rootCAs},
	}
}

func testPinnedDohSettings(resolver *DnsResolverSettings, dialer *testPinDialer) *DohSettings {
	settings := DefaultDohSettings()
	settings.Log = NewNoopLogger()
	settings.RequestTimeout = 5 * time.Second
	settings.DohServerStagger = 0
	settings.DnsResolverSettings = resolver
	if dialer != nil {
		settings.DialContextSettings = &DialContextSettings{DialContext: dialer.dial}
	}
	return settings
}

// Every address in addresses is an ip literal and one of allowed.
func testAllDialsTo(t *testing.T, addresses []string, allowed ...string) {
	t.Helper()
	for _, address := range addresses {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			t.Fatalf("dial address %q: %v", address, err)
		}
		if _, err := netip.ParseAddr(host); err != nil {
			t.Fatalf("a dial named the host %q instead of a pinned address", address)
		}
		if !slices.Contains(allowed, address) {
			t.Fatalf("dial to %s, expected only %v", address, allowed)
		}
	}
}

// The tunnel path: a hostname DoH url with pins resolves through a server whose
// certificate names the host, every dial goes to the pin, the fake plain dns
// server sees nothing, the pins are reported as the host's addresses, and the
// request is HTTP/2, so the pinned dial keeps the transport's ALPN offer.
func TestDohPinsTunnelPathDialsThePinsOverHttp2WithoutPlainDns(t *testing.T) {
	answer := netip.MustParseAddr("203.0.113.61")
	server := newTestPinnedDohServer(t, testPinnedDohHost, answer)
	pin := server.addrPort(t).Addr()
	fakeDns := newFakeDnsServer(t, map[string]netip.Addr{testPinnedDohHost: pin})
	defer fakeDns.close()
	dialer := &testPinDialer{dns: fakeDns}

	settings := testPinnedDohSettings(testPinnedResolverSettings(server.url(t, testPinnedDohHost), []string{pin.String()}, server.rootCAs), dialer)
	var resolvedLock sync.Mutex
	resolved := map[string][]netip.Addr{}
	settings.DohServerResolvedCallback = func(domain string, addrs []netip.Addr) {
		resolvedLock.Lock()
		defer resolvedLock.Unlock()
		resolved[domain] = addrs
	}
	cache := NewDohCache(settings)
	defer cache.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	addrs, authoritative := cache.QueryResult(ctx, "A", "site.example")
	if !authoritative || !slices.Equal(addrs, []netip.Addr{answer}) {
		t.Fatalf("QueryResult = %v (authoritative %t), expected %s", addrs, authoritative, answer)
	}
	if got := fakeDns.requests.Load(); got != 0 {
		t.Fatalf("the plain dns server saw %d queries; a pinned host is never looked up", got)
	}
	_, addresses := dialer.dials()
	if len(addresses) == 0 {
		t.Fatal("no dial through the tunnel dialer")
	}
	testAllDialsTo(t, addresses, server.addrPort(t).String())
	if server.requests.Load() == 0 || server.http2Requests.Load() != server.requests.Load() {
		t.Fatalf("requests %d, of them over HTTP/2 %d: the pinned dial must keep h2", server.requests.Load(), server.http2Requests.Load())
	}
	resolvedLock.Lock()
	if !slices.Equal(resolved[testPinnedDohHost], []netip.Addr{pin}) {
		t.Fatalf("server resolved callback = %v, expected the pins under %s", resolved, testPinnedDohHost)
	}
	resolvedLock.Unlock()

	// the control: the same settings without pins look the server name up
	// over remote plain dns through the tunnel, so the fake server above was
	// in a position to see a lookup
	unpinnedResolver := testPinnedResolverSettings(server.url(t, testPinnedDohHost), nil, server.rootCAs)
	unpinnedResolver.DohServerHostAddrs = nil
	unpinned := NewDohCache(testPinnedDohSettings(unpinnedResolver, dialer))
	defer unpinned.Close()
	if addrs, _ := unpinned.QueryResult(ctx, "A", testPinnedDohHost); !slices.Equal(addrs, []netip.Addr{pin}) {
		t.Fatalf("unpinned server name lookup = %v, expected %s from the plain dns server", addrs, pin)
	}
	if fakeDns.requests.Load() == 0 {
		t.Fatal("the unpinned control never reached the plain dns server")
	}
}

// The host path: the local DoH client dials the pins, and the host resolver,
// which would answer a hostname dial, is never asked.
func TestDohPinsHostPathNeverUsesTheHostResolver(t *testing.T) {
	answer := netip.MustParseAddr("203.0.113.62")
	server := newTestPinnedDohServer(t, testPinnedDohHost, answer)
	pin := server.addrPort(t).Addr()
	dohUrl := server.url(t, testPinnedDohHost)

	var hostResolverUses atomic.Int32
	tunnel := &testPinDialer{}
	tunnel.refuse.Store(true)
	settings := testPinnedDohSettings(&DnsResolverSettings{
		EnableLocalDoh:     true,
		LocalDohUrlsIpv4:   []string{dohUrl},
		LocalDohUrlsIpv6:   []string{dohUrl},
		DohServerHostAddrs: map[string][]string{testPinnedDohHost: {pin.String()}},
		TlsConfig:          &tls.Config{RootCAs: server.rootCAs},
	}, tunnel)
	settings.Resolver = testFailingHostResolver(t, &hostResolverUses)
	cache := NewDohCache(settings)
	defer cache.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	addrs, authoritative := cache.QueryResult(ctx, "A", "site.example")
	if !authoritative || !slices.Equal(addrs, []netip.Addr{answer}) {
		t.Fatalf("QueryResult = %v (authoritative %t), expected %s over the host path", addrs, authoritative, answer)
	}
	if got := hostResolverUses.Load(); got != 0 {
		t.Fatalf("the host resolver was used %d times", got)
	}
	if _, addresses := tunnel.dials(); len(addresses) != 0 {
		t.Fatalf("the host path dialed the tunnel: %v", addresses)
	}
	if server.http2Requests.Load() == 0 {
		t.Fatal("the host path did not speak HTTP/2 to the pinned server")
	}
}

// Every pin refused (an exit that blocks the provider, or pins gone stale):
// the resolution fails, with no plain dns query, no host resolver lookup, and
// no dial anywhere but the pins.
func TestDohPinsFailClosedWhenEveryPinIsRefused(t *testing.T) {
	fakeDns := newFakeDnsServer(t, map[string]netip.Addr{testPinnedDohHost: netip.MustParseAddr("192.0.2.99")})
	defer fakeDns.close()
	dialer := &testPinDialer{dns: fakeDns}
	dialer.refuse.Store(true)
	var hostResolverUses atomic.Int32

	settings := testPinnedDohSettings(testPinnedResolverSettings(
		"https://"+testPinnedDohHost+"/dns-query",
		[]string{"192.0.2.81", "2001:db8::81"},
		x509.NewCertPool(),
	), dialer)
	settings.Resolver = testFailingHostResolver(t, &hostResolverUses)
	cache := NewDohCache(settings)
	defer cache.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	addrs, authoritative := cache.QueryResult(ctx, "A", "site.example")
	if 0 < len(addrs) || authoritative {
		t.Fatalf("QueryResult = %v (authoritative %t) with every pin refused, expected a failure", addrs, authoritative)
	}
	if got := fakeDns.requests.Load(); got != 0 {
		t.Fatalf("the plain dns server saw %d queries", got)
	}
	if got := hostResolverUses.Load(); got != 0 {
		t.Fatalf("the host resolver was used %d times", got)
	}
	_, addresses := dialer.dials()
	if len(addresses) == 0 {
		t.Fatal("the pins were never dialed")
	}
	testAllDialsTo(t, addresses, "192.0.2.81:443", "[2001:db8::81]:443")
}

// A pin that points at a server whose certificate names another host fails:
// the pinned dial changes the address, never the name TLS verifies.
func TestDohPinsStillVerifyTheServerName(t *testing.T) {
	server := newTestPinnedDohServer(t, "other.example", netip.MustParseAddr("203.0.113.63"))
	pin := server.addrPort(t).Addr()
	dialer := &testPinDialer{}
	settings := testPinnedDohSettings(testPinnedResolverSettings(server.url(t, testPinnedDohHost), []string{pin.String()}, server.rootCAs), dialer)
	cache := NewDohCache(settings)
	defer cache.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	addrs, authoritative := cache.QueryResult(ctx, "A", "site.example")
	if 0 < len(addrs) || authoritative {
		t.Fatalf("QueryResult = %v (authoritative %t) from a server certified for another name", addrs, authoritative)
	}
	if got := server.requests.Load(); got != 0 {
		t.Fatalf("the server answered %d requests past a failed verification", got)
	}
	_, err := cache.remoteClient.queryWireDetailed(ctx, server.url(t, testPinnedDohHost), "A", "site.example")
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("query error = %v, expected a certificate verification failure", err)
	}
}

// A v4-only pin set answers AAAA for the host empty and authoritative, and A
// with the pin; a dial of the host uses v4 only. A v6-only set is the mirror
// image, and a dial narrowed to the family it lacks fails without a lookup.
func TestDohPinsAnswerAndDialByFamily(t *testing.T) {
	answer := netip.MustParseAddr("203.0.113.64")
	server := newTestPinnedDohServer(t, testPinnedDohHost, answer)
	pin := server.addrPort(t).Addr()
	if !pin.Is4() {
		t.Skip("the test server did not listen on the v4 loopback")
	}
	dialer := &testPinDialer{}
	cache := NewDohCache(testPinnedDohSettings(testPinnedResolverSettings(server.url(t, testPinnedDohHost), []string{pin.String()}, server.rootCAs), dialer))
	defer cache.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if addrs, authoritative := cache.QueryResult(ctx, "AAAA", testPinnedDohHost); 0 < len(addrs) || !authoritative {
		t.Fatalf("AAAA of a v4-only pinned host = %v (authoritative %t), expected an authoritative empty answer", addrs, authoritative)
	}
	if addrs, authoritative := cache.QueryResult(ctx, "A", "DNS.Example."); !authoritative || !slices.Equal(addrs, []netip.Addr{pin}) {
		t.Fatalf("A of the pinned host = %v (authoritative %t), expected the pin", addrs, authoritative)
	}
	if server.requests.Load() != 0 {
		t.Fatal("a lookup of the pinned host reached a DoH server")
	}
	if addrs, _ := cache.QueryResult(ctx, "A", "site.example"); !slices.Equal(addrs, []netip.Addr{answer}) {
		t.Fatalf("lookup through the v4 pin = %v, expected %s", addrs, answer)
	}
	networks, _ := dialer.dials()
	for _, network := range networks {
		if network != "tcp4" {
			t.Fatalf("a dial of a v4-only pinned host used %s", network)
		}
	}

	pins := map[string][]netip.Addr{testPinnedDohHost: {netip.MustParseAddr("2001:db8::64")}}
	recorder := &testPinDialer{}
	recorder.refuse.Store(true)
	dial := dohPinnedDialContext(pins, nil, recorder.dial)
	if _, err := dial(ctx, "tcp", net.JoinHostPort(testPinnedDohHost, "443")); err == nil {
		t.Fatal("a refused dial succeeded")
	}
	if networks, addresses := recorder.dials(); !slices.Equal(networks, []string{"tcp6"}) || !slices.Equal(addresses, []string{"[2001:db8::64]:443"}) {
		t.Fatalf("v6-only pins dialed %v %v, expected tcp6 to the pin", networks, addresses)
	}
	_, err := dial(ctx, "tcp4", net.JoinHostPort(testPinnedDohHost, "443"))
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) || !dnsErr.IsNotFound {
		t.Fatalf("a v4 dial of a v6-only pinned host = %v, expected a not found error", err)
	}
	if _, addresses := recorder.dials(); len(addresses) != 1 {
		t.Fatalf("a dial of the missing family reached the dialer: %v", addresses)
	}
	// an unpinned host passes through untouched
	if _, err := dial(ctx, "tcp", "other.example:443"); err == nil {
		t.Fatal("a refused dial succeeded")
	}
	if _, addresses := recorder.dials(); addresses[len(addresses)-1] != "other.example:443" {
		t.Fatalf("an unpinned host was rewritten: %v", addresses)
	}
}

// A pinned host is never looked up through the cache: an opaque record type is
// not forwarded, a TXT lookup answers empty, and no request leaves.
func TestDohPinnedHostIsNeverForwarded(t *testing.T) {
	settings := testPinnedDohSettings(testPinnedResolverSettings(
		"https://"+testPinnedDohHost+"/dns-query",
		[]string{"192.0.2.82"},
		x509.NewCertPool(),
	), nil)
	cache := NewDohCache(settings)
	defer cache.Close()
	var requests atomic.Int32
	cache.remoteClient.httpClient.Transport = dohRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("synthetic resolver failure")
	})
	ctx := context.Background()
	if response, usable := cache.Forward(ctx, dnsTypeHttps, testPinnedDohHost); usable || response != nil {
		t.Fatal("an HTTPS lookup of a pinned host was forwarded")
	}
	if addrs, authoritative := cache.QueryResult(ctx, "TXT", testPinnedDohHost); 0 < len(addrs) || !authoritative {
		t.Fatalf("TXT of a pinned host = %v (authoritative %t), expected an authoritative empty answer", addrs, authoritative)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("%d requests left for a pinned host", got)
	}
}

// A listed host whose addresses are all unusable still never resolves: a
// lookup answers empty, and a query through it fails without dialing.
func TestDohPinnedHostWithNoUsableAddressFailsClosed(t *testing.T) {
	fakeDns := newFakeDnsServer(t, map[string]netip.Addr{testPinnedDohHost: netip.MustParseAddr("192.0.2.99")})
	defer fakeDns.close()
	dialer := &testPinDialer{dns: fakeDns}
	resolver := testPinnedResolverSettings("https://"+testPinnedDohHost+"/dns-query", nil, x509.NewCertPool())
	resolver.DohServerHostAddrs = map[string][]string{"DNS.Example.": {"not an address", "0.0.0.0", "fe80::1%en0"}}
	cache := NewDohCache(testPinnedDohSettings(resolver, dialer))
	defer cache.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, recordType := range []string{"A", "AAAA"} {
		if addrs, authoritative := cache.QueryResult(ctx, recordType, testPinnedDohHost); 0 < len(addrs) || !authoritative {
			t.Fatalf("%s of a pinned host with no usable address = %v (authoritative %t)", recordType, addrs, authoritative)
		}
	}
	if addrs, authoritative := cache.QueryResult(ctx, "A", "site.example"); 0 < len(addrs) || authoritative {
		t.Fatalf("QueryResult = %v (authoritative %t) through a dead pin set", addrs, authoritative)
	}
	if _, addresses := dialer.dials(); len(addresses) != 0 {
		t.Fatalf("a dead pin set dialed %v", addresses)
	}
	if got := fakeDns.requests.Load(); got != 0 {
		t.Fatalf("the plain dns server saw %d queries", got)
	}
}

// The one-shot queries (DohQuery, DohQueryTxt) dial pins like the cache.
func TestDohOneShotQueriesDialThePins(t *testing.T) {
	answer := netip.MustParseAddr("203.0.113.65")
	server := newTestPinnedDohServer(t, testPinnedDohHost, answer)
	pin := server.addrPort(t).Addr()
	dialer := &testPinDialer{}
	settings := testPinnedDohSettings(testPinnedResolverSettings(server.url(t, testPinnedDohHost), []string{pin.String()}, server.rootCAs), dialer)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result := DohQuery(ctx, 0, "A", settings, "site.example")
	if _, ok := result[answer]; !ok {
		t.Fatalf("DohQuery = %v, expected %s", result, answer)
	}
	_, addresses := dialer.dials()
	if len(addresses) == 0 {
		t.Fatal("the one-shot query never dialed")
	}
	testAllDialsTo(t, addresses, server.addrPort(t).String())
}

// Every DoH client is built to offer h2 first by ALPN, with http/1.1 behind it.
// An h2 server that refuses a client offering no h2 (as DNS4EU does) is
// answered over HTTP/2, and a server that speaks only HTTP/1.1 is still
// answered, over HTTP/1.1.
func TestDohClientNegotiatesHttp2ByAlpn(t *testing.T) {
	answer := netip.MustParseAddr("203.0.113.66")
	newServer := func(http2 bool, protoMajors *sync.Map) *httptest.Server {
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			protoMajors.Store(r.ProtoMajor, true)
			writeDohWire(w, r, []netip.Addr{answer}, 60, false)
		}))
		if http2 {
			server.EnableHTTP2 = true
			server.TLS = &tls.Config{
				NextProtos: []string{"h2"},
				// refuse a client that does not offer h2
				GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
					if !slices.Contains(hello.SupportedProtos, "h2") {
						return nil, errors.New("no h2 offered")
					}
					return nil, nil
				},
			}
		}
		server.StartTLS()
		t.Cleanup(server.Close)
		return server
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for _, http2 := range []bool{true, false} {
		var protoMajors sync.Map
		server := newServer(http2, &protoMajors)
		pool := x509.NewCertPool()
		pool.AddCert(server.Certificate())
		newSettings := func(resolver *DnsResolverSettings) *DohSettings {
			settings := DefaultDohSettings()
			settings.Log = NewNoopLogger()
			settings.RequestTimeout = 5 * time.Second
			resolver.TlsConfig = &tls.Config{RootCAs: pool}
			settings.DnsResolverSettings = resolver
			settings.DialContextSettings = &DialContextSettings{DialContext: (&net.Dialer{}).DialContext}
			return settings
		}

		remote := NewDohCache(newSettings(&DnsResolverSettings{EnableRemoteDoh: true, RemoteDohUrlsIpv4: []string{server.URL}}))
		local := NewDohCache(newSettings(&DnsResolverSettings{EnableLocalDoh: true, LocalDohUrlsIpv4: []string{server.URL}}))
		for _, c := range []struct {
			cache  *DohCache
			client *dohClient
		}{
			{cache: remote, client: remote.remoteClient},
			{cache: local, client: local.localClient},
		} {
			addrs, authoritative := c.cache.QueryResult(ctx, "A", "alpn.example")
			if !authoritative || !slices.Equal(addrs, []netip.Addr{answer}) {
				t.Fatalf("h2 server %t: QueryResult = %v (authoritative %t)", http2, addrs, authoritative)
			}
			// the transport settles its ALPN offer at its first request
			// (net/http configureHTTP2, enabled by http2.ConfigureTransports)
			nextProtos := c.client.httpClient.Transport.(*http.Transport).TLSClientConfig.NextProtos
			if !slices.Equal(nextProtos, []string{"h2", "http/1.1"}) {
				t.Fatalf("a DoH client offered ALPN %v, expected h2 ahead of http/1.1", nextProtos)
			}
			c.cache.Close()
		}
		if result := DohQuery(ctx, 0, "A", newSettings(&DnsResolverSettings{EnableRemoteDoh: true, RemoteDohUrlsIpv4: []string{server.URL}}), "alpn.example"); len(result) != 1 {
			t.Fatalf("h2 server %t: DohQuery = %v", http2, result)
		}
		want := 1
		if http2 {
			want = 2
		}
		protoMajors.Range(func(key any, _ any) bool {
			if key.(int) != want {
				t.Fatalf("h2 server %t: a request arrived over HTTP/%d, expected HTTP/%d", http2, key.(int), want)
			}
			return true
		})
		if _, ok := protoMajors.Load(want); !ok {
			t.Fatalf("h2 server %t: no request arrived", http2)
		}
	}
}

// The loggable form of a DoH url is its scheme and host: never the path, where
// a personal configuration id sits, nor user info, a query or a fragment.
func TestDohUrlForLogKeepsSchemeAndHostOnly(t *testing.T) {
	cases := []struct {
		dohUrl string
		want   string
	}{
		{dohUrl: "https://dns.example/abc123", want: "https://dns.example"},
		{dohUrl: " HTTPS://dns.example:8443/dns-query/abc123/device ", want: "https://dns.example:8443"},
		{dohUrl: "https://[2001:db8::53]/dns-query", want: "https://[2001:db8::53]"},
		{dohUrl: "https://user:secret@dns.example/abc123?dns=AAAB#frag", want: "https://dns.example"},
		{dohUrl: "dns.example/abc123", want: "<doh url>"},
		{dohUrl: "https://dns.example/%zz", want: "<doh url>"},
		{dohUrl: "", want: "<doh url>"},
	}
	for _, c := range cases {
		if got := DohUrlForLog(c.dohUrl); got != c.want {
			t.Errorf("DohUrlForLog(%q) = %q, expected %q", c.dohUrl, got, c.want)
		}
	}
}

// A failing personal-path url: neither the query errors nor the warm failure
// line carry its path (ADBLOCK3RD B7).
func TestDohErrorsAndWarmLineNameNoPath(t *testing.T) {
	const personalId = "abc123def0"
	failing := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failing.Close()
	servfail := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := testingDohRequestQuery(t, r)
		w.Header().Set("Content-Type", "application/dns-message")
		w.Write(testingDohPolicyWire(t, raw, testingDohPolicyAnswer{rcode: dnsmessage.RCodeServerFailure, ede: -1}))
	}))
	defer servfail.Close()
	closed := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closedUrl := closed.URL
	closed.Close()

	pool := x509.NewCertPool()
	pool.AddCert(failing.Certificate())
	pool.AddCert(servfail.Certificate())
	log := newRecordingLogger()
	for _, base := range []string{failing.URL, servfail.URL, closedUrl} {
		dohUrl := base + "/" + personalId + "/dns-query"
		settings := DefaultDohSettings()
		settings.Log = log
		settings.RequestTimeout = 5 * time.Second
		settings.DnsResolverSettings = &DnsResolverSettings{
			EnableRemoteDoh:   true,
			RemoteDohUrlsIpv4: []string{dohUrl},
			TlsConfig:         &tls.Config{RootCAs: pool},
		}
		settings.DialContextSettings = &DialContextSettings{DialContext: (&net.Dialer{}).DialContext}
		cache := NewDohCache(settings)
		_, err := cache.remoteClient.queryWireDetailed(context.Background(), dohUrl, "A", "secret-name.example")
		if err != nil && (strings.Contains(err.Error(), personalId) || strings.Contains(err.Error(), "/dns-query")) {
			t.Errorf("query error exposed the url path: %q", err)
		}
		if cache.Warm(context.Background(), 1) {
			t.Errorf("%s: warm succeeded against a failing server", DohUrlForLog(dohUrl))
		}
		cache.Close()
	}
	warmLines := log.linesWith("[dns]warm failed")
	if len(warmLines) != 3 {
		t.Fatalf("warm failure lines = %q, expected one per server", warmLines)
	}
	for _, line := range log.lines() {
		if strings.Contains(line, personalId) || strings.Contains(line, "secret-name") {
			t.Fatalf("a log line exposed the url path or the name: %q", line)
		}
	}
	for _, line := range warmLines {
		if !strings.Contains(line, "https://127.0.0.1:") && !strings.Contains(line, "https://[::1]:") {
			t.Fatalf("warm failure line %q does not name the server", line)
		}
	}
}

// A sink answer (0.0.0.0, ::) from a server the user chose is the answer, cached
// as usual, but is never bound to an exit (DohResultCallback); a mixed answer
// and a real one are, as before (ADBLOCK3RD B4).
func TestDohSinkAnswersFireNoDohResultCallback(t *testing.T) {
	realAddr := netip.MustParseAddr("203.0.113.70")
	answers := map[string][]netip.Addr{
		"sink.example.":  {netip.IPv4Unspecified()},
		"sink6.example.": {netip.IPv6Unspecified()},
		"mixed.example.": {netip.IPv4Unspecified(), realAddr},
		"real.example.":  {realAddr},
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p dnsmessage.Parser
		if _, err := p.Start(testingDohRequestQuery(t, r)); err != nil {
			t.Error(err)
			return
		}
		question, err := p.Question()
		if err != nil {
			t.Error(err)
			return
		}
		writeDohWire(w, r, answers[question.Name.String()], 60, false)
	}))
	defer server.Close()
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())

	var bindLock sync.Mutex
	bound := map[string][]netip.Addr{}
	settings := DefaultDohSettings()
	settings.Log = NewNoopLogger()
	settings.RequestTimeout = 5 * time.Second
	settings.DnsResolverSettings = &DnsResolverSettings{
		EnableRemoteDoh:   true,
		RemoteDohUrlsIpv4: []string{server.URL},
		TlsConfig:         &tls.Config{RootCAs: pool},
	}
	// a caller dial context, so the cache captures the delivering route as
	// the tunnel's does
	settings.DialContextSettings = &DialContextSettings{DialContext: (&net.Dialer{}).DialContext}
	settings.DohResultCallback = func(domain string, addrs []netip.Addr, route *DohRoute) {
		bindLock.Lock()
		defer bindLock.Unlock()
		bound[domain] = addrs
	}
	cache := NewDohCache(settings)
	defer cache.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, c := range []struct {
		recordType string
		domain     string
		want       []netip.Addr
	}{
		{recordType: "A", domain: "sink.example", want: []netip.Addr{netip.IPv4Unspecified()}},
		{recordType: "AAAA", domain: "sink6.example", want: []netip.Addr{netip.IPv6Unspecified()}},
		{recordType: "A", domain: "mixed.example", want: []netip.Addr{netip.IPv4Unspecified(), realAddr}},
		{recordType: "A", domain: "real.example", want: []netip.Addr{realAddr}},
	} {
		addrs, authoritative := cache.QueryResult(ctx, c.recordType, c.domain)
		slices.SortFunc(addrs, netip.Addr.Compare)
		if !authoritative || !slices.Equal(addrs, c.want) {
			t.Fatalf("%s %s = %v (authoritative %t), expected %v", c.recordType, c.domain, addrs, authoritative, c.want)
		}
		if entry := testingDohCacheEntry(cache, c.recordType, c.domain); entry == nil {
			t.Fatalf("%s %s was not cached", c.recordType, c.domain)
		}
	}
	bindLock.Lock()
	defer bindLock.Unlock()
	if _, ok := bound["sink.example"]; ok {
		t.Fatal("a 0.0.0.0 sink answer was bound to an exit")
	}
	if _, ok := bound["sink6.example"]; ok {
		t.Fatal("a :: sink answer was bound to an exit")
	}
	if len(bound["mixed.example"]) != 2 || len(bound["real.example"]) != 1 {
		t.Fatalf("bound answers = %v, expected the mixed and the real answer", bound)
	}
}

// The field is a wire format of the hosted proxy api: it round trips, is left
// out when nil, and an older writer's json without it reads as nil.
func TestDohServerHostAddrsJson(t *testing.T) {
	settings := &DnsResolverSettings{
		EnableRemoteDoh:    true,
		RemoteDohUrlsIpv4:  []string{"https://dns.example/dns-query"},
		DohServerHostAddrs: map[string][]string{"dns.example": {"192.0.2.83", "2001:db8::83"}},
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"doh_server_host_addrs":{"dns.example":["192.0.2.83","2001:db8::83"]}`) {
		t.Fatalf("json = %s", encoded)
	}
	var decoded DnsResolverSettings
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(decoded.DohServerHostAddrs["dns.example"], settings.DohServerHostAddrs["dns.example"]) {
		t.Fatalf("decoded pins = %v", decoded.DohServerHostAddrs)
	}
	encoded, err = json.Marshal(DefaultDnsResolverSettings())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "doh_server_host_addrs") {
		t.Fatalf("nil pins were written: %s", encoded)
	}
	var older DnsResolverSettings
	if err := json.Unmarshal([]byte(`{"enable_remote_doh":true,"remote_doh_urls_ipv4":["https://192.0.2.84/dns-query"]}`), &older); err != nil {
		t.Fatal(err)
	}
	if older.DohServerHostAddrs != nil {
		t.Fatalf("older json read with pins %v", older.DohServerHostAddrs)
	}
}

// The copies of resolver settings that clone their lists clone the pins too;
// the built-in copy names no pins, which belong to custom servers.
func TestDohServerHostAddrsCopies(t *testing.T) {
	pins := map[string][]string{"dns.example": {"192.0.2.85"}}
	settings := DefaultDohSettings()
	settings.DnsResolverSettings.DohServerHostAddrs = pins

	internal := internalDohSettings(settings, []string{"api.example"})
	internal.DnsResolverSettings.DohServerHostAddrs["dns.example"][0] = "192.0.2.86"
	internal.DnsResolverSettings.DohServerHostAddrs["other.example"] = []string{"192.0.2.87"}
	if pins["dns.example"][0] != "192.0.2.85" || len(pins) != 1 {
		t.Fatalf("the internal copy shares the pins: %v", pins)
	}

	if builtIn := builtInDohServerSettings(settings); builtIn.DnsResolverSettings.DohServerHostAddrs != nil {
		t.Fatalf("the built-in copy kept pins %v", builtIn.DnsResolverSettings.DohServerHostAddrs)
	}
	if settings.DnsResolverSettings.DohServerHostAddrs == nil {
		t.Fatal("the built-in copy cleared the original's pins")
	}

	// the tun's copy for a v4-only stack
	tunSettings := DefaultTunSettings()
	tunSettings.DisableIpv6 = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resolver := &DnsResolverSettings{
		EnableRemoteDoh:    true,
		RemoteDohUrlsIpv4:  []string{"https://dns.example/dns-query"},
		RemoteDohUrlsIpv6:  []string{"https://dns.example/dns-query"},
		DohServerHostAddrs: pins,
	}
	tun, err := CreateTunWithResolver(ctx, tunSettings, resolver)
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()
	copied := tun.DohCache().settings.DnsResolverSettings
	if copied == resolver || !slices.Equal(copied.DohServerHostAddrs["dns.example"], pins["dns.example"]) {
		t.Fatalf("the v4-only tun copy = %v", copied.DohServerHostAddrs)
	}
	copied.DohServerHostAddrs["dns.example"][0] = "192.0.2.88"
	if pins["dns.example"][0] != "192.0.2.85" {
		t.Fatal("the v4-only tun copy shares the pins")
	}
	if len(copied.RemoteDohUrlsIpv6) != 0 || !slices.Equal(copied.RemoteDohUrlsIpv4, resolver.RemoteDohUrlsIpv4) {
		t.Fatalf("the v4-only tun copy lists = %v %v", copied.RemoteDohUrlsIpv4, copied.RemoteDohUrlsIpv6)
	}
}
