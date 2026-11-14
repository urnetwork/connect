// Measures cluster-decision ownership using real publication and cache paths.
// No association worker, network, native process or wall-clock wait is started.
package connect

import (
	"context"
	"encoding/json"
	"net/netip"
	"os"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
	"weak"
)

// Constructs valid disjoint default-size history blocks and publishes one
// same-name cluster through the actual clustering implementation.
func newBlockActionMemoryFixture(t testing.TB, blockCount, membersPerBlock int) (*RemoteUserNatMultiClient, []netip.Addr) {
	t.Helper()
	settings := DefaultIpAssocSettings()
	if blockCount < 1 || settings.AssociationBlockCount < blockCount || membersPerBlock < 1 || settings.MaxEntityCount < membersPerBlock {
		t.Fatal("measurement fixture exceeds an existing association bound")
	}
	assoc := &IpAssoc{
		ctx: context.Background(), settings: settings,
		lastActive: map[netip.Addr]time.Time{}, baseNames: map[netip.Addr][]string{},
	}
	assoc.clusters.Store(&ipAssocClusters{members: map[netip.Addr][]netip.Addr{}})
	members := make([]netip.Addr, 0, blockCount*membersPerBlock)
	names := []string{"same-cluster.example.test"}
	for blockIndex := range blockCount {
		block := assoc.blockWithLock(time.Unix(1000, 0).Add(time.Duration(blockIndex) * settings.AssociationBlockDuration))
		for memberIndex := range membersPerBlock {
			index := blockIndex*membersPerBlock + memberIndex
			addr := netip.AddrFrom4([4]byte{198, 18, byte(index >> 8), byte(index)})
			ordinal, ok := block.index(addr, settings.MaxEntityCount)
			if !ok {
				t.Fatal("valid association block refused an observed member")
			}
			block.counts[ordinal] = 1
			assoc.baseNames[addr] = names
			members = append(members, addr)
		}
	}
	assoc.dirty = true
	assoc.updateClusters()
	if len(assoc.GetClusterAddrs(members[0])) != len(members) {
		t.Fatal("actual same-name publication did not contain every observed address")
	}
	defaults := DefaultMultiClientSettings()
	multi := &RemoteUserNatMultiClient{
		ipAssoc:              assoc,
		blockActionCache:     newBlockActionCache(defaults.BlockActionDecisionTtl, defaults.BlockActionDecisionMaxCount),
		blockActionCollector: newBlockActionCollector(defaults.BlockActionAggMaxCount, NewNoopLogger()),
	}
	return multi, members
}

// Supplies a real decision path while retaining no IP packet or native owner.
func blockActionMemoryPath(addr netip.Addr) *IpPath {
	return &IpPath{Version: 4, Protocol: IpProtocolTcp, DestinationIp: addr.AsSlice(), DestinationPort: 443}
}

// Adds a new same-name observation to the existing newest valid block.
func addBlockActionMemoryMember(t testing.TB, multi *RemoteUserNatMultiClient, addr netip.Addr) {
	t.Helper()
	assoc := multi.ipAssoc
	block := assoc.blocks[len(assoc.blocks)-1]
	index, ok := block.index(addr, assoc.settings.MaxEntityCount)
	if !ok {
		t.Fatal("adjacent publication fixture exhausted its unchanged block")
	}
	block.counts[index] = 1
	assoc.baseNames[addr] = []string{"same-cluster.example.test"}
	assoc.dirty = true
	assoc.updateClusters()
}

// Counts backing arrays once even when many immutable readers share them.
type blockActionMemoryCensus struct {
	PublishedMembers           int
	CacheEntries               int
	CollectorEntries           int
	UniqueAddressBackings      int
	UniqueAddressCapacityBytes uint64
	CacheLogicalAddressBytes   uint64
}

// This structural reading allocates only temporary metadata; it never keeps an
// address backing alive after returning its scalar census.
func readBlockActionMemoryCensus(multi *RemoteUserNatMultiClient) blockActionMemoryCensus {
	counts := blockActionMemoryCensus{}
	backings := map[*netip.Addr]int{}
	add := func(members []netip.Addr) {
		if len(members) != 0 && backings[&members[0]] < cap(members) {
			backings[&members[0]] = cap(members)
		}
	}
	published := multi.ipAssoc.clusters.Load()
	counts.PublishedMembers = len(published.members)
	for _, members := range published.members {
		add(members)
	}
	multi.blockActionCache.stateLock.Lock()
	counts.CacheEntries = len(multi.blockActionCache.decisions)
	for _, decision := range multi.blockActionCache.decisions {
		add(decision.clusterIps)
		counts.CacheLogicalAddressBytes += uint64(len(decision.clusterIps)) * uint64(unsafe.Sizeof(netip.Addr{}))
	}
	multi.blockActionCache.stateLock.Unlock()
	multi.blockActionCollector.stateLock.Lock()
	counts.CollectorEntries = len(multi.blockActionCollector.agg)
	for _, aggregate := range multi.blockActionCollector.agg {
		add(aggregate.ips)
	}
	multi.blockActionCollector.stateLock.Unlock()
	counts.UniqueAddressBackings = len(backings)
	for _, capacity := range backings {
		counts.UniqueAddressCapacityBytes += uint64(capacity) * uint64(unsafe.Sizeof(netip.Addr{}))
	}
	return counts
}

// Cache misses are actual production decisions; no reference implementation is
// used to assert allocation or retention improvements.
func fillBlockActionMemoryCache(t testing.TB, multi *RemoteUserNatMultiClient, members []netip.Addr, destinations int) {
	t.Helper()
	state := &blockActionState{version: 1}
	for _, member := range members[:destinations] {
		if multi.blockActionDecision(state, nil, false, blockActionMemoryPath(member)) == nil {
			t.Fatal("actual cache builder refused a valid decision")
		}
	}
}

// A merged raw component is bounded by history entities, not the post-name-merge
// graph-node cap. This proves the static maximum without allocating its fanout.
func TestBlockActionDefaultRawClusterHistoryBound(t *testing.T) {
	settings := DefaultIpAssocSettings()
	multi, members := newBlockActionMemoryFixture(t, settings.AssociationBlockCount, settings.MaxEntityCount)
	if len(members) != 16384 || len(members) <= settings.MaxComponentNodeCount {
		t.Fatal("default raw-member geometry differs from the audited bound")
	}
	_, first := multi.ipAssoc.clusterMembers(members[0])
	for _, member := range members {
		_, view := multi.ipAssoc.clusterMembers(member)
		if len(view) != len(members) || &view[0] != &first[0] {
			t.Fatal("one published cluster lost shared immutable member ownership")
		}
	}
	t.Logf("raw_members=%d node_cap=%d address_bytes=%d one_view_logical_bytes=%d", len(members), settings.MaxComponentNodeCount, unsafe.Sizeof(netip.Addr{}), uintptr(len(members))*unsafe.Sizeof(netip.Addr{}))
}

// A future ownership-preserving optimization must retain O(members+destinations)
// address storage. The current correct per-destination clone fails this witness.
func TestBlockActionDecisionClusterStorageIsAmortized(t *testing.T) {
	multi, members := newBlockActionMemoryFixture(t, 1, 512)
	const destinations = 256
	fillBlockActionMemoryCache(t, multi, members, destinations)
	counts := readBlockActionMemoryCensus(multi)
	limit := uint64(2*len(members)+destinations) * uint64(unsafe.Sizeof(netip.Addr{}))
	if counts.CacheEntries != destinations {
		t.Fatal("memory witness did not retain the required distinct destinations")
	}
	if limit < counts.UniqueAddressCapacityBytes {
		t.Fatalf("BLOCK_ACTION_CLUSTER_RETAINED_MULTIPLIER: actual=%d linear_limit=%d roots=%d members=%d destinations=%d", counts.UniqueAddressCapacityBytes, limit, counts.UniqueAddressBackings, len(members), destinations)
	}
}

// Public event mutation must not become a writer of cache or association state.
// Existing per-event sharing across callbacks is deliberately outside this root.
func TestBlockActionEventMutationKeepsImmutableDecision(t *testing.T) {
	multi, members := newBlockActionMemoryFixture(t, 1, 16)
	multi.config.Store(&multiClientConfig{serverNameLookup: &blockActionMemoryLookup{}})
	state := &blockActionState{version: 1}
	decision := multi.blockActionDecision(state, nil, false, blockActionMemoryPath(members[0]))
	beforeIps, beforeHosts := slices.Clone(decision.clusterIps), slices.Clone(decision.clusterHosts)
	beforePublished := slices.Clone(multi.ipAssoc.GetClusterAddrs(members[0]))
	called := 0
	unsub := multi.blockActionCollector.addCallback(func(actions []*BlockAction) {
		called++
		if len(actions) != 1 || len(actions[0].Ips) != len(members) || len(actions[0].Hosts) != 1 {
			t.Error("actual event did not reach the mutating callback with complete members")
			return
		}
		actions[0].Ips[0] = netip.MustParseAddr("203.0.113.250")
		actions[0].Hosts[0] = "mutated.example.test"
	})
	defer unsub()
	multi.blockActionCollector.add(decision, false, false, nil, "", 128)
	multi.blockActionCollector.flush()
	if called != 1 {
		t.Fatal("event mutation witness was not reached")
	}
	if !slices.Equal(decision.clusterIps, beforeIps) || !slices.Equal(decision.clusterHosts, beforeHosts) || !slices.Equal(multi.ipAssoc.GetClusterAddrs(members[0]), beforePublished) {
		t.Fatal("BLOCK_ACTION_EVENT_MUTATED_INTERNAL_VIEW")
	}
}

// One lookup can pause a real miss after it captured its published members.
type blockActionMemoryLookup struct {
	blocked atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (lookup *blockActionMemoryLookup) ServerNames(string) []string {
	if lookup.entered != nil && lookup.blocked.CompareAndSwap(false, true) {
		close(lookup.entered)
		<-lookup.release
	}
	return []string{"same-cluster.example.test"}
}

// An old snapshot reader cannot evict already-cached newer immutable state.
func TestBlockActionStaleLookupKeepsNewerDecision(t *testing.T) {
	cache := newBlockActionCache(time.Minute, 16)
	addr := netip.MustParseAddr("198.18.0.1")
	now := time.Now()
	newer := &blockActionDecision{overridesVersion: 1, clusterVersion: 2, expireTime: now.Add(time.Minute)}
	cache.put(addr, newer)
	if cache.get(addr, 1, 1, false, now) != nil {
		t.Fatal("stale reader received a newer decision under the wrong version")
	}
	cache.stateLock.Lock()
	retained := cache.decisions[addr]
	cache.stateLock.Unlock()
	if retained != newer {
		t.Fatal("BLOCK_ACTION_STALE_LOOKUP_EVICTED_NEWER")
	}
}

// A builder paused outside cache locks cannot repin an older generation after
// a new publication has already installed its current decision.
func TestBlockActionOldMissCannotRepinAfterPublication(t *testing.T) {
	multi, members := newBlockActionMemoryFixture(t, 1, 16)
	lookup := &blockActionMemoryLookup{entered: make(chan struct{}), release: make(chan struct{})}
	multi.config.Store(&multiClientConfig{serverNameLookup: lookup})
	state := &blockActionState{version: 1}
	oldDone := make(chan *blockActionDecision, 1)
	go func() { oldDone <- multi.blockActionDecision(state, nil, false, blockActionMemoryPath(members[0])) }()
	released, joined := false, false
	var older *blockActionDecision
	finish := func() *blockActionDecision {
		if !released {
			close(lookup.release)
			released = true
		}
		if !joined {
			older = <-oldDone
			joined = true
		}
		return older
	}
	defer finish()
	<-lookup.entered
	addBlockActionMemoryMember(t, multi, netip.MustParseAddr("198.18.1.1"))
	newer := multi.blockActionDecision(state, nil, false, blockActionMemoryPath(members[1]))
	older = finish()
	if older == nil || newer == nil || older.clusterVersion == newer.clusterVersion {
		t.Fatal("old/new publication barrier did not exercise distinct actual versions")
	}
	multi.blockActionCache.stateLock.Lock()
	stale := false
	for _, decision := range multi.blockActionCache.decisions {
		stale = stale || decision.clusterVersion != newer.clusterVersion
	}
	multi.blockActionCache.stateLock.Unlock()
	if stale {
		t.Fatal("BLOCK_ACTION_OLD_MISS_REPINNED_RETIRED_VIEW")
	}
}

// Every cache clear owns an epoch: a builder already outside its locks must not
// refill it after pressure or configuration retirement completes.
func TestBlockActionClearedCacheRejectsInflightMiss(t *testing.T) {
	multi, members := newBlockActionMemoryFixture(t, 1, 16)
	lookup := &blockActionMemoryLookup{entered: make(chan struct{}), release: make(chan struct{})}
	multi.config.Store(&multiClientConfig{serverNameLookup: lookup})
	state := &blockActionState{version: 1}
	done := make(chan *blockActionDecision, 1)
	go func() { done <- multi.blockActionDecision(state, nil, false, blockActionMemoryPath(members[0])) }()
	released, joined := false, false
	var result *blockActionDecision
	finish := func() *blockActionDecision {
		if !released {
			close(lookup.release)
			released = true
		}
		if !joined {
			result = <-done
			joined = true
		}
		return result
	}
	defer finish()
	<-lookup.entered
	multi.blockActionCache.clear()
	if finish() == nil {
		t.Fatal("clear witness did not complete its actual pre-clear builder")
	}
	multi.blockActionCache.stateLock.Lock()
	count := len(multi.blockActionCache.decisions)
	multi.blockActionCache.stateLock.Unlock()
	if count != 0 {
		t.Fatal("BLOCK_ACTION_CLEARED_CACHE_WAS_REPINNED")
	}
}

// Retains one actual aggregate without leaving a strong decision reference in
// the measurement's stack across the subsequent pressure/GC observations.
//
//go:noinline
func seedBlockActionMemoryCollector(multi *RemoteUserNatMultiClient, member netip.Addr) weak.Pointer[netip.Addr] {
	decision := multi.blockActionDecision(&blockActionState{version: 1}, nil, false, blockActionMemoryPath(member))
	multi.blockActionCollector.add(decision, false, false, nil, "", 128)
	return weak.Make(&decision.clusterIps[0])
}

// Host heap points supplement the deterministic backing census. Collection is
// test-only; these are neither natural quiet results nor mobile footprint gates.
type blockActionMemoryPoint struct {
	Phase        string
	Census       blockActionMemoryCensus
	HeapAlloc    uint64
	HeapInuse    uint64
	RuntimeTotal uint64
	TotalAlloc   uint64
	GCCycles     uint32
}

func readBlockActionMemoryPoint(phase string, multi *RemoteUserNatMultiClient) blockActionMemoryPoint {
	runtime.GC()
	runtime.GC()
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	return blockActionMemoryPoint{Phase: phase, Census: readBlockActionMemoryCensus(multi), HeapAlloc: memory.HeapAlloc, HeapInuse: memory.HeapInuse, RuntimeTotal: memory.Sys - memory.HeapReleased, TotalAlloc: memory.TotalAlloc, GCCycles: memory.NumGC}
}

// Fresh-process opt-in captures exact backing multiplicity and heap deltas at
// fixed32/64 process inputs without allocating the theoretical1.5-GiB fanout.
func TestBlockActionClusterRetentionMeasurement(t *testing.T) {
	profile, err := strconv.Atoi(os.Getenv("URNETWORK_BLOCK_ACTION_MEMORY_PROFILE_MIB"))
	if err != nil || profile != 32 && profile != 64 {
		t.Skip("opt-in fresh process: URNETWORK_BLOCK_ACTION_MEMORY_PROFILE_MIB=32 or64")
	}
	previousSizing := MemoryBudget()
	previousLimit := debug.SetMemoryLimit(int64(profile) << 20)
	previousGC := debug.SetGCPercent(25)
	SetMemoryBudget(ByteCount(profile) << 20)
	defer func() {
		SetMemoryBudget(previousSizing)
		debug.SetMemoryLimit(previousLimit)
		debug.SetGCPercent(previousGC)
	}()
	multi, members := newBlockActionMemoryFixture(t, 1, 512)
	points := []blockActionMemoryPoint{readBlockActionMemoryPoint("published", multi)}
	fillBlockActionMemoryCache(t, multi, members, 256)
	owner := seedBlockActionMemoryCollector(multi, members[0])
	points = append(points, readBlockActionMemoryPoint("cache_and_epoch_live", multi))
	multi.ipAssoc.ShedMemory()
	points = append(points, readBlockActionMemoryPoint("assoc_shed_unvisited_cache", multi))
	multi.blockActionCache.clear()
	points = append(points, readBlockActionMemoryPoint("cache_clear_epoch_still_live", multi))
	multi.blockActionCollector.flush()
	points = append(points, readBlockActionMemoryPoint("epoch_flushed_no_external_retainer", multi))
	if owner.Value() != nil || points[len(points)-1].Census.UniqueAddressCapacityBytes != 0 {
		t.Fatal("released cache/association/epoch still retains a cluster address owner")
	}
	encoded, err := json.Marshal(struct {
		ProfileMiB       int
		AddressBytes     uintptr
		MemberCount      int
		DestinationCount int
		Points           []blockActionMemoryPoint
	}{ProfileMiB: profile, AddressBytes: unsafe.Sizeof(netip.Addr{}), MemberCount: 512, DestinationCount: 256, Points: points})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("BLOCK_ACTION_MEMORY_MEASUREMENT %s", encoded)
	runtime.KeepAlive(multi)
}

// Measures real destination misses, including the actual clone/sort/walk, with
// fixed cluster membership and no fixture construction in the timed section.
func BenchmarkBlockActionClusterMiss512(b *testing.B) {
	multi, members := newBlockActionMemoryFixture(b, 1, 512)
	state, path := &blockActionState{version: 1}, blockActionMemoryPath(members[0])
	b.ReportAllocs()
	for b.Loop() {
		multi.blockActionCache.clear()
		multi.blockActionDecision(state, nil, false, path)
	}
}

// A warmed hit must retain its decision without additional cluster copying.
func BenchmarkBlockActionClusterHit512(b *testing.B) {
	multi, members := newBlockActionMemoryFixture(b, 1, 512)
	state, path := &blockActionState{version: 1}, blockActionMemoryPath(members[0])
	first := multi.blockActionDecision(state, nil, false, path)
	b.ReportAllocs()
	for b.Loop() {
		if multi.blockActionDecision(state, nil, false, path) != first {
			b.Fatal("warmed hit unexpectedly rebuilt within the bounded benchmark")
		}
	}
}

// A fixed64-destination burst measures the memory multiplication directly.
func BenchmarkBlockActionClusterFanout512x64(b *testing.B) {
	multi, members := newBlockActionMemoryFixture(b, 1, 512)
	state := &blockActionState{version: 1}
	paths := make([]*IpPath, 64)
	for index := range paths {
		paths[index] = blockActionMemoryPath(members[index])
	}
	b.ReportAllocs()
	for b.Loop() {
		multi.blockActionCache.clear()
		for _, path := range paths {
			multi.blockActionDecision(state, nil, false, path)
		}
	}
	b.ReportMetric(float64(readBlockActionMemoryCensus(multi).UniqueAddressCapacityBytes), "retained-address-B")
}
