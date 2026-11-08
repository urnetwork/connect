package connect

import (
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// The starvation escape end to end, over the rerace harness's in-memory
// providers in a synctest bubble. A provider whose evaluation fails once, as
// a ping lost to our own uplink fails it, is cooled off for 5 minutes. When
// that leaves the window short with nothing else to draw, the escape lists the
// provider again on the next round and the expansion evaluates it: the window
// (re)forms within a pass or two, and without the watchdog's rebuild, which
// acts only on a window that never formed.
//
// A refusal can still happen and is right: a round that ran between the
// failed candidate's retirement and its note listed the provider before it
// failed. The next escape round lists it again.

// The quality window's log lines containing every one of parts.
func cooloffEscapeQualityLines(harness *reraceHarness, parts ...string) []string {
	lines := []string{}
	for _, line := range harness.log.linesWith("window=quality") {
		matched := true
		for _, part := range parts {
			if !strings.Contains(line, part) {
				matched = false
				break
			}
		}
		if matched {
			lines = append(lines, line)
		}
	}
	return lines
}

// Waits in synctest time until the quality window holds n live exits, for at
// most limit, and reports how long it took.
func cooloffEscapeWaitLive(harness *reraceHarness, n int, limit time.Duration) (time.Duration, bool) {
	start := time.Now()
	for len(harness.liveChannels()) < n {
		if limit <= time.Since(start) {
			return time.Since(start), false
		}
		time.Sleep(100 * time.Millisecond)
	}
	return time.Since(start), true
}

// A window whose only provider failed one evaluation tries that provider again
// on the next round instead of leaving it out for the whole cool-off, and
// forms before the outcome watchdog's rebuild (45 s), whose clear would also
// lift the cool-off. Without the escape, or with the expansion refusing what
// the escape lists, the window formed only through that clear.
func TestCooloffOneProviderWindowRecoversAfterATransientFailure(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		harness := newReraceHarness(t, []reraceExitSpec{
			{name: "a", failEvaluations: 1},
		}, nil)
		if got := harness.exit("a").evaluations.Load(); got < 2 {
			t.Fatalf("the provider was constructed %d times, want its failed evaluation and another", got)
		}
		if lines := cooloffEscapeQualityLines(harness, "event=window_rebuild"); 0 < len(lines) {
			t.Errorf("the window formed only after the watchdog rebuilt it: %v", lines)
		}
		if lines := cooloffEscapeQualityLines(harness, "action=clear"); 0 < len(lines) {
			t.Errorf("the cool-off was cleared, not escaped: %v", lines)
		}
		path := reraceFlow(4, 0, 47301)
		if _, _, ok := harness.connect(path); !ok {
			t.Fatal("the recovered provider did not carry a connect")
		}
		if harness.boundTo(path) != harness.channel("a") {
			t.Error("the flow is not on the recovered provider")
		}
	})
}

// The same window with the watchdog turned off: only the escape can bring the
// provider back within the cool-off. Before, the window stayed empty for the
// whole 5 minutes, and the harness, which waits a minute, never formed.
func TestCooloffOneProviderWindowRecoversWithoutTheWatchdog(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		harness := newReraceHarness(t, []reraceExitSpec{
			{name: "a", failEvaluations: 1},
		}, func(settings *MultiClientSettings) {
			settings.WindowOutcomeDeadline = 0
			settings.WindowOutcomeRebuildDeadline = 0
		})
		if got := harness.exit("a").evaluations.Load(); got < 2 {
			t.Fatalf("the provider was constructed %d times, want its failed evaluation and another", got)
		}
	})
}

// A window that formed, so its watchdog is disarmed for its life, loses exits
// (a provider restart, a network change) and each lost provider fails one
// re-evaluation. Losing every exit, the window holds no provider and the
// escape skips the exclusions; losing one of several, the round that excludes
// the failed provider finds nothing else to draw and the next round skips
// them. Either way the window is whole again within a pass or two. Before, the
// expansion refused what the escape listed and the window stayed short for the
// whole cool-off.
func TestCooloffFormedWindowRecoversItsLostProviders(t *testing.T) {
	GetMessagePoolAggregateStats()
	names := []string{"a", "b", "c"}
	cases := []struct {
		providers int
		lost      int
	}{
		{providers: 1, lost: 1},
		{providers: 2, lost: 2},
		{providers: 3, lost: 3},
		{providers: 2, lost: 1},
		{providers: 3, lost: 1},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("providers=%d/lost=%d", c.providers, c.lost), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				specs := []reraceExitSpec{}
				for _, name := range names[:c.providers] {
					specs = append(specs, reraceExitSpec{name: name})
				}
				harness := newReraceHarness(t, specs, nil)
				lost := names[:c.lost]
				for _, name := range lost {
					harness.exit(name).failNextEvaluations(1)
				}
				for _, name := range lost {
					harness.channel(name).Cancel()
				}

				// the expansion backoff paces a pass that admitted nothing,
				// from 10 s doubling, so a pass or two is well inside 2
				// minutes, against the 5-minute cool-off
				elapsed, formed := cooloffEscapeWaitLive(harness, c.providers, 2*time.Minute)
				if !formed {
					t.Fatalf("the quality window holds %d live exits %s after the loss, want %d", len(harness.liveChannels()), elapsed, c.providers)
				}
				for _, name := range lost {
					exit := harness.exit(name)
					if 0 < exit.failNext.Load() {
						t.Errorf("%s was never evaluated again", name)
					}
					if got := exit.evaluations.Load(); got < 3 {
						t.Errorf("%s was constructed %d times, want its first, its failed re-evaluation and another", name, got)
					}
				}
				if lines := cooloffEscapeQualityLines(harness, "event=window_rebuild"); 0 < len(lines) {
					t.Errorf("the formed window was rebuilt: %v", lines)
				}
			})
		})
	}
}
