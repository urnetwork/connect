package connect

import (
	"testing"
	"testing/synctest"
)

// A SYN raced across the window's best-tier exits that none of them answers
// is re-raced at the retransmit past inferredDialFailureTimeout, as a flow
// bound to one silent exit is: each silent candidate leaves the race with its
// strike and its failure for the destination, so the retransmit widens to the
// other tiers. Before, the race never committed (a SYN race commits only on
// an answer), the silence inference read only a bound exit, and the collapse
// filter held every retransmit back from the exits that already had the SYN,
// so the connect waited out the app's own timeout although C and D answer.
func TestSilentWideRaceReracesAtTheInference(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		path := reraceFlow(4, 1, 47101)
		silentForPath := func(candidate *IpPath) reraceAnswer {
			if reraceSameFlow(candidate, path) {
				return reraceSilent
			}
			return reraceAccept
		}
		harness := newReraceHarness(t, []reraceExitSpec{
			{name: "a", tier: 0, answer: silentForPath},
			{name: "b", tier: 0, answer: silentForPath},
			{name: "c", tier: 1},
			{name: "d", tier: 1},
		}, nil)
		a, b := harness.channel("a"), harness.channel("b")
		synCount, elapsed, ok := harness.connect(path)
		if !ok {
			t.Fatalf("the connect never completed after %d SYNs: the silent race was never re-raced", synCount)
		}
		if synCount != 3 || elapsed < reraceSynSchedule[2] || reraceSynSchedule[2]+reraceSettle < elapsed {
			t.Errorf("the connect completed on SYN %d after %s, want on SYN 3 at %s", synCount, elapsed, reraceSynSchedule[2])
		}
		if bound := harness.boundTo(path); bound != harness.channel("c") && bound != harness.channel("d") {
			t.Error("the flow is not on C or D")
		}
		for _, channel := range []*multiClientChannel{a, b} {
			if got := channel.dialFailureCount(); got != 1 {
				t.Errorf("a silent race candidate carries %d strikes, want 1", got)
			}
			if channel.IsDone() {
				t.Error("a silent race candidate was gone at the re-race; the re-race tears no exit down")
			}
		}
		if got := len(harness.exit("a").syns(path)) + len(harness.exit("b").syns(path)); got != 2 {
			t.Errorf("the silent candidates received %d SYNs, want one each", got)
		}
		if metrics := harness.metrics(); metrics.DialFailuresIntercepted != 0 {
			t.Errorf("DialFailuresIntercepted = %d, want 0: nothing was signaled", metrics.DialFailuresIntercepted)
		}
	})
}
