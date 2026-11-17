package lagdriftsim

// One visitor's runs (lag.dev/DRIFTTEST.md V1, V5, V7, V10, E2, E5).
//
// A run opens 12 sessions at once, with a spare for each that hasn't opened by
// 4 s. After every ping round, 250 ms, the frame fits its round trips so far,
// pooled with the runs already in the verdict (V9), and evaluates V10's rules
// on the fit. When a rule holds, the frame closes the sessions, and the server
// takes each extender with enough round trips measured by then (E5), pools the
// run with the earlier counting runs (V7) and decides the verdict (V6).
//
// The study compares stop policies on the same visitors. Everything a run
// does before it stops (its pings, the frame's fits and their region tests)
// is the same whatever stops it, so a run's trace is computed once and each
// policy picks its own stop from it. A later run's trace depends on what the
// earlier runs put in the verdict, so traces are kept by that.

import (
	"fmt"
	"math"
	"strings"

	"github.com/urnetwork/connect/lagdrift"
)

const (
	// the frame's ping round, ms (V10)
	tickMs = 250
	// ticks to the cap
	capTicks = lagdrift.StopCapMs / tickMs
	// the frame fits once this many extenders of the run, or of the pooled
	// runs, have a round trip (L18's 6)
	fitExtenders = 6
	// a run counts with this many usable extenders (E5)
	countingExtenders = 6
	// the verdict pools the last this many counting runs (V7)
	windowRuns = 3
	// the runs each visitor makes
	visitorRuns = 3
)

// The confidences the study judges at (V6).
var confidences = []float64{0.95, 0.99, 0.999}

// What ends a run: V10's rules, with what the study varies.
type stopPolicy struct {
	name string
	// clear's k; +Inf rules out clear
	earlyK float64
	// false rules out converged
	converged bool
	// converged and clear need this many extenders with roundTrips each, and
	// the server takes an extender with roundTrips (E5)
	extenders  int
	roundTrips int
	// converged's window, ms, and its area ratio
	windowMs  int64
	areaRatio float64
}

// V10's policy at a k_early.
func v10Policy(earlyK float64) stopPolicy {
	return stopPolicy{
		name:       fmt.Sprintf("k_early %s", formatK(earlyK)),
		earlyK:     earlyK,
		converged:  true,
		extenders:  lagdrift.StopExtenders,
		roundTrips: lagdrift.StopRoundTrips,
		windowMs:   lagdrift.StopWindowMs,
		areaRatio:  lagdrift.BrowserStopTolerance.AreaRatio,
	}
}

func formatK(k float64) string {
	if math.IsInf(k, 1) {
		return "∞"
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", k), "0"), ".")
}

// The policy is lagdrift's own V10, so EvaluateStop decides as it does.
func (self stopPolicy) isLagdrift() bool {
	return self.earlyK == lagdrift.EarlyK && self.converged &&
		self.extenders == lagdrift.StopExtenders && self.roundTrips == lagdrift.StopRoundTrips &&
		self.windowMs == lagdrift.StopWindowMs && self.areaRatio == lagdrift.BrowserStopTolerance.AreaRatio
}

// One session of a run.
type session struct {
	poolIndex int
	id        string
	extender  *fleetExtender
	// when it opened, ms after the sessions opened; −1 when it never did
	openMs   float64
	answered []ping
}

// What the frame knows after a ping round.
type tickState struct {
	atMs       int64
	fit        *lagdrift.Fit
	roundTrips []int
	lastPongMs int64
	// Judge's D against the IP country, once asked for
	d      float64
	judged bool
}

// One run's sessions, and what the frame sees at each ping round, given the
// runs before it that the verdict holds.
type runTrace struct {
	sim      *visitorSim
	run      int
	sessions []*session
	prior    []lagdrift.Observation
	ticks    []*tickState
	history  []lagdrift.Snapshot
	warm     *lagdrift.Fit
	// per session: the answered pings the last estimate is from, and it
	estimateCounts []int
	estimates      []lagdrift.RoundTrip
}

// What one run of one policy came to.
type runOutcome struct {
	stopMs  int64
	reason  lagdrift.StopReason
	usable  int
	counted bool
	verdict *verdictRecord
}

// The verdict after a run, over the counting runs so far.
type verdictRecord struct {
	counting int
	verdicts [3]lagdrift.Verdict
	reasons  [3]lagdrift.Reason
	fitted   bool
	d        float64
	majorKm  float64
	minorKm  float64
	chi2     float64
	// some run's b is at its bound of 0
	bAtZero bool
	// the truth's Mahalanobis distance from the fit: the region at a
	// confidence holds the truth when this is within its k
	truthM float64
	// from the fit to the truth, km
	errorKm float64
}

// One visitor's outcomes: per policy, per run.
type visitorOutcome struct {
	spec     *visitorSpec
	outcomes [][]runOutcome
	// the study's checks that it decides as lagdrift does
	stopChecks       int
	stopMismatches   []string
	decideChecks     int
	decideMismatches []string
}

// A visitor being simulated.
type visitorSim struct {
	world  *world
	config *studyConfig
	spec   *visitorSpec
	fleet  []fleetExtender
	model  lagdrift.Model
	access accessLink
	// pool index → serving now
	serving   []bool
	stretches map[int]float64
	links     [visitorRuns]*runLink
	choices   [visitorRuns]choice
	sessions  [visitorRuns][]*session
	traces    map[string]*runTrace
	verdicts  map[string]*verdictRecord
	checks    bool
	outcome   *visitorOutcome
}

func simulateVisitor(w *world, config *studyConfig, spec *visitorSpec) *visitorOutcome {
	sim := &visitorSim{
		world:     w,
		config:    config,
		spec:      spec,
		fleet:     w.fleets[spec.fleet],
		model:     lagdrift.Model{Kappa: config.modelKappa, Sigma0Ms: lagdrift.DefaultSigma0Ms},
		stretches: map[int]float64{},
		traces:    map[string]*runTrace{},
		verdicts:  map[string]*verdictRecord{},
		checks:    config.checkEvery <= 1 || spec.index%config.checkEvery == 0,
	}
	sim.outcome = &visitorOutcome{spec: spec}
	r := newRand(config.seed, drawVisitor, uint64(spec.index))
	sim.access = drawAccessLink(r)
	sim.serving = make([]bool, len(w.pool))
	for i := range sim.serving {
		sim.serving[i] = r.Float64() < servingShare
	}
	sim.openSessions()

	for _, policy := range config.policies {
		sim.outcome.outcomes = append(sim.outcome.outcomes, sim.runPolicy(policy))
	}
	return sim.outcome
}

// The path's route stretch to a pool extender, drawn once per path.
func (self *visitorSim) stretch(poolIndex int) float64 {
	s, ok := self.stretches[poolIndex]
	if !ok {
		r := newRand(self.config.seed, drawPath, uint64(self.spec.index), uint64(poolIndex))
		s = drawStretch(r, self.config.truthKappa, self.config.stretchShape)
		self.stretches[poolIndex] = s
	}
	return s
}

// The propagation to a pool extender, ms: K · d(truth, its true place) times
// the path's stretch.
func (self *visitorSim) propagationMs(poolIndex int) float64 {
	d := lagdrift.DistanceKm(self.spec.truth, self.fleet[poolIndex].trueAt)
	return lagdrift.MsPerKm * d * (1 + self.stretch(poolIndex))
}

// Chooses each run's extenders (E2) and draws its link and sessions.
func (self *visitorSim) openSessions() {
	used := map[int]bool{}
	for run := 0; run < visitorRuns; run++ {
		candidates := []choiceCandidate{}
		for i, e := range self.world.pool {
			if !self.serving[i] {
				continue
			}
			at := self.fleet[i].at
			inCountry := e.site.code == self.spec.ipCode
			if self.spec.ipCode == "zz" {
				inCountry = self.spec.ipCountry.Contains(at)
			}
			candidates = append(candidates, choiceCandidate{
				index:      i,
				distanceKm: lagdrift.DistanceKm(self.spec.ipPlace, at),
				bearingDeg: lagdrift.BearingDeg(self.spec.ipPlace, at),
				inCountry:  inCountry,
				used:       used[i],
			})
		}
		c := chooseExtenders(candidates, newRand(self.config.seed, drawChoice, uint64(self.spec.index), uint64(run)))
		self.choices[run] = c
		r := newRand(self.config.seed, drawRun, uint64(self.spec.index), uint64(run))
		link := drawRunLink(&self.access, r)
		self.links[run] = link

		sessions := []*session{}
		spares := c.spares
		open := func(poolIndex int, afterMs float64) *session {
			sr := newRand(self.config.seed, drawSession, uint64(self.spec.index), uint64(run), uint64(poolIndex))
			s := &session{
				poolIndex: poolIndex,
				id:        self.world.pool[poolIndex].id,
				extender:  &self.fleet[poolIndex],
				openMs:    -1,
			}
			if sr.Float64() < sessionFailShare {
				return s
			}
			propagationMs := self.propagationMs(poolIndex)
			s.openMs = afterMs + sessionOpenMs(link, propagationMs, sr)
			for _, p := range sessionPings(link, propagationMs, s.openMs, sr) {
				if 0 < p.ms {
					s.answered = append(s.answered, p)
				}
			}
			return s
		}
		for _, poolIndex := range c.primaries {
			s := open(poolIndex, 0)
			sessions = append(sessions, s)
			used[poolIndex] = true
			// a session that hasn't opened by 4 s is replaced by the next spare
			if (s.openMs < 0 || spareAfterMs < s.openMs) && 0 < len(spares) {
				spare := open(spares[0], spareAfterMs)
				spares = spares[1:]
				sessions = append(sessions, spare)
				used[spare.poolIndex] = true
			}
		}
		self.sessions[run] = sessions
	}
}

// The runs of one policy.
func (self *visitorSim) runPolicy(policy stopPolicy) []runOutcome {
	outcomes := []runOutcome{}
	// the counting runs: their keys and usable observations
	countingKeys := []string{}
	countingObs := [][]lagdrift.Observation{}
	for run := 0; run < visitorRuns; run++ {
		windowKeys := lastN(countingKeys, windowRuns)
		windowObs := lastN(countingObs, windowRuns)
		prior := []lagdrift.Observation{}
		for _, obs := range windowObs {
			prior = append(prior, obs...)
		}
		trace := self.trace(run, strings.Join(windowKeys, ";")+fmt.Sprintf("|%d", policy.roundTrips), prior)
		tick, reason := trace.stopFor(policy)
		if self.checks && policy.isLagdrift() {
			trace.checkStops(tick)
		}
		obs := trace.serverObservations(tick, policy.roundTrips)
		outcome := runOutcome{
			stopMs:  int64(tick) * tickMs,
			reason:  reason,
			usable:  len(obs),
			counted: countingExtenders <= len(obs),
		}
		if outcome.counted {
			countingKeys = append(countingKeys, fmt.Sprintf("%d@%d", run, tick))
			countingObs = append(countingObs, obs)
		}
		windowKeys = lastN(countingKeys, windowRuns)
		windowObs = lastN(countingObs, windowRuns)
		outcome.verdict = self.verdict(strings.Join(windowKeys, ";")+fmt.Sprintf("|%d", policy.roundTrips), windowObs)
		outcomes = append(outcomes, outcome)
	}
	return outcomes
}

func lastN[T any](xs []T, n int) []T {
	if len(xs) <= n {
		return xs
	}
	return xs[len(xs)-n:]
}

// The run's trace given the counting runs before it, kept by key.
func (self *visitorSim) trace(run int, priorKey string, prior []lagdrift.Observation) *runTrace {
	key := fmt.Sprintf("%d|%s", run, priorKey)
	if t, ok := self.traces[key]; ok {
		return t
	}
	t := &runTrace{
		sim:            self,
		run:            run,
		sessions:       self.sessions[run],
		prior:          prior,
		estimateCounts: make([]int, len(self.sessions[run])),
		estimates:      make([]lagdrift.RoundTrip, len(self.sessions[run])),
	}
	self.traces[key] = t
	return t
}

// The frame's state after ping round j (1 to the cap), computed in order.
func (self *runTrace) tick(j int) *tickState {
	for len(self.ticks) < j {
		self.ticks = append(self.ticks, self.compute(len(self.ticks)+1))
	}
	return self.ticks[j-1]
}

func (self *runTrace) compute(j int) *tickState {
	atMs := int64(j) * tickMs
	state := &tickState{
		atMs:       atMs,
		roundTrips: make([]int, len(self.sessions)),
	}
	t := float64(atMs)
	obs := []lagdrift.Observation{}
	for i, s := range self.sessions {
		n := 0
		for n < len(s.answered) && s.answered[n].seenMs <= t {
			if state.lastPongMs < int64(s.answered[n].seenMs) {
				state.lastPongMs = int64(s.answered[n].seenMs)
			}
			n += 1
		}
		state.roundTrips[i] = n
		if n < lagdrift.MinRoundTripSamples {
			continue
		}
		if self.estimateCounts[i] != n {
			samples := make([]float64, n)
			for k := 0; k < n; k++ {
				samples[k] = s.answered[k].ms
			}
			estimate, ok := lagdrift.EstimateRoundTrip(samples)
			if !ok {
				continue
			}
			self.estimateCounts[i] = n
			self.estimates[i] = estimate
		}
		obs = append(obs, self.sim.observation(s, self.run, self.estimates[i]))
	}
	if fitExtenders <= len(obs) || fitExtenders <= distinctExtenders(self.prior) {
		all := append(append([]lagdrift.Observation{}, self.prior...), obs...)
		fit, err := self.sim.fit(all, self.warm)
		if err == nil {
			state.fit = fit
			self.warm = fit
		}
	}
	self.history = append(self.history, lagdrift.Snapshot{
		AtMs:       atMs,
		Fit:        state.fit,
		RoundTrips: state.roundTrips,
	})
	return state
}

func (self *visitorSim) observation(s *session, run int, estimate lagdrift.RoundTrip) lagdrift.Observation {
	return lagdrift.Observation{
		ExtenderId:   s.id,
		At:           s.extender.at,
		PlaceErrorKm: s.extender.placeErrorKm,
		Run:          run + 1,
		RoundTrip:    estimate,
	}
}

func distinctExtenders(obs []lagdrift.Observation) int {
	ids := map[string]bool{}
	for _, o := range obs {
		ids[o.ExtenderId] = true
	}
	return len(ids)
}

// Judge's D for the fit at ping round j, against the IP country.
func (self *runTrace) d(j int) float64 {
	state := self.tick(j)
	if !state.judged {
		state.judged = true
		state.d = math.Inf(1)
		if state.fit != nil {
			state.d = lagdrift.Judge(state.fit, self.sim.spec.ipCountry, lagdrift.DefaultConfidence).D
		}
	}
	return state.d
}

// Where a policy stops the run: the first ping round from 3 s at which a rule
// holds, in EvaluateStop's order, and the rule.
func (self *runTrace) stopFor(policy stopPolicy) (int, lagdrift.StopReason) {
	first := int(lagdrift.StopEarliestMs / tickMs)
	for j := first; j <= capTicks; j++ {
		state := self.tick(j)
		if lagdrift.StopInterruptedMs <= state.atMs-state.lastPongMs {
			return j, lagdrift.StopInterrupted
		}
		enough := state.fit != nil && enoughRoundTrips(state.roundTrips, policy.extenders, policy.roundTrips)
		if enough && policy.earlyK <= self.d(j) {
			return j, lagdrift.StopClear
		}
		if enough && policy.converged && steady(self.history[:j], state.atMs, state.fit, policy.windowMs, policy.areaRatio) {
			return j, lagdrift.StopConverged
		}
		if lagdrift.StopCapMs <= state.atMs {
			return j, lagdrift.StopCap
		}
	}
	return capTicks, lagdrift.StopCap
}

// At least extenders have roundTrips each.
func enoughRoundTrips(roundTrips []int, extenders int, need int) bool {
	count := 0
	for _, n := range roundTrips {
		if need <= n {
			count += 1
		}
	}
	return extenders <= count
}

// lagdrift's steadiness (V10) with the window and the area ratio as
// parameters: over the trailing window every snapshot has a fit, the largest
// area over the smallest is within the ratio, and every position is less than
// a quarter of the latest σ_minor from the latest.
func steady(history []lagdrift.Snapshot, nowMs int64, latest *lagdrift.Fit, windowMs int64, areaRatio float64) bool {
	start := -1
	for i, snapshot := range history {
		if snapshot.AtMs <= nowMs-windowMs {
			start = i
		}
	}
	if start < 0 {
		return false
	}
	minArea := math.Inf(1)
	maxArea := 0.0
	reachKm := latest.MinorKm / 4
	for _, snapshot := range history[start:] {
		if nowMs < snapshot.AtMs {
			break
		}
		if snapshot.Fit == nil {
			return false
		}
		area := snapshot.Fit.MajorKm * snapshot.Fit.MinorKm
		minArea = math.Min(minArea, area)
		maxArea = math.Max(maxArea, area)
		if !(math.Hypot(snapshot.Fit.EastKm-latest.EastKm, snapshot.Fit.NorthKm-latest.NorthKm) < reachKm) {
			return false
		}
	}
	return 0 < minArea && maxArea/minArea <= areaRatio
}

// Checks the study's stop against lagdrift.EvaluateStop at every ping round
// from 3 s to the stop.
func (self *runTrace) checkStops(stopTick int) {
	first := int(lagdrift.StopEarliestMs / tickMs)
	for j := first; j <= stopTick; j++ {
		state := self.tick(j)
		reason, ok := lagdrift.EvaluateStop(self.history[:j], 0, state.atMs, state.lastPongMs, self.sim.spec.ipCountry, lagdrift.BrowserStopTolerance)
		expectOk := j == stopTick
		self.sim.outcome.stopChecks += 1
		if ok != expectOk {
			self.sim.outcome.stopMismatches = append(self.sim.outcome.stopMismatches,
				fmt.Sprintf("visitor %d run %d at %d ms: lagdrift %v %q, study stops at %d ms", self.sim.spec.index, self.run+1, state.atMs, ok, reason, int64(stopTick)*tickMs))
		}
	}
}

// The server's usable observations when the sessions close after ping round
// j: each extender with roundTrips measured by then (E5).
func (self *runTrace) serverObservations(j int, roundTrips int) []lagdrift.Observation {
	closeMs := float64(j * tickMs)
	obs := []lagdrift.Observation{}
	for _, s := range self.sessions {
		samples := []float64{}
		for _, p := range s.answered {
			if closeMs < p.measuredMs {
				break
			}
			samples = append(samples, p.ms)
		}
		if len(samples) < roundTrips {
			continue
		}
		estimate, ok := lagdrift.EstimateRoundTrip(samples)
		if !ok {
			continue
		}
		obs = append(obs, self.sim.observation(s, self.run, estimate))
	}
	return obs
}

// The server's verdict over the window's counting runs (V6, V7), kept by key.
// Decide's rules at each confidence, with one region test and at most one
// physical check for all three.
func (self *visitorSim) verdict(key string, windowObs [][]lagdrift.Observation) *verdictRecord {
	if v, ok := self.verdicts[key]; ok {
		return v
	}
	v := &verdictRecord{counting: len(windowObs)}
	self.verdicts[key] = v
	if len(windowObs) == 0 {
		for i := range confidences {
			v.verdicts[i] = lagdrift.VerdictInconclusive
			v.reasons[i] = reasonTooFewExtenders
		}
		return v
	}
	obs := []lagdrift.Observation{}
	for _, o := range windowObs {
		obs = append(obs, o...)
	}
	c := self.spec.ipCountry
	fit, err := self.fit(obs, nil)
	if err != nil || !(fit.MajorKm <= lagdrift.DefaultMaxSigmaKm) {
		for i := range confidences {
			v.verdicts[i] = lagdrift.VerdictInconclusive
			v.reasons[i] = lagdrift.ReasonLowPrecision
		}
		if err == nil {
			v.recordFit(fit, self.spec.truth, math.NaN())
		}
		return v
	}
	j := lagdrift.Judge(fit, c, lagdrift.DefaultConfidence)
	v.fitted = true
	v.recordFit(fit, self.spec.truth, j.D)
	physical := -1
	for i, confidence := range confidences {
		k := lagdrift.KForConfidence(confidence)
		overlaps := j.Inside || j.D <= k
		if overlaps && physical < 0 {
			physical = 0
			if lagdrift.PhysicallyPossible(obs, c) {
				physical = 1
			}
		}
		switch {
		case overlaps && physical == 0:
			v.verdicts[i] = lagdrift.VerdictInconclusive
			v.reasons[i] = lagdrift.ReasonInconsistent
		case j.Inside:
			v.verdicts[i] = lagdrift.VerdictAgree
			v.reasons[i] = lagdrift.ReasonInsideIpCountry
		case overlaps:
			v.verdicts[i] = lagdrift.VerdictAgree
			v.reasons[i] = lagdrift.ReasonRegionOverlaps
		default:
			v.verdicts[i] = lagdrift.VerdictDisagree
			v.reasons[i] = lagdrift.ReasonRegionOutside
		}
	}
	if self.checks && !self.config.freeLastMile {
		verdict, reason, _ := lagdrift.Decide(fit, c, obs, lagdrift.DefaultConfidence, lagdrift.DefaultMaxSigmaKm)
		self.outcome.decideChecks += 1
		if verdict != v.verdicts[1] || reason != v.reasons[1] {
			self.outcome.decideMismatches = append(self.outcome.decideMismatches,
				fmt.Sprintf("visitor %d %s: lagdrift %s/%s, study %s/%s", self.spec.index, key, verdict, reason, v.verdicts[1], v.reasons[1]))
		}
	}
	return v
}

// lagdrift.FitPosition from the address's place, or, when the study emulates
// a change to V5 and a run's b is at its bound of 0, the same fit with the
// position's covariance taken with b left free. That covariance comes from
// the fit of the same round trips each 50 ms longer, which lifts every b off
// its bound and leaves the position's problem unchanged, started from this
// fit: its covariance is the full inverse's position block.
func (self *visitorSim) fit(obs []lagdrift.Observation, warm *lagdrift.Fit) (*lagdrift.Fit, error) {
	fit, err := lagdrift.FitPosition(self.model, self.spec.ipPlace, obs, warm)
	if err != nil || !self.config.freeLastMile || !atBound(fit) {
		return fit, err
	}
	const liftMs = 50
	lifted := make([]lagdrift.Observation, len(obs))
	for i, o := range obs {
		o.RoundTrip.Ms += liftMs
		if 0 < o.RoundTrip.MinMs {
			o.RoundTrip.MinMs += liftMs
		}
		lifted[i] = o
	}
	liftedWarm := &lagdrift.Fit{At: fit.At, Runs: fit.Runs}
	for _, b := range fit.LastMileMs {
		liftedWarm.LastMileMs = append(liftedWarm.LastMileMs, b+liftMs)
	}
	free, liftedErr := lagdrift.FitPosition(self.model, self.spec.ipPlace, lifted, liftedWarm)
	if liftedErr != nil {
		return fit, err
	}
	emulated := *fit
	emulated.Cov = free.Cov
	emulated.MajorKm, emulated.MinorKm, emulated.BearingDeg = lagdrift.CovEllipse(free.Cov)
	return &emulated, nil
}

// Some run's b is at its bound of 0.
func atBound(fit *lagdrift.Fit) bool {
	for _, b := range fit.LastMileMs {
		if b == 0 {
			return true
		}
	}
	return false
}

// P4's reason for a window with no counting run.
const reasonTooFewExtenders lagdrift.Reason = "too_few_extenders"

func (self *verdictRecord) recordFit(fit *lagdrift.Fit, truth lagdrift.Point, d float64) {
	self.d = d
	self.majorKm = fit.MajorKm
	self.minorKm = fit.MinorKm
	self.chi2 = fit.Chi2PerDof
	self.bAtZero = atBound(fit)
	east, north := lagdrift.AzimuthalKm(fit.At, truth)
	self.errorKm = math.Hypot(east, north)
	// (e n) Σ⁻¹ (e n)ᵀ
	a := fit.Cov[0][0]
	b := (fit.Cov[0][1] + fit.Cov[1][0]) / 2
	c := fit.Cov[1][1]
	det := a*c - b*b
	if 0 < det {
		self.truthM = math.Sqrt((c*east*east - 2*b*east*north + a*north*north) / det)
	} else {
		self.truthM = math.Inf(1)
	}
}
