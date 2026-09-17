// Quality notifications retire measurement evidence, never message ownership,
// delivery lifetimes, peer permissions, or the bytes already spent pacing.
package connect

import "time"

// Several long feedback turns fit while noisy listener updates cannot keep
// remeasurement open forever. Another generation needs a quiet interval first.
const windowQualityRemeasureInterval = 5 * time.Second

// An exact peer affects its logical services only; a local network event
// affects every live service. Snapshot under the map lock, reset outside it.
func (self *SendBuffer) networkQualityChanged(peerId Id, at time.Time) {
	if self == nil {
		return
	}
	self.mutex.Lock()
	sequences := make([]*SendSequence, 0, len(self.sendSequences))
	if !self.closed {
		for id, sequence := range self.sendSequences {
			if peerId == (Id{}) || id.Destination == peerId {
				sequences = append(sequences, sequence)
			}
		}
	}
	self.mutex.Unlock()
	for _, sequence := range sequences {
		sequence.networkQualityChanged(at)
	}
}

// One per-sequence lock prevents an estimate computed before reset from
// committing afterwards. Sampling has independent generation checks below.
func (self *SendSequence) networkQualityChanged(at time.Time) {
	at = time.Unix(0, self.client.feedbackArrivalNanos(at))
	self.windowQualityLock.Lock()
	defer self.windowQualityLock.Unlock()
	if service := self.windowPacer.service; service != nil {
		// The shared service owns coalescing. A sibling born mid-generation
		// inherits its original boundary, never a renewed shrink interval.
		generationAt := service.networkQualityChanged(at)
		self.windowQualityLastNotification = at
		if generationAt.UnixNano() <= self.windowQualityAfterNanos {
			return
		}
		at = generationAt
	} else if !self.windowQualityLastNotification.IsZero() &&
		at.Sub(self.windowQualityLastNotification) <= windowQualityRemeasureInterval {
		if at.After(self.windowQualityLastNotification) {
			self.windowQualityLastNotification = at
		}
		return
	}
	self.windowQualityLastNotification = at
	if self.rttWindow != nil {
		self.rttWindow.networkQualityChanged(at)
	}
	self.deliveredBytesMutex.Lock()
	self.windowQualityAfterNanos = at.UnixNano()
	self.deliveredBytesHead, self.deliveredBytesCount = 0, 0
	self.deliveredByteTotal, self.deliveredServiceByteTotal = 0, 0
	clear(self.deliveredBytes)
	self.deliveredBytesMutex.Unlock()
	self.windowSize.remeasure(at)
}

// Quality reset keeps the last valid rate as a provisional zero hold. Its
// replacement requires an independently qualified pair of new-path ACKs.
func (self *windowPacingService) networkQualityChanged(at time.Time) time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !self.qualityLastNotification.IsZero() && at.Sub(self.qualityLastNotification) <= windowQualityRemeasureInterval {
		if at.After(self.qualityLastNotification) {
			self.qualityLastNotification = at
		}
		return self.qualityChangedAt
	}
	self.qualityLastNotification = at
	rate, _, latest := self.measureWithLock(time.Second, at, false)
	self.serviceHoldRate = max(rate, latest, self.serviceHoldRate)
	self.qualityChangedAt = at
	self.qualityRoundTripPending = true
	self.qualityServiceMeasured = false
	self.serviceEpochAt = at
	self.windowDeliveryAfterNanos = at.UnixNano()
	clear(self.samples[:])
	self.aggregate = windowServiceDeliveryRing{}
	self.hasSamples, self.newestBucket = false, 0
	self.feedbackAt, self.feedbackCycleBefore = time.Time{}, time.Time{}
	self.feedbackCycle, self.feedbackComplete, self.feedbackFresh = windowServiceSample{}, windowServiceSample{}, windowServiceSample{}
	self.feedbackPending, self.feedbackLimited, self.feedbackInterval = false, false, 0
	self.feedbackDrainAt, self.feedbackDrainPending = time.Time{}, 0
	self.roundTripProbe = windowPacingRoundTripProbe{}
	// A signal does not prove an empty physical flight or replenish probe,
	// FIFO, burst or meter credit. Existing writes still own their ACK bytes.
	self.queueObservedAt, self.heldPacingRate = at, 0
	self.drainServiceEpoch = false
	self.drainObservedAt, self.drainProgressUntil, self.drainObservedFlight = time.Time{}, time.Time{}, 0
	return at
}

// Exact first-send time is immutable across delayed ACK processing and
// retransmission. Equality is conservatively old at a same-clock boundary.
func (self *windowPacingService) acceptQualityRoundTripWithLock(raw time.Duration, at time.Time) bool {
	if self.qualityChangedAt.IsZero() {
		return true
	}
	if raw <= 0 {
		return false
	}
	if !at.Add(-raw).After(self.qualityChangedAt) {
		return false
	}
	if self.qualityRoundTripPending {
		self.minRoundTrip, self.latestRoundTrip, self.compression = 0, 0, 0
		self.lastRoundTrip = time.Time{}
		self.roundTripStats = windowBurstStats{}
		if timing := self.receiverRoundTrips; timing != nil {
			clear(timing.samples)
			timing.tail, timing.count, timing.lastAtNanos = 0, 0, 0
			timing.observed, timing.baselineSet = false, false
			timing.baseline = windowReceiverRoundTripSample{}
		}
		self.qualityRoundTripPending = false
	}
	return true
}

// Shared timing may qualify a new sibling before its first local RTT arrives.
func (self *windowPacingService) freshQualityEstimate() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return !self.qualityRoundTripPending && self.qualityServiceMeasured
}

// A sibling may reset shared evidence while this lane computes its candidate.
// Neither growth nor shrinking may commit that old-generation calculation.
func (self *windowPacingService) qualityEstimateGeneration(generation time.Time) (bool, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	current := self.qualityChangedAt == generation
	return current, current && !self.qualityRoundTripPending && self.qualityServiceMeasured
}

// Reads pin the generation for their later pacing-hold publication.
func (self *windowPacingService) qualityGeneration() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.qualityChangedAt
}

// A sibling's pre-event estimate cannot reinstate the old held pace.
func (self *windowPacingService) holdPacingForGeneration(rate ByteCount, generation time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.qualityChangedAt == generation &&
		(self.qualityChangedAt.IsZero() || !self.qualityRoundTripPending && self.qualityServiceMeasured) {
		self.heldPacingRate = rate
	}
}

// A bounded first-copy timeout cannot invalidate the very first fresh path
// sample. Recovery still fires at its original physical time plus this floor.
func (self *windowPacingService) qualityRecoveryInterval(firstSentAtNanos int64, cold, maximum time.Duration) time.Duration {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.qualityChangedAt.IsZero() || !self.qualityRoundTripPending || firstSentAtNanos <= self.qualityChangedAt.UnixNano() {
		return 0
	}
	return max(0, min(cold, maximum))
}
