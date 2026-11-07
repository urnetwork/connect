package connect

// C14: the mux answers a local zone's names on the device, for every query type,
// over UDP and TCP, so no query for one leaves it (DESIGN §12.9, owner decision 85).
// These run a real client stack against a real mux and a fake DoH upstream that
// remembers what it was asked, unless a test says otherwise.

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/urnetwork/connect/protocol"
)

const (
	// answers each query type from the zone's addresses
	localZoneAddressName = "laptop.zone.example"
	// owned, with nothing to answer for any type
	localZoneEmptyName = "printer.zone.example"
	// nothing in the zone
	localZoneForeignName = "elsewhere.example"
	// distinct from the mux's ResponseTtl, so an answer shows whose it is
	localZoneTtl = uint32(77)
)

// Every type the plan names: what a browser and a mail client ask of a name.
var localZoneQtypes = []dnsmessage.Type{
	dnsmessage.TypeA,
	dnsmessage.TypeAAAA,
	dnsTypeHttps,
	dnsmessage.TypeTXT,
	dnsmessage.TypeMX,
}

// One owned name's addresses. Answer returns them for every type, as a simple
// table may, so what reaches the client shows the mux's own filtering.
type localZoneRecord struct {
	addrs []netip.Addr
	ttl   uint32
}

// A zone over a fixed table, remembering the hosts it was asked for.
type localZoneTable struct {
	records map[string]localZoneRecord

	stateLock sync.Mutex
	hosts     []string
}

func (self *localZoneTable) Answer(host string, qtype uint16) ([]netip.Addr, uint32, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.hosts = append(self.hosts, host)
	record, owned := self.records[host]
	return record.addrs, record.ttl, owned
}

func (self *localZoneTable) askedHosts() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.hosts)
}

func newLocalZoneTable() *localZoneTable {
	return &localZoneTable{
		records: map[string]localZoneRecord{
			localZoneAddressName: {
				addrs: []netip.Addr{testDocAddr(4, 7), testDocAddr(6, 7)},
				ttl:   localZoneTtl,
			},
			localZoneEmptyName: {ttl: localZoneTtl},
		},
	}
}

// A Blocker that blocks one name and remembers what it was asked.
type localZoneBlocker struct {
	blockedHost string

	stateLock sync.Mutex
	hosts     []string
}

func (self *localZoneBlocker) BlockHost(host string) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.hosts = append(self.hosts, host)
	return host == self.blockedHost
}

func (self *localZoneBlocker) BlockIp(ip netip.Addr) bool {
	return false
}

func (self *localZoneBlocker) Enabled() bool {
	return true
}

func (self *localZoneBlocker) SetEnabled(enabled bool) {
}

func (self *localZoneBlocker) askedHosts() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.hosts)
}

// A fake DoH upstream that answers with one address and remembers each question,
// so a test can say which names left the device.
type localZoneUpstream struct {
	server *httptest.Server

	stateLock sync.Mutex
	questions []dnsmessage.Question
}

func newLocalZoneUpstream(t *testing.T, ipVersion int, resolved netip.Addr) *localZoneUpstream {
	t.Helper()
	self := &localZoneUpstream{}
	self.server = newFamilyHttptestTlsServer(t, ipVersion, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("dns"))
		if err == nil {
			var parser dnsmessage.Parser
			if _, err := parser.Start(raw); err == nil {
				if question, err := parser.Question(); err == nil {
					self.stateLock.Lock()
					self.questions = append(self.questions, question)
					self.stateLock.Unlock()
				}
			}
		}
		writeDohWire(w, r, []netip.Addr{resolved}, 60, false)
	}))
	t.Cleanup(self.server.Close)
	return self
}

// The names asked of the upstream, in lower case and without a trailing dot.
func (self *localZoneUpstream) askedNames() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	names := make([]string, 0, len(self.questions))
	for _, question := range self.questions {
		names = append(names, strings.ToLower(strings.TrimSuffix(question.Name.String(), ".")))
	}
	return names
}

func (self *localZoneUpstream) requireNotAsked(t *testing.T, names ...string) {
	t.Helper()
	asked := self.askedNames()
	for _, name := range names {
		if slices.Contains(asked, name) {
			t.Fatalf("the upstream was asked for %s: %v", name, asked)
		}
	}
}

func (self *localZoneUpstream) requireAsked(t *testing.T, name string) {
	t.Helper()
	if asked := self.askedNames(); !slices.Contains(asked, name) {
		t.Fatalf("the upstream was not asked for %s: %v", name, asked)
	}
}

// One client path to the mux's DNS, exchanging raw DNS messages.
type localZoneExchange func(t *testing.T, query []byte) []byte

// Sends one query over a fresh UDP socket through the client stack.
func localZoneUdpExchange(ctx context.Context, h *dnsClientHarness) localZoneExchange {
	return func(t *testing.T, query []byte) []byte {
		t.Helper()
		conn, err := h.clientTun.DialContext(ctx, "udp", testDnsDialAddress(h.ipVersion))
		if err != nil {
			t.Fatalf("dial udp dns: %v", err)
		}
		defer conn.Close()
		if err := conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write(query); err != nil {
			t.Fatalf("write udp dns: %v", err)
		}
		reply := make([]byte, 4096)
		n, err := conn.Read(reply)
		if err != nil {
			t.Fatalf("read udp dns: %v", err)
		}
		return reply[:n]
	}
}

// Sends one query over a fresh TCP connection through the client stack, which
// the mux redirects to its internal DNS server.
func localZoneTcpExchange(ctx context.Context, h *dnsClientHarness) localZoneExchange {
	return func(t *testing.T, query []byte) []byte {
		t.Helper()
		conn, err := h.clientTun.DialContext(ctx, "tcp", testDnsDialAddress(h.ipVersion))
		if err != nil {
			t.Fatalf("dial tcp dns: %v", err)
		}
		defer conn.Close()
		if err := conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
			t.Fatal(err)
		}
		return localZoneTcpRoundTrip(t, conn, query)
	}
}

func localZoneTcpRoundTrip(t *testing.T, conn net.Conn, query []byte) []byte {
	t.Helper()
	frame := make([]byte, 2+len(query))
	binary.BigEndian.PutUint16(frame[:2], uint16(len(query)))
	copy(frame[2:], query)
	if _, err := conn.Write(frame); err != nil {
		t.Fatalf("write tcp dns: %v", err)
	}
	var replyLength [2]byte
	if _, err := io.ReadFull(conn, replyLength[:]); err != nil {
		t.Fatalf("read tcp dns length: %v", err)
	}
	reply := make([]byte, int(binary.BigEndian.Uint16(replyLength[:])))
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("read tcp dns reply: %v", err)
	}
	return reply
}

func localZoneQuery(t *testing.T, name string, qtype dnsmessage.Type, id uint16) []byte {
	t.Helper()
	builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id, RecursionDesired: true})
	if err := builder.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	if err := builder.Question(dnsmessage.Question{
		Name:  dnsmessage.MustNewName(name + "."),
		Type:  qtype,
		Class: dnsmessage.ClassINET,
	}); err != nil {
		t.Fatal(err)
	}
	query, err := builder.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return query
}

// Parses a reply and holds it to the query it answers: the id, and the question
// exactly as asked, its case included.
func localZoneParseReply(t *testing.T, reply []byte, name string, qtype dnsmessage.Type, id uint16) (dnsmessage.Header, []dnsmessage.Resource) {
	t.Helper()
	var parser dnsmessage.Parser
	header, err := parser.Start(reply)
	if err != nil {
		t.Fatal(err)
	}
	question, err := parser.Question()
	if err != nil {
		t.Fatal(err)
	}
	if err := parser.SkipAllQuestions(); err != nil {
		t.Fatal(err)
	}
	answers, err := parser.AllAnswers()
	if err != nil {
		t.Fatal(err)
	}
	if header.ID != id || !header.Response {
		t.Fatalf("%s %v header = %+v, want a response to %04x", name, qtype, header, id)
	}
	if question.Name.String() != name+"." || question.Type != qtype {
		t.Fatalf("%s %v question echoed as %s %v", name, qtype, question.Name, question.Type)
	}
	return header, answers
}

// Holds a reply to the empty NOERROR that NODATA is.
func localZoneRequireNodata(t *testing.T, reply []byte, name string, qtype dnsmessage.Type, id uint16) {
	t.Helper()
	header, answers := localZoneParseReply(t, reply, name, qtype, id)
	if header.RCode != dnsmessage.RCodeSuccess || header.Truncated || len(answers) != 0 {
		t.Fatalf("%s %v: rcode=%v tc=%t answers=%d, want NODATA", name, qtype, header.RCode, header.Truncated, len(answers))
	}
}

// Holds a reply to one address record of the zone's ttl.
func localZoneRequireAddress(t *testing.T, reply []byte, name string, qtype dnsmessage.Type, id uint16, want netip.Addr) {
	t.Helper()
	header, answers := localZoneParseReply(t, reply, name, qtype, id)
	if header.RCode != dnsmessage.RCodeSuccess || len(answers) != 1 {
		t.Fatalf("%s %v: rcode=%v answers=%d, want one address", name, qtype, header.RCode, len(answers))
	}
	var got netip.Addr
	switch body := answers[0].Body.(type) {
	case *dnsmessage.AResource:
		got = netip.AddrFrom4(body.A)
	case *dnsmessage.AAAAResource:
		got = netip.AddrFrom16(body.AAAA)
	}
	if got != want {
		t.Fatalf("%s %v answered %v, want %s", name, qtype, answers[0].Body, want)
	}
	if answers[0].Header.TTL != localZoneTtl {
		t.Fatalf("%s %v ttl = %d, want the zone's %d", name, qtype, answers[0].Header.TTL, localZoneTtl)
	}
}

func newLocalZoneHarness(t *testing.T, ipVersion int, upstream *localZoneUpstream) (*dnsClientHarness, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	h := newDnsClientHarness(t, ctx, ipVersion, localDohTlsResolverSettings(ipVersion, upstream.server))
	t.Cleanup(h.close)
	return h, ctx
}

// Runs a body once over UDP and once over TCP.
func forEachLocalZoneExchange(ctx context.Context, h *dnsClientHarness, body func(exchange localZoneExchange)) {
	for _, exchange := range []localZoneExchange{localZoneUdpExchange(ctx, h), localZoneTcpExchange(ctx, h)} {
		body(exchange)
	}
}

// §22.7's "a private name gets A or NODATA on the device for every qtype", at
// connect's level. An owned name with addresses answers them for A and AAAA, and
// every other type, HTTPS included, is NODATA. An owned name with none is NODATA
// for all of them. The upstream hears of neither.
func TestUpgradeMuxLocalZoneAnswersEveryQtypeOnTheDevice(t *testing.T) {
	forEachIpVersion(t, func(t *testing.T, ipVersion int) {
		upstream := newLocalZoneUpstream(t, ipVersion, testDocAddr(ipVersion, 45))
		h, ctx := newLocalZoneHarness(t, ipVersion, upstream)
		h.mux.SetLocalZone(newLocalZoneTable())

		var id uint16 = 0x7100
		forEachLocalZoneExchange(ctx, h, func(exchange localZoneExchange) {
			for _, qtype := range localZoneQtypes {
				id += 1
				reply := exchange(t, localZoneQuery(t, localZoneAddressName, qtype, id))
				switch qtype {
				case dnsmessage.TypeA:
					localZoneRequireAddress(t, reply, localZoneAddressName, qtype, id, testDocAddr(4, 7))
				case dnsmessage.TypeAAAA:
					localZoneRequireAddress(t, reply, localZoneAddressName, qtype, id, testDocAddr(6, 7))
				default:
					localZoneRequireNodata(t, reply, localZoneAddressName, qtype, id)
				}

				id += 1
				reply = exchange(t, localZoneQuery(t, localZoneEmptyName, qtype, id))
				localZoneRequireNodata(t, reply, localZoneEmptyName, qtype, id)
			}
		})

		// A name the zone does not own goes out on the same paths, so a clean
		// upstream below means the owned names were refused it, not that it
		// was never reachable.
		forEachLocalZoneExchange(ctx, h, func(exchange localZoneExchange) {
			id += 1
			exchange(t, localZoneQuery(t, localZoneForeignName, dnsmessage.TypeTXT, id))
		})
		upstream.requireAsked(t, localZoneForeignName)
		upstream.requireNotAsked(t, localZoneAddressName, localZoneEmptyName)
	})
}

// A name the zone does not own falls through unchanged, for addresses and for a
// forwarded type alike, over both paths.
func TestUpgradeMuxLocalZonePassesOtherNamesThrough(t *testing.T) {
	forEachIpVersion(t, func(t *testing.T, ipVersion int) {
		resolved := testDocAddr(ipVersion, 45)
		upstream := newLocalZoneUpstream(t, ipVersion, resolved)
		h, ctx := newLocalZoneHarness(t, ipVersion, upstream)
		zone := newLocalZoneTable()
		h.mux.SetLocalZone(zone)

		qtype := testDnsQType(ipVersion)
		var id uint16 = 0x7200
		forEachLocalZoneExchange(ctx, h, func(exchange localZoneExchange) {
			id += 1
			reply := exchange(t, localZoneQuery(t, localZoneForeignName, qtype, id))
			header, _ := localZoneParseReply(t, reply, localZoneForeignName, qtype, id)
			if header.RCode != dnsmessage.RCodeSuccess {
				t.Fatalf("rcode = %v", header.RCode)
			}
			if _, ok := parseDohWire(reply, qtype).AddrTtls[resolved]; !ok {
				t.Fatalf("%s did not answer the upstream's %s", localZoneForeignName, resolved)
			}

			id += 1
			reply = exchange(t, localZoneQuery(t, localZoneForeignName, dnsmessage.TypeMX, id))
			localZoneRequireNodata(t, reply, localZoneForeignName, dnsmessage.TypeMX, id)
		})
		upstream.requireAsked(t, localZoneForeignName)
		if !slices.Contains(zone.askedHosts(), localZoneForeignName) {
			t.Fatalf("the zone was not consulted: %v", zone.askedHosts())
		}
		if names := h.mux.ServerNames(resolved.String()); !slices.Contains(names, localZoneForeignName) {
			t.Fatalf("a name the zone does not own lost its reverse record: %v", names)
		}
	})
}

// With no zone the mux behaves as it always has, and removing a zone restores
// that with no rebuild. This is C14's default-off pin.
func TestUpgradeMuxLocalZoneNilKeepsTodaysBehavior(t *testing.T) {
	forEachIpVersion(t, func(t *testing.T, ipVersion int) {
		resolved := testDocAddr(ipVersion, 45)
		upstream := newLocalZoneUpstream(t, ipVersion, resolved)
		h, ctx := newLocalZoneHarness(t, ipVersion, upstream)

		qtype := testDnsQType(ipVersion)
		var id uint16 = 0x7300
		requireFromUpstream := func(exchange localZoneExchange) {
			t.Helper()
			id += 1
			reply := exchange(t, localZoneQuery(t, localZoneAddressName, qtype, id))
			localZoneParseReply(t, reply, localZoneAddressName, qtype, id)
			if _, ok := parseDohWire(reply, qtype).AddrTtls[resolved]; !ok {
				t.Fatalf("%s did not answer the upstream's %s", localZoneAddressName, resolved)
			}
		}

		forEachLocalZoneExchange(ctx, h, requireFromUpstream)
		upstream.requireAsked(t, localZoneAddressName)

		h.mux.SetLocalZone(newLocalZoneTable())
		h.mux.SetLocalZone(nil)
		forEachLocalZoneExchange(ctx, h, requireFromUpstream)
		if h.mux.getLocalZone() != nil {
			t.Fatal("a removed zone is still installed")
		}
	})
}

// The zone sits ahead of the blocker: an owned name is answered without asking
// it, even a name it would block, and the blocker still sees everything else.
func TestUpgradeMuxLocalZoneDoesNotConsultTheBlocker(t *testing.T) {
	forEachIpVersion(t, func(t *testing.T, ipVersion int) {
		upstream := newLocalZoneUpstream(t, ipVersion, testDocAddr(ipVersion, 45))
		h, ctx := newLocalZoneHarness(t, ipVersion, upstream)
		h.mux.SetLocalZone(newLocalZoneTable())
		blocker := &localZoneBlocker{blockedHost: localZoneAddressName}
		h.mux.SetBlocker(blocker)

		var id uint16 = 0x7400
		forEachLocalZoneExchange(ctx, h, func(exchange localZoneExchange) {
			id += 1
			reply := exchange(t, localZoneQuery(t, localZoneAddressName, dnsmessage.TypeA, id))
			localZoneRequireAddress(t, reply, localZoneAddressName, dnsmessage.TypeA, id, testDocAddr(4, 7))
			id += 1
			reply = exchange(t, localZoneQuery(t, localZoneEmptyName, dnsTypeHttps, id))
			localZoneRequireNodata(t, reply, localZoneEmptyName, dnsTypeHttps, id)
		})
		if asked := blocker.askedHosts(); 0 < len(asked) {
			t.Fatalf("the blocker was consulted for an owned name: %v", asked)
		}

		forEachLocalZoneExchange(ctx, h, func(exchange localZoneExchange) {
			id += 1
			exchange(t, localZoneQuery(t, localZoneForeignName, dnsmessage.TypeTXT, id))
		})
		if asked := blocker.askedHosts(); len(asked) != 2 || asked[0] != localZoneForeignName || asked[1] != localZoneForeignName {
			t.Fatalf("the blocker was asked %v, want %s over each path", asked, localZoneForeignName)
		}
	})
}

// A query's casing is the client's (dns 0x20): the zone is asked in lower case,
// and the reply echoes the question as it came.
func TestUpgradeMuxLocalZoneMatchesMixedCase(t *testing.T) {
	forEachIpVersion(t, func(t *testing.T, ipVersion int) {
		upstream := newLocalZoneUpstream(t, ipVersion, testDocAddr(ipVersion, 45))
		h, ctx := newLocalZoneHarness(t, ipVersion, upstream)
		zone := newLocalZoneTable()
		h.mux.SetLocalZone(zone)

		const mixedName = "LaPtOp.ZoNe.ExAmPlE"
		var id uint16 = 0x7500
		forEachLocalZoneExchange(ctx, h, func(exchange localZoneExchange) {
			id += 1
			reply := exchange(t, localZoneQuery(t, mixedName, dnsmessage.TypeA, id))
			localZoneRequireAddress(t, reply, mixedName, dnsmessage.TypeA, id, testDocAddr(4, 7))
			id += 1
			reply = exchange(t, localZoneQuery(t, mixedName, dnsmessage.TypeTXT, id))
			localZoneRequireNodata(t, reply, mixedName, dnsmessage.TypeTXT, id)
		})
		for _, host := range zone.askedHosts() {
			if host != localZoneAddressName {
				t.Fatalf("the zone was asked for %q, want %q", host, localZoneAddressName)
			}
		}
		upstream.requireNotAsked(t, localZoneAddressName)
	})
}

// A local answer enters neither the reverse index nor the resolver cache: the
// address names nothing, and once the zone is gone the name is resolved afresh.
// The zone also beats an answer the cache already holds.
func TestUpgradeMuxLocalZoneLeavesNoTrace(t *testing.T) {
	forEachIpVersion(t, func(t *testing.T, ipVersion int) {
		resolved := testDocAddr(ipVersion, 45)
		local := testDocAddr(ipVersion, 7)
		upstream := newLocalZoneUpstream(t, ipVersion, resolved)
		h, ctx := newLocalZoneHarness(t, ipVersion, upstream)
		h.mux.SetLocalZone(newLocalZoneTable())
		exchange := localZoneUdpExchange(ctx, h)
		qtype := testDnsQType(ipVersion)

		reply := exchange(t, localZoneQuery(t, localZoneAddressName, qtype, 0x7601))
		localZoneRequireAddress(t, reply, localZoneAddressName, qtype, 0x7601, local)
		if names := h.mux.ServerNames(local.String()); len(names) != 0 {
			t.Fatalf("a local answer entered the reverse index: %v", names)
		}

		// a cached local answer would come back here with the upstream unasked
		h.mux.SetLocalZone(nil)
		reply = exchange(t, localZoneQuery(t, localZoneAddressName, qtype, 0x7602))
		localZoneParseReply(t, reply, localZoneAddressName, qtype, 0x7602)
		if _, ok := parseDohWire(reply, qtype).AddrTtls[resolved]; !ok {
			t.Fatalf("the name did not resolve afresh once the zone was gone")
		}
		upstream.requireAsked(t, localZoneAddressName)

		// the name is now cached and reverse-indexed, and the zone still wins
		h.mux.SetLocalZone(newLocalZoneTable())
		reply = exchange(t, localZoneQuery(t, localZoneAddressName, qtype, 0x7603))
		localZoneRequireAddress(t, reply, localZoneAddressName, qtype, 0x7603, local)
	})
}

// resolver.arpa comes first: a zone cannot take the special-use zone, which is
// answered as NODATA without asking it.
func TestUpgradeMuxLocalZoneComesAfterResolverArpa(t *testing.T) {
	mux, rec, closeMux := newResolverArpaTestMux(t)
	defer closeMux()
	const arpaName = "_dns.resolver.arpa"
	zone := newLocalZoneTable()
	zone.records[arpaName] = localZoneRecord{addrs: []netip.Addr{testDocAddr(4, 7)}, ttl: localZoneTtl}
	mux.SetLocalZone(zone)

	const id = 0x7701
	if !mux.SendPacket(TransferPath{}, protocol.ProvideMode_Network, dnsQueryPacketTyped(t, arpaName+".", dnsmessage.TypeA, id), 0) {
		t.Fatal("not claimed")
	}
	if _, received := rec.counts(); received != 1 {
		t.Fatalf("received %d replies, want 1", received)
	}
	assertResolverArpaNodata(t, rec.receivedPackets()[0], id, dnsmessage.TypeA)
	if response := mux.resolveDnsTcpQuery(localZoneQuery(t, arpaName, dnsmessage.TypeA, id)); response == nil {
		t.Fatal("no reply over tcp")
	} else {
		localZoneRequireNodata(t, response, arpaName, dnsmessage.TypeA, id)
	}
	if asked := zone.askedHosts(); 0 < len(asked) {
		t.Fatalf("the zone was asked for %v", asked)
	}
}

// The zone answers before the v6-unroutable rule, which speaks for the tunnel
// and has no say over a name on this device.
func TestUpgradeMuxLocalZoneComesBeforeIpv6Unroutable(t *testing.T) {
	mux, rec, closeMux := newResolverArpaTestMux(t)
	defer closeMux()
	mux.SetLocalZone(newLocalZoneTable())
	mux.SetIpv6Unroutable(func() bool { return true })

	const id = 0x7801
	if !mux.SendPacket(TransferPath{}, protocol.ProvideMode_Network, dnsQueryPacketTyped(t, localZoneAddressName+".", dnsmessage.TypeAAAA, id), 0) {
		t.Fatal("not claimed")
	}
	_, _, answers := parseDnsBlockedReply(t, rec.receivedPackets()[0])
	if len(answers) != 1 {
		t.Fatalf("answers = %d, want the zone's address", len(answers))
	}
}

// A query for an owned name stays on the device even when no reply can be built.
// The other local answers return unclaimed there, which here would hand the
// private name to the egress in the clear. No message holds 65,536 answers.
func TestUpgradeMuxLocalZoneUnbuildableReplyStaysOnTheDevice(t *testing.T) {
	forEachIpVersion(t, func(t *testing.T, ipVersion int) {
		mux, rec, closeMux := newResolverArpaTestMux(t)
		defer closeMux()
		zone := newLocalZoneTable()
		zone.records[localZoneAddressName] = localZoneRecord{
			addrs: slices.Repeat([]netip.Addr{testDocAddr(4, 7)}, 0x10000),
			ttl:   localZoneTtl,
		}
		mux.SetLocalZone(zone)

		const id = 0x7901
		query := dnsQueryPacketTypedVersion(t, ipVersion, localZoneAddressName+".", dnsmessage.TypeA, id)
		if !mux.SendPacket(TransferPath{}, protocol.ProvideMode_Network, query, 0) {
			t.Fatal("not claimed")
		}
		if sent, received := rec.counts(); sent != 0 || received != 0 {
			t.Fatalf("sent/received = %d/%d, want 0/0: nothing upstream and no reply", sent, received)
		}
		if response := mux.resolveDnsTcpQuery(localZoneQuery(t, localZoneAddressName, dnsmessage.TypeA, id)); response != nil {
			t.Fatalf("tcp replied with %d bytes", len(response))
		}
		if sent, _ := rec.counts(); sent != 0 {
			t.Fatalf("sent %d packets upstream", sent)
		}
	})
}

// The batch entry takes the same path as a single query.
func TestUpgradeMuxLocalZoneAnswersABatch(t *testing.T) {
	forEachIpVersion(t, func(t *testing.T, ipVersion int) {
		mux, rec, closeMux := newResolverArpaTestMux(t)
		defer closeMux()
		mux.SetLocalZone(newLocalZoneTable())

		queries := [][]byte{
			dnsQueryPacketTypedVersion(t, ipVersion, localZoneAddressName+".", dnsmessage.TypeA, 0x7a01),
			dnsQueryPacketTypedVersion(t, ipVersion, localZoneEmptyName+".", dnsTypeHttps, 0x7a02),
		}
		if sent := mux.SendPacketBatch(TransferPath{}, protocol.ProvideMode_Network, queries, 0); sent != len(queries) {
			t.Fatalf("batch handled %d packets, want %d", sent, len(queries))
		}
		if sent, received := rec.counts(); sent != 0 || received != len(queries) {
			t.Fatalf("sent/received = %d/%d, want 0/%d", sent, received, len(queries))
		}
	})
}
