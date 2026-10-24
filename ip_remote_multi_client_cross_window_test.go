// Public packet entry points over actual quality and speed windows reproduce
// the all-failed first-window fallback. Providers and routes stay in memory.
package connect

import (
	"context"
	"net"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Adds the window a provider belongs to without changing the shared rerace
// fixture or its original quality-only discovery behavior.
type privateCrossWindowExitSpec struct {
	exitSpec           reraceExitSpec
	windowType         WindowType
	reputationFailures []string
}

// Uses the shared real NoContract/Route construction, with explicit rank-mode
// discovery for each window. Every destination has one retained owner.
type privateCrossWindowGenerator struct {
	*reraceGenerator
	exitWindowTypes        map[*reraceExit]WindowType
	exitReputationFailures map[*reraceExit][]string
	unavailableExits       map[*reraceExit]bool
	fixedDestinations      bool
}

// Enumerates only the requested window and family, using the same exclusions
// and identity custody as the existing real in-memory rerace fixture.
func (self *privateCrossWindowGenerator) draw(
	families []IpFamily,
	count int,
	excludeDestinations []MultiHopId,
	rankMode string,
	stamp bool,
) map[MultiHopId]DestinationStats {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	destinations := map[MultiHopId]DestinationStats{}
	held := map[*reraceExit]bool{}
	for _, exit := range self.clientExits {
		held[exit] = true
	}
	for _, exit := range self.exits {
		if count <= len(destinations) {
			break
		}
		if self.exitWindowTypes[exit].RankMode() != rankMode || self.unavailableExits[exit] || held[exit] ||
			slices.Contains(excludeDestinations, exit.destination) ||
			families != nil && !slices.Contains(families, exit.family) {
			continue
		}
		stats := DestinationStats{
			Tier:               exit.tier,
			ReputationFailures: slices.Clone(self.exitReputationFailures[exit]),
		}
		if stamp {
			stats.IpFamily = exit.family
		}
		destinations[exit.destination] = stats
	}
	return destinations
}

// Explicit membership retains the production constructor's quality-only
// rule; dynamic experiments keep both windows.
func (self *privateCrossWindowGenerator) FixedDestinationSize() (int, bool) {
	if !self.fixedDestinations {
		return 0, false
	}
	count := 0
	for _, windowType := range self.exitWindowTypes {
		if windowType == WindowTypeQuality {
			count++
		}
	}
	return count, true
}

// Implements ordinary discovery without touching the platform or network.
func (self *privateCrossWindowGenerator) NextDestinations(count int, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
	return self.draw(nil, count, excludeDestinations, rankMode, false), nil
}

// Implements the actual constructor's family-aware discovery capability.
func (self *privateCrossWindowGenerator) NextDestinationsWithIpFamily(count int, excludeDestinations []MultiHopId, rankMode string, ipFamily IpFamilyFilter) (map[MultiHopId]DestinationStats, error) {
	families := []IpFamily{IpFamilyDualstack, IpFamilyV4Only}
	if ipFamily == IpFamilyFilterV6Capable {
		families = []IpFamily{IpFamilyDualstack, IpFamilyV6Only}
	}
	return self.draw(families, count, excludeDestinations, rankMode, true), nil
}

// Optional barriers observe real construction and computed fixture readiness.
// They never change window publication, production knobs or timer state.
type privateCrossWindowHarnessOptions struct {
	fixedDestinations     bool
	afterConstruct        func(*reraceHarness)
	inspectExpectedCounts func(map[WindowType]int)
}

// The ordinary fixture has no observation barrier or explicit membership.
func newPrivateCrossWindowHarness(t *testing.T, specs []privateCrossWindowExitSpec, configure func(*MultiClientSettings), fixedDestinations ...bool) *reraceHarness {
	t.Helper()
	return newPrivateCrossWindowHarnessWithOptions(t, specs, configure, privateCrossWindowHarnessOptions{
		fixedDestinations: len(fixedDestinations) != 0 && fixedDestinations[0],
	})
}

// Builds actual windows at their exact fixture caps. Publication and optional
// observation barriers precede readiness; cleanup joins every owned worker.
func newPrivateCrossWindowHarnessWithOptions(t *testing.T, specs []privateCrossWindowExitSpec, configure func(*MultiClientSettings), options privateCrossWindowHarnessOptions) *reraceHarness {
	t.Helper()
	resetBackendDegraded()
	t.Cleanup(resetBackendDegraded)
	ctx, cancel := context.WithCancel(t.Context())
	harness := &reraceHarness{
		t:         t,
		ctx:       ctx,
		generator: &reraceGenerator{clientExits: map[Id]*reraceExit{}, unsubs: map[Id]func(){}},
		log:       newRecordingLogger(),
		source:    SourceId(NewId()),
		synTimes:  map[string][]time.Time{},
	}
	generator := &privateCrossWindowGenerator{
		reraceGenerator:        harness.generator,
		exitWindowTypes:        map[*reraceExit]WindowType{},
		exitReputationFailures: map[*reraceExit][]string{},
		unavailableExits:       map[*reraceExit]bool{},
		fixedDestinations:      options.fixedDestinations,
	}
	windowCounts := map[WindowType]int{}
	for _, spec := range specs {
		exit := harness.addExit(spec.exitSpec)
		generator.exitWindowTypes[exit] = spec.windowType
		generator.exitReputationFailures[exit] = slices.Clone(spec.reputationFailures)
		windowCounts[spec.windowType]++
	}
	settings := DefaultMultiClientSettings()
	settings.Log = harness.log
	settings.ProviderProbe = false
	settings.HeartbeatInterval = 0
	settings.StandingReserve = false
	settings.StrictWindowSizeHardMax = true
	for _, windowType := range []WindowType{WindowTypeQuality, WindowTypeSpeed} {
		count := windowCounts[windowType]
		settings.WindowSizes[windowType] = WindowSizeSettings{
			WindowSizeMin:            count,
			WindowSizeMax:            count,
			WindowSizeHardMax:        count,
			WindowSizeReconnectScale: 1,
		}
	}
	if configure != nil {
		configure(settings)
	}
	harness.multiClient = NewRemoteUserNatMultiClient(ctx, generator, harness.receive, protocol.ProvideMode_Network, settings)
	t.Cleanup(func() {
		if t.Failed() {
			for _, line := range harness.log.lines() {
				t.Log(line)
			}
		}
		if err := harness.multiClient.CloseAndWait(context.Background()); err != nil {
			t.Errorf("close multi client: %s", err)
		}
		cancel()
		for _, client := range harness.generator.clientList() {
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Errorf("close channel client: %s", err)
			}
		}
		for _, exit := range harness.generator.exitList() {
			if err := exit.provider.CloseAndWait(context.Background()); err != nil {
				t.Errorf("close provider %s: %s", exit.name, err)
			}
		}
		harness.exitWorkers.Wait()
	})
	if options.afterConstruct != nil {
		options.afterConstruct(harness)
	}
	expectedCounts := map[WindowType]int{
		WindowTypeQuality: windowCounts[WindowTypeQuality],
		WindowTypeSpeed:   windowCounts[WindowTypeSpeed],
	}
	// Fixing window type restricts selection. Only a positive fixed size
	// disables the other window's resize target; an auto-sized fixed-type
	// profile still forms both ordinary minimum ownership counts.
	if fixedWindowType, profileWindowSize, fixed := settings.DefaultPerformanceProfile.FixedWindow(); fixed && 0 < profileWindowSize.FixedWindowSize {
		for _, windowType := range []WindowType{WindowTypeQuality, WindowTypeSpeed} {
			if windowType != fixedWindowType {
				expectedCounts[windowType] = 0
			}
		}
	}
	if generator.fixedDestinations {
		expectedCounts[WindowTypeSpeed] = 0
	}
	if options.inspectExpectedCounts != nil {
		options.inspectExpectedCounts(expectedCounts)
	}
	for _, windowType := range []WindowType{WindowTypeQuality, WindowTypeSpeed} {
		window := harness.multiClient.windows[windowType]
		if window == nil {
			if expectedCounts[windowType] != 0 {
				t.Fatalf("window %v was not constructed", windowType)
			}
			continue
		}
		deadline := time.After(time.Minute)
		for {
			var changed <-chan struct{}
			clients := window.orderedClients(true, 0, &changed)
			if len(clients) == expectedCounts[windowType] {
				break
			}
			select {
			case <-changed:
			case <-deadline:
				t.Fatalf("window %v formed %d candidates, want %d", windowType, len(clients), expectedCounts[windowType])
			}
		}
	}
	synctest.Wait()
	harness.start = time.Now()
	return harness
}

// Uses documentation addresses for both ends; the shared fixture's private
// source-address literals are never copied into this experiment's packets.
func privateCrossWindowFlow(version int, destinationIndex int, sourcePort int) *IpPath {
	path := reraceFlow(version, destinationIndex, sourcePort)
	if version == 4 {
		path.SourceIp = net.IPv4(192, 0, 2, 2).To4()
	} else {
		path.SourceIp = net.ParseIP("2001:db8:1::2")
	}
	return path
}

// Locates an actual retained owner in either window without bypassing any
// production candidate policy in the sends being tested.
func privateCrossWindowChannel(t *testing.T, harness *reraceHarness, name string) *multiClientChannel {
	t.Helper()
	destination := harness.exit(name).destination
	for _, window := range harness.multiClient.windows {
		for _, client := range window.unorderedClients() {
			if client.Destination() == destination && !client.IsDone() {
				return client
			}
		}
	}
	t.Fatalf("no retained owner for %s", name)
	return nil
}

// Reads exact standing service evidence after synctest joins every causal
// provider reply and client intercept; no short negative timeout is involved.
func privateCrossWindowAssertFailed(t *testing.T, harness *reraceHarness, path *IpPath, names ...string) {
	t.Helper()
	for _, name := range names {
		client := privateCrossWindowChannel(t, harness, name)
		key, ok := destinationServiceFailureKeyFor(path, client)
		if !ok {
			t.Fatal("fixture produced a malformed service key")
		}
		harness.multiClient.stateLock.Lock()
		entry, found := harness.multiClient.destinationServiceFailures[key]
		harness.multiClient.stateLock.Unlock()
		if !found || entry.count == 0 {
			t.Fatalf("%s has no completed standing service refusal", name)
		}
	}
}

// The causal regression: the same source SYN exhausts the quality window,
// then its next source retransmit uses a healthy speed exit. OLD retries the
// quality failures and deterministically fails the final delivery assertion.
func privateCrossWindowTestUntriedSpeedAfterQualityRefusal(t *testing.T, version int) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		harness := newPrivateCrossWindowHarness(t, []privateCrossWindowExitSpec{
			{exitSpec: reraceExitSpec{name: "quality-a", answer: reraceAlways(reraceSignal)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "quality-b", answer: reraceAlways(reraceSignal)}, windowType: WindowTypeQuality},
			{exitSpec: reraceExitSpec{name: "speed"}, windowType: WindowTypeSpeed},
		}, nil)
		path := privateCrossWindowFlow(version, 0, 43100+version)
		if !harness.syn(path) {
			t.Fatal("first source SYN was not admitted")
		}
		privateCrossWindowAssertFailed(t, harness, path, "quality-a", "quality-b")
		if got := len(harness.exit("speed").syns(path)); got != 0 {
			t.Fatalf("speed received %d SYNs before quality refusal", got)
		}
		if harness.boundTo(path) != nil || len(harness.delivered(path)) != 0 {
			t.Fatal("refused quality attempt became established or escaped to the app")
		}
		if !harness.syn(path) {
			t.Fatal("source retransmit was not admitted")
		}
		if _, ok := harness.synAck(path); !ok {
			t.Fatalf("causal regression: ipv%d source retransmit retried known-failed quality exits; quality SYN counts=%d/%d speed=%d", version, len(harness.exit("quality-a").syns(path)), len(harness.exit("quality-b").syns(path)), len(harness.exit("speed").syns(path)))
		}
		if harness.boundTo(path) != privateCrossWindowChannel(t, harness, "speed") ||
			len(harness.exit("speed").syns(path)) != 1 ||
			len(harness.exit("quality-a").syns(path)) != 1 || len(harness.exit("quality-b").syns(path)) != 1 {
			t.Fatal("source retransmit did not preserve the exhausted cohort while choosing speed once")
		}
		reraceAssertNoErrorDelivered(t, harness, path)
	})
}

// IPv4 causal root remains independent of the IPv6 OLD failure outcome.
func TestPrivateCrossWindowUntriedSpeedAfterQualityRefusalIp4(t *testing.T) {
	assertMessagePoolOwnership(t)
	privateCrossWindowTestUntriedSpeedAfterQualityRefusal(t, 4)
}

// IPv6 causal root runs even when the IPv4 OLD assertion fails.
func TestPrivateCrossWindowUntriedSpeedAfterQualityRefusalIp6(t *testing.T) {
	assertMessagePoolOwnership(t)
	privateCrossWindowTestUntriedSpeedAfterQualityRefusal(t, 6)
}
