// Controls pin exact optional-credit ownership without timers or queue polling.
package connect

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/v2026/protocol"
)

// The queue uses the same bounded constructor settings and real hierarchy.
func adaptiveReturnControl(t *testing.T, rootLimit ByteCount) (context.Context, context.CancelFunc, *TcpBufferSettings, *TransferMemoryBudget, natMemoryReservation, *tcpReturnReadQueue) {
	t.Helper()
	root := NewTransferMemoryBudget(rootLimit)
	settings := DefaultLocalUserNatSettingsWithBufferSize(64)
	settings.MemoryBudget = NewTransferMemoryBudgetWithParent(rootLimit, root)
	bounded := boundedNatMemorySettings(settings).TcpBufferSettings
	ctx, cancel := context.WithCancel(context.Background())
	memory, admitted := reserveNatMemory(bounded.MemoryBudget, natTcpFlowMemoryByteCount(bounded))
	if !admitted {
		t.Fatal("control cold claim refused")
	}
	queue := newTcpReturnReadQueue(ctx, bounded, newTcpReturnBatchWorkspace(ctx, bounded))
	return ctx, cancel, bounded, root, memory, queue
}

// Only a successful queue offer takes the newly allocated root.
func adaptiveReturnOffer(t *testing.T, queue *tcpReturnReadQueue) {
	t.Helper()
	packet := MessagePoolCopy(make([]byte, DefaultMtu))
	if !queue.offer(packet) {
		MessagePoolReturn(packet)
		t.Fatal("ready control packet refused")
	}
}

// Matches TcpSequence.receiveBatch's synchronous original-root return.
func adaptiveReturnFinish(queue *tcpReturnReadQueue, packets [][]byte) {
	for _, packet := range packets {
		MessagePoolReturn(packet)
	}
	queue.finishBatch()
}

// Closing a cold constructor still reaches its real Run join/release owner.
func TestTcpAdaptiveColdConstructorKeepsCap2Claim(t *testing.T) {
	assertMessagePoolOwnership(t)
	settings := DefaultLocalUserNatSettingsWithBufferSize(64)
	settings.MemoryBudget = NewTransferMemoryBudget(mib(2))
	boundedSettings := boundedNatMemorySettings(settings)
	bounded := boundedSettings.TcpBufferSettings
	if boundedSettings.UdpBufferSettings.WriteBatchSize != 2 || bounded.ReadBufferByteCount != 4096 {
		t.Fatal("adaptive TCP batching changed UDP or the socket read working set")
	}
	cold := *bounded
	cold.WriteBatchSize = bounded.MemoryColdWriteBatchSize
	if natTcpFlowMemoryByteCount(bounded) != natTcpFundedFlowMemoryByteCount(&cold) || bounded.WriteBatchSize != 8 {
		t.Fatal("cold constructor prepays optional cap8 owners")
	}
	budget := NewTransferMemoryBudget(natTcpFlowMemoryByteCount(bounded))
	copySettings := *bounded
	copySettings.MemoryBudget, copySettings.ReturnQueueBudget = budget, budget
	sequence := newTcpSequenceWithTransferKey(context.Background(), nil, TransferPath{}, TransferKey{}, protocol.ProvideMode_Public,
		4, net.ParseIP("192.0.2.1"), 10001, net.ParseIP("198.51.100.1"), 443, 100, &copySettings)
	if sequence == nil || budget.UsedByteCount() != budget.TotalByteCount() || sequence.ipPath.tcpReturnBatchWorkspace.funded.Load() || sequence.ipPath.tcpReturnBatchWorkspace.acquire() {
		t.Fatal("cold constructor lacks identity or acquired unfunded scratch")
	}
	sequence.Close()
	sequence.Run()
	if budget.UsedByteCount() != 0 {
		t.Fatal("canceled cold constructor retained claim")
	}
}

// Two queued packets cost only the base; the actual third funds the whole delta.
func TestTcpAdaptiveHotReadyBoundaryFundsExclusiveOwners(t *testing.T) {
	assertMessagePoolOwnership(t)
	ctx, cancel, settings, root, memory, queue := adaptiveReturnControl(t, mib(2))
	defer cancel()
	defer memory.release()
	base := root.UsedByteCount()
	adaptiveReturnOffer(t, queue)
	adaptiveReturnOffer(t, queue)
	if root.UsedByteCount() != base || queue.workspace.funded.Load() {
		t.Fatal("speculative hot reservation")
	}
	adaptiveReturnOffer(t, queue)
	want := natTcpFundedFlowMemoryByteCount(settings)
	if root.UsedByteCount() != want || !queue.workspace.funded.Load() {
		t.Fatal("third ready packet lacks full incremental claim")
	}
	batch, ok := queue.takeBatch(ctx, 0)
	if !ok || len(batch) != 3 || !queue.workspace.acquire() || queue.workspace.acquire() {
		t.Fatal("hot callback witness not exclusive")
	}
	if root.UsedByteCount() != want {
		t.Fatal("drain released callback roots early")
	}
	queue.workspace.release()
	adaptiveReturnFinish(queue, batch)
	if root.UsedByteCount() != base || queue.workspace.funded.Load() {
		t.Fatal("empty callback did not return optional claim")
	}
	queue.close()
	queue.releaseAfterJoin()
}

// An unrelated root owner can refuse promotion without consuming the leaf.
// The third packet then resumes through the original cap2 queue after a drain.
func TestTcpAdaptiveFullRootFallsBackWithoutBudgetWait(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel, settings, root, memory, queue := adaptiveReturnControl(t, mib(2))
		defer cancel()
		defer memory.release()
		base := root.UsedByteCount()
		other, ok := reserveNatMemory(root, root.TotalByteCount()-base)
		if !ok {
			t.Fatal("control root not filled")
		}
		defer other.release()
		var attempts atomic.Int64
		queue.afterFundingAttemptForTest = func(admitted bool) {
			if admitted {
				t.Error("full root admitted optional claim")
			}
			attempts.Add(1)
		}
		for range 3 {
			adaptiveReturnOffer(t, queue)
			adaptiveReturnOffer(t, queue)
			packet := MessagePoolCopy(make([]byte, DefaultMtu))
			done := make(chan bool, 1)
			go func() {
				accepted := queue.offer(packet)
				if !accepted {
					MessagePoolReturn(packet)
				}
				done <- accepted
			}()
			synctest.Wait()
			select {
			case <-done:
				t.Fatal("cold full queue admitted third without credit")
			default:
			}
			if settings.MemoryBudget.UsedByteCount() != base || root.UsedByteCount() != root.TotalByteCount() || queue.workspace.funded.Load() {
				t.Fatal("refused promotion changed exact claims")
			}
			batch, ok := queue.takeBatch(ctx, 0)
			if !ok || len(batch) != 2 {
				t.Fatal("fallback did not retain cap2")
			}
			adaptiveReturnFinish(queue, batch)
			if !<-done {
				t.Fatal("fallback lost already-owned packet")
			}
			batch, ok = queue.takeBatch(ctx, 0)
			if !ok || len(batch) != 1 {
				t.Fatal("fallback retry changed identity count")
			}
			adaptiveReturnFinish(queue, batch)
		}
		if attempts.Load() != 3 {
			t.Fatalf("promotion attempts=%d want3 actual full boundaries", attempts.Load())
		}
		queue.close()
		queue.releaseAfterJoin()
	})
}

// Actual queued bulk across callback boundaries reuses one grant. Empty-tail
// demotion is deliberately measured separately, not called universally churn-free.
func TestTcpAdaptiveQueuedBulkRetainsOneGrant(t *testing.T) {
	assertMessagePoolOwnership(t)
	ctx, cancel, _, root, memory, queue := adaptiveReturnControl(t, mib(2))
	defer cancel()
	defer memory.release()
	base := root.UsedByteCount()
	attempts := 0
	queue.afterFundingAttemptForTest = func(admitted bool) {
		if !admitted {
			t.Fatal("funded bulk refused")
		}
		attempts++
	}
	for range 8 {
		adaptiveReturnOffer(t, queue)
	}
	for cycle := range 4 {
		batch, ok := queue.takeBatch(ctx, 0)
		if !ok || len(batch) != 8 {
			t.Fatal("ready bulk lost cap8")
		}
		if cycle != 3 {
			for range 8 {
				adaptiveReturnOffer(t, queue)
			}
		}
		adaptiveReturnFinish(queue, batch)
		if cycle != 3 && (!queue.workspace.funded.Load() || root.UsedByteCount() == base) {
			t.Fatal("queued bulk churned reservation")
		}
	}
	if attempts != 1 || root.UsedByteCount() != base {
		t.Fatal("steady queued grant or final release changed")
	}
	queue.close()
	queue.releaseAfterJoin()
}

// Cancellation after exact admission but before permission publication returns
// the grant and leaves the pending packet caller-owned; queued originals drain.
func TestTcpAdaptiveCanceledPromotionNeverPublishesPermission(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		_, cancel, _, root, memory, queue := adaptiveReturnControl(t, mib(2))
		defer cancel()
		defer memory.release()
		base := root.UsedByteCount()
		adaptiveReturnOffer(t, queue)
		adaptiveReturnOffer(t, queue)
		entered, release := make(chan struct{}), make(chan struct{})
		queue.afterFundingAttemptForTest = func(admitted bool) {
			if !admitted {
				t.Error("expected real optional admission")
			}
			close(entered)
			<-release
		}
		packet := MessagePoolCopy(make([]byte, DefaultMtu))
		done := make(chan bool, 1)
		go func() {
			accepted := queue.offer(packet)
			if !accepted {
				MessagePoolReturn(packet)
			}
			done <- accepted
		}()
		<-entered
		if root.UsedByteCount() == base || queue.workspace.funded.Load() {
			t.Fatal("test missed prepublication credit boundary")
		}
		cancel()
		close(release)
		if <-done || root.UsedByteCount() != base || queue.workspace.funded.Load() {
			t.Fatal("canceled optional admission escaped")
		}
		queue.close()
		batch, ok := queue.takeBatch(context.Background(), 1)
		if !ok || len(batch) != 1 {
			t.Fatal("canceled original lost drain")
		}
		adaptiveReturnFinish(queue, batch)
		batch, ok = queue.takeBatch(context.Background(), 1)
		if !ok || len(batch) != 1 {
			t.Fatal("second original lost drain")
		}
		adaptiveReturnFinish(queue, batch)
		queue.releaseAfterJoin()
	})
}

// Parent cancellation cannot erase an oversized callback's original roots.
func TestTcpAdaptiveCancellationKeepsLiveCallbackFunding(t *testing.T) {
	assertMessagePoolOwnership(t)
	ctx, cancel, settings, root, memory, queue := adaptiveReturnControl(t, mib(2))
	defer cancel()
	defer memory.release()
	base := root.UsedByteCount()
	for range 3 {
		adaptiveReturnOffer(t, queue)
	}
	batch, ok := queue.takeBatch(ctx, 0)
	if !ok || len(batch) != 3 || !queue.workspace.acquire() {
		t.Fatal("live callback boundary absent")
	}
	cancel()
	queue.close()
	if root.UsedByteCount() != natTcpFundedFlowMemoryByteCount(settings) || !queue.workspace.inUse.Load() || !queue.workspace.funded.Load() {
		t.Fatal("cancellation revoked owned callback credit")
	}
	queue.workspace.release()
	adaptiveReturnFinish(queue, batch)
	queue.releaseAfterJoin()
	if root.UsedByteCount() != base {
		t.Fatal("joined callback retained optional credit")
	}
}

// Empty-tail release is safe but intentionally reveals its reserve/release
// cost. Three separately drained bursts must not masquerade as one hot grant.
func TestTcpAdaptiveColdBurstsExposeReservationChurn(t *testing.T) {
	assertMessagePoolOwnership(t)
	ctx, cancel, _, root, memory, queue := adaptiveReturnControl(t, mib(2))
	defer cancel()
	defer memory.release()
	base := root.UsedByteCount()
	attempts := 0
	queue.afterFundingAttemptForTest = func(admitted bool) {
		if !admitted {
			t.Fatal("burst funding refused")
		}
		attempts++
	}
	for range 3 {
		for range 3 {
			adaptiveReturnOffer(t, queue)
		}
		batch, ok := queue.takeBatch(ctx, 0)
		if !ok || len(batch) != 3 {
			t.Fatal("real ready burst changed")
		}
		adaptiveReturnFinish(queue, batch)
		if root.UsedByteCount() != base || queue.workspace.funded.Load() {
			t.Fatal("empty burst retained optional owners")
		}
	}
	if attempts != 3 {
		t.Fatalf("actual burst funding attempts=%d want3", attempts)
	}
	queue.close()
	queue.releaseAfterJoin()
}

// A reset already built before cancellation keeps its original unconditional
// queue handoff. Both ordinary and adaptive queues drain it after cancellation.
func TestTcpAdaptiveBuiltFinalControlSurvivesCancellation(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, maximum := range []int{2, 8} {
		ctx, cancel, settings, root, memory, initial := adaptiveReturnControl(t, mib(2))
		copySettings := *settings
		copySettings.WriteBatchSize = maximum
		queue := newTcpReturnReadQueue(ctx, &copySettings, newTcpReturnBatchWorkspace(ctx, &copySettings))
		base := root.UsedByteCount()
		// The packet exists before cancellation, exactly as in Run's deferred
		// reset branch after its one preconstruction context check.
		packet := MessagePoolCopy([]byte{4, 1, 2, 3})
		cancel()
		if !queue.offerFinal(packet) {
			MessagePoolReturn(packet)
			t.Fatal("already-built final control lost unconditional handoff")
		}
		if root.UsedByteCount() != base {
			t.Fatal("final control speculatively promoted")
		}
		queue.close()
		batch, ok := queue.takeBatch(context.Background(), 1)
		if !ok || len(batch) != 1 || len(batch[0]) != 4 || batch[0][3] != 3 {
			t.Fatal("final control identity changed in canceled drain")
		}
		adaptiveReturnFinish(queue, batch)
		queue.releaseAfterJoin()
		initial.close()
		initial.releaseAfterJoin()
		memory.release()
		if root.UsedByteCount() != 0 {
			t.Fatal("final control cleanup retained budget")
		}
	}
}

// Queue demand can disappear while the shared admission lock runs. Exact
// credit then returns before a third packet enters the still-cold queue.
func TestTcpAdaptiveDisappearedDemandReturnsUnpublishedGrant(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel, _, root, memory, queue := adaptiveReturnControl(t, mib(2))
		defer cancel()
		defer memory.release()
		base := root.UsedByteCount()
		adaptiveReturnOffer(t, queue)
		adaptiveReturnOffer(t, queue)
		entered, release := make(chan struct{}), make(chan struct{})
		queue.afterFundingAttemptForTest = func(admitted bool) {
			if !admitted {
				t.Error("control grant absent")
			}
			close(entered)
			<-release
		}
		packet := MessagePoolCopy(make([]byte, DefaultMtu))
		done := make(chan bool, 1)
		go func() {
			accepted := queue.offer(packet)
			if !accepted {
				MessagePoolReturn(packet)
			}
			done <- accepted
		}()
		<-entered
		batch, ok := queue.takeBatch(ctx, 0)
		if !ok || len(batch) != 2 || queue.workspace.funded.Load() {
			t.Fatal("unpublished funding enlarged cold drain")
		}
		adaptiveReturnFinish(queue, batch)
		if root.UsedByteCount() == base {
			t.Fatal("test did not retain the exact pending grant")
		}
		close(release)
		if !<-done || root.UsedByteCount() != base || queue.workspace.funded.Load() {
			t.Fatal("vanished demand retained speculative permission")
		}
		batch, ok = queue.takeBatch(ctx, 0)
		if !ok || len(batch) != 1 {
			t.Fatal("third original packet not cold-admitted")
		}
		adaptiveReturnFinish(queue, batch)
		queue.close()
		queue.releaseAfterJoin()
	})
}
