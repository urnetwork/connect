// Reliable senders blocked on the resend capacity gate pass in arrival order,
// one turn each, so the runtime's wake order cannot decide whose Packs reach
// admission while the gate is open.
package connect

import (
	"runtime"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
)

// The root cause of one reliable sender passing the gate again and again while
// its siblings stay blocked. Three senders wait on a closed gate in a fixed
// order and each sends three Packs; once the gate opens and stays open, they
// must pass one turn each in arrival order. The gate used to wake every waiter
// and stay open for whoever ran: on one processor the first to run passed all
// three of its Packs without blocking before a sibling ran at all.
func TestResendCapacityGatePassesWaitersInArrivalOrder(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	synctest.Test(t, func(t *testing.T) {
		sequence := &SendSequence{ctx: t.Context()}
		sequence.resendCapacityUnavailable.Store(true)
		const senderCount = 3
		const packCount = 3
		var passesLock sync.Mutex
		var passes []int
		var senders sync.WaitGroup
		// Senders start, and so first wait, in index order.
		for index := range senderCount {
			senders.Go(func() {
				for range packCount {
					if admitted, err, _ := sequence.awaitResendCapacity(&SendPack{Ctx: t.Context()}, -1); !admitted || err != nil {
						t.Errorf("sender %d refused at an open gate: %t, %v", index, admitted, err)
						return
					}
					func() {
						passesLock.Lock()
						defer passesLock.Unlock()
						passes = append(passes, index)
					}()
				}
			})
			synctest.Wait()
		}

		sequence.resendCapacityUnavailable.Store(false)
		senders.Wait()
		if want := []int{0, 1, 2, 0, 1, 2, 0, 1, 2}; !slices.Equal(passes, want) {
			t.Fatalf("senders passed the opened gate in order %v, want one turn each in arrival order %v", passes, want)
		}
	})
}

// A waiter given the turn may find the gate closed again by the time it runs.
// It stays first in line, and it passes first at the next opening.
func TestResendCapacityGateKeepsFirstWaiterFirstWhenItClosesAgain(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	synctest.Test(t, func(t *testing.T) {
		sequence := &SendSequence{ctx: t.Context()}
		gate := &sequence.resendCapacityUnavailable
		gate.Store(true)
		var passesLock sync.Mutex
		var passes []int
		var senders sync.WaitGroup
		for index := range 2 {
			senders.Go(func() {
				if admitted, err, _ := sequence.awaitResendCapacity(&SendPack{Ctx: t.Context()}, -1); !admitted || err != nil {
					t.Errorf("sender %d refused at an open gate: %t, %v", index, admitted, err)
					return
				}
				passesLock.Lock()
				defer passesLock.Unlock()
				passes = append(passes, index)
			})
			synctest.Wait()
		}

		// The first waiter gets the turn, then the gate closes before it runs.
		gate.Store(false)
		gate.Store(true)
		synctest.Wait()
		if len(passes) != 0 || gate.waiterCount.Load() != 2 {
			t.Fatalf("a closed gate let %v pass and kept %d waiters, want none and 2", passes, gate.waiterCount.Load())
		}

		gate.Store(false)
		senders.Wait()
		if want := []int{0, 1}; !slices.Equal(passes, want) {
			t.Fatalf("waiters passed the reopened gate in order %v, want %v", passes, want)
		}
	})
}

// A waiter that gives up while holding the turn passes it on, so the next
// waiter is not stranded behind a turn nobody will use.
func TestResendCapacityGateAbandonedTurnPassesToNextWaiter(t *testing.T) {
	gate := &resendCapacityGate{}
	gate.Store(true)
	first, second := gate.queue(), gate.queue()
	if first == nil || second == nil {
		t.Fatal("a closed gate did not queue its waiters")
	}
	hasTurn := func(waiter *resendCapacityWaiter) bool {
		select {
		case <-waiter.ready:
			return true
		default:
			return false
		}
	}

	gate.Store(false)
	if !hasTurn(first) || hasTurn(second) {
		t.Fatal("opening did not give the turn to the first waiter alone")
	}
	gate.abandon(first)
	if !hasTurn(second) {
		t.Fatal("an abandoned turn did not pass to the next waiter")
	}
	if !gate.pass(second) {
		t.Fatal("the next waiter could not pass the open gate with its turn")
	}
	if count := gate.waiterCount.Load(); count != 0 || gate.first != nil || gate.last != nil || !gate.open(true) {
		t.Fatalf("gate after every waiter left holds count %d head=%p tail=%p open=%t", count, gate.first, gate.last, gate.open(true))
	}
}

// A sender with no time to wait holds no place in line, so it takes an open
// gate even while a waiter holds the turn, as it did before waiters queued.
func TestResendCapacityGateLetsASenderThatCannotWaitPassAnOpenGate(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	synctest.Test(t, func(t *testing.T) {
		sequence := &SendSequence{ctx: t.Context()}
		gate := &sequence.resendCapacityUnavailable
		gate.Store(true)
		waiterResult := make(chan bool, 1)
		go func() {
			admitted, err, _ := sequence.awaitResendCapacity(&SendPack{Ctx: t.Context()}, -1)
			waiterResult <- admitted && err == nil
		}()
		synctest.Wait()

		gate.Store(false)
		if count := gate.waiterCount.Load(); count != 1 {
			t.Fatalf("waiter count with the turn given but not taken = %d, want 1", count)
		}
		if admitted, err, _ := sequence.awaitResendCapacity(&SendPack{Ctx: t.Context()}, 0); !admitted || err != nil {
			t.Fatalf("zero-timeout sender at an open gate = %t, %v; want admitted", admitted, err)
		}
		synctest.Wait()
		select {
		case admitted := <-waiterResult:
			if !admitted {
				t.Fatal("waiter holding the turn was refused")
			}
		default:
			t.Fatal("waiter holding the turn did not pass")
		}
	})
}
