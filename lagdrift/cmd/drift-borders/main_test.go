package main

import (
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect/lagdrift"
)

func TestFeatureCodes(t *testing.T) {
	cases := []struct {
		adm0  string
		iso   string
		codes []string
		err   bool
	}{
		{adm0: "DEU", iso: "DE", codes: []string{"de"}},
		{adm0: "FRA", iso: "FR", codes: []string{"fr"}},
		// a release whose ISO_A2_EH left France -99
		{adm0: "FRA", iso: "-99", codes: []string{"fr"}},
		{adm0: "KOS", iso: "XK", codes: []string{"xk"}},
		{adm0: "SOL", iso: "-99", codes: []string{"so"}},
		{adm0: "ESB", iso: "-99", codes: []string{"cy", "gb"}},
		// a -99 nobody mapped
		{adm0: "ZZZ", iso: "-99", err: true},
		// a code that contradicts the exceptions
		{adm0: "NOR", iso: "SE", err: true},
	}
	for _, c := range cases {
		codes, err := featureCodes(c.adm0, c.iso)
		if (err != nil) != c.err || !slices.Equal(codes, c.codes) {
			t.Errorf("%s %s: %v, %v", c.adm0, c.iso, codes, err)
		}
	}
}

// A feature collection with France's mainland and Réunion in one feature, as
// Natural Earth draws it, and a -99 feature.
const testCollection = `{"type": "FeatureCollection", "features": [
{"type": "Feature", "properties": {"ADM0_A3": "FRA", "ISO_A2_EH": "FR", "NAME": "France"},
 "geometry": {"type": "MultiPolygon", "coordinates": [
  [[[2, 46], [3, 46], [3, 47], [2, 47], [2, 46]]],
  [[[55.3, -21.3], [55.8, -21.3], [55.8, -20.9], [55.3, -20.9], [55.3, -21.3]]]]}},
{"type": "Feature", "properties": {"ADM0_A3": "SOL", "ISO_A2_EH": "-99", "NAME": "Somaliland"},
 "geometry": {"type": "Polygon", "coordinates": [[[44, 9], [45, 9], [45, 10], [44, 9]]]}}
]}`

func TestConvertSplitsAndMapsCodes(t *testing.T) {
	countries, err := convert([]byte(testCollection), false)
	if err != nil {
		t.Fatal(err)
	}
	codes := []string{}
	for _, country := range countries {
		codes = append(codes, country.Code)
	}
	if !slices.Equal(codes, []string{"fr", "re", "so"}) {
		t.Fatalf("codes %v", codes)
	}
	if len(countries[0].Polygons) != 1 || len(countries[1].Polygons) != 1 {
		t.Errorf("France's polygons %d, Réunion's %d", len(countries[0].Polygons), len(countries[1].Polygons))
	}
	// the closing point is dropped
	if n := len(countries[0].Polygons[0][0]); n != 4 {
		t.Errorf("a ring of %d points", n)
	}
	// strictly, every split must find its polygon
	if _, err := convert([]byte(testCollection), true); err == nil || !strings.Contains(err.Error(), "split") {
		t.Errorf("strict: %v", err)
	}
}

// Simplification keeps a ring's shape within the tolerance and a small
// island as a triangle.
func TestSimplifyRing(t *testing.T) {
	// a circle of 100 km with a point every km
	center := lagdrift.Point{Lat: 50, Lon: 10}
	ring := []lagdrift.Point{}
	for i := 0; i < 628; i++ {
		a := float64(i) / 100
		ring = append(ring, lagdrift.Point{
			Lat: center.Lat + 100*math.Sin(a)/kmPerDegLat,
			Lon: center.Lon + 100*math.Cos(a)/(kmPerDegLon*math.Cos(center.Lat*math.Pi/180)),
		})
	}
	simplified := simplifyRing(ring, 5)
	if !(8 <= len(simplified) && len(simplified) < 60) {
		t.Errorf("a 100 km circle at 5 km keeps %d points", len(simplified))
	}
	// every point of the original is within the tolerance of the outline
	c := lagdrift.Country{Code: "zz", Polygons: [][][]lagdrift.Point{{simplified}}}
	borders, err := lagdrift.NewBorders("", []lagdrift.Country{c})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ring {
		if borders.CountryAt(p, 5.5) != "zz" {
			t.Fatalf("%v is more than 5.5 km from the simplified circle", p)
		}
	}
	// a 1 km island
	island := []lagdrift.Point{{Lat: 0, Lon: 0}, {Lat: 0, Lon: 0.005}, {Lat: 0.004, Lon: 0.006}, {Lat: 0.008, Lon: 0.004}, {Lat: 0.006, Lon: -0.002}}
	if s := simplifyRing(island, 5); len(s) != 3 {
		t.Errorf("a small island keeps %d points", len(s))
	}
}
