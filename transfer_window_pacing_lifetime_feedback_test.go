// Lifetime expiry must account for validated feedback received while the send
// owner is pacing, without dispatching an expired write or consuming ACKs twice.
package connect

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The first record expires at 500 ms; a younger original is paced until 550 ms
// and expires at 600 ms. Feedback at 400 ms protects only the first record.
func runWindowPacingLifetimeFeedback(t *testing.T, selective bool) {
	t.Helper()
	runWindowRetryClockFixture(t, 4*time.Second, 500*time.Millisecond, func(t *testing.T, fixture *windowRetryClockFixture) {
		older := fixture.sequence.resendQueue.PeekFirst()
		olderId := older.messageId
		time.Sleep(100 * time.Millisecond)
		service := fixture.sequence.windowPacer.service
		service.stateLock.Lock()
		service.next = fixture.start.Add(550 * time.Millisecond)
		service.stateLock.Unlock()
		frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(1280)}
		clear(frame.MessageBytes)
		if !fixture.client.SendWithTimeout(frame, fixture.sequence.destination, func(error) {}, time.Second) {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatal("younger message was not admitted")
		}
		close(fixture.releaseInitial)
		synctest.Wait()
		time.Sleep(300 * time.Millisecond)
		ok, err := fixture.sequence.ackMessage(receiveAckMessage{
			sequenceId: fixture.sequence.sequenceId, messageId: olderId, selective: selective,
		}, 0)
		if !ok || err != nil {
			t.Fatalf("forced older feedback was refused: %v", err)
		}
		synctest.Wait()
		if !fixture.sequence.ackWindow.PendingDispositionFor(older.sequenceNumber, olderId) {
			t.Fatal("feedback must be coalesced while the sender remains paced")
		}
		time.Sleep(100*time.Millisecond + time.Nanosecond)
		synctest.Wait()
		if fixture.sequence.ctx.Err() != nil || len(fixture.ackFailedAt) != 0 || len(fixture.route) != 0 {
			t.Fatal("validated feedback failed to preserve the live younger pacing wait")
		}
		fixture.sequence.resendQueue.stateLock.Lock()
		originalSendTime := older.sendTime
		fixture.sequence.resendQueue.stateLock.Unlock()
		if originalSendTime != fixture.start {
			t.Fatal("pending lifetime renewal rewrote the original RTT tag timestamp")
		}
		time.Sleep(50*time.Millisecond - time.Nanosecond)
		synctest.Wait()
		select {
		case bytes := <-fixture.route:
			pack := decodeSendPackLifecycleWirePack(t, bytes)
			MessagePoolReturn(bytes)
			if pack.SequenceNumber != 1 {
				t.Fatal("feedback rescheduled recovery instead of preserving the younger original")
			}
		default:
			t.Fatal("feedback postponed the original physical dispatch")
		}
		if len(fixture.route) != 0 || len(fixture.ackFailedAt) != 0 {
			t.Fatal("expiry handling duplicated a write or failed delivered data")
		}
	})
}

// A cumulative head received before expiry already covers the older lifetime.
func TestWindowPacingLifetimePendingHeadPreservesYoungerWrite(t *testing.T) {
	runWindowPacingLifetimeFeedback(t, false)
}

// A SACK preserves its ordinary extended lifetime without acknowledging a hole.
func TestWindowPacingLifetimePendingSackPreservesYoungerWrite(t *testing.T) {
	runWindowPacingLifetimeFeedback(t, true)
}

// A delayed handoff after admission cannot turn local permission into a physical
// dispatch after expiry. Cleanup must still release the reserved service state.
func TestWindowPacingLifetimeLateAdmissionCannotDispatch(t *testing.T) {
	runWindowInitialLifetimeFixture(t, 400*time.Millisecond, 500*time.Millisecond, false, func(t *testing.T, fixture *windowInitialLifetimeFixture) {
		time.Sleep(600 * time.Millisecond)
		synctest.Wait()
		if len(fixture.route) != 0 || len(fixture.written) != 0 {
			t.Fatal("late admitted handoff physically dispatched an expired original")
		}
		select {
		case <-fixture.failed:
		default:
			t.Fatal("late admitted handoff retained the expired owner")
		}
		service := fixture.sequence.windowPacer.service
		service.stateLock.Lock()
		reserved := service.pacingReservations
		service.stateLock.Unlock()
		if reserved != 0 {
			t.Fatal("late expiry retained a service reservation")
		}
	}, func(fixture *windowInitialLifetimeFixture) {
		fixture.sequence.windowPacer.afterAdmissionForTest = func() { time.Sleep(200 * time.Millisecond) }
	})
}

// A pending retry still owns the first copy's ACK identity. Its reply must
// cancel local waiting before a duplicate write, including at a short lifetime.
func TestWindowPacingLifetimeRetryWaitKeepsAckIdentity(t *testing.T) {
	runWindowRetryClockFixture(t, 4*time.Second, 500*time.Millisecond, func(t *testing.T, fixture *windowRetryClockFixture) {
		messageId := fixture.sequence.resendQueue.PeekFirst().messageId
		fixture.delayRetry(1200 * time.Millisecond)
		time.Sleep(400 * time.Millisecond)
		ok, err := fixture.sequence.ackMessage(receiveAckMessage{
			sequenceId: fixture.sequence.sequenceId, messageId: messageId,
		}, 0)
		if !ok || err != nil {
			t.Fatalf("original reply was refused: %v", err)
		}
		synctest.Wait()
		if fixture.sequence.resendQueue.GetByMessageId(messageId) != nil || len(fixture.sequence.sendItems) != 0 {
			t.Fatal("ACK during retry pacing did not retire the original")
		}
		if len(fixture.route) != 0 || len(fixture.ackFailedAt) != 0 || fixture.sequence.ctx.Err() != nil {
			t.Fatal("acknowledged retry wrote a duplicate or failed its owner")
		}
		time.Sleep(900 * time.Millisecond)
		synctest.Wait()
		if len(fixture.route) != 0 || len(fixture.ackFailedAt) != 0 {
			t.Fatal("canceled retry retained a later physical write or expired lifetime")
		}
	})
}

// The recovery heap can put a younger record first while an older lifetime
// expires sooner. Idle waiting must still visit the older absolute deadline.
func TestWindowPacingLifetimeIdleWaitKeepsOlderDeadline(t *testing.T) {
	runWindowRetryClockFixture(t, 4*time.Second, 500*time.Millisecond, func(t *testing.T, fixture *windowRetryClockFixture) {
		older := fixture.sequence.resendQueue.PeekFirst()
		fixture.sequence.setResendTime(older, fixture.start.Add(2*time.Second))
		time.Sleep(100 * time.Millisecond)
		frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(1280)}
		clear(frame.MessageBytes)
		if !fixture.client.SendWithTimeout(frame, fixture.sequence.destination, func(error) {}, time.Second) {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatal("younger message was not admitted")
		}
		close(fixture.releaseInitial)
		synctest.Wait()
		if len(fixture.route) != 1 {
			t.Fatal("younger original was not physically written")
		}
		MessagePoolReturn(<-fixture.route)
		time.Sleep(300 * time.Millisecond)
		synctest.Wait()
		if len(fixture.route) != 1 {
			t.Fatal("younger retry did not precede the older lifetime")
		}
		MessagePoolReturn(<-fixture.route)
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		select {
		case at := <-fixture.ackFailedAt:
			if at != fixture.start.Add(500*time.Millisecond) {
				t.Fatal("recovery ordering extended the older lifetime")
			}
		default:
			t.Fatal("younger recovery timer hid an older ACK lifetime")
		}
	})
}

// Expiry wins a tie with pacing permission before any dispatch-delay hook runs.
func TestWindowPacingLifetimeEqualPacingDeadlineExpiresFirst(t *testing.T) {
	runWindowInitialLifetimeFixture(t, 500*time.Millisecond, 500*time.Millisecond, false, func(t *testing.T, fixture *windowInitialLifetimeFixture) {
		time.Sleep(500 * time.Millisecond)
		synctest.Wait()
		select {
		case at := <-fixture.failed:
			if at != fixture.start.Add(500*time.Millisecond) {
				t.Fatal("equal pacing deadline postponed ACK expiry")
			}
		default:
			t.Fatal("pacing permission overtook expiry at the same timestamp")
		}
		if len(fixture.route) != 0 || len(fixture.written) != 0 {
			t.Fatal("equal pacing deadline physically dispatched expired data")
		}
	}, func(fixture *windowInitialLifetimeFixture) {
		fixture.sequence.windowPacer.afterWaitForTest = func() { time.Sleep(200 * time.Millisecond) }
	})
}

// Finishing pacing cannot hide expiry in the following bounded route write.
// The route is explicitly full before the real worker is released.
func TestWindowPacingLifetimeFullRouteKeepsOriginalDeadline(t *testing.T) {
	runWindowInitialLifetimeFixture(t, 400*time.Millisecond, 500*time.Millisecond, false, func(t *testing.T, fixture *windowInitialLifetimeFixture) {
		time.Sleep(500 * time.Millisecond)
		synctest.Wait()
		select {
		case at := <-fixture.failed:
			if at != fixture.start.Add(500*time.Millisecond) {
				t.Fatal("route backpressure extended the original lifetime")
			}
		default:
			t.Fatal("route write hid expiry after pacing completed")
		}
		if len(fixture.route) != cap(fixture.route) || len(fixture.written) != 0 {
			t.Fatal("expired route write displaced or dispatched data")
		}
	}, func(fixture *windowInitialLifetimeFixture) {
		fixture.sequence.sendBufferSettings.WriteTimeout = time.Second
		for range cap(fixture.route) {
			bytes := MessagePoolGet(16)
			clear(bytes)
			fixture.route <- bytes
		}
	})
}

// A slot released before expiry still admits the original exactly once.
func TestWindowPacingLifetimeRouteRecoveryBeforeExpiry(t *testing.T) {
	runWindowInitialLifetimeFixture(t, 400*time.Millisecond, 500*time.Millisecond, false, func(t *testing.T, fixture *windowInitialLifetimeFixture) {
		time.Sleep(450 * time.Millisecond)
		MessagePoolReturn(<-fixture.route)
		synctest.Wait()
		select {
		case boundary := <-fixture.written:
			if boundary.at != fixture.start.Add(450*time.Millisecond) {
				t.Fatal("route recovery postponed the successful write")
			}
			close(fixture.release)
		default:
			t.Fatal("a route slot before lifetime expiry did not admit the original")
		}
		if len(fixture.route) != cap(fixture.route) || len(fixture.failed) != 0 {
			t.Fatal("route recovery duplicated or failed the initial write")
		}
		time.Sleep(50 * time.Millisecond)
		synctest.Wait()
		select {
		case at := <-fixture.failed:
			if at != fixture.start.Add(500*time.Millisecond) {
				t.Fatal("successful route admission renewed the original lifetime")
			}
		default:
			t.Fatal("route recovery hid the subsequent unacknowledged lifetime")
		}
	}, func(fixture *windowInitialLifetimeFixture) {
		fixture.sequence.sendBufferSettings.WriteTimeout = time.Second
		for range cap(fixture.route) {
			bytes := MessagePoolGet(16)
			clear(bytes)
			fixture.route <- bytes
		}
	})
}

// An older head ACK removes an intermediate lifetime wake. The retained newer
// write keeps the remaining original route budget, without gaining a new one.
func TestWindowPacingLifetimeRouteWakePreservesWriterBudget(t *testing.T) {
	runWindowRetryClockFixture(t, 4*time.Second, 500*time.Millisecond, func(t *testing.T, fixture *windowRetryClockFixture) {
		older := fixture.sequence.resendQueue.PeekFirst()
		olderId := older.messageId
		// Force the younger original, not an already-due older recovery, to
		// own the route wait beginning at 400 ms. Its lifetime stays 500 ms.
		fixture.sequence.setResendTime(older, fixture.start.Add(2*time.Second))
		finished := make(chan time.Time, 1)
		fixture.sequence.sendBuffer.afterInitialWriteQueuedForTest = func(id sendSequenceId, number uint64) {
			if id.Destination == fixture.sequence.destination && number == 1 {
				finished <- time.Now()
				<-fixture.client.Ctx().Done()
			}
		}
		fixture.sequence.sendBufferSettings.WriteTimeout = 350 * time.Millisecond
		time.Sleep(400 * time.Millisecond)
		for range cap(fixture.route) {
			bytes := MessagePoolGet(16)
			clear(bytes)
			fixture.route <- bytes
		}
		service := fixture.sequence.windowPacer.service
		service.stateLock.Lock()
		service.next = fixture.start.Add(400 * time.Millisecond)
		service.stateLock.Unlock()
		frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(1280)}
		clear(frame.MessageBytes)
		if !fixture.client.SendWithTimeout(frame, fixture.sequence.destination, func(error) {}, time.Second, sendPackRecoveryOption{retainAfterAckTimeout: true}) {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatal("retained younger message was not admitted")
		}
		close(fixture.releaseInitial)
		synctest.Wait()
		time.Sleep(50 * time.Millisecond)
		if ok, err := fixture.sequence.ackMessage(receiveAckMessage{sequenceId: fixture.sequence.sequenceId, messageId: olderId}, 0); !ok || err != nil {
			t.Fatalf("older route-wait feedback was refused: %v", err)
		}
		synctest.Wait()
		time.Sleep(300*time.Millisecond - time.Nanosecond)
		synctest.Wait()
		if len(finished) != 0 || fixture.sequence.ctx.Err() != nil {
			t.Fatal("acknowledged older expiry shortened the retained writer's budget")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		select {
		case at := <-finished:
			if at != fixture.start.Add(750*time.Millisecond) {
				t.Fatal("intermediate lifetime wake renewed the route-write timeout")
			}
		default:
			t.Fatal("writer exceeded its original 350 ms route budget")
		}
		if len(fixture.route) != cap(fixture.route) || len(fixture.ackFailedAt) != 0 {
			t.Fatal("route-wait accounting dispatched or failed acknowledged bytes")
		}
	})
}
