package connect

// Local route replacement is distinguishable from a network failure. Fake
// time and an instance-owned paced reconnect make both timing contracts exact.

import (
	"context"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// A body clone runs after selection and before client acquisition. Retiring
// that selected route must refresh a ready replacement before reconnect wait;
// the same ordering is exercised by parallel, preferred serial, and hello.
func TestClientStrategyRetirementRefreshSkipsReconnect(t *testing.T) {
	for _, mode := range []string{"parallel", "serial", "hello"} {
		synctest.Test(t, func(t *testing.T) {
			strategy, old := newRetirementTestStrategy(t)
			strategy.settings.RequestTimeout = time.Minute
			strategy.settings.ReconnectTimeout = 15 * time.Second
			strategy.reconnectFactory = NewPacedReconnect
			if mode == "serial" {
				strategy.RecordDeliveryOutcome(strategy.dialerInfo(old), deliveryVerifiedByteCount, false)
			}
			var oldPools atomic.Int32
			old.httpClientFactory = func() *http.Client {
				oldPools.Add(1)
				return retirementTestClient(&retirementTestTransport{})
			}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://api.example/request", strings.NewReader("input"))
			if err != nil {
				t.Fatal(err)
			}
			hello, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.example/hello", strings.NewReader("input"))
			if err != nil {
				t.Fatal(err)
			}
			var replaced atomic.Bool
			clone := func() (io.ReadCloser, error) {
				if replaced.CompareAndSwap(false, true) {
					strategy.SetVlessConfigs(strategy.VlessConfigs())
					for current := range strategy.dialers {
						current.httpClientFactory = func() *http.Client { return retirementTestClient(&retirementTestTransport{}) }
					}
				}
				return io.NopCloser(strings.NewReader("input")), nil
			}
			request.GetBody, hello.GetBody = clone, clone
			started := time.Now()
			var result *httpResult
			if mode == "parallel" {
				result, err = strategy.HttpParallel(request)
			} else {
				result, err = strategy.HttpSerial(request, hello)
			}
			if err != nil || result == nil || string(result.bodyBytes) != "ok" {
				t.Fatalf("%s: result=%v err=%v", mode, result, err)
			}
			if elapsed := time.Since(started); elapsed != 0 {
				t.Errorf("%s: ready replacement waited %s for reconnect", mode, elapsed)
			}
			if pools := oldPools.Load(); pools != 0 {
				t.Errorf("%s: old snapshot created %d pools", mode, pools)
			}
		})
	}
}

// Even a changed generation cannot turn an ordinary network error into an
// immediate retry. Unchanged and replaced routes retain the original wait.
func TestClientStrategyOrdinaryFailureKeepsReconnect(t *testing.T) {
	for _, replace := range []bool{false, true} {
		for _, mode := range []string{"parallel", "hello"} {
			synctest.Test(t, func(t *testing.T) {
				strategy, dialer := newRetirementTestStrategy(t)
				strategy.settings.RequestTimeout = time.Minute
				strategy.settings.ReconnectTimeout = 15 * time.Second
				strategy.reconnectFactory = NewPacedReconnect
				var attempts atomic.Int32
				transport := &retirementTestTransport{roundTrip: func(request *http.Request) (*http.Response, error) {
					if attempts.Add(1) == 1 {
						if replace {
							strategy.SetVlessConfigs(strategy.VlessConfigs())
							for current := range strategy.dialers {
								current.httpClientFactory = func() *http.Client { return retirementTestClient(&retirementTestTransport{}) }
							}
						}
						return nil, io.ErrUnexpectedEOF
					}
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Request: request}, nil
				}}
				dialer.httpClientFactory = func() *http.Client { return retirementTestClient(transport) }
				request := newClientStrategyLifecycleRequest(t, t.Context(), "https://api.example/request")
				hello := newClientStrategyLifecycleRequest(t, t.Context(), "https://api.example/hello")
				started := time.Now()
				var err error
				if mode == "parallel" {
					_, err = strategy.HttpParallel(request)
				} else {
					_, err = strategy.HttpSerial(request, hello)
				}
				if err != nil {
					t.Fatalf("%s replace=%t: %v", mode, replace, err)
				}
				if elapsed := time.Since(started); elapsed != 15*time.Second {
					t.Fatalf("%s replace=%t: network failure retry = %s, want 15s", mode, replace, elapsed)
				}
			})
		}
	}
}

// After one immediate refresh, every further replacement waits for reconnect.
// A canceled request bounds even a configuration that changes on every clone.
func TestClientStrategyRetirementRefreshCannotSpin(t *testing.T) {
	for _, mode := range []string{"parallel", "serial", "hello"} {
		synctest.Test(t, func(t *testing.T) {
			strategy, dialer := newRetirementTestStrategy(t)
			strategy.settings.RequestTimeout = time.Minute
			strategy.settings.ReconnectTimeout = 10 * time.Second
			strategy.reconnectFactory = NewPacedReconnect
			if mode == "serial" {
				strategy.RecordDeliveryOutcome(strategy.dialerInfo(dialer), deliveryVerifiedByteCount, false)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.example/request", strings.NewReader("input"))
			if err != nil {
				t.Fatal(err)
			}
			hello, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.example/hello", strings.NewReader("input"))
			if err != nil {
				t.Fatal(err)
			}
			var clones atomic.Int32
			clone := func() (io.ReadCloser, error) {
				clones.Add(1)
				strategy.SetVlessConfigs(strategy.VlessConfigs())
				return io.NopCloser(strings.NewReader("input")), nil
			}
			request.GetBody, hello.GetBody = clone, clone
			started := time.Now()
			if mode == "parallel" {
				_, err = strategy.HttpParallel(request)
			} else {
				_, err = strategy.HttpSerial(request, hello)
			}
			if err == nil {
				t.Fatal("perpetually retired snapshots unexpectedly succeeded")
			}
			if elapsed := time.Since(started); elapsed != 25*time.Second {
				t.Fatalf("%s: caller cancellation took %s", mode, elapsed)
			}
			want := int32(4)
			if mode == "serial" {
				want++
			}
			if got := clones.Load(); got != want {
				t.Fatalf("%s: replacement attempts = %d, want %d", mode, got, want)
			}
		})
	}
}

// A successful hello must not reset the outer evaluation's refresh allowance.
// Replacing only data attempts otherwise bypasses the hello reconnect wait.
func TestClientStrategyRetirementRefreshDataOnlyCannotSpin(t *testing.T) {
	for _, preferred := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			strategy, dialer := newRetirementTestStrategy(t)
			strategy.settings.RequestTimeout = time.Minute
			strategy.settings.ReconnectTimeout = 10 * time.Second
			strategy.reconnectFactory = NewPacedReconnect
			if preferred {
				strategy.RecordDeliveryOutcome(strategy.dialerInfo(dialer), deliveryVerifiedByteCount, false)
			}
			installClients := func() {
				for current := range strategy.dialers {
					current.httpClientFactory = func() *http.Client { return retirementTestClient(&retirementTestTransport{}) }
				}
			}
			installClients()
			ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.example/request", strings.NewReader("input"))
			if err != nil {
				t.Fatal(err)
			}
			hello := newClientStrategyLifecycleRequest(t, ctx, "https://api.example/hello")
			var clones atomic.Int32
			request.GetBody = func() (io.ReadCloser, error) {
				// A spin cannot advance fake time; the guard makes the old
				// failure terminate deterministically rather than hang the test.
				if clones.Add(1) > 8 {
					cancel()
				}
				strategy.SetVlessConfigs(strategy.VlessConfigs())
				installClients()
				return io.NopCloser(strings.NewReader("input")), nil
			}
			started := time.Now()
			_, err = strategy.HttpSerial(request, hello)
			if err == nil || ctx.Err() != context.DeadlineExceeded {
				t.Fatalf("preferred=%t: data-only churn err=%v cancellation=%v clones=%d", preferred, err, ctx.Err(), clones.Load())
			}
			if elapsed := time.Since(started); elapsed != 25*time.Second {
				t.Fatalf("preferred=%t: cancellation took %s", preferred, elapsed)
			}
			if got := clones.Load(); got != 4 {
				t.Fatalf("preferred=%t: data replacement attempts = %d, want 4", preferred, got)
			}
		})
	}
}

// Final shutdown has no ready successor. It cancels the request immediately
// and neither the retained snapshot nor a later update may revive a pool.
func TestClientStrategyClosedSnapshotCannotRefresh(t *testing.T) {
	for _, mode := range []string{"parallel", "serial", "hello"} {
		synctest.Test(t, func(t *testing.T) {
			strategy, dialer := newRetirementTestStrategy(t)
			strategy.settings.RequestTimeout = time.Minute
			strategy.settings.ReconnectTimeout = 15 * time.Second
			strategy.reconnectFactory = NewPacedReconnect
			if mode == "serial" {
				strategy.RecordDeliveryOutcome(strategy.dialerInfo(dialer), deliveryVerifiedByteCount, false)
			}
			var pools atomic.Int32
			dialer.httpClientFactory = func() *http.Client { pools.Add(1); return retirementTestClient(&retirementTestTransport{}) }
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://api.example/request", strings.NewReader("input"))
			if err != nil {
				t.Fatal(err)
			}
			hello, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.example/hello", strings.NewReader("input"))
			if err != nil {
				t.Fatal(err)
			}
			clone := func() (io.ReadCloser, error) { strategy.Close(); return io.NopCloser(strings.NewReader("input")), nil }
			request.GetBody, hello.GetBody = clone, clone
			started := time.Now()
			if mode == "parallel" {
				_, err = strategy.HttpParallel(request)
			} else {
				_, err = strategy.HttpSerial(request, hello)
			}
			if err == nil || time.Since(started) != 0 {
				t.Fatalf("%s: closed snapshot err=%v elapsed=%s", mode, err, time.Since(started))
			}
			generation := strategy.dialerGeneration
			strategy.SetVlessConfigs(strategy.VlessConfigs())
			if pools.Load() != 0 || len(strategy.dialerWeights(false)) != 0 || generation != strategy.dialerGeneration {
				t.Fatalf("%s: terminal strategy revived a route", mode)
			}
		})
	}
}

// Every published membership transition advances the generation, including
// discovery additions, pruning, country replacement, and terminal shutdown.
// Repeating a no-op must not invent a ready successor for an old snapshot.
func TestClientStrategyDialerGenerationTracksMembership(t *testing.T) {
	strategy, dialer := newRetirementTestStrategy(t)
	config := dialer.vlessConfig
	check := func(name string, delta uint64, change func()) {
		before := strategy.dialerGeneration
		change()
		if got := strategy.dialerGeneration - before; got != delta {
			t.Fatalf("%s: generation advanced %d, want %d", name, got, delta)
		}
	}
	check("replace vless", 1, func() { strategy.SetVlessConfigs([]*VlessConfig{config}) })
	check("remove vless", 1, func() { strategy.SetVlessConfigs(nil) })
	check("empty vless", 0, func() { strategy.SetVlessConfigs(nil) })
	custom := map[netip.Addr]string{netip.MustParseAddr("192.0.2.19"): "synthetic-secret"}
	check("configuration before discovery", 0, func() { strategy.SetCustomExtenders(custom) })
	strategy.settings.ExpandExtenderProfileCount = 1
	expand := func() {
		if got := len(strategy.expandExtenderDialers()); got != 1 {
			t.Fatalf("expanded %d routes, want 1", got)
		}
	}
	check("discovery add", 1, expand)
	strategy.settings.ExtenderDropTimeout = 0
	check("prune", 1, strategy.collapseExtenderDialers)
	check("rediscovery add", 1, expand)
	check("custom removal", 1, func() { strategy.SetCustomExtenders(nil) })
	check("custom update before discovery", 0, func() { strategy.SetCustomExtenders(custom) })
	check("custom discovery add", 1, expand)
	strategy.extenderSpoofCountryCode = "old-test-country"
	check("country removal and add", 2, expand)
	check("close", 1, strategy.Close)
	check("repeated close", 0, strategy.Close)
	check("closed vless update", 0, func() { strategy.SetVlessConfigs([]*VlessConfig{config}) })
	check("closed custom update", 0, func() { strategy.SetCustomExtenders(nil) })
	check("closed discovery", 0, func() { strategy.expandExtenderDialers() })
}
