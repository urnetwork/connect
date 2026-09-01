package connect

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"
)

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
