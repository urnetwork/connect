package lagdrift

// Country borders by GeoLite2's country code (lag.dev/DRIFTTEST.md G1, G2),
// and the drift country (V6): the country containing a fit, else the nearest
// within a distance, else none.

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
)

// The drift country is the nearest within this far when none contains the
// fit, km (V6).
const DriftCountryWithinKm = 300

var ErrInvalidCountryCode = errors.New("lagdrift: a country code is two lower-case letters")

// A degree box around a polygon or a country.
type degreeBox struct {
	minLat float64
	minLon float64
	maxLat float64
	maxLon float64
}

func emptyBox() degreeBox {
	return degreeBox{
		minLat: math.Inf(1),
		minLon: math.Inf(1),
		maxLat: math.Inf(-1),
		maxLon: math.Inf(-1),
	}
}

func (self *degreeBox) add(p Point) {
	self.minLat = math.Min(self.minLat, p.Lat)
	self.minLon = math.Min(self.minLon, p.Lon)
	self.maxLat = math.Max(self.maxLat, p.Lat)
	self.maxLon = math.Max(self.maxLon, p.Lon)
}

func (self *degreeBox) contains(p Point) bool {
	return self.minLat <= p.Lat && p.Lat <= self.maxLat && self.minLon <= p.Lon && p.Lon <= self.maxLon
}

// False only when every point of the box is more than km from p. Two points
// at latitudes within ±φ and longitudes Δλ apart are at least d apart where
// sin(d / 2R) = cos φ · sin(Δλ / 2) (haversine), so the box is widened by
// km / 110.574 degrees of latitude and by 2 asin(sin(km / 2R) / cos φ) of
// longitude, φ the widened box's highest latitude, across the antimeridian too.
func (self *degreeBox) near(p Point, km float64) bool {
	latMargin := km / kmPerDegLat
	if p.Lat < self.minLat-latMargin || self.maxLat+latMargin < p.Lat {
		return false
	}
	highLat := math.Max(math.Abs(self.minLat), math.Abs(self.maxLat)) + latMargin
	if 90 <= highLat || math.Pi*EarthRadiusKm <= km {
		return true
	}
	s := math.Sin(km/(2*EarthRadiusKm)) / math.Cos(highLat*radPerDeg)
	if 1 <= s {
		return true
	}
	lonMargin := 2 * math.Asin(s) * degPerRad
	// the longitude's distance to the box's span, around the earth
	if self.minLon <= p.Lon && p.Lon <= self.maxLon {
		return true
	}
	gap := math.Min(lonGap(p.Lon, self.minLon), lonGap(p.Lon, self.maxLon))
	return gap <= lonMargin
}

// The difference between two longitudes the short way around, degrees.
func lonGap(a float64, b float64) float64 {
	d := math.Mod(math.Abs(a-b), 360)
	return math.Min(d, 360-d)
}

// Countries by code. Read-only once made, so safe for concurrent use.
type Borders struct {
	version string
	// sorted by code
	countries []Country
	// per country, a box around each polygon's outline, and around all of them
	polygonBoxes [][]degreeBox
	countryBoxes []degreeBox
}

// Borders of the given countries, merging the polygons of a code given more
// than once. Codes are GeoLite2's: two lower-case letters.
func NewBorders(version string, countries []Country) (*Borders, error) {
	codeCountries := map[string]*Country{}
	codes := []string{}
	for _, country := range countries {
		if !validCode(country.Code) {
			return nil, fmt.Errorf("%w: %q", ErrInvalidCountryCode, country.Code)
		}
		merged, ok := codeCountries[country.Code]
		if !ok {
			merged = &Country{Code: country.Code}
			codeCountries[country.Code] = merged
			codes = append(codes, country.Code)
		}
		merged.Polygons = append(merged.Polygons, country.Polygons...)
	}
	slices.Sort(codes)
	borders := &Borders{version: version}
	for _, code := range codes {
		country := codeCountries[code]
		boxes := make([]degreeBox, len(country.Polygons))
		countryBox := emptyBox()
		for i, polygon := range country.Polygons {
			boxes[i] = emptyBox()
			if 0 < len(polygon) {
				for _, p := range polygon[0] {
					boxes[i].add(p)
					countryBox.add(p)
				}
			}
		}
		country.index = newCountryIndex(country.Polygons)
		borders.countries = append(borders.countries, *country)
		borders.polygonBoxes = append(borders.polygonBoxes, boxes)
		borders.countryBoxes = append(borders.countryBoxes, countryBox)
	}
	return borders, nil
}

func validCode(code string) bool {
	return len(code) == 2 && 'a' <= code[0] && code[0] <= 'z' && 'a' <= code[1] && code[1] <= 'z'
}

// The geometry's version, e.g. ne-admin0-10m-v5.1.2: what a verdict records as
// geometry_version (G1, P4).
func (self *Borders) Version() string {
	return self.version
}

// Every country's code, sorted.
func (self *Borders) Codes() []string {
	codes := make([]string, len(self.countries))
	for i, country := range self.countries {
		codes[i] = country.Code
	}
	return codes
}

// The country of a code, in either case, or nil when there is none. The
// country is the borders' own and must not be changed.
func (self *Borders) Country(code string) *Country {
	code = strings.ToLower(code)
	i, ok := slices.BinarySearchFunc(self.countries, code, func(country Country, code string) int {
		return strings.Compare(country.Code, code)
	})
	if !ok {
		return nil
	}
	return &self.countries[i]
}

// The drift country (V6): the code of the country containing p, else of the
// nearest country whose boundary is within withinKm, else "" (at sea). Where
// countries overlap, as disputed areas given to each claimant do, the first
// code in order wins. The distance is measured to the boundary as the region
// test measures it, in the azimuthal plane centered on p.
func (self *Borders) CountryAt(p Point, withinKm float64) string {
	if !p.Valid() {
		return ""
	}
	for i, country := range self.countries {
		if !self.countryBoxes[i].contains(p) {
			continue
		}
		for j, polygon := range country.Polygons {
			if self.polygonBoxes[i][j].contains(p) && polygonContains(polygon, p) {
				return country.Code
			}
		}
	}
	if !(0 < withinKm) {
		return ""
	}
	bestCode := ""
	bestKm := math.Inf(1)
	identity := [2][2]float64{{1, 0}, {0, 1}}
	for i, country := range self.countries {
		if !self.countryBoxes[i].near(p, withinKm) {
			continue
		}
		search := newBoundarySearch(p, identity)
		for j, polygon := range country.Polygons {
			if !self.polygonBoxes[i][j].near(p, withinKm) {
				continue
			}
			for _, ring := range polygon {
				search.ring(ring)
			}
		}
		if search.found && search.best < bestKm {
			bestKm = search.best
			bestCode = country.Code
		}
	}
	if bestKm <= withinKm {
		return bestCode
	}
	return ""
}
