// Pending cumulative feedback keeps finite arrival-based ownership through
// real pacing and retained-item expiry boundaries.
package connect

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Replaying a still-retained head while Run is parked cannot refresh arrival.
func TestAckLifetimePendingPacedDuplicateKeepsArrival(t *testing.T) {
	testAckLifetimePendingPacedHead(t, "duplicate")
}

// A selective receipt for the older item cannot renew the uncovered tail.
func TestAckLifetimePendingPacedSackKeepsTailDeadline(t *testing.T) {
	testAckLifetimePendingPacedHead(t, "selective")
}

// A valid retained message name from another sequence has no renewal authority.
func TestAckLifetimePendingPacedWrongSequenceKeepsDeadline(t *testing.T) {
	testAckLifetimePendingPacedHead(t, "wrong_sequence")
}

// An unknown name in the correct sequence cannot renew any retained owner.
func TestAckLifetimePendingPacedUnknownKeepsDeadline(t *testing.T) {
	testAckLifetimePendingPacedHead(t, "unknown")
}

// Withdrawal metadata from a released carrier schedules recovery, not head progress.
func TestAckLifetimePendingPacedWithdrawalKeepsDeadline(t *testing.T) {
	testAckLifetimePendingPacedHead(t, "withdrawal")
}

// Explicit child cancellation withdraws its renewed wait without closing the client.
func TestAckLifetimePendingPacedCancellationReleasesCustody(t *testing.T) {
	testAckLifetimePendingPacedHead(t, "cancel")
}

// A silent tail expires while pacing still owns it, but the already received
// cumulative prefix must complete successfully during that same owner unwind.
func TestAckLifetimePendingPacedExpiryPreservesDeliveredPrefix(t *testing.T) {
	testAckLifetimePendingPacedHead(t, "expired_wait")
}

// The real worker owns an original at 0 ms and a younger offer at 100 ms.
// Feedback arrives at 400 ms while the younger reservation waits until 800 ms;
// its original 600 ms deadline therefore separates pending from applied progress.
func testAckLifetimePendingPacedHead(t *testing.T, feedback string) {
	t.Helper()
	runWindowRetryClockFixture(t, 4*time.Second, 500*time.Millisecond, func(t *testing.T, fixture *windowRetryClockFixture) {
		sequence := fixture.sequence
		snapshots := observeWindowRetryOwner(fixture)
		resume := make(chan struct{})
		close(resume)
		fixture.resumeSecondDue = resume
		older := sequence.resendQueue.PeekFirst()
		olderId := older.messageId
		prefixCompleted := make(chan error, 2)
		if feedback == "expired_wait" {
			older.acks.add(sendAckRecord{callback: func(err error) { prefixCompleted <- err }})
		}
		time.Sleep(100 * time.Millisecond)
		service := sequence.windowPacer.service
		service.stateLock.Lock()
		service.next = fixture.start.Add(800 * time.Millisecond)
		if feedback == "expired_wait" {
			service.next = fixture.start.Add(1200 * time.Millisecond)
		}
		service.stateLock.Unlock()
		youngerFailedAt := make(chan time.Time, 2)
		frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(1280)}
		clear(frame.MessageBytes)
		if !fixture.client.SendWithTimeout(frame, sequence.destination, func(err error) {
			if err != nil {
				youngerFailedAt <- time.Now()
			}
		}, time.Second) {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatal("younger message was not admitted")
		}
		close(fixture.releaseInitial)
		synctest.Wait()
		before := releaseWindowRetryOwnerWrite(t, snapshots, fixture.start.Add(100*time.Millisecond))
		service.stateLock.Lock()
		waiting := service.pacingReservations == 1 && service.waiterHead == &sequence.windowPacer.waiter
		service.stateLock.Unlock()
		if !waiting || before.retained != 2 || before.written {
			t.Fatal("real younger original did not retain its pacing reservation")
		}
		time.Sleep(300 * time.Millisecond)
		arrival := time.Now()
		ack := receiveAckMessage{sequenceId: sequence.sequenceId, messageId: olderId}
		wantHandoff := receiveAckHandoffAccepted
		switch feedback {
		case "selective":
			ack.selective = true
		case "wrong_sequence":
			ack.sequenceId = NewId()
			ack.evictions = &ackEvictionNotice{sequenceNumbers: []uint64{1}}
			wantHandoff = receiveAckHandoffSequenceMissing
		case "unknown":
			ack.messageId = NewId()
		case "withdrawal":
			ack.messageId = NewId()
			ack.evictions = &ackEvictionNotice{sequenceNumbers: []uint64{1}}
		}
		if result, err := sequence.ackMessageDetailed(ack, 0); result != wantHandoff || err != nil {
			t.Fatalf("feedback=%s handoff=%d error=%v", feedback, result, err)
		}
		synctest.Wait()
		sequence.resendQueue.stateLock.Lock()
		pendingEvictionPhases := sequence.pendingEvictionPhases
		sequence.resendQueue.stateLock.Unlock()
		if feedback == "withdrawal" && (pendingEvictionPhases == 0 || sequence.ackWindow.PendingCumulativeProgress()) {
			t.Fatal("withdrawal metadata gained head authority or lost its retained recovery owner")
		}
		if feedback == "wrong_sequence" && pendingEvictionPhases != 0 {
			t.Fatal("stale generation published withdrawal state")
		}
		if feedback == "wrong_sequence" || feedback == "unknown" || feedback == "withdrawal" || feedback == "selective" {
			deadline := fixture.start.Add(500 * time.Millisecond)
			if feedback == "selective" {
				deadline = fixture.start.Add(600 * time.Millisecond)
			}
			time.Sleep(time.Until(deadline))
			synctest.Wait()
			if sequence.ctx.Err() == nil || len(youngerFailedAt) != 1 || <-youngerFailedAt != deadline || len(fixture.route) != 0 {
				t.Fatal("noncumulative feedback renewed the paced tail or dispatched it")
			}
		} else {
			time.Sleep(time.Until(fixture.start.Add(600 * time.Millisecond)))
			synctest.Wait()
			if sequence.ctx.Err() != nil || len(youngerFailedAt) != 0 || len(fixture.route) != 0 || !sequence.ackWindow.PendingCumulativeProgress() {
				t.Fatal("pending cumulative progress did not preserve the tail past original expiry")
			}
			if feedback == "cancel" {
				sequence.cancel()
				synctest.Wait()
				if !errors.Is(sequence.ctx.Err(), context.Canceled) || len(youngerFailedAt) != 1 || len(fixture.route) != 0 {
					t.Fatal("cancellation failed to withdraw the renewed original")
				}
			} else if feedback == "expired_wait" {
				deadline := arrival.Add(500 * time.Millisecond)
				time.Sleep(time.Until(deadline.Add(-time.Nanosecond)))
				synctest.Wait()
				if sequence.ctx.Err() != nil || len(prefixCompleted) != 0 || len(youngerFailedAt) != 0 || !sequence.ackWindow.PendingCumulativeProgress() {
					t.Fatal("fixture did not retain pending delivery through the renewed tail lifetime")
				}
				time.Sleep(time.Nanosecond)
				synctest.Wait()
				if sequence.ctx.Err() == nil || len(youngerFailedAt) != 1 || <-youngerFailedAt != deadline || len(fixture.route) != 0 {
					t.Fatal("held silent tail did not expire once at its arrival-based deadline")
				}
				<-sequence.done
				if len(prefixCompleted) != 1 {
					t.Fatal("received cumulative prefix did not complete exactly once during tail expiry")
				}
				if err := <-prefixCompleted; err != nil {
					t.Fatalf("received cumulative prefix failed during tail expiry: %v", err)
				}
				if len(fixture.ackFailedAt) != 0 {
					t.Fatal("another callback failed the already delivered prefix")
				}
			} else {
				time.Sleep(100 * time.Millisecond)
				if feedback == "duplicate" {
					if result, err := sequence.ackMessageDetailed(ack, 0); result != receiveAckHandoffAccepted || err != nil {
						t.Fatalf("pending duplicate handoff=%d error=%v", result, err)
					}
					synctest.Wait()
				}
				time.Sleep(100 * time.Millisecond)
				synctest.Wait()
				var written windowRetryOwnerObservation
				select {
				case written = <-snapshots.afterWrite:
				default:
					t.Fatal("resumed owner did not publish its real younger write")
				}
				if len(fixture.route) != 1 || written.lastHeadAckTime != arrival || len(fixture.ackFailedAt) != 0 ||
					before.written || !written.written || written.messageId != before.messageId {
					t.Fatal("resumed owner lost arrival time, prefix delivery, or the one younger write")
				}
				wire := <-fixture.route
				pack := decodeSendPackLifecycleWirePack(t, wire)
				MessagePoolReturn(wire)
				if pack.SequenceNumber != 1 {
					t.Fatal("pending progress dispatched a duplicate prefix")
				}
				deadline := arrival.Add(500 * time.Millisecond)
				time.Sleep(time.Until(deadline.Add(-time.Nanosecond)))
				synctest.Wait()
				if sequence.ctx.Err() != nil || len(youngerFailedAt) != 0 {
					t.Fatal("renewed tail expired before its arrival-based deadline")
				}
				time.Sleep(time.Nanosecond)
				synctest.Wait()
				if sequence.ctx.Err() == nil || len(youngerFailedAt) != 1 || <-youngerFailedAt != deadline || len(fixture.ackFailedAt) != 0 {
					t.Fatal("head replay or snapshot consumption renewed a stopped tail")
				}
			}
		}
		// Context cancellation precedes cleanup; join the worker before
		// inspecting its private lifetime index and retained-item slice.
		<-sequence.done
		service.stateLock.Lock()
		released := service.pacingReservations == 0 && service.reservedByteCount == 0 && service.waiterHead == nil
		service.stateLock.Unlock()
		if !released || len(sequence.sendItems) != 0 || sequence.resendQueue.Len() != 0 || len(sequence.ackLifetimes.items) != 0 || fixture.client.Ctx().Err() != nil {
			t.Fatal("child termination retained custody or canceled its live parent")
		}
	})
}

// A retained sibling selects item-local expiry before the owner's next snapshot.
// Pending progress must protect ordinary tails at that boundary too, and its
// later application cannot mint another full interval.
func TestAckLifetimePendingHeadPrecedesRetainedExpiry(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := returnRetentionFixture(t, 2)
		start := time.Now()
		prefix, prefixCompleted := returnRetentionWrite(t, fixture, false)
		_, tailCompleted := returnRetentionWrite(t, fixture, false)
		sequence := fixture.sequence()
		held, release := make(chan struct{}), make(chan struct{})
		sequence.sendBuffer.afterInitialWriteQueuedForTest = func(id sendSequenceId, number uint64) {
			if id.Destination == sequence.destination && number == 3 {
				close(held)
				select {
				case <-release:
				case <-fixture.sender.Ctx().Done():
				}
			}
		}
		retained, retainedCompleted := returnRetentionWrite(t, fixture, true)
		<-held
		time.Sleep(400 * time.Millisecond)
		arrival := time.Now()
		fixture.forward(fixture.receive(prefix), fixture.senderIn)
		if !sequence.ackWindow.PendingCumulativeProgress() || len(prefixCompleted) != 0 {
			t.Fatal("prefix acknowledgement was not held before the owner snapshot")
		}
		time.Sleep(200 * time.Millisecond)
		close(release)
		synctest.Wait()
		requireReturnRetentionResult(t, prefixCompleted, nil)
		if sequence.ctx.Err() != nil || len(tailCompleted) != 0 || len(retainedCompleted) != 0 || sequence.lastHeadAckTime != arrival {
			t.Fatal("pre-snapshot expiry ignored pending progress or reset its arrival")
		}
		deadline := start.Add(900 * time.Millisecond)
		time.Sleep(time.Until(deadline.Add(-time.Nanosecond)))
		synctest.Wait()
		if len(tailCompleted) != 0 {
			t.Fatal("ordinary tail expired before its renewed deadline")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		requireReturnRetentionResult(t, tailCompleted, context.DeadlineExceeded)
		if sequence.ctx.Err() != nil || len(retainedCompleted) != 0 || fixture.sender.Ctx().Err() != nil {
			t.Fatal("ordinary expiry canceled its retained sibling or shared parent")
		}
		fixture.drop(retained)
		recovery := fixture.takePack(retained.pack.SequenceNumber)
		if !recovery.pack.Head {
			t.Fatal("retained sibling did not recover across the expired ordinary tail")
		}
		fixture.forward(fixture.receive(recovery), fixture.senderIn)
		requireReturnRetentionResult(t, retainedCompleted, nil)
		if len(sequence.sendItems) != 0 || sequence.resendQueue.Len() != 0 || len(sequence.ackLifetimes.items) != 0 {
			t.Fatal("surviving sibling did not release all acknowledged custody")
		}
	})
}
