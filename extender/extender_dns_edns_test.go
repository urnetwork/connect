// The udp 53 forwarder answers as a resolver does (A6). Every DoH query asks
// with an OPT record, so the upstream answer carries one, and a prober whose
// query had none gets the answer without it (RFC 6891); a prober that sent one
// keeps it. The DoH query asks with the name in lower case, and the prober gets
// its own name back, case and all, as a 0x20 client checks.

package extender

import (
	"context"
	"net/netip"
	"strings"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// newTestDnsQuery with an OPT record (UDP size 1232) when edns is set.
func newTestDnsQueryWithEdns(t *testing.T, id uint16, name string, qType dnsmessage.Type, edns bool) []byte {
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
	queryBytes, err := message.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return queryBytes
}

// newTestDnsAnswer with an OPT record, as a DoH upstream answers a query that
// asked with one.
func newTestDnsAnswerWithOpt(t *testing.T, name string, addr netip.Addr) []byte {
	t.Helper()
	var message dnsmessage.Message
	if err := message.Unpack(newTestDnsAnswer(t, name, dnsmessage.TypeA, addr, 1)); err != nil {
		t.Fatal(err)
	}
	var optHeader dnsmessage.ResourceHeader
	if err := optHeader.SetEDNS0(1232, dnsmessage.RCodeSuccess, false); err != nil {
		t.Fatal(err)
	}
	message.Additionals = append(message.Additionals, dnsmessage.Resource{Header: optHeader, Body: &dnsmessage.OPTResource{}})
	answerBytes, err := message.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return answerBytes
}

// An upstream answer with an OPT record reaches a prober whose query had none
// without the OPT record, its answer and id intact, and reaches a prober whose
// query had one with it.
func TestExtenderForwardedAnswerHasOptOnlyForEdnsQueries(t *testing.T) {
	addr := netip.MustParseAddr("192.0.2.11")
	answerBytes := newTestDnsAnswerWithOpt(t, testForwardName, addr)
	fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.DnsForward = func(context.Context, dnsmessage.Type, string) ([]byte, bool) {
			return answerBytes, true
		}
	})
	conn := newTestDnsClient(t, fixture)

	cases := []struct {
		id   uint16
		edns bool
	}{
		{id: 0x6891, edns: false},
		{id: 0x6892, edns: true},
	}
	for _, c := range cases {
		responseBytes := exchangeTestDnsQuery(t, conn, newTestDnsQueryWithEdns(t, c.id, testForwardName, dnsmessage.TypeA, c.edns))
		var message dnsmessage.Message
		if err := message.Unpack(responseBytes); err != nil {
			t.Fatalf("edns %t: the answer does not parse: %v", c.edns, err)
		}
		if message.Header.ID != c.id || !message.Header.Response {
			t.Fatalf("edns %t: answer header = %+v, expected a response with id %#x", c.edns, message.Header, c.id)
		}
		if len(message.Answers) != 1 {
			t.Fatalf("edns %t: answer count = %d, expected one", c.edns, len(message.Answers))
		}
		if resource, ok := message.Answers[0].Body.(*dnsmessage.AResource); !ok || netip.AddrFrom4(resource.A) != addr {
			t.Fatalf("edns %t: answer = %v, expected %v", c.edns, message.Answers[0].Body, addr)
		}
		optCount := 0
		for _, additional := range message.Additionals {
			if additional.Header.Type == dnsmessage.TypeOPT {
				optCount += 1
			}
		}
		expectedOptCount := 0
		if c.edns {
			expectedOptCount = 1
		}
		if optCount != expectedOptCount {
			t.Errorf("edns %t: %d OPT records, expected %d", c.edns, optCount, expectedOptCount)
		}
	}
}

// A mixed-case query, as a 0x20 client sends, gets its own name back, while
// the upstream answer names the question in lower case as the DoH cache asks
// it. The answer record, which points at the question name, reads the same.
func TestExtenderForwardedAnswerEchoesTheQueryCase(t *testing.T) {
	const mixedCaseName = "ReSoLvE.ExAmPlE."
	addr := netip.MustParseAddr("192.0.2.13")
	lowerCaseAnswerBytes := newTestDnsAnswer(t, strings.ToLower(mixedCaseName), dnsmessage.TypeA, addr, 1)
	forwardNames := make(chan string, 1)
	fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
		settings.DnsForward = func(_ context.Context, _ dnsmessage.Type, name string) ([]byte, bool) {
			select {
			case forwardNames <- name:
			default:
			}
			return lowerCaseAnswerBytes, true
		}
	})
	conn := newTestDnsClient(t, fixture)

	responseBytes := exchangeTestDnsQuery(t, conn, newTestDnsQuery(t, 0x0020, mixedCaseName, dnsmessage.TypeA))
	if name := <-forwardNames; !strings.EqualFold(name, strings.TrimSuffix(mixedCaseName, ".")) {
		t.Fatalf("forwarded name = %q", name)
	}
	var message dnsmessage.Message
	if err := message.Unpack(responseBytes); err != nil {
		t.Fatalf("the answer does not parse: %v", err)
	}
	if message.Header.ID != 0x0020 {
		t.Fatalf("answer id = %#x, expected the query id", message.Header.ID)
	}
	if len(message.Questions) != 1 || message.Questions[0].Name.String() != mixedCaseName {
		t.Fatalf("answer question = %v, expected the query's %s", message.Questions, mixedCaseName)
	}
	if len(message.Answers) != 1 || message.Answers[0].Header.Name.String() != mixedCaseName {
		t.Fatalf("answer records = %v, expected one for %s", message.Answers, mixedCaseName)
	}
	if resource, ok := message.Answers[0].Body.(*dnsmessage.AResource); !ok || netip.AddrFrom4(resource.A) != addr {
		t.Fatalf("answer = %v, expected %v", message.Answers[0].Body, addr)
	}
}
