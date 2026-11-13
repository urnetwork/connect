package connect

// Tests for the limited window and the windows' selector-mode requests
// (server FP2-PROTOCOL.md §7.4-§7.8, §12.5): its size from the normal
// windows, the `select` every window asks with, discovery in selector mode
// and against an older server, the limited window's rank mode across profile
// changes, the merged monitor, and the process counters.

import (
	"context"
	"math"
	"net"
	"slices"
	"sync"
	"testing"
	"time"
)

// --- fixtures ---

// A flow over documentation addresses.
func capabilityTestPath(ipVersion int, ipProtocol IpProtocol, sourcePort int) *IpPath {
	path := &IpPath{
		Version:         ipVersion,
		Protocol:        ipProtocol,
		SourcePort:      sourcePort,
		DestinationPort: 443,
	}
	if ipVersion == 4 {
		path.SourceIp = net.IPv4(192, 0, 2, 10).To4()
		path.DestinationIp = net.IPv4(203, 0, 113, 10).To4()
	} else {
		path.SourceIp = net.ParseIP("2001:db8::10")
		path.DestinationIp = net.ParseIP("2001:db8:1::10")
	}
	return path
}

// A bare window of the type over the generator.
func capabilityTestWindow(ctx context.Context, generator MultiClientGenerator, settings *MultiClientSettings, windowType WindowType) *multiClientWindow {
	window := familyTestWindow(ctx, generator, settings)
	window.windowType = windowType
	return window
}

// A bare added, unwarned exit of the window type with the server's labels.
func capabilityTestChannel(t *testing.T, ctx context.Context, settings *MultiClientSettings, windowType WindowType, capabilities ProviderCapabilities) *multiClientChannel {
	t.Helper()
	client := familyTestChannel(t, ctx, settings, capabilities.IpFamily())
	client.args.DestinationStats.Capabilities = capabilities
	client.args.windowType = windowType
	return client
}

// A bare parent over the windows.
func capabilityTestParent(ctx context.Context, cancel context.CancelFunc, generator MultiClientGenerator, settings *MultiClientSettings, windows map[WindowType]*multiClientWindow) *RemoteUserNatMultiClient {
	parent := &RemoteUserNatMultiClient{
		ctx:                ctx,
		cancel:             cancel,
		log:                NewNoopLogger(),
		generator:          generator,
		settings:           settings,
		windows:            windows,
		ip4PathUpdates:     map[Ip4Path]*multiClientChannelUpdate{},
		ip6PathUpdates:     map[Ip6Path]*multiClientChannelUpdate{},
		affinityIp4Paths:   map[Ip4Path]map[Ip4Path]time.Time{},
		affinityIp6Paths:   map[Ip6Path]map[Ip6Path]time.Time{},
		clientUpdates:      map[*multiClientChannel]map[*multiClientChannelUpdate]bool{},
		reliabilityMetrics: newReliabilityMetrics(),
	}
	parent.config.Store(&multiClientConfig{})
	parent.limitedWindowSizeFractionBits.Store(math.Float64bits(settings.LimitedWindowSizeFraction))
	return parent
}

// Wires the limited window's hooks as the constructor does, once the parent
// holds every window.
func capabilityTestWireLimitedWindow(parent *RemoteUserNatMultiClient) {
	limitedWindow := parent.windows[WindowTypeLimited]
	hooks := &limitedWindowHooks{
		sizing: parent.limitedWindowSizing,
		wake: func() {
			limitedWindow.resizeMonitor.NotifyAll()
		},
	}
	for _, window := range parent.windows {
		window.limitedHooks.Store(hooks)
	}
}

// One recorded selector-mode request.
type capabilityTestCall struct {
	count          int
	ipFamily       IpFamilyFilter
	rankMode       string
	providerSelect FindProvidersSelect
	clientId       *Id
}

// A capability-aware generator: each request is recorded and answered by
// answer, echoing `select` while echo is set, as a server in selector mode
// does. SelectorMode follows the answers, as the api generator's does.
type capabilityTestGenerator struct {
	testingEmptyMultiClientGenerator
	mutex  sync.Mutex
	echo   bool
	answer func(call capabilityTestCall) map[MultiHopId]DestinationStats
	calls  []capabilityTestCall
	mode   providerSelectorMode
}

func newCapabilityTestGenerator(echo bool, answer func(call capabilityTestCall) map[MultiHopId]DestinationStats) *capabilityTestGenerator {
	return &capabilityTestGenerator{
		echo:   echo,
		answer: answer,
	}
}

func (self *capabilityTestGenerator) ask(call capabilityTestCall) (map[MultiHopId]DestinationStats, bool, error) {
	self.mutex.Lock()
	self.calls = append(self.calls, call)
	echo := self.echo
	answer := self.answer
	self.mutex.Unlock()
	destinations := map[MultiHopId]DestinationStats{}
	if answer != nil {
		destinations = answer(call)
	}
	self.mode.observe(echo)
	return destinations, echo, nil
}

func (self *capabilityTestGenerator) recordedCalls() []capabilityTestCall {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return slices.Clone(self.calls)
}

func (self *capabilityTestGenerator) NextDestinationsWithCapabilities(count int, excludeDestinations []MultiHopId, rankMode string, ipFamily IpFamilyFilter, providerSelect FindProvidersSelect) (map[MultiHopId]DestinationStats, bool, error) {
	return self.ask(capabilityTestCall{count: count, ipFamily: ipFamily, rankMode: rankMode, providerSelect: providerSelect})
}

func (self *capabilityTestGenerator) NextDestinationsWithCapabilitiesContext(ctx context.Context, count int, excludeDestinations []MultiHopId, rankMode string, ipFamily IpFamilyFilter, providerSelect FindProvidersSelect) (map[MultiHopId]DestinationStats, bool, error) {
	return self.ask(capabilityTestCall{count: count, ipFamily: ipFamily, rankMode: rankMode, providerSelect: providerSelect})
}

func (self *capabilityTestGenerator) NextDestinationsForClientIdWithCapabilities(clientId Id, excludeDestinations []MultiHopId, rankMode string, providerSelect FindProvidersSelect) (map[MultiHopId]DestinationStats, bool, error) {
	return self.ask(capabilityTestCall{count: 1, rankMode: rankMode, providerSelect: providerSelect, clientId: &clientId})
}

// The by-id form a window checks for before it asks in selector mode.
func (self *capabilityTestGenerator) NextDestinationsForClientId(clientId Id, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
	destinations, _, err := self.ask(capabilityTestCall{count: 1, rankMode: rankMode, clientId: &clientId})
	return destinations, err
}

func (self *capabilityTestGenerator) SelectorMode() (bool, bool) {
	return self.mode.get()
}

// A fixed destination set with nothing to list.
type capabilityTestFixedGenerator struct {
	testingEmptyMultiClientGenerator
}

func (self *capabilityTestFixedGenerator) FixedDestinationSize() (int, bool) {
	return 1, true
}

// One labelled destination per class, keyed for the answer.
func capabilityTestClassDestinations() map[MultiHopId]DestinationStats {
	destinations := map[MultiHopId]DestinationStats{}
	for _, capabilities := range []ProviderCapabilities{capabilityClassA, capabilityClassB, capabilityClassC, capabilityClassD} {
		destinations[RequireMultiHopId(NewId())] = DestinationStats{
			IpFamily:     capabilities.IpFamily(),
			Capabilities: capabilities,
		}
	}
	return destinations
}

// --- sizing ---

// The limited window reads its size from the normal windows' last resize
// passes: half the largest active target (desktop auto: quality 6 and speed
// 1 give 3), the hard max likewise; an inactive window of a fixed profile
// never counts.
func TestLimitedWindowSizingFromTheNormalWindows(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()
	generator := newCapabilityTestGenerator(true, nil)
	quality := capabilityTestWindow(ctx, generator, settings, WindowTypeQuality)
	speed := capabilityTestWindow(ctx, generator, settings, WindowTypeSpeed)
	limited := capabilityTestWindow(ctx, generator, settings, WindowTypeLimited)
	parent := capabilityTestParent(ctx, cancel, generator, settings, map[WindowType]*multiClientWindow{
		WindowTypeQuality: quality,
		WindowTypeSpeed:   speed,
		WindowTypeLimited: limited,
	})
	capabilityTestWireLimitedWindow(parent)

	// before any normal pass: nothing to fill
	if size := limited.limitedWindowSize(); size.WindowSizeMax != 0 {
		t.Fatalf("limited window sized %d before any normal window computed a target", size.WindowSizeMax)
	}

	wake := limited.resizeMonitor.NotifyChannel()
	quality.publishSizing(windowSizing{published: true, active: true, targetSize: 6, hardMax: 6})
	select {
	case <-wake:
	default:
		t.Fatal("a normal window's new target did not wake the limited window")
	}
	speed.publishSizing(windowSizing{published: true, active: true, targetSize: 1, hardMax: 4})
	if size := limited.limitedWindowSize(); size.WindowSizeMax != 3 || size.WindowSizeHardMax != 3 {
		t.Errorf("desktop auto: target %d hard max %d, want 3 and 3", size.WindowSizeMax, size.WindowSizeHardMax)
	}
	// an unchanged publication wakes nothing
	wake = limited.resizeMonitor.NotifyChannel()
	speed.publishSizing(windowSizing{published: true, active: true, targetSize: 1, hardMax: 4})
	select {
	case <-wake:
		t.Error("an unchanged target woke the limited window")
	default:
	}

	// a fixed Speed profile of 4: quality is inactive and does not count
	quality.publishSizing(windowSizing{published: true, active: false, targetSize: 6, hardMax: 6})
	speed.publishSizing(windowSizing{published: true, active: true, targetSize: 4, hardMax: 0})
	if size := limited.limitedWindowSize(); size.WindowSizeMax != 2 || size.WindowSizeHardMax != 0 {
		t.Errorf("fixed profile of 4: target %d hard max %d, want 2 and unbounded", size.WindowSizeMax, size.WindowSizeHardMax)
	}

	// the cap
	settings.LimitedWindowSizeMax = 1
	if size := limited.limitedWindowSize(); size.WindowSizeMax != 1 || size.WindowSizeHardMax != 1 {
		t.Errorf("capped: target %d hard max %d, want 1 and 1", size.WindowSizeMax, size.WindowSizeHardMax)
	}
	settings.LimitedWindowSizeMax = 0

	// a runtime fraction of 0 turns it off
	parent.SetLimitedWindowSizeFraction(0)
	if size := limited.limitedWindowSize(); size.WindowSizeMax != 0 {
		t.Errorf("fraction 0: target %d, want none", size.WindowSizeMax)
	}
	parent.SetLimitedWindowSizeFraction(0.5)
	if size := limited.limitedWindowSize(); size.WindowSizeMax != 2 {
		t.Errorf("fraction back at 0.5: target %d, want 2", size.WindowSizeMax)
	}
}

// The limited window does not fill: from a server known not to echo
// `select` (until an answer echoes it), without a capability-aware generator,
// or under the user's Fixed IP.
func TestLimitedWindowDoesNotFillWhereItCannot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()

	build := func(generator MultiClientGenerator) (*multiClientWindow, *multiClientWindow) {
		quality := capabilityTestWindow(ctx, generator, settings, WindowTypeQuality)
		limited := capabilityTestWindow(ctx, generator, settings, WindowTypeLimited)
		parent := capabilityTestParent(ctx, cancel, generator, settings, map[WindowType]*multiClientWindow{
			WindowTypeQuality: quality,
			WindowTypeLimited: limited,
		})
		capabilityTestWireLimitedWindow(parent)
		quality.publishSizing(windowSizing{published: true, active: true, targetSize: 6, hardMax: 6})
		return quality, limited
	}

	// an older server: the first answer without the echo turns it off, an
	// echo turns it back on
	generator := newCapabilityTestGenerator(false, nil)
	_, limited := build(generator)
	if size := limited.limitedWindowSize(); size.WindowSizeMax != 3 {
		t.Fatalf("before any answer: target %d, want 3", size.WindowSizeMax)
	}
	generator.ask(capabilityTestCall{})
	if size := limited.limitedWindowSize(); size.WindowSizeMax != 0 {
		t.Errorf("after an answer without the echo: target %d, want none", size.WindowSizeMax)
	}
	generator.mutex.Lock()
	generator.echo = true
	generator.mutex.Unlock()
	generator.ask(capabilityTestCall{})
	if size := limited.limitedWindowSize(); size.WindowSizeMax != 3 {
		t.Errorf("after an echoed answer: target %d, want 3", size.WindowSizeMax)
	}

	// a generator without labels
	_, limited = build(newFamilyTestGenerator(nil))
	if size := limited.limitedWindowSize(); size.WindowSizeMax != 0 {
		t.Errorf("without a capability-aware generator: target %d, want none", size.WindowSizeMax)
	}

	// the user's Fixed IP
	_, limited = build(newCapabilityTestGenerator(true, nil))
	limited.performanceProfile = &PerformanceProfile{
		WindowType: WindowTypeQuality,
		WindowSize: WindowSizeSettings{WindowSizeMin: 1, WindowSizeMax: 1, FixedWindowSize: 1},
	}
	if size := limited.limitedWindowSize(); size.WindowSizeMax != 0 {
		t.Errorf("under the Fixed IP: target %d, want none", size.WindowSizeMax)
	}
}

// A multi client builds the limited window beside quality and speed, and a
// fixed destination none.
func TestMultiClientBuildsTheLimitedWindow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()
	settings.Log = NewNoopLogger()

	multi := NewRemoteUserNatMultiClient(ctx, newCapabilityTestGenerator(true, nil), nil, 0, settings)
	if multi.windows[WindowTypeLimited] == nil || len(multi.windows) != 3 {
		t.Errorf("windows %d, want quality, speed and limited", len(multi.windows))
	}
	if multi.LimitedWindowSizeFraction() != 0.5 {
		t.Errorf("fraction %v, want the default 0.5", multi.LimitedWindowSizeFraction())
	}
	multi.Close()

	fixed := NewRemoteUserNatMultiClient(ctx, &capabilityTestFixedGenerator{}, nil, 0, settings)
	defer fixed.Close()
	if fixed.windows[WindowTypeLimited] != nil || fixed.LimitedWindowSize() != 0 || fixed.LimitedWindowTargetSize() != 0 {
		t.Error("a fixed destination built a limited window")
	}
}

// --- requests ---

// What a window's requests carry before its first resize pass: the
// configured target and ipv6 minimum; the limited window nothing, which holds
// its listing.
func TestInitialRequestSizing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()
	generator := newCapabilityTestGenerator(true, nil)
	cases := []struct {
		name           string
		windowType     WindowType
		profile        *PerformanceProfile
		wantTarget     int
		wantDualstack  int
		wantHeldListed bool
	}{
		{name: "quality in auto", windowType: WindowTypeQuality, wantTarget: 6, wantDualstack: 1},
		{name: "speed in auto", windowType: WindowTypeSpeed, wantTarget: 1, wantDualstack: 0},
		{name: "limited", windowType: WindowTypeLimited, wantTarget: 0, wantDualstack: 0, wantHeldListed: true},
		{
			name:          "the active window of a fixed speed profile",
			windowType:    WindowTypeSpeed,
			profile:       &PerformanceProfile{WindowType: WindowTypeSpeed, WindowSize: WindowSizeSettings{WindowSizeMin: 4, WindowSizeMax: 4, FixedWindowSize: 4}},
			wantTarget:    4,
			wantDualstack: 1,
		},
		{
			name:       "the inactive window of a fixed speed profile",
			windowType: WindowTypeQuality,
			profile:    &PerformanceProfile{WindowType: WindowTypeSpeed, WindowSize: WindowSizeSettings{WindowSizeMin: 4, WindowSizeMax: 4, FixedWindowSize: 4}},
			wantTarget: 0,
		},
	}
	for _, c := range cases {
		window := capabilityTestWindow(ctx, generator, settings, c.windowType)
		window.performanceProfile = c.profile
		window.initRequestSizing()
		if window.requestTargetCount != c.wantTarget || window.requestDualstackTargetCount != c.wantDualstack {
			t.Errorf("%s: target %d dualstack %d, want %d and %d", c.name, window.requestTargetCount, window.requestDualstackTargetCount, c.wantTarget, c.wantDualstack)
		}
		if window.limitedListingHeld() != c.wantHeldListed {
			t.Errorf("%s: listing held %t, want %t", c.name, window.limitedListingHeld(), c.wantHeldListed)
		}
		if providerSelect := window.providerSelect(); providerSelect.TargetCount < 1 {
			t.Errorf("%s: target_count %d, the server refuses one below 1", c.name, providerSelect.TargetCount)
		}
	}
}

// The `select` a window asks with (FP2-PROTOCOL.md §7.4): a normal window's
// dualstack and dualproto with its target, held dualproto clients, ipv6
// minimum and held dualstack clients; the limited window's ipv4 and tcp with
// its target, held tcp clients and no ipv6 need. A warned client holds no
// slot.
func TestProviderSelectCountsTheHeldLabels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()
	generator := newCapabilityTestGenerator(true, nil)

	quality := capabilityTestWindow(ctx, generator, settings, WindowTypeQuality)
	quality.setRequestSizing(6, 1)
	for _, capabilities := range []ProviderCapabilities{capabilityClassA, capabilityClassB, capabilityClassD} {
		client := capabilityTestChannel(t, ctx, settings, WindowTypeQuality, capabilities)
		quality.clients[client.ClientId()] = client
	}
	warned := capabilityTestChannel(t, ctx, settings, WindowTypeQuality, capabilityClassA)
	warned.setWarning(true, warnUnhealthy)
	quality.clients[warned.ClientId()] = warned
	want := FindProvidersSelect{
		Ip:                   ProviderIpCapabilityDualstack,
		Protocol:             ProviderProtocolCapabilityDualproto,
		TargetCount:          6,
		HeldCount:            2,
		DualstackTargetCount: 1,
		HeldDualstackCount:   1,
	}
	if got := quality.providerSelect(); got != want {
		t.Errorf("quality select %+v, want %+v", got, want)
	}

	speed := capabilityTestWindow(ctx, generator, settings, WindowTypeSpeed)
	speed.setRequestSizing(1, 0)
	if got := speed.providerSelect(); got.DualstackTargetCount != 0 || got.TargetCount != 1 || got.HeldCount != 0 {
		t.Errorf("speed select %+v, want target 1, nothing held and no ipv6 minimum", got)
	}

	limited := capabilityTestWindow(ctx, generator, settings, WindowTypeLimited)
	limited.setRequestSizing(3, 1)
	for _, capabilities := range []ProviderCapabilities{capabilityClassC, capabilityClassD} {
		client := capabilityTestChannel(t, ctx, settings, WindowTypeLimited, capabilities)
		limited.clients[client.ClientId()] = client
	}
	want = FindProvidersSelect{
		Ip:                   ProviderIpCapabilityIpv4,
		Protocol:             ProviderProtocolCapabilityTcp,
		TargetCount:          3,
		HeldCount:            2,
		DualstackTargetCount: 0,
		HeldDualstackCount:   1,
	}
	if got := limited.providerSelect(); got != want {
		t.Errorf("limited select %+v, want %+v", got, want)
	}
}

// --- discovery ---

// In selector mode a normal window asks once, v4-capable, with its `select`:
// the server serves the ipv6 need from that request, so no separate
// v6-capable request is sent even while the window's shortfall is open. An
// answer without a dualstack provider starves the family.
func TestNormalWindowAsksOnceInSelectorMode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()
	answerDualstack := true
	generator := newCapabilityTestGenerator(true, func(call capabilityTestCall) map[MultiHopId]DestinationStats {
		destinations := map[MultiHopId]DestinationStats{}
		capabilities := capabilityClassB
		if answerDualstack {
			capabilities = capabilityClassA
		}
		destinations[RequireMultiHopId(NewId())] = DestinationStats{IpFamily: capabilities.IpFamily(), Capabilities: capabilities}
		return destinations
	})
	generator.mode.observe(true)
	window := capabilityTestWindow(ctx, generator, settings, WindowTypeQuality)
	window.setRequestSizing(6, 1)
	window.setIpv6Shortfall(1)

	destinations, err := window.discoverDestinations(nil, false, "quality")
	if err != nil {
		t.Fatal(err)
	}
	calls := generator.recordedCalls()
	if len(calls) != 1 || calls[0].ipFamily != IpFamilyFilterV4Capable || calls[0].count != settings.WindowExpandBlockCount {
		t.Fatalf("calls %+v, want one v4-capable request", calls)
	}
	if calls[0].providerSelect.DualstackTargetCount != 1 || calls[0].providerSelect.Protocol != ProviderProtocolCapabilityDualproto {
		t.Errorf("select %+v, want the quality window's dualstack and dualproto with its ipv6 minimum", calls[0].providerSelect)
	}
	if len(destinations) != 1 || window.ipv6Starved(time.Now()) {
		t.Errorf("a dualstack answer: %d destinations, starved %t", len(destinations), window.ipv6Starved(time.Now()))
	}

	answerDualstack = false
	if _, err := window.discoverDestinations(nil, false, "quality"); err != nil {
		t.Fatal(err)
	}
	if !window.ipv6Starved(time.Now()) {
		t.Error("an answer without a dualstack provider did not starve the open shortfall")
	}
}

// The separate v6-capable request of IPV6.md B2 goes only to a server that
// does not echo `select`: ahead of the main fill once the server is known,
// after it on the answer that first shows it.
func TestShortfallRequestGoesOnlyToAServerWithoutTheEcho(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()
	answer := func(call capabilityTestCall) map[MultiHopId]DestinationStats {
		return map[MultiHopId]DestinationStats{RequireMultiHopId(NewId()): {}}
	}

	filters := func(calls []capabilityTestCall) []IpFamilyFilter {
		filters := []IpFamilyFilter{}
		for _, call := range calls {
			filters = append(filters, call.ipFamily)
		}
		return filters
	}

	// known: today's order, v6-capable first
	known := newCapabilityTestGenerator(false, answer)
	known.mode.observe(false)
	window := capabilityTestWindow(ctx, known, settings, WindowTypeQuality)
	window.setRequestSizing(6, 1)
	window.setIpv6Shortfall(1)
	if _, err := window.discoverDestinations(nil, false, "quality"); err != nil {
		t.Fatal(err)
	}
	if got := filters(known.recordedCalls()); !slices.Equal(got, []IpFamilyFilter{IpFamilyFilterV6Capable, IpFamilyFilterV4Capable}) {
		t.Errorf("a known older server got %v, want the v6-capable request then the main fill", got)
	}

	// first contact: the main fill, then the shortfall request
	first := newCapabilityTestGenerator(false, answer)
	window = capabilityTestWindow(ctx, first, settings, WindowTypeQuality)
	window.setRequestSizing(6, 1)
	window.setIpv6Shortfall(1)
	if _, err := window.discoverDestinations(nil, false, "quality"); err != nil {
		t.Fatal(err)
	}
	if got := filters(first.recordedCalls()); !slices.Equal(got, []IpFamilyFilter{IpFamilyFilterV4Capable, IpFamilyFilterV6Capable}) {
		t.Errorf("first contact with an older server got %v, want the main fill then the v6-capable request", got)
	}

	// no shortfall: one request either way
	none := newCapabilityTestGenerator(false, answer)
	none.mode.observe(false)
	window = capabilityTestWindow(ctx, none, settings, WindowTypeQuality)
	window.setRequestSizing(6, 1)
	if _, err := window.discoverDestinations(nil, false, "quality"); err != nil {
		t.Fatal(err)
	}
	if got := filters(none.recordedCalls()); !slices.Equal(got, []IpFamilyFilter{IpFamilyFilterV4Capable}) {
		t.Errorf("no shortfall got %v, want the main fill alone", got)
	}
}

// A family-swap probe asks the v6-capable request alone, in selector mode
// with the window's `select`.
func TestProbeAsksTheV6CapableRequestAlone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()
	generator := newCapabilityTestGenerator(true, nil)
	generator.mode.observe(true)
	window := capabilityTestWindow(ctx, generator, settings, WindowTypeQuality)
	window.setRequestSizing(6, 1)
	window.setIpv6Shortfall(1)
	if _, err := window.discoverDestinations(nil, true, "quality"); err != nil {
		t.Fatal(err)
	}
	calls := generator.recordedCalls()
	if len(calls) != 1 || calls[0].ipFamily != IpFamilyFilterV6Capable || calls[0].count != 1 ||
		calls[0].providerSelect.Ip != ProviderIpCapabilityDualstack {
		t.Errorf("probe calls %+v, want one v6-capable request for the shortfall", calls)
	}
}

// The limited window asks once, v4-capable, with ipv4 and tcp selectors and
// the profile's rank mode, and keeps only the providers labelled tcp: classes
// C and D.
func TestLimitedWindowAsksForTcpProviders(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()
	answer := capabilityTestClassDestinations()
	generator := newCapabilityTestGenerator(true, func(call capabilityTestCall) map[MultiHopId]DestinationStats {
		return answer
	})
	window := capabilityTestWindow(ctx, generator, settings, WindowTypeLimited)
	window.setRequestSizing(3, 0)

	destinations, err := window.discoverDestinations(nil, false, window.requestRankMode())
	if err != nil {
		t.Fatal(err)
	}
	calls := generator.recordedCalls()
	if len(calls) != 1 {
		t.Fatalf("calls %d, want one", len(calls))
	}
	call := calls[0]
	if call.ipFamily != IpFamilyFilterV4Capable || call.rankMode != "quality" ||
		call.providerSelect.Ip != ProviderIpCapabilityIpv4 || call.providerSelect.Protocol != ProviderProtocolCapabilityTcp ||
		call.providerSelect.TargetCount != 3 || call.providerSelect.DualstackTargetCount != 0 {
		t.Errorf("limited request %+v, want v4-capable quality with ipv4, tcp, target 3 and no ipv6 need", call)
	}
	if len(destinations) != 2 {
		t.Fatalf("kept %d destinations, want the two tcp ones", len(destinations))
	}
	for _, destination := range destinations {
		if destination.stats.Capabilities.Protocol != ProviderProtocolCapabilityTcp {
			t.Errorf("the limited window kept a %q provider", destination.stats.Capabilities.Protocol)
		}
	}

	// a probe never runs for the limited window
	if probed, _ := window.discoverDestinations(nil, true, "quality"); len(probed) != 0 || len(generator.recordedCalls()) != 1 {
		t.Error("a probe asked for the limited window")
	}
}

// From a server that predates selector mode the limited window fills
// nothing: its answer carries no labels, every provider reads dualproto, and
// a tcp window never takes one (the old-server cell of FP2-PROTOCOL.md §5.4).
// The rolling-deploy cell -- a new server before its first export answers
// only classes A and B -- fills nothing either.
func TestLimitedWindowFillsNothingWithoutTcpProviders(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()

	unlabelled := map[MultiHopId]DestinationStats{}
	for _, family := range []IpFamily{IpFamilyDualstack, IpFamilyV4Only} {
		unlabelled[RequireMultiHopId(NewId())] = DestinationStats{IpFamily: family, Capabilities: providerCapabilitiesFromIpFamily(family)}
	}
	older := newCapabilityTestGenerator(false, func(call capabilityTestCall) map[MultiHopId]DestinationStats {
		return unlabelled
	})
	window := capabilityTestWindow(ctx, older, settings, WindowTypeLimited)
	window.setRequestSizing(3, 0)
	if destinations, _ := window.discoverDestinations(nil, false, "quality"); len(destinations) != 0 {
		t.Errorf("an older server's answer filled %d limited clients", len(destinations))
	}

	rolling := newCapabilityTestGenerator(true, func(call capabilityTestCall) map[MultiHopId]DestinationStats {
		return map[MultiHopId]DestinationStats{
			RequireMultiHopId(NewId()): {IpFamily: IpFamilyDualstack, Capabilities: capabilityClassA},
			RequireMultiHopId(NewId()): {IpFamily: IpFamilyV4Only, Capabilities: capabilityClassB},
		}
	})
	window = capabilityTestWindow(ctx, rolling, settings, WindowTypeLimited)
	window.setRequestSizing(3, 0)
	if destinations, _ := window.discoverDestinations(nil, false, "quality"); len(destinations) != 0 {
		t.Errorf("a server before its first export filled %d limited clients", len(destinations))
	}

	// a generator without labels is never asked for the limited window
	family := newFamilyTestGenerator(map[IpFamily]int{IpFamilyV4Only: 2})
	window = capabilityTestWindow(ctx, family, settings, WindowTypeLimited)
	if destinations, _ := window.discoverDestinations(nil, false, "quality"); len(destinations) != 0 || len(family.recordedCalls()) != 0 {
		t.Error("a generator without labels was asked for the limited window")
	}
}

// A sticky redial and a recent exit are asked for by id in selector mode, and
// a fresh selector-mode answer relabels them; the limited window drops a
// provider the answer relabels dualproto.
func TestNamedLookupsTakeTheFreshLabels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()
	destination := RequireMultiHopId(NewId())
	fresh := capabilityClassA
	generator := newCapabilityTestGenerator(true, func(call capabilityTestCall) map[MultiHopId]DestinationStats {
		return map[MultiHopId]DestinationStats{
			destination: {IpFamily: fresh.IpFamily(), Capabilities: fresh},
		}
	})
	remembered := DestinationStats{Tier: 3, IpFamily: IpFamilyV4Only, Capabilities: capabilityClassD}

	// the user's Fixed IP redials its lost exit with the fresh labels
	sticky := capabilityTestWindow(ctx, generator, settings, WindowTypeQuality)
	sticky.performanceProfile = &PerformanceProfile{
		WindowType: WindowTypeQuality,
		WindowSize: WindowSizeSettings{WindowSizeMin: 1, WindowSizeMax: 1, FixedWindowSize: 1},
	}
	sticky.setRequestSizing(1, 1)
	sticky.stickyRedial.Remember(destination, remembered)
	redial, ok, err := sticky.enumerateStickyRedial(nil, "quality")
	if err != nil || !ok {
		t.Fatalf("redial %v %v", ok, err)
	}
	if redial.stats.Capabilities != capabilityClassA || redial.stats.IpFamily != IpFamilyDualstack || redial.stats.Tier != 3 {
		t.Errorf("redial stats %+v, want the fresh labels on the remembered stats", redial.stats)
	}
	calls := generator.recordedCalls()
	if len(calls) != 1 || calls[0].clientId == nil || *calls[0].clientId != destination.Tail() || calls[0].providerSelect.Protocol == "" {
		t.Errorf("redial calls %+v, want one by-id request in selector mode", calls)
	}

	// a limited window's recent exit relabelled dualproto is not its to hold
	limited := capabilityTestWindow(ctx, generator, settings, WindowTypeLimited)
	limited.setRequestSizing(3, 0)
	exit := recentExit{destination: destination, stats: remembered, lostTime: time.Now(), reason: "transport"}
	answers := limited.lookupRecentExits(generator, []recentExit{exit}, nil, "quality")()
	if len(answers) != 1 || answers[0].found {
		t.Errorf("a relabelled dualproto provider was found for the limited window: %+v", answers)
	}
	fresh = capabilityClassC
	answers = limited.lookupRecentExits(generator, []recentExit{exit}, nil, "quality")()
	if len(answers) != 1 || !answers[0].found || answers[0].stats.Capabilities != capabilityClassC {
		t.Errorf("a tcp provider's recent exit: %+v, want found with the fresh labels", answers)
	}
}

// --- profile changes ---

// A profile change regenerates the limited window with the normal windows:
// its clients are cancelled, its requests carry the new profile's rank mode,
// its target is recomputed from the normal windows' passes under the new
// profile, never from their sizing under the old one, and an equal profile
// changes nothing. A post-quantum toggle is a profile change (§12.5).
func TestProfileChangeRegeneratesTheLimitedWindow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()
	generator := newCapabilityTestGenerator(true, nil)
	windows := map[WindowType]*multiClientWindow{
		WindowTypeQuality: capabilityTestWindow(ctx, generator, settings, WindowTypeQuality),
		WindowTypeSpeed:   capabilityTestWindow(ctx, generator, settings, WindowTypeSpeed),
		WindowTypeLimited: capabilityTestWindow(ctx, generator, settings, WindowTypeLimited),
	}
	parent := capabilityTestParent(ctx, cancel, generator, settings, windows)
	capabilityTestWireLimitedWindow(parent)

	fill := func() map[WindowType]*multiClientChannel {
		clients := map[WindowType]*multiClientChannel{}
		for windowType, window := range windows {
			capabilities := capabilityClassA
			if windowType == WindowTypeLimited {
				capabilities = capabilityClassD
			}
			client := capabilityTestChannel(t, ctx, settings, windowType, capabilities)
			client.ctx, client.cancel = context.WithCancel(ctx)
			window.stateLock.Lock()
			clear(window.clients)
			window.clients[client.ClientId()] = client
			window.stateLock.Unlock()
			clients[windowType] = client
		}
		return clients
	}
	fixedSpeed := &PerformanceProfile{WindowType: WindowTypeSpeed, WindowSize: WindowSizeSettings{WindowSizeMin: 4, WindowSizeMax: 4}}
	fixedQuality := &PerformanceProfile{WindowType: WindowTypeQuality, WindowSize: WindowSizeSettings{WindowSizeMin: 4, WindowSizeMax: 4}}
	steps := []struct {
		name     string
		profile  *PerformanceProfile
		rankMode string
	}{
		{name: "auto to fixed speed", profile: fixedSpeed, rankMode: "speed"},
		{name: "fixed speed to auto", profile: nil, rankMode: "quality"},
		{name: "auto to fixed quality", profile: fixedQuality, rankMode: "quality"},
		{name: "fixed quality to fixed speed", profile: fixedSpeed, rankMode: "speed"},
		{name: "post-quantum on", profile: &PerformanceProfile{WindowType: WindowTypeSpeed, WindowSize: fixedSpeed.WindowSize, PostQuantumEncryption: true}, rankMode: "speed"},
	}
	// the normal windows' sizing as their passes under the profile in force
	// published it, which sizes the limited window to 3
	publishOld := func() {
		windows[WindowTypeQuality].publishSizing(windowSizing{published: true, active: true, targetSize: 6, hardMax: 6})
		windows[WindowTypeSpeed].publishSizing(windowSizing{published: true, active: true, targetSize: 2, hardMax: 4})
	}
	for _, step := range steps {
		clients := fill()
		publishOld()
		if size := windows[WindowTypeLimited].limitedWindowSize(); size.WindowSizeMax != 3 {
			t.Fatalf("%s: limited target %d before the change, want 3", step.name, size.WindowSizeMax)
		}
		if err := parent.SetPerformanceProfile(step.profile); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		for windowType, client := range clients {
			if !client.IsDone() {
				t.Errorf("%s: the %s window's client was not cancelled", step.name, windowStatsWindowOf(windowType))
			}
		}
		if size := windows[WindowTypeLimited].limitedWindowSize(); size.WindowSizeMax != 0 {
			t.Errorf("%s: limited target %d from the old profile's sizing, want none until the normal windows size again", step.name, size.WindowSizeMax)
		}
		// the passes under the new profile size it again
		windows[WindowTypeQuality].publishSizing(windowSizing{published: true, active: true, targetSize: 4, hardMax: 0})
		windows[WindowTypeSpeed].publishSizing(windowSizing{published: true, active: false, targetSize: 8, hardMax: 8})
		if size := windows[WindowTypeLimited].limitedWindowSize(); size.WindowSizeMax != 2 || size.WindowSizeHardMax != 0 {
			t.Errorf("%s: recomputed limited target %d hard max %d, want 2 and unbounded", step.name, size.WindowSizeMax, size.WindowSizeHardMax)
		}
		if got := windows[WindowTypeLimited].requestRankMode(); got != step.rankMode {
			t.Errorf("%s: limited rank mode %q, want %q", step.name, got, step.rankMode)
		}
		windows[WindowTypeLimited].stateLock.Lock()
		profile := windows[WindowTypeLimited].performanceProfile
		windows[WindowTypeLimited].stateLock.Unlock()
		if !performanceProfilesEqual(profile, step.profile) {
			t.Errorf("%s: the limited window kept profile %+v", step.name, profile)
		}
	}

	// an equal profile is a no-op: nothing is cancelled, and the sizing stays
	clients := fill()
	publishOld()
	equal := *steps[len(steps)-1].profile
	if err := parent.SetPerformanceProfile(&equal); err != nil {
		t.Fatal(err)
	}
	for windowType, client := range clients {
		if client.IsDone() {
			t.Errorf("an equal profile cancelled the %s window's client", windowStatsWindowOf(windowType))
		}
	}
	if size := windows[WindowTypeLimited].limitedWindowSize(); size.WindowSizeMax != 3 {
		t.Errorf("an equal profile changed the limited target to %d, want 3", size.WindowSizeMax)
	}
}

// --- the monitor ---

// The apps' status reads the normal windows: the limited window adds its
// address-family and udp availability, never its size, readiness, failure or
// providers. Its availability changes reach a listener.
func TestMergedMonitorTakesOnlyTheLimitedWindowsAvailability(t *testing.T) {
	quality := NewRemoteUserNatMultiClientMonitorWithDefaults()
	limited := NewRemoteUserNatMultiClientMonitorWithDefaults()
	merged := newMergedMultiClientMonitorWithAvailability(
		[]MultiClientMonitor{quality},
		[]MultiClientMonitor{limited},
	)

	limited.addWindowExpandEvent(true, 3, true, false, false)
	limitedClientId := NewId()
	limited.AddProviderEvent(limitedClientId, ProviderStateAdded, NewId(), nil, IpFamilyDualstack)
	event := merged.WindowExpandEvent()
	if event.TargetSize != 0 || event.MinSatisfied || event.Failed {
		t.Errorf("merged %+v took the limited window's size or readiness", event)
	}
	if !event.Ipv6Available {
		t.Error("a limited dualstack exit is not merged into Ipv6Available")
	}
	if len(merged.ProviderEvents()) != 0 {
		t.Errorf("merged provider events %d, want none of the limited window's", len(merged.ProviderEvents()))
	}

	quality.AddWindowExpandEventWithAvailability(true, 6, false, true)
	event = merged.WindowExpandEvent()
	if event.TargetSize != 6 || !event.MinSatisfied || !event.UdpAvailable || !event.Ipv6Available {
		t.Errorf("merged %+v, want quality's size, readiness and udp with the limited window's ipv6", event)
	}

	// a listener hears the limited window's availability change
	events := make(chan *WindowExpandEvent, 16)
	unsub := merged.AddMonitorEventCallback(func(windowExpandEvent *WindowExpandEvent, providerEvents map[Id]*ProviderEvent, reset bool) {
		if _, ok := providerEvents[limitedClientId]; ok {
			t.Error("a limited window provider event reached the listener")
		}
		events <- windowExpandEvent
	})
	defer unsub()
	quality.AddWindowExpandEventWithAvailability(true, 6, false, false)
	limited.addWindowExpandEvent(true, 3, false, false, false)
	deadline := time.After(10 * time.Second)
	for {
		select {
		case event := <-events:
			if !event.Ipv6Available && !event.UdpAvailable {
				return
			}
		case <-deadline:
			t.Fatal("the limited window's availability change never reached the listener")
		}
	}
}

// The legacy publisher reads udp unavailable; the exported publisher with
// availability carries it.
func TestWindowExpandEventCarriesUdpAvailable(t *testing.T) {
	monitor := NewRemoteUserNatMultiClientMonitorWithDefaults()
	monitor.AddWindowExpandEvent(true, 1, true)
	if event := monitor.WindowExpandEvent(); event.UdpAvailable || !event.Ipv6Available {
		t.Errorf("legacy publisher %+v, want udp unavailable and ipv6 available", event)
	}
	monitor.AddWindowExpandEventWithAvailability(true, 1, false, true)
	if event := monitor.WindowExpandEvent(); !event.UdpAvailable || event.Ipv6Available {
		t.Errorf("availability publisher %+v, want udp available and ipv6 unavailable", event)
	}
}

// --- process counters ---

// The counters of §8.2: window clients by label (moved per pass, retracted
// at the end), flows by the window of their first exit, flows no window could
// carry, and the limited window's own label.
func TestWindowClientStatsCountsCapabilities(t *testing.T) {
	stats := NewWindowClientStats()
	labels := windowClientLabelCounts{}
	ip, protocol := windowStatsLabelsOf(capabilityClassD)
	labels[ip][protocol] = 2
	stats.moveClientLabels(WindowTypeLimited, windowClientLabelCounts{}, labels)
	stats.addFlow(WindowTypeLimited, capabilityTestPath(4, IpProtocolTcp, 40000))
	stats.addFlow(WindowTypeQuality, capabilityTestPath(6, IpProtocolUdp, 40001))
	stats.addNoCompatible(capabilityTestPath(4, IpProtocolUdp, 40002))

	clients := map[string]int64{}
	stats.VisitWindowClients(func(window string, ip string, protocol string, count int64) {
		clients[window+"/"+ip+"/"+protocol] = count
	})
	if clients["limited/ipv4/tcp"] != 2 {
		t.Errorf("limited ipv4/tcp clients %d, want 2", clients["limited/ipv4/tcp"])
	}
	flows := map[string]uint64{}
	stats.VisitWindowFlows(func(window string, ipVersion string, protocol string, count uint64) {
		flows[window+"/"+ipVersion+"/"+protocol] = count
	})
	if flows["limited/4/tcp"] != 1 || flows["quality/6/udp"] != 1 {
		t.Errorf("flows %v, want one limited v4 tcp and one quality v6 udp", flows)
	}
	noCompatible := map[string]uint64{}
	stats.VisitNoCompatible(func(ipVersion string, protocol string, count uint64) {
		noCompatible[ipVersion+"/"+protocol] = count
	})
	if noCompatible["4/udp"] != 1 {
		t.Errorf("no-compatible %v, want one v4 udp", noCompatible)
	}

	// the window's end retracts its clients
	stats.moveClientLabels(WindowTypeLimited, labels, windowClientLabelCounts{})
	stats.VisitWindowClients(func(window string, ip string, protocol string, count int64) {
		if count != 0 {
			t.Errorf("%s/%s/%s still counts %d after the window ended", window, ip, protocol, count)
		}
	})

	windows := []string{}
	stats.VisitExpandPasses(func(window string, state string, count uint64) {
		if !slices.Contains(windows, window) {
			windows = append(windows, window)
		}
	})
	if !slices.Contains(windows, "limited") {
		t.Errorf("window labels %v, want limited among them", windows)
	}
}

// A flow counts once, under the window of its first exit, however often it
// is re-raced.
func TestBindClientFlowCountsAFlowOnce(t *testing.T) {
	parent := bindFlowTestParent()
	parent.settings.WindowClientStats = NewWindowClientStats()
	limited := bindFlowTestChannel(parent)
	limited.args = &multiClientChannelArgs{windowType: WindowTypeLimited}
	quality := bindFlowTestChannel(parent)
	quality.args = &multiClientChannelArgs{windowType: WindowTypeQuality}
	update := newMultiClientChannelUpdate(context.Background(), capabilityTestPath(4, IpProtocolTcp, 40100))
	defer update.Close()

	update.selectAffinityClient(limited)
	parent.bindClientFlow(update, limited)
	update.selectAffinityClient(quality)
	parent.bindClientFlow(update, quality)

	flows := map[string]uint64{}
	parent.settings.WindowClientStats.VisitWindowFlows(func(window string, ipVersion string, protocol string, count uint64) {
		if 0 < count {
			flows[window+"/"+ipVersion+"/"+protocol] = count
		}
	})
	if len(flows) != 1 || flows["limited/4/tcp"] != 1 {
		t.Errorf("flows %v, want the one flow under its first exit's window", flows)
	}
}
