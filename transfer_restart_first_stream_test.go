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

// Offers one Pack of sequenceId as the shared receive pump does, with a zero
// handoff timeout, fails unless the buffer took it, and answers its message id.
func (self *replacementFixture) offer(sequenceId Id, number uint64, payload string) Id {
	self.t.Helper()
	messageId := NewId()
	pack := &protocol.Pack{
		MessageId:      messageId.Bytes(),
		SequenceId:     sequenceId.Bytes(),
		SequenceNumber: number,
		Head:           number == 0,
		Frames: []*protocol.Frame{{
			MessageType:  protocol.MessageType_IpIpPacketToProvider,
			MessageBytes: MessagePoolCopy([]byte(payload)),
		}},
	}
	wire, err := ProtoMarshal(&protocol.TransferFrame{
		TransferPath: sendTransferPath(self.source.SourceId, DestinationId(self.client.ClientId())).ToProtobuf(),
		Pack:         pack,
	})
	if err != nil {
		MessagePoolReturn(pack.Frames[0].MessageBytes)
		self.t.Fatalf("marshal a receive Pack: %v", err)
	}
	receivePack := &ReceivePack{
		Source:             self.source,
		SequenceId:         sequenceId,
		Pack:               pack,
		ReceiveCallback:    self.client.receiveCallback,
		MessageByteCount:   ByteCount(len(payload)),
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
		self.t.Fatalf("the shared pump's handoff of %q: accepted=%t err=%v", payload, accepted, err)
	}
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
	_, listenerReceive := newConditioner(ctx, pair.edgeSend)
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
