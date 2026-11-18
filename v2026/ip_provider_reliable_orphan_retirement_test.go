// TCP retirement preserves shared reliable ingress without crossing flow
// generations or bypassing the final custody of live packets.
package connect

import (
	"bytes"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"unsafe"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Takes a synthetic packet through the actual receive ledger with an exact TCP
// sequence number, so old and replacement generation bytes stay distinguishable.
func providerReliableGenerationPack(t *testing.T, f *reliableTcpIngressFixture, sequence *ReceiveSequence,
	path *IpPath, number uint64, id Id, flags byte, tcpSequence uint32, payload []byte) {
	t.Helper()
	packet := MessagePoolCopy(ipOosTcpPacketSequence(path, flags, tcpSequence, payload))
	frame, err := ipPacketToProviderFrame(packet, DefaultProtocolVersion)
	if err != nil || !frame.Raw {
		MessagePoolReturn(packet)
		t.Fatalf("generation fixture frame=%+v err=%v", frame, err)
	}
	pack := &ReceivePack{Source: f.source, SequenceId: sequence.sequenceId,
		Pack: &protocol.Pack{MessageId: id.Bytes(), SequenceId: sequence.sequenceId.Bytes(),
			SequenceNumber: number, Head: number == 0, Frames: []*protocol.Frame{frame}},
		ReceiveCallback: f.client.receiveCallback, Ctx: f.ctx, Unwrapped: true,
		EncryptionRole: sequenceTlsRoleServer, MessageByteCount: ByteCount(len(packet))}
	if accepted, err := sequence.Pack(pack, 0); !accepted || err != nil {
		pack.messagePoolReturn()
		t.Fatalf("generation Pack admission: accepted=%t err=%v", accepted, err)
	}
}

// The original payload has already selected its full TCP owner. A distinct SYN
// replaces that owner at an exact boundary; neither old bytes nor an old reset
// may touch the successor, and both live flows retain real admission custody.
func runProviderReliableOrphanReplacement(t *testing.T, afterLookup bool, stop string) {
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
							t.Errorf("ipv%d protocol-%d after-lookup=%t leaked generation ownership: used=%d reserved=%d released=%d",
								version, protocolVersion, afterLookup, budget.UsedByteCount(), budget.reservedByteCount.Load(), budget.releasedByteCount.Load())
						}
					}
				})
				f := newReliableTcpIngressFixtureWithPath(t, reliableDeliveryTestPath(version), natBudget)
				f.establishForControl()
				f.provider.settings.ProtocolVersion = protocolVersion
				sequence := f.runningReceiveSequence(reliableIngressBudgetSettings(receiveBudget))
				path := *f.path
				path.SourcePort++
				created := make(chan *TcpSequence, 2)
				f.nat.settings.TcpBufferSettings.beforeSequenceRunWithStateForTest = func(flow *TcpSequence) { created <- flow }
				synId := NewId()
				orphanPayloadSyn(t, f, sequence, &path, 0, synId)
				old := <-created
				oldId, siblingId, freshId := NewId(), NewId(), NewId()
				oldPayload := []byte("only the retired generation may consume this")
				siblingPayload := []byte("healthy sibling still needs TCP custody")
				freshPayload := []byte("replacement must own these new bytes")
				providerReliableGenerationPack(t, f, sequence, &path, 1, oldId, tcpFlagPsh|tcpFlagAck, 101, oldPayload)
				providerReliableGenerationPack(t, f, sequence, f.path, 2, siblingId, tcpFlagPsh|tcpFlagAck, 101, siblingPayload)
				synctest.Wait()
				if present, head := reliableIngressCumulativeHead(sequence); !present || head.messageId != synId {
					t.Fatal("fixture did not bind the original pending payload to its full TCP generation")
				}
				var returns atomic.Int32
				f.provider.afterReturnEnqueueForTest = func(bool) { returns.Add(1) }
				entered, release := make(chan struct{}), make(chan struct{})
				var releaseOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				defer unblock()
				var lookups atomic.Int32
				if afterLookup {
					f.nat.settings.TcpBufferSettings.beforeSequenceSendForTest = func(flow *TcpSequence) {
						if flow == old && lookups.Add(1) == 1 {
							close(entered)
							<-release
						}
					}
					f.nat.reliableCapacity.notify()
					<-entered
				}
				// Drain completed fixture notifications before the replacement's
				// exact NAT completion. No test wait runs while the ledger is locked.
				for len(f.processed) != 0 {
					<-f.processed
				}
				var replacement *TcpSequence
				func() {
					if !afterLookup {
						// Quiescence above proves no retry is executing. Keep the
						// receive worker before its next pump while the NAT atomically
						// installs the successor; this forces the between-retries case.
						sequence.deliveryQueue.mutex.Lock()
						defer sequence.deliveryQueue.mutex.Unlock()
					}
					packet := MessagePoolCopy(ipOosTcpPacketSequence(&path, tcpFlagSyn, 700, nil))
					if !f.nat.SendPacket(f.source, protocol.ProvideMode_Network, packet, 0) {
						MessagePoolReturn(packet)
						t.Fatal("replacement SYN did not enter the real NAT")
					}
					replacement = <-created
					<-f.processed
					if replacement == old || replacement.initialSynSeq != 700 || old.ctx.Err() == nil || len(replacement.sendItems) != 1 {
						t.Fatal("fixture did not atomically replace the selected TCP generation")
					}
					// A wrong retry could now actually enqueue stale bytes. Leaving
					// this queue full would hide that corruption behind backpressure.
					item := <-replacement.sendItems
					if !item.tcp.syn || item.tcp.seq != 700 {
						t.Error("replacement lost its exact initiating SYN")
					}
					item.release()
				}()
				switch stop {
				case "source":
					f.provider.stateLock.Lock()
					lifecycle := f.provider.sourceLifecycles[f.source.SourceId]
					f.provider.stateLock.Unlock()
					if lifecycle == nil {
						t.Fatal("replacement fixture lost the retained source lease")
					}
					lifecycle.admissions.close()
					lifecycle.cancel()
				case "provider":
					f.provider.cancel()
				case "nat":
					f.nat.Close()
				}
				unblock()
				f.nat.reliableCapacity.notify()
				synctest.Wait()
				if stop != "" {
					if present, head := reliableIngressCumulativeHead(sequence); !present || head.messageId != synId {
						t.Fatalf("%s shutdown falsely settled the old generation", stop)
					}
					if ack := sequence.ackWindow.Snapshot(false); ack.selectiveAcks[oldId].messageId == oldId || ack.selectiveAcks[siblingId].messageId == siblingId {
						t.Fatalf("%s shutdown falsely selectively ACKed retained data", stop)
					}
					if sequence.ctx.Err() == nil || len(replacement.sendItems) != 0 || returns.Load() != 0 {
						t.Fatalf("%s shutdown gained an obsolete-generation exception", stop)
					}
					return
				}
				if sequence.ctx.Err() != nil || replacement.ctx.Err() != nil || f.tcp.ctx.Err() != nil || f.provider.ctx.Err() != nil {
					t.Fatalf("ipv%d protocol-%d after-lookup=%t replacement retirement canceled shared work: sequence=%v replacement=%v sibling=%v provider=%v",
						version, protocolVersion, afterLookup, sequence.ctx.Err(), replacement.ctx.Err(), f.tcp.ctx.Err(), f.provider.ctx.Err())
				}
				if len(replacement.sendItems) != 0 || returns.Load() != 0 {
					t.Fatalf("ipv%d protocol-%d after-lookup=%t obsolete input crossed into its replacement: packets=%d resets=%d",
						version, protocolVersion, afterLookup, len(replacement.sendItems), returns.Load())
				}
				if present, head := reliableIngressCumulativeHead(sequence); !present || head.messageId != oldId {
					t.Fatal("obsolete generation did not settle only its own receipt")
				}
				if afterLookup && lookups.Load() != 1 {
					t.Fatal("fixture did not hold exactly one selected-generation admission")
				}
				providerReliableGenerationPack(t, f, sequence, &path, 1, oldId, tcpFlagPsh|tcpFlagAck, 101, oldPayload)
				// Refill through the real NAT with the successor's own SYN
				// retransmission. Fresh successor data must now wait for custody.
				packet := MessagePoolCopy(ipOosTcpPacketSequence(&path, tcpFlagSyn, 700, nil))
				if !f.nat.SendPacket(f.source, protocol.ProvideMode_Network, packet, 0) {
					MessagePoolReturn(packet)
					t.Fatal("replacement SYN retransmission did not enter NAT")
				}
				<-f.processed
				providerReliableGenerationPack(t, f, sequence, &path, 3, freshId, tcpFlagPsh|tcpFlagAck, 701, freshPayload)
				synctest.Wait()
				if len(replacement.sendItems) != 1 || returns.Load() != 0 {
					t.Fatal("settled duplicate changed replacement custody")
				}
				if present, head := reliableIngressCumulativeHead(sequence); !present || head.messageId != oldId {
					t.Fatal("live-generation payload gained a terminal synthetic ACK")
				}
				if ack := sequence.ackWindow.Snapshot(false); ack.selectiveAcks[siblingId].messageId == siblingId || ack.selectiveAcks[freshId].messageId == freshId {
					t.Fatal("pending live-generation data gained a false selective ACK")
				}
				(<-f.tcp.sendItems).release()
				(<-replacement.sendItems).release()
				synctest.Wait()
				for _, expected := range []struct {
					flow    *TcpSequence
					seq     uint32
					payload []byte
				}{
					{flow: f.tcp, seq: 101, payload: siblingPayload},
					{flow: replacement, seq: 701, payload: freshPayload},
				} {
					select {
					case item := <-expected.flow.sendItems:
						if item.tcp.seq != expected.seq || !bytes.Equal(item.tcp.payload, expected.payload) || item.memory.budget != natBudget {
							t.Error("live generation lost its exact packet or prepaid owner")
						}
						item.release()
					default:
						t.Fatal("obsolete generation prevented a healthy flow's final admission")
					}
				}
				if present, head := reliableIngressCumulativeHead(sequence); !present || head.messageId != freshId || sequence.ctx.Err() != nil {
					t.Fatal("healthy flows did not advance the same receive sequence")
				}
				providerReliableGenerationPack(t, f, sequence, f.path, 2, siblingId, tcpFlagPsh|tcpFlagAck, 101, siblingPayload)
				providerReliableGenerationPack(t, f, sequence, &path, 3, freshId, tcpFlagPsh|tcpFlagAck, 701, freshPayload)
				synctest.Wait()
				if len(f.tcp.sendItems) != 0 || len(replacement.sendItems) != 0 || returns.Load() != 0 || f.dials.Load() != 0 {
					t.Fatal("duplicates crossed generation ownership or depended on WAN delivery")
				}
			})
		}
	}
}

// An in-progress old lookup cannot tear down siblings or touch a live successor.
func TestProviderReliableOrphanPayloadReplacementAfterLookupPreservesSameSequence(t *testing.T) {
	runProviderReliableOrphanReplacement(t, true, "")
}

// Capacity retries preserve their first generation binding after map replacement.
func TestProviderReliableOrphanPayloadReplacementBetweenRetriesPreservesSameSequence(t *testing.T) {
	runProviderReliableOrphanReplacement(t, false, "")
}

// Superseded-flow handling cannot override explicit provider, source or NAT
// shutdown, even with a live successor already installed for the same tuple.
func TestProviderReliableOrphanReplacementAuthorityNeverAcked(t *testing.T) {
	for _, stop := range []string{"source", "provider", "nat"} {
		runProviderReliableOrphanReplacement(t, true, stop)
	}
}

// The adjacent empty-control branch keeps its terminal outcome. RST applies
// under the map lock; a following SYN is secured only in its actual new queue.
func TestProviderReliableRetirementControlsPreserveSameSequence(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, version := range []int{4, 6} {
		for _, flags := range []byte{tcpFlagAck, tcpFlagFin, tcpFlagFin | tcpFlagAck, tcpFlagRst} {
			synctest.Test(t, func(t *testing.T) {
				budget := NewTransferMemoryBudget(mib(2))
				t.Cleanup(func() {
					if budget.UsedByteCount() != 0 || budget.reservedByteCount.Load() != budget.releasedByteCount.Load() {
						t.Errorf("ipv%d flags=%#x leaked control ownership", version, flags)
					}
				})
				f := newReliableTcpIngressFixtureWithPath(t, reliableDeliveryTestPath(version), budget)
				f.establishForControl()
				sequence := f.runningReceiveSequence()
				var lookups atomic.Int32
				f.nat.settings.TcpBufferSettings.beforeSequenceSendForTest = func(flow *TcpSequence) {
					if flow == f.tcp {
						lookups.Add(1)
						flow.Cancel()
					}
				}
				id := NewId()
				providerReliableGenerationPack(t, f, sequence, f.path, 0, id, flags, 101, nil)
				synctest.Wait()
				wantLookups := int32(1)
				if flags == tcpFlagRst {
					wantLookups = 0
				}
				if sequence.ctx.Err() != nil || f.provider.ctx.Err() != nil || lookups.Load() != wantLookups || f.tcp.ctx.Err() == nil {
					t.Fatalf("ipv%d flags=%#x lost the exact terminal control boundary", version, flags)
				}
				if present, head := reliableIngressCumulativeHead(sequence); !present || head.messageId != id {
					t.Fatal("applied terminal control did not settle its own receipt")
				}
				next := *f.path
				next.SourcePort++
				created := make(chan *TcpSequence, 1)
				f.nat.settings.TcpBufferSettings.beforeSequenceRunWithStateForTest = func(flow *TcpSequence) { created <- flow }
				nextId := NewId()
				providerReliableGenerationPack(t, f, sequence, &next, 1, nextId, tcpFlagSyn, 700, nil)
				flow := <-created
				synctest.Wait()
				if present, head := reliableIngressCumulativeHead(sequence); !present || head.messageId != nextId || len(flow.sendItems) != 1 {
					t.Fatal("terminal control prevented the new SYN's actual custody")
				}
				item := <-flow.sendItems
				if !item.tcp.syn || item.tcp.seq != 700 {
					t.Error("next generation did not own its exact initiating SYN")
				}
				item.release()
				providerReliableGenerationPack(t, f, sequence, &next, 1, nextId, tcpFlagSyn, 700, nil)
				synctest.Wait()
				if len(flow.sendItems) != 0 || sequence.ctx.Err() != nil || f.dials.Load() != 0 {
					t.Fatal("new SYN duplicate repeated admission or depended on WAN delivery")
				}
			})
		}
	}
}

// The retained receive charge includes the bounded owner and a conservative
// channel-object allowance; a generation token does not retain a TcpSequence.
func TestProviderReliableGenerationOwnerFitsRetainedMetadata(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newReliableTcpIngressFixtureWithPath(t, reliableDeliveryTestPath(4), nil)
		budget := NewTransferMemoryBudget(kib(128))
		sequence := f.runningReceiveSequence(reliableIngressBudgetSettings(budget))
		packet := MessagePoolCopy(ipOosTcpPacketSequence(f.path, tcpFlagPsh|tcpFlagAck, 101, []byte("charged generation owner")))
		defer MessagePoolReturn(packet)
		frame, err := ipPacketToProviderFrame(packet, DefaultProtocolVersion)
		if err != nil || !frame.Raw {
			t.Fatal("metadata fixture did not create a borrowed raw frame")
		}
		item := &receiveItem{ack: true, frames: []*protocol.Frame{frame}}
		before := item.queueByteCount
		sequence.prepareReceiveDeliveryCredit(item)
		metadata := item.queueByteCount - before - item.deliveryPrepaid
		if ownerBytes := ByteCount(unsafe.Sizeof(providerReliablePacket{})); ownerBytes > 320 {
			t.Fatalf("generation owner exceeds its NAT metadata reservation: bytes=%d reserved=320", ownerBytes)
		}
		required := ByteCount(unsafe.Sizeof(receiveDeliveryReceipt{})+unsafe.Sizeof(receiveDeliveryOperation{})+
			unsafe.Sizeof(receiveDeliveryClaim{})+unsafe.Sizeof(providerReliablePacket{})) + 128
		if !item.deliveryPrepared || metadata < required {
			t.Fatalf("retained generation metadata is undercharged: reserved=%d required=%d", metadata, required)
		}
		if budget.UsedByteCount() != 0 {
			t.Fatal("metadata sizing unexpectedly reserved packet memory")
		}
	})
}
