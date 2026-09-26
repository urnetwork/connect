package connect

import (
	"context"
	"net/netip"
	"reflect"
	"sync"
	"testing"
	"time"
	"unsafe"
)

// One site's addresses collapse to one node, even though the next epoch still
// needs all of the raw observations. No goroutine or wall clock drives this test.
func testingIpAssocScratchMatrix(entityCount, blockCount int, sharedName bool) *IpAssoc {
	assoc := &IpAssoc{
		ctx: context.Background(), settings: DefaultIpAssocSettings(),
		lastActive: map[netip.Addr]time.Time{}, baseNames: map[netip.Addr][]string{},
	}
	assoc.clusters.Store(&ipAssocClusters{members: map[netip.Addr][]netip.Addr{}})
	for range blockCount {
		block := newIpAssocBlock(time.Time{})
		for i := range entityCount {
			addr := netip.AddrFrom4([4]byte{192, 0, byte(i / 256), byte(i % 256)})
			index, _ := block.index(addr, entityCount)
			block.counts[index] = 100
			if sharedName {
				assoc.baseNames[addr] = []string{"same-site.test"}
			}
		}
		for i := range entityCount {
			for j := i + 1; j < entityCount; j++ {
				block.coCounts[ipAssocPackIndexPair(uint16(i), uint16(j))] = 100
			}
		}
		assoc.blocks = append(assoc.blocks, block)
	}
	assoc.dirty = true
	return assoc
}

func TestIpAssocScratchRetainsRawInputAcrossNameMerge(t *testing.T) {
	const entities = 128
	const rawPairs = entities * (entities - 1) / 2
	assoc := testingIpAssocScratchMatrix(entities, 1, true)
	for pass := range 3 {
		assoc.dirty = true
		assoc.updateClusters()
		if len(assoc.scratch.pairs) != 0 {
			t.Fatal("same-site observations must collapse to no inter-node pairs")
		}
		if cap(assoc.scratch.pairs) < rawPairs {
			t.Fatalf("pass %d shrank %d raw pairs to compact output capacity %d", pass, rawPairs, cap(assoc.scratch.pairs))
		}
		if got := len(assoc.GetClusterAddrs(assoc.blocks[0].addrs[0])); got != entities {
			t.Fatalf("cluster members = %d, want %d", got, entities)
		}
	}
}

func TestIpAssocScratchRetainsRawInputAcrossHistoryCoalescing(t *testing.T) {
	const entities, blocks = 40, 8
	const rawPairs = entities * (entities - 1) / 2 * blocks
	assoc := testingIpAssocScratchMatrix(entities, blocks, false)
	assoc.updateClusters()
	if got := len(assoc.scratch.pairs); got != rawPairs/blocks {
		t.Fatalf("coalesced pairs = %d, want %d", got, rawPairs/blocks)
	}
	if cap(assoc.scratch.pairs) < rawPairs {
		t.Fatalf("history still needs %d raw pairs; retained only %d", rawPairs, cap(assoc.scratch.pairs))
	}
}

func TestIpAssocScratchShrinksAfterTrueInputDrop(t *testing.T) {
	assoc := testingIpAssocScratchMatrix(128, 1, true)
	assoc.updateClusters()
	largeCapacity := cap(assoc.scratch.pairs)
	small := testingIpAssocScratchMatrix(4, 1, true)
	assoc.blocks, assoc.baseNames, assoc.dirty = small.blocks, small.baseNames, true
	assoc.updateClusters()
	if got := cap(assoc.scratch.pairs); got != 6 || got >= largeCapacity {
		t.Fatalf("true raw-input drop should shrink pairs to 6; got %d (large %d)", got, largeCapacity)
	}
}

func TestIpAssocScratchShrinkHysteresisUsesRawDemand(t *testing.T) {
	assoc := testingIpAssocScratchMatrix(128, 1, true)
	const highCapacity, quarter = 16384, 4096
	for key := range assoc.blocks[0].coCounts {
		if len(assoc.blocks[0].coCounts) == quarter {
			break
		}
		delete(assoc.blocks[0].coCounts, key)
	}
	assoc.scratch.pairs = make([]ipAssocAggPair, 0, highCapacity)
	assoc.updateClusters()
	if got := cap(assoc.scratch.pairs); got != highCapacity {
		t.Fatalf("raw fill at one quarter must retain hysteresis capacity: %d", got)
	}
	for key := range assoc.blocks[0].coCounts {
		delete(assoc.blocks[0].coCounts, key)
		break
	}
	assoc.dirty = true
	assoc.updateClusters()
	if got := cap(assoc.scratch.pairs); got != quarter-1 {
		t.Fatalf("raw fill below one quarter must release excess capacity: %d", got)
	}
}

func TestIpAssocScratchShedReleasesWorkspaceAndRebuilds(t *testing.T) {
	assoc := testingIpAssocScratchMatrix(128, 1, true)
	assoc.updateClusters()
	assoc.ShedMemory()
	if !reflect.DeepEqual(assoc.scratch, ipAssocScratch{}) {
		t.Fatal("memory pressure left aggregation/clustering workspace reachable")
	}
	fresh := testingIpAssocScratchMatrix(4, 1, true)
	assoc.blocks, assoc.baseNames, assoc.dirty = fresh.blocks, fresh.baseNames, true
	assoc.updateClusters()
	if got := len(assoc.GetClusterAddrs(assoc.blocks[0].addrs[0])); got != 4 {
		t.Fatalf("cluster did not rebuild after shedding: %d members", got)
	}
}

type ipAssocPausedContext struct {
	context.Context
	once    sync.Once
	entered chan struct{}
	resume  chan struct{}
}

func (c *ipAssocPausedContext) Err() error {
	c.once.Do(func() { close(c.entered); <-c.resume })
	return c.Context.Err()
}

func TestIpAssocScratchShedDuringPassCannotRetainOrPublish(t *testing.T) {
	assoc := testingIpAssocScratchMatrix(128, 1, true)
	ctx := &ipAssocPausedContext{Context: context.Background(), entered: make(chan struct{}), resume: make(chan struct{})}
	assoc.ctx = ctx
	passDone, shedDone := make(chan struct{}), make(chan struct{})
	go func() { assoc.updateClusters(); close(passDone) }()
	<-ctx.entered // the matrix was copied and clustering owns the scratch
	go func() { assoc.ShedMemory(); close(shedDone) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		assoc.stateLock.Lock()
		generation := assoc.generation
		assoc.stateLock.Unlock()
		if generation != 0 {
			break
		}
		if time.Now().After(deadline) {
			close(ctx.resume)
			t.Fatal("shed did not invalidate the in-flight generation")
		}
		time.Sleep(time.Millisecond)
	}
	close(ctx.resume)
	<-passDone
	<-shedDone
	if len(assoc.clusters.Load().members) != 0 {
		t.Fatal("in-flight pass published pre-pressure clusters")
	}
	if !reflect.DeepEqual(assoc.scratch, ipAssocScratch{}) {
		t.Fatal("in-flight pass retained workspace after pressure returned")
	}
}

func BenchmarkIpAssocScratchRepeatedInput(b *testing.B) {
	for _, shape := range []struct {
		name             string
		entities, blocks int
		sameName         bool
	}{
		{"same_site", 128, 1, true},
		{"repeated_history", 40, 8, false},
		{"ordinary", 12, 1, false},
	} {
		b.Run(shape.name, func(b *testing.B) {
			assoc := testingIpAssocScratchMatrix(shape.entities, shape.blocks, shape.sameName)
			assoc.updateClusters()
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				assoc.dirty = true
				assoc.updateClusters()
			}
			b.ReportMetric(float64(cap(assoc.scratch.pairs))*float64(unsafe.Sizeof(ipAssocAggPair{})), "retained-pairs-B")
		})
	}
}
