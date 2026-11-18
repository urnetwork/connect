package connect

import (
	"maps"
	"net/netip"
	"slices"
	"sync"
	"time"
)

// net_extender_strategy_evidence.go -- the dial evidence of a strategy that
// reads a directory it does not own (ClientStrategySettings.
// ExtenderDirectoryReadOnly).
//
// A directory's holds and limits are evidence about the path of whoever dials
// through it. When several strategies of one process dial along different
// paths -- the sn miner's slots, each through a proxy of its own -- and share
// one directory, a slot whose proxy is down would report every extender it
// tried as failed and hold it for every slot, and for the owner's own network
// client and mesh. So a reading strategy keeps the outcomes of its own dials
// here instead: an address it failed to reach is held for it alone, on the
// directory's hold schedule and clock, a success clears that, and the
// directory hears nothing from it. It still honors what the directory's owner
// judged, since a held or revoked address is no candidate for anyone.
//
// Like the directory, the holds survive a path change, and a webrtc failure
// holds only an address that serves no socket carrier (S8, S9). Every method
// is safe for concurrent use.
type clientStrategyExtenderEvidence struct {
	// the directory whose hold schedule, clock and records this follows; it
	// is read, never written
	directory *ExtenderDirectory

	stateLock sync.Mutex
	ipHolds   map[netip.Addr]*clientStrategyExtenderHold
}

// One address this strategy failed to reach.
type clientStrategyExtenderHold struct {
	consecutiveFailureCount int
	holdUntilTime           time.Time
}

func newClientStrategyExtenderEvidence(directory *ExtenderDirectory) *clientStrategyExtenderEvidence {
	return &clientStrategyExtenderEvidence{
		directory: directory,
		ipHolds:   map[netip.Addr]*clientStrategyExtenderHold{},
	}
}

// A completed dial reached the address: its run of failures ends.
func (self *clientStrategyExtenderEvidence) recordSuccess(ip netip.Addr) {
	if !ip.IsValid() {
		return
	}
	ip = ip.Unmap()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.ipHolds, ip)
}

// A completed dial failed: the address is held for this strategy for the
// directory's doubling hold timeout.
func (self *clientStrategyExtenderEvidence) recordFailure(ip netip.Addr, connectMode ExtenderConnectMode) {
	if !ip.IsValid() {
		return
	}
	ip = ip.Unmap()
	// the directory is an external object, consulted with no lock held
	if connectMode == ExtenderConnectModeWebRtc && self.directory.addressServesSocketCarrier(ip) {
		return
	}
	now := self.directory.settings.Now()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	hold := self.ipHolds[ip]
	if hold == nil {
		hold = &clientStrategyExtenderHold{}
		self.ipHolds[ip] = hold
	}
	hold.consecutiveFailureCount += 1
	hold.holdUntilTime = now.Add(self.directory.holdTimeout(hold.consecutiveFailureCount))
	// bounded as the directory's own addresses are: past its cap, the holds
	// that passed longest ago go first
	if maxCount := self.directory.settings.MaxAddressCount; 0 < maxCount && maxCount < len(self.ipHolds) {
		ips := slices.Collect(maps.Keys(self.ipHolds))
		slices.SortFunc(ips, func(a netip.Addr, b netip.Addr) int {
			return self.ipHolds[a].holdUntilTime.Compare(self.ipHolds[b].holdUntilTime)
		})
		for _, oldIp := range ips[:len(ips)-maxCount] {
			delete(self.ipHolds, oldIp)
		}
	}
}

// Whether this strategy holds the address now.
func (self *clientStrategyExtenderEvidence) held(ip netip.Addr) bool {
	ip = ip.Unmap()
	now := self.directory.settings.Now()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	hold := self.ipHolds[ip]
	return hold != nil && extenderBefore(now, hold.holdUntilTime)
}

// The addresses this strategy holds now, which the expansion leaves out. A
// hold that has passed is kept, since its failure count still doubles the
// next one, until a success clears it.
func (self *clientStrategyExtenderEvidence) heldIps() []netip.Addr {
	now := self.directory.settings.Now()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	heldIps := []netip.Addr{}
	for ip, hold := range self.ipHolds {
		if extenderBefore(now, hold.holdUntilTime) {
			heldIps = append(heldIps, ip)
		}
	}
	return heldIps
}
