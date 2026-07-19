package connect

import (
	"context"
	// "encoding/binary"
	// "errors"
	"fmt"
	// "io"
	// "math"
	// mathrand "math/rand"
	"net"
	// "slices"
	"strconv"
	// "strings"
	"sync"
	// "time"
	// "net/netip"

	// "github.com/google/gopacket"
	// "github.com/google/gopacket/layers"

	"maps"

	// "google.golang.org/protobuf/proto"

	// "github.com/urnetwork/glog"

	"github.com/urnetwork/connect/protocol"
)

type SecurityPolicyResult int

const (
	SecurityPolicyResultDrop     SecurityPolicyResult = 0
	SecurityPolicyResultAllow    SecurityPolicyResult = 1
	SecurityPolicyResultIncident SecurityPolicyResult = 2
)

func (self SecurityPolicyResult) String() string {
	switch self {
	case SecurityPolicyResultDrop:
		return "drop"
	case SecurityPolicyResultAllow:
		return "allow"
	case SecurityPolicyResultIncident:
		return "incident"
	default:
		return "unknown"
	}
}

type SecurityPolicy interface {
	Stats() *SecurityPolicyStatsCollector
	// InspectEgress decides the fate of a packet on the send (client->destination)
	// direction. payload is the L4 payload (may be nil for header-only inspection); it is
	// valid only for the duration of the call.
	InspectEgress(provideMode protocol.ProvideMode, ipPath *IpPath, payload []byte) (SecurityPolicyResult, error)
	// InspectIngress decides the fate of a packet on the return (destination->client)
	// direction.
	InspectIngress(provideMode protocol.ProvideMode, ipPath *IpPath, payload []byte) (SecurityPolicyResult, error)
	// RefreshEgress and RefreshIngress refresh a tracked flow's DPI activity time from a sent or
	// received packet respectively, so an active flow is not reclaimed by the idle scan (or the
	// capacity-LRU) while traffic still flows in either direction. They make no security decision;
	// call them at every forwarding point alongside (or in place of) the inspection.
	RefreshEgress(ipPath *IpPath)
	RefreshIngress(ipPath *IpPath)
}

// egressRelationship combines the packet source's provide mode with the local
// client's own provide mode into the single relationship the security policy
// enforces on egress. ProvideMode is a set of flags with no ordinal meaning (see
// its definition in protocol) — this is a per-case decision, never max/min:
// egress may reach non-public destinations (e.g. a LAN) only under a genuine
// same-Network relationship on BOTH sides. Anything else, including an
// unspecified None, is treated as Public so the public-destination rules apply.
func egressRelationship(source, client protocol.ProvideMode) protocol.ProvideMode {
	if source == protocol.ProvideMode_Network && client == protocol.ProvideMode_Network {
		return protocol.ProvideMode_Network
	}
	return protocol.ProvideMode_Public
}

// securityPolicy inspects both directions of a flow from one object, so the egress DPI
// detector's flow table is shared with the ingress activity refresh (see dmcaDetector.touch).
type securityPolicy struct {
	stats *SecurityPolicyStatsCollector
	cfaa  *cfaaDetector
	dmca  *dmcaDetector
}

func DefaultSecurityPolicy(ctx context.Context) SecurityPolicy {
	return DefaultSecurityPolicyWithStats(ctx, DefaultSecurityPolicyStatsCollector())
}

func DefaultSecurityPolicyWithStats(ctx context.Context, stats *SecurityPolicyStatsCollector) SecurityPolicy {
	return NewSecurityPolicy(
		ctx,
		DefaultCfaaSecurityPolicySettings(),
		DefaultDmcaSecurityPolicySettings(),
		DefaultWebStandardSettings(),
		stats,
	)
}

func NewSecurityPolicy(ctx context.Context, cfaaSettings *CfaaSecurityPolicySettings, dmcaSettings *DmcaSecurityPolicySettings, webSettings *WebStandardSettings, stats *SecurityPolicyStatsCollector) SecurityPolicy {
	return &securityPolicy{
		stats: stats,
		cfaa:  newCfaaDetector(cfaaSettings),
		dmca:  newDmcaDetector(ctx, dmcaSettings, newWebStandardDetector(webSettings)),
	}
}

// DefaultProviderSecurityPolicy is the policy for the provider (exit) role: it egresses a remote
// client's traffic, so it runs Reverse(client policy). The provider's ingress (the remote client's
// outbound, received from the tunnel) gets the client policy's egress DPI; the provider's egress
// (the return into the tunnel) gets the client policy's ingress source check. A provider keeps its
// own detector + stats, independent of the device's multi-client policy.
func DefaultProviderSecurityPolicy(ctx context.Context) SecurityPolicy {
	return DefaultProviderSecurityPolicyWithStats(ctx, DefaultSecurityPolicyStatsCollector())
}

func DefaultProviderSecurityPolicyWithStats(ctx context.Context, stats *SecurityPolicyStatsCollector) SecurityPolicy {
	return Reverse(DefaultSecurityPolicyWithStats(ctx, stats))
}

func (self *securityPolicy) Stats() *SecurityPolicyStatsCollector {
	return self.stats
}

func (self *securityPolicy) InspectEgress(provideMode protocol.ProvideMode, ipPath *IpPath, payload []byte) (SecurityPolicyResult, error) {
	result, err := self.inspectEgress(provideMode, ipPath, payload)
	if ipPath != nil {
		self.stats.AddDestination(ipPath, result, 1)
	}
	return result, err
}

func (self *securityPolicy) inspectEgress(provideMode protocol.ProvideMode, ipPath *IpPath, payload []byte) (SecurityPolicyResult, error) {
	if protocol.ProvideMode_Network == provideMode {
		return SecurityPolicyResultAllow, nil
	}

	// apply public rules:
	// - only public unicast network destinations
	// - block insecure or known unencrypted traffic
	if !isPublicUnicast(ipPath.DestinationIp) {
		return SecurityPolicyResultIncident, nil
	}

	// static endpoint reputation (blocked ips + port policy) on the destination
	switch self.cfaa.inspect(ipPath.DestinationIp, ipPath.DestinationPort, ipPath.Protocol, ipPath.Version) {
	case cfaaDrop:
		return SecurityPolicyResultDrop, nil
	case cfaaAllow:
		return SecurityPolicyResultAllow, nil
	default:
		// No static verdict — run stateful payload DPI. Switch on the verdict so
		// the policy reads explicitly: a positive BitTorrent signature is reported;
		// a flow that looks fully encrypted is dropped UNLESS it matched a
		// sanctioned web standard (TLS/QUIC/DTLS/STUN) — that web-standard match is
		// the fallback that rescues an otherwise-ambiguous encrypted flow.
		// Enforcement of each verdict honors the detector settings (log-only,
		// drop/report toggles), applied by result().
		switch v := self.dmca.classify(ipPath, payload); v {
		case dmcaBittorrent:
			return self.dmca.result(v), nil
		case dmcaDropEncrypted:
			return self.dmca.result(v), nil
		default:
			// still inspecting, a sanctioned web standard, or benign plaintext
			return SecurityPolicyResultAllow, nil
		}
	}
}

func (self *securityPolicy) InspectIngress(provideMode protocol.ProvideMode, ipPath *IpPath, payload []byte) (SecurityPolicyResult, error) {
	result, err := self.inspectIngress(provideMode, ipPath)
	if ipPath != nil {
		self.stats.AddSource(ipPath, result, 1)
	}
	return result, err
}

// RefreshEgress/RefreshIngress refresh the flow's DPI activity time without making a decision (see
// the SecurityPolicy interface).
func (self *securityPolicy) RefreshEgress(ipPath *IpPath) {
	if ipPath != nil {
		self.dmca.touchEgress(ipPath)
	}
}

func (self *securityPolicy) RefreshIngress(ipPath *IpPath) {
	if ipPath != nil {
		self.dmca.touchIngress(ipPath)
	}
}

func (self *securityPolicy) inspectIngress(provideMode protocol.ProvideMode, ipPath *IpPath) (SecurityPolicyResult, error) {
	// network-relationship traffic (e.g. same network_id) bypasses the public
	// rules, mirroring the egress policy. The return path of a network-mode
	// flow echoes the network provide mode, so a private service on any port
	// (including the p2p range) is not filtered here.
	if protocol.ProvideMode_Network == provideMode {
		return SecurityPolicyResultAllow, nil
	}

	// mirror the egress static drops (blocked ips + port policy), evaluated on the
	// source endpoint
	if cfaaDrop == self.cfaa.inspect(ipPath.SourceIp, ipPath.SourcePort, ipPath.Protocol, ipPath.Version) {
		return SecurityPolicyResultDrop, nil
	}
	return SecurityPolicyResultAllow, nil
}

type disableSecurityPolicy struct {
	stats *SecurityPolicyStatsCollector
}

func DisableSecurityPolicy() SecurityPolicy {
	return &disableSecurityPolicy{
		stats: DefaultSecurityPolicyStatsCollector(),
	}
}

// DisableSecurityPolicyWithStats matches the SecurityPolicyGenerator signature (ctx is
// unused — the disabled policy keeps no flow state and runs no scan).
func DisableSecurityPolicyWithStats(ctx context.Context, stats *SecurityPolicyStatsCollector) SecurityPolicy {
	return &disableSecurityPolicy{
		stats: stats,
	}
}

func (self *disableSecurityPolicy) Stats() *SecurityPolicyStatsCollector {
	return self.stats
}

func (self *disableSecurityPolicy) InspectEgress(provideMode protocol.ProvideMode, ipPath *IpPath, payload []byte) (SecurityPolicyResult, error) {
	return SecurityPolicyResultAllow, nil
}

func (self *disableSecurityPolicy) InspectIngress(provideMode protocol.ProvideMode, ipPath *IpPath, payload []byte) (SecurityPolicyResult, error) {
	return SecurityPolicyResultAllow, nil
}

func (self *disableSecurityPolicy) RefreshEgress(ipPath *IpPath) {}

func (self *disableSecurityPolicy) RefreshIngress(ipPath *IpPath) {}

// reverseSecurityPolicy swaps the egress and ingress directions of an underlying policy — the
// provider's view of a flow. The remote client's egress (the outbound packet the provider receives
// from the tunnel) is the provider's ingress, and the return is the provider's egress; so a provider
// runs Reverse(client policy), wired with the same convention as the multi-client. The flow key is
// unchanged (the underlying policy still keys by the egress 5-tuple), so only the method is swapped.
type reverseSecurityPolicy struct {
	policy SecurityPolicy
}

func Reverse(policy SecurityPolicy) SecurityPolicy {
	return &reverseSecurityPolicy{policy: policy}
}

func (self *reverseSecurityPolicy) Stats() *SecurityPolicyStatsCollector {
	return self.policy.Stats()
}

func (self *reverseSecurityPolicy) InspectEgress(provideMode protocol.ProvideMode, ipPath *IpPath, payload []byte) (SecurityPolicyResult, error) {
	return self.policy.InspectIngress(provideMode, ipPath, payload)
}

func (self *reverseSecurityPolicy) InspectIngress(provideMode protocol.ProvideMode, ipPath *IpPath, payload []byte) (SecurityPolicyResult, error) {
	return self.policy.InspectEgress(provideMode, ipPath, payload)
}

func (self *reverseSecurityPolicy) RefreshEgress(ipPath *IpPath) {
	self.policy.RefreshIngress(ipPath)
}

func (self *reverseSecurityPolicy) RefreshIngress(ipPath *IpPath) {
	self.policy.RefreshEgress(ipPath)
}

// Testing_FlowCount reports the number of tracked DMCA flows. Test hook: exact flow-table
// assertions (fill == n, reclaim -> 0) are deterministic where heap-delta assertions are
// not (allocator noise, -race). Reach it by type-asserting a SecurityPolicy to
// interface{ Testing_FlowCount() int }.
func (self *securityPolicy) Testing_FlowCount() int {
	return self.dmca.flowCount()
}

func (self *reverseSecurityPolicy) Testing_FlowCount() int {
	if p, ok := self.policy.(interface{ Testing_FlowCount() int }); ok {
		return p.Testing_FlowCount()
	}
	return 0
}

func isPublicUnicast(ip net.IP) bool {
	switch {
	case ip.IsPrivate(),
		ip.IsLoopback(),
		ip.IsLinkLocalUnicast(),
		ip.IsMulticast(),
		ip.IsUnspecified():
		return false
	default:
		return true
	}
}

type SecurityPolicyStats = map[SecurityPolicyResult]map[SecurityDestination]uint64

type SecurityDestination struct {
	Version  int
	Protocol IpProtocol
	Ip       string
	Port     int
}

func newSecurityDestinationPort(ipPath *IpPath) SecurityDestination {
	return SecurityDestination{
		Version:  ipPath.Version,
		Protocol: ipPath.Protocol,
		Ip:       "",
		Port:     ipPath.DestinationPort,
	}
}

func newSecurityDestination(ipPath *IpPath) SecurityDestination {
	return SecurityDestination{
		Version:  ipPath.Version,
		Protocol: ipPath.Protocol,
		Ip:       ipPath.DestinationIp.String(),
		Port:     ipPath.DestinationPort,
	}
}

func newSecuritySourcePort(ipPath *IpPath) SecurityDestination {
	return SecurityDestination{
		Version:  ipPath.Version,
		Protocol: ipPath.Protocol,
		Ip:       "",
		Port:     ipPath.SourcePort,
	}
}

func newSecuritySource(ipPath *IpPath) SecurityDestination {
	return SecurityDestination{
		Version:  ipPath.Version,
		Protocol: ipPath.Protocol,
		Ip:       ipPath.SourceIp.String(),
		Port:     ipPath.SourcePort,
	}
}

func (self *SecurityDestination) Cmp(b SecurityDestination) int {
	if self.Version < b.Version {
		return -1
	} else if b.Version < self.Version {
		return 1
	}

	if self.Protocol < b.Protocol {
		return -1
	} else if b.Protocol < self.Protocol {
		return 1
	}

	if self.Ip < b.Ip {
		return -1
	} else if b.Ip < self.Ip {
		return 1
	}

	if self.Port < b.Port {
		return -1
	} else if b.Port < self.Port {
		return 1
	}

	return 0
}

func (self *SecurityDestination) String() string {
	return fmt.Sprintf("ipv%d %s %s",
		self.Version,
		self.Protocol.String(),
		net.JoinHostPort(self.Ip, strconv.Itoa(self.Port)),
	)
}

// get current counts of outcomes per (protocol, destination port)
type SecurityPolicyStatsCollector struct {
	includeIp bool

	stateLock               sync.Mutex
	resultDestinationCounts SecurityPolicyStats
}

func DefaultSecurityPolicyStatsCollector() *SecurityPolicyStatsCollector {
	return &SecurityPolicyStatsCollector{
		includeIp:               false,
		resultDestinationCounts: SecurityPolicyStats{},
	}
}

func (self *SecurityPolicyStatsCollector) AddDestination(ipPath *IpPath, result SecurityPolicyResult, count uint64) {
	var destination SecurityDestination
	if self.includeIp {
		destination = newSecurityDestination(ipPath)
	} else {
		// port only, no ip
		destination = newSecurityDestinationPort(ipPath)
	}

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	destinationCounts, ok := self.resultDestinationCounts[result]
	if !ok {
		destinationCounts = map[SecurityDestination]uint64{}
		self.resultDestinationCounts[result] = destinationCounts
	}
	destinationCounts[destination] += count
}

func (self *SecurityPolicyStatsCollector) AddSource(ipPath *IpPath, result SecurityPolicyResult, count uint64) {
	var destination SecurityDestination
	if self.includeIp {
		destination = newSecuritySource(ipPath)
	} else {
		// port only, no ip
		destination = newSecuritySourcePort(ipPath)
	}

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	destinationCounts, ok := self.resultDestinationCounts[result]
	if !ok {
		destinationCounts = map[SecurityDestination]uint64{}
		self.resultDestinationCounts[result] = destinationCounts
	}
	destinationCounts[destination] += count
}

func (self *SecurityPolicyStatsCollector) Stats(reset bool) SecurityPolicyStats {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	resultDestinationCounts := SecurityPolicyStats{}
	for result, destinationCounts := range self.resultDestinationCounts {
		resultDestinationCounts[result] = maps.Clone(destinationCounts)
	}
	if reset {
		clear(self.resultDestinationCounts)
	}
	return resultDestinationCounts
}
