package lagdrift

// k per continent (lag.dev/DRIFTTEST.md V11, the user's decisions of
// 2026-11-17: "drop 99.9%", then "fixed defaults, alarm"). T3 found the
// textbook region, KForConfidence, too narrow in a thin tail of fits: about
// right at 99% outside Oceania, far too narrow around New Zealand, and wrong
// 5–10 times too often at 99.9%, which the user dropped. A run's k is a fixed
// table by confidence and continent, DefaultK, from T3's 99% quantiles on
// drift geometry and never below textbook. lag.dev's weekly measurement of
// its own tracks is an alarm that the table needs re-deriving, not a setting,
// since its 8-extender runs have much heavier tails than a drift run's. The
// clear stop's k, KEarly, scales with k, so a clear stop is never easier than
// the verdict, and a stop rule without it allows no clear stop.

import (
	"math"
	"slices"
	"strings"
)

// k_early's scale: EarlyK over the textbook 99% k, the exact ratio 4.0 / 3.035,
// about 1.318 (V11).
const KEarlyScale = 4.0 / 3.035

// GeoLite2's continent codes.
var continentCodes = []string{"AF", "AN", "AS", "EU", "NA", "OC", "SA"}

// V11's table where it is above the textbook k, by continent code, "" for an
// unknown continent. A continent not listed keeps the textbook k, which V11
// writes as 2.448 at 95% and 3.035 at 99% (EU).
var (
	defaultK95 = map[string]float64{"AF": 2.50, "OC": 2.79}
	defaultK99 = map[string]float64{"NA": 3.25, "SA": 3.50, "AF": 3.95, "OC": 7.88, "AS": 3.35, "AN": 3.49, "": 3.49}
)

// V11's k at a confidence for a continent, from its fixed table, never below
// the textbook KForConfidence. The continent is GeoLite2's two-letter code
// (AF, AN, AS, EU, NA, OC, SA) in either case; anything else is unknown.
//
//   - 95%: AF 2.50, OC 2.79, every other continent textbook (2.448);
//   - 99%: NA 3.25, SA 3.50, AF 3.95, OC 7.88, AS 3.35, AN and unknown 3.49,
//     EU textbook (3.035).
//
// V11 allows only 95% and 99%; any other confidence gets the textbook k.
func DefaultK(confidence float64, continent string) float64 {
	k := KForConfidence(confidence)
	var defaults map[string]float64
	switch confidence {
	case 0.95:
		defaults = defaultK95
	case 0.99:
		defaults = defaultK99
	default:
		return k
	}
	code := strings.ToUpper(continent)
	if !slices.Contains(continentCodes, code) {
		code = ""
	}
	return math.Max(k, defaults[code])
}

// The clear stop's k for a run's k (V11): max(EarlyK, KEarlyScale × k), so a
// clear stop is never easier than the verdict. At the textbook 99% k it is
// EarlyK. A stop tolerance needs it (StopTolerance.KEarly): without one, no
// clear stop holds.
func KEarly(k float64) float64 {
	return math.Max(EarlyK, KEarlyScale*k)
}
