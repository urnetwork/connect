package connect

import (
	"bytes"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Compare the actual packet's exact directional tuple. The unrelated DATA
// used to open the Transfer gap is observed separately from the tested flow.
func immediateParentHealthMatchesPath(t *testing.T, packet []byte, expected *IpPath) bool {
	t.Helper()
	var path IpPath
	if _, err := parseIpPathWithPayloadBorrowed(packet, &path); err != nil {
		t.Fatal("application received an invalid IP packet", err)
	}
	actualKey, actualOk := ipPacketFlowKeyFromPath(&path)
	expectedKey, expectedOk := ipPacketFlowKeyFromPath(expected)
	if !actualOk || !expectedOk {
		t.Fatal("application recorder lacks an exact directional tuple")
	}
	return actualKey == expectedKey
}

// A separate return stream supplies a real missing ordinal without replacing
// the admitted source channel's context or its ordinary parent receive path.
func immediateParentHealthWire(t *testing.T, exit *multiClientChannel, egress *IpPath) *immediateProgressFixture {
	t.Helper()
	return openImmediateParentHealthGap(t, exit, egress, newWindowRoundFixture(t, nil, nil))
}

func openImmediateParentHealthGap(t *testing.T, exit *multiClientChannel, egress *IpPath, wire *windowRoundFixture) *immediateProgressFixture {
	t.Helper()
	wire.receiver.AddReceiveCallback(exit.clientReceive)
	path := *egress.Reverse()
	path.SourcePort++
	fixture := &immediateProgressFixture{t: t, wire: wire, returnPath: &path}
	warm := wire.write(16)
	wire.forward(warm, wire.receiverIn)
	wire.acknowledge()
	sequence := uint32(1000)
	wire.frameFactory = func(size int) *protocol.Frame {
		packet := MessagePoolCopy(ipOosTcpPacketSequence(&path, tcpFlagAck|tcpFlagPsh,
			sequence, make([]byte, size)))
		sequence += uint32(size)
		frame, err := ipPacketFromProviderFrame(packet, 2)
		if err != nil {
			t.Fatal(err)
		}
		return frame
	}
	fixture.missing = wire.write(1240)
	fixture.base = fixture.missing.pack.SequenceNumber
	if wire.receiveSequence().nextSequenceNumber != fixture.base {
		t.Fatal("return DATA is not missing at the actual receive floor")
	}
	return fixture
}

// Source SYN admission, return-gap decoding and the real parent/collapse path
// remain connected. Application receipt is independent of exit-health credit.
func checkImmediateParentHealth(t *testing.T, batch bool) {
	t.Helper()
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		fixture := newAffinitySynQueueFixture(t, 4, nil)
		update := fixture.initial(t)
		exit := fixture.first.selected
		exit.clientReceivePacketCallback = fixture.parent.clientReceivePacket
		// The generic queue fixture uses a minimal stall-only channel. Real
		// channels obtain effective policy from this same parent hook.
		exit.reliabilitySettingsFunc = fixture.parent.ReliabilitySettings
		if !exit.reliabilitySettings().ProviderProbe {
			t.Fatal("parent default provider-probe policy is not enabled")
		}
		var delivered, repaired [][]byte
		var wire *immediateProgressFixture
		returnPath := fixture.path.ReverseValue()
		record := func(packet []byte) {
			if immediateParentHealthMatchesPath(t, packet, &returnPath) {
				delivered = append(delivered, bytes.Clone(packet))
			} else if wire != nil && immediateParentHealthMatchesPath(t, packet, wire.returnPath) {
				repaired = append(repaired, bytes.Clone(packet))
			} else {
				t.Fatal("application received an unowned return flow")
			}
		}
		// The batch API deliberately uses this callback for the first-answer
		// resolution path; its batch callback handles established flows.
		fixture.parent.SetReceivePacketCallback(func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
			record(packet)
		})
		if batch {
			exit.args.ReceivePackets = fixture.parent.clientReceivePackets
			fixture.parent.SetReceivePacketsCallback(func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packets [][]byte) {
				for _, packet := range packets {
					record(packet)
				}
			})
		}
		var qualificationRefreshes atomic.Int32
		exit.qualificationRefreshFunc = func(MultiHopId) { qualificationRefreshes.Add(1) }
		exit.setQuarantined(blackholeNoReceiveAck)
		exit.stateLock.Lock()
		exit.survivedQuarantine = true
		exit.quarantineLiftConnectSeen = false
		initialSuccesses := len(exit.connectSuccessTimes)
		exit.stateLock.Unlock()
		if initialSuccesses != 0 || update.receivedInbound.Load() {
			t.Fatal("source SYN already has a response or a connect-health receipt")
		}
		wire = immediateParentHealthWire(t, exit, fixture.path)
		synAck := reraceTcpPacket(fixture.path.Reverse(), tcpFlagSyn|tcpFlagAck,
			7000, fixture.path.SequenceNumber+1, nil)
		wire.sendControl(synAck, true)
		synctest.Wait()
		if len(delivered) != 1 || !bytes.Equal(delivered[0], synAck) ||
			!update.receivedInbound.Load() || update.client.Load() != exit || update.synGenerationAwaiting {
			t.Fatal("ahead-of-gap SYN-ACK lost application delivery or actual TCP ownership")
		}
		exit.stateLock.Lock()
		successes := len(exit.connectSuccessTimes)
		promotion := exit.quarantineLiftConnectSeen
		quarantined := exit.quarantined
		receiveAcks, receiveSyns := exit.packetStats.receiveAckCount, exit.packetStats.receiveSynCount
		receiveStamped := !exit.lastReceiveAckTime.IsZero()
		exit.stateLock.Unlock()
		if successes != 0 || promotion || !quarantined || receiveAcks != 0 || receiveSyns != 0 ||
			receiveStamped || qualificationRefreshes.Load() != 0 {
			t.Errorf("AHEAD_OF_GAP_PARENT_CONNECT_HEALTH: batch=%t successes=%d promotion=%t quarantined=%t receive_acks=%d receive_syns=%d receive_stamped=%t qualification=%d",
				batch, successes, promotion, quarantined, receiveAcks, receiveSyns, receiveStamped, qualificationRefreshes.Load())
		}

		wire.repair()
		synctest.Wait()
		expectedRepair := ipOosTcpPacketSequence(wire.returnPath, tcpFlagAck|tcpFlagPsh, 1000, make([]byte, 1240))
		if len(repaired) != 1 || !bytes.Equal(repaired[0], expectedRepair) {
			t.Fatal("actual gap repair did not deliver its exact distinct-flow DATA")
		}
		exit.stateLock.Lock()
		promotion, quarantined = exit.quarantineLiftConnectSeen, exit.quarantined
		exit.stateLock.Unlock()
		if promotion || quarantined {
			t.Fatal("actual receive progress did not clear quarantine and reset its promotion receipt")
		}
		ordered := reraceTcpPacket(fixture.path.Reverse(), tcpFlagAck|tcpFlagPsh,
			7001, fixture.path.SequenceNumber+1, []byte("ordered"))
		wire.sendControl(ordered, false)
		synctest.Wait()
		exit.stateLock.Lock()
		successes, promotion, quarantined = len(exit.connectSuccessTimes), exit.quarantineLiftConnectSeen, exit.quarantined
		receiveStamped = !exit.lastReceiveAckTime.IsZero()
		exit.stateLock.Unlock()
		if len(delivered) != 2 || !bytes.Equal(delivered[1], ordered) || successes != 1 ||
			!promotion || quarantined || !receiveStamped || qualificationRefreshes.Load() == 0 {
			t.Fatalf("same-flow ordered progress lost health: batch=%t delivered=%d successes=%d promotion=%t quarantined=%t receive_stamped=%t qualification=%d",
				batch, len(delivered), successes, promotion, quarantined, receiveStamped, qualificationRefreshes.Load())
		}
		// A second ordered packet on this generation must not repeat its health receipt.
		wire.sendControl(synAck, true)
		synctest.Wait()
		exit.stateLock.Lock()
		successes = len(exit.connectSuccessTimes)
		exit.stateLock.Unlock()
		if successes != 1 || len(delivered) != 3 {
			t.Fatal("same generation repeated connect-health credit or lost delivery", successes, len(delivered))
		}

		// Reuse the exact tuple with a new source generation, not a new flow.
		fixture.path.SequenceNumber += 1000
		freshSyn := reraceTcpPacket(fixture.path, tcpFlagSyn, fixture.path.SequenceNumber, 0, nil)
		if fixture.first.offer(freshSyn) != 1 {
			t.Fatal("same-tuple new source SYN was refused")
		}
		fixture.first.acknowledgeAll(freshSyn)
		fresh := fixture.parent.receiveUpdate(fixture.path)
		if fresh != update || fresh.receivedInbound.Load() || !fresh.synGenerationAwaiting {
			t.Fatal("new source SYN did not reset this same flow's response generation")
		}
		freshSynAck := reraceTcpPacket(fixture.path.Reverse(), tcpFlagSyn|tcpFlagAck,
			9000, fixture.path.SequenceNumber+1, nil)
		wire.sendControl(freshSynAck, true)
		synctest.Wait()
		exit.stateLock.Lock()
		successes = len(exit.connectSuccessTimes)
		exit.stateLock.Unlock()
		if !fresh.receivedInbound.Load() || fresh.synGenerationAwaiting || successes != 2 ||
			len(delivered) != 4 || !bytes.Equal(delivered[3], freshSynAck) {
			t.Fatal("same-tuple new generation lost its ordinary connect-health receipt", successes, len(delivered))
		}
	})
}

// A real first application SYN-ACK ahead of a gap delivers without health credit.
func TestImmediateApplicationSynAckDoesNotCreditParentHealth(t *testing.T) {
	checkImmediateParentHealth(t, false)
}

// The batch first-answer and established paths preserve the same distinction.
func TestImmediateApplicationSynAckBatchDoesNotCreditParentHealth(t *testing.T) {
	checkImmediateParentHealth(t, true)
}

// Keep the ACK frontier in a checksummed packet; the generic SYN ownership
// helper deliberately builds zero-ACK controls and cannot model this source DATA.
func immediateParentHealthSend(t *testing.T, parent *RemoteUserNatMultiClient, mode string, packet []byte) bool {
	t.Helper()
	packets := [][]byte{MessagePoolCopy(packet)}
	witnesses := groupTestPacketWitnesses(t, packets)
	defer requireGroupTestWitnessesReleased(t, packets, witnesses)
	source := SourceId(NewId())
	switch mode {
	case "batch":
		return parent.SendPacketBatch(source, protocol.ProvideMode_Network, packets, 0) == 1
	case "mux":
		mux := &IpMux{upstream: parent.SendPacket, upstreamGroupSend: parent.sendPacketGroup}
		return mux.SendPacketBatch(source, protocol.ProvideMode_Network, packets, 0) == 1
	default:
		accepted := parent.SendPacket(source, protocol.ProvideMode_Network, packets[0], 0)
		if !accepted {
			MessagePoolReturn(packets[0])
		}
		return accepted
	}
}

// A response may run inside the public source admission before its new SYN
// generation is published. Exercise both proof restorations without replacing
// the ordinary parent/collapse receive callback or setting protocol receipts.
func checkImmediateSynAdmissionHealth(t *testing.T, mode string) {
	t.Helper()
	GetMessagePoolAggregateStats()
	for _, scenario := range []string{"ordered", "ahead", "ahead-ordered-ahead", "ahead-advanced-ahead"} {
		synctest.Test(t, func(t *testing.T) {
			assertMessagePoolOwnership(t)
			path := icmpTcpTestPath(4)
			var wire *immediateProgressFixture
			var parent *RemoteUserNatMultiClient
			var update *multiClientChannelUpdate
			var exit *multiClientChannel
			var delivered, repaired [][]byte
			returnPath := path.ReverseValue()
			record := func(packet []byte) {
				if immediateParentHealthMatchesPath(t, packet, &returnPath) {
					delivered = append(delivered, bytes.Clone(packet))
				} else if wire != nil && immediateParentHealthMatchesPath(t, packet, wire.returnPath) {
					repaired = append(repaired, bytes.Clone(packet))
				} else {
					t.Fatal("inline application received an unowned return flow")
				}
			}
			readSuccesses := func() int {
				exit.stateLock.Lock()
				defer exit.stateLock.Unlock()
				return len(exit.connectSuccessTimes)
			}
			parent, update = defaultCollapseTestParent(t, path,
				func(group *parsedPacketGroup, _ time.Duration, ack bool) (bool, error) {
					if !ack {
						t.Fatal("source SYN lost reliable transfer ownership")
					}
					if group.ipPath.SequenceNumber == 200 {
						if group.admissionObservations == nil || update.synAdmissions == nil {
							t.Fatal("new SYN has no actual pending admission proof")
						}
						if scenario == "ordered" {
							wire.repair()
						}
						synAck := reraceTcpPacket(path.Reverse(), tcpFlagSyn|tcpFlagAck, 7000, 201, nil)
						wire.sendControl(synAck, true)
						synctest.Wait()
						want := 0
						if scenario == "ordered" {
							want = 1
						}
						if len(delivered) != 1 || !bytes.Equal(delivered[0], synAck) ||
							readSuccesses() != want || !update.receivedInbound.Load() {
							t.Errorf("SYN_ADMISSION_HEALTH: mode=%s scenario=%s delivered=%d successes=%d want=%d established=%t",
								mode, scenario, len(delivered), readSuccesses(), want, update.receivedInbound.Load())
						}
						if scenario == "ahead-ordered-ahead" || scenario == "ahead-advanced-ahead" {
							wire.repair()
							ack := uint32(201)
							if scenario == "ahead-advanced-ahead" {
								// The banner case needs no outgoing DATA; this separate
								// case advances a real admitted source byte frontier.
								payload := []byte("source bytes!")
								sourceData := reraceTcpPacket(path, tcpFlagAck|tcpFlagPsh, 201, 7001, payload)
								if !immediateParentHealthSend(t, parent, mode, sourceData) {
									t.Fatal("source DATA before SYN publication was refused")
								}
								ack += uint32(len(payload))
							}
							ordered := reraceTcpPacket(path.Reverse(), tcpFlagAck|tcpFlagPsh, 7001, ack, []byte("inline ordered"))
							wire.sendControl(ordered, false)
							synctest.Wait()
							if readSuccesses() != 1 || len(delivered) != 2 || !bytes.Equal(delivered[1], ordered) {
								t.Fatal("same-flow ordered DATA before publication lost its one health receipt")
							}
							// A newer pending proof snapshot must not erase that credit.
							// Immediate controls consumed real sequence ordinals outside
							// write's helper counter. Capture the next physical original.
							if accepted, err := wire.wire.send(1240); !accepted || err != nil {
								t.Fatal("next gap DATA was refused", accepted, err)
							}
							wire.missing = wire.wire.take(wire.wire.senderOut)
							if wire.missing.pack == nil || wire.missing.pack.Nack || wire.missing.pack.DeliverImmediately {
								t.Fatal("next gap DATA lacks its actual reliable ordinal")
							}
							wire.base = wire.missing.pack.SequenceNumber
							if wire.wire.receiveSequence().nextSequenceNumber != wire.base {
								t.Fatal("next actual DATA does not open a gap at the receive floor")
							}
							wire.sendControl(synAck, true)
							synctest.Wait()
							if len(delivered) != 3 || readSuccesses() != 1 {
								t.Fatal("later ahead-of-gap proof duplicated health or lost delivery")
							}
						}
					}
					for _, packet := range group.packets {
						MessagePoolReturn(packet.packet)
					}
					return true, nil
				})
			exit = update.client.Load()
			exit.packetStats = &clientWindowStats{log: NewNoopLogger()}
			exit.clientReceivePacketCallback = parent.clientReceivePacket
			parent.SetReceivePacketCallback(func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
				record(packet)
			})
			if mode == "batch" {
				exit.args.ReceivePackets = parent.clientReceivePackets
				parent.SetReceivePacketsCallback(func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packets [][]byte) {
					for _, packet := range packets {
						record(packet)
					}
				})
			}
			if !collapseOwnershipPublicSend(t, parent, mode, path, 100, tcpFlagSyn, nil) || update.receivedInbound.Load() {
				t.Fatal("initial source SYN did not establish the unresponded generation")
			}
			wire = immediateParentHealthWire(t, exit, path)
			if !collapseOwnershipPublicSend(t, parent, mode, path, 200, tcpFlagSyn, nil) ||
				!update.receivedInbound.Load() || update.synGenerationAwaiting || update.synGenerationNumber != 200 ||
				update.synAdmissions != nil {
				t.Fatal("source completion lost or leaked its exact inline SYN response proof")
			}
			if scenario != "ordered" {
				wire.repair()
			}
			synctest.Wait()
			wantRepairs := 1
			if scenario == "ahead-ordered-ahead" || scenario == "ahead-advanced-ahead" {
				wantRepairs = 2
			}
			if len(repaired) != wantRepairs {
				t.Fatal("inline gap repair lost a distinct-flow original", len(repaired), wantRepairs)
			}
			for index, packet := range repaired {
				expected := ipOosTcpPacketSequence(wire.returnPath, tcpFlagAck|tcpFlagPsh,
					1000+uint32(index*1240), make([]byte, 1240))
				if !bytes.Equal(packet, expected) {
					t.Fatal("inline gap repair changed a distinct-flow original", index)
				}
			}
			for i := 0; i < 2; i++ {
				ordered := reraceTcpPacket(path.Reverse(), tcpFlagAck|tcpFlagPsh,
					7100+uint32(i), 201, []byte("after publication"))
				wire.sendControl(ordered, false)
				synctest.Wait()
				if readSuccesses() != 1 {
					t.Fatalf("publication repeated or suppressed same-generation health: mode=%s scenario=%s round=%d successes=%d",
						mode, scenario, i, readSuccesses())
				}
			}
			// The prior generation's credited receipt must not leak to a new
			// source SYN on the SAME tuple, even on the same selected provider.
			if !collapseOwnershipPublicSend(t, parent, mode, path, 300, tcpFlagSyn, nil) ||
				update.receivedInbound.Load() || !update.synGenerationAwaiting {
				t.Fatal("same-tuple next SYN did not reset response ownership")
			}
			synAck := reraceTcpPacket(path.Reverse(), tcpFlagSyn|tcpFlagAck, 9000, 301, nil)
			wire.sendControl(synAck, true)
			synctest.Wait()
			if !update.receivedInbound.Load() || update.synGenerationAwaiting || readSuccesses() != 2 {
				t.Fatal("next generation lost or inherited connect-health credit", readSuccesses())
			}
		})
	}
}

// Scalar source completion preserves an inline response's exact health receipt.
func TestImmediateSynAdmissionPreservesParentHealthReceipt(t *testing.T) {
	checkImmediateSynAdmissionHealth(t, "singleton")
}

// Native batch publication preserves both deferred and already-earned health.
func TestImmediateSynAdmissionBatchPreservesParentHealthReceipt(t *testing.T) {
	checkImmediateSynAdmissionHealth(t, "batch")
}

// The mux group entry preserves the same source-generation boundary.
func TestImmediateSynAdmissionMuxPreservesParentHealthReceipt(t *testing.T) {
	checkImmediateSynAdmissionHealth(t, "mux")
}
