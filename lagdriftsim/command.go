package lagdriftsim

// The study as cmd/drift-sim runs it: every part, then REPORT.md, which is
// the program's output from end to end: its prose describes the code, and its
// numbers, summary and recommendation come from the run.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/connect/lagdrift"
)

// What cmd/drift-sim asks for.
type Options struct {
	Seed uint64
	// each scenario's visitors per region, times this
	Scale float64
	// the borders file (lagdrift.ReadBorders); empty runs synthetic countries
	// only
	BordersPath string
	// the parts to run, by name; empty runs every part
	Parts   []string
	Workers int
	// the command line, for the report
	CommandLine string
	// progress, or nil
	Log io.Writer
}

// Runs the study and returns REPORT.md.
func RunStudy(options Options) (string, error) {
	return runParts(options, studyParts(options.Seed, options.Scale))
}

func runParts(options Options, parts []*studyPart) (string, error) {
	logf := func(format string, args ...any) {
		if options.Log != nil {
			fmt.Fprintf(options.Log, format+"\n", args...)
		}
	}
	var borders *lagdrift.Borders
	bordersVersion := "none: synthetic countries only"
	if options.BordersPath != "" {
		f, err := os.Open(options.BordersPath)
		if err != nil {
			return "", err
		}
		borders, err = lagdrift.ReadBorders(f)
		f.Close()
		if err != nil {
			return "", fmt.Errorf("%s: %w", options.BordersPath, err)
		}
		bordersVersion = borders.Version()
	}

	started := time.Now()
	ran := []*studyPart{}
	results := map[string]*studyResult{}
	worlds := map[string]*world{}
	for _, part := range parts {
		if 0 < len(options.Parts) && !slices.Contains(options.Parts, part.name) {
			continue
		}
		partStarted := time.Now()
		config := part.config
		config.workers = options.Workers
		w := newWorld(worldConfig{seed: options.Seed, misplacedShare: config.misplacedShare, sites: config.sites, extraSites: config.extraSites}, borders)
		specs, skipped := buildVisitors(w, options.Seed, part.counts, 0)
		result := runStudy(w, &config, specs)
		result.skipped = skipped
		ran = append(ran, part)
		results[part.name] = result
		worlds[part.name] = w
		logf("%s: %s in %s", part.name, result, time.Since(partStarted).Round(time.Second))
	}
	if len(ran) == 0 {
		return "", errors.New("lagdriftsim: no part to run")
	}
	method := worlds[ran[0].name]
	timingStarted := time.Now()
	times := timeFits(mergedFitSamples(ran, results))
	logf("timed %d sampled fits again in %s", countTimes(times), time.Since(timingStarted).Round(time.Second))

	var b strings.Builder
	b.WriteString(reportIntro())
	b.WriteString(reportSummary(ran, results))
	b.WriteString(reportMethod(method, options.CommandLine, bordersVersion))
	b.WriteString(reportFindings(ran, results, times))
	b.WriteString("## Tables\n\n")
	fmt.Fprintf(&b, "Generated %s in %s: seed %d, scale %g, %s, borders %s, lagdrift's EarlyK %s.\n\n",
		time.Now().UTC().Format("2006-01-02"), time.Since(started).Round(time.Second), options.Seed, options.Scale, runtime.Version(), bordersVersion, formatK(lagdrift.EarlyK))
	writeParts(&b, ran, results, worlds, times)
	return b.String(), nil
}

// The parts' fit samples, at most maxFitSamples per kind in all, taken
// evenly across the parts in their order.
func mergedFitSamples(parts []*studyPart, results map[string]*studyResult) [fitKinds][]fitSample {
	merged := [fitKinds][]fitSample{}
	for kind := range merged {
		all := []fitSample{}
		for _, part := range parts {
			all = append(all, results[part.name].fitSamples[kind]...)
		}
		stride := max(1, (len(all)+maxFitSamples-1)/maxFitSamples)
		for i := 0; i < len(all); i += stride {
			merged[kind] = append(merged[kind], all[i])
		}
	}
	return merged
}

func countTimes(times fitTimes) int {
	n := 0
	for _, kindTimes := range times {
		n += len(kindTimes)
	}
	return n
}

// Writes the report at path.
func WriteReport(path string, report string) error {
	return os.WriteFile(path, []byte(report), 0o644)
}

// The tables of every part that ran.
func writeParts(b *strings.Builder, parts []*studyPart, results map[string]*studyResult, worlds map[string]*world, times fitTimes) {
	main := results["main"]
	worldOf := func(name string) *world {
		if w, ok := worlds[name]; ok {
			return w
		}
		return worlds[parts[0].name]
	}
	section := func(title string) {
		fmt.Fprintf(b, "### %s\n\n", title)
	}
	paragraph := func(format string, args ...any) {
		fmt.Fprintf(b, format+"\n\n", args...)
	}

	section("Parts")
	t := &table{header: []string{"Part", "What it varies", "Visitors", "Policies"}}
	for _, part := range parts {
		if result, ok := results[part.name]; ok {
			t.add(part.name, part.about, formatThousands(float64(len(result.summaries))), fmt.Sprint(len(part.config.policies)))
		}
	}
	b.WriteString(t.String() + "\n")

	if main != nil {
		section("Scenarios")
		b.WriteString(scenarioTable(main) + "\n")
	}

	for _, named := range []struct {
		name   string
		result *studyResult
	}{{"lagdrift", main}} {
		result := named.result
		if result == nil {
			continue
		}
		v10 := result.policyIndex(v10Policy(lagdrift.EarlyK).name)
		section("Coverage and calibration: " + named.name)
		paragraph("The fleet around each region's visitors, and the single-run fits of the tiny-country visitors: their σ_major and how often a run's b is held at its bound of 0. Policy k_early %s.", formatK(lagdrift.EarlyK))
		b.WriteString(coverageTable(worldOf("main"), result, v10) + "\n")
		paragraph("How often the region at each confidence misses the truth, for tiny-country visitors (V6's coverage, the false-reject rate's ceiling for any country). Nominal: 5%%, 1%% and 0.1%%.")
		b.WriteString(calibrationTable(result, v10) + "\n")

		section("False rejects: " + named.name)
		paragraph("Honest visitors rejected (disagree), after 1, 2 and 3 pooled runs, at k_early %s. Bold exceeds 1 − confidence.", formatK(lagdrift.EarlyK))
		b.WriteString(falseRejectTable(result, v10) + "\n")
		paragraph("The tiny country per region:")
		b.WriteString(falseRejectRegionTable(result, v10, scenarioTiny) + "\n")
		if 0 < result.count(ofScenario(scenarioRealHonest)) {
			paragraph("Real countries per region:")
			b.WriteString(falseRejectRegionTable(result, v10, scenarioRealHonest) + "\n")
		}

		section("k_early: " + named.name)
		paragraph("Per policy and confidence: the false-reject rate of the worst honest scenario and number of runs (bold above 1 − confidence), and the false rejects the clear stop adds over no clear stop at all (k_early ∞), per 10,000 honest verdicts. The cap-only policy never stops early.")
		b.WriteString(earlyKTable(result) + "\n")

		section("Power: " + named.name)
		synthetic := []*scenario{scenarioClaim300, scenarioClaim1000, scenarioClaim3000}
		real := []*scenario{scenarioRealClaim300, scenarioRealClaim1000, scenarioRealClaim3000}
		paragraph("Claims rejected (disagree) at 99%%, after 1, 2 and 3 pooled runs, at k_early %s. Synthetic claims: the claimed country's nearest point is exactly the distance away.", formatK(lagdrift.EarlyK))
		b.WriteString(powerTable(result, v10, 1, synthetic) + "\n")
		if 0 < result.count(ofScenario(scenarioRealClaim1000)) {
			paragraph("Real countries, their nearest point 0.8 to 1.25 times the distance away:")
			b.WriteString(powerTable(result, v10, 1, real) + "\n")
		}
		paragraph("At each confidence, all regions:")
		b.WriteString(powerConfidenceTable(result, v10, append(synthetic, real...)) + "\n")

		section("Stop times: " + named.name)
		for run := 0; run < visitorRuns; run++ {
			paragraph("Run %d, k_early %s: seconds from the sessions opening.", run+1, formatK(lagdrift.EarlyK))
			b.WriteString(stopTable(result, v10, run) + "\n")
		}
	}

	if result, ok := results["no early stop"]; ok {
		section("What stopping early does")
		paragraph("One run per visitor under V10, under V10 without the clear stop (k_early ∞), and with no early stop at all, so every run lasts to the 12 s cap. At 99%%.")
		t := &table{header: comparisonHeader}
		for p, policy := range result.config.policies {
			t.add(comparisonRow(policy.name, result, p)...)
		}
		b.WriteString(t.String() + "\n")
	}

	if refits, ok := results["cadence: refits"]; ok {
		if full, ok := results["cadence: full fits"]; ok {
			section("V10's cadence")
			paragraph("The same visitors, 3 runs each: the frame's fit by V10's cadence (a warm refit at each ping round, the full multi-start when the extenders change, every 2 s and before any stop) and by the full multi-start at every ping round. Under the cadence, %d stops were withdrawn because the full fit before them landed in another basin.", refits.withdrawnStops)
			b.WriteString(cadenceTable(refits, full) + "\n")
		}
	}

	if variants := vpnVariantTable(parts, results); strings.Count(variants, "\n") > 2 {
		section("A visitor far from the address's place (VPN)")
		paragraph("Visitors whose address claims a country they are not in, as a VPN's virtual location does, with the extenders chosen around the address's place (E2) or by a variant (choice.go): the mid and far bands spread together with the near band (joint spread), an extender at each VPN hub nearest the address's place (VPN hubs), or greedily by the region they are expected to give for the address's place and the VPN hubs (information). Each on the study's dense fleet and on a sparse one of one extender in each of 35 cities. Power is the share rejected at 99%%, k_early %s.", formatK(lagdrift.EarlyK))
		b.WriteString(variants + "\n")
		for _, pool := range []string{"dense", "sparse"} {
			if pairs := vpnPowerTable(parts, results, pool, true); pairs != "" {
				paragraph("The named pairs on the %s fleet:", pool)
				b.WriteString(pairs + "\n")
				paragraph("Real countries at each distance, on the %s fleet:", pool)
				b.WriteString(vpnPowerTable(parts, results, pool, false) + "\n")
			}
		}
	}

	if interrupted := interruptedTable(parts, results); strings.Count(interrupted, "\n") > 2 {
		section("The interrupted stop")
		paragraph("The review's rule (V10): an interrupted run counts only if converged or clear held, by the server's check, when the silence began. Against an honest client and one that goes silent at its first look whose region touches the claimed country at 99%% once the run would count (6 extenders with 12 round trips), or when clear would stop it outside (review F-02). One run, 99%%.")
		b.WriteString(interrupted + "\n")
	}

	kappaRows := &table{header: comparisonHeader}
	for _, part := range parts {
		result, ok := results[part.name]
		if !ok || !strings.HasPrefix(part.name, "κ ") {
			continue
		}
		kappaRows.add(comparisonRow(part.about, result, result.policyIndex(v10Policy(lagdrift.EarlyK).name))...)
	}
	if 0 < len(kappaRows.rows) {
		section("κ")
		paragraph("After 1 run at 99%%, k_early %s.", formatK(lagdrift.EarlyK))
		b.WriteString(kappaRows.String() + "\n")
	}

	if result, ok := results["stop rule"]; ok {
		section("V10's constants")
		paragraph("One constant changed at a time from V10 (k_early %s); round trips also set the server's usable extender (E5). After 1 run at 99%%.", formatK(lagdrift.EarlyK))
		t := &table{header: comparisonHeader}
		for p, policy := range result.config.policies {
			t.add(comparisonRow(policy.name, result, p)...)
		}
		b.WriteString(t.String() + "\n")
	}

	stress := &table{header: comparisonHeader}
	for _, name := range []string{"main", "misplaced", "normal stretch"} {
		result, ok := results[name]
		if !ok {
			continue
		}
		stress.add(comparisonRow(name, result, result.policyIndex(v10Policy(lagdrift.EarlyK).name))...)
	}
	if 1 < len(stress.rows) {
		section("Other worlds")
		paragraph("After 1 run at 99%%, k_early %s.", formatK(lagdrift.EarlyK))
		b.WriteString(stress.String() + "\n")
	}

	oceania := &table{header: oceaniaHeader}
	var oceaniaBase *studyResult
	for _, part := range parts {
		result, ok := results[part.name]
		if !ok || !strings.HasPrefix(part.name, "Oceania: ") {
			continue
		}
		if part.name == "Oceania: base" {
			oceaniaBase = result
		}
		oceania.add(oceaniaRow(strings.TrimPrefix(part.name, "Oceania: ")+": "+part.about, result)...)
	}
	if 0 < len(oceania.rows) {
		section("Oceania")
		paragraph("Oceania's visitors, one run each at k_early %s: in worlds that each take one source of error away, and under models that each try one remedy. The region's misses are the tiny country's; a false reject needs the whole region to leave the country (V6).", formatK(lagdrift.EarlyK))
		b.WriteString(oceania.String() + "\n")
		if oceaniaBase != nil {
			paragraph("The base world per city:")
			b.WriteString(oceaniaCityTable(oceaniaBase) + "\n")
		}
	}

	section("What a fit costs")
	paragraph("By who fits and what. In the study: every fit's wall-clock duration, shown as the upper limit of its doubling bucket from 0.25 ms; the study's workers share the machine with each other and anything else, so these count waits for a core. One at a time: a sample of the same fits run again after the study on one thread, each measured by the CPU time it took, which is what a quiet machine would see. V10's budget: a warm refit at p95 ≤ 1 ms and a full fit at p95 ≤ 10 ms, in Go.")
	b.WriteString(fitCostTable(parts, results, times) + "\n")

	section("Checks against lagdrift")
	paragraph("The study's stops and verdicts against lagdrift.EvaluateStop and lagdrift.Decide, every seventh visitor. And the fit's own (V5's acceptance): the checked visitors' verdict fits refitted warm from themselves, which should move under 1 km in at least 99.9%% of fits, and, for every 21st visitor's single-run verdicts, a grid of starts (the address's place, the three fastest extenders, 6 bearings at 300, 1,000 and 3,000 km from the fit), each refitted until it settles, which should find no fixed point with a χ² lower by more than 1, the warm rule's tolerance.")
	t = &table{header: []string{"Part", "EvaluateStop checks", "disagree", "Decide checks", "disagree", "Warm refits", "moved ≥ 1 km", "farthest, km", "Grids", "lower χ²", "lower by > 1", "largest drop"}}
	for _, part := range parts {
		if result, ok := results[part.name]; ok {
			fc := result.fitChecks
			t.add(part.name, formatThousands(float64(result.stopChecks)), fmt.Sprint(len(result.stopMismatches)), formatThousands(float64(result.decideChecks)), fmt.Sprint(len(result.decideMismatches)),
				formatThousands(float64(fc.refits)), rate{k: fc.refitMoved, n: fc.refits}.short(), fmt.Sprintf("%.1f", fc.refitMovedKm),
				formatThousands(float64(fc.grids)), fmt.Sprint(fc.gridLower), fmt.Sprint(fc.gridLowerBy1), fmt.Sprintf("%.2f", fc.gridLargestBy))
		}
	}
	b.WriteString(t.String() + "\n")
	for _, part := range parts {
		if result, ok := results[part.name]; ok {
			for _, example := range result.fitChecks.examples[:min(3, len(result.fitChecks.examples))] {
				fmt.Fprintf(b, "- %s: %s\n", part.name, example)
			}
		}
	}
	for _, part := range parts {
		if result, ok := results[part.name]; ok {
			for _, m := range append(result.stopMismatches[:min(3, len(result.stopMismatches))], result.decideMismatches[:min(3, len(result.decideMismatches))]...) {
				fmt.Fprintf(b, "- %s: %s\n", part.name, m)
			}
		}
	}
	b.WriteString("\n")
}
