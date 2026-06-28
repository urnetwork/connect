package connect

import (
	// "context"
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

	"golang.org/x/exp/maps"

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
	// Inspect decides the fate of a packet. payload is the L4 payload (may be nil
	// for header-only inspection); it is valid only for the duration of the call.
	Inspect(provideMode protocol.ProvideMode, ipPath *IpPath, payload []byte) (SecurityPolicyResult, error)
}

type egressSecurityPolicy struct {
	stats *SecurityPolicyStatsCollector
	cfaa  *cfaaDetector
	dmca  *dmcaDetector
}

func DefaultEgressSecurityPolicy() SecurityPolicy {
	return DefaultEgressSecurityPolicyWithStats(DefaultSecurityPolicyStatsCollector())
}

func DefaultEgressSecurityPolicyWithStats(stats *SecurityPolicyStatsCollector) SecurityPolicy {
	return NewEgressSecurityPolicy(
		DefaultCfaaSecurityPolicySettings(),
		DefaultDmcaSecurityPolicySettings(),
		DefaultWebStandardSettings(),
		stats,
	)
}

func NewEgressSecurityPolicy(cfaaSettings *CfaaSecurityPolicySettings, dmcaSettings *DmcaSecurityPolicySettings, webSettings *WebStandardSettings, stats *SecurityPolicyStatsCollector) SecurityPolicy {
	return &egressSecurityPolicy{
		stats: stats,
		cfaa:  newCfaaDetector(cfaaSettings),
		dmca:  newDmcaDetector(dmcaSettings, newWebStandardDetector(webSettings)),
	}
}

func (self *egressSecurityPolicy) Stats() *SecurityPolicyStatsCollector {
	return self.stats
}

func (self *egressSecurityPolicy) Inspect(provideMode protocol.ProvideMode, ipPath *IpPath, payload []byte) (SecurityPolicyResult, error) {
	result, err := self.inspect(provideMode, ipPath, payload)
	if ipPath != nil {
		self.stats.AddDestination(ipPath, result, 1)
	}
	return result, err
}

func (self *egressSecurityPolicy) inspect(provideMode protocol.ProvideMode, ipPath *IpPath, payload []byte) (SecurityPolicyResult, error) {
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

type ingressSecurityPolicy struct {
	stats *SecurityPolicyStatsCollector
	cfaa  *cfaaDetector
}

func DefaultIngressSecurityPolicy() SecurityPolicy {
	return DefaultIngressSecurityPolicyWithStats(DefaultSecurityPolicyStatsCollector())
}

func DefaultIngressSecurityPolicyWithStats(stats *SecurityPolicyStatsCollector) SecurityPolicy {
	return NewIngressSecurityPolicy(DefaultCfaaSecurityPolicySettings(), stats)
}

func NewIngressSecurityPolicy(cfaaSettings *CfaaSecurityPolicySettings, stats *SecurityPolicyStatsCollector) SecurityPolicy {
	return &ingressSecurityPolicy{
		stats: stats,
		cfaa:  newCfaaDetector(cfaaSettings),
	}
}

func (self *ingressSecurityPolicy) Stats() *SecurityPolicyStatsCollector {
	return self.stats
}

func (self *ingressSecurityPolicy) Inspect(provideMode protocol.ProvideMode, ipPath *IpPath, payload []byte) (SecurityPolicyResult, error) {
	result, err := self.inspect(provideMode, ipPath)
	self.stats.AddSource(ipPath, result, 1)
	return result, err
}

func (self *ingressSecurityPolicy) inspect(provideMode protocol.ProvideMode, ipPath *IpPath) (SecurityPolicyResult, error) {
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
	return DisableSecurityPolicyWithStats(DefaultSecurityPolicyStatsCollector())
}

func DisableSecurityPolicyWithStats(stats *SecurityPolicyStatsCollector) SecurityPolicy {
	return &disableSecurityPolicy{
		stats: stats,
	}
}

func (self *disableSecurityPolicy) Stats() *SecurityPolicyStatsCollector {
	return self.stats
}

func (self *disableSecurityPolicy) Inspect(provideMode protocol.ProvideMode, ipPath *IpPath, payload []byte) (SecurityPolicyResult, error) {
	return SecurityPolicyResultAllow, nil
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
