package connect

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// Every request stays inside a synthetic RoundTripper. Marking a route known
// controls only whether the real strategy first enters its hello discovery.
func authObservationTestApi(ctx context.Context, counts *AuthNetworkClientObservations, known bool, transport http.RoundTripper) (*BringYourApi, *ClientStrategy) {
	ctx = WithAuthNetworkClientObservations(ctx, counts)
	settings := DefaultClientStrategySettings()
	settings.RequestTimeout = 15 * time.Second
	dialer := &clientDialer{settings: settings, minimumWeight: 1, httpClient: &http.Client{Transport: transport}}
	if known {
		dialer.Update(ctx, nil)
	}
	strategy := &ClientStrategy{
		ctx: ctx, log: NewNoopLogger(), settings: settings,
		dialers: map[*clientDialer]bool{dialer: true}, extenderIpSecrets: map[netip.Addr]string{},
	}
	return NewBringYourApi(ctx, strategy, "https://api.auth.example"), strategy
}

func authObservationTestWrote(request *http.Request) {
	if trace := httptrace.ContextClientTrace(request.Context()); trace != nil && trace.WroteRequest != nil {
		trace.WroteRequest(httptrace.WroteRequestInfo{})
	}
}

func authObservationTestResponse(request *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: request}
}

func requireAuthObservation(t *testing.T, counts *AuthNetworkClientObservations, phase, result string) {
	t.Helper()
	values := counts.Snapshot()
	if len(values) != 36 {
		t.Fatalf("observation has %d cells, want 36", len(values))
	}
	var total, matched uint64
	for _, value := range values {
		total += value.Count
		if value.Phase == phase && value.Result == result {
			matched += value.Count
		}
	}
	if total != 1 || matched != 1 {
		t.Fatalf("logical request total=%d, %s/%s=%d, want exactly one", total, phase, result, matched)
	}
}

// Hello traffic, even after its request write, cannot manufacture a POST.
func TestAuthObservationNoPostWhileHelloTimesOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		counts := &AuthNetworkClientObservations{}
		api, _ := authObservationTestApi(t.Context(), counts, false, serialTestRoundTripper(func(request *http.Request) (*http.Response, error) {
			if request.Method != http.MethodGet {
				t.Error("auth POST escaped stalled discovery")
			}
			authObservationTestWrote(request)
			<-request.Context().Done()
			return nil, request.Context().Err()
		}))
		defer api.Close()
		_, err := api.AuthNetworkClientSync(&AuthNetworkClientArgs{})
		if err == nil {
			t.Fatal("stalled discovery unexpectedly succeeded")
		}
		requireAuthObservation(t, counts, "no_post", "timeout")
	})
}

// A successful hello does not count as writing the later auth request.
func TestAuthObservationPreWriteIgnoresHelloProgress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		counts := &AuthNetworkClientObservations{}
		posts := 0
		api, _ := authObservationTestApi(t.Context(), counts, false, serialTestRoundTripper(func(request *http.Request) (*http.Response, error) {
			if request.Method == http.MethodGet && posts == 0 {
				authObservationTestWrote(request)
				return authObservationTestResponse(request, http.StatusOK, `{}`), nil
			}
			if request.Method == http.MethodPost {
				posts++
			}
			<-request.Context().Done()
			return nil, request.Context().Err()
		}))
		defer api.Close()
		_, err := api.AuthNetworkClientSync(&AuthNetworkClientArgs{})
		if err == nil || posts != 1 {
			t.Fatalf("stalled POST err=%v posts=%d", err, posts)
		}
		requireAuthObservation(t, counts, "pre_write", "timeout")
	})
}

func TestAuthObservationResponseWaitRetainsWholeRequestBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		counts := &AuthNetworkClientObservations{}
		api, _ := authObservationTestApi(t.Context(), counts, true, serialTestRoundTripper(func(request *http.Request) (*http.Response, error) {
			authObservationTestWrote(request)
			<-request.Context().Done()
			return nil, request.Context().Err()
		}))
		defer api.Close()
		started := time.Now()
		_, err := api.AuthNetworkClientSync(&AuthNetworkClientArgs{})
		if err == nil || time.Since(started) != 15*time.Second {
			t.Fatalf("observation changed the response deadline: err=%v elapsed=%s", err, time.Since(started))
		}
		requireAuthObservation(t, counts, "response_wait", "timeout")
	})
}

type authObservationWaitingBody struct {
	ctx context.Context
}

func (self authObservationWaitingBody) Read([]byte) (int, error) {
	<-self.ctx.Done()
	return 0, self.ctx.Err()
}

func (self authObservationWaitingBody) Close() error { return nil }

func TestAuthObservationBodyReadDeadlineIsNotResponseWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		counts := &AuthNetworkClientObservations{}
		api, _ := authObservationTestApi(t.Context(), counts, true, serialTestRoundTripper(func(request *http.Request) (*http.Response, error) {
			authObservationTestWrote(request)
			response := authObservationTestResponse(request, http.StatusOK, "")
			response.Body = authObservationWaitingBody{ctx: request.Context()}
			return response, nil
		}))
		defer api.Close()
		_, err := api.AuthNetworkClientSync(&AuthNetworkClientArgs{})
		if err == nil {
			t.Fatal("stalled body unexpectedly succeeded")
		}
		requireAuthObservation(t, counts, "body_read", "timeout")
	})
}

// The metric classifies the last POST, not the furthest earlier attempt. A
// late write trace on the old request must not advance the current request.
func TestAuthObservationLastPostOwnsItsProgress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		counts := &AuthNetworkClientObservations{}
		var first *http.Request
		api, strategy := authObservationTestApi(ctx, counts, true, serialTestRoundTripper(func(request *http.Request) (*http.Response, error) {
			first = request
			authObservationTestWrote(request)
			response := authObservationTestResponse(request, http.StatusOK, "")
			response.Body = &serialTestReadErrorCloseRecorder{err: errors.New("synthetic body error")}
			return response, nil
		}))
		defer api.Close()
		second := &clientDialer{
			settings: strategy.settings, priority: 1, minimumWeight: 1,
			httpClient: &http.Client{Transport: serialTestRoundTripper(func(request *http.Request) (*http.Response, error) {
				authObservationTestWrote(first)
				cancel()
				return nil, context.Canceled
			})},
		}
		second.Update(ctx, nil)
		strategy.dialers[second] = true
		_, err := api.AuthNetworkClientSync(&AuthNetworkClientArgs{})
		if err == nil || first == nil {
			t.Fatal("fixture did not exercise both auth attempts")
		}
		requireAuthObservation(t, counts, "pre_write", "canceled")
	})
}

// HTTP and JSON/application outcomes do not become success merely because
// bytes arrived; status/error strings are never copied into metric labels.
func TestAuthObservationClassifiesResponseOutcomes(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{http.StatusOK, `{"by_client_jwt":"synthetic-token"}`, "ok"},
		{http.StatusUnauthorized, "synthetic authentication error", "http_auth"},
		{http.StatusForbidden, "synthetic authorization error", "http_auth"},
		{http.StatusTooManyRequests, "synthetic rate error", "http_rate"},
		{http.StatusServiceUnavailable, "synthetic backend error", "http_error"},
		{http.StatusOK, `{"error":{"message":"synthetic application error"}}`, "api_error"},
		{http.StatusOK, `not-json`, "decode_error"},
		{http.StatusOK, `{"by_client_jwt":123}`, "decode_error"},
	}
	for _, test := range cases {
		counts := &AuthNetworkClientObservations{}
		api, _ := authObservationTestApi(t.Context(), counts, true, serialTestRoundTripper(func(request *http.Request) (*http.Response, error) {
			// A positive response establishes body_read even when an alternate
			// transport has no WroteRequest callback.
			return authObservationTestResponse(request, test.status, test.body), nil
		}))
		api.AuthNetworkClientSyncWithCtx(t.Context(), &AuthNetworkClientArgs{})
		api.Close()
		requireAuthObservation(t, counts, "body_read", test.want)
	}
}

func TestAuthObservationNilAndConcurrentCollectorsStayBounded(t *testing.T) {
	var absent *AuthNetworkClientObservations
	absent.record(authNoPost, authTimeout)
	ctx := t.Context()
	if WithAuthNetworkClientObservations(ctx, nil) != ctx {
		t.Fatal("nil observer changed the caller context")
	}
	counts := &AuthNetworkClientObservations{}
	var workers sync.WaitGroup
	for range 10 {
		workers.Go(func() {
			for range 100 {
				counts.record(authPreWrite, authCanceled)
			}
		})
	}
	workers.Wait()
	counts.record(authRequestPhase(-1), authTimeout)
	counts.record(authNoPost, authRequestResult(99))
	var total uint64
	for _, value := range counts.Snapshot() {
		total += value.Count
	}
	if total != 1000 || len(absent.Snapshot()) != 36 {
		t.Fatal("collector lost counts or exceeded its fixed vocabulary")
	}
}

// Failed writes and caller cancellation are not evidence that the server
// received a complete POST. An unrelated default API stays uninstrumented.
func TestAuthObservationFailedWriteAndDefaultAreUnchanged(t *testing.T) {
	for _, counts := range []*AuthNetworkClientObservations{new(AuthNetworkClientObservations), nil} {
		ctx, cancel := context.WithCancel(t.Context())
		posts := 0
		api, _ := authObservationTestApi(ctx, counts, true, serialTestRoundTripper(func(request *http.Request) (*http.Response, error) {
			posts++
			if trace := httptrace.ContextClientTrace(request.Context()); trace != nil && trace.WroteRequest != nil {
				trace.WroteRequest(httptrace.WroteRequestInfo{Err: errors.New("synthetic write error")})
			}
			cancel()
			return nil, context.Canceled
		}))
		_, err := api.AuthNetworkClientSync(&AuthNetworkClientArgs{})
		api.Close()
		cancel()
		if err == nil || posts != 1 {
			t.Fatal("observation changed the canceled transport result or attempt count")
		}
		if counts != nil {
			requireAuthObservation(t, counts, "pre_write", "canceled")
		}
	}
}
