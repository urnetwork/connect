// Peer-scoped controlled routes keep actual platform control ownership separate
// from the signed peer contract, application delivery, and acknowledgement path.
package connect

import (
	"bytes"
	"testing"
	"testing/synctest"
)

// Embedding the concrete gateway preserves its optional carrier-type method.
// Only the immutable destination predicate changes for this two-peer fixture.
type privateH1PeerGateway struct {
	*sendGatewayTransport
	peerId Id
}

// Every route generation retains the original carrier policy and peer identity.
func newPrivateH1PeerGateway(peerId Id, carrier TransportType) *privateH1PeerGateway {
	return &privateH1PeerGateway{
		sendGatewayTransport: NewSendGatewayTransportWithType(carrier),
		peerId:               peerId,
	}
}

// This route represents one peer, including its legitimate contract and ACK
// traffic. Platform and third-peer messages retain their separate route waits.
func (self *privateH1PeerGateway) MatchesSend(destination TransferPath) bool {
	return self.sendGatewayTransport.MatchesSend(destination) && destination == DestinationId(self.peerId)
}

// Registry membership includes both reader and writer transports. Select the
// one actual peer writer that owns this fixture's sender channel, outside locks.
func privateH1FixtureSendTransport(t *testing.T, fixture *windowRoundFixture) Transport {
	t.Helper()
	manager := fixture.sender.RouteManager()
	manager.mutex.Lock()
	var candidates []Transport
	for candidate, routes := range manager.writerMatchState.transportRoutes {
		for _, route := range routes {
			if route == fixture.senderOut {
				candidates = append(candidates, candidate)
				break
			}
		}
	}
	catalogCount := len(manager.writerMatchState.transportRoutes)
	manager.mutex.Unlock()
	var selected Transport
	eligibleCount := 0
	for _, candidate := range candidates {
		if candidate.MatchesSend(DestinationId(fixture.receiver.ClientId())) {
			selected = candidate
			eligibleCount += 1
		}
	}
	if eligibleCount != 1 {
		t.Fatalf("controlled H1 fixture peer writer count=%d catalog=%d", eligibleCount, catalogCount)
	}
	return selected
}

// Actual public Provide sends remain pending for the platform while one signed
// peer sequence completes through the real receiver and its actual ACK worker.
func TestCanonicalH1ProofFixtureIsolatesPlatformControl(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := privateH1NewFixture(t, 2, mib(1), false, TransportTypeH1, nil)
		if fixture.budget.UsedByteCount() != 0 {
			t.Fatal("completed OOB registration retained peer Transfer credit")
		}
		// Deliberately use the ordinary in-band API after setup so this root
		// tests a real ControlId send, not only a transport predicate.
		for _, client := range []*Client{fixture.sender, fixture.receiver} {
			client.ContractManager().SetProvideModes(client.ContractManager().GetProvideModes())
		}
		synctest.Wait()
		manager := fixture.sender.RouteManager()
		for _, destination := range []Id{ControlId, NewId()} {
			writer := manager.OpenMultiRouteWriter(DestinationId(destination))
			routes := writer.GetActiveRoutes()
			manager.CloseMultiRouteWriter(writer)
			if len(routes) != 0 {
				t.Fatalf("platform or third-peer control escaped peer-only gateway: destination=%v routes=%d", destination, len(routes))
			}
		}
		writer := manager.OpenMultiRouteWriter(DestinationId(fixture.receiver.ClientId()))
		routes := writer.GetActiveRoutes()
		policy := writer.(*MultiRouteSelector).transferFlightPolicy()
		manager.CloseMultiRouteWriter(writer)
		if len(routes) != 1 || routes[0] != fixture.senderOut || !policy.h1Only || !policy.h1MessageLimitsKnown || policy.h1MaxMessageByteCount != 16*1024 {
			t.Fatalf("peer-only wrapper lost its real H1 route or ceiling: routes=%d policy=%+v", len(routes), policy)
		}
		controlSequences := make(map[Id]*SendSequence)
		fixture.sender.sendBuffer.mutex.Lock()
		for sequenceId, sequence := range fixture.sender.sendBuffer.sendSequencesBySequenceId {
			if sequence.destination == ControlId {
				controlSequences[sequenceId] = sequence
			}
		}
		fixture.sender.sendBuffer.mutex.Unlock()
		if len(controlSequences) == 0 || len(fixture.senderOut) != 0 || len(fixture.receiverOut) != 0 {
			t.Fatal("public Provide was suppressed or reached a peer route")
		}
		beforePeer := fixture.budget.UsedByteCount()
		if admitted, err := fixture.send(64); !admitted || err != nil {
			t.Fatalf("isolated peer admission=%t/%v", admitted, err)
		}
		frame := fixture.data()
		path, err := FilteredTransferPath(frame.bytes)
		sequence := fixture.sequence()
		if err != nil || path.SourceId != fixture.sender.ClientId() || path.DestinationId != fixture.receiver.ClientId() || len(frame.pack.Frames) != 1 || !bytes.Equal(frame.pack.SequenceId, sequence.sequenceId.Bytes()) || sequence.sendContract == nil {
			t.Fatalf("captured peer data lost its original signed sequence: path=%+v err=%v", path, err)
		}
		if fixture.budget.UsedByteCount() <= beforePeer {
			t.Fatal("peer original did not retain its funded owner")
		}
		fixture.forward(frame, fixture.receiverIn)
		fixture.acknowledge()
		if fixture.ackedCount != 1 || fixture.deliveredCount != 1 || sequence.resendQueue.Len() != 0 || fixture.budget.UsedByteCount() != beforePeer {
			t.Fatalf("peer ACK did not exclusively release its original owner: ack=%d delivered=%d pending=%d used=%d before=%d", fixture.ackedCount, fixture.deliveredCount, sequence.resendQueue.Len(), fixture.budget.UsedByteCount(), beforePeer)
		}
		fixture.sender.sendBuffer.mutex.Lock()
		controlsUnchanged := true
		for sequenceId, sequence := range controlSequences {
			controlsUnchanged = controlsUnchanged && fixture.sender.sendBuffer.sendSequencesBySequenceId[sequenceId] == sequence
		}
		fixture.sender.sendBuffer.mutex.Unlock()
		if !controlsUnchanged || len(fixture.senderOut) != 0 || len(fixture.receiverOut) != 0 {
			t.Fatal("peer ACK consumed platform control or emitted unrelated peer traffic")
		}
	})
}

// The two legitimate registrations coexist in the catalog. Only the send
// gateway belongs to the peer writer; the receive gateway remains installed.
func TestCanonicalH1FixtureCatalogKeepsReceiveTransport(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newWindowRoundFixture(t, nil, nil, TransportTypeH1)
		manager := fixture.sender.RouteManager()
		manager.mutex.Lock()
		catalogCount := len(manager.writerMatchState.transportRoutes)
		manager.mutex.Unlock()
		if catalogCount != 2 {
			t.Fatalf("fixture did not preserve both legitimate transports: %d", catalogCount)
		}
		transport := privateH1FixtureSendTransport(t, fixture)
		manager.UpdateTransportWithProperties(transport, []Route{fixture.senderOut}, TransferCarrierProperties{MaxMessageByteCount: 16 * 1024})
		writer := manager.OpenMultiRouteWriter(DestinationId(fixture.receiver.ClientId()))
		routes := writer.GetActiveRoutes()
		policy := writer.(*MultiRouteSelector).transferFlightPolicy()
		manager.CloseMultiRouteWriter(writer)
		reader := manager.OpenMultiRouteReader(DestinationId(fixture.sender.ClientId()))
		receiveRoutes := reader.GetActiveRoutes()
		manager.CloseMultiRouteReader(reader)
		if len(routes) != 1 || routes[0] != fixture.senderOut || len(receiveRoutes) != 1 || receiveRoutes[0] != fixture.senderIn || !policy.h1Only || !policy.h1MessageLimitsKnown || policy.h1MaxMessageByteCount != 16*1024 {
			t.Fatalf("catalog selection changed the directional route or H1 ceiling: send=%d receive=%d policy=%+v", len(routes), len(receiveRoutes), policy)
		}
		if admitted, err := fixture.send(64); !admitted || err != nil {
			t.Fatalf("catalog peer admission=%t/%v", admitted, err)
		}
		frame := fixture.takePack(0)
		fixture.forward(frame, fixture.receiverIn)
		fixture.acknowledge()
		if fixture.ackedCount != 1 || fixture.deliveredCount != 1 {
			t.Fatal("catalog selection lost actual peer delivery or ACK")
		}
	})
}
