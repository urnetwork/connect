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
// way, so it is discounted in every run at once (V7). A block's weight h scales
// its V⁻¹.
//
// The solver is driftFit.ts's: Gauss–Newton on local east and north km and the
// offsets, with the diagonal damped by 1.001 (Levenberg–Marquardt), the
// position step capped at 400 km, warm-started from the last fit, and settled
// once a step moves the position less than 0.05 km and every offset less than
// 0.005 ms, or after 12 iterations from a warm start and 30 from a cold one.
// The variances depend on p, so they and the Huber weights are recomputed at
// every iteration. An offset at its bound of 0 whose step would go below it is
// held there for that step (an active set), so the position's step is the
// constrained one.
//
// The covariance of (east, north, b) is (JᵀWJ)⁻¹ × max(1, χ²/(n − 2 − R)),
// with W the Huber-weighted V⁻¹ at the solution and χ² = Σ h rᵀV⁻¹r. An offset
// at 0 is held fixed, and the position's covariance is the 2 × 2 of the rest.

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
	damping        = 1.001
	// a cold start's offset, as a share of the run's lowest round trip
	coldLastMileShare = 0.7
	// a pivot this small is singular
	minPivot = 1e-20
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

	var a [][]float64
	var chi2 float64
	settled := false
	for iteration := 0; ; iteration++ {
		var g []float64
		a, g, chi2 = normalEquations(m, blocks, p, b, paramCount)
		if settled || maxIterations <= iteration {
			break
		}
		step, ok := constrainedStep(a, g, b)
		if !ok {
			break
		}
		length := math.Hypot(step[0], step[1])
		scale := 1.0
		if MaxStepKm < length {
			scale = MaxStepKm / length
		}
		p = MoveKm(p, step[0]*scale, step[1]*scale)
		settled = length*scale < settledKm
		for r := range b {
			next := math.Max(0, b[r]+step[2+r]*scale)
			settled = settled && math.Abs(next-b[r]) < settledMs
			b[r] = next
		}
	}

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

// JᵀWJ, JᵀWr and χ² = Σ h rᵀV⁻¹r at (p, b), with W the Huber-weighted block
// inverse. The parameters are east km, north km, then b per run; J is the
// model's slope, so a step solves (JᵀWJ) δ = JᵀWr.
func normalEquations(m Model, blocks []*fitBlock, p Point, b []float64, paramCount int) ([][]float64, []float64, float64) {
	a := make([][]float64, paramCount)
	for i := range a {
		a[i] = make([]float64, paramCount)
	}
	g := make([]float64, paramCount)
	chi2 := 0.0

	type rowTerms struct {
		jEast  float64
		jNorth float64
		run    int
		r      float64
		d      float64
		t      float64
	}
	alpha := make([]float64, paramCount)
	terms := []rowTerms{}
	for _, block := range blocks {
		terms = terms[:0]
		for i := range alpha {
			alpha[i] = 0
		}
		// Q = tᵀD⁻¹t, ρ = tᵀD⁻¹r, α = JᵀD⁻¹t
		q := 0.0
		rho := 0.0
		for _, row := range block.rows {
			d := DistanceKm(p, row.at)
			beta := BearingDeg(p, row.at) * radPerDeg
			term := rowTerms{
				jEast:  -math.Sin(beta) * MsPerKm,
				jNorth: -math.Cos(beta) * MsPerKm,
				run:    row.run,
				r:      row.y - (b[row.run] + MsPerKm*d),
				d:      row.noiseVar,
				t:      m.pathSigmaMs(d, row.placeKm),
			}
			terms = append(terms, term)
			q += term.t * term.t / term.d
			rho += term.t * term.r / term.d
			alpha[0] += term.jEast * term.t / term.d
			alpha[1] += term.jNorth * term.t / term.d
			alpha[2+term.run] += term.t / term.d
		}
		h := 1.0
		if 0 < q {
			z := math.Abs(rho / math.Sqrt(q*(1+q)))
			if HuberC < z {
				h = HuberC / z
			}
		}
		for _, term := range terms {
			index := [3]int{0, 1, 2 + term.run}
			j := [3]float64{term.jEast, term.jNorth, 1}
			for u := 0; u < 3; u++ {
				g[index[u]] += h * j[u] * term.r / term.d
				for v := 0; v < 3; v++ {
					a[index[u]][index[v]] += h * j[u] * j[v] / term.d
				}
			}
			chi2 += h * term.r * term.r / term.d
		}
		shrink := 1 / (1 + q)
		for u := 0; u < paramCount; u++ {
			if alpha[u] == 0 {
				continue
			}
			g[u] -= h * alpha[u] * rho * shrink
			for v := 0; v < paramCount; v++ {
				a[u][v] -= h * alpha[u] * alpha[v] * shrink
			}
		}
		chi2 -= h * rho * rho * shrink
	}
	return a, g, chi2
}

// The damped Gauss–Newton step, with each offset at its bound of 0 whose step
// would go below it held there. False when the system is singular.
func constrainedStep(a [][]float64, g []float64, b []float64) ([]float64, bool) {
	paramCount := len(g)
	free := make([]bool, paramCount)
	for i := range free {
		free[i] = true
	}
	for {
		step, ok := solveFree(a, g, free, damping)
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
