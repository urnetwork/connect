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
	// choiceHubs takes at most this many hub extenders
	maxHubs = 4
	// choiceJoint: an extender nearer than this to the address's place covers
	// no direction
	directionMinKm  = 100
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

// E2 and the variants the study compares with it, for a visitor far from
// the address's place (a VPN):
//
//   - choiceE2: E2 as it is;
//   - choiceJoint: the mid and far bands spread by bearing together with the
//     near band, so they cover the directions the near extenders don't;
//   - choiceHubs: after the near band, an extender at each of the VPN hubs
//     nearest the address's place but more than 300 km from it (up to 4), the
//     rest from the mid and far bands;
//   - choiceInformation: after the near band, greedily the extender that most
//     shrinks the region, by the model's variances, averaged over plausible
//     places for the visitor: the address's place and the VPN hubs;
//   - choiceInformationRing: the same, with the plausible places the address's
//     place and a ring of 12 places at each of 800 and 2,000 km around it, so
//     the region shrinks wherever near the address the visitor may be.
const (
	choiceE2              = "E2"
	choiceJoint           = "joint spread"
	choiceHubs            = "VPN hubs"
	choiceInformation     = "information"
	choiceInformationRing = "information ring"
)

// How to choose.
type choiceOptions struct {
	mode string
	// choiceHubs: the candidate nearest each hub, in the order to take them
	hubIndexes []int
	// choiceInformation: a set's score, lower better
	score func(picked []choiceCandidate) float64
}

// The choice from candidates, every one serving now. r breaks ties.
func chooseExtenders(candidates []choiceCandidate, r *rand.Rand) choice {
	return chooseExtendersBy(candidates, choiceOptions{mode: choiceE2}, r)
}

// The choice from candidates by a mode. r breaks ties.
func chooseExtendersBy(candidates []choiceCandidate, options choiceOptions, r *rand.Rand) choice {
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
	near := pickNear(nearPool, ordered)
	for _, c := range near {
		take(c)
	}
	chosen := slices.Clone(near)

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
	bands := func(midWant int, farWant int) {
		short := 0
		for i, band := range [][2]float64{{midMinKm, midMaxKm}, {midMaxKm, farMaxKm}, {farMaxKm, math.Inf(1)}} {
			want := []int{midWant, farWant, 0}[i] + short
			var picked []choiceCandidate
			if options.mode == choiceJoint {
				picked = pickSpreadAround(inBand(band[0], band[1]), chosen, want)
			} else {
				picked = pickSpread(inBand(band[0], band[1]), want)
			}
			for _, c := range picked {
				take(c)
				chosen = append(chosen, c)
			}
			short = want - len(picked)
		}
	}
	switch options.mode {
	case choiceHubs:
		hubs := 0
		for _, index := range options.hubIndexes {
			if maxHubs <= hubs {
				break
			}
			for _, c := range ordered {
				if c.index == index && !taken[c.index] && midMinKm <= c.distanceKm {
					take(c)
					chosen = append(chosen, c)
					hubs += 1
				}
			}
		}
		rest := runExtenders - len(primaries)
		bands((rest+1)/2, rest/2)
	case choiceInformation, choiceInformationRing:
		for _, unusedOnly := range []bool{true, false} {
			for len(primaries) < runExtenders {
				best := -1
				bestScore := math.Inf(1)
				for i, c := range ordered {
					if taken[c.index] || (unusedOnly && c.used) {
						continue
					}
					if score := options.score(append(slices.Clone(chosen), c)); score < bestScore {
						best = i
						bestScore = score
					}
				}
				if best < 0 {
					break
				}
				take(ordered[best])
				chosen = append(chosen, ordered[best])
			}
		}
	default:
		bands(bandCount, bandCount)
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

// want candidates from band whose bearings, with those of fixed, have the
// largest smallest gap, unused first; fixed stay as they are.
func pickSpreadAround(band []choiceCandidate, fixed []choiceCandidate, want int) []choiceCandidate {
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
	// a direction is covered by an extender far enough away for its bearing
	// to mean one, once however many share it
	directions := []choiceCandidate{}
	for _, c := range fixed {
		if c.distanceKm < directionMinKm {
			continue
		}
		duplicate := false
		for _, d := range directions {
			if math.Abs(math.Remainder(c.bearingDeg-d.bearingDeg, 360)) < 1 {
				duplicate = true
			}
		}
		if !duplicate {
			directions = append(directions, c)
		}
	}
	picked := spreadFrom(unused, directions, len(directions)+want)
	if len(picked) < len(directions)+want {
		picked = spreadFrom(used, picked, len(directions)+want)
	}
	return picked[len(directions):]
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
