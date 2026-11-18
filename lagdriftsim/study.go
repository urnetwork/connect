package lagdriftsim

// A study: visitors of several scenarios, each simulated under every stop
// policy, summarized for the report's tables. Visitors run on parallel
// workers, but every draw is keyed (seed.go), so the summaries are the same in
// any order, and they are kept in the visitors' order.

import (
	"fmt"
	"runtime"
	"slices"
	"sync"

	"github.com/urnetwork/connect/lagdrift"
)

// How a study's world is drawn and what it varies.
type studyConfig struct {
	seed uint64
	// the route stretch's standard deviation in the simulated world, and its
	// shape (network.go)
	truthKappa   float64
	stretchShape string
	// κ as the model assumes it (V3)
	modelKappa float64
	// extenders somewhere else entirely, a share of the fleet (world.go)
	misplacedShare float64
	// κ as the model assumes it for one region's visitors, in place of
	// modelKappa (a κ calibrated per continent, V4)
	regionModelKappas map[string]float64
	// diagnoses: the route stretch only on paths whose length is in
	// [routeMinKm, routeMaxKm) (both 0: every path), every extender exactly at
	// its place, and pings with no queue, jitter, spike or loss
	routeMinKm   float64
	routeMaxKm   float64
	noPlaceError bool
	idealPings   bool
	// the model's place error for an extender at least this share of its
	// distance from the address's place (a distance-dependent s_ext), within
	// lagdrift's [10, 100] km hold
	placeErrorPerKm float64
	// the fleet's sites in place of extenderSites, and more sites besides
	sites      []extenderSite
	extraSites []extenderSite
	// how a run's extenders are chosen (choice.go)
	choiceMode string
	// the review's rule (V10): an interrupted run counts only if converged
	// or clear held, by the server's check, when the silence began
	interruptedRule bool
	// the server's verdict from the lowest χ² of lagdrift's fit and a grid of
	// starts around the fit and the address's place, when lower by more than
	// 1: what wider starts would change
	wideStarts bool
	// V10 (e2e): the frame decides its stops on the fit over usable extenders
	// only, those with the policy's round trips; else over every extender with
	// 4
	stopOnUsable bool
	// far sessions open late, as on the real stack: 3 s × min(1, d / 7,500 km)
	// more than their round trips take
	lateFarSessions bool
	// V11's calibration tracks: lag.dev's own choice, one run to the cap with
	// no frame fits, and the fit measured from the city point as well
	lagTracks bool
	// V11's k at 99% per continent, a visitor's from its address's continent
	// (else DefaultK), with KEarly of it for clear; nil leaves V11 out
	kByContinent map[string]float64
	// the runs each visitor makes: 1 to 3, 0 for 3
	runs int
	// V10's cadence: the frame refits warm at each ping round and runs the
	// full multi-start when its extenders change, every 2 s and before any
	// stop; without it, the full multi-start at every ping round
	cadence  bool
	policies []stopPolicy
	// every this many visitors, the study checks its stops and verdicts
	// against lagdrift.EvaluateStop and lagdrift.Decide
	checkEvery int
	workers    int
}

// How many visitors a scenario has, per region, in the regions given (none:
// every region).
type scenarioCount struct {
	scenario  *scenario
	perRegion int
	regions   []string
}

// The parts of a visitor's outcome the tables use.
type visitorSummary struct {
	index     int
	scenario  *scenario
	truth     lagdrift.Point
	continent string
	region    string
	cityName  string
	access    string
	borderKm  float64
	claimName string
	// per policy, per run
	runs [][visitorRuns]runSummary
}

type runSummary struct {
	stopMs  int64
	reason  lagdrift.StopReason
	usable  int
	counted bool
	// the verdict after the run: the counting runs it pools, and per
	// confidence
	counting  int
	verdicts  [3]lagdrift.Verdict
	reasons   [3]lagdrift.Reason
	kVerdicts [2]lagdrift.Verdict
	// at the visitor's V11 k
	v11Verdict lagdrift.Verdict
	v11K       float64
	// a lag track's fit measured from the city point, and whether light rules
	// the city point out from two or more extenders
	cityM          float64
	cityImpossible bool
	at             lagdrift.Point
	cov            [2][2]float64
	fitted         bool
	d              float64
	majorKm        float64
	minorKm        float64
	chi2           float64
	bAtZero        bool
	truthM         float64
	errorKm        float64
}

// The study's result: its summaries and its checks against lagdrift.
type studyResult struct {
	config           *studyConfig
	summaries        []*visitorSummary
	stopChecks       int
	stopMismatches   []string
	decideChecks     int
	decideMismatches []string
	fitChecks        fitChecks
	fitCosts         [fitKinds]fitCost
	withdrawnStops   int
	// a sample of the fits' inputs per kind, at most maxFitSamples each
	fitSamples [fitKinds][]fitSample
	// visitors a scenario couldn't place (no claim at its distance, a city
	// outside its country's borders)
	skipped map[string]int
}

// The visitors of the scenarios, in order: for each scenario, perRegion per
// region, cycling through the region's cities. Real scenarios need borders;
// without them they are skipped.
func buildVisitors(w *world, seed uint64, counts []scenarioCount, firstIndex int) ([]*visitorSpec, map[string]int) {
	regionCities := citiesByRegion()
	candidates := claimCandidates()
	var options map[string][]claimOption
	for _, count := range counts {
		if count.scenario.kind == kindClaim && count.scenario.geometry == geometryReal && w.borders != nil && options == nil {
			options = w.claimOptions()
		}
	}
	specs := []*visitorSpec{}
	skipped := map[string]int{}
	index := firstIndex
	for _, count := range counts {
		sc := count.scenario
		for _, region := range regions {
			if 0 < len(count.regions) && !slices.Contains(count.regions, region) {
				continue
			}
			if 0 < len(sc.pairs) {
				continue
			}
			cities := regionCities[region]
			for k := 0; k < count.perRegion; k++ {
				c := cities[k%len(cities)]
				var spec *visitorSpec
				switch {
				case sc.geometry == geometryReal && w.borders == nil:
				case sc.kind == kindHonest && sc.geometry == geometrySynthetic:
					spec = syntheticHonest(index, sc, c, seed)
				case sc.kind == kindClaim && sc.geometry == geometrySynthetic:
					spec = syntheticClaim(index, sc, c, candidates, seed)
				case sc.kind == kindHonest:
					spec = realHonest(index, sc, c, w.borders, seed)
				default:
					spec = realClaim(index, sc, c, options[c.name+"/"+c.code], w.borders, seed)
				}
				if spec == nil {
					skipped[sc.name+", "+region] += 1
				} else {
					specs = append(specs, spec)
				}
				index += 1
			}
		}
	}
	// the named pairs: perRegion visitors each, in their city's region
	for _, count := range counts {
		sc := count.scenario
		for _, pair := range sc.pairs {
			c, ok := cityNamed(pair.cityName)
			for k := 0; k < count.perRegion; k++ {
				var spec *visitorSpec
				if ok && w.borders != nil {
					spec = realPair(index, sc, c, pair.code, w.borders, seed)
				}
				if spec == nil {
					skipped[sc.name+", "+pair.cityName+"/"+pair.code] += 1
				} else {
					specs = append(specs, spec)
				}
				index += 1
			}
		}
	}
	return specs, skipped
}

// The visitor city of a name.
func cityNamed(name string) (visitorCity, bool) {
	for _, c := range visitorCities {
		if c.name == name {
			return c, true
		}
	}
	return visitorCity{}, false
}

// Simulates every visitor under every policy.
func runStudy(w *world, config *studyConfig, specs []*visitorSpec) *studyResult {
	workers := config.workers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	summaries := make([]*visitorSummary, len(specs))
	outcomes := make([]*visitorOutcome, len(specs))
	var wg sync.WaitGroup
	next := make(chan int)
	for k := 0; k < workers; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				outcome := simulateVisitor(w, config, specs[i])
				summaries[i] = summarize(outcome)
				outcome.outcomes = nil
				outcomes[i] = outcome
			}
		}()
	}
	for i := range specs {
		next <- i
	}
	close(next)
	wg.Wait()

	result := &studyResult{config: config, summaries: summaries}
	for _, outcome := range outcomes {
		result.stopChecks += outcome.stopChecks
		result.stopMismatches = append(result.stopMismatches, outcome.stopMismatches...)
		result.decideChecks += outcome.decideChecks
		result.decideMismatches = append(result.decideMismatches, outcome.decideMismatches...)
		result.fitChecks.add(outcome.fitChecks)
		for kind := range result.fitCosts {
			result.fitCosts[kind].merge(outcome.fitCosts[kind])
		}
		result.withdrawnStops += outcome.withdrawnStops
		for _, sample := range outcome.fitSamples {
			if len(result.fitSamples[sample.kind]) < maxFitSamples {
				result.fitSamples[sample.kind] = append(result.fitSamples[sample.kind], sample)
			}
		}
		outcome.fitSamples = nil
	}
	return result
}

func summarize(outcome *visitorOutcome) *visitorSummary {
	spec := outcome.spec
	s := &visitorSummary{
		index:     spec.index,
		scenario:  spec.scenario,
		truth:     spec.truth,
		continent: spec.continent,
		region:    spec.region,
		cityName:  spec.city.name,
		borderKm:  spec.borderKm,
		claimName: spec.claimName,
	}
	for _, runs := range outcome.outcomes {
		var row [visitorRuns]runSummary
		for i, o := range runs {
			v := o.verdict
			row[i] = runSummary{
				stopMs:         o.stopMs,
				reason:         o.reason,
				usable:         o.usable,
				counted:        o.counted,
				counting:       v.counting,
				verdicts:       v.verdicts,
				reasons:        v.reasons,
				kVerdicts:      v.kVerdicts,
				v11Verdict:     v.v11Verdict,
				v11K:           v.v11K,
				cityM:          v.cityM,
				cityImpossible: v.cityImpossible,
				at:             v.at,
				cov:            v.cov,
				fitted:         v.fitted,
				d:              v.d,
				majorKm:        v.majorKm,
				minorKm:        v.minorKm,
				chi2:           v.chi2,
				bAtZero:        v.bAtZero,
				truthM:         v.truthM,
				errorKm:        v.errorKm,
			}
		}
		s.runs = append(s.runs, row)
	}
	return s
}

// The fits kept per kind to time again.
const maxFitSamples = 1500

// The policy's index in the study's config, or −1.
func (self *studyResult) policyIndex(name string) int {
	for i, p := range self.config.policies {
		if p.name == name {
			return i
		}
	}
	return -1
}

func (self *studyResult) String() string {
	return fmt.Sprintf("%d visitors, %d stop checks (%d mismatches), %d verdict checks (%d mismatches)",
		len(self.summaries), self.stopChecks, len(self.stopMismatches), self.decideChecks, len(self.decideMismatches))
}

// The study's k for a confidence index.
func kAt(i int) float64 {
	return lagdrift.KForConfidence(confidences[i])
}
