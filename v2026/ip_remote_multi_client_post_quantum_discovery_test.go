package connect

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Under PostQuantumEncryption every window client runs EncryptionModeRequired,
// so a provider that cannot seal a session can never carry the window's
// traffic. Discovery has to ask for providers that advertise post-quantum
// encryption on every path a window discovers by, and only while the profile
// requires it (support inbox 7918, 7843,
// https://github.com/urnetwork/connect/issues/220). The requests are read as
// the JSON the platform receives.

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

func newPostQuantumDiscoveryFixture(t *testing.T, ctx context.Context, performanceProfile *PerformanceProfile) *postQuantumDiscoveryFixture {
	t.Helper()
	strategySettings := DefaultClientStrategySettings()
	strategySettings.EnableResilient = false
	strategySettings.RequestTimeout = time.Second
	strategy := NewClientStrategy(ctx, strategySettings)
	t.Cleanup(strategy.Close)

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
	generatorSettings := DefaultApiMultiClientGeneratorSettings()
	generatorSettings.ProviderDiscovery = discovery
	// the discovery above answers every request; nothing is dialed
	fixture.generator = NewApiMultiClientGenerator(
		ctx,
		[]*ProviderSpec{{BestAvailable: true}},
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
		_ = fixture.generator.CloseAndWait(closeCtx)
	})

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
	if _, err := self.generator.NextDestinationsContext(ctx, 3, excluded, "quality"); err != nil {
		t.Fatal(err)
	}
	if _, err := self.generator.NextDestinationsWithIpFamily(3, excluded, "speed", IpFamilyFilterV6Capable); err != nil {
		t.Fatal(err)
	}
	if _, err := self.generator.NextDestinationsForClientId(NewId(), excluded, "quality"); err != nil {
		t.Fatal(err)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(self.testRequests) != 3 {
		t.Fatalf("discovered by %d requests, want 3", len(self.testRequests))
	}
	return slices.Clone(self.testRequests)
}

// The value a request asks for post_quantum_encryption, and whether it names
// the field at all.
func testingPostQuantumEncryptionRequested(t *testing.T, body string) (requested bool, present bool) {
	t.Helper()
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		t.Fatal(err)
	}
	value, present := fields["post_quantum_encryption"]
	if !present {
		return false, false
	}
	if err := json.Unmarshal(value, &requested); err != nil {
		t.Fatal(err)
	}
	return requested, true
}

// A multi client whose profile requires post-quantum encryption asks for
// providers that advertise it: the windows' own discovery, and every path a
// window discovers by, the named redial of a lost exit included. Turning the
// requirement off returns discovery to the request of an older client, and
// turning it on again asks again. Before this change every request was
// capability-blind, and a post-quantum window had to dial and fail through
// providers that could never seal its session.
func TestPostQuantumProfileAsksDiscoveryForPostQuantumProviders(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	postQuantum := &PerformanceProfile{WindowType: WindowTypeAuto, PostQuantumEncryption: true}
	fixture := newPostQuantumDiscoveryFixture(t, ctx, postQuantum)

	// every window request so far was made under the post-quantum profile
	for _, body := range fixture.windowDiscovered(t, ctx) {
		if requested, _ := testingPostQuantumEncryptionRequested(t, body); !requested {
			t.Fatalf("a post-quantum window discovered without asking for post-quantum providers: %s", body)
		}
	}
	for _, body := range fixture.discoverEveryPath(t, ctx) {
		if requested, _ := testingPostQuantumEncryptionRequested(t, body); !requested {
			t.Fatalf("a post-quantum discovery path did not ask for post-quantum providers: %s", body)
		}
	}

	AssertEqual(t, fixture.multi.SetPerformanceProfile(&PerformanceProfile{WindowType: WindowTypeAuto}), nil)
	for _, body := range fixture.discoverEveryPath(t, ctx) {
		if _, present := testingPostQuantumEncryptionRequested(t, body); present {
			t.Fatalf("discovery still asks for post-quantum providers after the profile stopped requiring them: %s", body)
		}
	}

	AssertEqual(t, fixture.multi.SetPerformanceProfile(postQuantum), nil)
	for _, body := range fixture.discoverEveryPath(t, ctx) {
		if requested, _ := testingPostQuantumEncryptionRequested(t, body); !requested {
			t.Fatalf("discovery did not ask for post-quantum providers after the profile required them again: %s", body)
		}
	}
}

// A multi client without the requirement, the default and every client before
// this change, keeps the request exactly as it was: no
// post_quantum_encryption field, so discovery stays capability-blind.
func TestDefaultProfileKeepsCapabilityBlindDiscovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	fixture := newPostQuantumDiscoveryFixture(t, ctx, nil)
	requests := append(fixture.windowDiscovered(t, ctx), fixture.discoverEveryPath(t, ctx)...)
	for _, body := range requests {
		if _, present := testingPostQuantumEncryptionRequested(t, body); present {
			t.Fatalf("a client without the requirement asked about post-quantum providers: %s", body)
		}
	}
}
