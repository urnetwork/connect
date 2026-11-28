package lagdrift

// Synthetic runs for the fit's tests (lag.dev/DRIFTTEST.md T2): a visitor at
// a known place, extenders around them whose paths each carry one route error
// and one place error drawn for the path, and pings with queueing drawn per
// run. The generators are seeded, so every test sees the same draws.

import (
	"fmt"
	"math"
	"math/rand/v2"
)

// One path: an extender at its listed place, its true place off by the place
// error, and the route error of the path from the visitor, ms.
type syntheticPath struct {
	id           string
	listed       Point
	actual       Point
	placeErrorKm float64
	routeMs      float64
}

// A seeded generator, so a test's draws are the same on every run.
func testRand(seed uint64) *rand.Rand {
	return rand.New(rand.NewPCG(seed, 0x6c616764726966))
}

// A place km away from p at a bearing, on the sphere.
func destination(p Point, bearingDeg float64, km float64) Point {
	lat1 := p.Lat * radPerDeg
	lon1 := p.Lon * radPerDeg
	b := bearingDeg * radPerDeg
	a := km / EarthRadiusKm
	lat2 := math.Asin(math.Sin(lat1)*math.Cos(a) + math.Cos(lat1)*math.Sin(a)*math.Cos(b))
	lon2 := lon1 + math.Atan2(math.Sin(b)*math.Sin(a)*math.Cos(lat1), math.Cos(a)-math.Sin(lat1)*math.Sin(lat2))
	return Point{Lat: lat2 * degPerRad, Lon: wrapLon(lon2 * degPerRad)}
}

// Paths from truth to extenders at the given distances, spread by bearing,
// with errors drawn as the model says: route ~ N(0, (κ K d)²) and a place off
// by N(0, s²) km along the path.
func syntheticPaths(rng *rand.Rand, m Model, truth Point, prefix string, distancesKm []float64, placeErrorKm float64) []syntheticPath {
	paths := []syntheticPath{}
	offset := rng.Float64() * 360
	for i, d := range distancesKm {
		bearing := math.Mod(offset+float64(i)*137.508, 360)
		listed := destination(truth, bearing, d)
		// the true place is off along the path, so the round trip sees it
		actual := destination(truth, bearing, math.Max(1, d+rng.NormFloat64()*placeErrorKm))
		paths = append(paths, syntheticPath{
			id:           fmt.Sprintf("%s%d", prefix, i),
			listed:       listed,
			actual:       actual,
			placeErrorKm: placeErrorKm,
			routeMs:      rng.NormFloat64() * m.Kappa * MsPerKm * d,
		})
	}
	return paths
}

// One run over paths: each ping's round trip is the last mile, the true
// distance, the path's route error and exponential queueing.
func syntheticRun(rng *rand.Rand, truth Point, paths []syntheticPath, run int, lastMileMs float64, queueMs float64, pings int) []Observation {
	obs := []Observation{}
	for _, path := range paths {
		base := lastMileMs + MsPerKm*DistanceKm(truth, path.actual) + path.routeMs
		samples := make([]float64, pings)
		for k := range samples {
			samples[k] = math.Max(0.05, base+rng.ExpFloat64()*queueMs)
		}
		rt, ok := EstimateRoundTrip(samples)
		if !ok {
			continue
		}
		obs = append(obs, Observation{
			ExtenderId:   path.id,
			At:           path.listed,
			PlaceErrorKm: path.placeErrorKm,
			Run:          run,
			RoundTrip:    rt,
		})
	}
	return obs
}

// E2's bands: 4 near, 4 between 300 and 2,500 km, 4 between 2,500 and
// 9,000 km.
var bandDistancesKm = []float64{60, 140, 220, 280, 450, 900, 1500, 2300, 3000, 4500, 6500, 8500}

// The Mahalanobis distance of truth from the fit, in the fit's plane.
func mahalanobis(f *Fit, truth Point) float64 {
	e, n := AzimuthalKm(f.At, truth)
	c := f.Cov
	det := c[0][0]*c[1][1] - c[0][1]*c[1][0]
	q := (e*e*c[1][1] - e*n*(c[0][1]+c[1][0]) + n*n*c[0][0]) / det
	return math.Sqrt(q)
}
