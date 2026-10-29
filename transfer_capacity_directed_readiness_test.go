package connect

import (
	"runtime"
	"testing"
	"testing/synctest"
	"weak"
)

// Every observation is synchronous or follows synctest quiescence. No timing,
// packet owner, source-capacity assignment or scheduler lottery grants a turn.
func testingCapacityReady(ready <-chan struct{}) bool {
	select {
	case <-ready:
		return true
	default:
		return false
	}
}

// All intrusive ownership disappears when the last place is retired.
func testingCapacityEmpty(t *testing.T, gate *resendCapacityGate) {
	t.Helper()
	if gate.waiterCount.Load() != 0 || gate.first != nil || gate.last != nil {
		t.Fatal("capacity gate retained a count or intrusive queue after retirement")
	}
}

// Opening exactly after the ordinary closed decision, before the published
// waiter count, must not lose the only readiness edge.
func TestResendCapacityQueueRetainsOpeningBeforeCountPublication(t *testing.T) {
	gate := new(resendCapacityGate)
	gate.Store(true)
	entered := false
	gate.beforeQueueAppendForTest = func() {
		entered = true
		if gate.waiterCount.Load() != 0 || gate.first != nil {
			t.Fatal("publication barrier did not precede the first waiter count")
		}
		gate.Store(false)
	}
	waiter := gate.queue()
	if waiter == nil {
		t.Fatal("closed decision did not allocate its actual ordinary place")
	}
	defer gate.abandon(waiter)
	if !entered || gate.Load() || !testingCapacityReady(waiter.ready) || !gate.pass(waiter) {
		t.Fatal("CAPACITY_OPENING_BEFORE_COUNT_WAS_LOST")
	}
	testingCapacityEmpty(t, gate)
}

// A reclosed first turn stays at the head. Passing or abandoning it transfers
// exactly one place, preserving the ordinary channel-based FIFO contract.
func TestResendCapacityIntrusiveReclosePreservesOrdinaryFifo(t *testing.T) {
	gate := new(resendCapacityGate)
	gate.Store(true)
	first, second, third := gate.queue(), gate.queue(), gate.queue()
	if first == nil || second == nil || third == nil {
		t.Fatal("closed gate did not retain three ordinary producers")
	}
	defer gate.abandon(first)
	defer gate.abandon(second)
	defer gate.abandon(third)
	gate.Store(false)
	if !testingCapacityReady(first.ready) || testingCapacityReady(second.ready) || testingCapacityReady(third.ready) {
		t.Fatal("opening gave more than the oldest ordinary turn")
	}
	oldReady := first.ready
	gate.Store(true)
	if gate.pass(first) || first.ready == oldReady || testingCapacityReady(first.ready) || gate.first != first || gate.waiterCount.Load() != 3 {
		t.Fatal("reclosing lost or admitted the oldest ordinary place")
	}
	gate.Store(false)
	if !testingCapacityReady(first.ready) || testingCapacityReady(second.ready) || !gate.pass(first) || !testingCapacityReady(second.ready) || testingCapacityReady(third.ready) {
		t.Fatal("reopening did not preserve exact ordinary arrival order")
	}
	gate.abandon(second)
	if !testingCapacityReady(third.ready) || !gate.pass(third) {
		t.Fatal("abandoned ordinary turn stranded its next producer")
	}
	testingCapacityEmpty(t, gate)
}

// Directed places join the same FIFO; a reclosed grant must be revalidated,
// while an existing zero-timeout producer keeps its immediate open offer.
func TestResendCapacityDirectedTurnRespectsOrdinaryFifoAndReclose(t *testing.T) {
	gate := new(resendCapacityGate)
	gate.Store(true)
	first := gate.queue()
	wake := make(chan struct{}, 1)
	directed := new(resendCapacityWaiter)
	epoch := gate.watch(directed, wake)
	last := gate.queue()
	if first == nil || last == nil || epoch == 0 || directed.ready != nil {
		t.Fatal("ordinary/directed FIFO premise was not established")
	}
	defer gate.abandon(first)
	defer gate.abandon(last)
	defer gate.abandonWatch(directed, epoch)
	gate.Store(false)
	if !testingCapacityReady(first.ready) || testingCapacityReady(wake) || gate.open(true) || !gate.open(false) {
		t.Fatal("directed subscriber changed older or zero-timeout admission")
	}
	if !gate.pass(first) || !testingCapacityReady(wake) || !gate.watchGranted(directed, epoch) || testingCapacityReady(last.ready) {
		t.Fatal("ordinary retirement did not transfer exactly one directed turn")
	}
	gate.Store(true)
	if gate.passWatch(directed, epoch) || directed.ready != nil || gate.first != directed || testingCapacityReady(last.ready) {
		t.Fatal("reclosed directed turn bypassed capacity or lost its FIFO place")
	}
	gate.Store(false)
	if !testingCapacityReady(wake) || !gate.watchGranted(directed, epoch) || !gate.passWatch(directed, epoch) || !testingCapacityReady(last.ready) || !gate.pass(last) {
		t.Fatal("directed regrant did not advance the ordinary sibling")
	}
	testingCapacityEmpty(t, gate)
}

// Ordinary APIs cannot consume a prepaid directed slot, and an API invoked on
// the wrong gate cannot touch the intrusive links under a foreign lock.
func TestResendCapacityDirectedRejectsWrongGateAndOrdinaryApi(t *testing.T) {
	left, right := new(resendCapacityGate), new(resendCapacityGate)
	left.Store(true)
	right.Store(true)
	wake := make(chan struct{}, 1)
	slot := new(resendCapacityWaiter)
	epoch := left.watch(slot, wake)
	other := right.queue()
	if epoch == 0 || other == nil {
		t.Fatal("two independently owned queue premises were not established")
	}
	defer left.abandonWatch(slot, epoch)
	defer right.abandon(other)
	left.Store(false)
	right.Store(false)
	if right.watchGranted(slot, epoch) || right.passWatch(slot, epoch) || right.pass(slot) || left.pass(slot) {
		t.Fatal("foreign or ordinary API consumed a directed installation")
	}
	right.abandonWatch(slot, epoch)
	right.abandon(slot)
	left.abandon(slot)
	if left.first != slot || left.last != slot || left.waiterCount.Load() != 1 || !left.watchGranted(slot, epoch) || right.first != other || right.last != other || right.waiterCount.Load() != 1 {
		t.Fatal("foreign/ordinary withdrawal corrupted independent queue custody")
	}
	if !left.passWatch(slot, epoch) || !right.pass(other) {
		t.Fatal("wrong API changed the rightful installation's permission")
	}
	testingCapacityEmpty(t, left)
	testingCapacityEmpty(t, right)
}

// A slot may be reused across gates or on the original gate. Old immutable
// installation tokens cannot grant, pass or withdraw either newer owner.
func TestResendCapacityDirectedRejectsCrossGateAndSameGateAba(t *testing.T) {
	left, right := new(resendCapacityGate), new(resendCapacityGate)
	wake := make(chan struct{}, 1)
	slot := new(resendCapacityWaiter)
	first := left.watch(slot, wake)
	if first == 0 || !left.watchGranted(slot, first) || !left.passWatch(slot, first) {
		t.Fatal("first directed installation did not retire normally")
	}
	second := right.watch(slot, wake)
	if second <= first || !right.watchGranted(slot, second) {
		t.Fatal("cross-gate reuse did not receive a distinct installation token")
	}
	if left.watchGranted(slot, first) || left.passWatch(slot, first) || right.watchGranted(slot, first) || right.passWatch(slot, first) {
		t.Fatal("stale directed token granted or passed a newer cross-gate owner")
	}
	left.abandonWatch(slot, first)
	right.abandonWatch(slot, first)
	if right.first != slot || right.waiterCount.Load() != 1 || !right.watchGranted(slot, second) || !right.passWatch(slot, second) {
		t.Fatal("stale cross-gate withdrawal changed the newer installation")
	}
	third := left.watch(slot, wake)
	if third <= second || !left.watchGranted(slot, third) {
		t.Fatal("same-gate reuse did not advance its persistent installation token")
	}
	if left.watchGranted(slot, first) || left.passWatch(slot, first) {
		t.Fatal("same-gate stale token was accepted after slot reuse")
	}
	left.abandonWatch(slot, first)
	if left.first != slot || left.waiterCount.Load() != 1 || !left.watchGranted(slot, third) {
		t.Fatal("same-gate stale withdrawal consumed the fresh turn")
	}
	left.abandonWatch(slot, third)
	left.abandonWatch(slot, third)
	testingCapacityEmpty(t, left)
	testingCapacityEmpty(t, right)
	if slot.owner.Load() != nil || slot.queued || slot.turn || slot.previous != nil || slot.next != nil || slot.ready != nil || slot.wake != nil {
		t.Fatal("retired reused slot still retains intrusive or parent-wake ownership")
	}
}

// Register after the sole ACK already opened the source; the lock-protected
// check/subscribe step must immediately retain the directed turn and wake.
func TestResendCapacityDirectedRegistrationAfterOpeningKeepsTurn(t *testing.T) {
	gate := new(resendCapacityGate)
	gate.Store(true)
	gate.Store(false)
	wake := make(chan struct{}, 1)
	slot := new(resendCapacityWaiter)
	epoch := gate.watch(slot, wake)
	if epoch == 0 || slot.ready != nil || !testingCapacityReady(wake) || !gate.watchGranted(slot, epoch) || !gate.passWatch(slot, epoch) {
		t.Fatal("late directed registration lost the already-published readiness")
	}
	testingCapacityEmpty(t, gate)
}

// Terminal notification revokes every kind of grant and wakes withdrawal.
// Repeated close and later open publications can never revive the source.
func TestResendCapacityTerminalCloseRevokesAndReleasesAllKinds(t *testing.T) {
	for _, previouslyGranted := range []bool{false, true} {
		gate := new(resendCapacityGate)
		gate.Store(true)
		ordinary := gate.queue()
		wake := make(chan struct{}, 1)
		slot := new(resendCapacityWaiter)
		epoch := gate.watch(slot, wake)
		last := gate.queue()
		if ordinary == nil || last == nil || epoch == 0 {
			t.Fatal("terminal test did not retain all producer kinds")
		}
		if previouslyGranted {
			gate.Store(false)
		}
		gate.close()
		gate.close()
		gate.Store(false)
		if !gate.Load() || gate.open(false) || gate.open(true) || !testingCapacityReady(ordinary.ready) || !testingCapacityReady(last.ready) || !testingCapacityReady(wake) || gate.watchGranted(slot, epoch) || gate.passWatch(slot, epoch) || gate.pass(ordinary) || gate.pass(last) {
			t.Fatal("terminal source failed to wake withdrawal or granted admission")
		}
		if gate.queue() != nil || gate.watch(new(resendCapacityWaiter), wake) != 0 {
			t.Fatal("terminal source admitted a new FIFO installation")
		}
		gate.abandon(ordinary)
		gate.abandonWatch(slot, epoch)
		gate.abandon(last)
		testingCapacityEmpty(t, gate)
	}
}

// A normal producer blocked on the actual source gate must leave on terminal
// close even if its caller context is still live; no timer is advanced.
func TestResendCapacityTerminalCloseReturnsBlockedOrdinaryProducer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sequence := &SendSequence{ctx: t.Context()}
		sequence.resendCapacityUnavailable.Store(true)
		type result struct {
			admitted bool
			err      error
		}
		done := make(chan result, 1)
		joined := false
		defer func() {
			sequence.resendCapacityUnavailable.close()
			if !joined {
				<-done
			}
		}()
		go func() {
			admitted, err, _ := sequence.awaitResendCapacity(&SendPack{Ctx: t.Context()}, -1)
			done <- result{admitted: admitted, err: err}
		}()
		synctest.Wait()
		if sequence.resendCapacityUnavailable.waiterCount.Load() != 1 {
			t.Fatal("normal source producer did not reach its actual capacity wait")
		}
		sequence.resendCapacityUnavailable.close()
		synctest.Wait()
		select {
		case observed := <-done:
			joined = true
			if observed.admitted || observed.err == nil || t.Context().Err() != nil {
				t.Fatal("terminal close admitted work or needed caller cancellation")
			}
		default:
			t.Fatal("terminal source retained its normal blocked producer")
		}
		testingCapacityEmpty(t, &sequence.resendCapacityUnavailable)
	})
}

// Directed registration uses its caller-owned slot and existing wake only.
// Source review additionally verifies these methods start no worker.
func TestResendCapacityDirectedRegistrationDoesNotAllocateChannelOrWorkerState(t *testing.T) {
	gate := new(resendCapacityGate)
	wake := make(chan struct{}, 1)
	slot := new(resendCapacityWaiter)
	invalid := false
	allocations := testing.AllocsPerRun(100, func() {
		epoch := gate.watch(slot, wake)
		if epoch == 0 || slot.ready != nil || !gate.watchGranted(slot, epoch) || !testingCapacityReady(wake) || !gate.passWatch(slot, epoch) {
			invalid = true
		}
	})
	if invalid || allocations != 0 || gate.waiterCount.Load() != 0 || slot.owner.Load() != nil || slot.wake != nil {
		t.Fatalf("directed registration retained channel/queue state or allocated: invalid=%t allocations=%v", invalid, allocations)
	}
}

// The helper's frame cannot accidentally keep the removed slot alive during GC.
//
//go:noinline
func testingReleasedCapacityDirectedSlot(t *testing.T, gate *resendCapacityGate) weak.Pointer[resendCapacityWaiter] {
	t.Helper()
	gate.Store(true)
	slot := new(resendCapacityWaiter)
	epoch := gate.watch(slot, make(chan struct{}, 1))
	if epoch == 0 {
		t.Fatal("slot ownership premise was not established")
	}
	gate.abandonWatch(slot, epoch)
	if slot.owner.Load() != nil || slot.previous != nil || slot.next != nil || slot.ready != nil || slot.wake != nil || slot.queued {
		t.Fatal("retired slot still retains an intrusive link or parent-wake owner")
	}
	return weak.Make(slot)
}

// A departed directed slot cannot remain reachable from an otherwise-live
// empty gate; there is no retained slice backing to keep it alive indirectly.
func TestResendCapacityEmptyGateDoesNotRetainDirectedSlot(t *testing.T) {
	gate := new(resendCapacityGate)
	owner := testingReleasedCapacityDirectedSlot(t, gate)
	testingCapacityEmpty(t, gate)
	runtime.GC()
	runtime.GC()
	if owner.Value() != nil {
		t.Fatal("empty capacity gate kept a retired directed slot reachable")
	}
	runtime.KeepAlive(gate)
}
