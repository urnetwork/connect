// Real native TCP window updates cross the public ready-only Multi entry and
// genuine Transfer delivery custody before provider persist repairs a refusal.
package connect

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A single pipe is admitted once; the HTTP server owns the accepted endpoint.
type persistNativeListener struct {
	connection net.Conn
	accepted   atomic.Bool
	closed     chan struct{}
	closeDo    sync.Once
}

func (self *persistNativeListener) Accept() (net.Conn, error) {
	select {
	case <-self.closed:
		return nil, net.ErrClosed
	default:
	}
	if self.accepted.CompareAndSwap(false, true) {
		return self.connection, nil
	}
	<-self.closed
	return nil, net.ErrClosed
}

func (self *persistNativeListener) Close() error {
	self.closeDo.Do(func() { close(self.closed) })
	return nil
}

func (self *persistNativeListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(198, 51, 100, 19), Port: 80}
}

// Value-only full IPv4 flow/header evidence survives a consuming native offer.
type persistNativeHeader struct {
	source, destination         [4]byte
	sourcePort, destinationPort uint16
	seq, ack                    uint32
	window                      uint16
	payload                     int
	ackFlag, syn, fin, rst      bool
}

// Borrows packet only while copying exact tuple/sequence/window evidence.
func persistNativePacketHeader(packet []byte) (persistNativeHeader, *IpPath, error) {
	path, payload, err := ParseIpPathWithPayload(packet)
	if err != nil {
		return persistNativeHeader{}, nil, err
	}
	if path.Version != 4 || path.Protocol != IpProtocolTcp {
		return persistNativeHeader{}, nil, errors.New("expected one native IPv4 TCP flow")
	}
	h := persistNativeHeader{sourcePort: uint16(path.SourcePort), destinationPort: uint16(path.DestinationPort),
		seq: path.SequenceNumber, ack: path.AckSequenceNumber, window: path.TcpWindowSize,
		payload: len(payload), ackFlag: path.Ack, syn: path.Syn, fin: path.Fin, rst: path.Rst}
	copy(h.source[:], path.SourceIp.To4())
	copy(h.destination[:], path.DestinationIp.To4())
	return h, path, nil
}

func (self persistNativeHeader) pureAck() bool {
	return self.ackFlag && !self.syn && !self.fin && !self.rst && self.payload == 0
}

func (self persistNativeHeader) opens(zero persistNativeHeader) bool {
	return self.pureAck() && self.window != 0 && self.source == zero.source && self.destination == zero.destination &&
		self.sourcePort == zero.sourcePort && self.destinationPort == zero.destinationPort && self.seq == zero.seq && self.ack == zero.ack
}

// The exact retained Transfer item/generation, not a transport-local token.
type persistNativeWire struct {
	owner               sendSequenceId
	generation, message Id
	number              uint64
	sequence            *SendSequence
	header              persistNativeHeader
}

// Only actual public native results and real collapse predicates are recorded.
type persistNativeOffer struct {
	header                        persistNativeHeader
	accepted                      bool
	count                         int
	beforeEligible, afterEligible bool
	beforeSeen, afterSeen         bool
	beforeWindow, afterWindow     uint16
	beforeAck, afterAck           uint32
	collapseDrops                 uint64
	elapsed                       time.Duration
	copy                          []byte
	wire                          *persistNativeWire
}

// Copied by the source worker at its own stable observation boundaries.
type persistNativeCapacityObservation struct {
	at                time.Time
	observations      uint64
	afterPublication  bool
	currentGeneration bool
	retained          bool
	held              bool
	number            uint64
	queued            int
	unavailable       bool
	effectiveWindow   ByteCount
	ceiling           ByteCount
}

// One connection, one held receipt, one paused read owner, one probe packet.
// Prelude/recovery serialization waits for genuine Transfer ACKs; the exact
// discriminating reopen offers remain ordinary public zero-wait offers.
type persistNativeFixture struct {
	t                                    *testing.T
	ctx                                  context.Context
	cancel                               context.CancelFunc
	closing                              atomic.Bool
	holdZero                             bool
	tun                                  *Tun
	connection                           net.Conn
	sender, receiver                     *Client
	parent                               *RemoteUserNatMultiClient
	selected                             *multiClientChannel
	nat                                  *LocalUserNat
	provider                             *RemoteUserNatProvider
	local, origin                        net.Conn
	listener                             *persistNativeListener
	server                               *http.Server
	serverDone, bridgeDone               chan struct{}
	serverConnections, handlers          sync.WaitGroup
	bridgeStarted                        bool
	forward, reverse                     Route
	body                                 []byte
	dials                                atomic.Int64
	providerSequence                     atomic.Pointer[TcpSequence]
	waits                                chan zeroWindowWaitSnapshot
	waitLocker                           atomic.Pointer[zeroWindowWaitLocker]
	syn, synAck                          atomic.Pointer[persistNativeHeader]
	firstReturnData                      atomic.Pointer[persistNativeHeader]
	zero                                 atomic.Pointer[persistNativeHeader]
	zeroWire, sourceWire, openWire       atomic.Pointer[persistNativeWire]
	held                                 atomic.Pointer[receiveDeliveryClaim]
	claimTaken                           atomic.Bool
	claimReady, capacityReady, zeroAcked chan struct{}
	capacityDo, zeroAckDo                sync.Once
	capacityStateLock                    sync.Mutex
	capacityState, capacityPublished     persistNativeCapacityObservation
	zeroOffered                          chan persistNativeOffer
	serialAcks                           chan persistNativeWire
	resumeBridge, releaseProbe           chan struct{}
	resumeDo, probeReleaseDo             sync.Once
	probeSeen                            chan persistNativeHeader
	probeDo                              sync.Once
	regenerated                          chan persistNativeOffer
	nativeOffers                         atomic.Int64
}

// Root one requires the complete native-refusal -> persist -> actual ACK chain.
func TestTcpPersistNativeRefusedReopenRecoversExactHttp(t *testing.T) {
	checkPersistNativeReopen(t, true, false)
}

// The same actual window-update admission must work with no held credit.
func TestTcpPersistNativeAcceptedReopenControl(t *testing.T) {
	checkPersistNativeReopen(t, false, false)
}

// Canceling the parked flow joins it while shared parents remain live.
func TestTcpPersistNativeRefusedReopenCancellationOwnsBuffers(t *testing.T) {
	checkPersistNativeReopen(t, true, true)
}

// All test policy is fixed before constructors; no production/lane/NoAck or
// collapse setting is changed after a packet has been admitted.
func newPersistNativeFixture(t *testing.T, holdZero bool) *persistNativeFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	local, origin := net.Pipe()
	f := &persistNativeFixture{t: t, ctx: ctx, cancel: cancel, holdZero: holdZero, local: local, origin: origin,
		forward: make(Route, 16), reverse: make(Route, 16), body: bytes.Repeat([]byte("native-persist"), 2600),
		serverDone: make(chan struct{}), bridgeDone: make(chan struct{}), waits: make(chan zeroWindowWaitSnapshot, 16),
		claimReady: make(chan struct{}), capacityReady: make(chan struct{}), zeroAcked: make(chan struct{}),
		zeroOffered: make(chan persistNativeOffer, 1), serialAcks: make(chan persistNativeWire, 16),
		resumeBridge: make(chan struct{}), releaseProbe: make(chan struct{}), probeSeen: make(chan persistNativeHeader, 1), regenerated: make(chan persistNativeOffer, 8)}
	t.Cleanup(f.close)
	f.listener = &persistNativeListener{connection: origin, closed: make(chan struct{})}
	f.server = &http.Server{ConnState: func(_ net.Conn, state http.ConnState) {
		switch state {
		case http.StateNew:
			f.serverConnections.Add(1)
		case http.StateClosed, http.StateHijacked:
			f.serverConnections.Done()
		}
	}, Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		f.handlers.Add(1)
		defer f.handlers.Done()
		body := f.body
		if request.URL.Path == "/reuse" {
			body = []byte("reuse")
		}
		writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = writer.Write(body)
	})}
	go func() { defer close(f.serverDone); _ = f.server.Serve(f.listener) }()
	tunSettings := DefaultTunSettingsWithBufferSize(64)
	tunSettings.Log = NewNoopLogger()
	tunSettings.DialRace = 1
	tunSettings.TcpReceiveBuffer = TcpBufferRange{Min: 16 * 1024, Default: 16 * 1024, Max: 16 * 1024}
	var err error
	f.tun, err = CreateTun(ctx, tunSettings)
	if err != nil {
		t.Fatal(err)
	}
	newClient := func(source bool) *Client {
		var client *Client
		settings := closeWaitClientSettings()
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		settings.SendBufferSettings.ResendQueueBudget = NewTransferMemoryBudget(2 * 1024 * 1024)
		settings.SendBufferSettings.ResendQueueRetainedByteAccounting = true
		if source {
			// An accepted ACK alone fills this deliberately tiny protocol window.
			// Both arms use it; the entire real retained budget remains fixed.
			settings.SendBufferSettings.ResendQueueMaxByteCount = 1
			settings.SendBufferSettings.ResendQueueMinByteCount = 0
			// Max is only the adaptive opening. Pin this fixture's intended
			// one-item limit with the supported hard ceiling, not a policy bypass.
			settings.SendBufferSettings.DeliverySizedWindowCeilingByteCount = 1
			if !settings.SendBufferSettings.WindowSizingActive() {
				t.Fatal("native custody fixture requires the adaptive window policy")
			}
			settings.SendBufferSettings.TransferWireMessageObserver = func(event TransferWireMessageObservation) {
				if event.Resend || f.closing.Load() {
					return
				}
				pack := decodeSendPackLifecycleWirePack(t, event.TransferFrameBytes)
				for _, frame := range pack.Frames {
					if frame.MessageType != protocol.MessageType_IpIpPacketToProvider {
						continue
					}
					packet, err := ipPacketToProviderBytes(frame)
					if err != nil {
						t.Error(err)
						continue
					}
					header, _, err := persistNativePacketHeader(packet)
					generation, generationErr := IdFromBytes(pack.SequenceId)
					message, messageErr := IdFromBytes(pack.MessageId)
					if err != nil || generationErr != nil || messageErr != nil || pack.Nack {
						t.Error("actual TCP packet lost reliable Transfer generation")
						continue
					}
					client.sendBuffer.mutex.Lock()
					sequence := client.sendBuffer.sendSequencesBySequenceId[generation]
					var owner sendSequenceId
					if sequence != nil {
						owner = sequence.id()
					}
					current := sequence != nil && client.sendBuffer.sendSequences[owner] == sequence
					client.sendBuffer.mutex.Unlock()
					if !current || owner.Destination != f.receiver.ClientId() || owner.LogicalLane != pack.LogicalLane || owner.ForceStream != pack.ForceStream || owner.CompanionContract != pack.CompanionContract {
						t.Error("actual native packet lost its complete current owner")
						continue
					}
					wire := &persistNativeWire{owner: owner, generation: generation, message: message, number: pack.SequenceNumber, sequence: sequence, header: header}
					// One accepted native offer is outstanding. Negotiated lane
					// activation may move later packets away from the initial SYN.
					f.sourceWire.Store(wire)
					if zero := f.zero.Load(); zero != nil && header == *zero {
						f.zeroWire.CompareAndSwap(nil, wire)
					}
					if zero := f.zero.Load(); zero != nil && header.opens(*zero) {
						f.openWire.CompareAndSwap(nil, wire)
					}
				}
			}
			observeCapacity := func(owner sendSequenceId, afterPublication bool) {
				wire := f.zeroWire.Load()
				if wire == nil || owner != wire.owner || !f.claimTaken.Load() || f.closing.Load() {
					return
				}
				number, retained := wire.sequence.retainedAckSequenceNumber(wire.message)
				client.sendBuffer.mutex.Lock()
				current := client.sendBuffer.sendSequences[owner] == wire.sequence &&
					client.sendBuffer.sendSequencesBySequenceId[wire.generation] == wire.sequence
				client.sendBuffer.mutex.Unlock()
				window := wire.sequence.sendWindowSnapshot(time.Now())
				observation := persistNativeCapacityObservation{
					at: time.Now(), afterPublication: afterPublication, currentGeneration: current,
					retained: retained, held: f.held.Load() != nil, number: number,
					queued:          wire.sequence.resendQueue.Len(),
					unavailable:     wire.sequence.resendCapacityUnavailable.Load(),
					effectiveWindow: window.Window, ceiling: window.Ceiling,
				}
				ready := afterPublication && observation.unavailable && current && retained &&
					number == wire.number && observation.held && window.Window == 1 && window.Ceiling == 1
				func() {
					f.capacityStateLock.Lock()
					defer f.capacityStateLock.Unlock()
					observation.observations = f.capacityState.observations + 1
					f.capacityState = observation
					if ready {
						f.capacityPublished = observation
					}
				}()
				if ready {
					f.capacityDo.Do(func() { close(f.capacityReady) })
				}
			}
			// The ACK snapshot precedes capacity publication; it is evidence only.
			settings.SendBufferSettings.afterApplyAckSnapshotForTest = func(owner sendSequenceId) {
				observeCapacity(owner, false)
			}
			settings.SendBufferSettings.beforeResendCapacityWaitForTest = func(owner sendSequenceId) {
				observeCapacity(owner, true)
			}
			settings.SendBufferSettings.afterAckSendItemForTest = func(owner sendSequenceId, number uint64) {
				wire := f.sourceWire.Load()
				if wire == nil || owner != wire.owner || number != wire.number || f.closing.Load() {
					return
				}
				client.sendBuffer.mutex.Lock()
				current := client.sendBuffer.sendSequences[owner] == wire.sequence && client.sendBuffer.sendSequencesBySequenceId[wire.generation] == wire.sequence
				client.sendBuffer.mutex.Unlock()
				if !current {
					t.Error("native ACK retirement changed full send generation")
					return
				}
				select {
				case f.serialAcks <- *wire:
				default:
					t.Error("serial native ACK receipts exceeded fixed slots")
				}
				if zero := f.zeroWire.Load(); zero != nil && zero.owner == owner && zero.number == number {
					f.zeroAckDo.Do(func() { close(f.zeroAcked) })
				}
			}
		}
		client = NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		return client
	}
	f.sender = newClient(true)
	f.receiver = newClient(false)
	f.sender.ContractManager().AddNoContractPeer(f.receiver.ClientId())
	f.receiver.ContractManager().AddNoContractPeer(f.sender.ClientId())
	f.sender.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(f.receiver.ClientId())), []Route{f.forward})
	f.receiver.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{f.forward})
	f.receiver.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(f.sender.ClientId())), []Route{f.reverse})
	f.sender.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{f.reverse})
	f.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, peer Peer) {
		if !f.holdZero || f.closing.Load() || ctx.Err() != nil || f.claimTaken.Load() {
			return
		}
		zero := f.zero.Load()
		if zero == nil {
			return
		}
		for _, frame := range frames {
			if frame.MessageType != protocol.MessageType_IpIpPacketToProvider {
				continue
			}
			packet, err := ipPacketToProviderBytes(frame)
			if err != nil {
				continue
			}
			header, _, err := persistNativePacketHeader(packet)
			if err != nil || header != *zero || !f.claimTaken.CompareAndSwap(false, true) {
				continue
			}
			claim := peer.deliveryReceipt().hold()
			if claim == nil {
				t.Error("accepted zero-window ACK lacks real delivery custody")
				return
			}
			f.held.Store(claim)
			if f.closing.Load() {
				if f.held.CompareAndSwap(claim, nil) {
					claim.complete(false)
				}
				return
			}
			close(f.claimReady)
			if wire := f.zeroWire.Load(); wire != nil {
				wire.sequence.ackWindow.Wake()
			}
		}
	})
	natSettings := DefaultLocalUserNatSettings()
	natSettings.Log = NewNoopLogger()
	natSettings.TcpBufferSettings.ReturnQueueBudget = NewTransferMemoryBudget(2 * 1024 * 1024)
	natSettings.TcpBufferSettings.beforeSequenceRunWithStateForTest = func(sequence *TcpSequence) {
		if !f.providerSequence.CompareAndSwap(nil, sequence) {
			t.Error("test created a second provider TCP flow")
			return
		}
		locker := &zeroWindowWaitLocker{sequence: sequence, waited: f.waits}
		f.waitLocker.Store(locker)
		sequence.receiveAckCondOnce.Do(func() { sequence.receiveAckCond = sync.NewCond(locker) })
		original := sequence.receiveCallback
		sequence.receiveCallback = func(source TransferPath, key TransferKey, mode protocol.ProvideMode, recovery receiveRecoveryMode, path *IpPath, packet []byte) {
			if recovery == receiveRecoveryModeDedicatedTcpControl {
				header, _, err := persistNativePacketHeader(packet)
				zero := f.zero.Load()
				if err == nil && zero != nil && header.source == zero.destination && header.destination == zero.source &&
					header.sourcePort == zero.destinationPort && header.destinationPort == zero.sourcePort &&
					header.payload == 1 && header.seq == zero.ack-1 && header.ack == zero.seq && header.ackFlag && !header.syn && !header.fin && !header.rst {
					f.probeDo.Do(func() { f.probeSeen <- header })
					select {
					case <-f.releaseProbe:
					case <-sequence.ctx.Done():
					}
				}
			}
			original(source, key, mode, recovery, path, packet)
		}
	}
	natSettings.TcpBufferSettings.DialContextSettings = &DialContextSettings{DialContext: func(context.Context, string, string) (net.Conn, error) {
		if f.dials.Add(1) != 1 {
			return nil, errors.New("unexpected second upstream TCP connection")
		}
		return local, nil
	}}
	f.nat = NewLocalUserNat(ctx, "native persist custody", natSettings)
	providerSettings := DefaultRemoteUserNatProviderSettings()
	providerSettings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
	f.provider = NewRemoteUserNatProvider(f.receiver, f.nat, providerSettings)
	multiSettings := DefaultMultiClientSettings()
	multiSettings.Log = NewNoopLogger()
	multiSettings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
	multiSettings.LocalDialContextSettings = &DialContextSettings{DialContext: func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("unexpected local fallback")
	}}
	f.parent = NewRemoteUserNatMultiClient(ctx, &testingEmptyMultiClientGenerator{}, func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
		if header, _, err := persistNativePacketHeader(packet); err == nil {
			copyHeader := header
			if header.syn && header.ackFlag {
				f.synAck.CompareAndSwap(nil, &copyHeader)
			} else if header.payload != 0 {
				f.firstReturnData.CompareAndSwap(nil, &copyHeader)
			}
		}
		if n, err := f.tun.Write(packet); ctx.Err() == nil && (err != nil || n != len(packet)) {
			t.Errorf("actual return TUN write: n=%d err=%v", n, err)
		}
	}, 1, multiSettings)
	if !f.parent.settings.TcpCollapsePrevention || f.parent.ReliabilitySettings().TcpCollapseMaxHold != 0 {
		t.Fatal("test changed shipping TCP collapse policy")
	}
	f.selected = newPacketTransferTestChannel()
	f.selected.ctx, f.selected.log, f.selected.settings, f.selected.client = ctx, NewNoopLogger(), f.parent.settings, f.sender
	f.selected.args = &multiClientChannelArgs{Destination: RequireMultiHopId(f.receiver.ClientId())}
	f.selected.healthy = true
	f.selected.clientReceivePacketCallback = f.parent.clientReceivePacket
	f.sender.AddReceiveCallback(f.selected.clientReceive)
	f.bridgeStarted = true
	go f.runBridge()
	return f
}

// Takes one actual native packet and preserves the real metadata admission
// gate. Only pure ACK paths are inspected after the public consuming entry.
func (self *persistNativeFixture) offer(packet []byte, keepCopy bool) persistNativeOffer {
	header, path, err := persistNativePacketHeader(packet)
	if err != nil {
		MessagePoolReturn(packet)
		self.t.Error(err)
		return persistNativeOffer{}
	}
	if self.nativeOffers.Add(1) > 256 {
		MessagePoolReturn(packet)
		self.t.Error("native custody proof exceeded fixed packet bound")
		self.cancel()
		return persistNativeOffer{}
	}
	result := persistNativeOffer{header: header}
	if keepCopy {
		result.copy = bytes.Clone(packet)
	}
	var update *multiClientChannelUpdate
	var copiedPath IpPath
	if header.pureAck() {
		copiedPath = *path
		copiedPath.SourceIp, copiedPath.DestinationIp = bytes.Clone(path.SourceIp), bytes.Clone(path.DestinationIp)
		update, _, _ = self.parent.sendUpdate(path, flowPin{})
		update.stateLock.Lock()
		result.beforeEligible = update.canUpdateSequenceForClientWithLock(&parsedPacket{ipPath: path}, self.selected, true)
		result.beforeSeen, result.beforeWindow, result.beforeAck = update.sequenceAdmissionStateSeen, update.sequenceAdmissionWindow, update.sequenceAdmissionAck
		update.stateLock.Unlock()
	}
	beforeDrops := self.parent.TcpCollapseDropCount()
	accepted := []bool{true}
	started := time.Now()
	result.count = self.parent.SendNativePacketBatchWithResults(SourceId(self.sender.ClientId()), protocol.ProvideMode_Network, [][]byte{packet}, 5*time.Second, accepted)
	result.elapsed = time.Since(started)
	result.accepted = accepted[0]
	result.collapseDrops = self.parent.TcpCollapseDropCount() - beforeDrops
	if update != nil {
		update.stateLock.Lock()
		result.afterEligible = update.canUpdateSequenceForClientWithLock(&parsedPacket{ipPath: &copiedPath}, self.selected, true)
		result.afterSeen, result.afterWindow, result.afterAck = update.sequenceAdmissionStateSeen, update.sequenceAdmissionWindow, update.sequenceAdmissionAck
		update.stateLock.Unlock()
	}
	return result
}

// Prelude/recovery waits occur only in this fixture-owned read goroutine,
// never in Client callbacks or in the discriminating ready-only native call.
func (self *persistNativeFixture) runBridge() {
	defer close(self.bridgeDone)
	for {
		if !self.waitCapacity() {
			return
		}
		packet, err := self.tun.Read()
		if err != nil {
			return
		}
		header, path, err := persistNativePacketHeader(packet)
		if err != nil {
			MessagePoolReturn(packet)
			self.t.Error(err)
			return
		}
		if header.syn && !header.ackFlag {
			copyHeader := header
			self.syn.CompareAndSwap(nil, &copyHeader)
			update, _, _ := self.parent.sendUpdate(path, flowPin{})
			update.client.Store(self.selected)
			self.parent.bindClientFlow(update, self.selected)
		}
		firstZero := header.pureAck() && header.window == 0 && self.zero.Load() == nil
		if firstZero {
			copyHeader := header
			self.zero.Store(&copyHeader)
		}
		result := self.offer(packet, false)
		if firstZero {
			self.zeroOffered <- result
			select {
			case <-self.resumeBridge:
			case <-self.ctx.Done():
				return
			}
			continue
		}
		if result.accepted {
			if !self.waitSerialAck() {
				return
			}
			result.wire = self.sourceWire.Load()
		}
		if zero := self.zero.Load(); zero != nil && header.opens(*zero) {
			select {
			case self.regenerated <- result:
			default:
				self.t.Error("regenerated ACK observations exceeded fixed slots")
			}
		}
	}
}

// This fixture read owner may wait for published capacity. The real native
// discriminating offers deliberately do not call this helper before refusal.
func (self *persistNativeFixture) waitCapacity() bool {
	wire := self.sourceWire.Load()
	if wire == nil {
		return true
	}
	gate := &wire.sequence.resendCapacityUnavailable
	waiter := gate.queue()
	if waiter == nil {
		return true
	}
	for {
		select {
		case <-self.ctx.Done():
			gate.abandon(waiter)
			return false
		case <-waiter.ready:
			if gate.pass(waiter) {
				return true
			}
		}
	}
}

func (self *persistNativeFixture) waitSerialAck() bool {
	select {
	case receipt := <-self.serialAcks:
		wire := self.sourceWire.Load()
		if wire == nil || receipt.owner != wire.owner || receipt.generation != wire.generation || receipt.number != wire.number {
			self.t.Error("serialized native offer retired a different source generation")
		}
		return true
	case <-self.ctx.Done():
		return false
	}
}

func (self *persistNativeFixture) await(channel <-chan struct{}, label string) {
	self.t.Helper()
	select {
	case <-channel:
	case <-self.ctx.Done():
		self.t.Fatalf("%s: %v", label, self.ctx.Err())
	}
}

// Cleanup is registered before any constructors or child workers can fail.
func (self *persistNativeFixture) close() {
	self.closing.Store(true)
	self.probeReleaseDo.Do(func() { close(self.releaseProbe) })
	self.resumeDo.Do(func() { close(self.resumeBridge) })
	if claim := self.held.Swap(nil); claim != nil {
		claim.complete(false)
	}
	self.cancel()
	if self.connection != nil {
		_ = self.connection.Close()
	}
	if self.listener != nil {
		_ = self.listener.Close()
	}
	_ = self.local.Close()
	_ = self.origin.Close()
	if self.server != nil {
		_ = self.server.Close()
	}
	if self.tun != nil {
		_ = self.tun.Close()
	}
	if self.bridgeStarted {
		<-self.bridgeDone
	}
	<-self.serverDone
	self.serverConnections.Wait()
	self.handlers.Wait()
	if self.provider != nil {
		self.provider.Close()
	}
	if self.nat != nil {
		if err := self.nat.CloseAndWait(context.Background()); err != nil {
			self.t.Error(err)
		}
	}
	if self.parent != nil {
		if err := self.parent.CloseAndWait(context.Background()); err != nil {
			self.t.Error(err)
		}
	}
	for _, client := range []*Client{self.sender, self.receiver} {
		if client != nil {
			if err := client.CloseAndWait(context.Background()); err != nil {
				self.t.Error(err)
			}
			if used := client.settings.SendBufferSettings.ResendQueueBudget.UsedByteCount(); used != 0 {
				self.t.Errorf("joined native Transfer owner retained %d bytes", used)
			}
		}
	}
	for _, route := range []Route{self.forward, self.reverse} {
		for len(route) != 0 {
			MessagePoolReturn(<-route)
		}
	}
	if self.nat != nil && self.nat.settings.TcpBufferSettings.ReturnQueueBudget.UsedByteCount() != 0 {
		self.t.Error("joined provider retained inner TCP return bytes")
	}
	if self.tun != nil {
		self.tun.stack.Wait()
	}
}

// The absent-probe watchdog is not the sole state proof: actual native zero
// and reopen headers, public indexed refusal, exact held Transfer ownership,
// reader Cond.Wait and empty inner debt all precede it. OLD receives one
// explicitly captured real window ACK as a positive release before failure.
func checkPersistNativeReopen(t *testing.T, holdZero, cancelAfterRefusal bool) {
	t.Helper()
	MessagePoolReturn(MessagePoolGet(1))
	assertMessagePoolOwnership(t)
	f := newPersistNativeFixture(t, holdZero)
	connection, err := f.tun.DialContext(f.ctx, "tcp4", "198.51.100.19:80")
	if err != nil {
		t.Fatal(err)
	}
	f.connection = connection
	deadline, _ := f.ctx.Deadline()
	if err := connection.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	request := []byte("GET /asset HTTP/1.1\r\nHost: native-persist.example\r\n\r\n")
	if n, err := connection.Write(request); err != nil || n != len(request) {
		t.Fatalf("actual native HTTP write: n=%d err=%v", n, err)
	}
	var zeroOffer persistNativeOffer
	select {
	case zeroOffer = <-f.zeroOffered:
	case <-f.ctx.Done():
		t.Fatal("native receive window never closed", f.ctx.Err())
	}
	if !zeroOffer.accepted || zeroOffer.count != 1 || !zeroOffer.header.pureAck() || zeroOffer.header.window != 0 {
		t.Fatalf("zero-window ACK lacks actual reliable admission: %+v", zeroOffer)
	}
	zero := zeroOffer.header
	var parked zeroWindowWaitSnapshot
	observedWait := false
	for !observedWait || parked.window != 0 || parked.ack != zero.ack || parked.next != zero.ack || parked.retained != 0 || parked.chunks != 0 {
		select {
		case parked = <-f.waits:
			observedWait = true
		case <-f.ctx.Done():
			t.Fatal("real provider reader did not park fully-ACKed at native zero window", f.ctx.Err())
		}
	}
	if locker := f.waitLocker.Load(); locker == nil || locker.lost.Load() != 0 {
		t.Fatal("provider wait observations are incomplete")
	}
	wire := f.zeroWire.Load()
	if wire == nil {
		t.Fatal("zero-window ACK has no full actual Transfer identity")
	}
	if holdZero {
		f.await(f.claimReady, "real zero-window ACK delivery claim")
		select {
		case <-f.capacityReady:
		case <-f.ctx.Done():
			observation := func() persistNativeCapacityObservation {
				f.capacityStateLock.Lock()
				defer f.capacityStateLock.Unlock()
				return f.capacityState
			}()
			t.Fatalf("actual source Transfer capacity publication: %v; last_worker_state=%+v", f.ctx.Err(), observation)
		}
		published := func() persistNativeCapacityObservation {
			f.capacityStateLock.Lock()
			defer f.capacityStateLock.Unlock()
			return f.capacityPublished
		}()
		t.Logf("actual source Transfer capacity publication: %+v", published)
		if number, retained := wire.sequence.retainedAckSequenceNumber(wire.message); !retained || number != wire.number || f.held.Load() == nil {
			t.Fatal("held zero-window ACK lost its exact queued/detached retry owner")
		}
		select {
		case <-f.zeroAcked:
			t.Fatal("held zero-window ACK received premature genuine Transfer ACK")
		default:
		}
	} else {
		f.await(f.zeroAcked, "accepted zero-window ACK retirement")
		if !f.waitSerialAck() || !f.waitCapacity() {
			t.Fatal("accepted zero-window ACK capacity did not reopen")
		}
	}
	// Both stream edges come from actual native SYN/SYN-ACK packets, rather
	// than assuming the provider's independently selected initial sequence.
	syn, synAck, firstData := f.syn.Load(), f.synAck.Load(), f.firstReturnData.Load()
	if syn == nil || synAck == nil || firstData == nil || firstData.ack != zero.seq ||
		zero.seq != syn.seq+1+uint32(len(request)) || synAck.ack != syn.seq+1 ||
		synAck.source != zero.destination || synAck.destination != zero.source || synAck.sourcePort != zero.destinationPort || synAck.destinationPort != zero.sourcePort {
		t.Fatal("actual SYN/SYN-ACK did not pin the exact HTTP stream edges")
	}
	prefixCount := int(uint32(zero.ack - (synAck.seq + 1)))
	if prefixCount < 1024 || prefixCount > 16*1024 {
		t.Fatalf("actual zero-window prefix is outside fixed receiver bound: %d", prefixCount)
	}
	prefix := make([]byte, prefixCount)
	if _, err := io.ReadFull(connection, prefix); err != nil {
		t.Fatal("actual native application drain", err)
	}
	// The bridge is parked. Read returned after synchronous native window
	// publication; drain ALL prior updates through the real consuming entry.
	var refused persistNativeOffer
	openOffers, acceptedOpen := 0, 0
	for drained := 0; ; drained++ {
		if drained == 64 {
			t.Fatal("native published-update drain exceeded queue bound")
		}
		packet := f.tun.ep.Read()
		if packet == nil {
			break
		}
		pooled := messagePoolCopyPacketBuffer(packet)
		packet.DecRef()
		if !holdZero && !f.waitCapacity() {
			MessagePoolReturn(pooled)
			t.Fatal("positive native reopen had no published source capacity")
		}
		offer := f.offer(pooled, true)
		if !offer.header.opens(zero) {
			if offer.header.pureAck() && offer.header.window == 0 && offer.header.ack == zero.ack {
				if offer.accepted && !f.waitSerialAck() {
					t.Fatal("published duplicate ACK did not retire")
				}
				continue
			}
			if !holdZero && offer.accepted {
				if !f.waitSerialAck() {
					t.Fatal("positive native follow-up did not retire")
				}
				continue
			}
			t.Fatalf("unexpected native traffic during exclusive reopen drain: %+v", offer.header)
		}
		openOffers++
		if holdZero {
			if offer.accepted || offer.count != 0 || !offer.beforeEligible || !offer.afterEligible || offer.collapseDrops != 0 ||
				!offer.beforeSeen || !offer.afterSeen || offer.beforeWindow != 0 || offer.afterWindow != 0 || offer.beforeAck != zero.ack || offer.afterAck != zero.ack {
				t.Fatalf("actual native reopen was not an unowned readiness refusal with unchanged metadata: %+v", offer)
			}
			if refused.copy == nil {
				refused = offer
			}
		} else if offer.accepted && offer.count == 1 {
			acceptedOpen++
			if !f.waitSerialAck() || !f.waitCapacity() {
				t.Fatal("accepted reopen ACK capacity did not drain")
			}
		}
	}
	if openOffers == 0 {
		t.Fatal("actual application drain produced no native window-reopen ACK")
	}
	if holdZero && f.openWire.Load() != nil {
		t.Fatal("a refused native reopen ACK unexpectedly acquired Transfer custody")
	}
	if holdZero {
		sequence := f.providerSequence.Load()
		sequence.mutex.Lock()
		stillParked := sequence.receiveWindowSize == 0 && sequence.receiveSeqAck == zero.ack && sequence.receiveSeq == zero.ack &&
			sequence.returnByteCount == 0 && sequence.returnHead == len(sequence.returnChunks)
		sequence.mutex.Unlock()
		if !stillParked {
			t.Fatal("refused native ACK changed the real provider's fully-ACKed zero-window state")
		}
	}
	if cancelAfterRefusal {
		if refused.copy == nil || f.held.Load() == nil {
			t.Fatal("cancel control lacks actual refused ACK and held original custody")
		}
		sequence := f.providerSequence.Load()
		sequence.Close()
		f.await(sequence.retirementDone, "canceled provider TCP flow retirement")
		if f.ctx.Err() != nil || f.parent.ctx.Err() != nil || f.nat.ctx.Err() != nil || f.sender.ctx.Err() != nil || f.receiver.ctx.Err() != nil {
			t.Fatal("canceling one parked TCP flow canceled shared parents")
		}
		f.held.Swap(nil).complete(true)
		f.await(f.zeroAcked, "canceled flow's already-applied ACK custody release")
		if used := f.nat.settings.TcpBufferSettings.ReturnQueueBudget.UsedByteCount(); used != 0 {
			t.Fatalf("joined canceled TCP flow retained %d inner return bytes", used)
		}
		return // registered cleanup joins the still-live shared parents and pool roots
	}
	if holdZero {
		claim := f.held.Swap(nil)
		if claim == nil {
			t.Fatal("original zero-window custody disappeared before explicit release")
		}
		claim.complete(true)
		f.await(f.zeroAcked, "released zero-window ACK genuine peer feedback")
	} else if acceptedOpen == 0 {
		t.Fatal("positive control admitted no actual window-reopen ACK")
	}
	// The original zero-window receipt is already handled, not a retirement
	// token for a future probe response. Recovery offers then serialize normally.
	for len(f.serialAcks) != 0 {
		<-f.serialAcks
	}
	f.resumeDo.Do(func() { close(f.resumeBridge) })
	f.probeReleaseDo.Do(func() { close(f.releaseProbe) })
	missingProbe := false
	if holdZero {
		watchdog := time.NewTimer(3 * time.Second)
		select {
		case probe := <-f.probeSeen:
			if probe.seq != zero.ack-1 || probe.payload != 1 {
				t.Fatal("persist used a new stream byte instead of the already-ACKed frontier")
			}
		case <-watchdog.C:
			missingProbe = true
		case <-f.ctx.Done():
			t.Fatal("persist chain setup expired", f.ctx.Err())
		}
		watchdog.Stop()
		if missingProbe {
			// Positive release uses the exact real source ACK captured before its
			// refused consuming offer, never a fabricated inner acknowledgement.
			if offer := f.offer(MessagePoolCopy(refused.copy), false); !offer.accepted || offer.count != 1 {
				t.Fatalf("equivalent actual native window ACK could not release healthy reader: %+v", offer)
			}
			f.waitSerialAck()
		} else {
			select {
			case offer := <-f.regenerated:
				if !offer.header.opens(zero) || !offer.accepted || offer.count != 1 || !offer.beforeEligible || offer.collapseDrops != 0 ||
					offer.wire == nil || offer.wire.header != offer.header || f.openWire.Load() != offer.wire {
					t.Fatalf("actual persist response was not admitted by the unchanged Multi window gate: %+v", offer)
				}
			case <-f.ctx.Done():
				t.Fatal("actual gVisor persist response never reopened provider", f.ctx.Err())
			}
		}
	}
	reader := bufio.NewReader(io.MultiReader(bytes.NewReader(prefix), connection))
	readResponse := func(expected []byte) {
		t.Helper()
		response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
		if err != nil {
			t.Fatal("same actual HTTP stream response", err)
		}
		body, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if response.StatusCode != http.StatusOK || response.ContentLength != int64(len(expected)) || !bytes.Equal(body, expected) || readErr != nil || closeErr != nil {
			t.Fatalf("native HTTP corruption/loss: status=%d bytes=%d read=%v close=%v", response.StatusCode, len(body), readErr, closeErr)
		}
	}
	readResponse(f.body)
	reuse := []byte("GET /reuse HTTP/1.1\r\nHost: native-persist.example\r\n\r\n")
	if n, err := connection.Write(reuse); err != nil || n != len(reuse) {
		t.Fatalf("same native connection second request: n=%d err=%v", n, err)
	}
	readResponse([]byte("reuse"))
	if f.dials.Load() != 1 || f.tun.LinkStats().OutboundDropCount != 0 {
		t.Fatal("native proof reconnected or lost an unobserved source packet")
	}
	t.Logf("actual native persist custody: held=%t zero_ack=%d exact_inner_empty=true actual_open_offers=%d accepted_control=%d native_offers=%d refused_offer_elapsed=%s same_http_reused=true", holdZero, zero.ack, openOffers, acceptedOpen, f.nativeOffers.Load(), refused.elapsed)
	if missingProbe {
		t.Fatalf("TCP_NATIVE_REFUSED_REOPEN_NO_PERSIST: actual native reopen ACK was refused before Transfer custody; provider reader parked at window0 ack=next=%d with empty return queue; equivalent captured ACK alone resumed exact HTTP", zero.ack)
	}
}
