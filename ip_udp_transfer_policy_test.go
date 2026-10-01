package connect

import (
	"bytes"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Inspect real Transfer packets in both directions. No-contract peers prevent
// contract-opening reliability from hiding an incorrectly selected policy.
func TestUdpTransferMatchedWirePolicy(t *testing.T) {
	for _, policy := range []struct {
		name     string
		noAck    bool
		defaults bool
	}{
		{name: "ack", noAck: false},
		{name: "noack", noAck: true},
		{name: "defaults", noAck: true, defaults: true},
	} {
		noAck := policy.noAck
		for _, providerReturn := range []bool{false, true} {
			for _, grouped := range []bool{false, true} {
				if providerReturn && grouped {
					continue // Socket packetization ownership is checked below.
				}
				t.Run(fmt.Sprintf("%s/provider=%t/group=%t", policy.name, providerReturn, grouped), func(t *testing.T) {
					assertMessagePoolOwnership(t)
					synctest.Test(t, func(t *testing.T) {
						var override []bool
						if !policy.defaults {
							override = []bool{noAck}
						}
						fixture, _, sequence, _ := newProviderDatagramReturnPolicyTest(t, override...)
						var noAckEvents []NoAckSendObservation
						fixture.sender.settings.SendBufferSettings.NoAckSendObserver = func(observation NoAckSendObservation) {
							noAckEvents = append(noAckEvents, observation)
						}
						if providerReturn {
							commitProviderDatagramForPolicyTest(t, sequence, 1000)
						} else {
							channel := newPacketTransferTestChannel()
							channel.settings = DefaultMultiClientSettings()
							channel.ctx, channel.client = fixture.ctx, fixture.sender
							if !policy.defaults {
								channel.settings.UdpTransferNoAck = noAck
								channel.settings.UdpCollapsePrevention = false
							}
							channel.performanceProfile = &PerformanceProfile{AllowDirect: true}
							destination, err := NewMultiHopId(fixture.receiver.ClientId())
							if err != nil {
								t.Fatal(err)
							}
							channel.args = &multiClientChannelArgs{Destination: destination}
							packet := parsedPacket{packet: MessagePoolGet(64), ipPath: udpTestPath(4)}
							clear(packet.packet)
							packet.packet[0], packet.packet[9] = 0x45, 17
							var sent bool
							if grouped {
								sent, err = channel.SendGroupDetailed(&parsedPacketGroup{packets: []parsedPacket{packet}, ipPath: packet.ipPath, byteCount: 64}, time.Second)
							} else {
								sent, err = channel.SendDetailed(&packet, time.Second)
							}
							if !sent || err != nil {
								MessagePoolReturn(packet.packet)
								t.Fatalf("client admission: %t %v", sent, err)
							}
						}
						wire := fixture.takePack(0)
						if wire.pack.Nack != noAck {
							t.Fatalf("wire Nack=%t, want %t", wire.pack.Nack, noAck)
						}
						fixture.forward(wire, fixture.receiverIn)
						if noAck {
							if len(fixture.receiverOut) != 0 {
								t.Fatal("NoAck datagram generated Transfer ACK work")
							}
						} else {
							fixture.acknowledge()
						}
						if fixture.deliveredCount != 1 {
							t.Fatalf("delivered=%d, want 1", fixture.deliveredCount)
						}
						if noAck {
							if len(noAckEvents) != 2 || noAckEvents[0].Phase != NoAckSendPhaseStarted || noAckEvents[1].Phase != NoAckSendPhaseCompleted || noAckEvents[0].Token == 0 || noAckEvents[0].Token != noAckEvents[1].Token {
								t.Fatalf("NoAck send lost its exact observer pair: %+v", noAckEvents)
							}
						} else if len(noAckEvents) != 0 {
							t.Fatalf("ACK send fabricated NoAck events: %+v", noAckEvents)
						}
					})
				})
			}
		}
	}
}

// Default constructors must agree for ordinary and memory-sized providers.
// ACK remains required for TCP, explicit route-discovery requests and unknown
// IP metadata; the NoAck default cannot demote these ownership boundaries.
func TestUdpTransferDefaultPolicyInvariants(t *testing.T) {
	client := DefaultMultiClientSettings()
	for _, provider := range []*RemoteUserNatProviderSettings{
		DefaultRemoteUserNatProviderSettings(),
		DefaultRemoteUserNatProviderSettingsWithMemoryTarget(mib(3)),
		DefaultRemoteUserNatProviderSettingsWithMemoryTarget(mib(24)),
	} {
		if !client.UdpTransferNoAck || !provider.UdpTransferNoAck || client.UdpCollapsePrevention {
			t.Fatal("default UDP policies are not symmetric established NoAck")
		}
	}
	udp := udpTestPath(4)
	for _, direct := range []bool{false, true} {
		channel := &multiClientChannel{settings: client, performanceProfile: &PerformanceProfile{AllowDirect: direct}}
		if channel.ipPacketTransferAckRequired(udp) {
			t.Fatalf("default established UDP unexpectedly requires ACK: direct=%t", direct)
		}
		if !channel.ipPacketTransferAckRequired(icmpTcpTestPath(4)) || !channel.ipPacketTransferAckRequired(nil) || !ipPacketTransferAckForRequest(udp, true) {
			t.Fatal("datagram default disabled TCP, unknown-packet or explicit setup ACK ownership")
		}
	}
}

func commitProviderDatagramForPolicyTest(t *testing.T, sequence *UdpSequence, size int) {
	t.Helper()
	lease, err := sequence.prepareReturnRead(size)
	if err != nil || lease == nil {
		t.Fatalf("prepare datagram: %v", err)
	}
	packets, err := sequence.DataPackets(bytes.Repeat([]byte{0x5a}, size), size, sequence.udpBufferSettings.Mtu)
	if err != nil {
		lease.abort()
		t.Fatal(err)
	}
	lease.commit(packets)
}

// A dropped NoAck datagram neither spends recovery work nor pins the next
// datagram behind the lost sequence. Reliable mode retains its existing test
// proving recovery after the flow retires in ip_provider_datagram_return_test.
func TestProviderDatagramNoAckLossDoesNotCollapseLaterDelivery(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture, provider, sequence, _ := newProviderDatagramReturnPolicyTest(t, true)
		commitProviderDatagramForPolicyTest(t, sequence, 1000)
		first := fixture.takePack(0)
		if !first.pack.Nack {
			t.Fatal("NoAck policy was not applied")
		}
		fixture.drop(first)
		time.Sleep(time.Second)
		synctest.Wait()
		if len(fixture.senderOut) != 0 || sequence.returnReadPending.Load() {
			t.Fatal("lost NoAck datagram retained retransmission or socket-read ownership")
		}
		commitProviderDatagramForPolicyTest(t, sequence, 1000)
		fixture.forward(fixture.takePack(0), fixture.receiverIn)
		if fixture.deliveredCount != 1 || len(fixture.receiverOut) != 0 {
			t.Fatalf("later delivery collapsed: delivered=%d ack work=%d", fixture.deliveredCount, len(fixture.receiverOut))
		}
		if drops := provider.CongestionDropStats(); drops.ReturnSendPacketCount != 0 || drops.ReturnQueuePacketCount != 0 {
			t.Fatalf("underlay loss became software loss: %+v", drops)
		}
		if snapshot := fixture.sender.SendRecoveryStats(); snapshot.TimeoutResendWriteCount != 0 {
			t.Fatalf("NoAck datagram consumed retransmission work: %+v", snapshot)
		}
	})
}

func TestProviderDatagramNoAckReceiverInjectionOwnsBytes(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture, provider, sequence, _ := newProviderDatagramReturnPolicyTest(t, true)
		entered, release := make(chan struct{}), make(chan struct{})
		defer close(release)
		remote := &RemoteUserNatClient{
			securityPolicy: DisableSecurityPolicy(),
			receivePacketCallback: func(_ TransferPath, _ protocol.ProvideMode, path *IpPath, packet []byte) {
				before := bytes.Clone(packet)
				if path.Protocol != IpProtocolUdp || len(packet) != 1028 {
					t.Error("wrong received UDP packet")
				}
				close(entered)
				<-release
				if !bytes.Equal(packet, before) {
					t.Error("borrowed injection bytes were reused before callback returned")
				}
			},
		}
		unsubscribe := fixture.receiver.AddReceiveCallback(remote.ClientReceive)
		defer unsubscribe()
		commitProviderDatagramForPolicyTest(t, sequence, 1000)
		fixture.forward(fixture.takePack(0), fixture.receiverIn)
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("device injection did not start")
		}
		if len(fixture.receiverOut) != 0 || sequence.returnReadPending.Load() {
			t.Fatal("NoAck injection created ACK work or pinned the sender's read lease")
		}
		if used := fixture.sender.settings.SendBufferSettings.ResendQueueBudget.UsedByteCount(); used != 0 {
			t.Fatalf("NoAck write retained sender memory: %d", used)
		}
		lifecycle := provider.acquireSourceLifecycle(fixture.receiver.ClientId())
		if lifecycle.evidence.lastAckNanos.Load() != 0 || lifecycle.evidence.outstanding.Load() != 0 {
			t.Fatal("a NoAck local write fabricated peer ACK evidence")
		}
		provider.releaseSourceLifecycle(fixture.receiver.ClientId(), lifecycle)
		release <- struct{}{}
		synctest.Wait()
		if len(fixture.receiverOut) != 0 {
			t.Fatal("NoAck injection completion generated an ACK")
		}
	})
}

// A full local writer and queue must pause socket reads, retain the exact
// datagram and later drain once. All memory stays inside the existing budget.
func TestProviderDatagramNoAckRetainsRefusedLocalAdmission(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture, provider, sequence, root := newProviderDatagramReturnPolicyTest(t, true)
		var completed []NoAckSendObservation
		fixture.sender.settings.SendBufferSettings.NoAckSendObserver = func(observation NoAckSendObservation) {
			if observation.Phase == NoAckSendPhaseCompleted {
				completed = append(completed, observation)
			}
		}
		for range cap(fixture.senderOut) {
			fixture.senderOut <- MessagePoolGet(1)
		}
		// The one admission slot includes the active writer until its write.
		for range 1 {
			packet := MessagePoolGet(16)
			if sent, err := fixture.sender.SendWithTimeoutDetailed(&protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider, MessageBytes: packet}, fixture.receiver.ClientId(), nil, 0, NoAck(), protocol.ProvideMode_Network); !sent || err != nil {
				MessagePoolReturn(packet)
				t.Fatalf("fill source queue: %t %v", sent, err)
			}
			synctest.Wait()
		}
		commitProviderDatagramForPolicyTest(t, sequence, 1000)
		synctest.Wait()
		if !sequence.returnReadPending.Load() {
			t.Fatal("refused provider datagram lost its bounded socket-read owner")
		}
		if _, err := sequence.prepareReturnRead(1000); err == nil {
			t.Fatal("a second socket read bypassed the pending datagram")
		}
		time.Sleep(50 * time.Millisecond)
		synctest.Wait()
		if root.UsedByteCount() > root.TotalByteCount() || provider.CongestionDropStats().ReturnSendPacketCount != 0 {
			t.Fatal("local retry grew memory or discarded the owned datagram")
		}
		if len(completed) != 0 {
			t.Fatal("a refused retry was published before its exact input owner completed")
		}
		for range cap(fixture.senderOut) {
			MessagePoolReturn(<-fixture.senderOut)
		}
		synctest.Wait()
		time.Sleep(provider.settings.ReturnSendRetryTimeout)
		synctest.Wait()
		for range 1 {
			fixture.drop(fixture.takePack(0))
		}
		wire := fixture.takePack(0)
		if !wire.pack.Nack || len(wire.pack.Frames) != 1 || len(wire.pack.Frames[0].MessageBytes) != 1028 {
			t.Fatal("retained datagram changed across refused local admission")
		}
		fixture.forward(wire, fixture.receiverIn)
		if fixture.deliveredCount != 1 || sequence.returnReadPending.Load() || provider.CongestionDropStats().ReturnSendPacketCount != 0 {
			t.Fatal("local admission recovery did not deliver exactly once")
		}
		recovered := 0
		for _, observation := range completed {
			if observation.Err != nil {
				if observation.Err != ErrNoAckSendNotAdmitted || !observation.RecoveredByOwner || observation.OwnerTrackingOverflow {
					t.Fatalf("unrecovered local admission: %+v", observation)
				}
				recovered++
			}
		}
		if recovered == 0 {
			t.Fatal("test did not exercise a refused Pack followed by exact-owner recovery")
		}
	})
}
