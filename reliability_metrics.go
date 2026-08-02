package connect

import (
	"sync"
	"sync/atomic"
	"time"
)

// Reliability work on this client has repeatedly shipped fixes that were
// correct in isolation and changed nothing on a real device, because the only
// available measurement was how long a freeze felt. These counters exist to
// replace that with a number.
//
// The headline number is blast radius: how many live flows one provider
// failure destroys. Providers are split-tcp, so an exit *is* the tcp endpoint
// for every flow pinned to it -- when it dies those connections cannot be
// migrated, on any platform. The client cannot make that cost zero, so the
// goal is to make it small (spread flows over more exits) and to make the
// recovery immediate (tell the peer at once rather than letting it time out).
// Both are measured here, so a candidate fix can be judged instead of assumed.

const (
	// a destination is only a recovery candidate for as long as a user would
	// plausibly still be waiting on it. beyond this the reconnect is a new
	// request, not a recovery, and counting it would flatter the numbers.
	recoveryTrackerMaxAge = 60 * time.Second
	// bounds memory when many flows die at once and nothing comes back --
	// entries are dropped oldest-first rather than allowed to accumulate.
	recoveryTrackerMaxEntries = 4096
)

// blackholeAgeString renders how long a stat clock has been running, for the
// blackhole verdict line. A zero time means the clock never started, which is
// materially different from "started just now" -- it is the difference between
// a provider that has unacked sends outstanding and one that has none.
func blackholeAgeString(t time.Time) string {
	if t.IsZero() {
		return "none"
	}
	return time.Since(t).Round(time.Millisecond).String()
}

// recoveryKey identifies a remote endpoint across the death and rebirth of the
// flows that talk to it. Port is included because a browser reconnecting to
// the same host on a new source port is the recovery being measured, but a
// different service on that host is not.
type recoveryKey struct {
	// net.IP is a byte slice and not comparable; the string conversion is the
	// standard way to key on one.
	ip   string
	port int
}

func newRecoveryKey(ip []byte, port int) recoveryKey {
	return recoveryKey{ip: string(ip), port: port}
}

// reliabilityMetrics is written from the packet hot path, so every counter is
// an atomic and the only lock guards the recovery tracker -- which is touched
// once per exit loss and once per first-packet-from-a-pending-destination,
// never per packet.
type reliabilityMetrics struct {
	flowsOpened atomic.Uint64

	// exitLossEvents counts provider failures; flowsLostToExit counts the
	// connections they cost. The ratio is the blast radius.
	exitLossEvents         atomic.Uint64
	flowsLostToExit        atomic.Uint64
	maxFlowsLostInOneEvent atomic.Uint64

	// recovery spans from the moment a flow's exit died to the first packet
	// back from that same destination over a replacement exit. This is the
	// interval the user experiences as the freeze.
	recoveryCount    atomic.Uint64
	recoveryNanos    atomic.Uint64
	recoveryMaxNanos atomic.Uint64
	// recoveryMissed counts destinations that died and never came back inside
	// recoveryTrackerMaxAge. Without it a fix that abandons flows entirely
	// would look better than one that recovers them slowly.
	recoveryMissed atomic.Uint64

	// dialFailures counts provider dial-failure signals intercepted at the
	// channel; flowsReraced counts the flows silently unbound so the
	// application's own retransmit races them onto another exit. The gap
	// between them is failures that matched no live flow (late or stale).
	dialFailures atomic.Uint64
	flowsReraced atomic.Uint64

	// a blackhole verdict is only as good as the evidence it is built on, and
	// two conditions make the evidence inadmissible: the local uplink went
	// stale (nothing was deliverable, so silence convicts the network, not
	// the provider), and the transport under the channel is known down.
	// verdictsHeldUplinkStale and verdictsHeldTransportDown count verdicts
	// suppressed for each reason; removalsDeferred counts provider removals
	// postponed while a hold was in effect. This package only defines them --
	// the verdict-gating work that increments them lands separately, and
	// defining the counters first means that work ships already measured.
	verdictsHeldUplinkStale   atomic.Uint64
	verdictsHeldTransportDown atomic.Uint64
	removalsDeferred          atomic.Uint64

	pendingLock sync.Mutex
	pending     map[recoveryKey]time.Time
}

func newReliabilityMetrics() *reliabilityMetrics {
	return &reliabilityMetrics{
		pending: map[recoveryKey]time.Time{},
	}
}

// Every method below tolerates a nil receiver. Measurement sits directly on
// the packet path, and a client assembled without one -- tests build the
// struct literally -- must lose its counters, not its traffic.

func (self *reliabilityMetrics) flowOpened() {
	if self == nil {
		return
	}
	self.flowsOpened.Add(1)
}

func (self *reliabilityMetrics) dialFailureIntercepted() {
	if self == nil {
		return
	}
	self.dialFailures.Add(1)
}

func (self *reliabilityMetrics) flowReraced() {
	if self == nil {
		return
	}
	self.flowsReraced.Add(1)
}

func (self *reliabilityMetrics) verdictHeldUplinkStale() {
	if self == nil {
		return
	}
	self.verdictsHeldUplinkStale.Add(1)
}

func (self *reliabilityMetrics) verdictHeldTransportDown() {
	if self == nil {
		return
	}
	self.verdictsHeldTransportDown.Add(1)
}

func (self *reliabilityMetrics) removalDeferred() {
	if self == nil {
		return
	}
	self.removalsDeferred.Add(1)
}

// exitLost records one provider failure and the flows it destroyed, and arms
// each of their destinations for a recovery measurement.
func (self *reliabilityMetrics) exitLost(destinations []recoveryKey) {
	if self == nil {
		return
	}
	self.exitLossEvents.Add(1)
	self.flowsLostToExit.Add(uint64(len(destinations)))

	n := uint64(len(destinations))
	for {
		observed := self.maxFlowsLostInOneEvent.Load()
		if n <= observed || self.maxFlowsLostInOneEvent.CompareAndSwap(observed, n) {
			break
		}
	}

	if len(destinations) == 0 {
		return
	}

	now := time.Now()

	self.pendingLock.Lock()
	defer self.pendingLock.Unlock()

	for _, key := range destinations {
		// keep the earliest death for a destination. several flows to one host
		// die together, and the user is waiting from the first of them.
		if _, exists := self.pending[key]; !exists {
			self.pending[key] = now
		}
	}
	// evict after inserting, not before: a single exit can be carrying more
	// flows than the tracker holds, and evicting first would leave the whole
	// batch resident.
	self.evictPendingWithLock(now)
}

// destinationReachable reports the first packet back from a remote endpoint. It
// closes out a pending recovery if one is armed, and is a no-op otherwise --
// which is the overwhelmingly common case, so it takes the lock only after a
// racy empty check.
func (self *reliabilityMetrics) destinationReachable(ip []byte, port int) {
	if self == nil || len(ip) == 0 {
		return
	}

	self.pendingLock.Lock()
	defer self.pendingLock.Unlock()

	if len(self.pending) == 0 {
		return
	}

	key := newRecoveryKey(ip, port)
	lostTime, ok := self.pending[key]
	if !ok {
		return
	}
	delete(self.pending, key)

	// Past the window this is not a recovery, it is the user happening to
	// visit the site again. Eviction is lazy -- it only runs when another exit
	// dies -- so a stale entry can survive well beyond the age bound and, left
	// unchecked here, is credited as an enormously slow recovery. On device
	// that turned a 14s average into "avg 2m, worst 9m" and made the number
	// measure browsing habits rather than the tunnel.
	if recoveryTrackerMaxAge <= time.Since(lostTime) {
		self.recoveryMissed.Add(1)
		return
	}

	nanos := time.Since(lostTime).Nanoseconds()
	if nanos < 0 {
		nanos = 0
	}
	self.recoveryCount.Add(1)
	self.recoveryNanos.Add(uint64(nanos))
	for {
		observed := self.recoveryMaxNanos.Load()
		if uint64(nanos) <= observed || self.recoveryMaxNanos.CompareAndSwap(observed, uint64(nanos)) {
			break
		}
	}
}

// evictPendingWithLock retires destinations that never came back. Callers hold
// pendingLock.
func (self *reliabilityMetrics) evictPendingWithLock(now time.Time) {
	for key, lostTime := range self.pending {
		if recoveryTrackerMaxAge <= now.Sub(lostTime) {
			delete(self.pending, key)
			self.recoveryMissed.Add(1)
		}
	}

	// age alone does not bound the map when losses arrive faster than they
	// expire, so drop the oldest until it fits.
	for recoveryTrackerMaxEntries < len(self.pending) {
		var oldestKey recoveryKey
		var oldestTime time.Time
		first := true
		for key, lostTime := range self.pending {
			if first || lostTime.Before(oldestTime) {
				oldestKey, oldestTime, first = key, lostTime, false
			}
		}
		if first {
			break
		}
		delete(self.pending, oldestKey)
		self.recoveryMissed.Add(1)
	}
}

func (self *reliabilityMetrics) reset() {
	if self == nil {
		return
	}
	self.flowsOpened.Store(0)
	self.exitLossEvents.Store(0)
	self.flowsLostToExit.Store(0)
	self.maxFlowsLostInOneEvent.Store(0)
	self.recoveryCount.Store(0)
	self.recoveryNanos.Store(0)
	self.recoveryMaxNanos.Store(0)
	self.recoveryMissed.Store(0)
	self.dialFailures.Store(0)
	self.flowsReraced.Store(0)
	self.verdictsHeldUplinkStale.Store(0)
	self.verdictsHeldTransportDown.Store(0)
	self.removalsDeferred.Store(0)

	self.pendingLock.Lock()
	defer self.pendingLock.Unlock()
	self.pending = map[recoveryKey]time.Time{}
}

// ReliabilityMetricsSnapshot is a consistent-enough read of the counters for
// reporting. The counters are sampled independently, so a snapshot taken while
// traffic is flowing can straddle an update; that is fine for A/B comparison
// over a run and avoids taking a lock on the packet path.
type ReliabilityMetricsSnapshot struct {
	FlowsOpened uint64

	ExitLossEvents         uint64
	FlowsLostToExit        uint64
	MaxFlowsLostInOneEvent uint64
	// MeanFlowsLostPerExitLoss is the blast radius: the average number of
	// connections one provider failure costs. Lower is the goal.
	MeanFlowsLostPerExitLoss float64

	RecoveryCount     uint64
	RecoveryMissed    uint64
	RecoveryMeanNanos int64
	RecoveryMaxNanos  int64
	// RecoveryPending is how many destinations are still waiting to come back
	// at the moment of the snapshot.
	RecoveryPending int

	// DialFailuresIntercepted counts provider could-not-connect signals;
	// FlowsReraced counts the flows quietly moved to another exit in
	// response. These are the events the user never sees -- each one would
	// previously have been a 3-63s syn-backoff hang.
	DialFailuresIntercepted uint64
	FlowsReraced            uint64

	// VerdictsHeldUplinkStale and VerdictsHeldTransportDown count blackhole
	// verdicts suppressed because the evidence was inadmissible (the local
	// uplink was stale, the transport was known down); RemovalsDeferred
	// counts provider removals postponed while such a hold was in effect.
	VerdictsHeldUplinkStale   uint64
	VerdictsHeldTransportDown uint64
	RemovalsDeferred          uint64
}

func (self *reliabilityMetrics) snapshot() *ReliabilityMetricsSnapshot {
	if self == nil {
		return &ReliabilityMetricsSnapshot{}
	}
	exitLossEvents := self.exitLossEvents.Load()
	flowsLost := self.flowsLostToExit.Load()
	recoveryCount := self.recoveryCount.Load()
	recoveryNanos := self.recoveryNanos.Load()

	snapshot := &ReliabilityMetricsSnapshot{
		FlowsOpened:            self.flowsOpened.Load(),
		ExitLossEvents:         exitLossEvents,
		FlowsLostToExit:        flowsLost,
		MaxFlowsLostInOneEvent: self.maxFlowsLostInOneEvent.Load(),
		RecoveryCount:          recoveryCount,
		RecoveryMissed:         self.recoveryMissed.Load(),
		RecoveryMaxNanos:       int64(self.recoveryMaxNanos.Load()),

		DialFailuresIntercepted: self.dialFailures.Load(),
		FlowsReraced:            self.flowsReraced.Load(),

		VerdictsHeldUplinkStale:   self.verdictsHeldUplinkStale.Load(),
		VerdictsHeldTransportDown: self.verdictsHeldTransportDown.Load(),
		RemovalsDeferred:          self.removalsDeferred.Load(),
	}

	if 0 < exitLossEvents {
		snapshot.MeanFlowsLostPerExitLoss = float64(flowsLost) / float64(exitLossEvents)
	}
	if 0 < recoveryCount {
		snapshot.RecoveryMeanNanos = int64(recoveryNanos / recoveryCount)
	}

	self.pendingLock.Lock()
	snapshot.RecoveryPending = len(self.pending)
	self.pendingLock.Unlock()

	return snapshot
}

// ReliabilityMetrics reports what provider failures have cost this session.
func (self *RemoteUserNatMultiClient) ReliabilityMetrics() *ReliabilityMetricsSnapshot {
	return self.reliabilityMetrics.snapshot()
}

// ResetReliabilityMetrics zeroes the counters so an A/B run starts clean.
// Pending recoveries are dropped rather than carried across the boundary,
// since a recovery that began under the previous config would otherwise be
// credited to the next one.
func (self *RemoteUserNatMultiClient) ResetReliabilityMetrics() {
	self.reliabilityMetrics.reset()
}
