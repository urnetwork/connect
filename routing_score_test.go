package connect

import (
	"math"
	"testing"
)

func TestChallengerWinsHysteresis(t *testing.T) {
	tests := []struct {
		name          string
		incumbent     float64
		challenger    float64
		hysteresisPct float64
		wantWins      bool
	}{
		// Positive incumbent cases (original brief tests)
		{"positive: 105 does not beat 100 at 10%", 100, 105, 10, false},
		{"positive: 111 beats 100 at 10%", 100, 111, 10, true},
		{"positive: zero hysteresis is plain >", 100, 100.5, 0, true},

		// Zero incumbent: hysteresis margin disappears, plain > applies.
		{"zero incumbent: challenger > incumbent", 0, 0.1, 10, true},
		{"zero incumbent: challenger <= incumbent", 0, 0, 10, false},
		{"zero incumbent with 0% hysteresis", 0, 0.1, 0, true},

		// Negative incumbent: the critical bug case.
		// With old formula: incumbent=-5, pct=10 -> threshold=-5.5, so -5.2 "wins" (WRONG).
		// With new formula: incumbent=-5, pct=10 -> threshold=-4.5, so -5.2 rejects, -4 accepts (CORRECT).
		{"negative: -4 beats -5 at 10%", -5, -4, 10, true},
		{"negative: -5.2 does not beat -5 at 10%", -5, -5.2, 10, false},
		{"negative: zero hysteresis is plain >", -5, -4.9, 0, true},
		{"negative: equal does not win", -5, -5, 10, false},

		// Boundary: large negative incumbent with large hysteresis.
		{"large negative: -100 needs 10% to reach -90", -100, -91, 10, false},
		{"large negative: -100 at 10% margin beats -89", -100, -89, 10, true},

		// Extremely close values near zero.
		{"tiny positive challenger beats by margin", 0.001, 0.00111, 10, true},
		{"tiny negative incumbent", -0.001, 0, 10, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := challengerWins(tc.incumbent, tc.challenger, tc.hysteresisPct)
			if got != tc.wantWins {
				t.Errorf("challengerWins(%f, %f, %f) = %v, want %v",
					tc.incumbent, tc.challenger, tc.hysteresisPct, got, tc.wantWins)
			}
		})
	}
}

func TestExitScoreLatencyPrefersLowRtt(t *testing.T) {
	w := classWeights(ClassLatency)
	fast := exitScore(ExitMetrics{RttMillis: 20, GoodputBytesPerSec: 1e6}, w)
	slow := exitScore(ExitMetrics{RttMillis: 200, GoodputBytesPerSec: 1e6}, w)
	if !(fast > slow) {
		t.Fatalf("latency class must rank low-RTT higher: fast=%f slow=%f", fast, slow)
	}
}

func TestExitScoreBulkPrefersGoodput(t *testing.T) {
	w := classWeights(ClassBulk)
	fat := exitScore(ExitMetrics{RttMillis: 80, GoodputBytesPerSec: 5e6}, w)
	thin := exitScore(ExitMetrics{RttMillis: 80, GoodputBytesPerSec: 5e5}, w)
	if !(fat > thin) {
		t.Fatalf("bulk class must rank high-goodput higher: fat=%f thin=%f", fat, thin)
	}
}

func TestExitScoreSanitizesHostileInputs(t *testing.T) {
	w := classWeights(ClassLatency)

	// Baseline: a healthy exit with perfect latency and jitter.
	healthy := exitScore(ExitMetrics{RttMillis: 50, GoodputBytesPerSec: 1e6, Jitter: 20, StallEvents: 0}, w)

	// Assertion 1: Hostile RTT must score WORSE than healthy (not better).
	hostileRttCases := []struct {
		name string
		rtt  float64
	}{
		{"negative RTT", -100},
		{"+Inf RTT", math.Inf(1)},
		{"NaN RTT", math.NaN()},
	}
	for _, tc := range hostileRttCases {
		hostile := exitScore(ExitMetrics{RttMillis: tc.rtt, GoodputBytesPerSec: 1e6, Jitter: 20, StallEvents: 0}, w)
		if !(hostile < healthy) {
			t.Errorf("hostile %s: score=%f should be < healthy=%f", tc.name, hostile, healthy)
		}
		// Assertion 2: Hostile RTT must not win at 10% hysteresis.
		if challengerWins(healthy, hostile, 10) {
			t.Errorf("hostile %s should not win challengerWins against healthy at 10%%, score=%f vs incumbent=%f",
				tc.name, hostile, healthy)
		}
	}

	// Assertion 3: Hostile Jitter must score WORSE than healthy.
	hostileJitterCases := []struct {
		name   string
		jitter float64
	}{
		{"negative Jitter", -50},
		{"+Inf Jitter", math.Inf(1)},
		{"NaN Jitter", math.NaN()},
	}
	for _, tc := range hostileJitterCases {
		hostile := exitScore(ExitMetrics{RttMillis: 50, GoodputBytesPerSec: 1e6, Jitter: tc.jitter, StallEvents: 0}, w)
		if !(hostile < healthy) {
			t.Errorf("hostile %s: score=%f should be < healthy=%f", tc.name, hostile, healthy)
		}
		// Assertion 4: Hostile Jitter must not win at 10% hysteresis.
		if challengerWins(healthy, hostile, 10) {
			t.Errorf("hostile %s should not win challengerWins against healthy at 10%%, score=%f vs incumbent=%f",
				tc.name, hostile, healthy)
		}
	}

	// Assertion 5: Finiteness check (no NaN/Inf output).
	allHostileMetrics := []ExitMetrics{
		{RttMillis: -100, GoodputBytesPerSec: 1e6, Jitter: 0, StallEvents: 0},
		{RttMillis: 50, GoodputBytesPerSec: math.Inf(1), Jitter: 0, StallEvents: 0},
		{RttMillis: 50, GoodputBytesPerSec: 1e6, Jitter: math.NaN(), StallEvents: 0},
		{RttMillis: math.Inf(1), GoodputBytesPerSec: 1e6, Jitter: 0, StallEvents: 0},
		{RttMillis: math.NaN(), GoodputBytesPerSec: 1e6, Jitter: 0, StallEvents: 0},
	}

	for i, m := range allHostileMetrics {
		score := exitScore(m, w)
		if math.IsNaN(score) || math.IsInf(score, 0) {
			t.Errorf("exitScore case %d returned non-finite: %f", i, score)
		}
	}
}
