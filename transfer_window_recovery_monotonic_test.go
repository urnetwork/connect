// Recovery deadlines retain the client's elapsed-time clock at a physical write.
package connect

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The sender stores physical time as stabilized nanoseconds for feedback.
// Reconstructing a recovery deadline must retain its original monotonic clock,
// so wall-clock adjustments cannot shorten or lengthen the elapsed interval.
func TestWindowPacingPhysicalRecoveryKeepsMonotonicClock(t *testing.T) {
	assertMessagePoolOwnership(t)
	ctx, cancel := context.WithCancel(context.Background())
	destination := NewId()
	written := make(chan struct{})
	settings := DefaultClientSettings()
	settings.Log = NewNoopLogger()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
	settings.SendBufferSettings.WindowSizing = WindowSizingFromDelivery
	settings.SendBufferSettings.ApplyWindowSizing()
	settings.SendBufferSettings.MinResendInterval = 300 * time.Millisecond
	settings.SendBufferSettings.RttMinResendInterval = 300 * time.Millisecond
	settings.SendBufferSettings.afterInitialWriteQueuedForTest = func(id sendSequenceId, number uint64) {
		if id.Destination == destination && number == 0 {
			close(written)
			<-ctx.Done()
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
	frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(1280)}
	clear(frame.MessageBytes)
	if !client.SendWithTimeout(frame, destination, func(error) {}, time.Second) {
		MessagePoolReturn(frame.MessageBytes)
		t.Fatal("Pack not admitted")
	}
	<-written
	sequence := client.sendBuffer.lookupSendSequence(sendSequenceId{Destination: destination}, nil)
	item := sequence.resendQueue.PeekFirst()
	base := client.feedbackTimeBase
	if base == base.Round(0) {
		t.Fatal("fixture needs the real monotonic client clock")
	}
	physical := base.Add(time.Duration(item.pacingSentAtNanos - base.UnixNano()))
	want := physical.Add(300 * time.Millisecond)
	if !item.resendTime.Equal(want) {
		t.Fatalf("deadline changed its elapsed interval: got %s want %s", item.resendTime, want)
	}
	if item.resendTime != want {
		t.Fatal("physical recovery deadline discarded the client's monotonic clock")
	}
	if item.resendTime.Sub(physical) != 300*time.Millisecond {
		t.Fatal("reconstructed deadline changed the configured recovery interval")
	}
}
