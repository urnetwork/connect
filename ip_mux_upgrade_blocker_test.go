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
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
	"golang.org/x/net/dns/dnsmessage"
)

func (self *ipMuxRecorder) receivedPackets() [][]byte {
	self.mu.Lock()
	defer self.mu.Unlock()
	out := make([][]byte, len(self.received))
	copy(out, self.received)
	return out
}

// dnsQueryPacketTyped is dnsQueryPacket with the record type controlled, so
// blocker tests can model HTTPS/SVCB (65) and TXT queries for blocked names.
func dnsQueryPacketTyped(t *testing.T, name string, qtype dnsmessage.Type, id uint16) []byte {
	t.Helper()
	qb := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id, RecursionDesired: true})
	if err := qb.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	if err := qb.Question(dnsmessage.Question{
		Name:  dnsmessage.MustNewName(name),
		Type:  qtype,
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
		SourcePort:      44444,
		DestinationIp:   net.ParseIP("10.0.0.1"),
		DestinationPort: 53,
	}, queryPayload)
}

// parseDnsBlockedReply decodes a downstream packet as the udp dns reply the
// mux synthesized: reversed path (from :53), then the message sections.
// (parseDnsReply in ip_mux_upgrade_test.go is A-record-specific.)
func parseDnsBlockedReply(t *testing.T, packet []byte) (dnsmessage.Header, dnsmessage.Question, []dnsmessage.Resource) {
	t.Helper()
	ipPath, payload, err := ParseIpPathWithPayload(packet)
	if err != nil {
		t.Fatal(err)
	}
	if ipPath.Protocol != IpProtocolUdp || ipPath.SourcePort != 53 {
		t.Fatalf("reply not from :53: %+v", ipPath)
	}
	var p dnsmessage.Parser
	header, err := p.Start(payload)
	if err != nil {
		t.Fatal(err)
	}
	question, err := p.Question()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SkipAllQuestions(); err != nil {
		t.Fatal(err)
	}
	answers, err := p.AllAnswers()
	if err != nil {
		t.Fatal(err)
	}
	return header, question, answers
}

// TestUpgradeMuxDnsBlocked drives blocked and unblocked queries through the
// mux send path directly and asserts the synthesized replies: A→0.0.0.0,
// AAAA→::, every other type (HTTPS/SVCB 65, TXT) → empty NOERROR; and that
// disabling the blocker (or removing it) returns queries to the resolution
// pipeline without a mux rebuild.
func TestUpgradeMuxDnsBlocked(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec := &ipMuxRecorder{}
	settings := DefaultUpgradeMuxSettings()
	// an empty resolver: unblocked queries enter the pipeline and fail to
	// resolve (no servers), which sends nothing downstream — so every
	// downstream packet in this test is a blocker-synthesized reply.
	settings.Dns.Resolver = &DnsResolverSettings{}
	settings.Dns.Fallback = nil
	settings.Dns.ResolveTimeout = 200 * time.Millisecond
	mux, err := NewUpgradeMux(ctx, TransferPath{}, protocol.ProvideMode_Network, 0, rec.receive, settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mux.Close()
	mux.SetUpstream(rec.upstream)

	blocker := blockerTestNew([]string{"ads.example.com"}, nil, nil)
	mux.SetBlocker(blocker)

	waitReceived := func(count int) bool {
		return waitForCondition(5*time.Second, func() bool {
			_, received := rec.counts()
			return count <= received
		})
	}

	responseTtl := settings.Dns.ResponseTtl

	// blocked A: 0.0.0.0 with the settings ttl, echoing the transaction id
	if !mux.SendPacket(TransferPath{}, protocol.ProvideMode_Network, dnsQueryPacketTyped(t, "sub.ads.example.com.", dnsmessage.TypeA, 0x0a01), 0) {
		t.Fatal("blocked A query not claimed")
	}
	if !waitReceived(1) {
		t.Fatal("no reply to a blocked A query")
	}
	header, question, answers := parseDnsBlockedReply(t, rec.receivedPackets()[0])
	if header.ID != 0x0a01 || !header.Response || header.RCode != dnsmessage.RCodeSuccess {
		t.Fatalf("blocked A header = %+v", header)
	}
	if question.Type != dnsmessage.TypeA {
		t.Fatalf("blocked A question echoed as %v", question.Type)
	}
	if len(answers) != 1 {
		t.Fatalf("blocked A answers = %d, want 1", len(answers))
	}
	if a, ok := answers[0].Body.(*dnsmessage.AResource); !ok || netip.AddrFrom4(a.A) != netip.IPv4Unspecified() {
		t.Fatalf("blocked A answer = %+v, want 0.0.0.0", answers[0].Body)
	}
	if answers[0].Header.TTL != responseTtl {
		t.Fatalf("blocked A ttl = %d, want %d", answers[0].Header.TTL, responseTtl)
	}

	// blocked AAAA: ::
	if !mux.SendPacket(TransferPath{}, protocol.ProvideMode_Network, dnsQueryPacketTyped(t, "ads.example.com.", dnsmessage.TypeAAAA, 0x0a02), 0) {
		t.Fatal("blocked AAAA query not claimed")
	}
	if !waitReceived(2) {
		t.Fatal("no reply to a blocked AAAA query")
	}
	_, _, answers = parseDnsBlockedReply(t, rec.receivedPackets()[1])
	if len(answers) != 1 {
		t.Fatalf("blocked AAAA answers = %d, want 1", len(answers))
	}
	if a, ok := answers[0].Body.(*dnsmessage.AAAAResource); !ok || netip.AddrFrom16(a.AAAA) != netip.IPv6Unspecified() {
		t.Fatalf("blocked AAAA answer = %+v, want ::", answers[0].Body)
	}

	// blocked HTTPS/SVCB (type 65): claimed with an empty NOERROR, so the
	// browser cannot reconnect via the record's ip hints
	if !mux.SendPacket(TransferPath{}, protocol.ProvideMode_Network, dnsQueryPacketTyped(t, "ads.example.com.", dnsmessage.Type(65), 0x0a03), 0) {
		t.Fatal("blocked HTTPS query not claimed")
	}
	if !waitReceived(3) {
		t.Fatal("no reply to a blocked HTTPS query")
	}
	header, question, answers = parseDnsBlockedReply(t, rec.receivedPackets()[2])
	if header.RCode != dnsmessage.RCodeSuccess || len(answers) != 0 {
		t.Fatalf("blocked HTTPS reply: rcode=%v answers=%d, want NOERROR/0", header.RCode, len(answers))
	}
	if question.Type != dnsmessage.Type(65) {
		t.Fatalf("blocked HTTPS question echoed as %v", question.Type)
	}

	// blocked TXT: same empty NOERROR shape
	if !mux.SendPacket(TransferPath{}, protocol.ProvideMode_Network, dnsQueryPacketTyped(t, "ads.example.com.", dnsmessage.TypeTXT, 0x0a04), 0) {
		t.Fatal("blocked TXT query not claimed")
	}
	if !waitReceived(4) {
		t.Fatal("no reply to a blocked TXT query")
	}
	_, _, answers = parseDnsBlockedReply(t, rec.receivedPackets()[3])
	if len(answers) != 0 {
		t.Fatalf("blocked TXT answers = %d, want 0", len(answers))
	}

	// an unblocked name is not answered by the blocker: it enters the
	// pipeline, fails to resolve (no servers), and sends nothing
	if !mux.SendPacket(TransferPath{}, protocol.ProvideMode_Network, dnsQueryPacketTyped(t, "ok.example.org.", dnsmessage.TypeA, 0x0a05), 0) {
		t.Fatal("unblocked A query not claimed")
	}
	// an unblocked HTTPS/SVCB query is claimed and routed to the DoH forward path (not
	// passed through). This test's resolver has remote DoH disabled, so the forward
	// fails fast with a prompt SERVFAIL (never silence on the claimed type).
	if !mux.SendPacket(TransferPath{}, protocol.ProvideMode_Network, dnsQueryPacketTyped(t, "ok.example.org.", dnsmessage.Type(65), 0x0a06), 0) {
		t.Fatal("unblocked HTTPS query not claimed")
	}
	// an unblocked non-A/AAAA/SVCB type (TXT) passes through to the upstream unclaimed
	if mux.SendPacket(TransferPath{}, protocol.ProvideMode_Network, dnsQueryPacketTyped(t, "ok.example.org.", dnsmessage.TypeTXT, 0x0a0a), 0) {
		// pass-through returns the upstream's result; the recorder upstream
		// accepts, so SendPacket returns true — assert it reached upstream
	}
	if !waitReceived(5) {
		t.Fatal("no SERVFAIL for the unblocked HTTPS query with remote DoH off")
	}
	time.Sleep(500 * time.Millisecond)
	// the 4 blocker-synthesized replies plus the HTTPS forward SERVFAIL; the
	// unblocked A produced none
	if _, received := rec.counts(); received != 5 {
		t.Fatalf("unexpected downstream replies: %d, want 5", received)
	}
	header, question, answers = parseDnsBlockedReply(t, rec.receivedPackets()[4])
	if header.ID != 0x0a06 || header.RCode != dnsmessage.RCodeServerFailure || len(answers) != 0 {
		t.Fatalf("unblocked HTTPS forward-failure reply: id=%04x rcode=%v answers=%d, want 0a06/SERVFAIL/0", header.ID, header.RCode, len(answers))
	}
	if question.Type != dnsmessage.Type(65) {
		t.Fatalf("unblocked HTTPS question echoed as %v", question.Type)
	}
	if sent, _ := rec.counts(); sent != 1 {
		t.Fatalf("upstream pass-throughs: %d, want 1 (the unblocked TXT query)", sent)
	}

	// toggling off returns blocked names to the pipeline (no reply), and
	// toggling back on blocks again — no mux rebuild
	blocker.SetEnabled(false)
	if !mux.SendPacket(TransferPath{}, protocol.ProvideMode_Network, dnsQueryPacketTyped(t, "ads.example.com.", dnsmessage.TypeA, 0x0a07), 0) {
		t.Fatal("disabled-blocker A query not claimed")
	}
	time.Sleep(500 * time.Millisecond)
	if _, received := rec.counts(); received != 5 {
		t.Fatalf("disabled blocker still replied: %d", received)
	}
	blocker.SetEnabled(true)
	if !mux.SendPacket(TransferPath{}, protocol.ProvideMode_Network, dnsQueryPacketTyped(t, "ads.example.com.", dnsmessage.TypeA, 0x0a08), 0) {
		t.Fatal("re-enabled blocker A query not claimed")
	}
	if !waitReceived(6) {
		t.Fatal("re-enabled blocker did not reply")
	}

	// removing the blocker entirely returns to pipeline behavior
	mux.SetBlocker(nil)
	if !mux.SendPacket(TransferPath{}, protocol.ProvideMode_Network, dnsQueryPacketTyped(t, "ads.example.com.", dnsmessage.TypeA, 0x0a09), 0) {
		t.Fatal("nil-blocker A query not claimed")
	}
	time.Sleep(500 * time.Millisecond)
	if _, received := rec.counts(); received != 6 {
		t.Fatalf("nil blocker still replied: %d", received)
	}
}

// TestUpgradeMuxDnsBlockBeatsCache resolves a name with the blocker disabled
// (warming the resolver cache and the reverse index), then enables the
// blocker: the next query must be blocked — the check sits ahead of the
// cache — and blocked answers must not pollute the reverse index.
func TestUpgradeMuxDnsBlockBeatsCache(t *testing.T) {
	const resolved = "203.0.113.45"
	const queryName = "cached.blockme.test"

	dohServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeDohWire(w, r, []netip.Addr{netip.MustParseAddr(resolved)}, 60, false)
	}))
	defer dohServer.Close()

	pool := x509.NewCertPool()
	pool.AddCert(dohServer.Certificate())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	h := newDnsClientHarness(t, ctx, &DnsResolverSettings{
		EnableLocalDoh:   true,
		LocalDohUrlsIpv4: []string{dohServer.URL},
		TlsConfig:        &tls.Config{RootCAs: pool},
	})
	defer h.close()

	blocker := blockerTestNew([]string{"blockme.test"}, nil, nil)
	blocker.SetEnabled(false)
	h.mux.SetBlocker(blocker)

	// disabled: resolves normally and records the reverse index
	addrs, err := h.resolver().LookupHost(ctx, queryName)
	if err != nil {
		t.Fatalf("LookupHost (blocker disabled): %v", err)
	}
	if !slices.Contains(addrs, resolved) {
		t.Fatalf("LookupHost = %v, want to contain %s", addrs, resolved)
	}

	// enabled: the same (cached) name is now blocked — subdomain semantics
	// via the blocked base "blockme.test"
	blocker.SetEnabled(true)
	addrs, err = h.resolver().LookupHost(ctx, queryName)
	if err != nil {
		t.Fatalf("LookupHost (blocker enabled): %v", err)
	}
	if slices.Contains(addrs, resolved) {
		t.Fatalf("blocked name still resolves the cached address: %v", addrs)
	}
	if !slices.Contains(addrs, "0.0.0.0") && !slices.Contains(addrs, "::") {
		t.Fatalf("blocked name did not answer the unspecified address: %v", addrs)
	}

	// blocked answers never pollute the reverse index
	if names := h.mux.ServerNames("0.0.0.0"); len(names) != 0 {
		t.Fatalf("reverse index polluted for 0.0.0.0: %v", names)
	}
	if names := h.mux.ServerNames("::"); len(names) != 0 {
		t.Fatalf("reverse index polluted for :: : %v", names)
	}
}
