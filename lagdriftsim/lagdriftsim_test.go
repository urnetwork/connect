package lagdriftsim

import (
	"math"
	"os"
	"path/filepath"
	"reflect"
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
