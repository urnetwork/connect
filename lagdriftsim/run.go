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
	"slices"
	"strings"
	"time"

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
	// a custom client that goes silent at its first look whose region
	// touches the IP country at 99% once the run would count, or when clear
	// would stop it outside, ending the run as interrupted at a moment it
	// likes (review F-02)
	attack bool
}

// The attacker of the interrupted-stop gap, at V10 otherwise.
func attackPolicy() stopPolicy {
	p := v10Policy(lagdrift.EarlyK)
	p.name = "silent at a favourable look"
	p.attack = true
	return p
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
	return self.earlyK == lagdrift.EarlyK && self.converged && !self.attack &&
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
	// Judge's D against the IP country, and whether the fit is in it, once
	// asked for
	d      float64
	inside bool
	judged bool
	// under the cadence: the fit was the full multi-start, and its
	// observations, for the full fit before a stop
	full bool
	obs  []lagdrift.Observation
}

// One run's sessions, and what the frame sees at each ping round, given the
// runs before it that the verdict holds.
//
// Under V10's cadence the frame refits from the warm fit at each ping round
// and runs the full multi-start when the extenders in the fit change, at least
// every 2 s, and before any stop. When that last one lands in another basin,
// the stop is withdrawn and the steadiness window restarts from it: the run
// goes on in a fork of the trace, which every policy that would have stopped
// there shares.
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

	cadence bool
	// the steadiness window looks no further back than this history index
	historyFrom int
	// the last full fit: when, and over which extenders
	lastFullMs  int64
	lastFullSet string
	// the full fit before a stop at a ping round, and the fork it leads to
	// when it lands in another basin
	preStops map[int]*lagdrift.Fit
	forks    map[int]*runTrace
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
	fitChecks        fitChecks
	fitCosts         [fitKinds]fitCost
	// stops the full fit before them withdrew (V10's cadence)
	withdrawnStops int
	fitSamples     []fitSample
}

// The fit's own checks (the acceptance test of V5's starts): a warm refit
// of every verdict's fit, and, for the checked visitors, a grid of starts.
type fitChecks struct {
	refits int
	// refits that moved 1 km or more, and the farthest, km
	refitMoved   int
	refitMovedKm float64
	grids        int
	// grids that found a fixed point with a lower χ², and the largest drop
	gridLower     int
	gridLowerBy1  int
	gridLargestBy float64
	// of those lower by more than 1, the ones nearer the truth than the fit
	gridLowerNearer int
	examples        []string
}

func (self *fitChecks) add(other fitChecks) {
	self.refits += other.refits
	self.refitMoved += other.refitMoved
	self.refitMovedKm = math.Max(self.refitMovedKm, other.refitMovedKm)
	self.grids += other.grids
	self.gridLower += other.gridLower
	self.gridLowerBy1 += other.gridLowerBy1
	self.gridLargestBy = math.Max(self.gridLargestBy, other.gridLargestBy)
	self.gridLowerNearer += other.gridLowerNearer
	if len(self.examples) < 6 {
		self.examples = append(self.examples, other.examples[:min(len(other.examples), 6-len(self.examples))]...)
	}
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
	// the runs each visitor makes, at most visitorRuns
	runs int
	// the fits made so far, for sampling
	fitCalls int
}

func simulateVisitor(w *world, config *studyConfig, spec *visitorSpec) *visitorOutcome {
	sim := &visitorSim{
		world:     w,
		config:    config,
		spec:      spec,
		fleet:     w.fleets[spec.fleet],
		model:     lagdrift.Model{Kappa: modelKappaFor(config, spec.region), Sigma0Ms: lagdrift.DefaultSigma0Ms},
		stretches: map[int]float64{},
		traces:    map[string]*runTrace{},
		verdicts:  map[string]*verdictRecord{},
		checks:    config.checkEvery <= 1 || spec.index%config.checkEvery == 0,
		runs:      visitorRuns,
	}
	if 0 < config.runs && config.runs < visitorRuns {
		sim.runs = config.runs
	}
	sim.outcome = &visitorOutcome{spec: spec}
	r := newRand(config.seed, drawVisitor, uint64(spec.index))
	sim.access = drawAccessLink(r)
	if config.idealPings {
		sim.access = accessLink{kind: sim.access.kind, lastMileMs: sim.access.lastMileMs}
	}
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

// How this visitor's runs choose their extenders.
func (self *visitorSim) choiceOptions(candidates []choiceCandidate) choiceOptions {
	options := choiceOptions{mode: self.config.choiceMode}
	switch options.mode {
	case choiceHubs:
		// each hub's nearest serving extender, the hubs nearest the address's
		// place first
		hubs := slices.Clone(vpnHubs)
		slices.SortStableFunc(hubs, func(a place, b place) int {
			return compareFloat(lagdrift.DistanceKm(self.spec.ipPlace, a.at), lagdrift.DistanceKm(self.spec.ipPlace, b.at))
		})
		for _, hub := range hubs {
			best := -1
			bestKm := math.Inf(1)
			for _, c := range candidates {
				if km := lagdrift.DistanceKm(hub.at, self.fleet[c.index].at); km < bestKm {
					best = c.index
					bestKm = km
				}
			}
			if 0 <= best && bestKm < midMinKm {
				options.hubIndexes = append(options.hubIndexes, best)
			}
		}
	case choiceInformation:
		options.score = self.informationScore()
	case choiceInformationRing, choiceRingMid:
		options.score = self.informationRingScore()
	}
	return options
}

// A set's expected region, by the model's variances, averaged in log over
// plausible places for the visitor: the address's place (weight 1) and the VPN
// hubs (1/7 each). Lower is better. A place's region is the position block of
// the inverse information of (east, north, b), b free; a near extender's
// direction fades within 10 km of it, as lagdrift smooths its distance there.
func (self *visitorSim) informationScore() func(picked []choiceCandidate) float64 {
	places := []weighted{{at: self.spec.ipPlace, weight: 1}}
	for _, hub := range vpnHubs {
		places = append(places, weighted{at: hub.at, weight: 1 / float64(len(vpnHubs))})
	}
	return self.regionScore(places)
}

// informationScore's, over the address's place (weight 1) and a ring of 12
// places at each of 800 and 2,000 km around it (1/24 each).
func (self *visitorSim) informationRingScore() func(picked []choiceCandidate) float64 {
	places := []weighted{{at: self.spec.ipPlace, weight: 1}}
	for _, km := range []float64{800, 2000} {
		for k := 0; k < 12; k++ {
			places = append(places, weighted{at: destination(self.spec.ipPlace, 30*float64(k), km), weight: 1.0 / 24})
		}
	}
	return self.regionScore(places)
}

// A plausible place for the visitor and its weight.
type weighted struct {
	at     lagdrift.Point
	weight float64
}

// The weighted log of the region a set of extenders is expected to give at
// each place.
func (self *visitorSim) regionScore(places []weighted) func(picked []choiceCandidate) float64 {
	const measurementMs = 0.3
	return func(picked []choiceCandidate) float64 {
		total := 0.0
		for _, p := range places {
			var a [3][3]float64
			for _, c := range picked {
				e := self.fleet[c.index]
				d := lagdrift.DistanceKm(p.at, e.at)
				beta := lagdrift.BearingDeg(p.at, e.at) * radPerDeg
				fade := math.Min(1, d/10)
				j := [3]float64{-math.Sin(beta) * lagdrift.MsPerKm * fade, -math.Cos(beta) * lagdrift.MsPerKm * fade, 1}
				route := self.model.Kappa * lagdrift.MsPerKm * d
				placeMs := lagdrift.MsPerKm * lagdrift.HeldPlaceErrorKm(e.placeErrorKm)
				variance := measurementMs*measurementMs + self.model.Sigma0Ms*self.model.Sigma0Ms + route*route + placeMs*placeMs
				for u := 0; u < 3; u++ {
					for v := 0; v < 3; v++ {
						a[u][v] += j[u] * j[v] / variance
					}
				}
			}
			total += p.weight * positionLogDet(a)
		}
		return total
	}
}

// log det of the position block of a 3 × 3 information's inverse, or a large
// number when it is singular.
func positionLogDet(a [3][3]float64) float64 {
	det := a[0][0]*(a[1][1]*a[2][2]-a[1][2]*a[2][1]) - a[0][1]*(a[1][0]*a[2][2]-a[1][2]*a[2][0]) + a[0][2]*(a[1][0]*a[2][1]-a[1][1]*a[2][0])
	if !(1e-30 < math.Abs(det)) {
		return 1e6
	}
	// the inverse's position block is the adjugate's over det
	c00 := (a[1][1]*a[2][2] - a[1][2]*a[2][1]) / det
	c01 := -(a[0][1]*a[2][2] - a[0][2]*a[2][1]) / det
	c10 := -(a[1][0]*a[2][2] - a[1][2]*a[2][0]) / det
	c11 := (a[0][0]*a[2][2] - a[0][2]*a[2][0]) / det
	block := c00*c11 - c01*c10
	if !(0 < block) {
		return 1e6
	}
	return math.Log(block)
}

// The model's κ for a region's visitors.
func modelKappaFor(config *studyConfig, region string) float64 {
	if kappa, ok := config.regionModelKappas[region]; ok {
		return kappa
	}
	return config.modelKappa
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

// Where a pool extender is.
func (self *visitorSim) trueAt(poolIndex int) lagdrift.Point {
	if self.config.noPlaceError {
		return self.fleet[poolIndex].at
	}
	return self.fleet[poolIndex].trueAt
}

// The propagation to a pool extender, ms: K · d(truth, its true place) times
// the path's stretch.
func (self *visitorSim) propagationMs(poolIndex int) float64 {
	d := lagdrift.DistanceKm(self.spec.truth, self.trueAt(poolIndex))
	stretch := self.stretch(poolIndex)
	if self.config.routeMinKm != 0 || self.config.routeMaxKm != 0 {
		if d < self.config.routeMinKm || self.config.routeMaxKm <= d {
			stretch = 0
		}
	}
	return lagdrift.MsPerKm * d * (1 + stretch)
}

// Chooses each run's extenders (E2) and draws its link and sessions.
func (self *visitorSim) openSessions() {
	used := map[int]bool{}
	for run := 0; run < self.runs; run++ {
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
		c := chooseExtendersBy(candidates, self.choiceOptions(candidates), newRand(self.config.seed, drawChoice, uint64(self.spec.index), uint64(run)))
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
	for run := 0; run < self.runs; run++ {
		windowKeys := lastN(countingKeys, windowRuns)
		windowObs := lastN(countingObs, windowRuns)
		prior := []lagdrift.Observation{}
		for _, obs := range windowObs {
			prior = append(prior, obs...)
		}
		trace := self.trace(run, strings.Join(windowKeys, ";")+fmt.Sprintf("|%d", policy.roundTrips), prior)
		stopTrace, tick, reason := trace.stopFor(policy)
		if self.checks && policy.isLagdrift() && !trace.cadence {
			trace.checkStops(tick)
		}
		obs := trace.serverObservations(tick, policy.roundTrips)
		outcome := runOutcome{
			stopMs:  int64(tick) * tickMs,
			reason:  reason,
			usable:  len(obs),
			counted: countingExtenders <= len(obs),
		}
		if outcome.counted && self.config.interruptedRule && reason == lagdrift.StopInterrupted {
			// the silence began at the attacker's look, or at the last pong
			silence := tick
			if !policy.attack {
				silence = int(stopTrace.tick(tick).lastPongMs / tickMs)
			}
			outcome.counted = stopTrace.heldAt(policy, silence)
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
		cadence:        self.config.cadence,
		preStops:       map[int]*lagdrift.Fit{},
		forks:          map[int]*runTrace{},
	}
	self.traces[key] = t
	return t
}

// The extenders of observations, as a key.
func extenderSet(obs []lagdrift.Observation) string {
	ids := []string{}
	for _, o := range obs {
		if !slices.Contains(ids, o.ExtenderId) {
			ids = append(ids, o.ExtenderId)
		}
	}
	slices.Sort(ids)
	return strings.Join(ids, ",")
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
		var fit *lagdrift.Fit
		var err error
		if self.cadence {
			state.obs = all
			set := extenderSet(all)
			state.full = self.warm == nil || set != self.lastFullSet || lagdrift.StopWindowMs <= atMs-self.lastFullMs
			if state.full {
				fit, err = self.sim.fit(all, self.warm, false)
				self.lastFullMs = atMs
				self.lastFullSet = set
			} else {
				fit, err = self.sim.refit(all, self.warm)
			}
		} else {
			fit, err = self.sim.fit(all, self.warm, false)
		}
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
	placeErrorKm := s.extender.placeErrorKm
	if 0 < self.config.placeErrorPerKm {
		byDistance := self.config.placeErrorPerKm * lagdrift.DistanceKm(self.spec.ipPlace, s.extender.at)
		placeErrorKm = math.Max(lagdrift.HeldPlaceErrorKm(placeErrorKm), byDistance)
	}
	return lagdrift.Observation{
		ExtenderId:   s.id,
		At:           s.extender.at,
		PlaceErrorKm: placeErrorKm,
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
			j := lagdrift.Judge(state.fit, self.sim.spec.ipCountry, lagdrift.DefaultConfidence)
			state.d = j.D
			state.inside = j.Inside
		}
	}
	return state.d
}

// Where a policy stops the run: the first ping round from 3 s at which a rule
// holds, in EvaluateStop's order, and the rule. Under the cadence, a stop on a
// warm refit first runs the full fit, and goes on in a fork when that lands in
// another basin.
func (self *runTrace) stopFor(policy stopPolicy) (*runTrace, int, lagdrift.StopReason) {
	trace := self
	first := int(lagdrift.StopEarliestMs / tickMs)
	for j := first; j <= capTicks; j++ {
		// the attacker waits until the run would count, 6 extenders with 12
		// round trips (E5), then goes silent at a look that touches
		if state := trace.tick(j); policy.attack && state.fit != nil && enoughRoundTrips(state.roundTrips, countingExtenders, policy.roundTrips) && trace.touches(j) {
			return trace, j, lagdrift.StopInterrupted
		}
		reason, ok := trace.rule(policy, j)
		if !ok {
			continue
		}
		if policy.attack && reason == lagdrift.StopClear {
			// clear outside: the client goes silent rather than be rejected
			return trace, j, lagdrift.StopInterrupted
		}
		if trace.cadence && (reason == lagdrift.StopClear || reason == lagdrift.StopConverged) && !trace.tick(j).full {
			if fork := trace.forkAt(j); fork != nil {
				self.sim.outcome.withdrawnStops += 1
				trace = fork
				continue
			}
		}
		return trace, j, reason
	}
	return trace, capTicks, lagdrift.StopCap
}

// The region at 99% touches the IP country at ping round j: the fit is in it
// or D is within k.
func (self *runTrace) touches(j int) bool {
	d := self.d(j)
	return self.tick(j).inside || d <= lagdrift.KForConfidence(lagdrift.DefaultConfidence)
}

// converged or clear held at ping round j by the server's check, with its
// wider tolerance (V10): what an interrupted run needs to count under the
// review's rule.
func (self *runTrace) heldAt(policy stopPolicy, j int) bool {
	if j < int(lagdrift.StopEarliestMs/tickMs) {
		return false
	}
	server := policy
	server.attack = false
	server.areaRatio = lagdrift.ServerStopTolerance.AreaRatio
	reason, ok := self.rule(server, j)
	return ok && (reason == lagdrift.StopConverged || reason == lagdrift.StopClear)
}

// The rule that holds at ping round j under a policy, if one does.
func (self *runTrace) rule(policy stopPolicy, j int) (lagdrift.StopReason, bool) {
	state := self.tick(j)
	if lagdrift.StopInterruptedMs <= state.atMs-state.lastPongMs {
		return lagdrift.StopInterrupted, true
	}
	enough := state.fit != nil && enoughRoundTrips(state.roundTrips, policy.extenders, policy.roundTrips)
	if enough && !math.IsInf(policy.earlyK, 1) && policy.earlyK <= self.d(j) {
		return lagdrift.StopClear, true
	}
	if enough && policy.converged && steady(self.history[self.historyFrom:j], state.atMs, state.fit, policy.windowMs, policy.areaRatio) {
		return lagdrift.StopConverged, true
	}
	if lagdrift.StopCapMs <= state.atMs {
		return lagdrift.StopCap, true
	}
	return "", false
}

// A fork lands in another basin farther than this from the refit, km.
const basinKm = 1

// The fork the full fit before a stop at ping round j leads to, or nil when it
// stays in the refit's basin.
func (self *runTrace) forkAt(j int) *runTrace {
	if fork, ok := self.forks[j]; ok {
		return fork
	}
	state := self.tick(j)
	full, ok := self.preStops[j]
	if !ok {
		var err error
		full, err = self.sim.fit(state.obs, state.fit, false)
		if err != nil {
			full = nil
		}
		self.preStops[j] = full
	}
	if full == nil || lagdrift.DistanceKm(full.At, state.fit.At) <= basinKm {
		self.forks[j] = nil
		return nil
	}
	forkState := *state
	forkState.fit = full
	forkState.full = true
	forkState.judged = false
	fork := &runTrace{
		sim:            self.sim,
		run:            self.run,
		sessions:       self.sessions,
		prior:          self.prior,
		ticks:          append(slices.Clone(self.ticks[:j-1]), &forkState),
		history:        append(slices.Clone(self.history[:j-1]), lagdrift.Snapshot{AtMs: state.atMs, Fit: full, RoundTrips: state.roundTrips}),
		warm:           full,
		estimateCounts: slices.Clone(self.estimateCounts),
		estimates:      slices.Clone(self.estimates),
		cadence:        true,
		historyFrom:    j - 1,
		lastFullMs:     state.atMs,
		lastFullSet:    extenderSet(state.obs),
		preStops:       map[int]*lagdrift.Fit{},
		forks:          map[int]*runTrace{},
	}
	self.forks[j] = fork
	return fork
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
	fit, err := self.fit(obs, nil, true)
	if err == nil && self.config.wideStarts {
		fit = self.widestFit(obs, fit)
	}
	if err != nil || !(fit.MajorKm <= lagdrift.DefaultMaxSigmaKm) {
		for i := range confidences {
			v.verdicts[i] = lagdrift.VerdictInconclusive
			v.reasons[i] = lagdrift.ReasonLowPrecision
		}
		if err == nil {
			// no region test: D is +Inf, as Judge gives with no country
			v.recordFit(fit, self.spec.truth, math.Inf(1))
		}
		return v
	}
	self.checkFit(key, obs, fit)
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
	if self.checks {
		verdict, reason, _ := lagdrift.Decide(fit, c, obs, lagdrift.DefaultConfidence, lagdrift.DefaultMaxSigmaKm)
		self.outcome.decideChecks += 1
		if verdict != v.verdicts[1] || reason != v.reasons[1] {
			self.outcome.decideMismatches = append(self.outcome.decideMismatches,
				fmt.Sprintf("visitor %d %s: lagdrift %s/%s, study %s/%s", self.spec.index, key, verdict, reason, v.verdicts[1], v.reasons[1]))
		}
	}
	return v
}

// Who fits and what: the frame after a ping round or the server for a
// verdict, over one run or a pool, by the full multi-start (FitPosition) or
// the warm refit (V10).
type fitKind int

const (
	fitFrameSingle fitKind = iota
	fitFramePooled
	fitServerSingle
	fitServerPooled
	fitRefitSingle
	fitRefitPooled
	fitKinds
)

var fitKindNames = [fitKinds]string{
	"the frame's full fit, one run",
	"the frame's full fit, pooled runs",
	"the server's fit, one run",
	"the server's fit, pooled runs",
	"the frame's warm refit, one run",
	"the frame's warm refit, pooled runs",
}

func fitKindOf(server bool, obs []lagdrift.Observation) fitKind {
	pooled := false
	for _, o := range obs {
		if o.Run != obs[0].Run {
			pooled = true
			break
		}
	}
	switch {
	case server && pooled:
		return fitServerPooled
	case server:
		return fitServerSingle
	case pooled:
		return fitFramePooled
	default:
		return fitFrameSingle
	}
}

// V10's warm refit. A variable so the study's parts can tell whether the
// cadence is there to run.
var refitPosition = lagdrift.RefitPosition

// The frame's warm refit, timed.
func (self *visitorSim) refit(obs []lagdrift.Observation, warm *lagdrift.Fit) (*lagdrift.Fit, error) {
	kind := fitRefitSingle
	if fitKindOf(false, obs) == fitFramePooled {
		kind = fitRefitPooled
	}
	self.sampleFit(kind, obs, warm)
	start := time.Now()
	f, err := refitPosition(self.model, self.spec.ipPlace, obs, warm)
	self.outcome.fitCosts[kind].add(time.Since(start))
	return f, err
}

// lagdrift.FitPosition from the address's place, timed: the frame fits after
// every ping round, so what a fit costs is what the widget costs.
func (self *visitorSim) fit(obs []lagdrift.Observation, warm *lagdrift.Fit, server bool) (*lagdrift.Fit, error) {
	kind := fitKindOf(server, obs)
	self.sampleFit(kind, obs, warm)
	start := time.Now()
	f, err := lagdrift.FitPosition(self.model, self.spec.ipPlace, obs, warm)
	self.outcome.fitCosts[kind].add(time.Since(start))
	return f, err
}

// One fit's inputs, kept to time it again once the study is done.
type fitSample struct {
	kind     fitKind
	model    lagdrift.Model
	expected lagdrift.Point
	obs      []lagdrift.Observation
	warm     *lagdrift.Fit
}

// Every this many fits, by a visitor's own count, is sampled for timing.
const fitSampleEvery = 29

// Keeps a sample of the fits' inputs, chosen by the visitor and its count of
// fits so the samples are the same in every run of the study.
func (self *visitorSim) sampleFit(kind fitKind, obs []lagdrift.Observation, warm *lagdrift.Fit) {
	self.fitCalls += 1
	if (uint64(self.spec.index)*2654435761+uint64(self.fitCalls))%fitSampleEvery != 0 {
		return
	}
	self.outcome.fitSamples = append(self.outcome.fitSamples, fitSample{
		kind:     kind,
		model:    self.model,
		expected: self.spec.ipPlace,
		obs:      obs,
		warm:     warm,
	})
}

// What the fits cost: how many, and their durations in buckets that double
// from 0.25 ms.
type fitCost struct {
	fits    int
	total   time.Duration
	buckets [16]int
}

func (self *fitCost) add(d time.Duration) {
	self.fits += 1
	self.total += d
	bucket := 0
	for limit := 250 * time.Microsecond; limit < d && bucket < len(self.buckets)-1; limit *= 2 {
		bucket += 1
	}
	self.buckets[bucket] += 1
}

func (self *fitCost) merge(other fitCost) {
	self.fits += other.fits
	self.total += other.total
	for i := range self.buckets {
		self.buckets[i] += other.buckets[i]
	}
}

// The duration below which a share of the fits ended: a bucket's upper limit.
func (self *fitCost) quantile(q float64) time.Duration {
	need := int(math.Ceil(q * float64(self.fits)))
	seen := 0
	limit := 250 * time.Microsecond
	for _, n := range self.buckets {
		seen += n
		if need <= seen {
			return limit
		}
		limit *= 2
	}
	return limit
}

// The fit's χ², summed.
func chi2Of(fit *lagdrift.Fit) float64 {
	return fit.Chi2PerDof * float64(max(1, fit.Observations-2-len(fit.Runs)))
}

// Refits from fit until a refit moves it less than 0.1 km, at most 4 times.
func (self *visitorSim) converge(obs []lagdrift.Observation, fit *lagdrift.Fit) (*lagdrift.Fit, bool) {
	for i := 0; i < 4; i++ {
		next, err := lagdrift.FitPosition(self.model, self.spec.ipPlace, obs, fit)
		if err != nil {
			return fit, false
		}
		moved := lagdrift.DistanceKm(fit.At, next.At)
		fit = next
		if moved < 0.1 {
			return fit, true
		}
	}
	return fit, false
}

// Checks a checked visitor's verdict fit: a warm refit from it moves under
// 1 km, and, for a third of them, no start on a grid converges to a fixed
// point with a lower χ². The grid is the address's place, the three fastest extenders' places,
// and 6 bearings at 300, 1,000 and 3,000 km from the fit, each starting with
// b at 0.7 times its run's lowest round trip, as a cold fit does.
func (self *visitorSim) checkFit(key string, obs []lagdrift.Observation, fit *lagdrift.Fit) {
	if !self.checks {
		return
	}
	checks := &self.outcome.fitChecks
	refit, err := lagdrift.FitPosition(self.model, self.spec.ipPlace, obs, fit)
	if err == nil {
		checks.refits += 1
		moved := lagdrift.DistanceKm(fit.At, refit.At)
		if 1 <= moved {
			checks.refitMoved += 1
		}
		checks.refitMovedKm = math.Max(checks.refitMovedKm, moved)
	}
	// the grid on a third of the checked visitors' single runs
	if self.spec.index%(3*max(1, self.config.checkEvery)) != 0 || 1 < len(fit.Runs) {
		return
	}
	starts := []lagdrift.Point{self.spec.ipPlace}
	fastest := slices.Clone(obs)
	slices.SortFunc(fastest, func(a lagdrift.Observation, b lagdrift.Observation) int {
		return compareFloat(a.RoundTrip.Ms, b.RoundTrip.Ms)
	})
	for _, o := range fastest[:min(3, len(fastest))] {
		starts = append(starts, o.At)
	}
	for _, km := range []float64{300, 1000, 3000} {
		for k := 0; k < 6; k++ {
			starts = append(starts, destination(fit.At, 60*float64(k)+20, km))
		}
	}
	coldLastMile := map[int]float64{}
	for _, o := range obs {
		if b, ok := coldLastMile[o.Run]; !ok || o.RoundTrip.Ms < b {
			coldLastMile[o.Run] = o.RoundTrip.Ms
		}
	}
	lastMile := []float64{}
	for _, run := range fit.Runs {
		lastMile = append(lastMile, 0.7*coldLastMile[run])
	}
	checks.grids += 1
	best := 0.0
	var bestFit *lagdrift.Fit
	for _, start := range starts {
		found, ok := self.converge(obs, &lagdrift.Fit{At: start, LastMileMs: slices.Clone(lastMile), Runs: fit.Runs})
		if !ok {
			continue
		}
		if drop := chi2Of(fit) - chi2Of(found); best < drop {
			best = drop
			bestFit = found
		}
	}
	if 1e-3 < best {
		checks.gridLower += 1
		fitTruthKm := lagdrift.DistanceKm(fit.At, self.spec.truth)
		lowerTruthKm := lagdrift.DistanceKm(bestFit.At, self.spec.truth)
		if 1 < best {
			checks.gridLowerBy1 += 1
			if lowerTruthKm < fitTruthKm {
				checks.gridLowerNearer += 1
			}
		}
		checks.gridLargestBy = math.Max(checks.gridLargestBy, best)
		if len(checks.examples) < 6 && 1 < best {
			checks.examples = append(checks.examples, fmt.Sprintf("visitor %d (%s, %s) %s: χ² %.2f at %.3f,%.3f, %.0f km from the truth; a start converges to χ² %.2f at %.3f,%.3f, %.0f km away and %.0f km from the truth",
				self.spec.index, self.spec.region, self.spec.city.name, key, chi2Of(fit), fit.At.Lat, fit.At.Lon, fitTruthKm, chi2Of(bestFit), bestFit.At.Lat, bestFit.At.Lon, lagdrift.DistanceKm(fit.At, bestFit.At), lowerTruthKm))
		}
	}
}

// The fit of the lowest χ² among fit and the fixed points a grid of starts
// settles in, when lower than fit's by more than 1, V5's margin: starts at 300,
// 1,000 and 3,000 km in 6 bearings around the fit and around the address's
// place, each with b at 0.7 times its run's lowest round trip.
func (self *visitorSim) widestFit(obs []lagdrift.Observation, fit *lagdrift.Fit) *lagdrift.Fit {
	coldLastMile := map[int]float64{}
	for _, o := range obs {
		if b, ok := coldLastMile[o.Run]; !ok || o.RoundTrip.Ms < b {
			coldLastMile[o.Run] = o.RoundTrip.Ms
		}
	}
	lastMile := []float64{}
	for _, run := range fit.Runs {
		lastMile = append(lastMile, 0.7*coldLastMile[run])
	}
	best := fit
	for _, center := range []lagdrift.Point{fit.At, self.spec.ipPlace} {
		for _, km := range []float64{300, 1000, 3000} {
			for k := 0; k < 6; k++ {
				start := destination(center, 60*float64(k)+20, km)
				found, ok := self.converge(obs, &lagdrift.Fit{At: start, LastMileMs: slices.Clone(lastMile), Runs: fit.Runs})
				if ok && chi2Of(found) < chi2Of(best)-1 {
					best = found
				}
			}
		}
	}
	return best
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
