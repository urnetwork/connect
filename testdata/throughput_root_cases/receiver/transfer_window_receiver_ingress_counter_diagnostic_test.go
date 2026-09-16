// This isolated diagnostic carries exact ingress snapshots through a bounded
// test ledger. It does not change the wire or read a configured serializer rate.
package connect

import (
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

const ingressOfferTrainTrace = false
const ingressControlledOfferTrainEnabled = true

// Receiver-owned service identity keeps independent peers and options apart.
type ingressCounterDiagnosticKey struct {
	receiver          Id
	source            Id
	forceStream       bool
	companionContract bool
	role              sequenceTlsRole
	companion         bool
}

// The tuple is captured once at the named message's first physical ingress.
type ingressCounterDiagnosticTuple struct {
	generation     uint64
	bytes          uint64
	at             int64
	physicalBytes  uint64
	duplicateBytes uint64
}

// Every accepted pair retains its actual sender and receiver elapsed time.
type ingressCounterDiagnosticRate struct {
	at               int64
	rate             ByteCount
	offeredRate      ByteCount
	bytes            uint64
	physicalBytes    uint64
	duplicateBytes   uint64
	senderStart      int64
	senderEnd        int64
	receiverStart    int64
	receiverEnd      int64
	previousAckAt    int64
	previousLane     uint32
	lane             uint32
	queueCount       int
	queueBytes       ByteCount
	packCount        int
	admissionBlocked bool
	firstOffer       ingressCounterDiagnosticOffer
	lastOffer        ingressCounterDiagnosticOffer
}

// A sender retains one anchor and a fixed ring of recent complete pairs.
type ingressCounterDiagnosticSender struct {
	last        ingressCounterDiagnosticTuple
	sentAt      int64
	ackAt       int64
	rates       [64]ingressCounterDiagnosticRate
	next        int
	latest      ByteCount
	firstSentAt int64
	lane        uint32
	traceCount  int
	lastOffer   ingressCounterDiagnosticOffer
}

// These fields record existing proof state; they do not define a new policy.
type ingressCounterDiagnosticOffer struct {
	at            int64
	epoch         uint64
	drained       bool
	pendingWrites int
	reservations  int
	sourceIdleAt  int64
	train         uint64
}

// A worker's real wait is recorded separately from its paced physical writes.
type ingressOfferWaitDiagnostic struct {
	firstAt        int64
	service        *windowPacingService
	waiting        bool
	train          uint64
	at             int64
	blocked        bool
	sourceEmpty    bool
	schedulerCount int
	packCount      int
	events         int
}

// All test state has explicit caps and is reset only after client workers join.
var ingressCounterDiagnostic = struct {
	stateLock            sync.Mutex
	waits                map[Id]*ingressOfferWaitDiagnostic
	trains               map[*windowPacingService]uint64
	pauses               map[*windowPacingService]time.Time
	controlledBoundaries int
	windowBoundaries     int
	sourceBoundaries     int
	offers               map[Id]ingressCounterDiagnosticOffer
	offerOrder           [4096]Id
	offerNext            int
	offerEpochs          map[*windowPacingService]uint64
	enabled              bool
	nextGeneration       uint64
	traffic              map[ingressCounterDiagnosticKey]ingressCounterDiagnosticTuple
	groups               map[ingressCounterDiagnosticKey]ingressCounterDiagnosticTuple
	tuples               map[Id]ingressCounterDiagnosticTuple
	order                [4096]Id
	next                 int
	senders              map[*windowPacingService]*ingressCounterDiagnosticSender
	pairs                int
	resets               int
}{pauses: make(map[*windowPacingService]time.Time), trains: make(map[*windowPacingService]uint64), waits: make(map[Id]*ingressOfferWaitDiagnostic), offers: make(map[Id]ingressCounterDiagnosticOffer), offerEpochs: make(map[*windowPacingService]uint64), traffic: make(map[ingressCounterDiagnosticKey]ingressCounterDiagnosticTuple), groups: make(map[ingressCounterDiagnosticKey]ingressCounterDiagnosticTuple), tuples: make(map[Id]ingressCounterDiagnosticTuple), senders: make(map[*windowPacingService]*ingressCounterDiagnosticSender)}

// Called after decoding identity, with time and outer size captured before unwrap.
func recordIngressCounterDiagnostic(receiver, source Id, pack *protocol.Pack, role sequenceTlsRole, companion bool, transport TransportType, bytes ByteCount, at time.Time) {
	if pack == nil || pack.Nack || transport != TransportTypeH1 || bytes <= 0 {
		return
	}
	messageId, err := IdFromBytes(pack.MessageId)
	if err != nil {
		return
	}
	d := &ingressCounterDiagnostic
	d.stateLock.Lock()
	defer d.stateLock.Unlock()
	if !d.enabled {
		return
	}
	key := ingressCounterDiagnosticKey{receiver: receiver, source: source, forceStream: pack.ForceStream, companionContract: pack.CompanionContract, role: role, companion: companion}
	traffic, exists := d.traffic[key]
	if !exists && len(d.traffic) >= 128 {
		return
	}
	traffic.physicalBytes += uint64(bytes)
	if _, seen := d.tuples[messageId]; seen {
		traffic.duplicateBytes += uint64(bytes)
		d.traffic[key] = traffic
		tuple := d.groups[key]
		tuple.bytes += uint64(bytes)
		tuple.at = at.UnixNano()
		tuple.physicalBytes, tuple.duplicateBytes = traffic.physicalBytes, traffic.duplicateBytes
		d.groups[key] = tuple
		return
	}
	d.traffic[key] = traffic
	tuple, exists := d.groups[key]
	if !exists {
		if len(d.groups) >= 128 {
			return
		}
		d.nextGeneration++
		tuple.generation = d.nextGeneration
	}
	tuple.bytes += uint64(bytes)
	tuple.physicalBytes, tuple.duplicateBytes = traffic.physicalBytes, traffic.duplicateBytes
	tuple.at = at.UnixNano()
	d.groups[key] = tuple
	old := d.order[d.next]
	delete(d.tuples, old)
	d.order[d.next] = messageId
	d.next = (d.next + 1) % len(d.order)
	d.tuples[messageId] = tuple
}

// A snapshot becomes available only when its actual ACK names that message.
// Queue-protected first-write timing limits the diagnostic to confirmed H1.
func observeIngressCounterDiagnostic(sequence *SendSequence, ack receiveAckMessage) {
	service := sequence.windowPacer.service
	if service == nil || sequence.resendQueue == nil {
		return
	}
	var sentAt int64
	var queueCount int
	var queueBytes ByteCount
	func() {
		sequence.resendQueue.stateLock.Lock()
		defer sequence.resendQueue.stateLock.Unlock()
		queueCount, queueBytes = len(sequence.resendQueue.orderedItems), sequence.resendQueue.byteCount
		item := sequence.resendQueue.messageIdItems[ack.messageId]
		if item != nil && item.rttH1 && !item.serviceCreditObserved && item.rttState == sendItemRttObserved {
			sentAt = item.pacingSentAtNanos
		}
	}()
	if sentAt == 0 {
		return
	}
	d := &ingressCounterDiagnostic
	d.stateLock.Lock()
	defer d.stateLock.Unlock()
	if !d.enabled {
		return
	}
	tuple, exists := d.tuples[ack.messageId]
	if !exists {
		return
	}
	sender := d.senders[service]
	if sender == nil {
		if len(d.senders) >= 128 {
			return
		}
		sender = &ingressCounterDiagnosticSender{firstSentAt: sentAt}
		d.senders[service] = sender
	}
	last := sender.last
	if wait := d.waits[sequence.sequenceId]; wait != nil {
		offset := ack.receivedAtNanos - wait.firstAt
		if int64(2*time.Second) <= offset && offset < int64(3*time.Second) && ingressOfferTrainTrace && wait.events < 768 {
			wait.events++
			fmt.Printf("offer-wait-ack offset-nanos=%d train=%d previous-train=%d sender-offset-nanos=%d receiver-offset-nanos=%d bytes=%d same-lane=%t\n", offset, d.offers[ack.messageId].train, sender.lastOffer.train, sentAt-wait.firstAt, tuple.at-wait.firstAt, tuple.bytes-last.bytes, sequence.logicalLane == sender.lane)
		}
	}
	if tuple.generation != last.generation || tuple.bytes <= last.bytes || tuple.at <= last.at {
		if tuple.generation == last.generation && last.bytes != 0 {
			return
		}
	} else if currentOffer := d.offers[ack.messageId]; sender.lastOffer.train != 0 && currentOffer.train != 0 && sender.lastOffer.train != currentOffer.train {
		// An actual all-worker source/window wait lies between these offers.
		// One compressed head from the new train cannot price that idle.
		d.resets++
	} else if sentAt >= sender.ackAt {
		// The next named envelope was not offered before the prior feedback.
		// That source idle cannot create a lower serialization estimate.
		d.resets++
	} else {
		span := tuple.at - last.at
		rate := ByteCount(float64(tuple.bytes-last.bytes) * float64(time.Second) / float64(span))
		if rate > 0 {
			offeredRate := ByteCount(math.MaxInt64)
			if offeredSpan := sentAt - sender.sentAt; offeredSpan > 0 {
				offeredRate = ByteCount(float64(tuple.bytes-last.bytes) * float64(time.Second) / float64(offeredSpan))
			}
			sender.rates[sender.next] = ingressCounterDiagnosticRate{
				at: ack.receivedAtNanos, rate: rate, offeredRate: offeredRate,
				bytes: tuple.bytes - last.bytes, physicalBytes: tuple.physicalBytes - last.physicalBytes,
				duplicateBytes: tuple.duplicateBytes - last.duplicateBytes,
				senderStart:    sender.sentAt, senderEnd: sentAt,
				receiverStart: last.at, receiverEnd: tuple.at, previousAckAt: sender.ackAt,
				previousLane: sender.lane, lane: sequence.logicalLane,
				queueCount: queueCount, queueBytes: queueBytes,
				packCount: len(sequence.packs), admissionBlocked: sequence.resendCapacityUnavailable.Load(), firstOffer: sender.lastOffer, lastOffer: d.offers[ack.messageId],
			}
			sender.next = (sender.next + 1) % len(sender.rates)
			sender.latest = rate
			d.pairs++
		}
	}
	sender.last, sender.sentAt, sender.ackAt = tuple, sentAt, ack.receivedAtNanos
	sender.lane = sequence.logicalLane
	sender.lastOffer = d.offers[ack.messageId]
}

// Read-only rate selection uses the existing sampler's bounded local horizon.
func ingressCounterDiagnosticEstimate(service *windowPacingService, horizon time.Duration, now time.Time) (ByteCount, ByteCount, bool) {
	d := &ingressCounterDiagnostic
	d.stateLock.Lock()
	defer d.stateLock.Unlock()
	if !d.enabled {
		return 0, 0, false
	}
	sender := d.senders[service]
	if sender == nil {
		return 0, 0, false
	}
	if sender.latest == 0 {
		// A first endpoint supplies no receiver rate and cannot own discovery.
		return 0, 0, false
	}
	var rate, latest ByteCount
	var latestAt int64
	held := service.serviceHoldRate
	for _, sample := range sender.rates {
		if sample.at > now.UnixNano() || sample.rate <= 0 {
			continue
		}
		if sample.rate < held && sample.offeredRate <= sample.rate {
			continue
		}
		if sample.at > latestAt {
			latest, latestAt = sample.rate, sample.at
		}
		if now.Add(-horizon).UnixNano() <= sample.at {
			rate = max(rate, sample.rate)
		}
	}
	if latest == 0 {
		latest = held
	}
	return rate, latest, true
}

// Keep every original phase, byte limit, duration and acceptance gate unchanged.
func startIngressCounterDiagnostic(t *testing.T) func() {
	t.Helper()
	d := &ingressCounterDiagnostic
	d.stateLock.Lock()
	d.enabled = true
	d.waits = make(map[Id]*ingressOfferWaitDiagnostic)
	d.trains = make(map[*windowPacingService]uint64)
	d.pauses = make(map[*windowPacingService]time.Time)
	d.controlledBoundaries, d.windowBoundaries, d.sourceBoundaries = 0, 0, 0
	d.offers = make(map[Id]ingressCounterDiagnosticOffer)
	d.offerOrder, d.offerNext = [4096]Id{}, 0
	d.offerEpochs = make(map[*windowPacingService]uint64)
	d.traffic = make(map[ingressCounterDiagnosticKey]ingressCounterDiagnosticTuple)
	d.groups = make(map[ingressCounterDiagnosticKey]ingressCounterDiagnosticTuple)
	d.tuples = make(map[Id]ingressCounterDiagnosticTuple)
	d.senders = make(map[*windowPacingService]*ingressCounterDiagnosticSender)
	d.order, d.next, d.pairs, d.resets = [4096]Id{}, 0, 0, 0
	d.stateLock.Unlock()
	return func() {
		d.stateLock.Lock()
		defer d.stateLock.Unlock()
		t.Logf("receiver-ingress pairs=%d source-resets=%d retained-tuples=%d groups=%d", d.pairs, d.resets, len(d.tuples), len(d.groups))
		t.Logf("offer-train window-boundaries=%d source-boundaries=%d controlled-boundaries=%d", d.windowBoundaries, d.sourceBoundaries, d.controlledBoundaries)
		d.enabled = false
	}
}

// Same physical model and original threshold; only available information changes.
func TestWindowPathReceiverIngressCounterShrinkDiagnostic(t *testing.T) {
	assertMessagePoolOwnership(t)
	defer startIngressCounterDiagnostic(t)()
	checkWindowMismatchCell(t, windowPathCell{SendWindow: mib(48), ReceiveWindow: mib(2), ReceiveWindowAfter: kib(64), WindowChangeAfter: 2 * time.Second, Warmup: 4 * time.Second, RoundTrip: 100 * time.Millisecond, Compression: 50 * time.Millisecond, Flows: 8, Rate: 12500000})
}

// The same receiver evidence must still adapt to actual slower serialization.
func TestWindowPathReceiverIngressCounterRateChangesDiagnostic(t *testing.T) {
	defer startIngressCounterDiagnostic(t)()
	TestWindowPathServiceCapacityChanges(t)
}

// The original long propagation change keeps its unchanged capacity and loss gate.
func TestWindowPathReceiverIngressCounterLongRoundTripDiagnostic(t *testing.T) {
	defer startIngressCounterDiagnostic(t)()
	TestWindowPathServiceRoundTripGrowthBeyondOldRing(t)
}

// Runs only at an existing retaining controller read, with the service lock held.
// The bounded trace does not call a sampler, mutate evidence, or inspect peer state.
func traceIngressCounterDiagnosticRepriceWithLock(service *windowPacingService, horizon time.Duration, now time.Time, rate, latest ByteCount) {
	oldRate, newRate := service.serviceHoldRate, max(rate, latest)
	if newRate <= 0 || oldRate <= newRate {
		return
	}
	d := &ingressCounterDiagnostic
	d.stateLock.Lock()
	defer d.stateLock.Unlock()
	sender := d.senders[service]
	if !ingressOfferTrainTrace || sender == nil || sender.traceCount >= 128 || now.UnixNano()-sender.firstSentAt < int64(2*time.Second) {
		return
	}
	sender.traceCount++
	fmt.Printf("reprice n=%d offset-nanos=%d old=%d new=%d peak=%d latest=%d horizon-nanos=%d outstanding=%.3f reserved=%d reservations=%d total=%d sent=%d drained=%t pending-writes=%d drain-left-nanos=%d raw-rtt-nanos=%d min-rtt-nanos=%d feedback-pending=%t feedback-drain-pending=%d probe-written=%t probe-reset=%t probe-pending-bytes=%d\n", sender.traceCount, now.UnixNano()-sender.firstSentAt, oldRate, newRate, rate, latest, horizon, service.outstandingWithLock(), service.reservedByteCount, service.pacingReservations, service.total, service.sent, service.drained, service.pendingWrites, service.drainUntil.Sub(now), service.latestRoundTrip, service.minRoundTrip, service.feedbackPending, service.feedbackDrainPending, service.roundTripProbe.written, service.roundTripProbe.resetService, service.roundTripProbe.pendingByteCount)
	var latestAt int64
	for _, sample := range sender.rates {
		if sample.at <= now.UnixNano() && sample.at > latestAt {
			latestAt = sample.at
		}
	}
	for _, sample := range sender.rates {
		if sample.rate == 0 || (sender.traceCount > 4 && sample.at != latestAt && sample.rate != rate) {
			continue
		}
		eligible := sample.at <= now.UnixNano() && !(sample.rate < oldRate && sample.offeredRate <= sample.rate)
		recent := sample.at >= now.Add(-horizon).UnixNano() && sample.at <= now.UnixNano()
		fmt.Printf("pair n=%d at-offset-nanos=%d rate=%d offered-rate=%d unique-bytes=%d physical-bytes=%d duplicate-bytes=%d receiver-span-nanos=%d offer-span-nanos=%d ack-span-nanos=%d receiver-start-offset-nanos=%d offer-start-offset-nanos=%d offer-end-offset-nanos=%d previous-lane=%d lane=%d queue-count=%d queue-bytes=%d pack-count=%d admission-blocked=%t eligible=%t recent=%t offer-first-epoch=%d offer-last-epoch=%d first-offer-drained=%t last-offer-drained=%t first-offer-tails=%d last-offer-tails=%d first-offer-reservations=%d last-offer-reservations=%d first-offer-idle-at=%d last-offer-idle-at=%d first-offer-train=%d last-offer-train=%d\n", sender.traceCount, sample.at-sender.firstSentAt, sample.rate, sample.offeredRate, sample.bytes, sample.physicalBytes, sample.duplicateBytes, sample.receiverEnd-sample.receiverStart, sample.senderEnd-sample.senderStart, sample.at-sample.previousAckAt, sample.receiverStart-sender.firstSentAt, sample.senderStart-sender.firstSentAt, sample.senderEnd-sender.firstSentAt, sample.previousLane, sample.lane, sample.queueCount, sample.queueBytes, sample.packCount, sample.admissionBlocked, eligible, recent, sample.firstOffer.epoch, sample.lastOffer.epoch, sample.firstOffer.drained, sample.lastOffer.drained, sample.firstOffer.pendingWrites, sample.lastOffer.pendingWrites, sample.firstOffer.reservations, sample.lastOffer.reservations, sample.firstOffer.sourceIdleAt, sample.lastOffer.sourceIdleAt, sample.firstOffer.train, sample.lastOffer.train)
	}
}

// Called immediately before the existing begin-write transition consumes drain
// proof. Service-to-ledger lock order matches the read-only diagnostic consumer.
func recordIngressOfferDiagnosticWithLock(service *windowPacingService, sequenceId, messageId Id, at time.Time, resend bool) {
	d := &ingressCounterDiagnostic
	d.stateLock.Lock()
	defer d.stateLock.Unlock()
	if !d.enabled {
		return
	}
	if pause, exists := d.pauses[service]; exists {
		if at.After(pause) {
			d.trains[service]++
			d.controlledBoundaries++
		}
		delete(d.pauses, service)
	}
	if resend {
		return
	}
	epoch, exists := d.offerEpochs[service]
	if !exists && len(d.offerEpochs) >= 128 {
		return
	}
	if service.drained {
		epoch++
	}
	d.offerEpochs[service] = epoch
	if _, exists := d.offers[messageId]; exists {
		return
	}
	old := d.offerOrder[d.offerNext]
	delete(d.offers, old)
	d.offerOrder[d.offerNext] = messageId
	d.offerNext = (d.offerNext + 1) % len(d.offerOrder)
	var idleAt int64
	if !service.sourceIdleAt.IsZero() {
		idleAt = service.sourceIdleAt.UnixNano()
	}
	wait := d.waits[sequenceId]
	if wait == nil && len(d.waits) < 128 {
		wait = &ingressOfferWaitDiagnostic{firstAt: at.UnixNano(), service: service}
		d.waits[sequenceId] = wait
	}
	var train uint64
	if wait != nil {
		if d.trains[service] == 0 {
			d.trains[service] = 1
		}
		train = d.trains[service]
		if offset := at.UnixNano() - wait.firstAt; int64(2*time.Second) <= offset && offset < int64(3*time.Second) && ingressOfferTrainTrace && wait.events < 768 {
			wait.events++
			fmt.Printf("offer-wait-write offset-nanos=%d train=%d drained=%t tails=%d reservations=%d\n", offset, train, service.drained, service.pendingWrites, service.pacingReservations)
		}
	}
	d.offers[messageId] = ingressCounterDiagnosticOffer{train: train, at: at.UnixNano(), epoch: epoch, drained: service.drained, pendingWrites: service.pendingWrites, reservations: service.pacingReservations, sourceIdleAt: idleAt}
}

// The worker publishes its actual select boundary. Local pacing stays within
// a write and therefore cannot masquerade as source or window starvation.
func recordIngressOfferWaitStart(sequence *SendSequence, at time.Time, blocked bool, schedulerCount int) {
	d := &ingressCounterDiagnostic
	d.stateLock.Lock()
	defer d.stateLock.Unlock()
	if !d.enabled {
		return
	}
	wait := d.waits[sequence.sequenceId]
	if wait == nil {
		return
	}
	wait.at = at.UnixNano()
	wait.blocked = blocked
	wait.packCount, wait.schedulerCount = len(sequence.packs), schedulerCount
	wait.sourceEmpty = wait.packCount == 0 && schedulerCount == 0
	wait.waiting = blocked || wait.sourceEmpty
}

// The isolated registry is bounded to 128 lanes. An active sibling prevents a
// service-wide boundary; all lanes must have spent actual time waiting.
func recordIngressOfferWaitEnd(sequence *SendSequence, at time.Time) {
	d := &ingressCounterDiagnostic
	d.stateLock.Lock()
	defer d.stateLock.Unlock()
	if !d.enabled {
		return
	}
	wait := d.waits[sequence.sequenceId]
	if wait == nil || !wait.waiting {
		return
	}
	allWaiting, waitingAt := true, wait.at
	for _, sibling := range d.waits {
		if sibling.service != wait.service {
			continue
		}
		if !sibling.waiting {
			allWaiting = false
			break
		}
		waitingAt = max(waitingAt, sibling.at)
	}
	if allWaiting && waitingAt < at.UnixNano() {
		d.trains[wait.service]++
		if wait.blocked {
			d.windowBoundaries++
		} else {
			d.sourceBoundaries++
		}
	}
	wait.waiting = false
	wait.at = 0
}

// Called only when the real admission method returns a controlled drain wait.
// Ending or invalidating physical proof does not erase the known local pause.
func recordIngressControlledPauseWithLock(service *windowPacingService, at time.Time) {
	if !ingressControlledOfferTrainEnabled {
		return
	}
	d := &ingressCounterDiagnostic
	d.stateLock.Lock()
	defer d.stateLock.Unlock()
	if !d.enabled || len(d.pauses) >= 128 {
		return
	}
	if _, exists := d.pauses[service]; !exists {
		d.pauses[service] = at
	}
}

// Reuse the exact ablation's two filter-only matrix rows and original gates.
func TestWindowPathReceiverOfferTrainShortServiceDiagnostic(t *testing.T) {
	defer startIngressCounterDiagnostic(t)()
	TestWindowPathServicePerformanceMatrix(t)
}

// Cold startup has no earlier large window from which to retain capacity.
func TestWindowPathReceiverOfferTrainColdSmallWindowDiagnostic(t *testing.T) {
	assertMessagePoolOwnership(t)
	defer startIngressCounterDiagnostic(t)()
	checkWindowMismatchCell(t, windowPathCell{SendWindow: mib(48), ReceiveWindow: kib(64), Warmup: 4 * time.Second, RoundTrip: 100 * time.Millisecond, Compression: 50 * time.Millisecond, Flows: 8, Rate: 12500000})
}
