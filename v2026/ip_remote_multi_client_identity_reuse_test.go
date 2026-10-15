// A fixed destination's failed attempts reuse one derived identity instead of
// registering and retiring one per attempt. Every identity and endpoint here
// is synthetic; no public API request is allowed.
package connect

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// One credential authority counting registrations and retirements behind a
// generator whose public API refuses every request.
type identityReuseFixture struct {
	generator   *ApiMultiClientGenerator
	destination MultiHopId
	mintCount   atomic.Int32
	retireCount atomic.Int32
}

// `specs` nil pins the generator to one provider, as a hosted proxy pinned to
// a provider device is.
func newIdentityReuseFixture(t *testing.T, ctx context.Context, specs []*ProviderSpec, configure func(*ApiMultiClientGeneratorSettings)) *identityReuseFixture {
	t.Helper()
	api, strategy := authObservationTestApi(ctx, nil, true, serialTestRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Error("identity reuse contacted the public API")
		return nil, errors.New("unexpected public request")
	}))
	t.Cleanup(api.Close)
	fixture := &identityReuseFixture{}
	settings := DefaultApiMultiClientGeneratorSettings()
	settings.ClientCredentials = &generatorCredentialFixture{
		auth: func(context.Context, *AuthNetworkClientArgs) (*AuthNetworkClientResult, error) {
			fixture.mintCount.Add(1)
			return &AuthNetworkClientResult{ByClientJwt: generatorCredentialTestJwt(t, NewId())}, nil
		},
		remove: func(context.Context, *RemoveNetworkClientArgs) (*RemoveNetworkClientResult, error) {
			fixture.retireCount.Add(1)
			return &RemoveNetworkClientResult{}, nil
		},
	}
	if configure != nil {
		configure(settings)
	}
	provider, source := NewId(), NewId()
	if specs == nil {
		specs = []*ProviderSpec{{ClientId: &provider}}
	}
	fixture.destination = RequireMultiHopId(provider)
	fixture.generator = NewApiMultiClientGenerator(ctx, specs, strategy, nil,
		"https://api.reuse.example", "synthetic-parent-token", "wss://platform.reuse.example",
		"synthetic reuse", "synthetic", "test", &source, DefaultClientSettings, settings)
	return fixture
}

// One attempt whose candidate failed before a client was constructed: the
// window hands the args back, as it does for an expired or failed setup.
func (self *identityReuseFixture) failArgsAttempt(t *testing.T) *MultiClientGeneratorClientArgs {
	t.Helper()
	args, err := self.generator.NewClientArgsForDestinationContext(t.Context(), self.destination)
	if err != nil {
		t.Fatal(err)
	}
	self.generator.RemoveClientArgs(args)
	synctest.Wait()
	return args
}

// One attempt whose candidate client was constructed, optionally served for
// `served`, and then torn down, as a failed evaluation does, optionally after
// the platform refused it.
func (self *identityReuseFixture) failClientAttempt(t *testing.T, refusal AuthorizationCloseCause, served time.Duration) *MultiClientGeneratorClientArgs {
	t.Helper()
	args, err := self.generator.NewClientArgsForDestinationContext(t.Context(), self.destination)
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(t.Context(), args.ClientId, NewNoContractClientOob(), closeWaitClientSettings())
	// tracked as NewClientContext tracks a constructed window client
	self.generator.transportLock.Lock()
	if len(self.generator.transports) == 0 {
		self.generator.transportIdle = make(chan struct{})
	}
	self.generator.transports[client] = &apiWindowClientTransport{createTime: time.Now()}
	self.generator.transportLock.Unlock()
	if refusal != 0 {
		self.generator.noteWindowCredentialRefused(client, refusal)
	}
	time.Sleep(served)
	self.generator.RemoveClientWithArgs(client, args)
	client.Cancel()
	synctest.Wait()
	return args
}

// The root cause of the registration volume behind every failed attempt: each
// re-dial of a fixed destination registered a fresh derived client and the
// failed one was retired, two transactions per attempt against an unreachable
// provider. Five failed attempts must share one identity, under a fresh
// instance id each time, and its single retirement happens at close.
func TestFixedDestinationRedialReusesItsIdentity(t *testing.T) {
	// the process-wide message pool worker must not start inside the bubble
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		fixture := newIdentityReuseFixture(t, t.Context(), nil, nil)
		clientIds := map[Id]bool{}
		instanceIds := map[Id]bool{}
		for range 5 {
			args := fixture.failArgsAttempt(t)
			clientIds[args.ClientId] = true
			instanceIds[args.ClientAuth.InstanceId] = true
		}
		t.Logf("five failed attempts: %d registrations, %d retirements, %d identities", fixture.mintCount.Load(), fixture.retireCount.Load(), len(clientIds))
		if fixture.mintCount.Load() != 1 || fixture.retireCount.Load() != 0 || len(clientIds) != 1 {
			t.Errorf("five failed attempts made %d registrations and %d retirements over %d identities, want 1, 0 and 1",
				fixture.mintCount.Load(), fixture.retireCount.Load(), len(clientIds))
		}
		if len(instanceIds) != 5 {
			t.Errorf("reused identity kept an instance id: %d distinct of 5", len(instanceIds))
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

// A torn-down candidate client parks its identity once every Client and OOB
// join completes, and the next attempt takes it.
func TestFixedDestinationClientTeardownParksIdentity(t *testing.T) {
	// the process-wide message pool worker must not start inside the bubble
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		fixture := newIdentityReuseFixture(t, t.Context(), nil, nil)
		first := fixture.failClientAttempt(t, 0, 0)
		second := fixture.failClientAttempt(t, 0, 0)
		if first.ClientId != second.ClientId || fixture.mintCount.Load() != 1 || fixture.retireCount.Load() != 0 {
			t.Errorf("client teardown: reused=%t registrations=%d retirements=%d",
				first.ClientId == second.ClientId, fixture.mintCount.Load(), fixture.retireCount.Load())
		}
		retiredBeforeClose := fixture.retireCount.Load()
		if err := fixture.generator.CloseAndWait(t.Context()); err != nil {
			t.Fatal(err)
		}
		if retiredAtClose := fixture.retireCount.Load() - retiredBeforeClose; retiredAtClose != 1 {
			t.Fatalf("close retired %d identities, want 1", retiredAtClose)
		}
	})
}

// A credential the platform definitively refused is retired, and the next
// attempt registers a fresh identity. An unavailable close says nothing about
// the credential and keeps it reusable.
func TestFixedDestinationRefusedIdentityIsRetired(t *testing.T) {
	// the process-wide message pool worker must not start inside the bubble
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		fixture := newIdentityReuseFixture(t, t.Context(), nil, nil)
		first := fixture.failClientAttempt(t, AuthorizationCloseUnavailable, 0)
		second := fixture.failClientAttempt(t, AuthorizationCloseRevoked, 0)
		if first.ClientId != second.ClientId || fixture.retireCount.Load() != 1 {
			t.Fatalf("refusal: unavailable kept=%t retirements=%d", first.ClientId == second.ClientId, fixture.retireCount.Load())
		}
		third := fixture.failArgsAttempt(t)
		if third.ClientId == second.ClientId || fixture.mintCount.Load() != 2 {
			t.Fatalf("a refused identity was reused: registrations=%d", fixture.mintCount.Load())
		}
		if err := fixture.generator.CloseAndWait(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

// A client that served past a failed attempt (lifetime rotation, loss after
// service) retires its identity, so the replacement gets a fresh one.
func TestFixedDestinationServedIdentityRotates(t *testing.T) {
	// the process-wide message pool worker must not start inside the bubble
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		fixture := newIdentityReuseFixture(t, t.Context(), nil, nil)
		served := fixture.failClientAttempt(t, 0, windowIdentityReuseMaxClientAge)
		replacement := fixture.failArgsAttempt(t)
		if served.ClientId == replacement.ClientId || fixture.mintCount.Load() != 2 || fixture.retireCount.Load() != 1 {
			t.Errorf("a served identity was reused: registrations=%d retirements=%d", fixture.mintCount.Load(), fixture.retireCount.Load())
		}
		if err := fixture.generator.CloseAndWait(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

// Reuse keeps the generation token: a stale generation's late cleanup can
// neither park nor retire the live replacement under the same client id, and
// the current generation parks it for the next dial.
func TestFixedDestinationStaleGenerationCannotParkReplacement(t *testing.T) {
	// the process-wide message pool worker must not start inside the bubble
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		fixture := newIdentityReuseFixture(t, t.Context(), nil, nil)
		clientId := NewId()
		stale := &WindowClientIdentity{ClientId: clientId, ByJwt: "synthetic-stale", InstanceId: NewId(), Destination: fixture.destination}
		replacement := &WindowClientIdentity{ClientId: clientId, ByJwt: "synthetic-replacement", InstanceId: NewId(), Destination: fixture.destination}
		fixture.generator.identityState.Record(stale)
		fixture.generator.identityState.Record(replacement)
		fixture.generator.RemoveClientArgs(&MultiClientGeneratorClientArgs{ClientId: clientId, ClientAuth: &ClientAuth{InstanceId: stale.InstanceId}})
		synctest.Wait()
		if parked, _ := fixture.generator.identityState.TakeParked(fixture.destination); fixture.retireCount.Load() != 0 || parked != nil {
			t.Fatal("a stale generation retired or parked the live replacement")
		}
		fixture.generator.RemoveClientArgs(&MultiClientGeneratorClientArgs{ClientId: clientId, ClientAuth: &ClientAuth{InstanceId: replacement.InstanceId}})
		synctest.Wait()
		args, err := fixture.generator.NewClientArgsForDestinationContext(t.Context(), fixture.destination)
		if err != nil || args.ClientId != clientId || args.ClientAuth.ByJwt != replacement.ByJwt || fixture.retireCount.Load() != 0 || fixture.mintCount.Load() != 0 {
			t.Errorf("the current generation was not reused: retirements=%d registrations=%d", fixture.retireCount.Load(), fixture.mintCount.Load())
		}
		if err := fixture.generator.CloseAndWait(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

// A setup refusal from the control authority (401/403) marks the identity.
func TestCredentialRefusedSetupErrorClassification(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{err: &HttpStatusError{StatusCode: http.StatusUnauthorized}, want: true},
		{err: errors.Join(errors.New("setup"), &HttpStatusError{StatusCode: http.StatusForbidden}), want: true},
		{err: &HttpStatusError{StatusCode: http.StatusServiceUnavailable}, want: false},
		{err: context.DeadlineExceeded, want: false},
	} {
		if got := credentialRefusedSetupError(c.err); got != c.want {
			t.Errorf("refusal(%v) = %t, want %t", c.err, got, c.want)
		}
	}
}

// Discovery generators never park: a discovered provider may not be dialed
// again, and an identity must not follow the window to another provider.
func TestDiscoveryGeneratorRetiresEveryIdentity(t *testing.T) {
	// the process-wide message pool worker must not start inside the bubble
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		fixture := newIdentityReuseFixture(t, t.Context(), []*ProviderSpec{{BestAvailable: true}}, nil)
		first := fixture.failArgsAttempt(t)
		second := fixture.failArgsAttempt(t)
		if first.ClientId == second.ClientId || fixture.mintCount.Load() != 2 || fixture.retireCount.Load() != 2 {
			t.Fatalf("discovery generator reused an identity: registrations=%d retirements=%d", fixture.mintCount.Load(), fixture.retireCount.Load())
		}
		if err := fixture.generator.CloseAndWait(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

// An owner that opts out keeps one registration and retirement per attempt.
func TestFixedDestinationReuseCanBeDisabled(t *testing.T) {
	// the process-wide message pool worker must not start inside the bubble
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		fixture := newIdentityReuseFixture(t, t.Context(), nil, func(settings *ApiMultiClientGeneratorSettings) {
			settings.ReuseFixedDestinationIdentities = false
		})
		fixture.failArgsAttempt(t)
		fixture.failArgsAttempt(t)
		if fixture.mintCount.Load() != 2 || fixture.retireCount.Load() != 2 {
			t.Fatalf("disabled reuse: registrations=%d retirements=%d", fixture.mintCount.Load(), fixture.retireCount.Load())
		}
		if err := fixture.generator.CloseAndWait(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

// At process shutdown a store keeps the parked identity for the restarted
// process, as it keeps every live identity; nothing is retired.
func TestParkedIdentitySurvivesShutdownWithStore(t *testing.T) {
	// the process-wide message pool worker must not start inside the bubble
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		fixture := newIdentityReuseFixture(t, ctx, nil, nil)
		store := &fakeIdentityStore{}
		fixture.generator.SetIdentityStore(store)
		parked := fixture.failArgsAttempt(t)
		cancel()
		if err := fixture.generator.CloseAndWait(t.Context()); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if fixture.retireCount.Load() != 0 {
			t.Fatalf("shutdown retired %d identities a store keeps", fixture.retireCount.Load())
		}
		persisted := store.snapshot()
		if len(persisted) != 1 || persisted[0].ClientId != parked.ClientId || persisted[0].Destination != fixture.destination {
			t.Fatalf("store lost the parked identity: %d persisted", len(persisted))
		}
	})
}

// Parking closes atomically with taking the parked set: a teardown that
// races the owner's close retires rather than parks.
func TestWindowIdentityParkingClosesWithTheParkedSet(t *testing.T) {
	state := newWindowIdentityState(t.Context(), nil)
	destination := RequireMultiHopId(NewId())
	first := &WindowClientIdentity{ClientId: NewId(), InstanceId: NewId(), Destination: destination}
	second := &WindowClientIdentity{ClientId: NewId(), InstanceId: NewId(), Destination: destination}
	state.Record(first)
	state.Record(second)
	if state.Park(first.ClientId, NewId(), nil) {
		t.Fatal("a superseded generation parked")
	}
	if !state.Park(first.ClientId, first.InstanceId, nil) {
		t.Fatal("the current generation did not park")
	}
	if state.Park(second.ClientId, second.InstanceId, nil) {
		t.Fatal("a destination parked a second identity")
	}
	if parked := state.CloseParking(); len(parked) != 1 || parked[0].ClientId != first.ClientId {
		t.Fatalf("close took %d parked identities", len(parked))
	}
	if state.Park(second.ClientId, second.InstanceId, nil) {
		t.Fatal("an identity parked after close")
	}
	if parked, _ := state.TakeParked(destination); parked != nil {
		t.Fatal("a closed state handed out a parked identity")
	}
}

// Local control that refuses every frame, so a candidate's setup fails after
// its client and key exist, the way a failed attempt tears a client down.
type refusingIdentityReuseControl struct{}

func (self *refusingIdentityReuseControl) ConnectControl(context.Context, string, *ConnectControlArgs) (*ConnectControlResult, error) {
	return nil, errors.New("synthetic control unavailable")
}

// Every client generated a fresh identity key, and each registration of a
// different key for the same client id appends a signed generation to its
// history, which has a finite lifetime bound. A reused identity's next client
// must carry the same key, so its re-registration is idempotent.
func TestFixedDestinationReusedIdentityKeepsItsKey(t *testing.T) {
	// the process-wide message pool worker must not start inside the bubble
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		fixture := newIdentityReuseFixture(t, t.Context(), nil, func(settings *ApiMultiClientGeneratorSettings) {
			settings.ClientControl = &refusingIdentityReuseControl{}
		})
		type constructed struct {
			clientId Id
			keySeed  []byte
		}
		clients := []constructed{}
		fixture.generator.newPlatformTransport = func(client *Client, _ *ClientAuth, _ TransportMode, _ *PlatformTransportSettings) apiWindowPlatformTransport {
			clients = append(clients, constructed{clientId: client.ClientId(), keySeed: client.ClientKeyManager().Seed()})
			return newFakeWindowPlatformTransport(true)
		}
		for range 3 {
			args, err := fixture.generator.NewClientArgsForDestinationContext(t.Context(), fixture.destination)
			if err != nil {
				t.Fatal(err)
			}
			if client, err := fixture.generator.NewClientContext(t.Context(), t.Context(), args, closeWaitClientSettings()); client != nil || err == nil {
				t.Fatal("a refused setup returned a client")
			}
			synctest.Wait()
		}
		if len(clients) != 3 || fixture.mintCount.Load() != 1 {
			t.Errorf("three attempts constructed %d clients over %d registrations, want 3 over 1", len(clients), fixture.mintCount.Load())
		} else {
			for _, client := range clients[1:] {
				if client.clientId != clients[0].clientId || !bytes.Equal(client.keySeed, clients[0].keySeed) {
					t.Errorf("a reused identity's client registered a different identity key")
				}
			}
		}
		if err := fixture.generator.CloseAndWait(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}
