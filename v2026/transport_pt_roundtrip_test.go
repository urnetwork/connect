package connect

import (
	"context"
	"encoding/binary"
	"slices"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Round trips over the dns carrier at its real entry points: the client
// translation's WriteTo and the server translation's read loop, joined by
// an in-process packet network. The server runs in the mode that can only
// answer with a pump item a request produced, so a response proves the
// pairing survived the single-question shape; the response datagrams are
// then read on the wire for the opt record the paired request asked for.

// dnsResponseFacts are the fields of one response datagram.
type dnsResponseFacts struct {
	id       uint16
	qr       bool
	aa       bool
	arCount  int
	optCount int
}

func parseDnsResponseFacts(t *testing.T, label string, msg []byte) dnsResponseFacts {
	t.Helper()
	if len(msg) < 12 {
		t.Fatalf("%s: %d bytes is not a dns message", label, len(msg))
	}
	facts := dnsResponseFacts{
		id:      binary.BigEndian.Uint16(msg[0:2]),
		qr:      msg[2]&0x80 != 0,
		aa:      msg[2]&0x04 != 0,
		arCount: int(binary.BigEndian.Uint16(msg[10:12])),
	}
	var parser dnsmessage.Parser
	if _, err := parser.Start(msg); err != nil {
		t.Fatalf("%s: parse: %v", label, err)
	}
	if err := parser.SkipAllQuestions(); err != nil {
		t.Fatalf("%s: questions: %v", label, err)
	}
	if err := parser.SkipAllAnswers(); err != nil {
		t.Fatalf("%s: answers: %v", label, err)
	}
	if err := parser.SkipAllAuthorities(); err != nil {
		t.Fatalf("%s: authorities: %v", label, err)
	}
	additionals, err := parser.AllAdditionals()
	if err != nil {
		t.Fatalf("%s: additionals: %v", label, err)
	}
	for _, additional := range additionals {
		if additional.Header.Type == dnsmessage.TypeOPT {
			facts.optCount += 1
			if int(additional.Header.Class) != dnsEdnsUdpPayloadByteCount {
				t.Errorf("%s: opt udp payload = %d, expected %d", label, additional.Header.Class, dnsEdnsUdpPayloadByteCount)
			}
		}
	}
	if !facts.qr || !facts.aa {
		t.Errorf("%s: qr %t aa %t, expected an authoritative response", label, facts.qr, facts.aa)
	}
	return facts
}

// dnsCarrierTestServer is a decode53 translation that requires pump items,
// with a barrier on the pump items its read loop queued.
type dnsCarrierTestServer struct {
	conn        *memoryPacketConn
	translation *packetTranslation
	pumpAdded   chan *pumpItem
}

func newDnsCarrierTestServer(t *testing.T, ctx context.Context, network *memoryPacketNetwork, tld []byte) *dnsCarrierTestServer {
	t.Helper()
	conn := network.listen(dnsCarrierTestAddr("192.0.2.1", 4053))
	translation, err := NewPacketTranslation(ctx, PacketTranslationModeDecode53RequireDnsPump, conn, dnsCarrierTestSettings(tld))
	if err != nil {
		t.Fatal(err)
	}
	server := &dnsCarrierTestServer{
		conn:        conn,
		translation: translation,
		pumpAdded:   make(chan *pumpItem, 256),
	}
	// set before any datagram reaches the read loop, which orders the write
	// before the pump consumer's read of it through the delivery channels
	translation.pumpAddedForTest = func(item *pumpItem, limit bool) {
		if limit {
			t.Errorf("pump item for %s hit the queue limit", item.addr)
		}
		server.pumpAdded <- item
	}
	return server
}

// waitPumpItems returns the next n pump items the server queued.
func (self *dnsCarrierTestServer) waitPumpItems(t *testing.T, n int) []*pumpItem {
	t.Helper()
	items := make([]*pumpItem, 0, n)
	for len(items) < n {
		select {
		case item := <-self.pumpAdded:
			items = append(items, item)
		case <-time.After(dnsCarrierTestWait):
			t.Fatalf("only %d of %d pump items were queued", len(items), n)
		}
	}
	return items
}

// readPacket reads one translated packet with a bounded wait.
func readTranslatedPacket(t *testing.T, label string, translation *packetTranslation) ([]byte, string) {
	t.Helper()
	buf := make([]byte, 2048)
	translation.SetReadDeadline(time.Now().Add(dnsCarrierTestWait))
	n, addr, err := translation.ReadFrom(buf)
	if err != nil {
		t.Fatalf("%s: read: %v", label, err)
	}
	return buf[:n], addr.String()
}

// A new client and a new server: the client's single-question requests
// reach the server as one packet and queue one pump item each, all marked
// edns; the server's response goes out paired with those requests, with the
// opt record they asked for, and arrives at the client as one packet.
func TestPacketTranslationRoundTripPairsResponsesWithOneQuestionRequests(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tld := []byte("pt.example.")
	network := newMemoryPacketNetwork()
	server := newDnsCarrierTestServer(t, ctx, network, tld)
	defer server.translation.Close()

	clientConn := network.listen(dnsCarrierTestAddr("192.0.2.10", 40000))
	client, err := NewPacketTranslation(ctx, PacketTranslationModeDns, clientConn, dnsCarrierTestSettings(tld))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	request := dnsCarrierTestPacket(500, 0x4d)
	c := encodeDnsRequestCount(request, tld)
	if n, err := client.WriteTo(request, server.conn.addr); err != nil || n != len(request) {
		t.Fatalf("client write = %d, %v", n, err)
	}

	// the request datagrams, as written
	var decodeBuf [1024]byte
	requestIds := map[uint16]bool{}
	requestHeaders := map[[18]byte]bool{}
	for i := range c {
		datagram := clientConn.nextWrite(t, "request")
		facts := requireCarrierQueryShape(t, "request", datagram, tld)
		id, header, _, _, err, otherData, edns := decodeDnsRequest(datagram, decodeBuf, [][]byte{tld})
		if err != nil || otherData || !edns {
			t.Fatalf("fragment %d: decode: err %v other %t edns %t", i, err, otherData, edns)
		}
		if id != facts.id {
			t.Fatalf("fragment %d: decoded id %#x, wire id %#x", i, id, facts.id)
		}
		requestIds[id] = true
		requestHeaders[header] = true
	}

	received, from := readTranslatedPacket(t, "server", server.translation)
	if !slices.Equal(received, request) {
		t.Fatalf("the server read %d bytes that differ from the %d sent", len(received), len(request))
	}
	if from != clientConn.addr.String() {
		t.Fatalf("the server read from %s, expected %s", from, clientConn.addr)
	}

	items := server.waitPumpItems(t, c)
	for _, item := range items {
		if item.addr.String() != clientConn.addr.String() {
			t.Fatalf("pump item for %s, expected %s", item.addr, clientConn.addr)
		}
		if !requestIds[item.id] || !requestHeaders[item.header] {
			t.Fatalf("pump item %#x %x is not one of the requests", item.id, item.header)
		}
		if !item.edns {
			t.Fatalf("pump item %#x is not marked edns", item.id)
		}
	}

	response := dnsCarrierTestPacket(200, 0x5e)
	if n, err := server.translation.WriteTo(response, clientConn.addr); err != nil || n != len(response) {
		t.Fatalf("server write = %d, %v", n, err)
	}
	for i := range encodeDnsResponseCount(response, tld) {
		datagram := server.conn.nextWrite(t, "response")
		facts := parseDnsResponseFacts(t, "response", datagram)
		if !requestIds[facts.id] {
			t.Fatalf("response %d: id %#x is not a request id", i, facts.id)
		}
		if facts.optCount != 1 || facts.arCount != 1 {
			t.Errorf("response %d: %d opt records, arcount %d, expected the opt record the request carried", i, facts.optCount, facts.arCount)
		}
		_, pumpHeader, _, _, err := decodeDnsResponse(datagram, decodeBuf, [][]byte{tld})
		if err != nil {
			t.Fatalf("response %d: decode: %v", i, err)
		}
		if !requestHeaders[pumpHeader] {
			t.Fatalf("response %d: paired header %x is not a request header", i, pumpHeader)
		}
	}

	received, from = readTranslatedPacket(t, "client", client)
	if !slices.Equal(received, response) {
		t.Fatalf("the client read %d bytes that differ from the %d sent", len(received), len(response))
	}
	if from != server.conn.addr.String() {
		t.Fatalf("the client read from %s, expected %s", from, server.conn.addr)
	}
}

// An old client and a new server: requests in the two-question shape, with
// the aa bit and no opt record, still reach the server as one packet and
// queue pump items, now marked not edns, and the response to them is the
// response those clients always received: paired, and with no opt record.
func TestPacketTranslationAnswersLegacyRequestsWithoutEdns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tld := []byte("pt.example.")
	network := newMemoryPacketNetwork()
	server := newDnsCarrierTestServer(t, ctx, network, tld)
	defer server.translation.Close()

	// the old client is the frozen encoder writing straight to the socket
	legacyConn := network.listen(dnsCarrierTestAddr("192.0.2.20", 40001))
	defer legacyConn.Close()

	var encodeBuf [1024]byte
	request := dnsCarrierTestPacket(500, 0x6f)
	c := encodeDnsRequestCount(request, tld)
	requestIds := map[uint16]bool{}
	offset := 0
	for i := range c {
		header := dnsCarrierTestHeader(0x12, uint8(c), uint8(i))
		n, datagram, err := legacyEncodeDnsRequest(uint16(0x7000+i), header, request[offset:], encodeBuf, tld)
		if err != nil {
			t.Fatal(err)
		}
		offset += n
		facts := parseDnsQueryFacts(t, "legacy request", datagram)
		if facts.qdCount != 2 || !facts.aa || facts.arCount != 0 {
			t.Fatalf("the frozen encoder is not the legacy shape: qdcount %d aa %t arcount %d", facts.qdCount, facts.aa, facts.arCount)
		}
		requestIds[facts.id] = true
		if _, err := legacyConn.WriteTo(datagram, server.conn.addr); err != nil {
			t.Fatal(err)
		}
	}

	received, from := readTranslatedPacket(t, "server", server.translation)
	if !slices.Equal(received, request) {
		t.Fatalf("the server read %d bytes that differ from the %d sent", len(received), len(request))
	}
	if from != legacyConn.addr.String() {
		t.Fatalf("the server read from %s, expected %s", from, legacyConn.addr)
	}
	for _, item := range server.waitPumpItems(t, c) {
		if !requestIds[item.id] {
			t.Fatalf("pump item %#x is not one of the requests", item.id)
		}
		if item.edns {
			t.Fatalf("pump item %#x is marked edns for a request without an opt record", item.id)
		}
	}

	response := dnsCarrierTestPacket(200, 0x70)
	if n, err := server.translation.WriteTo(response, legacyConn.addr); err != nil || n != len(response) {
		t.Fatalf("server write = %d, %v", n, err)
	}
	var decodeBuf [1024]byte
	for i := range encodeDnsResponseCount(response, tld) {
		delivered := legacyConn.nextInbox(t, "response")
		facts := parseDnsResponseFacts(t, "response", delivered.data)
		if !requestIds[facts.id] {
			t.Fatalf("response %d: id %#x is not a request id", i, facts.id)
		}
		if facts.arCount != 0 || facts.optCount != 0 {
			t.Errorf("response %d: arcount %d, %d opt records, expected none for a request without one", i, facts.arCount, facts.optCount)
		}
		_, _, header, data, err := decodeDnsResponse(delivered.data, decodeBuf, [][]byte{tld})
		if err != nil {
			t.Fatalf("response %d: decode: %v", i, err)
		}
		if header[16] != 1 || !slices.Equal(data, response) {
			t.Fatalf("response %d: count %d, %d bytes, expected the whole %d-byte packet", i, header[16], len(data), len(response))
		}
	}
}
