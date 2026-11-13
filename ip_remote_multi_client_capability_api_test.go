package connect

// Tests for find-providers2's selector mode on the wire (server
// FP2-PROTOCOL.md §5, §7.4, §12.5): what the api generator sends and how it
// reads the answer, and the requests a running multi client's windows make,
// the limited window's included, against a server stub that echoes `select`.

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A request's `select`, decoded from the JSON the platform receives; nil
// when the request has none.
func capabilityTestRequestSelect(t *testing.T, body string) *FindProvidersSelect {
	t.Helper()
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		t.Fatal(err)
	}
	raw, ok := fields["select"]
	if !ok {
		return nil
	}
	members := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &members); err != nil {
		t.Fatal(err)
	}
	if len(members) != 6 {
		t.Errorf("select sends %d members, want all six: %s", len(members), raw)
	}
	providerSelect := &FindProvidersSelect{}
	if err := json.Unmarshal(raw, providerSelect); err != nil {
		t.Fatal(err)
	}
	return providerSelect
}

// A discovery stub that records each request as JSON and answers it from
// answer: a server in selector mode when echo is set (it echoes `select`
// and labels its providers), an older server otherwise.
type capabilityTestDiscovery struct {
	stateLock sync.Mutex
	echo      bool
	providers []*FindProvidersProvider
	requests  []string
	changed   chan struct{}
}

func newCapabilityTestDiscovery(echo bool, providers []*FindProvidersProvider) *capabilityTestDiscovery {
	return &capabilityTestDiscovery{
		echo:      echo,
		providers: providers,
		changed:   make(chan struct{}, 1),
	}
}

func (self *capabilityTestDiscovery) discovery() NetworkProviderDiscovery {
	return localDiscoveryTestFunc(func(callCtx context.Context, token string, args *FindProviders2Args) (*FindProviders2Result, error) {
		body, err := json.Marshal(args)
		if err != nil {
			return nil, err
		}
		self.stateLock.Lock()
		self.requests = append(self.requests, string(body))
		echo := self.echo
		providers := self.providers
		self.stateLock.Unlock()
		select {
		case self.changed <- struct{}{}:
		default:
		}
		result := &FindProviders2Result{Providers: []*FindProvidersProvider{}}
		for _, provider := range providers {
			answered := *provider
			if !echo || args.Select == nil {
				// an older server, or legacy mode: no labels
				answered.Capabilities = nil
				answered.OverflowStep = 0
			}
			result.Providers = append(result.Providers, &answered)
		}
		if echo && args.Select != nil {
			echoed := *args.Select
			result.Select = &echoed
		}
		return result, nil
	})
}

func (self *capabilityTestDiscovery) recordedRequests() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.requests)
}

// Waits until a recorded request satisfies match, and returns it.
func (self *capabilityTestDiscovery) awaitRequest(t *testing.T, ctx context.Context, description string, match func(body string, providerSelect *FindProvidersSelect) bool) string {
	t.Helper()
	seen := 0
	for {
		requests := self.recordedRequests()
		for _, body := range requests[seen:] {
			if match(body, capabilityTestRequestSelect(t, body)) {
				return body
			}
		}
		seen = len(requests)
		select {
		case <-self.changed:
		case <-ctx.Done():
			t.Fatalf("no request for %s", description)
		}
	}
}

// In selector mode the api generator sends the window's `select`, all six
// members, and reads each provider's labels and overflow step from the
// echoed answer; SelectorMode follows it. A legacy call sends no `select`
// and is read as before. Under post-quantum every selector-mode request
// carries require_modern_provider.
func TestApiGeneratorAsksInSelectorMode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tcpProvider := &FindProvidersProvider{
		ClientId:     NewId(),
		Tier:         33,
		IpFamily:     IpFamilyV4Only,
		Capabilities: &capabilityClassD,
		OverflowStep: 1,
	}
	stub := newCapabilityTestDiscovery(true, []*FindProvidersProvider{tcpProvider})
	generator := newPostQuantumDiscoveryGenerator(t, ctx, []*ProviderSpec{{BestAvailable: true}}, stub.discovery())

	if _, known := generator.SelectorMode(); known {
		t.Fatal("selector mode known before any answer")
	}
	providerSelect := FindProvidersSelect{
		Ip:                   ProviderIpCapabilityDualstack,
		Protocol:             ProviderProtocolCapabilityDualproto,
		TargetCount:          6,
		HeldCount:            2,
		DualstackTargetCount: 1,
	}
	destinations, selectorMode, err := generator.NextDestinationsWithCapabilities(4, nil, "quality", IpFamilyFilterV4Capable, providerSelect)
	if err != nil {
		t.Fatal(err)
	}
	if !selectorMode {
		t.Error("an echoed answer did not read as selector mode")
	}
	if mode, known := generator.SelectorMode(); !mode || !known {
		t.Error("SelectorMode does not follow the echoed answer")
	}
	requests := stub.recordedRequests()
	if got := capabilityTestRequestSelect(t, requests[len(requests)-1]); got == nil || *got != providerSelect {
		t.Errorf("sent select %+v, want %+v", got, providerSelect)
	}
	stats, ok := destinations[RequireMultiHopId(tcpProvider.ClientId)]
	if !ok || stats.Capabilities != capabilityClassD || stats.IpFamily != IpFamilyV4Only || stats.OverflowStep != 1 || stats.Tier != 33 {
		t.Errorf("destination %+v, want class D overflow at tier 33", stats)
	}

	// legacy calls send no select and read no labels
	legacy, err := generator.NextDestinationsWithIpFamily(4, nil, "quality", IpFamilyFilterV4Capable)
	if err != nil {
		t.Fatal(err)
	}
	requests = stub.recordedRequests()
	if capabilityTestRequestSelect(t, requests[len(requests)-1]) != nil {
		t.Error("a legacy call sent select")
	}
	if stats := legacy[RequireMultiHopId(tcpProvider.ClientId)]; stats.Capabilities != capabilityClassB {
		t.Errorf("legacy destination labelled %+v, want ip_family with dualproto", stats.Capabilities)
	}

	// a named lookup in selector mode
	named, selectorMode, err := generator.NextDestinationsForClientIdWithCapabilities(tcpProvider.ClientId, nil, "quality", providerSelect)
	if err != nil || !selectorMode {
		t.Fatalf("named lookup: %v %v", selectorMode, err)
	}
	requests = stub.recordedRequests()
	if capabilityTestRequestSelect(t, requests[len(requests)-1]) == nil {
		t.Error("a named lookup in selector mode sent no select")
	}
	if stats := named[RequireMultiHopId(tcpProvider.ClientId)]; stats.Capabilities != capabilityClassD {
		t.Errorf("named destination labelled %+v, want class D", stats.Capabilities)
	}

	// post-quantum narrows every selector-mode request (§12.5)
	generator.SetPostQuantumEncryption(true)
	if _, _, err := generator.NextDestinationsWithCapabilities(4, nil, "quality", IpFamilyFilterV4Capable, providerSelect); err != nil {
		t.Fatal(err)
	}
	if _, _, err := generator.NextDestinationsWithCapabilitiesContext(ctx, 1, nil, "quality", IpFamilyFilterV6Capable, providerSelect); err != nil {
		t.Fatal(err)
	}
	if _, _, err := generator.NextDestinationsForClientIdWithCapabilities(tcpProvider.ClientId, nil, "quality", providerSelect); err != nil {
		t.Fatal(err)
	}
	requests = stub.recordedRequests()
	for _, body := range requests[len(requests)-3:] {
		assertModernDiscoveryRequest(t, body, "a post-quantum selector-mode request")
	}
}

// An answer that does not echo `select` is a server that predates selector
// mode: its providers are labelled from ip_family with dualproto whatever
// they carry, and SelectorMode reads it as known and off (the new-client,
// old-server cell of §5.4).
func TestApiGeneratorReadsAnUnechoedAnswerAsLegacy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	provider := &FindProvidersProvider{
		ClientId:     NewId(),
		IpFamily:     IpFamilyDualstack,
		Capabilities: &capabilityClassC,
	}
	discovery := localDiscoveryTestFunc(func(callCtx context.Context, token string, args *FindProviders2Args) (*FindProviders2Result, error) {
		// labels, but no echo
		return &FindProviders2Result{Providers: []*FindProvidersProvider{provider}}, nil
	})
	generator := newPostQuantumDiscoveryGenerator(t, ctx, []*ProviderSpec{{BestAvailable: true}}, discovery)
	destinations, selectorMode, err := generator.NextDestinationsWithCapabilities(4, nil, "quality", IpFamilyFilterV4Capable, FindProvidersSelect{
		Ip:          ProviderIpCapabilityIpv4,
		Protocol:    ProviderProtocolCapabilityTcp,
		TargetCount: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if selectorMode {
		t.Error("an answer without the echo read as selector mode")
	}
	if mode, known := generator.SelectorMode(); mode || !known {
		t.Errorf("SelectorMode = %t %t, want known and off", mode, known)
	}
	if stats := destinations[RequireMultiHopId(provider.ClientId)]; stats.Capabilities != capabilityClassA || stats.IpFamily != IpFamilyDualstack {
		t.Errorf("unechoed provider labelled %+v %q, want its ip_family with dualproto", stats.Capabilities, stats.IpFamily)
	}
}

// A running multi client's windows ask in selector mode (§7.4): quality with
// dualstack and dualproto, its target 6 and ipv6 minimum 1; speed with its
// target 1 and no ipv6 minimum; and the limited window with ipv4 and tcp,
// target 6 (the same size as quality's 6, D8), no ipv6 need, v4-capable,
// and the profile's rank mode: quality in auto, speed under a fixed Speed
// profile, where its target is the profile's size. Under post-quantum every
// request, the limited window's included, asks for modern providers.
func TestMultiClientWindowsAskInSelectorMode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	stub := newCapabilityTestDiscovery(true, nil)
	generator := newPostQuantumDiscoveryGenerator(t, ctx, []*ProviderSpec{{BestAvailable: true}}, stub.discovery())
	settings := DefaultMultiClientSettings()
	settings.Log = NewNoopLogger()
	// an expansion that finds nothing ends and asks again promptly, so each
	// window's next request follows a profile change without waiting out the
	// production timeouts
	settings.WindowExpandTimeout = 200 * time.Millisecond
	settings.DiscoveryExpandRetryMin = 50 * time.Millisecond
	settings.DiscoveryExpandRetryMax = 100 * time.Millisecond
	settings.WindowResizeTimeout = 100 * time.Millisecond
	multi := NewRemoteUserNatMultiClient(ctx, generator, nil, protocol.ProvideMode_Public, settings)
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		if err := multi.CloseAndWait(closeCtx); err != nil {
			t.Error(err)
		}
	}()

	rankMode := func(body string) string {
		args := &FindProviders2Args{}
		if err := json.Unmarshal([]byte(body), args); err != nil {
			t.Fatal(err)
		}
		return args.RankMode
	}
	stub.awaitRequest(t, ctx, "the quality window", func(body string, providerSelect *FindProvidersSelect) bool {
		return providerSelect != nil && rankMode(body) == "quality" && providerSelect.Protocol == ProviderProtocolCapabilityDualproto &&
			providerSelect.Ip == ProviderIpCapabilityDualstack && providerSelect.TargetCount == 6 && providerSelect.DualstackTargetCount == 1
	})
	stub.awaitRequest(t, ctx, "the speed window", func(body string, providerSelect *FindProvidersSelect) bool {
		return providerSelect != nil && rankMode(body) == "speed" && providerSelect.Protocol == ProviderProtocolCapabilityDualproto &&
			providerSelect.TargetCount == 1 && providerSelect.DualstackTargetCount == 0
	})
	limitedBody := stub.awaitRequest(t, ctx, "the limited window in auto", func(body string, providerSelect *FindProvidersSelect) bool {
		return providerSelect != nil && providerSelect.Protocol == ProviderProtocolCapabilityTcp
	})
	limitedArgs := &FindProviders2Args{}
	if err := json.Unmarshal([]byte(limitedBody), limitedArgs); err != nil {
		t.Fatal(err)
	}
	want := FindProvidersSelect{
		Ip:          ProviderIpCapabilityIpv4,
		Protocol:    ProviderProtocolCapabilityTcp,
		TargetCount: 6,
	}
	if *limitedArgs.Select != want || limitedArgs.RankMode != "quality" || limitedArgs.IpFamily != IpFamilyFilterV4Capable {
		t.Errorf("limited request %s, want ipv4/tcp target 6 in quality, v4-capable", limitedBody)
	}
	for _, body := range stub.recordedRequests() {
		if capabilityTestRequestSelect(t, body) == nil {
			t.Errorf("a window request sent no select: %s", body)
		}
		assertCapabilityBlindDiscoveryRequest(t, body, "a window request without post-quantum")
	}
	if multi.LimitedWindowTargetSize() != 6 {
		t.Errorf("limited target %d, want 6", multi.LimitedWindowTargetSize())
	}

	// a fixed Speed profile of 4: the limited window asks speed for 4
	fixedSpeed := &PerformanceProfile{WindowType: WindowTypeSpeed, WindowSize: WindowSizeSettings{WindowSizeMin: 4, WindowSizeMax: 4}}
	if err := multi.SetPerformanceProfile(fixedSpeed); err != nil {
		t.Fatal(err)
	}
	stub.awaitRequest(t, ctx, "the limited window under a fixed speed profile", func(body string, providerSelect *FindProvidersSelect) bool {
		return providerSelect != nil && providerSelect.Protocol == ProviderProtocolCapabilityTcp &&
			rankMode(body) == "speed" && providerSelect.TargetCount == 4
	})

	// post-quantum: every request asks for modern providers, the limited
	// window's included
	postQuantum := &PerformanceProfile{WindowType: WindowTypeQuality, WindowSize: WindowSizeSettings{WindowSizeMin: 4, WindowSizeMax: 4}, PostQuantumEncryption: true}
	if err := multi.SetPerformanceProfile(postQuantum); err != nil {
		t.Fatal(err)
	}
	stub.awaitRequest(t, ctx, "the limited window under post-quantum", func(body string, providerSelect *FindProvidersSelect) bool {
		requested, _ := testingDiscoveryRequestFlag(t, body, "require_modern_provider")
		return requested && providerSelect != nil && providerSelect.Protocol == ProviderProtocolCapabilityTcp && rankMode(body) == "quality"
	})
	stub.awaitRequest(t, ctx, "the quality window under post-quantum", func(body string, providerSelect *FindProvidersSelect) bool {
		requested, _ := testingDiscoveryRequestFlag(t, body, "require_modern_provider")
		return requested && providerSelect != nil && providerSelect.Protocol == ProviderProtocolCapabilityDualproto
	})
}
