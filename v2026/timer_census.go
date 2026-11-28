// A sampled count of the changes this library makes to Go runtime timers on
// its hot paths, by where it makes them and how far ahead each changed timer
// is due. A process that measures what sets off the runtime's timer heap walks
// installs one (server CPUOPT7.md §11, LBALTOPT2.md R14(a)).
//
// A Reset of a timer that is pending, or still in its P's heap as a zombie,
// marks the timer modified, and once the earliest modified deadline on a P
// passes, that P's next timer check walks its whole heap. So a P walks about
// once per wait for its next modification plus that modification's horizon,
// and the shortest horizons among frequent modifications set the rate, not
// the modifications' CPU. A stop, or a select that wakes on another case,
// leaves a zombie that a walk or the heap's head removes; a new timer is an
// insert. Neither sets off a walk.
//
// The census is process-wide because the runtime's timer heaps are. Without
// one installed, a counted change costs an atomic load. With one, a change is
// sampled through the calling thread's random source (math/rand/v2), with no
// lock and no allocation, and only a sampled change reaches Record.
package connect

import (
	"fmt"
	"math"
	mathrand "math/rand/v2"
	"sync/atomic"
	"time"
)

// Where the library changes a runtime timer on a hot path: a fixed set of
// TimerCensusSiteCount values.
type TimerCensusSite int

const (
	// the reused timer of a wait (resetOrCreateTimer), at the callers without
	// a site of their own
	TimerCensusSiteTimer TimerCensusSite = iota
	// a route write's wait for a writable route (the multi-route writer)
	TimerCensusSiteRouteWriteTimer
	// a receive sequence's wait to hand a Pack to its consumer
	TimerCensusSiteReceiveHandoffTimer
	// a receive sequence's delivery wait
	TimerCensusSiteReceiveDeliveryTimer
	// a receive sequence's ACK compression wait
	TimerCensusSiteAckCompressTimer
	// a send Pack call's admission waits (sendPackWait)
	TimerCensusSitePackWaitTimer
	// a send sequence's window pacing wait
	TimerCensusSiteWindowPacingTimer
	// a sequence loop's deadline timer (sequenceDeadlineTimer): it stops a
	// pending timer for an earlier deadline and arms a new one, and never moves
	// a pending timer, so it counts only stops
	TimerCensusSiteSequenceDeadlineTimer

	TimerCensusSiteCount
)

// A process's census of the library's timer changes.
type TimerCensus struct {
	// One change in SamplePeriod is recorded, as SamplePeriod changes. A
	// positive power of two; 1 records every change.
	SamplePeriod int
	// Records count changes at site: a timer set to fire horizon from now (a
	// nonpositive horizon is due at once), or, when stop, a pending timer
	// stopped, with horizon zero. Called on the goroutine making the change,
	// possibly concurrently, so it must not block.
	Record func(site TimerCensusSite, horizon time.Duration, stop bool, count int)
}

// The installed census, as the counted changes read it.
type installedTimerCensus struct {
	// a sampled change has these bits of a random number clear
	mask   uint32
	count  int
	record func(site TimerCensusSite, horizon time.Duration, stop bool, count int)
}

var timerCensus atomic.Pointer[installedTimerCensus]

// SetTimerCensus installs census for the process, or removes the installed
// one when nil. Panics on a SamplePeriod that is not a positive power of two
// or a nil Record.
func SetTimerCensus(census *TimerCensus) {
	if census == nil {
		timerCensus.Store(nil)
		return
	}
	period := census.SamplePeriod
	if period <= 0 || period&(period-1) != 0 || math.MaxUint32 < uint64(period) {
		panic(fmt.Errorf("timer census: the sample period %d is not a positive power of two", period))
	}
	if census.Record == nil {
		panic(fmt.Errorf("timer census: no Record"))
	}
	timerCensus.Store(&installedTimerCensus{
		mask:   uint32(period - 1),
		count:  period,
		record: census.Record,
	})
}

// Counts a Reset at site that arms its timer d from now.
func censusTimerReset(site TimerCensusSite, d time.Duration) {
	census := timerCensus.Load()
	if census == nil {
		return
	}
	if mathrand.Uint32()&census.mask != 0 {
		return
	}
	census.record(site, d, false, census.count)
}

// Counts a stop of a pending timer at site.
func censusTimerStop(site TimerCensusSite) {
	census := timerCensus.Load()
	if census == nil {
		return
	}
	if mathrand.Uint32()&census.mask != 0 {
		return
	}
	census.record(site, 0, true, census.count)
}
