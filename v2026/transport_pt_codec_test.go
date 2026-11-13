package connect

import (
	"encoding/base32"
	mathrand "math/rand"
	"slices"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

func TestDnsRequestEncodeDecode(t *testing.T) {
	var encodeBuf [1024]byte
	var decodeBuf [1024]byte

	tld := []byte("a.dev.")
	tlds := [][]byte{
		[]byte("b.dev."),
		[]byte("c.bar."),
		[]byte("a.dev."),
	}

	for range 32 {
		rlen := 4*1024 + mathrand.Intn(32*1024)
		data := make([]byte, rlen)
		mathrand.Read(data)

		c := 0
		i := 0
		for i < len(data) {

			var header [18]byte
			mathrand.Read(header[0:16])
			header[16] = uint8(c)
			header[17] = 0

			n, encoded, err := encodeDnsRequest(uint16(i), header, data[i:], encodeBuf, tld)
			AssertEqual(t, err, nil)

			id, decodedHeader, decoded, decodedTld, err, otherData, edns := decodeDnsRequest(encoded, decodeBuf, tlds)
			AssertEqual(t, err, nil)
			AssertEqual(t, id, uint16(i))
			AssertEqual(t, data[i:i+n], decoded)
			AssertEqual(t, header, decodedHeader)
			AssertEqual(t, decodedTld, tld)
			AssertEqual(t, otherData, false)
			AssertEqual(t, edns, true)

			i += n
			c += 1
		}

		AssertEqual(t, encodeDnsRequestCount(data, tld), c)
	}

	var header [18]byte
	mathrand.Read(header[0:16])
	header[16] = 0
	header[17] = 0

	_, encoded, err := encodeDnsRequest(uint16(0), header, make([]byte, 0), encodeBuf, tld)
	AssertEqual(t, err, nil)

	id, decodedHeader, decoded, decodedTld, err, otherData, edns := decodeDnsRequest(encoded, decodeBuf, tlds)
	AssertEqual(t, err, nil)
	AssertEqual(t, id, uint16(0))
	AssertEqual(t, make([]byte, 0), decoded)
	AssertEqual(t, header, decodedHeader)
	AssertEqual(t, decodedTld, tld)
	AssertEqual(t, otherData, false)
	AssertEqual(t, edns, true)
}

func TestDnsResponseEncodeDecode(t *testing.T) {
	var encodeBuf [1024]byte
	var decodeBuf [1024]byte

	tld := []byte("a.dev.")
	tlds := [][]byte{
		[]byte("b.dev."),
		[]byte("c.bar."),
		[]byte("a.dev."),
	}

	for range 32 {
		rlen := 4*1024 + mathrand.Intn(32*1024)
		data := make([]byte, rlen)
		mathrand.Read(data)

		c := 0
		i := 0
		for i < len(data) {
			var header [18]byte
			mathrand.Read(header[0:16])
			header[16] = uint8(c)
			header[17] = 0

			n, encoded, err := encodeDnsResponse(uint16(i), header, header, data[i:], encodeBuf, tld, i%2 == 0)
			AssertEqual(t, err, nil)

			id, decodedPumpHeader, decodedHeader, decoded, err := decodeDnsResponse(encoded, decodeBuf, tlds)
			AssertEqual(t, err, nil)
			AssertEqual(t, id, uint16(i))
			AssertEqual(t, data[i:i+n], decoded)
			AssertEqual(t, header, decodedPumpHeader)
			AssertEqual(t, header, decodedHeader)

			i += n
			c += 1
		}

		AssertEqual(t, encodeDnsResponseCount(data, tld), c)
	}

	var header [18]byte
	mathrand.Read(header[0:16])
	header[16] = 0
	header[17] = 0

	_, encoded, err := encodeDnsResponse(uint16(0), header, header, make([]byte, 0), encodeBuf, tld, false)
	AssertEqual(t, err, nil)

	id, decodedPumpHeader, decodedHeader, decoded, err := decodeDnsResponse(encoded, decodeBuf, tlds)
	AssertEqual(t, err, nil)
	AssertEqual(t, id, uint16(0))
	AssertEqual(t, make([]byte, 0), decoded)
	AssertEqual(t, header, decodedPumpHeader)
	AssertEqual(t, header, decodedHeader)
}

// An old client against a new extender: the two-question request with the
// aa bit and no opt record, as the frozen encoder emits it, decodes with
// the same id, header, bytes and tld, is not taken for a forwarder query,
// and is detected as a request without edns. A decoder that enforced
// qdcount 1 would strand every deployed client.
func TestDnsRequestLegacyTwoQuestionShapeDecodes(t *testing.T) {
	var encodeBuf [1024]byte
	var decodeBuf [1024]byte

	tld := []byte("pt.example.")
	tlds := [][]byte{[]byte("other.example."), tld}
	packet := dnsCarrierTestPacket(1000, 0x19)
	c := encodeDnsRequestCount(packet, tld)

	offset := 0
	for i := range c {
		header := dnsCarrierTestHeader(0x28, uint8(c), uint8(i))
		n, encoded, err := legacyEncodeDnsRequest(uint16(0x5000+i), header, packet[offset:], encodeBuf, tld)
		if err != nil {
			t.Fatal(err)
		}
		// the fixture must be the shape that shipped, or the claim is empty
		if qdCount := int(encoded[4])<<8 | int(encoded[5]); qdCount != 2 || encoded[2]&0x04 == 0 || encoded[10] != 0 || encoded[11] != 0 {
			t.Fatalf("fragment %d: the frozen encoder is not the legacy shape", i)
		}

		id, decodedHeader, decoded, decodedTld, err, otherData, edns := decodeDnsRequest(encoded, decodeBuf, tlds)
		if err != nil {
			t.Fatalf("fragment %d: the legacy request was rejected: %v", i, err)
		}
		if otherData {
			t.Fatalf("fragment %d: the legacy request was taken for a forwarder query", i)
		}
		if edns {
			t.Fatalf("fragment %d: edns detected on a request without an opt record", i)
		}
		if id != uint16(0x5000+i) || decodedHeader != header || !slices.Equal(decodedTld, tld) {
			t.Fatalf("fragment %d: id %#x header %x tld %q", i, id, decodedHeader, decodedTld)
		}
		if !slices.Equal(decoded, packet[offset:offset+n]) {
			t.Fatalf("fragment %d: bytes differ", i)
		}
		offset += n
	}
	if offset != len(packet) {
		t.Fatalf("decoded %d bytes, expected %d", offset, len(packet))
	}

	header := dnsCarrierTestHeader(0x29, 0, 0)
	_, encoded, err := legacyEncodeDnsRequest(9, header, nil, encodeBuf, tld)
	if err != nil {
		t.Fatal(err)
	}
	id, decodedHeader, decoded, _, err, otherData, edns := decodeDnsRequest(encoded, decodeBuf, tlds)
	if err != nil || otherData || edns || id != 9 || decodedHeader != header || len(decoded) != 0 {
		t.Fatalf("legacy pump: err %v other %t edns %t id %d header %x data %d", err, otherData, edns, id, decodedHeader, len(decoded))
	}
}

// The edns detection reads the opt record, not the additional count: an
// additional record of another type does not mark a request, and an opt
// record after one does.
func TestDnsRequestEdnsDetectionReadsOnlyOptRecords(t *testing.T) {
	var decodeBuf [1024]byte
	tld := []byte("pt.example.")
	header := dnsCarrierTestHeader(0x31, 0, 0)
	name := base32.HexEncoding.WithPadding(base32.NoPadding).EncodeToString(header[:]) + "." + string(tld)

	build := func(additionals func(b *dnsmessage.Builder)) []byte {
		b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 11})
		if err := b.StartQuestions(); err != nil {
			t.Fatal(err)
		}
		if err := b.Question(dnsmessage.Question{
			Name:  dnsmessage.MustNewName(name),
			Type:  dnsmessage.TypeTXT,
			Class: dnsmessage.ClassINET,
		}); err != nil {
			t.Fatal(err)
		}
		if additionals != nil {
			if err := b.StartAdditionals(); err != nil {
				t.Fatal(err)
			}
			additionals(&b)
		}
		msg, err := b.Finish()
		if err != nil {
			t.Fatal(err)
		}
		return msg
	}
	aRecord := func(b *dnsmessage.Builder) {
		if err := b.AResource(dnsmessage.ResourceHeader{
			Name:  dnsmessage.MustNewName("glue.example."),
			Type:  dnsmessage.TypeA,
			Class: dnsmessage.ClassINET,
		}, dnsmessage.AResource{A: [4]byte{192, 0, 2, 7}}); err != nil {
			t.Fatal(err)
		}
	}
	optRecord := func(b *dnsmessage.Builder) {
		if err := b.OPTResource(dnsEdnsResourceHeader(), dnsmessage.OPTResource{}); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		label       string
		additionals func(b *dnsmessage.Builder)
		edns        bool
	}{
		{label: "none", additionals: nil, edns: false},
		{label: "a record only", additionals: aRecord, edns: false},
		{label: "a record then opt", additionals: func(b *dnsmessage.Builder) { aRecord(b); optRecord(b) }, edns: true},
		{label: "opt only", additionals: optRecord, edns: true},
	}
	for _, c := range cases {
		id, decodedHeader, _, decodedTld, err, otherData, edns := decodeDnsRequest(build(c.additionals), decodeBuf, [][]byte{tld})
		if err != nil || otherData || id != 11 || decodedHeader != header || !slices.Equal(decodedTld, tld) {
			t.Fatalf("%s: err %v other %t id %d header %x tld %q", c.label, err, otherData, id, decodedHeader, decodedTld)
		}
		if edns != c.edns {
			t.Errorf("%s: edns = %t, expected %t", c.label, edns, c.edns)
		}
	}
}

// A response carries the opt record only when the request it is paired
// with carried one, as RFC 6891 requires, and the response without it is
// byte for byte the response of before, so a client from before the shape
// change sees nothing new. Either response decodes on the client.
func TestDnsResponseCarriesEdnsOnlyForEdnsRequests(t *testing.T) {
	var encodeBuf [1024]byte
	var decodeBuf [1024]byte
	tld := []byte("pt.example.")
	pumpHeader := dnsCarrierTestHeader(0x41, 0, 0)
	header := dnsCarrierTestHeader(0x42, 1, 0)
	packet := dnsCarrierTestPacket(300, 0x43)

	n, withEdns, err := encodeDnsResponse(21, pumpHeader, header, packet, encodeBuf, tld, true)
	if err != nil || n != len(packet) {
		t.Fatalf("with edns: n %d err %v", n, err)
	}
	withEdns = slices.Clone(withEdns)
	m, withoutEdns, err := encodeDnsResponse(21, pumpHeader, header, packet, encodeBuf, tld, false)
	if err != nil || m != len(packet) {
		t.Fatalf("without edns: n %d err %v", m, err)
	}
	withoutEdns = slices.Clone(withoutEdns)

	// the opt record is the trailing 11 bytes and arcount is the only other
	// difference
	const optByteCount = 11
	if len(withEdns) != len(withoutEdns)+optByteCount {
		t.Fatalf("with edns %d bytes, without %d, expected a difference of %d", len(withEdns), len(withoutEdns), optByteCount)
	}
	if withEdns[10] != 0 || withEdns[11] != 1 || withoutEdns[10] != 0 || withoutEdns[11] != 0 {
		t.Fatalf("arcount with edns %x, without %x", withEdns[10:12], withoutEdns[10:12])
	}
	if !slices.Equal(withEdns[:10], withoutEdns[:10]) || !slices.Equal(withEdns[12:len(withoutEdns)], withoutEdns[12:]) {
		t.Fatal("the response with edns differs from the one without beyond arcount and the opt record")
	}

	var parser dnsmessage.Parser
	if _, err := parser.Start(withEdns); err != nil {
		t.Fatal(err)
	}
	parser.SkipAllQuestions()
	parser.SkipAllAnswers()
	parser.SkipAllAuthorities()
	additionals, err := parser.AllAdditionals()
	if err != nil {
		t.Fatal(err)
	}
	if len(additionals) != 1 || additionals[0].Header.Type != dnsmessage.TypeOPT {
		t.Fatalf("additionals = %v, expected one opt record", additionals)
	}
	if int(additionals[0].Header.Class) != dnsEdnsUdpPayloadByteCount || additionals[0].Header.TTL != 0 {
		t.Fatalf("opt class %d ttl %#x", additionals[0].Header.Class, additionals[0].Header.TTL)
	}

	for _, response := range [][]byte{withEdns, withoutEdns} {
		id, decodedPumpHeader, decodedHeader, decoded, err := decodeDnsResponse(response, decodeBuf, [][]byte{tld})
		if err != nil || id != 21 || decodedPumpHeader != pumpHeader || decodedHeader != header || !slices.Equal(decoded, packet) {
			t.Fatalf("decode: err %v id %d pump %x header %x %d bytes", err, id, decodedPumpHeader, decodedHeader, len(decoded))
		}
	}
}
