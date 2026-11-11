// A delivery-fairness bound must preserve the existing combined DATA burst.
// These component controls use signed public H1 traffic and SDK32 queue sizes.
package connect

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Eight actual eight-packet originals form the unchanged64-frame callback.
// The first head is paused while the other seven acquire real input custody.
func TestReceiveBufferedDataKeepsContiguous64FrameBatch(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		assertMessagePoolOwnership(t)
		layout := consumerPaidPackByteLayout{label: "32-batch-control", byteLimit: 174762,
			leafBytes: 2181038, rootBytes: 21810380, countLimit: 85}
		root := NewTransferMemoryBudget(layout.rootBytes)
		rx := NewTransferMemoryBudgetWithParent(layout.leafBytes, root)
		head := NewTransferMemoryBudgetWithParent(layout.leafBytes, root)
		pack := NewTransferMemoryBudgetWithParent(layout.leafBytes, root)
		entered, resume := make(chan struct{}), make(chan struct{})
		var resumeOnce sync.Once
		release := func() { resumeOnce.Do(func() { close(resume) }) }
		var targetId atomic.Pointer[Id]
		var sequence atomic.Pointer[ReceiveSequence]
		var armed atomic.Bool
		var batchCount atomic.Int64
		var callbackViolation atomic.Bool
		var ackBeforeCallback atomic.Bool
		var warmNumber atomic.Uint64
		fixture, log, _, _ := newConsumerPaidPackByteFixture(t, layout, rx, head, pack,
			func(event TransferProgressEvent) {
				if armed.Load() && event.Stage == "receive_ack_begin" && event.SequenceNumber > warmNumber.Load() && batchCount.Load() == 0 {
					ackBeforeCallback.Store(true)
				}
			}, func(receiver bool, settings *ClientSettings) {
				if !receiver {
					return
				}
				settings.ReceiveBufferSettings.receiveOwnerStageForTest = func(actual *ReceiveSequence, owner *decodedPackOwner, stage string) {
					id := targetId.Load()
					if stage != "head_buffered" || owner == nil || id == nil || owner.receiveItem.messageId != *id {
						return
					}
					sequence.Store(actual)
					close(entered)
					select {
					case <-resume:
					case <-actual.ctx.Done():
					}
				}
			})
		t.Cleanup(func() { release(); fixture.cancel() })
		var packetsLock sync.Mutex
		packets := map[uint32]int{}
		fixture.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, peer Peer) {
			if !armed.Load() {
				return
			}
			if peer.TransferKey.LogicalLane != 0 || len(frames) != receiveDeliverBatchMaxFrames {
				callbackViolation.Store(true)
			}
			packetsLock.Lock()
			defer packetsLock.Unlock()
			for _, frame := range frames {
				if frame == nil || !frame.Raw || frame.MessageType != protocol.MessageType_IpIpPacketFromProvider {
					callbackViolation.Store(true)
					continue
				}
				var path IpPath
				payload, err := parseIpPathWithPayloadBorrowed(frame.MessageBytes, &path)
				if err != nil || path.Protocol != IpProtocolTcp || len(payload) != 1240 {
					callbackViolation.Store(true)
					continue
				}
				packets[path.SequenceNumber]++
				if packets[path.SequenceNumber] != 1 {
					callbackViolation.Store(true)
				}
			}
			batchCount.Add(1)
		})
		var receipts [9]atomic.Bool
		var completions [9]atomic.Int64
		started := time.Now()
		warm := consumerPaidPackByteSend(t, fixture, 0, 1, 0, &receipts[0], &completions[0], started)
		warmNumber.Store(warm.scan.sequenceNumber)
		fixture.forward(warm.frame, fixture.receiverIn)
		fixture.acknowledge()
		if !receipts[0].Load() || fixture.deliveredCount != 1 || root.UsedByteCount() != 0 {
			t.Fatal("signed warm original did not settle its real callback, ACK and roots")
		}
		var wires [8]consumerPaidPackByteWire
		for index := range wires {
			wires[index] = consumerPaidPackByteSend(t, fixture, 0, 8, index+1, &receipts[index+1], &completions[index+1], started)
			if wires[index].scan.sequenceId != warm.scan.sequenceId || wires[index].scan.sequenceNumber != warm.scan.sequenceNumber+uint64(index)+1 {
				t.Fatal("actual batch source changed generation or emitted a retry instead of the next original")
			}
		}
		target := wires[0].scan.messageId
		targetId.Store(&target)
		armed.Store(true)
		fixture.forward(wires[0].frame, fixture.receiverIn)
		<-entered
		actual := sequence.Load()
		if actual == nil || len(actual.deliverItems) != 1 || len(actual.deliverFrames) != 8 || actual.packQueueCount.Load() != 0 || batchCount.Load() != 0 {
			t.Fatal("first public batch original did not stop at its actual pre-callback owner boundary")
		}
		for index := 1; index < len(wires); index++ {
			fixture.forward(wires[index].frame, fixture.receiverIn)
		}
		if len(actual.packs) != 7 || actual.packQueueCount.Load() != 7 || fixture.receiver.ReceiveStats().PackHandoffDropCount != 0 || len(fixture.receiverOut) != 0 || batchCount.Load() != 0 {
			t.Fatal("seven public successors did not retain exactly seven ready DATA owners without an early ACK")
		}
		cutStarted := time.Now()
		release()
		synctest.Wait()
		if batchCount.Load() != 1 || callbackViolation.Load() || ackBeforeCallback.Load() || fixture.deliveredCount != 65 || len(fixture.receiverOut) != 1 || time.Now() != cutStarted {
			t.Fatalf("contiguous DATA lost its64-frame callback/prefix: batches=%d violation=%t early_ack=%t delivered=%d wire_acks=%d", batchCount.Load(), callbackViolation.Load(), ackBeforeCallback.Load(), fixture.deliveredCount, len(fixture.receiverOut))
		}
		ack := fixture.take(fixture.receiverOut)
		last := wires[len(wires)-1]
		if ack.ack == nil || ack.ack.Selective || ack.ack.DeliveredImmediately || Id(ack.ack.MessageId) != last.scan.messageId || Id(ack.ack.SequenceId) != last.scan.sequenceId {
			t.Fatal("combined callback did not emit exactly its real final cumulative prefix")
		}
		fixture.forward(ack, fixture.senderIn)
		for index := range receipts {
			if !receipts[index].Load() {
				t.Fatalf("combined real ACK omitted original%d", index)
			}
		}
		packetsLock.Lock()
		deliveredPackets := len(packets)
		packetsLock.Unlock()
		if deliveredPackets != 64 || fixture.sender.SendRecoveryStats().TimeoutResendWriteCount != 0 || len(fixture.receiverOut) != 0 {
			t.Fatal("combined batch repeated packets, needed recovery, or emitted another ACK")
		}
		fixture.cancel()
		joined, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, client := range []*Client{fixture.sender, fixture.receiver} {
			if err := client.CloseAndWait(joined); err != nil {
				t.Fatal(err)
			}
		}
		for _, route := range []Route{fixture.senderOut, fixture.senderIn, fixture.receiverOut, fixture.receiverIn} {
			for len(route) != 0 {
				MessagePoolReturn(<-route)
			}
		}
		for _, budget := range []*TransferMemoryBudget{pack, head, rx, root} {
			assertRetainedBudgetBalance(t, budget)
			if budget.UsedByteCount() != 0 || budget.Stats().QuotaByteCount != 0 {
				t.Fatal("combined batch retained custody after joined shutdown")
			}
		}
		if len(log.messages()) != 0 {
			t.Fatal("combined DATA batch generated an independent receive error")
		}
		t.Log("BUFFERED_DATA_CONTIGUOUS_BATCH_PRESERVED frames=64 originals=8 callbacks=1 cumulative_acks=1 elapsed=0 owners_joined=true")
	})
}
