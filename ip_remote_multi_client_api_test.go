package connect

// RemoveClientArgs teardown scope: a shutdown-caused teardown (generator ctx
// done) preserves the window identities ONLY when an identity store is
// configured (the proxy case — a replacement container reuses them). With the
// default nil store (plain sdk apps) the historical best-effort delete runs,
// so window platform-client rows do not leak until server-side idle reap.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Discovery retains the destination and the nearest eight intermediaries when
// a server returns a longer path; shorter legacy paths are unaffected.
func TestNextDestinationsRetainsMaximumIntermediariesAndDestination(t *testing.T) {
	intermediaryIds := make([]Id, MaxMultihopLength+3)
	for idIndex := range intermediaryIds {
		intermediaryIds[idIndex] = NewId()
	}
	providerId := NewId()
	wantEstimatedBytesPerSecond := ByteCount(7_500_000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/hello" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if request.URL.Path != "/network/find-providers2" {
			http.NotFound(w, request)
			return
		}
		if err := json.NewEncoder(w).Encode(&FindProviders2Result{
			Providers: []*FindProvidersProvider{{
				ClientId:                providerId,
				IntermediaryIds:         intermediaryIds,
				EstimatedBytesPerSecond: wantEstimatedBytesPerSecond,
				Tier:                    0,
				NetworkOnly:             true,
				ReputationFailedNames:   " Bloomberg ,canva,bloomberg",
			}},
		}); err != nil {
			t.Errorf("encode discovery response: %v", err)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultClientStrategySettings()
	settings.EnableNormal = true
	settings.EnableResilient = false
	settings.RequestTimeout = 5 * time.Second
	strategy := NewClientStrategy(ctx, settings)
	generator := NewApiMultiClientGenerator(
		ctx,
		[]*ProviderSpec{{BestAvailable: true}},
		strategy,
		nil,
		server.URL,
		"test-jwt",
		server.URL,
		"test-description",
		"test-spec",
		"0.0.0-test",
		nil,
		DefaultClientSettings,
		DefaultApiMultiClientGeneratorSettings(),
	)
	destinations, err := generator.NextDestinationsContext(ctx, 1, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(destinations) != 1 {
		t.Fatalf("destination count=%d want=1", len(destinations))
	}
	wantIds := append(
		slices.Clone(intermediaryIds[len(intermediaryIds)-MaxMultihopLength:]),
		providerId,
	)
	for destination, stats := range destinations {
		if !slices.Equal(destination.Ids(), wantIds) {
			t.Fatalf("destination ids=%v want=%v", destination.Ids(), wantIds)
		}
		if stats.EstimatedBytesPerSecond != wantEstimatedBytesPerSecond || !stats.NetworkOnly {
			t.Fatalf("discovery stats=%+v, want speed and network-only metadata", stats)
		}
		if !slices.Equal(stats.ReputationFailures, []string{"bloomberg", "canva"}) {
			t.Fatalf("reputation failures=%q, want normalized Bloomberg/Canva", stats.ReputationFailures)
		}
	}
}

// newRemoveClientTestGenerator builds a generator against a counting api
// server. The client strategy lives on its own ctx (like the app-scoped
// strategy in the field), so it outlives the generator teardown.
func newRemoveClientTestGenerator(t *testing.T, generatorCtx context.Context, strategyCtx context.Context) (*ApiMultiClientGenerator, *atomic.Int32, func()) {
	t.Helper()

	removeCount := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/network/remove-client") {
			removeCount.Add(1)
		}
		fmt.Fprintf(w, "{}")
	}))

	// a single-dialer strategy so each api call maps to exactly one server hit
	settings := DefaultClientStrategySettings()
	settings.EnableNormal = true
	settings.EnableResilient = false
	settings.RequestTimeout = 5 * time.Second
	strategy := NewClientStrategy(strategyCtx, settings)

	clientId := NewId()
	generator := NewApiMultiClientGenerator(
		generatorCtx,
		[]*ProviderSpec{{ClientId: &clientId}},
		strategy,
		nil,
		server.URL,
		"test-jwt",
		server.URL,
		"test-description",
		"test-spec",
		"0.0.0-test",
		nil,
		DefaultClientSettings,
		DefaultApiMultiClientGeneratorSettings(),
	)
	return generator, removeCount, server.Close
}

func TestRemoveClientArgsTeardownNoStoreDeletes(t *testing.T) {
	strategyCtx, strategyCancel := context.WithCancel(context.Background())
	defer strategyCancel()
	generatorCtx, generatorCancel := context.WithCancel(context.Background())

	generator, removeCount, closeServer := newRemoveClientTestGenerator(t, generatorCtx, strategyCtx)
	defer closeServer()

	// shutdown-caused teardown with NO identity store: the best-effort
	// delete must still reach the api so the platform client row is removed
	generatorCancel()
	generator.RemoveClientArgs(&MultiClientGeneratorClientArgs{
		ClientId: NewId(),
	})

	if !waitForCondition(5*time.Second, func() bool {
		return 1 <= removeCount.Load()
	}) {
		t.Fatal("teardown with no identity store must best-effort delete the network client")
	}
}

func TestRemoveClientArgsTeardownStorePreservesIdentities(t *testing.T) {
	strategyCtx, strategyCancel := context.WithCancel(context.Background())
	defer strategyCancel()
	generatorCtx, generatorCancel := context.WithCancel(context.Background())

	generator, removeCount, closeServer := newRemoveClientTestGenerator(t, generatorCtx, strategyCtx)
	defer closeServer()

	// an identity store is configured (the proxy case): identities must
	// survive teardown for the replacement container
	store := &fakeIdentityStore{}
	generator.SetIdentityStore(store)

	identity := &WindowClientIdentity{
		ClientId:    NewId(),
		ByJwt:       "jwt-live",
		InstanceId:  NewId(),
		Destination: RequireMultiHopId(NewId()),
	}
	generator.identityState.Record(identity)
	waitForPersisted(t, store, "the live identity", func(persisted []*WindowClientIdentity) bool {
		return len(persisted) == 1
	})

	// shutdown-caused teardown: neither the persisted identity nor the live
	// network client is removed
	generatorCancel()
	generator.RemoveClientArgs(&MultiClientGeneratorClientArgs{
		ClientId: identity.ClientId,
	})

	time.Sleep(500 * time.Millisecond)
	if count := removeCount.Load(); count != 0 {
		t.Fatalf("teardown with an identity store deleted %d network clients, want 0 (identities must survive)", count)
	}
	persisted := store.snapshot()
	if len(persisted) != 1 || persisted[0].ClientId != identity.ClientId {
		t.Fatalf("teardown with an identity store must preserve the persisted snapshot, have %d", len(persisted))
	}
}

func TestRemoveClientArgsLiveEvictionDeletes(t *testing.T) {
	strategyCtx, strategyCancel := context.WithCancel(context.Background())
	defer strategyCancel()
	generatorCtx, generatorCancel := context.WithCancel(context.Background())
	defer generatorCancel()

	generator, removeCount, closeServer := newRemoveClientTestGenerator(t, generatorCtx, strategyCtx)
	defer closeServer()

	// a window eviction while the ctx is live removes for real — with or
	// without a store configured
	store := &fakeIdentityStore{}
	generator.SetIdentityStore(store)
	identity := &WindowClientIdentity{
		ClientId:    NewId(),
		ByJwt:       "jwt-evict",
		InstanceId:  NewId(),
		Destination: RequireMultiHopId(NewId()),
	}
	generator.identityState.Record(identity)
	waitForPersisted(t, store, "the live identity", func(persisted []*WindowClientIdentity) bool {
		return len(persisted) == 1
	})

	generator.RemoveClientArgs(&MultiClientGeneratorClientArgs{
		ClientId: identity.ClientId,
	})

	if !waitForCondition(5*time.Second, func() bool {
		return 1 <= removeCount.Load()
	}) {
		t.Fatal("a live window eviction must delete the network client")
	}
	waitForPersisted(t, store, "the evicted identity dropped", func(persisted []*WindowClientIdentity) bool {
		return len(persisted) == 0
	})
}

func TestRemoveClientArgsStaleGenerationCannotDeleteLiveReplacement(t *testing.T) {
	strategyCtx, strategyCancel := context.WithCancel(context.Background())
	defer strategyCancel()
	generatorCtx, generatorCancel := context.WithCancel(context.Background())
	defer generatorCancel()

	generator, removeCount, closeServer := newRemoveClientTestGenerator(t, generatorCtx, strategyCtx)
	defer closeServer()
	store := &fakeIdentityStore{}
	generator.SetIdentityStore(store)

	clientId := NewId()
	oldIdentity := &WindowClientIdentity{
		ClientId:    clientId,
		ByJwt:       "jwt-old",
		InstanceId:  NewId(),
		Destination: RequireMultiHopId(NewId()),
	}
	replacement := &WindowClientIdentity{
		ClientId:    clientId,
		ByJwt:       "jwt-replacement",
		InstanceId:  NewId(),
		Destination: RequireMultiHopId(NewId()),
	}
	generator.identityState.Record(oldIdentity)
	generator.identityState.Record(replacement)
	waitForPersisted(t, store, "replacement identity", func(persisted []*WindowClientIdentity) bool {
		return len(persisted) == 1 && persisted[0].InstanceId == replacement.InstanceId
	})

	// The old channel's asynchronous cleanup arrives after replacement under
	// the same client id. It must not erase persistence or issue the server
	// removal, which is keyed only by client id and would kill the live row.
	generator.RemoveClientArgs(&MultiClientGeneratorClientArgs{
		ClientId: clientId,
		ClientAuth: &ClientAuth{
			InstanceId: oldIdentity.InstanceId,
		},
	})
	time.Sleep(100 * time.Millisecond)
	if count := removeCount.Load(); count != 0 {
		t.Fatalf("stale generation emitted %d remove-client requests", count)
	}
	persisted := store.snapshot()
	if len(persisted) != 1 || persisted[0].InstanceId != replacement.InstanceId {
		t.Fatal("stale generation erased the persisted replacement")
	}

	// The current generation still owns cleanup and must perform both effects.
	generator.RemoveClientArgs(&MultiClientGeneratorClientArgs{
		ClientId: clientId,
		ClientAuth: &ClientAuth{
			InstanceId: replacement.InstanceId,
		},
	})
	if !waitForCondition(5*time.Second, func() bool {
		return removeCount.Load() == 1
	}) {
		t.Fatal("current generation did not remove its network client")
	}
	waitForPersisted(t, store, "current identity removed", func(persisted []*WindowClientIdentity) bool {
		return len(persisted) == 0
	})
}

// A window client can still be sending its final contract-close control when
// the window retires it. The derived client JWT must remain valid until that
// OOB lifecycle is joined; deleting the network-client row first makes the
// close fail with 401 and leaves server-side contract cleanup behind.
func TestRemoveClientWithArgsJoinsOobBeforeIdentityRevocation(t *testing.T) {
	controlStarted := make(chan struct{})
	controlRelease := make(chan struct{})
	controlDone := make(chan error, 1)
	var startOnce sync.Once
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(controlRelease) }) })
	removeCount := &atomic.Int32{}

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/connect/control":
			var args ConnectControlArgs
			if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
				t.Errorf("decode control: %v", err)
				http.Error(w, "bad control", http.StatusBadRequest)
				return
			}
			startOnce.Do(func() { close(controlStarted) })
			<-controlRelease
			_ = json.NewEncoder(w).Encode(&ConnectControlResult{Pack: args.Pack})
		case "/network/remove-client":
			removeCount.Add(1)
			_, _ = w.Write([]byte("{}"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer apiServer.Close()

	strategyCtx, strategyCancel := context.WithCancel(context.Background())
	defer strategyCancel()
	strategySettings := DefaultClientStrategySettings()
	strategySettings.EnableNormal = true
	strategySettings.EnableResilient = false
	strategySettings.RequestTimeout = 5 * time.Second
	strategy := NewClientStrategy(strategyCtx, strategySettings)

	generatorCtx, generatorCancel := context.WithCancel(context.Background())
	defer generatorCancel()
	providerId := NewId()
	generator := NewApiMultiClientGenerator(
		generatorCtx,
		[]*ProviderSpec{{ClientId: &providerId}},
		strategy,
		nil,
		apiServer.URL,
		"network-jwt",
		apiServer.URL,
		"test-description",
		"test-spec",
		"0.0.0-test",
		nil,
		DefaultClientSettings,
		DefaultApiMultiClientGeneratorSettings(),
	)

	clientCtx, clientCancel := context.WithCancel(context.Background())
	defer clientCancel()
	clientId := NewId()
	clientOob := NewApiOutOfBandControl(clientCtx, strategy, "derived-client-jwt", apiServer.URL)
	client := NewClient(clientCtx, clientId, clientOob, DefaultClientSettings())
	clientOob.SendControlWithCtx(
		context.Background(),
		[]*protocol.Frame{},
		func(_ []*protocol.Frame, err error) { controlDone <- err },
	)
	select {
	case <-controlStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup control did not reach the server")
	}

	clientArgs := &MultiClientGeneratorClientArgs{
		ClientId: clientId,
		ClientAuth: &ClientAuth{
			ByJwt:      "derived-client-jwt",
			InstanceId: NewId(),
		},
	}
	generator.RemoveClientWithArgs(client, clientArgs)
	client.Cancel()
	time.Sleep(100 * time.Millisecond)
	if count := removeCount.Load(); count != 0 {
		t.Fatalf("identity was revoked while cleanup control was in flight: remove count %d", count)
	}

	releaseOnce.Do(func() { close(controlRelease) })
	select {
	case err := <-controlDone:
		if err != nil {
			t.Fatalf("cleanup control failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup control did not finish")
	}
	if !waitForCondition(5*time.Second, func() bool { return removeCount.Load() == 1 }) {
		t.Fatal("identity was not revoked after cleanup control completed")
	}
}

// A generated client's channel hands retirement back asynchronously after its
// cancellation edge. Generator teardown must wait for that Client/OOB join;
// otherwise a P2P send route can retain pooled Transfer frames after teardown.
func TestApiMultiClientGeneratorCloseAndWaitJoinsGeneratedClientRetirement(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	strategySettings := DefaultClientStrategySettings()
	strategy := NewClientStrategy(ctx, strategySettings)
	generator := NewApiMultiClientGenerator(
		ctx,
		nil,
		strategy,
		nil,
		"http://127.0.0.1:1",
		"network-jwt",
		"http://127.0.0.1:1",
		"test-description",
		"test-spec",
		"0.0.0-test",
		nil,
		DefaultClientSettings,
		DefaultApiMultiClientGeneratorSettings(),
	)
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), closeWaitClientSettings())
	args := &MultiClientGeneratorClientArgs{
		ClientId: client.ClientId(),
		ClientAuth: &ClientAuth{
			InstanceId: NewId(),
		},
	}

	// Model the successful NewClient boundary without making a platform request.
	// The channel owner observes Client.Done and then returns the client through
	// the same RemoveClientWithArgs path used by RemoteUserNatMultiClient.
	generator.transportLock.Lock()
	generator.transportIdle = make(chan struct{})
	generator.transports[client] = &apiWindowClientTransport{}
	generator.transportLock.Unlock()
	go func() {
		<-client.Done()
		generator.RemoveClientWithArgs(client, args)
	}()

	retirementEntered := make(chan struct{})
	releaseRetirement := make(chan struct{})
	var enteredOnce sync.Once
	var releaseOnce sync.Once
	client.beforeRunDoneWaitForTest = func() {
		enteredOnce.Do(func() { close(retirementEntered) })
		<-releaseRetirement
	}
	defer releaseOnce.Do(func() { close(releaseRetirement) })

	closeResult := make(chan error, 1)
	go func() {
		closeResult <- generator.CloseAndWait(ctx)
	}()
	waitCloseWaitBarrier(t, ctx, retirementEntered, "generated client retirement")
	select {
	case err := <-closeResult:
		t.Fatalf("generator close skipped held client retirement: %v", err)
	default:
	}

	releaseOnce.Do(func() { close(releaseRetirement) })
	select {
	case err := <-closeResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatalf("wait for generated client retirement: %v", ctx.Err())
	}
}
