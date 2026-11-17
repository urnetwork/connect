package lagdrift

import (
	"math"
	"testing"
)

// A box in degrees as one closed ring.
func boxRing(minLat float64, minLon float64, maxLat float64, maxLon float64) []Point {
	return []Point{
		{Lat: minLat, Lon: minLon},
		{Lat: minLat, Lon: maxLon},
		{Lat: maxLat, Lon: maxLon},
		{Lat: maxLat, Lon: minLon},
	}
}

// A fit at a place with a covariance, as the region test reads one.
func fitAt(at Point, cov [2][2]float64) *Fit {
	major, minor, bearing := CovEllipse(cov)
	return &Fit{At: at, Cov: cov, MajorKm: major, MinorKm: minor, BearingDeg: bearing}
}

func circleCov(sigmaKm float64) [2][2]float64 {
	return [2][2]float64{{sigmaKm * sigmaKm, 0}, {0, sigmaKm * sigmaKm}}
}

// A covariance with axes major and minor, the major at a bearing.
func tiltedCov(majorKm float64, minorKm float64, bearingDeg float64) [2][2]float64 {
	// the major axis's direction, east and north
	e := math.Sin(bearingDeg * radPerDeg)
	n := math.Cos(bearingDeg * radPerDeg)
	a := majorKm * majorKm
	b := minorKm * minorKm
	return [2][2]float64{
		{a*e*e + b*n*n, (a - b) * e * n},
		{(a - b) * e * n, a*n*n + b*e*e},
	}
}

// The Mahalanobis distance to the nearest point of the country's boundary,
// by brute force: every edge sampled every few meters, straight in degrees,
// but the cuts along the antimeridian and the poles.
func bruteForceD(f *Fit, c *Country) float64 {
	c00, c01, c11 := f.Cov[0][0], f.Cov[0][1], f.Cov[1][1]
	det := c00*c11 - c01*c01
	best := math.Inf(1)
	for _, polygon := range c.Polygons {
		for _, ring := range polygon {
			for i := range ring {
				a := ring[i]
				b := ring[(i+1)%len(ring)]
				if (a.Lon == 180 && b.Lon == 180) || (a.Lon == -180 && b.Lon == -180) ||
					(a.Lat == 90 && b.Lat == 90) || (a.Lat == -90 && b.Lat == -90) {
					continue
				}
				steps := 2000
				for s := 0; s <= steps; s++ {
					q := Point{
						Lat: a.Lat + (b.Lat-a.Lat)*float64(s)/float64(steps),
						Lon: a.Lon + (b.Lon-a.Lon)*float64(s)/float64(steps),
					}
					e, n := AzimuthalKm(f.At, q)
					m2 := (e*e*c11 - 2*e*n*c01 + n*n*c00) / det
					best = math.Min(best, math.Sqrt(m2))
				}
			}
		}
	}
	return best
}

var squareCountry = &Country{Code: "zz", Polygons: [][][]Point{{boxRing(0, 0, 2, 2)}}}

func TestKForConfidence(t *testing.T) {
	cases := []struct {
		confidence float64
		k          float64
	}{
		{confidence: 0.95, k: 2.4477},
		{confidence: 0.99, k: 3.0349},
		{confidence: 0.999, k: 3.7169},
	}
	for _, c := range cases {
		if k := KForConfidence(c.confidence); math.Abs(k-c.k) > 1e-4 {
			t.Errorf("%.3f: k %.6f, not %.4f", c.confidence, k, c.k)
		}
	}
	if KForConfidence(0) != 0 || !math.IsInf(KForConfidence(1), 1) {
		t.Error("the bounds")
	}
}

// Inside, an overlapping region, and a region clear of the country (V6).
func TestJudgeInsideOverlapsOutside(t *testing.T) {
	inside := Judge(fitAt(Point{Lat: 1, Lon: 1}, circleCov(30)), squareCountry, 0.99)
	if !inside.Inside || !inside.Overlaps || inside.DistanceKm != 0 || !(inside.MarginKm < 0) {
		t.Errorf("inside: %+v", inside)
	}
	// 1° from the edge is 111 km: D = 111 / 30
	if math.Abs(inside.D-111.2/30) > 0.05 {
		t.Errorf("inside D %.4f", inside.D)
	}
	// half a degree west of the west edge, about 56 km, and σ = 30 km: D ≈ 1.85
	near := Judge(fitAt(Point{Lat: 1, Lon: -0.5}, circleCov(30)), squareCountry, 0.99)
	if near.Inside || !near.Overlaps || !(near.D < near.K) || !(near.MarginKm <= 0) {
		t.Errorf("overlapping: %+v", near)
	}
	if math.Abs(near.DistanceKm-55.6) > 0.2 || math.Abs(near.SignificanceKm-30*near.K) > 0.01 {
		t.Errorf("overlapping figures: %+v", near)
	}
	// 3° west, about 333 km
	far := Judge(fitAt(Point{Lat: 1, Lon: -3}, circleCov(30)), squareCountry, 0.99)
	if far.Inside || far.Overlaps || !(far.D > far.K) || !(far.MarginKm > 0) {
		t.Errorf("outside: %+v", far)
	}
	if math.Abs(far.MarginKm-(far.DistanceKm-far.SignificanceKm)) > 1e-9 {
		t.Errorf("margin %.9f is not distance − significance", far.MarginKm)
	}
}

// T2: margin_km > 0 exactly when D > k, for tilted ellipses too, and D and
// the significance are the brute-force ones.
func TestJudgeMarginIsPositiveExactlyWhenDExceedsK(t *testing.T) {
	rng := testRand(31)
	for trial := 0; trial < 200; trial++ {
		at := Point{Lat: -2 + rng.Float64()*6, Lon: -2 + rng.Float64()*6}
		major := 5 + rng.Float64()*120
		minor := major * (0.2 + 0.8*rng.Float64())
		f := fitAt(at, tiltedCov(major, minor, rng.Float64()*180))
		confidence := []float64{0.95, 0.99, 0.999}[trial%3]
		j := Judge(f, squareCountry, confidence)
		if j.Inside != squareCountry.Contains(at) {
			t.Fatalf("inside %v at %v", j.Inside, at)
		}
		if !j.Inside && (0 < j.MarginKm) != (j.K < j.D) {
			t.Errorf("margin %.6f with D %.6f and k %.6f", j.MarginKm, j.D, j.K)
		}
		if !j.Inside && j.Overlaps != (j.D <= j.K) {
			t.Errorf("overlaps %v with D %.6f and k %.6f", j.Overlaps, j.D, j.K)
		}
		if want := bruteForceD(f, squareCountry); math.Abs(j.D-want) > 1e-3*math.Max(1, want) {
			t.Errorf("D %.6f, brute force %.6f", j.D, want)
		}
		// the radius along the line to q: k / √(uᵀΣ⁻¹u)
		if !j.Inside && 0 < j.DistanceKm {
			search := newBoundarySearch(f.At, f.Cov)
			for _, ring := range squareCountry.Polygons[0] {
				search.ring(ring)
			}
			qe, qn := search.nearestKm()
			ue := qe / math.Hypot(qe, qn)
			un := qn / math.Hypot(qe, qn)
			c := f.Cov
			det := c[0][0]*c[1][1] - c[0][1]*c[1][0]
			quad := (ue*ue*c[1][1] - 2*ue*un*c[0][1] + un*un*c[0][0]) / det
			if want := j.K / math.Sqrt(quad); math.Abs(j.SignificanceKm-want) > 1e-6*want {
				t.Errorf("significance %.9f, not %.9f", j.SignificanceKm, want)
			}
		}
	}
}

// A long thin ellipse is clear of a border its long axis runs along, and
// reaches one its long axis points at.
func TestJudgeTiltedEllipseClearOfTheBorder(t *testing.T) {
	// 1.5° west of the square's west edge, about 167 km, with the ellipse long
	// north-south: 300 km by 20 km
	f := fitAt(Point{Lat: 1, Lon: -1.5}, tiltedCov(300, 20, 0))
	j := Judge(f, squareCountry, 0.99)
	if j.Overlaps || !(0 < j.MarginKm) {
		t.Errorf("a north-south ellipse 167 km west of the square: %+v", j)
	}
	// turned east-west it reaches the square
	g := fitAt(Point{Lat: 1, Lon: -1.5}, tiltedCov(300, 20, 90))
	if j := Judge(g, squareCountry, 0.99); !j.Overlaps || 0 < j.MarginKm {
		t.Errorf("an east-west ellipse 167 km west of the square: %+v", j)
	}
}

// A concave border: the fit in a U's notch is nearest the notch's sides, not
// its outline's far edges.
func TestJudgeConcaveBorder(t *testing.T) {
	u := &Country{Code: "zu", Polygons: [][][]Point{{{
		{Lat: 0, Lon: 0}, {Lat: 0, Lon: 3}, {Lat: 3, Lon: 3}, {Lat: 3, Lon: 2},
		{Lat: 1, Lon: 2}, {Lat: 1, Lon: 1}, {Lat: 3, Lon: 1}, {Lat: 3, Lon: 0},
	}}}}
	// the notch's middle: half a degree, about 56 km, from its sides
	at := Point{Lat: 2, Lon: 1.5}
	f := fitAt(at, circleCov(25))
	j := Judge(f, u, 0.99)
	if j.Inside || !j.Overlaps {
		t.Errorf("in the notch: %+v", j)
	}
	if math.Abs(j.DistanceKm-55.6) > 0.5 {
		t.Errorf("the notch's sides are %.2f km away", j.DistanceKm)
	}
	if want := bruteForceD(f, u); math.Abs(j.D-want) > 1e-3*want {
		t.Errorf("D %.6f, brute force %.6f", j.D, want)
	}
	// a tight fit in the notch is clear of the country
	if j := Judge(fitAt(at, circleCov(10)), u, 0.99); j.Overlaps {
		t.Errorf("a tight fit in the notch: %+v", j)
	}
}

// An island of the country inside the region counts as touching, though the
// mainland is far.
func TestJudgeIslandInsideTheRegion(t *testing.T) {
	c := &Country{Code: "zi", Polygons: [][][]Point{
		{boxRing(10, 10, 14, 14)},
		// a 2 km island
		{boxRing(0, 0.4, 0.02, 0.42)},
	}}
	f := fitAt(Point{Lat: 0, Lon: 0}, circleCov(30))
	j := Judge(f, c, 0.99)
	if j.Inside || !j.Overlaps || !(j.D < 1.6) {
		t.Errorf("an island 45 km away: %+v", j)
	}
	// without the island, the region is clear of the mainland
	if j := Judge(f, &Country{Code: "zi", Polygons: c.Polygons[:1]}, 0.99); j.Overlaps {
		t.Errorf("the mainland alone: %+v", j)
	}
}

// A hole: a fit in it is outside the country, and its edge is the hole's.
func TestJudgeHole(t *testing.T) {
	c := &Country{Code: "zh", Polygons: [][][]Point{{boxRing(0, 0, 10, 10), boxRing(4, 4, 6, 6)}}}
	center := Point{Lat: 5, Lon: 5}
	if c.Contains(center) {
		t.Error("the hole's center is in the country")
	}
	if !c.Contains(Point{Lat: 2, Lon: 2}) {
		t.Error("the land around the hole is not")
	}
	// the hole's edge is a degree, about 111 km, from its center
	tight := Judge(fitAt(center, circleCov(20)), c, 0.99)
	if tight.Inside || tight.Overlaps || math.Abs(tight.DistanceKm-111) > 2 {
		t.Errorf("a tight fit in the hole: %+v", tight)
	}
	loose := Judge(fitAt(center, circleCov(60)), c, 0.99)
	if loose.Inside || !loose.Overlaps {
		t.Errorf("a loose fit in the hole: %+v", loose)
	}
}

// A country split at the antimeridian, as Natural Earth splits them: a fit
// across the line is near, not half the earth away.
func TestJudgeAcrossTheAntimeridian(t *testing.T) {
	c := &Country{Code: "zf", Polygons: [][][]Point{
		{boxRing(-1, 179, 1, 180)},
		{boxRing(-1, -180, 1, -179)},
	}}
	// half a degree east of the eastern part, about 56 km
	f := fitAt(Point{Lat: 0, Lon: -178.5}, circleCov(25))
	j := Judge(f, c, 0.99)
	if j.Inside || !j.Overlaps || math.Abs(j.DistanceKm-55.6) > 0.5 {
		t.Errorf("east of the antimeridian: %+v", j)
	}
	// half a degree west of the western part
	g := fitAt(Point{Lat: 0, Lon: 178.5}, circleCov(25))
	if j := Judge(g, c, 0.99); j.Inside || math.Abs(j.DistanceKm-55.6) > 0.5 {
		t.Errorf("west of the antimeridian: %+v", j)
	}
	// on the line itself, inside both halves' span
	for _, lon := range []float64{179.5, -179.5} {
		if j := Judge(fitAt(Point{Lat: 0.5, Lon: lon}, circleCov(25)), c, 0.99); !j.Inside {
			t.Errorf("inside at %.1f: %+v", lon, j)
		}
	}
}

// Natural Earth's cuts are not borders (lag.dev/DRIFTTEST.md V6, V10): a fit
// inside a country beside its cut at the antimeridian, or near the pole that
// closes Antarctica, is as far from the boundary as from the real edges, and
// the clear stop holds there.
func TestJudgeSkipsNaturalEarthsCuts(t *testing.T) {
	split := &Country{Code: "zf", Polygons: [][][]Point{
		{boxRing(-1, 179, 1, 180)},
		{boxRing(-1, -180, 1, -179)},
	}}
	// closed along the pole, as Natural Earth draws Antarctica
	polar := &Country{Code: "zq", Polygons: [][][]Point{{{
		{Lat: -70, Lon: -180}, {Lat: -68, Lon: -90}, {Lat: -72, Lon: 0}, {Lat: -66, Lon: 90},
		{Lat: -70, Lon: 180}, {Lat: -90, Lon: 180}, {Lat: -90, Lon: -180},
	}}}}
	for _, c := range []struct {
		name    string
		country *Country
		at      Point
		sigmaKm float64
		// the nearest real edge
		nearest Point
	}{
		{name: "west of the cut", country: split, at: Point{Lat: 0, Lon: 179.9}, sigmaKm: 5, nearest: Point{Lat: 0, Lon: 179}},
		{name: "east of the cut", country: split, at: Point{Lat: 0, Lon: -179.9}, sigmaKm: 5, nearest: Point{Lat: 0, Lon: -179}},
		{name: "near the pole", country: polar, at: Point{Lat: -89, Lon: 0}, sigmaKm: 20, nearest: Point{Lat: -72, Lon: 0}},
	} {
		f := fitAt(c.at, circleCov(c.sigmaKm))
		j := Judge(f, c.country, 0.99)
		wantKm := DistanceKm(c.at, c.nearest)
		if !j.Inside || math.Abs(j.D*c.sigmaKm-wantKm) > 0.01*wantKm {
			t.Errorf("%s: D %.3f, %.1f km from the boundary, not %.1f: %+v", c.name, j.D, j.D*c.sigmaKm, wantKm, j)
		}
		if brute := bruteForceD(f, c.country); math.Abs(j.D-brute) > 1e-3*brute {
			t.Errorf("%s: D %.6f, brute force %.6f", c.name, j.D, brute)
		}
		history := []Snapshot{{AtMs: StopEarliestMs, Fit: f, RoundTrips: []int{12, 12, 12, 12, 12, 12, 12, 12}}}
		if reason, ok := EvaluateStop(history, 0, StopEarliestMs, StopEarliestMs, c.country, BrowserStopTolerance); !ok || reason != StopClear {
			t.Errorf("%s: stop %q %v, not clear", c.name, reason, ok)
		}
	}
	// a fit outside still finds the edge across the line
	if j := Judge(fitAt(Point{Lat: 0, Lon: -178.5}, circleCov(25)), split, 0.99); j.Inside || math.Abs(j.DistanceKm-55.6) > 0.5 {
		t.Errorf("outside across the line: %+v", j)
	}
}

// A country holding the fit's antipode wraps around the azimuthal plane's
// center, but inside is decided in degrees: a fit at Shanghai is not in a
// country around its antipode in Argentina.
func TestJudgeAtTheAntipode(t *testing.T) {
	shanghai := Point{Lat: 31.23, Lon: 121.47}
	antipodal := &Country{Code: "za", Polygons: [][][]Point{{boxRing(-35, -62, -27, -55)}}}
	f := fitAt(shanghai, circleCov(50))
	j := Judge(f, antipodal, 0.99)
	if j.Inside || j.Overlaps {
		t.Errorf("Shanghai against its antipode: %+v", j)
	}
	if !(19000 < j.DistanceKm && j.DistanceKm < 20100) {
		t.Errorf("the antipode's country is %.0f km away", j.DistanceKm)
	}
}

// A ring that repeats its first point is the same ring.
func TestJudgeClosedRing(t *testing.T) {
	ring := boxRing(0, 0, 2, 2)
	closed := &Country{Code: "zc", Polygons: [][][]Point{{append(ring, ring[0])}}}
	f := fitAt(Point{Lat: 1, Lon: -0.7}, tiltedCov(60, 25, 70))
	a := Judge(f, squareCountry, 0.99)
	b := Judge(f, closed, 0.99)
	if a != b {
		t.Errorf("open %+v, closed %+v", a, b)
	}
}

func TestJudgeWithoutACountry(t *testing.T) {
	j := Judge(fitAt(Point{Lat: 1, Lon: 1}, circleCov(30)), nil, 0.99)
	if j.Overlaps || !math.IsInf(j.D, 1) {
		t.Errorf("no country: %+v", j)
	}
}
