package connect

import (
	"testing"
	"time"
)

// --- Task 7: the end-to-end chain proof ---
//
// The individual tasks (1-6) each prove their OWN link works in isolation:
// the classifier names a class, exitMetricsSnapshot reads real telemetry,
// the scorer reorders, the reward tap records, the priors move. None of them
// prove the links are actually WIRED TOGETHER -- a classifier that names a
// class nobody reads, or a reward tap that records the wrong exit's outcome,
// would still pass every one of those tests. This file drives the real
// production seams in sequence and checks the OUTPUT OF EACH LINK, not just
// the final state, so a broken link fails its own assertion instead of
// disappearing into an inconclusive final-state check.

// chainProofResult captures each link's observable output from one run of
// runScoredRoutingChain, so the caller can pin down exactly which link
// produced a wrong answer.
type chainProofResult struct {
	resolvedClass   TrafficClass
	incumbent       *multiClientChannel
	challenger      *multiClientChannel
	orderedFront    *multiClientChannel
	rewardLines     []string
	priorsPopulated bool
	winnerBias      float64
}

// runScoredRoutingChain drives the real production seams -- construction-time
// classifier install (maybeInstallLightClassifier, Task 2's real site, not a
// test double), the SAME gate sendPacket's coalesceOrderedClients uses
// (scoredPlacementEnabled + scoredPlacementReorder, ip_remote_multi_client.go
// ~5429), the real per-flow reward tap (recordFlowReward), and the real
// interval fold (foldRewardAndPersist) -- against a synthetic two-exit
// window:
//
//   - incumbent: the legacy front (candidates[0]), no telemetry at all --
//     exitMetricsSnapshot's honest "no data" reading (Task 3's NaN-not-zero
//     convention: RttMillis/Jitter NaN, Goodput/StallEvents 0), which
//     exitScore sanitizes to the WORST sub-score for Rtt/Jitter. Its raw
//     ClassStreaming score is therefore exactly 0.
//   - challenger: real, positive goodput (the same window-stats recipe
//     routing_telemetry_test.go's TestExitMetricsSnapshotGoodputFromWindowStats
//     and routing_reward_tap_test.go's rewardTestGoodClient use) plus two
//     real RTT samples (so both RttMillis and Jitter are real, non-NaN
//     numbers), and zero reconvictions. Its raw score is strictly positive.
//
// With PlacementHysteresisPct left at its zero value (plain greater-than),
// the challenger's positive score against the incumbent's exact 0 is decisive
// on its own -- no load tie-break or hysteresis margin is doing the work,
// only the classifier + telemetry + scorer chain.
func runScoredRoutingChain(t *testing.T, settings *MultiClientSettings) chainProofResult {
	t.Helper()

	parent := &RemoteUserNatMultiClient{settings: settings}
	parent.config.Store(&multiClientConfig{
		serverNameLookup: stubServerNameLookup{names: []string{"netflix.com"}},
	})
	logger := newRecordingLogger()
	parent.log = logger
	store := &fakePriorsStore{loaded: map[string]ProviderPrior{}}
	parent.SetPriorsStore(store)

	// Link 1: construction-time classifier install (Task 2's real call site).
	parent.maybeInstallLightClassifier()

	ipPath := testLightIpPath(IpProtocolTcp, "93.184.216.1", 443)

	incumbentProviderId, challengerProviderId := NewId(), NewId()
	// no telemetry applied: NaN rtt/jitter, 0 goodput, 0 stalls (the honest
	// "no data" reading exitMetricsSnapshot must produce per Task 3).
	incumbent := rewardTestChannel(incumbentProviderId, incumbentProviderId)
	// real goodput + no reconvictions (rewardTestGoodClient, Task 3/5's own
	// fixture), plus real RTT samples so Jitter is also a real number, not NaN.
	challenger := rewardTestGoodClient(challengerProviderId)
	challenger.addSendRttSample(30 * time.Millisecond)
	challenger.addSendRttSample(35 * time.Millisecond)

	candidates := []*multiClientChannel{incumbent, challenger}
	if candidates[0] != incumbent {
		t.Fatal("test fixture bug: the legacy (unscored) order must start with incumbent")
	}

	// classify through the EXACT seam scoredPlacementReorder itself reads
	// (self.flowClassifier), so this assertion reflects what placement
	// actually sees -- not a parallel classification the test computed on
	// its own.
	var classifier FlowClassifier
	if p := parent.flowClassifier.Load(); p != nil {
		classifier = *p
	}
	resolvedClass := classifyOrUnknown(classifier, ipPath, "").Class

	// Link 2: the SAME gate sendPacket's coalesceOrderedClients uses --
	// scoredPlacementEnabled decides whether scoredPlacementReorder (fed by
	// Task 3's real telemetry) ever runs at all. With ScoredPlacement off,
	// this reduces to the untouched legacy order, exactly like production.
	ordered := candidates
	if scoredPlacementEnabled(parent.reliabilitySettings()) {
		ordered = parent.scoredPlacementReorder(ordered, ipPath, "")
	}
	winner := ordered[0]

	// Link 3: the real per-flow tap and its own interval fold -- both gated
	// on RewardInstrumentation at their own call sites, not re-checked here.
	parent.recordFlowReward(ipPath, "", winner, 0)
	parent.foldRewardAndPersist()

	result := chainProofResult{
		resolvedClass: resolvedClass,
		incumbent:     incumbent,
		challenger:    challenger,
		orderedFront:  winner,
		rewardLines:   logger.linesWith("event=reward"),
	}
	if parent.providerPriors != nil {
		if winnerProviderId, ok := providerIdentity(winner); ok {
			result.priorsPopulated = true
			result.winnerBias = parent.providerPriors.Bias(winnerProviderId.String())
		}
	}
	return result
}

// TestScoredRoutingChainEndToEnd is Task 7's chain proof: with every Phase 2
// knob on, a synthetic window must show (1) the classifier naming a real
// class through the real install site, (2) the scorer -- fed by real
// telemetry -- promoting a genuinely-better challenger ahead of the legacy
// front, and (3) the reward tap recording THAT SAME WINNING exit's outcome
// and folding it into that exit's own provider prior, moving it off neutral.
// One connected chain, not three independently-passing pieces: the reward
// step reads back providerIdentity(winner) specifically, so a reward tap
// that recorded the WRONG exit (e.g. always the legacy front, or a stale
// reference) would still fail here even though "some prior somewhere moved"
// would have passed.
//
// Each assertion is anchored to a concrete intermediate value, not the final
// state, so a break in any one of the three named knobs fails a DIFFERENT
// assertion:
//
//   - LightClassifier off: flowClassifier stays nil (maybeInstallLightClassifier
//     is a no-op) -> classifyOrUnknown returns ClassUnknown -> the classify
//     assertion (want ClassStreaming) fails FIRST, before the reorder is
//     even reached.
//   - ScoredPlacement off: scoredPlacementEnabled(...) is false -> this
//     function's own gate (identical to sendPacket's coalesceOrderedClients)
//     never calls scoredPlacementReorder -> ordered stays the untouched
//     legacy order (incumbent still in front) -> the reorder assertion
//     (want challenger in front) fails.
//   - RewardInstrumentation off: recordFlowReward and foldRewardAndPersist
//     are both no-ops at their own gate -> providerPriors stays nil -> the
//     reward/prior assertion fails.
//
// Verified by re-running this exact test with each knob individually forced
// back to false (settings.LightClassifier / settings.ScoredPlacement /
// settings.RewardInstrumentation): each one trips exactly the assertion
// named above and no other -- see the task report for the captured failure
// output.
func TestScoredRoutingChainEndToEnd(t *testing.T) {
	settings := DefaultMultiClientSettings()
	settings.LightClassifier = true
	settings.ScoredPlacement = true
	settings.RewardInstrumentation = true
	// PlacementHysteresisPct, PlacementDemoteConsecutive, and
	// QuarantineReentryRamp are all left at their zero values: this proof is
	// about the three knobs named in the brief, not hysteresis/demotion/
	// re-entry tuning. A zero hysteresis makes challengerWins reduce to
	// plain greater-than, so the challenger's genuine (0 vs positive) score
	// edge is decisive on its own.

	result := runScoredRoutingChain(t, settings)

	// Link 1: classify -- the real classifier, installed through the real
	// site, must resolve the real class.
	if result.resolvedClass != ClassStreaming {
		t.Fatalf("chain link 1 (classify) broken: want ClassStreaming (netflix.com/443 via the light classifier), got %v -- LightClassifier is either off or not actually installed", result.resolvedClass)
	}

	// Link 2: reorder -- the scorer must promote the challenger AHEAD of the
	// legacy front. This is the "scorer picks a different exit than the
	// legacy order" the brief asks for, checked against the concrete
	// candidate identity, not just "order changed somehow".
	if result.orderedFront != result.challenger {
		t.Fatalf("chain link 2 (reorder) broken: want the challenger promoted to front, got the legacy incumbent still in front -- ScoredPlacement is either off or scoredPlacementReorder did not use the real telemetry/classification")
	}

	// Link 3: reward tap + fold -- exactly one reward line, and the SAME
	// exit that won placement must have its provider's prior moved above
	// neutral.
	if len(result.rewardLines) != 1 {
		t.Fatalf("chain link 3 (reward tap) broken: want exactly 1 [rel] event=reward line for the winning exit's flow, got %d: %v", len(result.rewardLines), result.rewardLines)
	}
	if !result.priorsPopulated {
		t.Fatal("chain link 3 (reward tap -> priors) broken: providerPriors was never populated -- RewardInstrumentation is either off or the tap/fold did not run")
	}
	if !(result.winnerBias > 0.5) {
		t.Fatalf("chain link 3 (reward tap -> prior) broken: want the WINNING exit's provider prior above neutral (0.5), got %v", result.winnerBias)
	}
}
