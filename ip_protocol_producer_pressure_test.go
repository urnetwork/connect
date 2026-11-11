// Actual IP producers keep authorized controls independent of DATA admission.
package connect

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// One actual reliable original owns the entire resized DATA leaf. Its wire
// stays test-owned and unacknowledged until only that child is canceled.
type protocolProducerPressure struct {
	client            *Client
	budget            *TransferMemoryBudget
	total             ByteCount
	used              ByteCount
	sequence          *SendSequence
	parked            Route
	liveRoute         Route
	transport         Transport
	completion        protocolPressureCompletion
	refusals          atomic.Int64
	lifecycleObserver func(SendPackLifecycleObservation)
	once              sync.Once
}

// Uses public serialization, then republishes the unchanged live peer route
// so a dynamic cap change has reached the ordinary admission publication.
func newProtocolProducerPressure(t *testing.T, client *Client, liveRoute Route) *protocolProducerPressure {
	t.Helper()
	pressure := &protocolProducerPressure{client: client, budget: client.settings.SendBufferSettings.ResendQueueBudget, parked: make(Route, 16), liveRoute: liveRoute}
	pressure.lifecycleObserver = client.settings.SendBufferSettings.SendPackLifecycleObserver
	pressure.total = pressure.budget.TotalByteCount()
	t.Cleanup(func() { pressure.release(t) })
	peer := NewId()
	client.ContractManager().AddNoContractPeer(peer)
	client.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(peer)), []Route{pressure.parked})
	packet := MessagePoolCopy(ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagAck, 9000, bytes.Repeat([]byte{0x6a}, 1240)))
	frame, err := ipPacketToProviderFrame(packet, DefaultProtocolVersion)
	if err != nil {
		MessagePoolReturn(packet)
		t.Fatal(err)
	}
	admitted, err := client.SendWithTimeoutDetailed(frame, peer, pressure.completion.record, time.Second,
		sendPackLifecycleObserverOption{observer: func(SendPackLifecycleObservation) {}})
	if !admitted || err != nil {
		MessagePoolReturn(frame.MessageBytes)
		t.Fatalf("pressure original was refused: %v", err)
	}
	synctest.Wait()
	client.sendBuffer.mutex.Lock()
	for _, sequence := range client.sendBuffer.sendSequencesBySequenceId {
		if sequence.id().Destination == peer {
			pressure.sequence = sequence
		}
	}
	client.sendBuffer.mutex.Unlock()
	pressure.used = pressure.budget.UsedByteCount()
	if pressure.sequence == nil || pressure.used == 0 || pressure.sequence.resendQueue.Len() != 1 || len(pressure.parked) != 1 || pressure.completion.calls.Load() != 0 {
		t.Fatal("DATA pressure lacks an actual serialized unacknowledged owner")
	}
	pressure.budget.SetTotalByteCount(pressure.used)
	routes := client.RouteManager()
	routes.mutex.Lock()
	var liveTransport Transport
	for transport, transportRoutes := range routes.writerMatchState.transportRoutes {
		if len(transportRoutes) == 1 && transportRoutes[0] == liveRoute {
			liveTransport = transport
			break
		}
	}
	routes.mutex.Unlock()
	if liveTransport == nil {
		t.Fatal("pressure fixture lost the original live carrier")
	}
	pressure.transport = liveTransport
	routes.UpdateTransport(liveTransport, []Route{liveRoute})
	synctest.Wait()
	client.sendBuffer.mutex.Lock()
	published := false
	for _, sequence := range client.sendBuffer.sendSequencesBySequenceId {
		if sequence != pressure.sequence && sequence.resendCapacityUnavailable.Load() {
			published = true
		}
	}
	client.sendBuffer.mutex.Unlock()
	if !published {
		t.Fatal("live peer did not publish full DATA admission after the unchanged route wake")
	}
	pressure.require(t)
	client.settings.SendBufferSettings.SendPackLifecycleObserver = func(event SendPackLifecycleObservation) {
		var refusal *SendPackAdmissionError
		if event.Phase == SendPackLifecyclePhaseTerminal && event.AckRequired &&
			errors.As(event.Err, &refusal) && refusal.Boundary == "resend-capacity" && refusal.Timeout == 0 &&
			!refusal.RecoveredByOwner && !refusal.OwnerTrackingOverflow && errors.Is(refusal.Err, ErrSendPackNotAdmitted) {
			pressure.refusals.Add(1)
			return
		}
		if pressure.lifecycleObserver != nil {
			pressure.lifecycleObserver(event)
		}
	}
	return pressure
}

// A protocol transition cannot release or borrow the retained DATA original.
func (self *protocolProducerPressure) require(t *testing.T) {
	t.Helper()
	if self.budget.UsedByteCount() != self.used || self.budget.TotalByteCount() != self.used || self.completion.calls.Load() != 0 || self.sequence.resendQueue.Len() != 1 {
		t.Fatal("protocol producer changed the pressure owner's DATA custody")
	}
}

// Joins only the pressure child; the actual native flow and shared Client live.
func (self *protocolProducerPressure) release(t *testing.T) {
	t.Helper()
	self.once.Do(func() {
		if self.sequence != nil {
			self.sequence.cancel()
			<-self.sequence.done
		}
		synctest.Wait()
		for len(self.parked) != 0 {
			MessagePoolReturn(<-self.parked)
		}
		self.client.settings.SendBufferSettings.SendPackLifecycleObserver = self.lifecycleObserver
		self.budget.SetTotalByteCount(self.total)
		if self.transport != nil && self.client.ctx.Err() == nil {
			self.client.RouteManager().UpdateTransport(self.transport, []Route{self.liveRoute})
			synctest.Wait()
		}
		if self.sequence != nil && (self.completion.calls.Load() != 1 || self.completion.canceled.Load() != 1 || self.completion.success.Load() != 0 || self.completion.other.Load() != 0) {
			t.Error("pressure cancellation lost its exact non-success callback")
		}
		if self.budget.UsedByteCount() != 0 {
			t.Errorf("pressure child retained %d DATA bytes after joining", self.budget.UsedByteCount())
		}
	})
}

// Public native DATA+ACK input must not turn a valid ACK into DATA merely
// because its preceding member uses the same directional flow and generation.
func TestNativeProtocolProducerSeparatesAckAtFullData(t *testing.T) {
	checkNativeProtocolProducerPressure(t, false)
}

// Alternating classes retain exact indexes after an accepted child, a refused
// DATA child, and another accepted child; the source never retries accepted roots.
func TestNativeProtocolProducerAlternatingPartialCustody(t *testing.T) {
	checkNativeProtocolProducerPressure(t, true)
}

// Every offered packet reaches the real Multi/Client/provider path. DATA is
// retried only after its separate pressure owner has actually relinquished credit.
func checkNativeProtocolProducerPressure(t *testing.T, alternating bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture, _, ack := nativeAckPartitionCoveredFixture(t)
		pressure := newProtocolProducerPressure(t, fixture.sender, fixture.forward)
		providerPackets := watchNativePureAckProvider(t, fixture)
		beforeTerminals := fixture.senderTerminals.Load()
		// The same ordinary producer can already send a singleton ACK. This
		// isolates class mixing from cold-worker or encryption readiness.
		if !fixture.offer(ack) {
			t.Fatal("singleton native ACK did not pass the established full DATA leaf")
		}
		synctest.Wait()
		if providerPackets.Load() != 1 || fixture.senderTerminals.Load() != beforeTerminals+1 {
			t.Fatal("singleton native ACK lacked actual provider delivery and Transfer completion")
		}
		pressure.require(t)
		path := groupRecoveryParsed(t, ack).ipPath
		data := groupRecoveryPacket(path.SequenceNumber, path.AckSequenceNumber, path.TcpWindowSize, tcpFlagAck, []byte("next"))
		templates := [][]byte{data, ack}
		want := []bool{false, true}
		if alternating {
			templates = [][]byte{ack, data, ack, data}
			want = []bool{true, false, true, false}
		}
		membership := make([]bool, len(templates))
		before := time.Now()
		count := fixture.parent.SendNativePacketBatchWithResults(SourceId(fixture.sender.ClientId()), protocol.ProvideMode_Network,
			nativeAckPartitionOwnPackets(t, fixture, templates...), 5*time.Second, membership)
		synctest.Wait()
		wanted := 1
		if alternating {
			wanted = 2
		}
		if count != wanted || !slices.Equal(membership, want) || providerPackets.Load() != int64(1+wanted) ||
			fixture.senderTerminals.Load() != beforeTerminals+int64(1+wanted) || pressure.refusals.Load() != int64(wanted) || time.Since(before) != 0 {
			t.Fatalf("NATIVE_PROTOCOL_MIXED_DATA_PRESSURE: count=%d membership=%v provider=%d terminals=%d refusals=%d elapsed=%s",
				count, membership, providerPackets.Load(), fixture.senderTerminals.Load()-beforeTerminals, pressure.refusals.Load(), time.Since(before))
		}
		pressure.require(t)
		pressure.release(t)
		if fixture.sender.ctx.Err() != nil || fixture.receiver.ctx.Err() != nil || fixture.providerSequence.Load().ctx.Err() != nil {
			t.Fatal("pressure-child cancellation ended the healthy native flow")
		}
		readDone := make(chan struct{})
		uploaded := make(chan []byte, 1)
		t.Cleanup(func() { fixture.close(); <-readDone })
		go func() {
			defer close(readDone)
			payload := make([]byte, 4)
			if _, err := io.ReadFull(fixture.origin, payload); err == nil {
				uploaded <- payload
			}
		}()
		if !fixture.offer(data) {
			t.Fatal("identical refused DATA could not retry after exact pressure refund")
		}
		synctest.Wait()
		select {
		case payload := <-uploaded:
			if !bytes.Equal(payload, []byte("next")) {
				t.Fatal("class separation changed the retried DATA bytes")
			}
		default:
			t.Fatal("refunded DATA retry did not reach the original socket")
		}
		fixture.close()
		<-readDone
	})
}

// The actual provider callback builds logical groups before its real Client
// admits them. A queued DATA run must not charge a following pure close or lose its order.
func TestProviderProtocolProducerSeparatesFinAtFullData(t *testing.T) {
	checkProviderProtocolTerminalPressure(t, tcpFlagAck|tcpFlagFin)
}

// A trailing pure reset likewise retains its own class and callback receipt.
func TestProviderProtocolProducerSeparatesRstAtFullData(t *testing.T) {
	checkProviderProtocolTerminalPressure(t, tcpFlagAck|tcpFlagRst)
}

// Borrows a DATA/terminal batch through the real provider callback, queue,
// Client serializer, consumer delivery and reliable acknowledgement.
func checkProviderProtocolTerminalPressure(t *testing.T, flags byte) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newNativeDuplicateAckFixture(t, false)
		pressure := newProtocolProducerPressure(t, fixture.receiver, fixture.reverse)
		path := fixture.providerSequence.Load().IpPath()
		returnPath := path.Reverse()
		data := ipOosTcpPacketSequence(returnPath, tcpFlagAck, 9000, []byte("next"))
		control := ipOosTcpPacketSequence(returnPath, flags, 9004, nil)
		var delivered [][]byte
		remove := fixture.sender.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
			for _, frame := range frames {
				if frame.MessageType != protocol.MessageType_IpIpPacketFromProvider {
					continue
				}
				packet, err := ipPacketFromProviderBytes(frame)
				if err == nil && (bytes.Equal(packet, data) || bytes.Equal(packet, control)) {
					delivered = append(delivered, bytes.Clone(packet))
				}
			}
		})
		t.Cleanup(remove)
		before := fixture.returnTerminals.Load()
		fixture.provider.ReceiveBatch(SourceId(fixture.sender.ClientId()), protocol.ProvideMode_Network, path, [][]byte{data, control})
		synctest.Wait()
		// Both actual producer runs own raw custody. The terminal is exempt
		// from DATA bytes but cannot cross its same-flow DATA predecessor.
		// ReceiveBatch's compatibility API supplies TransferKey{}, hence a
		// client-role lane-zero source. The real handshake's server-role reply
		// is a different generation to this same destination and owns no batch.
		settings := fixture.receiver.settings
		if settings.SendBufferSettings.LogicalDataLaneCount != 0 || settings.DefaultTransferOpts.CompanionContract || settings.DefaultTransferOpts.ForceStream {
			t.Fatal("provider batch fixture changed its explicit default transfer identity")
		}
		expectedSource := sendSequenceId{Destination: fixture.sender.ClientId(), EncryptionRole: sequenceTlsRoleClient}
		fixture.receiver.sendBuffer.mutex.Lock()
		source := fixture.receiver.sendBuffer.sendSequences[expectedSource]
		indexed := source != nil && fixture.receiver.sendBuffer.sendSequencesBySequenceId[source.sequenceId] == source
		fixture.receiver.sendBuffer.mutex.Unlock()
		if source == nil || !indexed || source.id() != expectedSource || source.packAdmission == nil || source.protocolAdmission == nil {
			t.Fatal("provider batch lost its actual source and separate raw class owners")
		}
		source.packAdmission.mutex.Lock()
		dataCount := source.packAdmission.count
		source.packAdmission.mutex.Unlock()
		source.protocolAdmission.mutex.Lock()
		protocolCount := source.protocolAdmission.count
		source.protocolAdmission.mutex.Unlock()
		if dataCount != 1 || protocolCount != 1 || len(delivered) != 0 || fixture.returnTerminals.Load() != before {
			t.Fatalf("PROVIDER_PROTOCOL_ORDERED_DATA_PRESSURE: flags=%x data_raw=%d protocol_raw=%d delivered=%d terminals=%d", flags, dataCount, protocolCount, len(delivered), fixture.returnTerminals.Load()-before)
		}
		pressure.require(t)
		pressure.release(t)
		if len(delivered) != 2 || !bytes.Equal(delivered[0], data) || !bytes.Equal(delivered[1], control) ||
			fixture.returnTerminals.Load() != before+2 || source.ctx.Err() != nil ||
			fixture.sender.ctx.Err() != nil || fixture.receiver.ctx.Err() != nil {
			t.Fatalf("provider class split lost ordered delivery after exact pressure refund: flags=%x delivered=%d terminals=%d", flags, len(delivered), fixture.returnTerminals.Load()-before)
		}
		source.packAdmission.mutex.Lock()
		dataCount = source.packAdmission.count
		source.packAdmission.mutex.Unlock()
		source.protocolAdmission.mutex.Lock()
		protocolCount = source.protocolAdmission.count
		source.protocolAdmission.mutex.Unlock()
		if dataCount != 0 || protocolCount != 0 {
			t.Fatal("provider serialization retained a pre-sequence class slot")
		}
		remove()
		fixture.close()
	})
}

// A pure future FIN still uses normal TCP ordering after independent byte
// admission; it cannot close the socket while its preceding DATA is missing.
func TestNativeProtocolProducerSeparatesFinAtFullData(t *testing.T) {
	checkNativeProtocolTerminalPressure(t, tcpFlagAck|tcpFlagFin)
}

// An accepted reset ends only its actual TCP child. Refused DATA never gains
// a successful delivery receipt merely because the reset shares its tuple.
func TestNativeProtocolProducerSeparatesRstAtFullData(t *testing.T) {
	checkNativeProtocolTerminalPressure(t, tcpFlagAck|tcpFlagRst)
}

// The provider's real dispatch and terminal handling decide the TCP fate.
func checkNativeProtocolTerminalPressure(t *testing.T, flags byte) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture, _, ack := nativeAckPartitionCoveredFixture(t)
		pressure := newProtocolProducerPressure(t, fixture.sender, fixture.forward)
		path := groupRecoveryParsed(t, ack).ipPath
		data := groupRecoveryPacket(path.SequenceNumber, path.AckSequenceNumber, path.TcpWindowSize, tcpFlagAck, []byte("next"))
		control := groupRecoveryPacket(path.SequenceNumber+4, path.AckSequenceNumber, path.TcpWindowSize, flags, nil)
		var delivered atomic.Int64
		remove := fixture.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
			for _, frame := range frames {
				if frame.MessageType != protocol.MessageType_IpIpPacketToProvider {
					continue
				}
				packet, err := ipPacketToProviderBytes(frame)
				if err == nil && bytes.Equal(packet, control) {
					delivered.Add(1)
				}
			}
		})
		t.Cleanup(remove)
		before := fixture.senderTerminals.Load()
		membership := []bool{false, false}
		count := fixture.parent.SendNativePacketBatchWithResults(SourceId(fixture.sender.ClientId()), protocol.ProvideMode_Network,
			nativeAckPartitionOwnPackets(t, fixture, data, control), 5*time.Second, membership)
		synctest.Wait()
		if count != 1 || !slices.Equal(membership, []bool{false, true}) || delivered.Load() != 1 || fixture.senderTerminals.Load() != before+1 || pressure.refusals.Load() != 1 {
			t.Fatalf("NATIVE_TERMINAL_MIXED_DATA_PRESSURE: flags=%x count=%d membership=%v delivered=%d terminals=%d refusals=%d",
				flags, count, membership, delivered.Load(), fixture.senderTerminals.Load()-before, pressure.refusals.Load())
		}
		pressure.require(t)
		ended := fixture.providerSequence.Load().ctx.Err() != nil
		if ended != (flags&tcpFlagRst != 0) || fixture.sender.ctx.Err() != nil || fixture.receiver.ctx.Err() != nil {
			t.Fatal("terminal class separation changed TCP ordering or shared Client lifetime")
		}
		pressure.release(t)
		fixture.close()
	})
}

// Payload-bearing close flags and malformed packets never acquire the
// producer's protocol exemption, even when the peer has an established flow.
func TestNativeProtocolProducerPayloadAndMalformedControlsStayData(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture, _, ack := nativeAckPartitionCoveredFixture(t)
		pressure := newProtocolProducerPressure(t, fixture.sender, fixture.forward)
		path := groupRecoveryParsed(t, ack).ipPath
		before := fixture.senderTerminals.Load()
		malformed := bytes.Clone(ack)
		malformed[0] = 0
		for _, packet := range [][]byte{
			groupRecoveryPacket(path.SequenceNumber, path.AckSequenceNumber, path.TcpWindowSize, tcpFlagAck, []byte("next")),
			groupRecoveryPacket(path.SequenceNumber, path.AckSequenceNumber, path.TcpWindowSize, tcpFlagAck|tcpFlagFin, []byte("next")),
			malformed,
		} {
			if fixture.offer(packet) {
				t.Fatal("payload-bearing or malformed control bypassed full DATA admission")
			}
			synctest.Wait()
			pressure.require(t)
		}
		if fixture.senderTerminals.Load() != before || fixture.providerSequence.Load().ctx.Err() != nil || pressure.refusals.Load() != 2 {
			t.Fatal("refused DATA control gained completion or closed the actual flow")
		}
		if !fixture.offer(ack) {
			t.Fatal("malformed DATA sibling damaged later pure ACK admission")
		}
		synctest.Wait()
		pressure.require(t)
		pressure.release(t)
		fixture.close()
	})
}

// A real public reset between class runs invalidates the old batch's epoch.
// Its already-admitted prefix remains owned and completes exactly once.
func TestNativeProtocolProducerResetBetweenRunsKeepsPrefixCustody(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture, _, ack := nativeAckPartitionCoveredFixture(t)
		pressure := newProtocolProducerPressure(t, fixture.sender, fixture.forward)
		path := groupRecoveryParsed(t, ack).ipPath
		data := groupRecoveryPacket(path.SequenceNumber, path.AckSequenceNumber, path.TcpWindowSize, tcpFlagAck, []byte("next"))
		reset := groupRecoveryPacket(path.SequenceNumber, path.AckSequenceNumber, path.TcpWindowSize, tcpFlagAck|tcpFlagRst, nil)
		resetSent := false
		fixture.parent.beforePacketGroupGateForTest = func(_ *multiClientChannelUpdate, _ *multiClientChannel, group *parsedPacketGroup) {
			if resetSent || len(group.packets) != 1 || len(group.packets[0].payload) == 0 {
				return
			}
			resetSent = true
			if !fixture.offer(reset) {
				t.Error("interleaved real native reset was refused")
			}
			synctest.Wait()
		}
		before := fixture.senderTerminals.Load()
		membership := []bool{false, false, false}
		count := fixture.parent.SendNativePacketBatchWithResults(SourceId(fixture.sender.ClientId()), protocol.ProvideMode_Network,
			nativeAckPartitionOwnPackets(t, fixture, ack, data, ack), 5*time.Second, membership)
		synctest.Wait()
		if !resetSent || count != 1 || !slices.Equal(membership, []bool{true, false, false}) ||
			fixture.senderTerminals.Load() != before+2 || fixture.providerSequence.Load().ctx.Err() == nil {
			t.Fatalf("interleaved reset changed old-run ownership: reset=%t count=%d membership=%v terminals=%d ended=%t",
				resetSent, count, membership, fixture.senderTerminals.Load()-before, fixture.providerSequence.Load().ctx.Err() != nil)
		}
		pressure.require(t)
		pressure.release(t)
		if fixture.sender.ctx.Err() != nil || fixture.receiver.ctx.Err() != nil {
			t.Fatal("terminal child closed a shared Client")
		}
		fixture.close()
	})
}
