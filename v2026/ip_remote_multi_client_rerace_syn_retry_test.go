// The rerace controls retain enabled automatic SYN continuation alongside
// explicit source-RTO ordering and exact-flow barrier checks.
package connect

import (
	"testing"
	"testing/synctest"
)

// An ordinary provider failure completes from the original source SYN in
// either address family, with the automatic dispatcher entirely unheld.
func TestDialFailureReraceRetainedSynCompletesWithoutSourceRetransmit(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	for _, version := range []int{4, 6} {
		synctest.Test(t, func(t *testing.T) {
			harness := newReraceHarness(t, []reraceExitSpec{
				{name: "a", tier: 0, answer: reraceAlways(reraceSignal)},
				{name: "b", tier: 1},
			}, nil)
			path := reraceFlow(version, 0, 47004)
			synCount, elapsed, ok := harness.connect(path)
			if !ok || synCount != 1 || elapsed != 0 {
				t.Fatalf("ipv%d: automatic continuation completed=%t source_syns=%d elapsed=%s, want true/1/0s", version, ok, synCount, elapsed)
			}
			if harness.boundTo(path) != harness.channel("b") || len(harness.exit("a").syns(path)) != 1 ||
				len(harness.exit("b").syns(path)) != 1 || len(harness.delivered(path)) != 1 {
				t.Fatalf("ipv%d: automatic continuation did not deliver exactly one SYN-ACK through B", version)
			}
			metrics := harness.metrics()
			if metrics.DialFailuresIntercepted != 1 || metrics.FlowsReraced != 1 ||
				harness.channel("a").dialFailureCount() != 1 || harness.channel("b").dialFailureCount() != 0 {
				t.Fatalf("ipv%d: automatic continuation changed the exact failure accounting", version)
			}
			reraceAssertNoErrorDelivered(t, harness, path)
		})
	}
}

// Emptying an admitted multi-candidate race can automatically create a fresh
// race from the retained SYN, without another source offer or timer.
func TestDialFailureWithdrawnRaceRetriesRetainedSynWithoutSourceRetransmit(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness := newReraceHarness(t, []reraceExitSpec{
			{name: "a", tier: 0, answer: reraceAlways(reraceSignal)},
			{name: "b", tier: 0, answer: reraceAlways(reraceSignal)},
			{name: "c", tier: 1},
			{name: "d", tier: 1},
		}, nil)
		a, b := harness.channel("a"), harness.channel("b")
		path := reraceFlow(4, 0, 47005)
		synCount, elapsed, ok := harness.connect(path)
		if !ok || synCount != 1 || elapsed != 0 {
			t.Fatalf("automatic fresh race completed=%t source_syns=%d elapsed=%s, want true/1/0s", ok, synCount, elapsed)
		}
		if bound := harness.boundTo(path); bound != harness.channel("c") && bound != harness.channel("d") {
			t.Fatal("the retained SYN did not start a fresh race on C or D")
		}
		if len(harness.exit("a").syns(path)) != 1 || len(harness.exit("b").syns(path)) != 1 ||
			a.dialFailureCount() != 1 || b.dialFailureCount() != 1 || harness.metrics().DialFailuresIntercepted != 2 {
			t.Fatal("the emptied race did not retain exactly one failure per original candidate")
		}
		reraceAssertNoErrorDelivered(t, harness, path)
	})
}

// A different flow's real failure bypasses the barrier. The target's repeated
// entry assertions stay valid until its one retained retry is released.
func TestReraceSynRetryBarrierPinsExactFlowAndRetainsEntry(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		harness := newReraceHarness(t, []reraceExitSpec{
			{name: "a", tier: 0, answer: reraceAlways(reraceSilent)},
			{name: "b", tier: 1},
		}, nil)
		path, otherPath := reraceFlow(4, 0, 47006), reraceFlow(4, 1, 47007)
		if !harness.syn(path) || !harness.syn(otherPath) {
			t.Fatal("initial source SYNs were refused")
		}
		update := harness.update(path)
		retry := reraceHoldSynRetry(t, harness, update)
		harness.inject("a", reraceProviderSignal(t, otherPath))
		retry.assertNotEntered(t)
		if harness.boundTo(path) != harness.channel("a") || harness.boundTo(otherPath) != harness.channel("b") ||
			len(harness.exit("b").syns(otherPath)) != 1 || len(harness.exit("b").syns(path)) != 0 {
			t.Fatal("the exact-flow barrier intercepted another flow's automatic retry")
		}

		harness.inject("a", reraceProviderSignal(t, path))
		retry.assertEntered(t)
		retry.assertEntered(t)
		if harness.update(path) != update || harness.boundTo(path) != nil || len(harness.exit("b").syns(path)) != 0 {
			t.Fatal("the target retry escaped its exact source barrier")
		}
		retry.release()
		retry.release()
		harness.settle()
		if harness.update(path) != update || harness.boundTo(path) != harness.channel("b") ||
			len(harness.sentSyns(path)) != 1 || len(harness.exit("b").syns(path)) != 1 {
			t.Fatal("the released target did not resume exactly one retained SYN")
		}
	})
}
