// Private, uncompiled/unrun fixture controls: pooled final-share evidence is
// distinct from unpooled frame detachment and joined receive-owner closure.
package connect

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/protocol"
)

// Called after Client and NAT joins. Inspect actual constructor-owned queues,
// indexes and complete backing slices, not just their visible lengths.
// Clearing these references is not a measurement of Go garbage collection.
func receiptAdmissionGenericOwnersError(frames []*protocol.Frame, sequences map[*ReceiveSequence]struct{}) error {
	for index, frame := range frames {
		if frame == nil || frame.MessageBytes != nil {
			return fmt.Errorf("generic frame %d retains its receive-owned message bytes", index)
		}
	}
	for sequence := range sequences {
		select {
		case <-sequence.done:
		default:
			return fmt.Errorf("generic receive sequence owned workers have not joined")
		}
		if sequence.ctx.Err() == nil || len(sequence.packs) != 0 {
			return fmt.Errorf("joined generic receive sequence retains its input owner")
		}
		if len(sequence.deliverItems) != 0 || len(sequence.deliverFrames) != 0 || len(sequence.heldScratch) != 0 {
			return fmt.Errorf("joined generic receive sequence retains a current batch or held scratch")
		}
		for _, item := range sequence.deliverItems[:cap(sequence.deliverItems)] {
			if item != nil {
				return fmt.Errorf("joined generic receive sequence retains a batch backing item")
			}
		}
		for _, frame := range sequence.deliverFrames[:cap(sequence.deliverFrames)] {
			if frame != nil {
				return fmt.Errorf("joined generic receive sequence retains a batch backing frame")
			}
		}
		for _, item := range sequence.heldScratch[:cap(sequence.heldScratch)] {
			if item != nil {
				return fmt.Errorf("joined generic receive sequence retains a held scratch backing item")
			}
		}
		if queue := sequence.receiveQueue; queue != nil {
			clear := func() bool {
				queue.stateLock.Lock()
				defer queue.stateLock.Unlock()
				if len(queue.orderedItems) != 0 || len(queue.maxHeap.orderedItems) != 0 ||
					len(queue.messageIdItems) != 0 || len(queue.sequenceNumberItems) != 0 ||
					queue.byteCount != 0 || queue.queueByteCount != 0 || queue.borrowedByteCount != 0 {
					return false
				}
				for _, item := range queue.orderedItems[:cap(queue.orderedItems)] {
					if item != nil {
						return false
					}
				}
				for _, item := range queue.maxHeap.orderedItems[:cap(queue.maxHeap.orderedItems)] {
					if item != nil {
						return false
					}
				}
				return true
			}()
			if !clear {
				return fmt.Errorf("joined generic receive sequence retains its hold owner or index")
			}
		}
		if queue := sequence.deliveryQueue; queue != nil {
			clear := func() bool {
				queue.mutex.Lock()
				defer queue.mutex.Unlock()
				if len(queue.items) != 0 || len(queue.operations) != 0 ||
					queue.bytes != 0 || queue.controlCount != 0 || queue.controlBytes != 0 {
					return false
				}
				for _, receipt := range queue.items[:cap(queue.items)] {
					if receipt != nil {
						return false
					}
				}
				for _, operation := range queue.operations[:cap(queue.operations)] {
					if operation != nil {
						return false
					}
				}
				return true
			}()
			if !clear {
				return fmt.Errorf("joined generic receive sequence retains a receipt or downstream operation")
			}
		}
	}
	return nil
}

// The same actual callback/ACK/owner path straddles the last pool class.
// Exact V3 fails only the 8193-byte final-share oracle during cleanup.
func TestReceiveReceiptAdmissionGenericPoolBoundary(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		for _, size := range []int{8192, 8193} {
			f := newReceiptAdmissionFixtureWithProvider(t, 1, false)
			delivered := f.observeGeneric()
			var observed *protocol.Frame
			var pooled bool
			remove := f.client.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
				for _, frame := range frames {
					if frame.MessageType == protocol.MessageType_TestSimpleMessage {
						observed = frame
						pooled, _ = MessagePoolCheck(frame.MessageBytes)
					}
				}
			})
			t.Cleanup(remove)
			messageId := NewId()
			f.offerGeneric(0, messageId, size)
			sequence := f.sequence(f.sequenceId, 0)
			if observed == nil || observed.MessageBytes != nil || pooled != (size == 8192) ||
				delivered.Load() != 1 || !f.hasAck(f.sequenceId, messageId, false) ||
				sequence == nil || sequence.ctx.Err() != nil || sequence.nextSequenceNumber != 1 ||
				sequence.receiveQueue.Len() != 0 || sequence.deliveryQueue != nil {
				t.Fatalf("generic %d-byte pool boundary changed actual callback/ACK/owner custody", size)
			}
		}
	})
}

// Keep the exact pooled negative oracle: an unreleased owner makes the
// witness return false, and releasing that last owner returns the root.
func TestReceiveReceiptWitnessPooledFinalReturnRequiresLastOwner(t *testing.T) {
	assertMessagePoolOwnership(t)
	owner := MessagePoolGet(8192)
	pooled, _ := MessagePoolCheck(owner)
	witness := MessagePoolShareReadOnly(owner)
	returnedWitness := MessagePoolReturn(witness)
	returnedOwner := MessagePoolReturn(owner)
	if !pooled || returnedWitness || !returnedOwner {
		t.Fatal("pooled final-share oracle failed to distinguish a retained owner")
	}
}

// An unpooled root cannot use the pool's return boolean as a refcount.
// Its separate oracle must reject a nonempty frame until actual detachment.
func TestReceiveReceiptWitnessUnpooledFrameMustDetach(t *testing.T) {
	assertMessagePoolOwnership(t)
	frame, err := ToFrame(&protocol.SimpleMessage{Content: strings.Repeat("g", 8190)}, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	defer returnDecodedFrameMessageBytes(frame)
	if pooled, _ := MessagePoolCheck(frame.MessageBytes); pooled || len(frame.MessageBytes) != 8193 {
		t.Fatal("unpooled negative control did not cross the real last pool class")
	}
	frames := []*protocol.Frame{frame}
	if err := receiptAdmissionGenericOwnersError(frames, nil); err == nil ||
		!strings.Contains(err.Error(), "retains its receive-owned message bytes") {
		t.Fatal("unpooled closure oracle accepted a still-referenced frame")
	}
	returnDecodedFrameMessageBytes(frame)
	if err := receiptAdmissionGenericOwnersError(frames, nil); err != nil {
		t.Fatal(err)
	}
}

// The oracle itself must reject Run-only exit. This synthetic lifecycle value
// tests the observer; real owner coverage remains in constructor tests below.
func TestReceiveReceiptWitnessRequiresFullSequenceJoin(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sequence := &ReceiveSequence{ctx: ctx, exit: make(chan struct{}), done: make(chan struct{})}
	close(sequence.exit)
	sequences := map[*ReceiveSequence]struct{}{sequence: {}}
	if err := receiptAdmissionGenericOwnersError(nil, sequences); err == nil ||
		!strings.Contains(err.Error(), "owned workers have not joined") {
		t.Fatal("generic closure oracle accepted Run-only exit without the owner join")
	}
	close(sequence.done)
	if err := receiptAdmissionGenericOwnersError(nil, sequences); err != nil {
		t.Fatal(err)
	}
}

// A real selectively held unpooled item must detach on explicit cancellation,
// without a missing predecessor, callback delivery, cumulative ACK or GC wait.
func TestReceiveReceiptAdmissionUnpooledHeldCancelClearsOwners(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newReceiptAdmissionFixtureWithProvider(t, 1, false, func(settings *ReceiveBufferSettings) {
			settings.ReceiveQueueMaxByteCount = 16 * 1024
			settings.ReceiveHoldPolicy = ReceiveHoldRefuse
		})
		delivered := f.observeGeneric()
		messageId := NewId()
		f.offerGeneric(1, messageId, 12*1024)
		sequence := f.sequence(f.sequenceId, 0)
		if sequence == nil {
			t.Fatal("actual unpooled held sequence was not constructed")
		}
		held := sequence.receiveQueue.GetByMessageId(messageId)
		if held == nil || len(held.frames) != 1 || !held.committed || held.memoryBudget != f.receiveBudget ||
			!f.hasAck(f.sequenceId, messageId, true) || f.hasAck(f.sequenceId, messageId, false) ||
			delivered.Load() != 0 || sequence.deliveryQueue != nil {
			t.Fatal("actual unpooled selective owner was not established")
		}
		frame := held.frames[0]
		if pooled, _ := MessagePoolCheck(frame.MessageBytes); pooled || len(frame.MessageBytes) != 12*1024 {
			t.Fatal("held cancellation control did not retain an unpooled root")
		}
		f.cancel()
		if err := f.nat.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := f.client.CloseAndWait(context.Background()); err != nil {
			t.Fatal(err)
		}
		f.collectAcks()
		if held.frames != nil || held.memoryBudget != nil || delivered.Load() != 0 ||
			f.hasAck(f.sequenceId, messageId, false) || f.receiveBudget.UsedByteCount() != 0 {
			t.Fatal("unpooled held cancellation retained memory or invented delivery")
		}
		if err := receiptAdmissionGenericOwnersError([]*protocol.Frame{frame},
			map[*ReceiveSequence]struct{}{sequence: {}}); err != nil {
			t.Fatal(err)
		}
	})
}
