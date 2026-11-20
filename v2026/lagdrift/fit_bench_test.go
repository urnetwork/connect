package lagdrift

// The fit's cost (lag.dev/DRIFTTEST.md V10's budget, on a developer machine:
// a warm refit at p95 ≤ 1 ms and a full fit at p95 ≤ 10 ms), on the corpus's
// geometry, for single runs and 3-run pools of 12 to 20 extenders.

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

// Corpus visitors with 12 to 20 extenders a run.
func benchVisitors(seed uint64, runs int, count int) []corpusVisitor {
	rng := testRand(seed)
	visitors := []corpusVisitor{}
	for i := 0; i < count; i++ {
		visitors = append(visitors, corpusDrawN(rng, runs, 12+i%9))
	}
	return visitors
}

// A tick later: each round trip a little lower or higher, as a few more pings
// move the 10th percentile, and the warm fit from the tick before.
type benchTick struct {
	v    corpusVisitor
	warm *Fit
	next []Observation
}

func benchTicks(b testing.TB, seed uint64, runs int, count int) []benchTick {
	m := DefaultModel()
	ticks := []benchTick{}
	for i, v := range benchVisitors(seed, runs, count) {
		warm, err := FitPosition(m, v.expected, v.obs, nil)
		if err != nil {
			b.Fatal(err)
		}
		next := slices.Clone(v.obs)
		for j := range next {
			next[j].RoundTrip.Ms += 0.08 * float64((i+j)%5-2)
			next[j].RoundTrip.Count += 1
		}
		ticks = append(ticks, benchTick{v: v, warm: warm, next: next})
	}
	return ticks
}

func BenchmarkFitPositionOneRun(b *testing.B) {
	visitors := benchVisitors(301, 1, 64)
	m := DefaultModel()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		v := visitors[i%len(visitors)]
		if _, err := FitPosition(m, v.expected, v.obs, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFitPositionThreeRuns(b *testing.B) {
	visitors := benchVisitors(303, 3, 32)
	m := DefaultModel()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		v := visitors[i%len(visitors)]
		if _, err := FitPosition(m, v.expected, v.obs, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRefitPositionOneRun(b *testing.B) {
	ticks := benchTicks(b, 305, 1, 64)
	m := DefaultModel()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		t := ticks[i%len(ticks)]
		if _, err := RefitPosition(m, t.v.expected, t.next, t.warm); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRefitPositionThreeRuns(b *testing.B) {
	ticks := benchTicks(b, 307, 3, 32)
	m := DefaultModel()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		t := ticks[i%len(ticks)]
		if _, err := RefitPosition(m, t.v.expected, t.next, t.warm); err != nil {
			b.Fatal(err)
		}
	}
}

// The p50 and p95 of each, with the budget's: run with
// LAGDRIFT_FIT_TIMES=1 go test ./lagdrift -run FitTimes -v. CPU time is shared
// with whatever else the machine runs, so the budget isn't asserted.
func TestFitTimes(t *testing.T) {
	if testing.Short() || !envSet("LAGDRIFT_FIT_TIMES") {
		t.Skip("set LAGDRIFT_FIT_TIMES=1 to time the fits")
	}
	m := DefaultModel()
	percentiles := func(durations []time.Duration) (time.Duration, time.Duration) {
		slices.Sort(durations)
		return durations[len(durations)/2], durations[len(durations)*95/100]
	}
	for _, runs := range []int{1, 3} {
		var full []time.Duration
		for _, v := range benchVisitors(uint64(400+runs), runs, 200) {
			start := time.Now()
			if _, err := FitPosition(m, v.expected, v.obs, nil); err != nil {
				t.Fatal(err)
			}
			full = append(full, time.Since(start))
		}
		var refit []time.Duration
		for _, tick := range benchTicks(t, uint64(500+runs), runs, 200) {
			start := time.Now()
			if _, err := RefitPosition(m, tick.v.expected, tick.next, tick.warm); err != nil {
				t.Fatal(err)
			}
			refit = append(refit, time.Since(start))
		}
		fullP50, fullP95 := percentiles(full)
		refitP50, refitP95 := percentiles(refit)
		fmt.Printf("%d run(s), 12-20 extenders: FitPosition p50 %v p95 %v (budget 10ms); RefitPosition p50 %v p95 %v (budget 1ms)\n", runs, fullP50, fullP95, refitP50, refitP95)
	}
}
