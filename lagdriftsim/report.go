package lagdriftsim

// The study's tables, as Markdown for REPORT.md. A rate is shown with its
// Wilson 95% interval, so a reader can tell a difference from noise.

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/connect/lagdrift"
)

// k of n.
type rate struct {
	k int
	n int
}

func (self rate) value() float64 {
	if self.n == 0 {
		return math.NaN()
	}
	return float64(self.k) / float64(self.n)
}

// The Wilson score interval at 95%.
func (self rate) interval() (float64, float64) {
	if self.n == 0 {
		return math.NaN(), math.NaN()
	}
	const z = 1.959964
	n := float64(self.n)
	p := float64(self.k) / n
	center := (p + z*z/(2*n)) / (1 + z*z/n)
	half := z * math.Sqrt(p*(1-p)/n+z*z/(4*n*n)) / (1 + z*z/n)
	return math.Max(0, center-half), math.Min(1, center+half)
}

// "1.23% (0.98–1.54)", or "–" for no visitors.
func (self rate) String() string {
	if self.n == 0 {
		return "–"
	}
	lo, hi := self.interval()
	return fmt.Sprintf("%s (%s–%s)", percent(self.value()), percentBare(lo), percentBare(hi))
}

// The rate alone.
func (self rate) short() string {
	if self.n == 0 {
		return "–"
	}
	return percent(self.value())
}

func percent(p float64) string {
	return percentBare(p) + "%"
}

func percentBare(p float64) string {
	switch {
	case math.IsNaN(p):
		return "–"
	case p == 0:
		return "0"
	case p < 0.001:
		return fmt.Sprintf("%.3f", 100*p)
	case p < 0.1:
		return fmt.Sprintf("%.2f", 100*p)
	default:
		return fmt.Sprintf("%.1f", 100*p)
	}
}

// A Markdown table.
type table struct {
	header []string
	rows   [][]string
}

func (self *table) add(cells ...string) {
	self.rows = append(self.rows, cells)
}

func (self *table) String() string {
	var b strings.Builder
	b.WriteString("| " + strings.Join(self.header, " | ") + " |\n")
	align := make([]string, len(self.header))
	for i := range align {
		align[i] = "---"
		if 0 < i {
			align[i] = "--:"
		}
	}
	b.WriteString("|" + strings.Join(align, "|") + "|\n")
	for _, row := range self.rows {
		b.WriteString("| " + strings.Join(row, " | ") + " |\n")
	}
	return b.String()
}

// Visitors a table cell counts.
type visitorFilter func(*visitorSummary) bool

func ofScenario(sc *scenario) visitorFilter {
	return func(s *visitorSummary) bool { return s.scenario == sc }
}

func ofScenarioRegion(sc *scenario, region string) visitorFilter {
	return func(s *visitorSummary) bool { return s.scenario == sc && s.region == region }
}

func ofKind(kind string) visitorFilter {
	return func(s *visitorSummary) bool { return s.scenario.kind == kind }
}

func ofRegion(kind string, region string) visitorFilter {
	return func(s *visitorSummary) bool { return s.scenario.kind == kind && s.region == region }
}

// How often pred holds after run (0 to 2) under a policy, over the visitors
// filter passes.
func (self *studyResult) rateOf(filter visitorFilter, policy int, run int, pred func(runSummary) bool) rate {
	r := rate{}
	if policy < 0 {
		return r
	}
	for _, s := range self.summaries {
		if !filter(s) {
			continue
		}
		r.n += 1
		if pred(s.runs[policy][run]) {
			r.k += 1
		}
	}
	return r
}

func verdictIs(confidence int, verdict lagdrift.Verdict) func(runSummary) bool {
	return func(r runSummary) bool { return r.verdicts[confidence] == verdict }
}

// The truth is outside the region at a confidence, among fitted verdicts.
func (self *studyResult) missRate(filter visitorFilter, policy int, run int, confidence int) rate {
	k := kAt(confidence)
	return self.rateOf(func(s *visitorSummary) bool {
		return filter(s) && s.runs[policy][run].fitted
	}, policy, run, func(r runSummary) bool { return k < r.truthM })
}

// The stop times of run (0 to 2) under a policy, ms, sorted.
func (self *studyResult) stopTimes(filter visitorFilter, policy int, run int) []int64 {
	times := []int64{}
	for _, s := range self.summaries {
		if filter(s) {
			times = append(times, s.runs[policy][run].stopMs)
		}
	}
	slices.Sort(times)
	return times
}

func quantile(sorted []int64, q float64) string {
	if len(sorted) == 0 {
		return "–"
	}
	i := min(len(sorted)-1, int(math.Ceil(q*float64(len(sorted))))-1)
	return fmt.Sprintf("%.2f", float64(sorted[max(0, i)])/1000)
}

func (self *studyResult) count(filter visitorFilter) int {
	n := 0
	for _, s := range self.summaries {
		if filter(s) {
			n += 1
		}
	}
	return n
}

// The scenarios of a kind that have visitors in the result.
func (self *studyResult) present(scenarios []*scenario) []*scenario {
	out := []*scenario{}
	for _, sc := range scenarios {
		if 0 < self.count(ofScenario(sc)) {
			out = append(out, sc)
		}
	}
	return out
}

func confidenceName(i int) string {
	return strings.TrimSuffix(strings.TrimSuffix(fmt.Sprintf("%.1f", 100*confidences[i]), ".0"), "") + "%"
}

// The scenarios: how many visitors each has and how many it couldn't place.
func scenarioTable(result *studyResult) string {
	t := &table{header: []string{"Scenario", "Address", "Country", "Visitors", "Couldn't place"}}
	for _, sc := range append(slices.Clone(honestScenarios), claimScenarios...) {
		n := result.count(ofScenario(sc))
		skipped := 0
		for key, k := range result.skipped {
			if strings.HasPrefix(key, sc.name+", ") {
				skipped += k
			}
		}
		if n == 0 && skipped == 0 {
			continue
		}
		address := "their own country"
		if sc.kind == kindClaim {
			address = fmt.Sprintf("claims a country %s km away", formatThousands(sc.claimKm))
		}
		country := "Natural Earth 1:10m"
		if sc.geometry == geometrySynthetic {
			switch {
			case sc.kind == kindClaim:
				country = "a disk around a real city, its edge exactly at the distance"
			case sc.depthKm < 0:
				country = fmt.Sprintf("a disk of %s km radius, the visitor anywhere in it", formatThousands(sc.radiusKm))
			default:
				country = fmt.Sprintf("a disk of %s km radius, the visitor %s km inside its edge", formatThousands(sc.radiusKm), formatThousands(sc.depthKm))
			}
		} else if sc.kind == kindClaim {
			country = "Natural Earth 1:10m, its nearest point 0.8 to 1.25 times the distance away"
		}
		t.add(sc.name, address, country, formatThousands(float64(n)), formatThousands(float64(skipped)))
	}
	return t.String()
}

func formatThousands(x float64) string {
	s := fmt.Sprintf("%.0f", x)
	n := len(s)
	if n <= 4 {
		return s
	}
	out := ""
	for i, c := range s {
		if 0 < i && (n-i)%3 == 0 {
			out += ","
		}
		out += string(c)
	}
	return out
}

// The fleet around each region's visitors.
func coverageTable(w *world, result *studyResult, policy int) string {
	t := &table{header: []string{"Region", "Cities", "Extenders within 1,000 km", "4th nearest extender, km", "σ_major, 1 run, km", "b held at 0"}}
	regionCities := citiesByRegion()
	for _, region := range regions {
		within := []float64{}
		fourth := []float64{}
		for _, c := range regionCities[region] {
			ds := []float64{}
			for _, e := range w.pool {
				ds = append(ds, lagdrift.DistanceKm(c.at, e.site.at))
			}
			slices.Sort(ds)
			n := 0
			for _, d := range ds {
				if d <= 1000 {
					n += 1
				}
			}
			within = append(within, float64(n))
			fourth = append(fourth, ds[3])
		}
		sigmas := []float64{}
		atZero := rate{}
		for _, s := range result.summaries {
			if s.region != region || s.scenario != scenarioTiny {
				continue
			}
			r := s.runs[policy][0]
			if r.fitted {
				sigmas = append(sigmas, r.majorKm)
			}
		}
		atZero = result.rateOf(ofScenarioRegion(scenarioTiny, region), policy, 0, func(r runSummary) bool { return r.bAtZero })
		t.add(region, fmt.Sprint(len(regionCities[region])), fmt.Sprintf("%.0f", median(within)), fmt.Sprintf("%.0f", median(fourth)), fmt.Sprintf("%.0f", median(sigmas)), atZero.short())
	}
	return t.String()
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	sorted := slices.Clone(xs)
	slices.Sort(sorted)
	return sorted[len(sorted)/2]
}

// How often the region misses the truth, per region, after 1 and 3 runs.
func calibrationTable(result *studyResult, policy int) string {
	t := &table{header: []string{"Region", "1 run: 95%", "99%", "99.9%", "3 runs: 95%", "99%", "99.9%"}}
	for _, region := range regions {
		row := []string{region}
		for _, run := range []int{0, 2} {
			for c := range confidences {
				row = append(row, result.missRate(ofScenarioRegion(scenarioTiny, region), policy, run, c).short())
			}
		}
		t.add(row...)
	}
	row := []string{"All"}
	for _, run := range []int{0, 2} {
		for c := range confidences {
			row = append(row, result.missRate(ofScenario(scenarioTiny), policy, run, c).String())
		}
	}
	t.add(row...)
	row = []string{"k that would hold it (nominal 2.448, 3.035, 3.717)"}
	for _, run := range []int{0, 2} {
		row = append(row, result.empiricalKs(ofScenario(scenarioTiny), policy, run)...)
	}
	t.add(row...)
	return t.String()
}

// False rejects per honest scenario and runs, at each confidence.
func falseRejectTable(result *studyResult, policy int) string {
	t := &table{header: []string{"Scenario", "Runs", "95% (limit 5%)", "99% (limit 1%)", "99.9% (limit 0.1%)", "Inconclusive at 99%"}}
	for _, sc := range result.present(honestScenarios) {
		for run := 0; run < result.runs(); run++ {
			row := []string{sc.name, fmt.Sprint(run + 1)}
			for c := range confidences {
				r := result.rateOf(ofScenario(sc), policy, run, verdictIs(c, lagdrift.VerdictDisagree))
				cell := r.String()
				if 0 < r.n && float64(r.k)/float64(r.n) > 1-confidences[c] {
					cell = "**" + cell + "**"
				}
				row = append(row, cell)
			}
			row = append(row, result.rateOf(ofScenario(sc), policy, run, verdictIs(1, lagdrift.VerdictInconclusive)).short())
			t.add(row...)
		}
	}
	return t.String()
}

// False rejects of one scenario per region, after 1 and 3 runs, at 99%.
func falseRejectRegionTable(result *studyResult, policy int, sc *scenario) string {
	t := &table{header: []string{"Region", "1 run: 95%", "99%", "99.9%", "3 runs: 95%", "99%", "99.9%"}}
	for _, region := range regions {
		row := []string{region}
		for _, run := range []int{0, 2} {
			for c := range confidences {
				r := result.rateOf(ofScenarioRegion(sc, region), policy, run, verdictIs(c, lagdrift.VerdictDisagree))
				cell := r.short()
				if 0 < r.n && float64(r.k)/float64(r.n) > 1-confidences[c] {
					cell = "**" + cell + "**"
				}
				row = append(row, cell)
			}
		}
		t.add(row...)
	}
	return t.String()
}

// The k_early sweep: per policy and confidence, the false-reject rate of the
// worst honest scenario and number of runs, and the false rejects the clear
// stop adds over no clear stop at all, per 10,000 honest verdicts.
func earlyKTable(result *studyResult) string {
	header := []string{"Policy"}
	for c := range confidences {
		header = append(header, confidenceName(c)+": worst cell", "added per 10,000")
	}
	header = append(header, "Honest: median stop, s", "stopped clear", "Claims: median stop, s", "stopped clear")
	t := &table{header: header}
	baseline := result.policyIndex(v10Policy(math.Inf(1)).name)
	scenarios := result.present(honestScenarios)
	for p, policy := range result.config.policies {
		row := []string{policy.name}
		for c := range confidences {
			worst := rate{}
			worstName := ""
			for _, sc := range scenarios {
				for run := 0; run < result.runs(); run++ {
					r := result.rateOf(ofScenario(sc), p, run, verdictIs(c, lagdrift.VerdictDisagree))
					if worst.n == 0 || r.value() > worst.value() {
						worst = r
						worstName = fmt.Sprintf("%s, %d", sc.name, run+1)
					}
				}
			}
			added, total := 0, 0
			for _, s := range result.summaries {
				if s.scenario.kind != kindHonest {
					continue
				}
				for run := 0; run < result.runs(); run++ {
					total += 1
					with := s.runs[p][run].verdicts[c] == lagdrift.VerdictDisagree
					without := s.runs[baseline][run].verdicts[c] == lagdrift.VerdictDisagree
					if with && !without {
						added += 1
					}
					if without && !with {
						added -= 1
					}
				}
			}
			cell := worst.short()
			if worst.value() > 1-confidences[c] {
				cell = "**" + cell + "**"
			}
			row = append(row, cell+" ("+worstName+")", fmt.Sprintf("%+.1f", 10000*float64(added)/float64(max(1, total))))
		}
		for _, kind := range []string{kindHonest, kindClaim} {
			times := []int64{}
			clear := rate{}
			for run := 0; run < result.runs(); run++ {
				times = append(times, result.stopTimes(ofKind(kind), p, run)...)
				r := result.rateOf(ofKind(kind), p, run, func(r runSummary) bool { return r.reason == lagdrift.StopClear })
				clear.k += r.k
				clear.n += r.n
			}
			slices.Sort(times)
			row = append(row, quantile(times, 0.5), clear.short())
		}
		t.add(row...)
	}
	return t.String()
}

// Power per region and claim distance: disagree after 1, 2 and 3 runs at
// 99%, and inconclusive after 1 run.
func powerTable(result *studyResult, policy int, confidence int, scenarios []*scenario) string {
	t := &table{header: []string{"Region", "Claim", "1 run", "2 runs", "3 runs", "Inconclusive, 1 run", "Distance to the country, km"}}
	for _, region := range regions {
		for _, sc := range scenarios {
			filter := ofScenarioRegion(sc, region)
			if result.count(filter) == 0 {
				continue
			}
			row := []string{region, fmt.Sprintf("%s km", formatThousands(sc.claimKm))}
			for run := 0; run < result.runs(); run++ {
				row = append(row, result.rateOf(filter, policy, run, verdictIs(confidence, lagdrift.VerdictDisagree)).short())
			}
			row = append(row, result.rateOf(filter, policy, 0, verdictIs(confidence, lagdrift.VerdictInconclusive)).short())
			distances := []float64{}
			for _, s := range result.summaries {
				if filter(s) {
					distances = append(distances, s.borderKm)
				}
			}
			row = append(row, fmt.Sprintf("%.0f", median(distances)))
			t.add(row...)
		}
	}
	for _, sc := range scenarios {
		filter := ofScenario(sc)
		if result.count(filter) == 0 {
			continue
		}
		row := []string{"All", fmt.Sprintf("%s km", formatThousands(sc.claimKm))}
		for run := 0; run < result.runs(); run++ {
			row = append(row, result.rateOf(filter, policy, run, verdictIs(confidence, lagdrift.VerdictDisagree)).String())
		}
		row = append(row, result.rateOf(filter, policy, 0, verdictIs(confidence, lagdrift.VerdictInconclusive)).short(), "")
		t.add(row...)
	}
	return t.String()
}

// Power at every confidence, all regions, per claim distance and runs.
func powerConfidenceTable(result *studyResult, policy int, scenarios []*scenario) string {
	t := &table{header: []string{"Claim", "Runs", "95%", "99%", "99.9%"}}
	for _, sc := range scenarios {
		if result.count(ofScenario(sc)) == 0 {
			continue
		}
		for run := 0; run < result.runs(); run++ {
			row := []string{fmt.Sprintf("%s km", formatThousands(sc.claimKm)), fmt.Sprint(run + 1)}
			for c := range confidences {
				row = append(row, result.rateOf(ofScenario(sc), policy, run, verdictIs(c, lagdrift.VerdictDisagree)).short())
			}
			t.add(row...)
		}
	}
	return t.String()
}

// Stop times and reasons, per kind and region, for one run.
func stopTable(result *studyResult, policy int, run int) string {
	t := &table{header: []string{"Visitors", "Region", "p10, s", "p50, s", "p90, s", "p99, s", "converged", "clear", "interrupted", "cap", "run counted"}}
	for _, kind := range []string{kindHonest, kindClaim} {
		for _, region := range append(slices.Clone(regions), "All") {
			filter := ofRegion(kind, region)
			if region == "All" {
				filter = ofKind(kind)
			}
			times := result.stopTimes(filter, policy, run)
			if len(times) == 0 {
				continue
			}
			row := []string{kind, region, quantile(times, 0.1), quantile(times, 0.5), quantile(times, 0.9), quantile(times, 0.99)}
			for _, reason := range []lagdrift.StopReason{lagdrift.StopConverged, lagdrift.StopClear, lagdrift.StopInterrupted, lagdrift.StopCap} {
				row = append(row, result.rateOf(filter, policy, run, func(r runSummary) bool { return r.reason == reason }).short())
			}
			row = append(row, result.rateOf(filter, policy, run, func(r runSummary) bool { return r.counted }).short())
			t.add(row...)
		}
	}
	return t.String()
}

// One row of a comparison across parts or policies: false rejects of the
// tiny country and the large country's edge, power at 300 and 1,000 km, all
// after 1 run at 99%, the region's miss rate, and the median stop.
func comparisonRow(name string, result *studyResult, policy int) []string {
	row := []string{name}
	for _, sc := range []*scenario{scenarioTiny, scenarioLargeEdge} {
		row = append(row, result.rateOf(ofScenario(sc), policy, 0, verdictIs(1, lagdrift.VerdictDisagree)).short())
	}
	row = append(row, result.missRate(ofScenario(scenarioTiny), policy, 0, 1).short())
	for _, sc := range []*scenario{scenarioClaim300, scenarioClaim1000, scenarioClaim3000} {
		r := result.rateOf(ofScenario(sc), policy, 0, verdictIs(1, lagdrift.VerdictDisagree))
		row = append(row, r.short())
	}
	times := result.stopTimes(func(s *visitorSummary) bool { return true }, policy, 0)
	capped := result.rateOf(func(s *visitorSummary) bool { return true }, policy, 0, func(r runSummary) bool { return r.reason == lagdrift.StopCap })
	return append(row, quantile(times, 0.5), quantile(times, 0.9), capped.short())
}

var comparisonHeader = []string{"", "False rejects: tiny country", "large country, 5 km in", "Region misses the truth (tiny)", "Power: 300 km", "1,000 km", "3,000 km", "Median stop, s", "p90 stop, s", "At the cap"}

// One row of the Oceania diagnosis: the region's misses and the false
// rejects they cause after V6, the power a remedy costs, and the fits' size.
func oceaniaRow(name string, result *studyResult) []string {
	row := []string{name}
	row = append(row, result.missRate(ofScenario(scenarioTiny), 0, 0, 1).String())
	for _, sc := range []*scenario{scenarioTiny, scenarioLargeEdge, scenarioRealHonest} {
		row = append(row, result.rateOf(ofScenario(sc), 0, 0, verdictIs(1, lagdrift.VerdictDisagree)).short())
	}
	row = append(row, result.rateOf(ofScenario(scenarioRealHonest), 0, 0, verdictIs(2, lagdrift.VerdictDisagree)).short())
	for _, sc := range []*scenario{scenarioClaim300, scenarioClaim1000} {
		row = append(row, result.rateOf(ofScenario(sc), 0, 0, verdictIs(1, lagdrift.VerdictDisagree)).short())
	}
	sigmas := []float64{}
	for _, s := range result.summaries {
		if s.scenario == scenarioTiny && s.runs[0][0].fitted {
			sigmas = append(sigmas, s.runs[0][0].majorKm)
		}
	}
	row = append(row, fmt.Sprintf("%.0f", median(sigmas)))
	row = append(row, result.rateOf(ofScenario(scenarioTiny), 0, 0, func(r runSummary) bool { return r.bAtZero }).short())
	return row
}

var oceaniaHeader = []string{"Oceania", "Region misses the truth, 99% (tiny)", "False rejects, 99%: tiny", "large country, 5 km in", "real countries", "real countries, 99.9%", "Power, 99%: 300 km", "1,000 km", "σ_major, km", "b held at 0"}

// Oceania's base part per city: where the region misses.
func oceaniaCityTable(result *studyResult) string {
	t := &table{header: []string{"City", "Tiny: visitors", "region misses, 99%", "σ_major, km", "Real country: false rejects, 99%", "99.9%", "distance to the border, km"}}
	for _, c := range citiesByRegion()[regionOceania] {
		tiny := func(s *visitorSummary) bool { return s.scenario == scenarioTiny && s.cityName == c.name }
		real := func(s *visitorSummary) bool { return s.scenario == scenarioRealHonest && s.cityName == c.name }
		sigmas := []float64{}
		borders := []float64{}
		for _, s := range result.summaries {
			if tiny(s) && s.runs[0][0].fitted {
				sigmas = append(sigmas, s.runs[0][0].majorKm)
			}
			if real(s) {
				borders = append(borders, s.borderKm)
			}
		}
		t.add(c.name, fmt.Sprint(result.count(tiny)), result.missRate(tiny, 0, 0, 1).short(), fmt.Sprintf("%.0f", median(sigmas)),
			result.rateOf(real, 0, 0, verdictIs(1, lagdrift.VerdictDisagree)).short(), result.rateOf(real, 0, 0, verdictIs(2, lagdrift.VerdictDisagree)).short(), fmt.Sprintf("%.0f", median(borders)))
	}
	return t.String()
}

// Fit durations by kind, over the parts that ran: wall clock in the study,
// and CPU time timed one at a time after it.
func fitCostTable(parts []*studyPart, results map[string]*studyResult, times fitTimes) string {
	t := &table{header: []string{"Fit", "In the study: fits", "mean, ms", "p50 under, ms", "p95 under, ms", "One at a time: fits", "p50, ms", "p90, ms", "p95, ms", "p99, ms", "most, ms"}}
	costs := mergedFitCosts(parts, results)
	for kind, cost := range costs {
		if cost.fits == 0 {
			continue
		}
		sorted := times[kind]
		cpu := func(q float64) string {
			if len(sorted) == 0 {
				return "–"
			}
			return fmt.Sprintf("%.2f", float64(durationQuantile(sorted, q).Microseconds())/1000)
		}
		t.add(fitKindNames[kind], formatThousands(float64(cost.fits)), fmt.Sprintf("%.2f", float64(cost.total.Microseconds())/1000/float64(cost.fits)),
			durationMs(cost.quantile(0.5)), durationMs(cost.quantile(0.95)),
			formatThousands(float64(len(sorted))), cpu(0.5), cpu(0.9), cpu(0.95), cpu(0.99), cpu(1))
	}
	return t.String()
}

func mergedFitCosts(parts []*studyPart, results map[string]*studyResult) [fitKinds]fitCost {
	costs := [fitKinds]fitCost{}
	for _, part := range parts {
		if result, ok := results[part.name]; ok {
			for kind := range costs {
				costs[kind].merge(result.fitCosts[kind])
			}
		}
	}
	return costs
}

func durationMs(d time.Duration) string {
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.2f", float64(d.Microseconds())/1000), "0"), ".")
}

// The cadence against the full fit at every ping round, per k_early: the
// rule's outcome, the worst false-reject cell at 99% and 99.9%, the median
// stop, and how many runs stopped elsewhere or ended in another verdict.
func cadenceTable(refits *studyResult, full *studyResult) string {
	t := &table{header: []string{"k_early", "Rule: cadence", "full fits", "Worst cell at 99%: cadence", "full fits", "at 99.9%: cadence", "full fits", "Median stop, s: cadence", "full fits", "Runs that stop elsewhere", "Verdicts at 99% that differ"}}
	refitChecks, _ := recommendEarlyK(refits)
	fullChecks, _ := recommendEarlyK(full)
	ruleOf := func(checks []earlyKCheck, k float64) string {
		for _, check := range checks {
			if check.k == k {
				if check.holds {
					return "holds"
				}
				return fmt.Sprintf("fails (%d)", len(check.failures))
			}
		}
		return "–"
	}
	worst := func(result *studyResult, policy int, c int) string {
		w := rate{}
		for _, sc := range result.present(honestScenarios) {
			for run := 0; run < result.runs(); run++ {
				r := result.rateOf(ofScenario(sc), policy, run, verdictIs(c, lagdrift.VerdictDisagree))
				if w.n == 0 || w.value() < r.value() {
					w = r
				}
			}
		}
		return w.short()
	}
	for p, policy := range refits.config.policies {
		q := full.policyIndex(policy.name)
		if q < 0 {
			continue
		}
		moved, differ, n := 0, 0, 0
		for i, s := range refits.summaries {
			o := full.summaries[i]
			if s.index != o.index {
				continue
			}
			for run := 0; run < refits.runs(); run++ {
				n += 1
				if s.runs[p][run].stopMs != o.runs[q][run].stopMs {
					moved += 1
				}
				if s.runs[p][run].verdicts[1] != o.runs[q][run].verdicts[1] {
					differ += 1
				}
			}
		}
		all := func(*visitorSummary) bool { return true }
		times := func(result *studyResult, policy int) []int64 {
			ts := []int64{}
			for run := 0; run < result.runs(); run++ {
				ts = append(ts, result.stopTimes(all, policy, run)...)
			}
			slices.Sort(ts)
			return ts
		}
		t.add(formatK(policy.earlyK), ruleOf(refitChecks, policy.earlyK), ruleOf(fullChecks, policy.earlyK),
			worst(refits, p, 1), worst(full, q, 1), worst(refits, p, 2), worst(full, q, 2),
			quantile(times(refits, p), 0.5), quantile(times(full, q), 0.5),
			rate{k: moved, n: n}.short(), rate{k: differ, n: n}.short())
	}
	return t.String()
}

// The VPN parts that ran, by pool, in the modes' order.
func vpnPartsOf(parts []*studyPart, results map[string]*studyResult, pool string) ([]string, []*studyResult) {
	modes := []string{}
	ran := []*studyResult{}
	for _, part := range parts {
		if result, ok := results[part.name]; ok && strings.HasPrefix(part.name, "VPN "+pool+": ") {
			modes = append(modes, part.config.choiceMode)
			ran = append(ran, result)
		}
	}
	return modes, ran
}

// Power against a VPN at 99%, k_early 4: per named pair or distance, after 1
// and 3 runs, under each choice.
func vpnPowerTable(parts []*studyPart, results map[string]*studyResult, pool string, byPair bool) string {
	modes, ran := vpnPartsOf(parts, results, pool)
	if len(ran) == 0 {
		return ""
	}
	header := []string{"Visitor / address", "Distance to the country, km"}
	if !byPair {
		header = []string{"Claim", "Distance to the country, km"}
	}
	for _, mode := range modes {
		header = append(header, mode+": 1 run", "3 runs")
	}
	t := &table{header: header}
	type row struct {
		name   string
		filter visitorFilter
	}
	rows := []row{}
	if byPair {
		for _, pair := range vpnPairs {
			name := pair.cityName + "/" + strings.ToUpper(pair.code)
			rows = append(rows, row{name: name, filter: func(s *visitorSummary) bool { return s.scenario == scenarioVpnPairs && s.claimName == name }})
		}
		rows = append(rows, row{name: "All pairs", filter: ofScenario(scenarioVpnPairs)})
	} else {
		for _, sc := range vpnScenarios {
			rows = append(rows, row{name: formatThousands(sc.claimKm) + " km", filter: ofScenario(sc)})
		}
	}
	for _, r := range rows {
		distances := []float64{}
		for _, s := range ran[0].summaries {
			if r.filter(s) {
				distances = append(distances, s.borderKm)
			}
		}
		cells := []string{r.name, fmt.Sprintf("%.0f", median(distances))}
		for _, result := range ran {
			policy := result.policyIndex(v10Policy(lagdrift.EarlyK).name)
			cells = append(cells, result.rateOf(r.filter, policy, 0, verdictIs(1, lagdrift.VerdictDisagree)).short(),
				result.rateOf(r.filter, policy, result.runs()-1, verdictIs(1, lagdrift.VerdictDisagree)).short())
		}
		t.add(cells...)
	}
	return t.String()
}

// Each VPN part's k_early, false rejects and power side by side.
func vpnVariantTable(parts []*studyPart, results map[string]*studyResult) string {
	t := &table{header: []string{"Fleet, choice", "k_early by the rule", "False rejects, 99%, tiny: 1 run", "3 runs", "real countries: 1 run", "3 runs", "Worst cell at 99.9%", "Region misses, 99%", "VPN pairs: 1 run", "3 runs", "Median stop, s"}}
	for _, part := range parts {
		result, ok := results[part.name]
		if !ok || !strings.HasPrefix(part.name, "VPN ") {
			continue
		}
		policy := result.policyIndex(v10Policy(lagdrift.EarlyK).name)
		checks, recommended := recommendEarlyK(result)
		rule := "none holds"
		if 0 <= recommended {
			rule = formatK(checks[recommended].k)
		}
		worst := rate{}
		for _, sc := range result.present(honestScenarios) {
			for run := 0; run < result.runs(); run++ {
				r := result.rateOf(ofScenario(sc), policy, run, verdictIs(2, lagdrift.VerdictDisagree))
				if worst.n == 0 || worst.value() < r.value() {
					worst = r
				}
			}
		}
		last := result.runs() - 1
		times := result.stopTimes(func(*visitorSummary) bool { return true }, policy, 0)
		t.add(strings.TrimPrefix(part.name, "VPN "), rule,
			result.rateOf(ofScenario(scenarioTiny), policy, 0, verdictIs(1, lagdrift.VerdictDisagree)).short(),
			result.rateOf(ofScenario(scenarioTiny), policy, last, verdictIs(1, lagdrift.VerdictDisagree)).short(),
			result.rateOf(ofScenario(scenarioRealHonest), policy, 0, verdictIs(1, lagdrift.VerdictDisagree)).short(),
			result.rateOf(ofScenario(scenarioRealHonest), policy, last, verdictIs(1, lagdrift.VerdictDisagree)).short(),
			worst.short(),
			result.missRate(ofScenario(scenarioTiny), policy, 0, 1).short(),
			result.rateOf(ofScenario(scenarioVpnPairs), policy, 0, verdictIs(1, lagdrift.VerdictDisagree)).short(),
			result.rateOf(ofScenario(scenarioVpnPairs), policy, last, verdictIs(1, lagdrift.VerdictDisagree)).short(),
			quantile(times, 0.5))
	}
	return t.String()
}

// The interrupted-stop rule: honest V10 and the silent attacker, without and
// with it, after one run at 99%.
func interruptedTable(parts []*studyPart, results map[string]*studyResult) string {
	t := &table{header: []string{"Rule", "Client", "Claims agree: 300 km", "1,000 km", "3,000 km", "Claims rejected: 300 km", "1,000 km", "3,000 km", "False rejects (tiny)", "Runs that don't count", "interrupted"}}
	for _, part := range parts {
		result, ok := results[part.name]
		if !ok || !strings.HasPrefix(part.name, "interrupted: ") {
			continue
		}
		for p, policy := range result.config.policies {
			client := "honest (V10)"
			if policy.attack {
				client = "silent at a favourable look"
			}
			row := []string{strings.TrimPrefix(part.name, "interrupted: "), client}
			for _, verdict := range []lagdrift.Verdict{lagdrift.VerdictAgree, lagdrift.VerdictDisagree} {
				for _, sc := range []*scenario{scenarioClaim300, scenarioClaim1000, scenarioClaim3000} {
					row = append(row, result.rateOf(ofScenario(sc), p, 0, verdictIs(1, verdict)).short())
				}
			}
			all := func(*visitorSummary) bool { return true }
			row = append(row, result.rateOf(ofScenario(scenarioTiny), p, 0, verdictIs(1, lagdrift.VerdictDisagree)).short(),
				result.rateOf(all, p, 0, func(r runSummary) bool { return !r.counted }).short(),
				result.rateOf(all, p, 0, func(r runSummary) bool { return r.reason == lagdrift.StopInterrupted }).short())
			t.add(row...)
		}
	}
	return t.String()
}

// lagdrift's starts against wide ones for the server's verdict, one run, 99%
// and 99.9%: false rejects, the region's misses, and power against a VPN.
func startsTable(parts []*studyPart, results map[string]*studyResult) string {
	t := &table{header: []string{"Verdict's starts", "False rejects, 99%: tiny", "real countries", "99.9%: tiny", "real countries", "Region misses, 99%", "99.9%", "VPN pairs", "300 km", "800 km", "1,500 km", "3,000 km", "6,000 km"}}
	for _, part := range parts {
		result, ok := results[part.name]
		if !ok || !strings.HasPrefix(part.name, "starts: ") {
			continue
		}
		row := []string{strings.TrimPrefix(part.name, "starts: ")}
		for _, c := range []int{1, 2} {
			for _, sc := range []*scenario{scenarioTiny, scenarioRealHonest} {
				row = append(row, result.rateOf(ofScenario(sc), 0, 0, verdictIs(c, lagdrift.VerdictDisagree)).short())
			}
		}
		row = append(row, result.missRate(ofScenario(scenarioTiny), 0, 0, 1).short(), result.missRate(ofScenario(scenarioTiny), 0, 0, 2).short())
		for _, sc := range append([]*scenario{scenarioVpnPairs}, vpnScenarios...) {
			row = append(row, result.rateOf(ofScenario(sc), 0, 0, verdictIs(1, lagdrift.VerdictDisagree)).short())
		}
		t.add(row...)
	}
	return t.String()
}

// The region's misses per calibration world, one run: per confidence, and
// the false rejects of the tiny country and the large country's edge.
func calibrationWorldTable(parts []*studyPart, results map[string]*studyResult) string {
	t := &table{header: []string{"World", "Region misses: 95%", "99%", "99.9%", "False rejects, 99%: tiny", "large country, 5 km in", "99.9%: tiny", "k that holds 95%", "99%", "99.9%"}}
	for _, part := range parts {
		result, ok := results[part.name]
		if !ok || !strings.HasPrefix(part.name, "calibration: ") {
			continue
		}
		row := []string{strings.TrimPrefix(part.name, "calibration: ") + ": " + part.about}
		for c := range confidences {
			row = append(row, result.missRate(ofScenario(scenarioTiny), 0, 0, c).String())
		}
		row = append(row, result.rateOf(ofScenario(scenarioTiny), 0, 0, verdictIs(1, lagdrift.VerdictDisagree)).short(),
			result.rateOf(ofScenario(scenarioLargeEdge), 0, 0, verdictIs(1, lagdrift.VerdictDisagree)).short(),
			result.rateOf(ofScenario(scenarioTiny), 0, 0, verdictIs(2, lagdrift.VerdictDisagree)).short())
		for _, k := range result.empiricalKs(ofScenario(scenarioTiny), 0, 0) {
			row = append(row, k)
		}
		t.add(row...)
	}
	return t.String()
}

// The k at each confidence whose region would miss the truth at the nominal
// rate: the quantile of the truth's Mahalanobis distance from the fit, over
// fitted verdicts. Nominal: 2.448, 3.035 and 3.717.
func (self *studyResult) empiricalKs(filter visitorFilter, policy int, run int) []string {
	ms := []float64{}
	for _, s := range self.summaries {
		if filter(s) && s.runs[policy][run].fitted {
			ms = append(ms, s.runs[policy][run].truthM)
		}
	}
	slices.Sort(ms)
	out := []string{}
	for _, c := range confidences {
		if len(ms) == 0 {
			out = append(out, "–")
			continue
		}
		i := min(len(ms)-1, int(math.Ceil(c*float64(len(ms))))-1)
		out = append(out, fmt.Sprintf("%.2f", ms[max(0, i)]))
	}
	return out
}

// The k options for the user's choice: as now (2.448, 3.035, 3.717), the
// empirical k (2.448, 3.28, 7.15), and no 99.9% with 3.28 at 99%.
var kOptionNames = []string{"(a) k as now", "(b) empirical k", "(c) no 99.9%, 3.28 at 99%"}

// The verdict at confidence index c under a k option, false where the option
// has no such confidence.
func optionVerdict(option int, r runSummary, c int) (lagdrift.Verdict, bool) {
	switch {
	case option == 0:
		return r.verdicts[c], true
	case c == 0:
		return r.verdicts[0], true
	case c == 1:
		return r.kVerdicts[0], true
	case option == 1:
		return r.kVerdicts[1], true
	default:
		return "", false
	}
}

func optionK(option int, c int) float64 {
	if option == 0 || c == 0 {
		return kAt(c)
	}
	return recalibratedKs[c-1]
}

// False rejects and power under each k option, after 1 and 3 runs.
func recalibrationTable(result *studyResult, policy int) string {
	t := &table{header: []string{"k", "Confidence", "False rejects, tiny: 1 run", "3 runs", "real countries: 1 run", "3 runs", "Power: 300 km, 1 run", "3 runs", "1,000 km: 1 run", "3 runs", "3,000 km: 1 run", "3 runs", "real 1,000 km: 1 run", "3 runs"}}
	last := result.runs() - 1
	for option := range kOptionNames {
		for c := range confidences {
			if _, ok := optionVerdict(option, runSummary{}, c); !ok {
				continue
			}
			disagree := func(r runSummary) bool {
				v, _ := optionVerdict(option, r, c)
				return v == lagdrift.VerdictDisagree
			}
			row := []string{fmt.Sprintf("%s: %.3f", kOptionNames[option], optionK(option, c)), confidenceName(c)}
			for _, sc := range []*scenario{scenarioTiny, scenarioRealHonest, scenarioClaim300, scenarioClaim1000, scenarioClaim3000, scenarioRealClaim1000} {
				row = append(row, result.rateOf(ofScenario(sc), policy, 0, disagree).short(), result.rateOf(ofScenario(sc), policy, last, disagree).short())
			}
			t.add(row...)
		}
	}
	return t.String()
}

// The Mahalanobis distance of p from a fit at at with covariance cov.
func mahalanobisKm(at lagdrift.Point, cov [2][2]float64, p lagdrift.Point) float64 {
	east, north := lagdrift.AzimuthalKm(at, p)
	a := cov[0][0]
	b := (cov[0][1] + cov[1][0]) / 2
	c := cov[1][1]
	det := a*c - b*b
	if !(0 < det) {
		return math.Inf(1)
	}
	return math.Sqrt((c*east*east - 2*b*east*north + a*north*north) / det)
}

// A city point's error, per axis, km: a mean radial error of about 30 km.
const cityErrorKm = 24

// V4 estimating k from tracks: honest single-run fits measured from a city
// point off the truth by a normal error of 24 km per axis (about 30 km), as
// is and with the city's variance added to the fit's covariance, and how much
// the 99% and 99.9% quantiles of a sample of n tracks spread.
func calibrationFromTracksTable(result *studyResult, policy int) string {
	truthMs, cityMs, correctedMs := []float64{}, []float64{}, []float64{}
	for _, s := range result.summaries {
		r := s.runs[policy][0]
		if s.scenario.kind != kindHonest || !r.fitted {
			continue
		}
		rr := newRand(result.config.seed, drawScenario, stringKey("city error"), uint64(s.index))
		city := lagdrift.MoveKm(s.truth, cityErrorKm*rr.NormFloat64(), cityErrorKm*rr.NormFloat64())
		widened := r.cov
		widened[0][0] += cityErrorKm * cityErrorKm
		widened[1][1] += cityErrorKm * cityErrorKm
		truthMs = append(truthMs, r.truthM)
		cityMs = append(cityMs, mahalanobisKm(r.at, r.cov, city))
		correctedMs = append(correctedMs, mahalanobisKm(r.at, widened, city))
	}
	if len(truthMs) == 0 {
		return ""
	}
	quantileOf := func(xs []float64, q float64) float64 {
		sorted := slices.Clone(xs)
		slices.Sort(sorted)
		return sorted[min(len(sorted)-1, max(0, int(math.Ceil(q*float64(len(sorted))))-1))]
	}
	t := &table{header: []string{"Measured from", "Tracks", "k at 99%: estimate", "5–95% of estimates", "k at 99.9%: estimate", "5–95% of estimates"}}
	for _, from := range []struct {
		name string
		ms   []float64
	}{{"the truth", truthMs}, {"the city point", cityMs}, {"the city point, its variance added", correctedMs}} {
		t.add(from.name, formatThousands(float64(len(from.ms))), fmt.Sprintf("%.2f", quantileOf(from.ms, 0.99)), "", fmt.Sprintf("%.2f", quantileOf(from.ms, 0.999)), "")
	}
	r := newRand(result.config.seed, drawScenario, stringKey("bootstrap"))
	for _, n := range []int{1000, 3000, 10000, 30000} {
		k99, k999 := []float64{}, []float64{}
		for b := 0; b < 300; b++ {
			sample := make([]float64, n)
			for i := range sample {
				sample[i] = correctedMs[r.IntN(len(correctedMs))]
			}
			k99 = append(k99, quantileOf(sample, 0.99))
			k999 = append(k999, quantileOf(sample, 0.999))
		}
		t.add("the city point, its variance added: a sample of", formatThousands(float64(n)),
			fmt.Sprintf("%.2f", quantileOf(k99, 0.5)), fmt.Sprintf("%.2f–%.2f", quantileOf(k99, 0.05), quantileOf(k99, 0.95)),
			fmt.Sprintf("%.2f", quantileOf(k999, 0.5)), fmt.Sprintf("%.2f–%.2f", quantileOf(k999, 0.05), quantileOf(k999, 0.95)))
	}
	return t.String()
}

// The empirical k per region, from the truth, honest single runs.
func regionKTable(result *studyResult, policy int) string {
	t := &table{header: []string{"Region", "Fits", "k at 95%", "99%", "99.9%"}}
	for _, region := range append(slices.Clone(regions), "All") {
		filter := ofRegion(kindHonest, region)
		if region == "All" {
			filter = ofKind(kindHonest)
		}
		n := 0
		for _, s := range result.summaries {
			if filter(s) && s.runs[policy][0].fitted {
				n += 1
			}
		}
		t.add(append([]string{region, formatThousands(float64(n))}, result.empiricalKs(filter, policy, 0)...)...)
	}
	return t.String()
}
