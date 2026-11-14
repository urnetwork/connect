// Includes cluster publication, destination decisions and public event output
// in one measured pass. Inputs use real activity in a fixed ascending/permuted order.
package connect

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"testing"
	"time"
)

// Each alternative owns one prebuilt observation set. Switching one address
// forces a genuine publication without constructing inputs in the timed pass.
type blockActionPublicationFixture struct {
	multi       *RemoteUserNatMultiClient
	inputs      [2]*IpAssoc
	paths       []*IpPath
	members     int
	hostCount   int
	unsubscribe func()
	events      int
	packets     int
}

// Phase1 differs only in its last address; every measured consumer remains on
// the same destination in both phases, so version changes cause actual misses.
func blockActionPublicationAddr(index, count, phase int) netip.Addr {
	if index == count-1 && phase == 1 {
		return netip.AddrFrom4([4]byte{198, 19, byte(index >> 8), byte(index)})
	}
	return netip.AddrFrom4([4]byte{198, 18, byte(index >> 8), byte(index)})
}

// This owner has no background worker or shedder registration. Activity still
// traverses AddEgressPacket and the real same-name lookup/association builder.
func newBlockActionPublicationInput(t testing.TB, count, stride, offset, phase int) *IpAssoc {
	t.Helper()
	settings := DefaultIpAssocSettings()
	if count < 2 || settings.MaxEntityCount < count || stride <= 0 || offset < 0 {
		t.Fatal("publication input exceeds the unchanged association geometry")
	}
	assoc := &IpAssoc{
		ctx: context.Background(), settings: settings,
		lastActive: map[netip.Addr]time.Time{}, baseNames: map[netip.Addr][]string{},
	}
	assoc.clusters.Store(&ipAssocClusters{members: map[netip.Addr][]netip.Addr{}})
	assoc.SetServerNameLookup(&blockActionMemoryLookup{})
	seen := make([]bool, count)
	for ordinal := range count {
		index := (ordinal*stride + offset) % count
		if seen[index] {
			t.Fatal("activity permutation repeated an address")
		}
		seen[index] = true
		assoc.AddEgressPacket(blockActionMemoryPath(blockActionPublicationAddr(index, count, phase)))
	}
	if len(assoc.blocks) != 1 || len(assoc.blocks[0].addrs) != count {
		t.Fatal("bounded activity construction did not create one complete input block")
	}
	return assoc
}

// Publishes one of two observed sets through one real owner and retains only
// scalar receipts from event callbacks. No emitted payload is kept by the fixture.
func newBlockActionPublicationFixture(t testing.TB, count, stride, offset int, uniqueHosts bool) *blockActionPublicationFixture {
	t.Helper()
	defaults := DefaultMultiClientSettings()
	assoc := &IpAssoc{
		ctx: context.Background(), settings: DefaultIpAssocSettings(),
		lastActive: map[netip.Addr]time.Time{}, baseNames: map[netip.Addr][]string{},
	}
	assoc.clusters.Store(&ipAssocClusters{members: map[netip.Addr][]netip.Addr{}})
	fixture := &blockActionPublicationFixture{
		multi: &RemoteUserNatMultiClient{
			ipAssoc:              assoc,
			blockActionCache:     newBlockActionCache(defaults.BlockActionDecisionTtl, defaults.BlockActionDecisionMaxCount),
			blockActionCollector: newBlockActionCollector(defaults.BlockActionAggMaxCount, NewNoopLogger()),
		},
		members: count,
	}
	for phase := range fixture.inputs {
		fixture.inputs[phase] = newBlockActionPublicationInput(t, count, stride, offset, phase)
	}
	lookup := &blockActionPublicationLookup{names: map[string][]string{}}
	commonNames := []string{"same-cluster.example.test"}
	fixture.hostCount = 1
	if uniqueHosts {
		fixture.hostCount = count
	}
	for index := range count {
		names := commonNames
		if uniqueHosts {
			names = []string{fmt.Sprintf("member-%d.same-cluster.example.test", index)}
		}
		for phase := range fixture.inputs {
			lookup.names[blockActionPublicationAddr(index, count, phase).String()] = names
		}
	}
	fixture.multi.config.Store(&multiClientConfig{serverNameLookup: lookup})
	fixture.paths = make([]*IpPath, 64)
	for index := range fixture.paths {
		fixture.paths[index] = blockActionMemoryPath(blockActionPublicationAddr(index, count, 0))
	}
	fixture.unsubscribe = fixture.multi.blockActionCollector.addCallback(func(actions []*BlockAction) {
		if len(actions) != 1 || len(actions[0].Ips) != fixture.members || len(actions[0].Hosts) != fixture.hostCount {
			t.Error("publication pass did not emit one complete cluster action")
			return
		}
		fixture.events++
		fixture.packets += actions[0].PacketCount
	})
	t.Cleanup(func() { fixture.release() })
	return fixture
}

// Lookup shapes are prebuilt and immutable. Common-host rows expose one host
// per action; unique-host rows expose512, including the proposed event-copy cost.
type blockActionPublicationLookup struct {
	names map[string][]string
}

func (lookup *blockActionPublicationLookup) ServerNames(ip string) []string {
	return lookup.names[ip]
}

// Cleanup is outside benchmark timing. Bare owners have no cancel/worker to
// join; release every cache, publication, observation map and callback closure.
func (fixture *blockActionPublicationFixture) release() {
	fixture.multi.blockActionCache.clear()
	fixture.multi.blockActionCollector.flush()
	fixture.multi.ipAssoc.ShedMemory()
	fixture.unsubscribe()
	fixture.unsubscribe = nil
	for _, input := range fixture.inputs {
		input.ShedMemory()
	}
	fixture.inputs = [2]*IpAssoc{}
	fixture.paths = nil
}

// Every timed call changes membership and includes the real clustering pass,
// monotonic cache retirement/misses, collector work and one event flush attempt.
func (fixture *blockActionPublicationFixture) pass(t testing.TB, phase, consumers int) {
	t.Helper()
	if phase < 0 || len(fixture.inputs) <= phase || consumers < 0 || len(fixture.paths) < consumers {
		t.Fatal("invalid fixed publication pass")
	}
	assoc, input := fixture.multi.ipAssoc, fixture.inputs[phase]
	assoc.stateLock.Lock()
	assoc.blocks, assoc.baseNames, assoc.lastActive = input.blocks, input.baseNames, input.lastActive
	assoc.dirty = true
	assoc.stateLock.Unlock()
	previousVersion := assoc.ClusterVersion()
	assoc.updateClusters()
	if assoc.ClusterVersion() == previousVersion {
		t.Fatal("timed work did not produce a genuine changed publication")
	}
	state := &blockActionState{version: 1}
	beforeEvents, beforePackets := fixture.events, fixture.packets
	for _, path := range fixture.paths[:consumers] {
		decision := fixture.multi.blockActionDecision(state, nil, false, path)
		if decision == nil || len(decision.clusterIps) != fixture.members {
			t.Fatal("timed consumer did not build a complete current decision")
		}
		fixture.multi.blockActionCollector.add(decision, false, false, nil, "", 128)
	}
	fixture.multi.blockActionCollector.flush()
	expectedEvents := 0
	if consumers != 0 {
		expectedEvents = 1
	}
	if fixture.events-beforeEvents != expectedEvents || fixture.packets-beforePackets != consumers {
		t.Fatal("timed event did not preserve exact consumer accounting")
	}
}

// Both activity orders cover the same members, and publication must not sort
// or overwrite the observation backing that the next clustering pass reuses.
func TestBlockActionPublicationPermutationPreservesObservedInput(t *testing.T) {
	for _, shape := range []struct {
		stride, offset int
		uniqueHosts    bool
	}{{stride: 1}, {stride: 73, offset: 17}, {stride: 73, offset: 17, uniqueHosts: true}} {
		fixture := newBlockActionPublicationFixture(t, 512, shape.stride, shape.offset, shape.uniqueHosts)
		input := fixture.inputs[0].blocks[0].addrs
		before := slices.Clone(input)
		for ordinal, addr := range input {
			index := (ordinal*shape.stride + shape.offset) % len(input)
			if addr != blockActionPublicationAddr(index, len(input), 0) {
				t.Fatal("actual activity order differs from the declared coprime permutation")
			}
		}
		fixture.pass(t, 0, 0)
		if !slices.Equal(input, before) {
			t.Fatal("publication reordered reusable observed-address backing")
		}
		published := slices.Clone(fixture.multi.ipAssoc.GetClusterAddrs(blockActionPublicationAddr(0, 512, 0)))
		slices.SortFunc(published, func(a, b netip.Addr) int { return a.Compare(b) })
		if len(published) != 512 {
			t.Fatal("permuted activity lost published members")
		}
		for index, addr := range published {
			if addr != blockActionPublicationAddr(index, len(published), 0) {
				t.Fatal("activity permutation changed membership")
			}
		}
	}
}

// One op is one512-member changed publication plus N distinct destination
// decisions and an event epoch; ns/op and B/op are not per-consumer units.
func benchmarkBlockActionPublication(b *testing.B, stride, offset, consumers int, uniqueHosts bool) {
	fixture := newBlockActionPublicationFixture(b, 512, stride, offset, uniqueHosts)
	// Warm reusable aggregate scratch once. Every measured iteration below
	// still publishes a different cluster and includes its complete sort cost.
	fixture.pass(b, 0, consumers)
	phase := 1
	b.ReportAllocs()
	for b.Loop() {
		fixture.pass(b, phase, consumers)
		phase ^= 1
	}
	b.ReportMetric(1, "publications/op")
	b.ReportMetric(float64(consumers), "consumers/op")
	b.ReportMetric(float64(fixture.hostCount), "cluster-hosts")
	if consumers == 0 {
		b.ReportMetric(0, "events/op")
	} else {
		b.ReportMetric(1, "events/op")
	}
	b.ReportMetric(float64(readBlockActionMemoryCensus(fixture.multi).UniqueAddressCapacityBytes), "retained-address-B")
}

// Captures publication tax when no destination consumes the new view.
func BenchmarkBlockActionPublicationAscendingN0(b *testing.B) {
	benchmarkBlockActionPublication(b, 1, 0, 0, false)
}

// Includes one cache miss and its emitted action per publication.
func BenchmarkBlockActionPublicationAscendingN1(b *testing.B) {
	benchmarkBlockActionPublication(b, 1, 0, 1, false)
}

// Includes64 distinct misses aggregated into one public cluster action.
func BenchmarkBlockActionPublicationAscendingN64(b *testing.B) {
	benchmarkBlockActionPublication(b, 1, 0, 64, false)
}

// Charges real sorting work on a non-ascending full-cycle activity order.
func BenchmarkBlockActionPublicationPermutedN0(b *testing.B) {
	benchmarkBlockActionPublication(b, 73, 17, 0, false)
}

// Measures one consumer against the same deterministic unordered input.
func BenchmarkBlockActionPublicationPermutedN1(b *testing.B) {
	benchmarkBlockActionPublication(b, 73, 17, 1, false)
}

// Measures amortization over64 consumers without omitting publication cost.
func BenchmarkBlockActionPublicationPermutedN64(b *testing.B) {
	benchmarkBlockActionPublication(b, 73, 17, 64, false)
}

// Keeps unique-host lookup/publication overhead visible with no consumers.
func BenchmarkBlockActionPublicationUniqueHostsN0(b *testing.B) {
	benchmarkBlockActionPublication(b, 73, 17, 0, true)
}

// Charges the full512-host action once for one destination consumer.
func BenchmarkBlockActionPublicationUniqueHostsN1(b *testing.B) {
	benchmarkBlockActionPublication(b, 73, 17, 1, true)
}

// Exposes pre-existing host fanout as well as address-copy amortization.
func BenchmarkBlockActionPublicationUniqueHostsN64(b *testing.B) {
	benchmarkBlockActionPublication(b, 73, 17, 64, true)
}
