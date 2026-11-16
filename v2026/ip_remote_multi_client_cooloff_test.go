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
// every evaluation, with the given receiving-siblings seam and a fresh uplink,
// sized by a quality profile of two to four exits. A profile pins the window,
// so it opens no IPv6 shortfall.
func cooloffTestWindow(t *testing.T, generator MultiClientGenerator, receivingSiblings func(*multiClientChannel) int) *multiClientWindow {
	return cooloffTestWindowWithProfile(t, generator, receivingSiblings, unfixedTestProfile(WindowTypeQuality))
}

// cooloffTestWindow with the given performance profile; nil sizes the window
// by the default settings, with their soft minimum of one v6-capable exit.
func cooloffTestWindowWithProfile(t *testing.T, generator MultiClientGenerator, receivingSiblings func(*multiClientChannel) int, performanceProfile *PerformanceProfile) *multiClientWindow {
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
		performanceProfile,
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
// it has not offered before, so a round is never empty. While set, every call
// waits for listingHeld to close.
type cooloffTestGenerator struct {
	recentExitTestGenerator
	listingHeld <-chan struct{}
}

func (self *cooloffTestGenerator) NextDestinations(count int, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
	if self.listingHeld != nil {
		<-self.listingHeld
	}
	destinations, err := self.recentExitTestGenerator.NextDestinations(count, excludeDestinations, rankMode)
	if err == nil {
		destinations[RequireMultiHopId(NewId())] = DestinationStats{}
	}
	return destinations, err
}

// A live cool-off window over generator that holds its exit before any round
// lists. The constructor starts the enumerator, and a round before the install
// holds no provider, so the starvation escape rightly skips the exclusions: a
// second such round evaluated again a provider the first had noted. Only the
// first round can read the window early, and it has nothing noted to skip.
func cooloffTestHeldWindow(t *testing.T, generator *cooloffTestGenerator) *multiClientWindow {
	t.Helper()
	listingHeld := make(chan struct{})
	generator.listingHeld = listingHeld
	window := cooloffTestWindow(t, generator, nil)
	installStickyTestChannel(window, cooloffTestHeldChannel())
	close(listingHeld)
	return window
}

// While the window holds a provider and discovery still has others to offer,
// a provider that failed its evaluation is evaluated once and then left out of
// every round of its cool-off.
func TestCooloffExcludesProvidersThatFailedEvaluation(t *testing.T) {
	first := RequireMultiHopId(NewId())
	second := RequireMultiHopId(NewId())
	generator := &cooloffTestGenerator{recentExitTestGenerator: recentExitTestGenerator{
		discoveredDestinations: map[MultiHopId]DestinationStats{first: {}, second: {}},
	}}
	cooloffTestHeldWindow(t, generator)

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

// Family-aware discovery: both requests offer the dual-stack provider unless
// they exclude it, and the v4-capable fill adds one IPv4 provider it has not
// offered before, so a round is never empty. Every evaluation fails.
type cooloffFamilyTestGenerator struct {
	cooloffTestGenerator
	dualstack      MultiHopId
	familyExcludes map[IpFamilyFilter][][]MultiHopId
}

func (self *cooloffFamilyTestGenerator) NextDestinationsWithIpFamily(count int, excludeDestinations []MultiHopId, rankMode string, ipFamily IpFamilyFilter) (map[MultiHopId]DestinationStats, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.calls = append(self.calls, recentExitTestCall{kind: "discover"})
	self.familyExcludes[ipFamily] = append(self.familyExcludes[ipFamily], slices.Clone(excludeDestinations))
	destinations := map[MultiHopId]DestinationStats{}
	// offered once the window has asked the v6-capable request, so its
	// evaluation runs with the IPv6 shortfall open
	if 0 < len(self.familyExcludes[IpFamilyFilterV6Capable]) && !slices.Contains(excludeDestinations, self.dualstack) {
		destinations[self.dualstack] = DestinationStats{IpFamily: IpFamilyDualstack}
	}
	if ipFamily == IpFamilyFilterV4Capable {
		destinations[RequireMultiHopId(NewId())] = DestinationStats{IpFamily: IpFamilyV4Only}
	}
	return destinations, nil
}

func (self *cooloffFamilyTestGenerator) recordedFamilyExcludes(ipFamily IpFamilyFilter) [][]MultiHopId {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.familyExcludes[ipFamily])
}

// The evaluation is a control-plane ping, which says nothing of the
// provider's egress families: a dual-stack provider that failed while the
// window filled its IPv6 shortfall (the window's one exit carries no IPv6)
// is left out of the v4-capable fill too, and evaluated once in its
// cool-off. Kept to the v6-capable request, it was drawn again by the
// v4-capable fill, which the platform draws dual-stack first, on every round.
func TestCooloffLeavesAProviderThatFailedDuringAnIpv6ShortfallOutOfEveryRequest(t *testing.T) {
	dualstack := RequireMultiHopId(NewId())
	generator := &cooloffFamilyTestGenerator{
		dualstack:      dualstack,
		familyExcludes: map[IpFamilyFilter][][]MultiHopId{},
	}
	window := cooloffTestWindowWithProfile(t, generator, nil, nil)
	installStickyTestChannel(window, cooloffTestHeldChannel())

	waitRecentExitTestCalls(t, &generator.recentExitTestGenerator, "evaluate the dual-stack provider and note it", func(calls []recentExitTestCall) bool {
		return 0 < recentExitTestCount(calls, "evaluate", dualstack) && cooloffTestNoted(window, dualstack)
	})
	if len(generator.recordedFamilyExcludes(IpFamilyFilterV6Capable)) == 0 {
		t.Fatal("the window never asked the v6-capable request: its IPv6 shortfall was not open")
	}
	start := len(generator.recordedFamilyExcludes(IpFamilyFilterV4Capable))
	calls := waitRecentExitTestCalls(t, &generator.recentExitTestGenerator, "fill five more times", func(calls []recentExitTestCall) bool {
		return start+5 <= len(generator.recordedFamilyExcludes(IpFamilyFilterV4Capable))
	})
	if count := recentExitTestCount(calls, "evaluate", dualstack); count != 1 {
		t.Errorf("the dual-stack provider was evaluated %d times within its cool-off, want 1", count)
	}
	// the first fill may come from a round that read the cool-off before the
	// note
	fills := generator.recordedFamilyExcludes(IpFamilyFilterV4Capable)
	for i, excludes := range fills[start+1:] {
		if !slices.Contains(excludes, dualstack) {
			t.Errorf("v4-capable fill %d after the failure did not exclude the dual-stack provider", i+2)
		}
	}
}

// Whether the window's cool-off holds an entry for destination, read without
// the refusal's log line.
func cooloffTestNoted(window *multiClientWindow, destination MultiHopId) bool {
	window.cooloff.stateLock.Lock()
	defer window.cooloff.stateLock.Unlock()
	_, ok := window.cooloff.entries[destination]
	return ok
}

// The escape's call-site wiring in a live window: the window holds an exit,
// and discovery offers only the provider that failed. The round that excludes
// it comes back empty and reports so (noteCooloffRound), the next round skips
// the exclusions and lists it, and the expansion evaluates what that round
// listed.
func TestCooloffEscapeEvaluatesTheOnlyProviderAgainWhenDiscoveryRunsDry(t *testing.T) {
	only := RequireMultiHopId(NewId())
	generator := &recentExitTestGenerator{
		discoveredDestinations: map[MultiHopId]DestinationStats{only: {}},
	}
	window := cooloffTestWindow(t, generator, nil)
	installStickyTestChannel(window, cooloffTestHeldChannel())

	// the first round that excluded the provider, as a position in the calls
	excludedAt := func(calls []recentExitTestCall) int {
		excludes := generator.recordedDiscoveryExcludes()
		discover := 0
		for i, call := range calls {
			if call.kind != "discover" {
				continue
			}
			if discover < len(excludes) && slices.Contains(excludes[discover], only) {
				return i
			}
			discover += 1
		}
		return -1
	}
	waitRecentExitTestCalls(t, generator, "evaluate the provider again after a round excluded it", func(calls []recentExitTestCall) bool {
		at := excludedAt(calls)
		return 0 <= at && 0 <= recentExitTestIndex(calls, at+1, "evaluate", only)
	})
}

// An empty probe round says nothing of the pool: a probe asks only the
// v6-capable request (a legacy generator is asked nothing). The round after it
// still applies the exclusions, so a failed provider stays out while discovery
// has others to offer.
func TestCooloffAnEmptyProbeRoundKeepsTheExclusions(t *testing.T) {
	failed := RequireMultiHopId(NewId())
	generator := &cooloffTestGenerator{recentExitTestGenerator: recentExitTestGenerator{
		discoveredDestinations: map[MultiHopId]DestinationStats{failed: {}},
	}}
	window := cooloffTestHeldWindow(t, generator)

	waitRecentExitTestCalls(t, &generator.recentExitTestGenerator, "evaluate the provider and note it", func(calls []recentExitTestCall) bool {
		return 0 < recentExitTestCount(calls, "evaluate", failed) && cooloffTestNoted(window, failed)
	})
	probeTaken := func() bool {
		window.stateLock.Lock()
		defer window.stateLock.Unlock()
		return !window.ipv6ProbeRequested
	}
	for range 3 {
		window.requestIpv6Probe()
		deadline := time.Now().Add(30 * time.Second)
		for !probeTaken() {
			if deadline.Before(time.Now()) {
				t.Fatal("the enumerator did not take the probe request")
			}
			time.Sleep(time.Millisecond)
		}
		// the probe round, then two ordinary rounds that read its report
		start := recentExitTestCount(generator.recordedCalls(), "discover", MultiHopId{})
		waitRecentExitTestCalls(t, &generator.recentExitTestGenerator, "discover twice after the probe", func(calls []recentExitTestCall) bool {
			return start+2 <= recentExitTestCount(calls, "discover", MultiHopId{})
		})
	}
	if count := recentExitTestCount(generator.recordedCalls(), "evaluate", failed); count != 1 {
		t.Errorf("the failed provider was evaluated %d times within its cool-off, want 1: an empty probe lifted the exclusions", count)
	}
}

// The exclude line is written when the number of excluded providers changes,
// not on every round: discovery runs on every expansion pass.
func TestCooloffLogsTheExclusionsWhenTheirCountChanges(t *testing.T) {
	window := recentExitBareWindow(&recentExitTestGenerator{}, unfixedTestProfile(WindowTypeSpeed))
	log := newRecordingLogger()
	window.log = log
	first := RequireMultiHopId(NewId())
	second := RequireMultiHopId(NewId())
	rounds := func() {
		for range 3 {
			window.addCooloffExclusions(map[MultiHopId]bool{}, 1, true)
		}
	}

	window.noteFailedDestination(first, cooloffReasonEvaluation)
	rounds()
	AssertEqual(t, len(log.linesWith("action=exclude count=1")), 1)
	window.noteFailedDestination(second, cooloffReasonEvaluation)
	rounds()
	AssertEqual(t, len(log.linesWith("action=exclude count=2")), 1)
	// both pass their cool-off
	past := time.Now().Add(-window.cooloffTimeout())
	window.cooloff.note(first, past, cooloffMaxCount)
	window.cooloff.note(second, past, cooloffMaxCount)
	rounds()
	AssertEqual(t, len(log.linesWith("action=exclude count=0")), 1)
	AssertEqual(t, len(log.linesWith("action=exclude")), 3)
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

		AssertEqual(t, cooloffTestNoted(window, client.Destination()), receiving != 0)
		AssertEqual(t, len(log.linesWith("action=note")), receiving)
	}
}

// The starvation escape: the exclusions are skipped for a round while the
// window holds no provider, and for the round after one whose exclusions left
// discovery empty. Each skip that leaves entries out says so, and its listing
// says it skipped. The entries stay for the rounds that apply the exclusions.
func TestCooloffStarvationEscape(t *testing.T) {
	window := recentExitBareWindow(&recentExitTestGenerator{}, unfixedTestProfile(WindowTypeSpeed))
	log := newRecordingLogger()
	window.log = log
	destination := RequireMultiHopId(NewId())
	window.noteFailedDestination(destination, cooloffReasonEvaluation)

	excluded := map[MultiHopId]bool{}
	count, listing := window.addCooloffExclusions(excluded, 0, true)
	AssertEqual(t, count, 0)
	AssertEqual(t, listing.skipped, true)
	AssertEqual(t, len(excluded), 0)
	AssertEqual(t, len(log.linesWith("reason=empty_window")), 1)

	count, listing = window.addCooloffExclusions(excluded, 1, true)
	AssertEqual(t, count, 1)
	AssertEqual(t, listing.skipped, false)
	AssertEqual(t, excluded, map[MultiHopId]bool{destination: true})
	window.noteCooloffRound(1, true)

	excluded = map[MultiHopId]bool{}
	count, listing = window.addCooloffExclusions(excluded, 1, true)
	AssertEqual(t, count, 0)
	AssertEqual(t, listing.skipped, true)
	AssertEqual(t, len(excluded), 0)
	AssertEqual(t, len(log.linesWith("reason=empty_round")), 1)

	// the skip is spent, and a round that found something sets none
	count, _ = window.addCooloffExclusions(excluded, 1, true)
	AssertEqual(t, count, 1)
	window.noteCooloffRound(1, false)
	excluded = map[MultiHopId]bool{}
	count, _ = window.addCooloffExclusions(excluded, 1, true)
	AssertEqual(t, count, 1)
}

// The expansion refuses a candidate listed before its provider failed, and
// one a round returned despite excluding it. A candidate the escape listed
// after the failure is evaluated: refusing it too minted and retired an
// identity for nothing and kept the window empty for the whole cool-off.
func TestCooloffRefusesOnlyACandidateListedBeforeItsProviderFailed(t *testing.T) {
	window := recentExitBareWindow(&recentExitTestGenerator{}, unfixedTestProfile(WindowTypeSpeed))
	log := newRecordingLogger()
	window.log = log
	destination := RequireMultiHopId(NewId())

	// a round listed the provider, and then it failed
	_, before := window.addCooloffExclusions(map[MultiHopId]bool{}, 0, true)
	window.noteFailedDestination(destination, cooloffReasonEvaluation)
	AssertEqual(t, window.cooloffRefuses(destination, before), true)

	// an escape round lists it after the failure
	_, escape := window.addCooloffExclusions(map[MultiHopId]bool{}, 0, true)
	AssertEqual(t, escape.skipped, true)
	AssertEqual(t, window.cooloffRefuses(destination, escape), false)

	// a round that excluded it, whose request returned it anyway
	_, excluding := window.addCooloffExclusions(map[MultiHopId]bool{}, 1, true)
	AssertEqual(t, excluding.skipped, false)
	AssertEqual(t, window.cooloffRefuses(destination, excluding), true)
	AssertEqual(t, window.cooloffRefuses(destination, cooloffListing{}), true)

	// it fails again after the escape round listed it
	window.noteFailedDestination(destination, cooloffReasonEvaluation)
	AssertEqual(t, window.cooloffRefuses(destination, escape), true)

	// a provider that never failed, or whose cool-off has passed, is never
	// refused
	AssertEqual(t, window.cooloffRefuses(RequireMultiHopId(NewId()), excluding), false)
	expired := RequireMultiHopId(NewId())
	window.cooloff.note(expired, time.Now().Add(-window.cooloffTimeout()), cooloffMaxCount)
	AssertEqual(t, window.cooloffRefuses(expired, cooloffListing{}), false)

	AssertEqual(t, len(log.linesWith("action=refuse")), 4)
}

// The escape's "holds no provider" counts the exits that are live and take new
// flows, or are kept from them only because they are full. A window holding
// only an exit that is done but not yet reaped, or only exits warned for
// anything else, skips the exclusions on that round instead of waiting a round
// for the empty-round path. A full window is not starving: it keeps them.
func TestCooloffWindowWithoutALiveExitHoldsNoProvider(t *testing.T) {
	failed := RequireMultiHopId(NewId())
	cases := []struct {
		name    string
		exit    func() *multiClientChannel
		skipped bool
	}{
		{name: "live", exit: cooloffTestHeldChannel, skipped: false},
		{name: "warned", exit: func() *multiClientChannel {
			client := cooloffTestHeldChannel()
			client.setWarning(true, warnUnhealthy)
			return client
		}, skipped: true},
		{name: "full", exit: func() *multiClientChannel {
			client := cooloffTestHeldChannel()
			client.setWarning(true, warnCapacity)
			return client
		}, skipped: false},
		{name: "quarantined", exit: func() *multiClientChannel {
			client := cooloffTestHeldChannel()
			client.quarantined = true
			return client
		}, skipped: true},
		{name: "done", exit: func() *multiClientChannel {
			client := cooloffTestHeldChannel()
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			client.ctx = ctx
			return client
		}, skipped: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			window := recentExitBareWindow(&recentExitTestGenerator{}, unfixedTestProfile(WindowTypeSpeed))
			window.noteFailedDestination(failed, cooloffReasonEvaluation)
			exit := c.exit()
			window.clients[exit.ClientId()] = exit

			excluded, count, listing := window.discoveryRoundExclusions(false, nil)
			AssertEqual(t, listing.skipped, c.skipped)
			AssertEqual(t, excluded[failed], !c.skipped)
			AssertEqual(t, count, map[bool]int{false: 1, true: 0}[c.skipped])
		})
	}
}

// A family-aware generator's discovery with a context, which a demand read
// asks.
type cooloffFamilyContextTestGenerator struct {
	recentExitTestGenerator
}

func (self *cooloffFamilyContextTestGenerator) NextDestinationsWithIpFamilyContext(ctx context.Context, count int, excludeDestinations []MultiHopId, rankMode string, ipFamily IpFamilyFilter) (map[MultiHopId]DestinationStats, error) {
	return map[MultiHopId]DestinationStats{}, nil
}

// An empty answer lets the starvation escape skip the next round's exclusions
// only when the round asked the whole pool. A probe asks only the v6-capable
// request, and a demand read for an IPv6 flow asks a family-aware generator
// for one v6-capable candidate: a pool of providers that carry only IPv4
// answers either with nothing, and counting that lifted the cool-off in a pool
// that had plenty else to offer.
func TestCooloffRoundAsksThePoolUnlessItAskedOnlyForIpv6(t *testing.T) {
	familyAware := recentExitBareWindow(&cooloffFamilyContextTestGenerator{}, unfixedTestProfile(WindowTypeSpeed))
	legacy := recentExitBareWindow(&recentExitTestGenerator{}, unfixedTestProfile(WindowTypeSpeed))
	ipv4 := &discoveryDemandRead{need: flowNeed{ipVersion: 4}}
	ipv6 := &discoveryDemandRead{need: flowNeed{ipVersion: 6}}

	AssertEqual(t, familyAware.cooloffRoundAsksThePool(false, nil), true)
	AssertEqual(t, familyAware.cooloffRoundAsksThePool(true, nil), false)
	AssertEqual(t, familyAware.cooloffRoundAsksThePool(false, ipv4), true)
	AssertEqual(t, familyAware.cooloffRoundAsksThePool(false, ipv6), false)
	// without the family filter a demand read asks the whole pool
	AssertEqual(t, legacy.cooloffRoundAsksThePool(false, ipv6), true)
}

// Only a round that asks the pool spends the skip an empty round left. A probe,
// or a family-filtered IPv6 demand read, landing between the empty round and
// the next ordinary one applies the exclusions and leaves the skip for it, so
// the escape is not a round late.
func TestCooloffRoundThatAsksOnlyForIpv6LeavesTheSkip(t *testing.T) {
	window := recentExitBareWindow(&cooloffFamilyContextTestGenerator{}, unfixedTestProfile(WindowTypeSpeed))
	failed := RequireMultiHopId(NewId())
	window.noteFailedDestination(failed, cooloffReasonEvaluation)
	held := cooloffTestHeldChannel()
	window.clients[held.ClientId()] = held

	cases := []struct {
		name       string
		probeOnly  bool
		demandRead *discoveryDemandRead
	}{
		{name: "probe", probeOnly: true},
		{name: "ipv6 demand read", demandRead: &discoveryDemandRead{need: flowNeed{ipVersion: 6}}},
	}
	for _, c := range cases {
		// a round that applied the exclusions came back empty
		_, count, listing := window.discoveryRoundExclusions(false, nil)
		AssertEqual(t, listing.skipped, false)
		window.noteCooloffRound(count, true)

		// the round that asks only for IPv6 applies them and leaves the skip
		excluded, _, listing := window.discoveryRoundExclusions(c.probeOnly, c.demandRead)
		if listing.skipped || !excluded[failed] {
			t.Errorf("%s: spent the skip an empty round left (skipped=%v excluded=%v)", c.name, listing.skipped, excluded[failed])
		}

		// the next round that asks the pool spends it
		excluded, _, listing = window.discoveryRoundExclusions(false, nil)
		if !listing.skipped || excluded[failed] {
			t.Errorf("%s: the next round that asks the pool did not skip (skipped=%v excluded=%v)", c.name, listing.skipped, excluded[failed])
		}
	}
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
	window.noteFailedDestination(destination, cooloffReasonEvaluation)
	AssertEqual(t, cooloffTestNoted(window, destination), true)
	window.rebuildWindow(45 * time.Second)
	AssertEqual(t, cooloffTestNoted(window, destination), false)
	if lines := log.linesWith("action=clear"); len(lines) != 1 || !strings.Contains(lines[0], "reason=rebuild") {
		t.Fatalf("want one clear line naming the rebuild, got %v", lines)
	}

	aborted := outcomeTestWindow(ctx, newRecordingLogger())
	aborted.armOutcome()
	aborted.noteFailedDestination(destination, cooloffReasonEvaluation)
	aborted.noteClientAdded(nil)
	aborted.rebuildWindow(45 * time.Second)
	AssertEqual(t, cooloffTestNoted(aborted, destination), true)
}

// A network change clears every window's cool-off, as it starts the expansion
// backoff over: the providers a window found failing failed on the old path,
// and a phone that lost its uplink must not come back to windows that leave
// them out for the rest of the cool-off.
func TestCooloffClearedByANetworkChange(t *testing.T) {
	parent, _, closeParent := groupTestParent(t, DisableSecurityPolicy())
	defer closeParent()
	log := newRecordingLogger()
	failed := RequireMultiHopId(NewId())
	for _, kind := range []WindowType{WindowTypeQuality, WindowTypeSpeed} {
		window := &multiClientWindow{
			ctx: parent.ctx, log: log, settings: parent.settings,
			windowType: kind, clients: map[Id]*multiClientChannel{},
			resizeMonitor: NewMonitor(),
		}
		window.noteFailedDestination(failed, cooloffReasonEvaluation)
		AssertEqual(t, cooloffTestNoted(window, failed), true)
		parent.windows[kind] = window
	}
	parent.NotifyNetworkChanged()
	for _, window := range parent.windows {
		if cooloffTestNoted(window, failed) {
			t.Errorf("the %s window kept its cool-off across a network change", window.windowName())
		}
	}
	AssertEqual(t, len(log.linesWith("action=clear reason=network_change")), 2)
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
	AssertEqual(t, cooloff.addExclusions(excluded, start.Add(timeout-time.Second), timeout), 2)
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
	AssertEqual(t, len(cooloff.entries), 1)
}

// A fixed destination set is never cooled off: its own retry backoff governs
// it. Nor is anything with the cool-off turned off.
func TestCooloffSkipsFixedDestinationsAndCanBeTurnedOff(t *testing.T) {
	destination := RequireMultiHopId(NewId())

	fixed := recentExitBareWindow(&recentExitFixedTestGenerator{}, unfixedTestProfile(WindowTypeSpeed))
	fixed.noteFailedDestination(destination, cooloffReasonEvaluation)
	excluded := map[MultiHopId]bool{}
	fixed.addCooloffExclusions(excluded, 1, true)
	AssertEqual(t, len(excluded), 0)

	off := recentExitBareWindow(&recentExitTestGenerator{}, unfixedTestProfile(WindowTypeSpeed))
	off.settings.EvaluationFailureCooloff = 0
	off.noteFailedDestination(destination, cooloffReasonEvaluation)
	off.addCooloffExclusions(excluded, 1, true)
	AssertEqual(t, len(excluded), 0)

	on := recentExitBareWindow(&recentExitTestGenerator{}, unfixedTestProfile(WindowTypeSpeed))
	on.noteFailedDestination(destination, cooloffReasonEvaluation)
	on.addCooloffExclusions(excluded, 1, true)
	AssertEqual(t, excluded, map[MultiHopId]bool{destination: true})
}
