package connect

// RemoveClientArgs teardown scope: a shutdown-caused teardown (generator ctx
// done) preserves the window identities ONLY when an identity store is
// configured (the proxy case — a replacement container reuses them). With the
// default nil store (plain sdk apps) the historical best-effort delete runs,
// so window platform-client rows do not leak until server-side idle reap.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

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
