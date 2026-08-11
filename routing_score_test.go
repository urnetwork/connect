package connect

import "testing"

func TestChallengerWinsHysteresis(t *testing.T) {
	// 10% margin: 105 does not beat 100, 111 does.
	if challengerWins(100, 105, 10) {
		t.Fatal("105 should not beat 100 at 10% hysteresis")
	}
	if !challengerWins(100, 111, 10) {
		t.Fatal("111 should beat 100 at 10% hysteresis")
	}
	// zero hysteresis is plain greater-than (legacy behavior)
	if !challengerWins(100, 100.5, 0) {
		t.Fatal("zero hysteresis must be plain >")
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
