// A real signed successor announcement occupies a reliable sequence position.
// Its physical storage must not depend on its application payload being nonzero.
package connect

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

func TestReceivePublicContractAheadCurrentHoleUnderForeignRxPressure(t *testing.T) {
	GetMessagePoolAggregateStats()
	blocked := false
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		// Small signed grants force the ordinary public rollover promptly. All
		// capacities and protocol settings are immutable before either Client.
		layout := consumerPaidPackByteLayout{label: "inflight-control", byteLimit: 174762, leafBytes: 64 * 1024, rootBytes: 640 * 1024, countLimit: 85}
		root := NewTransferMemoryBudget(layout.rootBytes)
		rx := NewTransferMemoryBudgetWithParent(layout.leafBytes, root)
		head := NewTransferMemoryBudgetWithParent(layout.leafBytes, root)
		pack := NewTransferMemoryBudgetWithParent(layout.leafBytes, root)
		t.Cleanup(func() {
			for _, budget := range []*TransferMemoryBudget{root, rx, head, pack} {
				assertRetainedBudgetBalance(t, budget)
				if budget.Stats().QuotaByteCount != 0 {
					t.Error("inflight hole retained shared Pack quota")
				}
			}
		})
		var target atomic.Pointer[Id]
		var targetCumulative, targetSelective, actualAckReceipts atomic.Uint64
		progress := func(event TransferProgressEvent) {
			if event.Stage != "receive_ack_begin" {
				return
			}
			actualAckReceipts.Add(1)
			if id := target.Load(); id != nil && event.MessageId == *id {
				if event.Selective {
					targetSelective.Add(1)
				} else {
					targetCumulative.Add(1)
				}
			}
		}
		fixture, log, _, _ := newConsumerPaidPackByteFixture(t, layout, rx, head, pack, progress, func(receiver bool, settings *ClientSettings) {
			settings.ContractManagerSettings.InitialContractTransferByteCount = 64 * 1024
			settings.ContractManagerSettings.StandardContractTransferByteCount = 64 * 1024
			settings.SendBufferSettings.ContractAheadFloorByteCount = 16 * 1024
			if receiver {
				settings.ReceiveBufferSettings.AdvertiseReceiveWindow = true
			}
		})
		entered, released := make(chan struct{}), make(chan struct{})
		var enterOnce, releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(released) }) }
		t.Cleanup(unblock)
		var held atomic.Bool
		var targetDeliveries, reversePackets atomic.Int64
		fixture.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, peer Peer) {
			if peer.TransferKey.LogicalLane == 0 && held.Load() {
				enterOnce.Do(func() {
					close(entered)
					select {
					case <-released:
					case <-fixture.ctx.Done():
					}
				})
			}
			if peer.TransferKey.LogicalLane == 1 {
				targetDeliveries.Add(int64(len(frames)))
			}
		})
		fixture.sender.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
			for _, frame := range frames {
				if frame.MessageType == protocol.MessageType_IpIpPacketToProvider && frame.Raw && len(frame.MessageBytes) == 40 {
					reversePackets.Add(1)
				}
			}
		})
		var receipts [512]atomic.Bool
		next := 0
		var announcement *windowRoundFrame
		// Unlike the older byte-limit helper, retain actual control outputs;
		// never synthesize a Pack, ACK, contract, sequence number or retry.
		send := func(lane uint32, count, payload int) *windowRoundFrame {
			if next == len(receipts) {
				t.Fatal("bounded public send allowance exhausted")
			}
			slot := next
			next++
			frames := make([]*protocol.Frame, count)
			for index := range frames {
				packet := ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagAck, uint32(1000+slot*8+index), bytes.Repeat([]byte{0x62}, payload))
				frames[index] = &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider, Raw: true, MessageBytes: MessagePoolCopy(packet)}
			}
			if !fixture.sender.SendMultiWithTimeout(frames, fixture.receiver.ClientId(), func(err error) {
				if err == nil {
					receipts[slot].Store(true)
				}
			}, time.Second, TransferKey{LogicalLane: lane}) {
				for _, frame := range frames {
					MessagePoolReturn(frame.MessageBytes)
				}
				t.Fatal("public sender refused pressure fixture input")
			}
			for range cap(fixture.senderOut) {
				wire := fixture.take(fixture.senderOut)
				if wire.pack == nil {
					fixture.forward(wire, fixture.receiverIn)
					continue
				}
				if wire.pack.ContractAhead && wire.pack.LogicalLane == 1 {
					announcement = wire
					continue
				}
				if len(wire.pack.Frames) == 0 {
					fixture.forward(wire, fixture.receiverIn)
					fixture.acknowledge()
					continue
				}
				return wire
			}
			t.Fatal("real public data did not reach the owned wire")
			return nil
		}
		warm := send(1, 1, 1240)
		fixture.forward(warm, fixture.receiverIn)
		fixture.acknowledge()
		sequence := consumerForeignHeadSequence(t, fixture, warm)
		sender := inflightHoleSender(t, fixture, warm)
		for range 64 {
			if sender.sendContractRemainingByteCount() <= sender.announceAheadByteCount()+2*1280 {
				break
			}
			wire := send(1, 1, 1240)
			fixture.forward(wire, fixture.receiverIn)
			fixture.acknowledge()
		}
		if announcement != nil || sender.aheadSendContract != nil || len(sender.sendItems) != 0 || sequence.receiveContract == nil || !sequence.receiveContract.verifiedReceiveProofSet || root.UsedByteCount() != 0 {
			t.Fatal("target was not genuinely warm, verified and quiet immediately before its real announcement threshold")
		}
		// Trigger the actual successor while RX is still free. Keep its wire
		// unoffered, and deliver/ACK every real prerequisite before pressure.
		var following *windowRoundFrame
		for range 4 {
			wire := send(1, 1, 1240)
			if announcement != nil && wire.pack.SequenceNumber > announcement.pack.SequenceNumber {
				following = wire
			} else {
				fixture.forward(wire, fixture.receiverIn)
				fixture.acknowledge()
			}
			synctest.Wait()
			for len(fixture.senderOut) != 0 {
				extra := fixture.take(fixture.senderOut)
				if extra.pack != nil && extra.pack.ContractAhead && extra.pack.LogicalLane == 1 {
					announcement = extra
				} else if extra.pack != nil && announcement != nil && extra.pack.LogicalLane == 1 && extra.pack.SequenceNumber > announcement.pack.SequenceNumber {
					if following != nil {
						t.Fatal("more than one held following target wire")
					}
					following = extra
				} else {
					fixture.forward(extra, fixture.receiverIn)
				}
			}
			fixture.acknowledge()
			if announcement != nil {
				break
			}
		}
		if announcement == nil || announcement.pack.SequenceNumber != sequence.holdPromise.ingressFloor {
			t.Fatal("real announcement predecessor did not deliver and ACK before foreign pressure")
		}
		foreignWarm := send(0, 1, 1240)
		fixture.forward(foreignWarm, fixture.receiverIn)
		fixture.acknowledge()
		held.Store(true)
		seed := send(0, 1, 1)
		fixture.forward(seed, fixture.receiverIn)
		<-entered
		// Every occupied byte is a decoded original held by the real foreign
		// worker or its real queue. Small payload-bearing TCP DATA makes the residue less
		// than one actual control envelope without anonymous TryReserve filler.
		// Its nonzero application byte keeps foreign pressure DATA after the
		// protocol-only classification change.
		var foreignUnsent *windowRoundFrame
		for range layout.countLimit - 2 {
			wire := send(0, 1, 1)
			path, err := FilteredTransferPath(wire.bytes)
			if err != nil {
				t.Fatal(err)
			}
			scan, status := scanReceiveMemoryPack(wire.bytes, path)
			if status != receiveMemoryPackScanReady {
				t.Fatal("foreign public packet did not scan")
			}
			if rx.Available() < scan.retainedByteCount {
				foreignUnsent = wire
				break
			}
			fixture.forward(wire, fixture.receiverIn)
		}
		if announcement == nil || announcement.pack.Head || !announcement.pack.ContractAhead || len(announcement.pack.Frames) != 0 || announcement.pack.ContractFrame == nil || announcement.pack.ContractId != nil || announcement.pack.SequenceNumber != sequence.holdPromise.ingressFloor {
			if announcement != nil {
				t.Logf("ACTUAL_ANNOUNCEMENT_SHAPE head=%t ahead=%t frames=%d proof=%t compact=%t number=%d lane=%d", announcement.pack.Head, announcement.pack.ContractAhead, len(announcement.pack.Frames), announcement.pack.ContractFrame != nil, announcement.pack.ContractId != nil, announcement.pack.SequenceNumber, announcement.pack.LogicalLane)
			}
			t.Fatalf("real public sender did not emit exact current announcement: present=%t floor=%d sender_next=%d send_items=%d remaining=%d threshold=%d ahead_supported=%t ahead_present=%t rx_free=%d drops=%d", announcement != nil, sequence.holdPromise.ingressFloor, sender.nextSequenceNumber, len(sender.sendItems), sender.sendContractRemainingByteCount(), sender.announceAheadByteCount(), sender.contractAheadSupported.Load(), sender.aheadSendContract != nil, rx.Available(), fixture.receiver.ReceiveStats().PackHandoffDropCount)
		}
		id := RequireIdFromBytes(announcement.pack.MessageId)
		target.Store(&id)
		path, err := FilteredTransferPath(announcement.bytes)
		if err != nil {
			t.Fatal(err)
		}
		scan, status := scanReceiveMemoryPack(announcement.bytes, path)
		if status != receiveMemoryPackScanReady || rx.Available() >= scan.retainedByteCount || head.Available() < scan.retainedByteCount || root.Available() < scan.retainedByteCount || sequence.holdPromise.ingressHead != nil || sequence.holdPromise.set {
			t.Fatal("actual current announcement did not isolate foreign RX pressure with independent Head/root room")
		}
		original := proto.Clone(announcement.pack).(*protocol.Pack)
		beforeDrops := fixture.receiver.ReceiveStats().PackHandoffDropCount
		fixture.forward(announcement, fixture.receiverIn)
		fixture.acknowledge()
		blocked = fixture.receiver.ReceiveStats().PackHandoffDropCount != beforeDrops
		if blocked && (targetCumulative.Load() != 0 || targetSelective.Load() != 0 || sequence.holdPromise.ingressFloor != original.SequenceNumber) {
			t.Fatal("undelivered announcement gained a false ACK/SACK or advanced the floor")
		}
		// Public reverse TCP ACK traffic still crosses the same Client pump;
		// its actual wire and actual Transfer ACK are the progress witness.
		var reverseDone atomic.Bool
		packet := MessagePoolCopy(ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagAck, 7000, nil))
		if !fixture.receiver.SendMultiWithTimeout([]*protocol.Frame{{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: packet}}, fixture.sender.ClientId(), func(err error) { reverseDone.Store(err == nil) }, time.Second, TransferKey{LogicalLane: 2}) {
			MessagePoolReturn(packet)
			t.Fatal("real reverse TCP ACK input was refused")
		}
		fixture.acknowledge()
		synctest.Wait()
		// This receiver is the sender-side default ACK compressor. Advance
		// exactly its configured release timer, not an arbitrary deadline.
		time.Sleep(fixture.sender.settings.ReceiveBufferSettings.AckCompressTimeout)
		synctest.Wait()
		for len(fixture.senderOut) != 0 {
			wire := fixture.take(fixture.senderOut)
			if wire.ack != nil {
				fixture.forward(wire, fixture.receiverIn)
			}
		}
		synctest.Wait()
		if !reverseDone.Load() || reversePackets.Load() != 1 || actualAckReceipts.Load() == 0 {
			t.Fatalf("unrelated real TCP ACK/Transfer ACK progress stopped behind the physical current hole: completed=%t packets=%d actual_ack_receipts=%d sender_pending=%d receiver_pending=%d", reverseDone.Load(), reversePackets.Load(), actualAckReceipts.Load(), len(fixture.senderOut), len(fixture.receiverOut))
		}
		// Recover through an actual sender retransmission after releasing the
		// foreign application owner; no manual original replay or forged ACK.
		unblock()
		synctest.Wait()
		fixture.acknowledge()
		if foreignUnsent != nil {
			// This source was held before its first receiver offer. Forward it
			// only as explicit fixture cleanup, never as target recovery.
			fixture.forward(foreignUnsent, fixture.receiverIn)
			fixture.acknowledge()
		}
		if blocked {
			retry := pressureContractNaturalRetry(t, fixture, original)
			if !bytes.Equal(retry.pack.MessageId, original.MessageId) || retry.pack.SequenceNumber != original.SequenceNumber || !retry.pack.ContractAhead {
				t.Fatal("natural retry lost its exact announcement identity")
			}
			fixture.forward(retry, fixture.receiverIn)
			fixture.acknowledge()
		}
		if following != nil {
			fixture.forward(following, fixture.receiverIn)
			fixture.acknowledge()
		}
		if targetCumulative.Load() == 0 || targetSelective.Load() != 0 || sequence.holdPromise.ingressFloor <= original.SequenceNumber || sender.aheadSendContract == nil || !sender.aheadSendContract.acknowledgedAhead || root.UsedByteCount() != 0 || len(log.messages()) != 0 {
			t.Fatal("real successor verification/ACK/floor progression or exact ownership did not drain")
		}
		// The fixture's paused key publisher is owned by its parent context.
		// Release that owner before joining either Client's manager workers.
		fixture.cancel()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if fixture.sender.CloseAndWait(ctx) != nil || fixture.receiver.CloseAndWait(ctx) != nil {
			t.Fatal("inflight hole clients did not join")
		}
		t.Logf("INFLIGHT_HOLE_BEHAVIOR_DRAINED current=%d cost=%d blocked=%t reverse_ack_packets=1 successor_verified=true", original.SequenceNumber, scan.retainedByteCount, blocked)
	})
	if !t.Failed() {
		t.Log("INFLIGHT_HOLE_CLEANUP_JOINED clients=true budgets=true pool=true")
		if blocked {
			t.Error("CURRENT_CONTRACT_AHEAD_REFUSED_WITH_INDEPENDENT_HEAD_CAPACITY")
		}
	}
}

// Read only at the actual quiescent public-wire boundary. The older helper
// intentionally selects lane0; this witness's independent target is lane1.
func inflightHoleSender(t *testing.T, fixture *windowRoundFixture, frame *windowRoundFrame) *SendSequence {
	t.Helper()
	synctest.Wait()
	fixture.sender.sendBuffer.mutex.Lock()
	defer fixture.sender.sendBuffer.mutex.Unlock()
	sequence := fixture.sender.sendBuffer.sendSequencesBySequenceId[RequireIdFromBytes(frame.pack.SequenceId)]
	if sequence == nil || sequence.logicalLane != frame.pack.LogicalLane || sequence.destination != fixture.receiver.ClientId() {
		t.Fatal("actual inflight target sender identity or logical lane differs")
	}
	return sequence
}
