package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
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

// A GeoJSON feature: properties as JSON, and polygons as boxes.
func testFeature(properties string, boxes ...[4]float64) string {
	polygons := []string{}
	for _, b := range boxes {
		minLon, minLat, maxLon, maxLat := b[0], b[1], b[2], b[3]
		polygons = append(polygons, fmt.Sprintf("[[[%g,%g],[%g,%g],[%g,%g],[%g,%g],[%g,%g]]]",
			minLon, minLat, maxLon, minLat, maxLon, maxLat, minLon, maxLat, minLon, minLat))
	}
	return fmt.Sprintf(`{"type":"Feature","properties":%s,"geometry":{"type":"MultiPolygon","coordinates":[%s]}}`,
		properties, strings.Join(polygons, ","))
}

func testLayer(features ...string) []byte {
	return []byte(`{"type":"FeatureCollection","features":[` + strings.Join(features, ",") + `]}`)
}

// G3's union on synthetic layers: a view's claim and a disputed piece's go to
// their claimants, and Taiwan and China are kept apart both ways, merged
// polygons and pieces included.
func TestWithClaims(t *testing.T) {
	var (
		aland    = [4]float64{0, 0, 10, 10}
		disputed = [4]float64{10, 0, 12, 2}
		bland    = [4]float64{12, 0, 20, 10}
		mainland = [4]float64{100, 20, 118, 40}
		taiwan   = [4]float64{121, 22, 122, 25}
		kinmen   = [4]float64{118.2, 24.3, 118.5, 24.6}
		paracel  = [4]float64{112, 16, 112.2, 16.2}
		islet    = [4]float64{111.4, 16.4, 111.5, 16.5}
	)
	defaultData := testLayer(
		testFeature(`{"ADM0_A3":"DEU","ISO_A2_EH":"DE"}`, aland, disputed),
		testFeature(`{"ADM0_A3":"POL","ISO_A2_EH":"PL"}`, bland),
		testFeature(`{"ADM0_A3":"CHN","ISO_A2_EH":"CN"}`, mainland, paracel),
		testFeature(`{"ADM0_A3":"TWN","ISO_A2_EH":"TW"}`, taiwan, kinmen),
		testFeature(`{"ADM0_A3":"VNM","ISO_A2_EH":"VN"}`, [4]float64{102, 9, 108, 22}),
	)
	// not strict: the synthetic layer has none of the features splits name
	base, err := convert(defaultData, false)
	if err != nil {
		t.Fatal(err)
	}
	views := map[string][]byte{
		// Poland's view: the disputed strip is Poland's, merged with it
		"pol": testLayer(
			testFeature(`{"ADM0_A3":"DEU","ISO_A2_EH":"DE"}`, aland),
			testFeature(`{"ADM0_A3":"POL","ISO_A2_EH":"PL"}`, [4]float64{10, 0, 20, 10}),
			// a piece this view gives no one
			testFeature(`{"ADM0_A3":null,"ISO_A2_EH":null}`, [4]float64{50, 50, 51, 51}),
		),
		// China's view: Taiwan and Kinmen China's
		"chn": testLayer(testFeature(`{"ADM0_A3":"CHN","ISO_A2_EH":"CN"}`, mainland, paracel, taiwan, kinmen)),
		// Taiwan's view: the mainland Taiwan's, merged with Taiwan itself
		"twn": testLayer(
			testFeature(`{"ADM0_A3":"TWN","ISO_A2_EH":"TW"}`, [4]float64{100, 20, 122, 40}, kinmen),
		),
	}
	readView := func(view string) ([]byte, error) {
		if data, ok := views[view]; ok {
			return data, nil
		}
		return testLayer(testFeature(`{"ADM0_A3":"DEU","ISO_A2_EH":"DE"}`, aland)), nil
	}
	disputedData := testLayer(
		testFeature(`{"BRK_NAME":"Strip","ADM0_A3":"DEU","ADM0_A3_PL":"POL","ADM0_A3_DE":"DEU"}`, disputed),
		testFeature(`{"BRK_NAME":"Taiwan","ADM0_A3":"TWN","ADM0_A3_CN":"CHN","ADM0_A3_TW":"TWN"}`, taiwan),
		testFeature(`{"BRK_NAME":"Paracel Is.","ADM0_A3":"CHN","ADM0_A3_VN":"VNM","ADM0_A3_TW":"TWN"}`, paracel, islet),
	)
	countries, pieceClaims, err := withClaims(base, defaultData, readView, disputedData)
	if err != nil {
		t.Fatal(err)
	}
	borders, err := lagdrift.NewBorders("", countries)
	if err != nil {
		t.Fatal(err)
	}
	holders := func(p lagdrift.Point) []string {
		codes := []string{}
		for _, code := range borders.Codes() {
			if borders.Country(code).Contains(p) {
				codes = append(codes, code)
			}
		}
		return codes
	}
	cases := []struct {
		name  string
		p     lagdrift.Point
		codes []string
	}{
		{name: "the disputed strip", p: lagdrift.Point{Lat: 1, Lon: 11}, codes: []string{"de", "pl"}},
		{name: "Germany", p: lagdrift.Point{Lat: 5, Lon: 5}, codes: []string{"de"}},
		{name: "Taiwan", p: lagdrift.Point{Lat: 23.5, Lon: 121.5}, codes: []string{"tw"}},
		{name: "Kinmen", p: lagdrift.Point{Lat: 24.45, Lon: 118.35}, codes: []string{"tw"}},
		{name: "the mainland", p: lagdrift.Point{Lat: 30, Lon: 110}, codes: []string{"cn"}},
		{name: "a Paracel island", p: lagdrift.Point{Lat: 16.1, Lon: 112.1}, codes: []string{"cn", "vn"}},
		{name: "a Paracel islet only the disputed layer draws", p: lagdrift.Point{Lat: 16.45, Lon: 111.45}, codes: []string{"cn", "vn"}},
		{name: "a piece no view gives", p: lagdrift.Point{Lat: 50.5, Lon: 50.5}, codes: []string{}},
	}
	for _, c := range cases {
		if got := holders(c.p); !slices.Equal(got, c.codes) {
			t.Errorf("%s: %v, not %v", c.name, got, c.codes)
		}
	}
	if len(pieceClaims["pl"]) != 1 || len(pieceClaims["tw"]) != 0 {
		t.Errorf("piece claims: pl %d, tw %d", len(pieceClaims["pl"]), len(pieceClaims["tw"]))
	}
}

// G3's list is checked: a borders file missing a claimant fails.
func TestCheckClaimsFindsAMissingClaimant(t *testing.T) {
	crimea := [4]float64{33, 44.5, 35, 46}
	disputedData := testLayer(testFeature(`{"BRK_NAME":"Crimea","ADM0_A3":"RUS"}`, crimea))
	borders, err := lagdrift.NewBorders("", []lagdrift.Country{
		{Code: "ru", Polygons: [][][]lagdrift.Point{{{{Lat: 44, Lon: 32}, {Lat: 44, Lon: 36}, {Lat: 47, Lon: 36}, {Lat: 47, Lon: 32}}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = checkClaims(borders, disputedData, false, 0)
	if err == nil || !strings.Contains(err.Error(), "Crimea: ua doesn't hold") {
		t.Errorf("a borders file without Ukraine's Crimea: %v", err)
	}
}

// The whole conversion on Natural Earth's pinned files, G3's list included,
// when LAGDRIFT_NE_SOURCE names a directory holding them (see main's -source).
func TestBordersFromNaturalEarth(t *testing.T) {
	source := os.Getenv("LAGDRIFT_NE_SOURCE")
	if source == "" {
		t.Skip("set LAGDRIFT_NE_SOURCE to a directory of Natural Earth v5.1.2's GeoJSON")
	}
	dir := t.TempDir()
	if err := run(filepath.Join(dir, "borders.bin"), filepath.Join(dir, "world.json"), source, nil, defaultSimplifyKm, defaultWorldScale); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(dir, "borders.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	borders, err := lagdrift.ReadBorders(f)
	if err != nil {
		t.Fatal(err)
	}
	if borders.Version() != bordersVersion || len(borders.Codes()) != 250 {
		t.Errorf("version %s, %d codes", borders.Version(), len(borders.Codes()))
	}
}
