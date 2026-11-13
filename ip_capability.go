package connect

// ip_capability.go — the two capability axes of find-providers2's selector
// mode (server FP2-PROTOCOL.md §3, §5, §7): a provider's label on the ip axis
// (dualstack or ipv4) and on the protocol axis (dualproto or tcp), the
// `select` a window asks with, and what a flow needs from the exit that
// carries it.
//
// Labels come only from the server. A provider answered without them -- by an
// older server, which ignores `select`, or a destination that bypassed
// discovery (a fixed client id, a restored identity) -- is labelled from its
// `ip_family` and carries udp: every provider an older server returns passed
// its hard udp gate, and no negative evidence means capable (D7.4). Nothing a
// client observes on its own flows changes a label (D7.5, D11.8).
//
// The ip label is mirrored into the destination's `IpFamily`, which every
// placement site already reads, so an exit has one ip category. The protocol
// label decides only udp: tcp and icmp follow every label (§7.2).

// A provider's label on the ip axis.
type ProviderIpCapability string

const (
	// carries ipv4 and ipv6
	ProviderIpCapabilityDualstack ProviderIpCapability = "dualstack"
	// carries ipv4
	ProviderIpCapabilityIpv4 ProviderIpCapability = "ipv4"
)

// A provider's label on the protocol axis.
type ProviderProtocolCapability string

const (
	// carries tcp and udp
	ProviderProtocolCapabilityDualproto ProviderProtocolCapability = "dualproto"
	// carries tcp: the provider's udp failed the quality probe
	ProviderProtocolCapabilityTcp ProviderProtocolCapability = "tcp"
	// A selector, never a label: every protocol label, dualproto and tcp
	// providers in one pool ranked by the request's rank mode alone, with no
	// overflow (server FP2-PROTOCOL.md §6.3). A label that reads any
	// normalizes to tcp like every unknown label.
	ProviderProtocolCapabilityAny ProviderProtocolCapability = "any"
)

// One provider's labels, as find-providers2 answers them in selector mode
// (`FindProvidersProvider.Capabilities`).
type ProviderCapabilities struct {
	Ip       ProviderIpCapability       `json:"ip"`
	Protocol ProviderProtocolCapability `json:"protocol"`
}

// The labels this build places by. An empty protocol is a provider labelled
// without evidence against it, which carries udp (D7.4). A protocol this build
// does not know reads as tcp and an unknown ip label as ipv4, so a label a
// newer server adds never sends a flow into an exit that may not carry it.
func (self ProviderCapabilities) Normalize() ProviderCapabilities {
	normalized := ProviderCapabilities{
		Ip:       ProviderIpCapabilityIpv4,
		Protocol: ProviderProtocolCapabilityTcp,
	}
	if self.Ip == ProviderIpCapabilityDualstack {
		normalized.Ip = ProviderIpCapabilityDualstack
	}
	switch self.Protocol {
	case "", ProviderProtocolCapabilityDualproto:
		normalized.Protocol = ProviderProtocolCapabilityDualproto
	}
	return normalized
}

// Whether a provider with these labels carries udp.
func (self ProviderCapabilities) CarriesUdp() bool {
	return self.Normalize().Protocol == ProviderProtocolCapabilityDualproto
}

// The category a selector-mode provider is placed by: its ip label. Every
// provider of the two-axis model carries ipv4 (FP2-PROTOCOL.md §3.6).
func (self ProviderCapabilities) IpFamily() IpFamily {
	if self.Normalize().Ip == ProviderIpCapabilityDualstack {
		return IpFamilyDualstack
	}
	return IpFamilyV4Only
}

// The labels of a provider answered without them (FP2-PROTOCOL.md §5.4):
// `dualstack` when its proven category is dualstack, else `ipv4`, and
// `dualproto`. A v6-only provider reads `ipv4` here, the doc's mapping, but it
// keeps its v6-only category for placement (see addProviderDestinations).
func providerCapabilitiesFromIpFamily(ipFamily IpFamily) ProviderCapabilities {
	capabilities := ProviderCapabilities{
		Ip:       ProviderIpCapabilityIpv4,
		Protocol: ProviderProtocolCapabilityDualproto,
	}
	if ipFamily.Normalize() == IpFamilyDualstack {
		capabilities.Ip = ProviderIpCapabilityDualstack
	}
	return capabilities
}

// find-providers2's `select` (FP2-PROTOCOL.md §5.1). A request that carries it
// is in selector mode: the server answers every provider with its labels and
// echoes the selectors it applied. A request without it is in legacy mode and
// gets exactly the answer an older client gets.
//
// Every member is sent. An absent member takes the server's default, and the
// server defaults dualstack_target_count to 1, which a window that wants no
// ipv6 slot must not get.
type FindProvidersSelect struct {
	Ip       ProviderIpCapability       `json:"ip"`
	Protocol ProviderProtocolCapability `json:"protocol"`
	// The window's target, at least 1. Only the protocol axis reads it: a
	// dualproto request overflows to tcp providers when the held dualproto
	// clients and the eligible candidates do not reach it (D7.3, D11.2).
	TargetCount int `json:"target_count"`
	// The window clients held whose protocol label is the requested one:
	// every one for any.
	HeldCount int `json:"held_count"`
	// The window's ipv6 minimum (D11.1). The server reads it as 0 for an ipv4
	// request.
	DualstackTargetCount int `json:"dualstack_target_count"`
	// The window clients held that are labelled dualstack.
	HeldDualstackCount int `json:"held_dualstack_count"`
}

// The capabilities a normal window asks find-providers2 for: `select.ip` and
// `select.protocol` (server FP2-PROTOCOL.md §5.1, §6.3). The window fills in
// the counts (FindProvidersSelect). The limited window always asks with ipv4
// and tcp.
type ProviderSelectors struct {
	// dualstack: the window's ipv6 minimum is drawn from dualstack providers
	// first. ipv4: no ipv6 need, so dualstack providers come only in
	// proportion to their share of the pool.
	Ip ProviderIpCapability
	// dualproto: dualproto providers, and tcp ones as overflow only when the
	// dualproto ones cannot reach the window's target. any: dualproto and tcp
	// providers in one pool, with no overflow and no tier offset.
	Protocol ProviderProtocolCapability
}

// The selectors as a normal window sends them. An ip other than ipv4 reads as
// dualstack and a protocol other than any as dualproto: the server's defaults
// for an absent member (§5.1), so a settings value built without the field
// asks as every window did before it, and tcp stays the limited window's.
func (self ProviderSelectors) normalize() ProviderSelectors {
	normalized := ProviderSelectors{
		Ip:       ProviderIpCapabilityDualstack,
		Protocol: ProviderProtocolCapabilityDualproto,
	}
	if self.Ip == ProviderIpCapabilityIpv4 {
		normalized.Ip = ProviderIpCapabilityIpv4
	}
	if self.Protocol == ProviderProtocolCapabilityAny {
		normalized.Protocol = ProviderProtocolCapabilityAny
	}
	return normalized
}

// What a flow needs from the exit that carries it: its ip version (4 or 6)
// and its protocol. The zero value needs nothing, for callers that gather
// exits without a flow in hand.
type flowNeed struct {
	ipVersion  int
	ipProtocol IpProtocol
}

// The need of the flow on this path. A nil path needs nothing.
func flowNeedOf(ipPath *IpPath) flowNeed {
	if ipPath == nil {
		return flowNeed{}
	}
	return flowNeed{
		ipVersion:  ipPath.Version,
		ipProtocol: ipPath.Protocol,
	}
}

// Whether an exit of this category and these labels can carry the flow
// (FP2-PROTOCOL.md §7.2): ipv6 needs an ipv6-capable category, udp needs
// dualproto, and tcp and icmp follow every label.
func (self flowNeed) carriedBy(ipFamily IpFamily, capabilities ProviderCapabilities) bool {
	if self.ipVersion != 0 && !ipFamily.SupportsIpVersion(self.ipVersion) {
		return false
	}
	return self.ipProtocol != IpProtocolUdp || capabilities.CarriesUdp()
}
