package connect

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestContractStatsStreamMark verifies a contract whose path carries a
// stream id is marked `Stream` in its stats events. This is the receive
// side's only signal that the flow rides a stream (e.g. a companion reply
// on an active stream — the platform marks the stored contract with the
// stream id, which `newSequenceContract` parses into the path).
func TestContractStatsStreamMark(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultClientSettings()
	settings.ContractManagerSettings.ContractStatsEpoch = 10 * time.Millisecond
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	defer client.Cancel()
	contractManager := client.ContractManager()

	contractId := NewId()
	path := TransferPath{
		SourceId:      NewId(),
		DestinationId: client.ClientId(),
		StreamId:      NewId(),
	}

	contractManager.registerContractStats(contractId, true, false, path, 1000)

	eventsChannel := make(chan []*ContractStatsEvent, 16)
	unsub := contractManager.AddContractStatsCallback(func(contractStatsEvents []*ContractStatsEvent) {
		eventsChannel <- contractStatsEvents
	})
	defer unsub()

	select {
	case events := <-eventsChannel:
		AssertEqual(t, 1, len(events))
		AssertEqual(t, contractId, events[0].ContractId)
		AssertEqual(t, true, events[0].Receive)
		AssertEqual(t, true, events[0].Stream)
		AssertEqual(t, path, events[0].Path)
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for contract stats events")
	}
}

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
	AssertEqual(t, false, event.Stream)
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

// TestReceiveContractSupersedeClosesStats guards the fix for receive contracts
// accumulating open in the UI (server/../sdk ContractDetailsView): when a new
// (typically larger) receive contract supersedes the current one, the
// superseded contract's STATS are closed immediately so it stops showing as an
// open contract -- even though the contract itself lingers in
// openReceiveContracts for the sender's resend/reorder window and is
// wire-closed later by the overflow trim. Mirrors the send side, which closes a
// drained predecessor in ackItem. Without this, exhausted receive contracts
// pile up open (up to MaxOpenReceiveContract) under continuous traffic.
func TestReceiveContractSupersedeClosesStats(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultClientSettings()
	settings.ContractManagerSettings.ContractStatsEpoch = 10 * time.Millisecond
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	defer client.Cancel()
	contractManager := client.ContractManager()

	// track the last-seen Open state per contract from the stats events
	var lock sync.Mutex
	lastOpen := map[Id]bool{}
	unsub := contractManager.AddContractStatsCallback(func(events []*ContractStatsEvent) {
		lock.Lock()
		defer lock.Unlock()
		for _, e := range events {
			lastOpen[e.ContractId] = e.Open
		}
	})
	defer unsub()
	waitOpen := func(id Id, want bool, msg string) {
		deadline := time.Now().Add(5 * time.Second)
		for {
			lock.Lock()
			v, ok := lastOpen[id]
			lock.Unlock()
			if ok && v == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("timeout: %s (id=%s seen=%v/%v)", msg, id, v, ok)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	source := TransferPath{SourceId: NewId(), DestinationId: client.ClientId()}
	rs := NewReceiveSequence(ctx, client, source, NewId(), sequenceTlsRoleServer, false, DefaultReceiveBufferSettings())

	contractPath := TransferPath{SourceId: source.SourceId, DestinationId: client.ClientId()}
	idA := NewId()
	idB := NewId()
	// A: the initial small contract; B: the larger successor (the grow pattern)
	contractA := &sequenceContract{localId: NewId(), contractId: idA, path: contractPath, transferByteCount: ByteCount(16 * 1024)}
	contractB := &sequenceContract{localId: NewId(), contractId: idB, path: contractPath, transferByteCount: ByteCount(32 * 1024 * 1024)}

	// A becomes current and reports open
	AssertEqual(t, nil, rs.setContract(contractA))
	waitOpen(idA, true, "A should report open")

	// B supersedes A -> A's stats must close (the fix), B opens
	AssertEqual(t, nil, rs.setContract(contractB))
	waitOpen(idA, false, "superseded A must close its stats, not linger open")
	waitOpen(idB, true, "B should report open")

	// stats-only close: A's CONTRACT stays in the receive buffer (2 < Max, no
	// wire-level trim/close), so the resend/reorder window is unchanged
	AssertEqual(t, 2, len(rs.openReceiveContracts))
}

// TestContractManagerCloseAllContractStats pins the teardown fix: at client
// teardown, closing all contract stats must mark every open entry closed and
// emit the closes SYNCHRONOUSLY to attached listeners (so a removed peer's
// contracts don't linger open in the UI), then drop the entries. This is the
// event that must "escape" before the client is cancelled and the stats
// listener is removed (see the multi-client channel teardown).
func TestContractManagerCloseAllContractStats(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// a long epoch so ONLY our explicit CloseAllContractStats emits (no tick)
	settings := DefaultClientSettings()
	settings.ContractManagerSettings.ContractStatsEpoch = 1 * time.Hour
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	defer client.Cancel()
	contractManager := client.ContractManager()

	var lock sync.Mutex
	closed := map[Id]bool{}
	unsub := contractManager.AddContractStatsCallback(func(events []*ContractStatsEvent) {
		lock.Lock()
		defer lock.Unlock()
		for _, e := range events {
			if !e.Open {
				closed[e.ContractId] = true
			}
		}
	})
	defer unsub()

	a := NewId()
	b := NewId()
	path := TransferPath{SourceId: client.ClientId(), DestinationId: NewId()}
	contractManager.registerContractStats(a, false, false, path, 1000)
	contractManager.registerContractStats(b, true, false, path, 2000)

	// teardown: every open contract must emit a close synchronously (the callback
	// runs inline, so the closes are delivered by the time this returns)
	contractManager.CloseAllContractStats()

	func() {
		lock.Lock()
		defer lock.Unlock()
		if !closed[a] || !closed[b] {
			t.Fatalf("expected synchronous close events for both contracts, got %v", closed)
		}
	}()

	// the entries are gone (a closed entry is dropped after emitting)
	func() {
		contractManager.contractStatsLock.Lock()
		defer contractManager.contractStatsLock.Unlock()
		if 0 != len(contractManager.contractStatsEntries) {
			t.Fatalf("expected all stats entries removed, got %d", len(contractManager.contractStatsEntries))
		}
	}()
}
