package connect

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
	"golang.org/x/net/dns/dnsmessage"
)

// TestUpgradeMuxPassthrough verifies that traffic the mux does not claim flows through
// unchanged. (DNS interception and the HTTP pass/drop policy are covered by the tests below.)
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
	// these harness tests exercise the tunnel-DoH path only; disable the local fallback
	settings.Dns.Fallback = nil

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

	// local RFC 8484 wire DoH server over TLS
	var dohRequests int32
	dohServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&dohRequests, 1)
		// answer A queries; AAAA returns an empty answer so LookupHost resolves to the A record
		writeDohWire(w, r, []netip.Addr{netip.MustParseAddr(resolved)}, 60, false)
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
	if atomic.LoadInt32(&dohRequests) == 0 {
		t.Fatal("mux did not resolve via the local DoH server")
	}

	// reverse index (point 4): the mux records the hostname it served for the IP
	names := h.mux.ServerNames(resolved)
	if !slices.Contains(names, queryName) {
		t.Fatalf("ServerNames(%s) = %v, want to contain %s", resolved, names, queryName)
	}
}

// TestUpgradeMuxDnsNoReplyOnFailure: on a DoH resolution failure (the resolver errors, not an
// authoritative no-record answer) the mux sends NO downstream response at all — the client's
// query then times out and is retried, rather than getting a SERVFAIL or empty answer that a
// browser surfaces as "can't resolve address".
func TestUpgradeMuxDnsNoReplyOnFailure(t *testing.T) {
	// a DoH server that always fails: empty (no records) and not an authoritative no-record
	dohServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer dohServer.Close()

	pool := x509.NewCertPool()
	pool.AddCert(dohServer.Certificate())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec := &ipMuxRecorder{}
	settings := DefaultUpgradeMuxSettings()
	settings.Dns.Resolver = &DnsResolverSettings{
		EnableLocalDoh:   true,
		LocalDohUrlsIpv4: []string{dohServer.URL},
		TlsConfig:        &tls.Config{RootCAs: pool},
	}
	// isolate the no-reply-on-failure behavior: no local fallback to answer
	settings.Dns.Fallback = nil
	settings.Dns.ResolveTimeout = 200 * time.Millisecond
	mux, err := NewUpgradeMux(ctx, TransferPath{}, protocol.ProvideMode_Network, 0, rec.receive, settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mux.Close()
	mux.SetUpstream(rec.upstream)

	if !mux.SendPacket(TransferPath{}, protocol.ProvideMode_Network, dnsQueryPacket(t, "fail.example.test."), 0) {
		t.Fatal("SendPacket returned false; the DNS query was not claimed")
	}

	// let the resolve budget (200ms) be exhausted, then confirm nothing was sent back downstream
	time.Sleep(1 * time.Second)
	if _, received := rec.counts(); received != 0 {
		t.Fatalf("mux sent %d downstream replies on a resolution failure; want 0 (no response)", received)
	}
}

// dnsQueryPacket crafts the IPv4/UDP DNS A-query packet for an FQDN (name must end in ".") as the
// mux sees it on the send path (destined for :53).
func dnsQueryPacket(t *testing.T, name string) []byte {
	t.Helper()
	qb := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 0x1234, RecursionDesired: true})
	if err := qb.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	if err := qb.Question(dnsmessage.Question{
		Name:  dnsmessage.MustNewName(name),
		Type:  dnsmessage.TypeA,
		Class: dnsmessage.ClassINET,
	}); err != nil {
		t.Fatal(err)
	}
	queryPayload, err := qb.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return ipOosPacket(&IpPath{
		Version:         4,
		Protocol:        IpProtocolUdp,
		SourceIp:        net.ParseIP("169.254.9.9"),
		SourcePort:      33333,
		DestinationIp:   net.ParseIP("10.0.0.1"),
		DestinationPort: 53,
	}, queryPayload)
}

// TestUpgradeMuxDnsLocalFallback: when the tunnel-DoH can't resolve, a query is raced — after the
// LocalFallbackTimeout handicap — against the local-egress fallback resolver, which answers so the
// client (and the OS) gets a timely response rather than DNS hanging while the tunnel comes up.
func TestUpgradeMuxDnsLocalFallback(t *testing.T) {
	// the tunnel-DoH always fails (503)
	tunnelServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer tunnelServer.Close()
	tunnelPool := x509.NewCertPool()
	tunnelPool.AddCert(tunnelServer.Certificate())

	// the local fallback resolves to a fixed IP
	fallbackServer, fallbackResolver := newDohWireServer(t, "203.0.113.77")
	defer fallbackServer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec := &ipMuxRecorder{}
	settings := DefaultUpgradeMuxSettings()
	settings.Dns.Resolver = &DnsResolverSettings{
		EnableLocalDoh:   true,
		LocalDohUrlsIpv4: []string{tunnelServer.URL},
		TlsConfig:        &tls.Config{RootCAs: tunnelPool},
	}
	settings.Dns.Fallback = fallbackResolver
	settings.Dns.LocalFallbackTimeout = 200 * time.Millisecond
	mux, err := NewUpgradeMux(ctx, TransferPath{}, protocol.ProvideMode_Network, 0, rec.receive, settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mux.Close()
	mux.SetUpstream(rec.upstream)

	if !mux.SendPacket(TransferPath{}, protocol.ProvideMode_Network, dnsQueryPacket(t, "fallback.example.test."), 0) {
		t.Fatal("SendPacket returned false; the DNS query was not claimed")
	}

	// the tunnel-DoH fails, so the handicapped fallback should answer shortly after its 200ms delay
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, received := rec.counts(); 0 < received {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no fallback reply; the local fallback must answer when the tunnel-DoH fails")
}

// TestUpgradeMuxResolveTimeoutReDerived: ResolveTimeout is the single DNS timeout — the tun's DoH
// request timeout is derived from it at creation and re-derived on SetSettings, so a runtime
// settings change fully propagates.
func TestUpgradeMuxResolveTimeoutReDerived(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec := &ipMuxRecorder{}
	settings := DefaultUpgradeMuxSettings()
	settings.Dns.ResolveTimeout = 12 * time.Second
	mux, err := NewUpgradeMux(ctx, TransferPath{}, protocol.ProvideMode_Network, 0, rec.receive, settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mux.Close()

	if got := mux.mux.Tun().DohCache().settings.RequestTimeout; got != 12*time.Second {
		t.Fatalf("initial DoH request timeout = %v, want 12s (derived from ResolveTimeout)", got)
	}

	next := DefaultUpgradeMuxSettings()
	next.Dns.ResolveTimeout = 27 * time.Second
	mux.SetSettings(next)

	if got := mux.mux.Tun().DohCache().settings.RequestTimeout; got != 27*time.Second {
		t.Fatalf("after SetSettings, DoH request timeout = %v, want 27s (re-derived from ResolveTimeout)", got)
	}
}

// newDohWireServer starts a local TLS DoH server (RFC 8484 wire) that answers A queries with
// a fixed IP, and returns resolver settings wired to trust and use it (local DoH).
func newDohWireServer(t *testing.T, resolvedIp string) (*httptest.Server, *DnsResolverSettings) {
	t.Helper()
	ip := netip.MustParseAddr(resolvedIp)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// answer A queries; AAAA gets an empty answer (NODATA)
		writeDohWire(w, r, []netip.Addr{ip}, 60, false)
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

	serverA, dnsA := newDohWireServer(t, ipA)
	defer serverA.Close()
	serverB, dnsB := newDohWireServer(t, ipB)
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
		name   string
		packet []byte
		want   peekResult
	}{
		{"v4 tcp 80", mkv4(tcp, 80), peekHttp},
		{"v4 tcp 443", mkv4(tcp, 443), peekOther},
		{"v4 udp 53", mkv4(udp, 53), peekDns},
		{"v4 udp 4500", mkv4(udp, 4500), peekOther},
		{"v4 icmp", mkv4(icmp, 0), peekOther},
		{"v6 tcp 80", mkv6(tcp, 80), peekHttp},
		{"v6 tcp 443", mkv6(tcp, 443), peekOther},
		{"v6 udp 53", mkv6(udp, 53), peekDns},
		{"v6 extension header", mkv6(hopopt, 80), peekUndecided},
		{"short", []byte{0x45, 0x00}, peekUndecided},
		{"empty", nil, peekUndecided},
	}
	for _, c := range cases {
		if got := peekClaim(c.packet); got != c.want {
			t.Errorf("%s: peekClaim = %d, want %d", c.name, got, c.want)
		}
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
