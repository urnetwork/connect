// Protected ingress permissions survive full roots, sibling borrowing and role changes.
package connect

import "testing"

// Returns one synthetic owner with independent provider and client descendants.
func ingressProtectionTestBudgets() (root, client, send, pack, reorder, provider *TransferMemoryBudget) {
	root = NewTransferMemoryBudget(1000)
	client = NewTransferMemoryBudgetWithParent(800, root)
	send = NewTransferMemoryBudgetWithParent(800, client)
	pack = NewTransferMemoryBudgetWithParent(200, client)
	reorder = NewTransferMemoryBudgetWithParent(100, client)
	provider = NewTransferMemoryBudgetWithParent(1000, root)
	client.ProtectIngressBudgets(pack, reorder)
	return
}

// All ordinary siblings stop before consuming either protected receive window.
func TestTransferMemoryIngressProtectionFullRoot(t *testing.T) {
	root, client, send, pack, reorder, provider := ingressProtectionTestBudgets()
	if send.Available() != 500 || !send.TryReserve(500) || send.TryReserve(1) {
		t.Fatal("outbound admission consumed protected client ingress permission")
	}
	if provider.Available() != 200 || !provider.TryReserve(200) || provider.TryReserve(1) {
		t.Fatal("root sibling consumed protected ingress permission")
	}
	if root.UsedByteCount() != 700 || pack.Available() != 200 || reorder.Available() != 100 {
		t.Fatal("idle protection became an allocated-byte charge or lost its capacity")
	}
	if !pack.TryReserve(200) || !reorder.TryReserve(100) || root.UsedByteCount() != 1000 || client.UsedByteCount() != 800 {
		t.Fatal("incoming ACK/reorder windows could not fill their prepaid permissions")
	}
	if pack.TryReserve(1) || reorder.TryReserve(1) || provider.TryReserve(1) || send.TryReserve(1) {
		t.Fatal("protected admission exceeded an existing ceiling")
	}
	pack.Release(200)
	reorder.Release(100)
	if provider.TryReserve(1) || send.TryReserve(1) {
		t.Fatal("release lent the protected window to an ordinary owner")
	}
	send.Release(500)
	provider.Release(200)
	if root.UsedByteCount() != 0 || root.reservedByteCount.Load() != root.releasedByteCount.Load() {
		t.Fatal("ancestor reservation ownership did not reconcile")
	}
}

// Credit may move between receive stages at a full parent, but not escape the partition.
func TestTransferMemoryIngressProtectionCreditMove(t *testing.T) {
	root, _, send, pack, reorder, provider := ingressProtectionTestBudgets()
	if !send.TryReserve(500) || !provider.TryReserve(200) || !pack.TryReserve(100) {
		t.Fatal("synthetic admitted owners did not fit")
	}
	if pack.tryMoveReservation(send, 100) {
		t.Fatal("moving ingress credit bypassed the protected sibling ceiling")
	}
	if !pack.tryMoveReservation(reorder, 100) || pack.UsedByteCount() != 0 || reorder.UsedByteCount() != 100 || root.UsedByteCount() != 800 {
		t.Fatal("same-root handoff lost or duplicated its claim")
	}
	if !pack.TryReserve(200) || root.UsedByteCount() != 1000 {
		t.Fatal("moving into reorder stole the Pack window")
	}
	pack.Release(200)
	reorder.Release(100)
	send.Release(500)
	provider.Release(200)
	if root.UsedByteCount() != 0 || root.reservedByteCount.Load() != root.releasedByteCount.Load() {
		t.Fatal("moved ownership did not reconcile at the parent")
	}
}

// Shrinking preserves old owners but atomically changes both ingress permissions.
func TestTransferMemoryIngressProtectionRoleResize(t *testing.T) {
	root, client, send, pack, reorder, _ := ingressProtectionTestBudgets()
	if !send.TryReserve(500) {
		t.Fatal("original role did not admit its owner")
	}
	root.SetTotalByteCounts(
		TransferMemoryBudgetTotal{Budget: client, TotalByteCount: 600},
		TransferMemoryBudgetTotal{Budget: pack, TotalByteCount: 100},
		TransferMemoryBudgetTotal{Budget: reorder, TotalByteCount: 100},
	)
	if send.UsedByteCount() != 500 || send.TryReserve(1) || pack.TryReserve(1) {
		t.Fatal("role shrink evicted an owner or admitted over its protected ceiling")
	}
	send.Release(100)
	if !pack.TryReserve(100) || !reorder.TryReserve(100) || client.UsedByteCount() != 600 {
		t.Fatal("drained old role did not reopen protected admission")
	}
	root.SetTotalByteCounts(
		TransferMemoryBudgetTotal{Budget: client, TotalByteCount: 800},
		TransferMemoryBudgetTotal{Budget: pack, TotalByteCount: 200},
	)
	if !send.TryReserve(100) || send.TryReserve(1) || !pack.TryReserve(100) {
		t.Fatal("role expansion did not preserve both overlapping live owners")
	}
	send.Release(500)
	pack.Release(200)
	reorder.Release(100)
	if root.UsedByteCount() != 0 {
		t.Fatal("resized ownership did not reconcile")
	}
}

// A protected owner's state never constrains an independently constructed device.
func TestTransferMemoryIngressProtectionOwnerIsolation(t *testing.T) {
	_, _, send, _, _, _ := ingressProtectionTestBudgets()
	other := NewTransferMemoryBudget(800)
	if !send.TryReserve(500) || !other.TryReserve(800) {
		t.Fatal("independent owners shared admission state")
	}
	send.Release(500)
	other.Release(800)
}
