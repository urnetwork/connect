// Local pacing must precede the start of an ordinary physical recovery timer.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A valid serialization reservation holds the initial write for600ms. The
// worker's unchanged300ms RTO must start when that write actually begins,
// with a real missing reply still recovered at the same bounded interval.
func TestWindowPacingOrdinaryRecoveryStartsAtPhysicalWrite(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		destination := NewId()
		startWorker, written, releaseWorker := make(chan struct{}), make(chan struct{}), make(chan struct{})
		settings := DefaultClientSettings()
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		settings.SendBufferSettings.WindowSizing = WindowSizingFromDelivery
		settings.SendBufferSettings.ApplyWindowSizing()
		settings.SendBufferSettings.MinResendInterval = 300 * time.Millisecond
		settings.SendBufferSettings.RttMinResendInterval = 300 * time.Millisecond
		settings.SendBufferSettings.MaxResendInterval = 4 * time.Second
		settings.SendBufferSettings.AckTimeout = time.Minute
		settings.SendBufferSettings.beforeRunSendSequenceForTest = func(sendSequenceId) {
			select {
			case <-startWorker:
			case <-ctx.Done():
			}
		}
		settings.SendBufferSettings.afterInitialWriteQueuedForTest = func(id sendSequenceId, number uint64) {
			if id.Destination != destination || number != 0 {
				return
			}
			close(written)
			select {
			case <-releaseWorker:
			case <-ctx.Done():
			}
		}
		client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		route := make(Route, 8)
		defer func() {
			cancel()
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Errorf("client cleanup: %v", err)
			}
			for len(route) > 0 {
				MessagePoolReturn(<-route)
			}
		}()
		client.ContractManager().AddNoContractPeer(destination)
		client.RouteManager().UpdateTransport(&h1SendClientTransportForGroupTest{sendClientTransport: NewSendClientTransport(DestinationId(destination))}, []Route{route})
		sequence := client.sendBuffer.createSendSequence(sendSequenceId{Destination: destination}, &SendPack{Destination: destination})
		if sequence == nil || sequence.windowPacer.service == nil {
			t.Fatal("no shared pacing service")
		}
		start := time.Now()
		service := sequence.windowPacer.service
		service.stateLock.Lock()
		service.next = start.Add(600 * time.Millisecond)
		service.stateLock.Unlock()
		frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(1280)}
		if !client.SendWithTimeout(frame, destination, func(error) {}, time.Second) {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatal("Pack not admitted")
		}
		close(startWorker)
		<-written
		physicalAt := time.Now()
		if physicalAt != start.Add(600*time.Millisecond) {
			t.Fatalf("physical release=%s want600ms", physicalAt.Sub(start))
		}
		select {
		case bytes := <-route:
			MessagePoolReturn(bytes)
		default:
			t.Fatal("physical write missing")
		}
		item := sequence.resendQueue.PeekFirst()
		if item == nil || item.pacingSentAtNanos != physicalAt.UnixNano() {
			t.Fatal("actual physical timing was not published")
		}
		deadline := item.resendTime
		t.Logf("offer=%s physical=%s scheduled-retry=%s configured-rto=300ms", start.Sub(start), physicalAt.Sub(start), deadline.Sub(start))
		if deadline != physicalAt.Add(300*time.Millisecond) {
			t.Error("local pacing consumed the initial recovery timer before its physical write")
		}
		close(releaseWorker)
		synctest.Wait()
		time.Sleep(10 * time.Millisecond)
		synctest.Wait()
		early := len(route)
		for len(route) > 0 {
			MessagePoolReturn(<-route)
		}
		if early != 0 {
			t.Errorf("ordinary message physically retried%d times within10ms of its first physical write", early)
		}
		if early == 0 {
			time.Sleep(time.Until(physicalAt.Add(300 * time.Millisecond)))
			synctest.Wait()
			if len(route) != 1 {
				t.Errorf("a missing reply was not recovered at the unchanged physical300ms RTO: writes=%d", len(route))
			}
		}
	})
}
