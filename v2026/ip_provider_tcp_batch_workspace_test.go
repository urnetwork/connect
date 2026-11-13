// Adaptive callback permission retains the provider's two equal scratch slots.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
)

// An idle real flow supplies the original witness. A controlled ready queue
// exercises its exact funding/admission boundary; the socket-worker control
// separately proves that Run carries real ordered bytes through this path.
func TestTcpAdaptiveProviderSlotAndOriginWitness(t *testing.T) {
	assertMessagePoolOwnership(t)
	budget := NewTransferMemoryBudget(mib(2))
	synctest.Test(t, func(t *testing.T) {
		fixture := newTcpProviderMemoryRetirementFixtureWithWriteBatchSize(t, budget, 8)
		fixture.send(tcpFlagSyn, 1000, 0, nil)
		sequence := fixture.nextSequence(1000)
		fixture.takeControls()
		synctest.Wait()
		workspace := sequence.ipPath.tcpReturnBatchWorkspace
		if workspace == nil || workspace.ctx != sequence.ctx || workspace.funded.Load() || workspace.acquire() {
			t.Fatal("cold constructor published funded callback permission")
		}
		base := budget.UsedByteCount()
		queue := newTcpReturnReadQueue(sequence.ctx, sequence.tcpBufferSettings, workspace)
		defer func() {
			queue.close()
			for {
				packets, ok := queue.takeBatch(context.Background(), 0)
				if !ok {
					break
				}
				adaptiveReturnFinish(queue, packets)
			}
			queue.releaseAfterJoin()
		}()
		for range 2 {
			adaptiveReturnOffer(t, queue)
		}
		if budget.UsedByteCount() != base || workspace.funded.Load() {
			t.Fatal("two ready roots speculatively funded callback scratch")
		}
		for range 6 {
			adaptiveReturnOffer(t, queue)
		}
		if budget.UsedByteCount() != base+workspace.incrementBytes || !workspace.funded.Load() {
			t.Fatal("expanded queue did not retain the complete optional claim")
		}
		func() {
			packets, ok := queue.takeBatch(sequence.ctx, 0)
			if !ok {
				t.Fatal("funded queue did not expose ready roots")
			}
			defer adaptiveReturnFinish(queue, packets)
			if len(packets) != 8 {
				t.Fatal("funded queue changed the eight ready roots")
			}
			if !workspace.acquire() {
				t.Fatal("funded originating callback could not borrow its workspace")
			}
			defer workspace.release()
			if workspace.acquire() {
				t.Fatal("same flow scratch borrowed concurrently")
			}
			provider := fixture.provider
			memory, source, ok := provider.startReturnMemoryOperationForSourceWithWorkspace(packets, receiveRecoveryModeTcpSocket, fixture.source.SourceId, sequence.ctx, workspace)
			if !ok {
				t.Fatal("funded originating eight-packet callback refused")
			}
			defer provider.finishMemoryOperation(&memory)
			defer provider.releaseSourceLifecycle(fixture.source.SourceId, source)
			if memory.bytes != kib(32) {
				t.Fatal("large callback monopolized the shared provider partition")
			}
			siblingCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			siblingId := NewId()
			func() {
				sibling, siblingSource, ok := provider.startReturnMemoryOperationForSource(packets[:1], receiveRecoveryModeTcpSocket, siblingId, siblingCtx)
				if !ok {
					t.Fatal("large callback blocked the independent sibling slot")
				}
				defer provider.finishMemoryOperation(&sibling)
				defer provider.releaseSourceLifecycle(siblingId, siblingSource)
				if sibling.bytes != kib(32) || provider.tcpReturnMemory.UsedByteCount() != kib(64) {
					t.Fatal("provider callback owners did not retain two equal slots")
				}
			}()
			for _, witness := range []*tcpReturnBatchWorkspace{workspace, nil} {
				flowCtx := sequence.ctx
				if witness != nil {
					flowCtx = siblingCtx
				}
				invalid, invalidSource, admitted := provider.startReturnMemoryOperationForSourceWithWorkspace(packets, receiveRecoveryModeTcpSocket, fixture.source.SourceId, flowCtx, witness)
				if admitted {
					provider.releaseSourceLifecycle(fixture.source.SourceId, invalidSource)
					provider.finishMemoryOperation(&invalid)
					t.Fatal("foreign or missing witness escaped the callback permission guard")
				}
			}
		}()
		if budget.UsedByteCount() != base || workspace.funded.Load() || workspace.inUse.Load() || fixture.provider.tcpReturnMemory.UsedByteCount() != 0 {
			t.Fatal("completed callback retained optional credit or provider scratch")
		}
	})
	if budget.UsedByteCount() != 0 {
		t.Fatal("joined provider/flow graph retained a claim")
	}
}
