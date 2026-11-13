// Concurrent senders on one SendSequence share its bounded pre-sequence
// admission. A released slot belongs to the oldest waiter it fits, so the
// runtime's wake order cannot decide which sender's Packs reach the sequence.
package connect

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

type sendPackAdmissionResult struct {
	acquired bool
	err      error
}

// Start one sender's blocking admission and return once it is decided or
// durably blocked, so senders started in turn queue in that order.
func startSendPackAdmission(
	sequence *SendSequence,
	sendPack *SendPack,
	timeout time.Duration,
) <-chan sendPackAdmissionResult {
	result := make(chan sendPackAdmissionResult, 1)
	go func() {
		acquired, err, _ := sequence.acquirePackAdmission(sendPack, timeout)
		result <- sendPackAdmissionResult{acquired: acquired, err: err}
	}()
	synctest.Wait()
	return result
}

// Read a sender's admission result without waiting for one.
func decidedSendPackAdmission(
	result <-chan sendPackAdmissionResult,
) (sendPackAdmissionResult, bool) {
	select {
	case decided := <-result:
		return decided, true
	default:
		return sendPackAdmissionResult{}, false
	}
}

// The root cause of one sender taking every slot. A release hands its slot to
// the oldest waiter before any woken goroutine runs, so a sender that comes
// straight back for its next Pack must queue behind the waiters it left.
// Admission used to wake every waiter and give the slot to whichever acquire
// ran first, which here is the returning sender: on one processor, nothing the
// release wakes can run before this goroutine's next line.
func TestSendPackAdmissionGivesReleasedSlotToOldestWaiter(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	synctest.Test(t, func(t *testing.T) {
		sequence := &SendSequence{ctx: t.Context(), packAdmission: newSendPackAdmission(1)}
		holder := &SendPack{Ctx: t.Context()}
		if acquired, err, _ := sequence.acquirePackAdmission(holder, 0); !acquired || err != nil {
			t.Fatalf("holder admission = %t, %v", acquired, err)
		}
		first := &SendPack{Ctx: t.Context()}
		firstResult := startSendPackAdmission(sequence, first, -1)
		second := &SendPack{Ctx: t.Context()}
		secondResult := startSendPackAdmission(sequence, second, -1)

		holder.releaseAdmission()
		returning := &SendPack{Ctx: t.Context()}
		if acquired, err, _ := sequence.acquirePackAdmission(returning, 0); acquired || err != nil {
			t.Fatalf("a sender returning after the release took the slot ahead of both waiters: %t, %v", acquired, err)
		}
		synctest.Wait()
		if result, decided := decidedSendPackAdmission(firstResult); !decided || !result.acquired || result.err != nil {
			t.Fatalf("oldest waiter after the release = decided %t, %+v; want admitted", decided, result)
		}
		if result, decided := decidedSendPackAdmission(secondResult); decided {
			t.Fatalf("second waiter decided %+v while the oldest holds the only slot", result)
		}

		first.releaseAdmission()
		synctest.Wait()
		if result, decided := decidedSendPackAdmission(secondResult); !decided || !result.acquired || result.err != nil {
			t.Fatalf("second waiter after the oldest released = decided %t, %+v; want admitted", decided, result)
		}
		second.releaseAdmission()
		if count := sequence.packAdmission.count; count != 0 {
			t.Fatalf("admission holds %d slots after every Pack released", count)
		}
	})
}

// Eight senders block on one real sequence's two admission slots while the
// test, standing in for the paused send loop, drains one admitted Pack at a
// time. Each sender must get an equal share. Before the fix the sender that
// waited first won every slot on one processor: the runtime readies a woken
// group newest first, so the oldest waiter ran first, took the slot, and waited
// again ahead of the siblings it beat, which then got nothing.
func TestSendSequenceConcurrentSendersShareAdmissionOnOneProcessor(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	synctest.Test(t, func(t *testing.T) {
		const senderCount = 8
		const drainCount = 8 * senderCount
		fixture := newSendPackCallerOwnerFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var senders sync.WaitGroup
		// Senders start, and so first wait, in index order. The first fills
		// both slots before the rest start.
		for index := range senderCount {
			value := fmt.Sprintf("sender-%d", index)
			senders.Go(func() {
				for {
					if _, accepted, _ := fixture.send(ctx, value, -1); !accepted {
						return
					}
				}
			})
			synctest.Wait()
		}
		sequence := fixture.currentSequence()
		if sequence == nil || len(sequence.packs) != 2 || sequence.packAdmission.count != 2 {
			t.Fatal("senders did not fill the sequence's two admission slots")
		}

		drain := func() string {
			sendPack := <-sequence.packs
			value := string(sendPack.Frame.MessageBytes)
			sendPack.returnFrames()
			sendPack.releaseRaw()
			return value
		}
		drainedCounts := map[string]int{}
		for range drainCount {
			drainedCounts[drain()] += 1
			// The freed slot's sender queues its Pack and its next attempt.
			synctest.Wait()
		}
		cancel()
		senders.Wait()
		for len(sequence.packs) != 0 {
			drain()
		}
		if count := sequence.packAdmission.count; count != 0 {
			t.Fatalf("admission holds %d slots after every Pack drained", count)
		}

		// The first sender's head start and the two admitted Packs are the
		// only unevenness an arrival-order handoff leaves.
		t.Logf("admitted Packs per sender over %d drains: %v", drainCount, drainedCounts)
		share := drainCount / senderCount
		for index := range senderCount {
			value := fmt.Sprintf("sender-%d", index)
			if drained := drainedCounts[value]; drained < share-2 {
				t.Errorf("%s got %d of the first %d admitted Packs, want about %d; all: %v", value, drained, drainCount, share, drainedCounts)
			}
		}
	})
}

// With flow isolation one flow may hold at most capacity-1 slots. A waiter
// whose flow is at that limit keeps its place while a later waiter of another
// flow takes a released slot, and it is admitted when its own flow releases.
func TestSendPackAdmissionWaiterAtItsFlowLimitKeepsItsPlace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sequence := &SendSequence{ctx: t.Context(), packAdmission: newSendPackAdmission(4)}
		sequence.flowIsolation.Store(true)
		bulkKey := testSendSchedulingKey(1000)
		interactiveKey := testSendSchedulingKey(2000)
		var bulkPacks []*SendPack
		for index := range 3 {
			bulkPack := &SendPack{Ctx: t.Context(), schedulingKey: bulkKey}
			if acquired, err, _ := sequence.acquirePackAdmission(bulkPack, 0); !acquired || err != nil {
				t.Fatalf("bulk Pack %d admission = %t, %v", index, acquired, err)
			}
			bulkPacks = append(bulkPacks, bulkPack)
		}
		interactive := &SendPack{Ctx: t.Context(), schedulingKey: interactiveKey}
		if acquired, err, _ := sequence.acquirePackAdmission(interactive, 0); !acquired || err != nil {
			t.Fatalf("interactive Pack in the reserved slot = %t, %v", acquired, err)
		}
		bulkWaiter := &SendPack{Ctx: t.Context(), schedulingKey: bulkKey}
		bulkResult := startSendPackAdmission(sequence, bulkWaiter, -1)
		interactiveWaiter := &SendPack{Ctx: t.Context(), schedulingKey: interactiveKey}
		interactiveResult := startSendPackAdmission(sequence, interactiveWaiter, -1)

		interactive.releaseAdmission()
		synctest.Wait()
		if result, decided := decidedSendPackAdmission(interactiveResult); !decided || !result.acquired || result.err != nil {
			t.Fatalf("interactive waiter behind a bulk waiter at its limit = decided %t, %+v; want admitted", decided, result)
		}
		if result, decided := decidedSendPackAdmission(bulkResult); decided {
			t.Fatalf("bulk waiter decided %+v while its flow holds its limit", result)
		}

		bulkPacks[0].releaseAdmission()
		synctest.Wait()
		if result, decided := decidedSendPackAdmission(bulkResult); !decided || !result.acquired || result.err != nil {
			t.Fatalf("bulk waiter after its flow released = decided %t, %+v; want admitted", decided, result)
		}
		for _, sendPack := range []*SendPack{bulkPacks[1], bulkPacks[2], bulkWaiter, interactiveWaiter} {
			sendPack.releaseAdmission()
		}
		if count := sequence.packAdmission.count; count != 0 {
			t.Fatalf("admission holds %d slots after every Pack released", count)
		}
	})
}

// A waiter that gives up, by cancellation or by its timeout, leaves the queue
// without taking a slot: the next release goes to the waiter behind it.
func TestSendPackAdmissionWaiterThatGivesUpLeavesTheQueue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sequence := &SendSequence{ctx: t.Context(), packAdmission: newSendPackAdmission(1)}
		holder := &SendPack{Ctx: t.Context()}
		if acquired, err, _ := sequence.acquirePackAdmission(holder, 0); !acquired || err != nil {
			t.Fatalf("holder admission = %t, %v", acquired, err)
		}
		canceledCtx, cancel := context.WithCancel(t.Context())
		canceledResult := startSendPackAdmission(sequence, &SendPack{Ctx: canceledCtx}, -1)
		timedOutResult := startSendPackAdmission(sequence, &SendPack{Ctx: t.Context()}, time.Second)
		last := &SendPack{Ctx: t.Context()}
		lastResult := startSendPackAdmission(sequence, last, -1)

		cancel()
		synctest.Wait()
		if result, decided := decidedSendPackAdmission(canceledResult); !decided || result.acquired || result.err == nil {
			t.Fatalf("canceled waiter = decided %t, %+v; want a refusal with an error", decided, result)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if result, decided := decidedSendPackAdmission(timedOutResult); !decided || result.acquired || result.err != nil {
			t.Fatalf("timed-out waiter = decided %t, %+v; want a refusal without an error", decided, result)
		}

		holder.releaseAdmission()
		synctest.Wait()
		if result, decided := decidedSendPackAdmission(lastResult); !decided || !result.acquired || result.err != nil {
			t.Fatalf("waiter behind two that gave up = decided %t, %+v; want admitted", decided, result)
		}
		last.releaseAdmission()
		if count := sequence.packAdmission.count; count != 0 {
			t.Fatalf("admission holds %d slots after every Pack released", count)
		}
	})
}

// Closing admission ends every wait with the closed sequence's error and
// refuses later acquires. A Pack admitted before the close still releases.
func TestSendPackAdmissionCloseEndsEveryWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sequence := &SendSequence{ctx: t.Context(), packAdmission: newSendPackAdmission(1)}
		holder := &SendPack{Ctx: t.Context()}
		if acquired, err, _ := sequence.acquirePackAdmission(holder, 0); !acquired || err != nil {
			t.Fatalf("holder admission = %t, %v", acquired, err)
		}
		firstResult := startSendPackAdmission(sequence, &SendPack{Ctx: t.Context()}, -1)
		secondResult := startSendPackAdmission(sequence, &SendPack{Ctx: t.Context()}, -1)

		sequence.packAdmission.close()
		synctest.Wait()
		for _, waiterResult := range []<-chan sendPackAdmissionResult{firstResult, secondResult} {
			if result, decided := decidedSendPackAdmission(waiterResult); !decided || result.acquired || result.err == nil {
				t.Fatalf("waiter after close = decided %t, %+v; want a refusal with an error", decided, result)
			}
		}
		if acquired, err, _ := sequence.acquirePackAdmission(&SendPack{Ctx: t.Context()}, -1); acquired || err == nil {
			t.Fatalf("admission after close = %t, %v; want a refusal with an error", acquired, err)
		}
		holder.releaseAdmission()
		if count := sequence.packAdmission.count; count != 0 {
			t.Fatalf("admission holds %d slots after the admitted Pack released", count)
		}
	})
}
