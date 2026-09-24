//go:build !windows

package connect

import (
	"syscall"
	"time"
)

// The cpu time of the scale tests (GEOMAP §5.8 item 5): a timed loop is judged
// by what it costs the process rather than by the wall clock, so a host
// loaded by other work does not fail it.

// The process's user and system time so far.
func testProcessCpuTime() time.Duration {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}
