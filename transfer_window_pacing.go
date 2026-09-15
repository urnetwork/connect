package connect

import (
	"context"
	"math"
	"sync"
	"time"
)

// H1 terminates at a relay; its socket can accept a whole deep window before
// the forwarding queue serializes it. Limit bursts to two milliseconds of the
// measured/initial service rate, capped by the configured target wire rate.
// The window still owns flight and memory limits.
// One sequence owns this pacer, including recovery writes, and waits only once
// per burst. Idle time never accumulates permission for a window-sized burst.
type windowBurstPacer struct {
	service      *windowPacingService
	next         time.Time
	timer        *time.Timer
	rate         ByteCount
	rateUpdated  time.Time
	probeRate    ByteCount
	probeLimit   ByteCount
	probeSent    ByteCount
	serviceSent  ByteCount
	serviceAcked ByteCount
}

// A receiver's hold is memory capacity, not relay service capacity. Before
// delivery is measured, pace one initial window per residence. Then allow
// ten percent above recent service discovers spare capacity; queued service
// leaves a drain margin. The target caps both. A whole-flight startup average
// includes window-limited idle gaps and imposes a second slow-start ramp.
func windowPacingRate(estimate SendWindowEstimate, target ByteCount) ByteCount {
	if estimate.WindowRoundTrip <= 0 && estimate.ServiceByteRate <= 0 {
		return target
	}
	rate := float64(target)
	if estimate.WindowRoundTrip > 0 {
		rate = float64(estimate.Initial) / estimate.WindowRoundTrip.Seconds()
	}
	if estimate.ServiceByteRate > 0 {
		serviceRate := float64(estimate.ServiceByteRate)
		if estimate.ServiceBacklogged {
			// Leave service for draining an observed queue. Matching a
			// noisy estimate exactly can preserve it indefinitely.
			serviceRate *= .95
		} else {
			serviceRate *= 1.1
		}
		if estimate.ServiceEstablished {
			rate = serviceRate
		} else {
			// A few control bytes before the opening data window cannot
			// establish the service rate of a saturated path.
			rate = max(rate, serviceRate)
		}
	}
	if rate >= float64(target) {
		return target
	}
	return max(1, ByteCount(rate))
}

// Spend one bounded opening allowance discovering serialization capacity. A
// handshake can reveal RTT before bulk traffic; throttling that first data
// train to Initial/RTT would make the pacer measure its own slow startup.
// Charge a message crossing the probe boundary partly at each rate, and never
// replenish this allowance on idle or timer wakes.
func (self *windowBurstPacer) waitForService(ctx context.Context, byteCount int) error {
	return self.waitForServiceWrite(ctx, byteCount, false)
}

func (self *windowBurstPacer) waitForServiceWrite(ctx context.Context, byteCount int, resend bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if self.service != nil {
		if !resend {
			self.serviceSent += ByteCount(byteCount)
		}
		deadline := self.service.reserve(time.Now(), byteCount, self.rate, self.probeRate, self.probeLimit, resend)
		return self.waitUntil(ctx, deadline)
	}
	probe := min(ByteCount(byteCount), max(0, self.probeLimit-self.probeSent))
	if probe > 0 && self.probeRate > 0 {
		self.probeSent += probe
		if err := self.wait(ctx, int(probe), self.probeRate); err != nil {
			return err
		}
		byteCount -= int(probe)
	}
	if byteCount > 0 {
		return self.wait(ctx, byteCount, self.rate)
	}
	return nil
}

func (self *windowBurstPacer) wait(ctx context.Context, byteCount int, rate ByteCount) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	now := time.Now()
	// Keep small scheduling delays as credit toward the next burst. Reset
	// after a real idle period so catch-up stays bounded by one burst.
	if self.next.Before(now.Add(-2 * time.Millisecond)) {
		self.next = now
	}
	self.next = self.next.Add(time.Duration(float64(byteCount) * float64(time.Second) / float64(rate)))
	return self.waitUntil(ctx, self.next)
}

// Each producer owns its timer; the shared service lock is never held while
// a producer waits. A canceled reservation costs at most that one message.
func (self *windowBurstPacer) waitUntil(ctx context.Context, deadline time.Time) error {
	if delay := time.Until(deadline); delay > 2*time.Millisecond {
		if self.timer == nil {
			self.timer = time.NewTimer(0)
		}
		self.timer.Reset(delay)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-self.timer.C:
		}
	}
	return nil
}

// Logical sequences to one destination share serialization capacity and one
// opening probe. References are protected by SendBuffer.mutex; all timing and
// delivery methods below are safe for concurrent use.
type windowPacingService struct {
	stateLock       sync.Mutex
	references      int
	next            time.Time
	probeSent       ByteCount
	samples         [deliveredBytesRingSize]windowServiceSample
	newestBucket    int64
	hasSamples      bool
	total           ByteCount
	sent            ByteCount
	minRoundTrip    time.Duration
	latestRoundTrip time.Duration
	compression     time.Duration
	lastRoundTrip   time.Time
	bucketInterval  time.Duration
}

// Concurrent sequence workers may apply ACKs out of order. Keep their bytes
// in their original arrival intervals, with each interval's last arrival.
type windowServiceSample struct {
	bucket       int64
	firstAtNanos int64
	firstBytes   ByteCount
	lastAtNanos  int64
	bytes        ByteCount
}

// Reserve one message atomically so concurrent sequences cannot each spend
// the same burst or opening-window bytes. Idle never replenishes the probe.
func (self *windowPacingService) reserve(now time.Time, byteCount int, rate, probeRate, probeLimit ByteCount, resend bool) time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !resend {
		self.sent += ByteCount(byteCount)
	}
	if self.next.Before(now.Add(-2 * time.Millisecond)) {
		self.next = now
	}
	probe := min(ByteCount(byteCount), max(0, probeLimit-self.probeSent))
	if probeRate > 0 && probe > 0 {
		self.probeSent += probe
		self.next = self.next.Add(time.Duration(float64(probe) * float64(time.Second) / float64(probeRate)))
		byteCount -= int(probe)
	}
	if byteCount > 0 && rate > 0 {
		self.next = self.next.Add(time.Duration(float64(byteCount) * float64(time.Second) / float64(rate)))
	}
	return self.next
}

// First delivery across all of this service's sequences, sampled on one
// common clock. Cumulative release of an already SACKed suffix adds no bytes.
func (self *windowPacingService) observe(bytes ByteCount, at time.Time) {
	if bytes <= 0 {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.total += bytes
	bucket := at.UnixNano() / int64(max(deliverySizedWindowSampleInterval, self.bucketInterval))
	if self.hasSamples && bucket <= self.newestBucket-int64(len(self.samples)) {
		return
	}
	if !self.hasSamples || self.newestBucket < bucket {
		self.newestBucket = bucket
	}
	self.hasSamples = true
	index := (bucket%int64(len(self.samples)) + int64(len(self.samples))) % int64(len(self.samples))
	sample := &self.samples[index]
	if sample.bucket != bucket {
		*sample = windowServiceSample{bucket: bucket}
	}
	if sample.bytes == 0 || at.UnixNano() < sample.firstAtNanos {
		sample.firstAtNanos = at.UnixNano()
		sample.firstBytes = bytes
	} else if at.UnixNano() == sample.firstAtNanos {
		sample.firstBytes += bytes
	}
	if sample.bytes == 0 || at.UnixNano() > sample.lastAtNanos {
		sample.lastAtNanos = at.UnixNano()
	}
	sample.bytes += bytes
}

// The bounded peak excludes idle gaps; the latest positive sample is a
// conservative fallback when no recent delivery can refresh the estimate.
func (self *windowPacingService) measured(horizon time.Duration, now time.Time) (ByteCount, ByteCount, ByteCount) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	rate, latest := ByteCount(0), ByteCount(0)
	var samples [deliveredBytesRingSize]*windowServiceSample
	count := 0
	interval := max(deliverySizedWindowSampleInterval, self.bucketInterval)
	// Preserve a rate long enough to receive feedback from sends using it.
	// A queued RTT must not extend an old fast sample's life after a slowdown.
	horizon = min(horizon, max(4*interval, self.minRoundTrip+self.compression+2*interval))
	cutoff := now.Add(-horizon).UnixNano()
	for offset := int64(0); offset < int64(len(self.samples)); offset++ {
		bucket := self.newestBucket - offset
		index := (bucket%int64(len(self.samples)) + int64(len(self.samples))) % int64(len(self.samples))
		sample := &self.samples[index]
		if sample.bucket != bucket || sample.bytes <= 0 {
			continue
		}
		samples[count] = sample
		count++
	}
	minSpan := int64(self.compression)
	byteRate := func(bytes ByteCount, span int64) ByteCount {
		value := float64(bytes) * float64(time.Second) / float64(span)
		measured := ByteCount(math.MaxInt64)
		if value < float64(math.MaxInt64) {
			measured = ByteCount(value)
		}
		return measured
	}
	observeRate := func(bytes ByteCount, span, atNanos int64) {
		measured := byteRate(bytes, span)
		if latest == 0 {
			latest = measured
		}
		if horizon > 0 && atNanos >= cutoff {
			rate = max(rate, measured)
		}
	}
	for i := 0; i < count; i++ {
		newer := samples[i]
		if latest > 0 && newer.lastAtNanos < cutoff {
			break
		}
		// With immediate ACKs a small peer window can fit entirely inside
		// one bucket. Its first/last arrivals still measure serialization.
		if span := newer.lastAtNanos - newer.firstAtNanos; span > 0 && span >= minSpan {
			observeRate(newer.bytes-newer.firstBytes, span, newer.lastAtNanos)
		}
		bytes := newer.bytes
		for j := i + 1; j < count; j++ {
			span := newer.lastAtNanos - samples[j].lastAtNanos
			if span < minSpan && newer.lastAtNanos-samples[j].firstAtNanos >= minSpan {
				// The first checkpoint preserves a short opening train
				// whose last checkpoint is too close to the next bucket.
				span = newer.lastAtNanos - samples[j].firstAtNanos
				bytes += samples[j].bytes - samples[j].firstBytes
			}
			if span > 0 && span >= minSpan {
				observeRate(bytes, span, newer.lastAtNanos)
				break
			}
			bytes += samples[j].bytes
		}
	}
	// Once residence proves a queue, average a full feedback interval and
	// several compression turns. Repeated ACK peaks cannot drain that queue.
	if count > 1 && self.minRoundTrip > 0 && self.latestRoundTrip > self.minRoundTrip+self.compression+2*time.Millisecond {
		newer := samples[0]
		bytes := newer.bytes
		minSpan := int64(max(self.minRoundTrip, 4*self.compression, 4*interval))
		for j := 1; j < count; j++ {
			// A window-limited gap cannot establish sustained service.
			// Keep discovery from the active train until a whole feedback
			// interval has continuous delivery at the new offered rate.
			if samples[j-1].firstAtNanos-samples[j].lastAtNanos > int64(self.compression+2*interval) {
				break
			}
			span := newer.lastAtNanos - samples[j].lastAtNanos
			if span >= minSpan {
				measured := byteRate(bytes, span)
				flight := float64(measured) * (self.minRoundTrip + self.compression + 2*time.Millisecond).Seconds()
				if float64(max(0, self.sent-self.total)) > flight {
					latest = measured
					if horizon > 0 && newer.lastAtNanos >= cutoff {
						rate = measured
					}
				}
				break
			}
			bytes += samples[j].bytes
		}
	}
	return rate, self.total, latest
}

// This clock uses local ACK arrival, excluding pacing and worker waits.
// Quiet services replace their old path baseline on resume.
func (self *windowPacingService) observeRoundTrip(roundTrip, compression time.Duration, at time.Time) {
	if roundTrip <= 0 {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if at.Before(self.lastRoundTrip) {
		return
	}
	if self.minRoundTrip == 0 || roundTrip < self.minRoundTrip || at.Sub(self.lastRoundTrip) >= time.Minute {
		self.minRoundTrip = roundTrip
	}
	self.lastRoundTrip = at
	self.latestRoundTrip = roundTrip
	self.compression = max(0, compression)
	slots := time.Duration(len(self.samples) - 2)
	interval := max(deliverySizedWindowSampleInterval, self.compression/4, (self.minRoundTrip+self.compression+slots-1)/slots)
	if interval != max(deliverySizedWindowSampleInterval, self.bucketInterval) {
		clear(self.samples[:])
		self.hasSamples = false
		self.newestBucket = 0
	}
	self.bucketInterval = interval
}

// Before one service residence has been delivered, excess flight can still be
// a fast opening train in propagation. Require queue-delay evidence then.
func (self *windowPacingService) backlogged(rate ByteCount) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.minRoundTrip <= 0 || rate <= 0 {
		return false
	}
	bound := float64(rate) * (self.minRoundTrip + self.compression + 2*time.Millisecond).Seconds()
	if float64(self.total) < bound && self.latestRoundTrip <= self.minRoundTrip+self.compression+2*time.Millisecond {
		return false
	}
	return float64(max(0, self.sent-self.total)) > bound
}

func (self *windowBurstPacer) close() {
	if self.timer != nil {
		self.timer.Stop()
	}
	if self.service != nil {
		self.service.stateLock.Lock()
		self.service.sent -= max(0, self.serviceSent-self.serviceAcked)
		self.service.stateLock.Unlock()
		self.serviceSent = 0
		self.serviceAcked = 0
	}
}
