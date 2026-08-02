package connect

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/pion/datachannel"
	"github.com/pion/logging"
	"github.com/pion/webrtc/v4"

	"github.com/urnetwork/connect/protocol"
)

type WebRtcConn interface {
	net.Conn
	Connected() bool
	AddConnectedCallback(func(connected bool)) func()
	// closed when the conn cancels for a reason where the outer transport
	// should reconnect without backoff (e.g., remote requested fresh
	// negotiation or host network change). The channel is persistent and
	// one-shot; implementations without this signal return a never-closed
	// channel.
	ImmediateReconnect() <-chan struct{}
}

type SignalSender interface {
	SendSignal(path TransferPath, signal *protocol.Frame, opts ...any)
}

type SignalReceiver interface {
	ReceiveSignal(source TransferPath, signal *protocol.Frame) error
}

// exchangeSignalReceiver is the owned-value fast path for signal dispatch.
// The Client receive callback's frame bytes are callback-scoped, so the
// asynchronous signal queue decodes them once into an owning protobuf value
// before returning. Implementations of only SignalReceiver retain the framed
// compatibility path.
type exchangeSignalReceiver interface {
	ReceiveExchangeSignals(source TransferPath, signals *protocol.ExchangeSignals) error
}

type clientSignalDispatcher struct {
	client    *Client
	receiver  SignalReceiver
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	shards    []*clientSignalReceiver
}

// signalPeerPrioritizer is implemented by WebRtcManager. The transfer receive
// callback has already authenticated the relationship represented by Peer, so
// a Network-mode signal is the earliest trustworthy indication that this peer
// should reclaim bounded P2P admission from speculative public connections.
type signalPeerPrioritizer interface {
	PrioritizePeer(peerId Id)
}

func prioritizeNetworkSignalPeer(
	receiver SignalReceiver,
	source TransferPath,
	frames []*protocol.Frame,
	peer Peer,
) {
	if peer.ProvideMode != protocol.ProvideMode_Network || source.SourceId == (Id{}) {
		return
	}
	for _, frame := range frames {
		if frame != nil && frame.MessageType == protocol.MessageType_TransferExchangeSignals {
			if prioritizer, ok := receiver.(signalPeerPrioritizer); ok {
				prioritizer.PrioritizePeer(source.SourceId)
			}
			return
		}
	}
}

// conforms to `SignalSender`
type ClientSignalSender struct {
	client *Client
}

func NewClientSignalSender(client *Client) *ClientSignalSender {
	return &ClientSignalSender{
		client: client,
	}
}

func (self *ClientSignalSender) SendSignal(path TransferPath, signal *protocol.Frame, opts ...any) {
	success := self.client.SendWithTimeout(signal, path.DestinationMask(), nil, -1, opts...)
	// a dropped signal wedges the p2p setup until the transport retry —
	// always loud. The V(1) positive is the send-side half of the signal
	// delivery trace (receive side: [signal]receive).
	if !success {
		self.client.log.Infof("[signal]send failed ->%s\n", path.DestinationMask())
	} else if self.client.log.V(1).Enabled() {
		self.client.log.Infof("[signal]send ->%s\n", path.DestinationMask())
	}
}

type clientSignalReceiver struct {
	client            *Client
	receiver          SignalReceiver
	ctx               context.Context
	cancel            context.CancelFunc
	queueLock         sync.Mutex
	closed            bool
	queueLimit        int
	receiveFrames     []*receivedSignalFrame
	receiveFrameHead  int
	receiveFrameCount int
	queueMonitor      *Monitor
	spaceMonitor      *Monitor
	runOnce           sync.Once
}

type receivedSignalFrame struct {
	source          TransferPath
	frame           *protocol.Frame
	exchangeSignals *protocol.ExchangeSignals
	candidateBatch  bool
	candidateCount  int
	candidateBytes  int
	keyValid        bool
	key             receivedSignalFrameKey
}

type receivedSignalFrameKey struct {
	source   TransferPath
	streamId Id
}

func ReceiveSignalsFromClient(client *Client, receiver SignalReceiver) func() {
	cancelCtx, cancel := context.WithCancel(client.Ctx())
	bufferSize := defaultTransferBufferSize
	if client.settings != nil && client.settings.ReceiveBufferSettings != nil {
		bufferSize = client.settings.ReceiveBufferSettings.SequenceBufferSize
	}
	bufferSize = max(1, bufferSize)
	workerCount := 4
	if client.settings != nil && client.settings.WebRtcSettings != nil &&
		0 < client.settings.WebRtcSettings.SignalReceiveWorkerCount {
		workerCount = client.settings.WebRtcSettings.SignalReceiveWorkerCount
	}
	workerCount = min(bufferSize, max(1, workerCount))
	dispatcher := &clientSignalDispatcher{
		client:   client,
		receiver: receiver,
		ctx:      cancelCtx,
		cancel:   cancel,
		shards:   make([]*clientSignalReceiver, 0, workerCount),
	}
	for workerIndex := range workerCount {
		queueLimit := bufferSize / workerCount
		if workerIndex < bufferSize%workerCount {
			queueLimit++
		}
		shard := &clientSignalReceiver{
			client:       client,
			receiver:     receiver,
			ctx:          cancelCtx,
			cancel:       cancel,
			queueLimit:   queueLimit,
			queueMonitor: NewMonitor(),
			spaceMonitor: NewMonitor(),
		}
		dispatcher.shards = append(dispatcher.shards, shard)
	}
	context.AfterFunc(cancelCtx, dispatcher.Close)
	unsub := client.AddReceiveCallback(dispatcher.Receive)
	return func() {
		unsub()
		dispatcher.Close()
	}
}

// ReceiveFunction. Frames for one peer/stream hash to one worker and stay
// ordered; independent peers can negotiate in parallel. The sum of shard
// capacities remains the original receive queue limit, and enqueue still
// blocks when the selected bounded shard is full, preserving the Client
// receive callback's intentional backpressure.
func (self *clientSignalDispatcher) Receive(source TransferPath, frames []*protocol.Frame, peer Peer) {
	prioritizeNetworkSignalPeer(self.receiver, source, frames, peer)
	for _, frame := range frames {
		self.handleControlFrame(source, frame)
	}
}

func (self *clientSignalDispatcher) handleControlFrame(source TransferPath, frame *protocol.Frame) {
	switch frame.MessageType {
	case protocol.MessageType_TransferExchangeSignals:
		select {
		case <-self.ctx.Done():
			return
		default:
		}

		if self.client.log.V(1).Enabled() {
			self.client.log.Infof("[signal]receive from %s\n", source)
		}
		received, err := newReceivedSignalFrame(source, frame)
		if err != nil {
			self.client.log.Infof("[signal]receive frame err=%s\n", err)
			return
		}
		shardIndex := receivedSignalShardIndex(received, len(self.shards))
		shard := self.shards[shardIndex]
		shard.start()
		if !shard.enqueue(received) {
			received.Close()
		}
	}
}

func (self *clientSignalDispatcher) Close() {
	self.closeOnce.Do(func() {
		self.cancel()
		for _, shard := range self.shards {
			shard.Close()
		}
	})
}

// ReceiveFunction
func (self *clientSignalReceiver) Receive(source TransferPath, frames []*protocol.Frame, peer Peer) {
	prioritizeNetworkSignalPeer(self.receiver, source, frames, peer)
	for _, frame := range frames {
		self.handleControlFrame(source, frame)
	}
}

func (self *clientSignalReceiver) handleControlFrame(source TransferPath, frame *protocol.Frame) {
	switch frame.MessageType {
	case protocol.MessageType_TransferExchangeSignals:
		select {
		case <-self.ctx.Done():
			return
		default:
		}

		// receive-side half of the signal delivery trace (send side:
		// [signal]send)
		if self.client.log.V(1).Enabled() {
			self.client.log.Infof("[signal]receive from %s\n", source)
		}

		received, err := newReceivedSignalFrame(source, frame)
		if err != nil {
			self.client.log.Infof("[signal]receive frame err=%s\n", err)
			return
		}
		self.start()
		if !self.enqueue(received) {
			received.Close()
		}
	}
}

func (self *clientSignalReceiver) start() {
	self.runOnce.Do(func() {
		go HandleError(self.run, self.cancel)
	})
}

func (self *clientSignalReceiver) Close() {
	self.queueLock.Lock()
	if !self.closed {
		self.closed = true
		for i := range self.receiveFrameCount {
			index := (self.receiveFrameHead + i) % len(self.receiveFrames)
			received := self.receiveFrames[index]
			if received != nil {
				received.Close()
			}
		}
		self.receiveFrames = nil
		self.receiveFrameHead = 0
		self.receiveFrameCount = 0
	}
	self.queueLock.Unlock()
	self.cancel()
	self.queueMonitor.NotifyAll()
	self.spaceMonitor.NotifyAll()
}

func (self *clientSignalReceiver) run() {
	for {
		received := self.dequeue()
		if received == nil {
			return
		}
		func() {
			defer received.Close()
			if receiver, ok := self.receiver.(exchangeSignalReceiver); ok && received.exchangeSignals != nil {
				if err := receiver.ReceiveExchangeSignals(received.source, received.exchangeSignals); err != nil {
					self.client.log.Infof("[signal]receive err=%s\n", err)
				}
				return
			}
			if err := received.prepareFrame(); err != nil {
				self.client.log.Infof("[signal]receive frame err=%s\n", err)
				return
			}
			if err := self.receiver.ReceiveSignal(received.source, received.frame); err != nil {
				self.client.log.Infof("[signal]receive err=%s\n", err)
			}
		}()
	}
}

func newReceivedSignalFrame(source TransferPath, frame *protocol.Frame) (*receivedSignalFrame, error) {
	exchangeSignals := &protocol.ExchangeSignals{}
	if err := ProtoUnmarshal(frame.MessageBytes, exchangeSignals); err != nil {
		return &receivedSignalFrame{
			source: source,
			frame: &protocol.Frame{
				MessageType:  frame.MessageType,
				Raw:          frame.Raw,
				MessageBytes: MessagePoolCopy(frame.MessageBytes),
			},
		}, nil
	}

	candidateBatch := false
	var key receivedSignalFrameKey
	keyValid := false
	if streamId, err := IdFromBytes(exchangeSignals.StreamId); err == nil {
		keyValid = true
		key = receivedSignalFrameKey{
			source:   source,
			streamId: streamId,
		}
		if isCandidateBatch(exchangeSignals) {
			candidateBatch = true
		}
	}
	received := &receivedSignalFrame{
		source: source,
		frame: &protocol.Frame{
			MessageType: frame.MessageType,
			Raw:         frame.Raw,
		},
		exchangeSignals: exchangeSignals,
		keyValid:        keyValid,
		key:             key,
	}
	if candidateBatch {
		received.candidateBatch = true
		for _, signal := range exchangeSignals.Signals {
			received.candidateCount++
			received.candidateBytes += len(signal.IceCandidate)
		}
	}
	return received, nil
}

func receivedSignalShardIndex(received *receivedSignalFrame, shardCount int) int {
	if shardCount <= 1 || received == nil || !received.keyValid {
		return 0
	}
	// Allocation-free FNV-1a over the stable ordering key. SourceId +
	// streamId is sufficient: WebRtcManager uses exactly this pair to find a
	// peer connection.
	hash := uint32(2166136261)
	for _, b := range received.key.source.SourceId {
		hash = (hash ^ uint32(b)) * 16777619
	}
	for _, b := range received.key.streamId {
		hash = (hash ^ uint32(b)) * 16777619
	}
	return int(hash % uint32(shardCount))
}

func (self *receivedSignalFrame) Close() {
	if self.frame != nil {
		if self.frame.MessageBytes != nil {
			MessagePoolReturn(self.frame.MessageBytes)
		}
		self.frame = nil
	}
	self.exchangeSignals = nil
}

func (self *receivedSignalFrame) prepareFrame() error {
	if self.frame == nil || self.frame.MessageBytes != nil || self.exchangeSignals == nil {
		return nil
	}
	messageBytes, err := ProtoMarshal(self.exchangeSignals)
	if err != nil {
		return err
	}
	self.frame.MessageBytes = messageBytes
	return nil
}

func (self *receivedSignalFrame) isCandidateBatch() bool {
	return isCandidateBatch(self.exchangeSignals)
}

func isCandidateBatch(exchangeSignals *protocol.ExchangeSignals) bool {
	if exchangeSignals == nil || exchangeSignals.ResetSignals || len(exchangeSignals.Signals) == 0 {
		return false
	}
	for _, signal := range exchangeSignals.Signals {
		if signal == nil || signal.SignalType != protocol.SignalType_IceCandidate {
			return false
		}
	}
	return true
}

func (self *receivedSignalFrame) appendCandidateBatch(received *receivedSignalFrame) bool {
	if !self.candidateBatch || !received.candidateBatch || self.key != received.key {
		return false
	}
	// Coalescing is an allocation optimization, not permission to bypass the
	// bounded queue. Once a batch reaches the same count/byte ceiling as the
	// pre-SDP candidate buffer, leave the next frame as a queue entry; a full
	// shard then blocks the Client receive callback as intentional
	// backpressure.
	if maxBufferedRemoteIceCandidateCount < self.candidateCount+received.candidateCount ||
		maxBufferedRemoteIceCandidateBytes < self.candidateBytes+received.candidateBytes {
		return false
	}
	self.exchangeSignals.Signals = append(self.exchangeSignals.Signals, received.exchangeSignals.Signals...)
	self.candidateCount += received.candidateCount
	self.candidateBytes += received.candidateBytes
	return true
}

func (self *clientSignalReceiver) enqueue(received *receivedSignalFrame) bool {
	for {
		var spaceNotify chan struct{}
		self.queueLock.Lock()
		if self.closed || self.ctx.Err() != nil {
			self.queueLock.Unlock()
			return false
		}
		if received.candidateBatch {
			if batch := self.tailLocked(); batch != nil && batch.candidateBatch && batch.key == received.key {
				if batch.appendCandidateBatch(received) {
					self.queueLock.Unlock()
					received.Close()
					return true
				}
			}
		}
		if self.queueLenLocked() < self.queueLimit {
			if self.receiveFrames == nil {
				// A fixed-capacity ring makes the queue's backing storage obey
				// the same bound as its live item count. The former
				// append-plus-head scheme retained an ever-growing nil prefix
				// whenever sustained traffic kept the queue from becoming
				// completely empty.
				self.receiveFrames = make([]*receivedSignalFrame, self.queueLimit)
			}
			tail := (self.receiveFrameHead + self.receiveFrameCount) % len(self.receiveFrames)
			self.receiveFrames[tail] = received
			self.receiveFrameCount++
			self.queueLock.Unlock()
			self.queueMonitor.NotifyAll()
			return true
		}
		spaceNotify = self.spaceMonitor.NotifyChannel()
		self.queueLock.Unlock()

		select {
		case <-self.ctx.Done():
			return false
		case <-spaceNotify:
		}
	}
}

func (self *clientSignalReceiver) queueLenLocked() int {
	return self.receiveFrameCount
}

func (self *clientSignalReceiver) tailLocked() *receivedSignalFrame {
	if self.receiveFrameCount == 0 {
		return nil
	}
	index := (self.receiveFrameHead + self.receiveFrameCount - 1) % len(self.receiveFrames)
	return self.receiveFrames[index]
}

func (self *clientSignalReceiver) dequeue() *receivedSignalFrame {
	for {
		var queueNotify chan struct{}
		self.queueLock.Lock()
		if 0 < self.receiveFrameCount {
			received := self.receiveFrames[self.receiveFrameHead]
			self.receiveFrames[self.receiveFrameHead] = nil
			self.receiveFrameCount--
			if self.receiveFrameCount == 0 {
				self.receiveFrameHead = 0
			} else {
				self.receiveFrameHead = (self.receiveFrameHead + 1) % len(self.receiveFrames)
			}
			self.queueLock.Unlock()
			self.spaceMonitor.NotifyAll()
			return received
		}
		if self.closed || self.ctx.Err() != nil {
			self.queueLock.Unlock()
			return nil
		}
		queueNotify = self.queueMonitor.NotifyChannel()
		self.queueLock.Unlock()

		select {
		case <-self.ctx.Done():
			return nil
		case <-queueNotify:
		}
	}
}

func DefaultWebRtcSettings() *WebRtcSettings {
	return &WebRtcSettings{
		// FIXME
		// SendBufferSize: mib(1),

		// sctp receive buffer per peer connection, so scaled by the memory
		// budget. sized so a handful of peer connections plus the transfer
		// queues coexist within the client memory target — each new
		// connection reserves this amount from the shared client budget
		// (see MemoryBudget)
		ReceiveBufferSize: MemoryScaledByteCount(kib(512), kib(256)),
		// Pion's receive MTU is a per-packet demux buffer, not the SCTP
		// receive window. Its SCTP path MTU stays below Ethernet's 1500-byte
		// packet size, so the former 4 KiB value only enlarged persistent and
		// scratch buffers.
		ReceiveMtu: 1500,
		// Match P2pTransportSettings.MaxMessageByteCount. Pion otherwise
		// advertises a ~1 GiB SCTP message and permits the association to
		// reassemble far beyond the transport's bounded read buffer.
		MaxMessageSize: 64 * 1024,
		// each peer connection holds the pion ice/dtls/sctp stack plus up to
		// `ReceiveBufferSize` of queued data, so the count is bounded (scaled
		// by the memory budget). At the cap new p2p setups are refused and
		// those streams stay on the platform transport; the p2p transport
		// retries with backoff, so capacity recovers as connections close.
		MaxPeerConnectionCount: MemoryScaledCount(32, 8),
		DisconnectedTimeout:    30 * time.Second,
		FailedTimeout:          30 * time.Second,
		// Pion's two-second default halves idle STUN/radio wakeups versus the
		// former one-second override without changing the 30-second
		// disconnect/failure thresholds.
		KeepAliveTimeout: 2 * time.Second,
		// Host candidates are available immediately. Bound the lifetime of
		// dead STUN socket/gather goroutines instead of accepting Pion's
		// five-second default for every URL and address family.
		StunGatherTimeout: 2 * time.Second,

		DataChannelLabel: "data",
		// TransferFrames carry their own sequence numbers and already reorder
		// across concurrent routes. Keeping SCTP reliable but unordered lets a
		// later frame reach that recovery layer when an earlier SCTP fragment
		// is lost, instead of adding a second head-of-line barrier. A
		// deterministic one-datagram-loss measurement reduced the next-frame
		// tail from about 110 ms to below 1.4 ms.
		DataChannelOrdered:       false,
		SignalReceiveWorkerCount: 4,
		// SNAP exchanges SCTP INIT state in the already-required SDP,
		// removing the cookie handshake when both native peers support it.
		// The attribute is negotiated; mixed/older peers fall back to the
		// standard association handshake.
		EnableSctpSnap: true,
		// SCTP runs inside authenticated DTLS for WebRTC. RFC 9653 negotiates
		// whether that stronger integrity check may replace SCTP's redundant
		// CRC32c; a peer that does not advertise support continues to receive
		// and send normal checksums. This removes one checksum on each DATA/
		// SACK path when both directions support it.
		EnableSctpZeroChecksum: true,
		// Pion's Reno-style congestion avoidance otherwise adds one ~1.2 KiB
		// MTU only after a complete cwnd is acknowledged. On a measured 50 ms
		// path with independent wireless loss, recovery from the observed
		// 20-50 KiB window took seconds and held throughput below 1 MiB/s.
		// Four MTUs retains loss response and a zero minimum (unlike a forced
		// floor), while making recovery competitive with modern transports.
		SctpCwndCAStep: 4 * 1200,
		// ICE consent can remain healthy while the SCTP/data plane is
		// half-open. Once a write leaves unacknowledged SCTP bytes, require a
		// reverse SCTP packet within this bound or rebuild the association.
		// The worker is lazy and has no idle timer/radio wakeups.
		SctpNoProgressTimeout: 10 * time.Second,
		// openrelay.metered.ca and stun.stunprotocol.org are defunct — every
		// gather against them burned a multi-second i/o timeout per attempt
		// (observed on-device 2026-07-25) and delayed candidate gathering.
		// Keep a small set of live anycast servers.
		IceServerUrls: []string{
			"stun:stun.cloudflare.com:3478",
			"stun:stun.l.google.com:19302",
		},
	}
}

type WebRtcSettings struct {
	// Log, when set, is used by the webrtc manager, p2p transports, and the
	// pion stack. nil resolves to `DefaultLogger()`.
	// `NewClientWithTag` propagates the client log here when nil.
	Log Logger

	ReceiveBufferSize ByteCount
	ReceiveMtu        ByteCount
	MaxMessageSize    ByteCount
	// the maximum number of live peer connections across all peers and
	// streams. 0 is no limit.
	MaxPeerConnectionCount int
	// MemoryBudget, when set, is the shared client transfer budget: each new
	// peer connection reserves `ReceiveBufferSize` from it at creation and
	// releases it at teardown, so p2p setups are admitted only while the
	// client memory target has headroom (refused setups stay on the platform
	// transport, as at the count cap). nil disables byte admission.
	MemoryBudget        *TransferMemoryBudget
	DisconnectedTimeout time.Duration
	FailedTimeout       time.Duration
	KeepAliveTimeout    time.Duration
	StunGatherTimeout   time.Duration

	DataChannelLabel   string
	DataChannelOrdered bool
	// SignalReceiveWorkerCount hashes each peer/stream onto one ordered
	// worker. Separate peers negotiate concurrently, while each stream's SDP
	// and ICE ordering and the bounded receive callback backpressure remain
	// intact.
	SignalReceiveWorkerCount int
	EnableSctpSnap           bool
	EnableSctpZeroChecksum   bool
	// SCTP congestion controls are byte counts. Zero retains Pion's RFC-style
	// default. CwndCAStep changes only additive recovery; MinCwnd is a hard
	// floor and FastRtxWnd is the loss-retransmit burst cap.
	SctpMinCwnd    uint32
	SctpFastRtxWnd uint32
	SctpCwndCAStep uint32
	// SctpNoProgressTimeout bounds a half-open data plane after outbound
	// activity. Zero disables the watchdog. It observes reverse SCTP packets,
	// not application callbacks, so transfer send/receive/forward callbacks
	// retain their intentional synchronous backpressure semantics.
	SctpNoProgressTimeout time.Duration
	// UseEgressOnlyIceInterfaces gathers host/server-reflexive candidates
	// only from the current default-route IPv4/IPv6 addresses. Device VPN
	// clients enable this to exclude their own tunnel, stale utun, bridge,
	// AWDL, and VM interfaces; generic server callers leave it false for
	// deliberate multihoming.
	UseEgressOnlyIceInterfaces bool

	// add stun:xxx urls here
	IceServerUrls []string
}

func webRtcDataChannelInit(settings *WebRtcSettings) *webrtc.DataChannelInit {
	ordered := settings.DataChannelOrdered
	return &webrtc.DataChannelInit{
		Ordered: &ordered,
		// Reliability remains SCTP's default. The upper transfer layer can
		// recover loss, but retaining lower-layer recovery avoids turning a
		// cold path's first loss into its conservative application resend
		// timeout. Unordered delivery alone removes the duplicate HOL barrier.
	}
}

// pionLoggerFactory routes pion logs through a `Logger`, so the webrtc stack
// follows the same logger as the peer connection that created it (and is
// silenced with it). Without this, pion writes to its own default factory
// (stdout), bypassing per-client logging entirely.
// pion levels map: Error->Errorf, Warn->Warningf, Info->V(1), Debug/Trace->V(2).
type pionLoggerFactory struct {
	log Logger
}

func (self *pionLoggerFactory) NewLogger(scope string) logging.LeveledLogger {
	return &pionLeveledLogger{
		log:   self.log,
		scope: scope,
	}
}

type pionLeveledLogger struct {
	log   Logger
	scope string
}

func (self *pionLeveledLogger) Trace(msg string) {
	if self.log.V(2).Enabled() {
		self.log.Infof("[pion:%s]%s", self.scope, msg)
	}
}

func (self *pionLeveledLogger) Tracef(format string, args ...any) {
	if v := self.log.V(2); v.Enabled() {
		v.Infof("[pion:"+self.scope+"]"+format, args...)
	}
}

func (self *pionLeveledLogger) Debug(msg string) {
	if self.log.V(2).Enabled() {
		self.log.Infof("[pion:%s]%s", self.scope, msg)
	}
}

func (self *pionLeveledLogger) Debugf(format string, args ...any) {
	if v := self.log.V(2); v.Enabled() {
		v.Infof("[pion:"+self.scope+"]"+format, args...)
	}
}

func (self *pionLeveledLogger) Info(msg string) {
	if self.log.V(1).Enabled() {
		self.log.Infof("[pion:%s]%s", self.scope, msg)
	}
}

func (self *pionLeveledLogger) Infof(format string, args ...any) {
	if v := self.log.V(1); v.Enabled() {
		v.Infof("[pion:"+self.scope+"]"+format, args...)
	}
}

func (self *pionLeveledLogger) Warn(msg string) {
	if v := self.log.V(1); v.Enabled() {
		self.log.Warningf("[pion:%s]%s", self.scope, msg)
	}
}

func (self *pionLeveledLogger) Warnf(format string, args ...any) {
	if v := self.log.V(1); v.Enabled() {
		self.log.Warningf("[pion:"+self.scope+"]"+format, args...)
	}
}

func (self *pionLeveledLogger) Error(msg string) {
	self.log.Errorf("[pion:%s]%s", self.scope, msg)
}

func (self *pionLeveledLogger) Errorf(format string, args ...any) {
	self.log.Errorf("[pion:"+self.scope+"]"+format, args...)
}

type WebRtcManager struct {
	ctx          context.Context
	log          Logger
	signalSender SignalSender
	settings     *WebRtcSettings

	stateLock                  sync.Mutex
	peerConns                  map[peerConnKey]*peerConn
	prioritizedPeers           map[Id]time.Time
	pendingPrioritizedPeerSlot map[Id]time.Time

	peerConnectionFactoryLock        sync.Mutex
	peerConnectionFactory            *webRtcPeerConnectionFactory
	peerConnectionFactoryInitErr     error
	peerConnectionFactoryInitialized bool
	peerConnectionFactoryRetryTime   time.Time
	peerConnectionFactoryClosed      bool
	newPeerConnectionFactory         func(*WebRtcSettings) (*webRtcPeerConnectionFactory, error)

	capacityMonitor *Monitor

	networkChangeLock   sync.Mutex
	networkChangeWorker *coalescingCallbackWorker
	networkChangeUnsub  func()
}

const peerConnectionFactoryRetryTimeout = time.Second
const peerConnectionPriorityTimeout = 30 * time.Second
const maxPeerConnectionPriorityCount = 64

func NewWebRtcManager(ctx context.Context, signalSender SignalSender, settings *WebRtcSettings) *WebRtcManager {
	manager := &WebRtcManager{
		ctx:                        ctx,
		log:                        loggerOrDefault(settings.Log),
		signalSender:               signalSender,
		settings:                   settings,
		peerConns:                  map[peerConnKey]*peerConn{},
		prioritizedPeers:           map[Id]time.Time{},
		pendingPrioritizedPeerSlot: map[Id]time.Time{},
		capacityMonitor:            NewMonitor(),
		newPeerConnectionFactory:   newWebRtcPeerConnectionFactory,
	}
	context.AfterFunc(ctx, func() {
		manager.closeNetworkChangeWorker()
		manager.closePeerConnectionFactory()
	})
	return manager
}

func (self *WebRtcManager) prunePeerPrioritiesLocked(now time.Time) {
	for peerId, until := range self.prioritizedPeers {
		if !now.Before(until) {
			delete(self.prioritizedPeers, peerId)
		}
	}
	for peerId, until := range self.pendingPrioritizedPeerSlot {
		if !now.Before(until) {
			delete(self.pendingPrioritizedPeerSlot, peerId)
		}
	}
}

// oldestEvictablePeerConnLocked returns bounded capacity that a trusted,
// explicitly selected network peer may reclaim. Priority expires unless the
// provider continues to observe Network traffic, so an abandoned selection
// cannot pin a slot forever.
func (self *WebRtcManager) oldestEvictablePeerConnLocked(now time.Time) *peerConn {
	var oldest *peerConn
	for _, conn := range self.peerConns {
		if conn.ctx.Err() != nil || now.Before(conn.priorityUntil) {
			continue
		}
		if oldest == nil || conn.createdAt.Before(oldest.createdAt) {
			oldest = conn
		}
	}
	return oldest
}

// PrioritizePeer gives a trusted same-network peer prompt access to the
// existing bounded P2P capacity. Providers can have every reservation occupied
// by speculative public negotiations; without preemption, an explicitly
// selected network peer competes in a wake-all retry lottery and may remain on
// the relay indefinitely. No count or byte ceiling is raised: at most one
// oldest non-priority connection is retired, and ordinary admissions pause
// until the selected peer consumes the released slot.
func (self *WebRtcManager) PrioritizePeer(peerId Id) {
	if peerId == (Id{}) {
		return
	}
	now := time.Now()
	until := now.Add(peerConnectionPriorityTimeout)

	var victim *peerConn
	self.stateLock.Lock()
	if self.prioritizedPeers == nil {
		self.prioritizedPeers = map[Id]time.Time{}
	}
	if self.pendingPrioritizedPeerSlot == nil {
		self.pendingPrioritizedPeerSlot = map[Id]time.Time{}
	}
	self.prunePeerPrioritiesLocked(now)
	if _, exists := self.prioritizedPeers[peerId]; !exists &&
		maxPeerConnectionPriorityCount <= len(self.prioritizedPeers) {
		var oldestPeerId Id
		var oldestUntil time.Time
		for candidatePeerId, candidateUntil := range self.prioritizedPeers {
			if oldestUntil.IsZero() || candidateUntil.Before(oldestUntil) {
				oldestPeerId = candidatePeerId
				oldestUntil = candidateUntil
			}
		}
		delete(self.prioritizedPeers, oldestPeerId)
		delete(self.pendingPrioritizedPeerSlot, oldestPeerId)
	}
	self.prioritizedPeers[peerId] = until

	hasLivePeerConn := false
	for key, conn := range self.peerConns {
		if key.PeerId == peerId && conn.ctx.Err() == nil {
			conn.priorityUntil = until
			hasLivePeerConn = true
		}
	}
	if hasLivePeerConn {
		delete(self.pendingPrioritizedPeerSlot, peerId)
	} else {
		_, alreadyPending := self.pendingPrioritizedPeerSlot[peerId]
		self.pendingPrioritizedPeerSlot[peerId] = until
		if !alreadyPending {
			countFull := 0 < self.settings.MaxPeerConnectionCount &&
				self.settings.MaxPeerConnectionCount <= len(self.peerConns)
			budgetFull := self.settings.MemoryBudget != nil &&
				self.settings.MemoryBudget.Available() < self.settings.ReceiveBufferSize
			if countFull || budgetFull {
				victim = self.oldestEvictablePeerConnLocked(now)
			}
		}
	}
	self.stateLock.Unlock()

	if victim != nil {
		if self.log.V(1).Enabled() {
			self.log.Infof("[p2p]priority peer %s evicts non-priority peer %s\n", peerId, victim.key.PeerId)
		}
		victim.Cancel()
	}
}

// startNetworkChangeWorker is lazy for the same reason as the Pion factory:
// every transfer Client owns a WebRtcManager, but many never create a peer
// connection. Those clients need neither an OS-path listener nor an idle
// goroutine. Once a manager does bind ICE state to a path, host callbacks may
// run on a UI/NetworkExtension-owned thread, so teardown is dispatched onto a
// single coalescing worker and repeated notifications remain bounded.
func (self *WebRtcManager) startNetworkChangeWorker() {
	self.networkChangeLock.Lock()
	defer self.networkChangeLock.Unlock()
	if self.networkChangeWorker != nil || self.ctx.Err() != nil {
		return
	}
	worker := newCoalescingCallbackWorker(self.ctx, self.networkChanged)
	self.networkChangeWorker = worker
	self.networkChangeUnsub = AddNetworkChangeListener(worker.Dispatch)
}

func (self *WebRtcManager) closeNetworkChangeWorker() {
	self.networkChangeLock.Lock()
	worker := self.networkChangeWorker
	unsub := self.networkChangeUnsub
	self.networkChangeWorker = nil
	self.networkChangeUnsub = nil
	self.networkChangeLock.Unlock()
	if unsub != nil {
		unsub()
	}
	if worker != nil {
		worker.Close()
	}
}

// webRtcPeerConnectionFactory owns immutable Pion state reused by all peer
// connections in one manager. It is initialized lazily: a multi-client that
// never receives a stream does not pay for an API, certificate, or native ICE
// resources.
type webRtcPeerConnectionFactory struct {
	newPeerConnection func() (*webrtc.PeerConnection, error)
	close             func() error
}

func (self *webRtcPeerConnectionFactory) NewPeerConnection() (*webrtc.PeerConnection, error) {
	return self.newPeerConnection()
}

func (self *webRtcPeerConnectionFactory) Close() error {
	if self == nil || self.close == nil {
		return nil
	}
	return self.close()
}

func (self *WebRtcManager) newPeerConnection() (*webrtc.PeerConnection, error) {
	self.startNetworkChangeWorker()

	self.peerConnectionFactoryLock.Lock()
	defer self.peerConnectionFactoryLock.Unlock()

	if self.peerConnectionFactoryClosed || self.ctx.Err() != nil {
		return nil, os.ErrClosed
	}
	if !self.peerConnectionFactoryInitialized ||
		(self.peerConnectionFactoryInitErr != nil &&
			!time.Now().Before(self.peerConnectionFactoryRetryTime)) {
		self.peerConnectionFactory, self.peerConnectionFactoryInitErr =
			self.newPeerConnectionFactory(self.settings)
		self.peerConnectionFactoryInitialized = true
		if self.peerConnectionFactoryInitErr != nil {
			// Certificate/random-source and native socket setup failures can
			// be transient. Cache briefly to collapse a many-stream retry
			// stampede, but never poison this manager for its entire lifetime.
			self.peerConnectionFactoryRetryTime =
				time.Now().Add(peerConnectionFactoryRetryTimeout)
		} else {
			self.peerConnectionFactoryRetryTime = time.Time{}
		}
	}
	if self.peerConnectionFactoryInitErr != nil {
		return nil, self.peerConnectionFactoryInitErr
	}
	return self.peerConnectionFactory.NewPeerConnection()
}

func (self *WebRtcManager) closePeerConnectionFactory() {
	self.peerConnectionFactoryLock.Lock()
	if self.peerConnectionFactoryClosed {
		self.peerConnectionFactoryLock.Unlock()
		return
	}
	self.peerConnectionFactoryClosed = true
	factory := self.peerConnectionFactory
	self.peerConnectionFactory = nil
	self.peerConnectionFactoryLock.Unlock()

	if err := factory.Close(); err != nil && self.log.V(1).Enabled() {
		self.log.Infof("[peerconn]factory close err = %s\n", err)
	}
}

// networkChanged immediately retires peer connections bound to the old path
// and invalidates manager-scoped ICE state. The outer P2P transports retain
// their platform route and re-negotiate without the normal reconnect delay;
// the next admission lazily rebuilds the factory against current interfaces.
func (self *WebRtcManager) networkChanged() {
	var factory *webRtcPeerConnectionFactory
	self.stateLock.Lock()
	for _, conn := range self.peerConns {
		conn.requestImmediateReconnect()
		conn.Cancel()
	}
	self.peerConnectionFactoryLock.Lock()
	if !self.peerConnectionFactoryClosed {
		factory = self.peerConnectionFactory
		self.peerConnectionFactory = nil
		self.peerConnectionFactoryInitErr = nil
		self.peerConnectionFactoryInitialized = false
		self.peerConnectionFactoryRetryTime = time.Time{}
	}
	self.peerConnectionFactoryLock.Unlock()
	self.stateLock.Unlock()

	if err := factory.Close(); err != nil && self.log.V(1).Enabled() {
		self.log.Infof("[peerconn]network-change factory close err = %s\n", err)
	}
}

// peerConnectionAdmissionError identifies a temporary capacity refusal. The
// stream remains usable over the platform route while the P2P transport waits
// for a count/budget release instead of polling and rebuilding state.
type peerConnectionAdmissionError struct {
	message string
}

func (self *peerConnectionAdmissionError) Error() string {
	return self.message
}

// AdmissionNotify returns notifications for the manager-local connection
// count and the potentially device-shared byte budget. Capture both before
// attempting admission so a release cannot be lost between the failed check
// and the wait.
func (self *WebRtcManager) AdmissionNotify() (countNotify <-chan struct{}, budgetNotify <-chan struct{}) {
	countNotify = self.capacityMonitor.NotifyChannel()
	if budget := self.settings.MemoryBudget; budget != nil {
		budgetNotify = budget.CapacityNotify()
	}
	return
}

// SignalReceiver
func (self *WebRtcManager) ReceiveSignal(source TransferPath, frame *protocol.Frame) error {
	message, err := FromFrame(frame)
	if err != nil {
		return err
	}
	if v, ok := message.(*protocol.ExchangeSignals); ok {
		return self.ReceiveExchangeSignals(source, v)
	}
	return nil
}

func (self *WebRtcManager) ReceiveExchangeSignals(source TransferPath, v *protocol.ExchangeSignals) error {
	streamId, err := IdFromBytes(v.StreamId)
	if err != nil {
		return err
	}
	key := peerConnKey{
		PeerId:   source.SourceId,
		StreamId: streamId,
	}
	var conn *peerConn
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		conn = self.peerConns[key]
		if self.log.V(2).Enabled() && conn == nil {
			self.log.Infof("[signal]miss %s (%v)\n", key, self.peerConns)
		}
	}()
	if conn == nil {
		return nil
	}
	if v.ResetSignals && conn.resetRemoteSignals() {
		// A fresh active PeerConnection is offering against a passive
		// association that still looks connected. This is the asymmetric
		// idle-blackhole case: the active side can observe its unacknowledged
		// data and restart, while the passive side has no outbound SCTP work
		// from which to infer failure. The old behavior treated the fresh
		// offer as a duplicate forever. Retire the passive association; its
		// outer transport immediately creates a new one and sends
		// WaitingForSdpOffer, which makes the active side replay the cached
		// offer without another reset.
		self.log.V(1).Infof("[peerconn]fresh offer replaces negotiated passive %s\n", conn.key)
		conn.requestImmediateReconnect()
		conn.cancel()
		return nil
	}
	var firstCandidateError error
	for _, signal := range v.Signals {
		if signal == nil {
			continue
		}
		if self.log.V(2).Enabled() {
			self.log.Infof("[signal]%s\n", signal.SignalType)
		}
		if err := conn.ReceiveSignalFromPeer(signal); err != nil {
			if signal.SignalType == protocol.SignalType_IceCandidate {
				// One malformed trickle candidate must not suppress every
				// later candidate in the same bounded batch.
				if firstCandidateError == nil {
					firstCandidateError = err
				}
				continue
			}
			return err
		}
	}
	return firstCandidateError
}

func (self *WebRtcManager) NewP2pConnActive(ctx context.Context, path TransferPath) (WebRtcConn, error) {
	return self.newP2pConn(ctx, path, true)
}

func (self *WebRtcManager) NewP2pConnPassive(ctx context.Context, path TransferPath) (WebRtcConn, error) {
	return self.newP2pConn(ctx, path, false)
}

func (self *WebRtcManager) newP2pConn(ctx context.Context, path TransferPath, active bool) (conn *peerConn, err error) {
	if err = ctx.Err(); err != nil {
		return
	}
	if err = self.ctx.Err(); err != nil {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	// Stream teardown can race while waiting for another setup to release the
	// manager lock. Do not allocate Pion state or reserve capacity for work
	// whose owner has already gone away.
	if err = ctx.Err(); err != nil {
		return
	}
	if err = self.ctx.Err(); err != nil {
		return
	}

	key := peerConnKey{
		PeerId:   path.DestinationId,
		StreamId: path.StreamId,
	}
	now := time.Now()
	self.prunePeerPrioritiesLocked(now)
	priorityUntil, priority := self.prioritizedPeers[key.PeerId]

	// Once trusted Network traffic requests a slot, do not let one of the
	// wake-all speculative waiters steal the release first. The pending entry
	// is time-bounded and removed as soon as this peer is admitted.
	if !priority && 0 < len(self.pendingPrioritizedPeerSlot) {
		err = &peerConnectionAdmissionError{
			message: "peer connection waiting for prioritized network peer",
		}
		return
	}

	// refuse new peer connections at the cap. A create for an existing key
	// replaces that connection (the map does not grow), so it is allowed.
	if maxCount := self.settings.MaxPeerConnectionCount; 0 < maxCount && maxCount <= len(self.peerConns) {
		if _, ok := self.peerConns[key]; !ok {
			if priority {
				if victim := self.oldestEvictablePeerConnLocked(now); victim != nil {
					victim.Cancel()
				}
			}
			err = &peerConnectionAdmissionError{
				message: fmt.Sprintf("peer connection limit reached (%d)", maxCount),
			}
			return
		}
	}

	// byte admission against the shared client budget: a peer connection can
	// queue up to `ReceiveBufferSize`, so each conn owns that reservation for
	// its lifetime. A new setup is refused when the budget has no headroom
	// (the stream stays on the platform transport, as at the count cap); a
	// replacement also needs real headroom. If it cannot coexist briefly with
	// the old connection, cancel the old one and retry on its release rather
	// than overdrawing the supposedly fixed memory ceiling.
	var reserveByteCount ByteCount
	if budget := self.settings.MemoryBudget; budget != nil {
		reserveByteCount = self.settings.ReceiveBufferSize
		if !budget.TryReserve(reserveByteCount) {
			if replacedConn := self.peerConns[key]; replacedConn != nil {
				replacedConn.Cancel()
			} else if priority {
				if victim := self.oldestEvictablePeerConnLocked(now); victim != nil {
					victim.Cancel()
				}
			}
			err = &peerConnectionAdmissionError{
				message: fmt.Sprintf(
					"peer connection memory budget exhausted (used=%d total=%d need=%d)",
					budget.UsedByteCount(),
					budget.TotalByteCount(),
					reserveByteCount,
				),
			}
			return
		}
	}

	conn, err = newPeerConn(
		ctx,
		key,
		path.SourceId,
		active,
		self.signalSender,
		self.settings,
		self.newPeerConnection,
	)
	if err != nil {
		if 0 < reserveByteCount {
			self.settings.MemoryBudget.Release(reserveByteCount)
		}
		return
	}
	conn.priorityUntil = priorityUntil
	go HandleError(func() {
		defer func() {
			conn.Cancel()
			self.stateLock.Lock()
			if conn == self.peerConns[key] {
				delete(self.peerConns, key)
			}
			self.stateLock.Unlock()
			if 0 < reserveByteCount {
				self.settings.MemoryBudget.Release(reserveByteCount)
			}
			self.capacityMonitor.NotifyAll()
		}()
		conn.Run()
	})

	replacedConn := self.peerConns[key]
	if replacedConn != nil {
		replacedConn.Cancel()
	}
	self.peerConns[key] = conn
	delete(self.pendingPrioritizedPeerSlot, key.PeerId)
	return
}

type peerConnKey struct {
	PeerId   Id
	StreamId Id
}

func (self peerConnKey) String() string {
	return fmt.Sprintf("s(%s) <>%s", self.StreamId, self.PeerId)
}

// conforms to WebRtcConn
type peerConn struct {
	ctx    context.Context
	cancel context.CancelFunc
	log    Logger

	key       peerConnKey
	sourceId  Id
	active    bool
	createdAt time.Time
	// priorityUntil is protected by WebRtcManager.stateLock. It lets an
	// explicitly selected Network peer retain bounded P2P admission while
	// traffic refreshes the priority, without permanently pinning a slot.
	priorityUntil time.Time

	signalSender SignalSender
	settings     *WebRtcSettings

	// api *webrtc.API
	pc *webrtc.PeerConnection

	connectedCallbacks *CallbackList[*connectedCallback]
	connMonitor        *Monitor
	connectedMonitor   *Monitor
	connectedDispatch  sync.Once
	outboundProgress   chan struct{}
	progressWatchOnce  sync.Once

	// Closed once when the outer transport should reconnect without honoring
	// the usual backoff delay. A persistent one-shot channel cannot lose a
	// notification if network change or remote restart races the caller's
	// subscription.
	immediateReconnect     chan struct{}
	immediateReconnectOnce sync.Once

	// Pion's offer/answer state machine is not safe to advance concurrently.
	// Client signal sharding serializes a peer/stream in production, but this
	// lock also protects direct SignalReceiver users and teardown races.
	// Never call SignalSender while holding it: sends are intentional
	// synchronous backpressure and can synchronously deliver the response.
	signalLock sync.Mutex
	stateLock  sync.Mutex
	conn       datachannel.ReadWriteCloserDeadliner
	connected  bool
	// Incremented with each connected transition. Callback entries use the
	// generation to suppress duplicate/late initial delivery and preserve
	// ordering when registration races an ICE state change.
	connectedGeneration uint64
	offer               *protocol.ExchangeSignal
	answer              *protocol.ExchangeSignal

	// candidates emitted before sdp negotiation completes are buffered
	// here so they aren't dropped. flushed once iceCandidatesReady is set.
	iceCandidateBuffer []*webrtc.ICECandidate
	iceCandidatesReady bool

	// Remote candidates can legally race ahead of SDP when signaling crosses
	// queues or transports. Pion rejects AddICECandidate before a remote
	// description, so retain a strictly bounded set and apply it immediately
	// after SDP succeeds instead of silently losing the path.
	remoteDescriptionSet          bool
	remoteIceCandidateBuffer      []webrtc.ICECandidateInit
	remoteIceCandidateBufferBytes int
	remoteIceCandidateOverflowLog bool

	readDeadline  time.Time
	writeDeadline time.Time
}

const (
	maxBufferedRemoteIceCandidateCount = 64
	maxBufferedRemoteIceCandidateBytes = 64 * 1024
	maxIceCandidatesPerSignalFrame     = 32
	maxIceCandidateBytesPerSignalFrame = 32 * 1024
)

func newPeerConn(
	ctx context.Context,
	key peerConnKey,
	sourceId Id,
	active bool,
	signalSender SignalSender,
	settings *WebRtcSettings,
	newPeerConnection func() (*webrtc.PeerConnection, error),
) (*peerConn, error) {
	pc, err := newPeerConnection()
	if err != nil {
		return nil, err
	}

	cancelCtx, cancel := context.WithCancel(ctx)

	conn := &peerConn{
		ctx:          cancelCtx,
		cancel:       cancel,
		log:          loggerOrDefault(settings.Log),
		key:          key,
		sourceId:     sourceId,
		active:       active,
		createdAt:    time.Now(),
		signalSender: signalSender,
		settings:     settings,
		// api:                api,
		pc:                 pc,
		connectedCallbacks: NewCallbackList[*connectedCallback](),
		connMonitor:        NewMonitor(),
		connectedMonitor:   NewMonitor(),
		outboundProgress:   make(chan struct{}, 1),
		immediateReconnect: make(chan struct{}),
	}
	return conn, nil
}

func (self *peerConn) Run() {
	defer func() {
		self.cancel()

		self.pc.Close()
		self.connMonitor.NotifyAll()
		self.connectedMonitor.NotifyAll()

		// drop any candidates that arrived before negotiation completed but
		// after Run started its early-exit path; they would otherwise be
		// retained until the peerConn is GC'd
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.iceCandidateBuffer = nil
		}()
		func() {
			self.signalLock.Lock()
			defer self.signalLock.Unlock()
			self.remoteIceCandidateBuffer = nil
			self.remoteIceCandidateBufferBytes = 0
		}()
	}()
	if self.ctx.Err() != nil {
		return
	}

	// Connected callback dispatch starts lazily with the first subscriber.
	// Failed negotiations that never install a P2P route therefore do not
	// allocate another waiting goroutine.

	self.pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		self.handleICEConnectionState(state)
	})
	self.pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		self.handlePeerConnectionState(state)
	})

	// register ice candidate handler before SetLocalDescription so candidates
	// emitted during gathering aren't dropped. candidates are buffered until
	// the negotiation is far enough along to send them (after the peer has
	// our sdp). flushIceCandidates flips the ready flag and drains the buffer.
	self.pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		var send bool
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if self.iceCandidatesReady {
				send = true
			} else {
				self.iceCandidateBuffer = append(self.iceCandidateBuffer, candidate)
			}
		}()
		if send {
			self.sendIceCandidate(candidate)
		}
	})

	if self.active {
		dc, err := self.pc.CreateDataChannel(
			self.settings.DataChannelLabel,
			webRtcDataChannelInit(self.settings),
		)
		if err != nil {
			return
		}

		dc.OnOpen(func() {
			self.handleOpenDataChannel(dc)
		})
	} else {
		self.pc.OnDataChannel(func(dc *webrtc.DataChannel) {
			if dc.Label() != self.settings.DataChannelLabel {
				self.log.V(1).Infof("[peerconn]ignoring unexpected data channel label %q\n", dc.Label())
				// Installing a custom handler replaces Pion's default handler,
				// which closes undeclared channels. Preserve that resource
				// bound explicitly so a peer cannot retain arbitrary SCTP
				// streams by opening labels this transport never consumes.
				if err := dc.Close(); err != nil && self.log.V(1).Enabled() {
					self.log.Infof("[peerconn]unexpected data channel close err = %s\n", err)
				}
				return
			}
			dc.OnOpen(func() {
				self.handleOpenDataChannel(dc)
			})
		})
	}

	if self.active {
		offer, err := self.pc.CreateOffer(nil)
		if err != nil {
			return
		}
		err = self.pc.SetLocalDescription(offer)
		if err != nil {
			return
		}

		offerBytes, err := json.Marshal(&offer)
		if err != nil {
			return
		}

		signal := &protocol.ExchangeSignal{
			SignalType: protocol.SignalType_SdpOffer,
			Sdp:        offerBytes,
		}
		self.setOfferSignal(signal)
		// Mark only the first offer from this PeerConnection generation. A
		// remote passive association may still answer ICE consent after its
		// SCTP data plane went stale; ResetSignals distinguishes this fresh
		// generation from an ordinary duplicate/replay of our cached offer.
		self.sendSignalsWithReset([]*protocol.ExchangeSignal{signal}, true)
	} else {
		signal := &protocol.ExchangeSignal{
			SignalType: protocol.SignalType_WaitingForSdpOffer,
		}
		self.sendSignal(signal)
	}

	select {
	case <-self.ctx.Done():
	}
}

func (self *peerConn) startConnectedDispatch() {
	self.connectedDispatch.Do(func() {
		// A single dispatch goroutine serializes callback invocation and emits
		// the latest state/generation, so flap-collapsing remains in order and
		// user callbacks never run on Pion's state-change goroutine.
		go HandleError(func() {
			for {
				notify := self.connectedMonitor.NotifyChannel()
				var current bool
				var generation uint64
				func() {
					self.stateLock.Lock()
					defer self.stateLock.Unlock()
					current = self.connected
					generation = self.connectedGeneration
				}()
				for _, callback := range self.connectedCallbacks.Get() {
					callback.deliver(generation, current)
				}
				select {
				case <-self.ctx.Done():
					return
				case <-notify:
				}
			}
		}, self.cancel)
	})
}

func (self *peerConn) handleICEConnectionState(state webrtc.ICEConnectionState) {
	connected := state == webrtc.ICEConnectionStateConnected ||
		state == webrtc.ICEConnectionStateCompleted
	if self.log.V(2).Enabled() {
		self.log.Infof("[peerconn]state=%v (%t)\n", state, connected)
	}
	self.setConnected(connected)
	if state == webrtc.ICEConnectionStateFailed ||
		state == webrtc.ICEConnectionStateClosed {
		// A failed ICE agent does not necessarily close PeerConnection or the
		// detached data channel. Without explicit cancellation the route
		// disappears but its peer slot/reservation and outer setup loop can
		// remain stranded forever.
		if self.ctx.Err() == nil {
			self.cancel()
		}
	}
}

func (self *peerConn) handlePeerConnectionState(state webrtc.PeerConnectionState) {
	if state == webrtc.PeerConnectionStateFailed ||
		state == webrtc.PeerConnectionStateClosed {
		// Covers DTLS/SCTP failures that occur while ICE itself still reports
		// connected. Cancellation is idempotent with ICE and manager/network
		// change teardown.
		if self.ctx.Err() == nil {
			self.cancel()
		}
	}
}

func (self *peerConn) ReceiveSignalFromPeer(signal *protocol.ExchangeSignal) error {
	if signal == nil {
		return nil
	}

	self.signalLock.Lock()
	toSend, flushLocalCandidates, immediateReconnect, fatal, err := self.receiveSignalFromPeerLocked(signal)
	self.signalLock.Unlock()

	if err != nil {
		if fatal {
			self.requestImmediateReconnect()
			self.cancel()
		}
		return err
	}
	// These sends intentionally remain synchronous. Keeping them outside
	// signalLock permits a synchronous answer/candidate response without a
	// lock cycle while preserving transfer-client backpressure.
	if len(toSend) != 0 {
		self.sendSignals(toSend)
	}
	if flushLocalCandidates {
		self.flushIceCandidates()
	}
	if immediateReconnect {
		self.log.V(1).Infof("[peerconn]waiting-for-offer after answer; requesting immediate reconnect\n")
		self.requestImmediateReconnect()
		self.cancel()
	}
	return nil
}

// resetRemoteSignals applies ExchangeSignals.reset_signals. It returns true
// when this passive PeerConnection has already accepted an offer and therefore
// must be replaced rather than mutated in place. Pion does not support
// rewinding an established offer/answer/ICE/DTLS/SCTP stack to a pristine
// generation.
func (self *peerConn) resetRemoteSignals() bool {
	self.signalLock.Lock()
	defer self.signalLock.Unlock()

	if self.active {
		return false
	}
	if self.offerSignal() != nil {
		return true
	}
	// A fresh passive connection can accept the reset's offer directly, but
	// any candidates received before the generation boundary belong to the
	// abandoned association and must not be applied to it.
	self.remoteIceCandidateBuffer = nil
	self.remoteIceCandidateBufferBytes = 0
	self.remoteIceCandidateOverflowLog = false
	return false
}

func (self *peerConn) receiveSignalFromPeerLocked(
	signal *protocol.ExchangeSignal,
) (
	toSend []*protocol.ExchangeSignal,
	flushLocalCandidates bool,
	immediateReconnect bool,
	fatal bool,
	err error,
) {
	switch signal.SignalType {
	case protocol.SignalType_SdpOffer:
		if self.active {
			return
		}
		if self.offerSignal() != nil {
			// already accepted an offer; ignore the duplicate
			return
		}
		// Decode and apply before committing our cached offer. A malformed or
		// semantically invalid first frame must not poison later retransmits.
		var offer webrtc.SessionDescription
		if err = json.Unmarshal(signal.Sdp, &offer); err != nil {
			return
		}
		if err = self.pc.SetRemoteDescription(offer); err != nil {
			return
		}
		self.setOfferSignal(signal)
		self.remoteDescriptionSet = true
		self.flushRemoteIceCandidatesLocked()

		var answer webrtc.SessionDescription
		answer, err = self.pc.CreateAnswer(nil)
		if err != nil {
			fatal = true
			return
		}
		if err = self.pc.SetLocalDescription(answer); err != nil {
			fatal = true
			return
		}
		var answerBytes []byte
		answerBytes, err = json.Marshal(&answer)
		if err != nil {
			fatal = true
			return
		}
		answerSignal := &protocol.ExchangeSignal{
			SignalType: protocol.SignalType_SdpAnswer,
			Sdp:        answerBytes,
		}
		self.setAnswerSignal(answerSignal)
		toSend = []*protocol.ExchangeSignal{answerSignal}
		flushLocalCandidates = true

	case protocol.SignalType_SdpAnswer:
		if !self.active {
			return
		}
		if self.answerSignal() != nil {
			// already accepted an answer; ignore the duplicate
			return
		}
		// As with offers, do not cache an invalid first answer.
		var answer webrtc.SessionDescription
		if err = json.Unmarshal(signal.Sdp, &answer); err != nil {
			return
		}
		if err = self.pc.SetRemoteDescription(answer); err != nil {
			return
		}
		self.setAnswerSignal(signal)
		self.remoteDescriptionSet = true
		self.flushRemoteIceCandidatesLocked()
		flushLocalCandidates = true

	case protocol.SignalType_IceCandidate:
		var candidate webrtc.ICECandidateInit
		if err = json.Unmarshal(signal.IceCandidate, &candidate); err != nil {
			return
		}
		if !self.remoteDescriptionSet {
			self.bufferRemoteIceCandidateLocked(candidate)
			return
		}
		self.addRemoteIceCandidateLocked(candidate)

	case protocol.SignalType_WaitingForSdpOffer:
		if !self.active {
			break
		}
		if self.answerSignal() == nil {
			// not yet negotiated; re-send our cached offer
			if signal := self.offerSignal(); signal != nil {
				toSend = []*protocol.ExchangeSignal{signal}
			}
		} else {
			// peer is asking for a fresh offer despite our prior answer.
			// they likely restarted; signal the outer transport to reconnect
			// without backoff, then cancel.
			immediateReconnect = true
		}
	}
	return
}

func iceCandidateInitByteCount(candidate webrtc.ICECandidateInit) int {
	n := len(candidate.Candidate)
	if candidate.SDPMid != nil {
		n += len(*candidate.SDPMid)
	}
	if candidate.UsernameFragment != nil {
		n += len(*candidate.UsernameFragment)
	}
	return n
}

// signalLock must be held.
func (self *peerConn) bufferRemoteIceCandidateLocked(candidate webrtc.ICECandidateInit) {
	byteCount := iceCandidateInitByteCount(candidate)
	if maxBufferedRemoteIceCandidateCount <= len(self.remoteIceCandidateBuffer) ||
		maxBufferedRemoteIceCandidateBytes < self.remoteIceCandidateBufferBytes+byteCount {
		if !self.remoteIceCandidateOverflowLog {
			self.remoteIceCandidateOverflowLog = true
			self.log.Infof(
				"[peerconn]remote ICE pre-SDP buffer full (count=%d bytes=%d); dropping excess candidates\n",
				len(self.remoteIceCandidateBuffer),
				self.remoteIceCandidateBufferBytes,
			)
		}
		return
	}
	self.remoteIceCandidateBuffer = append(self.remoteIceCandidateBuffer, candidate)
	self.remoteIceCandidateBufferBytes += byteCount
}

// signalLock must be held.
func (self *peerConn) addRemoteIceCandidateLocked(candidate webrtc.ICECandidateInit) {
	// A malformed individual candidate should not discard a valid SDP
	// negotiation or other paths. Pion also treats several unsupported
	// candidate forms as a deliberate no-op.
	if err := self.pc.AddICECandidate(candidate); err != nil && self.log.V(1).Enabled() {
		self.log.Infof("[peerconn]AddICECandidate err = %s\n", err)
	}
}

// signalLock must be held.
func (self *peerConn) flushRemoteIceCandidatesLocked() {
	candidates := self.remoteIceCandidateBuffer
	self.remoteIceCandidateBuffer = nil
	self.remoteIceCandidateBufferBytes = 0
	for _, candidate := range candidates {
		self.addRemoteIceCandidateLocked(candidate)
	}
}

func (self *peerConn) sendIceCandidate(candidate *webrtc.ICECandidate) {
	self.sendIceCandidates([]*webrtc.ICECandidate{candidate})
}

func (self *peerConn) sendIceCandidates(candidates []*webrtc.ICECandidate) {
	signals := make([]*protocol.ExchangeSignal, 0, min(len(candidates), maxIceCandidatesPerSignalFrame))
	signalBytes := 0
	flush := func() {
		if len(signals) != 0 {
			self.sendSignals(signals)
			signals = make([]*protocol.ExchangeSignal, 0, min(len(candidates), maxIceCandidatesPerSignalFrame))
			signalBytes = 0
		}
	}
	for _, candidate := range candidates {
		if candidate == nil {
			continue
		}
		candidateBytes, err := json.Marshal(candidate.ToJSON())
		if err != nil {
			continue
		}
		if 0 < len(signals) &&
			(maxIceCandidatesPerSignalFrame <= len(signals) ||
				maxIceCandidateBytesPerSignalFrame < signalBytes+len(candidateBytes)) {
			// Keep signal frames bounded. If an unusual multihomed server
			// gathers many candidates, additional frames flow through the
			// normal synchronous send callback/backpressure rather than
			// creating one oversized protobuf.
			flush()
		}
		signals = append(signals, &protocol.ExchangeSignal{
			SignalType:   protocol.SignalType_IceCandidate,
			IceCandidate: candidateBytes,
		})
		signalBytes += len(candidateBytes)
	}
	flush()
}

func (self *peerConn) flushIceCandidates() {
	var toSend []*webrtc.ICECandidate
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.iceCandidatesReady = true
		toSend = self.iceCandidateBuffer
		self.iceCandidateBuffer = nil
	}()
	// Candidates gathered before SDP readiness are already adjacent and
	// share one destination. Send one protobuf frame instead of one transfer
	// callback/frame per interface, cutting allocations and queue pressure
	// without adding a timer or delaying late trickle candidates.
	self.sendIceCandidates(toSend)
}

// ImmediateReconnect returns a persistent one-shot channel that closes when
// the outer transport should reconnect without backoff.
func (self *peerConn) ImmediateReconnect() <-chan struct{} {
	return self.immediateReconnect
}

func (self *peerConn) requestImmediateReconnect() {
	self.immediateReconnectOnce.Do(func() {
		close(self.immediateReconnect)
	})
}

func (self *peerConn) Connected() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return self.connected
}

func (self *peerConn) setConnected(connected bool) {
	changed := false

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if self.connected != connected {
			self.connected = connected
			self.connectedGeneration++
			changed = true
		}
	}()

	if changed {
		if connected && self.log.V(1).Enabled() {
			// One low-frequency line identifies whether the formed route is
			// direct and which address family won ICE. This is essential when
			// a nominally connected network peer is actually using a slow
			// server-reflexive path; keep it off the packet hot path.
			self.log.Infof(
				"[peerconn]connected %s local=%s remote=%s\n",
				self.key,
				self.LocalAddr(),
				self.RemoteAddr(),
			)
		}
		// signal the dispatch goroutine; it will read the latest state
		// under the lock and serialize callback invocation. this avoids
		// out-of-order observation if two setConnected calls race.
		self.connectedMonitor.NotifyAll()
	}
}

func (self *peerConn) AddConnectedCallback(callbackFunc func(connected bool)) func() {
	callback := &connectedCallback{
		callback: callbackFunc,
	}
	callbackId := self.connectedCallbacks.Add(callback)
	self.startConnectedDispatch()
	// Fire current state so a late subscriber doesn't miss a transition that
	// the dispatch goroutine has already emitted. Generation-aware delivery
	// makes this safe if a newer transition is concurrently delivered first.
	self.stateLock.Lock()
	connected := self.connected
	generation := self.connectedGeneration
	self.stateLock.Unlock()
	callback.deliver(generation, connected)
	return func() {
		callback.close()
		self.connectedCallbacks.Remove(callbackId)
	}
}

type connectedCallback struct {
	lock           sync.Mutex
	callback       func(bool)
	lastGeneration uint64
	delivered      bool
	closed         bool
}

func (self *connectedCallback) deliver(generation uint64, connected bool) {
	self.lock.Lock()
	defer self.lock.Unlock()
	if self.closed || (self.delivered && generation <= self.lastGeneration) {
		return
	}
	self.delivered = true
	self.lastGeneration = generation
	HandleError(func() {
		self.callback(connected)
	})
}

func (self *connectedCallback) close() {
	self.lock.Lock()
	self.closed = true
	self.lock.Unlock()
}

func (self *peerConn) setOfferSignal(offer *protocol.ExchangeSignal) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.offer != nil {
		return false
	}
	self.offer = offer
	return true
}

func (self *peerConn) offerSignal() *protocol.ExchangeSignal {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.offer
}

func (self *peerConn) setAnswerSignal(answer *protocol.ExchangeSignal) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.answer != nil {
		return false
	}
	self.answer = answer
	return true
}

func (self *peerConn) answerSignal() *protocol.ExchangeSignal {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.answer
}

func (self *peerConn) sendSignal(signal *protocol.ExchangeSignal) {
	self.sendSignals([]*protocol.ExchangeSignal{signal})
}

func (self *peerConn) sendSignals(signalValues []*protocol.ExchangeSignal) {
	self.sendSignalsWithReset(signalValues, false)
}

func (self *peerConn) sendSignalsWithReset(signalValues []*protocol.ExchangeSignal, resetSignals bool) {
	if len(signalValues) == 0 {
		return
	}
	signals := &protocol.ExchangeSignals{
		StreamId:     self.key.StreamId.Bytes(),
		ResetSignals: resetSignals,
		Signals:      signalValues,
	}
	// passive peers send signals in the return direction of the stream,
	// so they ask for a companion contract (verified as
	// ProvideMode_Stream). this is what the destination is willing to
	// accept in the asymmetric case where it only enables Stream for
	// return traffic.
	//
	// active peers ride the stream itself (`ForceStream`): p2p exists only
	// for a stream, and a network peer's data path is ForceStream (the
	// multi-client forces AllowDirect). The peer is frequently an ephemeral
	// per-window provider client that grants ONLY the stream contract for
	// this pair — the initiator has no plain contract to it — so a
	// default-opts (plain contract) offer is undeliverable while data flows
	// fine over the stream. Sending the offer with ForceStream routes it onto
	// the same stream contract the data uses, so it reaches the peer. This is
	// the §16-class alignment: a send-sequence-keying option invisible to the
	// signaling layer must match the data path or the signal forks onto an
	// undeliverable sequence. See PACKETRESEARCH1 §17.
	var opts []any
	if self.active {
		opts = append(opts, ForceStream())
	} else {
		opts = append(opts, CompanionContract())
	}
	self.signalSender.SendSignal(
		DestinationId(self.key.PeerId).AddSource(self.sourceId),
		RequireToFrameWithDefaultProtocolVersion(signals),
		opts...,
	)
}

func (self *peerConn) handleOpenDataChannel(dc *webrtc.DataChannel) {
	if err := self.setOpenDataChannel(dc); err != nil {
		self.log.V(1).Infof("[peerconn]data channel detach err = %s\n", err)
		self.requestImmediateReconnect()
		self.cancel()
	}
}

func (self *peerConn) setOpenDataChannel(dc *webrtc.DataChannel) error {
	conn, err := detachWithDeadline(dc)
	if err != nil {
		return err
	}

	var prev datachannel.ReadWriteCloserDeadliner
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		prev = self.conn
		if prev == nil {
			self.conn = conn
			self.connMonitor.NotifyAll()
		}
	}()
	if prev != nil {
		// One peer connection carries one transport. A duplicate channel must
		// not replace and close the live backpressured net.Conn.
		conn.Close()
	}

	return nil
}

func (self *peerConn) dataChannelConn(deadline time.Time) (datachannel.ReadWriteCloserDeadliner, error) {
	conn := func() (datachannel.ReadWriteCloserDeadliner, chan struct{}) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.conn, self.connMonitor.NotifyChannel()
	}
	var deadlineTimer *time.Timer
	var deadlineC <-chan time.Time
	if !deadline.IsZero() {
		timeout := time.Until(deadline)
		if timeout <= 0 {
			return nil, os.ErrDeadlineExceeded
		}
		deadlineTimer = time.NewTimer(timeout)
		deadlineC = deadlineTimer.C
		defer deadlineTimer.Stop()
	}
	for {
		c, update := conn()
		if c != nil {
			return c, nil
		}
		select {
		case <-self.ctx.Done():
			return nil, os.ErrClosed
		case <-update:
		case <-deadlineC:
			return nil, os.ErrDeadlineExceeded
		}
	}
}

func (self *peerConn) Read(b []byte) (n int, err error) {
	var deadline time.Time
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		deadline = self.readDeadline
	}()
	var c datachannel.ReadWriteCloserDeadliner
	c, err = self.dataChannelConn(deadline)
	if err != nil {
		return
	}
	c.SetReadDeadline(deadline)
	n, err = c.Read(b)
	return
}

func (self *peerConn) noteOutboundSctpActivity() {
	if self.settings.SctpNoProgressTimeout <= 0 {
		return
	}
	self.progressWatchOnce.Do(func() {
		go HandleError(self.runSctpProgressWatchdog, self.cancel)
	})
	select {
	case self.outboundProgress <- struct{}{}:
	default:
	}
}

// runSctpProgressWatchdog closes a half-open data plane that ICE consent
// cannot see. Pion's reliable SCTP association retries indefinitely; if the
// remote SCTP endpoint disappears while its ICE agent/socket still answers,
// PeerConnection remains "connected" and a small first write after an idle
// period can otherwise sit unacknowledged forever.
//
// The worker starts only after the first successful application write. It has
// no idle ticker: while the SCTP buffered amount is zero it waits on the
// coalescing outbound-activity channel.
// With data outstanding, any reverse SCTP packet (normally a SACK) refreshes
// the deadline. This observes transport progress without putting a timeout
// around intentional transfer callback backpressure.
func (self *peerConn) runSctpProgressWatchdog() {
	timeout := self.settings.SctpNoProgressTimeout
	sampleInterval := min(250*time.Millisecond, timeout/4)
	if sampleInterval <= 0 {
		sampleInterval = time.Millisecond
	}
	var sampleTimer *time.Timer
	defer func() {
		if sampleTimer != nil {
			sampleTimer.Stop()
		}
	}()

	for {
		select {
		case <-self.ctx.Done():
			return
		case <-self.outboundProgress:
		}

		bufferedAmount, lastReceived, ok := webRtcSctpProgress(self.pc)
		if !ok {
			continue
		}
		lastProgress := time.Now()
		for 0 < bufferedAmount {
			remaining := timeout - time.Since(lastProgress)
			if remaining <= 0 {
				self.log.Infof(
					"[peerconn]SCTP no progress for %s with %d bytes buffered; reconnecting\n",
					timeout,
					bufferedAmount,
				)
				self.requestImmediateReconnect()
				self.cancel()
				return
			}

			sample := min(sampleInterval, remaining)
			sampleC := resetOrCreateTimer(&sampleTimer, sample)
			select {
			case <-self.ctx.Done():
				return
			case <-sampleC:
			}

			var received uint64
			bufferedAmount, received, ok = webRtcSctpProgress(self.pc)
			if !ok {
				break
			}
			if received != lastReceived {
				lastReceived = received
				lastProgress = time.Now()
			}
		}
	}
}

func (self *peerConn) Write(b []byte) (n int, err error) {
	var deadline time.Time
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		deadline = self.writeDeadline
	}()
	var c datachannel.ReadWriteCloserDeadliner
	c, err = self.dataChannelConn(deadline)
	if err != nil {
		return
	}
	c.SetWriteDeadline(deadline)
	n, err = c.Write(b)
	if 0 < n {
		self.noteOutboundSctpActivity()
	}
	return
}

// LocalAddr returns the local network address, if known.
func (self *peerConn) LocalAddr() net.Addr {
	sctp := self.pc.SCTP()
	if sctp == nil {
		return newWebRtcAddr("")
	}
	dtls := sctp.Transport()
	if dtls == nil {
		return newWebRtcAddr("")
	}
	ice := dtls.ICETransport()
	if ice == nil {
		return newWebRtcAddr("")
	}
	pair, err := ice.GetSelectedCandidatePair()
	if err != nil || pair == nil {
		return newWebRtcAddr("")
	}
	return newWebRtcAddr(pair.Local.Address)
}

// RemoteAddr returns the remote network address, if known.
func (self *peerConn) RemoteAddr() net.Addr {
	sctp := self.pc.SCTP()
	if sctp == nil {
		return newWebRtcAddr("")
	}
	dtls := sctp.Transport()
	if dtls == nil {
		return newWebRtcAddr("")
	}
	ice := dtls.ICETransport()
	if ice == nil {
		return newWebRtcAddr("")
	}
	pair, err := ice.GetSelectedCandidatePair()
	if err != nil || pair == nil {
		return newWebRtcAddr("")
	}
	return newWebRtcAddr(pair.Remote.Address)
}

func (self *peerConn) SetDeadline(t time.Time) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.readDeadline = t
	self.writeDeadline = t

	return nil
}

func (self *peerConn) SetReadDeadline(t time.Time) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.readDeadline = t

	return nil
}

func (self *peerConn) SetWriteDeadline(t time.Time) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.writeDeadline = t

	return nil
}

func (self *peerConn) Close() error {
	self.cancel()
	return nil
}

func (self *peerConn) Cancel() {
	self.cancel()
}

// conforms to `net.Addr`
type webRtcAddr struct {
	addr string
}

func newWebRtcAddr(addr string) net.Addr {
	return &webRtcAddr{
		addr: addr,
	}
}

func (self *webRtcAddr) Network() string {
	return "udp"
}

func (self *webRtcAddr) String() string {
	return self.addr
}
