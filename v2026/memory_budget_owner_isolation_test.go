package connect

import "testing"

// Separate default transports and memory-targeted owners must never share an
// admission root. Exhausting one owner's slot cap cannot park the other.
func TestDefaultPlatformTransportBudgetsAreIndependentOwners(t *testing.T) {
	previous := MemoryBudget()
	SetMemoryBudget(mib(32))
	defer SetMemoryBudget(previous)

	first := DefaultPlatformTransportSettings().PlatformTransportBudget
	second := DefaultPlatformTransportSettings().PlatformTransportBudget
	device := NewPlatformTransportBudgetForMemoryTarget(mib(20))
	if first == nil || second == nil || device == nil {
		t.Fatal("a transport owner received no admission budget")
	}
	if first == second || first == device || second == device ||
		first.root != first || second.root != second || device.root != device {
		t.Fatal("unrelated transport owners share an admission root")
	}

	claims := make([]*platformTransportBudgetReservation, 0, first.Stats().MaxTransportCount)
	for range first.Stats().MaxTransportCount {
		claim := first.register(platformTransportBudgetH1, kib(256), true)
		if !claim.TryAcquire() {
			t.Fatal("first owner failed to fill its own slot cap")
		}
		claims = append(claims, claim)
	}
	blocked := first.register(platformTransportBudgetH1, kib(256), true)
	if blocked.TryAcquire() {
		t.Fatal("first owner bypassed its own slot cap")
	}
	peer := second.register(platformTransportBudgetH1, kib(256), true)
	if !peer.TryAcquire() {
		t.Fatal("first owner saturation blocked an unrelated owner")
	}
	peer.Release()
	blocked.Release()
	for _, claim := range claims {
		claim.Release()
	}
	if first.Stats().UsedTransportCount != 0 || second.Stats().UsedTransportCount != 0 {
		t.Fatal("owner budgets retained claims after release")
	}
}
