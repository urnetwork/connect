// The provider share limiter's pacing, driven by a fixed clock and a timer the
// test fires, so every wait it asks for is exact and nothing sleeps.
package connect

import (
	"context"
	"testing"
	"time"
)

// A limiter test clock: now is fixed until advanced, and every wait the
// limiter asks for is recorded with a timer the test fires.
type providerShareLimiterTestClock struct {
	t       *testing.T
	nowTime time.Time
	waits   chan time.Duration
	fire    chan time.Time
}

func newProviderShareLimiterTestClock(t *testing.T) *providerShareLimiterTestClock {
	return &providerShareLimiterTestClock{
		t:       t,
		nowTime: time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC),
		waits:   make(chan time.Duration, 16),
		fire:    make(chan time.Time),
	}
}

func (self *providerShareLimiterTestClock) now() time.Time {
	return self.nowTime
}

func (self *providerShareLimiterTestClock) after(delay time.Duration) <-chan time.Time {
	select {
	case self.waits <- delay:
	default:
		self.t.Error("provider share limiter test clock recorded too many waits")
	}
	return self.fire
}

// The limiter at rate bytes per second, with a 200 ms bucket of at least 64 KiB.
func newProviderShareLimiterForTest(clock *providerShareLimiterTestClock, rate ByteCount) *ProviderShareLimiter {
	limiter := newProviderShareLimiterWithClock(DefaultProviderShareLimiterSettings(), clock.now, clock.after)
	limiter.SetByteRate(rate)
	return limiter
}

func requireProviderShareLimiterWait(t *testing.T, clock *providerShareLimiterTestClock, want time.Duration) {
	t.Helper()
	select {
	case delay := <-clock.waits:
		if delay != want {
			t.Fatalf("limiter wait = %s, want %s", delay, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("limiter never asked to wait")
	}
}

func requireProviderShareLimiterNoWait(t *testing.T, clock *providerShareLimiterTestClock) {
	t.Helper()
	select {
	case delay := <-clock.waits:
		t.Fatalf("limiter asked to wait %s, want no wait", delay)
	default:
	}
}

// Without a rate nothing waits, however much is sent.
func TestProviderShareLimiterWithoutRateNeverWaits(t *testing.T) {
	clock := newProviderShareLimiterTestClock(t)
	limiter := newProviderShareLimiterForTest(clock, 0)
	for range 4 {
		if !limiter.Wait(context.Background(), 1024*1024*1024) {
			t.Fatal("unlimited wait refused")
		}
	}
	requireProviderShareLimiterNoWait(t, clock)
	if limiter.ByteRate() != 0 {
		t.Fatalf("rate = %d, want none", limiter.ByteRate())
	}
}

// A nil limiter is the provider's default and never waits.
func TestProviderShareLimiterNilNeverWaits(t *testing.T) {
	var limiter *ProviderShareLimiter
	if !limiter.Wait(context.Background(), 1024*1024) {
		t.Fatal("nil limiter refused")
	}
}

// The bucket passes a burst of 200 ms of the rate at once, and the bytes past
// it wait exactly as long as the rate needs to cover them.
func TestProviderShareLimiterPacesPastTheBurst(t *testing.T) {
	clock := newProviderShareLimiterTestClock(t)
	// 1,000,000 bytes/s: the bucket is 200 ms, 200,000 bytes
	limiter := newProviderShareLimiterForTest(clock, 1000*1000)
	if !limiter.Wait(context.Background(), 200*1000) {
		t.Fatal("burst refused")
	}
	requireProviderShareLimiterNoWait(t, clock)

	// the bucket is empty, so 1,000,000 bytes more wait their whole second
	done := make(chan bool, 1)
	go func() {
		done <- limiter.Wait(context.Background(), 1000*1000)
	}()
	requireProviderShareLimiterWait(t, clock, time.Second)
	select {
	case <-done:
		t.Fatal("paced wait returned before its timer")
	default:
	}
	clock.fire <- clock.nowTime
	if !<-done {
		t.Fatal("paced wait refused after its timer")
	}
}

// Time refills the bucket at the rate, up to its size and no further.
func TestProviderShareLimiterRefillsAtTheRateUpToTheBurst(t *testing.T) {
	clock := newProviderShareLimiterTestClock(t)
	// 1,000,000 bytes/s: the bucket is 200,000 bytes
	limiter := newProviderShareLimiterForTest(clock, 1000*1000)
	if !limiter.Wait(context.Background(), 200*1000) {
		t.Fatal("burst refused")
	}
	clock.nowTime = clock.nowTime.Add(100 * time.Millisecond)
	// 100 ms refilled 100,000 bytes
	if !limiter.Wait(context.Background(), 100*1000) {
		t.Fatal("refilled bytes refused")
	}
	requireProviderShareLimiterNoWait(t, clock)

	// an hour refills only the bucket's 200,000 bytes; 50,000 bytes past it
	// wait 50 ms
	clock.nowTime = clock.nowTime.Add(time.Hour)
	done := make(chan bool, 1)
	go func() {
		done <- limiter.Wait(context.Background(), 250*1000)
	}()
	requireProviderShareLimiterWait(t, clock, 50*time.Millisecond)
	clock.fire <- clock.nowTime
	if !<-done {
		t.Fatal("paced wait refused after its timer")
	}
}

// A wait its context ends gives its bytes back, so the next wait is paced as
// if the canceled one had never reserved anything.
func TestProviderShareLimiterCanceledWaitGivesItsBytesBack(t *testing.T) {
	clock := newProviderShareLimiterTestClock(t)
	limiter := newProviderShareLimiterForTest(clock, 1000*1000)
	if !limiter.Wait(context.Background(), 200*1000) {
		t.Fatal("burst refused")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		done <- limiter.Wait(ctx, 500*1000)
	}()
	requireProviderShareLimiterWait(t, clock, 500*time.Millisecond)
	cancel()
	if <-done {
		t.Fatal("canceled wait reported its bytes sendable")
	}

	// with the 500,000 bytes given back, 10,000 bytes wait 10 ms, not 510 ms
	go func() {
		done <- limiter.Wait(context.Background(), 10*1000)
	}()
	requireProviderShareLimiterWait(t, clock, 10*time.Millisecond)
	clock.fire <- clock.nowTime
	if !<-done {
		t.Fatal("paced wait refused after its timer")
	}
}

// Lifting the limit releases a reservation that is waiting for the old rate.
func TestProviderShareLimiterRateChangeReleasesWaits(t *testing.T) {
	clock := newProviderShareLimiterTestClock(t)
	limiter := newProviderShareLimiterForTest(clock, 1000*1000)
	if !limiter.Wait(context.Background(), 200*1000) {
		t.Fatal("burst refused")
	}
	done := make(chan bool, 1)
	go func() {
		done <- limiter.Wait(context.Background(), 10*1000*1000)
	}()
	requireProviderShareLimiterWait(t, clock, 10*time.Second)
	limiter.SetByteRate(0)
	select {
	case sendable := <-done:
		if !sendable {
			t.Fatal("released wait refused")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lifting the limit left the wait waiting")
	}
	if !limiter.Wait(context.Background(), 1024*1024*1024) {
		t.Fatal("unlimited wait refused")
	}
	requireProviderShareLimiterNoWait(t, clock)
}

// A new rate starts with a full bucket and no debt from the old one.
func TestProviderShareLimiterRateChangeForgivesDebt(t *testing.T) {
	clock := newProviderShareLimiterTestClock(t)
	limiter := newProviderShareLimiterForTest(clock, 1000*1000)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan bool, 1)
	go func() {
		done <- limiter.Wait(ctx, 2*1000*1000)
	}()
	requireProviderShareLimiterWait(t, clock, 1800*time.Millisecond)
	// 2,000,000 bytes/s: a bucket of 400,000 bytes, all of it available
	limiter.SetByteRate(2 * 1000 * 1000)
	if !<-done {
		t.Fatal("released wait refused")
	}
	if !limiter.Wait(context.Background(), 400*1000) {
		t.Fatal("new bucket refused")
	}
	requireProviderShareLimiterNoWait(t, clock)
}
