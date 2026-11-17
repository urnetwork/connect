package lagdriftsim

// The study's deterministic generators. Its tables must reproduce exactly, so
// every draw comes from a fixed-seed PCG (CODESTYLE.md, Randomness:
// simulations), keyed by the study's seed, by what it draws for and by whom:
// a visitor, a run, an extender. A draw then doesn't depend on the order the
// workers run in, nor on what else the study changes. A path's route stretch
// is the same whatever κ the model assumes and wherever a run stops, which is
// what lets the study compare those on the same visitors.

import (
	"hash/fnv"
	"math"
	"math/rand/v2"
)

// What a generator draws for: its first key after the seed.
const (
	// an extender's place and its error, per fleet
	drawFleet uint64 = iota + 1
	// a visitor's place, access link and the extenders serving
	drawVisitor
	// the route stretch of the path to one extender
	drawPath
	// tie-breaks in the extender choice
	drawChoice
	// a run's last mile, queue, spikes and drop
	drawRun
	// one session of a run: its opening and its pings
	drawSession
	// the study's scenarios: cities, countries, claims
	drawScenario
)

// A generator for one purpose, keyed by the seed and keys.
func newRand(seed uint64, keys ...uint64) *rand.Rand {
	h := splitMix(seed)
	for _, key := range keys {
		h = splitMix(h ^ key)
	}
	// a simulation's generator: fixed-seed so the study reproduces
	return rand.New(rand.NewPCG(h, splitMix(h^0x6a09e667f3bcc909)))
}

// SplitMix64's finalizer: spreads a key over all 64 bits.
func splitMix(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

// A string as a key.
func stringKey(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}

// A float64 as a key, by its bits.
func floatKey(f float64) uint64 {
	return math.Float64bits(f)
}

// A Gamma(shape, 1) draw: Marsaglia and Tsang's method, boosted for a shape
// below 1.
func gammaDraw(r *rand.Rand, shape float64) float64 {
	if shape < 1 {
		u := r.Float64()
		for u == 0 {
			u = r.Float64()
		}
		return gammaDraw(r, shape+1) * math.Pow(u, 1/shape)
	}
	d := shape - 1.0/3
	c := 1 / math.Sqrt(9*d)
	for {
		x := r.NormFloat64()
		v := 1 + c*x
		if v <= 0 {
			continue
		}
		v = v * v * v
		u := r.Float64()
		if u < 1-0.0331*x*x*x*x {
			return d * v
		}
		if 0 < u && math.Log(u) < 0.5*x*x+d*(1-v+math.Log(v)) {
			return d * v
		}
	}
}
