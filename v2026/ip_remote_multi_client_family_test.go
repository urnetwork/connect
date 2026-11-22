package connect

// Tests for the address-family half of the window (IPV6.md B1–B6, D3): the
// soft v6-capable minimum and its exemptions, the family-aware discovery
// order and starvation backoff, the placement predicate on every candidate
// path, the local v6 downgrade, the Ipv6Available flag, and the v6 no-route
// reply.

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// familyTestGenerator is a family-aware generator with a configurable
// population per category. NextDestinationsWithIpFamily mirrors the
// platform's selection: capable filters draw dualstack first, then the
// single family; exact filters draw one category. The plain NextDestinations
// is the older server's answer: every provider, category unknown (legacy).
// Every call is recorded so a test can assert exactly what was asked for.
type familyTestGenerator struct {
	testingEmptyMultiClientGenerator
	mutex      sync.Mutex
	population map[IpFamily][]MultiHopId
	calls      []familyTestCall
}

type familyTestCall struct {
	filter IpFamilyFilter
	count  int
	plain  bool
}

func newFamilyTestGenerator(counts map[IpFamily]int) *familyTestGenerator {
	population := map[IpFamily][]MultiHopId{}
	for family, count := range counts {
		for range count {
			population[family] = append(population[family], RequireMultiHopId(NewId()))
		}
	}
	return &familyTestGenerator{population: population}
}

func (self *familyTestGenerator) recordCall(call familyTestCall) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.calls = append(self.calls, call)
}

func (self *familyTestGenerator) recordedCalls() []familyTestCall {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return slices.Clone(self.calls)
}

func (self *familyTestGenerator) draw(families []IpFamily, count int, excludeDestinations []MultiHopId, stamp bool) map[MultiHopId]DestinationStats {
	excluded := map[MultiHopId]bool{}
	for _, destination := range excludeDestinations {
		excluded[destination] = true
	}
	destinations := map[MultiHopId]DestinationStats{}
	for _, family := range families {
		for _, destination := range self.population[family] {
			if count <= len(destinations) {
				return destinations
			}
			if excluded[destination] {
				continue
			}
			stats := DestinationStats{}
			if stamp {
				stats.IpFamily = family
			}
			destinations[destination] = stats
		}
	}
	return destinations
}

func (self *familyTestGenerator) NextDestinations(count int, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
	self.recordCall(familyTestCall{plain: true, count: count})
	return self.draw([]IpFamily{IpFamilyDualstack, IpFamilyV4Only, IpFamilyV6Only}, count, excludeDestinations, false), nil
}

func (self *familyTestGenerator) NextDestinationsWithIpFamily(count int, excludeDestinations []MultiHopId, rankMode string, ipFamily IpFamilyFilter) (map[MultiHopId]DestinationStats, error) {
	self.recordCall(familyTestCall{filter: ipFamily, count: count})
	var families []IpFamily
	switch ipFamily {
	case IpFamilyFilterDefault, IpFamilyFilterV4Capable:
		families = []IpFamily{IpFamilyDualstack, IpFamilyV4Only}
	case IpFamilyFilterV6Capable:
		families = []IpFamily{IpFamilyDualstack, IpFamilyV6Only}
	case IpFamilyFilterDualstack:
		families = []IpFamily{IpFamilyDualstack}
	case IpFamilyFilterV4Only:
		families = []IpFamily{IpFamilyV4Only}
	case IpFamilyFilterV6Only:
		families = []IpFamily{IpFamilyV6Only}
	default:
		return nil, fmt.Errorf("unknown ip family filter %q", ipFamily)
	}
	return self.draw(families, count, excludeDestinations, true), nil
}

// familyTestWindow is a bare quality window over the generator with the
// notify seams the family code touches.
func familyTestWindow(ctx context.Context, generator MultiClientGenerator, settings *MultiClientSettings) *multiClientWindow {
	return &multiClientWindow{
		ctx:              ctx,
		log:              NewNoopLogger(),
		generator:        generator,
		windowType:       WindowTypeQuality,
		settings:         settings,
		clients:          map[Id]*multiClientChannel{},
		monitor:          NewRemoteUserNatMultiClientMonitorWithDefaults(),
		resizeMonitor:    NewMonitor(),
		generatorMonitor: NewMonitor(),
	}
}

// familyTestChannel is a bare channel of one category, added and unwarned,
// with the stats every candidate path reads. Each channel owns its own
// underlying client: a channel is keyed by that client's id.
func familyTestChannel(t *testing.T, ctx context.Context, settings *MultiClientSettings, family IpFamily) *multiClientChannel {
	t.Helper()
	parent := familyTestClient(t, ctx)
	return &multiClientChannel{
		ctx:      ctx,
		log:      NewNoopLogger(),
		client:   parent,
		settings: settings,
		args: &multiClientChannelArgs{
			MultiClientGeneratorClientArgs: MultiClientGeneratorClientArgs{ClientId: parent.ClientId()},
			Destination:                    RequireMultiHopId(NewId()),
			DestinationStats:               DestinationStats{IpFamily: family},
		},
		eventBuckets:              []*multiClientEventBucket{},
		ip4DestinationSourceCount: map[Ip4Path]map[Ip4Path]int{},
		ip6DestinationSourceCount: map[Ip6Path]map[Ip6Path]int{},
		packetStats:               &clientWindowStats{log: NewNoopLogger()},
	}
}

func familyTestClient(t *testing.T, ctx context.Context) *Client {
	t.Helper()
	clientSettings := DefaultClientSettings()
	clientSettings.Log = NewNoopLogger()
	parent := NewClient(ctx, NewId(), NewNoContractClientOob(), clientSettings)
	t.Cleanup(parent.Close)
	return parent
}

func familyTestFilters(calls []familyTestCall) []IpFamilyFilter {
	filters := []IpFamilyFilter{}
	for _, call := range calls {
		if call.plain {
			filters = append(filters, "plain")
		} else {
			filters = append(filters, call.filter)
		}
	}
	return filters
}

// --- B1: the soft minimum and its exemptions ---

func TestIpv6CapableShortfall(t *testing.T) {
	for _, c := range []struct {
		name             string
		target           int
		fixedDestination bool
		inactive         bool
		held             int
		want             int
	}{
		{name: "forming or full without IPv6", target: 1, want: 1},
		{name: "satisfied", target: 1, held: 1},
		{name: "partial target", target: 3, held: 1, want: 2},
		{name: "fixed destination", target: 1, fixedDestination: true},
		{name: "inactive profile window", target: 1, inactive: true},
		{name: "no requested target"},
	} {
		if got := ipv6CapableShortfall(c.target, c.fixedDestination, c.inactive, c.held); got != c.want {
			t.Errorf("%s: shortfall %d, want %d", c.name, got, c.want)
		}
	}
	negative := WindowSizeSettings{WindowSizeMinIpv6Capable: -1}
	if err := negative.Validate(); err == nil {
		t.Error("negative IPv6 minimum validated")
	}
}

// The hard minimum ignores a window whose only exits are v6-only: such a
// window is not connected for v4 traffic, whatever its size. A v6-only exit
// beside a v4-capable one counts, and a fixed destination counts whatever it
// holds.
func TestHardMinimumCountIgnoresV6OnlyWindow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()

	v6Only := familyTestChannel(t, ctx, settings, IpFamilyV6Only)
	v6Only2 := familyTestChannel(t, ctx, settings, IpFamilyV6Only)
	dualstack := familyTestChannel(t, ctx, settings, IpFamilyDualstack)
	legacy := familyTestChannel(t, ctx, settings, IpFamilyLegacy)

	if got := hardMinimumCount([]*multiClientChannel{v6Only, v6Only2}, false); got != 0 {
		t.Errorf("v6-only window counted %d toward the hard minimum, want 0", got)
	}
	if got := hardMinimumCount([]*multiClientChannel{v6Only, v6Only2}, true); got != 2 {
		t.Errorf("fixed destination v6-only window counted %d, want 2", got)
	}
	if got := hardMinimumCount([]*multiClientChannel{v6Only, dualstack}, false); got != 2 {
		t.Errorf("mixed window counted %d, want 2", got)
	}
	if got := hardMinimumCount([]*multiClientChannel{legacy}, false); got != 1 {
		t.Errorf("legacy window counted %d, want 1", got)
	}

	window := familyTestWindow(ctx, newFamilyTestGenerator(nil), settings)
	window.clients[v6Only.ClientId()] = v6Only
	if got := window.hardMinimumClientCount(false); got != 0 {
		t.Errorf("window of one v6-only exit counted %d, want 0", got)
	}
	window.clients[dualstack.ClientId()] = dualstack
	if got := window.hardMinimumClientCount(false); got != 2 {
		t.Errorf("window with a dualstack exit counted %d, want 2", got)
	}
	// the window's min-satisfied verdict is judged on that count with the
	// hard minimum untouched by the soft one
	if windowMinSatisfied(2, hardMinimumCount([]*multiClientChannel{v6Only, v6Only2}, false), 0, false, false, 0) {
		t.Error("a v6-only window satisfied the hard minimum")
	}
}

// --- B2: discovery order, the single v6-capable request, starvation ---

// The main fill asks for v4-capable providers, which the platform draws
// dualstack first. With an open v6 shortfall the window asks for exactly one
// v6-capable candidate ahead of the main fill, sized to the shortfall, and
// that candidate leads the push order.
func TestEnumerateDestinationsFamilyOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()
	settings.WindowExpandBlockCount = 4
	generator := newFamilyTestGenerator(map[IpFamily]int{
		IpFamilyDualstack: 1,
		IpFamilyV4Only:    5,
		IpFamilyV6Only:    2,
	})
	window := familyTestWindow(ctx, generator, settings)

	// no shortfall: one v4-capable request, dualstack drawn first
	destinations, err := window.enumerateDestinations(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := familyTestFilters(generator.recordedCalls()); !slices.Equal(got, []IpFamilyFilter{IpFamilyFilterV4Capable}) {
		t.Fatalf("no-shortfall calls = %v, want [v4-capable]", got)
	}
	if len(destinations) != 4 {
		t.Fatalf("no-shortfall destinations = %d, want 4", len(destinations))
	}
	// the platform draws dualstack first, so the one dualstack provider is
	// in a four-of-six answer; order inside one answer is the map's and
	// carries nothing
	dualstackDrawn := false
	for _, destination := range destinations {
		if !destination.stats.IpFamily.SupportsIpv4() {
			t.Errorf("a v4-capable fill returned %q", destination.stats.IpFamily)
		}
		if destination.stats.IpFamily == IpFamilyDualstack {
			dualstackDrawn = true
		}
	}
	if !dualstackDrawn {
		t.Error("the v4-capable fill did not draw the dualstack provider first")
	}

	// an open shortfall of one: the v6-capable request first, sized one,
	// then the main fill; the v6-capable candidate leads the order and is
	// not repeated by the main fill
	generator.calls = nil
	window.setIpv6Shortfall(1)
	destinations, err = window.enumerateDestinations(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	calls := generator.recordedCalls()
	if got := familyTestFilters(calls); !slices.Equal(got, []IpFamilyFilter{IpFamilyFilterV6Capable, IpFamilyFilterV4Capable}) {
		t.Fatalf("shortfall calls = %v, want [v6-capable v4-capable]", got)
	}
	if calls[0].count != 1 {
		t.Errorf("v6-capable request count = %d, want 1 (the shortfall)", calls[0].count)
	}
	if !destinations[0].stats.IpFamily.SupportsIpv6() {
		t.Errorf("first destination family = %q, want the v6-capable candidate first", destinations[0].stats.IpFamily)
	}
	seen := map[MultiHopId]int{}
	for _, destination := range destinations {
		seen[destination.destination] += 1
	}
	for destination, count := range seen {
		if count != 1 {
			t.Errorf("destination %v pushed %d times", destination, count)
		}
	}

	// the resize-pass probe: the v6-capable request alone
	generator.calls = nil
	destinations, err = window.enumerateDestinations(nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := familyTestFilters(generator.recordedCalls()); !slices.Equal(got, []IpFamilyFilter{IpFamilyFilterV6Capable}) {
		t.Fatalf("probe calls = %v, want [v6-capable]", got)
	}
	if len(destinations) != 1 || !destinations[0].stats.IpFamily.SupportsIpv6() {
		t.Errorf("probe returned %d destinations, want the one v6-capable candidate", len(destinations))
	}
}

// A family the platform has nothing for is starved: the window stops asking
// for it until IpFamilyStarvedRetryTimeout has passed, then asks once more.
// A swap victim drain-warned on the strength of a fill that is not coming is
// released.
func TestEnumerateDestinationsStarvation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()
	settings.IpFamilyStarvedRetryTimeout = 100 * time.Millisecond
	generator := newFamilyTestGenerator(map[IpFamily]int{IpFamilyV4Only: 5})
	window := familyTestWindow(ctx, generator, settings)
	victim := familyTestChannel(t, ctx, settings, IpFamilyV4Only)
	victim.markFamilySwapVictim()
	window.clients[victim.ClientId()] = victim

	window.setIpv6Shortfall(1)
	if _, err := window.enumerateDestinations(nil, false); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if !window.ipv6Starved(now) {
		t.Fatal("an empty v6-capable answer did not starve the family")
	}
	if window.ipv6FillWanted(now) != 0 {
		t.Error("a starved family still wants a fill")
	}
	if victim.isFamilySwapVictim() {
		t.Error("starvation did not release the swap victim")
	}

	// while starved, only the main fill is asked for
	generator.calls = nil
	if _, err := window.enumerateDestinations(nil, false); err != nil {
		t.Fatal(err)
	}
	if got := familyTestFilters(generator.recordedCalls()); !slices.Equal(got, []IpFamilyFilter{IpFamilyFilterV4Capable}) {
		t.Fatalf("starved calls = %v, want [v4-capable]", got)
	}

	// after the retry timeout the family is asked for again
	time.Sleep(150 * time.Millisecond)
	if window.ipv6Starved(time.Now()) {
		t.Fatal("the family stayed starved past the retry timeout")
	}
	generator.calls = nil
	if _, err := window.enumerateDestinations(nil, false); err != nil {
		t.Fatal(err)
	}
	if got := familyTestFilters(generator.recordedCalls()); !slices.Equal(got, []IpFamilyFilter{IpFamilyFilterV6Capable, IpFamilyFilterV4Capable}) {
		t.Fatalf("post-timeout calls = %v, want [v6-capable v4-capable]", got)
	}

	// two failed v6-capable evaluations in a row starve the family too
	window2 := familyTestWindow(ctx, newFamilyTestGenerator(map[IpFamily]int{IpFamilyV6Only: 1}), settings)
	window2.setIpv6Shortfall(1)
	window2.noteIpv6CandidateFailed()
	if window2.ipv6Starved(time.Now()) {
		t.Error("one failed candidate starved the family")
	}
	window2.noteIpv6CandidateFailed()
	if !window2.ipv6Starved(time.Now()) {
		t.Error("two failed candidates did not starve the family")
	}
	window2.noteIpv6CandidateAdmitted()
	window2.ipv6FillFailures = 0
}

// A generator without the family capability is called exactly as before, its
// destinations read as legacy (v4 only), and a probe round asks it nothing.
func TestEnumerateDestinationsLegacyGenerator(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()
	generator := &legacyFamilyTestGenerator{
		inner: newFamilyTestGenerator(map[IpFamily]int{IpFamilyDualstack: 2}),
	}
	window := familyTestWindow(ctx, generator, settings)
	window.setIpv6Shortfall(1)

	destinations, err := window.enumerateDestinations(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := familyTestFilters(generator.recordedCalls()); !slices.Equal(got, []IpFamilyFilter{"plain"}) {
		t.Fatalf("legacy calls = %v, want [plain]", got)
	}
	if len(destinations) != 2 {
		t.Fatalf("legacy destinations = %d, want 2", len(destinations))
	}
	for _, destination := range destinations {
		if destination.stats.IpFamily != IpFamilyLegacy || destination.stats.IpFamily.SupportsIpv6() {
			t.Errorf("legacy destination family = %q, want legacy carrying v4 only", destination.stats.IpFamily)
		}
	}
	generator.inner.calls = nil
	destinations, err = window.enumerateDestinations(nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(destinations) != 0 || len(generator.recordedCalls()) != 0 {
		t.Errorf("a probe round asked a legacy generator (%d calls, %d destinations)", len(generator.recordedCalls()), len(destinations))
	}
}

// legacyFamilyTestGenerator hides the family population behind the plain
// interface only: it embeds the empty generator, not familyTestGenerator, so
// the optional capability's method is not promoted and the window's type
// assertion for it fails, exactly like a generator built before the field.
type legacyFamilyTestGenerator struct {
	testingEmptyMultiClientGenerator
	inner *familyTestGenerator
}

func (self *legacyFamilyTestGenerator) NextDestinations(count int, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
	return self.inner.NextDestinations(count, excludeDestinations, rankMode)
}

func (self *legacyFamilyTestGenerator) recordedCalls() []familyTestCall {
	return self.inner.recordedCalls()
}

// The capacity collapse and the swap victim prefer to keep dualstack exits:
// single-family exits are shed first, and only an exit that cannot carry v6
// gives its slot to a v6-capable candidate.
func TestCollapseFamilyOrderAndSwapVictim(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()

	dualstack := familyTestChannel(t, ctx, settings, IpFamilyDualstack)
	v4Only := familyTestChannel(t, ctx, settings, IpFamilyV4Only)
	v6Only := familyTestChannel(t, ctx, settings, IpFamilyV6Only)
	legacy := familyTestChannel(t, ctx, settings, IpFamilyLegacy)

	// descending weight within a family class, dualstack always first
	clients := []*multiClientChannel{v4Only, dualstack, legacy, v6Only}
	weights := map[*multiClientChannel]float32{v4Only: 10, dualstack: 1, legacy: 5, v6Only: 7}
	slices.SortFunc(clients, func(a, b *multiClientChannel) int {
		if c := collapseFamilyOrder(a, b); c != 0 {
			return c
		}
		if weights[a] < weights[b] {
			return 1
		} else if weights[b] < weights[a] {
			return -1
		}
		return 0
	})
	if clients[0] != dualstack {
		t.Errorf("collapse order keeps %q first, want dualstack", clients[0].IpFamily())
	}
	if clients[len(clients)-1] != legacy {
		t.Errorf("collapse order sheds %q last, want the lightest single-family exit", clients[len(clients)-1].IpFamily())
	}

	window := familyTestWindow(ctx, newFamilyTestGenerator(nil), settings)
	flows := map[*multiClientChannel]int{}
	window.flowCountFunc = func(client *multiClientChannel) int { return flows[client] }

	// flowless single-family exits first, lightest wins; dualstack is never
	// a victim, and neither is a v6-only exit (it already carries v6)
	victim, flowless := window.selectFamilySwapVictim([]*multiClientChannel{dualstack, v4Only, legacy, v6Only}, weights)
	if victim != legacy || !flowless {
		t.Errorf("victim = %v flowless=%t, want the lightest flowless single-family exit (legacy)", victim, flowless)
	}
	// loaded exits: the one carrying the least traffic, drain-warned
	flows[legacy] = 3
	flows[v4Only] = 8
	victim, flowless = window.selectFamilySwapVictim([]*multiClientChannel{dualstack, v4Only, legacy, v6Only}, weights)
	if victim != legacy || flowless {
		t.Errorf("victim = %v flowless=%t, want the lightest loaded single-family exit (legacy)", victim, flowless)
	}
	victim, _ = window.selectFamilySwapVictim([]*multiClientChannel{dualstack, v6Only}, weights)
	if victim != nil {
		t.Errorf("victim = %v, want none when every exit carries v6", victim)
	}

	// admission: the candidate that closes the shortfall outranks a
	// qualified one; with no shortfall the order is poolAdmitOrder's
	closes := []bool{false, false, true}
	qualified := []bool{false, true, false}
	if got := poolAdmitOrderWithShortfall(closes, qualified, 3); !slices.Equal(got, []int{2, 1, 0}) {
		t.Errorf("admit order with shortfall = %v, want [2 1 0]", got)
	}
	if got := poolAdmitOrderWithShortfall(nil, qualified, 3); !slices.Equal(got, poolAdmitOrder(qualified, 3)) {
		t.Errorf("admit order without shortfall = %v, want poolAdmitOrder %v", got, poolAdmitOrder(qualified, 3))
	}
	if got := poolAdmitOrderWithShortfall(closes, qualified, 1); !slices.Equal(got, []int{2}) {
		t.Errorf("admit order count 1 = %v, want [2]", got)
	}
}

// --- B3: the placement predicate on every candidate path ---

func TestIpFamilySupportsIpVersion(t *testing.T) {
	cases := []struct {
		family IpFamily
		v4     bool
		v6     bool
	}{
		{IpFamilyDualstack, true, true},
		{IpFamilyV4Only, true, false},
		{IpFamilyV6Only, false, true},
		{IpFamilyLegacy, true, false},
		{IpFamily("something-newer"), true, false},
	}
	for _, c := range cases {
		if got := c.family.SupportsIpVersion(4); got != c.v4 {
			t.Errorf("%q supports v4 = %t, want %t", c.family, got, c.v4)
		}
		if got := c.family.SupportsIpVersion(6); got != c.v6 {
			t.Errorf("%q supports v6 = %t, want %t", c.family, got, c.v6)
		}
		if c.family.SupportsIpVersion(5) {
			t.Errorf("%q supports version 5", c.family)
		}
	}
	bare := &multiClientChannel{}
	if !bare.supportsFlow(4, IpProtocolUnknown) || bare.supportsFlow(6, IpProtocolUnknown) || !bare.supportsFlow(0, IpProtocolUnknown) {
		t.Error("a bare channel must read as legacy: v4 and any, never v6")
	}
}

// A v6 flow never lands on a v4-only or legacy exit, a v4 flow never on a
// v6-only exit, on every path that offers candidates: the window's ordered
// offer, the cross-tier offer, the last-resort offer, the race candidates
// and the plain narrowing helper.
func TestCandidatePathsRespectIpVersion(t *testing.T) {
	forEachIpVersion(t, func(t *testing.T, ipVersion int) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		settings := DefaultMultiClientSettings()
		generator := newFamilyTestGenerator(nil)

		families := []IpFamily{IpFamilyDualstack, IpFamilyV4Only, IpFamilyV6Only, IpFamilyLegacy}
		all := []*multiClientChannel{}
		window := familyTestWindow(ctx, generator, settings)
		for _, family := range families {
			client := familyTestChannel(t, ctx, settings, family)
			window.clients[client.ClientId()] = client
			all = append(all, client)
		}
		multiClient := &RemoteUserNatMultiClient{
			ctx:                ctx,
			cancel:             cancel,
			log:                NewNoopLogger(),
			generator:          generator,
			settings:           settings,
			windows:            map[WindowType]*multiClientWindow{WindowTypeQuality: window},
			ip4PathUpdates:     map[Ip4Path]*multiClientChannelUpdate{},
			ip6PathUpdates:     map[Ip6Path]*multiClientChannelUpdate{},
			affinityIp4Paths:   map[Ip4Path]map[Ip4Path]time.Time{},
			affinityIp6Paths:   map[Ip6Path]map[Ip6Path]time.Time{},
			clientUpdates:      map[*multiClientChannel]map[*multiClientChannelUpdate]bool{},
			reliabilityMetrics: newReliabilityMetrics(),
		}

		check := func(name string, candidates []*multiClientChannel) {
			t.Helper()
			wantCount := 0
			for _, client := range all {
				if client.IpFamily().SupportsIpVersion(ipVersion) {
					wantCount += 1
				}
			}
			if len(candidates) != wantCount {
				t.Errorf("%s: %d candidates for v%d, want %d", name, len(candidates), ipVersion, wantCount)
			}
			for _, client := range candidates {
				if !client.IpFamily().SupportsIpVersion(ipVersion) {
					t.Errorf("%s: a %q exit was offered to a v%d flow", name, client.IpFamily(), ipVersion)
				}
			}
		}
		need := flowNeed{ipVersion: ipVersion}
		check("OrderedClientsForFlow", window.OrderedClientsForFlow(need))
		check("orderedClientsCrossTierForFlow", window.orderedClientsCrossTierForFlow(need))
		check("lastResortClientsForFlow", window.lastResortClientsForFlow(need))
		check("raceCandidates", multiClient.raceCandidates(window, need))
		check("candidatesForFlow", candidatesForFlow(all, need))

		// the version-agnostic offers still return everything
		if got := len(window.OrderedClients()); got != len(all) {
			t.Errorf("OrderedClients = %d, want every exit (%d)", got, len(all))
		}
		if got := len(candidatesForFlow(all, flowNeed{})); got != len(all) {
			t.Errorf("candidatesForFlow(any) = %d, want every exit (%d)", got, len(all))
		}
	})
}

// --- B5 retired: no local downgrade (server FP2-PROTOCOL.md D11.8) ---

// B5's old trigger -- v6 strikes across distinct destinations while v4 stays
// healthy -- leaves a dualstack exit's labels alone end to end: its category,
// the monitor's provider event, the exit readout, its v6 placement and the
// window's v6 availability are all unchanged, no family_downgrade event is
// published and the window opens no v6 shortfall. The strikes still count
// toward the ordinary starvation warning. Pre-change, the third strike
// latched the exit to v4-only and republished it.
func TestIpv6DialStrikesLeaveTheServerLabels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()
	settings.DialFailureRerace = false
	window := familyTestWindow(ctx, newFamilyTestGenerator(nil), settings)
	client := familyTestChannel(t, ctx, settings, IpFamilyDualstack)
	window.clients[client.ClientId()] = client

	log := newRecordingLogger()
	multiClient := &RemoteUserNatMultiClient{
		ctx:                ctx,
		cancel:             cancel,
		log:                log,
		settings:           settings,
		windows:            map[WindowType]*multiClientWindow{WindowTypeQuality: window},
		ip4PathUpdates:     map[Ip4Path]*multiClientChannelUpdate{},
		ip6PathUpdates:     map[Ip6Path]*multiClientChannelUpdate{},
		clientUpdates:      map[*multiClientChannel]map[*multiClientChannelUpdate]bool{},
		reliabilityMetrics: newReliabilityMetrics(),
	}
	multiClient.SetReceivePacketCallback(func(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {})

	// three v6 flows, to two distinct destinations, all bound to the exit: an
	// exit strikes each flow once, so three strikes take three flows
	first := icmpTcpTestPath(6)
	second := icmpTcpTestPath(6)
	second.DestinationIp = udpTestPath(6).SourceIp
	third := icmpTcpTestPath(6)
	third.SourcePort += 1
	multiClient.clientUpdates[client] = map[*multiClientChannelUpdate]bool{}
	for _, path := range []*IpPath{first, second, third} {
		update := &multiClientChannelUpdate{ipPath: path}
		update.client.Store(client)
		multiClient.ip6PathUpdates[path.ToIp6Path()] = update
		multiClient.clientUpdates[client][update] = true
	}
	// v4 stays healthy: a proven v4 connect, the "v4 healthy" half of B5
	client.addConnectSuccess(4)

	window.monitor.AddProviderEvent(client.ClientId(), ProviderStateAdded, client.Destination().Tail(), nil, client.IpFamily())
	addedEvent := window.monitor.ProviderEvents()[client.ClientId()]
	resizeNotify := window.resizeMonitor.NotifyChannel()

	multiClient.clientDialFailureSignal(client, first)
	multiClient.clientDialFailureSignal(client, second)
	multiClient.clientDialFailureSignal(client, third)

	if got := client.dialFailureCount(); got != 3 {
		t.Fatalf("dial strikes = %d, want 3: the strike record is kept", got)
	}
	if got := client.IpFamily(); got != IpFamilyDualstack {
		t.Errorf("family after three v6 strikes = %q, want the server's dualstack", got)
	}
	if got := client.capabilities(); got.Ip != ProviderIpCapabilityDualstack {
		t.Errorf("labels after three v6 strikes = %+v, want the server's dualstack", got)
	}
	if !client.supportsFlow(6, IpProtocolTcp) {
		t.Error("the exit stopped carrying v6 flows on strikes alone")
	}
	if event := window.monitor.ProviderEvents()[client.ClientId()]; event != addedEvent {
		t.Errorf("monitor event = %+v, want the Added event untouched", event)
	}
	select {
	case <-resizeNotify:
		t.Error("the strikes woke the resize pass to fill a v6 shortfall")
	default:
	}
	if exits := multiClient.Exits(); len(exits) != 1 || exits[0].IpFamily != IpFamilyDualstack {
		t.Errorf("exits = %+v, want one dualstack exit", exits)
	}
	if !window.ipv6Available() || multiClient.Ipv6Unroutable() {
		t.Error("the window stopped reading as v6 available on strikes alone")
	}
	if candidates := window.OrderedClientsForFlow(flowNeed{ipVersion: 6, ipProtocol: IpProtocolTcp}); len(candidates) != 1 {
		t.Errorf("v6 placement offers %d exits, want the dualstack exit", len(candidates))
	}
	if 0 < len(log.linesWith("family_downgrade")) {
		t.Error("a family_downgrade event was published")
	}
}

// Udp flows whose datagrams get no answer leave the protocol label alone:
// no client-side state lowers a label (D7.5), so the exit still carries udp
// until a fresh find-providers2 answer relabels it.
func TestUnansweredUdpLeavesTheProtocolLabel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()
	window := familyTestWindow(ctx, newFamilyTestGenerator(nil), settings)
	client := familyTestChannel(t, ctx, settings, IpFamilyV4Only)
	window.clients[client.ClientId()] = client
	for i := range 4 {
		path := udpTestPath(4)
		path.SourcePort += i
		client.addDialFailure(path.DestinationIp.String(), path.Version)
	}
	if got := client.capabilities().Protocol; got != ProviderProtocolCapabilityDualproto {
		t.Errorf("protocol label after unanswered udp = %q, want dualproto", got)
	}
	if !client.supportsFlow(4, IpProtocolUdp) || !window.udpAvailable() {
		t.Error("unanswered udp lowered the exit's udp placement")
	}
}

// A label changes only when a fresh find-providers2 answer relabels the
// entry: an echoed answer's labels replace the remembered ones a redial
// would use, and a legacy answer leaves them.
func TestOnlyAFreshAnswerRelabelsAnExit(t *testing.T) {
	remembered := DestinationStats{
		Tier:         2,
		IpFamily:     IpFamilyDualstack,
		Capabilities: ProviderCapabilities{Ip: ProviderIpCapabilityDualstack, Protocol: ProviderProtocolCapabilityDualproto},
	}
	fresh := DestinationStats{
		IpFamily:     IpFamilyV4Only,
		Capabilities: ProviderCapabilities{Ip: ProviderIpCapabilityIpv4, Protocol: ProviderProtocolCapabilityTcp},
	}
	relabelled := relabelledStats(remembered, fresh, true)
	if relabelled.IpFamily != IpFamilyV4Only || relabelled.Capabilities != fresh.Capabilities || relabelled.Tier != 2 {
		t.Errorf("selector-mode answer relabelled to %+v, want the fresh labels on the remembered stats", relabelled)
	}
	if kept := relabelledStats(remembered, fresh, false); !reflect.DeepEqual(kept, remembered) {
		t.Errorf("legacy answer changed the remembered stats to %+v", kept)
	}
}

// --- B6: Ipv6Available and the no-route reply ---

func TestIpv6AvailablePerWindowAndMerged(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()
	window := familyTestWindow(ctx, newFamilyTestGenerator(nil), settings)
	multiClient := &RemoteUserNatMultiClient{
		windows: map[WindowType]*multiClientWindow{WindowTypeQuality: window},
	}

	if window.ipv6Available() || multiClient.Ipv6Available() {
		t.Fatal("an empty window reads as v6 available")
	}
	v4Only := familyTestChannel(t, ctx, settings, IpFamilyV4Only)
	window.clients[v4Only.ClientId()] = v4Only
	if window.ipv6Available() {
		t.Fatal("a v4-only exit reads as v6 available")
	}
	v6Only := familyTestChannel(t, ctx, settings, IpFamilyV6Only)
	window.clients[v6Only.ClientId()] = v6Only
	if !window.ipv6Available() || !multiClient.Ipv6Available() {
		t.Fatal("a v6-only exit does not read as v6 available")
	}
	v6Only.setWarning(true, warnUnhealthy)
	if window.ipv6Available() {
		t.Fatal("a warned v6-capable exit reads as v6 available")
	}
	dualstack := familyTestChannel(t, ctx, settings, IpFamilyDualstack)
	window.clients[dualstack.ClientId()] = dualstack
	if !multiClient.Ipv6Available() {
		t.Fatal("a dualstack exit does not read as v6 available")
	}

	// the monitor flag: per window, merged by OR
	quality := NewRemoteUserNatMultiClientMonitorWithDefaults()
	speed := NewRemoteUserNatMultiClientMonitorWithDefaults()
	merged := NewMergedMultiClientMonitor([]MultiClientMonitor{quality, speed})
	quality.AddWindowExpandEvent(true, 6, false)
	speed.AddWindowExpandEvent(true, 1, false)
	if merged.WindowExpandEvent().Ipv6Available {
		t.Error("merged Ipv6Available is true with no window reporting it")
	}
	speed.AddWindowExpandEvent(true, 1, true)
	if !merged.WindowExpandEvent().Ipv6Available {
		t.Error("merged Ipv6Available is false with one window reporting it")
	}
	if !speed.WindowExpandEvent().Ipv6Available || quality.WindowExpandEvent().Ipv6Available {
		t.Error("per-window Ipv6Available did not follow its own event")
	}
}

// Once the windows have formed, a v6 flow with no v6-capable exit anywhere
// is answered with an icmpv6 unreachable as if from the destination, so the
// application fails fast and falls back to v4. A v4 flow, a v6 flow while a
// v6-capable exit exists, and a v6 flow while the windows are still forming
// get no such reply.
func TestIpv6NoRouteReply(t *testing.T) {
	multiClient, _, closeParent := groupTestParent(t, DisableSecurityPolicy())
	defer closeParent()
	ctx := multiClient.ctx

	var mutex sync.Mutex
	replies := []*receivePacket{}
	multiClient.SetReceivePacketCallback(func(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {
		mutex.Lock()
		defer mutex.Unlock()
		replies = append(replies, &receivePacket{
			Source:      source,
			ProvideMode: provideMode,
			IpPath:      ipPath,
			Packet:      slices.Clone(packet),
		})
	})
	multiClient.groupRaceCandidatesForTest = func(group *parsedPacketGroup) []*multiClientChannel {
		return nil
	}
	replyCount := func() int {
		mutex.Lock()
		defer mutex.Unlock()
		return len(replies)
	}
	send := func(path *IpPath) {
		t.Helper()
		packet := MessagePoolCopy(ipOosTcpPacketSequence(path, tcpFlagSyn, path.SequenceNumber, nil))
		group := requireGroupTestPacketGroup(t, packet)
		multiClient.sendPacketGroup(SourceId(NewId()), protocol.ProvideMode_Network, group, 50*time.Millisecond)
	}
	egress := icmpTcpTestPath(6)

	// still forming: no exit at all, no reply
	if multiClient.Ipv6Unroutable() {
		t.Fatal("empty windows read as v6 unroutable")
	}
	send(egress)
	if replyCount() != 0 {
		t.Fatalf("a v6 flow was answered no-route while the windows were still forming")
	}

	// formed with a v4-only exit: unroutable, one reply carrying the flow
	window := familyTestWindow(ctx, newFamilyTestGenerator(nil), multiClient.settings)
	v4Only := familyTestChannel(t, ctx, multiClient.settings, IpFamilyV4Only)
	window.clients[v4Only.ClientId()] = v4Only
	multiClient.windows[WindowTypeQuality] = window
	if !multiClient.Ipv6Unroutable() {
		t.Fatal("a formed v4-only window does not read as v6 unroutable")
	}
	send(egress)
	mutex.Lock()
	got := slices.Clone(replies)
	mutex.Unlock()
	if len(got) != 1 {
		t.Fatalf("replies = %d, want exactly one no-route reply", len(got))
	}
	parsed, ok := ipParseIcmpUnreachable(got[0].Packet)
	if !ok {
		t.Fatal("the reply is not an icmp unreachable")
	}
	if parsed.Version != 6 || parsed.ToIp6Path() != egress.ToIp6Path() {
		t.Errorf("reply embeds %+v, want the failed v6 flow %+v", parsed, egress)
	}
	if got[0].IpPath == nil || got[0].IpPath.Version != 6 {
		t.Error("the reply was delivered without the flow's v6 path")
	}
	if multiClient.Ipv6NoRouteCount() != 1 {
		t.Errorf("Ipv6NoRouteCount = %d, want 1", multiClient.Ipv6NoRouteCount())
	}

	// a v6-capable exit exists (even if it is not a candidate right now):
	// no reply, the flow waits for the ordinary retry pacing instead
	dualstack := familyTestChannel(t, ctx, multiClient.settings, IpFamilyDualstack)
	window.clients[dualstack.ClientId()] = dualstack
	if multiClient.Ipv6Unroutable() {
		t.Fatal("a window with a dualstack exit reads as v6 unroutable")
	}
	send(egress)

	// a v4 flow with no candidate is never answered this way
	delete(window.clients, dualstack.ClientId())
	send(icmpTcpTestPath(4))

	if replyCount() != 1 {
		t.Errorf("replies = %d, want still 1: no reply while v6 is available, none for v4", replyCount())
	}
	if multiClient.Ipv6NoRouteCount() != 1 {
		t.Errorf("Ipv6NoRouteCount = %d, want still 1", multiClient.Ipv6NoRouteCount())
	}
	if multiClient.replyIpv6NoRoute(nil) {
		t.Error("a nil path replied")
	}
	if multiClient.replyIpv6NoRoute(icmpTcpTestPath(4)) {
		t.Error("a v4 path replied with a v6 no-route")
	}
}

// --- categories on the readouts ---

func TestExitInfoAndProviderEventCarryIpFamily(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()
	window := familyTestWindow(ctx, newFamilyTestGenerator(nil), settings)
	multiClient := &RemoteUserNatMultiClient{
		ctx:            ctx,
		log:            NewNoopLogger(),
		settings:       settings,
		windows:        map[WindowType]*multiClientWindow{WindowTypeQuality: window},
		ip4PathUpdates: map[Ip4Path]*multiClientChannelUpdate{},
		ip6PathUpdates: map[Ip6Path]*multiClientChannelUpdate{},
	}
	byFamily := map[IpFamily]*multiClientChannel{}
	for _, family := range []IpFamily{IpFamilyDualstack, IpFamilyV4Only, IpFamilyV6Only, IpFamilyLegacy} {
		client := familyTestChannel(t, ctx, settings, family)
		window.clients[client.ClientId()] = client
		byFamily[family] = client
		window.monitor.AddProviderEvent(client.ClientId(), ProviderStateAdded, client.Destination().Tail(), nil, client.IpFamily())
	}

	exits := multiClient.Exits()
	if len(exits) != 4 {
		t.Fatalf("exits = %d, want 4", len(exits))
	}
	events := window.monitor.ProviderEvents()
	for family, client := range byFamily {
		index := slices.IndexFunc(exits, func(exit *ExitInfo) bool { return exit.ClientId == client.ClientId() })
		if index < 0 {
			t.Fatalf("no exit for the %q client", family)
		}
		if exits[index].IpFamily != family {
			t.Errorf("exit family = %q, want %q", exits[index].IpFamily, family)
		}
		if event := events[client.ClientId()]; event == nil || event.IpFamily != family {
			t.Errorf("provider event family = %v, want %q", event, family)
		}
	}

	// the monitor rewrite is a no-op for unknown ids and unchanged categories
	if window.monitor.SetProviderIpFamily(NewId(), IpFamilyV4Only) {
		t.Error("an unknown client id reported a category change")
	}
	if window.monitor.SetProviderIpFamily(byFamily[IpFamilyV4Only].ClientId(), IpFamilyV4Only) {
		t.Error("an unchanged category reported a change")
	}
	if !window.monitor.SetProviderIpFamily(byFamily[IpFamilyDualstack].ClientId(), IpFamilyV4Only) {
		t.Error("a changed category was not reported")
	}
	// removal carries the exit's category, the server's label
	var removed *ProviderEvent
	var removedMutex sync.Mutex
	unsub := window.monitor.AddMonitorEventCallback(func(windowExpandEvent *WindowExpandEvent, providerEvents map[Id]*ProviderEvent, reset bool) {
		removedMutex.Lock()
		defer removedMutex.Unlock()
		for _, event := range providerEvents {
			if event.State == ProviderStateRemoved {
				removed = event
			}
		}
	})
	defer unsub()
	window.clientRemoveCallback = func(client *multiClientChannel) {}
	window.removeClients(byFamily[IpFamilyDualstack])
	deadline := time.Now().Add(time.Second)
	for {
		removedMutex.Lock()
		event := removed
		removedMutex.Unlock()
		if event != nil {
			if event.IpFamily != IpFamilyDualstack {
				t.Errorf("removed event family = %q, want the server's dualstack", event.IpFamily)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no removed event was published")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A fixed-destination exit (a user-selected peer, whose category is always
// legacy because discovery is bypassed) carries every version: the user
// chose it with no alternative to prefer. It reads as v6 available so a
// peer session is never answered no-route, and it is never a swap victim.
func TestFixedDestinationExitCarriesEveryVersion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()
	peer := familyTestChannel(t, ctx, settings, IpFamilyLegacy)
	peer.args.FixedDestination = true

	forEachIpVersion(t, func(t *testing.T, ipVersion int) {
		if !peer.supportsFlow(ipVersion, IpProtocolUnknown) {
			t.Errorf("a fixed-destination legacy exit refused v%d", ipVersion)
		}
	})
	if peer.IpFamily() != IpFamilyLegacy {
		t.Errorf("fixed exit family = %q, want legacy (the readout is unchanged)", peer.IpFamily())
	}

	window := familyTestWindow(ctx, &orderedClientsTestGenerator{fixedSize: 1, fixedIsSet: true}, settings)
	window.clients[peer.ClientId()] = peer
	if !window.ipv6Available() {
		t.Error("a fixed-destination window does not read as v6 available")
	}
	multiClient := &RemoteUserNatMultiClient{windows: map[WindowType]*multiClientWindow{WindowTypeQuality: window}}
	if multiClient.Ipv6Unroutable() {
		t.Error("a peer session reads as v6 unroutable")
	}
	if victim, _ := window.selectFamilySwapVictim([]*multiClientChannel{peer}, map[*multiClientChannel]float32{}); victim != nil {
		t.Error("a fixed-destination exit was chosen as a swap victim")
	}
	if got := len(window.OrderedClientsForFlow(flowNeed{ipVersion: 6})); got != 1 {
		t.Errorf("fixed window offered %d exits to a v6 flow, want 1", got)
	}
}
