package connect

// The third-party preset mechanisms at the UpgradeMux (ADBLOCK3RD 6.2 and the
// connect part of 6.3): sink answers delivered but never named (B4), the
// blocked-lookups count (B9), the failover and the resolver health (C2, C5),
// the probe (C6), the Fast DNS connect window (C7), a preset whose pins the
// exit refuses (C11, 6.3), and that settings without a preset resolve as
// before. The DoH servers are synthetic: a fake transport answers by host, or
// the mux's own tun dials pins into a test upstream; nothing touches the
// network.

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/urnetwork/connect/protocol"
)

// Synthetic DoH endpoints for a preset, its failover and a host fallback, outside the
// built-in endpoints, so the guard leaves their answers alone.
const (
	testPresetDohUrl   = "https://192.0.2.91/dns-query"
	testFailoverDohUrl = "https://192.0.2.92/dns-query"
	testFallbackDohUrl = "https://192.0.2.93/dns-query"
)

// A synthetic DoH resolver behind a fake transport. answer gives the response to
// a question asked of a host, false failing the request instead; nil answers
// every question with no records. Every question asked is recorded.
type testPresetResolver struct {
	t         *testing.T
	answer    func(host string, question dnsmessage.Question) (testingDohPolicyAnswer, bool)
	stateLock sync.Mutex
	questions []string
}

func newTestPresetResolver(t *testing.T, answer func(host string, question dnsmessage.Question) (testingDohPolicyAnswer, bool)) *testPresetResolver {
	return &testPresetResolver{t: t, answer: answer}
}

func (self *testPresetResolver) roundTrip(request *http.Request) (*http.Response, error) {
	query := testingDohRequestQuery(self.t, request)
	var p dnsmessage.Parser
	if _, err := p.Start(query); err != nil {
		return nil, err
	}
	question, err := p.Question()
	if err != nil {
		return nil, err
	}
	name := strings.TrimSuffix(question.Name.String(), ".")
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.questions = append(self.questions, request.URL.Hostname()+" "+name)
	}()
	answer := testingDohNoError(-1)
	if self.answer != nil {
		var ok bool
		answer, ok = self.answer(request.URL.Hostname(), question)
		if !ok {
			return nil, errors.New("synthetic resolver failure")
		}
	}
	recorder := httptest.NewRecorder()
	recorder.Header().Set("Content-Type", "application/dns-message")
	recorder.Write(testingDohPolicyWire(self.t, query, answer))
	response := recorder.Result()
	response.Request = request
	return response, nil
}

// Whether the resolver was asked about name.
func (self *testPresetResolver) asked(name string) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, question := range self.questions {
		if strings.HasSuffix(question, " "+name) {
			return true
		}
	}
	return false
}

// The questions asked so far.
func (self *testPresetResolver) asks() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.questions)
}

// A mux whose resolver is the synthetic preset (testPresetDohUrl), with no
// fallback, a test logger, and the settings configure leaves. Its upstream
// records and drops what the mux egresses.
func newTestPresetMux(t *testing.T, configure func(settings *UpgradeMuxSettings)) (*UpgradeMux, *ipMuxRecorder) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	rec := &ipMuxRecorder{}
	settings := DefaultUpgradeMuxSettings()
	settings.Dns.Resolver = &DnsResolverSettings{
		EnableRemoteDoh:       true,
		DnsUpgradeMaskAddress: DefaultDnsUpgradeMaskAddress,
		RemoteDohUrlsIpv4:     []string{testPresetDohUrl},
	}
	settings.Dns.Fallback = nil
	if configure != nil {
		configure(settings)
	}
	mux, err := NewUpgradeMux(ctx, TransferPath{}, protocol.ProvideMode_Network, 0, rec.receive, settings, NewNoopLogger())
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		mux.Close()
		cancel()
	})
	mux.SetUpstream(rec.upstream)
	return mux, rec
}

// Installs the synthetic resolver behind the mux's tunnel resolver.
func (self *testPresetResolver) installPrimary(mux *UpgradeMux) {
	mux.mux.Tun().DohCache().remoteClient.httpClient.Transport = dohRoundTripperFunc(self.roundTrip)
}

// Installs the synthetic resolver behind the mux's failover resolver.
func (self *testPresetResolver) installFailover(t *testing.T, mux *UpgradeMux) {
	t.Helper()
	failover := mux.failoverDohCache.Load()
	if failover == nil {
		t.Fatal("the mux built no failover resolver")
	}
	failover.remoteClient.httpClient.Transport = dohRoundTripperFunc(self.roundTrip)
}

// The reply to the client query with id among the packets the mux delivered,
// or nil.
func testPresetReply(rec *ipMuxRecorder, id uint16) []byte {
	for _, packet := range rec.receivedPackets() {
		_, payload, err := ParseIpPathWithPayload(packet)
		if err != nil || len(payload) < 12 {
			continue
		}
		if binary.BigEndian.Uint16(payload[:2]) == id {
			return payload
		}
	}
	return nil
}

// Whether the mux has retired the flight of key: it answered, or gave up.
func testPresetFlightRetired(mux *UpgradeMux, key DohKey) bool {
	mux.inflightLock.Lock()
	defer mux.inflightLock.Unlock()
	_, ok := mux.inflight[key]
	return !ok
}

// Sends one client query over UDP/53 and waits until the mux answered it or
// retired its flight unanswered (a failure the client would retry). Returns the
// reply, or nil when the mux gave up without one.
func testPresetQuery(t *testing.T, mux *UpgradeMux, rec *ipMuxRecorder, name string, qType dnsmessage.Type, id uint16) []byte {
	t.Helper()
	query := testingDnsClientQuery(t, name+".", qType, id, false)
	packet := ipOosPacket(&IpPath{
		Version:         4,
		Protocol:        IpProtocolUdp,
		SourceIp:        net.ParseIP("169.254.9.9"),
		SourcePort:      44444,
		DestinationIp:   net.ParseIP("10.0.0.1"),
		DestinationPort: 53,
	}, query)
	if !mux.SendPacket(TransferPath{}, protocol.ProvideMode_Network, packet, 0) {
		t.Fatalf("the query for %s was not claimed", name)
	}
	recordType := qType.String()
	switch qType {
	case dnsmessage.TypeA:
		recordType = "A"
	case dnsmessage.TypeAAAA:
		recordType = "AAAA"
	}
	key := NewDohKey(recordType, name)
	if !waitForCondition(20*time.Second, func() bool {
		return testPresetReply(rec, id) != nil || testPresetFlightRetired(mux, key)
	}) {
		t.Fatalf("the query for %s was neither answered nor retired", name)
	}
	return testPresetReply(rec, id)
}

// A reply's response code and its A and AAAA answers.
func testPresetReplyAnswers(t *testing.T, reply []byte) (dnsmessage.RCode, []netip.Addr) {
	t.Helper()
	var message dnsmessage.Message
	if err := message.Unpack(reply); err != nil {
		t.Fatalf("reply does not parse: %v", err)
	}
	var addrs []netip.Addr
	for _, answer := range message.Answers {
		switch body := answer.Body.(type) {
		case *dnsmessage.AResource:
			addrs = append(addrs, netip.AddrFrom4(body.A))
		case *dnsmessage.AAAAResource:
			addrs = append(addrs, netip.AddrFrom16(body.AAAA))
		}
	}
	sort.Slice(addrs, func(i int, j int) bool { return addrs[i].Less(addrs[j]) })
	return message.Header.RCode, addrs
}

// A health predicate to wait on.
func testPresetHealthIs(mux *UpgradeMux, state DnsResolverHealthState) func() bool {
	return func() bool { return mux.DnsResolverHealth().State == state }
}

// A synthetic clock for the health thresholds.
type testPresetClock struct {
	nanos atomic.Int64
}

func newTestPresetClock(mux *UpgradeMux) *testPresetClock {
	clock := &testPresetClock{}
	clock.nanos.Store(time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC).UnixNano())
	mux.resolverHealthNow = clock.now
	return clock
}

func (self *testPresetClock) now() time.Time {
	return time.Unix(0, self.nanos.Load())
}

func (self *testPresetClock) advance(duration time.Duration) {
	self.nanos.Add(int64(duration))
}

// A sink answer (0.0.0.0, ::) is delivered to the client but never named in the
// reverse index, on the UDP and the TCP path; a real answer is named, and a
// mixed answer is recorded as before.
func TestUpgradeMuxSinkAnswersAreDeliveredButNeverNamed(t *testing.T) {
	mux, rec := newTestPresetMux(t, nil)
	realAddr := netip.MustParseAddr("203.0.113.80")
	mixedAddr := netip.MustParseAddr("203.0.113.81")
	resolver := newTestPresetResolver(t, func(host string, question dnsmessage.Question) (testingDohPolicyAnswer, bool) {
		switch strings.TrimSuffix(question.Name.String(), ".") {
		case "sink.example", "sinktcp.example":
			return testingDohNoError(-1, netip.IPv4Unspecified(), netip.IPv6Unspecified()), true
		case "mixed.example":
			return testingDohNoError(-1, netip.IPv4Unspecified(), mixedAddr), true
		default:
			return testingDohNoError(-1, realAddr), true
		}
	})
	resolver.installPrimary(mux)

	if _, addrs := testPresetReplyAnswers(t, testPresetQuery(t, mux, rec, "sink.example", dnsmessage.TypeA, 0x0b41)); !slices.Equal(addrs, []netip.Addr{netip.IPv4Unspecified()}) {
		t.Fatalf("A sink reply = %v, expected 0.0.0.0", addrs)
	}
	if _, addrs := testPresetReplyAnswers(t, testPresetQuery(t, mux, rec, "sink.example", dnsmessage.TypeAAAA, 0x0b42)); !slices.Equal(addrs, []netip.Addr{netip.IPv6Unspecified()}) {
		t.Fatalf("AAAA sink reply = %v, expected ::", addrs)
	}
	tcpReply := mux.resolveDnsTcpQuery(testingDnsClientQuery(t, "sinktcp.example.", dnsmessage.TypeA, 0x0b43, false))
	if _, addrs := testPresetReplyAnswers(t, tcpReply); !slices.Equal(addrs, []netip.Addr{netip.IPv4Unspecified()}) {
		t.Fatalf("TCP sink reply = %v, expected 0.0.0.0", addrs)
	}
	for _, sink := range []string{"0.0.0.0", "::"} {
		if names := mux.ServerNames(sink); slices.Contains(names, "sink.example") || slices.Contains(names, "sinktcp.example") {
			t.Fatalf("ServerNames(%s) = %v: a sink answer was named", sink, names)
		}
	}

	testPresetQuery(t, mux, rec, "real.example", dnsmessage.TypeA, 0x0b44)
	if names := mux.ServerNames(realAddr.String()); !slices.Contains(names, "real.example") {
		t.Fatalf("ServerNames(%s) = %v, expected real.example", realAddr, names)
	}
	testPresetQuery(t, mux, rec, "mixed.example", dnsmessage.TypeA, 0x0b45)
	if names := mux.ServerNames(mixedAddr.String()); !slices.Contains(names, "mixed.example") {
		t.Fatalf("ServerNames(%s) = %v, expected the mixed answer recorded", mixedAddr, names)
	}
	if names := mux.ServerNames("0.0.0.0"); !slices.Contains(names, "mixed.example") {
		t.Fatalf("ServerNames(0.0.0.0) = %v, expected the mixed answer recorded as before", names)
	}
}

// The blocked-lookups count (B9): one count per client query answered with a
// sink or an NXDOMAIN carrying extended dns error 15, 16 or 17, a cached block
// and an opaque reply included; nothing else counts.
func TestUpgradeMuxBlockedCount(t *testing.T) {
	mux, rec := newTestPresetMux(t, nil)
	resolver := newTestPresetResolver(t, func(host string, question dnsmessage.Question) (testingDohPolicyAnswer, bool) {
		switch strings.TrimSuffix(question.Name.String(), ".") {
		case "zero.example", "zerotcp.example":
			return testingDohNoError(-1, netip.IPv4Unspecified()), true
		case "zero6.example":
			return testingDohNoError(-1, netip.IPv6Unspecified()), true
		case "ede15.example":
			return testingDohNxdomain(15), true
		case "ede16.example":
			return testingDohNxdomain(16), true
		case "ede17.example", "ede17https.example":
			return testingDohNxdomain(17), true
		case "ede4.example":
			return testingDohNxdomain(4), true
		case "ede18.example":
			return testingDohNxdomain(18), true
		case "nxdomain.example", "nxdomainhttps.example":
			return testingDohNxdomain(-1), true
		case "refused.example":
			return testingDohPolicyAnswer{rcode: dnsmessage.RCodeRefused, ede: -1}, true
		case "mixed.example":
			return testingDohNoError(-1, netip.IPv4Unspecified(), netip.MustParseAddr("203.0.113.82")), true
		default:
			return testingDohNoError(-1, netip.MustParseAddr("203.0.113.83")), true
		}
	})
	resolver.installPrimary(mux)
	mux.SetBlocker(blockerTestNew([]string{"blocker.example"}, nil, nil))

	expect := func(label string, want uint64) {
		t.Helper()
		if got := mux.DnsBlockedCount(); got != want {
			t.Fatalf("%s: blocked count = %d, expected %d", label, got, want)
		}
		if got := mux.DnsResolverHealth().BlockedAnswerCount; got != want {
			t.Fatalf("%s: health blocked count = %d, expected %d", label, got, want)
		}
	}
	nodata := func(label string, reply []byte) {
		t.Helper()
		if reply == nil {
			t.Fatalf("%s: no reply", label)
		}
		if rcode, addrs := testPresetReplyAnswers(t, reply); rcode != dnsmessage.RCodeSuccess || len(addrs) != 0 {
			t.Fatalf("%s: reply %v %v, expected NODATA", label, rcode, addrs)
		}
	}

	testPresetQuery(t, mux, rec, "zero.example", dnsmessage.TypeA, 0x0b51)
	expect("0.0.0.0", 1)
	testPresetQuery(t, mux, rec, "zero6.example", dnsmessage.TypeAAAA, 0x0b52)
	expect("::", 2)
	for i, name := range []string{"ede15.example", "ede16.example", "ede17.example"} {
		// the filter's NXDOMAIN reaches the client as NODATA
		nodata(name, testPresetQuery(t, mux, rec, name, dnsmessage.TypeA, uint16(0x0b53+i)))
		expect(name, uint64(3+i))
	}
	// a cached block counts again when it answers another query
	testPresetQuery(t, mux, rec, "zero.example", dnsmessage.TypeA, 0x0b56)
	expect("cached 0.0.0.0", 6)
	if !resolver.asked("zero.example") || len(slices.DeleteFunc(resolver.asks(), func(question string) bool { return !strings.HasSuffix(question, " zero.example") })) != 1 {
		t.Fatalf("the cached block was asked again: %v", resolver.asks())
	}
	// an opaque reply counts by the same rule, and so does the TCP path
	reply := testPresetQuery(t, mux, rec, "ede17https.example", dnsTypeHttps, 0x0b57)
	if rcode, _ := testPresetReplyAnswers(t, reply); rcode != dnsmessage.RCodeNameError {
		t.Fatalf("forwarded block reply = %v, expected NXDOMAIN", rcode)
	}
	expect("forwarded NXDOMAIN EDE 17", 7)
	mux.resolveDnsTcpQuery(testingDnsClientQuery(t, "zerotcp.example.", dnsmessage.TypeA, 0x0b58, false))
	expect("TCP 0.0.0.0", 8)

	// what never counts
	nodata("nxdomain", testPresetQuery(t, mux, rec, "nxdomain.example", dnsmessage.TypeA, 0x0b59))
	nodata("ede 4", testPresetQuery(t, mux, rec, "ede4.example", dnsmessage.TypeA, 0x0b5a))
	nodata("ede 18", testPresetQuery(t, mux, rec, "ede18.example", dnsmessage.TypeA, 0x0b5b))
	if reply := testPresetQuery(t, mux, rec, "nxdomainhttps.example", dnsTypeHttps, 0x0b5c); reply == nil {
		t.Fatal("no reply to a forwarded plain NXDOMAIN")
	}
	testPresetQuery(t, mux, rec, "mixed.example", dnsmessage.TypeA, 0x0b5d)
	testPresetQuery(t, mux, rec, "real.example", dnsmessage.TypeA, 0x0b5e)
	if reply := testPresetQuery(t, mux, rec, "blocker.example", dnsmessage.TypeA, 0x0b5f); reply == nil {
		t.Fatal("the built-in blocker did not answer")
	}
	expect("plain NXDOMAIN, EDE 4 and 18, mixed, real and blocker answers", 8)

	// a REFUSED is a failure: no reply, no count
	func() {
		settings := DefaultUpgradeMuxSettings()
		settings.Dns.Resolver = &DnsResolverSettings{EnableRemoteDoh: true, RemoteDohUrlsIpv4: []string{testPresetDohUrl}}
		settings.Dns.ResolveTimeout = 300 * time.Millisecond
		mux.SetSettings(settings)
		resolver.installPrimary(mux)
	}()
	if reply := testPresetQuery(t, mux, rec, "refused.example", dnsmessage.TypeA, 0x0b60); reply != nil {
		t.Fatalf("a REFUSED was answered: %x", reply)
	}
	expect("REFUSED, and the settings swap keeps the count", 8)

	mux.ResetDnsBlockedCount()
	expect("reset", 0)
	testPresetQuery(t, mux, rec, "zero.example", dnsmessage.TypeA, 0x0b61)
	expect("after the reset", 1)
}

// A built-in endpoint's filtered response is never delivered, so it never
// counts: the guard turns it into that endpoint failing (3.1).
func TestUpgradeMuxBlockedCountSkipsBuiltInEndpoints(t *testing.T) {
	mux, rec := newTestPresetMux(t, func(settings *UpgradeMuxSettings) {
		settings.Dns.Resolver.RemoteDohUrlsIpv4 = []string{testBuiltInDohA}
		settings.Dns.ResolveTimeout = 300 * time.Millisecond
	})
	resolver := newTestPresetResolver(t, func(host string, question dnsmessage.Question) (testingDohPolicyAnswer, bool) {
		switch strings.TrimSuffix(question.Name.String(), ".") {
		case "sink.example":
			return testingDohNoError(-1, netip.IPv4Unspecified()), true
		default:
			return testingDohNxdomain(17), true
		}
	})
	resolver.installPrimary(mux)
	for i, name := range []string{"sink.example", "ede17.example"} {
		if reply := testPresetQuery(t, mux, rec, name, dnsmessage.TypeA, uint16(0x0b70+i)); reply != nil {
			t.Fatalf("%s: a built-in endpoint's filtered answer was delivered", name)
		}
	}
	if !resolver.asked("sink.example") || !resolver.asked("ede17.example") {
		t.Fatalf("the built-in endpoint was not asked: %v", resolver.asks())
	}
	if got := mux.DnsBlockedCount(); got != 0 {
		t.Fatalf("blocked count = %d from a built-in endpoint, expected 0", got)
	}
}

// Exclusive filtering in the client harness: a filtering server alone blocks
// the name; listed beside an unfiltered server it does not, which is why a
// preset's lists hold only the preset's urls (A6).
func TestUpgradeMuxExclusiveFilteringInTheClientHarness(t *testing.T) {
	const ipVersion = 4
	unfilteredAddr := testDocAddr(ipVersion, 84)
	newFiltering := func(release <-chan struct{}) *httptest.Server {
		return newFamilyHttptestTlsServer(t, ipVersion, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if release != nil {
				select {
				case <-release:
				case <-r.Context().Done():
				}
			}
			writeDohWire(w, r, []netip.Addr{netip.IPv4Unspecified()}, 60, false)
		}))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	filtering := newFiltering(nil)
	defer filtering.Close()
	alone := newDnsClientHarness(t, ctx, ipVersion, localDohTlsResolverSettings(ipVersion, filtering))
	addrs, err := alone.resolver().LookupNetIP(ctx, "ip4", "ads.example")
	alone.close()
	if err != nil || !slices.Equal(addrs, []netip.Addr{netip.IPv4Unspecified()}) {
		t.Fatalf("a filtering server alone: LookupNetIP = %v, %v; expected 0.0.0.0", addrs, err)
	}

	// the filtering server answers only after the unfiltered one already has
	release := make(chan struct{})
	slowFiltering := newFiltering(release)
	defer slowFiltering.Close()
	defer close(release)
	unfiltered := newFamilyHttptestTlsServer(t, ipVersion, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeDohWire(w, r, []netip.Addr{unfilteredAddr}, 60, false)
	}))
	defer unfiltered.Close()
	mixed := localDohTlsResolverSettings(ipVersion, slowFiltering)
	mixed.LocalDohUrlsIpv4 = append(mixed.LocalDohUrlsIpv4, unfiltered.URL)
	mixed.TlsConfig.RootCAs.AddCert(unfiltered.Certificate())
	both := newDnsClientHarness(t, ctx, ipVersion, mixed)
	defer both.close()
	addrs, err = both.resolver().LookupNetIP(ctx, "ip4", "ads.example")
	if err != nil || !slices.Equal(addrs, []netip.Addr{unfilteredAddr}) {
		t.Fatalf("filtering beside an unfiltered server: LookupNetIP = %v, %v; expected the unfiltered %s, filtering lost", addrs, err, unfilteredAddr)
	}
}

// The failover (C2) engages only after both thresholds, counted only while the
// tunnel is ready, and only once the failover answered; from then on new
// queries go to the failover alone.
func TestUpgradeMuxFailoverOnlyAfterThresholdsWhileTunnelReady(t *testing.T) {
	mux, rec := newTestPresetMux(t, func(settings *UpgradeMuxSettings) {
		settings.Dns.Failover = &DnsResolverSettings{EnableRemoteDoh: true, RemoteDohUrlsIpv4: []string{testFailoverDohUrl}}
		settings.Dns.ResolverFailingMinCount = 3
		settings.Dns.ResolverFailingMinDuration = 10 * time.Second
		settings.Dns.FailoverProbeInterval = time.Hour
		settings.Dns.ResolveTimeout = 300 * time.Millisecond
	})
	clock := newTestPresetClock(mux)
	primary := newTestPresetResolver(t, func(string, dnsmessage.Question) (testingDohPolicyAnswer, bool) {
		return testingDohPolicyAnswer{}, false
	})
	primary.installPrimary(mux)
	failoverAddr := netip.MustParseAddr("198.51.100.92")
	failover := newTestPresetResolver(t, func(string, dnsmessage.Question) (testingDohPolicyAnswer, bool) {
		return testingDohNoError(-1, failoverAddr), true
	})
	failover.installFailover(t, mux)
	var ready atomic.Bool
	mux.SetTunnelReady(ready.Load)

	health := func(state DnsResolverHealthState, failureCount int) {
		t.Helper()
		got := mux.DnsResolverHealth()
		if got.State != state || got.ConsecutiveFailureCount != failureCount {
			t.Fatalf("health = %s with %d failures, expected %s with %d", got.State, got.ConsecutiveFailureCount, state, failureCount)
		}
	}

	// the tunnel not ready: a failure does not count
	if reply := testPresetQuery(t, mux, rec, "f1.example", dnsmessage.TypeA, 0x0c01); reply != nil {
		t.Fatal("a failing resolver answered")
	}
	health(DnsResolverHealthCold, 0)

	ready.Store(true)
	testPresetQuery(t, mux, rec, "f2.example", dnsmessage.TypeA, 0x0c02)
	health(DnsResolverHealthCold, 1)
	clock.advance(5 * time.Second)
	testPresetQuery(t, mux, rec, "f3.example", dnsmessage.TypeA, 0x0c03)
	health(DnsResolverHealthCold, 2)
	// three failures in a row, but over six seconds: not yet failing
	clock.advance(time.Second)
	testPresetQuery(t, mux, rec, "f4.example", dnsmessage.TypeA, 0x0c04)
	health(DnsResolverHealthCold, 3)
	clock.advance(5 * time.Second)
	testPresetQuery(t, mux, rec, "f5.example", dnsmessage.TypeA, 0x0c05)
	if !waitForCondition(10*time.Second, testPresetHealthIs(mux, DnsResolverHealthFailedOver)) {
		t.Fatalf("health = %+v past both thresholds, expected failedOver", mux.DnsResolverHealth())
	}
	for _, name := range []string{"f1.example", "f2.example", "f3.example", "f4.example", "f5.example"} {
		if failover.asked(name) {
			t.Fatalf("the failover was asked %s before the switch", name)
		}
	}
	if asks := failover.asks(); len(asks) == 0 || !slices.Contains(asks, "192.0.2.92 "+dohWarmDomain) {
		t.Fatalf("the failover was not proven before the switch: %v", asks)
	}

	reply := testPresetQuery(t, mux, rec, "g1.example", dnsmessage.TypeA, 0x0c06)
	if _, addrs := testPresetReplyAnswers(t, reply); !slices.Equal(addrs, []netip.Addr{failoverAddr}) {
		t.Fatalf("failed over reply = %v, expected the failover's %s", addrs, failoverAddr)
	}
	if primary.asked("g1.example") {
		t.Fatal("a failed over query also went to the failing resolver")
	}
	if got := mux.DnsResolverHealth(); got.State != DnsResolverHealthFailedOver || !got.LastSuccessTime.IsZero() {
		t.Fatalf("health = %+v: failover answers are not the resolver's successes", got)
	}
}

// While failed over, the prober asks the resolver every FailoverProbeInterval
// and its first answer switches back; a network change starts again on the
// resolver.
func TestUpgradeMuxFailoverReturnsAtTheResolversFirstAnswer(t *testing.T) {
	mux, rec := newTestPresetMux(t, func(settings *UpgradeMuxSettings) {
		settings.Dns.Failover = &DnsResolverSettings{EnableRemoteDoh: true, RemoteDohUrlsIpv4: []string{testFailoverDohUrl}}
		settings.Dns.ResolverFailingMinCount = 2
		settings.Dns.ResolverFailingMinDuration = 0
		settings.Dns.FailoverProbeInterval = 20 * time.Millisecond
		settings.Dns.ResolveTimeout = 300 * time.Millisecond
	})
	var primaryDown atomic.Bool
	primaryDown.Store(true)
	primaryAddr := netip.MustParseAddr("198.51.100.91")
	primary := newTestPresetResolver(t, func(string, dnsmessage.Question) (testingDohPolicyAnswer, bool) {
		if primaryDown.Load() {
			return testingDohPolicyAnswer{}, false
		}
		return testingDohNoError(-1, primaryAddr), true
	})
	primary.installPrimary(mux)
	failoverAddr := netip.MustParseAddr("198.51.100.92")
	failover := newTestPresetResolver(t, func(string, dnsmessage.Question) (testingDohPolicyAnswer, bool) {
		return testingDohNoError(-1, failoverAddr), true
	})
	failover.installFailover(t, mux)
	mux.SetTunnelReady(func() bool { return true })

	nextId := uint16(0x0d00)
	failOver := func(names ...string) {
		t.Helper()
		for _, name := range names {
			nextId += 1
			testPresetQuery(t, mux, rec, name, dnsmessage.TypeA, nextId)
		}
		if !waitForCondition(10*time.Second, testPresetHealthIs(mux, DnsResolverHealthFailedOver)) {
			t.Fatalf("health = %+v, expected failedOver", mux.DnsResolverHealth())
		}
	}
	failOver("h1.example", "h2.example")

	primaryDown.Store(false)
	if !waitForCondition(10*time.Second, testPresetHealthIs(mux, DnsResolverHealthOk)) {
		t.Fatalf("health = %+v after the resolver healed, expected ok", mux.DnsResolverHealth())
	}
	if mux.DnsResolverHealth().LastSuccessTime.IsZero() {
		t.Fatal("the resolver's probe answer left no success time")
	}
	reply := testPresetQuery(t, mux, rec, "i1.example", dnsmessage.TypeA, 0x0d80)
	if _, addrs := testPresetReplyAnswers(t, reply); !slices.Equal(addrs, []netip.Addr{primaryAddr}) {
		t.Fatalf("reply after the switch back = %v, expected the resolver's %s", addrs, primaryAddr)
	}
	if failover.asked("i1.example") {
		t.Fatal("a query after the switch back went to the failover")
	}

	// fail over again, then a network change starts on the resolver
	primaryDown.Store(true)
	failOver("j1.example", "j2.example")
	mux.NetworkChanged()
	if got := mux.DnsResolverHealth(); got.State != DnsResolverHealthCold || got.ConsecutiveFailureCount != 0 {
		t.Fatalf("health after a network change = %+v, expected cold", got)
	}
	testPresetQuery(t, mux, rec, "k1.example", dnsmessage.TypeA, 0x0d81)
	if !primary.asked("k1.example") || failover.asked("k1.example") {
		t.Fatalf("after a network change the query went to the failover: resolver %v, failover %v", primary.asks(), failover.asks())
	}
}

// Without a Failover a failing resolver fails closed: the health says failing,
// and queries keep going to the resolver alone.
func TestUpgradeMuxNilFailoverNeverSwitches(t *testing.T) {
	mux, rec := newTestPresetMux(t, func(settings *UpgradeMuxSettings) {
		settings.Dns.ResolverFailingMinCount = 2
		settings.Dns.ResolverFailingMinDuration = 0
		settings.Dns.FailoverProbeInterval = 20 * time.Millisecond
		settings.Dns.ResolveTimeout = 300 * time.Millisecond
	})
	primary := newTestPresetResolver(t, func(string, dnsmessage.Question) (testingDohPolicyAnswer, bool) {
		return testingDohPolicyAnswer{}, false
	})
	primary.installPrimary(mux)
	mux.SetTunnelReady(func() bool { return true })
	for i, name := range []string{"n1.example", "n2.example", "n3.example"} {
		if reply := testPresetQuery(t, mux, rec, name, dnsmessage.TypeA, uint16(0x0e00+i)); reply != nil {
			t.Fatalf("%s: a failing resolver without a failover answered", name)
		}
	}
	if got := mux.DnsResolverHealth(); got.State != DnsResolverHealthFailing || got.ConsecutiveFailureCount != 3 {
		t.Fatalf("health = %+v, expected failing with 3 failures", got)
	}
	if mux.failoverDohCache.Load() != nil {
		t.Fatal("a mux without a Failover built a failover resolver")
	}
	if _, primaryActive := mux.activeTunnelDohCache(); !primaryActive {
		t.Fatal("a mux without a Failover switched away from its resolver")
	}
	testPresetQuery(t, mux, rec, "n4.example", dnsmessage.TypeA, 0x0e04)
	if !primary.asked("n4.example") {
		t.Fatal("a query after failing did not go to the resolver")
	}
}

// Health (C5) moves with fresh answers and failures of the resolver, per
// generation: a cache hit is no evidence, a stale answer is a failure, a
// canceled resolution and a retired generation's evidence count nothing, and a
// settings swap starts over. State changes reach the callbacks.
func TestUpgradeMuxResolverHealthMovesPerGeneration(t *testing.T) {
	mux, rec := newTestPresetMux(t, func(settings *UpgradeMuxSettings) {
		settings.Dns.ResolverFailingMinCount = 3
		settings.Dns.ResolverFailingMinDuration = 0
		settings.Dns.ResolveTimeout = 300 * time.Millisecond
	})
	clock := newTestPresetClock(mux)
	states := make(chan DnsResolverHealthState, 16)
	removeCallback := mux.AddDnsResolverHealthCallback(func(health DnsResolverHealth) {
		states <- health.State
	})
	defer removeCallback()
	var primaryDown atomic.Bool
	primary := newTestPresetResolver(t, func(host string, question dnsmessage.Question) (testingDohPolicyAnswer, bool) {
		if primaryDown.Load() {
			return testingDohPolicyAnswer{}, false
		}
		return testingDohNoError(-1, netip.MustParseAddr("198.51.100.95")), true
	})
	primary.installPrimary(mux)
	mux.SetTunnelReady(func() bool { return true })

	testPresetQuery(t, mux, rec, "cached.example", dnsmessage.TypeA, 0x0f01)
	got := mux.DnsResolverHealth()
	if got.State != DnsResolverHealthOk || !got.LastSuccessTime.Equal(clock.now()) || got.ConsecutiveFailureCount != 0 {
		t.Fatalf("health after a fresh answer = %+v", got)
	}
	select {
	case state := <-states:
		if state != DnsResolverHealthOk {
			t.Fatalf("callback state = %s, expected ok", state)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the health callback did not run")
	}

	primaryDown.Store(true)
	generation := mux.tunnelDohGeneration.Load()
	testPresetQuery(t, mux, rec, "x1.example", dnsmessage.TypeA, 0x0f02)
	if got := mux.DnsResolverHealth(); got.ConsecutiveFailureCount != 1 || got.State != DnsResolverHealthOk {
		t.Fatalf("health after a failure = %+v", got)
	}
	// a cache hit answers, but proves nothing either way
	testPresetQuery(t, mux, rec, "cached.example", dnsmessage.TypeA, 0x0f03)
	if got := mux.DnsResolverHealth(); got.ConsecutiveFailureCount != 1 {
		t.Fatalf("a cache hit moved the health: %+v", got)
	}
	// an answer served stale is a failure; a canceled resolution is nothing
	mux.recordResolverEvidence(generation, clock.now(), true, false, dohLookupResult{addrs: []netip.Addr{netip.MustParseAddr("198.51.100.96")}, authoritative: true, stale: true})
	mux.recordResolverEvidence(generation, clock.now(), true, true, dohLookupResult{})
	mux.recordResolverEvidence(generation, clock.now(), false, false, dohLookupResult{})
	if got := mux.DnsResolverHealth(); got.ConsecutiveFailureCount != 2 {
		t.Fatalf("health after a stale answer, a canceled and an unready failure = %+v, expected 2 failures", got)
	}
	testPresetQuery(t, mux, rec, "x2.example", dnsmessage.TypeA, 0x0f04)
	if !waitForCondition(5*time.Second, testPresetHealthIs(mux, DnsResolverHealthFailing)) {
		t.Fatalf("health = %+v after three failures, expected failing", mux.DnsResolverHealth())
	}

	// a settings swap starts over, and the old generation's evidence is dropped
	settings := DefaultUpgradeMuxSettings()
	settings.Dns.Resolver = &DnsResolverSettings{EnableRemoteDoh: true, RemoteDohUrlsIpv4: []string{testPresetDohUrl}}
	settings.Dns.ResolverFailingMinCount = 3
	settings.Dns.ResolverFailingMinDuration = 0
	settings.Dns.ResolveTimeout = 300 * time.Millisecond
	mux.SetSettings(settings)
	primary.installPrimary(mux)
	if got := mux.DnsResolverHealth(); got.State != DnsResolverHealthCold || got.ConsecutiveFailureCount != 0 || !got.LastSuccessTime.IsZero() {
		t.Fatalf("health after a settings swap = %+v, expected cold", got)
	}
	mux.recordResolverEvidence(generation, clock.now(), true, false, dohLookupResult{})
	if got := mux.DnsResolverHealth(); got.ConsecutiveFailureCount != 0 {
		t.Fatalf("a retired generation's failure counted: %+v", got)
	}
	primaryDown.Store(false)
	testPresetQuery(t, mux, rec, "y1.example", dnsmessage.TypeA, 0x0f05)
	if got := mux.DnsResolverHealth(); got.State != DnsResolverHealthOk {
		t.Fatalf("health after the new resolver answered = %+v, expected ok", got)
	}
}

// The thresholds of the health state, with explicit times.
func TestResolverHealthStateThresholds(t *testing.T) {
	start := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	var health resolverHealthState
	health.failure(start, start, 3, 10*time.Second)
	health.failure(start.Add(time.Second), start.Add(time.Second), 3, 10*time.Second)
	if health.failing || health.state() != DnsResolverHealthCold {
		t.Fatalf("two failures made the resolver %s", health.state())
	}
	health.failure(start.Add(2*time.Second), start.Add(9*time.Second), 3, 10*time.Second)
	if health.failing {
		t.Fatal("three failures over nine seconds made the resolver failing")
	}
	// a slow failure that started before the run moves its start back
	health.failure(start.Add(-2*time.Second), start.Add(8*time.Second), 3, 10*time.Second)
	if !health.failing || health.failureCount != 4 || !health.failureRunStart.Equal(start.Add(-2*time.Second)) {
		t.Fatalf("health = %+v, expected failing from the earliest start", health)
	}
	health.failedOver = true
	if health.state() != DnsResolverHealthFailedOver {
		t.Fatalf("state = %s, expected failedOver", health.state())
	}
	health.success(start.Add(20 * time.Second))
	if health.failing || health.failedOver || health.failureCount != 0 || health.state() != DnsResolverHealthOk {
		t.Fatalf("health after a success = %+v", health)
	}

	var disabled resolverHealthState
	for i := range 10 {
		disabled.failure(start, start.Add(time.Duration(i)*time.Minute), 0, 0)
	}
	if disabled.failing {
		t.Fatal("a non-positive count made the resolver failing")
	}
	var countOnly resolverHealthState
	countOnly.failure(start, start, 2, 0)
	countOnly.failure(start, start, 2, 0)
	if !countOnly.failing {
		t.Fatal("a non-positive duration did not let the count decide")
	}
}

// The probe (C6) asks every url of the resolver once and reports each one's
// response as the server sent it, leaving the answer cache, the reverse index,
// the server scores, the health and the count untouched.
func TestUpgradeMuxProbeDnsResolverReportsEachUrl(t *testing.T) {
	urls := []string{
		"https://192.0.2.101/dns-query",
		"https://192.0.2.102/dns-query",
		"https://192.0.2.103/dns-query",
		"https://192.0.2.104/dns-query",
		"https://192.0.2.105/abc123def0/dns-query",
	}
	mux, _ := newTestPresetMux(t, func(settings *UpgradeMuxSettings) {
		settings.Dns.Resolver.RemoteDohUrlsIpv4 = urls
		settings.Dns.ResolveTimeout = 5 * time.Second
	})
	answers := map[string]testingDohPolicyAnswer{
		"192.0.2.101": {rcode: dnsmessage.RCodeRefused, ede: -1},
		"192.0.2.102": {rcode: dnsmessage.RCodeServerFailure, ede: -1},
		"192.0.2.103": testingDohNxdomain(17),
		"192.0.2.104": testingDohNoError(-1, netip.IPv4Unspecified()),
	}
	resolver := newTestPresetResolver(t, func(host string, question dnsmessage.Question) (testingDohPolicyAnswer, bool) {
		answer, ok := answers[host]
		return answer, ok
	})
	resolver.installPrimary(mux)

	results := mux.ProbeDnsResolver(context.Background(), "canary.example")
	if len(results) != len(urls) {
		t.Fatalf("%d probe results, expected one per url: %+v", len(results), results)
	}
	for i, result := range results {
		if result.Url != urls[i] {
			t.Fatalf("result %d is for %s, expected %s", i, result.Url, urls[i])
		}
	}
	if r := results[0]; !r.Answered || r.RCode != dnsmessage.RCodeRefused || r.Err != nil {
		t.Fatalf("REFUSED probe = %+v", r)
	}
	if r := results[1]; !r.Answered || r.RCode != dnsmessage.RCodeServerFailure {
		t.Fatalf("SERVFAIL probe = %+v", r)
	}
	if r := results[2]; !r.Answered || r.RCode != dnsmessage.RCodeNameError || !slices.Equal(r.EdeCodes, []uint16{17}) {
		t.Fatalf("NXDOMAIN probe = %+v", r)
	}
	if r := results[3]; !r.Answered || r.RCode != dnsmessage.RCodeSuccess || !slices.Equal(r.Addrs, []netip.Addr{netip.IPv4Unspecified()}) {
		t.Fatalf("address probe = %+v", r)
	}
	if r := results[4]; r.Answered || r.Err == nil || strings.Contains(r.Err.Error(), "abc123def0") || !strings.Contains(r.Err.Error(), "https://192.0.2.105") {
		t.Fatalf("failed probe = %+v, expected an error naming the server without its path", r)
	}
	if len(resolver.asks()) != len(urls) {
		t.Fatalf("questions = %v, expected one per url", resolver.asks())
	}

	if entry := testingDohCacheEntry(mux.mux.Tun().DohCache(), "A", "canary.example"); entry != nil {
		t.Fatal("the probe wrote the answer cache")
	}
	if count := mux.reverse.count(); count != 0 {
		t.Fatalf("the probe recorded %d reverse index entries", count)
	}
	if scores := mux.DnsServerScores(); len(scores) != 0 {
		t.Fatalf("the probe recorded server scores %v", scores)
	}
	if got := mux.DnsResolverHealth(); got.State != DnsResolverHealthCold || got.ConsecutiveFailureCount != 0 || got.BlockedAnswerCount != 0 {
		t.Fatalf("the probe moved the health or the count: %+v", got)
	}
}

// Fast DNS on connect with a preset (C7): with FallbackUntilTunnelReady the host
// fallback answers while the tunnel is not ready and never once it is; the
// preset is never asked on the host path.
func TestUpgradeMuxFallbackOnlyDuringTheConnectWindow(t *testing.T) {
	mux, rec := newTestPresetMux(t, func(settings *UpgradeMuxSettings) {
		settings.Dns.Fallback = &DnsResolverSettings{EnableLocalDoh: true, LocalDohUrlsIpv4: []string{testFallbackDohUrl}}
		settings.Dns.FallbackUntilTunnelReady = true
		settings.Dns.LocalFallbackTimeout = 50 * time.Millisecond
		settings.Dns.ColdLocalFallbackTimeout = 10 * time.Millisecond
		settings.Dns.ResolveTimeout = time.Second
	})
	// a tunnel resolver that never answers
	mux.mux.Tun().DohCache().remoteClient.httpClient.Transport = dohRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	fallbackAddr := netip.MustParseAddr("198.51.100.93")
	fallback := newTestPresetResolver(t, func(string, dnsmessage.Question) (testingDohPolicyAnswer, bool) {
		return testingDohNoError(-1, fallbackAddr), true
	})
	mux.fallbackDohCache.Load().localClient.httpClient.Transport = dohRoundTripperFunc(fallback.roundTrip)
	var ready atomic.Bool
	mux.SetTunnelReady(ready.Load)

	reply := testPresetQuery(t, mux, rec, "w1.example", dnsmessage.TypeA, 0x1001)
	if _, addrs := testPresetReplyAnswers(t, reply); !slices.Equal(addrs, []netip.Addr{fallbackAddr}) {
		t.Fatalf("connect window reply = %v, expected the fallback's %s", addrs, fallbackAddr)
	}

	ready.Store(true)
	if reply := testPresetQuery(t, mux, rec, "w2.example", dnsmessage.TypeA, 0x1002); reply != nil {
		t.Fatalf("an answer once the tunnel was ready, with a tunnel resolver that never answers: %x", reply)
	}
	reply = testPresetQuery(t, mux, rec, "w3.example", dnsTypeHttps, 0x1003)
	if rcode, _ := testPresetReplyAnswers(t, reply); rcode != dnsmessage.RCodeServerFailure {
		t.Fatalf("a forwarded query once the tunnel was ready = %v, expected SERVFAIL from the tunnel alone", rcode)
	}
	if fallback.asked("w2.example") || fallback.asked("w3.example") {
		t.Fatalf("the fallback raced once the tunnel was ready: %v", fallback.asks())
	}
	for _, question := range fallback.asks() {
		if !strings.HasPrefix(question, "192.0.2.93 ") {
			t.Fatalf("the host path asked %s; only the fallback's own server may be asked there", question)
		}
	}
}

// Without FallbackUntilTunnelReady the fallback races every query, as before,
// tunnel ready or not.
func TestUpgradeMuxFallbackWithoutTheFlagRacesAsBefore(t *testing.T) {
	mux, rec := newTestPresetMux(t, func(settings *UpgradeMuxSettings) {
		settings.Dns.Fallback = &DnsResolverSettings{EnableLocalDoh: true, LocalDohUrlsIpv4: []string{testFallbackDohUrl}}
		settings.Dns.LocalFallbackTimeout = 50 * time.Millisecond
		settings.Dns.ColdLocalFallbackTimeout = 10 * time.Millisecond
		settings.Dns.ResolveTimeout = time.Second
	})
	mux.mux.Tun().DohCache().remoteClient.httpClient.Transport = dohRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	fallbackAddr := netip.MustParseAddr("198.51.100.94")
	fallback := newTestPresetResolver(t, func(string, dnsmessage.Question) (testingDohPolicyAnswer, bool) {
		return testingDohNoError(-1, fallbackAddr), true
	})
	mux.fallbackDohCache.Load().localClient.httpClient.Transport = dohRoundTripperFunc(fallback.roundTrip)
	mux.SetTunnelReady(func() bool { return true })

	reply := testPresetQuery(t, mux, rec, "v1.example", dnsmessage.TypeA, 0x1101)
	if _, addrs := testPresetReplyAnswers(t, reply); !slices.Equal(addrs, []netip.Addr{fallbackAddr}) {
		t.Fatalf("reply = %v, expected the fallback's %s as before", addrs, fallbackAddr)
	}
}

// A preset whose pins the exit refuses (an exit network blocking the provider,
// or pins gone stale, 6.3): over the mux's real tun path every dial goes to a
// pin and nothing else, no plain dns ever leaves, and a family preset (no
// Failover) stays failing with no answer.
func TestUpgradeMuxRefusedPinsFailClosedWithoutPlaintext(t *testing.T) {
	pins := []netip.Addr{netip.MustParseAddr("192.0.2.121"), netip.MustParseAddr("2001:db8::121")}
	mux, rec, egress := newTestRefusingPinsMux(t, pins, nil)
	mux.SetTunnelReady(func() bool { return true })

	for i, name := range []string{"p1.example", "p2.example", "p3.example"} {
		if reply := testPresetQuery(t, mux, rec, name, dnsmessage.TypeA, uint16(0x1201+i)); reply != nil {
			t.Fatalf("%s: a refused preset answered", name)
		}
	}
	if !waitForCondition(5*time.Second, testPresetHealthIs(mux, DnsResolverHealthFailing)) {
		t.Fatalf("health = %+v, expected failing", mux.DnsResolverHealth())
	}
	egress.check(t, pins)
}

// The same with an ad-blocking preset's Failover: the mux moves to the failover,
// then back when the exit lets the preset through again.
func TestUpgradeMuxRefusedPinsFailOverAndBack(t *testing.T) {
	pins := []netip.Addr{netip.MustParseAddr("192.0.2.122"), netip.MustParseAddr("2001:db8::122")}
	var healed atomic.Bool
	presetAddr := netip.MustParseAddr("198.51.100.122")
	mux, rec, egress := newTestRefusingPinsMux(t, pins, func(settings *UpgradeMuxSettings) {
		settings.Dns.Failover = &DnsResolverSettings{EnableRemoteDoh: true, RemoteDohUrlsIpv4: []string{testFailoverDohUrl}}
		settings.Dns.FailoverProbeInterval = 20 * time.Millisecond
	})
	// once healed, the preset answers; until then its requests take the real
	// tun path to the refusing exit
	cache := mux.mux.Tun().DohCache()
	refusingTransport := cache.remoteClient.httpClient.Transport
	healedPreset := newTestPresetResolver(t, func(string, dnsmessage.Question) (testingDohPolicyAnswer, bool) {
		return testingDohNoError(-1, presetAddr), true
	})
	cache.remoteClient.httpClient.Transport = dohRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if healed.Load() {
			return healedPreset.roundTrip(request)
		}
		return refusingTransport.RoundTrip(request)
	})
	failoverAddr := netip.MustParseAddr("198.51.100.123")
	failover := newTestPresetResolver(t, func(string, dnsmessage.Question) (testingDohPolicyAnswer, bool) {
		return testingDohNoError(-1, failoverAddr), true
	})
	failover.installFailover(t, mux)
	mux.SetTunnelReady(func() bool { return true })

	for i, name := range []string{"q1.example", "q2.example", "q3.example"} {
		testPresetQuery(t, mux, rec, name, dnsmessage.TypeA, uint16(0x1301+i))
	}
	if !waitForCondition(10*time.Second, testPresetHealthIs(mux, DnsResolverHealthFailedOver)) {
		t.Fatalf("health = %+v, expected failedOver", mux.DnsResolverHealth())
	}
	reply := testPresetQuery(t, mux, rec, "q4.example", dnsmessage.TypeA, 0x1304)
	if _, addrs := testPresetReplyAnswers(t, reply); !slices.Equal(addrs, []netip.Addr{failoverAddr}) {
		t.Fatalf("failed over reply = %v, expected the failover's", addrs)
	}
	egress.check(t, pins)

	healed.Store(true)
	if !waitForCondition(10*time.Second, testPresetHealthIs(mux, DnsResolverHealthOk)) {
		t.Fatalf("health = %+v after the exit healed, expected ok", mux.DnsResolverHealth())
	}
	reply = testPresetQuery(t, mux, rec, "q5.example", dnsmessage.TypeA, 0x1305)
	if _, addrs := testPresetReplyAnswers(t, reply); !slices.Equal(addrs, []netip.Addr{presetAddr}) {
		t.Fatalf("reply after healing = %v, expected the preset's", addrs)
	}
}

// What the refusing exit saw leave the mux: tcp connection attempts by
// destination, and every plain dns packet.
type testRefusingPinsEgress struct {
	stateLock    sync.Mutex
	destinations map[netip.AddrPort]int
	plainDns     []string
}

// Every connection attempt went to a pin on 443, and no plain dns left.
func (self *testRefusingPinsEgress) check(t *testing.T, pins []netip.Addr) {
	t.Helper()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(self.plainDns) != 0 {
		t.Fatalf("plain dns left the mux: %v", self.plainDns)
	}
	if len(self.destinations) == 0 {
		t.Fatal("no connection attempt left the mux")
	}
	for destination := range self.destinations {
		if destination.Port() != 443 || !slices.Contains(pins, destination.Addr()) {
			t.Fatalf("a connection attempt to %s, expected only the pins %v on 443", destination, pins)
		}
	}
}

// A mux whose resolver is a hostname preset with pins and plain dns lists
// that a lookup could use (remote plain dns disabled, as a preset has it),
// dialing through its own tun into an upstream that refuses every tcp
// connection with a reset, as an exit blocking the provider would.
func newTestRefusingPinsMux(t *testing.T, pins []netip.Addr, configure func(settings *UpgradeMuxSettings)) (*UpgradeMux, *ipMuxRecorder, *testRefusingPinsEgress) {
	t.Helper()
	pinValues := []string{}
	for _, pin := range pins {
		pinValues = append(pinValues, pin.String())
	}
	mux, rec := newTestPresetMux(t, func(settings *UpgradeMuxSettings) {
		settings.Dns.Resolver = &DnsResolverSettings{
			EnableRemoteDoh:    true,
			RemoteDohUrlsIpv4:  []string{"https://" + testPinnedDohHost + "/dns-query"},
			RemoteDohUrlsIpv6:  []string{"https://" + testPinnedDohHost + "/dns-query"},
			RemoteDnsIpv4:      []string{"192.0.2.53"},
			RemoteDnsIpv6:      []string{"2001:db8::53"},
			DohServerHostAddrs: map[string][]string{testPinnedDohHost: pinValues},
		}
		settings.Dns.ResolverFailingMinCount = 2
		settings.Dns.ResolverFailingMinDuration = 0
		settings.Dns.ResolveTimeout = 2 * time.Second
		if configure != nil {
			configure(settings)
		}
	})
	egress := &testRefusingPinsEgress{destinations: map[netip.AddrPort]int{}}
	mux.SetUpstream(func(source TransferPath, provideMode protocol.ProvideMode, packet []byte, timeout time.Duration) bool {
		ipPath, err := ParseIpPath(packet)
		if err != nil {
			return true
		}
		destination, _ := netip.AddrFromSlice(ipPath.DestinationIp)
		destination = destination.Unmap()
		if ipPath.DestinationPort == 53 {
			egress.stateLock.Lock()
			egress.plainDns = append(egress.plainDns, destination.String())
			egress.stateLock.Unlock()
			return true
		}
		if ipPath.Protocol != IpProtocolTcp || !ipPath.Syn || ipPath.Ack {
			return true
		}
		egress.stateLock.Lock()
		egress.destinations[netip.AddrPortFrom(destination, uint16(ipPath.DestinationPort))] += 1
		egress.stateLock.Unlock()
		reset := tcpRstForPolicyReject(packet)
		if reset == nil {
			return true
		}
		resetCopy := append([]byte(nil), reset...)
		MessagePoolReturn(reset)
		go mux.Receive(source, provideMode, ipPath.Reverse(), resetCopy)
		return true
	})
	return mux, rec, egress
}

// Settings without a preset are unchanged (ADBLOCK3RD section 7: inert until a
// resolver setting carries pins, a failover or the flag).
func TestDefaultUpgradeMuxSettingsCarryNoPreset(t *testing.T) {
	settings := DefaultUpgradeMuxSettings()
	if settings.Dns.Failover != nil || settings.Dns.FallbackUntilTunnelReady || settings.Dns.Fallback != nil {
		t.Fatalf("defaults carry a failover %v, a connect-window flag %t or a fallback %v", settings.Dns.Failover, settings.Dns.FallbackUntilTunnelReady, settings.Dns.Fallback)
	}
	if settings.Dns.Resolver.DohServerHostAddrs != nil || DefaultDnsResolverSettings().DohServerHostAddrs != nil || DefaultDnsUpgradeFallbackSettings().DohServerHostAddrs != nil {
		t.Fatal("default resolver settings carry pins")
	}
	if settings.Dns.ResolverFailingMinCount != 4 || settings.Dns.ResolverFailingMinDuration != 10*time.Second || settings.Dns.FailoverProbeInterval != 30*time.Second {
		t.Fatalf("health tunables = %d, %s, %s", settings.Dns.ResolverFailingMinCount, settings.Dns.ResolverFailingMinDuration, settings.Dns.FailoverProbeInterval)
	}
	if pinned := dohPinnedHostAddrs(settings.Dns.Resolver); pinned != nil {
		t.Fatalf("default settings pin %v", pinned)
	}
	// without pins a dial passes through as it was asked
	var dialed []string
	dial := func(ctx context.Context, network string, address string) (net.Conn, error) {
		dialed = append(dialed, network+" "+address)
		return nil, errors.New("synthetic dial refusal")
	}
	dohPinnedDialContext(dohPinnedHostAddrs(settings.Dns.Resolver), nil, dial)(context.Background(), "tcp", "doh.example:443")
	if !slices.Equal(dialed, []string{"tcp doh.example:443"}) {
		t.Fatalf("an unpinned dial became %v", dialed)
	}
}

// The default resolver through the mux, with the tunnel ready and failing
// resolutions, behaves as before: the built-in endpoints' guard holds (a sink
// or an EDE NXDOMAIN is no answer and counts nothing), a forwarded reply keeps
// its OPT record only for a client that sent one, a failing default resolver
// never switches anywhere, and a hostname url without pins is still looked up
// over remote plain dns through the tunnel.
func TestUpgradeMuxWithoutPresetSettingsResolvesAsBefore(t *testing.T) {
	mux, rec := newTestPresetMux(t, func(settings *UpgradeMuxSettings) {
		settings.Dns.Resolver = DefaultUpgradeMuxSettings().Dns.Resolver
		settings.Dns.ResolverFailingMinCount = 2
		settings.Dns.ResolverFailingMinDuration = 0
		settings.Dns.ResolveTimeout = 300 * time.Millisecond
	})
	realAddr := netip.MustParseAddr("198.51.100.30")
	resolver := newTestPresetResolver(t, func(host string, question dnsmessage.Question) (testingDohPolicyAnswer, bool) {
		switch strings.TrimSuffix(question.Name.String(), ".") {
		case "sink.example":
			return testingDohNoError(-1, netip.IPv4Unspecified()), true
		case "ede.example":
			return testingDohNxdomain(17), true
		case "down.example":
			return testingDohPolicyAnswer{}, false
		case "plain-https.example", "edns-https.example":
			// an OPT record as a resolver sends to a query that asked with
			// one, carrying a non-policy extended error
			return testingDohNoError(0), true
		default:
			return testingDohNoError(-1, realAddr), true
		}
	})
	resolver.installPrimary(mux)
	mux.SetTunnelReady(func() bool { return true })

	for i, name := range []string{"sink.example", "ede.example"} {
		if reply := testPresetQuery(t, mux, rec, name, dnsmessage.TypeA, uint16(0x1401+i)); reply != nil {
			t.Fatalf("%s: a built-in endpoint's filtered answer was delivered", name)
		}
	}
	if got := mux.DnsBlockedCount(); got != 0 {
		t.Fatalf("blocked count = %d with the defaults, expected 0", got)
	}
	if _, addrs := testPresetReplyAnswers(t, testPresetQuery(t, mux, rec, "real.example", dnsmessage.TypeA, 0x1403)); !slices.Equal(addrs, []netip.Addr{realAddr}) {
		t.Fatalf("real answer = %v", addrs)
	}
	if names := mux.ServerNames(realAddr.String()); !slices.Contains(names, "real.example") {
		t.Fatalf("ServerNames(%s) = %v", realAddr, names)
	}

	// a forwarded HTTPS reply: OPT only for a client that sent one
	for _, edns := range []bool{false, true} {
		id := uint16(0x1404)
		name := "plain-https.example."
		if edns {
			id = 0x1405
			name = "edns-https.example."
		}
		reply := testingDnsUdpExchange(t, mux, rec, testingDnsClientQuery(t, name, dnsTypeHttps, id, edns))
		if optCount, _ := testingDnsReplyOpt(t, reply, id); optCount != map[bool]int{false: 0, true: 1}[edns] {
			t.Fatalf("edns %t: %d OPT records", edns, optCount)
		}
	}

	for i, name := range []string{"down.example", "down.example"} {
		testPresetQuery(t, mux, rec, name, dnsmessage.TypeA, uint16(0x1406+i))
	}
	if got := mux.DnsResolverHealth(); got.State == DnsResolverHealthFailedOver {
		t.Fatalf("a mux without a failover failed over: %+v", got)
	}
	if _, primaryActive := mux.activeTunnelDohCache(); !primaryActive {
		t.Fatal("the default resolver was replaced")
	}
}
