package connect

import (
	"context"
	"net/netip"
	"slices"
	"testing"
	"time"
)

// The association owner has no worker: real packet activity and the ordinary
// clustering pass publish a shared member slice from deliberately unordered activity.
func testingBlockActionSnapshot(t *testing.T) (*RemoteUserNatMultiClient, *IpAssoc, []netip.Addr) {
	t.Helper()
	members := []netip.Addr{
		netip.MustParseAddr("198.18.0.3"),
		netip.MustParseAddr("198.18.0.1"),
		netip.MustParseAddr("198.18.0.2"),
	}
	assoc := &IpAssoc{
		ctx:        context.Background(),
		settings:   DefaultIpAssocSettings(),
		lastActive: map[netip.Addr]time.Time{},
		baseNames:  map[netip.Addr][]string{},
	}
	assoc.clusters.Store(&ipAssocClusters{members: map[netip.Addr][]netip.Addr{}})
	lookup := &testingServerNameLookup{serverNames: map[string][]string{}}
	for _, member := range members {
		lookup.serverNames[member.String()] = []string{"snapshot.example.test"}
	}
	assoc.SetServerNameLookup(lookup)
	for _, member := range members {
		assoc.AddEgressPacket(&IpPath{Version: 4, Protocol: IpProtocolTcp, DestinationIp: member.AsSlice(), DestinationPort: 443})
	}
	assoc.updateClusters()
	ordered := slices.Clone(members)
	slices.SortFunc(ordered, func(a, b netip.Addr) int { return a.Compare(b) })
	if got := assoc.GetClusterAddrs(members[0]); !slices.Equal(got, ordered) {
		t.Fatalf("fixture did not canonicalize deliberately unordered activity: got=%v want=%v", got, ordered)
	}
	members = ordered
	multi := &RemoteUserNatMultiClient{
		ipAssoc:          assoc,
		blockActionCache: newBlockActionCache(time.Hour, 16),
	}
	return multi, assoc, members
}

// The actual cache builder borrows one already ordered immutable publication
// without writing storage concurrently read by association and cache readers.
func TestBlockActionDecisionPreservesPublishedClusterSnapshot(t *testing.T) {
	multi, assoc, members := testingBlockActionSnapshot(t)
	published := assoc.clusters.Load()
	_, borrowed := assoc.clusterMembers(members[0])
	before := slices.Clone(borrowed)
	state := &blockActionState{version: 1}
	path := &IpPath{Version: 4, Protocol: IpProtocolTcp, DestinationIp: members[0].AsSlice(), DestinationPort: 443}
	decision := multi.blockActionDecision(state, nil, false, path)
	if !slices.Equal(borrowed, before) {
		t.Fatalf("BLOCK_ACTION_CLUSTER_SNAPSHOT_MUTATED: got=%v want=%v", borrowed, before)
	}
	if assoc.clusters.Load() != published || assoc.ClusterVersion() != published.version {
		t.Fatal("decision replaced or versioned the association snapshot")
	}
	ordered := slices.Clone(members)
	slices.SortFunc(ordered, func(a, b netip.Addr) int { return a.Compare(b) })
	if decision == nil || decision.clusterKey != ordered[0] || !slices.Equal(decision.clusterIps, ordered) {
		t.Fatalf("decision lost canonical cluster ordering: got=%+v want=%v", decision, ordered)
	}
	if &decision.clusterIps[0] != &borrowed[0] {
		t.Fatal("decision copied the immutable publication instead of sharing it")
	}
	for _, member := range members {
		_, got := assoc.clusterMembers(member)
		if !slices.Equal(got, before) || &got[0] != &borrowed[0] {
			t.Fatal("shared snapshot membership or backing identity changed")
		}
	}
}

// Distinct destination decisions share one ordered immutable payload; a hot
// cache hit reuses its destination decision without any copy or allocation.
func TestBlockActionDecisionOwnsCacheMissOrderingAndReusesHits(t *testing.T) {
	multi, assoc, members := testingBlockActionSnapshot(t)
	state := &blockActionState{version: 1}
	path := &IpPath{Version: 4, Protocol: IpProtocolTcp, DestinationIp: members[0].AsSlice(), DestinationPort: 443}
	first := multi.blockActionDecision(state, nil, false, path)
	otherPath := &IpPath{Version: 4, Protocol: IpProtocolTcp, DestinationIp: members[1].AsSlice(), DestinationPort: 443}
	second := multi.blockActionDecision(state, nil, false, otherPath)
	if first == nil || second == nil || !slices.Equal(first.clusterIps, second.clusterIps) || first.clusterKey != second.clusterKey {
		t.Fatal("cluster siblings lost equivalent canonical decisions")
	}
	_, borrowed := assoc.clusterMembers(members[0])
	if first == second || &first.clusterIps[0] != &borrowed[0] || &second.clusterIps[0] != &borrowed[0] {
		t.Fatal("destination decisions did not share the immutable publication")
	}
	cacheChanged := false
	allocations := testing.AllocsPerRun(100, func() {
		if multi.blockActionDecision(state, nil, false, path) != first {
			cacheChanged = true
		}
	})
	if cacheChanged || allocations != 0 {
		t.Fatalf("hot cache decision copied or rebuilt: changed=%t allocations=%v", cacheChanged, allocations)
	}
	if !slices.Equal(borrowed, members) {
		t.Fatal("cache activity reordered the shared published cluster")
	}
}

// A cache and an event epoch can retain their ordered state across a genuine
// cluster replacement and pressure shed without mutating retained readers.
func TestBlockActionDecisionRetainsOrderingAcrossPublicationAndShed(t *testing.T) {
	multi, assoc, members := testingBlockActionSnapshot(t)
	state := &blockActionState{version: 1}
	path := &IpPath{Version: 4, Protocol: IpProtocolTcp, DestinationIp: members[0].AsSlice(), DestinationPort: 443}
	first := multi.blockActionDecision(state, nil, false, path)
	before := slices.Clone(first.clusterIps)
	version := assoc.ClusterVersion()
	collector := newBlockActionCollector(16, NewNoopLogger())
	var actions []*BlockAction
	unsub := collector.addCallback(func(emitted []*BlockAction) { actions = emitted })
	defer unsub()
	collector.add(first, false, false, nil, "", 128)

	fourth := netip.MustParseAddr("198.18.0.4")
	assoc.SetServerNameLookup(&testingServerNameLookup{serverNames: map[string][]string{fourth.String(): {"snapshot.example.test"}}})
	assoc.AddEgressPacket(&IpPath{Version: 4, Protocol: IpProtocolTcp, DestinationIp: fourth.AsSlice(), DestinationPort: 443})
	assoc.updateClusters()
	if assoc.ClusterVersion() == version || len(assoc.GetClusterAddrs(members[0])) != 4 {
		t.Fatal("real added activity did not replace the cluster snapshot")
	}
	second := multi.blockActionDecision(state, nil, false, path)
	if second == first || second.clusterVersion != assoc.ClusterVersion() || len(second.clusterIps) != 4 || !slices.Equal(first.clusterIps, before) {
		t.Fatal("cluster publication invalidated retained decision ownership")
	}
	assoc.ShedMemory()
	singleton := multi.blockActionDecision(state, nil, false, path)
	if singleton == second || !slices.Equal(singleton.clusterIps, []netip.Addr{members[0]}) || singleton.clusterKey != members[0] || !slices.Equal(first.clusterIps, before) {
		t.Fatal("pressure shed corrupted retained or fallback decision ordering")
	}
	collector.flush()
	if len(actions) != 1 || !slices.Equal(actions[0].Ips, before) || actions[0].PacketCount != 1 || actions[0].ByteCount != 128 || len(collector.agg) != 0 {
		t.Fatal("retained block-action epoch lost membership, accounting or cleanup")
	}
}
