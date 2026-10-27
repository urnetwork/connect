// A discovery generator reuses identities no provider can have seen instead
// of registering and retiring one per candidate. Every identity and endpoint
// here is synthetic; no public API request is allowed.
package connect

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// A window transport that reports whether the platform ever registered it.
type registrationWindowPlatformTransport struct {
	everConnected bool
}

func (self *registrationWindowPlatformTransport) ConnectedNotify() <-chan struct{} {
	return nil
}

func (self *registrationWindowPlatformTransport) IsConnected() bool {
	return false
}

func (self *registrationWindowPlatformTransport) Close() {
}

func (self *registrationWindowPlatformTransport) EverConnected() bool {
	return self.everConnected
}

// A generator that discovers the best available providers, as a hosted
// proxy's quality and speed windows do.
func newDiscoveryIdentityReuseFixture(t *testing.T, ctx context.Context, configure func(*ApiMultiClientGeneratorSettings)) *identityReuseFixture {
	t.Helper()
	return newIdentityReuseFixture(t, ctx, []*ProviderSpec{{BestAvailable: true}}, configure)
}

// One candidate for a newly discovered destination that failed before a
// client was constructed: the window hands the args back.
func (self *identityReuseFixture) failDiscoveryArgsAttempt(t *testing.T, p2pOnly bool) *MultiClientGeneratorClientArgs {
	t.Helper()
	args, err := self.generator.NewClientArgsForDestinationContext(t.Context(), RequireMultiHopId(NewId()))
	if err != nil {
		t.Fatal(err)
	}
	args.P2pOnly = p2pOnly
	self.generator.RemoveClientArgs(args)
	synctest.Wait()
	return args
}

// One candidate for a newly discovered destination whose client was
// constructed on `transport` and torn down, as a failed setup or evaluation
// does, optionally after the platform refused it. Returns the args and the
// client's identity key seed.
func (self *identityReuseFixture) failDiscoveryClientAttempt(t *testing.T, transport apiWindowPlatformTransport, refusal AuthorizationCloseCause) (*MultiClientGeneratorClientArgs, []byte) {
	t.Helper()
	args, err := self.generator.NewClientArgsForDestinationContext(t.Context(), RequireMultiHopId(NewId()))
	if err != nil {
		t.Fatal(err)
	}
	settings := closeWaitClientSettings()
	if 0 < len(args.clientKeySeed) {
		settings.ClientKeySeed = bytes.Clone(args.clientKeySeed)
	}
	client := NewClient(t.Context(), args.ClientId, NewNoContractClientOob(), settings)
	keySeed := client.ClientKeyManager().Seed()
	// tracked as NewClientContext tracks a constructed window client
	self.generator.transportLock.Lock()
	if len(self.generator.transports) == 0 {
		self.generator.transportIdle = make(chan struct{})
	}
	self.generator.transports[client] = &apiWindowClientTransport{createTime: time.Now(), current: transport}
	self.generator.transportLock.Unlock()
	if refusal != 0 {
		self.generator.noteWindowCredentialRefused(client, refusal)
	}
	self.generator.RemoveClientWithArgs(client, args)
	client.Cancel()
	synctest.Wait()
	return args, keySeed
}

// The hosted-proxy churn of 2026-10-22: 65% of about 310 window identities a
// second were retired without ever connecting, each one registration and one
// retirement. Candidates that failed before any client existed, each for a
// different discovered provider, must share one identity under a fresh
// instance id each time, and its single retirement happens at close.
func TestDiscoveryGeneratorReusesIdentityNoProviderSaw(t *testing.T) {
	// the process-wide message pool worker must not start inside the bubble
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		fixture := newDiscoveryIdentityReuseFixture(t, t.Context(), nil)
		clientIds := map[Id]bool{}
		instanceIds := map[Id]bool{}
		for range 5 {
			args := fixture.failDiscoveryArgsAttempt(t, false)
			clientIds[args.ClientId] = true
			instanceIds[args.ClientAuth.InstanceId] = true
		}
		t.Logf("five failed candidates: %d registrations, %d retirements, %d identities", fixture.mintCount.Load(), fixture.retireCount.Load(), len(clientIds))
		if fixture.mintCount.Load() != 1 || fixture.retireCount.Load() != 0 || len(clientIds) != 1 {
			t.Errorf("five failed candidates made %d registrations and %d retirements over %d identities, want 1, 0 and 1",
				fixture.mintCount.Load(), fixture.retireCount.Load(), len(clientIds))
		}
		if len(instanceIds) != 5 {
			t.Errorf("a reused identity kept an instance id: %d distinct of 5", len(instanceIds))
		}
		retiredBeforeClose := fixture.retireCount.Load()
		if err := fixture.generator.CloseAndWait(t.Context()); err != nil {
			t.Fatal(err)
		}
		if retiredAtClose := fixture.retireCount.Load() - retiredBeforeClose; retiredAtClose != 1 {
			t.Fatalf("close retired %d identities, want the one parked", retiredAtClose)
		}
	})
}

// A candidate cut before its platform transport registered is the common
// case under a slow platform. Its identity parks, and its next client keeps
// the same identity key, so a key it registered is not appended again.
func TestDiscoveryGeneratorReusesIdentityOfClientThatNeverRegistered(t *testing.T) {
	// the process-wide message pool worker must not start inside the bubble
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		fixture := newDiscoveryIdentityReuseFixture(t, t.Context(), nil)
		first, firstKeySeed := fixture.failDiscoveryClientAttempt(t, &registrationWindowPlatformTransport{}, 0)
		second, secondKeySeed := fixture.failDiscoveryClientAttempt(t, &registrationWindowPlatformTransport{}, 0)
		if first.ClientId != second.ClientId || fixture.mintCount.Load() != 1 || fixture.retireCount.Load() != 0 {
			t.Errorf("never-registered client: reused=%t registrations=%d retirements=%d",
				first.ClientId == second.ClientId, fixture.mintCount.Load(), fixture.retireCount.Load())
		}
		if !bytes.Equal(firstKeySeed, secondKeySeed) {
			t.Error("the reused identity's next client registered a different identity key")
		}
		if err := fixture.generator.CloseAndWait(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

// An identity a provider may have seen never follows the window to another
// provider: a client whose transport registered, one whose transport cannot
// say, a refused credential and a p2p-only candidate each retire, and the
// next candidate registers a fresh identity.
func TestDiscoveryGeneratorRetiresIdentityAProviderMayHaveSeen(t *testing.T) {
	// the process-wide message pool worker must not start inside the bubble
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		fixture := newDiscoveryIdentityReuseFixture(t, t.Context(), nil)
		registered, _ := fixture.failDiscoveryClientAttempt(t, &registrationWindowPlatformTransport{everConnected: true}, 0)
		unknown, _ := fixture.failDiscoveryClientAttempt(t, newFakeWindowPlatformTransport(false), 0)
		refused, _ := fixture.failDiscoveryClientAttempt(t, &registrationWindowPlatformTransport{}, AuthorizationCloseRevoked)
		p2pOnly := fixture.failDiscoveryArgsAttempt(t, true)
		clientIds := map[Id]bool{registered.ClientId: true, unknown.ClientId: true, refused.ClientId: true, p2pOnly.ClientId: true}
		if len(clientIds) != 4 || fixture.mintCount.Load() != 4 || fixture.retireCount.Load() != 4 {
			t.Fatalf("seen identities: %d distinct, registrations=%d retirements=%d, want 4, 4 and 4",
				len(clientIds), fixture.mintCount.Load(), fixture.retireCount.Load())
		}
		if err := fixture.generator.CloseAndWait(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

// A parked identity no candidate took within the park age retires on its
// own, and the next candidate registers a fresh identity.
func TestDiscoveryParkedIdentityAgesOut(t *testing.T) {
	// the process-wide message pool worker must not start inside the bubble
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		fixture := newDiscoveryIdentityReuseFixture(t, t.Context(), nil)
		parked := fixture.failDiscoveryArgsAttempt(t, false)
		time.Sleep(fixture.generator.discoveryParkMaxAge - time.Second)
		synctest.Wait()
		if fixture.retireCount.Load() != 0 {
			t.Fatalf("a parked identity retired %d times before its park age", fixture.retireCount.Load())
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if fixture.retireCount.Load() != 1 {
			t.Fatalf("a parked identity past its park age retired %d times, want 1", fixture.retireCount.Load())
		}
		next, err := fixture.generator.NewClientArgsForDestinationContext(t.Context(), RequireMultiHopId(NewId()))
		if err != nil {
			t.Fatal(err)
		}
		if next.ClientId == parked.ClientId || fixture.mintCount.Load() != 2 {
			t.Fatalf("an aged-out identity was reused: registrations=%d", fixture.mintCount.Load())
		}
		fixture.generator.RemoveClientArgs(next)
		if err := fixture.generator.CloseAndWait(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

// The park holds at most DiscoveryIdentityParkMaxCount identities; one more
// retires, and candidates take the parked ones oldest first before minting.
func TestDiscoveryParkHoldsAtMostItsBound(t *testing.T) {
	// the process-wide message pool worker must not start inside the bubble
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		fixture := newDiscoveryIdentityReuseFixture(t, t.Context(), func(settings *ApiMultiClientGeneratorSettings) {
			settings.DiscoveryIdentityParkMaxCount = 2
		})
		inFlight := []*MultiClientGeneratorClientArgs{}
		for range 3 {
			args, err := fixture.generator.NewClientArgsForDestinationContext(t.Context(), RequireMultiHopId(NewId()))
			if err != nil {
				t.Fatal(err)
			}
			inFlight = append(inFlight, args)
		}
		for _, args := range inFlight {
			fixture.generator.RemoveClientArgs(args)
			synctest.Wait()
		}
		if fixture.mintCount.Load() != 3 || fixture.retireCount.Load() != 1 {
			t.Fatalf("three returned identities: registrations=%d retirements=%d, want 3 and 1", fixture.mintCount.Load(), fixture.retireCount.Load())
		}
		for i, want := range []Id{inFlight[0].ClientId, inFlight[1].ClientId} {
			args, err := fixture.generator.NewClientArgsForDestinationContext(t.Context(), RequireMultiHopId(NewId()))
			if err != nil {
				t.Fatal(err)
			}
			if args.ClientId != want {
				t.Fatalf("candidate %d did not take the oldest parked identity", i+1)
			}
		}
		if _, err := fixture.generator.NewClientArgsForDestinationContext(t.Context(), RequireMultiHopId(NewId())); err != nil {
			t.Fatal(err)
		}
		if fixture.mintCount.Load() != 4 {
			t.Fatalf("an empty park did not mint: registrations=%d, want 4", fixture.mintCount.Load())
		}
		if err := fixture.generator.CloseAndWait(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

// An owner that sets no park keeps one registration and retirement per
// discovery candidate.
func TestDiscoveryParkCanBeDisabled(t *testing.T) {
	// the process-wide message pool worker must not start inside the bubble
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		fixture := newDiscoveryIdentityReuseFixture(t, t.Context(), func(settings *ApiMultiClientGeneratorSettings) {
			settings.DiscoveryIdentityParkMaxCount = 0
		})
		fixture.failDiscoveryArgsAttempt(t, false)
		fixture.failDiscoveryArgsAttempt(t, false)
		if fixture.mintCount.Load() != 2 || fixture.retireCount.Load() != 2 {
			t.Fatalf("disabled park: registrations=%d retirements=%d", fixture.mintCount.Load(), fixture.retireCount.Load())
		}
		if err := fixture.generator.CloseAndWait(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

// Discovery parking closes atomically with taking the parked set, a
// superseded generation or a parked identity cannot park again, and a closed
// state hands nothing out.
func TestWindowIdentityDiscoveryParkingClosesWithTheParkedSet(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		state := newWindowIdentityState(t.Context(), nil)
		first := &WindowClientIdentity{ClientId: NewId(), InstanceId: NewId(), Destination: RequireMultiHopId(NewId())}
		second := &WindowClientIdentity{ClientId: NewId(), InstanceId: NewId(), Destination: RequireMultiHopId(NewId())}
		state.Record(first)
		state.Record(second)
		ageOut := func(*parkedDiscoveryIdentity) {
			t.Error("a parked identity aged out during the test")
		}
		if state.ParkDiscovery(first.ClientId, NewId(), nil, 4, time.Hour, ageOut) != nil {
			t.Fatal("a superseded generation parked")
		}
		if state.ParkDiscovery(first.ClientId, first.InstanceId, nil, 4, time.Hour, ageOut) == nil {
			t.Fatal("the current generation did not park")
		}
		if state.ParkDiscovery(first.ClientId, first.InstanceId, nil, 4, time.Hour, ageOut) != nil {
			t.Fatal("a parked identity parked twice")
		}
		if parked := state.CloseParking(); len(parked) != 1 || parked[0].ClientId != first.ClientId {
			t.Fatalf("close took %d parked identities", len(parked))
		}
		if state.ParkDiscovery(second.ClientId, second.InstanceId, nil, 4, time.Hour, ageOut) != nil {
			t.Fatal("an identity parked after close")
		}
		if parked, _ := state.TakeDiscoveryParked(); parked != nil {
			t.Fatal("a closed state handed out a parked identity")
		}
		// the closed park's age-out never fires
		time.Sleep(2 * time.Hour)
		synctest.Wait()
	})
}

// The production window loops on a discovery generator whose candidates are
// all cut before their platform transport registers, as a saturated platform
// cuts them: every pass constructs clients that fail setup. The window must
// recycle the identities of those candidates instead of registering one per
// candidate.
func TestDiscoveryWindowReusesIdentitiesOfCandidatesThatNeverConnected(t *testing.T) {
	// the process-wide message pool worker must not start inside the bubble
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		fixture := newDiscoveryIdentityReuseFixture(t, ctx, func(settings *ApiMultiClientGeneratorSettings) {
			settings.ClientControl = &refusingIdentityReuseControl{}
			settings.ProviderDiscovery = localDiscoveryTestFunc(func(_ context.Context, _ string, args *FindProviders2Args) (*FindProviders2Result, error) {
				providers := []*FindProvidersProvider{}
				for range max(1, args.Count) {
					providers = append(providers, &FindProvidersProvider{ClientId: NewId(), EstimatedBytesPerSecond: 1024})
				}
				return &FindProviders2Result{Providers: providers}, nil
			})
		})
		var candidateCount atomic.Int32
		fixture.generator.newPlatformTransport = func(*Client, *ClientAuth, TransportMode, *PlatformTransportSettings) apiWindowPlatformTransport {
			candidateCount.Add(1)
			return &registrationWindowPlatformTransport{}
		}

		window := outcomeTestWindow(ctx, newRecordingLogger())
		window.cancel = cancel
		window.generator = fixture.generator
		window.windowSizeLimit = 1
		window.clientChannelArgs = make(chan *multiClientChannelArgs)
		window.contractStatusCallbacks = NewCallbackList[*contractStatusCallbackWorker]()
		window.budgetFailThrottle = newLogThrottle(window.settings.EvaluationFailureLogInterval)
		window.backendDegradedForTest = func() bool { return false }

		var loops sync.WaitGroup
		loops.Add(2)
		go func() {
			defer loops.Done()
			window.randomEnumerateClientArgs()
		}()
		go func() {
			defer loops.Done()
			window.resize()
		}()
		time.Sleep(20 * time.Minute)
		cancel()
		loops.Wait()
		if err := fixture.generator.CloseAndWait(t.Context()); err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		synctest.Wait()

		t.Logf("%d candidates over %d registrations and %d retirements in 20 minutes", candidateCount.Load(), fixture.mintCount.Load(), fixture.retireCount.Load())
		if candidateCount.Load() < 10 {
			t.Fatalf("the window tried %d candidates, the test needs at least 10", candidateCount.Load())
		}
		// an expansion pass has at most four candidates in flight
		if 4 < fixture.mintCount.Load() {
			t.Fatalf("%d candidates that never connected registered %d identities, want at most 4", candidateCount.Load(), fixture.mintCount.Load())
		}
	})
}
