// The candidate policy learns a window from delivery, retains its high-water
// value between quality events, and continues adapting the physical pacer.
package connect

import (
	"testing"
	"time"
)

// Explicit clock samples isolate window policy from host scheduling and the
// shared-service sampler. Permission deliberately exceeds the opening window.
func newRetainedWindowFixture(t *testing.T) (*SendSequence, time.Time) {
	t.Helper()
	sequence := newEstimatorFixture(t, func(settings *SendBufferSettings) {
		settings.DeliverySizedWindowScale = 2
		settings.ResendQueueMinByteCount = 8 * 1024
		settings.ResendQueueMaxByteCount = 64 * 1024
		settings.ResendQueueBudget = NewTransferMemoryBudget(16 * 1024 * 1024)
		settings.TargetGoodputByteRate = 64 * 1024 * 1024
	})
	t.Cleanup(func() { sequence.resendQueue.Clear() })
	at := time.Now().Truncate(time.Second)
	sequence.rttWindow.closeSendTime(uint64(at.Add(-50*time.Millisecond).UnixMilli()), at)
	sequence.observeReceiveWindowAdvertisement(receiveAckMessage{
		receiveWindowSet: true, receiveWindowByteCount: 16 * 1024 * 1024,
		ackCompressTimeoutSet: true,
	})
	sequence.receiveWindowSetAtNanos.Store(at.UnixNano())
	return sequence, at
}

// Enough exact checkpoints replace the whole previous delivery history.
func sampleRetainedWindow(sequence *SendSequence, at time.Time, bytes ByteCount) time.Time {
	for range deliveredBytesRingSize + 4 {
		at = at.Add(deliverySizedWindowSampleInterval)
		sequence.observeDeliveredBytes(bytes, at)
	}
	return at
}

// An advertisement grants room to grow; it supplies no delivered capacity.
func TestWindowRetainedAdvertisementDoesNotLearnMaximum(t *testing.T) {
	sequence, at := newRetainedWindowFixture(t)
	estimate := sequence.sendWindowEstimate(at)
	if estimate.Window != 64*1024 || estimate.Ceiling != 16*1024*1024 {
		t.Fatalf("permission replaced the bootstrap: %+v", estimate)
	}
}

// Real slowdown changes pacing immediately while preserving learned room.
func TestWindowRetainedSlowFeedbackLowersPacingOnly(t *testing.T) {
	sequence, at := newRetainedWindowFixture(t)
	sequence.sendWindowEstimate(at)
	at = sampleRetainedWindow(sequence, at, 128*1024)
	fast := sequence.sendWindowEstimate(at)
	if !fast.Sized || fast.Window <= 64*1024 {
		t.Fatalf("qualified delivery failed to grow the window: %+v", fast)
	}
	at = sampleRetainedWindow(sequence, at, 4*1024)
	slow := sequence.sendWindowEstimate(at)
	if !slow.Sized || slow.Window != fast.Window {
		t.Errorf("ordinary feedback shrank the learned window: fast=%+v slow=%+v", fast, slow)
	}
	if slow.ServiceByteRate >= fast.ServiceByteRate || slow.PacingByteRate >= fast.PacingByteRate {
		t.Errorf("retaining the window froze adaptive pacing: fast=%+v slow=%+v", fast, slow)
	}
	at = sampleRetainedWindow(sequence, at, 256*1024)
	faster := sequence.sendWindowEstimate(at)
	if faster.Window <= fast.Window || faster.PacingByteRate <= slow.PacingByteRate {
		t.Errorf("later capacity failed to grow window and pacing: fast=%+v slow=%+v faster=%+v", fast, slow, faster)
	}
}

// Peer, memory and configured-byte permissions apply independently of the
// retained estimate and do not erase it when the temporary clamp is removed.
func TestWindowRetainedHardBoundsPreserveLearnedCapacity(t *testing.T) {
	for _, bound := range []string{"peer", "memory", "configured"} {
		sequence, at := newRetainedWindowFixture(t)
		at = sampleRetainedWindow(sequence, at, 128*1024)
		learned := sequence.sendWindowEstimate(at).Window
		at = sampleRetainedWindow(sequence, at, 4*1024)
		switch bound {
		case "peer":
			sequence.receiveWindowByteCount.Store(32 * 1024)
		case "memory":
			sequence.resendQueue.Budget().SetTotalByteCount(32 * 1024)
		case "configured":
			sequence.sendBufferSettings.DeliverySizedWindowCeilingByteCount = 32 * 1024
		}
		if estimate := sequence.sendWindowEstimate(at); estimate.Window != 32*1024 {
			t.Errorf("%s hard bound ignored: %+v", bound, estimate)
		}
		switch bound {
		case "peer":
			sequence.receiveWindowByteCount.Store(16 * 1024 * 1024)
		case "memory":
			sequence.resendQueue.Budget().SetTotalByteCount(16 * 1024 * 1024)
		case "configured":
			sequence.sendBufferSettings.DeliverySizedWindowCeilingByteCount = 0
		}
		if estimate := sequence.sendWindowEstimate(at); estimate.Window != learned {
			t.Errorf("%s temporary bound erased learned %d: %+v", bound, learned, estimate)
		}
	}
}

// A rate target limits the pacer; its RTT-derived byte sizing cannot bypass
// the separate window shrink policy.
func TestWindowRetainedTargetChangeDoesNotShrink(t *testing.T) {
	sequence, at := newRetainedWindowFixture(t)
	at = sampleRetainedWindow(sequence, at, 128*1024)
	before := sequence.sendWindowEstimate(at)
	sequence.sendBufferSettings.TargetGoodputByteRate = 128 * 1024
	after := sequence.sendWindowEstimate(at)
	if after.Window != before.Window || after.PacingByteRate >= before.PacingByteRate {
		t.Fatalf("target must cap pacing without shrinking learned bytes: before=%+v after=%+v", before, after)
	}
}

// An expired RTT is missing evidence, not permission to replace a learned
// window with the advertised maximum or the configured bootstrap.
func TestWindowRetainedMissingEvidenceHoldsCapacity(t *testing.T) {
	sequence, at := newRetainedWindowFixture(t)
	at = sampleRetainedWindow(sequence, at, 128*1024)
	before := sequence.sendWindowEstimate(at)
	after := sequence.sendWindowEstimate(at.Add(sequence.sendBufferSettings.RttWindowTimeout + time.Second))
	if after.Window != before.Window {
		t.Fatalf("missing evidence changed learned bytes: before=%+v after=%+v", before, after)
	}
}

// Reading statistics cannot learn an uncommitted high sample that the send
// worker never acted on, or consume a future remeasurement opportunity.
func TestWindowRetainedStatisticsCannotLearnCapacity(t *testing.T) {
	sequence, at := newRetainedWindowFixture(t)
	sequence.sendWindowEstimate(at)
	at = sampleRetainedWindow(sequence, at, 128*1024)
	for range 3 {
		if snapshot := sequence.sendWindowSnapshot(at); snapshot.Window != 64*1024 {
			t.Errorf("statistics changed the admitted window: %+v", snapshot)
		}
	}
	at = sampleRetainedWindow(sequence, at, 4*1024)
	if estimate := sequence.sendWindowEstimate(at); estimate.Window != 64*1024 {
		t.Fatalf("statistics learned delivery that admission did not observe: %+v", estimate)
	}
}

// The target no longer binds admission when the high-water window exceeds
// its current sizing candidate. Campaign metadata must report that distinction.
func TestWindowRetainedTargetMetadataNamesActualBound(t *testing.T) {
	sequence, at := newRetainedWindowFixture(t)
	at = sampleRetainedWindow(sequence, at, 128*1024)
	before := sequence.sendWindowEstimate(at)
	sequence.sendBufferSettings.TargetGoodputByteRate = 128 * 1024
	after := sequence.sendWindowEstimate(at)
	if after.Window != before.Window || after.TargetBound {
		t.Fatalf("retained capacity was mislabeled target-clamped: before=%+v after=%+v", before, after)
	}
}

// A bootstrap with no delivery interval cannot be advertised as a measured
// target-bound window just because a target-derived candidate is available.
func TestWindowRetainedTargetMetadataNeedsQualifiedDelivery(t *testing.T) {
	sequence, at := newRetainedWindowFixture(t)
	if estimate := sequence.sendWindowEstimate(at); estimate.TargetBound {
		t.Fatalf("unqualified target sizing was reported as the admission bound: %+v", estimate)
	}
}

// Keep the positive diagnostic when complete evidence actually grows the
// window to the target's lower bandwidth-delay product.
func TestWindowRetainedTargetMetadataReportsQualifiedBound(t *testing.T) {
	sequence, at := newRetainedWindowFixture(t)
	sequence.sendBufferSettings.TargetGoodputByteRate = 2 * 1024 * 1024
	at = sampleRetainedWindow(sequence, at, 128*1024)
	estimate := sequence.sendWindowEstimate(at)
	if !estimate.Sized || !estimate.TargetBound || estimate.Window <= 64*1024 || estimate.Window >= 1024*1024 {
		t.Fatalf("qualified target-limited growth lost its diagnosis: %+v", estimate)
	}
}
