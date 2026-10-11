// A retained provider return to a destination that answers nothing must stop
// being rewritten. Retention keeps socket-owned bytes through slow and lossy
// delivery; it must not turn a gone or refused destination into a permanent
// non-billable retransmission of the whole window.
package connect

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Shipped resend and lifetime policy, so the rewrite counts below are the
// field's. The receiver is established with one delivered round first, the
// same shape as a provider return to a client that was live and then went.
func newRetainedSilenceFixture(t *testing.T, configure func(*SendBufferSettings)) *windowRoundFixture {
	t.Helper()
	fixture, _, _ := newAckRetirementFixture(t, TransportTypeH1, 2, NewNoopLogger(), func(settings *ClientSettings) {
		defaults := DefaultSendBufferSettings()
		settings.SendBufferSettings.AckTimeout = defaults.AckTimeout
		settings.SendBufferSettings.MinResendInterval = defaults.MinResendInterval
		settings.SendBufferSettings.RttMinResendInterval = defaults.RttMinResendInterval
		settings.SendBufferSettings.MaxResendInterval = defaults.MaxResendInterval
		settings.SendBufferSettings.RetainedAckTimeout = defaults.RetainedAckTimeout
		if configure != nil {
			configure(settings.SendBufferSettings)
		}
	})
	fixture.forward(fixture.receive(fixture.write(32)), fixture.senderIn)
	return fixture
}

// Admits one socket-owned return and drops its first physical write, so the
// destination never sees it. The callback channel reports the terminal result.
func writeSilentRetainedReturn(t *testing.T, fixture *windowRoundFixture, content string) (*windowRoundFrame, chan error) {
	t.Helper()
	completed := make(chan error, 1)
	frame := RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: content})
	admitted, err := fixture.sender.SendWithTimeoutDetailed(frame, fixture.receiver.ClientId(), func(err error) {
		completed <- err
	}, time.Second, sendPackRecoveryOption{upstreamRecoverable: true, retainAfterAckTimeout: true})
	if !admitted || err != nil {
		MessagePoolReturn(frame.MessageBytes)
		t.Fatalf("retained return admission=%t error=%v", admitted, err)
	}
	pack := fixture.takePack(fixture.nextNumber)
	fixture.nextNumber++
	return pack, completed
}

// Discards every rewrite the destination would never answer and counts them.
func dropSenderRewrites(fixture *windowRoundFixture) int {
	synctest.Wait()
	count := 0
	for len(fixture.senderOut) != 0 {
		MessagePoolReturn(<-fixture.senderOut)
		count++
	}
	return count
}

// Advances virtual time in steps no longer than the rewrite cadence, so the
// bounded test route never fills, and returns the rewrites discarded.
func advanceDroppingRewrites(fixture *windowRoundFixture, duration time.Duration) int {
	step := DefaultSendBufferSettings().MaxResendInterval
	count := 0
	for 0 < duration {
		d := min(step, duration)
		time.Sleep(d)
		duration -= d
		count += dropSenderRewrites(fixture)
	}
	return count
}

// Returns the newest rewrite of one Pack now on the route and drops every
// other write, so concurrent rewrites of sibling items cannot reorder the take.
func takeRetainedRewrite(t *testing.T, fixture *windowRoundFixture, sequenceNumber uint64) *windowRoundFrame {
	t.Helper()
	synctest.Wait()
	var rewrite *windowRoundFrame
	for len(fixture.senderOut) != 0 {
		frame := fixture.take(fixture.senderOut)
		if frame.pack != nil && frame.pack.SequenceNumber == sequenceNumber {
			if rewrite != nil {
				fixture.drop(rewrite)
			}
			rewrite = frame
		} else {
			fixture.drop(frame)
		}
	}
	if rewrite == nil {
		t.Fatalf("no rewrite of Pack %d reached the route", sequenceNumber)
	}
	return rewrite
}

func requireNoRetainedResult(t *testing.T, completed <-chan error, when string) {
	t.Helper()
	select {
	case err := <-completed:
		t.Fatalf("%s: retained return finished early with %v", when, err)
	default:
	}
}

// The root cause of the leak: an admitted socket-owned return is excluded from
// the ACK lifetime, so a destination that never answers keeps the item on the
// wire forever. It must end once the destination has been silent for
// RetainedAckTimeout, fail its owner, and stop writing.
func TestRetainedReturnStopsRewritingSilentDestination(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newRetainedSilenceFixture(t, nil)
		retainedTimeout := DefaultSendBufferSettings().RetainedAckTimeout
		maxResendInterval := DefaultSendBufferSettings().MaxResendInterval
		first, completed := writeSilentRetainedReturn(t, fixture, "socket-owned return")
		fixture.drop(first)
		sequence := fixture.sequence()

		// Past the ordinary lifetime the item is still retained, as designed.
		rewrites := advanceDroppingRewrites(fixture, retainedTimeout-time.Second)
		requireNoRetainedResult(t, completed, "inside the retained bound")
		if sequence.ctx.Err() != nil {
			t.Fatal("the sequence exited before its destination was silent for the retained bound")
		}
		if rewrites == 0 {
			t.Fatal("the retained return was not rewritten while it waited")
		}

		rewrites += advanceDroppingRewrites(fixture, 2*time.Second)
		select {
		case err := <-completed:
			if err == nil {
				t.Fatal("a silent destination was reported as delivered")
			}
		default:
			t.Fatalf("retained return to a destination silent for %s was still pending after %d rewrites", retainedTimeout, rewrites)
		}
		if sequence.ctx.Err() == nil {
			t.Fatal("the silent sequence did not exit")
		}
		if bound := int(retainedTimeout/maxResendInterval) + 4; bound < rewrites {
			t.Fatalf("%d rewrites inside %s, want at most %d", rewrites, retainedTimeout, bound)
		}

		// Nothing more may reach the wire for the gone destination.
		if after := advanceDroppingRewrites(fixture, 30*time.Minute); after != 0 {
			t.Fatalf("%d writes after the retained bound ended the sequence", after)
		}
	})
}

// Retention exists for acknowledgement gaps longer than AckTimeout. A gap
// shorter than RetainedAckTimeout must still deliver the bytes exactly once.
func TestRetainedReturnSurvivesAckGapShorterThanBound(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newRetainedSilenceFixture(t, nil)
		settings := DefaultSendBufferSettings()
		gap := settings.AckTimeout + (settings.RetainedAckTimeout-settings.AckTimeout)/2
		first, completed := writeSilentRetainedReturn(t, fixture, "slow but live")
		fixture.drop(first)
		sequence := fixture.sequence()
		advanceDroppingRewrites(fixture, gap)
		requireNoRetainedResult(t, completed, "after an ack gap longer than AckTimeout")

		// The destination returns: the next rewrite is delivered and acknowledged.
		time.Sleep(settings.MaxResendInterval)
		recovery := takeRetainedRewrite(t, fixture, first.pack.SequenceNumber)
		fixture.forward(fixture.receive(recovery), fixture.senderIn)
		select {
		case err := <-completed:
			if err != nil {
				t.Fatalf("retained return after a recoverable gap ended with %v", err)
			}
		default:
			t.Fatal("retained return was not completed by its acknowledgement")
		}
		if sequence.ctx.Err() != nil {
			t.Fatal("a recoverable ack gap closed the sequence")
		}
		if fixture.deliveredCount != 2 {
			t.Fatalf("delivered %d messages, want the established round and the return once", fixture.deliveredCount)
		}
	})
}

// The bound is the destination's silence, not an item's age. A slow peer that
// acknowledges one return late restarts it for a younger outstanding return,
// even when that return's own age has passed the bound.
func TestRetainedSilenceRestartsOnAcknowledgement(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newRetainedSilenceFixture(t, nil)
		settings := DefaultSendBufferSettings()
		older, olderCompleted := writeSilentRetainedReturn(t, fixture, "older")
		fixture.drop(older)
		younger, youngerCompleted := writeSilentRetainedReturn(t, fixture, "younger")
		fixture.drop(younger)
		sequence := fixture.sequence()

		almost := settings.RetainedAckTimeout - 2*settings.MaxResendInterval
		advanceDroppingRewrites(fixture, almost)
		time.Sleep(settings.MaxResendInterval)
		olderRecovery := takeRetainedRewrite(t, fixture, older.pack.SequenceNumber)
		fixture.forward(fixture.receive(olderRecovery), fixture.senderIn)
		select {
		case err := <-olderCompleted:
			if err != nil {
				t.Fatalf("older retained return ended with %v", err)
			}
		default:
			t.Fatal("older retained return was not acknowledged")
		}

		// The younger return is now older than the bound, but the destination
		// answered recently, so it must still be retained.
		advanceDroppingRewrites(fixture, almost)
		requireNoRetainedResult(t, youngerCompleted, "after the destination answered within the bound")
		if sequence.ctx.Err() != nil {
			t.Fatal("an acknowledgement did not restart the destination's silence")
		}
		time.Sleep(settings.MaxResendInterval)
		youngerRecovery := takeRetainedRewrite(t, fixture, younger.pack.SequenceNumber)
		fixture.forward(fixture.receive(youngerRecovery), fixture.senderIn)
		select {
		case err := <-youngerCompleted:
			if err != nil {
				t.Fatalf("younger retained return ended with %v", err)
			}
		default:
			t.Fatal("younger retained return was not acknowledged")
		}
	})
}

// Zero keeps the historical retention for callers that opt out of the bound.
func TestRetainedSilenceZeroKeepsUnboundedRetention(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newRetainedSilenceFixture(t, func(settings *SendBufferSettings) {
			settings.RetainedAckTimeout = 0
		})
		first, completed := writeSilentRetainedReturn(t, fixture, "unbounded")
		fixture.drop(first)
		sequence := fixture.sequence()
		advanceDroppingRewrites(fixture, 2*DefaultSendBufferSettings().RetainedAckTimeout)
		requireNoRetainedResult(t, completed, "with the retained bound disabled")
		if sequence.ctx.Err() != nil {
			t.Fatal("a disabled retained bound closed the sequence")
		}
		time.Sleep(DefaultSendBufferSettings().MaxResendInterval)
		recovery := takeRetainedRewrite(t, fixture, first.pack.SequenceNumber)
		fixture.forward(fixture.receive(recovery), fixture.senderIn)
		select {
		case err := <-completed:
			if err != nil {
				t.Fatalf("unbounded retained return ended with %v", err)
			}
		default:
			t.Fatal("unbounded retained return was not acknowledged")
		}
	})
}
