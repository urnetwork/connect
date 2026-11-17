package lagdriftsim

// The server's choice of a run's extenders (lag.dev/DRIFTTEST.md E2), from the
// extenders serving now, around the address's place:
//
//   - near: the 4 nearest, preferring 2 inside the IP country;
//   - mid: 4 between 300 and 2,500 km, spread by bearing (the largest
//     smallest angular gap);
//   - far: 4 between 2,500 and 9,000 km, spread the same way;
//   - spares: up to 4 more, used only when a session fails to open in 4 s.
//
// A retest prefers, within each band, extenders the token's earlier runs
// haven't used, and falls back to repeats where a band has no others. The
// near band's candidates on a retest are the 8 nearest, so a retest's near
// extenders stay near. E2 doesn't say what fills a band that is short, so the
// study fills mid's shortfall from far, far's from beyond 9,000 km, and the
// rest from the nearest left: a run has 12 extenders whenever 12 serve.

import (
	"math"
	"math/rand/v2"
	"slices"
)

const (
	nearCount       = 4
	nearInCountry   = 2
	nearRetestPool  = 8
	bandCount       = 4
	midMinKm        = 300
	midMaxKm        = 2500
	farMaxKm        = 9000
	runExtenders    = 12
	maxSpares       = 4
	swapImprovement = 1e-9
)

// An extender the choice can take.
type choiceCandidate struct {
	// the pool's index
	index      int
	distanceKm float64
	bearingDeg float64
	inCountry  bool
	used       bool
}

// A run's extenders: 12 to open, and the spares in the order they replace a
// session that fails, as pool indexes.
type choice struct {
	primaries []int
	spares    []int
}

// The choice from candidates, every one serving now. r breaks ties.
func chooseExtenders(candidates []choiceCandidate, r *rand.Rand) choice {
	// nearest first; a shuffle first makes ties fall by r
	ordered := slices.Clone(candidates)
	r.Shuffle(len(ordered), func(i int, j int) {
		ordered[i], ordered[j] = ordered[j], ordered[i]
	})
	slices.SortStableFunc(ordered, func(a choiceCandidate, b choiceCandidate) int {
		return compareFloat(a.distanceKm, b.distanceKm)
	})
	taken := map[int]bool{}
	primaries := []int{}
	take := func(c choiceCandidate) {
		taken[c.index] = true
		primaries = append(primaries, c.index)
	}

	// near
	nearPool := ordered[:min(len(ordered), nearRetestPool)]
	for _, c := range pickNear(nearPool, ordered) {
		take(c)
	}

	// mid, then far, each spread by bearing
	inBand := func(lo float64, hi float64) []choiceCandidate {
		band := []choiceCandidate{}
		for _, c := range ordered {
			if !taken[c.index] && lo <= c.distanceKm && c.distanceKm < hi {
				band = append(band, c)
			}
		}
		return band
	}
	short := 0
	for _, band := range [][2]float64{{midMinKm, midMaxKm}, {midMaxKm, farMaxKm}, {farMaxKm, math.Inf(1)}} {
		want := bandCount + short
		if band[0] == farMaxKm {
			want = short
		}
		picked := pickSpread(inBand(band[0], band[1]), want)
		for _, c := range picked {
			take(c)
		}
		short = want - len(picked)
	}
	// whatever is still short: the nearest left
	for _, c := range ordered {
		if runExtenders <= len(primaries) {
			break
		}
		if !taken[c.index] {
			take(c)
		}
	}

	// spares: one near, one mid, one far, one near, from what is left
	spares := []int{}
	for _, band := range [][2]float64{{0, midMinKm}, {midMinKm, midMaxKm}, {midMaxKm, farMaxKm}, {0, math.Inf(1)}} {
		if maxSpares <= len(spares) {
			break
		}
		for _, c := range ordered {
			if !taken[c.index] && band[0] <= c.distanceKm && c.distanceKm < band[1] {
				taken[c.index] = true
				spares = append(spares, c.index)
				break
			}
		}
	}
	return choice{primaries: primaries, spares: spares}
}

// The near band: from the 8 nearest, unused first, up to 2 inside the IP
// country, then the nearest. ordered is every candidate, nearest first, for a
// band that is still short.
func pickNear(nearPool []choiceCandidate, ordered []choiceCandidate) []choiceCandidate {
	picked := []choiceCandidate{}
	pickedIndexes := map[int]bool{}
	add := func(c choiceCandidate) {
		picked = append(picked, c)
		pickedIndexes[c.index] = true
	}
	for _, unusedOnly := range []bool{true, false} {
		inCountry := 0
		for _, c := range picked {
			if c.inCountry {
				inCountry += 1
			}
		}
		for _, c := range ordered {
			if nearInCountry <= inCountry || nearCount <= len(picked) {
				break
			}
			if c.inCountry && !pickedIndexes[c.index] && (!unusedOnly || !c.used) {
				add(c)
				inCountry += 1
			}
		}
		for _, c := range nearPool {
			if nearCount <= len(picked) {
				break
			}
			if !pickedIndexes[c.index] && (!unusedOnly || !c.used) {
				add(c)
			}
		}
	}
	for _, c := range ordered {
		if nearCount <= len(picked) {
			break
		}
		if !pickedIndexes[c.index] {
			add(c)
		}
	}
	return picked
}

// want candidates from band whose bearings have the largest smallest gap,
// unused first: greedy farthest-point picking from the nearest, then swaps
// until none improves. Used candidates fill only what unused ones can't.
func pickSpread(band []choiceCandidate, want int) []choiceCandidate {
	if want <= 0 || len(band) == 0 {
		return nil
	}
	unused := []choiceCandidate{}
	used := []choiceCandidate{}
	for _, c := range band {
		if c.used {
			used = append(used, c)
		} else {
			unused = append(unused, c)
		}
	}
	picked := spreadFrom(unused, nil, want)
	if len(picked) < want {
		picked = spreadFrom(used, picked, want)
	}
	return picked
}

// Adds to picked, up to want in all, the candidates that keep the bearings'
// smallest gap largest. Only the added ones are swapped.
func spreadFrom(candidates []choiceCandidate, picked []choiceCandidate, want int) []choiceCandidate {
	fixed := len(picked)
	pool := slices.Clone(candidates)
	if len(picked) < want && 0 < len(pool) && len(picked) == 0 {
		picked = append(picked, pool[0])
		pool = pool[1:]
	}
	for len(picked) < want && 0 < len(pool) {
		best := -1
		bestGap := -1.0
		for i, c := range pool {
			gap := smallestGap(append(slices.Clone(picked), c))
			if bestGap < gap {
				best = i
				bestGap = gap
			}
		}
		picked = append(picked, pool[best])
		pool = slices.Delete(pool, best, best+1)
	}
	// swaps: an added pick for one left over, while the smallest gap grows
	for improved := true; improved; {
		improved = false
		current := smallestGap(picked)
		for i := fixed; i < len(picked); i++ {
			for j, c := range pool {
				trial := slices.Clone(picked)
				trial[i] = c
				if gap := smallestGap(trial); current+swapImprovement < gap {
					pool[j] = picked[i]
					picked = trial
					current = gap
					improved = true
				}
			}
		}
	}
	return picked
}

// The smallest angular gap between the bearings, around the circle, degrees.
// One bearing has a gap of 360.
func smallestGap(cs []choiceCandidate) float64 {
	if len(cs) <= 1 {
		return 360
	}
	bearings := make([]float64, len(cs))
	for i, c := range cs {
		bearings[i] = c.bearingDeg
	}
	slices.Sort(bearings)
	gap := 360 - bearings[len(bearings)-1] + bearings[0]
	for i := 1; i < len(bearings); i++ {
		gap = math.Min(gap, bearings[i]-bearings[i-1])
	}
	return gap
}

func compareFloat(a float64, b float64) int {
	switch {
	case a < b:
		return -1
	case b < a:
		return 1
	default:
		return 0
	}
}
