// Extreme claim sizes exercise real budget admission and exact lifecycle
// accounting without allocating the claimed payloads or starting transports.
package connect

import (
	"context"
	"math"
	"testing"
	"testing/synctest"
)

// requirePlatformBudgetOverflowEmpty checks live state independently of the
// cumulative counters, which intentionally saturate after MaxInt64 bytes.
func requirePlatformBudgetOverflowEmpty(t *testing.T, budgets ...*PlatformTransportBudget) {
	t.Helper()
	for _, budget := range budgets {
		stats := budget.Stats()
		if stats.UsedByteCount != 0 || stats.UsedTransportCount != 0 ||
			stats.PendingH1ByteCount != 0 || stats.PendingH1Count != 0 ||
			stats.ActiveHandoffCount != 0 || stats.PendingHandoffCount != 0 ||
			stats.ReservedByteCount < 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
			t.Fatalf("overflow lifecycle did not drain: %+v", stats)
		}
		budget.root.mutex.Lock()
		registered := len(budget.reservations)
		budget.root.mutex.Unlock()
		if registered != 0 {
			t.Fatalf("overflow lifecycle retained %d reservations", registered)
		}
	}
}

// One live byte plus MaxInt64 used to wrap negative and admit a huge claim
// against a finite owner. Every claim class must reject that sum, including
// an owner whose legitimate limit is exactly MaxInt64.
func TestPlatformTransportBudgetOverflowLiveClaimRejected(t *testing.T) {
	for _, limit := range []ByteCount{8, math.MaxInt64} {
		for _, class := range []platformTransportBudgetClass{
			platformTransportBudgetH1,
			platformTransportBudgetH3Auto,
			platformTransportBudgetH3Explicit,
			platformTransportBudgetExtender,
		} {
			budget := NewPlatformTransportBudget(limit, 0)
			healthy := budget.register(platformTransportBudgetH3Explicit, 1, false)
			defer healthy.Release()
			if !healthy.TryAcquire() {
				t.Fatal("healthy one-byte claim did not acquire")
			}
			huge := budget.register(class, math.MaxInt64, false)
			defer huge.Release()
			if huge.TryAcquire() || !huge.IsWaiting() {
				t.Fatalf("class %s bypassed limit %d with a wrapped live sum", class.name(), limit)
			}
			if stats := budget.Stats(); stats.UsedByteCount != 1 || stats.ReservedByteCount != 1 {
				t.Fatalf("rejected huge claim changed live accounting: %+v", stats)
			}
			huge.Release()
			healthy.Release()
			requirePlatformBudgetOverflowEmpty(t, budget)
		}
	}
}

// A child may fit MaxInt64 exactly while an unrelated root claim makes the
// ancestor sum overflow. Refusal must acquire neither level, and removing
// the unrelated byte must make the unchanged legitimate claim admissible.
func TestPlatformTransportBudgetOverflowRootRefusalIsAtomic(t *testing.T) {
	root := NewPlatformTransportBudget(math.MaxInt64, 0)
	child := newPlatformTransportBudget(math.MaxInt64, 0, root)
	peer := root.register(platformTransportBudgetH3Explicit, 1, false)
	defer peer.Release()
	if !peer.TryAcquire() {
		t.Fatal("root peer did not acquire")
	}
	huge := child.register(platformTransportBudgetH1, math.MaxInt64, false)
	defer huge.Release()
	if huge.TryAcquire() || !huge.IsWaiting() {
		t.Fatal("child bypassed an overflowed ancestor limit")
	}
	if child.Stats().UsedByteCount != 0 || root.Stats().UsedByteCount != 1 ||
		child.Stats().ReservedByteCount != 0 {
		t.Fatal("root refusal partially acquired the child")
	}
	peer.Release()
	if !huge.TryAcquire() || child.Stats().UsedByteCount != math.MaxInt64 ||
		root.Stats().UsedByteCount != math.MaxInt64 {
		t.Fatal("an exact MaxInt64 claim was refused after the peer drained")
	}
	huge.Release()
	requirePlatformBudgetOverflowEmpty(t, child, root)
}

// Three huge pending H1 claims exceed even uint64. Canceling them one at a
// time must retain the exact remaining demand, including a small survivor;
// saturating a stored total and then subtracting would lose that survivor.
func TestPlatformTransportBudgetOverflowPendingCancellationRetainsClaims(t *testing.T) {
	cases := []struct {
		name     string
		cap      int
		usesSlot bool
	}{
		{name: "uncapped slotless"},
		{name: "capped slotless", cap: 1},
		{name: "capped slotted", cap: 4, usesSlot: true},
	}
	for _, test := range cases {
		for _, class := range []platformTransportBudgetClass{
			platformTransportBudgetH3Auto,
			platformTransportBudgetExtender,
		} {
			root := NewPlatformTransportBudget(math.MaxInt64, test.cap)
			budget := newPlatformTransportBudget(math.MaxInt64, test.cap, root)
			pending := []*platformTransportBudgetReservation{}
			for range 3 {
				claim := budget.register(platformTransportBudgetH1, math.MaxInt64, test.usesSlot)
				defer claim.Release()
				pending = append(pending, claim)
			}
			survivor := budget.register(platformTransportBudgetH1, 5, test.usesSlot)
			defer survivor.Release()
			optional := budget.register(class, 1, false)
			defer optional.Release()
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			for index, claim := range pending {
				if optional.TryAcquire() || !optional.IsWaiting() {
					t.Fatalf("%s/%s ignored %d huge pending claims", test.name, class.name(), len(pending)-index)
				}
				for _, owner := range []*PlatformTransportBudget{budget, root} {
					if stats := owner.Stats(); stats.PendingH1ByteCount != math.MaxInt64 || stats.UsedByteCount != 0 {
						t.Fatalf("%s pending projection wrapped: %+v", test.name, stats)
					}
				}
				if claim.Acquire(ctx) {
					t.Fatal("already-canceled huge claim acquired")
				}
			}
			if budget.Stats().PendingH1ByteCount != 5 || root.Stats().PendingH1ByteCount != 5 {
				t.Fatalf("%s cancellation lost the small remaining claim", test.name)
			}
			if !optional.TryAcquire() {
				t.Fatalf("%s/%s retained phantom overflow after cancellation", test.name, class.name())
			}
			survivor.Release()
			optional.Release()
			requirePlatformBudgetOverflowEmpty(t, budget, root)
		}
	}
}

// A full carrier-slot cap still excludes structurally blocked H1 demand
// from a slotless Auto-H3 migration, even when the pending byte sum is huge.
func TestPlatformTransportBudgetOverflowKeepsPendingSlotFilter(t *testing.T) {
	budget := NewPlatformTransportBudget(2, 1)
	current := budget.register(platformTransportBudgetH1, 1, true)
	defer current.Release()
	if !current.TryAcquire() {
		t.Fatal("current carrier did not fill the slot cap")
	}
	pending := []*platformTransportBudgetReservation{}
	for range 3 {
		claim := budget.register(platformTransportBudgetH1, math.MaxInt64, true)
		defer claim.Release()
		pending = append(pending, claim)
	}
	optional := budget.register(platformTransportBudgetH3Auto, 1, false)
	defer optional.Release()
	if !optional.TryAcquire() || budget.Stats().UsedByteCount != 2 {
		t.Fatal("slot-excluded huge H1 demand blocked slotless Auto H3")
	}
	for _, claim := range pending {
		claim.Release()
	}
	optional.Release()
	current.Release()
	requirePlatformBudgetOverflowEmpty(t, budget)
}

// Acquiring, not just canceling, must remove exactly one pending huge claim.
// Repeated legitimate MaxInt64 leases also must not wrap lifetime counters.
func TestPlatformTransportBudgetOverflowPendingAcquisitionAndLifetimeCounters(t *testing.T) {
	budget := NewPlatformTransportBudget(math.MaxInt64, 0)
	claims := []*platformTransportBudgetReservation{}
	for range 3 {
		claim := budget.register(platformTransportBudgetH1, math.MaxInt64, false)
		defer claim.Release()
		claims = append(claims, claim)
	}
	for index, claim := range claims {
		if !claim.TryAcquire() {
			t.Fatalf("legitimate MaxInt64 lease %d did not acquire", index)
		}
		wantPending := ByteCount(math.MaxInt64)
		if index == len(claims)-1 {
			wantPending = 0
		}
		stats := budget.Stats()
		if stats.UsedByteCount != math.MaxInt64 || stats.PendingH1ByteCount != wantPending ||
			stats.ReservedByteCount != math.MaxInt64 {
			t.Fatalf("lease %d corrupted exact live/pending or saturated cumulative accounting: %+v", index, stats)
		}
		claim.Release()
		if stats := budget.Stats(); stats.UsedByteCount != 0 || stats.ReleasedByteCount != math.MaxInt64 {
			t.Fatalf("lease %d release wrapped lifetime accounting: %+v", index, stats)
		}
	}
	requirePlatformBudgetOverflowEmpty(t, budget)
}

// Reserving one higher-precedence H3 claim must not wrap the optional or H1
// claimant's own bytes; this path is separate from pending-H1 aggregation.
func TestPlatformTransportBudgetOverflowHigherPriorityDemand(t *testing.T) {
	cases := []struct {
		class       platformTransportBudgetClass
		higherClass platformTransportBudgetClass
	}{
		{class: platformTransportBudgetH1, higherClass: platformTransportBudgetH3Explicit},
		{class: platformTransportBudgetH3Auto, higherClass: platformTransportBudgetH3Explicit},
		{class: platformTransportBudgetExtender, higherClass: platformTransportBudgetH3Explicit},
		{class: platformTransportBudgetH3Auto, higherClass: platformTransportBudgetH3Auto},
	}
	for _, test := range cases {
		budget := NewPlatformTransportBudget(math.MaxInt64, 0)
		higher := budget.registerWithPriority(test.higherClass, math.MaxInt64, false, PlatformTransportBudgetPriorityForeground)
		defer higher.Release()
		claim := budget.registerWithPriority(test.class, 1, false, PlatformTransportBudgetPriorityBackground)
		defer claim.Release()
		if claim.TryAcquire() || !claim.IsWaiting() {
			t.Fatalf("%s ignored huge higher-priority %s demand", test.class.name(), test.higherClass.name())
		}
		if !higher.TryAcquire() || claim.TryAcquire() {
			t.Fatal("higher-priority MaxInt64 claim did not retain exclusive capacity")
		}
		higher.Release()
		if !claim.TryAcquire() {
			t.Fatal("lower-priority claim did not recover after the huge claim drained")
		}
		claim.Release()
		requirePlatformBudgetOverflowEmpty(t, budget)
	}
}

// An exact MaxInt64 -> MaxInt64 H1 handoff is legal, but its temporary 2*Max
// live sum is not ordinary headroom. Either endpoint's release must leave
// the other Max-sized claim charged at both hierarchy levels.
func TestPlatformTransportBudgetOverflowHandoffReleasesEitherEndpoint(t *testing.T) {
	cases := []struct {
		oldClass platformTransportBudgetClass
		newClass platformTransportBudgetClass
	}{
		{oldClass: platformTransportBudgetH1, newClass: platformTransportBudgetH3Explicit},
		{oldClass: platformTransportBudgetH3Explicit, newClass: platformTransportBudgetH1},
		{oldClass: platformTransportBudgetH1, newClass: platformTransportBudgetH1},
	}
	for _, test := range cases {
		for _, releaseOld := range []bool{false, true} {
			root := NewPlatformTransportBudget(math.MaxInt64, 0)
			budget := newPlatformTransportBudget(math.MaxInt64, 0, root)
			old := budget.register(test.oldClass, math.MaxInt64, false)
			defer old.Release()
			if !old.TryAcquire() {
				t.Fatal("old Max-sized carrier did not acquire")
			}
			next := budget.register(test.newClass, math.MaxInt64, false)
			defer next.Release()
			if !next.AllowHandoffFrom(old) || next.IsWaiting() {
				t.Fatal("legitimate Max-sized replacement did not receive a bounded handoff")
			}
			if next.TryAcquire() {
				t.Fatal("ordinary admission treated the handoff overflow as headroom")
			}
			if !next.Acquire(t.Context()) {
				t.Fatal("legal Max-sized handoff did not acquire")
			}
			for _, owner := range []*PlatformTransportBudget{budget, root} {
				stats := owner.Stats()
				if stats.UsedByteCount != math.MaxInt64 || stats.ActiveHandoffCount != 1 ||
					stats.ActiveHandoffByteCount != math.MaxInt64 || stats.HandoffAcquisitionCount != 1 {
					t.Fatalf("Max-sized handoff lost its exact overage/provenance: %+v", stats)
				}
			}
			probe := budget.register(platformTransportBudgetH3Explicit, 0, false)
			defer probe.Release()
			if probe.TryAcquire() || !probe.IsWaiting() {
				t.Fatal("even a zero-byte ordinary claim must wait while live usage exceeds the limit")
			}
			if releaseOld {
				old.Release()
			} else {
				next.Release()
			}
			for _, owner := range []*PlatformTransportBudget{budget, root} {
				stats := owner.Stats()
				if stats.UsedByteCount != math.MaxInt64 || stats.ActiveHandoffCount != 0 {
					t.Fatalf("handoff endpoint release lost the surviving Max-sized carrier: %+v", stats)
				}
			}
			if !probe.TryAcquire() {
				t.Fatal("resolved handoff retained phantom overflow")
			}
			probe.Release()
			old.Release()
			next.Release()
			requirePlatformBudgetOverflowEmpty(t, budget, root)
		}
	}
}

// Yield follows a different release path. Returning either Auto-H3 endpoint
// must retain its H1 peer exactly, clear the loan, and allow a later reacquire.
func TestPlatformTransportBudgetOverflowHandoffYieldRetainsPeer(t *testing.T) {
	for _, yieldOld := range []bool{false, true} {
		root := NewPlatformTransportBudget(math.MaxInt64, 0)
		budget := newPlatformTransportBudget(math.MaxInt64, 0, root)
		oldClass, newClass := platformTransportBudgetH1, platformTransportBudgetH3Auto
		if yieldOld {
			oldClass, newClass = newClass, oldClass
		}
		old := budget.register(oldClass, math.MaxInt64, false)
		defer old.Release()
		if !old.TryAcquire() {
			t.Fatal("old handoff carrier did not acquire")
		}
		next := budget.register(newClass, math.MaxInt64, false)
		defer next.Release()
		if !next.AllowHandoffFrom(old) || next.IsWaiting() || !next.Acquire(t.Context()) {
			t.Fatal("Auto-H3/H1 Max-sized handoff did not acquire")
		}
		auto, h1 := next, old
		if yieldOld {
			auto, h1 = old, next
		}
		if !auto.Yield() {
			t.Fatal("Auto-H3 handoff endpoint did not yield")
		}
		for _, owner := range []*PlatformTransportBudget{budget, root} {
			stats := owner.Stats()
			if stats.UsedByteCount != math.MaxInt64 || stats.ActiveHandoffCount != 0 ||
				stats.ReleasedByteCount != math.MaxInt64 {
				t.Fatalf("yield lost the retained H1 claim: %+v", stats)
			}
		}
		if auto.TryAcquire() {
			t.Fatal("yielded Auto H3 bypassed its still-live Max-sized H1 peer")
		}
		h1.Release()
		if !auto.TryAcquire() || !auto.Yield() {
			t.Fatal("Auto H3 could not reacquire and return the released Max-sized capacity")
		}
		auto.Release()
		requirePlatformBudgetOverflowEmpty(t, budget, root)
	}
}

// Discounting a one-byte old carrier cannot make two unrelated Max-sized
// demands fit. Subtracting from a saturated total would incorrectly allow it.
func TestPlatformTransportBudgetOverflowHandoffDoesNotEraseUnrelatedDemand(t *testing.T) {
	budget := NewPlatformTransportBudget(math.MaxInt64, 0)
	unrelated := budget.register(platformTransportBudgetH3Explicit, math.MaxInt64-1, false)
	defer unrelated.Release()
	if !unrelated.TryAcquire() {
		t.Fatal("unrelated carrier did not acquire")
	}
	old := budget.register(platformTransportBudgetH1, 1, false)
	defer old.Release()
	if !old.TryAcquire() {
		t.Fatal("old H1 carrier did not acquire its last byte")
	}
	next := budget.register(platformTransportBudgetH3Explicit, math.MaxInt64, false)
	defer next.Release()
	if !next.AllowHandoffFrom(old) || !next.IsWaiting() || next.TryAcquire() {
		t.Fatal("handoff discount erased unrelated live demand")
	}
	if stats := budget.Stats(); stats.UsedByteCount != math.MaxInt64 || stats.ActiveHandoffCount != 0 {
		t.Fatalf("refused handoff mutated live accounting: %+v", stats)
	}
	next.Release()
	old.Release()
	unrelated.Release()
	requirePlatformBudgetOverflowEmpty(t, budget)
}

// Overflowed demand can still have a sufficient preemption plan. The H1
// claimant must wait for the actual Auto-H3 Yield rather than acquiring a
// wrapped sum or permanently rejecting a legitimate MaxInt64 replacement.
func TestPlatformTransportBudgetOverflowPreemptionWaitsForYield(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		root := NewPlatformTransportBudget(math.MaxInt64, 0)
		budget := newPlatformTransportBudget(math.MaxInt64, 0, root)
		auto := budget.register(platformTransportBudgetH3Auto, math.MaxInt64, false)
		defer auto.Release()
		if !auto.TryAcquire() {
			t.Fatal("Auto H3 did not acquire its Max-sized lease")
		}
		h1 := budget.register(platformTransportBudgetH1, math.MaxInt64, false)
		defer h1.Release()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := make(chan bool, 1)
		go func() { result <- h1.Acquire(ctx) }()
		synctest.Wait()
		select {
		case <-result:
			t.Fatal("H1 acquired before its Max-sized victim yielded")
		default:
		}
		select {
		case <-auto.PreemptNotify():
		default:
			t.Fatal("overflowed but resolvable deficit did not request preemption")
		}
		if budget.Stats().PreemptedH3Count != 1 || root.Stats().PreemptedH3Count != 1 ||
			budget.Stats().UsedByteCount != math.MaxInt64 {
			t.Fatal("hierarchy double-preempted or prematurely charged the replacement")
		}
		if !auto.Yield() {
			t.Fatal("preempted Auto H3 did not yield")
		}
		synctest.Wait()
		select {
		case acquired := <-result:
			if !acquired {
				t.Fatal("H1 closed instead of acquiring the released capacity")
			}
		default:
			t.Fatal("H1 remained blocked after its victim yielded")
		}
		h1.Release()
		if !auto.TryAcquire() || !auto.Yield() {
			t.Fatal("preempted Auto H3 did not recover after H1 released")
		}
		auto.Release()
		requirePlatformBudgetOverflowEmpty(t, budget, root)
	})
}

// The child deficit is resolvable, but an unrelated nonpreemptible root byte
// prevents the MaxInt64 H1 claim from ever fitting. No victim may be revoked
// at either level, and cancellation must clear both pending copies exactly.
func TestPlatformTransportBudgetOverflowImpossibleRootDoesNotPreemptChild(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		root := NewPlatformTransportBudget(math.MaxInt64, 0)
		budget := newPlatformTransportBudget(math.MaxInt64, 0, root)
		peer := root.register(platformTransportBudgetH3Explicit, 1, false)
		defer peer.Release()
		if !peer.TryAcquire() {
			t.Fatal("nonpreemptible root peer did not acquire")
		}
		auto := budget.register(platformTransportBudgetH3Auto, math.MaxInt64-1, false)
		defer auto.Release()
		if !auto.TryAcquire() {
			t.Fatal("child Auto H3 did not fill the remaining root capacity")
		}
		h1 := budget.register(platformTransportBudgetH1, math.MaxInt64, false)
		defer h1.Release()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := make(chan bool, 1)
		go func() { result <- h1.Acquire(ctx) }()
		synctest.Wait()
		select {
		case <-result:
			t.Fatal("impossible H1 claim acquired wrapped capacity")
		default:
		}
		select {
		case <-auto.PreemptNotify():
			t.Fatal("locally resolvable overflow revoked a victim despite an impossible root")
		default:
		}
		if budget.Stats().PreemptedH3Count != 0 || root.Stats().PreemptedH3Count != 0 {
			t.Fatal("impossible hierarchy published a partial preemption plan")
		}
		cancel()
		synctest.Wait()
		select {
		case acquired := <-result:
			if acquired {
				t.Fatal("canceled impossible H1 acquired")
			}
		default:
			t.Fatal("canceled impossible H1 remained blocked")
		}
		if budget.Stats().PendingH1ByteCount != 0 || root.Stats().PendingH1ByteCount != 0 ||
			budget.Stats().UsedByteCount != math.MaxInt64-1 || root.Stats().UsedByteCount != math.MaxInt64 {
			t.Fatal("canceling impossible H1 lost an existing hierarchy claim")
		}
		auto.Release()
		peer.Release()
		requirePlatformBudgetOverflowEmpty(t, budget, root)
	})
}

// A handoff plan already credits the old carrier. Selecting it again as a
// preemption victim would pretend an impossible H1 replacement can fit.
func TestPlatformTransportBudgetOverflowHandoffPreemptionCreditsOldOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		budget := NewPlatformTransportBudget(10, 0)
		old := budget.register(platformTransportBudgetH3Auto, 4, false)
		defer old.Release()
		if !old.TryAcquire() {
			t.Fatal("old optional carrier did not acquire")
		}
		fixed := budget.register(platformTransportBudgetH1, 6, false)
		defer fixed.Release()
		if !fixed.TryAcquire() {
			t.Fatal("fixed H1 carrier did not acquire")
		}
		next := budget.register(platformTransportBudgetH1, 7, false)
		defer next.Release()
		if !next.AllowHandoffFrom(old) || !next.IsWaiting() {
			t.Fatal("impossible replacement unexpectedly fit its one handoff discount")
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := make(chan bool, 1)
		go func() { result <- next.Acquire(ctx) }()
		synctest.Wait()
		select {
		case <-old.PreemptNotify():
			t.Fatal("handoff preemption credited and revoked the same old carrier twice")
		default:
		}
		select {
		case <-result:
			t.Fatal("impossible handoff replacement stopped waiting")
		default:
		}
		cancel()
		synctest.Wait()
		if <-result {
			t.Fatal("canceled handoff replacement acquired")
		}
		old.Release()
		fixed.Release()
		requirePlatformBudgetOverflowEmpty(t, budget)
	})
}

// Priority comparison must not subtract extreme ints. The lowest-priority
// lease goes first; equal priorities retain the existing oldest-first order.
func TestPlatformTransportBudgetOverflowPreemptionPriorityAndFifo(t *testing.T) {
	maxPriority := int(^uint(0) >> 1)
	for _, priorities := range [][2]int{{maxPriority, -maxPriority - 1}, {1, 1}} {
		synctest.Test(t, func(t *testing.T) {
			budget := NewPlatformTransportBudget(2, 0)
			first := budget.registerWithPriority(platformTransportBudgetH3Auto, 1, false, priorities[0])
			defer first.Release()
			if !first.TryAcquire() {
				t.Fatal("first Auto H3 did not acquire")
			}
			second := budget.registerWithPriority(platformTransportBudgetH3Auto, 1, false, priorities[1])
			defer second.Release()
			if !second.TryAcquire() {
				t.Fatal("second Auto H3 did not acquire")
			}
			h1 := budget.register(platformTransportBudgetH1, 1, false)
			defer h1.Release()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan bool, 1)
			go func() { result <- h1.Acquire(ctx) }()
			synctest.Wait()
			select {
			case <-first.PreemptNotify():
			default:
				t.Fatalf("priority/FIFO ordering did not select the first victim: %v", priorities)
			}
			select {
			case <-second.PreemptNotify():
				t.Fatalf("priority/FIFO ordering selected the wrong additional victim: %v", priorities)
			default:
			}
			if !first.Yield() {
				t.Fatal("selected first victim did not yield")
			}
			synctest.Wait()
			if !<-result {
				t.Fatal("H1 did not acquire after the ordered victim yielded")
			}
			h1.Release()
			first.Release()
			second.Release()
			requirePlatformBudgetOverflowEmpty(t, budget)
		})
	}
}
