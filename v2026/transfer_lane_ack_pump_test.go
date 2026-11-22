// The lane fixture models ordered propagation with bounded in-flight ownership.
package connect

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

const laneAckTestQueueFrames = 64

// Takes each received route buffer through the shared ordered propagation
// fixture. Its fixed queue, one reader and one delivery worker own at most
// queueFrames+2 frames; completion joins every owner, including a blocked send.
func startLaneAckTestPump(ctx context.Context, from Route, to Route, delay time.Duration) <-chan struct{} {
	var workers sync.WaitGroup
	forwardFlightGateTestLane(ctx, &workers, from, flightGateTestLaneSettings{
		latency:     delay,
		queueFrames: laneAckTestQueueFrames,
		deliver: func(frame []byte, _ uint64) {
			select {
			case to <- frame:
			case <-ctx.Done():
				MessagePoolReturn(frame)
			}
		},
	})
	done := make(chan struct{})
	go func() {
		workers.Wait()
		close(done)
	}()
	return done
}

// Two frames entering together arrive together after one propagation delay.
// Virtual time and quiescence separate latency from per-frame service time.
func TestLaneAckPropagationAllowsMultipleFramesInFlight(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		from := make(Route, 2)
		to := make(Route, 2)
		done := startLaneAckTestPump(ctx, from, to, 20*time.Millisecond)
		defer func() {
			cancel()
			<-done
			for len(from) != 0 {
				MessagePoolReturn(<-from)
			}
			for len(to) != 0 {
				MessagePoolReturn(<-to)
			}
		}()
		from <- MessagePoolCopy([]byte{0})
		from <- MessagePoolCopy([]byte{1})
		synctest.Wait()
		time.Sleep(20 * time.Millisecond)
		synctest.Wait()
		for want := byte(0); want < 2; want += 1 {
			select {
			case frame := <-to:
				if frame[0] != want {
					t.Errorf("propagated identity=%d, want %d in FIFO order", frame[0], want)
				}
				MessagePoolReturn(frame)
			default:
				t.Errorf("frame %d did not arrive after one propagation delay", want)
			}
		}
	})
}

// A receiver that takes nothing bounds pipeline ownership and cancellation
// returns every accepted buffer before completion, including the blocked head.
func TestLaneAckPropagationBoundsBlockedReceiverAndJoins(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		baseline := MessagePoolOutstandingCount()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		const offered = 128
		from := make(Route, offered)
		to := make(Route)
		done := startLaneAckTestPump(ctx, from, to, 20*time.Millisecond)
		for range offered {
			from <- MessagePoolGet(64)
		}
		synctest.Wait()
		time.Sleep(20 * time.Millisecond)
		synctest.Wait()
		// One reader-owned frame and one blocked delivery accompany the
		// fixed pending queue. This is one read after a quiescence barrier.
		wantCallerOwned := offered - laneAckTestQueueFrames - 2
		if len(from) != wantCallerOwned {
			t.Errorf("blocked receiver left %d caller-owned frames, want %d; propagation ownership is not bounded", len(from), wantCallerOwned)
		}
		cancel()
		<-done
		if outstanding := MessagePoolOutstandingCount(); outstanding > baseline+uint64(len(from)) {
			t.Errorf("pump completed with %d retained roots beyond its caller's %d", outstanding-baseline, len(from))
		}
		for len(from) != 0 {
			MessagePoolReturn(<-from)
		}
	})
}
