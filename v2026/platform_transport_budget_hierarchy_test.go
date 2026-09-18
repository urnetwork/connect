package connect

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"
)

func requirePlatformHierarchyAcquire(t *testing.T, reservation *platformTransportBudgetReservation) {
	t.Helper()
	if !reservation.TryAcquire() {
		t.Fatalf("available hierarchy capacity was refused: %+v", reservation.budget.Stats())
	}
}

func waitPlatformHierarchyAcquire(t *testing.T, acquired <-chan bool) {
	t.Helper()
	select {
	case ok := <-acquired:
		if !ok {
			t.Fatal("hierarchy claim closed instead of acquiring")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("hierarchy claimant was not woken after capacity changed")
	}
}

func requirePlatformHierarchyBalanced(t *testing.T, budgets ...*PlatformTransportBudget) {
	t.Helper()
	for _, budget := range budgets {
		stats := budget.Stats()
		if stats.UsedByteCount != 0 || stats.UsedTransportCount != 0 ||
			stats.PendingH1ByteCount != 0 || stats.PendingH1Count != 0 ||
			stats.ActiveHandoffCount != 0 || stats.PendingHandoffCount != 0 ||
			stats.ReservedByteCount != stats.ReleasedByteCount {
			t.Errorf("hierarchy reservation leaked: %+v", stats)
		}
		budget.root.mutex.Lock()
		registered := len(budget.reservations)
		budget.root.mutex.Unlock()
		if registered != 0 {
			t.Errorf("hierarchy retained %d closed or pending claims", registered)
		}
	}
}

func requirePlatformHierarchyBounded(t *testing.T, budget *PlatformTransportBudget) {
	t.Helper()
	stats := budget.Stats()
	if stats.UsedByteCount < 0 || stats.UsedTransportCount < 0 ||
		stats.ReservedByteCount-stats.ReleasedByteCount != stats.UsedByteCount ||
		stats.ActiveHandoffCount > 1 ||
		stats.UsedByteCount > stats.TotalByteCount+stats.ActiveHandoffByteCount ||
		(0 < stats.MaxTransportCount &&
			stats.UsedTransportCount > stats.MaxTransportCount+stats.ActiveHandoffTransportCount) {
		t.Errorf("hierarchy escaped its byte/carrier-slot bound: %+v", stats)
	}
}

// Actual mobile settings retain their private limit while both devices and an
// API/feed/probe carrier draw from the same process allowance.
func TestPlatformTransportBudgetHierarchyMobileDevicesAndProcessShareCeiling(t *testing.T) {
	previousTarget := MemoryBudget()
	defer SetMemoryBudget(previousTarget)
	SetMemoryBudget(mib(32))
	root := DefaultPlatformTransportBudget()
	firstSettings := DefaultPlatformTransportSettingsWithMemoryTarget(mib(20))
	secondSettings := DefaultPlatformTransportSettingsWithMemoryTarget(mib(20))
	first, second := firstSettings.PlatformTransportBudget, secondSettings.PlatformTransportBudget
	if first == second || first.parent != root || second.parent != root {
		t.Fatal("mobile devices did not retain distinct limits on the same process root")
	}
	if root.Stats().TotalByteCount != mib(8) || root.Stats().MaxTransportCount != 16 ||
		first.Stats().TotalByteCount != mib(5) || second.Stats().TotalByteCount != mib(5) {
		t.Fatalf("mobile profile changed: root=%+v first=%+v second=%+v", root.Stats(), first.Stats(), second.Stats())
	}
	claims := []*platformTransportBudgetReservation{}
	defer func() {
		for _, claim := range claims {
			claim.Release()
		}
	}()
	for _, settings := range []*PlatformTransportSettings{firstSettings, secondSettings} {
		h1 := settings.PlatformTransportBudget.register(platformTransportBudgetH1, settings.H1BudgetByteCount, true)
		requirePlatformHierarchyAcquire(t, h1)
		outer := settings.PlatformTransportBudget.register(platformTransportBudgetExtender, mib(2), false)
		requirePlatformHierarchyAcquire(t, outer)
		claims = append(claims, h1, outer)
	}
	unowned := root.register(platformTransportBudgetExtender, mib(3)+kib(512), true)
	claims = append(claims, unowned)
	requirePlatformHierarchyAcquire(t, unowned)
	if stats := root.Stats(); stats.UsedByteCount != mib(8) || stats.UsedTransportCount != 3 {
		t.Fatalf("process root did not include both devices and unowned carrier: %+v", stats)
	}
	if first.Stats().UsedByteCount != mib(2)+kib(256) || second.Stats().UsedByteCount != mib(2)+kib(256) {
		t.Fatal("root claims were charged to an unrelated private device")
	}

	blocked := first.register(platformTransportBudgetH1, firstSettings.H1BudgetByteCount, true)
	claims = append(claims, blocked)
	before := first.Stats()
	if blocked.TryAcquire() || !blocked.IsWaiting() {
		t.Fatal("private headroom bypassed the process ceiling")
	}
	if after := first.Stats(); after.UsedByteCount != before.UsedByteCount || after.ReservedByteCount != before.ReservedByteCount {
		t.Fatalf("failed composed admission retained partial child capacity: before=%+v after=%+v", before, after)
	}
	wake := first.CapacityNotify()
	if wake != root.CapacityNotify() || wake != second.CapacityNotify() {
		t.Fatal("hierarchy capacity notifications do not share root changes")
	}
	acquired := make(chan bool, 1)
	go func() { acquired <- blocked.Acquire(t.Context()) }()
	unowned.Release()
	select {
	case <-wake:
	default:
		t.Fatal("unowned release did not wake child capacity subscribers")
	}
	waitPlatformHierarchyAcquire(t, acquired)
	for _, claim := range claims {
		claim.Release()
		claim.Release()
	}
	requirePlatformHierarchyBalanced(t, first, second, root)
}

func TestPlatformTransportBudgetHierarchyPreservesProductionOwnership(t *testing.T) {
	previousTarget := MemoryBudget()
	defer SetMemoryBudget(previousTarget)
	for _, processTarget := range []ByteCount{0, -1} {
		t.Run(fmt.Sprintf("process_%d", processTarget), func(t *testing.T) {
			SetMemoryBudget(processTarget)
			root := DefaultPlatformTransportBudget()
			first := NewPlatformTransportBudgetForMemoryTarget(mib(20))
			second := NewPlatformTransportBudgetForMemoryTarget(mib(20))
			if first == second || first.parent != nil || second.parent != nil {
				t.Fatal("normal process profiles lost independent private device budgets")
			}
			for _, budget := range []*PlatformTransportBudget{first, second} {
				claim := budget.register(platformTransportBudgetExtender, mib(5), true)
				requirePlatformHierarchyAcquire(t, claim)
				defer claim.Release()
			}
			if root.Stats().UsedByteCount != 0 {
				t.Fatal("normal private device claims changed legacy process accounting")
			}
			if NewPlatformTransportBudgetForMemoryTarget(0) != root || NewPlatformTransportBudgetForMemoryTarget(-1) != root {
				t.Fatal("disabled owner sizing did not retain the process default")
			}
		})
	}
}

// The tighter Android validation profile and the ordinary Android profile
// use identical admission. Raising the process target must not turn hierarchy
// accounting off and permit a separate API budget beside private devices.
func TestPlatformTransportBudgetHierarchyAllFiniteProcessProfiles(t *testing.T) {
	previousTarget := MemoryBudget()
	defer SetMemoryBudget(previousTarget)
	for _, profile := range []struct {
		deviceTarget  ByteCount
		processTarget ByteCount
	}{
		{mib(20), mib(32)},
		{mib(28), mib(40)},
		{mib(32), mib(64)},
	} {
		t.Run(fmt.Sprintf("device_%d_process_%d", profile.deviceTarget, profile.processTarget), func(t *testing.T) {
			SetMemoryBudget(profile.processTarget)
			root := DefaultPlatformTransportBudget()
			first := NewPlatformTransportBudgetForMemoryTarget(profile.deviceTarget)
			second := NewPlatformTransportBudgetForMemoryTarget(profile.deviceTarget)
			if first.parent != root || second.parent != root || first == second {
				t.Fatal("finite device budgets escaped the shared process root")
			}
			if root.Stats().TotalByteCount != profile.processTarget/4 ||
				first.Stats().TotalByteCount != profile.deviceTarget/4 {
				t.Fatal("hierarchy changed the configured carrier shares")
			}
			for _, budget := range []*PlatformTransportBudget{first, second} {
				claim := budget.register(platformTransportBudgetExtender, profile.deviceTarget/8, true)
				requirePlatformHierarchyAcquire(t, claim)
				defer claim.Release()
			}
			remaining := (profile.processTarget - profile.deviceTarget) / 4
			unowned := root.register(platformTransportBudgetExtender, remaining, true)
			requirePlatformHierarchyAcquire(t, unowned)
			defer unowned.Release()
			if root.Stats().UsedByteCount != profile.processTarget/4 {
				t.Fatal("owned and unowned claims were not aggregated at the process ceiling")
			}
			blocked := second.register(platformTransportBudgetExtender, 1, false)
			defer blocked.Release()
			if blocked.TryAcquire() {
				t.Fatal("finite device admission exceeded the shared process ceiling")
			}
		})
	}
}

func TestPlatformTransportBudgetHierarchyLocalRefusalDoesNotHoldRoot(t *testing.T) {
	root := NewPlatformTransportBudget(8, 4)
	first := newPlatformTransportBudget(5, 3, root)
	second := newPlatformTransportBudget(5, 3, root)
	existing := first.register(platformTransportBudgetExtender, 5, true)
	requirePlatformHierarchyAcquire(t, existing)
	blocked := first.register(platformTransportBudgetExtender, 1, true)
	if blocked.TryAcquire() || !blocked.IsWaiting() {
		t.Fatal("root headroom bypassed the private device limit")
	}
	sibling := second.register(platformTransportBudgetExtender, 3, true)
	requirePlatformHierarchyAcquire(t, sibling)
	if root.Stats().UsedByteCount != 8 || root.Stats().ReservedByteCount != 8 {
		t.Fatal("failed child admission retained process-root capacity")
	}
	blocked.Release()
	sibling.Release()
	existing.Release()
	requirePlatformHierarchyBalanced(t, root, first, second)
}

func TestPlatformTransportBudgetHierarchyCarrierSlotCapAndBidirectionalWake(t *testing.T) {
	root := NewPlatformTransportBudget(100, 2)
	first := newPlatformTransportBudget(100, 2, root)
	second := newPlatformTransportBudget(100, 2, root)
	a := first.register(platformTransportBudgetH1, 1, true)
	b := second.register(platformTransportBudgetH1, 1, true)
	requirePlatformHierarchyAcquire(t, a)
	requirePlatformHierarchyAcquire(t, b)
	unowned := root.register(platformTransportBudgetExtender, 1, true)
	if unowned.TryAcquire() || !unowned.IsWaiting() {
		t.Fatal("unowned carrier bypassed the shared carrier-slot cap")
	}
	acquired := make(chan bool, 1)
	go func() { acquired <- unowned.Acquire(t.Context()) }()
	a.Release()
	waitPlatformHierarchyAcquire(t, acquired)
	if root.Stats().UsedTransportCount != 2 || first.Stats().UsedTransportCount != 0 {
		t.Fatal("child release was not reflected in root carrier-slot admission")
	}
	next := first.register(platformTransportBudgetH1, 1, true)
	if next.TryAcquire() || !next.IsWaiting() {
		t.Fatal("device carrier bypassed the shared carrier-slot cap")
	}
	go func() { acquired <- next.Acquire(t.Context()) }()
	unowned.Release()
	waitPlatformHierarchyAcquire(t, acquired)
	next.Release()
	b.Release()
	requirePlatformHierarchyBalanced(t, root, first, second)
}

func TestPlatformTransportBudgetHierarchyPendingH1Cancellation(t *testing.T) {
	root := NewPlatformTransportBudget(6, 2)
	intermediate := newPlatformTransportBudget(6, 2, root)
	first := newPlatformTransportBudget(6, 2, intermediate)
	second := newPlatformTransportBudget(6, 2, root)
	h1 := first.register(platformTransportBudgetH1, 4, true)
	outer := second.register(platformTransportBudgetExtender, 3, false)
	unowned := root.register(platformTransportBudgetExtender, 3, true)
	if root.Stats().PendingH1ByteCount != 4 || first.Stats().PendingH1ByteCount != 4 {
		t.Fatal("H1 priority did not register at both hierarchy levels")
	}
	if outer.TryAcquire() || unowned.TryAcquire() {
		t.Fatal("carrier consumed capacity promised to another device's H1 claim")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if h1.Acquire(ctx) {
		t.Fatal("already-canceled hierarchy claim acquired")
	}
	if root.Stats().PendingH1Count != 0 || first.Stats().PendingH1Count != 0 {
		t.Fatal("cancellation left H1 priority registered at a hierarchy level")
	}
	requirePlatformHierarchyAcquire(t, outer)
	requirePlatformHierarchyAcquire(t, unowned)
	outer.Release()
	unowned.Release()
	h1.Release()
	requirePlatformHierarchyBalanced(t, root, intermediate, first, second)
}

func TestPlatformTransportBudgetHierarchyRootPreemptionAndYield(t *testing.T) {
	for _, class := range []platformTransportBudgetClass{platformTransportBudgetH1, platformTransportBudgetH3Explicit, platformTransportBudgetH3Auto} {
		for _, processOwned := range []bool{false, true} {
			t.Run(fmt.Sprintf("class_%d_process_%t", class, processOwned), func(t *testing.T) {
				root := NewPlatformTransportBudget(6, 2)
				first := newPlatformTransportBudget(6, 2, root)
				second := newPlatformTransportBudget(6, 2, root)
				auto := first.registerWithPriority(platformTransportBudgetH3Auto, 4, true, PlatformTransportBudgetPriorityBackground)
				requirePlatformHierarchyAcquire(t, auto)
				preempt := auto.PreemptNotify()
				claimantBudget := second
				if processOwned {
					claimantBudget = root
				}
				claim := claimantBudget.register(class, 4, true)
				acquired := make(chan bool, 1)
				go func() { acquired <- claim.Acquire(t.Context()) }()
				select {
				case <-preempt:
				case <-time.After(2 * time.Second):
					t.Fatal("root pressure did not reach the private Auto-H3 owner")
				}
				if root.Stats().UsedByteCount != 4 || first.Stats().UsedByteCount != 4 || second.Stats().UsedByteCount != 0 {
					t.Fatal("preemption released accounting before carrier teardown")
				}
				if !auto.Yield() || auto.Yield() {
					t.Fatal("optional hierarchy claim did not yield exactly once")
				}
				waitPlatformHierarchyAcquire(t, acquired)
				if auto.TryAcquire() {
					t.Fatal("yielded Auto H3 displaced the higher-priority claimant")
				}
				if auto.PreemptNotify() == preempt || first.Stats().PreemptedH3Count != 1 || root.Stats().PreemptedH3Count != 1 {
					t.Fatal("hierarchy did not reset and account one shared preemption signal")
				}
				claim.Release()
				requirePlatformHierarchyAcquire(t, auto)
				select {
				case <-auto.PreemptNotify():
					t.Fatal("reacquired Auto H3 retained a closed preemption channel")
				default:
				}
				auto.Release()
				requirePlatformHierarchyBalanced(t, root, first, second)
			})
		}
	}
}

func TestPlatformTransportBudgetHierarchyOneYieldSatisfiesBothDeficits(t *testing.T) {
	for _, rootLimit := range []ByteCount{6, 9} {
		t.Run(fmt.Sprintf("root_%d", rootLimit), func(t *testing.T) {
			root := NewPlatformTransportBudget(rootLimit, 4)
			child := newPlatformTransportBudget(3, 2, root)
			// Register the unrelated carrier first, making it the first victim
			// root ordering would choose if it ignored the child's revocation.
			sibling := root.register(platformTransportBudgetH3Auto, 3, true)
			requirePlatformHierarchyAcquire(t, sibling)
			auto := child.register(platformTransportBudgetH3Auto, 3, true)
			requirePlatformHierarchyAcquire(t, auto)
			h1 := child.register(platformTransportBudgetH1, 1, true)
			acquired := make(chan bool, 1)
			go func() { acquired <- h1.Acquire(t.Context()) }()
			select {
			case <-auto.PreemptNotify():
			case <-time.After(2 * time.Second):
				t.Fatal("local byte deficit did not revoke the local Auto carrier")
			}
			// Take the root lock through Stats to observe the completed
			// admission attempt, including its parent preemption decision.
			if stats := root.Stats(); stats.PreemptedH3Count != 1 || stats.UsedByteCount != 6 {
				t.Fatalf("one local yield should cover both deficits: %+v", stats)
			}
			select {
			case <-sibling.PreemptNotify():
				t.Fatal("root redundantly revoked an unrelated carrier while the child drained")
			default:
			}
			if !auto.Yield() {
				t.Fatal("local Auto carrier did not yield")
			}
			waitPlatformHierarchyAcquire(t, acquired)
			h1.Release()
			auto.Release()
			sibling.Release()
			requirePlatformHierarchyBalanced(t, root, child)
		})
	}
}

func TestPlatformTransportBudgetHierarchyUnresolvableH1DoesNotPreempt(t *testing.T) {
	for _, blockedAtRoot := range []bool{false, true} {
		t.Run(fmt.Sprintf("blocked_at_root_%t", blockedAtRoot), func(t *testing.T) {
			root := NewPlatformTransportBudget(5, 2)
			child := newPlatformTransportBudget(3, 1, root)
			autoBudget, autoBytes := root, ByteCount(4)
			if blockedAtRoot {
				root = NewPlatformTransportBudget(3, 1)
				child = newPlatformTransportBudget(3, 2, root)
				autoBudget, autoBytes = child, 2
			}
			fixedH1 := child.register(platformTransportBudgetH1, 1, true)
			requirePlatformHierarchyAcquire(t, fixedH1)
			auto := autoBudget.register(platformTransportBudgetH3Auto, autoBytes, false)
			requirePlatformHierarchyAcquire(t, auto)
			blockedH1 := child.register(platformTransportBudgetH1, 1, true)
			root.mutex.Lock()
			blockedH1.requestPreemptionLocked()
			root.mutex.Unlock()
			select {
			case <-auto.PreemptNotify():
				t.Fatal("an impossible H1 claim revoked a carrier at another hierarchy level")
			default:
			}
			if root.Stats().PreemptedH3Count != 0 || child.Stats().PreemptedH3Count != 0 {
				t.Fatal("an incomplete hierarchy preemption plan was partially applied")
			}
			blockedH1.Release()
			auto.Release()
			fixedH1.Release()
			requirePlatformHierarchyBalanced(t, root, child)
		})
	}
}

func TestPlatformTransportBudgetHierarchySerializesDeviceHandoffsAtRoot(t *testing.T) {
	root := NewPlatformTransportBudget(10, 2)
	first := newPlatformTransportBudget(10, 1, root)
	second := newPlatformTransportBudget(10, 1, root)
	oldA := first.register(platformTransportBudgetH1, 1, true)
	oldB := second.register(platformTransportBudgetH1, 1, true)
	requirePlatformHierarchyAcquire(t, oldA)
	requirePlatformHierarchyAcquire(t, oldB)
	nextA := first.register(platformTransportBudgetH3Explicit, 5, true)
	nextB := second.register(platformTransportBudgetH3Explicit, 5, true)
	if !nextA.AllowHandoffFrom(oldA) || !nextB.AllowHandoffFrom(oldB) {
		t.Fatal("device replacements could not pair their previous reservations")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if !nextA.Acquire(ctx) {
		t.Fatal("first composed handoff did not acquire")
	}
	if nextB.TryAcquire() || !nextB.IsWaiting() {
		t.Fatal("second device bypassed the process-root handoff serialization")
	}
	if stats := root.Stats(); stats.ActiveHandoffCount != 1 || stats.PendingHandoffCount != 1 ||
		stats.UsedByteCount != 7 || stats.UsedTransportCount != 3 ||
		stats.ActiveHandoffByteCount != 1 || stats.ActiveHandoffTransportCount != 1 {
		t.Fatalf("first process handoff accounting = %+v", stats)
	}
	acquired := make(chan bool, 1)
	go func() { acquired <- nextB.Acquire(ctx) }()
	oldA.Release()
	waitPlatformHierarchyAcquire(t, acquired)
	if stats := root.Stats(); stats.ActiveHandoffCount != 1 || stats.PendingHandoffCount != 0 ||
		stats.UsedByteCount != 11 || stats.UsedTransportCount != 3 || stats.HandoffAcquisitionCount != 2 {
		t.Fatalf("second process handoff accounting = %+v", stats)
	}
	for _, budget := range []*PlatformTransportBudget{root, first, second} {
		requirePlatformHierarchyBounded(t, budget)
	}
	oldB.Release()
	nextA.Release()
	nextB.Release()
	requirePlatformHierarchyBalanced(t, root, first, second)
}

func TestPlatformTransportBudgetHierarchyHandoffCleanupAtEitherLevel(t *testing.T) {
	for _, rootLimited := range []bool{false, true} {
		for _, childLimited := range []bool{false, true} {
			for _, releaseOld := range []bool{false, true} {
				t.Run(fmt.Sprintf("root_%t_child_%t_release_old_%t", rootLimited, childLimited, releaseOld), func(t *testing.T) {
					rootLimit, childLimit := ByteCount(8), ByteCount(8)
					if rootLimited {
						rootLimit = 4
					}
					if childLimited {
						childLimit = 4
					}
					root := NewPlatformTransportBudget(rootLimit, 4)
					child := newPlatformTransportBudget(childLimit, 4, root)
					old := child.register(platformTransportBudgetH1, 1, true)
					requirePlatformHierarchyAcquire(t, old)
					next := child.register(platformTransportBudgetH3Explicit, 4, true)
					if !next.AllowHandoffFrom(old) {
						t.Fatal("hierarchy pair was refused")
					}
					ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
					defer cancel()
					if !next.Acquire(ctx) {
						t.Fatal("composed handoff did not acquire")
					}
					if (root.Stats().ActiveHandoffCount == 1) != rootLimited ||
						(child.Stats().ActiveHandoffCount == 1) != childLimited {
						t.Fatalf("loans were not scoped to constrained levels: root=%+v child=%+v", root.Stats(), child.Stats())
					}
					requirePlatformHierarchyBounded(t, root)
					requirePlatformHierarchyBounded(t, child)
					if releaseOld {
						old.Release()
					} else {
						next.Release()
					}
					if root.Stats().ActiveHandoffCount != 0 || child.Stats().ActiveHandoffCount != 0 {
						t.Fatal("releasing one endpoint retained a hierarchical handoff")
					}
					old.Release()
					next.Release()
					requirePlatformHierarchyBalanced(t, root, child)
				})
			}
		}
	}
}

func TestPlatformTransportBudgetHierarchyHandoffPairingIsAtomic(t *testing.T) {
	root := NewPlatformTransportBudget(4, 3)
	child := newPlatformTransportBudget(8, 3, root)
	old := child.register(platformTransportBudgetH1, 1, true)
	requirePlatformHierarchyAcquire(t, old)
	next := child.register(platformTransportBudgetH3Explicit, 4, true)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if !next.AllowHandoffFrom(old) || !next.Acquire(ctx) {
		t.Fatal("root-only handoff did not acquire")
	}
	// The unconstrained child discarded its unused pair, but the root still
	// uses it. A second pairing must fail without leaving a child-only link.
	second := child.register(platformTransportBudgetH1, 1, true)
	if second.AllowHandoffFrom(old) {
		t.Fatal("a second replacement borrowed an active root handoff endpoint")
	}
	if child.Stats().PendingHandoffCount != 0 || root.Stats().PendingHandoffCount != 0 {
		t.Fatal("refused root pairing left a partial child handoff")
	}
	second.Release()
	if root.Stats().ActiveHandoffCount != 1 {
		t.Fatal("refused replacement disturbed the existing root handoff")
	}
	next.Release()
	old.Release()
	requirePlatformHierarchyBalanced(t, root, child)
}

func TestPlatformTransportBudgetHierarchyCanceledHandoffUnpairsEveryLevel(t *testing.T) {
	root := NewPlatformTransportBudget(4, 1)
	child := newPlatformTransportBudget(4, 1, root)
	old := child.register(platformTransportBudgetH1, 1, true)
	requirePlatformHierarchyAcquire(t, old)
	next := child.register(platformTransportBudgetH3Explicit, 4, true)
	if !next.AllowHandoffFrom(old) {
		t.Fatal("H1 handoff did not pair")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if next.Acquire(ctx) {
		t.Fatal("canceled handoff acquired")
	}
	for _, budget := range []*PlatformTransportBudget{root, child} {
		if stats := budget.Stats(); stats.ActiveHandoffCount != 0 || stats.PendingHandoffCount != 0 || stats.UsedByteCount != 1 {
			t.Fatalf("cancellation retained a handoff or changed the old carrier: %+v", stats)
		}
	}
	next.Release()
	old.Release()
	requirePlatformHierarchyBalanced(t, root, child)
}

func TestPlatformTransportBudgetHierarchyYieldClearsEitherHandoffEndpoint(t *testing.T) {
	for _, yieldOld := range []bool{false, true} {
		t.Run(fmt.Sprintf("yield_old_%t", yieldOld), func(t *testing.T) {
			root := NewPlatformTransportBudget(4, 1)
			child := newPlatformTransportBudget(4, 1, root)
			oldClass, newClass := platformTransportBudgetH1, platformTransportBudgetH3Auto
			oldBytes, newBytes := ByteCount(1), ByteCount(4)
			if yieldOld {
				oldClass, newClass = newClass, oldClass
				oldBytes, newBytes = newBytes, oldBytes
			}
			old := child.register(oldClass, oldBytes, true)
			requirePlatformHierarchyAcquire(t, old)
			next := child.register(newClass, newBytes, true)
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			if !next.AllowHandoffFrom(old) || !next.Acquire(ctx) {
				t.Fatal("H1/Auto handoff did not acquire at both levels")
			}
			toYield := next
			if yieldOld {
				toYield = old
			}
			if !toYield.Yield() {
				t.Fatal("Auto handoff endpoint did not yield")
			}
			if root.Stats().ActiveHandoffCount != 0 || child.Stats().ActiveHandoffCount != 0 {
				t.Fatal("yield retained a handoff at a hierarchy level")
			}
			old.Release()
			next.Release()
			requirePlatformHierarchyBalanced(t, root, child)
		})
	}
}

func TestPlatformTransportBudgetHierarchyConcurrentAdmissionAndCleanup(t *testing.T) {
	root := NewPlatformTransportBudget(48, 6)
	budgets := []*PlatformTransportBudget{root}
	for range 4 {
		budgets = append(budgets, newPlatformTransportBudget(24, 3, root))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var workers sync.WaitGroup
	for worker := range 20 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			budget := budgets[worker%len(budgets)]
			for iteration := range 80 {
				class := platformTransportBudgetH1
				if iteration%3 == 0 {
					class = platformTransportBudgetExtender
				}
				claim := budget.register(class, 8, true)
				if iteration%2 == 0 || class == platformTransportBudgetExtender {
					claim.TryAcquire()
				} else if !claim.Acquire(ctx) {
					t.Error("concurrent H1 claimant failed to make progress")
					return
				}
				requirePlatformHierarchyBounded(t, root)
				requirePlatformHierarchyBounded(t, budget)
				runtime.Gosched()
				claim.Release()
				claim.Release()
			}
		}()
	}
	workers.Wait()
	requirePlatformHierarchyBalanced(t, budgets...)
}

func TestPlatformTransportBudgetHierarchyConcurrentAcquireYieldRelease(t *testing.T) {
	root := NewPlatformTransportBudget(8, 2)
	child := newPlatformTransportBudget(8, 2, root)
	for range 100 {
		claim := child.register(platformTransportBudgetH3Auto, 4, true)
		requirePlatformHierarchyAcquire(t, claim)
		var workers sync.WaitGroup
		for action := range 4 {
			workers.Add(1)
			go func() {
				defer workers.Done()
				switch action {
				case 0:
					claim.Acquire(t.Context())
				case 1:
					claim.Yield()
				default:
					claim.Release()
				}
				claim.IsWaiting()
				claim.PreemptNotify()
				child.CapacityNotify()
				requirePlatformHierarchyBounded(t, root)
			}()
		}
		workers.Wait()
		claim.Release()
	}
	requirePlatformHierarchyBalanced(t, root, child)
}
