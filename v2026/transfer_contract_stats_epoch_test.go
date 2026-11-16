package connect

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func contractStatsEpochTestManager(t *testing.T, epoch time.Duration, wakes *atomic.Int64) (*ContractManager, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	settings := DefaultClientSettings()
	settings.ContractManagerSettings.ContractStatsEpoch = epoch
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	contractManager := client.ContractManager()
	if wakes != nil {
		contractManager.contractStatsWakeForTest = func() {
			wakes.Add(1)
		}
	}
	return contractManager, func() {
		client.Cancel()
		cancel()
	}
}

// The epoch worker arms one timer for its life, not one per epoch.
func TestContractStatsEpochReusesOneTimer(t *testing.T) {
	contractManager, closeManager := contractStatsEpochTestManager(t, 2*time.Millisecond, nil)
	defer closeManager()
	entry := contractManager.registerContractStats(NewId(), false, false, TransferPath{SourceId: NewId(), DestinationId: NewId()}, 1<<20)
	events := atomic.Int64{}
	unsub := contractManager.AddContractStatsCallback(func(contractStatsEvents []*ContractStatsEvent) {
		events.Add(int64(len(contractStatsEvents)))
	})
	defer unsub()

	timers := timerAllocationsUnder(t, ").runContractStats", func() {
		// keep the contract moving so every epoch has work
		for i := 1; i <= 100; i += 1 {
			entry.updateUsedByteCount(ByteCount(i * 100))
			time.Sleep(time.Millisecond)
		}
	})
	if events.Load() < 10 {
		t.Fatalf("only %d events over 100 moving epochs", events.Load())
	}
	if 4 < timers {
		t.Fatalf("the epoch worker allocated %d timer objects over ~50 epochs, want one timer for its life", timers)
	}
}

// With no contract moving, the worker parks instead of waking every epoch, and
// wakes again for the next change.
func TestContractStatsEpochParksWithoutChanges(t *testing.T) {
	wakes := atomic.Int64{}
	contractManager, closeManager := contractStatsEpochTestManager(t, 2*time.Millisecond, &wakes)
	defer closeManager()
	contractId := NewId()
	entry := contractManager.registerContractStats(contractId, false, false, TransferPath{SourceId: NewId(), DestinationId: NewId()}, 1<<20)
	eventsChannel := make(chan []*ContractStatsEvent, 64)
	unsub := contractManager.AddContractStatsCallback(func(contractStatsEvents []*ContractStatsEvent) {
		eventsChannel <- contractStatsEvents
	})
	defer unsub()

	// the new contract is reported once
	select {
	case <-eventsChannel:
	case <-time.After(5 * time.Second):
		t.Fatal("the new contract was not reported")
	}
	time.Sleep(20 * time.Millisecond)

	before := wakes.Load()
	time.Sleep(100 * time.Millisecond)
	if idleWakes := wakes.Load() - before; 2 < idleWakes {
		t.Fatalf("the worker woke %d times over 100ms with nothing changing at a 2ms epoch, want it parked", idleWakes)
	}

	// a change wakes it and is reported within about an epoch
	entry.updateUsedByteCount(4096)
	select {
	case events := <-eventsChannel:
		AssertEqual(t, 1, len(events))
		AssertEqual(t, ByteCount(4096), events[0].UsedByteCount)
		AssertEqual(t, true, events[0].Open)
	case <-time.After(5 * time.Second):
		t.Fatal("a change after parking was not reported")
	}

	// and so is a close
	contractManager.closeContractStats(contractId)
	select {
	case events := <-eventsChannel:
		AssertEqual(t, 1, len(events))
		AssertEqual(t, false, events[0].Open)
	case <-time.After(5 * time.Second):
		t.Fatal("a close after parking was not reported")
	}
}

// A contract registered while the worker is parked is reported.
func TestContractStatsEpochReportsContractsRegisteredWhileParked(t *testing.T) {
	wakes := atomic.Int64{}
	contractManager, closeManager := contractStatsEpochTestManager(t, 2*time.Millisecond, &wakes)
	defer closeManager()
	eventsChannel := make(chan []*ContractStatsEvent, 64)
	unsub := contractManager.AddContractStatsCallback(func(contractStatsEvents []*ContractStatsEvent) {
		eventsChannel <- contractStatsEvents
	})
	defer unsub()
	time.Sleep(50 * time.Millisecond)

	contractId := NewId()
	contractManager.registerContractStats(contractId, true, false, TransferPath{SourceId: NewId(), DestinationId: NewId()}, 1<<20)
	select {
	case events := <-eventsChannel:
		AssertEqual(t, 1, len(events))
		AssertEqual(t, contractId, events[0].ContractId)
	case <-time.After(5 * time.Second):
		t.Fatal("a contract registered while parked was not reported")
	}
}
