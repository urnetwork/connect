package connect

import "math"

// ExitMetrics is the per-(class,exit) signal snapshot the scorer reads. Every
// field is already collected elsewhere in this file's machinery; the scorer is
// pure and holds no locks.
type ExitMetrics struct {
	RttMillis          float64
	GoodputBytesPerSec float64
	StallEvents        int
	Jitter             float64
	Flows              int
}

// ScoreWeights weights each normalized metric. Per-class weights let the same
// scorer prefer low latency for interactive flows and high throughput for bulk.
type ScoreWeights struct {
	Rtt     float64
	Goodput float64
	Stall   float64
	Jitter  float64
}

func classWeights(c TrafficClass) ScoreWeights {
	switch c {
	case ClassLatency:
		return ScoreWeights{Rtt: 1.0, Goodput: 0.2, Stall: 1.0, Jitter: 0.8}
	case ClassStreaming:
		return ScoreWeights{Rtt: 0.4, Goodput: 0.8, Stall: 1.0, Jitter: 0.9}
	case ClassBulk:
		return ScoreWeights{Rtt: 0.1, Goodput: 1.0, Stall: 0.6, Jitter: 0.1}
	case ClassBrowsing:
		return ScoreWeights{Rtt: 0.6, Goodput: 0.6, Stall: 0.8, Jitter: 0.4}
	case ClassBackground:
		return ScoreWeights{Rtt: 0.2, Goodput: 0.7, Stall: 0.5, Jitter: 0.2}
	default: // ClassUnknown: balanced, class-neutral
		return ScoreWeights{Rtt: 0.5, Goodput: 0.5, Stall: 0.7, Jitter: 0.3}
	}
}

// exitScore returns a composite health score for an exit with the given metrics
// and traffic class weights. The score is bounded and sanitized: inputs are
// clamped to prevent NaN/Inf output, negative values are safe (mapped to 0),
// and non-finite inputs are replaced with a safe finite default. Higher scores
// indicate better exits. The function is pure: no locks, I/O, or time calls.
func exitScore(m ExitMetrics, w ScoreWeights) float64 {
	// Sanitize inputs to ensure output is always finite.
	rtt := m.RttMillis
	if rtt < 0 || math.IsNaN(rtt) || math.IsInf(rtt, 0) {
		rtt = 0
	}
	jitter := m.Jitter
	if jitter < 0 || math.IsNaN(jitter) || math.IsInf(jitter, 0) {
		jitter = 0
	}
	goodput := m.GoodputBytesPerSec
	if goodput < 0 || math.IsNaN(goodput) || math.IsInf(goodput, 0) {
		goodput = 0
	}

	rttGood := 1.0 / (1.0 + rtt/50.0)        // 50ms → 0.5
	jitterGood := 1.0 / (1.0 + jitter/20.0)  // 20ms → 0.5
	goodputGood := goodput / (goodput + 1e6) // 1MB/s → 0.5
	stallPenalty := float64(m.StallEvents) * 0.1
	return w.Rtt*rttGood + w.Goodput*goodputGood + w.Jitter*jitterGood - w.Stall*stallPenalty
}

// challengerWins applies incumbent hysteresis using an absolute-value margin:
// a challenger only displaces the incumbent if it beats it by more than
// hysteresisPct percent of the absolute incumbent value. This ensures the
// threshold moves toward "better" regardless of score sign (positive, negative,
// or zero incumbent). When hysteresisPct==0, reduces to plain greater-than for
// all score signs, which is the legacy behavior and required by existing callers.
func challengerWins(incumbent, challenger, hysteresisPct float64) bool {
	return challenger > incumbent+math.Abs(incumbent)*hysteresisPct/100.0
}
