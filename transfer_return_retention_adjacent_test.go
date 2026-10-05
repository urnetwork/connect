// Mixed return ownership survives expiration at the worker's local write and
// ingress boundaries, including when the surviving bytes were never written.
package connect

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The disposable original expires while the retained successor is inside the
// real shared pacer. The write must unwind before its immutable envelope is
// promoted to Head, and its terminal owner must remain live throughout.
func TestReturnRetentionOlderExpiryPreservesPacedSuccessor(t *testing.T) {
	runWindowRetryClockFixture(t, 4*time.Second, 500*time.Millisecond, func(t *testing.T, fixture *windowRetryClockFixture) {
		older := fixture.sequence.resendQueue.PeekFirst()
		fixture.sequence.setResendTime(older, fixture.start.Add(2*time.Second))
		time.Sleep(100 * time.Millisecond)
		service := fixture.sequence.windowPacer.service
		service.stateLock.Lock()
		service.next = fixture.start.Add(800 * time.Millisecond)
		service.stateLock.Unlock()
		completed := returnRetentionAdjacentEnqueue(t, fixture, "retained paced successor")
		close(fixture.releaseInitial)
		synctest.Wait()
		younger := fixture.sequence.resendQueue.GetBySequenceNumber(1)
		if younger == nil || younger.transportWriteObserved || len(fixture.route) != 0 {
			t.Fatal("fixture did not retain the younger original before its paced physical write")
		}
		messageID := younger.messageId
		time.Sleep(400 * time.Millisecond)
		synctest.Wait()
		returnRetentionAdjacentRequireLive(t, fixture, completed)
		returnRetentionAdjacentRequireExpired(t, fixture)
		returnRetentionAdjacentDeliver(t, fixture, completed, messageID, "retained paced successor")
	})
}

// This expiry happens after pacing has admitted a write but while the actual
// route is full. Releasing route capacity must write the retained successor's
// repaired Head, with its original payload and no second terminal callback.
func TestReturnRetentionOlderExpiryPreservesBlockedRouteSuccessor(t *testing.T) {
	runWindowRetryClockFixture(t, 4*time.Second, 500*time.Millisecond, func(t *testing.T, fixture *windowRetryClockFixture) {
		older := fixture.sequence.resendQueue.PeekFirst()
		fixture.sequence.setResendTime(older, fixture.start.Add(2*time.Second))
		fixture.sequence.sendBufferSettings.WriteTimeout = 350 * time.Millisecond
		time.Sleep(400 * time.Millisecond)
		for range cap(fixture.route) {
			marker := MessagePoolGet(1)
			marker[0] = 0xa5
			fixture.route <- marker
		}
		completed := returnRetentionAdjacentEnqueue(t, fixture, "retained blocked-route successor")
		close(fixture.releaseInitial)
		synctest.Wait()
		younger := fixture.sequence.resendQueue.GetBySequenceNumber(1)
		if younger == nil || younger.transportWriteObserved || len(fixture.route) != cap(fixture.route) {
			t.Fatal("fixture did not enter the younger original's full-route write")
		}
		messageID := younger.messageId
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		returnRetentionAdjacentRequireLive(t, fixture, completed)
		returnRetentionAdjacentRequireExpired(t, fixture)
		time.Sleep(50 * time.Millisecond)
		synctest.Wait()
		for range cap(fixture.route) {
			marker := <-fixture.route
			valid := len(marker) == 1 && marker[0] == 0xa5
			MessagePoolReturn(marker)
			if !valid {
				t.Fatal("blocked write displaced an owned route marker")
			}
		}
		returnRetentionAdjacentDeliver(t, fixture, completed, messageID, "retained blocked-route successor")
	})
}

// Admission has already transferred the only copy even when the send worker
// has not materialized a resend item. Expiry cannot drain a retained Pack that
// is waiting in the ordinary ingress channel behind the expired original.
func TestReturnRetentionOlderExpiryPreservesQueuedSuccessor(t *testing.T) {
	runWindowRetryClockFixture(t, 4*time.Second, 500*time.Millisecond, func(t *testing.T, fixture *windowRetryClockFixture) {
		time.Sleep(400 * time.Millisecond)
		completed := returnRetentionAdjacentEnqueue(t, fixture, "retained queued successor")
		if fixture.sequence.resendQueue.GetBySequenceNumber(1) != nil || len(fixture.sequence.packs) != 1 {
			t.Fatal("fixture did not leave the admitted retained Pack in ingress")
		}
		time.Sleep(100 * time.Millisecond)
		close(fixture.releaseInitial)
		synctest.Wait()
		returnRetentionAdjacentRequireLive(t, fixture, completed)
		returnRetentionAdjacentRequireExpired(t, fixture)
		younger := fixture.sequence.resendQueue.GetBySequenceNumber(1)
		if younger == nil {
			t.Fatal("expired predecessor prevented retained ingress from reaching its send owner")
		}
		returnRetentionAdjacentDeliver(t, fixture, completed, younger.messageId, "retained queued successor")
	})
}

func returnRetentionAdjacentEnqueue(t *testing.T, fixture *windowRetryClockFixture, content string) chan error {
	t.Helper()
	completed := make(chan error, 4)
	frame := RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: content})
	admitted, err := fixture.client.SendWithTimeoutDetailed(frame, fixture.sequence.destination, func(err error) {
		completed <- err
	}, time.Second, sendPackRecoveryOption{upstreamRecoverable: true, retainAfterAckTimeout: true})
	if !admitted || err != nil {
		MessagePoolReturn(frame.MessageBytes)
		t.Fatalf("retained successor admission=%t error=%v", admitted, err)
	}
	return completed
}

func returnRetentionAdjacentRequireLive(t *testing.T, fixture *windowRetryClockFixture, completed <-chan error) {
	t.Helper()
	if fixture.sequence.ctx.Err() != nil || len(completed) != 0 {
		t.Fatal("disposable predecessor expiry failed the retained successor owner")
	}
}

func returnRetentionAdjacentRequireExpired(t *testing.T, fixture *windowRetryClockFixture) {
	t.Helper()
	select {
	case at := <-fixture.ackFailedAt:
		if at != fixture.start.Add(500*time.Millisecond) {
			t.Fatalf("disposable predecessor expired at %s, want 500ms", at.Sub(fixture.start))
		}
	default:
		t.Fatal("retained successor hid the disposable predecessor's ACK deadline")
	}
}

// The clock only permits bounded recovery to run; the decoded physical frame
// supplies the identity/payload evidence, and its real ACK handoff completes
// the exact owner. No unrelated sequence or manufactured delivery credit is
// allowed to pay for the discarded predecessor.
func returnRetentionAdjacentDeliver(t *testing.T, fixture *windowRetryClockFixture, completed <-chan error, messageID Id, content string) {
	t.Helper()
	synctest.Wait()
	for len(fixture.route) == 0 && time.Now().Before(fixture.start.Add(3*time.Second)) {
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		returnRetentionAdjacentRequireLive(t, fixture, completed)
	}
	select {
	case wire := <-fixture.route:
		pack := decodeSendPackLifecycleWirePack(t, wire)
		MessagePoolReturn(wire)
		if !pack.Head || pack.SequenceNumber != 1 || RequireIdFromBytes(pack.MessageId) != messageID ||
			RequireIdFromBytes(pack.SequenceId) != fixture.sequence.sequenceId || len(pack.Frames) != 1 {
			t.Fatal("retained successor lost its original identity or repaired Head at physical dispatch")
		}
		var payload protocol.SimpleMessage
		if err := ProtoUnmarshal(pack.Frames[0].MessageBytes, &payload); err != nil || payload.Content != content {
			t.Fatalf("retained payload changed during expiration/rewrite: content=%q error=%v", payload.Content, err)
		}
	default:
		t.Fatal("retained successor never reached an available physical route")
	}
	if ok, err := fixture.sequence.ackMessage(receiveAckMessage{
		sequenceId: fixture.sequence.sequenceId, messageId: messageID,
	}, 0); !ok || err != nil {
		t.Fatalf("retained successor ACK was refused: admitted=%t error=%v", ok, err)
	}
	synctest.Wait()
	select {
	case err := <-completed:
		if err != nil {
			t.Fatalf("retained successor failed after its physical delivery: %v", err)
		}
	default:
		t.Fatal("retained successor did not complete after its delivery ACK")
	}
	if fixture.sequence.ctx.Err() != nil || len(completed) != 0 || len(fixture.ackFailedAt) != 0 ||
		fixture.sequence.resendQueue.Len() != 0 || len(fixture.sequence.sendItems) != 0 || len(fixture.sequence.ackLifetimes.items) != 0 {
		t.Fatal("mixed retirement duplicated completion or retained a terminal send owner")
	}
	service := fixture.sequence.windowPacer.service
	service.stateLock.Lock()
	reservations := service.pacingReservations
	outstanding := service.sent - service.total
	owned := fixture.sequence.windowPacer.serviceSent - fixture.sequence.windowPacer.serviceAcked
	service.stateLock.Unlock()
	if reservations != 0 || outstanding != 0 || owned != 0 {
		t.Fatalf("retirement left shared pacing ownership: reservations=%d outstanding=%d owned=%d", reservations, outstanding, owned)
	}
}
