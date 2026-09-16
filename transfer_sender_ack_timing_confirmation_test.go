// Synchronous replies cannot certify the physical write before it returns.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A real wire observer pauses immediately before the physical route call.
// The same exact reply is eligible only after that call succeeds.
func TestSenderReceiverTimingWaitsForPhysicalConfirmation(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) { senderReceiverTimingConfirmationTest(t, false, false) })
}

// The rejected physical write still leaves an ACK in the byte coalescer.
// Applying it must not resurrect the rejected timing through legacy tags.
func TestSenderReceiverTimingRejectsFailedPhysicalWrite(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) { senderReceiverTimingConfirmationTest(t, true, false) })
}

// Canceling the pending write discards its timing observation even though
// the exact wire-format reply already reached the ACK coalescer.
func TestSenderReceiverTimingCancelDropsUnconfirmedReply(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) { senderReceiverTimingConfirmationTest(t, false, true) })
}

// Explicit barriers force ACK-before-writer-return without a scheduler race.
func senderReceiverTimingConfirmationTest(t *testing.T, failWrite, cancelWrite bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	destination := NewId()
	observed := make(chan *protocol.Pack, 1)
	releaseWrite, written, releaseWorker := make(chan struct{}), make(chan struct{}), make(chan struct{})
	settings := DefaultClientSettings()
	settings.Log = NewNoopLogger()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
	settings.SendBufferSettings.WriteTimeout = 0
	settings.SendBufferSettings.TransferWireMessageObserver = func(observation TransferWireMessageObservation) {
		if observation.Resend {
			return
		}
		observed <- decodeSendPackLifecycleWirePack(t, observation.TransferFrameBytes)
		select {
		case <-releaseWrite:
		case <-ctx.Done():
		}
	}
	settings.SendBufferSettings.afterInitialWriteQueuedForTest = func(sendSequenceId, uint64) {
		close(written)
		select {
		case <-releaseWorker:
		case <-ctx.Done():
		}
	}
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	capacity := 4
	if failWrite || cancelWrite {
		capacity = 0
	}
	route := make(Route, capacity)
	defer func() {
		cancel()
		client.CloseAndWait(context.Background())
		for len(route) > 0 {
			MessagePoolReturn(<-route)
		}
	}()
	client.ContractManager().AddNoContractPeer(destination)
	client.RouteManager().UpdateTransport(&h1SendClientTransportForGroupTest{sendClientTransport: NewSendClientTransport(DestinationId(destination))}, []Route{route})
	frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(1000)}
	if !client.SendWithTimeout(frame, destination, func(error) {}, time.Second) {
		MessagePoolReturn(frame.MessageBytes)
		t.Fatal("initial Pack not admitted")
	}
	pack := <-observed
	sequence := client.sendBuffer.lookupSendSequence(sendSequenceId{Destination: destination}, nil)
	if sequence == nil {
		t.Fatal("live sequence missing")
	}
	time.Sleep(12 * time.Millisecond)
	delay := uint32(5000)
	if ok, err := sequence.Ack(&protocol.Ack{MessageId: pack.MessageId, SequenceId: pack.SequenceId, Tag: pack.Tag, ReceiverAckDelayMicros: &delay}, 0); !ok || err != nil {
		t.Fatalf("reply not admitted: %t %v", ok, err)
	}
	synctest.Wait()
	if got := sequence.rttWindow.Estimate(); got.SampleCount != 0 {
		t.Fatalf("unconfirmed physical write created RTT: %+v", got)
	}
	if cancelWrite {
		cancel()
	} else {
		close(releaseWrite)
	}
	<-written
	synctest.Wait()
	want := 1
	if failWrite || cancelWrite {
		want = 0
	}
	if got := sequence.rttWindow.Estimate(); got.SampleCount != want {
		t.Fatalf("physical confirmation sample count=%d want%d", got.SampleCount, want)
	}
	close(releaseWorker)
	synctest.Wait()
	got := sequence.rttWindow.Estimate()
	if got.SampleCount != want || want == 1 && got.Mean != 12*time.Millisecond {
		t.Fatalf("byte application resurrected rejected timing or duplicated confirmed timing: %+v wantCount%d", got, want)
	}
}
