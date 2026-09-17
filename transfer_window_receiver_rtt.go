// Receiver timing separates network RTT from the receiver's own residence.
// Samples remain paired, bounded by the existing RTT count and age settings.
package connect

import (
	"math"
	"time"
)

// A negative adjusted value is an ordinary legacy observation. It consumes
// the same sample lifetime without manufacturing receiver timing metadata.
type windowReceiverRoundTripSample struct {
	atNanos     int64
	raw         time.Duration
	adjusted    time.Duration
	compression time.Duration
}

// The service lock owns the ring. Storage is allocated only on the first
// receiver-timed ACK, and reads only inspect evidence without retiring it.
type windowReceiverRoundTrips struct {
	windowSize    int
	windowTimeout time.Duration
	samples       []windowReceiverRoundTripSample
	tail          int
	count         int
	lastAtNanos   int64
	observed      bool
	baseline      windowReceiverRoundTripSample
	baselineSet   bool
}

// Window residence is selected from complete tuples. The separate network
// minimum never combines one packet's RTT with another packet's receiver wait.
type windowReceiverRoundTripEstimate struct {
	minimum           time.Duration
	latest            time.Duration
	residence         time.Duration
	count             int
	receiverResidence time.Duration
	latestRaw         time.Duration
	feedbackPaired    bool
}

// Every sibling uses the same existing settings; no additional lifetime or
// sample-count knob is introduced for the logical service.
func newWindowPacingService(settings *SendBufferSettings) *windowPacingService {
	return &windowPacingService{drainMaximumTime: settings.AckTimeout, receiverRoundTrips: &windowReceiverRoundTrips{
		windowSize:    settings.RttWindowSize,
		windowTimeout: settings.RttWindowTimeout,
	}}
}

// Apply in arrival order. Later workers cannot put an old low sample into
// a newly filled ring or move the newest metadata clock backward.
func (self *windowReceiverRoundTrips) add(raw, adjusted, compression time.Duration, at time.Time) bool {
	atNanos := at.UnixNano()
	if raw < 0 || adjusted < -1 || raw < adjusted || self.observed && atNanos < self.lastAtNanos {
		return false
	}
	self.lastAtNanos, self.observed = atNanos, true
	if self.samples == nil {
		if adjusted < 0 {
			return true
		}
		if self.windowSize <= 0 {
			panic("invalid receiver RTT window size")
		}
		self.samples = make([]windowReceiverRoundTripSample, self.windowSize)
	}
	index := (self.tail + self.count) % len(self.samples)
	if self.count == len(self.samples) {
		index = self.tail
		self.tail = (self.tail + 1) % len(self.samples)
	} else {
		self.count++
	}
	self.samples[index] = windowReceiverRoundTripSample{
		atNanos: atNanos, raw: raw, adjusted: adjusted, compression: max(0, compression),
	}
	if adjusted >= 0 && (!self.baselineSet || adjusted < self.baseline.adjusted) {
		self.baseline, self.baselineSet = self.samples[index], true
	}
	return true
}

// Preserve measured receiver delay and add only the advertised portion not
// already present. This formula never subtracts unrelated historical minima.
func windowReceiverRoundTripResidence(raw, adjusted, compression time.Duration) time.Duration {
	delay := raw - adjusted
	if compression <= delay {
		return raw
	}
	if compression > time.Duration(math.MaxInt64)-adjusted {
		return time.Duration(math.MaxInt64)
	}
	return adjusted + compression
}

// One bounded scan covers both minima. Empty or expired metadata remains
// explicitly absent; looking at statistics cannot alter a later decision.
func (self *windowReceiverRoundTrips) estimate(at time.Time) windowReceiverRoundTripEstimate {
	estimate := windowReceiverRoundTripEstimate{}
	if self == nil {
		return estimate
	}
	cutoff, atNanos := at.Add(-self.windowTimeout).UnixNano(), at.UnixNano()
	for offset := 0; offset < self.count; offset++ {
		sample := self.samples[(self.tail+offset)%len(self.samples)]
		if sample.atNanos < cutoff || atNanos < sample.atNanos {
			continue
		}
		// Baseline metadata survives newer legacy replies, but a feedback
		// turn may use measured receiver wait only from its newest reply.
		estimate.feedbackPaired = sample.adjusted >= 0
		if sample.adjusted < 0 {
			continue
		}
		residence := windowReceiverRoundTripResidence(sample.raw, sample.adjusted, sample.compression)
		if estimate.count == 0 {
			estimate.minimum, estimate.residence = sample.adjusted, residence
		} else {
			estimate.minimum = min(estimate.minimum, sample.adjusted)
			estimate.residence = min(estimate.residence, residence)
		}
		receiverResidence := max(sample.raw-sample.adjusted, sample.compression)
		if estimate.count == 0 || receiverResidence < estimate.receiverResidence {
			estimate.receiverResidence = receiverResidence
		}
		estimate.latest, estimate.latestRaw = sample.adjusted, sample.raw
		estimate.count++
	}
	return estimate
}

// Ordinary samples remain raw for fallback. Receiver-capable ACKs publish
// one paired observation after the caller confirms its physical H1 write.
func (self *windowPacingService) observeReceiverRoundTrip(burst uint64, rawResidence, adjustedRoundTrip, compression time.Duration, at time.Time) {
	self.observeReceiverRoundTripForWrite(Id{}, Id{}, burst, rawResidence, adjustedRoundTrip, compression, at)
}

// Only the sender's exact first-write identity may attach receiver delay to
// an empty-flight probe. A later cumulative head cannot supply that delay.
func (self *windowPacingService) observeReceiverRoundTripForWrite(sequenceId, messageId Id, burst uint64, rawResidence, adjustedRoundTrip, compression time.Duration, at time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if rawResidence < 0 || adjustedRoundTrip < 0 || rawResidence < adjustedRoundTrip || at.Before(self.lastRoundTrip) {
		return
	}
	if !self.acceptQualityRoundTripWithLock(rawResidence, at) {
		return
	}
	if self.receiverRoundTrips == nil {
		settings := DefaultSendBufferSettings()
		self.receiverRoundTrips = &windowReceiverRoundTrips{windowSize: settings.RttWindowSize, windowTimeout: settings.RttWindowTimeout}
	}
	if !self.receiverRoundTrips.add(rawResidence, adjustedRoundTrip, compression, at) {
		return
	}
	if probe := &self.roundTripProbe; !probe.sentAt.IsZero() && probe.sequenceId == sequenceId && probe.messageId == messageId && messageId != (Id{}) {
		probe.receiverTiming = windowReceiverRoundTripSample{atNanos: at.UnixNano(), raw: rawResidence, adjusted: adjustedRoundTrip, compression: max(0, compression)}
		probe.receiverTimingSet = true
	}
	if self.roundTripStats.ring == nil {
		self.roundTripStats.ring = newWindowBucketStats(deliverySizedWindowSampleInterval, 4)
	}
	self.roundTripStats.add(burst, float64(rawResidence), at)
	self.observeRoundTripWithLock(rawResidence, compression, at, false)
}

// A metadata-capable shared service supersedes raw minima for window sizing.
// The caller uses the boolean to retain exact legacy behavior when absent.
func (self *windowPacingService) receiverWindowEstimate(at time.Time) (time.Duration, time.Duration, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	estimate := self.receiverRoundTrips.estimate(at)
	return estimate.minimum, estimate.residence, estimate.count > 0
}

// Internal queue and ring calculations consume the same recent evidence as
// window sizing. Legacy residence is kept separate and never raises a
// metadata-backed network minimum through max(raw, adjusted).
func (self *windowPacingService) roundTripEvidenceWithLock(at time.Time) windowReceiverRoundTripEstimate {
	estimate := self.receiverRoundTrips.estimate(at)
	if estimate.count == 0 {
		estimate.minimum, estimate.latest = self.minRoundTrip, self.latestRoundTrip
		estimate.residence = self.minRoundTrip + self.compression
	} else if self.receiverRoundTrips.baselineSet && self.receiverRoundTrips.baseline.atNanos <= at.UnixNano() {
		// Carrier queuing cannot raise the unloaded baseline or the burst's
		// sampling interval. Receiver residence comes from each complete
		// tuple's own raw-adjusted difference, never independent minima.
		estimate.minimum = self.receiverRoundTrips.baseline.adjusted
		estimate.residence = windowReceiverRoundTripResidence(estimate.minimum, estimate.minimum, estimate.receiverResidence)
	}
	return estimate
}

// Share one timestamped snapshot between sizing and its associated decisions.
func (self *windowPacingService) roundTripEvidence(at time.Time) windowReceiverRoundTripEstimate {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.roundTripEvidenceWithLock(at)
}

// A confirmed empty-flight tuple can raise the baseline. Older rows cannot
// dilute that proof, while later observations survive delayed confirmation.
func (self *windowReceiverRoundTrips) confirmBaseline(sample windowReceiverRoundTripSample) {
	if self == nil || sample.adjusted < 0 || sample.raw < sample.adjusted || self.baselineSet && sample.atNanos < self.baseline.atNanos {
		return
	}
	self.baseline, self.baselineSet = sample, true
	retained := 0
	oldCount := self.count
	for offset := 0; offset < oldCount; offset++ {
		index := (self.tail + offset) % len(self.samples)
		current := self.samples[index]
		if current.atNanos >= sample.atNanos && current.adjusted >= 0 && current.adjusted < self.baseline.adjusted {
			self.baseline = current
		}
		if current.atNanos < sample.atNanos {
			continue
		}
		self.samples[(self.tail+retained)%len(self.samples)] = current
		retained++
	}
	for offset := retained; offset < oldCount; offset++ {
		self.samples[(self.tail+offset)%len(self.samples)] = windowReceiverRoundTripSample{}
	}
	self.count = retained
}
