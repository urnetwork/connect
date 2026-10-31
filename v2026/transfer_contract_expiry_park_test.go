// The queued contract expiry worker arms one timer for its life and parks
// while no contract is queued, so a manager that once queued a contract does
// not wake every pass interval for the rest of the client's life.
package connect

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// Queues one contract for a fresh destination on a native ControlId client,
// the shape the expiry tests share.
func queueExpiryTestContract(t *testing.T, manager *ContractManager) {
	t.Helper()
	destination := NewId()
	frame := expiryContractFrame(t, ControlId, destination, NewId())
	defer MessagePoolReturn(frame.MessageBytes)
	if err := manager.HandleControlFrame(ContractKey{Destination: DestinationId(destination)}, frame); err != nil {
		t.Fatal(err)
	}
}

// With nothing queued the worker parks; a newly queued contract wakes it and
// still expires within the enqueue-age timeout plus one pass interval.
func TestContractExpiryParksWhileNoContractIsQueued(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		// passes every second
		const expireTimeout = 2 * time.Second
		wakes := atomic.Int64{}
		settings := closeWaitClientSettings()
		settings.ContractManagerSettings.ContractQueueExpireTimeout = expireTimeout
		client := NewClient(t.Context(), ControlId, NewNoContractClientOob(), settings)
		defer func() {
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		}()
		manager := client.ContractManager()
		manager.contractExpiryWakeForTest = func() {
			wakes.Add(1)
		}

		queueExpiryTestContract(t, manager)
		synctest.Wait()
		time.Sleep(expireTimeout + expireTimeout/2)
		synctest.Wait()
		if count := expiryQueueCount(manager); count != 0 {
			t.Fatalf("an orphan queue remained past its expiry: %d", count)
		}

		before := wakes.Load()
		time.Sleep(100 * time.Second)
		synctest.Wait()
		if idleWakes := wakes.Load() - before; idleWakes != 0 {
			t.Fatalf("the expiry worker woke %d times over 100s with nothing queued at a 1s interval, want it parked", idleWakes)
		}

		queueExpiryTestContract(t, manager)
		synctest.Wait()
		if count := expiryQueueCount(manager); count != 1 {
			t.Fatalf("queued reservations = %d, want one", count)
		}
		time.Sleep(expireTimeout + expireTimeout/2)
		synctest.Wait()
		if count := expiryQueueCount(manager); count != 0 {
			t.Fatalf("a contract queued after the park was not expired on schedule: %d", count)
		}
	})
}

// While a contract stays queued the worker passes on its interval with one
// timer for its life, not a new one per pass.
func TestContractExpiryReusesOneTimer(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		wakes := atomic.Int64{}
		settings := closeWaitClientSettings()
		// passes every minute, and the contract outlives the test
		settings.ContractManagerSettings.ContractQueueExpireTimeout = time.Hour
		client := NewClient(t.Context(), ControlId, NewNoContractClientOob(), settings)
		defer func() {
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		}()
		manager := client.ContractManager()
		manager.contractExpiryWakeForTest = func() {
			wakes.Add(1)
		}
		queueExpiryTestContract(t, manager)
		synctest.Wait()

		timers := timerAllocationsUnder(t, ").expireQueuedContracts", func() {
			time.Sleep(50 * time.Minute)
			synctest.Wait()
		})
		if count := expiryQueueCount(manager); count != 1 {
			t.Fatalf("queued reservations = %d, want the one still inside its timeout", count)
		}
		if wakes.Load() < 40 {
			t.Fatalf("only %d expiry passes over 50 minutes at a 1 minute interval", wakes.Load())
		}
		if 0 < timers {
			t.Fatalf("the expiry worker allocated %d timer objects over %d passes, want one timer for its life", timers, wakes.Load())
		}
	})
}
