package connect

// The P2P source rule of SUBPROTOCOL.md §11. These run a real
// P2pReceiveTransport over an in-memory carrier into a real client, so a frame
// crosses every lane and the route manager before the client judges it;
// transport_p2p_source_webrtc_test.go runs the same rule over real associations.

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A carrier that hands its fast lane to the test.
type p2pSourceFastConn struct {
	net.Conn
	messages chan p2pFastPathReceivedMessage
}

func (self *p2pSourceFastConn) FastPathReady() bool {
	return true
}

func (self *p2pSourceFastConn) WaitFastPathReady(context.Context, time.Duration) bool {
	return true
}

func (self *p2pSourceFastConn) WriteFastPathMessage([]byte) (int, error) {
	return 1, nil
}

func (self *p2pSourceFastConn) FastPathMessages() <-chan p2pFastPathReceivedMessage {
	return self.messages
}

// A client with one P2P association, reached through its carrier's far end.
type p2pSourceFixture struct {
	*sourceRuleFixture
	peerId Id
	// the far end of the reliable lane
	remote net.Conn
	// the fast lane
	fast chan p2pFastPathReceivedMessage
	// one signal per frame P2pPeerSourceOnly refused
	drops chan struct{}
}

// Publishes the association as P2pTransport.run does: each lane names the
// adjacent peer. Frames in prefetched arrived ahead of the ready header.
func newP2pSourceFixture(t *testing.T, peerId Id, prefetched [][]byte, configure ...func(*ClientSettings)) *p2pSourceFixture {
	t.Helper()
	drops := make(chan struct{}, 64)
	configure = append(configure, func(settings *ClientSettings) {
		settings.afterP2pSourceDropForTest = func() {
			drops <- struct{}{}
		}
	})
	fixture := newSourceRuleFixture(t, TransportTypeUnknown, configure...)

	ctx, cancel := context.WithCancel(t.Context())
	local, remote := net.Pipe()
	t.Cleanup(func() {
		remote.Close()
	})
	fast := make(chan p2pFastPathReceivedMessage, 8)
	settings := DefaultP2pTransportSettings()
	transport, lanes := newP2pReceiveTransportWithLanes(
		ctx,
		cancel,
		&p2pSourceFastConn{Conn: local, messages: fast},
		NewId(),
		settings,
		prefetched,
		nil,
		nil,
	)
	for laneIndex := range lanes {
		lanes[laneIndex].peerId = peerId
	}
	if !updateP2pConnectionReceiveRoutes(ctx, fixture.client.RouteManager(), lanes, true) {
		t.Fatal("the association's routes were not published")
	}
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		if err := transport.CloseAndWait(closeCtx); err != nil {
			t.Error(err)
		}
	})
	return &p2pSourceFixture{
		sourceRuleFixture: fixture,
		peerId:            peerId,
		remote:            remote,
		fast:              fast,
		drops:             drops,
	}
}

// One message on the reliable lane. The pipe holds no bytes, so the carrier has
// read it when this returns.
func (self *p2pSourceFixture) writeReliable(sequence *sourceRuleSequence, frames ...*protocol.Frame) {
	self.t.Helper()
	wire := sourceRulePackBytes(self.t, self.client.ClientId(), sequence, frames...)
	defer MessagePoolReturn(wire)
	self.remote.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := self.remote.Write(wire); err != nil {
		self.t.Fatal(err)
	}
}

// One message on the fast lane, which takes the pooled bytes.
func (self *p2pSourceFixture) writeFast(sequence *sourceRuleSequence, frames ...*protocol.Frame) {
	self.t.Helper()
	wire := sourceRulePackBytes(self.t, self.client.ClientId(), sequence, frames...)
	self.fast <- p2pFastPathReceivedMessage{message: wire, fragmentCount: 1}
}

func (self *p2pSourceFixture) requireDrop() {
	self.t.Helper()
	select {
	case <-self.drops:
	case <-time.After(30 * time.Second):
		self.t.Fatal("the client did not refuse the frame")
	}
}

func requireP2pPeerSourceOnly(settings *ClientSettings) {
	settings.P2pPeerSourceOnly = true
}

// Only the peer. A forged control id needs no contract and has no session, and
// the client's own id needs no contract, so both would otherwise pass every
// gate behind the carrier. The third client is a no-contract peer here for the
// same reason: only the carrier rule can stop it.
func TestP2pPeerSourceOnlyAcceptsOnlyThePeer(t *testing.T) {
	peerId := NewId()
	fixture := newP2pSourceFixture(t, peerId, nil, requireP2pPeerSourceOnly)
	receipts := newSubprotocolSourceReceipts()
	receipts.register(t, fixture.client, subprotocolSourceTestId, WithSubprotocolControlSource())
	thirdId := NewId()
	fixture.client.ContractManager().AddNoContractPeer(thirdId)
	fixture.client.ContractManager().AddNoContractPeer(peerId)

	for _, sourceId := range []Id{ControlId, fixture.client.ClientId(), thirdId} {
		fixture.writeReliable(
			newSourceRuleSequence(sourceId),
			sourceRuleSimpleFrame("forged"),
			subprotocolTestFrame(subprotocolSourceTestId, []byte("forged")),
		)
		fixture.requireDrop()
	}

	sequence := newSourceRuleSequence(peerId)
	fixture.writeReliable(
		sequence,
		sourceRuleSimpleFrame("honest"),
		subprotocolTestFrame(subprotocolSourceTestId, []byte("honest")),
	)
	delivery := fixture.requireDelivery("honest")
	if delivery.source.SourceId != peerId || delivery.peer.TransportType != TransportTypeP2p {
		t.Fatalf("delivery %+v", delivery)
	}
	for _, listener := range []string{"raw", "typed"} {
		received := receipts.get(listener)
		if len(received) != 1 || received[0].payload != "honest" || received[0].source.SourceId != peerId {
			t.Fatalf("%s received %+v", listener, received)
		}
	}
	if len(fixture.deliveries) != 0 {
		t.Fatalf("a forged frame was delivered: %+v", <-fixture.deliveries)
	}
	stats := fixture.client.ReceiveStats()
	if stats.P2pSourceDropCount != 3 || stats.P2pSourceDropByteCount == 0 {
		t.Fatalf("stats %+v", stats)
	}
	if stats := fixture.client.SubprotocolStats(); stats.DroppedControlSource != 0 {
		t.Fatalf("the carrier rule should have stopped these first: %+v", stats)
	}
}

// Every way into the route: frames that beat the ready header, the reliable
// lane and the fast lane. One forgotten lane would reopen the whole class.
func TestP2pPeerSourceOnlyHoldsOnEveryLane(t *testing.T) {
	peerId := NewId()
	prefetched := [][]byte{
		sourceRulePackBytes(t, Id{}, newSourceRuleSequence(ControlId), sourceRuleSimpleFrame("forged")),
	}
	// the fixture's client id is not known yet; a pack for another destination
	// is judged the same way, since the rule runs before routing
	fixture := newP2pSourceFixture(t, peerId, prefetched, requireP2pPeerSourceOnly)
	fixture.client.ContractManager().AddNoContractPeer(peerId)
	fixture.requireDrop()

	fixture.writeReliable(newSourceRuleSequence(ControlId), sourceRuleSimpleFrame("forged"))
	fixture.requireDrop()
	fixture.writeFast(newSourceRuleSequence(ControlId), sourceRuleSimpleFrame("forged"))
	fixture.requireDrop()

	// one sequence across both lanes, as a path-independent sender writes it
	sequence := newSourceRuleSequence(peerId)
	fixture.writeReliable(sequence, sourceRuleSimpleFrame("reliable"))
	fixture.requireDelivery("reliable")
	fixture.writeFast(sequence, sourceRuleSimpleFrame("fast"))
	fixture.requireDelivery("fast")

	if stats := fixture.client.ReceiveStats(); stats.P2pSourceDropCount != 3 {
		t.Fatalf("stats %+v", stats)
	}
}

// A route that names no peer has nothing to compare, so it fails closed,
// whatever the frame claims, the zero id included.
func TestP2pPeerSourceOnlyRefusesARouteWithNoPeer(t *testing.T) {
	fixture := newP2pSourceFixture(t, Id{}, nil, requireP2pPeerSourceOnly)
	someoneId := NewId()
	fixture.client.ContractManager().AddNoContractPeer(someoneId)

	fixture.writeReliable(newSourceRuleSequence(someoneId), sourceRuleSimpleFrame("nobody's"))
	fixture.requireDrop()
	fixture.writeReliable(newSourceRuleSequence(ControlId), sourceRuleSimpleFrame("nobody's"))
	fixture.requireDrop()

	if len(fixture.deliveries) != 0 {
		t.Fatalf("a route with no peer delivered %+v", <-fixture.deliveries)
	}
}

// The deprecated constructor publishes a P2P route the caller wired by hand,
// with no peer at all. It fails closed too, which is why the rule keys on the
// carrier's type and not on a route having declared a peer.
func TestP2pPeerSourceOnlyRefusesTheDeprecatedPeerlessRoute(t *testing.T) {
	drops := make(chan struct{}, 8)
	fixture := newSourceRuleFixture(t, TransportTypeUnknown, requireP2pPeerSourceOnly, func(settings *ClientSettings) {
		settings.afterP2pSourceDropForTest = func() {
			drops <- struct{}{}
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	local, remote := net.Pipe()
	defer remote.Close()
	transport, route := NewP2pReceiveTransport(ctx, cancel, local, NewId(), DefaultP2pTransportSettings())
	fixture.client.RouteManager().UpdateTransportWithProperties(
		transport,
		[]Route{route},
		p2pTransferCarrierProperties(transport),
	)
	defer func() {
		fixture.client.RouteManager().RemoveTransport(transport)
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		if err := transport.(P2pRouteLifecycle).CloseAndWait(closeCtx); err != nil {
			t.Error(err)
		}
	}()

	wire := sourceRulePackBytes(t, fixture.client.ClientId(), newSourceRuleSequence(NewId()), sourceRuleSimpleFrame("x"))
	defer MessagePoolReturn(wire)
	remote.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := remote.Write(wire); err != nil {
		t.Fatal(err)
	}
	select {
	case <-drops:
	case <-time.After(30 * time.Second):
		t.Fatal("the peerless route's frame was not refused")
	}
}

// A refused frame is judged before routing, so an opted-in client also forwards
// nothing a P2P peer addressed elsewhere under a false source.
func TestP2pPeerSourceOnlyRefusesForwardedFrames(t *testing.T) {
	peerId := NewId()
	fixture := newP2pSourceFixture(t, peerId, nil, requireP2pPeerSourceOnly)
	forwards := make(chan TransferPath, 8)
	fixture.client.AddForwardCallback(func(path TransferPath, _ []byte) {
		forwards <- path
	})
	otherId := NewId()

	forged := newSourceRuleSequence(NewId())
	wire := sourceRulePackBytes(t, otherId, forged, sourceRuleSimpleFrame("forged"))
	defer MessagePoolReturn(wire)
	fixture.remote.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := fixture.remote.Write(wire); err != nil {
		t.Fatal(err)
	}
	fixture.requireDrop()

	// the peer's own frame for another destination is not the carrier's business
	honest := sourceRulePackBytes(t, otherId, newSourceRuleSequence(peerId), sourceRuleSimpleFrame("x"))
	defer MessagePoolReturn(honest)
	if _, err := fixture.remote.Write(honest); err != nil {
		t.Fatal(err)
	}
	select {
	case path := <-forwards:
		if path.SourceId != peerId || path.DestinationId != otherId {
			t.Fatalf("forwarded %s", path)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the peer's frame was not forwarded")
	}
	if len(forwards) != 0 {
		t.Fatal("a refused frame was forwarded")
	}
}

// The rule is about P2P carriers. A platform route of the same client keeps
// delivering the control id and any client it attests.
func TestP2pPeerSourceOnlyLeavesPlatformCarriersAlone(t *testing.T) {
	fixture := newSourceRuleFixture(t, TransportTypeH1, requireP2pPeerSourceOnly)
	thirdId := NewId()
	fixture.client.ContractManager().AddNoContractPeer(thirdId)

	fixture.inject(newSourceRuleSequence(ControlId), sourceRuleSimpleFrame("control"))
	if delivery := fixture.requireDelivery("control"); !delivery.source.IsControlSource() {
		t.Fatalf("delivery %+v", delivery)
	}
	fixture.inject(newSourceRuleSequence(thirdId), sourceRuleSimpleFrame("third"))
	if delivery := fixture.requireDelivery("third"); delivery.source.SourceId != thirdId {
		t.Fatalf("delivery %+v", delivery)
	}
	if stats := fixture.client.ReceiveStats(); stats.P2pSourceDropCount != 0 {
		t.Fatalf("stats %+v", stats)
	}
}

// The read loop returns a refused frame's pool buffer, the only owner left: a
// share taken before the handoff ends up as the last reference.
func TestP2pPeerSourceOnlyReturnsTheRefusedBuffer(t *testing.T) {
	drops := make(chan struct{}, 1)
	fixture := newSourceRuleFixture(t, TransportTypeUnknown, requireP2pPeerSourceOnly, func(settings *ClientSettings) {
		settings.afterP2pSourceDropForTest = func() {
			drops <- struct{}{}
		}
	})
	peerId := NewId()
	route := make(chan []byte)
	fixture.client.RouteManager().UpdateTransportWithProperties(
		NewReceiveGatewayTransportWithType(TransportTypeP2p),
		[]Route{route},
		TransferCarrierProperties{receivePeerId: peerId},
	)

	wire := sourceRulePackBytes(t, fixture.client.ClientId(), newSourceRuleSequence(ControlId), sourceRuleSimpleFrame("forged"))
	share := MessagePoolShareReadOnly(wire)
	select {
	case route <- wire:
	case <-time.After(30 * time.Second):
		t.Fatal("the client did not read its route")
	}
	select {
	case <-drops:
	case <-time.After(30 * time.Second):
		t.Fatal("the frame was not refused")
	}
	// the buffer goes back just after the counter, on the same worker: a second
	// frame through the loop orders the check after it
	next := sourceRulePackBytes(t, fixture.client.ClientId(), newSourceRuleSequence(peerId), sourceRuleSimpleFrame("next"))
	fixture.client.ContractManager().AddNoContractPeer(peerId)
	select {
	case route <- next:
	case <-time.After(30 * time.Second):
		t.Fatal("the client did not read its route")
	}
	if !MessagePoolReturn(share) {
		t.Fatal("the client kept a reference to the refused frame")
	}
	fixture.requireDelivery("next")
}
