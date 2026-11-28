package main

// Douglas–Peucker simplification of a ring for the world map
// (lag.dev/DRIFTTEST.md G2).

import (
	"math"

	"github.com/urnetwork/connect/v2026/lagdrift"
)

const (
	kmPerDegLat = 110.574
	kmPerDegLon = 111.32
)

// A closed ring simplified so that no dropped point is farther than
// toleranceKm from the simplified outline, measured on a flat earth at the
// ring's mean latitude. The ring keeps its first point and the point farthest
// from it, and at least 3 points: a ring smaller than the tolerance becomes a
// triangle, not nothing, so a small island stays on the map.
func simplifyRing(ring []lagdrift.Point, toleranceKm float64) []lagdrift.Point {
	n := len(ring)
	if n <= 3 {
		return ring
	}
	meanLat := 0.0
	for _, p := range ring {
		meanLat += p.Lat
	}
	meanLat /= float64(n)
	kmPerLon := kmPerDegLon * math.Cos(meanLat*math.Pi/180)
	xs := make([]float64, n)
	ys := make([]float64, n)
	for i, p := range ring {
		xs[i] = p.Lon * kmPerLon
		ys[i] = p.Lat * kmPerDegLat
	}
	// the distance from point i to the segment from a to b, km
	distance := func(i int, a int, b int) float64 {
		dx := xs[b] - xs[a]
		dy := ys[b] - ys[a]
		length2 := dx*dx + dy*dy
		t := 0.0
		if 0 < length2 {
			t = math.Max(0, math.Min(1, ((xs[i]-xs[a])*dx+(ys[i]-ys[a])*dy)/length2))
		}
		return math.Hypot(xs[i]-(xs[a]+t*dx), ys[i]-(ys[a]+t*dy))
	}

	far := 0
	farKm := -1.0
	for i := 1; i < n; i++ {
		if d := math.Hypot(xs[i]-xs[0], ys[i]-ys[0]); farKm < d {
			far = i
			farKm = d
		}
	}
	keep := make([]bool, n)
	keep[0] = true
	keep[far] = true
	// the closed ring as two open chains, 0 → far and far → n (back to 0)
	type span struct{ a, b int }
	index := func(i int) int { return i % n }
	spans := []span{{a: 0, b: far}, {a: far, b: n}}
	for 0 < len(spans) {
		s := spans[len(spans)-1]
		spans = spans[:len(spans)-1]
		worst := -1
		worstKm := toleranceKm
		for i := s.a + 1; i < s.b; i++ {
			if d := distance(index(i), index(s.a), index(s.b)); worstKm < d {
				worst = i
				worstKm = d
			}
		}
		if 0 <= worst {
			keep[index(worst)] = true
			spans = append(spans, span{a: s.a, b: worst}, span{a: worst, b: s.b})
		}
	}
	kept := 0
	for _, k := range keep {
		if k {
			kept += 1
		}
	}
	if kept < 3 {
		// the point farthest from the line through the two kept
		third := -1
		thirdKm := -1.0
		for i := 0; i < n; i++ {
			if keep[i] {
				continue
			}
			if d := distance(i, 0, far); thirdKm < d {
				third = i
				thirdKm = d
			}
		}
		if 0 <= third {
			keep[third] = true
		}
	}
	simplified := []lagdrift.Point{}
	for i, p := range ring {
		if keep[i] {
			simplified = append(simplified, p)
		}
	}
	return simplified
}
