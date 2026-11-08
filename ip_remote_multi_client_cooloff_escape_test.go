package connect

import (
	"testing"
	"testing/synctest"
)

// A window whose only provider failed one evaluation, as a ping lost to our
// own uplink fails it, tries that provider again on the next round instead of
// leaving it out for the whole cool-off: the window holds no provider, so the
// cool-off's exclusions are skipped. Before the escape the provider stayed out
// for 5 minutes, the window stayed empty, and the harness, which waits a
// minute for the window to carry its provider, never formed.
func TestCooloffOneProviderWindowRecoversAfterATransientFailure(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		harness := newReraceHarness(t, []reraceExitSpec{
			{name: "a", failEvaluations: 1},
		}, nil)
		if got := harness.exit("a").evaluations.Load(); got < 2 {
			t.Fatalf("the provider was constructed %d times, want its failed evaluation and another", got)
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
