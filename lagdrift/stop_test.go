package lagdrift

import (
	"testing"
)

const testOpenedMs = 1_000_000

// A fit at a place, its offset from the expected place, and a round ellipse
// of sigmaKm, as a snapshot reads one.
func stopFit(at Point, eastKm float64, northKm float64, sigmaKm float64) *Fit {
	f := fitAt(at, circleCov(sigmaKm))
	f.EastKm = eastKm
	f.NorthKm = northKm
	return f
}

// Round trips so far: n extenders with 12 each, the rest with 5.
func roundTripsReady(n int) []int {
	counts := make([]int, 12)
	for i := range counts {
		counts[i] = 5
		if i < n {
			counts[i] = 12 + i
		}
	}
	return counts
}

// A snapshot every 250 ms from 1 s after the sessions opened through untilMs,
// each from fitAt(its time).
func stopHistory(untilMs int64, fitAt func(atMs int64) *Fit, ready int) []Snapshot {
	history := []Snapshot{}
	for t := int64(testOpenedMs + 1000); t <= untilMs; t += 250 {
		history = append(history, Snapshot{AtMs: t, Fit: fitAt(t), RoundTrips: roundTripsReady(ready)})
	}
	return history
}

// A steady fit inside the square country but near its edge, so clear doesn't
// hold: σ 40 km, 55 km from the west edge.
func steadyFit(int64) *Fit {
	return stopFit(Point{Lat: 1, Lon: 0.5}, 20, 10, 40)
}

func TestEvaluateStopWaitsThreeSeconds(t *testing.T) {
	far := func(int64) *Fit { return stopFit(Point{Lat: 1, Lon: -10}, 0, 0, 10) }
	history := stopHistory(testOpenedMs+3000, far, 12)
	if reason, ok := EvaluateStop(history, testOpenedMs, testOpenedMs+2999, testOpenedMs+2999, squareCountry, BrowserStopTolerance); ok {
		t.Errorf("stopped %s before 3 s", reason)
	}
	if reason, ok := EvaluateStop(history, testOpenedMs, testOpenedMs+3000, testOpenedMs+3000, squareCountry, BrowserStopTolerance); !ok || reason != StopClear {
		t.Errorf("at 3 s: %s, %v", reason, ok)
	}
}

func TestEvaluateStopCap(t *testing.T) {
	// pinging along, with no fit yet
	history := stopHistory(testOpenedMs+12000, func(int64) *Fit { return nil }, 0)
	if reason, ok := EvaluateStop(history, testOpenedMs, testOpenedMs+11999, testOpenedMs+11900, squareCountry, BrowserStopTolerance); ok {
		t.Errorf("stopped %s before the cap", reason)
	}
	if reason, ok := EvaluateStop(history, testOpenedMs, testOpenedMs+12000, testOpenedMs+11900, squareCountry, BrowserStopTolerance); !ok || reason != StopCap {
		t.Errorf("at the cap: %s, %v", reason, ok)
	}
}

func TestEvaluateStopInterrupted(t *testing.T) {
	history := stopHistory(testOpenedMs+6000, func(int64) *Fit { return nil }, 0)
	if reason, ok := EvaluateStop(history, testOpenedMs, testOpenedMs+4999, testOpenedMs+1000, squareCountry, BrowserStopTolerance); ok {
		t.Errorf("stopped %s 3.999 s after the last pong", reason)
	}
	if reason, ok := EvaluateStop(history, testOpenedMs, testOpenedMs+5000, testOpenedMs+1000, squareCountry, BrowserStopTolerance); !ok || reason != StopInterrupted {
		t.Errorf("4 s after the last pong: %s, %v", reason, ok)
	}
	// no pong at all: the sessions' opening stands in
	if reason, ok := EvaluateStop(nil, testOpenedMs, testOpenedMs+4000, 0, nil, BrowserStopTolerance); !ok || reason != StopInterrupted {
		t.Errorf("no pong in 4 s: %s, %v", reason, ok)
	}
}

func TestEvaluateStopConverged(t *testing.T) {
	now := int64(testOpenedMs + 3500)
	history := stopHistory(now, steadyFit, 8)
	if reason, ok := EvaluateStop(history, testOpenedMs, now, now, squareCountry, BrowserStopTolerance); !ok || reason != StopConverged {
		t.Errorf("a steady fit: %s, %v", reason, ok)
	}
	// 7 extenders with 12 round trips aren't enough
	if reason, ok := EvaluateStop(stopHistory(now, steadyFit, 7), testOpenedMs, now, now, squareCountry, BrowserStopTolerance); ok {
		t.Errorf("7 extenders ready: %s", reason)
	}
	// a history shorter than 2 s isn't steady
	if reason, ok := EvaluateStop(history[len(history)-8:], testOpenedMs, now, now, squareCountry, BrowserStopTolerance); ok {
		t.Errorf("1.75 s of history: %s", reason)
	}
	// a snapshot without a fit in the window
	gap := stopHistory(now, steadyFit, 8)
	gap[len(gap)-3].Fit = nil
	if reason, ok := EvaluateStop(gap, testOpenedMs, now, now, squareCountry, BrowserStopTolerance); ok {
		t.Errorf("a gap in the window: %s", reason)
	}
	// the position moved 12 km, more than a quarter of σ_minor (40 km)
	moving := stopHistory(now, func(atMs int64) *Fit {
		f := steadyFit(atMs)
		if atMs < now-1000 {
			f.EastKm += 12
		}
		return f
	}, 8)
	if reason, ok := EvaluateStop(moving, testOpenedMs, now, now, squareCountry, BrowserStopTolerance); ok {
		t.Errorf("a moving fit: %s", reason)
	}
}

// The area may move by the tolerance's ratio: the browser's ±5% and the
// server's wider ±8%.
func TestEvaluateStopAreaTolerance(t *testing.T) {
	now := int64(testOpenedMs + 3500)
	shrinking := func(ratio float64) []Snapshot {
		return stopHistory(now, func(atMs int64) *Fit {
			f := steadyFit(atMs)
			if atMs < now-1000 {
				f.MajorKm *= ratio
			}
			return f
		}, 8)
	}
	cases := []struct {
		ratio float64
		tol   StopTolerance
		stop  bool
	}{
		{ratio: 1.10, tol: BrowserStopTolerance, stop: true},
		{ratio: 1.11, tol: BrowserStopTolerance, stop: false},
		{ratio: 1.15, tol: ServerStopTolerance, stop: true},
		{ratio: 1.18, tol: ServerStopTolerance, stop: false},
	}
	for _, c := range cases {
		_, ok := EvaluateStop(shrinking(c.ratio), testOpenedMs, now, now, squareCountry, c.tol)
		if ok != c.stop {
			t.Errorf("an area ratio of %.2f with %.3f: stop %v", c.ratio, c.tol.AreaRatio, ok)
		}
		if StopHolds(StopConverged, shrinking(c.ratio), testOpenedMs, now, now, squareCountry, c.tol) != c.stop {
			t.Errorf("StopHolds disagrees at %.2f with %.3f", c.ratio, c.tol.AreaRatio)
		}
	}
}

// Clear: the EarlyK region wholly outside the country, or the fit inside and
// the region wholly inside, once 8 extenders are ready.
func TestEvaluateStopClear(t *testing.T) {
	now := int64(testOpenedMs + 3000)
	large := &Country{Code: "zl", Polygons: [][][]Point{{boxRing(-20, -20, 20, 20)}}}
	cases := []struct {
		name  string
		fit   *Fit
		c     *Country
		ready int
		clear bool
	}{
		{name: "far outside", fit: stopFit(Point{Lat: 1, Lon: -10}, 0, 0, 30), c: squareCountry, ready: 8, clear: true},
		{name: "deep inside", fit: stopFit(Point{Lat: 0, Lon: 0}, 0, 0, 30), c: large, ready: 8, clear: true},
		{name: "straddling", fit: stopFit(Point{Lat: 1, Lon: -0.3}, 0, 0, 30), c: squareCountry, ready: 8, clear: false},
		{name: "inside near the edge", fit: stopFit(Point{Lat: 1, Lon: 0.3}, 0, 0, 30), c: squareCountry, ready: 8, clear: false},
		{name: "too few ready", fit: stopFit(Point{Lat: 1, Lon: -10}, 0, 0, 30), c: squareCountry, ready: 7, clear: false},
		{name: "no country", fit: stopFit(Point{Lat: 1, Lon: -10}, 0, 0, 30), c: nil, ready: 8, clear: false},
	}
	for _, c := range cases {
		// one snapshot, so converged can't hold
		history := []Snapshot{{AtMs: now, Fit: c.fit, RoundTrips: roundTripsReady(c.ready)}}
		reason, ok := EvaluateStop(history, testOpenedMs, now, now, c.c, BrowserStopTolerance)
		if (ok && reason == StopClear) != c.clear {
			t.Errorf("%s: %s, %v", c.name, reason, ok)
		}
	}
}

// When several rules hold, interrupted comes first, then clear, converged and
// the cap.
func TestEvaluateStopOrder(t *testing.T) {
	far := func(int64) *Fit { return stopFit(Point{Lat: 1, Lon: -10}, 0, 0, 30) }
	now := int64(testOpenedMs + 12000)
	history := stopHistory(now, far, 12)
	if reason, _ := EvaluateStop(history, testOpenedMs, now, testOpenedMs+5000, squareCountry, BrowserStopTolerance); reason != StopInterrupted {
		t.Errorf("interrupted and clear: %s", reason)
	}
	if reason, _ := EvaluateStop(history, testOpenedMs, now, now, squareCountry, BrowserStopTolerance); reason != StopClear {
		t.Errorf("clear, converged and the cap: %s", reason)
	}
	if reason, _ := EvaluateStop(stopHistory(now, steadyFit, 8), testOpenedMs, now, now, squareCountry, BrowserStopTolerance); reason != StopConverged {
		t.Errorf("converged and the cap: %s", reason)
	}
	// snapshots after now are ignored
	future := append(stopHistory(testOpenedMs+3500, steadyFit, 8), Snapshot{AtMs: testOpenedMs + 9000, Fit: nil})
	if reason, ok := EvaluateStop(future, testOpenedMs, testOpenedMs+3500, testOpenedMs+3500, squareCountry, BrowserStopTolerance); !ok || reason != StopConverged {
		t.Errorf("with a snapshot after now: %s, %v", reason, ok)
	}
}
