package lagdriftsim

// REPORT.md's prose: what the study is and how it works. The numbers in it
// that come from the study's own data (the fleet, the cities) are computed;
// the rest describes the code it sits beside.

import (
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/urnetwork/connect/lagdrift"
)

// The report's opening, before the summary.
func reportIntro() string {
	return `# The drift test's error rates (lag.dev/DRIFTTEST.md T3)

A simulation study of the drift test against its authoritative model, ` + "`lagdrift`" + `: how often an honest visitor is
rejected, how often a false claim is caught, how long runs take, and where to set V10's early-stop threshold k_early
(` + "`lagdrift.EarlyK`" + `). drift-sim writes this file; edit the program, not the file.

`
}

// How the study works.
func reportMethod(w *world, commandLine string, bordersVersion string) string {
	var b strings.Builder
	sites := map[string]bool{}
	regionCounts := map[string]int{}
	for _, e := range w.pool {
		sites[e.site.name+"/"+e.site.code] = true
	}
	for _, c := range visitorCities {
		regionCounts[c.region] += 1
	}
	fmt.Fprintf(&b, `## How the study works

**The model under test** is `+"`github.com/urnetwork/connect/lagdrift`"+`, called as the frame and the api call it:
- `+"`EstimateRoundTrip`"+` for each extender's 10th percentile (V1);
- `+"`FitPosition`"+` for the frame's fit after every ping round, pooled with the runs in the verdict (V9), and for the
  server's fit of the pooled runs (V5, V7);
- `+"`Judge`"+` and `+"`PhysicallyPossible`"+` for the verdict (V5, V6), and V10's rules for the stop.

The study varies k_early and V10's other constants, which are constants in `+"`lagdrift`"+`. So it evaluates V10's rules
itself, with lagdrift's region test, and decides the verdict at all three confidences from one region test. Every
seventh visitor is checked against `+"`lagdrift.EvaluateStop`"+` at every ping round and against `+"`lagdrift.Decide`"+` at
every verdict (the checks table).

**Extenders.** lag.dev's e2e stack (`+"`lag.dev/e2e/stack/dots.json`"+`) has 12 real cities, all chosen around one visitor in
Austin: too few for E2's bands from anywhere else. So the fleet is a plausible global one, weighted toward Europe and
North America: %d extenders in %d datacenter cities (81 in Europe, 58 in North America, 21 in Asia, 2 in the Middle
East, 8 in South America, 6 in Africa, 6 in Oceania), with dots.json's 12 among them (`+"`places.go`"+`).
- **Places.** An extender's place is its city's point, as GeoLite2 gives a datacenter address. Its accuracy radius is
  10 km (30%%), 20 (25%%), 50 (25%%), 100 (15%%) or 200 km (5%%), or the place is derived (10%%, held at 50 km). The radius
  is about a 67%% radius, so the true place is off by a normal draw with a per-axis σ of radius / 1.5. A radius of
  200 km, which the model holds to 100 km, is a place the model trusts too much.
- **Fleets.** Each of %d fleets draws every extender's place error once, and visitors are spread over the fleets, so
  the rates average over place errors rather than resting on one draw.
- **Serving.** Each extender serves with a %.0f%% chance per visitor (A4).

**Visitors** are in %d cities (Europe %d, North America %d, Eastern Europe and the Middle East %d, Asia %d, South
America %d, Africa %d, Oceania %d; `+"`places.go`"+`): big cities inland and on coasts, cities on land borders (Strasbourg,
Basel, Geneva, Aachen, Detroit, El Paso, Tijuana, Kinshasa, Asunción, …) and in small countries (Luxembourg,
Singapore, Hong Kong). A visitor is within %d km of their city's point, which is the address's place.
- **Honest visitors** are in their address's country: either a synthetic disk around them at a controlled distance
  from its edge, or their city's real country from Natural Earth's Admin 0 at 1:10m (borders: %s). The worst case is
  the tiny country, a 20 km disk with the visitor anywhere in it (Luxembourg, Andorra, Singapore): there a false reject
  is exactly the region missing the truth, the rate every other country's is below.
- **Claims** have an address that claims a country 300, 1,000 or 3,000 km from where they are, placed at that country's
  capital or main VPN city, and E2 chooses the extenders around that place. The claimed country is either a disk around
  a real city whose edge is exactly at the distance, or a real country whose nearest point is 0.8 to 1.25 times the
  distance away.

**Runs.** A visitor makes 3 runs in the main part, and the verdict after each pools the counting runs so far (V7).
- **Extenders (E2):** the 4 nearest, preferring 2 in the IP country; 4 between 300 and 2,500 km and 4 between 2,500 and
  9,000 km, each spread by bearing (the largest smallest gap); 4 spares. A retest prefers extenders the earlier runs
  didn't use. A band that is short is filled from the next band out.
- **Sessions:** each opens 60 ms + 3 round trips + up to 250 ms after the run starts; %.0f%% never open, and a spare
  replaces each at 4 s. The extender sends one ping at a time, 250 ms apart or at the last pong, and a ping with no pong
  in 5 s is lost and holds the next for 5 s (M4).
- **Round trips** are b_r + K · d(truth, the extender's true place) · (1 + s) + queue(t) + jitter + spikes, plus a TCP
  retransmission (200 ms or more) for a dropped packet (`+"`network.go`"+`):
  - **s, the route stretch,** is drawn once per path: mean 0 and standard deviation κ, the model's route error, but
    floored at −0.3, since no path is faster than light in fiber along the great circle (K's speed is 2/3 of fiber's),
    and so skewed, with a tail of detours: a Gamma shifted to the floor. κ is %.2f unless a part says otherwise.
  - **The access link** is fiber (25%%: b 2–6 ms), cable or DSL (25%%: 6–20 ms), Wi-Fi (35%%: 5–24 ms) or mobile (15%%:
    20–50 ms), and b changes by up to ±15%% between runs. Its queue is shared by every path: an Ornstein–Uhlenbeck
    process with a 0.8 s correlation time and a scale of 0.3 ms (fiber) to 5 ms (mobile), plus each ping's own jitter.
    Spike episodes (0.02 a second on fiber, 0.3 on Wi-Fi and mobile, about 0.3 s long, 3 to 150 ms high) delay every
    ping sent in them. Retransmissions are 0.1%% (fiber) to 1.5%% (mobile) of pings, 5 times as many in a spike.
  - **The run's link** drops for good at a random moment in %.0f%% of runs, which ends them as `+"`interrupted`"+`.
- **The frame** fits every 250 ms once 6 extenders have 4 round trips each, pooled with the counting runs, and evaluates
  V10 from 3 s.
- **The server** takes each extender with 12 round trips measured by the moment the sessions closed (E5), counts the
  run with %d such, pools the last %d counting runs (V7), and decides at 95%%, 99%% and 99.9%%: inconclusive for a
  σ_major above %.0f km or a region that touches the country while the physical check fails, else agree when the region
  touches the country, else disagree (V6).

**Parts.** The main part sweeps k_early with 3 runs per visitor. The other parts, one run per visitor, compare stopping
early with not, vary κ in the world and in the model, change V10's other constants one at a time, change the world
(misplaced extenders, a normal route stretch), and diagnose Oceania.

**Rates** are shown with their Wilson 95%% intervals where it matters, so a difference can be told from noise. A
"worst cell" is the highest rate over the honest scenarios and the number of runs.

**Reproducing.** Every draw comes from a PCG generator keyed by the seed and by what it is for (`+"`seed.go`"+`), so a part
reproduces exactly, whatever the machine or the number of workers. This report came from:

`+"```"+`
go run ./lagdrift/cmd/drift-borders -borders /tmp/ne-admin0-10m.bin
%s
`+"```"+`

Without `+"`-borders`"+`, drift-sim looks for the file in the config repository (`+"`../config/{all,local}/lag/ne-admin0-10m*.bin`"+`),
and without one it runs the synthetic countries only. `+"`-scale 0.1`"+` runs a tenth of the visitors, and `+"`-parts`"+` some of
the parts.

`, len(w.pool), len(sites), fleetCount, 100*servingShare,
		len(visitorCities), regionCounts[regionEurope], regionCounts[regionNorthAmerica], regionCounts[regionEastMiddle], regionCounts[regionAsia],
		regionCounts[regionSouthAmerica], regionCounts[regionAfrica], regionCounts[regionOceania], visitorSpreadKm, bordersVersion,
		100*sessionFailShare, lagdrift.DefaultKappa, 100*linkDropShare, countingExtenders, windowRuns, float64(lagdrift.DefaultMaxSigmaKm), commandLine)
	return b.String()
}

// One k_early of the sweep against the rule the study sets it by.
type earlyKCheck struct {
	k      float64
	policy int
	holds  bool
	// the cells that break the rule
	failures []string
}

// The rule (T3): in every honest scenario, after 1, 2 and 3 runs and at every
// confidence, the false-reject rate is within 1 − confidence where it is with
// no clear stop at all (k_early ∞), and the clear stop adds no false reject
// where even that isn't: there the excess is the region's calibration, which
// no k_early can fix.
func checkEarlyK(result *studyResult, policy int, baseline int) earlyKCheck {
	check := earlyKCheck{k: result.config.policies[policy].earlyK, policy: policy, holds: true}
	for _, sc := range result.present(honestScenarios) {
		for run := 0; run < result.runs(); run++ {
			for c := range confidences {
				alpha := 1 - confidences[c]
				with := result.rateOf(ofScenario(sc), policy, run, verdictIs(c, lagdrift.VerdictDisagree))
				without := result.rateOf(ofScenario(sc), baseline, run, verdictIs(c, lagdrift.VerdictDisagree))
				cell := fmt.Sprintf("%s, %d run%s, %s", sc.name, run+1, plural(run+1), confidenceName(c))
				switch {
				case without.value() <= alpha && alpha < with.value():
					check.holds = false
					check.failures = append(check.failures, fmt.Sprintf("%s: %s, over %s", cell, with.short(), percent(alpha)))
				case alpha < without.value() && without.k < with.k:
					check.holds = false
					check.failures = append(check.failures, fmt.Sprintf("%s: %s, up from %s with no clear stop", cell, with.short(), without.short()))
				}
			}
		}
	}
	return check
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// The runs the result's visitors made.
func (self *studyResult) runs() int {
	if 0 < self.config.runs && self.config.runs < visitorRuns {
		return self.config.runs
	}
	return visitorRuns
}

// The sweep's checks, in its order, and the smallest finite k_early that
// holds, or −1.
func recommendEarlyK(result *studyResult) ([]earlyKCheck, int) {
	baseline := result.policyIndex(v10Policy(math.Inf(1)).name)
	checks := []earlyKCheck{}
	recommended := -1
	for p, policy := range result.config.policies {
		if !policy.converged || policy.roundTrips != lagdrift.StopRoundTrips || p == baseline {
			continue
		}
		check := checkEarlyK(result, p, baseline)
		checks = append(checks, check)
		if check.holds && (recommended < 0 || check.k < checks[recommended].k) {
			recommended = len(checks) - 1
		}
	}
	return checks, recommended
}

// The report's summary: the recommendation and the numbers it rests on.
func reportSummary(parts []*studyPart, results map[string]*studyResult) string {
	var b strings.Builder
	b.WriteString("## Summary\n\n")
	main := results["main"]
	if main == nil || main.policyIndex(v10Policy(math.Inf(1)).name) < 0 {
		b.WriteString("The main part, the k_early sweep, didn't run, so this report makes no recommendation.\n\n")
		return b.String()
	}
	checks, recommended := recommendEarlyK(main)
	doc := -1
	for i, check := range checks {
		if check.k == lagdrift.EarlyK {
			doc = i
		}
	}
	policy := main.policyIndex(v10Policy(lagdrift.EarlyK).name)
	if 0 <= recommended {
		check := checks[recommended]
		policy = check.policy
		fmt.Fprintf(&b, "- **k_early: %s.** It is the smallest of the sweep (%s) that keeps the false-reject rate within 1 − confidence at 95%%, 99%% and 99.9%%, after 1, 2 and 3 runs, in every honest scenario where a run with no clear stop is within it, and adds no false reject where even that isn't.", formatK(check.k), sweepList(checks))
	} else {
		fmt.Fprintf(&b, "- **k_early: none of the sweep (%s) holds** the rule that sets it: within 1 − confidence wherever a run with no clear stop is, and no false reject added where it isn't.", sweepList(checks))
	}
	if 0 <= doc {
		if checks[doc].holds {
			fmt.Fprintf(&b, " The doc's %s holds too.", formatK(lagdrift.EarlyK))
		} else {
			fmt.Fprintf(&b, " The doc's %s fails it: %s.", formatK(lagdrift.EarlyK), strings.Join(checks[doc].failures[:min(3, len(checks[doc].failures))], "; "))
		}
	}
	b.WriteString("\n")
	if 0 <= recommended && 0 <= doc && recommended != doc {
		for _, kind := range []string{kindHonest, kindClaim} {
			fmt.Fprintf(&b, "  - %s runs: %s stop clear at k_early %s and %s at %s; the median run is %s s and %s s.\n", kindName(kind),
				clearShare(main, checks[recommended].policy, kind), formatK(checks[recommended].k), clearShare(main, checks[doc].policy, kind), formatK(checks[doc].k),
				medianStop(main, checks[recommended].policy, kind), medianStop(main, checks[doc].policy, kind))
		}
	}
	name := main.config.policies[policy].name
	fmt.Fprintf(&b, "- **False rejects** at %s, after 1, 2 and 3 runs:\n", name)
	for _, sc := range main.present(honestScenarios) {
		fmt.Fprintf(&b, "  - %s:", sc.name)
		for c := range confidences {
			cells := []string{}
			for run := 0; run < main.runs(); run++ {
				cells = append(cells, main.rateOf(ofScenario(sc), policy, run, verdictIs(c, lagdrift.VerdictDisagree)).short())
			}
			sep := ";"
			if c == len(confidences)-1 {
				sep = "."
			}
			fmt.Fprintf(&b, " %s at %s%s", strings.Join(cells, ", "), confidenceName(c), sep)
		}
		b.WriteString("\n")
	}
	miss := main.missRate(ofScenario(scenarioTiny), policy, 0, 1)
	worstRegion, worstMiss := "", rate{}
	for _, region := range regions {
		r := main.missRate(ofScenarioRegion(scenarioTiny, region), policy, 0, 1)
		if worstRegion == "" || worstMiss.value() < r.value() {
			worstRegion, worstMiss = region, r
		}
	}
	fmt.Fprintf(&b, "- **Calibration:** the 99%% region misses the truth in %s of single runs (nominal 1%%), most in %s at %s. A country's false-reject rate is at most its region's miss rate, which the tiny country reaches.\n",
		miss.String(), worstRegion, worstMiss.short())
	b.WriteString("- **Power** at 99% after 1, 2 and 3 runs, all regions:")
	for _, sc := range []*scenario{scenarioClaim300, scenarioClaim1000, scenarioClaim3000, scenarioRealClaim300, scenarioRealClaim1000, scenarioRealClaim3000} {
		if main.count(ofScenario(sc)) == 0 {
			continue
		}
		cells := []string{}
		for run := 0; run < main.runs(); run++ {
			cells = append(cells, main.rateOf(ofScenario(sc), policy, run, verdictIs(1, lagdrift.VerdictDisagree)).short())
		}
		fmt.Fprintf(&b, " %s: %s;", sc.name, strings.Join(cells, ", "))
	}
	b.WriteString(" by region in the tables.\n")
	for _, kind := range []string{kindHonest, kindClaim} {
		times := main.stopTimes(ofKind(kind), policy, 0)
		capped := main.rateOf(ofKind(kind), policy, 0, func(r runSummary) bool { return r.reason == lagdrift.StopCap })
		fmt.Fprintf(&b, "- **Stop times** of %s first runs: median %s s, p90 %s s, p99 %s s; %s at the 12 s cap.\n", kindName(kind), quantile(times, 0.5), quantile(times, 0.9), quantile(times, 0.99), capped.short())
	}
	b.WriteString("\n")
	return b.String()
}

func kindName(kind string) string {
	if kind == kindHonest {
		return "honest visitors'"
	}
	return "claims'"
}

func sweepList(checks []earlyKCheck) string {
	ks := []string{}
	for _, check := range checks {
		ks = append(ks, formatK(check.k))
	}
	return strings.Join(ks, ", ")
}

func clearShare(result *studyResult, policy int, kind string) string {
	clear := rate{}
	for run := 0; run < result.runs(); run++ {
		r := result.rateOf(ofKind(kind), policy, run, func(r runSummary) bool { return r.reason == lagdrift.StopClear })
		clear.k += r.k
		clear.n += r.n
	}
	return clear.short()
}

func medianStop(result *studyResult, policy int, kind string) string {
	times := []int64{}
	for run := 0; run < result.runs(); run++ {
		times = append(times, result.stopTimes(ofKind(kind), policy, run)...)
	}
	slices.Sort(times)
	return quantile(times, 0.5)
}

// The report's findings beyond the summary, each from the parts that ran.
func reportFindings(parts []*studyPart, results map[string]*studyResult, times fitTimes) string {
	var b strings.Builder
	b.WriteString("## Findings\n\n")
	if main := results["main"]; main != nil && 0 <= main.policyIndex(v10Policy(math.Inf(1)).name) {
		checks, _ := recommendEarlyK(main)
		b.WriteString("**The k_early sweep.** Each value of the sweep against the rule:\n\n")
		for _, check := range checks {
			if check.holds {
				fmt.Fprintf(&b, "- %s: holds.\n", formatK(check.k))
			} else {
				fmt.Fprintf(&b, "- %s: fails in %d cell%s, e.g. %s.\n", formatK(check.k), len(check.failures), plural(len(check.failures)), check.failures[0])
			}
		}
		over := []string{}
		baseline := main.policyIndex(v10Policy(math.Inf(1)).name)
		for _, sc := range main.present(honestScenarios) {
			for run := 0; run < main.runs(); run++ {
				for c := range confidences {
					r := main.rateOf(ofScenario(sc), baseline, run, verdictIs(c, lagdrift.VerdictDisagree))
					if 1-confidences[c] < r.value() {
						over = append(over, fmt.Sprintf("%s, %d run%s, %s: %s", sc.name, run+1, plural(run+1), confidenceName(c), r.String()))
					}
				}
			}
		}
		if 0 < len(over) {
			fmt.Fprintf(&b, "\nWith no clear stop at all, these cells are already over 1 − confidence, which is the region's calibration rather than early stopping: %s.\n", strings.Join(over, "; "))
		} else {
			b.WriteString("\nWith no clear stop at all, every honest cell is within 1 − confidence.\n")
		}
		b.WriteString("\n")
	}
	if result, ok := results["no early stop"]; ok {
		v10 := result.policyIndex(v10Policy(lagdrift.EarlyK).name)
		none := result.policyIndex(capPolicy().name)
		fmt.Fprintf(&b, "**Stopping early** (one run): V10 at k_early %s rejects %s of tiny-country visitors at 99%%, against %s for runs that all last to the cap; its median run is %s s against 12 s, and it rejects %s of 1,000 km claims against %s.\n\n",
			formatK(lagdrift.EarlyK),
			result.rateOf(ofScenario(scenarioTiny), v10, 0, verdictIs(1, lagdrift.VerdictDisagree)).short(),
			result.rateOf(ofScenario(scenarioTiny), none, 0, verdictIs(1, lagdrift.VerdictDisagree)).short(),
			quantile(result.stopTimes(func(*visitorSummary) bool { return true }, v10, 0), 0.5),
			result.rateOf(ofScenario(scenarioClaim1000), v10, 0, verdictIs(1, lagdrift.VerdictDisagree)).short(),
			result.rateOf(ofScenario(scenarioClaim1000), none, 0, verdictIs(1, lagdrift.VerdictDisagree)).short())
	}
	kappaLines := []string{}
	for _, part := range parts {
		result, ok := results[part.name]
		if !ok || !strings.HasPrefix(part.name, "κ ") {
			continue
		}
		v10 := result.policyIndex(v10Policy(lagdrift.EarlyK).name)
		kappaLines = append(kappaLines, fmt.Sprintf("- %s: the region misses %s, false rejects %s (tiny) and %s (large country's edge), power %s at 300 km and %s at 1,000 km.",
			part.about, result.missRate(ofScenario(scenarioTiny), v10, 0, 1).short(),
			result.rateOf(ofScenario(scenarioTiny), v10, 0, verdictIs(1, lagdrift.VerdictDisagree)).short(),
			result.rateOf(ofScenario(scenarioLargeEdge), v10, 0, verdictIs(1, lagdrift.VerdictDisagree)).short(),
			result.rateOf(ofScenario(scenarioClaim300), v10, 0, verdictIs(1, lagdrift.VerdictDisagree)).short(),
			result.rateOf(ofScenario(scenarioClaim1000), v10, 0, verdictIs(1, lagdrift.VerdictDisagree)).short()))
	}
	if 0 < len(kappaLines) {
		b.WriteString("**κ** (one run, 99%):\n\n" + strings.Join(kappaLines, "\n") + "\n\n")
	}
	oceaniaLines := []string{}
	for _, part := range parts {
		result, ok := results[part.name]
		if !ok || !strings.HasPrefix(part.name, "Oceania: ") {
			continue
		}
		oceaniaLines = append(oceaniaLines, fmt.Sprintf("- %s (%s): the region misses %s; false rejects %s in real countries, %s in the tiny one; power %s at 300 km and %s at 1,000 km.",
			strings.TrimPrefix(part.name, "Oceania: "), part.about,
			result.missRate(ofScenario(scenarioTiny), 0, 0, 1).String(),
			result.rateOf(ofScenario(scenarioRealHonest), 0, 0, verdictIs(1, lagdrift.VerdictDisagree)).short(),
			result.rateOf(ofScenario(scenarioTiny), 0, 0, verdictIs(1, lagdrift.VerdictDisagree)).short(),
			result.rateOf(ofScenario(scenarioClaim300), 0, 0, verdictIs(1, lagdrift.VerdictDisagree)).short(),
			result.rateOf(ofScenario(scenarioClaim1000), 0, 0, verdictIs(1, lagdrift.VerdictDisagree)).short()))
	}
	if 0 < len(oceaniaLines) {
		b.WriteString("**Oceania** (one run, 99%), each line taking one source of error away or trying one remedy:\n\n" + strings.Join(oceaniaLines, "\n") + "\n\n")
	}
	if result, ok := results["stop rule"]; ok {
		b.WriteString("**V10's other constants** (one run, 99%):\n\n")
		for p, policy := range result.config.policies {
			times := result.stopTimes(func(*visitorSummary) bool { return true }, p, 0)
			fmt.Fprintf(&b, "- %s: median run %s s, p90 %s s; false rejects %s (tiny); power %s at 300 km.\n", policy.name, quantile(times, 0.5), quantile(times, 0.9),
				result.rateOf(ofScenario(scenarioTiny), p, 0, verdictIs(1, lagdrift.VerdictDisagree)).short(),
				result.rateOf(ofScenario(scenarioClaim300), p, 0, verdictIs(1, lagdrift.VerdictDisagree)).short())
		}
		b.WriteString("\n")
	}
	vpnLines := []string{}
	for _, part := range parts {
		result, ok := results[part.name]
		if !ok || !strings.HasPrefix(part.name, "VPN: ") || result.count(ofScenario(scenarioVpnPairs)) == 0 {
			continue
		}
		policy := result.policyIndex(v10Policy(lagdrift.EarlyK).name)
		last := result.runs() - 1
		pairs := []string{}
		for _, pair := range vpnPairs {
			name := pair.cityName + "/" + strings.ToUpper(pair.code)
			filter := func(s *visitorSummary) bool { return s.scenario == scenarioVpnPairs && s.claimName == name }
			pairs = append(pairs, fmt.Sprintf("%s %s/%s", name,
				result.rateOf(filter, policy, 0, verdictIs(1, lagdrift.VerdictDisagree)).short(),
				result.rateOf(filter, policy, last, verdictIs(1, lagdrift.VerdictDisagree)).short()))
		}
		vpnLines = append(vpnLines, fmt.Sprintf("- %s: rejected after 1/%d runs: %s; tiny-country false rejects %s/%s.",
			strings.TrimPrefix(part.name, "VPN: "), last+1, strings.Join(pairs, ", "),
			result.rateOf(ofScenario(scenarioTiny), policy, 0, verdictIs(1, lagdrift.VerdictDisagree)).short(),
			result.rateOf(ofScenario(scenarioTiny), policy, last, verdictIs(1, lagdrift.VerdictDisagree)).short()))
	}
	if 0 < len(vpnLines) {
		b.WriteString("**A visitor far from the address's place (VPN)**, at 99%:\n\n" + strings.Join(vpnLines, "\n") + "\n\n")
	}
	interruptedLines := []string{}
	for _, part := range parts {
		result, ok := results[part.name]
		if !ok || !strings.HasPrefix(part.name, "interrupted: ") {
			continue
		}
		for p, policy := range result.config.policies {
			if !policy.attack {
				continue
			}
			interruptedLines = append(interruptedLines, fmt.Sprintf("- %s: the silent client is agreed with in %s of 300 km claims and %s of 1,000 km ones; an honest client's runs that don't count: %s.",
				strings.TrimPrefix(part.name, "interrupted: "),
				result.rateOf(ofScenario(scenarioClaim300), p, 0, verdictIs(1, lagdrift.VerdictAgree)).short(),
				result.rateOf(ofScenario(scenarioClaim1000), p, 0, verdictIs(1, lagdrift.VerdictAgree)).short(),
				result.rateOf(func(*visitorSummary) bool { return true }, result.policyIndex(v10Policy(lagdrift.EarlyK).name), 0, func(r runSummary) bool { return !r.counted }).short()))
		}
	}
	if 0 < len(interruptedLines) {
		b.WriteString("**The interrupted stop** (one run, 99%):\n\n" + strings.Join(interruptedLines, "\n") + "\n\n")
	}
	b.WriteString("**The fit** (the checks table): ")
	refits, moved, grids, lower := 0, 0, 0, 0
	for _, part := range parts {
		if result, ok := results[part.name]; ok {
			refits += result.fitChecks.refits
			moved += result.fitChecks.refitMoved
			grids += result.fitChecks.grids
			lower += result.fitChecks.gridLowerBy1
		}
	}
	fmt.Fprintf(&b, "%s of %s warm refits of a verdict's fit moved it 1 km or more, and %d of %s grids of starts found a fixed point with a χ² lower by more than 1.",
		rate{k: moved, n: refits}.short(), formatThousands(float64(refits)), lower, formatThousands(float64(grids)))
	gridNearer := 0
	for _, part := range parts {
		if result, ok := results[part.name]; ok {
			gridNearer += result.fitChecks.gridLowerNearer
		}
	}
	if 0 < lower {
		fmt.Fprintf(&b, " Of those, %d lie nearer the truth than the fit.", gridNearer)
	}
	b.WriteString(" One at a time, by CPU time:")
	for kind, sorted := range times {
		if 0 < len(sorted) {
			fmt.Fprintf(&b, " %s, p50 %.2f ms and p95 %.2f ms;", fitKindNames[kind], float64(durationQuantile(sorted, 0.5).Microseconds())/1000, float64(durationQuantile(sorted, 0.95).Microseconds())/1000)
		}
	}
	b.WriteString(" against V10's budget of a warm refit at p95 ≤ 1 ms and a full fit at p95 ≤ 10 ms.\n\n")
	return b.String()
}
