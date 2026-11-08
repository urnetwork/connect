package connect

import (
	"errors"
	"slices"
	"testing"
	"time"
)

// The cool-off (ip_remote_multi_client_cooloff.go): a provider that failed
// its evaluation, or whose exit a verdict removed, is left out of the
// window's discovery until the cool-off has passed.

// Every evaluation fails, as with providers that never answer, and discovery
// keeps offering the same two. Each is evaluated once and then excluded from
// the rounds that follow. Before the cool-off the window drew them again on
// every round: a contract, an identity and an evaluation each time.
func TestCooloffExcludesProvidersThatFailedEvaluation(t *testing.T) {
	first := RequireMultiHopId(NewId())
	second := RequireMultiHopId(NewId())
	generator := &recentExitTestGenerator{
		discoveredDestinations: map[MultiHopId]DestinationStats{first: {}, second: {}},
	}
	recentExitTestWindow(t, generator, unfixedTestProfile(WindowTypeSpeed), nil, nil)

	calls := waitRecentExitTestCalls(t, generator, "evaluate both providers", func(calls []recentExitTestCall) bool {
		return 0 < recentExitTestCount(calls, "evaluate", first) && 0 < recentExitTestCount(calls, "evaluate", second)
	})
	start := recentExitTestCount(calls, "discover", MultiHopId{})
	calls = waitRecentExitTestCalls(t, generator, "discover five more times", func(calls []recentExitTestCall) bool {
		return start+5 <= recentExitTestCount(calls, "discover", MultiHopId{})
	})
	for _, destination := range []MultiHopId{first, second} {
		if count := recentExitTestCount(calls, "evaluate", destination); count != 1 {
			t.Errorf("a provider that failed its evaluation was evaluated %d times within its cool-off", count)
		}
	}
	excludes := generator.recordedDiscoveryExcludes()
	last := excludes[len(excludes)-1]
	if !slices.Contains(last, first) || !slices.Contains(last, second) {
		t.Errorf("the latest discovery excluded %v, want both failed providers", last)
	}
}

// A blackhole verdict on a fresh uplink is evidence against the provider:
// once the exit is removed, discovery leaves its provider out, though the
// platform still offers it.
func TestCooloffExcludesProvidersRemovedByAVerdict(t *testing.T) {
	client := stickyTestLostChannel(errors.New("Blackhole no-send-ack (send 0/0B recv 0/0B syn 0/0 nackAge 31s synAge - dsts=2)"))
	lost := client.Destination()
	generator := &recentExitTestGenerator{
		discoveredDestinations: map[MultiHopId]DestinationStats{lost: {}},
	}
	fresh := func(now time.Time) (bool, time.Time) { return false, time.Time{} }
	window := recentExitTestWindow(t, generator, unfixedTestProfile(WindowTypeSpeed), fresh, time.Now)
	installStickyTestChannel(window, client)

	waitRecentExitTestRemoved(t, window, client)
	start := recentExitTestCount(generator.recordedCalls(), "discover", MultiHopId{})
	calls := waitRecentExitTestCalls(t, generator, "discover three more times", func(calls []recentExitTestCall) bool {
		return start+3 <= recentExitTestCount(calls, "discover", MultiHopId{})
	})
	if count := recentExitTestCount(calls, "mint", lost); count != 0 {
		t.Errorf("the provider of an exit a verdict removed was minted %d times within its cool-off", count)
	}
	excludes := generator.recordedDiscoveryExcludes()
	if last := excludes[len(excludes)-1]; !slices.Contains(last, lost) {
		t.Errorf("the latest discovery excluded %v, want the removed exit's provider", last)
	}
}

// An entry excludes its provider until the cool-off has passed, then is
// dropped; the memory keeps the newest providers up to its bound.
func TestCooloffEntriesExpireAndAreBounded(t *testing.T) {
	first := RequireMultiHopId(NewId())
	second := RequireMultiHopId(NewId())
	third := RequireMultiHopId(NewId())
	start := time.Now()
	timeout := time.Minute
	cooloff := &destinationCooloff{}
	cooloff.note(first, start, 2)
	cooloff.note(second, start.Add(time.Second), 2)

	excluded := map[MultiHopId]bool{}
	cooloff.addExclusions(excluded, start.Add(timeout-time.Second), timeout)
	AssertEqual(t, excluded, map[MultiHopId]bool{first: true, second: true})

	// past the bound the oldest leaves
	cooloff.note(third, start.Add(2*time.Second), 2)
	excluded = map[MultiHopId]bool{}
	cooloff.addExclusions(excluded, start.Add(2*time.Second), timeout)
	AssertEqual(t, excluded, map[MultiHopId]bool{second: true, third: true})

	// past the cool-off an entry leaves
	excluded = map[MultiHopId]bool{}
	cooloff.addExclusions(excluded, start.Add(timeout+time.Second), timeout)
	AssertEqual(t, excluded, map[MultiHopId]bool{third: true})
	AssertEqual(t, len(cooloff.failTimes), 1)
}

// A fixed destination set is never cooled off: its own retry backoff governs
// it. Nor is anything with the cool-off turned off.
func TestCooloffSkipsFixedDestinationsAndCanBeTurnedOff(t *testing.T) {
	destination := RequireMultiHopId(NewId())

	fixed := recentExitBareWindow(&recentExitFixedTestGenerator{}, unfixedTestProfile(WindowTypeSpeed))
	fixed.noteFailedDestination(destination)
	excluded := map[MultiHopId]bool{}
	fixed.addCooloffExclusions(excluded)
	AssertEqual(t, len(excluded), 0)

	off := recentExitBareWindow(&recentExitTestGenerator{}, unfixedTestProfile(WindowTypeSpeed))
	off.settings.EvaluationFailureCooloff = 0
	off.noteFailedDestination(destination)
	off.addCooloffExclusions(excluded)
	AssertEqual(t, len(excluded), 0)

	on := recentExitBareWindow(&recentExitTestGenerator{}, unfixedTestProfile(WindowTypeSpeed))
	on.noteFailedDestination(destination)
	on.addCooloffExclusions(excluded)
	AssertEqual(t, excluded, map[MultiHopId]bool{destination: true})
}
