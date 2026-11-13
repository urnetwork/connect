// A call that waits arms one timer for its deadline, however often an edge
// wakes it before the deadline: TakeContract on its queue's notifications,
// SendSequence.Pack on session readiness edges and across its three
// admission waits, which share the caller's one deadline.
package connect

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026/protocol"
)

// Queue notifications that bring no contract wake the take without arming a
// new timer, and the take still ends at its timeout.
func TestTakeContractArmsOneTimerPerCall(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		client := NewClient(t.Context(), NewId(), NewNoContractClientOob(), closeWaitClientSettings())
		defer func() {
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		}()
		manager := client.ContractManager()
		key := ContractKey{Destination: DestinationId(NewId())}
		const takeTimeout = time.Minute
		const wakeCount = 64

		var taken *protocol.Contract
		start := time.Now()
		timers := timerAllocationsUnder(t, ").TakeContract", func() {
			done := make(chan struct{})
			go func() {
				defer close(done)
				taken = manager.TakeContract(t.Context(), key, takeTimeout)
			}()
			synctest.Wait()
			queue := func() *contractQueue {
				manager.mutex.Lock()
				defer manager.mutex.Unlock()
				return manager.destinationContracts[key]
			}()
			if queue == nil {
				t.Fatal("the waiting take holds no queue")
			}
			for range wakeCount {
				queue.updateMonitor.NotifyAll()
				synctest.Wait()
			}
			<-done
		})
		if taken != nil {
			t.Fatal("an empty queue handed out a contract")
		}
		if elapsed := time.Since(start); elapsed != takeTimeout {
			t.Fatalf("the take returned after %s, want its timeout %s", elapsed, takeTimeout)
		}
		if 4 < timers {
			t.Fatalf("the take allocated %d timer objects over %d wakes, want one timer for the call", timers, wakeCount)
		}
	})
}

// Readiness edges that do not establish the cipher wake a Required send
// without arming a new timer, and the send still ends at its budget.
func TestRequiredCipherWaitArmsOneTimerPerCall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sequence, session, cancel := newRequiredCipherWakeFixture()
		defer func() { cancel(); synctest.Wait() }()
		const budget = time.Minute
		const wakeCount = 64

		var result requiredCipherWakeResult
		start := time.Now()
		timers := timerAllocationsUnder(t, "(*SendSequence).Pack", func() {
			done := startRequiredCipherWakePack(sequence, sequence.ctx, budget)
			synctest.Wait()
			for range wakeCount {
				func() {
					session.stateLock.Lock()
					defer session.stateLock.Unlock()
					session.notifyRequiredCipherChangedWithLock()
				}()
				synctest.Wait()
			}
			result = <-done
		})
		if result.accepted || !errors.Is(result.err, ErrEncryptionRequiredNotEstablished) {
			t.Fatalf("unestablished admission = %+v", result)
		}
		if elapsed := time.Since(start); elapsed != budget {
			t.Fatalf("the send returned after %s, want its budget %s", elapsed, budget)
		}
		if 4 < timers {
			t.Fatalf("the Required wait allocated %d timer objects over %d edges, want one timer for the call", timers, wakeCount)
		}
	})
}

// Readiness edges during the Opportunistic establish hold wake the send
// without arming a new timer, and the hold still ends at its deadline.
func TestEstablishHoldWaitArmsOneTimerPerCall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sequence, session, cancel := newRequiredCipherWakeFixture()
		defer func() { cancel(); synctest.Wait() }()
		const hold = time.Minute
		const wakeCount = 64
		session.settings.Mode = EncryptionModeOpportunistic
		session.settings.OpportunisticEstablishHold = hold
		session.establishHoldStart = time.Now()

		var result requiredCipherWakeResult
		start := time.Now()
		timers := timerAllocationsUnder(t, "(*SendSequence).Pack", func() {
			done := startRequiredCipherWakePack(sequence, sequence.ctx, -1)
			synctest.Wait()
			for range wakeCount {
				func() {
					session.stateLock.Lock()
					defer session.stateLock.Unlock()
					session.notifyRequiredCipherChangedWithLock()
				}()
				synctest.Wait()
			}
			result = <-done
		})
		if !result.accepted || result.err != nil {
			t.Fatalf("the send after the hold = %+v, want it queued in plaintext", result)
		}
		if elapsed := time.Since(start); elapsed != hold {
			t.Fatalf("the hold ended after %s, want %s", elapsed, hold)
		}
		if 4 < timers {
			t.Fatalf("the establish hold allocated %d timer objects over %d edges, want one timer for the call", timers, wakeCount)
		}
	})
}

// A reliable Pack that waits at each admission boundary in turn (resend
// capacity, the admission slot, the handoff) spends one deadline: the
// caller's timeout from its first wait. The three waits arm one timer.
func TestSendPackAdmissionWaitsShareOneDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer func() { cancel(); synctest.Wait() }()
		sequence := &SendSequence{
			ctx: ctx, cancel: cancel,
			sendBufferSettings: &SendBufferSettings{},
			packAdmission:      newSendPackAdmission(1),
			packs:              make(chan *SendPack, 1),
			idleCondition:      NewIdleCondition(),
		}
		// every boundary is closed: no resend capacity, the one admission
		// slot held, and the handoff full
		sequence.resendCapacityUnavailable.Store(true)
		holder := &SendPack{Ctx: ctx}
		if acquired, err, _ := sequence.acquirePackAdmission(holder, 0); !acquired || err != nil {
			t.Fatalf("holder admission = %t, %v", acquired, err)
		}
		sequence.packs <- &SendPack{Ctx: ctx}

		const budget = 300 * time.Millisecond
		const stageWait = 100 * time.Millisecond
		var accepted bool
		var err error
		start := time.Now()
		timers := timerAllocationsUnder(t, "(*SendSequence).Pack", func() {
			done := make(chan struct{})
			go func() {
				defer close(done)
				accepted, err = sequence.Pack(&SendPack{
					TransferOptions: TransferOptions{Ack: true},
					Frame:           &protocol.Frame{},
					Ctx:             ctx,
				}, budget)
			}()
			synctest.Wait()
			time.Sleep(stageWait)
			sequence.resendCapacityUnavailable.Store(false)
			synctest.Wait()
			time.Sleep(stageWait)
			holder.releaseAdmission()
			<-done
		})
		if accepted || err != nil {
			t.Fatalf("the full handoff = %t, %v, want not admitted", accepted, err)
		}
		if elapsed := time.Since(start); elapsed != budget {
			t.Fatalf("the three waits took %s, want the caller's %s", elapsed, budget)
		}
		if 4 < timers {
			t.Fatalf("the three admission waits allocated %d timer objects, want one timer for the call", timers)
		}
	})
}
