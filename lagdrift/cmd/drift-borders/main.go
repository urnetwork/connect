// Command drift-borders writes the drift test's country borders
// (lag.dev/DRIFTTEST.md G1, G2, I5) from Natural Earth's Admin 0 countries, at
// a pinned release of github.com/nvkelso/natural-earth-vector (public domain):
//
//   - -borders: the full 1:10m, in lagdrift's borders format, for the api's
//     verdicts. It goes in the api's config as lag/ne-admin0-10m.bin.
//   - -world: the 1:50m simplified by Douglas–Peucker at about 5 km and
//     quantized, as lagdrift's world map JSON, for the SDK
//     (js/src/lag/drift/world.generated.json). A country too small to draw at
//     1:50m is taken from 1:10m, simplified the same way.
//
// It downloads the release's GeoJSON, or reads it from -source, a directory
// holding ne_10m_admin_0_countries.geojson and ne_50m_admin_0_countries.geojson,
// and checks each file's SHA-256 against the pin either way. -countries limits
// the output to some codes, for test fixtures.
//
//	go run ./lagdrift/cmd/drift-borders -borders ne-admin0-10m.bin -world world.generated.json
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/connect/lagdrift"
)

// The pinned release: its tag, the commit the tag names, and each file's
// SHA-256.
const (
	release       = "v5.1.2"
	releaseCommit = "f1890d9f152c896d250a77557a5751a93d494776"
	sourceUrlBase = "https://raw.githubusercontent.com/nvkelso/natural-earth-vector/" + releaseCommit + "/geojson/"

	countries10mFile   = "ne_10m_admin_0_countries.geojson"
	countries10mSha256 = "239eec57ac17f100a11e2536cffc56752c318b50ae765b0918ff7aab4ce8f255"
	countries50mFile   = "ne_50m_admin_0_countries.geojson"
	countries50mSha256 = "3e458fc036ad0a66411f2c1e6cac49c5d7bfb81cb1123bc513b22511a2b7fdeb"

	// the versions the files record (lagdrift Borders.Version)
	bordersVersion = "ne-admin0-10m-" + release
	worldVersion   = "ne-admin0-50m-" + release

	defaultSimplifyKm = 5
	defaultWorldScale = 1000
	downloadTimeout   = 5 * time.Minute
)

func main() {
	bordersPath := flag.String("borders", "", "write the 1:10m borders file here")
	worldPath := flag.String("world", "", "write the simplified 1:50m world map JSON here")
	source := flag.String("source", "", "read the GeoJSON from this directory instead of downloading it")
	codeList := flag.String("countries", "", "write only these codes, comma separated (for fixtures)")
	simplifyKm := flag.Float64("simplify-km", defaultSimplifyKm, "the world map's simplification tolerance, km")
	scale := flag.Int("scale", defaultWorldScale, "the world map's units per degree")
	flag.Parse()
	if *bordersPath == "" && *worldPath == "" {
		fmt.Fprintln(os.Stderr, "drift-borders: nothing to write: give -borders, -world or both")
		flag.Usage()
		os.Exit(2)
	}
	only := []string{}
	for _, code := range strings.Split(*codeList, ",") {
		if code = strings.ToLower(strings.TrimSpace(code)); code != "" {
			only = append(only, code)
		}
	}
	if err := run(*bordersPath, *worldPath, *source, only, *simplifyKm, *scale); err != nil {
		fmt.Fprintf(os.Stderr, "drift-borders: %v\n", err)
		os.Exit(1)
	}
}

func run(bordersPath string, worldPath string, source string, only []string, simplifyKm float64, scale int) error {
	data10m, err := readSource(source, countries10mFile, countries10mSha256)
	if err != nil {
		return err
	}
	countries10m, err := convert(data10m, true)
	if err != nil {
		return fmt.Errorf("%s: %w", countries10mFile, err)
	}
	countries10m = keep(countries10m, only)

	if bordersPath != "" {
		out := &bytes.Buffer{}
		if err := lagdrift.WriteBorders(out, bordersVersion, countries10m); err != nil {
			return err
		}
		// what it wrote reads back
		if _, err := lagdrift.ReadBorders(bytes.NewReader(out.Bytes())); err != nil {
			return fmt.Errorf("the borders file doesn't read back: %w", err)
		}
		if err := writeFile(bordersPath, out.Bytes()); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "drift-borders: %s: %s, %d countries, %d points, %d bytes\n", bordersPath, bordersVersion, len(countries10m), pointCount(countries10m), out.Len())
	}

	if worldPath != "" {
		data50m, err := readSource(source, countries50mFile, countries50mSha256)
		if err != nil {
			return err
		}
		countries50m, err := convert(data50m, false)
		if err != nil {
			return fmt.Errorf("%s: %w", countries50mFile, err)
		}
		countries50m = keep(countries50m, only)
		world, filled := worldCountries(countries50m, countries10m, simplifyKm, scale)
		out := &bytes.Buffer{}
		if err := lagdrift.WriteWorld(out, worldVersion, simplifyKm, scale, world); err != nil {
			return err
		}
		borders, err := lagdrift.ReadWorld(bytes.NewReader(out.Bytes()))
		if err != nil {
			return fmt.Errorf("the world map doesn't read back: %w", err)
		}
		// every code of the borders is on the map
		for _, country := range countries10m {
			if borders.Country(country.Code) == nil {
				return fmt.Errorf("the world map has no %s at scale %d", country.Code, scale)
			}
		}
		if err := writeFile(worldPath, out.Bytes()); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "drift-borders: %s: %s, %d countries (%d from 1:10m: %s), %d points, %d bytes\n", worldPath, worldVersion, len(borders.Codes()), len(filled), strings.Join(filled, " "), pointCount(borders.Countries()), out.Len())
	}
	return nil
}

// The GeoJSON file from the source directory, or downloaded from the pinned
// commit, checked against its SHA-256.
func readSource(source string, name string, sha256Hex string) ([]byte, error) {
	var data []byte
	if source != "" {
		var err error
		data, err = os.ReadFile(filepath.Join(source, name))
		if err != nil {
			return nil, err
		}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), downloadTimeout)
		defer cancel()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceUrlBase+name, nil)
		if err != nil {
			return nil, err
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s: %s", sourceUrlBase+name, response.Status)
		}
		data, err = io.ReadAll(response.Body)
		if err != nil {
			return nil, err
		}
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != sha256Hex {
		return nil, fmt.Errorf("%s has SHA-256 %s, not the pinned %s (Natural Earth %s)", name, got, sha256Hex, release)
	}
	return data, nil
}

func writeFile(path string, data []byte) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, data, 0o644)
}

// The GeoJSON this command reads.
type featureCollection struct {
	Type     string    `json:"type"`
	Features []feature `json:"features"`
}

type feature struct {
	Properties struct {
		Adm0A3  string `json:"ADM0_A3"`
		IsoA2Eh string `json:"ISO_A2_EH"`
		Name    string `json:"NAME"`
	} `json:"properties"`
	Geometry struct {
		Type        string          `json:"type"`
		Coordinates json.RawMessage `json:"coordinates"`
	} `json:"geometry"`
}

// The countries of an Admin 0 feature collection, keyed by GeoLite2's codes
// (codes.go), sorted by code, each with its polygons in the features' order.
// strict requires every split to find a polygon, as at 1:10m.
func convert(data []byte, strict bool) ([]lagdrift.Country, error) {
	var collection featureCollection
	if err := json.Unmarshal(data, &collection); err != nil {
		return nil, err
	}
	if collection.Type != "FeatureCollection" || len(collection.Features) == 0 {
		return nil, errors.New("not a feature collection")
	}
	codePolygons := map[string][][][]lagdrift.Point{}
	splitMatches := map[string][]int{}
	for _, f := range collection.Features {
		adm0 := f.Properties.Adm0A3
		codes, err := featureCodes(adm0, f.Properties.IsoA2Eh)
		if err != nil {
			return nil, fmt.Errorf("%s (%s): %w", f.Properties.Name, adm0, err)
		}
		polygons, err := geometryPolygons(f.Geometry.Type, f.Geometry.Coordinates)
		if err != nil {
			return nil, fmt.Errorf("%s (%s): %w", f.Properties.Name, adm0, err)
		}
		featureSplits := splits[adm0]
		if _, ok := splitMatches[adm0]; !ok && 0 < len(featureSplits) {
			splitMatches[adm0] = make([]int, len(featureSplits))
		}
		for _, polygon := range polygons {
			polygonCodes := codes
			minLon, minLat, maxLon, maxLat := outlineBox(polygon)
			for i, s := range featureSplits {
				if s.contains(minLon, minLat, maxLon, maxLat) {
					polygonCodes = []string{s.code}
					splitMatches[adm0][i] += 1
					break
				}
			}
			for _, code := range polygonCodes {
				codePolygons[code] = append(codePolygons[code], polygon)
			}
		}
	}
	if strict {
		for adm0, matches := range splitMatches {
			for i, count := range matches {
				if count == 0 {
					return nil, fmt.Errorf("split %s of %s finds no polygon", splits[adm0][i].code, adm0)
				}
			}
		}
		for adm0 := range splits {
			if _, ok := splitMatches[adm0]; !ok {
				return nil, fmt.Errorf("no feature %s to split", adm0)
			}
		}
	}
	codes := []string{}
	for code := range codePolygons {
		codes = append(codes, code)
	}
	slices.Sort(codes)
	countries := []lagdrift.Country{}
	for _, code := range codes {
		countries = append(countries, lagdrift.Country{Code: code, Polygons: codePolygons[code]})
	}
	return countries, nil
}

// A Polygon's or MultiPolygon's polygons, each ring without its closing point.
func geometryPolygons(geometryType string, coordinates json.RawMessage) ([][][]lagdrift.Point, error) {
	var raw [][][][]float64
	switch geometryType {
	case "Polygon":
		var polygon [][][]float64
		if err := json.Unmarshal(coordinates, &polygon); err != nil {
			return nil, err
		}
		raw = [][][][]float64{polygon}
	case "MultiPolygon":
		if err := json.Unmarshal(coordinates, &raw); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("geometry %q", geometryType)
	}
	polygons := [][][]lagdrift.Point{}
	for _, rawPolygon := range raw {
		polygon := [][]lagdrift.Point{}
		for _, rawRing := range rawPolygon {
			ring := []lagdrift.Point{}
			for _, position := range rawRing {
				if len(position) < 2 {
					return nil, errors.New("a position without a longitude and latitude")
				}
				p := lagdrift.Point{Lat: position[1], Lon: position[0]}
				if !p.Valid() {
					return nil, fmt.Errorf("position %v out of range", position)
				}
				ring = append(ring, p)
			}
			for 1 < len(ring) && ring[len(ring)-1] == ring[0] {
				ring = ring[:len(ring)-1]
			}
			if len(ring) < 3 {
				return nil, errors.New("a ring of fewer than 3 points")
			}
			polygon = append(polygon, ring)
		}
		if 0 < len(polygon) {
			polygons = append(polygons, polygon)
		}
	}
	return polygons, nil
}

// The degree box around a polygon's outline.
func outlineBox(polygon [][]lagdrift.Point) (float64, float64, float64, float64) {
	minLon, minLat := math.Inf(1), math.Inf(1)
	maxLon, maxLat := math.Inf(-1), math.Inf(-1)
	for _, p := range polygon[0] {
		minLon = math.Min(minLon, p.Lon)
		minLat = math.Min(minLat, p.Lat)
		maxLon = math.Max(maxLon, p.Lon)
		maxLat = math.Max(maxLat, p.Lat)
	}
	return minLon, minLat, maxLon, maxLat
}

// Only the given codes, or all when none are given.
func keep(countries []lagdrift.Country, only []string) []lagdrift.Country {
	if len(only) == 0 {
		return countries
	}
	kept := []lagdrift.Country{}
	for _, country := range countries {
		if slices.Contains(only, country.Code) {
			kept = append(kept, country)
		}
	}
	return kept
}

// The world map: each 1:50m country simplified, and each code the 1:10m has and
// the 1:50m lacks, or loses to quantization, taken from the 1:10m. Returns the
// codes taken from the 1:10m too.
func worldCountries(countries50m []lagdrift.Country, countries10m []lagdrift.Country, simplifyKm float64, scale int) ([]lagdrift.Country, []string) {
	simplify := func(country lagdrift.Country) lagdrift.Country {
		simplified := lagdrift.Country{Code: country.Code}
		for _, polygon := range country.Polygons {
			rings := [][]lagdrift.Point{}
			for _, ring := range polygon {
				rings = append(rings, simplifyRing(ring, simplifyKm))
			}
			simplified.Polygons = append(simplified.Polygons, rings)
		}
		return simplified
	}
	// a country survives quantization when one of its outlines keeps 3
	// distinct points
	survives := func(country lagdrift.Country) bool {
		for _, polygon := range country.Polygons {
			distinct := map[[2]int64]bool{}
			for _, p := range polygon[0] {
				distinct[[2]int64{int64(math.Round(p.Lat * float64(scale))), int64(math.Round(p.Lon * float64(scale)))}] = true
			}
			if 3 <= len(distinct) {
				return true
			}
		}
		return false
	}
	world := []lagdrift.Country{}
	codes := map[string]bool{}
	for _, country := range countries50m {
		simplified := simplify(country)
		if survives(simplified) {
			world = append(world, simplified)
			codes[country.Code] = true
		}
	}
	filled := []string{}
	for _, country := range countries10m {
		if codes[country.Code] {
			continue
		}
		simplified := simplify(country)
		if !survives(simplified) {
			// too small to simplify: as drawn
			simplified = country
		}
		world = append(world, simplified)
		filled = append(filled, country.Code)
	}
	slices.SortFunc(world, func(a lagdrift.Country, b lagdrift.Country) int {
		return strings.Compare(a.Code, b.Code)
	})
	return world, filled
}

func pointCount(countries []lagdrift.Country) int {
	n := 0
	for _, country := range countries {
		for _, polygon := range country.Polygons {
			for _, ring := range polygon {
				n += len(ring)
			}
		}
	}
	return n
}
