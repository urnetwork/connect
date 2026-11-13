// The delivery-sized window learns from a round trip that its own queue
// inflates. RttWindow keeps the minimum of its last RttWindowSize samples, and
// under a sustained transfer that many acknowledgements arrive in well under a
// second, every one of them waiting behind the standing queue. The minimum
// then measures the queue, the window sized from it grows, and the queue grows
// with it until the window reaches its ceiling. Measured on a provider serving
// eight parallel downloads to one client: the minimum read 2 to 3.5 s against
// an idle path of 83 ms, the window stood at its 46 MB ceiling, and every
// packet to that client, datagrams included, waited about seven seconds.
//
// WindowLatencyBudget, when set, breaks the loop with two bounds. The path
// round trip is the lowest sample over pathRoundTripMinimumHorizon, kept by
// time rather than by sample count and shared by every sequence to the
// destination, so a quiet sibling's acknowledgements anchor a busy one. And the
// admitted window is at most twice what the destination delivers within that
// round trip plus the budget, so a window grown on no evidence cannot stand as
// queue. On the same provider and load these held the window near 4 MB and the
// datagram round trip near 250 ms, with the same throughput.
//
// A bounded window still keeps its own small queue, so a path that stays busy
// would see its minimum rise to that queue once the horizon passes, and the
// bound would ratchet up with it. When no sample near the minimum has arrived
// for pathRoundTripProbeInterval, the window drops to its floor for a short
// drain so the next samples measure the path again, as BBR's ProbeRTT does.
// The same probe is what lets a path whose real delay grew adopt it within one
// horizon. That is slower than the unbounded window, which follows a longer
// path within a few round trips, and is the price of not letting the window
// read its own queue as path delay. The window path cells that characterize the
// unbounded learner set the budget to zero.
package connect

import (
	"sync"
	"time"
)

// How long one low round trip anchors its path. The probe refreshes it well
// within this, so a path whose delay grew is adopted within one horizon.
const pathRoundTripMinimumHorizon = 12 * time.Second

// How long a busy path may go without a sample near its minimum before the
// window drains to measure it again.
const pathRoundTripProbeInterval = 10 * time.Second

// A sample within this factor of the minimum shows the queue was drained.
const pathRoundTripNearMinimum = 1.25

// The drain lasts at least this long, and at least two path round trips.
const pathRoundTripProbeDuration = 200 * time.Millisecond

// How long a delivery rate bounds windows whose own estimate has none, for the
// stretches where a saturated sequence takes no fresh delivery sample.
const pathDeliveryRateLifetime = 10 * time.Second

const pathRoundTripMinimumBucketCount = int(pathRoundTripMinimumHorizon / time.Second)

// One-second buckets over the horizon. Constant memory and constant work per
// observation, whatever the acknowledgement rate.
type pathRoundTripMinimum struct {
	buckets [pathRoundTripMinimumBucketCount]time.Duration
	seconds [pathRoundTripMinimumBucketCount]int64
}

// Records a sample, when there is one, and returns the lowest sample within
// the horizon, or zero when there is none.
func (self *pathRoundTripMinimum) observe(sample time.Duration, now time.Time) time.Duration {
	second := now.Unix()
	if 0 < sample {
		index := int(second % int64(pathRoundTripMinimumBucketCount))
		if index < 0 {
			index += pathRoundTripMinimumBucketCount
		}
		if self.seconds[index] != second || self.buckets[index] <= 0 || sample < self.buckets[index] {
			self.buckets[index] = sample
			self.seconds[index] = second
		}
	}
	minimum := time.Duration(0)
	for index, bucket := range self.buckets {
		if bucket <= 0 || int64(pathRoundTripMinimumBucketCount) <= second-self.seconds[index] {
			continue
		}
		if minimum == 0 || bucket < minimum {
			minimum = bucket
		}
	}
	return minimum
}

// What every sequence to one destination shares about the path.
type pathLatencyState struct {
	stateLock          sync.Mutex
	roundTrip          pathRoundTripMinimum
	nearMinimumAt      time.Time
	probeUntil         time.Time
	deliveryByteRate   ByteCount
	deliveryObservedAt time.Time
}

func (self *Client) pathLatency(destination Id) *pathLatencyState {
	return self.pathLatencyStateFor(destination, true)
}

func (self *Client) pathLatencyStateFor(destination Id, create bool) *pathLatencyState {
	if self == nil {
		return nil
	}
	self.pathLatencyLock.Lock()
	defer self.pathLatencyLock.Unlock()
	if self.pathLatencies == nil && create {
		self.pathLatencies = map[Id]*pathLatencyState{}
	}
	state, ok := self.pathLatencies[destination]
	if !ok && create {
		state = &pathLatencyState{}
		self.pathLatencies[destination] = state
	}
	return state
}

func (self *SendSequence) windowLatencyBudget() time.Duration {
	if self.sendBufferSettings == nil {
		return 0
	}
	return self.sendBufferSettings.WindowLatencyBudget
}

// Records this sequence's current minimum and returns the path's, or zero
// when the bound is off or there is no sample yet.
func (self *SendSequence) observePathRoundTrip(sample time.Duration, now time.Time) time.Duration {
	return self.observePathRoundTripAt(sample, now, now, true)
}

// Polling an estimator must not relabel an old minimum as a fresh ACK.
// Statistics read existing evidence without admitting a sample at all.
func (self *SendSequence) observePathRoundTripAt(sample time.Duration, sampledAt, now time.Time, observe bool) time.Duration {
	if self.windowLatencyBudget() <= 0 {
		return 0
	}
	state := self.client.pathLatencyStateFor(self.destination, observe)
	if state == nil {
		return 0
	}
	state.stateLock.Lock()
	defer state.stateLock.Unlock()
	fresh := observe && sample > 0 && !sampledAt.IsZero() && !sampledAt.After(now) &&
		now.Unix()-sampledAt.Unix() < int64(pathRoundTripMinimumBucketCount)
	if fresh {
		state.roundTrip.observe(sample, sampledAt)
	}
	minimum := state.roundTrip.observe(0, now)
	if fresh && float64(sample) <= float64(minimum)*pathRoundTripNearMinimum && state.nearMinimumAt.Before(sampledAt) {
		state.nearMinimumAt = sampledAt
	}
	return minimum
}

// Bounds the admitted window to twice what the destination delivers within
// its path round trip plus the latency budget, never below the floor, and
// drains it to the floor while the path is being measured again.
func (self *SendSequence) boundWindowByLatency(estimate SendWindowEstimate, now time.Time) SendWindowEstimate {
	return self.boundWindowByLatencyWithObservation(estimate, now, true)
}

func (self *SendSequence) boundWindowByLatencyWithObservation(estimate SendWindowEstimate, now time.Time, observe bool) SendWindowEstimate {
	budget := self.windowLatencyBudget()
	if budget <= 0 {
		return estimate
	}
	state := self.client.pathLatencyStateFor(self.destination, observe)
	if state == nil {
		return estimate
	}
	state.stateLock.Lock()
	deliveryByteRate := state.deliveryByteRate
	deliveryFresh := now.Sub(state.deliveryObservedAt) <= pathDeliveryRateLifetime
	if 0 < estimate.DeliveryByteRate {
		deliveryByteRate, deliveryFresh = estimate.DeliveryByteRate, true
		if observe {
			state.deliveryByteRate = estimate.DeliveryByteRate
			state.deliveryObservedAt = now
		}
	}
	roundTrip := state.roundTrip.observe(0, now)
	probing := false
	if 0 < roundTrip && deliveryFresh && 0 < deliveryByteRate {
		nearMinimumAt := state.nearMinimumAt
		if nearMinimumAt.IsZero() {
			nearMinimumAt = now
			if observe {
				state.nearMinimumAt = now
			}
		}
		if now.Before(state.probeUntil) {
			probing = true
		} else if pathRoundTripProbeInterval <= now.Sub(nearMinimumAt) {
			if observe {
				state.probeUntil = now.Add(max(pathRoundTripProbeDuration, 2*roundTrip))
				// the drain is the attempt; another waits a full interval
				state.nearMinimumAt = now
			}
			probing = true
		}
	}
	state.stateLock.Unlock()
	if deliveryByteRate <= 0 || !deliveryFresh || roundTrip <= 0 {
		return estimate
	}
	if probing {
		if estimate.Floor < estimate.Window {
			estimate.Window = estimate.Floor
			estimate.Reason = "latency probe"
		}
		return estimate
	}
	bound := float64(deliveryByteRate) * (roundTrip + budget).Seconds() * 2
	limit := estimate.Floor
	if float64(limit) < bound {
		limit = ByteCount(min(bound, float64(estimate.Window)))
	}
	if limit < estimate.Window {
		estimate.Window = limit
		estimate.Reason = "latency bound"
	}
	return estimate
}
