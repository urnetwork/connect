// The window fixture must offer sustained traffic through backpressure without
// imposing a per-message expiration on the delivery rate it measures.
package connect

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// A held sequence owns accepted work for longer than the old 50 ms deadline.
// Virtual time and the startup barrier make expiration independent of host load.
func TestSendWindowOfferWaitsForBackpressure(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		entered := make(chan struct{})
		release := make(chan struct{})
		var runs atomic.Int32
		var destination Id
		harness := newSendWindowHarnessWithClient(t, ctx, 25*time.Millisecond,
			func(settings *SendBufferSettings) {
				settings.beforeRunSendSequenceForTest = func(id sendSequenceId) {
					if id.Destination == destination && runs.Add(1) == 1 {
						close(entered)
						<-release
					}
				}
			}, func(settings *ClientSettings) {
				settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
			})
		destination = harness.receiverId
		offerDone := make(chan struct{})
		go func() {
			defer close(offerDone)
			harness.offer(t, 4*1024, 300*time.Millisecond)
		}()
		<-entered
		synctest.Wait()
		time.Sleep(100 * time.Millisecond)
		close(release)
		<-offerDone
		synctest.Wait()
		stats := harness.sender.ReceiveStats()
		if stats.SendPackDeadlineDropCount != 0 {
			t.Fatalf("fixture expired %d accepted Packs under backpressure; delivery must be bounded by the path", stats.SendPackDeadlineDropCount)
		}
		if runs.Load() != 1 {
			t.Fatalf("ending the offer ran %d application sequences; the same path samples must survive between offers", runs.Load())
		}
	})
}
