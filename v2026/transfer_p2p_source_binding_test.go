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
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/v2026/protocol"
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
	if transportType == TransportTypeUnknown || transportType == TransportTypeP2p {
		route = make(Route)
	}

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

// A direct peer's unreadable record cannot create a recovery session. Once a
// legitimate sequence opens that peer's session, the same lane can request repair.
func TestP2pRouteBoundPeerUndecryptableWrapIsNacked(t *testing.T) {
	fixture := newForgedSourceFixture(t, nil)
	fixture.inbound <- forgedSourceWrappedFrame(t, fixture.peerId, fixture.client.ClientId())
	fixture.barrier(t)
	session := fixture.client.encryptionSessionManager.Lookup(fixture.peerId, sequenceTlsRoleServer, false)
	if session == nil {
		t.Fatal("legitimate sequence did not acquire its session")
	}
	session.stateLock.Lock()
	alreadyNacked := !session.lastUnknownWrapNackTime.IsZero()
	session.stateLock.Unlock()
	if alreadyNacked || fixture.unknownWrapNackSessionCount() != 0 {
		t.Fatal("direct record created its own recovery session")
	}
	fixture.inbound <- forgedSourceWrappedFrame(t, fixture.peerId, fixture.client.ClientId())
	fixture.awaitUnknownWrapNack(t)
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
// wrap on it cannot send feedback, including through a session held by a real peer.
func TestUnboundP2pRouteUndecryptableWrapCreatesNoSession(t *testing.T) {
	fixture := newForgedSourceFixture(t, func(settings *ClientSettings) {
		// every undecryptable wrap that reaches a session nacks
		settings.EncryptionSettings.UnknownWrapNackMinInterval = 1 * time.Nanosecond
	})
	unboundRoute := fixture.publishUnboundRoute(TransportTypeP2p)
	platformRoute := fixture.publishUnboundRoute(TransportTypeH3)

	claimedId := NewId()
	unboundRoute <- forgedSourceWrappedFrame(t, claimedId, fixture.client.ClientId())
	unboundRoute <- nil // an unbuffered handoff fences the preceding read
	fixture.barrier(t)
	AssertEqual(t, 0, forgedSourcePeerStateOf(fixture.client, map[Id]bool{claimedId: true}).sessionCount)
	select {
	case <-fixture.unknownWrapNacks:
		t.Fatal("an unbound route's wrap was nacked with no session")
	default:
	}

	// The platform source now has a session. The unbound lane still cannot
	// act on its behalf. The handoff fences the receive worker's decision.
	platformRoute <- forgedSourceWrappedFrame(t, claimedId, fixture.client.ClientId())
	fixture.awaitUnknownWrapNack(t)
	session := fixture.client.encryptionSessionManager.Lookup(claimedId, sequenceTlsRoleServer, false)
	session.stateLock.Lock()
	prior := session.lastUnknownWrapNackTime
	session.stateLock.Unlock()
	unboundRoute <- forgedSourceWrappedFrame(t, claimedId, fixture.client.ClientId())
	unboundRoute <- nil
	fixture.barrier(t)
	session.stateLock.Lock()
	emitted := session.lastUnknownWrapNackTime != prior
	session.stateLock.Unlock()
	if emitted {
		t.Fatal("unattributed lane emitted feedback through an unrelated existing session")
	}
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

// A custom route has no platform attribution just because its type is unknown.
func TestUnknownCarrierCannotAllocateRecoverySession(t *testing.T) {
	f := newForgedSourceFixture(t, nil)
	route := f.publishUnboundRoute(TransportTypeUnknown)
	peer := NewId()
	route <- forgedSourceWrappedFrame(t, peer, f.client.ClientId())
	route <- nil
	f.barrier(t)
	if f.client.encryptionSessionManager.Lookup(peer, sequenceTlsRoleServer, false) != nil || f.unknownWrapNackSessionCount() != 0 {
		t.Fatal("unattributed carrier allocated recovery state")
	}
}

// Reusing the existing peer costs no slot, and refusal on another peer
// must keep a Required sequence from ever treating nil as a cleartext session.
func TestOrdinaryEncryptionSessionsShareHardLimit(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		settings := closeWaitClientSettings()
		settings.EncryptionSettings.Mode = EncryptionModeRequired
		settings.EncryptionSettings.MaxSessionCount = 1
		ctx, cancel := context.WithCancel(t.Context())
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		defer func() { cancel(); client.CloseAndWait(context.Background()) }()
		peer, other := NewId(), NewId()
		manager := client.EncryptionSessionManager()
		held := manager.Acquire(peer, sequenceTlsRoleServer, false)
		if held == nil {
			t.Fatal("first session refused")
		}
		duplicate := manager.Acquire(peer, sequenceTlsRoleServer, false)
		if duplicate != held {
			t.Fatal("same session charged twice")
		}
		duplicate.Release()
		blocked := NewReceiveSequence(ctx, client, SourceId(other), NewId(), sequenceTlsRoleServer, false, DefaultReceiveBufferSettings())
		if blocked.session != nil || blocked.ctx.Err() == nil {
			t.Error("full manager admitted a new receiving peer")
		}
		if blocked.session != nil {
			blocked.session.Release()
		}
		blocked.cancel()
		held.Release()
		manager.Testing_DropSessions(peer)
		synctest.Wait()
		recovered := manager.Acquire(NewId(), sequenceTlsRoleServer, false)
		if recovered == nil {
			t.Fatal("worker exit did not restore capacity")
		}
		recovered.Release()
	})
}

// Even with encryption off, a busy peer cannot spend a second sequence slot.
// An admitted sequence keeps delivering and retirement returns the slot.
func TestPeerReceiveSequenceLimitKeepsCurrentService(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		settings := closeWaitClientSettings()
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.ReceiveBufferSettings.MaxPeerSequenceCount = 1
		c := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		defer func() { cancel(); c.CloseAndWait(context.Background()) }()
		route := make(Route, 1)
		c.RouteManager().UpdateTransport(NewReceiveGatewayTransportWithType(TransportTypeH1), []Route{route})
		a, b := NewId(), NewId()
		for _, p := range []Id{a, b} {
			c.ContractManager().AddNoContractPeer(p)
		}
		delivered := map[Id]int{}
		c.AddReceiveCallback(func(path TransferPath, frames []*protocol.Frame, _ Peer) { delivered[path.SourceId] += len(frames) })
		send := func(p, id Id, n uint64) {
			pack := newReceiveLoopTestPack(t, id, false, "ordinary")
			pack.SequenceNumber = n
			pack.Head = n == 0
			wire, err := ProtoMarshal(&protocol.TransferFrame{TransferPath: TransferPath{SourceId: p, DestinationId: c.ClientId()}.ToProtobuf(), Pack: pack})
			if err != nil {
				t.Fatal(err)
			}
			route <- wire
			synctest.Wait()
		}
		ai, bi := NewId(), NewId()
		send(a, ai, 0)
		send(b, bi, 0)
		send(a, ai, 1)
		if delivered[a] != 2 || delivered[b] != 0 {
			t.Errorf("full receive table delivered %v", delivered)
		}
		buffer := c.receiveBuffer
		buffer.mutex.Lock()
		for seq := range buffer.activeReceiveSequences {
			if seq.source.SourceId == a {
				seq.Cancel()
			}
		}
		buffer.mutex.Unlock()
		synctest.Wait()
		send(b, bi, 0)
		if delivered[b] != 1 {
			t.Error("retired receive generation did not return its slot")
		}
	})
}

// An exact physical peer lane bounds failed opens before trying more ciphers.
// Clear repair traffic still runs, and the next second gets a fresh allowance.
func TestDirectLaneBoundsFailedRecordOpens(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		log := &captureLogger{enabled: true}
		f := newForgedSourceFixture(t, func(settings *ClientSettings) { settings.Log = log })
		f.barrier(t)
		attempts := func() int {
			log.mu.Lock()
			defer log.mu.Unlock()
			n := 0
			for _, line := range log.info {
				if strings.Contains(line, "[cr]unwrap err =") {
					n++
				}
			}
			return n
		}
		for range 33 {
			f.inbound <- forgedSourceWrappedFrame(t, f.peerId, f.client.ClientId())
		}
		f.barrier(t)
		if n := attempts(); n != 32 {
			t.Fatalf("attempted %d failed opens in one second", n)
		}
		time.Sleep(time.Second)
		f.inbound <- forgedSourceWrappedFrame(t, f.peerId, f.client.ClientId())
		f.barrier(t)
		if n := attempts(); n != 33 {
			t.Fatalf("new window has %d total failed opens", n)
		}
	})
}
