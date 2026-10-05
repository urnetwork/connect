package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// An opening admission policy must leave the owning sequence free to run
// recovery and notice route replacement. The first hop accepts writes here;
// deliberately missing ACKs provide no credit oracle to the sender.
func TestWindowPacingH1CreditAdmissionCannotStarveResend(t *testing.T) {
	testWindowPacingH1AdmissionOwnerProgress(t, false)
}

func TestWindowPacingH1CreditAdmissionCannotHideRouteChange(t *testing.T) {
	testWindowPacingH1AdmissionOwnerProgress(t, true)
}

func testWindowPacingH1AdmissionOwnerProgress(t *testing.T, changeRoute bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		destination := NewId()
		startWorker := make(chan struct{})
		settings := DefaultClientSettings()
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		settings.SendBufferSettings.WindowSizing = WindowSizingFromDelivery
		settings.SendBufferSettings.ApplyWindowSizing()
		settings.SendBufferSettings.beforeRunSendSequenceForTest = func(id sendSequenceId) {
			if id.Destination == destination {
				select {
				case <-ctx.Done():
				case <-startWorker:
				}
			}
		}
		client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		route := make(Route, 64)
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
		transport := NewSendGatewayTransportWithType(TransportTypeH1)
		client.RouteManager().UpdateTransport(transport, []Route{route})
		sequence := client.sendBuffer.createSendSequence(sendSequenceId{Destination: destination}, &SendPack{Destination: destination, Ctx: ctx})
		synctest.Wait()
		service := sequence.windowPacer.service
		service.queueObservedAt = time.Now().Add(-time.Second)
		service.heldPacingRate = 2510
		sequence.windowPacer.rate, sequence.windowPacer.estimateRate = 2510, 183
		sequence.windowPacer.probeRate, sequence.windowPacer.probeLimit = 147928994, 2958580
		sequence.windowPacer.rateUpdated = time.Now()
		enqueued := make(chan struct{})
		go func() {
			defer close(enqueued)
			for range 10 {
				frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(4000)}
				if !client.SendWithTimeout(frame, destination, func(error) {}, -1) {
					MessagePoolReturn(frame.MessageBytes)
					return
				}
			}
		}()
		start := time.Now()
		close(startWorker)
		synctest.Wait()
		time.Sleep(10 * time.Millisecond)
		synctest.Wait()
		seen := map[Id]bool{}
		var head Id
		initial, retry := 0, false
		drain := func() {
			for len(route) > 0 {
				wire := <-route
				pack := decodeSendPackLifecycleWirePack(t, wire)
				id, err := IdFromBytes(pack.MessageId)
				MessagePoolReturn(wire)
				if err != nil {
					t.Fatal(err)
				}
				if seen[id] {
					retry = retry || id == head
				} else {
					if initial == 0 {
						head = id
					}
					seen[id] = true
					initial++
				}
			}
		}
		drain()
		if changeRoute {
			before := sequence.transferFlightPolicy()
			client.RouteManager().UpdateTransport(transport, nil)
			client.RouteManager().UpdateTransport(NewSendGatewayTransportWithType(TransportTypeH3), []Route{route})
			synctest.Wait()
			after := sequence.transferFlightPolicy()
			if after.h1Only || after.generation == before.generation {
				t.Fatal("fixture did not replace the H1-only route generation")
			}
			time.Sleep(time.Until(start.Add(100 * time.Millisecond)))
			synctest.Wait()
			drain()
			t.Logf("initial physical messages after H1 retirement=%d", initial)
			if initial != 10 {
				t.Error("H1-only admission wait ignored the replacement reliable route generation")
			}
			cancel()
			<-enqueued
			return
		}
		time.Sleep(time.Until(start.Add(3 * time.Second)))
		synctest.Wait()
		drain()
		t.Logf("initial physical messages=%d retry by 3s=%t", initial, retry)
		if !retry {
			t.Error("fresh-data credit wait blocked the owner from retrying the lost head by its existing two-second recovery interval")
		}
		cancel()
		<-enqueued
	})
}
