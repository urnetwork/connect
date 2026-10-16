// Dedicated TCP return producers share their provider's two prepaid return
// slots. A finished operation's slot belongs to the oldest waiting producer,
// so the runtime's wake order cannot decide whose socket reads reach Transfer.
package connect

import (
	"context"
	"runtime"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
)

// The root cause of two TCP flows taking every return slot of a provider. Four
// producers return packets through the two slots, and each comes straight
// back for its next packet once its operation finishes, as a socket reader
// with more data does. The test, standing in for Transfer, finishes one
// operation at a time in the order they started. Each producer must get an
// equal share. The slots used to wake every waiter and go to whichever
// producer reserved first: on one processor nothing a finish wakes can run
// before the finishing producer reserves again, so the first two producers
// took every slot and the other two got none.
func TestProviderTcpReturnSlotsPassToOldestWaitingProducer(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		provider := &RemoteUserNatProvider{
			ctx:              ctx,
			tcpReturnMemory:  NewTransferMemoryBudget(kib(64)),
			memoryOperations: newLifecycleAdmission(),
		}
		packet := make([]byte, 1400)
		const producerCount = 4
		const operationCount = 8 * producerCount
		admitted := make(chan int, producerCount)
		finishes := make([]chan struct{}, producerCount)
		var producers sync.WaitGroup
		// Producers start, and so first wait, in index order. The first two
		// take both slots.
		for index := range producerCount {
			finishes[index] = make(chan struct{})
			producers.Go(func() {
				for {
					memory, ok := provider.startReturnMemoryOperation([][]byte{packet}, receiveRecoveryModeTcpSocket)
					if !ok {
						return
					}
					admitted <- index
					select {
					case <-ctx.Done():
						provider.finishMemoryOperation(&memory)
						return
					case <-finishes[index]:
					}
					provider.finishMemoryOperation(&memory)
				}
			})
			synctest.Wait()
		}

		var running []int
		take := func() {
			for len(admitted) != 0 {
				running = append(running, <-admitted)
			}
		}
		take()
		if !slices.Equal(running, []int{0, 1}) {
			t.Fatalf("producers holding the two return slots = %v, want [0 1]", running)
		}
		var order []int
		operationCounts := make([]int, producerCount)
		for range operationCount {
			index := running[0]
			running = running[1:]
			finishes[index] <- struct{}{}
			// The finished producer queues its next operation, and the slot
			// it freed admits whoever is next.
			synctest.Wait()
			take()
			order = append(order, running[len(running)-1])
			operationCounts[running[len(running)-1]] += 1
		}
		cancel()
		producers.Wait()
		if used := provider.tcpReturnMemory.UsedByteCount(); used != 0 {
			t.Fatalf("return slots hold %d bytes after every producer stopped", used)
		}

		t.Logf("admission order after the first two: %v", order)
		for index, count := range operationCounts {
			if count != operationCount/producerCount {
				t.Errorf("producer %d got %d of %d freed return slots, want %d each: %v", index, count, operationCount, operationCount/producerCount, operationCounts)
			}
		}
	})
}
