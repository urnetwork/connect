// The sequence deadline timer never moves a pending timer: moving one makes
// the runtime walk a whole P's timer heap once the moved deadline arrives
// (server LBALTOPT2 §3.4), so an earlier deadline replaces the pending timer
// and only a timer that already fired is reset.
package connect

import (
	"runtime"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

func TestSequenceDeadlineTimerReplacesAPendingTimerAndResetsAFiredOne(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		timer := newSequenceDeadlineTimer()
		defer timer.stop()

		if !timer.armBy(time.Now().Add(time.Hour)) {
			t.Fatal("the first deadline did not arm the timer")
		}
		pending := timer.timer
		if timer.armBy(time.Now().Add(2 * time.Hour)) {
			t.Fatal("a later deadline moved the timer")
		}
		if timer.timer != pending {
			t.Fatal("a later deadline replaced the timer")
		}

		// an earlier deadline replaces the pending timer rather than moving it
		start := time.Now()
		if !timer.armBy(start.Add(10 * time.Millisecond)) {
			t.Fatal("an earlier deadline did not move the wake")
		}
		if timer.timer == pending {
			t.Fatal("an earlier deadline moved the pending timer instead of replacing it")
		}
		if pending.Stop() {
			t.Fatal("the replaced timer was still pending")
		}
		<-timer.wake
		if waited := time.Since(start); waited != 10*time.Millisecond {
			t.Fatalf("the wake came after %s, want 10ms", waited)
		}
		timer.woke()

		// the fired timer is in no heap, so the next deadline resets it
		fired := timer.timer
		if !timer.armBy(time.Now().Add(20 * time.Millisecond)) {
			t.Fatal("a deadline after a wake did not arm the timer")
		}
		if timer.timer != fired {
			t.Fatal("a deadline after a wake replaced the fired timer instead of resetting it")
		}
		start = time.Now()
		<-timer.wake
		if waited := time.Since(start); waited != 20*time.Millisecond {
			t.Fatalf("the reset wake came after %s, want 20ms", waited)
		}

		// the replaced hour-long timer never wakes the loop
		timer.woke()
		time.Sleep(3 * time.Hour)
		select {
		case <-timer.wake:
			t.Fatal("a replaced timer woke the loop")
		default:
		}
	})
}

// processCpu is the process's user and system CPU time.
func processCpu() time.Duration {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}

// Arming an earlier deadline among many pending timers, as a server's
// sequences do: "move" resets the pending timer, as armBy did before;
// "replace" is armBy now. The background timers are spread over every P's
// heap and scattered through memory, as a server's are, because a heap walk's
// cost is a cache miss per timer. The process CPU per op includes the walks a
// moved timer sets off.
func BenchmarkSequenceDeadlineTimerEarlierArmAmongManyTimers(b *testing.B) {
	const backgroundCount = 400_000
	const fillerByteCount = 512
	workers := runtime.GOMAXPROCS(0)
	background := make([][]*time.Timer, workers)
	filler := make([][][]byte, workers)
	var setup sync.WaitGroup
	for w := 0; w < workers; w += 1 {
		setup.Add(1)
		go func() {
			defer setup.Done()
			for i := 0; i < backgroundCount/workers; i += 1 {
				background[w] = append(background[w], time.AfterFunc(time.Hour+time.Duration(i)*time.Millisecond, func() {}))
				filler[w] = append(filler[w], make([]byte, fillerByteCount))
				if i%1024 == 0 {
					runtime.Gosched()
				}
			}
		}()
	}
	setup.Wait()
	defer func() {
		for _, timers := range background {
			for _, timer := range timers {
				timer.Stop()
			}
		}
		runtime.KeepAlive(filler)
	}()
	moveArmBy := func(self *sequenceDeadlineTimer, deadline time.Time) {
		if !self.armedDeadline.IsZero() && !deadline.Before(self.armedDeadline) {
			return
		}
		self.armedDeadline = deadline
		if self.timer == nil {
			wake := self.wake
			self.timer = time.AfterFunc(time.Until(deadline), func() {
				select {
				case wake <- struct{}{}:
				default:
				}
			})
		} else {
			self.timer.Reset(time.Until(deadline))
		}
	}
	for _, c := range []struct {
		name  string
		armBy func(*sequenceDeadlineTimer, time.Time)
	}{
		{name: "move", armBy: moveArmBy},
		{name: "replace", armBy: func(self *sequenceDeadlineTimer, deadline time.Time) { self.armBy(deadline) }},
	} {
		b.Run(c.name, func(b *testing.B) {
			const loops = 64
			startCpu := processCpu()
			b.ResetTimer()
			var run sync.WaitGroup
			for l := 0; l < loops; l += 1 {
				run.Add(1)
				go func() {
					defer run.Done()
					timers := make([]*sequenceDeadlineTimer, 64)
					for i := range timers {
						timers[i] = newSequenceDeadlineTimer()
					}
					defer func() {
						for _, timer := range timers {
							timer.stop()
						}
					}()
					for i := l; i < b.N; i += loops {
						timer := timers[i%len(timers)]
						// idle: the far deadline, then a burst's ack compression deadline
						timer.woke()
						c.armBy(timer, time.Now().Add(time.Hour))
						c.armBy(timer, time.Now().Add(10*time.Millisecond))
						if (i/loops)%16 == 0 {
							// a sequence's loop waits between bursts
							time.Sleep(time.Millisecond)
						}
					}
				}()
			}
			run.Wait()
			b.StopTimer()
			b.ReportMetric(float64(processCpu()-startCpu)/float64(b.N), "cpu-ns/op")
		})
	}
}
