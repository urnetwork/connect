package main

// Disputed areas (lag.dev/DRIFTTEST.md G3, the user's decision of
// 2026-11-17): each belongs to every country that claims it, except Taiwan,
// which stays Taiwan's alone.
//
// Natural Earth's default layer draws de facto lines. Each of its points of
// view (ne_10m_admin_0_countries_<view>) draws the world as one country sees
// it, and its disputed-areas layer (ne_10m_admin_0_disputed_areas) gives each
// disputed piece's owner in every view (ADM0_A3_<view>). So each code's
// polygons are the union of its polygons in the default layer, in every
// point of view, and of every disputed piece a view gives it. A polygon
// already among the code's, to the microdegree, isn't added again; a view's
// country that is merely the same as the default's adds nothing.
//
// Taiwan and China are kept apart both ways (G3, 2026-11-17), since an IP
// database never labels one side with the other's code: no view's or piece's
// polygon goes to a code but tw when it is Taiwan's in the default layer
// (Taiwan, Penghu, Kinmen, Matsu, …), though several views give those to
// China, and none goes to tw when it is China's there, though Taiwan's view
// gives it all of the PRC, and the Paracels with it. A polygon is the other
// side's when it is one of that side's default polygons to the microdegree,
// or when any of its interior points (interiorPoints) lies in that side's
// default polygons or in a disputed piece that side holds de facto (ADM0_A3),
// which catches a view's polygon that merges China's mainland with more, and
// the Paracel islands the disputed layer draws but the default layer's China
// lacks. Everything else a view claims is added, which G3
// intends, for example Arunachal Pradesh to cn and the Falklands to ar.

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/urnetwork/connect/lagdrift"
)

// The points of view of Natural Earth v5.1.2 at 1:10m, by the suffix of
// ne_10m_admin_0_countries_<view>.geojson, with each file's SHA-256.
var viewSha256s = map[string]string{
	"arg": "5de5c83114945be5c328295abf31d29641ffe7f0280c0f8f746777648fae5803",
	"bdg": "201b2e38710402b6c7221094d49d5f87c4cb7e3cfa69274a571e2e13d73d8840",
	"bra": "de70a3924150e0a6f866fcb507270dc8040afbf9a4addab009783f241e7e2687",
	"chn": "a13bf5f310fde87bc0a5f994f8ce9bd706cc198d8ee37d221e61c2546b945372",
	"deu": "fc4d56c6bc256f779e0ba21579f81d64885f5296dba3c23c3e78a9b89a7aa0fb",
	"egy": "4ed6984fca9bc368619be9b9b7026beef007251b8ccab27ebaeeea0d4af74eaa",
	"esp": "e61193d14bc4b41c1652ff94a939d6b4123102329a41524e0accd3463e3079c8",
	"fra": "273def0394f5e344ff4938b1cbe9c48f36aea2e8e1635a8814768bc50f5dd44c",
	"gbr": "7acfd0ccade031cd165630e4d5e9b51650079976d94008100407d03e0e83a492",
	"grc": "ca4ad82c571e7b507b4382ede1377596e869f680ebcae258bed887c58e94d060",
	"idn": "cc5292e1082cdeaa8b92d3314faf89cb0f0b7d5ac4808c72fd7b6efdd3886249",
	"ind": "3e67a5a6ddeff3247ca8400cd2514fd4a5ac6e5432823b25d610a7028597d45f",
	"isr": "45e43bd18bb0d97290ec1751c8b669c7e99a69ca0413b7a0d3196505fc42d4aa",
	"ita": "36e4d8f5f00b5a9ed9a086ef38e330110d7e9cd8fc3b93f54c739123b80531ec",
	"jpn": "11bc047064a5cf2db03efc2aece341e657df2dd59ad715a8221cc1575697df1b",
	"kor": "0dd7351fd445628d190d40a3f7b756fea21fd058e63e400ad498e4108ea0130c",
	"mar": "71cfbfcb408cf9277dbd5700da168f4634e8e80a9a8b114eb595d739bebd6598",
	"nep": "b56e5675c080fca9f520a37cebae32d549cf60cbe2aa0b46453eb74cfb27c297",
	"nld": "9a8327cdff482625ffdb17b982203e23f90ef97a4265becc3c98dc66c145539f",
	"pak": "9c070579b14cdef62bc5f7ecf9dfc8320195b3c211c4032f8d46087a70a0599c",
	"pol": "4d8ef44ed174a06d678dc751bd0a29a4f36da310c3719a54dad88ad68855ed8f",
	"prt": "7c9d4d5a72a13ef7d242f9a3c4211ddaf009becddfbfae1311aafbd1c65982c3",
	"pse": "ec53197338ffdda19bd238d32e309bee2fefbc8cedda368365a62f92d63d8f18",
	"rus": "c8dc3acedf6e1b4e42a21a7eade7fa9664a6f4ff0ed0b3d60aeeeb45dc31b771",
	"sau": "43b6675bb0fe9f4e0feb68c5a1e9b0b71f4a41c6d5fd4134b41db6f843134cf8",
	"swe": "7f50094bd832c91839666d02dab070d0b6220c58852ef430a1df1e3544fd3259",
	"tur": "225a283ee34da427fd77bdf046017cf9bdaae576da6ed66ea23a8e6757b1018f",
	"twn": "a8719781480a862c9243ceab07d47e3be3569b5cd550662918d4bc0886a4b9be",
	"ukr": "28a1a17d7327cad576083d5122b867843f957468b1bb11168ded23fd5aea342e",
	"usa": "d3166691d3d86f113c0d8db52506f4b72936513691d1593f47010fed01fc0b93",
	"vnm": "90067c7f246a7e85a5d501b8f3d7367ad2287816c85ebd8d44634600caf26b64",
}

const (
	disputedFile   = "ne_10m_admin_0_disputed_areas.geojson"
	disputedSha256 = "9cafef8b7dfb6b164dc58f218f981f4ace9f716f6c03795d4c62d1ac9f3d50f5"
	// what the version strings add for the union of claims
	claimsRevision = "claims1"
	taiwanCode     = "tw"
	chinaCode      = "cn"
	// interior points are taken on this many parallels across a polygon
	interiorParallels = 9
)

func viewFile(view string) string {
	return "ne_10m_admin_0_countries_" + view + ".geojson"
}

// The views in a fixed order.
func viewNames() []string {
	views := []string{}
	for view := range viewSha256s {
		views = append(views, view)
	}
	slices.Sort(views)
	return views
}

// A polygon's points to the microdegree, as text: polygons with the same key
// are the same once written.
func polygonKey(polygon [][]lagdrift.Point) string {
	b := []byte{}
	for _, ring := range polygon {
		for _, p := range ring {
			b = strconv.AppendInt(b, int64(math.Round(p.Lat*1e6)), 10)
			b = append(b, ',')
			b = strconv.AppendInt(b, int64(math.Round(p.Lon*1e6)), 10)
			b = append(b, ';')
		}
		b = append(b, '|')
	}
	return string(b)
}

// Countries being unioned: each code's polygons in the order added, and their
// keys.
type claimUnion struct {
	codes    []string
	polygons map[string][][][]lagdrift.Point
	keys     map[string]map[string]bool
	// the default polygons of Taiwan and of China, by key and as countries
	taiwanKeys map[string]bool
	chinaKeys  map[string]bool
	taiwan     *lagdrift.Country
	china      *lagdrift.Country
	// the disputed pieces each code took, for the world map
	pieceClaims map[string][][][]lagdrift.Point
}

func newClaimUnion(base []lagdrift.Country) *claimUnion {
	union := &claimUnion{
		polygons:    map[string][][][]lagdrift.Point{},
		keys:        map[string]map[string]bool{},
		taiwanKeys:  map[string]bool{},
		chinaKeys:   map[string]bool{},
		taiwan:      &lagdrift.Country{Code: taiwanCode},
		china:       &lagdrift.Country{Code: chinaCode},
		pieceClaims: map[string][][][]lagdrift.Point{},
	}
	for _, country := range base {
		for _, polygon := range country.Polygons {
			union.add(country.Code, polygon, false)
			switch country.Code {
			case taiwanCode:
				union.taiwanKeys[polygonKey(polygon)] = true
				union.taiwan.Polygons = append(union.taiwan.Polygons, polygon)
			case chinaCode:
				union.chinaKeys[polygonKey(polygon)] = true
				union.china.Polygons = append(union.china.Polygons, polygon)
			}
		}
	}
	return union
}

// Adds a polygon to a code unless the code has it already, and says whether
// it did. A claim beyond the default (extra) isn't added to a code but tw when
// it is Taiwan's, nor to tw when it is China's.
func (self *claimUnion) add(code string, polygon [][]lagdrift.Point, extra bool) bool {
	key := polygonKey(polygon)
	if self.keys[code][key] {
		return false
	}
	if extra {
		if code != taiwanCode && (self.taiwanKeys[key] || containsAny(self.taiwan, interiorPoints(polygon))) {
			return false
		}
		if code == taiwanCode && (self.chinaKeys[key] || containsAny(self.china, interiorPoints(polygon))) {
			return false
		}
	}
	keys, ok := self.keys[code]
	if !ok {
		keys = map[string]bool{}
		self.keys[code] = keys
		self.codes = append(self.codes, code)
	}
	keys[key] = true
	self.polygons[code] = append(self.polygons[code], polygon)
	return true
}

func containsAny(country *lagdrift.Country, points []lagdrift.Point) bool {
	for _, p := range points {
		if country.Contains(p) {
			return true
		}
	}
	return false
}

// Points inside a polygon: on each of 9 parallels evenly across its outline's
// latitudes, the middle of every stretch between two crossings of its rings
// that is inside it (crossings alternate in and out, holes included).
func interiorPoints(polygon [][]lagdrift.Point) []lagdrift.Point {
	_, minLat, _, maxLat := outlineBox(polygon)
	points := []lagdrift.Point{}
	for k := 1; k <= interiorParallels; k++ {
		lat := minLat + (maxLat-minLat)*float64(k)/float64(interiorParallels+1)
		crossings := []float64{}
		for _, ring := range polygon {
			for i := range ring {
				a := ring[i]
				b := ring[(i+1)%len(ring)]
				if (lat < a.Lat) != (lat < b.Lat) {
					crossings = append(crossings, (b.Lon-a.Lon)*(lat-a.Lat)/(b.Lat-a.Lat)+a.Lon)
				}
			}
		}
		slices.Sort(crossings)
		for i := 0; i+1 < len(crossings); i += 2 {
			if crossings[i] < crossings[i+1] {
				points = append(points, lagdrift.Point{Lat: lat, Lon: (crossings[i] + crossings[i+1]) / 2})
			}
		}
	}
	return points
}

// The union's countries, sorted by code.
func (self *claimUnion) countries() []lagdrift.Country {
	codes := slices.Clone(self.codes)
	slices.Sort(codes)
	countries := []lagdrift.Country{}
	for _, code := range codes {
		countries = append(countries, lagdrift.Country{Code: code, Polygons: self.polygons[code]})
	}
	return countries
}

// The default layer's codes for each ADM0_A3, for reading the disputed
// layer's owners.
func adm0Codes(data []byte) (map[string][]string, error) {
	collection, err := parseCollection(data)
	if err != nil {
		return nil, err
	}
	codes := map[string][]string{}
	for _, f := range collection.Features {
		adm0 := f.text("ADM0_A3")
		featureCodes, err := featureCodes(adm0, f.text("ISO_A2_EH"))
		if err != nil {
			return nil, err
		}
		codes[adm0] = featureCodes
	}
	return codes, nil
}

// Adds the disputed layer's pieces: each to the codes of its de facto owner
// (ADM0_A3) and of the owners its views give it (ADM0_A3_<view>). The de facto
// owner mostly has the piece already, but not always: the Spratlys' piece holds
// islands the default layer's Spratlys lack. An owner the default layer
// doesn't have (a breakaway or an indeterminate area's own code) adds nothing,
// and a piece of a territory that a split codes apart goes to the split's
// code. Taiwan's piece goes to tw alone.
func (self *claimUnion) addDisputed(data []byte, owners map[string][]string) error {
	collection, err := parseCollection(data)
	if err != nil {
		return err
	}
	for _, f := range collection.Features {
		adm0 := f.text("ADM0_A3")
		polygons, err := geometryPolygons(f.Geometry.Type, f.Geometry.Coordinates)
		if err != nil {
			return fmt.Errorf("disputed %s: %w", f.text("BRK_NAME"), err)
		}
		claimants := []string{adm0}
		for key := range f.Properties {
			if !strings.HasPrefix(key, "ADM0_A3_") {
				continue
			}
			if owner := f.text(key); owner != "" {
				claimants = append(claimants, owner)
			}
		}
		slices.Sort(claimants)
		claimants = slices.Compact(claimants)
		for _, polygon := range polygons {
			minLon, minLat, maxLon, maxLat := outlineBox(polygon)
			for _, owner := range claimants {
				for _, code := range splitCodes(owner, owners[owner], minLon, minLat, maxLon, maxLat) {
					if adm0 == "TWN" && code != taiwanCode {
						continue
					}
					if self.add(code, polygon, true) {
						self.pieceClaims[code] = append(self.pieceClaims[code], polygon)
					}
				}
			}
		}
	}
	return nil
}

// An owner's codes for a polygon in a box, a split's code where the owner's
// territory is split apart there.
func splitCodes(adm0 string, codes []string, minLon float64, minLat float64, maxLon float64, maxLat float64) []string {
	for _, s := range splits[adm0] {
		if s.contains(minLon, minLat, maxLon, maxLat) {
			return []string{s.code}
		}
	}
	return codes
}

// Counts the disputed pieces China or Taiwan holds de facto as that side's,
// for keeping the two apart.
func (self *claimUnion) addDeFactoTerritory(data []byte) error {
	collection, err := parseCollection(data)
	if err != nil {
		return err
	}
	for _, f := range collection.Features {
		var side *lagdrift.Country
		var keys map[string]bool
		switch f.text("ADM0_A3") {
		case "CHN":
			side, keys = self.china, self.chinaKeys
		case "TWN":
			side, keys = self.taiwan, self.taiwanKeys
		default:
			continue
		}
		polygons, err := geometryPolygons(f.Geometry.Type, f.Geometry.Coordinates)
		if err != nil {
			return err
		}
		for _, polygon := range polygons {
			side.Polygons = append(side.Polygons, polygon)
			keys[polygonKey(polygon)] = true
		}
	}
	return nil
}

// The union of the default countries with every point of view's and the
// disputed pieces' claims, sorted by code, and the disputed pieces each code
// took. A point of view's polygons mostly recombine what the default layer
// already gives (Kazakhstan with Baykonur, India with Kashmir's pieces), and
// the pieces carry its claims.
func withClaims(base []lagdrift.Country, defaultData []byte, readView func(view string) ([]byte, error), disputedData []byte) ([]lagdrift.Country, map[string][][][]lagdrift.Point, error) {
	union := newClaimUnion(base)
	if err := union.addDeFactoTerritory(disputedData); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", disputedFile, err)
	}
	for _, view := range viewNames() {
		data, err := readView(view)
		if err != nil {
			return nil, nil, err
		}
		countries, err := convert(data, false)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", viewFile(view), err)
		}
		for _, country := range countries {
			for _, polygon := range country.Polygons {
				union.add(country.Code, polygon, true)
			}
		}
	}
	owners, err := adm0Codes(defaultData)
	if err != nil {
		return nil, nil, err
	}
	if err := union.addDisputed(disputedData, owners); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", disputedFile, err)
	}
	countries := union.countries()
	return countries, union.pieceClaims, nil
}

// G3's list (lag.dev/DRIFTTEST.md G3), checked on every file this command
// writes: Natural Earth's disputed pieces by name, or places, each with the
// codes that must hold it and the codes that must not. only means no code but
// the holders.
type claimCheck struct {
	name     string
	pieces   []string
	places   []lagdrift.Point
	holders  []string
	excluded []string
	only     bool
	// islands too small for the 1:50m map to be held to
	small bool
}

var claimChecks = []claimCheck{
	{name: "Crimea", pieces: []string{"Crimea"}, holders: []string{"ru", "ua"}},
	{name: "Indian-held Kashmir", pieces: []string{"Jammu and Kashmir"}, holders: []string{"in", "pk"}},
	{name: "Pakistani-held Kashmir", pieces: []string{"Gilgit-Baltistan", "Azad Kashmir"}, holders: []string{"in", "pk"}},
	{name: "Aksai Chin", pieces: []string{"Aksai Chin"}, holders: []string{"cn", "in"}, excluded: []string{taiwanCode}},
	{name: "the Shaksgam valley", pieces: []string{"Shaksam Valley"}, holders: []string{"cn", "in"}, excluded: []string{taiwanCode}},
	{name: "Siachen", pieces: []string{"Siachen Glacier"}, holders: []string{"in", "pk"}},
	{name: "Western Sahara", pieces: []string{"W. Sahara"}, holders: []string{"ma", "eh"}},
	{name: "Abkhazia", pieces: []string{"Abkhazia"}, holders: []string{"ge"}},
	{name: "South Ossetia", pieces: []string{"South Ossetia"}, holders: []string{"ge"}},
	{name: "the Golan", pieces: []string{"Golan Heights"}, holders: []string{"il", "sy"}},
	{name: "Kosovo", pieces: []string{"Kosovo"}, holders: []string{"xk", "rs"}},
	{name: "the Paracels", pieces: []string{"Paracel Is."}, holders: []string{"cn", "vn"}, excluded: []string{taiwanCode}, small: true},
	{name: "the Senkakus", pieces: []string{"Pinnacle Is."}, holders: []string{"jp", "cn", "tw"}, small: true},
	{name: "the Spratlys", pieces: []string{"Spratly Is."}, holders: []string{"bn", "cn", "my", "ph", "tw", "vn"}, small: true},
	{name: "Northern Cyprus", pieces: []string{"N. Cyprus"}, holders: []string{"cy"}},
	{name: "Bir Tawil", pieces: []string{"Bir Tawil"}, holders: []string{"eg", "sd"}},
	{name: "Taiwan", pieces: []string{"Taiwan"}, places: []lagdrift.Point{{Lat: 25.03, Lon: 121.57}}, holders: []string{taiwanCode}, only: true},
	{name: "Kinmen", places: []lagdrift.Point{{Lat: 24.45926, Lon: 118.37746}}, holders: []string{taiwanCode}, only: true, small: true},
	{name: "Beijing and Shanghai", places: []lagdrift.Point{{Lat: 39.90, Lon: 116.40}, {Lat: 31.23, Lon: 121.47}}, holders: []string{chinaCode}, excluded: []string{taiwanCode}},
}

// Checks G3's list on borders. A piece is checked at its interior points
// (interiorPoints); on the simplified world map (central), only at the middle
// one of its largest polygon's, and small islands not at all. A holder holds a point when it contains it or, on the world map, its
// boundary is within slackKm.
func checkClaims(borders *lagdrift.Borders, disputedData []byte, central bool, slackKm float64) error {
	collection, err := parseCollection(disputedData)
	if err != nil {
		return err
	}
	piecePoints := map[string][]lagdrift.Point{}
	for _, f := range collection.Features {
		name := strings.TrimPrefix(f.text("BRK_NAME"), "\ufeff")
		polygons, err := geometryPolygons(f.Geometry.Type, f.Geometry.Coordinates)
		if err != nil {
			return err
		}
		if central {
			// the piece's largest polygon, by its box
			largest := -1.0
			var center []lagdrift.Point
			for _, polygon := range polygons {
				minLon, minLat, maxLon, maxLat := outlineBox(polygon)
				points := interiorPoints(polygon)
				if area := (maxLon - minLon) * (maxLat - minLat); largest < area && 0 < len(points) {
					largest = area
					center = points[len(points)/2 : len(points)/2+1]
				}
			}
			piecePoints[name] = append(piecePoints[name], center...)
			continue
		}
		for _, polygon := range polygons {
			piecePoints[name] = append(piecePoints[name], interiorPoints(polygon)...)
		}
	}
	holds := func(code string, p lagdrift.Point) bool {
		country := borders.Country(code)
		if country == nil {
			return false
		}
		if country.Contains(p) {
			return true
		}
		if 0 < slackKm {
			fit := &lagdrift.Fit{At: p, Cov: [2][2]float64{{1, 0}, {0, 1}}}
			return lagdrift.Judge(fit, country, lagdrift.DefaultConfidence).DistanceKm <= slackKm
		}
		return false
	}
	failures := []string{}
	for _, check := range claimChecks {
		if central && check.small {
			continue
		}
		points := slices.Clone(check.places)
		for _, piece := range check.pieces {
			if len(piecePoints[piece]) == 0 {
				failures = append(failures, fmt.Sprintf("%s: no disputed piece %q", check.name, piece))
			}
			points = append(points, piecePoints[piece]...)
		}
		for _, p := range points {
			for _, code := range check.holders {
				if !holds(code, p) {
					failures = append(failures, fmt.Sprintf("%s: %s doesn't hold %.5f, %.5f", check.name, code, p.Lat, p.Lon))
				}
			}
			excluded := check.excluded
			if check.only {
				excluded = borders.Codes()
			}
			for _, code := range excluded {
				if slices.Contains(check.holders, code) {
					continue
				}
				if country := borders.Country(code); country != nil && country.Contains(p) {
					failures = append(failures, fmt.Sprintf("%s: %s holds %.5f, %.5f", check.name, code, p.Lat, p.Lon))
				}
			}
		}
	}
	if 0 < len(failures) {
		if 12 < len(failures) {
			failures = append(failures[:12], fmt.Sprintf("and %d more", len(failures)-12))
		}
		return fmt.Errorf("G3's claims don't hold:\n  %s", strings.Join(failures, "\n  "))
	}
	return nil
}
