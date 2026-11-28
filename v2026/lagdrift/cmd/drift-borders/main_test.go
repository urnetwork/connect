package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect/v2026/lagdrift"
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

func TestNoteClaimants(t *testing.T) {
	cases := []struct {
		note  string
		codes []string
		err   bool
	}{
		{note: "Admin. by Egypt; Claimed by Sudan", codes: []string{"sd"}},
		{note: "Claimed by China, Taiwan, Malaysia, the Philippines, and Brunei", codes: []string{"cn", "tw", "my", "ph", "bn"}},
		{note: "Admin. by France; Claimed by Mauritius and the Seychelles", codes: []string{"mu", "sc"}},
		{note: "Claimed by Colombia, Jamaica, Nicaragua and the United States", codes: []string{"co", "jm", "ni", "us"}},
		{note: "Leased to U.S.A. by Cuba; Claimed by Cuba", codes: []string{"cu"}},
		{note: "Claimed by Chile and Argentina; under survey", codes: []string{"cl", "ar"}},
		{note: "Self admin.; Claimed by Azer.", codes: []string{"az"}},
		{note: "Admin. by Venezuela; Claimed by multiple", codes: []string{}},
		{note: "Leased to Russia by Kazakhstan", codes: []string{}},
		{note: "", codes: []string{}},
		// a claimant without a code fails, rather than being dropped
		{note: "Admin. by Spain; Claimed by Atlantis", err: true},
	}
	for _, c := range cases {
		codes, err := noteClaimants(c.note)
		if (err != nil) != c.err || (!c.err && !slices.Equal(codes, c.codes)) {
			t.Errorf("%q: %v, %v", c.note, codes, err)
		}
	}
}

// G3's claims on synthetic layers: each disputed piece goes to its de facto
// owner, its sovereign, every view's owner and every claimant its note names;
// Taiwan's and Israel's own pieces stay their owners' alone; Taiwan and China
// are kept apart both ways; and a holder whose default polygons hold a piece
// already gets no second polygon.
func TestWithClaims(t *testing.T) {
	var (
		aland     = [4]float64{0, 0, 10, 10}
		strip     = [4]float64{10, 0, 12, 2}
		bland     = [4]float64{12, 0, 20, 10}
		mainland  = [4]float64{100, 20, 118, 40}
		taiwan    = [4]float64{121, 22, 122, 25}
		kinmen    = [4]float64{118.2, 24.3, 118.5, 24.6}
		paracel   = [4]float64{112, 16, 112.2, 16.2}
		islet     = [4]float64{111.4, 16.4, 111.5, 16.5}
		israel    = [4]float64{34, 29.5, 35, 33}
		westBank  = [4]float64{35, 31.3, 35.6, 32.5}
		britain   = [4]float64{-6, 50, 2, 58}
		gibraltar = [4]float64{-5.37, 36.1, -5.33, 36.16}
		spain     = [4]float64{-9, 36.2, 3, 43.5}
		egypt     = [4]float64{25, 22, 36, 31.5}
		halaib    = [4]float64{35, 22, 36, 23}
		sudan     = [4]float64{22, 10, 38, 21.9}
		india     = [4]float64{68, 8, 97, 30}
		arunachal = [4]float64{91.5, 26.7, 97, 29.4}
		georgia   = [4]float64{40, 41, 46.7, 43.6}
		abkhazia  = [4]float64{40, 42.4, 42, 43.6}
		ukraine   = [4]float64{22, 44.4, 40.2, 52.4}
		russia    = [4]float64{40.3, 44, 60, 60}
		donetsk   = [4]float64{37.5, 47.5, 38.5, 48.5}
		luhansk   = [4]float64{38.6, 48, 39.9, 48.9}
	)
	defaultData := testLayer(
		testFeature(`{"ADM0_A3":"DEU","ISO_A2_EH":"DE","SOV_A3":"DEU","HOMEPART":1}`, aland, strip),
		testFeature(`{"ADM0_A3":"POL","ISO_A2_EH":"PL","SOV_A3":"POL","HOMEPART":1}`, bland),
		testFeature(`{"ADM0_A3":"CHN","ISO_A2_EH":"CN","SOV_A3":"CH1","HOMEPART":1}`, mainland, paracel),
		testFeature(`{"ADM0_A3":"TWN","ISO_A2_EH":"TW","SOV_A3":"TWN","HOMEPART":1}`, taiwan, kinmen),
		testFeature(`{"ADM0_A3":"VNM","ISO_A2_EH":"VN","SOV_A3":"VNM","HOMEPART":1}`, [4]float64{102, 9, 108, 22}),
		testFeature(`{"ADM0_A3":"ISR","ISO_A2_EH":"IL","SOV_A3":"IS1","HOMEPART":1}`, israel),
		testFeature(`{"ADM0_A3":"PSX","ISO_A2_EH":"PS","SOV_A3":"IS1","HOMEPART":-99}`, westBank),
		testFeature(`{"ADM0_A3":"GBR","ISO_A2_EH":"GB","SOV_A3":"GB1","HOMEPART":1}`, britain),
		testFeature(`{"ADM0_A3":"GIB","ISO_A2_EH":"GI","SOV_A3":"GB1","HOMEPART":-99}`, gibraltar),
		testFeature(`{"ADM0_A3":"ESP","ISO_A2_EH":"ES","SOV_A3":"ESP","HOMEPART":1}`, spain),
		testFeature(`{"ADM0_A3":"EGY","ISO_A2_EH":"EG","SOV_A3":"EGY","HOMEPART":1}`, egypt),
		testFeature(`{"ADM0_A3":"SDN","ISO_A2_EH":"SD","SOV_A3":"SDN","HOMEPART":1}`, sudan),
		testFeature(`{"ADM0_A3":"IND","ISO_A2_EH":"IN","SOV_A3":"IND","HOMEPART":1}`, india),
		testFeature(`{"ADM0_A3":"GEO","ISO_A2_EH":"GE","SOV_A3":"GEO","HOMEPART":1}`, georgia),
		testFeature(`{"ADM0_A3":"UKR","ISO_A2_EH":"UA","SOV_A3":"UKR","HOMEPART":1}`, ukraine),
		testFeature(`{"ADM0_A3":"RUS","ISO_A2_EH":"RU","SOV_A3":"RUS","HOMEPART":1}`, russia),
	)
	// not strict: the synthetic layer has none of the features splits name
	base, err := convert(defaultData, false)
	if err != nil {
		t.Fatal(err)
	}
	disputedData := testLayer(
		testFeature(`{"BRK_NAME":"Strip","ADM0_A3":"DEU","SOV_A3":"DEU","ADM0_A3_PL":"POL","ADM0_A3_DE":"DEU","ADM0_A3_UN":-99}`, strip),
		testFeature(`{"BRK_NAME":"Taiwan","ADM0_A3":"TWN","SOV_A3":"TWN","ADM0_A3_CN":"CHN","ADM0_A3_TW":"TWN","NOTE_BRK":"Self admin.; Claimed by China"}`, taiwan),
		testFeature(`{"BRK_NAME":"Paracel Is.","ADM0_A3":"CHN","SOV_A3":"CH1","ADM0_A3_VN":"VNM","ADM0_A3_TW":"TWN","NOTE_BRK":"Admin. by China; Claimed by Vietnam and Taiwan"}`, paracel, islet),
		testFeature(`{"BRK_NAME":"Israel","ADM0_A3":"ISR","SOV_A3":"IS1","ADM0_A3_PK":"PSX"}`, israel),
		testFeature(`{"BRK_NAME":"West Bank","ADM0_A3":"PSX","SOV_A3":"IS1","ADM0_A3_IL":"PSX"}`, westBank),
		testFeature(`{"BRK_NAME":"Gibraltar","ADM0_A3":"GIB","SOV_A3":"GB1","ADM0_A3_ES":"GIB","NOTE_BRK":"Admin. By U.K.; Claimed by Spain"}`, gibraltar),
		testFeature(`{"BRK_NAME":"Halayib Triangle","ADM0_A3":"EGY","SOV_A3":"EGY","NOTE_BRK":"Admin. by Egypt; Claimed by Sudan"}`, halaib),
		testFeature(`{"BRK_NAME":"Arunachal Pradesh","ADM0_A3":"IND","SOV_A3":"IND","ADM0_A3_CN":"CHN","ADM0_A3_TW":"TWN","NOTE_BRK":"Admin. by India; Claimed by China"}`, arunachal),
		testFeature(`{"BRK_NAME":"Abkhazia","ADM0_A3":"B35","SOV_A3":"GEO","ADM0_A3_RU":"B35","NOTE_BRK":"Self admin.; Claimed by Georgia"}`, abkhazia),
		// Russia's view gives them their own codes, which name no country
		testFeature(`{"BRK_NAME":"Donetsk People's Republic","ADM0_A3":"UKR","SOV_A3":"UKR","ADM0_A3_RU":"C02","NOTE_BRK":"Self admin.; Claimed by Ukraine"}`, donetsk),
		testFeature(`{"BRK_NAME":"Luhansk People's Republic","ADM0_A3":"UKR","SOV_A3":"UKR","ADM0_A3_RU":"C03","NOTE_BRK":"Self admin.; Claimed by Ukraine"}`, luhansk),
	)
	countries, claims, err := withClaims(base, defaultData, disputedData)
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
		{name: "Tel Aviv", p: lagdrift.Point{Lat: 32.08, Lon: 34.78}, codes: []string{"il"}},
		{name: "the West Bank", p: lagdrift.Point{Lat: 31.9, Lon: 35.2}, codes: []string{"il", "ps"}},
		{name: "Gibraltar", p: lagdrift.Point{Lat: 36.13, Lon: -5.35}, codes: []string{"es", "gb", "gi"}},
		{name: "Madrid", p: lagdrift.Point{Lat: 40.4, Lon: -3.7}, codes: []string{"es"}},
		{name: "London", p: lagdrift.Point{Lat: 51.5, Lon: -0.1}, codes: []string{"gb"}},
		{name: "Halaib", p: lagdrift.Point{Lat: 22.5, Lon: 35.5}, codes: []string{"eg", "sd"}},
		{name: "Cairo", p: lagdrift.Point{Lat: 30, Lon: 31.2}, codes: []string{"eg"}},
		{name: "Arunachal Pradesh", p: lagdrift.Point{Lat: 28, Lon: 94}, codes: []string{"cn", "in", "tw"}},
		{name: "Delhi", p: lagdrift.Point{Lat: 28.6, Lon: 77.2}, codes: []string{"in"}},
		{name: "Abkhazia", p: lagdrift.Point{Lat: 43, Lon: 41}, codes: []string{"ge"}},
		{name: "Donetsk", p: lagdrift.Point{Lat: 48.0, Lon: 37.8}, codes: []string{"ru", "ua"}},
		{name: "Luhansk", p: lagdrift.Point{Lat: 48.57, Lon: 39.31}, codes: []string{"ru", "ua"}},
		{name: "Kyiv", p: lagdrift.Point{Lat: 50.45, Lon: 30.52}, codes: []string{"ua"}},
	}
	for _, c := range cases {
		if got := holders(c.p); !slices.Equal(got, c.codes) {
			t.Errorf("%s: %v, not %v", c.name, got, c.codes)
		}
	}
	// a holder whose default polygons hold a piece gets no second polygon:
	// of the Paracels, China takes only the islet its default lacks, and
	// Israel takes the West Bank but not its own piece
	for code, n := range map[string]int{"de": 2, "eg": 1, "in": 1, "ge": 1, "cn": 4, "il": 2, "pl": 2, "sd": 2, "tw": 3} {
		if got := len(borders.Country(code).Polygons); got != n {
			t.Errorf("%s has %d polygons, not %d", code, got, n)
		}
	}
	pieceCodes := map[string][]string{}
	for _, c := range claims {
		if codes, ok := pieceCodes[c.piece]; ok && !slices.Equal(codes, c.codes) {
			t.Errorf("%s's polygons go to %v and %v", c.piece, codes, c.codes)
		}
		pieceCodes[c.piece] = c.codes
	}
	for piece, codes := range map[string][]string{
		"Strip":             {"de", "pl"},
		"Taiwan":            {"tw"},
		"Paracel Is.":       {"cn", "vn"},
		"Israel":            {"il"},
		"West Bank":         {"il", "ps"},
		"Gibraltar":         {"es", "gb", "gi"},
		"Halayib Triangle":  {"eg", "sd"},
		"Arunachal Pradesh": {"cn", "in", "tw"},
		"Abkhazia":          {"ge"},
		// added: the release predates Russia's claim
		"Donetsk People's Republic": {"ru", "ua"},
		"Luhansk People's Republic": {"ru", "ua"},
	} {
		if !slices.Equal(pieceCodes[piece], codes) {
			t.Errorf("claim %s: %v, not %v", piece, pieceCodes[piece], codes)
		}
	}
	// an undisputed piece, or one with an added claimant, that the layer
	// lacks fails the build
	if _, _, err := withClaims(base, defaultData, testLayer(testFeature(`{"BRK_NAME":"Taiwan","ADM0_A3":"TWN"}`, taiwan))); err == nil || !strings.Contains(err.Error(), "Israel") {
		t.Errorf("a layer without Israel's piece: %v", err)
	}
	withoutDonetsk := testLayer(
		testFeature(`{"BRK_NAME":"Taiwan","ADM0_A3":"TWN"}`, taiwan),
		testFeature(`{"BRK_NAME":"Israel","ADM0_A3":"ISR"}`, israel),
		testFeature(`{"BRK_NAME":"Luhansk People's Republic","ADM0_A3":"UKR"}`, luhansk),
	)
	if _, _, err := withClaims(base, defaultData, withoutDonetsk); err == nil || !strings.Contains(err.Error(), "Donetsk") {
		t.Errorf("a layer without Donetsk's piece: %v", err)
	}
}

// G3's list and every claim are checked: a borders file missing a claimant
// fails.
func TestCheckClaimsFindsAMissingClaimant(t *testing.T) {
	crimea := [4]float64{33, 44.5, 35, 46}
	disputedData := testLayer(testFeature(`{"BRK_NAME":"Crimea","ADM0_A3":"RUS"}`, crimea))
	ru := lagdrift.Country{Code: "ru", Polygons: [][][]lagdrift.Point{{{{Lat: 44, Lon: 32}, {Lat: 44, Lon: 36}, {Lat: 47, Lon: 36}, {Lat: 47, Lon: 32}}}}}
	borders, err := lagdrift.NewBorders("", []lagdrift.Country{ru})
	if err != nil {
		t.Fatal(err)
	}
	err = checkClaims(borders, disputedData, nil, claimCheckMode{})
	if err == nil || !strings.Contains(err.Error(), "Crimea: ua doesn't hold") {
		t.Errorf("a borders file without Ukraine's Crimea: %v", err)
	}
	// a claim beyond G3's list
	strip := [][]lagdrift.Point{{{Lat: 0, Lon: 10}, {Lat: 0, Lon: 12}, {Lat: 2, Lon: 12}, {Lat: 2, Lon: 10}}}
	claims := []claim{{piece: "Strip", polygon: strip, codes: []string{"de", "pl"}}}
	borders, err = lagdrift.NewBorders("", []lagdrift.Country{ru, {Code: "de", Polygons: [][][]lagdrift.Point{strip}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []claimCheckMode{{}, {world: true, slackKm: 5, scale: 1000}} {
		failures := strings.Join(claimFailures(borders, claims, mode), "\n")
		if !strings.Contains(failures, "Strip: pl doesn't hold") || strings.Contains(failures, "Strip: de") {
			t.Errorf("%+v: a borders file without Poland's strip: %s", mode, failures)
		}
	}
	// on the world map, a polygon too small to draw isn't held to it
	speck := [][]lagdrift.Point{{{Lat: 0, Lon: 10}, {Lat: 0, Lon: 10.0004}, {Lat: 0.0004, Lon: 10.0004}}}
	if failures := claimFailures(borders, []claim{{piece: "Speck", polygon: speck, codes: []string{"pl"}}}, claimCheckMode{world: true, slackKm: 5, scale: 1000}); len(failures) != 0 {
		t.Errorf("a speck on the world map: %v", failures)
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
