package connect

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// testOobControl is a mock OutOfBandControl: it fails the first `failUntil`
// SendControl attempts and then succeeds, recording the number of attempts. It
// mirrors ApiOutOfBandControl by consuming the frames it is given and invoking
// the callback asynchronously.
type testOobControl struct {
	stateLock sync.Mutex
	attempts  int
	failUntil int
}

type delayedOobAttempt struct {
	callback OobResultFunction
}

type delayedOobControl struct {
	attempts chan delayedOobAttempt
}

func (self *delayedOobControl) SendControl(frames []*protocol.Frame, callback OobResultFunction) {
	for _, frame := range frames {
		MessagePoolReturn(frame.MessageBytes)
	}
	self.attempts <- delayedOobAttempt{callback: callback}
}

type contextualDelayedOobAttempt struct {
	ctx      context.Context
	callback OobResultFunction
}

type contextualDelayedOobControl struct {
	attempts chan contextualDelayedOobAttempt
}

func (self *contextualDelayedOobControl) SendControl(frames []*protocol.Frame, callback OobResultFunction) {
	panic("context-aware path was not used")
}

func (self *contextualDelayedOobControl) SendControlWithCtx(
	ctx context.Context,
	frames []*protocol.Frame,
	callback OobResultFunction,
) {
	for _, frame := range frames {
		MessagePoolReturn(frame.MessageBytes)
	}
	self.attempts <- contextualDelayedOobAttempt{ctx: ctx, callback: callback}
}

func (self *testOobControl) setFailUntil(n int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.failUntil = n
}

func (self *testOobControl) attemptCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.attempts
}

func (self *testOobControl) SendControl(frames []*protocol.Frame, callback OobResultFunction) {
	// mimic ApiOutOfBandControl: the input frames are consumed
	for _, frame := range frames {
		MessagePoolReturn(frame.MessageBytes)
	}

	self.stateLock.Lock()
	self.attempts += 1
	attempt := self.attempts
	failUntil := self.failUntil
	self.stateLock.Unlock()

	go func() {
		if attempt <= failUntil {
			callback(nil, fmt.Errorf("simulated oob failure (attempt %d)", attempt))
		} else {
			// success: the oob returns (the platform processed the message)
			callback(nil, nil)
		}
	}()
}

func newControlSyncOobTestClient(ctx context.Context, oob OutOfBandControl) *Client {
	settings := DefaultClientSettings()
	// the oob path does not use the transport; disable the control ping so the
	// client does not spin trying to ping without a transport
	settings.ControlPingTimeout = 0
	return NewClient(ctx, NewId(), oob, settings)
}

// the oob ack fires once after the platform succeeds, retrying through
// transient failures
func TestControlSyncOobRetryUntilSuccess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping testing in short mode")
	}

	makeFrame := func(index uint32) *protocol.Frame {
		frame, err := ToFrame(&protocol.SimpleMessage{MessageIndex: index}, DefaultProtocolVersion)
		AssertEqual(t, err, nil)
		return frame
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	oob := &testOobControl{failUntil: 3}
	client := newControlSyncOobTestClient(ctx, oob)

	cs := NewControlSyncOob(ctx, client, "m1")
	// a fast fixed cadence: this test runs real retries
	cs.settings.RetryTimeout = 10 * time.Millisecond
	cs.settings.RetryMaxTimeout = 10 * time.Millisecond
	defer cs.Close()

	var ackLock sync.Mutex
	ackCount := 0
	ackCh := make(chan error, 1)
	cs.Send(makeFrame(1), func(err error) {
		ackLock.Lock()
		ackCount += 1
		ackLock.Unlock()
		select {
		case ackCh <- err:
		default:
		}
	})

	select {
	case err := <-ackCh:
		AssertEqual(t, err, nil)
	case <-time.After(10 * time.Second):
		t.Fatal("oob ack did not fire")
	}

	// 3 failures + 1 success
	AssertEqual(t, oob.attemptCount(), 4)

	// the ack fires exactly once
	select {
	case <-time.After(500 * time.Millisecond):
	}
	ackLock.Lock()
	AssertEqual(t, ackCount, 1)
	ackLock.Unlock()
}

// immediate success acks without retrying
func TestControlSyncOobImmediateSuccess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping testing in short mode")
	}

	makeFrame := func(index uint32) *protocol.Frame {
		frame, err := ToFrame(&protocol.SimpleMessage{MessageIndex: index}, DefaultProtocolVersion)
		AssertEqual(t, err, nil)
		return frame
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	oob := &testOobControl{failUntil: 0}
	client := newControlSyncOobTestClient(ctx, oob)

	cs := NewControlSyncOob(ctx, client, "m1")
	// a fast fixed cadence, should it retry
	cs.settings.RetryTimeout = 10 * time.Millisecond
	cs.settings.RetryMaxTimeout = 10 * time.Millisecond
	defer cs.Close()

	ackCh := make(chan error, 1)
	cs.Send(makeFrame(1), func(err error) {
		select {
		case ackCh <- err:
		default:
		}
	})

	select {
	case err := <-ackCh:
		AssertEqual(t, err, nil)
	case <-time.After(5 * time.Second):
		t.Fatal("oob ack did not fire")
	}
	AssertEqual(t, oob.attemptCount(), 1)
}

// a newer Send for the same scope supersedes an older one still retrying
func TestControlSyncOobLatestSupersedes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping testing in short mode")
	}

	makeFrame := func(index uint32) *protocol.Frame {
		frame, err := ToFrame(&protocol.SimpleMessage{MessageIndex: index}, DefaultProtocolVersion)
		AssertEqual(t, err, nil)
		return frame
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// keep failing so neither send completes until allowed
	oob := &testOobControl{failUntil: 1 << 30}
	client := newControlSyncOobTestClient(ctx, oob)

	cs := NewControlSyncOob(ctx, client, "m1")
	// a fast fixed cadence: this test runs real retries
	cs.settings.RetryTimeout = 10 * time.Millisecond
	cs.settings.RetryMaxTimeout = 10 * time.Millisecond
	defer cs.Close()

	firstAcked := make(chan struct{}, 1)
	cs.Send(makeFrame(1), func(err error) {
		select {
		case firstAcked <- struct{}{}:
		default:
		}
	})

	// let the first send fail a few times
	select {
	case <-time.After(100 * time.Millisecond):
	}

	// supersede with a newer send (still failing), then allow success — so
	// the first send is superseded while it can never succeed
	secondAcked := make(chan error, 1)
	cs.Send(makeFrame(2), func(err error) {
		select {
		case secondAcked <- err:
		default:
		}
	})
	oob.setFailUntil(0)

	select {
	case err := <-secondAcked:
		AssertEqual(t, err, nil)
	case <-time.After(5 * time.Second):
		t.Fatal("second oob ack did not fire")
	}

	// the superseded first send must not ack
	select {
	case <-firstAcked:
		t.Fatal("superseded send unexpectedly acked")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestControlSyncOobLateInflightSuccessCannotAckSupersededGeneration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	makeFrame := func(index uint32) *protocol.Frame {
		frame, err := ToFrame(&protocol.SimpleMessage{MessageIndex: index}, DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		return frame
	}
	oob := &delayedOobControl{attempts: make(chan delayedOobAttempt, 2)}
	client := newControlSyncOobTestClient(ctx, oob)
	defer client.Cancel()
	cs := NewControlSyncOob(ctx, client, "generation")
	defer cs.Close()

	firstAcked := make(chan struct{}, 1)
	cs.Send(makeFrame(1), func(error) {
		firstAcked <- struct{}{}
	})
	firstAttempt := <-oob.attempts

	secondAcked := make(chan struct{}, 1)
	cs.Send(makeFrame(2), func(error) {
		secondAcked <- struct{}{}
	})
	secondAttempt := <-oob.attempts

	// The first request was already handed to OOB when the second generation
	// superseded it. Its late success must not be observable as a current ack.
	firstAttempt.callback(nil, nil)
	select {
	case <-firstAcked:
		t.Fatal("late success acknowledged the superseded OOB generation")
	case <-time.After(50 * time.Millisecond):
	}

	secondAttempt.callback(nil, nil)
	select {
	case <-secondAcked:
	case <-time.After(time.Second):
		t.Fatal("current OOB generation did not acknowledge")
	}
}

// Success must claim and retire its generation before invoking arbitrary user
// code. Otherwise there is a check/act window: a newer Send can supersede the
// request after its "current" check but before the old ack is published.
func TestControlSyncOobSuccessClaimsGenerationBeforeAck(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	frame, err := ToFrame(&protocol.SimpleMessage{MessageIndex: 1}, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	oob := &delayedOobControl{attempts: make(chan delayedOobAttempt, 1)}
	client := newControlSyncOobTestClient(ctx, oob)
	defer client.Cancel()
	cs := NewControlSyncOob(ctx, client, "atomic-success")
	defer cs.Close()

	acked := make(chan error, 1)
	cs.Send(frame, func(error) {
		cs.sendLock.Lock()
		generationStillOwned := cs.currentCancel != nil
		cs.sendLock.Unlock()
		if generationStillOwned {
			acked <- errors.New("ack ran before the successful generation was claimed")
			return
		}
		acked <- nil
	})
	attempt := <-oob.attempts
	attempt.callback(nil, nil)

	select {
	case err := <-acked:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("success callback did not run")
	}
}

func TestControlSyncOobSupersessionCancelsProductionRequestContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	makeFrame := func(index uint32) *protocol.Frame {
		frame, err := ToFrame(&protocol.SimpleMessage{MessageIndex: index}, DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		return frame
	}
	oob := &contextualDelayedOobControl{
		attempts: make(chan contextualDelayedOobAttempt, 2),
	}
	client := newControlSyncOobTestClient(ctx, oob)
	defer client.Cancel()
	cs := NewControlSyncOob(ctx, client, "cancel-request")
	defer cs.Close()

	cs.Send(makeFrame(1), nil)
	first := <-oob.attempts
	cs.Send(makeFrame(2), nil)
	second := <-oob.attempts

	select {
	case <-first.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("superseding send did not cancel the old OOB request context")
	}
	select {
	case <-second.ctx.Done():
		t.Fatal("current OOB request was canceled with its predecessor")
	default:
	}

	second.callback(nil, nil)
	select {
	case <-second.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("successful current request did not retire its context")
	}
}

// Close stops retrying
func TestControlSyncOobCloseStopsRetries(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping testing in short mode")
	}

	makeFrame := func(index uint32) *protocol.Frame {
		frame, err := ToFrame(&protocol.SimpleMessage{MessageIndex: index}, DefaultProtocolVersion)
		AssertEqual(t, err, nil)
		return frame
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	oob := &testOobControl{failUntil: 1 << 30} // always fail
	client := newControlSyncOobTestClient(ctx, oob)

	cs := NewControlSyncOob(ctx, client, "m1")
	// a fast fixed cadence: this test runs real retries
	cs.settings.RetryTimeout = 10 * time.Millisecond
	cs.settings.RetryMaxTimeout = 10 * time.Millisecond

	acked := make(chan struct{}, 1)
	cs.Send(makeFrame(1), func(err error) {
		select {
		case acked <- struct{}{}:
		default:
		}
	})

	// let it retry a few times
	select {
	case <-time.After(100 * time.Millisecond):
	}
	attemptsBefore := oob.attemptCount()
	AssertEqual(t, 0 < attemptsBefore, true)

	cs.Close()

	select {
	case <-time.After(300 * time.Millisecond):
	}
	// retries stop promptly (allow a couple of in-flight attempts at close,
	// but not the ~30 that would accrue over the wait if it kept retrying)
	attemptsAfter := oob.attemptCount()
	AssertEqual(t, attemptsAfter <= attemptsBefore+2, true)

	// it never acked (always failing)
	select {
	case <-acked:
		t.Fatal("send acked despite always failing")
	default:
	}
}

// One retry wait a ControlSyncOob asked for. It fires only when the test sends
// on `fire`.
type controlSyncOobRetryWait struct {
	timeout time.Duration
	fire    chan time.Time
}

// Hands each retry wait of `cs` to the test in place of a timer.
func captureControlSyncOobRetryWaits(cs *ControlSyncOob) <-chan controlSyncOobRetryWait {
	waits := make(chan controlSyncOobRetryWait, 2)
	cs.retryAfterForTest = func(timeout time.Duration) <-chan time.Time {
		wait := controlSyncOobRetryWait{
			timeout: timeout,
			fire:    make(chan time.Time, 1),
		}
		waits <- wait
		return wait.fire
	}
	return waits
}

// A liveness deadline does not decide the asserted ordering.
func receiveControlSyncOobTestValue[T any](t *testing.T, values <-chan T, message string) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(10 * time.Second):
		t.Fatal(message)
		var empty T
		return empty
	}
}

// Fails `attempt` on its own goroutine, as the api's callback does. The
// returned channel closes when the callback, and with it the retry wait,
// returns.
func failControlSyncOobAttempt(attempt delayedOobAttempt) <-chan struct{} {
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		attempt.callback(nil, errors.New("synthetic control refusal"))
	}()
	return returned
}

func controlSyncOobTestFrame(t *testing.T, index uint32) *protocol.Frame {
	t.Helper()
	frame, err := ToFrame(&protocol.SimpleMessage{MessageIndex: index}, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

// A sync of a default client, with an oob whose every attempt the test answers
// and retry waits the test fires.
func newControlSyncOobBackoffTest(t *testing.T) (*ControlSyncOob, *Client, *delayedOobControl, <-chan controlSyncOobRetryWait) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	oob := &delayedOobControl{attempts: make(chan delayedOobAttempt, 1)}
	client := newControlSyncOobTestClient(ctx, oob)
	t.Cleanup(client.Cancel)
	cs := NewControlSyncOob(ctx, client, "backoff")
	t.Cleanup(cs.Close)
	return cs, client, oob, captureControlSyncOobRetryWaits(cs)
}

// Fails the send's next attempt and returns the retry wait it asked for. With
// `fire`, the wait fires and its callback returns, having started the next
// attempt's launcher, before this returns.
func failControlSyncOobAttemptAndWait(t *testing.T, oob *delayedOobControl, waits <-chan controlSyncOobRetryWait, fire bool) (controlSyncOobRetryWait, <-chan struct{}) {
	t.Helper()
	attempt := receiveControlSyncOobTestValue(t, oob.attempts, "oob attempt did not arrive")
	returned := failControlSyncOobAttempt(attempt)
	wait := receiveControlSyncOobTestValue(t, waits, "retry wait was not requested")
	if fire {
		wait.fire <- time.Now()
		receiveControlSyncOobTestValue(t, returned, "fired retry wait did not return")
	}
	return wait, returned
}

// The middle of each jitter range, so a wait is 3/4 of its nominal wait.
func controlSyncOobMiddleJitter(half time.Duration) time.Duration {
	return half / 2
}

// Consecutive failures of one send back off: the nominal wait doubles from
// RetryTimeout to RetryMaxTimeout (the defaults: 1 s to 30 s), and each wait
// is jittered within [d/2, d) of it. Every retry used to wait a fixed 1 s, so
// 1000 providers in one process retried about 1000 times a second, in
// lockstep, while the platform refused them
// (https://github.com/urnetwork/connect/issues/227).
func TestControlSyncOobRetryBacksOffExponentiallyToCap(t *testing.T) {
	cs, _, oob, waits := newControlSyncOobBackoffTest(t)
	cs.retryJitterForTest = controlSyncOobMiddleJitter

	cs.Send(controlSyncOobTestFrame(t, 1), nil)
	want := []time.Duration{
		750 * time.Millisecond,
		1500 * time.Millisecond,
		3 * time.Second,
		6 * time.Second,
		12 * time.Second,
		22500 * time.Millisecond,
		22500 * time.Millisecond,
		22500 * time.Millisecond,
	}
	got := []time.Duration{}
	for range want {
		wait, _ := failControlSyncOobAttemptAndWait(t, oob, waits, true)
		got = append(got, wait.timeout)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("retry waits = %v, want %v", got, want)
	}
}

// Drawn from math/rand, each wait stays in [d/2, d) of its nominal wait d:
// never over the cap, and spread below the nominal wait that clients refused
// at the same moment would otherwise share.
func TestControlSyncOobRetryJitterStaysWithinNominalRange(t *testing.T) {
	cs, _, oob, waits := newControlSyncOobBackoffTest(t)

	cs.Send(controlSyncOobTestFrame(t, 1), nil)
	nominals := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		30 * time.Second,
		30 * time.Second,
		30 * time.Second,
	}
	for i, nominal := range nominals {
		wait, _ := failControlSyncOobAttemptAndWait(t, oob, waits, true)
		if wait.timeout < nominal/2 || nominal <= wait.timeout {
			t.Errorf("failure %d waited %s, want in [%s, %s)", i+1, wait.timeout, nominal/2, nominal)
		}
	}
}

// A success ends its send's backoff: the next send's first failure waits from
// RetryTimeout again, however far the previous send had backed off.
func TestControlSyncOobSuccessResetsRetryBackoff(t *testing.T) {
	cs, _, oob, waits := newControlSyncOobBackoffTest(t)
	cs.retryJitterForTest = controlSyncOobMiddleJitter

	acked := make(chan error, 1)
	cs.Send(controlSyncOobTestFrame(t, 1), func(err error) {
		acked <- err
	})
	for _, want := range []time.Duration{750 * time.Millisecond, 1500 * time.Millisecond, 3 * time.Second, 6 * time.Second} {
		if wait, _ := failControlSyncOobAttemptAndWait(t, oob, waits, true); wait.timeout != want {
			t.Fatalf("backing off: waited %s, want %s", wait.timeout, want)
		}
	}
	attempt := receiveControlSyncOobTestValue(t, oob.attempts, "oob attempt did not arrive")
	attempt.callback(nil, nil)
	if err := receiveControlSyncOobTestValue(t, acked, "successful send did not acknowledge"); err != nil {
		t.Fatal(err)
	}

	cs.Send(controlSyncOobTestFrame(t, 2), nil)
	wait, returned := failControlSyncOobAttemptAndWait(t, oob, waits, false)
	if want := 750 * time.Millisecond; wait.timeout != want {
		t.Fatalf("the send after a success first waited %s, want %s", wait.timeout, want)
	}
	cs.Close()
	receiveControlSyncOobTestValue(t, returned, "closed sync kept waiting")
}

// A newer Send ends the older send's pending retry wait without another
// attempt, and its own first failure waits from RetryTimeout, not from where
// the older send had backed off to.
func TestControlSyncOobSupersessionStopsRetryAndRestartsBackoff(t *testing.T) {
	cs, _, oob, waits := newControlSyncOobBackoffTest(t)
	cs.retryJitterForTest = controlSyncOobMiddleJitter

	cs.Send(controlSyncOobTestFrame(t, 1), nil)
	for _, want := range []time.Duration{750 * time.Millisecond, 1500 * time.Millisecond} {
		if wait, _ := failControlSyncOobAttemptAndWait(t, oob, waits, true); wait.timeout != want {
			t.Fatalf("backing off: waited %s, want %s", wait.timeout, want)
		}
	}
	// the third wait stays pending until the newer send supersedes it
	olderWait, olderReturned := failControlSyncOobAttemptAndWait(t, oob, waits, false)
	if want := 3 * time.Second; olderWait.timeout != want {
		t.Fatalf("backing off: waited %s, want %s", olderWait.timeout, want)
	}

	cs.Send(controlSyncOobTestFrame(t, 2), nil)
	receiveControlSyncOobTestValue(t, olderReturned, "superseded send kept waiting")
	// the timer would fire now; nothing waits on it
	olderWait.fire <- time.Now()

	wait, returned := failControlSyncOobAttemptAndWait(t, oob, waits, false)
	if want := 750 * time.Millisecond; wait.timeout != want {
		t.Fatalf("the superseding send first waited %s, want %s", wait.timeout, want)
	}
	// the newer send is held at its wait, so any attempt now is the older send's
	select {
	case <-oob.attempts:
		t.Fatal("superseded send attempted again")
	default:
	}
	cs.Close()
	receiveControlSyncOobTestValue(t, returned, "closed sync kept waiting")
}

// Closing the sync, or its client, ends a pending retry wait at once, without
// another attempt, however long the backed-off wait.
func TestControlSyncOobStopEndsRetryWait(t *testing.T) {
	stops := []struct {
		name string
		stop func(cs *ControlSyncOob, client *Client)
	}{
		{name: "sync close", stop: func(cs *ControlSyncOob, _ *Client) { cs.Close() }},
		{name: "client cancel", stop: func(_ *ControlSyncOob, client *Client) { client.Cancel() }},
	}
	for _, s := range stops {
		cs, client, oob, waits := newControlSyncOobBackoffTest(t)
		cs.retryJitterForTest = controlSyncOobMiddleJitter

		cs.Send(controlSyncOobTestFrame(t, 1), nil)
		for range 5 {
			failControlSyncOobAttemptAndWait(t, oob, waits, true)
		}
		wait, returned := failControlSyncOobAttemptAndWait(t, oob, waits, false)
		if want := 22500 * time.Millisecond; wait.timeout != want {
			t.Fatalf("%s: backing off: waited %s, want %s", s.name, wait.timeout, want)
		}
		s.stop(cs, client)
		receiveControlSyncOobTestValue(t, returned, s.name+" did not end the retry wait")
		// the timer would fire now; nothing waits on it
		wait.fire <- time.Now()
		select {
		case <-oob.attempts:
			t.Fatalf("%s: retried after the stop", s.name)
		default:
		}
	}
}
