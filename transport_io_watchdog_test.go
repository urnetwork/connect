// An ioWatchdog bounds each operation to its timeout from its start with one
// timer, armed about once per timeout while operations run and idle without
// them. Virtual time places its boundaries exactly; one real-time test drives
// its arm handoff from two goroutines.
package connect

import (
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// A watchdog that counts its interrupts and timer arms.
type countedIoWatchdog struct {
	*ioWatchdog
	interrupts atomic.Int64
	arms       atomic.Int64
}

// Creates a counted watchdog with the timeout, stopped at cleanup.
func newCountedIoWatchdog(t *testing.T, timeout time.Duration) *countedIoWatchdog {
	counted := &countedIoWatchdog{}
	counted.ioWatchdog = newIoWatchdog(timeout, func() {
		counted.interrupts.Add(1)
	}, func() {
		counted.arms.Add(1)
	})
	t.Cleanup(counted.stop)
	return counted
}

// An operation still in progress is interrupted exactly its timeout after it
// began, once, and never before.
func TestIoWatchdogInterruptsExactlyTimeoutAfterTheOperationBegan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const timeout = 3 * time.Second
		watchdog := newCountedIoWatchdog(t, timeout)
		watchdog.begin(time.Now())
		time.Sleep(timeout - time.Nanosecond)
		synctest.Wait()
		if watchdog.interrupts.Load() != 0 {
			t.Fatal("interrupted before the timeout")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if watchdog.interrupts.Load() != 1 {
			t.Fatalf("%d interrupts at the timeout, want 1", watchdog.interrupts.Load())
		}
		time.Sleep(10 * timeout)
		synctest.Wait()
		if watchdog.interrupts.Load() != 1 || watchdog.arms.Load() != 1 {
			t.Fatalf("an interrupted watchdog went on: %d interrupts, %d arms", watchdog.interrupts.Load(), watchdog.arms.Load())
		}
	})
}

// A fire finds a later operation than the one it was armed for, and re-arms
// for the rest of that one's timeout.
func TestIoWatchdogReArmsForTheRestOfALaterOperation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const timeout = 2 * time.Second
		watchdog := newCountedIoWatchdog(t, timeout)
		watchdog.begin(time.Now())
		watchdog.end()
		time.Sleep(timeout / 4)
		later := time.Now()
		watchdog.begin(later)
		time.Sleep(time.Until(later.Add(timeout)) - time.Nanosecond)
		synctest.Wait()
		if watchdog.interrupts.Load() != 0 {
			t.Fatal("interrupted the later operation early, at the first operation's timeout")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if watchdog.interrupts.Load() != 1 || watchdog.arms.Load() != 2 {
			t.Fatalf("%d interrupts after %d arms at the later operation's timeout, want 1 after 2", watchdog.interrupts.Load(), watchdog.arms.Load())
		}
	})
}

// With no operation in progress at a fire the watchdog goes idle: no
// interrupt and no further fire, until the next operation arms it.
func TestIoWatchdogGoesIdleUntilTheNextOperation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const timeout = time.Second
		watchdog := newCountedIoWatchdog(t, timeout)
		watchdog.begin(time.Now())
		watchdog.end()
		time.Sleep(100 * timeout)
		synctest.Wait()
		if watchdog.interrupts.Load() != 0 || watchdog.arms.Load() != 1 {
			t.Fatalf("an idle watchdog: %d interrupts, %d arms, want none and 1", watchdog.interrupts.Load(), watchdog.arms.Load())
		}
		next := time.Now()
		watchdog.begin(next)
		if watchdog.arms.Load() != 2 {
			t.Fatal("the next operation did not arm the idle watchdog")
		}
		time.Sleep(time.Until(next.Add(timeout)))
		synctest.Wait()
		if watchdog.interrupts.Load() != 1 {
			t.Fatal("the next operation was not bounded")
		}
	})
}

// Short operations back to back, 10ms apart over ten timeouts, arm and fire
// the timer about once per timeout and are never interrupted.
func TestIoWatchdogBusyOperationsArmOncePerTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const timeout = time.Second
		watchdog := newCountedIoWatchdog(t, timeout)
		start := time.Now()
		operationCount := 0
		for time.Since(start) < 10*timeout {
			watchdog.begin(time.Now())
			time.Sleep(time.Millisecond)
			watchdog.end()
			operationCount += 1
			time.Sleep(9 * time.Millisecond)
		}
		synctest.Wait()
		if watchdog.interrupts.Load() != 0 {
			t.Fatal("interrupted an operation that ran less than its timeout")
		}
		if armAllowance := 1 + int64(time.Since(start)/timeout); armAllowance < watchdog.arms.Load() {
			t.Fatalf("%d operations over %s armed the timer %d times, want at most %d", operationCount, time.Since(start), watchdog.arms.Load(), armAllowance)
		}
	})
}

// A stopped watchdog never fires.
func TestIoWatchdogStopEndsTheTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const timeout = time.Second
		watchdog := newCountedIoWatchdog(t, timeout)
		watchdog.begin(time.Now())
		watchdog.stop()
		time.Sleep(10 * timeout)
		synctest.Wait()
		if watchdog.interrupts.Load() != 0 {
			t.Fatal("a stopped watchdog interrupted")
		}
	})
}

// Operations race the timer's idle handoff on real goroutines: whichever side
// arms the timer, an operation left in progress is still interrupted. A lost
// arm would leave the watchdog marked armed with no timer, and the last
// operation unbounded.
func TestIoWatchdogArmHandoffNeverLosesTheTimer(t *testing.T) {
	const timeout = 5 * time.Millisecond
	// an operation preempted past the timeout is rightly interrupted, which
	// ends that watchdog; the handoff is checked on a run with none
	for range 3 {
		watchdog := newCountedIoWatchdog(t, timeout)
		for i := range 2000 {
			watchdog.begin(time.Now())
			watchdog.end()
			if i%20 == 0 {
				// long enough for a fire to find no operation and go idle
				time.Sleep(timeout + time.Duration(i%3)*time.Millisecond)
			}
		}
		if watchdog.interrupts.Load() != 0 {
			continue
		}
		watchdog.begin(time.Now())
		if !waitForCondition(5*time.Second, func() bool { return watchdog.interrupts.Load() == 1 }) {
			t.Fatalf("the last operation was never interrupted after %d arms", watchdog.arms.Load())
		}
		return
	}
	t.Fatalf("every run had an operation preempted past %s", timeout)
}

// A busy H1 writer, a payload every 100ms over three WriteTimeouts, arms its
// write watchdog about once per WriteTimeout and installs no write deadline.
func TestH1BusyWriterArmsWriteWatchdogOncePerWriteTimeout(t *testing.T) {
	resetH1UpgradeTestState(t)
	MessagePoolReturn(MessagePoolGet(1))
	for _, h1Plus := range []bool{true, false} {
		synctest.Test(t, func(t *testing.T) {
			arms := &h1WatchdogArms{}
			fixture := newH1LivenessFixture(t, withH1Plus(h1Plus), arms.observe, func(settings *PlatformTransportSettings) {
				settings.ReadTimeout = time.Minute
			})
			writeTimeout := fixture.transport.settings.WriteTimeout
			fixture.client.resetDeadlineStats()
			arms.write.Store(0)
			start := time.Now()
			payloadCount := 0
			for time.Since(start) < 3*writeTimeout {
				fixture.sendMessage(t, h1PlusOwnedTestMessage(0x31, 64))
				payloadCount += 1
				synctest.Wait()
				time.Sleep(100 * time.Millisecond)
			}
			fixture.assertConnected(t)
			if int(fixture.payloads.Load()) != payloadCount {
				t.Fatalf("h1+=%t: the peer read %d of %d payloads", h1Plus, fixture.payloads.Load(), payloadCount)
			}
			if armAllowance := 1 + int64(time.Since(start)/writeTimeout); armAllowance < arms.write.Load() {
				t.Fatalf("h1+=%t: %d payloads over %s armed the write watchdog %d times, want at most %d", h1Plus, payloadCount, time.Since(start), arms.write.Load(), armAllowance)
			}
			if stats := fixture.client.deadlineStatsSnapshot(); 1 < stats.writeDeadlineCount {
				t.Fatalf("h1+=%t: %d payloads installed the write deadline %d times, want at most once", h1Plus, payloadCount, stats.writeDeadlineCount)
			}
		})
	}
}
