package lagdrift

// Calibrated k (lag.dev/DRIFTTEST.md V11, the user's decision of 2026-11-17:
// "drop 99.9%, calibrate"). T3 found the textbook region, KForConfidence, too
// narrow in a thin tail of fits: about right at 99% outside Oceania, far too
// narrow around New Zealand, and wrong 5–10 times too often at 99.9%, which
// the user dropped. A run's k is the larger of the textbook k and a
// calibrated k, the empirical quantile V4 measures per continent from lag.dev
// tracks; a continent without enough tracks uses DefaultK, T3's per-region k.
// The clear stop's k, KEarly, scales with k, so a clear stop is never easier
// than the verdict.

import (
	"math"
	"slices"
	"strings"
)

// k_early's scale: EarlyK over the textbook 99% k, 4.0 / 3.035 ≈ 1.32 (V11).
const KEarlyScale = 4.0 / 3.035

// GeoLite2's continent codes.
var continentCodes = []string{"AF", "AN", "AS", "EU", "NA", "OC", "SA"}

// V11's defaults where T3 found the textbook k too narrow, by continent code,
// "" for an unknown continent. A continent not listed keeps the textbook k,
// which V11 writes as 2.448 at 95% and 3.035 at 99%.
var (
	defaultK95 = map[string]float64{"AF": 2.50, "OC": 2.79}
	defaultK99 = map[string]float64{"NA": 3.13, "SA": 3.50, "AF": 3.71, "OC": 7.88, "AN": 3.49, "": 3.49}
)

// V11's default k at a confidence for a continent, never below the textbook
// KForConfidence. The continent is GeoLite2's two-letter code (AF, AN, AS, EU,
// NA, OC, SA) in either case; anything else is unknown.
//
//   - 95%: AF 2.50, OC 2.79, every other continent textbook (2.448);
//   - 99%: NA 3.13, SA 3.50, AF 3.71, OC 7.88, AN and unknown 3.49, EU and AS
//     textbook (3.035).
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

// The clear stop's k for a run's k (V11): max(EarlyK, 1.32 × k), with 1.32 =
// KEarlyScale, so a clear stop is never easier than the verdict. At the
// textbook 99% k it is EarlyK.
func KEarly(k float64) float64 {
	return math.Max(EarlyK, KEarlyScale*k)
}
