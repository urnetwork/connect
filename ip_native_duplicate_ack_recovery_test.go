// Real native/Multi/Transfer/provider delivery checks that TCP collapse
// preserves the duplicate acknowledgements needed for fast return repair.
package connect

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The three unchanged ACKs follow actual post-Transfer inner loss, with a fully
// committed handshake ACK already covering their scalar sequence/window state.
func TestNativeDuplicateAckAfterInnerLossReachesProviderFastRecovery(t *testing.T) {
	checkNativeDuplicateAckRecovery(t, true, false)
}

// A real window change traverses the same public entry and provider ACK handler.
func TestNativeWindowChangingDuplicateAcksRepairInnerLoss(t *testing.T) {
	checkNativeDuplicateAckRecovery(t, true, true)
}

// An advancing cumulative ACK releases healthy return custody without replay.
func TestNativeHealthyReturnAckReleasesWithoutFastReplay(t *testing.T) {
	checkNativeDuplicateAckRecovery(t, false, false)
}

type nativeDuplicateAckConsumer struct {
	stateLock sync.Mutex
	base      uint32
	next      uint32
	seenSyn   bool
	loss      bool
	dropped   bool
	packets   int
	replays   int
	pending   map[uint32]string
	received  string
}

// Bytes are copied only into this bounded synthetic inner consumer. A discarded
// segment still returns through the real Multi callback and receives Transfer ACK.
func (self *nativeDuplicateAckConsumer) receive(t *testing.T, packet []byte) {
	path, payload, err := ParseIpPathWithPayload(packet)
	if err != nil || path == nil || path.Protocol != IpProtocolTcp || !path.Ack {
		t.Errorf("unexpected native return packet: %v", err)
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if path.Syn {
		if self.seenSyn || len(payload) != 0 {
			t.Error("native return handshake was repeated or carried unexpected data")
			return
		}
		self.base, self.next, self.seenSyn = path.SequenceNumber+1, path.SequenceNumber+1, true
		return
	}
	if len(payload) == 0 {
		return
	}
	self.packets++
	if self.packets > 16 || len(payload) != 4 || !self.seenSyn || path.Fin || path.Rst {
		t.Error("return flight exceeded its exact four-byte segment fixture")
		return
	}
	if self.loss && !self.dropped && path.SequenceNumber == self.base {
		self.dropped = true
		return
	}
	if self.dropped && path.SequenceNumber == self.base {
		self.replays++
	}
	if int32(path.SequenceNumber-self.next) >= 0 {
		self.pending[path.SequenceNumber] = string(payload)
	}
	for {
		payload, exists := self.pending[self.next]
		if !exists {
			break
		}
		delete(self.pending, self.next)
		self.received += payload
		self.next += uint32(len(payload))
	}
}

type nativeDuplicateAckFixture struct {
	t                *testing.T
	ctx              context.Context
	cancel           context.CancelFunc
	closeDo          sync.Once
	local, origin    net.Conn
	sender, receiver *Client
	parent           *RemoteUserNatMultiClient
	provider         *RemoteUserNatProvider
	nat              *LocalUserNat
	selected         *multiClientChannel
	update           *multiClientChannelUpdate
	providerSequence atomic.Pointer[TcpSequence]
	forward, reverse Route
	consumer         nativeDuplicateAckConsumer
	senderTerminals  atomic.Int64
	returnTerminals  atomic.Int64
	initial          uint32
	window           uint16
}

// Constructors keep production collapse and original recovery timers. All routes
// are in memory and every dial is either the one owned pipe or an explicit error.
func newNativeDuplicateAckFixture(t *testing.T, loss bool) *nativeDuplicateAckFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	local, origin := net.Pipe()
	f := &nativeDuplicateAckFixture{t: t, ctx: ctx, cancel: cancel, local: local, origin: origin,
		forward: make(Route, 16), reverse: make(Route, 16), initial: 1000, window: 4096,
		consumer: nativeDuplicateAckConsumer{loss: loss, pending: make(map[uint32]string)}}
	t.Cleanup(f.close)
	constructed := false
	defer func() {
		if !constructed {
			f.close()
		}
	}()
	newClient := func(terminals *atomic.Int64) *Client {
		settings := closeWaitClientSettings()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		settings.SendBufferSettings.ResendQueueBudget = NewTransferMemoryBudget(128 * 1024)
		settings.SendBufferSettings.ResendQueueRetainedByteAccounting = true
		settings.ReceiveBufferSettings.AckCompressTimeout = 0
		settings.SendBufferSettings.SendPackLifecycleObserver = func(event SendPackLifecycleObservation) {
			if event.Phase != SendPackLifecyclePhaseTerminal || event.MessageType != protocol.MessageType_IpIpPacketToProvider && event.MessageType != protocol.MessageType_IpIpPacketFromProvider {
				return
			}
			if event.Err != nil {
				if ctx.Err() == nil {
					t.Errorf("native packet Transfer completion: %v", event.Err)
				}
				return
			}
			terminals.Add(1)
		}
		return NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	}
	f.sender, f.receiver = newClient(&f.senderTerminals), newClient(&f.returnTerminals)
	f.sender.ContractManager().AddNoContractPeer(f.receiver.ClientId())
	f.receiver.ContractManager().AddNoContractPeer(f.sender.ClientId())
	f.sender.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(f.receiver.ClientId())), []Route{f.forward})
	f.receiver.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{f.forward})
	f.receiver.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(f.sender.ClientId())), []Route{f.reverse})
	f.sender.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{f.reverse})
	natSettings := DefaultLocalUserNatSettingsWithBufferSize(16)
	natSettings.Log = NewNoopLogger()
	natSettings.MemoryBudget = NewTransferMemoryBudget(2 * 1024 * 1024)
	natSettings.TcpBufferSettings.Mtu = 44
	natSettings.TcpBufferSettings.ReadBufferByteCount = 16
	natSettings.TcpBufferSettings.AckCompressTimeout = 0
	natSettings.TcpBufferSettings.beforeSequenceRunWithStateForTest = func(sequence *TcpSequence) {
		if !f.providerSequence.CompareAndSwap(nil, sequence) {
			t.Error("fixture constructed more than one provider TCP flow")
		}
	}
	var dialed atomic.Bool
	natSettings.TcpBufferSettings.DialContextSettings = &DialContextSettings{DialContext: func(context.Context, string, string) (net.Conn, error) {
		if !dialed.CompareAndSwap(false, true) {
			return nil, errors.New("unexpected second provider dial")
		}
		return local, nil
	}}
	f.nat = NewLocalUserNat(ctx, "native duplicate ACK recovery", natSettings)
	providerSettings := DefaultRemoteUserNatProviderSettings()
	providerSettings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
	f.provider = NewRemoteUserNatProvider(f.receiver, f.nat, providerSettings)
	multiSettings := DefaultMultiClientSettings()
	multiSettings.Log = NewNoopLogger()
	multiSettings.SecurityPolicyGenerator = DisableSecurityPolicyWithStats
	multiSettings.LocalDialContextSettings = &DialContextSettings{DialContext: func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("unexpected local fallback dial")
	}}
	f.parent = NewRemoteUserNatMultiClient(ctx, &testingEmptyMultiClientGenerator{}, func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
		f.consumer.receive(t, packet)
	}, protocol.ProvideMode_Network, multiSettings)
	if !f.parent.settings.TcpCollapsePrevention || f.parent.ReliabilitySettings().TcpCollapseMaxHold != 0 {
		t.Fatal("fixture must preserve default persistent TCP collapse")
	}
	syn := groupRecoveryPacket(f.initial, 0, f.window, tcpFlagSyn, nil)
	path, err := ParseIpPath(syn)
	if err != nil {
		t.Fatal(err)
	}
	f.update, _, _ = f.parent.sendUpdate(path, flowPin{})
	f.selected = newPacketTransferTestChannel()
	f.selected.ctx, f.selected.log, f.selected.settings, f.selected.client = ctx, NewNoopLogger(), f.parent.settings, f.sender
	f.selected.healthy = true
	f.selected.args = &multiClientChannelArgs{Destination: RequireMultiHopId(f.receiver.ClientId())}
	f.selected.clientReceivePacketCallback = f.parent.clientReceivePacket
	f.update.client.Store(f.selected)
	f.parent.bindClientFlow(f.update, f.selected)
	f.sender.AddReceiveCallback(f.selected.clientReceive)
	if !f.offer(syn) {
		t.Fatal("actual native SYN was refused")
	}
	synctest.Wait()
	f.consumer.stateLock.Lock()
	base, seen := f.consumer.base, f.consumer.seenSyn
	f.consumer.stateLock.Unlock()
	if !seen || f.providerSequence.Load() == nil || f.senderTerminals.Load() != 1 {
		t.Fatal("actual native SYN did not finish provider admission and return handshake")
	}
	if !f.offer(groupRecoveryPacket(f.initial+1, base, f.window, tcpFlagAck, nil)) {
		t.Fatal("actual native handshake ACK was refused")
	}
	synctest.Wait()
	f.update.stateLock.Lock()
	covered := f.update.sequenceAckPositionSeen && f.update.sequenceAckPosition == f.initial+1 &&
		f.update.sequenceAdmissionAck == base && f.update.sequenceAdmissionWindow == f.window
	f.update.stateLock.Unlock()
	if !covered || f.senderTerminals.Load() != 2 {
		t.Fatal("handshake ACK did not reach final provider admission and commit its original control coverage")
	}
	constructed = true
	return f
}

// The consuming public native entry owns every packet, including refusals.
func (self *nativeDuplicateAckFixture) offer(template []byte) bool {
	accepted := []bool{false}
	count := self.parent.SendNativePacketBatchWithResults(SourceId(self.sender.ClientId()), protocol.ProvideMode_Network,
		[][]byte{MessagePoolCopy(template)}, 5*time.Second, accepted)
	if count != 0 && count != 1 || (count == 1) != accepted[0] {
		self.t.Error("native result lost exact packet membership")
	}
	return count == 1 && accepted[0]
}

// Snapshot only after a real quiescence barrier; no timer advance proves fast
// repair. All three duplicates must reach the actual provider's pure-ACK path.
func checkNativeDuplicateAckRecovery(t *testing.T, loss, changeWindow bool) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newNativeDuplicateAckFixture(t, loss)
		defer f.close()
		want := "abcdefghijklmnop"
		if n, err := f.origin.Write([]byte(want)); err != nil || n != len(want) {
			t.Fatalf("actual origin write: n=%d err=%v", n, err)
		}
		synctest.Wait()
		sequence := f.providerSequence.Load()
		f.consumer.stateLock.Lock()
		base, ack, packets, dropped := f.consumer.base, f.consumer.next, f.consumer.packets, f.consumer.dropped
		f.consumer.stateLock.Unlock()
		sequence.mutex.Lock()
		retained, attempts := sequence.returnByteCount, sequence.returnAttempts
		sequence.mutex.Unlock()
		queued, queuedBytes, _ := f.receiver.ResendQueueSize(f.sender.ClientId(), MultiHopId{}, false, false)
		if packets != 4 || dropped != loss || retained == 0 || attempts != 0 || f.returnTerminals.Load() < 2 || queued != 0 || queuedBytes != 0 {
			t.Fatalf("post-Transfer flight premise: packets=%d dropped=%t retained=%d attempts=%d return_terminals=%d transfer_retained=%d/%d", packets, dropped, retained, attempts, f.returnTerminals.Load(), queued, queuedBytes)
		}
		if loss && ack != base || !loss && ack != base+uint32(len(want)) {
			t.Fatal("synthetic inner consumer did not retain the exact expected cumulative frontier")
		}
		before := time.Now()
		drops := f.parent.TcpCollapseDropCount()
		beforeTerminals := f.senderTerminals.Load()
		accepted := 0
		count := 1
		if loss {
			count = 3
		}
		for index := 0; index < count; index++ {
			window := f.window
			if changeWindow {
				window += uint16(index + 1)
			}
			if f.offer(groupRecoveryPacket(f.initial+1, ack, window, tcpFlagAck, nil)) {
				accepted++
			}
			synctest.Wait()
		}
		sequence.mutex.Lock()
		duplicates, attempts, remaining := sequence.returnDuplicateAcks, sequence.returnAttempts, sequence.returnByteCount
		sequence.mutex.Unlock()
		f.consumer.stateLock.Lock()
		replays, received := f.consumer.replays, f.consumer.received
		f.consumer.stateLock.Unlock()
		terminalDelta := f.senderTerminals.Load() - beforeTerminals
		elapsed := time.Since(before)
		if loss {
			if accepted != 3 || terminalDelta != 3 || duplicates < 3 || attempts != 1 || replays != 1 || received != want || elapsed != 0 {
				t.Fatalf("NATIVE_DUPLICATE_ACK_LOST_FAST_RECOVERY: accepted=%d provider_terminals=%d duplicate_acks=%d attempts=%d replays=%d received=%q elapsed=%s collapse_drops=%d", accepted, terminalDelta, duplicates, attempts, replays, received, elapsed, f.parent.TcpCollapseDropCount()-drops)
			}
			if !f.offer(groupRecoveryPacket(f.initial+1, base+uint32(len(want)), f.window, tcpFlagAck, nil)) {
				t.Fatal("repaired flight's cumulative native ACK was refused")
			}
			synctest.Wait()
			sequence.mutex.Lock()
			remaining = sequence.returnByteCount
			sequence.mutex.Unlock()
			if remaining != 0 {
				t.Fatal("repaired native flight kept provider return custody after cumulative ACK")
			}
		} else if accepted != 1 || terminalDelta != 1 || attempts != 0 || replays != 0 || received != want || remaining != 0 || elapsed != 0 {
			t.Fatalf("healthy native ACK changed return custody: accepted=%d terminals=%d attempts=%d replays=%d received=%q retained=%d elapsed=%s", accepted, terminalDelta, attempts, replays, received, remaining, elapsed)
		}
	})
}

// Cancel the common owner, unblock the only pipe, and join all real workers
// before draining routes. Caller cleanup also handles a partial constructor.
func (self *nativeDuplicateAckFixture) close() {
	self.closeDo.Do(func() {
		self.cancel()
		self.origin.Close()
		self.local.Close()
		joinCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if self.provider != nil {
			self.provider.Close()
		}
		if self.nat != nil {
			if err := self.nat.CloseAndWait(joinCtx); err != nil {
				self.t.Error(err)
			}
		}
		if self.parent != nil {
			if err := self.parent.CloseAndWait(joinCtx); err != nil {
				self.t.Error(err)
			}
		}
		for _, client := range []*Client{self.sender, self.receiver} {
			if client != nil {
				if err := client.CloseAndWait(joinCtx); err != nil {
					self.t.Error(err)
				}
				if budget := client.settings.SendBufferSettings.ResendQueueBudget; budget != nil && budget.UsedByteCount() != 0 {
					self.t.Errorf("joined native client retained %d resend bytes", budget.UsedByteCount())
				}
			}
		}
		for _, route := range []Route{self.forward, self.reverse} {
			for len(route) != 0 {
				MessagePoolReturn(<-route)
			}
		}
		if self.nat != nil && self.nat.settings.MemoryBudget.UsedByteCount() != 0 {
			self.t.Errorf("joined NAT/provider retained %d bytes", self.nat.settings.MemoryBudget.UsedByteCount())
		}
	})
}
