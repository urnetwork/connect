// Candidate demand preserves identity ownership without speculative mints.
package connect

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// Only construction is held. Fixed discovery and credential generation are
// the actual API generator, with synthetic authority instead of an API call.
type fixedDemandApiGenerator struct {
	*ApiMultiClientGenerator
	setupEntered chan struct{}
	setupCalls   atomic.Int32
}

// Run the real enumerator and in-memory provider transport together. Every
// generated client and the producer are joined by cleanup.
func newDemandExpansionFixture(t *testing.T, fixed bool, windowType WindowType) (*multiClientExpandLifecycleFixture, *TestMultiClientGenerator, *atomic.Int32, <-chan struct{}) {
	t.Helper()
	fixture := newMultiClientExpandLifecycleFixture(t)
	window := fixture.window
	window.windowType = windowType
	window.createFailThrottle = newLogThrottle(evaluationFailureLogInterval)
	window.enumerateZeroThrottle = newLogThrottle(evaluationFailureLogInterval)
	generator := window.generator.(*multiClientExpandLifecycleGenerator).TestMultiClientGenerator
	// The older fixture preloads one inert synthetic args object. Here the
	// actual producer owns every requested credential instead.
	<-window.clientChannelArgs
	window.clientChannelArgs = make(chan *multiClientChannelArgs)
	if fixed {
		window.generator = generator
	}
	minted := &atomic.Int32{}
	newArgs := generator.newClientArgs
	generator.newClientArgs = func() (*MultiClientGeneratorClientArgs, error) {
		minted.Add(1)
		return newArgs()
	}
	entered := make(chan struct{}, 4)
	var stateLock sync.Mutex
	clients := []*Client{}
	newClient := generator.newClient
	generator.newClient = func(ctx context.Context, args *MultiClientGeneratorClientArgs, settings *ClientSettings) (*Client, error) {
		client, err := newClient(ctx, args, settings)
		if client != nil {
			stateLock.Lock()
			clients = append(clients, client)
			stateLock.Unlock()
			entered <- struct{}{}
		}
		return client, err
	}
	producerDone := make(chan struct{})
	go func() { defer close(producerDone); window.randomEnumerateClientArgs() }()
	t.Cleanup(func() {
		fixture.releasePing()
		fixture.cancelWindow()
		<-producerDone
		stateLock.Lock()
		joined := append([]*Client(nil), clients...)
		stateLock.Unlock()
		for _, client := range joined {
			client.Cancel()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := client.CloseAndWait(ctx); err != nil {
				t.Errorf("join generated candidate: %v", err)
			}
			cancel()
		}
	})
	return fixture, generator, minted, entered
}

// A failed construction leaves the fixed destination eligible for the next
// request. Its healthy retry is admitted, without a prefetched third mint.
func TestFixedCandidateFailureKeepsHealthyReplacement(t *testing.T) {
	fixture, generator, minted, entered := newDemandExpansionFixture(t, true, WindowTypeQuality)
	newClient := generator.newClient
	calls := 0
	generator.newClient = func(ctx context.Context, args *MultiClientGeneratorClientArgs, settings *ClientSettings) (*Client, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("synthetic first setup refusal")
		}
		return newClient(ctx, args, settings)
	}
	if added := <-fixture.start(); added != 0 {
		t.Fatalf("failed setup added=%d", added)
	}
	done := fixture.start()
	fixture.wait(t, "replacement constructed", entered)
	fixture.wait(t, "replacement ping held", fixture.pingResultEntered)
	fixture.releasePing()
	if added := <-done; added != 1 || minted.Load() != 2 {
		t.Fatalf("replacement demand: added=%d minted=%d", added, minted.Load())
	}
}

// The dynamic pool must still construct a second demanded candidate while
// the first evaluation callback is held, for both ordinary window types.
func testDynamicCandidateDemand(t *testing.T, windowType WindowType) {
	t.Helper()
	fixture, _, _, entered := newDemandExpansionFixture(t, false, windowType)
	done := make(chan int, 1)
	go func() {
		done <- fixture.window.expand(WindowSizeSettings{WindowSizeMin: 2, WindowSizeMax: 2, WindowSizeHardMax: 2}, 0, 0, 2, 2, 2, 0)
	}()
	fixture.wait(t, "first demanded setup", entered)
	fixture.wait(t, "first ping held", fixture.pingResultEntered)
	fixture.wait(t, "second setup while first ping held", entered)
	fixture.releasePing()
	if added := <-done; added != 2 {
		t.Fatalf("dynamic demanded candidates admitted=%d, want 2", added)
	}
}

// Quality candidates retain overlapping evaluation, not one serial peer wait.
func TestDynamicQualityCandidateDemandStaysConcurrent(t *testing.T) {
	testDynamicCandidateDemand(t, WindowTypeQuality)
}

// Speed candidates use the same unchanged dynamic producer and pool budget.
func TestDynamicSpeedCandidateDemandStaysConcurrent(t *testing.T) {
	testDynamicCandidateDemand(t, WindowTypeSpeed)
}

// A legacy generator lacks cooperative cancellation; keep its established
// late-result retirement without allowing that result into a new demand.
func TestFixedCandidateLegacyLateMintRetiresOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		entered, release, retired := make(chan struct{}), make(chan struct{}), make(chan struct{})
		id := NewId()
		generator := &TestMultiClientGenerator{
			newClientArgs: func() (*MultiClientGeneratorClientArgs, error) {
				close(entered)
				<-release
				return &MultiClientGeneratorClientArgs{ClientId: id}, nil
			},
			removeClientArgs: func(args *MultiClientGeneratorClientArgs) {
				if args.ClientId != id {
					t.Error("late cleanup changed identity")
				}
				close(retired)
			},
		}
		window := outcomeEnumeratorTestWindow(t.Context(), newRecordingLogger(), generator)
		done := make(chan struct{})
		go func() {
			defer close(done)
			args, err := window.fixedClientArgs(ctx, RequireMultiHopId(NewId()))
			if args != nil || !errors.Is(err, context.Canceled) {
				t.Errorf("late mint result=%v error=%v", args != nil, err)
			}
		}()
		<-entered
		cancel()
		<-done
		close(release)
		<-retired
	})
}

// Keep the first candidate in setup until the test cancels its window.
func (self *fixedDemandApiGenerator) NewClientContext(_ context.Context, callCtx context.Context, _ *MultiClientGeneratorClientArgs, _ *ClientSettings) (*Client, error) {
	self.setupCalls.Add(1)
	close(self.setupEntered)
	<-callCtx.Done()
	return nil, callCtx.Err()
}

// One expansion request must not create an unused second identity merely
// because its request notification precedes the first candidate's admission.
func TestFixedCandidateMintsOnlyRequestedIdentity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var publicCalls, minted, retired atomic.Int32
		api, strategy := authObservationTestApi(t.Context(), nil, true, serialTestRoundTripper(func(request *http.Request) (*http.Response, error) {
			publicCalls.Add(1)
			return nil, context.Canceled
		}))
		defer api.Close()
		source, provider := NewId(), NewId()
		settings := DefaultApiMultiClientGeneratorSettings()
		settings.ClientCredentials = &generatorCredentialFixture{
			auth: func(context.Context, *AuthNetworkClientArgs) (*AuthNetworkClientResult, error) {
				minted.Add(1)
				return &AuthNetworkClientResult{ByClientJwt: generatorCredentialTestJwt(t, NewId())}, nil
			},
			remove: func(context.Context, *RemoveNetworkClientArgs) (*RemoveNetworkClientResult, error) {
				retired.Add(1)
				return &RemoveNetworkClientResult{}, nil
			},
		}
		generator := &fixedDemandApiGenerator{
			ApiMultiClientGenerator: NewApiMultiClientGenerator(t.Context(), []*ProviderSpec{{ClientId: &provider}}, strategy, nil,
				"https://api.candidate.example", "synthetic-parent-token", "wss://platform.candidate.example",
				"synthetic candidate", "synthetic", "test", &source, DefaultClientSettings, settings),
			setupEntered: make(chan struct{}),
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		window := outcomeEnumeratorTestWindow(ctx, newRecordingLogger(), generator)
		window.cancel = cancel
		enumerationDone := make(chan struct{})
		go func() {
			defer close(enumerationDone)
			window.randomEnumerateClientArgs()
		}()
		// The enumerator is now blocked handing its first offer to expand;
		// only the production expansion request sends the wake notification.
		synctest.Wait()
		beforeDemand := minted.Load()
		expandDone := make(chan int, 1)
		go func() {
			expandDone <- window.expand(WindowSizeSettings{WindowSizeMin: 1, WindowSizeMax: 1, WindowSizeHardMax: 1}, 0, 0, 1, 1, 1, 0)
		}()
		<-generator.setupEntered
		synctest.Wait()
		beforeCancel := minted.Load()
		cancel()
		if added := <-expandDone; added != 0 {
			t.Errorf("canceled setup admitted %d clients", added)
		}
		<-enumerationDone
		if err := generator.CloseAndWait(t.Context()); err != nil {
			t.Fatal(err)
		}
		if publicCalls.Load() != 0 || retired.Load() != minted.Load() || generator.setupCalls.Load() != 1 {
			t.Fatalf("ownership changed: public=%d minted=%d retired=%d setup=%d", publicCalls.Load(), minted.Load(), retired.Load(), generator.setupCalls.Load())
		}
		if beforeDemand != 0 || beforeCancel != 1 {
			t.Errorf("fixed candidate mint demand: before=%d pending=%d; want 0 before demand, 1 before any setup outcome", beforeDemand, beforeCancel)
		}
	})
}

// Synthetic mint and retirement endpoints retain the ordinary generator shape.
type fixedDemandGenerator struct {
	testingEmptyMultiClientGenerator
	mint       func(context.Context, MultiHopId) (*MultiClientGeneratorClientArgs, error)
	remove     func(*MultiClientGeneratorClientArgs)
	setupCalls atomic.Int32
}

// Destination-aware mint records the exact accepted offer.
func (self *fixedDemandGenerator) NewClientArgsForDestinationContext(ctx context.Context, destination MultiHopId) (*MultiClientGeneratorClientArgs, error) {
	return self.mint(ctx, destination)
}

// Only an actually returned identity can reach this cleanup path.
func (self *fixedDemandGenerator) RemoveClientArgs(args *MultiClientGeneratorClientArgs) {
	self.remove(args)
}

// Count any accidental construction after cancellation or expiry.
func (self *fixedDemandGenerator) NewClient(ctx context.Context, args *MultiClientGeneratorClientArgs, settings *ClientSettings) (*Client, error) {
	self.setupCalls.Add(1)
	return self.testingEmptyMultiClientGenerator.NewClient(ctx, args, settings)
}

// No demand exists after an expansion is canceled.
func TestFixedCandidateCanceledDemandMintsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	generator := &fixedDemandGenerator{
		mint: func(context.Context, MultiHopId) (*MultiClientGeneratorClientArgs, error) {
			t.Error("canceled demand minted an identity")
			return nil, nil
		},
		remove: func(*MultiClientGeneratorClientArgs) { t.Error("canceled demand retired an unminted identity") },
	}
	window := outcomeEnumeratorTestWindow(t.Context(), newRecordingLogger(), generator)
	args, err := window.fixedClientArgs(ctx, RequireMultiHopId(NewId()))
	if args != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled demand result=%v error=%v", args != nil, err)
	}
}

// A cooperative mint remains joined through cancellation, including a late
// committed identity returned while its callback is unwinding.
func testFixedCandidateCancellation(t *testing.T, mintErr error) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		entered, release := make(chan struct{}), make(chan struct{})
		id, destination := NewId(), RequireMultiHopId(NewId())
		retired := 0
		generator := &fixedDemandGenerator{
			mint: func(callCtx context.Context, got MultiHopId) (*MultiClientGeneratorClientArgs, error) {
				if got != destination {
					t.Error("mint destination changed")
				}
				close(entered)
				<-callCtx.Done()
				<-release
				return &MultiClientGeneratorClientArgs{ClientId: id}, mintErr
			},
			remove: func(args *MultiClientGeneratorClientArgs) {
				if args.ClientId != id {
					t.Error("retired another identity")
				}
				retired++
			},
		}
		window := outcomeEnumeratorTestWindow(t.Context(), newRecordingLogger(), generator)
		done := make(chan struct{})
		go func() {
			defer close(done)
			args, err := window.fixedClientArgs(ctx, destination)
			if args != nil || !errors.Is(err, context.Canceled) {
				t.Errorf("canceled mint result=%v error=%v", args != nil, err)
			}
		}()
		<-entered
		cancel()
		synctest.Wait()
		select {
		case <-done:
			t.Error("cancellation abandoned its mint owner")
		default:
		}
		close(release)
		<-done
		if retired != 1 {
			t.Fatalf("retired=%d, want the one committed identity", retired)
		}
	})
}

// A canceled accepted mint joins and retires its committed late success.
func TestFixedCandidateCancellationJoinsAndRetiresMint(t *testing.T) {
	testFixedCandidateCancellation(t, nil)
}

// Cancellation cannot discard an identity returned alongside a later error.
func TestFixedCandidateCancellationRetiresPartialMint(t *testing.T) {
	testFixedCandidateCancellation(t, errors.New("synthetic post-mint failure"))
}

// Transient, nil-result and partial-result failures retain retry cadence and
// destination, retiring only the partial identity before the successful mint.
func TestFixedCandidateMintRetriesWithinDemand(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls, retired := 0, 0
		id, partialId, destination := NewId(), NewId(), RequireMultiHopId(NewId())
		generator := &fixedDemandGenerator{
			mint: func(_ context.Context, got MultiHopId) (*MultiClientGeneratorClientArgs, error) {
				calls++
				if got != destination {
					t.Error("retry changed destination")
				}
				switch calls {
				case 1:
					return nil, errors.New("synthetic temporary mint failure")
				case 2:
					return nil, nil
				case 3:
					return &MultiClientGeneratorClientArgs{ClientId: partialId}, errors.New("synthetic partial mint failure")
				}
				return &MultiClientGeneratorClientArgs{ClientId: id}, nil
			},
			remove: func(args *MultiClientGeneratorClientArgs) {
				if args == nil || args.ClientId != partialId {
					t.Error("retry retired an unowned identity")
				}
				retired++
			},
		}
		window := outcomeEnumeratorTestWindow(t.Context(), newRecordingLogger(), generator)
		window.settings.WindowEnumerateErrorTimeout = 3 * time.Second
		started := time.Now()
		args, err := window.fixedClientArgs(t.Context(), destination)
		if err != nil || args == nil || args.ClientId != id || calls != 4 || retired != 1 || time.Since(started) != 9*time.Second {
			t.Fatalf("retry lost demand: calls=%d retired=%d elapsed=%s error=%v", calls, retired, time.Since(started), err)
		}
	})
}

// A mint that returns success after acquisition expired owns retirement,
// not a late provider evaluation or an extension of the existing pass budget.
func TestFixedCandidateExpiredMintRetiresBeforeSetup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls, retired := 0, 0
		id, destination := NewId(), RequireMultiHopId(NewId())
		generator := &fixedDemandGenerator{
			mint: func(ctx context.Context, got MultiHopId) (*MultiClientGeneratorClientArgs, error) {
				calls++
				if got != destination {
					t.Error("expiry changed the demanded destination")
				}
				<-ctx.Done()
				return &MultiClientGeneratorClientArgs{ClientId: id}, nil
			},
			remove: func(args *MultiClientGeneratorClientArgs) {
				if args == nil || args.ClientId != id {
					t.Error("expiry retired an unowned identity")
				}
				retired++
			},
		}
		window := outcomeEnumeratorTestWindow(t.Context(), newRecordingLogger(), generator)
		window.settings.WindowExpandTimeout = 3 * time.Second
		window.clientChannelArgs = make(chan *multiClientChannelArgs, 1)
		window.clientChannelArgs <- &multiClientChannelArgs{Destination: destination, FixedDestination: true, deferredClientArgs: true}
		started := time.Now()
		added := window.expand(WindowSizeSettings{WindowSizeMin: 1, WindowSizeMax: 1, WindowSizeHardMax: 1}, 0, 0, 1, 1, 1, 0)
		if added != 0 || calls != 1 || retired != 1 || generator.setupCalls.Load() != 0 || time.Since(started) != 3*time.Second {
			t.Fatalf("expired demand: added=%d mints=%d retired=%d setup=%d elapsed=%s", added, calls, retired, generator.setupCalls.Load(), time.Since(started))
		}
	})
}

// Discovery offers can outlive a sibling's admission. Discard an already
// healthy fixed destination without minting or spending the demanded slot.
func TestFixedCandidateStaleOfferKeepsMissingDestinationDemand(t *testing.T) {
	ctx := t.Context()
	activeDestination, missingDestination := RequireMultiHopId(NewId()), RequireMultiHopId(NewId())
	calls, retired := 0, 0
	generator := &fixedDemandGenerator{
		mint: func(_ context.Context, got MultiHopId) (*MultiClientGeneratorClientArgs, error) {
			calls++
			if got != missingDestination {
				t.Error("stale active destination minted a duplicate")
			}
			return &MultiClientGeneratorClientArgs{ClientId: NewId()}, nil
		},
		remove: func(*MultiClientGeneratorClientArgs) { retired++ },
	}
	window := outcomeEnumeratorTestWindow(ctx, newRecordingLogger(), generator)
	window.cancel = func() {}
	active := familyTestChannel(t, ctx, window.settings, IpFamilyV4Only)
	active.args.Destination = activeDestination
	window.clients[active.ClientId()] = active
	window.clientChannelArgs = make(chan *multiClientChannelArgs, 2)
	for _, destination := range []MultiHopId{activeDestination, missingDestination} {
		window.clientChannelArgs <- &multiClientChannelArgs{Destination: destination, FixedDestination: true, deferredClientArgs: true}
	}
	window.settings.EvaluationPoolMultiple = 1
	added := window.expand(WindowSizeSettings{WindowSizeMin: 2, WindowSizeMax: 2, WindowSizeHardMax: 2}, 1, 0, 2, 2, 1, 0)
	if added != 0 || calls != 1 || retired != 1 {
		t.Fatalf("stale offer consumed demand or changed failure cleanup: added=%d calls=%d retired=%d", added, calls, retired)
	}
}
