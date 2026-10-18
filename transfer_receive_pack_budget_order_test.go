// Every client of a device hands reliable Packs to its receive sequences
// through the device's one shared Pack queue budget, and each client has a
// single receive loop that waits there for capacity. Bytes a sequence worker
// frees belong to the oldest waiting loop, so the runtime's wake order cannot
// decide whose client receives.
package connect

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// The root cause of one client's receive loop taking every Pack budget byte a
// worker frees while the other clients of the device wait. Four producers,
// each standing in for one client's receive loop, hand H1 Packs to their own
// receive sequence through a shared budget that fits two Packs, and each comes
// straight back with its next Pack once a handoff succeeds, as a loop with
// more frames on its reader does. The test, standing in for the sequence
// workers, dequeues one Pack at a time in the order they were admitted. Each
// producer must get an equal share. The budget used to wake every waiting
// loop and let whichever reserved first win: on one processor the oldest woken
// loop ran first, reserved, came straight back and waited again ahead of the
// siblings it beat, so it got every freed byte and the other three none.
func TestReceiveSequenceSharedPackBudgetPassesToOldestWaitingLoop(t *testing.T) {
	// Keep the process-global pool statistics worker outside the bubble.
	MessagePoolReturn(MessagePoolGet(1))
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	synctest.Test(t, func(t *testing.T) {
		const packByteCount = 40
		const producerCount = 4
		const handoffCount = 8 * producerCount
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		budget := NewTransferMemoryBudget(2 * packByteCount)
		settings := DefaultReceiveBufferSettingsWithBufferSize(8)
		settings.PackQueueBudget = budget
		sequences := make([]*ReceiveSequence, producerCount)
		for index := range producerCount {
			sequences[index] = newReceiveSequence(ctx, &Client{}, SourceId(NewId()), NewId(), TransferKey{}, settings)
		}

		var producers sync.WaitGroup
		// Producers start, and so first wait, in index order. The first fills
		// the budget with two Packs and waits with its third.
		for index := range producerCount {
			producers.Go(func() {
				for {
					receivePack := &ReceivePack{
						MessageByteCount:   packByteCount,
						TransferFrameBytes: make([]byte, packByteCount),
						TransportType:      TransportTypeH1,
					}
					if accepted, err := sequences[index].Pack(receivePack, -1); !accepted || err != nil {
						receivePack.messagePoolReturn()
						return
					}
				}
			})
			synctest.Wait()
		}

		// Arrival order of the admitted Packs not yet dequeued, by producer.
		var admitted []int
		queuedCounts := make([]int, producerCount)
		takeAdmitted := func() {
			for index, sequence := range sequences {
				for ; queuedCounts[index] < len(sequence.packs); queuedCounts[index] += 1 {
					admitted = append(admitted, index)
				}
			}
		}
		takeAdmitted()
		if len(admitted) != 2 || admitted[0] != 0 || admitted[1] != 0 {
			t.Fatalf("Packs admitted before any dequeue by producer = %v, want [0 0]", admitted)
		}
		var order []int
		handoffCounts := make([]int, producerCount)
		for range handoffCount {
			index := admitted[0]
			admitted = admitted[1:]
			receivePack := <-sequences[index].packs
			queuedCounts[index] -= 1
			sequences[index].releasePackQueue(receivePack)
			receivePack.messagePoolReturn()
			// The freed bytes admit one waiting Pack, and its producer comes
			// back with the next one.
			synctest.Wait()
			before := len(admitted)
			takeAdmitted()
			if len(admitted) != before+1 {
				t.Fatalf("one dequeue admitted %d Packs, want 1", len(admitted)-before)
			}
			order = append(order, admitted[len(admitted)-1])
			handoffCounts[admitted[len(admitted)-1]] += 1
		}
		cancel()
		for _, sequence := range sequences {
			sequence.Close()
		}
		producers.Wait()
		if used := budget.UsedByteCount(); used != 0 {
			t.Fatalf("shared Pack budget holds %d bytes after every sequence closed", used)
		}

		t.Logf("admission order after the first two: %v", order)
		for index, count := range handoffCounts {
			if count != handoffCount/producerCount {
				t.Errorf("producer %d got %d of %d freed Pack budget slots, want %d each: %v", index, count, handoffCount, handoffCount/producerCount, handoffCounts)
			}
		}
	})
}

// A Pack waiting in line for budget bytes keeps its sequence's count and byte
// slots. One that gives up, by its timeout or by its sequence closing, leaves
// the line and returns its slots, and the next release admits the Pack behind
// it.
func TestReceiveSequencePackThatGivesUpLeavesTheBudgetLine(t *testing.T) {
	// Keep the process-global pool statistics worker outside the bubble.
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		const packByteCount = 40
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		budget := NewTransferMemoryBudget(packByteCount)
		settings := DefaultReceiveBufferSettingsWithBufferSize(8)
		settings.PackQueueBudget = budget
		newSequence := func() *ReceiveSequence {
			return newReceiveSequence(ctx, &Client{}, SourceId(NewId()), NewId(), TransferKey{}, settings)
		}
		newPack := func() *ReceivePack {
			return &ReceivePack{
				MessageByteCount:   packByteCount,
				TransferFrameBytes: make([]byte, packByteCount),
				TransportType:      TransportTypeH1,
			}
		}
		type packResult struct {
			accepted bool
			err      error
		}
		startPack := func(sequence *ReceiveSequence, timeout time.Duration) <-chan packResult {
			result := make(chan packResult, 1)
			go func() {
				receivePack := newPack()
				accepted, err := sequence.Pack(receivePack, timeout)
				if !accepted {
					receivePack.messagePoolReturn()
				}
				result <- packResult{accepted: accepted, err: err}
			}()
			synctest.Wait()
			return result
		}
		decided := func(result <-chan packResult) (packResult, bool) {
			select {
			case decided := <-result:
				return decided, true
			default:
				return packResult{}, false
			}
		}

		holder := newSequence()
		defer holder.Close()
		if accepted, err := holder.Pack(newPack(), 0); !accepted || err != nil {
			t.Fatalf("holder Pack = %t, %v", accepted, err)
		}
		timedOut, closed, last := newSequence(), newSequence(), newSequence()
		defer timedOut.Close()
		timedOutResult := startPack(timedOut, time.Second)
		closedResult := startPack(closed, -1)
		lastResult := startPack(last, -1)
		for _, sequence := range []*ReceiveSequence{timedOut, closed, last} {
			if count, byteCount := sequence.packQueueCount.Load(), sequence.packQueueByteCount.Load(); count != 1 || byteCount != packByteCount {
				t.Fatalf("Pack waiting for budget bytes holds %d slots of %d bytes, want 1 of %d", count, byteCount, packByteCount)
			}
		}

		time.Sleep(time.Second)
		synctest.Wait()
		if result, ok := decided(timedOutResult); !ok || result.accepted || result.err != nil {
			t.Fatalf("timed-out Pack = decided %t, %+v; want a refusal without an error", ok, result)
		}
		closed.Close()
		synctest.Wait()
		if result, ok := decided(closedResult); !ok || result.accepted || result.err == nil {
			t.Fatalf("Pack of a closed sequence = decided %t, %+v; want a refusal with an error", ok, result)
		}
		for _, sequence := range []*ReceiveSequence{timedOut, closed} {
			if count, byteCount := sequence.packQueueCount.Load(), sequence.packQueueByteCount.Load(); count != 0 || byteCount != 0 {
				t.Fatalf("Pack that gave up left %d slots of %d bytes", count, byteCount)
			}
		}

		receivePack := <-holder.packs
		holder.releasePackQueue(receivePack)
		receivePack.messagePoolReturn()
		synctest.Wait()
		if result, ok := decided(lastResult); !ok || !result.accepted || result.err != nil {
			t.Fatalf("Pack behind two that gave up = decided %t, %+v; want admitted", ok, result)
		}
		if used := budget.UsedByteCount(); used != packByteCount || len(last.packs) != 1 {
			t.Fatalf("budget holds %d bytes and the last sequence %d Packs, want %d and 1", used, len(last.packs), packByteCount)
		}
		last.Close()
		assertRetainedBudgetBalance(t, budget)
	})
}

// Physical-lane cancellation must withdraw the queued reservation as well as
// the sequence's count and byte slots. Otherwise the next budget release is
// granted to a Pack whose caller has already returned, starving live lanes.
func TestReceiveSequenceRetiredCarrierLeavesTheBudgetLine(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		const packByteCount = 40
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		budget := NewTransferMemoryBudget(packByteCount)
		if !budget.TryReserve(packByteCount) {
			t.Fatal("could not reserve the initial budget owner")
		}
		settings := DefaultReceiveBufferSettingsWithBufferSize(8)
		settings.PackQueueBudget = budget
		retired := newReceiveSequence(ctx, &Client{}, SourceId(NewId()), NewId(), TransferKey{}, settings)
		live := newReceiveSequence(ctx, &Client{}, SourceId(NewId()), NewId(), TransferKey{}, settings)
		defer retired.Close()
		var closeLive sync.Once
		defer closeLive.Do(live.Close)
		type packResult struct {
			accepted bool
			err      error
		}
		startPack := func(sequence *ReceiveSequence, carrierDone <-chan struct{}) <-chan packResult {
			result := make(chan packResult, 1)
			go func() {
				pack := &ReceivePack{
					MessageByteCount:   packByteCount,
					TransferFrameBytes: MessagePoolGet(packByteCount),
					TransportType:      TransportTypeH1,
					carrierDone:        carrierDone,
				}
				accepted, err := sequence.Pack(pack, -1)
				if !accepted {
					pack.messagePoolReturn()
				}
				result <- packResult{accepted: accepted, err: err}
			}()
			synctest.Wait()
			return result
		}
		carrierDone := make(chan struct{})
		retiredResult := startPack(retired, carrierDone)
		liveResult := startPack(live, nil)
		if retired.packQueueCount.Load() != 1 || live.packQueueCount.Load() != 1 {
			t.Fatal("both lanes must be waiting for the occupied shared budget")
		}
		close(carrierDone)
		synctest.Wait()
		result := <-retiredResult
		if result.accepted || result.err == nil {
			t.Fatalf("retired carrier result = %+v, want refused with an error", result)
		}
		if count, bytes := retired.packQueueCount.Load(), retired.packQueueByteCount.Load(); count != 0 || bytes != 0 {
			t.Errorf("retired carrier retained %d queue slots and %d bytes", count, bytes)
		}
		budget.Release(packByteCount)
		synctest.Wait()
		select {
		case result := <-liveResult:
			if !result.accepted || result.err != nil {
				t.Errorf("live carrier result = %+v, want admitted", result)
			}
		default:
			t.Error("the retired carrier consumed the grant owed to the next live lane")
		}
		cancel()
		synctest.Wait()
		closeLive.Do(live.Close)
		if used := budget.UsedByteCount(); used != 0 {
			t.Errorf("retired carrier left %d budget bytes after all callers joined", used)
		}
	})
}
