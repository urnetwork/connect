package connect

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// The cool-off (ip_remote_multi_client_cooloff.go): a provider that failed
// its evaluation, or whose exit a verdict removed while another exit was
// receiving, is left out of the window's discovery until the cool-off has
// passed, except where the starvation escape lets it back.

// A live quality window over the recent-exit test generator, which fails
// every evaluation, with the given receiving-siblings seam and a fresh uplink.
func cooloffTestWindow(t *testing.T, generator MultiClientGenerator, receivingSiblings func(*multiClientChannel) int) *multiClientWindow {
	t.Helper()
	resetBackendDegraded()
	t.Cleanup(resetBackendDegraded)

	settings := DefaultMultiClientSettings()
	settings.WindowResizeTimeout = 20 * time.Millisecond
	settings.WindowExpandTimeout = 20 * time.Millisecond
	settings.DiscoveryExpandRetryMin = 20 * time.Millisecond
	settings.DiscoveryExpandRetryMax = 20 * time.Millisecond

	fresh := func(now time.Time) (bool, time.Time) { return false, time.Time{} }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return newMultiClientWindow(
		ctx,
		cancel,
		generator,
		nil,
		nil,
		false,
		nil,
		DisableSecurityPolicy(),
		func(*multiClientChannel) {},
		nil,
		WindowTypeQuality,
		unfixedTestProfile(WindowTypeQuality),
		settings,
		func() *ReliabilitySettings { return ReliabilitySettingsFrom(settings) },
		fresh,
		time.Now,
		nil,
		nil,
		nil,
		receivingSiblings,
		nil,
		nil,
		nil,
		nil,
	)
}

// An exit that stays in the window for the test: fresh, with no error.
func cooloffTestHeldChannel() *multiClientChannel {
	client := stickyTestPastLifetimeChannel()
	client.firstEventTime = time.Now()
	return client
}

// Discovery offers the two failing providers and, on every call, one provider
// it has not offered before, so a round is never empty.
type cooloffTestGenerator struct {
	recentExitTestGenerator
}

func (self *cooloffTestGenerator) NextDestinations(count int, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
	destinations, err := self.recentExitTestGenerator.NextDestinations(count, excludeDestinations, rankMode)
	if err == nil {
		destinations[RequireMultiHopId(NewId())] = DestinationStats{}
	}
	return destinations, err
}

// While the window holds a provider and discovery still has others to offer,
// a provider that failed its evaluation is evaluated once and then left out of
// every round of its cool-off.
func TestCooloffExcludesProvidersThatFailedEvaluation(t *testing.T) {
	first := RequireMultiHopId(NewId())
	second := RequireMultiHopId(NewId())
	generator := &cooloffTestGenerator{recentExitTestGenerator{
		discoveredDestinations: map[MultiHopId]DestinationStats{first: {}, second: {}},
	}}
	window := cooloffTestWindow(t, generator, nil)
	installStickyTestChannel(window, cooloffTestHeldChannel())

	calls := waitRecentExitTestCalls(t, &generator.recentExitTestGenerator, "evaluate both providers", func(calls []recentExitTestCall) bool {
		return 0 < recentExitTestCount(calls, "evaluate", first) && 0 < recentExitTestCount(calls, "evaluate", second)
	})
	start := recentExitTestCount(calls, "discover", MultiHopId{})
	calls = waitRecentExitTestCalls(t, &generator.recentExitTestGenerator, "discover five more times", func(calls []recentExitTestCall) bool {
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

// A verdict removal is held against the provider only while another exit is
// receiving: then the uplink demonstrably delivers. With no receiving sibling,
// in a one-exit window or with the uplink stale past the gate's hold, the
// silence may be ours.
func TestCooloffNotesAVerdictOnlyWhenASiblingReceives(t *testing.T) {
	verdict := errors.New("Blackhole no-send-ack (send 0/0B recv 0/0B syn 0/0 nackAge 31s synAge - dsts=2)")
	for _, receiving := range []int{0, 1} {
		window := recentExitBareWindow(&recentExitTestGenerator{}, unfixedTestProfile(WindowTypeSpeed))
		log := newRecordingLogger()
		window.log = log
		window.receivingSiblingsFunc = func(exclude *multiClientChannel) int { return receiving }
		client := stickyTestLostChannel(verdict)
		window.clients[client.ClientId()] = client

		removal := window.classifyErrorRemoval(client, verdict, &clientWindowStats{}, recentExitTestWindowSize(), false)
		delete(window.clients, client.ClientId())
		removal.finish()

		AssertEqual(t, window.cooloffHolds(client.Destination()), receiving != 0)
		AssertEqual(t, len(log.linesWith("action=note")), receiving)
	}
}

// The starvation escape: the exclusions are skipped for a round while the
// window holds no provider, and for the round after one whose exclusions left
// discovery empty. Each skip that leaves entries out says so.
func TestCooloffStarvationEscape(t *testing.T) {
	window := recentExitBareWindow(&recentExitTestGenerator{}, unfixedTestProfile(WindowTypeSpeed))
	log := newRecordingLogger()
	window.log = log
	destination := RequireMultiHopId(NewId())
	window.noteFailedDestination(destination, cooloffScopeAll, cooloffReasonEvaluation)

	excluded := map[MultiHopId]bool{}
	AssertEqual(t, window.addCooloffExclusions(excluded, 0), 0)
	AssertEqual(t, len(excluded), 0)
	AssertEqual(t, len(log.linesWith("reason=empty_window")), 1)

	AssertEqual(t, window.addCooloffExclusions(excluded, 1), 1)
	AssertEqual(t, excluded, map[MultiHopId]bool{destination: true})
	window.noteCooloffRound(1, true)

	excluded = map[MultiHopId]bool{}
	AssertEqual(t, window.addCooloffExclusions(excluded, 1), 0)
	AssertEqual(t, len(excluded), 0)
	AssertEqual(t, len(log.linesWith("reason=empty_round")), 1)

	// the skip is spent, and a round that found something sets none
	AssertEqual(t, window.addCooloffExclusions(excluded, 1), 1)
	window.noteCooloffRound(1, false)
	excluded = map[MultiHopId]bool{}
	AssertEqual(t, window.addCooloffExclusions(excluded, 1), 1)

	// the entry is still held for the expansion's refusal through the skips
	AssertEqual(t, window.cooloffHolds(destination), true)
	AssertEqual(t, len(log.linesWith("action=refuse")), 1)
}

// A candidate that failed while the window filled an IPv6 shortfall is kept
// out of the v6-capable request only; the v4-capable fill may still draw it.
// A later failure of any other kind widens it to every request.
func TestCooloffKeepsAnIpv6FillFailureToTheIpv6Request(t *testing.T) {
	window := recentExitBareWindow(&recentExitTestGenerator{}, unfixedTestProfile(WindowTypeSpeed))
	destination := RequireMultiHopId(NewId())
	other := RequireMultiHopId(NewId())
	AssertEqual(t, cooloffEvaluationScope(1, IpFamilyDualstack), cooloffScopeIpv6)
	AssertEqual(t, cooloffEvaluationScope(1, IpFamilyV4Only), cooloffScopeAll)
	AssertEqual(t, cooloffEvaluationScope(0, IpFamilyDualstack), cooloffScopeAll)
	window.noteFailedDestination(destination, cooloffScopeIpv6, cooloffReasonEvaluation)

	excluded := map[MultiHopId]bool{}
	AssertEqual(t, window.addCooloffExclusions(excluded, 1), 0)
	AssertEqual(t, len(excluded), 0)
	AssertEqual(t, window.cooloffHolds(destination), false)
	ipv6 := window.cooloffIpv6Exclusions([]MultiHopId{other})
	AssertEqual(t, len(ipv6), 2)
	AssertEqual(t, slices.Contains(ipv6, destination) && slices.Contains(ipv6, other), true)

	// a skipped round applies neither
	window.addCooloffExclusions(map[MultiHopId]bool{}, 0)
	AssertEqual(t, window.cooloffIpv6Exclusions([]MultiHopId{other}), []MultiHopId{other})

	window.noteFailedDestination(destination, cooloffScopeAll, cooloffReasonVerdict)
	excluded = map[MultiHopId]bool{}
	AssertEqual(t, window.addCooloffExclusions(excluded, 1), 1)
	AssertEqual(t, window.cooloffHolds(destination), true)
	// and a narrower note never narrows it again
	window.noteFailedDestination(destination, cooloffScopeIpv6, cooloffReasonEvaluation)
	AssertEqual(t, window.cooloffHolds(destination), true)
}

// The watchdog's silent rebuild is the programmatic form of a reconnect, and
// a reconnect starts with no cool-off: the rebuild clears it. A rebuild that
// stands down because a provider landed first leaves it.
func TestCooloffClearedByTheRebuild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	destination := RequireMultiHopId(NewId())

	log := newRecordingLogger()
	window := outcomeTestWindow(ctx, log)
	window.armOutcome()
	window.noteFailedDestination(destination, cooloffScopeAll, cooloffReasonEvaluation)
	AssertEqual(t, window.cooloffHolds(destination), true)
	window.rebuildWindow(45 * time.Second)
	AssertEqual(t, window.cooloffHolds(destination), false)
	if lines := log.linesWith("action=clear"); len(lines) != 1 || !strings.Contains(lines[0], "reason=rebuild") {
		t.Fatalf("want one clear line naming the rebuild, got %v", lines)
	}

	aborted := outcomeTestWindow(ctx, newRecordingLogger())
	aborted.armOutcome()
	aborted.noteFailedDestination(destination, cooloffScopeAll, cooloffReasonEvaluation)
	aborted.noteClientAdded(nil)
	aborted.rebuildWindow(45 * time.Second)
	AssertEqual(t, aborted.cooloffHolds(destination), true)
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
	cooloff.note(first, start, cooloffScopeAll, 2)
	cooloff.note(second, start.Add(time.Second), cooloffScopeAll, 2)

	excluded := map[MultiHopId]bool{}
	AssertEqual(t, cooloff.addExclusions(excluded, start.Add(timeout-time.Second), timeout, cooloffScopeAll), 2)
	AssertEqual(t, excluded, map[MultiHopId]bool{first: true, second: true})

	// past the bound the oldest leaves
	cooloff.note(third, start.Add(2*time.Second), cooloffScopeAll, 2)
	excluded = map[MultiHopId]bool{}
	cooloff.addExclusions(excluded, start.Add(2*time.Second), timeout, cooloffScopeAll)
	AssertEqual(t, excluded, map[MultiHopId]bool{second: true, third: true})

	// past the cool-off an entry leaves
	excluded = map[MultiHopId]bool{}
	cooloff.addExclusions(excluded, start.Add(timeout+time.Second), timeout, cooloffScopeAll)
	AssertEqual(t, excluded, map[MultiHopId]bool{third: true})
	AssertEqual(t, len(cooloff.entries), 1)
}

// A fixed destination set is never cooled off: its own retry backoff governs
// it. Nor is anything with the cool-off turned off.
func TestCooloffSkipsFixedDestinationsAndCanBeTurnedOff(t *testing.T) {
	destination := RequireMultiHopId(NewId())

	fixed := recentExitBareWindow(&recentExitFixedTestGenerator{}, unfixedTestProfile(WindowTypeSpeed))
	fixed.noteFailedDestination(destination, cooloffScopeAll, cooloffReasonEvaluation)
	excluded := map[MultiHopId]bool{}
	fixed.addCooloffExclusions(excluded, 1)
	AssertEqual(t, len(excluded), 0)

	off := recentExitBareWindow(&recentExitTestGenerator{}, unfixedTestProfile(WindowTypeSpeed))
	off.settings.EvaluationFailureCooloff = 0
	off.noteFailedDestination(destination, cooloffScopeAll, cooloffReasonEvaluation)
	off.addCooloffExclusions(excluded, 1)
	AssertEqual(t, len(excluded), 0)

	on := recentExitBareWindow(&recentExitTestGenerator{}, unfixedTestProfile(WindowTypeSpeed))
	on.noteFailedDestination(destination, cooloffScopeAll, cooloffReasonEvaluation)
	on.addCooloffExclusions(excluded, 1)
	AssertEqual(t, excluded, map[MultiHopId]bool{destination: true})
}
