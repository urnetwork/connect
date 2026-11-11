// Two signed public sequences learn independent encoded ceilings backed by one
// physical RX owner. The witness preserves sender gates, ACK custody and retry.
package connect

import (
	"bytes"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A numerically equal encoded advertisement is not a shared retained-memory
// grant. Two real parked owners can exhaust RX below both learned windows.
func TestReceiveSharedEncodedAdvertisementsCanExhaustPhysicalRx(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		const laneCount = 2
		const frameCount = 8
		const retainedCost = ByteCount(30544)
		const rxLimit = laneCount * retainedCost
		const rootLimit = ByteCount(1024 * 1024)
		root := NewTransferMemoryBudget(rootLimit)
		client := NewTransferMemoryBudgetWithParent(rootLimit, root)
		rx := NewTransferMemoryBudgetWithParent(rxLimit, client)
		head := NewTransferMemoryBudgetWithParent(rxLimit, client)
		pack := NewTransferMemoryBudgetWithParent(rxLimit, client)
		tx := NewTransferMemoryBudget(rootLimit)
		t.Cleanup(func() {
			for _, budget := range []*TransferMemoryBudget{tx, pack, head, rx, client, root} {
				assertRetainedBudgetBalance(t, budget)
				if budget.Stats().QuotaByteCount != 0 {
					t.Error("shared advertisement witness retained quota after both public Clients joined")
				}
			}
		})
		type laneBarrier struct {
			firstId Id
			armed   atomic.Bool
			entered chan struct{}
			release chan struct{}
			once    sync.Once
		}
		var barriers [laneCount]laneBarrier
		for index := range barriers {
			barriers[index].entered = make(chan struct{})
			barriers[index].release = make(chan struct{})
		}
		var retryTarget atomic.Pointer[Id]
		var actualTargetRetries atomic.Uint64
		layout := consumerPaidPackByteLayout{label: "two-public-shared-advertisements", leafBytes: rxLimit,
			rootBytes: rootLimit, countLimit: 64, byteLimit: 128 * 1024}
		fixture, logger, _, _ := newConsumerPaidPackByteFixture(t, layout, rx, head, pack, nil,
			func(receiver bool, settings *ClientSettings) {
				if !receiver {
					send := settings.SendBufferSettings
					send.ResendQueueBudget = tx
					send.ResendQueueRetainedByteAccounting = true
					send.WindowSizing = WindowSizingFromDelivery
					send.ApplyWindowSizing()
					send.LiveDiagnostics = true
					send.TransferWireMessageObserver = func(event TransferWireMessageObservation) {
						if target := retryTarget.Load(); target != nil && event.MessageId == *target && event.Resend {
							actualTargetRetries.Add(1)
						}
					}
					return
				}
				receive := settings.ReceiveBufferSettings
				receive.AdvertiseReceiveWindow = true
				if receive.AdvertisedReceiveWindowMaxByteCount != 0 {
					t.Fatal("default advertisement witness inherited an optional cap")
				}
				receive.H1PackHandoffTimeout, receive.ReliablePackHandoffTimeout = 0, 0
				receive.receiveOwnerStageForTest = func(sequence *ReceiveSequence, owner *decodedPackOwner, stage string) {
					if stage != "pack_released" || sequence == nil || owner == nil {
						return
					}
					lane := sequence.transferKey.LogicalLane
					if lane == 0 || lane > laneCount {
						return
					}
					barrier := &barriers[lane-1]
					if barrier.armed.Load() && owner.receiveItem.messageId == barrier.firstId && barrier.armed.CompareAndSwap(true, false) {
						close(barrier.entered)
						<-barrier.release
					}
				}
			})
		release := func(index int) {
			barriers[index].once.Do(func() { close(barriers[index].release) })
			synctest.Wait()
		}
		t.Cleanup(func() {
			fixture.cancel()
			for index := range barriers {
				release(index)
			}
		})
		started := time.Now()
		var receipts [7]atomic.Bool
		var completionTimes [7]atomic.Int64
		var originals []consumerPaidPackByteWire
		send := func(index, frames int) consumerPaidPackByteWire {
			t.Helper()
			slot := len(originals)
			if slot == len(receipts) {
				t.Fatal("bounded public source exceeded its seven originals")
			}
			wire := consumerPaidPackByteSend(t, fixture, uint32(index+1), frames, slot,
				&receipts[slot], &completionTimes[slot], started)
			originals = append(originals, wire)
			return wire
		}
		var current, tail [laneCount]consumerPaidPackByteWire
		var receivers [laneCount]*ReceiveSequence
		var senders [laneCount]*SendSequence
		var currentEncoded, tailEncoded [laneCount]ByteCount
		var advertisements [laneCount]uint64
		for index := range laneCount {
			warm := send(index, 1)
			fixture.forward(warm.frame, fixture.receiverIn)
			ackWire := fixture.take(fixture.receiverOut)
			ack := ackWire.ack
			if ack == nil || ack.Selective || ack.ReceiveWindowByteCount == nil ||
				!bytes.Equal(ack.MessageId, warm.frame.pack.MessageId) || ack.GetReceiveWindowByteCount() != uint64(rxLimit) {
				t.Fatal("warm signed delivery did not publish its actual per-sequence encoded ceiling")
			}
			advertisements[index] = ack.GetReceiveWindowByteCount()
			fixture.forward(ackWire, fixture.senderIn)
			if !warm.acked.Load() || root.UsedByteCount() != ByteCount(index)*retainedCost {
				t.Fatal("warm signed original lacked its real ACK/refund or changed the other lane's retained owner")
			}
			current[index] = send(index, frameCount)
			currentEncoded[index] = ByteCount(len(current[index].frame.bytes))
			tail[index] = send(index, frameCount)
			tailEncoded[index] = ByteCount(len(tail[index].frame.bytes))
			receivers[index] = consumerForeignHeadSequence(t, fixture, current[index].frame)
			buffer := fixture.sender.sendBuffer
			buffer.mutex.Lock()
			senders[index] = buffer.sendSequencesBySequenceId[current[index].scan.sequenceId]
			buffer.mutex.Unlock()
			sequence := receivers[index]
			if senders[index] == nil || sequence.receiveQueue.Budget() != rx || sequence.receiveContract == nil ||
				!sequence.receiveContract.verifiedReceiveProofSet || !sequence.holdPromise.establishedContract ||
				sequence.holdPromise.ingressFloor != current[index].scan.sequenceNumber ||
				tail[index].scan.sequenceId != current[index].scan.sequenceId ||
				tail[index].scan.sequenceNumber != current[index].scan.sequenceNumber+1 ||
				tail[index].frame.pack.Head || tail[index].frame.pack.ContractFrame != nil || tail[index].frame.pack.ContractId != nil ||
				current[index].scan.retainedByteCount != retainedCost || tail[index].scan.retainedByteCount != retainedCost ||
				current[index].scan.protocolFrameCount != 0 || tail[index].scan.protocolFrameCount != 0 ||
				fixture.receiver.ContractManager().ReceiveNoContract(fixture.sender.ClientId()) {
				t.Fatal("two-sequence setup lost signed authority, exact current identity or actual DATA cost")
			}
			barriers[index].firstId = tail[index].scan.messageId
			barriers[index].armed.Store(true)
			fixture.forward(tail[index].frame, fixture.receiverIn)
			select {
			case <-barriers[index].entered:
			default:
				t.Fatal("actual decoded future owner did not reach its worker custody barrier")
			}
		}
		if receivers[0] == receivers[1] || senders[0] == senders[1] ||
			rx.UsedByteCount() != rxLimit || head.UsedByteCount() != 0 || root.UsedByteCount() != rxLimit ||
			pack.Stats().QuotaByteCount != 0 || len(fixture.receiverOut) != 0 || fixture.deliveredCount != laneCount {
			t.Fatal("two actual owners did not isolate full shared physical RX below the unchanged root and Head caps")
		}
		// The sender publishes its own immutable item census. Queue scalar
		// copies use the queue lock after the window snapshot, never escaped items.
		readSender := func(index int, expectedCount int, expectedEncoded ByteCount) (SendSequenceLiveSnapshot, ByteCount, ByteCount) {
			t.Helper()
			sequence := senders[index]
			sequence.readSendLiveSnapshot()
			synctest.Wait()
			published := sequence.liveDiagnostics.latest.Load()
			window := sequence.sendWindowSnapshot(time.Now())
			sequence.resendQueue.stateLock.Lock()
			debt := sequence.resendQueue.windowByteCountWithLock()
			charge := sequence.resendQueue.queueByteCount
			sequence.resendQueue.stateLock.Unlock()
			if published == nil || !published.Sampled || published.Closed || !published.PeerWindowAdvertised ||
				published.PeerWindowByteCount != ByteCount(advertisements[index]) ||
				published.SequenceId != current[index].scan.sequenceId || published.OriginalItemCount != expectedCount ||
				published.UnacknowledgedItemCount != expectedCount || published.OriginalEncodedByteCount != expectedEncoded ||
				debt != expectedEncoded || charge <= debt || !published.RetainedAccountingEnabled ||
				sequence.sendBufferSettings.WindowSizing != WindowSizingFromDelivery ||
				sequence.sendBufferSettings.DeliverySizedWindowScale <= 0 || window.Window > ByteCount(advertisements[index]) ||
				debt >= window.Window || sequence.ctx.Err() != nil {
				t.Fatalf("real sender permission/custody did not preserve the advertised encoded debt: lane=%d debt=%d window=%d charge=%d published=%+v",
					index+1, debt, window.Window, charge, published)
			}
			return *published, debt, window.Window
		}
		var debtBefore, windowBefore [laneCount]ByteCount
		for index := range laneCount {
			_, debtBefore[index], windowBefore[index] = readSender(index, 2, currentEncoded[index]+tailEncoded[index])
			if debtBefore[index]+sendPackH1WireByteCount >= windowBefore[index] {
				t.Fatalf("actual sender gate prevents the pressure shape: lane=%d debt=%d H1max=%d window=%d",
					index+1, debtBefore[index], sendPackH1WireByteCount, windowBefore[index])
			}
		}
		if tx.Available() < 4*retainedCost || root.Available() < 2*retainedCost || head.Available() < 2*retainedCost {
			t.Fatal("another real admission bound prevents the intended RX-only pressure cut")
		}
		refused := send(0, frameCount)
		encoded := ByteCount(len(refused.frame.bytes))
		if refused.scan.retainedByteCount != retainedCost || refused.frame.pack.Head || refused.frame.pack.Nack ||
			refused.scan.sequenceId != current[0].scan.sequenceId || refused.scan.sequenceNumber != tail[0].scan.sequenceNumber+1 ||
			debtBefore[0]+encoded >= windowBefore[0] {
			t.Fatal("actual third original was not wholly within its sender's learned encoded window")
		}
		id := refused.scan.messageId
		retryTarget.Store(&id)
		before := fixture.receiver.ReceiveStats()
		copy := &windowRoundFrame{bytes: MessagePoolCopy(refused.frame.bytes), pack: refused.frame.pack}
		fixture.forward(copy, fixture.receiverIn)
		after := fixture.receiver.ReceiveStats()
		if after.PackHandoffDropCount != before.PackHandoffDropCount+1 ||
			after.ReceiveQueueDropCount != before.ReceiveQueueDropCount || refused.acked.Load() ||
			len(fixture.receiverOut) != 0 || rx.UsedByteCount() != rxLimit || head.UsedByteCount() != 0 ||
			root.UsedByteCount() != rxLimit || fixture.deliveredCount != laneCount {
			t.Fatalf("complete DATA within its encoded window did not isolate one physical RX refusal: handoff=%d queue=%d rx=%d head=%d root=%d ack=%t",
				after.PackHandoffDropCount-before.PackHandoffDropCount, after.ReceiveQueueDropCount-before.ReceiveQueueDropCount,
				rx.UsedByteCount(), head.UsedByteCount(), root.UsedByteCount(), refused.acked.Load())
		}
		first, firstDebt, firstWindow := readSender(0, 3, debtBefore[0]+encoded)
		second, secondDebt, secondWindow := readSender(1, 2, debtBefore[1])
		root.admissionLock.Lock()
		var retainedTails int
		var invalidOwner bool
		for index, sequence := range receivers {
			if sequence.holdPromise.set || sequence.holdPromise.reservedByteCount != 0 || sequence.holdPromise.closed {
				invalidOwner = true
			}
			for owned := sequence.holdPromise.ingressHead; owned != nil; owned = owned.ingressNext {
				owner := owned.decodedOwner
				if owner == nil || !owned.receiveMemoryOwned || owner.receiveItem.memoryBudget != rx ||
					owner.receiveItem.QueueByteCount() != retainedCost || owner.receiveItem.messageId != tail[index].scan.messageId {
					invalidOwner = true
				} else {
					retainedTails++
				}
			}
		}
		root.admissionLock.Unlock()
		if invalidOwner || retainedTails != laneCount || first.SelectiveAckCount != 0 || second.SelectiveAckCount != 0 {
			t.Fatal("predecode refusal lost a real retained owner or published a false selective receipt")
		}
		t.Logf("PUBLIC_SHARED_ENCODED_ADVERTISEMENTS sequences=2 windows=%d,%d effective=%d,%d encoded_debt=%d,%d refused_wire=%d refused_retained=%d rx_used=%d rx_cap=%d head_used=0 head_cap=%d root_used=%d root_cap=%d predecode_refusals=1 false_receipts=0",
			advertisements[0], advertisements[1], firstWindow, secondWindow, firstDebt, secondDebt, encoded, retainedCost,
			rx.UsedByteCount(), rxLimit, head.TotalByteCount(), root.UsedByteCount(), rootLimit)
		// Both exact currents acquire genuine Head ownership before either
		// parked future returns. This tests recovery without releasing a fake filler.
		for index := range laneCount {
			fixture.forward(current[index].frame, fixture.receiverIn)
		}
		if head.UsedByteCount() != laneCount*retainedCost || rx.UsedByteCount() != rxLimit ||
			fixture.receiver.ReceiveStats().PackHandoffDropCount != after.PackHandoffDropCount {
			t.Fatal("exact current recovery did not preserve both RX owners while paying separate Head custody")
		}
		for index := range laneCount {
			release(index)
			fixture.acknowledge()
			if !current[index].acked.Load() || !tail[index].acked.Load() || receivers[index].ctx.Err() != nil {
				t.Fatal("real current and retained tail did not drain with exact ACKs while sibling lifetime stayed healthy")
			}
		}
		if root.UsedByteCount() != 0 || head.UsedByteCount() != 0 || rx.UsedByteCount() != 0 || refused.acked.Load() {
			t.Fatal("current/tail return did not refund independently of the still-refused source original")
		}
		if len(fixture.senderOut) != 0 {
			t.Fatal("an earlier physical write remained queued before the sole outstanding original's natural retry")
		}
		// Await the sender's unchanged retry timer, not a manually re-offered
		// original. The physical source still owns the refused identity.
		wireBytes := <-fixture.senderOut
		fixture.senderOut <- wireBytes
		retry := fixture.take(fixture.senderOut)
		if actualTargetRetries.Load() != 1 || retry.pack == nil ||
			!bytes.Equal(retry.pack.MessageId, refused.frame.pack.MessageId) ||
			!bytes.Equal(retry.pack.SequenceId, refused.frame.pack.SequenceId) ||
			retry.pack.SequenceNumber != refused.frame.pack.SequenceNumber || len(retry.pack.Frames) != frameCount {
			t.Fatal("ordinary retry did not preserve the exact physically refused original")
		}
		for index, frame := range retry.pack.Frames {
			if !bytes.Equal(frame.MessageBytes, refused.frame.pack.Frames[index].MessageBytes) {
				t.Fatal("recovery changed refused DATA payload ownership")
			}
		}
		fixture.forward(retry, fixture.receiverIn)
		fixture.acknowledge()
		for _, wire := range originals {
			if !wire.acked.Load() {
				t.Fatal("an actual admitted original did not complete by real cumulative ACK")
			}
		}
		if fixture.deliveredCount != laneCount+5*frameCount || root.UsedByteCount() != 0 || tx.UsedByteCount() != 0 ||
			pack.Stats().QuotaByteCount != 0 || fixture.sender.ctx.Err() != nil || fixture.receiver.ctx.Err() != nil || len(logger.messages()) != 0 {
			t.Fatal("shared advertisement recovery lost a parent, payload, or final physical/quota refund")
		}
		t.Logf("PUBLIC_SHARED_ENCODED_RECOVERY retry_identity_preserved=true actual_retries=%d elapsed=%s callbacks=%d delivered_frames=%d final_rx=0 final_head=0 final_root=0 final_tx=0",
			actualTargetRetries.Load(), time.Since(started), len(originals), fixture.deliveredCount)
	})
}

// The optional ceiling really reaches both senders through ACK. Admission is
// checked before serialization, so one legal original may cross that ceiling;
// the next original must wait. This is not a shared retained-credit guarantee.
func TestReceiveSharedSmallAdvertisementsGateActualPublicOriginals(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		const frameCount = 8
		const advertised = ByteCount(16*1024 + 1)
		const rxLimit = ByteCount(2 * 30544)
		const rootLimit = ByteCount(1024 * 1024)
		root := NewTransferMemoryBudget(rootLimit)
		rx := NewTransferMemoryBudgetWithParent(rxLimit, root)
		head := NewTransferMemoryBudgetWithParent(rxLimit, root)
		pack := NewTransferMemoryBudgetWithParent(rxLimit, root)
		tx := NewTransferMemoryBudget(rootLimit)
		t.Cleanup(func() {
			for _, budget := range []*TransferMemoryBudget{tx, pack, head, rx, root} {
				assertRetainedBudgetBalance(t, budget)
				if budget.Stats().QuotaByteCount != 0 {
					t.Error("small advertisement left quota after actual Client join")
				}
			}
		})
		layout := consumerPaidPackByteLayout{label: "two-public-small-advertisements", leafBytes: rxLimit,
			rootBytes: rootLimit, countLimit: 64, byteLimit: 128 * 1024}
		fixture, logger, _, _ := newConsumerPaidPackByteFixture(t, layout, rx, head, pack, nil,
			func(receiver bool, settings *ClientSettings) {
				if receiver {
					settings.ReceiveBufferSettings.AdvertiseReceiveWindow = true
					settings.ReceiveBufferSettings.AdvertisedReceiveWindowMaxByteCount = advertised
					return
				}
				send := settings.SendBufferSettings
				send.ResendQueueBudget = tx
				send.ResendQueueRetainedByteAccounting = true
				send.WindowSizing = WindowSizingFromDelivery
				send.ApplyWindowSizing()
				send.LiveDiagnostics = true
			})
		started := time.Now()
		var receipts [6]atomic.Bool
		var completionTimes [6]atomic.Int64
		var current, tail [2]consumerPaidPackByteWire
		var senders [2]*SendSequence
		var encodedDebt [2]ByteCount
		for index := range 2 {
			warm := consumerPaidPackByteSend(t, fixture, uint32(index+1), 1, 3*index,
				&receipts[3*index], &completionTimes[3*index], started)
			fixture.forward(warm.frame, fixture.receiverIn)
			wire := fixture.take(fixture.receiverOut)
			if wire.ack == nil || wire.ack.Selective || wire.ack.ReceiveWindowByteCount == nil ||
				wire.ack.GetReceiveWindowByteCount() != uint64(advertised) ||
				!bytes.Equal(wire.ack.MessageId, warm.frame.pack.MessageId) {
				t.Fatal("small ceiling was not advertised by the actual signed receiver")
			}
			fixture.forward(wire, fixture.senderIn)
			current[index] = consumerPaidPackByteSend(t, fixture, uint32(index+1), frameCount, 3*index+1,
				&receipts[3*index+1], &completionTimes[3*index+1], started)
			tail[index] = consumerPaidPackByteSend(t, fixture, uint32(index+1), frameCount, 3*index+2,
				&receipts[3*index+2], &completionTimes[3*index+2], started)
			buffer := fixture.sender.sendBuffer
			buffer.mutex.Lock()
			senders[index] = buffer.sendSequencesBySequenceId[current[index].scan.sequenceId]
			buffer.mutex.Unlock()
			if senders[index] == nil {
				t.Fatal("small-ceiling source identity disappeared")
			}
			encodedDebt[index] = ByteCount(len(current[index].frame.bytes) + len(tail[index].frame.bytes))
			sequence := senders[index]
			sequence.readSendLiveSnapshot()
			synctest.Wait()
			published := sequence.liveDiagnostics.latest.Load()
			window := sequence.sendWindowSnapshot(time.Now())
			if published == nil || !published.Sampled || published.Closed || !published.PeerWindowAdvertised ||
				published.PeerWindowByteCount != advertised || published.OriginalItemCount != 2 ||
				published.UnacknowledgedItemCount != 2 || published.OriginalEncodedByteCount != encodedDebt[index] ||
				window.Window > advertised || encodedDebt[index] < advertised ||
				encodedDebt[index] >= advertised+sendPackH1WireByteCount ||
				!sequence.resendCapacityUnavailable.Load() || tx.Available() < 4*30544 || root.UsedByteCount() != 0 {
				t.Fatalf("small learned ceiling did not close the real encoded gate after its one-original crossing: lane=%d debt=%d window=%d snapshot=%+v", index+1, encodedDebt[index], window.Window, published)
			}
		}
		frames := make([]*protocol.Frame, frameCount)
		for index := range frames {
			packet := ipOosTcpPacketSequence(reliableDeliveryTestPath(4), tcpFlagAck,
				uint32(3000+index), bytes.Repeat([]byte{0x67}, 1240))
			frames[index] = &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider,
				Raw: true, MessageBytes: MessagePoolCopy(packet)}
		}
		var lastAcked atomic.Bool
		var lastTerminals atomic.Int64
		offerResult := make(chan bool, 1)
		go func() {
			offerResult <- fixture.sender.SendMultiWithTimeout(frames, fixture.receiver.ClientId(), func(err error) {
				lastTerminals.Add(1)
				lastAcked.Store(err == nil)
			}, time.Second, TransferKey{LogicalLane: 1})
		}()
		var offerJoined bool
		var offerAccepted bool
		t.Cleanup(func() {
			fixture.cancel()
			if !offerJoined {
				offerAccepted = <-offerResult
			}
			if !offerAccepted {
				for _, frame := range frames {
					MessagePoolReturn(frame.MessageBytes)
				}
			}
		})
		synctest.Wait()
		select {
		case offerAccepted = <-offerResult:
			offerJoined = true
			if !offerAccepted {
				t.Fatal("bounded public offer expired before the real ACK could release its gate")
			}
		default:
		}
		if len(fixture.senderOut) != 0 || lastTerminals.Load() != 0 || root.UsedByteCount() != 0 ||
			fixture.receiver.ReceiveStats().PackHandoffDropCount != 0 {
			t.Fatal("third original crossed a closed encoded gate or acquired receive custody")
		}
		// The sibling's actual current/tail can complete without changing the
		// blocked source's advertised window or releasing its missing current.
		fixture.forward(current[1].frame, fixture.receiverIn)
		fixture.forward(tail[1].frame, fixture.receiverIn)
		fixture.acknowledge()
		if !current[1].acked.Load() || !tail[1].acked.Load() || current[0].acked.Load() ||
			len(fixture.senderOut) != 0 || lastTerminals.Load() != 0 {
			t.Fatal("sibling progress changed the other source's real prefix gate")
		}
		fixture.forward(current[0].frame, fixture.receiverIn)
		fixture.acknowledge()
		if !offerJoined {
			offerAccepted = <-offerResult
			offerJoined = true
		}
		if !offerAccepted {
			t.Fatal("real prefix ACK did not reopen the original public offer")
		}
		last := fixture.take(fixture.senderOut)
		if last.pack == nil || last.pack.Nack || last.pack.LogicalLane != 1 ||
			!bytes.Equal(last.pack.SequenceId, current[0].frame.pack.SequenceId) ||
			last.pack.SequenceNumber != tail[0].scan.sequenceNumber+1 || len(last.pack.Frames) != frameCount {
			t.Fatal("public gate release did not serialize the exact next reliable original")
		}
		fixture.forward(tail[0].frame, fixture.receiverIn)
		fixture.forward(last, fixture.receiverIn)
		fixture.acknowledge()
		for index := range receipts {
			if !receipts[index].Load() {
				t.Fatal("small-ceiling source lost its actual cumulative callback")
			}
		}
		if !lastAcked.Load() || lastTerminals.Load() != 1 || fixture.deliveredCount != 2+5*frameCount ||
			root.UsedByteCount() != 0 || tx.UsedByteCount() != 0 || fixture.sender.ctx.Err() != nil ||
			fixture.receiver.ctx.Err() != nil || len(logger.messages()) != 0 ||
			fixture.receiver.ReceiveStats().PackHandoffDropCount != 0 || time.Since(started) != 0 {
			t.Fatal("real small-window completion lost exact custody, zero-time ACK progress or refunds")
		}
		t.Logf("PUBLIC_SMALL_ENCODED_ADVERTISEMENTS cap=%d legal_H1_max=%d debt_before=%d,%d next_wire_held=true sibling_progress=true actual_prefix_ACK_reopened=true no_physical_refusals=true final_root=0 final_tx=0", advertised, sendPackH1WireByteCount, encodedDebt[0], encodedDebt[1])
	})
}
