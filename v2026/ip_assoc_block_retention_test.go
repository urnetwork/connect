package connect

import (
	"context"
	"net/netip"
	"os"
	"runtime"
	"runtime/debug"
	"testing"
	"time"
	"weak"
)

func newBlockRotationMemoryAssoc() *IpAssoc {
	assoc := &IpAssoc{
		ctx: context.Background(), settings: DefaultIpAssocSettings(),
		lastActive: map[netip.Addr]time.Time{}, baseNames: map[netip.Addr][]string{},
	}
	assoc.clusters.Store(&ipAssocClusters{members: map[netip.Addr][]netip.Addr{}})
	return assoc
}

// This is a valid saturated block, not an enlarged production bound. Separate
// helper frames ensure test locals cannot keep an evicted owner alive at GC.
func fillBlockRotationMemoryBlock(block *ipAssocBlock, settings *IpAssocSettings) {
	for i := range settings.MaxEntityCount {
		block.index(netip.AddrFrom4([4]byte{198, 18, byte(i >> 8), byte(i)}), settings.MaxEntityCount)
	}
	for i := uint16(0); int(i) < settings.MaxEntityCount && len(block.coCounts) < settings.MaxAssociationCount; i++ {
		for j := i + 1; int(j) < settings.MaxEntityCount && len(block.coCounts) < settings.MaxAssociationCount; j++ {
			block.coCounts[ipAssocPackIndexPair(i, j)] = 1
		}
	}
}

//go:noinline
func expiredBlockRotationMemoryOwners(assoc *IpAssoc) []weak.Pointer[ipAssocBlock] {
	now := time.Unix(1_000, 0)
	count := assoc.settings.AssociationBlockCount
	owners := make([]weak.Pointer[ipAssocBlock], 0, count)
	for i := range count {
		block := assoc.blockWithLock(now.Add(time.Duration(i) * assoc.settings.AssociationBlockDuration))
		fillBlockRotationMemoryBlock(block, assoc.settings)
		owners = append(owners, weak.Make(block))
	}
	// Advance only the supplied block clock. The default eight 300-second
	// history remains unchanged; there is no wall-clock wait or policy edit.
	for i := count; i < 2*count; i++ {
		assoc.blockWithLock(now.Add(time.Duration(i) * assoc.settings.AssociationBlockDuration))
	}
	return owners
}

func liveBlockRotationMemoryOwners(owners []weak.Pointer[ipAssocBlock]) int {
	count := 0
	for _, owner := range owners {
		if owner.Value() != nil {
			count++
		}
	}
	return count
}

func TestIpAssocBlockRotationReleasesEvictedMatrixOwners(t *testing.T) {
	assoc := newBlockRotationMemoryAssoc()
	owners := expiredBlockRotationMemoryOwners(assoc)
	if len(assoc.blocks) != assoc.settings.AssociationBlockCount {
		t.Fatal("rotation changed the live history length")
	}
	for _, block := range assoc.blocks {
		if len(block.indexes) != 0 || len(block.coCounts) != 0 {
			t.Fatal("expired input is still logically visible")
		}
	}
	runtime.GC()
	runtime.GC()
	if count := liveBlockRotationMemoryOwners(owners); count != 0 {
		t.Fatalf("rotation has zero visible old blocks but retains %d evicted matrix owners through its backing array", count)
	}
	runtime.KeepAlive(assoc)
}

func TestIpAssocBlockRotationKeepsLiveMatrixAndNames(t *testing.T) {
	assoc := newBlockRotationMemoryAssoc()
	now := time.Unix(1_000, 0)
	count := assoc.settings.AssociationBlockCount
	current := make([]*ipAssocBlock, count)
	for i := range count {
		block := assoc.blockWithLock(now.Add(time.Duration(i) * assoc.settings.AssociationBlockDuration))
		addr := netip.AddrFrom4([4]byte{198, 18, 0, byte(i + 1)})
		block.index(addr, assoc.settings.MaxEntityCount)
		block.counts[0] = uint32(i + 10)
		assoc.baseNames[addr] = []string{"fixture.example"}
		current[i] = block
	}
	newest := assoc.blockWithLock(now.Add(time.Duration(count) * assoc.settings.AssociationBlockDuration))
	if assoc.blocks[count-1] != newest || len(assoc.baseNames) != count-1 {
		t.Fatal("rotation changed new-block or name-expiry semantics")
	}
	for i, block := range assoc.blocks[:count-1] {
		if block != current[i+1] || block.counts[0] != uint32(i+11) {
			t.Fatal("rotation changed a still-live block or its counters")
		}
	}
	assoc.ShedMemory()
	if assoc.blocks != nil {
		t.Fatal("pressure release retained the current block array")
	}
}

// Opt-in fresh-process measurement. Both points force collection only in the
// test to distinguish reachable owners from idle spans. This is not a physical
// gate, a new GC policy, or attribution of a short (<40m) device burst to rotation.
func TestIpAssocBlockRotationRetentionMeasurement(t *testing.T) {
	if os.Getenv("URNETWORK_IPASSOC_ROTATION_MEASURE") != "1" {
		t.Skip("opt-in fresh process: URNETWORK_IPASSOC_ROTATION_MEASURE=1")
	}
	debug.FreeOSMemory()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	assoc := newBlockRotationMemoryAssoc()
	owners := expiredBlockRotationMemoryOwners(assoc)
	debug.FreeOSMemory()
	runtime.ReadMemStats(&after)
	t.Logf("rotation: live_blocks=%d expired_owners=%d heap_delta=%d inuse_delta=%d runtime_delta=%d stack_delta=%d",
		len(assoc.blocks), liveBlockRotationMemoryOwners(owners), int64(after.HeapAlloc)-int64(before.HeapAlloc),
		int64(after.HeapInuse)-int64(before.HeapInuse),
		int64(after.Sys-after.HeapReleased)-int64(before.Sys-before.HeapReleased),
		int64(after.StackInuse)-int64(before.StackInuse))
	runtime.KeepAlive(assoc)
}

func BenchmarkIpAssocBlockRotation(b *testing.B) {
	assoc := newBlockRotationMemoryAssoc()
	now := time.Unix(1_000, 0)
	for i := range assoc.settings.AssociationBlockCount {
		assoc.blockWithLock(now.Add(time.Duration(i) * assoc.settings.AssociationBlockDuration))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		now = now.Add(assoc.settings.AssociationBlockDuration)
		assoc.blockWithLock(now)
	}
}
