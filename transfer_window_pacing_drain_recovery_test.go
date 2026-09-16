// Controlled drains must cover observed flight and preserve unambiguous probes.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The tail has exactly 1.2 seconds of physical flight left when the successor
// asks to drain. Virtual-time barriers force expiry before the covering ACK.
func testWindowPacingConfiguredLongFlight(t *testing.T, mixed bool) {
	t.Helper()
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		settings := DefaultClientSettings()
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		settings.SendBufferSettings.WindowSizing = WindowSizingFromDelivery
		settings.SendBufferSettings.ApplyWindowSizing()
		settings.SendBufferSettings.AckTimeout = time.Minute
		settings.SendBufferSettings.beforeRunSendSequenceForTest = func(sendSequenceId) { <-ctx.Done() }
		client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		defer func() { cancel(); client.CloseAndWait(context.Background()) }()
		destination := NewId()
		sequence := client.sendBuffer.createSendSequence(sendSequenceId{Destination: destination}, &SendPack{Destination: destination})
		if sequence == nil || sequence.windowPacer.service == nil {
			t.Fatal("configured delivery-sized sequence did not acquire a shared service")
		}
		start := time.Now()
		service := sequence.windowPacer.service
		service.sent = 1000
		service.observeRoundTrip(time.Millisecond, 0, start.Add(-90*time.Millisecond))
		residence := 1200 * time.Millisecond
		if mixed {
			residence = 10 * time.Millisecond
		}
		for ago := 8; ago > 0; ago-- {
			service.observeRoundTrip(residence, 0, start.Add(-time.Duration(ago)*10*time.Millisecond))
		}
		if mixed {
			service.observeRoundTrip(1200*time.Millisecond, 0, start)
		}
		sequenceId, tail, resumed := sequence.sequenceId, NewId(), NewId()
		service.beginWrite(sequenceId, tail, 1, start, false)
		service.finishWrite(sequenceId, tail, true)
		pacer := &windowBurstPacer{service: service, serviceSequenceId: sequenceId, rate: 1000000}
		defer pacer.close()
		type admission struct {
			at  time.Time
			err error
		}
		admitted := make(chan admission, 1)
		go func() {
			err := pacer.waitForServiceMessage(context.Background(), 1000, false, sequenceId, resumed, 2)
			admitted <- admission{at: time.Now(), err: err}
		}()
		synctest.Wait()
		service.stateLock.Lock()
		drainUntil, waiterHead := service.drainUntil, service.waiterHead
		service.stateLock.Unlock()
		if drainUntil.IsZero() || waiterHead != &pacer.waiter {
			t.Fatal("the actual FIFO head did not enter its controlled drain")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		var result admission
		early := false
		select {
		case result = <-admitted:
			early = true
			t.Errorf("observed 1.2 s flight was released at %s before its covering ACK", result.at.Sub(start))
			service.finishWrite(sequenceId, resumed, true)
		default:
		}
		time.Sleep(200 * time.Millisecond)
		service.acknowledgeWrite(sequenceId, tail, 1, false, 0, time.Now())
		service.observe(1000, time.Now())
		synctest.Wait()
		if !early {
			result = <-admitted
			service.finishWrite(sequenceId, resumed, true)
		}
		if result.err != nil {
			t.Fatal(result.err)
		}
		at := result.at.Add(1200 * time.Millisecond)
		time.Sleep(time.Until(at))
		service.acknowledgeWrite(sequenceId, resumed, 2, false, 0, time.Now())
		service.observe(1000, time.Now())
		pacer.serviceAcked = 1000
		if got := service.roundTrip(); got != 1200*time.Millisecond {
			t.Errorf("expired pause lost the unqueued RTT probe: floor=%s, want 1.2s", got)
		}
	})
}

// A completed recent residence longer than one second must survive the bound.
func TestWindowPacingConfiguredLongFlightDrainsBeforeProbe(t *testing.T) {
	testWindowPacingConfiguredLongFlight(t, false)
}

// A real new long-path reply is eligible even before its statistics bucket
// completes; an older short-path mean cannot shorten the ensuing drain.
func TestWindowPacingDrainCoversLatestLongResidence(t *testing.T) {
	testWindowPacingConfiguredLongFlight(t, true)
}

// The service already observed a 1.2 second path, but this lane retains its
// 300 ms resend floor. The real worker must allow its drained probe's reply
// before creating an ambiguous second physical copy of that same message.
func TestWindowPacingDrainedProbeOutlivesStaleLaneTimer(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		destination := NewId()
		startWorker := make(chan struct{})
		initialWrite := make(chan struct{})
		releaseInitial := make(chan struct{})
		settings := DefaultClientSettings()
		settings.Log = NewNoopLogger()
		settings.EncryptionSettings.Mode = EncryptionModeOff
		settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		settings.SendBufferSettings.WindowSizing = WindowSizingFromDelivery
		settings.SendBufferSettings.ApplyWindowSizing()
		settings.SendBufferSettings.AckTimeout = time.Minute
		settings.SendBufferSettings.afterInitialWriteQueuedForTest = func(id sendSequenceId, number uint64) {
			if id.Destination == destination && number == 0 {
				close(initialWrite)
				select {
				case <-releaseInitial:
				case <-ctx.Done():
				}
			}
		}
		settings.SendBufferSettings.beforeRunSendSequenceForTest = func(id sendSequenceId) {
			if id.Destination == destination {
				select {
				case <-startWorker:
				case <-ctx.Done():
				}
			}
		}
		client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
		route := make(Route, 32)
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
		client.RouteManager().UpdateTransport(
			&h1SendClientTransportForGroupTest{sendClientTransport: NewSendClientTransport(DestinationId(destination))},
			[]Route{route},
		)
		sequence := client.sendBuffer.createSendSequence(sendSequenceId{Destination: destination}, &SendPack{Destination: destination})
		synctest.Wait()
		service := sequence.windowPacer.service
		start := time.Now()
		service.observeRoundTrip(time.Millisecond, 0, start.Add(-90*time.Millisecond))
		for ago := 8; ago > 0; ago-- {
			service.observeRoundTrip(1200*time.Millisecond, 0, start.Add(-time.Duration(ago)*10*time.Millisecond))
		}
		sequence.rttWindow.closeSendTime(uint64(start.Add(-time.Millisecond).UnixMilli()), start)
		if got := sequence.rttWindow.ScaledRtt(); got != 300*time.Millisecond {
			t.Fatalf("stale lane resend interval=%s, want 300ms", got)
		}
		tailSequence, tailMessage := NewId(), NewId()
		service.sent = 1000
		service.beginWrite(tailSequence, tailMessage, 1, start.Add(-1100*time.Millisecond), false)
		service.finishWrite(tailSequence, tailMessage, true)
		frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(1000)}
		if !client.SendWithTimeout(frame, destination, func(error) {}, time.Second) {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatal("probe Pack was not admitted")
		}
		close(startWorker)
		synctest.Wait()
		service.stateLock.Lock()
		drainUntil, waiterHead := service.drainUntil, service.waiterHead
		service.stateLock.Unlock()
		if drainUntil.IsZero() || waiterHead != &sequence.windowPacer.waiter {
			t.Fatal("actual send worker did not wait for its old physical tail")
		}
		time.Sleep(100 * time.Millisecond)
		service.acknowledgeWrite(tailSequence, tailMessage, 1, false, 0, time.Now())
		service.observe(1000, time.Now())
		synctest.Wait()
		<-initialWrite
		var bytes []byte
		select {
		case bytes = <-route:
		default:
			t.Fatal("successful drain did not release the actual probe write")
		}
		pack := decodeSendPackLifecycleWirePack(t, bytes)
		probeId, err := IdFromBytes(pack.MessageId)
		if err != nil {
			t.Fatal(err)
		}
		MessagePoolReturn(bytes)
		probeAt := time.Now()
		probeState := func() windowPacingRoundTripProbe {
			service.stateLock.Lock()
			defer service.stateLock.Unlock()
			return service.roundTripProbe
		}()
		if probeState.messageId != probeId || !probeState.written || !probeState.resetService {
			t.Fatal("released initial write is not the physically confirmed controlled probe")
		}
		if item := sequence.resendQueue.PeekFirst(); item == nil || item.messageId != probeId || item.resendTime != probeAt.Add(300*time.Millisecond) {
			t.Fatalf("initial recovery must start its unchanged 300ms lane timer at the physical write: %+v", item)
		}
		close(releaseInitial)
		synctest.Wait()
		time.Sleep(300 * time.Millisecond)
		synctest.Wait()
		select {
		case retry := <-route:
			retryPack := decodeSendPackLifecycleWirePack(t, retry)
			retryId, decodeErr := IdFromBytes(retryPack.MessageId)
			MessagePoolReturn(retry)
			if decodeErr != nil || retryId != probeId {
				t.Fatalf("retry identity=%s error=%v, want the original probe=%s", retryId, decodeErr, probeId)
			}
			t.Errorf("same probe was physically retried after %s despite observed 1.2s residence", time.Since(probeAt))
		default:
		}
		time.Sleep(time.Until(probeAt.Add(1200 * time.Millisecond)))
		acknowledgeSendPackLifecycleWirePack(t, client, destination, pack)
		synctest.Wait()
		if got := service.roundTrip(); got != 1200*time.Millisecond {
			t.Errorf("premature recovery destroyed the unambiguous floor sample: got=%s, want=1.2s", got)
		}
	})
}
