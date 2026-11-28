package connect

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// An h3 transport whose claim the budget cannot admit, on a budget whose h1
// claim still fits. Its h3 runner is a stand-in that reports the target
// carrying once the claim is admitted; its h1 dials the local platform.
type testingH3FallbackFixture struct {
	budget    *PlatformTransportBudget
	h3Claim   ByteCount
	h1Claim   ByteCount
	blocker   *platformTransportBudgetReservation
	platform  *testingPlatformServer
	transport *PlatformTransport
	h3Started chan struct{}
}

func newTestingH3FallbackFixture(t *testing.T, fallbackTimeout time.Duration) *testingH3FallbackFixture {
	t.Helper()
	fixture := &testingH3FallbackFixture{
		platform:  newTestingPlatformServer(t),
		h3Started: make(chan struct{}, 1),
	}
	settings := testingPlatformTransportSettings()
	settings.H3BudgetFallbackTimeout = fallbackTimeout
	fixture.h3Claim = settings.H3MemoryReservationByteCount(TransportModeH3, nil)
	fixture.h1Claim = settings.H1MemoryReservationByteCount()
	// one complete h3 graph and one h1: another explicit h3 claim holds the
	// graph, so the transport's own claim can never be admitted beside it
	fixture.budget = NewPlatformTransportBudget(fixture.h3Claim+fixture.h1Claim, 16)
	settings.PlatformTransportBudget = fixture.budget
	fixture.blocker = fixture.budget.register(platformTransportBudgetH3Explicit, fixture.h3Claim, true)
	if !fixture.blocker.TryAcquire() {
		t.Fatal("the blocking h3 claim did not fit")
	}
	t.Cleanup(fixture.blocker.Release)

	var transportReady atomic.Pointer[PlatformTransport]
	settings.runH3ModeForTest = func(ctx context.Context, mode TransportMode, _ time.Duration) {
		if mode != TransportModeH3 {
			t.Errorf("h3 runner ran mode %s", mode)
			return
		}
		transport := transportReady.Load()
		select {
		case fixture.h3Started <- struct{}{}:
		default:
		}
		// the target's connection carries until its runner ends
		transport.setModeAvailable(TransportModeH3, true)
		<-ctx.Done()
		transport.setModeAvailable(TransportModeH3, false)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	fixture.transport = NewPlatformTransportWithTargetMode(
		ctx,
		NewClientStrategyWithDefaults(ctx),
		NewRouteManager(ctx, "h3-budget-fallback"),
		fixture.platform.url,
		&ClientAuth{
			ByJwt:      "testing",
			InstanceId: NewId(),
			AppVersion: "testing",
		},
		TransportModeH3,
		settings,
	)
	transportReady.Store(fixture.transport)
	t.Cleanup(func() {
		join, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := fixture.transport.CloseAndWait(join); err != nil {
			t.Errorf("transport did not close: %v", err)
		}
		stats := fixture.budget.Stats()
		if stats.UsedByteCount != fixture.blocker.byteCount && stats.UsedByteCount != 0 {
			t.Errorf("closed transport kept budget: %+v", stats)
		}
	})
	return fixture
}

// The transport the budget cannot admit on h3 carries its client on h1: it
// dials the platform over h1, registers its routes and reads connected, and
// it no longer reads waiting for budget, so the SDK counts it as trying.
// Before R20 it waited for budget for as long as it lived.
func TestPlatformTransportH3BudgetRefusalCarriesOnH1(t *testing.T) {
	fixture := newTestingH3FallbackFixture(t, 0)
	transport := fixture.transport

	if !waitForCondition(15*time.Second, func() bool {
		return transport.IsConnected() && transport.ActiveMode() == TransportModeH1
	}) {
		t.Fatalf("h3 transport refused by the budget never carried on h1: mode=%s connected=%t fallback=%t waiting=%t",
			transport.ActiveMode(), transport.IsConnected(), transport.H3BudgetFallback(), transport.IsWaitingForBudget())
	}
	if fixture.platform.connectCount.Load() == 0 || !transport.EverConnected() {
		t.Fatal("the h1 fallback reported connected without reaching the platform")
	}
	if !transport.H3BudgetFallback() || transport.IsWaitingForBudget() {
		t.Fatalf("status fallback=%t waiting=%t, want true/false", transport.H3BudgetFallback(), transport.IsWaitingForBudget())
	}
	select {
	case <-fixture.h3Started:
		t.Fatal("the h3 runner started without its claim")
	default:
	}
	stats := fixture.budget.Stats()
	if stats.UsedByteCount != fixture.h3Claim+fixture.h1Claim || stats.UsedTransportCount != 2 {
		t.Fatalf("budget %+v, want the blocker and the fallback's h1", stats)
	}
	if !transport.h3BudgetReservation.IsWaiting() {
		t.Fatal("the h3 claim was admitted while the blocker held the graph")
	}
	func() {
		fixture.budget.mutex.Lock()
		defer fixture.budget.mutex.Unlock()
		if !transport.h3BudgetReservation.deferred {
			t.Fatal("the h3 claim kept its precedence while h1 carried the client")
		}
	}()
}

// Once the budget can admit the claim, the transport returns to h3: the h3
// runner starts while h1 still carries, h1 drains once h3 is active, and the
// fallback then stops and returns its claim.
func TestPlatformTransportH3BudgetFallbackReturnsToH3WhenAdmitted(t *testing.T) {
	fixture := newTestingH3FallbackFixture(t, 0)
	transport := fixture.transport

	if !waitForCondition(15*time.Second, func() bool {
		return transport.IsConnected() && transport.ActiveMode() == TransportModeH1
	}) {
		t.Fatal("the h1 fallback never carried the client")
	}

	fixture.blocker.Release()
	select {
	case <-fixture.h3Started:
	case <-time.After(15 * time.Second):
		t.Fatal("the h3 claim was not retried when the budget freed")
	}
	if !testingWaitForActiveMode(transport, TransportModeH3, 15*time.Second) {
		t.Fatalf("active mode %s, want h3", transport.ActiveMode())
	}
	// the testing platform is silent, so the superseded h1 connection drains
	// after InactiveDrainTimeout and the fallback then stops
	if !waitForCondition(15*time.Second, func() bool {
		return !transport.H3BudgetFallback() && fixture.budget.Stats().UsedByteCount == fixture.h3Claim
	}) {
		t.Fatalf("the fallback did not stop after h3 took over: fallback=%t budget=%+v",
			transport.H3BudgetFallback(), fixture.budget.Stats())
	}
	stats := fixture.budget.Stats()
	if stats.UsedTransportCount != 1 || stats.PendingH1Count != 0 {
		t.Fatalf("budget %+v, want only the h3 claim", stats)
	}
	if transport.ActiveMode() != TransportModeH3 || transport.IsWaitingForBudget() {
		t.Fatalf("after the fallback: mode=%s waiting=%t", transport.ActiveMode(), transport.IsWaitingForBudget())
	}
	connects := fixture.platform.connectCount.Load()
	// the stopped fallback dials no more
	transport.Kick()
	if !testingWaitForActiveMode(transport, TransportModeH3, 5*time.Second) {
		t.Fatal("kick lost the target")
	}
	if got := fixture.platform.connectCount.Load(); got != connects {
		t.Fatalf("the stopped fallback dialed h1 again: %d -> %d", connects, got)
	}
}

// A claim admitted within the fallback timeout never touches h1: no h1
// claim is registered and nothing dials the platform over h1. This is the
// migration that releases the old carrier just after building its
// replacement.
func TestPlatformTransportH3AdmittedWithinFallbackTimeoutStaysOnH3(t *testing.T) {
	fixture := newTestingH3FallbackFixture(t, time.Hour)
	transport := fixture.transport

	if !waitForCondition(15*time.Second, func() bool {
		fixture.budget.mutex.Lock()
		defer fixture.budget.mutex.Unlock()
		// contending: the runner cleared the deferral it registered with
		return !transport.h3BudgetReservation.deferred
	}) {
		t.Fatal("the h3 claim never contended")
	}
	if !transport.IsWaitingForBudget() || transport.H3BudgetFallback() {
		t.Fatal("a claim within its fallback timeout must read waiting, not fallen back")
	}
	fixture.blocker.Release()
	select {
	case <-fixture.h3Started:
	case <-time.After(15 * time.Second):
		t.Fatal("the h3 claim was not admitted when the budget freed")
	}
	if !testingWaitForActiveMode(transport, TransportModeH3, 15*time.Second) {
		t.Fatalf("active mode %s, want h3", transport.ActiveMode())
	}
	stats := fixture.budget.Stats()
	if transport.h3BudgetFallback.isEngaged() || fixture.platform.connectCount.Load() != 0 ||
		stats.UsedByteCount != fixture.h3Claim || stats.PendingH1Count != 0 {
		t.Fatalf("the admitted claim detoured through h1: engaged=%t connects=%d budget=%+v",
			transport.h3BudgetFallback.isEngaged(), fixture.platform.connectCount.Load(), stats)
	}
}

// A held h3 transport, such as a provider's standby, does not contend for its
// claim, so its pending claim takes no precedence over an h1 claim the budget
// can admit. Before R20 the held standby's explicit claim was reserved
// against every h1 claim, and a phone providing on h3 could admit neither
// its consumer's h3 nor its h1.
func TestPlatformTransportHeldH3ClaimDoesNotHoldBackH1(t *testing.T) {
	settings := testingPlatformTransportSettings()
	h3Claim := settings.H3MemoryReservationByteCount(TransportModeH3, nil)
	h1Claim := settings.H1MemoryReservationByteCount()
	// either claim fits alone, but not both: the held claim's precedence
	// alone would refuse the h1
	budget := NewPlatformTransportBudget(h3Claim+h1Claim-1, 16)
	settings.PlatformTransportBudget = budget
	settings.StartDisabled = true
	held := testingH3FallbackTransport(t, settings)

	h1 := budget.register(platformTransportBudgetH1, h1Claim, true)
	defer h1.Release()
	if !h1.TryAcquire() {
		t.Fatalf("the held standby's h3 claim held back an h1 claim that fits: %+v", budget.Stats())
	}
	h1.Release()

	// once enabled, it contends: its claim is admitted, or it carries on h1
	held.SetEnabled(true)
	if !waitForCondition(15*time.Second, func() bool {
		return held.h3BudgetReservation.IsAcquired()
	}) {
		t.Fatal("the enabled transport never contended for its claim")
	}
}

// With a negative fallback timeout the transport keeps the old behavior: it
// waits for its claim, which keeps its precedence, and never dials h1.
func TestPlatformTransportH3BudgetFallbackDisabledWaitsForClaim(t *testing.T) {
	settings := testingPlatformTransportSettings()
	settings.H3BudgetFallbackTimeout = -1
	settings.PlatformTransportBudget = NewPlatformTransportBudget(mib(1), 16)
	transport := testingH3FallbackTransport(t, settings)
	if transport.h3BudgetFallback != nil || transport.h1RetainedBudget != nil ||
		transport.modePreferences[TransportModeH1] != DefaultTransportModePreferences()[TransportModeH1] {
		t.Fatal("a disabled fallback still prepared h1")
	}
	func() {
		settings.PlatformTransportBudget.mutex.Lock()
		defer settings.PlatformTransportBudget.mutex.Unlock()
		if transport.h3BudgetReservation.deferred {
			t.Fatal("a claim with no fallback registered without precedence")
		}
	}()
	if !waitForCondition(5*time.Second, transport.IsWaitingForBudget) || transport.H3BudgetFallback() {
		t.Fatal("the claim that does not fit must wait, with no fallback")
	}
}

func testingH3FallbackTransport(t *testing.T, settings *PlatformTransportSettings) *PlatformTransport {
	t.Helper()
	settings.runH3ModeForTest = func(ctx context.Context, _ TransportMode, _ time.Duration) {
		<-ctx.Done()
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	transport := NewPlatformTransportWithTargetMode(
		ctx,
		NewClientStrategyWithDefaults(ctx),
		NewRouteManager(ctx, "h3-budget-fallback"),
		"ws://127.0.0.1:1",
		&ClientAuth{
			ByJwt:      "testing",
			InstanceId: NewId(),
			AppVersion: "testing",
		},
		TransportModeH3,
		settings,
	)
	t.Cleanup(func() {
		join, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := transport.CloseAndWait(join); err != nil {
			t.Errorf("transport did not close: %v", err)
		}
	})
	return transport
}

// The h1 runner's stand down ends with its context, while the better mode
// may still read active, since the election ends with the same context. The
// runner must then return. Before the fix, a runner that had never connected
// went around its loop without parking: an Auto transport that ranks h3 above
// h1 spun at close, and its close never joined.
func TestPlatformTransportH1StandDownEndsWithContext(t *testing.T) {
	platform := newTestingPlatformServer(t)
	settings := testingPlatformTransportSettings()
	settings.ModePreferences = map[TransportMode]int{TransportModeH3: 1, TransportModeH1: 2}
	// h1 starts a tier after h3, by which time h3 is active, so its first
	// pass stands down before it ever dials
	settings.ModeInitialDelay = 200 * time.Millisecond
	standingDown := make(chan struct{}, 1)
	settings.h1StandDownForTest = func(context.Context) {
		select {
		case standingDown <- struct{}{}:
		default:
		}
	}
	var transportReady atomic.Pointer[PlatformTransport]
	ready := make(chan struct{})
	settings.runH3ModeForTest = func(ctx context.Context, _ TransportMode, _ time.Duration) {
		<-ready
		transportReady.Load().setModeAvailable(TransportModeH3, true)
		<-ctx.Done()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	transport := NewPlatformTransportWithTargetMode(
		ctx,
		NewClientStrategyWithDefaults(ctx),
		NewRouteManager(ctx, "h1-stand-down"),
		platform.url,
		&ClientAuth{ByJwt: "testing", InstanceId: NewId(), AppVersion: "testing"},
		TransportModeAuto,
		settings,
	)
	transportReady.Store(transport)
	close(ready)
	select {
	case <-standingDown:
	case <-time.After(15 * time.Second):
		t.Fatal("h1 never stood down for the active h3")
	}
	if platform.connectCount.Load() != 0 {
		t.Fatal("h1 dialed before it stood down, so it did not stand down from a fresh start")
	}
	join, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	if err := transport.CloseAndWait(join); err != nil {
		t.Fatalf("close did not join the standing-down h1 runner: %v", err)
	}
}

// A claim on its fallback that only the preemption of an optional Auto-H3
// lease can admit keeps its precedence while that preemption runs, so the
// lease's owner cannot take back the capacity it yields: the claim is
// admitted and the lease waits. Deferred through its own preemption, the
// claim let the lease reacquire what it yielded, and revoked it again on the
// next retry, forever.
func TestPlatformTransportBudgetFallbackClaimKeepsWhatItPreempts(t *testing.T) {
	budget := NewPlatformTransportBudget(10, 4)
	lease := budget.registerWithPriority(platformTransportBudgetH3Auto, 6, false, PlatformTransportBudgetPriorityBackground)
	defer lease.Release()
	if !lease.TryAcquire() {
		t.Fatal("the optional lease did not fit")
	}
	claim := budget.registerWithPriorityDeferred(platformTransportBudgetH3Explicit, 6, true,
		PlatformTransportBudgetPriorityForeground, true)
	defer claim.Release()

	acquired, preempting, _ := claim.TryAcquireOrPreempt(true)
	if acquired || !preempting {
		t.Fatalf("acquired=%t preempting=%t, want a preemption under way", acquired, preempting)
	}
	select {
	case <-lease.PreemptNotify():
	default:
		t.Fatal("the lease was not asked to yield")
	}
	// the lease's owner tears down its carrier, yields, and contends again
	if !lease.Yield() {
		t.Fatal("the lease did not yield")
	}
	if lease.TryAcquire() {
		t.Fatal("the yielded lease took back the capacity its preemptor needs")
	}
	acquired, _, _ = claim.TryAcquireOrPreempt(true)
	if !acquired {
		t.Fatalf("the claim was not admitted to the capacity it preempted: %+v", budget.Stats())
	}

	// a claim preemption cannot admit is deferred and holds back nobody
	blocked := budget.registerWithPriorityDeferred(platformTransportBudgetH3Explicit, 6, true,
		PlatformTransportBudgetPriorityForeground, false)
	defer blocked.Release()
	acquired, preempting, notify := blocked.TryAcquireOrPreempt(true)
	if acquired || preempting {
		t.Fatalf("acquired=%t preempting=%t, want a refusal", acquired, preempting)
	}
	select {
	case <-notify:
		t.Fatal("the refused claim's own deferral woke its retry")
	default:
	}
	h1 := budget.register(platformTransportBudgetH1, 4, true)
	defer h1.Release()
	if !h1.TryAcquire() {
		t.Fatal("the deferred claim held back an h1 claim that fits")
	}
}
