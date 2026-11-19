package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

// A window not yet armed: each poll of the outcome watchdog finds nothing to do.
func newOutcomeWatchTestWindow(ctx context.Context) *multiClientWindow {
	return &multiClientWindow{
		ctx:      ctx,
		settings: DefaultMultiClientSettings(),
	}
}

// The outcome watchdog arms one timer for its life, not one per poll.
func TestWindowOutcomeWatchReusesOneTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		window := newOutcomeWatchTestWindow(ctx)
		done := make(chan struct{})
		timers := timerAllocationsUnder(t, ").watchOutcome", func() {
			go func() {
				defer close(done)
				window.watchOutcome()
			}()
			time.Sleep(100 * time.Second)
		})
		cancel()
		<-done
		if 4 < timers {
			t.Fatalf("the outcome watchdog allocated %d timer objects over ~100 polls, want one timer for its life", timers)
		}
	})
}

// The first installed provider disarms the watchdog for the window's life, so
// the watcher ends instead of polling on.
func TestWindowOutcomeWatchEndsOnceAdded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		window := newOutcomeWatchTestWindow(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			window.watchOutcome()
		}()

		time.Sleep(10 * time.Second)
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("the outcome watcher ended before a provider was installed")
		default:
		}

		window.outcomeLock.Lock()
		window.everAdded = true
		window.outcomeLock.Unlock()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("the outcome watcher kept polling a window whose watchdog a provider disarmed")
		}
	})
}
