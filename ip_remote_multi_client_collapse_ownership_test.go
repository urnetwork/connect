package connect

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"unsafe"

	"github.com/urnetwork/connect/protocol"
)

func TestTcpCollapseStateLayout(t *testing.T) {
	t.Logf("update=%d parsedPacket=%d parsedPacketGroup=%d", unsafe.Sizeof(multiClientChannelUpdate{}), unsafe.Sizeof(parsedPacket{}), unsafe.Sizeof(parsedPacketGroup{}))
}

func collapseOwnershipPublicSend(t *testing.T, parent *RemoteUserNatMultiClient, mode string, path *IpPath, seq uint32, flags byte, payload []byte) bool {
	t.Helper()
	packet := MessagePoolCopy(ipOosTcpPacketSequence(path, flags, seq, payload))
	packets := [][]byte{packet}
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
		ok := parent.SendPacket(source, protocol.ProvideMode_Network, packet, 0)
		if !ok {
			MessagePoolReturn(packet)
		}
		return ok
	}
}

// The user's admission invariant: exactly the same seq/ACK/window/payload,
// with no SYN, flow reset, clock advance or intervening successful packet,
// must still be accepted immediately after selected-client queue refusal.
func TestTcpCollapseFailedAdmissionAllowsImmediateIdenticalRetry(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, mode := range []string{"singleton", "batch", "mux"} {
		t.Run(mode, func(t *testing.T) {
			parent, update, closeParent := groupTestParent(t, DisableSecurityPolicy())
			defer closeParent()
			parent.settings.TcpCollapsePrevention = true
			parent.settings.TcpCollapseMaxHold = 0
			calls, admit := 0, false
			update.client.Store(&multiClientChannel{ctx: parent.ctx, settings: parent.settings,
				sendGroupForTest: func(group *parsedPacketGroup, _ time.Duration, ack bool) (bool, error) {
					calls++
					if !ack {
						t.Fatal("TCP send did not require ACK")
					}
					if !admit {
						return false, nil
					}
					for _, packet := range group.packets {
						MessagePoolReturn(packet.packet)
					}
					return true, nil
				},
			})
			path, payload := icmpTcpTestPath(4), []byte("same TCP bytes")
			if collapseOwnershipPublicSend(t, parent, mode, path, 100, tcpFlagAck, payload) || calls != 1 {
				t.Fatalf("first attempt never passed gate or did not fail actual queue: calls=%d", calls)
			}
			admit = true
			if !collapseOwnershipPublicSend(t, parent, mode, path, 100, tcpFlagAck, payload) || calls != 2 {
				t.Fatalf("identical immediate retry did not pass gate and queue: calls=%d", calls)
			}
		})
	}
}

func TestTcpCollapseSynGenerationCommitsOnlyOnAdmission(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, mode := range []string{"singleton", "batch", "mux"} {
		for _, scenario := range []string{"same-isn-collapses", "new-isn-success-resets", "new-isn-refusal-preserves", "first-syn-refusal-retries"} {
			t.Run(mode+"/"+scenario, func(t *testing.T) {
				parent, update, closeParent := groupTestParent(t, DisableSecurityPolicy())
				defer closeParent()
				parent.settings.TcpCollapsePrevention = true
				parent.settings.TcpCollapseMaxHold = 0
				admit := scenario != "first-syn-refusal-retries"
				calls := 0
				update.client.Store(&multiClientChannel{ctx: parent.ctx, settings: parent.settings,
					sendGroupForTest: func(group *parsedPacketGroup, _ time.Duration, ack bool) (bool, error) {
						calls++
						if !ack {
							t.Fatal("SYN/data lost Transfer ACK")
						}
						if !admit {
							return false, nil
						}
						for _, packet := range group.packets {
							MessagePoolReturn(packet.packet)
						}
						return true, nil
					},
				})
				path := icmpTcpTestPath(4)
				send := func(seq uint32, flags byte, payload []byte) bool {
					return collapseOwnershipPublicSend(t, parent, mode, path, seq, flags, payload)
				}
				if scenario == "first-syn-refusal-retries" {
					if send(100, tcpFlagSyn, nil) || calls != 1 {
						t.Fatal("first SYN did not reach the refusing queue")
					}
					admit = true
					if !send(100, tcpFlagSyn, nil) || calls != 2 {
						t.Fatal("failed same-ISN SYN could not immediately retry")
					}
					return
				}
				if !send(100, tcpFlagSyn, nil) || !send(101, tcpFlagAck, []byte{1}) {
					t.Fatal("initial SYN/data admission failed")
				}
				if send(101, tcpFlagAck, []byte{1}) {
					t.Fatal("old-generation duplicate was not initially collapsed")
				}
				switch scenario {
				case "same-isn-collapses":
					before := calls
					if send(100, tcpFlagSyn, nil) || calls != before {
						t.Fatal("already-admitted same-ISN SYN bypassed gate and reset the generation")
					}
				case "new-isn-success-resets":
					if !send(50, tcpFlagSyn, nil) || !send(51, tcpFlagAck, []byte{1}) {
						t.Fatal("successful different-ISN SYN did not admit new data below the old committed frontier")
					}
				case "new-isn-refusal-preserves":
					admit = false
					before := calls
					if send(50, tcpFlagSyn, nil) || calls != before+1 {
						t.Fatal("different-ISN SYN did not pass gate then fail queue admission")
					}
					admit = true
					if send(101, tcpFlagAck, []byte{1}) {
						t.Fatal("failed different-ISN SYN erased committed old-flow collapse state")
					}
					if !send(50, tcpFlagSyn, nil) {
						t.Fatal("failed different-ISN SYN was not immediately retryable")
					}
				}
			})
		}
	}
}

// Real asynchronous pre-serialization expiry is a local refusal, not a peer
// failure. It must revoke the earlier successful queue-admission coverage or
// lifetime collapse would discard the inner sender's sole recovery attempt.
func TestTcpCollapseUnwrittenExpiryAllowsImmediateRetry(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, hold := range []time.Duration{0, 1500 * time.Millisecond} {
		t.Run(hold.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				destination := NewId()
				startSequence := make(chan struct{})
				settings := DefaultClientSettings()
				settings.Log = NewNoopLogger()
				settings.EncryptionSettings.Mode = EncryptionModeOff
				settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
				settings.SendBufferSettings.beforeRunSendSequenceForTest = func(id sendSequenceId) {
					if id.Destination == destination {
						select {
						case <-startSequence:
						case <-ctx.Done():
						}
					}
				}
				client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
				route := make(Route, 4)
				defer func() {
					cancel()
					client.CloseAndWait(context.Background())
					for len(route) > 0 {
						MessagePoolReturn(<-route)
					}
				}()
				client.ContractManager().AddNoContractPeer(destination)
				client.RouteManager().UpdateTransport(NewSendClientTransport(DestinationId(destination)), []Route{route})
				parent, update, closeParent := groupTestParent(t, DisableSecurityPolicy())
				defer closeParent()
				parent.settings.TcpCollapsePrevention = true
				parent.settings.TcpCollapseMaxHold = hold
				selected := newPacketTransferTestChannel()
				selected.ctx, selected.client = ctx, client
				selected.args = &multiClientChannelArgs{Destination: RequireMultiHopId(destination)}
				// The public entry point currently routes even one packet through
				// logical-group admission, which has no per-Pack caller deadline.
				// Retain its real collapse/commit path but exercise the actual
				// singleton Transfer admission at the existing selected-client seam.
				// No completion result or expiry is injected.
				selected.sendGroupForTest = func(group *parsedPacketGroup, timeout time.Duration, ack bool) (bool, error) {
					if len(group.packets) != 1 {
						t.Fatal("expiry fixture requires exactly one actual singleton Pack")
					}
					return selected.SendDetailedWithAck(&group.packets[0], timeout, ack)
				}
				update.client.Store(selected)
				path := icmpTcpTestPath(4)
				send := func() bool {
					packet := MessagePoolCopy(ipOosTcpPacketSequence(path, tcpFlagAck, 100, []byte("same TCP byte range")))
					ok := parent.SendPacket(SourceId(NewId()), protocol.ProvideMode_Network, packet, 100*time.Millisecond)
					if !ok {
						MessagePoolReturn(packet)
					}
					return ok
				}
				if !send() {
					t.Fatal("initial Pack was not admitted")
				}
				synctest.Wait()
				time.Sleep(200 * time.Millisecond)
				close(startSequence)
				synctest.Wait()
				if client.ReceiveStats().SendPackDeadlineDropCount != 1 || len(route) != 0 {
					t.Fatal("did not reproduce actual unwritten expiry")
				}
				stats, err := selected.WindowStats()
				if err != nil || stats.sendNackCount != 0 {
					t.Fatalf("expired local owner remained pending or poisoned client: %+v / %v", stats, err)
				}
				if !send() {
					t.Fatal("exact unwritten expiry left false collapse coverage; identical immediate retry was discarded")
				}
				synctest.Wait()
				if len(route) != 1 {
					t.Fatalf("recovered retry writes=%d, want one", len(route))
				}
				wire := <-route
				pack := decodeSendPackLifecycleWirePack(t, wire)
				MessagePoolReturn(wire)
				acknowledgeSendPackLifecycleWirePack(t, client, destination, pack)
				synctest.Wait()
			})
		})
	}
}

// Uses the real public parse/group/collapse/admission/commit path. Only the
// final selected-client queue admission is controlled; no state is injected
// into the collapse high-water counters.
func TestTcpCollapseRefusedLowerHoleIsNotCoveredByLaterAdmission(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, mode := range []string{"singleton", "batch", "mux"} {
		for _, hold := range []time.Duration{0, 1500 * time.Millisecond} {
			t.Run(fmt.Sprintf("%s/hold=%s", mode, hold), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					parent, update, closeParent := groupTestParent(t, DisableSecurityPolicy())
					defer closeParent()
					parent.settings.TcpCollapsePrevention = true
					parent.settings.TcpCollapseMaxHold = hold
					refuseLower := true
					var admitted []uint32
					selected := &multiClientChannel{
						ctx: parent.ctx, settings: parent.settings,
						sendGroupForTest: func(group *parsedPacketGroup, _ time.Duration, ack bool) (bool, error) {
							if !ack {
								t.Fatal("TCP queue admission lost Transfer ACK ownership")
							}
							if group.ipPath.SequenceNumber == 100 && refuseLower {
								return false, nil
							}
							for _, packet := range group.packets {
								admitted = append(admitted, packet.ipPath.SequenceNumber)
								MessagePoolReturn(packet.packet)
							}
							return true, nil
						},
					}
					update.client.Store(selected)
					path := &IpPath{Version: 4, Protocol: IpProtocolTcp,
						SourceIp: net.IPv4(198, 51, 100, 10), SourcePort: 32100,
						DestinationIp: net.IPv4(203, 0, 113, 20), DestinationPort: 443}
					mux := &IpMux{upstream: parent.SendPacket, upstreamGroupSend: parent.sendPacketGroup}
					source := SourceId(NewId())
					send := func(sequence uint32) bool {
						packet := MessagePoolCopy(ipOosTcpPacketSequence(path, tcpFlagAck, sequence, []byte{1}))
						packets := [][]byte{packet}
						witnesses := groupTestPacketWitnesses(t, packets)
						defer requireGroupTestWitnessesReleased(t, packets, witnesses)
						switch mode {
						case "batch":
							return parent.SendPacketBatch(source, protocol.ProvideMode_Network, packets, 0) == 1
						case "mux":
							return mux.SendPacketBatch(source, protocol.ProvideMode_Network, packets, 0) == 1
						default:
							ok := parent.SendPacket(source, protocol.ProvideMode_Network, packet, 0)
							if !ok {
								MessagePoolReturn(packet)
							}
							return ok
						}
					}
					if send(100) || update.sequencePacketCount != 0 {
						t.Fatal("failed first admission advanced collapse state")
					}
					if !send(200) {
						t.Fatal("higher packet was not admitted")
					}
					refuseLower = false
					if !send(100) {
						t.Fatalf("never-admitted lower TCP hole was collapsed: admitted=%v highWater=%d collapsed=%d hold=%s", admitted, update.sequenceNumber, parent.TcpCollapseDropCount(), hold)
					}
					if len(admitted) != 2 || admitted[0] != 200 || admitted[1] != 100 {
						t.Fatalf("admission lineage=%v, want higher then recovered lower", admitted)
					}
				})
			})
		}
	}
}

// The hold's escape is an admission opportunity, not a successful send. A
// refusing selected queue must not consume it and suppress the immediate retry.
func TestTcpCollapseFailedEscapeDoesNotCommitHold(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		parent, update, closeParent := groupTestParent(t, DisableSecurityPolicy())
		defer closeParent()
		parent.settings.TcpCollapsePrevention = true
		parent.settings.TcpCollapseMaxHold = 1500 * time.Millisecond
		admit := true
		update.client.Store(&multiClientChannel{
			ctx: parent.ctx, settings: parent.settings,
			sendGroupForTest: func(group *parsedPacketGroup, _ time.Duration, ack bool) (bool, error) {
				if !ack {
					t.Fatal("TCP was not ACK-required")
				}
				if !admit {
					return false, nil
				}
				for _, packet := range group.packets {
					MessagePoolReturn(packet.packet)
				}
				return true, nil
			},
		})
		path := icmpTcpTestPath(4)
		send := func() bool {
			packet := MessagePoolCopy(ipOosTcpPacketSequence(path, tcpFlagAck, 100, []byte{1}))
			ok := parent.SendPacket(SourceId(NewId()), protocol.ProvideMode_Network, packet, 0)
			if !ok {
				MessagePoolReturn(packet)
			}
			return ok
		}
		if !send() {
			t.Fatal("initial admission failed")
		}
		time.Sleep(3 * time.Second)
		admit = false
		if send() {
			t.Fatal("refused escape reported success")
		}
		admit = true
		if !send() {
			t.Fatal("failed queue admission consumed the hold escape and collapsed the only retry")
		}
	})
}

func TestTcpCollapseCoverageDoesNotInventHoles(t *testing.T) {
	client, update := collapseTestClient(0)
	defer update.Close()
	data := collapseTestPacket(100, 5000, 10, false, false)
	update.updateSequence(data)
	control := collapseTestPacket(200, 6000, 0, false, false)
	control.ipPath.TcpWindowSize = 0
	if !collapseTestCanSend(client, update, control) {
		t.Fatal("ACK/window progress was suppressed by a data interval")
	}
	update.updateSequence(control)
	if collapseTestCanSend(client, update, control) {
		t.Fatal("identical pure control was not covered")
	}
	covered := collapseTestPacket(100, 6000, 10, false, false)
	covered.ipPath.TcpWindowSize = 0
	if collapseTestCanSend(client, update, covered) {
		t.Fatal("pure ACK erased independently retained data coverage")
	}
	for _, sequence := range []uint32{90, 150, 200} {
		hole := collapseTestPacket(sequence, 6000, 1, false, false)
		hole.ipPath.TcpWindowSize = 0
		if !collapseTestCanSend(client, update, hole) {
			t.Fatalf("pure ACK invented data coverage at %d", sequence)
		}
	}
	control.ipPath.TcpWindowSize = 32768
	if !collapseTestCanSend(client, update, control) {
		t.Fatal("zero-window reopen was suppressed")
	}
	update.updateSequence(control)
	if collapseTestCanSend(client, update, control) {
		t.Fatal("reopened window did not commit")
	}
	// A disjoint admission cannot bridge the unknown middle. The bounded
	// one-interval proof forgets the older accepted range conservatively.
	later := collapseTestPacket(300, 6000, 10, false, false)
	update.updateSequence(later)
	for _, sequence := range []uint32{100, 250} {
		if !collapseTestCanSend(client, update, collapseTestPacket(sequence, 6000, 1, false, false)) {
			t.Fatalf("disjoint admission falsely covered %d", sequence)
		}
	}
}

func TestTcpCollapseCoverageWrapAndFin(t *testing.T) {
	client, update := collapseTestClient(0)
	defer update.Close()
	first := collapseTestPacket(^uint32(0)-3, 5000, 8, false, false)
	update.updateSequence(first) // [fffffffc, 4)
	for _, sequence := range []uint32{^uint32(0) - 2, 0, 3} {
		if collapseTestCanSend(client, update, collapseTestPacket(sequence, 5000, 1, false, false)) {
			t.Fatalf("wrapped covered byte %x escaped", sequence)
		}
	}
	for _, sequence := range []uint32{^uint32(0) - 4, 4, 0x80000000} {
		if !collapseTestCanSend(client, update, collapseTestPacket(sequence, 5000, 1, false, false)) {
			t.Fatalf("wrapped hole %x was collapsed", sequence)
		}
	}
	fin := collapseTestPacket(4, 5000, 0, false, false)
	fin.ipPath.Fin = true
	if !collapseTestCanSend(client, update, fin) {
		t.Fatal("FIN at the wrapped right edge was collapsed")
	}
	update.updateSequence(fin)
	if update.sequenceNumber != 5 || collapseTestCanSend(client, update, fin) {
		t.Fatal("FIN's independent sequence byte did not commit")
	}
}

// The real expiry test above covers completion after commit. This isolates the
// opposite legal ordering: a SendSequence can complete its admission before
// the public queue call returns. That late return must not recreate the hole.
func TestTcpCollapseUnwrittenCompletionBeforeCommit(t *testing.T) {
	assertMessagePoolOwnership(t)
	parent, update, closeParent := groupTestParent(t, DisableSecurityPolicy())
	defer closeParent()
	parent.settings.TcpCollapsePrevention, parent.settings.TcpCollapseMaxHold = true, 0
	selected := newPacketTransferTestChannel()
	selected.ctx, selected.settings = parent.ctx, parent.settings
	selected.sendTransferForTest = func(completed func(error)) (bool, error) {
		completed(errSendPackExpiredUnwritten)
		return true, nil
	}
	calls := 0
	selected.sendGroupForTest = func(group *parsedPacketGroup, timeout time.Duration, ack bool) (bool, error) {
		calls++
		ok, err := selected.SendDetailedWithAck(&group.packets[0], timeout, ack)
		if ok {
			MessagePoolReturn(group.packets[0].packet)
		}
		return ok, err
	}
	update.client.Store(selected)
	for range 2 {
		if !collapseOwnershipPublicSend(t, parent, "singleton", icmpTcpTestPath(4), 100, tcpFlagAck, []byte{1}) {
			t.Fatal("callback-before-return recreated false coverage")
		}
	}
	if calls != 2 || update.sequenceCovered || update.sequenceSynSeen {
		t.Fatalf("calls=%d covered=%t SYN=%t", calls, update.sequenceCovered, update.sequenceSynSeen)
	}
}

func TestTcpCollapseStaleCommitAndProviderRebindFailOpen(t *testing.T) {
	client, update := collapseTestClient(0)
	defer update.Close()
	firstClient := &multiClientChannel{settings: client.settings}
	update.client.Store(firstClient)
	group := func(packet *parsedPacket) *parsedPacketGroup {
		g := &parsedPacketGroup{packets: []parsedPacket{*packet}, ipPath: packet.ipPath}
		g.prepareCollapseAdmission(update)
		return g
	}
	old := group(collapseTestPacket(100, 5000, 10, false, false))
	newSyn := group(collapseTestPacket(50, 0, 0, true, false))
	update.commitSequenceGroupForClient(newSyn, firstClient)
	update.commitSequenceGroupForClient(old, firstClient)
	if update.sequenceNumber != 51 || update.sequenceCoveredFrom != 50 || update.sequenceCoveredTo != 51 {
		t.Fatal("stale old-generation send overwrote a committed new SYN")
	}
	newData := group(collapseTestPacket(51, 5000, 1, false, false))
	update.commitSequenceGroupForClient(newData, firstClient)
	if client.canSendPacket(&newData.packets[0], update, firstClient) {
		t.Fatal("committed original provider has no coverage")
	}
	replacement := &multiClientChannel{settings: client.settings}
	update.client.Store(replacement)
	if !client.canSendPacket(&newData.packets[0], update, replacement) {
		t.Fatal("replacement inherited the old provider's byte ownership")
	}
	stale := group(collapseTestPacket(52, 5000, 1, false, false))
	update.commitSequenceGroupForClient(stale, firstClient)
	if !client.canSendPacket(&stale.packets[0], update, replacement) {
		t.Fatal("late old-provider success covered a replacement's hole")
	}
}

// One sender's blocked then refused admission must not remove another
// sender's successful ownership. No lock is held over the queue call; two
// concurrent offers are conservatively allowed until one actually commits.
func TestTcpCollapseConcurrentRefusalPreservesSuccessfulAdmission(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		parent, update, closeParent := groupTestParent(t, DisableSecurityPolicy())
		defer closeParent()
		parent.settings.TcpCollapsePrevention, parent.settings.TcpCollapseMaxHold = true, 0
		path := icmpTcpTestPath(4)
		update.ipPath = path
		parent.sendClientPathForTest = func(_ *IpPath, _ flowPin, callback func(*multiClientChannelUpdate, *multiClientChannel)) {
			callback(update, update.client.Load())
		}
		entered, release := make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		selected := &multiClientChannel{ctx: parent.ctx, settings: parent.settings}
		selected.sendGroupForTest = func(group *parsedPacketGroup, _ time.Duration, _ bool) (bool, error) {
			if calls.Add(1) == 1 {
				close(entered)
				<-release
				return false, nil
			}
			for _, packet := range group.packets {
				MessagePoolReturn(packet.packet)
			}
			return true, nil
		}
		update.client.Store(selected)
		send := func() bool {
			return collapseOwnershipPublicSend(t, parent, "singleton", path, 100, tcpFlagAck, []byte{1})
		}
		first := make(chan bool, 1)
		go func() { first <- send() }()
		<-entered
		if !send() {
			t.Fatal("an uncommitted concurrent offer suppressed the real admission")
		}
		close(release)
		if <-first {
			t.Fatal("refused queue claimed success")
		}
		if send() || calls.Load() != 2 {
			t.Fatal("late refusal erased the peer sender's successful coverage")
		}
	})
}
