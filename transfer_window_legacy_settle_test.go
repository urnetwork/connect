package connect

import (
	"testing"
	"time"
)

// A legacy reply permits the configured bootstrap but does not advertise new
// capacity. That bootstrap remains the effective and learned window throughout
// a rising delivery trajectory; only the fresh candidate grows to its ceiling.
// The candidate retains the exact delivery/RTT arithmetic, monotonic trajectory
// and eventual saturation checks without requiring an implicit initial shrink.
// Explicit timestamps keep the whole path independent of host scheduling.
func TestWindowRetainedLegacyBootstrapBoundsGrowingCandidates(t *testing.T) {
	restore := MemoryBudget()
	t.Cleanup(func() { SetMemoryBudget(restore) })
	SetMemoryBudget(0)

	const roundTrip = 50 * time.Millisecond
	const sampleInterval = deliverySizedWindowSampleInterval
	// twenty samples per step spans 200 ms, which is twice the rate window's
	// own minimum span at this round trip, so each step's rate is measured
	// wholly within that step
	const samplesPerStep = 20

	sequence := newEstimatorFixture(t, func(settings *SendBufferSettings) {
		settings.DeliverySizedWindowScale = deliverySizedWindowScale
		settings.TargetGoodputByteRate = targetGoodputByteRate
		// far above the legacy clamp, so the share is not the binding term
		settings.ResendQueueBudget = NewTransferMemoryBudget(mib(64))
	})
	legacyClamp := sequence.sendBufferSettings.ResendQueueMaxByteCount
	floor := sequence.sendBufferSettings.ResendQueueMinByteCount

	// the peer answers, and never carries the field: this is the whole of what
	// makes it a legacy peer
	sequence.observeReceiveWindowAdvertisement(receiveAckMessage{receiveWindowSet: false})

	// a whole second, so every millisecond conversion below is exact
	base := time.Unix(1700000000, 0)
	for i := range 8 {
		receiveTime := base.Add(time.Duration(i) * time.Millisecond)
		sequence.rttWindow.closeSendTime(
			uint64(receiveTime.Add(-roundTrip).UnixMilli()),
			receiveTime,
		)
	}
	if sampled := sequence.rttWindow.estimate(base); sampled.Min != roundTrip {
		t.Fatalf(
			"the fixture's round trip minimum is %s rather than %s, so the delivery arithmetic below is not the arithmetic asserted",
			sampled.Min,
			roundTrip,
		)
	}

	// A delivery rate that rises past what the clamp permits. At this round
	// trip the receiver's default compression also reserves residence. Early
	// candidates are interior or floor-bound; the final three exceed the
	// legacy permission before clamping and must never raise effective bytes.
	perSampleSteps := []ByteCount{
		kib(8), kib(16), kib(32), kib(64), kib(128), kib(256), kib(512), mib(1),
	}

	at := base
	candidates := make([]ByteCount, 0, len(perSampleSteps))
	reachedAt := -1
	for step, perSample := range perSampleSteps {
		for range samplesPerStep {
			at = at.Add(sampleInterval)
			sequence.observeDeliveredBytes(perSample, at)
		}
		estimate := sequence.sendWindowEstimate(at)
		candidates = append(candidates, estimate.CandidateWindow)
		t.Logf(
			"step %d: %d per sample gives window %d candidate %d ceiling %d reason %q targetBound %t sized %t",
			step, perSample, estimate.Window, estimate.CandidateWindow, estimate.Ceiling,
			estimate.Reason, estimate.TargetBound, estimate.Sized,
		)

		if !estimate.Sized {
			t.Errorf(
				"step %d reports an unsized estimate (%q); the delivery term is not acting, so this row is not measuring what it claims",
				step, estimate.Reason,
			)
		}
		if estimate.TargetBound {
			t.Errorf(
				"step %d is bound by the one-gigabit target rather than by the peer branch; this row is about the legacy clamp and a target-clamped step measures the target",
				step,
			)
		}
		if estimate.Ceiling != legacyClamp {
			t.Errorf(
				"step %d resolves a ceiling of %d rather than the legacy clamp %d; against a peer that never advertises the ceiling is this sender's own shipping hold at every point of the trajectory",
				step, estimate.Ceiling, legacyClamp,
			)
		}
		if estimate.Window != legacyClamp || estimate.LearnedWindow != legacyClamp ||
			!sequence.ackSeen.Load() || sequence.receiveWindowSet.Load() {
			t.Errorf("step %d changed the legacy opening or invented an advertisement: %+v", step, estimate)
		}
		if estimate.RoundTrip != roundTrip || estimate.WindowRoundTrip != roundTrip+defaultAckCompressTimeout ||
			int64(estimate.DeliveredByteCount)*sampleInterval.Nanoseconds() != int64(perSample)*estimate.Interval.Nanoseconds() {
			t.Errorf("step %d lost the exact current-phase delivery or RTT evidence: %+v", step, estimate)
		}
		wantCandidate := min(max(2*perSample*ByteCount((roundTrip+defaultAckCompressTimeout)/sampleInterval), floor), legacyClamp)
		if estimate.CandidateWindow != wantCandidate {
			t.Errorf("step %d candidate=%d want=%d from current delivery", step, estimate.CandidateWindow, wantCandidate)
		}
		// The bound, checked at every point and not only at the end. This is
		// the mitigation: the sender never offers a legacy peer more than a
		// receiver of its own generation can hold, so the eviction it could
		// not be told about is never provoked.
		if legacyClamp < estimate.Window {
			t.Errorf(
				"step %d offers a window of %d against a legacy clamp of %d. A legacy receiver cannot confess an eviction, so an overrun here is a silent withdrawal the sender learns of only when its selective acknowledgement timeout expires",
				step, estimate.Window, legacyClamp,
			)
		}
		if estimate.Window < floor {
			t.Errorf("step %d is below the working floor: %d against %d", step, estimate.Window, floor)
		}
		if 0 < step && estimate.CandidateWindow < candidates[step-1] {
			t.Errorf(
				"step %d candidate shrank from %d to %d on a rising delivery rate",
				step, candidates[step-1], estimate.CandidateWindow,
			)
		}
		if reachedAt < 0 && estimate.CandidateWindow == legacyClamp {
			reachedAt = step
		}
		if 0 <= reachedAt && estimate.CandidateWindow != legacyClamp {
			t.Errorf(
				"step %d candidate left the legacy clamp, reading %d against the %d it had reached at step %d",
				step, estimate.CandidateWindow, legacyClamp, reachedAt,
			)
		}
	}

	if reachedAt < 0 {
		t.Errorf(
			"the candidate never reached the legacy clamp %d over a delivery rate rising to %d per sample; it ended at %d",
			legacyClamp,
			perSampleSteps[len(perSampleSteps)-1],
			candidates[len(candidates)-1],
		)
	} else {
		t.Logf(
			"candidate reached the legacy clamp %d from step %d of %d, and held it for the remaining %d steps",
			legacyClamp, reachedAt, len(perSampleSteps), len(perSampleSteps)-1-reachedAt,
		)
	}
	if candidates[0] >= legacyClamp {
		t.Errorf(
			"the candidate trajectory started at %d, already at or above the clamp %d, so it never exercised delivery growth",
			candidates[0], legacyClamp,
		)
	}
}
