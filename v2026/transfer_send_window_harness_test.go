// The carrier retains complete frames; a full shared Client handoff may
// refuse one without ACK, leaving the real sender to recover that original.
package connect

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Hold the receive worker and preserve its actual accepted prefix. Shared
// admission refuses the suffix immediately without ACK. After release, only
// the sender's normal timer may recover those same identities. One and two
// slots retain the original count bound and combined-callback coverage.
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
				var identities [3]atomic.Pointer[Id]
				var acceptedAttempts, refusedAttempts [3]atomic.Uint64
				var selectiveAcks, cumulativeAcks [3]atomic.Uint64
				var changedIdentity atomic.Bool
				harness := newSendWindowHarnessWithClient(t, ctx, 200*time.Millisecond, nil,
					func(settings *ClientSettings) {
						settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
						settings.ReceiveBufferSettings.SequenceBufferSize = handoffSlots
						settings.ReceiveBufferSettings.beforeRunReceiveSequenceForTest = func(receiveSequenceId) {
							enteredOnce.Do(func() { close(entered) })
							<-release
						}
						// This existing metadata observer records real Pack disposition;
						// it neither admits work nor gates any callback or owner.
						settings.ReceiveBufferSettings.ProgressObserver = func(event TransferProgressEvent) {
							// ACK metadata is immutable before coalescing. Observe the
							// actual incoming disposition rather than a mutable pooled
							// sendItem flag whose worker may rewrite it on a later turn.
							if event.Stage == "receive_ack_begin" {
								for i := range identities {
									if id := identities[i].Load(); id != nil && *id == event.MessageId {
										if event.Selective {
											selectiveAcks[i].Add(1)
										} else {
											cumulativeAcks[i].Add(1)
										}
									}
								}
								return
							}
							if event.Stage != "receive_pack_end" {
								return
							}
							if event.SequenceNumber >= 3 || event.NoAck {
								changedIdentity.Store(true)
								return
							}
							index := event.SequenceNumber
							if first := identities[index].Load(); first != nil {
								if *first != event.MessageId {
									changedIdentity.Store(true)
								}
							} else {
								id := event.MessageId
								identities[index].Store(&id)
							}
							if event.Success {
								acceptedAttempts[index].Add(1)
							} else {
								refusedAttempts[index].Add(1)
							}
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
				if before.PackHandoffWaitCount != 0 || before.PackHandoffWaitSuccess != 0 ||
					before.PackHandoffDropCount != uint64(len(want)-handoffSlots) || before.PackHandoffMaxCount != uint64(handoffSlots) {
					t.Fatalf("paused receiver did not preserve the bounded accepted prefix and refuse its suffix: %+v", before)
				}
				if len(delivered) != 0 || len(acks) != 0 || changedIdentity.Load() {
					t.Fatalf("paused receiver delivered %d frames, acknowledged %d Packs, or changed identity", len(delivered), len(acks))
				}
				var queued *ReceiveSequence
				harness.receiver.receiveBuffer.mutex.Lock()
				for _, sequence := range harness.receiver.receiveBuffer.receiveSequences {
					if sequence.source.SourceId == harness.sender.ClientId() {
						queued = sequence
					}
				}
				harness.receiver.receiveBuffer.mutex.Unlock()
				if queued == nil || len(queued.packs) != handoffSlots || queued.packQueueCount.Load() != int64(handoffSlots) {
					t.Fatal("disposition counters did not describe the actual full receive queue")
				}
				for i := range want {
					accepted, refused := uint64(0), uint64(1)
					if i < handoffSlots {
						accepted, refused = 1, 0
					}
					if identities[i].Load() == nil || acceptedAttempts[i].Load() != accepted || refusedAttempts[i].Load() != refused {
						t.Fatalf("initial Pack %d did not have its exact prefix/suffix disposition", i)
					}
				}
				releaseWorker()
				synctest.Wait()
				if !slices.Equal(delivered, want[:handoffSlots]) || len(acks) != 0 {
					t.Fatalf("released receiver did not deliver only its accepted prefix before delayed ACKs: %q", delivered)
				}
				if batchSizes[0] < handoffSlots {
					t.Fatalf("first callback batch = %d frames, want at least %d queued frames", batchSizes[0], handoffSlots)
				}
				acknowledged := make([]bool, len(want))
				acceptAck := func(result ackResult) {
					t.Helper()
					if result.index < 0 || len(want) <= result.index || result.err != nil {
						t.Fatalf("invalid completion: index=%d error=%v", result.index, result.err)
					}
					if acknowledged[result.index] {
						t.Fatalf("Pack %d acknowledged more than once", result.index)
					}
					acknowledged[result.index] = true
				}
				// Preserve the original one-second bound for the already accepted
				// prefix. No refused Pack can be acknowledged by this transition.
				prefixDeadline := time.After(time.Second)
				for range handoffSlots {
					select {
					case result := <-acks:
						if handoffSlots <= result.index {
							t.Fatal("the receiver acknowledged an original it refused")
						}
						acceptAck(result)
					case <-prefixDeadline:
						t.Fatal("accepted prefix was not acknowledged within its original bound")
					}
				}
				synctest.Wait()
				var source *SendSequence
				harness.sender.sendBuffer.mutex.Lock()
				for _, sequence := range harness.sender.sendBuffer.sendSequencesBySequenceId {
					if sequence.destination == harness.receiverId {
						source = sequence
					}
				}
				harness.sender.sendBuffer.mutex.Unlock()
				if source == nil {
					t.Fatal("the refused suffix lost its actual sender sequence")
				}
				count, _, _, _ := source.ResendQueueSizeAndMessageTypes()
				if count != len(want)-handoffSlots || len(acks) != 0 || !slices.Equal(delivered, want[:handoffSlots]) {
					t.Fatal("accepted-prefix ACKs retired or delivered a refused suffix original")
				}
				// The accepted prefix must positively exercise this observer before
				// its zero counters can prove absence of suffix ACK/SACK receipts.
				var observedPrefixAcks uint64
				for i := 0; i < handoffSlots; i++ {
					observedPrefixAcks += cumulativeAcks[i].Load()
				}
				if observedPrefixAcks == 0 {
					t.Fatal("accepted-prefix completions lack actual cumulative ACK observation")
				}
				for i := range want {
					// ContainsMessageId copies the stable sequence number while
					// membership is locked; no pooled sendItem pointer escapes.
					number, present := source.resendQueue.ContainsMessageId(*identities[i].Load())
					if i < handoffSlots {
						if !acknowledged[i] || present {
							t.Fatal("accepted prefix did not retire exactly its source owners")
						}
					} else if acknowledged[i] || !present || number != uint64(i) ||
						selectiveAcks[i].Load() != 0 || cumulativeAcks[i].Load() != 0 {
						t.Fatal("refused suffix lost exact retry custody or received an actual ACK/SACK")
					}
				}
				// No test replay, route injection, timer change or fabricated ACK:
				// the unchanged sender must recover before its 60-second lifetime.
				// The finite virtual bound allows its real cold resend/defer policy.
				recoveryDeadline := time.After(30 * time.Second)
				for range len(want) - handoffSlots {
					select {
					case result := <-acks:
						acceptAck(result)
					case <-recoveryDeadline:
						t.Fatal("natural sender recovery did not deliver and acknowledge the refused suffix")
					}
				}
				synctest.Wait()
				after := harness.receiver.ReceiveStats()
				count, _, _, _ = source.ResendQueueSizeAndMessageTypes()
				if changedIdentity.Load() || len(acks) != 0 || !slices.Equal(delivered, want) || count != 0 ||
					after.PackHandoffWaitCount != 0 || after.PackHandoffWaitSuccess != 0 ||
					after.PackHandoffDropCount < before.PackHandoffDropCount || after.PackHandoffMaxCount != uint64(handoffSlots) {
					t.Fatalf("recovery changed identities, exact delivery/completion, queue bounds or nonblocking admission: delivered=%q stats=%+v", delivered, after)
				}
				for i := handoffSlots; i < len(want); i++ {
					if acceptedAttempts[i].Load() == 0 || refusedAttempts[i].Load() == 0 {
						t.Fatal("a refused original was not later accepted with its same observed identity")
					}
				}
				if stats := harness.sender.DestinationSendStats(harness.receiverId); stats.ResendWriteByteCount == 0 ||
					harness.sender.ReceiveStats().SendPackDeadlineDropCount != 0 {
					t.Fatalf("the real sender did not recover its refused originals cleanly: %+v", stats)
				}
			})
		})
	}
}
