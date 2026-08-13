package connect

import (
	"math"
	"strings"
	"testing"
	"time"
)

// --- exitMetricsSnapshot: the plumbing ---

// TestExitMetricsSnapshotNoDataIsNotZero pins the trap this whole task exists
// to avoid: exitScore sanitizes a bare 0 RTT/Jitter into the BEST possible
// sub-score (routing_score.go), so a fresh channel with no window activity
// and no timed round trip must NOT report 0 there. It must report NaN, which
// exitScore's existing hostile-input guard already reads as the WORST
// sub-score. Flows passes straight through (it is the caller's own read, not
// this function's job), and Goodput/StallEvents are legitimately 0 for an
// untouched channel -- 0 is the honest, non-exploitable reading for both
// (see exitScore's own comments on why those two need no NaN guard).
func TestExitMetricsSnapshotNoDataIsNotZero(t *testing.T) {
	client := stallTestChannel()

	m := exitMetricsSnapshot(client, 7)
	AssertEqual(t, m.Flows, 7)
	AssertEqual(t, m.GoodputBytesPerSec, float64(0))
	AssertEqual(t, m.StallEvents, 0)
	if !math.IsNaN(m.RttMillis) {
		t.Fatalf("unmeasured RTT must read NaN (exitScore's worst case), got %v", m.RttMillis)
	}
	if !math.IsNaN(m.Jitter) {
		t.Fatalf("unmeasured jitter must read NaN (exitScore's worst case), got %v", m.Jitter)
	}
}

// TestExitMetricsSnapshotNilClientIsSafe: the placement path must never panic
// on a nil candidate. Flows still passes through, and the telemetry fields
// read exactly like the no-data case above.
func TestExitMetricsSnapshotNilClientIsSafe(t *testing.T) {
	m := exitMetricsSnapshot(nil, 3)
	AssertEqual(t, m.Flows, 3)
	AssertEqual(t, m.GoodputBytesPerSec, float64(0))
	AssertEqual(t, m.StallEvents, 0)
	if !math.IsNaN(m.RttMillis) || !math.IsNaN(m.Jitter) {
		t.Fatalf("a nil client must read as unmeasured, got rtt=%v jitter=%v", m.RttMillis, m.Jitter)
	}
}

// TestExitMetricsSnapshotGoodputFromWindowStats proves the GoodputBytesPerSec
// wire: known, deterministic window stats (buckets and byte counts set
// directly, the same technique TestLifetimeJitterRemoveTimeUsesEffectiveLifetime
// uses, so the expected value does not depend on real sleep timing) must
// produce the EXACT value windowStatsWithCoalesce(false).EffectiveByteCountPerSecond()
// itself would compute -- proving exitMetricsSnapshot reads the real accessor
// rather than reimplementing or approximating it.
func TestExitMetricsSnapshotGoodputFromWindowStats(t *testing.T) {
	client := stallTestChannel()

	now := time.Now()
	client.stateLock.Lock()
	// two "settled" buckets spanning 2s, plus two "may be partial" buckets
	// windowStatsWithCoalesce(false) trims off -- this is the exact shape
	// windowStatsWithCoalesce's own doc describes.
	client.eventBuckets = []*multiClientEventBucket{
		{createTime: now.Add(-3 * time.Second), eventTime: now.Add(-3 * time.Second)},
		{createTime: now.Add(-2 * time.Second), eventTime: now.Add(-1 * time.Second)},
		{createTime: now, eventTime: now},
		{createTime: now, eventTime: now},
	}
	client.packetStats.sendAckByteCount = 500000
	client.packetStats.receiveAckByteCount = 500000
	client.stateLock.Unlock()

	wantStats, err := client.windowStatsWithCoalesce(false)
	if err != nil {
		t.Fatalf("windowStatsWithCoalesce: %v", err)
	}
	want := float64(wantStats.EffectiveByteCountPerSecond())
	if want <= 0 {
		t.Fatalf("test fixture is not exercising the goodput path: want %v", want)
	}

	m := exitMetricsSnapshot(client, 0)
	AssertEqual(t, m.GoodputBytesPerSec, want)
}

// TestExitMetricsSnapshotStallEventsFromReconvictions proves the StallEvents
// wire: quarantineReconvictionCount() is this channel's completed
// bench-then-lift cycle count -- a hard send-stall conviction removes the
// channel outright (convictSendStalls), so it is never a placement candidate
// at all, and reconvictions are the chronic-misbehavior signal that DOES
// survive while an exit is still in the window.
func TestExitMetricsSnapshotStallEventsFromReconvictions(t *testing.T) {
	client := stallTestChannel()
	AssertEqual(t, exitMetricsSnapshot(client, 0).StallEvents, 0)

	client.setQuarantined(blackholeNoReceiveAck)
	client.clearQuarantine()
	AssertEqual(t, exitMetricsSnapshot(client, 0).StallEvents, 1)

	client.setQuarantined(blackholeNoReceiveSyn)
	client.clearQuarantine()
	AssertEqual(t, exitMetricsSnapshot(client, 0).StallEvents, 2)
}

// TestExitMetricsSnapshotUsesCoalesceFalseNeverWindowStats is the hard
// constraint from the brief, pinned in source: exitMetricsSnapshot must call
// windowStatsWithCoalesce(false), and must NEVER call WindowStats() (the
// coalescing variant), which perturbs the resize pass's ~15s cadence
// bookkeeping and must not run at new-flow frequency.
func TestExitMetricsSnapshotUsesCoalesceFalseNeverWindowStats(t *testing.T) {
	source, err := readSource("ip_remote_multi_client.go")
	if err != nil {
		t.Fatal(err)
	}
	body, ok := functionBody(source, "func exitMetricsSnapshot(")
	if !ok {
		t.Fatal("could not find exitMetricsSnapshot")
	}
	if !strings.Contains(body, "windowStatsWithCoalesce(false)") {
		t.Error("exitMetricsSnapshot does not call windowStatsWithCoalesce(false)")
	}
	if strings.Contains(body, ".WindowStats()") {
		t.Error("exitMetricsSnapshot calls WindowStats() -- this perturbs the resize pass's cadence bookkeeping and must never run on the placement path")
	}
}

// --- the RTT/jitter EWMA ---

// telemetryTestEwma returns a fresh channel's rttEwmaSnapshot fields for
// readability in the table test below.
func telemetryTestEwma(client *multiClientChannel) (rtt float64, jitter float64, rttOk bool, jitterOk bool) {
	return client.rttEwmaSnapshot()
}

func almostEqual(a, b float64) bool {
	const epsilon = 1e-9
	return math.Abs(a-b) < epsilon
}

// TestAddSendRttSampleEwmaAndJitter hand-verifies the smoothing math: the
// classic 1/8 TCP SRTT weight for the mean, and the matching RTTVAR-style
// mean-absolute-deviation for jitter, seeded (not smoothed) on the very
// first deviation. Every operand here is a multiple of 0.125, so the
// expected values are exact in float64 -- no accumulated rounding to chase.
func TestAddSendRttSampleEwmaAndJitter(t *testing.T) {
	client := stallTestChannel()

	// before any sample: unmeasured
	if _, _, rttOk, jitterOk := telemetryTestEwma(client); rttOk || jitterOk {
		t.Fatal("a fresh channel must report no RTT/jitter measurement")
	}

	// sample 1: 100ms -- seeds the EWMA, no deviation to report yet
	client.addSendRttSample(100 * time.Millisecond)
	rtt, _, rttOk, jitterOk := telemetryTestEwma(client)
	if !rttOk || jitterOk {
		t.Fatalf("after one sample: want rttOk=true jitterOk=false, got rttOk=%v jitterOk=%v", rttOk, jitterOk)
	}
	if !almostEqual(rtt, 100) {
		t.Fatalf("first sample must seed the EWMA exactly: got %v want 100", rtt)
	}

	// sample 2: 140ms -- diff=40, ewma=100+0.125*40=105; jitter seeds at |40|=40
	client.addSendRttSample(140 * time.Millisecond)
	rtt, jitter, rttOk, jitterOk := telemetryTestEwma(client)
	if !rttOk || !jitterOk {
		t.Fatalf("after two samples: want both ok, got rttOk=%v jitterOk=%v", rttOk, jitterOk)
	}
	if !almostEqual(rtt, 105) || !almostEqual(jitter, 40) {
		t.Fatalf("got rtt=%v jitter=%v, want rtt=105 jitter=40", rtt, jitter)
	}

	// sample 3: 90ms -- diff=-15, ewma=105+0.125*(-15)=103.125;
	// jitter=40+0.125*(15-40)=36.875
	client.addSendRttSample(90 * time.Millisecond)
	rtt, jitter, rttOk, jitterOk = telemetryTestEwma(client)
	if !rttOk || !jitterOk {
		t.Fatal("after three samples both must stay ok")
	}
	if !almostEqual(rtt, 103.125) || !almostEqual(jitter, 36.875) {
		t.Fatalf("got rtt=%v jitter=%v, want rtt=103.125 jitter=36.875", rtt, jitter)
	}
}

// TestAddSendRttSampleDropsNonPositiveDurations: clock skew or a synthetic
// zero/negative duration must never be folded in -- exitScore reads a 0 RTT
// as the BEST possible sub-score, so recording a bogus one here would be the
// same trap exitMetricsSnapshot's NaN gating exists to avoid, one layer down.
func TestAddSendRttSampleDropsNonPositiveDurations(t *testing.T) {
	client := stallTestChannel()
	client.addSendRttSample(0)
	client.addSendRttSample(-5 * time.Millisecond)
	if _, _, rttOk, _ := telemetryTestEwma(client); rttOk {
		t.Fatal("a non-positive duration must not count as a measurement")
	}

	client.addSendRttSample(50 * time.Millisecond)
	client.addSendRttSample(-1 * time.Millisecond)
	rtt, _, rttOk, jitterOk := telemetryTestEwma(client)
	if !rttOk || jitterOk {
		t.Fatal("the dropped negative sample must not count as a second sample")
	}
	if !almostEqual(rtt, 50) {
		t.Fatalf("the one real sample must be unaffected by the dropped one: got %v want 50", rtt)
	}
}

// TestExitMetricsSnapshotOneRttSampleHasNoJitterYet: a single round trip has
// no deviation to report, so exitMetricsSnapshot must report a real RttMillis
// but NaN Jitter -- one sample is not enough to fabricate a "0 jitter"
// reading, which would trip the exact same zero-is-best trap as RTT itself.
func TestExitMetricsSnapshotOneRttSampleHasNoJitterYet(t *testing.T) {
	client := stallTestChannel()
	client.addSendRttSample(75 * time.Millisecond)

	m := exitMetricsSnapshot(client, 0)
	if math.IsNaN(m.RttMillis) {
		t.Fatal("one completed round trip must report a real RttMillis")
	}
	if !almostEqual(m.RttMillis, 75) {
		t.Fatalf("got RttMillis=%v want 75", m.RttMillis)
	}
	if !math.IsNaN(m.Jitter) {
		t.Fatalf("one sample has no deviation yet -- Jitter must read NaN, got %v", m.Jitter)
	}
}

// TestSendDetailedWithAckRecordsRttSample is the production-wiring anchor:
// the ack callback that fires on a successful send must feed
// addSendRttSample, or nothing on the real transfer path ever populates the
// EWMA this whole task exists to add. Source-anchored like
// TestLifetimeJitterSourceAnchor and TestDrainBranchWarnsUnconditionally --
// driving this through a real *Client/transport is what the busy-probe suite
// already exists for, and duplicating that harness here would test the
// transport, not this one call.
func TestSendDetailedWithAckRecordsRttSample(t *testing.T) {
	source, err := readSource("ip_remote_multi_client.go")
	if err != nil {
		t.Fatal(err)
	}
	body, ok := functionBody(source, "func (self *multiClientChannel) SendDetailedWithAck(")
	if !ok {
		t.Fatal("could not find SendDetailedWithAck")
	}
	if !strings.Contains(body, "self.addSendRttSample(") {
		t.Error("SendDetailedWithAck's ack callback does not record an RTT sample -- the EWMA never sees real traffic")
	}
}

// --- the zero-RTT trap, end to end ---

// TestExitScoreUnmeasuredDoesNotBeatMeasured is the brief's central
// assertion, at the exitScore level: an exit with a real (even mediocre)
// RTT measurement must outscore one with none, for an RTT-weighted class.
// Before this task, exitMetricsSnapshot left RttMillis at its zero value for
// an unmeasured exit, and exitScore sanitizes a bare 0 into the BEST
// possible sub-score -- so the unmeasured exit would have WON here. The NaN
// convention this task uses instead sanitizes to the WORST sub-score,
// matching exitScore's documented hostile-input handling exactly.
func TestExitScoreUnmeasuredDoesNotBeatMeasured(t *testing.T) {
	unmeasured := stallTestChannel()
	measured := stallTestChannel()
	measured.addSendRttSample(80 * time.Millisecond) // real, but not great

	weights := classWeights(ClassLatency) // heavily RTT-weighted: Rtt=1.0

	scoreUnmeasured := exitScore(exitMetricsSnapshot(unmeasured, 0), weights)
	scoreMeasured := exitScore(exitMetricsSnapshot(measured, 0), weights)

	if scoreUnmeasured >= scoreMeasured {
		t.Fatalf(
			"unmeasured exit (score=%v) must not beat a real 80ms measurement (score=%v) -- the zero-RTT trap",
			scoreUnmeasured, scoreMeasured,
		)
	}

	// and it must read exactly as exitScore's own documented hostile-input
	// case does: the same worst-case contribution a NaN/Inf/negative RTT
	// gets, not some intermediate "neutral" value
	hostile := exitScore(ExitMetrics{RttMillis: math.NaN(), Jitter: math.NaN()}, weights)
	AssertEqual(t, scoreUnmeasured, hostile)
}

// TestScoredPlacementReorderDoesNotPromoteUnmeasuredExitOnRtt is the same
// proof through the real call path: two otherwise-identical candidates (both
// start at 0 flows, so the less-loaded tie-break cannot explain the
// outcome), one with a real RTT sample and one with none. A latency-class
// flow must prefer the MEASURED exit. Before this task's NaN convention, the
// unmeasured exit's fabricated 0ms would have looked better than a real
// measurement and won the reorder instead -- the exact regression this test
// exists to catch.
func TestScoredPlacementReorderDoesNotPromoteUnmeasuredExitOnRtt(t *testing.T) {
	parent, clients := flowCapTestParent(t, 0, 0, 0)
	parent.SetFlowClassifier(fixedClassifier{class: ClassLatency})
	clients[1].addSendRttSample(30 * time.Millisecond)
	ipPath := &IpPath{Version: 4, Protocol: IpProtocolTcp}

	got := parent.scoredPlacementReorder(clients, ipPath, "")
	if len(got) != 2 || got[0] != clients[1] {
		t.Fatalf("the measured exit (clients[1]) must be promoted to the front, not the unmeasured one")
	}
}
