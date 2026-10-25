// Forces the real owner gap between the eviction cut and ACK snapshot.
// The old-carrier withdrawal adds no new ACK state or timer-driven progress.
package connect

import (
	"bytes"
	"reflect"
	"testing"
	"testing/synctest"
	"time"
)

// Setup first applies the genuine SACK. The new gate is installed before
// construction, then armed only when all earlier feedback is quiescent.
func newEvictionWakeFixture(t *testing.T) (*evictionFeedbackFixture, *evictionFeedbackGate) {
	t.Helper()
	beforeSnapshot := newEvictionFeedbackGate()
	f := newEvictionFeedbackFixture(t, false, func(settings *SendBufferSettings) {
		settings.afterTakePendingEvictionsForTest = func(sendSequenceId) {
			beforeSnapshot.park()
		}
	})
	t.Cleanup(beforeSnapshot.release)
	f.wire.forward(f.sack, f.wire.senderIn)
	return f, beforeSnapshot
}

// Called only while Run is held at a channel gate. The item remains exactly
// queued and SACK-held; the retired carrier contributes no pending ACK entry.
func requireEvictionWakeHeldVictim(t *testing.T, f *evictionFeedbackFixture) {
	t.Helper()
	valid := func() bool {
		queue := f.sequence.resendQueue
		queue.stateLock.Lock()
		defer queue.stateLock.Unlock()
		item := queue.sequenceNumberItems[f.victim.pack.SequenceNumber]
		return item != nil && item.messageId == RequireIdFromBytes(f.victim.pack.MessageId) &&
			item.selectiveAcked && time.Now().Before(item.resendTime)
	}()
	if !valid || f.sequence.ackWindow.Pending() ||
		f.wire.sender.SendRecoveryStats().SendEvictionResendCount != 0 {
		t.Fatal("lost-wake premise lacks the exact applied SACK or has unrelated ACK/recovery work")
	}
}

// The prompt result is fixed before the cleanup-only Wake. Reconciliation
// then uses the exact original frames, real receiver ACKs and all pool owners.
func finishEvictionWakeRound(t *testing.T, f *evictionFeedbackFixture, at time.Time, label string) {
	t.Helper()
	synctest.Wait()
	prompt := f.wire.sender.SendRecoveryStats().SendEvictionResendCount == 1 &&
		len(f.wire.senderOut) == 1 && f.pendingCount() == 0
	if time.Now() != at || f.wire.ackedCount != 1 || f.wire.deliveredCount != 1 {
		t.Fatal("withdrawal advanced a timer or fabricated terminal delivery credit")
	}
	if !prompt {
		t.Errorf("%s: exact queued withdrawal lost its wake after the earlier cut", label)
		// This is not feedback and cannot satisfy the already-recorded result.
		// It only lets the failing predecessor reconcile its live retry owner.
		f.sequence.ackWindow.Wake()
		synctest.Wait()
	}
	if f.wire.sender.SendRecoveryStats().SendEvictionResendCount != 1 ||
		len(f.wire.senderOut) != 1 || f.pendingCount() != 0 || time.Now() != at {
		t.Fatal("cleanup wake did not recover the exact retained owner without a timer")
	}
	resent := f.wire.take(f.wire.senderOut)
	if resent.pack == nil || !bytes.Equal(resent.pack.SequenceId, f.victim.pack.SequenceId) ||
		!bytes.Equal(resent.pack.MessageId, f.victim.pack.MessageId) ||
		resent.pack.SequenceNumber != f.victim.pack.SequenceNumber ||
		!reflect.DeepEqual(resent.pack.Frames, f.victim.pack.Frames) {
		t.Fatal("wake recovery changed the original retained identity or payload")
	}
	f.wire.forward(f.wire.receive(f.gap), f.wire.senderIn)
	f.wire.forward(f.wire.receive(resent), f.wire.senderIn)
	if f.wire.deliveredCount != 4 || f.wire.ackedCount != 4 ||
		f.sequence.resendQueue.Len() != 0 || time.Now() != at {
		t.Fatal("wake recovery did not reconcile all original owners at the same virtual time")
	}
	t.Logf("%s: prompt=%t cleanup_only_wake=%t exact_recovery=true terminal_owners=4 timer_advance=0",
		label, prompt, !prompt)
}

// Intended V3-red/V4-green: Snapshot drains the real wake after an empty
// pending cut. Only the independent marker remains to keep Run runnable.
func TestEvictionFeedbackWithdrawalAfterEmptyCutCannotLoseWake(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f, beforeSnapshot := newEvictionWakeFixture(t)
		beforeSnapshot.armed.Store(true)
		f.sequence.ackWindow.Wake()
		beforeSnapshot.requireEntered(t)
		requireEvictionWakeHeldVictim(t, f)
		if state := pendingEvictionStateForTest(f.sequence); len(state.numbers) != 0 ||
			state.pending != 0 || state.inFlight != 0 {
			t.Fatal("lost-wake setup did not capture an empty eviction cut")
		}

		at := time.Now()
		f.wire.forward(f.notice, f.wire.senderIn)
		snapshot := f.sequence.ackWindow.Snapshot(false)
		if f.pendingCount() != 1 || len(snapshot.ackNotify) != 1 ||
			snapshot.ackUpdateCount != 0 || len(snapshot.selectiveAcks) != 0 {
			t.Fatal("retired carrier did not publish only the exact marker and one wake")
		}
		f.afterSnapshot.armed.Store(true)
		beforeSnapshot.release()
		f.afterSnapshot.requireEntered(t)
		requireEvictionWakeHeldVictim(t, f)
		if state := pendingEvictionStateForTest(f.sequence); len(state.numbers) != 1 ||
			state.pending == 0 || state.inFlight != 0 ||
			len(f.sequence.ackWindow.Snapshot(false).ackNotify) != 0 || time.Now() != at {
			t.Fatal("ACK snapshot did not consume the wake while preserving the later exact marker")
		}
		f.afterSnapshot.release()
		finishEvictionWakeRound(t, f, at, "withdrawal between empty cut and snapshot")
	})
}

// Positive predecessor control: the same real withdrawal captured before
// Snapshot must already dispatch on V3, without the new final-wait predicate.
func TestEvictionFeedbackWithdrawalBeforeCutRemainsRunnable(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f, beforeSnapshot := newEvictionWakeFixture(t)
		f.stopAfterSnapshot(t)
		requireEvictionWakeHeldVictim(t, f)
		at := time.Now()
		f.wire.forward(f.notice, f.wire.senderIn)
		beforeSnapshot.armed.Store(true)
		f.afterSnapshot.release()
		beforeSnapshot.requireEntered(t)
		requireEvictionWakeHeldVictim(t, f)
		if state := pendingEvictionStateForTest(f.sequence); len(state.numbers) != 1 ||
			state.pending != 0 || state.inFlight == 0 || time.Now() != at {
			t.Fatal("positive control did not capture the genuine withdrawal before Snapshot")
		}
		beforeSnapshot.release()
		finishEvictionWakeRound(t, f, at, "withdrawal before capture")
	})
}
