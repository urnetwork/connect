package connect

import (
	"context"
	"errors"
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
	serviceClosed         bool
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
	// A window smaller than a full target-rate compressed feedback interval
	// relies on coalescing to earn timely cumulative ACKs. Keep its existing
	// burst; only better-provisioned flows can smooth release independently.
	holdCompressionBurst bool
}

func windowPacingHoldCompressionBurst(estimate SendWindowEstimate, compression time.Duration) bool {
	ceiling := estimate.Ceiling
	if ceiling <= 0 {
		ceiling = estimate.Window
	}
	if ceiling <= 0 || estimate.PacingProbeByteRate <= 0 || compression <= 0 {
		return true
	}
	return float64(ceiling) < float64(estimate.PacingProbeByteRate)*compression.Seconds()
}

// Production writes register identity during the locked pacing handoff.
// Standalone timing callers have no physical message to register.
type windowPacingWriteStart struct {
	sequenceId Id
	messageId  Id
	number     uint64
	owner      *SendSequence
	recovery   bool
	// Only selectors with a generation-bound dispatch may bypass this wait.
	routePolicyEnabled    bool
	reliableOnlyRequested bool
	writeDeadline         time.Time
	writeReadyOnly        bool
}

// Standalone pacing has no message lifetime; production uses the send owner's
// earliest retained deadline, including older messages and an active retry.
func (self *windowPacingWriteStart) lifetime(now time.Time) (time.Time, error) {
	if self == nil || self.owner == nil {
		return time.Time{}, nil
	}
	if self.recovery && self.owner.ackWindow != nil && self.owner.ackWindow.pendingDeliveryFor(self.number, self.messageId) {
		return time.Time{}, errWindowPacingAcknowledged
	}
	deadline, err := self.owner.nextAckLifetime(now)
	if err != nil {
		return deadline, err
	}
	if !self.writeDeadline.IsZero() {
		if !now.Before(self.writeDeadline) && !self.writeReadyOnly {
			return self.writeDeadline, errTransferRouteWriteTimeout
		}
		if deadline.IsZero() || self.writeDeadline.Before(deadline) {
			deadline = self.writeDeadline
		}
	}
	return deadline, nil
}

// Every park uses one immutable level/notification pair. A replacement that
// precedes subscription is already visible; an H1 replacement keeps its debt.
func (self *windowPacingWriteStart) pacingRoute() (<-chan struct{}, error) {
	if self == nil || self.owner == nil || !self.routePolicyEnabled {
		return nil, nil
	}
	policy := self.owner.transferFlightPolicy()
	if !self.owner.h1PacingWrite(policy, self.reliableOnlyRequested) {
		return policy.notify, errTransferRoutePolicyChanged
	}
	return policy.notify, nil
}

// Call-local state retains one charged reservation across carrier revalidation.
// Paid meter credit and write enrollment are never consumed a second time.
type windowPacingReservation struct {
	started   bool
	active    bool
	metered   bool
	complete  bool
	byteCount int
	resend    bool
}

// A burst may take longer than the measurement it came from while service
// changes. Its byte allowance is never multiplied along with that duration.
const windowPacingBurstTimeScale = 2

// A short-RTT service must not turn the 10 ms measurement bucket into a
// 10 ms FIFO release: reverse-direction ACKs would sit behind that whole
// burst. One quarter of the measurement bucket bounds timer overhead while
// preserving useful batches; one quarter of longer RTTs permits larger ones.
const windowPacingMinimumBurstTime = deliverySizedWindowSampleInterval / 4

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

// A receiver's hold grants memory. Startup paces one initial window per
// residence; a fresh qualified cumulative interval can replace that fallback
// until physical service is known. Ten percent above recent delivery discovers
// spare capacity; queued service leaves a drain margin. The target caps both.
//
// Two floors keep measured delivery from being read as a capacity limit it
// has not proved. Before this service has observed queueing, the pace is at
// least the admitted window over one residence, because the only thing
// delivery measured was the pacer's own previous release. After an admitting
// read has granted a pace, that pace is held until a backlogged read or a
// recovery write supplies congestion evidence.
func windowPacingRate(estimate SendWindowEstimate, target ByteCount) ByteCount {
	if estimate.WindowRoundTrip <= 0 && estimate.ServiceByteRate <= 0 && estimate.AggregateDeliveryByteRate <= 0 {
		return target
	}
	rate := float64(target)
	if estimate.WindowRoundTrip > 0 {
		rate = float64(estimate.Initial) / estimate.WindowRoundTrip.Seconds()
	}
	if estimate.ServiceByteRate <= 0 {
		deliveryRate := estimate.AggregateDeliveryByteRate
		if deliveryRate <= 0 {
			deliveryRate = estimate.DeliveryByteRate
		}
		if deliveryRate > 0 {
			rate = 1.1 * float64(deliveryRate)
		}
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
	// Until this service has observed queueing, measured delivery bounds
	// capacity only from below: the pacer itself was the limit. Release the
	// admitted window once per residence so the window rule's evidence, not
	// the previous release rate, drives growth.
	if estimate.PacingDiscovery && estimate.WindowRoundTrip > 0 && estimate.Window > 0 {
		rate = max(rate, float64(estimate.Window)/estimate.WindowRoundTrip.Seconds())
	}
	// Pacing adapts downward only when feedback proves congestion. A smaller
	// peer permission or a window-limited interval lowers measured service
	// without lowering the path's capacity.
	if !estimate.ServiceBacklogged && estimate.PacingHeldByteRate > 0 {
		rate = max(rate, float64(estimate.PacingHeldByteRate))
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
	reservation := windowPacingReservation{}
	defer self.releaseServiceReservation(&reservation, start, false)
	return self.waitForServiceReservation(ctx, byteCount, resend, start, &reservation)
}

// Route rejection keeps the same waiter and original charge. Terminal return
// removes it once; successful H1 admission enrolls before releasing the FIFO.
func (self *windowBurstPacer) waitForServiceReservation(ctx context.Context, byteCount int, resend bool, start *windowPacingWriteStart, reservation *windowPacingReservation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if reservation.complete {
		_, err := start.lifetime(time.Now())
		if err == nil {
			_, err = start.pacingRoute()
		}
		return err
	}
	if !reservation.started {
		if self.service == nil {
			self.service = &windowPacingService{}
		}
		if !resend {
			self.serviceSent += ByteCount(byteCount)
		}
		self.service.reserve(time.Now(), byteCount, self.rate, self.estimateRate, self.probeRate, self.probeLimit, resend, &self.waiter)
		*reservation = windowPacingReservation{started: true, active: true, byteCount: byteCount, resend: resend}
	}
	byteCount, resend = reservation.byteCount, reservation.resend
	err := self.waitUntilChangedForWrite(ctx, self.waiter.deadline, nil, start)
	if err == nil {
		err = self.waitForTurn(ctx, start)
	}
	for err == nil && !reservation.metered {
		now := time.Now()
		if _, err = start.lifetime(now); err != nil {
			break
		}
		if _, err = start.pacingRoute(); err != nil {
			break
		}
		delay, update := self.service.admitBurst(now, ByteCount(byteCount), resend, &self.waiter)
		if delay <= 0 {
			reservation.metered = true
			break
		}
		err = self.waitUntilChangedForWrite(ctx, now.Add(delay), update, start)
	}
	if err == nil {
		_, err = start.pacingRoute()
	}
	if errors.Is(err, errTransferRoutePolicyChanged) {
		return err
	}
	self.releaseServiceReservation(reservation, start, err == nil)
	if err == nil && self.afterAdmissionForTest != nil {
		self.afterAdmissionForTest()
	}
	if err == nil {
		err = ctx.Err()
		if err == nil {
			_, err = start.lifetime(time.Now())
		}
		if err != nil && start != nil {
			self.service.finishWrite(start.sequenceId, start.messageId, false)
		}
	}
	if err == nil {
		// The dispatch-edge barrier may have published another generation.
		// Paid admission survives it; actual carrier confirmation still waits.
		_, err = start.pacingRoute()
	}
	return err
}

// Release ownership, never serialization or probe debt. A bypassed non-H1
// dispatch does not enroll an H1 probe or manufacture a drained delivery.
func (self *windowBurstPacer) releaseServiceReservation(reservation *windowPacingReservation, start *windowPacingWriteStart, admitted bool) {
	if !reservation.active {
		return
	}
	self.service.stateLock.Lock()
	if admitted && start != nil {
		self.waiter.sentAt = time.Now()
		self.service.beginWriteWithLock(start.sequenceId, start.messageId, start.number, self.waiter.sentAt, reservation.resend)
	}
	self.service.removeWaiterWithLock(&self.waiter)
	self.service.pacingReservations--
	if !admitted && self.service.drained && self.service.pacingReservations == 0 {
		self.service.sourceIdleAt = time.Now()
	}
	if !reservation.resend {
		self.service.reservedByteCount -= ByteCount(reservation.byteCount)
	}
	reservation.active, reservation.complete = false, admitted
	self.service.stateLock.Unlock()
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
	if deadline.IsZero() {
		return ctx.Err()
	}
	return self.waitUntilChangedForWrite(ctx, deadline, update, nil)
}

// One existing timer handles service waits and owner expiry. ACK feedback may
// finish or renew the oldest lifetime without resetting the service deadline.
// A zero service deadline waits only for a FIFO handoff or cancellation.
func (self *windowBurstPacer) waitUntilChangedForWrite(ctx context.Context, deadline time.Time, update <-chan struct{}, start *windowPacingWriteStart) error {
	var acknowledgements <-chan struct{}
	if start != nil && start.owner != nil && start.owner.ackWindow != nil {
		acknowledgements = start.owner.ackWindow.Notify()
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		now := time.Now()
		lifetime, err := start.lifetime(now)
		if err != nil {
			return err
		}
		recovery, err := start.recoveryDeadline(now)
		if err != nil {
			return err
		}
		routeUpdate, err := start.pacingRoute()
		if err != nil {
			return err
		}
		if !deadline.IsZero() && !now.Before(deadline) {
			return nil
		}
		// A genuine zero writer budget may take ready admission, but cannot
		// park for serialization, FIFO handoff, burst credit or drain proof.
		if start != nil && start.writeReadyOnly {
			return errTransferRouteWriteTimeout
		}
		wake := deadline
		if !recovery.IsZero() && (wake.IsZero() || recovery.Before(wake)) {
			wake = recovery
		}
		if !lifetime.IsZero() && (wake.IsZero() || lifetime.Before(wake)) {
			wake = lifetime
		}
		var timer <-chan time.Time
		if !wake.IsZero() {
			if self.timer == nil {
				self.timer = time.NewTimer(0)
			}
			self.timer.Reset(time.Until(wake))
			timer = self.timer.C
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-update:
			return ctx.Err()
		case <-routeUpdate:
			// Re-arm from the new immutable snapshot, never the closed edge.
			continue
		case <-acknowledgements:
			// This nested wait and the outer snapshot have the same owner.
			// Leave feedback intact; only consume its wakeup edge here.
			continue
		case <-timer:
		}
		if _, err := start.lifetime(time.Now()); err != nil {
			return err
		}
		if wake == deadline && self.afterWaitForTest != nil {
			self.afterWaitForTest()
		}
		if wake == deadline && self.service != nil {
			self.service.observePacingTimerWake(time.Now().Sub(deadline))
		}
	}
}

// Logical sequences to one destination share serialization capacity and one
// opening probe. References are protected by SendBuffer.mutex; all timing and
// delivery methods below are safe for concurrent use.
type windowPacingService struct {
	stateLock                sync.Mutex
	references               int
	next                     time.Time
	burst                    windowPacingBurst
	dispatchBurst            windowPacingBurst
	burstEstimateTime        time.Duration
	dispatchInterval         time.Duration
	timerWakeDelay           time.Duration
	feedbackBurstByteCount   ByteCount
	burstMeter               windowPacingBurstMeter
	waiterHead               *windowPacingWaiter
	waiterTail               *windowPacingWaiter
	probeSent                ByteCount
	samples                  [deliveredBytesRingSize]windowServiceSample
	aggregate                windowServiceDeliveryRing
	newestBucket             int64
	hasSamples               bool
	serviceEpochAt           time.Time
	serviceHoldRate          ByteCount
	windowDeliveryAfterNanos int64
	// Delivery excluded from this sampling epoch still repays lifetime bytes.
	// A held rate carries its own support and epoch across a physical reset.
	serviceEpochByteBase ByteCount
	serviceHoldByteCount ByteCount
	serviceHoldEpochAt   time.Time
	// A proved receiver-held cumulative prefix keeps its raw delivery clock,
	// but intervals crossing that release cannot authorize faster service.
	receiverHeldPrefixAtNanos int64
	// Fixed summaries survive ACK gaps longer than the timestamp ring.
	// An incomplete cycle holds service; actual timestamps or fully applied
	// proved delivery complete it, independently of the old byte rate.
	feedbackAt          time.Time
	feedbackCycleBefore time.Time
	// Each head value is valid only with its matching nonzero raw clock.
	// Clearing those clocks on a quality change revokes both authorities.
	feedbackHeadAtNanos      int64
	feedbackCycleHeadAtNanos int64
	feedbackCycle            windowServiceSample
	feedbackComplete         windowServiceSample
	feedbackFresh            windowServiceSample
	feedbackPending          bool
	feedbackLimited          bool
	feedbackInterval         time.Duration
	feedbackDrainAt          time.Time
	feedbackDrainPending     ByteCount
	total                    ByteCount
	sent                     ByteCount
	reservedByteCount        ByteCount
	pacingReservations       int
	maxMessageByteCount      ByteCount
	minRoundTrip             time.Duration
	latestRoundTrip          time.Duration
	compression              time.Duration
	lastRoundTrip            time.Time
	lastRecoveryWriteAt      time.Time
	roundTripStats           windowBurstStats
	receiverRoundTrips       *windowReceiverRoundTrips
	drainMaximumTime         time.Duration
	drainStartedAt           time.Time
	drainCheckAt             time.Time
	drainObservedAt          time.Time
	drainObservedFlight      float64
	drainProgressUntil       time.Time
	drainUntil               time.Time
	drainWake                chan struct{}
	drainServiceEpoch        bool
	sourceIdleAt             time.Time
	bucketInterval           time.Duration
	writes                   map[Id]windowPacingWrite
	pendingWrites            int
	unprovableWriteCount     int
	drained                  bool
	drainedSent              ByteCount
	drainGeneration          uint64
	roundTripProbe           windowPacingRoundTripProbe
	// Discovery ends at the first observed queue or recovery write. Until
	// then measured delivery is the pacer's own previous release and bounds
	// capacity only from below. The held pace is the last pace admission
	// granted, and it falls only with congestion evidence.
	queueObservedAt         time.Time
	heldPacingRate          ByteCount
	qualityChangedAt        time.Time
	qualityLastNotification time.Time
	qualityRoundTripPending bool
	qualityServiceMeasured  bool
}

// Admission and statistics read the same discovery state and held pace.
func (self *windowPacingService) pacingHold() (bool, ByteCount) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.queueObservedAt.IsZero(), self.heldPacingRate
}

// Only the admitting read records the granted pace. The rate owner has
// already applied the previous hold, so a lower value here means a
// backlogged read chose to leave drain capacity.
func (self *windowPacingService) holdPacing(rate ByteCount) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.heldPacingRate = rate
}

// Discovery ends at the drain check's queue margin. A held pace tolerates
// one permitted burst of return-path residence; a longer queue releases it
// even after physical flight drains. Backlogged admission reads also lower it.
func (self *windowPacingService) observeQueueWithLock(timing windowReceiverRoundTripEstimate, roundTripCompression time.Duration, at time.Time) {
	margin := max(2*time.Millisecond, timing.minimum/4)
	if timing.minimum > 0 && timing.latest > timing.minimum+roundTripCompression+margin {
		if self.queueObservedAt.IsZero() {
			self.queueObservedAt = at
		}
		burstTime := windowPacingBurstMaximumTime(max(deliverySizedWindowSampleInterval, self.burstEstimateTime))
		if timing.latest-timing.minimum-roundTripCompression > max(margin, burstTime) {
			self.heldPacingRate = 0
		}
	}
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
	sequenceId          Id
	messageId           Id
	number              uint64
	sentAt              time.Time
	ackedAt             time.Time
	compression         time.Duration
	written             bool
	serviceRate         ByteCount
	serviceByteCount    ByteCount
	serviceEpochAt      time.Time
	resetService        bool
	resetInitialService bool
	pendingByteCount    ByteCount
	receiverTiming      windowReceiverRoundTripSample
	receiverTimingSet   bool
	observedRoundTrip   time.Duration
}

// Records delivery while send workers are pacing. SACKs may complete the
// probe itself but cannot prove that an entire sequence's earlier writes drained.
func (self *windowPacingService) acknowledgeWrite(sequenceId, messageId Id, number uint64, selective bool, compression time.Duration, at time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if write, ok := self.writes[sequenceId]; ok && write.messageId == messageId && write.unambiguous && !selective {
		previous := write
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
		self.storeWriteWithLock(sequenceId, previous, write)
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
	// A resumed write clears drained, but cannot erase the old flight's
	// unapplied bytes. Newer ACKs cannot spend that earlier byte credit.
	if self.feedbackPending && self.drained {
		self.feedbackDrainAt = at
		self.feedbackDrainPending = max(0, self.drainedSent-self.total)
		self.feedbackFresh = windowServiceSample{}
	}
	unqueued := self.drained && !resend
	resetInitialService := false
	if unqueued && !self.drainServiceEpoch && self.serviceHoldRate == 0 && (self.hasSamples && self.total > 0 || self.drainedSent > self.total) {
		// A cold opening may supply only one cumulative checkpoint. Its
		// next isolated reply cannot price the turnaround as serialization.
		_, _, latest, _ := self.measureWithLock(0, at, false)
		resetInitialService = latest == 0
	}
	self.drained = false
	self.sourceIdleAt = time.Time{}
	if self.writes == nil {
		self.writes = map[Id]windowPacingWrite{}
	}
	previous := self.writes[sequenceId]
	if !previous.pending {
		self.pendingWrites++
	}
	self.storeWriteWithLock(sequenceId, previous, windowPacingWrite{messageId: messageId, unambiguous: !resend, pending: true, generation: self.drainGeneration})
	if resend {
		self.lastRecoveryWriteAt = at
		self.abortDrainWithLock()
	}
	if unqueued {
		self.roundTripProbe = windowPacingRoundTripProbe{sequenceId: sequenceId, messageId: messageId, number: number, sentAt: at, serviceRate: self.serviceHoldRate, serviceByteCount: self.heldServiceByteCountWithLock(), serviceEpochAt: self.serviceHoldEpochAt, resetService: self.drainServiceEpoch, resetInitialService: resetInitialService, pendingByteCount: max(0, self.drainedSent-self.total), observedRoundTrip: self.latestRoundTrip}
	} else if resend && self.roundTripProbe.messageId == messageId {
		self.roundTripProbe = windowPacingRoundTripProbe{}
	}
	// The pause belongs to this first resumed write, including retries.
	// A later naturally empty flight must retain its serialization samples.
	self.drainServiceEpoch = false
}

// Only the confirmed, unretried controlled probe borrows the residence
// observed before dispatch. Its absolute deadline cannot move with later ACKs,
// and the caller still enforces the configured recovery and delivery limits.
func (self *windowPacingService) probeRecoveryDeadline(sequenceId, messageId Id, scale float32, maximum time.Duration) time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	probe := &self.roundTripProbe
	if !probe.resetService || !probe.written || !probe.ackedAt.IsZero() ||
		probe.sequenceId != sequenceId || probe.messageId != messageId ||
		probe.observedRoundTrip <= 0 || maximum <= 0 {
		return time.Time{}
	}
	interval := maximum
	scaled := float64(probe.observedRoundTrip) * max(1, float64(scale))
	if scaled < float64(maximum) {
		interval = time.Duration(scaled)
	}
	return probe.sentAt.Add(interval)
}

// Recovery can bypass H1 pacing after a carrier change. Invalidate at the
// common write boundary, before either copy can return an ambiguous ACK.
func (self *windowPacingService) invalidateProbe(sequenceId Id) {
	self.invalidateMessageProbe(sequenceId, Id{})
}

// A copied message invalidates its own probe. An unrelated retry still
// invalidates sequence-tail drain proof but cannot duplicate the probe copy.
func (self *windowPacingService) invalidateMessageProbe(sequenceId, messageId Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.drainServiceEpoch = false
	self.sourceIdleAt = time.Time{}
	if self.roundTripProbe.sequenceId == sequenceId && (messageId == (Id{}) || self.roundTripProbe.messageId == messageId) {
		self.roundTripProbe = windowPacingRoundTripProbe{}
	}
	if write, ok := self.writes[sequenceId]; ok {
		previous := write
		write.unambiguous = false
		self.storeWriteWithLock(sequenceId, previous, write)
		self.drained = false
		if write.pending {
			self.abortDrainWithLock()
		}
	}
}

// The carrier actually used, including a route change during the writer call,
// must confirm H1 before either a tail or a probe can establish a drained relay.
func (self *windowPacingService) finishWrite(sequenceId, messageId Id, h1 bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if write, ok := self.writes[sequenceId]; ok && write.messageId == messageId {
		previous := write
		write.written = h1
		write.unambiguous = write.unambiguous && h1
		if !h1 && write.pending {
			self.abortDrainWithLock()
		}
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
		self.storeWriteWithLock(sequenceId, previous, write)
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
		if !self.acceptQualityRoundTripWithLock(probe.ackedAt.Sub(probe.sentAt), probe.ackedAt) {
			self.roundTripProbe = windowPacingRoundTripProbe{}
			return
		}
		if probe.resetInitialService {
			// Old accounting or a sibling may have supplied a valid pair
			// since dispatch, even without an intervening controller read.
			_, _, probe.serviceRate, probe.serviceByteCount = self.measureWithLock(0, probe.ackedAt, false)
			probe.serviceEpochAt = self.serviceEpochAt
		}
		self.roundTripProbe = windowPacingRoundTripProbe{}
		// A drained pause contains local idle, not serialization. Keep
		// the established rate until the resumed train measures service.
		if probe.resetService || probe.resetInitialService {
			previousFeedbackAt, previousHeadAtNanos := self.feedbackAt, self.feedbackHeadAtNanos
			self.serviceEpochByteBase = self.total - self.serviceByteCountAfterWithLock(probe.ackedAt)
			self.serviceEpochAt, self.serviceHoldRate = probe.ackedAt, probe.serviceRate
			self.serviceHoldByteCount, self.serviceHoldEpochAt = probe.serviceByteCount, probe.serviceEpochAt
			self.feedbackAt, self.feedbackCycleBefore = probe.ackedAt, time.Time{}
			self.feedbackHeadAtNanos, self.feedbackCycleHeadAtNanos = 0, 0
			if probe.receiverTimingSet && probe.receiverTiming.atNanos == probe.ackedAt.UnixNano() &&
				!previousFeedbackAt.After(probe.ackedAt) {
				self.feedbackHeadAtNanos = probe.receiverTiming.atNanos - int64(probe.receiverTiming.raw-probe.receiverTiming.adjusted)
				if previousFeedbackAt.Equal(probe.ackedAt) {
					self.feedbackHeadAtNanos = min(self.feedbackHeadAtNanos, previousHeadAtNanos)
				}
			}
			self.feedbackCycle, self.feedbackComplete, self.feedbackFresh = windowServiceSample{}, windowServiceSample{}, windowServiceSample{}
			self.feedbackPending, self.feedbackDrainAt, self.feedbackDrainPending, self.feedbackInterval = false, time.Time{}, 0, 0
		}
		evidenceAt := probe.ackedAt
		if self.lastRoundTrip.After(evidenceAt) {
			evidenceAt = self.lastRoundTrip
		}
		previousPath := self.roundTripEvidenceWithLock(evidenceAt).minimum
		if probe.receiverTimingSet {
			self.receiverRoundTrips.confirmBaseline(probe.receiverTiming)
		}
		self.observeRoundTripWithLock(probe.ackedAt.Sub(probe.sentAt), probe.compression, probe.ackedAt, true)
		if currentPath := self.roundTripEvidenceWithLock(evidenceAt).minimum; previousPath > 0 && currentPath > previousPath {
			// Delivery from the old, smaller flight cannot qualify the new path.
			self.windowDeliveryAfterNanos = max(self.windowDeliveryAfterNanos, probe.ackedAt.UnixNano())
		}
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
	queued       bool
	firstQueued  bool
	lastQueued   bool
	// Raw endpoints keep ownership and expiry. The second clock is usable
	// only when every byte carries its own confirmed receiver timing.
	receiverFirstAtNanos int64
	receiverLastAtNanos  int64
	receiverBytes        ByteCount
	supply               windowServiceSupply
}

// Offer coverage is separate from raw delivery ownership. Missing production
// offers stay distinct from legacy observations that never carried this proof.
type windowServiceSupply struct {
	earliestSentNanos  int64
	latestSentNanos    int64
	maximumOfferGap   time.Duration
	minimumPath        time.Duration
	maximumGap         time.Duration
	receiverMaximumGap time.Duration
	lastQueue          time.Duration
	// These belong to exact cumulative heads, not every credited byte.
	// The right bound includes every head, even across reversed arrivals.
	headFirstAtNanos   int64
	headLastMinAtNanos int64
	headMaximumAtNanos int64
	recorded           bool
	complete           bool
	rawClock           bool
	headComplete       bool
}

// Merge bounded provenance before changing either summary's raw endpoints.
// Receiver clocks reuse the existing sample fields, including feedback summaries.
func (self *windowServiceSample) mergeSupply(other windowServiceSample) {
	if other.bytes <= 0 {
		return
	}
	if self.bytes <= 0 {
		self.supply = other.supply
		self.receiverFirstAtNanos, self.receiverLastAtNanos = other.receiverFirstAtNanos, other.receiverLastAtNanos
		self.receiverBytes = other.receiverBytes
		return
	}
	self.supply.recorded = self.supply.recorded || other.supply.recorded
	self.supply.complete = self.supply.complete && other.supply.complete
	offerGap := max(int64(0), other.supply.earliestSentNanos-self.supply.latestSentNanos,
		self.supply.earliestSentNanos-other.supply.latestSentNanos)
	self.supply.maximumOfferGap = max(self.supply.maximumOfferGap, other.supply.maximumOfferGap, time.Duration(offerGap))
	self.supply.earliestSentNanos = min(self.supply.earliestSentNanos, other.supply.earliestSentNanos)
	self.supply.latestSentNanos = max(self.supply.latestSentNanos, other.supply.latestSentNanos)
	self.supply.minimumPath = max(self.supply.minimumPath, other.supply.minimumPath)
	self.supply.rawClock = self.supply.rawClock && other.supply.rawClock
	self.supply.headComplete = self.supply.headComplete && other.supply.headComplete
	self.supply.headMaximumAtNanos = max(self.supply.headMaximumAtNanos, other.supply.headMaximumAtNanos)
	gap := max(int64(0), other.firstAtNanos-self.lastAtNanos, self.firstAtNanos-other.lastAtNanos)
	self.supply.maximumGap = max(self.supply.maximumGap, other.supply.maximumGap, time.Duration(gap))
	if self.receiverBytes == self.bytes && other.receiverBytes == other.bytes {
		receiverGap := int64(0)
		if self.lastAtNanos < other.firstAtNanos {
			receiverGap = max(int64(0), other.receiverFirstAtNanos-self.receiverLastAtNanos)
		} else if other.lastAtNanos < self.firstAtNanos {
			receiverGap = max(int64(0), self.receiverFirstAtNanos-other.receiverLastAtNanos)
		}
		self.supply.receiverMaximumGap = max(self.supply.receiverMaximumGap, other.supply.receiverMaximumGap, time.Duration(receiverGap))
	}
	if other.firstAtNanos < self.firstAtNanos {
		self.supply.headFirstAtNanos = other.supply.headFirstAtNanos
		self.receiverFirstAtNanos = other.receiverFirstAtNanos
	} else if other.firstAtNanos == self.firstAtNanos {
		self.supply.headFirstAtNanos = min(self.supply.headFirstAtNanos, other.supply.headFirstAtNanos)
		self.receiverFirstAtNanos = min(self.receiverFirstAtNanos, other.receiverFirstAtNanos)
	}
	if other.lastAtNanos > self.lastAtNanos {
		self.supply.headLastMinAtNanos = other.supply.headLastMinAtNanos
		self.supply.lastQueue = other.supply.lastQueue
		self.receiverLastAtNanos = other.receiverLastAtNanos
	} else if other.lastAtNanos == self.lastAtNanos {
		self.supply.headLastMinAtNanos = min(self.supply.headLastMinAtNanos, other.supply.headLastMinAtNanos)
		self.supply.lastQueue = min(self.supply.lastQueue, other.supply.lastQueue)
		self.receiverLastAtNanos = max(self.receiverLastAtNanos, other.receiverLastAtNanos)
	}
	self.receiverBytes += other.receiverBytes
}

// Continuous samples retain the existing rule. A discontinuous interval needs
// all credited offers before its left endpoint in the selected clock, or an
// exact selected tail whose own queue covers the entire interval, not one gap.
// That queue uses its own captured baseline, separately from whole-credit supply.
func (self windowServiceSupply) covers(receiverStart int64, span int64, interval time.Duration, receiverTimed bool) bool {
	gap := self.maximumGap
	if receiverTimed {
		gap = self.receiverMaximumGap
	}
	if !self.recorded || interval <= time.Duration(math.MaxInt64)/2 && gap <= 2*interval {
		return true
	}
	if !self.complete || self.minimumPath <= 0 || span <= 0 {
		return false
	}
	if receiverStart >= int64(self.minimumPath) && self.latestSentNanos > 0 &&
		self.latestSentNanos <= receiverStart-int64(self.minimumPath) {
		return true
	}
	return self.lastQueue >= time.Duration(span)
}

// Sparse ACKs can still cover continuously confirmed physical offers. The
// excluded left checkpoint must own its exact offer, and every selected
// offer must follow it without a source gap. Their span can only slow the
// measured rate; it supplies a delivery lower bound, not decrease authority.
func (self windowServiceSupply) continuousOfferSpan(span, headStart, offerStart int64, interval time.Duration) int64 {
	if span <= 0 || interval <= 0 || interval > time.Duration(math.MaxInt64)/2 ||
		!self.recorded || !self.complete || !self.headComplete || headStart <= 0 ||
		self.headMaximumAtNanos <= headStart || offerStart <= 0 ||
		self.earliestSentNanos < offerStart || self.latestSentNanos < self.earliestSentNanos {
		return 0
	}
	gap := max(self.maximumOfferGap, time.Duration(self.earliestSentNanos-offerStart))
	if gap > 2*interval {
		return 0
	}
	return max(span, self.headMaximumAtNanos-headStart, self.latestSentNanos-offerStart)
}

// A head boundary may prove preoffering without granting byte retiming.
// It may only lengthen the already-selected denominator; a fully retimed
// interval keeps its shorter exact clock, never a new raw-arrival floor.
// Unknown paired boundaries cannot borrow a tail's queue across a gap.
func (self windowServiceSupply) qualifiedSpan(span, receiverStart, headStart int64, interval time.Duration, receiverTimed bool) int64 {
	if span <= 0 {
		return 0
	}
	if self.headComplete && headStart > 0 {
		if self.headMaximumAtNanos <= headStart {
			return 0
		}
		span = max(span, self.headMaximumAtNanos-headStart)
		receiverStart = headStart
	} else if !receiverTimed && self.recorded && !self.rawClock &&
		(interval > time.Duration(math.MaxInt64)/2 || self.maximumGap > 2*interval) {
		return 0
	}
	if !self.covers(receiverStart, span, interval, receiverTimed) {
		return 0
	}
	return span
}

// Reserve one message atomically so concurrent sequences cannot each spend
// the same burst or opening-window bytes. Idle never replenishes the probe.
func (self *windowPacingService) reserve(now time.Time, byteCount int, rate, estimateRate, probeRate, probeLimit ByteCount, resend bool, waiter *windowPacingWaiter) time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	// A recovery write is congestion evidence; it ends discovery and
	// releases the held pace.
	if resend {
		if self.queueObservedAt.IsZero() {
			self.queueObservedAt = now
		}
		self.heldPacingRate = 0
	}
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
	maximumInterval := max(deliverySizedWindowSampleInterval, self.bucketInterval)
	interval := maximumInterval
	if self.dispatchInterval > 0 && !waiter.holdCompressionBurst {
		interval = min(interval, max(self.dispatchInterval, 2*self.timerWakeDelay))
	}
	messageBytes := ByteCount(byteCount)
	probe := ByteCount(0)
	// Queue or recovery evidence ends opening credit for every sibling.
	// A later quiet restart may spend only the historical remainder.
	if self.queueObservedAt.IsZero() {
		probe = min(ByteCount(byteCount), max(0, probeLimit-self.probeSent))
	}
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
	// Smoothing physical releases must not tighten the existing feedback
	// jitter allowance. An opposing service can still hold our ACK behind
	// its own permitted burst, even when our individual release is smaller.
	feedbackBytes := float64(estimateRate) * maximumInterval.Seconds()
	self.feedbackBurstByteCount = ByteCount(math.MaxInt64)
	if feedbackBytes < float64(math.MaxInt64) {
		self.feedbackBurstByteCount = max(self.maxMessageByteCount, ByteCount(feedbackBytes))
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
func (self *windowBurstPacer) waitForTurn(ctx context.Context, start *windowPacingWriteStart) error {
	self.service.stateLock.Lock()
	first := self.service.waiterHead == &self.waiter
	self.service.stateLock.Unlock()
	if !first {
		return self.waitUntilChangedForWrite(ctx, time.Time{}, self.waiter.ready, start)
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

// Fine releases require a scheduler that can actually wake at that cadence.
// Amortize observed timer lateness without growing beyond the old byte/time
// allowance. A decaying peak adapts immediately to load and returns toward
// fine dispatch after timely wakes; ACK, FIFO and route waits do not feed it.
func (self *windowPacingService) observePacingTimerWake(late time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	maximum := max(deliverySizedWindowSampleInterval, self.bucketInterval) / 2
	self.timerWakeDelay = max(min(max(0, late), maximum), self.timerWakeDelay-self.timerWakeDelay/8)
}

// The lock is released before waiting, so siblings and ACK delivery continue
// while a late producer waits for the next shared byte allowance.
func (self *windowPacingService) admitBurst(now time.Time, bytes ByteCount, resend bool, waiter *windowPacingWaiter) (time.Duration, <-chan struct{}) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !resend {
		timing := self.roundTripEvidenceWithLock(now)
		naturallyDraining := self.observeDrainProgressWithLock(now, max(4*deliverySizedWindowSampleInterval, timing.residence))
		maximum := self.drainMaximumTime
		if maximum <= 0 {
			maximum = windowPacingDrainMaximumTime
		}
		spanForResidence := func(residence float64) time.Duration {
			if residence >= float64(maximum/2) {
				return maximum
			}
			return min(maximum, max(4*deliverySizedWindowSampleInterval, 2*time.Duration(residence)))
		}
		setDeadline := func(span time.Duration) {
			self.drainUntil = self.drainStartedAt.Add(span)
			cooldown := time.Duration(math.MaxInt64)
			if span <= time.Duration(math.MaxInt64/8) {
				cooldown = max(windowPacingDrainMinimumInterval, 8*span)
			}
			self.drainCheckAt = self.drainStartedAt.Add(cooldown)
		}
		if self.pendingWrites == 0 {
			self.drainUntil = time.Time{}
		} else if !self.drainUntil.IsZero() {
			// A new path sample can arrive while the older short pause is
			// asleep. Extend that same pause from its original start; repeated
			// evidence cannot renew the configured absolute lifetime.
			span := spanForResidence(float64(self.latestRoundTrip))
			// Real above-burst repayment ends an already-expired attempt.
			// It does not prove empty flight or safe sustained resumed pacing;
			// a still-active pause and no-progress extensions remain bounded.
			if self.drainUntil.Before(self.drainStartedAt.Add(span)) &&
				(now.Before(self.drainUntil) || !naturallyDraining) {
				setDeadline(span)
			}
			if !now.Before(self.drainUntil) {
				self.drainUntil = time.Time{}
			}
		}
		if self.drainUntil.IsZero() && self.pendingWrites > 0 && !naturallyDraining && !now.Before(self.drainCheckAt) && self.roundTripStats.ring != nil {
			mean, count := self.roundTripStats.ring.mean(now)
			threshold := float64(self.minRoundTrip) + float64(self.compression) + float64(max(2*time.Millisecond, self.minRoundTrip/4))
			if timing.count > 0 {
				mean, count = float64(timing.latest), timing.count
				threshold = float64(timing.minimum) + float64(max(2*time.Millisecond, timing.minimum/4))
			}
			if count > 0 && mean > threshold && self.canDrainWithLock() {
				// Residence is only a reason to test for a changed path.
				// The cumulative tail ACK still has to prove an empty relay.
				self.drainStartedAt = now
				setDeadline(spanForResidence(max(mean, float64(self.latestRoundTrip))))
				self.drainServiceEpoch = true
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

// Maintain the number of tails that cannot prove an empty relay. Repeated
// invalidations do not add debt, and a fresh original tail replaces it.
func (self *windowPacingService) storeWriteWithLock(sequenceId Id, previous, write windowPacingWrite) {
	if previous.pending && (!previous.unambiguous || previous.generation != self.drainGeneration) {
		self.unprovableWriteCount--
	}
	if write.pending && (!write.unambiguous || write.generation != self.drainGeneration) {
		self.unprovableWriteCount++
	}
	self.writes[sequenceId] = write
}

// A copied or abandoned tail cannot supply the physical proof a pause needs.
// An in-progress writer remains eligible because confirmation can still arrive.
func (self *windowPacingService) canDrainWithLock() bool {
	return self.unprovableWriteCount == 0
}

// Abandon an impossible measurement without treating logical delivery or
// cancellation as a drained relay. An unrelated live RTT probe remains valid.
func (self *windowPacingService) abortDrainWithLock() {
	self.drainServiceEpoch = false
	if !self.drainUntil.IsZero() {
		self.drainUntil = time.Time{}
		if self.drainWake != nil {
			select {
			case self.drainWake <- struct{}{}:
			default:
			}
		}
	}
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
	self.observeWithLock(bytes, at)
}

// Legacy and worker fallback credit retain the raw arrival clock.
func (self *windowPacingService) observeWithLock(bytes ByteCount, at time.Time) {
	self.observeAckWithLock(windowServiceAckCredit{bytes: bytes}, at)
}

// Exact head timing changes only a delivery interval's serialization clock.
// Probe accounting, epochs and physical flight continue using raw arrivals.
func (self *windowPacingService) observeAckWithLock(credit windowServiceAckCredit, at time.Time) {
	bytes := credit.bytes
	if bytes <= 0 {
		return
	}
	self.accountAckWithLock(bytes, at)
	if at.Before(self.serviceEpochAt) {
		return
	}
	self.observeAckSampleWithLock(credit, at)
}

// Repayment survives estimator generations, including delayed publication of
// bytes already covered by a physically proved drain.
func (self *windowPacingService) accountAckWithLock(bytes ByteCount, at time.Time) {
	if probe := &self.roundTripProbe; !probe.sentAt.IsZero() && !at.After(probe.sentAt) {
		// Only older arrivals complete the drained train's missing samples.
		// Current delivery must not spend that older byte credit.
		probe.pendingByteCount = max(0, probe.pendingByteCount-bytes)
	}
	if self.feedbackPending && !self.feedbackDrainAt.IsZero() && !at.After(self.feedbackDrainAt) {
		self.feedbackDrainPending = max(0, self.feedbackDrainPending-bytes)
	}
	self.total += bytes
	self.serviceEpochByteBase += bytes
}

// Only eligible new-generation bytes reach serialization sampling.
func (self *windowPacingService) observeAckSampleWithLock(credit windowServiceAckCredit, at time.Time) {
	bytes, receiverTiming := credit.bytes, credit.receiverTiming
	self.serviceEpochByteBase -= bytes
	timing := self.roundTripEvidenceWithLock(at)
	ackSample := windowServiceSample{firstAtNanos: at.UnixNano(), lastAtNanos: at.UnixNano(), bytes: bytes,
		supply: windowServiceSupply{recorded: credit.offerRecorded,
			complete: credit.offerRecorded && credit.offerComplete && credit.firstSentAtNanos > 0 &&
				credit.lastSentAtNanos >= credit.firstSentAtNanos && timing.minimum > 0,
			rawClock:        !credit.receiverTimingObserved,
			earliestSentNanos: credit.firstSentAtNanos, latestSentNanos: credit.lastSentAtNanos,
			maximumOfferGap: credit.maximumOfferGap, minimumPath: timing.minimum}}
	headAtNanos := credit.receiverHeadAtNanos
	if receiverTiming.receivedAtNanos == at.UnixNano() && at.UnixNano() > 0 &&
		receiverTiming.receiverDelay >= 0 && int64(receiverTiming.receiverDelay) <= at.UnixNano() {
		ackSample.receiverBytes = bytes
		ackSample.receiverFirstAtNanos = at.UnixNano() - int64(receiverTiming.receiverDelay)
		ackSample.receiverLastAtNanos = ackSample.receiverFirstAtNanos
		if headAtNanos == 0 {
			headAtNanos = ackSample.receiverFirstAtNanos
		}
		if receiverTiming.headSentAtNanos > 0 && receiverTiming.headSentAtNanos <= at.UnixNano()-int64(receiverTiming.receiverDelay) {
			ackSample.supply.lastQueue = max(0, time.Duration(at.UnixNano()-receiverTiming.headSentAtNanos)-receiverTiming.receiverDelay-timing.minimum)
		}
	}
	if headAtNanos > 0 && headAtNanos <= at.UnixNano() {
		ackSample.supply.headFirstAtNanos = headAtNanos
		ackSample.supply.headLastMinAtNanos = headAtNanos
		ackSample.supply.headMaximumAtNanos = headAtNanos
		ackSample.supply.headComplete = true
	}
	roundTripCompression := self.compression
	if timing.count > 0 {
		roundTripCompression = 0
	}
	queued := timing.minimum > 0 && timing.latest > timing.minimum+roundTripCompression+2*time.Millisecond
	self.observeQueueWithLock(timing, roundTripCompression, at)
	arrivalBucket := at.UnixNano() / int64(max(deliverySizedWindowSampleInterval, self.bucketInterval))
	arrivalIndex := (arrivalBucket%int64(len(self.samples)) + int64(len(self.samples))) % int64(len(self.samples))
	queued = queued || self.samples[arrivalIndex].bucket == arrivalBucket && self.samples[arrivalIndex].queued
	// A gap beyond the continuous-feedback allowance starts a partial
	// measurement turn. Keep every real byte and the original gap duration.
	feedbackDelay := self.compression
	if receiverTiming.receivedAtNanos != 0 && receiverTiming.receivedAtNanos == at.UnixNano() && receiverTiming.receiverDelay >= 0 {
		// Only this credit's receiver wait can join it to the previous
		// turn. A sibling's metadata cannot explain the refill silence.
		feedbackDelay = receiverTiming.receiverDelay
	}
	maximumGap := feedbackDelay + 2*max(deliverySizedWindowSampleInterval, self.bucketInterval)
	if !self.feedbackPending && !self.feedbackAt.IsZero() && at.Sub(self.feedbackAt) > maximumGap && self.serviceHoldRate > 0 && (self.outstandingWithLock() > 0 || self.drained) {
		self.feedbackCycleBefore = self.feedbackAt
		self.feedbackCycleHeadAtNanos = self.feedbackHeadAtNanos
		self.feedbackCycle, self.feedbackFresh = windowServiceSample{}, windowServiceSample{}
		self.feedbackPending = true
		self.feedbackLimited = self.pendingWrites > 0 &&
			self.outstandingWithLock()+float64(bytes) <= self.flightBoundAtWithLock(self.serviceHoldRate, timing.residence)
		self.feedbackInterval = max(time.Nanosecond, self.compression)
		self.feedbackDrainAt, self.feedbackDrainPending = time.Time{}, 0
		if probe := self.roundTripProbe; !probe.sentAt.IsZero() && at.After(self.feedbackCycleBefore) && !at.After(probe.sentAt) {
			self.feedbackDrainAt, self.feedbackDrainPending = probe.sentAt, probe.pendingByteCount
		}
	}
	// Independent workers may still apply this accepted interval in pieces.
	// Keep its bounded summary until newer measured evidence supersedes it.
	if !self.feedbackPending && self.feedbackComplete.bytes > 0 &&
		(self.feedbackCycleBefore.IsZero() || at.After(self.feedbackCycleBefore)) {
		complete := &self.feedbackComplete
		complete.mergeSupply(ackSample)
		if at.UnixNano() < complete.firstAtNanos {
			complete.firstAtNanos, complete.firstBytes = at.UnixNano(), bytes
		} else if at.UnixNano() == complete.firstAtNanos {
			complete.firstBytes += bytes
		}
		complete.lastAtNanos = max(complete.lastAtNanos, at.UnixNano())
		complete.bytes += bytes
		complete.queued = complete.queued || queued
		self.feedbackInterval = max(self.feedbackInterval, self.compression)
		self.feedbackCycle = *complete
	}
	// Keep the new side of a proved drain independently. Its slow pair may
	// outlive the ring while a worker still owes old delivery accounting.
	if self.feedbackPending && !self.feedbackDrainAt.IsZero() && at.After(self.feedbackDrainAt) {
		fresh := &self.feedbackFresh
		fresh.mergeSupply(ackSample)
		if fresh.bytes == 0 || at.UnixNano() < fresh.firstAtNanos {
			fresh.firstAtNanos, fresh.firstBytes = at.UnixNano(), bytes
		} else if at.UnixNano() == fresh.firstAtNanos {
			fresh.firstBytes += bytes
		}
		fresh.lastAtNanos = max(fresh.lastAtNanos, at.UnixNano())
		fresh.bytes += bytes
		fresh.queued = fresh.queued || queued
	}
	if self.feedbackPending && at.After(self.feedbackCycleBefore) {
		self.feedbackInterval = max(self.feedbackInterval, self.compression)
		cycle := &self.feedbackCycle
		cycle.mergeSupply(ackSample)
		if cycle.bytes == 0 || at.UnixNano() < cycle.firstAtNanos {
			cycle.firstAtNanos, cycle.firstBytes = at.UnixNano(), bytes
		} else if at.UnixNano() == cycle.firstAtNanos {
			cycle.firstBytes += bytes
		}
		cycle.lastAtNanos = max(cycle.lastAtNanos, at.UnixNano())
		cycle.bytes += bytes
		cycle.queued = cycle.queued || queued
		if self.outstandingWithLock()+float64(bytes) > self.flightBoundAtWithLock(self.serviceHoldRate, timing.residence) {
			self.feedbackLimited = false
		}
	}
	if self.feedbackAt.Before(at) {
		self.feedbackAt = at
		self.feedbackHeadAtNanos = ackSample.supply.headLastMinAtNanos
	} else if self.feedbackAt.Equal(at) {
		self.feedbackHeadAtNanos = min(self.feedbackHeadAtNanos, ackSample.supply.headLastMinAtNanos)
	}
	if self.feedbackCycleBefore.Equal(at) {
		self.feedbackCycleHeadAtNanos = min(self.feedbackCycleHeadAtNanos, ackSample.supply.headLastMinAtNanos)
		for _, cycle := range []*windowServiceSample{&self.feedbackCycle, &self.feedbackComplete} {
			if cycle.firstAtNanos == at.UnixNano() && cycle.firstBytes == 0 {
				cycle.supply.headFirstAtNanos = self.feedbackCycleHeadAtNanos
				cycle.supply.headComplete = cycle.supply.headComplete && self.feedbackCycleHeadAtNanos > 0
			}
		}
	}
	bucket := at.UnixNano() / int64(max(deliverySizedWindowSampleInterval, self.bucketInterval))
	if self.hasSamples && bucket <= self.newestBucket-int64(len(self.samples)) {
		self.completeFeedbackCycleWithLock()
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
	sample.mergeSupply(ackSample)
	if sample.bytes == 0 || at.UnixNano() < sample.firstAtNanos {
		sample.firstAtNanos = at.UnixNano()
		sample.firstBytes = bytes
		sample.firstQueued = queued
	} else if at.UnixNano() == sample.firstAtNanos {
		sample.firstBytes += bytes
		sample.firstQueued = sample.firstQueued || queued
	}
	if sample.bytes == 0 || at.UnixNano() > sample.lastAtNanos {
		sample.lastAtNanos = at.UnixNano()
		sample.lastQueued = queued
	} else if at.UnixNano() == sample.lastAtNanos {
		sample.lastQueued = sample.lastQueued || queued
	}
	sample.bytes += bytes
	sample.queued = sample.queued || queued
	self.completeFeedbackCycleWithLock()
}

// A partial limited flight cannot turn a later refill's idle gap into service.
// Continuous arrivals, queued serialization, or fully applied physical proof
// complete the cycle without requiring delivery at the previously held rate.
func (self *windowPacingService) completeFeedbackCycleWithLock() {
	if !self.feedbackPending {
		return
	}
	cycle := &self.feedbackCycle
	completeDrain := self.drained && self.total >= self.drainedSent ||
		!self.feedbackDrainAt.IsZero() && self.feedbackDrainPending == 0 && cycle.lastAtNanos <= self.feedbackDrainAt.UnixNano()
	if completeDrain {
		cycle.supply.maximumGap = max(cycle.supply.maximumGap, time.Duration(cycle.firstAtNanos-self.feedbackCycleBefore.UnixNano()))
		cycle.receiverBytes = 0
		cycle.firstAtNanos, cycle.firstBytes = self.feedbackCycleBefore.UnixNano(), 0
		cycle.supply.headFirstAtNanos = self.feedbackCycleHeadAtNanos
		cycle.supply.headComplete = cycle.supply.headComplete && self.feedbackCycleHeadAtNanos > 0
	}
	waitingForDrainBytes := self.drained && self.total < self.drainedSent || self.feedbackDrainPending > 0
	if waitingForDrainBytes || cycle.lastAtNanos-cycle.firstAtNanos < int64(self.feedbackInterval) {
		return
	}
	if self.feedbackLimited && !completeDrain {
		timing := self.roundTripEvidenceWithLock(time.Unix(0, cycle.lastAtNanos))
		feedbackDelay := max(self.compression, self.feedbackInterval)
		roundTripCompression := self.compression
		if timing.feedbackPaired {
			roundTripCompression = 0
			// A receiver timer advertises an upper bound, not an observed
			// continuous turn. Its paired clock records the actual wait.
			feedbackDelay = timing.latestRaw - timing.latest
		}
		maximumGap := feedbackDelay + 2*max(deliverySizedWindowSampleInterval, self.bucketInterval)
		// A modest queue inside a small flight does not explain a later
		// refill silence. Sparse serialization must cover its actual gap.
		maximumGap = max(maximumGap, timing.latest-timing.minimum-roundTripCompression)
		before := cycle.lastAtNanos
		for offset := int64(0); offset < int64(len(self.samples)); offset++ {
			bucket := self.newestBucket - offset
			index := (bucket%int64(len(self.samples)) + int64(len(self.samples))) % int64(len(self.samples))
			sample := &self.samples[index]
			if sample.bucket != bucket || sample.bytes <= 0 || sample.lastAtNanos < cycle.firstAtNanos {
				continue
			}
			if before-sample.lastAtNanos > int64(maximumGap) {
				break
			}
			before = max(cycle.firstAtNanos, sample.firstAtNanos)
		}
		if cycle.lastAtNanos-before < int64(self.feedbackInterval) {
			return
		}
	}
	self.feedbackPending = false
	self.feedbackComplete = *cycle
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
	rate, total, latest, _ := self.measureWithLock(horizon, now, retain)
	return rate, total, latest
}

// Capacity uses the selected rate's epoch support, never its lifetime total.
// Statistics and admission read the same proof; only admission retains it.
func (self *windowPacingService) measureCapacity(horizon time.Duration, now time.Time, retain bool) (ByteCount, ByteCount, ByteCount) {
	rate, bytes, latest, _ := self.measureCapacityEvidence(horizon, now, retain)
	return rate, bytes, latest
}

// The proof timestamp belongs to the selected current rate, or the latest
// rate when current evidence is absent. A hold or offer-only lower bound has
// no decrease proof; it cannot borrow another selected interval's timestamp.
func (self *windowPacingService) measureCapacityEvidence(horizon time.Duration, now time.Time, retain bool) (ByteCount, ByteCount, ByteCount, int64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	rate, _, latest, bytes, proofAtNanos := self.measureEvidenceWithLock(horizon, now, retain)
	if rate <= 0 && latest <= 0 {
		bytes = 0
	}
	return rate, bytes, latest, proofAtNanos
}

// The current epoch owns all eligible credited bytes. A later boundary can
// conservatively recover only complete retained buckets or feedback summaries.
// Four KiB is the largest establishment threshold; older totals stay raw.
func (self *windowPacingService) serviceByteCountAfterWithLock(after time.Time) ByteCount {
	if after.Equal(self.serviceEpochAt) {
		return min(kib(4), max(0, self.total-self.serviceEpochByteBase))
	}
	bytes := ByteCount(0)
	for _, sample := range self.samples {
		if sample.bytes > 0 && sample.firstAtNanos >= after.UnixNano() {
			bytes += min(kib(4)-bytes, sample.bytes)
		}
	}
	for _, sample := range []windowServiceSample{self.feedbackCycle, self.feedbackComplete, self.feedbackFresh} {
		if sample.bytes > 0 && sample.firstAtNanos >= after.UnixNano() {
			bytes = max(bytes, min(kib(4), sample.bytes))
		}
	}
	return min(max(0, self.total), bytes)
}

// Same-epoch delivery can complete a held sample's byte qualification. Once
// the epoch changes, only that held rate's captured support survives with it.
func (self *windowPacingService) heldServiceByteCountWithLock() ByteCount {
	bytes := self.serviceHoldByteCount
	if self.serviceHoldRate > 0 && self.serviceHoldEpochAt.Equal(self.serviceEpochAt) {
		bytes = max(bytes, self.serviceByteCountAfterWithLock(self.serviceEpochAt))
	}
	return bytes
}

// Probe enrollment and confirmation inspect the same timestamp evidence as
// controller reads. The last result is byte support for the selected current
// rate, or for the latest/held rate when no current interval exists.
func (self *windowPacingService) measureWithLock(horizon time.Duration, now time.Time, retain bool) (ByteCount, ByteCount, ByteCount, ByteCount) {
	rate, total, latest, bytes, _ := self.measureEvidenceWithLock(horizon, now, retain)
	return rate, total, latest, bytes
}

// Preserve each chosen rate's own endpoint. Newer rejected intervals cannot
// refresh it, and a hold without a qualifying interval reports unknown proof.
func (self *windowPacingService) measureEvidenceWithLock(horizon time.Duration, now time.Time, retain bool) (ByteCount, ByteCount, ByteCount, ByteCount, int64) {
	timing := self.roundTripEvidenceWithLock(now)
	roundTripCompression := self.compression
	if timing.count > 0 {
		roundTripCompression = 0
	}
	rate, latest := ByteCount(0), ByteCount(0)
	rateAtNanos, latestAtNanos := int64(0), int64(0)
	epochAt, hold := self.serviceEpochAt, self.serviceHoldRate
	holdByteCount := self.heldServiceByteCountWithLock()
	initialBoundaryAt := time.Time{}
	if self.roundTripProbe.resetInitialService {
		initialBoundaryAt = self.roundTripProbe.sentAt
	}
	if probe := self.roundTripProbe; probe.resetService {
		// Tail ACKs can prove delivery before their coalesced bytes apply.
		// A partial old train cannot replace its held service during that gap.
		boundary := probe.ackedAt
		if boundary.IsZero() && probe.pendingByteCount > 0 {
			boundary = probe.sentAt.Add(time.Nanosecond)
		}
		if epochAt.Before(boundary) {
			epochAt, hold = boundary, probe.serviceRate
			holdByteCount = probe.serviceByteCount
		}
	}
	if self.feedbackPending && self.feedbackDrainPending > 0 {
		boundary := self.feedbackDrainAt.Add(time.Nanosecond)
		if epochAt.Before(boundary) {
			epochAt = boundary
		}
	}
	epochByteCount := self.serviceByteCountAfterWithLock(epochAt)
	compression := self.compression
	if self.feedbackCycle.bytes > 0 && self.feedbackAt.UnixNano() <= self.feedbackCycle.lastAtNanos {
		compression = max(compression, self.feedbackInterval)
	}
	byteRate := func(bytes ByteCount, span int64) ByteCount {
		value := float64(bytes) * float64(time.Second) / float64(span)
		measured := ByteCount(math.MaxInt64)
		if value < float64(math.MaxInt64) {
			measured = ByteCount(value)
		}
		return measured
	}
	heldPrefixIncrease := func(measured ByteCount, span, atNanos int64) bool {
		// A validated held prefix owns real delivery, not a faster serializer.
		// Growth requires an independent interval after its release; lower
		// service evidence remains useful even while a prefix is recovering.
		after := self.receiverHeldPrefixAtNanos
		return measured > hold && after != 0 && (atNanos <= after || span >= atNanos-after)
	}
	// A bounded drain has not yet distinguished queueing from propagation.
	// Preserve qualified service through its physical proof and write handoff;
	// expiry or abandonment ends the hold, while new bytes may raise it.
	eligibleCycle := (self.feedbackPending || self.feedbackComplete.bytes > 0) &&
		(epochAt.IsZero() || !self.feedbackCycleBefore.Before(epochAt))
	interval := max(deliverySizedWindowSampleInterval, self.bucketInterval)
	pendingDrain := hold > 0 && self.drainServiceEpoch && (self.drained || now.Before(self.drainUntil))
	if pendingDrain || self.feedbackPending && eligibleCycle {
		cycle := self.feedbackCycle
		span := cycle.lastAtNanos - self.feedbackCycleBefore.UnixNano()
		rawSpan := span
		cycle.supply.maximumGap = max(cycle.supply.maximumGap, time.Duration(cycle.firstAtNanos-self.feedbackCycleBefore.UnixNano()))
		span = cycle.supply.qualifiedSpan(span, 0, self.feedbackCycleHeadAtNanos, interval, false)
		if span > 0 && eligibleCycle && self.feedbackAt.UnixNano() <= cycle.lastAtNanos && cycle.bytes > 0 && rawSpan >= int64(max(time.Nanosecond, compression, self.feedbackInterval)) {
			measured := byteRate(cycle.bytes, span)
			if measured > hold && !heldPrefixIncrease(measured, span, cycle.lastAtNanos) {
				if retain {
					self.serviceHoldRate = measured
					self.serviceHoldByteCount, self.serviceHoldEpochAt = epochByteCount, epochAt
					if self.roundTripProbe.resetService || self.roundTripProbe.resetInitialService {
						self.roundTripProbe.serviceRate = measured
						self.roundTripProbe.serviceByteCount, self.roundTripProbe.serviceEpochAt = epochByteCount, epochAt
					}
				}
				if horizon > 0 && cycle.lastAtNanos >= now.Add(-horizon).UnixNano() {
					rate = measured
				}
				return rate, self.total, measured, epochByteCount, cycle.lastAtNanos
			}
		}
		return 0, self.total, hold, holdByteCount, 0
	}
	var samples [deliveredBytesRingSize]*windowServiceSample
	count := 0
	// Preserve a rate long enough to receive feedback from sends using it.
	// A queued RTT must not extend an old fast sample's life after a slowdown.
	horizon = min(horizon, max(4*interval, timing.residence+2*interval))
	cutoff := now.Add(-horizon).UnixNano()
	for offset := int64(0); offset < int64(len(self.samples)); offset++ {
		bucket := self.newestBucket - offset
		index := (bucket%int64(len(self.samples)) + int64(len(self.samples))) % int64(len(self.samples))
		sample := &self.samples[index]
		if sample.bucket != bucket || sample.bytes <= 0 || !epochAt.IsZero() && sample.firstAtNanos < epochAt.UnixNano() {
			continue
		}
		if !initialBoundaryAt.IsZero() && sample.firstAtNanos <= initialBoundaryAt.UnixNano() && initialBoundaryAt.UnixNano() < sample.lastAtNanos {
			// An aggregate spanning both trains cannot be split exactly.
			continue
		}
		samples[count] = sample
		count++
	}
	minSpan := int64(compression)
	increaseSpan := int64(max(compression, interval))
	rejectedIncreaseAt, qualifiedAt := int64(0), int64(0)
	invalidReceiverAt := int64(0)
	offerRate := ByteCount(0)
	offerSet := false
	offerRejectedIncreaseAt, offerInvalidReceiverAt := int64(0), int64(0)
	recordRate := func(measured ByteCount, atNanos, proofAtNanos int64) {
		qualifiedAt = max(qualifiedAt, atNanos)
		if latest == 0 {
			latest = measured
			latestAtNanos = proofAtNanos
		}
		if horizon > 0 && atNanos >= cutoff {
			if measured > rate || measured == rate && proofAtNanos > rateAtNanos {
				rate, rateAtNanos = measured, proofAtNanos
			}
		}
	}
	observeRate := func(bytes ByteCount, span, atNanos int64, intervalLimited, firstQueued bool, supply windowServiceSupply, receiverStart, headStart int64, receiverTimed bool, offerStart int64) bool {
		proofAtNanos := atNanos
		offerOnly := false
		if qualified := supply.qualifiedSpan(span, receiverStart, headStart, interval, receiverTimed); qualified > 0 {
			span = qualified
		} else {
			if offerSet {
				// Keep the nearest accepted lower bound while looking only
				// for an independently covered pair with this right endpoint.
				return false
			}
			span = supply.continuousOfferSpan(span, headStart, offerStart, interval)
			offerOnly = true
			// An offered train measures delivery from below, even when its
			// own cadence limits it. Only independent old-pace flight may
			// turn that lower bound into a decrease at the admission edge.
			proofAtNanos = 0
		}
		if span <= 0 {
			return false
		}
		measured := byteRate(bytes, span)
		if heldPrefixIncrease(measured, span, atNanos) || measured > hold && intervalLimited && span < increaseSpan && (hold > 0 || firstQueued) {
			// A retained prefix or carrier reader can empty a completed flight
			// at memory speed. Preserve its delivery without letting that release
			// cadence prove more physical service, even after flight reaches zero.
			rejectedIncreaseAt = max(rejectedIncreaseAt, atNanos)
			return false
		}
		if offerOnly {
			// A lower bound cannot hide an older covered interval: its rate
			// also sizes the physical burst even when pacing stays held.
			offerRate, offerSet = measured, true
			offerRejectedIncreaseAt, offerInvalidReceiverAt = rejectedIncreaseAt, invalidReceiverAt
			return false
		}
		recordRate(measured, atNanos, proofAtNanos)
		return true
	}
	for i := 0; i < count; i++ {
		newer := samples[i]
		if latest > 0 && newer.lastAtNanos < cutoff {
			break
		}
		offerSet = false
		// With immediate ACKs a small peer window can fit entirely inside
		// one bucket. Its first/last arrivals still measure serialization.
		// Exact receiver endpoints already remove their own ACK waiting.
		// A cold pair can discover service before the advertised timer;
		// a train queued from its first checkpoint still needs a full turn.
		// Compressed cumulative prefixes can have independently phased heads:
		// removing one first checkpoint does not remove every lane's leading
		// compressed bytes. Without exact receiver timing, require the same
		// full sampler turn for growth even before residence proves a queue.
		if span := newer.lastAtNanos - newer.firstAtNanos; span > 0 && (span >= minSpan || hold == 0 && newer.receiverBytes == newer.bytes) {
			if newer.receiverBytes == newer.bytes {
				span = newer.receiverLastAtNanos - newer.receiverFirstAtNanos
			}
			if span > 0 {
				receiverStart := int64(0)
				if newer.receiverBytes == newer.bytes {
					receiverStart = newer.receiverFirstAtNanos
				} else if newer.supply.rawClock {
					receiverStart = newer.firstAtNanos
				}
				observeRate(newer.bytes-newer.firstBytes, span, newer.lastAtNanos, newer.queued || compression > 0 && newer.receiverBytes != newer.bytes, newer.firstQueued, newer.supply, receiverStart, newer.supply.headFirstAtNanos, newer.receiverBytes == newer.bytes, 0)
			} else {
				invalidReceiverAt = max(invalidReceiverAt, newer.lastAtNanos)
			}
		}
		bytes := newer.bytes
		coverage := *newer
		queued := newer.queued
		receiverTimed := newer.receiverBytes == newer.bytes
		for j := i + 1; j < count; j++ {
			if !initialBoundaryAt.IsZero() && newer.firstAtNanos > initialBoundaryAt.UnixNano() && samples[j].lastAtNanos <= initialBoundaryAt.UnixNano() {
				// Preserve real pairs on either side, including late old
				// accounting, but never join them across a proved drain.
				break
			}
			span := newer.lastAtNanos - samples[j].lastAtNanos
			receiverTimed = receiverTimed && samples[j].receiverBytes == samples[j].bytes
			receiverSpan := newer.receiverLastAtNanos - samples[j].receiverLastAtNanos
			firstQueued := samples[j].lastQueued
			candidateBytes := bytes
			candidateSupply := coverage.supply
			candidateSupply.maximumGap = max(candidateSupply.maximumGap, time.Duration(coverage.firstAtNanos-samples[j].lastAtNanos))
			if coverage.receiverBytes == coverage.bytes && samples[j].receiverBytes == samples[j].bytes {
				candidateSupply.receiverMaximumGap = max(candidateSupply.receiverMaximumGap, time.Duration(coverage.receiverFirstAtNanos-samples[j].receiverLastAtNanos))
			}
			receiverStart := int64(0)
			headStart := int64(0)
			offerStart := int64(0)
			if samples[j].supply.headComplete {
				headStart = samples[j].supply.headLastMinAtNanos
			}
			// A multi-checkpoint bucket cannot identify the offer belonging
			// to its excluded endpoint. Keep that ambiguity explicit.
			if samples[j].firstAtNanos == samples[j].lastAtNanos && samples[j].supply.complete {
				offerStart = samples[j].supply.latestSentNanos
			}
			if receiverTimed && samples[j].receiverBytes == samples[j].bytes {
				receiverStart = samples[j].receiverLastAtNanos
			} else if candidateSupply.rawClock && samples[j].supply.rawClock {
				receiverStart = samples[j].lastAtNanos
			}
			if span < minSpan && newer.lastAtNanos-samples[j].firstAtNanos >= minSpan {
				// The first checkpoint preserves a short opening train
				// whose last checkpoint is too close to the next bucket.
				span = newer.lastAtNanos - samples[j].firstAtNanos
				receiverSpan = newer.receiverLastAtNanos - samples[j].receiverFirstAtNanos
				firstQueued = samples[j].firstQueued
				candidateBytes += samples[j].bytes - samples[j].firstBytes
				combined := coverage
				combined.mergeSupply(*samples[j])
				candidateSupply = combined.supply
				headStart = 0
				offerStart = 0
				if samples[j].supply.headComplete {
					headStart = samples[j].supply.headFirstAtNanos
				}
				if receiverTimed && samples[j].receiverBytes == samples[j].bytes {
					receiverStart = samples[j].receiverFirstAtNanos
				} else if candidateSupply.rawClock && samples[j].supply.rawClock {
					receiverStart = samples[j].firstAtNanos
				} else {
					receiverStart = 0
				}
			}
			if span > 0 && (span >= minSpan || hold == 0 && receiverTimed) {
				if receiverTimed {
					span = receiverSpan
				}
				if span <= 0 {
					invalidReceiverAt = max(invalidReceiverAt, newer.lastAtNanos)
				} else if observeRate(candidateBytes, span, newer.lastAtNanos, queued || samples[j].queued || compression > 0 && !receiverTimed, firstQueued, candidateSupply, receiverStart, headStart, receiverTimed, offerStart) {
					offerSet = false
					break
				}
			}
			coverage.mergeSupply(*samples[j])
			coverage.firstAtNanos = min(coverage.firstAtNanos, samples[j].firstAtNanos)
			coverage.bytes += samples[j].bytes
			bytes += samples[j].bytes
			queued = queued || samples[j].queued
		}
		if offerSet {
			// Unselected older intervals cannot reject the accepted fallback.
			rejectedIncreaseAt, invalidReceiverAt = offerRejectedIncreaseAt, offerInvalidReceiverAt
			recordRate(offerRate, newer.lastAtNanos, 0)
		}
	}
	heldAfterRejection := rejectedIncreaseAt > 0 && qualifiedAt <= rejectedIncreaseAt && max(rate, latest) < hold
	if heldAfterRejection {
		// A rejected peak cannot manufacture either direction of a change
		// from its partial interval. Later independent pairs still supersede it.
		rate, latest = 0, hold
	}
	// Once residence proves a queue, average a full feedback interval and
	// several compression turns. Repeated ACK peaks cannot drain that queue.
	if count > 1 && timing.minimum > 0 && timing.latest > timing.minimum+roundTripCompression+2*time.Millisecond {
		newer := samples[0]
		bytes := newer.bytes
		coverage := *newer
		minSpan := int64(max(timing.minimum, 4*compression, 4*interval))
		queued := self.outstandingWithLock() > self.flightBoundAtWithLock(rate, timing.residence)
		continuousGap := compression + 2*interval
		maximumGap := continuousGap
		if queued {
			// Current flight alone cannot rewrite an earlier window idle.
			// Excess residence must also cover a sparse serializer's gap.
			maximumGap = max(maximumGap, timing.latest-timing.minimum-roundTripCompression)
		}
		for j := 1; j < count; j++ {
			if !initialBoundaryAt.IsZero() && newer.firstAtNanos > initialBoundaryAt.UnixNano() && samples[j].lastAtNanos <= initialBoundaryAt.UnixNano() {
				break
			}
			// A window-limited gap cannot establish sustained service.
			// Keep discovery from the active train until a whole feedback
			// interval has continuous delivery at the new offered rate.
			gap := samples[j-1].firstAtNanos - samples[j].lastAtNanos
			if gap > int64(continuousGap) {
				freshSpan := newer.lastAtNanos - samples[j-1].firstAtNanos
				// Include the first checkpoint only as a phase allowance:
				// a still-supported fresh train cannot inherit older silence.
				// Published rates continue to exclude that endpoint's bytes.
				if hold > 0 && freshSpan >= increaseSpan && byteRate(bytes, freshSpan) >= hold {
					break
				}
			}
			if gap > int64(maximumGap) {
				break
			}
			span := newer.lastAtNanos - samples[j].lastAtNanos
			if span >= minSpan {
				candidateSupply := coverage.supply
				candidateSupply.maximumGap = max(candidateSupply.maximumGap, time.Duration(coverage.firstAtNanos-samples[j].lastAtNanos))
				receiverStart := int64(0)
				if candidateSupply.rawClock && samples[j].supply.rawClock {
					receiverStart = samples[j].lastAtNanos
				}
				headStart := int64(0)
				if samples[j].supply.headComplete {
					headStart = samples[j].supply.headLastMinAtNanos
				}
				span = candidateSupply.qualifiedSpan(span, receiverStart, headStart, interval, false)
				if span <= 0 {
					break
				}
				measured := byteRate(bytes, span)
				if !heldPrefixIncrease(measured, span, newer.lastAtNanos) && self.outstandingWithLock() > self.flightBoundAtWithLock(measured, timing.residence) {
					heldAfterRejection = false
					latest = measured
					latestAtNanos = newer.lastAtNanos
					if horizon > 0 && newer.lastAtNanos >= cutoff {
						rate = measured
						rateAtNanos = newer.lastAtNanos
					}
				}
				break
			}
			coverage.mergeSupply(*samples[j])
			coverage.firstAtNanos = min(coverage.firstAtNanos, samples[j].firstAtNanos)
			coverage.bytes += samples[j].bytes
			bytes += samples[j].bytes
		}
	}
	completedFallback := false
	if rate == 0 && latest == 0 {
		for _, cycle := range []windowServiceSample{self.feedbackFresh, self.feedbackComplete} {
			span := cycle.lastAtNanos - cycle.firstAtNanos
			if cycle.bytes > cycle.firstBytes && span >= int64(max(time.Nanosecond, compression, self.feedbackInterval)) && (epochAt.IsZero() || cycle.firstAtNanos >= epochAt.UnixNano()) {
				receiverStart := int64(0)
				if cycle.supply.rawClock {
					receiverStart = cycle.firstAtNanos
				}
				if observeRate(cycle.bytes-cycle.firstBytes, span, cycle.lastAtNanos, cycle.queued, cycle.queued, cycle.supply, receiverStart, cycle.supply.headFirstAtNanos, false, 0) {
					completedFallback = true
					break
				}
			}
		}
	}
	if rate > 0 || latest > 0 {
		if invalidReceiverAt > 0 && qualifiedAt <= invalidReceiverAt {
			// Equal or reversed receiver endpoints cannot manufacture a
			// sample. A later independently timed interval ends this hold.
			return 0, self.total, hold, holdByteCount, 0
		}
		if retain && !heldAfterRejection {
			self.qualityServiceMeasured = true
			// Only controller acceptance commits this fresh pair's boundary.
			// Later old bytes still count delivery, but cannot re-enter its rate.
			if self.feedbackPending && epochAt.After(self.feedbackCycleBefore) {
				self.serviceEpochAt = epochAt
				self.serviceEpochByteBase = self.total - epochByteCount
				self.feedbackCycleBefore, self.feedbackDrainAt = time.Time{}, time.Time{}
				// Preserve timing provenance for repeated reads of these same ACKs.
				self.feedbackComplete = self.feedbackFresh
				if self.feedbackFresh.bytes > 0 {
					self.feedbackCycle = self.feedbackFresh
				}
				self.feedbackFresh = windowServiceSample{}
				self.feedbackPending, self.feedbackDrainPending = false, 0
			}
			if !completedFallback && count > 0 && samples[0].lastAtNanos >= self.feedbackComplete.lastAtNanos {
				self.feedbackComplete = windowServiceSample{}
			}
			self.serviceHoldRate = rate
			if rate == 0 {
				self.serviceHoldRate = latest
			}
			self.serviceHoldByteCount, self.serviceHoldEpochAt = epochByteCount, epochAt
			if self.roundTripProbe.resetService || self.roundTripProbe.resetInitialService {
				// A sibling may deliver fresh service before this probe's ACK.
				// Later confirmation cannot restore the superseded held rate.
				self.roundTripProbe.serviceRate = self.serviceHoldRate
				self.roundTripProbe.serviceByteCount, self.roundTripProbe.serviceEpochAt = epochByteCount, epochAt
			}
		}
	} else {
		latest = hold
		return rate, self.total, latest, holdByteCount, 0
	}
	if heldAfterRejection {
		return rate, self.total, latest, holdByteCount, 0
	}
	proofAtNanos := rateAtNanos
	if rate == 0 {
		proofAtNanos = latestAtNanos
	}
	return rate, self.total, latest, epochByteCount, proofAtNanos
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
	if !self.acceptQualityRoundTripWithLock(roundTrip, at) {
		return
	}
	if roundTrip > 0 && self.receiverRoundTrips != nil && !at.Before(self.lastRoundTrip) {
		self.receiverRoundTrips.add(roundTrip, -1, compression, at)
	}
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
	return self.roundTripEvidenceWithLock(time.Now()).minimum
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
	if !self.lastRoundTrip.IsZero() && at.Sub(self.lastRoundTrip) >= time.Minute {
		// A quiet service replaces its path baseline; discovery and the held
		// pace restart with it.
		self.queueObservedAt, self.heldPacingRate = time.Time{}, 0
	}
	if self.minRoundTrip == 0 || roundTrip < self.minRoundTrip || unqueued || at.Sub(self.lastRoundTrip) >= time.Minute {
		self.minRoundTrip = roundTrip
	}
	self.lastRoundTrip = at
	self.latestRoundTrip = roundTrip
	self.compression = max(0, compression)
	slots := time.Duration(len(self.samples) - 2)
	timing := self.roundTripEvidenceWithLock(at)
	residenceInterval := timing.residence / slots
	if timing.residence%slots != 0 {
		residenceInterval++
	}
	interval := max(deliverySizedWindowSampleInterval, self.compression/4, residenceInterval)
	// Cache on feedback, not on each packet reservation: timing already
	// owns the bounded receiver-ring scan here. Measurement buckets and
	// their ACK-compression qualification remain completely unchanged.
	self.dispatchInterval = min(interval, max(windowPacingMinimumBurstTime, timing.minimum/4))
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
			if sample.bucket != bucket || sample.bytes <= 0 && !sample.queued || !self.serviceEpochAt.IsZero() && sample.firstAtNanos < self.serviceEpochAt.UnixNano() {
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
			if sample.bytes == 0 {
				if destination.bytes == 0 {
					*destination = sample
					destination.bucket = bucket
				} else {
					destination.queued = true
				}
				continue
			}
			if destination.bytes == 0 {
				queued := destination.queued
				*destination = sample
				destination.bucket = bucket
				destination.queued = destination.queued || queued
				continue
			}
			destination.mergeSupply(sample)
			if sample.firstAtNanos < destination.firstAtNanos {
				destination.firstAtNanos, destination.firstBytes = sample.firstAtNanos, sample.firstBytes
				destination.firstQueued = sample.firstQueued
			} else if sample.firstAtNanos == destination.firstAtNanos {
				destination.firstBytes += sample.firstBytes
				destination.firstQueued = destination.firstQueued || sample.firstQueued
			}
			if destination.lastAtNanos < sample.lastAtNanos {
				destination.lastQueued = sample.lastQueued
			} else if sample.lastAtNanos == destination.lastAtNanos {
				destination.lastQueued = destination.lastQueued || sample.lastQueued
			}
			destination.lastAtNanos = max(destination.lastAtNanos, sample.lastAtNanos)
			destination.bytes += sample.bytes
			destination.queued = destination.queued || sample.queued
		}
	}
	self.bucketInterval = interval
	roundTripCompression := self.compression
	if timing.count > 0 {
		roundTripCompression = 0
	}
	if !at.Before(self.serviceEpochAt) {
		self.observeQueueWithLock(timing, roundTripCompression, at)
	}
	if !at.Before(self.serviceEpochAt) && timing.minimum > 0 && timing.latest > timing.minimum+roundTripCompression+2*time.Millisecond {
		// Timing arrives before worker accounting. Preserve the queued bucket
		// even when newer clean observations rotate out its timing tuple.
		bucket := at.UnixNano() / int64(interval)
		if self.hasSamples && bucket <= self.newestBucket-int64(len(self.samples)) {
			return
		}
		if !self.hasSamples || bucket > self.newestBucket {
			self.newestBucket = bucket
		}
		self.hasSamples = true
		index := (bucket%int64(len(self.samples)) + int64(len(self.samples))) % int64(len(self.samples))
		sample := &self.samples[index]
		if sample.bucket != bucket {
			*sample = windowServiceSample{bucket: bucket, firstAtNanos: at.UnixNano(), lastAtNanos: at.UnixNano()}
		}
		sample.queued = true
	}
}

// Before one service residence has been delivered, excess flight can still be
// a fast opening train in propagation. Require queue-delay evidence then.
func (self *windowPacingService) backlogged(rate ByteCount) bool {
	return self.backloggedAt(rate, time.Now())
}

// Statistics and admission use the same caller clock without retiring RTT
// samples. Receiver waiting is flight residence, not network queue delay.
func (self *windowPacingService) backloggedAt(rate ByteCount, at time.Time) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.backloggedWithLock(rate, rate, at)
}

// Newer queue timing can come from an interval that failed coverage. Only
// independently excess old-pace flight may then revoke the admitted pace.
func (self *windowPacingService) backloggedCapacityAt(rate ByteCount, proofAtNanos int64, at time.Time) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	flightRate := rate
	if proofAtNanos == 0 || proofAtNanos < self.lastRoundTrip.UnixNano() || proofAtNanos > at.UnixNano() {
		flightRate = max(flightRate, self.heldPacingRate)
	}
	return self.backloggedWithLock(rate, flightRate, at)
}

// Explicit-rate callers retain their existing flight proof. Capacity reads
// may require the higher independent grant without changing either rate.
func (self *windowPacingService) backloggedWithLock(rate, flightRate ByteCount, at time.Time) bool {
	// An unacknowledged original may have been lost, not queued. Recovery
	// ACKs retain byte credit but cannot provide an unambiguous RTT. Require
	// fresh timing before repeatedly pricing their own paced cadence below
	// service; otherwise missing flight creates a multiplicative slowdown.
	if !self.lastRecoveryWriteAt.IsZero() && !self.lastRoundTrip.After(self.lastRecoveryWriteAt) {
		return false
	}
	timing := self.roundTripEvidenceWithLock(at)
	if timing.count == 0 && timing.minimum <= 0 || rate <= 0 {
		return false
	}
	bound := self.flightBoundAtWithLock(flightRate, timing.residence)
	compression := self.compression
	if timing.count > 0 {
		compression = 0
	}
	// A lower delivery estimate can shrink its own flight allowance after a
	// permission-limited or compressed turn. Historical bytes do not let that
	// lower bound revoke an admitted faster pace without queue residence.
	if (float64(max(self.total, self.drainedSent)) < bound || rate < self.heldPacingRate) &&
		timing.latest <= timing.minimum+compression+2*time.Millisecond {
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
	return self.flightBoundAtWithLock(rate, self.roundTripEvidenceWithLock(time.Now()).residence)
}

// The residence is either one complete receiver tuple or the unchanged
// legacy minimum plus advertised compression.
func (self *windowPacingService) flightBoundAtWithLock(rate ByteCount, residence time.Duration) float64 {
	burst := float64(max(self.maxMessageByteCount, self.burstMeter.limit, self.feedbackBurstByteCount))
	if self.burstMeter.limit <= 0 {
		burst += float64(rate) * (2 * time.Millisecond).Seconds()
	}
	return burst + float64(rate)*residence.Seconds()
}

func (self *windowBurstPacer) close() {
	if self.timer != nil {
		self.timer.Stop()
	}
	if self.service != nil {
		self.service.stateLock.Lock()
		if self.serviceClosed {
			self.service.stateLock.Unlock()
			return
		}
		self.serviceClosed = true
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
				self.service.unprovableWriteCount = self.service.pendingWrites
				self.service.abortDrainWithLock()
				self.service.drained = false
				self.service.sourceIdleAt = time.Time{}
			}
			delete(self.service.writes, self.serviceSequenceId)
		}
		if self.service.roundTripProbe.sequenceId == self.serviceSequenceId {
			self.service.roundTripProbe = windowPacingRoundTripProbe{}
		}
		self.service.notifyDrainWithLock()
		self.serviceSent = 0
		self.serviceAcked = 0
		self.service.stateLock.Unlock()
	}
}

// A physically proved path increase shares one bounded history boundary.
// Statistics only read it; ordinary queued observations cannot advance it.
func (self *windowPacingService) windowDeliveryStep() int64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.windowDeliveryAfterNanos
}
