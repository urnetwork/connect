package lagdrift

// The verdict's geometry (lag.dev/DRIFTTEST.md V6): a visitor is outside their
// IP country only when the whole uncertainty region, the ellipse
// {q : (q − p̂)ᵀ Σ⁻¹ (q − p̂) ≤ k²} around the fit, lies outside it.
//
// Inside is p̂ in one of the country's polygons and in none of that polygon's
// holes. It is tested in degrees, by ray casting along the parallel, which is
// how Natural Earth draws its polygons (edges straight in degrees, split at the
// antimeridian, Antarctica closed along the pole). An azimuthal projection
// can't decide it: a country holding the fit's antipode wraps around the
// projection's center.
//
// Otherwise the region touches the country exactly when it contains a point of
// its boundary. Every ring of every polygon is projected onto the azimuthal
// equidistant plane centered on p̂, whitened with Σ = LLᵀ, and searched for the
// point q nearest p̂: D = ‖L⁻¹(q − p̂)‖. D ≤ k overlaps, D > k is outside. No
// convexity is assumed, so concave borders, holes and islands are all exact,
// and an island inside the region counts as touching. Natural Earth's cuts are
// not borders and are skipped (isCut): the edges along the antimeridian that
// split a polygon there, and those along a pole that close Antarctica. The
// country goes on across them, so a fit inside beside a cut is as far from
// the boundary as from the country's real edges, and V10's clear stop holds
// there as anywhere; for a fit outside, the line to a cut crosses a real edge
// first.
//
// An edge is straight in degrees, as Natural Earth's are, so before projecting
// an edge longer than 0.25° in latitude or longitude is split into equal parts
// no longer than that. A part whose ends are both more than 10,000 km from p̂
// is taken at its ends alone: near the antipode the projection stretches a
// short edge into a long chord that can pass near the center. Its ends stand
// for it to within half its length, so the error there is under 20 km in
// 10,000.

import (
	"math"
)

const (
	// an edge is split into parts no longer than this, degrees
	maxEdgeDeg = 0.25
	// a part with both ends this far from the fit is taken at its ends, km
	farKm = 10000
	// a covariance's whitening floors its variances here, km²
	minVarianceKm2 = 1e-9
)

// The textbook k of a confidence: √χ²₂(confidence) = √(−2 ln(1 − confidence)).
// 2.448 at 95% and 3.035 at 99%. 0 for a confidence of 0 or less, and +Inf for
// 1 or more. A run's k is never below it (V11, DefaultK).
func KForConfidence(confidence float64) float64 {
	if !(0 < confidence) {
		return 0
	}
	if 1 <= confidence {
		return math.Inf(1)
	}
	return math.Sqrt(-2 * math.Log(1-confidence))
}

// A country's land: polygons whose first ring is the outline and whose other
// rings are holes. A ring's last point may repeat its first or not. Its edges
// are straight in degrees and never cross the antimeridian: a polygon that
// does is split there, as Natural Earth splits them.
type Country struct {
	Code     string      `json:"code"`
	Polygons [][][]Point `json:"polygons"`

	// bounds on each polygon, for skipping the far ones; Borders sets it,
	// and a country without it is searched whole, to the same result
	index *countryIndex
}

// Bounds on each of a country's polygons.
type countryIndex struct {
	polygons []polygonBound
}

type polygonBound struct {
	// the outline's degree box
	box degreeBox
	// every boundary point, and every chord the region test draws between
	// them, is within radiusKm of center
	center   Point
	radiusKm float64
}

// Bounds on a country's polygons. A lon/lat-straight edge stays within half
// its length, at most √(Δlat² + Δlon²) degrees of 111.2 km, of its nearer end,
// and a chord between split points in the azimuthal plane, within 10,000 km,
// strays from the edge by less than 32 km more.
func newCountryIndex(polygons [][][]Point) *countryIndex {
	index := &countryIndex{polygons: make([]polygonBound, len(polygons))}
	for i, polygon := range polygons {
		bound := polygonBound{box: emptyBox()}
		if len(polygon) == 0 || len(polygon[0]) == 0 {
			bound.radiusKm = math.Inf(1)
			index.polygons[i] = bound
			continue
		}
		sum := vector3{}
		for _, ring := range polygon {
			for _, p := range ring {
				sum = sum.add(unitVector(p))
			}
		}
		for _, p := range polygon[0] {
			bound.box.add(p)
		}
		if sum.norm() < 1e-9 {
			bound.radiusKm = math.Inf(1)
			index.polygons[i] = bound
			continue
		}
		bound.center = sum.point()
		vertexKm := 0.0
		halfEdgeKm := 0.0
		for _, ring := range polygon {
			for j, p := range ring {
				vertexKm = math.Max(vertexKm, DistanceKm(bound.center, p))
				q := ring[(j+1)%len(ring)]
				halfEdgeKm = math.Max(halfEdgeKm, math.Hypot(q.Lat-p.Lat, q.Lon-p.Lon)*111.2/2)
			}
		}
		bound.radiusKm = vertexKm + halfEdgeKm + 32
		index.polygons[i] = bound
	}
	return index
}

// The country has a boundary to judge against: a ring with a point. One
// without, such as a country of no polygons, is an unknown country to Decide
// and EvaluateStop, as a nil one is (lag.dev/DRIFTTEST.md V6, V10).
func (self *Country) hasBoundary() bool {
	for _, polygon := range self.Polygons {
		for _, ring := range polygon {
			if 0 < len(ring) {
				return true
			}
		}
	}
	return false
}

// p is inside one of the country's polygons and in none of that polygon's
// holes.
func (self *Country) Contains(p Point) bool {
	for i, polygon := range self.Polygons {
		if self.index != nil && !self.index.polygons[i].box.contains(p) {
			continue
		}
		if polygonContains(polygon, p) {
			return true
		}
	}
	return false
}

func polygonContains(polygon [][]Point, p Point) bool {
	if len(polygon) == 0 || !ringContains(polygon[0], p) {
		return false
	}
	for _, hole := range polygon[1:] {
		if ringContains(hole, p) {
			return false
		}
	}
	return true
}

// Ray casting along the parallel toward the east, with edges straight in
// degrees.
func ringContains(ring []Point, p Point) bool {
	inside := false
	n := len(ring)
	for i, j := 0, n-1; i < n; j, i = i, i+1 {
		a := ring[i]
		b := ring[j]
		if (p.Lat < a.Lat) != (p.Lat < b.Lat) {
			x := (b.Lon-a.Lon)*(p.Lat-a.Lat)/(b.Lat-a.Lat) + a.Lon
			if p.Lon < x {
				inside = !inside
			}
		}
	}
	return inside
}

// The region test of a fit against a country (V6).
type Judgement struct {
	// the fit is in the country
	Inside bool `json:"inside"`
	// the region touches the country: Inside, or D ≤ k
	Overlaps bool `json:"overlaps"`
	// the Mahalanobis distance to the nearest point of the country's
	// boundary, inside or out
	D float64 `json:"d"`
	// ‖q − p̂‖ to that point, km; 0 inside
	DistanceKm float64 `json:"distanceKm"`
	// k / √(uᵀΣ⁻¹u): the region's radius toward that point, km
	SignificanceKm float64 `json:"significanceKm"`
	// DistanceKm − SignificanceKm: positive exactly when the whole region
	// is outside, km
	MarginKm float64 `json:"marginKm"`
	// an extension of lag.dev/DRIFTTEST.md I3: the k judged at
	K float64 `json:"k"`
}

// The region test of f against c with the region's k (V6): the run's k from
// V11 (DefaultK, or a calibrated k), or the textbook KForConfidence. The
// figures are toward q, the boundary point nearest the fit in whitened space,
// with u the unit vector from p̂ to q:
//
//	DistanceKm = ‖q − p̂‖, 0 when inside
//	SignificanceKm = k / √(uᵀΣ⁻¹u) = k ‖q − p̂‖ / D
//	MarginKm = DistanceKm − SignificanceKm
//
// Outside, MarginKm is computed as ‖q − p̂‖ (D − k) / D, so its sign is
// exactly D > k's. Inside, it is −SignificanceKm. When p̂ is on the boundary
// itself, u is undefined and SignificanceKm is the region's smallest radius,
// k σ_minor. A nil or empty country is never touched: D, DistanceKm and
// MarginKm are +Inf.
//
// lag.dev/DRIFTTEST.md I3 (V11): Judge takes k, where it took a confidence.
func Judge(f *Fit, c *Country, k float64) Judgement {
	inf := math.Inf(1)
	if f == nil || c == nil {
		return Judgement{D: inf, DistanceKm: inf, MarginKm: inf, K: k}
	}
	search := newBoundarySearch(f.At, f.Cov)
	sigmaMajorKm := search.sigmaMajorKm()
	for i, polygon := range c.Polygons {
		// a polygon whose every point is farther than the best in whitened
		// space can't hold the nearest
		if c.index != nil && search.found {
			bound := c.index.polygons[i]
			if search.best < (DistanceKm(f.At, bound.center)-bound.radiusKm)/sigmaMajorKm {
				continue
			}
		}
		for _, ring := range polygon {
			search.ring(ring)
		}
	}
	if !search.found {
		return Judgement{D: inf, DistanceKm: inf, MarginKm: inf, K: k}
	}
	inside := c.Contains(f.At)
	d := search.best
	qEast, qNorth := search.nearestKm()
	distanceKm := math.Hypot(qEast, qNorth)

	var significanceKm float64
	if 0 < d && 0 < distanceKm {
		significanceKm = k * distanceKm / d
	} else {
		_, minorKm, _ := CovEllipse(f.Cov)
		significanceKm = k * minorKm
	}
	j := Judgement{
		Inside:         inside,
		Overlaps:       inside || d <= k,
		D:              d,
		SignificanceKm: significanceKm,
		K:              k,
	}
	if inside {
		j.DistanceKm = 0
		j.MarginKm = -significanceKm
	} else if 0 < d && 0 < distanceKm {
		j.DistanceKm = distanceKm
		j.MarginKm = distanceKm * (d - k) / d
	} else {
		j.DistanceKm = distanceKm
		j.MarginKm = distanceKm - significanceKm
	}
	return j
}

// The boundary point nearest a fit in whitened space, over the rings given
// to it.
type boundarySearch struct {
	projection *azimuthal
	// Σ = LLᵀ, L = [[l11, 0], [l21, l22]]
	l11 float64
	l21 float64
	l22 float64

	found bool
	// the whitened distance and point of the nearest so far
	best   float64
	bestWx float64
	bestWy float64
}

func newBoundarySearch(at Point, cov [2][2]float64) *boundarySearch {
	s00 := math.Max(minVarianceKm2, cov[0][0])
	s01 := (cov[0][1] + cov[1][0]) / 2
	l11 := math.Sqrt(s00)
	l21 := s01 / l11
	l22 := math.Sqrt(math.Max(minVarianceKm2, cov[1][1]-l21*l21))
	return &boundarySearch{
		projection: newAzimuthal(at),
		l11:        l11,
		l21:        l21,
		l22:        l22,
		best:       math.Inf(1),
	}
}

// The whitening's largest axis, √λ_max(LLᵀ), km: no point is nearer in
// whitened space than its distance over this.
func (self *boundarySearch) sigmaMajorKm() float64 {
	s00 := self.l11 * self.l11
	s01 := self.l11 * self.l21
	s11 := self.l21*self.l21 + self.l22*self.l22
	return math.Sqrt((s00+s11)/2 + math.Hypot((s00-s11)/2, s01))
}

// L⁻¹ applied to east and north km.
func (self *boundarySearch) whiten(eastKm float64, northKm float64) (float64, float64) {
	x := eastKm / self.l11
	return x, (northKm - self.l21*x) / self.l22
}

// The nearest point found, east and north km from the fit.
func (self *boundarySearch) nearestKm() (float64, float64) {
	return self.l11 * self.bestWx, self.l21*self.bestWx + self.l22*self.bestWy
}

// Searches a closed ring's edges, starting with the one from its last point
// to its first, all but its cuts.
func (self *boundarySearch) ring(ring []Point) {
	n := len(ring)
	if n == 0 {
		return
	}
	prev := ring[n-1]
	prevE, prevN, prevD := self.projection.project(prev)
	if n == 1 {
		self.segment(prevE, prevN, prevD, prevE, prevN, prevD)
		return
	}
	for _, cur := range ring {
		if isCut(prev, cur) {
			prevE, prevN, prevD = self.projection.project(cur)
		} else {
			splitEdge(prev, cur, func(p Point) {
				e, nn, d := self.projection.project(p)
				self.segment(prevE, prevN, prevD, e, nn, d)
				prevE, prevN, prevD = e, nn, d
			})
		}
		prev = cur
	}
}

// An edge Natural Earth draws to cut a polygon, not a border: along the
// antimeridian, both ends at longitude 180 or both at −180, or along a pole,
// both ends at latitude 90 or both at −90. The country goes on across it
// (lag.dev/DRIFTTEST.md V6).
func isCut(a Point, b Point) bool {
	return (math.Abs(a.Lon) == 180 && a.Lon == b.Lon) || (math.Abs(a.Lat) == 90 && a.Lat == b.Lat)
}

func (self *boundarySearch) segment(aEast float64, aNorth float64, aDistance float64, bEast float64, bNorth float64, bDistance float64) {
	ax, ay := self.whiten(aEast, aNorth)
	bx, by := self.whiten(bEast, bNorth)
	if farKm < aDistance && farKm < bDistance {
		self.consider(ax, ay)
		self.consider(bx, by)
		return
	}
	dx := bx - ax
	dy := by - ay
	length2 := dx*dx + dy*dy
	t := 0.0
	if 0 < length2 {
		t = math.Max(0, math.Min(1, -(ax*dx+ay*dy)/length2))
	}
	self.consider(ax+t*dx, ay+t*dy)
}

func (self *boundarySearch) consider(x float64, y float64) {
	self.found = true
	d := math.Hypot(x, y)
	if d < self.best {
		self.best = d
		self.bestWx = x
		self.bestWy = y
	}
}

// Calls f with each point an edge from a to b is split at, then b. The edge is
// split into ⌈max(|Δlat|, |Δlon|) / 0.25°⌉ equal parts, straight in degrees.
func splitEdge(a Point, b Point, f func(Point)) {
	dLat := b.Lat - a.Lat
	dLon := b.Lon - a.Lon
	parts := max(1, int(math.Ceil(math.Max(math.Abs(dLat), math.Abs(dLon))/maxEdgeDeg)))
	for s := 1; s < parts; s++ {
		f(Point{
			Lat: a.Lat + dLat*float64(s)/float64(parts),
			Lon: a.Lon + dLon*float64(s)/float64(parts),
		})
	}
	f(b)
}
