package lagdrift

// The fit (lag.dev/DRIFTTEST.md V2, V5, V7): one position p for every run, a
// last-mile offset b_r ≥ 0 per run, and a route and place error uᵢ shared by
// every run that used extender i:
//
//	y_{r,i} = b_r + K · d(p, eᵢ) + uᵢ + ε_{r,i}
//
// uᵢ ~ N(0, τᵢ²) and ε_{r,i} ~ N(0, σ_meas,r,i² + σ₀²) (V3). Generalized least
// squares minimizes rᵀV⁻¹r, where V is block-diagonal by extender: each block
// is the diagonal of its runs' ε variances, D, plus τᵢ² everywhere. A block is
// D + t tᵀ with t_j the τ of its j-th row, so Sherman–Morrison inverts it in
// closed form, whatever its size:
//
//	V⁻¹ = D⁻¹ − D⁻¹ t tᵀ D⁻¹ / (1 + Q),  Q = tᵀ D⁻¹ t
//
// Huber weights (c = 2) apply per extender, to the standardized residual of
// what its runs share: z = tᵀD⁻¹r / √(Q (1 + Q)), which is N(0, 1) under the
// model for a block of any size and is r / σ for a single run (V5). A detoured
// path, or an extender in the wrong place, moves every run's residual the same
// way, which is z's direction, so it is discounted in every run at once (V7).
// The rest of the block's residual, ‖r⊥‖² = rᵀV⁻¹r − z², is how its runs
// differ from each other, which is measurement noise, and keeps its full
// weight. So the fit minimizes
//
//	F = Σ_blocks 2ρ_H(z) + ‖r⊥‖²
//
// with ρ_H(z) = z²/2 within c and c|z| − c²/2 beyond, which is rᵀV⁻¹r with
// every z within c, and Σ (r/σ)² Huberized for single runs.
//
// The predicted distance is smoothed within 10 km of an extender
// (smoothedDistanceKm), so the model is differentiable at an extender's place.
//
// The solver steps as driftFit.ts's does, on local east and north km, with its
// Levenberg–Marquardt damping made adaptive: λ times JᵀWJ's position diagonal
// is added to the step's matrix, λ starting at driftFit.ts's 0.001, and a step
// is taken only when it lowers F. λ follows Nielsen's rule: after a step
// taken, it is multiplied by max(1/3, 1 − (2γ − 1)³), γ the gain in F over the
// quadratic model's, and never falls below 0.001; after a step refused, it is
// multiplied by ν, which starts at 2 and doubles with each refusal in a row.
// The position step is capped at 400 km. The Huber weights are recomputed at
// every step. An offset at its bound of 0 is held there (an active set). With
// driftFit.ts's fixed damping alone, a step against a gross outlier can
// overshoot a direction the other extenders hardly constrain and circle there
// for good.
//
// Starts and convergence (V5, 2026-11-17): the fit starts from the expected
// point, the places of the three fastest extenders and the warm fit, if any.
// It iterates each start until a step lowers F by less than 10⁻⁶ of F and the
// undamped step from there would move the position less than 0.1 km, or no
// step lowers F, within 100 trial steps, and keeps the lowest F. It leaves the
// warm fit's basin only for an F lower by more than 1, so the radar's shadow
// doesn't flip between near-equal fits.
//
// lag.dev/DRIFTTEST.md V3 recomputes the variances, whose route error grows
// with dᵢ, at every iteration. Here they are recomputed at every pass
// (feasible GLS in two steps), and the covariance measures them from the fit:
//
//  1. the first pass measures each dᵢ from the expected place and runs the
//     cold starts, the expected point and the three fastest extenders, so it
//     depends on the data alone;
//  2. the second measures each dᵢ from the first pass's fit and runs from that
//     fit and from the warm fit, which wins unless the other's F is lower by
//     more than 1.
//
// Recomputed at every iteration, the variances make the fit the fixed point of
// a reweighting that moves with the position, and in about 1 fit in 20 of
// E2's geometry (by simulation) that point repels the iteration, which then
// circles it without settling from any start. With the variances fixed for a
// pass, F is one function its starts all minimize, so their Fs compare. And a
// warm refit of the same data repeats the first pass exactly, and finds the
// warm fit already a minimum of the same second F.
//
// Each start is minimized by variable projection (minimizeFit): the offsets
// enter the model linearly and F is convex in them, so at every position they
// are solved exactly, and Levenberg–Marquardt steps the position alone on the
// profile min_b F, with the Schur complement of Newton's matrix over the free
// offsets. Newton's matrix is JᵀWJ plus the position's curvature that
// Gauss–Newton leaves out, −K Σⱼ (Wr)ⱼ ∇²dⱼ. Near an extender a residual of a
// millisecond curves F as much as JᵀWJ does (|r| / K d is near 1 at 100 km),
// and a joint step in the position and the offsets zig-zags across the valley
// they trade along, since moving away from a near extender lengthens the far
// paths alike and b absorbs that. Gauss–Newton alone crawled along it for
// hundreds of steps in about 1 pooled fit in 20.
//
// The covariance of (east, north, b) is (JᵀWJ)⁻¹ × max(1, χ²/(n − 2 − R)) at
// the solution, with W the Huber-weighted block inverse (normalEquations) and
// χ² = Σ rᵀWr. It is the full inverse, every offset free, even one the solver
// holds at 0 (V5, 2026-11-17): with the extenders mostly to one side, moving
// away from them lengthens every path alike, which b absorbs, so a fit can
// slide away until b reaches 0, and conditioning on b = 0 would claim a
// certainty the fit doesn't have.

import (
	"errors"
	"math"
	"slices"
)

const (
	// the Huber weights' c (V5)
	HuberC = 2
	// the largest position step, km (driftFit.ts)
	MaxStepKm = 400
	// fewer extenders can't place anyone
	MinFitExtenders = 3

	// a start has converged when a step lowers F by less than this share of F
	// and the undamped step from there would move the position less than
	// this, km (V5)
	convergedKm    = 0.1
	convergedShare = 1e-6
	// a start's trial steps (V5)
	maxFitIterations = 100
	// the starts besides the warm fit and the expected point: the places of
	// this many fastest extenders (V5)
	startExtenders = 3
	// the warm fit's basin is left only for an F lower by more than this (V5)
	warmBasinMargin = 1
	// Levenberg–Marquardt's λ, which scales JᵀWJ's diagonal into the step's
	// matrix: it starts at, and never falls below, driftFit.ts's 0.001
	minDamping = 0.001
	maxDamping = 1e9
	// a cold start's offset, as a share of the run's lowest round trip
	coldLastMileShare = 0.7
	// a pivot this small is singular
	minPivot = 1e-20
	// solving the offsets at a position: Newton's steps until they move less
	// than this, ms, or this many, each halved at most this many times
	offsetSettledMs     = 1e-9
	maxOffsetIterations = 30
	maxOffsetBacktracks = 20
	// the passes of feasible GLS
	fitPasses = 2
	// the distance is smoothed this close to an extender, km
	smoothingKm = MinPlaceErrorKm
)

var (
	ErrTooFewObservations = errors.New("lagdrift: too few observations to fit")
	ErrSingularFit        = errors.New("lagdrift: the fit is singular")
	ErrInvalidExpected    = errors.New("lagdrift: the expected place is not a valid point")
)

// One extender's round trip in one run (V5, V7).
type Observation struct {
	// the extender: every run to the same id shares its route error
	ExtenderId string `json:"extenderId"`
	// the extender's place
	At Point `json:"at"`
	// GeoLite2's accuracy radius for the extender's address, km. The model
	// holds it to [10, 100], and uses 50 when it is not positive (V3).
	PlaceErrorKm float64 `json:"placeErrorKm"`
	// the run's number: one last-mile offset per distinct run
	Run int `json:"run"`
	// V1's estimate
	RoundTrip RoundTrip `json:"roundTrip"`
}

// A fit of one or more runs (V5, V7).
type Fit struct {
	// the fitted position
	At Point `json:"at"`
	// the position's offset from the expected place, km, in the azimuthal
	// equidistant projection centered on the expected place
	EastKm  float64 `json:"eastKm"`
	NorthKm float64 `json:"northKm"`
	// b per run, ms, in the order of Runs
	LastMileMs []float64 `json:"lastMileMs"`
	// the position's covariance: east and north at the fit, km²
	Cov [2][2]float64 `json:"cov"`
	// χ² / (n − 2 − R)
	Chi2PerDof float64 `json:"chi2PerDof"`
	// distinct extenders in the fit
	Extenders int `json:"extenders"`
	// the 1σ ellipse: its axes, km, and the major axis's bearing, degrees
	// clockwise from north in [0, 180)
	MajorKm    float64 `json:"majorKm"`
	MinorKm    float64 `json:"minorKm"`
	BearingDeg float64 `json:"bearingDeg"`

	// An extension of lag.dev/DRIFTTEST.md I3: the run numbers, ascending,
	// that LastMileMs follows.
	Runs []int `json:"runs"`
	// an extension: the observations in the fit
	Observations int `json:"observations"`

	// the steps taken, and whether the last was settled, for tests
	iterations int
	settled    bool
}

// One usable observation.
type fitRow struct {
	at Point
	y  float64
	// σ_meas² + σ₀²
	noiseVar float64
	// s_ext, held
	placeKm float64
	// the run's index in the fit's runs
	run int
	// the observation's run number, until the runs are indexed
	runNumber int
	// τ, the route and place error, for the pass
	tauMs float64
}

// The observations of one extender: one block of V.
type fitBlock struct {
	extenderId string
	rows       []fitRow
}

// Fits the position of obs, one or more runs, from expected (V2, V5, V7). An
// observation with a round trip that is not a positive finite number, a
// negative or non-finite standard error, or an invalid place is left out. It
// needs 3 distinct extenders and 3 + R observations for R runs, else
// ErrTooFewObservations. The 6-extender minimum of a run is the caller's.
//
// warm, when given, is a start too, with its offsets matched by run number
// through warm.Runs (by index when warm has no Runs and as many offsets as obs
// has runs). The other starts, and a run warm lacks, begin each offset at 0.7
// times the run's lowest round trip.
//
// Blocks are in the order their extenders first appear in obs, and rows in a
// block in obs's order. The TypeScript model sums in the same order.
func FitPosition(m Model, expected Point, obs []Observation, warm *Fit) (*Fit, error) {
	if !expected.Valid() {
		return nil, ErrInvalidExpected
	}

	blocks := []*fitBlock{}
	extenderBlocks := map[string]*fitBlock{}
	runNumbers := []int{}
	for _, o := range obs {
		if !usableObservation(o) {
			continue
		}
		block, ok := extenderBlocks[o.ExtenderId]
		if !ok {
			block = &fitBlock{extenderId: o.ExtenderId}
			extenderBlocks[o.ExtenderId] = block
			blocks = append(blocks, block)
		}
		block.rows = append(block.rows, fitRow{
			at:        o.At,
			y:         o.RoundTrip.Ms,
			noiseVar:  m.noiseVar(o.RoundTrip.SigmaMs),
			placeKm:   HeldPlaceErrorKm(o.PlaceErrorKm),
			runNumber: o.Run,
		})
		if !slices.Contains(runNumbers, o.Run) {
			runNumbers = append(runNumbers, o.Run)
		}
	}
	slices.Sort(runNumbers)
	runCount := len(runNumbers)
	n := 0
	for _, block := range blocks {
		for j := range block.rows {
			block.rows[j].run = slices.Index(runNumbers, block.rows[j].runNumber)
		}
		n += len(block.rows)
	}
	if len(blocks) < MinFitExtenders || n < 3+runCount {
		return nil, ErrTooFewObservations
	}
	paramCount := 2 + runCount

	coldLastMile := make([]float64, runCount)
	for r := range coldLastMile {
		coldLastMile[r] = math.Inf(1)
	}
	for _, block := range blocks {
		for _, row := range block.rows {
			coldLastMile[row.run] = math.Min(coldLastMile[row.run], row.y)
		}
	}
	for r := range coldLastMile {
		coldLastMile[r] *= coldLastMileShare
	}

	// τ for each row, its distance measured from a place
	routeErrorsFrom := func(at Point) {
		for _, block := range blocks {
			for j := range block.rows {
				row := &block.rows[j]
				row.tauMs = m.pathSigmaMs(DistanceKm(at, row.at), row.placeKm)
			}
		}
	}

	// pass 1: the route errors measured from the expected place, one F that
	// every cold start minimizes, from the expected point and the places of
	// the three fastest extenders (each by its lowest round trip); the lowest
	// F wins, so this pass depends on the data alone
	iterations := 0
	routeErrorsFrom(expected)
	coldStarts := append([]Point{expected}, fastestPlaces(blocks, startExtenders)...)
	var first *fitMinimum
	for _, start := range coldStarts {
		minimum := minimizeFit(blocks, start, coldLastMile, paramCount)
		iterations += minimum.iterations
		if first == nil || minimum.objective < first.objective {
			first = minimum
		}
	}
	// pass 2: the route errors measured from the first pass's fit, from that
	// fit and from the warm fit, if any, whose basin is left only for an F
	// lower by more than 1
	routeErrorsFrom(first.p)
	final := minimizeFit(blocks, first.p, first.b, paramCount)
	iterations += final.iterations
	if warm != nil && warm.At.Valid() {
		warmB := slices.Clone(coldLastMile)
		for r, run := range runNumbers {
			if lastMile, ok := warmLastMile(warm, run, r, runCount); ok {
				warmB[r] = math.Max(0, lastMile)
			}
		}
		fromWarm := minimizeFit(blocks, warm.At, warmB, paramCount)
		iterations += fromWarm.iterations
		if !(final.objective < fromWarm.objective-warmBasinMargin) {
			final = fromWarm
		}
	}
	converged := first.converged && final.converged
	p := final.p
	b := final.b

	// the covariance and χ² with the route errors measured from the fit
	routeErrorsFrom(p)
	a, _, chi2, _, _ := normalEquations(blocks, p, b, paramCount)

	// lag.dev/DRIFTTEST.md V5: an offset held at its bound in the solver is
	// free here, so the position keeps the freedom b would absorb
	free := make([]bool, paramCount)
	for i := range free {
		free[i] = true
	}
	inverse, ok := invertFree(a, free)
	if !ok {
		return nil, ErrSingularFit
	}
	dof := n - 2 - runCount
	chi2PerDof := chi2 / float64(max(1, dof))
	factor := math.Max(1, chi2PerDof)
	cov := [2][2]float64{
		{inverse[0][0] * factor, inverse[0][1] * factor},
		{inverse[1][0] * factor, inverse[1][1] * factor},
	}
	for _, row := range cov {
		for _, v := range row {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return nil, ErrSingularFit
			}
		}
	}
	if !(0 < cov[0][0]) || !(0 < cov[1][1]) {
		return nil, ErrSingularFit
	}
	majorKm, minorKm, bearingDeg := CovEllipse(cov)
	eastKm, northKm := AzimuthalKm(expected, p)
	return &Fit{
		At:           p,
		EastKm:       eastKm,
		NorthKm:      northKm,
		LastMileMs:   b,
		Cov:          cov,
		Chi2PerDof:   chi2PerDof,
		Extenders:    len(blocks),
		MajorKm:      majorKm,
		MinorKm:      minorKm,
		BearingDeg:   bearingDeg,
		Runs:         runNumbers,
		Observations: n,
		iterations:   iterations,
		settled:      converged,
	}, nil
}

// Where one start's minimization of F ended.
type fitMinimum struct {
	p         Point
	b         []float64
	objective float64
	// trial steps taken
	iterations int
	// it met the convergence rule, rather than the iteration cap
	converged bool
}

// Minimizes F, with the rows' τ as they are, from (p, b), by variable
// projection: the offsets enter the model linearly and F is convex in them, so
// at every position they are solved exactly (solveOffsets), and
// Levenberg–Marquardt steps the position alone on the profile
// φ(p) = min_b F(p, b). Its matrix is the Schur complement of Newton's matrix
// (newtonMatrix) over the free offsets, damped by λ times JᵀWJ's position
// diagonal, and a step is taken only when it lowers φ. A position and its
// offsets otherwise trade along a curved valley, since moving away from a near
// extender lengthens the far paths alike and b absorbs that, and a joint step
// zig-zags across it. It converges once a step taken lowers φ by less than
// 10⁻⁶ of φ and the undamped step from there would move the position less
// than 0.1 km, or when no step lowers φ, within 100 trial steps.
func minimizeFit(blocks []*fitBlock, p Point, b []float64, paramCount int) *fitMinimum {
	eq := solveOffsets(blocks, p, b, paramCount)
	lambda := minDamping
	nu := 2.0
	minimum := &fitMinimum{}
	for minimum.iterations < maxFitIterations {
		minimum.iterations += 1
		s, gr, ok := eq.profile()
		var step [2]float64
		if ok {
			step, ok = solve2([2][2]float64{
				{s[0][0] + lambda*eq.a[0][0], s[0][1]},
				{s[1][0], s[1][1] + lambda*eq.a[1][1]},
			}, gr)
		}
		if ok {
			if length := math.Hypot(step[0], step[1]); MaxStepKm < length {
				step[0] *= MaxStepKm / length
				step[1] *= MaxStepKm / length
			}
			next := solveOffsets(blocks, MoveKm(eq.p, step[0], step[1]), eq.b, paramCount)
			if next.objective <= eq.objective {
				// the gain over the quadratic model's 2δᵀg − δᵀSδ
				predicted := 2*(step[0]*gr[0]+step[1]*gr[1]) -
					(step[0]*(s[0][0]*step[0]+s[0][1]*step[1]) + step[1]*(s[1][0]*step[0]+s[1][1]*step[1]))
				gain := 0.0
				if 0 < predicted {
					gain = (eq.objective - next.objective) / predicted
				}
				drop := eq.objective - next.objective
				eq = next
				c := 2*gain - 1
				lambda = math.Max(minDamping, lambda*math.Max(1.0/3, 1-c*c*c))
				nu = 2
				// converged when the whole step from here is short and φ
				// barely moved: a damped step can be short far from the minimum
				if drop < convergedShare*eq.objective {
					if s, gr, ok := eq.profile(); ok {
						if whole, ok := solve2(s, gr); ok && math.Hypot(whole[0], whole[1]) < convergedKm {
							minimum.converged = true
							break
						}
					}
				}
				continue
			}
		}
		// no step lowers φ at this damping: damp more, unless no step can,
		// which is a minimum
		lambda *= nu
		nu *= 2
		if maxDamping < lambda {
			minimum.converged = true
			break
		}
	}
	minimum.p = eq.p
	minimum.b = eq.b
	minimum.objective = eq.objective
	return minimum
}

// A position, its offsets, and the normal equations there.
type fitEquations struct {
	p         Point
	b         []float64
	a         [][]float64
	g         []float64
	objective float64
	curvature [2][2]float64
}

// The position's profile at the equations' offsets: the Schur complement of
// Newton's matrix over the free offsets, S = H_pp − H_pf H_ff⁻¹ H_fp, and the
// reduced gradient g_p − H_pf H_ff⁻¹ g_f. An offset at 0 is held, not free.
func (self *fitEquations) profile() ([2][2]float64, [2]float64, bool) {
	h := newtonMatrix(self.a, self.curvature)
	free := []int{}
	for r := range self.b {
		if 0 < self.b[r] {
			free = append(free, 2+r)
		}
	}
	s := [2][2]float64{{h[0][0], h[0][1]}, {h[1][0], h[1][1]}}
	gr := [2]float64{self.g[0], self.g[1]}
	if 0 < len(free) {
		// H_ff⁻¹ [H_fp | g_f], column by column
		k := len(free)
		for col := 0; col < 3; col++ {
			augmented := make([][]float64, k)
			for row, i := range free {
				augmented[row] = make([]float64, k+1)
				for c, j := range free {
					augmented[row][c] = h[i][j]
				}
				if col < 2 {
					augmented[row][k] = h[i][col]
				} else {
					augmented[row][k] = self.g[i]
				}
			}
			x, ok := gaussSolve(augmented)
			if !ok {
				return s, gr, false
			}
			for u := 0; u < 2; u++ {
				sum := 0.0
				for row, i := range free {
					sum += h[u][i] * x[row]
				}
				if col < 2 {
					s[u][col] -= sum
				} else {
					gr[u] -= sum
				}
			}
		}
	}
	return s, gr, true
}

// The offsets that minimize F at the position p, from b: Newton's steps on the
// offsets alone, which F is convex in, each offset at its bound of 0 whose
// step would go below it held there, a step halved while it raises F, until
// the offsets move less than 10⁻⁹ ms or 30 steps.
func solveOffsets(blocks []*fitBlock, p Point, b []float64, paramCount int) *fitEquations {
	eq := &fitEquations{p: p, b: slices.Clone(b)}
	eq.a, eq.g, _, eq.objective, eq.curvature = normalEquations(blocks, p, eq.b, paramCount)
	held := make([]bool, paramCount)
	held[0] = true
	held[1] = true
	for iteration := 0; iteration < maxOffsetIterations; iteration++ {
		free := make([]bool, paramCount)
		for i := range free {
			free[i] = !held[i]
		}
		step, ok := constrainedStepOver(eq.a, eq.g, eq.b, free)
		if !ok {
			break
		}
		largest := 0.0
		for r := range eq.b {
			largest = math.Max(largest, math.Abs(step[2+r]))
		}
		if largest < offsetSettledMs {
			break
		}
		t := 1.0
		var next *fitEquations
		for tries := 0; tries < maxOffsetBacktracks; tries++ {
			nextB := make([]float64, len(eq.b))
			for r := range eq.b {
				nextB[r] = math.Max(0, eq.b[r]+step[2+r]*t)
			}
			candidate := &fitEquations{p: p, b: nextB}
			candidate.a, candidate.g, _, candidate.objective, candidate.curvature = normalEquations(blocks, p, nextB, paramCount)
			if candidate.objective <= eq.objective {
				next = candidate
				break
			}
			t /= 2
		}
		if next == nil {
			break
		}
		eq = next
	}
	return eq
}

// The 2 × 2 system's solution; false when it is singular.
func solve2(m [2][2]float64, v [2]float64) ([2]float64, bool) {
	solution, ok := gaussSolve([][]float64{{m[0][0], m[0][1], v[0]}, {m[1][0], m[1][1], v[1]}})
	if !ok {
		return [2]float64{}, false
	}
	return [2]float64{solution[0], solution[1]}, true
}

// JᵀWJ with the position block's curvature added: half of F's Hessian.
func newtonMatrix(a [][]float64, curvature [2][2]float64) [][]float64 {
	h := make([][]float64, len(a))
	for i := range a {
		h[i] = slices.Clone(a[i])
	}
	for u := 0; u < 2; u++ {
		for v := 0; v < 2; v++ {
			h[u][v] += curvature[u][v]
		}
	}
	return h
}

// The places of the n extenders with the lowest round trips, each at the place
// of its lowest, in the order of their round trips (ties in the blocks' order).
func fastestPlaces(blocks []*fitBlock, n int) []Point {
	type fastest struct {
		at Point
		y  float64
	}
	extenders := make([]fastest, 0, len(blocks))
	for _, block := range blocks {
		f := fastest{at: block.rows[0].at, y: block.rows[0].y}
		for _, row := range block.rows[1:] {
			if row.y < f.y {
				f = fastest{at: row.at, y: row.y}
			}
		}
		extenders = append(extenders, f)
	}
	slices.SortStableFunc(extenders, func(a fastest, b fastest) int {
		if a.y < b.y {
			return -1
		}
		if b.y < a.y {
			return 1
		}
		return 0
	})
	places := []Point{}
	for _, f := range extenders[:min(n, len(extenders))] {
		places = append(places, f.at)
	}
	return places
}

func usableObservation(o Observation) bool {
	if !o.At.Valid() {
		return false
	}
	if !(0 < o.RoundTrip.Ms) || math.IsInf(o.RoundTrip.Ms, 1) {
		return false
	}
	if !(0 <= o.RoundTrip.SigmaMs) || math.IsInf(o.RoundTrip.SigmaMs, 1) {
		return false
	}
	return true
}

// The warm fit's offset for a run, if it has one.
func warmLastMile(warm *Fit, run int, index int, runCount int) (float64, bool) {
	if len(warm.Runs) == len(warm.LastMileMs) && 0 < len(warm.Runs) {
		if i := slices.Index(warm.Runs, run); 0 <= i {
			return warm.LastMileMs[i], true
		}
		return 0, false
	}
	if len(warm.Runs) == 0 && len(warm.LastMileMs) == runCount {
		return warm.LastMileMs[index], true
	}
	return 0, false
}

// The normal equations at (p, b): JᵀWJ, JᵀWr, χ² = Σ rᵀWr and the objective
// F = Σ 2ρ_H(z) + ‖r⊥‖², with τ the rows' own. Per block, the shared
// component of r, z = tᵀD⁻¹r / √(Q (1 + Q)), is Huber-weighted by h, and the
// rest, ‖r⊥‖² = rᵀD⁻¹r − (tᵀD⁻¹r)² / Q, is not:
//
//	W = D⁻¹ − ω D⁻¹ t tᵀ D⁻¹,  ω = (1 + Q − h) / (Q (1 + Q))
//
// so rᵀWr = ‖r⊥‖² + h z², and W = V⁻¹ when h = 1. ρ_H(z) is z²/2 within c
// and c|z| − c²/2 beyond. The parameters are east km, north km, then b per
// run; J is the model's slope, and JᵀWr is half of −∂F/∂δ. The last return
// is the position block's curvature that JᵀWJ leaves out, −K Σⱼ (Wr)ⱼ ∇²dⱼ:
// half of ∂²F/∂δ² is JᵀWJ plus it, with W held, which is Newton's matrix.
func normalEquations(blocks []*fitBlock, p Point, b []float64, paramCount int) ([][]float64, []float64, float64, float64, [2][2]float64) {
	a := make([][]float64, paramCount)
	for i := range a {
		a[i] = make([]float64, paramCount)
	}
	g := make([]float64, paramCount)
	chi2 := 0.0
	objective := 0.0

	type rowTerms struct {
		jEast  float64
		jNorth float64
		run    int
		r      float64
		d      float64
		t      float64
		// ∇²d, the smoothed distance's curvature in east and north
		curvature [2][2]float64
	}
	var curvature [2][2]float64
	alpha := make([]float64, paramCount)
	terms := []rowTerms{}
	for _, block := range blocks {
		terms = terms[:0]
		for i := range alpha {
			alpha[i] = 0
		}
		// Q = tᵀD⁻¹t, ρ = tᵀD⁻¹r, rᵀD⁻¹r and α = JᵀD⁻¹t
		q := 0.0
		rho := 0.0
		rr := 0.0
		for _, row := range block.rows {
			d := DistanceKm(p, row.at)
			beta := BearingDeg(p, row.at) * radPerDeg
			smooth, slope := smoothedDistanceKm(d)
			sinBeta := math.Sin(beta)
			cosBeta := math.Cos(beta)
			term := rowTerms{
				jEast:     -sinBeta * MsPerKm * slope,
				jNorth:    -cosBeta * MsPerKm * slope,
				run:       row.run,
				r:         row.y - (b[row.run] + MsPerKm*smooth),
				d:         row.noiseVar,
				t:         row.tauMs,
				curvature: distanceCurvature(d, sinBeta, cosBeta),
			}
			terms = append(terms, term)
			t := row.tauMs
			q += t * t / term.d
			rho += t * term.r / term.d
			rr += term.r * term.r / term.d
			alpha[0] += term.jEast * t / term.d
			alpha[1] += term.jNorth * t / term.d
			alpha[2+term.run] += t / term.d
		}
		omega := 0.0
		if 0 < q {
			z := math.Abs(rho / math.Sqrt(q*(1+q)))
			h := 1.0
			huber := z * z
			if HuberC < z {
				h = HuberC / z
				huber = 2*HuberC*z - HuberC*HuberC
			}
			omega = (1 + q - h) / (q * (1 + q))
			perp := rr - rho*rho/q
			chi2 += perp + h*z*z
			objective += perp + huber
		} else {
			chi2 += rr
			objective += rr
		}
		for _, term := range terms {
			index := [3]int{0, 1, 2 + term.run}
			j := [3]float64{term.jEast, term.jNorth, 1}
			for u := 0; u < 3; u++ {
				g[index[u]] += j[u] * term.r / term.d
				for v := 0; v < 3; v++ {
					a[index[u]][index[v]] += j[u] * j[v] / term.d
				}
			}
			// −K (Wr)ⱼ ∇²dⱼ, with (Wr)ⱼ = (rⱼ − ω tⱼ ρ) / Dⱼ
			wr := (term.r - omega*term.t*rho) / term.d
			for u := 0; u < 2; u++ {
				for v := 0; v < 2; v++ {
					curvature[u][v] -= MsPerKm * wr * term.curvature[u][v]
				}
			}
		}
		for u := 0; u < paramCount; u++ {
			if alpha[u] == 0 {
				continue
			}
			g[u] -= omega * alpha[u] * rho
			for v := 0; v < paramCount; v++ {
				a[u][v] -= omega * alpha[u] * alpha[v]
			}
		}
	}
	return a, g, chi2, objective, curvature
}

// ∇²d of the smoothed distance (smoothedDistanceKm) in east and north km at a
// distance d, β the bearing from the fit to the extender: (I − u uᵀ) / d with u
// the unit vector from the extender, and I / ε within ε of it.
func distanceCurvature(d float64, sinBeta float64, cosBeta float64) [2][2]float64 {
	if d < smoothingKm {
		// 1.0: smoothingKm is an untyped integer constant, and 1 / 10 is 0
		return [2][2]float64{{1.0 / smoothingKm, 0}, {0, 1.0 / smoothingKm}}
	}
	return [2][2]float64{
		{cosBeta * cosBeta / d, -sinBeta * cosBeta / d},
		{-sinBeta * cosBeta / d, sinBeta * sinBeta / d},
	}
}

// The distance the model predicts a round trip from (V2), and its slope in the
// true distance d: d itself, except within 10 km of the extender, below its
// place error's floor (V3), where it is (d² + ε²) / (2ε), ε = 10 km. That is
// d's value and slope at ε, and smooth at the extender's place, where d has a
// cone that a visitor in the extender's city would leave the fit chattering
// on. It adds at most 5 km, 0.075 ms.
func smoothedDistanceKm(d float64) (float64, float64) {
	if smoothingKm <= d {
		return d, 1
	}
	return (d*d + smoothingKm*smoothingKm) / (2 * smoothingKm), d / smoothingKm
}

// The step solving a δ = g over the free parameters, the rest held at a step
// of 0, and each offset at its bound of 0 whose step would go below it held
// there too.
func constrainedStepOver(a [][]float64, g []float64, b []float64, free []bool) ([]float64, bool) {
	free = slices.Clone(free)
	for {
		step, ok := solveFree(a, g, free)
		if !ok {
			return nil, false
		}
		held := false
		for r := range b {
			if free[2+r] && b[r] <= 0 && step[2+r] < 0 {
				free[2+r] = false
				held = true
			}
		}
		if !held {
			return step, true
		}
	}
}

// Solves the system over the free parameters; a held parameter's step is 0.
// Gaussian elimination with partial pivoting (driftFit.ts solve3).
func solveFree(a [][]float64, g []float64, free []bool) ([]float64, bool) {
	indexes := []int{}
	for i, isFree := range free {
		if isFree {
			indexes = append(indexes, i)
		}
	}
	k := len(indexes)
	augmented := make([][]float64, k)
	for row, i := range indexes {
		augmented[row] = make([]float64, k+1)
		for col, j := range indexes {
			augmented[row][col] = a[i][j]
		}
		augmented[row][k] = g[i]
	}
	solution, ok := gaussSolve(augmented)
	if !ok {
		return nil, false
	}
	step := make([]float64, len(g))
	for row, i := range indexes {
		step[i] = solution[row]
	}
	return step, true
}

// Solves the augmented k × (k + 1) system in place.
func gaussSolve(m [][]float64) ([]float64, bool) {
	k := len(m)
	for c := 0; c < k; c++ {
		p := c
		for r := c + 1; r < k; r++ {
			if math.Abs(m[p][c]) < math.Abs(m[r][c]) {
				p = r
			}
		}
		if !(minPivot < math.Abs(m[p][c])) {
			return nil, false
		}
		m[c], m[p] = m[p], m[c]
		for r := 0; r < k; r++ {
			if r == c {
				continue
			}
			f := m[r][c] / m[c][c]
			for j := c; j <= k; j++ {
				m[r][j] -= f * m[c][j]
			}
		}
	}
	solution := make([]float64, k)
	for i := range solution {
		solution[i] = m[i][k] / m[i][i]
	}
	return solution, true
}

// The inverse of a over the free parameters, as a full-size matrix whose held
// rows and columns are 0.
func invertFree(a [][]float64, free []bool) ([][]float64, bool) {
	paramCount := len(a)
	inverse := make([][]float64, paramCount)
	for i := range inverse {
		inverse[i] = make([]float64, paramCount)
	}
	for j := 0; j < paramCount; j++ {
		if !free[j] {
			continue
		}
		unit := make([]float64, paramCount)
		unit[j] = 1
		column, ok := solveFree(a, unit, free)
		if !ok {
			return nil, false
		}
		for i := 0; i < paramCount; i++ {
			inverse[i][j] = column[i]
		}
	}
	return inverse, true
}

// The ellipse of a 2 × 2 east and north covariance: the square roots of its
// eigenvalues, km, and the major axis's bearing, degrees clockwise from north in
// [0, 180).
func CovEllipse(cov [2][2]float64) (majorKm float64, minorKm float64, bearingDeg float64) {
	a := cov[0][0]
	b := (cov[0][1] + cov[1][0]) / 2
	c := cov[1][1]
	mid := (a + c) / 2
	disc := math.Hypot((a-c)/2, b)
	majorKm = math.Sqrt(math.Max(0, mid+disc))
	minorKm = math.Sqrt(math.Max(0, mid-disc))
	// the major axis's angle from east, counterclockwise
	theta := math.Atan2(2*b, a-c) / 2
	bearingDeg = math.Mod(math.Mod(90-theta*degPerRad, 180)+180, 180)
	return
}
