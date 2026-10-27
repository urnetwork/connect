// Focused controls for the prepaid workspace boundary. The separate real-TCP
// root supplies the full socket/worker/share proof; these pin each exact owner.
package connect

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

// Keeps two explicit workspace owners while a third source/flow waits. This
// isolated gate control never claims that reservations are real socket data.
func runProviderReturnMemoryOwnerCancellation(t *testing.T, owner string) {
	t.Helper()
	assertMessagePoolOwnership(t)
	var budget *TransferMemoryBudget
	synctest.Test(t, func(t *testing.T) {
		budget = NewTransferMemoryBudget(mib(2))
		fixture := newTcpProviderMemoryRetirementFixture(t, budget)
		provider := fixture.provider
		packet := MessagePoolCopy(ipOosTcpPacket(fixture.path, tcpFlagAck, nil))
		defer MessagePoolReturn(packet)
		first, ok := provider.startReturnMemoryOperation([][]byte{packet}, receiveRecoveryModeTcpSocket)
		if !ok {
			t.Fatal("first workspace reservation failed")
		}
		second, ok := provider.startReturnMemoryOperation([][]byte{packet}, receiveRecoveryModeTcpSocket)
		if !ok {
			provider.finishMemoryOperation(&first)
			t.Fatal("second workspace reservation failed")
		}
		var releaseOnce sync.Once
		release := func() {
			releaseOnce.Do(func() {
				provider.finishMemoryOperation(&first)
				provider.finishMemoryOperation(&second)
			})
		}
		t.Cleanup(release)
		sourceId := NewId()
		sourceAdmitted := make(chan *providerSourceLifecycle, 1)
		provider.afterSourceAdmissionForTest = func(id Id) {
			if id == sourceId {
				provider.stateLock.Lock()
				lifecycle := provider.sourceLifecycles[id]
				provider.stateLock.Unlock()
				sourceAdmitted <- lifecycle
			}
		}
		flowCtx, cancelFlow := context.WithCancel(context.Background())
		defer cancelFlow()
		finished := make(chan bool, 1)
		go func() {
			memory, lifecycle, admitted := provider.startReturnMemoryOperationForSource(
				[][]byte{packet}, receiveRecoveryModeDedicatedTcpControl, sourceId, flowCtx,
			)
			if admitted {
				provider.releaseSourceLifecycle(sourceId, lifecycle)
				provider.finishMemoryOperation(&memory)
			}
			finished <- admitted
		}()
		lifecycle := <-sourceAdmitted
		synctest.Wait()
		provider.memoryOperations.stateLock.Lock()
		active := provider.memoryOperations.activeCount
		provider.memoryOperations.stateLock.Unlock()
		if active != 3 || provider.tcpReturnMemory.UsedByteCount() != kib(64) {
			t.Fatal("third operation did not hold the exact source before its full-slot wait")
		}
		select {
		case <-finished:
			t.Fatal("third operation did not wait behind both held workspaces")
		default:
		}
		closed := make(chan struct{})
		switch owner {
		case "flow":
			cancelFlow()
		case "source":
			provider.contractStatus(providerSourceReliabilityStatus(sourceId))
		case "provider":
			closing := make(chan struct{})
			provider.beforeReturnAdmissionsWaitForTest = func() { close(closing) }
			go func() {
				provider.Close()
				close(closed)
			}()
			<-closing
		default:
			t.Fatal("unknown cancellation owner")
		}
		var admitted bool
		if owner == "provider" {
			// Close's cancellation also starts the constructor's Close callback.
			// That second caller can wait on sync.Once's mutex while the first
			// joins our two workspaces. A mutex wait is not synctest-durable;
			// use the exact waiter completion instead of global quiescence here.
			admitted = <-finished
		} else {
			synctest.Wait()
			select {
			case admitted = <-finished:
			default:
				t.Fatal("owner cancellation did not end its pre-memory wait")
			}
		}
		if admitted {
			t.Error("canceled owner consumed a held workspace")
		}
		if provider.tcpReturnMemory.UsedByteCount() != kib(64) {
			t.Fatal("canceled wait released an independent workspace")
		}
		provider.memoryOperations.stateLock.Lock()
		active = provider.memoryOperations.activeCount
		provider.memoryOperations.stateLock.Unlock()
		if active != 2 {
			t.Errorf("canceled wait retained its memory gate: active=%d", active)
		}
		provider.stateLock.Lock()
		current := provider.sourceLifecycles[sourceId]
		provider.stateLock.Unlock()
		if owner == "source" {
			if current != lifecycle || !lifecycle.terminal || lifecycle.ctx.Err() == nil {
				t.Fatal("terminal verdict did not preserve its exact source tombstone")
			}
			select {
			case <-lifecycle.admissions.Done():
			default:
				t.Error("terminal source retained the canceled callback's admission")
			}
			if fresh := provider.acquireSourceLifecycle(sourceId); fresh != nil {
				provider.releaseSourceLifecycle(sourceId, fresh)
				t.Fatal("terminal source was recreated after pre-memory cancellation")
			}
		}
		if owner != "provider" && (provider.ctx.Err() != nil || fixture.client.Ctx().Err() != nil) {
			t.Fatal("local cancellation required a shared owner to close")
		}
		if owner != "flow" && flowCtx.Err() != nil {
			t.Fatal("source/provider cancellation canceled the independent flow parent")
		}
		if owner == "provider" {
			select {
			case <-closed:
				t.Error("provider close skipped the two independently held memory owners")
			default:
			}
		}
		release()
		if owner == "provider" {
			<-closed
		}
	})
	if stats := budget.Stats(); stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
		t.Fatalf("pre-memory cancellation left a budget owner: %+v", stats)
	}
}

// Flow cancellation alone ends the pre-memory wait with both slots unchanged.
func TestProviderReturnMemoryWaitObservesFlowCancellation(t *testing.T) {
	runProviderReturnMemoryOwnerCancellation(t, "flow")
}

// An inline terminal source verdict closes the same admitted generation.
func TestProviderReturnMemoryWaitObservesSourceRevocation(t *testing.T) {
	runProviderReturnMemoryOwnerCancellation(t, "source")
}

// Provider close cancels the waiter before joining its separately held owners.
func TestProviderReturnMemoryWaitObservesProviderClose(t *testing.T) {
	runProviderReturnMemoryOwnerCancellation(t, "provider")
}

// Already-canceled origins must not reserve even when both slots are free.
func TestProviderReturnMemoryCanceledFlowRefusesOpenWorkspace(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		fixture := newTcpProviderMemoryRetirementFixture(t, NewTransferMemoryBudget(mib(2)))
		packet := MessagePoolCopy(ipOosTcpPacket(fixture.path, tcpFlagAck, nil))
		defer MessagePoolReturn(packet)
		var sourceAdmissions atomic.Int32
		fixture.provider.afterSourceAdmissionForTest = func(Id) { sourceAdmissions.Add(1) }
		flowCtx, cancelFlow := context.WithCancel(context.Background())
		cancelFlow()
		memory, lifecycle, admitted := fixture.provider.startReturnMemoryOperationForSource(
			[][]byte{packet}, receiveRecoveryModeDedicatedTcpControl, NewId(), flowCtx,
		)
		if admitted {
			fixture.provider.releaseSourceLifecycle(lifecycle.sourceId, lifecycle)
			fixture.provider.finishMemoryOperation(&memory)
			t.Fatal("already-canceled flow reserved an open workspace")
		}
		if lifecycle != nil || memory.budget != nil || sourceAdmissions.Load() != 0 ||
			fixture.provider.tcpReturnMemory.UsedByteCount() != 0 {
			t.Fatal("already-canceled flow created a source or workspace owner")
		}
	})
}

// Revoke at the actual item handoff, after its early source lease and workspace
// exist. A failed child retain must release only its own item, not the outer
// callback lease twice, and must never reacquire a newer source generation.
func TestProviderReturnMemorySourceLeaseRejectsTerminalAtItemHandoff(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		fixture := newTcpProviderMemoryRetirementFixture(t, NewTransferMemoryBudget(mib(2)))
		provider := fixture.provider
		source := SourceId(NewId())
		flowCtx, cancelFlow := context.WithCancel(context.Background())
		defer cancelFlow()
		path := *fixture.path
		path.tcpReturnContext = flowCtx
		var admittedCount int
		var lifecycle *providerSourceLifecycle
		provider.afterSourceAdmissionForTest = func(id Id) {
			if id == source.SourceId {
				admittedCount++
				provider.stateLock.Lock()
				lifecycle = provider.sourceLifecycles[id]
				provider.stateLock.Unlock()
			}
		}
		var starts, completions int
		provider.settings.ReturnSendObserver = func(event RemoteUserNatProviderReturnSendObservation) {
			if event.Phase == RemoteUserNatProviderReturnSendPhaseStarted {
				starts++
				if lifecycle == nil || provider.tcpReturnMemory.UsedByteCount() != kib(32) {
					t.Error("item started without its existing source/workspace lease")
				}
				provider.contractStatus(providerSourceReliabilityStatus(source.SourceId))
			} else {
				completions++
				if event.Sent {
					t.Error("terminal source handed its return to Transfer")
				}
			}
		}
		returnPath := path.ReverseValue()
		packet := MessagePoolCopy(ipOosTcpPacket(&returnPath, tcpFlagAck, nil))
		provider.receiveTransferWithRecovery(source, TransferKey{}, tcpRetirementNetworkProvideMode,
			receiveRecoveryModeDedicatedTcpControl, &path, packet)
		if !MessagePoolReturn(packet) {
			t.Error("failed exact-source item handoff retained the borrowed packet")
		}
		synctest.Wait()
		if starts != 1 || completions != 1 || admittedCount != 1 || lifecycle == nil {
			t.Fatalf("source handoff events: starts=%d completions=%d acquisitions=%d", starts, completions, admittedCount)
		}
		provider.stateLock.Lock()
		current := provider.sourceLifecycles[source.SourceId]
		provider.stateLock.Unlock()
		if current != lifecycle || !lifecycle.terminal || lifecycle.ctx.Err() == nil ||
			provider.tcpReturnMemory.UsedByteCount() != 0 {
			t.Fatal("terminal handoff replaced its source or retained the workspace")
		}
		select {
		case <-lifecycle.admissions.Done():
		default:
			t.Fatal("terminal handoff retained or double-counted its outer source lease")
		}
		if provider.ctx.Err() != nil || fixture.client.Ctx().Err() != nil || flowCtx.Err() != nil {
			t.Fatal("terminal handoff closed an unrelated parent")
		}
	})
}
