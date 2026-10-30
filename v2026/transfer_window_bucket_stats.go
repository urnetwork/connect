// Fixed-duration rolling measurements with zero-order hold between measured
// buckets. An explicit zero is a measurement; an empty bucket is not.
package connect

import (
	"math"
	"time"
)

// One bucket's mean is independent of how often the estimate is read.
type windowStatsBucket struct {
	number int64
	count  uint64
	mean   float64
}

// Retains m completed buckets and the current partial bucket, plus the last
// expired measured bucket as the hold value. Mean gives each known time bucket
// equal weight. Startup buckets before the first measurement remain unknown.
// The caller serializes access; observation times may arrive out of order,
// while estimate times advance monotonically.
type windowBucketStats struct {
	interval time.Duration
	buckets  []windowStatsBucket
	newest   int64
	started  bool
	held     float64
	hasHeld  bool
	resetAt  time.Time
}

// The extra slot keeps the current partial bucket outside the rolling mean.
func newWindowBucketStats(interval time.Duration, lookback int) *windowBucketStats {
	if interval <= 0 || lookback < 1 || lookback > 1024 {
		panic("invalid rolling bucket interval or lookback")
	}
	return &windowBucketStats{interval: interval, buckets: make([]windowStatsBucket, lookback+1)}
}

// Begin a new measurement epoch with the previous burst's summary as the
// hold value. Even a late sample in this same time bucket belongs to the old
// epoch when its observation predates the reset.
func (self *windowBucketStats) reset(value float64, at time.Time) bool {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return false
	}
	clear(self.buckets)
	self.newest, self.started = self.bucketNumber(at), true
	self.held, self.hasHeld, self.resetAt = value, true, at
	return true
}

// Mathematical floor also gives correct bucket boundaries before Unix epoch.
func (self *windowBucketStats) bucketNumber(at time.Time) int64 {
	nanos := at.UnixNano()
	number := nanos / int64(self.interval)
	if nanos%int64(self.interval) < 0 {
		number--
	}
	return number
}

// A long idle jump costs one scan of retained storage, independent of elapsed
// time. Expiration preserves the newest measured bucket's mean for holding.
func (self *windowBucketStats) advance(number int64) {
	if self.started && number <= self.newest {
		return
	}
	oldest := number - int64(len(self.buckets)-1)
	var lastExpired windowStatsBucket
	for i := range self.buckets {
		bucket := &self.buckets[i]
		if bucket.count != 0 && bucket.number < oldest {
			if lastExpired.count == 0 || lastExpired.number < bucket.number {
				lastExpired = *bucket
			}
			*bucket = windowStatsBucket{}
		}
	}
	if lastExpired.count != 0 {
		self.held, self.hasHeld = lastExpired.mean, true
	}
	self.started, self.newest = true, number
}

// Late observations can repair a retained bucket and its following held
// buckets. An expired observation cannot overwrite the current hold value.
func (self *windowBucketStats) add(value float64, at time.Time) bool {
	if !self.resetAt.IsZero() && at.Before(self.resetAt) {
		return false
	}
	return self.addRetained(value, at)
}

// An independently verified epoch may contain an earlier arrival than its
// first applied observation. Keep the same retention and finite-value guards;
// callers without that identity must use the reset timestamp through add.
func (self *windowBucketStats) addRetained(value float64, at time.Time) bool {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return false
	}
	number := self.bucketNumber(at)
	self.advance(number)
	if number < self.newest-int64(len(self.buckets)-1) {
		return false
	}
	index := (number%int64(len(self.buckets)) + int64(len(self.buckets))) % int64(len(self.buckets))
	bucket := &self.buckets[index]
	if bucket.count == 0 {
		bucket.number = number
	}
	bucket.count++
	weight := 1 / float64(bucket.count)
	bucket.mean = bucket.mean*(1-weight) + value*weight
	return true
}

// Each acknowledged burst starts a new rolling ring, seeded by the preceding
// burst's mean. Multiple bursts may be in flight: an older burst can still
// release ownership, but cannot overwrite newer measurement evidence.
// The service serializes access. Storage and reset cost are fixed by lookback.
type windowBurstStats struct {
	ring      *windowBucketStats
	burst     uint64
	count     uint64
	burstMean float64
	lastAt    time.Time
}

// Keep one completed burst as the zero-order hold until completed buckets
// from this burst supply new evidence. Partial buckets never change the mean.
func (self *windowBurstStats) add(burst uint64, value float64, at time.Time) bool {
	if math.IsNaN(value) || math.IsInf(value, 0) || (self.count != 0 && burst < self.burst) {
		return false
	}
	if self.count != 0 && self.burst < burst {
		if at.Before(self.lastAt) {
			return false
		}
		self.ring.reset(self.burstMean, at)
		self.count = 0
	}
	// The burst id identifies this epoch even when workers apply a current
	// sample that arrived before the first one that triggered its reset.
	if !self.ring.addRetained(value, at) {
		return false
	}
	self.burst = burst
	if self.lastAt.Before(at) {
		self.lastAt = at
	}
	self.count++
	weight := 1 / float64(self.count)
	self.burstMean = self.burstMean*(1-weight) + value*weight
	return true
}

// Returns the mean of the last m completed n-duration buckets and how many
// have measured or held values. A zero count means no completed evidence.
// Reading repeatedly never creates observations or changes their weighting.
func (self *windowBucketStats) mean(at time.Time) (float64, int) {
	number := self.bucketNumber(at)
	if self.started && number < self.newest {
		return 0, 0
	}
	self.advance(number)
	value, known := self.held, self.hasHeld
	mean, count := float64(0), 0
	for number := self.newest - int64(len(self.buckets)-1); number < self.newest; number++ {
		index := (number%int64(len(self.buckets)) + int64(len(self.buckets))) % int64(len(self.buckets))
		bucket := self.buckets[index]
		if bucket.count != 0 && bucket.number == number {
			value, known = bucket.mean, true
		}
		if known {
			count++
			weight := 1 / float64(count)
			mean = mean*(1-weight) + value*weight
		}
	}
	return mean, count
}
