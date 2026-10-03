// Physical delivery must reach the service estimator while its sender worker
// waits. Retiring reliable items and invoking callbacks remain worker-owned.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Three real writes are acknowledged while their owner is durably blocked.
func TestWindowPacingServiceAckArrivalPrecedesPausedWorker(t *testing.T) {
	testWindowPacingServiceAckArrival(t, false, false)
}

// A SACK, its duplicate, and a covering head credit each wire envelope once.
func TestWindowPacingServiceSelectiveArrivalPrecedesPausedWorker(t *testing.T) {
	testWindowPacingServiceAckArrival(t, true, false)
}

// Cancellation after publication cannot subtract already delivered ownership.
func TestWindowPacingServiceAckArrivalThenCancelBalancesOwnership(t *testing.T) {
	testWindowPacingServiceAckArrival(t, true, true)
}

// Explicit initial-write and coalescer barriers separate receipt from worker
// application, without making a scheduler delay the failure condition.
func testWindowPacingServiceAckArrival(t *testing.T, selective, cancelBeforeApply bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		destination := NewId()
		workerPaused, releaseWorker := make(chan struct{}), make(chan struct{})
		coalesced := make(chan uint64, 8)
		settings := DefaultClientSettings()
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		settings.SendBufferSettings.WindowSizing = WindowSizingFromDelivery
		settings.SendBufferSettings.ApplyWindowSizing()
		settings.SendBufferSettings.afterInitialWriteQueuedForTest = func(id sendSequenceId, number uint64) {
			if id.Destination != destination || number != 2 {
				return
			}
			close(workerPaused)
			select {
			case <-releaseWorker:
			case <-ctx.Done():
			}
		}
		settings.SendBufferSettings.afterAckCoalescedForTest = func(id sendSequenceId, number uint64) {
			if id.Destination == destination {
				coalesced <- number
			}
		}
		client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		route := make(Route, 8)
		defer func() {
			cancel()
			client.CloseAndWait(context.Background())
			for {
				select {
				case bytes := <-route:
					MessagePoolReturn(bytes)
				default:
					return
				}
			}
		}()
		client.ContractManager().AddNoContractPeer(destination)
		client.RouteManager().UpdateTransport(&h1SendClientTransportForGroupTest{sendClientTransport: NewSendClientTransport(DestinationId(destination))}, []Route{route})
		var packs [3]*protocol.Pack
		var sizes [3]ByteCount
		callbacks := make(chan error, 3)
		for i := range packs {
			frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(1000 + i)}
			if !client.SendWithTimeout(frame, destination, func(err error) { callbacks <- err }, time.Second) {
				MessagePoolReturn(frame.MessageBytes)
				t.Fatal("initial Pack refused")
			}
			bytes := <-route
			packs[i], sizes[i] = decodeSendPackLifecycleWirePack(t, bytes), ByteCount(len(bytes))
			MessagePoolReturn(bytes)
		}
		<-workerPaused
		synctest.Wait()
		sequence := client.sendBuffer.lookupSendSequence(sendSequenceId{Destination: destination}, nil)
		if sequence == nil || sequence.windowPacer.service == nil {
			t.Fatal("real sender did not create its H1 service")
		}
		service := sequence.windowPacer.service
		steps := []struct {
			index     int
			selective bool
			want      ByteCount
		}{
			{index: 0, want: sizes[0]},
			{index: 1, want: sizes[0] + sizes[1]},
			{index: 2, want: sizes[0] + sizes[1] + sizes[2]},
		}
		if selective {
			steps = []struct {
				index     int
				selective bool
				want      ByteCount
			}{
				{index: 2, selective: true, want: sizes[2]},
				{index: 2, selective: true, want: sizes[2]},
				{index: 1, want: sizes[0] + sizes[1] + sizes[2]},
				{index: 2, want: sizes[0] + sizes[1] + sizes[2]},
			}
		}
		for i, step := range steps {
			time.Sleep(10 * time.Millisecond)
			pack := packs[step.index]
			if ok, err := sequence.Ack(&protocol.Ack{MessageId: pack.MessageId, SequenceId: pack.SequenceId, Tag: pack.Tag, Selective: step.selective}, 0); !ok || err != nil {
				t.Fatalf("ACK refused: %t %v", ok, err)
			}
			<-coalesced
			synctest.Wait()
			service.stateLock.Lock()
			total := service.total
			service.stateLock.Unlock()
			if total != step.want {
				t.Errorf("step %d: paused worker hid delivered wire bytes: got %d want %d", i, total, step.want)
			}
			select {
			case err := <-callbacks:
				t.Fatalf("coalescer retired worker-owned delivery: %v", err)
			default:
			}
		}
		if cancelBeforeApply {
			sequence.Cancel()
			cancel()
		} else {
			close(releaseWorker)
		}
		synctest.Wait()
		for range packs {
			<-callbacks
		}
		service.stateLock.Lock()
		total, sent := service.total, service.sent
		service.stateLock.Unlock()
		want := sizes[0] + sizes[1] + sizes[2]
		if total != want || sent < total {
			t.Errorf("worker completion duplicated credit or released acknowledged ownership: sent=%d total=%d want=%d", sent, total, want)
		}
	})
}
