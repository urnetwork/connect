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

	// "github.com/golang/glog"

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
	Stats() *securityPolicyStats
	Inspect(provideMode protocol.ProvideMode, packet []byte) (*IpPath, SecurityPolicyResult, error)
}

type EgressSecurityPolicy struct {
	stats *securityPolicyStats
}

func DefaultEgressSecurityPolicy() *EgressSecurityPolicy {
	return DefaultEgressSecurityPolicyWithStats(DefaultSecurityPolicyStats())
}

func DefaultEgressSecurityPolicyWithStats(stats *securityPolicyStats) *EgressSecurityPolicy {
	return &EgressSecurityPolicy{
		stats: stats,
	}
}

func (self *EgressSecurityPolicy) Stats() *securityPolicyStats {
	return self.stats
}

func (self *EgressSecurityPolicy) Inspect(provideMode protocol.ProvideMode, packet []byte) (*IpPath, SecurityPolicyResult, error) {
	ipPath, result, err := self.inspect(provideMode, packet)
	if ipPath != nil {
		self.stats.AddDestination(ipPath, result, 1)
	}
	return ipPath, result, err
}

func (self *EgressSecurityPolicy) inspect(provideMode protocol.ProvideMode, packet []byte) (*IpPath, SecurityPolicyResult, error) {
	ipPath, err := ParseIpPath(packet)
	if err != nil {
		// bad ip packet
		return nil, SecurityPolicyResultDrop, err
	}

	// if protocol.ProvideMode_Network == provideMode {
	// 	return ipPath, SecurityPolicyResultAllow, nil
	// } else {

	// apply public rules:
	// - only public unicast network destinations
	// - block insecure or known unencrypted traffic

	if !isPublicUnicast(ipPath.DestinationIp) {
		return ipPath, SecurityPolicyResultIncident, nil
	}

	switch ipPath.Version {
	case 4:
		if dIp4 := ipPath.DestinationIp.To4(); dIp4 != nil {
			if blockIp4s[[4]byte(dIp4)] {
				return ipPath, SecurityPolicyResultDrop, nil
			}
		}
	case 6:
		// FIXME
	}

	// block insecure or unencrypted traffic
	// block known destructive protocols
	// - allow secure web and dns traffic (443)
	// - allow email protocols (465, 993, 995)
	// - allow dns over tls (853)
	// - allow user ports (>=1024)
	// - allow ports used by apply system: ntp (123), wifi calling (500)
	//   see https://support.apple.com/en-us/103229
	// - block bittorrent (6881-6889)
	// - FIXME temporarily enabling 53 and 80 until inline protocol translation is implemented
	// TODO in the future, allow a control message to dynamically adjust the security rules
	allow := func() bool {
		dPort := ipPath.DestinationPort
		// sPort := ipPath.SourcePort
		switch {
		case dPort == 53:
			// dns
			// FIXME for now we allow plain dns. TODO to upgrade the protocol to doh inline.
			return ipPath.Protocol == IpProtocolUdp
		case dPort == 80:
			// http
			// FIXME for now we allow http. It's important for some radio streaming. TODO to upgrade the protcol to https inline.
			return ipPath.Protocol == IpProtocolTcp
		case dPort == 443:
			// https/quic
			return true
		case dPort == 465, dPort == 993, dPort == 995:
			// email
			return true
		case dPort == 853:
			// dns over tls
			return true
		case dPort == 123, dPort == 500:
			// apple system ports
			return true
		case 6881 <= dPort && dPort <= 6889, dPort == 6969:
			// bittorrent
			return false
		case dPort == 1337, dPort == 9337, dPort == 2710:
			// unoffical bittorrent related
			return false
		case dPort < 1024:
			return false
		case 11000 <= dPort:
			// rtp and p2p
			// note many games use 10xxx so we allow this
			// FIXME turn this off when we have better deep packet inspection
			return false
		default:
			return true
		}
	}
	if allow() {
		return ipPath, SecurityPolicyResultAllow, nil
	}
	return ipPath, SecurityPolicyResultDrop, nil
}

type IngressSecurityPolicy struct {
	stats *securityPolicyStats
}

func DefaultIngressSecurityPolicy() *IngressSecurityPolicy {
	return DefaultIngressSecurityPolicyWithStats(DefaultSecurityPolicyStats())
}

func DefaultIngressSecurityPolicyWithStats(stats *securityPolicyStats) *IngressSecurityPolicy {
	return &IngressSecurityPolicy{
		stats: stats,
	}
}

func (self *IngressSecurityPolicy) Stats() *securityPolicyStats {
	return self.stats
}

func (self *IngressSecurityPolicy) Inspect(provideMode protocol.ProvideMode, packet []byte) (*IpPath, SecurityPolicyResult, error) {
	ipPath, result, err := self.inspect(provideMode, packet)
	if ipPath != nil {
		self.stats.AddSource(ipPath, result, 1)
	}
	return ipPath, result, err
}

func (self *IngressSecurityPolicy) inspect(provideMode protocol.ProvideMode, packet []byte) (*IpPath, SecurityPolicyResult, error) {
	ipPath, err := ParseIpPath(packet)
	if err != nil {
		// bad ip packet
		return nil, SecurityPolicyResultDrop, err
	}

	allow := func() bool {
		// dPort := ipPath.DestinationPort
		sPort := ipPath.SourcePort
		switch {
		case 11000 <= sPort:
			// rtp and p2p
			// note many games use 10xxx so we allow this
			// FIXME turn this off when we have better deep packet inspection
			return false
		default:
			return true
		}
	}
	if allow() {
		return ipPath, SecurityPolicyResultAllow, nil
	}
	return ipPath, SecurityPolicyResultDrop, nil
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
type securityPolicyStats struct {
	includeIp bool

	stateLock               sync.Mutex
	resultDestinationCounts SecurityPolicyStats
}

func DefaultSecurityPolicyStats() *securityPolicyStats {
	return &securityPolicyStats{
		includeIp:               false,
		resultDestinationCounts: SecurityPolicyStats{},
	}
}

func (self *securityPolicyStats) AddDestination(ipPath *IpPath, result SecurityPolicyResult, count uint64) {
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

func (self *securityPolicyStats) AddSource(ipPath *IpPath, result SecurityPolicyResult, count uint64) {
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

func (self *securityPolicyStats) Stats(reset bool) SecurityPolicyStats {
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
