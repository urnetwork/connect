package lagdrift

// One extender's round trip (lag.dev/DRIFTTEST.md V1): the 10th percentile of
// the run's pings to it, since queueing only ever adds to propagation, and a
// distribution-free standard error from order statistics. Deterministic, with
// no resampling, so the TypeScript model agrees with it exactly.

import (
	"math"
	"slices"
)

const (
	// fewer samples have no estimate
	MinRoundTripSamples = 4
	// the round trip's 95% interval is ±1.96 standard errors
	roundTripZ = 1.96
	// the autocorrelation's inflation of the variance is held to [1, 8]
	maxRoundTripTau = 8
)

// An extender's round trip over a run (V1).
type RoundTrip struct {
	// the 10th percentile, by nearest rank, ms
	Ms float64 `json:"ms"`
	// its standard error, ms
	SigmaMs float64 `json:"sigmaMs"`
	// the samples it is from
	Count int `json:"count"`
	// the lowest sample, ms, for the physical check (V5). An extension of
	// lag.dev/DRIFTTEST.md I3: zero when unknown, and the check then uses Ms.
	MinMs float64 `json:"minMs"`
}

// The 10th percentile of a run's round trips to one extender and its
// distribution-free standard error (V1). samplesMs are in arrival order, which
// the autocorrelation needs. A sample that is not a positive finite number (a
// lost ping) is left out, and false means fewer than 4 remain.
//
// The estimate is the sample of rank ⌈0.1 n⌉. Its 95% interval is
// [x₍ₗ₎, x₍ᵤ₎] with l = ⌊0.1 n − h⌋ and u = ⌈0.1 n + h⌉, where
// h = 1.96 √(n · 0.1 · 0.9 · τ): the binomial's ranks at the effective sample
// size n / τ, mapped back onto the n samples. τ = (1 + ρ) / (1 − ρ), from the
// lag-1 autocorrelation ρ, held to [1, 8]. The standard error is
// (x₍ᵤ₎ − x₍ₗ₎) / (2 · 1.96), with u held to n. When l falls below the first
// rank, the interval's upper half is doubled: (2 (x₍ᵤ₎ − x₍ₖ₎)) / (2 · 1.96).
func EstimateRoundTrip(samplesMs []float64) (RoundTrip, bool) {
	xs := make([]float64, 0, len(samplesMs))
	for _, x := range samplesMs {
		if 0 < x && !math.IsInf(x, 1) {
			xs = append(xs, x)
		}
	}
	n := len(xs)
	if n < MinRoundTripSamples {
		return RoundTrip{}, false
	}
	tau := roundTripTau(xs)

	sorted := slices.Clone(xs)
	slices.Sort(sorted)
	at := func(rank int) float64 {
		return sorted[min(max(rank, 1), n)-1]
	}
	// explicit conversions keep these from fusing into a multiply-add, so the
	// ranks are the ones the TypeScript model computes
	center := float64(float64(n) * RoundTripQuantile)
	// q n is an integer for every n a multiple of 10; the guard keeps its
	// rounding from moving the rank up
	rank := max(int(math.Ceil(center-1e-9)), 1)
	y := at(rank)
	half := float64(roundTripZ * math.Sqrt(float64(n)*RoundTripQuantile*(1-RoundTripQuantile)*tau))
	lo := int(math.Floor(float64(center - half)))
	hi := int(math.Ceil(float64(center + half)))
	var width float64
	if 1 <= lo {
		width = at(hi) - at(lo)
	} else {
		width = 2 * (at(hi) - y)
	}
	return RoundTrip{
		Ms:      y,
		SigmaMs: width / (2 * roundTripZ),
		Count:   n,
		MinMs:   sorted[0],
	}, true
}

// τ = (1 + ρ) / (1 − ρ) from the lag-1 autocorrelation ρ of xs in order, held
// to [1, 8]. Constant samples have ρ = 0.
func roundTripTau(xs []float64) float64 {
	n := len(xs)
	sum := 0.0
	for _, x := range xs {
		sum += x
	}
	mean := sum / float64(n)
	num := 0.0
	den := 0.0
	for i, x := range xs {
		d := x - mean
		den += d * d
		if i+1 < n {
			num += d * (xs[i+1] - mean)
		}
	}
	if !(0 < den) {
		return 1
	}
	rho := num / den
	if 1 <= rho {
		return maxRoundTripTau
	}
	tau := (1 + rho) / (1 - rho)
	return math.Max(1, math.Min(maxRoundTripTau, tau))
}
