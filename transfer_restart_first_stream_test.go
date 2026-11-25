package connect

// A client restarted in place, under its kept identity, opens its first stream
// to a listener that still holds its old sequences (QD-1232). Two delays sat in
// that path, each at one of connect's timer steps:
//
//   - The listener's receive buffer replaces the old process's receive
//     sequence when the restarted client's new sequence arrives. It dropped the
//     new sequence's first Packs while the old worker exited, its head and
//     ClientHello among them, so the handshake waited for the restarted
//     client's cold resend at MinResendInterval (2 s).
//   - The restarted client starts the listener's old send sequence at the first
//     Pack that reaches it, the answer to its open, and holds that behind the
//     Packs the old process never acknowledged, which the listener resends only
//     at their backoff, up to MaxResendInterval.
//
// The receive-buffer tests run on the synctest clock, with barriers on the
// workers' real exit and wait points. The restart tests use real clients on
// conditioned loopback routes, so their bounds are real time, each one a broken
// run's, and never a sleep.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A receive buffer whose workers can be held at their exit, as a worker parked
// in a callback holds it, and whose replacements report reaching their wait.
type replacementFixture struct {
	t         *testing.T
	ctx       context.Context
	client    *Client
	source    TransferPath
	delivered chan string
	// the receiving client's writes, its acks among them
	output Route

	stateLock sync.Mutex
	// a sequence id whose worker's exit waits for its release
	holds map[Id]chan struct{}
	// sequence ids whose workers reached their predecessor wait
	waiting map[Id]bool
	// called for each delivered payload with the delivering sequence's id
	onDeliver func(string)
}

func newReplacementFixture(t *testing.T) *replacementFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f := &replacementFixture{
		t:         t,
		ctx:       ctx,
		source:    SourceId(NewId()),
		delivered: make(chan string, 16),
		holds:     map[Id]chan struct{}{},
		waiting:   map[Id]bool{},
	}
	settings := closeWaitClientSettings()
	settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
	settings.EncryptionSettings.beforeEncryptedKeyPublishForTest = func() { <-ctx.Done() }
	settings.ReceiveBufferSettings.AckCompressTimeout = 0
	settings.ReceiveBufferSettings.beforeAckWorkerStopForTest = func(id receiveSequenceId) {
		f.stateLock.Lock()
		release := f.holds[id.SequenceId]
		f.stateLock.Unlock()
		if release != nil {
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
	}
	settings.ReceiveBufferSettings.beforePredecessorWaitForTest = func(id receiveSequenceId) {
		f.stateLock.Lock()
		defer f.stateLock.Unlock()
		f.waiting[id.SequenceId] = true
	}
	f.client = NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	f.client.ContractManager().AddNoContractPeer(f.source.SourceId)
	f.output = make(Route, 64)
	f.client.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{f.output})
	f.client.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, _ Peer) {
		for _, frame := range frames {
			payload := string(frame.MessageBytes)
			f.stateLock.Lock()
			onDeliver := f.onDeliver
			f.stateLock.Unlock()
			if onDeliver != nil {
				onDeliver(payload)
			}
			select {
			case f.delivered <- payload:
			default:
				t.Error("the fixture's delivery capacity was exceeded")
			}
		}
	})
	t.Cleanup(func() {
		f.releaseAll()
		cancel()
		if err := f.client.CloseAndWait(context.Background()); err != nil {
			t.Errorf("join the receiving client: %v", err)
		}
		for len(f.output) != 0 {
			MessagePoolReturn(<-f.output)
		}
	})
	return f
}

// One acknowledgement the receiving client wrote for the fixture's source.
type replacementFixtureAck struct {
	messageId        Id
	selective        bool
	contractMissing  bool
	nothingDelivered bool
}

// Reads every acknowledgement written so far, in order.
func (self *replacementFixture) takeAcks() []replacementFixtureAck {
	self.t.Helper()
	var acks []replacementFixtureAck
	for {
		select {
		case wire := <-self.output:
			var frame protocol.TransferFrame
			err := ProtoUnmarshal(wire, &frame)
			MessagePoolReturn(wire)
			if err != nil {
				self.t.Fatalf("a written frame does not decode: %v", err)
			}
			if frame.Ack == nil {
				continue
			}
			messageId, err := IdFromBytes(frame.Ack.MessageId)
			if err != nil {
				self.t.Fatalf("an ack names no message: %v", err)
			}
			acks = append(acks, replacementFixtureAck{
				messageId:        messageId,
				selective:        frame.Ack.Selective,
				contractMissing:  0 < len(frame.Ack.MissingContractId),
				nothingDelivered: frame.Ack.NothingDelivered,
			})
		default:
			return acks
		}
	}
}

// Holds the worker of sequenceId at its exit until the returned release runs.
func (self *replacementFixture) hold(sequenceId Id) func() {
	release := make(chan struct{})
	self.stateLock.Lock()
	self.holds[sequenceId] = release
	self.stateLock.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() { close(release) })
	}
}

func (self *replacementFixture) releaseAll() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for sequenceId, release := range self.holds {
		select {
		case <-release:
		default:
			close(release)
		}
		delete(self.holds, sequenceId)
	}
}

func (self *replacementFixture) reachedWait(sequenceId Id) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.waiting[sequenceId]
}

// Hands pack to the buffer as the shared receive pump does, with a zero handoff
// timeout, and fails unless the buffer took it.
func (self *replacementFixture) handOff(pack *protocol.Pack, byteCount ByteCount, what string) {
	self.t.Helper()
	wire, err := ProtoMarshal(&protocol.TransferFrame{
		TransferPath: sendTransferPath(self.source.SourceId, DestinationId(self.client.ClientId())).ToProtobuf(),
		Pack:         pack,
	})
	if err != nil {
		for _, frame := range pack.Frames {
			MessagePoolReturn(frame.MessageBytes)
		}
		self.t.Fatalf("marshal %s: %v", what, err)
	}
	receivePack := &ReceivePack{
		Source:             self.source,
		SequenceId:         RequireIdFromBytes(pack.SequenceId),
		Pack:               pack,
		ReceiveCallback:    self.client.receiveCallback,
		MessageByteCount:   byteCount,
		TransferFrameBytes: wire,
		TransportType:      TransportTypeH1,
		Ctx:                self.ctx,
		Unwrapped:          true,
		EncryptionRole:     sequenceTlsRoleServer,
	}
	accepted, err := self.client.receiveBuffer.Pack(receivePack, 0)
	if !accepted {
		receivePack.messagePoolReturn()
	}
	if !accepted || err != nil {
		self.t.Fatalf("the shared pump's handoff of %s: accepted=%t err=%v", what, accepted, err)
	}
}

// Offers one Pack of sequenceId and answers its message id.
func (self *replacementFixture) offer(sequenceId Id, number uint64, payload string) Id {
	self.t.Helper()
	messageId := NewId()
	self.handOff(&protocol.Pack{
		MessageId:      messageId.Bytes(),
		SequenceId:     sequenceId.Bytes(),
		SequenceNumber: number,
		Head:           number == 0,
		Frames: []*protocol.Frame{{
			MessageType:  protocol.MessageType_IpIpPacketToProvider,
			MessageBytes: MessagePoolCopy([]byte(payload)),
		}},
	}, ByteCount(len(payload)), "\""+payload+"\"")
	return messageId
}

// Offers the head of sequenceId with no frame, as a contract-only head is, and
// answers its message id.
func (self *replacementFixture) offerContractHead(sequenceId Id) Id {
	self.t.Helper()
	messageId := NewId()
	self.handOff(&protocol.Pack{
		MessageId:      messageId.Bytes(),
		SequenceId:     sequenceId.Bytes(),
		SequenceNumber: 0,
		Head:           true,
	}, 0, "a contract head")
	return messageId
}

// Offers a compact head of sequenceId, one that names contractId and carries
// no contract, and answers its message id.
func (self *replacementFixture) offerCompactHead(sequenceId Id, number uint64, contractId Id) Id {
	self.t.Helper()
	messageId := NewId()
	self.handOff(&protocol.Pack{
		MessageId:      messageId.Bytes(),
		SequenceId:     sequenceId.Bytes(),
		SequenceNumber: number,
		Head:           true,
		ContractId:     contractId.Bytes(),
	}, 0, "a compact head")
	return messageId
}

// The worker the buffer holds for sequenceId on the fixture's lane, nil for none.
func (self *replacementFixture) sequence(sequenceId Id) *ReceiveSequence {
	buffer := self.client.receiveBuffer
	buffer.mutex.Lock()
	defer buffer.mutex.Unlock()
	return buffer.receiveSequences[receiveSequenceId{
		Source:         self.source,
		SequenceId:     sequenceId,
		EncryptionRole: sequenceTlsRoleServer,
	}]
}

// Reads every payload delivered so far, in order.
func (self *replacementFixture) takeDelivered() []string {
	var payloads []string
	for {
		select {
		case payload := <-self.delivered:
			payloads = append(payloads, payload)
		default:
			return payloads
		}
	}
}

// A newer sequence's first Packs reach the buffer while the worker it replaces
// is still exiting. The shared pump hands them off at once with a zero timeout
// and drops none: the replacement is installed as the lane's head and holds
// them, reading nothing until the old worker has exited, and then delivers them
// in order. Before, each was dropped as a handoff and waited for its sender's
// resend, which a client restarted in place sends only at its cold
// MinResendInterval.
func TestReceiveBufferReplacementTakesPacksWhileItsPredecessorExits(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newReplacementFixture(t)
		oldSequenceId := NewId()
		newSequenceId := NewId()
		f.offer(oldSequenceId, 0, "old-0")
		synctest.Wait()
		if got := f.takeDelivered(); len(got) != 1 || got[0] != "old-0" {
			t.Fatalf("the old sequence delivered %v", got)
		}
		old := f.sequence(oldSequenceId)
		if old == nil {
			t.Fatal("the old sequence has no worker")
		}
		releaseOld := f.hold(oldSequenceId)
		var deliveredBeforeExit atomic.Int32
		f.stateLock.Lock()
		f.onDeliver = func(string) {
			select {
			case <-old.exit:
			default:
				deliveredBeforeExit.Add(1)
			}
		}
		f.stateLock.Unlock()

		f.offer(newSequenceId, 0, "new-0")
		f.offer(newSequenceId, 1, "new-1")
		synctest.Wait()
		replacement := f.sequence(newSequenceId)
		if stats := f.client.ReceiveStats(); stats.PackHandoffDropCount != 0 {
			t.Fatalf("the replacement's first Packs were dropped: %d handoff drops", stats.PackHandoffDropCount)
		}
		if old.ctx.Err() == nil {
			t.Fatal("the newer sequence did not retire the old worker")
		}
		if replacement == nil || !f.reachedWait(newSequenceId) ||
			replacement.predecessorExit != (<-chan struct{})(old.exit) ||
			replacement.packQueueCount.Load() != 2 {
			t.Fatal("the replacement is not waiting for the old worker with both Packs queued")
		}
		if got := f.takeDelivered(); len(got) != 0 {
			t.Fatalf("delivered %v while the replaced worker still ran", got)
		}

		releaseOld()
		synctest.Wait()
		if got := f.takeDelivered(); len(got) != 2 || got[0] != "new-0" || got[1] != "new-1" {
			t.Fatalf("after the old worker exited the replacement delivered %v", got)
		}
		if n := deliveredBeforeExit.Load(); n != 0 {
			t.Fatalf("the replacement delivered %d payloads before the old worker's exit", n)
		}
		if stats := f.client.ReceiveStats(); stats.PackHandoffDropCount != 0 {
			t.Fatalf("handoff drops after the replacement ran: %d", stats.PackHandoffDropCount)
		}
	})
}

// A third sequence replaces a replacement that is still waiting for the worker
// it replaced. The waiting one never ran, so the newest waits for the worker
// that did, not for the waiting one, and no chain of waiting workers forms. The
// waiting one exits without reading the Packs it held, which their sender
// resends, and the newest reads nothing until the running worker has exited.
func TestReceiveBufferReplacementOfAWaitingReplacementWaitsForTheRunningWorker(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newReplacementFixture(t)
		firstSequenceId := NewId()
		secondSequenceId := NewId()
		thirdSequenceId := NewId()
		f.offer(firstSequenceId, 0, "first-0")
		synctest.Wait()
		f.takeDelivered()
		first := f.sequence(firstSequenceId)
		releaseFirst := f.hold(firstSequenceId)

		f.offer(secondSequenceId, 0, "second-0")
		synctest.Wait()
		second := f.sequence(secondSequenceId)
		if second == nil || !f.reachedWait(secondSequenceId) {
			t.Fatal("the second sequence is not waiting for the first")
		}

		f.offer(thirdSequenceId, 0, "third-0")
		synctest.Wait()
		third := f.sequence(thirdSequenceId)
		if third == nil || !f.reachedWait(thirdSequenceId) ||
			third.predecessorExit != (<-chan struct{})(first.exit) {
			t.Fatal("the third sequence does not wait for the running first worker")
		}
		select {
		case <-second.exit:
		default:
			t.Fatal("the replaced waiting sequence did not exit while the first still ran")
		}
		if f.sequence(secondSequenceId) != nil {
			t.Fatal("the replaced waiting sequence is still indexed")
		}
		if got := f.takeDelivered(); len(got) != 0 {
			t.Fatalf("delivered %v while the first worker still ran", got)
		}

		releaseFirst()
		synctest.Wait()
		if got := f.takeDelivered(); len(got) != 1 || got[0] != "third-0" {
			t.Fatalf("after the first worker exited the lane delivered %v", got)
		}
		if stats := f.client.ReceiveStats(); stats.PackHandoffDropCount != 0 {
			t.Fatalf("handoff drops: %d", stats.PackHandoffDropCount)
		}
	})
}

// Closing the buffer ends a replacement still waiting for the worker it
// replaced. It exits at once without reading its Packs, since it never ran,
// and the buffer's join returns only once the held worker has exited too.
func TestReceiveBufferCloseEndsAWaitingReplacement(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newReplacementFixture(t)
		oldSequenceId := NewId()
		newSequenceId := NewId()
		f.offer(oldSequenceId, 0, "old-0")
		synctest.Wait()
		f.takeDelivered()
		old := f.sequence(oldSequenceId)
		releaseOld := f.hold(oldSequenceId)
		f.offer(newSequenceId, 0, "new-0")
		synctest.Wait()
		replacement := f.sequence(newSequenceId)
		if replacement == nil || !f.reachedWait(newSequenceId) {
			t.Fatal("the replacement is not waiting")
		}
		joined := make(chan error, 1)
		go func() {
			joined <- f.client.receiveBuffer.closeAndWait(context.Background())
		}()
		synctest.Wait()
		select {
		case <-replacement.done:
		default:
			t.Fatal("the closed replacement did not exit")
		}
		select {
		case <-old.done:
			t.Fatal("the held worker finished before its release")
		case <-joined:
			t.Fatal("the buffer's join returned while the held worker ran")
		default:
		}
		if got := f.takeDelivered(); len(got) != 0 {
			t.Fatalf("the closed replacement delivered %v", got)
		}
		releaseOld()
		synctest.Wait()
		select {
		case err := <-joined:
			if err != nil {
				t.Fatalf("the buffer's join: %v", err)
			}
		default:
			t.Fatal("the buffer's join did not return once both workers exited")
		}
	})
}

// The restart tests' bounds, each a broken run's, in real time.
type restartFirstStreamTestSettings struct {
	// How soon after the restart the restarted client's first message must
	// reach the listener, and the listener's answer the restarted client. Above
	// a handshake and a few round trips; below the cold resend and the
	// listener's backoff, which each test sets so that a broken run waits out
	// one of them.
	ReachTimeout time.Duration
	// Each step before the restart, and each barrier's wait.
	StepTimeout time.Duration
	// The restarted client's cold resend interval, what the client of a broken
	// listener waits for: well past ReachTimeout.
	ColdResendInterval time.Duration
	// The pair's TlsTimeout, as sidecar's test network sets it.
	TlsTimeout time.Duration
}

func defaultRestartFirstStreamTestSettings() *restartFirstStreamTestSettings {
	return &restartFirstStreamTestSettings{
		ReachTimeout:       10 * time.Second,
		StepTimeout:        30 * time.Second,
		ColdResendInterval: 60 * time.Second,
		TlsTimeout:         30 * time.Second,
	}
}

// A message one side received, with the lane a reply reproduces.
type restartFirstStreamMessage struct {
	content string
	key     TransferKey
}

// Two Required clients on conditioned loopback routes: a listener that stays
// up, and an edge that is restarted in place under its kept identity, as
// EdgeClientPool's client is (QD-1174), and opens to the listener.
type restartFirstStreamPair struct {
	t        *testing.T
	ctx      context.Context
	settings *restartFirstStreamTestSettings

	listener   *Client
	listenerId Id
	edgeId     Id
	// adjusts the edge's settings at each start, nil for none
	mutateEdge func(*ClientSettings)

	stateLock sync.Mutex
	edge      *Client
	identity  *restartedResponderIdentity
	// while set, the listener's writes toward the edge are lost, as they are
	// to a process that has exited
	edgeDown bool
	// while set, the edge's writes toward the listener that it answers true for
	// are lost; it borrows each wire message
	dropTowardListener func([]byte) bool

	edgeSend    chan []byte
	edgeReceive chan []byte
	listenerGot chan restartFirstStreamMessage
	edgeGot     chan restartFirstStreamMessage
}

func restartFirstStreamClientSettings(settings *restartFirstStreamTestSettings) *ClientSettings {
	s := DefaultClientSettingsWithBufferSize(64)
	s.SendBufferSettings.AckTimeout = 60 * time.Second
	s.SendBufferSettings.IdleTimeout = 1 * time.Second
	s.SendBufferSettings.MinResendInterval = 10 * time.Millisecond
	s.ReceiveBufferSettings.GapTimeout = 60 * time.Second
	s.ReceiveBufferSettings.IdleTimeout = 60 * time.Second
	s.ForwardBufferSettings.IdleTimeout = 1 * time.Second
	s.ContractManagerSettings.LegacyCreateContract = false
	s.EncryptionSettings.Mode = EncryptionModeRequired
	s.EncryptionSettings.TlsTimeout = settings.TlsTimeout
	return s
}

// A pair whose listener's settings mutateListener adjusts, nil for none.
func newRestartFirstStreamPair(
	t *testing.T,
	ctx context.Context,
	settings *restartFirstStreamTestSettings,
	mutateListener func(*ClientSettings),
	mutateEdge func(*ClientSettings),
) *restartFirstStreamPair {
	pair := &restartFirstStreamPair{
		t:           t,
		ctx:         ctx,
		settings:    settings,
		listenerId:  NewId(),
		edgeId:      NewId(),
		mutateEdge:  mutateEdge,
		edgeSend:    make(chan []byte),
		edgeReceive: make(chan []byte),
		listenerGot: make(chan restartFirstStreamMessage, 64),
		edgeGot:     make(chan restartFirstStreamMessage, 64),
	}
	listenerSend := make(chan []byte)
	_, towardListener := newConditioner(ctx, pair.edgeSend)
	listenerReceive := make(chan []byte)
	// the tap owns each frame it takes until it forwards or returns it
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case message := <-towardListener:
				if drop := pair.dropTowardListenerFunc(); drop != nil && drop(message) {
					MessagePoolReturn(message)
					continue
				}
				if !forwardOwnedTestWire(ctx, listenerReceive, message) {
					return
				}
			}
		}
	}()
	_, towardEdge := newConditioner(ctx, listenerSend)
	// the tap owns each frame it takes until it forwards or returns it
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case message := <-towardEdge:
				if pair.isEdgeDown() {
					MessagePoolReturn(message)
					continue
				}
				if !forwardOwnedTestWire(ctx, pair.edgeReceive, message) {
					return
				}
			}
		}
	}()

	listenerOob := &grantingClientOob{
		sourceId: pair.listenerId,
		settings: DefaultContractManagerSettings(),
		destSecretKey: func(destinationId Id) ([]byte, bool) {
			key, ok := pair.currentIdentity().provideSecretKeys[protocol.ProvideMode_Network]
			return key, ok
		},
		destClientPublicKey: func(destinationId Id) []byte {
			return pair.currentEdge().ClientKeyManager().PublicKey()
		},
	}
	listenerSettings := restartFirstStreamClientSettings(settings)
	if mutateListener != nil {
		mutateListener(listenerSettings)
	}
	pair.listener = NewClient(ctx, pair.listenerId, listenerOob, listenerSettings)
	pair.listener.RouteManager().UpdateTransport(newDataGatewayTransport(), []Route{listenerSend})
	pair.listener.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{listenerReceive})
	blackholeControlId(ctx, pair.listener.RouteManager())
	pair.listener.ContractManager().SetProvideModes(map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true})
	pair.listener.AddReceiveCallback(restartFirstStreamCollect(pair.listenerGot))

	pair.startEdge(nil)
	return pair
}

// A receive callback that hands each SimpleMessage on, with its reply lane.
func restartFirstStreamCollect(got chan restartFirstStreamMessage) ReceiveFunction {
	return func(source TransferPath, frames []*protocol.Frame, peer Peer) {
		for _, frame := range frames {
			if m, err := FromFrame(frame); err == nil {
				if sm, ok := m.(*protocol.SimpleMessage); ok {
					select {
					case got <- restartFirstStreamMessage{content: sm.Content, key: peer.TransferKey}:
					default:
					}
				}
			}
		}
	}
}

// Starts the edge, under identity when one is given and a fresh one otherwise.
func (self *restartFirstStreamPair) startEdge(identity *restartedResponderIdentity) {
	settings := restartFirstStreamClientSettings(self.settings)
	if self.mutateEdge != nil {
		self.mutateEdge(settings)
	}
	if identity != nil {
		settings.ClientKeySeed = identity.clientKeySeed
		settings.EncryptionSettings.ProvideTlsCertificatePem = identity.tlsCertificatePem
		settings.EncryptionSettings.ProvideTlsPrivateKeyPem = identity.tlsPrivateKeyPem
	}
	edgeOob := &grantingClientOob{
		sourceId: self.edgeId,
		settings: DefaultContractManagerSettings(),
		destSecretKey: func(destinationId Id) ([]byte, bool) {
			return self.listener.ContractManager().GetProvideSecretKey(protocol.ProvideMode_Network)
		},
		destClientPublicKey: func(destinationId Id) []byte {
			return self.listener.ClientKeyManager().PublicKey()
		},
	}
	edge := NewClient(self.ctx, self.edgeId, edgeOob, settings)
	if identity != nil {
		edge.ContractManager().LoadProvideSecretKeys(identity.provideSecretKeys)
	}
	blackholeControlId(self.ctx, edge.RouteManager())
	edge.ContractManager().SetProvideModes(map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true})
	edge.AddReceiveCallback(restartFirstStreamCollect(self.edgeGot))
	kept := &restartedResponderIdentity{
		clientKeySeed:     edge.ClientKeyManager().Seed(),
		provideSecretKeys: edge.ContractManager().GetProvideSecretKeys(),
		tlsCertificatePem: edge.EncryptionSessionManager().ProvideTlsCertificatePem(),
		tlsPrivateKeyPem:  edge.EncryptionSessionManager().ProvideTlsPrivateKeyPem(),
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.edge = edge
		self.identity = kept
		self.edgeDown = false
	}()
	edge.RouteManager().UpdateTransport(newDataGatewayTransport(), []Route{self.edgeSend})
	edge.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{self.edgeReceive})
}

func (self *restartFirstStreamPair) currentEdge() *Client {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.edge
}

func (self *restartFirstStreamPair) currentIdentity() *restartedResponderIdentity {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.identity
}

func (self *restartFirstStreamPair) isEdgeDown() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.edgeDown
}

func (self *restartFirstStreamPair) dropTowardListenerFunc() func([]byte) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.dropTowardListener
}

// Loses, from here, every write of the edge's toward the listener that drop
// answers true for; nil loses none.
func (self *restartFirstStreamPair) setDropTowardListener(drop func([]byte) bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.dropTowardListener = drop
}

// Stops the edge as a killed process stops, joins it, and answers the identity
// it kept. From here the listener's writes toward it are lost.
func (self *restartFirstStreamPair) stopEdge() *restartedResponderIdentity {
	self.t.Helper()
	kept := self.currentIdentity()
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.edgeDown = true
	}()
	joinCtx, cancelJoin := context.WithTimeout(self.ctx, self.settings.StepTimeout)
	defer cancelJoin()
	if err := self.currentEdge().CloseAndWait(joinCtx); err != nil {
		self.t.Fatalf("the edge did not stop: %v", err)
	}
	return kept
}

// Sends content from the edge to the listener.
func (self *restartFirstStreamPair) edgeSends(content string) {
	self.t.Helper()
	if !self.currentEdge().SendWithTimeout(requiredGateFrame(self.t, content), self.listenerId, func(error) {}, self.settings.StepTimeout) {
		self.t.Fatalf("the edge could not send %q", content)
	}
}

// Sends content from the listener to the edge on the lane key names, and
// answers a channel that closes when the edge acknowledges it.
func (self *restartFirstStreamPair) listenerSends(content string, key TransferKey) <-chan struct{} {
	self.t.Helper()
	acked := make(chan struct{})
	var once sync.Once
	ackCallback := func(err error) {
		if err == nil {
			once.Do(func() { close(acked) })
		}
	}
	if !self.listener.SendWithTimeout(requiredGateFrame(self.t, content), self.edgeId, ackCallback, self.settings.StepTimeout, key) {
		self.t.Fatalf("the listener could not send %q", content)
	}
	return acked
}

// Waits until deadline for got to carry content, and answers it.
func restartFirstStreamWait(got chan restartFirstStreamMessage, content string, deadline time.Time) (restartFirstStreamMessage, bool) {
	for {
		select {
		case message := <-got:
			if message.content == content {
				return message, true
			}
		case <-time.After(time.Until(deadline)):
			return restartFirstStreamMessage{}, false
		}
	}
}

func (self *restartFirstStreamPair) close() {
	self.listener.Cancel()
	self.currentEdge().Cancel()
}

// Runs the pair to the restart: the edge opens, the listener answers and writes
// once more on the open's lane, and each is acknowledged, so the listener holds
// nothing unacknowledged toward the edge; then the edge stops. Answers the
// identity it kept and the listener's reply lane.
func (self *restartFirstStreamPair) runToRestart() (*restartedResponderIdentity, TransferKey) {
	self.t.Helper()
	self.edgeSends("open")
	open, ok := restartFirstStreamWait(self.listenerGot, "open", time.Now().Add(self.settings.StepTimeout))
	if !ok {
		self.t.Fatal("the edge's first open did not reach the listener")
	}
	for _, content := range []string{"answer", "before"} {
		acked := self.listenerSends(content, open.key)
		if _, ok := restartFirstStreamWait(self.edgeGot, content, time.Now().Add(self.settings.StepTimeout)); !ok {
			self.t.Fatalf("the listener's %q did not reach the edge", content)
		}
		select {
		case <-acked:
		case <-time.After(self.settings.StepTimeout):
			self.t.Fatalf("the edge did not acknowledge %q", content)
		}
	}
	waitForSealedSession(self.t, self.ctx, self.listener)
	return self.stopEdge(), open.key
}

// The restarted edge's first open reaches the listener while the listener's
// worker for the edge's old sequence is still exiting, held at its exit
// barrier, and without the restarted edge's resend, which comes only at its
// cold interval, far past ReachTimeout: the listener keeps the new sequence's
// head and ClientHello in the replacement worker, which reads them once the
// old worker has exited. Before, the listener dropped them while the old worker
// exited, and the open waited for that resend.
func TestRestartedClientsFirstOpenReachesItsListenerWithoutItsResend(t *testing.T) {
	settings := defaultRestartFirstStreamTestSettings()
	ctx, cancel := context.WithTimeout(context.Background(), 3*settings.StepTimeout)
	defer cancel()

	var holdLock sync.Mutex
	var holding bool
	var edgeId Id
	held := make(chan struct{}, 8)
	release := make(chan struct{})
	replacementWaiting := make(chan struct{}, 8)
	pair := newRestartFirstStreamPair(t, ctx, settings, func(s *ClientSettings) {
		s.ReceiveBufferSettings.beforeAckWorkerStopForTest = func(id receiveSequenceId) {
			holdLock.Lock()
			hold := holding && id.Source.SourceId == edgeId
			holdLock.Unlock()
			if !hold {
				return
			}
			held <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		s.ReceiveBufferSettings.beforePredecessorWaitForTest = func(id receiveSequenceId) {
			holdLock.Lock()
			watch := holding && id.Source.SourceId == edgeId
			holdLock.Unlock()
			if watch {
				replacementWaiting <- struct{}{}
			}
		}
	}, func(s *ClientSettings) {
		s.SendBufferSettings.MinResendInterval = settings.ColdResendInterval
		s.SendBufferSettings.MaxResendInterval = settings.ColdResendInterval
	})
	defer pair.close()
	edgeId = pair.edgeId
	var releaseOnce sync.Once
	releaseHeld := func() {
		releaseOnce.Do(func() { close(release) })
	}
	defer releaseHeld()

	kept, _ := pair.runToRestart()
	func() {
		holdLock.Lock()
		defer holdLock.Unlock()
		holding = true
	}()
	startTime := time.Now()
	pair.startEdge(kept)
	// The Required gate holds the send until the restarted edge's session
	// seals, which takes the listener's answer to its ClientHello, which the
	// held worker holds back: so the send runs beside the barriers.
	restarted := pair.currentEdge()
	sent := make(chan bool, 1)
	go func() {
		sent <- restarted.SendWithTimeout(requiredGateFrame(t, "open after the restart"), pair.listenerId, func(error) {}, settings.StepTimeout)
	}()

	select {
	case <-held:
	case <-time.After(settings.StepTimeout):
		t.Fatal("the restarted edge's sequence did not replace the listener's worker for its old one")
	}
	select {
	case <-replacementWaiting:
	case <-time.After(settings.ReachTimeout):
		t.Fatalf("no replacement took the restarted edge's first Packs while the old worker exited (%d handoff drops)",
			pair.listener.ReceiveStats().PackHandoffDropCount)
	}
	releaseHeld()
	if _, ok := restartFirstStreamWait(pair.listenerGot, "open after the restart", startTime.Add(settings.ReachTimeout)); !ok {
		t.Fatalf("the restarted edge's first open did not reach the listener within %s (%d handoff drops)",
			settings.ReachTimeout, pair.listener.ReceiveStats().PackHandoffDropCount)
	}
	if !<-sent {
		t.Fatal("the restarted edge's send was refused")
	}
	if drops := pair.listener.ReceiveStats().PackHandoffDropCount; drops != 0 {
		t.Fatalf("the listener dropped %d of the restarted edge's Packs", drops)
	}
	t.Logf("the restarted edge's first open reached the listener %s after the restart", time.Since(startTime))
}

// A receive sequence that has delivered nothing holds a later Pack: the first
// Pack that reaches a receiver restarted in place, the answer to its first open.
// Its selective ack says so (Ack.nothing_delivered), so the sender writes what
// the receiver lacks at once. Once the head has come and the sequence has
// delivered, no ack says it, a later gap's included.
func TestReceiverThatHasDeliveredNothingSaysSo(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newReplacementFixture(t)
		sequenceId := NewId()
		answer := f.offer(sequenceId, 2, "answer")
		synctest.Wait()
		acks := f.takeAcks()
		if len(acks) != 1 || acks[0].messageId != answer || !acks[0].selective || !acks[0].nothingDelivered {
			t.Fatalf("the held Pack's acks %+v do not say the sequence delivered nothing", acks)
		}
		if got := f.takeDelivered(); len(got) != 0 {
			t.Fatalf("delivered %v with no head", got)
		}

		f.offer(sequenceId, 0, "head")
		f.offer(sequenceId, 1, "next")
		synctest.Wait()
		if got := f.takeDelivered(); len(got) != 3 || got[0] != "head" || got[1] != "next" || got[2] != "answer" {
			t.Fatalf("once the head came the sequence delivered %v", got)
		}
		for _, ack := range f.takeAcks() {
			if ack.nothingDelivered {
				t.Fatalf("an ack after the sequence delivered says it delivered nothing: %+v", ack)
			}
		}

		later := f.offer(sequenceId, 4, "later")
		synctest.Wait()
		acks = f.takeAcks()
		if len(acks) != 1 || acks[0].messageId != later || !acks[0].selective || acks[0].nothingDelivered {
			t.Fatalf("a gap after delivery was acked %+v, want a selective ack without nothing_delivered", acks)
		}
		if requests := f.client.SendRecoveryStats().NothingDeliveredAckCount; requests != 1 {
			t.Fatalf("%d nothing-delivered acks, want the one", requests)
		}
	})
}

// The other way a receiver restarted in place starts a sequence: the sender
// writes the sequence's head again in plaintext, since it carries only the
// contract, while the Packs behind it were sealed under keys the restarted
// receiver never had, so it delivers the head and drops them. It has delivered
// no frame, so the selective ack of the next Pack it can read still says so.
func TestReceiverThatDeliveredOnlyAContractHeadSaysItDeliveredNothing(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newReplacementFixture(t)
		sequenceId := NewId()
		head := f.offerContractHead(sequenceId)
		synctest.Wait()
		acks := f.takeAcks()
		if len(acks) != 1 || acks[0].messageId != head || acks[0].selective || acks[0].nothingDelivered {
			t.Fatalf("the contract head was acked %+v, want one cumulative ack", acks)
		}
		answer := f.offer(sequenceId, 3, "answer")
		synctest.Wait()
		acks = f.takeAcks()
		if len(acks) != 1 || acks[0].messageId != answer || !acks[0].selective || !acks[0].nothingDelivered {
			t.Fatalf("the held answer's acks %+v do not say the sequence delivered nothing", acks)
		}
		f.offer(sequenceId, 1, "one")
		f.offer(sequenceId, 2, "two")
		synctest.Wait()
		if got := f.takeDelivered(); len(got) != 3 || got[0] != "one" || got[1] != "two" || got[2] != "answer" {
			t.Fatalf("once the missing Packs came the sequence delivered %v", got)
		}
		for _, ack := range f.takeAcks() {
			if ack.nothingDelivered {
				t.Fatalf("an ack after the sequence delivered says it delivered nothing: %+v", ack)
			}
		}
	})
}

// A receive sequence that has delivered nothing gets a compact head whose
// contract it does not hold, as a receiver restarted in place gets the sender's
// resent head, so its request for the contract says it delivered nothing, and
// the sender writes everything again with the head. Once the sequence has
// delivered, a request for a contract it lacks asks for that alone.
func TestReceiverMissingAContractSaysWhetherItDeliveredNothing(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newReplacementFixture(t)
		sequenceId := NewId()
		resent := f.offerCompactHead(sequenceId, 0, NewId())
		synctest.Wait()
		acks := f.takeAcks()
		if len(acks) != 1 || acks[0].messageId != resent || !acks[0].contractMissing || !acks[0].nothingDelivered {
			t.Fatalf("the resent head's acks %+v do not ask for its contract saying the sequence delivered nothing", acks)
		}

		f.offer(sequenceId, 0, "head")
		synctest.Wait()
		if got := f.takeDelivered(); len(got) != 1 || got[0] != "head" {
			t.Fatalf("the sequence delivered %v, want its head", got)
		}
		f.takeAcks()
		compact := f.offerCompactHead(sequenceId, 1, NewId())
		synctest.Wait()
		acks = f.takeAcks()
		if len(acks) != 1 || acks[0].messageId != compact || !acks[0].contractMissing || acks[0].nothingDelivered {
			t.Fatalf("after delivery the compact head's acks are %+v, want a request for its contract alone", acks)
		}
		if requests := f.client.SendRecoveryStats().NothingDeliveredAckCount; requests != 1 {
			t.Fatalf("%d nothing-delivered sayings, want the one", requests)
		}
	})
}

// A selective ack that says the receiver has delivered nothing makes every Pack
// below it that the receiver has not acknowledged due at once, as receiver-state
// recovery, and each at most once: a repeated ack writes nothing again, and a
// later one only what lies above the first. A Pack already due keeps its time
// and takes the same recovery, and a Pack the receiver holds and the Packs above
// the acknowledged one keep theirs.
func TestSenderWritesEveryPackBelowANothingDeliveredAckAtOnce(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	now := base.Add(10 * time.Second)
	sequence, items := newSelectiveAckRecoveryTestSequence(8, base)
	items[2].selectiveAcked = true
	items[4].selectiveAcked = true
	sequence.setResendTime(items[1], base.Add(time.Second))
	var originalDue [8]time.Time
	for index, item := range items {
		originalDue[index] = item.resendTime
	}

	sequence.rewriteForLostReceiver(4, now)
	for index, item := range items {
		wantDue, wantKind := originalDue[index], sendRecoveryNone
		switch index {
		case 0, 3:
			wantDue, wantKind = now, sendRecoveryReceiverLost
		case 1:
			wantKind = sendRecoveryReceiverLost
		}
		if item.resendTime != wantDue || item.recoveryKind != wantKind {
			t.Fatalf("item %d after the nothing-delivered ack: due %s kind %d, want %s kind %d",
				index, item.resendTime.Sub(base), item.recoveryKind, wantDue.Sub(base), wantKind)
		}
		if item.sendCount != 1 || sequence.resendQueue.GetByMessageId(item.messageId) != item {
			t.Fatalf("item %d lost its backoff or its retained ownership", index)
		}
	}
	if first := sequence.resendQueue.PeekFirst(); first != items[1] ||
		sequence.resendQueue.cmp(items[0], items[3]) >= 0 {
		t.Fatal("the resend queue does not hold the already due Pack first and then the missing Packs oldest first")
	}

	later := now.Add(time.Second)
	for index := range items {
		items[index].recoveryKind = sendRecoveryNone
	}
	sequence.rewriteForLostReceiver(4, later)
	sequence.rewriteForLostReceiver(3, later)
	for index, item := range items {
		if item.recoveryKind != sendRecoveryNone {
			t.Fatalf("a repeated nothing-delivered ack wrote item %d again", index)
		}
	}

	sequence.rewriteForLostReceiver(7, later)
	for index, item := range items {
		wantKind := sendRecoveryNone
		if index == 5 || index == 6 {
			wantKind = sendRecoveryReceiverLost
		}
		if item.recoveryKind != wantKind {
			t.Fatalf("after a later nothing-delivered ack item %d has kind %d, want %d", index, item.recoveryKind, wantKind)
		}
	}
	if items[5].resendTime != later || items[7].resendTime != originalDue[7] {
		t.Fatal("the later ack did not make only the Packs above the first due")
	}
}

// A sender whose compact head and the two Packs written behind it wait for
// their acknowledgement, on their own timers.
type compactHeadBacklog struct {
	sequence   *SendSequence
	contractId Id
	head       *sendItem
	one        *sendItem
	two        *sendItem
}

func newCompactHeadBacklog(t *testing.T) *compactHeadBacklog {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	client, sequence, destinationId, contract := newSendNoContractHarness(t, ctx)
	sequence.sendBuffer = client.sendBuffer
	contract.compactContractRecoverySupported = true
	contract.contract = &protocol.Contract{StoredContractBytes: make([]byte, 256)}
	route := make(chan []byte, 3)
	client.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(destinationId)), []Route{route})
	for _, content := range []string{"head", "one", "two"} {
		frame := RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: content})
		if !contract.update(MessageByteCount([]*protocol.Frame{frame})) {
			t.Fatalf("the contract refused %q", content)
		}
		sequence.sendRecord([]*protocol.Frame{frame}, sendAckRecord{}, noAckSendRecord{}, true, false)
	}
	for range 3 {
		select {
		case wire := <-route:
			MessagePoolReturn(wire)
		case <-ctx.Done():
			t.Fatalf("wait for the writes: %v", ctx.Err())
		}
	}
	if len(sequence.sendItems) != 3 || !sequence.sendItems[0].head || sequence.sendItems[0].hasContractFrame {
		t.Fatal("the fixture did not write a compact head and two Packs behind it")
	}
	backlog := &compactHeadBacklog{
		sequence:   sequence,
		contractId: contract.contractId,
		head:       sequence.sendItems[0],
		one:        sequence.sendItems[1],
		two:        sequence.sendItems[2],
	}
	now := time.Now()
	if !backlog.one.resendTime.After(now) || !backlog.two.resendTime.After(now) {
		t.Fatal("the Packs behind the head are already due")
	}
	t.Cleanup(func() {
		items := []*sendItem{backlog.head, backlog.one, backlog.two}
		for _, item := range items {
			sequence.resendQueue.RemoveByMessageId(item.messageId)
		}
		sequence.sendItems = nil
		for _, item := range items {
			item.messagePoolReturn()
		}
	})
	return backlog
}

// A receiver that asks for a compact head's contract and says it has delivered
// no frame of the sequence lost the sequence's state: a client restarted in
// place that the sender's resent head reached before the answer to its first
// open did, and that would hold that answer behind the rest of the old
// process's backlog. The request rebuilds the head, as before, and makes every
// other unacknowledged Pack due at once, each once.
func TestSenderWritesEverythingAgainForAReceiverThatLostItsContract(t *testing.T) {
	backlog := newCompactHeadBacklog(t)
	backlog.sequence.receiveContractMissingAck(backlog.head.messageId, sequenceAck{
		contractMissing:   true,
		missingContractId: backlog.contractId,
		nothingDelivered:  true,
	})
	now := time.Now()
	if !backlog.head.hasContractFrame || backlog.head.recoveryKind != sendRecoveryContractMissing {
		t.Fatal("the head was not rebuilt with its contract")
	}
	for _, item := range []*sendItem{backlog.one, backlog.two} {
		if item.recoveryKind != sendRecoveryReceiverLost || item.resendTime.After(now) ||
			item.sendCount != 1 || backlog.sequence.resendQueue.GetByMessageId(item.messageId) != item {
			t.Fatalf("Pack %d is not due at once for the lost receiver: kind %d", item.sequenceNumber, item.recoveryKind)
		}
	}
	if backlog.sequence.receiverLostRewrittenBelow != 3 {
		t.Fatalf("rewritten below %d, want every Pack sent", backlog.sequence.receiverLostRewrittenBelow)
	}
}

// A receiver that asks for a compact head's contract after it has delivered
// still holds what it took, and lacks only the contract, as one that retired it
// behind MaxOpenReceiveContract newer ones does. Only the head is rebuilt; the
// Packs behind it keep their timers.
func TestSenderRebuildsOnlyTheHeadForAReceiverThatHasDelivered(t *testing.T) {
	backlog := newCompactHeadBacklog(t)
	oneDue, twoDue := backlog.one.resendTime, backlog.two.resendTime
	backlog.sequence.receiveContractMissingAck(backlog.head.messageId, sequenceAck{
		contractMissing:   true,
		missingContractId: backlog.contractId,
	})
	if !backlog.head.hasContractFrame || backlog.head.recoveryKind != sendRecoveryContractMissing {
		t.Fatal("the head was not rebuilt with its contract")
	}
	if backlog.one.resendTime != oneDue || backlog.two.resendTime != twoDue ||
		backlog.one.recoveryKind != sendRecoveryNone || backlog.two.recoveryKind != sendRecoveryNone ||
		backlog.sequence.receiverLostRewrittenBelow != 0 {
		t.Fatal("a receiver that has delivered had the Packs behind the head written again")
	}
}

// A peer that lost its session with this side, as a client restarted in place
// did, lost the state of this sequence with it, though its acknowledgements no
// longer say so once it has delivered a frame, such as an old Pack written
// again on its own timer. Every Pack the sequence holds unacknowledged that it
// last wrote in plaintext, the listener's handshake records and identity proof
// on its control carrier for one, is made due at once, as receiver-state
// recovery, and each at most once: a later nack writes nothing again. A Pack
// sealed under the epoch the peer lost waits for the reseal it is owed, and a
// selectively acknowledged one, which the peer holds, keeps its time. Before,
// each waited for its timer, backed off through the restart, and the restarted
// client's new handshake waited behind the old proof that way.
func TestSenderWritesWhatWentOutInPlaintextAtOnceForAPeerThatLostItsSession(t *testing.T) {
	for _, held := range []bool{false, true} {
		backlog := newCompactHeadBacklog(t)
		oneDue, twoDue := backlog.one.resendTime, backlog.two.resendTime
		// sealed under the epoch the peer lost
		backlog.two.sealGeneration = 3
		backlog.one.selectiveAcked = held
		at := time.Now()
		backlog.sequence.rewritePlaintextForLostPeer(at)
		if backlog.head.recoveryKind != sendRecoveryReceiverLost || backlog.head.resendTime.After(at) ||
			backlog.sequence.resendQueue.GetByMessageId(backlog.head.messageId) != backlog.head {
			t.Fatalf("held %t: the head, written in plaintext, is not due at once: kind %d", held, backlog.head.recoveryKind)
		}
		if held {
			if backlog.one.recoveryKind != sendRecoveryNone || backlog.one.resendTime != oneDue {
				t.Fatal("a Pack the peer holds was made due")
			}
		} else if backlog.one.recoveryKind != sendRecoveryReceiverLost || backlog.one.resendTime.After(at) {
			t.Fatalf("the Pack written in plaintext is not due at once: kind %d", backlog.one.recoveryKind)
		}
		if backlog.two.recoveryKind != sendRecoveryNone || backlog.two.resendTime != twoDue {
			t.Fatalf("held %t: a Pack sealed under the lost epoch was made due ahead of its reseal", held)
		}
		if backlog.sequence.lostPeerRewrittenBelow != 3 {
			t.Fatalf("held %t: rewritten below %d, want every Pack sent", held, backlog.sequence.lostPeerRewrittenBelow)
		}

		// as the writes leave them: on their timers, with no recovery pending
		later := at.Add(time.Minute)
		for _, item := range []*sendItem{backlog.head, backlog.one} {
			backlog.sequence.setResendTime(item, later)
			item.recoveryKind = sendRecoveryNone
		}
		backlog.sequence.rewritePlaintextForLostPeer(time.Now())
		for _, item := range []*sendItem{backlog.head, backlog.one} {
			if item.recoveryKind != sendRecoveryNone || item.resendTime != later {
				t.Fatalf("held %t: a later nack wrote Pack %d again", held, item.sequenceNumber)
			}
		}
	}
}

// The ack window keeps a receiver's saying that it delivered nothing when a
// later copy of the same acknowledgement, written after the sequence delivered,
// coalesces with it, and when an unwritten response returns: the Packs below
// were missing there when it said so.
func TestAckWindowKeepsANothingDeliveredSaying(t *testing.T) {
	window := newSequenceAckWindow()
	held, resent := NewId(), NewId()
	window.Update(sequenceAck{sequenceNumber: 3, messageId: held, selective: true, nothingDelivered: true})
	window.Update(sequenceAck{sequenceNumber: 3, messageId: held, selective: true})
	window.UpdateContractMissing(sequenceAck{
		sequenceNumber: 1, messageId: resent, contractMissing: true, missingContractId: NewId(), nothingDelivered: true,
	})
	window.UpdateContractMissing(sequenceAck{
		sequenceNumber: 1, messageId: resent, contractMissing: true, missingContractId: NewId(),
	})
	snapshot := window.Snapshot(true)
	if !snapshot.selectiveAcks[held].nothingDelivered || !snapshot.contractMissingAcks[resent].nothingDelivered {
		t.Fatal("a later copy took back the saying")
	}

	window.Update(sequenceAck{sequenceNumber: 3, messageId: held, selective: true})
	window.Restore([]sequenceAck{{sequenceNumber: 3, messageId: held, selective: true, nothingDelivered: true}})
	if snapshot := window.Snapshot(true); !snapshot.selectiveAcks[held].nothingDelivered {
		t.Fatal("a returned response lost the saying to the newer copy")
	}
}

// Both of the receive pump's decoders read Ack.nothing_delivered, the owned one on
// the ack hot path and the allocating one, and a selective ack or a request for
// a missing contract carries it to the sender's ack message, while a cumulative
// ack never does.
func TestAckNothingDeliveredDecodesOnEveryPath(t *testing.T) {
	path := TransferPath{DestinationId: NewId(), SourceId: NewId()}
	missingContractId := NewId()
	for _, ack := range []struct {
		selective       bool
		contractMissing bool
	}{{true, false}, {false, false}, {false, true}} {
		frame := sendAckFrame{
			path: path, messageId: NewId(), sequenceId: NewId(),
			selective: ack.selective, nothingDelivered: true,
		}
		if ack.contractMissing {
			frame.missingContractId = &missingContractId
		}
		wire := marshalSendAckTransferFrame(&frame)
		var allocated protocol.TransferFrame
		if !unmarshalTransferFrame(wire, &allocated, true) || allocated.Ack == nil || !allocated.Ack.NothingDelivered {
			MessagePoolReturn(wire)
			t.Fatalf("%+v: the allocating decoder lost nothing_delivered", ack)
		}
		decoded := inboundDecodedTransferFrames.take()
		if !unmarshalOwnedTransferFrame(wire, decoded, true) || decoded.frame.Ack == nil || !decoded.frame.Ack.NothingDelivered {
			inboundDecodedTransferFrames.put(decoded)
			MessagePoolReturn(wire)
			t.Fatalf("%+v: the owned decoder lost nothing_delivered", ack)
		}
		message, err := receiveAckMessageFromProtocol(decoded.frame.Ack)
		inboundDecodedTransferFrames.put(decoded)
		MessagePoolReturn(wire)
		if want := ack.selective || ack.contractMissing; err != nil || message.nothingDelivered != want {
			t.Fatalf("%+v: the ack message's nothing_delivered is %t, want %t (%v)", ack, message.nothingDelivered, want, err)
		}
	}
}

// The listener holds a write the old process never acknowledged when the edge
// restarts in place. The restarted edge's open reaches the listener, and the
// listener's answer, written on the same send sequence behind that write,
// reaches the restarted edge within ReachTimeout: the restarted edge holds the
// answer, says in its selective ack that it has delivered no frame of the
// sequence, and the listener writes the old write again at once, as the head.
// The listener's timers are set past the bound, so a broken run waits for them.
// Before, the restarted edge held the answer until the listener's backoff
// brought the old write again, and the measured runs waited up to 18.6 s.
//
// The old write may come sooner, by the other recovery of a restarted peer.
// When it is still on its way as the edge comes back, it reaches the restarted
// edge sealed under the old session, which the edge cannot open and nacks, and
// the listener reseals it once the edge seals under the new session, before the
// answer is written (`SendSequence.resealUnreadable`): about half the runs.
// Either way the old write went again at once, as receiver-state recovery, and
// a run in which neither recovery wrote it waited for a timer.
func TestRestartedClientGetsItsListenersAnswerWithoutTheListenersBackoff(t *testing.T) {
	settings := defaultRestartFirstStreamTestSettings()
	ctx, cancel := context.WithTimeout(context.Background(), 3*settings.StepTimeout)
	defer cancel()
	pair := newRestartFirstStreamPair(t, ctx, settings, func(s *ClientSettings) {
		// past ReachTimeout, so only the restarted edge's word brings the
		// held write again within it
		s.SendBufferSettings.MinResendInterval = settings.ColdResendInterval
		s.SendBufferSettings.RttMinResendInterval = settings.ColdResendInterval
		s.SendBufferSettings.MaxResendInterval = settings.ColdResendInterval
	}, nil)
	defer pair.close()

	kept, key := pair.runToRestart()
	// the old process never acknowledges it
	pair.listenerSends("held", key)
	startTime := time.Now()
	pair.startEdge(kept)
	pair.edgeSends("open after the restart")
	open, ok := restartFirstStreamWait(pair.listenerGot, "open after the restart", startTime.Add(settings.ReachTimeout))
	if !ok {
		t.Fatalf("the restarted edge's open did not reach the listener within %s", settings.ReachTimeout)
	}
	openTime := time.Since(startTime)
	pair.listenerSends("answer after the restart", open.key)
	if _, ok := restartFirstStreamWait(pair.edgeGot, "answer after the restart", startTime.Add(settings.ReachTimeout)); !ok {
		t.Fatalf("the listener's answer did not reach the restarted edge within %s (listener nothing-delivered writes %d, reseal writes %d, edge acks %d)",
			settings.ReachTimeout, pair.listener.SendRecoveryStats().ReceiverLostWriteCount,
			pair.listener.SendRecoveryStats().ResealWriteCount,
			pair.currentEdge().SendRecoveryStats().NothingDeliveredAckCount)
	}
	stats := pair.listener.SendRecoveryStats()
	if stats.ReceiverLostWriteCount == 0 && stats.ResealWriteCount == 0 {
		t.Fatal("the answer arrived without a nothing-delivered write or a reseal: the scenario did not happen")
	}
	t.Logf("the restarted edge's open reached the listener %s after the restart, and the answer reached the edge %s after it",
		openTime, time.Since(startTime))
}
