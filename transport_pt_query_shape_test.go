package connect

import (
	"context"
	"encoding/base32"
	"encoding/binary"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Wire-level tests of the dns carrier's request shape. RFC 9619 makes a
// query with qdcount above 1 a malformed message, and a query with the aa
// bit set is not a query a resolver emits, so these tests read the raw
// header fields of the emitted datagrams instead of asking whether the
// datagrams decode.

const dnsCarrierTestWait = 10 * time.Second

// dnsQueryFacts are the fields of one request datagram: the counts and the
// flag bits from the raw header, the opt record from the parsed additional
// section, and the first question.
type dnsQueryFacts struct {
	id              uint16
	qdCount         int
	arCount         int
	aa              bool
	qr              bool
	opCode          int
	optCount        int
	optUdpPayload   int
	optTtl          uint32
	optName         string
	firstQuestion   dnsmessage.Question
	parsedQuestions int
}

// parseDnsQueryFacts reads a request datagram. It fails the test on a
// datagram that does not parse, which no carrier request may be.
func parseDnsQueryFacts(t *testing.T, label string, msg []byte) dnsQueryFacts {
	t.Helper()
	if len(msg) < 12 {
		t.Fatalf("%s: %d bytes is not a dns message", label, len(msg))
	}
	facts := dnsQueryFacts{
		id:      binary.BigEndian.Uint16(msg[0:2]),
		qdCount: int(binary.BigEndian.Uint16(msg[4:6])),
		arCount: int(binary.BigEndian.Uint16(msg[10:12])),
		aa:      msg[2]&0x04 != 0,
		qr:      msg[2]&0x80 != 0,
		opCode:  int(msg[2]>>3) & 0x0f,
	}
	var parser dnsmessage.Parser
	if _, err := parser.Start(msg); err != nil {
		t.Fatalf("%s: parse: %v", label, err)
	}
	questions, err := parser.AllQuestions()
	if err != nil {
		t.Fatalf("%s: questions: %v", label, err)
	}
	facts.parsedQuestions = len(questions)
	if 0 < len(questions) {
		facts.firstQuestion = questions[0]
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
			facts.optUdpPayload = int(additional.Header.Class)
			facts.optTtl = additional.Header.TTL
			facts.optName = additional.Header.Name.String()
		}
	}
	return facts
}

// requireCarrierQueryShape asserts the shape every carrier request has: a
// standard query, one TXT question under the tld, no aa bit, and one edns0
// opt record that advertises the carrier's udp payload size. It returns the
// facts for further checks.
func requireCarrierQueryShape(t *testing.T, label string, msg []byte, tld []byte) dnsQueryFacts {
	t.Helper()
	facts := parseDnsQueryFacts(t, label, msg)
	if facts.qr || facts.opCode != 0 {
		t.Fatalf("%s: not a standard query: qr %t opcode %d", label, facts.qr, facts.opCode)
	}
	if facts.qdCount != 1 || facts.parsedQuestions != 1 {
		t.Errorf("%s: qdcount = %d (%d parsed), expected 1", label, facts.qdCount, facts.parsedQuestions)
	}
	if facts.aa {
		t.Errorf("%s: aa bit set on a query", label)
	}
	if facts.optCount != 1 {
		t.Errorf("%s: %d opt records, expected 1", label, facts.optCount)
	} else {
		if facts.optUdpPayload != dnsEdnsUdpPayloadByteCount {
			t.Errorf("%s: opt udp payload = %d, expected %d", label, facts.optUdpPayload, dnsEdnsUdpPayloadByteCount)
		}
		if facts.optTtl != 0 {
			t.Errorf("%s: opt ttl = %#x, expected extended rcode 0, version 0, do clear", label, facts.optTtl)
		}
		if facts.optName != "." {
			t.Errorf("%s: opt name = %q, expected the root", label, facts.optName)
		}
	}
	if facts.arCount != facts.optCount {
		t.Errorf("%s: arcount = %d, expected %d", label, facts.arCount, facts.optCount)
	}
	question := facts.firstQuestion
	if question.Type != dnsmessage.TypeTXT || question.Class != dnsmessage.ClassINET {
		t.Errorf("%s: question is %v %v, expected TXT IN", label, question.Type, question.Class)
	}
	name := question.Name.Data[:question.Name.Length]
	if len(name) <= len(tld) || !slices.Equal(name[len(name)-len(tld):], tld) || name[len(name)-len(tld)-1] != '.' {
		t.Errorf("%s: question name %q is not under %q", label, question.Name.String(), tld)
	}
	return facts
}

// dnsQueryFirstLabel returns the raw bytes the first label of the question
// name encodes: the pt header followed by the first packet bytes.
func dnsQueryFirstLabel(t *testing.T, question dnsmessage.Question) []byte {
	t.Helper()
	name := question.Name.Data[:question.Name.Length]
	end := slices.Index(name, '.')
	if end < 0 {
		t.Fatalf("question name %q has no label boundary", question.Name.String())
	}
	decoded, err := base32.HexEncoding.WithPadding(base32.NoPadding).DecodeString(string(name[:end]))
	if err != nil {
		t.Fatalf("first label %q: %v", name[:end], err)
	}
	return decoded
}

// The codec emits one TXT question, no aa bit and an edns0 opt record, for
// a data fragment and for a header-only pump alike, and the first label
// still carries the pt header followed by the first packet bytes.
func TestDnsRequestIsOneQuestionWithoutAaWithEdns(t *testing.T) {
	var encodeBuf [1024]byte
	tld := []byte("pt.example.")
	packet := dnsCarrierTestPacket(400, 0x21)

	cases := []struct {
		label  string
		packet []byte
	}{
		{label: "data", packet: packet},
		{label: "pump", packet: nil},
	}
	for _, c := range cases {
		header := dnsCarrierTestHeader(0x77, uint8(encodeDnsRequestCount(c.packet, tld)), 0)
		n, encoded, err := encodeDnsRequest(0x2b2b, header, c.packet, encodeBuf, tld)
		if err != nil {
			t.Fatalf("%s: encode: %v", c.label, err)
		}
		if expected := min(len(c.packet), 157-len(header)-len(tld)); n != expected {
			t.Fatalf("%s: encoded %d bytes, expected %d", c.label, n, expected)
		}
		facts := requireCarrierQueryShape(t, c.label, encoded, tld)
		if facts.id != 0x2b2b {
			t.Errorf("%s: id = %#x, expected %#x", c.label, facts.id, 0x2b2b)
		}
		firstLabel := dnsQueryFirstLabel(t, facts.firstQuestion)
		expectedLabel := append(slices.Clone(header[:]), c.packet[:min(len(c.packet), 12)]...)
		if !slices.Equal(firstLabel, expectedLabel) {
			t.Errorf("%s: first label decodes to %x, expected header and leading bytes %x", c.label, firstLabel, expectedLabel)
		}
	}
}

// memoryPacketNetwork is an in-process packet network for the carrier
// tests. A datagram written to a listening address lands in that conn's
// inbox intact, and a copy of every datagram a conn writes is published on
// its writes channel for wire assertions. There are no deadlines: the
// translation never sets them on the socket it wraps.
type memoryPacketNetwork struct {
	stateLock sync.Mutex
	addrConns map[string]*memoryPacketConn
}

type memoryPacketDatagram struct {
	data []byte
	from net.Addr
}

type memoryPacketConn struct {
	network   *memoryPacketNetwork
	addr      net.Addr
	inbox     chan memoryPacketDatagram
	writes    chan []byte
	closed    chan struct{}
	closeOnce sync.Once
}

func newMemoryPacketNetwork() *memoryPacketNetwork {
	return &memoryPacketNetwork{
		addrConns: map[string]*memoryPacketConn{},
	}
}

func (self *memoryPacketNetwork) listen(addr net.Addr) *memoryPacketConn {
	conn := &memoryPacketConn{
		network: self,
		addr:    addr,
		inbox:   make(chan memoryPacketDatagram, 1024),
		writes:  make(chan []byte, 1024),
		closed:  make(chan struct{}),
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addrConns[addr.String()] = conn
	return conn
}

func (self *memoryPacketNetwork) lookup(addr net.Addr) (*memoryPacketConn, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	conn, ok := self.addrConns[addr.String()]
	return conn, ok
}

func (self *memoryPacketNetwork) remove(conn *memoryPacketConn) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.addrConns[conn.addr.String()] == conn {
		delete(self.addrConns, conn.addr.String())
	}
}

func (self *memoryPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	select {
	case datagram := <-self.inbox:
		return copy(p, datagram.data), datagram.from, nil
	case <-self.closed:
		return 0, nil, net.ErrClosed
	}
}

// WriteTo publishes the copy first, so a test that has read a datagram
// from writes knows the carrier emitted it, then delivers it like udp
// would: an address nobody listens on, or a full inbox, drops it.
func (self *memoryPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	select {
	case <-self.closed:
		return 0, net.ErrClosed
	default:
	}
	data := slices.Clone(p)
	select {
	case self.writes <- data:
	case <-self.closed:
		return 0, net.ErrClosed
	}
	if peer, ok := self.network.lookup(addr); ok {
		select {
		case peer.inbox <- memoryPacketDatagram{data: slices.Clone(p), from: self.addr}:
		default:
		}
	}
	return len(p), nil
}

func (self *memoryPacketConn) Close() error {
	self.closeOnce.Do(func() {
		close(self.closed)
		self.network.remove(self)
	})
	return nil
}

func (self *memoryPacketConn) LocalAddr() net.Addr {
	return self.addr
}

func (self *memoryPacketConn) SetDeadline(time.Time) error {
	return nil
}

func (self *memoryPacketConn) SetReadDeadline(time.Time) error {
	return nil
}

func (self *memoryPacketConn) SetWriteDeadline(time.Time) error {
	return nil
}

// nextWrite returns the next datagram the conn wrote. The wait is a bound
// on a failed test, not the proof: a caller reads only datagrams a returned
// WriteTo has already committed.
func (self *memoryPacketConn) nextWrite(t *testing.T, label string) []byte {
	t.Helper()
	select {
	case data := <-self.writes:
		return data
	case <-time.After(dnsCarrierTestWait):
		t.Fatalf("%s: no datagram was written", label)
		return nil
	}
}

// nextInbox returns the next datagram delivered to the conn.
func (self *memoryPacketConn) nextInbox(t *testing.T, label string) memoryPacketDatagram {
	t.Helper()
	select {
	case datagram := <-self.inbox:
		return datagram
	case <-time.After(dnsCarrierTestWait):
		t.Fatalf("%s: no datagram was delivered", label)
		return memoryPacketDatagram{}
	}
}

func dnsCarrierTestAddr(host string, port int) *net.UDPAddr {
	return &net.UDPAddr{IP: net.ParseIP(host), Port: port}
}

// dnsCarrierTestSettings are the translation settings the shape tests
// share: one tld, no write pacing, and a state timeout no test can outlive.
func dnsCarrierTestSettings(tld []byte) *PacketTranslationSettings {
	settings := DefaultPacketTranslationSettings()
	settings.DnsTlds = [][]byte{tld}
	settings.WritePacketsPerSecond = 0
	settings.DnsStateTimeout = time.Minute
	return settings
}

// The client translation, at the socket it wraps, writes every fragment of
// a packet in the single-question shape, and the fragments still decode on
// the decoder that shipped before the shape change with the right count,
// index and bytes.
func TestPacketTranslationClientWritesOneQuestionRequests(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tld := []byte("pt.example.")
	network := newMemoryPacketNetwork()
	clientConn := network.listen(dnsCarrierTestAddr("192.0.2.10", 40000))
	serverAddr := dnsCarrierTestAddr("192.0.2.1", 4053)

	client, err := NewPacketTranslation(ctx, PacketTranslationModeDns, clientConn, dnsCarrierTestSettings(tld))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	packet := dnsCarrierTestPacket(500, 0x3c)
	c := encodeDnsRequestCount(packet, tld)
	if n, err := client.WriteTo(packet, serverAddr); err != nil || n != len(packet) {
		t.Fatalf("write = %d, %v", n, err)
	}

	var decodeBuf [1024]byte
	offset := 0
	var firstHeader [18]byte
	for i := range c {
		datagram := clientConn.nextWrite(t, "request")
		requireCarrierQueryShape(t, "request", datagram, tld)

		_, header, data, decodedTld, err, otherData := legacyDecodeDnsRequest(datagram, decodeBuf, [][]byte{tld})
		if err != nil || otherData {
			t.Fatalf("fragment %d: the legacy decoder: err %v other %t", i, err, otherData)
		}
		if !slices.Equal(decodedTld, tld) {
			t.Fatalf("fragment %d: tld = %q", i, decodedTld)
		}
		if i == 0 {
			firstHeader = header
		} else if !slices.Equal(header[:16], firstHeader[:16]) {
			t.Fatalf("fragment %d: header %x is not the packet's %x", i, header[:16], firstHeader[:16])
		}
		if int(header[16]) != c || int(header[17]) != i {
			t.Fatalf("fragment %d: count %d index %d, expected %d and %d", i, header[16], header[17], c, i)
		}
		if !slices.Equal(data, packet[offset:offset+len(data)]) {
			t.Fatalf("fragment %d: bytes differ", i)
		}
		offset += len(data)
	}
	if offset != len(packet) {
		t.Fatalf("the fragments carried %d bytes, expected %d", offset, len(packet))
	}
	select {
	case extra := <-clientConn.writes:
		t.Fatalf("an extra %d-byte datagram was written", len(extra))
	default:
	}
}
