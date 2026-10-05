// The send-window instrument must preserve its lossless carrier contract when
// a receive worker is delayed; host scheduling must not manufacture loss.
package connect

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Hold the receive worker behind a barrier and fill its handoff. The next
// complete frames must wait on the carrier, then arrive after release. Two
// handoff slots force a combined callback, so delivery cannot be measured by
// callback count; one slot preserves the original backpressure regression.
func TestSendWindowHarnessPreservesPacksUnderReceiveBackpressure(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, handoffSlots := range []int{1, 2} {
		t.Run(fmt.Sprintf("handoff_slots_%d", handoffSlots), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				entered, release := make(chan struct{}), make(chan struct{})
				var enteredOnce sync.Once
				var releaseOnce sync.Once
				releaseWorker := func() { releaseOnce.Do(func() { close(release) }) }
				defer releaseWorker()
				harness := newSendWindowHarnessWithClient(t, ctx, 200*time.Millisecond, nil,
					func(settings *ClientSettings) {
						settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
						settings.ReceiveBufferSettings.SequenceBufferSize = handoffSlots
						settings.ReceiveBufferSettings.beforeRunReceiveSequenceForTest = func(receiveSequenceId) {
							enteredOnce.Do(func() { close(entered) })
							<-release
						}
					})
				var delivered []string
				var batchSizes []int
				harness.receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
					batchSizes = append(batchSizes, len(frames))
					for _, frame := range frames {
						var message protocol.SimpleMessage
						if err := ProtoUnmarshal(frame.MessageBytes, &message); err != nil {
							t.Errorf("decode delivered frame: %v", err)
							continue
						}
						delivered = append(delivered, message.Content)
					}
				})
				want := []string{"synthetic window frame 0", "synthetic window frame 1", "synthetic window frame 2"}
				type ackResult struct {
					index int
					err   error
				}
				acks := make(chan ackResult, len(want))
				for i, content := range want {
					frame := RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: content})
					admitted, err := harness.sender.SendWithTimeoutDetailed(frame, harness.receiverId,
						func(err error) { acks <- ackResult{index: i, err: err} }, 0)
					if !admitted || err != nil {
						MessagePoolReturn(frame.MessageBytes)
						t.Fatalf("offer %d: admitted=%t err=%v", i, admitted, err)
					}
					if i == 0 {
						<-entered
					}
					synctest.Wait()
				}
				before := harness.receiver.ReceiveStats()
				if before.PackHandoffWaitCount != 1 || before.PackHandoffWaitSuccess != 0 || before.PackHandoffMaxCount != uint64(handoffSlots) {
					t.Fatalf("paused receiver did not fill its handoff and backpressure the next Pack: %+v", before)
				}
				if len(delivered) != 0 || len(acks) != 0 {
					t.Fatalf("paused receiver delivered %d frames and acknowledged %d Packs", len(delivered), len(acks))
				}
				releaseWorker()
				synctest.Wait()
				if before.PackHandoffDropCount != 0 || len(delivered) != len(want) {
					t.Fatalf("lossless window fixture dropped %d Packs behind a paused receiver and delivered %d of %d frames (callback batches=%v)", before.PackHandoffDropCount, len(delivered), len(want), batchSizes)
				}
				if !slices.Equal(delivered, want) {
					t.Fatalf("delivered frames = %q, want exactly once in order %q", delivered, want)
				}
				if batchSizes[0] < handoffSlots {
					t.Fatalf("first callback batch = %d frames, want at least %d queued frames", batchSizes[0], handoffSlots)
				}
				after := harness.receiver.ReceiveStats()
				if after.PackHandoffDropCount != 0 || after.PackHandoffWaitSuccess != after.PackHandoffWaitCount || after.PackHandoffMaxCount != uint64(handoffSlots) {
					t.Fatalf("released receiver did not drain its bounded handoff losslessly: %+v", after)
				}
				ackDeadline := time.After(time.Second)
				acknowledged := make([]bool, len(want))
				for range want {
					select {
					case result := <-acks:
						if result.err != nil {
							t.Fatalf("acknowledge delivered Pack %d: %v", result.index, result.err)
						}
						if acknowledged[result.index] {
							t.Fatalf("Pack %d acknowledged more than once", result.index)
						}
						acknowledged[result.index] = true
					case <-ackDeadline:
						t.Fatal("delivered Packs were not all acknowledged")
					}
				}
				synctest.Wait()
				if len(acks) != 0 || !slices.Equal(delivered, want) {
					t.Fatalf("duplicate completion: extra ACKs=%d, delivered=%q", len(acks), delivered)
				}
				if stats := harness.sender.DestinationSendStats(harness.receiverId); stats.ResendWriteByteCount != 0 {
					t.Fatalf("lossless fixture retransmitted %d bytes", stats.ResendWriteByteCount)
				}
			})
		})
	}
}
