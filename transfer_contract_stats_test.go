package connect

import (
	"context"
	"testing"
	"time"
)

func TestContractStatsEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultClientSettings()
	settings.ContractManagerSettings.ContractStatsEpoch = 10 * time.Millisecond
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	defer client.Cancel()
	contractManager := client.ContractManager()

	contractId := NewId()
	path := TransferPath{
		SourceId:      client.ClientId(),
		DestinationId: NewId(),
	}

	entry := contractManager.registerContractStats(contractId, false, true, path, 1000)

	eventsChannel := make(chan []*ContractStatsEvent, 16)
	unsub := contractManager.AddContractStatsCallback(func(contractStatsEvents []*ContractStatsEvent) {
		eventsChannel <- contractStatsEvents
	})
	defer unsub()

	nextEvent := func() *ContractStatsEvent {
		select {
		case events := <-eventsChannel:
			// the test drives one contract, so each epoch has at most one event
			AssertEqual(t, 1, len(events))
			return events[0]
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for contract stats events")
			return nil
		}
	}

	// the initial event for the open contract
	event := nextEvent()
	AssertEqual(t, contractId, event.ContractId)
	AssertEqual(t, false, event.Receive)
	AssertEqual(t, true, event.Companion)
	AssertEqual(t, path, event.Path)
	AssertEqual(t, ByteCount(1000), event.TransferByteCount)
	AssertEqual(t, ByteCount(0), event.UsedByteCount)
	AssertEqual(t, true, event.Open)

	// ongoing usage is reported with deltas
	entry.updateUsedByteCount(100)
	event = nextEvent()
	AssertEqual(t, ByteCount(100), event.UsedByteCount)
	AssertEqual(t, ByteCount(100), event.UsedByteCountDelta)
	AssertEqual(t, true, event.Open)

	entry.updateUsedByteCount(250)
	event = nextEvent()
	AssertEqual(t, ByteCount(250), event.UsedByteCount)
	AssertEqual(t, ByteCount(150), event.UsedByteCountDelta)

	// no change, no event
	select {
	case events := <-eventsChannel:
		t.Fatalf("unexpected events %v", events)
	case <-time.After(100 * time.Millisecond):
	}

	// close emits a final closed event and removes the entry
	contractManager.CloseContract(contractId, 250, 0)
	event = nextEvent()
	AssertEqual(t, false, event.Open)
	AssertEqual(t, ByteCount(250), event.UsedByteCount)

	func() {
		contractManager.contractStatsLock.Lock()
		defer contractManager.contractStatsLock.Unlock()
		AssertEqual(t, 0, len(contractManager.contractStatsEntries))
	}()
}

func TestContractStatsCloseWithoutListeners(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := NewClient(ctx, NewId(), NewNoContractClientOob(), DefaultClientSettings())
	defer client.Cancel()
	contractManager := client.ContractManager()

	contractId := NewId()
	contractManager.registerContractStats(contractId, true, false, TransferPath{}, 1000)

	// with no epoch worker, close removes the entry immediately
	contractManager.CloseContract(contractId, 0, 0)

	func() {
		contractManager.contractStatsLock.Lock()
		defer contractManager.contractStatsLock.Unlock()
		AssertEqual(t, 0, len(contractManager.contractStatsEntries))
	}()
}
