package connect

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Under PostQuantumEncryption every window client runs EncryptionModeRequired,
// so a provider that cannot seal a session can never carry the window's
// traffic. Discovery has to ask for modern providers (require_modern_provider)
// on every path a window discovers by, and only while the profile requires it
// (support inbox 7918, 7843,
// https://github.com/urnetwork/connect/issues/220). It no longer asks for
// declaring providers (post_quantum_encryption). Destinations the generator
// does not discover, fixed ClientId specs and restored window identities, are
// not narrowed. The requests are read as the JSON the platform receives.

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

// Fixed ClientId destinations and restored window identities are not
// discovered, so the modern requirement does not narrow them: the generator
// returns them unchanged even when the one request for its other specs, which
// carries only those specs, answers no provider. The window clients'
// EncryptionModeRequired still decides every send to them.
func TestModernDiscoveryKeepsFixedAndRestoredDestinations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	fixedClientId := NewId()
	fixed := RequireMultiHopId(fixedClientId)
	restored := RequireMultiHopId(NewId())

	// the discovery runs on this goroutine, inside the generator's calls
	requests := []*FindProviders2Args{}
	requestBodies := []string{}
	discovery := localDiscoveryTestFunc(func(callCtx context.Context, token string, args *FindProviders2Args) (*FindProviders2Result, error) {
		body, err := json.Marshal(args)
		if err != nil {
			return nil, err
		}
		requests = append(requests, args)
		requestBodies = append(requestBodies, string(body))
		// the answer of a platform with no modern provider to offer
		return &FindProviders2Result{Providers: []*FindProvidersProvider{}}, nil
	})
	generator := newPostQuantumDiscoveryGenerator(t, ctx, []*ProviderSpec{{ClientId: &fixedClientId}, {BestAvailable: true}}, discovery)
	generator.SetIdentityStore(&fakeIdentityStore{
		persisted: []*WindowClientIdentity{{
			ClientId:    NewId(),
			ByJwt:       "jwt-restored",
			InstanceId:  NewId(),
			Destination: restored,
		}},
	})
	// as the multi client sets it for a profile with PostQuantumEncryption
	generator.SetPostQuantumEncryption(true)

	wantDestinations := map[MultiHopId]DestinationStats{fixed: {}, restored: {}}
	destinations, err := generator.NextDestinationsContext(ctx, 3, nil, "quality")
	AssertEqual(t, err, nil)
	AssertEqual(t, destinations, wantDestinations)
	destinations, err = generator.NextDestinationsWithIpFamily(3, nil, "speed", IpFamilyFilterV6Capable)
	AssertEqual(t, err, nil)
	AssertEqual(t, destinations, wantDestinations)

	AssertEqual(t, len(requests), 2)
	for i, request := range requests {
		// only the discovery spec rides the request, never the fixed one
		AssertEqual(t, request.Specs, []*ProviderSpec{{BestAvailable: true}})
		assertModernDiscoveryRequest(t, requestBodies[i], "discovery beside fixed and restored destinations")
	}
}

// A generator whose every spec names a fixed ClientId, as a pinned tunnel's
// does, has nothing to discover. With the requirement set it still makes no
// request and returns its destination unchanged, so a provider with no
// evidence yet stays reachable.
func TestModernDiscoveryMakesNoRequestForFixedDestinations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	fixedClientId := NewId()
	// the discovery runs on this goroutine, inside the generator's calls
	requestCount := 0
	discovery := localDiscoveryTestFunc(func(callCtx context.Context, token string, args *FindProviders2Args) (*FindProviders2Result, error) {
		requestCount += 1
		return &FindProviders2Result{Providers: []*FindProvidersProvider{}}, nil
	})
	generator := newPostQuantumDiscoveryGenerator(t, ctx, []*ProviderSpec{{ClientId: &fixedClientId}}, discovery)
	generator.SetPostQuantumEncryption(true)

	wantDestinations := map[MultiHopId]DestinationStats{RequireMultiHopId(fixedClientId): {}}
	destinations, err := generator.NextDestinationsContext(ctx, 3, nil, "quality")
	AssertEqual(t, err, nil)
	AssertEqual(t, destinations, wantDestinations)
	destinations, err = generator.NextDestinationsWithIpFamily(3, nil, "speed", IpFamilyFilterV4Capable)
	AssertEqual(t, err, nil)
	AssertEqual(t, destinations, wantDestinations)
	AssertEqual(t, requestCount, 0)
}
