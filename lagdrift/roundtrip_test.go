package lagdrift

import (
	"math"
	"testing"
)

func TestEstimateRoundTripNeedsFourSamples(t *testing.T) {
	if _, ok := EstimateRoundTrip([]float64{10, 11, 12}); ok {
		t.Error("three samples have an estimate")
	}
	// lost pings are left out before counting
	if _, ok := EstimateRoundTrip([]float64{10, 11, 12, math.NaN(), -1, 0, math.Inf(1)}); ok {
		t.Error("three valid samples among lost ones have an estimate")
	}
	rt, ok := EstimateRoundTrip([]float64{10, 11, 12, 13})
	if !ok || rt.Count != 4 || rt.Ms != 10 || rt.MinMs != 10 {
		t.Errorf("four samples: %+v, %v", rt, ok)
	}
}

// A steady climb is strongly autocorrelated, so τ is held at 8, and the
// interval's lower rank falls below the first: its upper half is doubled.
//
// n = 20: the estimate is rank ⌈2⌉ = 2, h = 1.96 √(20 · 0.09 · 8) = 7.438, so
// l = ⌊2 − 7.438⌋ < 1 and u = ⌈9.438⌉ = 10, and σ = 2 (10 − 2) / 3.92.
func TestEstimateRoundTripAutocorrelatedFewSamples(t *testing.T) {
	samples := make([]float64, 20)
	for i := range samples {
		samples[i] = float64(i + 1)
	}
	rt, ok := EstimateRoundTrip(samples)
	if !ok {
		t.Fatal("no estimate")
	}
	if rt.Ms != 2 || rt.Count != 20 || rt.MinMs != 1 {
		t.Errorf("estimate %+v", rt)
	}
	if want := 16 / (2 * 1.96); math.Abs(rt.SigmaMs-want) > 1e-12 {
		t.Errorf("σ %.12f, not %.12f", rt.SigmaMs, want)
	}
	if tau := roundTripTau(samples); tau != 8 {
		t.Errorf("τ %.6f, not 8", tau)
	}
}

// Alternating samples are negatively autocorrelated, so τ is 1, and the
// interval has both ends.
//
// n = 100, sorted 1–50 then 101–150: the estimate is rank 10, h = 5.88, so
// l = 4 and u = 16, and σ = (16 − 4) / 3.92.
func TestEstimateRoundTripIndependentManySamples(t *testing.T) {
	samples := []float64{}
	for i := 1; i <= 50; i++ {
		samples = append(samples, float64(i), float64(100+i))
	}
	rt, ok := EstimateRoundTrip(samples)
	if !ok {
		t.Fatal("no estimate")
	}
	if rt.Ms != 10 || rt.Count != 100 {
		t.Errorf("estimate %+v", rt)
	}
	if want := 12 / (2 * 1.96); math.Abs(rt.SigmaMs-want) > 1e-12 {
		t.Errorf("σ %.12f, not %.12f", rt.SigmaMs, want)
	}
	if tau := roundTripTau(samples); tau != 1 {
		t.Errorf("τ %.6f, not 1", tau)
	}
}

// The estimate is the nearest-rank 10th percentile whatever the order, and
// queueing above it doesn't move it.
func TestEstimateRoundTripIsTheTenthPercentile(t *testing.T) {
	rng := testRand(21)
	for trial := 0; trial < 50; trial++ {
		n := 4 + rng.IntN(60)
		samples := make([]float64, n)
		for i := range samples {
			samples[i] = 20 + rng.ExpFloat64()*3
		}
		rt, ok := EstimateRoundTrip(samples)
		if !ok {
			t.Fatal("no estimate")
		}
		below := 0
		for _, x := range samples {
			if x < rt.Ms {
				below += 1
			}
		}
		rank := int(math.Ceil(float64(n)/10 - 1e-9))
		if below != max(rank, 1)-1 {
			t.Errorf("n %d: %d samples below the estimate, rank %d", n, below, rank)
		}
		if !(0 <= rt.SigmaMs) || rt.MinMs > rt.Ms {
			t.Errorf("n %d: %+v", n, rt)
		}
	}
	// constant samples: no spread, no error
	rt, _ := EstimateRoundTrip([]float64{7, 7, 7, 7, 7})
	if rt.Ms != 7 || rt.SigmaMs != 0 {
		t.Errorf("constant samples: %+v", rt)
	}
}
