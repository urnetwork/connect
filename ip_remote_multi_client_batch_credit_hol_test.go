// Native batch isolation uses real retained Transfer bytes and exact peer ACKs.
// The two causal admission assertions are unchanged; controls also require
// ready whole-group wire progress without weakening reliable retention.
package connect

import (
	"bytes"
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Only test-owned scalar observations survive callbacks. The existing fixture
// owns packet witnesses, selected-client construction and client shutdown.
type nativeBatchCreditFixture struct {
	t                   *testing.T
	base                *groupDispositionQueueFixture
	source              *SendSequence
	udpUpdate           *multiClientChannelUpdate
	udpPath             *IpPath
	seedAck             protocol.Ack
	seed                []byte
	tcp                 []byte
	udp                 []byte
	capacityRelease     chan struct{}
	capacityReleaseOnce sync.Once
	workers             sync.WaitGroup
	udpStarted          atomic.Int64
	udpAdmitted         atomic.Int64
	fastAttempted       atomic.Int64
	fastWritten         atomic.Int64
	stateLock           sync.Mutex
	phases              [16]SendPackLifecyclePhase
	phaseInvalid        bool
	otherRoute          Route
}

// One real original fills the deliberately small protocol window. The source
// publishes its closed gate before the barrier; no capacity flag is assigned.
func newNativeBatchCreditFixture(t *testing.T) *nativeBatchCreditFixture {
	t.Helper()
	self := &nativeBatchCreditFixture{
		t: t, capacityRelease: make(chan struct{}),
		udpPath: &IpPath{Version: 4, Protocol: IpProtocolUdp,
			SourceIp: net.IPv4(192, 0, 2, 91), DestinationIp: net.IPv4(198, 51, 100, 92),
			SourcePort: 44001, DestinationPort: 443},
	}
	capacityEntered := make(chan sendSequenceId, 1)
	var capacityOnce sync.Once
	self.base = newGroupDispositionQueueFixture(t, nil, func(settings *ClientSettings) {
		// The unbudgeted queue's documented first-item allowance lets one
		// actual original reach retention, then closes subsequent admission.
		settings.SendBufferSettings.ResendQueueMaxByteCount = 1
		settings.SendBufferSettings.beforeResendCapacityWaitForTest = func(id sendSequenceId) {
			if self.base == nil || id.Destination != self.base.peer {
				return
			}
			capacityOnce.Do(func() {
				capacityEntered <- id
				<-self.capacityRelease
			})
		}
		previous := settings.SendBufferSettings.SendPackLifecycleObserver
		settings.SendBufferSettings.SendPackLifecycleObserver = func(event SendPackLifecycleObservation) {
			previous(event)
			if event.DestinationId == ControlId {
				return
			}
			self.stateLock.Lock()
			if event.Token == 0 || uint64(len(self.phases)) <= event.Token {
				self.phaseInvalid = true
			} else if event.Phase != self.phases[event.Token]+1 {
				self.phaseInvalid = true
			} else {
				self.phases[event.Token] = event.Phase
			}
			self.stateLock.Unlock()
			if !event.AckRequired && event.Phase == SendPackLifecyclePhaseStarted {
				self.udpStarted.Add(1)
			}
		}
		settings.SendBufferSettings.afterGroupAdmissionForTest = func(target sendGroupAdmissionTarget) {
			if group, ok := target.(*parsedPacketGroup); ok && group.ipPath.Protocol == IpProtocolUdp {
				self.udpAdmitted.Add(1)
			}
		}
		settings.SendBufferSettings.afterNoAckFastPathForTest = func(_ sendSequenceId, attempted, written bool, _ time.Duration) {
			if attempted {
				self.fastAttempted.Add(1)
			}
			if written {
				self.fastWritten.Add(1)
			}
		}
	})
	self.base.selected.settings = self.base.parent.settings
	self.udpUpdate = newMultiClientChannelUpdate(self.base.parent.ctx, self.udpPath)
	self.udpUpdate.client.Store(self.base.selected)
	// The reused helper is intentionally single-flow. Give the two real
	// five-tuples independent collapse/admission state before any offer.
	self.base.parent.sendClientPathForTest = func(path *IpPath, _ flowPin, callback func(*multiClientChannelUpdate, *multiClientChannel)) {
		update := self.base.update
		if path.Protocol == IpProtocolUdp {
			update = self.udpUpdate
		}
		update.ipPath = path
		callback(update, update.client.Load())
	}
	t.Cleanup(func() {
		self.base.cancel()
		self.releaseCapacity()
		self.workers.Wait()
		if err := self.base.closeClient(context.Background()); err != nil {
			t.Errorf("join native batch source: %v", err)
		}
		self.udpUpdate.Close()
		for len(self.otherRoute) != 0 {
			MessagePoolReturn(<-self.otherRoute)
		}
		self.stateLock.Lock()
		invalid := self.phaseInvalid
		for _, phase := range self.phases {
			invalid = invalid || phase != 0 && phase != SendPackLifecyclePhaseTerminal
		}
		self.stateLock.Unlock()
		if invalid {
			t.Error("original lifecycle was missing, duplicated, overflowed or left unjoined")
		}
		if self.source != nil {
			if count, size := self.source.resendQueue.QueueSize(); count != 0 || size != 0 {
				t.Error("joined source retained reliable ownership")
			}
		}
	})
	self.seed = groupRecoveryPacket(100, 500, 64, tcpFlagAck, []byte{0x11})
	self.tcp = groupRecoveryPacket(101, 500, 64, tcpFlagAck, []byte{0x22})
	self.udp = ipOosUdpPacket(self.udpPath, []byte{0x33})
	if !self.base.selected.ipPacketTransferAckRequired(groupRecoveryParsed(t, self.seed).ipPath) ||
		self.base.selected.ipPacketTransferAckRequired(self.udpPath) {
		t.Fatal("fixture must retain default TCP acknowledgement and bound UDP NoAck policy")
	}
	if self.base.offer(self.seed) != 1 {
		t.Fatal("seed original was not admitted through the public native batch")
	}
	self.base.requireParked()
	if self.base.update.sequenceClaims == nil || self.base.update.sequenceClaims.collapseSource == nil {
		t.Fatal("public seed admission did not publish its actual source owner")
	}
	self.source = self.base.update.sequenceClaims.collapseSource
	self.base.unpark()
	synctest.Wait()
	select {
	case id := <-capacityEntered:
		if id != self.source.id() {
			t.Fatal("capacity barrier belongs to another source")
		}
	default:
		t.Fatal("real retained original did not close its capacity gate")
	}
	count, retainedBytes := self.source.resendQueue.QueueSize()
	if !self.source.resendCapacityUnavailable.Load() || count != 1 ||
		retainedBytes <= 1 || self.source.noAckFastPath.Load() == nil || len(self.base.route) != 1 {
		t.Fatal("seed did not establish real retention, a ready scalar snapshot and one written frame")
	}
	wire := <-self.base.route
	pack := decodeSendPackLifecycleWirePack(t, wire)
	if pack.Nack || len(pack.Frames) != 1 || !bytes.Equal(pack.Frames[0].MessageBytes, self.seed) {
		MessagePoolReturn(wire)
		t.Fatal("retained seed wire differs from its reliable original")
	}
	self.seedAck = protocol.Ack{
		MessageId:  append([]byte(nil), pack.MessageId...),
		SequenceId: append([]byte(nil), pack.SequenceId...),
	}
	MessagePoolReturn(wire)
	return self
}

// Cleanup and normal completion both release the same source barrier once.
func (self *nativeBatchCreditFixture) releaseCapacity() {
	self.capacityReleaseOnce.Do(func() { close(self.capacityRelease) })
}

// The exact decoded original supplies the acknowledgement identity. Public
// offer success and lifecycle callbacks never manufacture delivery credit.
func (self *nativeBatchCreditFixture) releasePeerCredit() {
	self.t.Helper()
	if !self.base.client.sendBuffer.Ack(self.base.peer, &self.seedAck, 0) {
		self.t.Fatal("withheld exact peer acknowledgement was refused")
	}
	self.releaseCapacity()
	synctest.Wait()
}

// Creates caller owners before launching a worker, so witness bookkeeping is
// never shared with an asynchronous producer. Both batch entries consume them.
func (self *nativeBatchCreditFixture) ownPackets(templates ...[]byte) [][]byte {
	packets := make([][]byte, len(templates))
	for index, template := range templates {
		packets[index] = MessagePoolCopy(template)
		self.base.witnesses = append(self.base.witnesses, MessagePoolShareReadOnly(packets[index]))
	}
	return packets
}

// A compact result is published only after the borrowed acceptance slice is
// no longer in use. The mux reports a count, not per-position disposition.
type nativeBatchCreditResult struct {
	count    int
	accepted uint64
}

// Quiescence is not completion: require the buffered result explicitly, then
// fail through the existing cancel/release/join cleanup if progress is missing.
func (self *nativeBatchCreditFixture) completedBatch(done <-chan nativeBatchCreditResult) nativeBatchCreditResult {
	self.t.Helper()
	synctest.Wait()
	select {
	case result := <-done:
		return result
	default:
		self.t.Fatal("batch did not complete after exact peer acknowledgement")
		return nativeBatchCreditResult{}
	}
}

// The send-only mux uses its real batch implementation and ordinary upstream
// wiring, without constructing an unrelated TUN receive pump.
func (self *nativeBatchCreditFixture) startBatch(mux bool, timeout time.Duration, templates ...[]byte) <-chan nativeBatchCreditResult {
	packets := self.ownPackets(templates...)
	result := make(chan nativeBatchCreditResult, 1)
	self.workers.Add(1)
	go func() {
		defer self.workers.Done()
		observation := nativeBatchCreditResult{}
		if mux {
			entry := &IpMux{}
			entry.SetUpstream(self.base.parent.SendPacket)
			entry.setUpstreamGroupSend(self.base.parent.sendPacketGroup)
			observation.count = entry.SendPacketBatch(self.base.source, protocol.ProvideMode_Network, packets, timeout)
		} else {
			accepted := make([]bool, len(packets))
			observation.count = self.base.parent.SendPacketBatchWithResults(
				self.base.source, protocol.ProvideMode_Network, packets, timeout, accepted)
			for index, value := range accepted {
				if value {
					observation.accepted |= uint64(1) << uint(index)
				}
			}
		}
		result <- observation
	}()
	return result
}

// Exact decoded bytes identify every expected original; only reliable frames
// receive peer ACKs. Each bounded round follows actual capacity progress.
func (self *nativeBatchCreditFixture) finishPackets(templates ...[]byte) {
	self.t.Helper()
	seen := make([]bool, len(templates))
	for range len(templates) {
		synctest.Wait()
		if len(self.base.route) == 0 {
			self.t.Fatal("expected source frame did not reach the ready route")
		}
		wire := <-self.base.route
		pack := decodeSendPackLifecycleWirePack(self.t, wire)
		if len(pack.Frames) != 1 {
			MessagePoolReturn(wire)
			self.t.Fatal("fixture unexpectedly combined unrelated logical groups")
		}
		index := -1
		for candidate, template := range templates {
			if !seen[candidate] && bytes.Equal(pack.Frames[0].MessageBytes, template) {
				index = candidate
				break
			}
		}
		if index < 0 {
			MessagePoolReturn(wire)
			self.t.Fatal("route duplicated an original or changed its bytes")
		}
		seen[index] = true
		isUdp := templates[index][9] == byte(ipProtocolNumberUdp)
		if pack.Nack != isUdp {
			MessagePoolReturn(wire)
			self.t.Fatal("TCP reliability or UDP NoAck policy changed on the actual wire")
		}
		if !pack.Nack && !self.base.client.sendBuffer.Ack(self.base.peer,
			&protocol.Ack{MessageId: pack.MessageId, SequenceId: pack.SequenceId}, 0) {
			MessagePoolReturn(wire)
			self.t.Fatal("actual reliable successor acknowledgement was refused")
		}
		MessagePoolReturn(wire)
	}
	synctest.Wait()
	if len(self.base.route) != 0 {
		self.t.Fatal("unexpected extra physical frame")
	}
}

// A desired isolation assertion, deliberately separate from the passing
// characterization below. Both public entry points share the same premise.
func testNativeBatchTcpFirstIsolation(t *testing.T, mux, requireIsolation bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newNativeBatchCreditFixture(t)
		done := f.startBatch(mux, -1, f.tcp, f.udp)
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("TCP-first call bypassed its retained-credit barrier")
		default:
		}
		if f.base.started.Load() != 2 || f.udpStarted.Load() != 0 || f.udpAdmitted.Load() != 0 {
			if !requireIsolation {
				t.Fatal("current serial admission characterization changed")
			}
		}
		udpStarted, udpAdmitted := f.udpStarted.Load(), f.udpAdmitted.Load()
		f.releasePeerCredit()
		result := f.completedBatch(done)
		if result.count != 2 || !mux && result.accepted != 3 {
			t.Fatal("credit release did not admit both original owners")
		}
		f.finishPackets(f.tcp, f.udp)
		if requireIsolation && (udpStarted != 1 || udpAdmitted != 1) {
			t.Errorf("isolation: later UDP reached started=%d admitted=%d before unrelated TCP credit; want 1/1", udpStarted, udpAdmitted)
		}
	})
}

// Expected semantic red on the unchanged integrated producer path.
func TestNativeBatchTcpFirstUdpAdmissionIsolation(t *testing.T) {
	testNativeBatchTcpFirstIsolation(t, false, true)
}

// The ordinary mux upstream must not hide the same cross-flow serialization.
func TestIpMuxTcpFirstUdpAdmissionIsolation(t *testing.T) {
	testNativeBatchTcpFirstIsolation(t, true, true)
}

// A TCP-only batch still waits for the exact retained original. Visiting a
// different flow early never manufactures capacity for this reliable owner.
func TestNativeBatchTcpRetainsReliableCredit(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newNativeBatchCreditFixture(t)
		done := f.startBatch(false, -1, f.tcp)
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("reliable owner bypassed its exact peer-credit wait")
		default:
		}
		if f.udpStarted.Load() != 0 || len(f.base.route) != 0 {
			t.Fatal("TCP-only wait manufactured another original or wire write")
		}
		f.releasePeerCredit()
		if result := f.completedBatch(done); result.count != 1 || result.accepted != 1 {
			t.Fatal("exact credit release lost its reliable owner")
		}
		f.finishPackets(f.tcp)
	})
}

// Reversing only group order preserves UDP admission and its ready write
// while the same reliable TCP owner still waits for peer credit.
func TestNativeBatchUdpFirstReachesOwnAdmission(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newNativeBatchCreditFixture(t)
		done := f.startBatch(false, -1, f.udp, f.tcp)
		synctest.Wait()
		if f.udpStarted.Load() != 1 || f.udpAdmitted.Load() != 1 ||
			f.fastAttempted.Load() != 1 || f.fastWritten.Load() != 1 || len(f.base.route) != 1 {
			t.Fatal("UDP-first did not write its ready whole group before the TCP wait")
		}
		select {
		case <-done:
			t.Fatal("later TCP did not retain its own capacity wait")
		default:
		}
		f.releasePeerCredit()
		if result := f.completedBatch(done); result.count != 2 || result.accepted != 3 {
			t.Fatal("UDP-first call lost a member after real credit release")
		}
		f.finishPackets(f.udp, f.tcp)
	})
}

// Refusing the TCP owner at the zero-timeout boundary still visits UDP and
// reports exact partial native admission; no refused bytes become recovery.
func TestNativeBatchZeroTimeoutPreservesPartialAdmission(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newNativeBatchCreditFixture(t)
		result := <-f.startBatch(false, 0, f.tcp, f.udp)
		if result.count != 1 || result.accepted != 2 || f.udpAdmitted.Load() != 1 {
			t.Fatal("zero-timeout refusal lost exact UDP-only acceptance")
		}
		f.releasePeerCredit()
		f.finishPackets(f.udp)
		if f.base.offer(f.tcp) != 1 {
			t.Fatal("refused TCP original acquired false collapse ownership")
		}
		f.base.acknowledgeAll(f.tcp)
	})
}

// Fake time advances only to the real caller timer. The primary causal tests
// use no clock wait; this adjacent case checks the actual SDK default budget.
func TestNativeBatchFiniteTimeoutPreservesPartialAdmission(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newNativeBatchCreditFixture(t)
		start := time.Now()
		result := <-f.startBatch(false, 5*time.Second, f.tcp, f.udp)
		if time.Since(start) != 5*time.Second || result.count != 1 ||
			result.accepted != 2 || f.udpStarted.Load() != 1 || f.udpAdmitted.Load() != 1 {
			t.Fatal("finite TCP timeout changed the later group's exact admission")
		}
		// Cancellation joins the still-owned UDP and seed without advancing
		// unrelated recovery timers to pretend they were acknowledged.
	})
}

// Canceling an indefinitely blocked native caller releases its refused TCP
// once without erasing the already-written UDP's exact partial membership.
func TestNativeBatchCancellationJoinsOriginalOwners(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newNativeBatchCreditFixture(t)
		done := f.startBatch(false, -1, f.tcp, f.udp)
		synctest.Wait()
		if f.udpStarted.Load() != 1 || f.udpAdmitted.Load() != 1 {
			t.Fatal("cancellation premise lost the independently admitted UDP")
		}
		f.finishPackets(f.udp)
		f.base.cancel()
		if result := <-done; result.count != 1 || result.accepted != 2 {
			t.Fatal("cancellation lost exact prior UDP-only ownership")
		}
	})
}

// A distinct destination on the same Client has a ready source and route.
// Independent admission must progress even while another batch call waits.
func TestNativeBatchIndependentDestinationProgress(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newNativeBatchCreditFixture(t)
		peer := NewId()
		selected := newPacketTransferTestChannel()
		selected.ctx, selected.client, selected.settings = f.base.parent.ctx, f.base.client, f.base.parent.settings
		selected.args = &multiClientChannelArgs{Destination: RequireMultiHopId(peer)}
		f.udpUpdate.client.Store(selected)
		f.otherRoute = make(Route, 4)
		f.base.client.ContractManager().AddNoContractPeer(peer)
		f.base.client.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(peer)), []Route{f.otherRoute})
		blocked := f.startBatch(false, -1, f.tcp)
		synctest.Wait()
		if result := <-f.startBatch(false, 0, f.udp); result.count != 1 || result.accepted != 1 {
			t.Fatal("independent destination admission inherited another source's credit gate")
		}
		synctest.Wait()
		if len(f.otherRoute) != 1 {
			t.Fatal("independent destination did not reach its ready route")
		}
		wire := <-f.otherRoute
		pack := decodeSendPackLifecycleWirePack(t, wire)
		if !pack.Nack || len(pack.Frames) != 1 || !bytes.Equal(pack.Frames[0].MessageBytes, f.udp) {
			MessagePoolReturn(wire)
			t.Fatal("independent destination changed UDP policy or bytes")
		}
		MessagePoolReturn(wire)
		f.releasePeerCredit()
		if result := f.completedBatch(blocked); result.count != 1 || result.accepted != 1 {
			t.Fatal("independent progress lost the still-owned TCP producer")
		}
		f.finishPackets(f.tcp)
	})
}

// This control models the native reader's serial calls, not one mixed batch.
// Visiting all groups nonblocking cannot expose a next batch not read yet.
func TestNativeBatchNextReadRemainsBehindPriorCall(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newNativeBatchCreditFixture(t)
		first, second := f.ownPackets(f.tcp), f.ownPackets(f.udp)
		nextRead := make(chan struct{})
		done := make(chan int, 1)
		f.workers.Add(1)
		go func() {
			defer f.workers.Done()
			count := f.base.parent.SendPacketBatch(f.base.source, protocol.ProvideMode_Network, first, -1)
			close(nextRead)
			count += f.base.parent.SendPacketBatch(f.base.source, protocol.ProvideMode_Network, second, -1)
			done <- count
		}()
		synctest.Wait()
		select {
		case <-nextRead:
			t.Fatal("reader advanced before its prior native call returned")
		default:
		}
		if f.udpStarted.Load() != 0 {
			t.Fatal("unread next batch somehow reached Transfer")
		}
		f.releasePeerCredit()
		synctest.Wait()
		select {
		case count := <-done:
			if count != 2 {
				t.Fatal("released native reader lost an original")
			}
		default:
			t.Fatal("released native reader did not complete both batch calls")
		}
		f.finishPackets(f.tcp, f.udp)
	})
}

// A scalar Transfer call is a control, not a replacement native entry: even
// RemoteUserNatMultiClient.SendPacket wraps a logical group in this source.
func TestNativeBatchGroupedNoAckVersusScalarReadyWrite(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newNativeBatchCreditFixture(t)
		if result := <-f.startBatch(false, 0, f.udp); result.count != 1 || result.accepted != 1 {
			t.Fatal("logical UDP group did not acquire ordinary queue ownership")
		}
		if f.fastAttempted.Load() != 1 || f.fastWritten.Load() != 1 || len(f.base.route) != 1 {
			t.Fatal("eligible whole group did not share the zero-wait ready-write boundary")
		}
		f.finishPackets(f.udp)
		template := ipOosUdpPacket(f.udpPath, []byte{0x44})
		packet := f.ownPackets(template)[0]
		frame, err := ipPacketToProviderFrame(packet, f.base.parent.settings.ProtocolVersion)
		if err != nil {
			MessagePoolReturn(packet)
			t.Fatal(err)
		}
		if !frame.Raw {
			MessagePoolReturn(frame.MessageBytes)
			MessagePoolReturn(packet)
			t.Fatal("scalar control requires the existing raw v2 packet path")
		}
		if success, err := f.base.client.SendWithTimeoutDetailed(frame, f.base.peer, nil, 0,
			NoAck(), scheduleIpFlow(f.udpPath)); err != nil || !success {
			MessagePoolReturn(packet)
			t.Fatalf("ready scalar NoAck control refused: %v", err)
		}
		if f.fastAttempted.Load() != 2 || f.fastWritten.Load() != 2 {
			t.Fatal("scalar control did not retain its independent zero-wait route attempt")
		}
		f.finishPackets(template)
		f.releasePeerCredit()
	})
}
