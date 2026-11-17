package lagdrift

import (
	"errors"
	"math"
	"slices"
	"testing"
)

var chicago = Point{Lat: 41.8781, Lon: -87.6298}

// A single run on E2's geometry places the visitor within the region, and its
// ellipse is a plausible size.
func TestFitPositionSingleRunFindsTheVisitor(t *testing.T) {
	m := DefaultModel()
	rng := testRand(1)
	truth := Point{Lat: 41.862, Lon: -87.842}
	paths := syntheticPaths(rng, m, truth, "e", bandDistancesKm, 25)
	obs := syntheticRun(rng, truth, paths, 1, 7.5, 4.5, 40)
	f, err := FitPosition(m, chicago, obs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.Extenders != 12 || f.Observations != 12 || !slices.Equal(f.Runs, []int{1}) || len(f.LastMileMs) != 1 {
		t.Fatalf("fit shape: %+v", f)
	}
	if d := mahalanobis(f, truth); !(d < 4) {
		t.Errorf("the truth is %.2f σ from the fit (%.1f km)", d, DistanceKm(f.At, truth))
	}
	if !(5 < f.MinorKm && f.MajorKm < 300) {
		t.Errorf("ellipse %.1f × %.1f km", f.MajorKm, f.MinorKm)
	}
	if !(0 <= f.BearingDeg && f.BearingDeg < 180) {
		t.Errorf("bearing %.2f", f.BearingDeg)
	}
	if !(0 < f.LastMileMs[0] && f.LastMileMs[0] < 20) {
		t.Errorf("last mile %.2f ms", f.LastMileMs[0])
	}
	eastKm, northKm := AzimuthalKm(chicago, f.At)
	if f.EastKm != eastKm || f.NorthKm != northKm {
		t.Errorf("offset %.3f, %.3f, not %.3f, %.3f", f.EastKm, f.NorthKm, eastKm, northKm)
	}
}

// The region is calibrated: with noise drawn as the model says, the truth is
// inside the 99% region at least about 99% of the time. The overdispersion
// factor never narrows it, so it may cover more.
func TestFitPositionRegionCoversTheTruth(t *testing.T) {
	m := DefaultModel()
	rng := testRand(2)
	k99 := KForConfidence(0.99)
	k95 := KForConfidence(0.95)
	trials := 300
	inside99 := 0
	inside95 := 0
	for trial := 0; trial < trials; trial++ {
		truth := Point{Lat: -50 + rng.Float64()*110, Lon: -180 + rng.Float64()*360}
		expected := destination(truth, rng.Float64()*360, rng.Float64()*200)
		paths := syntheticPaths(rng, m, truth, "e", bandDistancesKm, 10+rng.Float64()*90)
		obs := syntheticRun(rng, truth, paths, 0, 2+rng.Float64()*20, 1+rng.Float64()*6, 30)
		f, err := FitPosition(m, expected, obs, nil)
		if err != nil {
			t.Fatal(err)
		}
		d := mahalanobis(f, truth)
		if d <= k99 {
			inside99 += 1
		}
		if d <= k95 {
			inside95 += 1
		}
	}
	if share := float64(inside99) / float64(trials); share < 0.97 {
		t.Errorf("the 99%% region covered the truth %.1f%% of the time", 100*share)
	}
	if share := float64(inside95) / float64(trials); share < 0.92 {
		t.Errorf("the 95%% region covered the truth %.1f%% of the time", 100*share)
	}
}

// Pooled runs: one position and one offset per run, and a pooled fit at
// least as tight as each run alone.
func TestFitPositionPoolsRuns(t *testing.T) {
	m := DefaultModel()
	rng := testRand(3)
	truth := Point{Lat: 40.4168, Lon: -3.7038}
	pathsA := syntheticPaths(rng, m, truth, "a", bandDistancesKm, 25)
	pathsB := syntheticPaths(rng, m, truth, "b", bandDistancesKm, 25)
	obsA := syntheticRun(rng, truth, pathsA, 4, 5, 3, 40)
	obsB := syntheticRun(rng, truth, pathsB, 7, 15, 3, 40)
	pooled, err := FitPosition(m, truth, append(slices.Clone(obsA), obsB...), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pooled.Runs, []int{4, 7}) || len(pooled.LastMileMs) != 2 || pooled.Extenders != 24 {
		t.Fatalf("pooled shape: runs %v, offsets %v, extenders %d", pooled.Runs, pooled.LastMileMs, pooled.Extenders)
	}
	// the second run's access link added 10 ms
	if diff := pooled.LastMileMs[1] - pooled.LastMileMs[0]; math.Abs(diff-10) > 3 {
		t.Errorf("the offsets differ by %.2f ms, not about 10", diff)
	}
	single, err := FitPosition(m, truth, obsA, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !(pooled.MajorKm*pooled.MinorKm < single.MajorKm*single.MinorKm) {
		t.Errorf("pooled %.1f × %.1f km is no tighter than one run's %.1f × %.1f km", pooled.MajorKm, pooled.MinorKm, single.MajorKm, single.MinorKm)
	}
	if d := mahalanobis(pooled, truth); !(d < 4) {
		t.Errorf("the truth is %.2f σ from the pooled fit", d)
	}
}

// T2: a repeated extender's route error isn't counted twice, so one run
// repeated three times on the same 12 paths has a wider ellipse than three
// runs on 36 distinct paths.
func TestFitPositionRepeatedPathsShareTheirRouteError(t *testing.T) {
	m := DefaultModel()
	rng := testRand(4)
	truth := Point{Lat: 48.8566, Lon: 2.3522}
	same := syntheticPaths(rng, m, truth, "s", bandDistancesKm, 25)
	repeated := []Observation{}
	distinct := []Observation{}
	for run := 0; run < 3; run++ {
		repeated = append(repeated, syntheticRun(rng, truth, same, run, 6, 3, 40)...)
		paths := syntheticPaths(rng, m, truth, string(rune('x'+run)), bandDistancesKm, 25)
		distinct = append(distinct, syntheticRun(rng, truth, paths, run, 6, 3, 40)...)
	}
	fitRepeated, err := FitPosition(m, truth, repeated, nil)
	if err != nil {
		t.Fatal(err)
	}
	fitDistinct, err := FitPosition(m, truth, distinct, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fitRepeated.Extenders != 12 || fitDistinct.Extenders != 36 {
		t.Fatalf("extenders %d and %d", fitRepeated.Extenders, fitDistinct.Extenders)
	}
	areaRepeated := fitRepeated.MajorKm * fitRepeated.MinorKm
	areaDistinct := fitDistinct.MajorKm * fitDistinct.MinorKm
	if !(areaDistinct*1.5 < areaRepeated) {
		t.Errorf("repeated paths %.0f km², distinct %.0f km²", areaRepeated, areaDistinct)
	}
	// a repeat shrinks only the measurement error, so three repeats shrink the
	// area by less than three independent runs would
	single, err := FitPosition(m, truth, repeated[:12], nil)
	if err != nil {
		t.Fatal(err)
	}
	if !(1.2*single.MajorKm*single.MinorKm/3 < areaRepeated) {
		t.Errorf("one run %.0f km², three repeats %.0f km²", single.MajorKm*single.MinorKm, areaRepeated)
	}
}

// The block covariance: three runs with identical round trips are one run
// whose per-run variance, σ_meas² + σ₀², is divided by 3, since only that part
// averages; the shared route and place error doesn't. The objectives are the
// same function, so the position, the offsets, χ² and the covariance before
// its overdispersion factor agree to the solver's settling.
func TestFitPositionThreeIdenticalRunsAreOneRunWithAThirdOfTheNoise(t *testing.T) {
	m := DefaultModel()
	rng := testRand(5)
	truth := Point{Lat: 35.6762, Lon: 139.6503}
	paths := syntheticPaths(rng, m, truth, "t", bandDistancesKm, 25)
	one := syntheticRun(rng, truth, paths, 0, 6, 3, 40)
	// a detour, so a Huber weight is in play too
	one[3].RoundTrip.Ms += 12
	three := []Observation{}
	for run := 0; run < 3; run++ {
		for _, o := range one {
			o.Run = run
			three = append(three, o)
		}
	}
	third := Model{Kappa: m.Kappa, Sigma0Ms: m.Sigma0Ms / math.Sqrt(3)}
	oneThird := slices.Clone(one)
	for i := range oneThird {
		oneThird[i].RoundTrip.SigmaMs /= math.Sqrt(3)
	}
	f1, err := FitPosition(third, truth, oneThird, nil)
	if err != nil {
		t.Fatal(err)
	}
	f3, err := FitPosition(m, truth, three, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d := DistanceKm(f1.At, f3.At); convergedKm < d {
		t.Errorf("the fits are %.9f km apart", d)
	}
	for r := 0; r < 3; r++ {
		if math.Abs(f3.LastMileMs[r]-f1.LastMileMs[0]) > 0.01 {
			t.Errorf("run %d's offset %.9f, not %.9f", r, f3.LastMileMs[r], f1.LastMileMs[0])
		}
	}
	chi1 := f1.Chi2PerDof * float64(12-3)
	chi3 := f3.Chi2PerDof * float64(36-2-3)
	if math.Abs(chi1-chi3) > 1e-4*math.Max(1, chi1) {
		t.Errorf("χ² %.9f and %.9f", chi1, chi3)
	}
	for i := 0; i < 2; i++ {
		for j := 0; j < 2; j++ {
			c1 := f1.Cov[i][j] / math.Max(1, f1.Chi2PerDof)
			c3 := f3.Cov[i][j] / math.Max(1, f3.Chi2PerDof)
			if math.Abs(c1-c3) > 1e-4*math.Abs(f1.Cov[0][0]) {
				t.Errorf("cov[%d][%d] %.9f and %.9f", i, j, c1, c3)
			}
		}
	}
}

// One detoured path can't drag the position: its Huber weight discounts it.
func TestFitPositionDiscountsADetouredPath(t *testing.T) {
	m := DefaultModel()
	rng := testRand(6)
	truth := Point{Lat: 50.1109, Lon: 8.6821}
	paths := syntheticPaths(rng, m, truth, "d", bandDistancesKm, 10)
	for i := range paths {
		paths[i].routeMs = 0
	}
	clean := syntheticRun(rng, truth, paths, 0, 5, 1, 40)
	cleanFit, err := FitPosition(m, truth, clean, nil)
	if err != nil {
		t.Fatal(err)
	}
	// the nearest extender's path detours by 30 ms in every run
	detoured := slices.Clone(clean)
	detoured[0].RoundTrip.Ms += 30
	detouredFit, err := FitPosition(m, truth, detoured, nil)
	if err != nil {
		t.Fatal(err)
	}
	if moved := DistanceKm(cleanFit.At, detouredFit.At); 3*cleanFit.MajorKm < moved {
		t.Errorf("a detoured path moved the fit %.1f km (σ_major %.1f km)", moved, cleanFit.MajorKm)
	}
}

// A warm start from the last fit lands where a cold start does.
func TestFitPositionWarmStartAgrees(t *testing.T) {
	m := DefaultModel()
	rng := testRand(7)
	truth := Point{Lat: -33.8688, Lon: 151.2093}
	paths := syntheticPaths(rng, m, truth, "w", bandDistancesKm, 25)
	obs := syntheticRun(rng, truth, paths, 2, 9, 3, 40)
	cold, err := FitPosition(m, truth, obs, nil)
	if err != nil {
		t.Fatal(err)
	}
	warm, err := FitPosition(m, truth, obs, cold)
	if err != nil {
		t.Fatal(err)
	}
	if d := DistanceKm(cold.At, warm.At); 0.2 < d {
		t.Errorf("warm and cold fits are %.3f km apart", d)
	}
	if math.Abs(cold.LastMileMs[0]-warm.LastMileMs[0]) > 0.02 {
		t.Errorf("offsets %.4f and %.4f", cold.LastMileMs[0], warm.LastMileMs[0])
	}
	// a warm fit without Runs matches offsets by index
	bare := *cold
	bare.Runs = nil
	warmBare, err := FitPosition(m, truth, obs, &bare)
	if err != nil {
		t.Fatal(err)
	}
	if d := DistanceKm(warmBare.At, warm.At); 1e-9 < d {
		t.Errorf("a warm fit without runs differs by %.6f km", d)
	}
}

// An offset pushed below 0 is held at its bound, and the position's
// covariance is then the 2 × 2 of the rest.
func TestFitPositionHoldsTheOffsetAtZero(t *testing.T) {
	m := DefaultModel()
	rng := testRand(8)
	truth := Point{Lat: 52.52, Lon: 13.405}
	paths := syntheticPaths(rng, m, truth, "z", bandDistancesKm, 10)
	for i := range paths {
		paths[i].routeMs = 0
	}
	obs := syntheticRun(rng, truth, paths, 0, 0, 0.01, 40)
	// every round trip a little short of the distance: b would go negative
	for i := range obs {
		obs[i].RoundTrip.Ms -= 0.8
	}
	f, err := FitPosition(m, truth, obs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.LastMileMs[0] != 0 {
		t.Errorf("the offset is %.6f, not held at 0", f.LastMileMs[0])
	}
	if !(0 < f.MinorKm && f.MajorKm < 500) {
		t.Errorf("ellipse %.1f × %.1f km", f.MajorKm, f.MinorKm)
	}
}

func TestFitPositionNeedsThreeExtenders(t *testing.T) {
	m := DefaultModel()
	rng := testRand(9)
	truth := Point{Lat: 1.3521, Lon: 103.8198}
	paths := syntheticPaths(rng, m, truth, "n", bandDistancesKm[:2], 25)
	obs := syntheticRun(rng, truth, paths, 0, 5, 2, 40)
	if _, err := FitPosition(m, truth, obs, nil); !errors.Is(err, ErrTooFewObservations) {
		t.Errorf("two extenders: %v", err)
	}
	// invalid observations are left out, not fitted
	obs = append(obs, Observation{ExtenderId: "nan", At: Point{Lat: 2, Lon: 104}, Run: 0, RoundTrip: RoundTrip{Ms: math.NaN()}})
	obs = append(obs, Observation{ExtenderId: "far", At: Point{Lat: 95, Lon: 0}, Run: 0, RoundTrip: RoundTrip{Ms: 10}})
	if _, err := FitPosition(m, truth, obs, nil); !errors.Is(err, ErrTooFewObservations) {
		t.Errorf("with invalid observations: %v", err)
	}
	if _, err := FitPosition(m, Point{Lat: math.NaN()}, obs, nil); !errors.Is(err, ErrInvalidExpected) {
		t.Errorf("an invalid expected place: %v", err)
	}
}

// The ellipse of a covariance: axes and the major axis's bearing.
func TestCovEllipse(t *testing.T) {
	cases := []struct {
		cov     [2][2]float64
		major   float64
		minor   float64
		bearing float64
	}{
		// east-west
		{cov: [2][2]float64{{400, 0}, {0, 100}}, major: 20, minor: 10, bearing: 90},
		// north-south
		{cov: [2][2]float64{{100, 0}, {0, 400}}, major: 20, minor: 10, bearing: 0},
		// northeast: variance 400 along 45°, 100 across
		{cov: [2][2]float64{{250, 150}, {150, 250}}, major: 20, minor: 10, bearing: 45},
		// northwest
		{cov: [2][2]float64{{250, -150}, {-150, 250}}, major: 20, minor: 10, bearing: 135},
		// a circle reads east
		{cov: [2][2]float64{{100, 0}, {0, 100}}, major: 10, minor: 10, bearing: 90},
	}
	for _, c := range cases {
		major, minor, bearing := CovEllipse(c.cov)
		if math.Abs(major-c.major) > 1e-9 || math.Abs(minor-c.minor) > 1e-9 || math.Abs(bearing-c.bearing) > 1e-9 {
			t.Errorf("%v: %.6f × %.6f at %.6f°, not %.1f × %.1f at %.1f°", c.cov, major, minor, bearing, c.major, c.minor, c.bearing)
		}
	}
}

// Within 10 km of an extender the smoothed distance curves as I / 10 km, and
// beyond as (I − u uᵀ) / d. smoothingKm is an untyped integer constant, so
// 1 / smoothingKm would be integer division, 0, and lose the curvature there.
func TestDistanceCurvatureNearAnExtender(t *testing.T) {
	for _, d := range []float64{0, 3.5, 9.99} {
		if c := distanceCurvature(d, 0.6, 0.8); c != [2][2]float64{{0.1, 0}, {0, 0.1}} {
			t.Errorf("at %.2f km: %v, not I / 10 km", d, c)
		}
	}
	want := [2][2]float64{{0.032, -0.024}, {-0.024, 0.018}}
	c := distanceCurvature(20, 0.6, 0.8)
	for i := range c {
		for j := range c[i] {
			if math.Abs(c[i][j]-want[i][j]) > 1e-15 {
				t.Errorf("at 20 km: %v, not %v", c, want)
			}
		}
	}
	// the smoothed distance meets the true one at 10 km, value and slope
	if d, slope := smoothedDistanceKm(10); d != 10 || slope != 1 {
		t.Errorf("at 10 km: %v, %v", d, slope)
	}
	if d, slope := smoothedDistanceKm(0); d != 5 || slope != 0 {
		t.Errorf("at the extender: %v, %v", d, slope)
	}
}
