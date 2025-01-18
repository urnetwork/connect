package connect

type SecurityPolicyResult int

const (
	SecurityPolicyResultDrop     SecurityPolicyResult = 0
	SecurityPolicyResultAllow    SecurityPolicyResult = 1
	SecurityPolicyResultIncident SecurityPolicyResult = 2
)

type SecurityPolicy struct {
	stats *SecurityPolicyStats
}

func DefaultSecurityPolicy() *SecurityPolicy {
	return &SecurityPolicy{}
}

func (self *SecurityPolicy) Stats() *SecurityPolicyStats {
	return self.stats
}

func (self *SecurityPolicy) Inspect(provideMode protocol.ProvideMode, packet []byte) (*IpPath, SecurityPolicyResult, error) {
	ipPath, result, err := self.inspect(provideMode, packet)
	if err != nil {
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

	// FIXME for testing
	if true {
		return ipPath, SecurityPolicyResultAllow, nil
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

type SecurityDestination struct {
	Version  int
	Protocol IpProtocol
	Ip       netip.Addr
	Port     int
}

func newSecurityDestinationPort(ipPath *IpPath) SecurityDestination {
	// fixme use ip 0.0.0.0
}

func newSecurityDestination(ipPath *IpPath) SecurityDestination {
	// fixme
}

// get current counts of outcomes per (protocol, destination port)
type SecurityPolicyStats struct {
	// FIXME default false to save memory
	includeIp               bool
	stateLock               sync.Mutex
	resultDestinationCounts map[SecurityPolicyResult]map[SecurityDestination]int
}

func (self *SecurityPolicyStats) Add(ipPath *IpPath, result SecurityPolicyResult, count int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	var destination SecurityDestination
	if self.includeIp {
		destination = newSecurityDestination(ipPath)
	} else {
		// port only, no ip
		destination = newSecurityDestinationPort(ipPath)
	}

	destinationCounts, ok := self.resultDestinationCounts[destination]
	if !ok {
		destinationCounts = map[SecurityDestination]int{}
		self.resultDestinationCounts[destination] = destinationCounts
	}
	destinationCounts[destination] += count
}

func (self *SecurityPolicyStats) ResultCounts() map[SecurityPolicyResult]map[SecurityDestination]int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	resultDestinationCounts := map[SecurityPolicyResult]map[SecurityDestination]int{}
	for result, destinationCounts := range self.resultDestinationCounts {
		resultDestinationCounts[result] = maps.Clone(destinationCounts)
	}
	return resultDestinationCounts
}

func (self *SecurityPolicyStats) Reset() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	clear(self.resultDestinationCounts)
}
