package connect

import (
	"context"
	"errors"
	"maps"
	"net/netip"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests for the bridge that cancels one strategy call when the strategy's own
// context ends first: parallelEvalWithRouteHedge for a dial or GET, and
// serialEvalWithAttemptContext for a serial request.

// One evaluation path that owns a bridge.
type contextBridgeTestPath struct {
	name string
	eval func(
		strategy *ClientStrategy,
		ctx context.Context,
		eval func(context.Context, *clientDialer) *evalResult,
	) *evalResult
}

func contextBridgeTestPaths() []contextBridgeTestPath {
	return []contextBridgeTestPath{
		{
			name: "parallel",
			eval: func(
				strategy *ClientStrategy,
				ctx context.Context,
				eval func(context.Context, *clientDialer) *evalResult,
			) *evalResult {
				return strategy.parallelEval(ctx, false, eval)
			},
		},
		{
			name: "serial",
			eval: func(
				strategy *ClientStrategy,
				ctx context.Context,
				eval func(context.Context, *clientDialer) *evalResult,
			) *evalResult {
				return strategy.serialEval(ctx, eval, func(context.Context, *clientDialer) *evalResult {
					return &evalResult{err: errors.New("synthetic hello failure")}
				})
			},
		},
	}
}

// One proven route on a strategy living on `strategyCtx`. Both paths attempt a
// proven route synchronously, on the calling goroutine.
func newContextBridgeTestStrategy(strategyCtx context.Context) *ClientStrategy {
	settings := DefaultClientStrategySettings()
	settings.RequestTimeout = 30 * time.Minute
	dialer := &clientDialer{
		description:     "proven",
		minimumWeight:   1,
		successCount:    1,
		lastSuccessTime: time.Now(),
		settings:        settings,
	}
	return &ClientStrategy{
		ctx:               strategyCtx,
		log:               NewNoopLogger(),
		settings:          settings,
		dialers:           map[*clientDialer]bool{dialer: true},
		extenderIpSecrets: map[netip.Addr]string{},
	}
}

// An attempt that blocks until its context ends. `entered` closes when the
// attempt starts, and `attemptErrs` receives the error its context ended
// with. The test context bounds a bridge that never cancels the attempt.
func contextBridgeTestBlockingAttempt(testCtx context.Context) (
	eval func(context.Context, *clientDialer) *evalResult,
	entered <-chan struct{},
	attemptErrs <-chan error,
) {
	enteredSignal := make(chan struct{})
	var enterOnce sync.Once
	attemptErrChannel := make(chan error, 1)
	eval = func(evalCtx context.Context, _ *clientDialer) *evalResult {
		enterOnce.Do(func() {
			close(enteredSignal)
		})
		select {
		case <-evalCtx.Done():
		case <-testCtx.Done():
		}
		err := evalCtx.Err()
		select {
		case attemptErrChannel <- err:
		default:
		}
		if err == nil {
			err = errors.New("attempt outlived the test")
		}
		return &evalResult{err: err}
	}
	return eval, enteredSignal, attemptErrChannel
}

// The calling goroutine's id, from the header of its own trace
// ("goroutine 7 [running]:").
func contextBridgeTestGoroutineId() string {
	trace := make([]byte, 64)
	trace = trace[:runtime.Stack(trace, false)]
	goroutineId, _, _ := strings.Cut(strings.TrimPrefix(string(trace), "goroutine "), " ")
	return goroutineId
}

// The traces of the live goroutines that goroutine `creatorGoroutineId`
// started. Each trace names its creator on its "created by" line.
func contextBridgeTestGoroutinesStartedBy(creatorGoroutineId string) []string {
	traces := make([]byte, 64*1024)
	for {
		n := runtime.Stack(traces, true)
		if n < len(traces) {
			traces = traces[:n]
			break
		}
		traces = make([]byte, 2*len(traces))
	}
	creatorSuffix := " in goroutine " + creatorGoroutineId
	startedTraces := []string{}
	for _, trace := range strings.Split(string(traces), "\n\n") {
		for _, line := range strings.Split(trace, "\n") {
			if strings.HasPrefix(line, "created by ") && strings.HasSuffix(line, creatorSuffix) {
				startedTraces = append(startedTraces, trace)
				break
			}
		}
	}
	return startedTraces
}

// A strategy context that counts the callbacks registered on it.
// context.AfterFunc schedules through a context's own AfterFunc method, so a
// bridge's registration and its stop both pass through here.
type contextBridgeTestLifetime struct {
	done chan struct{}

	stateLock      sync.Mutex
	err            error
	nextCallbackId int
	callbacks      map[int]func()
}

func newContextBridgeTestLifetime() *contextBridgeTestLifetime {
	return &contextBridgeTestLifetime{
		done:      make(chan struct{}),
		callbacks: map[int]func(){},
	}
}

func (self *contextBridgeTestLifetime) Deadline() (time.Time, bool) {
	return time.Time{}, false
}

func (self *contextBridgeTestLifetime) Done() <-chan struct{} {
	return self.done
}

func (self *contextBridgeTestLifetime) Err() error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.err
}

// Carries no values, so in particular no standard cancel context, which
// context.AfterFunc would register on directly instead.
func (self *contextBridgeTestLifetime) Value(key any) any {
	return nil
}

// Keeps the contract of context.AfterFunc: once canceled, f runs in its own
// goroutine, and stop reports whether it kept f from running.
func (self *contextBridgeTestLifetime) AfterFunc(f func()) func() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.err != nil {
		go f()
		return func() bool {
			return false
		}
	}
	callbackId := self.nextCallbackId
	self.nextCallbackId += 1
	self.callbacks[callbackId] = f
	return func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if _, ok := self.callbacks[callbackId]; !ok {
			return false
		}
		delete(self.callbacks, callbackId)
		return true
	}
}

func (self *contextBridgeTestLifetime) cancel() {
	callbacks := func() []func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.err != nil {
			return nil
		}
		self.err = context.Canceled
		close(self.done)
		callbacks := slices.Collect(maps.Values(self.callbacks))
		clear(self.callbacks)
		return callbacks
	}()
	for _, callback := range callbacks {
		go callback()
	}
}

func (self *contextBridgeTestLifetime) registrationCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.callbacks)
}

// A dial, GET or serial request used to start a goroutine that waited on the
// call's context and the strategy's to merge them, parked for the call's
// whole life. While the call's proven route is attempted, on the calling
// goroutine, that goroutine has started nothing.
func TestStrategyEvalParksNoGoroutineForTheStrategyContext(t *testing.T) {
	testCtx, testCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer testCancel()

	type attemptObservation struct {
		synchronous            bool
		startedGoroutineTraces []string
	}
	for _, path := range contextBridgeTestPaths() {
		strategyCtx, strategyCancel := context.WithCancel(context.Background())
		t.Cleanup(strategyCancel)
		strategy := newContextBridgeTestStrategy(strategyCtx)

		attemptObservations := make(chan attemptObservation, 1)
		results := make(chan *evalResult, 1)
		go func() {
			callerGoroutineId := contextBridgeTestGoroutineId()
			results <- path.eval(strategy, context.Background(), func(context.Context, *clientDialer) *evalResult {
				observation := attemptObservation{
					synchronous:            contextBridgeTestGoroutineId() == callerGoroutineId,
					startedGoroutineTraces: contextBridgeTestGoroutinesStartedBy(callerGoroutineId),
				}
				select {
				case attemptObservations <- observation:
				default:
				}
				return &evalResult{}
			})
		}()

		var observation attemptObservation
		select {
		case observation = <-attemptObservations:
		case <-testCtx.Done():
			t.Fatalf("%s: wait for the attempt: %v", path.name, testCtx.Err())
		}
		select {
		case result := <-results:
			if result == nil || result.err != nil {
				t.Errorf("%s: result = %#v, want the proven route's success", path.name, result)
			}
		case <-testCtx.Done():
			t.Fatalf("%s: wait for the call: %v", path.name, testCtx.Err())
		}
		if !observation.synchronous {
			t.Errorf("%s: the proven route was not attempted on the calling goroutine", path.name)
		}
		if n := len(observation.startedGoroutineTraces); n != 0 {
			t.Errorf(
				"%s: the call had started %d goroutine(s) by its attempt:\n%s",
				path.name,
				n,
				strings.Join(observation.startedGoroutineTraces, "\n\n"),
			)
		}
	}
}

// Canceling the strategy still cancels a call in flight: the attempt sees its
// context canceled, and the call returns no result.
func TestStrategyCancellationStillCancelsAnEvalInFlight(t *testing.T) {
	testCtx, testCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer testCancel()

	for _, path := range contextBridgeTestPaths() {
		strategyCtx, strategyCancel := context.WithCancel(context.Background())
		t.Cleanup(strategyCancel)
		strategy := newContextBridgeTestStrategy(strategyCtx)

		eval, entered, attemptErrs := contextBridgeTestBlockingAttempt(testCtx)
		results := make(chan *evalResult, 1)
		go func() {
			results <- path.eval(strategy, context.Background(), eval)
		}()
		select {
		case <-entered:
		case <-testCtx.Done():
			t.Fatalf("%s: wait for the attempt: %v", path.name, testCtx.Err())
		}

		strategyCancel()
		select {
		case err := <-attemptErrs:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("%s: attempt ended with %v, want canceled by the strategy", path.name, err)
			}
		case <-testCtx.Done():
			t.Fatalf("%s: the strategy's cancellation did not reach the attempt: %v", path.name, testCtx.Err())
		}
		select {
		case result := <-results:
			if result != nil {
				t.Errorf("%s: canceled call result = %#v, want none", path.name, result)
			}
		case <-testCtx.Done():
			t.Fatalf("%s: wait for the canceled call: %v", path.name, testCtx.Err())
		}
	}
}

// A call removes its registration from the strategy context before it
// returns, however it ends: canceled by its caller, successful, or canceled
// by the strategy. A strategy lives as long as its app, so a registration
// left per call would grow without bound and run every stale callback when
// the strategy closed. A canceled caller ends only its own call; the strategy
// stays live for the next one.
func TestStrategyEvalLeavesNoRegistrationOnTheStrategyContext(t *testing.T) {
	testCtx, testCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer testCancel()

	for _, path := range contextBridgeTestPaths() {
		lifetime := newContextBridgeTestLifetime()
		t.Cleanup(lifetime.cancel)
		strategy := newContextBridgeTestStrategy(lifetime)

		// the caller cancels
		callerCtx, callerCancel := context.WithCancel(context.Background())
		eval, entered, attemptErrs := contextBridgeTestBlockingAttempt(testCtx)
		results := make(chan *evalResult, 1)
		go func() {
			results <- path.eval(strategy, callerCtx, eval)
		}()
		select {
		case <-entered:
		case <-testCtx.Done():
			callerCancel()
			t.Fatalf("%s: wait for the attempt: %v", path.name, testCtx.Err())
		}
		if n := lifetime.registrationCount(); n != 1 {
			t.Errorf("%s: %d registration(s) on the strategy context during the call, want its bridge alone", path.name, n)
		}
		callerCancel()
		select {
		case err := <-attemptErrs:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("%s: attempt ended with %v, want canceled by the caller", path.name, err)
			}
		case <-testCtx.Done():
			t.Fatalf("%s: the caller's cancellation did not reach the attempt: %v", path.name, testCtx.Err())
		}
		select {
		case result := <-results:
			if result != nil {
				t.Errorf("%s: canceled call result = %#v, want none", path.name, result)
			}
		case <-testCtx.Done():
			t.Fatalf("%s: wait for the canceled call: %v", path.name, testCtx.Err())
		}
		if lifetime.Err() != nil {
			t.Errorf("%s: a canceled caller canceled the strategy", path.name)
		}
		if n := lifetime.registrationCount(); n != 0 {
			t.Errorf("%s: a call canceled by its caller left %d registration(s) on the strategy context", path.name, n)
		}

		// a success on the same, still live strategy
		result := path.eval(strategy, context.Background(), func(context.Context, *clientDialer) *evalResult {
			return &evalResult{}
		})
		if result == nil || result.err != nil {
			t.Errorf("%s: result after a canceled caller = %#v, want the proven route's success", path.name, result)
		}
		if n := lifetime.registrationCount(); n != 0 {
			t.Errorf("%s: a successful call left %d registration(s) on the strategy context", path.name, n)
		}

		// the strategy cancels
		eval, entered, attemptErrs = contextBridgeTestBlockingAttempt(testCtx)
		go func() {
			results <- path.eval(strategy, context.Background(), eval)
		}()
		select {
		case <-entered:
		case <-testCtx.Done():
			t.Fatalf("%s: wait for the attempt: %v", path.name, testCtx.Err())
		}
		lifetime.cancel()
		select {
		case err := <-attemptErrs:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("%s: attempt ended with %v, want canceled by the strategy", path.name, err)
			}
		case <-testCtx.Done():
			t.Fatalf("%s: the strategy's cancellation did not reach the attempt: %v", path.name, testCtx.Err())
		}
		select {
		case result := <-results:
			if result != nil {
				t.Errorf("%s: canceled call result = %#v, want none", path.name, result)
			}
		case <-testCtx.Done():
			t.Fatalf("%s: wait for the canceled call: %v", path.name, testCtx.Err())
		}
		if n := lifetime.registrationCount(); n != 0 {
			t.Errorf("%s: a call canceled by the strategy left %d registration(s) on the strategy context", path.name, n)
		}
	}
}
