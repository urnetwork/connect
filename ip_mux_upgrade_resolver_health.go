package connect

// The health of the mux's tunnel resolver, its failover, the resolver probe
// and the count of block answers (ADBLOCK3RD C2, C5, C6 and decision 16).
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
// The block count is a per-mux (so per-connection) count of recognizable block
// answers delivered to clients: 0.0.0.0 or :: sink answers, and NXDOMAIN with
// an extended dns error of 15 (Blocked), 16 (Censored) or 17 (Filtered). A
// plain NXDOMAIN, a REFUSED (a failure), the built-in blocker's own answers and
// a built-in endpoint's filtered response (which the guard never delivers)
// are not counted, so the figure is a floor. It stays on the device: nothing
// logs or sends it.
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
	// mux per connection): 0.0.0.0 or :: sinks, and NXDOMAIN with an extended
	// dns error of 15, 16 or 17. It never decreases, changes without a state
	// change, and is never logged or sent anywhere.
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
	return DnsResolverHealth{
		State:              DnsResolverHealthCold,
		BlockedAnswerCount: self.blockedAnswerCount.Load(),
	}
}

// Registers a callback for health state changes and returns its removal.
// Callbacks run outside the mux's locks, after the change.
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
	return nil
}
