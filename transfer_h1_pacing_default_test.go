// Default H1 writes rely on TCP backpressure, not a second pacing deadline.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Force a five-second service reservation before releasing the actual send
// worker. Ordinary default H1 traffic must still reach the physical writer
// immediately, with retry ownership and hard admission bounds intact.
func TestH1DefaultWriteDoesNotWaitForApplicationPacing(t *testing.T) {
	testH1ApplicationPacingWrite(t, nil, 0)
}

// Experiments can still exercise the controller without changing app defaults.
func TestH1ApplicationPacingExplicitOptIn(t *testing.T) {
	testH1ApplicationPacingWrite(t, func(settings *SendBufferSettings) {
		settings.ApplicationPacing = true
	}, 5*time.Second)
}

func testH1ApplicationPacingWrite(t *testing.T, configure func(*SendBufferSettings), expectedDelay time.Duration) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		destination := NewId()
		startWorker, written, releaseWorker := make(chan struct{}), make(chan struct{}), make(chan struct{})
		settings := DefaultClientSettings()
		if configure != nil {
			configure(settings.SendBufferSettings)
		}
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		settings.SendBufferSettings.ResendQueueBudget = NewTransferMemoryBudget(2 * 1024 * 1024)
		settings.SendBufferSettings.ResendQueueRetainedByteAccounting = true
		settings.SendBufferSettings.beforeRunSendSequenceForTest = func(sendSequenceId) {
			select {
			case <-startWorker:
			case <-ctx.Done():
			}
		}
		settings.SendBufferSettings.afterInitialWriteQueuedForTest = func(id sendSequenceId, number uint64) {
			if id.Destination == destination && number == 0 {
				close(written)
				select {
				case <-releaseWorker:
				case <-ctx.Done():
				}
			}
		}
		client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		route := make(Route, 8)
		defer func() {
			cancel()
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
			for len(route) > 0 {
				MessagePoolReturn(<-route)
			}
		}()
		client.ContractManager().AddNoContractPeer(destination)
		client.RouteManager().UpdateTransport(&h1SendClientTransportForGroupTest{sendClientTransport: NewSendClientTransport(DestinationId(destination))}, []Route{route})
		sequence := client.sendBuffer.createSendSequence(sendSequenceId{Destination: destination}, &SendPack{Destination: destination, Ctx: ctx})
		if sequence == nil || sequence.windowPacer.service == nil {
			t.Fatal("missing delivery-sizing service")
		}
		start := time.Now()
		sequence.windowPacer.service.stateLock.Lock()
		sequence.windowPacer.service.next = start.Add(5 * time.Second)
		sequence.windowPacer.service.stateLock.Unlock()
		frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(1280)}
		if !client.SendWithTimeout(frame, destination, func(error) {}, 10*time.Second) {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatal("bounded Pack admission failed")
		}
		close(startWorker)
		<-written
		if elapsed := time.Since(start); elapsed != expectedDelay {
			t.Errorf("H1_APPLICATION_PACING_STALL: physical write delayed %s, want %s", elapsed, expectedDelay)
		}
		if !sequence.transferFlightPolicy().h1Only || !settings.SendBufferSettings.WindowSizingActive() ||
			!settings.ReceiveBufferSettings.AdvertiseReceiveWindow || sequence.resendQueue.PeekFirst() == nil {
			t.Error("unpaced default lost route policy, sizing, advertisement or retry custody")
		}
		close(releaseWorker)
	})
}

// Window growth remains independent of the optional packet scheduler. A full
// opening delivered each RTT grows the window, but not past peer or byte caps.
func TestWindowResizingGrowsWithoutApplicationPacing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const opening = ByteCount(1024 * 1024)
		const roundTrip = 50 * time.Millisecond
		const permission = ByteCount(3 * 1024 * 1024)
		sequence := newEstimatorFixture(t, func(settings *SendBufferSettings) {
			settings.ApplicationPacing = false
			settings.DeliverySizedWindowScale = deliverySizedWindowScale
			settings.ResendQueueBudget = NewTransferMemoryBudget(permission)
			settings.ResendQueueMaxByteCount = opening
			settings.ResendQueueMinByteCount = 64 * 1024
		})
		defer sequence.resendQueue.Clear()
		sampleRoundTrip(sequence, roundTrip)
		sequence.observeReceiveWindowAdvertisement(receiveAckMessage{
			ackCompressTimeoutSet: true, receiveWindowSet: true,
			receiveWindowByteCount: uint32(permission),
		})
		start := time.Now()
		for index := range 40 {
			sequence.observeDeliveredBytes(opening/5, start.Add(time.Duration(index)*10*time.Millisecond))
		}
		estimate := sequence.sendWindowEstimate(start.Add(400 * time.Millisecond))
		if sequence.sendBufferSettings.ApplicationPacing || !estimate.Sized || estimate.Window <= opening || estimate.Window > permission {
			t.Fatalf("resizing without pacing lost growth or bounds: %+v", estimate)
		}
	})
}

func TestApplicationPacingDefaultOffKeepsWindowPolicy(t *testing.T) {
	send := DefaultSendBufferSettings()
	receive := DefaultReceiveBufferSettings()
	send.ResendQueueBudget = NewTransferMemoryBudget(2 * 1024 * 1024)
	if send.ApplicationPacing || !send.WindowSizingActive() || !receive.AdvertiseReceiveWindow {
		t.Fatal("default pacing policy disabled delivery sizing or receive permission")
	}
	if (SendBufferSettings{}).ApplicationPacing {
		t.Fatal("zero-value settings enabled application pacing")
	}
}

// Fresh delivery can fill either limiting permission, but cannot turn removal
// of scheduling waits into permission to exceed the peer or owner's byte cap.
func TestWindowResizingWithoutPacingHonorsPeerAndBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const opening = ByteCount(1024 * 1024)
		const limit = ByteCount(1536 * 1024)
		for _, values := range []struct {
			name       string
			budget     ByteCount
			permission ByteCount
		}{
			{name: "peer", budget: 8 * opening, permission: limit},
			{name: "budget", budget: limit, permission: 8 * opening},
		} {
			sequence := newEstimatorFixture(t, func(settings *SendBufferSettings) {
				settings.ApplicationPacing = false
				settings.DeliverySizedWindowScale = deliverySizedWindowScale
				settings.ResendQueueBudget = NewTransferMemoryBudget(values.budget)
				settings.ResendQueueMaxByteCount = opening
				settings.ResendQueueMinByteCount = 64 * 1024
			})
			t.Cleanup(func() { sequence.resendQueue.Clear() })
			sampleRoundTrip(sequence, 50*time.Millisecond)
			sequence.observeReceiveWindowAdvertisement(receiveAckMessage{
				ackCompressTimeoutSet: true, receiveWindowSet: true,
				receiveWindowByteCount: uint32(values.permission),
			})
			start := time.Now()
			for index := range 40 {
				sequence.observeDeliveredBytes(4*opening/5, start.Add(time.Duration(index)*10*time.Millisecond))
			}
			estimate := sequence.sendWindowEstimate(start.Add(400 * time.Millisecond))
			if !estimate.Sized || estimate.Window != limit || estimate.Ceiling != limit {
				t.Errorf("%s bound changed without pacing: %+v", values.name, estimate)
			}
		}
	})
}
