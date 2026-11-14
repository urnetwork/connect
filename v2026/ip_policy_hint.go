package connect

import (
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// policyHintCache remembers destinations (ip, port, transport) whose flow the
// local security policy dropped as unsanctioned encrypted traffic, so the
// app's retries to the same destination are handled consistently from their
// first packet: routed locally when the local security bypass is on, refused
// at once when it is off. It is local state only and is never sent anywhere.
//
// Entries expire after ttl; at maxCount an insert evicts the soonest to expire
// of a sample (approximate lru, as blockActionCache). A BitTorrent incident never
// creates a hint. Safe for concurrent use.
type policyHintCache struct {
	ttl      time.Duration
	maxCount int
	// injectable for tests
	now func() time.Time

	// lock-free fast path for the common empty cache
	count atomic.Int64

	stateLock   sync.Mutex
	expireTimes map[policyHintKey]time.Time
}

type policyHintKey struct {
	addr     netip.Addr
	port     uint16
	protocol IpProtocol
}

// newPolicyHintCache returns nil (no hints) when ttl or maxCount is not
// positive. now nil uses time.Now.
func newPolicyHintCache(ttl time.Duration, maxCount int, now func() time.Time) *policyHintCache {
	if ttl <= 0 || maxCount <= 0 {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	return &policyHintCache{
		ttl:         ttl,
		maxCount:    maxCount,
		now:         now,
		expireTimes: map[policyHintKey]time.Time{},
	}
}

func policyHintKeyForPath(ipPath *IpPath) (policyHintKey, bool) {
	switch ipPath.Protocol {
	case IpProtocolTcp, IpProtocolUdp:
	default:
		return policyHintKey{}, false
	}
	addr, ok := ipAssocAddr(ipPath.DestinationIp)
	if !ok {
		return policyHintKey{}, false
	}
	return policyHintKey{
		addr:     addr,
		port:     uint16(ipPath.DestinationPort),
		protocol: ipPath.Protocol,
	}, true
}

func (self *policyHintCache) add(ipPath *IpPath) {
	if self == nil {
		return
	}
	key, ok := policyHintKeyForPath(ipPath)
	if !ok {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if _, ok := self.expireTimes[key]; !ok && self.maxCount <= len(self.expireTimes) {
		self.evictSoonestSampleWithLock()
	}
	self.expireTimes[key] = self.now().Add(self.ttl)
	self.count.Store(int64(len(self.expireTimes)))
}

func (self *policyHintCache) has(ipPath *IpPath) bool {
	if self == nil || self.count.Load() == 0 {
		return false
	}
	key, ok := policyHintKeyForPath(ipPath)
	if !ok {
		return false
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	expireTime, ok := self.expireTimes[key]
	if !ok {
		return false
	}
	if !self.now().Before(expireTime) {
		delete(self.expireTimes, key)
		self.count.Store(int64(len(self.expireTimes)))
		return false
	}
	return true
}

func (self *policyHintCache) len() int {
	if self == nil {
		return 0
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.expireTimes)
}

func (self *policyHintCache) evictSoonestSampleWithLock() {
	var evictKey policyHintKey
	var evictTime time.Time
	found := false
	i := 0
	for key, expireTime := range self.expireTimes {
		if !found || expireTime.Before(evictTime) {
			evictKey = key
			evictTime = expireTime
			found = true
		}
		i += 1
		if blockActionEvictSampleSize <= i {
			break
		}
	}
	if found {
		delete(self.expireTimes, evictKey)
	}
}
