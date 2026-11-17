// C20's tests: DohCache.ForwardDnssec sends Forward's query with EDNS0's DO
// bit, returns the raw answer, and takes only an answer that echoes the bit;
// Forward's query and answers stay as they were for a client that never calls
// it. The DoH servers are local httptest servers on the local DoH path, with
// the remote path off and every server launched at once, so nothing waits on
// a stagger and nothing leaves the host.
package connect

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// The DNSSEC types these tests answer with, which dnsmessage does not name.
const (
	testDnsTypeRrsig  dnsmessage.Type = 46
	testDnsTypeNsec   dnsmessage.Type = 47
	testDnsTypeDnskey dnsmessage.Type = 48
	testDnsTypeTlsa   dnsmessage.Type = 52
)

// A DoH server that keeps every query's wire and answers each with the same
// response.
type testDnssecDohServer struct {
	server   *httptest.Server
	response []byte

	stateLock  sync.Mutex
	queryWires [][]byte
}

func newTestDnssecDohServer(t *testing.T, response []byte) *testDnssecDohServer {
	self := &testDnssecDohServer{response: response}
	self.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queryWire, err := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("dns"))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.queryWires = append(self.queryWires, queryWire)
		}()
		w.Header().Set("Content-Type", "application/dns-message")
		w.Write(self.response)
	}))
	t.Cleanup(self.server.Close)
	return self
}

func (self *testDnssecDohServer) queries() [][]byte {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([][]byte{}, self.queryWires...)
}

// A cache whose only DoH servers are the given ones, on the local path.
func newTestDnssecDohCache(t *testing.T, servers ...*testDnssecDohServer) *DohCache {
	settings := DefaultDohSettings()
	// a bound on a broken test, not a wait any test takes
	settings.RequestTimeout = 30 * time.Second
	settings.DohServerStagger = 0
	settings.DnsResolverSettings.EnableRemoteDoh = false
	settings.DnsResolverSettings.EnableLocalDoh = true
	settings.DnsResolverSettings.LocalDohUrlsIpv4 = []string{}
	settings.DnsResolverSettings.LocalDohUrlsIpv6 = []string{}
	for _, server := range servers {
		settings.DnsResolverSettings.LocalDohUrlsIpv4 = append(settings.DnsResolverSettings.LocalDohUrlsIpv4, server.server.URL)
	}
	cache := NewDohCache(settings)
	t.Cleanup(cache.Close)
	return cache
}

// A response to question as a DNSSEC-aware server gives one: a record of the
// question's type and its RRSIG, and an OPT record that echoes the DO bit.
// adjust, when set, changes it before it is packed.
func testDnssecResponse(t *testing.T, question dnsmessage.Question, adjust func(response *dnsmessage.Message)) []byte {
	t.Helper()
	var optHeader dnsmessage.ResourceHeader
	if err := optHeader.SetEDNS0(1232, dnsmessage.RCodeSuccess, true); err != nil {
		t.Fatal(err)
	}
	answerHeader := dnsmessage.ResourceHeader{Name: question.Name, Class: dnsmessage.ClassINET, TTL: 300}
	response := &dnsmessage.Message{
		Header:    dnsmessage.Header{Response: true, RecursionDesired: true, RecursionAvailable: true},
		Questions: []dnsmessage.Question{question},
		Answers: []dnsmessage.Resource{
			{Header: answerHeader, Body: &dnsmessage.UnknownResource{Type: question.Type, Data: bytes.Repeat([]byte{0x5a}, 35)}},
			{Header: answerHeader, Body: &dnsmessage.UnknownResource{Type: testDnsTypeRrsig, Data: bytes.Repeat([]byte{0xa5}, 64)}},
		},
		Additionals: []dnsmessage.Resource{{Header: optHeader, Body: &dnsmessage.OPTResource{}}},
	}
	if adjust != nil {
		adjust(response)
	}
	wire, err := response.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

// The same response from a server that ignores EDNS0: no OPT record.
func testDnssecResponseWithoutOpt(t *testing.T, question dnsmessage.Question) []byte {
	t.Helper()
	return testDnssecResponse(t, question, func(response *dnsmessage.Message) {
		response.Additionals = nil
	})
}

func testDnssecQuestion(t *testing.T, name string, qType dnsmessage.Type) dnsmessage.Question {
	t.Helper()
	dnsName, err := dnsmessage.NewName(name)
	if err != nil {
		t.Fatal(err)
	}
	return dnsmessage.Question{Name: dnsName, Type: qType, Class: dnsmessage.ClassINET}
}

func TestDohForwardDnssecQueryCarriesTheDoBit(t *testing.T) {
	question := testDnssecQuestion(t, "_25._tcp.mx.dnssec.example.", testDnsTypeTlsa)
	response := testDnssecResponse(t, question, nil)
	server := newTestDnssecDohServer(t, response)
	cache := newTestDnssecDohCache(t, server)

	forwarded, usable := cache.ForwardDnssec(context.Background(), testDnsTypeTlsa, "_25._tcp.mx.dnssec.example")
	if !usable {
		t.Fatal("ForwardDnssec refused a signed answer that echoes the DO bit")
	}
	if !bytes.Equal(forwarded, response) {
		t.Fatalf("ForwardDnssec returned %x, and the server answered %x", forwarded, response)
	}

	queries := server.queries()
	if len(queries) != 1 {
		t.Fatalf("the server took %d queries, want 1", len(queries))
	}
	query := queries[0]
	var parser dnsmessage.Parser
	header, err := parser.Start(query)
	if err != nil {
		t.Fatal(err)
	}
	if header.ID != 0 || header.Response || !header.RecursionDesired {
		t.Fatalf("query header %+v, want Forward's: id 0, recursion desired", header)
	}
	questions, err := parser.AllQuestions()
	if err != nil {
		t.Fatal(err)
	}
	if len(questions) != 1 || questions[0] != question {
		t.Fatalf("query questions %v, want %v", questions, question)
	}
	// one question, no answer or authority records, one additional record
	if counts := query[4:12]; !bytes.Equal(counts, []byte{0, 1, 0, 0, 0, 0, 0, 1}) {
		t.Fatalf("query section counts %x", counts)
	}
	if err := parser.SkipAllAnswers(); err != nil {
		t.Fatal(err)
	}
	if err := parser.SkipAllAuthorities(); err != nil {
		t.Fatal(err)
	}
	opt, err := parser.AdditionalHeader()
	if err != nil {
		t.Fatal(err)
	}
	if opt.Type != dnsmessage.TypeOPT || opt.Name.String() != "." || !opt.DNSSECAllowed() {
		t.Fatalf("query's additional record %+v, want an OPT record with the DO bit", opt)
	}
	if extendedRCode := opt.ExtendedRCode(header.RCode); extendedRCode != dnsmessage.RCodeSuccess {
		t.Fatalf("query's extended RCODE %v", extendedRCode)
	}
	// the OPT record whole: the root, type 41, the read cap as payload size,
	// extended RCODE 0, version 0, the DO bit and no options
	wantOpt := []byte{0, 0, 41, byte(maxDohResponseBytes >> 8), byte(maxDohResponseBytes & 0xff), 0, 0, 0x80, 0, 0, 0}
	if !bytes.HasSuffix(query, wantOpt) {
		t.Fatalf("query %x does not end with the OPT record %x", query, wantOpt)
	}
}

func TestDohForwardDnssecAsksTheRootAsDot(t *testing.T) {
	question := testDnssecQuestion(t, ".", testDnsTypeDnskey)
	response := testDnssecResponse(t, question, nil)
	server := newTestDnssecDohServer(t, response)
	cache := newTestDnssecDohCache(t, server)

	forwarded, usable := cache.ForwardDnssec(context.Background(), testDnsTypeDnskey, ".")
	if !usable || !bytes.Equal(forwarded, response) {
		t.Fatalf("ForwardDnssec for the root returned usable %t, %x", usable, forwarded)
	}
	queries := server.queries()
	if len(queries) != 1 {
		t.Fatalf("the server took %d queries, want 1", len(queries))
	}
	var parser dnsmessage.Parser
	if _, err := parser.Start(queries[0]); err != nil {
		t.Fatal(err)
	}
	queryQuestion, err := parser.Question()
	if err != nil {
		t.Fatal(err)
	}
	if queryQuestion != question {
		t.Fatalf("the root's query asked %v, want %v", queryQuestion, question)
	}
}

func TestDohForwardDnssecSkipsAServerThatDropsTheDoBit(t *testing.T) {
	question := testDnssecQuestion(t, "signed.dnssec.example.", dnsmessage.TypeMX)
	plain := newTestDnssecDohServer(t, testDnssecResponseWithoutOpt(t, question))
	signedResponse := testDnssecResponse(t, question, nil)
	signed := newTestDnssecDohServer(t, signedResponse)

	cache := newTestDnssecDohCache(t, plain, signed)
	forwarded, usable := cache.ForwardDnssec(context.Background(), dnsmessage.TypeMX, "signed.dnssec.example")
	if !usable || !bytes.Equal(forwarded, signedResponse) {
		t.Fatalf("ForwardDnssec over a plain and a signing server returned usable %t, %x, want the signing server's %x", usable, forwarded, signedResponse)
	}

	// alone, the plain server's answer is no answer, and the server keeps its
	// standing for the cache's other queries, since its answer is a usable
	// Forward answer
	plainOnly := newTestDnssecDohCache(t, plain)
	if forwarded, usable := plainOnly.ForwardDnssec(context.Background(), dnsmessage.TypeMX, "signed.dnssec.example"); usable || forwarded != nil {
		t.Fatalf("ForwardDnssec took an answer without the DO bit: usable %t, %x", usable, forwarded)
	}
	if score := plainOnly.ServerScores()[plain.server.URL]; score <= 0 {
		t.Fatalf("the plain server's score is %v after a well-formed answer", score)
	}
}

func TestDohForwardDnssecRefusesAnswersItCannotUse(t *testing.T) {
	question := testDnssecQuestion(t, "signed.dnssec.example.", dnsmessage.TypeA)
	otherName := testDnssecQuestion(t, "other.dnssec.example.", dnsmessage.TypeA)
	optWith := func(extendedRCode dnsmessage.RCode, dnssecOk bool) dnsmessage.Resource {
		var optHeader dnsmessage.ResourceHeader
		if err := optHeader.SetEDNS0(1232, extendedRCode, dnssecOk); err != nil {
			t.Fatal(err)
		}
		return dnsmessage.Resource{Header: optHeader, Body: &dnsmessage.OPTResource{}}
	}
	signed := testDnssecResponse(t, question, nil)
	type responseCase struct {
		about    string
		response []byte
		usable   bool
	}
	cases := []responseCase{
		{about: "a signed answer", response: signed, usable: true},
		{about: "a denial with its proof", usable: true, response: testDnssecResponse(t, question, func(response *dnsmessage.Message) {
			response.RCode = dnsmessage.RCodeNameError
			response.Answers = nil
			denialHeader := dnsmessage.ResourceHeader{Name: question.Name, Class: dnsmessage.ClassINET, TTL: 300}
			response.Authorities = []dnsmessage.Resource{
				{Header: denialHeader, Body: &dnsmessage.UnknownResource{Type: testDnsTypeNsec, Data: bytes.Repeat([]byte{0x3c}, 24)}},
				{Header: denialHeader, Body: &dnsmessage.UnknownResource{Type: testDnsTypeRrsig, Data: bytes.Repeat([]byte{0xc3}, 64)}},
			}
		})},
		{about: "no data, signed", usable: true, response: testDnssecResponse(t, question, func(response *dnsmessage.Message) {
			response.Answers = nil
		})},
		{about: "the question in another case", usable: true, response: testDnssecResponse(t, question, func(response *dnsmessage.Message) {
			response.Questions[0].Name = testDnssecQuestion(t, "SIGNED.Dnssec.EXAMPLE.", dnsmessage.TypeA).Name
		})},
		{about: "no OPT record", response: testDnssecResponseWithoutOpt(t, question)},
		{about: "an OPT record without the DO bit", response: testDnssecResponse(t, question, func(response *dnsmessage.Message) {
			response.Additionals = []dnsmessage.Resource{optWith(dnsmessage.RCodeSuccess, false)}
		})},
		{about: "two OPT records", response: testDnssecResponse(t, question, func(response *dnsmessage.Message) {
			response.Additionals = []dnsmessage.Resource{optWith(dnsmessage.RCodeSuccess, true), optWith(dnsmessage.RCodeSuccess, true)}
		})},
		{about: "SERVFAIL", response: testDnssecResponse(t, question, func(response *dnsmessage.Message) {
			response.RCode = dnsmessage.RCodeServerFailure
		})},
		{about: "REFUSED", response: testDnssecResponse(t, question, func(response *dnsmessage.Message) {
			response.RCode = dnsmessage.RCodeRefused
		})},
		{about: "BADVERS, NOERROR in the header and the rest in the OPT record", response: testDnssecResponse(t, question, func(response *dnsmessage.Message) {
			response.Additionals = []dnsmessage.Resource{optWith(dnsmessage.RCode(16), true)}
		})},
		{about: "truncated", response: testDnssecResponse(t, question, func(response *dnsmessage.Message) {
			response.Truncated = true
		})},
		{about: "a query, not a response", response: testDnssecResponse(t, question, func(response *dnsmessage.Message) {
			response.Response = false
		})},
		{about: "another name", response: testDnssecResponse(t, question, func(response *dnsmessage.Message) {
			response.Questions = []dnsmessage.Question{otherName}
		})},
		{about: "another type", response: testDnssecResponse(t, question, func(response *dnsmessage.Message) {
			response.Questions[0].Type = dnsmessage.TypeAAAA
		})},
		{about: "another class", response: testDnssecResponse(t, question, func(response *dnsmessage.Message) {
			response.Questions[0].Class = dnsmessage.ClassCHAOS
		})},
		{about: "two questions", response: testDnssecResponse(t, question, func(response *dnsmessage.Message) {
			response.Questions = append(response.Questions, otherName)
		})},
		{about: "no question", response: testDnssecResponse(t, question, func(response *dnsmessage.Message) {
			response.Questions = nil
		})},
		{about: "cut short, as the read cap cuts a longer one", response: signed[:len(signed)-3]},
		{about: "not dns", response: []byte("not a dns message")},
		{about: "empty", response: []byte{}},
	}
	for _, c := range cases {
		if usable := dnssecResponseUsable(c.response, dnsmessage.TypeA, "signed.dnssec.example"); usable != c.usable {
			t.Errorf("%s: usable %t, want %t", c.about, usable, c.usable)
		}
	}

	root := testDnssecQuestion(t, ".", testDnsTypeDnskey)
	if !dnssecResponseUsable(testDnssecResponse(t, root, nil), testDnsTypeDnskey, "") {
		t.Error("the root's signed DNSKEY answer is refused")
	}
	if dnssecResponseUsable(signed, dnsmessage.TypeA, "") {
		t.Error("a name's answer is taken for the root's")
	}
}

// The circular rule Forward keeps holds for ForwardDnssec: a DoH server's own
// name never goes to a DoH server.
func TestDohForwardDnssecRefusesADohServerName(t *testing.T) {
	question := testDnssecQuestion(t, "doh-server.dnssec.example.", dnsmessage.TypeA)
	server := newTestDnssecDohServer(t, testDnssecResponse(t, question, nil))
	settings := DefaultDohSettings()
	settings.RequestTimeout = 30 * time.Second
	settings.DnsResolverSettings.EnableRemoteDoh = false
	settings.DnsResolverSettings.EnableLocalDoh = true
	settings.DnsResolverSettings.RemoteDohUrlsIpv4 = []string{"https://doh-server.dnssec.example/dns-query"}
	settings.DnsResolverSettings.RemoteDohUrlsIpv6 = []string{}
	settings.DnsResolverSettings.LocalDohUrlsIpv4 = []string{server.server.URL}
	cache := NewDohCache(settings)
	t.Cleanup(cache.Close)

	if forwarded, usable := cache.ForwardDnssec(context.Background(), dnsmessage.TypeA, "doh-server.dnssec.example"); usable || forwarded != nil {
		t.Fatalf("ForwardDnssec asked for a DoH server's own name: usable %t", usable)
	}
	if queries := server.queries(); len(queries) != 0 {
		t.Fatalf("a DoH server's name reached a DoH server %d times", len(queries))
	}
}

func TestDohForwardDnssecAfterCloseAnswersNothing(t *testing.T) {
	question := testDnssecQuestion(t, "closed.dnssec.example.", dnsmessage.TypeA)
	server := newTestDnssecDohServer(t, testDnssecResponse(t, question, nil))
	cache := newTestDnssecDohCache(t, server)
	cache.Close()

	if forwarded, usable := cache.ForwardDnssec(context.Background(), dnsmessage.TypeA, "closed.dnssec.example"); usable || forwarded != nil {
		t.Fatalf("a closed cache forwarded: usable %t", usable)
	}
	if queries := server.queries(); len(queries) != 0 {
		t.Fatalf("a closed cache sent %d queries", len(queries))
	}
}

// Default off (C20): Forward's query is the bytes it always was, with no OPT
// record, before and after the same cache has forwarded with the DO bit.
func TestDohForwardQuerySetsNoDoBit(t *testing.T) {
	question := testDnssecQuestion(t, "plain.example.", dnsmessage.Type(65))
	server := newTestDnssecDohServer(t, testDnssecResponseWithoutOpt(t, question))
	cache := newTestDnssecDohCache(t, server)

	// id 0, recursion desired, one question and no other record; the name
	// plain.example; type HTTPS (65), class IN
	wantQuery := []byte{
		0, 0, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0,
		5, 'p', 'l', 'a', 'i', 'n', 7, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 0,
		0, 65, 0, 1,
	}
	if _, usable := cache.Forward(context.Background(), dnsmessage.Type(65), "plain.example"); !usable {
		t.Fatal("Forward refused a plain answer")
	}
	// the same cache's DNSSEC forward, which this server's answer cannot serve
	if _, usable := cache.ForwardDnssec(context.Background(), dnsmessage.Type(65), "plain.example"); usable {
		t.Fatal("ForwardDnssec took an answer without the DO bit")
	}
	if _, usable := cache.Forward(context.Background(), dnsmessage.Type(65), "plain.example"); !usable {
		t.Fatal("Forward refused a plain answer after a DNSSEC forward")
	}
	queries := server.queries()
	if len(queries) != 3 {
		t.Fatalf("the server took %d queries, want 3", len(queries))
	}
	for _, index := range []int{0, 2} {
		if !bytes.Equal(queries[index], wantQuery) {
			t.Errorf("Forward's query %d is %x, want %x", index, queries[index], wantQuery)
		}
	}
	if bytes.Equal(queries[1], wantQuery) {
		t.Error("ForwardDnssec sent Forward's query")
	}
}

// Default off (C20): Forward returns what it always returned, raw, for answers
// ForwardDnssec refuses: no OPT record, an OPT record without the DO bit and a
// truncated answer; and refuses what it always refused.
func TestDohForwardAnswersAsBeforeDnssec(t *testing.T) {
	question := testDnssecQuestion(t, "plain.example.", dnsmessage.TypeA)
	var optWithoutDo dnsmessage.ResourceHeader
	if err := optWithoutDo.SetEDNS0(1232, dnsmessage.RCodeSuccess, false); err != nil {
		t.Fatal(err)
	}
	usableResponses := [][]byte{
		testDnssecResponseWithoutOpt(t, question),
		testDnssecResponse(t, question, func(response *dnsmessage.Message) {
			response.Additionals = []dnsmessage.Resource{{Header: optWithoutDo, Body: &dnsmessage.OPTResource{}}}
		}),
		testDnssecResponse(t, question, func(response *dnsmessage.Message) {
			response.Additionals = nil
			response.Truncated = true
		}),
		testDnssecResponse(t, question, func(response *dnsmessage.Message) {
			response.Additionals = nil
			response.RCode = dnsmessage.RCodeNameError
			response.Answers = nil
		}),
	}
	for index, response := range usableResponses {
		server := newTestDnssecDohServer(t, response)
		cache := newTestDnssecDohCache(t, server)
		forwarded, usable := cache.Forward(context.Background(), dnsmessage.TypeA, "plain.example")
		if !usable || !bytes.Equal(forwarded, response) {
			t.Errorf("answer %d: Forward returned usable %t, %x, want %x", index, usable, forwarded, response)
		}
	}

	serverFailure := newTestDnssecDohServer(t, testDnssecResponse(t, question, func(response *dnsmessage.Message) {
		response.Additionals = nil
		response.RCode = dnsmessage.RCodeServerFailure
	}))
	if forwarded, usable := newTestDnssecDohCache(t, serverFailure).Forward(context.Background(), dnsmessage.TypeA, "plain.example"); usable || forwarded != nil {
		t.Errorf("Forward took a SERVFAIL: usable %t", usable)
	}
}
