package lagdrift

import (
	"fmt"
	"testing"
)

// An observation whose lowest round trip is minMs.
func physicalObservation(id string, at Point, minMs float64) Observation {
	return Observation{
		ExtenderId:   id,
		At:           at,
		PlaceErrorKm: 10,
		Run:          0,
		RoundTrip:    RoundTrip{Ms: minMs + 1, SigmaMs: 0.2, Count: 40, MinMs: minMs},
	}
}

// Round trips from a visitor at truth, no faster than light in fiber.
func honestObservations(truth Point, extenders []Point) []Observation {
	obs := []Observation{}
	for i, at := range extenders {
		ms := 2 + MsPerKm*DistanceKm(truth, at)
		obs = append(obs, physicalObservation(fmt.Sprintf("e%d", i), at, ms))
	}
	return obs
}

var physicalExtenders = []Point{
	{Lat: 1, Lon: 3}, {Lat: 3, Lon: 1}, {Lat: -1, Lon: 1}, {Lat: 1, Lon: -1},
	{Lat: 10, Lon: 10}, {Lat: -10, Lon: 10}, {Lat: 10, Lon: -10}, {Lat: -10, Lon: -10},
}

func TestPhysicallyPossibleHonestRoundTrips(t *testing.T) {
	obs := honestObservations(Point{Lat: 1, Lon: 1}, physicalExtenders)
	if !PhysicallyPossible(obs, squareCountry) {
		t.Error("honest round trips from inside the country")
	}
	// a country 20° away is out of reach of the near extenders
	far := &Country{Code: "zx", Polygons: [][][]Point{{boxRing(20, 20, 22, 22)}}}
	if PhysicallyPossible(obs, far) {
		t.Error("a country 3,000 km beyond 2 ms round trips")
	}
	if !PhysicallyPossible(obs, nil) || !PhysicallyPossible(nil, far) {
		t.Error("no country, or no observations, is always possible")
	}
}

// One misplaced extender or broken path can't make the measurement
// contradict itself; two can.
func TestPhysicallyPossibleLeavesOneOut(t *testing.T) {
	obs := honestObservations(Point{Lat: 1, Lon: 1}, physicalExtenders)
	// an extender listed 25° from where it is answers in 2 ms: its disc
	// misses the country
	one := append([]Observation{}, obs...)
	one[4] = physicalObservation("e4", Point{Lat: 30, Lon: 30}, 2)
	if !PhysicallyPossible(one, squareCountry) {
		t.Error("one bad disc dropped")
	}
	two := append([]Observation{}, one...)
	two[5] = physicalObservation("e5", Point{Lat: -30, Lon: 30}, 2)
	if PhysicallyPossible(two, squareCountry) {
		t.Error("two bad discs")
	}
}

// The discs can overlap entirely inside a large country, away from its
// boundary: then a candidate inside the overlap decides, not the boundary.
func TestPhysicallyPossibleInsideALargeCountry(t *testing.T) {
	large := &Country{Code: "zl", Polygons: [][][]Point{{boxRing(-20, -20, 20, 20)}}}
	near := []Point{{Lat: 0.5, Lon: 0}, {Lat: -0.5, Lon: 0}, {Lat: 0, Lon: 0.5}, {Lat: 0, Lon: -0.5}}
	obs := []Observation{}
	for i, at := range near {
		// 1 ms reaches 160 km: nowhere near the boundary, 2,000 km away
		obs = append(obs, physicalObservation(fmt.Sprintf("n%d", i), at, 1))
	}
	if !PhysicallyPossible(obs, large) {
		t.Error("discs overlapping in the middle of a large country")
	}
	// the same discs against a country 550 km away, which they don't reach
	beyond := &Country{Code: "zb", Polygons: [][][]Point{{boxRing(5, 5, 7, 7)}}}
	if PhysicallyPossible(obs, beyond) {
		t.Error("discs of 160 km against a country 550 km away")
	}
}

// The lowest round trip in any run is the one that counts, and an extender's
// runs are one disc.
func TestPhysicallyPossibleUsesTheLowestRoundTrip(t *testing.T) {
	obs := honestObservations(Point{Lat: 1, Lon: 1}, physicalExtenders)
	far := &Country{Code: "zx", Polygons: [][][]Point{{boxRing(-30, 40, -28, 42)}}}
	// slow runs reach the far country
	slow := []Observation{}
	for _, o := range obs {
		o.Run = 1
		o.RoundTrip.MinMs += 200
		o.RoundTrip.Ms += 200
		slow = append(slow, o)
	}
	if !PhysicallyPossible(slow, far) {
		t.Error("200 ms round trips reach 4,000 km")
	}
	// the fast run's discs decide
	if PhysicallyPossible(append(slow, obs...), far) {
		t.Error("the fast run's discs don't reach")
	}
	// without MinMs, the estimate stands in
	for i := range obs {
		obs[i].RoundTrip.MinMs = 0
	}
	if PhysicallyPossible(obs, far) {
		t.Error("estimates alone")
	}
}
