// A shared sender must retire every due ACK lifetime even while another write
// waits for physical pacing. These are actual worker roots with virtual time.
package connect

import (
	"context"
	"github.com/urnetwork/connect/protocol"
	"testing"
	"testing/synctest"
	"time"
)

// A real sender waiting behind another service producer still owns a deadline.
// The live FIFO head must survive expiration of the younger reservation.
func TestWindowPacingLifetimeWorkerExpiresBehindFifoHead(t *testing.T) {
	var head *windowBurstPacer
	runWindowInitialLifetimeFixture(t, 100*time.Millisecond, 500*time.Millisecond, false, func(t *testing.T, fixture *windowInitialLifetimeFixture) {
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		service := fixture.sequence.windowPacer.service
		service.stateLock.Lock()
		queued := service.waiterHead == &head.waiter && head.waiter.next == &fixture.sequence.windowPacer.waiter
		service.stateLock.Unlock()
		if !queued {
			t.Fatal("fixture did not park the real sender behind the FIFO head")
		}
		time.Sleep(400 * time.Millisecond)
		synctest.Wait()
		select {
		case at := <-fixture.failed:
			if at != fixture.start.Add(500*time.Millisecond) {
				t.Fatal("FIFO wait changed the original lifetime")
			}
		default:
			t.Fatal("real sender's FIFO wait hid its ACK lifetime")
		}
		service.stateLock.Lock()
		headPreserved := service.waiterHead == &head.waiter && service.waiterTail == &head.waiter
		service.stateLock.Unlock()
		if len(fixture.route) != 0 || !headPreserved {
			t.Fatal("expiration dispatched the message or removed the live FIFO head")
		}
	}, func(fixture *windowInitialLifetimeFixture) {
		ctx := fixture.client.Ctx()
		head = &windowBurstPacer{service: fixture.sequence.windowPacer.service, serviceSequenceId: NewId(), rate: 1000000}
		head.afterWaitForTest = func() { <-ctx.Done() }
		done := make(chan error, 1)
		go func() { done <- head.waitForService(ctx, 1000) }()
		synctest.Wait()
		fixture.cleanup = func() {
			<-done
			head.close()
		}
	})
}

// A controlled service drain cannot borrow time from a waiting message's
// shorter ACK lifetime or turn local expiry into physical delivery proof.
func TestWindowPacingLifetimeWorkerExpiresInsideControlledDrain(t *testing.T) {
	runWindowInitialLifetimeFixture(t, 0, 500*time.Millisecond, false, func(t *testing.T, fixture *windowInitialLifetimeFixture) {
		service := fixture.sequence.windowPacer.service
		service.stateLock.Lock()
		draining := service.drainUntil.After(fixture.start.Add(500*time.Millisecond)) && service.pendingWrites == 1
		service.stateLock.Unlock()
		if !draining {
			t.Fatal("fixture did not enter a drain beyond the waiting message lifetime")
		}
		time.Sleep(500 * time.Millisecond)
		synctest.Wait()
		select {
		case at := <-fixture.failed:
			if at != fixture.start.Add(500*time.Millisecond) {
				t.Fatal("controlled drain extended the message lifetime")
			}
		default:
			t.Fatal("controlled drain hid the real sender's ACK lifetime")
		}
		service.stateLock.Lock()
		unproved := !service.drained && service.pendingWrites == 1
		service.stateLock.Unlock()
		if len(fixture.route) != 0 || !unproved {
			t.Fatal("local expiration dispatched a message or supplied physical proof")
		}
	}, func(fixture *windowInitialLifetimeFixture) {
		service := fixture.sequence.windowPacer.service
		service.drainMaximumTime = time.Minute
		service.sent = 1000
		service.observeRoundTrip(time.Millisecond, 0, fixture.start.Add(-90*time.Millisecond))
		for ago := 8; ago > 0; ago-- {
			service.observeRoundTrip(time.Second, 0, fixture.start.Add(-time.Duration(ago)*10*time.Millisecond))
		}
		sequence, tail := NewId(), NewId()
		service.beginWrite(sequence, tail, 1, fixture.start, false)
		service.finishWrite(sequence, tail, true)
	})
}

// Resuming a nominally timely pacer after the lifetime must recheck expiry
// before the real write, even when the wake itself was delayed.
func TestWindowPacingLifetimeWorkerRejectsLateTimerRelease(t *testing.T) {
	runWindowInitialLifetimeFixture(t, 400*time.Millisecond, 500*time.Millisecond, false, func(t *testing.T, fixture *windowInitialLifetimeFixture) {
		time.Sleep(600 * time.Millisecond)
		synctest.Wait()
		if len(fixture.route) != 0 || len(fixture.written) != 0 {
			t.Fatal("late pacing wake physically wrote an expired message")
		}
		select {
		case <-fixture.failed:
		default:
			t.Fatal("late pacing wake did not retire the expired message")
		}
	}, func(fixture *windowInitialLifetimeFixture) {
		fixture.sequence.windowPacer.afterWaitForTest = func() { time.Sleep(200 * time.Millisecond) }
	})
}

// Cancellation releases the blocked sender without dispatching its message.
func TestWindowPacingLifetimeWorkerPreservesCancellation(t *testing.T) {
	runWindowInitialLifetimeFixture(t, 600*time.Millisecond, 500*time.Millisecond, false, func(t *testing.T, fixture *windowInitialLifetimeFixture) {
		time.Sleep(100 * time.Millisecond)
		fixture.cancel()
		synctest.Wait()
		if fixture.client.Ctx().Err() != context.Canceled || len(fixture.route) != 0 || len(fixture.written) != 0 {
			t.Fatal("canceled pacing wait dispatched or changed cancellation")
		}
		if fixture.sequence.windowPacer.service.pacingReservations != 0 {
			t.Fatal("canceled pacing wait retained a reservation")
		}
	})
}

// A due retry must still retire at the original message lifetime.
func TestWindowPacingLifetimeBoundsRetryWait(t *testing.T) {
	runWindowRetryClockFixture(t, 4*time.Second, 500*time.Millisecond, func(t *testing.T, fixture *windowRetryClockFixture) {
		fixture.delayRetry(1200 * time.Millisecond)
		time.Sleep(500 * time.Millisecond)
		synctest.Wait()
		select {
		case at := <-fixture.ackFailedAt:
			if at != fixture.start.Add(500*time.Millisecond) {
				t.Fatal("paced retry extended the original lifetime")
			}
		default:
			t.Fatal("retry wait hid the lifetime wakeup")
		}
		if len(fixture.route) != 0 || len(fixture.secondDue) != 0 {
			t.Fatal("expired retry physically dispatched")
		}
	})
}

// A younger message waiting for pacing cannot hide an older record deadline.
func TestWindowPacingLifetimeWaitPreservesOlderRecordDeadline(t *testing.T) {
	runWindowRetryClockFixture(t, 4*time.Second, 500*time.Millisecond, func(t *testing.T, fixture *windowRetryClockFixture) {
		time.Sleep(100 * time.Millisecond)
		service := fixture.sequence.windowPacer.service
		service.stateLock.Lock()
		service.next = fixture.start.Add(600 * time.Millisecond)
		service.stateLock.Unlock()
		frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(1280)}
		clear(frame.MessageBytes)
		if !fixture.client.SendWithTimeout(frame, fixture.sequence.destination, func(error) {}, time.Second) {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatal("younger Pack was not admitted")
		}
		close(fixture.releaseInitial)
		synctest.Wait()
		time.Sleep(400 * time.Millisecond)
		synctest.Wait()
		select {
		case at := <-fixture.ackFailedAt:
			if at != fixture.start.Add(500*time.Millisecond) {
				t.Fatal("older record's lifetime changed")
			}
		default:
			t.Fatal("younger pacing wait hid the older record's deadline")
		}
	})
}
