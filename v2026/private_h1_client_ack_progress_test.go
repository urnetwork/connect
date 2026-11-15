// The shared Client pump must distinguish physical ACK arrival from actual
// sender completion when a different receive sequence cannot accept a Pack.
package connect

import (
	"bytes"
	"context"
	"hash/crc64"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gorilla/websocket"
	"github.com/urnetwork/connect/v2026/protocol"
)

func TestPrivateH1ClientAckCompletionBeforeForeignPackRelease(t *testing.T) {
	privateH1ClientAckProgress(t, false, false)
}

func TestPrivateH1PlusClientAckCompletionBeforeForeignPackRelease(t *testing.T) {
	privateH1ClientAckProgress(t, true, false)
}

func TestPrivateH1ClientCarrierCancelReleasesReceiptWithoutFalseAck(t *testing.T) {
	privateH1ClientAckProgress(t, false, true)
}

func TestPrivateH1PlusClientCarrierCancelReleasesReceiptWithoutFalseAck(t *testing.T) {
	privateH1ClientAckProgress(t, true, true)
}

// This is a reduced-count composition witness, not a production throughput
// fixture. Every retained byte belongs to a real decoded/queued application
// original. The constructor selects a one-slot sequence queue to reach its
// count boundary with three genuine Packs; no budget is filled by a test
// reserve. The shipping reliable timeout and all timers remain unchanged.
//
// The first application callback is held independently of this Client's
// outgoing original and its ACK. A second Pack occupies the queue; a
// third reaches the actual full Pack handoff. Its peer's already-produced
// genuine ACK is then physically read over the same H1 generation. Manual
// callback release is permitted only after the primary observation, for exact
// recovery and cleanup. The cancellation controls retire only the
// physical carrier after the third Pack has truthfully refused, before offering
// the ACK, then deliver that genuine ACK through the existing route. This avoids inventing an ordering between a
// queued valid ACK and concurrent route withdrawal.
func privateH1ClientAckProgress(t *testing.T, plus, cancelCarrier bool) {
	resetH1UpgradeTestState(t)
	GetMessagePoolAggregateStats()
	var cause string
	synctest.Test(t, func(t *testing.T) {
		t.Cleanup(func() {
			if !t.Failed() {
				t.Log("H1_CLIENT_ACK_PROGRESS_CLEANUP_JOINED")
			}
		})
		assertMessagePoolOwnership(t)
		packBudget := NewTransferMemoryBudget(kib(64))
		rxBudget := NewTransferMemoryBudget(kib(64))
		t.Cleanup(func() {
			assertRetainedBudgetBalance(t, packBudget)
			assertRetainedBudgetBalance(t, rxBudget)
		})
		var ackBegins atomic.Int64
		f := newWindowRoundFixture(t, nil, func(settings *ReceiveBufferSettings) {
			if settings.ReliablePackHandoffTimeout != -1 {
				t.Fatal("shipping reliable handoff timeout changed")
			}
			settings.ProtocolVersion = 2
			settings.SequenceBufferSize = 1
			settings.H1SequenceBufferSize = 1
			settings.H1SequenceBufferAdaptiveMaxSize = 0
			settings.PackQueueBudget = packBudget
			settings.PackQueueRetainedByteAccounting = true
			settings.ReceiveQueueBudget = rxBudget
			settings.ReceiveQueueRetainedByteAccounting = true
			settings.ProgressObserver = func(event TransferProgressEvent) {
				if event.Stage == "receive_ack_begin" {
					ackBegins.Add(1)
				}
			}
		})
		pending := func(client *Client, destination Id) int {
			client.sendBuffer.mutex.Lock()
			var sequences []*SendSequence
			for _, sequence := range client.sendBuffer.sendSequencesBySequenceId {
				if sequence.destination == destination {
					sequences = append(sequences, sequence)
				}
			}
			client.sendBuffer.mutex.Unlock()
			count := 0
			for _, sequence := range sequences {
				n, _, _, _ := sequence.ResendQueueSizeAndMessageTypes()
				count += n
			}
			return count
		}

		manualRelease := make(chan struct{})
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(manualRelease) }) }
		var completed, callbackErrors atomic.Int64
		frame := RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: string(make([]byte, 800))})
		accepted, err := f.receiver.SendWithTimeoutDetailed(frame, f.sender.ClientId(), func(err error) {
			if err != nil {
				callbackErrors.Add(1)
				return
			}
			completed.Add(1)
		}, time.Second)
		if !accepted {
			MessagePoolReturn(frame.MessageBytes)
		}
		if !accepted || err != nil {
			t.Fatalf("independent outgoing original was not admitted: accepted=%t err=%v", accepted, err)
		}
		original := f.take(f.receiverOut)
		if original.pack == nil || original.pack.Nack {
			t.Fatal("outgoing original lacks ordinary retry custody")
		}
		f.forward(original, f.senderIn)
		ack := f.take(f.senderOut)
		if ack.ack == nil || ack.ack.Selective || !bytes.Equal(ack.ack.MessageId, original.pack.MessageId) {
			t.Fatal("actual peer did not produce the exact cumulative ACK")
		}

		entered := make(chan struct{})
		returned := make(chan struct{})
		var callbackOnce sync.Once
		f.receiver.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, _ Peer) {
			if source.SourceId != f.sender.ClientId() || len(frames) == 0 {
				return
			}
			callbackOnce.Do(func() {
				close(entered)
				defer close(returned)
				select {
				case <-manualRelease:
				case <-f.ctx.Done():
				}
			})
		})
		incoming := []*windowRoundFrame{f.write(800), f.write(800), f.write(800)}
		if pending(f.receiver, f.sender.ClientId()) != 1 || pending(f.sender, f.receiver.ClientId()) != 3 {
			t.Fatal("the four actual reliable originals were not retained")
		}
		ackHash := crc64.Checksum(ack.bytes, transferProgressChecksumTable())
		var ackRead atomic.Bool
		h1 := newH1LivenessFixtureWithRoutes(t, false, 0, f.receiver.RouteManager(), func(settings *PlatformTransportSettings) {
			ApplyMobilePlatformTransportMemoryPolicy(settings, mib(64))
			settings.EnableH1Plus = plus
			settings.ProgressObserver = func(event TransferProgressEvent) {
				if event.Stage == "h1_read" && event.Success && event.WireHash == ackHash && event.ByteCount == len(ack.bytes) {
					ackRead.Store(true)
				}
			}
		})
		var finishOnce sync.Once
		finish := func() {
			finishOnce.Do(func() {
				release()
				h1.cancel()
				_ = h1.client.Close()
				if err := h1.transport.CloseAndWait(context.Background()); err != nil {
					t.Error(err)
				}
				f.cancel()
				join, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				for _, client := range []*Client{f.sender, f.receiver} {
					if err := client.CloseAndWait(join); err != nil {
						t.Error(err)
					}
				}
				for _, wire := range f.heldFrames {
					if wire.bytes != nil {
						MessagePoolReturn(wire.bytes)
						wire.bytes = nil
					}
				}
				for _, route := range []Route{f.senderOut, f.senderIn, f.receiverOut, f.receiverIn} {
					for len(route) != 0 {
						MessagePoolReturn(<-route)
					}
				}
			})
		}
		t.Cleanup(finish)
		for i, wire := range incoming {
			if err := h1.peer.WriteMessage(websocket.BinaryMessage, wire.bytes); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
			if i == 0 {
				select {
				case <-entered:
				default:
					t.Fatal("actual first receive callback was not entered")
				}
				if packBudget.UsedByteCount() != 0 || rxBudget.UsedByteCount() <= 0 {
					t.Fatal("first callback did not own decoded RX after releasing Pack credit")
				}
			}
		}
		sequence := f.receiveSequence()
		lane := &h1.transport.h1RetainedBudget.receive
		handoff := f.receiver.ReceiveStats()
		blockedHandoff := handoff.PackHandoffWaitCount == 1 && handoff.PackHandoffDropCount == 0
		refusedHandoff := handoff.PackHandoffWaitCount == 0 && handoff.PackHandoffDropCount == 1
		if sequence.packQueueCount.Load() != 1 || len(sequence.packs) != 1 || sequence.packQueueH1Limit != 1 ||
			(!blockedHandoff && !refusedHandoff) || packBudget.UsedByteCount() <= 0 ||
			rxBudget.UsedByteCount() <= packBudget.UsedByteCount() ||
			blockedHandoff && h1RetainedTestByteCount(lane) <= 0 ||
			refusedHandoff && h1RetainedTestByteCount(lane) != 0 ||
			f.ackedCount != 0 || completed.Load() != 0 || callbackErrors.Load() != 0 {
			t.Fatal("real callback, queued Pack and exact third-Pack disposition were not established")
		}
		select {
		case <-returned:
			t.Fatal("the held callback returned before its independent release")
		default:
		}
		started := time.Now()
		if !cancelCarrier {
			if err := h1.peer.WriteMessage(websocket.BinaryMessage, ack.bytes); err != nil {
				t.Fatal(err)
			}
		}
		synctest.Wait()
		completedBeforeRelease := completed.Load()
		if !cancelCarrier && !ackRead.Load() || !time.Now().Equal(started) || callbackErrors.Load() != 0 ||
			h1.transport.ReceiveStats().H1.QueueDropMessageCount != 0 ||
			h1.transport.ReceiveStats().H1.QueueBackpressureMessageCount != 0 {
			t.Fatal("exact ACK was not physically read without a timer or carrier pressure")
		}
		h1.assertConnected(t)
		if completedBeforeRelease == 0 {
			if (!cancelCarrier && !blockedHandoff || cancelCarrier && !refusedHandoff) ||
				ackBegins.Load() != 0 || pending(f.receiver, f.sender.ClientId()) != 1 ||
				f.ackedCount != 0 || sequence.packQueueCount.Load() != 1 || len(sequence.packs) != 1 ||
				blockedHandoff && h1RetainedTestByteCount(lane) <= 0 ||
				refusedHandoff && h1RetainedTestByteCount(lane) != 0 {
				t.Fatal("ACK completion was absent for a cause other than the exact parked shared Client")
			}
		} else if completedBeforeRelease != 1 || ackBegins.Load() != 1 || pending(f.receiver, f.sender.ClientId()) != 0 {
			t.Fatal("independent ACK completion was duplicated or not owned by the normal sender")
		}
		if cancelCarrier {
			if completedBeforeRelease != 0 || !refusedHandoff {
				t.Fatal("cancellation control did not reach truthful shared-pump refusal")
			}
			if err := h1.transport.CloseAndWait(context.Background()); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
			if f.receiver.ctx.Err() != nil || h1RetainedTestByteCount(lane) != 0 ||
				completed.Load() != 0 || f.ackedCount != 0 || pending(f.sender, f.receiver.ClientId()) != 3 {
				t.Fatal("physical cancellation canceled the Client, leaked its receipt, or falsely acknowledged data")
			}
			// This is the peer's saved real ACK, not a synthesized completion.
			// It enters the existing route after the canceled carrier withdrew.
			f.forward(ack, f.receiverIn)
			if completed.Load() != 1 || f.ackedCount != 0 {
				t.Fatal("alternate-route ACK did not complete independently of the still-held callback")
			}
			release()
			synctest.Wait()
		} else {
			// Never before the primary observation: this release breaks only the
			// fixture's callback dependency so every real original can drain.
			release()
			synctest.Wait()
		}
		if completed.Load() != 1 || callbackErrors.Load() != 0 || pending(f.receiver, f.sender.ClientId()) != 0 {
			t.Fatal("normal ACK decoding did not retire the exact outgoing original after recovery")
		}
		select {
		case <-returned:
		default:
			t.Fatal("actual receive callback did not join after recovery")
		}
		needsRetry := cancelCarrier || refusedHandoff
		wantDelivered := 3
		if needsRetry {
			wantDelivered = 2
		}
		if f.deliveredCount != wantDelivered || sequence.packQueueCount.Load() != 0 || packBudget.UsedByteCount() != 0 {
			t.Fatal("recovery changed the accepted prefix or retained an unaccepted Pack")
		}
		forwardAcks := func(forbidThird bool) int {
			forwarded := 0
			for _, message := range h1.payloadMessagesSnapshot() {
				var envelope protocol.TransferFrame
				if err := ProtoUnmarshal(message, &envelope); err != nil {
					t.Fatal(err)
				}
				if envelope.Ack != nil || envelope.Frame != nil && envelope.Frame.MessageType == protocol.MessageType_TransferAck {
					route := make(Route, 1)
					route <- MessagePoolCopy(message)
					wire := f.take(route)
					if wire.ack == nil || wire.ack.Selective || forbidThird && bytes.Equal(incoming[2].pack.MessageId, wire.ack.MessageId) {
						t.Fatal("actual return wire was not cumulative or acknowledged unaccepted data")
					}
					f.forward(wire, f.senderIn)
					forwarded++
				}
			}
			for len(f.receiverOut) != 0 {
				wire := f.take(f.receiverOut)
				if wire.ack == nil || wire.ack.Selective || forbidThird && bytes.Equal(incoming[2].pack.MessageId, wire.ack.MessageId) {
					t.Fatal("return wire was not cumulative or acknowledged unaccepted data during cleanup")
				}
				f.forward(wire, f.senderIn)
				forwarded++
			}
			return forwarded
		}
		forwarded := forwardAcks(needsRetry)
		if needsRetry {
			if f.ackedCount != 2 || pending(f.sender, f.receiver.ClientId()) != 1 {
				t.Fatal("unaccepted third original lost retry custody or acquired an ACK")
			}
			f.forward(incoming[2], f.receiverIn)
			forwarded += forwardAcks(false)
		}
		if forwarded == 0 || f.deliveredCount != 3 || f.ackedCount != 3 || pending(f.sender, f.receiver.ClientId()) != 0 ||
			packBudget.UsedByteCount() != 0 || rxBudget.UsedByteCount() != 0 {
			t.Fatal("actual data ACKs or paid owners failed exact recovery before cleanup")
		}
		finish()
		if h1RetainedTestByteCount(lane) != 0 {
			t.Fatal("joined carrier retained receive credit")
		}
		t.Logf("H1_CLIENT_ACK_PROGRESS_DRAINED plus=%t carrier_cancel=%t physical_ack=%t completed_before_release=%d outgoing_completed=1 incoming_delivered=3 incoming_acked=3", plus, cancelCarrier, ackRead.Load(), completedBeforeRelease)
		if !cancelCarrier && completedBeforeRelease == 0 {
			cause = "H1_CLIENT_ACK_BLOCKED_BY_FOREIGN_PACK: physical_ack_read=true actual_sender_completion_before_foreign_release=false genuine_recovery=true"
		}
	})
	if !t.Failed() && cause != "" {
		t.Fatal(cause)
	}
}
