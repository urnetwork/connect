package lagdriftsim

// What a fit costs, measured apart from the study. The study's own timings
// are wall-clock durations on a machine its workers, and anything else, share,
// so a fit that waits for a core counts its wait. Once the study is done, the
// fits it sampled are run again one at a time on one thread, and each is
// measured by the CPU time the process spent on it.

import (
	"runtime"
	"slices"
	"syscall"
	"time"

	"github.com/urnetwork/connect/lagdrift"
)

// The CPU time of each sampled fit, sorted, per kind.
type fitTimes [fitKinds][]time.Duration

// The process's CPU time so far, user and system.
func processCpu() time.Duration {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}

// Runs the samples again, one at a time with one P, and measures each.
func timeFits(samples [fitKinds][]fitSample) fitTimes {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	runtime.GC()
	times := fitTimes{}
	for kind, kindSamples := range samples {
		for _, sample := range kindSamples {
			start := processCpu()
			if fitKind(kind) == fitRefitSingle || fitKind(kind) == fitRefitPooled {
				refitPosition(sample.model, sample.expected, sample.obs, sample.warm)
			} else {
				lagdrift.FitPosition(sample.model, sample.expected, sample.obs, sample.warm)
			}
			times[kind] = append(times[kind], processCpu()-start)
		}
		slices.Sort(times[kind])
	}
	return times
}

// The duration below which a share q of sorted durations fall.
func durationQuantile(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := min(len(sorted)-1, max(0, int(float64(len(sorted))*q+0.999999)-1))
	return sorted[i]
}
