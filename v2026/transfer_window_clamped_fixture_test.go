// Explicit delivery and round-trip samples distinguish memory clamps from
// delivery limits without requiring a particular host throughput.
package connect

import (
	"testing"
	"time"
)

// Builds the same budget and other-queue floors as the live clamped fixture.
// All observations share one explicit clock; no transfer goroutine runs.
func newSampledShareWindowFixture(t *testing.T, perSample ByteCount) (*SendSequence, *TransferMemoryBudget, time.Time) {
	t.Helper()
	budget := NewTransferMemoryBudget(384 * 1024)
	sequence := newEstimatorFixture(t, func(settings *SendBufferSettings) {
		settings.DeliverySizedWindowScale = deliverySizedWindowScale
		settings.ResendQueueBudget = budget
		settings.ResendQueueMaxByteCount = 64 * 1024
		settings.ResendQueueMinByteCount = 32 * 1024
	})
	t.Cleanup(func() { sequence.resendQueue.Clear() })
	for range 2 {
		other := newResendQueue(budget, 32*1024)
		t.Cleanup(func() { other.Clear() })
	}
	base := time.Unix(1700000000, 0)
	for index := range 8 {
		received := base.Add(time.Duration(index) * time.Millisecond)
		sequence.rttWindow.closeSendTime(uint64(received.Add(-25*time.Millisecond).UnixMilli()), received)
	}
	sequence.observeReceiveWindowAdvertisement(receiveAckMessage{
		receiveWindowSet:       true,
		receiveWindowByteCount: 16 * 1024 * 1024,
		// This arithmetic fixture models immediate ACKs at a fixed 25 ms RTT.
		ackCompressTimeoutSet: true,
	})
	sequence.receiveWindowSetAtNanos.Store(base.UnixNano())
	at := base
	for range 20 {
		at = at.Add(deliverySizedWindowSampleInterval)
		sequence.observeDeliveredBytes(perSample, at)
	}
	return sequence, budget, at
}

// Other queues' floors reduce the lendable share below the pool total, but
// that share is still the memory bound and must be reported as such.
func TestAClampedWindowReportsTheMemoryShare(t *testing.T) {
	sequence, _, at := newSampledShareWindowFixture(t, 128*1024)
	estimate := sequence.sendWindowEstimate(at)
	if !estimate.Sized || estimate.Window != 320*1024 || estimate.Ceiling != 320*1024 {
		t.Fatalf("sampled share estimate=%+v, want a 320 KiB memory clamp", estimate)
	}
	if estimate.Reason != "the memory budget's share" {
		t.Fatalf("memory-clamped window reason=%q, want the memory budget's share", estimate.Reason)
	}
}

// Delivery grows the configured opening to 251300 bytes. A smaller memory
// share limits admission temporarily without erasing that learned capacity.
func TestALearnedWindowSurvivesTheMemoryClamp(t *testing.T) {
	sequence, budget, at := newSampledShareWindowFixture(t, 50260)
	for _, row := range []struct {
		total  ByteCount
		window ByteCount
		reason string
	}{
		{total: 384 * 1024, window: 251300, reason: "delivery"},
		{total: 192 * 1024, window: 128 * 1024, reason: "the memory budget's share"},
		{total: 384 * 1024, window: 251300, reason: "delivery"},
	} {
		budget.SetTotalByteCount(row.total)
		estimate := sequence.sendWindowEstimate(at)
		if !estimate.Sized || estimate.Window != row.window || estimate.CandidateWindow != row.window || estimate.Reason != row.reason {
			t.Fatalf("total %d: estimate=%+v, want window %d bound by %q", row.total, estimate, row.window, row.reason)
		}
		if estimate.Initial != 64*1024 || estimate.LearnedWindow != 251300 {
			t.Fatalf("total %d: memory clamp changed delivery-grown capacity: %+v", row.total, estimate)
		}
		if estimate.Ceiling != row.total-64*1024 {
			t.Fatalf("total %d: ceiling=%d, want the share after two other floors", row.total, estimate.Ceiling)
		}
	}
}

// Qualified delivery can grow the opening while remaining below even the
// smaller memory share. Permission alone does not teach the unused capacity.
func TestDeliveryGrowthRemainsBelowTheSmallMemoryShare(t *testing.T) {
	sequence, budget, at := newSampledShareWindowFixture(t, 24518)
	budget.SetTotalByteCount(192 * 1024)
	estimate := sequence.sendWindowEstimate(at)
	if !estimate.Sized || estimate.Window != 122590 || estimate.CandidateWindow != 122590 || estimate.LearnedWindow != 122590 || estimate.Ceiling != 131072 || estimate.Reason != "delivery" {
		t.Fatalf("small-share delivery estimate=%+v, want window 122590 below share 131072", estimate)
	}
	if estimate.Initial != 64*1024 || estimate.Window <= estimate.Initial {
		t.Fatalf("delivery did not grow the configured opening: %+v", estimate)
	}
}

// A configured ceiling below the lendable share is a separate bound even
// when both are below the pool's total capacity.
func TestAConfiguredCeilingDoesNotReportTheMemoryShare(t *testing.T) {
	sequence, _, at := newSampledShareWindowFixture(t, 128*1024)
	sequence.sendBufferSettings.DeliverySizedWindowCeilingByteCount = 64 * 1024
	estimate := sequence.sendWindowEstimate(at)
	if !estimate.Sized || estimate.Window != 64*1024 || estimate.Ceiling != 64*1024 {
		t.Fatalf("configured-ceiling estimate=%+v, want 64 KiB", estimate)
	}
	if estimate.Reason == "the memory budget's share" {
		t.Fatalf("configured ceiling was attributed to memory: %+v", estimate)
	}
}
