package connect

// Observes real decode53 wire writes after explicitly ordered requests. Pump
// admission barriers replace sleeps; expiry is advanced at the stored boundary.

import (
	"testing"
	"time"
)

// Owns one in-memory dns carrier and the requests its decoder has admitted.
type dnsEdnsTestServer struct {
	network     *memoryPacketNetwork
	conn        *memoryPacketConn
	translation *packetTranslation
	tld         []byte
	pumpAdded   chan *pumpItem
	nextId      uint16
}

// Creates a non-require-pump server so exhausted queues synthesize responses.
func newDnsEdnsTestServer(t *testing.T, maxPeers int64) *dnsEdnsTestServer {
	t.Helper()
	tld := []byte("pt.example.")
	network := newMemoryPacketNetwork()
	conn := network.listen(dnsCarrierTestAddr("192.0.2.1", 4053))
	settings := dnsCarrierTestSettings(tld)
	if maxPeers != 0 {
		settings.DnsMaxPumpHosts = maxPeers
	}
	translation, err := NewPacketTranslation(t.Context(), PacketTranslationModeDecode53, conn, settings)
	if err != nil {
		t.Fatal(err)
	}
	server := &dnsEdnsTestServer{
		network:     network,
		conn:        conn,
		translation: translation,
		tld:         tld,
		pumpAdded:   make(chan *pumpItem, 1),
	}
	translation.pumpAddedForTest = func(item *pumpItem, limit bool) {
		if limit {
			t.Error("test request exhausted the pump queue")
		}
		server.pumpAdded <- item
	}
	t.Cleanup(func() { translation.Close() })
	return server
}

// Each peer has an independent endpoint and capability history.
func (self *dnsEdnsTestServer) peer(t *testing.T, port int) *memoryPacketConn {
	t.Helper()
	peer := self.network.listen(dnsCarrierTestAddr("192.0.2.10", port))
	t.Cleanup(func() { peer.Close() })
	return peer
}

// A header-only request exercises the real decoder without retaining payloads.
func (self *dnsEdnsTestServer) request(t *testing.T, peer *memoryPacketConn, edns bool) *pumpItem {
	t.Helper()
	self.nextId++
	var buf [1024]byte
	header := dnsCarrierTestHeader(byte(self.nextId), 0, 0)
	encode := encodeDnsRequest
	if !edns {
		encode = legacyEncodeDnsRequest
	}
	_, datagram, err := encode(self.nextId, header, nil, buf, self.tld)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := peer.WriteTo(datagram, self.conn.addr); err != nil {
		t.Fatal(err)
	}
	select {
	case item := <-self.pumpAdded:
		if item.edns != edns || item.id != self.nextId || item.addr.String() != peer.addr.String() {
			t.Fatal("decoder admitted the wrong request or capability")
		}
		return item
	case <-time.After(dnsCarrierTestWait):
		t.Fatal("request did not reach the pump queue")
		return nil
	}
}

// A returned WriteTo orders all fragments before their raw wire inspection.
func (self *dnsEdnsTestServer) respond(t *testing.T, peer *memoryPacketConn, expectedEdns ...bool) {
	t.Helper()
	capacity := 191 - 18 + 160 - len(self.tld)
	response := dnsCarrierTestPacket((len(expectedEdns)-1)*capacity+1, 0x3c)
	if _, err := self.translation.WriteTo(response, peer.addr); err != nil {
		t.Fatal(err)
	}
	for i, edns := range expectedEdns {
		facts := parseDnsResponseFacts(t, "response", self.conn.nextWrite(t, "response"))
		expectedCount := 0
		if edns {
			expectedCount = 1
		}
		if facts.optCount != expectedCount {
			t.Fatalf("fragment %d: opt count=%d, want %d", i, facts.optCount, expectedCount)
		}
	}
}

// Depleting requests must not deplete the peer's latest advertised capability.
func TestPacketTranslationSynthesizedResponsesRetainEdns(t *testing.T) {
	server := newDnsEdnsTestServer(t, 0)
	peer := server.peer(t, 40000)
	server.request(t, peer, true)
	server.respond(t, peer, true)
	server.respond(t, peer, true)
}

// An older paired request keeps its own response shape, while synthetic tails
// use the latest request even after that newest pump item was already consumed.
func TestPacketTranslationMixedQueuedRequestsUseLatestEdns(t *testing.T) {
	for _, latestEdns := range []bool{true, false} {
		server := newDnsEdnsTestServer(t, 0)
		peer := server.peer(t, 40000)
		server.request(t, peer, !latestEdns)
		server.request(t, peer, latestEdns)
		server.respond(t, peer, latestEdns)
		server.respond(t, peer, !latestEdns, latestEdns)
	}
}

// Both capability transitions and two peers sharing an ip remain independent.
func TestPacketTranslationEdnsTransitionsStayPerPeer(t *testing.T) {
	server := newDnsEdnsTestServer(t, 0)
	first, second := server.peer(t, 40000), server.peer(t, 40001)
	server.request(t, first, true)
	server.request(t, second, false)
	server.respond(t, first, true)
	server.respond(t, second, false)
	server.respond(t, first, true)
	server.respond(t, second, false)
	server.request(t, first, false)
	server.request(t, second, true)
	server.respond(t, first, false)
	server.respond(t, second, true)
	server.respond(t, first, false)
	server.respond(t, second, true)
}

// A consumed request still arms expiry until its capability reaches the same
// inclusive timeout boundary. Sending responses never extends that lifetime.
func TestPacketTranslationEdnsCapabilityExpiresAfterQueueDrains(t *testing.T) {
	server := newDnsEdnsTestServer(t, 0)
	peer := server.peer(t, 40000)
	item := server.request(t, peer, true)
	server.respond(t, peer, true)
	if oldest, ok := server.translation.dnsPumpQueue.OldestUpdateTime(); !ok || !oldest.Equal(item.updateTime) {
		t.Fatal("drained pump queue no longer schedules capability expiry")
	}
	server.translation.dnsPumpQueue.RemoveOlder(item.updateTime)
	server.respond(t, peer, false)
	if _, ok := server.translation.dnsPumpQueue.OldestUpdateTime(); ok {
		t.Fatal("expired capability still retains an expiry timer")
	}
}

// Sequentially drained queues cannot leave an unbounded capability map behind.
func TestPacketTranslationEdnsCapabilityEvictsOldestPeer(t *testing.T) {
	server := newDnsEdnsTestServer(t, 2)
	first, second, third := server.peer(t, 40000), server.peer(t, 40001), server.peer(t, 40002)
	server.request(t, first, true)
	server.respond(t, first, true)
	server.request(t, second, true)
	server.respond(t, second, true)
	server.request(t, third, true)
	server.respond(t, third, true)
	server.respond(t, first, false)
	server.respond(t, second, true)
	server.respond(t, third, true)
	queue := server.translation.dnsPumpQueue
	queue.stateLock.Lock()
	defer queue.stateLock.Unlock()
	if len(queue.peerStateEntries) != 2 || queue.orderedPeerStates.Len() != 2 {
		t.Fatal("peer capability indexes exceeded their configured bound")
	}
}

// Closing joins every state owner before releasing both queued and drained
// peers, so retained translations do not retain retired capability state.
func TestPacketTranslationCloseReleasesEdnsState(t *testing.T) {
	server := newDnsEdnsTestServer(t, 0)
	peer := server.peer(t, 40000)
	server.request(t, peer, true)
	if err := server.translation.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := server.translation.dnsPumpQueue.OldestUpdateTime(); ok {
		t.Fatal("closed translation retained queued or capability state")
	}
	queue := server.translation.dnsPumpQueue
	queue.stateLock.Lock()
	defer queue.stateLock.Unlock()
	if len(queue.peerStateEntries) != 0 || len(queue.addrMaxHeap) != 0 || queue.orderedPeerStates.Len() != 0 {
		t.Fatal("closed translation retained queue or capability indexes")
	}
}

// A real request still changes synthetic capability when its consumable header
// is refused by the per-peer or global bound. Paired responses keep their bit.
func TestPumpEdnsCapabilityUpdatesWhenHeadersAreFull(t *testing.T) {
	for _, globalLimit := range []bool{false, true} {
		settings := DefaultPacketTranslationSettings()
		if globalLimit {
			settings.DnsMaxPumpHosts = 1
		} else {
			settings.DnsMaxPumpHostsPerAddress = 1
		}
		queue := newPumpQueue(settings)
		addr := dnsCarrierTestAddr("192.0.2.1", 4053)
		paired := &pumpItem{addr: addr, edns: true}
		if queue.Add(paired) {
			t.Fatal("first header was refused")
		}
		if !queue.Add(&pumpItem{addr: addr, edns: false}) {
			t.Fatal("second header did not reach the configured bound")
		}
		items, edns := queue.RemoveAvailable(addr, 2)
		if len(items) != 1 || items[0] != paired || !items[0].edns || edns {
			t.Fatal("header refusal lost the latest request or rewrote the paired request")
		}
	}
}

// A snapshot belongs to the response already being composed. A later real
// request changes only the next response, including transitions back to legacy.
func TestPumpEdnsResponseSnapshotSurvivesLaterRequest(t *testing.T) {
	queue := newPumpQueue(DefaultPacketTranslationSettings())
	addr := dnsCarrierTestAddr("192.0.2.1", 4053)
	queue.Add(&pumpItem{addr: addr, edns: true})
	items, firstEdns := queue.RemoveAvailable(addr, 2)
	queue.Add(&pumpItem{addr: addr, edns: false})
	newItems, nextEdns := queue.RemoveAvailable(addr, 2)
	if len(items) != 1 || !items[0].edns || !firstEdns || len(newItems) != 1 || newItems[0].edns || nextEdns {
		t.Fatal("a later request changed an existing response snapshot")
	}
}
