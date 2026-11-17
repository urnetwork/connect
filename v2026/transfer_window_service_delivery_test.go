// Common delivery retains original offer/arrival clocks. These direct ring
// controls isolate expiry, rebucketing and generation policy from scheduling.
package connect

import (
	"math"
	"testing"
	"time"
	"unsafe"
)

// Synthetic confirmed offers share the arrival clock's exact interval.
func windowServiceDeliveryTestSample(at time.Time, bytes ByteCount) windowServiceDeliverySample {
	return windowServiceDeliverySample{
		bytes: bytes, firstBytes: bytes,
		firstAtNanos: at.UnixNano(), lastAtNanos: at.UnixNano(),
		firstSentAtNanos: at.Add(-time.Millisecond).UnixNano(), eligible: true,
	}
}

// The common fallback is service scoped, so its fixed cost is multiplied by
// active destinations. Six entries keep the complete proof under 384 bytes on
// supported 64-bit hosts; growth belongs in cadence, not per-service memory.
func TestWindowServiceDeliveryRingMemoryBound(t *testing.T) {
	if len((windowServiceDeliveryRing{}).samples) != windowServiceDeliveryRingSize ||
		unsafe.Sizeof(windowServiceDeliveryRing{}) > 384 {
		t.Fatalf("common delivery history exceeded its fixed bound: entries=%d bytes=%d",
			len((windowServiceDeliveryRing{}).samples), unsafe.Sizeof(windowServiceDeliveryRing{}))
	}
}

// Reordered publication and tied sibling replies count every byte once while
// excluding all bytes that belong to the first arrival endpoint.
func TestWindowServiceDeliveryTiedReorderedArrivals(t *testing.T) {
	start := time.Unix(1700000000, 0)
	var ring windowServiceDeliveryRing
	for _, index := range []int{5, 2, 0, 4, 1, 3} {
		at := start.Add(time.Duration(index) * 10 * time.Millisecond)
		ring.insert(windowServiceDeliveryTestSample(at, 1000))
		ring.insert(windowServiceDeliveryTestSample(at, 3000))
	}
	estimate := ring.estimate(start.Add(50*time.Millisecond), 40*time.Millisecond, 0)
	if estimate.byteRate() != 400000 || estimate.bytes != 20000 || estimate.firstBytes != 4000 ||
		estimate.lastAtNanos-estimate.firstAtNanos != int64(40*time.Millisecond) {
		t.Fatalf("reordered/tied delivery changed endpoint ownership: %+v", estimate)
	}
}

// Cadence changes can merge buckets, but cannot relocate bytes onto the new
// bucket boundary or discard a still-required long-span endpoint.
func TestWindowServiceDeliveryRebucketKeepsRawEndpoints(t *testing.T) {
	start := time.Unix(1700000000, int64(2*time.Millisecond))
	var ring windowServiceDeliveryRing
	ring.resize(20 * time.Millisecond)
	for i := range 9 {
		ring.insert(windowServiceDeliveryTestSample(start.Add(time.Duration(i)*10*time.Millisecond), 1000))
	}
	want := ring.estimate(start.Add(80*time.Millisecond), 80*time.Millisecond, 0)
	if want.byteRate() != 100000 {
		t.Fatalf("invalid independent rate fixture: %+v", want)
	}
	for _, interval := range []time.Duration{20 * time.Millisecond, 10 * time.Millisecond, 15 * time.Millisecond, 10 * time.Millisecond} {
		ring.resize(interval)
		got := ring.estimate(start.Add(80*time.Millisecond), 4*interval, 0)
		if got.byteRate() != want.byteRate() || got.lastAtNanos != want.lastAtNanos ||
			(got.firstAtNanos-start.UnixNano())%int64(10*time.Millisecond) != 0 ||
			got.firstAtNanos < start.Add(80*time.Millisecond-time.Duration(len(ring.samples))*interval).UnixNano() {
			t.Fatalf("interval=%s retimed shared delivery: got=%+v want=%+v", interval, got, want)
		}
	}
}

// One late old publication cannot overwrite the newest modulo slot, including
// the exact ring-length boundary rather than just a much older timestamp.
func TestWindowServiceDeliveryRejectsRetiredModuloSlot(t *testing.T) {
	start := time.Unix(1700000000, 0)
	var ring windowServiceDeliveryRing
	ring.insert(windowServiceDeliveryTestSample(start, 1000))
	before := ring
	ring.insert(windowServiceDeliveryTestSample(start.Add(-time.Duration(len(ring.samples))*deliverySizedWindowSampleInterval), 900000))
	if ring != before {
		t.Fatal("expired delivery replaced current modulo ownership")
	}
}

// Endpoint freshness, future publication and first physical offer are separate
// qualifications; no rejected interval may fall back to a guessed rate.
func TestWindowServiceDeliveryRejectsStaleFutureAndOldOffers(t *testing.T) {
	start := time.Unix(1700000000, 0)
	var ring windowServiceDeliveryRing
	for i := range 5 {
		ring.insert(windowServiceDeliveryTestSample(start.Add(time.Duration(i)*10*time.Millisecond), 1000))
	}
	latest := start.Add(40 * time.Millisecond)
	if ring.estimate(latest.Add(40*time.Millisecond), 40*time.Millisecond, 0).byteRate() != 100000 {
		t.Fatal("exact freshness boundary rejected current evidence")
	}
	for _, at := range []time.Time{latest.Add(40*time.Millisecond + time.Nanosecond), latest.Add(-time.Nanosecond)} {
		if got := ring.estimate(at, 40*time.Millisecond, 0); got.byteRate() != 0 {
			t.Fatalf("stale/future endpoint repriced pacing: at=%s %+v", at.Sub(start), got)
		}
	}
	if got := ring.estimate(latest, 40*time.Millisecond, start.UnixNano()); got.byteRate() != 0 {
		t.Fatalf("a pre-permission offer survived its boundary: %+v", got)
	}
}

// Ambiguous or unknown offers poison their exact interval rather than
// becoming confirmed H1 evidence through another lane's valid timestamp.
func TestWindowServiceDeliveryRejectsIncompleteOfferProvenance(t *testing.T) {
	start := time.Unix(1700000000, 0)
	for _, invalid := range []string{"unknown", "unconfirmed", "after-arrival"} {
		var ring windowServiceDeliveryRing
		for i := range 5 {
			sample := windowServiceDeliveryTestSample(start.Add(time.Duration(i)*10*time.Millisecond), 1000)
			if i == 0 {
				switch invalid {
				case "unknown":
					sample.firstSentAtNanos = 0
				case "unconfirmed":
					sample.eligible = false
				case "after-arrival":
					sample.firstSentAtNanos = sample.firstAtNanos + 1
				}
			}
			ring.insert(sample)
		}
		if got := ring.estimate(start.Add(40*time.Millisecond), 40*time.Millisecond, 0); got.byteRate() != 0 {
			t.Errorf("%s offer invented pacing evidence: %+v", invalid, got)
		}
	}
}

// Extremely long residences saturate instead of wrapping a qualification
// horizon negative and making an isolated reply appear sufficient.
func TestWindowServiceDeliveryLongResidenceSaturates(t *testing.T) {
	for _, residence := range []time.Duration{time.Duration(math.MaxInt64/2 + 1), time.Duration(math.MaxInt64)} {
		if got := windowServiceDeliveryMinimumSpan(residence); got != time.Duration(math.MaxInt64) {
			t.Fatalf("residence=%s wrapped the common history: %s", residence, got)
		}
	}
}

// Common arrival progress is a cold fallback. An independently measured slow
// serializer still owns pacing, including its existing queue-drain margin.
func TestWindowServiceDeliveryCannotOverridePositiveSerialization(t *testing.T) {
	for _, queued := range []bool{false, true} {
		estimate := SendWindowEstimate{
			Initial: 512 * 1024, WindowRoundTrip: 100 * time.Millisecond,
			AggregateDeliveryByteRate: 10000000, ServiceByteRate: 100000,
			ServiceEstablished: true, ServiceBacklogged: queued,
		}
		want := ByteCount(110000)
		if queued {
			want = 95000
		}
		if got := windowPacingRate(estimate, 100000000); got != want {
			t.Errorf("queued=%t common delivery overrode serialization: got=%d want=%d", queued, got, want)
		}
	}
}

// A signal clears existing common history, and credit captured before it
// still repays ownership without seeding the replacement generation.
func TestWindowServiceDeliveryQualityResetRejectsDelayedCredit(t *testing.T) {
	start := time.Unix(1700000000, 0)
	fixture := newWindowReceiverCreditFixture(t, nil, start, 0)
	for i := range 5 {
		at := start.Add(time.Duration(i) * 10 * time.Millisecond)
		item := fixture.write(uint64(i), 1000, at)
		fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(item, at.Add(10*time.Millisecond), 0))
	}
	before := start.Add(50 * time.Millisecond)
	if got := fixture.service.aggregateDelivery(before, 10*time.Millisecond, 0).byteRate(); got != 100000 {
		t.Fatalf("fresh common fixture did not qualify: %d", got)
	}
	delayed := fixture.write(5, 1000, before.Add(time.Millisecond))
	credit := fixture.sequence.takePacingServiceCredit(delayed)
	change := before.Add(2 * time.Millisecond)
	fixture.sequence.networkQualityChanged(change)
	if fixture.service.aggregate.hasSamples {
		t.Fatal("quality reset retained old common history")
	}
	fixture.sequence.observePacingServiceCredit(credit, change.Add(time.Millisecond))
	if fixture.service.aggregate.hasSamples || fixture.service.total != 6000 {
		t.Fatal("delayed old credit seeded the new generation or failed repayment")
	}
	fresh := fixture.write(6, 1000, change.Add(2*time.Millisecond))
	fixture.sequence.coalesceReceivedAck(fixture.ackWindow, fixture.ack(fresh, change.Add(12*time.Millisecond), 0))
	if !fixture.service.aggregate.hasSamples || fixture.service.total != 7000 ||
		fixture.service.aggregateDelivery(change.Add(12*time.Millisecond), 10*time.Millisecond, 0).byteRate() != 0 {
		t.Fatal("one fresh reply borrowed the retired history or lost its own credit")
	}
}

// A coarse aggregate cannot carry its old first endpoint through a shorter
// retention horizon. Partial aggregates are rejected whole, never split.
func TestWindowServiceDeliveryShortCadenceRejectsOldFirstEndpoint(t *testing.T) {
	start := time.Unix(1700000000, 0)
	var ring windowServiceDeliveryRing
	ring.resize(time.Second)
	ring.insert(windowServiceDeliveryTestSample(start.Add(time.Millisecond), 1000))
	ring.insert(windowServiceDeliveryTestSample(start.Add(900*time.Millisecond), 90000))
	ring.resize(10 * time.Millisecond)
	for _, offset := range []time.Duration{910 * time.Millisecond, 920 * time.Millisecond} {
		ring.insert(windowServiceDeliveryTestSample(start.Add(offset), 1000))
	}
	if got := ring.estimate(start.Add(920*time.Millisecond), 40*time.Millisecond, 0); got.byteRate() != 0 {
		t.Fatalf("a fresh last endpoint revived retired coarse bytes: %+v rate=%d", got, got.byteRate())
	}
	for _, offset := range []time.Duration{930 * time.Millisecond, 940 * time.Millisecond, 950 * time.Millisecond} {
		ring.insert(windowServiceDeliveryTestSample(start.Add(offset), 1000))
	}
	if got := ring.estimate(start.Add(950*time.Millisecond), 40*time.Millisecond, 0); got.byteRate() != 100000 ||
		got.firstAtNanos != start.Add(910*time.Millisecond).UnixNano() {
		t.Fatalf("fresh short-cadence history did not replace the excluded aggregate: %+v", got)
	}
}

// Late endpoints in old buckets followed by an early endpoint in the newest
// bucket force the full two-slot quantization allowance at every residence.
func TestWindowServiceDeliveryRetainsWorstEndpointPhase(t *testing.T) {
	start := time.Unix(1700000000, 0)
	for _, residence := range []time.Duration{time.Millisecond, 10 * time.Millisecond, 25 * time.Millisecond, 100 * time.Millisecond, 400 * time.Millisecond, 1200 * time.Millisecond, 30 * time.Second} {
		service := newWindowPacingService(DefaultSendBufferSettings())
		service.observeRoundTrip(residence, 0, start)
		service.stateLock.Lock()
		service.observeAggregateDeliveryWithLock(windowServiceAckCredit{
			bytes: 1000, firstSentAtNanos: start.Add(-residence).UnixNano(), receiverTimingEligible: true,
		}, start)
		cadence := max(deliverySizedWindowSampleInterval, service.aggregate.interval)
		origin := time.Unix(0, start.UnixNano()/int64(cadence)*int64(cadence))
		at := origin
		for i := range len(service.aggregate.samples) + 3 {
			at = origin.Add(time.Duration(i+1) * cadence)
			if i < len(service.aggregate.samples)+2 {
				at = at.Add(-time.Nanosecond)
			} else {
				at = at.Add(-cadence)
			}
			service.observeAggregateDeliveryWithLock(windowServiceAckCredit{
				bytes: 1000, firstSentAtNanos: at.Add(-residence).UnixNano(), receiverTimingEligible: true,
			}, at)
		}
		service.stateLock.Unlock()
		estimate := service.aggregateDelivery(at, residence, 0)
		minimumSpan := max(2*residence, 4*cadence)
		if estimate.byteRate() <= 0 || estimate.lastAtNanos-estimate.firstAtNanos < int64(minimumSpan) ||
			estimate.firstAtNanos < at.Add(-time.Duration(len(service.aggregate.samples))*cadence).UnixNano() {
			t.Errorf("residence=%s cadence=%s lost the worst-phase interval: %+v", residence, cadence, estimate)
		}
		if got := service.aggregateDelivery(at.Add(minimumSpan+time.Nanosecond), residence, 0); got.byteRate() != 0 {
			t.Errorf("residence=%s revived a stale worst-phase interval: %+v", residence, got)
		}
	}
}

// The first endpoint expires immediately beyond the horizon; equality still
// retains a complete exact aggregate rather than dropping it prematurely.
func TestWindowServiceDeliveryFirstEndpointRetentionBoundary(t *testing.T) {
	at := time.Unix(1700000001, 0)
	for _, ageExtra := range []time.Duration{0, time.Nanosecond} {
		var ring windowServiceDeliveryRing
		first := at.Add(-time.Duration(len(ring.samples))*deliverySizedWindowSampleInterval - ageExtra)
		sample := windowServiceDeliveryTestSample(first, 1000)
		sample.add(windowServiceDeliveryTestSample(at.Add(-20*time.Millisecond), 8000))
		ring.insert(sample)
		ring.insert(windowServiceDeliveryTestSample(at, 1000))
		got := ring.estimate(at, 40*time.Millisecond, 0)
		if ageExtra == 0 && (got.byteRate() <= 0 || got.firstAtNanos != first.UnixNano()) ||
			ageExtra > 0 && got.byteRate() != 0 {
			t.Errorf("first endpoint age=%s violated retention: %+v", ageExtra, got)
		}
	}
}
