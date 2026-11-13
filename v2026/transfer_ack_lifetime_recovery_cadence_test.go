// Both production recovery caps must honor useful cumulative progress without
// creating an already-due retry after the original send-time lifetime.
package connect

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// A confirmed raw H1 residence is bounded by the renewed lifetime at selection.
func TestAckLifetimeRecoverySelectionUsesProgressDeadline(t *testing.T) {
	testAckLifetimeRecoveryAfterProgress(t, "selection")
}

// A successful retry after the original deadline schedules a future wake.
func TestAckLifetimeRecoverySuccessfulWriteBacksOff(t *testing.T) {
	testAckLifetimeRecoveryAfterProgress(t, "success")
}

// A ready-only route refusal must retain the same finite backoff and custody.
func TestAckLifetimeRecoveryRefusedWriteBacksOff(t *testing.T) {
	testAckLifetimeRecoveryAfterProgress(t, "refused")
}

// Real initial writes and validated ingress establish the item and cumulative
// clock. An owner hook then lends exclusive state to one bounded production
// recovery call, so reverting either cap fails an assertion without spinning.
func testAckLifetimeRecoveryAfterProgress(t *testing.T, operation string) {
	t.Helper()
	runWindowRetryClockFixture(t, 4*time.Second, 500*time.Millisecond, func(t *testing.T, fixture *windowRetryClockFixture) {
		sequence := fixture.sequence
		prefix := sequence.resendQueue.PeekFirst()
		prefixId := prefix.messageId
		prefixTag := sequenceTagFromProtocol(decodeSendPackLifecycleWirePack(t, prefix.transferFrameBytes).Tag)
		tailWritten, releaseTail := make(chan struct{}), make(chan struct{})
		ownerHeld, releaseOwner := make(chan struct{}), make(chan struct{})
		failed := make(chan time.Time, 2)
		sequence.sendBuffer.afterInitialWriteQueuedForTest = func(id sendSequenceId, number uint64) {
			if id.Destination == sequence.destination && number == 1 {
				close(tailWritten)
				select {
				case <-releaseTail:
				case <-fixture.client.Ctx().Done():
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
		frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(1280)}
		clear(frame.MessageBytes)
		if !fixture.client.SendWithTimeout(frame, sequence.destination, func(err error) {
			if err != nil {
				failed <- time.Now()
			}
		}, time.Second) {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatal("tail was not admitted")
		}
		close(fixture.releaseInitial)
		<-tailWritten
		MessagePoolReturn(<-fixture.route)
		tail := sequence.sendItems[1]
		time.Sleep(300 * time.Millisecond)
		arrival := time.Now()
		held := false
		sequence.sendBuffer.afterApplyAckSnapshotForTest = func(id sendSequenceId) {
			if id.Destination == sequence.destination && !held && sequence.lastHeadAckTime == arrival {
				held = true
				close(ownerHeld)
				select {
				case <-releaseOwner:
				case <-fixture.client.Ctx().Done():
				}
			}
		}
		ack := receiveAckMessage{sequenceId: sequence.sequenceId, messageId: prefixId}
		if operation == "selection" {
			// Raw residence requires validated receiver timing. Echo the real
			// first write's tag; a legacy head supplies no receiver timing row.
			ack.tag, ack.receiverAckDelaySet = prefixTag, true
		}
		if result, err := sequence.ackMessageDetailed(ack, 0); result != receiveAckHandoffAccepted || err != nil {
			t.Fatalf("head handoff=%d error=%v", result, err)
		}
		close(releaseTail)
		<-ownerHeld
		time.Sleep(201 * time.Millisecond)
		now := time.Now()
		deadline := arrival.Add(tail.ackTimeout)
		if !now.After(tail.sendTime.Add(tail.ackTimeout)) || !now.Before(deadline) || sequence.ackWindow.Pending() || len(sequence.sendItems) != 1 {
			t.Fatal("fixture did not retain an uncovered tail beyond its original lifetime")
		}
		attempts := 0
		tailNumber := tail.sequenceNumber
		sequence.sendBufferSettings.TransferWireMessageObserver = func(observation TransferWireMessageObservation) {
			if observation.Resend && observation.SequenceNumber == tailNumber {
				attempts++
			}
		}
		queuedBytes := sequence.resendQueue.queueByteCount
		if operation == "selection" {
			interval := sequence.sharedRawRecoveryInterval(tail, now)
			if interval <= 0 || sequence.firstPhysicalRecoveryTime(tail).Add(interval).Before(deadline) || !sequence.transferFlightPolicy().h1Only {
				t.Fatalf("timed head did not establish raw residence beyond the original deadline: interval=%s physical=%s deadline=%s", interval, sequence.firstPhysicalRecoveryTime(tail).Sub(fixture.start), deadline.Sub(fixture.start))
			}
			_, outcome, err := sequence.selectDueRecovery(tail, now, sequence.transferFlightPolicy(), sequence.ackWindow)
			if err != nil || outcome != sendRecoverySelectionDeferred || tail.resendTime != deadline || attempts != 0 {
				t.Fatalf("selection lost renewed cap: outcome=%d error=%v next=%s want=%s attempts=%d", outcome, err, tail.resendTime.Sub(fixture.start), deadline.Sub(fixture.start), attempts)
			}
		} else {
			// Pacing itself is covered by the pending-head roots. This call
			// isolates the actual recovery writer's success/refusal rearm edge.
			sequence.sendBufferSettings.disableWindowPacingForTest = true
			sequence.sendBufferSettings.WriteTimeout = 0
			if operation == "refused" {
				for range cap(fixture.route) {
					fixture.route <- MessagePoolGet(1)
				}
			}
			if err := sequence.writeSelectedRecovery(tail, sendRecoverySelection{kind: sendRecoveryNone}); err != nil {
				t.Fatalf("recovery owner returned unexpectedly: %v", err)
			}
			if tail.resendTime != deadline || !tail.resendTime.After(now) || tail.sendCount != 2 || attempts != 1 {
				t.Fatalf("retry did not back off: operation=%s next=%s want=%s count=%d attempts=%d", operation, tail.resendTime.Sub(fixture.start), deadline.Sub(fixture.start), tail.sendCount, attempts)
			}
			if operation == "success" {
				if sequence.resendWriteCount.Load() != 1 || len(fixture.route) != 1 {
					t.Fatal("successful recovery did not write exactly one copy")
				}
				wire := <-fixture.route
				pack := decodeSendPackLifecycleWirePack(t, wire)
				MessagePoolReturn(wire)
				if RequireIdFromBytes(pack.MessageId) != tail.messageId {
					t.Fatal("recovery changed the retained message identity")
				}
			} else if sequence.resendWriteCount.Load() != 0 || sequence.client.recoveryWriteErrorCount.Load() != 1 || len(fixture.route) != cap(fixture.route) {
				t.Fatal("route refusal consumed a share or lost its failed-attempt accounting")
			}
			for len(fixture.route) > 0 {
				MessagePoolReturn(<-fixture.route)
			}
		}
		if sequence.resendQueue.GetByMessageId(tail.messageId) != tail || sequence.resendQueue.queueByteCount != queuedBytes || len(failed) != 0 {
			t.Fatal("recovery changed retained custody or completed an unacknowledged item")
		}
		wantAttempts := attempts
		close(releaseOwner)
		synctest.Wait()
		time.Sleep(time.Until(deadline.Add(-time.Nanosecond)))
		synctest.Wait()
		if attempts != wantAttempts || len(failed) != 0 || len(fixture.route) != 0 {
			t.Fatal("resumed owner retried before its future deadline")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if attempts != wantAttempts || len(failed) != 1 || <-failed != deadline || sequence.ctx.Err() == nil || len(fixture.ackFailedAt) != 0 || fixture.client.Ctx().Err() != nil {
			t.Fatal("stopped head did not expire once without another recovery attempt")
		}
		<-sequence.done
		if len(sequence.sendItems) != 0 || sequence.resendQueue.Len() != 0 || len(sequence.ackLifetimes.items) != 0 {
			t.Fatal("finite expiry retained message custody")
		}
	})
}
