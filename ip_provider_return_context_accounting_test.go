// Pins visible metadata against the unchanged reservations and exposes link
// allocation cost. Visible sizes are not a claim about all Go runtime memory.
package connect

import (
	"context"
	"reflect"
	"testing"
	"testing/synctest"
	"unsafe"
)

// The asynchronous envelope's 512-byte base owns the item, while 48 bytes per
// packet cover up to twice the live slice backing. Packet roots are separate.
func TestProviderReturnCustodyMetadataFitsExistingClaims(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		budget := NewTransferMemoryBudget(mib(2))
		fixture := newTcpProviderMemoryRetirementFixture(t, budget)
		provider := fixture.provider
		itemBytes := ByteCount(unsafe.Sizeof(providerReturnItem{}))
		pathBytes := ByteCount(unsafe.Sizeof(IpPath{}))
		linkBytes := ByteCount(unsafe.Sizeof(providerReturnContextLink{}))
		if itemBytes > 512 {
			t.Fatalf("return item layout=%d exceeds unchanged 512-byte envelope", itemBytes)
		}
		for _, count := range []int{1, natMemoryBatchCount, providerReturnBatchMaxFrames} {
			packet := MessagePoolCopy(ipOosTcpPacket(fixture.path, tcpFlagAck, nil))
			packets := make([][]byte, count)
			for index := range packets {
				packets[index] = packet
			}
			baseline := budget.UsedByteCount()
			item := provider.takeReturnItemForPackets(packets, receiveRecoveryModeNonblocking)
			if item == nil {
				MessagePoolReturn(packet)
				t.Fatal("bounded asynchronous envelope was not admitted")
			}
			wantCharge := ByteCount(512+48*count) + ByteCount(count)*natPacketMemoryByteCount(packet)
			for range count {
				item.packets = append(item.packets, MessagePoolShareReadOnly(packet))
			}
			metadataBytes := itemBytes + ByteCount(cap(item.packets))*ByteCount(unsafe.Sizeof([]byte{}))
			if metadataBytes > ByteCount(512+48*count) {
				t.Errorf("count=%d item/backing metadata=%d exceeds existing metadata claim=%d", count, metadataBytes, 512+48*count)
			}
			if item.memory.bytes != wantCharge || budget.UsedByteCount() != baseline+wantCharge {
				t.Errorf("count=%d item charge=%d root delta=%d want=%d", count, item.memory.bytes, budget.UsedByteCount()-baseline, wantCharge)
			}
			provider.releaseReturnItem(item)
			if !MessagePoolReturn(packet) {
				t.Error("metadata accounting test retained an asynchronous packet share")
			}
			if budget.UsedByteCount() != baseline {
				t.Error("asynchronous envelope did not return its original claim")
			}
		}
		// These three persistent objects belong to the existing 24-KiB flow
		// metadata/worker row. IpPath adds an interface, not another context:
		// its referent is the sequence's already-owned originating context.
		flowObjects := ByteCount(unsafe.Sizeof(TcpSequence{})) + pathBytes + 2*16
		if flowObjects > kib(24) {
			t.Fatalf("sequence/path/address layouts=%d exceed existing fixed flow row", flowObjects)
		}
		// Bounded TCP batches own at most two frames, while the existing
		// temporary arrays are allocated at the production batch limit.
		callbackObjects := itemBytes + linkBytes + 2*pathBytes +
			ByteCount(providerReturnBatchMaxFrames)*(ByteCount(unsafe.Sizeof((*byte)(nil)))+ByteCount(unsafe.Sizeof([]byte{}))) +
			ByteCount(natMemoryBatchCount)*ByteCount(unsafe.Sizeof(SendPack{}.singleFrameValue))
		if callbackObjects > kib(24) {
			t.Fatalf("visible callback metadata=%d exceeds existing scratch floor", callbackObjects)
		}
		ctx, cancel := context.WithCancel(context.Background())
		childBytes := reflect.TypeOf(ctx).Elem().Size()
		cancel()
		t.Logf("visible layouts: item=%d/512 path=%d link=%d flow_objects=%d/24576 callback_objects=%d/24576 cancel_context_struct=%d retained_item_pool_capacity=%d",
			itemBytes, pathBytes, linkBytes, flowObjects, callbackObjects, childBytes, cap(provider.returnItemPool))
		t.Log("layout checks exclude allocator rounding, context channels/maps/closures, goroutine stacks and inspection scratch; benchmark allocations and their ownership must be reviewed separately")
	})
}

// A source gate can be reclaimed between consecutive callbacks. Measure that
// common first-child registration cost too, not only a reused parent map.
func BenchmarkProviderReturnContextFreshSourceLink(b *testing.B) {
	providerCtx, cancelProvider := context.WithCancel(context.Background())
	defer cancelProvider()
	flowCtx, cancelFlow := context.WithCancel(context.Background())
	defer cancelFlow()
	b.ReportAllocs()
	for b.Loop() {
		sourceCtx, cancelSource := context.WithCancel(providerCtx)
		link := newProviderReturnContextLink(sourceCtx, flowCtx)
		if link.ctx.Err() != nil {
			b.Fatal("unexpected canceled fresh-source link")
		}
		link.close()
		cancelSource()
	}
}

// Paired source-generation baseline for interpreting fresh-source link cost.
func BenchmarkProviderReturnContextFreshSourceOnly(b *testing.B) {
	providerCtx, cancelProvider := context.WithCancel(context.Background())
	defer cancelProvider()
	b.ReportAllocs()
	for b.Loop() {
		sourceCtx, cancelSource := context.WithCancel(providerCtx)
		if sourceCtx.Err() != nil {
			b.Fatal("unexpected canceled fresh source")
		}
		cancelSource()
	}
}

// Closing a link is not a deallocation promise: an accepted raw Pack may keep
// its canceled child context while queued. This bounded ring models that
// retention without pretending to measure whole-provider throughput or RSS.
func BenchmarkProviderReturnContextAcceptedChildRetention(b *testing.B) {
	sourceCtx, cancelSource := context.WithCancel(context.Background())
	defer cancelSource()
	flowCtx, cancelFlow := context.WithCancel(context.Background())
	defer cancelFlow()
	var queued [2]context.Context
	index := 0
	b.ReportAllocs()
	for b.Loop() {
		link := newProviderReturnContextLink(sourceCtx, flowCtx)
		queued[index] = link.ctx
		link.close()
		index = (index + 1) % len(queued)
	}
	for _, ctx := range queued {
		if ctx != nil && ctx.Err() != context.Canceled {
			b.Fatal("accepted child did not retain only a canceled admission context")
		}
	}
}
