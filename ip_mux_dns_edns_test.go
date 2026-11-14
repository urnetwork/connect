package connect

import (
	"context"
	"encoding/binary"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
	"golang.org/x/net/dns/dnsmessage"
)

// RFC 6891 on the mux's forwarded responses. Every DoH query asks with an OPT
// record, so the upstream response carries one; a client whose own query had
// none must not get it (RFC 6891 §7), and a client that sent one keeps it.

// A client query for name, with an OPT record (UDP size 1232) when edns is set.
func testingDnsClientQuery(t *testing.T, name string, qType dnsmessage.Type, id uint16, edns bool) []byte {
	t.Helper()
	message := dnsmessage.Message{
		Header: dnsmessage.Header{ID: id, RecursionDesired: true},
		Questions: []dnsmessage.Question{{
			Name:  dnsmessage.MustNewName(name),
			Type:  qType,
			Class: dnsmessage.ClassINET,
		}},
	}
	if edns {
		var optHeader dnsmessage.ResourceHeader
		if err := optHeader.SetEDNS0(1232, dnsmessage.RCodeSuccess, false); err != nil {
			t.Fatal(err)
		}
		message.Additionals = []dnsmessage.Resource{{Header: optHeader, Body: &dnsmessage.OPTResource{}}}
	}
	query, err := message.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return query
}

// An upstream response to query: the question echoed, one A answer or one
// HTTPS answer with an ipv4hint, and an OPT record, as a resolver answers a
// query that asked with one.
func testingDnsOptResponse(t *testing.T, query []byte) []byte {
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
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: header.ID, Response: true, RecursionAvailable: true})
	if err := b.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	if err := b.Question(question); err != nil {
		t.Fatal(err)
	}
	if err := b.StartAnswers(); err != nil {
		t.Fatal(err)
	}
	rh := dnsmessage.ResourceHeader{Name: question.Name, Class: dnsmessage.ClassINET, TTL: 300}
	switch question.Type {
	case dnsmessage.TypeA:
		if err := b.AResource(rh, dnsmessage.AResource{A: netip.MustParseAddr("198.51.100.30").As4()}); err != nil {
			t.Fatal(err)
		}
	case dnsTypeHttps:
		var r dnsmessage.HTTPSResource
		r.Priority = 1
		r.Target = question.Name
		hint := netip.MustParseAddr("198.51.100.31").As4()
		r.SetParam(dnsmessage.SVCParamIPv4Hint, hint[:])
		if err := b.HTTPSResource(rh, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.StartAdditionals(); err != nil {
		t.Fatal(err)
	}
	var optHeader dnsmessage.ResourceHeader
	if err := optHeader.SetEDNS0(1232, dnsmessage.RCodeSuccess, false); err != nil {
		t.Fatal(err)
	}
	if err := b.OPTResource(optHeader, dnsmessage.OPTResource{}); err != nil {
		t.Fatal(err)
	}
	response, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return response
}

// The OPT records and answers of a client reply, which must parse.
func testingDnsReplyOpt(t *testing.T, reply []byte, id uint16) (optCount int, answerCount int) {
	t.Helper()
	var message dnsmessage.Message
	if err := message.Unpack(reply); err != nil {
		t.Fatalf("reply does not parse: %v", err)
	}
	if message.Header.ID != id || !message.Header.Response {
		t.Fatalf("reply header = %+v, want a response with id %#x", message.Header, id)
	}
	for _, additional := range message.Additionals {
		if additional.Header.Type == dnsmessage.TypeOPT {
			optCount += 1
		}
	}
	return optCount, len(message.Answers)
}

// A mux whose tunnel DoH is one synthetic server answering every query with
// testingDnsOptResponse, with no local fallback.
func newTestingDnsOptMux(t *testing.T) (*UpgradeMux, *ipMuxRecorder) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	rec := &ipMuxRecorder{}
	settings := DefaultUpgradeMuxSettings()
	settings.Dns.Resolver = &DnsResolverSettings{
		EnableRemoteDoh:       true,
		DnsUpgradeMaskAddress: DefaultDnsUpgradeMaskAddress,
		RemoteDohUrlsIpv4:     []string{testCustomDohA},
	}
	settings.Dns.Fallback = nil
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
	mux.mux.Tun().DohCache().remoteClient.httpClient.Transport = dohRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		recorder.Header().Set("Content-Type", "application/dns-message")
		recorder.Write(testingDnsOptResponse(t, testingDohRequestQuery(t, request)))
		response := recorder.Result()
		response.Request = request
		return response, nil
	})
	return mux, rec
}

// Sends a client query over UDP/53 and returns the mux's reply payload.
func testingDnsUdpExchange(t *testing.T, mux *UpgradeMux, rec *ipMuxRecorder, query []byte) []byte {
	t.Helper()
	_, receivedBefore := rec.counts()
	packet := ipOosPacket(&IpPath{
		Version:         4,
		Protocol:        IpProtocolUdp,
		SourceIp:        net.ParseIP("169.254.9.9"),
		SourcePort:      44444,
		DestinationIp:   net.ParseIP("10.0.0.1"),
		DestinationPort: 53,
	}, query)
	if !mux.SendPacket(TransferPath{}, protocol.ProvideMode_Network, packet, 0) {
		t.Fatal("dns query was not claimed")
	}
	if !waitForCondition(5*time.Second, func() bool {
		_, received := rec.counts()
		return receivedBefore < received
	}) {
		t.Fatal("no dns reply")
	}
	packets := rec.receivedPackets()
	_, payload, err := ParseIpPathWithPayload(packets[len(packets)-1])
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

// Whether a query carries an OPT record, read past its question.
func TestDnsQueryHasOpt(t *testing.T) {
	for _, edns := range []bool{false, true} {
		var p dnsmessage.Parser
		if _, err := p.Start(testingDnsClientQuery(t, "edns.example.", dnsmessage.TypeA, 0x0101, edns)); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Question(); err != nil {
			t.Fatal(err)
		}
		if hasOpt := DnsQueryHasOpt(&p); hasOpt != edns {
			t.Errorf("query with opt %t: DnsQueryHasOpt = %t", edns, hasOpt)
		}
	}
}

// The raw forwarding path over UDP (HTTPS) and over the mux's TCP/53 (A): a
// client that sent no OPT gets the response without the upstream's OPT, its
// answer intact; a client that sent one gets the OPT.
func TestUpgradeMuxForwardedResponseHasOptOnlyForEdnsClients(t *testing.T) {
	mux, rec := newTestingDnsOptMux(t)

	udpCases := []struct {
		id   uint16
		edns bool
	}{
		{id: 0x6891, edns: false},
		{id: 0x6892, edns: true},
	}
	for _, c := range udpCases {
		// a distinct name per case: each is its own flight, not coalesced
		name := "plain.example."
		if c.edns {
			name = "edns.example."
		}
		reply := testingDnsUdpExchange(t, mux, rec, testingDnsClientQuery(t, name, dnsTypeHttps, c.id, c.edns))
		optCount, answerCount := testingDnsReplyOpt(t, reply, c.id)
		if answerCount != 1 {
			t.Errorf("udp https edns %t: %d answers, want 1", c.edns, answerCount)
		}
		if want := map[bool]int{false: 0, true: 1}[c.edns]; optCount != want {
			t.Errorf("udp https edns %t: %d OPT records, want %d", c.edns, optCount, want)
		}
	}

	tcpCases := []struct {
		id   uint16
		edns bool
	}{
		{id: 0x6893, edns: false},
		{id: 0x6894, edns: true},
	}
	for _, c := range tcpCases {
		reply := mux.resolveDnsTcpQuery(testingDnsClientQuery(t, "tcp.example.", dnsmessage.TypeA, c.id, c.edns))
		if len(reply) < 2 || binary.BigEndian.Uint16(reply[:2]) != c.id {
			t.Fatalf("tcp a edns %t: reply = %x", c.edns, reply)
		}
		optCount, answerCount := testingDnsReplyOpt(t, reply, c.id)
		if answerCount != 1 {
			t.Errorf("tcp a edns %t: %d answers, want 1", c.edns, answerCount)
		}
		if want := map[bool]int{false: 0, true: 1}[c.edns]; optCount != want {
			t.Errorf("tcp a edns %t: %d OPT records, want %d", c.edns, optCount, want)
		}
	}
}

// The parsed address path synthesizes its reply and adds no OPT record, so a
// client that sent none gets none.
func TestUpgradeMuxParsedResponseHasNoOptForNonEdnsClient(t *testing.T) {
	mux, rec := newTestingDnsOptMux(t)
	const id = 0x6895
	reply := testingDnsUdpExchange(t, mux, rec, testingDnsClientQuery(t, "parsed.example.", dnsmessage.TypeA, id, false))
	optCount, answerCount := testingDnsReplyOpt(t, reply, id)
	if answerCount != 1 || optCount != 0 {
		t.Fatalf("parsed a reply = %d answers, %d OPT records; want 1, 0", answerCount, optCount)
	}
}

// Coalesced responders with and without OPT share one upstream response; each
// gets its own form of it.
func TestUpgradeMuxCoalescedForwardKeepsOptPerResponder(t *testing.T) {
	mux, rec := newTestingDnsOptMux(t)
	const domain = "coalesced.example"
	responder := func(id uint16, srcPort int, edns bool) dnsResponder {
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
			question:    httpsQuestion(domain),
			edns:        edns,
			source:      TransferPath{},
			provideMode: protocol.ProvideMode_Network,
			reverse:     path.Reverse(),
		}
	}
	key := NewDohKey("HTTPS", domain)
	fl, _ := mux.attachDnsResponder(key, responder(0x6896, 40001, false))
	if fl == nil {
		t.Fatal("expected a new flight for the first responder")
	}
	mux.attachDnsResponder(key, responder(0x6897, 40002, true))

	upstream := testingDnsOptResponse(t, testingDnsClientQuery(t, domain+".", dnsTypeHttps, 0, true))
	mux.fanOutRawForward(key, fl, domain, upstream)

	if !waitForCondition(2*time.Second, func() bool { _, received := rec.counts(); return received == 2 }) {
		t.Fatal("coalesced responders did not both get a reply")
	}
	optCounts := map[uint16]int{}
	for _, packet := range rec.receivedPackets() {
		_, payload, err := ParseIpPathWithPayload(packet)
		if err != nil {
			t.Fatal(err)
		}
		id := binary.BigEndian.Uint16(payload[:2])
		optCount, answerCount := testingDnsReplyOpt(t, payload, id)
		if answerCount != 1 {
			t.Errorf("responder %#x: %d answers, want 1", id, answerCount)
		}
		optCounts[id] = optCount
	}
	if optCounts[0x6896] != 0 || optCounts[0x6897] != 1 {
		t.Fatalf("OPT records per responder = %v, want 0 for %#x and 1 for %#x", optCounts, 0x6896, 0x6897)
	}
}

// The DoH cache asks with the name in lower case, so a forwarded response
// names the question in lower case; DnsResponseForQuery gives a query its own
// id and name back, case and all, removes the OPT record for a query without
// one, and refuses a response to another question.
func TestDnsResponseForQueryRestoresTheQueryCase(t *testing.T) {
	cache := testingDohPolicyCache(t, map[string]testingDohPolicyAnswer{
		"192.0.2.71": testingDohNoError(17, netip.MustParseAddr("198.51.100.40")),
	}, []string{testCustomDohA}, nil)
	response, ok := cache.Forward(context.Background(), dnsmessage.TypeA, "ReSoLvE.ExAmPlE")
	if !ok {
		t.Fatal("forward failed")
	}
	var upstream dnsmessage.Message
	if err := upstream.Unpack(response); err != nil {
		t.Fatal(err)
	}
	if got := upstream.Questions[0].Name.String(); got != "resolve.example." {
		t.Fatalf("upstream question = %s, want the lower case name the cache asked", got)
	}

	question := dnsmessage.Question{
		Name:  dnsmessage.MustNewName("ReSoLvE.ExAmPlE."),
		Type:  dnsmessage.TypeA,
		Class: dnsmessage.ClassINET,
	}
	for _, edns := range []bool{false, true} {
		patched, ok := DnsResponseForQuery(response, 0x2020, question, edns)
		if !ok {
			t.Fatalf("edns %t: the response was refused", edns)
		}
		optCount, answerCount := testingDnsReplyOpt(t, patched, 0x2020)
		var message dnsmessage.Message
		if err := message.Unpack(patched); err != nil {
			t.Fatal(err)
		}
		if got := message.Questions[0].Name.String(); got != "ReSoLvE.ExAmPlE." || answerCount != 1 {
			t.Errorf("edns %t: question %s with %d answers, want ReSoLvE.ExAmPlE. with 1", edns, got, answerCount)
		}
		if want := map[bool]int{false: 0, true: 1}[edns]; optCount != want {
			t.Errorf("edns %t: %d OPT records, want %d", edns, optCount, want)
		}
	}

	for _, other := range []dnsmessage.Question{
		{Name: dnsmessage.MustNewName("other.example."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET},
		{Name: dnsmessage.MustNewName("ReSoLvE.ExAmPlE."), Type: dnsmessage.TypeAAAA, Class: dnsmessage.ClassINET},
	} {
		if _, ok := DnsResponseForQuery(response, 0x2020, other, true); ok {
			t.Errorf("a response for resolve.example A was given to %s %v", other.Name, other.Type)
		}
	}
}
