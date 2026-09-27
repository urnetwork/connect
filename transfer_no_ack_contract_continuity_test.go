// TCP flow retirement must not discard the authenticated Transfer state used
// by unrelated NoAck datagrams on that same multiplexed receive sequence.
package connect

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// Retain only the first fixed-size identity diagnostic and a total, never a
// payload, signed contract proof, or unbounded per-packet history.
type noAckContractContinuityLogger struct {
	Logger
	stateLock sync.Mutex
	count     int
	first     string
}

func (self *noAckContractContinuityLogger) Infof(format string, values ...any) {
	if !strings.Contains(format, "drop nack contract mismatch") {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.count++
	if self.first == "" {
		self.first = fmt.Sprintf(format, values...)
	}
}

func (self *noAckContractContinuityLogger) snapshot() (int, string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.count, self.first
}

// The existing TCP fixture parks its worker before deferred map cleanup. A
// canceled TCP sequence therefore remains indexed deterministically, exactly
// the lifecycle interval in which a final empty ACK/FIN can arrive. Transfer
// itself uses real Clients, signed grants, ACK workers and both NoAck APIs.
func runNoAckContractContinuity(t *testing.T, group bool, flags byte, closeAfterLookup bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		log := &noAckContractContinuityLogger{Logger: NewNoopLogger()}
		contractsAtFakeTime := func(settings *ClientSettings) {
			settings.Log = log
			settings.ContractManagerSettings.NetworkEventTimeEnableContracts = time.Unix(1, 0)
			settings.ContractManagerSettings.NetworkEventTimeChangeHmac = time.Unix(1, 0)
			settings.ContractManagerSettings.LegacyCreateContract = false
		}
		f := newReliableTcpIngressFixture(t, 4, nil, contractsAtFakeTime)
		f.establishForControl()
		f.client.ContractManager().SetProvideModes(map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true})
		settings := DefaultClientSettingsWithBufferSize(64)
		contractsAtFakeTime(settings)
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-f.ctx.Done() }
		sender := NewClient(f.ctx, f.source.SourceId,
			&idleContractTestOob{sourceId: f.source.SourceId, peer: func() *Client { return f.client }}, settings)
		outbound, inbound, reverse := make(Route, 64), make(Route, 64), make(Route, 64)
		var noAckFrames, ackFrames atomic.Int32
		forwardDone := make(chan struct{})
		go func() {
			defer close(forwardDone)
			for {
				select {
				case <-f.ctx.Done():
					return
				case wire := <-outbound:
					var transfer protocol.TransferFrame
					if err := proto.Unmarshal(wire, &transfer); err == nil && transfer.Pack != nil {
						for _, frame := range transfer.Pack.Frames {
							if frame.MessageType != protocol.MessageType_TestSimpleMessage {
								continue
							}
							var message protocol.SimpleMessage
							if proto.Unmarshal(frame.MessageBytes, &message) == nil && strings.HasPrefix(message.Content, "datagram-") {
								if transfer.Pack.Nack {
									noAckFrames.Add(1)
								} else {
									ackFrames.Add(1)
								}
							}
						}
					}
					select {
					case inbound <- wire:
					case <-f.ctx.Done():
						MessagePoolReturn(wire)
						return
					}
				}
			}
		}()
		t.Cleanup(func() {
			f.cancel()
			<-forwardDone
			if err := sender.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
			if err := f.client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
			for _, route := range []Route{outbound, inbound, reverse} {
				for len(route) != 0 {
					MessagePoolReturn(<-route)
				}
			}
		})
		sender.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(f.client.ClientId())), []Route{outbound})
		f.client.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{inbound})
		f.client.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(sender.ClientId())), []Route{reverse})
		sender.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{reverse})
		var delivered atomic.Int32
		f.client.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
			for _, frame := range frames {
				if frame.MessageType == protocol.MessageType_TestSimpleMessage {
					var message protocol.SimpleMessage
					if proto.Unmarshal(frame.MessageBytes, &message) == nil && strings.HasPrefix(message.Content, "datagram-") {
						delivered.Add(1)
					}
				}
			}
		})
		opening := RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: "signed-contract readiness"})
		ready := make(chan error, 1)
		if !sender.SendWithTimeout(opening, f.client.ClientId(), func(err error) { ready <- err }, time.Second) {
			MessagePoolReturn(opening.MessageBytes)
			t.Fatal("contract readiness was not admitted")
		}
		if err := <-ready; err != nil {
			t.Fatalf("contract readiness failed: %v", err)
		}
		synctest.Wait()
		sendSequence := sender.sendBuffer.lookupSendSequence(sendSequenceId{Destination: f.client.ClientId()}, nil)
		if sendSequence == nil || sendSequence.sendContract == nil || !sendSequence.sendContractAcked {
			t.Fatal("sender did not establish a positively acknowledged signed contract")
		}
		f.client.receiveBuffer.mutex.Lock()
		var receiveSequence *ReceiveSequence
		for id, sequence := range f.client.receiveBuffer.receiveSequences {
			if id.SequenceId == sendSequence.sequenceId {
				receiveSequence = sequence
			}
		}
		f.client.receiveBuffer.mutex.Unlock()
		if receiveSequence == nil || receiveSequence.openReceiveContracts[sendSequence.sendContract.contractId] == nil {
			t.Fatal("receiver never verified the sender's exact signed contract")
		}
		sendDatagrams := func(first, count int) {
			frames := make([]*protocol.Frame, count)
			for index := range count {
				frames[index] = RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: fmt.Sprintf("datagram-%d", first+index)})
			}
			if group {
				accepted, err := sender.sendMultiHopGroupWithTimeoutDetailed(frames, RequireMultiHopId(f.client.ClientId()), nil, time.Second, NoAck())
				if !accepted || err != nil {
					for _, frame := range frames {
						MessagePoolReturn(frame.MessageBytes)
					}
					t.Fatalf("NoAck group admission: %t %v", accepted, err)
				}
			} else {
				for _, frame := range frames {
					if !sender.SendWithTimeout(frame, f.client.ClientId(), nil, time.Second, NoAck()) {
						MessagePoolReturn(frame.MessageBytes)
						t.Fatal("NoAck singleton admission failed")
					}
				}
			}
			synctest.Wait()
		}
		sendDatagrams(0, 1)
		if delivered.Load() != 1 || noAckFrames.Load() != 1 || ackFrames.Load() != 0 {
			t.Fatal("initial datagram did not use and deliver on the established NoAck contract")
		}
		// The TCP worker remains behind its existing fixture barrier. Only its
		// flow is canceled; provider, source, NAT and both Clients remain live.
		if closeAfterLookup {
			// All NAT work is quiescent after the first delivered datagram;
			// install the one-shot ordering before offering the next packet.
			f.nat.settings.TcpBufferSettings.beforeSequenceSendForTest = func(sequence *TcpSequence) {
				if sequence == f.tcp {
					sequence.Cancel()
				}
			}
		} else {
			f.tcp.Cancel()
		}
		packet := MessagePoolCopy(ipOosTcpPacketSequence(f.path, flags, 101, nil))
		control, err := ipPacketToProviderFrame(packet, DefaultProtocolVersion)
		if err != nil {
			MessagePoolReturn(packet)
			t.Fatal(err)
		}
		if !sender.SendWithTimeout(control, f.client.ClientId(), nil, time.Second) {
			MessagePoolReturn(control.MessageBytes)
			t.Fatal("late reliable TCP control was not admitted")
		}
		synctest.Wait()
		sendDatagrams(1, 30)
		mismatches, firstMismatch := log.snapshot()
		if receiveSequence.ctx.Err() != nil || delivered.Load() != 31 || mismatches != 0 {
			t.Errorf("group=%t flags=%#x close_after_lookup=%t shared_lane=%v NoAck_delivery=%d/31 contract_mismatches=%d first=%s", group, flags, closeAfterLookup, receiveSequence.ctx.Err(), delivered.Load(), mismatches, firstMismatch)
		}
		if noAckFrames.Load() != 31 || ackFrames.Load() != 0 {
			t.Errorf("group=%t datagram policy changed: NoAck=%d Ack=%d", group, noAckFrames.Load(), ackFrames.Load())
		}
		t.Logf("shared_lane=%v NoAck_delivery=%d/31 contract_mismatches=%d wire_NoAck=%d wire_Ack=%d", receiveSequence.ctx.Err(), delivered.Load(), mismatches, noAckFrames.Load(), ackFrames.Load())
	})
}

func TestNoAckSignedContractSurvivesTcpRetirementSingleton(t *testing.T) {
	runNoAckContractContinuity(t, false, tcpFlagAck, false)
}

func TestNoAckSignedContractSurvivesTcpRetirementGroup(t *testing.T) {
	runNoAckContractContinuity(t, true, tcpFlagAck, false)
}

func TestNoAckSignedContractSurvivesTcpFinRetirementSingleton(t *testing.T) {
	runNoAckContractContinuity(t, false, tcpFlagFin|tcpFlagAck, false)
}

func TestNoAckSignedContractSurvivesTcpFinRetirementGroup(t *testing.T) {
	runNoAckContractContinuity(t, true, tcpFlagFin|tcpFlagAck, false)
}

func TestNoAckSignedContractSurvivesTcpAdmissionRetirementSingleton(t *testing.T) {
	runNoAckContractContinuity(t, false, tcpFlagAck, true)
}

func TestNoAckSignedContractSurvivesTcpAdmissionRetirementGroup(t *testing.T) {
	runNoAckContractContinuity(t, true, tcpFlagFin|tcpFlagAck, true)
}
