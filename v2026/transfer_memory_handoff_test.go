package connect

import (
	"sync"
	"testing"
)

func TestTransferMemoryReservationMoveAtFullRoot(t *testing.T) {
	root := NewTransferMemoryBudget(64)
	common := NewTransferMemoryBudgetWithParent(40, root)
	source := NewTransferMemoryBudgetWithParent(40, common)
	target := NewTransferMemoryBudgetWithParent(40, common)
	if !source.TryReserve(32) || !root.TryReserve(32) {
		t.Fatal("fixture reservation failed")
	}
	if !source.tryMoveReservation(target, 32) {
		t.Fatal("full-root move refused existing ownership")
	}
	if source.UsedByteCount() != 0 || target.UsedByteCount() != 32 || common.UsedByteCount() != 32 || root.UsedByteCount() != 64 {
		t.Fatal("move changed common-ancestor charge")
	}
	if root.reservedByteCount.Load() != 64 || root.releasedByteCount.Load() != 0 || common.reservedByteCount.Load() != 32 || common.releasedByteCount.Load() != 0 {
		t.Fatal("move transiently released or re-reserved a common ancestor")
	}
	target.Release(32)
	root.Release(32)
	for _, budget := range []*TransferMemoryBudget{source, target, common, root} {
		if budget.UsedByteCount() != 0 || budget.reservedByteCount.Load() != budget.releasedByteCount.Load() {
			t.Fatal("final owner release did not balance every budget")
		}
	}
}

func TestTransferMemoryReservationMovePreservesTargetCeiling(t *testing.T) {
	root := NewTransferMemoryBudget(64)
	source := NewTransferMemoryBudgetWithParent(64, root)
	target := NewTransferMemoryBudgetWithParent(16, root)
	if !source.TryReserve(32) || source.tryMoveReservation(target, 32) {
		t.Fatal("claim move bypassed destination-child ceiling")
	}
	if source.UsedByteCount() != 32 || target.UsedByteCount() != 0 || root.UsedByteCount() != 32 || root.releasedByteCount.Load() != 0 {
		t.Fatal("refused move changed the live source claim")
	}
	if source.tryMoveReservation(NewTransferMemoryBudget(64), 32) || source.tryMoveReservation(target, -1) || source.tryMoveReservation(target, 65) {
		t.Fatal("invalid move succeeded")
	}
	source.Release(32)
}

func TestTransferMemoryReservationMoveParentChildAndCancel(t *testing.T) {
	root := NewTransferMemoryBudget(32)
	child := NewTransferMemoryBudgetWithParent(32, root)
	if !root.TryReserve(32) || !root.tryMoveReservation(child, 32) || !child.tryMoveReservation(root, 32) || !root.tryMoveReservation(root, 32) {
		t.Fatal("parent/child/identity handoff failed")
	}
	root.Release(32)
	if root.UsedByteCount() != 0 || child.UsedByteCount() != 0 || child.reservedByteCount.Load() != child.releasedByteCount.Load() {
		t.Fatal("canceled final owner leaked or double-released its claim")
	}
}

func TestTransferMemoryReservationConcurrentMovesStayBounded(t *testing.T) {
	root := NewTransferMemoryBudget(64)
	left := NewTransferMemoryBudgetWithParent(64, root)
	right := NewTransferMemoryBudgetWithParent(64, root)
	if !left.TryReserve(32) || !right.TryReserve(32) {
		t.Fatal("fixture reservation failed")
	}
	var workers sync.WaitGroup
	for _, pair := range [][2]*TransferMemoryBudget{{left, right}, {right, left}} {
		workers.Go(func() {
			for range 1000 {
				if !pair[0].tryMoveReservation(pair[1], 1) || !pair[1].tryMoveReservation(pair[0], 1) {
					t.Error("small exclusively owned move failed")
					return
				}
			}
		})
	}
	workers.Wait()
	if root.UsedByteCount() != 64 || left.UsedByteCount() != 32 || right.UsedByteCount() != 32 || root.releasedByteCount.Load() != 0 {
		t.Fatal("concurrent moves escaped the shared admission lock")
	}
	left.Release(32)
	right.Release(32)
}
