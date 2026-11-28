package lagdrift

// The verdict over a fit (lag.dev/DRIFTTEST.md V6, V5): the part of the
// verdict that is geometry, shared by the api and the demo token's local
// verdict (W8). The api decides the run-level reasons before it, those V6
// lists first and P4 names (ip_mismatch, too_few_extenders,
// ip_family_mismatch, reports_missing), since they need no fit.

// One of P4's verdicts.
type Verdict string

const (
	VerdictAgree        Verdict = "agree"
	VerdictDisagree     Verdict = "disagree"
	VerdictInconclusive Verdict = "inconclusive"
)

// One of P4's reasons.
type Reason string

const (
	ReasonInsideIpCountry  Reason = "inside_ip_country"
	ReasonRegionOverlaps   Reason = "region_overlaps"
	ReasonRegionOutside    Reason = "region_outside"
	ReasonInconsistent     Reason = "inconsistent"
	ReasonLowPrecision     Reason = "low_precision"
	ReasonIpCountryUnknown Reason = "ip_country_unknown"
)

// The default of lag.yml's max_sigma_km: a fit whose σ_major is larger is too
// imprecise to reject the visitor (V6), km. It never stops an agree: a wide
// region that touches the IP country agrees however wide it is (the user,
// 2026-11-20).
const DefaultMaxSigmaKm = 1500

// The verdict of a fit against the IP country c (V6) with the region's k
// (V11, as Judge takes it), and obs the fit's observations for the physical
// check (V5):
//
//  1. c unknown (nil, or without a ring with a point) → inconclusive,
//     ip_country_unknown;
//  2. no fit → inconclusive, low_precision;
//  3. the region touches c but c is physically impossible → inconclusive,
//     inconsistent. The check never rejects on its own (V5);
//  4. p̂ in c → agree, inside_ip_country;
//  5. the region touches c → agree, region_overlaps;
//  6. the region is outside c but σ_major is above maxSigmaKm →
//     inconclusive, low_precision: bounds rejection only, never acceptance;
//  7. otherwise → disagree, region_outside.
//
// The judgement is zero for the first two.
func Decide(f *Fit, c *Country, obs []Observation, k float64, maxSigmaKm float64) (Verdict, Reason, Judgement) {
	if c == nil || !c.hasBoundary() {
		return VerdictInconclusive, ReasonIpCountryUnknown, Judgement{}
	}
	if f == nil {
		return VerdictInconclusive, ReasonLowPrecision, Judgement{}
	}
	j := Judge(f, c, k)
	switch {
	case j.Overlaps && !PhysicallyPossible(obs, c):
		return VerdictInconclusive, ReasonInconsistent, j
	case j.Inside:
		return VerdictAgree, ReasonInsideIpCountry, j
	case j.Overlaps:
		return VerdictAgree, ReasonRegionOverlaps, j
	case !(f.MajorKm <= maxSigmaKm):
		return VerdictInconclusive, ReasonLowPrecision, j
	default:
		return VerdictDisagree, ReasonRegionOutside, j
	}
}
