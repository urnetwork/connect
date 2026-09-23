package connect

import (
	"context"
	"testing"
	"time"
)

func requireInactivePlatformHandoffEvidence(t *testing.T, stats PlatformTransportBudgetStats) {
	t.Helper()
	if stats.ActiveHandoffCount == 0 && (stats.ActiveHandoffID != 0 || stats.ActiveHandoffByteCount != 0 ||
		stats.ActiveHandoffTransportCount != 0 || stats.ActiveHandoffH1ByteCount != 0 ||
		stats.ActiveHandoffFromClass != "" || stats.ActiveHandoffToClass != "") {
		t.Fatalf("inactive loan retained evidence: %+v", stats)
	}
}

func requirePlatformHandoffAcquire(t *testing.T, claim *platformTransportBudgetReservation) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if !claim.Acquire(ctx) {
		t.Fatalf("paired carrier did not acquire: %+v", claim.budget.StatsWithRoot())
	}
}

func TestPlatformTransportBudgetStatsWithRootPreservesBorrowingScope(t *testing.T) {
	for _, test := range []struct {
		name                                      string
		rootLimit, childLimit, oldBytes, newBytes ByteCount
		oldClass, newClass                        platformTransportBudgetClass
		rootLoan, childLoan, processOwned         bool
	}{
		{"both levels", 4, 4, 1, 4, platformTransportBudgetH1, platformTransportBudgetH3Explicit, true, true, false},
		{"child only", 8, 4, 1, 4, platformTransportBudgetH1, platformTransportBudgetH3Explicit, false, true, false},
		{"root only device", 4, 8, 1, 4, platformTransportBudgetH1, platformTransportBudgetH3Explicit, true, false, false},
		{"root only process", 4, 4, 1, 4, platformTransportBudgetH1, platformTransportBudgetH3Explicit, true, false, true},
		{"H1 to H1", 1, 1, 1, 1, platformTransportBudgetH1, platformTransportBudgetH1, true, true, false},
		{"H3 to H1", 4, 4, 4, 1, platformTransportBudgetH3Auto, platformTransportBudgetH1, true, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := NewPlatformTransportBudget(test.rootLimit, 4)
			child := newPlatformTransportBudget(test.childLimit, 4, root)
			owner := child
			if test.processOwned {
				owner = root
			}
			old := owner.register(test.oldClass, test.oldBytes, true)
			requirePlatformHierarchyAcquire(t, old)
			defer old.Release()
			next := owner.register(test.newClass, test.newBytes, true)
			defer next.Release()
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if !next.AllowHandoffFrom(old) || !next.Acquire(ctx) {
				t.Fatal("replacement did not acquire")
			}
			snapshot := child.StatsWithRoot()
			if (snapshot.Root.ActiveHandoffCount == 1) != test.rootLoan ||
				(snapshot.Budget.ActiveHandoffCount == 1) != test.childLoan {
				t.Fatalf("snapshot changed borrowing scope: %+v", snapshot)
			}
			pair := snapshot.RootHandoff
			if pair.ID == 0 || pair.FromClass != test.oldClass.name() || pair.ToClass != test.newClass.name() ||
				pair.H1ByteCount != 1 || pair.ByteCount != 1 || pair.TransportCount != 1 {
				t.Fatalf("snapshot lacks explicit H1 pair evidence: %+v", snapshot)
			}
			if test.processOwned {
				if pair.Owner != "process" || snapshot.BudgetHandoff != (PlatformTransportBudgetHandoffStats{}) {
					t.Fatalf("ownerless root pair was attributed to a device: %+v", snapshot)
				}
			} else if pair.Owner != "device" || snapshot.BudgetHandoff != pair {
				t.Fatalf("device/root pair identity is inconsistent: %+v", snapshot)
			}
			for _, stats := range []PlatformTransportBudgetStats{snapshot.Root, snapshot.Budget} {
				requireInactivePlatformHandoffEvidence(t, stats)
				if stats.ActiveHandoffCount == 1 && (stats.ActiveHandoffID != pair.ID ||
					stats.ActiveHandoffFromClass != pair.FromClass || stats.ActiveHandoffToClass != pair.ToClass ||
					stats.ActiveHandoffH1ByteCount != pair.H1ByteCount) {
					t.Fatalf("active loan is not backed by the reported pair: %+v", snapshot)
				}
			}
			old.Release()
			snapshot = child.StatsWithRoot()
			if snapshot.RootHandoff != (PlatformTransportBudgetHandoffStats{}) || snapshot.BudgetHandoff != (PlatformTransportBudgetHandoffStats{}) {
				t.Fatalf("resolved pair retained provenance: %+v", snapshot)
			}
			requireInactivePlatformHandoffEvidence(t, snapshot.Root)
			requireInactivePlatformHandoffEvidence(t, snapshot.Budget)
		})
	}
}

func TestPlatformTransportBudgetStatsWithRootIdentifiesOtherDevice(t *testing.T) {
	root := NewPlatformTransportBudget(4, 2)
	first := newPlatformTransportBudget(8, 2, root)
	second := newPlatformTransportBudget(8, 2, root)
	old := first.register(platformTransportBudgetH1, 1, true)
	requirePlatformHierarchyAcquire(t, old)
	defer old.Release()
	next := first.register(platformTransportBudgetH3Explicit, 4, true)
	defer next.Release()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if !next.AllowHandoffFrom(old) || !next.Acquire(ctx) {
		t.Fatal("replacement did not acquire")
	}
	snapshot := second.StatsWithRoot()
	if snapshot.RootHandoff.Owner != "other_device" || snapshot.BudgetHandoff != (PlatformTransportBudgetHandoffStats{}) {
		t.Fatalf("another device's pair was misattributed: %+v", snapshot)
	}
}

// Independent provider/window family managers can replace H1 at the same
// time. A child loan stays paired until its old carrier drains, even if other
// carriers meanwhile release enough space for a second replacement locally.
// Ordinary ownerless extender pressure can make that second pair borrow only
// at the process root. Both identities must survive the diagnostic bridge.
func TestPlatformTransportBudgetStatsWithRootIndependentFamilyHandoffs(t *testing.T) {
	previousMemoryBudget := MemoryBudget()
	SetMemoryBudget(mib(32))
	defer SetMemoryBudget(previousMemoryBudget)
	settings := DefaultPlatformTransportSettingsWithMemoryTarget(mib(20))
	child, root := settings.PlatformTransportBudget, DefaultPlatformTransportBudget()
	t.Cleanup(func() { requirePlatformHierarchyBalanced(t, root, child) })
	if settings.H1BudgetByteCount != kib(256) || settings.H3BudgetByteCount != mib(3) {
		t.Fatal("regression requires the actual 20/32-MiB carrier profile")
	}
	stopObserver, observerDone := make(chan struct{}), make(chan struct{})
	distinctObserved := make(chan struct{}, 1)
	var badSnapshot *PlatformTransportBudgetHierarchyStats
	go func() {
		defer close(observerDone)
		for {
			snapshot := child.StatsWithRoot()
			if (snapshot.RootAdditionalHandoff.ID != 0 && snapshot.RootAdditionalHandoff != snapshot.BudgetHandoff) ||
				(snapshot.BudgetAdditionalHandoff.ID != 0 && snapshot.BudgetAdditionalHandoff != snapshot.RootHandoff) ||
				(snapshot.RootHandoff.Owner == "device" && snapshot.RootHandoff != snapshot.BudgetHandoff && snapshot.RootHandoff != snapshot.BudgetAdditionalHandoff) ||
				(snapshot.BudgetHandoff.ID != 0 && snapshot.BudgetHandoff != snapshot.RootHandoff && snapshot.BudgetHandoff != snapshot.RootAdditionalHandoff) ||
				snapshot.Root.UsedByteCount != snapshot.Root.ReservedByteCount-snapshot.Root.ReleasedByteCount ||
				snapshot.Root.UsedByteCount > snapshot.Root.TotalByteCount+snapshot.Root.ActiveHandoffByteCount {
				badSnapshot = &snapshot
				return
			}
			if snapshot.RootAdditionalHandoff.ID != 0 {
				select {
				case distinctObserved <- struct{}{}:
				default:
				}
			}
			select {
			case <-stopObserver:
				return
			default:
			}
		}
	}()
	defer func() {
		close(stopObserver)
		<-observerDone
		if badSnapshot != nil {
			t.Errorf("concurrent manager snapshot mixed pair lifetimes: %+v", *badSnapshot)
		}
	}()
	newH1 := func() *PlatformTransport {
		claim := child.register(platformTransportBudgetH1, settings.H1BudgetByteCount, true)
		t.Cleanup(claim.Release)
		return &PlatformTransport{targetMode: TransportModeH1, h1BudgetReservation: claim}
	}
	oldA, oldB := newH1(), newH1()
	requirePlatformHierarchyAcquire(t, oldA.h1BudgetReservation)
	requirePlatformHierarchyAcquire(t, oldB.h1BudgetReservation)
	fillers := make([]*PlatformTransport, 7)
	for i := range fillers {
		fillers[i] = newH1()
		requirePlatformHierarchyAcquire(t, fillers[i].h1BudgetReservation)
	}
	newA := &PlatformTransport{targetMode: TransportModeH3,
		h3BudgetReservation: child.register(platformTransportBudgetH3Explicit, settings.H3BudgetByteCount, true)}
	t.Cleanup(newA.h3BudgetReservation.Release)
	firstOld := &FamilyPlatformTransportGroup{standbyTransport: oldA}
	firstNew := &FamilyPlatformTransportGroup{standbyTransport: newA}
	if !firstNew.CanMakeBeforeBreakFrom(firstOld) {
		t.Fatal("first family manager could not prepare its child handoff")
	}
	requirePlatformHandoffAcquire(t, newA.h3BudgetReservation)
	if child.Stats().ActiveHandoffCount != 1 || root.Stats().ActiveHandoffCount != 0 {
		t.Fatal("first replacement did not borrow only at the child")
	}
	fillers[0].h1BudgetReservation.Release()
	fillers[1].h1BudgetReservation.Release()
	// These are the actual standalone API/feed QUIC policy claims, not fake
	// unowned H1 transports: each is 1,664 KiB and never borrows itself.
	policy := newExtenderQuicMemoryPolicy(t.Context(), DefaultConnectSettings())
	for range 2 {
		claim, err := policy.acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(claim.Release)
	}
	if root.Stats().UsedByteCount != mib(8) {
		t.Fatalf("ordinary ownerless pressure did not fill the root: %+v", root.Stats())
	}
	newB := newH1()
	secondOld := &FamilyPlatformTransportGroup{standbyTransport: oldB}
	secondNew := &FamilyPlatformTransportGroup{standbyTransport: newB}
	if !secondNew.CanMakeBeforeBreakFrom(secondOld) {
		t.Fatal("independent family manager could not prepare H1-to-H1 replacement")
	}
	requirePlatformHandoffAcquire(t, newB.h1BudgetReservation)
	snapshot := child.StatsWithRoot()
	if snapshot.Root.ActiveHandoffCount != 1 || snapshot.Budget.ActiveHandoffCount != 1 ||
		snapshot.RootHandoff.ID == snapshot.BudgetHandoff.ID ||
		snapshot.RootAdditionalHandoff != snapshot.BudgetHandoff ||
		snapshot.BudgetAdditionalHandoff != snapshot.RootHandoff ||
		snapshot.RootHandoff.Owner != "device" || snapshot.BudgetHandoff.Owner != "device" ||
		snapshot.Root.UsedByteCount != mib(8)+kib(256) || snapshot.Budget.UsedByteCount != mib(5) {
		t.Fatalf("overlapping manager loans lost identity or changed capacity: %+v", snapshot)
	}
	select {
	case <-distinctObserved:
	case <-time.After(time.Second):
		t.Fatal("concurrent observer did not see both manager loans")
	}
	oldA.h1BudgetReservation.Release()
	snapshot = child.StatsWithRoot()
	if snapshot.RootHandoff != snapshot.BudgetHandoff || snapshot.RootAdditionalHandoff.ID != 0 || snapshot.BudgetAdditionalHandoff.ID != 0 {
		t.Fatalf("first pair teardown retained extra evidence: %+v", snapshot)
	}
	oldB.h1BudgetReservation.Release()
	snapshot = child.StatsWithRoot()
	if snapshot.RootHandoff.ID != 0 || snapshot.BudgetHandoff.ID != 0 || snapshot.Root.ActiveHandoffCount != 0 || snapshot.Budget.ActiveHandoffCount != 0 {
		t.Fatalf("both pair teardowns did not clear the evidence: %+v", snapshot)
	}
}

func TestPlatformTransportBudgetStatsWithRootConcurrentOwnerlessHandoff(t *testing.T) {
	root := NewPlatformTransportBudget(8, 8)
	child := newPlatformTransportBudget(4, 8, root)
	oldChild := child.register(platformTransportBudgetH1, 1, true)
	requirePlatformHierarchyAcquire(t, oldChild)
	defer oldChild.Release()
	newChild := child.register(platformTransportBudgetH3Explicit, 4, true)
	defer newChild.Release()
	if !newChild.AllowHandoffFrom(oldChild) {
		t.Fatal("child pair refused")
	}
	requirePlatformHandoffAcquire(t, newChild)
	oldRoot := root.register(platformTransportBudgetH1, 1, true)
	requirePlatformHierarchyAcquire(t, oldRoot)
	defer oldRoot.Release()
	newRoot := root.register(platformTransportBudgetH3Explicit, 3, true)
	defer newRoot.Release()
	if !newRoot.AllowHandoffFrom(oldRoot) {
		t.Fatal("ownerless pair refused")
	}
	requirePlatformHandoffAcquire(t, newRoot)
	snapshot := child.StatsWithRoot()
	if snapshot.Root.ActiveHandoffCount != 1 || snapshot.Budget.ActiveHandoffCount != 1 ||
		snapshot.RootHandoff.Owner != "process" || snapshot.BudgetHandoff.Owner != "device" ||
		snapshot.RootHandoff.ID == snapshot.BudgetHandoff.ID ||
		snapshot.RootAdditionalHandoff != snapshot.BudgetHandoff || snapshot.BudgetAdditionalHandoff.ID != 0 ||
		snapshot.Root.UsedByteCount != 9 || snapshot.Root.ActiveHandoffByteCount != 1 {
		t.Fatalf("ownerless root loan was conflated with the child loan: %+v", snapshot)
	}
}

func TestPlatformTransportBudgetStatsWithRootIsAtomicDuringHandoff(t *testing.T) {
	root := NewPlatformTransportBudget(4, 2)
	child := newPlatformTransportBudget(4, 2, root)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 200 {
			old := child.register(platformTransportBudgetH1, 1, true)
			if !old.TryAcquire() {
				t.Error("old carrier could not acquire")
				old.Release()
				return
			}
			next := child.register(platformTransportBudgetH3Explicit, 4, true)
			if !next.AllowHandoffFrom(old) || !next.Acquire(ctx) {
				t.Error("replacement could not acquire")
				next.Release()
				old.Release()
				return
			}
			old.Release()
			next.Release()
		}
	}()
	for {
		snapshot := child.StatsWithRoot()
		if snapshot.Root != snapshot.Budget || snapshot.RootHandoff != snapshot.BudgetHandoff {
			t.Fatalf("snapshot mixed different lifecycle moments: %+v", snapshot)
		}
		select {
		case <-done:
			requirePlatformHierarchyBalanced(t, root, child)
			return
		case <-ctx.Done():
			t.Fatal("atomic snapshot/handoff workers did not complete")
		default:
		}
	}
}
