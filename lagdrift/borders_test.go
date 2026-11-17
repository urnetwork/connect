package lagdrift

import (
	"bytes"
	"errors"
	"os"
	"slices"
	"testing"
)

// Belgium, the Netherlands, Germany and Luxembourg, from Natural Earth
// v5.1.2 by drift-borders -countries be,nl,de,lu: the 1:10m borders file and
// the 1:50m world map, as drawn with G3's claims, of which these four have
// none.
func fixtureBorders(t *testing.T) (*Borders, *Borders) {
	t.Helper()
	full, err := os.Open("testdata/benelux-de-10m.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer full.Close()
	borders, err := ReadBorders(full)
	if err != nil {
		t.Fatal(err)
	}
	simple, err := os.Open("testdata/benelux-de-world.json")
	if err != nil {
		t.Fatal(err)
	}
	defer simple.Close()
	world, err := ReadWorld(simple)
	if err != nil {
		t.Fatal(err)
	}
	return borders, world
}

var syntheticCountries = []Country{
	{Code: "zz", Polygons: [][][]Point{{boxRing(0, 0, 2, 2)}}},
	// two polygons of one code given apart, one with a hole and a closing point
	{Code: "zy", Polygons: [][][]Point{{boxRing(10, 10, 14, 14), boxRing(11, 11, 12, 12)}}},
	{Code: "zy", Polygons: [][][]Point{{append(boxRing(-1, 179, 1, 180), Point{Lat: -1, Lon: 179})}}},
	{Code: "zx", Polygons: [][][]Point{{boxRing(-0.123456789, -60.987654321, 0.5, -60)}}},
}

func TestBordersRoundTrip(t *testing.T) {
	out := &bytes.Buffer{}
	if err := WriteBorders(out, "synthetic-v1", syntheticCountries); err != nil {
		t.Fatal(err)
	}
	b, err := ReadBorders(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if b.Version() != "synthetic-v1" || !slices.Equal(b.Codes(), []string{"zx", "zy", "zz"}) {
		t.Fatalf("version %q, codes %v", b.Version(), b.Codes())
	}
	zy := b.Country("ZY")
	if zy == nil || len(zy.Polygons) != 2 || len(zy.Polygons[0]) != 2 {
		t.Fatalf("zy: %+v", zy)
	}
	// the closing point is dropped
	if n := len(zy.Polygons[1][0]); n != 4 {
		t.Errorf("a closed ring reads back with %d points", n)
	}
	// points are microdegrees
	zx := b.Country("zx").Polygons[0][0]
	if zx[0] != (Point{Lat: -0.123457, Lon: -60.987654}) {
		t.Errorf("a point reads back as %+v", zx[0])
	}
	// writing what was read gives the same bytes
	again := &bytes.Buffer{}
	if err := WriteBorders(again, b.Version(), b.Countries()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), again.Bytes()) {
		t.Error("a second round trip changed the file")
	}
	if b.Country("de") != nil {
		t.Error("a code that isn't there")
	}
}

// A damaged file is an error, never wrong borders.
func TestReadBordersRefusesDamage(t *testing.T) {
	out := &bytes.Buffer{}
	if err := WriteBorders(out, "synthetic-v1", syntheticCountries); err != nil {
		t.Fatal(err)
	}
	good := out.Bytes()
	damaged := [][]byte{
		nil,
		[]byte("not a borders file at all"),
		good[:len(good)/2],
		good[:len(good)-1],
		append(slices.Clone(good), 0),
	}
	// a flipped byte anywhere
	for i := 0; i < len(good); i += 7 {
		flipped := slices.Clone(good)
		flipped[i] ^= 0x40
		damaged = append(damaged, flipped)
	}
	for i, data := range damaged {
		if _, err := ReadBorders(bytes.NewReader(data)); !errors.Is(err, ErrInvalidBorders) {
			t.Errorf("damage %d: %v", i, err)
		}
	}
	if err := WriteBorders(&bytes.Buffer{}, "", []Country{{Code: "ZZ"}}); !errors.Is(err, ErrInvalidCountryCode) {
		t.Errorf("an upper-case code: %v", err)
	}
}

func TestWorldRoundTrip(t *testing.T) {
	out := &bytes.Buffer{}
	if err := WriteWorld(out, "synthetic-world", 5, 1000, syntheticCountries); err != nil {
		t.Fatal(err)
	}
	w, err := ReadWorld(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if w.Version() != "synthetic-world" || !slices.Equal(w.Codes(), []string{"zx", "zy", "zz"}) {
		t.Fatalf("version %q, codes %v", w.Version(), w.Codes())
	}
	if p := w.Country("zx").Polygons[0][0][0]; p != (Point{Lat: -0.123, Lon: -60.988}) {
		t.Errorf("a point reads back as %+v", p)
	}
	if _, err := ReadWorld(bytes.NewReader([]byte(`{"format":"other","scale":1,"countries":[]}`))); !errors.Is(err, ErrInvalidWorld) {
		t.Errorf("another format: %v", err)
	}
}

// The drift country (V6): the containing country, else the nearest within
// the distance, else none; overlapping countries give the first code.
func TestCountryAt(t *testing.T) {
	b, err := NewBorders("", append(slices.Clone(syntheticCountries),
		Country{Code: "aa", Polygons: [][][]Point{{boxRing(1, 1, 3, 3)}}}))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		p        Point
		withinKm float64
		code     string
	}{
		{p: Point{Lat: 0.5, Lon: 0.5}, withinKm: 300, code: "zz"},
		// in both zz and aa: aa's code comes first
		{p: Point{Lat: 1.5, Lon: 1.5}, withinKm: 300, code: "aa"},
		// in zy's hole: nearest, zy's own boundary
		{p: Point{Lat: 11.5, Lon: 11.5}, withinKm: 300, code: "zy"},
		{p: Point{Lat: 11.5, Lon: 11.5}, withinKm: 10, code: ""},
		// 1° south of zz, 111 km
		{p: Point{Lat: -1, Lon: 1}, withinKm: 300, code: "zz"},
		{p: Point{Lat: -1, Lon: 1}, withinKm: 100, code: ""},
		// across the antimeridian from zy's sliver, 56 km
		{p: Point{Lat: 0, Lon: -179.5}, withinKm: 100, code: "zy"},
		{p: Point{Lat: 40, Lon: -40}, withinKm: 300, code: ""},
	}
	for _, c := range cases {
		if code := b.CountryAt(c.p, c.withinKm); code != c.code {
			t.Errorf("%+v within %.0f km: %q, not %q", c.p, c.withinKm, code, c.code)
		}
	}
}

// T2's Belgium, Netherlands and Germany, in both the 1:10m and the
// simplified map.
func TestBordersBeneluxAndGermany(t *testing.T) {
	borders, world := fixtureBorders(t)
	if borders.Version() != "ne-admin0-10m-v5.1.2-claims1" || world.Version() != "ne-admin0-50m-v5.1.2-claims1" {
		t.Errorf("versions %q and %q", borders.Version(), world.Version())
	}
	places := []struct {
		name string
		p    Point
		code string
	}{
		{name: "Brussels", p: Point{Lat: 50.85, Lon: 4.35}, code: "be"},
		{name: "Antwerp", p: Point{Lat: 51.22, Lon: 4.40}, code: "be"},
		{name: "Amsterdam", p: Point{Lat: 52.37, Lon: 4.90}, code: "nl"},
		{name: "Maastricht", p: Point{Lat: 50.85, Lon: 5.69}, code: "nl"},
		{name: "Aachen", p: Point{Lat: 50.78, Lon: 6.08}, code: "de"},
		{name: "Berlin", p: Point{Lat: 52.52, Lon: 13.40}, code: "de"},
		{name: "Luxembourg", p: Point{Lat: 49.61, Lon: 6.13}, code: "lu"},
		// the North Sea, 40 km off the Dutch coast
		{name: "the North Sea", p: Point{Lat: 52.6, Lon: 4.0}, code: "nl"},
	}
	for _, b := range []*Borders{borders, world} {
		for _, place := range places {
			if code := b.CountryAt(place.p, DriftCountryWithinKm); code != place.code {
				t.Errorf("%s: %s, not %s (%s)", place.name, code, place.code, b.Version())
			}
		}
	}
	// a visitor in Utrecht whose address says Belgium: a region a quarter as
	// wide as the distance is clear of Belgium, one twice as wide reaches it
	utrecht := Point{Lat: 52.09, Lon: 5.12}
	for _, b := range []*Borders{borders, world} {
		be := b.Country("be")
		k := KForConfidence(0.99)
		d := Judge(fitAt(utrecht, circleCov(10)), be, 0.99).DistanceKm
		if !(60 < d && d < 120) {
			t.Fatalf("Utrecht is %.1f km from Belgium (%s)", d, b.Version())
		}
		if j := Judge(fitAt(utrecht, circleCov(d/(4*k))), be, 0.99); j.Inside || j.Overlaps || !(0 < j.MarginKm) {
			t.Errorf("Utrecht against Belgium, narrow: %+v (%s)", j, b.Version())
		}
		if j := Judge(fitAt(utrecht, circleCov(2*d/k)), be, 0.99); j.Inside || !j.Overlaps || !(j.MarginKm <= 0) {
			t.Errorf("Utrecht against Belgium, wide: %+v (%s)", j, b.Version())
		}
		if j := Judge(fitAt(utrecht, circleCov(10)), b.Country("nl"), 0.99); !j.Inside || !j.Overlaps {
			t.Errorf("Utrecht against the Netherlands: %+v (%s)", j, b.Version())
		}
	}
	// the two maps agree on the distance to within the simplification
	full := Judge(fitAt(utrecht, circleCov(10)), borders.Country("be"), 0.99)
	simple := Judge(fitAt(utrecht, circleCov(10)), world.Country("be"), 0.99)
	if d := full.DistanceKm - simple.DistanceKm; 6 < d || d < -6 {
		t.Errorf("Utrecht is %.2f km from Belgium at 1:10m and %.2f km on the world map", full.DistanceKm, simple.DistanceKm)
	}
}
