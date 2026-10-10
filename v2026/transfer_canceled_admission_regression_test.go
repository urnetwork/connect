package connect

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// A caller already canceled before entry owns no accepted work and must not
// materialize a destination worker or its fixed queues.
func TestSendBufferPreCanceledCallerDoesNotCreateSequence(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, timeout := range []time.Duration{0, time.Second, -1} {
		t.Run(timeout.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fixture := newSendPackCallerOwnerFixture(t)
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				_, admitted, err := fixture.send(ctx, "never-admitted", timeout)
				if admitted || err == nil || err.Error() != "Done." {
					t.Fatalf("pre-canceled caller result: admitted=%t err=%v", admitted, err)
				}
				if fixture.currentSequence() != nil {
					t.Fatal("pre-canceled caller created a destination sequence")
				}
			})
		})
	}
}

// Mutex waits are not synctest durable blocking points. Inspect the real Pack
// stack so cancellation happens only after this caller is waiting on the map
// mutex; a sleep alone would also allow cancellation before Pack entry.
func waitSendPackMapLock(t *testing.T) {
	t.Helper()
	stackBytes := make([]byte, 1<<20)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n := runtime.Stack(stackBytes, true)
		for _, stack := range strings.Split(string(stackBytes[:n]), "\n\n") {
			if strings.Contains(stack, "(*SendBuffer).lookupSendSequence(") &&
				strings.Contains(stack, "sync.(*Mutex).Lock(") &&
				strings.Contains(stack, "TestSendBufferCallerCancelWhileWaitingForCreationLock.func") {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("real Pack did not block on the sender map mutex")
}

func TestSendBufferCallerCancelWhileWaitingForCreationLock(t *testing.T) {
	assertMessagePoolOwnership(t)
	clientCtx, closeClient := context.WithCancel(context.Background())
	settings := closeWaitClientSettings()
	settings.SendBufferSettings.SequenceBufferSize = 2
	settings.beforeClientKeyPublishForTest = func() { <-clientCtx.Done() }
	settings.SendBufferSettings.beforeRunSendSequenceForTest = func(sendSequenceId) { <-clientCtx.Done() }
	ledger := &TransferMemoryOwnerLedger{}
	settings.MemoryOwnerLedger = ledger
	client := NewClient(clientCtx, NewId(), NewNoContractClientOob(), settings)
	t.Cleanup(func() {
		closeClient()
		if err := client.CloseAndWait(context.Background()); err != nil {
			t.Errorf("send cancellation fixture close: %v", err)
		}
	})
	accepted := &sendPackCallerOwnerFixture{client: client,
		id: sendSequenceId{Destination: NewId(), EncryptionRole: sequenceTlsRoleClient}}
	accepted.fill(t)
	refused := &sendPackCallerOwnerFixture{client: client,
		id: sendSequenceId{Destination: NewId(), EncryptionRole: sequenceTlsRoleClient}}
	callerCtx, cancelCaller := context.WithCancel(context.Background())
	defer cancelCaller()
	type result struct {
		admitted bool
		err      error
	}
	done := make(chan result, 1)
	buffer := client.sendBuffer
	func() {
		buffer.mutex.Lock()
		defer buffer.mutex.Unlock()
		go func() {
			_, admitted, err := refused.send(callerCtx, "canceled-at-map-lock", -1)
			done <- result{admitted, err}
		}()
		waitSendPackMapLock(t)
		cancelCaller()
	}()
	got := <-done
	if got.admitted || got.err == nil || got.err.Error() != "Done." {
		t.Fatalf("lock-wait canceled caller result: admitted=%t err=%v", got.admitted, got.err)
	}
	if refused.currentSequence() != nil {
		t.Error("lock-wait canceled caller created a destination sequence")
	}
	// The only allowed worker, channel slots and cache entries belong to the
	// earlier accepted siblings. Include admitted_total to catch transient owners.
	snapshot := ledger.Snapshot()
	if !snapshot.Complete || snapshot.Send.Workers != 1 || snapshot.Send.AdmittedTotal != 1 ||
		snapshot.Send.KnownChannelSlotBytes != 2*8 || snapshot.Send.CleanupWorkers != 0 {
		t.Errorf("canceled caller acquired sequence ownership: %+v", snapshot)
	}
	buffer.mutex.Lock()
	if len(buffer.sendSequences) != 1 || len(buffer.wireSendSequences) != 1 ||
		len(buffer.sendSequencesBySequenceId) != 1 || len(buffer.activeSendSequences) != 1 ||
		len(buffer.windowPacingServices) > 1 {
		t.Error("canceled caller populated an owner or pacing cache")
	}
	buffer.mutex.Unlock()
	accepted.requireSiblings(t)
}
