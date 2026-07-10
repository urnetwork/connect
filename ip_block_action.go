package connect

import (
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// block action overrides let the user override the multi client's default egress
// decisions - both the security decision (block) and the local routing decision.
// an override matches a destination by host, and the match extends to the
// destination's `IpAssoc` cluster, so blocking a single host blocks the ips that
// activity-associate with it (cdns, apis, etc).
//
// every egress decision is surfaced as a `BlockAction`, deduplicated per
// destination per event epoch, so listeners see all routing decisions without
// per-packet dispatch.

type BlockOverride struct {
	Block bool
}

type RouteOverride struct {
	Local bool
}

type BlockActionOverride struct {
	// set by the caller. must be unique per override
	OverrideId Id
	// traffic matches when it overlaps any of the hosts.
	// a host can be
	// 1. an exact hostname
	// 2. a wildcard hostname: *.H matches subdomains of H, **.H matches H and subdomains
	// 3. an ipv4 or ipv6 subnet
	// 4. an ipv4 or ipv6
	Hosts         []string
	BlockOverride *BlockOverride
	RouteOverride *RouteOverride
}

// a routing decision surfaced to listeners, aggregated per cluster per epoch.
// decisions are made on the cluster level (see `IpAssoc`), so one action carries
// all the ips and hosts in the destination's cluster
type BlockAction struct {
	// time of the first decision in the epoch
	Time time.Time
	// all ips in the cluster
	Ips []netip.Addr
	// all server names observed for the cluster ips
	Hosts []string
	Block bool
	Local bool
	// set when an override determined the decision
	BlockOverrideId *Id
	RouteOverrideId *Id
	PacketCount     int
	ByteCount       ByteCount
}

type BlockActionFunction func(blockActions []*BlockAction)

// cumulative packet counts by route. read with `RemoteUserNatMultiClient.PacketStats`.
// block counts split by direction: egress is traffic blocked on the way out
// (the send path, or the provider's return into the tunnel), ingress is
// traffic blocked on the way in (the provider's receive from the tunnel)
type PacketStats struct {
	RemoteEgressPacketCount  int64
	RemoteEgressByteCount    ByteCount
	RemoteIngressPacketCount int64
	RemoteIngressByteCount   ByteCount
	LocalEgressPacketCount   int64
	LocalEgressByteCount     ByteCount
	LocalIngressPacketCount  int64
	LocalIngressByteCount    ByteCount
	BlockEgressPacketCount   int64
	BlockEgressByteCount     ByteCount
	BlockIngressPacketCount  int64
	BlockIngressByteCount    ByteCount
}

type PacketStatsFunction func(packetStats *PacketStats)

type packetStatsCounters struct {
	remoteEgressPacketCount  atomic.Int64
	remoteEgressByteCount    atomic.Int64
	remoteIngressPacketCount atomic.Int64
	remoteIngressByteCount   atomic.Int64
	localEgressPacketCount   atomic.Int64
	localEgressByteCount     atomic.Int64
	localIngressPacketCount  atomic.Int64
	localIngressByteCount    atomic.Int64
	blockEgressPacketCount   atomic.Int64
	blockEgressByteCount     atomic.Int64
	blockIngressPacketCount  atomic.Int64
	blockIngressByteCount    atomic.Int64
}

func (self *packetStatsCounters) snapshot() *PacketStats {
	return &PacketStats{
		RemoteEgressPacketCount:  self.remoteEgressPacketCount.Load(),
		RemoteEgressByteCount:    ByteCount(self.remoteEgressByteCount.Load()),
		RemoteIngressPacketCount: self.remoteIngressPacketCount.Load(),
		RemoteIngressByteCount:   ByteCount(self.remoteIngressByteCount.Load()),
		LocalEgressPacketCount:   self.localEgressPacketCount.Load(),
		LocalEgressByteCount:     ByteCount(self.localEgressByteCount.Load()),
		LocalIngressPacketCount:  self.localIngressPacketCount.Load(),
		LocalIngressByteCount:    ByteCount(self.localIngressByteCount.Load()),
		BlockEgressPacketCount:   self.blockEgressPacketCount.Load(),
		BlockEgressByteCount:     ByteCount(self.blockEgressByteCount.Load()),
		BlockIngressPacketCount:  self.blockIngressPacketCount.Load(),
		BlockIngressByteCount:    ByteCount(self.blockIngressByteCount.Load()),
	}
}

// compiled override rules
type blockActionMatcher struct {
	exactHosts    map[string][]*BlockActionOverride
	wildcardHosts []*blockActionWildcardHost
	addrs         map[netip.Addr][]*BlockActionOverride
	prefixes      []*blockActionPrefix
}

type blockActionWildcardHost struct {
	// ".h" suffix match
	dotSuffix string
	// **.h also matches h itself
	base     string
	override *BlockActionOverride
}

type blockActionPrefix struct {
	prefix   netip.Prefix
	override *BlockActionOverride
}

// returns nil when there are no overrides
func newBlockActionMatcher(overrides []*BlockActionOverride) *blockActionMatcher {
	if len(overrides) == 0 {
		return nil
	}
	matcher := &blockActionMatcher{
		exactHosts: map[string][]*BlockActionOverride{},
		addrs:      map[netip.Addr][]*BlockActionOverride{},
	}
	for _, override := range overrides {
		for _, host := range override.Hosts {
			host = strings.ToLower(strings.TrimSpace(host))
			if host == "" {
				continue
			}
			if after, ok := strings.CutPrefix(host, "**."); ok {
				matcher.wildcardHosts = append(matcher.wildcardHosts, &blockActionWildcardHost{
					dotSuffix: "." + after,
					base:      after,
					override:  override,
				})
			} else if after, ok := strings.CutPrefix(host, "*."); ok {
				matcher.wildcardHosts = append(matcher.wildcardHosts, &blockActionWildcardHost{
					dotSuffix: "." + after,
					override:  override,
				})
			} else if prefix, err := netip.ParsePrefix(host); err == nil {
				matcher.prefixes = append(matcher.prefixes, &blockActionPrefix{
					prefix:   prefix.Masked(),
					override: override,
				})
			} else if addr, err := netip.ParseAddr(host); err == nil {
				addr = addr.Unmap()
				matcher.addrs[addr] = append(matcher.addrs[addr], override)
			} else {
				matcher.exactHosts[host] = append(matcher.exactHosts[host], override)
			}
		}
	}
	return matcher
}

// the merged effect of the matched overrides
type blockActionMatch struct {
	blockOverride   *BlockOverride
	blockOverrideId Id
	routeOverride   *RouteOverride
	routeOverrideId Id
}

func (self *blockActionMatch) any() bool {
	return self.blockOverride != nil || self.routeOverride != nil
}

// merge is order-independent: block=true wins over block=false,
// and local=true wins over local=false
func (self *blockActionMatch) merge(override *BlockActionOverride) {
	if override.BlockOverride != nil {
		if self.blockOverride == nil || !self.blockOverride.Block && override.BlockOverride.Block {
			self.blockOverride = override.BlockOverride
			self.blockOverrideId = override.OverrideId
		}
	}
	if override.RouteOverride != nil {
		if self.routeOverride == nil || !self.routeOverride.Local && override.RouteOverride.Local {
			self.routeOverride = override.RouteOverride
			self.routeOverrideId = override.OverrideId
		}
	}
}

// blockActionApply combines the security policy result with the matched overrides
// into the final egress decision. the rules are
//  1. an incident (martian/malformed) is always blocked and not overridable
//  2. the block override takes precedence over the security decision,
//     and the route override over the default route (local when the security
//     result is drop and the local security bypass is on, else egress)
//  3. drop-classified traffic never egresses to a provider.
//     un-blocking it routes it locally
func blockActionApply(r SecurityPolicyResult, localSecurityBypass bool, match *blockActionMatch) (block bool, local bool) {
	switch r {
	case SecurityPolicyResultAllow:
	case SecurityPolicyResultDrop:
		if localSecurityBypass {
			local = true
		} else {
			block = true
		}
	default:
		// incident. always blocked, not overridable
		block = true
		return
	}

	if match != nil {
		if match.blockOverride != nil {
			block = match.blockOverride.Block
		}
		if match.routeOverride != nil {
			local = match.routeOverride.Local
		}
		if r == SecurityPolicyResultDrop && !block {
			// drop-classified traffic never egresses to a provider
			local = true
		}
		if block {
			local = false
		}
	}
	return
}

func (self *blockActionMatcher) matchAddr(match *blockActionMatch, addr netip.Addr, serverNames []string) {
	for _, override := range self.addrs[addr] {
		match.merge(override)
	}
	for _, blockActionPrefix := range self.prefixes {
		if blockActionPrefix.prefix.Contains(addr) {
			match.merge(blockActionPrefix.override)
		}
	}
	for _, serverName := range serverNames {
		serverName = strings.ToLower(serverName)
		for _, override := range self.exactHosts[serverName] {
			match.merge(override)
		}
		for _, wildcardHost := range self.wildcardHosts {
			if strings.HasSuffix(serverName, wildcardHost.dotSuffix) || serverName == wildcardHost.base {
				match.merge(wildcardHost.override)
			}
		}
	}
}

// immutable snapshot swapped on `SetBlockActionOverrides`
type blockActionState struct {
	version uint64
	// nil when no overrides are set
	matcher *blockActionMatcher
}

// a cached decision for a destination.
// valid while the overrides and cluster versions are unchanged, up to a ttl
// (server names for the destination can drift as dns is observed)
type blockActionDecision struct {
	match            *blockActionMatch
	overridesVersion uint64
	clusterVersion   uint64
	expireTime       time.Time

	// the destination's cluster. a destination with no cluster is
	// a cluster of itself
	clusterKey   netip.Addr
	clusterIps   []netip.Addr
	clusterHosts []string
}

type blockActionCache struct {
	ttl      time.Duration
	maxCount int

	stateLock sync.Mutex
	decisions map[netip.Addr]*blockActionDecision
}

func newBlockActionCache(ttl time.Duration, maxCount int) *blockActionCache {
	return &blockActionCache{
		ttl:       ttl,
		maxCount:  maxCount,
		decisions: map[netip.Addr]*blockActionDecision{},
	}
}

func (self *blockActionCache) get(addr netip.Addr, overridesVersion uint64, clusterVersion uint64, now time.Time) *blockActionDecision {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	decision, ok := self.decisions[addr]
	if !ok {
		return nil
	}
	if decision.overridesVersion != overridesVersion || decision.clusterVersion != clusterVersion || decision.expireTime.Before(now) {
		delete(self.decisions, addr)
		return nil
	}
	return decision
}

func (self *blockActionCache) put(addr netip.Addr, decision *blockActionDecision) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.maxCount <= len(self.decisions) {
		clear(self.decisions)
	}
	self.decisions[addr] = decision
}

func (self *blockActionCache) clear() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	clear(self.decisions)
}

// defaultRemoteDohIgnoreAddrs is the built-in ignore baseline: the hard coded
// remote doh resolver ips from `DefaultDnsResolverSettings`. these are always
// excluded from the override and association logic, independent of the host's
// `SetBlockActionIgnoreHosts` wiring — user override rules must never capture
// dns to the well known resolvers, which the os or browser may query directly.
var defaultRemoteDohIgnoreAddrs = sync.OnceValue(func() map[netip.Addr]bool {
	addrs := map[netip.Addr]bool{}
	resolver := DefaultDnsResolverSettings()
	dohUrlLists := [][]string{
		resolver.RemoteDohUrlsIpv4,
		resolver.RemoteDohUrlsIpv6,
	}
	for _, dohUrls := range dohUrlLists {
		for _, dohUrl := range dohUrls {
			u, err := url.Parse(strings.TrimSpace(dohUrl))
			if err != nil {
				continue
			}
			if addr, err := netip.ParseAddr(u.Hostname()); err == nil {
				// unmapped, matching the `ipAssocAddr` normalization
				addrs[addr.Unmap()] = true
			}
		}
	}
	return addrs
})

// destinations excluded from the override and association logic,
// e.g. the dns resolver endpoints. host values use the same forms as
// `BlockActionOverride.Hosts`. matching is on the destination itself
// (ip and observed server names), not its cluster, since ignored
// destinations do not associate.
// immutable snapshot swapped on `SetBlockActionIgnoreHosts`
type blockActionIgnoreState struct {
	version uint64
	// nil when no ignore host values are set
	matcher *blockActionMatcher
}

// caches the per-destination ignore result.
// valid while the ignore version is unchanged, up to a ttl
// (server names for the destination can drift as dns is observed)
type blockActionIgnoreCache struct {
	ttl      time.Duration
	maxCount int

	stateLock sync.Mutex
	entries   map[netip.Addr]*blockActionIgnoreEntry
}

type blockActionIgnoreEntry struct {
	ignored    bool
	version    uint64
	expireTime time.Time
}

func newBlockActionIgnoreCache(ttl time.Duration, maxCount int) *blockActionIgnoreCache {
	return &blockActionIgnoreCache{
		ttl:      ttl,
		maxCount: maxCount,
		entries:  map[netip.Addr]*blockActionIgnoreEntry{},
	}
}

func (self *blockActionIgnoreCache) get(addr netip.Addr, version uint64, now time.Time) *blockActionIgnoreEntry {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	entry, ok := self.entries[addr]
	if !ok {
		return nil
	}
	if entry.version != version || entry.expireTime.Before(now) {
		delete(self.entries, addr)
		return nil
	}
	return entry
}

func (self *blockActionIgnoreCache) put(addr netip.Addr, entry *blockActionIgnoreEntry) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.maxCount <= len(self.entries) {
		clear(self.entries)
	}
	self.entries[addr] = entry
}

func (self *blockActionIgnoreCache) clear() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	clear(self.entries)
}

// aggregates decisions per destination per epoch
type blockActionCollector struct {
	maxCount int

	callbacks     *CallbackList[BlockActionFunction]
	callbackCount atomic.Int64

	stateLock sync.Mutex
	agg       map[blockActionKey]*blockActionAgg
}

type blockActionKey struct {
	// the canonical cluster member (the lowest ip)
	clusterKey netip.Addr
	block      bool
	local      bool
	// zero when no override applied
	blockOverrideId Id
	routeOverrideId Id
}

type blockActionAgg struct {
	firstTime   time.Time
	ips         []netip.Addr
	hosts       []string
	packetCount int
	byteCount   ByteCount
}

func newBlockActionCollector(maxCount int) *blockActionCollector {
	return &blockActionCollector{
		maxCount:  maxCount,
		callbacks: NewCallbackList[BlockActionFunction](),
		agg:       map[blockActionKey]*blockActionAgg{},
	}
}

func (self *blockActionCollector) hasCallbacks() bool {
	return 0 < self.callbackCount.Load()
}

func (self *blockActionCollector) addCallback(callback BlockActionFunction) func() {
	callbackId := self.callbacks.Add(callback)
	self.callbackCount.Add(1)
	return func() {
		self.callbacks.Remove(callbackId)
		self.callbackCount.Add(-1)
	}
}

func (self *blockActionCollector) add(
	decision *blockActionDecision,
	block bool,
	local bool,
	match *blockActionMatch,
	byteCount ByteCount,
) {
	key := blockActionKey{
		clusterKey: decision.clusterKey,
		block:      block,
		local:      local,
	}
	if match != nil {
		if match.blockOverride != nil {
			key.blockOverrideId = match.blockOverrideId
		}
		if match.routeOverride != nil {
			key.routeOverrideId = match.routeOverrideId
		}
	}

	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	agg, ok := self.agg[key]
	if !ok {
		if self.maxCount <= len(self.agg) {
			// at capacity. drop the aggregation for this epoch
			return
		}
		agg = &blockActionAgg{
			firstTime: time.Now(),
			ips:       decision.clusterIps,
			hosts:     decision.clusterHosts,
		}
		self.agg[key] = agg
	}
	agg.packetCount += 1
	agg.byteCount += byteCount
}

func (self *blockActionCollector) flush() {
	var agg map[blockActionKey]*blockActionAgg
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if len(self.agg) == 0 {
			return
		}
		agg = self.agg
		self.agg = map[blockActionKey]*blockActionAgg{}
	}()
	if len(agg) == 0 {
		return
	}

	blockActions := make([]*BlockAction, 0, len(agg))
	for key, keyAgg := range agg {
		blockAction := &BlockAction{
			Time:        keyAgg.firstTime,
			Ips:         keyAgg.ips,
			Hosts:       keyAgg.hosts,
			Block:       key.block,
			Local:       key.local,
			PacketCount: keyAgg.packetCount,
			ByteCount:   keyAgg.byteCount,
		}
		if key.blockOverrideId != (Id{}) {
			blockOverrideId := key.blockOverrideId
			blockAction.BlockOverrideId = &blockOverrideId
		}
		if key.routeOverrideId != (Id{}) {
			routeOverrideId := key.routeOverrideId
			blockAction.RouteOverrideId = &routeOverrideId
		}
		blockActions = append(blockActions, blockAction)
	}
	sort.Slice(blockActions, func(i int, j int) bool {
		return blockActions[i].Time.Before(blockActions[j].Time)
	})

	for _, callback := range self.callbacks.Get() {
		HandleError(func() {
			callback(blockActions)
		})
	}
}
