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

	// a VPN: the visitor far from their address's place, at the distances
	// the drift test's customers ask about, and named pairs
	scenarioRealClaim800  = &scenario{name: "real claim 800 km", kind: kindClaim, geometry: geometryReal, claimKm: 800}
	scenarioRealClaim1500 = &scenario{name: "real claim 1,500 km", kind: kindClaim, geometry: geometryReal, claimKm: 1500}
	scenarioRealClaim6000 = &scenario{name: "real claim 6,000 km", kind: kindClaim, geometry: geometryReal, claimKm: 6000}
	scenarioVpnPairs      = &scenario{name: "VPN pairs", kind: kindClaim, geometry: geometryReal, pairs: vpnPairs}
)

// The VPN distances, nearest first.
var vpnScenarios = []*scenario{scenarioRealClaim300, scenarioRealClaim800, scenarioRealClaim1500, scenarioRealClaim3000, scenarioRealClaim6000}

// The honest scenarios, worst first, and the claims, nearest first.
var (
	honestScenarios = []*scenario{scenarioTiny, scenarioSmallEdge, scenarioLargeEdge, scenarioSmallCenter, scenarioLargeInland, scenarioRealHonest}
	claimScenarios  = []*scenario{scenarioClaim300, scenarioClaim1000, scenarioClaim3000, scenarioRealClaim300, scenarioRealClaim800, scenarioRealClaim1000, scenarioRealClaim1500, scenarioRealClaim3000, scenarioRealClaim6000, scenarioVpnPairs}
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

// V10 with one of its other constants changed, at k_early.
func variantPolicies(earlyK float64) []stopPolicy {
	base := v10Policy(earlyK)
	policies := []stopPolicy{base}
	for _, roundTrips := range otherRoundTrips() {
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
	// sets the part up from the parts that ran before it, when given
	prepare func(part *studyPart, results map[string]*studyResult)
}

// The study's world and model unless a part varies them: the contract as it
// stands (lag.dev/DRIFTTEST.md, 2026-11-17), with E2's mid band chosen by the
// ring (the hybrid), V10's cadence, its stops decided on the usable
// extenders only, at lagdrift's StopRoundTrips, and its interrupted rule,
// and V11's k for the address's continent with k_early KEarly(k); far
// sessions opening late, as on the real stack (e2e); the route stretch
// skewed, κ 0.25 in the world and the model, no misplaced extenders.
func baseConfig(seed uint64, policies []stopPolicy) studyConfig {
	return studyConfig{
		seed:            seed,
		truthKappa:      lagdrift.DefaultKappa,
		stretchShape:    stretchSkewed,
		modelKappa:      lagdrift.DefaultKappa,
		policies:        policies,
		checkEvery:      7,
		choiceMode:      choiceHybrid,
		cadence:         refitPosition != nil,
		stopOnUsable:    true,
		interruptedRule: true,
		kByContinent:    v11Ks(),
		lateFarSessions: true,
	}
}

// V11's k at 99% per continent, lagdrift's DefaultK, "" for unknown.
func v11Ks() map[string]float64 {
	ks := map[string]float64{}
	for _, continent := range []string{"AF", "AN", "AS", "EU", "NA", "OC", "SA", ""} {
		ks[continent] = lagdrift.DefaultK(lagdrift.DefaultConfidence, continent)
	}
	return ks
}

// A config judged at the textbook k, 2.448, 3.035 and 3.717 at 95%, 99% and
// 99.9%, with k_early EarlyK: the region V11 recalibrated, for the parts that
// diagnose it.
func textbook(config studyConfig) studyConfig {
	config.kByContinent = nil
	return config
}

// The usable round trips the stops' evidence compares with lagdrift's
// StopRoundTrips: 12 beside 8, else 8.
func besideRoundTrips() int {
	if lagdrift.StopRoundTrips == 8 {
		return 12
	}
	return 8
}

// The usable round trips V10's constants compare with lagdrift's
// StopRoundTrips: the others of 8, 12 and 16.
func otherRoundTrips() []int {
	others := []int{}
	for _, roundTrips := range []int{8, 12, 16} {
		if roundTrips != lagdrift.StopRoundTrips {
			others = append(others, roundTrips)
		}
	}
	return others
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
	// the contract as it stands, under T3's k_early sweep, each value scaled
	// with the run's k as KEarly scales EarlyK
	main := &studyPart{
		name:   "main",
		about:  "the contract as it stands (V10, V11, E2's hybrid choice, far sessions opening late), κ 0.25 in the world and the model, 3 runs, the k_early sweep",
		config: baseConfig(seed, sweepPolicies()),
	}
	main.counts = append(main.counts, counted(scale, 800, scenarioTiny)...)
	main.counts = append(main.counts, counted(scale, 200, scenarioSmallEdge, scenarioLargeEdge, scenarioSmallCenter, scenarioLargeInland)...)
	main.counts = append(main.counts, counted(scale, 500, scenarioRealHonest)...)
	main.counts = append(main.counts, counted(scale, 150, scenarioClaim300, scenarioClaim1000, scenarioClaim3000)...)
	main.counts = append(main.counts, counted(scale, 150, scenarioRealClaim300, scenarioRealClaim1000, scenarioRealClaim3000)...)

	// the same visitors with each session opening after its own round trips
	// alone, as the study had them before the e2e found far sessions late
	staggered := &studyPart{
		name:   "staggered sessions",
		about:  "the main part's visitors with each session opening after its own round trips alone, as before the e2e found far sessions 2–3 s late; V10, 3 runs",
		config: baseConfig(seed, []stopPolicy{v10Policy(lagdrift.EarlyK)}),
		counts: main.counts,
	}
	staggered.config.lateFarSessions = false

	// a run that never stops early, beside V10 and V10 without clear
	noEarly := &studyPart{
		name:   "no early stop",
		about:  "one run under V10, V10 without clear, and no early stop at all (the cap)",
		config: baseConfig(seed, []stopPolicy{v10Policy(lagdrift.EarlyK), v10Policy(math.Inf(1)), capPolicy()}),
		counts: append(counted(scale, 200, scenarioTiny), counted(scale, 100, scenarioLargeEdge, scenarioClaim300, scenarioClaim1000)...),
	}
	noEarly.config.runs = 1

	single := func(name string, about string, policies []stopPolicy, counts []scenarioCount) *studyPart {
		part := &studyPart{name: name, about: about, config: baseConfig(seed, policies), counts: counts}
		part.config.runs = 1
		return part
	}

	// κ matched in the world and the model, and the model's 0.25 against
	// another world's and the reverse
	kappa := []*studyPart{}
	for _, truthKappa := range []float64{0.15, 0.25, 0.35} {
		for _, modelKappa := range []float64{0.15, 0.25, 0.35} {
			if truthKappa != modelKappa && truthKappa != lagdrift.DefaultKappa && modelKappa != lagdrift.DefaultKappa {
				continue
			}
			part := single("κ "+formatK(truthKappa)+"/"+formatK(modelKappa), "κ "+formatK(truthKappa)+" in the world, "+formatK(modelKappa)+" in the model, one run",
				[]stopPolicy{v10Policy(lagdrift.EarlyK)},
				append(counted(scale, 150, scenarioTiny), counted(scale, 100, scenarioLargeEdge, scenarioClaim300, scenarioClaim1000, scenarioClaim3000)...))
			part.config.truthKappa = truthKappa
			part.config.modelKappa = modelKappa
			kappa = append(kappa, part)
		}
	}

	other := append(counted(scale, 250, scenarioTiny), counted(scale, 150, scenarioLargeEdge, scenarioClaim300, scenarioClaim1000)...)
	variants := single("stop rule", "V10's constants one at a time: round trips, the steadiness window, the area ratio, the extenders; one run",
		variantPolicies(lagdrift.EarlyK), other)
	misplaced := single("misplaced", "3% of extenders misplaced by 300 to 1,500 km, beyond their accuracy radius; one run",
		[]stopPolicy{v10Policy(lagdrift.EarlyK)}, other)
	misplaced.config.misplacedShare = 0.03
	normal := single("normal stretch", "the route stretch normal, the model's own assumption, rather than skewed and floored; one run",
		[]stopPolicy{v10Policy(lagdrift.EarlyK)}, other)
	normal.config.stretchShape = stretchNormal

	parts := []*studyPart{main, staggered, noEarly}
	if refitPosition != nil {
		// the same visitors with V10's cadence and with the full multi-start
		// at every ping round
		counts := append(counted(scale, 300, scenarioTiny), counted(scale, 100, scenarioLargeEdge, scenarioRealHonest, scenarioClaim300, scenarioClaim1000)...)
		refits := &studyPart{name: "cadence: refits", about: "V10's cadence: a warm refit at each ping round, the full multi-start when the extenders change, every 2 s and before any stop", config: baseConfig(seed, sweepPolicies()), counts: counts}
		refits.config.cadence = true
		full := &studyPart{name: "cadence: full fits", about: "the same visitors with the full multi-start at every ping round", config: baseConfig(seed, sweepPolicies()), counts: counts}
		full.config.cadence = false
		parts = append(parts, refits, full)
	}
	parts = append(parts, kappa...)
	parts = append(parts, variants, misplaced, normal)
	parts = append(parts, v11Parts(seed, scale)...)
	parts = append(parts, usableStopParts(seed, scale)...)
	parts = append(parts, calibrationParts(seed, scale)...)
	parts = append(parts, startsParts(seed, scale)...)
	parts = append(parts, vpnParts(seed, scale)...)
	parts = append(parts, interruptedParts(seed, scale)...)
	return append(parts, oceaniaParts(seed, scale)...)
}

// The VPN study: visitors far from their address's place, on a dense fleet
// (the study's) and a sparse one, under E2 and its variants (choice.go), with
// the k_early sweep, so each variant's power, false rejects and k_early come
// from the same visitors.
func vpnParts(seed uint64, scale float64) []*studyPart {
	counts := append(counted(scale, 40, scenarioVpnPairs), counted(scale, 40, vpnScenarios...)...)
	counts = append(counts, counted(scale, 120, scenarioTiny)...)
	counts = append(counts, counted(scale, 60, scenarioLargeEdge, scenarioRealHonest)...)
	parts := []*studyPart{}
	for _, pool := range []struct {
		name  string
		sites []extenderSite
	}{{name: "dense", sites: nil}, {name: "sparse", sites: sparseSites}} {
		for _, mode := range []string{choiceE2, choiceJoint, choiceHubs, choiceInformation, choiceInformationRing, choiceRingMid, choiceHybrid} {
			part := &studyPart{
				name:   "VPN " + pool.name + ": " + mode,
				about:  "the " + pool.name + " fleet, extenders chosen by " + mode + "; 3 runs",
				config: baseConfig(seed, sweepPolicies()),
				counts: counts,
			}
			part.config.sites = pool.sites
			part.config.choiceMode = mode
			parts = append(parts, part)
		}
	}
	return parts
}

// V11's k (review V-8): calibrated on single-run lag.dev tracks, used on drift
// verdicts with the hybrid choice pooled over up to 3 runs. The lag part
// calibrates k per continent by V11's upper-bound rule; the drift parts judge
// the same visitors at V11's defaults and at that calibrated k.
func v11Parts(seed uint64, scale float64) []*studyPart {
	lag := &studyPart{
		name:   "V11: lag.dev tracks",
		about:  "lag.dev's own choice of 8 extenders, one run to the cap, the fit measured from the visitor's city point: V11's calibration tracks",
		config: baseConfig(seed, []stopPolicy{capPolicy()}),
		counts: counted(scale, 10000, scenarioTiny),
	}
	lag.config.runs = 1
	lag.config.lagTracks = true
	lag.config.choiceMode = choiceLag
	honest := append(counted(scale, 800, scenarioTiny), counted(scale, 500, scenarioRealHonest)...)
	honest = append(honest, counted(scale, 200, scenarioLargeEdge)...)
	drift := func(name string, about string) *studyPart {
		return &studyPart{name: name, about: about, config: baseConfig(seed, []stopPolicy{v10Policy(lagdrift.EarlyK)}), counts: honest}
	}
	defaults := drift("V11 drift: default k", "the hybrid choice, 3 runs, judged at V11's default k for the address's continent, k_early KEarly(k)")
	calibrated := drift("V11 drift: lag k", "the same visitors judged at the k calibrated from the lag.dev tracks by V11's upper-bound rule")
	calibrated.prepare = func(part *studyPart, results map[string]*studyResult) {
		if lagResult, ok := results[lag.name]; ok {
			for continent, k := range v11CalibratedKs(lagResult) {
				part.config.kByContinent[continent] = k
			}
		}
	}
	// V11's table as the user fixed it, on drift geometry, with enough
	// tiny-country visitors per continent to see a 1% rate, and claims for
	// power
	table := &studyPart{
		name:   "V11 table",
		about:  "the hybrid choice, 3 runs, judged at V11's fixed 99% table for the address's continent, k_early KEarly(k)",
		config: baseConfig(seed, []stopPolicy{v10Policy(lagdrift.EarlyK)}),
		counts: append(counted(scale, 2000, scenarioTiny), append(counted(scale, 500, scenarioRealHonest), counted(scale, 300, scenarioClaim300, scenarioClaim1000, scenarioClaim3000)...)...),
	}
	return []*studyPart{lag, defaults, calibrated, table}
}

// V10's stops on the verdict's evidence (e2e): the frame deciding its stops
// on every extender with 4 round trips, as before, or on the usable ones
// only, as V10 says, at lagdrift's StopRoundTrips and at 8 or 12 beside it.
// The contract otherwise, far sessions late, 3 runs.
func usableStopParts(seed uint64, scale float64) []*studyPart {
	counts := append(counted(scale, 800, scenarioTiny), counted(scale, 500, scenarioRealHonest)...)
	counts = append(counts, counted(scale, 150, scenarioClaim300, scenarioClaim1000, scenarioClaim3000)...)
	counts = append(counts, counted(scale, 40, scenarioVpnPairs)...)
	other := v10Policy(lagdrift.EarlyK)
	other.roundTrips = besideRoundTrips()
	other.name = "round trips " + itoa(other.roundTrips)
	parts := []*studyPart{}
	for _, usable := range []bool{false, true} {
		name := "late sessions: stops on 4"
		about := "far sessions " + lateSessionText() + " late; the frame's stops on every extender with 4 round trips"
		if usable {
			name = "late sessions: stops on usable"
			about = "far sessions " + lateSessionText() + " late; the frame's stops on the usable extenders only (V10)"
		}
		part := &studyPart{name: name, about: about, config: baseConfig(seed, []stopPolicy{v10Policy(lagdrift.EarlyK), other}), counts: counts}
		part.config.stopOnUsable = usable
		parts = append(parts, part)
	}
	return parts
}

// Where the region's excess misses come from: honest visitors, one run each,
// in worlds that each take away a source of error the model's variance
// doesn't hold, and in the model's own world without all three.
func calibrationParts(seed uint64, scale float64) []*studyPart {
	counts := append(counted(scale, 300, scenarioTiny), counted(scale, 100, scenarioLargeEdge)...)
	part := func(name string, about string, tweak func(*studyConfig)) *studyPart {
		config := textbook(baseConfig(seed, []stopPolicy{v10Policy(lagdrift.EarlyK)}))
		config.runs = 1
		tweak(&config)
		return &studyPart{name: "calibration: " + name, about: about, config: config, counts: counts}
	}
	return []*studyPart{
		part("base", "the study's world", func(c *studyConfig) {}),
		part("ideal pings", "no queue, jitter, spike or loss", func(c *studyConfig) { c.idealPings = true }),
		part("no place error", "every extender exactly at its place", func(c *studyConfig) { c.noPlaceError = true }),
		part("normal stretch", "the route stretch normal and unfloored", func(c *studyConfig) { c.stretchShape = stretchNormal }),
		part("model's world", "all three: the route stretch normal, no place error, ideal pings", func(c *studyConfig) {
			c.idealPings = true
			c.noPlaceError = true
			c.stretchShape = stretchNormal
		}),
	}
}

// lagdrift's starts against wider ones for the server's verdict: the same
// visitors, one run each, honest and far from the address's place.
func startsParts(seed uint64, scale float64) []*studyPart {
	counts := append(counted(scale, 150, scenarioTiny), counted(scale, 60, scenarioLargeEdge, scenarioRealHonest)...)
	counts = append(counts, counted(scale, 40, scenarioVpnPairs)...)
	counts = append(counts, counted(scale, 40, vpnScenarios...)...)
	parts := []*studyPart{}
	for _, wide := range []bool{false, true} {
		name := "starts: lagdrift"
		about := "the server's verdict from lagdrift's fit, one run"
		if wide {
			name = "starts: wide"
			about = "the server's verdict from the lowest χ² of lagdrift's fit and a grid of starts around it and the address's place, one run"
		}
		part := &studyPart{name: name, about: about, config: textbook(baseConfig(seed, []stopPolicy{v10Policy(lagdrift.EarlyK)})), counts: counts}
		part.config.runs = 1
		part.config.wideStarts = wide
		parts = append(parts, part)
	}
	return parts
}

// The review's interrupted-stop rule (V10): honest runs and a client that
// goes silent at a favourable look, with and without it, one run each.
func interruptedParts(seed uint64, scale float64) []*studyPart {
	counts := append(counted(scale, 200, scenarioTiny), counted(scale, 100, scenarioLargeEdge, scenarioClaim300, scenarioClaim1000, scenarioClaim3000)...)
	parts := []*studyPart{}
	for _, rule := range []bool{false, true} {
		name := "interrupted: counted"
		about := "an interrupted run counts with 6 usable extenders, as before the review; one run"
		if rule {
			name = "interrupted: review's rule"
			about = "an interrupted run counts only if converged or clear held when the silence began; one run"
		}
		part := &studyPart{name: name, about: about, config: baseConfig(seed, []stopPolicy{v10Policy(lagdrift.EarlyK), attackPolicy()}), counts: counts}
		part.config.runs = 1
		part.config.interruptedRule = rule
		parts = append(parts, part)
	}
	return parts
}

// The k_early sweep.
func sweepPolicies() []stopPolicy {
	policies := []stopPolicy{}
	for _, k := range earlyKs {
		policies = append(policies, v10Policy(k))
	}
	return policies
}

// The Oceania diagnosis: Oceania's visitors, one run each, in worlds that
// each take one source of error away (what causes its regions to miss), and
// under models that each try one remedy (what a remedy costs in power).
func oceaniaParts(seed uint64, scale float64) []*studyPart {
	only := []string{regionOceania}
	counts := []scenarioCount{
		{scenario: scenarioTiny, perRegion: max(1, int(math.Round(scale*1200))), regions: only},
		{scenario: scenarioLargeEdge, perRegion: max(1, int(math.Round(scale*300))), regions: only},
		{scenario: scenarioRealHonest, perRegion: max(1, int(math.Round(scale*400))), regions: only},
		{scenario: scenarioClaim300, perRegion: max(1, int(math.Round(scale*200))), regions: only},
		{scenario: scenarioClaim1000, perRegion: max(1, int(math.Round(scale*200))), regions: only},
	}
	part := func(name string, about string, tweak func(*studyConfig)) *studyPart {
		config := textbook(baseConfig(seed, []stopPolicy{v10Policy(lagdrift.EarlyK)}))
		config.runs = 1
		tweak(&config)
		return &studyPart{name: "Oceania: " + name, about: about, config: config, counts: counts}
	}
	return []*studyPart{
		part("base", "the study's world", func(c *studyConfig) {}),
		part("normal stretch", "the route stretch normal and unfloored: no long tail of detours", func(c *studyConfig) { c.stretchShape = stretchNormal }),
		part("no long-path error", "no route error on paths of 2,500 km or more", func(c *studyConfig) { c.routeMaxKm = 2500 }),
		part("no short-path error", "no route error on paths under 2,500 km", func(c *studyConfig) {
			c.routeMinKm = 2500
			c.routeMaxKm = math.Inf(1)
		}),
		part("no place error", "every extender exactly at its place", func(c *studyConfig) { c.noPlaceError = true }),
		part("ideal pings", "no queue, jitter, spike or loss", func(c *studyConfig) { c.idealPings = true }),
		part("denser fleet", "9 more extenders: Adelaide, Canberra, Hobart, Darwin, Wellington, Christchurch, Port Moresby, Suva, Nouméa", func(c *studyConfig) {
			c.extraSites = oceaniaExtraSites
		}),
		part("model κ 0.30", "the model's κ 0.30 for Oceania (a κ calibrated per continent, V4)", func(c *studyConfig) {
			c.regionModelKappas = map[string]float64{regionOceania: 0.30}
		}),
		part("model κ 0.35", "the model's κ 0.35 for Oceania", func(c *studyConfig) {
			c.regionModelKappas = map[string]float64{regionOceania: 0.35}
		}),
		part("model κ 0.45", "the model's κ 0.45 for Oceania", func(c *studyConfig) {
			c.regionModelKappas = map[string]float64{regionOceania: 0.45}
		}),
		part("s_ext by distance", "each extender's place error at least 5% of its distance from the address's place, within lagdrift's [10, 100] km hold", func(c *studyConfig) {
			c.placeErrorPerKm = 0.05
		}),
	}
}
