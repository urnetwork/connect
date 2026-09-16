// Sequence-wide retry bookkeeping must distinguish a different physical copy.
package connect

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Protecting the probe's own timeout is insufficient if a later message's
// ordinary recovery erases it. This is the real worker's second due item.
func TestWindowPacingLaterMessageRetryPreservesUnretriedProbe(t *testing.T) {
	testWindowPacingProbeRecoveryFixture(t, nil, nil, func(t *testing.T, sequence *SendSequence, client *Client, route Route, probe *protocol.Pack, at time.Time, _ <-chan error) {
		time.Sleep(time.Millisecond)
		frame := &protocol.Frame{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: MessagePoolGet(1000)}
		if !client.SendWithTimeout(frame, sequence.destination, func(error) {}, time.Second) {
			MessagePoolReturn(frame.MessageBytes)
			t.Fatal("second Pack not admitted")
		}
		synctest.Wait()
		var second *protocol.Pack
		select {
		case bytes := <-route:
			second = decodeSendPackLifecycleWirePack(t, bytes)
			MessagePoolReturn(bytes)
		default:
			t.Fatal("later message was not physically written")
		}
		secondId, _ := IdFromBytes(second.MessageId)
		probeId, _ := IdFromBytes(probe.MessageId)
		if secondId == probeId {
			t.Fatal("second physical write reused the probe identity")
		}
		service := sequence.windowPacer.service
		probeMessage := func() Id {
			service.stateLock.Lock()
			defer service.stateLock.Unlock()
			return service.roundTripProbe.messageId
		}()
		if probeMessage != probeId {
			t.Fatal("new later message already erased the probe")
		}
		time.Sleep(300 * time.Millisecond)
		synctest.Wait()
		select {
		case bytes := <-route:
			retry := decodeSendPackLifecycleWirePack(t, bytes)
			MessagePoolReturn(bytes)
			retryId, _ := IdFromBytes(retry.MessageId)
			if retryId != secondId {
				t.Fatalf("wrong physical recovery: got=%s, want later=%s", retryId, secondId)
			}
		default:
			t.Fatal("later message ordinary timeout did not physically retry")
		}
		time.Sleep(time.Until(at.Add(1200 * time.Millisecond)))
		acknowledgeSendPackLifecycleWirePack(t, client, sequence.destination, probe)
		synctest.Wait()
		if got := sequence.windowPacer.service.roundTrip(); got != 1200*time.Millisecond {
			t.Fatalf("later-message retry erased unretried probe evidence: floor=%s, want1.2s", got)
		}
	})
}
