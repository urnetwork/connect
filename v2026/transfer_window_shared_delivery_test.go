// Qualified lane delivery is consumed by the real shared pacing clock. The
// utilization checks below are policy objectives; byte/probe ownership and
// exclusion of unrelated evidence are independent hard invariants.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

// Every physical write fits 8 KiB. A cumulative head may cover several such
// writes, and its complete flight remains within the actual opening window.
const windowSharedDeliveryMessageBytes = ByteCount(8 * 1024)

// No sockets or background source are needed: confirmed initial H1 items,
// once-only cumulative credit and virtual clock events drive service ownership.
type windowSharedDeliveryFixture struct {
	sequences []*SendSequence
	numbers   []uint64
	published map[*windowPacingService]ByteCount
	period    time.Duration
}

// Groups identify independent destination services. Members of one group
// include idle references, so reference count is never a capacity multiplier.
func newWindowSharedDeliveryFixture(t *testing.T, groups []int) *windowSharedDeliveryFixture {
	t.Helper()
	fixture := &windowSharedDeliveryFixture{
		numbers: make([]uint64, len(groups)), published: map[*windowPacingService]ByteCount{},
	}
	services := map[int]*windowPacingService{}
	for _, group := range groups {
		sequence, _ := newWindowCumulativePacingFixture(t)
		sequence.observeReceiveWindowAdvertisement(receiveAckMessage{
			receiveWindowSet: true, receiveWindowByteCount: 8 * 1024 * 1024,
			ackCompressTimeoutSet: true, ackCompressTimeoutMicros: 10000,
		})
		sequence.receiveWindowSetAtNanos.Store(time.Now().UnixNano())
		service := services[group]
		if service == nil {
			service = sequence.windowPacer.service
			services[group] = service
		}
		service.references++
		sequence.windowPacer.service = service
		sequence.windowPacer.serviceSequenceId = sequence.sequenceId
		fixture.sequences = append(fixture.sequences, sequence)
	}
	t.Cleanup(func() {
		for _, sequence := range fixture.sequences {
			sequence.windowPacer.close()
		}
		for service, published := range fixture.published {
			if service.sent != published || service.total != published || service.pacingReservations != 0 ||
				service.reservedByteCount != 0 || service.waiterHead != nil || service.waiterTail != nil {
				t.Errorf("shared reservation cleanup lost byte ownership: sent=%d total=%d published=%d reservations=%d reserved=%d",
					service.sent, service.total, published, service.pacingReservations, service.reservedByteCount)
			}
		}
	})
	return fixture
}

// Each physical write has a real retained item before carrier confirmation.
// Its frame allocation makes the existing resend queue own the same bytes.
func (self *windowSharedDeliveryFixture) recordWrite(t *testing.T, index int, messageId Id, bytes ByteCount, at time.Time) {
	t.Helper()
	sequence := self.sequences[index]
	item := &sendItem{
		transferItem: transferItem{messageId: messageId, sequenceNumber: self.numbers[index]},
		sendTime:     at, sendCount: 1, expectsAck: true, transferFrameBytes: make([]byte, int(bytes)),
		pacingByteCount: bytes, pacingSentAtNanos: at.UnixNano(), rttState: sendItemRttWritePending,
	}
	sequence.resendQueue.Add(item)
	sequence.finishReceiverRttWrite(item, transferWriteDisposition{transportType: TransportTypeH1, reliable: true}, nil)
	if !item.rttH1 || item.rttState != sendItemRttWriteConfirmed {
		t.Fatal("physical H1 write did not retain confirmed offer provenance")
	}
	if _, queued := sequence.resendQueue.QueueSize(); queued > sequence.sendBufferSettings.ResendQueueMaxByteCount {
		t.Fatal("retained physical items exceeded their current byte permission")
	}
}

// Cumulative publication traverses the real once-only credit owner. A repeated
// cumulative head and late SACK cannot contribute another byte to either ring.
func (self *windowSharedDeliveryFixture) publish(t *testing.T, index int, head Id, bytes ByteCount, at time.Time) {
	t.Helper()
	sequence := self.sequences[index]
	before := sequence.windowPacer.serviceAcked
	sequence.publishAckServiceCredit(head, false, at)
	sequence.publishAckServiceCredit(head, false, at)
	sequence.publishAckServiceCredit(head, true, at)
	if sequence.windowPacer.serviceAcked-before != bytes {
		t.Fatal("cumulative/repeated/SACK credit did not preserve exact physical bytes")
	}
	var removed ByteCount
	firstSentAtNanos := int64(0)
	for {
		item := sequence.resendQueue.RemoveFirst()
		if item == nil {
			break
		}
		if !item.serviceCreditObserved {
			t.Fatal("uncredited physical item escaped the cumulative owner")
		}
		removed += item.pacingByteCount
		if firstSentAtNanos == 0 || item.pacingSentAtNanos < firstSentAtNanos {
			firstSentAtNanos = item.pacingSentAtNanos
		}
	}
	if removed != bytes {
		t.Fatal("the cumulative flight did not release its exact retained frames")
	}
	sequence.observeAckedBytesForWrite(bytes, 0, windowServiceAckCredit{}, at, firstSentAtNanos)
	self.published[sequence.windowPacer.service] += bytes
}

// Spend each service's opening allowance through actual reservations and
// confirmed writes, then ACK that bounded flight. No counter is injected to
// disable discovery, and an idle sibling never receives another allowance.
func (self *windowSharedDeliveryFixture) spendProbe(t *testing.T) {
	t.Helper()
	seen := map[*windowPacingService]bool{}
	for index, sequence := range self.sequences {
		service := sequence.windowPacer.service
		if seen[service] {
			continue
		}
		seen[service] = true
		estimate := sequence.sendWindowEstimate(time.Now())
		probeLimit := estimate.PacingProbeByteCount
		if probeLimit <= 0 || probeLimit > 2*estimate.Initial {
			t.Fatalf("opening probe exceeded its configured bound: %+v", estimate)
		}
		for service.probeSent < probeLimit {
			estimate = sequence.sendWindowEstimate(time.Now())
			flight := min(probeLimit-service.probeSent, estimate.Window)
			pacer := &sequence.windowPacer
			pacer.rate, pacer.estimateRate = estimate.PacingByteRate, estimate.ServiceByteRate
			pacer.probeRate, pacer.probeLimit = estimate.PacingProbeByteRate, estimate.PacingProbeByteCount
			started := time.Now()
			var head Id
			for remaining := flight; remaining > 0; {
				bytes := min(remaining, windowSharedDeliveryMessageBytes)
				self.numbers[index]++
				head = NewId()
				if err := pacer.waitForServiceMessage(context.Background(), int(bytes), false,
					sequence.sequenceId, head, self.numbers[index]); err != nil {
					t.Fatal(err)
				}
				self.recordWrite(t, index, head, bytes, time.Now())
				service.finishWrite(sequence.sequenceId, head, true)
				remaining -= bytes
			}
			// Compression permits two opening windows of discovery, but
			// each actual flight still waits for its own cumulative ACK.
			time.Sleep(10 * time.Millisecond)
			at := time.Now()
			sequence.rttWindow.closeSendTime(uint64(started.UnixMilli()), at)
			service.acknowledgeWrite(sequence.sequenceId, head, self.numbers[index], false, 10*time.Millisecond, at)
			self.publish(t, index, head, flight, at)
		}
		if service.probeSent != probeLimit {
			t.Fatalf("real opening reservations did not spend the probe: %d/%d", service.probeSent, probeLimit)
		}
	}
}

// Five complete flights leave contemporaneous multi-RTT logical intervals.
// Legacy sibling ACKs are ordered 10 us apart under a declared 10 ms hold;
// a fully drained turn therefore supplies no serialization-capacity pair.
func (self *windowSharedDeliveryFixture) deliver(t *testing.T, flights []ByteCount) {
	t.Helper()
	active := 0
	for _, bytes := range flights {
		if bytes > 0 {
			active++
		}
	}
	if len(flights) != len(self.sequences) || active == 0 {
		t.Fatal("invalid explicit delivery schedule")
	}
	self.period = 10*time.Millisecond + time.Duration(active-1)*10*time.Microsecond
	for range 5 {
		started := time.Now()
		heads := make([]Id, len(self.sequences))
		groupBytes := map[*windowPacingService]ByteCount{}
		for index, sequence := range self.sequences {
			bytes := flights[index]
			if bytes == 0 {
				continue
			}
			if bytes > sequence.sendWindowEstimate(started).Window {
				t.Fatal("a complete training flight exceeded its effective window")
			}
			service := sequence.windowPacer.service
			groupBytes[service] += bytes
			for remaining := bytes; remaining > 0; {
				messageBytes := min(remaining, windowSharedDeliveryMessageBytes)
				self.numbers[index]++
				heads[index] = NewId()
				estimate := sequence.sendWindowEstimate(time.Now())
				pacer := &sequence.windowPacer
				pacer.rate, pacer.estimateRate = estimate.PacingByteRate, estimate.ServiceByteRate
				pacer.probeRate, pacer.probeLimit = estimate.PacingProbeByteRate, estimate.PacingProbeByteCount
				if err := pacer.waitForServiceMessage(context.Background(), int(messageBytes), false, sequence.sequenceId, heads[index], self.numbers[index]); err != nil {
					t.Fatal(err)
				}
				self.recordWrite(t, index, heads[index], messageBytes, time.Now())
				service.finishWrite(sequence.sequenceId, heads[index], true)
				remaining -= messageBytes
			}
		}
		for _, bytes := range groupBytes {
			if float64(bytes)/self.period.Seconds() >= 125000000 {
				t.Fatal("training schedule exceeded the independent serializer or configured target")
			}
		}
		if remaining := time.Until(started.Add(10 * time.Millisecond)); remaining > 0 {
			time.Sleep(remaining)
		} else {
			t.Fatal("training exceeded its fixed feedback deadline")
		}
		first := true
		for index, sequence := range self.sequences {
			bytes := flights[index]
			if bytes == 0 {
				continue
			}
			if !first {
				time.Sleep(10 * time.Microsecond)
			}
			first = false
			at := time.Now()
			service := sequence.windowPacer.service
			sequence.rttWindow.closeSendTime(uint64(started.UnixMilli()), at)
			service.acknowledgeWrite(sequence.sequenceId, heads[index], self.numbers[index], false, 10*time.Millisecond, at)
			self.publish(t, index, heads[index], bytes, at)
		}
		for service := range groupBytes {
			rate, _, held := service.measured(time.Second, time.Now())
			if rate != 0 || held != 0 || !service.drained || service.pendingWrites != 0 ||
				service.sent != self.published[service] || service.total != self.published[service] {
				t.Fatalf("cold complete flights changed service or ownership: rate=%d held=%d drained=%t pending=%d sent=%d total=%d published=%d",
					rate, held, service.drained, service.pendingWrites, service.sent, service.total, self.published[service])
			}
		}
	}
}

// One fixed, still-fresh estimate controls at most three earlier flight
// amounts. The virtual deadline is their observed delivery duration, not a
// host-time performance threshold or an exact required estimator output.
type windowSharedDeliveryRelease struct {
	elapsed   time.Duration
	released  ByteCount
	requested ByteCount
	complete  bool
}

// Exercise both reservation and dispatch; summing reported lane rates would
// miss their consumption of one global timeline and one shared burst meter.
func (self *windowSharedDeliveryFixture) consume(t *testing.T, flights []ByteCount) windowSharedDeliveryRelease {
	t.Helper()
	started := time.Now()
	span := 3 * self.period
	ctx, cancel := context.WithDeadline(context.Background(), started.Add(span))
	defer cancel()
	remaining := make([]ByteCount, len(flights))
	probes := map[*windowPacingService]ByteCount{}
	offered := map[*windowPacingService]ByteCount{}
	result := windowSharedDeliveryRelease{complete: true}
	for index, sequence := range self.sequences {
		if flights[index] == 0 {
			continue
		}
		estimate := sequence.sendWindowEstimate(started)
		if !estimate.Sized || estimate.DeliveryByteRate <= 0 || estimate.ServiceByteRate != 0 ||
			3*flights[index] > estimate.Window || estimate.Window > estimate.Ceiling ||
			estimate.PacingByteRate > estimate.PacingProbeByteRate {
			t.Fatalf("consumer lost qualified cold evidence or bounds: lane=%d %+v", index, estimate)
		}
		if span+time.Duration(len(flights))*10*time.Microsecond > max(2*estimate.WindowRoundTrip, 4*sequence.deliveredBytesSampleIntervalAt(started)) {
			t.Fatal("consumer decision would outlive its evidence freshness")
		}
		pacer := &sequence.windowPacer
		pacer.rate, pacer.estimateRate = estimate.PacingByteRate, estimate.ServiceByteRate
		pacer.probeRate, pacer.probeLimit = estimate.PacingProbeByteRate, estimate.PacingProbeByteCount
		probes[pacer.service] = pacer.service.probeSent
		if pacer.service.probeSent < pacer.probeLimit {
			t.Fatal("consumer still has an unspent opening probe")
		}
		remaining[index] = 3 * flights[index]
		result.requested += remaining[index]
	}
	for result.released < result.requested && result.complete {
		for index, sequence := range self.sequences {
			if remaining[index] == 0 {
				continue
			}
			bytes := min(remaining[index], ByteCount(1024))
			service := sequence.windowPacer.service
			offered[service] += bytes
			if err := sequence.windowPacer.waitForService(ctx, int(bytes)); err != nil {
				if err != context.DeadlineExceeded {
					t.Fatal(err)
				}
				result.complete = false
				break
			}
			remaining[index] -= bytes
			result.released += bytes
		}
	}
	result.elapsed = time.Since(started)
	for service, published := range self.published {
		if service.sent != published+offered[service] || service.total != published || service.pacingReservations != 0 ||
			service.reservedByteCount != 0 || service.waiterHead != nil || service.waiterTail != nil {
			t.Fatal("consumer lost exact offered-byte ownership or retained a waiter")
		}
		if probe, used := probes[service]; used && service.probeSent != probe {
			t.Fatal("a sibling replenished the spent opening probe")
		}
	}
	for index, sequence := range self.sequences {
		if flights[index] > 0 && sequence.sendWindowSnapshot(time.Now()).DeliveryByteRate <= 0 {
			t.Fatal("consumer result depended on stale cumulative evidence")
		}
	}
	return result
}

// The quality boundary is a production transition that disables blind
// discovery without resetting physical permission or the spent opening probe.
func checkWindowSharedDeliveryAfterQuality(t *testing.T, flights []ByteCount, quality bool) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		fixture := newWindowSharedDeliveryFixture(t, make([]int, len(flights)))
		fixture.spendProbe(t)
		if quality {
			for _, sequence := range fixture.sequences {
				sequence.networkQualityChanged(time.Now())
			}
			time.Sleep(time.Millisecond)
		}
		fixture.deliver(t, flights)
		result := fixture.consume(t, flights)
		t.Logf("quality=%t flights=%v observed-round=%s release=%+v", quality, flights, fixture.period, result)
		if !result.complete {
			t.Errorf("fresh physical delivery underfilled the shared pacing clock: %+v", result)
		}
	})
}

// A single lane retains the ordinary cumulative fallback after remeasurement.
func TestWindowPacingSharedDeliverySingleLaneControl(t *testing.T) {
	checkWindowSharedDeliveryAfterQuality(t, []ByteCount{8 * 1024}, true)
}

// Two fresh lanes describe one shared delivery rate after a quality event.
func TestWindowPacingSharedDeliveryAfterQuality(t *testing.T) {
	checkWindowSharedDeliveryAfterQuality(t, []ByteCount{8 * 1024, 8 * 1024}, true)
}

// Four logical histories must not divide the common reservation clock by four.
func TestWindowPacingSharedDeliveryFourLanesAfterQuality(t *testing.T) {
	checkWindowSharedDeliveryAfterQuality(t, []ByteCount{8 * 1024, 8 * 1024, 8 * 1024, 8 * 1024}, true)
}

// Unequal delivery rules out multiplying one lane's rate by reference count.
func TestWindowPacingSharedDeliveryUnequalLanesAfterQuality(t *testing.T) {
	checkWindowSharedDeliveryAfterQuality(t, []ByteCount{8 * 1024, 24 * 1024}, true)
}

// Larger unequal groups retain the same finite physical and window bounds.
func TestWindowPacingSharedDeliveryFourUnequalLanesAfterQuality(t *testing.T) {
	checkWindowSharedDeliveryAfterQuality(t, []ByteCount{8 * 1024, 8 * 1024, 16 * 1024, 32 * 1024}, true)
}

// Before any queue or quality event, the admitted-window discovery floor
// already satisfies the archived cold-root case on current production code.
func TestWindowPacingSharedDeliveryDiscoveryControl(t *testing.T) {
	checkWindowSharedDeliveryAfterQuality(t, []ByteCount{8 * 1024, 8 * 1024, 16 * 1024, 32 * 1024}, false)
}

// Idle references do not multiply capacity, and a new lane with no local RTT
// consumes the same measured service without learning a sibling's window.
func TestWindowPacingSharedDeliveryEarlyReturnUsesCommonRate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newWindowSharedDeliveryFixture(t, []int{0, 0, 0})
		fixture.spendProbe(t)
		for _, sequence := range fixture.sequences {
			sequence.networkQualityChanged(time.Now())
		}
		time.Sleep(time.Millisecond)
		fixture.deliver(t, []ByteCount{8 * 1024, 24 * 1024, 0})
		at := time.Now()
		active := fixture.sequences[0].sendWindowEstimate(at)
		idle := fixture.sequences[2].sendWindowEstimate(at)
		if active.AggregateDeliveryByteRate == 0 || active.AggregateDeliveryByteRate != idle.AggregateDeliveryByteRate ||
			active.PacingByteRate != idle.PacingByteRate || idle.Sized || idle.ServiceSized || idle.Window != idle.Initial {
			t.Fatalf("new lane changed common pacing or borrowed learned bytes: active=%+v idle=%+v", active, idle)
		}
		fixture.sequences[0].deliveredBytesCount = 0
		missing := fixture.sequences[0].sendWindowSnapshot(at)
		if missing.DeliveryByteRate != 0 || missing.Sized || missing.Window != active.Window || missing.PacingByteRate != active.PacingByteRate {
			t.Fatalf("missing logical history erased independently measured common delivery: %+v", missing)
		}
	})
}

// Statistics reads, permission changes and current carrier selection do not
// mutate or grant access to a sibling's pacing evidence outside its scope.
func TestWindowPacingSharedDeliveryHonorsReadAndPolicyBoundaries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newWindowSharedDeliveryFixture(t, []int{0, 0})
		fixture.spendProbe(t)
		for _, sequence := range fixture.sequences {
			sequence.networkQualityChanged(time.Now())
		}
		time.Sleep(time.Millisecond)
		fixture.deliver(t, []ByteCount{8 * 1024, 24 * 1024})
		at := time.Now()
		sequence, sibling := fixture.sequences[0], fixture.sequences[1]
		service := sequence.windowPacer.service
		history, hold, total, sent, probe := service.aggregate, service.heldPacingRate, service.total, service.sent, service.probeSent
		learned, initialized := sequence.windowSize.window, sequence.windowSize.initialized
		before := sequence.sendWindowSnapshot(at)
		for range 4 {
			if got := sequence.sendWindowSnapshot(at); got.AggregateDeliveryByteRate == 0 || got != before {
				t.Fatalf("observational read changed its evidence: before=%+v after=%+v", before, got)
			}
		}
		if history != service.aggregate || hold != service.heldPacingRate || total != service.total || sent != service.sent || probe != service.probeSent ||
			learned != sequence.windowSize.window || initialized != sequence.windowSize.initialized || service.qualityServiceMeasured {
			t.Fatal("common-rate statistics changed service, learned memory or physical ownership")
		}
		if stale := sequence.sendWindowSnapshot(at.Add(time.Second)); stale.AggregateDeliveryByteRate != 0 {
			t.Fatalf("an idle service reused expired common delivery: %+v", stale)
		}
		sequence.observeReceiveWindowAdvertisement(receiveAckMessage{receiveWindowSet: true, receiveWindowByteCount: 16 * 1024 * 1024})
		sequence.receiveWindowSetAtNanos.Store(at.Add(time.Nanosecond).UnixNano())
		if changed := sequence.sendWindowSnapshot(at.Add(time.Nanosecond)); changed.AggregateDeliveryByteRate != 0 {
			t.Fatalf("new permission borrowed old offers: %+v", changed)
		}
		if got := sibling.sendWindowSnapshot(at); got.AggregateDeliveryByteRate != before.AggregateDeliveryByteRate {
			t.Fatalf("one lane's permission invalidated a sibling's history: %+v", got)
		}
		sibling.contractMultiRouteWriter = &windowPacingPolicyWriter{policy: transferFlightPolicySnapshot{}}
		if got := sibling.sendWindowSnapshot(at); got.AggregateDeliveryByteRate != 0 {
			t.Fatalf("a different carrier consumed prior H1 evidence: %+v", got)
		}
		sibling.sendBufferSettings.DeliverySizedWindowScale = 0
		if got := sibling.sendWindowSnapshot(at); got.PacingByteRate != 0 || got.AggregateDeliveryByteRate != 0 {
			t.Fatalf("disabled pacing consumed shared history: %+v", got)
		}
	})
}

// Faster contemporaneous credit at another destination cannot enter this
// service's history; active and idle references remain accounting identities.
func TestWindowPacingSharedDeliveryKeepsServicesIndependent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newWindowSharedDeliveryFixture(t, []int{0, 0, 1})
		fixture.spendProbe(t)
		for _, sequence := range fixture.sequences {
			sequence.networkQualityChanged(time.Now())
		}
		time.Sleep(time.Millisecond)
		fixture.deliver(t, []ByteCount{8 * 1024, 24 * 1024, 128 * 1024})
		own := fixture.sequences[0].sendWindowSnapshot(time.Now())
		sibling := fixture.sequences[1].sendWindowSnapshot(time.Now())
		foreign := fixture.sequences[2].sendWindowSnapshot(time.Now())
		if own.AggregateDeliveryByteRate == 0 || own.AggregateDeliveryByteRate != sibling.AggregateDeliveryByteRate ||
			foreign.AggregateDeliveryByteRate < 2*own.AggregateDeliveryByteRate || own.PacingByteRate == foreign.PacingByteRate {
			t.Fatalf("independent services shared a rate: own=%+v sibling=%+v foreign=%+v", own, sibling, foreign)
		}
	})
}
