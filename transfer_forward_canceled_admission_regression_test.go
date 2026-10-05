package connect

import (
	"bytes"
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Use the resident's actual channel capacity and public native-frame entry.
// Park only construction-independent publisher work and the forward consumer,
// so accepted frames remain observable until their owner is joined.
func newCanceledForwardAdmissionClient(t *testing.T) (*Client, *TransferMemoryOwnerLedger) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	settings := DefaultClientSettingsWithBufferSize(4096)
	settings.Log = NewNoopLogger()
	settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
	settings.ForwardBufferSettings.beforeRunForwardSequenceForTest = func(TransferPath) { <-ctx.Done() }
	ledger := &TransferMemoryOwnerLedger{}
	settings.MemoryOwnerLedger = ledger
	client := NewClient(ctx, ControlId, NewNoContractClientOob(), settings)
	t.Cleanup(func() {
		cancel()
		if err := client.CloseAndWait(context.Background()); err != nil {
			t.Errorf("forward admission client close: %v", err)
		}
	})
	return client, ledger
}

func canceledForwardNativeFrame(t *testing.T, path TransferPath) []byte {
	t.Helper()
	wire, err := ProtoMarshal(&protocol.TransferFrame{
		TransferPath: path.ToProtobuf(),
		Ack:          &protocol.Ack{MessageId: NewId().Bytes(), SequenceId: NewId().Bytes()},
	})
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

func requireForwardAdmissionOwners(t *testing.T, client *Client, ledger *TransferMemoryOwnerLedger, want int64) {
	t.Helper()
	snapshot := ledger.Snapshot()
	if !snapshot.Complete || snapshot.Forward.Workers != want || snapshot.Forward.AdmittedTotal != uint64(want) ||
		snapshot.Forward.KnownChannelSlotBytes != want*4096*8 || snapshot.Forward.CleanupWorkers != 0 {
		t.Errorf("unexpected forward ownership: %+v", snapshot)
	}
	buffer := client.forwardBuffer
	buffer.mutex.Lock()
	defer buffer.mutex.Unlock()
	if len(buffer.forwardSequences) != int(want) || len(buffer.activeForwardSequences) != int(want) {
		t.Errorf("forward lookup/active owners = %d/%d, want %d", len(buffer.forwardSequences), len(buffer.activeForwardSequences), want)
	}
}

// Rejection leaves the exact native buffer owned by its caller. Zero admitted
// work must not create a worker, channel, route owner or transient admission.
func TestForwardBufferPreCanceledNativeCallerDoesNotCreateSequence(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, timeout := range []time.Duration{0, time.Second, -1} {
		t.Run(timeout.String(), func(t *testing.T) {
			client, ledger := newCanceledForwardAdmissionClient(t)
			ctx, cancel := context.WithCancel(client.ctx)
			cancel()
			wire := canceledForwardNativeFrame(t, NewTransferPath(NewId(), NewId(), Id{}))
			original := bytes.Clone(wire)
			defer MessagePoolReturn(wire)
			admitted, err := client.ForwardWithTimeoutDetailed(wire, timeout, Ctx(ctx))
			if admitted || err == nil || err.Error() != "Done." {
				t.Fatalf("pre-canceled native forward result: admitted=%t err=%v", admitted, err)
			}
			if !bytes.Equal(original, wire) {
				t.Fatal("rejected caller's native buffer changed")
			}
			requireForwardAdmissionOwners(t, client, ledger, 0)
		})
	}
}

// A real stack witness binds cancellation to the caller waiting inside Pack's
// creation mutex. A scheduling delay or test hook alone would not prove this.
func waitForwardPackCreationLock(t *testing.T) {
	t.Helper()
	stackBytes := make([]byte, 1<<20)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n := runtime.Stack(stackBytes, true)
		for _, stack := range strings.Split(string(stackBytes[:n]), "\n\n") {
			if strings.Contains(stack, "(*ForwardBuffer).Pack.func1(") &&
				strings.Contains(stack, "sync.(*Mutex).Lock(") &&
				strings.Contains(stack, "TestForwardBufferNativeCallerCancelWhileWaitingForCreationLock.func") {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("real native forward caller did not wait on the creation mutex")
}

func TestForwardBufferNativeCallerCancelWhileWaitingForCreationLock(t *testing.T) {
	assertMessagePoolOwnership(t)
	client, ledger := newCanceledForwardAdmissionClient(t)
	destination := NewId()
	var siblings [][]byte
	for range 2 {
		wire := canceledForwardNativeFrame(t, NewTransferPath(NewId(), destination, Id{}))
		siblings = append(siblings, bytes.Clone(wire))
		if !client.ForwardWithTimeout(wire, 0) {
			MessagePoolReturn(wire)
			t.Fatal("native sibling was not admitted")
		}
	}
	buffer := client.forwardBuffer
	buffer.mutex.Lock()
	sequence := buffer.forwardSequences[DestinationId(destination)]
	buffer.mutex.Unlock()
	callerCtx, cancelCaller := context.WithCancel(client.ctx)
	defer cancelCaller()
	wire := canceledForwardNativeFrame(t, NewTransferPath(NewId(), NewId(), Id{}))
	defer MessagePoolReturn(wire)
	type result struct {
		admitted bool
		err      error
	}
	done := make(chan result, 1)
	func() {
		buffer.mutex.Lock()
		defer buffer.mutex.Unlock()
		go func() {
			admitted, err := client.ForwardWithTimeoutDetailed(wire, -1, Ctx(callerCtx))
			done <- result{admitted, err}
		}()
		waitForwardPackCreationLock(t)
		cancelCaller()
	}()
	got := <-done
	if got.admitted || got.err == nil || got.err.Error() != "Done." {
		t.Fatalf("lock-wait canceled native result: admitted=%t err=%v", got.admitted, got.err)
	}
	requireForwardAdmissionOwners(t, client, ledger, 1)
	buffer.mutex.Lock()
	same := buffer.forwardSequences[DestinationId(destination)] == sequence
	buffer.mutex.Unlock()
	if !same || sequence.ctx.Err() != nil || len(sequence.packs) != 2 {
		t.Fatal("lock-wait cancellation changed the accepted sibling owner")
	}
	for _, expected := range siblings {
		pack := <-sequence.packs
		matches := bytes.Equal(expected, pack.TransferFrameBytes)
		MessagePoolReturn(pack.TransferFrameBytes)
		if !matches {
			t.Error("accepted native sibling bytes changed")
		}
	}
}
