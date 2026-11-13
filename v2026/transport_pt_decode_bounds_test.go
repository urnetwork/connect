package connect

import (
	"encoding/base32"
	"slices"
	"strings"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// Compression makes forty 120-byte questions fit in one 453-byte datagram.
// The decompressed labels, rather than the wire size, bound decoder writes.
func compressedDnsQuestions(t *testing.T) []byte {
	t.Helper()
	data := dnsCarrierTestPacket(120, 0x41)
	enc := base32.HexEncoding.WithPadding(base32.NoPadding)
	var labels []string
	for offset := 0; offset < len(data); offset += 30 {
		labels = append(labels, enc.EncodeToString(data[offset:offset+30]))
	}
	name := dnsmessage.MustNewName(strings.Join(labels, ".") + ".a.dev.")
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 0x1234})
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	for range 40 {
		if err := b.Question(dnsmessage.Question{Name: name, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET}); err != nil {
			t.Fatal(err)
		}
	}
	packet, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if len(packet) != 453 {
		t.Fatalf("compressed query size = %d, want 453", len(packet))
	}
	return packet
}

func TestDnsRequestRejectsCompressedQuestionOverflow(t *testing.T) {
	_, _, _, _, err, _, _ := decodeDnsRequest(compressedDnsQuestions(t), [1024]byte{}, [][]byte{[]byte("a.dev.")})
	if err == nil {
		t.Fatal("accepted questions whose decoded bytes exceed the buffer")
	}
}

// A malformed datagram must neither close the translation nor add a response
// pump. A following valid request proves the same decoder is still serving.
func TestPacketTranslationDropsCompressedQuestionOverflow(t *testing.T) {
	network := newMemoryPacketNetwork()
	tld := []byte("a.dev.")
	server := newDnsCarrierTestServer(t, t.Context(), network, tld)
	t.Cleanup(func() { server.translation.Close() })
	peer := network.listen(dnsCarrierTestAddr("192.0.2.10", 40000))
	t.Cleanup(func() { peer.Close() })
	if _, err := peer.WriteTo(compressedDnsQuestions(t), server.conn.addr); err != nil {
		t.Fatal(err)
	}
	header := dnsCarrierTestHeader(0x51, 1, 0)
	payload := []byte("valid request after oversized questions")
	_, packet, err := encodeDnsRequest(0x5678, header, payload, [1024]byte{}, tld)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := peer.WriteTo(packet, server.conn.addr); err != nil {
		t.Fatal(err)
	}
	decoded, _ := readTranslatedPacket(t, "after oversized questions", server.translation)
	if !slices.Equal(decoded, payload) {
		t.Fatalf("translated payload = %q, want %q", decoded, payload)
	}
	items := server.waitPumpItems(t, 1)
	if items[0].id != 0x5678 || items[0].header != header {
		t.Fatal("malformed request created a response pump")
	}
	if err := server.translation.EndError(); err != nil {
		t.Fatalf("malformed request ended the translation: %v", err)
	}
}
