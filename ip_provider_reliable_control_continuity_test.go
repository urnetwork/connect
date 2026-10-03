package connect

import (
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/protocol"
)

func reliableIngressControlItem(t *testing.T, path *IpPath, number uint64, flags byte, seq uint32) (*receiveItem, *protocol.Frame) {
	t.Helper()
	packet := MessagePoolCopy(ipOosTcpPacketSequence(path, flags, seq, nil))
	frame, err := ipPacketToProviderFrame(packet, DefaultProtocolVersion)
	if err != nil || !frame.Raw {
		MessagePoolReturn(packet)
		t.Fatalf("raw control fixture: frame=%+v err=%v", frame, err)
	}
	return &receiveItem{transferItem: transferItem{messageId: NewId(), sequenceNumber: number},
		ack: true, frames: []*protocol.Frame{frame}}, frame
}

func reliableIngressPackForPath(t *testing.T, f *reliableTcpIngressFixture, sequence *ReceiveSequence,
	path *IpPath, number uint64, id Id, flags byte, payload []byte) {
	t.Helper()
	packet := MessagePoolCopy(ipOosTcpPacketSequence(path, flags, 101, payload))
	frame, err := ipPacketToProviderFrame(packet, DefaultProtocolVersion)
	if err != nil || !frame.Raw {
		MessagePoolReturn(packet)
		t.Fatalf("raw path fixture: frame=%+v err=%v", frame, err)
	}
	pack := &ReceivePack{Source: f.source, SequenceId: sequence.sequenceId,
		Pack: &protocol.Pack{MessageId: id.Bytes(), SequenceId: sequence.sequenceId.Bytes(),
			SequenceNumber: number, Head: number == 0, Frames: []*protocol.Frame{frame}},
		ReceiveCallback: f.client.receiveCallback, Ctx: f.ctx, Unwrapped: true,
		EncryptionRole: sequenceTlsRoleServer, MessageByteCount: ByteCount(len(packet))}
	if accepted, err := sequence.Pack(pack, 0); !accepted || err != nil {
		pack.messagePoolReturn()
		t.Fatalf("path Pack admission: accepted=%t err=%v", accepted, err)
	}
}

// An empty late control for a locally absent flow has a terminal TCP outcome,
// not a congested data owner. Applying that outcome must not reform the shared
// Transfer sequence and strand a later, unrelated connection. The generated
// RFC reset and the exact input flags are asserted independently of the ACK.
func TestProviderReliableTcpOrphanControlPreservesNextFlow(t *testing.T) {
	for _, version := range []int{4, 6} {
		for _, budgeted := range []bool{false, true} {
			for _, control := range []struct {
				name  string
				flags byte
			}{{"ack", tcpFlagAck}, {"fin-ack", tcpFlagFin | tcpFlagAck}, {"fin", tcpFlagFin}} {
				t.Run(fmt.Sprintf("ipv%d/budget-%t/%s", version, budgeted, control.name), func(t *testing.T) {
					assertMessagePoolOwnership(t)
					synctest.Test(t, func(t *testing.T) {
						var budget *TransferMemoryBudget
						if budgeted {
							budget = NewTransferMemoryBudget(mib(2))
							t.Cleanup(func() {
								if budget.UsedByteCount() != 0 || budget.reservedByteCount.Load() != budget.releasedByteCount.Load() {
									t.Error("orphan control/next-flow leaked prepaid root ownership")
								}
							})
						}
						f := newReliableTcpIngressFixture(t, version, budget)
						f.establishForControl()
						sequence := f.runningReceiveSequence()
						orphan := *f.path
						orphan.SourcePort++
						type resetMetadata struct {
							valid, rst, ack bool
							seq, ackNumber  uint32
						}
						resets := make(chan resetMetadata, 4)
						remove := f.nat.AddReceivePacketCallback(func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, packet []byte) {
							var source, destination net.IP
							var transport []byte
							var valid bool
							if version == 4 {
								_, source, destination, transport, valid = parseIpv4(packet)
							} else {
								_, source, destination, transport, valid = parseIpv6(packet)
							}
							var tcp parsedTcp
							valid = valid && parseTcpPacket(source, destination, transport, &tcp)
							resets <- resetMetadata{valid, tcp.rst, tcp.ack, tcp.seq, tcp.ackNumber}
						})
						defer remove()
						id := NewId()
						reliableIngressPackForPath(t, f, sequence, &orphan, 0, id, control.flags, nil)
						synctest.Wait()
						select {
						case reset := <-resets:
							wantAck := control.flags&tcpFlagAck == 0
							wantAckNumber := uint32(0)
							if wantAck {
								wantAckNumber = 102 // FIN occupies one sequence number.
							}
							if !reset.valid || !reset.rst || reset.ack != wantAck || reset.seq != 0 || reset.ackNumber != wantAckNumber {
								t.Fatalf("orphan flags=%#x terminal reset=%+v", control.flags, reset)
							}
						default:
							t.Fatal("orphan TCP control never reached the local terminal-reset decision")
						}
						if sequence.ctx.Err() != nil {
							t.Fatalf("orphan flags=%#x applied reset but canceled shared Transfer lane: %v", control.flags, sequence.ctx.Err())
						}
						if present, head := reliableIngressCumulativeHead(sequence); !present || head.messageId != id {
							t.Fatal("terminal orphan control did not secure its exact Transfer receipt")
						}
						// Once secured, a duplicate Transfer message must not repeat
						// the local TCP action or interfere with another flow.
						reliableIngressPackForPath(t, f, sequence, &orphan, 0, id, control.flags, nil)
						synctest.Wait()
						if len(resets) != 0 {
							t.Fatal("secured duplicate repeated the downstream terminal action")
						}
						next := orphan
						next.SourcePort++
						nextID := NewId()
						reliableIngressPackForPath(t, f, sequence, &next, 1, nextID, tcpFlagSyn, nil)
						synctest.Wait()
						if present, head := reliableIngressCumulativeHead(sequence); !present || head.messageId != nextID {
							t.Fatal("orphan control prevented next flow's exact SYN admission")
						}
						if f.tcp.ctx.Err() != nil || len(f.tcp.sendItems) != 1 || f.dials.Load() != 0 {
							t.Fatal("orphan control disturbed the sibling flow or depended on a WAN ACK")
						}
					})
				})
			}
		}
	}
}

// A reset response is not permission to report undelivered application bytes
// as admitted. These payload-bearing orphan packets have no final TCP owner;
// neither an ACK flag nor a FIN may turn them into the control-only exception.
func TestProviderReliableTcpOrphanPayloadNeverAcked(t *testing.T) {
	for _, version := range []int{4, 6} {
		for _, flags := range []byte{tcpFlagAck, tcpFlagFin | tcpFlagAck} {
			t.Run(fmt.Sprintf("ipv%d/flags-%x", version, flags), func(t *testing.T) {
				assertMessagePoolOwnership(t)
				synctest.Test(t, func(t *testing.T) {
					f := newReliableTcpIngressFixture(t, version, nil)
					sequence := f.runningReceiveSequence()
					orphan := *f.path
					orphan.SourcePort++
					id := NewId()
					reliableIngressPackForPath(t, f, sequence, &orphan, 0, id, flags, []byte("not owned downstream"))
					synctest.Wait()
					if present, head := reliableIngressCumulativeHead(sequence); present {
						t.Errorf("orphan payload falsely cumulatively ACKed: head=%s item=%s", head.messageId, id)
					}
					if ack := sequence.ackWindow.Snapshot(false); ack.selectiveAcks[id].messageId == id {
						t.Error("orphan payload falsely selectively ACKed")
					}
					if f.tcp.ctx.Err() != nil || f.dials.Load() != 0 {
						t.Error("orphan payload rejection corrupted its live sibling")
					}
				})
			})
		}
	}
}

// Reset generation is an independently bounded policy. Disabling or rate
// limiting a regenerable reply must not turn an empty terminal control into
// a shared-lane ownership failure or bypass the configured response limit.
func TestProviderReliableTcpOrphanControlResetPolicy(t *testing.T) {
	for _, version := range []int{4, 6} {
		for _, policy := range []string{"disabled", "rate-limited"} {
			t.Run(fmt.Sprintf("ipv%d/%s", version, policy), func(t *testing.T) {
				assertMessagePoolOwnership(t)
				synctest.Test(t, func(t *testing.T) {
					f := newReliableTcpIngressFixture(t, version, nil)
					settings := f.nat.settings.TcpBufferSettings
					settings.EnableOrphanRst = policy != "disabled"
					settings.OrphanRstPerSecond = 1
					var resets atomic.Int64
					remove := f.nat.AddReceivePacketCallback(func(_ TransferPath, _ protocol.ProvideMode, _ *IpPath, _ []byte) { resets.Add(1) })
					defer remove()
					orphan := *f.path
					orphan.SourcePort++
					wantResets := int64(0)
					if policy == "rate-limited" {
						packet := MessagePoolCopy(ipOosTcpPacketSequence(&orphan, tcpFlagAck, 101, nil))
						if !f.nat.SendPacket(f.source, protocol.ProvideMode_Public, packet, 0) {
							MessagePoolReturn(packet)
							t.Fatal("could not prime exact buffer's response limit")
						}
						synctest.Wait()
						wantResets = 1
						if resets.Load() != wantResets {
							t.Fatal("response limit was not primed")
						}
					}
					sequence := f.runningReceiveSequence()
					id := NewId()
					reliableIngressPackForPath(t, f, sequence, &orphan, 0, id, tcpFlagFin|tcpFlagAck, nil)
					synctest.Wait()
					if sequence.ctx.Err() != nil {
						t.Fatalf("response policy %s canceled terminal control's shared lane", policy)
					}
					if present, head := reliableIngressCumulativeHead(sequence); !present || head.messageId != id {
						t.Error("local terminal decision did not secure exact input control")
					}
					if resets.Load() != wantResets {
						t.Errorf("terminal handling bypassed reset policy: got=%d want=%d", resets.Load(), wantResets)
					}
				})
			})
		}
	}
}

// Absent flow after an explicit source/provider/NAT retirement is not the
// active-owner orphan-control decision. The terminal-control exception must
// not produce successful receipt feedback once admission authority is gone.
func TestProviderReliableTcpOrphanControlAfterShutdownNeverAcked(t *testing.T) {
	for _, version := range []int{4, 6} {
		for _, stop := range []string{"source-retired", "provider-shutdown", "nat-shutdown"} {
			t.Run(fmt.Sprintf("ipv%d/%s", version, stop), func(t *testing.T) {
				assertMessagePoolOwnership(t)
				synctest.Test(t, func(t *testing.T) {
					f := newReliableTcpIngressFixture(t, version, nil)
					switch stop {
					case "source-retired":
						owner := f.nat.newSourceRetirementOwner()
						f.nat.retireSourceForOwner(owner, f.source.SourceId)
						defer f.nat.releaseSourceRetirementOwner(owner)
					case "provider-shutdown":
						f.provider.Close()
					case "nat-shutdown":
						f.nat.Close()
					}
					sequence := f.runningReceiveSequence()
					orphan := *f.path
					orphan.SourcePort++
					id := NewId()
					reliableIngressPackForPath(t, f, sequence, &orphan, 0, id, tcpFlagFin|tcpFlagAck, nil)
					synctest.Wait()
					if present, head := reliableIngressCumulativeHead(sequence); present {
						t.Errorf("retired admission authority falsely ACKed control: stop=%s head=%s item=%s", stop, head.messageId, id)
					}
					if ack := sequence.ackWindow.Snapshot(false); ack.selectiveAcks[id].messageId == id {
						t.Error("retired authority falsely selectively ACKed control")
					}
				})
			})
		}
	}
}

// FIN consumes sequence space on a live flow. It must retain its real final
// queue owner under pressure, rather than taking the absent-flow terminal
// control exception merely because it has no payload.
func TestProviderReliableTcpLiveFinWaitsForFinalAdmission(t *testing.T) {
	for _, version := range []int{4, 6} {
		t.Run(fmt.Sprintf("ipv%d", version), func(t *testing.T) {
			assertMessagePoolOwnership(t)
			synctest.Test(t, func(t *testing.T) {
				f := newReliableTcpIngressFixture(t, version, nil)
				f.establishForControl()
				sequence := f.runningReceiveSequence()
				id := NewId()
				reliableIngressPackForPath(t, f, sequence, f.path, 0, id, tcpFlagFin|tcpFlagAck, nil)
				synctest.Wait()
				if present, _ := reliableIngressCumulativeHead(sequence); present {
					t.Fatal("live FIN was ACKed before final queue ownership")
				}
				if sequence.ctx.Err() != nil || f.tcp.ctx.Err() != nil {
					t.Fatal("backpressured live FIN canceled its flow or shared lane")
				}
				(<-f.tcp.sendItems).release()
				synctest.Wait()
				select {
				case item := <-f.tcp.sendItems:
					if !item.tcp.fin || !item.tcp.ack || item.tcp.seq != 101 || len(item.tcp.payload) != 0 {
						t.Error("final owner did not receive exact retained FIN")
					}
					item.release()
				default:
					t.Fatal("released final queue did not admit original FIN")
				}
				if present, head := reliableIngressCumulativeHead(sequence); !present || head.messageId != id {
					t.Fatal("secured live FIN did not receive exact Transfer ACK")
				}
				if f.dials.Load() != 0 {
					t.Fatal("local FIN receipt depended on downstream WAN ACK")
				}
			})
		})
	}
}

// Batching is not downstream refusal. All these controls can be applied to
// the established final TCP owner, even though the source's data queue is
// full. A bounded control reserve must not cancel a healthy Transfer lane
// merely because flushDeliver decoded more than two controls before pumping.
func TestProviderReliableTcpHealthyControlBurstPreservesLane(t *testing.T) {
	for _, version := range []int{4, 6} {
		for _, count := range []int{1, 2, 3, 8} {
			t.Run(fmt.Sprintf("ipv%d/count-%d", version, count), func(t *testing.T) {
				assertMessagePoolOwnership(t)
				synctest.Test(t, func(t *testing.T) {
					f := newReliableTcpIngressFixture(t, version, nil)
					f.establishForControl()
					sequence := NewReceiveSequence(f.ctx, f.client, f.source, NewId(), sequenceTlsRoleServer, false, DefaultReceiveBufferSettings())
					ids := make([]Id, count)
					for index := range count {
						item, frame := reliableIngressControlItem(t, f.path, uint64(index), tcpFlagAck, 101)
						item.receiveCallback = f.client.receiveCallback
						ids[index] = item.messageId
						sequence.deliverItems = append(sequence.deliverItems, item)
						sequence.deliverFrames = append(sequence.deliverFrames, frame)
					}
					sequence.deliverPeer = Peer{ProvideMode: protocol.ProvideMode_Public}
					go runReliableIngressFixtureDelivery(sequence)
					synctest.Wait()
					if sequence.ctx.Err() != nil {
						t.Errorf("healthy %d-control batch canceled its shared Transfer lane: %v", count, sequence.ctx.Err())
					}
					if present, head := reliableIngressCumulativeHead(sequence); !present || head.messageId != ids[count-1] {
						t.Errorf("healthy control batch did not secure exact final head: present=%t head=%s want=%s", present, head.messageId, ids[count-1])
					}
					if f.tcp.ctx.Err() != nil || len(f.tcp.sendItems) != 1 || f.dials.Load() != 0 {
						t.Error("control burst changed the blocked data owner or depended on a WAN connection")
					}
				})
			})
		}
	}
}

// A successfully applied RST consumes only its TCP flow (and an orphan RST
// intentionally has no flow to reset). Neither is congestion or permission
// to tear down the multiplexed receive lane and lose other selectively held
// items. Its receipt is a local terminal-control action, never a WAN ACK.
func TestProviderReliableTcpResetPreservesNextFlow(t *testing.T) {
	for _, version := range []int{4, 6} {
		for _, budgeted := range []bool{false, true} {
			for _, orphan := range []bool{false, true} {
				t.Run(fmt.Sprintf("ipv%d/budget-%t/orphan-%t", version, budgeted, orphan), func(t *testing.T) {
					assertMessagePoolOwnership(t)
					synctest.Test(t, func(t *testing.T) {
						var budget *TransferMemoryBudget
						if budgeted {
							budget = NewTransferMemoryBudget(mib(2))
							t.Cleanup(func() {
								if budget.UsedByteCount() != 0 || budget.reservedByteCount.Load() != budget.releasedByteCount.Load() {
									t.Error("terminal reset/next-flow lifecycle leaked prepaid root ownership")
								}
							})
						}
						f := newReliableTcpIngressFixture(t, version, budget)
						f.establishForControl()
						sequence := f.runningReceiveSequence()
						path := *f.path
						if orphan {
							path.SourcePort++
						}
						item, _ := reliableIngressControlItem(t, &path, 0, tcpFlagRst, 101)
						resetID := item.messageId
						pack := &ReceivePack{Source: f.source, SequenceId: sequence.sequenceId,
							Pack: &protocol.Pack{MessageId: resetID.Bytes(), SequenceId: sequence.sequenceId.Bytes(),
								SequenceNumber: 0, Head: true, Frames: item.frames},
							ReceiveCallback: f.client.receiveCallback, Ctx: f.ctx, Unwrapped: true,
							EncryptionRole: sequenceTlsRoleServer, MessageByteCount: ByteCount(len(item.frames[0].MessageBytes))}
						if accepted, err := sequence.Pack(pack, 0); !accepted || err != nil {
							pack.messagePoolReturn()
							t.Fatalf("reset Pack admission: accepted=%t err=%v", accepted, err)
						}
						synctest.Wait()
						if sequence.ctx.Err() != nil {
							t.Fatalf("handled reset canceled unrelated shared Transfer lane: %v", sequence.ctx.Err())
						}
						if present, head := reliableIngressCumulativeHead(sequence); !present || head.messageId != resetID {
							t.Fatal("handled reset did not secure its exact terminal-control receipt")
						}
						if (f.tcp.ctx.Err() != nil) == orphan {
							t.Fatal("reset did not isolate the matching original TCP flow")
						}
						if budgeted && f.nat.controlMemory.UsedByteCount() != 0 {
							t.Fatal("handled reset retained its completed control credit")
						}
						path.SourcePort += 2
						next, _ := reliableIngressControlItem(t, &path, 1, tcpFlagSyn, 700)
						nextID := next.messageId
						nextPack := &ReceivePack{Source: f.source, SequenceId: sequence.sequenceId,
							Pack: &protocol.Pack{MessageId: nextID.Bytes(), SequenceId: sequence.sequenceId.Bytes(),
								SequenceNumber: 1, Frames: next.frames},
							ReceiveCallback: f.client.receiveCallback, Ctx: f.ctx, Unwrapped: true,
							EncryptionRole: sequenceTlsRoleServer, MessageByteCount: ByteCount(len(next.frames[0].MessageBytes))}
						if accepted, err := sequence.Pack(nextPack, 0); !accepted || err != nil {
							nextPack.messagePoolReturn()
							t.Fatalf("next flow Pack admission: accepted=%t err=%v", accepted, err)
						}
						synctest.Wait()
						if present, head := reliableIngressCumulativeHead(sequence); !present || head.messageId != nextID {
							t.Error("next flow could not secure a local SYN owner after handled reset")
						}
						if f.dials.Load() != 0 {
							t.Error("reset/next-SYN receipts depended on WAN TCP acknowledgement")
						}
					})
				})
			}
		}
	}
}
