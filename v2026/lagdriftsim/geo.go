package lagdriftsim

// Geometry the study needs beyond lagdrift's: moving along a great circle,
// synthetic round countries, a country's distance from a point, and a
// country cut down to the polygons near a visitor.

import (
	"math"

	"github.com/urnetwork/connect/v2026/lagdrift"
)

const (
	radPerDeg = math.Pi / 180
	degPerRad = 180 / math.Pi
)

// The place distanceKm from p along the great circle that leaves it at
// bearingDeg, clockwise from north.
func destination(p lagdrift.Point, bearingDeg float64, distanceKm float64) lagdrift.Point {
	lat1 := p.Lat * radPerDeg
	lon1 := p.Lon * radPerDeg
	theta := bearingDeg * radPerDeg
	delta := distanceKm / lagdrift.EarthRadiusKm
	sinLat2 := math.Sin(lat1)*math.Cos(delta) + math.Cos(lat1)*math.Sin(delta)*math.Cos(theta)
	lat2 := math.Asin(math.Max(-1, math.Min(1, sinLat2)))
	y := math.Sin(theta) * math.Sin(delta) * math.Cos(lat1)
	x := math.Cos(delta) - math.Sin(lat1)*sinLat2
	lon2 := lon1 + math.Atan2(y, x)
	return lagdrift.Point{
		Lat: lat2 * degPerRad,
		Lon: wrapLon(lon2 * degPerRad),
	}
}

// The longitude in [-180, 180).
func wrapLon(lon float64) float64 {
	return math.Mod(math.Mod(lon+180, 360)+360, 360) - 180
}

// A round country of radiusKm around center: a polygon of sides points on the
// circle, split at the antimeridian as Natural Earth splits its polygons, so
// lagdrift's edges stay straight in degrees.
func diskCountry(code string, center lagdrift.Point, radiusKm float64, sides int) *lagdrift.Country {
	// the ring with its longitudes unwrapped, so it is continuous
	ring := make([]lagdrift.Point, sides)
	prevLon := center.Lon
	for i := 0; i < sides; i++ {
		p := destination(center, 360*float64(i)/float64(sides), radiusKm)
		lon := p.Lon
		for lon-prevLon > 180 {
			lon -= 360
		}
		for prevLon-lon > 180 {
			lon += 360
		}
		ring[i] = lagdrift.Point{Lat: p.Lat, Lon: lon}
		prevLon = lon
	}
	minLon := math.Inf(1)
	maxLon := math.Inf(-1)
	for _, p := range ring {
		minLon = math.Min(minLon, p.Lon)
		maxLon = math.Max(maxLon, p.Lon)
	}
	polygons := [][][]lagdrift.Point{}
	switch {
	case maxLon <= 180 && -180 <= minLon:
		polygons = append(polygons, [][]lagdrift.Point{ring})
	case 180 < maxLon:
		west := clipLon(ring, 180, true)
		east := shiftLon(clipLon(ring, 180, false), -360)
		polygons = appendRing(polygons, west)
		polygons = appendRing(polygons, east)
	default:
		east := clipLon(ring, -180, false)
		west := shiftLon(clipLon(ring, -180, true), 360)
		polygons = appendRing(polygons, east)
		polygons = appendRing(polygons, west)
	}
	return &lagdrift.Country{Code: code, Polygons: polygons}
}

func appendRing(polygons [][][]lagdrift.Point, ring []lagdrift.Point) [][][]lagdrift.Point {
	if len(ring) < 3 {
		return polygons
	}
	return append(polygons, [][]lagdrift.Point{ring})
}

// The part of a closed ring at or west of lon (keepWest) or at or east of it,
// by Sutherland–Hodgman clipping against the meridian, straight in degrees.
func clipLon(ring []lagdrift.Point, lon float64, keepWest bool) []lagdrift.Point {
	inside := func(p lagdrift.Point) bool {
		if keepWest {
			return p.Lon <= lon
		}
		return lon <= p.Lon
	}
	out := []lagdrift.Point{}
	n := len(ring)
	for i := 0; i < n; i++ {
		a := ring[(i+n-1)%n]
		b := ring[i]
		crossing := func() lagdrift.Point {
			t := (lon - a.Lon) / (b.Lon - a.Lon)
			return lagdrift.Point{Lat: a.Lat + t*(b.Lat-a.Lat), Lon: lon}
		}
		switch {
		case inside(a) && inside(b):
			out = append(out, b)
		case inside(a) && !inside(b):
			out = append(out, crossing())
		case !inside(a) && inside(b):
			out = append(out, crossing(), b)
		}
	}
	return out
}

func shiftLon(ring []lagdrift.Point, by float64) []lagdrift.Point {
	shifted := make([]lagdrift.Point, len(ring))
	for i, p := range ring {
		shifted[i] = lagdrift.Point{Lat: p.Lat, Lon: math.Max(-180, math.Min(180, p.Lon+by))}
	}
	return shifted
}

// Whether p is in c, and the distance from p to c's boundary, km, measured
// as the region test measures it (V6): Judge with a covariance of 1 km² is
// the boundary's distance in km.
func borderDistanceKm(c *lagdrift.Country, p lagdrift.Point) (inside bool, distanceKm float64) {
	f := &lagdrift.Fit{
		At:  p,
		Cov: [2][2]float64{{1, 0}, {0, 1}},
	}
	j := lagdrift.Judge(f, c, lagdrift.KForConfidence(lagdrift.DefaultConfidence))
	return j.Inside, j.D
}

// c with only the polygons that come within keepKm of one of the places.
// Judge projects every boundary point of a country, and the farther parts of
// a large one can't decide a verdict near the places: a fit whose σ_major is
// within lag.yml's 1,500 km is at least keepKm / 1,500 standard deviations
// from them, and a fit far from every place is far from the truth.
func localCountry(c *lagdrift.Country, keepKm float64, places ...lagdrift.Point) *lagdrift.Country {
	if c == nil {
		return nil
	}
	local := &lagdrift.Country{Code: c.Code}
	for _, polygon := range c.Polygons {
		if len(polygon) == 0 {
			continue
		}
		near := false
		polygonCountry := &lagdrift.Country{Polygons: [][][]lagdrift.Point{polygon}}
		for _, p := range places {
			if polygonCountry.Contains(p) {
				near = true
			}
		}
		for _, q := range polygon[0] {
			if near {
				break
			}
			for _, p := range places {
				if lagdrift.DistanceKm(p, q) <= keepKm {
					near = true
					break
				}
			}
			if near {
				break
			}
		}
		if near {
			local.Polygons = append(local.Polygons, polygon)
		}
	}
	return local
}
