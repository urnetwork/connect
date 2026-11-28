package connect

// A provider's share limiter paces the bytes the provider returns to the
// clients it serves to a byte rate the provider's owner chose: the provider's
// maximum sharing speed (sdk provide share limits). The provider waits on it
// in its return sender (RemoteUserNatProvider.sendReturnItem), where waiting
// is allowed: a dedicated TCP socket reader that waits stops reading its
// upstream socket, so the flow slows to the rate through TCP flow control,
// and a datagram shard worker that waits fills its bounded queue, so later
// datagrams are refused and counted as on any congestion. No shared receive
// callback ever waits on it.
//
// The rate is a token bucket kept as a theoretical arrival time (the generic
// cell rate algorithm): each wait reserves its bytes at once by moving the
// arrival time on by the bytes' time at the rate, and waits for whatever of
// that is past the bucket's burst. Concurrent waits therefore queue in
// arrival order without polling, and a burst up to the bucket's size passes
// at once. The arithmetic is in whole nanoseconds, so a wait is exact. A rate
// change forgives the reservations and wakes every waiting one, which then
// proceeds: the new rate paces what follows.
//
// Safe for concurrent use. The owner keeps one limiter for the life of its
// provider role and passes it to each provider it builds, so a rate change
// reaches the provider in place.

import (
	"context"
	"sync"
	"time"
)

type ProviderShareLimiterSettings struct {
	// The bucket holds this long of the rate, so a burst up to it passes
	// without waiting. Non-positive holds only MinBurstByteCount.
	BurstDuration time.Duration
	// The bucket holds at least this many bytes at any rate, so a return of
	// common size never waits on its own reservation at a low rate.
	MinBurstByteCount ByteCount
}

// A fifth of a second of the rate, and at least 64 KiB: about one
// congestion window of a single fast flow, so pacing stays smooth without
// letting a burst run far past the rate.
func DefaultProviderShareLimiterSettings() *ProviderShareLimiterSettings {
	return &ProviderShareLimiterSettings{
		BurstDuration:     200 * time.Millisecond,
		MinBurstByteCount: 64 * 1024,
	}
}

type ProviderShareLimiter struct {
	settings *ProviderShareLimiterSettings

	stateLock sync.Mutex
	// bytes per second, 0 for no limit
	byteRate ByteCount
	// when every reserved byte is covered at the rate; a time before now is
	// a full bucket
	arrivalTime time.Time
	// closed and replaced on every rate change, which wakes the waiting
	// reservations
	rateChange chan struct{}

	// test seams; time.Now and time.After outside tests
	now   func() time.Time
	after func(delay time.Duration) <-chan time.Time
}

// A limiter with no limit. SetByteRate sets one.
func NewProviderShareLimiter(settings *ProviderShareLimiterSettings) *ProviderShareLimiter {
	return newProviderShareLimiterWithClock(settings, time.Now, time.After)
}

func newProviderShareLimiterWithClock(
	settings *ProviderShareLimiterSettings,
	now func() time.Time,
	after func(delay time.Duration) <-chan time.Time,
) *ProviderShareLimiter {
	return &ProviderShareLimiter{
		settings:   settings,
		rateChange: make(chan struct{}),
		now:        now,
		after:      after,
	}
}

// The fastest rate a limiter keeps, 1 GB/s. A faster one is kept as this, which
// holds every nanosecond product of the arithmetic inside int64.
const providerShareLimiterMaxByteRate = ByteCount(1000 * 1000 * 1000)

// Sets the rate in bytes per second; non-positive removes the limit. A change
// fills the bucket, forgives every reservation and wakes the waiting ones.
func (self *ProviderShareLimiter) SetByteRate(byteRate ByteCount) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	byteRate = min(max(byteRate, 0), providerShareLimiterMaxByteRate)
	if self.byteRate == byteRate {
		return
	}
	self.byteRate = byteRate
	self.arrivalTime = time.Time{}
	close(self.rateChange)
	self.rateChange = make(chan struct{})
}

// The rate in bytes per second, 0 for no limit.
func (self *ProviderShareLimiter) ByteRate() ByteCount {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.byteRate
}

// Takes byteCount bytes of the rate and waits until the rate covers them.
// True when the bytes may be sent: at once with no limit or while the bucket
// holds them, else after the wait or a rate change. False when ctx ends
// first; the bytes are then given back, so a canceled send does not slow the
// sends that follow it. A nil limiter never waits.
func (self *ProviderShareLimiter) Wait(ctx context.Context, byteCount ByteCount) bool {
	if self == nil || byteCount <= 0 {
		return true
	}
	delay, reserved, rateChange := func() (time.Duration, time.Duration, chan struct{}) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.byteRate <= 0 {
			return 0, 0, nil
		}
		now := self.now()
		reserved := self.byteDurationWithLock(byteCount)
		self.arrivalTime = self.arrivalTime.Add(reserved)
		if self.arrivalTime.Before(now.Add(reserved)) {
			// a full bucket at most
			self.arrivalTime = now.Add(reserved)
		}
		burst := self.byteDurationWithLock(self.burstByteCountWithLock())
		return self.arrivalTime.Sub(now.Add(burst)), reserved, self.rateChange
	}()
	if delay <= 0 {
		return true
	}
	select {
	case <-self.after(delay):
		return true
	case <-rateChange:
		return true
	case <-ctx.Done():
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			// a rate change since the reservation already forgave it
			if self.rateChange == rateChange {
				self.arrivalTime = self.arrivalTime.Add(-reserved)
			}
		}()
		return false
	}
}

// How long the rate takes to cover byteCount, in whole nanoseconds. Requires
// a rate.
func (self *ProviderShareLimiter) byteDurationWithLock(byteCount ByteCount) time.Duration {
	// whole seconds first, so the product stays far inside int64 for any count
	seconds := byteCount / self.byteRate
	remainder := byteCount % self.byteRate
	return time.Duration(seconds)*time.Second +
		time.Duration(remainder)*time.Second/time.Duration(self.byteRate)
}

// The bucket's size at the current rate.
func (self *ProviderShareLimiter) burstByteCountWithLock() ByteCount {
	burst := self.settings.MinBurstByteCount
	if 0 < self.settings.BurstDuration {
		rateBurst := ByteCount(int64(self.byteRate) * int64(self.settings.BurstDuration) / int64(time.Second))
		burst = max(burst, rateBurst)
	}
	return burst
}
