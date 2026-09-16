package connect

import (
	"context"
	"math"
	"sync"
	"time"
)

// H1 terminates at a relay; its socket can accept a whole deep window before
// the forwarding queue serializes it. Shared bursts use one estimate's bytes
// and twice its duration; the target still caps the serialization rate.
// The window still owns flight and memory limits.
// One sequence owns this pacer, including recovery writes. Waiting at burst
// boundaries and actual dispatch bounds late wakes too. Idle time never
// accumulates permission for a window-sized burst.
type windowBurstPacer struct {
	service               *windowPacingService
	serviceSequenceId     Id
	timer                 *time.Timer
	rate                  ByteCount
	estimateRate          ByteCount
	rateUpdated           time.Time
	probeRate             ByteCount
	probeLimit            ByteCount
	serviceSent           ByteCount
	serviceAcked          ByteCount
	afterWaitForTest      func()
	afterAdmissionForTest func()
	waiter                windowPacingWaiter
}

// Each sequence reuses one queue entry. Service locking protects links;
// the buffered wakeup survives a handoff before the next producer waits.
type windowPacingWaiter struct {
	previous      *windowPacingWaiter
	next          *windowPacingWaiter
	ready         chan struct{}
	burst         uint64
	sentAt        time.Time
	deadline      time.Time
	serialization time.Duration
}

// Production writes register identity during the locked pacing handoff.
// Standalone timing callers have no physical message to register.
type windowPacingWriteStart struct {
	sequenceId Id
	messageId  Id
	number     uint64
}

// A burst may take longer than the measurement it came from while service
// changes. Its byte allowance is never multiplied along with that duration.
const windowPacingBurstTimeScale = 2

// A residence change may pause new writes briefly to obtain a drained probe.
// Loss cannot extend that pause indefinitely or trigger it on every write.
const (
	windowPacingDrainMaximumTime     = time.Second
	windowPacingDrainMinimumInterval = 5 * time.Second
)

// Saturate a duration product so a valid long measurement cannot wrap into
// an expired burst. Both idle-credit expiry and burst completion use it.
func windowPacingBurstMaximumTime(interval time.Duration) time.Duration {
	if interval > time.Duration(math.MaxInt64/windowPacingBurstTimeScale) {
		return time.Duration(math.MaxInt64)
	}
	return interval * windowPacingBurstTimeScale
}

// A configured rate and a physical byte count cannot wrap a future deadline
// into the past. Duration rounding costs less than one nanosecond per write.
func windowPacingSerializationTime(bytes, rate ByteCount) time.Duration {
	if bytes <= 0 || rate <= 0 {
		return 0
	}
	span := float64(bytes) * float64(time.Second) / float64(rate)
	if span >= float64(math.MaxInt64) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(span)
}

// One burst's reservation state is shared by all producers of the service.
// Deadlines retain serialization already earned during a late timer wake;
// byte limits keep that time from becoming a larger individual burst.
type windowPacingBurst struct {
	start  time.Time
	bytes  ByteCount
	number uint64
}

// The caller supplies a measured byte/time pair and an independently charged
// serialization deadline. The estimate includes a one-message minimum so an
// indivisible physical write always fits its byte ceiling.
func (self *windowPacingBurst) reserve(now time.Time, byteCount, estimateBytes ByteCount, estimateTime time.Duration, before time.Time) time.Time {
	limit := max(1, estimateBytes)
	if byteCount > limit {
		panic("pacing burst estimate is smaller than its physical message")
	}
	maximumTime := windowPacingBurstMaximumTime(estimateTime)
	if self.start.IsZero() || now.Sub(self.start) >= maximumTime || self.bytes > limit-byteCount {
		self.start, self.bytes = before, 0
		self.number++
	}
	self.bytes += byteCount
	return self.start
}

// Dispatch can combine reservations from several nominal bursts. Meter their
// actual release too, with one shared byte allowance and rate-based refill.
// The enclosing service owns synchronization; estimate updates retain spent
// bytes, and idle time fills at most one allowance.
type windowPacingBurstMeter struct {
	at           time.Time
	available    float64
	spent        float64
	paidUntil    time.Time
	reducedLimit ByteCount
	limit        ByteCount
	rate         ByteCount
}

// Refill at the old rate before replacing the estimate; a rate increase cannot
// retrospectively earn more credit for an earlier interval.
func (self *windowPacingBurstMeter) update(now time.Time, limit, rate ByteCount) {
	if now.Before(self.at) {
		now = self.at
	}
	if self.at.IsZero() {
		self.available = float64(limit)
	} else {
		self.refill(now)
		if limit < self.limit && self.spent > 0 {
			if self.reducedLimit == 0 {
				self.reducedLimit = limit
			} else {
				self.reducedLimit = min(self.reducedLimit, limit)
			}
		}
		self.available = min(self.available, max(0, float64(limit)-self.spent))
	}
	self.at, self.limit, self.rate = now, limit, max(1, rate)
}

// Measurement reads do not create credit; only elapsed wall time does.
func (self *windowPacingBurstMeter) refill(now time.Time) {
	if self.at.Before(now) {
		earned := now.Sub(self.at).Seconds() * float64(self.rate)
		self.spent = max(0, self.spent-earned)
		if self.spent == 0 {
			self.reducedLimit = 0
		}
		self.available = min(max(0, float64(self.limit)-self.spent), self.available+earned)
		self.at = now
	}
}

// Return zero only after consuming every byte of this write's allowance.
func (self *windowPacingBurstMeter) wait(now time.Time, bytes ByteCount) time.Duration {
	self.refill(now)
	if bytes > self.limit {
		panic("pacing release estimate is smaller than its physical message")
	}
	charge := float64(bytes)
	if charge <= self.available {
		self.available -= charge
		self.spent += charge
		return 0
	}
	// A smaller estimate can owe more than one new allowance. Preserve that
	// debt; later growth cannot mistake previously withheld credit for spending.
	missing := max(charge-self.available, charge+self.spent-float64(self.limit))
	delay := missing * float64(time.Second) / float64(self.rate)
	if delay >= float64(math.MaxInt64) {
		return time.Duration(math.MaxInt64)
	}
	return max(time.Nanosecond, time.Duration(math.Ceil(delay)))
}

// A later burst may retain payment at the old rates only after that service
// has also elapsed since actual release. Delayed releases cannot borrow an
// already passed nominal deadline, and increases never earn fresh credit.
func (self *windowPacingBurstMeter) paidReservation(now, deadline time.Time) {
	if self.reducedLimit > 0 && !now.Before(self.paidUntil) && !now.Before(deadline) {
		self.available = max(self.available, float64(min(self.limit, self.reducedLimit)))
		self.spent, self.reducedLimit, self.at = 0, 0, now
	}
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
	return self.waitForServiceWriteStarted(ctx, byteCount, resend, nil)
}

// Register an actual write before advancing the FIFO and releasing reserved
// bytes, so an intervening ACK cannot credit work still in this local handoff.
func (self *windowBurstPacer) waitForServiceWriteStarted(ctx context.Context, byteCount int, resend bool, start *windowPacingWriteStart) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if self.service == nil {
		self.service = &windowPacingService{}
	}
	if !resend {
		self.serviceSent += ByteCount(byteCount)
	}
	deadline := self.service.reserve(time.Now(), byteCount, self.rate, self.estimateRate, self.probeRate, self.probeLimit, resend, &self.waiter)
	err := self.waitUntil(ctx, deadline)
	if err == nil {
		err = self.service.waitForTurn(ctx, &self.waiter)
	}
	for err == nil {
		now := time.Now()
		delay, update := self.service.admitBurst(now, ByteCount(byteCount), resend, &self.waiter)
		if delay <= 0 {
			break
		}
		err = self.waitUntilChanged(ctx, now.Add(delay), update)
	}
	self.service.stateLock.Lock()
	if err == nil && start != nil {
		self.waiter.sentAt = time.Now()
		self.service.beginWriteWithLock(start.sequenceId, start.messageId, start.number, self.waiter.sentAt, resend)
	}
	self.service.removeWaiterWithLock(&self.waiter)
	self.service.pacingReservations--
	if err != nil && self.service.drained && self.service.pacingReservations == 0 {
		self.service.sourceIdleAt = time.Now()
	}
	if !resend {
		self.service.reservedByteCount -= ByteCount(byteCount)
	}
	self.service.stateLock.Unlock()
	if err == nil && self.afterAdmissionForTest != nil {
		self.afterAdmissionForTest()
	}
	return err
}

// Register the physical message at the pacing boundary so shared delivery
// accounting can distinguish an admitted write from earlier acknowledged work.
func (self *windowBurstPacer) waitForServiceMessage(ctx context.Context, byteCount int, resend bool, sequenceId, messageId Id, number uint64) error {
	start := windowPacingWriteStart{sequenceId: sequenceId, messageId: messageId, number: number}
	return self.waitForServiceWriteStarted(ctx, byteCount, resend, &start)
}

// Standalone users share the same byte/time policy through a private service.
func (self *windowBurstPacer) wait(ctx context.Context, byteCount int, rate ByteCount) error {
	self.rate = rate
	return self.waitForService(ctx, byteCount)
}

// Each producer owns its timer; the shared service lock is never held while
// a producer waits. A canceled reservation costs at most that one message.
func (self *windowBurstPacer) waitUntil(ctx context.Context, deadline time.Time) error {
	return self.waitUntilChanged(ctx, deadline, nil)
}

// Tail delivery can end a bounded drain before its timer. Timer-dispatch
// instrumentation applies only when a timer actually releases the wait.
func (self *windowBurstPacer) waitUntilChanged(ctx context.Context, deadline time.Time, update <-chan struct{}) error {
	if delay := time.Until(deadline); delay > 0 {
		if self.timer == nil {
			self.timer = time.NewTimer(0)
		}
		self.timer.Reset(delay)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-update:
			return ctx.Err()
		case <-self.timer.C:
		}
		if self.afterWaitForTest != nil {
			self.afterWaitForTest()
		}
	}
	return ctx.Err()
}

// Logical sequences to one destination share serialization capacity and one
// opening probe. References are protected by SendBuffer.mutex; all timing and
// delivery methods below are safe for concurrent use.
type windowPacingService struct {
	stateLock           sync.Mutex
	references          int
	next                time.Time
	burst               windowPacingBurst
	dispatchBurst       windowPacingBurst
	burstEstimateTime   time.Duration
	burstMeter          windowPacingBurstMeter
	waiterHead          *windowPacingWaiter
	waiterTail          *windowPacingWaiter
	probeSent           ByteCount
	samples             [deliveredBytesRingSize]windowServiceSample
	newestBucket        int64
	hasSamples          bool
	serviceEpochAt      time.Time
	serviceHoldRate     ByteCount
	total               ByteCount
	sent                ByteCount
	reservedByteCount   ByteCount
	pacingReservations  int
	maxMessageByteCount ByteCount
	minRoundTrip        time.Duration
	latestRoundTrip     time.Duration
	compression         time.Duration
	lastRoundTrip       time.Time
	roundTripStats      windowBurstStats
	drainCheckAt        time.Time
	drainUntil          time.Time
	drainWake           chan struct{}
	drainServiceEpoch   bool
	sourceIdleAt        time.Time
	bucketInterval      time.Duration
	writes              map[Id]windowPacingWrite
	pendingWrites       int
	drained             bool
	drainedSent         ByteCount
	drainGeneration     uint64
	roundTripProbe      windowPacingRoundTripProbe
}

// One tail per live sequence bounds tracking independently of window size.
// Both a successful H1 write and its cumulative ACK are required to drain it;
// they may be observed in either order for a synchronous route.
type windowPacingWrite struct {
	messageId   Id
	unambiguous bool
	written     bool
	acked       bool
	pending     bool
	generation  uint64
}

// The first write after every sibling's tail is cumulatively acknowledged
// measures the changed path. Keep its earliest covering ACK before send-loop
// coalescing can absorb that timestamp into a later head.
type windowPacingRoundTripProbe struct {
	sequenceId       Id
	messageId        Id
	number           uint64
	sentAt           time.Time
	ackedAt          time.Time
	compression      time.Duration
	written          bool
	serviceRate      ByteCount
	resetService     bool
	pendingByteCount ByteCount
}

// Records delivery while send workers are pacing. SACKs may complete the
// probe itself but cannot prove that an entire sequence's earlier writes drained.
func (self *windowPacingService) acknowledgeWrite(sequenceId, messageId Id, number uint64, selective bool, compression time.Duration, at time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if write, ok := self.writes[sequenceId]; ok && write.messageId == messageId && write.unambiguous && !selective {
		write.acked = true
		if write.written && write.pending {
			write.pending = false
			self.pendingWrites--
			self.drained = self.pendingWrites == 0 && write.generation == self.drainGeneration
			if self.drained {
				self.drainedSent = self.sent - self.reservedByteCount
				if self.pacingReservations == 0 {
					self.sourceIdleAt = time.Now()
				}
			}
		}
		self.writes[sequenceId] = write
		self.notifyDrainWithLock()
	}
	probe := &self.roundTripProbe
	if !probe.sentAt.IsZero() && probe.sequenceId == sequenceId &&
		((!selective && probe.number <= number) || probe.messageId == messageId) &&
		(probe.ackedAt.IsZero() || at.Before(probe.ackedAt)) {
		probe.ackedAt, probe.compression = at, compression
		self.applyRoundTripProbeWithLock()
	}
}

// Called after pacing, immediately before exposing a physical write. Only
// the first new write after an acknowledged burst can refresh the RTT floor.
// A retransmission cannot prove which physical copy an ACK has covered.
func (self *windowPacingService) beginWrite(sequenceId, messageId Id, number uint64, at time.Time, resend bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.beginWriteWithLock(sequenceId, messageId, number, at, resend)
}

// Enrollment shares the release lock in production. Confirmation still
// comes from the actual writer, including its chosen carrier and any failure.
func (self *windowPacingService) beginWriteWithLock(sequenceId, messageId Id, number uint64, at time.Time, resend bool) {
	unqueued := self.drained && !resend
	self.drained = false
	self.sourceIdleAt = time.Time{}
	if self.writes == nil {
		self.writes = map[Id]windowPacingWrite{}
	}
	if !self.writes[sequenceId].pending {
		self.pendingWrites++
	}
	self.writes[sequenceId] = windowPacingWrite{messageId: messageId, unambiguous: !resend, pending: true, generation: self.drainGeneration}
	if unqueued {
		self.roundTripProbe = windowPacingRoundTripProbe{sequenceId: sequenceId, messageId: messageId, number: number, sentAt: at, serviceRate: self.serviceHoldRate, resetService: self.drainServiceEpoch, pendingByteCount: max(0, self.drainedSent-self.total)}
	} else if resend && self.roundTripProbe.messageId == messageId {
		self.roundTripProbe = windowPacingRoundTripProbe{}
	}
	// The pause belongs to this first resumed write, including retries.
	// A later naturally empty flight must retain its serialization samples.
	self.drainServiceEpoch = false
}

// Recovery can bypass H1 pacing after a carrier change. Invalidate at the
// common write boundary, before either copy can return an ambiguous ACK.
func (self *windowPacingService) invalidateProbe(sequenceId Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.drainServiceEpoch = false
	self.sourceIdleAt = time.Time{}
	if self.roundTripProbe.sequenceId == sequenceId {
		self.roundTripProbe = windowPacingRoundTripProbe{}
	}
	if write, ok := self.writes[sequenceId]; ok {
		write.unambiguous = false
		self.writes[sequenceId] = write
		self.drained = false
	}
}

// The carrier actually used, including a route change during the writer call,
// must confirm H1 before either a tail or a probe can establish a drained relay.
func (self *windowPacingService) finishWrite(sequenceId, messageId Id, h1 bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if write, ok := self.writes[sequenceId]; ok && write.messageId == messageId {
		write.written = h1
		write.unambiguous = write.unambiguous && h1
		if write.written && write.unambiguous && write.acked && write.pending {
			write.pending = false
			self.pendingWrites--
			self.drained = self.pendingWrites == 0 && write.generation == self.drainGeneration
			if self.drained {
				self.drainedSent = self.sent - self.reservedByteCount
				if self.pacingReservations == 0 {
					self.sourceIdleAt = time.Now()
				}
			}
		}
		self.writes[sequenceId] = write
	}
	self.notifyDrainWithLock()
	if self.roundTripProbe.sequenceId == sequenceId && self.roundTripProbe.messageId == messageId {
		if h1 {
			self.roundTripProbe.written = true
			self.applyRoundTripProbeWithLock()
		} else {
			self.roundTripProbe = windowPacingRoundTripProbe{}
		}
	}
}

// Applying the earliest valid covering ACK is independent of whether the
// physical writer or the ACK worker returned first.
func (self *windowPacingService) applyRoundTripProbeWithLock() {
	probe := self.roundTripProbe
	if probe.written && !probe.ackedAt.IsZero() {
		self.roundTripProbe = windowPacingRoundTripProbe{}
		// A drained pause contains local idle, not serialization. Keep
		// the established rate until the resumed train measures service.
		if probe.resetService {
			self.serviceEpochAt, self.serviceHoldRate = probe.ackedAt, probe.serviceRate
		}
		self.observeRoundTripWithLock(probe.ackedAt.Sub(probe.sentAt), probe.compression, probe.ackedAt, true)
	}
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
func (self *windowPacingService) reserve(now time.Time, byteCount int, rate, estimateRate, probeRate, probeLimit ByteCount, resend bool, waiter *windowPacingWaiter) time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	// A source with no physical flight or pacing demand can pause for
	// inner feedback. Its first isolated reply measures that local gap,
	// not serialization. Observe demand before reservation/timer waits.
	if !resend && self.drained && self.pacingReservations == 0 && !self.sourceIdleAt.IsZero() &&
		now.Sub(self.sourceIdleAt) >= max(deliverySizedWindowSampleInterval, self.bucketInterval, self.compression) {
		self.drainServiceEpoch = true
	}
	self.sourceIdleAt = time.Time{}
	if waiter.ready == nil {
		waiter.ready = make(chan struct{}, 1)
	}
	select {
	case <-waiter.ready:
	default:
	}
	waiter.previous, waiter.next = self.waiterTail, nil
	if self.waiterTail == nil {
		self.waiterHead = waiter
	} else {
		self.waiterTail.next = waiter
	}
	self.waiterTail = waiter
	if self.sent <= self.total && self.pacingReservations == 0 {
		self.maxMessageByteCount = ByteCount(byteCount)
	} else {
		self.maxMessageByteCount = max(self.maxMessageByteCount, ByteCount(byteCount))
	}
	self.pacingReservations++
	if !resend {
		self.sent += ByteCount(byteCount)
		self.reservedByteCount += ByteCount(byteCount)
	}
	interval := max(deliverySizedWindowSampleInterval, self.bucketInterval)
	messageBytes := ByteCount(byteCount)
	probe := min(ByteCount(byteCount), max(0, probeLimit-self.probeSent))
	hasEstimate := estimateRate > 0
	if estimateRate <= 0 {
		estimateRate = rate
	}
	refillRate := rate
	if probeRate > 0 && probe > 0 {
		if !hasEstimate {
			estimateRate = probeRate
		}
		refillRate = probeRate
	}
	bytes := float64(estimateRate) * interval.Seconds()
	estimateBytes := ByteCount(math.MaxInt64)
	if bytes < float64(math.MaxInt64) {
		estimateBytes = max(1, ByteCount(bytes))
	}
	largerThanSample := estimateBytes < messageBytes
	if estimateBytes < self.maxMessageByteCount {
		// Quantize the estimate to one physical message, and preserve its
		// byte/time rate. A larger-than-sample write pays serialization first.
		estimateBytes = self.maxMessageByteCount
		span := math.Ceil(float64(estimateBytes) * float64(time.Second) / float64(max(1, estimateRate)))
		if span >= float64(math.MaxInt64) {
			interval = time.Duration(math.MaxInt64)
		} else {
			interval = max(interval, time.Duration(span))
		}
	}
	if self.next.Before(now.Add(-windowPacingBurstMaximumTime(interval))) {
		self.next = now
	}
	before := self.next
	if probeRate > 0 && probe > 0 {
		self.probeSent += probe
		self.next = self.next.Add(windowPacingSerializationTime(probe, probeRate))
		byteCount -= int(probe)
	}
	self.next = self.next.Add(windowPacingSerializationTime(ByteCount(byteCount), rate))
	waiter.serialization = self.next.Sub(before)
	if largerThanSample {
		before = self.next
	}
	self.burstMeter.update(now, estimateBytes, refillRate)
	self.burstEstimateTime = interval
	deadline := self.burst.reserve(now, messageBytes, estimateBytes, interval, before)
	waiter.deadline = deadline
	return deadline
}

// Only the oldest reservation may spend release credit. Waiting producers
// keep their own deadline and cancellation without allocating per write.
func (self *windowPacingService) waitForTurn(ctx context.Context, waiter *windowPacingWaiter) error {
	self.stateLock.Lock()
	first := self.waiterHead == waiter
	self.stateLock.Unlock()
	if !first {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-waiter.ready:
		}
	}
	return ctx.Err()
}

// Cancellation removes even an interior reservation in constant time. A
// released head hands the byte meter to exactly its oldest live successor.
func (self *windowPacingService) removeWaiterWithLock(waiter *windowPacingWaiter) {
	if waiter.previous == nil {
		self.waiterHead = waiter.next
	} else {
		waiter.previous.next = waiter.next
	}
	if waiter.next == nil {
		self.waiterTail = waiter.previous
	} else {
		waiter.next.previous = waiter.previous
	}
	if waiter.previous == nil && waiter.next != nil {
		select {
		case waiter.next.ready <- struct{}{}:
		default:
		}
	}
	waiter.previous, waiter.next = nil, nil
}

// The lock is released before waiting, so siblings and ACK delivery continue
// while a late producer waits for the next shared byte allowance.
func (self *windowPacingService) admitBurst(now time.Time, bytes ByteCount, resend bool, waiter *windowPacingWaiter) (time.Duration, <-chan struct{}) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !resend {
		if self.pendingWrites == 0 || !now.Before(self.drainUntil) {
			self.drainUntil = time.Time{}
		}
		if self.drainUntil.IsZero() && self.pendingWrites > 0 && !now.Before(self.drainCheckAt) && self.roundTripStats.ring != nil {
			mean, count := self.roundTripStats.ring.mean(now)
			residence := time.Duration(min(float64(windowPacingDrainMaximumTime), mean))
			threshold := float64(self.minRoundTrip) + float64(self.compression) + float64(max(2*time.Millisecond, self.minRoundTrip/4))
			if count > 0 && mean > threshold {
				// Residence is only a reason to test for a changed path.
				// The cumulative tail ACK still has to prove an empty relay.
				span := max(4*deliverySizedWindowSampleInterval, 2*min(windowPacingDrainMaximumTime/2, residence))
				self.drainUntil = now.Add(span)
				self.drainServiceEpoch = true
				self.drainCheckAt = now.Add(max(windowPacingDrainMinimumInterval, 8*span))
				if self.drainWake == nil {
					self.drainWake = make(chan struct{}, 1)
				}
			}
		}
		if now.Before(self.drainUntil) {
			return self.drainUntil.Sub(now), self.drainWake
		}
	}
	self.burstMeter.paidReservation(now, waiter.deadline)
	delay := self.burstMeter.wait(now, bytes)
	if delay == 0 {
		// Keep the split probe's original serialization cost as well as
		// its actual release time. A later decrease cannot reprice it.
		if self.burstMeter.paidUntil.Before(now) {
			self.burstMeter.paidUntil = now
		}
		self.burstMeter.paidUntil = self.burstMeter.paidUntil.Add(waiter.serialization)
		self.dispatchBurst.reserve(now, bytes, self.burstMeter.limit, self.burstEstimateTime, now)
		waiter.burst = self.dispatchBurst.number
	} else if self.burstMeter.reducedLimit > 0 && !now.Before(waiter.deadline) && now.Before(self.burstMeter.paidUntil) {
		delay = min(delay, self.burstMeter.paidUntil.Sub(now))
	}
	return delay, nil
}

// Only one FIFO head waits for a drain, so one reusable buffered wakeup is
// enough. Cancellation cannot manufacture the drained flag required by a probe.
func (self *windowPacingService) notifyDrainWithLock() {
	if self.pendingWrites == 0 && !self.drainUntil.IsZero() && self.drainWake != nil {
		select {
		case self.drainWake <- struct{}{}:
		default:
		}
	}
}

// First delivery across all of this service's sequences, sampled on one
// common clock. Cumulative release of an already SACKed suffix adds no bytes.
func (self *windowPacingService) observe(bytes ByteCount, at time.Time) {
	if bytes <= 0 {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if probe := &self.roundTripProbe; probe.resetService && !at.After(probe.sentAt) {
		// Only older arrivals complete the drained train's missing samples.
		// Current delivery must not spend that older byte credit.
		probe.pendingByteCount = max(0, probe.pendingByteCount-bytes)
	}
	self.total += bytes
	if at.Before(self.serviceEpochAt) {
		return
	}
	bucket := at.UnixNano() / int64(max(deliverySizedWindowSampleInterval, self.bucketInterval))
	if self.hasSamples && bucket <= self.newestBucket-int64(len(self.samples)) {
		return
	}
	if !self.hasSamples || self.newestBucket < bucket {
		self.newestBucket = bucket
	}
	self.hasSamples = true
	if bucket < self.newestBucket {
		// A smaller interval cannot split an existing aggregate exactly.
		// Merge reordered ACKs into its real span, avoiding overlapping sums.
		for offset := int64(0); offset < int64(len(self.samples)); offset++ {
			retainedBucket := self.newestBucket - offset
			index := (retainedBucket%int64(len(self.samples)) + int64(len(self.samples))) % int64(len(self.samples))
			sample := &self.samples[index]
			if sample.bucket == retainedBucket && sample.bytes > 0 && sample.firstAtNanos <= at.UnixNano() && at.UnixNano() <= sample.lastAtNanos {
				bucket = retainedBucket
				break
			}
		}
	}
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
	return self.measure(horizon, now, true)
}

// A statistics reader computes the same estimate without advancing the hold
// later captured by physical probes. Only controller reads retain evidence.
func (self *windowPacingService) measure(horizon time.Duration, now time.Time, retain bool) (ByteCount, ByteCount, ByteCount) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	rate, latest := ByteCount(0), ByteCount(0)
	epochAt, hold := self.serviceEpochAt, self.serviceHoldRate
	if probe := self.roundTripProbe; probe.resetService {
		// Tail ACKs can prove delivery before their coalesced bytes apply.
		// A partial old train cannot replace its held service during that gap.
		boundary := probe.ackedAt
		if boundary.IsZero() && probe.pendingByteCount > 0 {
			boundary = probe.sentAt.Add(time.Nanosecond)
		}
		if epochAt.Before(boundary) {
			epochAt, hold = boundary, probe.serviceRate
		}
	}
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
		if sample.bucket != bucket || sample.bytes <= 0 || !epochAt.IsZero() && sample.firstAtNanos < epochAt.UnixNano() {
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
		queued := self.outstandingWithLock() > self.flightBoundWithLock(rate)
		maximumGap := self.compression + 2*interval
		if queued {
			// Current flight alone cannot rewrite an earlier window idle.
			// Excess residence must also cover a sparse serializer's gap.
			maximumGap = max(maximumGap, self.latestRoundTrip-self.minRoundTrip-self.compression)
		}
		for j := 1; j < count; j++ {
			// A window-limited gap cannot establish sustained service.
			// Keep discovery from the active train until a whole feedback
			// interval has continuous delivery at the new offered rate.
			if samples[j-1].firstAtNanos-samples[j].lastAtNanos > int64(maximumGap) {
				break
			}
			span := newer.lastAtNanos - samples[j].lastAtNanos
			if span >= minSpan {
				measured := byteRate(bytes, span)
				if self.outstandingWithLock() > self.flightBoundWithLock(measured) {
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
	if rate > 0 || latest > 0 {
		if retain {
			self.serviceHoldRate = rate
			if rate == 0 {
				self.serviceHoldRate = latest
			}
			if self.roundTripProbe.resetService {
				// A sibling may deliver fresh service before this probe's ACK.
				// Later confirmation cannot restore the superseded held rate.
				self.roundTripProbe.serviceRate = self.serviceHoldRate
			}
		}
	} else {
		latest = hold
	}
	return rate, self.total, latest
}

// This clock uses local ACK arrival, excluding pacing and worker waits.
// Quiet services replace their old path baseline on resume.
func (self *windowPacingService) observeRoundTrip(roundTrip, compression time.Duration, at time.Time) {
	self.observeBurstRoundTrip(0, roundTrip, compression, at)
}

// A new burst replaces the rolling ring with the preceding burst's mean as
// its hold value. This recent residence can trigger a drain, never raise RTT.
func (self *windowPacingService) observeBurstRoundTrip(burst uint64, roundTrip, compression time.Duration, at time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if roundTrip > 0 {
		if self.roundTripStats.ring == nil {
			self.roundTripStats.ring = newWindowBucketStats(deliverySizedWindowSampleInterval, 4)
		}
		self.roundTripStats.add(burst, float64(roundTrip), at)
	}
	self.observeRoundTripWithLock(roundTrip, compression, at, false)
}

// Shared physical feedback may replace a stale per-sequence minimum after a
// propagation increase, before the sequence has collected a new sample ring.
func (self *windowPacingService) roundTrip() time.Duration {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.minRoundTrip
}

// A write following an acknowledged burst can establish a larger propagation
// floor. Ordinary queued samples may lower the floor but cannot raise it.
func (self *windowPacingService) observeRoundTripWithLock(roundTrip, compression time.Duration, at time.Time, unqueued bool) {
	if roundTrip <= 0 {
		return
	}
	if at.Before(self.lastRoundTrip) {
		return
	}
	if self.minRoundTrip == 0 || roundTrip < self.minRoundTrip || unqueued || at.Sub(self.lastRoundTrip) >= time.Minute {
		self.minRoundTrip = roundTrip
	}
	self.lastRoundTrip = at
	self.latestRoundTrip = roundTrip
	self.compression = max(0, compression)
	slots := time.Duration(len(self.samples) - 2)
	interval := max(deliverySizedWindowSampleInterval, self.compression/4, (self.minRoundTrip+self.compression+slots-1)/slots)
	if interval != max(deliverySizedWindowSampleInterval, self.bucketInterval) {
		// Bucket widths are bookkeeping. Preserve real ACK timestamps so
		// a changed RTT cannot erase the pair that discovered new service.
		previous := self.samples
		newest := self.newestBucket
		clear(self.samples[:])
		self.hasSamples = false
		self.newestBucket = 0
		for offset := int64(0); offset < int64(len(previous)); offset++ {
			bucket := newest - offset
			index := (bucket%int64(len(previous)) + int64(len(previous))) % int64(len(previous))
			sample := previous[index]
			if sample.bucket != bucket || sample.bytes <= 0 || !self.serviceEpochAt.IsZero() && sample.firstAtNanos < self.serviceEpochAt.UnixNano() {
				continue
			}
			bucket = sample.lastAtNanos / int64(interval)
			if !self.hasSamples {
				self.hasSamples, self.newestBucket = true, bucket
			}
			if bucket <= self.newestBucket-int64(len(self.samples)) {
				continue
			}
			index = (bucket%int64(len(self.samples)) + int64(len(self.samples))) % int64(len(self.samples))
			destination := &self.samples[index]
			if destination.bytes == 0 {
				*destination = sample
				destination.bucket = bucket
				continue
			}
			if sample.firstAtNanos < destination.firstAtNanos {
				destination.firstAtNanos, destination.firstBytes = sample.firstAtNanos, sample.firstBytes
			} else if sample.firstAtNanos == destination.firstAtNanos {
				destination.firstBytes += sample.firstBytes
			}
			destination.lastAtNanos = max(destination.lastAtNanos, sample.lastAtNanos)
			destination.bytes += sample.bytes
		}
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
	bound := self.flightBoundWithLock(rate)
	if float64(max(self.total, self.drainedSent)) < bound && self.latestRoundTrip <= self.minRoundTrip+self.compression+2*time.Millisecond {
		return false
	}
	return self.outstandingWithLock() > bound
}

// Reserved bytes are still local. Cumulative delivery of every sibling tail
// supplies a lower bound on ACKed wire bytes before send-loop application;
// later application catches up to that bound without crediting it twice.
func (self *windowPacingService) outstandingWithLock() float64 {
	return float64(max(0, self.sent-max(self.total, self.drainedSent)-self.reservedByteCount))
}

// Physical flight contains propagation plus one permitted burst. Its byte
// allowance already includes the minimum of one indivisible message. Before
// the first reservation, retain the original two-millisecond rate allowance.
func (self *windowPacingService) flightBoundWithLock(rate ByteCount) float64 {
	burst := float64(max(self.maxMessageByteCount, self.burstMeter.limit))
	if self.burstMeter.limit <= 0 {
		burst += float64(rate) * (2 * time.Millisecond).Seconds()
	}
	return burst + float64(rate)*(self.minRoundTrip+self.compression).Seconds()
}

func (self *windowBurstPacer) close() {
	if self.timer != nil {
		self.timer.Stop()
	}
	if self.service != nil {
		self.service.stateLock.Lock()
		released := max(0, self.serviceSent-self.serviceAcked)
		self.service.sent -= released
		if released > 0 {
			// This removes unapplied owned bytes from the sent counter, so
			// its previous global delivery checkpoint no longer applies.
			self.service.drainedSent = 0
		}
		if write, ok := self.service.writes[self.serviceSequenceId]; ok {
			if write.pending {
				self.service.pendingWrites--
				// Cancellation releases memory, not physical delivery. An
				// older sibling ACK cannot certify the canceled tail drained.
				self.service.drainGeneration++
				self.service.drained = false
				self.service.sourceIdleAt = time.Time{}
			}
			delete(self.service.writes, self.serviceSequenceId)
		}
		if self.service.roundTripProbe.sequenceId == self.serviceSequenceId {
			self.service.roundTripProbe = windowPacingRoundTripProbe{}
		}
		self.service.notifyDrainWithLock()
		self.service.stateLock.Unlock()
		self.serviceSent = 0
		self.serviceAcked = 0
	}
}
