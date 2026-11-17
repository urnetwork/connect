package connect

import (
	"runtime"
	"strings"
	"sync"
	"testing"
)

// Counts the timer objects (time.NewTimer, time.After, time.AfterFunc) that
// were allocated from inside the named function -- closures included -- while
// run executes. It reads the allocation profile at full sampling, so the count
// is exact for the allocations made while run is in progress.
//
// A loop that arms a new timer for every wait allocates a timer and its
// channel on every tick; one that resets a single timer allocates them once.
func timerAllocationsUnder(t *testing.T, function string, run func()) int64 {
	t.Helper()
	return profiledAllocationsUnder(t, function, true, run)
}

// Counts every object allocated from inside the named function while run
// executes, timers included.
func allocationsUnder(t *testing.T, function string, run func()) int64 {
	t.Helper()
	return profiledAllocationsUnder(t, function, false, run)
}

func profiledAllocationsUnder(t *testing.T, function string, timersOnly bool, run func()) int64 {
	t.Helper()
	timerAllocationProfileLock.Lock()
	defer timerAllocationProfileLock.Unlock()

	previousRate := runtime.MemProfileRate
	runtime.MemProfileRate = 1
	defer func() {
		runtime.MemProfileRate = previousRate
	}()

	// the profile reports allocations as of the most recent completed cycle
	runtime.GC()
	runtime.GC()
	before := profiledAllocations(function, timersOnly)
	run()
	runtime.GC()
	runtime.GC()
	return profiledAllocations(function, timersOnly) - before
}

// The memory profile rate is process state.
var timerAllocationProfileLock sync.Mutex

func profiledAllocations(function string, timersOnly bool) int64 {
	n, _ := runtime.MemProfile(nil, true)
	records := make([]runtime.MemProfileRecord, n+256)
	n, ok := runtime.MemProfile(records, true)
	if !ok {
		n = len(records)
	}
	total := int64(0)
	for _, record := range records[:n] {
		timer := false
		owned := false
		frames := runtime.CallersFrames(record.Stack())
		for {
			frame, more := frames.Next()
			switch frame.Function {
			case "time.NewTimer", "time.After", "time.AfterFunc", "time.newTimer", "time.NewTicker":
				timer = true
			}
			if strings.Contains(frame.Function, function) {
				owned = true
			}
			if !more {
				break
			}
		}
		if owned && (timer || !timersOnly) {
			total += record.AllocObjects
		}
	}
	return total
}
