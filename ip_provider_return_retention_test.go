package connect

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Socket-owned return bytes cannot be regenerated upstream. A disposable
// control expiring on their sequence must fail only its own owner, and the
// existing Head wire marker must repair the abandoned sequence position.
func TestNonRetainedAckTimeoutPreservesRetainedItems(t *testing.T) {
	for _, version := range []int{1, 2} {
		for _, held := range []bool{false, true} {
			t.Run(fmt.Sprintf("v%d/held=%t", version, held), func(t *testing.T) {
				assertMessagePoolOwnership(t)
				synctest.Test(t, func(t *testing.T) {
					fixture := returnRetentionFixture(t, version)
					control, failed := returnRetentionWrite(t, fixture, false)
					retained, completed := returnRetentionWrite(t, fixture, true)
					sequence := fixture.sequence()
					if held {
						reply := fixture.receive(retained)
						if !reply.ack.Selective || fixture.deliveredCount != 1 {
							t.Fatal("successor was not held behind the missing control")
						}
						fixture.forward(reply, fixture.senderIn)
					}
					time.Sleep(500 * time.Millisecond)
					synctest.Wait()
					if sequence.ctx.Err() != nil || len(completed) != 0 || sequence.resendQueue.Len() != 1 {
						t.Fatal("disposable control expiry discarded the socket-owned successor")
					}
					requireReturnRetentionResult(t, failed, context.DeadlineExceeded)
					if sequence.resendQueue.GetByMessageId(RequireIdFromBytes(control.pack.MessageId)) != nil {
						t.Fatal("expired control retained ACK ownership")
					}
					recovery := fixture.takePack(retained.pack.SequenceNumber)
					if !recovery.pack.Head || !bytes.Equal(recovery.pack.MessageId, retained.pack.MessageId) ||
						!bytes.Equal(recovery.pack.SequenceId, retained.pack.SequenceId) {
						t.Fatal("abandoned prefix was not repaired with the same retained identity as Head")
					}
					fixture.forward(fixture.receive(recovery), fixture.senderIn)
					requireReturnRetentionResult(t, completed, nil)
					if fixture.deliveredCount != 2 || sequence.resendQueue.Len() != 0 || len(sequence.ackLifetimes.items) != 0 || len(failed) != 0 {
						t.Fatal("retained delivery was duplicated or left an ACK/lifetime owner")
					}
				})
			})
		}
	}
}

// Retiring a younger control never authorizes a head to jump over an older
// retained message. Its successor becomes Head only after that older real ACK.
func TestNonRetainedAckTimeoutPreservesOlderRetainedPrefix(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := returnRetentionFixture(t, 2)
		older, olderCompleted := returnRetentionWrite(t, fixture, true)
		_, failed := returnRetentionWrite(t, fixture, false)
		younger, youngerCompleted := returnRetentionWrite(t, fixture, true)
		sequence := fixture.sequence()
		time.Sleep(500 * time.Millisecond)
		synctest.Wait()
		if sequence.ctx.Err() != nil || len(olderCompleted) != 0 || len(youngerCompleted) != 0 ||
			sequence.resendQueue.Len() != 2 || len(fixture.senderOut) != 0 {
			t.Fatal("interior expiry lost retained ownership or promoted past the older retained prefix")
		}
		requireReturnRetentionResult(t, failed, context.DeadlineExceeded)
		fixture.forward(fixture.receive(older), fixture.senderIn)
		requireReturnRetentionResult(t, olderCompleted, nil)
		recovery := fixture.takePack(younger.pack.SequenceNumber)
		if !recovery.pack.Head || !bytes.Equal(recovery.pack.MessageId, younger.pack.MessageId) {
			t.Fatal("cumulative progress did not repair the now-oldest abandoned gap")
		}
		fixture.forward(fixture.receive(recovery), fixture.senderIn)
		requireReturnRetentionResult(t, youngerCompleted, nil)
		if fixture.deliveredCount != 3 || sequence.resendQueue.Len() != 0 {
			t.Fatal("ordered survivors did not complete exactly once")
		}
	})
}

// The due-retry branch has an independent expiry check, including the forced
// lifetime hook. It must use the same item-local policy as a paced lifetime.
func TestNonRetainedForcedAckTimeoutPreservesRetainedItems(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := returnRetentionFixture(t, 2)
		_, failed := returnRetentionWrite(t, fixture, false)
		retained, completed := returnRetentionWrite(t, fixture, true)
		sequence := fixture.sequence()
		sequence.sendBuffer.forceAckTimeoutForTest = func(id sendSequenceId) bool {
			return id.Destination == fixture.receiver.ClientId()
		}
		// New admission wakes the otherwise sleeping owner before real expiry.
		younger, youngerCompleted := returnRetentionWrite(t, fixture, true)
		if sequence.ctx.Err() != nil || len(completed) != 0 || len(youngerCompleted) != 0 {
			t.Fatal("due-retry expiry failed a retained owner")
		}
		requireReturnRetentionResult(t, failed, context.DeadlineExceeded)
		fixture.drop(retained)
		recovery := fixture.recovery(retained)
		if !recovery.pack.Head {
			t.Fatal("forced expiry did not repair its abandoned prefix")
		}
		fixture.forward(fixture.receive(recovery), fixture.senderIn)
		requireReturnRetentionResult(t, completed, nil)
		fixture.forward(fixture.receive(younger), fixture.senderIn)
		requireReturnRetentionResult(t, youngerCompleted, nil)
	})
}

func returnRetentionFixture(t *testing.T, version int) *windowRoundFixture {
	t.Helper()
	fixture, _, _ := newAckRetirementFixture(t, TransportTypeH1, version, NewNoopLogger(), func(settings *ClientSettings) {
		settings.SendBufferSettings.AckTimeout = 500 * time.Millisecond
		settings.SendBufferSettings.MinResendInterval = time.Second
		settings.SendBufferSettings.RttMinResendInterval = time.Second
		settings.SendBufferSettings.MaxResendInterval = time.Second
	})
	// Establish the receiver before withholding the disposable head.
	fixture.forward(fixture.receive(fixture.write(32)), fixture.senderIn)
	return fixture
}

func returnRetentionWrite(t *testing.T, fixture *windowRoundFixture, retained bool) (*windowRoundFrame, chan error) {
	t.Helper()
	completed := make(chan error, 4)
	frame := RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: fmt.Sprintf("return retained=%t", retained)})
	admitted, err := fixture.sender.SendWithTimeoutDetailed(frame, fixture.receiver.ClientId(), func(err error) {
		completed <- err
	}, time.Second, sendPackRecoveryOption{upstreamRecoverable: true, retainAfterAckTimeout: retained})
	if !admitted || err != nil {
		MessagePoolReturn(frame.MessageBytes)
		t.Fatalf("return admission=%t error=%v", admitted, err)
	}
	pack := fixture.takePack(fixture.nextNumber)
	fixture.nextNumber++
	return pack, completed
}

func requireReturnRetentionResult(t *testing.T, completed <-chan error, expected error) {
	t.Helper()
	select {
	case err := <-completed:
		if !errors.Is(err, expected) {
			t.Fatalf("terminal error=%v, want %v", err, expected)
		}
	default:
		t.Fatalf("missing terminal result %v", expected)
	}
}
