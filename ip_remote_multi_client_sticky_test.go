package connect

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"
)

// Phase 2 of the user's Fixed IP (ip_remote_multi_client_sticky.go): an exit
// lost to transport loss is asked for again by name before discovery, so a
// provider that is still online comes back with the same egress ip. Exits
// lost to a verdict, and every window that is not sticky, discover as before.

// stickyRedialTestGenerator answers plain discovery with a fixed set, and a
// named request with the named provider while it is online.
type stickyRedialTestGenerator struct {
	testingEmptyMultiClientGenerator

	stateLock      sync.Mutex
	discovered     map[MultiHopId]DestinationStats
	online         map[Id]bool
	namedErr       error
	namedRequests  []Id
	discoveryCount int
}

func (self *stickyRedialTestGenerator) NextDestinations(count int, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.discoveryCount += 1
	return maps.Clone(self.discovered), nil
}

func (self *stickyRedialTestGenerator) NextDestinationsForClientId(clientId Id, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.namedRequests = append(self.namedRequests, clientId)
	if self.namedErr != nil {
		return nil, self.namedErr
	}
	destinations := map[MultiHopId]DestinationStats{}
	if self.online[clientId] {
		// a named answer carries no discovery stats
		destinations[RequireMultiHopId(clientId)] = DestinationStats{}
	}
	return destinations, nil
}

func (self *stickyRedialTestGenerator) requests() (namedRequests []Id, discoveryCount int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.namedRequests), self.discoveryCount
}

// stickyRedialTestWindow is a bare window holding a profile, for one
// discovery round at a time.
func stickyRedialTestWindow(generator MultiClientGenerator, performanceProfile *PerformanceProfile) *multiClientWindow {
	return &multiClientWindow{
		ctx:                context.Background(),
		log:                NewNoopLogger(),
		generator:          generator,
		windowType:         WindowTypeSpeed,
		settings:           DefaultMultiClientSettings(),
		performanceProfile: performanceProfile,
		resizeMonitor:      NewMonitor(),
	}
}

// stickyRedialTestStats are the discovery stats a lost exit was dialed with.
func stickyRedialTestStats() DestinationStats {
	return DestinationStats{
		Tier:     1,
		Location: &ProviderLocation{CountryCode: "de"},
		IpFamily: IpFamilyDualstack,
	}
}

func TestStickyRedialHoldsOneExitUntilTaken(t *testing.T) {
	redial := &stickyRedial{}
	if _, _, ok := redial.Take(); ok {
		t.Fatal("an empty re-dial handed out an exit")
	}

	first := RequireMultiHopId(NewId())
	second := RequireMultiHopId(NewId())
	redial.Remember(first, DestinationStats{})
	redial.Remember(second, stickyRedialTestStats())
	destination, stats, ok := redial.Take()
	AssertEqual(t, ok, true)
	AssertEqual(t, destination, second)
	AssertEqual(t, stats.Location.CountryCode, "de")
	if _, _, ok := redial.Take(); ok {
		t.Fatal("a taken exit was handed out twice")
	}

	redial.Remember(first, DestinationStats{})
	redial.Forget()
	if _, _, ok := redial.Take(); ok {
		t.Fatal("a forgotten exit was handed out")
	}
}

// Only transport loss in a sticky window is remembered: a verdict is
// evidence against the provider, and a window that is not the user's Fixed
// IP has no egress ip to keep.
func TestStickyRedialRemembersOnlyStickyTransportLoss(t *testing.T) {
	newClient := func() *multiClientChannel {
		client := stickyTestPastLifetimeChannel()
		client.args.DestinationStats = stickyRedialTestStats()
		return client
	}
	window := stickyRedialTestWindow(&stickyRedialTestGenerator{}, fixedIpTestProfile(WindowTypeSpeed))

	client := newClient()
	AssertEqual(t, window.rememberStickyRedial(client, errTransportDownTimeout, true), true)
	destination, stats, ok := window.stickyRedial.Take()
	AssertEqual(t, ok, true)
	AssertEqual(t, destination, client.Destination())
	AssertEqual(t, stats.IpFamily, IpFamilyDualstack)

	for _, err := range []error{
		errors.New("Blackhole no-receive-ack (send 4/400B recv 0/0B syn 1/0 nackAge 0s synAge 0s dsts=2)"),
		errors.New("send stalled: no ack progress"),
		errContractReliability,
		errors.New("Done."),
	} {
		AssertEqual(t, window.rememberStickyRedial(newClient(), err, true), false)
	}
	AssertEqual(t, window.rememberStickyRedial(newClient(), errTransportDownTimeout, false), false)
	if _, _, ok := window.stickyRedial.Take(); ok {
		t.Fatal("a verdict or a non-sticky window was remembered for a re-dial")
	}
}

// A provider that is still admitted comes back alone, as the destination the
// window held and with the stats it was discovered with: the named answer
// carries no location or family, and a legacy family would cut the exit's v6.
func TestStickyRedialEnumeratesTheLostExitAlone(t *testing.T) {
	lost := RequireMultiHopId(NewId())
	other := RequireMultiHopId(NewId())
	generator := &stickyRedialTestGenerator{
		discovered: map[MultiHopId]DestinationStats{other: {}},
		online:     map[Id]bool{lost.Tail(): true},
	}
	window := stickyRedialTestWindow(generator, fixedIpTestProfile(WindowTypeSpeed))
	window.stickyRedial.Remember(lost, stickyRedialTestStats())

	ordered, err := window.enumerateDestinations(nil, false)
	AssertEqual(t, err, nil)
	AssertEqual(t, len(ordered), 1)
	AssertEqual(t, ordered[0].destination, lost)
	AssertEqual(t, ordered[0].stickyRedial, true)
	AssertEqual(t, ordered[0].stats.IpFamily, IpFamilyDualstack)
	AssertEqual(t, ordered[0].stats.Location.CountryCode, "de")
	namedRequests, discoveryCount := generator.requests()
	AssertEqual(t, namedRequests, []Id{lost.Tail()})
	AssertEqual(t, discoveryCount, 0)

	// taken: the next round discovers as usual
	ordered, err = window.enumerateDestinations(nil, false)
	AssertEqual(t, err, nil)
	AssertEqual(t, len(ordered), 1)
	AssertEqual(t, ordered[0].destination, other)
	AssertEqual(t, ordered[0].stickyRedial, false)
	namedRequests, discoveryCount = generator.requests()
	AssertEqual(t, len(namedRequests), 1)
	AssertEqual(t, discoveryCount, 1)
}

// A provider the platform no longer returns costs one named request, and the
// same round falls back to discovery.
func TestStickyRedialFallsBackWhenTheProviderIsGone(t *testing.T) {
	lost := RequireMultiHopId(NewId())
	other := RequireMultiHopId(NewId())
	generator := &stickyRedialTestGenerator{
		discovered: map[MultiHopId]DestinationStats{other: {}},
	}
	window := stickyRedialTestWindow(generator, fixedIpTestProfile(WindowTypeSpeed))
	window.stickyRedial.Remember(lost, stickyRedialTestStats())

	ordered, err := window.enumerateDestinations(nil, false)
	AssertEqual(t, err, nil)
	AssertEqual(t, len(ordered), 1)
	AssertEqual(t, ordered[0].destination, other)
	AssertEqual(t, ordered[0].stickyRedial, false)
	namedRequests, discoveryCount := generator.requests()
	AssertEqual(t, namedRequests, []Id{lost.Tail()})
	AssertEqual(t, discoveryCount, 1)
	if _, _, ok := window.stickyRedial.Take(); ok {
		t.Fatal("a provider the platform did not return stayed pending")
	}
}

// A platform that cannot be reached cannot discover either: the round fails
// for the enumerator's retry, and the lost exit stays pending for it.
func TestStickyRedialKeptWhileThePlatformIsUnreachable(t *testing.T) {
	lost := RequireMultiHopId(NewId())
	generator := &stickyRedialTestGenerator{
		namedErr: errors.New("platform unreachable"),
	}
	window := stickyRedialTestWindow(generator, fixedIpTestProfile(WindowTypeSpeed))
	window.stickyRedial.Remember(lost, stickyRedialTestStats())

	if _, err := window.enumerateDestinations(nil, false); err == nil {
		t.Fatal("an unreachable platform was not reported to the enumerator")
	}
	_, discoveryCount := generator.requests()
	AssertEqual(t, discoveryCount, 0)
	destination, _, ok := window.stickyRedial.Take()
	AssertEqual(t, ok, true)
	AssertEqual(t, destination, lost)
}

// A window that is no longer sticky does not dial the old exit, and a
// profile change or a shuffle forgets it.
func TestStickyRedialIgnoredOutsideAStickyWindow(t *testing.T) {
	lost := RequireMultiHopId(NewId())
	other := RequireMultiHopId(NewId())
	generator := &stickyRedialTestGenerator{
		discovered: map[MultiHopId]DestinationStats{other: {}},
		online:     map[Id]bool{lost.Tail(): true},
	}
	window := stickyRedialTestWindow(generator, unfixedTestProfile(WindowTypeSpeed))
	window.stickyRedial.Remember(lost, stickyRedialTestStats())

	ordered, err := window.enumerateDestinations(nil, false)
	AssertEqual(t, err, nil)
	AssertEqual(t, len(ordered), 1)
	AssertEqual(t, ordered[0].destination, other)
	namedRequests, _ := generator.requests()
	AssertEqual(t, len(namedRequests), 0)

	sticky := stickyRedialTestWindow(generator, fixedIpTestProfile(WindowTypeSpeed))
	sticky.stickyRedial.Remember(lost, stickyRedialTestStats())
	sticky.SetPerformanceProfile(fixedIpTestProfile(WindowTypeQuality))
	if _, _, ok := sticky.stickyRedial.Take(); ok {
		t.Fatal("a profile change kept the old exit pending")
	}
	sticky.stickyRedial.Remember(lost, stickyRedialTestStats())
	sticky.shuffle()
	if _, _, ok := sticky.stickyRedial.Take(); ok {
		t.Fatal("a shuffle kept the old exit pending")
	}
}

// stickyTestLostChannel is an exit whose window stats now fail with err, as
// the channel's own detector leaves it when it ends.
func stickyTestLostChannel(err error) *multiClientChannel {
	client := stickyTestPastLifetimeChannel()
	// a fresh exit, so only the loss can remove it
	client.firstEventTime = time.Now()
	client.addError(err)
	return client
}

// waitStickyTestDiscoveries waits until plain discovery has run `count`
// more times, which in a live window means its enumerator has started that
// many more rounds after the loss.
func waitStickyTestDiscoveries(t *testing.T, generator *stickyRedialTestGenerator, count int) {
	t.Helper()
	_, start := generator.requests()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, discoveryCount := generator.requests(); start+count <= discoveryCount {
			return
		}
		if deadline.Before(time.Now()) {
			t.Fatalf("the window did not discover %d more times", count)
		}
		time.Sleep(time.Millisecond)
	}
}

// End to end through the live window: a Fixed IP exit lost to transport loss
// is removed, and the window's next discovery asks for that provider by
// name. Before phase 2 the window went straight to discovery and came back
// on whatever provider ranked first, with a new egress ip.
func TestStickyExitTransportLossRedialsTheSameProvider(t *testing.T) {
	generator := &stickyRedialTestGenerator{
		discovered: map[MultiHopId]DestinationStats{},
		online:     map[Id]bool{},
	}
	window := stickyTestWindow(t, generator, WindowTypeSpeed, fixedIpTestProfile(WindowTypeSpeed))
	client := stickyTestLostChannel(errTransportDownTimeout)
	installStickyTestChannel(window, client)

	deadline := time.Now().Add(30 * time.Second)
	for {
		if namedRequests, _ := generator.requests(); slices.Contains(namedRequests, client.Destination().Tail()) {
			break
		}
		if deadline.Before(time.Now()) {
			t.Fatal("the window did not ask for its lost provider by name")
		}
		time.Sleep(time.Millisecond)
	}
	if slices.Contains(window.unorderedClients(), client) {
		t.Fatal("the lost exit was not removed")
	}
}

// A verdict against the provider, or a window that is not sticky, discovers
// as before: the lost provider is never asked for by name.
func TestStickyExitVerdictAndUnstickyLossDiscoverAsBefore(t *testing.T) {
	cases := []struct {
		name               string
		windowType         WindowType
		performanceProfile *PerformanceProfile
		err                error
	}{
		{
			"Fixed IP exit lost to a blackhole verdict",
			WindowTypeSpeed,
			fixedIpTestProfile(WindowTypeSpeed),
			errors.New("Blackhole no-receive-ack (send 4/400B recv 0/0B syn 1/0 nackAge 0s synAge 0s dsts=2)"),
		},
		{
			"speed exit without Fixed IP lost to transport loss",
			WindowTypeSpeed,
			unfixedTestProfile(WindowTypeSpeed),
			errTransportDownTimeout,
		},
		{
			"auto speed exit lost to transport loss",
			WindowTypeSpeed,
			nil,
			errTransportDownTimeout,
		},
	}
	for _, c := range cases {
		generator := &stickyRedialTestGenerator{
			discovered: map[MultiHopId]DestinationStats{},
			online:     map[Id]bool{},
		}
		window := stickyTestWindow(t, generator, c.windowType, c.performanceProfile)
		client := stickyTestLostChannel(c.err)
		installStickyTestChannel(window, client)

		deadline := time.Now().Add(30 * time.Second)
		for slices.Contains(window.unorderedClients(), client) {
			if deadline.Before(time.Now()) {
				t.Fatalf("%s: the lost exit was not removed", c.name)
			}
			time.Sleep(time.Millisecond)
		}
		waitStickyTestDiscoveries(t, generator, 2)
		if namedRequests, _ := generator.requests(); 0 < len(namedRequests) {
			t.Fatalf("%s: the window asked for the lost provider by name", c.name)
		}
	}
}

// A re-dial candidate is evaluated alone. The pool would otherwise take a
// second candidate for the same slot, and admit whichever answered first.
func TestStickyRedialCandidateIsEvaluatedAlone(t *testing.T) {
	fixture := newMultiClientExpandLifecycleFixture(t)
	// a pool of two candidates for the one slot
	fixture.window.settings.EvaluationPoolMultiple = 2
	args := <-fixture.window.clientChannelArgs
	args.stickyRedial = true
	fixture.window.clientChannelArgs <- args

	expandDone := fixture.start()
	fixture.wait(t, "held initial-ping result", fixture.pingResultEntered)

	// a discovery candidate arrives while the re-dial is evaluating
	secondArgs, err := fixture.window.generator.NewClientArgs()
	if err != nil {
		t.Fatal(err)
	}
	fixture.window.clientChannelArgs <- &multiClientChannelArgs{
		MultiClientGeneratorClientArgs: *secondArgs,
		Destination:                    args.Destination,
	}

	fixture.releasePing()
	if got := fixture.result(t, expandDone); got != 1 {
		t.Fatalf("re-dial admissions=%d, want 1", got)
	}
	select {
	case unused := <-fixture.window.clientChannelArgs:
		fixture.window.generator.RemoveClientArgs(&unused.MultiClientGeneratorClientArgs)
	default:
		t.Fatal("the pass took a second candidate while the re-dial was evaluating")
	}
}

// The API generator asks find-providers2 for the one named provider, not for
// its own specs, with its exclusions and through the same discovery path as
// a window round; an id it excludes is answered empty without a request.
func TestApiGeneratorNextDestinationsForClientId(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "API unavailable", http.StatusServiceUnavailable)
	}))
	defer api.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	strategySettings := DefaultClientStrategySettings()
	strategySettings.EnableResilient = false
	strategySettings.RequestTimeout = time.Second
	strategy := NewClientStrategy(ctx, strategySettings)
	defer strategy.Close()

	provider := NewId()
	excluded := NewId()
	requests := []*FindProviders2Args{}
	discovery := localDiscoveryTestFunc(func(callCtx context.Context, token string, args *FindProviders2Args) (*FindProviders2Result, error) {
		requests = append(requests, args)
		return &FindProviders2Result{Providers: []*FindProvidersProvider{{ClientId: provider}}}, nil
	})
	settings := DefaultApiMultiClientGeneratorSettings()
	settings.ProviderDiscovery = discovery
	generator := NewApiMultiClientGenerator(ctx, []*ProviderSpec{{BestAvailable: true}}, strategy, []Id{excluded}, api.URL, "jwt", api.URL, "test", "test", "test", nil, DefaultClientSettings, settings)
	t.Cleanup(func() { _ = generator.CloseAndWait(context.Background()) })

	held := RequireMultiHopId(NewId())
	destinations, err := generator.NextDestinationsForClientId(provider, []MultiHopId{held}, "speed")
	AssertEqual(t, err, nil)
	AssertEqual(t, len(destinations), 1)
	_, ok := destinations[RequireMultiHopId(provider)]
	AssertEqual(t, ok, true)
	AssertEqual(t, len(requests), 1)
	request := requests[0]
	AssertEqual(t, len(request.Specs), 1)
	AssertEqual(t, *request.Specs[0].ClientId, provider)
	AssertEqual(t, request.Specs[0].BestAvailable, false)
	AssertEqual(t, request.Count, 1)
	AssertEqual(t, request.RankMode, "speed")
	AssertEqual(t, request.ExcludeClientIds, []Id{excluded})
	AssertEqual(t, request.ExcludeDestinations, [][]Id{held.Ids()})

	destinations, err = generator.NextDestinationsForClientId(excluded, nil, "speed")
	AssertEqual(t, err, nil)
	AssertEqual(t, len(destinations), 0)
	AssertEqual(t, len(requests), 1)
}
