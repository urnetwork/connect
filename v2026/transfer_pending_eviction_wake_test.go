// The final wait predicate consumes no marker itself and does not turn the
// helper-only detached transition into a self-waking retry loop.
package connect

import "testing"

func TestPendingEvictionDetachedMarkerWaitsForReattachment(t *testing.T) {
	sequence, items := newPendingEvictionOwners(t, 7, 8)
	sequence.detachResendItem(items[0].messageId)
	sequence.observeEvictions(pendingEvictionAckForTest(7))
	before := pendingEvictionStateForTest(sequence)
	for range 1024 {
		if sequence.pendingEvictionsRunnable() {
			t.Fatal("detached marker made the final wait predicate self-runnable")
		}
	}
	after := pendingEvictionStateForTest(sequence)
	if before.pending != after.pending || before.inFlight != after.inFlight ||
		len(after.numbers) != 1 || !after.detached {
		t.Fatal("read-only final wait predicate consumed or rephased detached custody")
	}
	sequence.addResendItem(items[0])
	if !sequence.pendingEvictionsRunnable() {
		t.Fatal("reattached exact owner did not become runnable")
	}
	sequence.applyPendingEvictions(sequence.takePendingEvictions())
	if sequence.pendingEvictionsRunnable() || items[0].selectiveAcked ||
		items[0].recoveryKind != sendRecoveryEviction {
		t.Fatal("runnable marker did not settle after one ordinary owner cut")
	}
}

// A dirty bit whose exact owner was cumulatively removed costs one harmless
// capture, not an endless continue and not a repair of a replacement owner.
func TestPendingEvictionRetiredDirtyLevelSettlesAfterOneCut(t *testing.T) {
	sequence, items := newPendingEvictionOwners(t, 7)
	sequence.observeEvictions(pendingEvictionAckForTest(7))
	sequence.resendQueue.RemoveByMessageId(items[0].messageId)
	if !sequence.pendingEvictionsRunnable() {
		t.Fatal("pending owner summary disappeared before its normal capture")
	}
	sequence.applyPendingEvictions(sequence.takePendingEvictions())
	if sequence.pendingEvictionsRunnable() ||
		pendingEvictionStateForTest(sequence).inFlight != 0 {
		t.Fatal("retired dirty summary remained self-runnable after its empty owner cut")
	}
}
