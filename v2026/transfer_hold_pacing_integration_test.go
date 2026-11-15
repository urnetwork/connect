package connect

import (
	"bytes"
	"math"
	"testing"
	"testing/synctest"
	"time"
)

// Signature A emits a selective promise before delivery. The later cumulative
// ACK must still carry the real receive wait into the shared pacing service.
// Both ACKs come from the live receiver and pass the live sender's decoder.
func TestReceiveHoldSelectivePromisePreservesPacingReceiverHold(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture, _, _ := newAckRetirementFixture(t, TransportTypeH1, 2, NewNoopLogger(), func(settings *ClientSettings) {
			settings.SendBufferSettings.WindowSizing = WindowSizingFromDelivery
			settings.SendBufferSettings.ApplyWindowSizing()
			// This row tests the ACK evidence boundary; existing pacing rows
			// drive the service's reservation and rate decisions separately.
			settings.SendBufferSettings.disableWindowPacingForTest = true
			settings.ReceiveBufferSettings.ReceiveHoldPolicy = ReceiveHoldCommittedPrefix
			settings.ReceiveBufferSettings.ReceiveQueueBudget = NewTransferMemoryBudget(ByteCount(math.MaxInt32))
			settings.ReceiveBufferSettings.ReceiveQueueRetainedByteAccounting = true
		})
		first := fixture.write(receiveHoldPromisePayloadByteCount)
		time.Sleep(receiverHoldTestPath / 2)
		ack := fixture.receive(first)
		time.Sleep(receiverHoldTestPath / 2)
		fixture.forward(ack, fixture.senderIn)
		sequence := fixture.sequence()
		service := sequence.windowPacer.service
		if service == nil {
			t.Fatal("sender has no shared pacing service")
		}
		sibling := newReceiverHoldTestLane(t, service)
		t.Cleanup(sibling.sequence.windowPacer.close)
		sibling.write(t, 1)
		sibling.ack(1, false, receiverHoldTestCompression)
		// Keep a third lane in flight: an empty-flight probe is allowed to
		// confirm a new path baseline and would not prove carrier queuing.
		busy := newReceiverHoldTestLane(t, service)
		t.Cleanup(busy.sequence.windowPacer.close)
		busy.write(t, 1)
		busy.ack(1, false, receiverHoldTestCompression)
		busy.write(t, 1)
		gap := fixture.write(receiveHoldPromisePayloadByteCount)
		held := fixture.write(receiveHoldPromisePayloadByteCount)
		time.Sleep(receiverHoldTestPath / 2)
		selective := fixture.receive(held)
		if !selective.ack.Selective || !bytes.Equal(selective.ack.MessageId, held.pack.MessageId) || fixture.deliveredCount != 1 {
			t.Fatal("fixture did not promise the held frame before delivery")
		}
		time.Sleep(receiverHoldTestPath / 2)
		fixture.forward(selective, fixture.senderIn)
		if sequence.windowPacer.receiverHeld {
			t.Fatal("a selective promise was mistaken for a cumulative receiver hold")
		}
		time.Sleep(100 * time.Millisecond)
		cumulative := fixture.receive(gap)
		if cumulative.ack.Selective || !bytes.Equal(cumulative.ack.MessageId, held.pack.MessageId) ||
			cumulative.ack.GetReceiverAckDelayMicros() < uint32(100*time.Millisecond/time.Microsecond) {
			t.Fatalf("delivery lost the previously promised head's hold evidence: %+v", cumulative.ack)
		}
		time.Sleep(receiverHoldTestPath / 2)
		fixture.forward(cumulative, fixture.senderIn)
		if !sequence.windowPacer.receiverHeld {
			t.Fatal("the earlier selective promise hid the cumulative receiver hold from pacing")
		}
		// A later sibling sample with a real carrier queue must remain queue
		// evidence even though the just-delivered lane is receiver-held.
		sibling.write(t, 1)
		sibling.ackAfterQueue(2, false, time.Millisecond, 20*time.Millisecond)
		service.stateLock.Lock()
		unqueued := service.unqueuedWithLock(service.roundTripEvidenceWithLock(time.Now()))
		service.stateLock.Unlock()
		if unqueued {
			t.Fatal("a committed receive hold hid the sibling's carrier queue")
		}
	})
}
