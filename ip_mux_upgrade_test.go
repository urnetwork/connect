package connect

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// TestUpgradeMuxPassthrough verifies the step-2 behavior: with no DNS/HTTP claiming
// implemented, the UpgradeMux is transparent. The thorough DNS and HTTP upgrade
// tests (driving a Go net.Resolver and http.Client through the mux against a local
// HTTPS server for DoH/HTTPS) are added with steps 3 and 4.
func TestUpgradeMuxPassthrough(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec := &ipMuxRecorder{}
	mux, err := NewUpgradeMux(ctx, TransferPath{}, protocol.ProvideMode_Network, 0, rec.receive, DefaultUpgradeMuxSettings(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mux.Close()
	mux.SetUpstream(rec.upstream)

	if !mux.SendPacket(TransferPath{}, protocol.ProvideMode_Network, []byte("x"), 0) {
		t.Fatal("SendPacket returned false")
	}
	external := &IpPath{Version: 4, Protocol: IpProtocolTcp, DestinationIp: net.ParseIP("1.2.3.4"), DestinationPort: 80}
	mux.Receive(TransferPath{}, protocol.ProvideMode_Network, external, []byte("y"))

	sent, received := rec.counts()
	if sent != 1 || received != 1 {
		t.Fatalf("pass-through mismatch: sent=%d received=%d, want 1/1", sent, received)
	}
}

// dnsClientHarness wires a client-side gVisor Tun to an UpgradeMux: packets the
// client emits are pumped into the mux's send path, and packets the mux delivers
// downstream are written back into the client Tun. A Go net.Resolver / http.Client
// driven over clientTun.DialContext then exercises the mux end-to-end, exactly as
// the OS TUN would in production.
type dnsClientHarness struct {
	clientTun *Tun
	mux       *UpgradeMux
}

// createPrivateClientTun makes a client-side Tun on its own isolated stack, so multiple
// tests' client tuns do not accumulate default routes on the process-wide shared stack
// (which makes the suite hang). The mux already runs on a private stack.
func createPrivateClientTun(ctx context.Context) (*Tun, error) {
	settings := DefaultTunSettings()
	settings.PrivateStack = true
	// a single deterministic dial — the default DialRace fans each client dial into
	// several racing SYNs, multiplying DNAT'd connections and adding cross-test timing
	// nondeterminism that can hang the suite
	settings.DialRace = 1
	return CreateTun(ctx, settings)
}

func newDnsClientHarness(t *testing.T, ctx context.Context, dns *DnsResolverSettings) *dnsClientHarness {
	t.Helper()

	clientTun, err := createPrivateClientTun(ctx)
	if err != nil {
		t.Fatal(err)
	}

	settings := DefaultUpgradeMuxSettings()
	settings.Dns.Resolver = dns
	// short resolve budget so failure tests (which now retry within the budget, and which the
	// OS resolver then re-queries several times) stay fast; success resolves on attempt 1
	settings.Dns.ResolveTimeout = 200 * time.Millisecond

	writeToClient := func(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {
		clientTun.Write(packet)
	}
	mux, err := NewUpgradeMux(ctx, TransferPath{}, protocol.ProvideMode_Network, 0, writeToClient, settings, nil)
	if err != nil {
		clientTun.Close()
		t.Fatal(err)
	}
	// all traffic in these tests is claimed (UDP/53); the upstream is unused
	mux.SetUpstream(func(source TransferPath, provideMode protocol.ProvideMode, packet []byte, timeout time.Duration) bool {
		return true
	})

	// pump client-emitted packets into the mux
	go func() {
		for {
			packet, err := clientTun.Read()
			if err != nil {
				return
			}
			mux.SendPacket(TransferPath{}, protocol.ProvideMode_Network, packet, 0)
		}
	}()

	return &dnsClientHarness{clientTun: clientTun, mux: mux}
}

func (self *dnsClientHarness) close() {
	self.mux.Close()
	self.clientTun.Close()
}

// resolver returns a Go resolver whose queries are routed through the client Tun to
// the mux. The dialed address is irrelevant — the mux intercepts by port (UDP/53).
func (self *dnsClientHarness) resolver() *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			return self.clientTun.DialContext(ctx, "udp", "10.0.0.1:53")
		},
	}
}

// TestUpgradeMuxDnsDoh drives a Go net.Resolver through the mux, which resolves the
// query over local DoH against a local HTTPS (httptest TLS) server, and verifies both
// the resolution and the IP→hostname reverse index (point 4).
func TestUpgradeMuxDnsDoh(t *testing.T) {
	const resolved = "203.0.113.45"
	const queryName = "host.example.test"

	// local DoH server (Google/Cloudflare JSON API) over TLS
	var dohRequests int
	dohServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dohRequests++
		resp := DohResponse{Status: 0}
		// answer A queries; AAAA returns an empty answer so LookupHost resolves to the A record
		if qtype := r.URL.Query().Get("type"); qtype == "A" || qtype == "1" {
			resp.Answer = []DohAnswer{{
				Name: r.URL.Query().Get("name"),
				Type: 1,
				TTL:  60,
				Data: resolved,
			}}
		}
		w.Header().Set("Content-Type", "application/dns-json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer dohServer.Close()

	pool := x509.NewCertPool()
	pool.AddCert(dohServer.Certificate())

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	h := newDnsClientHarness(t, ctx, &DnsResolverSettings{
		EnableLocalDoh:   true,
		LocalDohUrlsIpv4: []string{dohServer.URL},
		TlsConfig:        &tls.Config{RootCAs: pool},
	})
	defer h.close()

	addrs, err := h.resolver().LookupHost(ctx, queryName)
	if err != nil {
		t.Fatalf("LookupHost: %v", err)
	}
	if !slices.Contains(addrs, resolved) {
		t.Fatalf("LookupHost = %v, want to contain %s", addrs, resolved)
	}
	if dohRequests == 0 {
		t.Fatal("mux did not resolve via the local DoH server")
	}

	// reverse index (point 4): the mux records the hostname it served for the IP
	names := h.mux.ServerNames(resolved)
	if !slices.Contains(names, queryName) {
		t.Fatalf("ServerNames(%s) = %v, want to contain %s", resolved, names, queryName)
	}
}

// TestUpgradeMuxDnsServfail: a resolution failure (the resolver errors rather than returning
// an authoritative no-record answer) is surfaced to the client as SERVFAIL — a temporary error
// the client retries — not NOERROR with no answers, which a client treats as an authoritative
// "no address" and gives up on (caching the negative).
func TestUpgradeMuxDnsServfail(t *testing.T) {
	const queryName = "fail.example.test"

	// a DoH server that always fails: empty (no records) but not an authoritative no-record
	dohServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer dohServer.Close()

	pool := x509.NewCertPool()
	pool.AddCert(dohServer.Certificate())

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	h := newDnsClientHarness(t, ctx, &DnsResolverSettings{
		EnableLocalDoh:   true,
		LocalDohUrlsIpv4: []string{dohServer.URL},
		TlsConfig:        &tls.Config{RootCAs: pool},
	})
	defer h.close()

	_, err := h.resolver().LookupHost(ctx, queryName)
	if err == nil {
		t.Fatal("want an error when the resolver fails")
	}
	// the failure must not look authoritative — otherwise the client gives up instead of retrying
	if dnsErr, ok := err.(*net.DNSError); ok && dnsErr.IsNotFound {
		t.Fatalf("resolver failure surfaced as authoritative not-found (%v); client would give up", err)
	}
}

// newDohJsonServer starts a local TLS DoH server (JSON API) that answers A queries with
// a fixed IP, and returns resolver settings wired to trust and use it (local DoH).
func newDohJsonServer(t *testing.T, resolvedIp string) (*httptest.Server, *DnsResolverSettings) {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := DohResponse{Status: 0}
		if qtype := r.URL.Query().Get("type"); qtype == "A" || qtype == "1" {
			resp.Answer = []DohAnswer{{
				Name: r.URL.Query().Get("name"),
				Type: 1,
				TTL:  60,
				Data: resolvedIp,
			}}
		}
		w.Header().Set("Content-Type", "application/dns-json")
		json.NewEncoder(w).Encode(resp)
	}))
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	dns := &DnsResolverSettings{
		EnableLocalDoh:   true,
		LocalDohUrlsIpv4: []string{server.URL},
		TlsConfig:        &tls.Config{RootCAs: pool},
	}
	return server, dns
}

// TestUpgradeMuxDnsRebuild verifies SetSettings rebuilds the DohCache at runtime: after
// switching to a different DoH server, resolution uses the new server.
func TestUpgradeMuxDnsRebuild(t *testing.T) {
	const ipA = "203.0.113.10"
	const ipB = "203.0.113.20"

	serverA, dnsA := newDohJsonServer(t, ipA)
	defer serverA.Close()
	serverB, dnsB := newDohJsonServer(t, ipB)
	defer serverB.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	h := newDnsClientHarness(t, ctx, dnsA)
	defer h.close()

	addrs, err := h.resolver().LookupHost(ctx, "a.example.test")
	if err != nil || !slices.Contains(addrs, ipA) {
		t.Fatalf("before rebuild: LookupHost = %v (err %v), want %s", addrs, err, ipA)
	}

	// switch the DNS resolution path at runtime
	h.mux.SetSettings(&UpgradeMuxSettings{
		Dns:  &DnsUpgradeSettings{Resolver: dnsB},
		Http: &HttpUpgradeSettings{Mode: HttpUpgradeUnencrypted},
	})

	addrs2, err := h.resolver().LookupHost(ctx, "b.example.test")
	if err != nil || !slices.Contains(addrs2, ipB) {
		t.Fatalf("after rebuild: LookupHost = %v (err %v), want %s", addrs2, err, ipB)
	}
}

// TestUpgradeMuxHttpModes verifies the no-termination HTTP modes: Unencrypted passes
// a TCP/80 packet through to the upstream, Block claims and drops it.
func TestUpgradeMuxHttpModes(t *testing.T) {
	craft := func() []byte {
		ipPath := &IpPath{
			Version:         4,
			Protocol:        IpProtocolTcp,
			SourceIp:        net.ParseIP("169.254.9.9"),
			SourcePort:      12345,
			DestinationIp:   net.ParseIP("93.184.216.34"),
			DestinationPort: 80,
		}
		return ipOosPacket(ipPath, nil)
	}

	newMux := func(t *testing.T, ctx context.Context, rec *ipMuxRecorder, mode HttpUpgradeMode) *UpgradeMux {
		settings := DefaultUpgradeMuxSettings()
		settings.Http = &HttpUpgradeSettings{Mode: mode}
		mux, err := NewUpgradeMux(ctx, TransferPath{}, protocol.ProvideMode_Network, 0, rec.receive, settings, nil)
		if err != nil {
			t.Fatal(err)
		}
		mux.SetUpstream(rec.upstream)
		return mux
	}

	cases := []struct {
		name     string
		mode     HttpUpgradeMode
		wantSent int
	}{
		{name: "unencrypted-passthrough", mode: HttpUpgradeUnencrypted, wantSent: 1},
		{name: "block-drops", mode: HttpUpgradeBlock, wantSent: 0},
	}
	for _, c := range cases {
		ctx, cancel := context.WithCancel(context.Background())
		rec := &ipMuxRecorder{}
		mux := newMux(t, ctx, rec, c.mode)

		if !mux.SendPacket(TransferPath{}, protocol.ProvideMode_Network, craft(), 0) {
			t.Errorf("%s: SendPacket returned false", c.name)
		}
		if sent, _ := rec.counts(); sent != c.wantSent {
			t.Errorf("%s: upstream sent=%d, want %d", c.name, sent, c.wantSent)
		}

		mux.Close()
		cancel()
	}
}

// httpUpgradeGet wires a client gVisor Tun to a mux (with the given settings), routes
// the mux's own upgrade connections to dialUpstream (the test "exit"), drives a Go
// http.Client over plaintext :80 through it, and returns the response. The Host
// (example.com) matches the httptest cert; the dialed IP is arbitrary (the mux DNATs
// by port and the upgrade dialer ignores the address).
func httpUpgradeGet(t *testing.T, settings *UpgradeMuxSettings, dialUpstream func(ctx context.Context, network, address string) (net.Conn, error)) (*http.Response, []byte) {
	t.Helper()

	// 30s to accommodate the bounded GET retries below.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)

	clientTun, err := createPrivateClientTun(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	writeToClient := func(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {
		clientTun.Write(packet)
	}
	mux, err := NewUpgradeMux(ctx, TransferPath{}, protocol.ProvideMode_Network, 0, writeToClient, settings, nil)
	if err != nil {
		clientTun.Close()
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		mux.Close()
		clientTun.Close()
		cancel()
	})
	mux.SetUpstream(func(source TransferPath, provideMode protocol.ProvideMode, packet []byte, timeout time.Duration) bool {
		return true // unused; client TCP/80 is claimed and the upgrade dials the exit directly
	})
	mux.dialUpstream = dialUpstream

	go func() {
		for {
			packet, err := clientTun.Read()
			if err != nil {
				return
			}
			mux.SendPacket(TransferPath{}, protocol.ProvideMode_Network, packet, 0)
		}
	}()

	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return clientTun.DialContext(ctx, "tcp", "10.9.9.9:80")
			},
			DisableKeepAlives: true,
		},
	}
	// the test bridges two gVisor stacks (clientTun and the mux) over Go pumps; that handshake
	// path is intermittently flaky in a way the single-stack production path is not (see
	// docs/IP_MUX_UPGRADE.md), and where a real client would simply retry. bound each attempt
	// and retry on a fresh connection (keep-alives are disabled, so each Do dials anew).
	const maxAttempts = 6
	const attemptTimeout = 4 * time.Second
	var resp *http.Response
	var body []byte
	var lastErr error
	for range maxAttempts {
		reqCtx, reqCancel := context.WithTimeout(ctx, attemptTimeout)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "http://example.com/", nil)
		if err != nil {
			reqCancel()
			t.Fatal(err)
		}
		r, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
			reqCancel()
			continue
		}
		b, err := io.ReadAll(r.Body)
		r.Body.Close()
		reqCancel()
		if err != nil {
			lastErr = err
			continue
		}
		resp, body = r, b
		break
	}
	if resp == nil {
		t.Fatalf("GET failed after %d attempts: %v", maxAttempts, lastErr)
	}
	return resp, body
}

// TestUpgradeMuxHttpsUpgrade drives a Go http.Client through the mux over plaintext
// :80; the mux DNATs and terminates the connection, re-issues the request to the
// origin over HTTPS (verified against a local TLS server), and relays the response
// back — exercising DNAT, termination, the HTTPS round-trip, and the pump un-NAT.
func TestUpgradeMuxHttpsUpgrade(t *testing.T) {
	const body = "upgraded-ok"

	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil {
			t.Errorf("origin received a non-TLS request")
		}
		w.Header().Set("X-Upgraded", "yes")
		io.WriteString(w, body)
	}))
	defer origin.Close()

	pool := x509.NewCertPool()
	pool.AddCert(origin.Certificate())

	settings := DefaultUpgradeMuxSettings()
	settings.Http = &HttpUpgradeSettings{
		Mode:      HttpUpgradeHttps,
		TlsConfig: &tls.Config{RootCAs: pool},
	}
	resp, got := httpUpgradeGet(t, settings, func(ctx context.Context, network, address string) (net.Conn, error) {
		return net.Dial("tcp", origin.Listener.Addr().String())
	})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	if string(got) != body {
		t.Fatalf("body=%q, want %q", got, body)
	}
	if resp.Header.Get("X-Upgraded") != "yes" {
		t.Fatalf("missing upgrade header (response not from the HTTPS origin): %v", resp.Header)
	}
}

// TestUpgradeMuxHttpsFallback verifies the HttpsFallback mode: when the HTTPS handshake
// to the origin fails (here, a plaintext origin), the mux falls back to plaintext HTTP.
func TestUpgradeMuxHttpsFallback(t *testing.T) {
	const body = "plain-fallback-ok"

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil {
			t.Errorf("origin received TLS but should be plaintext")
		}
		io.WriteString(w, body)
	}))
	defer origin.Close()

	settings := DefaultUpgradeMuxSettings()
	settings.Http = &HttpUpgradeSettings{Mode: HttpUpgradeHttpsFallback}
	resp, got := httpUpgradeGet(t, settings, func(ctx context.Context, network, address string) (net.Conn, error) {
		return net.Dial("tcp", origin.Listener.Addr().String())
	})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	if string(got) != body {
		t.Fatalf("body=%q, want %q (fallback to plaintext HTTP)", got, body)
	}
}

// deadTcpAddr returns a TCP address with no listener — bound then immediately closed — so
// dialing it is refused. Models a server with no HTTPS listener on :443.
func deadTcpAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// TestUpgradeMuxHttpsFallbackNoListener verifies that in HttpsFallback mode, when there is
// no listener on :443 (the HTTPS dial is refused — not merely a TLS-handshake failure), the
// request falls back to plaintext HTTP rather than returning a 502. This is the common case
// for a server with no HTTPS on the original IP (the reported Pandora behavior).
func TestUpgradeMuxHttpsFallbackNoListener(t *testing.T) {
	const body = "plain-fallback-ok"

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	}))
	defer origin.Close()
	noListener443 := deadTcpAddr(t)

	settings := DefaultUpgradeMuxSettings()
	settings.Http = &HttpUpgradeSettings{
		Mode:           HttpUpgradeHttpsFallback,
		UpgradeTimeout: 2 * time.Second,
	}
	resp, got := httpUpgradeGet(t, settings, func(ctx context.Context, network, address string) (net.Conn, error) {
		// :443 has no listener (refused); the plaintext origin answers the :80 fallback
		if _, port, _ := net.SplitHostPort(address); port == "443" {
			return net.Dial("tcp", noListener443)
		}
		return net.Dial("tcp", origin.Listener.Addr().String())
	})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	if string(got) != body {
		t.Fatalf("body=%q, want %q (no-listener-on-443 fallback to plaintext)", got, body)
	}
}

// TestUpgradeMuxHttpsFallbackOnErrorStatus: a host that connects over HTTPS but answers a 4xx
// (content served only over plaintext, e.g. an HTTP-only CDN with scheme-scoped signed URLs)
// falls back to http for an idempotent GET and returns the plaintext body — what broke Pandora.
func TestUpgradeMuxHttpsFallbackOnErrorStatus(t *testing.T) {
	const body = "plain-only-content"

	// :443 connects (valid cert for the SNI) but 403s the upgraded request
	httpsHit := make(chan struct{}, 1)
	httpsOrigin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case httpsHit <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer httpsOrigin.Close()
	pool := x509.NewCertPool()
	pool.AddCert(httpsOrigin.Certificate())

	// :80 serves the real content
	httpOrigin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	}))
	defer httpOrigin.Close()

	settings := DefaultUpgradeMuxSettings()
	settings.Http = &HttpUpgradeSettings{
		Mode:           HttpUpgradeHttpsFallback,
		UpgradeTimeout: 2 * time.Second,
		TlsConfig:      &tls.Config{RootCAs: pool},
	}
	resp, got := httpUpgradeGet(t, settings, func(ctx context.Context, network, address string) (net.Conn, error) {
		if _, port, _ := net.SplitHostPort(address); port == "443" {
			return net.Dial("tcp", httpsOrigin.Listener.Addr().String())
		}
		return net.Dial("tcp", httpOrigin.Listener.Addr().String())
	})

	// confirm the upgrade connected over HTTPS and got the 4xx (rather than failing the
	// handshake) — i.e. this exercised the error-status fallback, not the dial/handshake one
	select {
	case <-httpsHit:
	default:
		t.Fatal("HTTPS origin was not reached; test did not exercise the error-status fallback")
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200 (4xx over HTTPS should fall back to plaintext)", resp.StatusCode)
	}
	if string(got) != body {
		t.Fatalf("body=%q, want %q", got, body)
	}
}

// TestUpgradeMuxHttpsStrictNoListener verifies that in strict Https mode (no fallback), no
// listener on :443 yields a 502 — the request is not silently served in the clear.
func TestUpgradeMuxHttpsStrictNoListener(t *testing.T) {
	noListener443 := deadTcpAddr(t)

	settings := DefaultUpgradeMuxSettings()
	settings.Http = &HttpUpgradeSettings{
		Mode:           HttpUpgradeHttps,
		UpgradeTimeout: 2 * time.Second,
	}
	resp, _ := httpUpgradeGet(t, settings, func(ctx context.Context, network, address string) (net.Conn, error) {
		return net.Dial("tcp", noListener443)
	})

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502 (strict HTTPS, no listener, no fallback)", resp.StatusCode)
	}
}

// TestUpgradeMuxUpgradeFailureHoldOff verifies the per-server-name upgrade-failure cache:
// a record is honored within the hold-off, ignored when disabled, and cleared once older
// than the hold-off TTL (on check, and by the active-sweep prune used by
// sweepUpgradeFailuresLoop).
func TestUpgradeMuxUpgradeFailureHoldOff(t *testing.T) {
	mux := &UpgradeMux{httpsUpgradeFail: map[string]time.Time{}}
	const holdOff = time.Minute

	if mux.recentlyFailedUpgrade("a.com", holdOff) {
		t.Fatal("a.com should not be held off before any failure")
	}
	mux.recordUpgradeFailure("a.com", holdOff)
	if !mux.recentlyFailedUpgrade("a.com", holdOff) {
		t.Fatal("a.com should be held off after a failure")
	}
	if mux.recentlyFailedUpgrade("b.com", holdOff) {
		t.Fatal("an unrelated name should not be held off")
	}
	// holdOff <= 0 disables both the check and the record
	if mux.recentlyFailedUpgrade("a.com", 0) {
		t.Fatal("holdOff=0 should disable the hold-off")
	}
	mux.recordUpgradeFailure("c.com", 0)
	if _, ok := mux.httpsUpgradeFail["c.com"]; ok {
		t.Fatal("recordUpgradeFailure with holdOff=0 should not record")
	}

	// an entry older than the TTL is expired: dropped on check ...
	mux.httpsUpgradeFail["old.com"] = time.Now().Add(-2 * holdOff)
	if mux.recentlyFailedUpgrade("old.com", holdOff) {
		t.Fatal("an expired entry should not be held off")
	}
	if _, ok := mux.httpsUpgradeFail["old.com"]; ok {
		t.Fatal("recentlyFailedUpgrade should drop the expired entry it checked")
	}
	// ... and dropped by the sweep prune for names that are never re-checked
	mux.httpsUpgradeFail["stale.com"] = time.Now().Add(-2 * holdOff)
	mux.pruneUpgradeFailures(holdOff)
	if _, ok := mux.httpsUpgradeFail["stale.com"]; ok {
		t.Fatal("prune should clear the expired entry")
	}
	if _, ok := mux.httpsUpgradeFail["a.com"]; !ok {
		t.Fatal("prune should keep the live entry")
	}
	// prune with holdOff <= 0 (hold-off disabled) clears everything
	mux.pruneUpgradeFailures(0)
	if 0 < len(mux.httpsUpgradeFail) {
		t.Fatalf("prune(0) should clear all records, have %d", len(mux.httpsUpgradeFail))
	}
}

// TestPeekClaim verifies the cheap, alloc-free claim peek (onSend's fast path): it agrees
// with the full-parse decision for IPv4/IPv6 TCP/UDP, and defers (decided=false) to the full
// parse for IPv6 extension headers and short/unsupported headers.
func TestPeekClaim(t *testing.T) {
	mkv4 := func(proto byte, dport int) []byte {
		p := make([]byte, 28) // 20-byte IPv4 header + 8 bytes of L4 (ports)
		p[0] = 0x45           // ipv4, ihl=5
		p[9] = proto
		p[22] = byte(dport >> 8)
		p[23] = byte(dport)
		return p
	}
	mkv6 := func(nextHdr byte, dport int) []byte {
		p := make([]byte, 48) // 40-byte IPv6 header + 8 bytes of L4
		p[0] = 0x60           // ipv6
		p[6] = nextHdr
		p[42] = byte(dport >> 8)
		p[43] = byte(dport)
		return p
	}
	const tcp, udp, icmp, hopopt byte = 6, 17, 1, 0
	cases := []struct {
		name    string
		packet  []byte
		claim   bool
		decided bool
	}{
		{"v4 tcp 80", mkv4(tcp, 80), true, true},
		{"v4 tcp 443", mkv4(tcp, 443), false, true},
		{"v4 udp 53", mkv4(udp, 53), true, true},
		{"v4 udp 4500", mkv4(udp, 4500), false, true},
		{"v4 icmp", mkv4(icmp, 0), false, true},
		{"v6 tcp 80", mkv6(tcp, 80), true, true},
		{"v6 tcp 443", mkv6(tcp, 443), false, true},
		{"v6 udp 53", mkv6(udp, 53), true, true},
		{"v6 extension header", mkv6(hopopt, 80), false, false},
		{"short", []byte{0x45, 0x00}, false, false},
		{"empty", nil, false, false},
	}
	for _, c := range cases {
		claim, decided := peekClaim(c.packet)
		if claim != c.claim || decided != c.decided {
			t.Errorf("%s: peekClaim = (%v, %v), want (%v, %v)", c.name, claim, decided, c.claim, c.decided)
		}
	}
}

// TestHttpNatEviction verifies the maintenance loop's NAT-record eviction: a record idle
// longer than the timeout is dropped, a fresh one is kept, and a non-positive timeout is a
// no-op.
func TestHttpNatEviction(t *testing.T) {
	mux := &UpgradeMux{httpNat: map[httpNatKey]httpNatEntry{}}
	now := time.Now().UnixNano()
	idle := time.Minute
	dst := netip.MustParseAddr("93.184.216.34")
	stale := httpNatKey{ip: netip.MustParseAddr("10.0.0.1"), port: 1}
	fresh := httpNatKey{ip: netip.MustParseAddr("10.0.0.2"), port: 2}
	mux.httpNat[stale] = httpNatEntry{dst: dst, lastActivityNanos: now - int64(2*idle)}
	mux.httpNat[fresh] = httpNatEntry{dst: dst, lastActivityNanos: now}

	mux.evictHttpNat(idle)
	if _, ok := mux.httpNat[stale]; ok {
		t.Fatal("idle nat record should be evicted")
	}
	if _, ok := mux.httpNat[fresh]; !ok {
		t.Fatal("fresh nat record should be kept")
	}
	mux.evictHttpNat(0)
	if _, ok := mux.httpNat[fresh]; !ok {
		t.Fatal("evictHttpNat(0) should not evict")
	}
}

// TestReverseEviction verifies the maintenance loop's IP→hostname affinity-record eviction.
func TestReverseEviction(t *testing.T) {
	mux := &UpgradeMux{reverse: map[netip.Addr]reverseEntry{}}
	now := time.Now().UnixNano()
	ttl := time.Minute
	stale := netip.MustParseAddr("93.184.216.34")
	fresh := netip.MustParseAddr("93.184.216.35")
	mux.reverse[stale] = reverseEntry{serverNames: []string{"a.example"}, lastActivityNanos: now - int64(2*ttl)}
	mux.reverse[fresh] = reverseEntry{serverNames: []string{"b.example"}, lastActivityNanos: now}

	mux.evictReverse(ttl)
	if _, ok := mux.reverse[stale]; ok {
		t.Fatal("idle affinity record should be evicted")
	}
	if _, ok := mux.reverse[fresh]; !ok {
		t.Fatal("fresh affinity record should be kept")
	}
	mux.evictReverse(0)
	if _, ok := mux.reverse[fresh]; !ok {
		t.Fatal("evictReverse(0) should not evict")
	}
}

// TestReverseTouchKeepsActive verifies that a return packet refreshes an affinity record, so
// an IP with live return traffic is not idle-evicted (the fix for the routing-affinity flip).
func TestReverseTouchKeepsActive(t *testing.T) {
	mux := &UpgradeMux{reverse: map[netip.Addr]reverseEntry{}}
	ip := netip.MustParseAddr("93.184.216.34")
	// an entry old enough to be evicted
	mux.reverse[ip] = reverseEntry{serverNames: []string{"x.example"}, lastActivityNanos: time.Now().UnixNano() - int64(2*time.Minute)}
	// a return packet from that IP refreshes its activity
	mux.touchServerNames(net.ParseIP("93.184.216.34"))
	mux.evictReverse(time.Minute)
	if _, ok := mux.reverse[ip]; !ok {
		t.Fatal("an affinity record refreshed by return traffic should not be idle-evicted")
	}
}

// TestRewriteTcpAddrs verifies the in-place incremental-checksum rewrite (the bulk path)
// produces the same bytes — including IP and TCP checksums — as the gopacket
// parse/serialize fallback, for IPv4/IPv6 source and destination rewrites.
func TestRewriteTcpAddrs(t *testing.T) {
	v4 := func(s string) *IpPath {
		return &IpPath{Version: 4, Protocol: IpProtocolTcp, SourceIp: net.ParseIP("10.1.2.3"), SourcePort: 12345, DestinationIp: net.ParseIP(s), DestinationPort: 80}
	}
	v6 := func(s string) *IpPath {
		return &IpPath{Version: 6, Protocol: IpProtocolTcp, SourceIp: net.ParseIP("2001:db8::1"), SourcePort: 12345, DestinationIp: net.ParseIP(s), DestinationPort: 80}
	}
	cases := []struct {
		name     string
		base     []byte
		src, dst netip.Addr
	}{
		{"v4 dst", ipOosPacket(v4("93.184.216.34"), []byte("hello world")), netip.Addr{}, netip.MustParseAddr("169.254.5.6")},
		{"v4 src", ipOosPacket(v4("93.184.216.34"), []byte("hello world")), netip.MustParseAddr("169.254.7.8"), netip.Addr{}},
		{"v6 dst", ipOosPacket(v6("2001:db8::2"), []byte("hello world")), netip.Addr{}, netip.MustParseAddr("fd00::5")},
		{"v6 src", ipOosPacket(v6("2001:db8::2"), []byte("hello world")), netip.MustParseAddr("fd00::9"), netip.Addr{}},
	}
	for _, c := range cases {
		inPlace := rewriteTcpAddrs(append([]byte(nil), c.base...), c.src, c.dst)
		viaGopacket := rewriteTcpAddrsGopacket(append([]byte(nil), c.base...), c.src, c.dst)
		if inPlace == nil || viaGopacket == nil {
			t.Errorf("%s: rewrite returned nil (in-place=%v gopacket=%v)", c.name, inPlace == nil, viaGopacket == nil)
			continue
		}
		if !bytes.Equal(inPlace, viaGopacket) {
			t.Errorf("%s: in-place rewrite != gopacket rewrite\n in-place: %x\n gopacket: %x", c.name, inPlace, viaGopacket)
		}
	}
}

// TestUpgradeMuxHttpsKeepAlive verifies the terminator keeps a client connection alive
// across multiple requests (each upgraded over its own HTTPS connection to the origin).
func TestUpgradeMuxHttpsKeepAlive(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "path="+r.URL.Path)
	}))
	defer origin.Close()
	pool := x509.NewCertPool()
	pool.AddCert(origin.Certificate())

	// 30s to accommodate the bounded connect+request retries below.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	clientTun, err := createPrivateClientTun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer clientTun.Close()

	settings := DefaultUpgradeMuxSettings()
	settings.Http = &HttpUpgradeSettings{Mode: HttpUpgradeHttps, TlsConfig: &tls.Config{RootCAs: pool}}
	mux, err := NewUpgradeMux(ctx, TransferPath{}, protocol.ProvideMode_Network, 0,
		func(s TransferPath, p protocol.ProvideMode, ipPath *IpPath, packet []byte) { clientTun.Write(packet) },
		settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mux.Close()
	mux.SetUpstream(func(s TransferPath, p protocol.ProvideMode, packet []byte, timeout time.Duration) bool { return true })
	mux.dialUpstream = func(ctx context.Context, network, address string) (net.Conn, error) {
		return net.Dial("tcp", origin.Listener.Addr().String())
	}
	go func() {
		for {
			packet, err := clientTun.Read()
			if err != nil {
				return
			}
			mux.SendPacket(TransferPath{}, protocol.ProvideMode_Network, packet, 0)
		}
	}()

	// one client connection, two sequential requests (HTTP/1.1 keep-alive). the two-stack
	// handshake can flake the same way httpUpgradeGet's does (see docs/IP_MUX_UPGRADE.md);
	// retry the whole connect+requests sequence on a stall, as a real client would. a wrong
	// status/body is a real failure and fails immediately; only stalls/errors are retried.
	const maxAttempts = 6
	const attemptTimeout = 4 * time.Second
	ok := false
	var lastErr error
	for range maxAttempts {
		lastErr = func() error {
			dialCtx, dialCancel := context.WithTimeout(ctx, attemptTimeout)
			defer dialCancel()
			conn, err := clientTun.DialContext(dialCtx, "tcp", "10.9.9.9:80")
			if err != nil {
				return err
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(attemptTimeout))
			reader := bufio.NewReader(conn)
			for _, path := range []string{"/one", "/two"} {
				req, err := http.NewRequest("GET", "http://example.com"+path, nil)
				if err != nil {
					return err
				}
				if err := req.Write(conn); err != nil {
					return err
				}
				resp, err := http.ReadResponse(reader, req)
				if err != nil {
					return err
				}
				got, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK || string(got) != "path="+path {
					t.Fatalf("%s: status=%d body=%q, want 200 path=%s", path, resp.StatusCode, got, path)
				}
			}
			return nil
		}()
		if lastErr == nil {
			ok = true
			break
		}
	}
	if !ok {
		t.Fatalf("keep-alive requests failed after %d attempts: %v", maxAttempts, lastErr)
	}
}
