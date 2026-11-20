package lagdrift

// The fit's solver (fit.go): variable projection with Levenberg–Marquardt on
// the position, over a workspace that one fit reuses.
//
// A position's geometry, each row's slope, smoothed distance and curvature, is
// computed once per position (geometryAt), with the trig of each extender's
// latitude computed once per fit. The offsets are then solved at that
// position with arithmetic alone (evaluate without the position's terms), and
// the full normal equations are built once, at the solved offsets. Every sum
// runs in the same order whether or not the position's terms are built, so
// solving the offsets alone gives the full equations' values to the bit, and
// the fit is the one a direct evaluation at every step gives.

import (
	"math"
	"slices"
)

// A row's geometry at a position.
type rowGeometry struct {
	jEast     float64
	jNorth    float64
	smooth    float64
	curvature [2][2]float64
}

// One fit's workspace. Not safe for concurrent use; a fit makes its own.
type fitSolver struct {
	blocks     []*fitBlock
	paramCount int
	// per row, in block order: its geometry at the position last given to
	// geometryAt, and its residual at the offsets last evaluated
	geometry  []rowGeometry
	residuals []float64
	alpha     []float64
	// the offset solve's scratch
	candidate *fitEquations
	held      []bool
	free      []bool
	augmented [][]float64
	solution  []float64
}

// A position, its offsets, and the normal equations there.
type fitEquations struct {
	p         Point
	b         []float64
	a         [][]float64
	g         []float64
	chi2      float64
	objective float64
	curvature [2][2]float64
}

func newFitEquations(paramCount int, runCount int) *fitEquations {
	eq := &fitEquations{
		b: make([]float64, runCount),
		a: make([][]float64, paramCount),
		g: make([]float64, paramCount),
	}
	for i := range eq.a {
		eq.a[i] = make([]float64, paramCount)
	}
	return eq
}

func newFitSolver(blocks []*fitBlock, paramCount int) *fitSolver {
	rowCount := 0
	for _, block := range blocks {
		for j := range block.rows {
			row := &block.rows[j]
			row.cosLat = math.Cos(row.at.Lat * radPerDeg)
			row.sinLat = math.Sin(row.at.Lat * radPerDeg)
		}
		rowCount += len(block.rows)
	}
	solver := &fitSolver{
		blocks:     blocks,
		paramCount: paramCount,
		geometry:   make([]rowGeometry, rowCount),
		residuals:  make([]float64, rowCount),
		alpha:      make([]float64, paramCount),
		candidate:  newFitEquations(paramCount, paramCount-2),
		held:       make([]bool, paramCount),
		free:       make([]bool, paramCount),
		augmented:  make([][]float64, paramCount),
		solution:   make([]float64, paramCount),
	}
	for i := range solver.augmented {
		solver.augmented[i] = make([]float64, paramCount+1)
	}
	return solver
}

// Each row's geometry at p: DistanceKm(p, eᵢ) and BearingDeg(p, eᵢ), with p's
// trig computed once, the slope and curvature of the smoothed distance.
func (self *fitSolver) geometryAt(p Point) {
	cosP := math.Cos(p.Lat * radPerDeg)
	sinP := math.Sin(p.Lat * radPerDeg)
	i := 0
	for _, block := range self.blocks {
		for _, row := range block.rows {
			// DistanceKm(p, row.at)
			dLat := (row.at.Lat - p.Lat) * radPerDeg
			dLon := (row.at.Lon - p.Lon) * radPerDeg
			sLat := math.Sin(dLat / 2)
			sLon := math.Sin(dLon / 2)
			s := sLat*sLat + cosP*row.cosLat*sLon*sLon
			d := 2 * EarthRadiusKm * math.Asin(math.Min(1, math.Sqrt(s)))
			// BearingDeg(p, row.at), in radians
			y := math.Sin(dLon) * row.cosLat
			x := cosP*row.sinLat - sinP*row.cosLat*math.Cos(dLon)
			beta := math.Mod(math.Atan2(y, x)*degPerRad+360, 360) * radPerDeg
			smooth, slope := smoothedDistanceKm(d)
			sinBeta := math.Sin(beta)
			cosBeta := math.Cos(beta)
			self.geometry[i] = rowGeometry{
				jEast:     -sinBeta * MsPerKm * slope,
				jNorth:    -cosBeta * MsPerKm * slope,
				smooth:    smooth,
				curvature: distanceCurvature(d, sinBeta, cosBeta),
			}
			i += 1
		}
	}
}

// The normal equations at the geometry's position and offsets b, into eq:
// JᵀWJ, JᵀWr, χ² = Σ rᵀWr and the objective F = Σ 2ρ_H(z) + ‖r⊥‖², with τ
// the rows' own. Per block, the shared component of r,
// z = tᵀD⁻¹r / √(Q (1 + Q)), is Huber-weighted by h, and the rest,
// ‖r⊥‖² = rᵀD⁻¹r − (tᵀD⁻¹r)² / Q, is not:
//
//	W = D⁻¹ − ω D⁻¹ t tᵀ D⁻¹,  ω = (1 + Q − h) / (Q (1 + Q))
//
// so rᵀWr = ‖r⊥‖² + h z², and W = V⁻¹ when h = 1. ρ_H(z) is z²/2 within c
// and c|z| − c²/2 beyond. The parameters are east km, north km, then b per
// run; J is the model's slope, and JᵀWr is half of −∂F/∂δ. eq.curvature is
// the position block's curvature that JᵀWJ leaves out, −K Σⱼ (Wr)ⱼ ∇²dⱼ: half
// of ∂²F/∂δ² is JᵀWJ plus it, with W held, which is Newton's matrix.
//
// Without full, only the offsets' rows and columns of JᵀWJ and JᵀWr are
// built, with χ² and F, which is all solving the offsets needs; their values
// are the full equations' own.
func (self *fitSolver) evaluate(b []float64, eq *fitEquations, full bool) {
	paramCount := self.paramCount
	a := eq.a
	g := eq.g
	for i := range a {
		row := a[i]
		for j := range row {
			row[j] = 0
		}
		g[i] = 0
	}
	chi2 := 0.0
	objective := 0.0
	var curvature [2][2]float64
	alpha := self.alpha
	first := 0
	for _, block := range self.blocks {
		for i := range alpha {
			alpha[i] = 0
		}
		// Q = tᵀD⁻¹t, ρ = tᵀD⁻¹r, rᵀD⁻¹r and α = JᵀD⁻¹t
		q := 0.0
		rho := 0.0
		rr := 0.0
		for j, row := range block.rows {
			geo := &self.geometry[first+j]
			r := row.y - (b[row.run] + MsPerKm*geo.smooth)
			self.residuals[first+j] = r
			t := row.tauMs
			q += t * t / row.noiseVar
			rho += t * r / row.noiseVar
			rr += r * r / row.noiseVar
			alpha[0] += geo.jEast * t / row.noiseVar
			alpha[1] += geo.jNorth * t / row.noiseVar
			alpha[2+row.run] += t / row.noiseVar
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
		for j, row := range block.rows {
			geo := &self.geometry[first+j]
			r := self.residuals[first+j]
			d := row.noiseVar
			if !full {
				run := 2 + row.run
				g[run] += 1 * r / d
				a[run][run] += 1 * 1 / d
				continue
			}
			index := [3]int{0, 1, 2 + row.run}
			jv := [3]float64{geo.jEast, geo.jNorth, 1}
			for u := 0; u < 3; u++ {
				g[index[u]] += jv[u] * r / d
				for v := 0; v < 3; v++ {
					a[index[u]][index[v]] += jv[u] * jv[v] / d
				}
			}
			// −K (Wr)ⱼ ∇²dⱼ, with (Wr)ⱼ = (rⱼ − ω tⱼ ρ) / Dⱼ
			wr := (r - omega*row.tauMs*rho) / d
			for u := 0; u < 2; u++ {
				for v := 0; v < 2; v++ {
					curvature[u][v] -= MsPerKm * wr * geo.curvature[u][v]
				}
			}
		}
		low := 0
		if !full {
			low = 2
		}
		for u := low; u < paramCount; u++ {
			if alpha[u] == 0 {
				continue
			}
			g[u] -= omega * alpha[u] * rho
			for v := low; v < paramCount; v++ {
				a[u][v] -= omega * alpha[u] * alpha[v]
			}
		}
		first += len(block.rows)
	}
	eq.chi2 = chi2
	eq.objective = objective
	eq.curvature = curvature
}

// The equations at p with the offsets that minimize F there, from b, into eq:
// Newton's steps on the offsets alone, which F is convex in, each offset at
// its bound of 0 whose step would go below it held there, a step halved while
// it raises F, until the offsets move less than 10⁻⁹ ms or 30 steps. Then the
// full equations at those offsets.
func (self *fitSolver) solveOffsets(p Point, b []float64, eq *fitEquations) {
	self.geometryAt(p)
	eq.p = p
	copy(eq.b, b)
	self.evaluate(eq.b, eq, false)
	held := self.held
	for i := range held {
		held[i] = i < 2
	}
	for iteration := 0; iteration < maxOffsetIterations; iteration++ {
		free := self.free
		for i := range free {
			free[i] = !held[i]
		}
		step, ok := self.constrainedStep(eq.a, eq.g, eq.b, free)
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
		accepted := false
		candidate := self.candidate
		for tries := 0; tries < maxOffsetBacktracks; tries++ {
			for r := range eq.b {
				candidate.b[r] = math.Max(0, eq.b[r]+step[2+r]*t)
			}
			self.evaluate(candidate.b, candidate, false)
			if candidate.objective <= eq.objective {
				accepted = true
				break
			}
			t /= 2
		}
		if !accepted {
			break
		}
		copy(eq.b, candidate.b)
		eq.objective = candidate.objective
		eq.chi2 = candidate.chi2
		for i := range eq.a {
			copy(eq.a[i], candidate.a[i])
		}
		copy(eq.g, candidate.g)
	}
	self.evaluate(eq.b, eq, true)
}

// The step solving a δ = g over the free parameters, the rest held at a step
// of 0, and each offset at its bound of 0 whose step would go below it held
// there too. free is changed. The step is the solver's buffer.
func (self *fitSolver) constrainedStep(a [][]float64, g []float64, b []float64, free []bool) ([]float64, bool) {
	for {
		k := 0
		for i, isFree := range free {
			if !isFree {
				continue
			}
			row := self.augmented[k]
			c := 0
			for j, jFree := range free {
				if jFree {
					row[c] = a[i][j]
					c += 1
				}
			}
			row[c] = g[i]
			k += 1
		}
		system := self.augmented[:k]
		for i := range system {
			system[i] = system[i][:k+1]
		}
		solution, ok := gaussSolve(system)
		for i := range self.augmented {
			self.augmented[i] = self.augmented[i][:cap(self.augmented[i])]
		}
		if !ok {
			return nil, false
		}
		step := self.solution
		k = 0
		for i, isFree := range free {
			if isFree {
				step[i] = solution[k]
				k += 1
			} else {
				step[i] = 0
			}
		}
		heldOne := false
		for r := range b {
			if free[2+r] && b[r] <= 0 && step[2+r] < 0 {
				free[2+r] = false
				heldOne = true
			}
		}
		if !heldOne {
			return step, true
		}
	}
}

// Minimizes F, with the rows' τ as they are, from (p, b), by variable
// projection: the offsets enter the model linearly and F is convex in them, so
// at every position they are solved exactly (solveOffsets), and
// Levenberg–Marquardt steps the position alone on the profile
// φ(p) = min_b F(p, b). Its matrix is the Schur complement of Newton's matrix
// over the free offsets (profile), damped by λ times JᵀWJ's position diagonal,
// and a step is taken only when it lowers φ. A position and its offsets
// otherwise trade along a curved valley, since moving away from a near
// extender lengthens the far paths alike and b absorbs that, and a joint step
// zig-zags across it. It converges once a step taken lowers φ by less than
// 10⁻⁶ of φ and the undamped step from there would move the position less
// than 0.1 km, or when no step lowers φ, within maxIterations trial steps.
func (self *fitSolver) minimize(p Point, b []float64, maxIterations int) *fitMinimum {
	eq := newFitEquations(self.paramCount, len(b))
	next := newFitEquations(self.paramCount, len(b))
	self.solveOffsets(p, b, eq)
	lambda := minDamping
	nu := 2.0
	minimum := &fitMinimum{}
	for minimum.iterations < maxIterations {
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
			self.solveOffsets(MoveKm(eq.p, step[0], step[1]), eq.b, next)
			if next.objective <= eq.objective {
				// the gain over the quadratic model's 2δᵀg − δᵀSδ
				predicted := 2*(step[0]*gr[0]+step[1]*gr[1]) -
					(step[0]*(s[0][0]*step[0]+s[0][1]*step[1]) + step[1]*(s[1][0]*step[0]+s[1][1]*step[1]))
				gain := 0.0
				if 0 < predicted {
					gain = (eq.objective - next.objective) / predicted
				}
				drop := eq.objective - next.objective
				eq, next = next, eq
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
	minimum.b = slices.Clone(eq.b)
	minimum.objective = eq.objective
	return minimum
}

// The position's profile at the equations' offsets: the Schur complement of
// Newton's matrix over the free offsets, S = H_pp − H_pf H_ff⁻¹ H_fp, and the
// reduced gradient g_p − H_pf H_ff⁻¹ g_f. An offset at 0 is held, not free.
// Newton's matrix is JᵀWJ with the position's curvature added; the offsets'
// rows and columns are JᵀWJ's.
func (self *fitEquations) profile() ([2][2]float64, [2]float64, bool) {
	a := self.a
	s := [2][2]float64{
		{a[0][0] + self.curvature[0][0], a[0][1] + self.curvature[0][1]},
		{a[1][0] + self.curvature[1][0], a[1][1] + self.curvature[1][1]},
	}
	gr := [2]float64{self.g[0], self.g[1]}
	free := make([]int, 0, len(self.b))
	for r := range self.b {
		if 0 < self.b[r] {
			free = append(free, 2+r)
		}
	}
	k := len(free)
	if k == 0 {
		return s, gr, true
	}
	// H_ff⁻¹ [H_fp | g_f], column by column
	augmented := make([][]float64, k)
	for row := range augmented {
		augmented[row] = make([]float64, k+1)
	}
	for col := 0; col < 3; col++ {
		for row, i := range free {
			for c, j := range free {
				augmented[row][c] = a[i][j]
			}
			if col < 2 {
				augmented[row][k] = a[i][col]
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
				sum += a[u][i] * x[row]
			}
			if col < 2 {
				s[u][col] -= sum
			} else {
				gr[u] -= sum
			}
		}
	}
	return s, gr, true
}

// The 2 × 2 system's solution; false when it is singular.
func solve2(m [2][2]float64, v [2]float64) ([2]float64, bool) {
	solution, ok := gaussSolve([][]float64{{m[0][0], m[0][1], v[0]}, {m[1][0], m[1][1], v[1]}})
	if !ok {
		return [2]float64{}, false
	}
	return [2]float64{solution[0], solution[1]}, true
}
