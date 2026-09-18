// Physical-lane fixtures separate serialization from propagation latency.
package connect

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// Preparation borrows a frame, optionally refuses it or supplies a release
// barrier. Delivery takes the frame and must honor cancellation if it blocks.
type flightGateTestLaneSettings struct {
	latency       time.Duration
	queueFrames   int
	serialization func() time.Duration
	prepare       func([]byte) (bool, <-chan struct{})
	deliver       func([]byte, uint64)
}

// Takes each dequeued frame. A bounded pipeline overlaps serialization and
// latency, and one delivery worker preserves FIFO even when a delivery blocks.
// Cancellation joins both workers and returns all frames still owned here.
func forwardFlightGateTestLane(
	ctx context.Context,
	workers *sync.WaitGroup,
	from Route,
	settings flightGateTestLaneSettings,
) {
	type pendingFrame struct {
		frameBytes []byte
		ticket     uint64
		arrival    time.Time
		release    <-chan struct{}
	}
	queueFrames := settings.queueFrames
	if queueFrames <= 0 {
		queueFrames = 256
	}
	pending := make(chan pendingFrame, queueFrames)
	workers.Add(2)
	go func() {
		defer workers.Done()
		defer close(pending)
		var ticket uint64
		for {
			if ctx.Err() != nil {
				return
			}
			var frameBytes []byte
			select {
			case <-ctx.Done():
				return
			case frameBytes = <-from:
			}
			if frameBytes == nil {
				continue
			}
			ticket += 1
			if settings.serialization != nil {
				select {
				case <-ctx.Done():
					MessagePoolReturn(frameBytes)
					return
				case <-time.After(settings.serialization()):
				}
			}
			var release <-chan struct{}
			if settings.prepare != nil {
				var admitted bool
				admitted, release = settings.prepare(frameBytes)
				if !admitted {
					MessagePoolReturn(frameBytes)
					continue
				}
			}
			frame := pendingFrame{
				frameBytes: frameBytes,
				ticket:     ticket,
				arrival:    time.Now().Add(settings.latency),
				release:    release,
			}
			select {
			case <-ctx.Done():
				MessagePoolReturn(frameBytes)
				return
			case pending <- frame:
			}
		}
	}()
	go func() {
		defer workers.Done()
		defer func() {
			for frame := range pending {
				MessagePoolReturn(frame.frameBytes)
			}
		}()
		for frame := range pending {
			select {
			case <-ctx.Done():
				MessagePoolReturn(frame.frameBytes)
				return
			case <-time.After(time.Until(frame.arrival)):
			}
			if frame.release != nil {
				select {
				case <-ctx.Done():
					MessagePoolReturn(frame.frameBytes)
					return
				case <-frame.release:
				}
			}
			settings.deliver(frame.frameBytes, frame.ticket)
		}
	}()
}

// A runnable later frame cannot pass the blocked first delivery. The virtual
// clock and quiescence barrier force that ordering without scheduler luck.
func TestFlightGateTestLaneKeepsDeliveryOrderBehindBlockedHead(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		from := make(Route, 2)
		delivered := make(chan uint64, 2)
		releaseHead := make(chan struct{})
		var workers sync.WaitGroup
		forwardFlightGateTestLane(ctx, &workers, from, flightGateTestLaneSettings{
			latency: time.Second,
			deliver: func(frameBytes []byte, ticket uint64) {
				defer MessagePoolReturn(frameBytes)
				if ticket == 1 {
					<-releaseHead
				}
				delivered <- ticket
			},
		})
		from <- MessagePoolGet(64)
		from <- MessagePoolGet(64)
		synctest.Wait()
		time.Sleep(time.Second)
		synctest.Wait()
		select {
		case ticket := <-delivered:
			t.Errorf("lane delivered frame %d while its first frame was blocked", ticket)
		default:
		}
		close(releaseHead)
		synctest.Wait()
		for want := uint64(1); want <= 2; want += 1 {
			select {
			case got := <-delivered:
				if got != want {
					t.Errorf("delivered frame %d, want %d", got, want)
				}
			default:
				t.Errorf("frame %d was not delivered after releasing the head", want)
			}
		}
		cancel()
		workers.Wait()
	})
}

// Propagation is charged to each frame's serialization finish, so it overlaps
// the next serialization instead of reducing capacity to one frame per RTT.
func TestFlightGateTestLaneOverlapsSerializationAndLatency(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		from := make(Route, 2)
		start := time.Now()
		var arrivals []time.Duration
		var workers sync.WaitGroup
		forwardFlightGateTestLane(ctx, &workers, from, flightGateTestLaneSettings{
			latency:       100 * time.Millisecond,
			serialization: func() time.Duration { return 10 * time.Millisecond },
			deliver: func(frameBytes []byte, _ uint64) {
				arrivals = append(arrivals, time.Since(start))
				MessagePoolReturn(frameBytes)
			},
		})
		from <- MessagePoolGet(64)
		from <- MessagePoolGet(64)
		time.Sleep(120 * time.Millisecond)
		synctest.Wait()
		if len(arrivals) != 2 || arrivals[0] != 110*time.Millisecond || arrivals[1] != 120*time.Millisecond {
			t.Errorf("arrival offsets %v, want [110ms 120ms]", arrivals)
		}
		cancel()
		workers.Wait()
	})
}

// Cancellation releases a head held behind a barrier and every bounded queued
// frame. Pool reconciliation detects a forgotten pipeline or producer owner.
func TestFlightGateTestLaneCancellationReturnsHeldFrames(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		from := make(Route, 4)
		release := make(chan struct{})
		var workers sync.WaitGroup
		forwardFlightGateTestLane(ctx, &workers, from, flightGateTestLaneSettings{
			queueFrames: 2,
			prepare: func([]byte) (bool, <-chan struct{}) {
				return true, release
			},
			deliver: func(frameBytes []byte, _ uint64) {
				MessagePoolReturn(frameBytes)
				t.Error("delivered a frame before its release")
			},
		})
		for range 4 {
			from <- MessagePoolGet(64)
		}
		synctest.Wait()
		cancel()
		workers.Wait()
		for len(from) != 0 {
			MessagePoolReturn(<-from)
		}
	})
}
