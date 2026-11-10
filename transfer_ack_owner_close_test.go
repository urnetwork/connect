package connect

import (
	"bytes"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

type acceptedAckCloseResult struct{ err error }

func TestTransferAcceptedPrefixAckWinsLaterSourceCancel(t *testing.T) {
	checkTransferAcceptedAckClose(t, "prefix")
}

func TestTransferSelectiveAckIsNotDeliveryOnClose(t *testing.T) {
	checkTransferAcceptedAckClose(t, "selective")
}

func TestTransferWithdrawnSelectiveAckIsNotDeliveryOnClose(t *testing.T) {
	checkTransferAcceptedAckClose(t, "withdrawn")
}

func TestTransferUnknownAckIsNotDeliveryOnClose(t *testing.T) {
	checkTransferAcceptedAckClose(t, "unknown")
}

// The actual worker is held after its third physical initial write. Feedback
// is produced by the real receiver and accepted before source cancellation.
// No queue identity, ACK frontier or retained item's status is fabricated.
func checkTransferAcceptedAckClose(t *testing.T, kind string) {
	t.Helper()
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		barrier := newAckHandoffLifetimeBarrier()
		defer barrier.close()
		var destination Id
		fixture := newWindowRoundFixture(t, func(settings *SendBufferSettings) {
			settings.afterInitialWriteQueuedForTest = func(id sendSequenceId, number uint64) {
				if id.Destination == destination && number == 2 {
					barrier.wait()
				}
			}
		}, func(settings *ReceiveBufferSettings) {
			if kind == "withdrawn" {
				settings.ReceiveQueueMaxByteCount = 100
				settings.ReceiveHoldPolicy = ReceiveHoldEvict
				settings.EvictionNotice = true
			}
		}, TransportTypeH1)
		destination = fixture.receiver.ClientId()
		var results [3]atomic.Pointer[acceptedAckCloseResult]
		var calls [3]atomic.Int32
		var originals [3]*windowRoundFrame
		for index := range originals {
			frame := RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: string(bytes.Repeat([]byte{byte(index + 1)}, 64))})
			slot := index
			admitted, err := fixture.sender.SendWithTimeoutDetailed(frame, destination, func(err error) {
				results[slot].Store(&acceptedAckCloseResult{err: err})
				calls[slot].Add(1)
			}, time.Second)
			if !admitted || err != nil {
				if !admitted {
					MessagePoolReturn(frame.MessageBytes)
				}
				t.Fatalf("public original %d was not admitted: %t/%v", index, admitted, err)
			}
			originals[index] = fixture.takePack(uint64(index))
		}
		barrier.requireReached(t)
		sequence := fixture.sequence()
		if sequence.resendQueue.Len() != 3 {
			t.Fatal("the worker barrier did not retain all three real initial writes")
		}
		accept := func(reply *windowRoundFrame) {
			t.Helper()
			if reply.ack == nil || !bytes.Equal(reply.ack.SequenceId, originals[0].pack.SequenceId) {
				t.Fatal("receiver feedback did not belong to the captured source generation")
			}
			if !fixture.sender.sendBuffer.Ack(destination, reply.ack, 0) {
				t.Fatal("real feedback was not accepted before cancellation")
			}
			fixture.drop(reply)
		}
		switch kind {
		case "prefix", "unknown":
			reply := fixture.receive(originals[0])
			if reply.ack.Selective || !bytes.Equal(reply.ack.MessageId, originals[0].pack.MessageId) || fixture.deliveredCount != 1 {
				t.Fatal("receiver did not actually deliver and cumulatively acknowledge the exact prefix")
			}
			if kind == "unknown" {
				reply.ack.MessageId = NewId().Bytes()
			}
			accept(reply)
		case "selective", "withdrawn":
			reply := fixture.receive(originals[2])
			if !reply.ack.Selective || !bytes.Equal(reply.ack.MessageId, originals[2].pack.MessageId) || fixture.deliveredCount != 0 {
				t.Fatal("future tail did not produce exact selective-only receipt behind the missing prefix")
			}
			accept(reply)
			if kind == "withdrawn" {
				fixture.forward(originals[1], fixture.receiverIn)
				replies := fixture.takeAcknowledgements()
				evicted := false
				for _, reply := range replies {
					for _, number := range reply.ack.EvictedSequenceNumbers {
						if number != originals[2].pack.SequenceNumber {
							t.Fatal("receiver withdrew another original")
						}
						evicted = true
					}
					if !reply.ack.Selective {
						t.Fatal("withdrawal crossed the undelivered prefix")
					}
					accept(reply)
				}
				if !evicted || fixture.receiver.ReceiveStats().ReceiveQueueEvictionCount != 1 {
					t.Fatal("real bounded receiver did not withdraw its previously acknowledged tail")
				}
			}
		default:
			t.Fatal("unknown close-control kind")
		}
		pending := sequence.ackWindow.Snapshot(false)
		if kind == "prefix" && (pending.ackUpdateCount == 0 || pending.headAck.messageId != RequireIdFromBytes(originals[0].pack.MessageId)) {
			t.Fatal("accepted real prefix was not pending at the owner pause")
		}
		for index := range results {
			if results[index].Load() != nil {
				t.Fatal("paused owner applied feedback before the cut")
			}
		}
		sequence.Cancel()
		barrier.close()
		<-sequence.done
		synctest.Wait()
		for index := range results {
			result := results[index].Load()
			wantDelivered := kind == "prefix" && index == 0
			if result == nil || calls[index].Load() != 1 || (result.err == nil) != wantDelivered {
				t.Fatalf("%s original%d: completion=%v calls=%d want_delivered=%t", kind, index, result, calls[index].Load(), wantDelivered)
			}
		}
		if sequence.resendQueue.Len() != 0 || len(sequence.ackLifetimes.items) != 0 || len(fixture.senderOut) != 0 {
			t.Fatal("close retained originals, lifetimes or prepared a new network write")
		}
		t.Logf("ACCEPTED_ACK_CLOSE: kind=%s actual_originals=3 delivered_callbacks=%d all_terminal_once=true ownership_joined=true", kind, map[bool]int{false: 0, true: 1}[kind == "prefix"])
	})
}
