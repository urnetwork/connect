package connect

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// This fixture observes only synthetic TCP header metadata. The reset must
// cross real provider-return SendSequence/ReceiveSequence ownership before
// the client callback sees it; a NAT callback invocation is not delivery.
type orphanPayloadResetMetadata struct {
	valid, rst, ack bool
	payloadBytes    int
}

func reliableOrphanPayloadReceiveSequence(t *testing.T, f *reliableTcpIngressFixture) *ReceiveSequence {
	t.Helper()
	f.client.ContractManager().AddNoContractPeer(f.source.SourceId)
	settings := DefaultReceiveBufferSettings()
	settings.IdleTimeout, settings.AckCompressTimeout = time.Hour, 0
	sequence := NewReceiveSequence(f.ctx, f.client, f.source, NewId(),
		sequenceTlsRoleServer, false, settings)
	go sequence.Run()
	t.Cleanup(func() {
		sequence.Cancel()
		sequence.WaitForExit()
		sequence.Close()
	})
	return sequence
}

type orphanPayloadReturnGate struct {
	hold    atomic.Bool
	held    chan struct{}
	release chan struct{}
}

func reliableOrphanPayloadReturnClient(t *testing.T, f *reliableTcpIngressFixture, gates ...*orphanPayloadReturnGate) <-chan orphanPayloadResetMetadata {
	t.Helper()
	settings := DefaultClientSettings()
	settings.Log = NewNoopLogger()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.ReceiveBufferSettings.AckCompressTimeout = 0
	settings.beforeClientKeyPublishForTest = func() { <-f.ctx.Done() }
	device := NewClient(f.ctx, f.source.SourceId, NewNoContractClientOob(), settings)
	device.ContractManager().AddNoContractPeer(f.client.ClientId())
	f.client.ContractManager().AddNoContractPeer(device.ClientId())
	// This fixture's contract-free incoming sequence is Network mode. Pin the
	// same recorded return policy for the startup control and the real reset.
	f.provider.recordSourceProvideMode(f.source.SourceId, protocol.ProvideMode_Network)
	resets := make(chan orphanPayloadResetMetadata, 16)
	device.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
		for _, frame := range frames {
			if frame.MessageType != protocol.MessageType_IpIpPacketFromProvider {
				continue
			}
			packet, err := ipPacketFromProviderBytes(frame)
			if err != nil {
				continue
			}
			var source, destination net.IP
			var transport []byte
			var valid bool
			if packet[0]>>4 == 4 {
				_, source, destination, transport, valid = parseIpv4(packet)
			} else {
				_, source, destination, transport, valid = parseIpv6(packet)
			}
			var tcp parsedTcp
			valid = valid && parseTcpPacket(source, destination, transport, &tcp)
			resets <- orphanPayloadResetMetadata{valid, tcp.rst, tcp.ack, len(tcp.payload)}
		}
	})
	toDevice, toProvider := make(Route, 64), make(Route, 64)
	deviceOutput := toProvider
	var gateWorkers sync.WaitGroup
	if len(gates) != 0 {
		gate := gates[0]
		deviceOutput = make(Route, 64)
		gateWorkers.Add(1)
		go func() {
			defer gateWorkers.Done()
			for {
				select {
				case <-f.ctx.Done():
					return
				case wire := <-deviceOutput:
					if gate.hold.Load() {
						select {
						case gate.held <- struct{}{}:
						default:
						}
						select {
						case <-gate.release:
						case <-f.ctx.Done():
							MessagePoolReturn(wire)
							return
						}
					}
					select {
					case toProvider <- wire:
					case <-f.ctx.Done():
						MessagePoolReturn(wire)
						return
					}
				}
			}
		}()
	}
	providerSend := NewSendClientTransport(DestinationId(device.ClientId()))
	providerReceive := NewReceiveGatewayTransport()
	deviceSend := NewSendClientTransport(DestinationId(f.client.ClientId()))
	deviceReceive := NewReceiveGatewayTransport()
	f.client.RouteManager().UpdateTransport(providerSend, []Route{toDevice})
	f.client.RouteManager().UpdateTransport(providerReceive, []Route{toProvider})
	device.RouteManager().UpdateTransport(deviceReceive, []Route{toDevice})
	device.RouteManager().UpdateTransport(deviceSend, []Route{deviceOutput})
	t.Cleanup(func() {
		f.cancel()
		if err := device.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
		if err := f.client.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
		gateWorkers.Wait()
		f.client.RouteManager().RemoveTransport(providerSend)
		f.client.RouteManager().RemoveTransport(providerReceive)
		routes := []Route{toDevice, toProvider}
		if deviceOutput != toProvider {
			routes = append(routes, deviceOutput)
		}
		for _, route := range routes {
			for len(route) != 0 {
				MessagePoolReturn(<-route)
			}
		}
	})
	// Initialize the exact return SendSequence with a dedicated control, which
	// retains ownership through startup admission. Otherwise the first
	// regenerable reset can legitimately lose a zero-timeout startup race,
	// conflating the confirmed-delivery positive with the refusal control.
	warmPath := *f.path
	warmPath.SourceIp, warmPath.DestinationIp = warmPath.DestinationIp, warmPath.SourceIp
	warmPath.SourcePort, warmPath.DestinationPort = warmPath.DestinationPort, warmPath.SourcePort
	warmPacket := MessagePoolCopy(ipOosTcpPacketSequence(&warmPath, tcpFlagAck, 101, nil))
	warmKey := TransferKey{EncryptionRole: protocol.SequenceRole_SequenceRoleServer}
	f.provider.receiveTransferWithRecovery(f.source, warmKey, protocol.ProvideMode_Network,
		receiveRecoveryModeDedicatedTcpControl, &warmPath, warmPacket)
	MessagePoolReturn(warmPacket)
	select {
	case warm := <-resets:
		if !warm.valid || warm.rst || !warm.ack || warm.payloadBytes != 0 {
			t.Fatal("return startup control header was not delivered")
		}
	case <-time.After(time.Second):
		t.Fatal("return SendSequence did not initialize")
	}
	time.Sleep(10 * time.Millisecond)
	synctest.Wait()
	return resets
}

// RED on the pinned product: even after the client receives the synthesized
// RST, the payload-bearing orphan has already canceled the shared incoming
// Transfer sequence. Neither its live sibling nor a fresh flow can repair
// that permanent gap. No assertion interprets reset as payload delivery.
func TestProviderReliableOrphanPayloadDeliveredResetPreservesNextFlow(t *testing.T) {
	for _, version := range []int{4, 6} {
		for _, retired := range []bool{false, true} {
			t.Run(fmt.Sprintf("ipv%d/legitimate-rst-retirement-%t", version, retired), func(t *testing.T) {
				assertMessagePoolOwnership(t)
				synctest.Test(t, func(t *testing.T) {
					f := newReliableTcpIngressFixture(t, version, nil)
					f.establishForControl()
					sequence := reliableOrphanPayloadReceiveSequence(t, f)
					resets := reliableOrphanPayloadReturnClient(t, f)
					orphan := *f.path
					orphan.SourcePort++
					// Keep newly created fixture workers parked without the
					// original single-capture channel becoming a second blocker.
					f.nat.settings.TcpBufferSettings.beforeSequenceRunWithStateForTest = nil
					if retired {
						for _, flags := range []byte{tcpFlagSyn, tcpFlagRst} {
							packet := MessagePoolCopy(ipOosTcpPacketSequence(&orphan, flags, 100, nil))
							if !f.nat.SendPacket(f.source, protocol.ProvideMode_Public, packet, 0) {
								MessagePoolReturn(packet)
								t.Fatal("legitimate flow retirement setup was not admitted")
							}
							synctest.Wait()
						}
					}
					id := NewId()
					reliableIngressPackForPath(t, f, sequence, &orphan, 0, id, tcpFlagPsh|tcpFlagAck, []byte{0})
					time.Sleep(time.Millisecond)
					synctest.Wait()
					select {
					case reset := <-resets:
						if !reset.valid || !reset.rst || reset.ack || reset.payloadBytes != 0 {
							t.Fatal("client did not receive the exact terminal reset header")
						}
					default:
						t.Fatal("fixture did not deliver the provider reset through Transfer")
					}
					if f.tcp.ctx.Err() != nil || f.dials.Load() != 0 {
						t.Fatal("orphan disturbed the independent TCP owner")
					}
					if sequence.ctx.Err() != nil {
						t.Fatal("client received terminal reset, but orphan payload poisoned shared Transfer sequence")
					}
					if present, head := reliableIngressCumulativeHead(sequence); !present || head.messageId != id {
						t.Fatal("confirmed terminal reset did not settle its exact receipt")
					}
					next := orphan
					next.SourcePort++
					nextID := NewId()
					reliableIngressPackForPath(t, f, sequence, &next, 1, nextID, tcpFlagSyn, nil)
					synctest.Wait()
					if present, head := reliableIngressCumulativeHead(sequence); !present || head.messageId != nextID {
						t.Fatal("confirmed reset prevented independent flow's SYN admission")
					}
				})
			})
		}
	}
}

func TestProviderReliableOrphanPayloadUnavailableResetNeverAcked(t *testing.T) {
	for _, version := range []int{4, 6} {
		for _, policy := range []string{"disabled", "rate-limited", "return-refused"} {
			t.Run(fmt.Sprintf("ipv%d/%s", version, policy), func(t *testing.T) {
				assertMessagePoolOwnership(t)
				synctest.Test(t, func(t *testing.T) {
					f := newReliableTcpIngressFixture(t, version, nil)
					sequence := reliableOrphanPayloadReceiveSequence(t, f)
					resets := reliableOrphanPayloadReturnClient(t, f)
					orphan := *f.path
					orphan.SourcePort++
					settings := f.nat.settings.TcpBufferSettings
					settings.EnableOrphanRst = policy != "disabled"
					settings.OrphanRstPerSecond = 1
					if policy == "rate-limited" {
						packet := MessagePoolCopy(ipOosTcpPacketSequence(&orphan, tcpFlagAck, 100, nil))
						if !f.nat.SendPacket(f.source, protocol.ProvideMode_Public, packet, 0) {
							MessagePoolReturn(packet)
							t.Fatal("reset limiter priming was not admitted")
						}
						time.Sleep(time.Millisecond)
						synctest.Wait()
						select {
						case <-resets:
						default:
							t.Fatal("limiter fixture did not deliver its first reset")
						}
					}
					if policy == "return-refused" {
						f.provider.returnStateLock.Lock()
						f.provider.returnClosed = true
						f.provider.returnStateLock.Unlock()
					}
					id := NewId()
					reliableIngressPackForPath(t, f, sequence, &orphan, 0, id, tcpFlagPsh|tcpFlagAck, []byte{0})
					synctest.Wait()
					if len(resets) != 0 {
						t.Fatal("unavailable reset was delivered unexpectedly")
					}
					if present, _ := reliableIngressCumulativeHead(sequence); present {
						t.Fatal("unavailable reset falsely settled orphan payload")
					}
					if ack := sequence.ackWindow.Snapshot(false); ack.selectiveAcks[id].messageId == id {
						t.Fatal("unavailable reset falsely selectively acknowledged orphan payload")
					}
				})
			})
		}
	}
}

type orphanPayloadHeldAck struct {
	entered chan error
	release chan struct{}
}

func (ack *orphanPayloadHeldAck) sendAckResult(_ ByteCount, err error) {
	ack.entered <- err
	<-ack.release
}

func orphanPayloadSyn(t *testing.T, f *reliableTcpIngressFixture, sequence *ReceiveSequence, path *IpPath, number uint64, id Id) {
	t.Helper()
	packet := MessagePoolCopy(ipOosTcpPacketSequence(path, tcpFlagSyn, 100, nil))
	frame, err := ipPacketToProviderFrame(packet, DefaultProtocolVersion)
	if err != nil {
		MessagePoolReturn(packet)
		t.Fatal("synthetic sibling SYN could not be framed")
	}
	pack := &ReceivePack{Source: f.source, SequenceId: sequence.sequenceId,
		Pack: &protocol.Pack{MessageId: id.Bytes(), SequenceId: sequence.sequenceId.Bytes(),
			SequenceNumber: number, Head: number == 0, Frames: []*protocol.Frame{frame}},
		ReceiveCallback: f.client.receiveCallback, Ctx: f.ctx, Unwrapped: true,
		EncryptionRole: sequenceTlsRoleServer, MessageByteCount: ByteCount(len(packet))}
	if accepted, err := sequence.Pack(pack, 0); !accepted || err != nil {
		pack.messagePoolReturn()
		t.Fatal("synthetic sibling SYN did not enter Transfer")
	}
}

// Queue admission and even observed reset delivery do not settle the input:
// its return ACK owns completion. Hold that ACK after real receipt at the
// device, admit a different TCP flow's SYN and data, and then either release
// the ACK or cancel the input before its late success. The original receipt
// and every packet/credit must be disposed exactly once in both orders.
func TestProviderReliableOrphanPayloadResetAckOwnership(t *testing.T) {
	for _, version := range []int{4, 6} {
		for _, protocolVersion := range []int{1, 2} {
			for _, cancelInput := range []bool{false, true} {
				t.Run(fmt.Sprintf("ipv%d/protocol-%d/cancel-%t", version, protocolVersion, cancelInput), func(t *testing.T) {
					assertMessagePoolOwnership(t)
					synctest.Test(t, func(t *testing.T) {
						budget := NewTransferMemoryBudget(mib(2))
						t.Cleanup(func() {
							if budget.UsedByteCount() != 0 || budget.reservedByteCount.Load() != budget.releasedByteCount.Load() {
								t.Error("terminal reset ownership leaked prepaid root memory")
							}
						})
						f := newReliableTcpIngressFixture(t, version, budget)
						f.establishForControl()
						f.provider.settings.ProtocolVersion = protocolVersion
						sequence := reliableOrphanPayloadReceiveSequence(t, f)
						resets := reliableOrphanPayloadReturnClient(t, f)
						ack := &orphanPayloadHeldAck{entered: make(chan error, 2), release: make(chan struct{})}
						var releaseOnce sync.Once
						release := func() { releaseOnce.Do(func() { close(ack.release) }) }
						defer release()
						f.provider.returnAckTargetForTest = ack
						orphan := *f.path
						orphan.SourcePort++
						orphanID := NewId()
						reliableIngressPackForPath(t, f, sequence, &orphan, 0, orphanID, tcpFlagPsh|tcpFlagAck, []byte{0})
						time.Sleep(time.Millisecond)
						synctest.Wait()
						select {
						case reset := <-resets:
							if !reset.valid || !reset.rst || reset.payloadBytes != 0 {
								t.Fatal("held-ACK fixture did not deliver terminal reset")
							}
						default:
							t.Fatal("held-ACK fixture did not reach client receipt")
						}
						select {
						case err := <-ack.entered:
							if err != nil {
								t.Fatal("held ACK was not a successful return completion")
							}
						default:
							t.Fatal("reset completion did not reach exact ACK barrier")
						}
						if present, _ := reliableIngressCumulativeHead(sequence); present || sequence.ctx.Err() != nil {
							t.Fatal("reset callback/admission claimed success or canceled before ACK")
						}
						// A pending terminal response must not monopolize the
						// shared receive lane or the local NAT shard.
						newFlow := make(chan *TcpSequence, 1)
						f.nat.settings.TcpBufferSettings.beforeSequenceRunWithStateForTest = func(flow *TcpSequence) { newFlow <- flow }
						next := orphan
						next.SourcePort++
						synID := NewId()
						orphanPayloadSyn(t, f, sequence, &next, 1, synID)
						synctest.Wait()
						var sibling *TcpSequence
						select {
						case sibling = <-newFlow:
						default:
							t.Fatal("pending orphan reset blocked unrelated SYN")
						}
						select {
						case syn := <-sibling.sendItems:
							if !syn.tcp.syn || syn.tcp.seq != 100 {
								t.Error("unrelated SYN lost its exact downstream owner")
							}
							syn.release()
						default:
							t.Fatal("unrelated TCP owner did not retain its SYN")
						}
						dataID := NewId()
						reliableIngressPackForPath(t, f, sequence, &next, 2, dataID, tcpFlagPsh|tcpFlagAck, []byte{0})
						synctest.Wait()
						select {
						case data := <-sibling.sendItems:
							if data.tcp.seq != 101 || len(data.tcp.payload) != 1 {
								t.Error("unrelated data lost its exact downstream owner")
							}
							data.release()
						default:
							t.Fatal("pending orphan reset blocked unrelated data")
						}
						if f.dials.Load() != 0 {
							t.Fatal("local receipt ownership depended on an upstream socket")
						}
						if present, _ := reliableIngressCumulativeHead(sequence); present {
							t.Fatal("sibling success jumped the unconfirmed reset receipt")
						}
						if cancelInput {
							sequence.Cancel()
							synctest.Wait()
						}
						release()
						synctest.Wait()
						present, head := reliableIngressCumulativeHead(sequence)
						if cancelInput {
							if present {
								t.Fatal("late reset ACK resurrected a canceled input receipt")
							}
						} else if !present || head.messageId != dataID || sequence.ctx.Err() != nil {
							t.Fatal("confirmed reset did not advance through secured sibling SYN and data")
						}
					})
				})
			}
		}
	}
}

func TestProviderReliableOrphanPayloadResetShutdownDoesNotWaitForAck(t *testing.T) {
	for _, version := range []int{4, 6} {
		t.Run(fmt.Sprintf("ipv%d", version), func(t *testing.T) {
			assertMessagePoolOwnership(t)
			synctest.Test(t, func(t *testing.T) {
				f := newReliableTcpIngressFixture(t, version, nil)
				defer f.cancel()
				sequence := reliableOrphanPayloadReceiveSequence(t, f)
				gate := &orphanPayloadReturnGate{held: make(chan struct{}, 1), release: make(chan struct{})}
				resets := reliableOrphanPayloadReturnClient(t, f, gate)
				gate.hold.Store(true)
				orphan := *f.path
				orphan.SourcePort++
				reliableIngressPackForPath(t, f, sequence, &orphan, 0, NewId(), tcpFlagPsh|tcpFlagAck, []byte{0})
				time.Sleep(time.Millisecond)
				synctest.Wait()
				if len(resets) != 1 || len(gate.held) != 1 {
					t.Fatal("shutdown fixture did not hold the real reset ACK after client receipt")
				}
				closed := make(chan struct{})
				go func() { f.provider.Close(); close(closed) }()
				synctest.Wait()
				select {
				case <-closed:
				default:
					t.Fatal("provider shutdown waits for terminal reset's absent peer ACK")
				}
				if present, _ := reliableIngressCumulativeHead(sequence); present {
					t.Fatal("shutdown falsely settled an unacknowledged reset")
				}
			})
		})
	}
}

// A failed zero-timeout Transfer admission is final for this regenerable
// response. A separate later orphan owns a fresh target and can succeed; the
// first target must never be retried or reused after reporting refusal.
func TestProviderReliableOrphanPayloadResetFailedAdmissionThenFreshSuccess(t *testing.T) {
	for _, version := range []int{4, 6} {
		for _, protocolVersion := range []int{1, 2} {
			t.Run(fmt.Sprintf("ipv%d/protocol-%d", version, protocolVersion), func(t *testing.T) {
				assertMessagePoolOwnership(t)
				synctest.Test(t, func(t *testing.T) {
					f := newReliableTcpIngressFixture(t, version, nil)
					f.provider.settings.ProtocolVersion = protocolVersion
					sequence := reliableOrphanPayloadReceiveSequence(t, f)
					id := sendSequenceId{Destination: f.source.SourceId, EncryptionRole: sequenceTlsRoleServer}
					full := installProviderReturnTestSequence(t, f.provider, f.client, id)
					full.packs = make(chan *SendPack) // no receiver: admission always refuses
					attempts := make(chan providerReturnSendResult, 4)
					completed := make(chan providerReturnSendResult, 4)
					queued := make(chan bool, 4)
					var retries atomic.Int32
					f.provider.afterReturnSendAttemptForTest = func(result providerReturnSendResult) { attempts <- result }
					f.provider.afterReturnSendForTest = func(result providerReturnSendResult) { completed <- result }
					f.provider.afterReturnEnqueueForTest = func(value bool) { queued <- value }
					f.provider.beforeTcpReturnSendRetryForTest = func() { retries.Add(1) }
					orphan := *f.path
					orphan.SourcePort++
					firstID := NewId()
					reliableIngressPackForPath(t, f, sequence, &orphan, 0, firstID, tcpFlagPsh|tcpFlagAck, []byte{0})
					time.Sleep(20 * time.Millisecond)
					synctest.Wait()
					queuedCount, attemptCount, completeCount := len(queued), len(attempts), len(completed)
					var wasQueued, attemptSent, completeSent bool
					if queuedCount != 0 {
						wasQueued = <-queued
					}
					if attemptCount != 0 {
						attemptSent = (<-attempts).sent
					}
					if completeCount != 0 {
						completeSent = (<-completed).sent
					}
					if queuedCount != 1 || !wasQueued || attemptCount != 1 || attemptSent ||
						completeCount != 1 || completeSent || retries.Load() != 0 {
						t.Fatalf("reset admission counts queued=%d/%t attempt=%d/%t complete=%d/%t retries=%d", queuedCount, wasQueued, attemptCount, attemptSent, completeCount, completeSent, retries.Load())
					}
					if present, _ := reliableIngressCumulativeHead(sequence); present || sequence.ctx.Err() == nil {
						t.Fatal("failed reset admission did not preserve negative receipt outcome")
					}
					if ack := sequence.ackWindow.Snapshot(false); ack.selectiveAcks[firstID].messageId == firstID {
						t.Fatal("failed reset admission falsely selectively acknowledged input")
					}
					f.client.sendBuffer.mutex.Lock()
					delete(f.client.sendBuffer.sendSequences, id)
					delete(f.client.sendBuffer.wireSendSequences, id.wireId())
					f.client.sendBuffer.mutex.Unlock()
					f.provider.afterReturnSendAttemptForTest = nil
					f.provider.afterReturnSendForTest = nil
					f.provider.afterReturnEnqueueForTest = nil
					f.provider.beforeTcpReturnSendRetryForTest = nil
					nextSequence := reliableOrphanPayloadReceiveSequence(t, f)
					resets := reliableOrphanPayloadReturnClient(t, f)
					nextID := NewId()
					reliableIngressPackForPath(t, f, nextSequence, &orphan, 0, nextID, tcpFlagPsh|tcpFlagAck, []byte{0})
					time.Sleep(time.Millisecond)
					synctest.Wait()
					if len(resets) != 1 {
						t.Fatal("fresh terminal owner did not deliver its reset")
					}
					if present, head := reliableIngressCumulativeHead(nextSequence); !present || head.messageId != nextID || nextSequence.ctx.Err() != nil {
						t.Fatal("prior failure contaminated a fresh successful reset owner")
					}
				})
			})
		}
	}
}

func TestProviderReliableTerminalResetRejectsRetryingMode(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newReliableTcpIngressFixture(t, 4, nil)
		packet := MessagePoolCopy(ipOosTcpPacketSequence(f.path, tcpFlagRst, 100, nil))
		defer MessagePoolReturn(packet)
		for _, mode := range []receiveRecoveryMode{receiveRecoveryModeTcpSocket, receiveRecoveryModeDedicatedTcpControl} {
			target := &providerTerminalResetAckTarget{wake: make(chan struct{}, 1)}
			f.provider.receiveTransferWithRecoveryAndRelease(f.source, TransferKey{}, protocol.ProvideMode_Public,
				mode, f.path, packet, nil, target)
			if target.attempt() != receiveDeliveryRejected {
				t.Fatal("a retrying recovery mode acquired a final-only reset target")
			}
		}
		f.provider.Close()
		target := &providerTerminalResetAckTarget{wake: make(chan struct{}, 1)}
		f.provider.receiveTransferWithRecoveryAndRelease(f.source, TransferKey{}, protocol.ProvideMode_Public,
			receiveRecoveryModeRegenerableControl, f.path, packet, nil, target)
		if target.attempt() != receiveDeliveryRejected {
			t.Fatal("provider closed before send left terminal reset completion pending")
		}
	})
}

func TestProviderReliableOrphanPayloadResetPanicDuringRetryReleasesOwner(t *testing.T) {
	for _, version := range []int{4, 6} {
		t.Run(fmt.Sprintf("ipv%d", version), func(t *testing.T) {
			assertMessagePoolOwnership(t)
			synctest.Test(t, func(t *testing.T) {
				budget := NewTransferMemoryBudget(mib(2))
				t.Cleanup(func() {
					if budget.UsedByteCount() != 0 || budget.reservedByteCount.Load() != budget.releasedByteCount.Load() {
						t.Error("panicking terminal reset leaked prepaid root ownership")
					}
				})
				f := newReliableTcpIngressFixture(t, version, budget)
				sequence, id := f.deliver([]byte{0})
				synctest.Wait()
				q := sequence.deliveryQueue
				q.mutex.Lock()
				if len(q.operations) != 1 {
					q.mutex.Unlock()
					t.Fatal("retry fixture did not retain exactly one full-queue operation")
				}
				op := q.operations[0]
				q.mutex.Unlock()
				op.mutex.Lock()
				waiting := op.state == receiveDeliveryWaiting && !op.attempting && !op.terminal
				op.mutex.Unlock()
				if !waiting {
					t.Fatal("retry fixture did not reach final TCP queue pressure")
				}
				var panics atomic.Int32
				var whileAttempting atomic.Bool
				f.provider.afterReturnEnqueueForTest = func(bool) {
					op.mutex.Lock()
					whileAttempting.Store(op.attempting)
					op.mutex.Unlock()
					panics.Add(1)
					panic("Done") // contained sentinel, without a stack/payload dump
				}
				f.tcp.Cancel()
				f.nat.reliableCapacity.notify()
				synctest.Wait()
				op.mutex.Lock()
				terminal, attempting := op.terminal, op.attempting
				op.mutex.Unlock()
				if panics.Load() != 1 || !whileAttempting.Load() || !terminal || attempting {
					t.Fatal("terminal callback panic bypassed retry-owner disposal")
				}
				if present, _ := reliableIngressCumulativeHead(sequence); present || sequence.ctx.Err() == nil {
					t.Fatal("panicking terminal handoff falsely settled the original input")
				}
				if ack := sequence.ackWindow.Snapshot(false); ack.selectiveAcks[id].messageId == id {
					t.Fatal("panicking terminal handoff falsely selectively acknowledged input")
				}
			})
		})
	}
}
