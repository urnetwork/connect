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

	"github.com/urnetwork/connect/protocol/v2025"
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

type SecurityPolicy struct {
	stats *securityPolicyStats
}

func DefaultSecurityPolicy() *SecurityPolicy {
	return &SecurityPolicy{
		stats: DefaultSecurityPolicyStats(),
	}
}

func (self *SecurityPolicy) Stats() *securityPolicyStats {
	return self.stats
}

func (self *SecurityPolicy) Inspect(provideMode protocol.ProvideMode, packet []byte) (*IpPath, SecurityPolicyResult, error) {
	ipPath, result, err := self.inspect(provideMode, packet)
	if err == nil {
		self.stats.Add(ipPath, result, 1)
	}
	return ipPath, result, err
}

func (self *SecurityPolicy) inspect(provideMode protocol.ProvideMode, packet []byte) (*IpPath, SecurityPolicyResult, error) {
	ipPath, err := ParseIpPath(packet)
	if err != nil {
		// bad ip packet
		return ipPath, SecurityPolicyResultDrop, err
	}

	if protocol.ProvideMode_Public <= provideMode {
		// apply public rules:
		// - only public unicast network destinations
		// - block insecure or known unencrypted traffic

		if !isPublicUnicast(ipPath.DestinationIp) {
			return ipPath, SecurityPolicyResultIncident, nil
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
			switch port := ipPath.DestinationPort; {
			case port == 53:
				// dns
				// FIXME for now we allow plain dns. TODO to upgrade the protocol to doh inline.
				return true
			case port == 80:
				// http
				// FIXME for now we allow http. It's important for some radio streaming. TODO to upgrade the protcol to https inline.
				return true
			case port == 443:
				// https
				return true
			case port == 465, port == 993, port == 995:
				// email
				return true
			case port == 853:
				// dns over tls
				return true
			case port < 1024:
				return false
			case port == 123, port == 500:
				// apple system ports
				return true
			case 6881 <= port && port <= 6889:
				// bittorrent
				return false
			default:
				return true
			}
		}
		if !allow() {
			return ipPath, SecurityPolicyResultDrop, nil
		}
	}

	return ipPath, SecurityPolicyResultAllow, nil
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

func (self *securityPolicyStats) Add(ipPath *IpPath, result SecurityPolicyResult, count uint64) {
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
