// Real H1-classified Transfer workers distinguish useful cumulative delivery
// progress from old, unrelated and selective feedback when timing a silent tail.
package connect

import (
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// A real receiver's advancing head grants one full configured silence interval.
func TestH1ResidenceTailLifetimeFollowsUsefulHead(t *testing.T) {
	testH1ResidenceTailProgress(t, "useful_head")
}

// A replay of the already consumed cumulative head grants no further interval.
func TestH1ResidenceDuplicateHeadKeepsTailDeadline(t *testing.T) {
	testH1ResidenceTailProgress(t, "duplicate_head")
}

// A held older head cannot replace the useful newer head's clock.
func TestH1ResidenceStaleHeadKeepsTailDeadline(t *testing.T) {
	testH1ResidenceTailProgress(t, "stale_head")
}

// A valid message name in a different sequence is rejected before publication.
func TestH1ResidenceOtherSequenceKeepsTailDeadline(t *testing.T) {
	testH1ResidenceTailProgress(t, "other_sequence")
}

// An unknown message in the right sequence has no retained delivery authority.
func TestH1ResidenceUnknownMessageKeepsTailDeadline(t *testing.T) {
	testH1ResidenceTailProgress(t, "unknown_message")
}

// Receiver custody of a later SACK does not prove that the missing prefix moved.
func TestH1ResidenceLaterSackKeepsMissingTailDeadline(t *testing.T) {
	testH1ResidenceTailProgress(t, "later_sack")
}

// A second useful head restarts the interval for the remaining unreceived tail.
func TestH1ResidenceNewHeadRestartsRemainingTailDeadline(t *testing.T) {
	testH1ResidenceTailProgress(t, "new_head")
}

// Peer delivery with its actual ACK still held does not renew the sender's life.
func TestH1ResidenceHeldPeerAckKeepsOriginalTailDeadline(t *testing.T) {
	testH1ResidenceTailProgress(t, "held_peer_ack")
}

// All ordering is explicit: capture four real wire Packs, deliver two at the
// receiver, release one genuine cumulative ACK, then release one selected reply.
// Quiescence joins each transition before fake time crosses an exact deadline.
func testH1ResidenceTailProgress(t *testing.T, feedback string) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		var failureAt atomic.Int64
		var failureCount atomic.Uint64
		fixture, _, _ := newAckRetirementFixture(t, TransportTypeH1, 2, NewNoopLogger(), func(settings *ClientSettings) {
			settings.SendBufferSettings.SendPackLifecycleObserver = func(event SendPackLifecycleObservation) {
				if event.Phase == SendPackLifecyclePhaseTerminal && event.Err != nil {
					failureAt.CompareAndSwap(0, time.Now().UnixNano())
					failureCount.Add(1)
				}
			}
		})
		start := time.Now()
		oldest := fixture.write(64)
		predecessor := fixture.write(64)
		missing := fixture.write(64)
		later := fixture.write(64)
		sequence := fixture.sequence()
		timeout := sequence.sendBufferSettings.AckTimeout
		if timeout != DefaultMultiClientSettings().AckTimeout || !sequence.transferFlightPolicy().h1Only {
			t.Fatal("fixture must retain the configured reliable H1 lifetime")
		}
		oldReply := fixture.receive(oldest)
		progressReply := fixture.receive(predecessor)
		if oldReply.ack.Selective || progressReply.ack.Selective ||
			RequireIdFromBytes(progressReply.ack.MessageId) != RequireIdFromBytes(predecessor.pack.MessageId) ||
			fixture.deliveredCount != 2 || fixture.ackedCount != 0 {
			t.Fatal("real receiver did not retain the two exact cumulative ACKs")
		}
		replayedReply := &windowRoundFrame{bytes: MessagePoolCopy(progressReply.bytes), ack: progressReply.ack}
		fixture.heldFrames = append(fixture.heldFrames, replayedReply)
		wantAcked := 0
		deadline := start.Add(timeout)
		var progressAt time.Time
		time.Sleep(4 * time.Second)
		synctest.Wait()
		if feedback != "held_peer_ack" {
			fixture.forward(progressReply, fixture.senderIn)
			progressAt = sequence.lastHeadAckTime
			if progressAt != time.Now() || fixture.ackedCount != 2 || sequence.ctx.Err() != nil {
				t.Fatal("actual cumulative receiver ACK did not advance the retained sender head")
			}
			wantAcked = 2
			deadline = progressAt.Add(timeout)
		}
		time.Sleep(6 * time.Second)
		synctest.Wait()
		switch feedback {
		case "duplicate_head":
			fixture.forward(replayedReply, fixture.senderIn)
		case "stale_head":
			fixture.forward(oldReply, fixture.senderIn)
		case "other_sequence", "unknown_message":
			ack := receiveAckMessage{
				sequenceId: sequence.sequenceId,
				messageId:  RequireIdFromBytes(missing.pack.MessageId),
			}
			wantResult := receiveAckHandoffAccepted
			if feedback == "other_sequence" {
				ack.sequenceId = NewId()
				wantResult = receiveAckHandoffSequenceMissing
			} else {
				ack.messageId = NewId()
			}
			if result, err := sequence.ackMessageDetailed(ack, 0); result != wantResult || err != nil {
				t.Fatalf("invalid feedback handoff=%d err=%v want=%d", result, err, wantResult)
			}
			synctest.Wait()
		case "later_sack":
			reply := fixture.receive(later)
			if !reply.ack.Selective || RequireIdFromBytes(reply.ack.MessageId) != RequireIdFromBytes(later.pack.MessageId) {
				t.Fatal("real receiver did not retain the later out-of-order Pack")
			}
			fixture.forward(reply, fixture.senderIn)
		case "new_head":
			reply := fixture.receive(missing)
			if reply.ack.Selective || RequireIdFromBytes(reply.ack.MessageId) != RequireIdFromBytes(missing.pack.MessageId) {
				t.Fatal("real receiver did not advance the missing cumulative prefix")
			}
			fixture.forward(reply, fixture.senderIn)
			progressAt = sequence.lastHeadAckTime
			if progressAt != time.Now() {
				t.Fatal("new useful cumulative ACK did not restart the remaining tail")
			}
			wantAcked = 3
			deadline = progressAt.Add(timeout)
		case "useful_head", "held_peer_ack":
		default:
			t.Fatalf("unknown feedback control %q", feedback)
		}
		if sequence.lastHeadAckTime != progressAt || fixture.ackedCount != wantAcked {
			t.Fatal("nonadvancing feedback changed the useful head or delivered an unrelated owner")
		}
		time.Sleep(time.Until(start.Add(timeout)))
		synctest.Wait()
		if feedback != "held_peer_ack" && (sequence.ctx.Err() != nil || failureAt.Load() != 0) {
			t.Fatal("useful cumulative progress did not preserve the tail past its original deadline")
		}
		if time.Now().Before(deadline) {
			time.Sleep(time.Until(deadline.Add(-time.Nanosecond)))
			synctest.Wait()
			if sequence.ctx.Err() != nil || failureAt.Load() != 0 {
				t.Fatal("tail expired before one configured interval without useful head progress")
			}
			time.Sleep(time.Nanosecond)
			synctest.Wait()
		}
		if sequence.ctx.Err() == nil || failureAt.Load() != deadline.UnixNano() ||
			failureCount.Load() != uint64(4-wantAcked) || fixture.ackedCount != wantAcked ||
			sequence.resendQueue.Len() != 0 || len(sequence.ackLifetimes.items) != 0 {
			t.Fatalf("tail deadline/custody mismatch: feedback=%s deadline=%s failed_at=%s failed=%d acked=%d context=%v", feedback, deadline.Sub(start), time.Duration(failureAt.Load()-start.UnixNano()), failureCount.Load(), fixture.ackedCount, sequence.ctx.Err())
		}
	})
}
