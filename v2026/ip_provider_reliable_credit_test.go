package connect

import (
	"bytes"
	"fmt"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/v2026/protocol"
)

func reliableIngressBudgetSettings(budget *TransferMemoryBudget) func(*ReceiveBufferSettings) {
	return func(settings *ReceiveBufferSettings) {
		settings.ReceiveQueueBudget = budget
		settings.ReceiveQueueMinByteCount = 0
		settings.ReceiveQueueMaxByteCount = budget.TotalByteCount()
		settings.ReceiveQueueRetainedByteAccounting = true
		settings.ReceiveHoldPolicy = ReceiveHoldCommittedPrefix
	}
}

// A selectively leased item prepays future NAT ownership before the common
// root fills. Moving that labeled extra credit must preserve every still-live
// receive root, obey the target child ceiling, and require no root growth.
func TestProviderReliableTcpPrepaidSharedRootHandoff(t *testing.T) {
	for _, version := range []int{4, 6} {
		for _, ceiling := range []bool{false, true} {
			t.Run(fmt.Sprintf("ipv%d/child-ceiling-%t", version, ceiling), func(t *testing.T) {
				assertMessagePoolOwnership(t)
				synctest.Test(t, func(t *testing.T) {
					root := NewTransferMemoryBudget(mib(4))
					natBudget := NewTransferMemoryBudgetWithParent(mib(3), root)
					receiveBudget := NewTransferMemoryBudgetWithParent(kib(64), root)
					t.Cleanup(func() {
						for _, budget := range []*TransferMemoryBudget{root, natBudget, receiveBudget} {
							if budget.UsedByteCount() != 0 || budget.reservedByteCount.Load() != budget.releasedByteCount.Load() {
								t.Errorf("final owner accounting unbalanced: used=%d reserved=%d released=%d", budget.UsedByteCount(), budget.reservedByteCount.Load(), budget.releasedByteCount.Load())
							}
						}
					})
					f := newReliableTcpIngressFixture(t, version, natBudget)
					f.establishForControl()
					sequence := f.runningReceiveSequence(reliableIngressBudgetSettings(receiveBudget))
					payload := bytes.Repeat([]byte{0x6d}, 1200)
					id := f.pack(sequence, 1, payload)
					synctest.Wait()
					held := sequence.receiveQueue.GetByMessageId(id)
					if held == nil || !held.committed || held.deliveryPrepaid == 0 || held.memoryBudget != receiveBudget {
						t.Fatal("selective lease did not prepay its exact downstream ownership")
					}
					originalCharge, prepaid := held.queueByteCount, held.deliveryPrepaid
					rootBytes := MessagePoolRootByteCount(held.frames[0].MessageBytes)
					t.Logf("retained=%dB downstream-prepaid=%dB original-root=%dB", originalCharge, prepaid, rootBytes)
					witness := MessagePoolShareReadOnly(held.frames[0].MessageBytes)
					defer MessagePoolReturn(witness)
					fill := root.Available()
					if !root.TryReserve(fill) {
						t.Fatal("could not fill common root after exact SACK lease")
					}
					defer root.Release(fill)
					if ceiling {
						natBudget.SetTotalByteCount(natBudget.UsedByteCount())
						defer natBudget.SetTotalByteCount(mib(3))
					}
					rootReserved, rootReleased := root.reservedByteCount.Load(), root.releasedByteCount.Load()
					controlID := f.pack(sequence, 0, nil)
					synctest.Wait()
					if present, head := reliableIngressCumulativeHead(sequence); !present || head.messageId != controlID {
						t.Fatal("shared-root pressure blocked control or cumulatively ACKed data")
					}
					if ceiling {
						if held.deliveryPrepaid != prepaid || receiveBudget.UsedByteCount() != originalCharge {
							t.Fatal("target-child refusal consumed source credit")
						}
						natBudget.SetTotalByteCount(mib(3))
						synctest.Wait()
					}
					if held.deliveryPrepaid >= prepaid || held.queueByteCount < rootBytes ||
						receiveBudget.UsedByteCount() != held.queueByteCount {
						t.Fatal("handoff either failed or removed still-live receive-root credit")
					}
					if root.UsedByteCount() != root.TotalByteCount() || root.reservedByteCount.Load() != rootReserved || root.releasedByteCount.Load() != rootReleased {
						t.Fatal("same-root handoff released/re-reserved shared ownership")
					}
					(<-f.tcp.sendItems).release()
					synctest.Wait()
					select {
					case item := <-f.tcp.sendItems:
						if !bytes.Equal(item.tcp.payload, payload) || item.memory.budget != natBudget {
							t.Error("final TCP owner lost packet or exact NAT child claim")
						}
						item.release()
					default:
						t.Fatal("prepaid original did not enter final TCP queue")
					}
					if present, head := reliableIngressCumulativeHead(sequence); !present || head.messageId != id || receiveBudget.UsedByteCount() != 0 {
						t.Fatal("final admission did not release lease and advance exact head")
					}
				})
			})
		}
	}
}

func TestProviderReliableTcpPrepayRefusalBeforeSelectiveAck(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newReliableTcpIngressFixture(t, 4, nil)
		budget := NewTransferMemoryBudget(kib(2))
		sequence := f.runningReceiveSequence(reliableIngressBudgetSettings(budget))
		id := f.pack(sequence, 1, []byte("fits raw root but not prepaid ownership envelope"))
		synctest.Wait()
		if sequence.receiveQueue.GetByMessageId(id) != nil || budget.UsedByteCount() != 0 || sequence.nextSequenceNumber != 0 {
			t.Fatal("refused prepay consumed a receive position or budget")
		}
		ack := sequence.ackWindow.Snapshot(false)
		if ack.ackUpdateCount != 0 || len(ack.selectiveAcks) != 0 {
			t.Fatal("prepay-refused out-of-order item received selective credit")
		}
	})
}

func TestReceiveDeliveryHeadPrepayParticipatesInByteLimit(t *testing.T) {
	assertMessagePoolOwnership(t)
	q, _ := testReceiveDeliveryLedger(t, 1)
	q.sequence.client = &Client{}
	q.sequence.client.reliableProviderIngress.Store(true)
	budget := NewTransferMemoryBudget(kib(64))
	q.sequence.receiveQueue = newReceiveQueue(budget, 0)
	q.sequence.receiveQueue.setLifetimeBudget()
	item := testReceiveDeliveryTcpControl(0)
	// Use actual payload so the independent small-control reserve is ineligible.
	item.messagePoolReturn()
	packet := MessagePoolCopy(ipOosTcpPacketSequence(icmpTcpTestPath(4), tcpFlagAck, 101, []byte("data")))
	item = &receiveItem{transferItem: transferItem{messageId: NewId(), queueByteCount: MessagePoolRootByteCount(packet)}, ack: true,
		frames: []*protocol.Frame{{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: packet}}}
	q.maxBytes = item.QueueByteCount() + 512
	if _, accepted := q.append(item, true); accepted {
		t.Fatal("head admission tested the old byte count before prepay")
	}
	if q.bytes != 0 || budget.UsedByteCount() != 0 || len(q.items) != 0 || item.deliveryPrepaid == 0 {
		t.Fatal("refused head prepay changed owner counters")
	}
	item.messagePoolReturn()
	q.sequence.receiveQueue.Clear()
}

func TestProviderReliableTcpSameBurstDuplicateWaitsForReceipt(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newReliableTcpIngressFixture(t, 4, nil)
		sequence := NewReceiveSequence(f.ctx, f.client, f.source, NewId(), sequenceTlsRoleServer, false, DefaultReceiveBufferSettings())
		sequence.peerAudit = NewSequencePeerAudit(f.client, f.source, sequence.receiveBufferSettings.MaxPeerAuditDuration)
		t.Cleanup(sequence.peerAudit.Complete)
		f.client.ContractManager().AddNoContractPeer(f.source.SourceId)
		id := NewId()
		for range 2 {
			packet := MessagePoolCopy(ipOosTcpPacketSequence(f.path, tcpFlagAck, 101, []byte("same burst")))
			frame, _ := ipPacketToProviderFrame(packet, DefaultProtocolVersion)
			pack := &ReceivePack{Pack: &protocol.Pack{MessageId: id.Bytes(), SequenceNumber: 0, Head: true, Frames: []*protocol.Frame{frame}},
				ReceiveCallback: f.client.receiveCallback, Unwrapped: true, MessageByteCount: ByteCount(len(packet))}
			accepted, err := sequence.receive(pack)
			if err != nil {
				t.Fatal(err)
			}
			if !accepted {
				pack.messagePoolReturn()
			}
		}
		if sequence.ackWindow.Snapshot(false).ackUpdateCount != 0 {
			t.Fatal("same-burst duplicate ACKed a not-yet-dispatched item")
		}
		go runReliableIngressFixtureDelivery(sequence)
		synctest.Wait()
		if sequence.ackWindow.Snapshot(false).ackUpdateCount != 0 {
			t.Fatal("blocked final owner ACKed after same-burst dispatch")
		}
		f.cancel()
		synctest.Wait()
	})
}
