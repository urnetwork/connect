package connect

import (
	"context"
	"encoding/binary"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Use the public constructor and its immutable reliability projection. Only
// the selected provider's final queue decision is controlled; parsing, flow
// lookup, collapse, commit, and flow retirement use their production paths.
func defaultCollapseTestParent(
	t *testing.T,
	path *IpPath,
	admit func(*parsedPacketGroup, time.Duration, bool) (bool, error),
) (*RemoteUserNatMultiClient, *multiClientChannelUpdate) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	parent := NewRemoteUserNatMultiClient(ctx, &testingEmptyMultiClientGenerator{}, nil,
		protocol.ProvideMode_Network, DefaultMultiClientSettings())
	t.Cleanup(func() {
		cancel()
		if err := parent.CloseAndWait(context.Background()); err != nil {
			t.Errorf("join default collapse client: %v", err)
		}
	})
	update, _, _ := parent.sendUpdate(path, flowPin{})
	client := &multiClientChannel{
		ctx: ctx, settings: parent.settings, sendGroupForTest: admit,
	}
	update.client.Store(client)
	parent.bindClientFlow(update, client)
	return parent, update
}

// Establish through the real provider return path, not by setting the
// receivedInbound guard directly. The borrowed ingress packet is synchronous.
func defaultCollapseReceiveSynAck(t *testing.T, parent *RemoteUserNatMultiClient, update *multiClientChannelUpdate) {
	t.Helper()
	defaultCollapseDeliverSynAck(t, parent, update, update.client.Load(), update.sequenceSynNumber+1)
	if !update.receivedInbound.Load() {
		t.Fatal("provider SYN-ACK did not establish the flow")
	}
}

func defaultCollapseDeliverSynAck(t *testing.T, parent *RemoteUserNatMultiClient, update *multiClientChannelUpdate, source *multiClientChannel, ack uint32) {
	t.Helper()
	delivered := 0
	parent.SetReceivePacketCallback(func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {
		delivered++
	})
	reverse := update.ipPath.Reverse()
	packet, tcp := ipTransportPacket(reverse, ipProtocolNumberTcp, TcpHeaderSizeWithoutExtensions)
	binary.BigEndian.PutUint16(tcp[0:2], uint16(reverse.SourcePort))
	binary.BigEndian.PutUint16(tcp[2:4], uint16(reverse.DestinationPort))
	binary.BigEndian.PutUint32(tcp[4:8], 900)
	binary.BigEndian.PutUint32(tcp[8:12], ack)
	tcp[12], tcp[13] = byte(TcpHeaderSizeWithoutExtensions/4)<<4, tcpFlagSyn|tcpFlagAck
	binary.BigEndian.PutUint16(tcp[14:16], 4096)
	binary.BigEndian.PutUint16(tcp[16:18], ipPathTransportChecksum(reverse, ipProtocolNumberTcp, tcp))
	ingress, err := ParseIpPath(packet)
	if err != nil {
		t.Fatal(err)
	}
	if parent.receivePacketsCallback.Load() != nil {
		parent.SetReceivePacketsCallback(func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packets [][]byte) {
			delivered += len(packets)
		})
		parent.clientReceivePackets(source, TransferPath{}, protocol.ProvideMode_Network,
			TransportTypeUnknown, []*IpPath{ingress}, [][]byte{packet})
	} else {
		parent.clientReceivePacket(source, TransferPath{}, protocol.ProvideMode_Network,
			TransportTypeUnknown, ingress, packet)
	}
	if delivered != 1 {
		t.Fatalf("provider SYN-ACK did not reach the application: delivered=%d", delivered)
	}
}

func TestTcpCollapseDefaultEffectiveLifetimeHold(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		parent, _ := defaultCollapseTestParent(t, icmpTcpTestPath(4), nil)
		if !parent.settings.TcpCollapsePrevention || parent.defaultReliabilitySettings == nil ||
			parent.ReliabilitySettings().TcpCollapseMaxHold != 0 {
			t.Fatalf("constructed default must retain collapse for the flow lifetime: prevention=%t effective_hold=%s",
				parent.settings.TcpCollapsePrevention, parent.ReliabilitySettings().TcpCollapseMaxHold)
		}
	})
}

// Elapsed time cannot turn already-owned bytes or a same-ISN SYN into a new
// admission. In particular, the former 1.5-second default must not reopen the
// gate at the inner TCP sender's 3-second retry. Time advances only in synctest.
func TestTcpCollapseDefaultSuppressesDelayedOwnedPackets(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{4, 6} {
		for _, mode := range []string{"singleton", "batch", "mux"} {
			for _, kind := range []string{"same-syn", "data"} {
				t.Run(fmt.Sprintf("ipv%d/%s/%s", version, mode, kind), func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						calls := 0
						path := icmpTcpTestPath(version)
						parent, update := defaultCollapseTestParent(t, path,
							func(group *parsedPacketGroup, _ time.Duration, ack bool) (bool, error) {
								calls++
								if !ack {
									t.Error("TCP admission lost Transfer ACK ownership")
								}
								for _, packet := range group.packets {
									MessagePoolReturn(packet.packet)
								}
								return true, nil
							})
						flags, payload := byte(tcpFlagAck), []byte("owned TCP bytes")
						if kind == "same-syn" {
							flags, payload = tcpFlagSyn, nil
						}
						send := func() bool {
							return collapseOwnershipPublicSend(t, parent, mode, path, 100, flags, payload)
						}
						if !send() || calls != 1 {
							t.Fatalf("initial packet was not admitted: queue_calls=%d", calls)
						}
						owner := update.client.Load()
						defaultCollapseReceiveSynAck(t, parent, update)
						if send() || calls != 1 {
							t.Fatal("immediate identical packet was not collapsed")
						}
						for _, elapsed := range []time.Duration{3 * time.Second, time.Minute} {
							time.Sleep(elapsed)
							drops := parent.tcpCollapseDropCount.Load()
							if send() || calls != 1 || update.client.Load() != owner ||
								parent.tcpCollapseDropCount.Load() != drops+1 {
								t.Fatalf("elapsed time reopened owned %s: queue_calls=%d effective_hold=%s after=%s",
									kind, calls, parent.ReliabilitySettings().TcpCollapseMaxHold, elapsed)
							}
						}
					})
				})
			}
		}
	}
}

func TestTcpCollapseDefaultAdmissionAndResetBoundaries(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{4, 6} {
		for _, mode := range []string{"singleton", "batch", "mux"} {
			t.Run(fmt.Sprintf("ipv%d/%s", version, mode), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					admit, calls := false, 0
					path := icmpTcpTestPath(version)
					queue := func(group *parsedPacketGroup, _ time.Duration, ack bool) (bool, error) {
						calls++
						if !ack {
							t.Error("TCP admission lost Transfer ACK ownership")
						}
						if !admit {
							return false, nil
						}
						for _, packet := range group.packets {
							MessagePoolReturn(packet.packet)
						}
						return true, nil
					}
					parent, update := defaultCollapseTestParent(t, path, queue)
					send := func(seq uint32, flags byte, payload []byte) bool {
						return collapseOwnershipPublicSend(t, parent, mode, path, seq, flags, payload)
					}
					if send(100, tcpFlagSyn, nil) || calls != 1 || update.sequencePacketCount != 0 {
						t.Fatal("refused initial SYN changed collapse ownership")
					}
					admit = true
					if !send(100, tcpFlagSyn, nil) || calls != 2 || !send(101, tcpFlagAck, []byte{1}) {
						t.Fatal("refused initial SYN could not immediately retry and own data")
					}
					admit = false
					beforeCalls, beforeTime := calls, update.sequenceTime
					if send(50, tcpFlagSyn, nil) || calls != beforeCalls+1 || !update.sequenceTime.Equal(beforeTime) {
						t.Fatal("refused different-ISN SYN did not preserve the committed gate")
					}
					admit = true
					if send(101, tcpFlagAck, []byte{1}) || calls != beforeCalls+1 {
						t.Fatal("refused different-ISN SYN erased old-generation ownership")
					}
					if !send(50, tcpFlagSyn, nil) || !send(51, tcpFlagAck, []byte{1}) {
						t.Fatal("admitted different-ISN SYN did not reset the old high-water generation")
					}
					if send(50, tcpFlagSyn, nil) || send(51, tcpFlagAck, []byte{1}) {
						t.Fatal("new generation did not retain its own SYN/data gate")
					}
					// Provider replacement cannot inherit another queue's ownership.
					replacement := &multiClientChannel{ctx: parent.ctx, settings: parent.settings, sendGroupForTest: queue}
					update.client.Store(replacement)
					parent.bindClientFlow(update, replacement)
					if !send(51, tcpFlagAck, []byte{1}) || send(51, tcpFlagAck, []byte{1}) {
						t.Fatal("replacement provider did not acquire independent collapse ownership")
					}
					// A successfully sent RST clears the actual flow through the
					// shared reaper; the same tuple must then get a clean gate.
					if !send(52, tcpFlagRst, nil) {
						t.Fatal("flow-clear RST was not admitted")
					}
					synctest.Wait()
					parent.stateLock.Lock()
					retained := parent.flowUpdates[update] || parent.clientUpdates[replacement][update]
					if version == 4 {
						retained = retained || parent.ip4PathUpdates[path.ToIp4Path()] == update
					} else {
						retained = retained || parent.ip6PathUpdates[path.ToIp6Path()] == update
					}
					parent.stateLock.Unlock()
					if retained {
						t.Fatal("reaper retained the canceled flow before fresh lookup")
					}
					fresh, _, _ := parent.sendUpdate(path, flowPin{})
					if !update.IsDone() || fresh == update || fresh.sequencePacketCount != 0 {
						t.Fatal("cleared flow retained its prior collapse generation")
					}
					fresh.client.Store(&multiClientChannel{ctx: parent.ctx, settings: parent.settings, sendGroupForTest: queue})
					if !send(51, tcpFlagAck, []byte{1}) || send(51, tcpFlagAck, []byte{1}) {
						t.Fatal("fresh flow did not acquire independent collapse ownership")
					}
				})
			})
		}
	}
}

// Collapse governs duplicate admission, not observation of a failed upstream
// connect. Silent SYN retries must still reach the guarded failure inference,
// then acquire fresh ownership only if the replacement actually accepts them.
func TestTcpCollapseLifetimeSynRecovery(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{4, 6} {
		for _, mode := range []string{"singleton", "batch", "mux"} {
			for _, scenario := range []string{"silent-default", "silent-lifetime-override", "replacement-refused", "established", "rerace-disabled", "stale-inference"} {
				t.Run(fmt.Sprintf("ipv%d/%s/%s", version, mode, scenario), func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						oldCalls, newCalls := 0, 0
						admitReplacement := scenario != "replacement-refused"
						path := icmpTcpTestPath(version)
						queue := func(calls *int, admit *bool) func(*parsedPacketGroup, time.Duration, bool) (bool, error) {
							return func(group *parsedPacketGroup, _ time.Duration, ack bool) (bool, error) {
								*calls++
								if !ack || len(group.packets) != 1 || !group.ipPath.Syn || group.ipPath.SequenceNumber != 100 {
									t.Fatal("recovery changed the exact ACK-required SYN")
								}
								if admit != nil && !*admit {
									return false, nil
								}
								MessagePoolReturn(group.packets[0].packet)
								return true, nil
							}
						}
						parent, update := defaultCollapseTestParent(t, path, queue(&oldCalls, nil))
						if scenario == "silent-lifetime-override" || scenario == "rerace-disabled" {
							reliability := *parent.ReliabilitySettings()
							if scenario == "silent-lifetime-override" {
								// This control exposes inference starvation independently
								// of the shipping default's former finite escape.
								reliability.TcpCollapseMaxHold = 0
							} else {
								reliability.DialFailureRerace = false
							}
							parent.SetReliabilitySettings(&reliability)
						}
						original := update.client.Load()
						replacement := &multiClientChannel{ctx: parent.ctx, settings: parent.settings,
							sendGroupForTest: queue(&newCalls, &admitReplacement)}
						parent.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
							return []*multiClientChannel{replacement}
						}
						send := func() bool {
							return collapseOwnershipPublicSend(t, parent, mode, path, 100, tcpFlagSyn, nil)
						}
						if !send() || oldCalls != 1 || newCalls != 0 {
							t.Fatal("initial SYN did not acquire only the original owner")
						}
						time.Sleep(time.Second)
						if send() || oldCalls != 1 || newCalls != 0 || update.client.Load() != original {
							t.Fatal("early SYN retry was admitted or changed ownership")
						}
						time.Sleep(2 * time.Second)
						if scenario == "established" || scenario == "stale-inference" {
							if scenario == "stale-inference" {
								probe := *path
								probe.Syn = true
								if !update.synWaitExceeded(original, &probe, inferredDialFailureTimeout) {
									t.Fatal("setup did not produce a stale eligible inference")
								}
							}
							defaultCollapseReceiveSynAck(t, parent, update)
							// Model the legal interleaving where the real return lands
							// after eligibility but before guarded unbind. It must not
							// authorize a race or a duplicate old-owner admission.
							if scenario == "stale-inference" && parent.clientDialFailure(original, path) {
								t.Fatal("stale inferred failure unbound a now-established flow")
							}
						}
						if scenario == "established" || scenario == "rerace-disabled" || scenario == "stale-inference" {
							drops := parent.tcpCollapseDropCount.Load()
							if send() || oldCalls != 1 || newCalls != 0 || update.client.Load() != original ||
								parent.tcpCollapseDropCount.Load() != drops+1 || parent.reliabilityMetrics.flowsReraced.Load() != 0 {
								t.Fatal("ineligible failure reopened the gate or changed the live owner")
							}
							return
						}
						if scenario == "replacement-refused" {
							beforeTime := update.sequenceTime
							if send() || oldCalls != 1 || newCalls != 1 || update.client.Load() != nil ||
								!update.sequenceTime.Equal(beforeTime) || update.sequenceClient != original {
								t.Fatal("refused replacement claimed admission or changed committed collapse proof")
							}
							admitReplacement = true
						}
						if !send() || oldCalls != 1 || newCalls < 1 || update.client.Load() != replacement ||
							update.sequenceClient != replacement || parent.reliabilityMetrics.flowsReraced.Load() != 1 {
							t.Fatalf("silent retry did not acquire a fresh owner: old=%d replacement=%d reraced=%d",
								oldCalls, newCalls, parent.reliabilityMetrics.flowsReraced.Load())
						}
						committedCalls := newCalls
						if send() || oldCalls != 1 || newCalls != committedCalls {
							t.Fatal("replacement did not retain its newly accepted SYN")
						}
						parent.stateLock.Lock()
						oldBound, newBound := parent.clientUpdates[original][update], parent.clientUpdates[replacement][update]
						parent.stateLock.Unlock()
						if oldBound || !newBound {
							t.Fatal("recovery retained stale provider flow bookkeeping")
						}
					})
				})
			}
		}
	}
}

func TestTcpCollapseNewGenerationResponseOwnership(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{4, 6} {
		for _, mode := range []string{"singleton", "batch", "mux"} {
			for _, scenario := range []string{"silent", "refused", "inline-current", "inline-current-then-stale", "inline-current-prevention-off", "inline-wrapped", "stale-before-and-after", "wrapped-isn"} {
				t.Run(fmt.Sprintf("ipv%d/%s/%s", version, mode, scenario), func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						path := icmpTcpTestPath(version)
						oldCalls, newCalls := 0, 0
						newISN := uint32(50)
						if scenario == "wrapped-isn" || scenario == "inline-wrapped" {
							newISN = ^uint32(0)
						}
						answered := scenario == "inline-current" || scenario == "inline-current-then-stale" ||
							scenario == "inline-current-prevention-off" || scenario == "inline-wrapped"
						var parent *RemoteUserNatMultiClient
						var update *multiClientChannelUpdate
						parent, update = defaultCollapseTestParent(t, path,
							func(group *parsedPacketGroup, _ time.Duration, ack bool) (bool, error) {
								oldCalls++
								if !ack {
									t.Fatal("new generation lost Transfer recovery")
								}
								if group.ipPath.SequenceNumber == newISN {
									if scenario == "refused" {
										return false, nil
									}
									if answered {
										defaultCollapseDeliverSynAck(t, parent, update, update.client.Load(), newISN+1)
									}
									if scenario == "inline-current-then-stale" || scenario == "stale-before-and-after" {
										defaultCollapseDeliverSynAck(t, parent, update, update.client.Load(), 101)
									}
								}
								MessagePoolReturn(group.packets[0].packet)
								return true, nil
							})
						if mode == "batch" {
							parent.SetReceivePacketsCallback(func(TransferPath, protocol.ProvideMode, *IpPath, [][]byte) {})
						}
						if scenario == "inline-current-prevention-off" {
							parent.settings.TcpCollapsePrevention = false
						}
						original := update.client.Load()
						replacement := &multiClientChannel{ctx: parent.ctx, settings: parent.settings,
							sendGroupForTest: func(group *parsedPacketGroup, _ time.Duration, ack bool) (bool, error) {
								newCalls++
								if !ack || !group.ipPath.Syn || group.ipPath.SequenceNumber != newISN {
									t.Fatal("replacement did not acquire the exact new-generation SYN")
								}
								MessagePoolReturn(group.packets[0].packet)
								return true, nil
							}}
						parent.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
							return []*multiClientChannel{replacement}
						}
						send := func(seq uint32) bool {
							return collapseOwnershipPublicSend(t, parent, mode, path, seq, tcpFlagSyn, nil)
						}
						if !send(100) {
							t.Fatal("original generation was refused")
						}
						defaultCollapseReceiveSynAck(t, parent, update)
						beforeTime := update.sequenceTime
						accepted := send(newISN)
						if update.synAdmissions != nil {
							t.Fatal("completed new-generation admission retained a pending observation")
						}
						if scenario == "refused" {
							if accepted || !update.receivedInbound.Load() || update.sequenceSynNumber != 100 ||
								!update.sequenceTime.Equal(beforeTime) || oldCalls != 2 {
								t.Fatal("refused new generation changed the established original")
							}
							time.Sleep(3 * time.Second)
							if send(100) || oldCalls != 2 || newCalls != 0 || update.client.Load() != original {
								t.Fatal("refused new generation disturbed original retry suppression")
							}
							return
						}
						if !accepted || oldCalls != 2 || update.sequenceSynNumber != newISN {
							t.Fatal("different-ISN generation was not admitted")
						}
						if scenario == "stale-before-and-after" {
							defaultCollapseDeliverSynAck(t, parent, update, original, 101)
						}
						if update.receivedInbound.Load() != answered {
							t.Fatalf("new generation inherited or lost response ownership: established=%t want=%t",
								update.receivedInbound.Load(), answered)
						}
						time.Sleep(3 * time.Second)
						if scenario == "inline-current-prevention-off" {
							if !send(newISN) || oldCalls != 3 || newCalls != 0 || update.client.Load() != original {
								t.Fatal("disabled collapse lost inline response or changed its bypass policy")
							}
						} else if answered {
							if send(newISN) || oldCalls != 2 || newCalls != 0 || update.client.Load() != original {
								t.Fatal("inline current-generation reply was lost at admission commit")
							}
						} else if !send(newISN) || oldCalls != 2 || newCalls != 1 || update.client.Load() != replacement {
							t.Fatal("silent new generation inherited old establishment and could not rerace")
						}
					})
				})
			}
		}
	}
}

func TestTcpCollapseSynAdmissionConcurrentProofAndCleanup(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, refuseFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("refuse-first=%t", refuseFirst), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				path := icmpTcpTestPath(4)
				entered := make(chan *sendPackAdmissionObservations, 2)
				releaseFirst, releaseSecond := make(chan struct{}), make(chan struct{})
				firstReleased, secondReleased := false, false
				defer func() {
					if !firstReleased {
						close(releaseFirst)
					}
					if !secondReleased {
						close(releaseSecond)
					}
				}()
				parent, update := defaultCollapseTestParent(t, path,
					func(group *parsedPacketGroup, _ time.Duration, _ bool) (bool, error) {
						switch group.ipPath.SequenceNumber {
						case 50:
							entered <- group.admissionObservations
							<-releaseFirst
							if refuseFirst {
								return false, nil
							}
						case 60:
							entered <- group.admissionObservations
							<-releaseSecond
						}
						MessagePoolReturn(group.packets[0].packet)
						return true, nil
					})
				send := func(seq uint32) bool {
					return collapseOwnershipPublicSend(t, parent, "singleton", path, seq, tcpFlagSyn, nil)
				}
				if !send(100) {
					t.Fatal("original SYN refused")
				}
				defaultCollapseReceiveSynAck(t, parent, update)
				owner := update.client.Load()
				firstResult, secondResult := make(chan bool, 1), make(chan bool, 1)
				go func() { firstResult <- send(50) }()
				firstScope := <-entered
				go func() { secondResult <- send(60) }()
				secondScope := <-entered
				synctest.Wait()
				// Both exact proofs must survive responses in the opposite order,
				// with a stale old-generation response between them.
				defaultCollapseDeliverSynAck(t, parent, update, owner, 61)
				defaultCollapseDeliverSynAck(t, parent, update, owner, 101)
				defaultCollapseDeliverSynAck(t, parent, update, owner, 51)
				update.stateLock.Lock()
				count := 0
				for pending := update.synAdmissions; pending != nil; pending = pending.next {
					count++
					if pending.responseClient != owner {
						t.Error("concurrent offer lost its exact response owner")
					}
				}
				update.stateLock.Unlock()
				if count != 2 {
					t.Fatalf("pending observation count=%d, want exactly two live calls", count)
				}
				close(releaseFirst)
				firstReleased = true
				if accepted := <-firstResult; accepted == refuseFirst {
					t.Fatal("first queue result changed")
				}
				if firstScope.synAdmission != (tcpSynAdmission{}) {
					t.Fatal("completed first scope retains a flow/client/next registration")
				}
				close(releaseSecond)
				secondReleased = true
				if !<-secondResult {
					t.Fatal("second queue refused")
				}
				wantGeneration := uint32(50)
				if refuseFirst {
					wantGeneration = 60
				}
				if update.synAdmissions != nil || secondScope.synAdmission != (tcpSynAdmission{}) ||
					!update.receivedInbound.Load() || update.synGenerationNumber != wantGeneration {
					t.Fatal("concurrent refusal/stale commit leaked a receipt or erased the winner's inline reply")
				}
			})
		})
	}
}

func TestTcpCollapseSynAdmissionExceptionalCleanup(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, scenario := range []string{"refused", "canceled", "panic"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				path := icmpTcpTestPath(4)
				var scope *sendPackAdmissionObservations
				var parent *RemoteUserNatMultiClient
				var update *multiClientChannelUpdate
				parent, update = defaultCollapseTestParent(t, path,
					func(group *parsedPacketGroup, _ time.Duration, _ bool) (bool, error) {
						if group.ipPath.SequenceNumber == 50 {
							scope = group.admissionObservations
							if scenario == "panic" {
								panic("test admission exception")
							}
							if scenario == "canceled" {
								parent.cancel()
							}
							return false, nil
						}
						MessagePoolReturn(group.packets[0].packet)
						return true, nil
					})
				if !collapseOwnershipPublicSend(t, parent, "singleton", path, 100, tcpFlagSyn, nil) {
					t.Fatal("original SYN refused")
				}
				defaultCollapseReceiveSynAck(t, parent, update)
				func() {
					packet := MessagePoolCopy(ipOosTcpPacketSequence(path, tcpFlagSyn, 50, nil))
					defer MessagePoolReturn(packet) // the rejected/panicking caller retains this owner
					defer func() {
						if recovered := recover(); (recovered != nil) != (scenario == "panic") {
							t.Errorf("unexpected admission exception: %v", recovered)
						}
					}()
					if parent.SendPacket(SourceId(NewId()), protocol.ProvideMode_Network, packet, 0) {
						t.Error("exceptional queue admitted the new SYN")
					}
				}()
				if scope == nil || scope.synAdmission != (tcpSynAdmission{}) || update.synAdmissions != nil ||
					!update.receivedInbound.Load() || update.synGenerationNumber != 100 {
					t.Fatal("exceptional new-SYN admission retained a receipt or mutated the old generation")
				}
			})
		})
	}
}

func TestTcpCollapseCoverageInvalidationPreservesGeneration(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		path, calls := icmpTcpTestPath(4), 0
		parent, update := defaultCollapseTestParent(t, path,
			func(group *parsedPacketGroup, _ time.Duration, _ bool) (bool, error) {
				calls++
				MessagePoolReturn(group.packets[0].packet)
				return true, nil
			})
		send := func() bool { return collapseOwnershipPublicSend(t, parent, "singleton", path, 100, tcpFlagSyn, nil) }
		if !send() {
			t.Fatal("original SYN refused")
		}
		defaultCollapseReceiveSynAck(t, parent, update)
		// A later unwritten item invalidates conservative coverage of the
		// whole interval, not the identity of the already established SYN.
		(tcpCollapseAdmission{update: update, epoch: update.sequenceAdmissionEpoch}).complete(errSendPackExpiredUnwritten)
		if !send() || !update.receivedInbound.Load() || update.synGenerationNumber != 100 || update.synGenerationAwaiting {
			t.Fatal("coverage revocation turned a same-ISN retry into a fresh silent generation")
		}
		time.Sleep(3 * time.Second)
		if send() || calls != 2 {
			t.Fatal("restored same-generation ownership did not retain its lifetime gate")
		}
	})
}

func TestTcpCollapseOrderedMultiSynGroupGeneration(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, mode := range []string{"batch", "mux"} {
		for _, scenario := range []string{"first-response-only", "final-then-stale", "refused"} {
			t.Run(mode+"/"+scenario, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					path, calls := icmpTcpTestPath(4), 0
					var parent *RemoteUserNatMultiClient
					var update *multiClientChannelUpdate
					parent, update = defaultCollapseTestParent(t, path,
						func(group *parsedPacketGroup, _ time.Duration, ack bool) (bool, error) {
							calls++
							if !ack {
								t.Fatal("multi-SYN group lost Transfer ACK ownership")
							}
							if calls == 2 {
								if len(group.packets) != 2 || group.packets[0].ipPath.SequenceNumber != 50 || group.packets[1].ipPath.SequenceNumber != 60 {
									t.Fatal("public batching did not preserve the ordered two-SYN group")
								}
								if scenario == "refused" {
									return false, nil
								}
								if scenario == "final-then-stale" {
									defaultCollapseDeliverSynAck(t, parent, update, update.client.Load(), 61)
								}
								defaultCollapseDeliverSynAck(t, parent, update, update.client.Load(), 51)
							}
							for _, packet := range group.packets {
								MessagePoolReturn(packet.packet)
							}
							return true, nil
						})
					if !collapseOwnershipPublicSend(t, parent, mode, path, 100, tcpFlagSyn, nil) {
						t.Fatal("original SYN refused")
					}
					defaultCollapseReceiveSynAck(t, parent, update)
					packets := [][]byte{
						MessagePoolCopy(ipOosTcpPacketSequence(path, tcpFlagSyn, 50, nil)),
						MessagePoolCopy(ipOosTcpPacketSequence(path, tcpFlagSyn, 60, nil)),
					}
					witnesses := groupTestPacketWitnesses(t, packets)
					var accepted int
					if mode == "batch" {
						accepted = parent.SendPacketBatch(SourceId(NewId()), protocol.ProvideMode_Network, packets, 0)
					} else {
						mux := &IpMux{upstream: parent.SendPacket, upstreamGroupSend: parent.sendPacketGroup}
						accepted = mux.SendPacketBatch(SourceId(NewId()), protocol.ProvideMode_Network, packets, 0)
					}
					requireGroupTestWitnessesReleased(t, packets, witnesses)
					wantCount, wantGeneration, wantAnswered := 2, uint32(60), scenario == "final-then-stale"
					if scenario == "refused" {
						wantCount, wantGeneration, wantAnswered = 0, 100, true
					}
					if accepted != wantCount || calls != 2 || update.synAdmissions != nil ||
						update.synGenerationNumber != wantGeneration || update.receivedInbound.Load() != wantAnswered {
						t.Fatalf("ordered group committed wrong generation/proof: accepted=%d calls=%d generation=%d answered=%t",
							accepted, calls, update.synGenerationNumber, update.receivedInbound.Load())
					}
				})
			})
		}
	}
}
