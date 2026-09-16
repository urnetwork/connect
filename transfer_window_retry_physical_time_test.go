// Local retry pacing cannot spend the next physical recovery interval.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The first retry is due after 300 ms but its existing serialization reservation
// holds its physical write until 1.2 s. The next 600 ms backed-off interval starts
// there; a still-missing reply must be recovered at 1.8 s, without an immediate
// duplicate when the paced first retry returns.
func TestWindowPacingRetryStartsItsNextRecoveryAtPhysicalWrite(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		destination := NewId()
		initialWritten, releaseInitial := make(chan struct{}), make(chan struct{})
		releaseSecondDue := make(chan struct{})
		type recoveryBoundary struct {
			at       time.Time
			physical time.Time
			deadline time.Time
			copies   int
		}
		secondDue := make(chan recoveryBoundary, 1)
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
		settings.SendBufferSettings.afterInitialWriteQueuedForTest = func(id sendSequenceId, number uint64) {
			if id.Destination == destination && number == 0 {
				close(initialWritten)
				select {
				case <-releaseInitial:
				case <-ctx.Done():
				}
			}
		}
		var sequence *SendSequence
		firings := 0
		settings.SendBufferSettings.beforeDueResendForTest = func(id sendSequenceId, number uint64) {
			if id.Destination != destination || number != 0 {
				return
			}
			firings++
			if firings == 2 {
				item := sequence.resendQueue.PeekFirst()
				secondDue <- recoveryBoundary{at: time.Now(), physical: sequence.windowPacer.waiter.sentAt, deadline: item.resendTime, copies: item.sendCount}
				select {
				case <-releaseSecondDue:
				case <-ctx.Done():
				}
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
		start := time.Now()
		if !client.SendWithTimeout(frame, destination, func(error) {}, time.Second) {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatal("Pack not admitted")
		}
		<-initialWritten
		sequence = client.sendBuffer.lookupSendSequence(sendSequenceId{Destination: destination}, nil)
		readPack := func() *protocol.Pack {
			select {
			case bytes := <-route:
				defer MessagePoolReturn(bytes)
				return decodeSendPackLifecycleWirePack(t, bytes)
			default:
				t.Fatal("physical write missing")
				return nil
			}
		}
		original := readPack()
		service := sequence.windowPacer.service
		service.stateLock.Lock()
		service.next = start.Add(1200 * time.Millisecond)
		service.stateLock.Unlock()
		close(releaseInitial)
		synctest.Wait()
		time.Sleep(1200 * time.Millisecond)
		synctest.Wait()
		retry := readPack()
		if string(retry.MessageId) != string(original.MessageId) {
			t.Fatal("first physical retry changed logical identity")
		}
		select {
		case boundary := <-secondDue:
			t.Logf("first-retry-due=300ms first-retry-physical=%s next-deadline=%s next-firing=%s copies=%d", boundary.physical.Sub(start), boundary.deadline.Sub(start), boundary.at.Sub(start), boundary.copies)
			t.Fatal("local pacing consumed the next backed-off interval before the first retry physically began")
		default:
		}
		time.Sleep(time.Until(start.Add(1800*time.Millisecond - time.Nanosecond)))
		synctest.Wait()
		select {
		case <-secondDue:
			t.Fatal("retry fired before its unchanged 600 ms physical backoff")
		default:
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		select {
		case boundary := <-secondDue:
			t.Logf("first-retry-due=300ms first-retry-physical=%s next-deadline=%s next-firing=%s copies=%d", boundary.physical.Sub(start), boundary.deadline.Sub(start), boundary.at.Sub(start), boundary.copies)
			if boundary.physical != start.Add(1200*time.Millisecond) || boundary.deadline != start.Add(1800*time.Millisecond) || boundary.at != boundary.deadline || boundary.copies != 2 {
				t.Fatal("physical retry did not retain the unchanged bounded backoff")
			}
		default:
			t.Fatal("missing reply was not recovered at physical+600ms")
		}
		close(releaseSecondDue)
		synctest.Wait()
		retry = readPack()
		if string(retry.MessageId) != string(original.MessageId) {
			t.Fatal("next physical retry changed logical identity")
		}
	})
}
