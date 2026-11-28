package connect

// Lead server tests (`DohSettings.NamedRemoteDohUrls`,
// `DohSettings.DohServerLeadReserve`): on a network that black-holes the
// default DoH servers, a control-name lookup answers through a named bootstrap
// server, or through a regional preset, whatever order the weighted draw gives
// the servers and whatever requests to the defaults other lookups hold. The
// internal resolver has four request slots (net_http_internal_doh.go), and a
// request to a black-holed server holds its slot until the query deadline.
//
// Each test runs in a synctest bubble on an in-memory network. The bubble's
// clock moves only when every goroutine in it waits, so a lookup starved of a
// slot fails at its budget at once, and a lookup that answers does so before
// any time has passed. No test opens a socket or sleeps. The budget ends before
// the DoH request timeout, so a starved lookup ends there rather than falling
// back to the host's plain resolvers (net_http_doh_bootstrap.go), which would
// leave the bubble and the process.

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// A network that black-holes every DoH server but the ones it serves. A served
// server answers in memory, over tls on a pipe; a dial to any other address
// hangs until its context ends, which is what a censor that drops the default
// servers' packets does. Built and used inside one synctest bubble.
type dohLeadTestNetwork struct {
	servedAddresses map[string]bool
	serverTlsConfig *tls.Config
	clientTlsConfig *tls.Config
	handler         func(w http.ResponseWriter, r *http.Request)
	queryCount      atomic.Int32
	blackholedCount atomic.Int32
}

// Serves the DoH servers of dohUrls, https urls of ip literals on the default
// port, each answering with handler. The certificate is valid at the bubble's
// time, so the network is built inside the bubble.
func newDohLeadTestNetwork(t *testing.T, dohUrls []string, handler func(w http.ResponseWriter, r *http.Request)) *dohLeadTestNetwork {
	t.Helper()
	servedAddresses := map[string]bool{}
	hosts := []string{}
	for _, dohUrl := range dohUrls {
		_, addr, err := ParseControlDohUrl(dohUrl)
		if err != nil {
			t.Fatal(err)
		}
		servedAddresses[net.JoinHostPort(addr.String(), "443")] = true
		hosts = append(hosts, addr.String())
	}
	certPem, keyPem, err := selfSign(hosts, "lead server test", time.Hour, 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair(certPem, keyPem)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certPem) {
		t.Fatal("could not trust the test servers' certificate")
	}
	return &dohLeadTestNetwork{
		servedAddresses: servedAddresses,
		serverTlsConfig: &tls.Config{
			Certificates: []tls.Certificate{certificate},
			NextProtos:   []string{"http/1.1"},
			// no session tickets: on a pipe the client must read the server's
			// first flight to its end before it writes, and tickets would
			// follow the server's finished message
			SessionTicketsDisabled: true,
		},
		clientTlsConfig: &tls.Config{RootCAs: roots},
		handler:         handler,
	}
}

// The network's dial: a pipe to a served server, else a black hole.
func (self *dohLeadTestNetwork) dialContext(ctx context.Context, network string, address string) (net.Conn, error) {
	if !self.servedAddresses[address] {
		self.blackholedCount.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	conn, serverConn := net.Pipe()
	go self.serve(serverConn)
	return conn, nil
}

// Answers the DoH requests of one connection until the client closes it. A
// pipe's write waits for the peer to read, where a socket's would land in a
// buffer, so the connection is read while a response is written: a client
// that cancels a request in the middle of its response writes a tls close
// notification then, and with both ends writing neither would return.
func (self *dohLeadTestNetwork) serve(conn net.Conn) {
	defer conn.Close()
	tlsConn := tls.Server(conn, self.serverTlsConfig)
	if err := tlsConn.Handshake(); err != nil {
		return
	}
	requests := make(chan *http.Request)
	done := make(chan struct{})
	defer close(done)
	go func() {
		defer close(requests)
		reader := bufio.NewReader(tlsConn)
		for {
			request, err := http.ReadRequest(reader)
			if err != nil {
				return
			}
			select {
			case requests <- request:
			case <-done:
				return
			}
		}
	}()
	for request := range requests {
		self.queryCount.Add(1)
		recorder := httptest.NewRecorder()
		self.handler(recorder, request)
		response := recorder.Result()
		response.ContentLength = int64(recorder.Body.Len())
		if err := response.Write(tlsConn); err != nil {
			return
		}
	}
}

// A client strategy whose names under space.example resolve only over DoH, by
// dohSettings, on the test network. The caller closes it.
func newDohLeadTestStrategy(t *testing.T, network *dohLeadTestNetwork, dohSettings *DohSettings) *ClientStrategy {
	t.Helper()
	dohSettings.Log = NewNoopLogger()
	dohSettings.DnsResolverSettings.TlsConfig = network.clientTlsConfig
	dohSettings.DialContextSettings = &DialContextSettings{DialContext: network.dialContext}
	settings := DefaultClientStrategySettings()
	settings.Log = NewNoopLogger()
	settings.InternalDohDomains = []string{"space.example"}
	settings.DohSettings = dohSettings
	return NewClientStrategy(t.Context(), settings)
}

// The budget of a test lookup: its context, which ends a second into the
// lookup, before the DoH request timeout of the strategy's cache.
func newDohLeadTestLookupContext(t *testing.T, cache *DohCache) (context.Context, context.CancelFunc) {
	t.Helper()
	lookupBudget := time.Second
	if cache.settings.RequestTimeout <= lookupBudget {
		t.Fatalf("the DoH request timeout %s is within the lookup budget %s", cache.settings.RequestTimeout, lookupBudget)
	}
	return context.WithTimeout(t.Context(), lookupBudget)
}

// Forces the weighted order of a cache's queries: the servers for which last
// is true draw the smallest key there is and every other server draws one
// half. At the scores a fresh cache starts from, the seed of a named server
// included, every other server then comes first, which is checked here.
func forceDohLeadTestDraw(t *testing.T, cache *DohCache, last func(dohUrl string) bool) {
	t.Helper()
	stats := cache.remoteClient.stats
	func() {
		stats.lock.Lock()
		defer stats.lock.Unlock()
		stats.draw = func(dohUrl string) float64 {
			if last(dohUrl) {
				return math.SmallestNonzeroFloat64
			}
			return 0.5
		}
	}()
	ordered := stats.order(remoteDohUrls(cache.settings, cache.settings.IpVersion))
	lastCount := 0
	for i, dohUrl := range ordered {
		if last(dohUrl) {
			lastCount += 1
		} else if 0 < lastCount {
			t.Fatalf("the forced draw put a default at %d of %d, behind a server meant to come last", i, len(ordered))
		}
	}
	if lastCount == 0 || lastCount == len(ordered) {
		t.Fatalf("the forced draw ordered %d of %d servers last, expected the defaults ahead of the rest", lastCount, len(ordered))
	}
}

// A control-name lookup on a network that black-holes the defaults, under a
// draw that orders every default ahead of the served servers, answers through
// a served server before any time has passed, and the defaults are never given
// every request slot.
func testDohLeadServersLaunchFirst(t *testing.T, ingressCountryCode string, namedDohUrls []string, servedDohUrls []string) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		answer := netip.MustParseAddr("192.0.2.55")
		network := newDohLeadTestNetwork(t, servedDohUrls, func(w http.ResponseWriter, r *http.Request) {
			writeDohWire(w, r, []netip.Addr{answer}, 60, false)
		})
		strategy := newDohLeadTestStrategy(t, network, IngressDohSettings(ingressCountryCode, namedDohUrls, nil))
		defer strategy.Close()
		cache := strategy.internalDohResolver.getCache()
		slotCount := cap(cache.remoteClient.httpSem)
		forceDohLeadTestDraw(t, cache, func(dohUrl string) bool {
			return slices.Contains(servedDohUrls, dohUrl)
		})

		ctx, cancel := newDohLeadTestLookupContext(t, cache)
		defer cancel()
		started := time.Now()
		addrs, err := strategy.internalDohResolver.resolve(ctx, "tcp4", "api.space.example")
		elapsed := time.Since(started)
		if err != nil || !slices.Equal(addrs, []netip.Addr{answer}) {
			t.Fatalf("api.space.example = %v (%v) after %s with %d queries answered, expected %s from a lead server at once: %d black-holed defaults dialed for the %d request slots", addrs, err, elapsed, network.queryCount.Load(), answer, network.blackholedCount.Load(), slotCount)
		}
		if elapsed != 0 {
			t.Fatalf("a lead server answered after %s, expected at once", elapsed)
		}
		if blackholedCount := network.blackholedCount.Load(); slotCount <= int(blackholedCount) {
			t.Fatalf("%d black-holed defaults were dialed, expected fewer than the %d request slots", blackholedCount, slotCount)
		}
	})
}

// The field case (P216, and the flake of
// `TestInternalDohResolvesThroughControlDohWhenTheDefaultsAreUnreachable`): a
// user's named bootstrap server, on a network that black-holes the defaults,
// drawn behind them. Before the lead servers it waited for one of the four
// request slots, all held by black-holed defaults, until the lookup failed at
// its deadline (DNS defaults v3 follow-up, 2026-11-09).
func TestInternalDohNamedServerLeadsWhenTheDrawPutsTheDefaultsFirst(t *testing.T) {
	namedDohUrl := "https://192.0.2.53/dns-query"
	testDohLeadServersLaunchFirst(t, "", []string{namedDohUrl}, []string{namedDohUrl})
}

// The regional presets of an ingress country (cn, support inbox 7956, 7957,
// 7973) lead as a named server does. They carry no seed, so before the lead
// servers a cold lookup drew two default pairs ahead of every preset about
// half the time, and on a network that black-holes the defaults that lookup
// failed at its deadline.
func TestInternalDohRegionalPresetsLeadWhenTheDrawPutsTheDefaultsFirst(t *testing.T) {
	presetsIpv4, presetsIpv6 := RegionalControlDohUrls("cn")
	presets := slices.Concat(presetsIpv4, presetsIpv6)
	if len(presets) == 0 {
		t.Fatal("no cn presets")
	}
	testDohLeadServersLaunchFirst(t, "cn", nil, presets)
}

// The named server answers an AAAA question empty, which leaves that lookup
// collecting from the defaults until its deadline in case one of them resolves
// the name, and defaults that a network black-holes never answer. Another
// control-name lookup must still answer through the named server at once: the
// defaults leave the lead servers a request slot. Before the reserve they took
// all four, and every other control-name lookup failed at its deadline. Unlike
// the tests above this needs no forced draw: whatever the draw, the defaults
// end up holding every slot they may.
func TestInternalDohNamedServerKeepsASlotWhileDefaultsHoldTheRest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		namedDohUrl := "https://192.0.2.53/dns-query"
		answer := netip.MustParseAddr("192.0.2.56")
		// a v4 answer only: an AAAA question gets no record, NODATA
		network := newDohLeadTestNetwork(t, []string{namedDohUrl}, func(w http.ResponseWriter, r *http.Request) {
			writeDohWire(w, r, []netip.Addr{answer}, 60, false)
		})
		strategy := newDohLeadTestStrategy(t, network, ControlDohSettings([]string{namedDohUrl}, nil))
		defer strategy.Close()
		cache := strategy.internalDohResolver.getCache()
		slotCount := cap(cache.remoteClient.httpSem)

		collectCtx, collectCancel := newDohLeadTestLookupContext(t, cache)
		collected := make(chan struct{})
		defer func() {
			collectCancel()
			<-collected
		}()
		go func() {
			defer close(collected)
			strategy.internalDohResolver.resolve(collectCtx, "tcp6", "api.space.example")
		}()
		// the collecting lookup has launched everything it can
		synctest.Wait()
		heldCount := len(cache.remoteClient.httpSem)
		blackholedCount := network.blackholedCount.Load()

		ctx, cancel := newDohLeadTestLookupContext(t, cache)
		defer cancel()
		started := time.Now()
		addrs, err := strategy.internalDohResolver.resolve(ctx, "tcp4", "connect.space.example")
		elapsed := time.Since(started)
		if err != nil || !slices.Equal(addrs, []netip.Addr{answer}) || elapsed != 0 {
			t.Fatalf("connect.space.example = %v (%v) after %s, expected %s from the named server at once: the collecting lookup held %d of the %d request slots with %d black-holed defaults", addrs, err, elapsed, answer, heldCount, slotCount, blackholedCount)
		}
		if slotCount <= heldCount {
			t.Fatalf("the collecting lookup held all %d request slots", heldCount)
		}
		if blackholedCount == 0 {
			t.Fatal("the collecting lookup dialed no default, so nothing held a slot")
		}
	})
}
