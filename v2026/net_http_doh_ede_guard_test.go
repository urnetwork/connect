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
// or with only 0.0.0.0 or ::. From a built-in endpoint (the defaults and the
// regional presets) such a response is no answer and no miss: it never ends
// the race, earns no success credit and is never cached, so URnetwork's own
// defaults can neither filter a lookup nor turn a filter into a five minute
// negative answer. From a server the user chose it is the answer, as the user
// asked. The fake servers here answer by host, with no network.

// Built-in endpoints (DefaultDnsResolverSettings), the subject of the guard,
// and visibly synthetic custom servers standing in for a user's own choice.
const (
	testBuiltInDohA     = "https://9.9.9.10/dns-query"
	testBuiltInDohAIpv6 = "https://[2620:fe::10]/dns-query"
	testBuiltInDohB     = "https://1.1.1.1/dns-query"
	testCustomDohA      = "https://192.0.2.71/dns-query"
	testCustomDohB      = "https://192.0.2.72/dns-query"
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
// dns error when one is set.
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

// A cache whose remote DoH servers are the given urls, launched together,
// answering by host as answers says.
func testingDohPolicyCache(t *testing.T, answers map[string]testingDohPolicyAnswer, dohUrlsIpv4 []string, dohUrlsIpv6 []string) *DohCache {
	t.Helper()
	settings := DefaultDohSettings()
	settings.Log = NewNoopLogger()
	settings.DohServerStagger = 0
	settings.DnsResolverSettings = &DnsResolverSettings{
		EnableRemoteDoh:   true,
		RemoteDohUrlsIpv4: dohUrlsIpv4,
		RemoteDohUrlsIpv6: dohUrlsIpv6,
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

// The guard's endpoint set: the default DoH urls and the regional presets, in
// any spelling of the same endpoint, and nothing a user would add, Quad9's
// filtering 9.9.9.9 family included.
func TestDohBuiltInEndpointsAreTheDefaultsAndPresets(t *testing.T) {
	defaults := DefaultDnsResolverSettings()
	presetsIpv4, presetsIpv6 := RegionalControlDohUrls("cn")
	for _, dohUrl := range slices.Concat(
		defaults.RemoteDohUrlsIpv4,
		defaults.RemoteDohUrlsIpv6,
		defaults.LocalDohUrlsIpv4,
		defaults.LocalDohUrlsIpv6,
		presetsIpv4,
		presetsIpv6,
		[]string{
			"https://9.9.9.10:443/dns-query/",
			"HTTPS://1.1.1.1/dns-query",
			"https://[2606:4700:4700:0:0:0:0:1111]/dns-query",
			"https://[2620:fe:0:0:0:0:0:10]/dns-query",
		},
	) {
		if !dohEndpointGuarded(dohUrl) {
			t.Errorf("%s is not a guarded built-in endpoint", dohUrl)
		}
	}
	for _, dohUrl := range []string{
		testCustomDohA,
		"https://9.9.9.9/dns-query",
		"https://[2620:fe::fe]/dns-query",
		"https://149.112.112.112/dns-query",
		"https://94.140.14.14/dns-query",
		"https://8.8.8.8/dns-query",
		"https://208.67.222.222/dns-query",
		"https://9.9.9.10:8443/dns-query",
		"https://9.9.9.10/other-path",
	} {
		if dohEndpointGuarded(dohUrl) {
			t.Errorf("%s is guarded, want a user's own endpoint", dohUrl)
		}
	}
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
		RemoteDohUrlsIpv4: []string{testCustomDohA},
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

// Each policy shape from a built-in endpoint, with the other built-in endpoint
// failing, is a SERVFAIL (no answer, not authoritative), leaves nothing in the
// cache and earns the filtering endpoint no success credit. Without the guard
// the Quad9-style NXDOMAIN with EDE 17 became an authoritative miss cached for
// MissExpiration, and the 0.0.0.0 answers became the answer.
func TestDohPolicyFilteredResponseIsNeverAnswer(t *testing.T) {
	cases := []struct {
		name       string
		recordType string
		answer     testingDohPolicyAnswer
	}{
		{name: "nxdomain ede 17 filtered", recordType: "A", answer: testingDohNxdomain(17)},
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
			"9.9.9.10": c.answer,
			"1.1.1.1":  testingDohFailure(),
		}, []string{testBuiltInDohA, testBuiltInDohB}, nil)
		addrs, authoritative := cache.QueryResult(context.Background(), c.recordType, "filtered.example")
		if authoritative || len(addrs) != 0 {
			t.Errorf("%s: lookup = %v authoritative %t, want a SERVFAIL", c.name, addrs, authoritative)
		}
		if entry := testingDohCacheEntry(cache, c.recordType, "filtered.example"); entry != nil {
			t.Errorf("%s: cached %+v, want nothing", c.name, entry)
		}
		if score := cache.remoteClient.stats.scores()[testBuiltInDohA]; 0 < score {
			t.Errorf("%s: the filtering endpoint earned success credit %f", c.name, score)
		}
	}
}

// A built-in endpoint's censored NXDOMAIN does not stop another built-in
// endpoint from answering: the real address is the answer.
func TestDohCensoredNxdomainLosesToRealAnswer(t *testing.T) {
	realAddr := netip.MustParseAddr("198.51.100.22")
	cache := testingDohPolicyCache(t, map[string]testingDohPolicyAnswer{
		"9.9.9.10": testingDohNxdomain(16),
		"1.1.1.1":  testingDohNoError(-1, realAddr),
	}, []string{testBuiltInDohA, testBuiltInDohB}, nil)
	addrs, authoritative := cache.QueryResult(context.Background(), "A", "censored.example")
	if !authoritative || !slices.Equal(addrs, []netip.Addr{realAddr}) {
		t.Fatalf("lookup = %v authoritative %t, want %v", addrs, authoritative, realAddr)
	}
}

// A built-in endpoint's answer of only 0.0.0.0 is no answer: with the other
// endpoint failing the lookup is a SERVFAIL and nothing is cached. Next to a
// real address the 0.0.0.0 is dropped and the real address kept.
func TestDohUnspecifiedAnswerIsNotAccepted(t *testing.T) {
	cache := testingDohPolicyCache(t, map[string]testingDohPolicyAnswer{
		"9.9.9.10": testingDohNoError(-1, netip.MustParseAddr("0.0.0.0")),
		"1.1.1.1":  testingDohFailure(),
	}, []string{testBuiltInDohA, testBuiltInDohB}, nil)
	if addrs, authoritative := cache.QueryResult(context.Background(), "A", "sink.example"); authoritative || len(addrs) != 0 {
		t.Fatalf("0.0.0.0 lookup = %v authoritative %t, want a SERVFAIL", addrs, authoritative)
	}
	if entry := testingDohCacheEntry(cache, "A", "sink.example"); entry != nil {
		t.Fatalf("0.0.0.0 cached %+v", entry)
	}

	realAddr := netip.MustParseAddr("198.51.100.23")
	cache = testingDohPolicyCache(t, map[string]testingDohPolicyAnswer{
		"9.9.9.10": testingDohNoError(-1, netip.MustParseAddr("0.0.0.0"), realAddr),
		"1.1.1.1":  testingDohFailure(),
	}, []string{testBuiltInDohA, testBuiltInDohB}, nil)
	if addrs, authoritative := cache.QueryResult(context.Background(), "A", "mixed.example"); !authoritative || !slices.Equal(addrs, []netip.Addr{realAddr}) {
		t.Fatalf("mixed lookup = %v authoritative %t, want only %v", addrs, authoritative, realAddr)
	}
}

// Only the policy codes filter. EDE 29 (Synthesized) on an NXDOMAIN from every
// built-in endpoint stays an authoritative miss, and EDE 6, 7, 9, 21 or 29 on a
// real answer keeps the answer: those codes arrive on genuine responses.
func TestDohIgnoredExtendedErrorsKeepTheAnswer(t *testing.T) {
	cache := testingDohPolicyCache(t, map[string]testingDohPolicyAnswer{
		"9.9.9.10": testingDohNxdomain(29),
		"1.1.1.1":  testingDohNxdomain(29),
	}, []string{testBuiltInDohA, testBuiltInDohB}, nil)
	if addrs, authoritative := cache.QueryResult(context.Background(), "A", "synthesized.invalid"); !authoritative || len(addrs) != 0 {
		t.Fatalf("ede 29 nxdomain = %v authoritative %t, want an authoritative miss", addrs, authoritative)
	}
	if entry := testingDohCacheEntry(cache, "A", "synthesized.invalid"); entry == nil || !entry.Miss {
		t.Fatalf("ede 29 nxdomain cached %+v, want a miss", entry)
	}

	realAddr := netip.MustParseAddr("198.51.100.24")
	for _, ede := range []int{6, 7, 9, 21, 29} {
		cache := testingDohPolicyCache(t, map[string]testingDohPolicyAnswer{
			"9.9.9.10": testingDohNoError(ede, realAddr),
			"1.1.1.1":  testingDohFailure(),
		}, []string{testBuiltInDohA, testBuiltInDohB}, nil)
		if addrs, authoritative := cache.QueryResult(context.Background(), "A", "genuine.example"); !authoritative || !slices.Equal(addrs, []netip.Addr{realAddr}) {
			t.Errorf("ede %d answer = %v authoritative %t, want %v", ede, addrs, authoritative, realAddr)
		}
	}
}

// A list of the user's own filtering servers (an AdGuard-style list) answers
// as it answers: a blocked name is 0.0.0.0, an authoritative answer that is
// cached, not a SERVFAIL; an NXDOMAIN with EDE 17 is an authoritative miss
// cached for MissExpiration.
func TestDohCustomFilteringServerAnswersAsItAnswers(t *testing.T) {
	sink := netip.MustParseAddr("0.0.0.0")
	cache := testingDohPolicyCache(t, map[string]testingDohPolicyAnswer{
		"192.0.2.71": testingDohNoError(17, sink),
		"192.0.2.72": testingDohNoError(-1, sink),
	}, []string{testCustomDohA, testCustomDohB}, nil)
	if addrs, authoritative := cache.QueryResult(context.Background(), "A", "ads.example"); !authoritative || !slices.Equal(addrs, []netip.Addr{sink}) {
		t.Fatalf("custom sink lookup = %v authoritative %t, want %v", addrs, authoritative, sink)
	}
	if entry := testingDohCacheEntry(cache, "A", "ads.example"); entry == nil || !slices.Equal(entry.Addrs(), []netip.Addr{sink}) {
		t.Fatalf("custom sink cached %+v, want %v", entry, sink)
	}

	cache = testingDohPolicyCache(t, map[string]testingDohPolicyAnswer{
		"192.0.2.71": testingDohNxdomain(17),
		"192.0.2.72": testingDohFailure(),
	}, []string{testCustomDohA, testCustomDohB}, nil)
	if addrs, authoritative := cache.QueryResult(context.Background(), "A", "blocked.example"); !authoritative || len(addrs) != 0 {
		t.Fatalf("custom ede nxdomain = %v authoritative %t, want an authoritative miss", addrs, authoritative)
	}
	entry := testingDohCacheEntry(cache, "A", "blocked.example")
	if entry == nil || !entry.Miss || !entry.Valid(entry.Time.Add(cache.settings.LoneMissExpiration+time.Second), cache.settings.MissExpiration) {
		t.Fatalf("custom ede nxdomain cached %+v, want a miss for MissExpiration", entry)
	}
}

// In a list mixing built-in and custom servers the guard applies per endpoint:
// the built-in endpoint's filtered response is no answer, the custom server's
// sink answer or miss is.
func TestDohGuardAppliesPerEndpointInAMixedList(t *testing.T) {
	sink := netip.MustParseAddr("0.0.0.0")
	cache := testingDohPolicyCache(t, map[string]testingDohPolicyAnswer{
		"9.9.9.10":   testingDohNxdomain(17),
		"192.0.2.71": testingDohNoError(-1, sink),
	}, []string{testBuiltInDohA, testCustomDohA}, nil)
	if addrs, authoritative := cache.QueryResult(context.Background(), "A", "mixed-sink.example"); !authoritative || !slices.Equal(addrs, []netip.Addr{sink}) {
		t.Errorf("mixed list = %v authoritative %t, want the custom sink %v", addrs, authoritative, sink)
	}

	cache = testingDohPolicyCache(t, map[string]testingDohPolicyAnswer{
		"9.9.9.10":   testingDohNoError(-1, sink),
		"192.0.2.71": testingDohFailure(),
	}, []string{testBuiltInDohA, testCustomDohA}, nil)
	if addrs, authoritative := cache.QueryResult(context.Background(), "A", "builtin-sink.example"); authoritative || len(addrs) != 0 {
		t.Errorf("built-in sink in a mixed list = %v authoritative %t, want a SERVFAIL", addrs, authoritative)
	}

	cache = testingDohPolicyCache(t, map[string]testingDohPolicyAnswer{
		"9.9.9.10":   testingDohFailure(),
		"192.0.2.71": testingDohNxdomain(17),
	}, []string{testBuiltInDohA, testCustomDohA}, nil)
	if addrs, authoritative := cache.QueryResult(context.Background(), "A", "custom-miss.example"); !authoritative || len(addrs) != 0 {
		t.Errorf("custom ede nxdomain in a mixed list = %v authoritative %t, want an authoritative miss", addrs, authoritative)
	}
}

// A user who names a built-in endpoint in their own list gets the guard for
// that endpoint: it is the same endpoint, in any spelling.
func TestDohUserNamedBuiltInEndpointKeepsTheGuard(t *testing.T) {
	cache := testingDohPolicyCache(t, map[string]testingDohPolicyAnswer{
		"1.1.1.1":    testingDohNxdomain(16),
		"192.0.2.71": testingDohFailure(),
	}, []string{"https://1.1.1.1:443/dns-query/", testCustomDohA}, nil)
	if addrs, authoritative := cache.QueryResult(context.Background(), "A", "named.example"); authoritative || len(addrs) != 0 {
		t.Fatalf("user-named built-in endpoint's filter = %v authoritative %t, want a SERVFAIL", addrs, authoritative)
	}
}

// Quad9's filtering 9.9.9.9 is not a built-in endpoint (the default is its
// unfiltered 9.9.9.10), so a user who names it gets its blocks: its NXDOMAIN
// with EDE 17 is an authoritative miss cached for MissExpiration on the parsed
// path, and is forwarded on the raw path.
func TestDohUserNamedFilteringQuad9GetsItsBlocks(t *testing.T) {
	const filteringQuad9 = "https://9.9.9.9/dns-query"
	if dohEndpointGuarded(filteringQuad9) {
		t.Fatalf("%s is guarded, want a user's own endpoint", filteringQuad9)
	}
	answers := map[string]testingDohPolicyAnswer{
		"9.9.9.9":    testingDohNxdomain(17),
		"192.0.2.71": testingDohFailure(),
	}
	cache := testingDohPolicyCache(t, answers, []string{filteringQuad9, testCustomDohA}, nil)
	if addrs, authoritative := cache.QueryResult(context.Background(), "A", "malware.example"); !authoritative || len(addrs) != 0 {
		t.Fatalf("user-named 9.9.9.9 block = %v authoritative %t, want an authoritative miss", addrs, authoritative)
	}
	entry := testingDohCacheEntry(cache, "A", "malware.example")
	if entry == nil || !entry.Miss || !entry.Valid(entry.Time.Add(cache.settings.LoneMissExpiration+time.Second), cache.settings.MissExpiration) {
		t.Fatalf("user-named 9.9.9.9 block cached %+v, want a miss for MissExpiration", entry)
	}

	cache = testingDohPolicyCache(t, answers, []string{filteringQuad9, testCustomDohA}, nil)
	response, ok := cache.Forward(context.Background(), dnsTypeHttps, "malware.example")
	if !ok {
		t.Fatal("user-named 9.9.9.9 block was not forwarded")
	}
	var p dnsmessage.Parser
	if header, err := p.Start(response); err != nil || header.RCode != dnsmessage.RCodeNameError {
		t.Fatalf("user-named 9.9.9.9 raw forward = rcode %v, err %v; want its NXDOMAIN", header.RCode, err)
	}
}

// The raw forwarding path (SVCB/HTTPS, and A/AAAA over the mux's TCP path)
// skips a built-in endpoint's policy-filtered response for the next server,
// and reports no usable response when every server was filtered or failed. A
// custom server's filtered response is forwarded as it is.
func TestDohRawForwardSkipsPolicyFilteredResponses(t *testing.T) {
	cache := testingDohPolicyCache(t, map[string]testingDohPolicyAnswer{
		"9.9.9.10": testingDohNxdomain(17),
		"1.1.1.1":  testingDohFailure(),
	}, []string{testBuiltInDohA, testBuiltInDohB}, nil)
	if response, ok := cache.Forward(context.Background(), dnsTypeHttps, "filtered.example"); ok || len(response) != 0 {
		t.Fatalf("filtered raw forward = %d bytes usable %t, want none", len(response), ok)
	}

	cache = testingDohPolicyCache(t, map[string]testingDohPolicyAnswer{
		"9.9.9.10": testingDohNxdomain(17),
		"1.1.1.1":  testingDohNoError(-1),
	}, []string{testBuiltInDohA, testBuiltInDohB}, nil)
	response, ok := cache.Forward(context.Background(), dnsTypeHttps, "filtered.example")
	if !ok {
		t.Fatal("raw forward found no usable response")
	}
	var p dnsmessage.Parser
	header, err := p.Start(response)
	if err != nil || header.RCode != dnsmessage.RCodeSuccess {
		t.Fatalf("raw forward = rcode %v, err %v; want the unfiltered NOERROR", header.RCode, err)
	}

	cache = testingDohPolicyCache(t, map[string]testingDohPolicyAnswer{
		"192.0.2.71": testingDohNxdomain(17),
		"192.0.2.72": testingDohFailure(),
	}, []string{testCustomDohA, testCustomDohB}, nil)
	response, ok = cache.Forward(context.Background(), dnsTypeHttps, "custom.example")
	if !ok {
		t.Fatal("custom filtered raw forward was refused")
	}
	if header, err := p.Start(response); err != nil || header.RCode != dnsmessage.RCodeNameError {
		t.Fatalf("custom raw forward = rcode %v, err %v; want its NXDOMAIN", header.RCode, err)
	}
}

// The policy classification of a raw response, for each shape, and the
// RCODE-only usability rule beside it.
func TestDnsResponseFilteredClassifiesPolicyResponses(t *testing.T) {
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
		name     string
		qType    dnsmessage.Type
		answer   testingDohPolicyAnswer
		filtered bool
		usable   bool
	}{
		{name: "nxdomain ede 17", qType: dnsmessage.TypeA, answer: testingDohNxdomain(17), filtered: true, usable: true},
		{name: "nxdomain ede 16", qType: dnsTypeHttps, answer: testingDohNxdomain(16), filtered: true, usable: true},
		{name: "0.0.0.0 only", qType: dnsmessage.TypeA, answer: testingDohNoError(-1, netip.MustParseAddr("0.0.0.0")), filtered: true, usable: true},
		{name: ":: only", qType: dnsmessage.TypeAAAA, answer: testingDohNoError(-1, netip.MustParseAddr("::")), filtered: true, usable: true},
		{name: "noerror ede 15", qType: dnsmessage.TypeA, answer: testingDohNoError(15, realAddr), filtered: true, usable: true},
		{name: "refused ede 16", qType: dnsmessage.TypeA, answer: testingDohPolicyAnswer{rcode: dnsmessage.RCodeRefused, ede: 16}, filtered: true, usable: false},
		{name: "nxdomain ede 29", qType: dnsmessage.TypeA, answer: testingDohNxdomain(29), filtered: false, usable: true},
		{name: "nxdomain", qType: dnsmessage.TypeA, answer: testingDohNxdomain(-1), filtered: false, usable: true},
		{name: "answer ede 9", qType: dnsmessage.TypeA, answer: testingDohNoError(9, realAddr), filtered: false, usable: true},
		{name: "mixed 0.0.0.0 and real", qType: dnsmessage.TypeA, answer: testingDohNoError(-1, netip.MustParseAddr("0.0.0.0"), realAddr), filtered: false, usable: true},
		{name: "nodata", qType: dnsTypeHttps, answer: testingDohNoError(-1), filtered: false, usable: true},
	}
	for _, c := range cases {
		wire := testingDohPolicyWire(t, query(c.qType), c.answer)
		if filtered := dnsResponseFiltered(wire); filtered != c.filtered {
			t.Errorf("%s: filtered = %t, want %t", c.name, filtered, c.filtered)
		}
		if usable := dnsResponseUsable(wire); usable != c.usable {
			t.Errorf("%s: usable = %t, want %t", c.name, usable, c.usable)
		}
	}
}

// A lone NXDOMAIN, one source's (an endpoint and its other-family sibling
// counting as one source) and that source a built-in endpoint, is cached for
// LoneMissExpiration rather than MissExpiration: nothing corroborates it, and
// a resolver withdrawing from a country, or a censor, could answer NXDOMAIN
// without an extended dns error. Two sources' NXDOMAIN, or a custom server's,
// is cached for MissExpiration.
func TestDohLoneNxdomainIsCachedBriefly(t *testing.T) {
	cases := []struct {
		name        string
		answers     map[string]testingDohPolicyAnswer
		dohUrlsIpv4 []string
		dohUrlsIpv6 []string
		lone        bool
	}{
		{
			name:        "nxdomain and a failure",
			answers:     map[string]testingDohPolicyAnswer{"9.9.9.10": testingDohNxdomain(-1), "1.1.1.1": testingDohFailure()},
			dohUrlsIpv4: []string{testBuiltInDohA, testBuiltInDohB},
			lone:        true,
		},
		{
			name:        "nxdomain and a filtered nxdomain",
			answers:     map[string]testingDohPolicyAnswer{"9.9.9.10": testingDohNxdomain(-1), "1.1.1.1": testingDohNxdomain(17)},
			dohUrlsIpv4: []string{testBuiltInDohA, testBuiltInDohB},
			lone:        true,
		},
		{
			name:        "one operator's nxdomain on both families",
			answers:     map[string]testingDohPolicyAnswer{"9.9.9.10": testingDohNxdomain(-1), "2620:fe::10": testingDohNxdomain(-1)},
			dohUrlsIpv4: []string{testBuiltInDohA},
			dohUrlsIpv6: []string{testBuiltInDohAIpv6},
			lone:        true,
		},
		{
			name:        "two operators' nxdomain",
			answers:     map[string]testingDohPolicyAnswer{"9.9.9.10": testingDohNxdomain(-1), "1.1.1.1": testingDohNxdomain(-1)},
			dohUrlsIpv4: []string{testBuiltInDohA, testBuiltInDohB},
			lone:        false,
		},
		{
			name:        "a custom server's nxdomain and a failure",
			answers:     map[string]testingDohPolicyAnswer{"192.0.2.71": testingDohNxdomain(-1), "192.0.2.72": testingDohFailure()},
			dohUrlsIpv4: []string{testCustomDohA, testCustomDohB},
			lone:        false,
		},
	}
	for _, c := range cases {
		cache := testingDohPolicyCache(t, c.answers, c.dohUrlsIpv4, c.dohUrlsIpv6)
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

// Warm records server stats too: a built-in endpoint's filtered answer to the
// warm query is no success there either, while a custom server's sink answer
// warms as an answer.
func TestDohWarmGuardsBuiltInEndpointsOnly(t *testing.T) {
	sink := netip.MustParseAddr("0.0.0.0")
	cache := testingDohPolicyCache(t, map[string]testingDohPolicyAnswer{
		"9.9.9.10": testingDohNoError(-1, sink),
	}, []string{testBuiltInDohA}, nil)
	if cache.Warm(context.Background(), 1) {
		t.Error("a built-in endpoint's 0.0.0.0 warmed the cache")
	}
	if score := cache.remoteClient.stats.scores()[testBuiltInDohA]; 0 < score {
		t.Errorf("the filtering built-in endpoint earned warm credit %f", score)
	}

	cache = testingDohPolicyCache(t, map[string]testingDohPolicyAnswer{
		"192.0.2.71": testingDohNoError(-1, sink),
	}, []string{testCustomDohA}, nil)
	if !cache.Warm(context.Background(), 1) {
		t.Error("a custom server's 0.0.0.0 did not warm the cache")
	}
}
