package connect

// Regression tests for forged source ids on a direct peer (P2P) route
// (partner security researchers' report, 2026-11-16). A P2P association is
// set up through signaling the platform relays from an authenticated client,
// so the association's adjacent peer is known; the source id a frame on it
// names is the sender's word. Every forged undecryptable wrap made
// NotifyUndecryptableWrap create an encryption session and a send sequence
// for the named id and ask the platform for a contract to it, and every forged
// plaintext pack opened a receive sequence and a session for the named id
// before any contract check. The tests drive the client's own receive loop
// through a route published the way a P2P association publishes its receive
// lanes.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

// Frames each forged-source test sends, every one naming a distinct source.
const forgedSourceFrameCount = 256

// An out-of-band control that records the destination of every contract the
// client asks the platform for and refuses every request, so no contract is
// ever granted.
type forgedSourceRecordingOob struct {
	stateLock                    sync.Mutex
	createContractDestinationIds []Id
}

func (self *forgedSourceRecordingOob) SendControl(
	frames []*protocol.Frame,
	callback func(resultFrames []*protocol.Frame, err error),
) {
	for _, frame := range frames {
		if frame.MessageType == protocol.MessageType_TransferCreateContract {
			createContract := &protocol.CreateContract{}
			if err := proto.Unmarshal(frame.MessageBytes, createContract); err == nil {
				if destinationId, err := IdFromBytes(createContract.DestinationId); err == nil {
					func() {
						self.stateLock.Lock()
						defer self.stateLock.Unlock()
						self.createContractDestinationIds = append(self.createContractDestinationIds, destinationId)
					}()
				}
			}
		}
		// the control owns the frames it is handed
		MessagePoolReturn(frame.MessageBytes)
	}
	if callback != nil {
		HandleError(func() {
			callback(nil, errors.New("Not supported."))
		})
	}
}

// Contract requests whose destination is one of `peerIds`.
func (self *forgedSourceRecordingOob) createContractCount(peerIds map[Id]bool) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	count := 0
	for _, destinationId := range self.createContractDestinationIds {
		if peerIds[destinationId] {
			count += 1
		}
	}
	return count
}

// The per-peer state a client holds for any of `peerIds`.
type forgedSourcePeerState struct {
	sessionCount         int
	sendSequenceCount    int
	receiveSequenceCount int
}

func forgedSourcePeerStateOf(client *Client, peerIds map[Id]bool) forgedSourcePeerState {
	state := forgedSourcePeerState{}
	func() {
		client.encryptionSessionManager.stateLock.Lock()
		defer client.encryptionSessionManager.stateLock.Unlock()
		for key := range client.encryptionSessionManager.sessions {
			if peerIds[key.peerId] {
				state.sessionCount += 1
			}
		}
	}()
	func() {
		client.sendBuffer.mutex.Lock()
		defer client.sendBuffer.mutex.Unlock()
		for sendSequenceId := range client.sendBuffer.sendSequences {
			if peerIds[sendSequenceId.Destination] {
				state.sendSequenceCount += 1
			}
		}
	}()
	func() {
		client.receiveBuffer.mutex.Lock()
		defer client.receiveBuffer.mutex.Unlock()
		for receiveSequenceId := range client.receiveBuffer.receiveSequences {
			if peerIds[receiveSequenceId.Source.SourceId] {
				state.receiveSequenceCount += 1
			}
		}
	}()
	return state
}

// One client in the provider's encryption mode, its recording control, the
// adjacent peer of a P2P association, and the association's receive lane.
type forgedSourceFixture struct {
	client  *Client
	oob     *forgedSourceRecordingOob
	peerId  Id
	inbound Route
	// contents the receive callback delivered from the adjacent peer
	delivered chan string
	// one value per unknown-wrap nack the client handed to its send buffer
	unknownWrapNacks chan struct{}

	// the one sequence every barrier pack rides, in order
	barrierSequenceId Id

	stateLock sync.Mutex
	// receive routes published beside `inbound`, drained when the client ends
	extraRoutes []Route
	// the sequence number of the next barrier pack
	barrierSequenceNumber uint64
}

func newForgedSourceFixture(t *testing.T, configure func(settings *ClientSettings)) *forgedSourceFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	settings := DefaultClientSettings()
	// the providers' mode (sdk device_local_provider.go)
	settings.EncryptionSettings.Mode = EncryptionModeOpportunistic
	unknownWrapNacks := make(chan struct{}, forgedSourceFrameCount)
	settings.SendBufferSettings.beforeEncryptedControlPackForTest = func(ecBytes []byte) {
		ec := &protocol.EncryptedControl{}
		if err := proto.Unmarshal(ecBytes, ec); err != nil {
			return
		}
		if ec.ControlType == protocol.EncryptedControlType_EncryptedControlUnknownWrapNack {
			select {
			case unknownWrapNacks <- struct{}{}:
			default:
			}
		}
	}
	if configure != nil {
		configure(settings)
	}
	oob := &forgedSourceRecordingOob{}
	client := NewClient(ctx, NewId(), oob, settings)
	peerId := NewId()
	// the adjacent peer's own frames need no contract here, so a plaintext
	// pack from it is delivered and can serve as the barrier
	client.ContractManager().AddNoContractPeer(peerId)
	inbound := make(Route, forgedSourceFrameCount+8)
	publishP2pAssociationReceiveRoute(ctx, client, peerId, inbound)

	delivered := make(chan string, 8)
	client.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, peer Peer) {
		if source.SourceId != peerId {
			return
		}
		for _, frame := range frames {
			message, err := FromFrame(frame)
			if err != nil {
				continue
			}
			if simpleMessage, ok := message.(*protocol.SimpleMessage); ok {
				select {
				case delivered <- simpleMessage.Content:
				default:
				}
			}
		}
	})

	fixture := &forgedSourceFixture{
		client:            client,
		oob:               oob,
		peerId:            peerId,
		inbound:           inbound,
		delivered:         delivered,
		unknownWrapNacks:  unknownWrapNacks,
		barrierSequenceId: NewId(),
	}
	t.Cleanup(func() {
		cancel()
		join, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if err := client.CloseAndWait(join); err != nil {
			t.Error(err)
		}
		routes := func() []Route {
			fixture.stateLock.Lock()
			defer fixture.stateLock.Unlock()
			return append([]Route{inbound}, fixture.extraRoutes...)
		}()
		for _, route := range routes {
			for len(route) != 0 {
				MessagePoolReturn(<-route)
			}
		}
	})
	return fixture
}

// Publishes a receive route that binds no source beside the association's
// lane: a platform carrier, or with TransportTypeP2p a custom carrier.
func (self *forgedSourceFixture) publishUnboundRoute(transportType TransportType) Route {
	route := make(Route, forgedSourceFrameCount+8)
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.extraRoutes = append(self.extraRoutes, route)
	}()
	self.client.RouteManager().UpdateTransport(
		NewReceiveGatewayTransportWithType(transportType),
		[]Route{route},
	)
	return route
}

// Publishes `route` as the receive lane of a P2P association with `peerId`,
// through the same function a P2P association publishes its lanes with.
func publishP2pAssociationReceiveRoute(ctx context.Context, client *Client, peerId Id, route Route) {
	updateP2pConnectionReceiveRoutes(
		ctx,
		client.RouteManager(),
		[]p2pReceiveRouteLane{{
			transport:   NewReceiveGatewayTransportWithType(TransportTypeP2p),
			route:       route,
			reliability: CarrierReliabilityReliable,
		}},
		peerId,
		true,
	)
}

// One synthetic wrapped frame from `sourceId`: opaque bytes no session can
// open, with the client-role hint that pins the receiver's server session.
func forgedSourceWrappedFrame(t *testing.T, sourceId Id, destinationId Id) []byte {
	t.Helper()
	sessionRole := protocol.SequenceRole_SequenceRoleClient
	transferFrameBytes, err := proto.Marshal(&protocol.TransferFrame{
		TransferPath:           TransferPath{SourceId: sourceId, DestinationId: destinationId}.ToProtobuf(),
		EncryptedTransferFrame: make([]byte, 32),
		SessionRole:            &sessionRole,
	})
	if err != nil {
		t.Fatal(err)
	}
	return transferFrameBytes
}

// One plaintext pack from `sourceId` that asks for an ack.
func forgedSourcePackFrame(t *testing.T, sourceId Id, destinationId Id, content string) []byte {
	t.Helper()
	pack := newReceiveLoopTestPack(t, NewId(), false, content)
	transferFrameBytes, err := proto.Marshal(&protocol.TransferFrame{
		TransferPath: TransferPath{SourceId: sourceId, DestinationId: destinationId}.ToProtobuf(),
		Pack:         pack,
	})
	if err != nil {
		t.Fatal(err)
	}
	return transferFrameBytes
}

// Sends a plaintext pack from the adjacent peer on the association's lane and
// waits for its delivery. The receive loop handles one route in order, so the
// delivery follows the verdict on every frame sent before it on that route.
func (self *forgedSourceFixture) barrier(t *testing.T) {
	t.Helper()
	self.barrierOn(t, self.inbound)
}

// The barrier on `route`. Every barrier pack rides one sequence in order, so a
// later barrier continues the sequence rather than superseding it.
func (self *forgedSourceFixture) barrierOn(t *testing.T, route Route) {
	t.Helper()
	sequenceNumber := func() uint64 {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		sequenceNumber := self.barrierSequenceNumber
		self.barrierSequenceNumber += 1
		return sequenceNumber
	}()
	pack := newReceiveLoopTestPack(t, self.barrierSequenceId, false, "barrier")
	pack.SequenceNumber = sequenceNumber
	pack.Head = sequenceNumber == 0
	transferFrameBytes, err := proto.Marshal(&protocol.TransferFrame{
		TransferPath: TransferPath{SourceId: self.peerId, DestinationId: self.client.ClientId()}.ToProtobuf(),
		Pack:         pack,
	})
	if err != nil {
		t.Fatal(err)
	}
	route <- transferFrameBytes
	select {
	case content := <-self.delivered:
		AssertEqual(t, "barrier", content)
	case <-self.client.Done():
		t.Fatal("the client stopped")
	case <-time.After(30 * time.Second):
		t.Fatal("the adjacent peer's frame was not delivered")
	}
}

// Undecryptable wraps naming forged sources on a P2P association allocate
// nothing for those ids: no encryption session, no send sequence, no contract
// request to the platform.
func TestP2pRouteForgedSourceWrapsAllocateNoPeerState(t *testing.T) {
	fixture := newForgedSourceFixture(t, nil)
	forgedIds := map[Id]bool{}
	for range forgedSourceFrameCount {
		forgedId := NewId()
		forgedIds[forgedId] = true
		fixture.inbound <- forgedSourceWrappedFrame(t, forgedId, fixture.client.ClientId())
	}
	fixture.barrier(t)

	state := forgedSourcePeerStateOf(fixture.client, forgedIds)
	contractRequestCount := fixture.oob.createContractCount(forgedIds)
	if state.sessionCount != 0 || state.sendSequenceCount != 0 || contractRequestCount != 0 {
		t.Fatalf(
			"%d forged wraps allocated peer state: sessions=%d send sequences=%d contract requests=%d",
			forgedSourceFrameCount,
			state.sessionCount,
			state.sendSequenceCount,
			contractRequestCount,
		)
	}
	AssertEqual(t, uint64(forgedSourceFrameCount), fixture.client.ReceiveStats().ForgedSourceDropCount)
}

// Plaintext packs naming forged sources on a P2P association allocate
// nothing for those ids: no receive sequence and no encryption session.
func TestP2pRouteForgedSourcePacksAllocateNoPeerState(t *testing.T) {
	fixture := newForgedSourceFixture(t, nil)
	forgedIds := map[Id]bool{}
	for range forgedSourceFrameCount {
		forgedId := NewId()
		forgedIds[forgedId] = true
		fixture.inbound <- forgedSourcePackFrame(t, forgedId, fixture.client.ClientId(), "forged")
	}
	fixture.barrier(t)

	state := forgedSourcePeerStateOf(fixture.client, forgedIds)
	if state.receiveSequenceCount != 0 || state.sessionCount != 0 {
		t.Fatalf(
			"%d forged packs allocated peer state: receive sequences=%d sessions=%d",
			forgedSourceFrameCount,
			state.receiveSequenceCount,
			state.sessionCount,
		)
	}
	AssertEqual(t, uint64(forgedSourceFrameCount), fixture.client.ReceiveStats().ForgedSourceDropCount)
}

// A client with encryption off can neither open a wrap nor answer a handshake,
// so an undecryptable wrap, even from the authenticated adjacent peer, opens
// no session and starts no nack.
func TestUndecryptableWrapCreatesNoSessionWhenEncryptionIsOff(t *testing.T) {
	fixture := newForgedSourceFixture(t, func(settings *ClientSettings) {
		settings.EncryptionSettings.Mode = EncryptionModeOff
	})
	fixture.inbound <- forgedSourceWrappedFrame(t, fixture.peerId, fixture.client.ClientId())
	fixture.barrier(t)

	state := forgedSourcePeerStateOf(fixture.client, map[Id]bool{fixture.peerId: true})
	AssertEqual(t, 0, state.sessionCount)
}

// Waits for one unknown-wrap nack to reach the send buffer.
func (self *forgedSourceFixture) awaitUnknownWrapNack(t *testing.T) {
	t.Helper()
	select {
	case <-self.unknownWrapNacks:
	case <-self.client.Done():
		t.Fatal("the client stopped")
	case <-time.After(30 * time.Second):
		t.Fatal("no unknown-wrap nack was sent")
	}
}

// The live sessions the undecryptable-wrap nack created.
func (self *forgedSourceFixture) unknownWrapNackSessionCount() int {
	manager := self.client.encryptionSessionManager
	manager.stateLock.Lock()
	defer manager.stateLock.Unlock()
	return manager.unknownWrapNackSessionCount
}

// Desync recovery still works for the association's own peer: its
// undecryptable wrap opens the session its re-handshake will drive and nacks
// the sealer, and its plaintext still arrives (the barrier).
func TestP2pRouteBoundPeerUndecryptableWrapIsNacked(t *testing.T) {
	fixture := newForgedSourceFixture(t, nil)
	fixture.inbound <- forgedSourceWrappedFrame(t, fixture.peerId, fixture.client.ClientId())
	fixture.awaitUnknownWrapNack(t)
	fixture.barrier(t)

	state := forgedSourcePeerStateOf(fixture.client, map[Id]bool{fixture.peerId: true})
	AssertEqual(t, 1, state.sessionCount)
	AssertEqual(t, 1, fixture.unknownWrapNackSessionCount())
	AssertEqual(t, uint64(0), fixture.client.ReceiveStats().ForgedSourceDropCount)
}

// The platform attributes the source of every frame it relays, so an
// undecryptable wrap it delivers still opens a session and nacks the sealer.
func TestPlatformRouteUndecryptableWrapIsNacked(t *testing.T) {
	fixture := newForgedSourceFixture(t, nil)
	platformRoute := fixture.publishUnboundRoute(TransportTypeH3)
	sealerId := NewId()
	platformRoute <- forgedSourceWrappedFrame(t, sealerId, fixture.client.ClientId())
	fixture.awaitUnknownWrapNack(t)
	fixture.barrierOn(t, platformRoute)

	state := forgedSourcePeerStateOf(fixture.client, map[Id]bool{sealerId: true})
	AssertEqual(t, 1, state.sessionCount)
}

// A P2P-typed carrier that binds no source vouches for none: an undecryptable
// wrap on it opens no session, and it is nacked only through a session the
// named source already holds.
func TestUnboundP2pRouteUndecryptableWrapCreatesNoSession(t *testing.T) {
	fixture := newForgedSourceFixture(t, func(settings *ClientSettings) {
		// every undecryptable wrap that reaches a session nacks
		settings.EncryptionSettings.UnknownWrapNackMinInterval = 1 * time.Nanosecond
	})
	unboundRoute := fixture.publishUnboundRoute(TransportTypeP2p)
	platformRoute := fixture.publishUnboundRoute(TransportTypeH3)

	claimedId := NewId()
	unboundRoute <- forgedSourceWrappedFrame(t, claimedId, fixture.client.ClientId())
	fixture.barrierOn(t, unboundRoute)
	AssertEqual(t, 0, forgedSourcePeerStateOf(fixture.client, map[Id]bool{claimedId: true}).sessionCount)
	select {
	case <-fixture.unknownWrapNacks:
		t.Fatal("an unbound route's wrap was nacked with no session")
	default:
	}

	// once the platform has delivered the source's wrap, the source holds a
	// session, and a wrap the unbound route names it on is nacked through it
	platformRoute <- forgedSourceWrappedFrame(t, claimedId, fixture.client.ClientId())
	fixture.awaitUnknownWrapNack(t)
	unboundRoute <- forgedSourceWrappedFrame(t, claimedId, fixture.client.ClientId())
	fixture.awaitUnknownWrapNack(t)
	AssertEqual(t, 1, forgedSourcePeerStateOf(fixture.client, map[Id]bool{claimedId: true}).sessionCount)
	AssertEqual(t, 1, fixture.unknownWrapNackSessionCount())
}

// The sessions the nack path creates are bounded client-wide, and a session
// that ends returns its place.
func TestUnknownWrapNackSessionCountIsBounded(t *testing.T) {
	fixture := newForgedSourceFixture(t, func(settings *ClientSettings) {
		settings.EncryptionSettings.UnknownWrapNackMaxSessionCount = 2
	})
	platformRoute := fixture.publishUnboundRoute(TransportTypeH3)

	sealerIds := []Id{NewId(), NewId(), NewId()}
	for _, sealerId := range sealerIds {
		platformRoute <- forgedSourceWrappedFrame(t, sealerId, fixture.client.ClientId())
	}
	fixture.barrierOn(t, platformRoute)
	AssertEqual(t, 1, forgedSourcePeerStateOf(fixture.client, map[Id]bool{sealerIds[0]: true}).sessionCount)
	AssertEqual(t, 1, forgedSourcePeerStateOf(fixture.client, map[Id]bool{sealerIds[1]: true}).sessionCount)
	AssertEqual(t, 0, forgedSourcePeerStateOf(fixture.client, map[Id]bool{sealerIds[2]: true}).sessionCount)
	AssertEqual(t, 2, fixture.unknownWrapNackSessionCount())

	// a session that ends is no longer counted, whoever ended it
	fixture.client.encryptionSessionManager.Testing_DropSessions(sealerIds[0])
	deadline := time.Now().Add(30 * time.Second)
	for fixture.unknownWrapNackSessionCount() != 1 {
		if deadline.Before(time.Now()) {
			t.Fatalf("the ended session is still counted: %d", fixture.unknownWrapNackSessionCount())
		}
		select {
		case <-fixture.client.Done():
			t.Fatal("the client stopped")
		case <-time.After(10 * time.Millisecond):
		}
	}
	platformRoute <- forgedSourceWrappedFrame(t, sealerIds[2], fixture.client.ClientId())
	fixture.barrierOn(t, platformRoute)
	AssertEqual(t, 1, forgedSourcePeerStateOf(fixture.client, map[Id]bool{sealerIds[2]: true}).sessionCount)
	AssertEqual(t, 2, fixture.unknownWrapNackSessionCount())
}
