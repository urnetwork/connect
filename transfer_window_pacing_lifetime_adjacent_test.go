// Feedback that arrives during a paced write still owns its final delivery
// result when a different record expires before the worker can take a snapshot.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Expiring a younger paced original cannot turn the already delivered prefix
// into a failed application callback during sequence cleanup.
func TestWindowPacingLifetimeAcknowledgedPrefixSurvivesYoungerExpiry(t *testing.T) {
	runWindowRetryClockFixture(t, 4*time.Second, 500*time.Millisecond, func(t *testing.T, fixture *windowRetryClockFixture) {
		olderId := fixture.sequence.resendQueue.PeekFirst().messageId
		time.Sleep(100 * time.Millisecond)
		service := fixture.sequence.windowPacer.service
		service.stateLock.Lock()
		service.next = fixture.start.Add(800 * time.Millisecond)
		service.stateLock.Unlock()
		youngerFailedAt := make(chan time.Time, 1)
		frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(1280)}
		clear(frame.MessageBytes)
		if !fixture.client.SendWithTimeout(frame, fixture.sequence.destination, func(err error) {
			if err != nil {
				youngerFailedAt <- time.Now()
			}
		}, time.Second) {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatal("younger message was not admitted")
		}
		close(fixture.releaseInitial)
		synctest.Wait()
		time.Sleep(300 * time.Millisecond)
		if ok, err := fixture.sequence.ackMessage(receiveAckMessage{sequenceId: fixture.sequence.sequenceId, messageId: olderId}, 0); !ok || err != nil {
			t.Fatalf("older delivery reply was refused: %v", err)
		}
		synctest.Wait()
		if !fixture.sequence.ackWindow.PendingDispositionFor(0, olderId) {
			t.Fatal("older delivery was not published before lifetime expiry")
		}
		time.Sleep(200 * time.Millisecond)
		synctest.Wait()
		select {
		case at := <-youngerFailedAt:
			if at != fixture.start.Add(600*time.Millisecond) {
				t.Fatal("younger original did not expire at its own lifetime")
			}
		default:
			t.Fatal("younger original outlived its original lifetime")
		}
		if len(fixture.route) != 0 || fixture.sequence.ctx.Err() == nil {
			t.Fatal("younger expiry dispatched data or left its sequence open")
		}
		select {
		case at := <-fixture.ackFailedAt:
			t.Fatalf("already delivered prefix failed during younger cleanup at %s", at.Sub(fixture.start))
		default:
		}
	})
}

// Hold a real retry after pacing admission and order feedback on either side
// of the 500 ms lifetime before releasing its worker at 600 ms. The existing
// sender applies received delivery before checking expiration on resumption.
func runWindowPacingLifetimeDelayedRecoveryAck(t *testing.T, arrival time.Duration, selective bool) {
	t.Helper()
	runWindowRetryClockFixture(t, 4*time.Second, 500*time.Millisecond, func(t *testing.T, fixture *windowRetryClockFixture) {
		messageId := fixture.sequence.resendQueue.PeekFirst().messageId
		admitted, release := make(chan struct{}), make(chan struct{})
		fixture.sequence.windowPacer.afterAdmissionForTest = func() {
			close(admitted)
			select {
			case <-release:
			case <-fixture.client.Ctx().Done():
			}
		}
		fixture.delayRetry(400 * time.Millisecond)
		time.Sleep(400 * time.Millisecond)
		<-admitted
		time.Sleep(arrival - 400*time.Millisecond)
		if ok, err := fixture.sequence.ackMessage(receiveAckMessage{sequenceId: fixture.sequence.sequenceId, messageId: messageId, selective: selective}, 0); !ok || err != nil {
			t.Fatalf("held retry reply was refused: %v", err)
		}
		synctest.Wait()
		if !fixture.sequence.ackWindow.PendingDispositionFor(0, messageId) {
			t.Fatal("reply was not published while retry admission was held")
		}
		time.Sleep(600*time.Millisecond - arrival)
		close(release)
		synctest.Wait()
		if len(fixture.route) != 0 {
			t.Fatal("reply to the first copy failed to suppress an unissued retry")
		}
		if len(fixture.ackFailedAt) != 0 || fixture.sequence.ctx.Err() != nil {
			t.Fatal("already received reply lost its delivery result before the expiry check")
		}
		item := fixture.sequence.resendQueue.GetByMessageId(messageId)
		if selective {
			if item == nil {
				t.Fatal("selective reply prematurely released its recovery record")
			}
			// The existing cumulative probe may shorten recovery to one RTT;
			// this control concerns the retained identity and lifetime instead.
			if !item.selectiveAcked || item.sendTime.Add(item.ackTimeout) != fixture.start.Add(1100*time.Millisecond) {
				t.Fatal("selective reply lost its ordinary retained lifetime")
			}
		} else if item != nil || len(fixture.sequence.sendItems) != 0 {
			t.Fatal("cumulative delivery retained an acknowledged lifetime")
		}
	})
}

// A head received before expiry remains valid despite delayed consumption.
func TestWindowPacingLifetimeTimelyHeadSurvivesDelayedAdmission(t *testing.T) {
	runWindowPacingLifetimeDelayedRecoveryAck(t, 450*time.Millisecond, false)
}

// A selective reply received before expiry preserves its recovery window.
func TestWindowPacingLifetimeTimelySackSurvivesDelayedAdmission(t *testing.T) {
	runWindowPacingLifetimeDelayedRecoveryAck(t, 450*time.Millisecond, true)
}

// A paused worker accepts already received delivery before its expiry check,
// even after the nominal deadline, and must suppress the pending duplicate.
func TestWindowPacingLifetimeLateHeadSuppressesDelayedRetry(t *testing.T) {
	runWindowPacingLifetimeDelayedRecoveryAck(t, 550*time.Millisecond, false)
}

// Selective delivery has the same existing receipt-before-expiry ordering;
// resumption must retain its recovery record without writing another copy.
func TestWindowPacingLifetimeLateSackSuppressesDelayedRetry(t *testing.T) {
	runWindowPacingLifetimeDelayedRecoveryAck(t, 550*time.Millisecond, true)
}

// Cancel after the final pacing reservation is handed off, while the real
// route still has capacity; cancellation must precede physical publication.
func TestWindowPacingLifetimeCancellationAfterAdmissionCannotDispatch(t *testing.T) {
	runWindowInitialLifetimeFixture(t, 400*time.Millisecond, time.Second, false, func(t *testing.T, fixture *windowInitialLifetimeFixture) {
		time.Sleep(400 * time.Millisecond)
		synctest.Wait()
		if fixture.client.Ctx().Err() == nil {
			t.Fatal("admission barrier did not cancel the real sender")
		}
		if len(fixture.route) != 0 || len(fixture.written) != 0 {
			t.Fatal("cancellation after pacing admission still published a physical write")
		}
		service := fixture.sequence.windowPacer.service
		service.stateLock.Lock()
		reservations := service.pacingReservations
		service.stateLock.Unlock()
		if fixture.sequence.resendQueue.Len() != 0 || reservations != 0 {
			t.Fatal("canceled admission retained a recovery record or service reservation")
		}
	}, func(fixture *windowInitialLifetimeFixture) {
		fixture.sequence.windowPacer.afterAdmissionForTest = fixture.cancel
	})
}

// The standalone pacing entry point must return cancellation after admission
// and release its own service state without claiming physical delivery.
func TestWindowPacingLifetimeStandaloneCancellationAfterAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		service := &windowPacingService{}
		sequenceId, messageId := NewId(), NewId()
		pacer := &windowBurstPacer{
			service: service, serviceSequenceId: sequenceId, rate: 1000000,
			afterAdmissionForTest: cancel,
		}
		defer pacer.close()
		start := windowPacingWriteStart{sequenceId: sequenceId, messageId: messageId, number: 0}
		if err := pacer.waitForServiceWriteStarted(ctx, 1000, false, &start); err != context.Canceled {
			t.Errorf("canceled admission granted standalone write permission: %v", err)
		}
		pacer.close()
		if service.pacingReservations != 0 || service.reservedByteCount != 0 || service.pendingWrites != 0 || service.waiterHead != nil || service.waiterTail != nil {
			t.Fatal("canceled standalone admission retained service ownership")
		}
		if service.drained || service.sent != service.total {
			t.Fatal("canceled standalone admission claimed unproved delivery")
		}
	})
}
