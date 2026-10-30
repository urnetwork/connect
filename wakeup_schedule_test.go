package connect

import (
	"testing"
	"testing/synctest"
	"time"
)

func TestWakeupTime(t *testing.T) {
	wakeupEpoch := 1 * time.Second
	cases := []struct {
		input    time.Duration
		expected time.Duration
	}{
		{
			input:    0,
			expected: 0,
		},
		{
			input:    1,
			expected: wakeupEpoch,
		},
		{
			input:    wakeupEpoch - 1,
			expected: wakeupEpoch,
		},
		{
			input:    wakeupEpoch,
			expected: wakeupEpoch,
		},
		{
			input:    wakeupEpoch + 1,
			expected: 2 * wakeupEpoch,
		},
	}

	for _, c := range cases {
		inputTime := time.Unix(0, int64(c.input))
		actual := time.Duration(WakeupTime(inputTime, wakeupEpoch).UnixNano())
		AssertEqual(t, c.expected, actual)
	}
}

func TestResetWakeupTimerDoesNotAllocate(t *testing.T) {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	resetWakeupTimer(timer, time.Hour, time.Second)

	allocs := testing.AllocsPerRun(1000, func() {
		resetWakeupTimer(timer, time.Hour, time.Second)
	})

	AssertEqual(t, 0.0, allocs)
}

// The offset schedule is the wall-clock one shifted by the offset.
func TestWakeupTimeWithOffset(t *testing.T) {
	epoch := time.Second
	cases := []struct {
		input    time.Duration
		offset   time.Duration
		expected time.Duration
	}{
		{input: 1, offset: 0, expected: epoch},
		{input: 0, offset: 300 * time.Millisecond, expected: 300 * time.Millisecond},
		{input: 300 * time.Millisecond, offset: 300 * time.Millisecond, expected: 300 * time.Millisecond},
		{input: 300*time.Millisecond + 1, offset: 300 * time.Millisecond, expected: epoch + 300*time.Millisecond},
		{input: epoch + 200*time.Millisecond, offset: 300 * time.Millisecond, expected: epoch + 300*time.Millisecond},
		{input: epoch, offset: epoch - 1, expected: 2*epoch - 1},
	}
	for _, c := range cases {
		inputTime := time.Unix(0, int64(c.input))
		actual := time.Duration(wakeupTimeWithOffset(inputTime, epoch, c.offset).UnixNano())
		if actual != c.expected {
			t.Errorf("input=%s offset=%s: got %s, want %s", c.input, c.offset, actual, c.expected)
		}
	}
}

// Feeds one write schedule to a writer that resets its ping timer after every
// write, as the platform writers did, and to one that paces from its last
// write. Both ping at the same instants; the paced one arms its timer about
// once per ping interval instead of once per write.
func TestPingPacerPingsWhereAResetPerWriteDid(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const epoch = time.Second
		start := time.Now()
		// off a boundary, so the end never races a ping
		end := start.Add(9500 * time.Millisecond)
		// a write every 50ms for three seconds, a gap, two writes, then idle
		var writeTimes []time.Time
		for i := range 60 {
			writeTimes = append(writeTimes, start.Add(time.Duration(i+1)*50*time.Millisecond))
		}
		writeTimes = append(writeTimes, start.Add(5300*time.Millisecond), start.Add(5350*time.Millisecond))

		type writerResult struct {
			pingTimes []time.Duration
			armCount  int
		}
		runWriter := func(paced bool, writes <-chan struct{}, result chan<- writerResult) {
			var pingTimes []time.Duration
			armCount := 0
			var pacer *pingPacer
			var timer *time.Timer
			if paced {
				pacer = newPingPacer(epoch, epoch, 0, time.Now(), func() { armCount += 1 })
				defer pacer.stop()
				timer = pacer.timer
			} else {
				timer = time.NewTimer(0)
				defer timer.Stop()
				resetWakeupTimer(timer, epoch, epoch)
				armCount += 1
			}
			stop := time.NewTimer(time.Until(end))
			defer stop.Stop()
			for {
				select {
				case <-stop.C:
					result <- writerResult{pingTimes: pingTimes, armCount: armCount}
					return
				case <-writes:
					if paced {
						pacer.wrote(time.Now())
					} else {
						resetWakeupTimer(timer, epoch, epoch)
						armCount += 1
					}
				case <-timer.C:
					if paced {
						if !pacer.fired(time.Now()) {
							continue
						}
						pingTimes = append(pingTimes, time.Since(start))
						pacer.pinged(time.Now())
					} else {
						pingTimes = append(pingTimes, time.Since(start))
						resetWakeupTimer(timer, epoch, epoch)
						armCount += 1
					}
				}
			}
		}
		resetWrites := make(chan struct{})
		pacedWrites := make(chan struct{})
		resetResult := make(chan writerResult, 1)
		pacedResult := make(chan writerResult, 1)
		go runWriter(false, resetWrites, resetResult)
		go runWriter(true, pacedWrites, pacedResult)
		for _, writeTime := range writeTimes {
			time.Sleep(time.Until(writeTime))
			resetWrites <- struct{}{}
			pacedWrites <- struct{}{}
		}
		reset := <-resetResult
		paced := <-pacedResult

		if len(paced.pingTimes) != len(reset.pingTimes) {
			t.Fatalf("paced pings at %v, a reset per write pinged at %v", paced.pingTimes, reset.pingTimes)
		}
		for i := range paced.pingTimes {
			if paced.pingTimes[i] != reset.pingTimes[i] {
				t.Fatalf("paced pings at %v, a reset per write pinged at %v", paced.pingTimes, reset.pingTimes)
			}
		}
		if len(paced.pingTimes) == 0 {
			t.Fatal("neither writer pinged")
		}
		// one arm to start, at most one per ping interval of traffic, and one
		// per ping
		if maxArmCount := 1 + 6 + len(paced.pingTimes); maxArmCount < paced.armCount || reset.armCount <= paced.armCount {
			t.Fatalf("the paced writer armed its timer %d times, a reset per write %d times", paced.armCount, reset.armCount)
		}
	})
}

// With an offset the pacer pings that far past the wall-clock boundary, at the
// same cadence.
func TestPingPacerOffsetShiftsThePing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const epoch = time.Second
		const offset = 250 * time.Millisecond
		start := time.Now()
		if WakeupTime(start, epoch) != start {
			t.Fatal("the bubble clock does not start on a wakeup boundary")
		}
		pacer := newPingPacer(epoch, epoch, offset, start, nil)
		defer pacer.stop()
		var pingTimes []time.Duration
		for len(pingTimes) < 3 {
			<-pacer.timer.C
			if pacer.fired(time.Now()) {
				pingTimes = append(pingTimes, time.Since(start))
				pacer.pinged(time.Now())
			}
		}
		// pings complete in no time here, so each lands on the next boundary
		want := []time.Duration{epoch + offset, 2*epoch + offset, 3*epoch + offset}
		for i := range want {
			if pingTimes[i] != want[i] {
				t.Fatalf("pings at %v, want %v", pingTimes, want)
			}
		}
	})
}
