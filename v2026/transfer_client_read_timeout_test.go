// The client receive loop waits on its routes without a read timeout: a read
// that timed out returned no frame and the loop read again, so the timeout
// cost a timer per inbound frame and a wake per interval and did nothing.
package connect

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// An idle client's receive loop never wakes for a read timeout and never arms
// a read timer. It still wakes for a route added while it waits, and it exits
// with the client.
func TestClientReceiveLoopWaitsWithoutAReadTimeout(t *testing.T) {
	assertMessagePoolOwnership(t)
	MessagePoolReturn(MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		emptyReads := atomic.Int64{}
		frameReads := atomic.Int64{}
		settings := closeWaitClientSettings()
		settings.ReceiveBufferSettings.ProgressObserver = func(event TransferProgressEvent) {
			if event.Stage != "receive_wire" {
				return
			}
			if event.ByteCount == 0 {
				// a read that returned no frame
				emptyReads.Add(1)
			} else {
				frameReads.Add(1)
			}
		}

		var client *Client
		readTimers := timerAllocationsUnder(t, ").readWithCarrier", func() {
			client = NewClient(t.Context(), NewId(), NewNoContractClientOob(), settings)
			synctest.Wait()
			time.Sleep(10 * time.Minute)
			synctest.Wait()

			// a route that appears while the loop waits wakes it
			route := make(chan []byte, 1)
			client.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{route})
			synctest.Wait()
			frame := MessagePoolGet(64)
			for i := range frame {
				frame[i] = 0xff
			}
			route <- frame
			synctest.Wait()
			time.Sleep(10 * time.Minute)
			synctest.Wait()
		})
		if err := client.CloseAndWait(context.Background()); err != nil {
			t.Fatalf("the receive loop did not exit with its client: %v", err)
		}

		t.Logf("empty reads %d, frame reads %d, read timer objects %d", emptyReads.Load(), frameReads.Load(), readTimers)
		if got := emptyReads.Load(); got != 0 {
			t.Fatalf("the idle receive loop woke for %d read timeouts over 20 minutes, want none", got)
		}
		if got := frameReads.Load(); got != 1 {
			t.Fatalf("the receive loop read %d frames from a route added while it waited, want 1", got)
		}
		if 0 < readTimers {
			t.Fatalf("the receive loop allocated %d read timer objects, want none", readTimers)
		}
	})
}
