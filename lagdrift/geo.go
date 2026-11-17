package lagdrift

// The earth's geometry, as lag.dev's radar has it (driftFit.ts): great-circle
// distances and bearings on a sphere, steps in local east and north km, and
// the azimuthal equidistant projection the region test works in
// (lag.dev/DRIFTTEST.md V6).
//
// Every formula here is mirrored by the TypeScript model (V9), which must
// evaluate it in the same order, squaring with x*x rather than x**2.

import (
	"math"
)

// lag.dev/DRIFTTEST.md I3
const (
	// v, the speed a round trip's distance travels at, km/ms (V2)
	SpeedKmPerMs = 133.24
	// the speed of light in a vacuum, for the physical check, km/ms (V5)
	VacuumKmPerMs = 299.792458
	// the quantile of an extender's round trips that the fit uses (V1)
	RoundTripQuantile = 0.10
	// the verdict's confidence when none is chosen (V6)
	DefaultConfidence = 0.99
	// the region's k for a clear stop (V10)
	EarlyK = 4.0
)

const (
	// the mean earth radius, km
	EarthRadiusKm = 6371.0088
	// K = 2 / v: ms of round trip per km of distance (V2)
	MsPerKm = 2 / SpeedKmPerMs
)

const (
	kmPerDegLat = 110.574
	kmPerDegLon = 111.32
	radPerDeg   = math.Pi / 180
	degPerRad   = 180 / math.Pi
	// a step never moves the latitude past this, so east stays defined
	maxStepLat = 89.9
)

// A place on the earth in degrees: latitude north and longitude east, the
// longitude in [-180, 180].
type Point struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

// Finite, with the latitude in [-90, 90] and the longitude in [-180, 180].
func (self Point) Valid() bool {
	return -90 <= self.Lat && self.Lat <= 90 && -180 <= self.Lon && self.Lon <= 180
}

// The longitude in [-180, 180).
func wrapLon(lon float64) float64 {
	return math.Mod(math.Mod(lon+180, 360)+360, 360) - 180
}

// The great-circle distance from a to b, km (haversine).
func DistanceKm(a Point, b Point) float64 {
	dLat := (b.Lat - a.Lat) * radPerDeg
	dLon := (b.Lon - a.Lon) * radPerDeg
	sLat := math.Sin(dLat / 2)
	sLon := math.Sin(dLon / 2)
	s := sLat*sLat + math.Cos(a.Lat*radPerDeg)*math.Cos(b.Lat*radPerDeg)*sLon*sLon
	return 2 * EarthRadiusKm * math.Asin(math.Min(1, math.Sqrt(s)))
}

// The great-circle initial bearing from a to b, degrees clockwise from north
// in [0, 360).
func BearingDeg(a Point, b Point) float64 {
	p1 := a.Lat * radPerDeg
	p2 := b.Lat * radPerDeg
	dl := (b.Lon - a.Lon) * radPerDeg
	y := math.Sin(dl) * math.Cos(p2)
	x := math.Cos(p1)*math.Sin(p2) - math.Sin(p1)*math.Cos(p2)*math.Cos(dl)
	return math.Mod(math.Atan2(y, x)*degPerRad+360, 360)
}

// p moved by east and north km on the local flat earth, as the fit steps
// (driftFit.ts moveKm). The latitude is held to ±89.9.
func MoveKm(p Point, eastKm float64, northKm float64) Point {
	lat := math.Max(-maxStepLat, math.Min(maxStepLat, p.Lat+northKm/kmPerDegLat))
	return Point{
		Lat: lat,
		Lon: wrapLon(p.Lon + eastKm/(kmPerDegLon*math.Cos(p.Lat*radPerDeg))),
	}
}

// p in the azimuthal equidistant projection centered on origin: east and north
// km at p's true distance and initial bearing from origin. A fit's offset from
// the expected place, and the region test (V6), are in it.
func AzimuthalKm(origin Point, p Point) (eastKm float64, northKm float64) {
	d := DistanceKm(origin, p)
	b := BearingDeg(origin, p) * radPerDeg
	return d * math.Sin(b), d * math.Cos(b)
}

// Projects many points onto one origin's azimuthal equidistant plane. Its
// results are AzimuthalKm's and DistanceKm's, with the origin's trig computed
// once.
type azimuthal struct {
	origin Point
	cosLat float64
	sinLat float64
}

func newAzimuthal(origin Point) *azimuthal {
	return &azimuthal{
		origin: origin,
		cosLat: math.Cos(origin.Lat * radPerDeg),
		sinLat: math.Sin(origin.Lat * radPerDeg),
	}
}

// p's east and north km, and its distance from the origin, km.
func (self *azimuthal) project(p Point) (eastKm float64, northKm float64, distanceKm float64) {
	dLat := (p.Lat - self.origin.Lat) * radPerDeg
	dLon := (p.Lon - self.origin.Lon) * radPerDeg
	p2 := p.Lat * radPerDeg
	cosP2 := math.Cos(p2)
	sLat := math.Sin(dLat / 2)
	sLon := math.Sin(dLon / 2)
	s := sLat*sLat + self.cosLat*cosP2*sLon*sLon
	d := 2 * EarthRadiusKm * math.Asin(math.Min(1, math.Sqrt(s)))
	y := math.Sin(dLon) * cosP2
	x := self.cosLat*math.Sin(p2) - self.sinLat*cosP2*math.Cos(dLon)
	b := math.Mod(math.Atan2(y, x)*degPerRad+360, 360) * radPerDeg
	return d * math.Sin(b), d * math.Cos(b), d
}

// A point on the unit sphere: x toward (0, 0), y toward (0, 90), z toward the
// north pole.
type vector3 struct {
	x float64
	y float64
	z float64
}

func unitVector(p Point) vector3 {
	lat := p.Lat * radPerDeg
	lon := p.Lon * radPerDeg
	c := math.Cos(lat)
	return vector3{
		x: c * math.Cos(lon),
		y: c * math.Sin(lon),
		z: math.Sin(lat),
	}
}

func (self vector3) dot(o vector3) float64 {
	return self.x*o.x + self.y*o.y + self.z*o.z
}

func (self vector3) cross(o vector3) vector3 {
	return vector3{
		x: self.y*o.z - self.z*o.y,
		y: self.z*o.x - self.x*o.z,
		z: self.x*o.y - self.y*o.x,
	}
}

func (self vector3) scale(f float64) vector3 {
	return vector3{x: self.x * f, y: self.y * f, z: self.z * f}
}

func (self vector3) add(o vector3) vector3 {
	return vector3{x: self.x + o.x, y: self.y + o.y, z: self.z + o.z}
}

func (self vector3) norm() float64 {
	return math.Sqrt(self.x*self.x + self.y*self.y + self.z*self.z)
}

// The place a unit vector points at.
func (self vector3) point() Point {
	return Point{
		Lat: math.Atan2(self.z, math.Hypot(self.x, self.y)) * degPerRad,
		Lon: math.Atan2(self.y, self.x) * degPerRad,
	}
}
