package lagdriftsim

// The study's scenarios and its parts. A part is one world and one model,
// simulated under its stop policies for its scenarios' visitors.
//
// Honest visitors' countries are synthetic disks around them, at controlled
// distances from the edge, and, once the borders file is there, the real
// countries of their cities. A claim's country is a disk around a real city
// whose nearest point is exactly the claim's distance away, and, with the
// borders, a real country whose nearest point is within 0.8 to 1.25 times it.

import (
	"math"

	"github.com/urnetwork/connect/lagdrift"
)

var (
	// Luxembourg, Singapore, Andorra: a country no wider than the error, the
	// worst case for a false reject, where the region must cover the truth
	scenarioTiny = &scenario{name: "tiny country, 20 km", kind: kindHonest, geometry: geometrySynthetic, radiusKm: 20, depthKm: -1}
	// Belgium, Switzerland, Israel
	scenarioSmallEdge   = &scenario{name: "small country, 5 km in", kind: kindHonest, geometry: geometrySynthetic, radiusKm: 150, depthKm: 5}
	scenarioSmallCenter = &scenario{name: "small country, center", kind: kindHonest, geometry: geometrySynthetic, radiusKm: 150, depthKm: 150}
	// France, Spain, Germany
	scenarioLargeEdge   = &scenario{name: "large country, 5 km in", kind: kindHonest, geometry: geometrySynthetic, radiusKm: 600, depthKm: 5}
	scenarioLargeInland = &scenario{name: "large country, 100 km in", kind: kindHonest, geometry: geometrySynthetic, radiusKm: 600, depthKm: 100}

	scenarioClaim300  = &scenario{name: "claim 300 km", kind: kindClaim, geometry: geometrySynthetic, claimKm: 300}
	scenarioClaim1000 = &scenario{name: "claim 1,000 km", kind: kindClaim, geometry: geometrySynthetic, claimKm: 1000}
	scenarioClaim3000 = &scenario{name: "claim 3,000 km", kind: kindClaim, geometry: geometrySynthetic, claimKm: 3000}

	scenarioRealHonest    = &scenario{name: "real country", kind: kindHonest, geometry: geometryReal}
	scenarioRealClaim300  = &scenario{name: "real claim 300 km", kind: kindClaim, geometry: geometryReal, claimKm: 300}
	scenarioRealClaim1000 = &scenario{name: "real claim 1,000 km", kind: kindClaim, geometry: geometryReal, claimKm: 1000}
	scenarioRealClaim3000 = &scenario{name: "real claim 3,000 km", kind: kindClaim, geometry: geometryReal, claimKm: 3000}
)

// The honest scenarios, worst first, and the claims, nearest first.
var (
	honestScenarios = []*scenario{scenarioTiny, scenarioSmallEdge, scenarioLargeEdge, scenarioSmallCenter, scenarioLargeInland, scenarioRealHonest}
	claimScenarios  = []*scenario{scenarioClaim300, scenarioClaim1000, scenarioClaim3000, scenarioRealClaim300, scenarioRealClaim1000, scenarioRealClaim3000}
)

// The k_early values the study sweeps.
var earlyKs = []float64{3.0, 3.25, 3.5, 3.75, 4.0, 4.25, 4.5, 5.0, math.Inf(1)}

// A run with no early stop at all: only the cap or an interruption ends it.
func capPolicy() stopPolicy {
	p := v10Policy(math.Inf(1))
	p.name = "cap only"
	p.converged = false
	return p
}

// The k_early sweep and the cap-only baseline.
func sweepPolicies() []stopPolicy {
	policies := []stopPolicy{}
	for _, k := range earlyKs {
		policies = append(policies, v10Policy(k))
	}
	return append(policies, capPolicy())
}

// V10 with one of its other constants changed, at k_early.
func variantPolicies(earlyK float64) []stopPolicy {
	base := v10Policy(earlyK)
	policies := []stopPolicy{base}
	for _, roundTrips := range []int{8, 16} {
		p := base
		p.roundTrips = roundTrips
		p.name = "round trips " + itoa(roundTrips)
		policies = append(policies, p)
	}
	for _, windowMs := range []int64{1000, 3000} {
		p := base
		p.windowMs = windowMs
		p.name = "window " + itoa(int(windowMs)) + " ms"
		policies = append(policies, p)
	}
	for _, ratio := range []float64{1.05, 1.2} {
		p := base
		p.areaRatio = ratio
		p.name = "area ratio " + formatK(ratio)
		policies = append(policies, p)
	}
	for _, extenders := range []int{6, 10} {
		p := base
		p.extenders = extenders
		p.name = "extenders " + itoa(extenders)
		policies = append(policies, p)
	}
	return policies
}

func itoa(n int) string {
	return formatK(float64(n))
}

// One part of the study.
type studyPart struct {
	name   string
	about  string
	config studyConfig
	counts []scenarioCount
}

// The study's world and model unless a part varies them: the route stretch
// skewed, κ 0.25 in the world and the model, no misplaced extenders.
func baseConfig(seed uint64, policies []stopPolicy) studyConfig {
	return studyConfig{
		seed:         seed,
		truthKappa:   lagdrift.DefaultKappa,
		stretchShape: stretchSkewed,
		modelKappa:   lagdrift.DefaultKappa,
		policies:     policies,
		checkEvery:   7,
	}
}

func counted(scale float64, perRegion int, scenarios ...*scenario) []scenarioCount {
	counts := []scenarioCount{}
	for _, sc := range scenarios {
		counts = append(counts, scenarioCount{scenario: sc, perRegion: max(1, int(math.Round(scale*float64(perRegion))))})
	}
	return counts
}

// The study's parts, with each scenario's visitors per region times scale.
func studyParts(seed uint64, scale float64) []*studyPart {
	main := &studyPart{
		name:   "main",
		about:  "lagdrift as it is: κ 0.25 in the world and the model, the k_early sweep and the cap-only baseline",
		config: baseConfig(seed, sweepPolicies()),
	}
	main.counts = append(main.counts, counted(scale, 1200, scenarioTiny)...)
	main.counts = append(main.counts, counted(scale, 400, scenarioSmallEdge, scenarioLargeEdge, scenarioSmallCenter, scenarioLargeInland)...)
	main.counts = append(main.counts, counted(scale, 1000, scenarioRealHonest)...)
	main.counts = append(main.counts, counted(scale, 300, scenarioClaim300, scenarioClaim1000, scenarioClaim3000)...)
	main.counts = append(main.counts, counted(scale, 300, scenarioRealClaim300, scenarioRealClaim1000, scenarioRealClaim3000)...)

	fixed := &studyPart{
		name:   "free b",
		about:  "the main part's visitors with V5 changed: at a b of 0, the position's covariance with b left free",
		config: baseConfig(seed, sweepPolicies()),
		counts: main.counts,
	}
	fixed.config.freeLastMile = true

	kappa := []*studyPart{}
	for _, truthKappa := range []float64{0.15, 0.25, 0.35} {
		for _, modelKappa := range []float64{0.15, 0.25, 0.35} {
			part := &studyPart{
				name:   "κ " + formatK(truthKappa) + "/" + formatK(modelKappa),
				about:  "κ " + formatK(truthKappa) + " in the world, " + formatK(modelKappa) + " in the model",
				config: baseConfig(seed, []stopPolicy{v10Policy(lagdrift.EarlyK), v10Policy(math.Inf(1))}),
				counts: append(counted(scale, 300, scenarioTiny), append(counted(scale, 150, scenarioLargeEdge), counted(scale, 100, scenarioClaim300, scenarioClaim1000, scenarioClaim3000)...)...),
			}
			part.config.truthKappa = truthKappa
			part.config.modelKappa = modelKappa
			kappa = append(kappa, part)
		}
	}

	variants := &studyPart{
		name:   "stop rule",
		about:  "V10's constants one at a time: round trips, the steadiness window, the area ratio, the extenders",
		config: baseConfig(seed, variantPolicies(lagdrift.EarlyK)),
		counts: append(counted(scale, 400, scenarioTiny, scenarioLargeEdge), counted(scale, 200, scenarioClaim300, scenarioClaim1000)...),
	}

	misplaced := &studyPart{
		name:   "misplaced",
		about:  "3% of extenders misplaced by 300 to 1,500 km, beyond their accuracy radius",
		config: baseConfig(seed, []stopPolicy{v10Policy(lagdrift.EarlyK), v10Policy(math.Inf(1))}),
		counts: append(counted(scale, 400, scenarioTiny, scenarioLargeEdge), counted(scale, 200, scenarioClaim300, scenarioClaim1000)...),
	}
	misplaced.config.misplacedShare = 0.03

	normal := &studyPart{
		name:   "normal stretch",
		about:  "the route stretch normal, the model's own assumption, rather than skewed and floored",
		config: baseConfig(seed, []stopPolicy{v10Policy(lagdrift.EarlyK), v10Policy(math.Inf(1))}),
		counts: misplaced.counts,
	}
	normal.config.stretchShape = stretchNormal

	parts := []*studyPart{main, fixed}
	parts = append(parts, kappa...)
	return append(parts, variants, misplaced, normal)
}
