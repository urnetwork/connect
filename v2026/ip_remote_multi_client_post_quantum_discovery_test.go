package connect

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Under PostQuantumEncryption every window client runs EncryptionModeRequired,
// so a provider that cannot seal a session can never carry the window's
// traffic. Discovery has to ask for modern providers (require_modern_provider)
// on every path a window discovers by, and only while the profile requires it
// (support inbox 7918, 7843,
// https://github.com/urnetwork/connect/issues/220). It no longer asks for
// declaring providers (post_quantum_encryption). Destinations the generator
// does not discover, fixed ClientId specs and restored window identities, are
// confirmed by a named request with the same requirement before they are
// returned (owner, 2026-11-13: "They must have a positive quality probe
// handshake to count"). The requests are read as the JSON the platform
// receives.

// postQuantumDiscoveryFixture is a multi client over a real API generator
// whose find-providers2 requests go to a local discovery that records them
// and answers no providers, so the windows keep discovering and never dial.
type postQuantumDiscoveryFixture struct {
	generator *ApiMultiClientGenerator
	multi     *RemoteUserNatMultiClient
	// excluded by this test's own requests and by no window's
	marker MultiHopId

	stateLock sync.Mutex
	// each request as JSON, split by whether this test made it
	windowRequests []string
	testRequests   []string
	windowRequest  chan struct{}
}

// An API generator over specs whose find-providers2 requests all go to
// discovery, closed with the test.
func newPostQuantumDiscoveryGenerator(t *testing.T, ctx context.Context, specs []*ProviderSpec, discovery NetworkProviderDiscovery) *ApiMultiClientGenerator {
	t.Helper()
	strategySettings := DefaultClientStrategySettings()
	strategySettings.EnableResilient = false
	strategySettings.RequestTimeout = time.Second
	strategy := NewClientStrategy(ctx, strategySettings)
	t.Cleanup(strategy.Close)

	generatorSettings := DefaultApiMultiClientGeneratorSettings()
	generatorSettings.ProviderDiscovery = discovery
	generator := NewApiMultiClientGenerator(
		ctx,
		specs,
		strategy,
		nil,
		"https://api.example",
		"jwt",
		"wss://connect.example",
		"test",
		"test",
		"test",
		nil,
		DefaultClientSettings,
		generatorSettings,
	)
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		_ = generator.CloseAndWait(closeCtx)
	})
	return generator
}

func newPostQuantumDiscoveryFixture(t *testing.T, ctx context.Context, performanceProfile *PerformanceProfile) *postQuantumDiscoveryFixture {
	t.Helper()
	fixture := &postQuantumDiscoveryFixture{
		marker:        RequireMultiHopId(NewId()),
		windowRequest: make(chan struct{}, 1),
	}
	discovery := localDiscoveryTestFunc(func(callCtx context.Context, token string, args *FindProviders2Args) (*FindProviders2Result, error) {
		body, err := json.Marshal(args)
		if err != nil {
			return nil, err
		}
		marked := slices.ContainsFunc(args.ExcludeDestinations, func(ids []Id) bool {
			return slices.Equal(ids, fixture.marker.Ids())
		})
		func() {
			fixture.stateLock.Lock()
			defer fixture.stateLock.Unlock()
			if marked {
				fixture.testRequests = append(fixture.testRequests, string(body))
			} else {
				fixture.windowRequests = append(fixture.windowRequests, string(body))
			}
		}()
		if !marked {
			select {
			case fixture.windowRequest <- struct{}{}:
			default:
			}
		}
		return &FindProviders2Result{Providers: []*FindProvidersProvider{}}, nil
	})
	// the discovery above answers every request; nothing is dialed
	fixture.generator = newPostQuantumDiscoveryGenerator(t, ctx, []*ProviderSpec{{BestAvailable: true}}, discovery)

	multiSettings := DefaultMultiClientSettings()
	multiSettings.Log = NewNoopLogger()
	multiSettings.DefaultPerformanceProfile = performanceProfile
	fixture.multi = NewRemoteUserNatMultiClient(ctx, fixture.generator, nil, protocol.ProvideMode_Public, multiSettings)
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		if err := fixture.multi.CloseAndWait(closeCtx); err != nil {
			t.Error(err)
		}
	})
	return fixture
}

// Waits for a window's own discovery request, then returns every window
// request recorded so far.
func (self *postQuantumDiscoveryFixture) windowDiscovered(t *testing.T, ctx context.Context) []string {
	t.Helper()
	select {
	case <-self.windowRequest:
	case <-ctx.Done():
		t.Fatal("no window discovered")
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.windowRequests)
}

// Discovers by every generator path a window uses, each request marked as
// this test's, and returns the requests the paths made.
func (self *postQuantumDiscoveryFixture) discoverEveryPath(t *testing.T, ctx context.Context) []string {
	t.Helper()
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.testRequests = nil
	}()
	excluded := []MultiHopId{self.marker}
	if _, err := self.generator.NextDestinations(3, excluded, "quality"); err != nil {
		t.Fatal(err)
	}
	if _, err := self.generator.NextDestinationsContext(ctx, 3, excluded, "quality"); err != nil {
		t.Fatal(err)
	}
	if _, err := self.generator.NextDestinationsWithIpFamily(3, excluded, "speed", IpFamilyFilterV6Capable); err != nil {
		t.Fatal(err)
	}
	if _, err := self.generator.NextDestinationsWithIpFamilyContext(ctx, 1, excluded, "quality", IpFamilyFilterV4Capable); err != nil {
		t.Fatal(err)
	}
	if _, err := self.generator.NextDestinationsForClientId(NewId(), excluded, "quality"); err != nil {
		t.Fatal(err)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(self.testRequests) != 5 {
		t.Fatalf("discovered by %d requests, want 5", len(self.testRequests))
	}
	return slices.Clone(self.testRequests)
}

// The value a request asks for a boolean field, and whether it names the
// field at all.
func testingDiscoveryRequestFlag(t *testing.T, body string, field string) (requested bool, present bool) {
	t.Helper()
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		t.Fatal(err)
	}
	value, present := fields[field]
	if !present {
		return false, false
	}
	if err := json.Unmarshal(value, &requested); err != nil {
		t.Fatal(err)
	}
	return requested, true
}

// Fails unless the request sends require_modern_provider true and leaves
// post_quantum_encryption off the wire.
func assertModernDiscoveryRequest(t *testing.T, body string, description string) {
	t.Helper()
	if requested, _ := testingDiscoveryRequestFlag(t, body, "require_modern_provider"); !requested {
		t.Fatalf("%s did not ask for modern providers: %s", description, body)
	}
	if _, present := testingDiscoveryRequestFlag(t, body, "post_quantum_encryption"); present {
		t.Fatalf("%s still asked for declaring providers: %s", description, body)
	}
}

// Fails unless the request names neither capability field, as every request
// before them did not.
func assertCapabilityBlindDiscoveryRequest(t *testing.T, body string, description string) {
	t.Helper()
	for _, field := range []string{"require_modern_provider", "post_quantum_encryption"} {
		if _, present := testingDiscoveryRequestFlag(t, body, field); present {
			t.Fatalf("%s sent %s: %s", description, field, body)
		}
	}
}

// A multi client whose profile requires post-quantum encryption asks for
// modern providers: the windows' own discovery, and every path a window
// discovers by, family top-ups and the named redial of a lost exit included.
// None of them asks for declaring providers any more. Turning the requirement
// off returns discovery to the request of an older client, and turning it on
// again asks again. Before this change the profile asked for declaring
// providers instead, which takes a Public provider's declaration in place of
// the capability probe's evidence that it negotiates the session.
func TestPostQuantumProfileAsksDiscoveryForModernProviders(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	postQuantum := &PerformanceProfile{WindowType: WindowTypeAuto, PostQuantumEncryption: true}
	fixture := newPostQuantumDiscoveryFixture(t, ctx, postQuantum)

	// every window request so far was made under the post-quantum profile
	for _, body := range fixture.windowDiscovered(t, ctx) {
		assertModernDiscoveryRequest(t, body, "a post-quantum window's discovery")
	}
	for _, body := range fixture.discoverEveryPath(t, ctx) {
		assertModernDiscoveryRequest(t, body, "a post-quantum discovery path")
	}

	AssertEqual(t, fixture.multi.SetPerformanceProfile(&PerformanceProfile{WindowType: WindowTypeAuto}), nil)
	for _, body := range fixture.discoverEveryPath(t, ctx) {
		assertCapabilityBlindDiscoveryRequest(t, body, "discovery after the profile stopped requiring post-quantum encryption")
	}

	AssertEqual(t, fixture.multi.SetPerformanceProfile(postQuantum), nil)
	for _, body := range fixture.discoverEveryPath(t, ctx) {
		assertModernDiscoveryRequest(t, body, "discovery after the profile required post-quantum encryption again")
	}
}

// A multi client without the requirement, the default and every client before
// this change, keeps the request exactly as it was: neither
// require_modern_provider nor post_quantum_encryption, so discovery stays
// capability-blind.
func TestDefaultProfileKeepsCapabilityBlindDiscovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	fixture := newPostQuantumDiscoveryFixture(t, ctx, nil)
	requests := append(fixture.windowDiscovered(t, ctx), fixture.discoverEveryPath(t, ctx)...)
	for _, body := range requests {
		assertCapabilityBlindDiscoveryRequest(t, body, "a client without the requirement")
	}
}

// A generator whose find-providers2 requests are recorded and answered by
// `answer`, whose derived identities come from a counting credential
// authority, whose store restores `persisted` (no store for nil), and whose
// constructor excludes `excludeClientIds`. Nothing reaches a public endpoint.
// A call confirms beside its discovery, so `answer` can run on two goroutines
// at once.
type modernConfirmFixture struct {
	generator *ApiMultiClientGenerator
	mintCount atomic.Int32

	stateLock sync.Mutex
	requests  []*FindProviders2Args
}

func newModernConfirmFixture(
	t *testing.T,
	ctx context.Context,
	specs []*ProviderSpec,
	persisted []*WindowClientIdentity,
	answer func(args *FindProviders2Args) (*FindProviders2Result, error),
	excludeClientIds ...Id,
) *modernConfirmFixture {
	t.Helper()
	fixture := &modernConfirmFixture{}
	strategySettings := DefaultClientStrategySettings()
	strategySettings.EnableResilient = false
	strategySettings.RequestTimeout = time.Second
	strategy := NewClientStrategy(ctx, strategySettings)
	t.Cleanup(strategy.Close)

	generatorSettings := DefaultApiMultiClientGeneratorSettings()
	generatorSettings.ProviderDiscovery = localDiscoveryTestFunc(func(callCtx context.Context, token string, args *FindProviders2Args) (*FindProviders2Result, error) {
		func() {
			fixture.stateLock.Lock()
			defer fixture.stateLock.Unlock()
			fixture.requests = append(fixture.requests, args)
		}()
		return answer(args)
	})
	generatorSettings.ClientCredentials = &generatorCredentialFixture{
		auth: func(context.Context, *AuthNetworkClientArgs) (*AuthNetworkClientResult, error) {
			fixture.mintCount.Add(1)
			return &AuthNetworkClientResult{ByClientJwt: generatorCredentialTestJwt(t, NewId())}, nil
		},
		remove: func(context.Context, *RemoveNetworkClientArgs) (*RemoveNetworkClientResult, error) {
			return &RemoveNetworkClientResult{}, nil
		},
	}
	fixture.generator = NewApiMultiClientGenerator(
		ctx,
		specs,
		strategy,
		excludeClientIds,
		"https://api.modern.example",
		"synthetic-parent-token",
		"wss://platform.modern.example",
		"synthetic modern",
		"synthetic",
		"test",
		nil,
		DefaultClientSettings,
		generatorSettings,
	)
	if persisted != nil {
		fixture.generator.SetIdentityStore(&fakeIdentityStore{persisted: persisted})
	}
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		_ = fixture.generator.CloseAndWait(closeCtx)
	})
	return fixture
}

// Takes the requests recorded since the last take, split into confirmations
// of fixed and restored destinations and every other request.
func (self *modernConfirmFixture) takeRequests() (confirms []*FindProviders2Args, others []*FindProviders2Args) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, request := range self.requests {
		if testingModernConfirmRequest(request) {
			confirms = append(confirms, request)
		} else {
			others = append(others, request)
		}
	}
	self.requests = nil
	return confirms, others
}

// Whether a request names its providers by client id only and requires them
// modern, as a confirmation does.
func testingModernConfirmRequest(args *FindProviders2Args) bool {
	if !args.RequireModernProvider || len(args.Specs) == 0 {
		return false
	}
	for _, spec := range args.Specs {
		if spec.ClientId == nil {
			return false
		}
	}
	return true
}

// The client ids a request names.
func testingNamedClientIds(args *FindProviders2Args) map[Id]bool {
	clientIds := map[Id]bool{}
	for _, spec := range args.Specs {
		if spec.ClientId != nil {
			clientIds[*spec.ClientId] = true
		}
	}
	return clientIds
}

// The platform's answer to a named request: the named providers that are
// modern, and nothing else, as the server's named path answers.
func testingNamedModernAnswer(args *FindProviders2Args, modernClientIds ...Id) *FindProviders2Result {
	providers := []*FindProvidersProvider{}
	for clientId := range testingNamedClientIds(args) {
		if slices.Contains(modernClientIds, clientId) {
			providers = append(providers, &FindProvidersProvider{ClientId: clientId})
		}
	}
	return &FindProviders2Result{Providers: providers}
}

// The destinations of an answer, without their stats.
func testingDestinationSet(destinations map[MultiHopId]DestinationStats) map[MultiHopId]bool {
	destinationSet := map[MultiHopId]bool{}
	for destination := range destinations {
		destinationSet[destination] = true
	}
	return destinationSet
}

// Fails unless a confirmation names each of `clientIds` exactly once, in
// legacy mode with no family filter and no exclusion, at the window's rank
// mode, and requires them modern.
func assertModernConfirmRequest(t *testing.T, args *FindProviders2Args, rankMode string, clientIds ...Id) {
	t.Helper()
	wantClientIds := map[Id]bool{}
	for _, clientId := range clientIds {
		wantClientIds[clientId] = true
	}
	if !maps.Equal(testingNamedClientIds(args), wantClientIds) || len(args.Specs) != len(wantClientIds) {
		t.Fatalf("confirmation named %v, want %v", testingNamedClientIds(args), wantClientIds)
	}
	if !args.RequireModernProvider || args.PostQuantumEncryption {
		t.Fatalf("confirmation did not require modern providers alone: %+v", args)
	}
	if args.Select != nil || args.IpFamily != IpFamilyFilterDefault || args.RankMode != rankMode {
		t.Fatalf("confirmation carried a selector, a family filter or another rank mode: %+v", args)
	}
	if len(args.ExcludeClientIds) != 0 || len(args.ExcludeDestinations) != 0 || args.ForceMinimum || args.Strict {
		t.Fatalf("confirmation carried exclusions or discovery options: %+v", args)
	}
}

// The root cause of post-quantum windows failing to connect to about half
// their providers: a restored window identity re-formed the window against its
// provider from before with no check, though that provider never passed the
// modern requirement and could not seal the session. Under the requirement the
// platform must confirm it; one the platform does not answer is never
// returned by any discovery path and is forgotten, so no window can dial it,
// whether by its restored identity or a fresh one.
func TestPostQuantumNeverReturnsRestoredDestinationThePlatformDoesNotConfirm(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	restoredIdentity := &WindowClientIdentity{
		ClientId:    NewId(),
		ByJwt:       "synthetic-restored-token",
		InstanceId:  NewId(),
		Destination: RequireMultiHopId(NewId()),
	}
	discovered := RequireMultiHopId(NewId())
	fixture := newModernConfirmFixture(t, ctx, []*ProviderSpec{{BestAvailable: true}}, []*WindowClientIdentity{restoredIdentity}, func(args *FindProviders2Args) (*FindProviders2Result, error) {
		if testingModernConfirmRequest(args) {
			// the restored provider never had a positive probe handshake
			return testingNamedModernAnswer(args), nil
		}
		return &FindProviders2Result{Providers: []*FindProvidersProvider{{ClientId: discovered.Tail()}}}, nil
	})
	// as the multi client sets it for a profile with PostQuantumEncryption
	fixture.generator.SetPostQuantumEncryption(true)

	wantDestinations := map[MultiHopId]bool{discovered: true}
	destinations, err := fixture.generator.NextDestinationsContext(ctx, 3, nil, "quality")
	AssertEqual(t, err, nil)
	AssertEqual(t, testingDestinationSet(destinations), wantDestinations)
	confirms, others := fixture.takeRequests()
	AssertEqual(t, len(confirms), 1)
	assertModernConfirmRequest(t, confirms[0], "quality", restoredIdentity.Destination.Tail())
	AssertEqual(t, len(others), 1)
	AssertEqual(t, others[0].Specs, []*ProviderSpec{{BestAvailable: true}})
	AssertEqual(t, others[0].RequireModernProvider, true)

	// forgotten, so no window expansion of this process meets it again
	AssertEqual(t, len(fixture.generator.identityState.RestoredDestinations()), 0)

	// no other discovery path returns it either, and none asks about it again
	providerSelect := FindProvidersSelect{
		Ip:          ProviderIpCapabilityDualstack,
		Protocol:    ProviderProtocolCapabilityDualproto,
		TargetCount: 6,
	}
	for _, call := range []func() (map[MultiHopId]DestinationStats, error){
		func() (map[MultiHopId]DestinationStats, error) {
			return fixture.generator.NextDestinations(3, nil, "quality")
		},
		func() (map[MultiHopId]DestinationStats, error) {
			return fixture.generator.NextDestinationsWithIpFamily(1, nil, "speed", IpFamilyFilterV6Capable)
		},
		func() (map[MultiHopId]DestinationStats, error) {
			return fixture.generator.NextDestinationsWithIpFamilyContext(ctx, 3, nil, "quality", IpFamilyFilterV4Capable)
		},
		func() (map[MultiHopId]DestinationStats, error) {
			destinations, _, err := fixture.generator.NextDestinationsWithCapabilities(3, nil, "quality", IpFamilyFilterV4Capable, providerSelect)
			return destinations, err
		},
		func() (map[MultiHopId]DestinationStats, error) {
			destinations, _, err := fixture.generator.NextDestinationsWithCapabilitiesContext(ctx, 3, nil, "speed", IpFamilyFilterV4Capable, providerSelect)
			return destinations, err
		},
	} {
		destinations, err := call()
		AssertEqual(t, err, nil)
		AssertEqual(t, testingDestinationSet(destinations), wantDestinations)
	}
	confirms, others = fixture.takeRequests()
	AssertEqual(t, len(confirms), 0)
	AssertEqual(t, len(others), 5)

	// a dial of its destination could not reuse the restored identity: the
	// generator mints a fresh one instead
	args, err := fixture.generator.NewClientArgsForDestinationContext(ctx, restoredIdentity.Destination)
	AssertEqual(t, err, nil)
	AssertNotEqual(t, args.ClientId, restoredIdentity.ClientId)
	AssertEqual(t, fixture.mintCount.Load(), int32(1))
	fixture.generator.RemoveClientArgs(args)
}

// A restored identity the platform confirms modern is returned, and the
// window's dial of it reuses the restored identity itself, so its provider's
// flows resume as without the requirement.
func TestPostQuantumKeepsAndReusesConfirmedRestoredIdentity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	restoredIdentity := &WindowClientIdentity{
		ClientId:    NewId(),
		ByJwt:       "synthetic-restored-token",
		InstanceId:  NewId(),
		Destination: RequireMultiHopId(NewId()),
	}
	fixture := newModernConfirmFixture(t, ctx, []*ProviderSpec{{BestAvailable: true}}, []*WindowClientIdentity{restoredIdentity}, func(args *FindProviders2Args) (*FindProviders2Result, error) {
		if testingModernConfirmRequest(args) {
			return testingNamedModernAnswer(args, restoredIdentity.Destination.Tail()), nil
		}
		// a platform with no other modern provider to offer
		return &FindProviders2Result{Providers: []*FindProvidersProvider{}}, nil
	})
	fixture.generator.SetPostQuantumEncryption(true)

	destinations, err := fixture.generator.NextDestinationsContext(ctx, 3, nil, "speed")
	AssertEqual(t, err, nil)
	AssertEqual(t, destinations, map[MultiHopId]DestinationStats{restoredIdentity.Destination: {}})
	confirms, others := fixture.takeRequests()
	AssertEqual(t, len(confirms), 1)
	assertModernConfirmRequest(t, confirms[0], "speed", restoredIdentity.Destination.Tail())
	AssertEqual(t, len(others), 1)
	// still pending reuse
	AssertEqual(t, fixture.generator.identityState.RestoredDestinations(), []MultiHopId{restoredIdentity.Destination})

	args, err := fixture.generator.NewClientArgsForDestinationContext(ctx, restoredIdentity.Destination)
	AssertEqual(t, err, nil)
	AssertEqual(t, args.ClientId, restoredIdentity.ClientId)
	AssertEqual(t, args.ClientAuth.ByJwt, restoredIdentity.ByJwt)
	AssertEqual(t, args.ClientAuth.InstanceId, restoredIdentity.InstanceId)
	AssertEqual(t, fixture.mintCount.Load(), int32(0))
	fixture.generator.RemoveClientArgs(args)
}

// A fixed ClientId spec is confirmed the same way: in the one named request,
// never in discovery, on every call, since a fixed spec has no memory. A
// generator whose every spec is fixed, as a pinned tunnel's is, now asks the
// platform before each answer, and returns its destination only once the
// platform confirms it. The confirmation carries no `select`, so it reports
// nothing about selector mode.
func TestPostQuantumConfirmsFixedClientIdDestinations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	legacyClientId := NewId()
	modernClientId := NewId()
	fixture := newModernConfirmFixture(t, ctx, []*ProviderSpec{{ClientId: &legacyClientId}, {ClientId: &modernClientId}, {BestAvailable: true}}, nil, func(args *FindProviders2Args) (*FindProviders2Result, error) {
		if testingModernConfirmRequest(args) {
			return testingNamedModernAnswer(args, modernClientId), nil
		}
		return &FindProviders2Result{Providers: []*FindProvidersProvider{}}, nil
	})
	fixture.generator.SetPostQuantumEncryption(true)

	destinations, err := fixture.generator.NextDestinationsContext(ctx, 3, nil, "quality")
	AssertEqual(t, err, nil)
	AssertEqual(t, destinations, map[MultiHopId]DestinationStats{RequireMultiHopId(modernClientId): {}})
	confirms, others := fixture.takeRequests()
	AssertEqual(t, len(confirms), 1)
	assertModernConfirmRequest(t, confirms[0], "quality", legacyClientId, modernClientId)
	AssertEqual(t, len(others), 1)
	// only the discovery spec rides discovery, never a fixed one
	AssertEqual(t, others[0].Specs, []*ProviderSpec{{BestAvailable: true}})

	var pinnedModern atomic.Bool
	pinnedClientId := NewId()
	pinned := newModernConfirmFixture(t, ctx, []*ProviderSpec{{ClientId: &pinnedClientId}}, nil, func(args *FindProviders2Args) (*FindProviders2Result, error) {
		if pinnedModern.Load() {
			return testingNamedModernAnswer(args, pinnedClientId), nil
		}
		return testingNamedModernAnswer(args), nil
	})
	pinned.generator.SetPostQuantumEncryption(true)
	providerSelect := FindProvidersSelect{
		Ip:          ProviderIpCapabilityIpv4,
		Protocol:    ProviderProtocolCapabilityTcp,
		TargetCount: 1,
	}
	for range 2 {
		destinations, err := pinned.generator.NextDestinationsContext(ctx, 1, nil, "quality")
		AssertEqual(t, err, nil)
		AssertEqual(t, len(destinations), 0)
		destinations, selectorMode, err := pinned.generator.NextDestinationsWithCapabilities(1, nil, "quality", IpFamilyFilterV4Capable, providerSelect)
		AssertEqual(t, err, nil)
		AssertEqual(t, selectorMode, false)
		AssertEqual(t, len(destinations), 0)
	}
	confirms, others = pinned.takeRequests()
	AssertEqual(t, len(confirms), 4)
	for _, confirm := range confirms {
		assertModernConfirmRequest(t, confirm, "quality", pinnedClientId)
	}
	AssertEqual(t, len(others), 0)
	if _, known := pinned.generator.SelectorMode(); known {
		t.Fatal("a confirmation was read as an answer about selector mode")
	}

	// once the provider has its positive handshake, the pinned window dials it
	pinnedModern.Store(true)
	destinations, err = pinned.generator.NextDestinationsContext(ctx, 1, nil, "quality")
	AssertEqual(t, err, nil)
	AssertEqual(t, destinations, map[MultiHopId]DestinationStats{RequireMultiHopId(pinnedClientId): {}})
}

// Without the requirement nothing changes: fixed and restored destinations
// are returned as they are, with no request of their own, beside discovery
// that asks capability-blind, and a pinned generator makes no request at all.
// Clearing the requirement after it was set returns to exactly that.
func TestPostQuantumOffReturnsFixedAndRestoredWithoutConfirmation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	fixedClientId := NewId()
	restoredIdentity := &WindowClientIdentity{
		ClientId:    NewId(),
		ByJwt:       "synthetic-restored-token",
		InstanceId:  NewId(),
		Destination: RequireMultiHopId(NewId()),
	}
	fixture := newModernConfirmFixture(t, ctx, []*ProviderSpec{{ClientId: &fixedClientId}, {BestAvailable: true}}, []*WindowClientIdentity{restoredIdentity}, func(args *FindProviders2Args) (*FindProviders2Result, error) {
		// a platform that would confirm nothing
		return &FindProviders2Result{Providers: []*FindProvidersProvider{}}, nil
	})
	wantDestinations := map[MultiHopId]DestinationStats{
		RequireMultiHopId(fixedClientId): {},
		restoredIdentity.Destination:     {},
	}
	assertUnconfirmed := func() {
		t.Helper()
		destinations, err := fixture.generator.NextDestinationsContext(ctx, 3, nil, "quality")
		AssertEqual(t, err, nil)
		AssertEqual(t, destinations, wantDestinations)
		destinations, err = fixture.generator.NextDestinationsWithIpFamily(3, nil, "speed", IpFamilyFilterV6Capable)
		AssertEqual(t, err, nil)
		AssertEqual(t, destinations, wantDestinations)
		confirms, others := fixture.takeRequests()
		AssertEqual(t, len(confirms), 0)
		AssertEqual(t, len(others), 2)
		for _, request := range others {
			AssertEqual(t, request.Specs, []*ProviderSpec{{BestAvailable: true}})
			body, err := json.Marshal(request)
			AssertEqual(t, err, nil)
			assertCapabilityBlindDiscoveryRequest(t, string(body), "discovery without the requirement")
		}
		// still pending reuse
		AssertEqual(t, fixture.generator.identityState.RestoredDestinations(), []MultiHopId{restoredIdentity.Destination})
	}
	assertUnconfirmed()
	fixture.generator.SetPostQuantumEncryption(true)
	fixture.generator.SetPostQuantumEncryption(false)
	assertUnconfirmed()

	pinnedClientId := NewId()
	pinned := newModernConfirmFixture(t, ctx, []*ProviderSpec{{ClientId: &pinnedClientId}}, nil, func(args *FindProviders2Args) (*FindProviders2Result, error) {
		return &FindProviders2Result{Providers: []*FindProvidersProvider{}}, nil
	})
	destinations, err := pinned.generator.NextDestinationsContext(ctx, 1, nil, "quality")
	AssertEqual(t, err, nil)
	AssertEqual(t, destinations, map[MultiHopId]DestinationStats{RequireMultiHopId(pinnedClientId): {}})
	confirms, others := pinned.takeRequests()
	AssertEqual(t, len(confirms)+len(others), 0)
}

// A confirmation that fails confirms nothing: the fixed and restored
// destinations are left out of the answer, never dialed unconfirmed, while
// discovery still answers. The failure says nothing against them, so a
// restored identity stays pending and the next confirmation returns it. A
// pinned generator, with nothing to discover, fails the call. When discovery
// fails beside a confirmation that succeeds, the confirmed destinations are
// still returned, as fixed ones were before the requirement.
func TestPostQuantumConfirmationFailureDropsUnconfirmedDestinations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	fixedClientId := NewId()
	restoredIdentity := &WindowClientIdentity{
		ClientId:    NewId(),
		ByJwt:       "synthetic-restored-token",
		InstanceId:  NewId(),
		Destination: RequireMultiHopId(NewId()),
	}
	discovered := RequireMultiHopId(NewId())
	// the failures the platform answers with, under the lock
	var stateLock sync.Mutex
	confirmErr := errors.New("synthetic confirmation failure")
	var discoveryErr error
	setErrs := func(confirm error, discovery error) {
		stateLock.Lock()
		defer stateLock.Unlock()
		confirmErr = confirm
		discoveryErr = discovery
	}
	fixture := newModernConfirmFixture(t, ctx, []*ProviderSpec{{ClientId: &fixedClientId}, {BestAvailable: true}}, []*WindowClientIdentity{restoredIdentity}, func(args *FindProviders2Args) (*FindProviders2Result, error) {
		stateLock.Lock()
		defer stateLock.Unlock()
		if testingModernConfirmRequest(args) {
			if confirmErr != nil {
				return nil, confirmErr
			}
			return testingNamedModernAnswer(args, fixedClientId, restoredIdentity.Destination.Tail()), nil
		}
		if discoveryErr != nil {
			return nil, discoveryErr
		}
		return &FindProviders2Result{Providers: []*FindProvidersProvider{{ClientId: discovered.Tail()}}}, nil
	})
	fixture.generator.SetPostQuantumEncryption(true)

	destinations, err := fixture.generator.NextDestinationsContext(ctx, 3, nil, "quality")
	AssertEqual(t, err, nil)
	AssertEqual(t, testingDestinationSet(destinations), map[MultiHopId]bool{discovered: true})
	confirms, others := fixture.takeRequests()
	AssertEqual(t, len(confirms), 1)
	assertModernConfirmRequest(t, confirms[0], "quality", fixedClientId, restoredIdentity.Destination.Tail())
	AssertEqual(t, len(others), 1)
	AssertEqual(t, fixture.generator.identityState.RestoredDestinations(), []MultiHopId{restoredIdentity.Destination})

	// both requests fail: the call fails as discovery alone did
	setErrs(errors.New("synthetic confirmation failure"), errors.New("synthetic discovery failure"))
	destinations, err = fixture.generator.NextDestinationsContext(ctx, 3, nil, "quality")
	AssertNotEqual(t, err, nil)
	AssertEqual(t, len(destinations), 0)

	// discovery fails beside a confirmation that succeeds
	setErrs(nil, errors.New("synthetic discovery failure"))
	destinations, err = fixture.generator.NextDestinationsContext(ctx, 3, nil, "quality")
	AssertEqual(t, err, nil)
	AssertEqual(t, destinations, map[MultiHopId]DestinationStats{
		RequireMultiHopId(fixedClientId): {},
		restoredIdentity.Destination:     {},
	})

	// confirmed and discovered together
	setErrs(nil, nil)
	destinations, err = fixture.generator.NextDestinationsContext(ctx, 3, nil, "quality")
	AssertEqual(t, err, nil)
	AssertEqual(t, testingDestinationSet(destinations), map[MultiHopId]bool{
		RequireMultiHopId(fixedClientId): true,
		restoredIdentity.Destination:     true,
		discovered:                       true,
	})

	pinnedClientId := NewId()
	pinned := newModernConfirmFixture(t, ctx, []*ProviderSpec{{ClientId: &pinnedClientId}}, nil, func(args *FindProviders2Args) (*FindProviders2Result, error) {
		return nil, errors.New("synthetic confirmation failure")
	})
	pinned.generator.SetPostQuantumEncryption(true)
	destinations, err = pinned.generator.NextDestinationsContext(ctx, 1, nil, "quality")
	AssertNotEqual(t, err, nil)
	AssertEqual(t, len(destinations), 0)
}

// A restored multi-hop destination counts only when the platform confirms
// every hop modern: a confirmed exit behind an unconfirmed intermediary is
// dropped and forgotten.
func TestPostQuantumConfirmsEveryHopOfRestoredMultiHopDestination(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	intermediaryId := NewId()
	exitId := NewId()
	for _, modernClientIds := range [][]Id{{exitId}, {intermediaryId, exitId}} {
		restoredIdentity := &WindowClientIdentity{
			ClientId:    NewId(),
			ByJwt:       "synthetic-restored-token",
			InstanceId:  NewId(),
			Destination: RequireMultiHopId(intermediaryId, exitId),
		}
		fixture := newModernConfirmFixture(t, ctx, []*ProviderSpec{{BestAvailable: true}}, []*WindowClientIdentity{restoredIdentity}, func(args *FindProviders2Args) (*FindProviders2Result, error) {
			if testingModernConfirmRequest(args) {
				return testingNamedModernAnswer(args, modernClientIds...), nil
			}
			return &FindProviders2Result{Providers: []*FindProvidersProvider{}}, nil
		})
		fixture.generator.SetPostQuantumEncryption(true)

		destinations, err := fixture.generator.NextDestinationsContext(ctx, 3, nil, "quality")
		AssertEqual(t, err, nil)
		confirms, _ := fixture.takeRequests()
		AssertEqual(t, len(confirms), 1)
		assertModernConfirmRequest(t, confirms[0], "quality", intermediaryId, exitId)
		if len(modernClientIds) == 2 {
			AssertEqual(t, destinations, map[MultiHopId]DestinationStats{restoredIdentity.Destination: {}})
			AssertEqual(t, fixture.generator.identityState.RestoredDestinations(), []MultiHopId{restoredIdentity.Destination})
		} else {
			AssertEqual(t, len(destinations), 0)
			AssertEqual(t, len(fixture.generator.identityState.RestoredDestinations()), 0)
		}
	}
}
