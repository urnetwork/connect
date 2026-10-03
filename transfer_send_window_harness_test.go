// The send-window instrument must preserve its lossless carrier contract when
// a receive worker is delayed; host scheduling must not manufacture loss.
package connect

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Hold the receive worker behind a barrier and fill its one-Pack handoff.
// The next complete frames must wait on the carrier, then arrive after release.
func TestSendWindowHarnessPreservesPacksUnderReceiveBackpressure(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		entered, release := make(chan struct{}), make(chan struct{})
		var enteredOnce sync.Once
		var releaseOnce sync.Once
		releaseWorker := func() { releaseOnce.Do(func() { close(release) }) }
		defer releaseWorker()
		harness := newSendWindowHarnessWithClient(t, ctx, 200*time.Millisecond, nil,
			func(settings *ClientSettings) {
				settings.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
				settings.ReceiveBufferSettings.SequenceBufferSize = 1
				settings.ReceiveBufferSettings.beforeRunReceiveSequenceForTest = func(receiveSequenceId) {
					enteredOnce.Do(func() { close(entered) })
					<-release
				}
			})
		var delivered atomic.Int64
		harness.receiver.AddReceiveCallback(func(TransferPath, []*protocol.Frame, Peer) {
			delivered.Add(1)
		})
		for i := range 3 {
			frame := RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: "synthetic window frame"})
			admitted, err := harness.sender.SendWithTimeoutDetailed(frame, harness.receiverId, nil, 0)
			if !admitted || err != nil {
				MessagePoolReturn(frame.MessageBytes)
				t.Fatalf("offer %d: admitted=%t err=%v", i, admitted, err)
			}
			if i == 0 {
				<-entered
			}
			synctest.Wait()
		}
		before := harness.receiver.ReceiveStats()
		releaseWorker()
		synctest.Wait()
		if before.PackHandoffDropCount != 0 || delivered.Load() != 3 {
			t.Fatalf("lossless window fixture dropped %d Packs behind a paused receiver and delivered %d of 3", before.PackHandoffDropCount, delivered.Load())
		}
	})
}
