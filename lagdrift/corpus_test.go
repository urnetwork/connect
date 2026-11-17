package lagdrift

// A corpus like T3's (lag.dev/DRIFTTEST.md T3), for the fit's acceptance
// (V5): visitors near world cities, 12 extenders chosen as E2 chooses them
// from a pool of cities, and round trips drawn as the model says. The
// generators are seeded, so every run sees the same corpus.

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"
)

// Extender cities, roughly where providers are.
var corpusCities = []Point{
	// North America
	{Lat: 40.71, Lon: -74.01}, {Lat: 34.05, Lon: -118.24}, {Lat: 41.88, Lon: -87.63}, {Lat: 29.76, Lon: -95.37},
	{Lat: 33.45, Lon: -112.07}, {Lat: 39.95, Lon: -75.17}, {Lat: 32.78, Lon: -96.80}, {Lat: 37.77, Lon: -122.42},
	{Lat: 47.61, Lon: -122.33}, {Lat: 39.74, Lon: -104.99}, {Lat: 25.76, Lon: -80.19}, {Lat: 33.75, Lon: -84.39},
	{Lat: 42.36, Lon: -71.06}, {Lat: 44.98, Lon: -93.27}, {Lat: 38.63, Lon: -90.20}, {Lat: 36.17, Lon: -115.14},
	{Lat: 45.50, Lon: -73.57}, {Lat: 43.65, Lon: -79.38}, {Lat: 49.28, Lon: -123.12}, {Lat: 51.05, Lon: -114.07},
	{Lat: 19.43, Lon: -99.13}, {Lat: 20.67, Lon: -103.35}, {Lat: 25.69, Lon: -100.32}, {Lat: 21.16, Lon: -86.85},
	// South and Central America
	{Lat: -23.55, Lon: -46.63}, {Lat: -22.91, Lon: -43.17}, {Lat: -34.60, Lon: -58.38}, {Lat: -33.45, Lon: -70.67},
	{Lat: -12.05, Lon: -77.04}, {Lat: 4.71, Lon: -74.07}, {Lat: 10.48, Lon: -66.90}, {Lat: -0.18, Lon: -78.47},
	{Lat: 9.93, Lon: -84.08}, {Lat: 14.63, Lon: -90.51}, {Lat: 18.47, Lon: -69.90}, {Lat: -15.79, Lon: -47.88},
	// Europe
	{Lat: 51.51, Lon: -0.13}, {Lat: 48.86, Lon: 2.35}, {Lat: 52.52, Lon: 13.40}, {Lat: 50.11, Lon: 8.68},
	{Lat: 52.37, Lon: 4.90}, {Lat: 40.42, Lon: -3.70}, {Lat: 41.39, Lon: 2.17}, {Lat: 45.46, Lon: 9.19},
	{Lat: 41.90, Lon: 12.50}, {Lat: 48.21, Lon: 16.37}, {Lat: 50.08, Lon: 14.44}, {Lat: 52.23, Lon: 21.01},
	{Lat: 59.33, Lon: 18.07}, {Lat: 55.68, Lon: 12.57}, {Lat: 60.17, Lon: 24.94}, {Lat: 59.91, Lon: 10.75},
	{Lat: 53.35, Lon: -6.26}, {Lat: 38.72, Lon: -9.14}, {Lat: 47.50, Lon: 19.04}, {Lat: 44.43, Lon: 26.10},
	{Lat: 37.98, Lon: 23.73}, {Lat: 41.01, Lon: 28.98}, {Lat: 50.45, Lon: 30.52}, {Lat: 55.76, Lon: 37.62},
	{Lat: 43.30, Lon: 5.37}, {Lat: 53.48, Lon: -2.24}, {Lat: 47.38, Lon: 8.54}, {Lat: 50.85, Lon: 4.35},
	// Africa and the Middle East
	{Lat: 30.04, Lon: 31.24}, {Lat: 6.52, Lon: 3.38}, {Lat: -1.29, Lon: 36.82}, {Lat: -26.20, Lon: 28.05},
	{Lat: -33.92, Lon: 18.42}, {Lat: 33.57, Lon: -7.59}, {Lat: 36.75, Lon: 3.06}, {Lat: 5.60, Lon: -0.19},
	{Lat: 25.20, Lon: 55.27}, {Lat: 24.71, Lon: 46.68}, {Lat: 32.09, Lon: 34.78}, {Lat: 35.69, Lon: 51.39},
	// Asia
	{Lat: 35.68, Lon: 139.69}, {Lat: 34.69, Lon: 135.50}, {Lat: 37.57, Lon: 126.98}, {Lat: 31.23, Lon: 121.47},
	{Lat: 39.90, Lon: 116.41}, {Lat: 22.32, Lon: 114.17}, {Lat: 25.03, Lon: 121.57}, {Lat: 1.35, Lon: 103.82},
	{Lat: 13.76, Lon: 100.50}, {Lat: 3.14, Lon: 101.69}, {Lat: -6.21, Lon: 106.85}, {Lat: 14.60, Lon: 120.98},
	{Lat: 10.82, Lon: 106.63}, {Lat: 21.03, Lon: 105.85}, {Lat: 19.08, Lon: 72.88}, {Lat: 28.61, Lon: 77.21},
	{Lat: 12.97, Lon: 77.59}, {Lat: 13.08, Lon: 80.27}, {Lat: 24.86, Lon: 67.01}, {Lat: 23.81, Lon: 90.41},
	{Lat: 43.24, Lon: 76.89}, {Lat: 41.30, Lon: 69.24},
	// Oceania
	{Lat: -33.87, Lon: 151.21}, {Lat: -37.81, Lon: 144.96}, {Lat: -27.47, Lon: 153.03}, {Lat: -31.95, Lon: 115.86},
	{Lat: -34.93, Lon: 138.60}, {Lat: -36.85, Lon: 174.76}, {Lat: -41.29, Lon: 174.78}, {Lat: -43.53, Lon: 172.64},
	{Lat: -17.71, Lon: 178.07}, {Lat: 21.31, Lon: -157.86},
}

// E2's choice from the pool around a place: the 4 nearest, then 4 between 300
// and 2,500 km and 4 between 2,500 and 9,000 km, each spread by bearing (the
// largest smallest gap). A band short of cities takes the nearest outside it.
func corpusExtenders(at Point) []Point {
	type candidate struct {
		p       Point
		d       float64
		bearing float64
	}
	all := []candidate{}
	for _, c := range corpusCities {
		all = append(all, candidate{p: c, d: DistanceKm(at, c), bearing: BearingDeg(at, c)})
	}
	slices.SortFunc(all, func(a candidate, b candidate) int {
		if a.d < b.d {
			return -1
		}
		if b.d < a.d {
			return 1
		}
		return 0
	})
	chosen := []candidate{}
	used := map[int]bool{}
	for i := 0; i < 4; i++ {
		chosen = append(chosen, all[i])
		used[i] = true
	}
	spread := func(minKm float64, maxKm float64) {
		for k := 0; k < 4; k++ {
			best := -1
			bestGap := -1.0
			for i, c := range all {
				if used[i] || c.d < minKm || maxKm < c.d {
					continue
				}
				gap := 360.0
				for _, o := range chosen {
					if minKm <= o.d && o.d <= maxKm {
						g := math.Abs(math.Mod(c.bearing-o.bearing+540, 360) - 180)
						gap = math.Min(gap, g)
					}
				}
				if bestGap < gap {
					best = i
					bestGap = gap
				}
			}
			if best < 0 {
				// the nearest unused beyond the band's start
				for i, c := range all {
					if !used[i] && minKm <= c.d {
						best = i
						break
					}
				}
			}
			if best < 0 {
				return
			}
			used[best] = true
			chosen = append(chosen, all[best])
		}
	}
	spread(300, 2500)
	spread(2500, 9000)
	places := []Point{}
	for _, c := range chosen {
		places = append(places, c.p)
	}
	return places
}

// One corpus visitor: where they are, where their address says, and a run's
// observations.
type corpusVisitor struct {
	truth    Point
	expected Point
	obs      []Observation
	lastMile float64
}

// A visitor near a random pool city, with E2's extenders, route errors drawn
// per path (κ = 0.25 of the distance), places off by up to their accuracy
// radius, and pings with exponential queueing and rare spikes.
func corpusDraw(rng *rand.Rand, runs int) corpusVisitor {
	m := DefaultModel()
	city := corpusCities[rng.IntN(len(corpusCities))]
	truth := destination(city, rng.Float64()*360, rng.Float64()*150)
	expected := destination(truth, rng.Float64()*360, rng.ExpFloat64()*60)
	extenders := corpusExtenders(truth)
	type path struct {
		listed  Point
		actual  Point
		placeKm float64
		routeMs float64
	}
	paths := []path{}
	for _, at := range extenders {
		placeKm := 10 + rng.Float64()*90
		actual := destination(at, rng.Float64()*360, math.Abs(rng.NormFloat64())*placeKm/2)
		d := DistanceKm(truth, actual)
		routeMs := m.Kappa * MsPerKm * d * rng.NormFloat64()
		// a path is never faster than the great circle
		routeMs = math.Max(routeMs, -0.3*MsPerKm*d)
		paths = append(paths, path{listed: at, actual: actual, placeKm: placeKm, routeMs: routeMs})
	}
	lastMile := 1 + rng.Float64()*24
	obs := []Observation{}
	for run := 0; run < runs; run++ {
		runLastMile := lastMile + float64(run)*rng.Float64()*5
		queueMs := 0.5 + rng.Float64()*5
		for i, p := range paths {
			base := runLastMile + MsPerKm*DistanceKm(truth, p.actual) + p.routeMs
			n := 12 + rng.IntN(37)
			samples := make([]float64, n)
			for k := range samples {
				spike := 0.0
				if rng.Float64() < 0.05 {
					spike = 5 + rng.Float64()*40
				}
				samples[k] = base + rng.ExpFloat64()*queueMs + spike
			}
			rt, ok := EstimateRoundTrip(samples)
			if !ok {
				continue
			}
			obs = append(obs, Observation{
				ExtenderId:   string(rune('a' + i)),
				At:           p.listed,
				PlaceErrorKm: p.placeKm,
				Run:          run,
				RoundTrip:    rt,
			})
		}
	}
	return corpusVisitor{truth: truth, expected: expected, obs: obs, lastMile: lastMile}
}

// V5's acceptance on the corpus: the 99% region holds the truth, fits at
// b = 0 included, every start converges, and a warm refit from any fit
// moves it less than 1 km.
func TestFitPositionCorpus(t *testing.T) {
	m := DefaultModel()
	k99 := KForConfidence(0.99)
	for _, c := range []struct {
		runs  int
		fits  int
		seed  uint64
		label string
	}{
		{runs: 1, fits: 300, seed: 201, label: "one run"},
		{runs: 3, fits: 60, seed: 203, label: "three runs"},
	} {
		rng := testRand(c.seed)
		misses := 0
		unconverged := 0
		for i := 0; i < c.fits; i++ {
			v := corpusDraw(rng, c.runs)
			f, err := FitPosition(m, v.expected, v.obs, nil)
			if err != nil {
				t.Fatalf("%s %d: %v", c.label, i, err)
			}
			if k99 < mahalanobis(f, v.truth) {
				misses += 1
			}
			if !f.settled {
				unconverged += 1
			}
			refit, err := FitPosition(m, v.expected, v.obs, f)
			if err != nil {
				t.Fatalf("%s %d: %v", c.label, i, err)
			}
			if moved := DistanceKm(f.At, refit.At); 1 <= moved {
				t.Errorf("%s %d: a warm refit moved %.3f km", c.label, i, moved)
			}
		}
		if 2*c.fits/100 < misses {
			t.Errorf("%s: the 99%% region missed the truth in %d of %d fits", c.label, misses, c.fits)
		}
		if c.fits/100 < unconverged {
			t.Errorf("%s: %d of %d fits didn't converge", c.label, unconverged, c.fits)
		}
	}
}
