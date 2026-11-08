package connect

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// The policy-filter guard of the DoH race. A resolver that blocks a name by
// policy (Quad9's malware filter, a country withdrawal or court order, a block
// page) answers with an extended dns error (RFC 8914) of 4, 15, 16, 17 or 18,
// or with only 0.0.0.0 or ::. Such a response is no answer and no miss: it
// never ends the race, earns no success credit and is never cached, so a
// filter can neither beat an unfiltered server nor become a five minute
// negative answer. The fake servers here answer by host, with no network.

const (
	testEdeServerA = "192.0.2.61"
	testEdeServerB = "192.0.2.62"
)

// One fake DoH server's response: an RCODE, A/AAAA records, and an extended
// dns error INFO-CODE when ede is non-negative. failure fails the transport
// instead.
type testingDohPolicyAnswer struct {
	rcode   dnsmessage.RCode
	records []netip.Addr
	ede     int
	failure bool
}

func testingDohFailure() testingDohPolicyAnswer {
	return testingDohPolicyAnswer{failure: true}
}

func testingDohNoError(ede int, records ...netip.Addr) testingDohPolicyAnswer {
	return testingDohPolicyAnswer{rcode: dnsmessage.RCodeSuccess, records: records, ede: ede}
}

func testingDohNxdomain(ede int) testingDohPolicyAnswer {
	return testingDohPolicyAnswer{rcode: dnsmessage.RCodeNameError, ede: ede}
}

// The wire response for a query: header, the question echoed, the records of
// the question's family as answers, and an OPT record carrying the extended
// dns error when one is set. The query is taken from a GET ?dns= parameter or
// given directly.
func testingDohPolicyWire(t *testing.T, query []byte, answer testingDohPolicyAnswer) []byte {
	t.Helper()
	var p dnsmessage.Parser
	header, err := p.Start(query)
	if err != nil {
		t.Fatal(err)
	}
	question, err := p.Question()
	if err != nil {
		t.Fatal(err)
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:                 header.ID,
		Response:           true,
		RecursionAvailable: true,
		RCode:              answer.rcode,
	})
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	if err := b.Question(question); err != nil {
		t.Fatal(err)
	}
	if err := b.StartAnswers(); err != nil {
		t.Fatal(err)
	}
	rh := dnsmessage.ResourceHeader{Name: question.Name, Class: dnsmessage.ClassINET, TTL: 60}
	for _, ip := range answer.records {
		switch {
		case question.Type == dnsmessage.TypeA && ip.Is4():
			if err := b.AResource(rh, dnsmessage.AResource{A: ip.As4()}); err != nil {
				t.Fatal(err)
			}
		case question.Type == dnsmessage.TypeAAAA && ip.Is6():
			if err := b.AAAAResource(rh, dnsmessage.AAAAResource{AAAA: ip.As16()}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if 0 <= answer.ede {
		if err := b.StartAdditionals(); err != nil {
			t.Fatal(err)
		}
		var optHeader dnsmessage.ResourceHeader
		if err := optHeader.SetEDNS0(1232, dnsmessage.RCodeSuccess, false); err != nil {
			t.Fatal(err)
		}
		data := binary.BigEndian.AppendUint16(nil, uint16(answer.ede))
		data = append(data, "synthetic"...)
		if err := b.OPTResource(optHeader, dnsmessage.OPTResource{
			Options: []dnsmessage.Option{{Code: 15, Data: data}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	wire, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

// The query a DoH GET request carries.
func testingDohRequestQuery(t *testing.T, request *http.Request) []byte {
	t.Helper()
	query, err := base64.RawURLEncoding.DecodeString(request.URL.Query().Get("dns"))
	if err != nil {
		t.Fatal(err)
	}
	return query
}

// A cache whose remote DoH servers are testEdeServerA and testEdeServerB
// (in the order given), launched together, answering as answers says by host.
func testingDohPolicyCache(t *testing.T, answers map[string]testingDohPolicyAnswer, hosts ...string) *DohCache {
	t.Helper()
	settings := DefaultDohSettings()
	settings.Log = NewNoopLogger()
	settings.DohServerStagger = 0
	dohUrls := []string{}
	for _, host := range hosts {
		dohUrls = append(dohUrls, "https://"+host+"/dns-query")
	}
	settings.DnsResolverSettings = &DnsResolverSettings{
		EnableRemoteDoh:   true,
		RemoteDohUrlsIpv4: dohUrls,
	}
	cache := NewDohCache(settings)
	t.Cleanup(cache.Close)
	cache.remoteClient.httpClient.Transport = dohRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
		answer, ok := answers[request.URL.Hostname()]
		if !ok || answer.failure {
			return nil, errors.New("synthetic resolver failure")
		}
		recorder := httptest.NewRecorder()
		recorder.Header().Set("Content-Type", "application/dns-message")
		recorder.Write(testingDohPolicyWire(t, testingDohRequestQuery(t, request), answer))
		response := recorder.Result()
		response.Request = request
		return response, nil
	})
	return cache
}

func testingDohCacheEntry(cache *DohCache, recordType string, domain string) *DohResult {
	cache.stateLock.Lock()
	defer cache.stateLock.Unlock()
	return cache.queryResultExpiration[NewDohKey(recordType, domain)]
}

// Every DoH query carries an EDNS(0) OPT record, UDP size 1232, DO clear and
// no options, on the parsed address path and on the raw forwarding path:
// Cloudflare and AdGuard attach extended dns errors only to a query that has
// one.
func TestDohQueriesCarryEdnsOpt(t *testing.T) {
	var stateLock sync.Mutex
	opts := map[dnsmessage.Type][]dnsmessage.ResourceHeader{}
	optionCounts := map[dnsmessage.Type][]int{}
	settings := DefaultDohSettings()
	settings.Log = NewNoopLogger()
	settings.DnsResolverSettings = &DnsResolverSettings{
		EnableRemoteDoh:   true,
		RemoteDohUrlsIpv4: []string{"https://" + testEdeServerA + "/dns-query"},
	}
	cache := NewDohCache(settings)
	defer cache.Close()
	cache.remoteClient.httpClient.Transport = dohRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
		query := testingDohRequestQuery(t, request)
		var message dnsmessage.Message
		if err := message.Unpack(query); err != nil {
			t.Fatal(err)
		}
		func() {
			stateLock.Lock()
			defer stateLock.Unlock()
			qType := message.Questions[0].Type
			for _, additional := range message.Additionals {
				if additional.Header.Type == dnsmessage.TypeOPT {
					opts[qType] = append(opts[qType], additional.Header)
					optionCounts[qType] = append(optionCounts[qType], len(additional.Body.(*dnsmessage.OPTResource).Options))
				}
			}
		}()
		recorder := httptest.NewRecorder()
		recorder.Write(testingDohPolicyWire(t, query, testingDohNoError(-1, netip.MustParseAddr("198.51.100.20"))))
		response := recorder.Result()
		response.Request = request
		return response, nil
	})

	if addrs, authoritative := cache.QueryResult(context.Background(), "A", "opt.example"); !authoritative || len(addrs) != 1 {
		t.Fatalf("address query = %v %t", addrs, authoritative)
	}
	if _, ok := cache.Forward(context.Background(), dnsTypeHttps, "opt.example"); !ok {
		t.Fatal("raw forward failed")
	}
	stateLock.Lock()
	defer stateLock.Unlock()
	for _, qType := range []dnsmessage.Type{dnsmessage.TypeA, dnsTypeHttps} {
		headers := opts[qType]
		if len(headers) != 1 {
			t.Errorf("%v query carried %d OPT records, want 1", qType, len(headers))
			continue
		}
		if headers[0].Class != 1232 || headers[0].DNSSECAllowed() || optionCounts[qType][0] != 0 {
			t.Errorf("%v query OPT = size %d, DO %t, %d options; want 1232, false, 0",
				qType, headers[0].Class, headers[0].DNSSECAllowed(), optionCounts[qType][0])
		}
	}
}

// Each policy shape, with the other server failing, is a SERVFAIL (no answer,
// not authoritative), leaves nothing in the cache and earns the filtering
// server no success credit. Before the guard, the Quad9-style NXDOMAIN with
// EDE 17 became an authoritative miss cached for MissExpiration, and the
// 0.0.0.0 answers became the answer.
func TestDohPolicyFilteredResponseIsNeverAnswer(t *testing.T) {
	cases := []struct {
		name       string
		recordType string
		answer     testingDohPolicyAnswer
	}{
		{name: "nxdomain ede 17 filtered (quad9)", recordType: "A", answer: testingDohNxdomain(17)},
		{name: "nxdomain ede 15 blocked", recordType: "A", answer: testingDohNxdomain(15)},
		{name: "nxdomain ede 16 censored", recordType: "A", answer: testingDohNxdomain(16)},
		{name: "nxdomain ede 18 prohibited", recordType: "A", answer: testingDohNxdomain(18)},
		{name: "0.0.0.0 ede 16 (cloudflare 1.1.1.2)", recordType: "A", answer: testingDohNoError(16, netip.MustParseAddr("0.0.0.0"))},
		{name: "forged block page ede 4", recordType: "A", answer: testingDohNoError(4, netip.MustParseAddr("198.51.100.21"))},
		{name: "0.0.0.0 without ede (control d)", recordType: "A", answer: testingDohNoError(-1, netip.MustParseAddr("0.0.0.0"))},
		{name: ":: without ede", recordType: "AAAA", answer: testingDohNoError(-1, netip.MustParseAddr("::"))},
		{name: "refused ede 16 (opendns, france)", recordType: "A", answer: testingDohPolicyAnswer{rcode: dnsmessage.RCodeRefused, ede: 16}},
	}
	for _, c := range cases {
		cache := testingDohPolicyCache(t, map[string]testingDohPolicyAnswer{
			testEdeServerA: c.answer,
			testEdeServerB: testingDohFailure(),
		}, testEdeServerA, testEdeServerB)
		addrs, authoritative := cache.QueryResult(context.Background(), c.recordType, "filtered.example")
		if authoritative || len(addrs) != 0 {
			t.Errorf("%s: lookup = %v authoritative %t, want a SERVFAIL", c.name, addrs, authoritative)
		}
		if entry := testingDohCacheEntry(cache, c.recordType, "filtered.example"); entry != nil {
			t.Errorf("%s: cached %+v, want nothing", c.name, entry)
		}
		if score := cache.remoteClient.stats.scores()["https://"+testEdeServerA+"/dns-query"]; 0 < score {
			t.Errorf("%s: the filtering server earned success credit %f", c.name, score)
		}
	}
}

// A censored NXDOMAIN does not stop an unfiltered server from answering: the
// real address is the answer.
func TestDohCensoredNxdomainLosesToRealAnswer(t *testing.T) {
	realAddr := netip.MustParseAddr("198.51.100.22")
	cache := testingDohPolicyCache(t, map[string]testingDohPolicyAnswer{
		testEdeServerA: testingDohNxdomain(16),
		testEdeServerB: testingDohNoError(-1, realAddr),
	}, testEdeServerA, testEdeServerB)
	addrs, authoritative := cache.QueryResult(context.Background(), "A", "censored.example")
	if !authoritative || !slices.Equal(addrs, []netip.Addr{realAddr}) {
		t.Fatalf("lookup = %v authoritative %t, want %v", addrs, authoritative, realAddr)
	}
}

// An answer of only 0.0.0.0 is no answer: with the other server failing the
// lookup is a SERVFAIL and nothing is cached. Next to a real address the
// 0.0.0.0 is dropped and the real address kept.
func TestDohUnspecifiedAnswerIsNotAccepted(t *testing.T) {
	cache := testingDohPolicyCache(t, map[string]testingDohPolicyAnswer{
		testEdeServerA: testingDohNoError(-1, netip.MustParseAddr("0.0.0.0")),
		testEdeServerB: testingDohFailure(),
	}, testEdeServerA, testEdeServerB)
	if addrs, authoritative := cache.QueryResult(context.Background(), "A", "sink.example"); authoritative || len(addrs) != 0 {
		t.Fatalf("0.0.0.0 lookup = %v authoritative %t, want a SERVFAIL", addrs, authoritative)
	}
	if entry := testingDohCacheEntry(cache, "A", "sink.example"); entry != nil {
		t.Fatalf("0.0.0.0 cached %+v", entry)
	}

	realAddr := netip.MustParseAddr("198.51.100.23")
	cache = testingDohPolicyCache(t, map[string]testingDohPolicyAnswer{
		testEdeServerA: testingDohNoError(-1, netip.MustParseAddr("0.0.0.0"), realAddr),
		testEdeServerB: testingDohFailure(),
	}, testEdeServerA, testEdeServerB)
	if addrs, authoritative := cache.QueryResult(context.Background(), "A", "mixed.example"); !authoritative || !slices.Equal(addrs, []netip.Addr{realAddr}) {
		t.Fatalf("mixed lookup = %v authoritative %t, want only %v", addrs, authoritative, realAddr)
	}
}

// Only the policy codes filter. EDE 29 (Synthesized) on an NXDOMAIN from every
// server stays an authoritative miss, and EDE 6, 7, 9, 21 or 29 on a real
// answer keeps the answer: those codes arrive on genuine responses.
func TestDohIgnoredExtendedErrorsKeepTheAnswer(t *testing.T) {
	cache := testingDohPolicyCache(t, map[string]testingDohPolicyAnswer{
		testEdeServerA: testingDohNxdomain(29),
		testEdeServerB: testingDohNxdomain(29),
	}, testEdeServerA, testEdeServerB)
	if addrs, authoritative := cache.QueryResult(context.Background(), "A", "synthesized.invalid"); !authoritative || len(addrs) != 0 {
		t.Fatalf("ede 29 nxdomain = %v authoritative %t, want an authoritative miss", addrs, authoritative)
	}
	if entry := testingDohCacheEntry(cache, "A", "synthesized.invalid"); entry == nil || !entry.Miss {
		t.Fatalf("ede 29 nxdomain cached %+v, want a miss", entry)
	}

	realAddr := netip.MustParseAddr("198.51.100.24")
	for _, ede := range []int{6, 7, 9, 21, 29} {
		cache := testingDohPolicyCache(t, map[string]testingDohPolicyAnswer{
			testEdeServerA: testingDohNoError(ede, realAddr),
			testEdeServerB: testingDohFailure(),
		}, testEdeServerA, testEdeServerB)
		if addrs, authoritative := cache.QueryResult(context.Background(), "A", "genuine.example"); !authoritative || !slices.Equal(addrs, []netip.Addr{realAddr}) {
			t.Errorf("ede %d answer = %v authoritative %t, want %v", ede, addrs, authoritative, realAddr)
		}
	}
}

// The raw forwarding path (SVCB/HTTPS, and A/AAAA over the mux's TCP path)
// skips a policy-filtered response for the next server, and reports no usable
// response when every server was filtered or failed.
func TestDohRawForwardSkipsPolicyFilteredResponses(t *testing.T) {
	cache := testingDohPolicyCache(t, map[string]testingDohPolicyAnswer{
		testEdeServerA: testingDohNxdomain(17),
		testEdeServerB: testingDohFailure(),
	}, testEdeServerA, testEdeServerB)
	if response, ok := cache.Forward(context.Background(), dnsTypeHttps, "filtered.example"); ok || len(response) != 0 {
		t.Fatalf("filtered raw forward = %d bytes usable %t, want none", len(response), ok)
	}

	cache = testingDohPolicyCache(t, map[string]testingDohPolicyAnswer{
		testEdeServerA: testingDohNxdomain(17),
		testEdeServerB: testingDohNoError(-1),
	}, testEdeServerA, testEdeServerB)
	response, ok := cache.Forward(context.Background(), dnsTypeHttps, "filtered.example")
	if !ok {
		t.Fatal("raw forward found no usable response")
	}
	var p dnsmessage.Parser
	header, err := p.Start(response)
	if err != nil || header.RCode != dnsmessage.RCodeSuccess {
		t.Fatalf("raw forward = rcode %v, err %v; want the unfiltered NOERROR", header.RCode, err)
	}
}

// The usability rule of a raw response, for each shape.
func TestDnsResponseUsableRejectsPolicyFilteredResponses(t *testing.T) {
	query := func(qType dnsmessage.Type) []byte {
		message := dnsmessage.Message{Questions: []dnsmessage.Question{{
			Name:  dnsmessage.MustNewName("usable.example."),
			Type:  qType,
			Class: dnsmessage.ClassINET,
		}}}
		wire, err := message.Pack()
		if err != nil {
			t.Fatal(err)
		}
		return wire
	}
	realAddr := netip.MustParseAddr("198.51.100.25")
	cases := []struct {
		name   string
		qType  dnsmessage.Type
		answer testingDohPolicyAnswer
		usable bool
	}{
		{name: "nxdomain ede 17", qType: dnsmessage.TypeA, answer: testingDohNxdomain(17), usable: false},
		{name: "nxdomain ede 16", qType: dnsTypeHttps, answer: testingDohNxdomain(16), usable: false},
		{name: "0.0.0.0 only", qType: dnsmessage.TypeA, answer: testingDohNoError(-1, netip.MustParseAddr("0.0.0.0")), usable: false},
		{name: ":: only", qType: dnsmessage.TypeAAAA, answer: testingDohNoError(-1, netip.MustParseAddr("::")), usable: false},
		{name: "noerror ede 15", qType: dnsmessage.TypeA, answer: testingDohNoError(15, realAddr), usable: false},
		{name: "refused ede 16", qType: dnsmessage.TypeA, answer: testingDohPolicyAnswer{rcode: dnsmessage.RCodeRefused, ede: 16}, usable: false},
		{name: "nxdomain ede 29", qType: dnsmessage.TypeA, answer: testingDohNxdomain(29), usable: true},
		{name: "nxdomain", qType: dnsmessage.TypeA, answer: testingDohNxdomain(-1), usable: true},
		{name: "answer ede 9", qType: dnsmessage.TypeA, answer: testingDohNoError(9, realAddr), usable: true},
		{name: "mixed 0.0.0.0 and real", qType: dnsmessage.TypeA, answer: testingDohNoError(-1, netip.MustParseAddr("0.0.0.0"), realAddr), usable: true},
		{name: "nodata", qType: dnsTypeHttps, answer: testingDohNoError(-1), usable: true},
	}
	for _, c := range cases {
		if usable := dnsResponseUsable(testingDohPolicyWire(t, query(c.qType), c.answer)); usable != c.usable {
			t.Errorf("%s: usable = %t, want %t", c.name, usable, c.usable)
		}
	}
}

// A lone NXDOMAIN, one server's while every other server failed or was
// policy-filtered, is cached for LoneMissExpiration rather than
// MissExpiration: nothing corroborates it, and a resolver withdrawing from a
// country, or a censor, could answer NXDOMAIN without an extended dns error.
// Two servers' NXDOMAIN is cached for MissExpiration.
func TestDohLoneNxdomainIsCachedBriefly(t *testing.T) {
	cases := []struct {
		name    string
		answers map[string]testingDohPolicyAnswer
		lone    bool
	}{
		{name: "nxdomain and a failure", lone: true, answers: map[string]testingDohPolicyAnswer{
			testEdeServerA: testingDohNxdomain(-1),
			testEdeServerB: testingDohFailure(),
		}},
		{name: "nxdomain and a filtered nxdomain", lone: true, answers: map[string]testingDohPolicyAnswer{
			testEdeServerA: testingDohNxdomain(-1),
			testEdeServerB: testingDohNxdomain(17),
		}},
		{name: "two servers' nxdomain", lone: false, answers: map[string]testingDohPolicyAnswer{
			testEdeServerA: testingDohNxdomain(-1),
			testEdeServerB: testingDohNxdomain(-1),
		}},
	}
	for _, c := range cases {
		cache := testingDohPolicyCache(t, c.answers, testEdeServerA, testEdeServerB)
		if addrs, authoritative := cache.QueryResult(context.Background(), "A", "absent.example"); !authoritative || len(addrs) != 0 {
			t.Errorf("%s: lookup = %v authoritative %t, want an authoritative miss", c.name, addrs, authoritative)
			continue
		}
		entry := testingDohCacheEntry(cache, "A", "absent.example")
		if entry == nil || !entry.Miss {
			t.Errorf("%s: cached %+v, want a miss", c.name, entry)
			continue
		}
		loneExpiration := cache.settings.LoneMissExpiration
		afterLone := entry.Time.Add(loneExpiration + time.Second)
		if valid := entry.Valid(afterLone, cache.settings.MissExpiration); valid == c.lone {
			t.Errorf("%s: miss valid %s after the lookup = %t, want %t", c.name, loneExpiration+time.Second, valid, !c.lone)
		}
		if !entry.Valid(entry.Time.Add(loneExpiration-time.Second), cache.settings.MissExpiration) {
			t.Errorf("%s: miss invalid before %s", c.name, loneExpiration)
		}
	}
}
