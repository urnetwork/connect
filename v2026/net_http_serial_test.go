package connect

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// Provides a transport seam for exact request-attempt assertions.
type serialTestRoundTripper func(request *http.Request) (*http.Response, error)

func (self serialTestRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return self(request)
}

// No outcome is not a successful outcome. Treating every cold dialer as a
// previous success serializes first-use discovery and lets one black hole hide
// all of the healthy paths that should be probed in parallel.
func TestClientDialerWithoutOutcomeIsNotLastSuccess(t *testing.T) {
	dialer := &clientDialer{}
	if dialer.IsLastSuccess() {
		t.Fatal("new dialer was classified as a previously successful route")
	}
}

// Discovery cache maintenance retains a route whose latest outcome worked and
// expires only a failed route whose error has aged past the configured window.
func TestCollapseExtenderDialersRetainsSuccessAndExpiresFailure(t *testing.T) {
	settings := DefaultClientStrategySettings()
	settings.ExtenderDropTimeout = time.Minute
	now := time.Now()
	healthyDialer := &clientDialer{
		extenderConfig:  &ExtenderConfig{},
		successCount:    1,
		lastSuccessTime: now,
		settings:        settings,
	}
	expiredFailedDialer := &clientDialer{
		extenderConfig:  &ExtenderConfig{},
		successCount:    1,
		errorCount:      1,
		lastSuccessTime: now.Add(-2 * time.Minute),
		lastErrorTime:   now.Add(-time.Minute - time.Second),
		settings:        settings,
	}
	recentFailedDialer := &clientDialer{
		extenderConfig: &ExtenderConfig{},
		errorCount:     1,
		lastErrorTime:  now,
		settings:       settings,
	}
	strategy := &ClientStrategy{
		settings: settings,
		dialers: map[*clientDialer]bool{
			healthyDialer:       true,
			expiredFailedDialer: true,
			recentFailedDialer:  true,
		},
	}

	strategy.collapseExtenderDialers()

	if !strategy.dialers[healthyDialer] {
		t.Fatal("healthy discovered extender was removed")
	}
	if strategy.dialers[expiredFailedDialer] {
		t.Fatal("expired failed extender was retained")
	}
	if !strategy.dialers[recentFailedDialer] {
		t.Fatal("recently failed extender was removed before its drop timeout")
	}
}

// A route that worked previously can become a black hole. Its next POST must
// not inherit the whole request deadline, because doing so prevents every
// other proven route from being attempted.
func TestSerialEvalReservesRequestBudgetFromStalePreferredDialer(t *testing.T) {
	strategyCtx, strategyCancel := context.WithCancel(context.Background())
	defer strategyCancel()

	settings := DefaultClientStrategySettings()
	settings.RequestTimeout = 30 * time.Minute
	now := time.Now()
	staleDialer := &clientDialer{
		description:     "stale",
		minimumWeight:   1,
		priority:        0,
		successCount:    1,
		lastSuccessTime: now,
		settings:        settings,
	}
	healthyDialer := &clientDialer{
		description:     "healthy",
		minimumWeight:   1,
		priority:        1,
		successCount:    1,
		lastSuccessTime: now,
		settings:        settings,
	}
	strategy := &ClientStrategy{
		ctx:               strategyCtx,
		log:               loggerOrDefault(nil),
		settings:          settings,
		dialers:           map[*clientDialer]bool{staleDialer: true, healthyDialer: true},
		extenderIpSecrets: map[netip.Addr]string{},
	}

	var staleAttemptBudget time.Duration
	healthyAttempted := false
	eval := func(evalCtx context.Context, dialer *clientDialer) *evalResult {
		if dialer == staleDialer {
			deadline, ok := evalCtx.Deadline()
			if !ok {
				t.Fatal("stale route evaluation has no deadline")
			}
			staleAttemptBudget = time.Until(deadline)
			return &evalResult{err: errors.New("stale route")}
		}
		healthyAttempted = true
		return &evalResult{}
	}
	helloEval := func(context.Context, *clientDialer) *evalResult {
		t.Fatal("healthy proven route should avoid the hello fallback")
		return nil
	}

	result := strategy.serialEval(context.Background(), eval, helloEval)
	if result == nil || result.err != nil {
		t.Fatalf("healthy fallback result = %#v", result)
	}
	if !healthyAttempted {
		t.Fatal("healthy fallback route was not attempted")
	}
	maximumAttemptBudget := settings.RequestTimeout/3 + time.Second
	if maximumAttemptBudget < staleAttemptBudget {
		t.Fatalf(
			"stale preferred route received %s of a %s request budget",
			staleAttemptBudget,
			settings.RequestTimeout,
		)
	}
}

// Preferred GET/WebSocket routes use the same synchronous fast path before
// the parallel block. It needs the same deadline reservation or one stale
// route can prevent the parallel candidates from ever starting.
func TestParallelEvalReservesRequestBudgetFromStalePreferredDialer(t *testing.T) {
	strategyCtx, strategyCancel := context.WithCancel(context.Background())
	defer strategyCancel()

	settings := DefaultClientStrategySettings()
	settings.RequestTimeout = 30 * time.Minute
	now := time.Now()
	staleDialer := &clientDialer{
		description:     "stale",
		minimumWeight:   1,
		priority:        0,
		successCount:    1,
		lastSuccessTime: now,
		settings:        settings,
	}
	healthyDialer := &clientDialer{
		description:     "healthy",
		minimumWeight:   1,
		priority:        1,
		successCount:    1,
		lastSuccessTime: now,
		settings:        settings,
	}
	strategy := &ClientStrategy{
		ctx:               strategyCtx,
		log:               loggerOrDefault(nil),
		settings:          settings,
		dialers:           map[*clientDialer]bool{staleDialer: true, healthyDialer: true},
		extenderIpSecrets: map[netip.Addr]string{},
	}

	var staleAttemptBudget time.Duration
	eval := func(evalCtx context.Context, dialer *clientDialer) *evalResult {
		if dialer == staleDialer {
			deadline, ok := evalCtx.Deadline()
			if !ok {
				t.Fatal("stale route evaluation has no deadline")
			}
			staleAttemptBudget = time.Until(deadline)
			return &evalResult{err: errors.New("stale route")}
		}
		return &evalResult{}
	}

	result := strategy.parallelEval(context.Background(), eval)
	if result == nil || result.err != nil {
		t.Fatalf("healthy fallback result = %#v", result)
	}
	if result.dialer != healthyDialer {
		t.Fatalf("selected dialer = %v, expected healthy route", result.dialer)
	}
	maximumAttemptBudget := settings.RequestTimeout/3 + time.Second
	if maximumAttemptBudget < staleAttemptBudget {
		t.Fatalf(
			"stale preferred route received %s of a %s request budget",
			staleAttemptBudget,
			settings.RequestTimeout,
		)
	}
}

// Cancellation asks every parallel attempt to stop, but completion remains
// owned until each attempt stack has actually returned. Otherwise a caller can
// release the strategy or its transport while a dial still uses their state.
func TestParallelEvalCancellationJoinsAttemptWorker(t *testing.T) {
	testCtx, testCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer testCancel()

	strategyCtx, strategyCancel := context.WithCancel(context.Background())
	defer strategyCancel()
	settings := DefaultClientStrategySettings()
	settings.RequestTimeout = 30 * time.Minute
	settings.ParallelBlockSize = 1
	dialer := &clientDialer{
		description:   "blocked",
		minimumWeight: 1,
		settings:      settings,
	}
	strategy := &ClientStrategy{
		ctx:               strategyCtx,
		log:               loggerOrDefault(nil),
		settings:          settings,
		dialers:           map[*clientDialer]bool{dialer: true},
		extenderIpSecrets: map[netip.Addr]string{},
	}

	attemptEntered := make(chan struct{})
	attemptCanceled := make(chan struct{})
	releaseAttempt := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(releaseAttempt)
		})
	}
	t.Cleanup(release)

	requestCtx, requestCancel := context.WithCancel(context.Background())
	defer requestCancel()
	result := make(chan *evalResult, 1)
	go func() {
		result <- strategy.parallelEval(requestCtx, func(evalCtx context.Context, _ *clientDialer) *evalResult {
			close(attemptEntered)
			<-evalCtx.Done()
			close(attemptCanceled)
			<-releaseAttempt
			return &evalResult{err: evalCtx.Err()}
		})
	}()

	select {
	case <-testCtx.Done():
		t.Fatalf("wait for parallel attempt: %v", testCtx.Err())
	case <-attemptEntered:
	}
	requestCancel()
	select {
	case <-testCtx.Done():
		t.Fatalf("wait for parallel attempt cancellation: %v", testCtx.Err())
	case <-attemptCanceled:
	}
	select {
	case <-result:
		t.Fatal("parallel evaluation returned before its canceled attempt unwound")
	default:
	}

	release()
	select {
	case <-testCtx.Done():
		t.Fatalf("join parallel attempt: %v", testCtx.Err())
	case parallelResult := <-result:
		if parallelResult != nil {
			t.Fatalf("canceled parallel evaluation result = %#v", parallelResult)
		}
	}
}

// A failed route may consume the complete POST body before another proven
// route is tried. Every route attempt must receive a fresh body reader.
func TestHttpSerialReplaysCompletePostBodyAfterRouteFailure(t *testing.T) {
	strategyCtx, strategyCancel := context.WithCancel(context.Background())
	defer strategyCancel()

	settings := DefaultClientStrategySettings()
	settings.RequestTimeout = time.Second
	now := time.Now()
	requestBodyBytes := []byte(`{"user_auth":"acceptance@example.invalid"}`)
	var firstBodyBytes []byte
	var secondBodyBytes []byte

	failedDialer := &clientDialer{
		description:     "failed",
		minimumWeight:   1,
		priority:        0,
		successCount:    1,
		lastSuccessTime: now,
		settings:        settings,
		httpClient: &http.Client{Transport: serialTestRoundTripper(func(request *http.Request) (*http.Response, error) {
			var err error
			firstBodyBytes, err = io.ReadAll(request.Body)
			if err != nil {
				return nil, err
			}
			return nil, errors.New("route failed after consuming request body")
		})},
	}
	healthyDialer := &clientDialer{
		description:     "healthy",
		minimumWeight:   1,
		priority:        1,
		successCount:    1,
		lastSuccessTime: now,
		settings:        settings,
		httpClient: &http.Client{Transport: serialTestRoundTripper(func(request *http.Request) (*http.Response, error) {
			var err error
			secondBodyBytes, err = io.ReadAll(request.Body)
			if err != nil {
				return nil, err
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{},
				Body:       io.NopCloser(bytes.NewReader([]byte(`{}`))),
				Request:    request,
			}, nil
		})},
	}
	strategy := &ClientStrategy{
		ctx:               strategyCtx,
		log:               loggerOrDefault(nil),
		settings:          settings,
		dialers:           map[*clientDialer]bool{failedDialer: true, healthyDialer: true},
		extenderIpSecrets: map[netip.Addr]string{},
	}

	request, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		"https://api.example.invalid/auth/login",
		bytes.NewReader(requestBodyBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	helloRequest, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		"https://api.example.invalid/hello",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	result, err := strategy.HttpSerial(request, helloRequest)
	if err != nil {
		t.Fatalf("serial POST failed: %v", err)
	}
	if result.response.StatusCode != http.StatusOK {
		t.Fatalf("serial POST status = %s", result.response.Status)
	}
	if !bytes.Equal(firstBodyBytes, requestBodyBytes) {
		t.Fatalf("first route body = %q, want %q", firstBodyBytes, requestBodyBytes)
	}
	if !bytes.Equal(secondBodyBytes, requestBodyBytes) {
		t.Fatalf("fallback route body = %q, want %q", secondBodyBytes, requestBodyBytes)
	}
}

// Parallel evaluation has the same preferred-route fast path and must not
// share one consumed body between its attempts either.
func TestHttpParallelReplaysCompleteBodyAfterRouteFailure(t *testing.T) {
	strategyCtx, strategyCancel := context.WithCancel(context.Background())
	defer strategyCancel()

	settings := DefaultClientStrategySettings()
	settings.RequestTimeout = time.Second
	now := time.Now()
	requestBodyBytes := []byte(`{"request":"complete"}`)
	var firstBodyBytes []byte
	var secondBodyBytes []byte

	failedDialer := &clientDialer{
		description:     "failed",
		minimumWeight:   1,
		priority:        0,
		successCount:    1,
		lastSuccessTime: now,
		settings:        settings,
		httpClient: &http.Client{Transport: serialTestRoundTripper(func(request *http.Request) (*http.Response, error) {
			var err error
			firstBodyBytes, err = io.ReadAll(request.Body)
			if err != nil {
				return nil, err
			}
			return nil, errors.New("route failed after consuming request body")
		})},
	}
	healthyDialer := &clientDialer{
		description:     "healthy",
		minimumWeight:   1,
		priority:        1,
		successCount:    1,
		lastSuccessTime: now,
		settings:        settings,
		httpClient: &http.Client{Transport: serialTestRoundTripper(func(request *http.Request) (*http.Response, error) {
			var err error
			secondBodyBytes, err = io.ReadAll(request.Body)
			if err != nil {
				return nil, err
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{},
				Body:       io.NopCloser(bytes.NewReader([]byte(`{}`))),
				Request:    request,
			}, nil
		})},
	}
	strategy := &ClientStrategy{
		ctx:               strategyCtx,
		log:               loggerOrDefault(nil),
		settings:          settings,
		dialers:           map[*clientDialer]bool{failedDialer: true, healthyDialer: true},
		extenderIpSecrets: map[netip.Addr]string{},
	}

	request, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		"https://api.example.invalid/auth/login",
		bytes.NewReader(requestBodyBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := strategy.HttpParallel(request)
	if err != nil {
		t.Fatalf("parallel request failed: %v", err)
	}
	if result.response.StatusCode != http.StatusOK {
		t.Fatalf("parallel request status = %s", result.response.Status)
	}
	if !bytes.Equal(firstBodyBytes, requestBodyBytes) {
		t.Fatalf("first route body = %q, want %q", firstBodyBytes, requestBodyBytes)
	}
	if !bytes.Equal(secondBodyBytes, requestBodyBytes) {
		t.Fatalf("fallback route body = %q, want %q", secondBodyBytes, requestBodyBytes)
	}
}

// A one-shot body cannot safely participate in multi-route evaluation. Reject
// it before the first route sees bytes rather than emitting a later empty POST.
func TestHttpSerialRejectsNonReplayableBodyBeforeDial(t *testing.T) {
	strategyCtx, strategyCancel := context.WithCancel(context.Background())
	defer strategyCancel()

	settings := DefaultClientStrategySettings()
	dialCount := 0
	dialer := &clientDialer{
		minimumWeight:   1,
		successCount:    1,
		lastSuccessTime: time.Now(),
		settings:        settings,
		httpClient: &http.Client{Transport: serialTestRoundTripper(func(request *http.Request) (*http.Response, error) {
			dialCount += 1
			return nil, errors.New("unexpected dial")
		})},
	}
	strategy := &ClientStrategy{
		ctx:               strategyCtx,
		log:               loggerOrDefault(nil),
		settings:          settings,
		dialers:           map[*clientDialer]bool{dialer: true},
		extenderIpSecrets: map[netip.Addr]string{},
	}
	request, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		"https://api.example.invalid/auth/login",
		io.NopCloser(bytes.NewReader([]byte(`{}`))),
	)
	if err != nil {
		t.Fatal(err)
	}
	helloRequest, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		"https://api.example.invalid/hello",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = strategy.HttpSerial(request, helloRequest)
	if err == nil || err.Error() != "http request body is not replayable" {
		t.Fatalf("non-replayable body error = %v", err)
	}
	if dialCount != 0 {
		t.Fatalf("non-replayable request reached %d route(s)", dialCount)
	}
}
