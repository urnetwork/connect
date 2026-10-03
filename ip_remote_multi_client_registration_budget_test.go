// Initial provider evaluation starts after bounded control registration. Slow
// local setup must not spend the provider's independent response budget.
package connect

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// Older settings literals and the shared default both preserve finite setup.
func TestMultiClientSetupTimeoutDefaultsRemainFinite(t *testing.T) {
	for _, settings := range []*MultiClientSettings{{}, DefaultMultiClientSettings()} {
		if got := settings.clientSetupTimeout(); got != 30*time.Second {
			t.Fatalf("default setup bound=%s, want 30s", got)
		}
	}
	settings := &MultiClientSettings{WindowClientSetupTimeout: 7 * time.Second}
	if got := settings.clientSetupTimeout(); got != 7*time.Second {
		t.Fatalf("explicit setup bound=%s, want 7s", got)
	}
}

// A processed registration that completes after one ping interval still
// admits a provider whose real ping succeeds inside its own full interval.
func TestMultiClientExpandRegistrationRetainsFreshPingBudget(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newMultiClientExpandLifecycleFixture(t)
		fixture.window.settings.PingTimeout = 2 * time.Second
		fixture.window.settings.WindowExpandTimeout = time.Second
		fixture.window.settings.WindowClientSetupTimeout = 4 * time.Second
		generator := fixture.window.generator.(*multiClientExpandLifecycleGenerator).TestMultiClientGenerator
		newClient := generator.newClient
		registrationStarted := make(chan struct{})
		registrationReady := make(chan struct{})
		generator.newClient = func(
			ctx context.Context,
			args *MultiClientGeneratorClientArgs,
			settings *ClientSettings,
		) (*Client, error) {
			close(registrationStarted)
			select {
			case <-registrationReady:
				return newClient(ctx, args, settings)
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		expandDone := fixture.start()
		fixture.wait(t, "control registration start", registrationStarted)
		synctest.Wait()
		time.Sleep(3 * time.Second)
		close(registrationReady)
		synctest.Wait()
		select {
		case count := <-expandDone:
			t.Fatalf("registration spent the provider ping budget: admissions=%d", count)
		default:
		}

		fixture.wait(t, "held valid provider ping", fixture.pingResultEntered)
		time.Sleep(time.Second)
		fixture.releasePing()
		if got := fixture.result(t, expandDone); got != 1 {
			t.Fatalf("valid ping after processed registration admitted %d providers, want 1", got)
		}
		if got := fixture.window.failures.counts(time.Now())[windowFailureProvider]; got != 0 {
			t.Fatalf("slow control registration produced %d provider failures", got)
		}
		fixture.assertNoDirectArgsRemoval(t)
	})
}

// The real whole-window watchdog must not rebuild the evaluation epoch before
// its bounded setup and valid ping have had their independent allowances.
func TestMultiClientOutcomeRetainsOwnedSetupAndPing(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newMultiClientExpandLifecycleFixture(t)
		settings := fixture.window.settings
		settings.PingTimeout = 2 * time.Second
		settings.WindowExpandTimeout = time.Second
		settings.WindowClientSetupTimeout = 5 * time.Second
		settings.WindowOutcomeDeadline = 3 * time.Second
		settings.WindowOutcomeRebuildDeadline = 3 * time.Second
		generator := fixture.window.generator.(*multiClientExpandLifecycleGenerator).TestMultiClientGenerator
		newClient := generator.newClient
		registrationStarted := make(chan struct{})
		registrationReady := make(chan struct{})
		generator.newClient = func(ctx context.Context, args *MultiClientGeneratorClientArgs, settings *ClientSettings) (*Client, error) {
			close(registrationStarted)
			select {
			case <-registrationReady:
				return newClient(ctx, args, settings)
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		fixture.window.armOutcome()
		watchDone := make(chan struct{})
		go func() {
			defer close(watchDone)
			fixture.window.watchOutcome()
		}()
		defer func() {
			fixture.cancelWindow()
			<-watchDone
		}()
		expandDone := fixture.start()
		<-registrationStarted
		synctest.Wait()
		time.Sleep(4 * time.Second)
		synctest.Wait()
		select {
		case count := <-expandDone:
			t.Fatalf("window watchdog truncated owned setup: admissions=%d", count)
		default:
		}
		close(registrationReady)
		fixture.wait(t, "held valid provider ping", fixture.pingResultEntered)
		time.Sleep(time.Second)
		fixture.releasePing()
		if got := fixture.result(t, expandDone); got != 1 {
			t.Fatalf("window watchdog truncated a valid ping: admissions=%d", got)
		}
		fixture.window.outcomeLock.Lock()
		rebuilt := fixture.window.outcomeRebuilt
		fixture.window.outcomeLock.Unlock()
		if rebuilt {
			t.Fatal("valid owned evaluation triggered a premature window rebuild")
		}
	})
}

// No accepted candidate means no phase protection: an actually empty window
// still rebuilds and reports failure at its original configured deadlines.
func TestMultiClientOutcomeEmptyWindowKeepsPromptRescue(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newMultiClientExpandLifecycleFixture(t)
		settings := fixture.window.settings
		settings.WindowOutcomeDeadline = 2 * time.Second
		settings.WindowOutcomeRebuildDeadline = 2 * time.Second
		fixture.window.armOutcome()
		oldEpoch := fixture.window.evalEpochContext()
		watchDone := make(chan struct{})
		go func() {
			defer close(watchDone)
			fixture.window.watchOutcome()
		}()
		defer func() {
			fixture.cancelWindow()
			<-watchDone
		}()
		synctest.Wait()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if oldEpoch.Err() != context.Canceled {
			t.Fatal("empty window lost its prompt automatic rescue")
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		fixture.window.outcomeLock.Lock()
		failed := fixture.window.outcomeFailed
		fixture.window.outcomeLock.Unlock()
		if !failed {
			t.Fatal("empty rebuilt window lost its configured failure deadline")
		}
	})
}

// Successive accepted passes cannot slide the epoch cap indefinitely, and an
// old pass's cleanup cannot erase the current pass's bounded protection.
func TestMultiClientOutcomeOwnershipHasFixedEpochCap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	window := outcomeTestWindow(ctx, newRecordingLogger())
	window.settings.WindowExpandTimeout = time.Second
	window.settings.WindowClientSetupTimeout = 5 * time.Second
	window.settings.PingTimeout = 2 * time.Second
	window.armOutcome()
	first := &multiClientEvaluationOwner{deadline: window.outcomeArmTime.Add(7 * time.Second)}
	second := &multiClientEvaluationOwner{deadline: window.outcomeArmTime.Add(time.Hour)}
	window.beginOutcomeEvaluation(first)
	window.beginOutcomeEvaluation(second)
	window.releaseOutcomeEvaluation(first)
	window.outcomeLock.Lock()
	deadline := window.outcomeEvaluationDeadlineWithLock(3 * time.Second)
	owner := window.outcomeEvaluationOwner
	disabled := window.outcomeEvaluationDeadlineWithLock(0)
	window.outcomeLock.Unlock()
	if owner != second || deadline != 8*time.Second || disabled != 0 {
		t.Fatalf("epoch protection changed owner/cap/disable: current=%t deadline=%s disabled=%s", owner == second, deadline, disabled)
	}
	window.releaseOutcomeEvaluation(second)
	window.outcomeLock.Lock()
	deadline = window.outcomeEvaluationDeadlineWithLock(3 * time.Second)
	window.outcomeLock.Unlock()
	if deadline != 3*time.Second {
		t.Fatalf("ended pass retained watchdog protection: %s", deadline)
	}
}

// A setup timeout is local platform evidence, not a failed provider response.
func TestMultiClientSetupDeadlineIsPlatformFailure(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		window, generator, args, _ := newEvaluationChannelCreationTestState(ctx)
		window.settings.WindowClientSetupTimeout = 2 * time.Second
		result := beginEvaluationChannelCreation(ctx, generator, args, window.settings)
		<-generator.entered
		synctest.Wait()
		time.Sleep(2 * time.Second)
		err := <-result
		var setupErr *multiClientSetupError
		if !errors.As(err, &setupErr) || !errors.Is(err, context.DeadlineExceeded) || setupErr.argsOwned {
			t.Fatalf("setup deadline lost phase or unused-args ownership: %v", err)
		}
		if window.recordChannelCreationFailure(ctx, args, err) {
			t.Fatal("setup deadline became a terminal provider failure")
		}
		counts := window.failures.counts(time.Now())
		if counts[windowFailurePlatform] != 1 || counts[windowFailureProvider] != 0 {
			t.Fatalf("setup deadline classification: platform=%d provider=%d", counts[windowFailurePlatform], counts[windowFailureProvider])
		}
		window.generator.RemoveClientArgs(&args.MultiClientGeneratorClientArgs)
		if got := generator.removals.Load(); got != 1 {
			t.Fatalf("failed setup returned args %d times, want 1", got)
		}
	})
}

// A legacy constructor can finish after cancellation. Its client owns cleanup;
// the returned expansion pass must not also retire the same args as unused.
func TestMultiClientSetupLateClientKeepsCleanupOwnership(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newMultiClientExpandLifecycleFixture(t)
		fixture.window.settings.WindowClientSetupTimeout = 2 * time.Second
		generator := fixture.window.generator.(*multiClientExpandLifecycleGenerator).TestMultiClientGenerator
		newClient := generator.newClient
		registrationStarted := make(chan struct{})
		registrationReady := make(chan struct{})
		generator.newClient = func(
			ctx context.Context,
			args *MultiClientGeneratorClientArgs,
			settings *ClientSettings,
		) (*Client, error) {
			close(registrationStarted)
			<-registrationReady
			return newClient(ctx, args, settings)
		}
		expandDone := fixture.start()
		fixture.wait(t, "control registration start", registrationStarted)
		synctest.Wait()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		select {
		case count := <-expandDone:
			t.Fatalf("setup abandoned its unjoined constructor: admissions=%d", count)
		default:
		}
		close(registrationReady)
		if got := fixture.result(t, expandDone); got != 0 {
			t.Fatalf("late setup admitted %d canceled clients", got)
		}
		fixture.wait(t, "late client cleanup", fixture.clientRemoved)
		fixture.assertNoDirectArgsRemoval(t)
		if got := fixture.clientCount(); got != 0 {
			t.Fatalf("late setup installed %d clients", got)
		}
		counts := fixture.window.failures.counts(time.Now())
		if counts[windowFailurePlatform] != 1 || counts[windowFailureProvider] != 0 {
			t.Fatalf("late setup classification: platform=%d provider=%d", counts[windowFailurePlatform], counts[windowFailureProvider])
		}
	})
}

// Exposes the production API generator's separate setup-context capability.
type registrationBudgetContextGenerator struct {
	*TestMultiClientGenerator
	setup func(context.Context, context.Context, *MultiClientGeneratorClientArgs, *ClientSettings) (*Client, error)
}

// Keeps the admitted client on its lifecycle context, not its setup deadline.
func (self *registrationBudgetContextGenerator) NewClientContext(
	ctx context.Context,
	callCtx context.Context,
	args *MultiClientGeneratorClientArgs,
	settings *ClientSettings,
) (*Client, error) {
	return self.setup(ctx, callCtx, args, settings)
}

// An API-style constructor already retiring a failed client must retain that
// ownership when the window's independent setup deadline wraps its error.
func TestMultiClientSetupDeadlineRetainsGeneratorCleanupOwnership(t *testing.T) {
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newMultiClientExpandLifecycleFixture(t)
		fixture.window.settings.WindowClientSetupTimeout = 2 * time.Second
		legacy := fixture.window.generator.(*multiClientExpandLifecycleGenerator).TestMultiClientGenerator
		entered := make(chan struct{})
		fixture.window.generator = &registrationBudgetContextGenerator{
			TestMultiClientGenerator: legacy,
			setup: func(_ context.Context, callCtx context.Context, _ *MultiClientGeneratorClientArgs, _ *ClientSettings) (*Client, error) {
				close(entered)
				<-callCtx.Done()
				return nil, &multiClientSetupError{err: callCtx.Err(), argsOwned: true}
			},
		}
		expandDone := fixture.start()
		<-entered
		synctest.Wait()
		time.Sleep(2 * time.Second)
		if got := fixture.result(t, expandDone); got != 0 {
			t.Fatalf("failed setup admitted %d clients", got)
		}
		fixture.assertNoDirectArgsRemoval(t)
	})
}

// The real API generator starts joined retirement before returning a failed
// processed registration. Its error must prevent an earlier unused-args revoke.
func TestApiMultiClientRegistrationFailureRetainsArgsOwnership(t *testing.T) {
	joinEntered, releaseJoin := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseJoin) })
	_, _, _, callCtx, callCancel, result, attempts := newGeneratorRegistrationFixture(t, 4, func() {
		enteredOnce.Do(func() { close(joinEntered) })
		<-releaseJoin
	})
	_ = nextClientKeyRegistrationAttempt(t, attempts)
	waitForGeneratedKeyRegistration(t, callCtx, result)
	callCancel()
	got := <-result
	var setupErr *multiClientSetupError
	if got.client != nil || !errors.As(got.err, &setupErr) || !setupErr.argsOwned || !errors.Is(got.err, context.Canceled) {
		t.Fatalf("registration failure lost joined cleanup ownership: %v", got.err)
	}
	var registrationErr interface{ LocalControlRegistrationFailure() bool }
	if !errors.As(got.err, &registrationErr) || !registrationErr.LocalControlRegistrationFailure() {
		t.Fatal("processed local registration failure lost its typed non-provider cause")
	}
	waitCloseWaitBarrier(t, t.Context(), joinEntered, "registration retirement join")
}

// The marker is narrower than a setup error: a custom constructor can fail
// because its peer refused or went offline, which is not local registration.
func TestMultiClientGenericSetupFailureHasNoLocalRegistrationMarker(t *testing.T) {
	for _, err := range []error{context.DeadlineExceeded, errors.New("synthetic peer refusal"), &multiClientSetupError{err: context.DeadlineExceeded}} {
		var registrationErr interface{ LocalControlRegistrationFailure() bool }
		if errors.As(err, &registrationErr) && registrationErr.LocalControlRegistrationFailure() {
			t.Fatal("generic setup or peer failure was marked as local registration")
		}
	}
}

// The outer setup deadline may wrap an API registration cancellation; both
// the local cause and joined identity-retirement ownership must survive.
func TestMultiClientSetupDeadlineKeepsLocalRegistrationCause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		generator := &registrationBudgetContextGenerator{setup: func(_ context.Context, callCtx context.Context, _ *MultiClientGeneratorClientArgs, _ *ClientSettings) (*Client, error) {
			<-callCtx.Done()
			return nil, &multiClientSetupError{err: &localControlRegistrationError{err: callCtx.Err()}, argsOwned: true}
		}}
		_, err := newMultiClientChannelClient(ctx, cancel, &MultiClientGeneratorClientArgs{}, generator, DefaultClientSettings(), time.Second)
		var setupErr *multiClientSetupError
		var registrationErr interface{ LocalControlRegistrationFailure() bool }
		if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &setupErr) || !setupErr.argsOwned ||
			!errors.As(err, &registrationErr) || !registrationErr.LocalControlRegistrationFailure() {
			t.Fatalf("setup deadline erased exact registration ownership/cause: %v", err)
		}
	})
}

// Setup cancellation after success cannot terminate a healthy API-style client.
func TestMultiClientSetupContextKeepsHealthyClientLifetime(t *testing.T) {
	testMultiClientSetupHealthyLifetime(t, true)
}

// The compatibility bridge also stops before a legacy client is handed onward.
func TestMultiClientLegacySetupKeepsHealthyClientLifetime(t *testing.T) {
	testMultiClientSetupHealthyLifetime(t, false)
}

// Runs both generator contracts against real client ownership and virtual time.
func testMultiClientSetupHealthyLifetime(t *testing.T, contextAware bool) {
	t.Helper()
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		legacy := &TestMultiClientGenerator{
			newClient: func(ctx context.Context, args *MultiClientGeneratorClientArgs, settings *ClientSettings) (*Client, error) {
				return NewClient(ctx, args.ClientId, NewNoContractClientOob(), settings), nil
			},
		}
		var generator MultiClientGenerator = legacy
		var setupCtx context.Context
		if contextAware {
			generator = &registrationBudgetContextGenerator{
				TestMultiClientGenerator: legacy,
				setup: func(ctx context.Context, callCtx context.Context, args *MultiClientGeneratorClientArgs, settings *ClientSettings) (*Client, error) {
					setupCtx = callCtx
					if deadline, ok := callCtx.Deadline(); !ok || deadline.Sub(time.Now()) != 2*time.Second {
						t.Error("API-style setup did not receive its independent deadline")
					}
					return legacy.newClient(ctx, args, settings)
				},
			}
		}
		client, err := newMultiClientChannelClient(ctx, cancel, &MultiClientGeneratorClientArgs{ClientId: NewId()},
			generator, closeWaitClientSettings(), 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			cancel()
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Errorf("join healthy setup client: %v", err)
			}
		}()
		if contextAware && setupCtx.Err() != context.Canceled {
			t.Fatal("completed API-style setup retained its deadline timer")
		}
		time.Sleep(3 * time.Second)
		synctest.Wait()
		if err := client.Ctx().Err(); err != nil {
			t.Fatalf("completed setup retired a healthy client: %v", err)
		}
	})
}
