// A bounded common history counts confirmed H1 delivery once, across cold
// serialization epochs. It preserves raw clocks and excludes old offers.
package connect

import (
	"math"
	"time"
)

// Four measurement buckets plus two endpoint-phase slots are sufficient for
// max(2*residence, 4*cadence). With fewer slots a late first endpoint followed
// by an early newest endpoint cannot span a qualified interval before eviction.
const windowServiceDeliveryRingSize = 6

// The first tied arrival group is excluded from the rate numerator. The
// earliest physical offer pins permission and path-generation boundaries.
type windowServiceDeliverySample struct {
	bucket           int64
	bytes            ByteCount
	firstBytes       ByteCount
	firstAtNanos     int64
	lastAtNanos      int64
	firstSentAtNanos int64
	eligible         bool
}

// Preserve exact endpoints when ACK publication is reordered or rebinned.
func (self *windowServiceDeliverySample) add(other windowServiceDeliverySample) {
	if other.bytes <= 0 {
		return
	}
	if self.bytes == 0 {
		bucket := self.bucket
		*self = other
		self.bucket = bucket
		return
	}
	if other.firstAtNanos < self.firstAtNanos {
		self.firstAtNanos, self.firstBytes = other.firstAtNanos, other.firstBytes
	} else if other.firstAtNanos == self.firstAtNanos {
		self.firstBytes += other.firstBytes
	}
	self.lastAtNanos = max(self.lastAtNanos, other.lastAtNanos)
	self.firstSentAtNanos = min(self.firstSentAtNanos, other.firstSentAtNanos)
	self.bytes += other.bytes
	self.eligible = self.eligible && other.eligible
}

// Every selected byte must carry a known initial offer. Rounding occurs only
// at the final rate, without overflowing a valid high-throughput observation.
func (self windowServiceDeliverySample) byteRate() ByteCount {
	span := self.lastAtNanos - self.firstAtNanos
	if !self.eligible || span <= 0 || self.bytes <= self.firstBytes {
		return 0
	}
	rate := float64(self.bytes-self.firstBytes) * float64(time.Second) / float64(span)
	if rate >= float64(math.MaxInt64) {
		return ByteCount(math.MaxInt64)
	}
	return ByteCount(rate)
}

// One fixed ring covers at least two current residences. The service lock
// protects insertion, cadence changes and observational reads.
type windowServiceDeliveryRing struct {
	samples      [windowServiceDeliveryRingSize]windowServiceDeliverySample
	interval     time.Duration
	newestBucket int64
	hasSamples   bool
}

// Expired modulo slots cannot replace newer credit, even at the exact edge.
func (self *windowServiceDeliveryRing) insert(sample windowServiceDeliverySample) {
	interval := max(deliverySizedWindowSampleInterval, self.interval)
	bucket := sample.lastAtNanos / int64(interval)
	if self.hasSamples && bucket <= self.newestBucket-int64(len(self.samples)) {
		return
	}
	if !self.hasSamples || self.newestBucket < bucket {
		self.newestBucket, self.hasSamples = bucket, true
	}
	index := (bucket%int64(len(self.samples)) + int64(len(self.samples))) % int64(len(self.samples))
	destination := &self.samples[index]
	if destination.bytes == 0 || destination.bucket != bucket {
		*destination = windowServiceDeliverySample{bucket: bucket}
	}
	destination.add(sample)
}

// RTT changes retention without moving bytes to a different arrival time.
func (self *windowServiceDeliveryRing) resize(interval time.Duration) {
	interval = max(deliverySizedWindowSampleInterval, interval)
	if interval == max(deliverySizedWindowSampleInterval, self.interval) {
		return
	}
	previous, newest := self.samples, self.newestBucket
	clear(self.samples[:])
	self.newestBucket, self.hasSamples, self.interval = 0, false, interval
	for offset := int64(0); offset < int64(len(previous)); offset++ {
		bucket := newest - offset
		index := (bucket%int64(len(previous)) + int64(len(previous))) % int64(len(previous))
		if sample := previous[index]; sample.bytes > 0 && sample.bucket == bucket {
			self.insert(sample)
		}
	}
}

// Saturating doubling cannot turn a long path into a short pacing history.
func windowServiceDeliveryMinimumSpan(residence time.Duration) time.Duration {
	if residence > time.Duration(math.MaxInt64/2) {
		return time.Duration(math.MaxInt64)
	}
	return 2 * max(0, residence)
}

// Complete raw intervals may cross serialization drains, but may not borrow
// stale endpoints, future ACKs, unknown offers or pre-permission credit.
func (self *windowServiceDeliveryRing) estimate(at time.Time, minimumSpan time.Duration, afterNanos int64) windowServiceDeliverySample {
	interval := max(deliverySizedWindowSampleInterval, self.interval)
	minimumSpan = max(minimumSpan, windowServiceDeliveryMinimumSpan(windowServiceDeliveryMinimumSpan(interval)))
	horizon := time.Duration(math.MaxInt64)
	if interval <= time.Duration(math.MaxInt64/int64(len(self.samples))) {
		horizon = interval * time.Duration(len(self.samples))
	}
	// Retention is an evidence span. Idle expiry is checked separately below;
	// anchoring both to the read time rejects a complete interval at its exact
	// freshness boundary before that boundary has actually expired.
	newestAtNanos := int64(math.MinInt64)
	for offset := int64(0); offset < int64(len(self.samples)); offset++ {
		bucket := self.newestBucket - offset
		index := (bucket%int64(len(self.samples)) + int64(len(self.samples))) % int64(len(self.samples))
		sample := self.samples[index]
		if sample.bytes > 0 && sample.bucket == bucket && sample.lastAtNanos <= at.UnixNano() {
			newestAtNanos = max(newestAtNanos, sample.lastAtNanos)
		}
	}
	if newestAtNanos == int64(math.MinInt64) {
		return windowServiceDeliverySample{}
	}
	cutoffNanos := int64(math.MinInt64)
	if newestAtNanos >= int64(math.MinInt64)+int64(horizon) {
		cutoffNanos = newestAtNanos - int64(horizon)
	}
	selected := windowServiceDeliverySample{}
	for offset := int64(0); offset < int64(len(self.samples)); offset++ {
		bucket := self.newestBucket - offset
		index := (bucket%int64(len(self.samples)) + int64(len(self.samples))) % int64(len(self.samples))
		sample := self.samples[index]
		if sample.bytes <= 0 || sample.bucket != bucket || sample.lastAtNanos > at.UnixNano() ||
			sample.firstAtNanos < max(afterNanos, cutoffNanos) {
			continue
		}
		selected.add(sample)
		if selected.lastAtNanos-selected.firstAtNanos >= int64(minimumSpan) {
			break
		}
	}
	if !selected.eligible || selected.bytes <= selected.firstBytes || selected.firstSentAtNanos <= 0 ||
		selected.firstSentAtNanos < afterNanos || selected.firstSentAtNanos > selected.firstAtNanos ||
		selected.lastAtNanos-selected.firstAtNanos < int64(minimumSpan) ||
		at.UnixNano()-selected.lastAtNanos > int64(minimumSpan) {
		return windowServiceDeliverySample{}
	}
	return selected
}

// Credit arrives after the existing once-only owner and quality-generation
// check. Keep raw evidence before a cold serialization epoch can discard it.
func (self *windowPacingService) observeAggregateDeliveryWithLock(credit windowServiceAckCredit, at time.Time) {
	timingAt := at
	if self.lastRoundTrip.After(timingAt) {
		timingAt = self.lastRoundTrip
	}
	span := windowServiceDeliveryMinimumSpan(self.roundTripEvidenceWithLock(timingAt).residence)
	slots := time.Duration(len(self.aggregate.samples) - 2)
	interval := span / slots
	if span%slots != 0 {
		interval++
	}
	self.aggregate.resize(interval)
	self.aggregate.insert(windowServiceDeliverySample{
		bytes: credit.bytes, firstBytes: credit.bytes,
		firstAtNanos: at.UnixNano(), lastAtNanos: at.UnixNano(), firstSentAtNanos: credit.firstSentAtNanos,
		eligible: credit.receiverTimingEligible && credit.firstSentAtNanos > 0 && credit.firstSentAtNanos <= at.UnixNano(),
	})
}

// Fresh common evidence uses this caller's permission and the service's path
// boundary. Statistics and admission cannot alter the history by reading it.
func (self *windowPacingService) aggregateDelivery(at time.Time, residence time.Duration, afterNanos int64) windowServiceDeliverySample {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	residence = max(residence, self.roundTripEvidenceWithLock(at).residence)
	return self.aggregate.estimate(at, windowServiceDeliveryMinimumSpan(residence), max(afterNanos, self.windowDeliveryAfterNanos))
}
