// Exact receiver metadata must reach RTT measurement before the send worker resumes.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A real worker waits locally before the physical write, then stays behind
// its post-write barrier while another worker coalesces the reply. Raw RTT
// must include neither local wait and must remain separate from ACK delay.
func TestSenderReceiverTimingArrivesBeforeBlockedWorker(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		destination := NewId()
		startWorker, localWait, releaseLocal := make(chan struct{}), make(chan struct{}), make(chan struct{})
		written, releaseWorker := make(chan struct{}), make(chan struct{})
		settings := DefaultClientSettings()
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		settings.SendBufferSettings.WindowSizing = WindowSizingFromDelivery
		settings.SendBufferSettings.ApplyWindowSizing()
		settings.SendBufferSettings.beforeRunSendSequenceForTest = func(sendSequenceId) {
			select {
			case <-startWorker:
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
		route := make(Route, 8)
		defer func() {
			cancel()
			client.CloseAndWait(context.Background())
			for len(route) > 0 {
				MessagePoolReturn(<-route)
			}
		}()
		client.ContractManager().AddNoContractPeer(destination)
		client.RouteManager().UpdateTransport(&h1SendClientTransportForGroupTest{sendClientTransport: NewSendClientTransport(DestinationId(destination))}, []Route{route})
		sequence := client.sendBuffer.createSendSequence(sendSequenceId{Destination: destination}, &SendPack{Destination: destination})
		synctest.Wait()
		sequence.windowPacer.afterAdmissionForTest = func() {
			close(localWait)
			select {
			case <-releaseLocal:
			case <-ctx.Done():
			}
		}
		frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(1000)}
		if !client.SendWithTimeout(frame, destination, func(error) {}, time.Second) {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatal("initial Pack not admitted")
		}
		close(startWorker)
		<-localWait
		time.Sleep(40 * time.Millisecond)
		close(releaseLocal)
		<-written
		wire := <-route
		pack := decodeSendPackLifecycleWirePack(t, wire)
		MessagePoolReturn(wire)
		time.Sleep(12 * time.Millisecond)
		delay, compression, capacity := uint32(5000), uint32(0), uint64(2*1024*1024)
		ok, err := sequence.Ack(&protocol.Ack{MessageId: pack.MessageId, SequenceId: pack.SequenceId, Tag: pack.Tag, ReceiverAckDelayMicros: &delay, AckCompressTimeoutMicros: &compression, ReceiveWindowByteCount: &capacity}, 0)
		if !ok || err != nil {
			t.Fatalf("exact reply not accepted: %t %v", ok, err)
		}
		synctest.Wait()
		estimate := sequence.rttWindow.Estimate()
		if estimate.SampleCount != 1 || estimate.Mean != 12*time.Millisecond || estimate.Min != 12*time.Millisecond {
			t.Fatalf("coalesced exact RTT remained worker-dependent or included pacing: %+v, want one raw 12ms sample", estimate)
		}
		// Delay application again after ingress. Returning to the worker may
		// credit bytes but must not add another, longer raw sample.
		time.Sleep(15 * time.Millisecond)
		close(releaseWorker)
		synctest.Wait()
		estimate = sequence.rttWindow.Estimate()
		if estimate.SampleCount != 1 || estimate.Mean != 12*time.Millisecond {
			t.Fatalf("worker reapplied or retimed physical RTT: %+v", estimate)
		}
	})
}
