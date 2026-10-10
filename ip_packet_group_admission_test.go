// Adjacent caller and snapshot contracts for same-batch ready admission.
package connect

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Scheduler-only fixtures carry canonical scalar flow identity, not packets.
func nativeBatchAdmissionGroup(port int) *ipPacketGroup {
	return &ipPacketGroup{ipPath: &IpPath{
		Version: 4, Protocol: IpProtocolUdp,
		SourceIp: net.IPv4(192, 0, 2, 1), DestinationIp: net.IPv4(198, 51, 100, 2),
		SourcePort: port, DestinationPort: 443,
	}}
}

// The first visit starts each independent group's own budget, not a fresh
// second deadline after an earlier group has consumed that budget.
func TestNativeBatchRemainingTimeoutIsPerFirstOffer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first, second := nativeBatchAdmissionGroup(41001), nativeBatchAdmissionGroup(41002)
		var calls []time.Duration
		completed := 0
		start := time.Now()
		batch := ipPacketGroupBatch{
			timeout: 5 * time.Second, readyPass: true,
			send: func(group *ipPacketGroup, timeout time.Duration) bool {
				calls = append(calls, timeout)
				if group == first && 0 < timeout {
					<-time.After(timeout)
				}
				return false
			},
			complete: func(_ *ipPacketGroup, success bool) {
				if success {
					t.Error("refused scheduler fixture reported success")
				}
				completed++
			},
		}
		batch.offer(first)
		batch.offer(second)
		batch.finishPending()
		if time.Since(start) != 5*time.Second || completed != 2 || len(calls) != 4 ||
			calls[0] != 0 || calls[1] != 0 || calls[2] != 5*time.Second || calls[3] != 0 ||
			first.batchAdmission != nil || second.batchAdmission != nil {
			t.Fatalf("per-group budgets/completion changed: %v, %d", calls, completed)
		}
	})
}

// A later split of one flow has not been visited until its predecessor ends;
// it keeps the serial per-group budget instead of overtaking or expiring early.
func TestNativeBatchSameFlowDefersItsFirstOffer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first, second := nativeBatchAdmissionGroup(41001), nativeBatchAdmissionGroup(41001)
		var calls []*ipPacketGroup
		var budgets []time.Duration
		batch := ipPacketGroupBatch{
			timeout: 5 * time.Second, readyPass: true,
			send: func(group *ipPacketGroup, timeout time.Duration) bool {
				calls = append(calls, group)
				budgets = append(budgets, timeout)
				if 0 < timeout {
					<-time.After(timeout)
				}
				return false
			},
			complete: func(*ipPacketGroup, bool) {},
		}
		start := time.Now()
		batch.offer(first)
		batch.offer(second)
		if len(calls) != 1 {
			t.Fatal("same-flow suffix received an early offer")
		}
		batch.finishPending()
		if time.Since(start) != 10*time.Second || len(calls) != 3 ||
			calls[0] != first || calls[1] != first || calls[2] != second ||
			budgets[0] != 0 || budgets[1] != 5*time.Second || budgets[2] != 5*time.Second {
			t.Fatal("same-flow order or the suffix's original budget changed")
		}
	})
}

// A terminal non-queue refusal has no continuation. This is distinct from
// readiness backpressure and cannot become an extra hard-error attempt.
func TestNativeBatchTerminalRefusalHasNoRetry(t *testing.T) {
	group := nativeBatchAdmissionGroup(41001)
	calls, completed := 0, 0
	batch := ipPacketGroupBatch{
		timeout: -1, readyPass: true,
		send: func(group *ipPacketGroup, _ time.Duration) bool {
			calls++
			group.batchAdmission.stop = true
			return false
		},
		complete: func(_ *ipPacketGroup, success bool) {
			if success {
				t.Error("terminal refusal was accepted")
			}
			completed++
		},
	}
	batch.offer(group)
	batch.finishPending()
	if calls != 1 || completed != 1 || group.batchAdmission != nil {
		t.Fatal("terminal refusal acquired a retry or retained a continuation")
	}
}

// The selection seam supplies only the exact synchronous transfer outcome;
// public grouping, policy, provider reset and caller ownership remain real.
// The caller starts pool ownership outside the fake-time bubble.
func testNativeBatchSelectedReadyError(t *testing.T, requiredEncryption bool) {
	t.Helper()
	parent, tcpUpdate, closeParent := groupTestParent(t, DisableSecurityPolicy())
	defer closeParent()
	parent.settings.TcpCollapsePrevention, parent.settings.TcpCollapseMaxHold = true, 0
	udpPath := &IpPath{Version: 4, Protocol: IpProtocolUdp,
		SourceIp: net.IPv4(192, 0, 2, 91), DestinationIp: net.IPv4(198, 51, 100, 92),
		SourcePort: 44001, DestinationPort: 443}
	udpUpdate := newMultiClientChannelUpdate(parent.ctx, udpPath)
	defer udpUpdate.Close()
	client := newPacketTransferTestChannel()
	client.ctx, client.settings = parent.ctx, parent.settings
	tcpUpdate.client.Store(client)
	udpUpdate.client.Store(client)
	parent.sendClientPathForTest = func(path *IpPath, _ flowPin, callback func(*multiClientChannelUpdate, *multiClientChannel)) {
		update := tcpUpdate
		if path.Protocol == IpProtocolUdp {
			update = udpUpdate
		}
		update.ipPath = path
		callback(update, update.client.Load())
	}
	tcpCalls, udpCalls, resets := 0, 0, 0
	parent.SetReceivePacketCallback(func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, _ []byte) { resets++ })
	client.sendGroupForTest = func(group *parsedPacketGroup, timeout time.Duration, ack bool) (bool, error) {
		if group.ipPath.Protocol == IpProtocolTcp {
			tcpCalls++
			if !ack {
				t.Error("readiness visit weakened TCP acknowledgement")
			}
			if timeout == 0 {
				if requiredEncryption {
					return false, ErrEncryptionRequiredNotEstablished
				}
				return false, errors.New("synthetic terminal selected-client error")
			}
			if timeout != 5*time.Second || tcpUpdate.client.Load() != client {
				t.Error("deferred encryption wait lost its original budget or selected owner")
			}
		} else {
			udpCalls++
			if ack {
				t.Error("bound UDP lost its default NoAck policy")
			}
		}
		for _, packet := range group.packets {
			MessagePoolReturn(packet.packet)
		}
		return true, nil
	}
	packets := [][]byte{
		MessagePoolCopy(groupRecoveryPacket(101, 500, 64, tcpFlagAck, []byte{0x22})),
		MessagePoolCopy(ipOosUdpPacket(udpPath, []byte{0x33})),
	}
	witnesses := groupTestPacketWitnesses(t, packets)
	defer requireGroupTestWitnessesReleased(t, packets, witnesses)
	accepted := make([]bool, 2)
	count := parent.SendPacketBatchWithResults(SourceId(NewId()), protocol.ProvideMode_Network, packets, 5*time.Second, accepted)
	if requiredEncryption {
		if count != 2 || !accepted[0] || !accepted[1] || tcpCalls != 2 || udpCalls != 1 || resets != 0 {
			t.Fatal("ready encryption refusal reset, duplicated or discarded its original owner")
		}
	} else if count != 1 || accepted[0] || !accepted[1] || tcpCalls != 1 || udpCalls != 1 || resets != 1 {
		t.Fatal("terminal selected-client error was retried or lost its ordinary reset")
	}
}

// Required encryption gets the original wait, not a readiness-induced reset.
func TestNativeBatchReadyEncryptionRefusalKeepsSelectedOwner(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) { testNativeBatchSelectedReadyError(t, true) })
}

// Genuine selected-client errors retain the original terminal/reset behavior.
func TestNativeBatchReadyHardErrorIsNotRetried(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) { testNativeBatchSelectedReadyError(t, false) })
}

// Owns original buffers until one direct write succeeds. The parsed target
// uses production completion accounting; only the source worker is not run.
func readyUdpGroupPack(t *testing.T, ctx context.Context, peer Id, templates ...[]byte) (*SendPack, *parsedPacketGroup, *bool) {
	t.Helper()
	packets := make([][]byte, len(templates))
	frames := make([]*protocol.Frame, len(templates))
	parsed := make([]parsedPacket, len(templates))
	witnesses := make([][]byte, len(templates))
	var group *parsedPacketGroup
	taken := new(bool)
	built := 0
	t.Cleanup(func() {
		if !*taken {
			for _, packet := range packets[:built] {
				MessagePoolReturn(packet)
			}
		}
		if group != nil {
			group.finishGroupOffer()
		}
		requireGroupTestWitnessesReleased(t, packets[:built], witnesses[:built])
	})
	for index, template := range templates {
		packets[index] = MessagePoolCopy(template)
		witnesses[index] = MessagePoolShareReadOnly(packets[index])
		built++
		parsed[index] = *groupRecoveryParsed(t, packets[index])
		frame, err := ipPacketToProviderFrame(packets[index], DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		if !frame.Raw {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatal("ready group fixture requires raw protocol frames")
		}
		frames[index] = frame
	}
	group = &parsedPacketGroup{ipPath: parsed[0].ipPath, packets: parsed, byteCount: MessageByteCount(frames)}
	selected := newPacketTransferTestChannel()
	selected.addSendGroup(group)
	selected.prepareGroupCompletion(group, false, 0)
	pack := &SendPack{Ctx: ctx, Destination: peer, TransferOptions: TransferOptions{Ack: false},
		Frames: frames, logicalGroup: true, ackTarget: group, schedulingKey: ipSendSchedulingKey(group.ipPath)}
	return pack, group, taken
}

// Whole-group allowance is reserved once and restored only for an unwritten
// attempt. A real full route cannot consume packet, callback or byte credit.
func TestNativeBatchNoAckGroupContractRollback(t *testing.T) {
	assertMessagePoolOwnership(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var client *Client
	t.Cleanup(func() {
		if client != nil {
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		}
	})
	path := &IpPath{Version: 4, Protocol: IpProtocolUdp,
		SourceIp: net.IPv4(192, 0, 2, 91), DestinationIp: net.IPv4(198, 51, 100, 92),
		SourcePort: 44001, DestinationPort: 443}
	first, second := ipOosUdpPacket(path, []byte{1}), ipOosUdpPacket(path, []byte{2})
	bytes := ByteCount(len(first) + len(second))
	h := newNoAckBudgetHarness(t, ctx, func(contract *sequenceContract) {
		contract.transferByteCount, contract.effectiveTransferByteCount = bytes, bytes
	})
	client = h.client
	t.Cleanup(h.sequence.applyNoAckFastPathAccounting)
	pack, group, taken := readyUdpGroupPack(t, ctx, h.destinationId, first, second)
	snapshot := h.sequence.readNoAckFastPath(pack)
	if snapshot == nil || snapshot.contract != h.contract || snapshot.remainingByteCount == nil ||
		MessageByteCount(pack.Frames) != bytes {
		t.Fatal("whole UDP group was not eligible for the published contract")
	}
	// This directly driven sequence has no Run owner. Its synchronous write
	// returns after group completion; only this test applies contract charges.
	type accountingState struct {
		remaining, reserved, applied         ByteCount
		acked, unacked                       ByteCount
		writers                              uint64
		pending                              int
		admissionHeld, completed, terminal    bool
		completedPackets, outstandingPackets int
		completedBytes, outstandingBytes     ByteCount
	}
	assertAccounting := func(phase string, want accountingState) {
		t.Helper()
		flags := group.completionFlags.Load()
		got := accountingState{
			remaining:     snapshot.remainingByteCount.Load(),
			reserved:      snapshot.reservedByteCount.Load(),
			applied:       snapshot.appliedByteCount,
			acked:         h.contract.ackedByteCount,
			unacked:       h.contract.unackedByteCount,
			writers:       h.contract.noAckWriterState.Load(),
			admissionHeld: pack.admission != nil,
			completed:     flags&groupCompletionDone != 0,
			terminal:      flags&groupCompletionTerminal != 0,
		}
		h.sequence.noAckFastPathAccountingMutex.Lock()
		got.pending = len(h.sequence.pendingNoAckFastPathAccounting)
		h.sequence.noAckFastPathAccountingMutex.Unlock()
		owner := group.completionClient
		owner.stateLock.Lock()
		got.completedPackets, got.outstandingPackets = owner.packetStats.sendAckCount, owner.packetStats.sendNackCount
		got.completedBytes, got.outstandingBytes = owner.packetStats.sendAckByteCount, owner.packetStats.sendNackByteCount
		owner.stateLock.Unlock()
		if got != want {
			t.Fatalf("%s accounting state: got %+v want %+v", phase, got, want)
		}
	}
	want := accountingState{remaining: bytes, outstandingPackets: 2, outstandingBytes: bytes}
	assertAccounting("before write", want)
	h.route <- MessagePoolCopy([]byte("synthetic occupied route"))
	*taken = h.sequence.writeNoAckFastPath(snapshot, pack)
	if *taken {
		t.Fatal("full route accepted the whole group")
	}
	assertAccounting("unwritten rollback", want)
	h.sequence.applyNoAckFastPathAccounting()
	assertAccounting("unwritten accounting", want)
	MessagePoolReturn(<-h.route)
	*taken = h.sequence.writeNoAckFastPath(snapshot, pack)
	if !*taken {
		t.Fatal("same whole group did not write after real route pressure cleared")
	}
	wire := <-h.route
	decoded := decodeSendPackLifecycleWirePack(t, wire)
	if !decoded.Nack || len(decoded.Frames) != 2 ||
		!bytesEqualNativeGroup(decoded.Frames, first, second) {
		MessagePoolReturn(wire)
		t.Fatal("contract-backed whole-group wire changed its members")
	}
	MessagePoolReturn(wire)
	want = accountingState{
		reserved: bytes, writers: 1, pending: 1, completed: true, terminal: true,
		completedPackets: 2, completedBytes: bytes,
	}
	assertAccounting("written before accounting", want)
	h.sequence.applyNoAckFastPathAccounting()
	// Reserved is cumulative successful attribution, not an outstanding lease.
	// Applying matches it without refunding spent contract headroom.
	want.applied, want.acked, want.writers, want.pending = bytes, bytes, 0, 0
	assertAccounting("settled accounting", want)
	h.sequence.applyNoAckFastPathAccounting()
	assertAccounting("second accounting", want)
}

// Compares before the caller releases its decoded wire representation.
func bytesEqualNativeGroup(frames []*protocol.Frame, templates ...[]byte) bool {
	if len(frames) != len(templates) {
		return false
	}
	for index, frame := range frames {
		if !bytes.Equal(frame.MessageBytes, templates[index]) {
			return false
		}
	}
	return true
}

// A captured old snapshot is not authority to spend a retired contract.
func TestNativeBatchNoAckGroupRetiredContractRefuses(t *testing.T) {
	assertMessagePoolOwnership(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var client *Client
	t.Cleanup(func() {
		if client != nil {
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		}
	})
	h := newNoAckBudgetHarness(t, ctx)
	client = h.client
	path := &IpPath{Version: 4, Protocol: IpProtocolUdp,
		SourceIp: net.IPv4(192, 0, 2, 91), DestinationIp: net.IPv4(198, 51, 100, 92),
		SourcePort: 44001, DestinationPort: 443}
	pack, group, taken := readyUdpGroupPack(t, ctx, h.destinationId, ipOosUdpPacket(path, []byte{1}))
	snapshot := h.sequence.readNoAckFastPath(pack)
	if snapshot == nil {
		t.Fatal("fixture did not capture its original contract")
	}
	next := newContractAheadTestContract(t, h.client, h.destinationId)
	h.sequence.setContract(next, h.sequence.contractMetadata().generation)
	*taken = h.sequence.writeNoAckFastPath(snapshot, pack)
	if *taken || len(h.route) != 0 || pack.admission != nil ||
		group.completionFlags.Load()&groupCompletionDone != 0 {
		t.Fatal("retired snapshot consumed a new whole-group owner")
	}
}

// The actual snapshot path refuses during establishment and seals the same
// caller-owned group once the existing explicit establishment seam completes.
func TestNativeBatchNoAckGroupEncryptionHoldThenSeal(t *testing.T) {
	assertMessagePoolOwnership(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var client *Client
	t.Cleanup(func() {
		if client != nil {
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		}
	})
	h, session := establishHoldFastPathHarness(t, ctx, 30*time.Second)
	client = h.client
	path := &IpPath{Version: 4, Protocol: IpProtocolUdp,
		SourceIp: net.IPv4(192, 0, 2, 91), DestinationIp: net.IPv4(198, 51, 100, 92),
		SourcePort: 44001, DestinationPort: 443}
	pack, group, taken := readyUdpGroupPack(t, ctx, h.destinationId, ipOosUdpPacket(path, []byte{1}))
	snapshot := h.sequence.readNoAckFastPath(pack)
	if snapshot == nil {
		t.Fatal("fixture did not publish its existing writer")
	}
	if h.sequence.writeNoAckFastPath(snapshot, pack) || len(h.route) != 0 ||
		pack.admission != nil || group.completionFlags.Load()&groupCompletionDone != 0 {
		t.Fatal("establishment hold consumed or exposed a plaintext group")
	}
	establishHoldSealer(t, session)()
	*taken = h.sequence.writeNoAckFastPath(snapshot, pack)
	if !*taken || !establishHoldTakeOne(t, h.route) {
		t.Fatal("established group did not use the actual encrypted wire envelope")
	}
	h.sequence.applyNoAckFastPathAccounting()
}

// A real constructor-owned prior generation makes the reused observation
// node necessary. The final queue seam refuses the ready attempt, then
// delivers a real synchronous SYN-ACK only after proving the list acyclic.
func testNativeBatchNewSynRetry(t *testing.T, mux bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		path := groupRecoveryParsed(t, groupRecoveryPacket(100, 0, 64, tcpFlagSyn, nil)).ipPath
		var parent *RemoteUserNatMultiClient
		var update *multiClientChannelUpdate
		var scope *sendPackAdmissionObservations
		newCalls, udpCalls := 0, 0
		parent, update = defaultCollapseTestParent(t, path,
			func(group *parsedPacketGroup, timeout time.Duration, ack bool) (bool, error) {
				if group.ipPath.Protocol == IpProtocolUdp {
					udpCalls++
					if ack {
						t.Error("bound UDP changed its default reliability")
					}
				} else {
					if !ack {
						t.Error("SYN retry lost Transfer acknowledgement")
					}
					if group.ipPath.SequenceNumber == 50 {
						newCalls++
						observations := group.admissionObservations
						if observations == nil || update.synAdmissions != &observations.synAdmission ||
							observations.synAdmission.next != nil || update.sequenceSynOffers != 1 {
							t.Error("cached SYN retry duplicated or cyclically linked its synchronous proof")
							return false, nil
						}
						if timeout == 0 {
							scope = observations
							return false, nil
						}
						if timeout != 5*time.Second || scope != observations {
							t.Error("SYN retry lost its original budget or observation owner")
							return false, nil
						}
						defaultCollapseDeliverSynAck(t, parent, update, update.client.Load(), 51)
					}
				}
				for _, packet := range group.packets {
					MessagePoolReturn(packet.packet)
				}
				return true, nil
			})
		// Keep real constructor reliability/lifetime wiring; reserved synthetic
		// bytes do not stand in for a complete application-protocol exchange.
		parent.securityPolicy = DisableSecurityPolicy()
		if !parent.settings.TcpCollapsePrevention || parent.ReliabilitySettings().TcpCollapseMaxHold != 0 {
			t.Fatal("constructor did not preserve default collapse policy")
		}
		if !collapseOwnershipPublicSend(t, parent, "batch", path, 100, tcpFlagSyn, nil) {
			t.Fatal("prior SYN generation was not admitted through the public entry")
		}
		defaultCollapseReceiveSynAck(t, parent, update)
		udpPath := &IpPath{Version: 4, Protocol: IpProtocolUdp,
			SourceIp: net.IPv4(192, 0, 2, 91), DestinationIp: net.IPv4(198, 51, 100, 92),
			SourcePort: 44001, DestinationPort: 443}
		udpUpdate, _, _ := parent.sendUpdate(udpPath, flowPin{})
		selected := update.client.Load()
		udpUpdate.client.Store(selected)
		parent.bindClientFlow(udpUpdate, selected)
		packets := [][]byte{
			MessagePoolCopy(ipOosTcpPacketSequence(path, tcpFlagSyn, 50, nil)),
			MessagePoolCopy(ipOosUdpPacket(udpPath, []byte{0x33})),
		}
		witnesses := groupTestPacketWitnesses(t, packets)
		defer requireGroupTestWitnessesReleased(t, packets, witnesses)
		source := SourceId(NewId())
		var count int
		if mux {
			entry := &IpMux{upstream: parent.SendPacket, upstreamGroupSend: parent.sendPacketGroup}
			count = entry.SendPacketBatch(source, protocol.ProvideMode_Network, packets, 5*time.Second)
		} else {
			accepted := make([]bool, 2)
			count = parent.SendPacketBatchWithResults(source, protocol.ProvideMode_Network, packets, 5*time.Second, accepted)
			if !accepted[0] || !accepted[1] {
				t.Error("SYN retry lost exact accepted membership")
			}
		}
		if count != 2 || newCalls != 2 || udpCalls != 1 || scope == nil ||
			scope.synAdmission != (tcpSynAdmission{}) || update.synAdmissions != nil ||
			update.sequenceSynOffers != 0 || update.synGenerationNumber != 50 ||
			!update.receivedInbound.Load() || update.synGenerationAwaiting {
			t.Fatal("SYN retry lost inline current-generation proof or left pending ownership")
		}
	})
}

// A deferred ready refusal must unlink its per-attempt SYN receipt.
func TestNativeBatchNewSynRetryKeepsOneProof(t *testing.T) {
	testNativeBatchNewSynRetry(t, false)
}

// Mux grouping owns the identical finite SYN retry/response boundary.
func TestIpMuxNewSynRetryKeepsOneProof(t *testing.T) {
	testNativeBatchNewSynRetry(t, true)
}
