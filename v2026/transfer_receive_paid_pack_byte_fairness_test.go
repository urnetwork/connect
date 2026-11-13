// Candidate-only isolation guard: real hot H1 originals fill ordinary RX,
// while an already verified sibling retains its independent current-head path.
package connect

import (
	"bytes"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
	"google.golang.org/protobuf/proto"
)

func TestReceivePaidH1RxSaturationPreservesSiblingCurrentHead(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		layout := consumerPaidPackByteLayout{label: "32-rx-fairness", byteLimit: 174762, leafBytes: 2181038, rootBytes: 21810380, countLimit: 85}
		root := NewTransferMemoryBudget(layout.rootBytes)
		rx := NewTransferMemoryBudgetWithParent(layout.leafBytes, root)
		head := NewTransferMemoryBudgetWithParent(layout.leafBytes, root)
		pack := NewTransferMemoryBudgetWithParent(layout.leafBytes, root)
		t.Cleanup(func() {
			for _, budget := range []*TransferMemoryBudget{pack, head, rx, root} {
				assertRetainedBudgetBalance(t, budget)
				if budget.Stats().QuotaByteCount != 0 {
					t.Error("saturated hot peer retained shared quota after joining")
				}
			}
		})
		var refusedId, healthyId atomic.Pointer[Id]
		var refusedCumulative, refusedSelective, healthyCumulative atomic.Uint64
		progress := func(event TransferProgressEvent) {
			if event.Stage != "receive_ack_begin" {
				return
			}
			if id := refusedId.Load(); id != nil && event.MessageId == *id {
				if event.Selective {
					refusedSelective.Add(1)
				} else {
					refusedCumulative.Add(1)
				}
			}
			if id := healthyId.Load(); id != nil && event.MessageId == *id && !event.Selective {
				healthyCumulative.Add(1)
			}
		}
		fixture, log, _, _ := newConsumerPaidPackByteFixture(t, layout, rx, head, pack, progress)
		entered, release := make(chan struct{}), make(chan struct{})
		var enteredOnce, releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		t.Cleanup(unblock)
		var arm atomic.Bool
		var hotCompleted, siblingCompleted atomic.Int64
		var healthy *ReceiveSequence
		var healthyCost atomic.Int64
		var independentlyPaid atomic.Bool
		fixture.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, peer Peer) {
			if !arm.Load() {
				return
			}
			if peer.TransferKey.LogicalLane == 0 {
				enteredOnce.Do(func() {
					close(entered)
					select {
					case <-release:
					case <-fixture.ctx.Done():
					}
				})
				hotCompleted.Add(int64(len(frames)))
				return
			}
			if peer.TransferKey.LogicalLane != 1 {
				return
			}
			id := healthyId.Load()
			if id == nil || healthy == nil {
				t.Error("healthy callback lacks its actual established target")
				return
			}
			root.admissionLock.Lock()
			found := 0
			for current, visited := healthy.holdPromise.ingressHead, 0; current != nil && visited < 256; current, visited = current.ingressNext, visited+1 {
				owner := current.decodedOwner
				if owner != nil && owner.receiveItem.messageId == *id && owner.receiveItem.memoryBudget == head &&
					owner.receiveItem.QueueByteCount() == ByteCount(healthyCost.Load()) && current == &owner.receivePack &&
					current.Pack == &owner.pack && current.receiveMemoryOwned && current.ingressSequence == healthy && current.ingressLoanByteCount == 0 {
					found++
				}
			}
			paid := found == 1 && head.usedByteCount.Load() == ByteCount(healthyCost.Load()) &&
				rx.totalByteCount.Load()-rx.usedByteCount.Load() < ByteCount(healthyCost.Load()) &&
				root.usedByteCount.Load() == rx.usedByteCount.Load()+head.usedByteCount.Load() && pack.usedByteCount.Load() == 0
			root.admissionLock.Unlock()
			independentlyPaid.Store(paid)
			siblingCompleted.Add(int64(len(frames)))
		})
		var receipts [256]atomic.Bool
		var completions [256]atomic.Int64
		started := time.Now()
		nextSlot := 0
		send := func(lane uint32, count int) consumerPaidPackByteWire {
			if nextSlot == len(receipts) {
				t.Fatal("fixed hot-source send allowance exhausted")
			}
			slot := nextSlot
			nextSlot++
			return consumerPaidPackByteSend(t, fixture, lane, count, slot, &receipts[slot], &completions[slot], started)
		}
		warmHot := send(0, 1)
		fixture.forward(warmHot.frame, fixture.receiverIn)
		fixture.acknowledge()
		warmHealthy := send(1, 1)
		fixture.forward(warmHealthy.frame, fixture.receiverIn)
		fixture.acknowledge()
		hotSequence := consumerForeignHeadSequence(t, fixture, warmHot.frame)
		healthy = consumerForeignHeadSequence(t, fixture, warmHealthy.frame)
		hotSender := consumerEstablishedWireHeadSender(t, fixture, warmHot.frame)
		if !warmHot.acked.Load() || !warmHealthy.acked.Load() || healthy.receiveContract == nil ||
			!healthy.receiveContract.verifiedReceiveProofSet || !healthy.holdPromise.establishedContract ||
			healthy.holdPromise.ingressHead != nil || healthy.holdPromise.set || healthy.receiveQueue.Len() != 0 || root.UsedByteCount() != 0 {
			t.Fatal("actual healthy and hot generations were not fully verified, ACKed and quiet before pressure")
		}
		arm.Store(true)
		seed := send(0, 8)
		fixture.forward(seed.frame, fixture.receiverIn)
		<-entered
		var hot []consumerPaidPackByteWire
		hot = append(hot, seed)
		var refused consumerPaidPackByteWire
		for range layout.countLimit {
			next := send(0, 8)
			if rx.Available() < next.scan.retainedByteCount {
				refused = next
				break
			}
			beforeCount, beforeEncoded, beforeRx := hotSequence.packQueueCount.Load(), hotSequence.packQueueByteCount.Load(), rx.UsedByteCount()
			encoded := ByteCount(len(next.frame.bytes))
			fixture.forward(next.frame, fixture.receiverIn)
			if next.acked.Load() || hotSequence.packQueueCount.Load() != beforeCount+1 || hotSequence.packQueueByteCount.Load() != beforeEncoded+encoded || rx.UsedByteCount() != beforeRx+next.scan.retainedByteCount {
				t.Fatal("candidate could not fill ordinary RX with actual queued hot originals beyond the old logical-byte bound")
			}
			hot = append(hot, next)
		}
		if refused.frame == nil || hotSequence.packQueueByteCount.Load() <= layout.byteLimit ||
			hotSequence.packQueueCount.Load()+1 >= int64(layout.countLimit) || len(hotSequence.packs) >= cap(hotSequence.packs) ||
			rx.Available() >= refused.scan.retainedByteCount || root.Available() < refused.scan.retainedByteCount ||
			pack.receiveQuotaAvailable(root) < ByteCount(len(refused.frame.bytes)) || head.UsedByteCount() != 0 {
			t.Fatal("hot pressure did not isolate the real physical RX leaf with count/channel/shared-quota/root room")
		}
		beforeRx, beforeRoot, beforeQuota := rx.UsedByteCount(), root.UsedByteCount(), pack.Stats().QuotaByteCount
		beforeDrops := fixture.receiver.ReceiveStats().PackHandoffDropCount
		id := refused.scan.messageId
		refusedId.Store(&id)
		fixture.forward(refused.frame, fixture.receiverIn)
		if refused.acked.Load() || refusedCumulative.Load() != 0 || refusedSelective.Load() != 0 ||
			rx.UsedByteCount() != beforeRx || root.UsedByteCount() != beforeRoot || pack.Stats().QuotaByteCount != beforeQuota ||
			head.UsedByteCount() != 0 || fixture.receiver.ReceiveStats().PackHandoffDropCount-beforeDrops != 1 {
			t.Fatal("actual RX refusal stole another partition, ACKed the hot gap or changed existing custody")
		}
		current := send(1, 8)
		if !current.frame.pack.Head || current.frame.pack.ContractId == nil || current.frame.pack.ContractFrame != nil || current.scan.sequenceNumber != healthy.holdPromise.ingressFloor ||
			current.scan.sequenceNumber != healthy.nextSequenceNumber || rx.Available() >= current.scan.retainedByteCount || head.Available() < current.scan.retainedByteCount ||
			root.Available() < current.scan.retainedByteCount || pack.receiveQuotaAvailable(root) < ByteCount(len(current.frame.bytes)) {
			t.Fatal("actual already-established sibling did not emit an independently payable current compact Head")
		}
		currentId := current.scan.messageId
		healthyId.Store(&currentId)
		healthyCost.Store(int64(current.scan.retainedByteCount))
		beforeReserved, beforeReleased := head.Counts()
		fixture.forward(current.frame, fixture.receiverIn)
		fixture.acknowledge()
		afterReserved, afterReleased := head.Counts()
		if !current.acked.Load() || healthyCumulative.Load() == 0 || !independentlyPaid.Load() || siblingCompleted.Load() != 8 || hotCompleted.Load() != 0 ||
			afterReserved-beforeReserved != current.scan.retainedByteCount || afterReleased-beforeReleased != current.scan.retainedByteCount ||
			head.UsedByteCount() != 0 || rx.UsedByteCount() != beforeRx || root.UsedByteCount() != beforeRoot || pack.Stats().QuotaByteCount != beforeQuota ||
			refused.acked.Load() || refusedCumulative.Load() != 0 || refusedSelective.Load() != 0 {
			t.Fatal("healthy current Head did not progress with its own exact child charge/refund while hot RX remained saturated")
		}
		for _, wire := range hot {
			if wire.acked.Load() {
				t.Fatal("held hot source received an acceptance before callback release")
			}
		}
		releasedAt := time.Now()
		unblock()
		synctest.Wait()
		fixture.acknowledge()
		if refused.acked.Load() || refusedCumulative.Load() != 0 || refusedSelective.Load() != 0 {
			t.Fatal("a refused hot original received a false ACK/SACK with the accepted prefix")
		}
		retry := pressureContractNaturalRetry(t, fixture, refused.frame.pack)
		if !bytes.Equal(retry.pack.MessageId, refused.frame.pack.MessageId) || !bytes.Equal(retry.pack.SequenceId, refused.frame.pack.SequenceId) || retry.pack.SequenceNumber != refused.frame.pack.SequenceNumber || len(retry.pack.Frames) != len(refused.frame.pack.Frames) {
			t.Fatal("natural hot retry changed its retained identity")
		}
		for index := range retry.pack.Frames {
			if !proto.Equal(retry.pack.Frames[index], refused.frame.pack.Frames[index]) {
				t.Fatal("natural hot retry changed a payload")
			}
		}
		fixture.forward(retry, fixture.receiverIn)
		fixture.acknowledge()
		hot = append(hot, refused)
		for _, wire := range hot {
			if !wire.acked.Load() {
				t.Fatal("hot original failed actual retry/completion after sibling progress")
			}
		}
		if refusedCumulative.Load() == 0 || refusedSelective.Load() != 0 || hotCompleted.Load() != int64(8*len(hot)) || siblingCompleted.Load() != 8 ||
			len(hotSender.sendItems) != 0 || hotSequence.packQueueCount.Load() != 0 || hotSequence.packQueueByteCount.Load() != 0 ||
			healthy.holdPromise.ingressHead != nil || hotSequence.holdPromise.ingressHead != nil || root.UsedByteCount() != 0 || rx.UsedByteCount() != 0 ||
			head.UsedByteCount() != 0 || pack.Stats().QuotaByteCount != 0 || len(log.messages()) != 0 {
			t.Fatal("saturated-peer recovery did not finish exact payload/ACK/root/quota ownership")
		}
		t.Logf("PAID_PACK_RX_FAIRNESS_BEHAVIOR_DRAINED hot_groups=%d rx_peak=%d sibling_C=%d natural_recovery_ns=%d", len(hot), beforeRx, current.scan.retainedByteCount, time.Since(releasedAt).Nanoseconds())
	})
	if !t.Failed() {
		t.Log("PAID_PACK_RX_FAIRNESS_CLEANUP_JOINED")
	}
}
