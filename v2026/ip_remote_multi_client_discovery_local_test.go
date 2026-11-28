package connect

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

type localDiscoveryTestFunc func(context.Context, string, *FindProviders2Args) (*FindProviders2Result, error)

func (f localDiscoveryTestFunc) FindProviders2(ctx context.Context, token string, args *FindProviders2Args) (*FindProviders2Result, error) {
	return f(ctx, token, args)
}

// Exercise the production non-fixed discovery branch. Local refusal has no
// HTTP fallback, while removing the injection proves the HTTP trap is live.
func TestLocalProviderDiscoveryRequestAndNoHttpFallback(t *testing.T) {
	var httpCalls atomic.Int64
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalls.Add(1)
		http.Error(w, "API unavailable", http.StatusServiceUnavailable)
	}))
	defer api.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	s := DefaultClientStrategySettings()
	s.EnableResilient = false
	s.RequestTimeout = time.Second
	strategy := NewClientStrategy(ctx, s)
	defer strategy.Close()
	provider := NewId()
	var calls int
	local := localDiscoveryTestFunc(func(callCtx context.Context, token string, args *FindProviders2Args) (*FindProviders2Result, error) {
		calls++
		if token != "refreshed" || len(args.Specs) != 1 || !args.Specs[0].BestAvailable || args.Count != 3 || args.RankMode != "quality" {
			t.Error("local discovery lost request or current credentials")
		}
		if _, ok := callCtx.Deadline(); !ok {
			t.Error("local discovery lost its finite deadline")
		}
		if calls == 2 {
			return nil, errors.New("local refusal")
		}
		return &FindProviders2Result{Providers: []*FindProvidersProvider{{ClientId: provider, EstimatedBytesPerSecond: 1234, NetworkOnly: true}}}, nil
	})
	makeGenerator := func(hook NetworkProviderDiscovery) *ApiMultiClientGenerator {
		settings := DefaultApiMultiClientGeneratorSettings()
		settings.ProviderDiscovery = hook
		g := NewApiMultiClientGenerator(ctx, []*ProviderSpec{{BestAvailable: true}}, strategy, nil, api.URL, "initial", api.URL, "test", "test", "test", nil, DefaultClientSettings, settings)
		g.SetByJwt("refreshed")
		t.Cleanup(func() { _ = g.CloseAndWait(context.Background()) })
		return g
	}
	g := makeGenerator(local)
	destinations, err := g.NextDestinationsContext(ctx, 3, nil, "quality")
	if err != nil || len(destinations) != 1 {
		t.Fatal("local discovery failed", err)
	}
	for dest, stats := range destinations {
		if dest.Ids()[len(dest.Ids())-1] != provider || stats.EstimatedBytesPerSecond != 1234 || !stats.NetworkOnly {
			t.Fatal("discovery response lost identity or ranking")
		}
	}
	if _, err = g.NextDestinationsContext(ctx, 3, nil, "quality"); err == nil {
		t.Fatal("local refusal was hidden")
	}
	if httpCalls.Load() != 0 || calls != 2 {
		t.Fatal("local discovery escaped to HTTP or was unused")
	}
	legacy := makeGenerator(nil)
	if _, err = legacy.NextDestinationsContext(ctx, 3, nil, "quality"); err == nil {
		t.Fatal("disabled discovery hook unexpectedly succeeded")
	}
	if httpCalls.Load() == 0 {
		t.Fatal("missing discovery injection did not exercise the HTTP trap")
	}
}
