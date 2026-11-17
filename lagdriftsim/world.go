package lagdriftsim

// The simulated world: the fleet's extenders with their place errors, the
// countries, and the visitors of each scenario.
//
// An extender's place is GeoLite2's point for its city, with GeoLite2's
// accuracy radius for its address, which the model holds to [10, 100] km, or
// 50 km for a derived place (V3, A4). The accuracy radius is about a 67%
// radius, so the extender's true place is off by a normal draw with a
// per-axis σ of radius / 1.5. Each fleet is one draw of every extender's
// place error, and visitors are spread over 16 fleets, so the rates are
// averaged over fleets rather than conditioned on one.

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"

	"github.com/urnetwork/connect/lagdrift"
)

const (
	fleetCount = 16
	// GeoLite2's accuracy radius as about a 67% radius
	radiusPerSigma = 1.5
	// a derived place's share, and its error, km
	derivedPlaceShare = 0.10
	// a visitor is this far at most from their city's point, km
	visitorSpreadKm = 15
	// an extender serves now with this chance (A4)
	servingShare = 0.9
	// a country's polygons further than this from both the truth and the
	// address's place can't decide a verdict, km (localCountry)
	localKeepKm = 9000
)

// GeoLite2's accuracy radius for an extender's address, km, and its share.
var accuracyRadii = []struct {
	km    float64
	share float64
}{
	{km: 10, share: 0.30},
	{km: 20, share: 0.25},
	{km: 50, share: 0.25},
	{km: 100, share: 0.15},
	{km: 200, share: 0.05},
}

// One extender of the pool.
type poolExtender struct {
	id   string
	site *extenderSite
}

// An extender's place in one fleet.
type fleetExtender struct {
	// the model's: GeoLite2's point for its city
	at lagdrift.Point
	// GeoLite2's accuracy radius, km; 0 for a derived place, which the model
	// holds at 50
	placeErrorKm float64
	// where it is
	trueAt    lagdrift.Point
	misplaced bool
}

// The fleet and the borders, shared and read-only once made.
type world struct {
	pool   []poolExtender
	fleets [][]fleetExtender
	// nil: synthetic countries only
	borders *lagdrift.Borders
}

// How the world is drawn.
type worldConfig struct {
	seed uint64
	// an extender that is somewhere else entirely, 300 to 1,500 km from its
	// place: a share of the fleet, 0 in the study's main world
	misplacedShare float64
	// sites added to extenderSites
	extraSites []extenderSite
}

func newWorld(config worldConfig, borders *lagdrift.Borders) *world {
	w := &world{borders: borders}
	sites := append(slices.Clone(extenderSites), config.extraSites...)
	for i := range sites {
		s := &sites[i]
		for k := 1; k <= s.count; k++ {
			w.pool = append(w.pool, poolExtender{
				id:   fmt.Sprintf("%s/%d", s.name, k),
				site: s,
			})
		}
	}
	w.fleets = make([][]fleetExtender, fleetCount)
	for f := range w.fleets {
		w.fleets[f] = make([]fleetExtender, len(w.pool))
		for i, e := range w.pool {
			r := newRand(config.seed, drawFleet, uint64(f), stringKey(e.id))
			w.fleets[f][i] = drawFleetExtender(e.site.at, config.misplacedShare, r)
		}
	}
	return w
}

func drawFleetExtender(at lagdrift.Point, misplacedShare float64, r *rand.Rand) fleetExtender {
	e := fleetExtender{at: at}
	sigmaKm := 0.0
	if r.Float64() < derivedPlaceShare {
		e.placeErrorKm = 0
		sigmaKm = lagdrift.DerivedPlaceErrorKm / radiusPerSigma
	} else {
		u := r.Float64()
		for _, radius := range accuracyRadii {
			e.placeErrorKm = radius.km
			if u < radius.share {
				break
			}
			u -= radius.share
		}
		sigmaKm = e.placeErrorKm / radiusPerSigma
	}
	e.trueAt = lagdrift.MoveKm(at, sigmaKm*r.NormFloat64(), sigmaKm*r.NormFloat64())
	if r.Float64() < misplacedShare {
		e.misplaced = true
		e.trueAt = destination(at, uniform(r, 0, 360), uniform(r, 300, 1500))
	}
	return e
}

// The kinds of visitor.
const (
	// the address's country is the visitor's
	kindHonest = "honest"
	// the address claims a country claimKm away
	kindClaim = "claim"
)

// The geometries a scenario's countries come from.
const (
	geometryReal      = "real"
	geometrySynthetic = "synthetic"
)

// A group of visitors the study reports together.
type scenario struct {
	name     string
	kind     string
	geometry string
	// a synthetic honest visitor's country: a disk of radiusKm, with the truth
	// depthKm inside its edge, or anywhere inside when depthKm < 0
	radiusKm float64
	depthKm  float64
	// a claim's country: its nearest point is about this far from the truth, km
	claimKm float64
}

// One simulated visitor: where they are, what their address says, and the
// countries the verdict judges against.
type visitorSpec struct {
	index    int
	scenario *scenario
	region   string
	city     visitorCity
	truth    lagdrift.Point
	// the address's place and country
	ipPlace   lagdrift.Point
	ipCode    string
	ipCountry *lagdrift.Country
	// the place an address claims, for a claim
	claimName string
	// the truth's distance from the IP country's border, km: inside it for an
	// honest visitor, outside for a claim
	borderKm float64
	fleet    int
}

// A visitor's truth: their city's point, moved up to 15 km.
func drawTruth(c visitorCity, r *rand.Rand) lagdrift.Point {
	return destination(c.at, uniform(r, 0, 360), visitorSpreadKm*math.Sqrt(r.Float64()))
}

// The cities of each region, in visitorCities' order.
func citiesByRegion() map[string][]visitorCity {
	regionCities := map[string][]visitorCity{}
	for _, c := range visitorCities {
		regionCities[c.region] = append(regionCities[c.region], c)
	}
	return regionCities
}

// A synthetic honest visitor: the IP country is a disk around the truth, with
// the truth at the scenario's depth inside its edge, in a random direction.
func syntheticHonest(index int, sc *scenario, c visitorCity, seed uint64) *visitorSpec {
	r := newRand(seed, drawScenario, stringKey(sc.name), uint64(index))
	truth := drawTruth(c, r)
	depthKm := sc.depthKm
	if depthKm < 0 {
		// uniform over the disk
		depthKm = sc.radiusKm * (1 - math.Sqrt(r.Float64()))
	}
	center := destination(truth, uniform(r, 0, 360), sc.radiusKm-depthKm)
	country := diskCountry("zz", center, sc.radiusKm, 72)
	return &visitorSpec{
		index:     index,
		scenario:  sc,
		region:    c.region,
		city:      c,
		truth:     truth,
		ipPlace:   c.at,
		ipCode:    "zz",
		ipCountry: country,
		borderKm:  depthKm,
		fleet:     index % fleetCount,
	}
}

// Places an address can claim: every place the study knows, by country.
func claimCandidates() []place {
	candidates := slices.Clone(claimPlaces)
	for _, c := range visitorCities {
		candidates = append(candidates, c.place)
	}
	for _, s := range extenderSites {
		candidates = append(candidates, s.place)
	}
	return candidates
}

// A synthetic claim: the claimed country is a disk around a real city, sized
// so its nearest point is exactly the scenario's distance from the truth. The
// city is 50 to 500 km farther than that, so the disk's radius is 50 to 500 km.
// nil when no city fits.
func syntheticClaim(index int, sc *scenario, c visitorCity, candidates []place, seed uint64) *visitorSpec {
	r := newRand(seed, drawScenario, stringKey(sc.name), uint64(index))
	truth := drawTruth(c, r)
	fits := []place{}
	for _, p := range candidates {
		d := lagdrift.DistanceKm(truth, p.at)
		if sc.claimKm+50 <= d && d <= sc.claimKm+500 {
			fits = append(fits, p)
		}
	}
	if len(fits) == 0 {
		return nil
	}
	claimed := fits[r.IntN(len(fits))]
	radiusKm := lagdrift.DistanceKm(truth, claimed.at) - sc.claimKm
	return &visitorSpec{
		index:     index,
		scenario:  sc,
		region:    c.region,
		city:      c,
		truth:     truth,
		ipPlace:   claimed.at,
		ipCode:    "zz",
		ipCountry: diskCountry("zz", claimed.at, radiusKm, 72),
		claimName: claimed.name,
		borderKm:  sc.claimKm,
		fleet:     index % fleetCount,
	}
}

// A real honest visitor: their city's country from the borders, with the
// truth inside it. nil when the borders lack the country or the city's point
// isn't in it.
func realHonest(index int, sc *scenario, c visitorCity, borders *lagdrift.Borders, seed uint64) *visitorSpec {
	country := borders.Country(c.code)
	if country == nil || !country.Contains(c.at) {
		return nil
	}
	r := newRand(seed, drawScenario, stringKey(sc.name), uint64(index))
	truth := drawTruth(c, r)
	for tries := 0; !country.Contains(truth); tries += 1 {
		if 20 <= tries {
			truth = c.at
			break
		}
		truth = drawTruth(c, r)
	}
	local := localCountry(country, localKeepKm, truth, c.at)
	_, borderKm := borderDistanceKm(local, truth)
	return &visitorSpec{
		index:     index,
		scenario:  sc,
		region:    c.region,
		city:      c,
		truth:     truth,
		ipPlace:   c.at,
		ipCode:    c.code,
		ipCountry: local,
		borderKm:  borderKm,
		fleet:     index % fleetCount,
	}
}

// A country an address can claim, and its distance from a city.
type claimOption struct {
	claimed    place
	distanceKm float64
}

// A real claim: a country whose nearest point is within 0.8 to 1.25 times the
// scenario's distance from the truth, its address placed at the country's
// claim place. options are the city's, from realClaimOptions. nil when no
// country fits.
func realClaim(index int, sc *scenario, c visitorCity, options []claimOption, borders *lagdrift.Borders, seed uint64) *visitorSpec {
	r := newRand(seed, drawScenario, stringKey(sc.name), uint64(index))
	fits := []claimOption{}
	for _, o := range options {
		if 0.8*sc.claimKm <= o.distanceKm && o.distanceKm <= 1.25*sc.claimKm {
			fits = append(fits, o)
		}
	}
	if len(fits) == 0 {
		return nil
	}
	o := fits[r.IntN(len(fits))]
	truth := drawTruth(c, r)
	country := borders.Country(o.claimed.code)
	local := localCountry(country, localKeepKm, truth, o.claimed.at)
	inside, borderKm := borderDistanceKm(local, truth)
	if inside {
		return nil
	}
	return &visitorSpec{
		index:     index,
		scenario:  sc,
		region:    c.region,
		city:      c,
		truth:     truth,
		ipPlace:   o.claimed.at,
		ipCode:    o.claimed.code,
		ipCountry: local,
		claimName: o.claimed.name,
		borderKm:  borderKm,
		fleet:     index % fleetCount,
	}
}

// For each visitor city, the countries an address can claim and their
// distances from the city's point, from the borders: one claim place per
// country, the first listed, and only countries whose borders hold it.
func realClaimOptions(borders *lagdrift.Borders) map[string][]claimOption {
	claimed := []place{}
	seen := map[string]bool{}
	for _, p := range claimPlaces {
		country := borders.Country(p.code)
		if seen[p.code] || country == nil || !country.Contains(p.at) {
			continue
		}
		seen[p.code] = true
		claimed = append(claimed, p)
	}
	options := map[string][]claimOption{}
	for _, c := range visitorCities {
		for _, p := range claimed {
			if p.code == c.code {
				continue
			}
			inside, d := borderDistanceKm(borders.Country(p.code), c.at)
			if inside {
				continue
			}
			options[c.name+"/"+c.code] = append(options[c.name+"/"+c.code], claimOption{claimed: p, distanceKm: d})
		}
	}
	return options
}
