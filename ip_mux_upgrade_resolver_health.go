package connect

// The health of the mux's tunnel resolver, its failover, the resolver probe
// and the count of block answers (ADBLOCK3RD C2, C5, C6 and B9).
//
// Health follows the resolver the device chose (DnsUpgradeSettings.Resolver):
// fresh answers and failed resolutions of the address path move it, counted
// only while the device reports the tunnel ready (SetTunnelReady), so a tunnel
// that is forming or down never makes a resolver failing. A cache hit is no
// evidence, an answer served stale is a failure, and a resolution canceled
// because another path answered first is neither. A network change or a
// settings swap starts a new generation: the state returns to cold on the
// resolver, and evidence from the old generation is dropped.
//
// With a Failover configured, a failing resolver is replaced once the failover
// has answered: new queries go to the failover, one resolver at a time, and a
// prober asks the resolver again every FailoverProbeInterval, switching back
// at its first answer. Without one the resolver fails closed, as every mux did
// before failover existed.
//
// The block count (ADBLOCK3RD B9) is a per-mux (so per-connection) count of
// recognizable block answers delivered to clients, one per client query: 0.0.0.0
// or :: sink answers, and NXDOMAIN with an extended dns error of 15 (Blocked),
// 16 (Censored) or 17 (Filtered). A plain NXDOMAIN, an NXDOMAIN with any other
// extended error (4 and 18 included), a REFUSED (a failure), the built-in
// blocker's own answers and a built-in endpoint's filtered response (which the
// guard never delivers) are not counted, so the figure is a floor. The device
// resets it when the selected preset changes (ResetDnsBlockedCount). It stays
// on the device: nothing logs or sends it.
//
// All methods are safe for concurrent use.

import (
	"context"
	"net/netip"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// The state of the mux's tunnel resolver.
type DnsResolverHealthState string

const (
	// The resolver has not answered since the mux started, the network
	// changed or the settings changed, and is not failing.
	DnsResolverHealthCold DnsResolverHealthState = "cold"
	// The resolver has answered and is not failing.
	DnsResolverHealthOk DnsResolverHealthState = "ok"
	// The resolver is failing and queries still go to it: no Failover is
	// configured (fail closed), or the failover has not answered yet.
	DnsResolverHealthFailing DnsResolverHealthState = "failing"
	// The resolver is failing and queries go to the Failover resolver.
	DnsResolverHealthFailedOver DnsResolverHealthState = "failedOver"
)

// A snapshot of the tunnel resolver's health.
type DnsResolverHealth struct {
	State DnsResolverHealthState
	// The consecutive failed resolutions of the resolver in the current run,
	// counted while the tunnel is ready.
	ConsecutiveFailureCount int
	// The last fresh answer of the resolver in this generation, zero for none.
	LastSuccessTime time.Time
	// The block answers delivered to clients since the mux was created (one
	// mux per connection) or the count was last reset: DnsBlockedCount. It
	// changes without a state change, and is never logged or sent anywhere.
	BlockedAnswerCount uint64
}

// Called with the new health when the state changes.
type DnsResolverHealthFunction func(health DnsResolverHealth)

// One DoH url's answer to a resolver probe (UpgradeMux.ProbeDnsResolver).
type DnsResolverProbeResult struct {
	// The url as configured. Its path can hold a personal configuration id,
	// so it must never be logged; DohUrlForLog gives its loggable form.
	Url string
	// Whether a DNS response arrived. False when the request failed (Err).
	Answered bool
	// The response code, when Answered.
	RCode dnsmessage.RCode
	// The A records of the answer, sinks (0.0.0.0) included.
	Addrs []netip.Addr
	// The extended dns error INFO-CODEs (RFC 8914) the response carries.
	EdeCodes []uint16
	// Why no response arrived, naming the url by scheme and host only.
	Err error
}

// Installs (or, with nil, removes) the predicate that reports whether the
// tunnel is ready to carry traffic. Resolver failures count toward failing
// only while it reports true, and a FallbackUntilTunnelReady fallback races
// only while it reports false. Without a predicate the tunnel never counts as
// ready. The device installs it with the window, as it installs
// SetIpv6Unroutable.
func (self *UpgradeMux) SetTunnelReady(tunnelReady func() bool) {
	if tunnelReady == nil {
		self.tunnelReady.Store(nil)
	} else {
		self.tunnelReady.Store(&tunnelReady)
	}
}

// Whether the device reports the tunnel ready (see SetTunnelReady).
func (self *UpgradeMux) isTunnelReady() bool {
	if f := self.tunnelReady.Load(); f != nil {
		return (*f)()
	}
	return false
}

// The current health of the tunnel resolver, with the block count.
func (self *UpgradeMux) DnsResolverHealth() DnsResolverHealth {
	self.resolverHealthLock.Lock()
	defer self.resolverHealthLock.Unlock()
	return self.resolverHealthSnapshotWithLock()
}

func (self *UpgradeMux) resolverHealthSnapshotWithLock() DnsResolverHealth {
	return DnsResolverHealth{
		State:                   self.resolverHealth.state(),
		ConsecutiveFailureCount: self.resolverHealth.failureCount,
		LastSuccessTime:         self.resolverHealth.lastSuccessTime,
		BlockedAnswerCount:      self.blockedAnswerCount.Load(),
	}
}

// The block answers this mux has delivered to clients since it was created or
// last reset (ADBLOCK3RD B9): 0.0.0.0 or :: sinks, and NXDOMAIN with an
// extended dns error of 15, 16 or 17, each client query counted once, a cached
// block included. The same count as DnsResolverHealth().BlockedAnswerCount.
// The mux keeps no names with it and never logs or sends it.
func (self *UpgradeMux) DnsBlockedCount() uint64 {
	return self.blockedAnswerCount.Load()
}

// Zeroes the block count (DnsBlockedCount). The device resets it when the
// selected preset changes, so the figure belongs to the provider it names; a
// new mux, one per connection, starts at zero.
func (self *UpgradeMux) ResetDnsBlockedCount() {
	self.blockedAnswerCount.Store(0)
}

// Registers a callback for health state changes and returns its removal.
// Callbacks run on the mux's own notifier goroutine, never on a DNS worker and
// never under the mux's locks, one at a time and in order. Changes are
// coalesced: each call carries the health current when it runs, so a quick
// succession of states may be delivered as its last one.
func (self *UpgradeMux) AddDnsResolverHealthCallback(callback DnsResolverHealthFunction) func() {
	callbackId := self.resolverHealthCallbacks.Add(callback)
	return func() {
		self.resolverHealthCallbacks.Remove(callbackId)
	}
}

// Asks every DoH url of the tunnel resolver (Resolver, never Failover) for
// the A records of name, once each, through the tunnel, and returns each
// url's response in the resolver's order. It bypasses the answer cache, the
// reverse index, the exit binding, the server scores, the health and the
// block count. Each request is bounded by ctx and the resolver's request
// timeout. Nil when the mux owns no DNS or the resolver has no remote DoH.
func (self *UpgradeMux) ProbeDnsResolver(ctx context.Context, name string) []DnsResolverProbeResult {
	if !self.dnsUpgradeEnabled() {
		return nil
	}
	return self.mux.Tun().DohCache().probe(ctx, name)
}

// The tunnel resolver's health in one generation (see the file comment). The
// methods take the time explicitly, so the thresholds are tested without a
// clock. Guarded by UpgradeMux.resolverHealthLock.
type resolverHealthState struct {
	// the tunnel resolver generation (UpgradeMux.tunnelDohGeneration) the
	// state belongs to; evidence from another generation is dropped
	generation   uint64
	failureCount int
	// the start of the first failed resolution of the current run
	failureRunStart time.Time
	lastSuccessTime time.Time
	failing         bool
	// queries go to the failover resolver; only while failing
	failedOver bool
}

func (self *resolverHealthState) state() DnsResolverHealthState {
	switch {
	case self.failedOver:
		return DnsResolverHealthFailedOver
	case self.failing:
		return DnsResolverHealthFailing
	case !self.lastSuccessTime.IsZero():
		return DnsResolverHealthOk
	default:
		return DnsResolverHealthCold
	}
}

// A fresh answer of the resolver at now: the run of failures ends, and a mux
// that failed over returns to the resolver.
func (self *resolverHealthState) success(now time.Time) {
	self.failureCount = 0
	self.failureRunStart = time.Time{}
	self.lastSuccessTime = now
	self.failing = false
	self.failedOver = false
}

// A failed resolution of the resolver that started at start and failed at
// now. The resolver becomes failing once the run holds minCount failures
// (non-positive never) spanning minDuration from the start of its earliest
// (non-positive: any span), and stays failing until a success.
func (self *resolverHealthState) failure(start time.Time, now time.Time, minCount int, minDuration time.Duration) {
	self.failureCount += 1
	if self.failureRunStart.IsZero() || start.Before(self.failureRunStart) {
		self.failureRunStart = start
	}
	if !self.failing && 0 < minCount && minCount <= self.failureCount &&
		(minDuration <= 0 || minDuration <= now.Sub(self.failureRunStart)) {
		self.failing = true
	}
}

// Applies one change to the health under its lock, keeps the lock-free failed
// over mirror in step, and publishes the state for the notifier
// (runResolverHealthCallbacks).
func (self *UpgradeMux) updateResolverHealth(update func(health *resolverHealthState)) {
	self.resolverHealthLock.Lock()
	defer self.resolverHealthLock.Unlock()
	update(&self.resolverHealth)
	self.resolverFailedOver.Store(self.resolverHealth.failedOver)
	// nil only for a mux a test assembles by hand, which runs no notifier
	if self.resolverHealthStateMonitor != nil {
		self.resolverHealthStateMonitor.Set(self.resolverHealth.state())
	}
}

// The clock of the health thresholds: resolverHealthNow, which tests replace,
// else the wall clock.
func (self *UpgradeMux) healthNow() time.Time {
	if self.resolverHealthNow != nil {
		return self.resolverHealthNow()
	}
	return time.Now()
}

// Starts a new generation's health: cold on the resolver, which a network
// change or a settings swap does (markTunnelDohUnproven).
func (self *UpgradeMux) resetResolverHealth(generation uint64) {
	self.updateResolverHealth(func(health *resolverHealthState) {
		*health = resolverHealthState{generation: generation}
	})
	// a running failover prober re-reads the state now instead of at its next
	// interval, and finds nothing left to do
	self.wakeFailoverProber()
}

// The phase the failover prober acts on, read under the health lock.
func (self *UpgradeMux) resolverHealthPhase() (generation uint64, failing bool, failedOver bool) {
	self.resolverHealthLock.Lock()
	defer self.resolverHealthLock.Unlock()
	return self.resolverHealth.generation, self.resolverHealth.failing, self.resolverHealth.failedOver
}

// Records one address resolution that went to the resolver (not the
// failover) as health evidence. A fresh answer is a success. A failure (no
// answer, or an answer served stale) counts only while the tunnel was ready
// when the resolution started and is ready now, and never when the resolution
// was canceled: another path answered first, or the mux closed. A cache hit is
// no evidence. Evidence from a retired generation is dropped.
func (self *UpgradeMux) recordResolverEvidence(
	generation uint64,
	start time.Time,
	readyAtStart bool,
	canceled bool,
	result dohLookupResult,
) {
	switch {
	case result.fresh:
		now := self.healthNow()
		self.updateResolverHealth(func(health *resolverHealthState) {
			if health.generation == generation {
				health.success(now)
			}
		})
	case result.stale || (len(result.addrs) == 0 && !result.authoritative):
		if canceled || !readyAtStart || !self.isTunnelReady() {
			return
		}
		var minCount int
		var minDuration time.Duration
		if dns := self.settings.Load().Dns; dns != nil {
			minCount = dns.ResolverFailingMinCount
			minDuration = dns.ResolverFailingMinDuration
		}
		now := self.healthNow()
		failing := false
		self.updateResolverHealth(func(health *resolverHealthState) {
			if health.generation == generation {
				health.failure(start, now, minCount, minDuration)
				failing = health.failing && !health.failedOver
			}
		})
		if failing {
			self.ensureFailoverProber()
		}
	}
}

// The tunnel DoH cache new queries go to, and whether it is the resolver's
// (Resolver) rather than the failover's. One cache at a time: the two never
// share a fan-out.
func (self *UpgradeMux) activeTunnelDohCache() (*DohCache, bool) {
	if self.resolverFailedOver.Load() {
		if failover := self.failoverDohCache.Load(); failover != nil {
			return failover, false
		}
	}
	return self.mux.Tun().DohCache(), true
}

// The failover resolver's tunnel DoH cache for the settings, or nil without a
// Failover: built on the tun like the resolver's, so it egresses at the exit,
// shares the owner's memory target and wave caps, and binds its answers to
// the exit that carried them.
func buildFailoverDohCache(tun *Tun, settings *UpgradeMuxSettings) *DohCache {
	if settings == nil || settings.Dns == nil || settings.Dns.Resolver == nil || settings.Dns.Failover == nil {
		return nil
	}
	return tun.buildDohCache(settings.Dns.Failover, dohRequestTimeout(settings))
}

// Whether the failover prober has work: a failing resolver with a failover to
// prove, or a failed over mux whose resolver it probes.
func (self *UpgradeMux) failoverProbeNeeded() bool {
	if self.ctx.Err() != nil || self.failoverDohCache.Load() == nil {
		return false
	}
	_, failing, _ := self.resolverHealthPhase()
	return failing
}

// Starts the failover prober when it has work. At most one runs; a request
// while it runs wakes it instead.
func (self *UpgradeMux) ensureFailoverProber() {
	if !self.failoverProbeNeeded() {
		return
	}
	if !self.failoverProberRunning.CompareAndSwap(false, true) {
		self.wakeFailoverProber()
		return
	}
	go HandleError(self.runFailoverProber)
}

// Wakes a waiting failover prober to re-read the state. Coalesced: one pending
// wake carries every request.
func (self *UpgradeMux) wakeFailoverProber() {
	select {
	case self.failoverProberWake <- struct{}{}:
	default:
	}
}

// The failover prober. While the resolver is failing and not failed over it
// proves the failover (one warm query through the tunnel), retrying with the
// cold prober's backoff, and fails over at the failover's first answer. While
// failed over it probes the resolver every FailoverProbeInterval and switches
// back at the resolver's first answer. It exits when there is nothing left to
// do: a success, a network change or a settings swap.
func (self *UpgradeMux) runFailoverProber() {
	defer func() {
		self.failoverProberRunning.Store(false)
		// close the CAS handoff race: work requested while this worker was
		// returning but before running became false
		if self.failoverProbeNeeded() {
			self.ensureFailoverProber()
		}
	}()
	// a wake queued for the worker that just retired is already in the state
	select {
	case <-self.failoverProberWake:
	default:
	}

	wait := func(delay time.Duration) bool {
		select {
		case <-self.ctx.Done():
			return false
		case <-self.failoverProberWake:
			return true
		case <-time.After(delay):
			return true
		}
	}

	warmFunction := self.failoverWarmFunction
	if warmFunction == nil {
		return
	}
	failureCount := 0
	for self.ctx.Err() == nil {
		generation, failing, failedOver := self.resolverHealthPhase()
		failover := self.failoverDohCache.Load()
		if !failing || failover == nil {
			return
		}
		if !failedOver {
			// no query goes to the failover before it has answered
			if warmFunction(self.ctx, failover) {
				failureCount = 0
				self.updateResolverHealth(func(health *resolverHealthState) {
					if health.generation == generation && health.failing {
						health.failedOver = true
					}
				})
				continue
			}
			failureCount += 1
			if !wait(dnsColdProbeDelay(failureCount, self.dnsProbeInitialDelay, self.dnsProbeMaxDelay)) {
				return
			}
			continue
		}

		var probeInterval time.Duration
		if dns := self.settings.Load().Dns; dns != nil {
			probeInterval = dns.FailoverProbeInterval
		}
		if probeInterval <= 0 {
			// failed over until a network change or a settings swap
			return
		}
		if !wait(probeInterval) {
			return
		}
		if probeGeneration, _, stillFailedOver := self.resolverHealthPhase(); probeGeneration != generation || !stillFailedOver {
			continue
		}
		if warmFunction(self.ctx, self.mux.Tun().DohCache()) {
			now := self.healthNow()
			self.updateResolverHealth(func(health *resolverHealthState) {
				if health.generation == generation {
					health.success(now)
				}
			})
		}
	}
}

// Delivers health state changes to the callbacks, from its own goroutine so
// that a slow callback never holds up a DNS worker or the failover prober.
// Changes are coalesced: each delivery carries the current health. Runs until
// the mux closes.
func (self *UpgradeMux) runResolverHealthCallbacks() {
	deliveredState := self.resolverHealthStateMonitor.Value()
	for {
		state, update := self.resolverHealthStateMonitor.Get()
		if state != deliveredState {
			health := self.DnsResolverHealth()
			deliveredState = health.State
			for _, callback := range self.resolverHealthCallbacks.Get() {
				HandleError(func() {
					callback(health)
				})
			}
		}
		select {
		case <-self.ctx.Done():
			return
		case <-update:
		}
	}
}
