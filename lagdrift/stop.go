package lagdrift

// Stopping a run early (lag.dev/DRIFTTEST.md V10). The ellipse is set by how
// many paths there are, not how many round trips, so a run stops once more
// pinging can't change the answer. The frame evaluates the rules on its own
// fit after every ping round (250 ms), and the server evaluates the same rules
// on the signed reports at the moment the sessions closed, with wider
// tolerances.
//
//   - converged: at least 8 extenders are usable, with 8 or more round trips
//     each (E5's usable_round_trips, 2 s of pings), and over the trailing 2 s
//     the ellipse's area (σ_major · σ_minor) has stayed within the
//     tolerance's ratio and the position has moved less than a quarter of
//     σ_minor;
//   - clear: the same usable minimum, and the whole region at the run's
//     k_early (StopTolerance.KEarly, V11) lies outside the IP country, or the
//     fit is inside it and the whole region lies inside it. Both are
//     D ≥ k_early (V6). A tolerance without a k_early allows no clear stop;
//     falling back to EarlyK would make one easier than a verdict at a k
//     above 3.035 (review V-4);
//   - interrupted: no pong from any extender for 4 s;
//   - cap: 12 s after the sessions opened.
//
// Nothing stops a run in its first 3 s.

import (
	"math"
)

// lag.dev/DRIFTTEST.md V10
const (
	// no rule stops a run before this, ms after the sessions opened
	StopEarliestMs = 3000
	// converged looks back this far, ms
	StopWindowMs = 2000
	// interrupted: no pong for this long, ms
	StopInterruptedMs = 4000
	// cap: this long after the sessions opened, ms
	StopCapMs = 12000
	// converged and clear need this many usable extenders, each with at
	// least StopRoundTrips round trips: E5's usable minimum, 8 since T3
	// found it changes no rate and saves about a second when far sessions
	// open late (2026-11-17; 12 before)
	StopExtenders  = 8
	StopRoundTrips = 8
)

// The run's state after a ping round.
type Snapshot struct {
	// when, ms, on the clock of openedMs and nowMs
	AtMs int64 `json:"atMs"`
	// the fit then, or nil before the first
	Fit *Fit `json:"fit"`
	// round trips so far, per extender
	RoundTrips []int `json:"roundTrips"`
}

// Why a run stopped (V10, the `stop` of P5).
type StopReason string

const (
	StopConverged   StopReason = "converged"
	StopClear       StopReason = "clear"
	StopInterrupted StopReason = "interrupted"
	StopCap         StopReason = "cap"
)

// How far converged lets the ellipse's area move, the largest area over the
// window divided by the smallest, and the run's k_early for clear.
type StopTolerance struct {
	AreaRatio float64 `json:"areaRatio"`
	// lag.dev/DRIFTTEST.md I3 (V11): the run's k_early (KEarly of its k), for
	// the clear rule; unset (0), or anything not positive, allows no clear
	// stop (review V-4)
	KEarly float64 `json:"kEarly"`
}

// The clear rule's k: the tolerance's KEarly, and false when it has none.
func (self StopTolerance) kEarly() (float64, bool) {
	if 0 < self.KEarly {
		return self.KEarly, true
	}
	return 0, false
}

// The browser's and the server's area ratios. Neither carries a k_early, which
// is the run's: set KEarly to KEarly(k) for its k, or clear never holds.
var (
	// ±5%: 1.05 / 0.95
	BrowserStopTolerance = StopTolerance{AreaRatio: 1.105}
	// ±8%, since the browser's and the extenders' clocks see the pings
	// slightly apart (V10)
	ServerStopTolerance = StopTolerance{AreaRatio: 1.17}
)

// The rule that stops the run at nowMs, if one does (V10). history is the
// run's snapshots in time order; those after nowMs are ignored. lastPongMs is
// the last pong from any extender, before which openedMs stands in. c is the IP
// country, nil when unknown, which rules out clear, as a country without a
// ring with a point does, and as a tolerance without the run's k_early does.
// When several rules hold, the first of interrupted, clear, converged and cap
// is the reason.
func EvaluateStop(history []Snapshot, openedMs int64, nowMs int64, lastPongMs int64, c *Country, tol StopTolerance) (StopReason, bool) {
	for _, reason := range []StopReason{StopInterrupted, StopClear, StopConverged, StopCap} {
		if StopHolds(reason, history, openedMs, nowMs, lastPongMs, c, tol) {
			return reason, true
		}
	}
	return "", false
}

// The rule holds at nowMs, as EvaluateStop evaluates it. An extension of
// lag.dev/DRIFTTEST.md I3, for the server's check of the stop a frame
// reported (E5).
func StopHolds(reason StopReason, history []Snapshot, openedMs int64, nowMs int64, lastPongMs int64, c *Country, tol StopTolerance) bool {
	if nowMs < openedMs+StopEarliestMs {
		return false
	}
	switch reason {
	case StopInterrupted:
		return max(lastPongMs, openedMs)+StopInterruptedMs <= nowMs
	case StopCap:
		return openedMs+StopCapMs <= nowMs
	case StopClear:
		latest, ok := latestSnapshot(history, nowMs)
		if !ok || latest.Fit == nil || c == nil || !c.hasBoundary() || !enoughRoundTrips(latest.RoundTrips) {
			return false
		}
		kEarly, ok := tol.kEarly()
		if !ok {
			return false
		}
		return kEarly <= Judge(latest.Fit, c, kEarly).D
	case StopConverged:
		latest, ok := latestSnapshot(history, nowMs)
		if !ok || latest.Fit == nil || !enoughRoundTrips(latest.RoundTrips) {
			return false
		}
		return steady(history, nowMs, latest.Fit, tol)
	default:
		return false
	}
}

func latestSnapshot(history []Snapshot, nowMs int64) (Snapshot, bool) {
	for i := len(history) - 1; 0 <= i; i -= 1 {
		if history[i].AtMs <= nowMs {
			return history[i], true
		}
	}
	return Snapshot{}, false
}

// At least StopExtenders extenders are usable: StopRoundTrips or more round
// trips each (E5).
func enoughRoundTrips(roundTrips []int) bool {
	count := 0
	for _, n := range roundTrips {
		if StopRoundTrips <= n {
			count += 1
		}
	}
	return StopExtenders <= count
}

// Over the trailing 2 s, every snapshot has a fit, the largest area over the
// smallest is within the tolerance, and every position is less than a quarter
// of the latest σ_minor from the latest. The window reaches back to the last
// snapshot at or before nowMs − 2 s, so a history shorter than 2 s is never
// steady.
func steady(history []Snapshot, nowMs int64, latest *Fit, tol StopTolerance) bool {
	start := -1
	for i, snapshot := range history {
		if snapshot.AtMs <= nowMs-StopWindowMs {
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
	return 0 < minArea && maxArea/minArea <= tol.AreaRatio
}
