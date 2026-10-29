// Shared-root metadata refusal preserves the full ordinary retry field.
package connect

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"unsafe"
)

// Local child permission cannot bypass an exhausted common retained-memory root.
func TestNativeSynRetryReadinessSharedRootRefusalDoesNotNarrowField(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		control := newSynRetryReadyControl(t, 51412)
		ready, paid, packetPaid := control.owner.readiness, control.owner.readiness.credit, control.owner.credit
		count := len(ready.slots) + 1
		const childMaximum ByteCount = 64 * 1024
		root := NewTransferMemoryBudget(ByteCount(count) * childMaximum)
		children := make([]*TransferMemoryBudget, count)
		credits := make([]*nativeBatchCreditFixture, count)
		started := make([]int64, count)
		for index := range credits {
			child := NewTransferMemoryBudgetWithParent(childMaximum, root)
			children[index] = child
			credits[index] = newNativeBatchCreditFixture(t, func(settings *ClientSettings) {
				settings.SendBufferSettings.ResendQueueBudget = child
			})
			started[index] = credits[index].base.started.Load()
			queue := credits[index].source.resendQueue
			queue.stateLock.Lock()
			exact := queue.budget == child && queue.lifetimeBudget && queue.minByteCount == 0
			queue.stateLock.Unlock()
			if !exact {
				t.Fatal("parented source did not use exact retained admission without a queue floor")
			}
		}
		metadata := addReceiveQueueByteCount(
			addReceiveQueueByteCount(retainedMessageCapacity(ByteCount(unsafe.Sizeof(tcpSynRetryReadiness{}))),
				retainedMessageCapacity(ByteCount(count)*ByteCount(unsafe.Sizeof(tcpSynRetryReadySlot{})))),
			transferRetainedControlCreditByteCount(),
		)
		before := root.StatsWithDescendants(children...)
		for index, child := range before[1:] {
			if child.TotalByteCount-child.UsedByteCount < metadata {
				t.Fatalf("child%d cannot independently fit full replacement metadata: local=%d required=%d", index, child.TotalByteCount-child.UsedByteCount, metadata)
			}
		}
		held := root.Available()
		if held <= 0 || !root.TryReserve(held) {
			t.Fatal("actual shared root could not retain its remaining permission")
		}
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { root.Release(held) }) }
		t.Cleanup(release)
		full := root.StatsWithDescendants(children...)
		if full[0].UsedByteCount != full[0].TotalByteCount || root.Available() != 0 {
			t.Fatal("shared root was not exactly exhausted by actual retained owners")
		}
		for index, child := range children {
			if child.Parent() != root || child.TotalByteCount()-child.UsedByteCount() < metadata || child.Available() != 0 {
				t.Fatalf("child%d did not retain local headroom behind the full common root", index)
			}
		}
		control.harness.multiClient.groupRaceCandidatesForTest = func(*parsedPacketGroup) []*multiClientChannel {
			clients := make([]*multiClientChannel, count)
			for index, credit := range credits {
				clients[index] = credit.base.selected
			}
			return clients
		}
		control.harness.inject("a", reraceProviderSignal(t, control.path))
		synctest.Wait()
		control.update.stateLock.Lock()
		quiet := control.update.synRetry == control.owner && control.owner.readiness == ready &&
			control.owner.readinessOverflow && control.owner.references == 1 && !control.owner.queued &&
			control.update.client.Load() == nil && !ready.indexed.Load() && ready.count == 0 &&
			!paid.released.Load() && !packetPaid.released.Load() && bytes.Equal(control.owner.packet, control.original)
		control.update.stateLock.Unlock()
		if !quiet || control.harness.multiClient.synRetryReadinessRefusals.Load() != 1 {
			t.Fatal("SHARED_ROOT_METADATA_REFUSAL_LOST_BOUNDED_FALLBACK")
		}
		after := root.StatsWithDescendants(children...)
		for index := range full {
			if after[index] != full[index] {
				t.Fatalf("SHARED_ROOT_METADATA_REFUSAL_CHANGED_CREDIT: ledger=%d before=%+v after=%+v", index, full[index], after[index])
			}
		}
		for index, credit := range credits {
			if credit.base.started.Load() != started[index]+1 || credit.source.resendCapacityUnavailable.waiterCount.Load() != 0 ||
				credit.source.preparedFlightUnavailable.waiterCount.Load() != 0 || credit.base.selected.dialFailureCount() != 0 ||
				credit.base.selected.IsDone() || len(credit.base.route) != 0 {
				t.Fatalf("SHARED_ROOT_METADATA_REFUSAL_NARROWED_FIELD: source=%d", index)
			}
		}

		// Restore aggregate memory alone. Every exact peer ACK remains held,
		// so this budget notification cannot grant any source readiness turn.
		release()
		synctest.Wait()
		if root.Available() <= 0 {
			t.Fatal("actual shared-root release did not restore headroom")
		}
		for index, credit := range credits {
			if credit.base.started.Load() != started[index]+1 || credit.source.resendCapacityUnavailable.waiterCount.Load() != 0 ||
				credit.source.preparedFlightUnavailable.waiterCount.Load() != 0 || credit.base.selected.dialFailureCount() != 0 || len(credit.base.route) != 0 {
				t.Fatalf("SHARED_ROOT_MEMORY_WAKE_MANUFACTURED_RETRY: source=%d", index)
			}
		}
		control.close(t)
		requireSynRetryReadyRetired(t, ready, paid)
		if !packetPaid.released.Load() {
			t.Fatal("joined parent retained its original SYN credit")
		}
		for _, credit := range credits {
			credit.releaseCapacity()
			if err := credit.base.closeClient(context.Background()); err != nil {
				t.Fatalf("join shared-root source: %v", err)
			}
		}
		synctest.Wait()
		balanced := root.StatsWithDescendants(children...)
		for index, ledger := range balanced {
			if ledger.UsedByteCount != 0 || ledger.ReservedByteCount != ledger.ReleasedByteCount {
				t.Fatalf("SHARED_ROOT_METADATA_CLEANUP_UNBALANCED: ledger=%d stats=%+v", index, ledger)
			}
		}
	})
}
