package connect

// Default DNS resolution stays on the tunnel: the host-network fallback ("fast DNS on connect")
// is an owner opt-in, never part of DefaultUpgradeMuxSettings. These tests observe the mux's
// installed resolvers and its fallback warm hook directly, with no network or timing.

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

func TestDefaultUpgradeMuxSettingsResolveOnlyThroughTunnel(t *testing.T) {
	settings := DefaultUpgradeMuxSettings()
	if settings.Dns.Fallback != nil {
		t.Fatalf("default dns fallback = %+v, want nil so dns resolves only through the tunnel", settings.Dns.Fallback)
	}
	resolver := settings.Dns.Resolver
	if resolver.EnableLocalDoh || resolver.EnableLocalDns {
		t.Fatalf("default tunnel resolver dials the host network: local doh=%t local dns=%t", resolver.EnableLocalDoh, resolver.EnableLocalDns)
	}
	if !resolver.EnableRemoteDoh {
		t.Fatal("default tunnel resolver must resolve over doh through the tunnel")
	}
}

func TestDefaultUpgradeMuxHasNoHostFallbackResolver(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec := &ipMuxRecorder{}
	mux, err := NewUpgradeMux(ctx, TransferPath{}, protocol.ProvideMode_Network, 0, rec.receive, DefaultUpgradeMuxSettings(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mux.Close()

	var fallbackWarmCount atomic.Int32
	mux.fallbackDohWarmFunction = func(context.Context, *DohCache, int) bool {
		fallbackWarmCount.Add(1)
		return true
	}

	if mux.fallbackDohCache.Load() != nil {
		t.Fatal("default mux built a host-network fallback resolver; dns must resolve only through the tunnel")
	}
	// with no fallback the warm request returns synchronously without dialing the host network
	mux.warmFallbackDns()
	if mux.fallbackDohWarmerRunning.Load() || fallbackWarmCount.Load() != 0 {
		t.Fatalf("default mux warmed the host-network fallback (running=%t count=%d)", mux.fallbackDohWarmerRunning.Load(), fallbackWarmCount.Load())
	}
}

func TestUpgradeMuxHostFallbackOnlyWhenEnabled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec := &ipMuxRecorder{}
	mux, err := NewUpgradeMux(ctx, TransferPath{}, protocol.ProvideMode_Network, 0, rec.receive, DefaultUpgradeMuxSettings(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mux.Close()

	fallbackWarms := make(chan *DohCache, 4)
	mux.fallbackDohWarmFunction = func(_ context.Context, cache *DohCache, _ int) bool {
		fallbackWarms <- cache
		return true
	}

	// the owner opts in
	enabled := DefaultUpgradeMuxSettings()
	enabled.Dns.Fallback = DefaultDnsUpgradeFallbackSettings()
	mux.SetSettings(enabled)
	fallback := mux.fallbackDohCache.Load()
	if fallback == nil {
		t.Fatal("enabled fallback did not install a host-network resolver")
	}
	if warmed := <-fallbackWarms; warmed != fallback {
		t.Fatal("enabled fallback warmed a resolver other than the installed one")
	}
	if !enabled.Dns.Fallback.EnableLocalDoh || enabled.Dns.Fallback.EnableLocalDns {
		t.Fatalf("fallback must use encrypted doh over the host network, not plaintext dns: %+v", enabled.Dns.Fallback)
	}

	// and back out
	mux.SetSettings(DefaultUpgradeMuxSettings())
	if mux.fallbackDohCache.Load() != nil {
		t.Fatal("disabled fallback left a host-network resolver installed")
	}
}
