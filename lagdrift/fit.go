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
// The solver is driftFit.ts's Gauss–Newton on local east and north km and the
// offsets, with its Levenberg–Marquardt damping made adaptive: the diagonal is
// multiplied by 1 + λ, λ starting at driftFit.ts's 0.001, and a step is taken
// only when it lowers F. λ follows Nielsen's rule: after a step taken, it is
// multiplied by max(1/3, 1 − (2γ − 1)³), γ the gain in F over the quadratic
// model's, and never falls below 0.001; after a step refused, it is
// multiplied by ν, which starts at 2 and doubles with each refusal in a row. The position step is capped at 400 km, the fit warm-starts from
// the last one, and it settles once a step taken moves the position less than
// 0.05 km and every offset less than 0.005 ms, or when no step lowers F, or
// after 12 steps tried from a warm start and 30 from a cold one. The Huber
// weights are recomputed at every step. An offset at its bound of 0 whose step
// would go below it is held there for that step (an active set), so the
// position's step is the constrained one. With driftFit.ts's fixed damping
// alone, a step against a gross outlier can overshoot a direction the other
// extenders hardly constrain and circle there for good.
//
// lag.dev/DRIFTTEST.md V3 recomputes the variances, whose route error grows
// with dᵢ, at every iteration. Here they are recomputed at every pass (feasible
// GLS in two steps): the first pass measures each dᵢ from the expected place,
// the second from the first pass's fit, and the covariance from the fit.
// Recomputed at every iteration, they make the fit the fixed point of a
// reweighting that moves with the position, and in about 1 fit in 20 of E2's
// geometry (by simulation) that point repels the iteration, which then circles
// it without settling. With the variances fixed for a pass, F is a fixed
// function each pass minimizes, and the fit depends on the data alone, not on
// where the last fit left off.
//
// The covariance of (east, north, b) is (JᵀWJ)⁻¹ × max(1, χ²/(n − 2 − R)) at
// the solution, with W the Huber-weighted block inverse (normalEquations) and
// χ² = Σ rᵀWr. An offset at 0 is held fixed, and the position's covariance is
// the 2 × 2 of the rest.

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

	settledKm      = 0.05
	settledMs      = 0.005
	warmIterations = 12
	coldIterations = 30
	// Levenberg–Marquardt's damping: the diagonal is multiplied by 1 + λ, λ
	// starting at, and never below, driftFit.ts's 0.001
	minDamping = 0.001
	maxDamping = 1e9
	// a cold start's offset, as a share of the run's lowest round trip
	coldLastMileShare = 0.7
	// a pivot this small is singular
	minPivot = 1e-20
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
// warm, when given, starts the fit from its position and its offsets, matched
// by run number through warm.Runs (by index when warm has no Runs and as many
// offsets as obs has runs). A run it lacks starts, as a cold fit's runs do, at
// 0.7 times the run's lowest round trip, and a cold fit starts at expected.
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

	p := expected
	b := slices.Clone(coldLastMile)
	maxIterations := coldIterations
	if warm != nil && warm.At.Valid() {
		p = warm.At
		maxIterations = warmIterations
		for r, run := range runNumbers {
			if lastMile, ok := warmLastMile(warm, run, r, runCount); ok {
				b[r] = math.Max(0, lastMile)
			}
		}
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

	var a [][]float64
	var g []float64
	var objective float64
	settled := false
	iterations := 0
	reference := expected
	for pass := 0; pass < fitPasses; pass++ {
		routeErrorsFrom(reference)
		settled = false
		lambda := minDamping
		nu := 2.0
		a, g, _, objective = normalEquations(blocks, p, b, paramCount)
		for iteration := 0; iteration < maxIterations; iteration++ {
			iterations += 1
			step, ok := constrainedStep(a, g, b, 1+lambda)
			if ok {
				length := math.Hypot(step[0], step[1])
				scale := 1.0
				if MaxStepKm < length {
					scale = MaxStepKm / length
				}
				for i := range step {
					step[i] *= scale
				}
				nextP := MoveKm(p, step[0], step[1])
				nextB := make([]float64, len(b))
				small := math.Hypot(step[0], step[1]) < settledKm
				for r := range b {
					nextB[r] = math.Max(0, b[r]+step[2+r])
					small = small && math.Abs(nextB[r]-b[r]) < settledMs
				}
				nextA, nextG, _, nextObjective := normalEquations(blocks, nextP, nextB, paramCount)
				if nextObjective <= objective {
					// the gain over the quadratic model's 2δᵀg − δᵀAδ
					predicted := 0.0
					for i := range step {
						predicted += 2 * step[i] * g[i]
						for j := range step {
							predicted -= step[i] * a[i][j] * step[j]
						}
					}
					gain := 0.0
					if 0 < predicted {
						gain = (objective - nextObjective) / predicted
					}
					p, b = nextP, nextB
					a, g, objective = nextA, nextG, nextObjective
					c := 2*gain - 1
					lambda = math.Max(minDamping, lambda*math.Max(1.0/3, 1-c*c*c))
					nu = 2
					if small {
						settled = true
						break
					}
					continue
				}
			}
			// no step lowers F at this damping: damp more, unless no step
			// can, which is a minimum
			lambda *= nu
			nu *= 2
			if maxDamping < lambda {
				settled = true
				break
			}
		}
		reference = p
	}
	// the covariance and χ² with the route errors measured from the fit
	routeErrorsFrom(p)
	a, _, chi2, _ := normalEquations(blocks, p, b, paramCount)

	// an offset at its bound is held fixed: the covariance is the rest's
	free := make([]bool, paramCount)
	free[0] = true
	free[1] = true
	for r := range b {
		free[2+r] = 0 < b[r]
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
		settled:      settled,
	}, nil
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
// run; J is the model's slope, so a step solves (JᵀWJ) δ = JᵀWr, and JᵀWr is
// half of −∂F/∂δ.
func normalEquations(blocks []*fitBlock, p Point, b []float64, paramCount int) ([][]float64, []float64, float64, float64) {
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
	}
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
			term := rowTerms{
				jEast:  -math.Sin(beta) * MsPerKm * slope,
				jNorth: -math.Cos(beta) * MsPerKm * slope,
				run:    row.run,
				r:      row.y - (b[row.run] + MsPerKm*smooth),
				d:      row.noiseVar,
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
	return a, g, chi2, objective
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

// The Gauss–Newton step with the diagonal multiplied by diagonalFactor, each
// offset at its bound of 0 whose step would go below it held there. False when
// the system is singular.
func constrainedStep(a [][]float64, g []float64, b []float64, diagonalFactor float64) ([]float64, bool) {
	paramCount := len(g)
	free := make([]bool, paramCount)
	for i := range free {
		free[i] = true
	}
	for {
		step, ok := solveFree(a, g, free, diagonalFactor)
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

// Solves the system over the free parameters, with their diagonal multiplied by
// diagonalFactor; a held parameter's step is 0. Gaussian elimination with
// partial pivoting (driftFit.ts solve3).
func solveFree(a [][]float64, g []float64, free []bool, diagonalFactor float64) ([]float64, bool) {
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
			v := a[i][j]
			if i == j {
				v *= diagonalFactor
			}
			augmented[row][col] = v
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
		column, ok := solveFree(a, unit, free, 1)
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
