package lagdriftsim

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect/lagdrift"
)

// destination goes the distance along the bearing.
func TestDestination(t *testing.T) {
	from := lagdrift.Point{Lat: 48.8566, Lon: 2.3522}
	for _, bearing := range []float64{0, 45, 135, 270} {
		for _, km := range []float64{10, 500, 5000} {
			to := destination(from, bearing, km)
			if d := lagdrift.DistanceKm(from, to); math.Abs(d-km) > 1e-6*km {
				t.Errorf("bearing %g, %g km: went %g km", bearing, km, d)
			}
			if b := lagdrift.BearingDeg(from, to); math.Abs(math.Remainder(b-bearing, 360)) > 1e-6 {
				t.Errorf("bearing %g, %g km: left at %g", bearing, km, b)
			}
		}
	}
}

// A disk across the antimeridian is split there, and it still holds what is
// within its radius and its border is its radius from its center.
func TestDiskCountryAcrossTheAntimeridian(t *testing.T) {
	center := lagdrift.Point{Lat: -36.8, Lon: 179.5}
	c := diskCountry("zz", center, 300, 72)
	if len(c.Polygons) != 2 {
		t.Fatalf("%d polygons, expected the disk split in 2", len(c.Polygons))
	}
	for _, polygon := range c.Polygons {
		for _, p := range polygon[0] {
			if p.Lon < -180 || 180 < p.Lon {
				t.Fatalf("a point at longitude %g", p.Lon)
			}
		}
	}
	for _, bearing := range []float64{0, 90, 180, 270} {
		if !c.Contains(destination(center, bearing, 250)) {
			t.Errorf("250 km at %g° is outside", bearing)
		}
		if c.Contains(destination(center, bearing, 350)) {
			t.Errorf("350 km at %g° is inside", bearing)
		}
	}
	// from outside, the cut at the antimeridian is never the nearest point
	// (lagdrift's Judge); from inside it is boundary like any other edge
	for _, bearing := range []float64{60, 300} {
		inside, km := borderDistanceKm(c, destination(center, bearing, 350))
		if inside || math.Abs(km-50) > 2 {
			t.Errorf("350 km at %g°: inside %v, %g km from the border, expected 50 outside", bearing, inside, km)
		}
	}
	whole := diskCountry("zz", lagdrift.Point{Lat: -36.8, Lon: 170}, 300, 72)
	if inside, km := borderDistanceKm(whole, lagdrift.Point{Lat: -36.8, Lon: 170}); !inside || math.Abs(km-300) > 2 {
		t.Errorf("a disk's center: inside %v, %g km from the border, expected 300", inside, km)
	}
}

// localCountry keeps the polygons near the places, and the one holding them.
func TestLocalCountry(t *testing.T) {
	near := diskCountry("zz", lagdrift.Point{Lat: 10, Lon: 10}, 100, 24)
	far := diskCountry("zz", lagdrift.Point{Lat: -40, Lon: -120}, 100, 24)
	huge := diskCountry("zz", lagdrift.Point{Lat: 0, Lon: 60}, 2000, 24)
	c := &lagdrift.Country{Code: "zz", Polygons: append(append(near.Polygons, far.Polygons...), huge.Polygons...)}
	local := localCountry(c, 3000, lagdrift.Point{Lat: 10, Lon: 11}, lagdrift.Point{Lat: 0, Lon: 60})
	if len(local.Polygons) != 2 {
		t.Fatalf("%d polygons kept, expected the near one and the one holding a place", len(local.Polygons))
	}
}

// E2's bands: 4 near preferring 2 in the country, 4 mid and 4 far spread by
// bearing, spares apart, and a retest preferring extenders not used.
func TestChooseExtenders(t *testing.T) {
	ip := lagdrift.Point{Lat: 50, Lon: 10}
	candidates := []choiceCandidate{}
	add := func(bearing float64, km float64, inCountry bool) {
		p := destination(ip, bearing, km)
		candidates = append(candidates, choiceCandidate{
			index:      len(candidates),
			distanceKm: lagdrift.DistanceKm(ip, p),
			bearingDeg: lagdrift.BearingDeg(ip, p),
			inCountry:  inCountry,
		})
	}
	for i := 0; i < 6; i++ {
		add(float64(60*i), 20+float64(10*i), false)
	}
	add(200, 150, true)
	add(20, 180, true)
	for i := 0; i < 16; i++ {
		add(float64(22.5*float64(i)), 800+float64(50*i), false)
		add(float64(22.5*float64(i)+11), 4000+float64(100*i), false)
	}
	c := chooseExtenders(candidates, newRand(1, 99))
	if len(c.primaries) != runExtenders || len(c.spares) != maxSpares {
		t.Fatalf("%d primaries and %d spares", len(c.primaries), len(c.spares))
	}
	nearInCountryCount := 0
	mids := []choiceCandidate{}
	fars := []choiceCandidate{}
	for k, index := range c.primaries {
		cand := candidates[index]
		switch {
		case k < nearCount:
			if cand.inCountry {
				nearInCountryCount += 1
			}
		case cand.distanceKm < midMaxKm:
			mids = append(mids, cand)
		default:
			fars = append(fars, cand)
		}
	}
	if nearInCountryCount != 2 {
		t.Errorf("%d near extenders in the country, expected 2", nearInCountryCount)
	}
	if len(mids) != 4 || len(fars) != 4 {
		t.Fatalf("%d mid and %d far", len(mids), len(fars))
	}
	for _, band := range [][]choiceCandidate{mids, fars} {
		if gap := smallestGap(band); gap < 67.5 {
			t.Errorf("a band's smallest gap is %g°, expected the 16 bearings spread to at least 67.5°", gap)
		}
	}

	// a retest takes none of the first run's mid and far extenders
	used := map[int]bool{}
	for _, index := range c.primaries {
		used[index] = true
	}
	for i := range candidates {
		candidates[i].used = used[candidates[i].index]
	}
	retest := chooseExtenders(candidates, newRand(1, 100))
	for _, index := range retest.primaries[nearCount:] {
		if used[index] {
			t.Errorf("the retest repeats extender %d although others are free", index)
		}
	}
}

// A session pings one at a time, at most 4 a second, and a lost ping holds
// the next for 5 s.
func TestSessionPings(t *testing.T) {
	access := accessLink{kind: accessWifi, lastMileMs: 10, jitterMs: 1, queueMs: 1, lostShare: 0.05}
	link := drawRunLink(&access, newRand(3, 1))
	link.dropMs = math.Inf(1)
	pings := sessionPings(link, 20, 300, newRand(3, 2))
	if len(pings) < 10 {
		t.Fatalf("%d pings", len(pings))
	}
	lost := 0
	for i := 1; i < len(pings); i++ {
		gap := pings[i].sentMs - pings[i-1].sentMs
		if pings[i-1].ms == 0 {
			lost += 1
			if gap != pongTimeoutMs {
				t.Errorf("a lost ping held the next %g ms", gap)
			}
			continue
		}
		if gap < pingIntervalMs-1e-9 || pings[i].sentMs < pings[i-1].measuredMs {
			t.Errorf("ping %d went out %g ms after the last, before its pong", i, gap)
		}
	}
	if lost == 0 {
		t.Errorf("no ping lost at 5%%")
	}
}

// The skewed stretch has mean 0 and standard deviation κ, and no path is
// faster than fiber along the great circle.
func TestStretchMoments(t *testing.T) {
	r := newRand(5, 1)
	for _, kappa := range []float64{0.15, 0.25, 0.35} {
		n := 200000
		sum, sum2, low := 0.0, 0.0, 0.0
		for i := 0; i < n; i++ {
			s := drawStretch(r, kappa, stretchSkewed)
			sum += s
			sum2 += s * s
			low = math.Min(low, s)
		}
		mean := sum / float64(n)
		sd := math.Sqrt(sum2/float64(n) - mean*mean)
		if math.Abs(mean) > 0.005 || math.Abs(sd-kappa) > 0.01*kappa+0.002 || low < -0.3 {
			t.Errorf("κ %g: mean %g, sd %g, lowest %g", kappa, mean, sd, low)
		}
	}
}

// A small study decides every stop and verdict as lagdrift does, and comes
// out the same whatever the number of workers.
func TestStudyMatchesLagdriftAndReproduces(t *testing.T) {
	w := newWorld(worldConfig{seed: 7}, nil)
	counts := []scenarioCount{{scenario: scenarioTiny, perRegion: 1}, {scenario: scenarioClaim1000, perRegion: 1}}
	specs, _ := buildVisitors(w, 7, counts, 0)
	config := baseConfig(7, []stopPolicy{v10Policy(lagdrift.EarlyK), v10Policy(3)})
	// the full fit at every ping round, so every stop can be checked against
	// EvaluateStop on the same snapshots
	config.cadence = false
	config.runs = 2
	config.checkEvery = 1
	config.workers = 1
	one := runStudy(w, &config, specs)
	if one.stopChecks == 0 || one.decideChecks == 0 {
		t.Fatalf("no checks: %s", one)
	}
	for _, m := range append(one.stopMismatches, one.decideMismatches...) {
		t.Error(m)
	}
	config.workers = 4
	four := runStudy(w, &config, specs)
	for i := range one.summaries {
		if !reflect.DeepEqual(one.summaries[i].runs, four.summaries[i].runs) {
			t.Fatalf("visitor %d differs with 4 workers", i)
		}
	}
}

// drift-sim's report: a tiny study's REPORT.md has its summary and
// recommendation, its method, its findings and its tables, and WriteReport
// writes it as it is.
func TestReport(t *testing.T) {
	part := &studyPart{
		name:   "main",
		about:  "a tiny k_early sweep",
		config: baseConfig(3, []stopPolicy{v10Policy(lagdrift.EarlyK), v10Policy(math.Inf(1))}),
		counts: []scenarioCount{{scenario: scenarioTiny, perRegion: 1}, {scenario: scenarioClaim1000, perRegion: 1}},
	}
	part.config.runs = 1
	report, err := runParts(Options{Seed: 3, Scale: 1, CommandLine: "go run ./lagdriftsim/cmd/drift-sim -seed=3"}, []*studyPart{part})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# The drift test's error rates",
		"## Summary",
		"- **k_early: ",
		"- **False rejects** at ",
		"- **Power** at 99%",
		"## How the study works",
		"go run ./lagdriftsim/cmd/drift-sim -seed=3",
		"## Findings",
		"**The k_early sweep.**",
		"## Tables",
		"### False rejects: lagdrift",
		"### k_early: lagdrift",
		"### What a fit costs",
		"### Checks against lagdrift",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the report lacks %q", want)
		}
	}
	path := filepath.Join(t.TempDir(), "REPORT.md")
	if err := WriteReport(path, report); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(path)
	if err != nil || string(written) != report {
		t.Fatalf("WriteReport wrote %d bytes of %d: %v", len(written), len(report), err)
	}
	if _, err := runParts(Options{Seed: 3, Parts: []string{"none"}}, []*studyPart{part}); err == nil {
		t.Errorf("a study with no part to run made a report")
	}
}

// V10's cadence: the frame refits warm between full fits, the study times
// both, its stops are V10's rules on that history, and it reproduces whatever
// the number of workers.
func TestCadence(t *testing.T) {
	w := newWorld(worldConfig{seed: 11}, nil)
	counts := []scenarioCount{{scenario: scenarioTiny, perRegion: 1}, {scenario: scenarioClaim1000, perRegion: 1}}
	specs, _ := buildVisitors(w, 11, counts, 0)
	config := baseConfig(11, []stopPolicy{v10Policy(lagdrift.EarlyK), v10Policy(3)})
	config.cadence = true
	config.runs = 2
	config.workers = 1
	one := runStudy(w, &config, specs)
	if one.fitCosts[fitRefitSingle].fits == 0 || one.fitCosts[fitFrameSingle].fits == 0 {
		t.Fatalf("refits %d and full fits %d of single runs", one.fitCosts[fitRefitSingle].fits, one.fitCosts[fitFrameSingle].fits)
	}
	if one.fitCosts[fitRefitSingle].fits <= one.fitCosts[fitFrameSingle].fits {
		t.Errorf("%d refits for %d full fits: the full fit should run only on changes, every 2 s and before a stop",
			one.fitCosts[fitRefitSingle].fits, one.fitCosts[fitFrameSingle].fits)
	}
	for _, s := range one.summaries {
		for _, row := range s.runs {
			for run := 0; run < config.runs; run++ {
				if r := row[run]; r.stopMs < lagdrift.StopEarliestMs || lagdrift.StopCapMs < r.stopMs || r.reason == "" {
					t.Errorf("visitor %d run %d stopped at %d ms as %q", s.index, run+1, r.stopMs, r.reason)
				}
			}
		}
	}
	config.workers = 4
	four := runStudy(w, &config, specs)
	for i := range one.summaries {
		if !reflect.DeepEqual(one.summaries[i].runs, four.summaries[i].runs) {
			t.Fatalf("visitor %d differs with 4 workers", i)
		}
	}
}

// The VPN variants of E2: the joint spread keeps the mid and far bands off
// the near band's directions, the hubs take an extender at each hub beyond
// 300 km, and the information choice keeps E2's near band and fills 12.
func TestChoiceVariants(t *testing.T) {
	ip := lagdrift.Point{Lat: 40.4, Lon: -3.7}
	candidates := []choiceCandidate{}
	add := func(bearing float64, km float64) int {
		p := destination(ip, bearing, km)
		candidates = append(candidates, choiceCandidate{index: len(candidates), distanceKm: lagdrift.DistanceKm(ip, p), bearingDeg: lagdrift.BearingDeg(ip, p)})
		return len(candidates) - 1
	}
	// the near extenders all to the north east
	for i := 0; i < 4; i++ {
		add(40+float64(i), 150+float64(10*i))
	}
	for i := 0; i < 24; i++ {
		add(float64(15*i), 1000)
		add(float64(15*i)+7, 5000)
	}
	joint := chooseExtendersBy(candidates, choiceOptions{mode: choiceJoint}, newRand(1, 1))
	for _, index := range joint.primaries[nearCount:] {
		if gap := math.Abs(math.Remainder(candidates[index].bearingDeg-41.5, 360)); gap < 20 {
			t.Errorf("the joint spread took an extender %g° from the near band's direction", gap)
		}
	}
	hubs := chooseExtendersBy(candidates, choiceOptions{mode: choiceHubs, hubIndexes: []int{0, 4, 5, 6}}, newRand(1, 2))
	for _, hub := range []int{4, 5, 6} {
		if !slices.Contains(hubs.primaries, hub) {
			t.Errorf("the hub extender %d wasn't taken", hub)
		}
	}
	w := newWorld(worldConfig{seed: 1}, nil)
	spec := syntheticHonest(0, scenarioTiny, visitorCities[0], 1)
	sim := &visitorSim{world: w, config: &studyConfig{seed: 1, modelKappa: 0.25}, spec: spec, fleet: w.fleets[0], model: lagdrift.DefaultModel()}
	pool := []choiceCandidate{}
	for i := range w.pool {
		at := w.fleets[0][i].at
		pool = append(pool, choiceCandidate{index: i, distanceKm: lagdrift.DistanceKm(spec.ipPlace, at), bearingDeg: lagdrift.BearingDeg(spec.ipPlace, at)})
	}
	e2 := chooseExtendersBy(pool, choiceOptions{mode: choiceE2}, newRand(1, 3))
	information := chooseExtendersBy(pool, choiceOptions{mode: choiceInformation, score: sim.informationScore()}, newRand(1, 3))
	if len(information.primaries) != runExtenders || !reflect.DeepEqual(e2.primaries[:nearCount], information.primaries[:nearCount]) {
		t.Errorf("the information choice: %v, E2's near band %v", information.primaries, e2.primaries[:nearCount])
	}
	if score := sim.informationScore(); !(score(choiceOf(pool, information.primaries)) < score(choiceOf(pool, e2.primaries))) {
		t.Errorf("the information choice scores no better than E2's")
	}
}

func choiceOf(pool []choiceCandidate, indexes []int) []choiceCandidate {
	out := []choiceCandidate{}
	for _, index := range indexes {
		out = append(out, pool[index])
	}
	return out
}

// The review's interrupted rule: a client that goes silent at a favourable
// look before any stop rule held counts without the rule and not with it.
func TestInterruptedRule(t *testing.T) {
	w := newWorld(worldConfig{seed: 5}, nil)
	specs, _ := buildVisitors(w, 5, []scenarioCount{{scenario: scenarioClaim300, perRegion: 3}}, 0)
	counted := map[bool]int{}
	attacked := 0
	for _, rule := range []bool{false, true} {
		config := baseConfig(5, []stopPolicy{attackPolicy()})
		config.runs = 1
		config.interruptedRule = rule
		result := runStudy(w, &config, specs)
		for _, s := range result.summaries {
			r := s.runs[0][0]
			if r.reason != lagdrift.StopInterrupted {
				continue
			}
			if !rule {
				attacked += 1
			}
			if r.counted {
				counted[rule] += 1
			}
		}
	}
	if attacked == 0 {
		t.Fatalf("no client went silent")
	}
	if !(counted[true] < counted[false]) {
		t.Errorf("silent runs counted: %d without the rule, %d with it", counted[false], counted[true])
	}
}

// A named pair's visitor is in its city, with the address at the claimed
// country's claim place, and outside that country.
func TestRealPair(t *testing.T) {
	frankfurt := lagdrift.Point{Lat: 50.1109, Lon: 8.6821}
	borders, err := lagdrift.NewBorders("test", []lagdrift.Country{*diskCountry("de", frankfurt, 300, 72)})
	if err != nil {
		t.Fatal(err)
	}
	madrid, _ := cityNamed("Madrid")
	spec := realPair(0, scenarioVpnPairs, madrid, "de", borders, 1)
	if spec == nil {
		t.Fatalf("no visitor")
	}
	if spec.ipPlace != frankfurt || spec.claimName != "Madrid/DE" || spec.ipCountry.Contains(spec.truth) {
		t.Errorf("the visitor: %+v", spec)
	}
	if d := lagdrift.DistanceKm(spec.truth, frankfurt) - 300; math.Abs(spec.borderKm-d) > 5 {
		t.Errorf("%g km from the border, expected about %g", spec.borderKm, d)
	}
}

// The hybrid takes its mid band from the mid band's own candidates and hands
// a short mid band's slots to the far band, as E2 does.
func TestHybridChoice(t *testing.T) {
	ip := lagdrift.Point{Lat: 35.7, Lon: 139.7}
	candidates := []choiceCandidate{}
	add := func(bearing float64, km float64) {
		p := destination(ip, bearing, km)
		candidates = append(candidates, choiceCandidate{index: len(candidates), distanceKm: lagdrift.DistanceKm(ip, p), bearingDeg: lagdrift.BearingDeg(ip, p)})
	}
	for i := 0; i < 4; i++ {
		add(float64(90*i), 20)
	}
	// only 2 mid candidates, many far ones
	add(280, 1100)
	add(240, 2100)
	for i := 0; i < 12; i++ {
		add(float64(30*i), 6000)
	}
	score := func(picked []choiceCandidate) float64 { return -float64(len(picked)) }
	c := chooseExtendersBy(candidates, choiceOptions{mode: choiceHybrid, score: score}, newRand(1, 1))
	mid, far := 0, 0
	for _, index := range c.primaries {
		switch d := candidates[index].distanceKm; {
		case midMinKm <= d && d < midMaxKm:
			mid += 1
		case midMaxKm <= d:
			far += 1
		}
	}
	if len(c.primaries) != runExtenders || mid != 2 || far != 6 {
		t.Errorf("%d extenders, %d mid and %d far; expected 12 with 2 mid and 6 far", len(c.primaries), mid, far)
	}
}

// V11: a run judges at its confidence's k for the address's continent and
// stops clear at KEarly of it, so outside Europe a 99% run's k_early is above
// a 95% run's 4.0, and the 95% verdict comes from the 95% run's own stop. At
// the textbook k, every confidence shares the policy's k_early.
func TestV11Confidences(t *testing.T) {
	w := newWorld(worldConfig{seed: 13}, nil)
	specs, _ := buildVisitors(w, 13, []scenarioCount{{scenario: scenarioClaim1000, perRegion: 6, regions: []string{regionNorthAmerica, regionOceania}}}, 0)
	config := baseConfig(13, []stopPolicy{v10Policy(lagdrift.EarlyK)})
	config.runs = 1
	policy := config.policies[0]
	earlier := 0
	for _, spec := range specs {
		sim := newVisitorSim(w, &config, spec)
		for i, confidence := range []float64{0.95, 0.99} {
			if k := lagdrift.DefaultK(confidence, spec.continent); sim.ks[i] != k {
				t.Fatalf("%q at %g: k %g, V11's %g", spec.continent, confidence, sim.ks[i], k)
			}
		}
		at95, at99 := sim.atConfidence(policy, 0), sim.atConfidence(policy, 1)
		if at95.earlyK != lagdrift.EarlyK || at99.earlyK != lagdrift.KEarly(sim.ks[1]) {
			t.Fatalf("%q: k_early %g at 95%% and %g at 99%%", spec.continent, at95.earlyK, at99.earlyK)
		}
		outcomes := sim.runPolicy(policy)
		runs95 := sim.runsAt(at95, false)
		if outcomes[0].verdicts[0] != runs95[0].verdict.verdicts[0] || outcomes[0].verdicts[1] != outcomes[0].verdict.verdicts[1] {
			t.Errorf("visitor %d: verdicts %v, the 95%% run's %v", spec.index, outcomes[0].verdicts, runs95[0].verdict.verdicts)
		}
		if runs95[0].stopMs < outcomes[0].stopMs {
			earlier += 1
		}
	}
	if earlier == 0 {
		t.Errorf("no 95%% run stopped before its 99%% run")
	}
	plain := textbook(config)
	sim := newVisitorSim(w, &plain, specs[0])
	for i, confidence := range confidences {
		if sim.ks[i] != lagdrift.KForConfidence(confidence) {
			t.Errorf("textbook k at %g: %g", confidence, sim.ks[i])
		}
	}
	if sim.atConfidence(policy, 1).earlyK != lagdrift.EarlyK {
		t.Errorf("textbook k_early %g", sim.atConfidence(policy, 1).earlyK)
	}
}

// V11's upper bound: the value at rank n·q + 1.645·√(n·q·(1 − q)), rounded up
// to its 0.05 bin's upper edge, a value on an edge in the bin above it, and
// nothing from the overflow bin or past the sample.
func TestV11UpperBound(t *testing.T) {
	ms := []float64{}
	for i := 1; i <= 1000; i++ {
		ms = append(ms, 0.01*float64(i))
	}
	// rank ⌈990 + 1.645 √9.9⌉ = 996: 9.96, in [9.95, 10.00)
	if k, ok := v11UpperBound(ms, 0.99); !ok || k != 10.0 {
		t.Errorf("k %g %v, expected 10", k, ok)
	}
	// the rank-996 value exactly on the edge 10.00
	edge := slices.Clone(ms)
	for i := 995; i < len(edge); i++ {
		edge[i] = 10.0
	}
	if k, _ := v11UpperBound(edge, 0.99); k != 10.05 {
		t.Errorf("a value on an edge: k %g, expected 10.05", k)
	}
	high := []float64{}
	for i := 0; i < 1000; i++ {
		high = append(high, 12.5)
	}
	if _, ok := v11UpperBound(high, 0.99); ok {
		t.Errorf("an overflow bin's quantile was used")
	}
	if _, ok := v11UpperBound(ms[:50], 0.99); ok {
		t.Errorf("a rank past the sample was used")
	}
}

// lag.dev's track: 3 near from the 8 nearest in distinct cities, and 5 far,
// 1,500 km away and 500 km from every near pick, in distinct countries.
func TestLagTrackChoice(t *testing.T) {
	w := newWorld(worldConfig{seed: 3}, nil)
	paris, _ := cityNamed("Paris")
	candidates := []choiceCandidate{}
	for i, e := range w.pool {
		at := w.fleets[0][i].at
		candidates = append(candidates, choiceCandidate{index: i, distanceKm: lagdrift.DistanceKm(paris.at, at), bearingDeg: lagdrift.BearingDeg(paris.at, at), at: at, code: e.site.code, city: e.site.name})
	}
	c := chooseExtendersBy(candidates, choiceOptions{mode: choiceLag, visitorCode: "fr"}, newRand(3, 4))
	if len(c.primaries) != lagNear+lagFar || len(c.spares) != lagSpares {
		t.Fatalf("%d primaries and %d spares", len(c.primaries), len(c.spares))
	}
	near := c.primaries[:lagNear]
	cities := map[string]bool{}
	for _, index := range near {
		cities[candidates[index].city] = true
	}
	if len(cities) != lagNear {
		t.Errorf("the near extenders share a city: %v", cities)
	}
	countries := map[string]bool{}
	for _, index := range c.primaries[lagNear:] {
		far := candidates[index]
		countries[far.code] = true
		if far.distanceKm < lagFarMinKm {
			t.Errorf("a far extender %g km away", far.distanceKm)
		}
		for _, n := range near {
			if d := lagdrift.DistanceKm(candidates[n].at, far.at); d < lagFarApartKm {
				t.Errorf("a far extender %g km from a near one", d)
			}
		}
	}
	if len(countries) < lagFar {
		t.Errorf("the far extenders are in %d countries, expected 5", len(countries))
	}
}

// V10 (e2e): with stops on the usable fit, every ping round's fit holds only
// extenders with the usable round trips; and a far session opens late in
// proportion to its distance.
func TestStopsOnUsable(t *testing.T) {
	w := newWorld(worldConfig{seed: 9}, nil)
	specs, _ := buildVisitors(w, 9, []scenarioCount{{scenario: scenarioTiny, perRegion: 1, regions: []string{regionEurope}}}, 0)
	config := baseConfig(9, []stopPolicy{v10Policy(lagdrift.EarlyK)})
	config.stopOnUsable = true
	config.lateFarSessions = true
	config.runs = 1
	sim := newVisitorSim(w, &config, specs[0])
	roundTrips := lagdrift.StopRoundTrips
	trace := sim.trace(0, fmt.Sprintf("|%d", roundTrips), nil, roundTrips)
	fits := 0
	for j := 1; j <= capTicks; j++ {
		state := trace.tick(j)
		usable := 0
		for _, n := range state.roundTrips {
			if roundTrips <= n {
				usable += 1
			}
		}
		if state.fit != nil {
			fits += 1
			if usable < state.fit.Extenders {
				t.Fatalf("at %d ms the fit has %d extenders, only %d usable", state.atMs, state.fit.Extenders, usable)
			}
		}
	}
	if fits == 0 {
		t.Fatalf("no fit in 12 s")
	}
	for _, s := range sim.sessions[0] {
		if s.openMs < 0 {
			continue
		}
		d := lagdrift.DistanceKm(specs[0].truth, sim.trueAt(s.poolIndex))
		if late := lateSessionMs * math.Min(1, d/lateSessionKm); s.openMs < late {
			t.Errorf("a session %g km away opened at %g ms, before its %g ms of lateness", d, s.openMs, late)
		}
	}
}
