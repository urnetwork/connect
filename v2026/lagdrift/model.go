package lagdrift

// The variance of each extender's term (lag.dev/DRIFTTEST.md V3):
//
//	σᵢ² = σ_meas,i² + σ₀² + τᵢ²,  τᵢ² = (κ · K · dᵢ)² + (K · s_ext,i)²
//
// σ_meas,i is the round trip's standard error (V1) and σ₀ a floor for the
// stack's own timing. τᵢ is the path's route and place error: real paths stray
// from the great circle by a share κ of the distance, and the extender's place
// is off by s_ext,i km. τᵢ belongs to the path, not the run, so the runs of one
// extender share it (V7).

import (
	"math"
)

const (
	// κ until V4 calibrates it
	DefaultKappa = 0.25
	// σ₀, ms
	DefaultSigma0Ms = 1.0

	// an extender's place error is GeoLite2's accuracy radius held to
	// [10, 100] km, or 50 km for a place derived another way (V3, A4)
	MinPlaceErrorKm     = 10
	MaxPlaceErrorKm     = 100
	DerivedPlaceErrorKm = 50
)

// The constants V4 calibrates (V3).
type Model struct {
	// the route error's share of the distance
	Kappa float64 `json:"kappa"`
	// the timing floor, ms
	Sigma0Ms float64 `json:"sigma0Ms"`
}

// κ = 0.25 and σ₀ = 1 ms, until V4 calibrates them.
func DefaultModel() Model {
	return Model{
		Kappa:    DefaultKappa,
		Sigma0Ms: DefaultSigma0Ms,
	}
}

// The place error the model uses for an extender, km: the given accuracy
// radius held to [10, 100], or 50 when it is unknown (not a positive finite
// number).
func HeldPlaceErrorKm(placeErrorKm float64) float64 {
	if !(0 < placeErrorKm) || math.IsInf(placeErrorKm, 1) {
		return DerivedPlaceErrorKm
	}
	return math.Max(MinPlaceErrorKm, math.Min(MaxPlaceErrorKm, placeErrorKm))
}

// τ, the path's route and place error, ms, for an extender d km away whose
// place is off by placeKm (already held).
func (self Model) pathSigmaMs(dKm float64, placeKm float64) float64 {
	route := self.Kappa * MsPerKm * dKm
	place := MsPerKm * placeKm
	return math.Sqrt(route*route + place*place)
}

// σ_meas² + σ₀², ms²: the variance a run's own measurement adds.
func (self Model) noiseVar(sigmaMs float64) float64 {
	return sigmaMs*sigmaMs + self.Sigma0Ms*self.Sigma0Ms
}
