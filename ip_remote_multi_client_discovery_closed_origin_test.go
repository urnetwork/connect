package connect

import (
	"net/http"
	"slices"
	"testing"
)

// The direct strategy owns one native transport pool across authorities.
// Keep B's original socket live while closing only A's proven connection.
func TestRepeatFindProvidersClosedOriginCannotBorrowOtherLiveSocket(t *testing.T) {
	strategy, direct := newDeliveryTestStrategy(t, nil)
	first := newDiscoveryReachabilityHttpFixture(t, strategy, nil)
	other := newDiscoveryReachabilityHttpFixture(t, strategy, nil)
	other.mustLookup(t)
	other.requireEvents(t, "HP", 1, 1)
	first.mustLookup(t)
	first.mustLookup(t)
	first.requireEvents(t, "HPP", 1, 1, 1)

	direct.mutex.Lock()
	pool := direct.httpPool
	direct.mutex.Unlock()
	if pool == nil {
		t.Fatal("the shared native HTTP pool is missing")
	}
	pool.stateLock.Lock()
	sockets := make([]*clientHttpPoolConn, 0, len(pool.conns))
	for socket := range pool.conns {
		sockets = append(sockets, socket)
	}
	pool.stateLock.Unlock()
	var firstSocket, otherSocket *clientHttpPoolConn
	for _, socket := range sockets {
		switch socket.RemoteAddr().String() {
		case first.server.Listener.Addr().String():
			firstSocket = socket
		case other.server.Listener.Addr().String():
			otherSocket = socket
		}
	}
	if len(sockets) != 2 || firstSocket == nil || otherSocket == nil || firstSocket == otherSocket {
		t.Fatalf("missing the two owned native sockets: %d", len(sockets))
	}
	if err := firstSocket.Close(); err != nil {
		t.Fatal(err)
	}
	pool.stateLock.Lock()
	onlyOtherRemains := len(pool.conns) == 1 && pool.conns[otherSocket] && !pool.conns[firstSocket]
	pool.stateLock.Unlock()
	if !onlyOtherRemains {
		t.Fatal("closing the first origin did not leave exactly the second socket in the original pool")
	}
	// A generic request on B proves that the other registered wrapper really
	// is live. It cannot create typed provider-discovery evidence for A.
	probe, err := http.NewRequestWithContext(t.Context(), http.MethodGet, other.server.URL+"/hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := strategy.HttpParallel(probe); err != nil {
		t.Fatal(err)
	}
	other.requireEvents(t, "HPH", 1, 1, 1)
	first.mustLookup(t)
	first.lock.Lock()
	after := slices.Clone(first.events)
	first.lock.Unlock()
	wrong := []discoveryReachabilityHttpEvent{{'H', 1}, {'P', 1}, {'P', 1}, {'P', 2}}
	if slices.Equal(after, wrong) {
		t.Fatal("DISCOVERY_CLOSED_ORIGIN_BORROWED_LIVE_FOREIGN_SOCKET old_origin_closed=1 other_origin_live=1 fresh_origin_post_without_hello=1")
	}
	first.requireEvents(t, "HPPHP", 1, 1, 1, 2, 2)
	t.Log("DISCOVERY_CLOSED_ORIGIN_REQUALIFIED old_origin_closed=1 other_origin_live=1 fresh_hello=1")
}
