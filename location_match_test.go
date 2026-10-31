package connect

// Tests for the find-providers2 location fallback marker as the window carries
// it (location_match.go): discovery normalizes it into DestinationStats, the
// window's provider events carry it, the monitor folds the Added providers into
// WindowExpandEvent.LocationMatch and notifies on every change, and an exact
// answer or an older server never produces a fallback.

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// testLocation is a synthetic provider location with every id set.
func testLocation(region string, country string) *ProviderLocation {
	regionLocationId := NewId()
	countryLocationId := NewId()
	cityLocationId := NewId()
	return &ProviderLocation{
		Country:           country,
		CountryCode:       "zz",
		Region:            region,
		City:              "City " + region,
		CountryLocationId: &countryLocationId,
		RegionLocationId:  &regionLocationId,
		CityLocationId:    &cityLocationId,
	}
}

// locationMatchEvents records every window expand event a monitor listener
// receives, in delivery order.
type locationMatchEvents struct {
	stateLock sync.Mutex
	matches   []WindowLocationMatch
	notify    chan struct{}
}

func newLocationMatchEvents() *locationMatchEvents {
	return &locationMatchEvents{
		notify: make(chan struct{}, 1024),
	}
}

func (self *locationMatchEvents) callback(windowExpandEvent *WindowExpandEvent, _ map[Id]*ProviderEvent, _ bool) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.matches = append(self.matches, windowExpandEvent.LocationMatch)
	}()
	self.notify <- struct{}{}
}

// waits for the listener to deliver want, failing after a second
func (self *locationMatchEvents) waitFor(t *testing.T, want WindowLocationMatch) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		found := func() bool {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			return 0 < len(self.matches) && self.matches[len(self.matches)-1] == want
		}()
		if found {
			return
		}
		select {
		case <-self.notify:
		case <-deadline:
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			t.Fatalf("listener never received %+v; received %+v", want, self.matches)
		}
	}
}

func TestLocationMatchNormalizesUnknownLevelsToExact(t *testing.T) {
	for _, c := range []struct {
		match LocationMatch
		want  LocationMatch
	}{
		{match: "", want: LocationMatchExact},
		{match: "region", want: LocationMatchRegion},
		{match: "country", want: LocationMatchCountry},
		// a level a newer server introduces degrades to no notice
		{match: "continent", want: LocationMatchExact},
		{match: "Region", want: LocationMatchExact},
	} {
		if got := c.match.Normalize(); got != c.want {
			t.Errorf("%q normalized to %q, want %q", c.match, got, c.want)
		}
	}
}

// The generator carries each provider's find-providers2 match into its
// destination stats. A provider without one, as from a server that predates
// the field, and a level this build does not know are exact.
func TestNextDestinationsCarriesTheDiscoveredLocationMatch(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unused", http.StatusServiceUnavailable)
	}))
	defer api.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	strategySettings := DefaultClientStrategySettings()
	strategySettings.EnableResilient = false
	strategySettings.RequestTimeout = time.Second
	strategy := NewClientStrategy(ctx, strategySettings)
	defer strategy.Close()

	regionProvider := NewId()
	countryProvider := NewId()
	exactProvider := NewId()
	unknownProvider := NewId()
	location := testLocation("Region A", "Country A")
	// the wire form: an older server sends no "match" at all
	resultJson, err := json.Marshal(map[string]any{
		"providers": []map[string]any{
			{"client_id": regionProvider, "tier": 1, "match": "region", "location": location},
			{"client_id": countryProvider, "tier": 1, "match": "country"},
			{"client_id": exactProvider, "tier": 1},
			{"client_id": unknownProvider, "tier": 1, "match": "continent"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var strictRequested bool
	discovery := localDiscoveryTestFunc(func(_ context.Context, _ string, args *FindProviders2Args) (*FindProviders2Result, error) {
		strictRequested = strictRequested || args.Strict
		var result FindProviders2Result
		if err := json.Unmarshal(resultJson, &result); err != nil {
			return nil, err
		}
		return &result, nil
	})
	generatorSettings := DefaultApiMultiClientGeneratorSettings()
	generatorSettings.ProviderDiscovery = discovery
	locationId := NewId()
	generator := NewApiMultiClientGenerator(ctx, []*ProviderSpec{{LocationId: &locationId}}, strategy, nil, api.URL, "initial", api.URL, "test", "test", "test", nil, DefaultClientSettings, generatorSettings)
	defer func() { _ = generator.CloseAndWait(context.Background()) }()

	destinations, err := generator.NextDestinationsContext(ctx, 4, nil, "quality")
	if err != nil {
		t.Fatal(err)
	}
	if strictRequested {
		t.Fatal("window discovery asked for a strict answer and would lose the fallback")
	}
	want := map[Id]LocationMatch{
		regionProvider:  LocationMatchRegion,
		countryProvider: LocationMatchCountry,
		exactProvider:   LocationMatchExact,
		unknownProvider: LocationMatchExact,
	}
	if len(destinations) != len(want) {
		t.Fatalf("destinations = %d, want %d", len(destinations), len(want))
	}
	for destination, stats := range destinations {
		wantMatch, ok := want[destination.Tail()]
		if !ok {
			t.Fatalf("unexpected destination %s", destination.Tail())
		}
		if stats.LocationMatch != wantMatch {
			t.Errorf("%s match = %q, want %q", destination.Tail(), stats.LocationMatch, wantMatch)
		}
		if destination.Tail() == regionProvider && (stats.Location == nil || stats.Location.Region != location.Region) {
			t.Errorf("region provider lost its location: %+v", stats.Location)
		}
	}
}

// The window-level match is the broadest level among the Added providers: a
// window routes new flows to any of them, so one provider outside the requested
// location is enough for the notice. Mixed says the requested location also has
// providers in use. Providers that are not Added do not count, and removing the
// last fallback provider returns the window to exact.
func TestMonitorLocationMatchReportsTheBroadestLevelInUse(t *testing.T) {
	monitor := NewRemoteUserNatMultiClientMonitorWithDefaults()
	regionLocation := testLocation("Region A", "Country A")
	countryLocation := testLocation("Region B", "Country A")

	locationMatch := func() WindowLocationMatch {
		windowExpandEvent, _ := monitor.Events()
		return windowExpandEvent.LocationMatch
	}
	add := func(clientId Id, state ProviderState, location *ProviderLocation, match LocationMatch) {
		monitor.AddProviderEventWithLocationMatch(clientId, state, clientId, location, match, IpFamilyLegacy, nil)
	}

	exactClientId := NewId()
	regionClientId := NewId()
	countryClientId := NewId()

	add(exactClientId, ProviderStateAdded, testLocation("Region C", "Country A"), LocationMatchExact)
	AssertEqual(t, locationMatch(), WindowLocationMatch{})

	// a fallback provider still in evaluation carries no traffic yet
	add(regionClientId, ProviderStateInEvaluation, regionLocation, LocationMatchRegion)
	AssertEqual(t, locationMatch(), WindowLocationMatch{})

	add(regionClientId, ProviderStateAdded, regionLocation, LocationMatchRegion)
	AssertEqual(t, locationMatch(), WindowLocationMatch{
		Match:      LocationMatchRegion,
		Mixed:      true,
		Name:       "Region A",
		LocationId: *regionLocation.RegionLocationId,
	})

	add(countryClientId, ProviderStateAdded, countryLocation, LocationMatchCountry)
	AssertEqual(t, locationMatch(), WindowLocationMatch{
		Match:      LocationMatchCountry,
		Mixed:      true,
		Name:       "Country A",
		LocationId: *countryLocation.CountryLocationId,
	})

	add(exactClientId, ProviderStateRemoved, nil, LocationMatchExact)
	AssertEqual(t, locationMatch(), WindowLocationMatch{
		Match:      LocationMatchCountry,
		Name:       "Country A",
		LocationId: *countryLocation.CountryLocationId,
	})

	add(countryClientId, ProviderStateRemoved, countryLocation, LocationMatchCountry)
	AssertEqual(t, locationMatch(), WindowLocationMatch{
		Match:      LocationMatchRegion,
		Name:       "Region A",
		LocationId: *regionLocation.RegionLocationId,
	})

	add(regionClientId, ProviderStateRemoved, regionLocation, LocationMatchRegion)
	AssertEqual(t, locationMatch(), WindowLocationMatch{})
}

// An exact answer and an older server never produce a fallback: providers of
// the requested location, providers added through the forms that carry no
// match (fixed and restored destinations, older callers), and a level this
// build does not know.
func TestMonitorLocationMatchIsExactForExactProvidersAndOlderSenders(t *testing.T) {
	monitor := NewRemoteUserNatMultiClientMonitorWithDefaults()
	location := testLocation("Region A", "Country A")

	firstClientId := NewId()
	monitor.AddProviderEvent(firstClientId, ProviderStateAdded, firstClientId, location, IpFamilyLegacy)
	secondClientId := NewId()
	monitor.AddProviderEventWithExtenderIps(secondClientId, ProviderStateAdded, secondClientId, location, IpFamilyLegacy, nil)
	thirdClientId := NewId()
	monitor.AddProviderEventWithLocationMatch(thirdClientId, ProviderStateAdded, thirdClientId, location, LocationMatchExact, IpFamilyLegacy, nil)
	fourthClientId := NewId()
	monitor.AddProviderEventWithLocationMatch(fourthClientId, ProviderStateAdded, fourthClientId, location, "continent", IpFamilyLegacy, nil)

	windowExpandEvent, providerEvents := monitor.Events()
	AssertEqual(t, windowExpandEvent.LocationMatch, WindowLocationMatch{})
	for clientId, providerEvent := range providerEvents {
		if providerEvent.LocationMatch != LocationMatchExact {
			t.Errorf("%s carries match %q, want exact", clientId, providerEvent.LocationMatch)
		}
	}

	// an expand event from an older sender, which has no field, decodes to
	// exact over gob, the encoding of the sdk's device rpc bridge
	type olderWindowExpandEvent struct {
		TargetSize   int
		MinSatisfied bool
		Reason       string
	}
	var buffer bytes.Buffer
	if err := gob.NewEncoder(&buffer).Encode(&olderWindowExpandEvent{TargetSize: 4, MinSatisfied: true, Reason: WindowStallEvaluating}); err != nil {
		t.Fatal(err)
	}
	var decoded WindowExpandEvent
	if err := gob.NewDecoder(&buffer).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	AssertEqual(t, decoded.TargetSize, 4)
	AssertEqual(t, decoded.LocationMatch, WindowLocationMatch{})
}

// Every change of the window-level match reaches the listeners with the event
// that caused it.
func TestMonitorLocationMatchChangeNotifiesListeners(t *testing.T) {
	monitor := NewRemoteUserNatMultiClientMonitorWithDefaults()
	events := newLocationMatchEvents()
	unsub := monitor.AddMonitorEventCallback(events.callback)
	defer unsub()

	location := testLocation("Region A", "Country A")
	clientId := NewId()
	monitor.AddProviderEventWithLocationMatch(clientId, ProviderStateInEvaluation, clientId, location, LocationMatchRegion, IpFamilyLegacy, nil)
	events.waitFor(t, WindowLocationMatch{})

	monitor.AddProviderEventWithLocationMatch(clientId, ProviderStateAdded, clientId, location, LocationMatchRegion, IpFamilyLegacy, nil)
	region := WindowLocationMatch{
		Match:      LocationMatchRegion,
		Name:       "Region A",
		LocationId: *location.RegionLocationId,
	}
	events.waitFor(t, region)

	// updates that are not provider events keep the match on what they publish
	monitor.AddWindowExpandEvent(true, 2, false)
	monitor.SetStallStatus(WindowStallProvidersUnresponsive, false)
	monitor.SetProviderIpFamily(clientId, IpFamilyDualstack)
	if windowExpandEvent, _ := monitor.Events(); windowExpandEvent.LocationMatch != region {
		t.Fatalf("expand, stall and family updates lost the match: %+v", windowExpandEvent.LocationMatch)
	}
	events.waitFor(t, region)

	monitor.AddProviderEventWithLocationMatch(clientId, ProviderStateRemoved, clientId, location, LocationMatchRegion, IpFamilyLegacy, nil)
	events.waitFor(t, WindowLocationMatch{})
}

// The shown location is named by a provider that names it, then by the longest
// connected one, whatever the map order.
func TestMonitorLocationMatchNamesTheLongestConnectedNamedLocation(t *testing.T) {
	for range 16 {
		monitor := NewRemoteUserNatMultiClientMonitorWithDefaults()
		unnamedClientId := NewId()
		monitor.AddProviderEventWithLocationMatch(unnamedClientId, ProviderStateAdded, unnamedClientId, nil, LocationMatchRegion, IpFamilyLegacy, nil)
		firstLocation := testLocation("Region A", "Country A")
		firstClientId := NewId()
		monitor.AddProviderEventWithLocationMatch(firstClientId, ProviderStateAdded, firstClientId, firstLocation, LocationMatchRegion, IpFamilyLegacy, nil)
		// a strictly later connected-since
		time.Sleep(2 * time.Millisecond)
		secondClientId := NewId()
		monitor.AddProviderEventWithLocationMatch(secondClientId, ProviderStateAdded, secondClientId, testLocation("Region B", "Country A"), LocationMatchRegion, IpFamilyLegacy, nil)

		windowExpandEvent, _ := monitor.Events()
		AssertEqual(t, windowExpandEvent.LocationMatch, WindowLocationMatch{
			Match:      LocationMatchRegion,
			Name:       "Region A",
			LocationId: *firstLocation.RegionLocationId,
		})
	}

	// with no location at all the level still stands; the name is unknown
	monitor := NewRemoteUserNatMultiClientMonitorWithDefaults()
	clientId := NewId()
	monitor.AddProviderEventWithLocationMatch(clientId, ProviderStateAdded, clientId, nil, LocationMatchCountry, IpFamilyLegacy, nil)
	windowExpandEvent, _ := monitor.Events()
	AssertEqual(t, windowExpandEvent.LocationMatch, WindowLocationMatch{Match: LocationMatchCountry})
}

// The merged monitor of a multi client reports the match over the union of its
// windows' providers, and its listeners see the change.
func TestMergedMonitorLocationMatchSpansWindows(t *testing.T) {
	qualityMonitor := NewRemoteUserNatMultiClientMonitorWithDefaults()
	speedMonitor := NewRemoteUserNatMultiClientMonitorWithDefaults()
	merged := NewMergedMultiClientMonitor([]MultiClientMonitor{qualityMonitor, speedMonitor})
	events := newLocationMatchEvents()
	unsub := merged.AddMonitorEventCallback(events.callback)
	defer unsub()

	exactClientId := NewId()
	qualityMonitor.AddProviderEventWithLocationMatch(exactClientId, ProviderStateAdded, exactClientId, testLocation("Region C", "Country A"), LocationMatchExact, IpFamilyLegacy, nil)
	events.waitFor(t, WindowLocationMatch{})

	location := testLocation("Region A", "Country A")
	regionClientId := NewId()
	speedMonitor.AddProviderEventWithLocationMatch(regionClientId, ProviderStateAdded, regionClientId, location, LocationMatchRegion, IpFamilyLegacy, nil)
	mixedRegion := WindowLocationMatch{
		Match:      LocationMatchRegion,
		Mixed:      true,
		Name:       "Region A",
		LocationId: *location.RegionLocationId,
	}
	events.waitFor(t, mixedRegion)
	AssertEqual(t, merged.WindowExpandEvent().LocationMatch, mixedRegion)
	windowExpandEvent, _ := merged.Events()
	AssertEqual(t, windowExpandEvent.LocationMatch, mixedRegion)

	qualityMonitor.AddProviderEventWithLocationMatch(exactClientId, ProviderStateRemoved, exactClientId, nil, LocationMatchExact, IpFamilyLegacy, nil)
	events.waitFor(t, WindowLocationMatch{
		Match:      LocationMatchRegion,
		Name:       "Region A",
		LocationId: *location.RegionLocationId,
	})
}

// Merging the windows' folds names the same location as folding all of their
// providers at once: the longest connected provider at the broadest level,
// whichever window holds it and in whichever order the windows merge.
func TestMergedMonitorLocationMatchNamesAcrossWindows(t *testing.T) {
	olderMonitor := NewRemoteUserNatMultiClientMonitorWithDefaults()
	newerMonitor := NewRemoteUserNatMultiClientMonitorWithDefaults()
	olderLocation := testLocation("Region A", "Country A")
	olderClientId := NewId()
	olderMonitor.AddProviderEventWithLocationMatch(olderClientId, ProviderStateAdded, olderClientId, olderLocation, LocationMatchRegion, IpFamilyLegacy, nil)
	// a strictly later connected-since
	time.Sleep(2 * time.Millisecond)
	newerClientId := NewId()
	newerMonitor.AddProviderEventWithLocationMatch(newerClientId, ProviderStateAdded, newerClientId, testLocation("Region B", "Country A"), LocationMatchRegion, IpFamilyLegacy, nil)
	// a provider of the location itself, in its own window
	exactMonitor := NewRemoteUserNatMultiClientMonitorWithDefaults()
	exactClientId := NewId()
	exactMonitor.AddProviderEventWithLocationMatch(exactClientId, ProviderStateAdded, exactClientId, nil, LocationMatchExact, IpFamilyLegacy, nil)

	want := WindowLocationMatch{
		Match:      LocationMatchRegion,
		Mixed:      true,
		Name:       "Region A",
		LocationId: *olderLocation.RegionLocationId,
	}
	for _, monitors := range [][]MultiClientMonitor{
		{olderMonitor, newerMonitor, exactMonitor},
		{newerMonitor, olderMonitor, exactMonitor},
		{exactMonitor, newerMonitor, olderMonitor},
	} {
		merged := NewMergedMultiClientMonitor(monitors)
		AssertEqual(t, merged.WindowExpandEvent().LocationMatch, want)
	}
}

// locationMatchEventsMonitor is a monitor that keeps no fold, as an embedder's
// or a test's monitor does: only its provider events.
type locationMatchEventsMonitor struct {
	providerEvents map[Id]*ProviderEvent
}

func (self *locationMatchEventsMonitor) AddMonitorEventCallback(MonitorEventFunction) func() {
	return func() {}
}

func (self *locationMatchEventsMonitor) Events() (*WindowExpandEvent, map[Id]*ProviderEvent) {
	return self.WindowExpandEvent(), self.ProviderEvents()
}

func (self *locationMatchEventsMonitor) WindowExpandEvent() *WindowExpandEvent {
	return &WindowExpandEvent{}
}

func (self *locationMatchEventsMonitor) ProviderEvents() map[Id]*ProviderEvent {
	providerEvents := map[Id]*ProviderEvent{}
	for clientId, providerEvent := range self.providerEvents {
		providerEvents[clientId] = providerEvent
	}
	return providerEvents
}

// A window monitor without a fold is folded from its provider events.
func TestMergedMonitorLocationMatchFoldsAMonitorWithoutAFold(t *testing.T) {
	location := testLocation("Region A", "Country A")
	clientId := NewId()
	monitor := &locationMatchEventsMonitor{
		providerEvents: map[Id]*ProviderEvent{
			clientId: {
				EventTime:      time.Now(),
				ClientId:       clientId,
				State:          ProviderStateAdded,
				EgressClientId: clientId,
				Location:       location,
				LocationMatch:  LocationMatchCountry,
			},
		},
	}
	merged := NewMergedMultiClientMonitor([]MultiClientMonitor{monitor, NewRemoteUserNatMultiClientMonitorWithDefaults()})
	AssertEqual(t, merged.WindowExpandEvent().LocationMatch, WindowLocationMatch{
		Match:      LocationMatchCountry,
		Name:       "Country A",
		LocationId: *location.CountryLocationId,
	})
}

// Removing a window client publishes its terminal event with the match it was
// discovered at, like its location and family.
func TestWindowRemovalCarriesTheLocationMatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()
	window := familyTestWindow(ctx, newFamilyTestGenerator(nil), settings)
	client := familyTestChannel(t, ctx, settings, IpFamilyLegacy)
	client.args.Location = testLocation("Region A", "Country A")
	client.args.LocationMatch = LocationMatchRegion
	window.clients[client.ClientId()] = client
	window.monitor.AddProviderEventWithLocationMatch(client.ClientId(), ProviderStateAdded, client.Destination().Tail(), client.args.Location, client.args.LocationMatch, client.IpFamily(), nil)

	var removed *ProviderEvent
	var removedLock sync.Mutex
	unsub := window.monitor.AddMonitorEventCallback(func(_ *WindowExpandEvent, providerEvents map[Id]*ProviderEvent, _ bool) {
		removedLock.Lock()
		defer removedLock.Unlock()
		for _, providerEvent := range providerEvents {
			if providerEvent.State == ProviderStateRemoved {
				removed = providerEvent
			}
		}
	})
	defer unsub()
	window.clientRemoveCallback = func(client *multiClientChannel) {}
	window.removeClients(client)

	waitForTestCondition(t, time.Second, func() bool {
		removedLock.Lock()
		defer removedLock.Unlock()
		return removed != nil
	}, "no removed event was published")
	removedLock.Lock()
	defer removedLock.Unlock()
	if removed.LocationMatch != LocationMatchRegion {
		t.Errorf("removed event match = %q, want region", removed.LocationMatch)
	}
	if windowExpandEvent, _ := window.monitor.Events(); windowExpandEvent.LocationMatch != (WindowLocationMatch{}) {
		t.Errorf("the window kept the match of a removed provider: %+v", windowExpandEvent.LocationMatch)
	}
}

// locationMatchProviderGenerator discovers the live acking providers of a
// flappingProviderGenerator as providers find-providers2 found in a region,
// as a server answers a city with no admitted provider.
type locationMatchProviderGenerator struct {
	*flappingProviderGenerator
	location *ProviderLocation
}

func (self *locationMatchProviderGenerator) NextDestinations(count int, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
	destinations, err := self.flappingProviderGenerator.NextDestinations(count, excludeDestinations, rankMode)
	for destination, stats := range destinations {
		stats.Location = self.location
		stats.LocationMatch = LocationMatchRegion
		destinations[destination] = stats
	}
	return destinations, err
}

// The composed path: destinations discovered at the region level are
// evaluated and admitted by a real multi client, their Added events carry the
// match, and the multi client's merged monitor reports the window-level match
// with the region's name and id.
func TestMultiClientWindowCarriesTheDiscoveredLocationMatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()
	for _, windowType := range []WindowType{WindowTypeQuality, WindowTypeSpeed} {
		settings.WindowSizes[windowType] = WindowSizeSettings{
			WindowSizeMin:     1,
			WindowSizeMax:     1,
			WindowSizeHardMax: 2,
			FixedWindowSize:   1,
		}
	}
	settings.PingWriteTimeout = 200 * time.Millisecond
	settings.PingTimeout = 2 * time.Second
	settings.WindowResizeTimeout = 100 * time.Millisecond
	settings.WindowExpandTimeout = 2 * time.Second
	settings.DiscoveryExpandRetryMin = 100 * time.Millisecond
	settings.DiscoveryExpandRetryMax = 100 * time.Millisecond
	settings.WindowEnumerateErrorTimeout = 25 * time.Millisecond

	location := testLocation("Region A", "Country A")
	generator := &locationMatchProviderGenerator{
		flappingProviderGenerator: newFlappingProviderGenerator(ctx),
		location:                  location,
	}
	defer generator.close()
	multi := NewRemoteUserNatMultiClient(
		ctx,
		generator,
		func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {},
		protocol.ProvideMode_Public,
		settings,
	)
	defer multi.Close()

	var windowExpandEvent *WindowExpandEvent
	var providerEvents map[Id]*ProviderEvent
	waitForTestCondition(t, 10*time.Second, func() bool {
		windowExpandEvent, providerEvents = multi.Monitor().Events()
		for _, providerEvent := range providerEvents {
			if providerEvent.State == ProviderStateAdded {
				return true
			}
		}
		return false
	}, "the window never admitted a provider")

	for clientId, providerEvent := range providerEvents {
		if providerEvent.LocationMatch != LocationMatchRegion {
			t.Errorf("%s (%s) carries match %q, want region", clientId, providerEvent.State, providerEvent.LocationMatch)
		}
		if providerEvent.Location != location {
			t.Errorf("%s lost its location", clientId)
		}
	}
	AssertEqual(t, windowExpandEvent.LocationMatch, WindowLocationMatch{
		Match:      LocationMatchRegion,
		Name:       "Region A",
		LocationId: *location.RegionLocationId,
	})
}
