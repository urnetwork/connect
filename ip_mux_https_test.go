package connect

import (
	"context"
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/urnetwork/connect/protocol"
)

func httpsQuestion(domain string) dnsmessage.Question {
	return dnsmessage.Question{
		Name:  dnsmessage.MustNewName(domain + "."),
		Type:  dnsTypeHttps,
		Class: dnsmessage.ClassINET,
	}
}

// buildHttpsAnswer builds a DNS response echoing q with a single HTTPS answer carrying the
// given ipv4hint/ipv6hint SvcParams.
func buildHttpsAnswer(t *testing.T, id uint16, q dnsmessage.Question, v4 []string, v6 []string) []byte {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id, Response: true, RecursionAvailable: true})
	if err := b.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	if err := b.Question(q); err != nil {
		t.Fatal(err)
	}
	if err := b.StartAnswers(); err != nil {
		t.Fatal(err)
	}
	rh := dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 300}
	var r dnsmessage.HTTPSResource
	r.Priority = 1
	r.Target = q.Name
	if 0 < len(v4) {
		var v []byte
		for _, s := range v4 {
			a := netip.MustParseAddr(s).As4()
			v = append(v, a[:]...)
		}
		r.SetParam(dnsmessage.SVCParamIPv4Hint, v)
	}
	if 0 < len(v6) {
		var v []byte
		for _, s := range v6 {
			a := netip.MustParseAddr(s).As16()
			v = append(v, a[:]...)
		}
		r.SetParam(dnsmessage.SVCParamIPv6Hint, v)
	}
	if err := b.HTTPSResource(rh, r); err != nil {
		t.Fatal(err)
	}
	resp, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestParseHttpsHints(t *testing.T) {
	// v4 + v6 hints from one HTTPS record (v4 first, then v6, per appendSvcbHints)
	resp := buildHttpsAnswer(t, 0, httpsQuestion("cdn.example"),
		[]string{"104.16.1.1", "104.16.2.2"}, []string{"2606:4700::1"})
	got := parseHttpsHints(resp)
	want := []netip.Addr{
		netip.MustParseAddr("104.16.1.1"),
		netip.MustParseAddr("104.16.2.2"),
		netip.MustParseAddr("2606:4700::1"),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("parseHttpsHints = %v, want %v", got, want)
	}

	// an HTTPS record with no hints yields nothing
	if h := parseHttpsHints(buildHttpsAnswer(t, 0, httpsQuestion("nohints.example"), nil, nil)); len(h) != 0 {
		t.Fatalf("expected no hints, got %v", h)
	}

	// malformed input must not panic and yields no hints
	for _, bad := range [][]byte{nil, {0x00}, {0x00, 0x01, 0x02, 0x03}, make([]byte, 12)} {
		if h := parseHttpsHints(bad); len(h) != 0 {
			t.Fatalf("malformed input produced hints: %v", h)
		}
	}
}

func TestDnsResponseUsable(t *testing.T) {
	mk := func(rcode byte) []byte {
		b := make([]byte, 12)
		b[3] = rcode
		return b
	}
	if !dnsResponseUsable(mk(0)) { // NOERROR
		t.Fatal("NOERROR should be usable")
	}
	if !dnsResponseUsable(mk(3)) { // NXDOMAIN
		t.Fatal("NXDOMAIN should be usable")
	}
	if dnsResponseUsable(mk(2)) { // SERVFAIL
		t.Fatal("SERVFAIL should not be usable")
	}
	if dnsResponseUsable(mk(5)) { // REFUSED
		t.Fatal("REFUSED should not be usable")
	}
	if dnsResponseUsable([]byte{0x00}) {
		t.Fatal("a short response should not be usable")
	}
}

// TestDohCacheForward verifies the raw SVCB/HTTPS forward: the cache POSTs the query to the
// remote DoH server and returns the raw response wire (from which hints parse), and does not
// cache it (each call re-queries).
func TestDohCacheForward(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var requestCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
		raw, err := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("dns"))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var p dnsmessage.Parser
		header, err := p.Start(raw)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		q, err := p.Question()
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/dns-message")
		w.Write(buildHttpsAnswer(t, header.ID, q, []string{"104.16.1.1"}, nil))
	}))
	defer server.Close()

	settings := DefaultDohSettings()
	settings.RequestTimeout = 2 * time.Second
	settings.DnsResolverSettings.EnableRemoteDoh = true
	settings.DnsResolverSettings.EnableRemoteDns = false
	settings.DnsResolverSettings.EnableLocalDns = false
	settings.DnsResolverSettings.RemoteDohUrlsIpv4 = []string{server.URL}
	dohCache := NewDohCache(settings)

	resp, ok := dohCache.Forward(ctx, dnsTypeHttps, "cdn.example")
	if !ok {
		t.Fatal("Forward returned ok=false")
	}
	if hints := parseHttpsHints(resp); !slices.Contains(hints, netip.MustParseAddr("104.16.1.1")) {
		t.Fatalf("forwarded response hints = %v, want to contain 104.16.1.1", hints)
	}

	// the forward is best-effort and uncached: a second call re-queries the server
	if _, ok := dohCache.Forward(ctx, dnsTypeHttps, "cdn.example"); !ok {
		t.Fatal("second Forward returned ok=false")
	}
	AssertEqual(t, int32(2), atomic.LoadInt32(&requestCount))
}

// TestUpgradeMuxHttpsForwardFanOut covers the forward pipeline's delivery half (the
// tunnel-DoH round-trip can't be driven in a connect unit test — TestDohCacheForward
// covers that): a forwarded record is fanned out to every coalesced responder with its
// own transaction id, and its hint addresses are recorded into the reverse index.
func TestUpgradeMuxHttpsForwardFanOut(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec := &ipMuxRecorder{}
	mux, err := NewUpgradeMux(ctx, TransferPath{}, protocol.ProvideMode_Network, 0, rec.receive, DefaultUpgradeMuxSettings(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mux.Close()
	mux.SetUpstream(rec.upstream)

	const domain = "cdn.example.test"
	const hintIp = "104.16.9.9"

	mkResponder := func(id uint16, srcPort int, questionDomain string) dnsResponder {
		path := &IpPath{
			Version:         4,
			Protocol:        IpProtocolUdp,
			SourceIp:        net.ParseIP("10.0.0.1"),
			SourcePort:      srcPort,
			DestinationIp:   net.ParseIP("10.0.0.53"),
			DestinationPort: 53,
		}
		return dnsResponder{
			id:          id,
			question:    httpsQuestion(questionDomain),
			source:      TransferPath{},
			provideMode: protocol.ProvideMode_Network,
			reverse:     path.Reverse(),
		}
	}
	// two clients coalesced onto one flight for the same HTTPS question
	key := NewDohKey("HTTPS", domain)
	fl := mux.attachDnsResponder(key, mkResponder(0x1111, 40001, "CdN.Example.Test"))
	if fl == nil {
		t.Fatal("expected a new flight for the first responder")
	}
	mux.attachDnsResponder(key, mkResponder(0x2222, 40002, "cDn.eXAMPLE.tEST"))

	response := buildHttpsAnswer(t, 0, httpsQuestion(domain), []string{hintIp}, nil)
	mux.fanOutHttpsForward(key, fl, domain, response)

	// The responder snapshot and flight removal are atomic: a query attaching
	// after completion starts a new flight instead of joining a responder list
	// that has already been delivered.
	next := mux.attachDnsResponder(key, mkResponder(0x3333, 40003, "CDN.example.test"))
	if next == nil || next == fl {
		t.Fatal("post-completion query did not start a fresh HTTPS flight")
	}

	// both coalesced clients get the record, each stamped with its own transaction id
	if !waitForCondition(2*time.Second, func() bool { _, r := rec.counts(); return r == 2 }) {
		_, r := rec.counts()
		t.Fatalf("delivered %d replies, want 2 (one per coalesced responder)", r)
	}
	ids := map[uint16]bool{}
	questions := map[uint16]string{}
	for _, packet := range rec.receivedPackets() {
		_, payload, err := ParseIpPathWithPayload(packet)
		if err != nil {
			t.Fatalf("reply parse: %v", err)
		}
		var p dnsmessage.Parser
		h, err := p.Start(payload)
		if err != nil {
			t.Fatalf("dns parse: %v", err)
		}
		ids[h.ID] = true
		q, err := p.Question()
		if err != nil {
			t.Fatalf("dns question parse: %v", err)
		}
		questions[h.ID] = q.Name.String()
		// the delivered payload is the forwarded HTTPS record (its hint round-trips)
		if hints := parseHttpsHints(payload); !slices.Contains(hints, netip.MustParseAddr(hintIp)) {
			t.Fatalf("delivered reply missing the HTTPS hint: %v", hints)
		}
	}
	if !ids[0x1111] || !ids[0x2222] {
		t.Fatalf("delivered transaction ids = %v, want both 0x1111 and 0x2222", ids)
	}
	if questions[0x1111] != "CdN.Example.Test." || questions[0x2222] != "cDn.eXAMPLE.tEST." {
		t.Fatalf("delivered question casing = %v", questions)
	}

	// the hint ip -> domain was recorded, so a flow to the hint reports the server name
	if names := mux.ServerNames(hintIp); !slices.Contains(names, domain) {
		t.Fatalf("ServerNames(%s) = %v, want to contain %s", hintIp, names, domain)
	}
}
