// Pins protected ingress admission against role changes and speculative waiter grants.
package connect

import (
	"math"
	"testing"
)

// Tracks only claims admitted by this test and reconciles them after every verdict.
func ingressWaiterTestClaims(t *testing.T, root *TransferMemoryBudget) (
	reserve func(*TransferMemoryBudget, ByteCount) bool,
	release func(*TransferMemoryBudget, ByteCount),
) {
	t.Helper()
	owned := map[*TransferMemoryBudget]ByteCount{}
	t.Cleanup(func() {
		for budget, bytes := range owned {
			if bytes != 0 {
				budget.Release(bytes)
			}
		}
		for budget := range owned {
			stats := budget.Stats()
			if stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
				t.Errorf("test leaf ownership did not reconcile: %+v", stats)
			}
		}
		stats := root.Stats()
		if stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
			t.Errorf("test root ownership did not reconcile: %+v", stats)
		}
	})
	reserve = func(budget *TransferMemoryBudget, bytes ByteCount) bool {
		admitted := budget.TryReserve(bytes)
		if admitted {
			owned[budget] += bytes
		}
		stats := root.Stats()
		if admitted && bytes > 0 && stats.UsedByteCount > stats.TotalByteCount {
			t.Errorf("new admission exceeded the root ceiling: %+v", stats)
		}
		return admitted
	}
	release = func(budget *TransferMemoryBudget, bytes ByteCount) {
		if bytes < 0 || owned[budget] < bytes {
			t.Fatalf("test tried to release %d bytes from its %d-byte claim", bytes, owned[budget])
		}
		owned[budget] -= bytes
		budget.Release(bytes)
	}
	return
}

// A waiter blocked by protected child capacity cannot consume a sibling's sole wake.
func TestTransferMemoryIngressWaiterSkipsProtectedChild(t *testing.T) {
	root, _, send, _, _, provider := ingressProtectionTestBudgets()
	reserve, release := ingressWaiterTestClaims(t, root)
	if !reserve(send, 500) || !reserve(provider, 200) {
		t.Fatal("initial ordinary owners did not fit")
	}
	clientWaiter := newTransferMemoryBudgetWaiter()
	providerWaiter := newTransferMemoryBudgetWaiter()
	defer clientWaiter.reset()
	defer providerWaiter.reset()
	clientNotify := clientWaiter.subscribe(send, 100)
	providerNotify := providerWaiter.subscribe(provider, 100)
	if reserve(send, 100) || reserve(provider, 100) {
		t.Fatal("full ordinary permission admitted a waiting owner")
	}

	// Release synchronously completes the eligibility scan; no elapsed-time oracle.
	release(provider, 100)
	select {
	case <-providerNotify:
	default:
		t.Error("PROTECTED_WAITER_STARVED: eligible provider did not receive the sole grant")
	}
	select {
	case <-clientNotify:
		t.Error("PROTECTED_WAITER_MISGRANTED: protected client consumed the provider grant")
	default:
	}
	if send.Available() != 0 || provider.Available() != 100 || !reserve(provider, 100) {
		t.Error("released sibling capacity disagreed with actual admission")
	}
}

// Earlier grants compose with protected child permission before later FIFO decisions.
func TestTransferMemoryIngressWaiterAssignedGrantsRespectProtection(t *testing.T) {
	root, _, send, _, _, provider := ingressProtectionTestBudgets()
	reserve, release := ingressWaiterTestClaims(t, root)
	if !reserve(send, 400) || !reserve(provider, 300) {
		t.Fatal("initial ordinary owners did not fit")
	}
	first := newTransferMemoryBudgetWaiter()
	blocked := newTransferMemoryBudgetWaiter()
	sibling := newTransferMemoryBudgetWaiter()
	defer first.reset()
	defer blocked.reset()
	defer sibling.reset()
	firstNotify := first.subscribe(send, 50)
	blockedNotify := blocked.subscribe(send, 75)
	siblingNotify := sibling.subscribe(provider, 150)
	release(provider, 200)
	for _, row := range []struct {
		name   string
		notify <-chan struct{}
		want   bool
	}{
		{name: "first client", notify: firstNotify, want: true},
		{name: "second client", notify: blockedNotify, want: false},
		{name: "provider", notify: siblingNotify, want: true},
	} {
		granted := false
		select {
		case <-row.notify:
			granted = true
		default:
		}
		if granted != row.want {
			t.Errorf("PROTECTED_ASSIGNED_GRANT_MISMATCH: %s granted=%t want=%t", row.name, granted, row.want)
		}
	}
	if !reserve(send, 50) || !reserve(provider, 150) || reserve(send, 1) || reserve(provider, 1) {
		t.Error("assigned grants did not compose within the actual child and root permissions")
	}
}

// An old ordinary lease can receive its inner ack after shrink, then release itself.
func TestTransferMemoryIngressAvailabilityAfterRoleShrink(t *testing.T) {
	for _, ordinary := range []ByteCount{450, 500} {
		func() {
			root, client, send, pack, reorder, provider := ingressProtectionTestBudgets()
			reserve, release := ingressWaiterTestClaims(t, root)
			if !reserve(send, ordinary) || !reserve(provider, 700-ordinary) {
				t.Fatal("initial full-root ordinary owners did not fit")
			}
			root.SetTotalByteCounts(
				TransferMemoryBudgetTotal{Budget: client, TotalByteCount: 600},
				TransferMemoryBudgetTotal{Budget: pack, TotalByteCount: 100},
				TransferMemoryBudgetTotal{Budget: reorder, TotalByteCount: 100},
			)
			if send.UsedByteCount() != ordinary || root.UsedByteCount() != 700 || send.Available() != 0 || reserve(send, 1) {
				t.Errorf("ordinary=%d: role shrink evicted an owner or permitted ordinary growth", ordinary)
			}
			available := pack.Available()
			if available != 100 {
				t.Errorf("PROTECTED_SHRINK_PERMISSION_LOST: ordinary=%d Pack available=%d want=100", ordinary, available)
			}
			claim := available
			if claim <= 0 {
				claim = 100
			}
			if !reserve(pack, claim) {
				t.Errorf("PROTECTED_SHRINK_ACK_BLOCKED: ordinary=%d available=%d claim=%d", ordinary, available, claim)
				return
			}
			// Only admitted inner-ack ownership permits the old lease to retire.
			release(send, 100)
			if reorder.Available() != 100 || !reserve(reorder, 100) {
				t.Errorf("ordinary=%d: retained reorder permission could not progress after the ack", ordinary)
			}
		}()
	}
}

// Requested protection growth waits for real headroom without blocking prior grants.
func TestTransferMemoryIngressProtectionGrowthWaitsForHeadroom(t *testing.T) {
	root, client, send, pack, reorder, provider := ingressProtectionTestBudgets()
	reserve, release := ingressWaiterTestClaims(t, root)
	if !reserve(send, 500) || !reserve(provider, 200) {
		t.Fatal("initial ordinary owners did not fit")
	}
	root.SetTotalByteCounts(
		TransferMemoryBudgetTotal{Budget: client, TotalByteCount: 900},
		TransferMemoryBudgetTotal{Budget: pack, TotalByteCount: 300},
	)
	if pack.TotalByteCount() != 300 || root.UsedByteCount() != 700 || pack.Available() != 200 || reserve(pack, 201) {
		t.Error("requested growth invented root capacity or hid existing ingress permission")
	}
	if !reserve(pack, 200) || !reserve(reorder, 100) {
		t.Error("DEFERRED_PROTECTION_BLOCKED_OLD_PERMISSION: existing ingress grants stopped at a full root")
		return
	}
	if root.UsedByteCount() != 1000 || reserve(send, 1) || reserve(provider, 1) {
		t.Error("existing protected owners escaped the original root ceiling")
	}
	release(provider, 100)
	if pack.Available() != 100 || provider.Available() != 0 || !reserve(pack, 100) || root.UsedByteCount() != 1000 {
		t.Error("released real headroom did not fund the deferred protected grant exactly")
	}
}

// Invalid negative ceilings cannot wrap into permission or inherit a draining role.
func TestTransferMemoryIngressWaiterRejectsNegativeResize(t *testing.T) {
	for _, total := range []ByteCount{-1, math.MinInt64} {
		for _, scope := range []string{"root", "role", "ordinary", "protected"} {
			func() {
				root, client, send, pack, _, provider := ingressProtectionTestBudgets()
				reserve, _ := ingressWaiterTestClaims(t, root)
				if !reserve(send, 100) || !reserve(pack, 1) || !reserve(provider, 100) {
					t.Fatal("initial owners did not fit before the invalid resize")
				}
				target := root
				budgets := []*TransferMemoryBudget{send, pack}
				switch scope {
				case "role":
					target = client
				case "ordinary":
					target, budgets = send, []*TransferMemoryBudget{send}
				case "protected":
					target, budgets = pack, []*TransferMemoryBudget{pack}
				}
				notifications := make([]<-chan struct{}, len(budgets))
				for index, budget := range budgets {
					waiter := newTransferMemoryBudgetWaiter()
					defer waiter.reset()
					notifications[index] = waiter.subscribe(budget, 1)
				}
				// The resize publishes the notification decision synchronously.
				target.SetTotalByteCount(total)
				for index, budget := range budgets {
					if available := budget.Available(); available != 0 {
						t.Errorf("NEGATIVE_RESIZE_CAPACITY: scope=%s total=%d path=%d available=%d", scope, total, index, available)
					}
					if reserve(budget, 1) {
						t.Errorf("NEGATIVE_RESIZE_ADMITTED: scope=%s total=%d path=%d", scope, total, index)
					}
					select {
					case <-notifications[index]:
						t.Errorf("NEGATIVE_RESIZE_GRANTED: scope=%s total=%d path=%d", scope, total, index)
					default:
					}
				}
			}()
		}
	}
}

// A negative target forbids growth; a common ancestor may retain the same old claim.
func TestTransferMemoryIngressMoveRejectsNegativeTarget(t *testing.T) {
	for _, total := range []ByteCount{-1, math.MinInt64} {
		for _, scope := range []string{"root", "role", "ordinary", "protected"} {
			func() {
				root, client, send, pack, _, provider := ingressProtectionTestBudgets()
				resized, target := root, send
				switch scope {
				case "role":
					resized, target = client, pack
				case "ordinary":
					resized = send
				case "protected":
					resized, target = pack, pack
				}
				var sourceBytes, targetBytes ByteCount
				t.Cleanup(func() {
					if sourceBytes != 0 {
						provider.Release(sourceBytes)
					}
					if targetBytes != 0 {
						target.Release(targetBytes)
					}
					for _, budget := range []*TransferMemoryBudget{root, client, provider, target} {
						stats := budget.Stats()
						if stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
							t.Errorf("negative-target move ownership did not reconcile: %+v", stats)
						}
					}
				})
				if !provider.TryReserve(100) {
					t.Fatal("source owner did not fit")
				}
				sourceBytes = 100
				if !target.TryReserve(1) {
					t.Fatal("target owner did not fit")
				}
				targetBytes = 1
				resized.SetTotalByteCount(total)
				moved := provider.tryMoveReservation(target, 1)
				if moved {
					sourceBytes--
					targetBytes++
				}
				if want := scope == "root"; moved != want {
					t.Errorf("NEGATIVE_TARGET_MOVE: scope=%s total=%d moved=%t want=%t", scope, total, moved, want)
				}
				if root.UsedByteCount() != 101 {
					t.Error("same-tree move duplicated or released the existing root claim")
				}
			}()
		}
	}
}

// Floor subtraction cannot turn an invalid total into a large window ceiling.
func TestTransferMemoryIngressLendableRejectsNonpositiveResize(t *testing.T) {
	for _, total := range []ByteCount{0, -1, math.MinInt64} {
		budget := NewTransferMemoryBudget(2)
		reserve, release := ingressWaiterTestClaims(t, budget)
		if !reserve(budget, 1) {
			t.Fatal("original owner did not fit")
		}
		budget.addFloor(1)
		t.Cleanup(func() { budget.removeFloor(1) })
		if budget.LendableByteCount(0) != 1 {
			t.Fatal("positive floor-subtracted permission changed")
		}
		budget.SetTotalByteCount(total)
		if available := budget.LendableByteCount(0); available != 0 {
			t.Errorf("NEGATIVE_LENDABLE_OVERFLOW: total=%d available=%d", total, available)
		}
		release(budget, 1)
		stats := budget.Stats()
		if stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
			t.Error("floor-subtraction test owner did not reconcile")
		}
	}
}
