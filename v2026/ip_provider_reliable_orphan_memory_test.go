// Orphan reset refusals exercise the real receive ledger, finite shared memory,
// and an independent TCP owner on the same incoming sequence.
package connect

import (
	"bytes"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Borrows the normal policy and pauses only its return inspection. Ingress
// continues through the embedded policy, including the sibling's real packets.
type orphanResetReturnPolicy struct {
	SecurityPolicy
	inspect func() (SecurityPolicyResult, error)
}

// Run the test's exact inspection barrier and verdict, borrowing all inputs.
func (self *orphanResetReturnPolicy) InspectEgress(_ protocol.ProvideMode, _ *IpPath, _ []byte) (SecurityPolicyResult, error) {
	return self.inspect()
}

// The orphan and sibling both own pending payload before pressure is applied.
// The orphan loses its TCP owner; the sibling remains live and must not gain an
// ACK until its exact packet reaches TCP. No new production test hook is needed.
func runProviderReliableOrphanCapacityRefusal(t *testing.T, boundary string) {
	t.Helper()
	assertMessagePoolOwnership(t)
	for _, version := range []int{4, 6} {
		for _, protocolVersion := range []int{1, 2} {
			synctest.Test(t, func(t *testing.T) {
				root := NewTransferMemoryBudget(mib(4))
				natBudget := NewTransferMemoryBudgetWithParent(mib(2), root)
				receiveBudget := NewTransferMemoryBudgetWithParent(kib(128), root)
				t.Cleanup(func() {
					for _, budget := range []*TransferMemoryBudget{root, natBudget, receiveBudget} {
						if budget.UsedByteCount() != 0 || budget.reservedByteCount.Load() != budget.releasedByteCount.Load() {
							t.Errorf("%s ipv%d protocol-%d leaked budget: used=%d reserved=%d released=%d", boundary, version, protocolVersion,
								budget.UsedByteCount(), budget.reservedByteCount.Load(), budget.releasedByteCount.Load())
						}
					}
				})
				f := newReliableTcpIngressFixtureWithPath(t, reliableDeliveryTestPath(version), natBudget)
				f.establishForControl()
				f.provider.settings.ProtocolVersion = protocolVersion
				sequence := f.runningReceiveSequence(reliableIngressBudgetSettings(receiveBudget))
				orphan := *f.path
				orphan.SourcePort++
				created := make(chan *TcpSequence, 1)
				f.nat.settings.TcpBufferSettings.beforeSequenceRunWithStateForTest = func(flow *TcpSequence) { created <- flow }
				synId := NewId()
				orphanPayloadSyn(t, f, sequence, &orphan, 0, synId)
				orphanTcp := <-created
				orphanId, siblingId := NewId(), NewId()
				orphanPayload := []byte("orphan keeps only a regenerable reset")
				siblingPayload := []byte("sibling still needs exact TCP custody")
				reliableIngressPackForPath(t, f, sequence, &orphan, 1, orphanId, tcpFlagPsh|tcpFlagAck, orphanPayload)
				reliableIngressPackForPath(t, f, sequence, f.path, 2, siblingId, tcpFlagPsh|tcpFlagAck, siblingPayload)
				synctest.Wait()
				if present, head := reliableIngressCumulativeHead(sequence); !present || head.messageId != synId {
					t.Fatal("fixture did not retain both payloads behind their full TCP queues")
				}
				if ack := sequence.ackWindow.Snapshot(false); ack.selectiveAcks[siblingId].messageId == siblingId {
					t.Fatal("pending sibling payload received a false selective ACK")
				}

				var fill ByteCount
				defer func() { natBudget.Release(fill) }()
				fillBudget := func() {
					bytes := natBudget.Available()
					if bytes <= 0 || !natBudget.TryReserve(bytes) {
						t.Fatal("could not fill the exact return memory budget")
					}
					fill = bytes
					if natBudget.Available() != 0 {
						t.Fatal("return memory budget retained unexpected capacity")
					}
				}
				var returnInspections atomic.Int32
				entered, release := make(chan struct{}, 1), make(chan struct{})
				var releaseOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				defer unblock()
				var enqueueCount atomic.Int32
				var canceledAfterLookup atomic.Int32
				f.provider.afterReturnEnqueueForTest = func(bool) { enqueueCount.Add(1) }
				switch boundary {
				case "scratch":
					f.provider.securityPolicy = &orphanResetReturnPolicy{SecurityPolicy: f.provider.securityPolicy, inspect: func() (SecurityPolicyResult, error) {
						returnInspections.Add(1)
						return SecurityPolicyResultAllow, nil
					}}
					fillBudget()
				case "item":
					f.provider.securityPolicy = &orphanResetReturnPolicy{SecurityPolicy: f.provider.securityPolicy, inspect: func() (SecurityPolicyResult, error) {
						returnInspections.Add(1)
						entered <- struct{}{}
						<-release
						return SecurityPolicyResultAllow, nil
					}}
				case "queue", "queue-after-lookup":
					// Occupy the real worker, then every slot in the exact shard
					// selected by this flow's reset. The shared NAT worker stays free.
					returnPath := orphan
					returnPath.SourceIp, returnPath.DestinationIp = returnPath.DestinationIp, returnPath.SourceIp
					returnPath.SourcePort, returnPath.DestinationPort = returnPath.DestinationPort, returnPath.SourcePort
					packet := MessagePoolCopy(ipOosTcpPacketSequence(&returnPath, tcpFlagRst, 101, nil))
					defer MessagePoolReturn(packet)
					shard := providerReturnSendShard(f.source, packet, len(f.provider.returnSendQueues))
					f.provider.returnSendStarted = func() { entered <- struct{}{}; <-release }
					f.provider.receiveTransferWithRecovery(f.source, TransferKey{}, protocol.ProvideMode_Network,
						receiveRecoveryModeRegenerableControl, &orphan, packet)
					<-entered
					for range cap(f.provider.returnSendQueues[shard]) {
						f.provider.receiveTransferWithRecovery(f.source, TransferKey{}, protocol.ProvideMode_Network,
							receiveRecoveryModeRegenerableControl, &orphan, packet)
					}
					// Scratch releases wake the pending TCP operations. Finish
					// those retries before choosing the flow-retirement boundary;
					// otherwise cancellation can race a previous live lookup.
					synctest.Wait()
					if len(f.provider.returnSendQueues[shard]) != cap(f.provider.returnSendQueues[shard]) {
						t.Fatal("fixture did not saturate the exact return shard")
					}
					enqueueCount.Store(0)
				default:
					t.Fatal("unknown reset refusal boundary")
				}
				if boundary == "queue-after-lookup" {
					// Force retirement after a live lookup, before the exact
					// retained payload attempts final TCP admission.
					f.nat.settings.TcpBufferSettings.beforeSequenceSendForTest = func(flow *TcpSequence) {
						if flow == orphanTcp {
							canceledAfterLookup.Add(1)
							flow.Cancel()
						}
					}
				} else {
					orphanTcp.Cancel()
				}
				f.nat.reliableCapacity.notify()
				if boundary == "item" {
					<-entered
					fillBudget()
					unblock()
				}
				synctest.Wait()
				if sequence.ctx.Err() != nil || f.provider.ctx.Err() != nil || f.tcp.ctx.Err() != nil {
					t.Fatalf("%s ipv%d protocol-%d reset capacity refusal canceled shared work: sequence=%v provider=%v sibling=%v nat=%v enqueues=%d drops=%+v afterLookup=%d",
						boundary, version, protocolVersion, sequence.ctx.Err(), f.provider.ctx.Err(), f.tcp.ctx.Err(), f.nat.ctx.Err(),
						enqueueCount.Load(), f.provider.CongestionDropStats(), canceledAfterLookup.Load())
				}
				if present, head := reliableIngressCumulativeHead(sequence); !present || head.messageId != orphanId {
					t.Fatalf("%s ipv%d protocol-%d reset capacity refusal did not settle only its orphan", boundary, version, protocolVersion)
				}
				if boundary == "scratch" && returnInspections.Load() != 0 || boundary == "item" && returnInspections.Load() != 1 {
					t.Fatal("fixture did not refuse the selected memory boundary")
				}
				wantEnqueues := int32(0)
				if boundary == "queue" || boundary == "queue-after-lookup" {
					wantEnqueues = 1
				}
				if boundary == "queue-after-lookup" && canceledAfterLookup.Load() != 1 {
					t.Fatal("fixture did not retire exactly once after the live TCP lookup")
				}
				if enqueueCount.Load() != wantEnqueues || f.provider.CongestionDropStats().ReturnQueuePacketCount != 1 {
					t.Fatal("reset refusal was not one counted, nonblocking disposition")
				}
				if ack := sequence.ackWindow.Snapshot(false); ack.selectiveAcks[siblingId].messageId == siblingId {
					t.Fatal("reset refusal falsely selectively ACKed the live sibling")
				}

				// Exact retransmits must not regenerate the settled orphan, duplicate
				// the retained sibling, or publish a false ACK for that sibling.
				reliableIngressPackForPath(t, f, sequence, &orphan, 1, orphanId, tcpFlagPsh|tcpFlagAck, orphanPayload)
				reliableIngressPackForPath(t, f, sequence, f.path, 2, siblingId, tcpFlagPsh|tcpFlagAck, siblingPayload)
				synctest.Wait()
				if present, head := reliableIngressCumulativeHead(sequence); !present || head.messageId != orphanId {
					t.Fatal("duplicate crossed the live sibling's unresolved receipt")
				}
				if ack := sequence.ackWindow.Snapshot(false); ack.selectiveAcks[siblingId].messageId == siblingId {
					t.Fatal("pending sibling duplicate received a false selective ACK")
				}
				if enqueueCount.Load() != wantEnqueues || f.provider.CongestionDropStats().ReturnQueuePacketCount != 1 {
					t.Fatal("settled orphan duplicate generated another return attempt")
				}
				natBudget.Release(fill)
				fill = 0
				// The waiting return worker has already proved immediate queue refusal.
				// Stop its hook before draining so later sends cannot fill the barrier.
				f.provider.returnSendStarted = nil
				unblock()
				(<-f.tcp.sendItems).release()
				synctest.Wait()
				select {
				case item := <-f.tcp.sendItems:
					if item.tcp.seq != 101 || !bytes.Equal(item.tcp.payload, siblingPayload) || item.memory.budget != natBudget {
						t.Error("live sibling lost its exact payload or prepaid owner")
					}
					item.release()
				default:
					t.Fatal("reset refusal prevented sibling progress on the same sequence")
				}
				if present, head := reliableIngressCumulativeHead(sequence); !present || head.messageId != siblingId || sequence.ctx.Err() != nil {
					t.Fatal("sibling final admission did not advance the same live sequence")
				}
				reliableIngressPackForPath(t, f, sequence, f.path, 2, siblingId, tcpFlagPsh|tcpFlagAck, siblingPayload)
				synctest.Wait()
				if len(f.tcp.sendItems) != 0 || f.dials.Load() != 0 {
					t.Fatal("duplicate redelivered the sibling or depended on a WAN socket")
				}
			})
		}
	}
}

// Exhaust scratch before policy inspection while preserving the sibling's owner.
func TestProviderReliableOrphanPayloadScratchRefusalPreservesSameSequence(t *testing.T) {
	runProviderReliableOrphanCapacityRefusal(t, "scratch")
}

// Exhaust return-item memory after the reset has passed security inspection.
func TestProviderReliableOrphanPayloadItemRefusalPreservesSameSequence(t *testing.T) {
	runProviderReliableOrphanCapacityRefusal(t, "item")
}

// Fill the actual return shard without delaying the shared ingress worker.
func TestProviderReliableOrphanPayloadReturnQueueRefusalPreservesSameSequence(t *testing.T) {
	runProviderReliableOrphanCapacityRefusal(t, "queue")
}

// Losing the selected TCP owner before admission must make the same terminal
// decision as finding that orphan before lookup, without canceling its sibling.
func TestProviderReliableOrphanPayloadRetirementAfterLookupPreservesSameSequence(t *testing.T) {
	runProviderReliableOrphanCapacityRefusal(t, "queue-after-lookup")
}

// The same exhausted budget cannot turn a retired owner or an arbitrary packet
// into a successful terminal reset. Each case uses a fresh target and fixture.
func TestProviderReliableTerminalResetMemoryRefusalRejectsInvalidOwners(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, reason := range []string{"provider", "source", "nat", "return", "memory-gate", "malformed", "payload", "ack", "mode"} {
		synctest.Test(t, func(t *testing.T) {
			budget := NewTransferMemoryBudget(mib(2))
			t.Cleanup(func() {
				if budget.UsedByteCount() != 0 || budget.reservedByteCount.Load() != budget.releasedByteCount.Load() {
					t.Errorf("%s rejection leaked memory", reason)
				}
			})
			f := newReliableTcpIngressFixtureWithPath(t, reliableDeliveryTestPath(4), budget)
			source := f.provider.acquireSourceLifecycle(f.source.SourceId)
			defer f.provider.releaseSourceLifecycle(f.source.SourceId, source)
			packet := MessagePoolCopy(ipOosTcpPacketSequence(f.path, tcpFlagRst, 101, nil))
			if reason == "payload" || reason == "ack" {
				MessagePoolReturn(packet)
				flags, payload := tcpFlagAck, []byte(nil)
				if reason == "payload" {
					flags, payload = tcpFlagRst, []byte("retained bytes cannot become a reset refusal")
				}
				packet = MessagePoolCopy(ipOosTcpPacketSequence(f.path, flags, 101, payload))
			}
			defer MessagePoolReturn(packet)
			input := packet
			mode := receiveRecoveryModeRegenerableControl
			switch reason {
			case "provider":
				f.provider.cancel()
			case "source":
				source.admissions.close()
				source.cancel()
			case "nat":
				f.nat.Close()
			case "return":
				f.provider.returnStateLock.Lock()
				f.provider.returnClosed = true
				f.provider.returnStateLock.Unlock()
			case "memory-gate":
				f.provider.memoryOperations.close()
			case "malformed":
				input = packet[:1]
			case "mode":
				mode = receiveRecoveryModeTcpSocket
			}
			fill := budget.Available()
			if !budget.TryReserve(fill) {
				t.Fatal("could not fill rejection-control budget")
			}
			defer budget.Release(fill)
			target := &providerTerminalResetAckTarget{wake: make(chan struct{}, 1)}
			f.provider.receiveTransferWithRecoveryAndRelease(f.source, TransferKey{}, protocol.ProvideMode_Network,
				mode, f.path, input, nil, target)
			if target.attempt() != receiveDeliveryRejected {
				t.Fatalf("%s refusal incorrectly settled terminal input", reason)
			}
			if reason == "source" && (f.provider.ctx.Err() != nil || f.client.Ctx().Err() != nil) {
				t.Fatal("source cancellation affected shared parents")
			}
		})
	}
}

// Security rejection remains terminal even when inspection itself consumes the
// remaining data budget. The return-item capacity branch must not run first.
func TestProviderReliableOrphanPayloadPolicyFailureNeverAcked(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		budget := NewTransferMemoryBudget(mib(2))
		t.Cleanup(func() {
			if budget.UsedByteCount() != 0 || budget.reservedByteCount.Load() != budget.releasedByteCount.Load() {
				t.Error("policy rejection leaked memory")
			}
		})
		f := newReliableTcpIngressFixtureWithPath(t, reliableDeliveryTestPath(4), budget)
		sequence := f.runningReceiveSequence()
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		defer unblock()
		f.provider.securityPolicy = &orphanResetReturnPolicy{SecurityPolicy: f.provider.securityPolicy, inspect: func() (SecurityPolicyResult, error) {
			close(entered)
			<-release
			return SecurityPolicyResultDrop, nil
		}}
		orphan := *f.path
		orphan.SourcePort++
		id := NewId()
		reliableIngressPackForPath(t, f, sequence, &orphan, 0, id, tcpFlagPsh|tcpFlagAck, []byte("policy-blocked reset"))
		<-entered
		fill := budget.Available()
		if !budget.TryReserve(fill) {
			t.Fatal("could not fill policy-control budget")
		}
		defer budget.Release(fill)
		unblock()
		synctest.Wait()
		if present, _ := reliableIngressCumulativeHead(sequence); present || sequence.ctx.Err() == nil {
			t.Fatal("policy-blocked reset falsely settled the incoming orphan")
		}
		if ack := sequence.ackWindow.Snapshot(false); ack.selectiveAcks[id].messageId == id {
			t.Fatal("policy-blocked reset falsely selectively ACKed the incoming orphan")
		}
	})
}
