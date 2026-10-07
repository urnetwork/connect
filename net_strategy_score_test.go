package connect

// net_strategy_score_test.go — root-cause tests for the per-network,
// delivery-verified, decayed strategy scoring (net_strategy_score.go). Every
// test is deterministic: an injected clock, an injected retain coin or a seeded
// rng, and fixed verdict sequences. No real network and no sleeps.

import (
	mathrand "math/rand"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// newTestScores is a store with an injected clock fixed at `now`, so decay and
// ttl are driven by advancing the returned pointer rather than by wall time,
// and a deterministic retain coin (never retain -> always count a failure) so
// no test depends on the default time-seeded rng. The two retain tests install
// their own coin to exercise the gate.
func newTestScores(now *time.Time) *networkStrategyScores {
	scores := newNetworkStrategyScores()
	scores.now = func() time.Time { return *now }
	scores.retainFailedWinner = func() bool { return false }
	return scores
}

// TestStrategyScoreHandshakeFreezeIsNotCredited is root cause (b): "handshake
// succeeded" is meaningless under russia's 16 KiB freeze -- a path must
// DELIVER. A dial that connected and then froze at 16 KiB is a stall, not a
// success, so it must not be credited; a dial that moved the full 64 KiB is.
// Fail-before: classify the 16 KiB freeze as delivered and the frozen dialer is
// credited above neutral, the assertion that it is not then fails.
func TestStrategyScoreHandshakeFreezeIsNotCredited(t *testing.T) {
	now := time.Now()
	scores := newTestScores(&now)

	// the dial handshook and then froze at 16 KiB: a stall
	scores.recordVerdict("netA", "fragment", classifyDelivery(16*1024, false))
	frozen := scores.weight("netA", "fragment")
	if 1.0 <= frozen {
		t.Fatalf("a handshake that froze at 16 KiB was credited: weight %f, want below neutral 1.0", frozen)
	}

	// a dial that delivered the full 64 KiB with no stall: a success
	scores.recordVerdict("netA", "normal", classifyDelivery(deliveryVerifiedByteCount, false))
	delivered := scores.weight("netA", "normal")
	if delivered <= 1.0 {
		t.Fatalf("a 64 KiB delivery was not credited: weight %f, want above neutral 1.0", delivered)
	}

	if !(frozen < delivered) {
		t.Fatalf("the frozen dial was ranked at or above the delivering one: frozen %f, delivered %f", frozen, delivered)
	}
}

// TestStrategyScoreClassifyDeliveryThreshold pins the 64 KiB rule: anything
// short of it, or stalled, is not a delivery.
func TestStrategyScoreClassifyDeliveryThreshold(t *testing.T) {
	cases := []struct {
		bytes   int64
		stalled bool
		want    strategyVerdict
	}{
		{bytes: deliveryVerifiedByteCount, stalled: false, want: verdictDelivered},
		{bytes: deliveryVerifiedByteCount + 1, stalled: false, want: verdictDelivered},
		{bytes: deliveryVerifiedByteCount - 1, stalled: false, want: verdictStalled},
		{bytes: 16 * 1024, stalled: false, want: verdictStalled},
		{bytes: deliveryVerifiedByteCount, stalled: true, want: verdictStalled},
		{bytes: 0, stalled: false, want: verdictStalled},
	}
	for _, c := range cases {
		if got := classifyDelivery(c.bytes, c.stalled); got != c.want {
			t.Errorf("classifyDelivery(%d, %t) = %d, want %d", c.bytes, c.stalled, got, c.want)
		}
	}
}

// TestStrategyScoreWinnerNotReusedAcrossNetworks is root cause: the score is
// keyed by a client-only network id, so a winner on one network is not reused
// on another. Fail-before: drop the network id from the key and the second
// network reads the first network's winner, so the neutral assertion fails.
func TestStrategyScoreWinnerNotReusedAcrossNetworks(t *testing.T) {
	now := time.Now()
	scores := newTestScores(&now)

	for range 3 {
		scores.recordVerdict("netA", "fragment", verdictDelivered)
	}
	onA := scores.weight("netA", "fragment")
	onB := scores.weight("netB", "fragment")

	if onA <= 1.0 {
		t.Fatalf("the winner on network A was not credited: weight %f", onA)
	}
	if onB != 1.0 {
		t.Fatalf("the network A winner was reused on network B: weight %f, want neutral 1.0", onB)
	}
}

// TestStrategyScoreWinnerDecaysAndExpires is root cause (a): a stale winner
// must lose weight, so a regime change within days is not out-ranked by old
// evidence. The boost decays by the half-life and is dropped entirely past the
// ttl. Fail-before: remove the ttl expiry and the winner is still boosted a day
// later, so the neutral-after-ttl assertion fails.
func TestStrategyScoreWinnerDecaysAndExpires(t *testing.T) {
	start := time.Now()
	now := start
	scores := newTestScores(&now)

	scores.recordVerdict("netA", "fragment", verdictDelivered)
	fresh := scores.weight("netA", "fragment")
	if fresh <= 1.0 {
		t.Fatalf("a fresh winner was not credited: weight %f", fresh)
	}

	now = start.Add(strategyScoreHalfLife)
	decayed := scores.weight("netA", "fragment")
	if !(1.0 < decayed && decayed < fresh) {
		t.Fatalf("a half-life later the winner did not decay: fresh %f, decayed %f (want neutral < decayed < fresh)", fresh, decayed)
	}

	now = start.Add(strategyScoreTtl + time.Hour)
	expired := scores.weight("netA", "fragment")
	if expired != 1.0 {
		t.Fatalf("past the ttl the winner was not expired: weight %f, want neutral 1.0", expired)
	}
}

// TestStrategyScoreFailedWinnerRetainedPerCoin is root cause: a proven winner
// is kept on a failed delivery with probability ~0.5, so one transient stall
// does not flip it. The coin is injected here to pin the logic: heads retains
// (the failure is not counted), tails penalizes.
func TestStrategyScoreFailedWinnerRetainedPerCoin(t *testing.T) {
	now := time.Now()
	scores := newTestScores(&now)
	key := networkScoreKey{networkId: "netA", dialerKey: "fragment"}

	coin := true
	scores.retainFailedWinner = func() bool { return coin }
	setWinner := func() {
		scores.scores[key] = &decayedScore{deliveredWeight: 10, totalWeight: 10, updateTime: now}
	}

	setWinner()
	coin = true
	scores.recordVerdict("netA", "fragment", verdictStalled)
	if total := scores.scores[key].totalWeight; total != 10 {
		t.Fatalf("retain coin heads must keep the winner uncounted: total weight %f, want 10", total)
	}

	setWinner()
	coin = false
	scores.recordVerdict("netA", "fragment", verdictStalled)
	if total := scores.scores[key].totalWeight; total != 11 {
		t.Fatalf("retain coin tails must count the failure: total weight %f, want 11", total)
	}
}

// TestStrategyScoreFailedWinnerRetainedAboutHalf exercises the retain rule with
// the real seeded rng over many trials, so the ~0.5 rate is actually produced,
// not just the logic. A fixed seed makes it reproducible; the band and the
// never-all-one-way check make it meaningful.
func TestStrategyScoreFailedWinnerRetainedAboutHalf(t *testing.T) {
	now := time.Now()
	scores := newTestScores(&now)
	rng := mathrand.New(mathrand.NewSource(1))
	scores.retainFailedWinner = func() bool { return rng.Float64() < scores.retainFailedProbability }
	key := networkScoreKey{networkId: "netA", dialerKey: "fragment"}

	const trials = 1000
	retained := 0
	for range trials {
		// a fresh proven winner each trial
		scores.scores[key] = &decayedScore{deliveredWeight: 10, totalWeight: 10, updateTime: now}
		scores.recordVerdict("netA", "fragment", verdictStalled)
		if scores.scores[key].totalWeight == 10 {
			retained++
		}
	}
	if retained == 0 || retained == trials {
		t.Fatalf("the retain gate never flipped: retained %d of %d", retained, trials)
	}
	if retained < 400 || 600 < retained {
		t.Fatalf("retained %d of %d, want about half", retained, trials)
	}
}

// TestStrategyScoreConfigChangeInvalidates is root cause: a config change (new
// app version, new pushed tactics, the fingerprint kill switch) invalidates
// replay, because a winner learned under the old strategy set must not carry
// into the new one. Fail-before: make setConfigHash not clear and the winner
// survives the change, so the neutral-after assertion fails.
func TestStrategyScoreConfigChangeInvalidates(t *testing.T) {
	now := time.Now()
	scores := newTestScores(&now)
	scores.setConfigHash("config-v1") // baseline, clears nothing

	scores.recordVerdict("netA", "fragment", verdictDelivered)
	if before := scores.weight("netA", "fragment"); before <= 1.0 {
		t.Fatalf("the winner was not credited before the config change: weight %f", before)
	}

	scores.setConfigHash("config-v2")
	if after := scores.weight("netA", "fragment"); after != 1.0 {
		t.Fatalf("the config change did not invalidate the winner: weight %f, want neutral 1.0", after)
	}
}

// TestStrategyScoreReportNeverContainsNetworkId is root cause: the network id
// is client-only and must NEVER appear in any uploaded/emitted field. The
// report buckets by strategy family across all networks; the test records under
// a sentinel network id and asserts neither the raw identifiers, the derived
// id, nor any per-network key appear in the emitted json, and that the report
// is identical whichever network the same outcomes were recorded on.
func TestStrategyScoreReportNeverContainsNetworkId(t *testing.T) {
	now := time.Now()

	// synthetic, locally-administered test identifiers (never real)
	const sentinelBssid = "02:00:00:00:00:01"
	const sentinelMccMnc = "001-01"
	networkId := deriveNetworkId(sentinelBssid, sentinelMccMnc)
	if networkId == "" {
		t.Fatal("derived network id is empty")
	}

	scoresA := newTestScores(&now)
	for range 2 {
		scoresA.recordVerdict(networkId, "fragment", verdictDelivered)
	}
	scoresA.recordVerdict(networkId, "fragment", verdictStalled)
	scoresA.recordVerdict(networkId, "extender tcptls|192.0.2.7", verdictDelivered)

	reportBytes, err := scoresA.report().marshalJson()
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	report := string(reportBytes)

	for _, secret := range []string{sentinelBssid, sentinelMccMnc, networkId, "192.0.2.7"} {
		if strings.Contains(report, secret) {
			t.Fatalf("the emitted report leaked a client-only identifier %q: %s", secret, report)
		}
	}

	// the report must bucket by family only, carrying no address
	for _, family := range scoresA.report().sortedFamilies() {
		if strings.ContainsAny(family, "|") || strings.Contains(family, "192.0.2") {
			t.Fatalf("a report family carried an address: %q", family)
		}
	}

	// the same outcomes under a different network id produce the same report:
	// the network id does not leak into the aggregate
	otherNetworkId := deriveNetworkId("02:00:00:00:00:02", "001-02")
	scoresB := newTestScores(&now)
	for range 2 {
		scoresB.recordVerdict(otherNetworkId, "fragment", verdictDelivered)
	}
	scoresB.recordVerdict(otherNetworkId, "fragment", verdictStalled)
	scoresB.recordVerdict(otherNetworkId, "extender tcptls|192.0.2.7", verdictDelivered)
	otherBytes, err := scoresB.report().marshalJson()
	if err != nil {
		t.Fatalf("marshal other report: %v", err)
	}
	if report != string(otherBytes) {
		t.Fatalf("the report differed by network id:\n A=%s\n B=%s", report, otherBytes)
	}
}

// TestStrategyScoreInjectedPriorBlendsWhenNoEvidence is the server-prior phase
// seam: with no local evidence the scoring blends in an injected prior, so the
// phase that pushes per-country/asn priors can plug in without a server channel
// built here. Local delivery evidence overrides the prior.
func TestStrategyScoreInjectedPriorBlendsWhenNoEvidence(t *testing.T) {
	now := time.Now()
	scores := newTestScores(&now)
	scores.setPrior(func(networkId string, dialerKey string) (float32, bool) {
		if dialerKey == "fragment" {
			return 1.0, true // the phase says fragment is strong here
		}
		return 0, false
	})

	// no local evidence: the prior biases the unknown dialer above neutral
	if primed := scores.weight("netA", "fragment"); primed <= 1.0 {
		t.Fatalf("the injected prior did not bias an unknown dialer: weight %f, want above neutral", primed)
	}
	// a dialer the prior says nothing about stays neutral
	if neutral := scores.weight("netA", "normal"); neutral != 1.0 {
		t.Fatalf("a dialer with no prior and no evidence was not neutral: weight %f", neutral)
	}
	// local delivery evidence moves the score regardless of the prior
	scores.recordVerdict("netA", "normal", verdictDelivered)
	if learned := scores.weight("netA", "normal"); learned <= 1.0 {
		t.Fatalf("local delivery evidence was not credited: weight %f", learned)
	}
}

// TestStrategyRankingBiasedByDeliveryPerNetwork is the integration: a delivery
// outcome recorded through the strategy biases the weighted-shuffle weight of
// that dialer on that network, and not on another. It drives the real
// dialerWeights path, so the wiring from RecordDeliveryOutcome through the
// per-network store into the race is covered, not just the store in isolation.
func TestStrategyRankingBiasedByDeliveryPerNetwork(t *testing.T) {
	now := time.Now()
	settings := DefaultClientStrategySettings()
	fragmentDialer := &clientDialer{description: "fragment", minimumWeight: 0.5, successCount: 1, lastSuccessTime: now, settings: settings}
	normalDialer := &clientDialer{description: "normal", minimumWeight: 0.5, successCount: 1, lastSuccessTime: now, settings: settings}
	scores := newTestScores(&now)
	strategy := &ClientStrategy{
		log:               loggerOrDefault(nil),
		settings:          settings,
		dialers:           map[*clientDialer]bool{fragmentDialer: true, normalDialer: true},
		extenderIpSecrets: map[netip.Addr]string{},
		scores:            scores,
	}

	// baseline: no delivery evidence, so the two race at equal weight
	base := strategy.dialerWeights(false)
	if base[fragmentDialer] != base[normalDialer] {
		t.Fatalf("baseline weights differ without evidence: fragment %f, normal %f", base[fragmentDialer], base[normalDialer])
	}

	// the fragment dialer delivered on the current (unknown) network
	strategy.RecordDeliveryOutcome(strategy.dialerInfo(fragmentDialer), deliveryVerifiedByteCount, false)
	biased := strategy.dialerWeights(false)
	if biased[fragmentDialer] <= biased[normalDialer] {
		t.Fatalf("a delivering dialer was not ranked higher: fragment %f, normal %f", biased[fragmentDialer], biased[normalDialer])
	}

	// on a different network the delivery is not reused: equal again
	strategy.SetNetworkId("02:00:00:00:00:09")
	onOther := strategy.dialerWeights(false)
	if onOther[fragmentDialer] != onOther[normalDialer] {
		t.Fatalf("the delivery bias leaked to another network: fragment %f, normal %f", onOther[fragmentDialer], onOther[normalDialer])
	}
}
