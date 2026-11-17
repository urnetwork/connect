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
// Each start is minimized by variable projection (fitSolver.minimize): the offsets
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
// the solution, with W the Huber-weighted block inverse (fitSolver.evaluate) and
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
	// the trig of the extender's latitude
	cosLat float64
	sinLat float64
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
	problem, err := newFitProblem(m, expected, obs)
	if err != nil {
		return nil, err
	}
	// pass 1: the route errors measured from the expected place, one F that
	// every cold start minimizes, from the expected point and the places of
	// the three fastest extenders (each by its lowest round trip); the lowest
	// F wins, so this pass depends on the data alone
	iterations := 0
	problem.routeErrorsFrom(expected)
	coldStarts := append([]Point{expected}, fastestPlaces(problem.blocks, startExtenders)...)
	var first *fitMinimum
	for _, start := range coldStarts {
		minimum := problem.solver.minimize(start, problem.coldLastMile, maxFitIterations)
		iterations += minimum.iterations
		if first == nil || minimum.objective < first.objective {
			first = minimum
		}
	}
	// pass 2: the route errors measured from the first pass's fit, from that
	// fit and from the warm fit, if any, whose basin is left only for an F
	// lower by more than 1
	problem.routeErrorsFrom(first.p)
	final := problem.solver.minimize(first.p, first.b, maxFitIterations)
	iterations += final.iterations
	if warmB, ok := problem.warmLastMile(warm); ok {
		fromWarm := problem.solver.minimize(warm.At, warmB, maxFitIterations)
		iterations += fromWarm.iterations
		if !(final.objective < fromWarm.objective-warmBasinMargin) {
			final = fromWarm
		}
	}
	return problem.finish(final.p, final.b, iterations, first.converged && final.converged)
}

// FitPosition from the warm fit alone, as the frame refits on each tick
// (lag.dev/DRIFTTEST.md V10): its two passes each run one start, the first from
// the warm fit and the second from the first's fit, with V5's convergence
// rule. Without a warm fit it is FitPosition. On the same observations it
// lands where the fit it starts from did, unless that fit's first pass found
// a lower minimum from another start than the warm fit's basin holds; the
// full multi-start, run when the extenders change, every 2 s and before a
// stop, finds those.
func RefitPosition(m Model, expected Point, obs []Observation, warm *Fit) (*Fit, error) {
	problem, err := newFitProblem(m, expected, obs)
	if err != nil {
		return nil, err
	}
	warmB, ok := problem.warmLastMile(warm)
	if !ok {
		return FitPosition(m, expected, obs, nil)
	}
	problem.routeErrorsFrom(expected)
	first := problem.solver.minimize(warm.At, warmB, maxFitIterations)
	problem.routeErrorsFrom(first.p)
	final := problem.solver.minimize(first.p, first.b, maxFitIterations)
	return problem.finish(final.p, final.b, first.iterations+final.iterations, first.converged && final.converged)
}

// A fit's observations as blocks, with what every start shares.
type fitProblem struct {
	m            Model
	expected     Point
	blocks       []*fitBlock
	runNumbers   []int
	observations int
	coldLastMile []float64
	solver       *fitSolver
}

func newFitProblem(m Model, expected Point, obs []Observation) (*fitProblem, error) {
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
	return &fitProblem{
		m:            m,
		expected:     expected,
		blocks:       blocks,
		runNumbers:   runNumbers,
		observations: n,
		coldLastMile: coldLastMile,
		solver:       newFitSolver(blocks, 2+runCount),
	}, nil
}

// τ for each row, its distance measured from a place.
func (self *fitProblem) routeErrorsFrom(at Point) {
	for _, block := range self.blocks {
		for j := range block.rows {
			row := &block.rows[j]
			row.tauMs = self.m.pathSigmaMs(DistanceKm(at, row.at), row.placeKm)
		}
	}
}

// The warm fit's offsets for the problem's runs, a run it lacks at its cold
// start; false without a valid warm fit.
func (self *fitProblem) warmLastMile(warm *Fit) ([]float64, bool) {
	if warm == nil || !warm.At.Valid() {
		return nil, false
	}
	b := slices.Clone(self.coldLastMile)
	for r, run := range self.runNumbers {
		if lastMile, ok := warmLastMile(warm, run, r, len(self.runNumbers)); ok {
			b[r] = math.Max(0, lastMile)
		}
	}
	return b, true
}

// The fit at (p, b): its covariance and χ² with the route errors measured from
// p, and its ellipse.
func (self *fitProblem) finish(p Point, b []float64, iterations int, converged bool) (*Fit, error) {
	runCount := len(self.runNumbers)
	paramCount := 2 + runCount
	self.routeErrorsFrom(p)
	eq := newFitEquations(paramCount, runCount)
	self.solver.geometryAt(p)
	self.solver.evaluate(b, eq, true)

	// lag.dev/DRIFTTEST.md V5: an offset held at its bound in the solver is
	// free here, so the position keeps the freedom b would absorb
	free := make([]bool, paramCount)
	for i := range free {
		free[i] = true
	}
	inverse, ok := invertFree(eq.a, free)
	if !ok {
		return nil, ErrSingularFit
	}
	dof := self.observations - 2 - runCount
	chi2PerDof := eq.chi2 / float64(max(1, dof))
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
	eastKm, northKm := AzimuthalKm(self.expected, p)
	return &Fit{
		At:           p,
		EastKm:       eastKm,
		NorthKm:      northKm,
		LastMileMs:   b,
		Cov:          cov,
		Chi2PerDof:   chi2PerDof,
		Extenders:    len(self.blocks),
		MajorKm:      majorKm,
		MinorKm:      minorKm,
		BearingDeg:   bearingDeg,
		Runs:         self.runNumbers,
		Observations: self.observations,
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
