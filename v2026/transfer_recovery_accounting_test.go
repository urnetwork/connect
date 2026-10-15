// Recovery observations and outcomes belong to one bounded writer operation,
// even when validated feedback arrives at its publication or retry boundary.
package connect

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

// Successful writes consume their share; rejected writes leave it untouched.
type recoveryAccountingTestWriter struct {
	windowPacingPolicyWriter
	attempt func([]byte, time.Duration) (bool, TransportType, error)
	writes  int
}

func (self *recoveryAccountingTestWriter) WriteDetailedWithTransport(_ context.Context, wire []byte, timeout time.Duration) (bool, TransportType, error) {
	self.writes++
	if self.attempt != nil {
		return self.attempt(wire, timeout)
	}
	MessagePoolReturn(wire)
	return true, TransportTypeH3, nil
}

// Direct ownership avoids a scheduler race while retaining the production
// writer, lifetime index, ACK window, codec and shared-buffer paths.
func newRecoveryAccountingFixture(t *testing.T) (*SendSequence, *sendItem, *recoveryAccountingTestWriter, *int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	sequence := newEstimatorFixture(t, func(settings *SendBufferSettings) {
		settings.DeliverySizedWindowScale = 0
		settings.WriteTimeout = time.Second
	})
	sequence.ctx, sequence.cancel, sequence.log = ctx, cancel, NewNoopLogger()
	sequence.client = &Client{ctx: ctx, clientId: NewId(), log: sequence.log}
	sequence.sequenceId, sequence.destination = NewId(), NewId()
	sequence.ackWindow = newSequenceAckWindow()
	sequence.flightController = newSendFlightController(sequence.sendBufferSettings)
	writer := &recoveryAccountingTestWriter{}
	sequence.contractMultiRouteWriter = writer
	sequence.contractMultiRouteWriterDestination = DestinationId(sequence.destination)
	observations := 0
	sequence.sendBufferSettings.TransferWireMessageObserver = func(TransferWireMessageObservation) { observations++ }
	item := &sendItem{
		transferItem: transferItem{messageId: NewId(), sequenceNumber: 1},
		sendTime:     time.Now(), ackTimeout: 10 * time.Second, sendCount: 1, expectsAck: true,
	}
	item.transferFrameBytes = marshalSendPackTransferFrame(&sendPackFrame{
		path:      sendTransferPath(sequence.client.ClientId(), DestinationId(sequence.destination)),
		messageId: item.messageId, sequenceId: sequence.sequenceId, sequenceNumber: item.sequenceNumber,
	})
	sequence.sendItems = append(sequence.sendItems, item)
	sequence.addResendItem(item)
	t.Cleanup(func() {
		cancel()
		sequence.ackLifetimes.clear()
		for _, retained := range sequence.resendQueue.Clear() {
			retained.messagePoolReturn()
		}
	})
	return sequence, item, writer, &observations
}

// Feedback already accepted before publication suppresses an unissued retry,
// so neither a wire observation nor an actual writer call may be claimed.
func TestTransferRecoveryAlreadyAcknowledgedIsNotObserved(t *testing.T) {
	assertMessagePoolOwnership(t)
	sequence, item, writer, observations := newRecoveryAccountingFixture(t)
	sequence.ackWindow.Update(sequenceAck{sequenceNumber: item.sequenceNumber, messageId: item.messageId})
	_, err := sequence.writeMaybeWrappedBytes(item.transferFrameBytes, TransferPath{}, true, item, true, false)
	if !errors.Is(err, errWindowPacingAcknowledged) || *observations != 0 || writer.writes != 0 {
		t.Fatalf("already delivered retry: err=%v observations=%d route_calls=%d", err, *observations, writer.writes)
	}
}

// Once published, a concurrently received ACK cannot erase the dispatch that
// the observer describes. The ordinary ACK owner still completes the item.
func TestTransferRecoveryObservedAckKeepsSuccessfulDispatch(t *testing.T) {
	assertMessagePoolOwnership(t)
	sequence, item, writer, observations := newRecoveryAccountingFixture(t)
	sequence.sendBufferSettings.TransferWireMessageObserver = func(TransferWireMessageObservation) {
		*observations++
		sequence.ackWindow.Update(sequenceAck{sequenceNumber: item.sequenceNumber, messageId: item.messageId})
	}
	_, err := sequence.writeMaybeWrappedBytes(item.transferFrameBytes, TransferPath{}, true, item, true, false)
	if err != nil || *observations != 1 || writer.writes != 1 {
		t.Fatalf("ACK after observation erased dispatch: err=%v observations=%d route_calls=%d", err, *observations, writer.writes)
	}
}

// A real rejected writer call is a failed recovery attempt, not an unissued
// retry, even when delivery of an older copy becomes pending during the call.
func TestTransferRecoveryLateAckKeepsRouteFailure(t *testing.T) {
	assertMessagePoolOwnership(t)
	sequence, item, writer, observations := newRecoveryAccountingFixture(t)
	writer.attempt = func([]byte, time.Duration) (bool, TransportType, error) {
		sequence.ackWindow.Update(sequenceAck{sequenceNumber: item.sequenceNumber, messageId: item.messageId})
		return false, TransportTypeH3, nil
	}
	_, err := sequence.writeMaybeWrappedBytes(item.transferFrameBytes, TransferPath{}, true, item, true, false)
	if !errors.Is(err, errTransferRouteWriteTimeout) || *observations != 1 || writer.writes != 1 {
		t.Fatalf("ACK hid rejected dispatch: err=%v observations=%d route_calls=%d", err, *observations, writer.writes)
	}
}

// Cancellation and expiry already count as failed bounded dispatches. Moving
// the ACK gate must not remove their corresponding wire observation.
func TestTransferRecoveryPreflightTerminalIsObserved(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, expired := range []bool{false, true} {
		sequence, item, writer, observations := newRecoveryAccountingFixture(t)
		want := error(context.Canceled)
		if expired {
			item.sendTime = time.Now().Add(-2 * item.ackTimeout)
			sequence.ackLifetimes.update(item)
			want = context.DeadlineExceeded
		} else {
			sequence.cancel()
		}
		_, err := sequence.writeMaybeWrappedBytes(item.transferFrameBytes, TransferPath{}, true, item, true, false)
		if !errors.Is(err, want) || *observations != 1 || writer.writes != 0 {
			t.Fatalf("expired=%t terminal dispatch: err=%v observations=%d route_calls=%d", expired, err, *observations, writer.writes)
		}
	}
}

// A shorter older lifetime can wake and retry the same unconsumed share.
// Its later ACK renews eligibility but never starts another counted dispatch.
func TestTransferRecoveryLifetimeWakeKeepsOneObservedOutcome(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		sequence, item, writer, observations := newRecoveryAccountingFixture(t)
		older := &sendItem{transferItem: transferItem{messageId: NewId()},
			sendTime: time.Now(), ackTimeout: 10 * time.Millisecond, expectsAck: true}
		sequence.addResendItem(older)
		var firstShare []byte
		writer.attempt = func(wire []byte, timeout time.Duration) (bool, TransportType, error) {
			if writer.writes == 1 {
				if timeout != older.ackTimeout {
					t.Fatalf("first writer did not retain older lifetime: %s", timeout)
				}
				firstShare = wire
				time.Sleep(timeout)
				sequence.ackWindow.Update(sequenceAck{messageId: older.messageId})
			} else {
				if writer.writes != 2 || &wire[0] != &firstShare[0] {
					t.Fatal("lifetime retry replaced or repeated the one unconsumed share")
				}
				sequence.ackWindow.Update(sequenceAck{sequenceNumber: item.sequenceNumber, messageId: item.messageId})
			}
			return false, TransportTypeH3, nil
		}
		_, err := sequence.writeMaybeWrappedBytes(item.transferFrameBytes, TransferPath{}, true, item, true, false)
		if !errors.Is(err, errTransferRouteWriteTimeout) || *observations != 1 || writer.writes != 2 {
			t.Fatalf("lifetime continuation lost its outcome: err=%v observations=%d route_calls=%d", err, *observations, writer.writes)
		}
	})
}
