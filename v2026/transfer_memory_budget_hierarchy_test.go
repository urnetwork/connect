package connect

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTransferMemoryBudgetHierarchyExactAncestorsAndDrain(t *testing.T) {
	root := NewTransferMemoryBudget(100)
	client := NewTransferMemoryBudgetWithParent(100, root)
	provider := NewTransferMemoryBudgetWithParent(40, root)
	old := NewTransferMemoryBudgetWithParent(100, client)
	fresh := NewTransferMemoryBudgetWithParent(40, provider)
	AssertEqual(t, old.TryReserve(100), true)
	client.SetTotalByteCount(60)
	// Retuning the other group cannot invent capacity while the old client
	// generation is still draining above its new share.
	provider.SetTotalByteCount(40)
	AssertEqual(t, fresh.TryReserve(1), false)
	AssertEqual(t, old.TryReserve(1), false)
	AssertEqual(t, root.UsedByteCount(), ByteCount(100))
	notify := fresh.CapacityNotify()
	old.Release(40)
	select {
	case <-notify:
	case <-time.After(time.Second):
		t.Fatal("sibling release did not wake provider")
	}
	AssertEqual(t, fresh.TryReserve(40), true)
	AssertEqual(t, root.UsedByteCount(), ByteCount(100))
	// Reverse transition overlaps admitted provider ownership too.
	provider.SetTotalByteCount(10)
	client.SetTotalByteCount(100)
	AssertEqual(t, old.TryReserve(1), false)
	AssertEqual(t, fresh.TryReserve(1), false)
	fresh.Release(40)
	AssertEqual(t, old.TryReserve(40), true)
	old.Release(100)
	for _, budget := range []*TransferMemoryBudget{root, client, provider, old, fresh} {
		stats := budget.Stats()
		AssertEqual(t, stats.UsedByteCount, ByteCount(0))
		AssertEqual(t, stats.ReservedByteCount, stats.ReleasedByteCount)
	}
}

func TestTransferMemoryBudgetHierarchyConcurrentFanoutAndResize(t *testing.T) {
	root := NewTransferMemoryBudget(64)
	left := NewTransferMemoryBudgetWithParent(64, root)
	right := NewTransferMemoryBudgetWithParent(64, root)
	start := make(chan struct{})
	var wg sync.WaitGroup
	var escaped atomic.Bool
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			group := left
			if i%2 != 0 {
				group = right
			}
			leaf := NewTransferMemoryBudgetWithParent(64, group)
			<-start
			for range 300 {
				if leaf.TryReserve(7) {
					stats := root.Stats()
					if stats.UsedByteCount > stats.TotalByteCount || stats.UsedByteCount != stats.ReservedByteCount-stats.ReleasedByteCount {
						escaped.Store(true)
					}
					leaf.Release(7)
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for range 300 {
			left.SetTotalByteCount(8)
			right.SetTotalByteCount(64)
			right.SetTotalByteCount(8)
			left.SetTotalByteCount(64)
		}
	}()
	close(start)
	wg.Wait()
	AssertEqual(t, escaped.Load(), false)
	stats := root.Stats()
	AssertEqual(t, stats.UsedByteCount, ByteCount(0))
	AssertEqual(t, stats.ReservedByteCount, stats.ReleasedByteCount)
}

func TestTransferMemoryBudgetHierarchyResizeSharesAdmissionBoundary(t *testing.T) {
	root := NewTransferMemoryBudget(1)
	group := NewTransferMemoryBudgetWithParent(1, root)
	leaf := NewTransferMemoryBudgetWithParent(1, group)
	root.admissionLock.Lock()
	started := make(chan struct{}, 2)
	reserved := make(chan bool, 1)
	resized := make(chan struct{}, 1)
	go func() { started <- struct{}{}; reserved <- leaf.TryReserve(1) }()
	go func() { started <- struct{}{}; group.SetTotalByteCount(0); resized <- struct{}{} }()
	<-started
	<-started
	select {
	case <-reserved:
		root.admissionLock.Unlock()
		t.Fatal("leaf admission escaped root lock")
	case <-resized:
		root.admissionLock.Unlock()
		t.Fatal("group resize escaped root lock")
	case <-time.After(10 * time.Millisecond):
	}
	root.admissionLock.Unlock()
	oldOwner := <-reserved
	<-resized
	AssertEqual(t, leaf.TryReserve(1), false)
	if oldOwner {
		leaf.Release(1)
	}
	AssertEqual(t, root.UsedByteCount(), ByteCount(0))
}

func TestTransferMemoryBudgetHierarchyIntrusiveSiblingWaiter(t *testing.T) {
	root := NewTransferMemoryBudget(10)
	left := NewTransferMemoryBudgetWithParent(10, root)
	right := NewTransferMemoryBudgetWithParent(10, root)
	AssertEqual(t, left.TryReserve(10), true)
	waiter := newTransferMemoryBudgetWaiter()
	defer waiter.reset()
	notify := waiter.subscribe(right, 10)
	AssertEqual(t, right.TryReserve(10), false)
	left.Release(10)
	select {
	case <-notify:
	case <-time.After(time.Second):
		t.Fatal("intrusive sibling waiter did not wake")
	}
	AssertEqual(t, right.TryReserve(10), true)
	right.Release(10)
	AssertEqual(t, root.capacityWaiterCount.Load(), int64(0))
}

func TestTransferMemoryBudgetHierarchyRefusalDoesNotChargeAncestors(t *testing.T) {
	root := NewTransferMemoryBudget(100)
	group := NewTransferMemoryBudgetWithParent(10, root)
	leaf := NewTransferMemoryBudgetWithParent(100, group)
	AssertEqual(t, leaf.TryReserve(11), false)
	for _, budget := range []*TransferMemoryBudget{root, group, leaf} {
		AssertEqual(t, budget.Stats().ReservedByteCount, ByteCount(0))
	}
	AssertEqual(t, leaf.TryReserve(-1), false)
	AssertEqual(t, leaf.Available(), ByteCount(10))
}
