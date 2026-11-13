package connect

// Prefer providers that enforce at least the client's own security policy
// rules after a provider with older rules blocked this client's traffic.
//
// The client's fail fast and destination hints (securityRoute) act only on the
// client's own verdict. A provider on an older connect can still drop a flow
// the client's rules admit (a detector or an exception the provider does not
// have yet), and the app's retry may then be placed on the same kind of
// provider again. Providers report their rules generation and the cumulative
// count of this client's outbound packets their policy dropped
// (IpProviderDiagnostics; see ProviderDiagnostics for the direction names).
// When that count rises on a provider whose generation is older than the
// client's own, or unknown, the preference arms for its ttl, and every further
// block re-arms it. While it is armed, effectiveTier demotes each provider
// that is not current by providerPolicyDemerit, so new flows, the app's retry
// among them, are placed on a current provider when the window has one. A rise
// on a current provider is a verdict the client's own rules share, which
// another provider would not change, so it does not arm the preference.
//
// The generation is the only order. A provider whose generation is at least
// the client's own never takes the demerit, so the preference never ranks an
// older or unknown generation above a newer one, and a newer, stricter
// provider is never routed around. Unknown covers providers that predate the
// field and providers that have not reported yet; it never counts as current.
// The inputs are what providers already send; nothing new leaves the device.

import (
	"sync/atomic"
	"time"
)

// A whole evidence-of-failure step (see effectiveTier): a provider that is not
// current falls behind every current provider of the next tier, so a retry is
// not raced back onto older rules in preference to a slightly lower ranked
// current provider.
const providerPolicyDemerit = 2

// The client's preference for current providers. Every window channel shares
// one: the generation, ttl and clock are fixed at construction and the arm
// time is atomic, so it is safe for concurrent use. A nil preference is no
// preference.
type providerPolicyPreference struct {
	// the client's own rules generation, never 0
	generation uint64
	ttl        time.Duration
	// injectable for tests
	now func() time.Time

	// when the armed preference lapses, unix nanos; 0 before the first arm
	untilUnixNanos atomic.Int64
}

// Returns nil (no preference) when ttl is not positive or the client's own
// generation is unknown, since a client with no generation of its own has
// nothing to compare providers against. now nil uses time.Now.
func newProviderPolicyPreference(generation uint64, ttl time.Duration, now func() time.Time) *providerPolicyPreference {
	if generation == 0 || ttl <= 0 {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	return &providerPolicyPreference{
		generation: generation,
		ttl:        ttl,
		now:        now,
	}
}

// Reports whether a provider's generation is at least the client's own. Unknown
// (0) is never current. With no preference every provider is current.
func (self *providerPolicyPreference) current(providerGeneration uint64) bool {
	if self == nil {
		return true
	}
	return providerGeneration != 0 && self.generation <= providerGeneration
}

// Compares a provider's newly accepted diagnostics with its previous snapshot
// (nil before the first) and arms the preference when the provider is not
// current and reports more dropped outbound packets of this client than before.
// Returns true when this call armed a lapsed preference.
func (self *providerPolicyPreference) observe(previous *ProviderDiagnostics, next *ProviderDiagnostics) bool {
	if self == nil || next == nil {
		return false
	}
	var blockCount int64
	if previous != nil {
		blockCount = previous.BlockIngressPacketCount
	}
	if next.BlockIngressPacketCount <= blockCount {
		return false
	}
	if self.current(next.SecurityPolicyGeneration) {
		return false
	}
	now := self.now()
	until := now.Add(self.ttl).UnixNano()
	for {
		previousUntil := self.untilUnixNanos.Load()
		if until <= previousUntil {
			return false
		}
		if self.untilUnixNanos.CompareAndSwap(previousUntil, until) {
			return previousUntil <= now.UnixNano()
		}
	}
}

// Reports whether the preference is armed now. A nil preference never is.
func (self *providerPolicyPreference) active() bool {
	if self == nil {
		return false
	}
	return self.now().UnixNano() < self.untilUnixNanos.Load()
}

// The effectiveTier step for a provider of the given generation:
// providerPolicyDemerit while the preference is armed and the provider is not
// current, else 0.
func (self *providerPolicyPreference) demerit(providerGeneration uint64) int {
	if self.current(providerGeneration) || !self.active() {
		return 0
	}
	return providerPolicyDemerit
}

// The parent's preference, which reaches the channel on its args. nil (no
// preference) for bare fixtures.
func (self *multiClientChannel) policyPreference() *providerPolicyPreference {
	if self.args == nil {
		return nil
	}
	return self.args.providerPolicyPreference
}

// The rules generation the provider last reported, 0 (unknown) before its first
// diagnostics.
func (self *multiClientChannel) providerPolicyGeneration() uint64 {
	if diagnostics := self.providerDiagnostics.Load(); diagnostics != nil {
		return diagnostics.SecurityPolicyGeneration
	}
	return 0
}

// Feeds one accepted diagnostics snapshot to the preference and names the
// arming in the log.
func (self *multiClientChannel) observeProviderDiagnostics(previous *ProviderDiagnostics, next *ProviderDiagnostics) {
	preference := self.policyPreference()
	if !preference.observe(previous, next) {
		return
	}
	loggerOrDefault(self.log).Infof("%s\n", relEvent(
		"policy_prefer",
		"exit", self.ClientId(),
		"generation", next.SecurityPolicyGeneration,
		"own", preference.generation,
		"ttl", preference.ttl,
	))
}
