package lagdrift

// The physical check (lag.dev/DRIFTTEST.md V5). Nothing travels faster than
// light in a vacuum, so the visitor is within
//
//	Rᵢ = (yᵢ,min / 2) × 299.79 km + s_ext,i
//
// of each extender, where yᵢ,min is the lowest round trip to it in any run.
// The IP country is physically possible when, with some single extender's
// disc dropped, it meets the area where all the other discs overlap: one
// misplaced extender or one broken path can't make a measurement contradict
// itself.
//
// The method. For each i let Iᵢ be the intersection of every disc but i's.
// The country C meets some Iᵢ exactly when (a) a point of C's boundary lies
// in Iᵢ, or (b) Iᵢ lies inside C. Both are tested without missing a case:
//
//   - (a) C's boundary is sampled at its vertices and at the points its edges
//     are split at, every 0.25° as the region test splits them, so every point
//     of the boundary is within 20 km of a sample along an edge. A sample
//     counts when it is within Rⱼ + 20 km of every extender but at most one.
//     So a boundary point in Iᵢ always finds a sample, and a sample within
//     20 km of Iᵢ can count too: the check errs toward possible, the side that
//     keeps the region's verdict.
//   - (b) A nonempty Iᵢ has a point in it among: the extenders' places, one
//     point on each disc's circle, and the points where two circles cross. Its
//     boundary is made of arcs of the circles; an arc ends where two circles
//     cross, and a boundary with no such corner is a whole circle, every
//     point of which is in Iᵢ. A candidate counts when it is within 1 m of
//     every disc but at most one and C contains it.
//
// The check never rejects on its own (V5): only the region does. A missing
// country, or fewer than two extenders, is always possible.

import (
	"math"
	"slices"
)

const (
	// half the longest part an edge is split into (0.25° of latitude and
	// longitude), km: every point of a boundary is this close to a sample
	boundaryCoverKm = 20
	// how far outside a disc a candidate on its circle may compute, km
	circleToleranceKm = 0.001
)

// One extender's disc of the physical check.
type physicalDisc struct {
	center vector3
	// the center as a place
	point Point
	// the radius as an angle, radians
	angle float64
	// the cosines of the radius widened by boundaryCoverKm and by
	// circleToleranceKm; -2 for a disc that covers the earth
	coverCos     float64
	toleranceCos float64
}

// The IP country is physically possible for obs, with any one extender's disc
// left out (V5). An extender's disc is centered at the place of its run with
// the lowest round trip, RoundTrip.MinMs (else RoundTrip.Ms when MinMs is
// unknown), and its place error is held as the fit holds it (V3).
func PhysicallyPossible(obs []Observation, c *Country) bool {
	if c == nil || len(c.Polygons) == 0 {
		return true
	}
	discs := physicalDiscs(obs)
	if len(discs) <= 1 {
		return true
	}

	// (a) the boundary
	possible := false
	for i, polygon := range c.Polygons {
		// a polygon outside two widened discs has no sample that counts
		if c.index != nil && farFromTwo(discs, c.index.polygons[i]) {
			continue
		}
		for _, ring := range polygon {
			eachBoundarySample(ring, func(p Point) bool {
				if outsideAtMostOne(discs, unitVector(p), true) {
					possible = true
					return false
				}
				return true
			})
			if possible {
				return true
			}
		}
	}

	// (b) the candidates of every intersection
	candidates := []vector3{}
	for _, disc := range discs {
		candidates = append(candidates, disc.center, onCircle(disc))
	}
	for i := 0; i < len(discs); i++ {
		for j := i + 1; j < len(discs); j++ {
			candidates = append(candidates, circleCrossings(discs[i], discs[j])...)
		}
	}
	for _, candidate := range candidates {
		if outsideAtMostOne(discs, candidate, false) && c.Contains(candidate.point()) {
			return true
		}
	}
	return false
}

// Every point of the polygon is outside at least two discs widened by
// boundaryCoverKm.
func farFromTwo(discs []physicalDisc, bound polygonBound) bool {
	if math.IsInf(bound.radiusKm, 1) {
		return false
	}
	outside := 0
	for _, disc := range discs {
		reachKm := disc.angle*EarthRadiusKm + boundaryCoverKm
		if reachKm < DistanceKm(disc.point, bound.center)-bound.radiusKm {
			outside += 1
			if 1 < outside {
				return true
			}
		}
	}
	return false
}

// One disc per extender, in the order extenders first appear in obs, then
// sorted by radius so a sample far from the small discs is ruled out first.
func physicalDiscs(obs []Observation) []physicalDisc {
	type extender struct {
		at     Point
		yMin   float64
		placeK float64
	}
	extenderIds := []string{}
	extenders := map[string]*extender{}
	for _, o := range obs {
		if !usableObservation(o) {
			continue
		}
		y := o.RoundTrip.MinMs
		if !(0 < y) || o.RoundTrip.Ms < y {
			y = o.RoundTrip.Ms
		}
		e, ok := extenders[o.ExtenderId]
		if !ok {
			extenderIds = append(extenderIds, o.ExtenderId)
			extenders[o.ExtenderId] = &extender{at: o.At, yMin: y, placeK: HeldPlaceErrorKm(o.PlaceErrorKm)}
			continue
		}
		if y < e.yMin {
			e.at = o.At
			e.yMin = y
			e.placeK = HeldPlaceErrorKm(o.PlaceErrorKm)
		}
	}
	discs := make([]physicalDisc, 0, len(extenderIds))
	for _, extenderId := range extenderIds {
		e := extenders[extenderId]
		radiusKm := e.yMin/2*VacuumKmPerMs + e.placeK
		angle := radiusKm / EarthRadiusKm
		discs = append(discs, physicalDisc{
			center:       unitVector(e.at),
			point:        e.at,
			angle:        angle,
			coverCos:     capCos(angle + boundaryCoverKm/EarthRadiusKm),
			toleranceCos: capCos(angle + circleToleranceKm/EarthRadiusKm),
		})
	}
	slices.SortStableFunc(discs, func(a physicalDisc, b physicalDisc) int {
		if a.angle < b.angle {
			return -1
		}
		if b.angle < a.angle {
			return 1
		}
		return 0
	})
	return discs
}

// The cosine a point's dot product with a disc's center must reach to be in
// it; -2 for a disc that covers the earth.
func capCos(angle float64) float64 {
	if math.Pi <= angle {
		return -2
	}
	return math.Cos(angle)
}

// v is outside at most one disc, widened for a boundary sample or for a
// candidate.
func outsideAtMostOne(discs []physicalDisc, v vector3, boundary bool) bool {
	outside := 0
	for _, disc := range discs {
		threshold := disc.toleranceCos
		if boundary {
			threshold = disc.coverCos
		}
		if v.dot(disc.center) < threshold {
			outside += 1
			if 1 < outside {
				return false
			}
		}
	}
	return true
}

// Calls f with each vertex of a closed ring and each point its edges are split
// at (splitEdge), until f returns false.
func eachBoundarySample(ring []Point, f func(Point) bool) {
	n := len(ring)
	if n == 0 {
		return
	}
	if !f(ring[n-1]) {
		return
	}
	prev := ring[n-1]
	stop := false
	for _, cur := range ring {
		splitEdge(prev, cur, func(p Point) {
			if !stop && !f(p) {
				stop = true
			}
		})
		if stop {
			return
		}
		prev = cur
	}
}

// A point on a disc's circle.
func onCircle(disc physicalDisc) vector3 {
	// a direction perpendicular to the center: toward the east, or along x at
	// a pole
	axis := vector3{x: 0, y: 0, z: 1}.cross(disc.center)
	if axis.norm() < 1e-12 {
		axis = vector3{x: 0, y: 1, z: 0}
	} else {
		axis = axis.scale(1 / axis.norm())
	}
	angle := math.Min(disc.angle, math.Pi)
	return disc.center.scale(math.Cos(angle)).add(axis.scale(math.Sin(angle)))
}

// The points where two discs' circles cross: none, or two.
func circleCrossings(a physicalDisc, b physicalDisc) []vector3 {
	if math.Pi <= a.angle || math.Pi <= b.angle {
		return nil
	}
	c := a.center.dot(b.center)
	sin2 := 1 - c*c
	if sin2 < 1e-15 {
		return nil
	}
	cosA := math.Cos(a.angle)
	cosB := math.Cos(b.angle)
	// x = u a + v b + w (a × b), with x·a = cos α, x·b = cos β, |x| = 1
	u := (cosA - c*cosB) / sin2
	v := (cosB - c*cosA) / sin2
	w2 := (1 - (u*u + v*v + 2*u*v*c)) / sin2
	if w2 < 0 {
		return nil
	}
	w := math.Sqrt(w2)
	base := a.center.scale(u).add(b.center.scale(v))
	normal := a.center.cross(b.center)
	return []vector3{
		base.add(normal.scale(w)),
		base.add(normal.scale(-w)),
	}
}
