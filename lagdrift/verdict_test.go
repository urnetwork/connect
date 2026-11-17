package lagdrift

import (
	"testing"
)

// V6's order: unknown country and low precision first, then the physical
// check where the region touches the country, then the region.
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
		{name: "too wide", fit: fitAt(Point{Lat: 1, Lon: 1}, circleCov(2000)), c: squareCountry, obs: honest, verdict: VerdictInconclusive, reason: ReasonLowPrecision},
		{name: "inside", fit: fitAt(Point{Lat: 1, Lon: 1}, circleCov(30)), c: squareCountry, obs: honest, verdict: VerdictAgree, reason: ReasonInsideIpCountry},
		{name: "overlapping", fit: fitAt(Point{Lat: 1, Lon: -0.5}, circleCov(30)), c: squareCountry, obs: honest, verdict: VerdictAgree, reason: ReasonRegionOverlaps},
		{name: "outside", fit: fitAt(Point{Lat: 1, Lon: -3}, circleCov(30)), c: squareCountry, obs: honest, verdict: VerdictDisagree, reason: ReasonRegionOutside},
		{name: "inconsistent", fit: fitAt(Point{Lat: 1, Lon: 1}, circleCov(30)), c: squareCountry, obs: impossible, verdict: VerdictInconclusive, reason: ReasonInconsistent},
		// the physical check never rejects on its own, nor stops a rejection
		{name: "outside and impossible", fit: fitAt(Point{Lat: 1, Lon: -3}, circleCov(30)), c: squareCountry, obs: impossible, verdict: VerdictDisagree, reason: ReasonRegionOutside},
	}
	for _, c := range cases {
		verdict, reason, _ := Decide(c.fit, c.c, c.obs, DefaultConfidence, DefaultMaxSigmaKm)
		if verdict != c.verdict || reason != c.reason {
			t.Errorf("%s: %s/%s, not %s/%s", c.name, verdict, reason, c.verdict, c.reason)
		}
	}
}
