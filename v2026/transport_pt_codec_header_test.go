package connect

import (
	"bytes"
	"encoding/base32"
	"encoding/base64"
	"net"
	"strings"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// A short carrier header has no application bytes. It cannot create a
// pump or turn the unused part of the caller's scratch array into payload.
func TestDnsCarrierRequiresCompleteHeaders(t *testing.T) {
	name := dnsmessage.MustNewName("AA.carrier.example.")
	tlds := [][]byte{[]byte("carrier.example.")}
	query := dnsmessage.Message{Questions: []dnsmessage.Question{{Name: name, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET}}}
	wire, err := query.Pack()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, data, _, err, other, _ := decodeDnsRequest(wire, [1024]byte{}, tlds); err == nil || data != nil || other {
		t.Errorf("short request header returned %d bytes, other=%v, err=%v", len(data), other, err)
	}
	answer := dnsmessage.Message{Header: dnsmessage.Header{Response: true}, Answers: []dnsmessage.Resource{{
		Header: dnsmessage.ResourceHeader{Name: name, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET},
		Body:   &dnsmessage.TXTResource{TXT: []string{"AA"}},
	}}}
	wire, err = answer.Pack()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, data, err := decodeDnsResponse(wire, [1024]byte{}, tlds); err == nil || data != nil {
		t.Errorf("short response header returned %d bytes, err=%v", len(data), err)
	}
}

// The current question and its optional legacy pump exhaust the request's
// section budget. An ordinary single question still reaches the forwarder.
func TestDnsCarrierBoundsQuestionCount(t *testing.T) {
	q := dnsmessage.Question{Name: dnsmessage.MustNewName("host.example."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}
	for _, count := range []int{1, 3} {
		msg := dnsmessage.Message{}
		for range count {
			msg.Questions = append(msg.Questions, q)
		}
		wire, err := msg.Pack()
		if err != nil {
			t.Fatal(err)
		}
		_, _, _, _, err, other, _ := decodeDnsRequest(wire, [1024]byte{}, [][]byte{[]byte("carrier.example.")})
		if count == 1 && (!other || err != nil) || count == 3 && err == nil {
			t.Errorf("%d questions returned other=%v, err=%v", count, other, err)
		}
	}
}

// Both public constructors must reject a configuration that cannot encode
// even one byte or identify a complete pump header.
func TestDnsCarrierConstructionValidatesWireLimits(t *testing.T) {
	for _, tc := range []string{"no suffix", "long suffix", "long prefix", "zero timeout"} {
		settings := DefaultPacketTranslationSettings()
		settings.DnsTlds = [][]byte{[]byte("carrier.example.")}
		var prefix []byte
		switch tc {
		case "no suffix":
			settings.DnsTlds = nil
		case "long suffix":
			settings.DnsTlds = [][]byte{[]byte(strings.Repeat("a.", 70))}
		case "long prefix":
			prefix = make([]byte, 17)
		case "zero timeout":
			settings.DnsStateTimeout = 0
		}
		conn := newMemoryPacketNetwork().listen(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 53})
		pt, err := NewPacketTranslationWithPrefix(t.Context(), PacketTranslationModeDns, conn, settings, prefix)
		if err == nil {
			pt.Close()
			t.Errorf("%s was accepted", tc)
		}
		conn.Close()
	}
}

// A response may spread its bytes over many TXT strings. Check the cumulative
// boundary and the section-count limit independently of how a parser allocates.
func TestDnsResponseCapacityAndAnswerCount(t *testing.T) {
	const headerBytes = 18
	tld := []byte("carrier.example.")
	key := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(make([]byte, headerBytes))
	name := dnsmessage.MustNewName(key + "." + string(tld))
	for _, tc := range []struct {
		size, count int
		valid       bool
	}{{size: 1024, count: 1, valid: true}, {size: 1025, count: 1}, {size: 32, count: 16, valid: true}, {size: 32, count: 17}} {
		msg := dnsmessage.Message{Header: dnsmessage.Header{Response: true}}
		body := bytes.Repeat([]byte{7}, tc.size)
		for i := range tc.count {
			data := body
			if i != 0 {
				data = nil
			}
			var txt []string
			for len(data) > 0 {
				n := min(len(data), 150)
				txt = append(txt, base64.RawStdEncoding.EncodeToString(data[:n]))
				data = data[n:]
			}
			if len(txt) == 0 {
				txt = []string{""}
			}
			msg.Answers = append(msg.Answers, dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: name, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET}, Body: &dnsmessage.TXTResource{TXT: txt}})
		}
		wire, err := msg.Pack()
		if err != nil {
			t.Fatal(err)
		}
		_, _, _, got, err := decodeDnsResponse(wire, [1024]byte{}, [][]byte{tld})
		if tc.valid && (err != nil || !bytes.Equal(got, body[headerBytes:])) || !tc.valid && (err == nil || got != nil) {
			t.Errorf("size %d answers %d returned %d bytes, err %v", tc.size, tc.count, len(got), err)
		}
	}
}

// A refused client response leaves the same socket and translation usable.
func TestDnsClientServesNextResponseAfterRefusal(t *testing.T) {
	network := newMemoryPacketNetwork()
	tld := []byte("carrier.example.")
	conn := network.listen(dnsCarrierTestAddr("192.0.2.1", 41000))
	client, err := NewPacketTranslation(t.Context(), PacketTranslationModeDns, conn, dnsCarrierTestSettings(tld))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server := network.listen(dnsCarrierTestAddr("192.0.2.2", 53))
	defer server.Close()
	empty := dnsmessage.Message{Header: dnsmessage.Header{Response: true}}
	short, err := empty.Pack()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = server.WriteTo(short, conn.addr); err != nil {
		t.Fatal(err)
	}
	h := dnsCarrierTestHeader(12, 1, 0)
	body := []byte("next client response")
	_, wire, err := encodeDnsResponse(7, h, h, body, [1024]byte{}, tld, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = server.WriteTo(wire, conn.addr); err != nil {
		t.Fatal(err)
	}
	got, _ := readTranslatedPacket(t, "after refused response", client)
	if !bytes.Equal(got, body) || client.EndError() != nil {
		t.Fatalf("received %q, lifecycle %v", got, client.EndError())
	}
}
