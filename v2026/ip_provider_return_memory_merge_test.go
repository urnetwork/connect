// A queued FIFO grant remains an owned reservation when its flow retires.
// Abandoning that grant must pass the slot on without dropping source custody.
package connect

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
)

// Forces cancellation after the real budget grants a waiting producer, before
// provider admission returns. A healthy next source must receive that same slot.
func TestProviderReturnMemoryCanceledGrantPassesToNextSource(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newTcpProviderMemoryRetirementFixture(t, NewTransferMemoryBudget(mib(2)))
		provider := fixture.provider
		packet := MessagePoolCopy(ipOosTcpPacket(fixture.path, tcpFlagAck, nil))
		defer MessagePoolReturn(packet)
		first, ok := provider.startReturnMemoryOperation([][]byte{packet}, receiveRecoveryModeTcpSocket)
		if !ok {
			t.Fatal("first workspace was not admitted")
		}
		releaseFirst := sync.OnceFunc(func() { provider.finishMemoryOperation(&first) })
		defer releaseFirst()
		second, ok := provider.startReturnMemoryOperation([][]byte{packet}, receiveRecoveryModeTcpSocket)
		if !ok {
			t.Fatal("second workspace was not admitted")
		}
		releaseSecond := sync.OnceFunc(func() { provider.finishMemoryOperation(&second) })
		defer releaseSecond()
		granted, continueGrant := make(chan struct{}), make(chan struct{})
		releaseGrant := sync.OnceFunc(func() { close(continueGrant) })
		provider.afterReturnMemoryGrantForTest = sync.OnceFunc(func() {
			close(granted)
			<-continueGrant
		})
		flowCtx, cancelFlow := context.WithCancel(context.Background())
		finish := make(chan struct{})
		releaseOwners := sync.OnceFunc(func() { close(finish) })
		var workers sync.WaitGroup
		defer func() {
			cancelFlow()
			releaseGrant()
			releaseOwners()
			releaseFirst()
			releaseSecond()
			workers.Wait()
		}()
		start := func(sourceId Id, ctx context.Context) <-chan bool {
			result := make(chan bool, 1)
			workers.Go(func() {
				memory, source, admitted := provider.startReturnMemoryOperationForSource(
					[][]byte{packet}, receiveRecoveryModeDedicatedTcpControl, sourceId, ctx)
				result <- admitted
				if admitted {
					<-finish
					provider.releaseSourceLifecycle(sourceId, source)
					provider.finishMemoryOperation(&memory)
				}
			})
			return result
		}
		oldResult := start(NewId(), flowCtx)
		synctest.Wait()
		healthyResult := start(NewId(), context.Background())
		synctest.Wait()
		budget := provider.tcpReturnMemory
		budget.admissionLock.Lock()
		queued := len(budget.reserveWaiters)
		budget.admissionLock.Unlock()
		if queued != 2 || budget.UsedByteCount() != kib(64) {
			t.Fatal("two source owners did not queue behind the held workspaces")
		}
		releaseFirst()
		<-granted
		if budget.UsedByteCount() != kib(64) {
			t.Fatal("grant barrier did not retain real reserved bytes")
		}
		cancelFlow()
		releaseGrant()
		if <-oldResult {
			t.Error("retired flow accepted an already-granted workspace")
			releaseOwners()
		}
		synctest.Wait()
		select {
		case admitted := <-healthyResult:
			if !admitted {
				t.Error("abandoned grant did not pass to the healthy next source")
			}
		default:
			t.Fatal("abandoned grant left the healthy source waiting")
		}
		if budget.UsedByteCount() != kib(64) || provider.ctx.Err() != nil || fixture.client.Ctx().Err() != nil {
			t.Error("one retired flow lost an independent slot or canceled a shared owner")
		}
		releaseOwners()
		releaseSecond()
		workers.Wait()
		provider.memoryOperations.stateLock.Lock()
		active := provider.memoryOperations.activeCount
		provider.memoryOperations.stateLock.Unlock()
		provider.stateLock.Lock()
		sources := len(provider.sourceLifecycles)
		provider.stateLock.Unlock()
		budget.admissionLock.Lock()
		queued = len(budget.reserveWaiters)
		budget.admissionLock.Unlock()
		if active != 0 || sources != 0 || queued != 0 || budget.UsedByteCount() != 0 {
			t.Fatalf("joined producers retained gates/sources/waiters/bytes: %d/%d/%d/%d", active, sources, queued, budget.UsedByteCount())
		}
	})
}
