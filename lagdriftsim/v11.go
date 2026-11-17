package lagdriftsim

// V11's calibrated k (lag.dev/DRIFTTEST.md V11) and whether a k calibrated on
// lag.dev's own tracks holds on drift verdicts (review V-8).

import (
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/urnetwork/connect/lagdrift"
)

// The server's histogram of k: bins 0.05 wide from 1.50 to 12.00, below and
// above which a bin each.
const (
	v11KLowestEdge  = 1.50
	v11KHighestEdge = 12.00
	v11KBinWidth    = 0.05
)

// V11's continents, in the report's order.
var v11Continents = []string{"EU", "NA", "AS", "SA", "AF", "OC"}

// V11's upper bound of the q quantile of ms: the value at rank
// n·q + 1.645·√(n·q·(1 − q)), rounded up to its bin's upper edge. ok is false
// when it lands in the overflow bin, or past the sample.
func v11UpperBound(ms []float64, q float64) (float64, bool) {
	n := float64(len(ms))
	if n == 0 {
		return math.NaN(), false
	}
	sorted := slices.Clone(ms)
	slices.Sort(sorted)
	rank := int(math.Ceil(n*q + 1.645*math.Sqrt(n*q*(1-q))))
	if len(sorted) < rank {
		return math.NaN(), false
	}
	v := sorted[max(1, rank)-1]
	if v < v11KLowestEdge {
		return v11KLowestEdge, true
	}
	if v11KHighestEdge <= v {
		return math.NaN(), false
	}
	bin := math.Floor((v-v11KLowestEdge)/v11KBinWidth+1e-9) + 1
	return math.Round((v11KLowestEdge+bin*v11KBinWidth)*1000) / 1000, true
}

// The lag tracks' city-point distances per continent, light-impossible ones
// left out, and the count left out.
func v11LagMs(lag *studyResult) (map[string][]float64, map[string]int) {
	ms := map[string][]float64{}
	impossible := map[string]int{}
	for _, s := range lag.summaries {
		r := s.runs[0][0]
		if !r.fitted {
			continue
		}
		if r.cityImpossible {
			impossible[s.continent] += 1
			continue
		}
		ms[s.continent] = append(ms[s.continent], r.cityM)
	}
	return ms, impossible
}

// The k at 99% per continent calibrated from lag tracks by V11: the upper
// bound, never below the textbook k; a continent whose bound overflows keeps
// its default.
func v11CalibratedKs(lag *studyResult) map[string]float64 {
	ks := map[string]float64{}
	ms, _ := v11LagMs(lag)
	for continent, values := range ms {
		k, ok := v11UpperBound(values, lagdrift.DefaultConfidence)
		if !ok {
			k = lagdrift.DefaultK(lagdrift.DefaultConfidence, continent)
		}
		ks[continent] = math.Max(lagdrift.KForConfidence(lagdrift.DefaultConfidence), k)
	}
	return ks
}

// Per continent: the default k, the lag tracks and the k calibrated from
// them, and the k the drift verdicts would need to miss the truth 1% of the
// time after 1, 2 and 3 runs.
func v11KTable(lag *studyResult, drift *studyResult) string {
	t := &table{header: []string{"Continent", "Default k", "Lag tracks", "left out: light", "lag 99% quantile", "Calibrated k (upper bound)", "Drift needs: 1 run", "2 runs", "3 runs"}}
	ms, impossible := v11LagMs(lag)
	calibrated := v11CalibratedKs(lag)
	for _, continent := range v11Continents {
		row := []string{continent, fmt.Sprintf("%.3f", lagdrift.DefaultK(lagdrift.DefaultConfidence, continent)), formatThousands(float64(len(ms[continent]))), fmt.Sprint(impossible[continent])}
		if values := ms[continent]; 0 < len(values) {
			sorted := slices.Clone(values)
			slices.Sort(sorted)
			row = append(row, fmt.Sprintf("%.2f", sorted[min(len(sorted)-1, int(math.Ceil(0.99*float64(len(sorted))))-1)]))
		} else {
			row = append(row, "–")
		}
		if k, ok := calibrated[continent]; ok {
			row = append(row, fmt.Sprintf("%.2f", k))
		} else {
			row = append(row, "–")
		}
		if drift != nil {
			for run := 0; run < drift.runs(); run++ {
				ks := drift.empiricalKs(func(s *visitorSummary) bool { return s.continent == continent }, 0, run)
				row = append(row, ks[1])
			}
		}
		t.add(row...)
	}
	return t.String()
}

// Drift verdicts at a part's V11 k, per continent: the region's misses of the
// truth and the false rejects, after 1, 2 and 3 runs.
func v11VerdictTable(drift *studyResult) string {
	t := &table{header: []string{"Continent", "k", "Region misses: 1 run", "2 runs", "3 runs", "False rejects, tiny: 1 run", "2 runs", "3 runs", "real countries: 1 run", "2 runs", "3 runs"}}
	for _, continent := range append(slices.Clone(v11Continents), "All") {
		filter := func(s *visitorSummary) bool { return continent == "All" || s.continent == continent }
		k := "per continent"
		if continent != "All" {
			k = fmt.Sprintf("%.2f", drift.config.kByContinent[continent])
		}
		row := []string{continent, k}
		for run := 0; run < drift.runs(); run++ {
			row = append(row, drift.rateOf(func(s *visitorSummary) bool {
				return filter(s) && s.scenario == scenarioTiny && s.runs[0][run].fitted
			}, 0, run, func(r runSummary) bool { return r.v11K < r.truthM }).short())
		}
		for _, sc := range []*scenario{scenarioTiny, scenarioRealHonest} {
			for run := 0; run < drift.runs(); run++ {
				row = append(row, drift.rateOf(func(s *visitorSummary) bool { return filter(s) && s.scenario == sc }, 0, run, func(r runSummary) bool {
					return r.v11Verdict == lagdrift.VerdictDisagree
				}).short())
			}
		}
		t.add(row...)
	}
	return t.String()
}

// The V11 section of the report, when its parts ran.
func writeV11(b *strings.Builder, results map[string]*studyResult) {
	lag, okLag := results["V11: lag.dev tracks"]
	if !okLag {
		return
	}
	defaults := results["V11 drift: default k"]
	calibrated := results["V11 drift: lag k"]
	fmt.Fprintf(b, "### V11's k on drift verdicts\n\n")
	fmt.Fprintf(b, "V11 calibrates k at 99%% per continent on single-run lag.dev tracks (lag.dev's own choice of 8 extenders, the fit measured from the visitor's city point, light-impossible tracks left out) by its upper-bound rule: the value at rank n·q + 1.645·√(n·q·(1 − q)), rounded up to its 0.05 bin's upper edge, never below the textbook 3.035. Drift verdicts use the hybrid choice of 12 and pool up to 3 runs. Does lag.dev's k hold there? \"Drift needs\" is the k at which the drift verdicts' region would miss the truth 1%% of the time, from the default-k part's tiny-country visitors.\n\n")
	b.WriteString(v11KTable(lag, defaults) + "\n")
	for _, named := range []struct {
		name   string
		result *studyResult
	}{{"V11's default k", defaults}, {"the k calibrated on lag.dev tracks", calibrated}} {
		if named.result == nil {
			continue
		}
		fmt.Fprintf(b, "Drift verdicts at %s (tiny-country visitors for the region's misses; k_early is KEarly(k)):\n\n", named.name)
		b.WriteString(v11VerdictTable(named.result) + "\n")
	}
}
