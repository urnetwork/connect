package lagdrift

import (
	"testing"
)

// V6's order: unknown country and no fit first, then the physical check
// where the region touches the country, then the region. σ_major above
// max_sigma_km bounds rejection only: a wide region that touches the country
// agrees, and one clear of it is inconclusive, never a reject.
func TestDecide(t *testing.T) {
	honest := honestObservations(Point{Lat: 1, Lon: 1}, physicalExtenders)
	// two extenders whose round trips put the visitor near them, far away
	impossible := append([]Observation{}, honest...)
	impossible[4] = physicalObservation("e4", Point{Lat: 30, Lon: 30}, 2)
	impossible[5] = physicalObservation("e5", Point{Lat: -30, Lon: 30}, 2)
	cases := []struct {
		name    string
		fit     *Fit
		c       *Country
		obs     []Observation
		verdict Verdict
		reason  Reason
	}{
		{name: "no country", fit: fitAt(Point{Lat: 1, Lon: 1}, circleCov(30)), c: nil, obs: honest, verdict: VerdictInconclusive, reason: ReasonIpCountryUnknown},
		{name: "no fit", fit: nil, c: squareCountry, obs: honest, verdict: VerdictInconclusive, reason: ReasonLowPrecision},
		{name: "too wide, inside", fit: fitAt(Point{Lat: 1, Lon: 1}, circleCov(2000)), c: squareCountry, obs: honest, verdict: VerdictAgree, reason: ReasonInsideIpCountry},
		{name: "too wide, overlapping", fit: fitAt(Point{Lat: 1, Lon: -20}, circleCov(2000)), c: squareCountry, obs: honest, verdict: VerdictAgree, reason: ReasonRegionOverlaps},
		{name: "too wide, outside", fit: fitAt(Point{Lat: 1, Lon: -120}, circleCov(2000)), c: squareCountry, obs: honest, verdict: VerdictInconclusive, reason: ReasonLowPrecision},
		{name: "just precise enough, outside", fit: fitAt(Point{Lat: 1, Lon: -120}, circleCov(DefaultMaxSigmaKm)), c: squareCountry, obs: honest, verdict: VerdictDisagree, reason: ReasonRegionOutside},
		{name: "inside", fit: fitAt(Point{Lat: 1, Lon: 1}, circleCov(30)), c: squareCountry, obs: honest, verdict: VerdictAgree, reason: ReasonInsideIpCountry},
		{name: "overlapping", fit: fitAt(Point{Lat: 1, Lon: -0.5}, circleCov(30)), c: squareCountry, obs: honest, verdict: VerdictAgree, reason: ReasonRegionOverlaps},
		{name: "outside", fit: fitAt(Point{Lat: 1, Lon: -3}, circleCov(30)), c: squareCountry, obs: honest, verdict: VerdictDisagree, reason: ReasonRegionOutside},
		{name: "inconsistent", fit: fitAt(Point{Lat: 1, Lon: 1}, circleCov(30)), c: squareCountry, obs: impossible, verdict: VerdictInconclusive, reason: ReasonInconsistent},
		// the physical check never rejects on its own, nor stops a rejection
		{name: "outside and impossible", fit: fitAt(Point{Lat: 1, Lon: -3}, circleCov(30)), c: squareCountry, obs: impossible, verdict: VerdictDisagree, reason: ReasonRegionOutside},
	}
	for _, c := range cases {
		verdict, reason, _ := Decide(c.fit, c.c, c.obs, KForConfidence(DefaultConfidence), DefaultMaxSigmaKm)
		if verdict != c.verdict || reason != c.reason {
			t.Errorf("%s: %s/%s, not %s/%s", c.name, verdict, reason, c.verdict, c.reason)
		}
	}
}

// The verdict takes the run's k (V11): a region clear of the country at the
// textbook k can reach it at Oceania's.
func TestDecideAtTheRunsK(t *testing.T) {
	honest := honestObservations(Point{Lat: 1, Lon: 1}, physicalExtenders)
	// 1.5° west of the square, about 167 km, with σ 30 km: D ≈ 5.6
	f := fitAt(Point{Lat: 1, Lon: -1.5}, circleCov(30))
	for _, c := range []struct {
		k       float64
		verdict Verdict
		reason  Reason
	}{
		{k: KForConfidence(0.99), verdict: VerdictDisagree, reason: ReasonRegionOutside},
		{k: DefaultK(0.99, "NA"), verdict: VerdictDisagree, reason: ReasonRegionOutside},
		{k: DefaultK(0.99, "OC"), verdict: VerdictAgree, reason: ReasonRegionOverlaps},
	} {
		verdict, reason, j := Decide(f, squareCountry, honest, c.k, DefaultMaxSigmaKm)
		if verdict != c.verdict || reason != c.reason || j.K != c.k {
			t.Errorf("k %.3f: %s/%s %+v", c.k, verdict, reason, j)
		}
	}
}

// A country with no boundary to judge against is an unknown country, as nil
// is, not one the region lies wholly outside (review F-04): the shipped files
// can't hold one, but a caller can build one.
func TestDecideWithoutABoundary(t *testing.T) {
	honest := honestObservations(Point{Lat: 1, Lon: 1}, physicalExtenders)
	for _, c := range []*Country{
		{Code: "zn"},
		{Code: "zn", Polygons: [][][]Point{}},
		{Code: "zn", Polygons: [][][]Point{{}}},
		{Code: "zn", Polygons: [][][]Point{{{}}, {{}, {}}}},
	} {
		// wide too: an unknown country comes before low precision
		for _, f := range []*Fit{fitAt(Point{Lat: 1, Lon: 1}, circleCov(30)), fitAt(Point{Lat: 1, Lon: 1}, circleCov(2000))} {
			verdict, reason, j := Decide(f, c, honest, KForConfidence(DefaultConfidence), DefaultMaxSigmaKm)
			if verdict != VerdictInconclusive || reason != ReasonIpCountryUnknown || j != (Judgement{}) {
				t.Errorf("%+v: %s/%s %+v", c.Polygons, verdict, reason, j)
			}
		}
	}
}
