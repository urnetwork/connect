package main

// Disputed areas (lag.dev/DRIFTTEST.md G3, the user's decisions of
// 2026-11-17): a claim counts only inside a piece of Natural Earth's
// disputed-areas layer (ne_10m_admin_0_disputed_areas), and there each piece
// belongs to every country with a claim to it, except that Taiwan and China
// are kept apart. Everywhere else the default layer alone decides, so no
// country's map counts over another's undisputed territory.
//
// Each piece goes to:
//   - its de facto owner: its admin (ADM0_A3) and its sovereign (SOV_A3, as
//     the default layer's home part of that sovereignty), which differ for a
//     dependency (Gibraltar: gi and gb) and for the West Bank and Gaza
//     (Palestine's, under Israel);
//   - the owner every point of view gives it (ADM0_A3_<view>);
//   - every claimant its note names (NOTE_BRK's "Claimed by …"), which carries
//     the claims of countries without a point of view: Sudan's to Halaib,
//     South Sudan's to Abyei, Venezuela's to Essequibo (claimantCodes).
//
// An owner the default layer lacks (a breakaway's or an indeterminate area's
// own code, such as Abkhazia's B35) adds nothing, and an owner's territory
// that a split codes apart gives the split's code (codes.go).
//
// Two pieces stay their de facto owner's alone (undisputedPieces): Taiwan's,
// which several views give to China, and Israel's own, which the views that
// don't recognize Israel give to Palestine. G3 counts neither, since their
// control isn't contested and IP labels vary only where it is.
//
// Taiwan and China are kept apart both ways (G3): no piece goes to tw where
// the default layer gives cn alone or China holds it de facto (the Paracels,
// Aksai Chin and the Shaksgam valley, which Taiwan's view gives to tw), and
// none goes to cn where the default layer gives tw alone or Taiwan holds it.
// A piece is the other side's when one of its interior points
// (interiorPoints) is, or it is one of that side's polygons to the
// microdegree. What the default layer gives both, the Spratlys and
// Scarborough Reef (codes.go), is neither side's alone.
//
// A code whose default polygons already hold every interior point of a
// piece's polygon isn't given the polygon again, so a de facto owner's map
// gains no line inside its own territory; a code that holds some or none of
// it gets the polygon whole.

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/urnetwork/connect/lagdrift"
)

const (
	disputedFile   = "ne_10m_admin_0_disputed_areas.geojson"
	disputedSha256 = "9cafef8b7dfb6b164dc58f218f981f4ace9f716f6c03795d4c62d1ac9f3d50f5"
	// what the version strings add for the claims: claims1 was the union of
	// every point of view; claims2 counts claims only in disputed pieces
	claimsRevision = "claims2"
	taiwanCode     = "tw"
	chinaCode      = "cn"
	// interior points are taken on this many parallels across a polygon
	interiorParallels = 9
)

// A piece of the disputed layer, by ADM0_A3 and BRK_NAME.
type pieceId struct {
	adm0 string
	name string
}

// The pieces that stay their de facto owner's alone (G3). Each must match one
// piece of the pinned layer.
var undisputedPieces = []pieceId{
	// several views give Taiwan to China, and cross-strait claims are left
	// out both ways
	{adm0: "TWN", name: "Taiwan"},
	// the views that don't recognize Israel (Pakistan's, Saudi Arabia's,
	// Indonesia's and Bangladesh's) give all of it to Palestine
	{adm0: "ISR", name: "Israel"},
}

// The codes of the claimants NOTE_BRK names, as Natural Earth writes them
// after "Claimed by", without a leading "the". "multiple" names none. A name
// missing here fails the build, so no claimant is dropped unseen.
var claimantCodes = map[string][]string{
	"Argentina":         {"ar"},
	"Azer.":             {"az"},
	"Brazil":            {"br"},
	"Brunei":            {"bn"},
	"Canada":            {"ca"},
	"Chile":             {"cl"},
	"China":             {"cn"},
	"Colombia":          {"co"},
	"Comoros":           {"km"},
	"Cuba":              {"cu"},
	"Cyprus":            {"cy"},
	"Denmark":           {"dk"},
	"Djibouti":          {"dj"},
	"Equatorial Guinea": {"gq"},
	"Georgia":           {"ge"},
	"Guatemala":         {"gt"},
	"Haiti":             {"ht"},
	"Honduras":          {"hn"},
	"Iceland":           {"is"},
	"India":             {"in"},
	"Ireland":           {"ie"},
	"Jamaica":           {"jm"},
	"Japan":             {"jp"},
	"Lebanon":           {"lb"},
	"Madagascar":        {"mg"},
	"Malaysia":          {"my"},
	"Marshall Islands":  {"mh"},
	"Mauritius":         {"mu"},
	"Moldova":           {"md"},
	"Morocco":           {"ma"},
	"Nepal":             {"np"},
	"Nicaragua":         {"ni"},
	"Pakistan":          {"pk"},
	"Palestine":         {"ps"},
	"Philippines":       {"ph"},
	"Portugal":          {"pt"},
	"Saudi Arabia":      {"sa"},
	"Seychelles":        {"sc"},
	"Somalia":           {"so"},
	"South Sudan":       {"ss"},
	"Spain":             {"es"},
	"Sudan":             {"sd"},
	"Suriname":          {"sr"},
	"Syria":             {"sy"},
	"Taiwan":            {"tw"},
	"Tokelau":           {"tk"},
	"UAE":               {"ae"},
	"Ukraine":           {"ua"},
	"United States":     {"us"},
	"Uruguay":           {"uy"},
	"Vanuatu":           {"vu"},
	"Venezuela":         {"ve"},
	"Vietnam":           {"vn"},
	"Western Sahara":    {"eh"},
	"multiple":          {},
}

// The codes of the claimants a piece's note names: each clause "Claimed by A,
// B, and C" (or "A and B"), the others ignored.
func noteClaimants(note string) ([]string, error) {
	codes := []string{}
	for _, clause := range strings.Split(note, ";") {
		names, ok := strings.CutPrefix(strings.TrimSpace(clause), "Claimed by ")
		if !ok {
			continue
		}
		names = strings.ReplaceAll(names, ", and ", ", ")
		names = strings.ReplaceAll(names, " and ", ", ")
		for _, name := range strings.Split(names, ",") {
			name = strings.TrimPrefix(strings.TrimSpace(name), "the ")
			named, ok := claimantCodes[name]
			if !ok {
				return nil, fmt.Errorf("the claimant %q of %q has no code", name, note)
			}
			codes = append(codes, named...)
		}
	}
	return codes, nil
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

// One polygon of a disputed piece, and the codes holding it once the claims
// are applied, sorted.
type claim struct {
	piece   string
	polygon [][]lagdrift.Point
	codes   []string
}

// The countries with the claims applied: each code's polygons in the order
// added, and their keys.
type claimUnion struct {
	codes    []string
	polygons map[string][][][]lagdrift.Point
	keys     map[string]map[string]bool
	// the default layer, for whether a code holds a piece already
	defaults *lagdrift.Borders
	// the territory of Taiwan and of China, by key and as countries: the
	// default polygons each holds and the other doesn't, and the disputed
	// pieces each holds de facto
	taiwanKeys map[string]bool
	chinaKeys  map[string]bool
	taiwan     *lagdrift.Country
	china      *lagdrift.Country
}

func newClaimUnion(base []lagdrift.Country) (*claimUnion, error) {
	defaults, err := lagdrift.NewBorders("", base)
	if err != nil {
		return nil, err
	}
	union := &claimUnion{
		polygons:   map[string][][][]lagdrift.Point{},
		keys:       map[string]map[string]bool{},
		defaults:   defaults,
		taiwanKeys: map[string]bool{},
		chinaKeys:  map[string]bool{},
		taiwan:     &lagdrift.Country{Code: taiwanCode},
		china:      &lagdrift.Country{Code: chinaCode},
	}
	for _, country := range base {
		for _, polygon := range country.Polygons {
			union.add(country.Code, polygon)
		}
	}
	for _, polygon := range union.polygons[taiwanCode] {
		if key := polygonKey(polygon); !union.keys[chinaCode][key] {
			union.taiwanKeys[key] = true
			union.taiwan.Polygons = append(union.taiwan.Polygons, polygon)
		}
	}
	for _, polygon := range union.polygons[chinaCode] {
		if key := polygonKey(polygon); !union.keys[taiwanCode][key] {
			union.chinaKeys[key] = true
			union.china.Polygons = append(union.china.Polygons, polygon)
		}
	}
	return union, nil
}

// Adds a polygon to a code unless the code has it already.
func (self *claimUnion) add(code string, polygon [][]lagdrift.Point) {
	key := polygonKey(polygon)
	if self.keys[code][key] {
		return
	}
	keys, ok := self.keys[code]
	if !ok {
		keys = map[string]bool{}
		self.keys[code] = keys
		self.codes = append(self.codes, code)
	}
	keys[key] = true
	self.polygons[code] = append(self.polygons[code], polygon)
}

// Gives a piece's polygon, with its interior points, to a code, and says
// whether the code holds it: not when the polygon is the other side of the
// strait's. The code takes the polygon unless its default polygons hold
// every interior point already.
func (self *claimUnion) give(code string, polygon [][]lagdrift.Point, points []lagdrift.Point) bool {
	key := polygonKey(polygon)
	if code != taiwanCode && (self.taiwanKeys[key] || containsAny(self.taiwan, points)) {
		return false
	}
	if code == taiwanCode && (self.chinaKeys[key] || containsAny(self.china, points)) {
		return false
	}
	if !self.defaultHolds(code, points) {
		self.add(code, polygon)
	}
	return true
}

// The code's default polygons hold every point, of at least one.
func (self *claimUnion) defaultHolds(code string, points []lagdrift.Point) bool {
	country := self.defaults.Country(code)
	if country == nil || len(points) == 0 {
		return false
	}
	for _, p := range points {
		if !country.Contains(p) {
			return false
		}
	}
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
// layer's owners, and the ADM0_A3 of each sovereignty's home part, by SOV_A3
// (Israel's IS1 is ISR).
func defaultOwners(data []byte) (map[string][]string, map[string]string, error) {
	collection, err := parseCollection(data)
	if err != nil {
		return nil, nil, err
	}
	codes := map[string][]string{}
	sovereigns := map[string]string{}
	for _, f := range collection.Features {
		adm0 := f.text("ADM0_A3")
		featureCodes, err := featureCodes(adm0, f.text("ISO_A2_EH"))
		if err != nil {
			return nil, nil, err
		}
		codes[adm0] = featureCodes
		if homePart, _ := f.Properties["HOMEPART"].(float64); homePart == 1 {
			sovereign := f.text("SOV_A3")
			if other, ok := sovereigns[sovereign]; ok && other != adm0 {
				return nil, nil, fmt.Errorf("sovereignty %s has two home parts, %s and %s", sovereign, other, adm0)
			}
			sovereigns[sovereign] = adm0
		}
	}
	return codes, sovereigns, nil
}

// A piece's name, without the byte-order mark Natural Earth leaves on one.
func pieceName(f feature) string {
	return strings.TrimPrefix(f.text("BRK_NAME"), "\ufeff")
}

// Counts the disputed pieces China or Taiwan holds de facto as that side's,
// for keeping the two apart.
func (self *claimUnion) addDeFactoTerritory(collection *featureCollection) error {
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

// The default countries with every disputed piece given to its claimants,
// sorted by code, and the claims: each piece's polygons with the codes that
// hold them.
func withClaims(base []lagdrift.Country, defaultData []byte, disputedData []byte) ([]lagdrift.Country, []claim, error) {
	union, err := newClaimUnion(base)
	if err != nil {
		return nil, nil, err
	}
	collection, err := parseCollection(disputedData)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", disputedFile, err)
	}
	if err := union.addDeFactoTerritory(collection); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", disputedFile, err)
	}
	owners, sovereigns, err := defaultOwners(defaultData)
	if err != nil {
		return nil, nil, err
	}
	undisputed := make([]int, len(undisputedPieces))
	claims := []claim{}
	for _, f := range collection.Features {
		id := pieceId{adm0: f.text("ADM0_A3"), name: pieceName(f)}
		polygons, err := geometryPolygons(f.Geometry.Type, f.Geometry.Coordinates)
		if err != nil {
			return nil, nil, fmt.Errorf("%s %s: %w", disputedFile, id.name, err)
		}
		// the owners by ADM0_A3, and the codes the note names
		claimants := []string{id.adm0}
		named := []string{}
		if i := slices.Index(undisputedPieces, id); 0 <= i {
			undisputed[i] += 1
		} else {
			if sovereign, ok := sovereigns[f.text("SOV_A3")]; ok {
				claimants = append(claimants, sovereign)
			}
			for key := range f.Properties {
				if strings.HasPrefix(key, "ADM0_A3_") {
					if owner := f.text(key); owner != "" {
						claimants = append(claimants, owner)
					}
				}
			}
			named, err = noteClaimants(f.text("NOTE_BRK"))
			if err != nil {
				return nil, nil, fmt.Errorf("%s %s: %w", disputedFile, id.name, err)
			}
		}
		slices.Sort(claimants)
		claimants = slices.Compact(claimants)
		for _, polygon := range polygons {
			minLon, minLat, maxLon, maxLat := outlineBox(polygon)
			codes := slices.Clone(named)
			for _, owner := range claimants {
				codes = append(codes, splitCodes(owner, owners[owner], minLon, minLat, maxLon, maxLat)...)
			}
			slices.Sort(codes)
			codes = slices.Compact(codes)
			points := interiorPoints(polygon)
			c := claim{piece: id.name, polygon: polygon}
			for _, code := range codes {
				if union.give(code, polygon, points) {
					c.codes = append(c.codes, code)
				}
			}
			claims = append(claims, c)
		}
	}
	for i, id := range undisputedPieces {
		if undisputed[i] != 1 {
			return nil, nil, fmt.Errorf("%s: %d pieces %s of %s, not 1", disputedFile, undisputed[i], id.name, id.adm0)
		}
	}
	return union.countries(), claims, nil
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
	// a place the 1:50m map can't be held to: Kinmen is 2 km off the
	// mainland, which the map simplifies at 5 km
	notOnWorld bool
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
	{name: "the Paracels", pieces: []string{"Paracel Is."}, holders: []string{"cn", "vn"}, excluded: []string{taiwanCode}},
	{name: "the Senkakus", pieces: []string{"Pinnacle Is."}, holders: []string{"jp", "cn", "tw"}},
	{name: "the Spratlys", pieces: []string{"Spratly Is."}, holders: []string{"bn", "cn", "my", "ph", "tw", "vn"}},
	{name: "Northern Cyprus", pieces: []string{"N. Cyprus"}, holders: []string{"cy"}},
	{name: "Bir Tawil", pieces: []string{"Bir Tawil"}, holders: []string{"eg", "sd"}},
	{name: "the West Bank", pieces: []string{"West Bank"}, places: []lagdrift.Point{{Lat: 31.9038, Lon: 35.2034}}, holders: []string{"il", "ps"}},
	{name: "Gaza", pieces: []string{"Gaza"}, places: []lagdrift.Point{{Lat: 31.5017, Lon: 34.4668}}, holders: []string{"il", "ps"}},
	{name: "East Jerusalem", pieces: []string{"East Jerusalem"}, holders: []string{"il", "ps"}},
	{name: "Tel Aviv, Haifa and Beersheba", places: []lagdrift.Point{{Lat: 32.0853, Lon: 34.7818}, {Lat: 32.7940, Lon: 34.9896}, {Lat: 31.2518, Lon: 34.7913}}, holders: []string{"il"}, only: true},
	{name: "Ceuta and Melilla", pieces: []string{"Ceuta", "Melilla"}, holders: []string{"es", "ma"}},
	{name: "Gibraltar", pieces: []string{"Gibraltar"}, holders: []string{"gi", "es"}},
	{name: "the Falklands", pieces: []string{"Falkland Is."}, holders: []string{"fk", "ar"}},
	{name: "South Georgia", pieces: []string{"S. Georgia"}, holders: []string{"gs", "ar"}},
	{name: "the southern Kurils", pieces: []string{"Kuril Is."}, holders: []string{"ru", "jp"}},
	{name: "Dokdo", pieces: []string{"Dokdo"}, holders: []string{"kr", "jp"}},
	{name: "Halaib", pieces: []string{"Halayib Triangle"}, holders: []string{"eg", "sd"}},
	{name: "Abyei", pieces: []string{"Abyei"}, holders: []string{"sd", "ss"}},
	{name: "Essequibo", pieces: []string{"West of Essequibo River"}, holders: []string{"gy", "ve"}},
	{name: "Arunachal Pradesh", pieces: []string{"Arunachal Pradesh"}, holders: []string{"in", "cn", "tw"}},
	{name: "Taiwan", pieces: []string{"Taiwan"}, places: []lagdrift.Point{{Lat: 25.03, Lon: 121.57}}, holders: []string{taiwanCode}, only: true},
	{name: "Kinmen", places: []lagdrift.Point{{Lat: 24.45926, Lon: 118.37746}}, holders: []string{taiwanCode}, only: true, notOnWorld: true},
	{name: "Beijing and Shanghai", places: []lagdrift.Point{{Lat: 39.90, Lon: 116.40}, {Lat: 31.23, Lon: 121.47}}, holders: []string{chinaCode}, only: true},
	// places off G3's list, each its own country's alone
	{name: "Thimphu", places: []lagdrift.Point{{Lat: 27.4728, Lon: 89.6390}}, holders: []string{"bt"}, only: true},
	{name: "Barcelona", places: []lagdrift.Point{{Lat: 41.3874, Lon: 2.1686}}, holders: []string{"es"}, only: true},
	{name: "London", places: []lagdrift.Point{{Lat: 51.5072, Lon: -0.1276}}, holders: []string{"gb"}, only: true},
	{name: "Moscow", places: []lagdrift.Point{{Lat: 55.7558, Lon: 37.6173}}, holders: []string{"ru"}, only: true},
	{name: "Tokyo", places: []lagdrift.Point{{Lat: 35.6762, Lon: 139.6503}}, holders: []string{"jp"}, only: true},
}

// How checkClaims reads a file: the 1:10m borders exactly, at every interior
// point, or the simplified world map at one central point per polygon, with
// slack.
type claimCheckMode struct {
	world bool
	// on the world map, a holder holds a point within this of its boundary
	slackKm float64
	// on the world map, a claim's polygon is checked when its outline keeps 3
	// points at this many units per degree; a smaller one can't be drawn
	scale int
}

// Checks G3's list and every claim withClaims made on borders. A piece, or a
// claim's polygon, is checked at its interior points (interiorPoints); on the
// world map at the middle one, of each claim's polygon that the map can draw
// and of each listed piece's largest polygon.
func checkClaims(borders *lagdrift.Borders, disputedData []byte, claims []claim, mode claimCheckMode) error {
	failures, err := claimListFailures(borders, disputedData, mode)
	if err != nil {
		return err
	}
	failures = append(failures, claimFailures(borders, claims, mode)...)
	if 0 < len(failures) {
		if 12 < len(failures) {
			failures = append(failures[:12], fmt.Sprintf("and %d more", len(failures)-12))
		}
		return fmt.Errorf("G3's claims don't hold:\n  %s", strings.Join(failures, "\n  "))
	}
	return nil
}

// The middle of a polygon's interior points, if it has any.
func centralPoint(polygon [][]lagdrift.Point) []lagdrift.Point {
	points := interiorPoints(polygon)
	if len(points) == 0 {
		return nil
	}
	return points[len(points)/2 : len(points)/2+1]
}

// Whether a code of borders holds a point: contains it, or on the world map,
// has its boundary within the slack.
func holdsPoint(borders *lagdrift.Borders, code string, p lagdrift.Point, mode claimCheckMode) bool {
	country := borders.Country(code)
	if country == nil {
		return false
	}
	if country.Contains(p) {
		return true
	}
	if mode.world {
		fit := &lagdrift.Fit{At: p, Cov: [2][2]float64{{1, 0}, {0, 1}}}
		return lagdrift.Judge(fit, country, lagdrift.KForConfidence(lagdrift.DefaultConfidence)).DistanceKm <= mode.slackKm
	}
	return false
}

// G3's list's failures on borders.
func claimListFailures(borders *lagdrift.Borders, disputedData []byte, mode claimCheckMode) ([]string, error) {
	collection, err := parseCollection(disputedData)
	if err != nil {
		return nil, err
	}
	piecePoints := map[string][]lagdrift.Point{}
	for _, f := range collection.Features {
		name := pieceName(f)
		polygons, err := geometryPolygons(f.Geometry.Type, f.Geometry.Coordinates)
		if err != nil {
			return nil, err
		}
		if mode.world {
			// the piece's largest polygon, by its box
			largest := -1.0
			var center []lagdrift.Point
			for _, polygon := range polygons {
				minLon, minLat, maxLon, maxLat := outlineBox(polygon)
				if area := (maxLon - minLon) * (maxLat - minLat); largest < area {
					if points := centralPoint(polygon); 0 < len(points) {
						largest = area
						center = points
					}
				}
			}
			piecePoints[name] = append(piecePoints[name], center...)
			continue
		}
		for _, polygon := range polygons {
			piecePoints[name] = append(piecePoints[name], interiorPoints(polygon)...)
		}
	}
	failures := []string{}
	for _, check := range claimChecks {
		if mode.world && check.notOnWorld {
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
				if !holdsPoint(borders, code, p, mode) {
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
	return failures, nil
}

// The claims' failures on borders: each code of a claim must hold its
// polygon's interior points, or on the world map the middle one, of a polygon
// the map can draw.
func claimFailures(borders *lagdrift.Borders, claims []claim, mode claimCheckMode) []string {
	failures := []string{}
	for _, c := range claims {
		var points []lagdrift.Point
		if mode.world {
			if !drawable(c.polygon[0], mode.scale) {
				continue
			}
			points = centralPoint(c.polygon)
		} else {
			points = interiorPoints(c.polygon)
		}
		for _, p := range points {
			for _, code := range c.codes {
				if !holdsPoint(borders, code, p, mode) {
					failures = append(failures, fmt.Sprintf("%s: %s doesn't hold %.5f, %.5f", c.piece, code, p.Lat, p.Lon))
				}
			}
		}
	}
	return failures
}

// A ring keeps 3 distinct points at scale units per degree, as the world map
// writes it.
func drawable(ring []lagdrift.Point, scale int) bool {
	s := float64(scale)
	distinct := map[[2]int64]bool{}
	for _, p := range ring {
		distinct[[2]int64{int64(math.Round(p.Lat * s)), int64(math.Round(p.Lon * s))}] = true
	}
	return 3 <= len(distinct)
}
