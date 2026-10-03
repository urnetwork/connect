// A delayed physical write must not move an older deadline behind the new one.
package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Both physical writes use one real sequence worker. The second begins after
// local pacing, while the older unacknowledged item is already due. Extending
// the second deadline must repair both recovery heaps without removing its
// ACK lookup or byte ownership.
func TestWindowPacingPhysicalDeadlineKeepsOlderRecoveryFirst(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		destination := NewId()
		firstWritten, secondWritten := make(chan struct{}), make(chan struct{})
		releaseFirst, releaseSecond := make(chan struct{}), make(chan struct{})
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
			if id.Destination != destination || number > 1 {
				return
			}
			written, release := firstWritten, releaseFirst
			if number == 1 {
				written, release = secondWritten, releaseSecond
			}
			close(written)
			select {
			case <-release:
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
		send := func() {
			frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(1280)}
			if !client.SendWithTimeout(frame, destination, func(error) {}, time.Second) {
				MessagePoolReturn(frame.MessageBytes)
				t.Fatal("Pack not admitted")
			}
		}
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
		start := time.Now()
		send()
		<-firstWritten
		first := readPack()
		sequence := client.sendBuffer.lookupSendSequence(sendSequenceId{Destination: destination}, nil)
		queue := sequence.resendQueue
		queue.stateLock.Lock()
		// There is exactly one item in either heap; establish the older
		// timer before the real second admission changes their ordering.
		if len(queue.orderedItems) != 1 {
			queue.stateLock.Unlock()
			t.Fatal("first physical item is not retained alone")
		}
		queue.orderedItems[0].resendTime = start.Add(500 * time.Millisecond)
		queue.stateLock.Unlock()
		service := sequence.windowPacer.service
		service.stateLock.Lock()
		service.next = start.Add(600 * time.Millisecond)
		service.stateLock.Unlock()
		// Let the first nominal burst expire before offering the second.
		time.Sleep(20 * time.Millisecond)
		send()
		close(releaseFirst)
		<-secondWritten
		second := readPack()
		if second.SequenceNumber != first.SequenceNumber+1 || time.Since(start) != 600*time.Millisecond {
			t.Fatalf("second physical boundary=%s sequence=%d", time.Since(start), second.SequenceNumber)
		}
		firstDue, lastDue := queue.PeekFirst(), queue.PeekLast()
		count, retainedBytes := queue.QueueSize()
		t.Logf("at=%s first=%d/%s last=%d/%s retained=%d/%d", time.Since(start), firstDue.sequenceNumber, firstDue.resendTime.Sub(start), lastDue.sequenceNumber, lastDue.resendTime.Sub(start), count, retainedBytes)
		if firstDue.sequenceNumber != first.SequenceNumber || lastDue.sequenceNumber != second.SequenceNumber {
			t.Error("physical deadline update corrupted recovery heap ordering")
		}
		if count != 2 || retainedBytes <= 0 {
			t.Error("retiming lost retained ownership")
		}
		close(releaseSecond)
		synctest.Wait()
		select {
		case bytes := <-route:
			retry := decodeSendPackLifecycleWirePack(t, bytes)
			MessagePoolReturn(bytes)
			if string(retry.MessageId) != string(first.MessageId) {
				t.Error("newer message overtook the older due recovery")
			}
		default:
			t.Error("older due message was parked behind the new physical deadline")
		}
	})
}
