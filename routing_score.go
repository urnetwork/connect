package connect

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

// exitScore is a bounded composite in [0,1]-ish space; higher is better. RTT and
// jitter are inverted (lower is better) via a soft reciprocal so a 0 metric does
// not divide; stall events subtract. Constants are deliberate and unit-tested,
// not tuned here.
func exitScore(m ExitMetrics, w ScoreWeights) float64 {
	rttGood := 1.0 / (1.0 + m.RttMillis/50.0)                          // 50ms → 0.5
	jitterGood := 1.0 / (1.0 + m.Jitter/20.0)                          // 20ms → 0.5
	goodputGood := m.GoodputBytesPerSec / (m.GoodputBytesPerSec + 1e6) // 1MB/s → 0.5
	stallPenalty := float64(m.StallEvents) * 0.1
	return w.Rtt*rttGood + w.Goodput*goodputGood + w.Jitter*jitterGood - w.Stall*stallPenalty
}

// challengerWins applies incumbent hysteresis: a challenger only displaces the
// incumbent if it beats it by more than hysteresisPct percent. hysteresisPct==0
// reduces to a plain greater-than, which is the pre-change behavior.
func challengerWins(incumbent, challenger, hysteresisPct float64) bool {
	return challenger > incumbent*(1.0+hysteresisPct/100.0)
}
