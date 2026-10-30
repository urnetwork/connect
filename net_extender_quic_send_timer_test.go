package connect

import (
	"context"
	"errors"
	"os"
	"testing"
	"testing/synctest"
	"time"

	"github.com/quic-go/quic-go/qlog"
)

// A writer that waits for send-flight credit is woken by every ACK that frees
// a root, and most wakes still find the flight full. The wait arms one timer
// for its whole life, not one per wake: a timer per wake allocated a timer and
// its channel and armed and stopped it each time.
func TestQuicSendFlightWaitArmsOneTimerAcrossWakes(t *testing.T) {
	const wakeCount = 64
	timerAllocCount := testing.AllocsPerRun(16, func() {
		time.NewTimer(time.Hour).Stop()
	})
	flight := newQuicSendFlight()
	deadline := time.Now().Add(time.Hour)
	allocCount := testing.AllocsPerRun(16, func() {
		flight.mutex.Lock()
		flight.frameCount = quicSendFlightWriteLimit
		flight.mutex.Unlock()
		passCount := 0
		// read once per pass of the wait, after it has found the flight full
		readDeadline := func() time.Time {
			passCount += 1
			if passCount == wakeCount {
				flight.mutex.Lock()
				flight.frameCount = 0
				flight.mutex.Unlock()
			}
			// wake the next pass, as a freed root does
			select {
			case flight.notify <- struct{}{}:
			default:
			}
			return deadline
		}
		if err := flight.wait(context.Background(), false, readDeadline); err != nil {
			t.Fatal(err)
		}
		if passCount < wakeCount {
			t.Fatalf("the wait returned after %d passes, expected %d", passCount, wakeCount)
		}
	})
	if timerAllocCount < allocCount {
		t.Fatalf("a wait woken %d times allocated %.0f objects, expected at most one timer's %.0f", wakeCount, allocCount, timerAllocCount)
	}
}

// The reused timer follows the writer's deadline when it moves while the
// writer waits, later or earlier, and the full flight then fails the write at
// the new deadline exactly.
func TestQuicSendFlightWaitFollowsAMovedDeadline(t *testing.T) {
	cases := []struct {
		name          string
		movedDeadline time.Duration
	}{
		{name: "later", movedDeadline: 8 * time.Second},
		{name: "earlier", movedDeadline: 3 * time.Second},
	}
	for _, c := range cases {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			flight := newQuicSendFlight()
			writer := flight.newWriter(&flightTestStream{ctx: ctx})
			for i := range quicSendFlightWriteLimit {
				flightSent(flight, int64(i), &qlog.StreamFrame{StreamID: 0, Offset: int64(i), Length: 1})
			}
			start := time.Now()
			writer.SetWriteDeadline(start.Add(5 * time.Second))
			writeErrs := make(chan error, 1)
			go func() {
				_, err := writer.Write([]byte{1})
				writeErrs <- err
			}()
			time.Sleep(2 * time.Second)
			writer.SetWriteDeadline(start.Add(c.movedDeadline))
			time.Sleep(time.Until(start.Add(c.movedDeadline - time.Nanosecond)))
			synctest.Wait()
			select {
			case err := <-writeErrs:
				t.Fatalf("%s: the write ended at %s with %v, before its deadline %s", c.name, time.Since(start), err, c.movedDeadline)
			default:
			}
			time.Sleep(time.Nanosecond)
			synctest.Wait()
			select {
			case err := <-writeErrs:
				if !errors.Is(err, os.ErrDeadlineExceeded) {
					t.Fatalf("%s: the write ended with %v, expected its deadline", c.name, err)
				}
			default:
				t.Fatalf("%s: the write outlived its deadline %s", c.name, c.movedDeadline)
			}
			flight.close()
		})
	}
}
