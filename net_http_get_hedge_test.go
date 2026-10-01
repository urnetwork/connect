package connect

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func newGetHedgeTestStrategy(ctx context.Context, settings *ClientStrategySettings) *ClientStrategy {
	return &ClientStrategy{
		ctx: ctx, log: NewNoopLogger(), settings: settings,
		dialers: map[*clientDialer]bool{}, extenderIpSecrets: map[netip.Addr]string{},
	}
}

func addGetHedgeTestRoute(strategy *ClientStrategy, preferred bool, priority int, roundTrip func(*http.Request) (*http.Response, error)) *clientDialer {
	dialer := &clientDialer{
		minimumWeight: 1, priority: priority, settings: strategy.settings,
		httpClient: &http.Client{Transport: serialTestRoundTripper(roundTrip)},
	}
	if preferred {
		dialer.successCount = 1
		dialer.lastSuccessTime = time.Now()
	}
	strategy.dialers[dialer] = true
	return dialer
}

func getHedgeTestResponse(request *http.Request) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{},
		Body: io.NopCloser(strings.NewReader("fixture")), Request: request}
}

func getHedgeTestWritten(request *http.Request) {
	if trace := httptrace.ContextClientTrace(request.Context()); trace != nil && trace.WroteRequest != nil {
		trace.WroteRequest(httptrace.WroteRequestInfo{})
	}
}

// Default settings reproduced a 7.5s preferred establishment wait before the
// same immediate fallback. A GET head start makes progress at 1s without
// canceling or penalizing the losing route. POST and disabled policy retain
// their existing budget and route order.
func TestHttpGetStalePreferredRouteAllowsEarlyFallback(t *testing.T) {
	for _, test := range []struct {
		name, method       string
		delay              time.Duration
		wantElapsed        time.Duration
		wantStale          bool
		callerBudget       time.Duration
		alternatePreferred bool
	}{
		{"GET", http.MethodGet, time.Second, time.Second, false, 0, false},
		{"GET short caller budget", http.MethodGet, time.Second, 250 * time.Millisecond, false, 500 * time.Millisecond, false},
		{"GET short budget multiple preferred", http.MethodGet, time.Second, 500 * time.Millisecond / 3, false, 500 * time.Millisecond, true},
		{"GET head start exceeds deadline", http.MethodGet, time.Minute, 7500 * time.Millisecond, false, 0, false},
		{"GET disabled", http.MethodGet, 0, 7500 * time.Millisecond, true, 0, false},
		{"POST unchanged", http.MethodPost, time.Second, 7500 * time.Millisecond, true, 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if test.callerBudget > 0 {
					var deadlineCancel context.CancelFunc
					ctx, deadlineCancel = context.WithTimeout(ctx, test.callerBudget)
					defer deadlineCancel()
				}
				settings := DefaultClientStrategySettings()
				settings.GetPreferredRouteHedgeDelay = test.delay
				strategy := newGetHedgeTestStrategy(ctx, settings)
				var calls, active atomic.Int32
				preferred := addGetHedgeTestRoute(strategy, true, 0, func(request *http.Request) (*http.Response, error) {
					calls.Add(1)
					active.Add(1)
					defer active.Add(-1)
					<-request.Context().Done()
					return nil, request.Context().Err()
				})
				addGetHedgeTestRoute(strategy, test.alternatePreferred, 1, func(request *http.Request) (*http.Response, error) {
					calls.Add(1)
					getHedgeTestWritten(request)
					return getHedgeTestResponse(request), nil
				})
				request, err := http.NewRequestWithContext(ctx, test.method, "https://api.example/network/provider-locations", nil)
				if err != nil {
					t.Fatal(err)
				}
				began := time.Now()
				result, err := strategy.HttpParallel(request)
				if err != nil || result == nil || string(result.bodyBytes) != "fixture" {
					t.Fatalf("fallback result=%v err=%v", result, err)
				}
				if time.Since(began) != test.wantElapsed || calls.Load() != 2 || active.Load() != 0 {
					t.Fatalf("elapsed=%s want=%s calls=%d active=%d", time.Since(began), test.wantElapsed, calls.Load(), active.Load())
				}
				if preferred.IsLastSuccess() == test.wantStale {
					t.Fatalf("preferred stale=%t want=%t", !preferred.IsLastSuccess(), test.wantStale)
				}
			})
		})
	}
}

// A congested working handshake must survive the early fallback boundary. The
// alternative fails; the original connection still delivers its slow response.
func TestHttpGetEarlyFallbackRetainsSlowWorkingPreferredRoute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		settings := DefaultClientStrategySettings()
		strategy := newGetHedgeTestStrategy(ctx, settings)
		var calls atomic.Int32
		preferred := addGetHedgeTestRoute(strategy, true, 0, func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			select {
			case <-time.After(3 * time.Second):
			case <-request.Context().Done():
				return nil, request.Context().Err()
			}
			getHedgeTestWritten(request)
			select {
			case <-time.After(6 * time.Second):
			case <-request.Context().Done():
				return nil, request.Context().Err()
			}
			return getHedgeTestResponse(request), nil
		})
		addGetHedgeTestRoute(strategy, false, 1, func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			return nil, errors.New("synthetic unavailable alternative")
		})
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.example/network/provider-locations", nil)
		if err != nil {
			t.Fatal(err)
		}
		began := time.Now()
		result, err := strategy.HttpParallel(request)
		if err != nil || result == nil || time.Since(began) != 9*time.Second || calls.Load() != 2 || !preferred.IsLastSuccess() {
			t.Fatalf("result=%v err=%v elapsed=%s calls=%d preferred=%t", result, err, time.Since(began), calls.Load(), preferred.IsLastSuccess())
		}
	})
}

func TestHttpGetWrittenPreferredResponseDoesNotHedge(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		settings := DefaultClientStrategySettings()
		strategy := newGetHedgeTestStrategy(ctx, settings)
		var calls, traceWrites atomic.Int32
		ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) { traceWrites.Add(1) }})
		addGetHedgeTestRoute(strategy, true, 0, func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			getHedgeTestWritten(request)
			select {
			case <-time.After(9 * time.Second):
			case <-request.Context().Done():
				return nil, request.Context().Err()
			}
			return getHedgeTestResponse(request), nil
		})
		addGetHedgeTestRoute(strategy, false, 1, func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			return getHedgeTestResponse(request), nil
		})
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.example/network/provider-locations", nil)
		if err != nil {
			t.Fatal(err)
		}
		began := time.Now()
		result, err := strategy.HttpParallel(request)
		if err != nil || result == nil || time.Since(began) != 9*time.Second || calls.Load() != 1 || traceWrites.Load() != 1 {
			t.Fatalf("result=%v err=%v elapsed=%s calls=%d traceWrites=%d", result, err, time.Since(began), calls.Load(), traceWrites.Load())
		}
	})
}

// The shorter caller budget caps only the unwritten head start. A request
// written before that boundary keeps the caller's full response allowance.
func TestHttpGetShortCallerBudgetRetainsWrittenResponse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		strategy := newGetHedgeTestStrategy(ctx, DefaultClientStrategySettings())
		var calls atomic.Int32
		addGetHedgeTestRoute(strategy, true, 0, func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			getHedgeTestWritten(request)
			select {
			case <-time.After(400 * time.Millisecond):
			case <-request.Context().Done():
				return nil, request.Context().Err()
			}
			return getHedgeTestResponse(request), nil
		})
		addGetHedgeTestRoute(strategy, true, 1, func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			return getHedgeTestResponse(request), nil
		})
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.example/network/provider-locations", nil)
		if err != nil {
			t.Fatal(err)
		}
		began := time.Now()
		result, err := strategy.HttpParallel(request)
		if err != nil || result == nil || calls.Load() != 1 || time.Since(began) != 400*time.Millisecond {
			t.Fatalf("result=%v err=%v calls=%d elapsed=%s", result, err, calls.Load(), time.Since(began))
		}
	})
}

func TestHttpGetEarlyFallbackKeepsParallelBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		settings := DefaultClientStrategySettings()
		settings.ParallelBlockSize = 2
		strategy := newGetHedgeTestStrategy(ctx, settings)
		var active, maximum, calls atomic.Int32
		for route := range 8 {
			addGetHedgeTestRoute(strategy, route == 0, route, func(request *http.Request) (*http.Response, error) {
				calls.Add(1)
				n := active.Add(1)
				defer active.Add(-1)
				for old := maximum.Load(); n > old && !maximum.CompareAndSwap(old, n); old = maximum.Load() {
				}
				if route == 0 {
					<-request.Context().Done()
					return nil, request.Context().Err()
				}
				select {
				case <-time.After(2 * time.Second):
				case <-request.Context().Done():
					return nil, request.Context().Err()
				}
				getHedgeTestWritten(request)
				return getHedgeTestResponse(request), nil
			})
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.example/network/provider-locations", nil)
		if err != nil {
			t.Fatal(err)
		}
		began := time.Now()
		result, err := strategy.HttpParallel(request)
		if err != nil || result == nil || time.Since(began) != 3*time.Second || maximum.Load() != 2 || active.Load() != 0 || calls.Load() != 2 {
			t.Fatalf("result=%v err=%v elapsed=%s max=%d active=%d calls=%d", result, err, time.Since(began), maximum.Load(), active.Load(), calls.Load())
		}
	})
}

func TestHttpGetCancellationBeforeHedgeJoinsPreferred(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cancelTimer := time.AfterFunc(500*time.Millisecond, cancel)
		defer cancelTimer.Stop()
		settings := DefaultClientStrategySettings()
		strategy := newGetHedgeTestStrategy(ctx, settings)
		var calls, active atomic.Int32
		for route := range 2 {
			addGetHedgeTestRoute(strategy, route == 0, route, func(request *http.Request) (*http.Response, error) {
				calls.Add(1)
				active.Add(1)
				defer active.Add(-1)
				<-request.Context().Done()
				return nil, request.Context().Err()
			})
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.example/network/provider-locations", nil)
		if err != nil {
			t.Fatal(err)
		}
		began := time.Now()
		result, err := strategy.HttpParallel(request)
		if result != nil || !errors.Is(err, context.Canceled) || time.Since(began) != 500*time.Millisecond || calls.Load() != 1 || active.Load() != 0 {
			t.Fatalf("result=%v err=%v elapsed=%s calls=%d active=%d", result, err, time.Since(began), calls.Load(), active.Load())
		}
	})
}
